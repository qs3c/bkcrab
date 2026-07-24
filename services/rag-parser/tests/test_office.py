from __future__ import annotations

import asyncio
import hashlib
import io
import json
import re
import shutil
import time
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

import olefile
import pytest
from fastapi.testclient import TestClient
from PIL import Image

from app.main import Settings, _ParserWork, _run_parser_operation, create_app
from app.office import (
    DOC_REL_NS,
    REL_NS,
    MarkItDownConverter,
    OfficeError,
    OfficeLimits,
    build_office_bundle,
    preflight_ooxml,
)
from app.protocol import Manifest
from tests.fixtures.generate_minimal import generate_all
from tests.fixtures.generate_office_golden import generate_all as generate_office_golden

FIXTURE_ROOT = Path(__file__).resolve().parent / "fixtures"
EXPECTED = json.loads((FIXTURE_ROOT / "expected_minimal.json").read_text(encoding="utf-8"))
OFFICE_GOLDEN = json.loads(
    (FIXTURE_ROOT / "expected_office_golden.json").read_text(encoding="utf-8")
)
MIME_TYPES = {
    "docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
    "pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
    "xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
}


def _limits(**overrides: int) -> OfficeLimits:
    values = {
        "max_archive_entries": 10_000,
        "max_zip_entry_bytes": 8 * 1024 * 1024,
        "max_extracted_bytes": 64 * 1024 * 1024,
        "max_compression_ratio": 200,
        "max_asset_bytes": 2 * 1024 * 1024,
        "max_assets": 100,
        "max_image_pixels": 1_000_000,
    }
    values.update(overrides)
    return OfficeLimits(**values)


def _settings(
    temp_root: Path,
    max_input_bytes: int = 8 * 1024 * 1024,
    parse_timeout_seconds: int = 30,
) -> Settings:
    return Settings(
        service_version="office-test",
        max_input_bytes=max_input_bytes,
        max_output_bytes=64 * 1024 * 1024,
        max_entry_bytes=8 * 1024 * 1024,
        max_bundle_entries=1000,
        parse_timeout_seconds=parse_timeout_seconds,
        temp_root=temp_root,
        office_limits=_limits(),
    )


class _Result:
    text_content = "# Converted only through stream\n"


class _StreamOnlyEngine:
    def __init__(self):
        self.calls: list[tuple[bytes, str]] = []

    def convert_stream(self, stream, *, file_extension: str):
        self.calls.append((stream.read(4), file_extension))
        return _Result()

    def convert_uri(self, *_args, **_kwargs):
        raise AssertionError("URI conversion must never be reachable")


class _FakeConverter:
    def convert(self, _source: Path, source_format: str) -> str:
        return f"# Converted {source_format}\n"


class _FakeEMFConverter:
    def convert(self, _source: Path, destination: Path, _request_dir: Path) -> None:
        Image.new("RGB", (32, 24), (255, 255, 255)).save(destination, format="PNG")


class _VisioProbeConverter:
    def convert(self, source: Path, source_format: str) -> str:
        assert source_format == "docx"
        with zipfile.ZipFile(source) as archive:
            assert "word/embeddings/oleObject1.bin" not in archive.namelist()
            relationships = archive.read("word/_rels/document.xml.rels")
            assert b"/oleObject" not in relationships
            document = archive.read("word/document.xml").decode("utf-8")
        token = re.search(r"BKCRABIMAGE[A-F0-9]{32}\d{8}TOKEN", document)
        assert token is not None
        # Real MarkItDown output for an embedded Visio OLE object is a plain
        # sentinel, not Markdown image syntax.
        return f"# Visio\n\n{token.group(0)}\n"


class _BlockingConverter:
    def __init__(self, heartbeat: Path) -> None:
        self.heartbeat = heartbeat

    def convert(self, _source: Path, _source_format: str) -> str:
        while True:
            with self.heartbeat.open("ab") as handle:
                handle.write(b"x")
                handle.flush()
            time.sleep(0.01)


class _DisconnectWhenWorkerStarts:
    def __init__(self, heartbeat: Path) -> None:
        self.heartbeat = heartbeat

    async def is_disconnected(self) -> bool:
        return self.heartbeat.is_file()


def _sha_size(path: Path) -> tuple[str, int]:
    value = path.read_bytes()
    return hashlib.sha256(value).hexdigest(), len(value)


def _rewrite_zip(
    source: Path,
    destination: Path,
    *,
    replace: dict[str, bytes] | None = None,
    additions: dict[str, bytes] | None = None,
) -> None:
    replace = replace or {}
    additions = additions or {}
    with zipfile.ZipFile(source) as original, zipfile.ZipFile(
        destination, "w", compression=zipfile.ZIP_DEFLATED
    ) as output:
        for info in original.infolist():
            if info.filename in additions:
                raise AssertionError("test addition conflicts with fixture entry")
            output.writestr(info.filename, replace.get(info.filename, original.read(info)))
        for name, value in additions.items():
            output.writestr(name, value)


def _relationship_xml_with_external(
    original: bytes, *, relationship_type: str, target: str, rel_id: str = "rIdExternal"
) -> bytes:
    root = ET.fromstring(original)
    ET.SubElement(
        root,
        f"{{{REL_NS}}}Relationship",
        {
            "Id": rel_id,
            "Type": relationship_type,
            "Target": target,
            "TargetMode": "External",
        },
    )
    return ET.tostring(root, encoding="utf-8", xml_declaration=True)


