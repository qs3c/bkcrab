from __future__ import annotations

import hashlib
import io
import json
import tarfile
import zipfile
from pathlib import Path

import pymupdf
import pytest
from fastapi.testclient import TestClient

from app.main import Settings, create_app
from app.protocol import (
    MIME_TYPES,
    RendererDescriptor,
    RendererError,
    RendererLimits,
    preflight_ooxml,
)
from tests.fixtures import generate_office_files


class FakeConverter:
    def __init__(self, pages: int = 8) -> None:
        self.pages = pages
        self.request_directories: list[Path] = []

    def __call__(
        self, source: Path, source_format: str, output_pdf: Path, profile: Path, _timeout: float
    ) -> None:
        assert source.suffix == f".{source_format}"
        profile.mkdir(mode=0o700)
        self.request_directories.append(source.parent)
        document = pymupdf.open()
        for index in range(self.pages):
            page = document.new_page(width=200, height=100)
            page.insert_text((12, 24), f"page {index + 1}")
        document.save(output_pdf)
        document.close()


def settings(temp_root: Path, **limit_overrides: int) -> Settings:
    limits = {
        "max_input_bytes": 8 * 1024 * 1024,
        "max_pages": 6,
        "render_dpi": 100,
        "max_archive_entries": 10_000,
        "max_zip_entry_bytes": 8 * 1024 * 1024,
        "max_extracted_bytes": 64 * 1024 * 1024,
        "max_compression_ratio": 200,
        "max_page_pixels": 1_000_000,
        "max_page_bytes": 2 * 1024 * 1024,
        "max_bundle_bytes": 16 * 1024 * 1024,
    }
    limits.update(limit_overrides)
    return Settings(
        descriptor=RendererDescriptor(
            service_version="test-renderer",
            libreoffice_version="LibreOffice test",
            pymupdf_version=str(pymupdf.VersionBind),
        ),
        limits=RendererLimits(**limits),
        timeout_seconds=3,
        temp_root=temp_root,
    )


@pytest.fixture
def office_files(tmp_path: Path) -> dict[str, Path]:
    return generate_office_files(tmp_path / "fixtures")


def upload(path: Path, media_type: str) -> dict[str, tuple[str, io.BytesIO, str]]:
    return {"file": (path.name, io.BytesIO(path.read_bytes()), media_type)}


def read_bundle(payload: bytes) -> tuple[dict[str, object], dict[str, bytes]]:
    with tarfile.open(fileobj=io.BytesIO(payload), mode="r:") as archive:
        members = archive.getmembers()
        assert members[0].name == "manifest.json"
        assert all(member.isfile() and member.linkname == "" for member in members)
        values = {member.name: archive.extractfile(member).read() for member in members}
    return json.loads(values.pop("manifest.json")), values


def test_health_is_closed_descriptor(tmp_path: Path) -> None:
    client = TestClient(create_app(settings(tmp_path), FakeConverter()))
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {
        "descriptor": {
            "protocolVersion": "parser-eval-renderer/v1",
            "serviceVersion": "test-renderer",
            "libreOfficeVersion": "LibreOffice test",
            "pyMuPDFVersion": str(pymupdf.VersionBind),
        },
        "formats": ["docx", "pptx", "xlsx"],
        "limits": {
            "maxInputBytes": 8 * 1024 * 1024,
            "maxPages": 6,
            "renderDPI": 100,
            "maxPagePixels": 1_000_000,
            "maxPageBytes": 2 * 1024 * 1024,
            "maxBundleBytes": 16 * 1024 * 1024,
        },
    }


@pytest.mark.parametrize("source_format", ["docx", "pptx", "xlsx"])
def test_render_first_six_pages_with_manifest(
    tmp_path: Path, office_files: dict[str, Path], source_format: str
) -> None:
    temp_root = tmp_path / "requests"
    temp_root.mkdir()
    converter = FakeConverter(pages=8)
    client = TestClient(create_app(settings(temp_root), converter))
    source = office_files[source_format]
    source_bytes = source.read_bytes()
    response = client.post(
        f"/v1/render?format={source_format}", files=upload(source, MIME_TYPES[source_format])
    )
    assert response.status_code == 200, response.text
    assert response.headers["content-type"] == "application/x-tar"
    manifest, entries = read_bundle(response.content)
    assert manifest["protocolVersion"] == "parser-eval-renderer/v1"
    assert manifest["source"] == {
        "format": source_format,
        "byteSize": len(source_bytes),
        "sha256": hashlib.sha256(source_bytes).hexdigest(),
    }
    assert manifest["totalPages"] == 8
    assert manifest["coveredPages"] == 6
    assert manifest["renderDurationMs"] >= 0
    assert list(entries) == [f"pages/page-{page:04d}.png" for page in range(1, 7)]
    for page, descriptor in enumerate(manifest["pages"], start=1):
        payload = entries[descriptor["path"]]
        assert descriptor["page"] == page
        assert descriptor["width"] > 0 and descriptor["height"] > 0
        assert descriptor["mediaType"] == "image/png"
        assert descriptor["byteSize"] == len(payload)
        assert descriptor["sha256"] == hashlib.sha256(payload).hexdigest()
        assert payload.startswith(b"\x89PNG\r\n\x1a\n")
    assert not list(temp_root.iterdir())


def test_each_request_uses_independent_cleaned_directory(
    tmp_path: Path, office_files: dict[str, Path]
) -> None:
    temp_root = tmp_path / "requests"
    temp_root.mkdir()
    converter = FakeConverter(1)
    client = TestClient(create_app(settings(temp_root), converter))
    source = office_files["docx"]
    for _ in range(2):
        response = client.post("/v1/render?format=docx", files=upload(source, MIME_TYPES["docx"]))
        assert response.status_code == 200
    assert len(set(converter.request_directories)) == 2
    assert all(not directory.exists() for directory in converter.request_directories)


