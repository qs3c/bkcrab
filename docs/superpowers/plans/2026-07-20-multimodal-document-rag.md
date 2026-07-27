# RAG 多模态文档解析与结构化分块实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将现有纯文本 RAG 升级为“视觉解析 + 纯文本检索/回答 + 原图资源展示”的文档 RAG：PDF 支持逐页 auto/VLM 与降级，Office 统一通过 MarkItDown，分块统一处理 Markdown AST，图片/表格/代码语义参与文字检索，命中图片作为受控资源展示。

**Architecture:** Go 主服务负责租户权限、fenced task、VLM/Enricher 调用、artifact/asset 持久化、分块、embedding、Milvus 和回答边界；独立 Python `rag-parser` sidecar 负责 MarkItDown Office 转换和经许可 PDFEngine 的本地分析/渲染，永不持有 VLM key。SQL 保存不可变 document-version snapshot、active-version chunk catalog 与资源关系；Milvus 只保存 SearchContent 和向量并在每条 ANN route 下推 active filter。知识库回答模型只接收文字；普通 Agent 保留用户主动上传图片，但 RAG 不新增视觉输入。Web 通过 typed metadata 展示图片。

**Tech Stack:** Go；现有 Milvus 2.6、对象存储与 OpenAI-compatible provider；Markdown AST 使用 `github.com/yuin/goldmark` + GFM extension；Python 3.12；Microsoft MarkItDown 0.1.6（只安装 docx/pptx/xlsx extras）；经 ADR 批准的 PDFEngine；Pillow；FastAPI/Uvicorn；React/Next.js。

**Spec:** [`docs/superpowers/specs/2026-07-20-multimodal-document-rag-design.md`](../specs/2026-07-20-multimodal-document-rag-design.md)

**范围:** 本计划覆盖后端、parser sidecar、Docker/Kubernetes/Helm 接线、知识库页面、专用知识库问答和 Web Agent 聊天。外部 channel 主动发图、图片向量和旧 Office 格式不在本计划。

**基线:** 当前主分支已包含代码围栏标题保护以及标题进入 `SearchContent` 的改动；现有 `go test ./internal/rag/...` 必须在每阶段保持通过。现有 KB 迁移后 `parseMode=standard`，不会自动批量重建。

**安全/成本闸门:** 在 Task 3 的 task fencing/active-version 完成前不得接入真实 VLM；PDF sidecar 开发前必须提交依赖 ADR，明确 PyMuPDF 的 AGPL 合规、商业许可或经审核替代 engine 三选一；所有单测使用 fake sidecar/fake VLM，不依赖网络；真实 sidecar/VLM 集成测试使用环境变量门控。`advanced.enabled`、`office.enabled` 与 `enrichment.enabled` 都默认 false，Task 19 的 durable delete/GC 和对应发布闸门完成前不对普通用户开放。

---

## 文件结构总览

```text
internal/config/config.go
internal/config/env.go
internal/config/rag_test.go

internal/store/rag.go
internal/store/store.go
internal/store/database.go
internal/store/database_mysql.go
internal/store/rag_migration_test.go
internal/store/rag_task_claim_test.go
internal/store/rag_document_ai_budget_test.go

internal/rag/document/model.go
internal/rag/document/markdown.go
internal/rag/document/artifact.go
internal/rag/document/model_test.go
internal/rag/document/artifact_test.go

internal/rag/parse/parser.go
internal/rag/parse/markdown.go
internal/rag/parse/text.go
internal/rag/parse/sanitize.go
internal/rag/parse/sanitize_test.go
internal/rag/parse/pdf_native.go
internal/rag/parse/pdf_auto.go
internal/rag/parse/office.go
internal/rag/parse/sidecar/client.go
internal/rag/parse/sidecar/client_test.go

internal/rag/vision/vision.go
internal/rag/vision/openai.go
internal/rag/vision/image.go
internal/rag/vision/cache.go
internal/rag/vision/budget.go
internal/rag/vision/vision_test.go
internal/rag/vision/openai_test.go
internal/rag/vision/image_test.go
internal/rag/vision/budget_test.go

internal/rag/enrich/enrich.go
internal/rag/enrich/openai.go
internal/rag/enrich/cache.go
internal/rag/enrich/enrich_test.go
internal/rag/enrich/openai_test.go

internal/rag/assets/assets.go
internal/rag/assets/image.go
internal/rag/assets/assets_test.go
internal/rag/assets/image_test.go

internal/rag/split/split.go
internal/rag/split/markdown_ast.go
internal/rag/split/table.go
internal/rag/split/code.go
internal/rag/split/split_test.go
internal/rag/split/table_test.go
internal/rag/split/code_test.go
internal/rag/split/asset_test.go

internal/rag/pipeline.go
internal/rag/pipeline_test.go
internal/rag/vector/vector.go
internal/rag/vector/milvus.go
internal/rag/vector/fake.go
internal/rag/vector/chunks.go
internal/rag/vector/chunks_test.go
internal/rag/search.go
internal/rag/search_test.go
internal/rag/agent_adapter.go

internal/agent/tools/result.go
internal/agent/tools/result_test.go
internal/agent/tools/registry.go
internal/agent/tools/registry_result_test.go
internal/agent/tools/rag_search.go
internal/agent/sdkbridge.go
internal/agent/sdkbridge_result_test.go
internal/agent/loop.go
internal/agent/manager_rag_test.go
internal/agent/loop_usage_test.go

internal/setup/handlers_rag.go
internal/setup/handlers_rag_assets.go
internal/setup/handlers_rag_assets_test.go
internal/setup/handlers_agent_rag_assets.go
internal/setup/handlers_agent_rag_assets_test.go
internal/setup/handlers_rag_test.go
internal/setup/server.go

web/src/lib/api.ts
web/src/app/knowledge/page.tsx
web/src/app/knowledge/chat/knowledge-chat-client.tsx
web/src/components/chat-screen.tsx
web/src/components/rag-resource-gallery.tsx
web/src/components/rag-resource-gallery.test.mjs
web/package.json

services/rag-parser/pyproject.toml
services/rag-parser/uv.lock
services/rag-parser/Dockerfile
services/rag-parser/app/main.py
services/rag-parser/app/protocol.py
services/rag-parser/app/office.py
services/rag-parser/app/pdf_engine.py
services/rag-parser/app/pdf.py
services/rag-parser/docs/pdf-engine-adr.md
services/rag-parser/tests/test_protocol.py
services/rag-parser/tests/test_office.py
services/rag-parser/tests/test_pdf.py

testdata/rag-parser-protocol/v1/

deploy/docker/docker-compose.rag.yml
deploy/docker/.env.example
deploy/k8s/bkcrab.yaml
deploy/helm/bkcrab/values.yaml
deploy/helm/bkcrab/templates/rag-parser.yaml
deploy/helm/bkcrab/templates/rag-parser-networkpolicy.yaml
deploy/helm/bkcrab/templates/configmap.yaml
deploy/helm/bkcrab/templates/secrets.yaml
internal/users/account.go
internal/users/account_test.go
docs/database.md
```

依赖方向：

```text
setup / agent
      ↓
rag.Service / pipeline / search
      ↓
document ← parse ← sidecar client
      ↓       ↓
split      vision
      ↓
enrich
      ↓
store + objects + vector + embed

services/rag-parser 不依赖 bkcrab 数据库、对象存储或 VLM。
```

任务号表示文档组织顺序，不是单一串行链。公共主干是 1→2→3→4→5A→6→7→8→10→11→12；PDF 分支在 5B 许可证闸门通过后执行 9，Office 分支可直接由 5A/6/7/8/12 进入 16。5B/9 被阻断时不得连带阻塞 10–16 的通用/Office 工作。

---

## Phase A：先固定契约与昂贵任务的安全底座

### Task 1: 配置、能力接口与 API DTO

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/env.go`
- Modify: `internal/config/rag_test.go`
- Modify: `internal/setup/handlers_rag.go`
- Modify: `internal/setup/server.go`
- Modify: `web/src/lib/api.ts`
- Test: `internal/setup/handlers_rag_test.go`

- [ ] **Step 1: 写失败测试**

覆盖：

1. `RAGCfg.ApplyDefaults` 设置 sidecar、DocumentAI、页数/图片/像素/并发默认值；
2. `BKCRAB_RAG_ADVANCED_ENABLED`、`BKCRAB_RAG_OFFICE_ENABLED`、`BKCRAB_RAG_ENRICHMENT_ENABLED` 分别覆盖三个默认 `false` 的 feature flag；其它环境变量能覆盖 endpoint/model/timeout/concurrency/endpoint policy，但 secret 不进入日志/序列化测试输出；
3. `GET /api/rag/capabilities` 返回含 `.markdown` 的七种扩展名、按扩展名的真实 max bytes、文档级安全预算，以及独立 `advanced/pdfAuto/office/officeVision/enrichment` capability；
4. capability 读取 TTL health cache，不在 handler 同步探测 sidecar；
5. PDF engine unavailable 只使 `pdfAuto.available=false`，不影响 `office/officeVision`；Office gate/sidecar/golden 任一不满足时 `office.available=false`；TextModel/gate 不满足时 enrichment unavailable，基础 RAG 仍 available；
6. `ParseMode` 类型只接受 `standard/auto`，本 Task 尚不持久化 KB 字段；
7. API DTO 不返回 `ObjectKey`、artifact key 或任何对象存储内部字段；
8. `maxSearchContentBytes` 默认 60 KiB，按 UTF-8 bytes 计数；配置超过现有 Milvus content `maxLength=65535` 时启动失败。

- [ ] **Step 2: 确认测试失败**

Run:

```bash
go test ./internal/config ./internal/setup -run 'TestRAG.*(Defaults|Environment|Capabilities|ParseMode|DTO)' -v
```

Expected: FAIL，缺少新配置、健康快照、能力路由和 ParseMode 类型。

- [ ] **Step 3: 实现最小配置与 DTO**

增加：

- `RAGDocumentAICfg`；
- `RAGParserSidecarCfg`；
- `RAGFeatureCfg{AdvancedParsingEnabled, OfficeParsingEnabled, TextEnrichmentEnabled}`，三者默认 `false`；
- limits 中的页/asset/像素/解压/DPI/展示尺寸字段；
- 单任务 DocumentAI 请求/token/费用/响应/JSON 限制、用户 UTC 日周期配额、pending/reindex rate limits，以及既有 vision/asset/Milvus/GC limits；
- 固定 endpoint host/scheme/private-network policy；HTTP client 禁止 redirect 的执行留给 Task 8；
- 非导出的 `AdvancedConfigured` 只校验 advanced flag、VisionModel 与固定 endpoint policy；`PDFAutoAvailable/OfficeAvailable/OfficeVisionAvailable/EnrichmentAvailable` 严格消费各自 flag 和缓存 health/capability 快照；`AdvancedAvailable` 只派生为 `pdfAuto || officeVision`，不得形成循环依赖；
- `GET /api/rag/capabilities`；
- KB/document camelCase response DTO；
- `ParseMode` 校验类型；KB schema/CRUD 留给 Task 2 同提交完成。

环境变量使用明确的 `BKCRAB_RAG_ADVANCED_ENABLED`、`BKCRAB_RAG_OFFICE_ENABLED`、`BKCRAB_RAG_ENRICHMENT_ENABLED`、`BKCRAB_RAG_DOCUMENT_AI_*`、`BKCRAB_RAG_PARSER_*`、`BKCRAB_RAG_LIMITS_*` 名称/前缀。

- [ ] **Step 4: 通过测试与回归**

```bash
go fmt ./internal/config ./internal/setup
go test ./internal/config ./internal/setup -run 'TestRAG' -v
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/config internal/setup web/src/lib/api.ts
git commit -m "feat(rag): define advanced parsing config and capabilities"
```

---

### Task 2: 数据库迁移——解析快照、chunk catalog 与 assets

**Files:**

- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/rag.go`
- Create: `internal/store/rag_migration_test.go`
- Modify: `internal/store/rag_test.go`
- Modify: `internal/setup/handlers_rag.go`
- Modify: `internal/setup/handlers_rag_test.go`
- Modify: `docs/database.md`

