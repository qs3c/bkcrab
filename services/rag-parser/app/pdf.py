from __future__ import annotations

import math
import re
import shutil
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path

from .pdf_engine import (
    PDFEngine,
    PDFEngineWarning,
    PDFPageAnalysis,
    PDFPageFailure,
    PDFPageRender,
)
from .protocol import (
    PDF_ANALYZE_BUNDLE_KIND,
    PDF_PAGE_ERROR_CODES,
    PDF_RENDER_BUNDLE_KIND,
    PROTOCOL_VERSION,
    Bundle,
    Manifest,
    ManifestAsset,
    ManifestOccurrence,
    ManifestWarning,
    ParserDescriptor,
    PayloadEntry,
    PDFPage,
    ProtocolError,
    SourceDescriptor,
    SourceLocation,
    canonical_json_bytes,
    sha256_file,
    validate_page_primitive_document,
)

PDF_WRAPPER_VERSION = "pdf-wrapper-v1"
_PAGE_LIST_RE = re.compile(r"^[1-9][0-9]*(?:,[1-9][0-9]*)*$")


class PDFError(ValueError):
    def __init__(self, code: str, message: str, status_code: int = 422):
        super().__init__(message)
        self.code = code
        self.status_code = status_code


@dataclass(frozen=True)
class PDFLimits:
    max_pages: int = 300
    render_dpi: int = 180
    max_image_pixels: int = 40_000_000
    max_assets: int = 500
    max_asset_bytes: int = 20 * 1024 * 1024

    def validate(self) -> None:
        for name, value in (
            ("max_pages", self.max_pages),
            ("render_dpi", self.render_dpi),
            ("max_image_pixels", self.max_image_pixels),
            ("max_assets", self.max_assets),
            ("max_asset_bytes", self.max_asset_bytes),
        ):
            if value <= 0:
                raise RuntimeError(f"PDFLimits.{name} must be positive")


def parse_page_allowlist(value: str, max_pages: int) -> tuple[int, ...]:
    if not _PAGE_LIST_RE.fullmatch(value):
        raise PDFError(
            "invalid_page_allowlist",
            "pages must be a comma-separated list of unique positive integers",
            400,
        )
    pages = tuple(sorted(int(part) for part in value.split(",")))
    if len(pages) != len(set(pages)):
        raise PDFError("invalid_page_allowlist", "pages must not contain duplicates", 400)
    if len(pages) > max_pages or pages[-1] > max_pages:
        raise PDFError(
            "page_limit_exceeded",
            "requested page allowlist exceeds the configured page limit",
            400,
        )
    return pages


def _unit_id(source_sha256: str, page: int) -> str:
    return f"unit_page_{source_sha256[:12]}_{page:04d}"


def _location(page: int) -> SourceLocation:
    return SourceLocation("page", page, f"第 {page} 页")


def _parser(engine: PDFEngine) -> ParserDescriptor:
    return ParserDescriptor(engine.name, engine.version, PDF_WRAPPER_VERSION)


def _validate_failure(value: PDFPageFailure, expected_page: int) -> None:
    if value.page != expected_page or value.error_code not in PDF_PAGE_ERROR_CODES:
        raise ProtocolError(
            "invalid_pdf_engine_result",
            "PDF engine returned an invalid failed page record",
        )


def _validate_bbox(value: tuple[int, int, int, int], where: str) -> None:
    if (
        len(value) != 4
        or any(isinstance(item, bool) or not isinstance(item, int) for item in value)
        or any(item < 0 or item > 1000 for item in value)
        or value[2] <= value[0]
        or value[3] <= value[1]
    ):
        raise ProtocolError("invalid_pdf_engine_result", f"{where} is not a normalized bbox")


def _failure_warning(page: int, operation: str) -> ManifestWarning:
    return ManifestWarning(
        code="pdf_page_failed",
        message=f"第 {page} 页{operation}失败",
        location=_location(page),
        degraded=True,
    )


def _engine_warning(page: int, warning: PDFEngineWarning) -> ManifestWarning:
    return ManifestWarning(
        code=warning.code,
        message=warning.message,
        location=_location(page),
        degraded=True,
    )


def _validate_engine_catalog(
    values: tuple[PDFPageAnalysis | PDFPageRender | PDFPageFailure, ...],
    expected_pages: tuple[int, ...],
) -> None:
    if len(values) != len(expected_pages):
        raise ProtocolError(
            "invalid_pdf_engine_result",
            "PDF engine omitted or added a page record",
        )
    actual_pages = tuple(value.page for value in values)
    if actual_pages != expected_pages:
        raise ProtocolError(
            "invalid_pdf_engine_result",
            "PDF engine page records do not match the requested ordered catalog",
        )


