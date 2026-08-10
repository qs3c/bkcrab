from __future__ import annotations

import asyncio
import html
import math
from collections.abc import Awaitable, Callable

from .protocol import MetricResult, Sample
from .settings import Settings

ScoreFn = Callable[[str, Sample], Awaitable[float]]


def missing_input(metric: str, sample: Sample) -> str | None:
    if metric in {"context_precision", "context_recall", "factual_correctness"} and not sample.reference:
        return "reference is required"
    if metric in {"context_precision", "context_recall", "faithfulness"} and not sample.retrievedContexts:
        return "retrievedContexts is required"
    if metric in {"faithfulness", "response_relevancy", "factual_correctness"} and not sample.response:
        return "response is required"
    return None


def _safe_reason(value: object, limit: int) -> str:
    text = " ".join(str(value).split())
    return text[: max(0, min(limit, 2048))]


def _protect_contexts(sample: Sample) -> Sample:
    protected = [
        f'<UNTRUSTED_CONTEXT index="{index}">\n{html.escape(context)}\n</UNTRUSTED_CONTEXT>'
        for index, context in enumerate(sample.retrievedContexts)
    ]
    return sample.model_copy(update={"retrievedContexts": protected})


class MetricEngine:
    """Runs a fixed metric collection; no prompt or executable is accepted from requests."""

    def __init__(
        self,
        score_fn: ScoreFn | None = None,
        *,
        timeout_seconds: float = 120,
        reason_limit: int = 2048,
    ) -> None:
        self._score_fn = score_fn
        self._timeout_seconds = max(0.001, timeout_seconds)
        self._reason_limit = max(0, min(reason_limit, 2048))

    async def evaluate(self, metric: str, sample: Sample) -> MetricResult:
        if reason := missing_input(metric, sample):
            return MetricResult(status="skipped_missing_input", reason=reason)
        if self._score_fn is None:
            return MetricResult(status="error", reason="evaluator judge is not configured")
        try:
            value = float(
                await asyncio.wait_for(
                    self._score_fn(metric, _protect_contexts(sample)),
                    timeout=self._timeout_seconds,
                )
            )
            if not math.isfinite(value) or value < 0 or value > 1:
                raise ValueError("metric returned a score outside [0,1]")
            return MetricResult(status="ok", value=value)
        except TimeoutError:
            return MetricResult(status="error", reason="metric timeout")
        except Exception as exc:  # one judge failure must not erase sibling metrics
            return MetricResult(status="error", reason=_safe_reason(exc, self._reason_limit))


def collection_metric_types() -> dict[str, type]:
    from ragas.metrics.collections import (
        AnswerRelevancy,
        ContextPrecisionWithReference,
        ContextRecall,
        FactualCorrectness,
        Faithfulness,
    )

    return {
        "context_precision": ContextPrecisionWithReference,
        "context_recall": ContextRecall,
        "faithfulness": Faithfulness,
        "response_relevancy": AnswerRelevancy,
        "factual_correctness": FactualCorrectness,
    }


def build_ragas_engine(settings: Settings) -> MetricEngine:
    if not settings.judge_configured:
        return MetricEngine(
            timeout_seconds=settings.metric_timeout_seconds,
            reason_limit=settings.max_reason_chars,
        )

    from openai import AsyncOpenAI
    from ragas.embeddings.base import embedding_factory
    from ragas.llms.base import llm_factory

    llm_client = AsyncOpenAI(api_key=settings.llm_api_key, base_url=settings.llm_endpoint)
    embedding_client = AsyncOpenAI(
        api_key=settings.embedding_api_key, base_url=settings.embedding_endpoint
    )
    llm = llm_factory(settings.llm_model, provider="openai", client=llm_client)
    embeddings = embedding_factory(
        "openai", model=settings.embedding_model, client=embedding_client, interface="modern"
    )
    metric_types = collection_metric_types()
    metrics = {
        "context_precision": metric_types["context_precision"](llm=llm),
        "context_recall": metric_types["context_recall"](llm=llm),
        "faithfulness": metric_types["faithfulness"](llm=llm),
        "response_relevancy": metric_types["response_relevancy"](
            llm=llm, embeddings=embeddings
        ),
        "factual_correctness": metric_types["factual_correctness"](llm=llm),
    }

    async def score_metric(name: str, sample: Sample) -> float:
        values = {
            "user_input": sample.userInput,
            "retrieved_contexts": sample.retrievedContexts,
            "response": sample.response,
            "reference": sample.reference,
        }
        keys = {
            "context_precision": ("user_input", "reference", "retrieved_contexts"),
            "context_recall": ("user_input", "reference", "retrieved_contexts"),
            "faithfulness": ("user_input", "response", "retrieved_contexts"),
            "response_relevancy": ("user_input", "response"),
            "factual_correctness": ("response", "reference"),
        }[name]
        result = await metrics[name].ascore(**{key: values[key] for key in keys})
        return float(result.value)

    return MetricEngine(
        score_metric,
        timeout_seconds=settings.metric_timeout_seconds,
        reason_limit=settings.max_reason_chars,
    )
