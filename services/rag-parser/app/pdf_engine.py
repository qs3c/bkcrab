from __future__ import annotations

import math
import re
import threading
from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path
from typing import Protocol, runtime_checkable

try:
    import pypdfium2 as pdfium
    import pypdfium2.raw as pdfium_raw
    from pypdfium2.version import PYPDFIUM_INFO
except Exception as exc:  # Keep Office available if the native PDFium binary cannot load.
    pdfium = None
    pdfium_raw = None
    PYPDFIUM_INFO = None
    _PDFIUM_IMPORT_ERROR: Exception | None = exc
else:
    _PDFIUM_IMPORT_ERROR = None

NormalizedBBox = tuple[int, int, int, int]

_PDFIUM_LOCK = threading.RLock()
_TABLE_RE = re.compile(r"(?:\S\s{2,}\S\s{2,}\S)|(?:^|\n)\s*\|.+\|\s*(?:\n|$)")
_CODE_RE = re.compile(
    r"(?:^|\n)\s*(?:def |class |func |package |import |SELECT |CREATE |[}\]])|"
    r"(?:=>|:=|==|!=|\{\s*$)",
    re.MULTILINE,
)


class PDFEngineError(ValueError):
    """A document-level PDF engine failure safe to expose as a typed API error."""

    def __init__(self, code: str, message: str, status_code: int = 422):
        super().__init__(message)
        self.code = code
        self.status_code = status_code


class PDFEngineCancelled(RuntimeError):
    """Raised at a safe page/object boundary after cancellation is requested."""


@dataclass(frozen=True)
class PDFTextBlock:
    text: str
    bbox: NormalizedBBox


@dataclass(frozen=True)
class PDFEmbeddedImage:
    bbox: NormalizedBBox


@dataclass(frozen=True)
class PDFSignals:
    table: bool
    code: bool
    scanned: bool
    multicolumn: bool
    reading_order_uncertain: bool


@dataclass(frozen=True)
class PDFPageAnalysis:
    page: int
    width: float
    height: float
    native_markdown: str
    text_blocks: tuple[PDFTextBlock, ...]
    embedded_images: tuple[PDFEmbeddedImage, ...]
    signals: PDFSignals


@dataclass(frozen=True)
class PDFPageFailure:
    page: int
    error_code: str


@dataclass(frozen=True)
class PDFEngineWarning:
    code: str
    message: str


@dataclass(frozen=True)
class PDFRenderedImage:
    path: Path
    width: int
    height: int
    bbox: NormalizedBBox


@dataclass(frozen=True)
class PDFPageRender:
    page: int
    render_path: Path
    width: int
    height: int
    embedded_images: tuple[PDFRenderedImage, ...]
    warnings: tuple[PDFEngineWarning, ...] = field(default=())


@runtime_checkable
class PDFEngine(Protocol):
    """Engine-neutral boundary used by the HTTP/protocol layer."""

    name: str
    version: str

    def analyze(
        self,
        source: Path,
        *,
        max_pages: int,
        cancelled: Callable[[], bool],
    ) -> tuple[PDFPageAnalysis | PDFPageFailure, ...]: ...

    def render(
        self,
        source: Path,
        pages: tuple[int, ...],
        output_dir: Path,
        *,
        max_pages: int,
        dpi: int,
        max_image_pixels: int,
        max_assets: int,
        max_asset_bytes: int,
        cancelled: Callable[[], bool],
    ) -> tuple[PDFPageRender | PDFPageFailure, ...]: ...


def _check_cancelled(cancelled: Callable[[], bool]) -> None:
    if cancelled():
        raise PDFEngineCancelled


def _normalize_text(value: str) -> str:
    normalized = value.replace("\x00", "").replace("\r\n", "\n").replace("\r", "\n")
    normalized = "\n".join(line.rstrip() for line in normalized.splitlines()).strip()
    return f"{normalized}\n" if normalized else ""


def _normalize_bbox(
    bounds: tuple[float, float, float, float], width: float, height: float
) -> NormalizedBBox | None:
    left, bottom, right, top = (float(value) for value in bounds)
    if not all(math.isfinite(value) for value in (left, bottom, right, top)):
        return None
    left, right = sorted((left, right))
    bottom, top = sorted((bottom, top))
    if right <= left or top <= bottom:
        return None

    def bounded(value: float) -> int:
        return min(1000, max(0, round(value)))

    result = (
        bounded(left * 1000 / width),
        bounded((height - top) * 1000 / height),
        bounded(right * 1000 / width),
        bounded((height - bottom) * 1000 / height),
    )
    if result[2] <= result[0] or result[3] <= result[1]:
        return None
    return result


def _bbox_area(value: NormalizedBBox) -> float:
    return (value[2] - value[0]) * (value[3] - value[1]) / 1_000_000


