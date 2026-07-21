# Database

BkCrab uses MySQL by default and does not fall back to SQLite when the MySQL
configuration is missing or unavailable.

Required environment variables:

```bash
BKCRAB_STORAGE_TYPE=mysql
BKCRAB_STORAGE_DSN='bkcrab:password@tcp(mysql.example.com:3306)/bkcrab?parseTime=true&loc=UTC&charset=utf8mb4'
BKCRAB_STORAGE_AUTO_MIGRATE=true
```

`parseTime=true` is enforced by the application. Configure `tls=true` or a
registered MySQL TLS profile for managed production databases.

PostgreSQL and SQLite providers remain available for compatibility and tests,
but they must be selected explicitly. An empty storage type always means
MySQL, and an empty MySQL DSN is a startup error.

To copy a legacy SQLite database into MySQL:

```bash
bkcrab-migrate-storage \
  --sqlite /path/to/bkcrab.db \
  --mysql 'bkcrab:password@tcp(mysql.example.com:3306)/bkcrab?parseTime=true' \
  --replace
```

## RAG schema

RAG keeps ownership metadata, source-document state, and durable indexing tasks
in the primary relational database:

| Table | Purpose |
|---|---|
| `rag_kbs` | User-owned knowledge bases and their immutable embedding snapshot |
| `rag_chat_turns` | Persisted simple knowledge-base question/answer turns and citation snapshots |
| `rag_documents` | Uploaded source files, target version, and active retrieval version |
| `rag_index_tasks` | Durable asynchronous indexing and restart-recovery state |
| `rag_document_versions` | Immutable parse/index/config snapshots and version result state |
| `rag_chunks` | Authoritative versioned chunk payload and location catalog |
| `rag_assets` | Content-addressed, insert-only binary/display resource catalog |
| `rag_version_assets` | Exact-version pins that keep decorative or unchunked assets alive |
| `rag_chunk_assets` | Versioned chunk-to-asset occurrences, captions, and OCR text |
| `rag_cache_objects` | Durable per-object page/image/enrichment cache catalog and CAS generation |
| `rag_cache_object_fingerprints` | Many-to-many cache object associations to immutable parse/index fingerprints |
| `rag_object_write_staging` | Generation-fenced write-ahead handles for every externally stored RAG object |
| `rag_document_maintenance_leases` | Database-clock document fences for orphan and cache cleanup |
| `rag_index_gc_tasks` | Delayed cleanup work for one exact retired document version |
| `rag_document_ai_task_budgets` | Lockable per-index-task DocumentAI aggregate budget |
| `rag_document_ai_user_budgets` | Lockable per-user UTC-period DocumentAI aggregate budget |
| `rag_document_ai_usage` | Durable idempotency, reservation, and settlement ledger |

Original files, parse artifacts, and asset binaries are stored in the configured
object store. The primary database is authoritative for chunk text and metadata;
Milvus stores embeddings plus retrieval identifiers and is not the payload source
for newly indexed documents.

The types below are shown as SQLite/PostgreSQL and MySQL when they differ.
MySQL uses `DATETIME(6)` for timestamps; SQLite/PostgreSQL use `TIMESTAMP`.
Relationships are enforced by the application rather than foreign-key
constraints so that cleanup can be coordinated with the object and vector
stores.

### `rag_kbs`