def test_markitdown_wrapper_exposes_only_convert_stream(tmp_path: Path) -> None:
    source = tmp_path / "input.docx"
    source.write_bytes(b"PK\x03\x04payload")
    engine = _StreamOnlyEngine()
    converter = MarkItDownConverter(engine)
    assert converter.convert(source, "docx").startswith("# Converted")
    assert engine.calls == [(b"PK\x03\x04", ".docx")]


def test_docx_visio_ole_emits_safe_preview_and_downloadable_vsdx(
    tmp_path: Path,
) -> None:
    source = FIXTURE_ROOT / "visio-embedded.docx"
    request_dir = tmp_path / "request-visio"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(source, "docx", request_dir, _limits())
    sha256, byte_size = _sha_size(source)
    bundle = build_office_bundle(
        original_source=source,
        sanitized_source=preflight.sanitized_path,
        source_format="docx",
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=_VisioProbeConverter(),
        emf_converter=_FakeEMFConverter(),
        limits=_limits(),
        preflight_warnings=preflight.warnings,
    )
    assert len(bundle.manifest.assets) == 1
    assert bundle.manifest.assets[0].source_kind == "embedded_preview"
    assert bundle.manifest.assets[0].entry.endswith(".png")
    assert len(bundle.manifest.attachments) == 1
    attachment = bundle.manifest.attachments[0]
    assert attachment.kind == "visio_source"
    assert attachment.entry.endswith(".vsdx")
    assert attachment.file_name.endswith(".vsdx")
    assert bundle.manifest.occurrences[0].attachment_local_id == attachment.local_id
    payloads = {payload.path: payload for payload in bundle.payloads}
    vsdx = payloads[attachment.entry].opener().read()
    assert vsdx.startswith(b"PK\x03\x04")
    with zipfile.ZipFile(source) as office_archive:
        embedded = office_archive.read("word/embeddings/oleObject1.bin")
    with olefile.OleFileIO(io.BytesIO(embedded)) as ole:
        assert vsdx == ole.openstream("Package").read()
    with zipfile.ZipFile(io.BytesIO(vsdx)) as archive:
        assert "visio/document.xml" in archive.namelist()
    markdown = next(
        payload.opener().read().decode("utf-8")
        for payload in bundle.payloads
        if payload.path.startswith("units/")
    )
    assert "rag-asset://occ_document_0000_0001" in markdown
    assert "BKCRABIMAGE" not in markdown
    assert not bundle.manifest.warnings
    bundle.close()
    assert not request_dir.exists()


def test_unknown_internal_ole_is_not_allowlisted_as_visio(tmp_path: Path) -> None:
    source = FIXTURE_ROOT / "visio-embedded.docx"
    with zipfile.ZipFile(source) as archive:
        document = ET.fromstring(archive.read("word/document.xml"))
    ole = next(
        node for node in document.iter() if node.tag.rsplit("}", 1)[-1] == "OLEObject"
    )
    ole.set("ProgID", "Excel.Sheet.12")
    changed = tmp_path / "unknown-ole.docx"
    _rewrite_zip(
        source,
        changed,
        replace={
            "word/document.xml": ET.tostring(
                document, encoding="utf-8", xml_declaration=True
            )
        },
    )
    request_dir = tmp_path / "request"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(changed, "docx", request_dir, _limits())
    sha256, byte_size = _sha_size(changed)
    with pytest.raises(OfficeError, match="only embedded Visio"):
        build_office_bundle(
            original_source=changed,
            sanitized_source=preflight.sanitized_path,
            source_format="docx",
            source_sha256=sha256,
            source_size=byte_size,
            request_dir=request_dir,
            converter=_FakeConverter(),
            emf_converter=_FakeEMFConverter(),
            limits=_limits(),
        )


def test_corrupt_visio_ole_does_not_reach_markitdown(tmp_path: Path) -> None:
    source = FIXTURE_ROOT / "visio-embedded.docx"
    changed = tmp_path / "corrupt-visio.docx"
    _rewrite_zip(
        source,
        changed,
        replace={"word/embeddings/oleObject1.bin": b"not-an-ole-package"},
    )
    request_dir = tmp_path / "request"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(changed, "docx", request_dir, _limits())
    sha256, byte_size = _sha_size(changed)

    class _MustNotRun:
        def convert(self, _source: Path, _source_format: str) -> str:
            raise AssertionError("corrupt OLE must fail before MarkItDown")

    with pytest.raises(OfficeError, match="no valid VSDX"):
        build_office_bundle(
            original_source=changed,
            sanitized_source=preflight.sanitized_path,
            source_format="docx",
            source_sha256=sha256,
            source_size=byte_size,
            request_dir=request_dir,
            converter=_MustNotRun(),
            emf_converter=_FakeEMFConverter(),
            limits=_limits(),
        )


def _visio_fixture_parts() -> tuple[bytes, bytes]:
    with zipfile.ZipFile(FIXTURE_ROOT / "visio-embedded.docx") as archive:
        return (
            archive.read("word/embeddings/oleObject1.bin"),
            archive.read("word/media/image1.emf"),
        )