def _looks_multicolumn(blocks: tuple[PDFTextBlock, ...]) -> bool:
    if len(blocks) < 2:
        return False
    left = [block for block in blocks if block.bbox[2] <= 560]
    right = [block for block in blocks if block.bbox[0] >= 440]
    if not left or not right:
        return False
    return any(
        max(left_block.bbox[1], right_block.bbox[1])
        < min(left_block.bbox[3], right_block.bbox[3])
        for left_block in left
        for right_block in right
    )


class Pypdfium2PDFEngine:
    """License-approved PDFium adapter; no PDFium handle crosses this class."""

    name = "pypdfium2"
    version = str(PYPDFIUM_INFO) if PYPDFIUM_INFO is not None else "5.12.1"

    def __init__(self) -> None:
        if _PDFIUM_IMPORT_ERROR is not None:
            raise PDFEngineError(
                "pdf_engine_unavailable", "the approved PDFium binary could not be loaded", 503
            ) from _PDFIUM_IMPORT_ERROR
        if self.version != "5.12.1":
            raise PDFEngineError(
                "pdf_engine_version_mismatch",
                "the installed pypdfium2 version does not match the approved ADR",
                503,
            )

    def analyze(
        self,
        source: Path,
        *,
        max_pages: int,
        cancelled: Callable[[], bool],
    ) -> tuple[PDFPageAnalysis | PDFPageFailure, ...]:
        with _PDFIUM_LOCK:
            _check_cancelled(cancelled)
            document = self._open(source)
            try:
                page_count = len(document)
                self._validate_page_count(page_count, max_pages)
                results: list[PDFPageAnalysis | PDFPageFailure] = []
                for page_index in range(page_count):
                    _check_cancelled(cancelled)
                    page_number = page_index + 1
                    page = document[page_index]
                    try:
                        results.append(self._analyze_page(page, page_number, cancelled))
                    except PDFEngineCancelled:
                        raise
                    except Exception:
                        results.append(PDFPageFailure(page_number, "page_analyze_failed"))
                    finally:
                        page.close()
                return tuple(results)
            finally:
                document.close()

    def render(
        self,
        source: Path,
        pages: tuple[int, ...],
        output_dir: Path,
        *,
        max_pages: int,
        dpi: int,
        max_image_pixels: int,
        max_assets: int,
        max_asset_bytes: int,
        cancelled: Callable[[], bool],
    ) -> tuple[PDFPageRender | PDFPageFailure, ...]:
        if dpi <= 0:
            raise PDFEngineError("invalid_dpi", "PDF render DPI must be positive", 500)
        output_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
        with _PDFIUM_LOCK:
            _check_cancelled(cancelled)
            document = self._open(source)
            try:
                page_count = len(document)
                self._validate_page_count(page_count, max_pages)
                results: list[PDFPageRender | PDFPageFailure] = []
                extracted_count = 0
                for page_number in pages:
                    _check_cancelled(cancelled)
                    if page_number < 1 or page_number > page_count:
                        results.append(PDFPageFailure(page_number, "invalid_page"))
                        continue
                    page = document[page_number - 1]
                    try:
                        result, used = self._render_page(
                            page,
                            page_number,
                            output_dir,
                            dpi=dpi,
                            max_image_pixels=max_image_pixels,
                            remaining_assets=max_assets - extracted_count,
                            max_asset_bytes=max_asset_bytes,
                            cancelled=cancelled,
                        )
                        extracted_count += used
                        results.append(result)
                    except PDFEngineCancelled:
                        raise
                    except Exception:
                        self._remove_page_outputs(output_dir, page_number)
                        results.append(PDFPageFailure(page_number, "page_render_failed"))
                    finally:
                        page.close()
                return tuple(results)
            finally:
                document.close()

    @staticmethod
    def _open(source: Path):
        try:
            return pdfium.PdfDocument(source)
        except Exception as exc:
            raise PDFEngineError("invalid_pdf", "PDFium could not open the PDF") from exc

    @staticmethod
    def _validate_page_count(page_count: int, max_pages: int) -> None:
        if page_count <= 0:
            raise PDFEngineError("invalid_pdf", "PDF has no pages")
        if page_count > max_pages:
            raise PDFEngineError(
                "page_limit_exceeded",
                f"PDF page limit exceeded ({page_count} > {max_pages})",
                413,
            )

    @staticmethod
    def _analyze_page(page, page_number: int, cancelled: Callable[[], bool]) -> PDFPageAnalysis:
        width, height = (float(value) for value in page.get_size())
        if not all(math.isfinite(value) and value > 0 for value in (width, height)):
            raise ValueError("invalid page dimensions")
        text_page = page.get_textpage()
        try:
            native_markdown = _normalize_text(text_page.get_text_bounded())
            blocks: list[PDFTextBlock] = []
            for obj in page.get_objects(
                filter=(pdfium_raw.FPDF_PAGEOBJ_TEXT,), textpage=text_page
            ):
                _check_cancelled(cancelled)
                try:
                    text = _normalize_text(obj.extract()).strip()
                    bbox = _normalize_bbox(obj.get_bounds(), width, height)
                except Exception:
                    continue
                if text and bbox is not None:
                    blocks.append(PDFTextBlock(text, bbox))
        finally:
            text_page.close()

        image_boxes: list[PDFEmbeddedImage] = []
        for obj in page.get_objects(filter=(pdfium_raw.FPDF_PAGEOBJ_IMAGE,)):
            _check_cancelled(cancelled)
            try:
                bbox = _normalize_bbox(obj.get_bounds(), width, height)
            except Exception:
                continue
            if bbox is not None:
                image_boxes.append(PDFEmbeddedImage(bbox))

        block_values = tuple(blocks)
        image_values = tuple(image_boxes)
        text_chars = sum(not char.isspace() for char in native_markdown)
        image_coverage = min(1.0, sum(_bbox_area(item.bbox) for item in image_values))
        multicolumn = _looks_multicolumn(block_values)
        signals = PDFSignals(
            table=bool(_TABLE_RE.search(native_markdown)),
            code=bool(_CODE_RE.search(native_markdown)),
            scanned=bool(image_values) and text_chars < 80 and image_coverage >= 0.5,
            multicolumn=multicolumn,
            reading_order_uncertain=multicolumn,
        )
        return PDFPageAnalysis(
            page=page_number,
            width=width,
            height=height,
            native_markdown=native_markdown,
            text_blocks=block_values,
            embedded_images=image_values,
            signals=signals,
        )

    @classmethod
    def _render_page(
        cls,
        page,
        page_number: int,
        output_dir: Path,
        *,
        dpi: int,
        max_image_pixels: int,
        remaining_assets: int,
        max_asset_bytes: int,
        cancelled: Callable[[], bool],
    ) -> tuple[PDFPageRender, int]:
        width, height = (float(value) for value in page.get_size())
        scale = dpi / 72
        pixel_width = math.ceil(width * scale)
        pixel_height = math.ceil(height * scale)
        if (
            pixel_width <= 0
            or pixel_height <= 0
            or pixel_width * pixel_height > max_image_pixels
        ):
            raise ValueError("rendered page exceeds pixel limit")
        render_path = output_dir / f"render-{page_number:04d}.png"
        bitmap = page.render(scale=scale, rotation=0, maybe_alpha=True, rev_byteorder=True)
        try:
            image = bitmap.to_pil()
            safe_image = image.convert("RGB")
            safe_image.save(render_path, format="PNG", optimize=False)
            rendered_width, rendered_height = safe_image.size
            safe_image.close()
        finally:
            bitmap.close()
        if rendered_width * rendered_height > max_image_pixels:
            raise ValueError("rendered page exceeds pixel limit")

        embedded: list[PDFRenderedImage] = []
        warnings: list[PDFEngineWarning] = []
        objects = page.get_objects(filter=(pdfium_raw.FPDF_PAGEOBJ_IMAGE,))
        for object_index, obj in enumerate(objects, 1):
            _check_cancelled(cancelled)
            if len(embedded) >= remaining_assets:
                warnings.append(
                    PDFEngineWarning(
                        "pdf_embedded_image_limit",
                        "内嵌图片数量超过文档限制，已停止提取其余图片",
                    )
                )
                break
            image_path = output_dir / f"embedded-{page_number:04d}-{object_index:04d}.png"
            try:
                bbox = _normalize_bbox(obj.get_bounds(), width, height)
                if bbox is None:
                    raise ValueError("invalid image bounds")
                image_bitmap = obj.get_bitmap(render=True, scale_to_original=True)
                try:
                    pil_image = image_bitmap.to_pil()
                    image_width, image_height = pil_image.size
                    if (
                        image_width <= 0
                        or image_height <= 0
                        or image_width * image_height > max_image_pixels
                    ):
                        raise ValueError("embedded image exceeds pixel limit")
                    safe_embedded = pil_image.convert("RGB")
                    safe_embedded.save(image_path, format="PNG", optimize=False)
                    safe_embedded.close()
                finally:
                    image_bitmap.close()
                if image_path.stat().st_size > max_asset_bytes:
                    image_path.unlink(missing_ok=True)
                    raise ValueError("embedded image exceeds byte limit")
                embedded.append(
                    PDFRenderedImage(image_path, image_width, image_height, bbox)
                )
            except PDFEngineCancelled:
                raise
            except Exception:
                image_path.unlink(missing_ok=True)
                warnings.append(
                    PDFEngineWarning(
                        "pdf_embedded_image_failed",
                        "内嵌图片未能通过安全提取限制",
                    )
                )
        return (
            PDFPageRender(
                page_number,
                render_path,
                rendered_width,
                rendered_height,
                tuple(embedded),
                tuple(warnings),
            ),
            len(embedded),
        )

    @staticmethod
    def _remove_page_outputs(output_dir: Path, page_number: int) -> None:
        for path in output_dir.glob(f"*-{page_number:04d}*.png"):
            path.unlink(missing_ok=True)