| Column | Type | Meaning |
|---|---|---|
| `id` | `TEXT` / `VARCHAR(120)` | Knowledge-base ID and primary key |
| `user_id` | `TEXT` / `VARCHAR(120)` | Owning user ID |
| `name` | `TEXT` / `VARCHAR(191)` | User-visible name |
| `description` | `TEXT` / `LONGTEXT` | Optional user-visible description; empty by default |
| `embed_provider` | `TEXT` / `VARCHAR(64)` | Embedding configuration source captured at creation; defaults to `system` |
| `embed_model` | `TEXT` / `VARCHAR(191)` | Embedding model captured at creation |
| `embed_dims` | `INTEGER` | Embedding dimensions captured at creation |
| `chunk_size` | `INTEGER` | Target chunk size; defaults to 512 estimated tokens |
| `chunk_overlap` | `INTEGER` | Chunk overlap; defaults to 64 estimated tokens |
| `parse_mode` | `TEXT` / `VARCHAR(16)` | Requested parse mode: `standard` or `auto`; defaults to `standard` |
| `enrichment_enabled` | `BOOLEAN` | Whether text enrichment is requested; defaults to `false` |
| `status` | `TEXT` / `VARCHAR(32)` | Lifecycle state: `provisioning`, `active`, or `deleting` |
| `provisioning_generation` | `BIGINT` | Monotonic fence generation for collection creation; defaults to 0 |
| `provisioning_lease_owner` | `TEXT` / `VARCHAR(96)` | Current collection-provisioning worker; empty outside provisioning |
| `provisioning_lease_until` | timestamp, nullable | Database-time deadline for the in-flight external collection operation |
| `created_at` | timestamp | Creation time |
| `updated_at` | timestamp | Last metadata update time |

`embed_provider`, `embed_model`, and `embed_dims` form an immutable
snapshot. Metadata updates do not alter them, which prevents vectors with
different models or dimensions from being mixed in one Milvus collection.
Collection creation first inserts a fail-closed `provisioning` row and then
heartbeats its generation-fenced lease while Milvus is initialized. Activation
and abort are compare-and-set transitions. A tombstone stops heartbeats;
cleanup waits for the lease to quiesce, and an expired lease is converted to a
durable `deleting` handle by the lifecycle worker. Thus request cancellation,
process failure, and user deletion cannot leave an untracked collection.

Index:

- `idx_rag_kbs_user (user_id)` supports per-user knowledge-base listing and
  quota checks.
- `idx_rag_kbs_provisioning (status, provisioning_lease_until, updated_at)`
  supports bounded crash-recovery scans.

### `rag_chat_turns`

Each row is one completed turn in the simple knowledge-base chat. Sessions are
derived by grouping rows by `(user_id, kb_id, session_id)`, so no separate
session table is required.

| Column | Type | Meaning |
|---|---|---|
| `id` | `TEXT` / `VARCHAR(120)` | Turn ID and primary key |
| `user_id` | `TEXT` / `VARCHAR(120)` | User who owns this private chat history |
| `kb_id` | `TEXT` / `VARCHAR(120)` | Knowledge base used for retrieval |
| `session_id` | `TEXT` / `VARCHAR(120)` | Logical chat thread ID |
| `title` | `TEXT` / `VARCHAR(255)` | Stable title derived from the first question |
| `question` | `TEXT` / `LONGTEXT` | User question |
| `answer` | `TEXT` / `LONGTEXT` | LLM answer |
| `sources` | `TEXT` / `LONGTEXT` | JSON snapshot of the retrieval hits cited by the answer |
| `created_at` | timestamp | Turn completion time |

Indexes:

- `idx_rag_chat_sessions (user_id, kb_id, created_at)` supports recent-session listing.
- `idx_rag_chat_turns_session (user_id, kb_id, session_id, created_at)` supports ordered turn loading.

### `rag_documents`

