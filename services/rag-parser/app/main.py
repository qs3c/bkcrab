from __future__ import annotations

import asyncio
import hashlib
import logging
import os
import re
import shutil
import tempfile
import time
import uuid
from dataclasses import dataclass
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
_REQUEST_ID_RE = re.compile(r"^[A-Za-z0-9_.:-]{1,96}$")
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


def create_app(
    settings: Settings | None = None,
    converter: MarkItDownConverter | None = None,
) -> FastAPI:
    runtime = settings or Settings.from_env()
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

    @app.get("/healthz")
    async def healthz() -> dict[str, object]:
        return make_health_document(
            service_version=runtime.service_version,
            max_input_bytes=runtime.max_input_bytes,
            max_output_bytes=runtime.max_output_bytes,
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
