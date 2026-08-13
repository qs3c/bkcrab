from __future__ import annotations

from collections.abc import Iterator
from contextlib import contextmanager
from contextvars import ContextVar
from dataclasses import dataclass

from .protocol import EvaluationUsage
from .settings import Settings


@dataclass
class UsageMeter:
    llm_input_tokens: int = 0
    llm_output_tokens: int = 0
    embedding_tokens: int = 0

    def response(self, settings: Settings) -> EvaluationUsage:
        llm_cost = (
            self.llm_input_tokens * settings.llm_input_cost_per_million_usd
            + self.llm_output_tokens * settings.llm_output_cost_per_million_usd
        ) / 1_000_000
        embedding_cost = (
            self.embedding_tokens * settings.embedding_cost_per_million_usd
        ) / 1_000_000
        return EvaluationUsage(
            llmInputTokens=self.llm_input_tokens,
            llmOutputTokens=self.llm_output_tokens,
            llmEstimatedCostUsd=llm_cost,
            embeddingInputTokens=self.embedding_tokens,
            embeddingEstimatedCostUsd=embedding_cost,
        )


_current_meter: ContextVar[UsageMeter | None] = ContextVar("rag_eval_usage", default=None)


@contextmanager
def usage_scope() -> Iterator[UsageMeter]:
    meter = UsageMeter()
    token = _current_meter.set(meter)
    try:
        yield meter
    finally:
        _current_meter.reset(token)


def record_llm_response(response: object) -> None:
    meter = _current_meter.get()
    usage = getattr(response, "usage", None)
    if meter is None or usage is None:
        return
    meter.llm_input_tokens += max(0, int(getattr(usage, "prompt_tokens", 0) or 0))
    meter.llm_output_tokens += max(0, int(getattr(usage, "completion_tokens", 0) or 0))


def record_embedding_response(response: object) -> None:
    meter = _current_meter.get()
    usage = getattr(response, "usage", None)
    if meter is None or usage is None:
        return
    prompt_tokens = getattr(usage, "prompt_tokens", None)
    if prompt_tokens is None:
        prompt_tokens = getattr(usage, "total_tokens", 0)
    meter.embedding_tokens += max(0, int(prompt_tokens or 0))