| Column | Type | Meaning |
|---|---|---|
| `id` | `TEXT` / `VARCHAR(120)` | Document ID and primary key |
| `kb_id` | `TEXT` / `VARCHAR(120)` | Logical reference to `rag_kbs.id` |
| `file_name` | `TEXT` / `VARCHAR(255)` | Original file name |
| `file_type` | `TEXT` / `VARCHAR(32)` | Parsed type: `md`, `txt`, `pdf`, or `docx` |
| `file_size` | `BIGINT` | Original file size in bytes; defaults to 0 |
| `object_key` | `TEXT` | Key of the original file in object storage |
| `status` | `TEXT` / `VARCHAR(32)` | Index/lifecycle state: `PENDING`, `PROCESSING`, `DONE`, `FAILED`, or `DELETING` |
| `error_msg` | `TEXT` / `LONGTEXT` | Most recent indexing error; empty when no error is present |
| `chunk_count` | `INTEGER` | Number of chunks in the completed current index; defaults to 0 |
| `token_count` | `INTEGER` | Estimated token count in the completed current index; defaults to 0 |
| `version` | `BIGINT` | Highest allocated physical index version/current target |
| `source_sha256` | `TEXT` / `CHAR(64)` | SHA-256 of the uploaded source; empty only for migrated legacy rows |
| `active_version` | `BIGINT` | Retrieval-visible version, or 0 when no complete version is available |
| `index_format_version` | `SMALLINT` | 0 for pinned legacy payloads, 1 for the SQL catalog format |
| `processing_stage` | `TEXT` / `VARCHAR(24)` | Latest target stage; defaults to `queued` |
| `progress_current` | `INTEGER` | Completed units in the latest target stage |
| `progress_total` | `INTEGER` | Total units in the latest target stage |
| `progress_unit` | `TEXT` / `VARCHAR(24)` | Unit label for progress counters |
| `degraded` | `BOOLEAN` | Whether the latest target completed with degraded output |
| `warning_count` | `INTEGER` | Number of warnings for the latest target |
| `uploaded_at` | timestamp | Upload time |
| `indexed_at` | nullable timestamp | Time the latest target completed indexing |

`version` and `active_version` are deliberately separate: a failed replacement
does not hide a previously active index. `active_version=0` never means
"accept any legacy version". During migration, an old `DONE` row is pinned to
its old version with `index_format_version=0` and a synthetic `legacy-v0`
version snapshot; other legacy rows remain inactive until rebuilt.

Index:

- `idx_rag_documents_kb (kb_id)` supports document listing and knowledge-base
  cleanup.

### `rag_index_tasks`

| Column | Type | Meaning |
|---|---|---|
| `id` | SQLite `INTEGER AUTOINCREMENT`; PostgreSQL `BIGSERIAL`; MySQL `BIGINT AUTO_INCREMENT` | Task ID and primary key |
| `doc_id` | `TEXT` / `VARCHAR(120)` | Logical reference to `rag_documents.id` |
| `doc_version` | `BIGINT` | Physical fencing epoch owned by this attempt |
| `status` | `TEXT` / `VARCHAR(32)` | Task state: `PENDING`, `RUNNING`, `DONE`, `FAILED`, or `SUPERSEDED` |
| `retry_count` | `INTEGER` | Transient failures or lease expiries that caused another execution; defaults to 0 |
| `max_retry` | `INTEGER` | Retry limit; defaults to 3 |
| `claim_generation` | `BIGINT` | Fencing nonce incremented on every successful claim/reclaim; not a retry counter |
| `lease_owner` | `TEXT` / `VARCHAR(96)` | Stable worker ID holding the current lease |
| `lease_until` | nullable timestamp | Database-time lease deadline |
| `heartbeat_at` | nullable timestamp | Latest successful fenced heartbeat |
| `next_run_at` | nullable timestamp | Database-time retry eligibility deadline |
| `error_msg` | `TEXT` / `LONGTEXT` | Most recent task error; empty when no error is present |
| `created_at` | timestamp | Enqueue time |
| `started_at` | nullable timestamp | Most recent transition to `RUNNING` |
| `finished_at` | nullable timestamp | Terminal transition time for `DONE`, `FAILED`, or `SUPERSEDED` |

Workers claim with a database compare-and-set. `PENDING` rows whose
`next_run_at` is due and `RUNNING` rows whose lease expired are continuously
polled; an in-memory channel is only a wake hint. Claim, heartbeat, retry,
activation, and finish use database time. Every worker mutation must match
`task_id + doc_version + claim_generation + lease_owner`, a `RUNNING` state,
and an unexpired lease. Reclaim allocates a never-used `doc_version` and
terminalizes the old physical version, so late writes cannot activate or
overwrite the replacement. The index task intentionally has no
`attempt_count`; retries use `retry_count`, while `claim_generation` is only a
fencing nonce.

