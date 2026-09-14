# Sandbox Reliability Implementation Plan

**Goal:** Bound sandbox resource use and improve single-host recovery.

**Architecture:** Keep Docker Compose; add pool admission and safe lifecycle ownership, streaming Docker workspace sync, and an independent host liveness watchdog.

**Tech Stack:** Go 1.25, Docker Compose, Linux systemd, Python 3.

**Spec:** ../specs/2026-09-14-sandbox-reliability-design.md

## Global Constraints

- Keep Docker deployment and existing data volumes.
- Never claim application polling implements filesystem hard quotas.
- Deployment requires Tailscale connectivity and measured server budgets.

## Task 1: Resource limits and admission

- [ ] Add deployment-only sandbox limit parsing and pass limits into Docker creation and lifecycle admission.
- [ ] Test global/user reservation, cancellation and failure cleanup in internal/sandbox/admission_test.go.
- [ ] Protect live tool calls from eviction and preserve user context in lifecycle.go; test long calls.
- [ ] Run `go test ./internal/sandbox ./internal/config`.

## Task 2: Streaming persistence

- [ ] Add Docker streaming sync and hydration capabilities using io.Reader, scoped file paths and bounded buffers.
- [ ] Test large-file round trip, changed same-size data and symlink handling.
- [ ] Call synchronization after Docker execution and before eviction; preserve local files on restore.
- [ ] Run `go test ./internal/sandbox` and compile gateway.

## Task 3: Host recovery

- [ ] Add watchdog Python script, unit tests, systemd service/timer; check /livez independently of /readyz.
- [ ] Add configurable Compose resource budgets and log rotation, document install and backup restore.
- [ ] Validate Python tests and Compose config without printing secrets.

## Task 4: Deployment

- [ ] Recover Tailscale; inspect filesystem quota support and server budgets.
- [ ] Implement/enable supported hard disk quotas after preserving existing data.
- [ ] Commit and push reviewed changes; pull on server; stop existing service before replacement.
- [ ] Verify resource flags, queue behavior, readiness and watchdog operation; record any remaining blockers honestly.
