from __future__ import annotations

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


class MetricEngine:
    """Runs a fixed metric collection; no prompt or executable is accepted from requests."""

    def __init__(self, score_fn: ScoreFn | None = None) -> None:
        self._score_fn = score_fn

    async def evaluate(self, metric: str, sample: Sample) -> MetricResult:
        if reason := missing_input(metric, sample):
            return MetricResult(status="skipped_missing_input", reason=reason)
        if self._score_fn is None:
            return MetricResult(status="error", reason="evaluator judge is not configured")
        try:
            value = await self._score_fn(metric, sample)
            return MetricResult(status="ok", value=max(0.0, min(1.0, float(value))))
        except Exception as exc:  # one judge failure must not erase sibling metrics
            return MetricResult(status="error", reason=str(exc)[:2048])


def build_ragas_engine(settings: Settings) -> MetricEngine:
    if not settings.judge_configured:
        return MetricEngine()

    from openai import AsyncOpenAI
    from ragas.embeddings.base import embedding_factory
    from ragas.llms.base import llm_factory
    from ragas.metrics.collections import (
        AnswerRelevancy,
        ContextPrecisionWithReference,
        ContextRecall,
        FactualCorrectness,
        Faithfulness,
    )

    llm_client = AsyncOpenAI(api_key=settings.llm_api_key, base_url=settings.llm_endpoint)
    embedding_client = AsyncOpenAI(
        api_key=settings.embedding_api_key, base_url=settings.embedding_endpoint
    )
    llm = llm_factory(settings.llm_model, provider="openai", client=llm_client)
    embeddings = embedding_factory(
        "openai", model=settings.embedding_model, client=embedding_client, interface="modern"
    )
    metrics = {
        "context_precision": ContextPrecisionWithReference(llm=llm),
        "context_recall": ContextRecall(llm=llm),
        "faithfulness": Faithfulness(llm=llm),
        "response_relevancy": AnswerRelevancy(llm=llm, embeddings=embeddings),
        "factual_correctness": FactualCorrectness(llm=llm),
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

    return MetricEngine(score_metric)