A fenced transient failure atomically marks the current physical version
`FAILED`, returns the task to `PENDING`, increments `retry_count`, and sets a
database-time `next_run_at`; the next claim copies the immutable snapshot into
a newly allocated physical version. A provider-fingerprint mismatch is not a
retry: one fenced transaction supersedes the old task/version and creates a new
task with a newly supplied complete snapshot, so an endpoint/model change can
never continue writing the old version.

Successful publication has exactly one commit point. `ActivateAndFinishRAGIndexTask`
checks the complete unexpired fence and `rag_documents.version`, then in one SQL
transaction sets the new version and task `DONE`, switches `active_version`,
sets `index_format_version=1`, retires the previous active version, and inserts
that exact version's delayed GC task. A failed replacement therefore leaves the
old `active_version` searchable, while GC failure can only retry cleanup and
cannot rerun parsing, DocumentAI, or embedding.

Indexes:

- `idx_rag_tasks_status (status, created_at)` supports ordered recovery of
  old installations.
- `idx_rag_index_tasks_runnable (status, next_run_at, lease_until, created_at)`
  supports durable claim polling.
- unique `(doc_id, doc_version)` prevents two logical tasks from owning the
  same physical fencing epoch.

### `rag_document_versions`

The primary key is `(doc_id, doc_version)`, where `doc_version` is `BIGINT` in
every supported database. Snapshot fields are immutable after insert; only the
status/result fields are changed, and worker changes must be protected by the
index-task fence.

| Column group | Columns | Meaning |
|---|---|---|
| Identity/result | `doc_id`, `doc_version`, `status`, `parse_artifact_key`, `page_count`, `asset_count`, `degraded`, `warning_count` | Physical version and its result |
| Source/split | `source_sha256`, `parse_mode`, `chunk_size`, `chunk_overlap`, `parser_version`, `splitter_version`, `parse_fingerprint`, `index_fingerprint` | Reproducible source and parser snapshot |
| Vision | `vision_model`, `vision_provider_fingerprint`, `vision_prompt_version` | Vision provider/model/prompt snapshot |
| Enrichment | `text_model`, `text_provider_fingerprint`, `enrichment_prompt_version`, `enrichment_enabled` | Text enrichment snapshot |
| DocumentAI limits | `max_document_ai_requests`, `max_document_ai_tokens`, `max_document_ai_cost_microusd` | Per-document request, token, and integer micro-USD caps |
| Embedding | `embedding_provider`, `embedding_model`, `embedding_dimensions`, `embedding_contract_fingerprint` | KB creation-time embedding contract |
| Timestamps | `created_at`, `updated_at` | Creation and latest result transition |

Version states are `PENDING`, `RUNNING`, `DONE`, `RETIRED`, `GCED`, `FAILED`,
and `SUPERSEDED`. A migrated active legacy version uses the `legacy-v0`
sentinel in its parser/fingerprint fields and has enrichment disabled.

### `rag_chunks`

| Column | Type | Meaning |
|---|---|---|
| `kb_id`, `doc_id` | text ID | Owning knowledge base and document |
| `doc_version` | `BIGINT` | Exact physical index version |
| `chunk_index` | `INTEGER` | Stable chunk index within the version |
| `section_title` | text | Section title |
| `location_json` | text | Canonical source-location JSON |
| `raw_content` | text | Authoritative unenhanced chunk text |
| `enhancement` | text | Optional versioned enrichment |
| `search_content` | text | Bounded text embedded and searched |
| `token_count` | `INTEGER` | Estimated tokens |
| `created_at` | timestamp | Catalog write time |

