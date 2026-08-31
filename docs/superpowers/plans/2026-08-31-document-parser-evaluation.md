# Document Parser Evaluation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 bkcrab 中增加一套独立、仅超级管理员可用的文档解析评测平台，对上传的 `.docx/.pptx/.xlsx` 分别运行 MarkItDown 与 Anydoc，比较解析延迟，并用原文渲染页和双位置多模态裁判给最终标准化 Markdown 评分。

**Architecture:** 新增独立 `internal/parseeval` 领域，由一个可恢复、全局串行的 Go runner 编排 renderer、两个专用 parser sidecar 和现有多模态 provider。SQL 仅新增 `parser_eval_runs` 与 `parser_eval_documents` 两张表；原文、页面 PNG、完整 Markdown 和原始裁判响应继续使用现有 RAG object store。后台 API 和 `/admin/parser-evals/` 前端与旧 RAG 测评隔离，只复用鉴权、provider、parser、对象存储及通用 UI 基础设施。

**Tech Stack:** Go 1.x（`net/http`、`database/sql`、现有 RAG parser/provider/object store）、Python 3.12 + FastAPI + LibreOffice headless + PyMuPDF、Next.js 16 + React 19 + TypeScript、Docker Compose、SQLite/MySQL。

**Spec:** `docs/superpowers/specs/2026-08-31-document-parser-evaluation-design.md`

## Global Constraints

- 只新增两张业务表；attempt、judge、usage、aggregate 都写入两张表的封闭 JSON 槽位，不拆表。
- 评测输出必须来自现有 `parse.LocalParser` 的 Office standard 路径，并在 `ParsedDocument` 后、chunker 前停止；不得调用 DocumentAI、enrichment、chunk、Embedding、Milvus、检索或回答生成。
- 每次任务固定一个已配置、支持图片输入的裁判模型绑定；启动后冻结 provider/model/fingerprint，不回退到其他模型。
- 每个文档固定双 parser、双位置 judge；成功槽位不可被自动重试或人工重试覆盖。
- runner 只启一个 worker，parser 调用严格串行；不要为了全局信号量新增协调表。
- 所有 API（包括 artifact）使用现有 `requireSuperAdminSession` 门控，拒绝 admin API key 和 act-as 会话。
- 不修改或提交工作区中无关的 `resume/`。
- 每个任务都遵循 Red → Green → Refactor；先运行并看到目标测试失败，再写最小实现，再跑目标包测试。

---

## Task 1: 建立部署配置和封闭领域契约

**Files:**

- Create: `internal/config/parser_evaluation.go`
- Create: `internal/config/parser_evaluation_test.go`
- Modify: `internal/config/config.go`
- Modify: `internal/config/env.go`
- Create: `internal/parseeval/model.go`
- Create: `internal/parseeval/model_test.go`

- [ ] **Step 1: 写配置默认值、环境覆盖和校验失败测试**

  覆盖默认 endpoint、`enabled/workerEnabled` 显式布尔值、超时、50 文件、50 MiB 单文件、500 MiB 批次、90 天、6 页、100 DPI、40,000 Unicode 字符，以及非法 endpoint、非正超时、`maxFiles > 50`、`maxBatchBytes < maxFileBytes` 的拒绝。

  采用下列环境变量，不再引入同义配置名：

  ```text
  BKCRAB_PARSER_EVAL_ENABLED
  BKCRAB_PARSER_EVAL_WORKER_ENABLED
  BKCRAB_PARSER_EVAL_RENDERER_ENDPOINT
  BKCRAB_PARSER_EVAL_MARKITDOWN_ENDPOINT
  BKCRAB_PARSER_EVAL_ANYDOC_ENDPOINT
  BKCRAB_PARSER_EVAL_RENDER_TIMEOUT_MS
  BKCRAB_PARSER_EVAL_PARSE_TIMEOUT_MS
  BKCRAB_PARSER_EVAL_JUDGE_TIMEOUT_MS
  BKCRAB_PARSER_EVAL_MAX_FILE_BYTES
  BKCRAB_PARSER_EVAL_MAX_FILES
  BKCRAB_PARSER_EVAL_MAX_BATCH_BYTES
  BKCRAB_PARSER_EVAL_RETENTION_DAYS
  BKCRAB_PARSER_EVAL_MAX_PAGES
  BKCRAB_PARSER_EVAL_RENDER_DPI
  BKCRAB_PARSER_EVAL_MARKDOWN_JUDGE_CHARS
  ```

- [ ] **Step 2: 运行配置测试并确认失败**

  Run: `go test ./internal/config -run 'TestParserEvaluation|TestLoadEnvParserEvaluation'`

  Expected: FAIL，因为 `ParserEvaluationCfg` 和环境映射尚不存在。

- [ ] **Step 3: 实现顶层配置**

  在 `Config` 和 `EnvConfig` 上都增加 `ParserEvaluation ParserEvaluationCfg`。`EnvConfig` 是部署事实源，`ApplyToConfig` 把它复制到运行时顶层 `parserEvaluation`；不要把它放进用户/agent scope。

  ```go
  type ParserEvaluationCfg struct {
      Enabled              bool   `json:"enabled,omitempty"`
      WorkerEnabled        bool   `json:"workerEnabled,omitempty"`
      RendererEndpoint     string `json:"rendererEndpoint,omitempty"`
      MarkItDownEndpoint   string `json:"markitdownEndpoint,omitempty"`
      AnyDocEndpoint       string `json:"anydocEndpoint,omitempty"`
      RenderTimeoutMS      int    `json:"renderTimeoutMs,omitempty"`
      ParseTimeoutMS       int    `json:"parseTimeoutMs,omitempty"`
      JudgeTimeoutMS       int    `json:"judgeTimeoutMs,omitempty"`
      MaxFileBytes         int64  `json:"maxFileBytes,omitempty"`
      MaxFiles             int    `json:"maxFiles,omitempty"`
      MaxBatchBytes        int64  `json:"maxBatchBytes,omitempty"`
      RetentionDays        int    `json:"retentionDays,omitempty"`
      MaxPages             int    `json:"maxPages,omitempty"`
      RenderDPI            int    `json:"renderDPI,omitempty"`
      MarkdownJudgeChars   int    `json:"markdownJudgeChars,omitempty"`
  }
  ```

  提供 `DefaultParserEvaluationCfg()`、`ApplyDefaults()` 和 `Validate()`；默认 endpoints 分别为 `http://parser-eval-renderer:8080`、`http://parser-eval-markitdown:8080`、`http://parser-eval-anydoc:8080`。`workerEnabled` 默认随 `enabled` 开启，但必须保留显式 `false` 的 env-set 标记，支持 API-only pod。

