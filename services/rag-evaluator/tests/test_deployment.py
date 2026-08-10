from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]


def evaluator_service() -> str:
    compose = (ROOT / "deploy/docker/docker-compose.rag.yml").read_text(encoding="utf-8")
    return compose.split("  rag-evaluator:\n", 1)[1].split("\n  milvus-etcd:\n", 1)[0]


def test_evaluator_compose_is_internal_and_hardened() -> None:
    service = evaluator_service()
    assert "\n    ports:" not in service
    assert "read_only: true" in service
    assert 'user: "65532:65532"' in service
    assert "no-new-privileges:true" in service
    assert "cap_drop: [ALL]" in service
    for limit in ("tmpfs:", "pids_limit:", "cpus:", "mem_limit:"):
        assert limit in service
    assert "networks: [rag-evaluator-internal]" in service


def test_evaluator_receives_no_production_data_plane_credentials() -> None:
    service = evaluator_service()
    for forbidden in (
        "MILVUS_",
        "MINIO_",
        "S3_",
        "DATABASE_",
        "BKCRAB_RAG_EMBEDDING_API_KEY",
    ):
        assert forbidden not in service


def test_image_is_locked_non_root_without_office_dependencies() -> None:
    dockerfile = (ROOT / "services/rag-evaluator/Dockerfile").read_text(encoding="utf-8")
    assert "python:3.12.13-slim-bookworm" in dockerfile
    assert "uv sync --frozen" in dockerfile
    assert "USER 65532:65532" in dockerfile
    assert "GIT_PYTHON_REFRESH=quiet" in dockerfile
    assert "libreoffice" not in dockerfile.lower()