The primary key is `(doc_id, doc_version, chunk_index)`. The
`idx_rag_chunks_lookup (kb_id, doc_id, doc_version)` index supports retrieval
hydration. Bulk APIs use bounded batches and bind every value.

### `rag_assets`

`rag_assets` is content-addressed per document and insert-only for binary and
display fields. Repeated sightings only expand the `first_seen_version` /
`last_seen_version` range; captions, OCR, occurrence, and model semantics do
not belong here.

| Column group | Columns | Meaning |
|---|---|---|
| Identity | `id`, `doc_id`, `content_sha256` | Stable asset ID and per-document content identity; `(doc_id, content_sha256)` is unique |
| Source | `source_kind`, `source_mime`, `source_object_key`, `byte_size`, `width`, `height` | Original binary metadata and object key |
| Safe display | `display_mime`, `display_object_key`, `thumbnail_object_key`, `display_status`, `display_sha256` | Sanitized display derivatives |
| Version range | `first_seen_version`, `last_seen_version` (`BIGINT`) | Earliest/latest physical version referencing the content |
| Timestamps | `created_at`, `updated_at` | Creation and range-update times |

`idx_rag_assets_doc (doc_id)` supports document cleanup and artifact
rehydration. Internal object keys are never exposed by API DTOs.

### RAG cache catalog

`rag_cache_objects` has primary key `(doc_id, cache_kind, cache_key)` and stores
the exact object key, a monotonically increasing `generation`, and creation/
last-use timestamps. `rag_cache_object_fingerprints` associates an object with
one or more immutable `parse` or `index` fingerprints. Page and image entries
use parse fingerprints; enrichment entries use index fingerprints. Multiple
associations allow a content-addressed key shared with the active generation
to survive cleanup of an older staging generation.

Successful cache puts and validated hits refresh the catalog using database
time and advance the object generation. Lifecycle cleanup holds a document
maintenance lease, preserves active-version fingerprints, and selects expired
or over-quota unreferenced generations. It deletes one exact object, rechecks
the maintenance fence, then CAS-deletes the matching catalog generation. The
indexes `(doc_id, updated_at)` and
`(doc_id, fingerprint_kind, fingerprint, updated_at)` support bounded sweeps.

### Object write staging and version asset pins

Every original upload, normalized Markdown, parsed artifact, source asset, safe
display derivative, and thumbnail obtains a `rag_object_write_staging` row
before bytes are written to object storage. Each external write uses an
immutable physical creation key containing `versions/<doc_version>/`.
Artifacts/normalized Markdown belong to that exact document version. A
content-addressed asset keeps the key from its first successful creation epoch,
and later reindexes reuse the stable asset ID and object strictly read-only.
The row records the owning user/knowledge base/document, object kind and key,
publication reference, monotonic cleanup generation, and `WRITING`, `READY`,
`PUBLISHED`, or `DELETING` state. Publication changes the matching `READY` row
to retained `PUBLISHED` in the same SQL transaction that creates the durable
document, version, or asset reference. `PUBLISHED` is not a writer permit and
cannot be reopened by `BeginRAGObjectWrite`.

A lifecycle worker may claim only stale unreferenced rows. Cleanup changes the
row to `DELETING`, deletes that exact immutable key, and retains the tombstone.
The tombstone is periodically claimed and deleted again: this removes bytes
from a storage Put that completed after an earlier Delete acknowledgement.
Overlapping delayed Deletes can affect only their old physical generation, not
a currently published one. Staging-asset reclamation first changes all of the
asset's `PUBLISHED` rows to permanent `DELETING` tombstones and removes its SQL
catalog/pins in one maintenance-fenced transaction; only then does it issue
external Deletes. If the same content appears later, it receives a new creation
key. Document, knowledge-base, and user finalizers delete `PUBLISHED` objects,
but leave unacknowledged `WRITING`, `READY`, or `DELETING` rows for the global
re-sweep even after the ownership rows are gone.

