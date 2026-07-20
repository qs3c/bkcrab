from __future__ import annotations

import hashlib
import logging
import mimetypes
import posixpath
import re
import shutil
import stat
import warnings as pywarnings
import zipfile
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
    bbox: tuple[int, int, int, int] | None = None


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
                            raw, info.filename
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
    raw: bytes, part_name: str
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
        _validate_internal_relationship_target(part_name, target)
    return StdET.tostring(root, encoding="utf-8", xml_declaration=True), warnings


def _validate_internal_relationship_target(rel_part: str, target: str) -> None:
    if not target or "\x00" in target or "\\" in target or PureWindowsPath(target).drive:
        raise OfficeError("unsafe_ooxml_target", "OOXML relationship target is unsafe")
    parsed = urlsplit(target)
    if parsed.scheme or parsed.netloc or target.startswith("//"):
        raise OfficeError("unsafe_ooxml_target", "OOXML local/absolute URI target is forbidden")
    if target.startswith("#"):
        return
    source_part = _source_part_for_relationship(rel_part)
    if target.startswith("/"):
        resolved = posixpath.normpath(target.lstrip("/"))
    else:
        resolved = posixpath.normpath(posixpath.join(posixpath.dirname(source_part), target))
    if resolved == ".." or resolved.startswith("../"):
        raise OfficeError("unsafe_ooxml_target", "OOXML relationship target escapes the package")


def _source_part_for_relationship(rel_part: str) -> str:
    path = PurePosixPath(rel_part)
    if path.as_posix() == "_rels/.rels":
        return ""
    if path.parent.name != "_rels" or not path.name.endswith(".rels"):
        raise OfficeError("invalid_ooxml_relationship", "relationship part path is invalid")
    return (path.parent.parent / path.name[: -len(".rels")]).as_posix()


def _relationship_target(source_part: str, target: str) -> str:
    if target.startswith("/"):
        resolved = posixpath.normpath(target.lstrip("/"))
    else:
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
            for key in ("descr", "title", "name"):
                value = str(node.attrib.get(key, "")).strip()
                if value and not value.lower().startswith(("picture ", "image ")):
                    return value[:1024]
    return ""


def _blip_relationship_id(node: Any) -> str:
    return str(node.attrib.get(f"{{{DOC_REL_NS}}}embed", ""))


def _document_unit() -> tuple[str, SourceLocation]:
    return "unit_document_0000", SourceLocation("document", 0, "文档")


def _pptx_units(archive: zipfile.ZipFile) -> list[tuple[str, str, SourceLocation]]:
    slides = sorted(
        (
            name
            for name in archive.namelist()
            if re.fullmatch(r"ppt/slides/slide\d+\.xml", name)
        ),
        key=_numeric_suffix,
    )
    return [
        (
            name,
            f"unit_slide_{index:04d}",
            SourceLocation("slide", index, f"幻灯片 {index}"),
        )
        for index, name in enumerate(slides, 1)
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
) -> tuple[list[tuple[str, SourceLocation]], list[PendingOccurrence]]:
    pending: list[PendingOccurrence] = []
    if source_format == "docx":
        unit_id, location = _document_unit()
        _collect_part_occurrences(
            archive,
            "word/document.xml",
            unit_id,
            location,
            relationships,
            pending,
            {"inline", "anchor", "drawing", "pict"},
        )
        return [(unit_id, location)], pending
    if source_format == "pptx":
        units = _pptx_units(archive)
        for part, unit_id, location in units:
            _collect_part_occurrences(
                archive,
                part,
                unit_id,
                location,
                relationships,
                pending,
                {"pic", "sp", "graphicFrame"},
            )
        return [(unit_id, location) for _, unit_id, location in units], pending

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
            _collect_part_occurrences(
                archive,
                drawing_relationship.target,
                unit_id,
                location,
                relationships,
                pending,
                {"oneCellAnchor", "twoCellAnchor", "absoluteAnchor", "pic"},
            )
    return [(unit_id, location) for _, unit_id, location in units], pending


