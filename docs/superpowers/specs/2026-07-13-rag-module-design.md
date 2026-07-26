# RAG 板块设计：用户级知识库 + agent 受限检索

日期：2026-07-13
状态：已与用户逐节确认

## 背景与目标

bkcrab 目前没有 RAG 能力。本设计新增一个 RAG 板块：**知识库（KB）是用户级资源**——用户像安装技能一样自行创建、配置知识库并上传文档；**agent 是受限访问方**——由用户在 agent 配置中显式授权若干 KB，agent 通过唯一的只读工具 `rag_search` 检索。

入库流水线与检索策略参考 `jc-rag-kb-backend`（Java/Spring，PostgreSQL+pgvector）项目，移植其设计而非代码。

### 已确认的关键决策

| 决策点 | 结论 |
|---|---|
| 部署形态 | bkcrab 内嵌纯 Go 流水线 + 外部向量库 |
| 向量库 | Milvus（内部窄接口 `VectorStore`，可插拔其它实现） |
| agent 访问面 | 只读检索，单工具 `rag_search`；入库/删除/配置全走用户侧 Web 控制台 |
| 授权模型 | agent 配置引用 KB（`rag.kbs` 列表，同 `Skills []string` 模式），只能引用 agent 所属用户自己的 KB |
| Embedding 配置 | 系统级默认 + 用户级覆盖（OpenAI 兼容 API）；KB 创建时快照模型+维度并永久绑定 |
| 检索策略（一期） | 混合召回（dense + BM25 全文，均在 Milvus 内）+ RRF 融合；HyDE / Reranker / 置信度过滤留二期接口位 |
| 原始文件存储 | 复用现有 S3/MinIO 基础设施（`workspace.S3Config` 同套凭据），RAG 独立 key 前缀 |

## 第 1 节：整体架构与数据模型

新增包 `internal/rag/`，对外暴露单一门面 `rag.Service`（KB 管理 + 入库 + 检索）。内部子包：

### rag/store —— 主库三张新表

挂在现有 `store.Store` 上，MySQL/PG/SQLite 三方言通吃：

- **rag_kbs**：`id, user_id, name, description, embed_provider, embed_model, embed_dims, chunk_size, chunk_overlap, status, created_at, updated_at`。embedding 模型+维度在建库时快照、之后只读；换模型 = 显式"重建索引"操作。
- **rag_documents**：`id, kb_id, file_name, file_type, file_size, object_key, status(PENDING/PROCESSING/DONE/FAILED), error_msg, chunk_count, token_count, version, uploaded_at, indexed_at`。`version` 每次重建索引递增，用于识别并删除旧版本 chunks。
- **rag_index_tasks**：`id, doc_id, status, retry_count, max_retry, error_msg, created_at, started_at, finished_at`。异步索引状态 + 崩溃恢复：进程启动时把 PENDING/RUNNING 残留任务重新入队。

### rag/vector —— 向量库窄接口

```
VectorStore:
  EnsureCollection(kb)          // 建 collection：dense 向量字段(维度=KB快照) + VARCHAR 正文 + BM25 sparse 字段 + 标量字段
  UpsertChunks(kb, doc, chunks) // 带 doc_version 写入
  DeleteByDocVersion(kb, docID, olderThan) // 删旧版本实体
  DropCollection(kb)
  HybridSearch(kb, queryVec, queryText, topK) // dense+BM25 双路，Milvus 原生 RRFRanker 融合
```

首个实现：Milvus（官方 Go SDK）。**chunk 正文只存 Milvus**（VARCHAR 字段 + 标量字段 `doc_id/chunk_index/section_title/page_num/doc_version`），主库不重复存；重建索引从 S3 原件重跑流水线。

**Collection 策略：每 KB 一个 collection**（`rag_kb_<id>`，建库时按绑定维度创建，删库时 drop）。隔离干净、删除 O(1)、不同维度互不干扰。备选方案（按维度共享 collection + kb_id 分区键）在 KB 数量上千后经同一接口迁移。

### rag/objects —— 原始文件

复用 `workspace.S3Config` 同套 S3 凭据，独立薄封装（不塞进 agent 作用域的 `workspace.Store`，键形状不同）。Key：`rag/<user_id>/<kb_id>/<doc_id>/<file_name>`。

### 配置面（沿用三层 scope 合并）

- 系统级 `bkcrab.json`：`rag: { milvus: {address, user, password}, embedding: {endpoint, apiKey, model, dims}, limits: {maxFileMB, maxDocsPerKB, maxKBsPerUser} }`
- 用户级（configs 表 scope=user）：可覆盖 embedding 提供商（自带 key / 自选模型）
- agent 级（agents.config JSON）：`rag: { kbs: ["kb_..."], topN }`；后端校验每个 kb 的 `user_id == agent 所属 user`

## 第 2 节：入库流水线与检索链路

### 入库流水线（rag/pipeline）

1. **上传**：HTTP 收文件 → 校验类型/大小 → 原件写 S3 → 主库插 `rag_documents`(PENDING) + `rag_index_tasks` → 提交进程内 worker 池（有界队列）；任务表是恢复凭据。
2. **解析**（rag/parse）：一期支持 **MD / TXT / PDF / DOCX**。MD/TXT 原生；PDF 用纯 Go 文本提取（不做 OCR，扫描件报"无有效文本"）；DOCX 解 zip 提取 `document.xml` 纯文本并保留段落/标题样式。产出 `ParseResult{pages[], 标题结构}`。
3. **分块**（rag/split）：移植参考项目两个分块器——结构感知分块（MD 标题层级 / DOCX Heading 样式，携带 `section_title`）优先；无结构退化为滑动窗口（默认 chunk_size=512 token 估算、overlap=64，KB 级可配）。
4. **Embedding**（rag/embed）：OpenAI 兼容 `/v1/embeddings` 客户端，批量（每批 ≤16 条），按 KB 快照模型+维度请求，维度不符即报错。
5. **写入**：**先算后删再写**——全部向量算完后才向 Milvus upsert 新版本 chunks（带 `doc_version`），成功后删除该 doc 旧版本实体；期间旧数据始终可查。最后更新 `rag_documents` 状态/统计。
6. **失败重试**：任一步失败 → 任务 FAILED + error_msg → 指数退避（1s/2s/4s，最多 3 次）。

