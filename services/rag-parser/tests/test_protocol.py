from __future__ import annotations

import copy
import io
import json
import tarfile
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app.main import Settings, create_app
from app.office import OfficeLimits
from app.protocol import (
    PROTOCOL_VERSION,
    Bundle,
    BundleLimits,
    Manifest,
    MarkdownUnit,
    ParserDescriptor,
    PayloadEntry,
    ProtocolError,
    SourceDescriptor,
    SourceLocation,
    make_health_document,
    stream_tar,
    validate_bundle_path,
    validate_health_document,
    validate_page_primitive_document,
)

REPO_ROOT = Path(__file__).resolve().parents[3]
GOLDEN_ROOT = REPO_ROOT / "testdata" / "rag-parser-protocol" / "v2"


def _read_json(name: str) -> dict[str, object]:
    return json.loads((GOLDEN_ROOT / name).read_text(encoding="utf-8"))


@pytest.mark.parametrize(
    "name",
    [
        "manifest-office.json",
        "manifest-pdf-analyze.json",
        "manifest-pdf-render.json",
        "manifest-pdf-failed-page.json",
    ],
)
def test_shared_manifest_goldens_round_trip(name: str) -> None:
    value = _read_json(name)
    manifest = Manifest.from_dict(value)
    assert manifest.to_dict() == value


def test_shared_health_golden_and_pdf_stays_disabled() -> None:
    value = _read_json("health.json")
    assert validate_health_document(value) == value
    assert value["capabilities"]["pdf"] == {
        "enabled": False,
        "engine": "",
        "engineVersion": "",
    }


def test_shared_page_primitive_golden_has_exact_v1_shape() -> None:
    value = _read_json("page-primitive.json")
    assert validate_page_primitive_document(value) == value


def test_page_primitive_rejects_empty_text_blocks() -> None:
    value = _read_json("page-primitive.json")
    value["textBlocks"][0]["text"] = ""
    with pytest.raises(ProtocolError, match="must be a string"):
        validate_page_primitive_document(value)


def test_page_primitive_dimensions_match_go_positive_float64_boundary() -> None:
    value = _read_json("page-primitive.json")
    value["width"] = 5e-324
    value["height"] = 1e200
    assert validate_page_primitive_document(value) == value


@pytest.mark.parametrize("dimension", [0, -1, float("inf"), float("nan")])
def test_page_primitive_rejects_nonpositive_or_nonfinite_dimensions(dimension: float) -> None:
    value = _read_json("page-primitive.json")
    value["width"] = dimension
    with pytest.raises(ProtocolError):
        validate_page_primitive_document(value)


def test_manifest_rejects_unknown_fields_and_unknown_version() -> None:
    value = _read_json("manifest-office.json")
    unknown = copy.deepcopy(value)
    unknown["surprise"] = True
    with pytest.raises(ProtocolError, match="unknown fields"):
        Manifest.from_dict(unknown)
    wrong_version = copy.deepcopy(value)
    wrong_version["protocolVersion"] = "rag-parser/v1"
    with pytest.raises(ProtocolError, match="unsupported protocolVersion"):
        Manifest.from_dict(wrong_version)


def _duplicate_office_occurrence_order(value: dict[str, object]) -> None:
    occurrences = value["occurrences"]
    assert isinstance(occurrences, list)
    duplicate = copy.deepcopy(occurrences[0])
    duplicate["id"] = "occ_slide_0001_0002"
    occurrences.append(duplicate)


@pytest.mark.parametrize(
    ("name", "mutation"),
    [
        ("manifest-office.json", lambda value: value["source"].update({"format": "pdf"})),
        (
            "manifest-pdf-analyze.json",
            lambda value: value["source"].update({"format": "docx"}),
        ),
        (
            "manifest-office.json",
            lambda value: value["entries"][0].update({"mimeType": "image/jpeg"}),
        ),
        (
            "manifest-office.json",
            lambda value: value["units"][0].update({"id": "unit_slide_9999"}),
        ),
        (
            "manifest-office.json",
            lambda value: value["assets"][0].update({"sourceKind": "arbitrary"}),
        ),
        (
            "manifest-office.json",
            lambda value: value["parser"].update({"name": "   "}),
        ),
        ("manifest-office.json", _duplicate_office_occurrence_order),
    ],
)
def test_manifest_producer_validation_matches_go_invariants(name: str, mutation) -> None:
    value = _read_json(name)
    mutation(value)
    with pytest.raises(ProtocolError):
        Manifest.from_dict(value)


@pytest.mark.parametrize("name", ["manifest-pdf-analyze.json", "manifest-pdf-render.json"])
def test_pdf_protocol_layer_allows_an_empty_sorted_page_catalog_like_go(name: str) -> None:
    value = _read_json(name)
    for field in ("entries", "assets", "occurrences", "pages"):
        value[field] = []
    manifest = Manifest.from_dict(value)
    assert manifest.pages == ()


@pytest.mark.parametrize(
    ("name", "mutation"),
    [
        (
            "manifest-pdf-analyze.json",
            lambda value: value["pages"][0].update({"renderEntry": "units/page-0001.md"}),
        ),
        (
            "manifest-pdf-render.json",
            lambda value: value["pages"][0].update({"errorCode": "engine_error"}),
        ),
        (
            "manifest-pdf-failed-page.json",
            lambda value: value["pages"][1].update(
                {"nativeMarkdownEntry": "pages/page-0001.png"}
            ),
        ),
    ],
)
def test_pdf_required_forbidden_matrix_is_strict(name: str, mutation) -> None:
    value = _read_json(name)
    mutation(value)
    with pytest.raises(ProtocolError):
        Manifest.from_dict(value)