def test_pptx_visio_object_uses_its_nested_preview_and_attachment(
    tmp_path: Path,
) -> None:
    source = generate_all(tmp_path / "fixtures")["pptx"]
    ole_payload, emf_payload = _visio_fixture_parts()
    with zipfile.ZipFile(source) as archive:
        slide_name = next(
            name
            for name in archive.namelist()
            if name.startswith("ppt/slides/slide") and name.endswith(".xml")
        )
        rels_name = (
            f"{Path(slide_name).parent.as_posix()}/_rels/"
            f"{Path(slide_name).name}.rels"
        )
        slide = ET.fromstring(archive.read(slide_name))
        relationships = ET.fromstring(archive.read(rels_name))
    ole = ET.SubElement(
        slide,
        "oleObj",
        {f"{{{DOC_REL_NS}}}id": "rIdVisioOLE", "progId": "Visio.Drawing.15"},
    )
    ET.SubElement(ole, "embed")
    picture = ET.SubElement(ole, "pic")
    ET.SubElement(
        picture,
        "blip",
        {f"{{{DOC_REL_NS}}}embed": "rIdVisioPreview"},
    )
    ET.SubElement(
        relationships,
        f"{{{REL_NS}}}Relationship",
        {
            "Id": "rIdVisioOLE",
            "Type": f"{DOC_REL_NS}/oleObject",
            "Target": "../embeddings/oleObject-visio.bin",
        },
    )
    ET.SubElement(
        relationships,
        f"{{{REL_NS}}}Relationship",
        {
            "Id": "rIdVisioPreview",
            "Type": f"{DOC_REL_NS}/image",
            "Target": "../media/visio-preview.emf",
        },
    )
    changed = tmp_path / "visio.pptx"
    _rewrite_zip(
        source,
        changed,
        replace={
            slide_name: ET.tostring(slide, encoding="utf-8", xml_declaration=True),
            rels_name: ET.tostring(
                relationships, encoding="utf-8", xml_declaration=True
            ),
        },
        additions={
            "ppt/embeddings/oleObject-visio.bin": ole_payload,
            "ppt/media/visio-preview.emf": emf_payload,
        },
    )
    request_dir = tmp_path / "request-pptx"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(changed, "pptx", request_dir, _limits())
    sha256, byte_size = _sha_size(changed)
    bundle = build_office_bundle(
        original_source=changed,
        sanitized_source=preflight.sanitized_path,
        source_format="pptx",
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=_FakeConverter(),
        emf_converter=_FakeEMFConverter(),
        limits=_limits(),
    )
    assert len(bundle.manifest.attachments) == 1
    assert any(
        occurrence.attachment_local_id == bundle.manifest.attachments[0].local_id
        for occurrence in bundle.manifest.occurrences
    )
    bundle.close()


def test_xlsx_visio_object_resolves_vml_preview_by_shape_id(tmp_path: Path) -> None:
    source = generate_all(tmp_path / "fixtures")["xlsx"]
    ole_payload, emf_payload = _visio_fixture_parts()
    with zipfile.ZipFile(source) as archive:
        sheet_name = next(
            name
            for name in archive.namelist()
            if name.startswith("xl/worksheets/sheet") and name.endswith(".xml")
        )
        rels_name = (
            f"{Path(sheet_name).parent.as_posix()}/_rels/"
            f"{Path(sheet_name).name}.rels"
        )
        sheet = ET.fromstring(archive.read(sheet_name))
        relationships = ET.fromstring(archive.read(rels_name))
    ET.SubElement(
        sheet,
        "oleObject",
        {
            f"{{{DOC_REL_NS}}}id": "rIdVisioOLE",
            "progId": "Visio.Drawing.15",
            "shapeId": "1025",
        },
    )
    ET.SubElement(
        sheet,
        "legacyDrawing",
        {f"{{{DOC_REL_NS}}}id": "rIdVisioVML"},
    )
    ET.SubElement(
        relationships,
        f"{{{REL_NS}}}Relationship",
        {
            "Id": "rIdVisioOLE",
            "Type": f"{DOC_REL_NS}/oleObject",
            "Target": "../embeddings/oleObject-visio.bin",
        },
    )
    ET.SubElement(
        relationships,
        f"{{{REL_NS}}}Relationship",
        {
            "Id": "rIdVisioVML",
            "Type": f"{DOC_REL_NS}/vmlDrawing",
            "Target": "../drawings/vmlDrawing-visio.vml",
        },
    )
    vml = ET.Element("xml")
    shape = ET.SubElement(vml, "shape", {"id": "_x0000_s1025"})
    ET.SubElement(
        shape,
        "imagedata",
        {f"{{{DOC_REL_NS}}}id": "rIdVisioPreview"},
    )
    client_data = ET.SubElement(shape, "ClientData")
    ET.SubElement(client_data, "Anchor").text = "1, 0, 2, 0, 3, 0, 8, 0"
    vml_relationships = ET.Element(f"{{{REL_NS}}}Relationships")
    ET.SubElement(
        vml_relationships,
        f"{{{REL_NS}}}Relationship",
        {
            "Id": "rIdVisioPreview",
            "Type": f"{DOC_REL_NS}/image",
            "Target": "../media/visio-preview.emf",
        },
    )
    changed = tmp_path / "visio.xlsx"
    _rewrite_zip(
        source,
        changed,
        replace={
            sheet_name: ET.tostring(sheet, encoding="utf-8", xml_declaration=True),
            rels_name: ET.tostring(
                relationships, encoding="utf-8", xml_declaration=True
            ),
        },
        additions={
            "xl/embeddings/oleObject-visio.bin": ole_payload,
            "xl/drawings/vmlDrawing-visio.vml": ET.tostring(
                vml, encoding="utf-8", xml_declaration=True
            ),
            "xl/drawings/_rels/vmlDrawing-visio.vml.rels": ET.tostring(
                vml_relationships, encoding="utf-8", xml_declaration=True
            ),
            "xl/media/visio-preview.emf": emf_payload,
        },
    )
    request_dir = tmp_path / "request-xlsx"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(changed, "xlsx", request_dir, _limits())
    sha256, byte_size = _sha_size(changed)
    bundle = build_office_bundle(
        original_source=changed,
        sanitized_source=preflight.sanitized_path,
        source_format="xlsx",
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=_FakeConverter(),
        emf_converter=_FakeEMFConverter(),
        limits=_limits(),
    )
    visio_occurrences = [
        occurrence
        for occurrence in bundle.manifest.occurrences
        if occurrence.attachment_local_id
    ]
    assert len(bundle.manifest.attachments) == len(visio_occurrences) == 1
    assert visio_occurrences[0].alt_text.startswith("单元格 B3：")
    bundle.close()


