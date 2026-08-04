# Image generation batch operations

This runbook covers durable `image_gen_batch` execution on the fairqueue
resource `image.generate`. MySQL is the execution and recovery authority,
RabbitMQ carries only bounded task identity, Redis coordinates fairness, and
the workspace object store owns image bytes. Never restore service by enabling
the legacy synchronous tool beside fair workers.

## Contract and normal operation

The tool accepts three strict actions:

- `create`: exactly one `prompt` or an `items` array, with a total count of
  1–16. Each item is deterministically split into tasks of at most four images.
- `status`: a canonical `batch_id` only.
- `cancel`: a canonical `batch_id` only; repeated cancellation is idempotent.

`wait_seconds=0` returns immediately after the MySQL commit. A positive wait is
only a bounded convenience wait: expiry returns current status and the batch
continues. Status and cancel require the same canonical user and agent that
created the batch. A different session for that same agent is allowed; a
different agent is returned as not found.

Batch terminal states are `DONE`, `PARTIAL`, `FAILED`, and `CANCELED`.
`PARTIAL` means at least one task succeeded and at least one failed. Cancel
sets a durable batch fence, immediately terminalizes pending work, asks running
providers to stop, and lets the expired-task sweeper terminalize a crashed
worker. Successful artifacts remain available when later work fails or the
batch is canceled.

Default fair capacity is global=4, per-user base=2, per-user burst=4, borrowing
enabled. A task occupies one fair slot even when it requests four images. The
provider limiter is separate from fair capacity and bounds physical calls by
provider/model. Redis reservations are coordination hints: MySQL rechecks the
authoritative global and user counts while holding the resource lock, so a
missing reservation cannot permit a fifth valid `RUNNING` task.

## Artifact contract

Provider base64 or HTTPS output is validated and copied into the workspace.
Provider URLs, signed query strings, base64, and image bytes are not returned
in tool text or stored in MySQL, RabbitMQ, or Redis.

Every claim owns this canonical prefix inside the persisted origin scope:

```text
imagegen/<batch-id>/<task-id>/claims/<claim-generation>/
  image-<index>-<sha256>.<extension>
  manifest.json
```

Images are immutable and the manifest is written last. The manifest binds the
scope, batch/task IDs, exact claim generation, request fingerprint,
provider/model, ordered object keys, MIME, size, SHA-256, width, and height.
If a worker dies after writing images but before the manifest, the next claim
does not accept the incomplete set. If it dies after the manifest but before
MySQL finalize, salvage verifies the manifest and every object and finalizes
without another provider charge. Salvage uses the claim's explicit
`PreviousClaimGeneration`; it never guesses `current-1` after a generation
jump. Cancellation disables salvage and stale workers cannot publish into or
finalize a newer claim.

Use LocalFS only when exactly one bkcrab instance can execute and serve image
batches. Every multi-instance deployment must use the same S3 or compatible
workspace backend and credentials; otherwise a status request handled by a
different Pod cannot read the persisted origin objects. Validate cross-Pod
reads before scaling above one replica.

## Health and alerts

`/livez` reports process liveness. `/readyz` reports whether the API and MySQL
schema can serve durable create/status/cancel. A provider outage, Rabbit
outage, or Redis `RECOVERING` pauses new execution but does not remove the API
Pod from readiness: a fair-mode create still commits to MySQL and recovery
dispatches it later. A writer mismatch or incompatible schema fails closed.

Alert on increasing oldest-undispatched or expired age, nonzero DLQ depth,
long scheduler pauses, repeated fence loss, advisory-lock timeouts, provider
limiter exhaustion, artifact-store failures, and persistent differences
between Redis stable reservations and MySQL valid `RUNNING` rows. Logs and
metrics must not contain prompts, credentials, Authorization headers, signed
URLs, base64, raw operation IDs, owner tokens, or raw tenant IDs.

## Rollout and rollback

Do not canary modes at the Pod/ReplicaSet level. All live binaries that share
the resource must agree on the phase.

1. Confirm the generic fairqueue prerequisite and MySQL writer topology are
   complete. Expand the two image tables.
2. Deploy the compatible release everywhere with
   `BKCRAB_IMAGEGEN_BATCH_MODE=legacy`. Prove older binaries are zero. The old
   synchronous `image_gen` remains exposed; the fair runtime is stopped.
3. Roll every instance to `drain`. Prove the previous legacy ReplicaSet and
   synchronous calls are zero. New batch creates are rejected, but
   status/cancel and already claimed fair work remain available.
4. Roll every instance to `fair`. Prove the drain ReplicaSet is zero before
   admitting test users at the application/traffic layer. `image_gen_batch`
   replaces the legacy tool and all counts, including 1–4, use durable tasks.
5. Observe the evidence below, then widen application traffic. Scale above one
   replica only after shared S3 workspace validation.

Rollback uses the same two gates in reverse: `fair -> drain`, wait for fair
claims and the old ReplicaSet to reach zero, then `drain -> legacy`. Never run
legacy and fair claimants together and never make an automatic dependency-
failure fallback to the synchronous tool.