### 检索链路（Service.Search，即 rag_search 工具的实现）

1. 解析 agent 授权 KB 列表（工具参数可选 `kb` 名过滤，默认查全部授权 KB）；
2. query 算 embedding（按各 KB 绑定模型；同模型 KB 共享一次调用）；
3. 每 KB 在 Milvus 内 hybrid search：dense 路 + BM25 全文路，**RRF(k=60) 用 Milvus 原生 RRFRanker 完成**；
4. 跨 KB 按分数归并取 top-N（默认 5，agent 配置可调）；
5. 渲染工具文本：每条带 `[来源: <文档名> · <章节> · chunk#n]` 引用头 + 正文；空结果返回明确的"未检索到相关内容"文案。

`Service.Search` 内部分召回、后处理两段；后处理段一期只有截断 top-N，HyDE/Reranker/置信度过滤二期在此插入。

### rag_search 工具注册

走 builtin 工具注册表（与 `skill_manage` 同路径，`internal/agent/tools/rag_search.go`）。**仅当 agent 配置 `rag.kbs` 非空且系统 RAG 配置可用时才注册**（沿用"缺配置则不向 LLM 展示工具"规则）。工具描述动态列出已授权 KB 的名字与描述。每回合经 `registry.ForTurn()` 隔离，遵守多会话并发不变量。

## 第 3 节：HTTP API、Web UI、错误处理与测试

### HTTP API（internal/setup/handlers_rag.go）

沿用现有 handler 风格与 auth 中间件；全部要求登录态，资源归属校验 `user_id`，super_admin 可越权管理。

- KB 管理：`GET/POST /api/rag/kbs`；`GET/PATCH/DELETE /api/rag/kbs/{id}`（PATCH 仅 name/description/分块参数；DELETE 级联：Milvus drop + S3 删前缀 + 主库删行）
- 文档管理：`POST /api/rag/kbs/{id}/documents`（multipart，立即返回 doc 行，索引异步）；`GET .../documents`（状态供前端轮询）；`DELETE .../documents/{docId}`；`POST .../documents/{docId}/reindex`
- 调试检索：`POST /api/rag/kbs/{id}/search`（控制台试查召回质量）
- agent 授权：不新增端点，复用 agent 配置更新通道（`rag.kbs`），后端校验 KB 归属。

### Web UI（web/ dashboard 两处增量）

1. 用户侧「知识库」页：KB 列表/新建（选填分块参数，展示 embedding 快照）、文档上传列表（状态徽标 + 错误信息 + 重建索引按钮）、检索测试框；
2. agent 设置页「知识库访问」多选，列出 owner 的 KB 勾选授权。

### 错误处理

- **Milvus 不可达**：入库任务退避重试；`rag_search` 返回工具错误文本（"知识库服务暂不可用"），不打断 agent 回合（与 web_search 失败行为一致）。
- **embedding 失败**：入库侧任务 FAILED 带上游错误；检索侧返回工具错误文本。
- **配置漂移**：KB 的 embedding 快照与用户当前配置不一致时，入库/检索仍按快照走（保证维度一致）；UI 提示"该 KB 使用旧模型，可重建索引升级"。
- **删除竞态**：删 KB 先置 `status=deleting` 拒绝新任务，等在途任务终态后清理三处存储；agent 配置中悬空 kb 引用在工具注册时静默剔除并打日志。
- **配额**：`maxFileMB / maxDocsPerKB / maxKBsPerUser` 在上传/建库入口硬校验。

### 测试策略

- 纯逻辑单测：分块器（结构感知/滑窗/中文边界）、解析器（固定样例 PDF/DOCX/MD）、跨 KB 归并、工具参数校验；
- `VectorStore` 与 embedding 客户端各配内存 fake，覆盖 pipeline 全流程（含"先算后删再写"顺序断言、重试路径）与 `Service.Search`；
- store 新表：现有 SQLite 单测模式 + 环境变量门控的 MySQL 集成测试（同 `mysql_integration_test.go` 模式）；
- Milvus 真实集成测试：env 门控（本地 docker Milvus standalone）；
- 工具注册测试：无授权 KB 不出工具、`ForTurn` 副本隔离。

## 二期展望（本期不实现，仅留接口位）

- HyDE 二次召回、Reranker 精排、置信度过滤（`Service.Search` 后处理段插入）；
- KB 侧 ACL 表（跨用户共享 / is_public / READ-WRITE 分级）；
- `rag_ingest` 受控写入工具（agent 自沉淀知识）；
- 按维度共享 collection + 分区键（KB 数量上千后）。

## 参考

- 参考项目：`D:\fromGithub\jc-rag-kb\jc-rag-kb-backend`（入库流水线 `IndexService`、混合检索 `HybridRetrieverService`、增强检索 `EnhancedRetrieverService`、完整管线 `FullRagPipeline`）
- bkcrab 现有范式：技能分层加载（`internal/agent/skills.go`）、toolproviders 链（`internal/toolproviders/registry.go`）、S3 工作区（`internal/workspace/s3.go`）、builtin 工具注册（`internal/agent/tools/`）