@pytest.mark.parametrize("source_format", ["docx", "pptx", "xlsx"])
def test_three_format_spike_produces_units_assets_and_internal_markers(
    source_format: str, tmp_path: Path
) -> None:
    fixtures = generate_all(tmp_path / "fixtures")
    source = fixtures[source_format]
    request_dir = tmp_path / f"request-{source_format}"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(source, source_format, request_dir, _limits())
    sha256, byte_size = _sha_size(source)
    bundle = build_office_bundle(
        original_source=source,
        sanitized_source=preflight.sanitized_path,
        source_format=source_format,
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=MarkItDownConverter(),
        limits=_limits(),
        preflight_warnings=preflight.warnings,
    )
    expected = EXPECTED[source_format]
    assert len(bundle.manifest.units) == expected["unitCount"]
    assert bundle.manifest.units[0].location.kind == expected["unitKind"]
    assert len(bundle.manifest.assets) == expected["assetCount"]
    assert len(bundle.manifest.occurrences) == expected["occurrenceCount"]
    warning_codes = {warning.code for warning in bundle.manifest.warnings}
    if source_format == "xlsx":
        assert expected["warningCode"] in warning_codes
    else:
        assert expected["warningCode"] not in warning_codes
    assert Manifest.from_dict(bundle.manifest.to_dict()) == bundle.manifest
    markdown_payloads = [item for item in bundle.payloads if item.path.startswith("units/")]
    markdown = "".join(item.opener().read().decode("utf-8") for item in markdown_payloads)
    assert "rag-asset://occ_" in markdown
    assert "BKCRABIMAGE" not in markdown
    assert "BKCRABSHEET" not in markdown
    assert "BKCRABCODE" not in markdown
    if source_format == "xlsx":
        assert "## Summary" in markdown
    assert "data:image" not in markdown.lower()
    assert "file://" not in markdown.lower()
    bundle.close()
    assert not request_dir.exists()


@pytest.mark.parametrize("source_format", ["docx", "pptx", "xlsx"])
def test_office_positioning_golden(
    source_format: str, tmp_path: Path
) -> None:
    source = generate_office_golden(tmp_path / "fixtures")[source_format]
    request_dir = tmp_path / f"request-{source_format}"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(source, source_format, request_dir, _limits())
    sha256, byte_size = _sha_size(source)
    bundle = build_office_bundle(
        original_source=source,
        sanitized_source=preflight.sanitized_path,
        source_format=source_format,
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=MarkItDownConverter(),
        limits=_limits(),
        preflight_warnings=preflight.warnings,
    )
    expected = OFFICE_GOLDEN[source_format]
    markdown_payloads = [
        item for item in bundle.payloads if item.path.startswith("units/")
    ]
    markdown_by_unit = [
        item.opener().read().decode("utf-8") for item in markdown_payloads
    ]
    combined_markdown = "\n".join(markdown_by_unit)

    assert [unit.location.kind for unit in bundle.manifest.units] == expected[
        "unitKinds"
    ]
    assert [unit.location.label for unit in bundle.manifest.units] == expected[
        "unitLabels"
    ]
    assert len(bundle.manifest.assets) == expected["assetCount"]
    assert len(bundle.manifest.occurrences) == expected["occurrenceCount"]
    assert [item.alt_text for item in bundle.manifest.occurrences] == expected[
        "occurrenceAltTexts"
    ]
    assert [item.code for item in bundle.manifest.warnings] == expected["warningCodes"]
    for fragment in expected["requiredMarkdown"]:
        assert fragment in combined_markdown
    positions = [combined_markdown.index(fragment) for fragment in expected["orderedMarkdown"]]
    assert positions == sorted(positions)
    assert "data:image" not in combined_markdown.lower()
    assert "Picture" not in combined_markdown
    assert "image1.png" not in combined_markdown
    for occurrence in bundle.manifest.occurrences:
        assert combined_markdown.count(f"rag-asset://{occurrence.id}") == 1

    if source_format == "docx":
        assert combined_markdown.count("```") == 4
        prose_start = combined_markdown.index("ordinary monospace stays prose")
        assert combined_markdown.rfind("```", 0, prose_start) < prose_start
        with zipfile.ZipFile(source) as archive:
            document = ET.fromstring(archive.read("word/document.xml"))
            rel_ids = [
                node.attrib[f"{{{DOC_REL_NS}}}embed"]
                for node in document.iter()
                if node.tag.rsplit("}", 1)[-1] == "blip"
            ]
        assert len(rel_ids) == 2 and len(set(rel_ids)) == 1
    elif source_format == "pptx":
        assert bundle.manifest.units[0].id == "unit_slide_0001"
        assert "Presentation-order first slide" in markdown_by_unit[0]
        assert markdown_by_unit[1].rstrip().endswith(
            "> Remember the architecture caveat."
        )
    else:
        assert all(warning.degraded for warning in bundle.manifest.warnings)
        assert [occurrence.order for occurrence in bundle.manifest.occurrences] == [0, 0]

    bundle.close()
    assert not request_dir.exists()


