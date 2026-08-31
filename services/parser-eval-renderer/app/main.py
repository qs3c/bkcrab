from __future__ import annotations

import asyncio
import hashlib
import os
import shutil
import signal
import subprocess
import tempfile
import time
from collections.abc import Callable
from contextlib import suppress
from dataclasses import dataclass
from pathlib import Path
from urllib.parse import urljoin

import pymupdf
import uvicorn
from fastapi import FastAPI, Request
from fastapi.responses import FileResponse, JSONResponse
from starlette.background import BackgroundTask
from starlette.datastructures import UploadFile

from .protocol import (
    FORMATS,
    MIME_TYPES,
    PageArtifact,
    RendererDescriptor,
    RendererError,
    RendererLimits,
    build_manifest,
    env_positive_int,
    health_document,
    preflight_ooxml,
    write_bundle,
)

MIB = 1024 * 1024
UPLOAD_CHUNK_SIZE = 64 * 1024


@dataclass(frozen=True)
class Settings:
    descriptor: RendererDescriptor
    limits: RendererLimits
    timeout_seconds: float
    temp_root: Path | None = None

    @classmethod
    def from_env(cls) -> Settings:
        timeout_ms = env_positive_int("PARSER_EVAL_RENDER_TIMEOUT_MS", 600_000)
        temp_value = os.getenv("PARSER_EVAL_RENDERER_TEMP_ROOT", "").strip()
        temp_root = Path(temp_value).resolve() if temp_value else None
        if temp_root is not None and not temp_root.is_dir():
            raise RuntimeError("PARSER_EVAL_RENDERER_TEMP_ROOT must name an existing directory")
        descriptor = RendererDescriptor(
            service_version=os.getenv("PARSER_EVAL_RENDERER_SERVICE_VERSION", "dev").strip()
            or "dev",
            libreoffice_version=_libreoffice_version(),
            pymupdf_version=str(pymupdf.VersionBind),
        )
        limits = RendererLimits(
            max_input_bytes=env_positive_int("PARSER_EVAL_MAX_FILE_BYTES", 50 * MIB),
            max_pages=env_positive_int("PARSER_EVAL_MAX_PAGES", 6),
            render_dpi=env_positive_int("PARSER_EVAL_RENDER_DPI", 100),
            max_archive_entries=env_positive_int("PARSER_EVAL_RENDERER_MAX_OOXML_ENTRIES", 10_000),
            max_zip_entry_bytes=env_positive_int(
                "PARSER_EVAL_RENDERER_MAX_OOXML_ENTRY_BYTES", 100 * MIB
            ),
            max_extracted_bytes=env_positive_int(
                "PARSER_EVAL_RENDERER_MAX_EXTRACTED_BYTES", 500 * MIB
            ),
            max_compression_ratio=env_positive_int(
                "PARSER_EVAL_RENDERER_MAX_COMPRESSION_RATIO", 200
            ),
            max_page_pixels=env_positive_int("PARSER_EVAL_RENDERER_MAX_PAGE_PIXELS", 40_000_000),
            max_page_bytes=env_positive_int("PARSER_EVAL_RENDERER_MAX_PAGE_BYTES", 20 * MIB),
            max_bundle_bytes=env_positive_int("PARSER_EVAL_RENDERER_MAX_BUNDLE_BYTES", 128 * MIB),
        )
        limits.validate()
        return cls(
            descriptor=descriptor,
            limits=limits,
            timeout_seconds=timeout_ms / 1000,
            temp_root=temp_root,
        )


RenderConverter = Callable[[Path, str, Path, Path, float], None]


def _libreoffice_version() -> str:
    configured = os.getenv("PARSER_EVAL_LIBREOFFICE_VERSION", "").strip()
    if configured:
        return configured
    try:
        result = subprocess.run(
            ["soffice", "--version"],
            check=True,
            capture_output=True,
            text=True,
            timeout=5,
        )
    except (OSError, subprocess.SubprocessError):
        return "unavailable"
    return (result.stdout or result.stderr).strip()[:128] or "unknown"


