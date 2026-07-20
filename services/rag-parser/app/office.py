from __future__ import annotations

import hashlib
import logging
import mimetypes
import posixpath
import re
import secrets
import shutil
import stat
import warnings as pywarnings
import zipfile
from collections import defaultdict
from collections.abc import Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path, PurePosixPath, PureWindowsPath
from typing import Any, Protocol
from urllib.parse import urlsplit
from xml.etree import ElementTree as StdET

from defusedxml import ElementTree as SafeET
from PIL import Image

from .protocol import (
    OFFICE_BUNDLE_KIND,
    PROTOCOL_VERSION,
    Bundle,
    Manifest,
    ManifestAsset,
    ManifestOccurrence,
    ManifestWarning,
    MarkdownUnit,
    ParserDescriptor,
    PayloadEntry,
    SourceDescriptor,
    SourceLocation,
)

LOGGER = logging.getLogger("rag-parser.office")

WRAPPER_VERSION = "office-wrapper-v1"
MARKITDOWN_VERSION = "0.1.6"
OFFICE_FORMATS = ("docx", "pptx", "xlsx")
_PACKAGE_ABSOLUTE_ROOTS = {"_rels", "customXml", "docProps", "ppt", "word", "xl"}

REL_NS = "http://schemas.openxmlformats.org/package/2006/relationships"
DOC_REL_NS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

_XML_DECLARATION_ATTACK = re.compile(br"<!\s*(?:DOCTYPE|ENTITY)\b", re.IGNORECASE)
_PPTX_SLIDE_MARKER = re.compile(
    r"<!--\s*Slide\s+number\s*:\s*(\d+)\s*-->", re.IGNORECASE
)
_MARKDOWN_IMAGE = re.compile(r"!\[([^\]\n]{0,1024})\]\([^\n)]*\)")
_HTML_RESOURCE_TAG = re.compile(
    r"<\s*/?\s*(?:img|picture|source|svg|image)\b[^>]*>", re.IGNORECASE
)
_DANGEROUS_RELATIONSHIP_TYPES = (
    "afchunk",
    "attachedtemplate",
    "altchunk",
    "externallink",
    "oleobject",
    "package",
)
_SAFE_IMAGE_FORMATS = {
    "JPEG": ("jpg", "image/jpeg"),
    "PNG": ("png", "image/png"),
    "WEBP": ("webp", "image/webp"),
}

_XLSX_MAX_ROWS = 1_048_576
_XLSX_MAX_COLUMNS = 16_384


class OfficeError(ValueError):
    def __init__(self, code: str, message: str, status_code: int = 422):
        super().__init__(message)
        self.code = code
        self.status_code = status_code


class Converter(Protocol):
    def convert(self, source: Path, source_format: str) -> str: ...


@dataclass(frozen=True)
class OfficeLimits:
    max_archive_entries: int
    max_zip_entry_bytes: int
    max_extracted_bytes: int
    max_compression_ratio: int
    max_asset_bytes: int
    max_assets: int
    max_image_pixels: int


@dataclass(frozen=True)
class PreflightResult:
    sanitized_path: Path
    warnings: tuple[ManifestWarning, ...]


@dataclass(frozen=True)
class Relationship:
    rel_type: str
    target: str


@dataclass
class ExtractedImage:
    local_id: str
    path: Path
    extension: str
    mime_type: str
    width: int
    height: int
    sha256: str


@dataclass(frozen=True)
class PendingOccurrence:
    unit_id: str
    location: SourceLocation
    relationship: Relationship
    order: int
    alt_text: str
    part: str
    blip_index: int
    sentinel: str
    anchor_label: str = ""
    bbox: tuple[int, int, int, int] | None = None


@dataclass(frozen=True)
class CodeSentinel:
    unit_id: str
    start: str
    end: str
    text: str


@dataclass(frozen=True)
class InstrumentedSource:
    path: Path
    code_sentinels: tuple[CodeSentinel, ...]
    sheet_tokens: Mapping[str, str]


class MarkItDownConverter:
    """Narrow wrapper which intentionally exposes only convert_stream()."""

    def __init__(self, engine: Any | None = None):
        if engine is None:
            from markitdown import MarkItDown

            engine = MarkItDown(enable_plugins=False)
        self._engine = engine

    def convert(self, source: Path, source_format: str) -> str:
        if source_format not in OFFICE_FORMATS:
            raise OfficeError("unsupported_format", f"unsupported Office format: {source_format}")
        with source.open("rb") as stream:
            result = self._engine.convert_stream(stream, file_extension=f".{source_format}")
        text = getattr(result, "text_content", None)
        if not isinstance(text, str):
            raise OfficeError("markitdown_invalid_result", "MarkItDown returned no text_content")
        return _normalize_markdown(text)


def preflight_ooxml(
    source: Path,
    source_format: str,
    request_dir: Path,
    limits: OfficeLimits,
) -> PreflightResult:
    if source_format not in OFFICE_FORMATS:
        raise OfficeError("unsupported_format", f"unsupported Office format: {source_format}")
    sanitized = request_dir / f"sanitized.{source_format}"
    manifest_part = {
        "docx": "word/document.xml",
        "pptx": "ppt/presentation.xml",
        "xlsx": "xl/workbook.xml",
    }[source_format]
    warning_values: list[ManifestWarning] = []

    try:
        archive = zipfile.ZipFile(source)
    except (OSError, zipfile.BadZipFile) as exc:
        raise OfficeError("invalid_ooxml", "input is not a valid OOXML ZIP container") from exc

    with archive:
        infos = archive.infolist()
        _validate_zip_directory(infos, limits)
        names = {info.filename for info in infos if not info.is_dir()}
        required = {"[Content_Types].xml", "_rels/.rels", manifest_part}
        if not required.issubset(names):
            raise OfficeError(
                "invalid_ooxml",
                f"{source_format} container is missing required OOXML parts",
            )

        with zipfile.ZipFile(
            sanitized, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6
        ) as output:
            for info in infos:
                if info.is_dir():
                    continue
                raw = archive.read(info)
                if _is_xml_part(info.filename):
                    _reject_xml_declarations(raw, info.filename)
                    if info.filename.endswith(".rels"):
                        raw, relationship_warnings = _sanitize_relationships(
                            raw, info.filename, names
                        )
                        warning_values.extend(relationship_warnings)
                    else:
                        _parse_xml(raw, info.filename)
                output.writestr(_sanitized_zip_info(info.filename), raw)

    return PreflightResult(sanitized_path=sanitized, warnings=tuple(warning_values))


