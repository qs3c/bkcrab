import asyncio
from dataclasses import replace
from unittest.mock import AsyncMock

import httpx
import pytest
from fastapi.testclient import TestClient
from openai import AsyncOpenAI
from ragas.llms.base import llm_factory
from ragas.metrics.collections._factual_correctness import ClaimDecompositionOutput
from ragas.metrics.collections._faithfulness import StatementGeneratorOutput

from app.jev import JevJudge, decision_audit_scope, hybrid_metrics
from app.main import create_app
from app.metrics import MetricEngine
from app.settings import Settings
from app.usage import usage_scope


def settings(**kwargs):
    return replace(
        Settings.from_env(),
        metric_bundle_version="rag-jev-hybrid-v1",
        api_key="internal",
        jev_endpoint="http://main/internal/rag-eval/jev",
        **kwargs,
    )


def make_metrics(judge):
    llm = llm_factory("offline", provider="openai", client=AsyncOpenAI(api_key="offline"))
    original = object()
    metrics = hybrid_metrics({"response_relevancy": original}, judge, llm, "rag-jev-hybrid-v1")
    assert metrics["response_relevancy"] is original
    return llm, metrics


def test_hybrid_preserves_ap_and_claim_formulas():
    judge = AsyncMock()
    llm, metrics = make_metrics(judge)
    judge.verdicts.return_value = [1, 0, 1]
    result = asyncio.run(metrics["context_precision"].ascore("q", "ref", ["a", "b", "c"]))
    assert result.value == pytest.approx((1 + 2 / 3) / 2)
    llm.agenerate = AsyncMock(
        return_value=StatementGeneratorOutput(statements=["uses Redis", "TTL 60s"])
    )
    judge.verdicts.return_value = [1, 0]
    faithful = asyncio.run(metrics["faithfulness"].ascore("q", "a", ["uses Redis"]))
    assert faithful.value == 0.5
    recall = asyncio.run(metrics["context_recall"].ascore("q", ["uses Redis"], "ref"))
    assert recall.value == 0.5
    llm.agenerate = AsyncMock(
        side_effect=[
            ClaimDecompositionOutput(claims=["a", "b"]),
            ClaimDecompositionOutput(claims=["a"]),
        ]
    )
    judge.verdicts.side_effect = [[1, 0], [1]]
    correct = asyncio.run(metrics["factual_correctness"].ascore("a and b", "a"))
    assert correct.value == 0.67  # Ragas rounds F1 to two decimals.
    assert llm.agenerate.call_count == 2


def test_empty_extraction_is_error_not_zero():
    llm, metrics = make_metrics(AsyncMock())
    llm.agenerate = AsyncMock(return_value=ClaimDecompositionOutput(claims=[]))
    with pytest.raises(ValueError, match="no claims"):
        asyncio.run(metrics["factual_correctness"].ascore("answer", "ref"))


def install_proxy(monkeypatch, body):
    captured = []

    class Client:
        def __init__(self, **kwargs):
            assert kwargs["follow_redirects"] is False

        async def __aenter__(self):
            return self

        async def __aexit__(self, *args):
            pass

        async def post(self, endpoint, **kwargs):
            captured.append(kwargs)
            return httpx.Response(200, json=body, request=httpx.Request("POST", endpoint))

    monkeypatch.setattr("app.jev.httpx.AsyncClient", Client)
    return captured


def proxy_response(**kwargs):
    return {
        "rubric": "jev-eval-v1",
        "result": {
            "model": "jev-1.13.0",
            "probabilities": {"c0": 0.9, "c1": 0.1},
            "usage": {
                "inputTokens": 100,
                "outputTokens": 2,
                "estimatedCostUsd": 0.001,
                "attempts": 2,
                "unknown": True,
            },
        },
        **kwargs,
    }


def test_proxy_records_usage_even_when_metric_fails(monkeypatch):
    captured = install_proxy(monkeypatch, proxy_response(error="jev_judge_failed"))
    cfg = settings()
    judge = JevJudge(cfg, lambda: "admin")
    with (
        usage_scope() as meter,
        decision_audit_scope() as audits,
        pytest.raises(RuntimeError, match="unavailable"),
    ):
        asyncio.run(judge.verdicts("support", contexts=["ctx"], claims=["a", "b"]))
    usage = meter.response(cfg)
    assert usage.jevInputTokens == 100 and usage.jevEstimatedCostUsd == 0.001
    assert usage.jevUsageUnknown and audits[0]["usageUnknown"]
    assert captured[0]["headers"]["X-BkCrab-Eval-Owner"] == "admin"
    assert "instructions" not in captured[0]["json"]