def test_random_converter_sentinels_cannot_be_forged_by_document_text(
    tmp_path: Path,
) -> None:
    class SentinelProbeConverter:
        def __init__(self) -> None:
            self.nonces: list[str] = []

        def convert(self, source: Path, _source_format: str) -> str:
            with zipfile.ZipFile(source) as archive:
                xml = "\n".join(
                    archive.read(name).decode("utf-8")
                    for name in archive.namelist()
                    if name.endswith(".xml")
                )
            images = re.findall(
                r"BKCRABIMAGE[A-F0-9]{32}\d{8}TOKEN", xml
            )
            starts = re.findall(
                r"BKCRABCODESTART[A-F0-9]{32}\d{8}TOKEN", xml
            )
            ends = re.findall(
                r"BKCRABCODEEND[A-F0-9]{32}\d{8}TOKEN", xml
            )
            assert len(images) == 2
            assert len(starts) == len(ends) == 2
            nonce = re.fullmatch(
                r"BKCRABIMAGE([A-F0-9]{32})\d{8}TOKEN", images[0]
            )
            assert nonce is not None
            self.nonces.append(nonce.group(1))
            values = [
                "Natural BKCRABIMAGE00000001TOKEN text.",
                "Natural BKCRABSHEET0001TOKEN text.",
                (
                    "BKCRABCODESTART00000001TOKEN"
                    "ATTACK"
                    "BKCRABCODEEND00000001TOKEN"
                ),
            ]
            values.extend(
                f"{start}converter text{end}"
                for start, end in zip(starts, ends, strict=True)
            )
            values.append(images[0])
            values.append(f"![{images[1]}](data:image/png;base64,ignored)")
            return "\n\n".join(values)

    source = generate_office_golden(tmp_path / "fixtures")["docx"]
    converter = SentinelProbeConverter()
    markdown_values: list[str] = []
    for attempt in range(2):
        request_dir = tmp_path / f"request-{attempt}"
        request_dir.mkdir(mode=0o700)
        preflight = preflight_ooxml(source, "docx", request_dir, _limits())
        sha256, byte_size = _sha_size(source)
        bundle = build_office_bundle(
            original_source=source,
            sanitized_source=preflight.sanitized_path,
            source_format="docx",
            source_sha256=sha256,
            source_size=byte_size,
            request_dir=request_dir,
            converter=converter,
            limits=_limits(),
        )
        markdown_values.append(
            next(
                item.opener().read().decode("utf-8")
                for item in bundle.payloads
                if item.path.startswith("units/")
            )
        )
        assert not bundle.manifest.warnings
        bundle.close()

    assert converter.nonces[0] != converter.nonces[1]
    assert markdown_values[0] == markdown_values[1]
    assert "Natural BKCRABIMAGE00000001TOKEN text." in markdown_values[0]
    assert "Natural BKCRABSHEET0001TOKEN text." in markdown_values[0]
    assert "BKCRABCODESTART00000001TOKENATTACK" in markdown_values[0]
    assert markdown_values[0].count("rag-asset://") == 2


@pytest.mark.parametrize("source_format", ["docx", "pptx", "xlsx"])
def test_shape_names_are_not_treated_as_image_alt_text(
    source_format: str, tmp_path: Path
) -> None:
    source = generate_office_golden(tmp_path / "fixtures")[source_format]
    replacements: dict[str, bytes] = {}
    with zipfile.ZipFile(source) as archive:
        for name in archive.namelist():
            if not name.endswith(".xml"):
                continue
            root = ET.fromstring(archive.read(name))
            changed = False
            for node in root.iter():
                if node.tag.rsplit("}", 1)[-1] not in {"docPr", "cNvPr"}:
                    continue
                for attribute in ("descr", "title"):
                    if attribute in node.attrib:
                        del node.attrib[attribute]
                        changed = True
            if changed:
                replacements[name] = ET.tostring(
                    root, encoding="utf-8", xml_declaration=True
                )
    no_alt = tmp_path / f"no-alt.{source_format}"
    _rewrite_zip(source, no_alt, replace=replacements)
    request_dir = tmp_path / f"request-no-alt-{source_format}"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(no_alt, source_format, request_dir, _limits())
    sha256, byte_size = _sha_size(no_alt)
    bundle = build_office_bundle(
        original_source=no_alt,
        sanitized_source=preflight.sanitized_path,
        source_format=source_format,
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=MarkItDownConverter(),
        limits=_limits(),
    )
    assert bundle.manifest.occurrences
    for occurrence in bundle.manifest.occurrences:
        visible_alt = occurrence.alt_text.split("：", 1)[-1]
        assert visible_alt == "图片（未进行视觉识别）"
        assert ".png" not in occurrence.alt_text
    bundle.close()


