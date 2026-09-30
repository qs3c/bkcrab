# OpenIM Channel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** Connect OpenIM ordinary bot accounts to bkcrab agents through authenticated webhooks and REST text replies.

**Architecture:** Add an OpenIM adapter alongside existing channels. SQL ingress records provide cross-process deduplication and recover pending callbacks; the existing gateway routes accepted messages. An instance-level webhook fans out to bound bot accounts.

**Tech Stack:** Go standard HTTP/JSON, existing SQL stores, Next.js/React.

**Spec:** `docs/superpowers/specs/2026-09-29-openim-channel-design.md`

## Global Constraints

- No new dependencies or OpenIM source modifications.
- Single chat and explicit group mentions; complete text replies.
- Administrator credentials remain server-side; group allowlist required for groups.
- Preserve existing untracked `docs/patents/`.
- Durable ingress is not a claim of exactly-once agent execution.

## Task 1: Adapter and ingress store

Files: create `internal/channels/openim.go`, `openim_api.go`, `openim_test.go`; create `internal/store/channel_inbox.go`, `channel_inbox_test.go`; update SQL migration lists and `internal/config/config.go`.

- [x] Test ordinary text/mention decoding, self filtering, group allowlist, isolated IDs, token validation/refresh, duplicate ingress, concurrent claims and expired claim recovery.
- [x] Add `OpenIMConfig`, `NewOpenIM(config, bus, inbox)`, `Validate(ctx)`, `HandleWebhook(ctx, command, body)`, and the existing Channel methods.
- [x] Add inbox interface methods `SaveChannelInbox`, `ClaimChannelInbox`, `CompleteChannelInbox`, and `PruneChannelInbox` on DBStore, using database unique keys and compare-and-swap leases.
- [x] Run `go test ./internal/channels ./internal/store` and classify failures.

## Task 2: Gateway and management API

Files: `internal/gateway/channels.go`, `openim.go`, `dedup.go`, `routing.go`; `internal/setup/handlers_openim.go`, `server.go`; focused gateway/setup tests.

- [x] Register each OpenIM account with its ingress store; dispatch instance webhooks only after constant-time secret verification. Drop known bot senders to prevent bot-to-bot loops.
- [x] Add authenticated connect handler and bounded public callback handler. Validate URL, credentials, registered bot, allowed groups and credential uniqueness before persistence.
- [x] Keep OpenIM IDs namespaced and prevent group content-hash deduplication from dropping distinct identical messages.
- [x] Test callback authorization, multi-bot dispatch, invalid callbacks, config ownership and save behavior.
- [x] Run `go test ./internal/gateway ./internal/setup`.

## Task 3: Connection UI and deployment guide

Files: `web/src/components/openim-connect-dialog.tsx`, agent channel catalog, API client, `docs/openim-channel.md`.

- [x] Add form for API URL, administrator user ID/secret, bot user ID and allowed groups. Show webhook URL and exact OpenIM configuration only after successful validation; clear credentials when closed.
- [x] Document pre-registration, private Docker addresses, secret URL treatment, text-only scope, durable queue limitations and a manual end-to-end check.
- [x] Run `npx tsc --noEmit` and targeted ESLint in `web`, then review `git diff --check` and the complete patch.

Execution note: the referenced execution sub-skills are not installed in this workspace. Use the ordinary in-session implementation and verification workflow; no delegation is required.

## Validation results — 2026-09-29

- Passed: full `internal/channels`, `internal/gateway`, and `internal/setup` package tests, including OpenIM HTTP mocks, routing, ownership, and real SQLite ingress-to-bus coverage.
- Passed: `go test ./internal/store -run 'TestChannelInbox|TestMigrateIdempotentOnFreshInstall' -count=1`.
- Passed: `npx tsc --noEmit`, targeted ESLint (no errors; the existing channel page has eight unrelated warnings), and `git diff --check`.
- Full store regression has one pre-existing failure: `TestRAGEvalDatasetStagingCandidatesAreTTLBounded`, whose fixture writes local time while the query compares a UTC timestamp in SQLite.
- Full agent regression has one pre-existing failure: `TestPublishedSkillSummaryAndExecutionShareSnapshot`, because this Windows process lacks symlink creation privileges.
- Both failures reproduced using a Go source overlay restoring the affected store migration / agent loop files from HEAD. No unrelated fixes were applied.
- MySQL/PostgreSQL integration and a real OpenIM server were not validated. The user confirmed OpenIM is not deployed and requested completing bkcrab integration first. No server deployment, push, or live restart was performed.