- [ ] **Step 4: 写领域 DTO 的失败测试**

  测试所有 status/stage/engine/winner/artifact kind 的闭合集、JSON 拒绝未知字段和尾随数据、字符串/数组大小上限、成功槽位判断，以及 `ExecutionSnapshot.Validate()` 不允许凭证字段。

- [ ] **Step 5: 实现 `internal/parseeval/model.go`**

  固定这些公共契约：

  ```go
  type RunStatus string       // DRAFT, QUEUED, RUNNING, SUCCEEDED, PARTIAL, FAILED, CANCELLED
  type DocumentStatus string  // UPLOADED, RUNNING, SUCCEEDED, PARTIAL, FAILED
  type Engine string          // markitdown, anydoc
  type Winner string          // markitdown, anydoc, tie

  type JudgeBindingSnapshot struct {
      ID, Provider, Model, Fingerprint string
      ModelDisplayName string
      PricingKnown bool
      InputCostPerMillion, OutputCostPerMillion float64
  }

  type ExecutionSnapshot struct {
      MarkItDown, AnyDoc ParserDescriptor
      Renderer RendererDescriptor
      Judge JudgeBindingSnapshot
      RenderDPI, MaxPages, MarkdownJudgeChars int
      JudgePromptVersion, AppVersion, CreatedBy string
      ParserConcurrency int
  }

  type ErrorDetail struct { Code, Message string }
  type StoredArtifact struct { ObjectKey, SHA256, MediaType string; ByteSize int64 }
  type TruthPage struct { Page int; Width, Height int; Artifact StoredArtifact }
  type TruthResult struct {
      Status StepStatus
      Descriptor RendererDescriptor
      TotalPages, CoveredPages int
      Pages []TruthPage
      RenderDurationMs *int64
      Error ErrorDetail
  }
  type ParserResult struct {
      Status StepStatus
      Descriptor ParserDescriptor
      Order int
      ParseDurationMs, EndToEndDurationMs *int64
      Markdown StoredArtifact
      Stats StructureStats
      Warnings []ParseWarning
      Error ErrorDetail
  }
  type JudgeSlot struct {
      Order JudgeOrder
      Status StepStatus
      Verdict BlindVerdict
      Raw StoredArtifact
      DurationMs *int64
      Usage TokenUsage
      EstimatedCostUSD *float64
      Error ErrorDetail
  }
  type JudgeResult struct {
      Status StepStatus
      MarkItDownA, AnyDocA JudgeSlot
      MarkItDownScore, AnyDocScore *QualityScore
      Winner *Winner
      Usage TokenUsage
      EstimatedCostUSD *float64
      Error ErrorDetail
  }
  type RunSummary struct {
      MarkItDown, AnyDoc ParserAggregate
      Formats []FormatAggregate
      MacroQuality *QualityComparison
      Wins WinCounts
      Usage TokenUsage
      EstimatedCostUSD *float64
  }
  ```

  所有 JSON 通过具体 struct 编解码；提供 `DecodeClosedJSON[T]` 和 `EncodeBoundedJSON`，分别使用 `DisallowUnknownFields`/EOF 检查和字段级 Validate，禁止持久化任意 `map[string]any`。

- [ ] **Step 6: 运行测试并提交**

  Run: `go test ./internal/config ./internal/parseeval`

  Expected: PASS.

  Commit: `feat(parser-eval): add configuration and domain contracts`

---

## Task 2: 只用两张表实现持久状态机、租约和 fence

**Files:**

- Create: `internal/store/parser_eval_schema.go`
- Create: `internal/store/parser_eval.go`
- Create: `internal/store/parser_eval_test.go`
- Create: `internal/store/parser_eval_migration_test.go`
- Modify: `internal/store/database.go`

- [ ] **Step 1: 写 migration 失败测试**

  在 SQLite 内存库断言只出现 `parser_eval_runs`、`parser_eval_documents` 两张新表、documents 的 `(run_id, ordinal)` 唯一约束、任务状态/租约/过期扫描索引；MySQL DDL 测试断言 `VARCHAR(128)` ID、`LONGTEXT` JSON、InnoDB 和 `utf8mb4_unicode_ci`。

- [ ] **Step 2: 实现 additive migration**

  `migrateParserEvaluationSchema` 创建且只创建：

  ```text
  parser_eval_runs:
    id, status, stage, progress_json, execution_snapshot_json, summary_json,
    created_by, error_code, error_message, created_at, updated_at, started_at,
    finished_at, expires_at, lease_owner, lease_until, fence_token,
    cancel_requested_at

  parser_eval_documents:
    id, run_id, ordinal, file_name, format, media_type, size_bytes, sha256,
    source_object_key, status, stage, truth_json, markitdown_result_json,
    anydoc_result_json, judge_result_json, error_code, error_message,
    created_at, updated_at
  ```

  把 migration 接到 `DBStore.migrate` 中现有 RAG eval migration 之后。JSON 默认使用合法空对象/空 summary DTO，不用 SQL `NULL` 表示槽位状态。