@pytest.mark.parametrize(
    "path",
    ["../secret", "/absolute", "C:/windows", "//server/share", "a\\b", "manifest.json"],
)
def test_bundle_paths_reject_escape_and_reserved_manifest(path: str) -> None:
    with pytest.raises(ProtocolError):
        validate_bundle_path(path)


def _minimal_bundle(payload: bytes, cleanup) -> Bundle:
    entry = PayloadEntry.from_bytes(
        "units/0001.md", "text/markdown; charset=utf-8", payload
    )
    manifest = Manifest(
        protocol_version=PROTOCOL_VERSION,
        bundle_kind="office-convert",
        source=SourceDescriptor("docx", 1, "1" * 64),
        parser=ParserDescriptor("markitdown", "0.1.6", "office-wrapper-v3"),
        entries=(entry.descriptor(),),
        units=(
            MarkdownUnit(
                "unit_document_0000",
                SourceLocation("document", 0, "文档"),
                "units/0001.md",
            ),
        ),
        assets=(),
        attachments=(),
        occurrences=(),
        pages=(),
        warnings=(),
    )
    return Bundle(manifest=manifest, payloads=(entry,), cleanup=cleanup)


def test_tar_is_manifest_first_ordered_deterministic_and_cleanup_once() -> None:
    cleanups = []
    bundle = _minimal_bundle(b"# hello\n", lambda: cleanups.append("closed"))
    limits = BundleLimits(max_output_bytes=64 * 1024, max_entry_bytes=1024, max_entries=5)
    encoded = b"".join(stream_tar(bundle, limits, chunk_size=3))
    bundle.close()
    assert cleanups == ["closed"]

    with tarfile.open(fileobj=io.BytesIO(encoded), mode="r:") as archive:
        members = archive.getmembers()
        assert [member.name for member in members] == ["manifest.json", "units/0001.md"]
        assert all(member.mtime == 0 for member in members)
        assert archive.extractfile(members[1]).read() == b"# hello\n"
        manifest_value = json.load(archive.extractfile(members[0]))
        assert Manifest.from_dict(manifest_value).bundle_kind == "office-convert"


def test_tar_enforces_entry_and_total_output_quotas_and_still_cleans() -> None:
    cleanups = []
    bundle = _minimal_bundle(b"x" * 2048, lambda: cleanups.append("closed"))
    limits = BundleLimits(max_output_bytes=4096, max_entry_bytes=16, max_entries=5)
    with pytest.raises(ProtocolError, match="single-entry"):
        b"".join(stream_tar(bundle, limits))
    assert cleanups == ["closed"]

    bundle = _minimal_bundle(b"x", lambda: cleanups.append("closed-again"))
    limits = BundleLimits(max_output_bytes=512, max_entry_bytes=16, max_entries=5)
    with pytest.raises(ProtocolError, match="maxOutputBytes"):
        b"".join(stream_tar(bundle, limits))
    assert cleanups[-1] == "closed-again"


def _settings(temp_root: Path, *, max_input: int = 1024) -> Settings:
    return Settings(
        service_version="unit-test",
        max_input_bytes=max_input,
        max_output_bytes=64 * 1024,
        max_entry_bytes=16 * 1024,
        max_bundle_entries=50,
        parse_timeout_seconds=10,
        temp_root=temp_root,
        office_limits=OfficeLimits(
            max_archive_entries=100,
            max_zip_entry_bytes=16 * 1024,
            max_extracted_bytes=64 * 1024,
            max_compression_ratio=100,
            max_asset_bytes=16 * 1024,
            max_assets=10,
            max_image_pixels=1_000_000,
        ),
    )


def test_healthz_uses_runtime_limits_and_has_no_pdf_engine(tmp_path: Path) -> None:
    with TestClient(create_app(_settings(tmp_path), pdf_engine=None)) as client:
        response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == make_health_document(
        service_version="unit-test",
        max_input_bytes=1024,
        max_output_bytes=64 * 1024,
    )
    assert response.json()["capabilities"]["pdf"]["enabled"] is False


def test_health_rejects_an_unapproved_half_enabled_pdf_shape() -> None:
    value = make_health_document(
        service_version="test", max_input_bytes=1, max_output_bytes=1
    )
    value["capabilities"]["pdf"]["enabled"] = True
    with pytest.raises(ProtocolError, match="exactly when PDF is enabled"):
        validate_health_document(value)


@pytest.mark.parametrize(
    ("engine", "engine_version"),
    [("approved-engine", ""), ("", "1.0.0"), ("approved-engine", "1.0.0")],
)
def test_disabled_pdf_health_rejects_any_engine_metadata(
    engine: str, engine_version: str
) -> None:
    value = make_health_document(
        service_version="test", max_input_bytes=1, max_output_bytes=1
    )
    value["capabilities"]["pdf"].update(
        {"engine": engine, "engineVersion": engine_version}
    )
    with pytest.raises(ProtocolError, match="exactly when PDF is enabled"):
        validate_health_document(value)