- [ ] **Step 1: 写幂等 migration 失败测试**

测试从旧版四张 RAG 表启动，执行 migration 两次，并验证：

- `rag_kbs.parse_mode/enrichment_enabled`，旧 KB 分别回填 standard/false；
- `rag_documents` 的 source hash、active version、index format、progress、degraded/warning 字段；
- 新表 `rag_document_versions` 保存不可变 parse/index/config snapshot（含 KB 不可变 embedding provider/model/dimensions contract）；
- 新表 `rag_assets / rag_chunks / rag_chunk_assets`；
- 新表 `rag_document_ai_task_budgets / rag_document_ai_user_budgets / rag_document_ai_usage`，含可锁定聚合计数、task/user-period 索引和整数 micro-USD 字段；
- 新表 `rag_index_gc_tasks`，唯一键 `(doc_id,retired_version)`，与 index task 分离；
- SQLite/PostgreSQL 通用 migration 与 MySQL migration 语义一致；
- 旧 KB 自动得到 standard；
- 旧 DONE 文档固定为 `active_version=version,index_format_version=0`；旧非 DONE 只有能证明完整版本时才固定，否则 `active_version=0` 并安排重建；禁止 legacy unpinned；
- 每个 pinned legacy active version 回填 synthetic `rag_document_versions`（legacy-v0 sentinel、enrichment=false、KB chunk/embedding contract、空 DocumentAI 模型、默认预算、空 artifact、document 结果计数），DTO lookup 不缺行；
- document.version、所有 doc_version、asset first/last version 在三种方言统一为 BIGINT，Go 使用 int64；
- 新文档使用 `active_version=0,index_format_version=1`；
- KB create/update 与 store record 在 schema 同一提交开始持久化 `parseMode/enrichmentEnabled`；不可用时仍允许关闭，禁止新开启；
- document DTO 从 active/target snapshot 派生 applied/target parseMode、`needsReparse/needsReindex`；
- 旧 chat sources 不受影响。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/store -run 'TestRAGMigration|TestRAGChunk|TestRAGAsset' -v
```

- [ ] **Step 3: 实现 record 与 CRUD**

增加：

- `RAGAssetRecord`；
- `RAGChunkRecord`；
- `RAGChunkAssetRecord`；
- `RAGDocumentVersionRecord` 与不可变 snapshot CRUD；
- `RAGIndexGCTaskRecord` 与 delayed-task CRUD；
- `RAGDocumentAITaskBudgetRecord / RAGDocumentAIUserBudgetRecord / RAGDocumentAIUsageRecord` 及幂等 reserve/mark-sent/commit/release 事务 API；reserve 固定先锁 user-period 聚合行、再锁 task 聚合行、最后写 usage，禁止无锁 `SUM + INSERT`；
- version 输入字段创建后不可变；artifact/count/degraded/warning/status 结果字段创建时为空/0，只能经 IndexFence CAS 或 spec 明列的 claim/reclaim/supersede/activate/GC system-transition CAS 更新；
- 批量写新版本 chunk payload；
- 按命中 ref 批量读取 chunk；
- 按 chunk refs 批量读取 assets；
- content-addressed asset insert/upsert（binary/display 字段不可变）并更新 first/last seen version；caption/OCR 只写 versioned mapping；
- 删除某 doc version 的 chunk/mapping；
- 文档删除时删除 asset/chunk 元数据。

所有批量 API 必须有参数上限并使用占位符，不拼接用户值。

- [ ] **Step 4: 通过数据库测试**

```bash
go fmt ./internal/store ./internal/setup
go test ./internal/store -run 'TestRAG' -v
go test ./internal/setup -run 'TestRAG.*(ParseMode|DocumentDTO)' -v
```

如配置了 MySQL 集成环境，再运行项目现有 MySQL 门控测试。

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/setup docs/database.md
git commit -m "feat(store): persist rag parse snapshots chunks and assets"
```

---

### Task 3: Durable task claim、doc_version 与 active_version

**Files:**

- Modify: `internal/store/rag.go`
- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Modify: `internal/store/store.go`
- Create: `internal/store/rag_task_claim_test.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/rag/pipeline.go`
- Create: `internal/rag/pipeline_test.go`
- Modify: `internal/rag/vector/vector.go`
- Modify: `internal/rag/vector/milvus.go`
- Modify: `internal/rag/vector/fake.go`
- Modify: `internal/rag/vector/chunks.go`
- Modify: `internal/rag/vector/milvus_test.go`
- Modify: `internal/rag/vector/fake_test.go`
- Modify: `internal/rag/vector/chunks_test.go`
- Modify: `docs/database.md`

- [ ] **Step 1: 写并发/崩溃失败测试**

必须覆盖：

1. 两个 worker 同时 claim 同一 task，只有一个成功；
2. 未过期 lease 不能被其它 worker 抢走；
3. lease 过期后可恢复；
4. heartbeat/progress/warning/finish 只能由匹配 `taskID+docVersion+claimGeneration+leaseOwner` 的 fence 更新；
5. lease 过期 reclaim 原子分配从未使用的新 doc_version，旧 worker 的迟到 vector 写落到旧主键空间；
6. channel 满后后台 poller 能继续捞取 PENDING，不依赖重启；
7. 新索引失败时 `active_version` 保持旧值；
8. stale task 不覆盖新版本 DONE/FAILED/progress；
9. heartbeat CAS 失败立即 cancel worker context；
10. `AdvanceDocumentVersionAndCreateTask(expectedVersion,snapshot)` 并发时只有一个成功，新任务 supersede 旧非终态任务；
11. activate 事务校验完整 fence、未过期 lease、`task.doc_version == document.version`；
12. lease/next_run_at 使用数据库时间；transient 指数退避，permanent error 不重试；
13. 存量 task migration fixture 含“多个 DONE + 一个 RUNNING”“多个非终态”“孤儿 task”“snapshot 构造失败”：先 nullable，归档历史终态，每 doc 只保留最新 runnable，其余 supersede；survivor 分配全新版本和完整 snapshot，最后才加 NOT NULL/唯一约束，执行两遍幂等。
14. 当前 provider fingerprint 与 version snapshot 不匹配时 supersede/新建版本，不能在原 doc_version 下换 endpoint/model；
15. 每个 version 固定 KB 创建时的 embedding provider/model/dimensions；KB update 不能产生混合 embedding contract。
16. Task 3 新增的 task lease/retry 列和 `rag_index_tasks(status,next_run_at,lease_until,created_at)`、`rag_index_gc_tasks(status,next_run_at,lease_until,created_at)` 索引在三种方言存在且 migration 幂等。
17. version 状态覆盖 PENDING→RUNNING→DONE→RETIRED→GCED、FAILED、SUPERSEDED；lease reclaim/new reindex 都原子终态化旧 RUNNING/PENDING version，不留下永久 RUNNING 行。
18. 现有 `retry_count/max_retry` 统计 transient failure/lease expiry，`claim_generation` 只做每次 claim 的 fencing；不存在含义重叠的 index-task `attempt_count`，预算耗尽后 task/version FAILED 且旧 active 仍可检索。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/store ./internal/rag -run 'TestRAGTaskClaim|TestPipeline.*(Lease|Version|Poller|Active)' -v
```

- [ ] **Step 3: 实现 task pump**

实现：

- `AdvanceDocumentVersionAndCreateTask(expectedVersion, snapshot)`；
- `ClaimRAGIndexTask(workerID, leaseDuration)` 的数据库时间 SQL compare-and-set；首次 claim 置 task/version RUNNING，retry/reclaim 时终态化旧 version、更新 task/document doc_version 并递增 claim_generation；
- 接受不可变 `IndexFence` 的 `Heartbeat/UpdateProgress/Retry/FailRAGIndexTask` 与单事务 `ActivateAndFinishRAGIndexTask`；transient retry 原子设置旧 version FAILED、task PENDING/next_run_at，下一次 claim 使用新 doc_version；
- 新 reindex 的 supersede 事务同时终态化旧 task 和其当前 version；所有 system transition 也必须带旧 task/version/generation/status 条件 CAS；
- 保留现有 `retry_count/max_retry` 并删除新增 `attempt_count` 的设想；retry_count 只因 transient failure/lease expiry 增加，claim_generation 每次成功 claim 增加；
- 分阶段 legacy-task migration 与运行时 `SnapshotBuilder`；无法生成完整 snapshot 的旧 task 不得 runnable；
- 在 `docs/database.md` 固定一次性维护窗口 runbook：先关闭 upload/reindex、停止/缩容全部旧 indexing worker，等待超过旧 lease 上限并确认 heartbeat 不再变化；再由新版本执行 expand/backfill/validate/contract，最后只启动新 worker并恢复入口。明确不支持旧/新 worker 滚动混跑；
- 每实例 worker ID；
- 周期 poller；
- task 每个 claim generation 固定且独占一个物理 doc_version；version snapshot 不从可变 KB/config 重算；
- store/pipeline/vector/fake/Milvus/chunk hydrate 的 DocVersion 全部改为 `int64`，禁止在 Milvus int64 边界转回 Go `int`；
- processing progress 节流写入；
- `active_version`、index task DONE 与独立 `rag_index_gc_tasks` 创建在同一个最终 SQL 事务中完成；GC reclaim 只更新自己的 generation/lease，绝不推进 document.version；
- error classifier 与 `next_run_at` 指数退避。

保留进程内 doc/KB mutex 作为同实例优化，但不能把它当分布式正确性保证。

- [ ] **Step 4: 运行回归**

```bash
go fmt ./internal/store ./internal/rag/...
go test -race ./internal/store ./internal/rag -run 'TestRAGTaskClaim|TestPipeline' -v
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/rag docs/database.md
git commit -m "fix(rag): claim versioned index tasks with leases"
```

---

## Phase B：统一文档协议与本地 parser sidecar

### Task 4: ParsedDocument、Asset ID 与 artifact 协议

**Files:**

- Create: `internal/rag/document/model.go`
- Create: `internal/rag/document/markdown.go`
- Create: `internal/rag/document/artifact.go`
- Create: `internal/rag/document/model_test.go`
- Create: `internal/rag/document/artifact_test.go`
- Create: `internal/rag/assets/assets.go`
- Create: `internal/rag/assets/assets_test.go`
- Modify: `internal/rag/objects/objects.go`
- Modify: `internal/rag/objects/objects_test.go`

- [ ] **Step 1: 写契约失败测试**

断言：

- transient ParsedDocument 的 `BundleEntry` 不参与 JSON，且不得被当作 artifact 编码；
- ParsedDocument 实现幂等 `Close()`；constructor-owned cleanup 在 success/error/cancel 与重复 Close 时恰好释放一次，MD/TXT 为 no-op；
- canonical `ParsedArtifact` JSON round-trip，不含临时路径、asset bytes、对象 key 或 URL；
- unit/location 顺序和 1-based page/slide/sheet；
- occurrence 引用不存在 asset 时校验失败；
- asset ID 对同 doc+hash 稳定、跨 doc 不同；
- 相同 asset 多 occurrence 去重；
- persistence 将每个 `AssetLocalID` 确定性映射为 canonical `AssetID`，并改写 artifact occurrence；
- artifact cache hit 从 SQL/object store 按 asset ID rehydrate；缺 catalog/source object，或 ready asset 缺 display object 时 cache invalid 并 reparse；
- parser-authored `rag-asset://` 能解析，user-authored 不能信任；
- artifact key/cache key 只含安全 path segment；
- ExtractedAsset 不含对象存储 key，bundle entry 只允许受控相对路径；
- 临时 bundle handle 在成功、失败和 cancel 后都关闭/清理；
- objects 支持流式写入、读取单对象和安全删除；
- artifact 超过限额、路径穿越、重复 entry 被拒绝。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/document ./internal/rag/assets ./internal/rag/objects -v
```

- [ ] **Step 3: 实现 domain contract**

严格实现 spec 中的：

- transient `ParsedDocument / ExtractedAsset` 与 canonical `ParsedArtifact / ArtifactAsset`；
- parser-owned `AssetOccurrence / ParseWarning`、持久化后的 `ArtifactOccurrence / AssetRef` 与 local→canonical 映射；
- transient AltText 在发布前按 auto caption→安全 alt→中性占位折叠成唯一 ArtifactOccurrence.Caption，不持久化第二套 fallback；
- `Validate()`；
- normalized Markdown image marker；
- parse fingerprint、page cache key、enrichment cache key；
- `PersistParsedDocument` 的流式保存、canonicalize、原子 artifact 发布，artifact JSON/Markdown 编解码与对象 key helper；

不得把 asset bytes 内联进 JSON，也不得让 parser 生成对象存储 key。所有 Parser 通过 constructor 绑定 cleanup，调用方取得 ParsedDocument 后立即 `defer Close()`；`ParsedArtifact` cache rehydrate 失败必须触发重新解析。

- [ ] **Step 4: 通过测试**

```bash
go fmt ./internal/rag/document ./internal/rag/assets ./internal/rag/objects
go test ./internal/rag/document ./internal/rag/assets ./internal/rag/objects -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/document internal/rag/assets internal/rag/objects
git commit -m "feat(rag): add parsed document and asset contracts"
```

---

### Task 5A: 受限 bundle 协议与 Office sidecar

本 Task 不依赖 PDF engine 许可证决策，可以独立完成和发布；只要 5A 通过，Office converter 就能进入后续 Go 集成。

**Files:**

- Create: `services/rag-parser/pyproject.toml`
- Create: `services/rag-parser/uv.lock`
- Create: `services/rag-parser/Dockerfile`
- Create: `services/rag-parser/app/main.py`
- Create: `services/rag-parser/app/protocol.py`
- Create: `services/rag-parser/app/office.py`
- Create: `services/rag-parser/tests/test_protocol.py`
- Create: `services/rag-parser/tests/test_office.py`
- Create: `services/rag-parser/tests/fixtures/generate_minimal.py`
- Create: `services/rag-parser/tests/fixtures/expected_minimal.json`
- Create: `testdata/rag-parser-protocol/v1/manifest-office.json`
- Create: `testdata/rag-parser-protocol/v1/manifest-pdf-analyze.json`
- Create: `testdata/rag-parser-protocol/v1/manifest-pdf-render.json`
- Create: `testdata/rag-parser-protocol/v1/manifest-pdf-failed-page.json`
- Create: `testdata/rag-parser-protocol/v1/health.json`

- [ ] **Step 1: 建 Python 项目并写协议/Office 失败测试**

锁定 Python 3.12、`markitdown[docx,pptx,xlsx]==0.1.6`、Pillow、FastAPI/Uvicorn、pytest 与 ruff；本 Task 不加入任何 PDF engine package。

接口：

```text
GET  /healthz
POST /v1/office/convert?format=docx|pptx|xlsx
```

请求是 multipart 单文件；响应是流式 `application/x-tar`，只允许：

```text
manifest.json
units/0001.md
assets/<local-id>.<safe-ext>
```

用共享 golden 固定 spec 5.4 的完整 lowerCamelCase manifest/health schema：protocolVersion、bundleKind、source/parser、顶层 payload entry path/hash/MIME/size 目录、unit/location、asset localId/entry/尺寸、occurrence、warning 与 capability/limits。`manifest.json` 必须首 entry，后续路径有序；unit/asset/PDF page 引用与 entry 目录严格一一对应并做 hash/size/MIME 对账；analyze-ok、含 embedded XObject/occurrence 的 render-ok 与 failed-page 分别有 fixture 固定 required/forbidden 组合。Office occurrence 只引用 `units[].id`；PDF occurrence 只引用同一 manifest 的 `pages[].unitId`，analyze/render 对同页 unitId 稳定一致。Python golden 和后续 Go decoder 读取同一组 expected JSON。

失败测试覆盖 format/扩展/MIME magic allowlist、部署传入的 `maxInputBytes`（默认 50MB，不得写死）、OOXML zip bomb、输出 tar 相对路径、单 entry/总输出配额、重复/未知/未声明 entry、临时目录清理和 health 版本/limits。用 DOCX/PPTX/XLSX 三种最小 fixture 阻断性验证 MarkItDown 0.1.6 的图片 hook/data URI 与 OOXML 定位能力；另加入 DTD/entity、外部 relationship、attachedTemplate/OLE/altChunk/externalLink、`file://`、UNC/绝对路径和跨请求临时文件读取恶意 fixture，证明不会读取/回传本地内容。普通 hyperlink 只保留可见文本，不 dereference。

