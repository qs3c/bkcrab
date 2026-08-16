import asyncio
from dataclasses import replace

from fastapi.testclient import TestClient

from app.main import create_app
from app.metrics import MetricEngine
from app.settings import Settings

OWNER_HEADERS = {"X-BkCrab-Eval-Owner": "admin"}


def settings(**overrides):
    values = {"api_key": "", **overrides}
    return replace(Settings.from_env(), **values)


def payload(request_id="evb_test"):
    return {
        "requestId": request_id,
        "metricBundleVersion": "rag-core-v1",
        "metrics": ["faithfulness", "response_relevancy"],
        "samples": [
            {
                "caseId": "case-1",
                "userInput": "question",
                "retrievedContexts": ["context"],
                "retrievedContextIds": ["ctx-1"],
                "response": "answer",
                "reference": "reference",
            }
        ],
    }


def test_request_id_body_hash_idempotency_and_conflict():
    calls = 0

    async def score(_metric, _sample):
        nonlocal calls
        calls += 1
        return 0.8

    client = TestClient(create_app(settings(), MetricEngine(score)))
    first = client.post("/v1/evaluate", json=payload(), headers=OWNER_HEADERS)
    replay = client.post("/v1/evaluate", json=payload(), headers=OWNER_HEADERS)
    changed = payload()
    changed["samples"][0]["response"] = "different"
    conflict = client.post("/v1/evaluate", json=changed, headers=OWNER_HEADERS)
    assert first.status_code == replay.status_code == 200
    assert first.json() == replay.json()
    assert calls == 2
    assert conflict.status_code == 409


def test_batch_body_context_and_total_context_limits():
    client = TestClient(create_app(settings(max_request_bytes=500), MetricEngine()))
    too_large = payload("evb_body")
    too_large["samples"][0]["response"] = "x" * 1_000
    assert client.post("/v1/evaluate", json=too_large, headers=OWNER_HEADERS).status_code == 413

    batch_client = TestClient(create_app(settings(max_request_bytes=100_000), MetricEngine()))
    batch = payload("evb_batch")
    batch["samples"] *= 17
    assert batch_client.post("/v1/evaluate", json=batch, headers=OWNER_HEADERS).status_code == 422

    total = payload("evb_total")
    total["samples"][0]["retrievedContexts"] = ["x" * 60, "y" * 60]
    total["samples"][0]["retrievedContextIds"] = ["one", "two"]
    limited = TestClient(
        create_app(settings(max_request_bytes=10_000, max_total_context_bytes=100), MetricEngine())
    )
    assert limited.post("/v1/evaluate", json=total, headers=OWNER_HEADERS).status_code == 422


def test_authorization_and_metric_partial_failure():
    async def score(metric, _sample):
        if metric == "faithfulness":
            raise RuntimeError("faithfulness unavailable")
        return 0.9

    client = TestClient(create_app(settings(api_key="secret"), MetricEngine(score)))
    assert client.post("/v1/evaluate", json=payload(), headers=OWNER_HEADERS).status_code == 401
    response = client.post(
        "/v1/evaluate", json=payload(), headers={"Authorization": "Bearer secret", **OWNER_HEADERS}
    )
    assert response.status_code == 200
    metrics = response.json()["results"][0]["metrics"]
    assert metrics["faithfulness"]["status"] == "error"
    assert metrics["response_relevancy"] == {"status": "ok", "value": 0.9, "reason": ""}


def test_concurrent_duplicate_requests_are_computed_once():
    calls = 0

    async def score(_metric, _sample):
        nonlocal calls
        calls += 1
        await asyncio.sleep(0.01)
        return 1.0

    client = TestClient(create_app(settings(), MetricEngine(score)))

    async def send():
        return await asyncio.to_thread(client.post, "/v1/evaluate", json=payload("evb_concurrent"), headers=OWNER_HEADERS)

    async def concurrent():
        return await asyncio.gather(send(), send())

    first, second = asyncio.run(concurrent())
    assert first.status_code == second.status_code == 200
    assert calls == 2


def test_metric_work_is_bounded_and_parallel():
    active = 0
    peak = 0

    async def score(_metric, _sample):
        nonlocal active, peak
        active += 1
        peak = max(peak, active)
        await asyncio.sleep(0.01)
        active -= 1
        return 0.7

    client = TestClient(
        create_app(settings(evaluation_concurrency=2), MetricEngine(score))
    )
    request = payload("evb_parallel")
    request["samples"].append(
        {**request["samples"][0], "caseId": "case-2"}
    )
    response = client.post("/v1/evaluate", json=request, headers=OWNER_HEADERS)
    assert response.status_code == 200
    assert peak == 2
