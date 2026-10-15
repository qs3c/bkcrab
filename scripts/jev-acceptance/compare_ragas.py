"""Small billed adapter smoke, isolated from production routing and datasets.
Use the current app metric engine. The direct DeepSeek transport preserves
forced tool selection, disables thinking, and meters every actual LLM attempt.
"""

import asyncio
import json
import sys
import time
from dataclasses import replace
from pathlib import Path

import openai

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "services/rag-evaluator"))
OriginalClient = openai.AsyncOpenAI
spend = 0.0
calls = []


class BoundedDirectClient(OriginalClient):
    def __init__(self, *args, **kwargs):
        kwargs.update(max_retries=0, timeout=60)
        super().__init__(*args, **kwargs)
        if str(self.base_url).rstrip("/") != "https://api.deepseek.com/v1":
            return
        raw = self.chat.completions.create

        async def invoke(*args, **kwargs):
            global spend
            # At most $0.20 for all LLM attempts; $0.02 reserved before each.
            if spend + 0.02 > 0.20:
                raise RuntimeError("acceptance LLM budget reached")
            kwargs["extra_body"] = {
                **kwargs.get("extra_body", {}),
                "thinking": {"type": "disabled"},
            }
            start = time.monotonic()
            try:
                result = await raw(*args, **kwargs)
            except Exception:
                spend += 0.02  # Unknown billed usage is not zero.
                calls.append(
                    {"status": "error", "estimatedCostUsd": 0.02, "usageUnknown": True}
                )
                raise
            usage = result.usage
            cost = (usage.prompt_tokens * 0.3 + usage.completion_tokens * 1.2) / 1e6
            spend += cost
            calls.append(
                {
                    "status": "ok",
                    "model": result.model,
                    "inputTokens": usage.prompt_tokens,
                    "outputTokens": usage.completion_tokens,
                    "estimatedCostUsd": cost,
                    "durationMs": round((time.monotonic() - start) * 1000),
                }
            )
            return result

        self.chat.completions.create = invoke


openai.AsyncOpenAI = BoundedDirectClient
from app.metrics import build_ragas_engine, judge_owner_scope
from app.protocol import Sample
from app.settings import Settings
from app.usage import usage_scope


async def main():
    base = Settings.from_env()
    base.validate()
    engines = {}
    for bundle in ("rag-core-v1", "rag-jev-hybrid-v1"):
        cfg = replace(base, metric_bundle_version=bundle)
        cfg.validate()
        engines[bundle] = build_ragas_engine(cfg)
    samples = [
        Sample(
            caseId="synthetic-en",
            userInput="What are the office hours on weekdays?",
            response="The office opens at 9 AM and closes at 6 PM on weekdays.",
            reference="The office opens at 9 AM and closes at 5 PM on weekdays.",
            retrievedContexts=[
                "The office opens at 9 AM and closes at 5 PM on weekdays.",
                "The office walls are blue.",
            ],
            retrievedContextIds=["hours", "walls"],
        ),
        Sample(
            caseId="synthetic-zh",
            userInput="办公室在工作日的营业时间是什么？",
            response="办公室在工作日上午九点开门，下午六点关门。",
            reference="办公室在工作日上午九点开门，下午五点关门。",
            retrievedContexts=[
                "办公室在工作日上午九点开门，下午五点关门。",
                "办公室的墙壁是蓝色的。",
            ],
            retrievedContextIds=["hours", "walls"],
        ),
    ]
    with Path(sys.argv[1]).open("x") as output:
        for index, sample in enumerate(samples):
            bundles = list(engines) if index == 0 else list(reversed(engines))
            for bundle in bundles:
                for metric in (
                    "context_precision",
                    "faithfulness",
                    "context_recall",
                    "factual_correctness",
                ):
                    start = time.monotonic()
                    before = len(calls)
                    with judge_owner_scope("acceptance"), usage_scope() as meter:
                        result = await engines[bundle].evaluate(metric, sample)
                        usage = meter.response(base)
                    row = {
                        "caseId": sample.caseId,
                        "bundle": bundle,
                        "metric": metric,
                        "result": result.model_dump(),
                        "usage": usage.model_dump(),
                        "durationMs": round((time.monotonic() - start) * 1000),
                        "llmCalls": calls[before:],
                        "cumulativeLLMEstimatedCostUsd": spend,
                    }
                    output.write(json.dumps(row, ensure_ascii=False) + "\n")
                    output.flush()
                    print(sample.caseId, bundle, metric, result.status, flush=True)
                    if spend + 0.02 > 0.20:
                        return


if __name__ == "__main__":
    asyncio.run(main())