- [ ] **Step 3: 写 CRUD 与状态竞争失败测试**

  覆盖：DRAFT 添加/移除文档、同 run 50 文件与总字节原子限制、DRAFT→QUEUED、claim QUEUED 或租约过期 RUNNING、heartbeat/fence、取消请求、合法终态、PARTIAL/FAILED→QUEUED、成功槽位保护、对象删除后事务 purge、终态过期扫描。

- [ ] **Step 4: 实现精简 store API**

  记录类型保留 JSON 字符串，领域层负责封闭 DTO：

  ```go
  CreateParserEvalRun(ctx, *ParserEvalRunRecord) error
  GetParserEvalRun(ctx, id string) (*ParserEvalRunRecord, error)
  ListParserEvalRuns(ctx, cursor string, limit int) ([]ParserEvalRunRecord, error)
  ReserveParserEvalDocument(ctx, *ParserEvalDocumentRecord, maxFiles int, maxBytes int64) error
  CompleteParserEvalDocumentUpload(ctx, runID, documentID, sourceObjectKey string) (bool, error)
  AbortParserEvalDocumentUpload(ctx, runID, documentID string) (bool, error)
  DeleteParserEvalDraftDocument(ctx, runID, documentID string) (bool, error)
  ListParserEvalDocuments(ctx, runID string) ([]ParserEvalDocumentRecord, error)
  StartParserEvalRun(ctx, id, actor, snapshotJSON, progressJSON string) (bool, error)
  ClaimParserEvalRun(ctx, worker string, now time.Time, lease time.Duration) (*ParserEvalLease, bool, error)
  HeartbeatParserEvalRun(ctx, lease ParserEvalLease, now time.Time) (bool, error)
  PutParserEvalDocumentResults(ctx, lease ParserEvalLease, update ParserEvalDocumentUpdate) (bool, error)
  FinishParserEvalRun(ctx, lease ParserEvalLease, finish ParserEvalRunFinish) (bool, error)
  RequestCancelParserEvalRun(ctx, id string, at time.Time) (bool, error)
  RequeueParserEvalFailures(ctx, id string, at time.Time) (bool, error)
  ListExpiredParserEvalRuns(ctx, now time.Time, limit int) ([]ParserEvalRunRecord, error)
  PurgeParserEvalRun(ctx, id string) (bool, error)
  ```

  `ReserveParserEvalDocument` 在 DRAFT 事务中检查数量/总字节并先写 `source_object_key=''` 的短暂 reservation；`Complete...` 只允许把空 key 设置成服务端生成的 final key。这样相同幂等 ID 的并发请求会在写 object 前被 SQL 主键串行化，不会互相覆盖 source。reservation 不作为完成上传返回给客户端，`/start` 必须拒绝仍为空的 source key。

  `PutParserEvalDocumentResults` 使用 fence 条件更新，并在 SQL 层拒绝把成功 JSON 槽位替换为失败/空槽位。列表固定按 `created_at DESC, id DESC` cursor 排序。

- [ ] **Step 5: 验证两种方言并提交**

  Run: `go test ./internal/store -run ParserEval`

  Run: `go test ./internal/store`

  Expected: PASS.

  Commit: `feat(parser-eval): add two-table durable store`

---

## Task 3: 实现安全上传、对象布局和 artifact 读取

**Files:**

- Create: `internal/parseeval/keys.go`
- Create: `internal/parseeval/ooxml.go`
- Create: `internal/parseeval/service.go`
- Create: `internal/parseeval/service_test.go`

- [ ] **Step 1: 写上传与 key 失败测试**

  覆盖三个合法最小 OOXML、伪扩展名、错误 MIME、空文件、截断 ZIP、路径穿越 entry、entry 数/单 entry/总展开量/压缩比超限、文件名控制字符、声明大小不符、批次限制，以及以下服务端 key 的精确值：

  ```text
  parser-eval/runs/{runID}/documents/{documentID}/source.bin
  parser-eval/runs/{runID}/documents/{documentID}/truth/page-0001.png
  parser-eval/runs/{runID}/documents/{documentID}/outputs/markitdown.md
  parser-eval/runs/{runID}/documents/{documentID}/outputs/anydoc.md
  parser-eval/runs/{runID}/documents/{documentID}/judge/markitdown-a.json
  parser-eval/runs/{runID}/documents/{documentID}/judge/anydoc-a.json
  ```

- [ ] **Step 2: 实现 OOXML preflight 和流式 upload**

  API 单文件先写 0700 临时文件，同时计算 SHA-256 和实际字节；然后使用 `archive/zip` 做 ZIP/entry/required-part 校验，再从头流式 Put 到现有 `objects.Store`。required part 分别为 `word/document.xml`、`ppt/presentation.xml`、`xl/workbook.xml`，且都要求 `[Content_Types].xml`。

  任一步失败都删除临时文件和已写 source object。不要把 50 MiB 文件读入单个 `[]byte`。

- [ ] **Step 3: 实现草稿 service 和幂等 ID**

  `Service.CreateDraft`、`UploadDocument`、`RemoveDraftDocument` 使用 UUID；请求带 8–128 字符 `Idempotency-Key` 时，ID 为 `SHA-256(actor + operation + parentID + key)` 的 128-bit 十六进制前缀。相同 ID 已存在时比较不可变请求字段；一致就返回原记录，不一致返回 `idempotency_conflict`，无需第三张 idempotency 表。

  `UploadDocument` 只接受一个 multipart file，ordinal 在 store reservation 事务中取当前最大值加一。完成本地 spool/preflight/SHA 后先 reserve 文档行，再 Put final source object，最后设置 `source_object_key`；Put 失败时 abort reservation。并发相同幂等 ID 若 immutable metadata/SHA 一致：已 complete 就返回原记录，仍为空 key 就返回可重试 `upload_in_progress`；内容不一致返回 `idempotency_conflict`。服务进程在 Put 后崩溃留下的空-key reservation 可由 DRAFT 删除接口清理，且绝不能通过 `/start`。

