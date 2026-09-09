import asyncio
import json
from dataclasses import replace
from types import SimpleNamespace

import httpx
import openai
import pytest
from pydantic import BaseModel

from app.metrics import build_ragas_engine, judge_owner_scope
from app.protocol import ALLOWED_METRICS
from app.settings import Settings


@pytest.mark.parametrize("budget", [8192, 16384])
def test_judge_budget_reaches_real_instructor_http_request(monkeypatch, budget):
    # Keep the real Ragas/Instructor path, but do not send test telemetry.
    monkeypatch.setattr("ragas.llms.base.track", lambda _event: None)
    monkeypatch.setattr("ragas.embeddings.base.track", lambda _event: None)
    monkeypatch.setenv("RAG_EVALUATOR_LLM_MAX_TOKENS", str(budget))
    settings = replace(
        Settings.from_env(),
        llm_endpoint="https://judge.example/v1",
        llm_api_key="test-key",
        llm_model="test-judge",
        embedding_endpoint="https://embed.example/v1",
        embedding_api_key="test-key",
        embedding_model="test-embed",
    )
    requests = []

    def respond(request):
        body = json.loads(request.content)
        requests.append(body)
        assert request.headers["X-BkCrab-Eval-Owner"] == "test-owner"
        return httpx.Response(200, json={
            "id": "test-completion", "object": "chat.completion", "created": 0,
            "model": "test-judge",
            "choices": [{"index": 0, "finish_reason": "tool_calls", "message": {
                "role": "assistant", "content": None,
                "tool_calls": [{"id": "test-call", "type": "function", "function": {
                    "name": body["tools"][0]["function"]["name"],
                    "arguments": '{"correct":true}',
                }}],
            }}],
        })

    original_client = openai.AsyncOpenAI
    clients = []

    class TestClient(original_client):
        def __init__(self, **kwargs):
            super().__init__(
                **kwargs, http_client=httpx.AsyncClient(transport=httpx.MockTransport(respond))
            )
            clients.append(self)

    captured = {}

    def metric_type(**kwargs):
        captured["llm"] = kwargs["llm"]
        return SimpleNamespace()

    monkeypatch.setattr(openai, "AsyncOpenAI", TestClient)
    monkeypatch.setattr(
        "app.metrics.collection_metric_types",
        lambda: {name: metric_type for name in ALLOWED_METRICS},
    )
    build_ragas_engine(settings)

    class Verdict(BaseModel):
        correct: bool

    async def generate():
        try:
            with judge_owner_scope("test-owner"):
                result = await captured["llm"].agenerate("Assess 2 + 2 = 4", Verdict)
            assert result.correct is True
        finally:
            for client in clients:
                await client.close()

    asyncio.run(generate())
    assert len(requests) == 1
    assert requests[0]["max_tokens"] == budget


def test_judge_budget_default_exceeds_ragas_implicit_limit(monkeypatch):
    monkeypatch.delenv("RAG_EVALUATOR_LLM_MAX_TOKENS", raising=False)
    assert Settings.from_env().llm_max_tokens == 8192


@pytest.mark.parametrize("budget", [0, -1, 131073])
def test_judge_budget_rejects_unbounded_or_invalid_values(budget):
    with pytest.raises(ValueError, match="LLM max tokens"):
        replace(Settings.from_env(), llm_max_tokens=budget).validate()
