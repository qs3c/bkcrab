from __future__ import annotations

from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

METRIC_BUNDLE_VERSION = "rag-core-v1"
METRIC_BUNDLES = {"rag-core-v1", "rag-jev-precision-v1", "rag-jev-hybrid-v1"}
Bundle = Literal["rag-core-v1", "rag-jev-precision-v1", "rag-jev-hybrid-v1"]
PROTOCOL_VERSION = "rag-evaluator-v2"
ALLOWED_METRICS = frozenset(
    {"context_precision", "context_recall", "faithfulness", "response_relevancy", "factual_correctness"}
)


class Sample(BaseModel):
    model_config = ConfigDict(extra="forbid")
    caseId: str = Field(min_length=1, max_length=255)
    userInput: str = Field(min_length=1, max_length=65_536)
    retrievedContexts: list[str] = Field(default_factory=list, max_length=20)
    retrievedContextIds: list[str] = Field(default_factory=list, max_length=20)
    response: str = Field(default="", max_length=262_144)
    reference: str = Field(default="", max_length=262_144)
    referenceContexts: list[str] = Field(default_factory=list, max_length=20)

    @model_validator(mode="after")
    def bounded_contexts(self) -> Sample:
        contexts = [*self.retrievedContexts, *self.referenceContexts]
        if any(len(value.encode("utf-8")) > 65_536 for value in contexts):
            raise ValueError("context exceeds 65536 bytes")
        if len(self.retrievedContextIds) != len(self.retrievedContexts):
            raise ValueError("retrievedContextIds must match retrievedContexts")
        return self


class EvaluateRequest(BaseModel):
    model_config = ConfigDict(extra="forbid")
    requestId: str = Field(min_length=1, max_length=255)
    metricBundleVersion: Bundle
    jevModel: str = Field(default="", max_length=255)
    metrics: list[str] = Field(min_length=1, max_length=5)
    samples: list[Sample] = Field(min_length=1, max_length=16)

    @model_validator(mode="after")
    def closed_metrics_and_cases(self) -> EvaluateRequest:
        unknown = sorted(set(self.metrics) - ALLOWED_METRICS)
        if unknown:
            raise ValueError(f"unsupported metrics: {', '.join(unknown)}")
        if len(set(self.metrics)) != len(self.metrics):
            raise ValueError("duplicate metrics")
        case_ids = [sample.caseId for sample in self.samples]
        if len(set(case_ids)) != len(case_ids):
            raise ValueError("duplicate caseId")
        return self


class MetricResult(BaseModel):
    status: Literal["ok", "skipped_missing_input", "error"]
    value: float | None = Field(default=None, ge=0, le=1)
    reason: str = Field(default="", max_length=2048)
    details: dict = Field(default_factory=dict, exclude_if=lambda v: not v)


class CaseResult(BaseModel):
    caseId: str
    metrics: dict[str, MetricResult]


class EvaluationUsage(BaseModel):
    # Preserve the original v2 wire shape when no Jev call was made. Older Go
    # clients reject unknown JSON fields even when their values are zero.
    jevInputTokens: int = Field(default=0, ge=0, exclude_if=lambda v: v == 0)
    jevOutputTokens: int = Field(default=0, ge=0, exclude_if=lambda v: v == 0)
    jevEstimatedCostUsd: float = Field(default=0, ge=0, exclude_if=lambda v: v == 0)
    jevModel: str = Field(default="", exclude_if=lambda v: not v)
    jevUsageUnknown: bool = Field(default=False, exclude_if=lambda v: not v)
    llmInputTokens: int = Field(default=0, ge=0)
    llmOutputTokens: int = Field(default=0, ge=0)
    llmEstimatedCostUsd: float = Field(default=0, ge=0)
    embeddingInputTokens: int = Field(default=0, ge=0)
    embeddingEstimatedCostUsd: float = Field(default=0, ge=0)


class EvaluateResponse(BaseModel):
    requestId: str
    ragasVersion: str
    metricBundleVersion: Bundle = METRIC_BUNDLE_VERSION
    results: list[CaseResult]
    usage: EvaluationUsage = Field(default_factory=EvaluationUsage)
