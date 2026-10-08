# OpenIM presence and receipts implementation plan

> **For agentic workers:** Use inline execution in the current session. The referenced superpowers execution subskills are unavailable; the user has already authorized implementation and deployment.

**Goal:** Make the existing bkcrab bot a friend of the requesting user, maintain real online presence, and acknowledge single-chat messages when processing starts.

**Architecture:** Keep webhooks and the SQL inbox as the only ingress. Add an optional, leased WebSocket presence connection and a bounded read-receipt capability invoked by gateway task execution and successful steering. Resolve the exact upstream message ID to its sequence before acknowledging it.

**Tech Stack:** Go, existing gorilla/websocket, OpenIM v3.8.3-patch.12 REST, React, Docker.

**Spec:** User-approved scope in this conversation: friendship, online presence, read receipts; no OpenIM source fork.

## Global constraints

- Preserve local `.gitignore` and the remote repository's unrelated uncommitted Jev work.
- Keep account IDs and webhook capabilities unchanged when adding `wsUrl`.
- Never log tokens, passwords, token-bearing URLs or raw upstream errors.
- Only acknowledge the exact accepted single-chat message; no conversation-wide read call.
- Deploy bkcrab with Docker, stop the existing container before replacing its port bindings.

### Task 1: Presence lifecycle

Files: `internal/config/config.go`, `internal/channels/openim_api.go`, `openim.go`, new `openim_presence.go`, `openim_presence_test.go`, `lease.go`, `internal/gateway/channels.go`, `web/src/lib/api.ts`, `web/src/components/openim-connect-dialog.tsx`.

- [x] Write tests for URL normalization, token platform/user, reconnect, server ping response, context cancellation and permanent Stop.
- [x] Run `go test ./internal/channels -run 'OpenIM.*Presence' -count=1` and confirm failures before implementation.
- [x] Add optional `WSURL string` (`wsUrl`). Reject credentials, query, fragment and non-ws(s) schemes; preserve account identity.
- [x] Implement `func (o *OpenIM) runPresence(ctx context.Context)` using Linux platform 7. Obtain a fresh user token per connection; use 15-second pings, 60-second read timeout, token-expiry refresh and 1–30-second reconnect backoff. Drain WS frames without producing messages. Close and join all workers on cancellation.
- [x] Register OpenIM as singleton. Expose `Done() <-chan struct{}` and cancel the lease wrapper when permanently stopped, including when waiting for a lease.
- [x] Add optional WS address to the connection form; describe offline behavior when omitted.
- [x] Run focused tests and `npx tsc --noEmit` in `web`.

### Task 2: Exact read receipts

Files: new `internal/channels/openim_read.go`, `openim_read_test.go`, `read_receipt.go`, `read_receipt_test.go`, `internal/gateway/gateway.go`, `routing.go`.

- [x] Write mock API tests that include later messages, mismatched IDs/senders, missing persistence and group/synthetic messages. Assert the read request contains only the exact matching sequence.
- [x] Implement `func (o *OpenIM) MarkRead(ctx context.Context, msg bus.InboundMessage) error`. Search a bounded set of pages (first then recent pages based on total), match sender/recipient/type/message ID, retry briefly for Kafka persistence. Call `/msg/mark_msgs_as_read` with `userID`, `conversationID`, `seqs: [matchedSeq]` only.
- [x] Implement `func (m *Manager) MarkRead(ctx context.Context, msg bus.InboundMessage)` using optional capability dispatch, a 5-second deadline and sanitized failure logging. Skip internal/synthetic inputs.
- [x] Invoke at the task executor immediately before `HandleMessage`; invoke after successful steering. Receipt failure must not prevent an agent response.
- [x] Run channel, gateway, setup and config package tests; record pre-existing failures separately if any.

### Task 3: Deployment and live verification

Files: deployment runbook and validation notes; private operational scripts kept outside Git.

- [x] Restore OpenIM Docker networking and confirm all application processes healthy.
- [x] Import friendship for user `2305090317` and `bkcrab_assistant`, verify both directions.
- [ ] Review diff, commit only task files, push main. Build an isolated remote checkout of the exact commit.
- [ ] Snapshot the running bkcrab container configuration privately. Recreate only bkcrab with the new image, preserving mounts, environment, ports and networks; keep a stopped rollback container.
- [ ] Configure `wsUrl` through the authenticated channel API while preserving the agent and callback.
- [ ] Verify online status beyond a heartbeat interval, exact read notification, bot reply and disconnect/reconnect. Do not acknowledge historical user messages as part of the probe.
- [ ] Record results and rollback instructions, report outcome to user.

Self-review: all three requested behaviors are covered. Existing webhook ingress, bot identity and binding remain stable. No subagents or upstream source changes are needed.