def _collect_part_occurrences(
    archive: zipfile.ZipFile,
    part: str,
    unit_id: str,
    location: SourceLocation,
    relationships: Mapping[str, Mapping[str, Relationship]],
    output: list[PendingOccurrence],
    alt_ancestors: set[str],
) -> None:
    if part not in archive.namelist():
        raise OfficeError("invalid_ooxml_relationship", f"referenced OOXML part is missing: {part}")
    root = _parse_xml(archive.read(part), part)
    parents = _parent_map(root)
    part_relationships = relationships.get(part, {})
    for blip in (node for node in root.iter() if _local_name(node.tag) == "blip"):
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
            )
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
        if len(ordered) >= limits.max_assets:
            raise OfficeError("office_asset_limit", "Office image count exceeds the limit")
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


def _remove_converter_image_markup(text: str) -> str:
    def replacement(match: re.Match[str]) -> str:
        alt = match.group(1).strip()
        return f"[图片：{alt}]" if alt else "[图片]"

    return _HTML_RESOURCE_TAG.sub("", _MARKDOWN_IMAGE.sub(replacement, text))


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
    units: list[tuple[str, SourceLocation]],
) -> tuple[dict[str, str], list[ManifestWarning]]:
    if len(units) == 1:
        return {units[0][0]: markdown}, []
    if source_format == "pptx":
        matches = list(_PPTX_SLIDE_MARKER.finditer(markdown))
        sections: dict[int, str] = {}
        for index, match in enumerate(matches):
            slide = int(match.group(1))
            end = matches[index + 1].start() if index + 1 < len(matches) else len(markdown)
            sections[slide] = _normalize_markdown(markdown[match.start() : end])
        if set(sections) == set(range(1, len(units) + 1)):
            return {unit_id: sections[index] for index, (unit_id, _) in enumerate(units, 1)}, []
        slide_parts = [part for part, _, _ in _pptx_units(archive)]
        fallback = {
            unit_id: _fallback_part_text(archive, slide_parts[index - 1])
            for index, (unit_id, _) in enumerate(units, 1)
        }
    else:
        relationships = _load_relationships(archive)
        sheet_parts = [part for part, _, _ in _xlsx_units(archive, relationships)]
        fallback = {
            unit_id: _fallback_part_text(archive, sheet_parts[index - 1])
            for index, (unit_id, _) in enumerate(units, 1)
        }
    return fallback, [
        ManifestWarning(
            code="office_markdown_coarse_partition",
            message="MarkItDown 输出无法稳定分段，已按 slide/sheet 保守提取可见文本",
            location=None,
            degraded=True,
        )
    ]


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
    markdown = _normalize_markdown(
        _remove_converter_image_markup(converter.convert(sanitized_source, source_format))
    )
    with zipfile.ZipFile(sanitized_source) as archive:
        relationships = _load_relationships(archive)
        unit_values, pending = _find_occurrences(
            archive, source_format, relationships
        )
        unit_markdown, partition_warnings = _partition_markdown(
            archive, source_format, markdown, unit_values
        )
        assets, assets_by_target = _materialize_images(
            archive, pending, request_dir, limits
        )

    occurrences: list[ManifestOccurrence] = []
    occurrences_by_unit: dict[str, list[ManifestOccurrence]] = {
        unit_id: [] for unit_id, _ in unit_values
    }
    for order, item in enumerate(pending):
        asset = assets_by_target[item.relationship.target]
        location_fragment = (
            f"{item.location.kind}_{item.location.index:04d}"
            if item.location.index
            else "document_0000"
        )
        occurrence = ManifestOccurrence(
            id=f"occ_{location_fragment}_{order + 1:04d}",
            asset_local_id=asset.local_id,
            unit_id=item.unit_id,
            order=order,
            location=item.location,
            bbox=item.bbox,
            alt_text=item.alt_text,
            caption="",
            ocr_text="",
            decorative=False,
            confidence=1.0,
        )
        occurrences.append(occurrence)
        occurrences_by_unit[item.unit_id].append(occurrence)

    payloads: list[PayloadEntry] = []
    units: list[MarkdownUnit] = []
    manifest_warnings = list(preflight_warnings) + partition_warnings
    for index, (unit_id, location) in enumerate(unit_values, 1):
        value = unit_markdown.get(unit_id, "")
        local_occurrences = occurrences_by_unit[unit_id]
        if local_occurrences:
            markers = "\n".join(
                _marker(occurrence.id, occurrence.alt_text)
                for occurrence in local_occurrences
            )
            value = f"{value.rstrip()}\n\n### 相关图片\n\n{markers}\n"
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