## Routine failure behavior

- Provider rate limit/transient error: try only policy-eligible fallback
  candidates, or schedule a bounded delayed retry. An incomplete provider
  count commits no task result.
- Provider authentication error: fallback occurs only when the safe plan
  explicitly permits it. Credentials are resolved fresh from canonical config
  identity; they are never persisted in the plan.
- Safety rejection: terminal failure; never bypass policy through another
  provider.
- Rabbit down after create: the batch remains durable and undispatched;
  ordinary source recovery publishes after Rabbit returns.
- Redis down or rebuilding: no new claim starts and there is no legacy
  fallback. Recovery reconstructs active tenants, pending broker-backed work,
  and valid reservations from MySQL/Rabbit state.
- Object store down or validation failure: the task cannot become `DONE`.
- MySQL down before create: no row and no Rabbit orphan. MySQL down after a
  manifest: a later fenced reclaim salvages the manifest.
- Duplicate/stale delivery: exact claim returns terminal/stale and ACKs without
  a provider call. Poison/mismatched locators execute no work; only independently
  verified canonical candidates may be generation-repaired before confirmed
  DLQ publication and ACK.

## Generic administrative recovery

`image.generate` is registered for all three generic commands. Every command
is a read-only dry-run without `--apply`. Save dry-run output and external
attestations. Resume an interrupted apply by rerunning the same kind and
parameters; do not edit Redis control or the MySQL operation journal manually.

### RabbitMQ durable-data loss

First fence and isolate the old broker and stop every publisher for
`image.generate`.

```bash
bkcrab admin fairqueue rabbit-disaster-repair --resource image.generate
bkcrab admin fairqueue rabbit-disaster-repair --resource image.generate \
  --apply --confirm-old-broker-isolated --confirm-publishers-paused
```

Apply enters `RECOVERING`, captures a bounded MySQL sequence high-water, and
advances only nonterminal, noncanceled publish obligations using their
original guards. It performs zero direct publishes. After repair and the
stable-token zero-difference rebuild, the ordinary dispatcher republishes and
the journal reaches `COMPLETED`.

### Authoritative writer replacement

Physically fence the old writer, stop every image runtime and recovery
coordinator, prove the new writer/schema authoritative and valid `RUNNING=0`,
then use the exact old database-bound fingerprint.

```bash
bkcrab admin fairqueue rebind-writer --resource image.generate \
  --expected-old-writer-fingerprint <64-lowercase-hex>
bkcrab admin fairqueue rebind-writer --resource image.generate --apply \
  --expected-old-writer-fingerprint <64-lowercase-hex> \
  --confirm-old-writer-fenced --confirm-resource-runtimes-stopped \
  --confirm-new-writer-authoritative
```

Keep all image runtime components stopped from the first apply until the
matching journal is `COMPLETED`. Connection/session or fingerprint changes
fail closed and require operator reconciliation; do not force traffic open.

### Redis coordination loss or corruption

With authoritative MySQL and Rabbit verified, stop image runtimes and wait the
full recovery drain window. The command deletes only the registered
`{image.generate}` coordination namespace.

```bash
bkcrab admin fairqueue redis-force-rebuild --resource image.generate
bkcrab admin fairqueue redis-force-rebuild --resource image.generate \
  --apply --confirm-discard-redis-coordination-state
```

A total Redis loss does not erase an unfinished special-operation intent,
because its phase and progress are journaled in MySQL. An `ACTIVE` or
unmatched `READY_COMMITTED` record stays operator-required. Completion is
allowed only when the Redis `last_completed_operation_id` exactly matches the
MySQL journal. A writable standalone Redis primary is required; Cluster mode
is rejected.

## Release evidence

Before widening traffic, retain test/metric evidence for all of the following:

- batch 16/task 4 hard limits and deterministic `A5/B3/C1 -> A4,A1,B3,C1`;
- one user borrowing four idle slots, the next slot going to a waiting user,
  backlog convergence to 2/2, and no starvation with three users;
- total valid MySQL `RUNNING <= 4`, including two runtimes and missing Redis
  reservations;
- provider success/fallback/safety stop, exact-count enforcement, and physical
  provider concurrency;
- manifest-last recovery and provider-call-once salvage across a generation
  jump;
- `PARTIAL`, retained successful artifacts, idempotent cancel, cancel fence,
  and expired-cancel convergence;
- Rabbit, Redis, MySQL, and object-store recovery; and
- a security scan proving no image bytes/base64, credentials, Authorization
  headers, or signed URLs in queue/control payloads, stored provider plans,
  tool text, or logs.

The implementation acceptance run on 2026-08-04 passed the env-gated real
MySQL image store and capacity tests, the unique-namespace Rabbit/Redis probe,
LocalFS manifest/salvage tests, provider fallback and limiter tests, tool and
gateway regressions, 33 frontend tests, frontend lint with no errors, the
production frontend build, and full `go test ./...`, `go vet ./...`, and
`go build ./...` in an isolated release-layout copy under `E:\gotmp`. The
source worktree was not populated with generated Web or Go build artifacts.