- [ ] **Step 4: 实现受保护 artifact resolver**

  ```go
  type ArtifactRequest struct {
      Kind ArtifactKind
      Page int
      Engine Engine
      Order JudgeOrder
  }

  func (s *Service) OpenArtifact(ctx context.Context, runID, documentID string, req ArtifactRequest) (Artifact, error)
  ```

  resolver 只能从已解码的 `TruthResult`/`ParserResult`/`JudgeResult` 获取 object key；URL 参数绝不能拼 object key。返回值包含 reader、size、content type 和安全下载名，不向 API DTO 暴露 key。

- [ ] **Step 5: 测试并提交**

  Run: `go test ./internal/parseeval -run 'Upload|OOXML|Artifact|ObjectKey|Draft'`

  Expected: PASS，且 fake object store 断言失败补偿和删除顺序。

  Commit: `feat(parser-eval): add secure document and artifact service`

---

## Task 4: 增加 renderer sidecar 和严格 Go client

**Files:**

- Create: `services/parser-eval-renderer/pyproject.toml`
- Create: `services/parser-eval-renderer/uv.lock`
- Create: `services/parser-eval-renderer/Dockerfile`
- Create: `services/parser-eval-renderer/app/__init__.py`
- Create: `services/parser-eval-renderer/app/main.py`
- Create: `services/parser-eval-renderer/app/protocol.py`
- Create: `services/parser-eval-renderer/tests/fixtures.py`
- Create: `services/parser-eval-renderer/tests/test_main.py`
- Create: `internal/parseeval/renderer_client.go`
- Create: `internal/parseeval/renderer_client_test.go`

- [ ] **Step 1: 写 Python renderer 协议失败测试**

  用 dev dependencies `python-pptx`、`openpyxl` 生成最小 DOCX/PPTX/XLSX。测试 `/healthz` 的闭合 descriptor、只接受一个 `file`、伪格式/损坏 ZIP/超限/超时、前 6 页、manifest PNG hash/尺寸、成功和失败后独立 request/profile 目录清理。

- [ ] **Step 2: 运行 renderer 测试并确认失败**

  Run: `cd services/parser-eval-renderer; uv run --extra dev pytest`

  Expected: FAIL，因为服务尚未实现。

- [ ] **Step 3: 实现 `parser-eval-renderer/v1`**

  `POST /v1/render?format=docx|pptx|xlsx` 的固定流程：保存并校验上传 → 为本次请求创建独立 LibreOffice profile → `soffice --headless --convert-to pdf --outdir ... -env:UserInstallation=file://...` → PyMuPDF 读取总页数并以 100 DPI 渲染前 6 页 → 校验像素/单页/总 bundle → 返回 `manifest.json + pages/page-NNNN.png` tar。

  所有子进程通过 timeout 和进程组终止；异常只返回封闭错误码：`invalid_multipart`、`unsupported_format`、`invalid_ooxml`、`input_too_large`、`render_timeout`、`libreoffice_failed`、`invalid_pdf`、`render_limit_exceeded`、`bundle_limit_exceeded`。

  `uv.lock` 用 `uv lock` 生成并由 `uv sync --frozen` 验证。Dockerfile 使用 Python 3.12 bookworm、锁定的 uv 版本和可复现 LibreOffice apt snapshot；实施时先解析并提交基础镜像 digest，不提交浮动 `latest`。运行用户固定 `65532:65532`。

- [ ] **Step 4: 写 Go client 失败测试**

  测试健康 descriptor 严格解码、缓存快照、multipart 流、tar 路径/重复 entry/hash/大小/像素限制、descriptor 不匹配和 context 取消。

- [ ] **Step 5: 实现 Go client**

  client 复用 `sidecar.BundleHandle` 的安全 tar 解码思想，但协议类型放在 `internal/parseeval`，不伪装成 RAG parser manifest：

  ```go
  type RendererClient interface {
      Render(context.Context, document.Source) (*RenderedTruth, error)
      HealthSnapshot() RendererHealthSnapshot
      StartHealthProbe(context.Context)
  }
  ```

  `RenderedTruth` 拥有临时 bundle 目录并提供 `OpenPage`/`Close`。健康探测后台运行，capabilities 和 `/start` 只读缓存，不做请求路径网络探测。

- [ ] **Step 6: 完整验证并提交**

  Run: `cd services/parser-eval-renderer; uv run --extra dev pytest && uv run ruff check .`

  Run: `go test ./internal/parseeval -run Renderer`

  Expected: PASS.

  Commit: `feat(parser-eval): add isolated office renderer`

---

## Task 5: 精确定义 parser 两类延迟并复用 canonical Markdown join

**Files:**

- Modify: `services/rag-parser/app/main.py`
- Modify: `services/rag-parser/tests/test_office.py`
- Modify: `internal/rag/parse/sidecar/bundle.go`
- Modify: `internal/rag/parse/sidecar/client.go`
- Modify: `internal/rag/parse/sidecar/client_test.go`
- Modify: `internal/rag/parse/parser.go`
- Modify: `internal/rag/parse/office.go`
- Modify: `internal/rag/parse/office_test.go`
- Modify: `internal/rag/document/artifact.go`
- Modify: `internal/rag/document/artifact_test.go`