@pytest.mark.parametrize(
    ("url", "files", "data", "code"),
    [
        ("/v1/render?format=pdf", {}, {}, "unsupported_format"),
        ("/v1/render?format=docx", {}, {}, "invalid_multipart"),
        (
            "/v1/render?format=docx",
            {"file": ("sample.pdf", io.BytesIO(b"bad"), MIME_TYPES["docx"])},
            {},
            "invalid_multipart",
        ),
        (
            "/v1/render?format=docx",
            {"file": ("sample.docx", io.BytesIO(b"bad"), "application/zip")},
            {},
            "invalid_multipart",
        ),
        (
            "/v1/render?format=docx",
            {"file": ("sample.docx", io.BytesIO(b"not a zip"), MIME_TYPES["docx"])},
            {},
            "invalid_ooxml",
        ),
    ],
)
def test_render_rejects_invalid_requests(
    tmp_path: Path,
    url: str,
    files: dict[str, tuple[str, io.BytesIO, str]],
    data: dict[str, str],
    code: str,
) -> None:
    client = TestClient(create_app(settings(tmp_path), FakeConverter()))
    response = client.post(url, files=files, data=data)
    assert response.json() == {"error": {"code": code}}


def test_render_accepts_only_one_file(tmp_path: Path, office_files: dict[str, Path]) -> None:
    source = office_files["docx"]
    duplicate_files = [
        ("file", (source.name, source.read_bytes(), MIME_TYPES["docx"])),
        ("file", (source.name, source.read_bytes(), MIME_TYPES["docx"])),
    ]
    client = TestClient(create_app(settings(tmp_path), FakeConverter()))
    response = client.post("/v1/render?format=docx", files=duplicate_files)
    assert response.json() == {"error": {"code": "invalid_multipart"}}


@pytest.mark.parametrize("case", ["traversal", "entry-limit", "ratio-limit", "truncated"])
def test_ooxml_preflight_rejects_unsafe_archives(tmp_path: Path, case: str) -> None:
    source = tmp_path / "unsafe.docx"
    limits = settings(tmp_path).limits
    if case == "truncated":
        source.write_bytes(b"PK\x03\x04truncated")
    else:
        entries = {
            "[Content_Types].xml": b"<Types/>",
            "word/document.xml": b"<document/>",
        }
        if case == "traversal":
            entries["../escape"] = b"bad"
        elif case == "entry-limit":
            entries["extra"] = b"x"
            limits = RendererLimits(**{**vars(limits), "max_archive_entries": 2})
        else:
            entries["compressed"] = b"x" * 4096
            limits = RendererLimits(**{**vars(limits), "max_compression_ratio": 2})
        with zipfile.ZipFile(source, "w", compression=zipfile.ZIP_DEFLATED) as archive:
            for name, payload in entries.items():
                archive.writestr(name, payload)
    with pytest.raises(RendererError, match="invalid_ooxml"):
        preflight_ooxml(source, "docx", limits)


def test_input_and_render_limits_are_closed_errors(
    tmp_path: Path, office_files: dict[str, Path]
) -> None:
    source = office_files["docx"]
    client = TestClient(
        create_app(
            settings(tmp_path, max_input_bytes=len(source.read_bytes()) - 1), FakeConverter()
        )
    )
    response = client.post("/v1/render?format=docx", files=upload(source, MIME_TYPES["docx"]))
    assert response.status_code == 413
    assert response.json() == {"error": {"code": "input_too_large"}}

    client = TestClient(create_app(settings(tmp_path, max_page_pixels=10), FakeConverter()))
    response = client.post("/v1/render?format=docx", files=upload(source, MIME_TYPES["docx"]))
    assert response.status_code == 422
    assert response.json() == {"error": {"code": "render_limit_exceeded"}}
    assert not list(tmp_path.iterdir()) or all(
        path.name == "fixtures" for path in tmp_path.iterdir()
    )


@pytest.mark.parametrize(
    ("failure", "status"),
    [
        (RendererError("render_timeout", 504), 504),
        (RendererError("libreoffice_failed", 502), 502),
    ],
)
def test_converter_failures_are_closed_and_cleaned(
    tmp_path: Path,
    office_files: dict[str, Path],
    failure: RendererError,
    status: int,
) -> None:
    temp_root = tmp_path / "requests"
    temp_root.mkdir()

    def fail(*_args: object) -> None:
        raise failure

    client = TestClient(create_app(settings(temp_root), fail))
    source = office_files["docx"]
    response = client.post("/v1/render?format=docx", files=upload(source, MIME_TYPES["docx"]))
    assert response.status_code == status
    assert response.json() == {"error": {"code": failure.code}}
    assert not list(temp_root.iterdir())


def test_invalid_pdf_is_closed_and_cleaned(tmp_path: Path, office_files: dict[str, Path]) -> None:
    temp_root = tmp_path / "requests"
    temp_root.mkdir()

    def corrupt(_source: Path, _format: str, output: Path, profile: Path, _timeout: float) -> None:
        profile.mkdir()
        output.write_bytes(b"not pdf")

    client = TestClient(create_app(settings(temp_root), corrupt))
    source = office_files["docx"]
    response = client.post("/v1/render?format=docx", files=upload(source, MIME_TYPES["docx"]))
    assert response.status_code == 502
    assert response.json() == {"error": {"code": "invalid_pdf"}}
    assert not list(temp_root.iterdir())
