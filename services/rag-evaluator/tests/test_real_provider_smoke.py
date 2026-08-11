import asyncio
import os

import pytest

from app.metrics import build_ragas_engine
from app.protocol import Sample
from app.settings import Settings


def test_real_ragas_provider_smoke_is_explicitly_gated():
    if os.getenv("RAG_EVALUATOR_REAL_SMOKE") != "1":
        pytest.skip("RAG_EVALUATOR_REAL_SMOKE=1 is required for billed provider calls")

    settings = Settings.from_env()
    if not settings.judge_configured:
        pytest.skip("all RAG_EVALUATOR_LLM_* and RAG_EVALUATOR_EMBEDDING_* settings are required")

    sample = Sample.model_validate(
        {
            "caseId": "phase-h-real-smoke",
            "userInput": "What greeting is in the context?",
            "retrievedContexts": ["The guide says hello corpus."],
            "retrievedContextIds": ["phase-h#0"],
            "response": "The greeting is hello corpus.",
            "reference": "hello corpus",
        }
    )
    engine = build_ragas_engine(settings)
    for metric in ("faithfulness", "response_relevancy"):
        result = asyncio.run(engine.evaluate(metric, sample))
        assert result.status == "ok", result.reason
        assert result.value is not None and 0 <= result.value <= 1