- [ ] **Step 1: 写 `X-BkCrab-Parse-Duration-Ms` 边界测试**

  Python 测试用可控 converter 证明计时从完整上传和 metadata/OOXML 校验之后开始，到 bundle 构建及 `validate_bundle_for_stream` 完成后停止；tar streaming 时间不计入。header 必须是非负十进制整数。

- [ ] **Step 2: 实现 sidecar header**

  保留 request 总耗时日志；在 `_save_upload` 返回后单独 `parse_started = time.monotonic()`，完成 bundle validation 后计算值并放入成功响应 header。manifest protocol 不变。

- [ ] **Step 3: 写 Go timing 失败测试**

  覆盖合法/缺失/负数/非整数/超过 client timeout 的 header，以及 end-to-end 明显包含延迟的测试。缺失/非法主延迟只把 `ParseDuration` 留空，不破坏生产 bundle；端到端失败耗时由评测调用层仍可记录。

- [ ] **Step 4: 实现可选 timing 元数据和 parser hook**

  ```go
  type BundleTimings struct {
      ParseDuration *time.Duration
      EndToEndDuration time.Duration
  }

  type BundleHandle struct {
      Manifest Manifest
      Timings BundleTimings
      // existing ownership fields
  }
  ```

  `postBundle` 从发出请求前计时，直到 `DecodeBundle` 完成后设置 `EndToEndDuration`。在 `ParseOptions` 增加可选 `SidecarTimings func(sidecar.BundleTimings)`，`parseOffice` 拿到 bundle 后立即调用；生产调用方不设置 hook，行为不变。

- [ ] **Step 5: 提取 canonical join helper**

  ```go
  func JoinMarkdownUnits(units []MarkdownUnit) string
  ```

  helper 保持现有 `ParsedArtifact.NormalizedMarkdown()` 的精确规则：按稳定 unit 顺序以两个换行拼接，不额外 trim 或重写内容。`NormalizedMarkdown()` 改为委托 helper；评测 runner 后续直接对 `ParsedDocument.Units` 调用同一 helper。

- [ ] **Step 6: 回归并提交**

  Run: `cd services/rag-parser; uv run --extra dev pytest && uv run ruff check .`

  Run: `go test ./internal/rag/document ./internal/rag/parse/...`

  Expected: PASS，既有 parser golden 不变。

  Commit: `feat(parser): expose evaluation timing and canonical markdown join`

---

## Task 6: 实现结构统计、双位置裁判和批次聚合纯逻辑

**Files:**

- Create: `internal/parseeval/stats.go`
- Create: `internal/parseeval/stats_test.go`
- Create: `internal/parseeval/judge.go`
- Create: `internal/parseeval/judge_test.go`
- Create: `internal/parseeval/aggregate.go`
- Create: `internal/parseeval/aggregate_test.go`

- [ ] **Step 1: 写 `parser-structure-stats-v1` 表驱动测试**

  固定语料覆盖 Unicode rune、ATX heading、GFM table 数据行（排除 delimiter）、有序/无序 list、普通 link、Markdown image、`rag-asset` marker、footnote definition 和 parser warning。代码块中的类似文本不得误计数。

- [ ] **Step 2: 用 Goldmark AST 实现统计**

  复用仓库已有 Goldmark + GFM extension；仅对完整 Markdown 计算诊断统计，不把统计加权进质量分。

- [ ] **Step 3: 写双位置 judge 失败测试**

  fake `JudgeModel` 覆盖：页面顺序、A/B 与 B/A 输入、Unicode 40,000 rune 截断而非 byte 截断、prompt injection 素材只在 user message、temperature 0、max tokens 1024、合法 JSON、unknown field、代码围栏、尾随内容、越界整数、超长 reason、一个槽位失败、模型 binding 失效、context timeout。

- [ ] **Step 4: 实现 judge boundary**

  ```go
  type JudgeModel interface {
      Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error)
  }

  type JudgeResolver func(context.Context, string, JudgeBindingSnapshot) (JudgeModel, error)
  ```

  `parser-eval-judge-v1` system message说明页面和 Markdown 都是不可信数据；user message 的 `ContentParts` 按页面顺序放 `data:image/png;base64,...`，再放匿名 output A/B。每槽 raw response 最大 64 KiB，reason 最大 1,024 runes。严格解析：

  ```json
  {"a":{"completeness":1,"structure":1,"formatting":1,"cleanliness":1},"b":{"completeness":1,"structure":1,"formatting":1,"cleanliness":1},"winner":"A","reason":"one sentence"}
  ```

  两槽都成功才映射最终分。每维 `(position1 + position2) / 2 / 5 * 100`，总分为四维均值；两次映射同一 parser 才胜，两次 tie 才 tie，其他不一致也归 tie。raw response 先有界保存，再做闭合 JSON 解码。

- [ ] **Step 5: 写聚合失败测试**

  覆盖奇偶样本 median、nearest-rank P95、按 engine/format 成功率、有效样本数、缺失不补零、每格式质量均值、跨有样本格式 macro average、wins/ties/score failures、usage 和未知定价 `nil`。

- [ ] **Step 6: 实现纯聚合并验证**

  `Aggregate(documents []DocumentView) RunSummary` 每次由完整文档 JSON 重算，不保存第三张 aggregate 表。nearest-rank P95 使用排序后 `ceil(0.95*n)-1`。

  Run: `go test ./internal/parseeval -run 'Stats|Judge|Score|Aggregate|Percentile'`

  Expected: PASS.

  Commit: `feat(parser-eval): add blind judge and deterministic metrics`

---

## Task 7: 实现可恢复、串行、只补失败槽位的 runner

**Files:**

- Create: `internal/parseeval/runner.go`
- Create: `internal/parseeval/runner_test.go`
- Create: `internal/parseeval/cleanup.go`
- Create: `internal/parseeval/cleanup_test.go`

