from __future__ import annotations

import hashlib
import io
import json
import math
import os
import re
import tarfile
import zipfile
from dataclasses import dataclass
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any
from xml.etree import ElementTree

PROTOCOL_VERSION = "parser-eval-renderer/v1"
FORMATS = ("docx", "pptx", "xlsx")
MIME_TYPES = {
    "docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    "xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}
REQUIRED_PARTS = {
    "docx": "word/document.xml",
    "pptx": "ppt/presentation.xml",
    "xlsx": "xl/workbook.xml",
}
ERROR_CODES = frozenset(
    {
        "invalid_multipart",
        "unsupported_format",
        "invalid_ooxml",
        "input_too_large",
        "render_timeout",
        "libreoffice_failed",
        "invalid_pdf",
        "render_limit_exceeded",
        "bundle_limit_exceeded",
    }
)
_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")


class RendererError(RuntimeError):
    def __init__(self, code: str, status_code: int):
        if code not in ERROR_CODES:
            raise ValueError(f"unknown renderer error code {code}")
        super().__init__(code)
        self.code = code
        self.status_code = status_code


@dataclass(frozen=True)
class RendererDescriptor:
    service_version: str
    libreoffice_version: str
    pymupdf_version: str

    def to_dict(self) -> dict[str, str]:
        return {
            "protocolVersion": PROTOCOL_VERSION,
            "serviceVersion": self.service_version,
            "libreOfficeVersion": self.libreoffice_version,
            "pyMuPDFVersion": self.pymupdf_version,
        }


@dataclass(frozen=True)
class RendererLimits:
    max_input_bytes: int = 50 * 1024 * 1024
    max_pages: int = 6
    render_dpi: int = 100
    max_archive_entries: int = 10_000
    max_zip_entry_bytes: int = 100 * 1024 * 1024
    max_extracted_bytes: int = 500 * 1024 * 1024
    max_compression_ratio: int = 200
    max_page_pixels: int = 40_000_000
    max_page_bytes: int = 20 * 1024 * 1024
    max_bundle_bytes: int = 128 * 1024 * 1024

    def validate(self) -> None:
        values = vars(self)
        if any(
            isinstance(value, bool) or not isinstance(value, int) or value <= 0
            for value in values.values()
        ):
            raise RuntimeError("renderer limits must be positive integers")
        if self.max_pages > 100 or not 36 <= self.render_dpi <= 300:
            raise RuntimeError("renderer page limits are invalid")
        if self.max_page_bytes > self.max_bundle_bytes:
            raise RuntimeError("page limit cannot exceed bundle limit")

    def to_dict(self) -> dict[str, int]:
        return {
            "maxInputBytes": self.max_input_bytes,
            "maxPages": self.max_pages,
            "renderDPI": self.render_dpi,
            "maxPagePixels": self.max_page_pixels,
            "maxPageBytes": self.max_page_bytes,
            "maxBundleBytes": self.max_bundle_bytes,
        }


@dataclass(frozen=True)
class PageArtifact:
    page: int
    width: int
    height: int
    path: str
    sha256: str
    byte_size: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "page": self.page,
            "width": self.width,
            "height": self.height,
            "path": self.path,
            "mediaType": "image/png",
            "sha256": self.sha256,
            "byteSize": self.byte_size,
        }


def health_document(descriptor: RendererDescriptor, limits: RendererLimits) -> dict[str, Any]:
    return {
        "descriptor": descriptor.to_dict(),
        "formats": list(FORMATS),
        "limits": limits.to_dict(),
    }


def sha256_file(path: Path) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as source:
        while chunk := source.read(64 * 1024):
            digest.update(chunk)
            size += len(chunk)
    return digest.hexdigest(), size


def _safe_archive_name(value: str) -> bool:
    if not value or "\x00" in value or "\\" in value or value.startswith(("/", "//")):
        return False
    if PureWindowsPath(value).drive:
        return False
    candidate = value[:-1] if value.endswith("/") else value
    path = PurePosixPath(candidate)
    return (
        bool(candidate)
        and not path.is_absolute()
        and all(part not in {"", ".", ".."} for part in path.parts)
        and path.as_posix() == candidate
    )