`rag_version_assets` has primary key `(doc_id, doc_version, asset_id)` and pins
all assets emitted by one immutable parsed version, including decorative
images that are not attached to a text chunk. Exact-version GC first removes
the version's chunk occurrences and pins, then deletes a binary asset only
when no active or retained version references it.

`rag_document_maintenance_leases` stores one database-clock generation fence
per document. Cache pruning and orphan cleanup acquire this lease only after
index workers have quiesced; enqueue, publication, and finalization take the
same ownership lock hierarchy and therefore fail closed against an active
cleanup generation.

### `rag_chunk_assets`

Each row maps one asset occurrence into one exact chunk version. The primary
key is `(doc_id, doc_version, chunk_index, asset_id, ordinal)`; fields
`location_json`, `caption`, and `ocr_text` are versioned so an old worker cannot
overwrite the active version's semantics. Index
`idx_rag_chunk_assets_lookup (doc_id, doc_version, chunk_index, ordinal)`
supports bounded hydration by retrieval refs.

### `rag_index_gc_tasks`

GC work is separate from index work and is uniquely identified by
`(doc_id, retired_version)`. `retired_version` is `BIGINT`; `retired_at` and
`not_before` enforce the grace period. `status`, `claim_generation`,
`lease_owner`, `lease_until`, `heartbeat_at`, `attempt_count`, and `next_run_at`
provide delayed retry/lease state. GC always deletes one exact version and
must never advance `rag_documents.version`.

Index `idx_rag_index_gc_tasks_runnable (status, next_run_at, lease_until,
created_at)` supports delayed claims. A completed GC leaves the version row as
a `GCED` tombstone until document-level deletion or a later orphan sweep.

GC task states are `PENDING`, `RUNNING`, `DONE`, and `FAILED`. Claim and
heartbeat compare `id + retired_version + claim_generation + lease_owner` and
an unexpired lease. Unlike index-task reclaim, GC reclaim never allocates or
advances a document version. The vector and SQL catalog delete predicates are
always the exact `(doc_id, retired_version)` pair.

### DocumentAI budget and usage tables

`rag_document_ai_task_budgets` has primary key `task_id` and stores `user_id`,
the task caps (`max_requests`, `max_tokens`, `max_cost_microusd`), charged
aggregates, and `updated_at`. `rag_document_ai_user_budgets` has primary key
`(user_id, period_start_utc)` and stores the same charged aggregates for one
UTC date. Costs are integer micro-USD (`BIGINT`), never floating point.

`rag_document_ai_usage` has primary key `idempotency_key` and records:

- logical request, user/document/task/version, claim generation, and lease owner;
- operation and provider fingerprint;
- UTC period, reserved/actual input and output tokens, and estimated micro-USD;
- `RESERVED`, `SENT`, `COMMITTED`, `RELEASED`, or `OVERRUN` state;
- reservation expiry, send time, estimated-usage marker, and timestamps.

Indexes `(user_id, period_start_utc, provider_fingerprint)` and
`(task_id, logical_request_key)` support quota accounting and idempotency
inspection. Reservation and send-gate transactions lock the user-period
aggregate, task aggregate, current index task, and usage row in that fixed
order. PostgreSQL/MySQL use `SELECT FOR UPDATE`; SQLite uses `BEGIN IMMEDIATE`.
An unlocked `SUM + INSERT` quota check is not safe. Only
`RESERVED -> SENT -> COMMITTED/OVERRUN` and `RESERVED -> RELEASED` are legal.
An expired fence cannot reserve or enter `SENT`; a late response may settle its
own already-`SENT` idempotency key without changing task/version state.

### RAG deletion and orphan retention

Document and knowledge-base deletion is a two-phase operation. The first SQL
transaction writes `DELETING`, clears retrieval visibility, and supersedes
runnable index work. Search, claim, owner asset authorization, and Agent
session-scoped asset authorization all treat the tombstone as absent before
ETag/object access. Vector, object-store, chunk, and asset cleanup then runs
idempotently; a failure retains the tombstone so a later worker or repeated
request can resume instead of exposing a partially deleted resource.