- [ ] **Step 2: 确认失败**

```bash
cd services/rag-parser
uv run pytest tests/test_protocol.py tests/test_office.py -q
```

- [ ] **Step 3: 实现协议与 Office converter**

要求：

- MarkItDown 只调用 `convert_stream()`，不提供 URL/URI 参数；
- 不安装/启用 `markitdown-ocr`；
- MarkItDown 前用禁 DTD/entity/network 的 XML parser 预检 OOXML，拒绝危险 external relationships/local targets；每请求独立随机 0700 临时目录；
- Office wrapper 只使用 spike 已验证的 hook/data URI/sentinel 行为；不精确时从 OOXML 提取并按已记录粒度降级关联、写 warning，不能伪称精确原位；
- tar 响应按 spec 5.4 写完整 manifest-first bundle，不在 sidecar 内构造整个 bundle bytes；
- Pillow 在读取像素前设置 decompression bomb 限制；
- 临时目录在成功、失败和 request cancel 后清理；
- 日志只记录 request ID、格式、计数、耗时，不记录文档正文。

- [ ] **Step 4: 通过测试和容器 smoke**

```bash
cd services/rag-parser
uv run ruff check .
uv run pytest tests/test_protocol.py tests/test_office.py -q
docker build -t bkcrab-rag-parser:test .
```

启动容器后 `/healthz` 必须返回 protocol/service 版本、运行时 input/output limits、完整 Office capability 和 `pdf.enabled=false`；没有 PDF engine 不能导致 health 失败。

- [ ] **Step 5: Commit**

```bash
git add services/rag-parser testdata/rag-parser-protocol
git commit -m "feat(rag-parser): add sandboxed office conversion sidecar"
```

---

### Task 5B: PDF engine 许可证 ADR、adapter 与端点

本 Task 以已评审接受的许可证 ADR 为硬前置；没有选定方案时保持 PDF capability unavailable，但不得回滚或阻塞 Task 5A 的 Office 能力。

**Files:**

- Create: `services/rag-parser/docs/pdf-engine-adr.md`
- Create: `services/rag-parser/app/pdf_engine.py`
- Create: `services/rag-parser/app/pdf.py`
- Create: `services/rag-parser/tests/test_pdf.py`
- Modify: `services/rag-parser/pyproject.toml`
- Modify: `services/rag-parser/uv.lock`
- Modify: `services/rag-parser/Dockerfile`
- Modify: `services/rag-parser/app/main.py`
- Modify: `services/rag-parser/app/protocol.py`
- Reuse: `testdata/rag-parser-protocol/v1/manifest-pdf-analyze.json`
- Reuse: `testdata/rag-parser-protocol/v1/manifest-pdf-render.json`
- Reuse: `testdata/rag-parser-protocol/v1/manifest-pdf-failed-page.json`

- [ ] **Step 1: 先完成并批准许可证 ADR**

ADR 必须明确选择且批准恰好一种结果：接受 PyMuPDF/AGPL 合规义务、记录已购商业许可证的内部证明位置，或批准许可证兼容的替代 engine 与能力差异。不得提交密钥/合同，也不得在 ADR 未批准时把 PyMuPDF 当作普通 MIT/BSD 依赖写入 lock/image。

若没有可接受选项，本 Task 在这里停止并维持 `pdf=false`；Office 路径继续实施。

- [ ] **Step 2: 写 PDF engine contract 失败测试**

覆盖：

- adapter 暴露逐页文字/block/bbox/内嵌图分析和按页渲染；
- `POST /v1/pdf/analyze` 不返回全篇高分辨率位图；
- `POST /v1/pdf/render` 只接受服务端页码 allowlist，并只返回选中页固定 DPI render/内嵌图；
- manifest 每页包含 `status/errorCode/nativeMarkdownEntry/renderEntry/primitiveEntry`，并严格验证 bundleKind/status 的 required/forbidden 矩阵：ok 的 errorCode 必须空；analyze-ok 只要求 native+primitive，render-ok 只要求 render；failed 保留 page+确定性 unitId、只允许 allowlisted errorCode 且不能有该页资源；单页失败不影响其它页；
- analyze 覆盖 1..N 全部页且无重复/缺口；render 响应页集合与请求 allowlist 恰好相等，失败页也必须显式返回，禁止静默漏页或附加页；
- PDF 顶层 units 固定为空，`pages[].unitId` 是 occurrence 的 unit 声明；render fixture 返回内嵌 asset/occurrence 并证明 unitId/page/location 与 analyze 一致，未知页、重复 unitId 或悬空 XObject 拒绝；
- PDF manifest 仍使用 spec 5.4 lowerCamelCase canonical schema；page primitive 严格包含尺寸/文字统计、normalized block/image bbox 与固定 signals，Go/Python golden 字节语义一致；
- 页数、像素、DPI、超时、输出 bundle 配额和 cancel 均受限；
- `/healthz` 返回 ADR 选择的 engine 名称/版本与 `pdf=true`。

- [ ] **Step 3: 确认失败**

```bash
cd services/rag-parser
uv run pytest tests/test_pdf.py -q
```

- [ ] **Step 4: 实现 adapter、端点与隔离测试**

只通过 `PDFEngine` adapter 使用 ADR 选定依赖；协议层不能泄漏 engine-specific 类型。对真实最小 PDF 和故障 fake 运行测试，确保 analyze/render 都流式产出受限 bundle、临时目录在所有退出路径清理，日志不含正文或图片字节。

```bash
cd services/rag-parser
uv run ruff check .
uv run pytest -q
docker build -t bkcrab-rag-parser:test .
```

- [ ] **Step 5: Commit**

```bash
git add services/rag-parser
git commit -m "feat(rag-parser): add license-approved pdf engine adapter"
```

---

### Task 6: Go sidecar client、tar 防护与 artifact 持久化

本 Task 只以前置 Task 5A 的 protocol/Office endpoint 为必要依赖；Task 5B 未完成时，PDF client 方法返回 typed `ErrCapabilityUnavailable`，Office 仍可正常使用。

**Files:**

- Create: `internal/rag/parse/sidecar/client.go`
- Create: `internal/rag/parse/sidecar/protocol.go`
- Create: `internal/rag/parse/sidecar/client_test.go`
- Create: `internal/rag/parse/parser.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/gateway/gateway.go`

- [ ] **Step 1: 写 fake HTTP 失败测试**

覆盖：