def _validate_zip_directory(infos: list[zipfile.ZipInfo], limits: OfficeLimits) -> None:
    files = [info for info in infos if not info.is_dir()]
    if not files or len(files) > limits.max_archive_entries:
        raise OfficeError("ooxml_entry_limit", "OOXML entry count is outside the allowed range")
    names: set[str] = set()
    folded_names: set[str] = set()
    extracted = 0
    for info in files:
        name = _safe_zip_name(info.filename)
        folded = name.casefold()
        if name in names or folded in folded_names:
            raise OfficeError("duplicate_ooxml_entry", "OOXML contains duplicate entry names")
        names.add(name)
        folded_names.add(folded)
        if info.flag_bits & 0x1:
            raise OfficeError("encrypted_ooxml_entry", "encrypted OOXML entries are unsupported")
        unix_mode = (info.external_attr >> 16) & 0xFFFF
        if unix_mode and stat.S_ISLNK(unix_mode):
            raise OfficeError("ooxml_symlink", "OOXML symlink entries are forbidden")
        if info.file_size > limits.max_zip_entry_bytes:
            raise OfficeError("ooxml_entry_too_large", "an OOXML entry exceeds its byte limit")
        extracted += info.file_size
        if extracted > limits.max_extracted_bytes:
            raise OfficeError("ooxml_output_limit", "OOXML expanded bytes exceed the limit")
        if info.file_size:
            if info.compress_size <= 0:
                raise OfficeError("ooxml_zip_bomb", "OOXML has an invalid compression ratio")
            if info.file_size / info.compress_size > limits.max_compression_ratio:
                raise OfficeError("ooxml_zip_bomb", "OOXML compression ratio exceeds the limit")


def _safe_zip_name(name: str) -> str:
    if not name or "\x00" in name or "\\" in name:
        raise OfficeError("unsafe_ooxml_path", "OOXML entry path is unsafe")
    if name.startswith(("/", "//")) or PureWindowsPath(name).drive:
        raise OfficeError("unsafe_ooxml_path", "OOXML entry path must be relative")
    path = PurePosixPath(name)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        raise OfficeError("unsafe_ooxml_path", "OOXML entry path contains traversal")
    if path.as_posix() != name:
        raise OfficeError("unsafe_ooxml_path", "OOXML entry path is not canonical")
    return name


def _sanitized_zip_info(name: str) -> zipfile.ZipInfo:
    info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
    info.compress_type = zipfile.ZIP_DEFLATED
    info.external_attr = 0o600 << 16
    info.create_system = 3
    return info


def _is_xml_part(name: str) -> bool:
    lowered = name.lower()
    return lowered.endswith((".xml", ".rels")) or lowered == "[content_types].xml"


def _reject_xml_declarations(raw: bytes, part_name: str) -> None:
    if _XML_DECLARATION_ATTACK.search(raw):
        raise OfficeError(
            "unsafe_ooxml_xml",
            f"DTD/entity declarations are forbidden in {part_name}",
        )


def _parse_xml(raw: bytes, part_name: str) -> Any:
    try:
        return SafeET.fromstring(raw)
    except Exception as exc:
        raise OfficeError("invalid_ooxml_xml", f"invalid XML in {part_name}") from exc


def _sanitize_relationships(
    raw: bytes, part_name: str, package_names: set[str]
) -> tuple[bytes, list[ManifestWarning]]:
    root = _parse_xml(raw, part_name)
    warnings: list[ManifestWarning] = []
    for node in list(root):
        if _local_name(node.tag) != "Relationship":
            raise OfficeError("invalid_ooxml_relationship", "unexpected relationship XML node")
        rel_id = str(node.attrib.get("Id", ""))
        if not rel_id:
            raise OfficeError("invalid_ooxml_relationship", "relationship Id is required")
        rel_type = str(node.attrib.get("Type", ""))
        target = str(node.attrib.get("Target", ""))
        target_mode = str(node.attrib.get("TargetMode", ""))
        lowered_type = rel_type.lower()
        relationship_name = lowered_type.rsplit("/", 1)[-1]
        if relationship_name in _DANGEROUS_RELATIONSHIP_TYPES:
            raise OfficeError(
                "unsafe_ooxml_relationship",
                "attached templates, OLE, altChunk, packages, and external links are forbidden",
            )
        if target_mode.lower() == "external":
            if not lowered_type.endswith("/hyperlink"):
                raise OfficeError(
                    "unsafe_ooxml_relationship",
                    "external OOXML relationships other than hyperlinks are forbidden",
                )
            root.remove(node)
            warnings.append(
                ManifestWarning(
                    code="office_external_hyperlink_removed",
                    message="外部超链接仅保留可见文本，目标不会被访问",
                    location=None,
                    degraded=False,
                )
            )
            continue
        if target.startswith("/"):
            target = _canonical_package_absolute_target(
                part_name, target, package_names
            )
            node.set("Target", target)
        _validate_internal_relationship_target(part_name, target)
    return StdET.tostring(root, encoding="utf-8", xml_declaration=True), warnings


def _validate_internal_relationship_target(rel_part: str, target: str) -> None:
    if not target or "\x00" in target or "\\" in target or PureWindowsPath(target).drive:
        raise OfficeError("unsafe_ooxml_target", "OOXML relationship target is unsafe")
    parsed = urlsplit(target)
    if parsed.scheme or parsed.netloc or target.startswith("/"):
        raise OfficeError("unsafe_ooxml_target", "OOXML local/absolute URI target is forbidden")
    if target.startswith("#"):
        return
    source_part = _source_part_for_relationship(rel_part)
    resolved = posixpath.normpath(posixpath.join(posixpath.dirname(source_part), target))
    if resolved == ".." or resolved.startswith("../"):
        raise OfficeError("unsafe_ooxml_target", "OOXML relationship target escapes the package")


def _canonical_package_absolute_target(
    rel_part: str, target: str, package_names: set[str]
) -> str:
    if target.startswith("//"):
        raise OfficeError("unsafe_ooxml_target", "OOXML local/absolute URI target is forbidden")
    resolved = _safe_zip_name(target.lstrip("/"))
    root = resolved.split("/", 1)[0]
    if root not in _PACKAGE_ABSOLUTE_ROOTS or resolved not in package_names:
        raise OfficeError("unsafe_ooxml_target", "OOXML local absolute target is forbidden")
    source_part = _source_part_for_relationship(rel_part)
    return posixpath.relpath(resolved, posixpath.dirname(source_part) or ".")


def _source_part_for_relationship(rel_part: str) -> str:
    path = PurePosixPath(rel_part)
    if path.as_posix() == "_rels/.rels":
        return ""
    if path.parent.name != "_rels" or not path.name.endswith(".rels"):
        raise OfficeError("invalid_ooxml_relationship", "relationship part path is invalid")
    return (path.parent.parent / path.name[: -len(".rels")]).as_posix()


def _relationship_target(source_part: str, target: str) -> str:
    resolved = posixpath.normpath(posixpath.join(posixpath.dirname(source_part), target))
    return _safe_zip_name(resolved)


def _local_name(tag: str) -> str:
    return tag.rsplit("}", 1)[-1]


