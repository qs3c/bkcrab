from __future__ import annotations

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
    max_total_context_bytes: int
    max_reason_chars: int
    metric_timeout_seconds: float
    idempotency_cache_entries: int

    @classmethod
    def from_env(cls) -> Settings:
        return cls(
            service_version=os.getenv("RAG_EVALUATOR_SERVICE_VERSION", "0.1.0"),
            port=int(os.getenv("RAG_EVALUATOR_PORT", "8080")),
            api_key=os.getenv("RAG_EVALUATOR_API_KEY", ""),
            max_request_bytes=int(os.getenv("RAG_EVALUATOR_MAX_REQUEST_BYTES", "4194304")),
            llm_endpoint=os.getenv("RAG_EVALUATOR_LLM_ENDPOINT", ""),
            llm_api_key=os.getenv("RAG_EVALUATOR_LLM_API_KEY", ""),
            llm_model=os.getenv("RAG_EVALUATOR_LLM_MODEL", ""),
            embedding_endpoint=os.getenv("RAG_EVALUATOR_EMBEDDING_ENDPOINT", ""),
            embedding_api_key=os.getenv("RAG_EVALUATOR_EMBEDDING_API_KEY", ""),
            embedding_model=os.getenv("RAG_EVALUATOR_EMBEDDING_MODEL", ""),
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
