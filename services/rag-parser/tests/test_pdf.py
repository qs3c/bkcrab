from __future__ import annotations

import io
import json
import logging
import tarfile
import time
from collections.abc import Callable, Sequence
from pathlib import Path

import pypdfium2 as pdfium
import pytest
from fastapi.testclient import TestClient
from PIL import Image

from app.main import Settings, create_app
from app.office import OfficeLimits
from app.pdf import PDFLimits
from app.pdf_engine import (
    PDFEmbeddedImage,
    PDFEngineCancelled,
    PDFPageAnalysis,
    PDFPageFailure,
    PDFPageRender,
    PDFRenderedImage,
    PDFSignals,
    PDFTextBlock,
    Pypdfium2PDFEngine,
)
from app.protocol import Manifest, validate_page_primitive_document


def _pdf_bytes(texts: Sequence[str] = ("Hello PDFium",)) -> bytes:
    """Generate a tiny, deterministic, dependency-free PDF fixture."""

    objects: list[bytes] = []
    page_ids: list[int] = []
    content_ids: list[int] = []
    font_id = 3 + len(texts) * 2
    for index, text in enumerate(texts):
        page_ids.append(3 + index * 2)
        content_ids.append(4 + index * 2)
        escaped = text.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")
        stream = f"BT /F1 12 Tf 20 100 Td ({escaped}) Tj ET".encode()
        objects.extend(
            [
                (
                    f"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] "
                    f"/Resources << /Font << /F1 {font_id} 0 R >> >> "
                    f"/Contents {content_ids[-1]} 0 R >>"
                ).encode(),
                b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
            ]
        )
    pages = " ".join(f"{page_id} 0 R" for page_id in page_ids)
    all_objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        f"<< /Type /Pages /Kids [{pages}] /Count {len(page_ids)} >>".encode(),
        *objects,
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    result = bytearray(b"%PDF-1.4\n%\xe2\xe3\xcf\xd3\n")
    offsets = [0]
    for object_id, value in enumerate(all_objects, 1):
        offsets.append(len(result))
        result.extend(f"{object_id} 0 obj\n".encode())
        result.extend(value)
        result.extend(b"\nendobj\n")
    xref = len(result)
    result.extend(f"xref\n0 {len(all_objects) + 1}\n".encode())
    result.extend(b"0000000000 65535 f \n")
    for offset in offsets[1:]:
        result.extend(f"{offset:010d} 00000 n \n".encode())
    result.extend(
        (
            f"trailer\n<< /Size {len(all_objects) + 1} /Root 1 0 R >>\n"
            f"startxref\n{xref}\n%%EOF\n"
        ).encode()
    )
    return bytes(result)


def _settings(temp_root: Path, **overrides: int) -> Settings:
    pdf_limits = PDFLimits(
        max_pages=overrides.get("max_pages", 10),
        render_dpi=overrides.get("render_dpi", 144),
        max_image_pixels=overrides.get("max_image_pixels", 2_000_000),
        max_assets=10,
        max_asset_bytes=32 * 1024,
    )
    return Settings(
        service_version="pdf-test",
        max_input_bytes=overrides.get("max_input_bytes", 1024 * 1024),
        max_output_bytes=overrides.get("max_output_bytes", 1024 * 1024),
        max_entry_bytes=overrides.get("max_entry_bytes", 256 * 1024),
        max_bundle_entries=50,
        parse_timeout_seconds=overrides.get("parse_timeout_seconds", 10),
        temp_root=temp_root,
        office_limits=OfficeLimits(
            max_archive_entries=100,
            max_zip_entry_bytes=1024 * 1024,
            max_extracted_bytes=1024 * 1024,
            max_compression_ratio=100,
            max_asset_bytes=32 * 1024,
            max_assets=10,
            max_image_pixels=2_000_000,
        ),
        pdf_limits=pdf_limits,
    )


