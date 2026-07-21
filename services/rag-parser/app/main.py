from __future__ import annotations

import asyncio
import hashlib
import logging
import multiprocessing
import os
import re
import shutil
import tempfile
import time
import uuid
from collections.abc import Callable
from contextlib import suppress
from dataclasses import dataclass, field
from multiprocessing.connection import Connection
from pathlib import Path
from typing import Annotated, Any

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
    Bundle,
    BundleLimits,
    Manifest,
    PayloadEntry,
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
_PROCESS_POLL_SECONDS = 0.025
_PROCESS_EXIT_GRACE_SECONDS = 0.25
_PROCESS_KILL_GRACE_SECONDS = 0.25
_OFFICE_MIME_TYPES = {
    "docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    "xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}


def _limit_from_canonical(
    alias_name: str,
    canonical_name: str,
    default: int,
    convert: Callable[[int], int],
) -> int:
    """Resolve one parser limit from the main service's canonical setting.

    The legacy parser-specific variable remains accepted for standalone users.
    Deployments may temporarily provide both names during an upgrade, but a
    mismatch is a startup error instead of a silently divergent safety limit.
    """
    canonical_raw = os.getenv(canonical_name, "").strip()
    alias_raw = os.getenv(alias_name, "").strip()
    if not canonical_raw:
        return env_positive_int(alias_name, default)

    canonical_value = env_positive_int(canonical_name, 1)
    expected = convert(canonical_value)
    if expected <= 0:
        raise RuntimeError(f"{canonical_name} produces a non-positive parser limit")
    if alias_raw and env_positive_int(alias_name, default) != expected:
        raise RuntimeError(
            f"{alias_name} must match the limit derived from {canonical_name}"
        )
    return expected