def _load_relationships(archive: zipfile.ZipFile) -> dict[str, dict[str, Relationship]]:
    result: dict[str, dict[str, Relationship]] = {}
    for name in archive.namelist():
        if not name.endswith(".rels"):
            continue
        source_part = _source_part_for_relationship(name)
        root = _parse_xml(archive.read(name), name)
        relationships: dict[str, Relationship] = {}
        for node in root:
            if _local_name(node.tag) != "Relationship":
                continue
            rel_id = str(node.attrib.get("Id", ""))
            rel_type = str(node.attrib.get("Type", ""))
            target = str(node.attrib.get("Target", ""))
            if not rel_id or str(node.attrib.get("TargetMode", "")).lower() == "external":
                continue
            if rel_id in relationships:
                raise OfficeError("invalid_ooxml_relationship", "duplicate relationship Id")
            relationships[rel_id] = Relationship(
                rel_type=rel_type,
                target=_relationship_target(source_part, target),
            )
        result[source_part] = relationships
    return result


def _numeric_suffix(name: str) -> tuple[int, str]:
    match = re.search(r"(\d+)(?=\.xml$)", name)
    return (int(match.group(1)) if match else 2**31 - 1, name)


def _parent_map(root: Any) -> dict[Any, Any]:
    return {child: parent for parent in root.iter() for child in parent}


def _ancestor_with_local_name(node: Any, parents: Mapping[Any, Any], names: set[str]) -> Any | None:
    current = node
    while current in parents:
        current = parents[current]
        if _local_name(current.tag) in names:
            return current
    return None


def _alt_text(container: Any | None) -> str:
    if container is None:
        return ""
    for node in container.iter():
        if _local_name(node.tag) in {"docPr", "cNvPr"}:
            # OOXML shape/object names (often file names such as image1.png)
            # are identifiers, not user-authored alternative text.
            for key in ("descr", "title"):
                value = str(node.attrib.get(key, "")).strip()
                if value:
                    return value[:1024]
    return ""


def _blip_relationship_id(node: Any) -> str:
    return str(node.attrib.get(f"{{{DOC_REL_NS}}}embed", ""))


def _document_unit() -> tuple[str, SourceLocation]:
    return "unit_document_0000", SourceLocation("document", 0, "文档")


def _pptx_units(
    archive: zipfile.ZipFile,
    relationships: Mapping[str, Mapping[str, Relationship]],
) -> list[tuple[str, str, SourceLocation]]:
    root = _parse_xml(archive.read("ppt/presentation.xml"), "ppt/presentation.xml")
    presentation_rels = relationships.get("ppt/presentation.xml", {})
    slides: list[str] = []
    for node in root.iter():
        if _local_name(node.tag) != "sldId":
            continue
        rel_id = str(node.attrib.get(f"{{{DOC_REL_NS}}}id", ""))
        relationship = presentation_rels.get(rel_id)
        if relationship is None or not relationship.rel_type.lower().endswith("/slide"):
            raise OfficeError(
                "invalid_ooxml_relationship", "presentation slide relationship is missing"
            )
        slides.append(relationship.target)
    if not slides or len(slides) != len(set(slides)):
        raise OfficeError("invalid_ooxml", "presentation slide order is missing or duplicated")
    return [
        (
            part,
            f"unit_slide_{index:04d}",
            SourceLocation("slide", index, f"幻灯片 {index}"),
        )
        for index, part in enumerate(slides, 1)
    ]


def _xlsx_units(
    archive: zipfile.ZipFile,
    relationships: Mapping[str, Mapping[str, Relationship]],
) -> list[tuple[str, str, SourceLocation]]:
    root = _parse_xml(archive.read("xl/workbook.xml"), "xl/workbook.xml")
    workbook_rels = relationships.get("xl/workbook.xml", {})
    values: list[tuple[str, str, SourceLocation]] = []
    for index, sheet in enumerate((node for node in root.iter() if _local_name(node.tag) == "sheet"), 1):
        rel_id = str(sheet.attrib.get(f"{{{DOC_REL_NS}}}id", ""))
        relationship = workbook_rels.get(rel_id)
        if relationship is None:
            raise OfficeError("invalid_ooxml_relationship", "worksheet relationship is missing")
        label = str(sheet.attrib.get("name", f"Sheet{index}"))[:256]
        values.append(
            (
                relationship.target,
                f"unit_sheet_{index:04d}",
                SourceLocation("sheet", index, label),
            )
        )
    if not values:
        raise OfficeError("invalid_ooxml", "workbook has no sheets")
    return values


def _find_occurrences(
    archive: zipfile.ZipFile,
    source_format: str,
    relationships: Mapping[str, Mapping[str, Relationship]],
    sentinel_nonce: str,
) -> tuple[list[tuple[str, str, SourceLocation]], list[PendingOccurrence]]:
    pending: list[PendingOccurrence] = []
    if source_format == "docx":
        unit_id, location = _document_unit()
        part = "word/document.xml"
        _collect_part_occurrences(
            archive,
            part,
            unit_id,
            location,
            relationships,
            pending,
            {"inline", "anchor", "drawing", "pict"},
            sentinel_nonce,
        )
        return [(part, unit_id, location)], pending
    if source_format == "pptx":
        units = _pptx_units(archive, relationships)
        for part, unit_id, location in units:
            _collect_pptx_occurrences(
                archive,
                part,
                unit_id,
                location,
                relationships,
                pending,
                sentinel_nonce,
            )
        return units, pending

    units = _xlsx_units(archive, relationships)
    for sheet_part, unit_id, location in units:
        sheet_root = _parse_xml(archive.read(sheet_part), sheet_part)
        sheet_rels = relationships.get(sheet_part, {})
        drawing_rel_ids = [
            str(node.attrib.get(f"{{{DOC_REL_NS}}}id", ""))
            for node in sheet_root.iter()
            if _local_name(node.tag) == "drawing"
        ]
        for drawing_rel_id in drawing_rel_ids:
            drawing_relationship = sheet_rels.get(drawing_rel_id)
            if drawing_relationship is None:
                raise OfficeError("invalid_ooxml_relationship", "worksheet drawing is missing")
            _collect_xlsx_occurrences(
                archive,
                drawing_relationship.target,
                unit_id,
                location,
                relationships,
                pending,
                sentinel_nonce,
            )
    return units, pending


def _image_sentinel(nonce: str, index: int) -> str:
    return f"BKCRABIMAGE{nonce}{index:08d}TOKEN"


def _blip_nodes(root: Any) -> list[Any]:
    return [node for node in root.iter() if _local_name(node.tag) == "blip"]