- multipart 请求流式发送，不把 source 全部读进内存；
- Python/Go 共读 `testdata/rag-parser-protocol/v1`，严格解码 protocolVersion/bundleKind 与完整 lowerCamelCase manifest/health；未知字段、版本不匹配或 bundleKind/page status entry required/forbidden 组合非法均拒绝；
- 读取合法 tar 为带受限 bundle handle 的 transient ParsedDocument primitive；
- tar 边校验边写入配额临时目录，300 页/多图 bundle 不表示为 `[][]byte`；
- per-page failed status 只降级对应页；
- analyze/render 用稳定 `pages[].unitId` 合并；PDF occurrence 必须引用同一 render manifest 的 ok page unit，page 集合与请求不完全相等、location 不一致、ok errorCode 非空、failed 页带资源、重复/未知 unitId 或悬空 asset 一律拒绝；
- 拒绝 `../`、绝对路径、Windows volume path、symlink/hardlink、重复文件、未知 entry；
- 限制 entry 数、单 entry、总解压字节；
- 取消 context 能终止请求；
- timeout/5xx/invalid manifest 错误分类；
- 后台 TTL health probe 驱动 capabilities，HTTP handler 不同步探测；
- main effective per-extension limit 对 sidecar 格式取 config 与 health `maxInputBytes` 的较小值，`.markdown` 与预算字段进入 capabilities；
- `PersistParsedDocument` 流式保存 asset、完成 local→canonical ID 映射并原子发布 ParsedArtifact；
- ParsedArtifact cache hit 按 asset ID rehydrate，catalog/source object 或 ready asset 的 display object 缺失时 cache invalid 并重新解析；
- 临时文件在成功、失败和 cancel 后都清理。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/parse/sidecar -v
```

- [ ] **Step 3: 实现 client 与装配**

`Service` 注入窄接口：

```go
type PrimitiveExtractor interface {
    ConvertOffice(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error)
    AnalyzePDF(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error)
    RenderPDF(ctx context.Context, source document.Source, pages []int) (*sidecar.BundleHandle, error)
}
```

生产使用 HTTP client；测试使用 fake。protocol decoder 使用 `DisallowUnknownFields`、manifest-first tar 和声明 entry 的 hash/size/MIME 对账；health snapshot 记录独立 `office/pdf` capability 与 limits。5B 未完成或 PDF engine 不可用时两个 PDF 方法返回 `ErrCapabilityUnavailable`，不能影响 Office。gateway 启动不应因 sidecar 不健康而导致基础 RAG 整体启动失败。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/rag/parse/... ./internal/rag ./internal/gateway/...
go test ./internal/rag/parse/... ./internal/rag ./internal/gateway/... -run 'Test.*Sidecar|Test.*RAG' -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/parse internal/rag/service.go internal/gateway/gateway.go
git commit -m "feat(rag): integrate bounded document parser sidecar"
```

---

### Task 7: 全来源 Markdown 安全规范化与 Parser facade

**Files:**

- Modify: `go.mod`
- Modify: `go.sum`
- Create: `internal/rag/parse/markdown.go`
- Create: `internal/rag/parse/text.go`
- Create: `internal/rag/parse/sanitize.go`
- Create: `internal/rag/parse/sanitize_test.go`
- Rename/Modify: `internal/rag/parse/pdf.go` → `internal/rag/parse/pdf_native.go`
- Modify: `internal/rag/parse/parse.go`
- Modify: `internal/rag/parse/parse_test.go`
- Add fixtures under `internal/rag/parse/testdata/`

- [ ] **Step 1: 写图片清理与 streaming 失败测试**

MD 样例同时含：

- inline image；
- reference image；
- relative/network/data URI；
- HTML img/picture/source；
- SVG image、CSS background/url() 与其它 raw HTML 绕过；
- fenced code 内的 `![]()`；
- 用户伪造 `rag-asset://`；
- 普通超链接。

断言 parser 从不发网络请求；图片节点变成忽略提示和 warning；代码 fence/链接正文保留。TXT 的图片样式只是文本。再把 raw HTML、javascript/data/file/custom scheme、外部 image、未解析 rag-visual、伪 rag-asset、控制字符等同一 adversarial corpus 分别标成 user MD、Office、PDF native、VLM 来源；除“当前 occurrence map 中由 parser 生成的 typed rag-asset”外结果完全一致且二次 normalize 幂等。

另测配置上限大小的 reader 不被 `io.ReadAll`：使用只允许小块 Read 的 guard reader 或临时文件；standard PDF 和 auto sidecar-down fallback 都必须走新的临时文件/ReaderAt 实现。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/parse -run 'TestMarkdownImagePolicy|TestTextParser|TestParserDoesNotReadAll' -v
```

- [ ] **Step 3: 引入 Goldmark 并实现**

使用 Goldmark AST + GFM，不用正则替换。实现所有 parser 共用的 `NormalizeMarkdown(units, occurrenceMap, allowInternalAssets)`；它必须在 persist/publish ParsedArtifact 前执行，统一转义 raw HTML、清理危险 link/image scheme、验证 internal occurrence，代码 fence 内容原样。现有 `parse.Parse` 暂留 compatibility wrapper 仅供旧测试；新 pipeline、standard PDF 和 auto fallback 都只依赖 streaming Parser 接口，禁止回调旧 `io.ReadAll` 路径。

上传层认识 `md/markdown/txt/pdf/docx/pptx/xlsx` 并明确拒绝旧 Office，但 docx/pptx/xlsx 必须同时检查 `office.available`；Task 16 converter/golden 未完成前 capabilities 与 handler 都不能开放 Office 上传。auto PDF 根据 `pdfAuto.available` 走 advanced 或带 warning 的 native fallback；不能因 PDF unavailable 阻止 `officeVision`。

- [ ] **Step 4: 回归**

```bash
go fmt ./internal/rag/parse/...
go test ./internal/rag/parse/... -v
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add go.mod go.sum internal/rag/parse
git commit -m "feat(rag): normalize markdown and text with safe image policy"
```

---

## Phase C：PDF auto 纵向闭环

### Task 8: VLM 窄接口、JSON schema 与逐页缓存

**Files:**

- Create: `internal/rag/vision/vision.go`
- Create: `internal/rag/vision/openai.go`
- Create: `internal/rag/vision/schema.go`
- Create: `internal/rag/vision/image.go`
- Create: `internal/rag/vision/cache.go`
- Create: `internal/rag/vision/budget.go`
- Create: `internal/rag/vision/vision_test.go`
- Create: `internal/rag/vision/image_test.go`
- Create: `internal/rag/vision/budget_test.go`
- Create: `internal/rag/vision/openai_test.go`
- Modify: `internal/store/rag.go`
- Create: `internal/store/rag_document_ai_budget_test.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/gateway/gateway.go`

- [ ] **Step 1: 写协议失败测试**

覆盖：

- page image 只发送给 DocumentAI provider；
- prompt 要求 JSON、GFM table、fenced code、bbox 0..1000；
- schema 拒绝重复 visual key、未引用/多引用 marker、非法 bbox、外部 URL、data URI；
- 第一次非法允许一次 repair，第二次非法返回 typed error；
- page/image cache key 包含规范化视觉 bytes、provider fingerprint、model、prompt 与各自 schema version；同 model 名但 endpoint/provider 变化必须 miss；
- Office `DescribeImage` typed schema 校验 caption/OCR/kind/decorative/confidence、字段长度、未知字段、URL/base64/internal scheme，并允许一次受预算 repair；
- cache hit 不调用 provider；
- 只允许 `apiType=openai-compatible`；图片 resize/安全重编码后校验原始、编码、base64 膨胀三层字节上限；
- 同一个 `TaskDocumentAIBudget` 并发处理 Vision/Image/repair 预留；Reserve/MarkSent 都验证当前 IndexFence，旧 generation/过期 lease 不能预留或出网；task lease reclaim 恢复 ledger 已用额度，用户 UTC 日配额、pending/reindex gate 和单任务 requests/tokens/cost 任一耗尽都不请求；已发出 timeout 仍计费，明确未发送才 release；
- 两个 worker/并发 block 同时 reserve 最后一份额度时只有一个事务成功；SQLite/PostgreSQL/MySQL 不因 lost update 超发；旧 worker 与 reclaim 竞态时，最多只有当前 fence 能进入 SENT；
- usage 严格只允许 `RESERVED→SENT→COMMITTED/OVERRUN` 或 `RESERVED→RELEASED`；过期 RESERVED 安全释放，崩溃遗留 SENT 按估算结算，late response 只能结算自己的 attempt，所有 transition CAS 幂等；
- 上游禁止 redirect，endpoint scheme/host/private-address policy 生效；响应压缩/解压 bytes、output tokens、JSON 深度/数组/字段长度有硬限制；
- 429/5xx/timeout 分类；
- 日志不出现 base64 和 prompt 全文；
- global semaphore 限制并发。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/vision ./internal/store -run 'Test.*(Vision|Image|DocumentAIBudget)' -v
```

- [ ] **Step 3: 实现 Transcriber**

实现入库专用的 OpenAI-compatible DocumentAI client；可以复用 provider 的基础 HTTP/ContentPart 类型，但不复用知识库聊天的 defaultLLM 实例或其 model 选择。私有图片只在该 client 内编码为受控 data URL。client 不注册 tools、不带 agent history；固定指令与 JSON-escaped untrusted data 分离。HTTP transport 固定管理员 endpoint、禁 redirect/动态 URL，并执行响应/JSON/output 上限。

实现 durable `TaskDocumentAIBudget`：所有 outbound attempt 执行 `Reserve(fence) → MarkSent(fence) → Commit/Release`；PostgreSQL/MySQL 先 upsert 后按 user-period budget→task budget→index task 固定顺序 `SELECT FOR UPDATE`，SQLite 使用 `BEGIN IMMEDIATE`，持锁后重查 idempotency key、RUNNING/lease/fence 并条件更新两张聚合表，禁止以 `SUM + INSERT` 判余额。MarkSent 在网络写入前再次校验 fence；release/commit 用同一锁顺序回退未发送额度或保守预留差额，late response 只能结算自身 attempt。实际量异常超过预留时补记 OVERRUN 并阻止后续调用；reconciler 释放过期 RESERVED、把超时 SENT 按估算结算，所有状态 CAS。幂等 key 防同一结果重复计账，repair/真正重发各自计账；task_id 跨 doc_version reclaim 不变并恢复额度。Vision、Task 11 Enricher 和 Task 16 Office parser 必须注入同一实例，禁止各自创建满额 counter。

cache 通过对象存储文档前缀持久化；写入使用临时 key/原子替换语义或幂等覆盖。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/rag/vision ./internal/rag ./internal/store ./internal/gateway/...
go test ./internal/rag/vision ./internal/rag ./internal/store ./internal/gateway/... -run 'Test.*Vision|Test.*DocumentAIBudget|Test.*RAG' -v
```

`rag_document_ai_budget_test.go` 的 SQLite 并发测试默认运行；同一 test suite 在设置 `BKCRAB_TEST_POSTGRES_DSN` / 现有 `BKCRAB_TEST_MYSQL_DSN` 时分别连接真实 PostgreSQL/MySQL，验证两个独立连接争抢最后额度、fence reclaim 与 RESERVED/SENT crash reconciliation。仅 fake/store mock 通过不算三方言验收。

- [ ] **Step 5: Commit**

```bash
git add internal/rag/vision internal/rag/service.go internal/store internal/gateway/gateway.go
git commit -m "feat(rag): add cached document vision transcriber"
```

---

### Task 9: PDF auto router、视觉 crop 与逐页降级

硬前置是 Task 5B 的许可证 ADR 与 PDF capability 已完成；未完成时保持 auto PDF unavailable，不能用未批准的 engine 绕过门槛。

**Files:**

- Create: `internal/rag/parse/pdf_auto.go`
- Create: `internal/rag/parse/pdf_auto_test.go`
- Create: `internal/rag/parse/testdata/pdf-auto/`
- Modify: `internal/rag/parse/parser.go`
- Create: `internal/rag/assets/image.go`
- Create: `internal/rag/assets/image_test.go`

- [ ] **Step 1: 写路由表驱动测试**

样例页：

- 纯文本；
- 少文字扫描页；
- 文字 + 20% 图；
- GFM 表格候选；
- code/multicolumn；
- 恰好阈值边界；
- Vision 上限之后的页。

断言规则版本化、路由确定、页顺序不变。

降级测试：

- 某页 sidecar primitive 失败；
- 整个 sidecar unavailable 时 50MB PDF fallback 内存保持有界；
- VLM timeout/invalid JSON；
- render 成功但 VLM 失败时 embedded original 仍绑定 native page unit，以安全 alt/中性失败占位参与命中资源展示；临时整页 render 不被误存为普通图片；
- VLM JSON 合法但遗漏超过 30% 原生文字锚点或异常扩写；
- native 为空；
- 全篇空；
- VLM 成功但视觉 bbox 无法对应 XObject；
- embedded original 与 page crop 两条路径。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/parse ./internal/rag/assets -run 'TestPDFAuto|TestSafeImage' -v
```