def _request_directory(settings: Settings) -> Path:
    root = settings.temp_root
    directory = Path(tempfile.mkdtemp(prefix="parser-eval-render-", dir=root))
    directory.chmod(0o700)
    return directory


async def _save_upload(
    request: Request, upload: UploadFile, destination: Path, limit: int
) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    with destination.open("xb") as output:
        destination.chmod(0o600)
        while chunk := await upload.read(UPLOAD_CHUNK_SIZE):
            if await request.is_disconnected():
                raise asyncio.CancelledError
            size += len(chunk)
            if size > limit:
                raise RendererError("input_too_large", 413)
            digest.update(chunk)
            output.write(chunk)
    if size == 0:
        raise RendererError("invalid_ooxml", 422)
    return digest.hexdigest(), size


def libreoffice_convert(
    source: Path, _source_format: str, output_pdf: Path, profile: Path, timeout_seconds: float
) -> None:
    output_pdf.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    profile.mkdir(mode=0o700)
    profile_uri = urljoin("file:", profile.resolve().as_posix())
    command = [
        "soffice",
        "--headless",
        "--nologo",
        "--nodefault",
        "--nolockcheck",
        "--nofirststartwizard",
        "--convert-to",
        "pdf",
        "--outdir",
        str(output_pdf.parent),
        f"-env:UserInstallation={profile_uri}",
        str(source),
    ]
    popen_options: dict[str, object] = {"stdout": subprocess.DEVNULL, "stderr": subprocess.DEVNULL}
    if os.name == "posix":
        popen_options["start_new_session"] = True
    process = subprocess.Popen(command, **popen_options)
    try:
        return_code = process.wait(timeout=timeout_seconds)
    except subprocess.TimeoutExpired as error:
        if os.name == "posix":
            with suppress(ProcessLookupError):
                os.killpg(process.pid, signal.SIGKILL)
        else:
            process.kill()
        process.wait()
        raise RendererError("render_timeout", 504) from error
    if return_code != 0:
        raise RendererError("libreoffice_failed", 502)
    generated = output_pdf.parent / source.with_suffix(".pdf").name
    if not generated.is_file() or generated.stat().st_size == 0:
        raise RendererError("libreoffice_failed", 502)
    generated.replace(output_pdf)


def _render_bundle(
    source: Path,
    source_format: str,
    source_sha256: str,
    source_size: int,
    request_dir: Path,
    settings: Settings,
    converter: RenderConverter,
) -> Path:
    started = time.monotonic()
    output_dir = request_dir / "output"
    output_dir.mkdir(mode=0o700)
    pdf_path = output_dir / "rendered.pdf"
    profile = request_dir / "profile"
    converter(source, source_format, pdf_path, profile, settings.timeout_seconds)
    page_root = request_dir / "bundle"
    (page_root / "pages").mkdir(parents=True, mode=0o700)
    artifacts: list[PageArtifact] = []
    try:
        document = pymupdf.open(pdf_path)
    except (OSError, RuntimeError, ValueError) as error:
        raise RendererError("invalid_pdf", 502) from error
    try:
        total_pages = document.page_count
        if total_pages <= 0:
            raise RendererError("invalid_pdf", 502)
        matrix = pymupdf.Matrix(settings.limits.render_dpi / 72, settings.limits.render_dpi / 72)
        total_payload = 0
        for index in range(min(total_pages, settings.limits.max_pages)):
            try:
                pixmap = document.load_page(index).get_pixmap(matrix=matrix, alpha=False)
                width, height = pixmap.width, pixmap.height
                if width <= 0 or height <= 0 or width * height > settings.limits.max_page_pixels:
                    raise RendererError("render_limit_exceeded", 422)
                payload = pixmap.tobytes("png")
            except RendererError:
                raise
            except (OSError, RuntimeError, ValueError) as error:
                raise RendererError("invalid_pdf", 502) from error
            total_payload += len(payload)
            if (
                len(payload) > settings.limits.max_page_bytes
                or total_payload > settings.limits.max_bundle_bytes
            ):
                raise RendererError("render_limit_exceeded", 422)
            relative = f"pages/page-{index + 1:04d}.png"
            destination = page_root / relative
            destination.write_bytes(payload)
            destination.chmod(0o600)
            artifacts.append(
                PageArtifact(
                    page=index + 1,
                    width=width,
                    height=height,
                    path=relative,
                    sha256=hashlib.sha256(payload).hexdigest(),
                    byte_size=len(payload),
                )
            )
    finally:
        document.close()
    duration_ms = max(0, int((time.monotonic() - started) * 1000))
    manifest = build_manifest(
        settings.descriptor,
        source_format,
        source_size,
        source_sha256,
        total_pages,
        artifacts,
        duration_ms,
    )
    bundle_path = request_dir / "render.tar"
    write_bundle(bundle_path, manifest, page_root, settings.limits)
    return bundle_path


