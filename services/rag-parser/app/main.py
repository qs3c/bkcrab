from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import re
import shutil
import tempfile
import threading
import time
import uuid
from collections.abc import Callable
from contextlib import suppress
from dataclasses import dataclass, field
from pathlib import Path
from typing import Annotated

import uvicorn
from fastapi import FastAPI, File, Query, Request, UploadFile
from fastapi.responses import JSONResponse, StreamingResponse

from .office import (
    OFFICE_FORMATS,
    MarkItDownConverter,
    OfficeError,
    OfficeLimits,
    build_office_bundle,
    preflight_ooxml,
)
from .pdf import (
    PDFError,
    PDFLimits,
    build_pdf_analyze_bundle,
    build_pdf_render_bundle,
    parse_page_allowlist,
)
from .pdf_engine import (
    PDFEngine,
    PDFEngineError,
    Pypdfium2PDFEngine,
)
from .protocol import (
    BundleLimits,
    ProtocolError,
    env_positive_int,
    make_health_document,
    stream_tar,
    validate_bundle_for_stream,
)

LOGGER = logging.getLogger("rag-parser")

MIB = 1024 * 1024
UPLOAD_CHUNK_SIZE = 64 * 1024
OOXML_MAGIC = b"PK\x03\x04"
PDF_MAGIC = b"%PDF-"
_REQUEST_ID_RE = re.compile(r"^[A-Za-z0-9_.:-]{1,96}$")
_DEFAULT_PDF_ENGINE = object()
_OFFICE_MIME_TYPES = {
    "docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    "xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}


@dataclass(frozen=True)
class Settings:
    service_version: str
    max_input_bytes: int
    max_output_bytes: int
    max_entry_bytes: int
    max_bundle_entries: int
    parse_timeout_seconds: int
    temp_root: Path | None
    office_limits: OfficeLimits
    pdf_limits: PDFLimits = field(default_factory=PDFLimits)

    @classmethod
    def from_env(cls) -> Settings:
        max_input = env_positive_int("RAG_PARSER_MAX_INPUT_BYTES", 50 * MIB)
        max_output = env_positive_int("RAG_PARSER_MAX_OUTPUT_BYTES", 200 * MIB)
        max_entry = env_positive_int("RAG_PARSER_MAX_ENTRY_BYTES", 20 * MIB)
        temp_root_value = os.getenv("RAG_PARSER_TEMP_ROOT", "").strip()
        temp_root = Path(temp_root_value).resolve() if temp_root_value else None
        return cls(
            service_version=os.getenv("RAG_PARSER_SERVICE_VERSION", "dev").strip() or "dev",
            max_input_bytes=max_input,
            max_output_bytes=max_output,
            max_entry_bytes=min(max_entry, max_output),
            max_bundle_entries=env_positive_int("RAG_PARSER_MAX_BUNDLE_ENTRIES", 1000),
            parse_timeout_seconds=env_positive_int("RAG_PARSER_PARSE_TIMEOUT_SECONDS", 600),
            temp_root=temp_root,
            office_limits=OfficeLimits(
                max_archive_entries=env_positive_int(
                    "RAG_PARSER_MAX_OOXML_ENTRIES", 10000
                ),
                max_zip_entry_bytes=env_positive_int(
                    "RAG_PARSER_MAX_OOXML_ENTRY_BYTES", 50 * MIB
                ),
                max_extracted_bytes=env_positive_int(
                    "RAG_PARSER_MAX_EXTRACTED_BYTES", 200 * MIB
                ),
                max_compression_ratio=env_positive_int(
                    "RAG_PARSER_MAX_COMPRESSION_RATIO", 100
                ),
                max_asset_bytes=env_positive_int("RAG_PARSER_MAX_ASSET_BYTES", 20 * MIB),
                max_assets=env_positive_int("RAG_PARSER_MAX_ASSETS", 500),
                max_image_pixels=env_positive_int(
                    "RAG_PARSER_MAX_IMAGE_PIXELS", 40_000_000
                ),
            ),
            pdf_limits=PDFLimits(
                max_pages=env_positive_int("RAG_PARSER_MAX_PAGES", 300),
                render_dpi=env_positive_int("RAG_PARSER_PDF_RENDER_DPI", 180),
                max_image_pixels=env_positive_int(
                    "RAG_PARSER_MAX_IMAGE_PIXELS", 40_000_000
                ),
                max_assets=env_positive_int("RAG_PARSER_MAX_ASSETS", 500),
                max_asset_bytes=env_positive_int(
                    "RAG_PARSER_MAX_ASSET_BYTES", 20 * MIB
                ),
            ),
        )


def _request_id(request: Request) -> str:
    candidate = request.headers.get("x-request-id", "")
    return candidate if _REQUEST_ID_RE.fullmatch(candidate) else uuid.uuid4().hex