User deletion uses the same rule through an injected RAG cleaner. An account is
marked `deleting` before its knowledge bases are cleaned and is removed from
the user table only after every external/catalog cleanup succeeds. If the user
owns RAG data and no cleaner is installed, deletion is refused. DocumentAI
budget and usage rows are not reset while the user identity is still valid.

The orphan sweep uses `stagingArtifactTTL` and rechecks current references
before removing an asset or artifact. Assets referenced by an active version,
knowledge-base chat source snapshot, or Agent assistant `ragResources` snapshot
remain until document deletion. FAILED, SUPERSEDED, and GCED vector/chunk data
is different: once it is not the active version, the sweep may repeatedly issue
an exact-version delete so a late external upsert cannot survive indefinitely.
The GCED version row itself remains until whole-document cleanup. Likewise,
`rag_object_write_staging` rows in `DELETING` are durable cleanup tombstones,
not a backlog that should be manually truncated; their timestamps and cleanup
generations should continue advancing during a healthy lifecycle sweep.

See [rag-document-ai.md](rag-document-ai.md) for feature gates, durable budget
operations, telemetry, degradation, and incident-response guidance.

### Migration compatibility

RAG migrations are restart-safe and can run twice. They add missing columns
for an existing installation rather than relying only on `CREATE TABLE IF NOT
EXISTS`; SQLite rebuilds `rag_documents` to make `version` a true `BIGINT`,
while PostgreSQL and MySQL alter it in place. Existing chat `sources` JSON is
left byte-for-byte unchanged.

### Index-task migration maintenance window

The legacy index-task backfill is an offline expand/backfill/validate/contract
migration. Old and new indexing workers must never run together; this is not a
rolling-worker migration.

1. Disable upload and reindex entry points. Stop or scale every old indexing
   worker to zero, while leaving read-only retrieval available.
2. Wait longer than the maximum lease used by the old release. Confirm that
   task heartbeat values no longer change, then take the normal database
   backup/snapshot.
3. Start the new release with upload/reindex and indexing workers still
   disabled. Its base migration performs only the nullable expand step. After
   object storage and secret-free parser/provider configuration are available,
   run exactly one gateway instance with
   `BKCRAB_RAG_LEGACY_TASK_MIGRATION_MODE=offline-v1` to execute the runtime
   `SnapshotBuilder` backfill and contract step. The exact acknowledgement is
   intentionally separate from `BKCRAB_STORAGE_AUTO_MIGRATE`; without it, a
   database containing legacy task rows fails startup before changing those
   rows. Unset the acknowledgement immediately after the contract succeeds.
4. The backfill removes historical terminal task rows, keeps only the newest
   `(created_at,id)` non-terminal task per document, rejects orphans, allocates
   each survivor a fresh version greater than every known/active version, and
   writes a complete immutable `PENDING` version snapshot. A snapshot build
   failure leaves no runnable row and marks the document as needing reindex.
5. Before restoring traffic, verify that the following queries return no rows:

   ```sql
   SELECT doc_id, COUNT(*)
   FROM rag_index_tasks
   WHERE status IN ('PENDING','RUNNING')
   GROUP BY doc_id HAVING COUNT(*) > 1;

   SELECT t.id
   FROM rag_index_tasks t
   LEFT JOIN rag_document_versions v
     ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
   WHERE t.status IN ('PENDING','RUNNING') AND v.doc_id IS NULL;

   SELECT id FROM rag_index_tasks WHERE doc_version IS NULL;
   ```

   Also verify the runnable and unique task indexes described above. The
   runtime migration is idempotent and may be rerun after a partial failure.
6. Stop the one-off migration instance or restart it without the migration
   acknowledgement. Start only new-release workers, observe claims/heartbeats, and then restore
   upload/reindex entry points. Do not roll back by restarting an old worker
   after contract; restore the database backup or deploy a compatible forward
   fix instead.