def _png(path: Path, size: tuple[int, int], color: str) -> None:
    Image.new("RGB", size, color=color).save(path, format="PNG")


class _FakePDFEngine:
    name = "fake-pdf-engine"
    version = "1.2.3"

    def __init__(self) -> None:
        self.analyze_calls = 0
        self.render_calls: list[tuple[int, ...]] = []

    def analyze(
        self,
        _source: Path,
        *,
        max_pages: int,
        cancelled: Callable[[], bool],
    ) -> tuple[PDFPageAnalysis | PDFPageFailure, ...]:
        assert max_pages >= 2
        assert not cancelled()
        self.analyze_calls += 1
        return (
            PDFPageAnalysis(
                page=1,
                width=200,
                height=100,
                native_markdown="TOP SECRET native text\n",
                text_blocks=(PDFTextBlock("TOP SECRET native text", (100, 100, 900, 300)),),
                embedded_images=(PDFEmbeddedImage((200, 400, 800, 900)),),
                signals=PDFSignals(
                    table=False,
                    code=False,
                    scanned=False,
                    multicolumn=False,
                    reading_order_uncertain=False,
                ),
            ),
            PDFPageFailure(2, "page_analyze_failed"),
        )

    def render(
        self,
        _source: Path,
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
        assert max_pages >= 2 and dpi == 144
        assert max_image_pixels > 0 and max_assets > 0 and max_asset_bytes > 0
        assert not cancelled()
        self.render_calls.append(pages)
        output_dir.mkdir(parents=True, exist_ok=True)
        values: list[PDFPageRender | PDFPageFailure] = []
        for page in pages:
            if page == 2:
                values.append(PDFPageFailure(page, "page_render_failed"))
                continue
            render_path = output_dir / f"render-{page}.png"
            image_path = output_dir / f"image-{page}.png"
            _png(render_path, (400, 200), "white")
            _png(image_path, (20, 10), "blue")
            values.append(
                PDFPageRender(
                    page=page,
                    render_path=render_path,
                    width=400,
                    height=200,
                    embedded_images=(
                        PDFRenderedImage(image_path, 20, 10, (200, 400, 800, 900)),
                    ),
                )
            )
        return tuple(values)


def _bundle(response) -> tuple[list[str], Manifest, dict[str, bytes]]:
    assert response.status_code == 200, response.text
    assert response.headers["content-type"] == "application/x-tar"
    with tarfile.open(fileobj=io.BytesIO(response.content), mode="r:") as archive:
        names = [item.name for item in archive.getmembers()]
        payloads = {
            item.name: archive.extractfile(item).read()
            for item in archive.getmembers()
            if item.name != "manifest.json"
        }
        manifest = Manifest.from_dict(json.load(archive.extractfile("manifest.json")))
    return names, manifest, payloads


def test_health_advertises_the_approved_engine(tmp_path: Path) -> None:
    engine = _FakePDFEngine()
    with TestClient(create_app(_settings(tmp_path), pdf_engine=engine)) as client:
        response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json()["capabilities"]["pdf"] == {
        "enabled": True,
        "engine": "fake-pdf-engine",
        "engineVersion": "1.2.3",
    }


def test_default_app_enables_the_approved_pypdfium2_adapter(tmp_path: Path) -> None:
    with TestClient(create_app(_settings(tmp_path))) as client:
        pdf = client.get("/healthz").json()["capabilities"]["pdf"]
    assert pdf == {
        "enabled": True,
        "engine": "pypdfium2",
        "engineVersion": "5.12.1",
    }


def test_analyze_returns_native_and_primitive_without_page_renders(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.INFO, logger="rag-parser")
    engine = _FakePDFEngine()
    with TestClient(create_app(_settings(tmp_path), pdf_engine=engine)) as client:
        response = client.post(
            "/v1/pdf/analyze",
            files={"file": ("source.pdf", _pdf_bytes(), "application/pdf")},
        )
    names, manifest, payloads = _bundle(response)
    assert names[0] == "manifest.json"
    assert names[1:] == sorted(names[1:])
    assert not any(name.endswith(".png") for name in names)
    assert manifest.bundle_kind == "pdf-analyze"
    assert [page.status for page in manifest.pages] == ["ok", "failed"]
    assert manifest.pages[1].error_code == "page_analyze_failed"
    primitive = json.loads(payloads[manifest.pages[0].primitive_entry])
    assert validate_page_primitive_document(primitive) == primitive
    assert primitive["textChars"] == len("TOPSECRETnativetext")
    assert b"TOP SECRET native text" in payloads[manifest.pages[0].native_markdown_entry]
    assert "TOP SECRET" not in caplog.text
    assert list(tmp_path.iterdir()) == []


def test_render_uses_exact_allowlist_and_keeps_failed_page_explicit(tmp_path: Path) -> None:
    engine = _FakePDFEngine()
    with TestClient(create_app(_settings(tmp_path), pdf_engine=engine)) as client:
        response = client.post(
            "/v1/pdf/render?pages=2,1",
            files={"file": ("source.pdf", _pdf_bytes(), "application/pdf")},
        )
    names, manifest, payloads = _bundle(response)
    assert engine.render_calls == [(1, 2)]
    assert [page.page for page in manifest.pages] == [1, 2]
    assert [page.status for page in manifest.pages] == ["ok", "failed"]
    assert manifest.pages[0].render_entry in payloads
    assert manifest.pages[1].render_entry == ""
    assert len(manifest.assets) == len(manifest.occurrences) == 1
    assert manifest.occurrences[0].unit_id == manifest.pages[0].unit_id
    assert manifest.occurrences[0].location.index == 1
    assert names[1:] == sorted(names[1:])
    assert list(tmp_path.iterdir()) == []


@pytest.mark.parametrize("pages", ["", "0", "-1", "1,1", "1,x", "1,,2"])
def test_render_rejects_noncanonical_page_allowlists(tmp_path: Path, pages: str) -> None:
    with TestClient(create_app(_settings(tmp_path), pdf_engine=_FakePDFEngine())) as client:
        response = client.post(
            f"/v1/pdf/render?pages={pages}",
            files={"file": ("source.pdf", _pdf_bytes(), "application/pdf")},
        )
    assert response.status_code == 400
    assert list(tmp_path.iterdir()) == []


@pytest.mark.parametrize(
    ("filename", "content_type", "value"),
    [
        ("source.txt", "application/pdf", _pdf_bytes()),
        ("source.pdf", "text/plain", _pdf_bytes()),
        ("source.pdf", "application/pdf", b"not-pdf"),
    ],
)
def test_pdf_endpoint_validates_extension_mime_and_magic(
    tmp_path: Path, filename: str, content_type: str, value: bytes
) -> None:
    with TestClient(create_app(_settings(tmp_path), pdf_engine=_FakePDFEngine())) as client:
        response = client.post(
            "/v1/pdf/analyze", files={"file": (filename, value, content_type)}
        )
    assert response.status_code == 400
    assert list(tmp_path.iterdir()) == []


def test_pypdfium2_adapter_analyzes_and_renders_a_real_minimal_pdf(tmp_path: Path) -> None:
    source = tmp_path / "minimal.pdf"
    source.write_bytes(_pdf_bytes())
    engine = Pypdfium2PDFEngine()
    analyzed = engine.analyze(source, max_pages=10, cancelled=lambda: False)
    assert len(analyzed) == 1
    assert isinstance(analyzed[0], PDFPageAnalysis)
    assert "Hello PDFium" in analyzed[0].native_markdown
    rendered = engine.render(
        source,
        (1,),
        tmp_path / "rendered",
        max_pages=10,
        dpi=144,
        max_image_pixels=2_000_000,
        max_assets=10,
        max_asset_bytes=32 * 1024,
        cancelled=lambda: False,
    )
    assert len(rendered) == 1 and isinstance(rendered[0], PDFPageRender)
    with Image.open(rendered[0].render_path) as image:
        assert image.size == (400, 400)
        assert image.format == "PNG"


def test_real_engine_enforces_page_and_pixel_limits(tmp_path: Path) -> None:
    source = tmp_path / "two-pages.pdf"
    source.write_bytes(_pdf_bytes(("one", "two")))
    engine = Pypdfium2PDFEngine()
    with pytest.raises(ValueError, match="page limit"):
        engine.analyze(source, max_pages=1, cancelled=lambda: False)
    rendered = engine.render(
        source,
        (1,),
        tmp_path / "pixels",
        max_pages=10,
        dpi=144,
        max_image_pixels=100,
        max_assets=1,
        max_asset_bytes=1024,
        cancelled=lambda: False,
    )
    assert isinstance(rendered[0], PDFPageFailure)
    assert rendered[0].error_code == "page_render_failed"


def test_real_engine_finds_and_safely_extracts_an_embedded_image(tmp_path: Path) -> None:
    jpeg = tmp_path / "source.jpg"
    Image.new("RGB", (16, 8), color="green").save(jpeg, format="JPEG")
    source = tmp_path / "embedded.pdf"
    document = pdfium.PdfDocument.new()
    page = document.new_page(200, 200)
    image_object = pdfium.PdfImage.new(document)
    try:
        image_object.load_jpeg(jpeg)
        image_object.set_matrix(pdfium.PdfMatrix(80, 0, 0, 40, 20, 80))
        page.insert_obj(image_object)
        page.gen_content()
        document.save(source, version=17)
    finally:
        page.close()
        document.close()

    engine = Pypdfium2PDFEngine()
    analyzed = engine.analyze(source, max_pages=10, cancelled=lambda: False)
    assert isinstance(analyzed[0], PDFPageAnalysis)
    assert len(analyzed[0].embedded_images) == 1
    rendered = engine.render(
        source,
        (1,),
        tmp_path / "embedded-output",
        max_pages=10,
        dpi=144,
        max_image_pixels=2_000_000,
        max_assets=10,
        max_asset_bytes=32 * 1024,
        cancelled=lambda: False,
    )
    assert isinstance(rendered[0], PDFPageRender)
    assert len(rendered[0].embedded_images) == 1
    with Image.open(rendered[0].embedded_images[0].path) as extracted:
        assert extracted.format == "PNG"
        assert extracted.width > 0 and extracted.height > 0


class _CancellableEngine(_FakePDFEngine):
    def analyze(self, *args, cancelled: Callable[[], bool], **kwargs):
        while not cancelled():
            time.sleep(0.01)
        raise PDFEngineCancelled


def test_timeout_signals_engine_and_cleans_the_request_directory(tmp_path: Path) -> None:
    with TestClient(
        create_app(
            _settings(tmp_path, parse_timeout_seconds=1),
            pdf_engine=_CancellableEngine(),
        )
    ) as client:
        response = client.post(
            "/v1/pdf/analyze",
            files={"file": ("source.pdf", _pdf_bytes(), "application/pdf")},
        )
    assert response.status_code == 504
    assert response.json()["error"]["code"] == "parse_timeout"
    assert list(tmp_path.iterdir()) == []


def test_output_quota_is_checked_before_streaming_headers(tmp_path: Path) -> None:
    settings = _settings(tmp_path, max_output_bytes=1024, max_entry_bytes=512)
    with TestClient(create_app(settings, pdf_engine=_FakePDFEngine())) as client:
        response = client.post(
            "/v1/pdf/render?pages=1",
            files={"file": ("source.pdf", _pdf_bytes(), "application/pdf")},
        )
    assert response.status_code == 422
    assert response.json()["error"]["code"] in {"entry_too_large", "bundle_too_large"}
    assert list(tmp_path.iterdir()) == []