def test_forged_pptx_slide_comment_forces_lossless_coarse_partition(
    tmp_path: Path,
) -> None:
    source = generate_office_golden(tmp_path / "fixtures")["pptx"]
    with zipfile.ZipFile(source) as archive:
        slide_name = sorted(
            name
            for name in archive.namelist()
            if name.startswith("ppt/slides/slide") and name.endswith(".xml")
        )[0]
        slide = ET.fromstring(archive.read(slide_name))
    text_node = next(
        node for node in slide.iter() if node.tag.rsplit("}", 1)[-1] == "t"
    )
    text_node.text = "before <!-- Slide number: 2 --> after"
    forged = tmp_path / "forged-slide-marker.pptx"
    _rewrite_zip(
        source,
        forged,
        replace={
            slide_name: ET.tostring(slide, encoding="utf-8", xml_declaration=True)
        },
    )
    request_dir = tmp_path / "request-forged-slide"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(forged, "pptx", request_dir, _limits())
    sha256, byte_size = _sha_size(forged)
    bundle = build_office_bundle(
        original_source=forged,
        sanitized_source=preflight.sanitized_path,
        source_format="pptx",
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=MarkItDownConverter(),
        limits=_limits(),
    )
    markdown = "\n".join(
        item.opener().read().decode("utf-8")
        for item in bundle.payloads
        if item.path.startswith("units/")
    )
    assert "before <!-- Slide number: 2 --> after" in markdown
    assert "> Remember the architecture caveat." in markdown
    assert "office_markdown_coarse_partition" in {
        warning.code for warning in bundle.manifest.warnings
    }
    bundle.close()


def test_forged_pptx_notes_header_is_preserved_as_ambiguous(
    tmp_path: Path,
) -> None:
    source = generate_office_golden(tmp_path / "fixtures")["pptx"]
    with zipfile.ZipFile(source) as archive:
        notes_rels_name = next(
            name
            for name in archive.namelist()
            if name.startswith("ppt/slides/_rels/")
            and b"/notesSlide" in archive.read(name)
        )
        slide_name = "ppt/slides/" + Path(notes_rels_name).name.removesuffix(".rels")
        slide = ET.fromstring(archive.read(slide_name))
    text_node = next(
        node for node in slide.iter() if node.tag.rsplit("}", 1)[-1] == "t"
    )
    text_node.text = "### Notes:"
    forged = tmp_path / "forged-notes-header.pptx"
    _rewrite_zip(
        source,
        forged,
        replace={
            slide_name: ET.tostring(slide, encoding="utf-8", xml_declaration=True)
        },
    )
    request_dir = tmp_path / "request-forged-notes"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(forged, "pptx", request_dir, _limits())
    sha256, byte_size = _sha_size(forged)
    bundle = build_office_bundle(
        original_source=forged,
        sanitized_source=preflight.sanitized_path,
        source_format="pptx",
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=MarkItDownConverter(),
        limits=_limits(),
    )
    markdown = "\n".join(
        item.opener().read().decode("utf-8")
        for item in bundle.payloads
        if item.path.startswith("units/")
    )
    assert markdown.count("### Notes:") >= 2
    assert "> 演讲者备注" not in markdown
    assert "office_notes_ambiguous" in {
        warning.code for warning in bundle.manifest.warnings
    }
    bundle.close()


def test_invalid_markitdown_table_falls_back_to_cell_text(tmp_path: Path) -> None:
    class InvalidTableConverter:
        def convert(self, _source: Path, _source_format: str) -> str:
            return "| A | B |\n| --- |\n| one | two |\n"

    source = generate_all(tmp_path / "fixtures")["docx"]
    request_dir = tmp_path / "request"
    request_dir.mkdir(mode=0o700)
    preflight = preflight_ooxml(source, "docx", request_dir, _limits())
    sha256, byte_size = _sha_size(source)
    bundle = build_office_bundle(
        original_source=source,
        sanitized_source=preflight.sanitized_path,
        source_format="docx",
        source_sha256=sha256,
        source_size=byte_size,
        request_dir=request_dir,
        converter=InvalidTableConverter(),
        limits=_limits(),
    )
    markdown = next(
        item.opener().read().decode("utf-8")
        for item in bundle.payloads
        if item.path.startswith("units/")
    )
    assert "A | B" in markdown
    assert "one | two" in markdown
    assert "| --- |" not in markdown
    assert "office_table_invalid" in {
        warning.code for warning in bundle.manifest.warnings
    }
    bundle.close()


def test_dtd_entity_is_rejected_before_converter(tmp_path: Path) -> None:
    source = generate_all(tmp_path / "fixtures")["docx"]
    malicious = tmp_path / "malicious.docx"
    _rewrite_zip(
        source,
        malicious,
        additions={
            "word/evil.xml": b'<!DOCTYPE x [<!ENTITY leak SYSTEM "file:///secret">]><x>&leak;</x>'
        },
    )
    request_dir = tmp_path / "request"
    request_dir.mkdir()
    with pytest.raises(OfficeError, match="DTD/entity"):
        preflight_ooxml(malicious, "docx", request_dir, _limits())


