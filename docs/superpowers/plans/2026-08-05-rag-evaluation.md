# RAG 测评、参数实验与渐进式策略升级实施计划

**Goal:** 在不复制生产 RAG 逻辑、不自动重建存量知识库的前提下，交付超级管理员专用的 RAG 测评闭环：版本化 corpus/cases、真实双 pipeline 实验、独立 Ragas sidecar、基线/候选诊断、受控参数 promotion、在线策略热切换，以及由 KB 所有者主动触发的整库 generation 同步。

**Architecture:** Go 主服务拥有测评编排、真实索引/检索/回答执行、durable task、预算、数据与策略发布；Python `rag-evaluator` sidecar 仅运行固定 Ragas metric bundle。评测索引和 KB 策略升级都使用独立 physical collection generation。KB 查询只解析一个 active generation，升级期间继续读旧 generation，新 generation 全量完成后 CAS 原子切换。在线参数由 immutable `RAGRuntimePolicy` 的 atomic snapshot 提供。

**Tech Stack:** Go；现有 SQL 三方方言、对象存储、Milvus、RAG parser/DocumentAI/provider；Python 3.12 + FastAPI/Uvicorn + pinned Ragas；React/Next.js 16；Docker Compose RAG overlay。

**Spec:** [`docs/superpowers/specs/2026-08-05-rag-evaluation-design.md`](../specs/2026-08-05-rag-evaluation-design.md)

**范围:** 一期覆盖知识库索引、检索和专用知识库回答，不覆盖 Agent 工具调用/目标完成率测评。导入 transport 的最终格式、默认 judge、质量门禁值和 retention 数值在对应任务开始前确认；底层 canonical model、adapter、任务和版本契约不依赖这些选择。

**基线要求:**

- 每个任务开始和完成后保持现有 RAG 单测通过；
- 测评 feature flag 默认关闭；
- 不在默认测试访问外部模型、Milvus 或互联网；
- 不复用普通聊天记录和普通用户 usage ledger；
- 不在 generation resolver 完成双读/迁移前改变现有 KB 查询路径；
- 本机 Go 验证使用 `D:\gotmp`，不得把 Go 临时构建目录指向工作区。

本机 PowerShell 公共前置：

```powershell
$env:GOTMPDIR = 'D:\gotmp'
```

---

## 文件结构总览

```text
internal/config/
  config.go
  env.go
  rag_eval_test.go

internal/store/
  store.go
  database.go
  database_mysql.go
  rag_eval.go
  rag_eval_test.go
  rag_eval_migration_test.go
  rag_policy.go
  rag_policy_test.go
  rag_generation_test.go

internal/rag/eval/
  model.go
  model_test.go
  validate.go
  validate_test.go
  importer.go
  fingerprint.go
  fingerprint_test.go
  deterministic_metrics.go
  deterministic_metrics_test.go
  aggregate.go
  aggregate_test.go
  ragas_client.go
  ragas_client_test.go
  runner.go
  runner_test.go

internal/rag/
  search.go
  search_test.go
  answer.go
  answer_test.go
  policy.go
  policy_test.go
  generation.go
  generation_test.go
  pipeline.go
  vector/vector.go
  vector/fake.go
  vector/milvus.go

internal/setup/
  handlers_rag_eval.go
  handlers_rag_eval_test.go
  handlers_rag_policy.go
  handlers_rag_policy_test.go
  handlers_rag.go
  server.go

services/rag-evaluator/
  .dockerignore
  Dockerfile
  pyproject.toml
  uv.lock
  app/__init__.py
  app/main.py
  app/protocol.py
  app/metrics.py
  app/settings.py
  tests/test_health.py
  tests/test_protocol.py
  tests/test_metrics.py

web/src/lib/api.ts
web/src/components/app-sidebar.tsx
web/src/app/overview/page.tsx
web/src/app/admin/rag-evals/page.tsx
web/src/app/admin/rag-evals/components/*
web/src/app/knowledge/page.tsx
web/src/components/rag-policy-sync-dialog.tsx
web/src/components/rag-eval-*.test.mjs

deploy/docker/docker-compose.rag.yml
deploy/docker/.env.example
Dockerfile
docs/rag-evaluation.md
```

---

## Phase A：先固定契约、配置与持久化

