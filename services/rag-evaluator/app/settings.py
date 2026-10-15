from __future__ import annotations

import math
import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    service_version: str
    port: int
    api_key: str
    max_request_bytes: int
    llm_endpoint: str
    llm_api_key: str
    llm_model: str
    embedding_endpoint: str
    embedding_api_key: str
    embedding_model: str
    llm_input_cost_per_million_usd: float
    llm_output_cost_per_million_usd: float
    embedding_cost_per_million_usd: float
    max_total_context_bytes: int
    max_reason_chars: int
    metric_timeout_seconds: float
    idempotency_cache_entries: int
    evaluation_concurrency: int
    llm_max_tokens: int = 8192
    metric_bundle_version: str = "rag-core-v1"
    jev_endpoint: str = ""
    jev_model: str = "jev-1.13.0"
    jev_max_request_cost_usd: float = 0.01

    @classmethod
    def from_env(cls) -> Settings:
        return cls(
            metric_bundle_version=os.getenv("RAG_EVALUATOR_METRIC_BUNDLE_VERSION", "rag-core-v1"),
            jev_endpoint=os.getenv("RAG_EVALUATOR_JEV_ENDPOINT", ""),
            jev_model=os.getenv("RAG_EVALUATOR_JEV_MODEL", "jev-1.13.0"),
            jev_max_request_cost_usd=float(os.getenv("RAG_EVALUATOR_JEV_MAX_REQUEST_COST_USD", "0.01")),
            service_version=os.getenv("RAG_EVALUATOR_SERVICE_VERSION", "0.1.0"),
            port=int(os.getenv("RAG_EVALUATOR_PORT", "8080")),
            api_key=os.getenv("RAG_EVALUATOR_API_KEY", ""),
            max_request_bytes=int(os.getenv("RAG_EVALUATOR_MAX_REQUEST_BYTES", "4194304")),
            llm_endpoint=os.getenv("RAG_EVALUATOR_LLM_ENDPOINT", ""),
            llm_api_key=os.getenv("RAG_EVALUATOR_LLM_API_KEY", ""),
            llm_model=os.getenv("RAG_EVALUATOR_LLM_MODEL", ""),
            llm_max_tokens=int(os.getenv("RAG_EVALUATOR_LLM_MAX_TOKENS", "8192")),
            embedding_endpoint=os.getenv("RAG_EVALUATOR_EMBEDDING_ENDPOINT", ""),
            embedding_api_key=os.getenv("RAG_EVALUATOR_EMBEDDING_API_KEY", ""),
            embedding_model=os.getenv("RAG_EVALUATOR_EMBEDDING_MODEL", ""),
            llm_input_cost_per_million_usd=float(
                os.getenv("RAG_EVALUATOR_LLM_INPUT_COST_USD_PER_MILLION", "0")
            ),
            llm_output_cost_per_million_usd=float(
                os.getenv("RAG_EVALUATOR_LLM_OUTPUT_COST_USD_PER_MILLION", "0")
            ),
            embedding_cost_per_million_usd=float(
                os.getenv("RAG_EVALUATOR_EMBEDDING_COST_USD_PER_MILLION", "0")
            ),
            max_total_context_bytes=int(
                os.getenv("RAG_EVALUATOR_MAX_TOTAL_CONTEXT_BYTES", "1048576")
            ),
            max_reason_chars=int(os.getenv("RAG_EVALUATOR_MAX_REASON_CHARS", "2048")),
            metric_timeout_seconds=float(
                os.getenv("RAG_EVALUATOR_METRIC_TIMEOUT_SECONDS", "120")
            ),
            idempotency_cache_entries=int(
                os.getenv("RAG_EVALUATOR_IDEMPOTENCY_CACHE_ENTRIES", "1000")
            ),
            evaluation_concurrency=int(
                os.getenv("RAG_EVALUATOR_CONCURRENCY", "1")
            ),
        )

    @property
    def judge_configured(self) -> bool:
        return all(
            (
                self.llm_endpoint,
                self.llm_api_key,
                self.llm_model,
                self.embedding_endpoint,
                self.embedding_api_key,
                self.embedding_model,
            )
        )

    def validate(self) -> None:
        if self.metric_bundle_version not in {"rag-core-v1", "rag-jev-precision-v1", "rag-jev-hybrid-v1"}:
            raise ValueError("unsupported evaluator metric bundle")
        if self.metric_bundle_version != "rag-core-v1":
            if not self.jev_endpoint or not self.api_key or not self.jev_model or any(v in self.jev_model for v in ("latest", "preview")):
                raise ValueError("Jev bundle requires internal endpoint, authentication, and pinned model")
            if not math.isfinite(self.jev_max_request_cost_usd) or self.jev_max_request_cost_usd <= 0:
                raise ValueError("invalid Jev request cost limit")
        prices = (
            self.llm_input_cost_per_million_usd,
            self.llm_output_cost_per_million_usd,
            self.embedding_cost_per_million_usd,
        )
        if any(not math.isfinite(value) or value < 0 for value in prices):
            raise ValueError("evaluator token prices must be finite and non-negative")
        if self.judge_configured and any(value <= 0 for value in prices):
            raise ValueError("configured evaluator judge requires explicit positive token prices")
        if self.evaluation_concurrency < 1 or self.evaluation_concurrency > 32:
            raise ValueError("evaluator concurrency must be between 1 and 32")
        if self.llm_max_tokens < 1 or self.llm_max_tokens > 131_072:
            raise ValueError("evaluator LLM max tokens must be between 1 and 131072")
