from __future__ import annotations

import hashlib
import io
import json
import zipfile
from pathlib import Path
from xml.etree import ElementTree as ET

import pytest
from fastapi.testclient import TestClient

from app.main import Settings, create_app
from app.office import (
    REL_NS,
    MarkItDownConverter,
    OfficeError,
    OfficeLimits,
    build_office_bundle,
    preflight_ooxml,
)
from app.protocol import Manifest
from tests.fixtures.generate_minimal import generate_all

FIXTURE_ROOT = Path(__file__).resolve().parent / "fixtures"
EXPECTED = json.loads((FIXTURE_ROOT / "expected_minimal.json").read_text(encoding="utf-8"))
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


def _settings(temp_root: Path, max_input_bytes: int = 8 * 1024 * 1024) -> Settings:
    return Settings(
        service_version="office-test",
        max_input_bytes=max_input_bytes,
        max_output_bytes=64 * 1024 * 1024,
        max_entry_bytes=8 * 1024 * 1024,
        max_bundle_entries=1000,
        parse_timeout_seconds=30,
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
    assert expected["warningCode"] in {warning.code for warning in bundle.manifest.warnings}
    assert Manifest.from_dict(bundle.manifest.to_dict()) == bundle.manifest
    markdown_payloads = [item for item in bundle.payloads if item.path.startswith("units/")]
    markdown = "".join(item.opener().read().decode("utf-8") for item in markdown_payloads)
    assert "rag-asset://occ_" in markdown
    assert "data:image" not in markdown.lower()
    assert "file://" not in markdown.lower()
    bundle.close()
    assert not request_dir.exists()


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
