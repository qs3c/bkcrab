from __future__ import annotations

import hashlib
import io
import json
import math
import os
import re
import tarfile
from collections.abc import Callable, Iterable, Iterator, Mapping
from dataclasses import dataclass, field
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any, BinaryIO

PROTOCOL_VERSION = "rag-parser/v1"
OFFICE_BUNDLE_KIND = "office-convert"
PDF_ANALYZE_BUNDLE_KIND = "pdf-analyze"
PDF_RENDER_BUNDLE_KIND = "pdf-render"
MANIFEST_NAME = "manifest.json"
MANIFEST_MAX_BYTES = 1024 * 1024
TAR_BLOCK_SIZE = 512

_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
PDF_PAGE_ERROR_CODES = frozenset(
    {
        "engine_error",
        "invalid_page",
        "page_analyze_failed",
        "page_limit_exceeded",
        "page_render_failed",
        "timeout",
    }
)
_ENTRY_MIME_BY_EXTENSION = {
    ".md": "text/markdown; charset=utf-8",
    ".json": "application/json",
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".webp": "image/webp",
}
_ASSET_SOURCE_KINDS = frozenset({"embedded_original", "page_crop", "scanned_page"})


class ProtocolError(ValueError):
    """Raised when a manifest, health document, or bundle violates v1."""

    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


def _fail(code: str, message: str) -> None:
    raise ProtocolError(code, message)