def _timeout_seconds_from_milliseconds(milliseconds: int) -> int:
    # The gateway owns the millisecond-precision request deadline. The parser's
    # internal watchdog is whole-second, so round up: it must never abort a
    # request earlier merely because the canonical value is not divisible by
    # one second.
    return (milliseconds + 999) // 1000


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
        max_input = _limit_from_canonical(
            "RAG_PARSER_MAX_INPUT_BYTES",
            "BKCRAB_RAG_LIMITS_MAX_FILE_MB",
            50 * MIB,
            lambda megabytes: megabytes * MIB,
        )
        max_output = _limit_from_canonical(
            "RAG_PARSER_MAX_OUTPUT_BYTES",
            "BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES",
            200 * MIB,
            lambda value: value,
        )
        max_entry = env_positive_int("RAG_PARSER_MAX_ENTRY_BYTES", 20 * MIB)
        parse_timeout_seconds = _limit_from_canonical(
            "RAG_PARSER_PARSE_TIMEOUT_SECONDS",
            "BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS",
            600,
            _timeout_seconds_from_milliseconds,
        )
        temp_root_value = os.getenv("RAG_PARSER_TEMP_ROOT", "").strip()
        temp_root = Path(temp_root_value).resolve() if temp_root_value else None
        return cls(
            service_version=os.getenv("RAG_PARSER_SERVICE_VERSION", "dev").strip() or "dev",
            max_input_bytes=max_input,
            max_output_bytes=max_output,
            max_entry_bytes=min(max_entry, max_output),
            max_bundle_entries=env_positive_int("RAG_PARSER_MAX_BUNDLE_ENTRIES", 1000),
            parse_timeout_seconds=parse_timeout_seconds,
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


@dataclass(frozen=True)
class _ParserWork:
    operation: str
    source: Path
    source_sha256: str
    source_size: int
    request_dir: Path
    source_format: str = ""
    office_limits: OfficeLimits | None = None
    pdf_limits: PDFLimits | None = None
    converter: object | None = None
    pdf_engine: object | None = None
    requested_pages: tuple[int, ...] = ()


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


def _build_parser_bundle(work: _ParserWork) -> Bundle:
    if work.operation == "office":
        if work.office_limits is None:
            raise RuntimeError("Office parser work is missing limits")
        preflight = preflight_ooxml(
            work.source,
            work.source_format,
            work.request_dir,
            work.office_limits,
        )
        active_converter = work.converter
        if active_converter is None:
            active_converter = MarkItDownConverter()
        return build_office_bundle(
            original_source=work.source,
            sanitized_source=preflight.sanitized_path,
            source_format=work.source_format,
            source_sha256=work.source_sha256,
            source_size=work.source_size,
            request_dir=work.request_dir,
            converter=active_converter,
            limits=work.office_limits,
            preflight_warnings=preflight.warnings,
        )

    if work.pdf_limits is None or not isinstance(work.pdf_engine, PDFEngine):
        raise RuntimeError("PDF parser work is missing its engine or limits")
    if work.operation == "pdf-analyze":
        return build_pdf_analyze_bundle(
            source=work.source,
            source_sha256=work.source_sha256,
            source_size=work.source_size,
            request_dir=work.request_dir,
            engine=work.pdf_engine,
            limits=work.pdf_limits,
            cancelled=lambda: False,
        )
    if work.operation == "pdf-render":
        return build_pdf_render_bundle(
            source=work.source,
            source_sha256=work.source_sha256,
            source_size=work.source_size,
            request_dir=work.request_dir,
            requested_pages=work.requested_pages,
            engine=work.pdf_engine,
            limits=work.pdf_limits,
            cancelled=lambda: False,
        )
    raise RuntimeError("unsupported parser worker operation")


def _payload_source_in_request(source: Path, request_dir: Path) -> Path:
    try:
        root = request_dir.resolve(strict=True)
        resolved = source.resolve(strict=True)
    except OSError as exc:
        raise ProtocolError(
            "invalid_worker_payload", "parser worker payload is missing"
        ) from exc
    if (
        not resolved.is_relative_to(root)
        or source.is_symlink()
        or not resolved.is_file()
    ):
        raise ProtocolError(
            "invalid_worker_payload", "parser worker payload escaped its request directory"
        )
    return resolved


def _worker_bundle_result(bundle: Bundle, request_dir: Path) -> dict[str, Any]:
    materialized_root = request_dir / "worker-payloads"
    payloads: list[dict[str, str]] = []
    for index, payload in enumerate(bundle.payloads):
        source = payload.source_path
        if source is None:
            materialized_root.mkdir(mode=0o700, exist_ok=True)
            source = materialized_root / f"{index:06d}.payload"
            with payload.opener() as input_handle, source.open("xb") as output_handle:
                shutil.copyfileobj(input_handle, output_handle, length=UPLOAD_CHUNK_SIZE)
            source.chmod(0o600)
            materialized = PayloadEntry.from_file(payload.path, payload.mime_type, source)
            if materialized.descriptor() != payload.descriptor():
                raise ProtocolError(
                    "invalid_worker_payload",
                    "materialized parser worker payload changed content",
                )
        resolved = _payload_source_in_request(source, request_dir)
        payloads.append(
            {
                "path": payload.path,
                "mimeType": payload.mime_type,
                "source": str(resolved),
            }
        )
    return {"ok": True, "manifest": bundle.manifest.to_dict(), "payloads": payloads}


def _worker_error_result(exc: BaseException) -> dict[str, Any]:
    if isinstance(exc, OfficeError):
        kind = "office"
        status_code = exc.status_code
        code = exc.code
        message = str(exc)
    elif isinstance(exc, PDFError):
        kind = "pdf"
        status_code = exc.status_code
        code = exc.code
        message = str(exc)
    elif isinstance(exc, PDFEngineError):
        kind = "pdf-engine"
        status_code = exc.status_code
        code = exc.code
        message = str(exc)
    elif isinstance(exc, ProtocolError):
        kind = "protocol"
        status_code = 422
        code = exc.code
        message = str(exc)
    else:
        kind = "internal"
        status_code = 500
        code = "parser_worker_failed"
        message = "parser worker failed"
    return {
        "ok": False,
        "errorKind": kind,
        "statusCode": status_code,
        "code": code,
        "message": message,
    }


def _parser_worker(connection: Connection, work: _ParserWork) -> None:
    try:
        result = _worker_bundle_result(_build_parser_bundle(work), work.request_dir)
    except BaseException as exc:
        result = _worker_error_result(exc)
    try:
        connection.send(result)
    except (BrokenPipeError, EOFError, OSError):
        pass
    finally:
        connection.close()


def _worker_failure(operation: str) -> OfficeError | PDFError:
    if operation == "office":
        return OfficeError("parser_worker_failed", "Office parser worker failed", 500)
    return PDFError("parser_worker_failed", "PDF parser worker failed", 500)


def _raise_worker_error(result: dict[str, Any], operation: str) -> None:
    kind = result.get("errorKind")
    code = result.get("code")
    message = result.get("message")
    status_code = result.get("statusCode")
    if (
        not isinstance(kind, str)
        or not isinstance(code, str)
        or not isinstance(message, str)
        or not isinstance(status_code, int)
    ):
        raise _worker_failure(operation)
    if kind == "office":
        raise OfficeError(code, message, status_code)
    if kind == "pdf":
        raise PDFError(code, message, status_code)
    if kind == "pdf-engine":
        raise PDFEngineError(code, message, status_code)
    if kind == "protocol":
        raise ProtocolError(code, message)
    raise _worker_failure(operation)


def _restore_worker_bundle(result: object, work: _ParserWork) -> Bundle:
    if not isinstance(result, dict):
        raise _worker_failure(work.operation)
    if result.get("ok") is not True:
        _raise_worker_error(result, work.operation)

    manifest = Manifest.from_dict(result.get("manifest"))
    raw_payloads = result.get("payloads")
    if not isinstance(raw_payloads, list):
        raise _worker_failure(work.operation)
    payloads: list[PayloadEntry] = []
    for raw_payload in raw_payloads:
        if not isinstance(raw_payload, dict):
            raise _worker_failure(work.operation)
        path = raw_payload.get("path")
        mime_type = raw_payload.get("mimeType")
        source_value = raw_payload.get("source")
        if not all(isinstance(value, str) for value in (path, mime_type, source_value)):
            raise _worker_failure(work.operation)
        source = _payload_source_in_request(Path(source_value), work.request_dir)
        payloads.append(PayloadEntry.from_file(path, mime_type, source))
    bundle = Bundle(
        manifest=manifest,
        payloads=tuple(payloads),
        cleanup=lambda: shutil.rmtree(work.request_dir, ignore_errors=True),
    )
    if tuple(payload.descriptor() for payload in bundle.payloads) != manifest.entries:
        bundle.close()
        raise ProtocolError(
            "entry_mismatch", "parser worker payload metadata does not match manifest entries"
        )
    return bundle


def _reap_parser_worker(
    process: multiprocessing.Process, *, terminate_first: bool
) -> None:
    if terminate_first and process.is_alive():
        with suppress(OSError):
            process.terminate()
    process.join(_PROCESS_EXIT_GRACE_SECONDS)
    if process.is_alive():
        with suppress(OSError):
            process.terminate()
        process.join(_PROCESS_EXIT_GRACE_SECONDS)
    if process.is_alive():
        with suppress(OSError):
            process.kill()
        process.join(_PROCESS_KILL_GRACE_SECONDS)
    if process.is_alive():
        LOGGER.critical("parser worker could not be reaped name=%s", process.name)
        return
    process.close()


async def _run_parser_operation(
    request: Request,
    timeout_seconds: int,
    work: _ParserWork,
    timeout_error: OfficeError | PDFError,
) -> Bundle:
    """Run blocking document parsing in a force-terminable spawned process."""

    context = multiprocessing.get_context("spawn")
    receive_connection, send_connection = context.Pipe(duplex=False)
    process = context.Process(
        target=_parser_worker,
        args=(send_connection, work),
        name=f"rag-parser-{work.operation}",
        daemon=True,
    )
    started = False
    result_received = False
    deadline = asyncio.get_running_loop().time() + timeout_seconds
    try:
        try:
            process.start()
            started = True
        except BaseException as exc:
            LOGGER.exception("could not start parser worker operation=%s", work.operation)
            raise _worker_failure(work.operation) from exc
        finally:
            send_connection.close()

        while True:
            if receive_connection.poll():
                try:
                    result = receive_connection.recv()
                except EOFError as exc:
                    raise _worker_failure(work.operation) from exc
                result_received = True
                return _restore_worker_bundle(result, work)
            if not process.is_alive():
                raise _worker_failure(work.operation)
            remaining = deadline - asyncio.get_running_loop().time()
            if remaining <= 0:
                raise timeout_error
            if await request.is_disconnected():
                raise asyncio.CancelledError
            await asyncio.sleep(min(_PROCESS_POLL_SECONDS, remaining))
    finally:
        receive_connection.close()
        if started:
            _reap_parser_worker(process, terminate_first=not result_received)


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
            bundle = await _run_parser_operation(
                request,
                runtime.parse_timeout_seconds,
                _ParserWork(
                    operation="office",
                    source=original,
                    source_sha256=source_sha256,
                    source_size=source_size,
                    request_dir=request_dir,
                    source_format=source_format,
                    office_limits=runtime.office_limits,
                    converter=converter,
                ),
                OfficeError("parse_timeout", "Office conversion timed out", 504),
            )
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
            bundle = await _run_parser_operation(
                request,
                runtime.parse_timeout_seconds,
                _ParserWork(
                    operation="pdf-analyze",
                    source=original,
                    source_sha256=source_sha256,
                    source_size=source_size,
                    request_dir=request_dir,
                    pdf_limits=runtime.pdf_limits,
                    pdf_engine=active_pdf_engine,
                ),
                PDFError("parse_timeout", "PDF operation timed out", 504),
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
            bundle = await _run_parser_operation(
                request,
                runtime.parse_timeout_seconds,
                _ParserWork(
                    operation="pdf-render",
                    source=original,
                    source_sha256=source_sha256,
                    source_size=source_size,
                    request_dir=request_dir,
                    pdf_limits=runtime.pdf_limits,
                    pdf_engine=active_pdf_engine,
                    requested_pages=requested_pages,
                ),
                PDFError("parse_timeout", "PDF operation timed out", 504),
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