def _error_response(error: RendererError) -> JSONResponse:
    return JSONResponse(status_code=error.status_code, content={"error": {"code": error.code}})


def create_app(
    settings: Settings | None = None, converter: RenderConverter = libreoffice_convert
) -> FastAPI:
    resolved = settings or Settings.from_env()
    app = FastAPI(
        title="bkcrab parser evaluation renderer", docs_url=None, redoc_url=None, openapi_url=None
    )

    @app.exception_handler(RendererError)
    async def _renderer_error(_request: Request, error: RendererError) -> JSONResponse:
        return _error_response(error)

    @app.get("/healthz")
    async def healthz() -> dict[str, object]:
        return health_document(resolved.descriptor, resolved.limits)

    @app.post("/v1/render")
    async def render(request: Request) -> FileResponse:
        query_items = request.query_params.multi_items()
        if (
            len(query_items) != 1
            or query_items[0][0] != "format"
            or query_items[0][1] not in FORMATS
        ):
            raise RendererError("unsupported_format", 400)
        source_format = query_items[0][1]
        try:
            form = await request.form(
                max_files=2, max_fields=1, max_part_size=resolved.limits.max_input_bytes
            )
        except Exception as error:
            raise RendererError("invalid_multipart", 400) from error
        try:
            items = form.multi_items()
            if len(items) != 1 or items[0][0] != "file" or not isinstance(items[0][1], UploadFile):
                raise RendererError("invalid_multipart", 400)
            upload = items[0][1]
            filename = upload.filename or ""
            if (
                Path(filename).name != filename
                or any(character in filename for character in ("/", "\\", "\r", "\n", "\x00"))
                or Path(filename).suffix.lower() != f".{source_format}"
                or upload.content_type != MIME_TYPES[source_format]
            ):
                raise RendererError("invalid_multipart", 400)
            request_dir = _request_directory(resolved)
            keep_directory = False
            try:
                source = request_dir / f"source.{source_format}"
                source_sha256, source_size = await _save_upload(
                    request, upload, source, resolved.limits.max_input_bytes
                )
                preflight_ooxml(source, source_format, resolved.limits)
                bundle = await asyncio.to_thread(
                    _render_bundle,
                    source,
                    source_format,
                    source_sha256,
                    source_size,
                    request_dir,
                    resolved,
                    converter,
                )
                keep_directory = True
                return FileResponse(
                    bundle,
                    media_type="application/x-tar",
                    filename="render.tar",
                    background=BackgroundTask(shutil.rmtree, request_dir, ignore_errors=True),
                )
            finally:
                if not keep_directory:
                    shutil.rmtree(request_dir, ignore_errors=True)
        finally:
            await form.close()

    return app


app = create_app()


def main() -> None:
    uvicorn.run("app.main:app", host="0.0.0.0", port=8080, access_log=False)


if __name__ == "__main__":
    main()
