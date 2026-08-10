from dataclasses import replace

from fastapi.testclient import TestClient

from app.main import create_app
from app.metrics import MetricEngine
from app.settings import Settings


def test_health_contract_reports_all_compatibility_versions():
    settings = replace(Settings.from_env(), api_key="")
    response = TestClient(create_app(settings, MetricEngine())).get("/healthz")
    assert response.status_code == 200
    assert response.json() == {
        "ok": True,
        "serviceVersion": settings.service_version,
        "protocolVersion": "rag-evaluator-v1",
        "ragasVersion": "0.3.9",
        "metricBundleVersion": "rag-core-v1",
        "judgeConfigured": settings.judge_configured,
        "metricsInitialized": True,
    }