- [ ] **Step 3: 实现**

实现：

- spec 中五条 auto 条件；
- `rag-visual://` → occurrence/asset；
- 原图优先、crop fallback；
- display/thumbnail 安全重编码、去 metadata；
- 无安全 decoder 的 SVG/EMF/WMF 保留 source、`display_status=unavailable` 且不进入 AssetRef；
- decorative/small-image 过滤；
- per-page warning 和 degraded；
- standard/native fallback；
- VLM 页降级时保留已提取 embedded asset/occurrence，但没有合法 bbox 就不生成 crop；整个 sidecar unavailable 的 Go fallback 只承诺文字并记录无图片降级；
- progress callback `vision x/y`。

禁止把整页 render 当普通图片重复展示；`scanned_page` 按 spec 单独标记。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/rag/parse/... ./internal/rag/assets
go test ./internal/rag/parse ./internal/rag/assets -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/parse internal/rag/assets
git commit -m "feat(rag): route pdf pages through native or vision parsing"
```

---

### Task 10: Markdown AST/block-aware splitter

**Files:**

- Rewrite: `internal/rag/split/split.go`
- Create: `internal/rag/split/markdown_ast.go`
- Create: `internal/rag/split/table.go`
- Create: `internal/rag/split/code.go`
- Modify: `internal/rag/split/split_test.go`
- Create: `internal/rag/split/table_test.go`
- Create: `internal/rag/split/code_test.go`
- Create: `internal/rag/split/asset_test.go`

- [ ] **Step 1: 写完整边界测试**

必须断言：

- 标题 breadcrumb 与现有 `SearchContent` 行为不回退；
- fenced code 中 heading/image/table 字样不被解析；
- paragraph/list/blockquote 贪心装箱；
- 每个普通文本候选先单独执行超长拆分再参与装箱；“当前 chunk 已接近 512，再遇到一个自身未超过 512 的长句”必须开启新 chunk，任何结果都不得接近 2×ChunkSize；
- table 按行拆且重复表头；
- 超长单元格仍不超过 ChunkSize；
- code 优先函数/空行/行边界并补 fence；
- 超长单行安全切分；
- overlap 不产生半个 table/fence；
- image occurrence 只附着局部 chunk；
- 同 asset 多 occurrence 顺序稳定；
- RawContent、breadcrumb 与 provisional SearchContent 符合契约；
- table/code 预留 `min(EnhancementReserveTokens,ChunkSize/5)`，标题+raw 不侵占 reserve；
- tiny ChunkSize 不 panic。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/split -v
```

- [ ] **Step 3: 实现 AST splitter**

保留 `EstimateTokens` 作为一期 estimator；Task 10 只保证 breadcrumb+raw+reserve 的应用预算，最终 SearchContent 上限在 Task 11 的 FinalizeChunk 断言。删除 pipeline 中按 format 选择 `Markdown/SlidingWindow` 的分支。

普通文本仍可复用已经修复过的 `splitToFit`；代码/表格/image 使用独立 block splitter。

- [ ] **Step 4: 测试与 fuzz**

```bash
go fmt ./internal/rag/split
go test ./internal/rag/split -v
go test ./internal/rag/split -run Fuzz -fuzz=FuzzMarkdownSplit -fuzztime=20s
```

若 CI 不运行 fuzz，保留 fuzz target，常规测试运行 seed corpus。

- [ ] **Step 5: Commit**

```bash
git add internal/rag/split
git commit -m "feat(rag): split normalized markdown by semantic blocks"
```

---

### Task 11: 表格/代码 Enricher 与缓存

**Files:**

- Create: `internal/rag/enrich/enrich.go`
- Create: `internal/rag/enrich/openai.go`
- Create: `internal/rag/enrich/cache.go`
- Create: `internal/rag/enrich/enrich_test.go`
- Create: `internal/rag/enrich/openai_test.go`
- Modify: `internal/rag/chunktext/chunktext.go`
- Modify: `internal/rag/chunktext/chunktext_test.go`

- [ ] **Step 1: 写失败测试**

覆盖：

- paragraph/image 不调用 Enricher；
- system `TextEnrichmentEnabled`、TextModel 和 KB `enrichmentEnabled` 任一为 false 时严格 no-op；standard 模式不能绕过 opt-in；
- table/code 输出 schema；
- prompt 把文档当不可信数据；
- Enhancement 长度被限制；
- Enricher 接收每 chunk 实际剩余 token/字符预算，预算为 0 不调用；
- cache key 包含 raw/kind/text provider fingerprint/model/prompt/schema，provider endpoint 改变必须 miss；
- 与 Vision/ImageTranscriber 共用同一个 durable TaskDocumentAIBudget；并发 block + repair 不得各花一份完整预算，task retry/reclaim 不重置；
- timeout/invalid JSON → enhancement 为空且返回 warning；
- TextModel 未配置 → no-op；
- SearchContent 与公共 `chunktext.Answer(raw,enhancement)` 的标注格式；
- FinalizeChunk 先裁/丢 Enhancement，绝不为增强裁 RawContent；估算仍超限时确定性重拆 raw；
- embedding/Milvus 前 provider tokenizer + `maxSearchContentBytes` 二次保护；用多字节 UTF-8 fixture 证明按 bytes 而非 rune 计数，最终 payload ≤60 KiB/collection maxLength；
- 原始 table/code 一字不被模型结果替换；
- concurrency 上限。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag/enrich ./internal/rag/chunktext -v
```

- [ ] **Step 3: 实现**

`Enricher` 接口不依赖 splitter 的内部 AST 类型，只接收稳定 `EnrichableBlock` 与共享 `*TaskDocumentAIBudget`。client 无 tools/agent history，固定指令与 JSON-escaped untrusted table/code data 分离，并复用 Task 8 endpoint/redirect/response hardening。缓存存文档前缀。错误不冒泡成整篇失败。新增 `FinalizeChunk` 统一派生 SearchContent，并与 `Hit.AnswerText()` 共用 `chunktext.Answer`，不持久化第二份 AnswerText；同时执行 estimator/provider/bytes 边界。文档超过 block 或共享 requests/tokens/cost/用户周期预算后，其余 block no-op + warning。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/rag/enrich ./internal/rag/chunktext
go test ./internal/rag/enrich ./internal/rag/chunktext -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag/enrich internal/rag/chunktext
git commit -m "feat(rag): enrich table and code chunks without replacing source"
```

---

### Task 12: Pipeline 编排、artifact 复用与原子 active-version

**Files:**

- Modify: `internal/rag/pipeline.go`
- Modify: `internal/rag/pipeline_test.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/rag/vector/vector.go`
- Modify: `internal/rag/vector/milvus.go`
- Modify: `internal/rag/vector/fake.go`
- Modify: `internal/rag/vector/chunks.go`
- Modify: `internal/rag/vector/milvus_test.go`
- Modify: `internal/rag/vector/fake_test.go`
- Modify: `internal/rag/vector/chunks_test.go`
- Modify: `internal/rag/metadata.go`
- Modify: `internal/rag/metadata_test.go`

- [ ] **Step 1: 写阶段顺序与 crash-point 测试**

测试精确顺序：

```text
source hash/load
→ parse cache hit/miss
→ normalize every MarkdownUnit with typed occurrence map
→ persist assets/artifact
→ split
→ enrich
→ finalize SearchContent + shared Answer helper contract
→ embed SearchContent
→ stage SQL chunks/assets
→ upsert Milvus
→ ActivateAndFinishRAGIndexTask（同一 SQL 事务完成 CAS 激活、version/task DONE 与 delayed GC 入队）
```

每个边界注入失败，断言：

- activate 之前旧 active version 仍可见；
- 新 SQL payload 写入但 Milvus 失败时不会激活；
- Milvus 新版本写入但 activate 失败时搜索仍过滤旧版；
- lease reclaim 后旧 worker 迟到 upsert 的 doc_version 不会覆盖/激活新版本；
- activate 后 cleanup 失败只重试独立 GC，不重新 parse/embed；每个 retired active version 从自己的 retired_at 独立保留完整 grace；
- retry 命中 page/enrich cache，不重复 fake VLM；
- Vision/Office image/repair/Enricher 共用一个 task_id-scoped durable budget；并发和 reclaim 后 requests/tokens/cost 总额不重置，用户日配额也生效；
- 只改 chunk size 改 index_fingerprint 并复用 parse artifact；
- parser/vision prompt 变化强制 reparse；
- asset ID 跨 reparse 稳定；
- progress/stage/degraded/warning 正确；
- source 通过流/临时文件处理，没有无界 ReadAll。
- metadata/source sampling 读取 active_version，不读取 target document.version。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag -run 'TestPipeline.*(Stage|Failure|Cache|Active|Progress)' -v
go test ./internal/rag/vector -run 'Test.*DocVersion' -v
```

- [ ] **Step 3: 重构 doIndex**

拆成可测试的小阶段：

- `loadOrParseArtifact`；
- `normalizeParsedDocument`（所有来源共用 Task 7 sanitizer）；
- `persistParsedAssets`；
- `splitAndEnrich`；
- `finalizeChunks`；
- `embedChunks`；
- `stageIndexVersion`；
- `activateAndFinishIndexVersion`（内部只调用 store 的单事务 `ActivateAndFinishRAGIndexTask`，不再有第二次 finish/GC enqueue）。

Milvus `SearchHit` 返回 DocVersion 和内部 SearchContent。旧 `chunktext.Body` 只允许 `index_format_version=0` 的 pinned legacy fallback，不再是新版本正文来源。每个 Milvus batch 前检查 IndexFence 以减少浪费，但正确性依赖独占 doc_version 主键空间和激活 CAS。

- [ ] **Step 4: 全 RAG 回归**

```bash
go fmt ./internal/rag/...
go test -race ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag
git commit -m "feat(rag): index parsed artifacts with atomic active versions"
```

---

### Task 13: Search hydrate、reranker 增强文本与 AssetRef

**Files:**

- Modify: `internal/rag/search.go`
- Modify: `internal/rag/search_test.go`
- Modify: `internal/rag/vector/vector.go`
- Modify: `internal/rag/vector/fake.go`
- Modify: `internal/rag/vector/milvus.go`
- Modify: `internal/rag/rerank/http_test.go`
- Modify: `internal/rag/agent_adapter.go`

- [ ] **Step 1: 写检索失败测试**

覆盖：

- SQL active map 下推到单 dense、rewritten dense、HyDE dense、BM25 sparse 的每条 Milvus ANN route；
- Milvus filter encoder 只接受 canonical DB doc ID/int64 version，正确转义并拒绝超限；query 文本绝不进入 filter；
- 未激活新版本分数最高时仍只能返回旧 active version，不能先 topK 后过滤；
- `active_version=0` 不访问/不返回 Milvus；只有 `index_format_version=0` 的 pinned legacy 缺 catalog时回退正文、assets 为空；
- candidate-level batch hydrate raw/enhancement/SearchContent/location 在 reranker 前完成；
- reranker 收到完整 SearchContent（含图片/表格/代码增强）；
- 只为最终 top-N 查 assets；
- assets 按 occurrence ordinal 稳定；
- 同 asset 在多个 hit 中可重复引用，但 response helper 可去重；
- decorative asset 不返回；
- orphaned doc/chunk/asset 不泄漏；
- cross-KB 合并仍稳定；
- `FormatHits` 给模型的是共享 `chunktext.Answer` 派生文字，不含 asset ID/URL/object key。
- `Hit.Content` JSON 保持 RawContent，`Hit.AnswerText()` 统一派生 prompt/tool 文字；
- GC 竞态导致 hydrate 缺失且 active map 已改变时，整次 search 最多重试一次。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag -run 'TestSearch.*(Hydrate|Active|Asset|Legacy|Rerank)|TestFormatHits' -v
```