def preflight_ooxml(path: Path, source_format: str, limits: RendererLimits) -> None:
    if source_format not in FORMATS:
        raise RendererError("unsupported_format", 400)
    try:
        with zipfile.ZipFile(path) as archive:
            entries = archive.infolist()
            if not entries or len(entries) > limits.max_archive_entries:
                raise RendererError("invalid_ooxml", 422)
            names: set[str] = set()
            total = 0
            for entry in entries:
                if not _safe_archive_name(entry.filename) or entry.filename in names:
                    raise RendererError("invalid_ooxml", 422)
                names.add(entry.filename)
                if entry.is_dir():
                    continue
                if entry.file_size > limits.max_zip_entry_bytes:
                    raise RendererError("invalid_ooxml", 422)
                total += entry.file_size
                if total > limits.max_extracted_bytes:
                    raise RendererError("invalid_ooxml", 422)
                if entry.file_size and (
                    entry.compress_size == 0
                    or entry.file_size / entry.compress_size > limits.max_compression_ratio
                ):
                    raise RendererError("invalid_ooxml", 422)
            required = ("[Content_Types].xml", REQUIRED_PARTS[source_format])
            if any(name not in names for name in required):
                raise RendererError("invalid_ooxml", 422)
            for name in required:
                try:
                    with archive.open(name) as xml_part:
                        ElementTree.parse(xml_part)
                except (
                    ElementTree.ParseError,
                    OSError,
                    RuntimeError,
                    ValueError,
                    zipfile.BadZipFile,
                ) as error:
                    raise RendererError("invalid_ooxml", 422) from error
            if archive.testzip() is not None:
                raise RendererError("invalid_ooxml", 422)
    except RendererError:
        raise
    except (OSError, RuntimeError, ValueError, zipfile.BadZipFile, zipfile.LargeZipFile) as error:
        raise RendererError("invalid_ooxml", 422) from error


def build_manifest(
    descriptor: RendererDescriptor,
    source_format: str,
    source_size: int,
    source_sha256: str,
    total_pages: int,
    pages: list[PageArtifact],
    duration_ms: int,
) -> dict[str, Any]:
    if source_format not in FORMATS or source_size <= 0 or not _SHA256_RE.fullmatch(source_sha256):
        raise ValueError("invalid source identity")
    if total_pages <= 0 or len(pages) > total_pages or duration_ms < 0:
        raise ValueError("invalid render result")
    for expected, page in enumerate(pages, start=1):
        if (
            page.page != expected
            or page.width <= 0
            or page.height <= 0
            or page.byte_size <= 0
            or not _SHA256_RE.fullmatch(page.sha256)
        ):
            raise ValueError("invalid rendered page")
    return {
        "protocolVersion": PROTOCOL_VERSION,
        "descriptor": descriptor.to_dict(),
        "source": {"format": source_format, "byteSize": source_size, "sha256": source_sha256},
        "totalPages": total_pages,
        "coveredPages": len(pages),
        "renderDurationMs": duration_ms,
        "pages": [page.to_dict() for page in pages],
    }


def write_bundle(
    destination: Path, manifest: dict[str, Any], page_root: Path, limits: RendererLimits
) -> None:
    manifest_bytes = json.dumps(manifest, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    if len(manifest_bytes) > 1024 * 1024:
        raise RendererError("bundle_limit_exceeded", 422)
    with tarfile.open(destination, "w", format=tarfile.USTAR_FORMAT) as archive:
        info = tarfile.TarInfo("manifest.json")
        info.size = len(manifest_bytes)
        info.mode = 0o600
        info.mtime = 0
        archive.addfile(info, io.BytesIO(manifest_bytes))
        for page in manifest["pages"]:
            page_path = page_root / page["path"]
            if not page_path.is_file():
                raise RendererError("bundle_limit_exceeded", 422)
            info = archive.gettarinfo(str(page_path), arcname=page["path"])
            info.mode = 0o600
            info.mtime = 0
            with page_path.open("rb") as payload:
                archive.addfile(info, payload)
    if destination.stat().st_size > limits.max_bundle_bytes:
        destination.unlink(missing_ok=True)
        raise RendererError("bundle_limit_exceeded", 422)


def env_positive_int(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as error:
        raise RuntimeError(f"{name} must be a positive integer") from error
    if value <= 0 or not math.isfinite(value):
        raise RuntimeError(f"{name} must be a positive integer")
    return value
