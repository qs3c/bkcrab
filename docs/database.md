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
| `rag_documents` | Uploaded source files and the current version of their index |
| `rag_index_tasks` | Durable asynchronous indexing and restart-recovery state |

Original file bytes are stored in the configured object store. Chunk text,
vectors, and chunk-level metadata are stored in Milvus rather than duplicated
in these tables.

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
| `status` | `TEXT` / `VARCHAR(32)` | Lifecycle state: `active` or `deleting` |
| `created_at` | timestamp | Creation time |
| `updated_at` | timestamp | Last metadata update time |

`embed_provider`, `embed_model`, and `embed_dims` form an immutable
snapshot. Metadata updates do not alter them, which prevents vectors with
different models or dimensions from being mixed in one Milvus collection.

Index:

- `idx_rag_kbs_user (user_id)` supports per-user knowledge-base listing and
  quota checks.

### `rag_documents`

| Column | Type | Meaning |
|---|---|---|
| `id` | `TEXT` / `VARCHAR(120)` | Document ID and primary key |
| `kb_id` | `TEXT` / `VARCHAR(120)` | Logical reference to `rag_kbs.id` |
| `file_name` | `TEXT` / `VARCHAR(255)` | Original file name |
| `file_type` | `TEXT` / `VARCHAR(32)` | Parsed type: `md`, `txt`, `pdf`, or `docx` |
| `file_size` | `BIGINT` | Original file size in bytes; defaults to 0 |
| `object_key` | `TEXT` | Key of the original file in object storage |
| `status` | `TEXT` / `VARCHAR(32)` | Index state: `PENDING`, `PROCESSING`, `DONE`, or `FAILED` |
| `error_msg` | `TEXT` / `LONGTEXT` | Most recent indexing error; empty when no error is present |
| `chunk_count` | `INTEGER` | Number of chunks in the completed current index; defaults to 0 |
| `token_count` | `INTEGER` | Estimated token count in the completed current index; defaults to 0 |
| `version` | `INTEGER` | Current document-index version; starts at 1 and increments on reindex |
| `uploaded_at` | timestamp | Upload time |
| `indexed_at` | nullable timestamp | Time the current version completed indexing |

Chunks written to Milvus carry `doc_version`. The document `version`
therefore identifies stale chunks that can be removed after a replacement
index has been written successfully.

Index:

- `idx_rag_documents_kb (kb_id)` supports document listing and knowledge-base
  cleanup.

### `rag_index_tasks`

| Column | Type | Meaning |
|---|---|---|
| `id` | SQLite `INTEGER AUTOINCREMENT`; PostgreSQL `BIGSERIAL`; MySQL `BIGINT AUTO_INCREMENT` | Task ID and primary key |
| `doc_id` | `TEXT` / `VARCHAR(120)` | Logical reference to `rag_documents.id` |
| `status` | `TEXT` / `VARCHAR(32)` | Task state: `PENDING`, `RUNNING`, `DONE`, or `FAILED` |
| `retry_count` | `INTEGER` | Retries already attempted; defaults to 0 |
| `max_retry` | `INTEGER` | Retry limit; defaults to 3 |
| `error_msg` | `TEXT` / `LONGTEXT` | Most recent task error; empty when no error is present |
| `created_at` | timestamp | Enqueue time |
| `started_at` | nullable timestamp | Most recent transition to `RUNNING` |
| `finished_at` | nullable timestamp | Terminal transition time for `DONE` or `FAILED` |

At process startup, both `PENDING` and `RUNNING` rows are selected for
re-enqueueing. Treating a leftover `RUNNING` row as runnable is the
crash-recovery mechanism for work interrupted after it was claimed.

Index:

- `idx_rag_tasks_status (status, created_at)` supports ordered recovery of
  runnable tasks.