- [ ] **Step 3: 实现**

每个 KB 搜索开始先批量读取 `doc_id → active_version/index_format_version` 一致性快照。按 version/doc ID 稳定排序，为每组构造括号化 predicate，再用括号化 OR 合成唯一字节稳定表达式，并在 `SearchOption` 及每个 `AnnRequest` 上调用 `WithFilter`。Task 1 配置的 `maxMilvusFilterBytes=32KiB` 启动校验保证现有 `maxDocsPerKB=200` 的最坏表达式可容纳，并由 Milvus 集成测试验证；超限直接拒绝配置/查询，禁止用“多取一些再 post-filter”承担隔离。结果再按同一 SQL map 二次校验。候选 chunk payload 使用一次 SQL batch hydrate，禁止每 hit N+1；最终 top-N 再第二次 batch hydrate assets。

`rag.Hit` 添加 optional `enhancement/assets`；`Content` 是 RawContent；内部 `DocVersion/SearchContent` 不对 JSON 暴露。增加 `AnswerText()` 与 `BuildRAGResourceRefs(hits)`。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/rag/...
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag
git commit -m "feat(rag): hydrate search hits with raw chunks and assets"
```

---

### Task 14: 鉴权 asset API 与知识库问答模型边界

**Files:**

- Create: `internal/setup/handlers_rag_assets.go`
- Create: `internal/setup/handlers_rag_assets_test.go`
- Modify: `internal/setup/handlers_rag.go`
- Modify: `internal/setup/handlers_rag_test.go`
- Modify: `internal/setup/server.go`

- [ ] **Step 1: 写安全与回答边界失败测试**

asset API：

- owner 200；
- 跨用户与未知 ID 使用一致的 404/403 策略；
- platform admin actAs 行为与其它 RAG API 一致；
- thumbnail/original-display variant；
- Content-Type/Disposition/`Cache-Control: private, no-cache`/派生字节 ETag/nosniff；
- `Cross-Origin-Resource-Policy: same-origin`、无宽泛 CORS、项目统一 SameSite cookie policy；
- If-None-Match → 304；
- 身份/租户/DELETING 校验先于 If-None-Match/ETag/object read；对象仍存在但 doc/KB/user 已 DELETING 时立即统一 404；
- 数据从对象存储流式输出；
- source object key 永不出现在响应；
- unsafe source MIME 只返回安全 raster；`display_status!=ready` 不进入 Hit.Assets。

回答：

- `buildRAGChatPrompt` 包含图片 caption/OCR 和表格/代码的共享 AnswerText；
- source header 含转义/限长的 DocName/SectionTitle/location，正文置于 JSON-escaped untrusted data block；标题只出现一次且仍供回答使用；
- prompt 不含 asset ID/URL/object key/base64；
- 专用知识库 chat 的 provider Message.ContentParts 为 nil；
- chat response hits 包含 AssetRef；
- `rag_chat_turns.sources` 快照与历史接口能回放；
- 老 sources 没有 assets 仍正常。
- adversarial caption/OCR/table/code 中的伪 system/tool 指令不改变 role、ContentParts、权限或资源 metadata；知识库 chat 不注册 tools。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/setup -run 'TestRAGAsset|TestRAGChat.*(Asset|TextOnly|History)' -v
```

- [ ] **Step 3: 实现 endpoints 与 DTO**

注册：

```text
GET /api/rag/assets/{assetId}
GET /api/rag/assets/{assetId}/thumbnail
```

handler 始终通过 asset → doc → KB 做权限检查。
本 Task 的端点只服务 KB owner/admin surface；公共 Agent session 授权端点在 Task 18 实现，不能复用 owner 判断。
所有状态/授权检查在条件缓存处理之前；前端 renderer 禁用 raw HTML/危险 scheme，不使用 `dangerouslySetInnerHTML`。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/setup
go test ./internal/setup -run 'TestRAG' -v
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/setup
git commit -m "feat(api): serve authenticated rag visual resources"
```

此 Task 完成后，后端 PDF auto 纵向闭环成立。

---

## Phase D：前端纵向闭环与 Office

### Task 15: Knowledge UI——能力驱动上传、parseMode、进度与图片 gallery

**Files:**

- Modify: `web/src/lib/api.ts`
- Modify: `web/package.json`
- Modify: `web/src/app/knowledge/page.tsx`
- Modify: `web/src/app/knowledge/chat/knowledge-chat-client.tsx`
- Create: `web/src/components/rag-resource-gallery.tsx`
- Create: `web/src/components/rag-resource-gallery-state.ts`
- Create: `web/src/components/rag-resource-gallery.test.mjs`

- [ ] **Step 1: 写前端纯函数/组件失败测试**

覆盖：

- capabilities 驱动含 `.markdown` 的扩展与 `maxFileBytesByExtension`，不再硬编码 20MB；
- 高级 RAG Switch → `parseMode:auto`；
- `advanced/pdfAuto/office/officeVision` 分别显示 reason；PDF auto 不可用不阻塞 Office auto，auto PDF 明示 native fallback；
- 独立“表格/代码语义增强”opt-in → `enrichmentEnabled`；system/TextModel unavailable 时不能开启，关闭始终允许；
- 开启 auto 前提示页面/Office 图片可能发送给管理员配置的第三方 DocumentAI，并展示文档级预算；
- 开启 enrichment 前单独提示 standard/auto 的表格与代码会外发，并展示 capability 返回的请求/token/费用预算；
- applied/target parseMode 与 `needsReparse/needsReindex` 显示“需要重新解析/重新索引”，按钮文案对应；
- queued/loading/parsing/vision/chunking/enriching/embedding/indexing/finalizing/done/failed 全部 stage 文案，未知值回退“处理中”；
- polling 在 done/failed 停止，切 KB 清旧 timer；
- AssetRef 按 ID 去重、保持最终 hit 顺序、最多 6；
- 老 hit 没 assets；
- thumbnail/display URL 编码与 admin actAs；
- 图片 403/404 不影响回答；
- caption 按纯文本显示；
- Markdown/引用/caption renderer 禁用 raw HTML、dangerous URL scheme 与 `dangerouslySetInnerHTML`；
- gallery state/URL 纯函数；Dialog 键盘与 lazy loading 留到 Task 21 browser smoke，不在没有 DOM runner 时伪称组件自动测试。

- [ ] **Step 2: 确认失败**

```bash
cd web
npm test
```

在 `web/package.json` 增加与现有 `.test.mjs` 一致的 `"test": "node --test"`，不引入 Jest 参数。

- [ ] **Step 3: 实现**

知识库页：

- 创建/编辑 parseMode；
- 创建/编辑 enrichmentEnabled，旧 KB 默认关闭；变更只触发 needsReindex；
- md/txt/pdf 始终按 capability；docx/pptx/xlsx 仅在 `office.available=true` 时开放；
- 显示完整 stage enum 和未知 fallback；
- DONE + degraded 使用琥珀状态；
- 搜索测试 hit 卡片展示 thumbnail。

知识库问答：

- answer 下直接显示“相关图片（来自检索资料）”；
- 引用 details 内同步展示；
- Dialog 预览带文档/location/caption；
- 明确提示“图片由解析阶段转写，回答模型依据文字资料作答”。

- [ ] **Step 4: lint/test/build**

```bash
cd web
npm run lint
npm test
npm run build
```

- [ ] **Step 5: Commit**

```bash
git add web/package.json web/src
git commit -m "feat(web): configure advanced rag and display hit images"
```

若 Task 5B/9 已完成，此 Task 后 PDF auto 的内部管理员端到端闭环成立；若 PDF 许可证分支尚未通过，本 Task 的通用 capability/progress/gallery 仍可供 Office 分支使用。`advanced.enabled` 仍保持 false，直到 Task 19 与发布闸门完成。

---

### Task 16: Office docx/pptx/xlsx 资产位置与 Go parser 集成

本 Task 直接依赖 Task 5A/6 的 Office protocol/client、Task 8 的 `DescribeImage`/共享 budget，以及 Task 12 已装配的统一 parser→artifact→split/enrich→index pipeline（因此传递依赖 Task 4/7/10/11）；不依赖 Task 5B/9 的 PDF engine/router。任务编号不是硬串行屏障：若 PDF ADR 未批准，跳过 5B/9，仍可沿 4→5A→6→7→8→10→11→12→16 完成 Office standard/auto。

**Files:**

- Modify: `services/rag-parser/app/office.py`
- Expand: `services/rag-parser/tests/test_office.py`
- Create: `services/rag-parser/tests/fixtures/generate_office_golden.py`
- Create: `services/rag-parser/tests/fixtures/expected_office_golden.json`
- Create: `internal/rag/parse/office.go`
- Create: `internal/rag/parse/office_test.go`
- Modify: `internal/rag/parse/parser.go`
- Modify: `internal/rag/pipeline_test.go`

- [ ] **Step 1: 写三格式 golden 失败测试**

DOCX：

- heading/list/table；
- 明确 Code/Preformatted 样式与普通等宽文本的保守区分；
- 图片位于两个段落之间；
- 同图片重复 relationship；
- alt text；
- zip bomb/path traversal、DTD/entity、危险 external relationship、file/UNC/绝对本地 target；普通 hyperlink 不 dereference。

PPTX：

- slide 顺序；
- group shape；
- top/left reading order；
- 图片与相邻文字；
- speaker notes 固定保留为对应 slide 末尾的 blockquote；

XLSX：

- 多 sheet；
- GFM table；
- cell anchor；
- 图片无法精确落行时进入该 sheet 的相关图片小节。

Go 侧断言 local asset → stable asset/occurrence；standard 也保存原图并使用 alt/中性占位但不调 VLM；auto 通过 Task 8 `DescribeImage` typed schema 调 VLM；image cache key 含 normalized bytes/provider/model/prompt/schema；与 PDF/Enricher 共用 TaskDocumentAIBudget；单图失败或任一 task/user budget 耗尽 degraded。

- [ ] **Step 2: 确认失败**

```bash
cd services/rag-parser
uv run pytest tests/test_office.py -q
cd ../..
go test ./internal/rag/parse ./internal/rag -run 'TestOffice' -v
```

- [ ] **Step 3: 完成 wrapper 与 Office parser**

wrapper 以 MarkItDown Markdown 为主体，严格实现 Task 5 spike 对每种格式记录的 sentinel/hook/降级粒度。所有 XML/relationship 必须先过 Task 5A 安全预检；不要根据媒体临时文件名做模糊字符串匹配，也不要把 OOXML relationship 误当成天然存在的 Markdown 字符位置。

Code/Preformatted 样式转换 fence；语言不确定时不写 info string；普通等宽文本不误判。standard/auto 语义按 spec；Office sidecar 失败不回退旧 DOCX parser。三格式 golden 与 health feature version 均通过后，才让 `office.available=true` 并允许上传。

- [ ] **Step 4: 双侧回归**

```bash
cd services/rag-parser
uv run pytest -q
cd ../..
go test ./internal/rag/... -v
```

- [ ] **Step 5: Commit**

```bash
git add services/rag-parser internal/rag/parse internal/rag/pipeline_test.go
git commit -m "feat(rag): parse modern office documents through markitdown"
```

---

## Phase E：普通 Agent Web 聊天的 out-of-band 资源

### Task 17: 受信任的 typed ToolResult metadata side-channel

**Files:**

- Create: `internal/agent/tools/result.go`
- Create: `internal/agent/tools/result_test.go`
- Modify: `internal/agent/tools/registry.go`
- Create: `internal/agent/tools/registry_result_test.go`
- Modify: `internal/agent/sdkbridge.go`
- Create: `internal/agent/sdkbridge_result_test.go`
- Modify: `internal/agent/loop.go`
- Modify: `internal/agent/loop_usage_test.go`

- [ ] **Step 1: 写兼容失败测试**

覆盖：

- 现有 `(string,error)` handler 与 `Registry.Execute` 行为不变并自动包装为只有 Text 的 ToolResult；
- 受信 builtin 可通过 typed handler 返回受限 JSON metadata；
- 任意 tool text 即使包含 sandbox/rag prefix、长度前缀或 base64 也不会被解析为 metadata；
- 现有 `MetaSandboxPrefix` 只在原 sandbox builtin 的兼容 adapter 内保持行为；
- 返回给 LLM 的永远只有 `ToolResult.Text`；
- typed tool metadata 只在当前 turn 的 SDK Data/toolCallResult 内存通道存在，不持久化到 tool message，也不发 tool-result SSE；
- malformed/oversized metadata 不 panic、不持久化、不进入 LLM；
- producer + metadata key + shape/count/bytes allowlist，禁止任意 UI 注入；
- compaction/provider 序列化不发送 Metadata。
- 两个并发同名 rag_search 的 SDK responses 通过各自 `ToolResult.Data` 携带私有 bridge data，不串 metadata；error response 不聚合 Data。
- `GetResultFunc` 与 `ExecuteResult` 都执行同一 registry validator；toolAdapter 不能取得/缓存 raw ResultHandler，malformed metadata 通过 accessor/SDK path 也会被剥离。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/agent/tools ./internal/agent -run 'Test.*(ToolResult|ResultMetadata|Metadata)' -v
```