def _mapping(value: Any, where: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        _fail("invalid_json_shape", f"{where} must be an object")
    return value


def _list(value: Any, where: str) -> list[Any]:
    if not isinstance(value, list):
        _fail("invalid_json_shape", f"{where} must be an array")
    return value


def _exact_keys(value: Mapping[str, Any], expected: set[str], where: str) -> None:
    actual = set(value)
    if actual != expected:
        missing = sorted(expected - actual)
        unknown = sorted(actual - expected)
        _fail(
            "invalid_json_fields",
            f"{where} has missing fields {missing} and unknown fields {unknown}",
        )


def _string(value: Any, where: str, *, allow_empty: bool = False) -> str:
    if not isinstance(value, str) or (not allow_empty and not value):
        _fail("invalid_json_value", f"{where} must be a string")
    return value


def _non_blank_string(value: Any, where: str) -> str:
    result = _string(value, where)
    if not result.strip():
        _fail("invalid_json_value", f"{where} must contain a non-whitespace character")
    return result


def _integer(value: Any, where: str, *, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        _fail("invalid_json_value", f"{where} must be an integer >= {minimum}")
    return value


def _number(
    value: Any,
    where: str,
    *,
    minimum: float,
    maximum: float | None = None,
) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        _fail("invalid_json_value", f"{where} must be a number")
    result = float(value)
    if not math.isfinite(result) or result < minimum or (
        maximum is not None and result > maximum
    ):
        boundary = f"between {minimum} and {maximum}" if maximum is not None else f">= {minimum}"
        _fail("invalid_json_value", f"{where} must be finite and {boundary}")
    return result


def _boolean(value: Any, where: str) -> bool:
    if not isinstance(value, bool):
        _fail("invalid_json_value", f"{where} must be a boolean")
    return value


def _sha256(value: Any, where: str) -> str:
    result = _string(value, where)
    if not _SHA256_RE.fullmatch(result):
        _fail("invalid_sha256", f"{where} must be 64 lowercase hexadecimal characters")
    return result


def _identifier(value: Any, where: str) -> str:
    result = _string(value, where)
    if not _ID_RE.fullmatch(result):
        _fail("invalid_identifier", f"{where} is not a canonical identifier")
    return result


def validate_bundle_path(value: str, where: str = "entry path") -> str:
    if not value or "\x00" in value or "\\" in value:
        _fail("unsafe_entry_path", f"{where} is not a safe relative POSIX path")
    if PureWindowsPath(value).drive or value.startswith(("/", "//")):
        _fail("unsafe_entry_path", f"{where} must be relative")
    path = PurePosixPath(value)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        _fail("unsafe_entry_path", f"{where} contains an unsafe segment")
    canonical = path.as_posix()
    if canonical != value or canonical == MANIFEST_NAME:
        _fail("unsafe_entry_path", f"{where} is not canonical")
    return canonical


@dataclass(frozen=True)
class SourceLocation:
    kind: str
    index: int
    label: str

    @classmethod
    def from_dict(cls, value: Any, where: str) -> SourceLocation:
        obj = _mapping(value, where)
        _exact_keys(obj, {"kind", "index", "label"}, where)
        kind = _string(obj["kind"], f"{where}.kind")
        if kind not in {"document", "page", "slide", "sheet"}:
            _fail("invalid_location", f"{where}.kind is unsupported")
        index = _integer(obj["index"], f"{where}.index")
        if (kind == "document" and index != 0) or (kind != "document" and index < 1):
            _fail("invalid_location", f"{where}.index is invalid for {kind}")
        return cls(kind=kind, index=index, label=_string(obj["label"], f"{where}.label", allow_empty=True))

    def to_dict(self) -> dict[str, Any]:
        return {"kind": self.kind, "index": self.index, "label": self.label}


@dataclass(frozen=True)
class SourceDescriptor:
    format: str
    byte_size: int
    sha256: str

    @classmethod
    def from_dict(cls, value: Any) -> SourceDescriptor:
        obj = _mapping(value, "source")
        _exact_keys(obj, {"format", "byteSize", "sha256"}, "source")
        return cls(
            format=_string(obj["format"], "source.format"),
            byte_size=_integer(obj["byteSize"], "source.byteSize", minimum=1),
            sha256=_sha256(obj["sha256"], "source.sha256"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {"format": self.format, "byteSize": self.byte_size, "sha256": self.sha256}


@dataclass(frozen=True)
class ParserDescriptor:
    name: str
    version: str
    wrapper_version: str

    @classmethod
    def from_dict(cls, value: Any) -> ParserDescriptor:
        obj = _mapping(value, "parser")
        _exact_keys(obj, {"name", "version", "wrapperVersion"}, "parser")
        return cls(
            name=_non_blank_string(obj["name"], "parser.name"),
            version=_non_blank_string(obj["version"], "parser.version"),
            wrapper_version=_non_blank_string(obj["wrapperVersion"], "parser.wrapperVersion"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "version": self.version,
            "wrapperVersion": self.wrapper_version,
        }


@dataclass(frozen=True)
class EntryDescriptor:
    path: str
    sha256: str
    byte_size: int
    mime_type: str

    @classmethod
    def from_dict(cls, value: Any, where: str) -> EntryDescriptor:
        obj = _mapping(value, where)
        _exact_keys(obj, {"path", "sha256", "byteSize", "mimeType"}, where)
        return cls(
            path=validate_bundle_path(_string(obj["path"], f"{where}.path"), f"{where}.path"),
            sha256=_sha256(obj["sha256"], f"{where}.sha256"),
            byte_size=_integer(obj["byteSize"], f"{where}.byteSize"),
            mime_type=_string(obj["mimeType"], f"{where}.mimeType"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "sha256": self.sha256,
            "byteSize": self.byte_size,
            "mimeType": self.mime_type,
        }


@dataclass(frozen=True)
class MarkdownUnit:
    id: str
    location: SourceLocation
    markdown_entry: str

    @classmethod
    def from_dict(cls, value: Any, where: str) -> MarkdownUnit:
        obj = _mapping(value, where)
        _exact_keys(obj, {"id", "location", "markdownEntry"}, where)
        return cls(
            id=_identifier(obj["id"], f"{where}.id"),
            location=SourceLocation.from_dict(obj["location"], f"{where}.location"),
            markdown_entry=validate_bundle_path(
                _string(obj["markdownEntry"], f"{where}.markdownEntry"),
                f"{where}.markdownEntry",
            ),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "location": self.location.to_dict(),
            "markdownEntry": self.markdown_entry,
        }


@dataclass(frozen=True)
class ManifestAsset:
    local_id: str
    entry: str
    kind: str
    source_kind: str
    width: int
    height: int

    @classmethod
    def from_dict(cls, value: Any, where: str) -> ManifestAsset:
        obj = _mapping(value, where)
        _exact_keys(obj, {"localId", "entry", "kind", "sourceKind", "width", "height"}, where)
        kind = _string(obj["kind"], f"{where}.kind")
        if kind != "image":
            _fail("invalid_asset", f"{where}.kind must be image")
        return cls(
            local_id=_identifier(obj["localId"], f"{where}.localId"),
            entry=validate_bundle_path(_string(obj["entry"], f"{where}.entry"), f"{where}.entry"),
            kind=kind,
            source_kind=_string(obj["sourceKind"], f"{where}.sourceKind"),
            width=_integer(obj["width"], f"{where}.width", minimum=1),
            height=_integer(obj["height"], f"{where}.height", minimum=1),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "localId": self.local_id,
            "entry": self.entry,
            "kind": self.kind,
            "sourceKind": self.source_kind,
            "width": self.width,
            "height": self.height,
        }


def _bbox(value: Any, where: str) -> tuple[int, int, int, int] | None:
    if value is None:
        return None
    parts = _list(value, where)
    if len(parts) != 4:
        _fail("invalid_bbox", f"{where} must contain four coordinates")
    coords = tuple(_integer(part, f"{where}[{index}]") for index, part in enumerate(parts))
    x0, y0, x1, y1 = coords
    if max(coords) > 1000 or x0 >= x1 or y0 >= y1:
        _fail("invalid_bbox", f"{where} must be a non-empty 0..1000 rectangle")
    return coords


@dataclass(frozen=True)
class ManifestOccurrence:
    id: str
    asset_local_id: str
    unit_id: str
    order: int
    location: SourceLocation
    bbox: tuple[int, int, int, int] | None
    alt_text: str
    caption: str
    ocr_text: str
    decorative: bool
    confidence: float

    @classmethod
    def from_dict(cls, value: Any, where: str) -> ManifestOccurrence:
        obj = _mapping(value, where)
        _exact_keys(
            obj,
            {
                "id",
                "assetLocalId",
                "unitId",
                "order",
                "location",
                "bbox",
                "altText",
                "caption",
                "ocrText",
                "decorative",
                "confidence",
            },
            where,
        )
        return cls(
            id=_identifier(obj["id"], f"{where}.id"),
            asset_local_id=_identifier(obj["assetLocalId"], f"{where}.assetLocalId"),
            unit_id=_identifier(obj["unitId"], f"{where}.unitId"),
            order=_integer(obj["order"], f"{where}.order"),
            location=SourceLocation.from_dict(obj["location"], f"{where}.location"),
            bbox=_bbox(obj["bbox"], f"{where}.bbox"),
            alt_text=_string(obj["altText"], f"{where}.altText", allow_empty=True),
            caption=_string(obj["caption"], f"{where}.caption", allow_empty=True),
            ocr_text=_string(obj["ocrText"], f"{where}.ocrText", allow_empty=True),
            decorative=_boolean(obj["decorative"], f"{where}.decorative"),
            confidence=_number(obj["confidence"], f"{where}.confidence", minimum=0, maximum=1),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "id": self.id,
            "assetLocalId": self.asset_local_id,
            "unitId": self.unit_id,
            "order": self.order,
            "location": self.location.to_dict(),
            "bbox": list(self.bbox) if self.bbox is not None else None,
            "altText": self.alt_text,
            "caption": self.caption,
            "ocrText": self.ocr_text,
            "decorative": self.decorative,
            "confidence": self.confidence,
        }


@dataclass(frozen=True)
class PDFPage:
    page: int
    status: str
    error_code: str
    unit_id: str
    native_markdown_entry: str
    render_entry: str
    primitive_entry: str

    @classmethod
    def from_dict(cls, value: Any, where: str) -> PDFPage:
        obj = _mapping(value, where)
        _exact_keys(
            obj,
            {
                "page",
                "status",
                "errorCode",
                "unitId",
                "nativeMarkdownEntry",
                "renderEntry",
                "primitiveEntry",
            },
            where,
        )
        status = _string(obj["status"], f"{where}.status")
        if status not in {"ok", "failed"}:
            _fail("invalid_page_status", f"{where}.status is unsupported")

        def entry(name: str) -> str:
            raw = _string(obj[name], f"{where}.{name}", allow_empty=True)
            return validate_bundle_path(raw, f"{where}.{name}") if raw else ""

        return cls(
            page=_integer(obj["page"], f"{where}.page", minimum=1),
            status=status,
            error_code=_string(obj["errorCode"], f"{where}.errorCode", allow_empty=True),
            unit_id=_identifier(obj["unitId"], f"{where}.unitId"),
            native_markdown_entry=entry("nativeMarkdownEntry"),
            render_entry=entry("renderEntry"),
            primitive_entry=entry("primitiveEntry"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "page": self.page,
            "status": self.status,
            "errorCode": self.error_code,
            "unitId": self.unit_id,
            "nativeMarkdownEntry": self.native_markdown_entry,
            "renderEntry": self.render_entry,
            "primitiveEntry": self.primitive_entry,
        }


@dataclass(frozen=True)
class ManifestWarning:
    code: str
    message: str
    location: SourceLocation | None
    degraded: bool

    @classmethod
    def from_dict(cls, value: Any, where: str) -> ManifestWarning:
        obj = _mapping(value, where)
        _exact_keys(obj, {"code", "message", "location", "degraded"}, where)
        location = (
            None
            if obj["location"] is None
            else SourceLocation.from_dict(obj["location"], f"{where}.location")
        )
        return cls(
            code=_identifier(obj["code"], f"{where}.code"),
            message=_string(obj["message"], f"{where}.message"),
            location=location,
            degraded=_boolean(obj["degraded"], f"{where}.degraded"),
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            "location": self.location.to_dict() if self.location else None,
            "degraded": self.degraded,
        }


@dataclass(frozen=True)
class Manifest:
    protocol_version: str
    bundle_kind: str
    source: SourceDescriptor
    parser: ParserDescriptor
    entries: tuple[EntryDescriptor, ...]
    units: tuple[MarkdownUnit, ...]
    assets: tuple[ManifestAsset, ...]
    occurrences: tuple[ManifestOccurrence, ...]
    pages: tuple[PDFPage, ...]
    warnings: tuple[ManifestWarning, ...]

    @classmethod
    def from_dict(cls, value: Any) -> Manifest:
        obj = _mapping(value, "manifest")
        _exact_keys(
            obj,
            {
                "protocolVersion",
                "bundleKind",
                "source",
                "parser",
                "entries",
                "units",
                "assets",
                "occurrences",
                "pages",
                "warnings",
            },
            "manifest",
        )
        manifest = cls(
            protocol_version=_string(obj["protocolVersion"], "protocolVersion"),
            bundle_kind=_string(obj["bundleKind"], "bundleKind"),
            source=SourceDescriptor.from_dict(obj["source"]),
            parser=ParserDescriptor.from_dict(obj["parser"]),
            entries=tuple(
                EntryDescriptor.from_dict(item, f"entries[{index}]")
                for index, item in enumerate(_list(obj["entries"], "entries"))
            ),
            units=tuple(
                MarkdownUnit.from_dict(item, f"units[{index}]")
                for index, item in enumerate(_list(obj["units"], "units"))
            ),
            assets=tuple(
                ManifestAsset.from_dict(item, f"assets[{index}]")
                for index, item in enumerate(_list(obj["assets"], "assets"))
            ),
            occurrences=tuple(
                ManifestOccurrence.from_dict(item, f"occurrences[{index}]")
                for index, item in enumerate(_list(obj["occurrences"], "occurrences"))
            ),
            pages=tuple(
                PDFPage.from_dict(item, f"pages[{index}]")
                for index, item in enumerate(_list(obj["pages"], "pages"))
            ),
            warnings=tuple(
                ManifestWarning.from_dict(item, f"warnings[{index}]")
                for index, item in enumerate(_list(obj["warnings"], "warnings"))
            ),
        )
        manifest.validate()
        return manifest

    def to_dict(self) -> dict[str, Any]:
        return {
            "protocolVersion": self.protocol_version,
            "bundleKind": self.bundle_kind,
            "source": self.source.to_dict(),
            "parser": self.parser.to_dict(),
            "entries": [entry.to_dict() for entry in self.entries],
            "units": [unit.to_dict() for unit in self.units],
            "assets": [asset.to_dict() for asset in self.assets],
            "occurrences": [occurrence.to_dict() for occurrence in self.occurrences],
            "pages": [page.to_dict() for page in self.pages],
            "warnings": [warning.to_dict() for warning in self.warnings],
        }

    def validate(self) -> None:
        if self.protocol_version != PROTOCOL_VERSION:
            _fail("protocol_version_mismatch", "unsupported protocolVersion")
        if self.bundle_kind not in {
            OFFICE_BUNDLE_KIND,
            PDF_ANALYZE_BUNDLE_KIND,
            PDF_RENDER_BUNDLE_KIND,
        }:
            _fail("invalid_bundle_kind", "unsupported bundleKind")

        _string(self.source.format, "source.format")
        _integer(self.source.byte_size, "source.byteSize", minimum=1)
        _sha256(self.source.sha256, "source.sha256")
        _non_blank_string(self.parser.name, "parser.name")
        _non_blank_string(self.parser.version, "parser.version")
        _non_blank_string(self.parser.wrapper_version, "parser.wrapperVersion")

        entry_paths = [entry.path for entry in self.entries]
        for index, entry in enumerate(self.entries):
            where = f"entries[{index}]"
            validate_bundle_path(entry.path, f"{where}.path")
            _sha256(entry.sha256, f"{where}.sha256")
            _integer(entry.byte_size, f"{where}.byteSize")
            expected_mime = _ENTRY_MIME_BY_EXTENSION.get(PurePosixPath(entry.path).suffix.lower())
            if expected_mime is None or entry.mime_type != expected_mime:
                _fail(
                    "invalid_entry_mime",
                    f"{where}.mimeType does not match its allowlisted path extension",
                )
        if entry_paths != sorted(entry_paths) or len(entry_paths) != len(set(entry_paths)):
            _fail("invalid_entry_directory", "entries must be unique and sorted by path")
        _unique((unit.id for unit in self.units), "unit id")
        _unique((asset.local_id for asset in self.assets), "asset localId")
        _unique((occurrence.id for occurrence in self.occurrences), "occurrence id")

        entry_set = set(entry_paths)
        entry_by_path = {entry.path: entry for entry in self.entries}
        refs: list[str] = [unit.markdown_entry for unit in self.units]
        refs.extend(asset.entry for asset in self.assets)
        refs.extend(
            entry
            for page in self.pages
            for entry in (
                page.native_markdown_entry,
                page.render_entry,
                page.primitive_entry,
            )
            if entry
        )
        if set(refs) != entry_set or len(refs) != len(set(refs)):
            _fail(
                "invalid_entry_references",
                "every declared payload entry must be referenced exactly once",
            )

        for unit in self.units:
            if entry_by_path[unit.markdown_entry].mime_type != "text/markdown; charset=utf-8":
                _fail("invalid_entry_references", "unit markdownEntry must reference Markdown")
        for asset in self.assets:
            if not entry_by_path[asset.entry].mime_type.startswith("image/"):
                _fail("invalid_entry_references", "asset entry must reference an image")
        for page in self.pages:
            if (
                page.native_markdown_entry
                and entry_by_path[page.native_markdown_entry].mime_type
                != "text/markdown; charset=utf-8"
            ):
                _fail("invalid_entry_references", "nativeMarkdownEntry must reference Markdown")
            if (
                page.primitive_entry
                and entry_by_path[page.primitive_entry].mime_type != "application/json"
            ):
                _fail("invalid_entry_references", "primitiveEntry must reference JSON")
            if (
                page.render_entry
                and not entry_by_path[page.render_entry].mime_type.startswith("image/")
            ):
                _fail("invalid_entry_references", "renderEntry must reference an image")

        for index, asset in enumerate(self.assets):
            if asset.source_kind not in _ASSET_SOURCE_KINDS:
                _fail("invalid_asset", f"assets[{index}].sourceKind is unsupported")

        asset_ids = {asset.local_id for asset in self.assets}
        occurrence_asset_ids = {occurrence.asset_local_id for occurrence in self.occurrences}
        if occurrence_asset_ids - asset_ids or asset_ids - occurrence_asset_ids:
            _fail("invalid_occurrence", "every asset must have an occurrence and no occurrence may dangle")

        occurrence_orders: set[tuple[str, int]] = set()
        for index, occurrence in enumerate(self.occurrences):
            _integer(occurrence.order, f"occurrences[{index}].order")
            order_key = (occurrence.unit_id, occurrence.order)
            if order_key in occurrence_orders:
                _fail("invalid_occurrence", "occurrence order must be unique within its unit")
            occurrence_orders.add(order_key)

        if self.bundle_kind == OFFICE_BUNDLE_KIND:
            self._validate_office()
        else:
            self._validate_pdf()

    def _validate_office(self) -> None:
        if self.source.format not in {"docx", "pptx", "xlsx"}:
            _fail("invalid_office_manifest", "office-convert source format is unsupported")
        if self.pages:
            _fail("invalid_office_manifest", "office-convert pages must be empty")
        if not self.units:
            _fail("invalid_office_manifest", "office-convert requires at least one unit")
        unit_by_id = {unit.id: unit for unit in self.units}
        for unit in self.units:
            location = unit.location
            if location.kind == "document" and location.index == 0:
                expected_id = "unit_document_0000"
            elif location.kind in {"slide", "sheet"} and location.index > 0:
                expected_id = f"unit_{location.kind}_{location.index:04d}"
            else:
                _fail(
                    "invalid_office_manifest",
                    "Office unit location must be document, slide, or sheet",
                )
            if unit.id != expected_id:
                _fail(
                    "invalid_office_manifest",
                    "Office unit id must be deterministic for its location",
                )
        for occurrence in self.occurrences:
            unit = unit_by_id.get(occurrence.unit_id)
            if unit is None or unit.location != occurrence.location:
                _fail("invalid_occurrence", "Office occurrence must reference a matching unit")

    def _validate_pdf(self) -> None:
        if self.source.format != "pdf":
            _fail("invalid_pdf_manifest", "PDF source format must be pdf")
        if self.units:
            _fail("invalid_pdf_manifest", "PDF top-level units must be empty")
        page_numbers = [page.page for page in self.pages]
        unit_ids = [page.unit_id for page in self.pages]
        if len(page_numbers) != len(set(page_numbers)) or len(unit_ids) != len(set(unit_ids)):
            _fail("invalid_pdf_manifest", "PDF page numbers and unitId values must be unique")
        if page_numbers != sorted(page_numbers):
            _fail("invalid_pdf_manifest", "PDF page records must be sorted")
        if self.bundle_kind == PDF_ANALYZE_BUNDLE_KIND:
            if page_numbers != list(range(1, len(page_numbers) + 1)):
                _fail("invalid_pdf_manifest", "pdf-analyze pages must be ordered, complete, and gap-free")
            if self.assets or self.occurrences:
                _fail("invalid_pdf_manifest", "pdf-analyze cannot contain assets or occurrences")

        page_by_unit = {page.unit_id: page for page in self.pages}
        for page in self.pages:
            expected_unit_id = f"unit_page_{self.source.sha256[:12]}_{page.page:04d}"
            if page.unit_id != expected_unit_id:
                _fail(
                    "invalid_pdf_manifest",
                    "PDF unitId must derive from source SHA-256 and the 1-based page number",
                )
            if page.status == "failed":
                if not page.error_code or page.error_code not in PDF_PAGE_ERROR_CODES:
                    _fail("invalid_pdf_page", "failed PDF page requires an allowlisted errorCode")
                if page.native_markdown_entry or page.render_entry or page.primitive_entry:
                    _fail("invalid_pdf_page", "failed PDF page cannot reference payload entries")
                continue
            if page.error_code:
                _fail("invalid_pdf_page", "ok PDF page errorCode must be empty")
            if self.bundle_kind == PDF_ANALYZE_BUNDLE_KIND:
                if not page.native_markdown_entry or not page.primitive_entry or page.render_entry:
                    _fail("invalid_pdf_page", "pdf-analyze ok page requires native+primitive only")
            elif not page.render_entry or page.native_markdown_entry or page.primitive_entry:
                _fail("invalid_pdf_page", "pdf-render ok page requires render only")

        for occurrence in self.occurrences:
            page = page_by_unit.get(occurrence.unit_id)
            if page is None or page.status != "ok":
                _fail("invalid_occurrence", "PDF occurrence must reference an ok page")
            if occurrence.location.kind != "page" or occurrence.location.index != page.page:
                _fail("invalid_occurrence", "PDF occurrence location must match its page")


def _unique(values: Iterable[str], name: str) -> None:
    items = list(values)
    if len(items) != len(set(items)):
        _fail("duplicate_identifier", f"duplicate {name}")


def canonical_json_bytes(value: Mapping[str, Any]) -> bytes:
    return json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def parse_manifest_bytes(value: bytes) -> Manifest:
    if len(value) > MANIFEST_MAX_BYTES:
        _fail("manifest_too_large", "manifest.json exceeds 1 MiB")
    try:
        decoded = json.loads(value)
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ProtocolError("invalid_manifest_json", "manifest.json is not valid UTF-8 JSON") from exc
    return Manifest.from_dict(decoded)


def validate_health_document(value: Any) -> dict[str, Any]:
    obj = _mapping(value, "health")
    _exact_keys(obj, {"protocolVersion", "serviceVersion", "limits", "capabilities"}, "health")
    if _string(obj["protocolVersion"], "health.protocolVersion") != PROTOCOL_VERSION:
        _fail("protocol_version_mismatch", "unsupported health protocolVersion")
    _non_blank_string(obj["serviceVersion"], "health.serviceVersion")
    limits = _mapping(obj["limits"], "health.limits")
    _exact_keys(limits, {"maxInputBytes", "maxOutputBytes"}, "health.limits")
    _integer(limits["maxInputBytes"], "health.limits.maxInputBytes", minimum=1)
    _integer(limits["maxOutputBytes"], "health.limits.maxOutputBytes", minimum=1)
    capabilities = _mapping(obj["capabilities"], "health.capabilities")
    _exact_keys(capabilities, {"office", "pdf"}, "health.capabilities")
    office = _mapping(capabilities["office"], "health.capabilities.office")
    _exact_keys(
        office,
        {"enabled", "formats", "markitdownVersion", "wrapperVersion"},
        "health.capabilities.office",
    )
    _boolean(office["enabled"], "health.capabilities.office.enabled")
    formats = [_string(item, "health.capabilities.office.formats[]") for item in _list(office["formats"], "health.capabilities.office.formats")]
    if formats != sorted(set(formats)) or any(item not in {"docx", "pptx", "xlsx"} for item in formats):
        _fail("invalid_health", "Office formats must be unique, sorted, and allowlisted")
    _string(office["markitdownVersion"], "health.capabilities.office.markitdownVersion")
    _string(office["wrapperVersion"], "health.capabilities.office.wrapperVersion")
    pdf = _mapping(capabilities["pdf"], "health.capabilities.pdf")
    _exact_keys(pdf, {"enabled", "engine", "engineVersion"}, "health.capabilities.pdf")
    pdf_enabled = _boolean(pdf["enabled"], "health.capabilities.pdf.enabled")
    engine = _string(pdf["engine"], "health.capabilities.pdf.engine", allow_empty=True)
    engine_version = _string(
        pdf["engineVersion"], "health.capabilities.pdf.engineVersion", allow_empty=True
    )
    if pdf_enabled:
        if not engine or not engine_version:
            _fail("invalid_health", "PDF engine metadata must be present exactly when PDF is enabled")
    elif engine or engine_version:
        _fail("invalid_health", "PDF engine metadata must be present exactly when PDF is enabled")
    return dict(obj)


def validate_page_primitive_document(value: Any) -> dict[str, Any]:
    """Validate the exact v1 PDF page-analysis primitive payload schema."""

    obj = _mapping(value, "pagePrimitive")
    _exact_keys(
        obj,
        {
            "page",
            "width",
            "height",
            "textChars",
            "blockCount",
            "textCoverage",
            "textBlocks",
            "embeddedImages",
            "signals",
        },
        "pagePrimitive",
    )
    _integer(obj["page"], "pagePrimitive.page", minimum=1)
    width = _number(obj["width"], "pagePrimitive.width", minimum=0)
    height = _number(obj["height"], "pagePrimitive.height", minimum=0)
    if width <= 0 or height <= 0:
        _fail("invalid_page_primitive", "page dimensions must be finite and greater than zero")
    _integer(obj["textChars"], "pagePrimitive.textChars")
    block_count = _integer(obj["blockCount"], "pagePrimitive.blockCount")
    _number(obj["textCoverage"], "pagePrimitive.textCoverage", minimum=0, maximum=1)
    blocks = _list(obj["textBlocks"], "pagePrimitive.textBlocks")
    if block_count != len(blocks):
        _fail("invalid_page_primitive", "blockCount must equal len(textBlocks)")
    for index, block in enumerate(blocks):
        where = f"pagePrimitive.textBlocks[{index}]"
        block_obj = _mapping(block, where)
        _exact_keys(block_obj, {"text", "bbox"}, where)
        _string(block_obj["text"], f"{where}.text")
        if _bbox(block_obj["bbox"], f"{where}.bbox") is None:
            _fail("invalid_page_primitive", f"{where}.bbox cannot be null")
    for index, image in enumerate(
        _list(obj["embeddedImages"], "pagePrimitive.embeddedImages")
    ):
        where = f"pagePrimitive.embeddedImages[{index}]"
        image_obj = _mapping(image, where)
        _exact_keys(image_obj, {"bbox"}, where)
        if _bbox(image_obj["bbox"], f"{where}.bbox") is None:
            _fail("invalid_page_primitive", f"{where}.bbox cannot be null")
    signals = _mapping(obj["signals"], "pagePrimitive.signals")
    _exact_keys(
        signals,
        {"table", "code", "scanned", "multicolumn", "readingOrderUncertain"},
        "pagePrimitive.signals",
    )
    for name in ("table", "code", "scanned", "multicolumn", "readingOrderUncertain"):
        _boolean(signals[name], f"pagePrimitive.signals.{name}")
    return dict(obj)


@dataclass(frozen=True)
class PayloadEntry:
    path: str
    mime_type: str
    byte_size: int
    sha256: str
    opener: Callable[[], BinaryIO]

    @classmethod
    def from_bytes(cls, path: str, mime_type: str, value: bytes) -> PayloadEntry:
        canonical = validate_bundle_path(path)
        digest = hashlib.sha256(value).hexdigest()
        return cls(canonical, mime_type, len(value), digest, lambda: io.BytesIO(value))

    @classmethod
    def from_file(cls, path: str, mime_type: str, source: Path) -> PayloadEntry:
        canonical = validate_bundle_path(path)
        digest = hashlib.sha256()
        size = 0
        with source.open("rb") as handle:
            while chunk := handle.read(64 * 1024):
                size += len(chunk)
                digest.update(chunk)
        return cls(canonical, mime_type, size, digest.hexdigest(), lambda: source.open("rb"))

    def descriptor(self) -> EntryDescriptor:
        return EntryDescriptor(self.path, self.sha256, self.byte_size, self.mime_type)


@dataclass
class Bundle:
    manifest: Manifest
    payloads: tuple[PayloadEntry, ...]
    cleanup: Callable[[], None] = field(default=lambda: None, repr=False)
    _closed: bool = field(default=False, init=False, repr=False)

    def close(self) -> None:
        if not self._closed:
            self._closed = True
            self.cleanup()


@dataclass(frozen=True)
class BundleLimits:
    max_output_bytes: int
    max_entry_bytes: int
    max_entries: int


def _tar_header(name: str, size: int) -> bytes:
    info = tarfile.TarInfo(name=name)
    info.size = size
    info.mode = 0o644
    info.uid = 0
    info.gid = 0
    info.uname = ""
    info.gname = ""
    info.mtime = 0
    return info.tobuf(format=tarfile.USTAR_FORMAT, encoding="utf-8", errors="strict")


def _padded_size(size: int) -> int:
    return size + (-size % TAR_BLOCK_SIZE)


def _validate_bundle_for_stream(bundle: Bundle, limits: BundleLimits) -> bytes:
    bundle.manifest.validate()
    payloads = tuple(sorted(bundle.payloads, key=lambda item: item.path))
    if payloads != bundle.payloads:
        _fail("invalid_entry_directory", "payloads must already be sorted by path")
    if len(payloads) > limits.max_entries:
        _fail("too_many_entries", "bundle payload entry limit exceeded")
    descriptors = tuple(payload.descriptor() for payload in payloads)
    if descriptors != bundle.manifest.entries:
        _fail("entry_mismatch", "payload metadata does not match manifest entries")
    for payload in payloads:
        if payload.byte_size > limits.max_entry_bytes:
            _fail("entry_too_large", f"{payload.path} exceeds the single-entry limit")
    manifest_bytes = canonical_json_bytes(bundle.manifest.to_dict())
    if len(manifest_bytes) > MANIFEST_MAX_BYTES:
        _fail("manifest_too_large", "manifest.json exceeds 1 MiB")
    tar_size = TAR_BLOCK_SIZE + _padded_size(len(manifest_bytes)) + 2 * TAR_BLOCK_SIZE
    tar_size += sum(TAR_BLOCK_SIZE + _padded_size(payload.byte_size) for payload in payloads)
    if tar_size > limits.max_output_bytes:
        _fail("bundle_too_large", "tar response exceeds maxOutputBytes")
    return manifest_bytes


def validate_bundle_for_stream(bundle: Bundle, limits: BundleLimits) -> None:
    """Eagerly validate a bundle before HTTP response headers are committed."""

    _validate_bundle_for_stream(bundle, limits)


def stream_tar(bundle: Bundle, limits: BundleLimits, chunk_size: int = 64 * 1024) -> Iterator[bytes]:
    """Yield a deterministic USTAR stream without materializing the bundle."""

    try:
        manifest_bytes = _validate_bundle_for_stream(bundle, limits)
        yield _tar_header(MANIFEST_NAME, len(manifest_bytes))
        yield manifest_bytes
        if padding := -len(manifest_bytes) % TAR_BLOCK_SIZE:
            yield b"\0" * padding
        for payload in bundle.payloads:
            yield _tar_header(payload.path, payload.byte_size)
            consumed = 0
            digest = hashlib.sha256()
            with payload.opener() as handle:
                while chunk := handle.read(chunk_size):
                    consumed += len(chunk)
                    if consumed > payload.byte_size:
                        _fail("entry_changed", f"{payload.path} grew while streaming")
                    digest.update(chunk)
                    yield chunk
            if consumed != payload.byte_size or digest.hexdigest() != payload.sha256:
                _fail("entry_changed", f"{payload.path} changed while streaming")
            if padding := -consumed % TAR_BLOCK_SIZE:
                yield b"\0" * padding
        yield b"\0" * (2 * TAR_BLOCK_SIZE)
    finally:
        bundle.close()


def sha256_file(path: Path) -> tuple[str, int]:
    digest = hashlib.sha256()
    size = 0
    with path.open("rb") as handle:
        while chunk := handle.read(64 * 1024):
            size += len(chunk)
            digest.update(chunk)
    return digest.hexdigest(), size


def make_health_document(
    *,
    service_version: str,
    max_input_bytes: int,
    max_output_bytes: int,
    pdf_engine: str = "",
    pdf_engine_version: str = "",
) -> dict[str, Any]:
    if bool(pdf_engine) != bool(pdf_engine_version):
        raise RuntimeError("PDF engine name and version must be configured together")
    value = {
        "protocolVersion": PROTOCOL_VERSION,
        "serviceVersion": service_version,
        "limits": {
            "maxInputBytes": max_input_bytes,
            "maxOutputBytes": max_output_bytes,
        },
        "capabilities": {
            "office": {
                "enabled": True,
                "formats": ["docx", "pptx", "xlsx"],
                "markitdownVersion": "0.1.6",
                "wrapperVersion": "office-wrapper-v1",
            },
            "pdf": {
                "enabled": bool(pdf_engine),
                "engine": pdf_engine,
                "engineVersion": pdf_engine_version,
            },
        },
    }
    validate_health_document(value)
    return value


def env_positive_int(name: str, default: int) -> int:
    raw = os.getenv(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise RuntimeError(f"{name} must be a positive integer") from exc
    if value <= 0:
        raise RuntimeError(f"{name} must be a positive integer")
    return value