def _request_directory(settings: Settings) -> Path:
    if settings.temp_root is not None:
        if not settings.temp_root.is_dir():
            raise RuntimeError("RAG_PARSER_TEMP_ROOT must name an existing directory")
        directory = Path(tempfile.mkdtemp(prefix="request-", dir=settings.temp_root))
    else:
        directory = Path(tempfile.mkdtemp(prefix="rag-parser-request-"))
    directory.chmod(0o700)
    return directory


async def _save_upload(
    request: Request,
    upload: UploadFile,
    destination: Path,
    max_input_bytes: int,
) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    prefix = bytearray()
    with destination.open("xb") as output:
        while chunk := await upload.read(UPLOAD_CHUNK_SIZE):
            if await request.is_disconnected():
                raise asyncio.CancelledError
            if len(prefix) < len(OOXML_MAGIC):
                prefix.extend(chunk[: len(OOXML_MAGIC) - len(prefix)])
            size += len(chunk)
            if size > max_input_bytes:
                raise OfficeError(
                    "input_too_large",
                    "uploaded file exceeds maxInputBytes",
                    status_code=413,
                )
            digest.update(chunk)
            output.write(chunk)
    if size == 0 or bytes(prefix) != OOXML_MAGIC:
        raise OfficeError("invalid_ooxml_magic", "Office input must be an OOXML ZIP container")
    return digest.hexdigest(), size


def _validate_upload_metadata(upload: UploadFile, source_format: str) -> None:
    filename = upload.filename or ""
    extension = Path(filename).suffix.lower()
    if extension != f".{source_format}":
        raise OfficeError("extension_mismatch", "filename extension does not match format")
    if upload.content_type != _OFFICE_MIME_TYPES[source_format]:
        raise OfficeError("mime_mismatch", "multipart Content-Type does not match format")


async def _save_pdf_upload(
    request: Request,
    upload: UploadFile,
    destination: Path,
    max_input_bytes: int,
) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    prefix = bytearray()
    with destination.open("xb") as output:
        while chunk := await upload.read(UPLOAD_CHUNK_SIZE):
            if await request.is_disconnected():
                raise asyncio.CancelledError
            if len(prefix) < 1024:
                prefix.extend(chunk[: 1024 - len(prefix)])
            size += len(chunk)
            if size > max_input_bytes:
                raise PDFError(
                    "input_too_large", "uploaded file exceeds maxInputBytes", 413
                )
            digest.update(chunk)
            output.write(chunk)
    if size == 0 or PDF_MAGIC not in bytes(prefix):
        raise PDFError("invalid_pdf_magic", "PDF input does not contain a PDF header", 400)
    return digest.hexdigest(), size


def _validate_pdf_upload_metadata(upload: UploadFile) -> None:
    if Path(upload.filename or "").suffix.lower() != ".pdf":
        raise PDFError("extension_mismatch", "filename extension must be .pdf", 400)
    if upload.content_type != "application/pdf":
        raise PDFError("mime_mismatch", "multipart Content-Type must be application/pdf", 400)


async def _run_pdf_operation(
    request: Request,
    timeout_seconds: int,
    operation: Callable[[Callable[[], bool]], object],
):
    """Run PDFium with cooperative cancel checks at page/object boundaries."""

    cancelled = threading.Event()
    task = asyncio.create_task(asyncio.to_thread(operation, cancelled.is_set))
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    while True:
        remaining = deadline - asyncio.get_running_loop().time()
        if remaining <= 0:
            cancelled.set()
            with suppress(Exception):
                await task
            raise PDFError("parse_timeout", "PDF operation timed out", 504)
        done, _ = await asyncio.wait({task}, timeout=min(0.05, remaining))
        if done:
            return task.result()
        if await request.is_disconnected():
            cancelled.set()
            with suppress(Exception):
                await task
            raise asyncio.CancelledError


def _approved_pdf_engine_or_none() -> PDFEngine | None:
    try:
        return Pypdfium2PDFEngine()
    except PDFEngineError as exc:
        LOGGER.error("PDF capability disabled code=%s", exc.code)
        return None