def _primitive(value: PDFPageAnalysis) -> dict[str, object]:
    if (
        not math.isfinite(value.width)
        or not math.isfinite(value.height)
        or value.width <= 0
        or value.height <= 0
    ):
        raise ProtocolError("invalid_pdf_engine_result", "PDF page dimensions are invalid")
    for index, block in enumerate(value.text_blocks):
        if not block.text:
            raise ProtocolError("invalid_pdf_engine_result", "PDF text block is empty")
        _validate_bbox(block.bbox, f"textBlocks[{index}].bbox")
    for index, image in enumerate(value.embedded_images):
        _validate_bbox(image.bbox, f"embeddedImages[{index}].bbox")
    text_coverage = min(
        1.0,
        sum(
            (block.bbox[2] - block.bbox[0])
            * (block.bbox[3] - block.bbox[1])
            / 1_000_000
            for block in value.text_blocks
        ),
    )
    primitive = {
        "page": value.page,
        "width": float(value.width),
        "height": float(value.height),
        "textChars": sum(not char.isspace() for char in value.native_markdown),
        "blockCount": len(value.text_blocks),
        "textCoverage": text_coverage,
        "textBlocks": [
            {"text": block.text, "bbox": list(block.bbox)} for block in value.text_blocks
        ],
        "embeddedImages": [{"bbox": list(image.bbox)} for image in value.embedded_images],
        "signals": {
            "table": value.signals.table,
            "code": value.signals.code,
            "scanned": value.signals.scanned,
            "multicolumn": value.signals.multicolumn,
            "readingOrderUncertain": value.signals.reading_order_uncertain,
        },
    }
    validate_page_primitive_document(primitive)
    return primitive


def build_pdf_analyze_bundle(
    *,
    source: Path,
    source_sha256: str,
    source_size: int,
    request_dir: Path,
    engine: PDFEngine,
    limits: PDFLimits,
    cancelled: Callable[[], bool],
) -> Bundle:
    limits.validate()
    values = engine.analyze(source, max_pages=limits.max_pages, cancelled=cancelled)
    if not values:
        raise ProtocolError("invalid_pdf_engine_result", "PDF analyze returned no pages")
    expected_pages = tuple(range(1, len(values) + 1))
    _validate_engine_catalog(values, expected_pages)
    if len(values) > limits.max_pages:
        raise PDFError("page_limit_exceeded", "PDF page count exceeds the limit", 413)

    payloads: list[PayloadEntry] = []
    pages: list[PDFPage] = []
    warnings: list[ManifestWarning] = []
    for page_number, value in enumerate(values, 1):
        unit_id = _unit_id(source_sha256, page_number)
        if isinstance(value, PDFPageFailure):
            _validate_failure(value, page_number)
            pages.append(PDFPage(page_number, "failed", value.error_code, unit_id, "", "", ""))
            warnings.append(_failure_warning(page_number, "分析"))
            continue
        if not isinstance(value, PDFPageAnalysis):
            raise ProtocolError(
                "invalid_pdf_engine_result", "PDF analyze returned an unknown result type"
            )
        native_path = f"units/page-{page_number:04d}.md"
        primitive_path = f"pages/page-{page_number:04d}.json"
        payloads.extend(
            (
                PayloadEntry.from_bytes(
                    native_path,
                    "text/markdown; charset=utf-8",
                    value.native_markdown.encode("utf-8"),
                ),
                PayloadEntry.from_bytes(
                    primitive_path,
                    "application/json",
                    canonical_json_bytes(_primitive(value)),
                ),
            )
        )
        pages.append(
            PDFPage(page_number, "ok", "", unit_id, native_path, "", primitive_path)
        )

    payloads.sort(key=lambda item: item.path)
    manifest = Manifest(
        protocol_version=PROTOCOL_VERSION,
        bundle_kind=PDF_ANALYZE_BUNDLE_KIND,
        source=SourceDescriptor("pdf", source_size, source_sha256),
        parser=_parser(engine),
        entries=tuple(payload.descriptor() for payload in payloads),
        units=(),
        assets=(),
        occurrences=(),
        pages=tuple(pages),
        warnings=tuple(warnings),
    )
    manifest.validate()
    return Bundle(
        manifest=manifest,
        payloads=tuple(payloads),
        cleanup=lambda: shutil.rmtree(request_dir, ignore_errors=True),
    )


def _safe_output_file(path: Path, request_dir: Path, where: str) -> Path:
    try:
        resolved = path.resolve(strict=True)
    except OSError as exc:
        raise ProtocolError("invalid_pdf_engine_result", f"{where} is missing") from exc
    root = request_dir.resolve(strict=True)
    if not resolved.is_relative_to(root) or path.is_symlink() or not resolved.is_file():
        raise ProtocolError(
            "invalid_pdf_engine_result", f"{where} escaped the request directory"
        )
    return resolved


