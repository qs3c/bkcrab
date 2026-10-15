# Jev integration design

Approved in chat on 2026-10-07, following the source-based four-direction assessment.

## Scope

Implement three independently selectable integrations. Keep production defaults unchanged; no paid calls, deployment, commits, or pushes are part of this implementation. Preserve existing uncommitted batch-20 experiment work. Document parsing quality gates are outside this change.

1. Production reranking: keep the experimental NewJevHTTP contract unchanged. Add a bounded reusable Noul client, explicit Jev protocol, batching without truncation, request-scoped cost reservation, bounded retry under one deadline, observability, and existing RRF fallback. Add explicit score-filter disablement. Preserve original candidate indexes and stable ties.
2. Memory: optional off/shadow/enforce semantic guard injected into manual and automatic memory writers. Local validation runs first. Scan complete proposed entries (including composition), outside mutation locks, with a compare-before-write fence. Network errors never delete old memory. Shadow mode preserves local decisions; enforce mode rejects unreviewed additions/replacements. Removal remains possible. Existing read-time scanner remains local. Memory facts never grant permissions or override system/developer instructions.
3. Evaluation: keep Ragas 0.3.9 and rag-core-v1 as default. Introduce versioned Jev context precision and hybrid claim-verification bundle through the existing evaluator adapter. Generative extraction and response relevancy remain on the original LLM. All arithmetic stays in code. Use fixed server-owned prompts; the sidecar holds only its internal credential. Main process checks the evaluation owner and performs Jev requests. Track Jev usage separately, including retries, and preserve it for failed metrics. Failures are missing/error, never zero scores. Reject cross-bundle requests/resumes and preserve the bundle in stored metric records.

## Shared boundary

internal/jev provides only bounded HTTP Noul decisions (state + named questions -> validated probabilities, exact model, usage). Callers own risk/score thresholds and fallback. Deployment-only Jev endpoint/model/key/timeouts/prices are loaded once; keys are scrubbed from environment and never exposed through configuration APIs. Model aliases are rejected for calibrated production integrations. The local JSON byte guard is not a provider token limit.

## Validation

Offline HTTP contract tests cover missing/extra answers, wrong types, nonfinite values, oversized bodies, trailing JSON, retryable/permanent failures, cancellation, rate-limit backoff and budget exhaustion. Rerank tests cover index mapping, stable ties, split batches, and error propagation to RRF. Memory tests cover shadow/enforce, no IO under lock, concurrent updates, removals, credentials, multi-operation composition and payload-free audit logs. Evaluator tests cover original AP/claim arithmetic, version isolation, owner authentication, scoped usage including errors, and unchanged legacy behavior. Live multilingual accuracy, paid cost/latency and online reliability remain separate acceptance work; synthetic fixtures are not validation of model quality.