def _collect_part_occurrences(
    archive: zipfile.ZipFile,
    part: str,
    unit_id: str,
    location: SourceLocation,
    relationships: Mapping[str, Mapping[str, Relationship]],
    output: list[PendingOccurrence],
    alt_ancestors: set[str],
    sentinel_nonce: str,
) -> None:
    if part not in archive.namelist():
        raise OfficeError("invalid_ooxml_relationship", f"referenced OOXML part is missing: {part}")
    root = _parse_xml(archive.read(part), part)
    parents = _parent_map(root)
    part_relationships = relationships.get(part, {})
    for blip_index, blip in enumerate(_blip_nodes(root)):
        rel_id = _blip_relationship_id(blip)
        relationship = part_relationships.get(rel_id)
        if relationship is None or not relationship.rel_type.lower().endswith("/image"):
            continue
        ancestor = _ancestor_with_local_name(blip, parents, alt_ancestors)
        output.append(
            PendingOccurrence(
                unit_id=unit_id,
                location=location,
                relationship=relationship,
                order=len(output),
                alt_text=_alt_text(ancestor),
                part=part,
                blip_index=blip_index,
                sentinel=_image_sentinel(sentinel_nonce, len(output) + 1),
            )
        )


def _shape_coordinates(shape: Any) -> tuple[int, int]:
    for node in shape.iter():
        if _local_name(node.tag) != "xfrm":
            continue
        for child in node:
            if _local_name(child.tag) == "off":
                try:
                    return int(child.attrib.get("y", "0")), int(
                        child.attrib.get("x", "0")
                    )
                except ValueError:
                    return 0, 0
        break
    return 0, 0


def _pptx_shape_nodes(container: Any) -> Iterable[Any]:
    supported = {"sp", "pic", "graphicFrame", "grpSp", "cxnSp"}
    children = [child for child in container if _local_name(child.tag) in supported]
    ordered = sorted(
        enumerate(children),
        key=lambda item: (*_shape_coordinates(item[1]), item[0]),
    )
    for _, shape in ordered:
        if _local_name(shape.tag) == "grpSp":
            yield from _pptx_shape_nodes(shape)
        else:
            yield shape


def _collect_pptx_occurrences(
    archive: zipfile.ZipFile,
    part: str,
    unit_id: str,
    location: SourceLocation,
    relationships: Mapping[str, Mapping[str, Relationship]],
    output: list[PendingOccurrence],
    sentinel_nonce: str,
) -> None:
    if part not in archive.namelist():
        raise OfficeError("invalid_ooxml_relationship", "referenced slide part is missing")
    root = _parse_xml(archive.read(part), part)
    shape_tree = next(
        (node for node in root.iter() if _local_name(node.tag) == "spTree"), None
    )
    if shape_tree is None:
        raise OfficeError("invalid_ooxml", "slide shape tree is missing")
    all_blips = _blip_nodes(root)
    blip_indexes = {id(node): index for index, node in enumerate(all_blips)}
    part_relationships = relationships.get(part, {})
    for shape in _pptx_shape_nodes(shape_tree):
        if _local_name(shape.tag) != "pic":
            continue
        for blip in _blip_nodes(shape):
            rel_id = _blip_relationship_id(blip)
            relationship = part_relationships.get(rel_id)
            if relationship is None or not relationship.rel_type.lower().endswith("/image"):
                continue
            output.append(
                PendingOccurrence(
                    unit_id=unit_id,
                    location=location,
                    relationship=relationship,
                    order=len(output),
                    alt_text=_alt_text(shape),
                    part=part,
                    blip_index=blip_indexes[id(blip)],
                    sentinel=_image_sentinel(sentinel_nonce, len(output) + 1),
                )
            )


def _spreadsheet_column(index: int) -> str:
    value = index + 1
    letters = ""
    while value:
        value, remainder = divmod(value - 1, 26)
        letters = chr(ord("A") + remainder) + letters
    return letters


def _xlsx_anchor(anchor: Any) -> tuple[tuple[int, int], str]:
    marker = next(
        (child for child in anchor if _local_name(child.tag) == "from"), None
    )
    if marker is None:
        return (2**31 - 1, 2**31 - 1), ""
    row = next(
        (child.text for child in marker if _local_name(child.tag) == "row"), None
    )
    column = next(
        (child.text for child in marker if _local_name(child.tag) == "col"), None
    )
    try:
        row_index = int(row or "0")
        column_index = int(column or "0")
    except ValueError:
        return (2**31 - 1, 2**31 - 1), ""
    if not (0 <= row_index < _XLSX_MAX_ROWS) or not (
        0 <= column_index < _XLSX_MAX_COLUMNS
    ):
        raise OfficeError(
            "invalid_ooxml_anchor", "worksheet image anchor is outside Excel bounds"
        )
    return (row_index, column_index), f"{_spreadsheet_column(column_index)}{row_index + 1}"


def _collect_xlsx_occurrences(
    archive: zipfile.ZipFile,
    part: str,
    unit_id: str,
    location: SourceLocation,
    relationships: Mapping[str, Mapping[str, Relationship]],
    output: list[PendingOccurrence],
    sentinel_nonce: str,
) -> None:
    if part not in archive.namelist():
        raise OfficeError("invalid_ooxml_relationship", "worksheet drawing is missing")
    root = _parse_xml(archive.read(part), part)
    all_blips = _blip_nodes(root)
    blip_indexes = {id(node): index for index, node in enumerate(all_blips)}
    anchors = [
        node
        for node in root
        if _local_name(node.tag) in {"oneCellAnchor", "twoCellAnchor", "absoluteAnchor"}
    ]
    ordered_anchors = sorted(
        enumerate(anchors), key=lambda item: (*_xlsx_anchor(item[1])[0], item[0])
    )
    part_relationships = relationships.get(part, {})
    for _, anchor in ordered_anchors:
        _, anchor_label = _xlsx_anchor(anchor)
        for blip in _blip_nodes(anchor):
            rel_id = _blip_relationship_id(blip)
            relationship = part_relationships.get(rel_id)
            if relationship is None or not relationship.rel_type.lower().endswith("/image"):
                continue
            output.append(
                PendingOccurrence(
                    unit_id=unit_id,
                    location=location,
                    relationship=relationship,
                    order=len(output),
                    alt_text=_alt_text(anchor),
                    part=part,
                    blip_index=blip_indexes[id(blip)],
                    sentinel=_image_sentinel(sentinel_nonce, len(output) + 1),
                    anchor_label=anchor_label,
                )
            )


def _attribute_by_local_name(node: Any, name: str) -> str:
    for key, value in node.attrib.items():
        if _local_name(key) == name:
            return str(value)
    return ""


def _style_key(value: str) -> str:
    return re.sub(r"[\s_-]+", "", value).casefold()


def _docx_code_style_ids(archive: zipfile.ZipFile) -> set[str]:
    values = {"code", "preformatted"}
    if "word/styles.xml" not in archive.namelist():
        return values
    root = _parse_xml(archive.read("word/styles.xml"), "word/styles.xml")
    result = set(values)
    for style in root:
        if _local_name(style.tag) != "style":
            continue
        style_id = _attribute_by_local_name(style, "styleId")
        style_name = next(
            (
                _attribute_by_local_name(child, "val")
                for child in style
                if _local_name(child.tag) == "name"
            ),
            "",
        )
        if _style_key(style_id) in values or _style_key(style_name) in values:
            result.add(_style_key(style_id))
    return result


