# Multimodal document RAG and DocumentAI operations

bkcrab's multimodal document RAG uses visual models only while indexing. Search,
reranking, knowledge-base answers, and Agent `rag_search` remain text-only. A
retrieval hit may carry a stable `AssetRef`; the Web UI loads the corresponding
safe raster through a separately authenticated endpoint. Asset IDs, object
keys, URLs, base64, and image bytes are never added to answer-model messages.

## Capability and disclosure model

Three release gates are independent and default to `false`:

| Environment variable | Capability |
|---|---|
| `BKCRAB_RAG_ADVANCED_ENABLED` | Visual DocumentAI calls for PDF pages and Office images |
| `BKCRAB_RAG_OFFICE_ENABLED` | DOCX/PPTX/XLSX conversion by the parser sidecar |
| `BKCRAB_RAG_ENRICHMENT_ENABLED` | Text-model enrichment of table/code chunks |

An administrator must also configure the corresponding models and a healthy
sidecar. A knowledge base then opts into `parseMode=auto` and, independently,
`enrichmentEnabled=true`. The UI discloses that pages/images or table/code text
may be sent to the administrator-selected DocumentAI endpoint before either
option is enabled. Existing knowledge bases migrate as `standard` with
enrichment disabled.

`GET /api/rag/capabilities` is the runtime source of truth. It reports separate
`pdfAuto`, `office`, `officeVision`, and `enrichment` availability, reasons,
per-extension upload limits, and document-level request/token/cost budgets.
Handlers consume a background TTL health snapshot; they do not probe the
sidecar in a user request. PDF capability failure does not disable Office.

## Required configuration

The ingestion-only provider is separate from chat/answer providers:

```text
BKCRAB_RAG_DOCUMENT_AI_API_TYPE=openai-compatible
BKCRAB_RAG_DOCUMENT_AI_ENDPOINT=https://document-ai.example.com/v1
BKCRAB_RAG_DOCUMENT_AI_API_KEY=...
BKCRAB_RAG_DOCUMENT_AI_VISION_MODEL=vision-model
BKCRAB_RAG_DOCUMENT_AI_TEXT_MODEL=text-model
BKCRAB_RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS=document-ai.example.com
BKCRAB_RAG_DOCUMENT_AI_ALLOW_PRIVATE_ENDPOINT=false
BKCRAB_RAG_DOCUMENT_AI_TIMEOUT_MS=120000
BKCRAB_RAG_DOCUMENT_AI_VISION_CONCURRENCY=2
BKCRAB_RAG_DOCUMENT_AI_ENRICHMENT_CONCURRENCY=4
```

Only `openai-compatible` is supported in v1. HTTPS is required unless an
administrator explicitly permits a private endpoint. The client rejects
redirects, checks the configured scheme/host and resolved address, limits the
encoded visual request and compressed/decompressed response, caps output
tokens and JSON depth, and strictly decodes typed responses. Document content
cannot select or modify the endpoint. Never put the API key in a Compose file,
ConfigMap, log field, test fixture, or support bundle.

The local parser endpoint is configured separately:

```text
BKCRAB_RAG_PARSER_ENDPOINT=http://rag-parser:8080
BKCRAB_RAG_PARSER_TIMEOUT_MS=600000
```

The parser receives only source bytes and an allowlisted format. It has no
DocumentAI, embedding, object-store, or database credential. Deployment must
run it as non-root with a read-only root filesystem, bounded tmpfs/resources,
no privilege escalation, and no egress. `/healthz` reports the
`rag-parser/v1` protocol, effective input/output limits, Office golden gates,
and the approved PDF engine. The current PDF adapter is
`pypdfium2==5.12.1`; its license and image-distribution obligations are recorded
in `services/rag-parser/docs/pdf-engine-adr.md`.

## Limits and durable accounting

Important limits use the `BKCRAB_RAG_LIMITS_` prefix. Defaults include 300
DocumentAI requests, 200,000 tokens, and USD 1.00 estimated cost per logical
indexing task; per-user UTC-day defaults are 1,000 requests, 2,000,000 tokens,
and USD 10.00. Page, visual asset, enrichment block, response-byte, JSON-depth,
image-pixel, and input-byte limits apply in addition. Consult
`internal/config/config.go` for the complete canonical list and defaults.

Every outbound Vision, Office-image, repair, retry, or Enricher attempt shares
one fenced task budget. The durable state machine is:

```text
RESERVED -> SENT -> COMMITTED
                 -> OVERRUN
RESERVED -> RELEASED
```

Reservation locks the user-period aggregate, then task aggregate, then current
index-task fence. `MarkSent` revalidates the fence immediately before network
I/O. Only a request known not to have been sent can be released. A sent request
that times out is settled conservatively; reported actual usage above the
reservation becomes `OVERRUN`. Lease reclaim, reindex, and resource deletion do
not reset either aggregate. Costs are stored as integer micro-USD.

Operators can inspect the tables documented in [database.md](database.md):

- `rag_document_ai_task_budgets` for one logical task;
- `rag_document_ai_user_budgets` for the current UTC period;
- `rag_document_ai_usage` for idempotent attempt state and estimated/actual
  settlement.

