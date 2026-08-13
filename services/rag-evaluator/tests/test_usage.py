from dataclasses import replace
from types import SimpleNamespace

import pytest

from app.settings import Settings
from app.usage import record_embedding_response, record_llm_response, usage_scope


def test_usage_scope_captures_provider_reported_tokens_and_prices() -> None:
    settings = replace(
        Settings.from_env(),
        llm_input_cost_per_million_usd=2,
        llm_output_cost_per_million_usd=4,
        embedding_cost_per_million_usd=0.5,
    )
    with usage_scope() as meter:
        record_llm_response(
            SimpleNamespace(usage=SimpleNamespace(prompt_tokens=100, completion_tokens=50))
        )
        record_embedding_response(SimpleNamespace(usage=SimpleNamespace(prompt_tokens=200)))
    usage = meter.response(settings)
    assert usage.llmInputTokens == 100
    assert usage.llmOutputTokens == 50
    assert usage.embeddingInputTokens == 200
    assert usage.llmEstimatedCostUsd == pytest.approx(0.0004)
    assert usage.embeddingEstimatedCostUsd == pytest.approx(0.0001)


def test_configured_judge_requires_explicit_prices() -> None:
    settings = replace(
        Settings.from_env(),
        llm_endpoint="https://judge.example/v1",
        llm_api_key="secret",
        llm_model="judge",
        embedding_endpoint="https://embedding.example/v1",
        embedding_api_key="secret",
        embedding_model="embed",
    )
    with pytest.raises(ValueError, match="token prices"):
        settings.validate()