def _code_tokens(nonce: str, index: int) -> tuple[str, str]:
    return (
        f"BKCRABCODESTART{nonce}{index:08d}TOKEN",
        f"BKCRABCODEEND{nonce}{index:08d}TOKEN",
    )


def _instrument_text_nodes(
    text_nodes: list[Any],
    unit_id: str,
    nonce: str,
    index: int,
    text: str | None = None,
) -> CodeSentinel | None:
    nonempty = [node for node in text_nodes if node.text]
    if not nonempty:
        return None
    source_text = text if text is not None else "".join(
        str(node.text or "") for node in text_nodes
    )
    if not source_text.strip():
        return None
    start, end = _code_tokens(nonce, index)
    nonempty[0].text = start + str(nonempty[0].text)
    nonempty[-1].text = str(nonempty[-1].text) + end
    return CodeSentinel(unit_id=unit_id, start=start, end=end, text=source_text)


def _instrument_docx_code(
    root: Any,
    archive: zipfile.ZipFile,
    unit_id: str,
    sentinel_nonce: str,
    start_index: int,
) -> list[CodeSentinel]:
    code_styles = _docx_code_style_ids(archive)
    sentinels: list[CodeSentinel] = []
    for paragraph in (node for node in root.iter() if _local_name(node.tag) == "p"):
        properties = next(
            (child for child in paragraph if _local_name(child.tag) == "pPr"), None
        )
        if properties is None:
            continue
        style = next(
            (
                child
                for child in properties.iter()
                if _local_name(child.tag) == "pStyle"
            ),
            None,
        )
        if style is None or _style_key(_attribute_by_local_name(style, "val")) not in code_styles:
            continue
        text_nodes = [
            node for node in paragraph.iter() if _local_name(node.tag) == "t"
        ]
        sentinel = _instrument_text_nodes(
            text_nodes, unit_id, sentinel_nonce, start_index + len(sentinels)
        )
        if sentinel is not None:
            sentinels.append(sentinel)
    return sentinels


def _pptx_shape_is_code(shape: Any) -> bool:
    marker = next(
        (node for node in shape if _local_name(node.tag) == "nvSpPr"), None
    )
    if marker is None:
        return False
    properties = next(
        (node for node in marker.iter() if _local_name(node.tag) == "cNvPr"), None
    )
    if properties is None:
        return False
    allowed = {"code", "preformatted", "sourcecode"}
    return any(
        _style_key(str(properties.attrib.get(key, ""))) in allowed
        for key in ("name", "descr", "title")
    )


def _instrument_pptx_code(
    root: Any, unit_id: str, sentinel_nonce: str, start_index: int
) -> list[CodeSentinel]:
    sentinels: list[CodeSentinel] = []
    for shape in (node for node in root.iter() if _local_name(node.tag) == "sp"):
        if not _pptx_shape_is_code(shape):
            continue
        text_nodes = [node for node in shape.iter() if _local_name(node.tag) == "t"]
        paragraphs = []
        for paragraph in (
            node for node in shape.iter() if _local_name(node.tag) == "p"
        ):
            paragraph_text = "".join(
                str(node.text or "")
                for node in paragraph.iter()
                if _local_name(node.tag) == "t"
            )
            if paragraph_text:
                paragraphs.append(paragraph_text)
        sentinel = _instrument_text_nodes(
            text_nodes,
            unit_id,
            sentinel_nonce,
            start_index + len(sentinels),
            "\n".join(paragraphs),
        )
        if sentinel is not None:
            sentinels.append(sentinel)
    return sentinels


def _instrument_office_source(
    source: Path,
    source_format: str,
    units: list[tuple[str, str, SourceLocation]],
    pending: list[PendingOccurrence],
    request_dir: Path,
    sentinel_nonce: str,
) -> InstrumentedSource:
    destination = request_dir / f"converter-input.{source_format}"
    with zipfile.ZipFile(source) as archive:
        roots: dict[str, Any] = {}

        def load_root(part: str) -> Any:
            if part not in roots:
                roots[part] = _parse_xml(archive.read(part), part)
            return roots[part]

        if source_format in {"docx", "pptx"}:
            ancestor_names = (
                {"inline", "anchor", "drawing", "pict"}
                if source_format == "docx"
                else {"pic"}
            )
            metadata_name = "docPr" if source_format == "docx" else "cNvPr"
            for item in pending:
                root = load_root(item.part)
                blips = _blip_nodes(root)
                if item.blip_index >= len(blips):
                    raise OfficeError(
                        "office_image_hook_failed", "image sentinel target is missing"
                    )
                parents = _parent_map(root)
                container = _ancestor_with_local_name(
                    blips[item.blip_index], parents, ancestor_names
                )
                metadata = next(
                    (
                        node
                        for node in container.iter()
                        if _local_name(node.tag) == metadata_name
                    ),
                    None,
                ) if container is not None else None
                if metadata is None:
                    raise OfficeError(
                        "office_image_hook_failed",
                        "image sentinel metadata hook is missing",
                    )
                metadata.set("descr", item.sentinel)

        code_sentinels: list[CodeSentinel] = []
        if source_format == "docx":
            _, unit_id, _ = units[0]
            code_sentinels.extend(
                _instrument_docx_code(
                    load_root("word/document.xml"), archive, unit_id, sentinel_nonce, 1
                )
            )
        elif source_format == "pptx":
            for part, unit_id, _ in units:
                code_sentinels.extend(
                    _instrument_pptx_code(
                        load_root(part), unit_id, sentinel_nonce, len(code_sentinels) + 1
                    )
                )

        sheet_tokens: dict[str, str] = {}
        if source_format == "xlsx":
            workbook = load_root("xl/workbook.xml")
            sheet_nodes = [
                node for node in workbook.iter() if _local_name(node.tag) == "sheet"
            ]
            if len(sheet_nodes) != len(units):
                raise OfficeError("invalid_ooxml", "workbook sheet declaration mismatch")
            for index, ((_, unit_id, _), sheet) in enumerate(
                zip(units, sheet_nodes, strict=True), 1
            ):
                token = f"BKCRABSHEET{sentinel_nonce}{index:04d}TOKEN"
                sheet.set("name", token)
                sheet_tokens[unit_id] = token

        with zipfile.ZipFile(
            destination, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=6
        ) as output:
            for info in archive.infolist():
                if info.is_dir():
                    continue
                if info.filename in roots:
                    raw = StdET.tostring(
                        roots[info.filename], encoding="utf-8", xml_declaration=True
                    )
                    output.writestr(_sanitized_zip_info(info.filename), raw)
                    continue
                with archive.open(info) as source_handle, output.open(
                    _sanitized_zip_info(info.filename), "w"
                ) as destination_handle:
                    shutil.copyfileobj(source_handle, destination_handle, 64 * 1024)

    return InstrumentedSource(
        path=destination,
        code_sentinels=tuple(code_sentinels),
        sheet_tokens=sheet_tokens,
    )