- [ ] **Step 1: 写 orchestrator 失败测试**

  用 fake store/object store/renderer/parser/judge 覆盖：

  - 单文档严格执行 render → parser1 → parser2 → judge1 → judge2 → aggregate；
  - SHA-256 最后一个 byte 的最低 bit 为 0 时 MarkItDown 先跑，为 1 时 Anydoc 先跑；
  - 全部 parser 调用最大并发始终为 1；
  - render 失败仍跑双 parser；单 parser 失败不 judge；单 judge 失败不产最终分；
  - transient error 最多重试 2 次，deterministic error 不自动重试；
  - 已成功 truth/parser/judge 槽位在重启和人工 retry 后不再调用；
  - heartbeat/fence 失效立即停止写结果；cancel 在步骤边界停止后续任务；
  - SUCCEEDED/PARTIAL/FAILED/CANCELLED 分类准确。

- [ ] **Step 2: 实现 runner 依赖边界**

  ```go
  type OfficeParser interface {
      Parse(context.Context, document.Source, parse.ParseOptions) (*document.ParsedDocument, error)
  }

  type Runner struct {
      store ParserEvalStore
      objects objects.Store
      renderer RendererClient
      parser OfficeParser
      judgeResolver JudgeResolver
      cfg config.ParserEvaluationCfg
  }
  ```

  为两次 parser 调用设置 `Source.ParserEngine`，`ParseOptions{Mode: config.ParseModeStandard, ParserVersion: contractVersion}`，不设置任何 transcriber、DocumentAI budget 或 enrichment hook。解析成功后调用 `document.JoinMarkdownUnits(parsed.Units)`，保存完整 Markdown、SHA/byte、warnings、stats 和 timing，再 `Close()`。

- [ ] **Step 3: 实现租约循环和人工 retry 语义**

  单个 goroutine claim 一个 run，lease 2 分钟、每 30 秒 heartbeat；heartbeat 同时读取 cancel flag 并 cancel 当前 step context。run 内按 ordinal 串行处理文档。所有 SQL 写带 fence。

  自动 retry 只包裹当前外部 step，最多额外 2 次；人工 retry 仅重置失败/缺失槽位的错误终态并 `PARTIAL/FAILED -> QUEUED`，保留 snapshot、文件列表和所有成功槽位。

- [ ] **Step 4: 写清理顺序失败测试并实现**

  cleanup 每小时扫描终态且 `expires_at <= now` 的 run：先 `objects.DeletePrefix("parser-eval/runs/{runID}")`，成功后 `PurgeParserEvalRun` 事务删除 documents 再 run。对象删除失败不删 SQL，下次继续。手工删除调用同一 primitive；活动 run 返回冲突。

- [ ] **Step 5: 验证并提交**

  Run: `go test -race ./internal/parseeval -run 'Runner|Retry|Cancel|Lease|Fence|Cleanup'`

  Expected: PASS；fake pipeline 断言 chunk/index/provider-answer 依赖根本未注入。

  Commit: `feat(parser-eval): add durable serial evaluation runner`

---

## Task 8: 连接 Gateway、模型目录和缓存健康快照

**Files:**

- Modify: `internal/gateway/gateway.go`
- Create: `internal/gateway/parser_eval.go`
- Create: `internal/gateway/parser_eval_test.go`
- Modify: `cmd/bkcrab/main.go`

- [ ] **Step 1: 写视觉模型目录和 fingerprint 失败测试**

  构造多 provider config，断言只返回 `ModelEntry.Input` 含 `image`、endpoint/key 完整的绑定；返回值不含 API key。fingerprint 对 provider name、API base/type/auth type 和完整 secret-free `ModelEntry` 稳定，对这些字段变化敏感，对 API key 轮换不敏感。

- [ ] **Step 2: 实现目录和冻结 resolver**

  `ListParserEvaluationJudgeBindings(ctx, owner)` 只允许 active super_admin，绑定 ID 使用现有 `provider/model`。启动任务保存 `JudgeBindingSnapshot`；执行时重新 assemble owner config，要求 ID、vision capability 和 fingerprint 完全一致，再用当前 secret 创建 provider。失败返回 `judge_binding_unavailable`，不选默认模型、不 fallback。

- [ ] **Step 3: 写 Gateway assembly 失败测试**

  覆盖 disabled 时零副作用、enabled 时复用现有 object store、创建两个专用 sidecar client/renderer/`parse.LocalParser`、workerEnabled false 不启动 claim loop、健康缓存矩阵和关闭时 cancel。

- [ ] **Step 4: 实现独立 assembly**

  从 `env.ParserEvaluation` 创建：

  - MarkItDown client 指向 `MarkItDownEndpoint`，engine 固定 `markitdown`；
  - Anydoc client 指向 `AnyDocEndpoint`，engine 固定 `anydoc`；
  - pool 注入一个 `parse.LocalParser`，limits 与 config/RAG 有效最小值一致；
  - renderer client；
  - `parseeval.Service` 和 `parseeval.Runner`；
  - 三个后台 health probe，公开值只读缓存。

  Gateway 增加 `ParserEvaluationService()`、`ParserEvaluationRunner()`、`ParserEvaluationCapabilities()`、`ListParserEvaluationJudgeBindings()`、`ResolveParserEvaluationJudge()`。`cmd/bkcrab/main.go` 把这些 setter 交给 setup server。

- [ ] **Step 5: 验证并提交**

  Run: `go test ./internal/gateway ./cmd/bkcrab`

  Expected: PASS.

  Commit: `feat(parser-eval): wire isolated runtime and judge catalog`

---

## Task 9: 增加仅超级管理员 Session 可用的 Admin API

**Files:**