- [ ] **Step 3: 实现 typed registry path**

```go
type ResultMetadata map[string]json.RawMessage

type ToolResult struct {
    Text     string
    Metadata ResultMetadata
}

type ResultHandler func(context.Context, json.RawMessage) (ToolResult, error)

func (r *Registry) RegisterResult(name, description string, parameters any, fn ResultHandler)
func (r *Registry) RegisterResultFrom(name, description string, parameters any, fn ResultHandler, source ToolSource)
func (r *Registry) GetResultFunc(name string) ResultHandler
func (r *Registry) ExecuteResult(ctx context.Context, name, args string) (ToolResult, error)
```

`registeredTool.fn` 改为 `ResultHandler`。旧 `Register/RegisterFrom` 把 `ToolFunc` 包装成只有 Text 的 ResultHandler；旧 `GetFunc/Execute` 适配回 Text，并保持现有 error suffix 行为。Raw handler 只留在 registry 内部，`GetResultFunc/ExecuteResult` 都返回/执行 validator-wrapped 路径。Metadata 从不往返字符串。只有 `SourceBuiltin + rag_search + ragResources` 能通过 producer-specific validator；MCP/plugin、其它 tool name、未知 key、malformed/超数量/超字节的 `json.RawMessage` 一律剥离并记录受限 warning。provider/compaction 和 tool message 持久化只读 Text；Task 17 的 metadata 仅在当前 turn 内存中流向 Task 18，只有 Task 18 聚合后的最终 assistant metadata 才进入 assistant SSE/history。

SDK bridge 使用唯一实现，不留二选一：`toolAdapter` 按 name 调 `Registry.ExecuteResult`（或 validator-wrapped `GetResultFunc`），绝不持有 raw handler；Text 写 SDK Content，校验后的 Metadata 包进私有 `sdkBridgeData` 并写 SDK 现有 `ToolResult.Data`。executor response 天然带 tool-call ID；`executeToolsConcurrently` type-assert 每个 response 的 Data 并复制到对应 `toolCallResult.Metadata`。Data 不进入模型/SSE/history；禁止全局 side-channel map、直接执行分支和字符串编码。错误 response 丢弃 metadata。

- [ ] **Step 4: 回归**

```bash
go fmt ./internal/agent/...
go test -race ./internal/agent/tools ./internal/agent -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat(agent): support safe tool result metadata"
```

---

### Task 18: rag_search 资源聚合到最终 assistant metadata

**Files:**

- Modify: `internal/rag/agent_adapter.go`
- Modify: `internal/rag/service_test.go`
- Modify: `internal/agent/tools/rag_search.go`
- Modify: `internal/agent/tools/rag_search_test.go`
- Modify: `internal/agent/loop.go`
- Modify: `internal/agent/events.go`
- Modify: `internal/agent/manager_rag_test.go`
- Create: `internal/setup/handlers_agent_rag_assets.go`
- Create: `internal/setup/handlers_agent_rag_assets_test.go`
- Modify: `internal/setup/server.go`
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/components/chat-screen.tsx`
- Modify: `web/src/components/rag-resource-gallery-state.ts`
- Modify: `web/src/components/rag-resource-gallery.test.mjs`
- Reuse: `web/src/components/rag-resource-gallery.tsx`

- [ ] **Step 1: 写端到端失败测试**

断言：

- rag_search 给模型的文字没有 asset ID/URL；
- typed tool result Metadata 含稳定 `[]RAGResourceRef`（asset + KB/doc/chunk/location 来源）；
- 同 turn 多次 rag_search 按首次最终 hit 顺序去重；
- decorative/重复资源过滤，总数最多 6；
- 最终 assistant Metadata 含聚合资源；
- 下一轮发给 provider 的历史忽略 Metadata；
- SSE 实时与 history reload 相同；
- regular/streaming、iteration-cap delivery/fallback、metadata-only event、steer continuation 所有最终 assistant 分支都在 `sess.Append` 前附着 metadata；
- 普通 Agent 的用户上传 ContentParts 保持不变，但任何 RAGResourceRef 都不追加 ContentPart/image_url/base64；
- 无图片/旧历史行为不变；
- Web assistant answer 下 gallery 可见，tool 卡片是否折叠不影响；
- 非 Web channel 忽略 metadata；
- 公共 Agent visitor 对“自己可查看的 session 且 assistant metadata 已引用”的 asset 200；其它 session/未引用 asset/只猜 ID/cross-user 均拒绝；
- asset object 仍存在但 doc/KB/user 已 DELETING，或 session/承载引用的 assistant message 已删除/撤权时立即 404；If-None-Match 也不能先返回 304；
- KB chat 使用 owner asset URL，Agent chat 使用 session-scoped URL，历史 metadata 本身不保存 URL。
- rag_search 文本中的伪 system 指令/副作用工具诱导/伪 metadata 只作为 untrusted tool data；它不能构造 Metadata，也不能成为跳过既有工具权限/确认的授权信号。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag ./internal/agent/tools ./internal/agent ./internal/setup -run 'Test.*RAG.*(Resource|Metadata|Asset)' -v
cd web
npm test
```

- [ ] **Step 3: 实现**

只从本轮全部成功 `rag_search` typed result 聚合，按调用顺序和每次最终 hit 顺序去重。assistant metadata 使用 `RAGResourceRef`，知识库问答也通过同一 helper 从 hits 派生该 DTO。不要把资源注入 assistant Content/ContentParts。

新增：

```text
GET /api/agents/{agentId}/chat/{sessionId}/rag-assets/{assetId}
GET /api/agents/{agentId}/chat/{sessionId}/rag-assets/{assetId}/thumbnail
```

handler 验证请求者可查看未删除的 agent/session、session 属于该 agent、未删除 assistant message 的受信持久化 metadata 精确引用 asset，并再次检查 asset/doc/KB/user 均非 DELETING；不能仅按 asset ID 或 agent 当前 KB 集合授权。所有鉴权先于 ETag/304/object read。

- [ ] **Step 4: 全回归**

```bash
go fmt ./internal/rag/... ./internal/agent/... ./internal/setup
go test ./internal/rag/... ./internal/agent/... ./internal/setup/... -v
cd web
npm run lint
npm test
npm run build
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag internal/agent internal/setup web/src
git commit -m "feat(rag): surface retrieved images in web agent answers"
```

---

## Phase F：生命周期、部署、可观测性和完整验收

### Task 19: 删除/reindex/用户清理与历史资源生命周期

**Files:**

- Modify: `internal/rag/pipeline.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/rag/service_test.go`
- Modify: `internal/store/rag.go`
- Modify: `internal/rag/vector/vector.go`
- Modify: `internal/rag/vector/milvus.go`
- Modify: `internal/rag/vector/fake.go`
- Modify: `internal/rag/vector/chunks.go`
- Modify: `internal/rag/vector/milvus_test.go`
- Modify: `internal/rag/vector/fake_test.go`
- Modify: `internal/rag/vector/chunks_test.go`
- Create: `internal/rag/user_cleaner.go`
- Create: `internal/rag/user_cleaner_test.go`
- Modify: `internal/users/account.go`
- Create: `internal/users/account_test.go`
- Modify: `internal/setup/server.go`
- Modify: `internal/setup/handlers_admin.go`
- Create: `internal/setup/handlers_admin_rag_delete_test.go`

- [ ] **Step 1: 写生命周期失败测试**

覆盖：

- reindex 相同图片复用 ID/对象；
- 图片从新版本消失后旧 chat snapshot 仍能取；
- 每次 active N→M 都为 retired N 创建 `(doc_id,retired_version)` GC task；每个退休版本从自己的 retired_at 保留完整 grace，不因连续激活而提前删除；hydrate active 改变时最多重试一次；
- 连续 1→2→3 后先执行 version 1 GC，只精确删除 `doc_version=1`，仍在自己 grace 内的 version 2 向量/chunk 完整保留；禁止调用范围型 DeleteOldVersions；
- 过期 worker 在一次 GC 后迟到写入 GCED version 的 orphan 能被后续周期 sweep 再次精确删除；GCED tombstone 保留到文档删除；
- 从未被 active、RAG chat sources 或 Agent assistant ragResources 引用的 staging asset/artifact/cache 按 TTL 和每文档 fingerprint 配额删除；
- 文档先事务性标记 DELETING 并立刻从搜索/claim/owner+session asset auth 排除；对象尚未删时两类 endpoint 也立即 404，历史文字仍可读；
- KB/user DELETING 立即撤销其全部搜索、任务和资源授权，再异步全清；
- `Accounts.Delete` 注入窄 `RAGUserCleaner`：用户先 DELETING，清 KB/Milvus/object/catalog 全成功后才 `store.DeleteUser`；有 RAG 数据但 cleaner 缺失时拒绝删除；
- cleanup 中途失败可幂等重试；
- 删除与运行中 task 的 lease/version 竞争安全；
- 文档/KB 删除或 reindex 不直接重置当期 DocumentAI task/user aggregate；未发送 RESERVED 只按状态机 RELEASE，SENT/COMMITTED/OVERRUN 不退还。usage 按 period+audit TTL 保留，用户最终删除按隐私策略处理但不能在用户仍有效时清零配额。

- [ ] **Step 2: 确认失败**

```bash
go test ./internal/rag ./internal/store ./internal/users ./internal/setup -run 'Test.*RAG.*(ReindexAsset|Delete|Cleanup|User|GC)' -v
```

- [ ] **Step 3: 实现显式 cleanup orchestration**

不使用数据库 cascade 假装完成外部资源清理。第一事务先写 doc/KB/user tombstone、supersede/cancel runnable task；search active-map、claim 和两类 asset handler 都 fail closed。`internal/users` 只依赖窄接口，不反向 import `rag`；`internal/rag/user_cleaner.go` 实现接口，`internal/setup/server.go` 在装配时注入。外部清理失败保留 DELETING 状态和可重试任务，不删除用户 SQL 行。`ActivateAndFinish` 在同一事务中只创建延迟 cleanup task，不同步删旧版。

实现独立 `Claim/Heartbeat/FinishRAGIndexGCTask`：只 CAS GC row 的 claim_generation/lease，绝不修改 document.version。vector store 新增 `DeleteDocVersion(ctx,kbID,docID,version int64)`；Milvus 使用 `doc_id == X && doc_version == N`，fake 做相同精确语义，范围型 `DeleteOldVersions` 从 delayed-GC 调用点移除。SQL chunk/mapping 同样按精确版本删除，随后把 version 标为 GCED。