def _materialize_images(
    archive: zipfile.ZipFile,
    pending: Iterable[PendingOccurrence],
    request_dir: Path,
    limits: OfficeLimits,
) -> tuple[list[ExtractedImage], dict[str, ExtractedImage]]:
    assets_dir = request_dir / "bundle-assets"
    assets_dir.mkdir(mode=0o700)
    by_target: dict[str, ExtractedImage] = {}
    by_sha: dict[str, ExtractedImage] = {}
    ordered: list[ExtractedImage] = []
    for occurrence in pending:
        target = occurrence.relationship.target
        if target in by_target:
            continue
        if target not in archive.namelist():
            raise OfficeError("invalid_ooxml_relationship", "image relationship target is missing")
        temporary = assets_dir / f"candidate-{len(by_target) + 1:04d}.bin"
        digest = hashlib.sha256()
        size = 0
        with archive.open(target) as source, temporary.open("wb") as destination:
            while chunk := source.read(64 * 1024):
                size += len(chunk)
                if size > limits.max_asset_bytes:
                    raise OfficeError("office_asset_too_large", "an Office image exceeds the limit")
                digest.update(chunk)
                destination.write(chunk)
        sha256 = digest.hexdigest()
        extension, mime_type, width, height = _inspect_image(temporary, limits)
        existing = by_sha.get(sha256)
        if existing is not None:
            temporary.unlink()
            by_target[target] = existing
            continue
        if len(ordered) >= limits.max_assets:
            temporary.unlink()
            raise OfficeError("office_asset_limit", "Office image count exceeds the limit")
        local_id = f"asset_{len(ordered) + 1:04d}"
        final_path = assets_dir / f"{local_id}.{extension}"
        temporary.replace(final_path)
        asset = ExtractedImage(
            local_id=local_id,
            path=final_path,
            extension=extension,
            mime_type=mime_type,
            width=width,
            height=height,
            sha256=sha256,
        )
        ordered.append(asset)
        by_sha[sha256] = asset
        by_target[target] = asset
    return ordered, by_target


def _inspect_image(path: Path, limits: OfficeLimits) -> tuple[str, str, int, int]:
    old_limit = Image.MAX_IMAGE_PIXELS
    Image.MAX_IMAGE_PIXELS = limits.max_image_pixels
    try:
        with pywarnings.catch_warnings():
            pywarnings.simplefilter("error", Image.DecompressionBombWarning)
            with Image.open(path) as image:
                image_format = image.format
                width, height = image.size
                image.verify()
            if width <= 0 or height <= 0 or width * height > limits.max_image_pixels:
                raise OfficeError("office_image_pixels", "Office image pixel limit exceeded")
            if image_format not in _SAFE_IMAGE_FORMATS:
                raise OfficeError(
                    "unsupported_office_image",
                    "Office image is not PNG, JPEG, or WebP",
                )
            with Image.open(path) as image:
                image.load()
        extension, mime_type = _SAFE_IMAGE_FORMATS[image_format]
        return extension, mime_type, width, height
    except OfficeError:
        raise
    except (OSError, Image.DecompressionBombError, Image.DecompressionBombWarning) as exc:
        raise OfficeError("invalid_office_image", "Office image failed safe decoding") from exc
    finally:
        Image.MAX_IMAGE_PIXELS = old_limit


def _normalize_markdown(text: str) -> str:
    value = text.replace("\x00", "").replace("\r\n", "\n").replace("\r", "\n").strip()
    return f"{value}\n" if value else ""


def _rewrite_converter_images(
    text: str, token_markers: Mapping[str, str]
) -> tuple[str, set[str]]:
    seen: set[str] = set()

    def replacement(match: re.Match[str]) -> str:
        alt = match.group(1).strip()
        if alt in token_markers:
            seen.add(alt)
            return token_markers[alt]
        return f"[图片：{alt}]" if alt else "[图片]"

    value = _HTML_RESOURCE_TAG.sub("", _MARKDOWN_IMAGE.sub(replacement, text))
    return _normalize_markdown(value), seen


def _fallback_part_text(archive: zipfile.ZipFile, part: str) -> str:
    root = _parse_xml(archive.read(part), part)
    values = [
        (node.text or "").strip()
        for node in root.iter()
        if _local_name(node.tag) in {"t", "v"} and (node.text or "").strip()
    ]
    return _normalize_markdown("\n\n".join(values))


def _partition_markdown(
    archive: zipfile.ZipFile,
    source_format: str,
    markdown: str,
    units: list[tuple[str, str, SourceLocation]],
    relationships: Mapping[str, Mapping[str, Relationship]],
    sheet_tokens: Mapping[str, str],
) -> tuple[dict[str, str], list[ManifestWarning]]:
    if source_format == "docx" and len(units) == 1:
        return {units[0][1]: markdown}, []
    if source_format == "pptx":
        matches = list(_PPTX_SLIDE_MARKER.finditer(markdown))
        slide_numbers = [int(match.group(1)) for match in matches]
        if len(matches) == len(units) and slide_numbers == list(
            range(1, len(units) + 1)
        ):
            sections = {
                slide: _normalize_markdown(
                    markdown[
                        match.end() : (
                            matches[index + 1].start()
                            if index + 1 < len(matches)
                            else len(markdown)
                        )
                    ]
                )
                for index, (slide, match) in enumerate(zip(slide_numbers, matches, strict=True))
            }
            return {
                unit_id: sections[index]
                for index, (_, unit_id, _) in enumerate(units, 1)
            }, []
        slide_parts = [part for part, _, _ in units]
        fallback = {
            unit_id: _fallback_pptx_text(
                archive, slide_parts[index - 1], relationships
            )
            for index, (_, unit_id, _) in enumerate(units, 1)
        }
    else:
        marker_matches: list[tuple[int, int, str, SourceLocation]] = []
        for _, unit_id, location in units:
            token = sheet_tokens.get(unit_id, "")
            match = re.search(rf"(?m)^##\s+{re.escape(token)}\s*$", markdown)
            if match is None:
                break
            marker_matches.append((match.start(), match.end(), unit_id, location))
        if len(marker_matches) == len(units) and marker_matches == sorted(marker_matches):
            values: dict[str, str] = {}
            for index, (_, end, unit_id, location) in enumerate(marker_matches):
                section_end = (
                    marker_matches[index + 1][0]
                    if index + 1 < len(marker_matches)
                    else len(markdown)
                )
                body = markdown[end:section_end].strip()
                values[unit_id] = _normalize_markdown(
                    f"## {location.label}\n\n{body}" if body else f"## {location.label}"
                )
            return values, []
        sheet_parts = [part for part, _, _ in units]
        fallback = {
            unit_id: _fallback_part_text(archive, sheet_parts[index - 1])
            for index, (_, unit_id, _) in enumerate(units, 1)
        }
    return fallback, [
        ManifestWarning(
            code="office_markdown_coarse_partition",
            message="MarkItDown 输出无法稳定分段，已按 slide/sheet 保守提取可见文本",
            location=None,
            degraded=True,
        )
    ]