def test_path_traversal_and_zip_bomb_are_rejected(tmp_path: Path) -> None:
    source = generate_all(tmp_path / "fixtures")["docx"]
    traversal = tmp_path / "traversal.docx"
    _rewrite_zip(source, traversal, additions={"../escape.txt": b"secret"})
    request_dir = tmp_path / "request-traversal"
    request_dir.mkdir()
    with pytest.raises(OfficeError, match="traversal"):
        preflight_ooxml(traversal, "docx", request_dir, _limits())

    bomb = tmp_path / "bomb.docx"
    _rewrite_zip(source, bomb, additions={"word/bomb.bin": b"A" * 100_000})
    request_dir = tmp_path / "request-bomb"
    request_dir.mkdir()
    with pytest.raises(OfficeError, match="compression ratio"):
        preflight_ooxml(
            bomb,
            "docx",
            request_dir,
            _limits(max_compression_ratio=2),
        )


@pytest.mark.parametrize(
    "relationship_name",
    ["attachedTemplate", "oleObject", "altChunk", "externalLink", "package"],
)
def test_dangerous_external_relationships_are_rejected(
    relationship_name: str, tmp_path: Path
) -> None:
    source = generate_all(tmp_path / "fixtures")["docx"]
    with zipfile.ZipFile(source) as archive:
        root_rels = archive.read("_rels/.rels")
    changed = _relationship_xml_with_external(
        root_rels,
        relationship_type=(
            "http://schemas.openxmlformats.org/officeDocument/2006/relationships/"
            + relationship_name
        ),
        target="https://example.invalid/content",
    )
    malicious = tmp_path / f"external-{relationship_name}.docx"
    _rewrite_zip(source, malicious, replace={"_rels/.rels": changed})
    request_dir = tmp_path / f"request-{relationship_name}"
    request_dir.mkdir()
    with pytest.raises(OfficeError, match="forbidden"):
        preflight_ooxml(malicious, "docx", request_dir, _limits())


@pytest.mark.parametrize(
    "target",
    ["file:///definitely-secret", "C:/definitely-secret", "\\\\server\\secret"],
)
def test_local_hyperlink_target_is_removed_without_reading(target: str, tmp_path: Path) -> None:
    source = generate_all(tmp_path / "fixtures")["docx"]
    with zipfile.ZipFile(source) as archive:
        root_rels = archive.read("_rels/.rels")
    changed = _relationship_xml_with_external(
        root_rels,
        relationship_type=(
            "http://schemas.openxmlformats.org/officeDocument/2006/relationships/hyperlink"
        ),
        target=target,
    )
    malicious = tmp_path / "local-link.docx"
    _rewrite_zip(source, malicious, replace={"_rels/.rels": changed})
    request_dir = tmp_path / "request"
    request_dir.mkdir()
    preflight = preflight_ooxml(malicious, "docx", request_dir, _limits())
    assert {warning.code for warning in preflight.warnings} == {
        "office_external_hyperlink_removed"
    }
    with zipfile.ZipFile(preflight.sanitized_path) as archive:
        assert target.encode() not in archive.read("_rels/.rels")


def test_internal_local_absolute_relationship_target_is_rejected(tmp_path: Path) -> None:
    source = generate_all(tmp_path / "fixtures")["docx"]
    with zipfile.ZipFile(source) as archive:
        relationships = ET.fromstring(archive.read("word/_rels/document.xml.rels"))
    image_relationship = next(
        node
        for node in relationships
        if str(node.attrib.get("Type", "")).lower().endswith("/image")
    )
    image_relationship.set("Target", "/etc/definitely-secret")
    malicious = tmp_path / "absolute-local-target.docx"
    _rewrite_zip(
        source,
        malicious,
        replace={
            "word/_rels/document.xml.rels": ET.tostring(
                relationships, encoding="utf-8", xml_declaration=True
            )
        },
    )
    request_dir = tmp_path / "request-absolute"
    request_dir.mkdir()
    with pytest.raises(OfficeError, match="local absolute"):
        preflight_ooxml(malicious, "docx", request_dir, _limits())


@pytest.mark.parametrize(
    ("coordinate_name", "coordinate_value"),
    [("col", "-2"), ("row", "-1"), ("col", "16384"), ("row", "1048576")],
)
def test_xlsx_image_anchor_outside_excel_bounds_is_rejected(
    coordinate_name: str, coordinate_value: str, tmp_path: Path
) -> None:
    source = generate_office_golden(tmp_path / "fixtures")["xlsx"]
    with zipfile.ZipFile(source) as archive:
        drawing_name = next(
            name for name in archive.namelist() if name.startswith("xl/drawings/drawing")
        )
        drawing = ET.fromstring(archive.read(drawing_name))
    coordinate = next(
        node
        for node in drawing.iter()
        if node.tag.rsplit("}", 1)[-1] == coordinate_name
    )
    coordinate.text = coordinate_value
    malicious = tmp_path / f"invalid-{coordinate_name}-{coordinate_value}.xlsx"
    _rewrite_zip(
        source,
        malicious,
        replace={
            drawing_name: ET.tostring(
                drawing, encoding="utf-8", xml_declaration=True
            )
        },
    )
    request_dir = tmp_path / f"request-{coordinate_name}-{coordinate_value}"
    request_dir.mkdir()
    preflight = preflight_ooxml(malicious, "xlsx", request_dir, _limits())
    sha256, byte_size = _sha_size(malicious)
    with pytest.raises(OfficeError, match="outside Excel bounds"):
        build_office_bundle(
            original_source=malicious,
            sanitized_source=preflight.sanitized_path,
            source_format="xlsx",
            source_sha256=sha256,
            source_size=byte_size,
            request_dir=request_dir,
            converter=_FakeConverter(),
            limits=_limits(),
        )