### Task 1：配置、feature gate、closed schema 与 capabilities

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/env.go`
- Create: `internal/config/rag_eval_test.go`
- Modify: `deploy/docker/.env.example`

**Deliverables:**

- `RAGEvaluationCfg`：enabled、sidecar endpoint/timeout、batch limits、worker concurrency、run budgets、retention；
- evaluator LLM/embeddings 的部署级引用或明确占位，secret 日志屏蔽；
- `RAGRuntimePolicyData`、`RAGIngestionPolicyData`、`RAGEvalProfileData` 的 closed JSON schema/Go types；
- ApplyDefaults/Validate，未知 enum、非法 topN/minScore/chunk/dims 拒绝；
- `/api/admin/rag-evals/capabilities` 所需 snapshot DTO，不在 handler 同步探活 sidecar。

**Steps:**

- [x] 先写默认关闭、合法边界、未知枚举、secret `LogValue` 的失败测试。
- [x] 给所有新增限制设置保守默认值，但 `enabled=false`。
- [x] 区分 answer model 与 evaluator model；禁止把 evaluator secret marshal 到 API DTO。
- [x] 环境变量使用 `BKCRAB_RAG_EVAL_*` 前缀；兼容 Docker 注入，不覆盖普通 RAG 配置。
- [x] 为 policy/profile 写稳定 canonical JSON/fingerprint helper 的输入类型；真正 hash 在后续 eval 包实现。
- [x] 运行 `go test ./internal/config`。

**Gate:** 未启用时不创建 evaluator client/worker，不新增外部调用，不改变 `/api/rag/*` 行为。

### Task 2：测评 Dataset/Run/Result/Usage 数据库

**Files:**

- Create: `internal/store/rag_eval.go`
- Create: `internal/store/rag_eval_test.go`
- Create: `internal/store/rag_eval_migration_test.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`

**Deliverables:**

- spec 第 12.1/12.2 节的 dataset、version、corpus document、case、profile、run、case result、metric result、aggregate、usage 表；
- SQLite/MySQL/Postgres 等价 schema；
- immutable READY dataset version/profile；
- cursor pagination 和 bounded list；
- durable eval run claim/lease/fence/cancel；
- case/metric 幂等写入和清洗错误。

**Steps:**

- [x] 先写三方言建库/重复迁移测试和 SQLite CRUD/约束测试。
- [x] 用 closed status 常量与 scan helpers，不从任意字符串更新状态。
- [x] Dataset Version 从 DRAFT → VALIDATING → READY/FAILED 使用 CAS；READY 后 update 失败。
- [x] Run claim 必须返回 fence token；heartbeat 失败后旧 worker 写 case/result/finish 均返回 false。
- [x] `rag_eval_usage.idempotency_key` 唯一，重试不能重复计费。
- [x] 删除先 tombstone，不在请求事务中同步删除对象/collection。
- [x] 运行 `go test ./internal/store -run 'RAGEval|Eval'`。

**Gate:** migration 不回写或锁住现有大 RAG 表；feature flag 关闭时只是增加空表。

### Task 3：策略、KB generation、同步任务与审计数据库

**Files:**

- Create: `internal/store/rag_policy.go`
- Create: `internal/store/rag_policy_test.go`
- Create: `internal/store/rag_generation_test.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Modify: `internal/store/rag.go`

**Deliverables:**

- ingestion/runtime policy revisions；
- active policy pointers；
- KB generation、generation-document mapping、sync task、audit log；
- `rag_kbs.pinned_policy_version`、`active_generation_id`（先 nullable）；
- 每 KB 一个 ACTIVE generation 的事务/CAS 保证；
- sync task claim/lease/fence/cancel/rollback window。

**Steps:**

- [x] 先写 policy immutable、active pointer CAS、两个 ACTIVE 被拒、旧 fence 不能切换的测试。
- [x] 不依赖仅 PostgreSQL 支持的 partial unique index；对三方言使用事务、状态 CAS 和可移植辅助约束。
- [x] `ActivateRAGKBGeneration` 同一事务完成 target ACTIVE、old RETIRED、KB pointer/pinned policy 更新和 audit append。
- [x] generation mapping 是策略升级 Search 的权威 active version 集合。
- [x] sync 失败/取消不得修改 KB pointer；rollback 只能指向仍在 rollback window 且完整的 RETIRED generation。
- [x] 为删除 KB/用户增加 generation/sync/audit 的级联或显式清理顺序。
- [x] 运行 `go test ./internal/store -run 'RAGPolicy|RAGGeneration|RAGSync'`。

**Gate:** 此任务只建立存储能力；Search 仍走旧路径。

### Task 4：Canonical Dataset、校验器与 importer 边界

**Files:**

- Create: `internal/rag/eval/model.go`
- Create: `internal/rag/eval/model_test.go`
- Create: `internal/rag/eval/validate.go`
- Create: `internal/rag/eval/validate_test.go`
- Create: `internal/rag/eval/importer.go`
- Create: `internal/rag/eval/fingerprint.go`
- Create: `internal/rag/eval/fingerprint_test.go`

**Deliverables:**

- canonical corpus/case/profile structs；
- 字段长度、JSON 深度、ID 唯一、reference ID、metric coverage 校验；
- importer interface 与 registry；
- dataset/profile/ingestion fingerprint；
- validation report（errors/warnings/coverage）。

**Steps:**

- [x] 先写空 corpus/case、重复 ID、悬空 reference ID、过大 metadata、非法 UTF-8、缺 metric input 测试。
- [x] 明确“missing metric input → coverage warning/skip”，不要自动补假 reference。
- [x] canonical hash 使用稳定字段排序、规范化 JSON 和原件 sha256；对象键、createdAt 不影响 corpus fingerprint。
- [x] secret/token/endpoint credential 永不进入 fingerprint 或 validation report。
- [x] 只实现内存 canonical importer 测试桩；外部文件格式在决策后以独立 adapter 增加。
- [x] 运行 `go test ./internal/rag/eval -run 'Model|Valid|Fingerprint'`。

**Decision gate:** 在实现首个真实 import adapter 前确认 JSONL/JSON、CSV/XLSX、ZIP/目录和手工录入的首期组合。

---

## Phase B：把生产 pipeline 抽成可注入、可追踪但行为不变的边界

### Task 5：SearchOptions、SearchTrace 与生产兼容包装

**Files:**

- Modify: `internal/rag/search.go`
- Modify: `internal/rag/search_test.go`
- Modify: `internal/rag/query_planner.go`
- Modify: `internal/rag/agent_adapter.go`
- Modify: `internal/setup/handlers_rag.go`

**Deliverables:**

- `SearchWithOptions(ctx, owner, targets, SearchContext, SearchOptions) (hits, trace, error)`；
- 现有 `Search`/`SearchWithContext` 作为兼容包装，输出完全不变；
- options 显式控制 topN、candidateTopK、minScore、Rewrite、HyDE、reranker failure policy；
- trace 保存各阶段计数/分数/降级/耗时，不记录 secret 和无界原文日志。

**Steps:**

- [x] 先把现有 search tests 变为行为黄金测试，记录默认路径的 hits/order/score/fallback。
- [x] 新增 options 校验，禁止 candidateTopK < topN、非法 minScore、未知 failure policy。
- [x] Query planner 返回 rewrite/HyDE route metadata；关闭 Rewrite/HyDE 时走清晰、可测试的原 query 路径。
- [x] reranker 成功/失败、minScore 过滤、RRF fallback 都写 trace；`minScore` 仍只作用于 rerank score。
- [x] 保持 Agent adapter 与现有 handler 调用兼容，不在此任务改变产品默认。
- [x] 运行 `go test ./internal/rag/... ./internal/setup -run 'RAGSearch|QueryPlan|Rerank'`。

**Gate:** 对相同现有配置，兼容 wrapper 的输出和日志安全边界不得改变。

### Task 6：共享知识库 Answer 服务与 AnswerTrace

**Files:**

- Create: `internal/rag/answer.go`
- Create: `internal/rag/answer_test.go`
- Modify: `internal/setup/handlers_rag.go`
- Modify: `internal/setup/handlers_rag_assets_test.go`

**Deliverables:**

- 从 `handleRAGChat` 抽出共享 answer prompt/builder/call；
- `AnswerOptions`：model、temperature、maxTokens、prompt bundle version；
- `AnswerTrace`：response、citations、usage、latency、model/prompt version；
- production mode 保存 chat/usage，evaluation mode 不保存 chat 且返回独立 usage。

**Steps:**

- [x] 先固定当前 system prompt、user prompt、temperature=0.2、maxTokens 行为测试。
- [x] 把 prompt 变成 versioned bundle registry；`rag-answer-v1` 必须逐字等价当前 prompt。
- [x] evaluator 调用只能选择编译/部署允许的 bundle，不能上传任意 system prompt 代码；若产品需要可编辑 prompt，另设审核后的 immutable content revision。
- [x] evaluation path 不写 `rag_chat_turns`，不调用普通 usage meter；用返回 usage 交给 eval runner。
- [x] 保持历史问题规范化、引用编号和不可信 context 包裹不变。
- [x] 运行 `go test ./internal/rag ./internal/setup -run 'RAGAnswer|RAGChat|Prompt'`。

### Task 7：RuntimePolicy atomic snapshot 与默认行为迁移

**Files:**

- Create: `internal/rag/policy.go`
- Create: `internal/rag/policy_test.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/setup/server.go`
- Modify: `internal/setup/handlers_rag.go`
- Modify: `cmd/*` 或实际 gateway 装配文件

**Deliverables:**

- process-wide `RuntimePolicyProvider`；
- 每请求一次性捕获 immutable snapshot；
- version 1 从当前配置/常量合成，保证行为不变；
- 多 pod DB revision 轮询/通知扩展位；
- service 无需重建即可让新请求使用新的 topN/candidateTopK/minScore/answer options。

**Steps:**

- [x] 先写并发读/原子 swap、请求中途 swap 不改变已捕获 snapshot、非法 revision 拒绝测试。
- [x] 把 `ragChatTopN`、temperature、prompt version 和 service cfg 中的 query-time candidate/minScore 迁移到 snapshot。
- [x] 仍从现有环境/config 生成初始 version 1；feature flag 关闭时不要求新 policy 表已有数据。
- [x] snapshot 只复制 bounded value，不暴露可并发修改 map/slice。
- [x] 添加 revision version 到 search/answer trace。
- [x] 运行 `go test -race ./internal/rag ./internal/setup -run 'RuntimePolicy|RAGChat|RAGSearch'`。

**Gate:** migration 完成前线上输出与当前常量一致。

---

## Phase C：引入 collection generation，但先不改变生产可见性

### Task 8：VectorStore 使用不透明 collection key

**Files:**

- Modify: `internal/rag/vector/vector.go`
- Modify: `internal/rag/vector/fake.go`
- Modify: `internal/rag/vector/milvus.go`
- Modify: `internal/rag/vector/chunks.go`
- Modify: vector tests
- Modify: `internal/rag/pipeline.go`
- Modify: `internal/rag/search.go`
- Modify: `internal/rag/lifecycle.go`

**Deliverables:**

- 将接口语义从逻辑 `kbID` 改为受信任 resolver 提供的 opaque `collectionKey`；
- legacy resolver 仍返回原 KB ID 对应的当前物理名；
- collection key 只能由服务端生成/验证；
- fake 支持多个 generation 和不同 dims。

**Steps:**

- [x] 先写同一逻辑 KB 两个 collection key、不同 dims、互不污染的 fake tests。
- [x] 改参数名/类型时保持窄接口；禁止 handler/API 接收 collection key。
- [x] Milvus physical name derivation 必须 bounded、合法、无用户原文；generation ID 进入名字。
- [x] legacy adapter 确保现有 collection 无需重建即可读写。
- [x] 所有 delete/drop 调用使用已鉴权、已持久化的 physical key，不能字符串猜测。
- [x] 运行 `go test ./internal/rag/vector ./internal/rag/...`。

### Task 9：Legacy KB generation backfill 与双读校验

**Files:**

- Create: `internal/rag/generation.go`
- Create: `internal/rag/generation_test.go`
- Modify: `internal/store/rag_policy.go`
- Modify: `internal/store/database.go`
- Modify: startup migration/assembly
- Modify: `internal/rag/search.go`

**Deliverables:**

- 从现有 KB + document active versions 合成 legacy IngestionPolicy 和 generation mapping；
- idempotent backfill；
- `GenerationResolver`；
- shadow/double-read validation mode；
- 未迁移 KB 安全回退 legacy resolver。

**Steps:**

- [x] 先写空 KB、部分 active docs、删除中 doc、重复启动和并发 backfill 测试。
- [x] legacy policy fingerprint 由现有 KB/版本快照合成，不假装它等于最新 platform policy。
- [x] backfill 只引用现有 collection，不复制向量。
- [x] 双读模式比较旧 activeVersions 与 generation mapping；不一致报警但仍用旧路径。
- [x] 所有 KB 校验通过后通过 feature gate 切 generation resolver 为权威路径。
- [x] 运行 SQLite 默认测试和环境门控 MySQL/Postgres migration tests。

**Release gate:** 未完成线上数据抽样双读一致性前，不删除 legacy fallback。

---

## Phase D：Ragas sidecar 与评分客户端

### Task 10：`rag-evaluator` sidecar 骨架、协议与固定 metric bundle

**Files:**

- Create: `services/rag-evaluator/pyproject.toml`
- Create: `services/rag-evaluator/uv.lock`
- Create: `services/rag-evaluator/app/settings.py`
- Create: `services/rag-evaluator/app/protocol.py`
- Create: `services/rag-evaluator/app/metrics.py`
- Create: `services/rag-evaluator/app/main.py`
- Create: `services/rag-evaluator/tests/*`

**Deliverables:**

- `/healthz` 返回 service/protocol/Ragas/metric bundle 版本；
- `/v1/evaluate` closed protocol；
- Context Precision/Recall、Faithfulness、Response Relevancy、Factual Correctness adapters；
- metric input requirements 与 per-metric skip/error；
- requestId + body hash idempotency；
- batch/body/context/reason/timeout 限制。

**Steps:**

- [x] 在 `pyproject.toml` 精确 pin 已验证 Ragas minor/patch 和关键 SDK；生成 frozen lock。
- [x] 先用 fake LLM/embeddings 写协议、skip、partial failure、idempotency、limit tests。
- [x] 使用 collections-based API；增加一个 smoke test 防止依赖升级回退 legacy API。
- [x] metric registry 是闭集，不接受模块名、Python 表达式或任意 prompt。
- [x] context 作为不可信数据包裹，加入 prompt injection golden tests。
- [x] reason 截断且不得进入默认 access log；Uvicorn 禁 access log。
- [x] 运行 `uv run pytest` 和 `uv run ruff check .`（目录 `services/rag-evaluator`）。

**Decision gate:** 接真实 judge 前确认 evaluator LLM/embeddings、endpoint、数据外发和价格；默认测试继续 fake。

### Task 11：Sidecar 容器安全与健康缓存

**Files:**

- Create: `services/rag-evaluator/Dockerfile`
- Create: `services/rag-evaluator/.dockerignore`
- Modify: `deploy/docker/docker-compose.rag.yml`
- Modify: `deploy/docker/.env.example`
- Modify: Go evaluator health provider files/tests

**Deliverables:**

- Python 3.12 non-root image；
- read-only、tmpfs、cap drop、pids/cpu/memory、internal network、no host port；
- evaluator secret 通过 env/Docker secret 注入，不出现在 API payload；
- Go TTL health snapshot；capabilities 只读 snapshot，不同步探活。

**Steps:**

- [x] 沿用 `rag-parser` 固定 uv + locked install 范式，不复制其 LibreOffice 系统依赖。
- [x] healthcheck 验证 Ragas/metric 初始化，不发真实 judge 请求。
- [x] compose 只让 `bkcrab` 与 evaluator 进入 `rag-evaluator-internal`。
- [x] 增加部署静态测试：无 ports、read_only、非 root、no-new-privileges、resource limits。
- [x] evaluator 不获取 Milvus/S3/DB/生产 embedding key。
- [x] 构建镜像并在无外网运行 fake health/protocol smoke。

### Task 12：Go Ragas client 与确定性指标

**Files:**

- Create: `internal/rag/eval/ragas_client.go`
- Create: `internal/rag/eval/ragas_client_test.go`
- Create: `internal/rag/eval/deterministic_metrics.go`
- Create: `internal/rag/eval/deterministic_metrics_test.go`

**Deliverables:**

- bounded HTTP client、SSRF-safe configured endpoint、request idempotency；
- response exact decode、protocol/version compatibility、partial results；
- Hit@K/Recall@K/MRR/nDCG；
- citation precision/coverage、abstention accuracy 的明确初版规则；
- metric required-fields registry 在 Go/sidecar 间契约测试。

**Steps:**

- [x] 使用 `httptest` 覆盖 2xx/4xx/5xx、超时、截断、重复/未知 case/metric、NaN/Inf/out-of-range score。
- [x] 非幂等错误不可盲重试；requestId 相同的安全网络失败可受限重试。
- [x] deterministic metrics 使用 stable context IDs；无标注时 skip，不用文本模糊匹配冒充 ground truth。
- [x] 引用 parser 只识别当前 RAG 明确的 `[n]` 契约，越界引用计错并保留 reason。
- [x] metric status 使用 closed enum：ok/skipped_missing_input/error。
- [x] 运行 `go test ./internal/rag/eval -run 'Ragas|Metric|Citation|Abstention'`。

---

## Phase E：真实评测 runner 与结果分析

### Task 13：Dataset service、对象存储与首个 import adapter

**Files:**

- Add service files under `internal/rag/eval/`
- Modify: object store integration as needed
- Create/modify setup handlers later only after service tests

**Deliverables:**

- draft upload → validate → immutable READY version；
- corpus object keys 和 staging/commit/cleanup；
- 首个已确认 import adapter；
- validation report 和 preview pagination；
- delete/tombstone/GC。

**Steps:**

- [x] 决策首期格式后先写 golden files 和拒绝样本。
- [x] 原件流式写入对象存储并计算 sha256；不把整个大文件读入内存。
- [x] manifest READY 前保证 SQL/object 一致；失败 staging 可按 TTL 清理。
- [x] version READY 后不允许覆写 case/document；编辑创建下一 version。
- [x] 数据集删除不能删除仍被 run 引用的 version；先 tombstone，等 retention/refcount。
- [x] 测试路径穿越、重复文件名、压缩炸弹（若支持 ZIP）和超限。

### Task 14：Eval shadow generation builder 与复用

**Files:**

- Extend: `internal/rag/generation.go`
- Extend: `internal/rag/pipeline.go`
- Extend: `internal/rag/eval/fingerprint.go`
- Add tests in `internal/rag/eval/runner_test.go` and generation tests

**Deliverables:**

- corpus version + ingestion profile → isolated eval generation；
- 真实 parser/chunk/embed/vector pipeline 的显式 target；
- fingerprint READY generation 复用；
- refcount/TTL/fenced GC；
- 与生产 KB collection/object namespace 隔离。

**Steps:**

- [x] 先用 fake components 证明同 fingerprint 复用、任一关键参数变化新建、失败不 READY。
- [x] pipeline target 明确 owner/run/dataset/generation，不伪造生产 KB ID 绕过权限。
- [x] corpus artifact 可按安全 fingerprint 复用，但 chunk/vector generation 仍按完整 fingerprint。
- [x] BUILDING 单飞由 SQL claim 决定，多 pod 不重复支付。
- [x] generation refcount 与 run transaction 绑定，runner 失败也最终释放。
- [x] 删除 eval generation 不能调用生产 KB drop 路径。

### Task 15：Durable Evaluation Runner（Full + Online-only）

**Files:**

- Create: `internal/rag/eval/runner.go`
- Create: `internal/rag/eval/runner_test.go`
- Modify: gateway/server assembly
- Modify: usage integration

**Deliverables:**

- durable run worker、阶段/进度/取消/恢复；
- Full Pipeline 和 Online Only；
- 调用共享 Search/Answer；
- batch score；
- case/metric/usage 幂等持久化；
- 预算、并发和 deadline。

**Steps:**

- [ ] 先写状态机、重启重领、lease lost、取消、case partial failure 测试。
- [ ] run 创建时冻结 dataset/profile/metric/execution snapshot，运行中不得读取 draft profile。
- [ ] Full 模式建立/引用 generation；Online-only 必须显式引用 READY generation。
- [ ] 每个 case 先保存 pipeline result，再异步/batch score；重试评分不重复执行昂贵 answer，除非 result 不完整。
- [ ] evaluation answer 不写聊天记录；usage 写 eval ledger。
- [ ] 达到 cost/token/time budget 后停止新 case，保留已完成结果并给出明确状态。
- [ ] 使用独立 worker concurrency，不复用普通 index worker channel。
- [ ] 运行 `go test -race ./internal/rag/eval ./internal/rag/...`。

### Task 16：聚合、切片、比较、阈值曲线与导出 service

**Files:**

- Create: `internal/rag/eval/aggregate.go`
- Create: `internal/rag/eval/aggregate_test.go`
- Extend: store queries
- Add export service files/tests

**Deliverables:**

- mean/median/p50/p95/count/scored/skipped/error；
- tag/metadata bounded slices；
- baseline/candidate paired diff；
- minScore threshold curve；
- CSV/JSON 导出边界（最终格式按决策）；
- confirmation run eligibility。

**Steps:**

- [ ] 先写 skipped/error 不进入 scored 分母、小样本、全 skipped、paired case 缺失测试。
- [ ] baseline compare 只接受同 dataset version；online-only compare 还要求同 generation。
- [ ] 非 paired case 明确列出，不静默丢弃。
- [ ] threshold curve 基于保存的 rerank score + relevance label 计算；没有 label 时不伪造 precision/recall。
- [ ] 切片 key/value 做白名单、长度和基数上限，防止高基数拖垮 DB/UI。
- [ ] 导出重新鉴权并流式输出，大 trace 用受控对象引用。

---

## Phase F：策略 promotion 与 KB 渐进同步

### Task 17：RuntimePolicy promotion、门禁、多 pod 刷新与回滚

**Files:**

- Extend: `internal/rag/policy.go`
- Extend: `internal/store/rag_policy.go`
- Create: `internal/setup/handlers_rag_policy.go`
- Create: `internal/setup/handlers_rag_policy_test.go`
- Modify: `internal/setup/server.go`

**Deliverables:**

- 从 successful confirmation run 提取白名单参数；
- configurable quality/performance/cost gates；
- immutable revision + CAS active pointer + audit；
- 多 pod revision refresh；
- rollback。

**Steps:**

- [ ] 先写普通用户/admin key/actAs 拒绝、未通过门禁、未知参数、并发 publish CAS、rollback 测试。
- [ ] promotion body 只引用 run/profile + 允许发布字段，不接收任意完整配置覆盖。
- [ ] 若候选收益依赖未发布参数，要求 confirmation run 或显式阻断状态。
- [ ] 发布事务先写 revision/audit，再 CAS pointer；失败不得产生半 active 状态。
- [ ] pod 刷新校验完整 revision 后 atomic swap；校验失败继续旧 snapshot 并报警。
- [ ] rollback 创建 audit，恢复旧 revision pointer，不原地编辑。

### Task 18：IngestionPolicy promotion、新 KB pin 与 drift

**Files:**

- Extend: `internal/rag/policy.go`
- Extend: `internal/store/rag_policy.go`
- Modify: `internal/rag/service.go`
- Modify: KB creation/update DTO/handlers
- Extend: policy handler tests

**Deliverables:**

- 从 Full Pipeline confirmation run 发布 IngestionPolicy；
- active default pointer；
- 新 KB 创建时 pin policy + generation；
- 现有 KB 保持原 policy；
- drift/diff/estimate read model。

**Steps:**

- [ ] 先写发布新版后“新 KB 用新版、旧 KB 不变、旧 KB 新上传仍走旧策略”的测试。
- [ ] policy 保存 prompt/model contract version，不保存明文 API key。
- [ ] 新 KB provisioning 与 policy pin/collection dims 在一个可恢复流程中完成。
- [ ] 旧 KB 的普通单文档 reindex 必须继续使用 KB pinned policy，而不是 platform latest。
- [ ] drift 是 `pinned != latest`，不改变 health/status，也不阻塞查询。
- [ ] diff DTO 屏蔽 endpoint/key，Embedding 变化明确标记 full collection rebuild。

### Task 19：整库 Policy Sync、原子 generation 切换与 rollback

**Files:**

- Extend: `internal/rag/generation.go`
- Extend: `internal/rag/pipeline.go`
- Extend: `internal/rag/lifecycle.go`
- Extend: `internal/store/rag_policy.go`
- Extend: `internal/setup/handlers_rag_policy.go`
- Add comprehensive generation/sync tests

**Deliverables:**

- estimate → confirm → durable sync；
- KB mutation maintenance fence；
- 旧 generation 在线读；
- 全文档目标 versions + shadow collection；
- validation → atomic activate；
- cancel/failure cleanup；
- rollback window/GC。

**Steps:**

- [ ] 先写 v2 active/v3 building 期间每次查询只见 v2 的并发测试。
- [ ] 同一 KB 同时只允许一个 sync；同步和 KB delete/单 doc update/upload/delete 互斥。
- [ ] 一期同步时拒绝或暂停文档写操作，返回明确 409/423 业务错误；查询不阻塞。
- [ ] 对每个 corpus doc 创建目标 policy snapshot/version，但在 target generation 完成前不改变可见 mapping。
- [ ] 校验 document membership、chunk count、dims、SQL chunk refs、Milvus entity 抽样和 retrieval smoke。
- [ ] 激活使用 store Task 3 的单事务/CAS；切换后 Search 新请求只见 v3。
- [ ] 失败/取消 drop target collection 并恢复写；旧 generation/policy 不变。
- [ ] rollback 只在 old generation 完整且窗口内；GC 必须检查 active/ref/rollback。
- [ ] 覆盖同步中平台发布 v4：任务仍固定 v3，结束后显示 v4 drift。
- [ ] 运行 `go test -race ./internal/rag/... ./internal/store ./internal/setup -run 'Generation|PolicySync|RAGSearch'`。

**Release gate:** 在真实大 KB shadow rebuild、失败注入、进程重启和回滚演练完成前，不向普通用户显示同步按钮。

---

## Phase G：API 与 Web 纵向闭环

### Task 20：超级管理员 Eval API 与严格 session gate

**Files:**

- Create: `internal/setup/handlers_rag_eval.go`
- Create: `internal/setup/handlers_rag_eval_test.go`
- Modify: `internal/setup/server.go`

**Deliverables:**

- spec 第 13.1 节 dataset/profile/run/result/compare/export/capabilities API；
- `requireSuperAdminSession`；
- cursor pagination、idempotency、body limits；
- masked DTO 和稳定 error codes。

**Steps:**

- [ ] 权限测试覆盖匿名、普通 user、super_admin、actAs read-only、type=admin API key。
- [ ] 一期只有未 actAs 的 super_admin session 通过；后续 automation 不复用本门禁。
- [ ] handler 只调用 eval service，不直接拼 SQL、对象键或 sidecar payload。
- [ ] 上传端点使用 MaxBytesReader/stream；list/case/result 有 bounded filters。
- [ ] capabilities 返回 feature/sidecar/metrics/importers/limits，不返回 evaluator secret。
- [ ] export、trace、validation report 每次请求重新鉴权。

### Task 21：管理员导航、数据集与运行 UI

**Files:**

- Modify: `web/src/components/app-sidebar.tsx`
- Modify: `web/src/app/overview/page.tsx`
- Modify: `web/src/lib/api.ts`
- Create: `web/src/app/admin/rag-evals/page.tsx`
- Create: `web/src/app/admin/rag-evals/components/datasets-*`
- Create: `web/src/app/admin/rag-evals/components/run-*`
- Create tests

**Deliverables:**

- 管理员 sidebar/overview 入口；
- 数据集/版本/校验/preview；
- run wizard：dataset、Full/Online、baseline/candidate、metrics、budget；
- durable progress/cancel；
- 普通用户不展示入口。

**Steps:**

- [ ] 先写导航角色、run form state、validation error mapping 的 node tests。
- [ ] `/admin/rag-evals/` 使用现有 AuthGuard 路径门禁，但不把它当服务端安全边界。
- [ ] capabilities 驱动可用 metrics/importers；sidecar 不健康时解释并禁启动。
- [ ] run 创建前展示文档/case/外部调用/费用粗估和不可复现风险。
- [ ] 进度使用有界轮询、页面隐藏时降频、完成/取消停止。
- [ ] 不把完整 contexts/reasons 一次加载到列表首屏。

### Task 22：结果分析与参数 promotion UI

**Files:**

- Create: admin rag-evals result/profile/policy components
- Modify: `web/src/lib/api.ts`
- Create node tests

**Deliverables:**

- 质量/性能/成本卡片；
- mean/median/distribution/scored/skipped/error；
- baseline/candidate paired diff；
- case drill-down；
- threshold curve；
- promotion eligibility、门禁原因、confirmation run、publish/rollback 审计 UI。

**Steps:**

- [ ] 测试 skipped/error 分母展示，禁止只渲染漂亮平均值。
- [ ] delta 使用方向语义：latency/cost 下降是改善，error/quality 方向不同。
- [ ] case contexts 默认折叠，显示 raw recall/rerank score 和过滤原因。
- [ ] `minScore` 标签使用“最低 reranker 分数”，显示 reranker fallback rate。
- [ ] 完整索引参数可编辑用于实验；发布面只列白名单并标注未发布差异。
- [ ] 所有 publish/rollback 二次确认并要求备注。

### Task 23：KB policy drift、同步确认、进度和回滚 UI

**Files:**

- Modify: `web/src/app/knowledge/page.tsx`
- Create: `web/src/components/rag-policy-sync-dialog.tsx`
- Modify: `web/src/lib/api.ts`
- Create node tests

**Deliverables:**

- latest/outdated/syncing/failed 状态；
- 当前/最新 policy diff；
- estimate；
- 二次确认；
- 同步进度、取消、允许时回滚；
- 同步期间写操作 disabled 及解释。

**Steps:**

- [ ] 测试旧 KB 只出现非阻断提示，新 KB/latest 不出现噪音。
- [ ] 确认框展示 docs/bytes/pages/time/cost/temp storage；超大 KB 要求输入名称。
- [ ] 文案明确“重新索引整个知识库，期间旧索引继续回答”。
- [ ] Embedding 变化单独高亮“将创建新向量 collection”。
- [ ] 同步失败明确“旧索引仍正常”，错误文案不泄漏内部对象键/endpoint。
- [ ] 同步时上传/删除/reindex 控件 disabled，并处理服务端并发 409/423。

---

## Phase H：部署、清理、文档与完整验收

### Task 24：任务清理、retention、成本可观测性与删除闭环

**Files:**

- Extend: eval runner/lifecycle/store
- Modify: metrics/telemetry docs
- Add cleanup tests

**Deliverables:**

- dataset/run/object/generation tombstone + fenced GC；
- refcount/rollback window；
- eval usage/cost aggregates；
- bounded telemetry；
- orphan reconciliation。

**Steps:**

- [ ] 决策 retention 默认值后写 fake-clock tests。
- [ ] 删除 run 不删除被其它 run 复用的 generation；删除 dataset 不破坏保留 run。
- [ ] orphan reconciler 只删除 SQL 明确标记且 fence 有效的 physical key。
- [ ] telemetry 只记录 ID/hash/count/duration/status，不记录 question/context/response。
- [ ] sidecar、judge、parser、embedding、Milvus 每阶段有 latency/error/cost counter。
- [ ] 进程在任意 cleanup 中断后可幂等恢复。

### Task 25：Docker、主镜像、部署说明与离线 smoke

**Files:**

- Modify: `deploy/docker/docker-compose.rag.yml`
- Modify: `deploy/docker/.env.example`
- Modify: root `Dockerfile` as needed
- Create: `docs/rag-evaluation.md`
- Add deployment/static tests

**Deliverables:**

- evaluator image/build wiring；
- feature flag 和 judge 配置说明；
- 数据外发/资源/容量/备份/恢复说明；
- `/home/csb` Docker 部署兼容；
- health/capability smoke。

**Steps:**

- [ ] compose config 验证 evaluator 无 host port、只有 internal network、read_only/non-root/limits。
- [ ] 文档说明 evaluator 会接收 question/response/context，部署者必须选择允许的数据边界。
- [ ] 文档说明 policy publish 与 KB sync 的成本和回滚语义。
- [ ] 构建主镜像和 evaluator 镜像；默认 enabled=false 时 bkcrab 可不依赖 sidecar 启动。
- [ ] enabled=true 且 sidecar unhealthy 时生产 RAG 正常、eval capabilities 不可用。
- [ ] 不在本任务自动部署到服务器；部署需作为单独授权步骤执行。

### Task 26：E2E 黄金集、迁移演练与发布闸门

**Files:**

- Create: eval golden testdata
- Add Go/Python/Web E2E tests
- Update: spec coverage/self-review section if implementation diverges

**Deliverables:**

- 端到端黄金集；
- legacy KB generation migration；
- Full/Online-only baseline-candidate；
- RuntimePolicy promotion/rollback；
- IngestionPolicy publish/new-KB pin/old-KB drift/sync/rollback；
- 故障注入、重启、取消、budget、cleanup。

**Steps:**

- [ ] 使用 fake judge 完成本地确定性 E2E：导入 → Full run → score → compare。
- [ ] 用门控真实环境运行 Ragas + evaluator provider smoke。
- [ ] 用门控真实 Milvus 验证两个 dims 的 generation、原子 pointer 和 old collection rollback。
- [ ] 迁移一份含多文档/历史版本/删除中数据的数据库副本，验证 legacy backfill 幂等。
- [ ] 故障注入：parser、embedding、Milvus、reranker、answer、judge、DB heartbeat、进程 kill。
- [ ] 并发断言：v3 BUILDING 全程查询只见 v2；切换后新请求只见 v3；无混合集合。
- [ ] 运行全量验证：

```powershell
$env:GOTMPDIR = 'D:\gotmp'
go test ./...
pnpm --dir web test
pnpm --dir web lint
pnpm --dir web build
Push-Location services/rag-evaluator
uv run pytest
uv run ruff check .
Pop-Location
docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml config
```

- [ ] 记录真实测试中使用的模型、镜像 digest、Ragas 版本和数据集 hash。
- [ ] 完成安全、隐私、成本、回滚和孤儿资源自审。

---

## 发布顺序

1. **Foundation only:** 表、closed contracts、pipeline options/trace、policy snapshot；feature 关闭。
2. **Generation compatibility:** legacy backfill + 双读校验；不开放策略同步。
3. **Internal eval:** evaluator + canonical dataset + runner + API，只给 super_admin，promotion 关闭。
4. **Read-only analysis:** 结果 UI、baseline/candidate、threshold curve；收集实际成本和 judge 稳定性。
5. **Runtime promotion:** 门禁/审计/多 pod/rollback 演练后开放立即发布参数。
6. **Ingestion policy:** 新 KB pin 最新策略；旧 KB 仅显示 drift。
7. **KB opt-in sync:** 大 KB、失败注入、原子切换和 rollback 通过后开放用户同步。
8. **Automation later:** 另行设计 CI/API key scope，不扩大现有 admin key。

任何阶段发现 evaluator 故障都只关闭测评能力，不回滚或影响生产 RAG 查询。

## 决策检查点

| 最晚时间 | 必须确认 |
|---|---|
| Task 10 前 | evaluator LLM/embeddings、endpoint、数据外发、费用 |
| Task 13 前 | 首期导入格式、字段映射、ZIP/目录布局 |
| Task 16/17 前 | 默认 metric bundle、门禁指标与最小样本数 |
| Task 21 前 | 数据集/运行 UI 的导入交互 |
| Task 24 前 | dataset/run/result/generation retention |
| Task 26 前 | 默认并发、预算和生产 rollout 规模 |

这些决策不得通过实现者自行猜测写死；确认后更新 spec 的“待后续决策”和本计划对应 task。

## Spec Coverage / 自审

| Spec 主题 | 计划覆盖 |
|---|---|
| Canonical dataset + adapter | Tasks 4, 13 |
| 真实双 pipeline + trace | Tasks 5, 6, 14, 15 |
| Ragas sidecar | Tasks 10–12 |
| 核心/确定性指标 | Tasks 10, 12, 16 |
| Baseline/Candidate | Tasks 15, 16, 22 |
| RuntimePolicy | Tasks 7, 17 |
| IngestionPolicy | Task 18 |
| Opaque collection/generation | Tasks 8, 9, 14 |
| 整库统一策略同步 | Task 19 |
| Super-admin-only eval | Task 20 |
| Admin UI | Tasks 21, 22 |
| KB owner drift/sync UI | Task 23 |
| Budgets/retention/GC | Tasks 15, 24 |
| Docker/security/deploy | Tasks 11, 25 |
| E2E/migration/release | Task 26 |

### 不可违反的实施不变量

1. Ragas 不实现或替代 bkcrab 的 RAG pipeline。
2. 同一个可见 KB generation 只绑定一个完整 IngestionPolicy。
3. 发布新 IngestionPolicy 不自动改变任何存量 KB。
4. KB 同步完成前，生产查询始终读取旧 active generation。
5. production active pointer 只能在完整校验后由 fenced CAS 切换。
6. 评测数据、结果、策略发布只对未 actAs 的 super_admin session 开放。
7. 评测失败不能影响生产 RAG；sidecar 不健康只关闭新测评。
8. skipped/error 不能当作 0 或从分母中静默消失。
9. promotion 必须版本化、审计、可回滚，不能从 UI 直接改进程字段。
10. 所有昂贵调用和物理资源都有预算、幂等键、引用关系和最终清理路径。