def _code_fence(text: str) -> str:
    longest = max((len(match.group(0)) for match in re.finditer(r"`+", text)), default=0)
    fence = "`" * max(3, longest + 1)
    return f"{fence}\n{text.rstrip()}\n{fence}"


def _apply_code_sentinels(
    markdown_by_unit: dict[str, str],
    sentinels: Iterable[CodeSentinel],
) -> list[ManifestWarning]:
    warnings: list[ManifestWarning] = []
    for sentinel in sentinels:
        markdown = markdown_by_unit.get(sentinel.unit_id, "")
        pattern = re.compile(
            re.escape(sentinel.start) + r".*?" + re.escape(sentinel.end), re.DOTALL
        )
        value, count = pattern.subn(_code_fence(sentinel.text), markdown, count=1)
        if count == 1:
            markdown_by_unit[sentinel.unit_id] = _normalize_markdown(value)
            continue
        markdown_by_unit[sentinel.unit_id] = _normalize_markdown(
            markdown.replace(sentinel.start, "").replace(sentinel.end, "")
        )
        warnings.append(
            ManifestWarning(
                code="office_code_style_unresolved",
                message="明确代码样式未能通过固定 MarkItDown hook 定位，已保留普通文本",
                location=None,
                degraded=True,
            )
        )
    return warnings


def _pptx_speaker_notes_text(
    archive: zipfile.ZipFile,
    slide_part: str,
    relationships: Mapping[str, Mapping[str, Relationship]],
) -> str:
    notes_relationships = [
        relationship
        for relationship in relationships.get(slide_part, {}).values()
        if relationship.rel_type.lower().endswith("/notesslide")
    ]
    if len(notes_relationships) > 1:
        raise OfficeError("invalid_ooxml_relationship", "slide has duplicate notes relationships")
    if not notes_relationships:
        return ""
    notes_part = notes_relationships[0].target
    if notes_part not in archive.namelist():
        raise OfficeError("invalid_ooxml_relationship", "speaker notes part is missing")
    root = _parse_xml(archive.read(notes_part), notes_part)
    parents = _parent_map(root)
    paragraphs: list[str] = []
    for placeholder in (node for node in root.iter() if _local_name(node.tag) == "ph"):
        if str(placeholder.attrib.get("type", "")).casefold() != "body":
            continue
        shape = _ancestor_with_local_name(placeholder, parents, {"sp"})
        if shape is None:
            continue
        for paragraph in (node for node in shape.iter() if _local_name(node.tag) == "p"):
            value = "".join(
                str(node.text or "")
                for node in paragraph.iter()
                if _local_name(node.tag) == "t"
            ).strip()
            if value:
                paragraphs.append(value)
    return "\n".join(paragraphs)


def _pptx_has_speaker_notes(
    archive: zipfile.ZipFile,
    slide_part: str,
    relationships: Mapping[str, Mapping[str, Relationship]],
) -> bool:
    return bool(_pptx_speaker_notes_text(archive, slide_part, relationships))


def _fallback_pptx_text(
    archive: zipfile.ZipFile,
    slide_part: str,
    relationships: Mapping[str, Mapping[str, Relationship]],
) -> str:
    slide_text = _fallback_part_text(archive, slide_part).rstrip()
    notes = _pptx_speaker_notes_text(archive, slide_part, relationships)
    if not notes:
        return _normalize_markdown(slide_text)
    return _normalize_markdown(f"{slide_text}\n\n### Notes:\n\n{notes}")


def _normalize_speaker_notes(
    markdown: str, *, has_speaker_notes: bool, location: SourceLocation
) -> tuple[str, list[ManifestWarning]]:
    matches = list(re.finditer(r"(?m)^### Notes:\s*$", markdown))
    if not has_speaker_notes:
        return markdown, []
    if len(matches) != 1:
        return markdown, [
            ManifestWarning(
                code="office_notes_ambiguous",
                message="演讲者备注边界不唯一，已保留 MarkItDown 原文",
                location=location,
                degraded=True,
            )
        ]
    match = matches[0]
    before = markdown[: match.start()].rstrip()
    notes = markdown[match.end() :].strip()
    if not notes:
        return _normalize_markdown(before), []
    quoted = "\n".join(f"> {line}" if line else ">" for line in notes.splitlines())
    return _normalize_markdown(f"{before}\n\n> 演讲者备注\n>\n{quoted}"), []


def _table_cells(line: str) -> list[str]:
    value = line.strip()
    if value.startswith("|"):
        value = value[1:]
    if value.endswith("|"):
        value = value[:-1]
    return [cell.strip() for cell in value.split("|")]


def _validate_gfm_tables(
    markdown: str, location: SourceLocation
) -> tuple[str, list[ManifestWarning]]:
    lines = markdown.splitlines()
    warnings: list[ManifestWarning] = []
    output: list[str] = []
    index = 0
    in_fence = False
    while index < len(lines):
        line = lines[index]
        if re.match(r"^\s*(`{3,}|~{3,})", line):
            in_fence = not in_fence
            output.append(line)
            index += 1
            continue
        if (
            not in_fence
            and "|" in line
            and index + 1 < len(lines)
            and "---" in lines[index + 1]
        ):
            separator = lines[index + 1]
            header_cells = _table_cells(line)
            separator_cells = _table_cells(separator)
            end = index + 2
            while end < len(lines) and "|" in lines[end] and lines[end].strip():
                end += 1
            row_cells = [_table_cells(value) for value in lines[index + 2 : end]]
            valid = (
                len(header_cells) > 1
                and len(separator_cells) == len(header_cells)
                and all(
                    re.fullmatch(r":?-{3,}:?", cell) is not None
                    for cell in separator_cells
                )
                and all(len(cells) == len(header_cells) for cells in row_cells)
            )
            if valid:
                output.extend(lines[index:end])
            else:
                output.append(" | ".join(header_cells))
                output.extend(" | ".join(cells) for cells in row_cells)
                warnings.append(
                    ManifestWarning(
                        code="office_table_invalid",
                        message="MarkItDown 表格未通过保守 GFM 校验，已保留单元格文字",
                        location=location,
                        degraded=True,
                    )
                )
            index = end
            continue
        output.append(line)
        index += 1
    return _normalize_markdown("\n".join(output)), warnings


def _anchored_alt_text(alt_text: str, anchor_label: str) -> str:
    visible = alt_text.strip() or "图片（未进行视觉识别）"
    if anchor_label:
        return f"单元格 {anchor_label}：{visible}"
    return visible


def _marker(occurrence_id: str, alt_text: str) -> str:
    visible = alt_text.strip() or "图片（未进行视觉识别）"
    visible = visible.replace("[", "\\[").replace("]", "\\]")
    return f"![{visible}](rag-asset://{occurrence_id})"