def create_app(
    settings: Settings | None = None,
    converter: MarkItDownConverter | None = None,
    pdf_engine: PDFEngine | None | object = _DEFAULT_PDF_ENGINE,
) -> FastAPI:
    runtime = settings or Settings.from_env()
    runtime.pdf_limits.validate()
    active_pdf_engine = (
        _approved_pdf_engine_or_none() if pdf_engine is _DEFAULT_PDF_ENGINE else pdf_engine
    )
    if active_pdf_engine is not None and not isinstance(active_pdf_engine, PDFEngine):
        raise TypeError("pdf_engine must implement PDFEngine or be None")
    app = FastAPI(
        title="bkcrab rag-parser",
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )

    @app.exception_handler(OfficeError)
    async def handle_office_error(_: Request, exc: OfficeError) -> JSONResponse:
        return JSONResponse(
            status_code=exc.status_code,
            content={"error": {"code": exc.code, "message": str(exc)}},
        )

    @app.exception_handler(ProtocolError)
    async def handle_protocol_error(_: Request, exc: ProtocolError) -> JSONResponse:
        return JSONResponse(
            status_code=422,
            content={"error": {"code": exc.code, "message": str(exc)}},
        )

    @app.exception_handler(PDFError)
    async def handle_pdf_error(_: Request, exc: PDFError) -> JSONResponse:
        return JSONResponse(
            status_code=exc.status_code,
            content={"error": {"code": exc.code, "message": str(exc)}},
        )

    @app.exception_handler(PDFEngineError)
    async def handle_pdf_engine_error(_: Request, exc: PDFEngineError) -> JSONResponse:
        return JSONResponse(
            status_code=exc.status_code,
            content={"error": {"code": exc.code, "message": str(exc)}},
        )

    @app.get("/healthz")
    async def healthz() -> dict[str, object]:
        return make_health_document(
            service_version=runtime.service_version,
            max_input_bytes=runtime.max_input_bytes,
            max_output_bytes=runtime.max_output_bytes,
            pdf_engine=active_pdf_engine.name if active_pdf_engine is not None else "",
            pdf_engine_version=(
                active_pdf_engine.version if active_pdf_engine is not None else ""
            ),
        )

    @app.post("/v1/office/convert")
    async def convert_office(
        request: Request,
        file: Annotated[UploadFile, File()],
        format: Annotated[str, Query(pattern="^(docx|pptx|xlsx)$")],
    ) -> StreamingResponse:
        request_id = _request_id(request)
        started = time.monotonic()
        source_format = format.lower()
        if source_format not in OFFICE_FORMATS:
            raise OfficeError("unsupported_format", "unsupported Office format", status_code=400)
        form_items = list((await request.form()).multi_items())
        if len(form_items) != 1 or form_items[0][0] != "file":
            raise OfficeError(
                "invalid_multipart",
                "request must contain exactly one multipart file field named file",
                status_code=400,
            )
        _validate_upload_metadata(file, source_format)
        request_dir = _request_directory(runtime)
        original = request_dir / f"source.{source_format}"
        try:
            source_sha256, source_size = await _save_upload(
                request, file, original, runtime.max_input_bytes
            )
            await file.close()

            def prepare_bundle():
                preflight = preflight_ooxml(
                    original,
                    source_format,
                    request_dir,
                    runtime.office_limits,
                )
                active_converter = converter or MarkItDownConverter()
                return build_office_bundle(
                    original_source=original,
                    sanitized_source=preflight.sanitized_path,
                    source_format=source_format,
                    source_sha256=source_sha256,
                    source_size=source_size,
                    request_dir=request_dir,
                    converter=active_converter,
                    limits=runtime.office_limits,
                    preflight_warnings=preflight.warnings,
                )

            bundle = await asyncio.wait_for(
                asyncio.to_thread(prepare_bundle),
                timeout=runtime.parse_timeout_seconds,
            )
        except TimeoutError as exc:
            shutil.rmtree(request_dir, ignore_errors=True)
            raise OfficeError("parse_timeout", "Office conversion timed out", 504) from exc
        except BaseException:
            shutil.rmtree(request_dir, ignore_errors=True)
            await file.close()
            raise

        LOGGER.info(
            "office conversion prepared request_id=%s format=%s units=%d assets=%d "
            "occurrences=%d warnings=%d duration_ms=%d",
            request_id,
            source_format,
            len(bundle.manifest.units),
            len(bundle.manifest.assets),
            len(bundle.manifest.occurrences),
            len(bundle.manifest.warnings),
            round((time.monotonic() - started) * 1000),
        )
        limits = BundleLimits(
            max_output_bytes=runtime.max_output_bytes,
            max_entry_bytes=runtime.max_entry_bytes,
            max_entries=runtime.max_bundle_entries,
        )
        try:
            validate_bundle_for_stream(bundle, limits)
        except BaseException:
            bundle.close()
            raise
        return StreamingResponse(
            stream_tar(bundle, limits),
            media_type="application/x-tar",
            headers={
                "Content-Disposition": "attachment; filename=rag-parser-bundle.tar",
                "X-Request-ID": request_id,
                "X-Content-Type-Options": "nosniff",
            },
        )

    async def prepare_pdf_source(
        request: Request, upload: UploadFile, request_dir: Path
    ) -> tuple[Path, str, int]:
        form_items = list((await request.form()).multi_items())
        if len(form_items) != 1 or form_items[0][0] != "file":
            raise PDFError(
                "invalid_multipart",
                "request must contain exactly one multipart file field named file",
                400,
            )
        _validate_pdf_upload_metadata(upload)
        original = request_dir / "source.pdf"
        source_sha256, source_size = await _save_pdf_upload(
            request, upload, original, runtime.max_input_bytes
        )
        await upload.close()
        return original, source_sha256, source_size

    def pdf_response(bundle, request_id: str) -> StreamingResponse:
        limits = BundleLimits(
            max_output_bytes=runtime.max_output_bytes,
            max_entry_bytes=runtime.max_entry_bytes,
            max_entries=runtime.max_bundle_entries,
        )
        try:
            validate_bundle_for_stream(bundle, limits)
        except BaseException:
            bundle.close()
            raise
        return StreamingResponse(
            stream_tar(bundle, limits),
            media_type="application/x-tar",
            headers={
                "Content-Disposition": "attachment; filename=rag-parser-bundle.tar",
                "X-Request-ID": request_id,
                "X-Content-Type-Options": "nosniff",
            },
        )

    @app.post("/v1/pdf/analyze")
    async def analyze_pdf(
        request: Request,
        file: Annotated[UploadFile, File()],
    ) -> StreamingResponse:
        if active_pdf_engine is None:
            raise PDFError("capability_unavailable", "PDF engine is unavailable", 503)
        request_id = _request_id(request)
        started = time.monotonic()
        request_dir = _request_directory(runtime)
        try:
            original, source_sha256, source_size = await prepare_pdf_source(
                request, file, request_dir
            )

            def prepare(cancelled: Callable[[], bool]):
                return build_pdf_analyze_bundle(
                    source=original,
                    source_sha256=source_sha256,
                    source_size=source_size,
                    request_dir=request_dir,
                    engine=active_pdf_engine,
                    limits=runtime.pdf_limits,
                    cancelled=cancelled,
                )

            bundle = await _run_pdf_operation(
                request, runtime.parse_timeout_seconds, prepare
            )
        except BaseException:
            shutil.rmtree(request_dir, ignore_errors=True)
            await file.close()
            raise
        LOGGER.info(
            "pdf analyze prepared request_id=%s pages=%d failed_pages=%d warnings=%d duration_ms=%d",
            request_id,
            len(bundle.manifest.pages),
            sum(page.status == "failed" for page in bundle.manifest.pages),
            len(bundle.manifest.warnings),
            round((time.monotonic() - started) * 1000),
        )
        return pdf_response(bundle, request_id)

    @app.post("/v1/pdf/render")
    async def render_pdf(
        request: Request,
        file: Annotated[UploadFile, File()],
        pages: Annotated[str, Query()] = "",
    ) -> StreamingResponse:
        if active_pdf_engine is None:
            raise PDFError("capability_unavailable", "PDF engine is unavailable", 503)
        requested_pages = parse_page_allowlist(pages, runtime.pdf_limits.max_pages)
        request_id = _request_id(request)
        started = time.monotonic()
        request_dir = _request_directory(runtime)
        try:
            original, source_sha256, source_size = await prepare_pdf_source(
                request, file, request_dir
            )

            def prepare(cancelled: Callable[[], bool]):
                return build_pdf_render_bundle(
                    source=original,
                    source_sha256=source_sha256,
                    source_size=source_size,
                    request_dir=request_dir,
                    requested_pages=requested_pages,
                    engine=active_pdf_engine,
                    limits=runtime.pdf_limits,
                    cancelled=cancelled,
                )

            bundle = await _run_pdf_operation(
                request, runtime.parse_timeout_seconds, prepare
            )
        except BaseException:
            shutil.rmtree(request_dir, ignore_errors=True)
            await file.close()
            raise
        LOGGER.info(
            "pdf render prepared request_id=%s requested_pages=%d rendered_pages=%d "
            "assets=%d warnings=%d duration_ms=%d",
            request_id,
            len(requested_pages),
            sum(page.status == "ok" for page in bundle.manifest.pages),
            len(bundle.manifest.assets),
            len(bundle.manifest.warnings),
            round((time.monotonic() - started) * 1000),
        )
        return pdf_response(bundle, request_id)

    return app


app = create_app()


def main() -> None:
    logging.basicConfig(
        level=os.getenv("RAG_PARSER_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    port = env_positive_int("RAG_PARSER_PORT", 8080)
    if port > 65535:
        raise RuntimeError("RAG_PARSER_PORT must be <= 65535")
    uvicorn.run(app, host="0.0.0.0", port=port, access_log=False)


if __name__ == "__main__":
    main()