周期 orphan sweep 使用 `stagingArtifactTTL`：删除 asset/artifact/cache 前重新检查 active、RAG chat sources 与 Agent ragResources 引用；对 FAILED/SUPERSEDED/GCED version 的向量/chunk exact delete 则只检查它不是当前 active，不受 chat asset snapshot 阻塞。v1 的 GCED version tombstone 直到文档删除才移除，以便清理任意晚到的外部 upsert。vector/SQL 边界的 DocVersion 全部改为 `int64`，包括 `chunks.go` hydrate/转换路径。

- [ ] **Step 4: 测试**

```bash
go fmt ./internal/rag/... ./internal/store ./internal/users ./internal/setup
go test -race ./internal/rag ./internal/store ./internal/users ./internal/setup -run 'Test.*(RAG|Delete|Cleanup|GC)' -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/rag internal/store internal/users internal/setup
git commit -m "fix(rag): preserve assets across reindex and clean them on deletion"
```

---

### Task 20: sidecar 部署、健康检查和运行时限制

**Files:**

- Modify: `deploy/docker/docker-compose.rag.yml`
- Modify: `deploy/docker/.env.example`
- Modify: `deploy/docker/README.md`
- Modify: `deploy/k8s/bkcrab.yaml`
- Create: `deploy/k8s/rag-parser.yaml`
- Create: `deploy/k8s/rag-parser-networkpolicy.yaml`
- Modify: `deploy/helm/bkcrab/values.yaml`
- Create: `deploy/helm/bkcrab/templates/rag-parser.yaml`
- Create: `deploy/helm/bkcrab/templates/rag-parser-networkpolicy.yaml`
- Modify: `deploy/helm/bkcrab/templates/configmap.yaml`
- Modify: `deploy/helm/bkcrab/templates/secrets.yaml`
- Create: `internal/setup/rag_deployment_test.go`

- [ ] **Step 1: 写/扩展部署静态检查**

验证：

- Compose 有 rag-parser health dependency；
- bkcrab 只拿 parser endpoint，parser 不拿 VLM/API/object-store secret；
- parser `maxInputBytes/maxOutputBytes` 由同一 RAG limits 配置注入并在 health 回显；capabilities 对 sidecar 格式取两端较小值；
- parser 非 root、read-only、tmpfs size、CPU/memory、no-new-privileges；
- K8s/Helm readiness/liveness/resource/securityContext；
- Compose parser 网络 `internal:true` 且 bkcrab 双网连接；K8s/Helm 对 parser 设置 default-deny ingress/egress，parser ingress 只允许 bkcrab pod 访问 parser 端口，parser 无 egress；若命名空间限制 bkcrab egress，再单独允许 bkcrab → parser 端口；
- DocumentAI secret 只进入 bkcrab；
- parser image/version 固定，不用 latest。

- [ ] **Step 2: Compose config 检查**

```bash
docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml config
helm lint deploy/helm/bkcrab
```

- [ ] **Step 3: 实现并 smoke**

```bash
docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml up -d rag-parser
docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml ps
```

确认 healthz、无 secret、限制生效。不要在文档/日志中写真实 key。

- [ ] **Step 4: Commit**

```bash
git add deploy internal/setup/rag_deployment_test.go
git commit -m "build(rag): deploy constrained document parser sidecar"
```

---

### Task 21: 可观测性、golden corpus、E2E 与全量验证

**Files:**

- Modify: `internal/rag/pipeline_test.go`
- Modify: `internal/rag/search_test.go`
- Modify: `internal/rag/service_test.go`
- Modify: `internal/rag/vector/milvus_test.go`
- Create: `internal/rag/testdata/multimodal/README.md`
- Create: `internal/rag/testdata/multimodal/generate.go`
- Create: `internal/setup/rag_e2e_test.go`
- Create: `services/rag-parser/tests/fixtures/generate_multimodal.py`
- Create: `services/rag-parser/tests/fixtures/expected_multimodal.json`
- Modify: `docs/superpowers/specs/2026-07-20-multimodal-document-rag-design.md` only for confirmed implementation deviations
- Modify: `docs/database.md`
- Create: `docs/rag-document-ai.md`

- [ ] **Step 1: 建合法、可再生成的 golden corpus**

测试文档由仓库脚本或小型固定 fixture 生成，不能放入敏感/版权不明材料。每个 fixture 记录预期：

- units/locations；
- Markdown table/code；
- asset/occurrence 数；
- caption/OCR fake response；
- chunk boundaries；
- warnings/degraded。

另建 adversarial corpus：OOXML DTD/entity/external relationships/local paths，四种来源的 raw HTML/dangerous scheme，恶意页面/OCR/caption/table/code 中的伪 system/tool/metadata 指令，超深/超大 DocumentAI JSON。断言无本地文件回传、无隐式网络/XSS、无 RAG image ContentPart、无 metadata/权限伪造；模型 prompt 防护不替代确定性授权测试。

- [ ] **Step 2: 增加日志/指标断言**

记录：

- parser/cache/native/VLM page counts；
- VLM/enrichment call count/duration/usage；
- durable task/user-period budget reserve/sent/commit、cache hit、repair/retry 与 quota/rate-limit 拒绝；
- assets/decorative/warnings；
- task claim/lease/retry；
- active version switch；
- 不记录文档正文、图片 bytes、base64、object key 和 secret。

- [ ] **Step 3: 运行全量本地测试**

```bash
go test -race ./internal/rag/... ./internal/store/... ./internal/setup/... ./internal/agent/...
go test ./...
go vet ./...
go build ./...

cd services/rag-parser
uv run ruff check .
uv run pytest -q
cd ../..

cd web
npm run lint
npm test
npm run build
cd ..

git diff --check
```

- [ ] **Step 4: 运行门控真实集成测试**

仅在明确配置测试凭据/服务时：

```bash
RAG_PARSER_INTEGRATION=1 go test ./internal/rag/parse/... -run Integration -v
go test ./internal/rag/vector/... -run 'TestMilvusRoundTrip|TestMilvusActiveVersionFilter' -v
RAG_VISION_INTEGRATION=1 go test ./internal/rag/vision/... -run Integration -v
go test ./internal/store -run 'TestDocumentAIBudget.*(Postgres|MySQL)' -v
```

Milvus 行运行前显式设置现有门控 `RAG_TEST_MILVUS_ADDR`，需要认证时再设置现有 user/password 环境变量；不引入新的 `RAG_MILVUS_INTEGRATION` 开关。budget integration 行要求预先设置新 `BKCRAB_TEST_POSTGRES_DSN` 与现有 `BKCRAB_TEST_MYSQL_DSN`；测试日志必须遮蔽 DSN，缺任一方言不能勾选三方言发布闸门。

真实 VLM fixture 限制为少量非敏感页面，并设置费用上限。

- [ ] **Step 5: 手工 Web smoke**

1. 创建 standard KB，上传 MD/TXT，确认行为不回退；
2. 创建 auto KB，上传文字+图片 PDF；
3. 文档状态显示 vision x/y，完成后 degraded/warnings 合理；
4. 检索图片说明，hit 命中且显示缩略图；
5. 知识库问答只生成文字回答，答案下展示相关图片；
6. 浏览器网络中回答模型请求没有图片；asset 请求是独立同源 GET；
7. 刷新历史，图片仍显示；
8. Web Agent 使用 rag_search，最终回答下显示同一资源；
9. 上传 DOCX/PPTX/XLSX，验证标题、表格、图片位置；
10. 关闭 VLM/sidecar 模拟故障，验证 PDF 降级、Office 明确失败；
11. 跨用户访问 asset 被拒绝；
12. reindex 后历史图片仍显示，删除文档后显示资源已删除。

- [ ] **Step 6: 最终 Commit**

```bash
git add internal services web deploy docs
git commit -m "test(rag): cover multimodal parsing retrieval and resource display"
```

---

## 发布闸门

以下公共条件全部满足才开放相应 advanced 能力；`pdfAuto` 与 `officeVision` 按各自格式闸门独立发布：

- [ ] claim_generation/doc_version fencing、active filter 每条 ANN route、延迟 GC 测试通过；
- [ ] 存量 task 迁移按维护窗口 runbook 执行：所有旧 worker 已停止且 heartbeat 静止后才 backfill/contract，恢复时只有新 worker；
- [ ] canonical manifest/health 的 Python/Go shared golden、动态 maxInputBytes 和严格 tar decoder 通过；
- [ ] 全来源 Markdown sanitizer、Office DTD/entity/external relationship 恶意 corpus 与前端 raw-HTML 二次防线通过；
- [ ] 专用 KB answer 无 ContentParts；Agent 测试证明 RAG 不新增图片且用户主动上传图片仍保留；
- [ ] owner 与 agent-session 两种 asset 鉴权、DELETING/消息撤权即时失效、CORP/nosniff/安全 raster 通过；
- [ ] 前端不再硬编码扩展名/大小/预算，`.markdown` 和 per-extension limit 生效；
- [ ] sidecar 无 VLM/object-store secret，Compose internal network 与 K8s default-deny egress 生效；
- [ ] reindex 历史 asset 稳定；
- [ ] 文档/KB/用户 durable delete、exact-version GC/tombstone 和 orphan staging/cache sweep 通过；
- [ ] 共享 task budget、用户周期 ledger、repair/retry 计费与 enqueue/reindex rate limit 通过；
- [ ] prompt-injection adversarial corpus 不能绕过确定性权限/metadata/tool confirmation；
- [ ] 全量 Go/Python/Web 测试通过；
- [ ] 成本指标可以看到每文档 VLM 页数、cache hit 和 durable quota usage。

`pdfAuto` 额外要求：PDF engine ADR 已批准并满足许可证义务、真实 PDF golden 路由通过、单页失败不重跑已缓存页面。`officeVision` 额外要求：Office 三格式定位 golden 与 typed image-description/cache 通过；它不等待 PDF ADR。`enrichment` 额外要求：独立系统 gate + KB opt-in/披露、共享预算与 table/code adversarial 测试通过。

## Spec Coverage / 自审

| Spec 主题 | Plan Task |
|---|---|
| parseMode/capabilities/config | 1 |
| SQL version snapshot/chunk catalog/assets | 2 |
| durable DocumentAI task/user budget、usage 状态机与三方言并发 | 2、8、11、12、16、19、21 |
| task fencing/lease/active version | 3 |
| ParsedDocument/Asset/Artifact | 4 |
| MarkItDown/PDF sidecar 与安全协议 | 5A–6 |
| 全来源 Markdown sanitizer 与 MD/TXT 图片禁用 | 7 |
| VLM schema/cache | 8 |
| PDF auto/fallback/original crop | 9 |
| Markdown AST/table/code/image 分块 | 10 |
| 表格/代码增强 | 11 |
| parse→index 原子编排 | 12 |
| reranker/SearchContent/AssetRef | 13 |
| text-only answer + asset API/history | 14 |
| KB UI/progress/gallery | 15 |
| Office docx/pptx/xlsx | 16 |
| typed ToolResult + Web Agent session resources | 17–18 |
| reindex/delete/user cleanup | 19 |
| deployment/security limits | 20 |
| golden/e2e/observability/full regression | 21 |

自审不变量：

- 图片只在 parser/VLM 阶段作为视觉输入；
- embedding、BM25、reranker 和 RAG 注入的回答上下文只使用文字；普通 Agent 的用户原始图片能力不受影响；
- 原图由稳定 RAGResourceRef 在 UI 展示；
- parser 输出不是裸 Markdown string，而是 Markdown units + asset/occurrence/warnings；
- splitter 无网络调用；
- 表格/代码原文永不被增强结果替换；
- 视觉/图片/repair/增强共用同一 fenced durable budget，重试、reclaim、reindex 和资源删除都不能绕过用户周期额度；
- 未激活/过期 worker 的向量由 Milvus active filter 与 doc_version fencing 隔离；新索引失败不影响旧 active version；
- 旧数据只有 pinned active version 可读，但只有 reindex 后才获得图片资源；
- 没有旧 Office 格式支持；
- 许可证/Office 定位等外部决策都有明确阻断闸门、候选结果和产物文件；实现中的版本差异必须回写 spec 并说明原因。