def build_office_bundle(
    *,
    original_source: Path,
    sanitized_source: Path,
    source_format: str,
    source_sha256: str,
    source_size: int,
    request_dir: Path,
    converter: Converter,
    limits: OfficeLimits,
    preflight_warnings: Iterable[ManifestWarning] = (),
) -> Bundle:
    # The nonce prevents user-authored Office text/metadata from forging an
    # internal converter hook. It is removed before the deterministic bundle
    # contract is emitted.
    sentinel_nonce = secrets.token_hex(16).upper()
    with zipfile.ZipFile(sanitized_source) as archive:
        relationships = _load_relationships(archive)
        unit_sources, pending = _find_occurrences(
            archive, source_format, relationships, sentinel_nonce
        )
        assets, assets_by_target = _materialize_images(
            archive, pending, request_dir, limits
        )

    occurrences: list[ManifestOccurrence] = []
    pending_occurrences: list[tuple[PendingOccurrence, ManifestOccurrence]] = []
    occurrence_counts: defaultdict[str, int] = defaultdict(int)
    for item in pending:
        asset = assets_by_target[item.relationship.target]
        local_order = occurrence_counts[item.unit_id]
        occurrence_counts[item.unit_id] += 1
        location_fragment = (
            f"{item.location.kind}_{item.location.index:04d}"
            if item.location.index
            else "document_0000"
        )
        occurrence = ManifestOccurrence(
            id=f"occ_{location_fragment}_{local_order + 1:04d}",
            asset_local_id=asset.local_id,
            unit_id=item.unit_id,
            order=local_order,
            location=item.location,
            bbox=item.bbox,
            alt_text=_anchored_alt_text(
                item.alt_text, item.anchor_label if source_format == "xlsx" else ""
            ),
            caption="",
            ocr_text="",
            decorative=False,
            confidence=1.0,
        )
        occurrences.append(occurrence)
        pending_occurrences.append((item, occurrence))

    instrumented = _instrument_office_source(
        sanitized_source,
        source_format,
        unit_sources,
        pending,
        request_dir,
        sentinel_nonce,
    )
    markdown = _normalize_markdown(converter.convert(instrumented.path, source_format))
    notes_by_unit: dict[str, bool] = {}
    with zipfile.ZipFile(sanitized_source) as archive:
        unit_markdown, partition_warnings = _partition_markdown(
            archive,
            source_format,
            markdown,
            unit_sources,
            relationships,
            instrumented.sheet_tokens,
        )
        if source_format == "pptx":
            notes_by_unit = {
                unit_id: _pptx_has_speaker_notes(archive, part, relationships)
                for part, unit_id, _ in unit_sources
            }
    code_warnings = _apply_code_sentinels(
        unit_markdown, instrumented.code_sentinels
    )
    notes_warnings: list[ManifestWarning] = []
    if source_format == "pptx":
        for _, unit_id, location in unit_sources:
            value, value_warnings = _normalize_speaker_notes(
                unit_markdown.get(unit_id, ""),
                has_speaker_notes=notes_by_unit.get(unit_id, False),
                location=location,
            )
            unit_markdown[unit_id] = value
            notes_warnings.extend(value_warnings)
    table_warnings: list[ManifestWarning] = []
    for _, unit_id, location in unit_sources:
        value, value_warnings = _validate_gfm_tables(
            unit_markdown.get(unit_id, ""), location
        )
        unit_markdown[unit_id] = value
        table_warnings.extend(value_warnings)

    payloads: list[PayloadEntry] = []
    units: list[MarkdownUnit] = []
    manifest_warnings = (
        list(preflight_warnings)
        + partition_warnings
        + code_warnings
        + notes_warnings
        + table_warnings
    )
    for index, (_, unit_id, location) in enumerate(unit_sources, 1):
        value = unit_markdown.get(unit_id, "")
        local_pairs = [
            pair for pair in pending_occurrences if pair[0].unit_id == unit_id
        ]
        token_markers = {
            item.sentinel: _marker(occurrence.id, occurrence.alt_text)
            for item, occurrence in local_pairs
        }
        value, seen = _rewrite_converter_images(value, token_markers)
        missing = [pair for pair in local_pairs if pair[0].sentinel not in seen]
        if missing:
            markers = "\n".join(
                _marker(occurrence.id, occurrence.alt_text)
                for _item, occurrence in missing
            )
            value = f"{value.rstrip()}\n\n### 相关图片\n\n{markers}\n"
            if source_format == "xlsx":
                for item, _ in missing:
                    anchor = f"单元格 {item.anchor_label}" if item.anchor_label else "未知锚点"
                    manifest_warnings.append(
                        ManifestWarning(
                            code="office_image_coarse_location",
                            message=(
                                f"图片锚定于{anchor}；MarkItDown 0.1.6 无稳定行级 hook，"
                                "已放入该工作表的相关图片小节"
                            ),
                            location=location,
                            degraded=True,
                        )
                    )
            else:
                manifest_warnings.append(
                    ManifestWarning(
                        code="office_image_coarse_location",
                        message=f"图片按 {location.kind} 粒度关联，未声明精确 Markdown 字符位置",
                        location=location,
                        degraded=True,
                    )
                )
        entry_path = f"units/{index:04d}.md"
        payloads.append(
            PayloadEntry.from_bytes(
                entry_path,
                "text/markdown; charset=utf-8",
                value.encode("utf-8"),
            )
        )
        units.append(MarkdownUnit(unit_id, location, entry_path))

    manifest_assets: list[ManifestAsset] = []
    for asset in assets:
        entry_path = f"assets/{asset.local_id}.{asset.extension}"
        payloads.append(PayloadEntry.from_file(entry_path, asset.mime_type, asset.path))
        manifest_assets.append(
            ManifestAsset(
                local_id=asset.local_id,
                entry=entry_path,
                kind="image",
                source_kind="embedded_original",
                width=asset.width,
                height=asset.height,
            )
        )

    payloads.sort(key=lambda item: item.path)
    manifest = Manifest(
        protocol_version=PROTOCOL_VERSION,
        bundle_kind=OFFICE_BUNDLE_KIND,
        source=SourceDescriptor(source_format, source_size, source_sha256),
        parser=ParserDescriptor("markitdown", MARKITDOWN_VERSION, WRAPPER_VERSION),
        entries=tuple(payload.descriptor() for payload in payloads),
        units=tuple(units),
        assets=tuple(manifest_assets),
        occurrences=tuple(occurrences),
        pages=(),
        warnings=tuple(manifest_warnings),
    )
    manifest.validate()
    return Bundle(
        manifest=manifest,
        payloads=tuple(payloads),
        cleanup=lambda: shutil.rmtree(request_dir, ignore_errors=True),
    )


def detect_file_mime(filename: str) -> str:
    guessed, _ = mimetypes.guess_type(filename)
    return guessed or "application/octet-stream"