- Modify: `internal/setup/server.go`
- Create: `internal/setup/handlers_parser_eval.go`
- Create: `internal/setup/handlers_parser_eval_test.go`
- Modify: `internal/setup/server_lifecycle_test.go`

- [ ] **Step 1: 写权限和 route 失败测试**

  对以下每个 route 表驱动验证：super_admin session 通过，普通用户、admin API key、act-as session、未认证请求均拒绝。

  ```text
  GET    /api/admin/parser-evals/capabilities
  GET    /api/admin/parser-evals/runs
  POST   /api/admin/parser-evals/runs
  GET    /api/admin/parser-evals/runs/{runId}
  POST   /api/admin/parser-evals/runs/{runId}/documents
  DELETE /api/admin/parser-evals/runs/{runId}/documents/{documentId}
  POST   /api/admin/parser-evals/runs/{runId}/start
  POST   /api/admin/parser-evals/runs/{runId}/cancel
  POST   /api/admin/parser-evals/runs/{runId}/retry
  DELETE /api/admin/parser-evals/runs/{runId}
  GET    /api/admin/parser-evals/runs/{runId}/documents/{documentId}/artifacts/{kind}
  ```

- [ ] **Step 2: 写生命周期/错误映射失败测试**

  覆盖 closed JSON、body 限制、单 multipart file、8–128 idempotency key、DRAFT 条件、start capability/model snapshot、cancel/retry/delete 冲突、列表 cursor、partial results，以及 artifact 的 `page=1..6`、`engine=markitdown|anydoc`、`order=markitdown-a|anydoc-a` 闭合集。

- [ ] **Step 3: 实现 handlers 和 DTO redaction**

  Server 增加 parser-eval service/runner/capability catalog setter，并用 `evalAdmin := requireSuperAdminSession` 注册新 routes。统一响应形状：

  ```json
  {"ok":true,"data":{}}
  {"ok":false,"error":{"code":"stable_code","message":"safe message"}}
  ```

  `GET /runs/{id}` 一次返回 run、最多 50 documents 和 summary。序列化前把所有 object key 转换成受保护 artifact URL 或 availability flag，绝不返回内部 key。

  artifact response 设置正确 Office/PNG/Markdown/JSON Content-Type、`X-Content-Type-Options: nosniff`、`Cache-Control: no-store, private` 和 RFC 5987 安全文件名。

- [ ] **Step 4: 验证并提交**

  Run: `go test ./internal/setup -run ParserEval`

  Run: `go test ./internal/setup`

  Expected: PASS.

  Commit: `feat(parser-eval): expose super-admin evaluation api`

---

## Task 10: 增加独立前端、上传进度和证据视图

**Files:**

- Modify: `web/src/lib/api.ts`
- Modify: `web/src/components/app-sidebar.tsx`
- Modify: `web/src/components/rag-safe-render.ts`
- Modify: `web/src/components/rag-resource-gallery.test.mjs`
- Create: `web/src/app/admin/parser-evals/parser-eval-state.ts`
- Create: `web/src/app/admin/parser-evals/parser-eval-state.test.mjs`
- Create: `web/src/app/admin/parser-evals/page.tsx`
- Create: `web/src/app/admin/parser-evals/[id]/page.tsx`
- Create: `web/src/app/admin/parser-evals/components/create-run-panel.tsx`
- Create: `web/src/app/admin/parser-evals/components/run-list.tsx`
- Create: `web/src/app/admin/parser-evals/components/run-summary.tsx`
- Create: `web/src/app/admin/parser-evals/components/document-results.tsx`
- Create: `web/src/app/admin/parser-evals/components/evidence-dialog.tsx`

- [ ] **Step 1: 写前端状态纯函数失败测试**

  覆盖：仅 super_admin session 显示导航；扩展名/文件数/总字节校验；必须选择 vision binding；逐文件 upload success/failure/retry/remove；活动/后台/终态轮询间隔；两 parser 与双 judge 结果映射；PARTIAL/FAILED 操作；缺失分数显示 `—` 而非 `0`。

- [ ] **Step 2: 实现 typed API client**

  在 `api.ts` 加 `ParserEvalCapabilities`、`ParserEvalRunDetail`、`ParserEvalDocument`、`ParserEvalSummary` 及 11 个 endpoint function。上传使用 `XMLHttpRequest` 封装以提供单文件进度和 abort；其他请求继续复用 `apiFetch`。每次 create/upload/start/cancel/retry 生成稳定的客户端 idempotency key，并在同一次 UI retry 中复用。

- [ ] **Step 3: 实现列表/新建页**

  `/admin/parser-evals/` 加：capabilities/health banner、视觉模型 selector、拖拽或 file input（accept 精确为 `.docx,.pptx,.xlsx`）、1–50 文件队列、逐文件进度/重试/移除、覆盖范围提示、全部上传成功后 start、历史 run 列表。

  侧边栏在“RAG 测评”之后增加“解析测评”，不导入旧 RAG eval 的 dataset/run state；共享的仅是 super-admin session 判断，可把它提到通用 admin nav helper。

- [ ] **Step 4: 实现详情和 evidence dialog**

  详情页显示两个 engine 的 parse/end-to-end median、P95、成功率，四维/总分、wins/ties/failures、按格式表、macro average、usage/cost。文档行显示双 parser、双 timing、双方总分、winner 和安全错误。

  evidence dialog 通过受保护 API 按需加载：最多 6 个 truth PNG、两份 Markdown、stats/warnings、两个 judge verdict/reason/raw JSON 和下载按钮。不要预取所有 50 个文档的大 artifact。

- [ ] **Step 5: 加强 Markdown 安全渲染**

  在 `rag-safe-render.ts` 增加专用 `ParserEvalMarkdown`：`skipHtml: true`；所有 `<img>`/`rag-asset` 只显示文本占位；所有 remote/protocol-relative/custom-scheme link 移除 href；不允许 data image。复用 GFM 表格渲染，但不复用允许 same-origin image 的 agent renderer。

