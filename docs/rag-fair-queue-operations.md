# RAG fairqueue operations

This runbook covers the `rag.index` resource. Redis coordinates fairness;
MySQL is the canonical recovery and execution authority; RabbitMQ carries only
bounded task identity. Never restore service by starting an unfenced fallback
worker.

## Normal health and capacity

Redis must be a writable standalone primary with persistence. RabbitMQ needs
durable exchanges, queues, messages, and persistent storage. MySQL must be one
authoritative writer topology; every process must derive the same database-
bound fingerprint. Capacity is global=4, per-user base=2, burst=4, borrowing
enabled by default. Size Redis for active users plus per-task reservation
metadata and Rabbit for the durable pending backlog and DLQ.

`/livez` means only that the process is alive. `/readyz` means API/MySQL schema
operations can serve durable create/status/cancel paths. Rabbit unavailable or
Redis RECOVERING pauses new queue work without making the whole API process
dead. Treat these as alerts: increasing oldest undispatched/expired age, DLQ
depth above zero, a long-paused scheduler, repeated recovery/fence loss,
advisory-lock timeouts, or a persistent Redis-stable/MySQL-RUNNING difference.
Logs and metrics never expose credentials, raw epochs/operation IDs, owner
tokens, or raw tenant IDs.

## Rollout and rollback

Never mix legacy and fair claimants in a rolling canary.

1. Deploy the expand/dual-write compatible release in `legacy` everywhere.
   Prove every older writer/ReplicaSet is zero.
2. Run `bkcrab admin fairqueue contract-migrate`; save its aggregate dry-run.
   Apply only with `--apply --confirm-all-writers-dual-write`. Verify
   `rag_index_tasks.user_id IS_NULLABLE='NO'` directly in
   `INFORMATION_SCHEMA` and rerun the zero-owner/generation checks.
3. In a separate full rollout select `paused`. Drain legacy claims and prove
   old processes/heartbeats are zero. If the platform cannot prove this, stay
   paused.
4. In a second full rollout select `fair` and enable the shared infrastructure.
5. Roll back `fair -> paused`, drain, then `paused -> legacy` using only the
   compatible dual-write release. Contract sets the rollback floor; a
   pre-expand binary is forbidden.

## CLI safety contract

All commands are read-only without `--apply`. Unknown/unregistered resources
are rejected before connecting. Output contains aggregate counts and short
safe state only—not DSNs, secrets, raw IDs, epochs, tenants, or task identity.

```bash
bkcrab admin fairqueue contract-migrate
bkcrab admin fairqueue contract-migrate --apply \
  --confirm-all-writers-dual-write

bkcrab admin fairqueue rabbit-disaster-repair --resource rag.index
bkcrab admin fairqueue rabbit-disaster-repair --resource rag.index --apply \
  --confirm-old-broker-isolated --confirm-publishers-paused

bkcrab admin fairqueue rebind-writer --resource rag.index \
  --expected-old-writer-fingerprint <64-lowercase-hex>
bkcrab admin fairqueue rebind-writer --resource rag.index --apply \
  --expected-old-writer-fingerprint <64-lowercase-hex> \
  --confirm-old-writer-fenced --confirm-resource-runtimes-stopped \
  --confirm-new-writer-authoritative

bkcrab admin fairqueue redis-force-rebuild --resource rag.index
bkcrab admin fairqueue redis-force-rebuild --resource rag.index --apply \
  --confirm-discard-redis-coordination-state
```

An interrupted apply is resumed by rerunning the same command and kind. Do not
start a different special operation or manually edit its journal/control.

## Special-operation ordering and allowed start state

Every mutation follows one order: acquire the MySQL start lock, acquire the
Redis raw lock, run control preflight, CAS the MySQL journal, recheck both
fences, then begin Redis RECOVERING. Losing the raw lock inside the journal
window may leave MySQL `ACTIVE`, but it must cause zero Redis control/progress
or business mutation; rerun the same operation.

| Operation | Allowed initial state | Required external proof |
|---|---|---|
| Rabbit repair | NORMAL, or matching unfinished journal/control | old broker isolated; publishers paused |
| Writer rebind | stopped resource; old writer fenced; target schema ready; valid RUNNING=0 | runtimes and recovery coordinator stay stopped through COMPLETED |
| Redis force rebuild | corrupt/missing resource coordination with authoritative MySQL and Rabbit | explicit permission to discard only this resource's Redis keys; wait full drain window |

`ACTIVE` always requires the matching operator. `READY_COMMITTED` can be
completed without another Redis Begin only when Redis READY has the exact
matching last-completed kind/ID. `READY_COMMITTED` with missing Redis, or an
ID/kind/writer mismatch, remains operator-required. A normal runtime never
guesses or finishes a special operation.

## Redis loss or corrupt metadata

For complete Redis loss, rerun the same journaled operation when one exists;
otherwise normal startup performs a paged high-water rebuild from MySQL.
Recovery stays gated until known tenants, dispatched work, and valid RUNNING
reservations converge. Lease remaining time is calculated from `DB_NOW` and
mapped to Redis TIME; node wall-clock skew must not release a stable token
early.

For corrupt metadata use `redis-force-rebuild` dry-run, validate the bounded
key count/pages and authoritative writer/Rabbit checks, stop all resource
runtimes, then apply. It deletes only `{rag.index}` namespaced coordination
keys after the recovery drain window. Loss of the owner lock is resumable by a
new owner; the old owner can neither restore nor finish.

## RabbitMQ data loss

Fence and isolate the old broker first and keep every publisher paused. The
repair scans bounded MySQL pages and atomically advances
`dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1` while
clearing the marker. It never merely clears `dispatched_at`. Redis remains
RECOVERING until the repair and rebuild finish; only the ordinary READY-fenced
dispatcher republishes. A repair interrupted after a half page resumes from
the journal cursor. DLQ publication must confirm before the poison delivery is
ACKed; failure requeues the original.

## Writer failover and rebind

From the first rebind apply through journal `COMPLETED`, keep the resource
runtime, publisher, scheduler, and recovery coordinator stopped. Prove the old
writer is physically fenced and the new writer is authoritative; verify target
schema/generation/owner invariants and valid RUNNING=0. Use the exact expected-
old fingerprint. A missing attestation, stale expected record, connection-ID
switch, or TOCTOU recheck failure performs no business/Redis mutation. After
expected-old CAS and a complete rebuild, verify both MySQL and Redis identities
before restarting fair mode.
