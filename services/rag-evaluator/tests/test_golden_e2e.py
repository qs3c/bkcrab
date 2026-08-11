import json
from dataclasses import replace
from pathlib import Path

from fastapi.testclient import TestClient

from app.main import create_app
from app.metrics import MetricEngine
from app.settings import Settings

ROOT = Path(__file__).resolve().parents[3]


def test_phase_h_shared_golden_bundle_evaluator_protocol_e2e():
    bundle = json.loads((ROOT / "testdata/rag-eval/e2e_golden.json").read_text(encoding="utf-8"))

    async def score(_metric, _sample):
        return bundle["expected"]["candidateScore"]

    settings = replace(Settings.from_env(), api_key="")
    response = TestClient(create_app(settings, MetricEngine(score))).post(
        "/v1/evaluate", json=bundle["evaluatorRequest"]
    )
    assert response.status_code == 200
    body = response.json()
    assert body["requestId"] == bundle["evaluatorRequest"]["requestId"]
    assert body["ragasVersion"] == "0.3.9"
    assert body["metricBundleVersion"] == "rag-core-v1"
    assert set(body["results"][0]["metrics"]) == set(bundle["evaluatorRequest"]["metrics"])
    assert all(
        metric == {"status": "ok", "value": bundle["expected"]["candidateScore"], "reason": ""}
        for metric in body["results"][0]["metrics"].values()
    )
