# Multi-pod smoke test — stateless gateway + Postgres + S3 (MinIO)

This compose brings up:

- **Postgres 16** — sessions, memory, identity files, agent metadata, bindings
- **MinIO** — S3-compatible bucket for workspace artifacts (auto-creates `fastclaw` bucket)
- **pod-a** on `:18953` and **pod-b** on `:18954` — identical gateway binaries, pointed at the same DB and S3

Both pods use `FASTCLAW_AUTH_TOKEN=dev-admin-token`. Any admin API call takes `Authorization: Bearer dev-admin-token`.

## Run

```bash
docker compose -f deploy/multi-pod/docker-compose.yaml up --build
```

First build takes a couple of minutes (web UI + Go + minio bootstrap).

## Verify stateless-gateway

Run the checks in order. Each step should behave identically whether you hit pod A or pod B.

### 1. Status — both pods up

```bash
curl -s -H "Authorization: Bearer dev-admin-token" http://localhost:18953/api/status | jq .running
curl -s -H "Authorization: Bearer dev-admin-token" http://localhost:18954/api/status | jq .running
# both → true
```

### 2. Create an agent on pod A, list on pod B

```bash
curl -sX POST -H "Authorization: Bearer dev-admin-token" \
     -H "Content-Type: application/json" \
     -d '{"id":"test-alpha","model":"ollama/qwen3.5:35b-a3b-int4"}' \
     http://localhost:18953/api/agents

# Pod B sees it (may need a moment — hot-reload is async):
curl -s -H "Authorization: Bearer dev-admin-token" \
     http://localhost:18954/api/agents | jq '.[].id'
# → "fastclaw", "test-alpha", ...
```

### 3. Edit SOUL.md on pod A, read it back on pod B

```bash
curl -sX PUT -H "Authorization: Bearer dev-admin-token" \
     -H "Content-Type: application/json" \
     -d '{"content":"# Test Alpha\n\nI was written by pod A."}' \
     http://localhost:18953/api/agents/test-alpha/system-files/SOUL.md

curl -s -H "Authorization: Bearer dev-admin-token" \
     http://localhost:18954/api/agents/test-alpha/system-files/SOUL.md
# → {"content":"# Test Alpha\n\nI was written by pod A."}
```

Identity files live in Postgres; every pod reads the same row.

### 4. Create API key, bind the agent, test per-key scoping

```bash
# Admin creates a new API key
curl -sX POST -H "Authorization: Bearer dev-admin-token" \
     -H "Content-Type: application/json" \
     -d '{"id":"customer-1","name":"Customer 1"}' \
     http://localhost:18953/v1/admin/apikeys
# → {"apikey":{...}, "key":"fc_XXXX..."}  ← copy this

CUST_KEY=fc_XXXX

# Bind test-alpha to customer-1 (admin-only)
curl -sX POST -H "Authorization: Bearer dev-admin-token" \
     -H "Content-Type: application/json" \
     -d '{"apiKeyId":"customer-1"}' \
     http://localhost:18953/api/agents/test-alpha/binding

# Customer-1 lists agents — sees only test-alpha
curl -s -H "Authorization: Bearer $CUST_KEY" \
     http://localhost:18954/api/agents | jq '.[].id'
# → "test-alpha"   (not "fastclaw")

# Customer-1 tries to read a non-owned agent — forbidden
curl -s -o /dev/null -w "%{http_code}\n" \
     -H "Authorization: Bearer $CUST_KEY" \
     http://localhost:18954/api/agents/fastclaw/system-files/SOUL.md
# → 403
```

### 5. Workspace writes via S3

When an agent calls `write_file("report.pdf", ...)` the bytes land in MinIO, not on the pod filesystem. After running a tool that writes a file:

```bash
# MinIO console is at http://localhost:9001 (minioadmin / minioadmin).
# Browse bucket "fastclaw" and expect keys like:
#   <agent-id>/<filename>
```

Download via the gateway also works — the admin file-serve endpoint
generates a presigned URL and 302-redirects the browser, so the blob
stream never touches the pod.

### 6. Usage counters (admin only)

```bash
curl -s -H "Authorization: Bearer dev-admin-token" \
     http://localhost:18953/api/admin/usage | jq .
# Rows accumulate as agents write to workspace.
```

Query filters: `?apiKey=...`, `?agent=...`, `?kind=workspace_bytes,tokens_in`,
`?since=2026-04-01`, `?until=2026-04-30`.

### 7. Pod failover

Kill pod A while pod B is running:

```bash
docker compose -f deploy/multi-pod/docker-compose.yaml stop pod-a
```

Every request above still works against pod B — no state was lost. Sandbox
sessions in-flight on pod A would need to be re-initiated (the sandbox
itself is pod-local), but the agent's identity / sessions / memory /
workspace files are all in Postgres + MinIO.

## What this does NOT verify yet

- **Sandbox lifecycle** — lazy creation + idle eviction + flush is in code
  and unit-tested, but live behavior requires either the Docker backend
  (which needs docker-in-docker inside the pod; see
  `internal/sandbox/docker.go` for the image expectations) or a live E2B
  API key. Set `FASTCLAW_SANDBOX_BACKEND=docker` or `e2b` plus an `E2B_API_KEY`
  to test.

- **Chat flow** — this compose ships the gateway bare. Hook it up to your
  provider (OpenAI / Anthropic / Ollama) via `/api/config` after bringing
  up the stack, then open `http://localhost:18953` for the admin UI.

## Teardown

```bash
docker compose -f deploy/multi-pod/docker-compose.yaml down -v
```

`-v` wipes Postgres + MinIO volumes so the next run starts clean.