@pytest.mark.parametrize(
    "probabilities", [{"c0": 0.9}, {"c0": True, "c1": 0.1}, {"c0": 2, "c1": 0.1}]
)
def test_proxy_rejects_invalid_verdicts(monkeypatch, probabilities):
    body = proxy_response()
    body["result"]["probabilities"] = probabilities
    install_proxy(monkeypatch, body)
    with pytest.raises(RuntimeError):
        asyncio.run(
            JevJudge(settings(), lambda: "admin").verdicts(
                "support", contexts=["ctx"], claims=["a", "b"]
            )
        )


def test_proxy_retains_probabilities_and_threshold(monkeypatch):
    install_proxy(monkeypatch, proxy_response())
    with decision_audit_scope() as audit:
        verdicts = asyncio.run(
            JevJudge(settings(), lambda: "admin").verdicts(
                "support", contexts=["ctx"], claims=["a", "b"]
            )
        )
    assert verdicts == [1, 0]
    assert audit[0]["probabilities"] == [0.9, 0.1]
    assert audit[0]["threshold"] == 0.5


def test_bundle_isolation_precedes_scoring():
    calls = []

    async def score(metric, sample):
        calls.append(metric)
        return 1.0

    cfg = settings()
    client = TestClient(create_app(cfg, MetricEngine(score)))
    payload = {
        "requestId": "test",
        "metricBundleVersion": "rag-core-v1",
        "metrics": ["faithfulness"],
        "samples": [
            {
                "caseId": "1",
                "userInput": "q",
                "response": "a",
                "retrievedContexts": ["c"],
                "retrievedContextIds": ["1"],
            }
        ],
    }
    headers = {"Authorization": "Bearer internal", "X-BkCrab-Eval-Owner": "admin"}
    assert client.post("/v1/evaluate", json=payload, headers=headers).status_code == 409
    payload["metricBundleVersion"] = cfg.metric_bundle_version
    assert client.post("/v1/evaluate", json=payload, headers=headers).status_code == 409
    payload["jevModel"] = cfg.jev_model
    response = client.post("/v1/evaluate", json=payload, headers=headers)
    assert response.status_code == 200
    assert response.json()["metricBundleVersion"] == cfg.metric_bundle_version
    assert calls == ["faithfulness"]


def test_production_precision_engine_needs_no_generative_call(monkeypatch):
    from app.metrics import build_ragas_engine, judge_owner_scope
    from app.protocol import Sample

    cfg = settings(
        llm_endpoint="http://offline",
        llm_api_key="offline",
        llm_model="offline",
        embedding_endpoint="http://offline",
        embedding_api_key="offline",
        embedding_model="offline",
        llm_input_cost_per_million_usd=1,
        llm_output_cost_per_million_usd=1,
        embedding_cost_per_million_usd=1,
    )
    verdicts = AsyncMock(return_value=[1, 0])
    monkeypatch.setattr(JevJudge, "verdicts", verdicts)
    engine = build_ragas_engine(cfg)
    sample = Sample(
        caseId="1",
        userInput="q",
        reference="r",
        retrievedContexts=["a", "b"],
        retrievedContextIds=["1", "2"],
    )
    with judge_owner_scope("admin"):
        result = asyncio.run(engine.evaluate("context_precision", sample))
    assert result.status == "ok" and result.value == pytest.approx(1)
    assert verdicts.call_count == 1


def test_legacy_usage_wire_has_no_new_fields():
    from app.protocol import EvaluationUsage

    usage = EvaluationUsage(llmInputTokens=5).model_dump()
    assert usage == {
        "llmInputTokens": 5,
        "llmOutputTokens": 0,
        "llmEstimatedCostUsd": 0,
        "embeddingInputTokens": 0,
        "embeddingEstimatedCostUsd": 0,
    }
