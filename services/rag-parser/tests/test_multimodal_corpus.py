from __future__ import annotations

import hashlib
import json
import zipfile
from pathlib import Path

import pytest

from app.office import OfficeError, OfficeLimits, preflight_ooxml
from app.pdf import PDFLimits, build_pdf_analyze_bundle
from app.pdf_engine import PDFPageAnalysis, PDFPageRender, Pypdfium2PDFEngine
from tests.fixtures.generate_multimodal import generate_all

FIXTURE_ROOT = Path(__file__).resolve().parent / "fixtures"
EXPECTED = json.loads((FIXTURE_ROOT / "expected_multimodal.json").read_text(encoding="utf-8"))


def _office_limits() -> OfficeLimits:
    return OfficeLimits(
        max_archive_entries=10_000,
        max_zip_entry_bytes=8 * 1024 * 1024,
        max_extracted_bytes=64 * 1024 * 1024,
        max_compression_ratio=200,
        max_asset_bytes=2 * 1024 * 1024,
        max_assets=100,
        max_image_pixels=1_000_000,
    )


def test_multimodal_generator_emits_the_declared_legal_corpus(tmp_path: Path) -> None:
    paths = generate_all(tmp_path / "corpus")
    assert sorted(paths) == sorted(EXPECTED["generatedFiles"])
    assert all(path.is_file() for path in paths.values())
    assert EXPECTED["license"] == "CC0-1.0"


def test_multimodal_pdf_golden_has_expected_pages_signals_and_asset(tmp_path: Path) -> None:
    paths = generate_all(tmp_path / "corpus")
    engine = Pypdfium2PDFEngine()
    analyzed = engine.analyze(paths["multimodal-golden.pdf"], max_pages=10, cancelled=lambda: False)
    expected = EXPECTED["pdf"]
    assert len(analyzed) == expected["pageCount"]
    assert all(isinstance(page, PDFPageAnalysis) for page in analyzed)
    pages = [page for page in analyzed if isinstance(page, PDFPageAnalysis)]
    for page, wanted in zip(pages, expected["pages"], strict=True):
        assert page.page == wanted["page"]
        assert all(text in page.native_markdown for text in wanted["requiredText"])
        assert page.signals.table is wanted["signals"]["table"]
        assert page.signals.code is wanted["signals"]["code"]
        assert page.signals.scanned is wanted["signals"]["scanned"]
        assert len(page.embedded_images) == wanted["embeddedImageCount"]

    request_dir = tmp_path / "analyze-bundle"
    request_dir.mkdir()
    source = paths["multimodal-golden.pdf"]
    bundle = build_pdf_analyze_bundle(
        source=source,
        source_sha256=hashlib.sha256(source.read_bytes()).hexdigest(),
        source_size=source.stat().st_size,
        request_dir=request_dir,
        engine=engine,
        limits=PDFLimits(max_pages=10),
        cancelled=lambda: False,
    )
    try:
        manifest = bundle.manifest
        assert [f"page:{page.page}" for page in manifest.pages] == expected["unitLocations"]
        assert [warning.code for warning in manifest.warnings] == expected["warningCodes"]
        assert any(warning.degraded for warning in manifest.warnings) is expected["degraded"]
    finally:
        bundle.cleanup()

    rendered = engine.render(
        paths["multimodal-golden.pdf"],
        (2,),
        tmp_path / "rendered",
        max_pages=10,
        dpi=144,
        max_image_pixels=2_000_000,
        max_assets=10,
        max_asset_bytes=32 * 1024,
        cancelled=lambda: False,
    )
    assert len(rendered) == 1 and isinstance(rendered[0], PDFPageRender)
    assert len(rendered[0].embedded_images) == expected["renderPage2AssetCount"]


@pytest.mark.parametrize(
    ("fixture_name", "expected_action"),
    [
        ("adversarial-dtd.docx", "reject"),
        ("adversarial-external-relationship.docx", "reject"),
        ("adversarial-local-absolute.docx", "reject"),
        ("adversarial-local-hyperlink.docx", "strip"),
    ],
)
def test_multimodal_ooxml_adversarial_corpus_never_reads_or_returns_local_paths(
    fixture_name: str, expected_action: str, tmp_path: Path
) -> None:
    paths = generate_all(tmp_path / "corpus")
    request_dir = tmp_path / ("request-" + fixture_name)
    request_dir.mkdir(mode=0o700)
    if expected_action == "reject":
        with pytest.raises(OfficeError):
            preflight_ooxml(paths[fixture_name], "docx", request_dir, _office_limits())
        return

    result = preflight_ooxml(paths[fixture_name], "docx", request_dir, _office_limits())
    assert {warning.code for warning in result.warnings} == {"office_external_hyperlink_removed"}
    with zipfile.ZipFile(result.sanitized_path) as archive:
        relationship_bytes = b"".join(
            archive.read(name) for name in archive.namelist() if name.endswith(".rels")
        )
    assert b"BKCRAB_LOCAL_SENTINEL" not in relationship_bytes
    assert b"file://" not in relationship_bytes


def test_multimodal_adversarial_json_covers_prompt_depth_and_size(tmp_path: Path) -> None:
    paths = generate_all(tmp_path / "corpus")
    security = EXPECTED["documentAI"]
    page = json.loads(paths["page-transcription-injection.json"].read_text(encoding="utf-8"))
    image = json.loads(paths["image-description-injection.json"].read_text(encoding="utf-8"))
    assert security["promptInjection"] in page["markdown"]
    assert security["metadataInjection"] in page["visuals"][0]["caption"]
    assert security["toolInjection"] in image["caption"]
    assert (
        paths["deep-document-ai.json"].read_text(encoding="utf-8").count("[")
        > security["maxAcceptedDepth"]
    )
    assert paths["oversized-document-ai.json"].stat().st_size > security["maxAcceptedCaptionBytes"]