def test_endpoint_enforces_format_extension_mime_magic_and_dynamic_input_limit(
    tmp_path: Path,
) -> None:
    source = generate_all(tmp_path / "fixtures")["docx"]
    with TestClient(create_app(_settings(tmp_path), _FakeConverter())) as client:
        wrong_extension = client.post(
            "/v1/office/convert?format=docx",
            files={"file": ("wrong.pptx", source.read_bytes(), MIME_TYPES["docx"])},
        )
        assert wrong_extension.status_code == 422
        wrong_mime = client.post(
            "/v1/office/convert?format=docx",
            files={"file": ("input.docx", source.read_bytes(), "application/octet-stream")},
        )
        assert wrong_mime.status_code == 422
        wrong_magic = client.post(
            "/v1/office/convert?format=docx",
            files={"file": ("input.docx", b"not-a-zip", MIME_TYPES["docx"])},
        )
        assert wrong_magic.status_code == 422

    with TestClient(
        create_app(_settings(tmp_path, max_input_bytes=8), _FakeConverter())
    ) as client:
        too_large = client.post(
            "/v1/office/convert?format=docx",
            files={"file": ("input.docx", source.read_bytes(), MIME_TYPES["docx"])},
        )
        assert too_large.status_code == 413


def test_endpoint_streams_manifest_first_and_cleans_request_directory(tmp_path: Path) -> None:
    fixture_dir = tmp_path / "fixture-source"
    source = generate_all(fixture_dir)["docx"]
    runtime_temp = tmp_path / "runtime-temp"
    runtime_temp.mkdir()
    with TestClient(create_app(_settings(runtime_temp), _FakeConverter())) as client:
        response = client.post(
            "/v1/office/convert?format=docx",
            files={"file": ("input.docx", source.read_bytes(), MIME_TYPES["docx"])},
        )
    assert response.status_code == 200
    assert response.headers["content-type"].startswith("application/x-tar")
    with zipfile.ZipFile(source):
        pass
    import tarfile

    with tarfile.open(fileobj=io.BytesIO(response.content), mode="r:") as archive:
        names = archive.getnames()
        assert names[0] == "manifest.json"
        assert names[1:] == sorted(names[1:])
        manifest = Manifest.from_dict(json.load(archive.extractfile("manifest.json")))
        assert manifest.source.format == "docx"
    assert list(runtime_temp.iterdir()) == []


def test_office_timeout_terminates_worker_and_cleans_request_directory(
    tmp_path: Path,
) -> None:
    source = generate_all(tmp_path / "fixture-source")["docx"]
    runtime_temp = tmp_path / "runtime-temp"
    runtime_temp.mkdir()
    heartbeat = tmp_path / "office-worker-heartbeat"
    started = time.monotonic()
    with TestClient(
        create_app(
            _settings(runtime_temp, parse_timeout_seconds=5),
            _BlockingConverter(heartbeat),
        )
    ) as client:
        response = client.post(
            "/v1/office/convert?format=docx",
            files={"file": ("input.docx", source.read_bytes(), MIME_TYPES["docx"])},
        )
    elapsed = time.monotonic() - started

    assert response.status_code == 504
    assert response.json()["error"]["code"] == "parse_timeout"
    assert elapsed < 9
    assert heartbeat.is_file()
    heartbeat_size = heartbeat.stat().st_size
    time.sleep(0.15)
    assert heartbeat.stat().st_size == heartbeat_size
    assert list(runtime_temp.iterdir()) == []


def test_disconnect_terminates_parser_worker_with_bounded_cleanup(tmp_path: Path) -> None:
    fixture = generate_all(tmp_path / "fixture-source")["docx"]
    request_dir = tmp_path / "request-disconnect"
    request_dir.mkdir()
    source = request_dir / "source.docx"
    source.write_bytes(fixture.read_bytes())
    source_sha256, source_size = _sha_size(source)
    heartbeat = tmp_path / "disconnect-worker-heartbeat"
    work = _ParserWork(
        operation="office",
        source=source,
        source_sha256=source_sha256,
        source_size=source_size,
        request_dir=request_dir,
        source_format="docx",
        office_limits=_limits(),
        converter=_BlockingConverter(heartbeat),
    )

    started = time.monotonic()
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(
            _run_parser_operation(
                _DisconnectWhenWorkerStarts(heartbeat),
                30,
                work,
                OfficeError("parse_timeout", "Office conversion timed out", 504),
            )
        )
    elapsed = time.monotonic() - started

    assert elapsed < 9
    heartbeat_size = heartbeat.stat().st_size
    time.sleep(0.15)
    assert heartbeat.stat().st_size == heartbeat_size
    shutil.rmtree(request_dir)
    assert not request_dir.exists()