## Parsing and degradation

The common pipeline is source load, parse/artifact cache, Markdown safety
normalization, asset persistence, AST split, optional enrichment, embedding,
SQL/vector staging, and one fenced active-version switch. Progress stages are
`queued`, `loading`, `parsing`, `vision`, `chunking`, `enriching`, `embedding`,
`indexing`, `finalizing`, `done`, and `failed`.

- Standard PDF uses bounded native text extraction. Auto PDF analyzes each
  page, routes only complex pages to Vision, and falls back per page to native
  Markdown. A sidecar-wide outage falls back to native text and marks the
  result degraded; a document with no usable content fails.
- Modern Office always uses the sidecar. Standard mode retains extracted images
  with alt/neutral text but makes no Vision call. Auto mode transcribes images;
  one failed or over-budget image keeps the source asset and degrades only that
  occurrence. A sidecar/converter failure is explicit and does not invoke the
  legacy DOCX parser.
- Table/code enrichment requires both gates and KB opt-in. Failure or exhausted
  budget preserves the exact raw table/code and records a warning.
- Markdown/TXT never fetch document-authored images. External, relative,
  `data:`, HTML, CSS, local, and forged internal image references become inert
  placeholders/warnings.

All parser outputs pass the same Go Markdown normalizer before artifact
publication. Raw HTML and dangerous schemes cannot become browser requests;
the Web renderer is a second defense and does not enable raw HTML.

## Privacy-safe observability

The telemetry boundary accepts only closed event names, bounded token-like
dimensions, counters, durations, and usage amounts. Current events are:

```text
rag.parser.document
rag.parser.pages
rag.parser.sidecar_call
rag.result_cache
rag.document_ai.budget
rag.document_ai.call
rag.enrichment.batch
rag.index_task
rag.active_version_switch
rag.lifecycle.gc
```

Useful dimensions include `doc_id`, task/version/generation, format, parse mode,
operation, outcome/error code, cache kind/status, native/VLM/degraded page
counts, asset/decorative/warning/skipped counts, duration, request count,
input/output tokens, micro-USD cost, and `usage_estimated`. Default structured
telemetry logs at debug level; a deployment can inject a recorder that maps the
same events to counters and histograms.

The telemetry type cannot represent document Markdown/body, captions, OCR,
image bytes/base64, object keys, endpoints, provider response bodies, API keys,
DSNs, or arbitrary fields. Do not add any of those as labels: they create both
data exposure and unbounded-cardinality risk.

Recommended alerts:

- sustained parser-sidecar errors or stale health snapshots;
- elevated degraded-page/warning ratio by format and parser version;
- quota/rate-limit rejection or `OVERRUN` growth;
- SENT reservations settled with estimated usage;
- index lease reclaim/retry spikes;
- growing delayed-GC/deleting backlog;
- active-version activation failures.

## Lifecycle and incident response

Deleting a document, knowledge base, or user first writes a durable `DELETING`
tombstone. Search, task claim, owner asset access, and Agent session asset access
fail closed immediately, even if the vector/object bytes still exist. External
cleanup is idempotent and retryable. Retired index cleanup is leased separately
from indexing and deletes exactly `(doc_id, doc_version)` after its own grace
period; a `GCED` version tombstone remains available for later orphan sweeps.

For a provider incident, disable the advanced and/or enrichment feature gate;
standard retrieval and already-active indexes remain usable. For a parser
incident, check `/healthz`, protocol version, effective limits, tmpfs/resource
pressure, and network policies before restarting it. Do not release or edit
`SENT` usage rows manually. For a cleanup backlog, inspect tombstones and GC
leases before touching vectors or objects; never run a range delete in place of
exact-version GC. Do not remove `rag_object_write_staging` rows in `DELETING`:
they deliberately re-delete immutable physical creation keys so a storage Put
that acknowledges late cannot recreate an unreachable object. A `PUBLISHED`
row is a read-only registry, never permission to repair or overwrite that key.
Asset cleanup tombstones its old creation keys and removes the catalog before
external Delete; a later occurrence of the same content uses a new creation
epoch, so a delayed old Delete cannot remove the live object.

## Verification and release gates

The repository's legal corpus is generated by:

```bash
go run ./internal/rag/testdata/multimodal/generate.go \
  -output ./internal/rag/testdata/multimodal

cd services/rag-parser
uv run python tests/fixtures/generate_multimodal.py --output <temporary-directory>
```

It covers page/unit locations, GFM table/code, assets/occurrences, caption/OCR,
chunk boundaries, warning/degraded expectations, dangerous OOXML relationships,
raw HTML/schemes, prompt/tool/metadata injection, and oversized/deep JSON. All
material is repository-authored CC0 test data.

Real parser, Milvus, Vision, and PostgreSQL/MySQL budget tests remain explicitly
credential-gated. Do not enable an advanced capability until its format-specific
goldens, deterministic authorization tests, lifecycle/GC tests, deployment
network restrictions, and full Go/Python/Web suites pass.