- [ ] **Step 6: 验证并提交**

  Run: `cd web; npm test -- --test-name-pattern='ParserEval|parser eval|Markdown renderer'`

  Run: `cd web; npm run lint && npm run build`

  Expected: PASS；构建产出两个新 route。

  Commit: `feat(parser-eval): add independent admin interface`

---

## Task 11: Docker profile、可观测性、端到端验收和运维文档

**Files:**

- Modify: `deploy/docker/docker-compose.rag.yml`
- Modify: `deploy/docker/.env.example`
- Create: `deploy/docker/docker-compose.parser-eval-test.yml`
- Create: `internal/parseeval/docker_e2e_test.go`
- Create: `docs/parser-evaluation.md`
- Modify: `docs/database.md`

- [ ] **Step 1: 增加 `parser-evaluation` profile**

  新增 `parser-eval-markitdown`、`parser-eval-anydoc`、`parser-eval-renderer`。前两个复用 `services/rag-parser` 镜像并固定 engine；三者均：

  - `profiles: ["parser-evaluation"]`；
  - 只加入 `parser-eval-internal`，不发布 host port；
  - `user: 65532:65532`、`read_only: true`、bounded tmpfs、`cap_drop: [ALL]`、`no-new-privileges`；
  - 配置 CPU、memory、PID、healthcheck；
  - 不挂载对象存储/model credentials，internal network 禁止公网出口。

  `bkcrab` 接入该 internal network 并读取 Task 1 的 env；不要让未启用 profile 时的 `depends_on` 阻止主服务启动。`.env.example` 默认 `PARSER_EVAL_ENABLED=false`、`PARSER_EVAL_WORKER_ENABLED=true`。

- [ ] **Step 2: 加低基数 telemetry**

  在现有 telemetry 体系记录 run lifecycle、render/parse/judge/cleanup outcome 和 duration；metrics labels 只允许 engine、format、outcome、stable error code。run/document ID 只能在受控结构化日志字段，文件名、object key、正文、图片、Markdown、prompt、raw verdict 禁止记录。

- [ ] **Step 3: 写 Docker E2E 测试**

  test overlay 只为测试把三个 sidecar 绑定到 `127.0.0.1` 随机/测试端口。`docker_e2e_test.go` 使用 `services/rag-parser/tests/fixtures/generate_minimal.py` 生成三格式 fixture、真实 renderer 和两个真实 parser endpoint、内存 SQLite + local object store + fake vision JudgeModel，验证：

  - 每文件两份 final canonical Markdown；
  - parse/end-to-end 两类延迟；
  - 每个可评分文件两份 verdict 与四维分；
  - per-format 与 macro summary；
  - 中断并重建 runner 后只补缺失槽位；
  - cancel、retry、delete/cleanup；
  - `rag_chunks`、index/generation 表没有新增行，fake Milvus/embedding/rag-evaluator 完全不需要启动。

- [ ] **Step 4: 运行端到端验收**

  Run: `docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml -f deploy/docker/docker-compose.parser-eval-test.yml --profile parser-evaluation up -d --build parser-eval-renderer parser-eval-markitdown parser-eval-anydoc`

  Run: `$env:PARSER_EVAL_E2E='1'; go test ./internal/parseeval -run TestDockerE2E -count=1`

  Run: `docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml -f deploy/docker/docker-compose.parser-eval-test.yml --profile parser-evaluation down`

  Expected: PASS；三个容器健康，测试不依赖 Milvus/Embedding/rag-evaluator。

- [ ] **Step 5: 写运维和隐私文档**

  `docs/parser-evaluation.md` 记录启用命令、所有 env、只允许一个 worker pod、健康排障、延迟定义、评分覆盖、90 天保留/提前删除、备份敏感性和无公网 sidecar 约束。`docs/database.md` 只记录两张表和 object prefix 删除顺序。

- [ ] **Step 6: 全量回归**

  Run: `go test ./...`

  Run: `cd services/rag-parser; uv run --extra dev pytest && uv run ruff check .`

  Run: `cd services/parser-eval-renderer; uv run --extra dev pytest && uv run ruff check .`

  Run: `cd web; npm test && npm run lint && npm run build`

  Run: `docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml --profile parser-evaluation config`

  Expected: 全部 PASS，Compose 配置中评测容器无 host ports、只在 internal network。

- [ ] **Step 7: 最终提交**

  Commit: `feat(parser-eval): complete isolated parser benchmark platform`

---

## Definition of Done

- 超级管理员能在独立页面创建 1–50 文件任务并逐任务选择视觉裁判模型。
- `.docx/.pptx/.xlsx` 每个文件严格跑 MarkItDown 和 Anydoc 各一次，输出为生产同源 canonical Markdown，且没有后续 RAG 流程。
- 成功 parser 同时记录 sidecar `parseDurationMs` 和 Go `endToEndDurationMs`；汇总含样本数、median、nearest-rank P95 和成功率。
- 可评分文档使用前 6 个 100-DPI 真值页和双方前 40,000 Unicode 字符完成 A/B、B/A 两次判决。
- 页面可查看四维分、总分、胜负、结构统计、warnings、真值页、完整 Markdown 和两份原始 verdict，但 API 不泄露 object key。
- 文件级、renderer、单 parser、单 judge 失败均保留已有结果；重启和人工 retry 只补缺失/失败槽位。
- SQL 只有两张新表；任务删除和 90 天清理都先删 object prefix，成功后再事务删 SQL。
- 权限、容器隔离、日志脱敏、Markdown 安全渲染和无公网 sidecar 均通过测试。