def _validate_render_value(value: PDFPageRender, limits: PDFLimits, request_dir: Path) -> None:
    _safe_output_file(value.render_path, request_dir, "render_path")
    if (
        value.width <= 0
        or value.height <= 0
        or value.width * value.height > limits.max_image_pixels
    ):
        raise ProtocolError("invalid_pdf_engine_result", "render dimensions exceed limits")
    if len(value.embedded_images) > limits.max_assets:
        raise ProtocolError("invalid_pdf_engine_result", "embedded image count exceeds limits")
    for index, image in enumerate(value.embedded_images):
        _safe_output_file(image.path, request_dir, f"embedded_images[{index}].path")
        _validate_bbox(image.bbox, f"embedded_images[{index}].bbox")
        if (
            image.width <= 0
            or image.height <= 0
            or image.width * image.height > limits.max_image_pixels
            or image.path.stat().st_size > limits.max_asset_bytes
        ):
            raise ProtocolError(
                "invalid_pdf_engine_result", "embedded image dimensions/bytes exceed limits"
            )


def build_pdf_render_bundle(
    *,
    source: Path,
    source_sha256: str,
    source_size: int,
    request_dir: Path,
    requested_pages: tuple[int, ...],
    engine: PDFEngine,
    limits: PDFLimits,
    cancelled: Callable[[], bool],
) -> Bundle:
    limits.validate()
    if (
        not requested_pages
        or requested_pages != tuple(sorted(set(requested_pages)))
        or requested_pages[-1] > limits.max_pages
    ):
        raise PDFError("invalid_page_allowlist", "requested pages are not a valid allowlist", 400)
    output_dir = request_dir / "pdf-output"
    output_dir.mkdir(mode=0o700)
    values = engine.render(
        source,
        requested_pages,
        output_dir,
        max_pages=limits.max_pages,
        dpi=limits.render_dpi,
        max_image_pixels=limits.max_image_pixels,
        max_assets=limits.max_assets,
        max_asset_bytes=limits.max_asset_bytes,
        cancelled=cancelled,
    )
    _validate_engine_catalog(values, requested_pages)

    payloads: list[PayloadEntry] = []
    pages: list[PDFPage] = []
    assets: list[ManifestAsset] = []
    occurrences: list[ManifestOccurrence] = []
    warnings: list[ManifestWarning] = []
    asset_by_sha256: dict[str, ManifestAsset] = {}
    total_embedded = 0
    for value in values:
        page_number = value.page
        unit_id = _unit_id(source_sha256, page_number)
        if isinstance(value, PDFPageFailure):
            _validate_failure(value, page_number)
            pages.append(PDFPage(page_number, "failed", value.error_code, unit_id, "", "", ""))
            warnings.append(_failure_warning(page_number, "渲染"))
            continue
        if not isinstance(value, PDFPageRender):
            raise ProtocolError(
                "invalid_pdf_engine_result", "PDF render returned an unknown result type"
            )
        _validate_render_value(value, limits, request_dir)
        render_entry = f"pages/page-{page_number:04d}.png"
        payloads.append(PayloadEntry.from_file(render_entry, "image/png", value.render_path))
        pages.append(PDFPage(page_number, "ok", "", unit_id, "", render_entry, ""))
        warnings.extend(_engine_warning(page_number, warning) for warning in value.warnings)

        for order, image in enumerate(value.embedded_images, 1):
            total_embedded += 1
            if total_embedded > limits.max_assets:
                raise ProtocolError(
                    "invalid_pdf_engine_result", "PDF embedded image count exceeds limits"
                )
            digest, _ = sha256_file(image.path)
            asset = asset_by_sha256.get(digest)
            if asset is None:
                local_id = f"asset_{len(assets) + 1:04d}"
                entry_path = f"assets/{local_id}.png"
                payloads.append(PayloadEntry.from_file(entry_path, "image/png", image.path))
                asset = ManifestAsset(
                    local_id,
                    entry_path,
                    "image",
                    "embedded_original",
                    image.width,
                    image.height,
                )
                assets.append(asset)
                asset_by_sha256[digest] = asset
            occurrences.append(
                ManifestOccurrence(
                    id=f"occ_page_{page_number:04d}_{order:04d}",
                    asset_local_id=asset.local_id,
                    unit_id=unit_id,
                    order=order,
                    location=_location(page_number),
                    bbox=image.bbox,
                    alt_text="",
                    caption="",
                    ocr_text="",
                    decorative=False,
                    confidence=1.0,
                )
            )

    payloads.sort(key=lambda item: item.path)
    manifest = Manifest(
        protocol_version=PROTOCOL_VERSION,
        bundle_kind=PDF_RENDER_BUNDLE_KIND,
        source=SourceDescriptor("pdf", source_size, source_sha256),
        parser=_parser(engine),
        entries=tuple(payload.descriptor() for payload in payloads),
        units=(),
        assets=tuple(assets),
        occurrences=tuple(occurrences),
        pages=tuple(pages),
        warnings=tuple(warnings),
    )
    manifest.validate()
    return Bundle(
        manifest=manifest,
        payloads=tuple(payloads),
        cleanup=lambda: shutil.rmtree(request_dir, ignore_errors=True),
    )


def ensure_exact_pdf_page_set(
    values: Iterable[PDFPageAnalysis | PDFPageRender | PDFPageFailure],
    requested_pages: tuple[int, ...],
) -> None:
    """Public test hook for fake engines that need contract validation without I/O."""

    _validate_engine_catalog(tuple(values), requested_pages)
