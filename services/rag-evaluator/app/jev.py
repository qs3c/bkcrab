"""Jev verdicts via the authenticated main-process proxy; no provider keys here."""

from __future__ import annotations

import asyncio
import math
from contextlib import contextmanager
from contextvars import ContextVar

import httpx

from .usage import record_jev_response

_audits: ContextVar[list | None] = ContextVar("jev_metric_audits", default=None)


@contextmanager
def decision_audit_scope():
    audits = []
    token = _audits.set(audits)
    try:
        yield audits
    finally:
        _audits.reset(token)


class JevJudge:
    def __init__(self, settings, owner_fn):
        self.settings = settings
        self.owner_fn = owner_fn

    async def verdicts(self, kind, *, contexts, query="", reference="", claims=None):
        owner = self.owner_fn()
        if not owner:
            raise RuntimeError("evaluation judge owner is unavailable")
        payload = {"kind": kind, "model": self.settings.jev_model, "contexts": contexts}
        if kind == "precision":
            payload.update(query=query, reference=reference)
            count = len(contexts)
        else:
            payload["claims"] = claims or []
            count = len(payload["claims"])
        audit = {
            "rubric": "jev-eval-v1",
            "model": self.settings.jev_model,
            "usageUnknown": True,
            "kind": kind,
        }
        if _audits.get() is not None:
            _audits.get().append(audit)
        # No automatic sidecar retries: the main process owns the total attempt budget.
        try:
            async with httpx.AsyncClient(
                timeout=self.settings.metric_timeout_seconds,
                follow_redirects=False,
                trust_env=False,
            ) as client:
                response = await client.post(
                    self.settings.jev_endpoint,
                    json=payload,
                    headers={
                        "Authorization": f"Bearer {self.settings.api_key}",
                        "X-BkCrab-Eval-Owner": owner,
                    },
                )
                response.raise_for_status()
                body = response.json()
            result = body["result"]
            usage = result["usage"]
            for key in ("inputTokens", "outputTokens", "attempts"):
                if type(usage[key]) is not int or usage[key] < 0:
                    raise ValueError("invalid Jev usage")
            cost = usage["estimatedCostUsd"]
            if (
                isinstance(cost, bool)
                or not isinstance(cost, (float, int))
                or not math.isfinite(cost)
                or cost < 0
            ):
                raise ValueError("invalid Jev cost")
        except asyncio.CancelledError:
            record_jev_response(
                {"unknown": True, "estimatedCostUsd": self.settings.jev_max_request_cost_usd},
                self.settings.jev_model,
            )
            raise
        except (httpx.HTTPError, ValueError, KeyError, TypeError):
            record_jev_response(
                {"unknown": True, "estimatedCostUsd": self.settings.jev_max_request_cost_usd},
                self.settings.jev_model,
            )
            raise RuntimeError("Jev proxy unavailable or invalid response") from None
        record_jev_response(usage, result.get("model") or self.settings.jev_model)
        audit.update(
            rubric=body.get("rubric"),
            model=result.get("model") or self.settings.jev_model,
            usageUnknown=bool(usage.get("unknown")),
        )
        if body.get("error"):
            raise RuntimeError("Jev judge failed; score unavailable")
        if body.get("rubric") != "jev-eval-v1" or result.get("model") != self.settings.jev_model:
            raise RuntimeError("Jev rubric or resolved model mismatch")
        probabilities = result.get("probabilities", {})
        if set(probabilities) != {f"c{i}" for i in range(count)}:
            raise RuntimeError("Jev verdict coverage mismatch")
        values = [probabilities[f"c{i}"] for i in range(count)]
        if any(
            isinstance(p, bool)
            or not isinstance(p, (float, int))
            or not math.isfinite(p)
            or not 0 <= p <= 1
            for p in values
        ):
            raise RuntimeError("invalid Jev probability")
        audit["probabilities"] = values
        audit["threshold"] = 0.5
        return [int(p >= 0.5) for p in values]


def hybrid_metrics(metrics, judge, llm, bundle):
    # These private extension points are pinned to Ragas 0.3.9 and tested below.
    from ragas.metrics.collections._context_precision import ContextPrecisionWithReference
    from ragas.metrics.collections._context_recall import ContextRecall
    from ragas.metrics.collections._factual_correctness import FactualCorrectness
    from ragas.metrics.collections._faithfulness import (
        Faithfulness,
        NLIStatementOutput,
        StatementFaithfulnessAnswer,
        StatementGeneratorOutput,
    )
    from ragas.metrics.result import MetricResult
    from ragas.prompt.metrics.common import statement_generator_prompt

    class Precision(ContextPrecisionWithReference):
        async def ascore(self, user_input, reference, retrieved_contexts):
            verdicts = await judge.verdicts(
                "precision", query=user_input, reference=reference, contexts=retrieved_contexts
            )
            return MetricResult(value=float(self._calculate_average_precision(verdicts)))

    async def verify(claims, contexts):
        # Never silently truncate a long claim list or evidence. Oversized inputs
        # fail as unavailable; a future split strategy needs its own validation.
        verdicts = await judge.verdicts("support", claims=claims, contexts=contexts)
        return NLIStatementOutput(
            statements=[
                StatementFaithfulnessAnswer(
                    statement=claim, reason="Jev support verdict", verdict=v
                )
                for claim, v in zip(claims, verdicts, strict=True)
            ]
        )

    class Faithful(Faithfulness):
        async def _create_verdicts(self, statements, context):
            return await verify(statements, [context])

    class Correct(FactualCorrectness):
        async def _verify_claims(self, claims, reference):
            return await verify(claims, [reference])

        async def _decompose_claims(self, response):
            claims = await super()._decompose_claims(response)
            if not claims:
                raise ValueError("claim extraction produced no claims")
            return claims

    class Recall(ContextRecall):
        async def ascore(self, user_input, retrieved_contexts, reference):
            result = await self.llm.agenerate(
                statement_generator_prompt(user_input, reference), StatementGeneratorOutput
            )
            if not result.statements:
                raise ValueError("reference extraction produced no statements")
            verdicts = await judge.verdicts(
                "support", claims=result.statements, contexts=retrieved_contexts
            )
            return MetricResult(value=sum(verdicts) / len(verdicts))

    metrics["context_precision"] = Precision(llm=llm)
    if bundle == "rag-jev-hybrid-v1":
        metrics.update(
            faithfulness=Faithful(llm=llm),
            factual_correctness=Correct(llm=llm),
            context_recall=Recall(llm=llm),
        )
    return metrics
