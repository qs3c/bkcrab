# Hybrid Jev evaluator Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Deliver hybrid jev evaluator behind explicit deployment configuration.

**Architecture:** Follow the approved shared client and caller-owned policies. Keep existing defaults and experiment behavior. Execute inline in this chat under the user's instruction to start; execution subskills are not installed, so use the steps below directly.

**Tech Stack:** Go 1.25, net/http, Python 3.12, Ragas 0.3.9, pytest.

**Spec:** docs/superpowers/specs/2026-10-07-jev-integration-design.md

## Global Constraints

- Preserve existing uncommitted batch-20 experiment work.
- Keep production defaults unchanged; no paid calls, deployment, commits, or pushes are part of this implementation.
- Failures are missing/error, never zero scores.
- The local JSON byte guard is not a provider token limit.

## Task 1: Implement and verify the integration

**Files:** internal/jeveval/service.go; internal/setup/handlers_rag_eval_jev.go; services/rag-evaluator/app/jev.py; services/rag-evaluator/app/metrics.py; services/rag-evaluator/app/protocol.py; services/rag-evaluator/app/usage.py; services/rag-evaluator/app/settings.py; services/rag-evaluator/tests/test_jev.py; internal/rag/eval/ragas_client.go; internal/rag/eval/runner.go

**Interfaces:** Versioned metric bundles; server-owned relevance/support questions; separately metered usage

- [x] Add contract tests for the relevant cases in the spec before enabling the integration. Use local HTTP stubs; credentials and private texts must not appear in failure output.
- [x] Implement the production path, configuration validation, and caller-specific error policy with the interfaces above.
- [x] Run targeted tests and fix discovered failures:

```sh
go test ./internal/jeveval ./internal/setup ./internal/rag/eval; python -m pytest services/rag-evaluator/tests
```

- [x] Add deployment examples, explain observability and rollback, and record exact validation outcomes in docs/jev-integration.md.
- [x] Review the diff against the spec and confirm no paid provider request, default activation, commit, or unrelated edit occurred.

## Execution result

Implemented and verified offline on 2026-10-07. See [integration guide](../../jev-integration.md) for configuration, failure behavior, rollback, validation and remaining live acceptance. No billed requests, deployment, commit or push performed.
