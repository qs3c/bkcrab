# RAG 测评、参数实验与渐进式策略升级设计

日期：2026-08-05
状态：设计定稿（数据导入格式、评测模型和默认门禁值待后续产品决策）
前置：

- [RAG 板块设计：用户级知识库 + agent 受限检索](2026-07-13-rag-module-design.md)
- [RAG 多模态文档解析与结构化分块设计](2026-07-20-multimodal-document-rag-design.md)

> 本设计把测评建设成一个内部的、可复现的实验闭环：**固定数据集 → 运行 bkcrab 真实索引与在线问答流水线 → Ragas 评分 → 与基线对比 → 生成候选策略 → 人工审核发布 → 可回滚**。Ragas 只负责评分，不重新实现检索或回答。

## 1. 背景与现状

bkcrab 当前已经具备两条完整 RAG 流水线：

1. **索引 pipeline**：原件加载 → parser/DocumentAI → Markdown 规范化 → 分块/增强 → embedding → Milvus；
2. **在线 pipeline**：历史感知 query rewrite + HyDE → dense/BM25 混合召回 → RRF → 可选 reranker → `minScore` 过滤 → 知识库答案生成。

现有实现的重要约束如下：

- `internal/rag/search.go` 的 `SearchWithContext` 是知识库搜索、知识库问答和 Agent `rag_search` 共用的真实检索入口；
- `candidateTopK`、`minScore` 位于进程启动时创建的 `rag.Service` 不可变配置快照中；
- 知识库问答的 `topN=5`、`temperature=0.2` 和 system prompt 仍是处理器内常量/字面量；
- `minScore` 仅在 reranker 成功后生效；reranker 失败时当前行为是退回未应用该阈值的 RRF top-N；
- `rag_document_versions` 已保存解析、分块、模型和 prompt 版本快照，但激活边界仍是逐文档 `active_version`；
- 一个 KB 当前对应一个由 KB ID 派生的 Milvus collection，collection 的 dense 维度创建后不可改变；
- `rag-parser` 已提供成熟的 Python sidecar、内部网络、只读文件系统和资源限制部署范式。

当前缺少的是：

- 可版本化的评测数据集与标准样本契约；
- 隔离于生产索引的 shadow index；
- 对真实 pipeline 的实验参数注入和完整 trace；
- Ragas 评分 sidecar；
- 基线/候选对比、逐样本诊断与参数提升工作流；
- 可热切换的在线策略；
- KB 级统一索引策略与整库原子 generation 切换；
- 超级管理员专用的测评 UI 和审计发布链路。

## 2. 已确认的产品边界

### 2.1 目标

1. 超级管理员主界面提供“RAG 测评”入口；普通用户看不到入口，也不能访问测评 API。
2. 测评数据可以同时携带原始语料和问答样本，使一次完整实验能够真实跑过索引、检索和回答两条 pipeline。
3. Ragas 作为独立 Python sidecar，只评价 bkcrab 真实 pipeline 产生的 `response` 与 `retrieved_contexts`。
4. 支持完整 pipeline 实验和只重跑在线 pipeline 的快速实验。
5. 展示检索、生成、引用、拒答和工程成本多个维度，不用一个未经校准的总分掩盖退化。
6. 实验允许调整完整参数空间，包括分块、Embedding、DocumentAI、Rewrite/HyDE、reranker 和回答参数。
7. 生产发布只允许显式、受控的参数子集；发布有版本、审计、原子切换和回滚。
8. 索引策略发布后只自动用于新建 KB；现有 KB 永远保持原策略，直到 KB 所有者主动“同步最新策略”。
9. 同一个 KB 的可见索引在任意时刻只使用一套完整索引策略，不允许新旧策略按文档混用。
10. KB 同步新策略时，旧 generation 持续提供查询；新 generation 全量完成并校验后一次性切换。
11. 用户在 KB 详情页能看到策略过时提示、参数 diff、成本估算和同步进度，并自主决定是否升级。

### 2.2 明确不做

- 一期不测 Agent 是否正确调用 `rag_search`，也不引入 Tool Call Accuracy、Agent Goal Accuracy 等 Agent 指标。
- 不让 Ragas sidecar 自行实现 parser、chunker、retriever、reranker 或回答链路。
- 不把评测端点暴露到宿主机或公网。
- 不允许参数滑块直接修改当前线上请求使用的值。
- 不在发布新索引策略后自动重建所有存量 KB。
- 不允许同一 KB 的新文档静默采用新策略而旧文档保留老策略。
- 不允许同一 Milvus generation 混用不同 Embedding 模型或维度。
- 不默认生成单一“综合分”；若未来增加，必须先完成权重、量纲和业务门禁校准。
- 一期不启用 Ragas synthetic testset generation；首期评价人工或外部系统提供的固定数据集。
- 文件导入的最终格式组合本设计暂不锁死；先固定内部规范化契约和 adapter 边界。

### 2.3 关键决策

| 决策点 | 结论 |
|---|---|
| 测评对象 | 索引 pipeline + 在线检索/知识库回答 pipeline |
| Agent 测评 | 一期排除 |
| 评分引擎 | 独立 Python `rag-evaluator` sidecar，固定 Ragas 版本 |
| 被测链路 | Go 编排器调用 bkcrab 与生产共用的内部 pipeline 代码 |
| 数据集 | immutable version，包含 corpus + cases；导入 transport 可插拔 |
| 实验索引 | 独立 shadow generation，不读取或修改生产 active collection |
| 参数实验 | 完整参数空间均可调；每次运行保存不可变参数快照 |
| 在线发布 | 版本化 `RAGRuntimePolicy`，原子热切换，新请求生效 |
| 索引发布 | 版本化 `RAGIngestionPolicy`；新 KB pin 最新策略，旧 KB 保持原策略 |
| 旧 KB 升级 | KB 所有者主动同步，整库 shadow rebuild 后原子切换 |
| Embedding 变更 | 必须新建 physical collection/generation，禁止 collection 内混用 |
| 评测结果 | 聚合 + 分布 + 逐样本理由 + trace + 成本 + 基线 delta |
| 普通用户可见面 | 只在自己的 KB 详情页看到策略过时与同步入口；看不到测评后台和平台实验 |

## 3. 术语

- **Dataset**：逻辑数据集，可有多个不可变 Dataset Version。
- **Corpus**：某 Dataset Version 中参与索引的原始文档集合。
- **Case**：问题及可选标准答案、标准上下文、正确 context ID、历史与标签。
- **Experiment Profile**：一次实验使用的完整索引、检索、回答参数集合。
- **Baseline / Candidate**：对照参数方案与候选参数方案。
- **Evaluation Run**：固定 Dataset Version、Profile、代码/模型版本后的一次执行。
- **RAGIngestionPolicy**：平台发布的不可变索引策略版本。
- **RAGRuntimePolicy**：平台发布的不可变在线检索/回答策略版本。
- **Index Generation**：一个 KB 或评测语料在一个索引策略下构建出的完整物理索引世代。
- **Active Generation**：当前对查询可见的 generation。
- **Shadow Generation**：构建和校验中、尚未对正常查询可见的 generation。
- **Promotion**：从实验结果中选择允许发布的参数，生成新的平台策略版本。
- **Policy Drift**：KB pin 的索引策略不是当前平台最新索引策略；这是可选升级状态，不是故障。

## 4. 总体架构

```text
┌───────────────────────────┐
│ Super Admin Web           │
│ 数据集 / 运行 / 结果 / 策略 │
└─────────────┬─────────────┘
              │ admin-only API
              ▼
┌───────────────────────────────────────────────────────────┐
│ Go Evaluation Orchestrator                               │
│ auth / durable tasks / snapshots / budgets / audit       │
│                                                           │
│  ┌─────────────────────┐    ┌──────────────────────────┐  │
│  │ Real ingest pipeline│    │ Real online pipeline     │  │
│  │ parser→chunk→embed  │───▶│ rewrite/HyDE→retrieve   │  │
│  │ →shadow generation  │    │ →rerank→answer           │  │
│  └─────────────────────┘    └──────────────┬───────────┘  │
│                                            │ samples      │
└────────────────────────────────────────────┼──────────────┘
                                             ▼
                               ┌──────────────────────────┐
                               │ rag-evaluator (Python)   │
                               │ Ragas metrics only       │
                               └──────────────┬───────────┘
                                              │ per-case scores/reasons
                                              ▼
                               ┌──────────────────────────┐
                               │ SQL + Object Store       │
                               │ results/traces/artifacts │
                               └──────────────────────────┘
```

职责边界：

- **Go 主服务**拥有租户权限、原始数据、真实 pipeline、参数快照、任务状态、成本账本、结果持久化和策略发布权；
- **Milvus**保存生产/实验 generation 的物理 collection；逻辑 KB 先解析 active generation，再访问物理 collection；
- **对象存储**保存 immutable eval corpus、导入原件、可选大 trace 和导出文件；
- **rag-evaluator**接收有限、标准化的样本批次，运行指标并返回分数与理由；它不持有 Milvus/S3/生产 RAG 凭据；
- **rag-parser**继续只负责文档本地解析，不感知测评概念。

## 5. 规范化数据集

### 5.1 Dataset Version

数据集采用逻辑 Dataset + immutable Version：

```text
Dataset "客服制度黄金集"
  ├── version 1 (immutable)
  ├── version 2 (immutable)
  └── draft import → 校验成功后发布为 version 3
```

已进入 RUNNING 的 run 永远引用具体 `dataset_version_id`。编辑数据不是原地覆盖，而是创建新 version。这样 Baseline 和 Candidate 才能保证输入完全一致。

### 5.2 Corpus Document

规范化 corpus document 至少包含：

```json
{
  "id": "doc-policy-001",
  "fileName": "差旅管理制度.pdf",
  "mediaType": "application/pdf",
  "objectKey": "rag-eval/datasets/.../source.pdf",
  "sha256": "...",
  "sizeBytes": 1234567,
  "metadata": {
    "department": "finance"
  }
}
```

文档原件只存一次；不同 Experiment Profile 可复用原件和满足指纹条件的解析 artifact。

### 5.3 Case

规范化单轮 case：

```json
{
  "id": "case-001",
  "user_input": "出差住宿标准是多少？",
  "reference": "员工住宿标准按城市级别……",
  "reference_contexts": ["……"],
  "reference_context_ids": ["doc-policy-001#section-4"],
  "history": [],
  "expected_abstention": false,
  "tags": ["制度问答", "精确事实"],
  "metadata": {
    "difficulty": "normal"
  }
}
```

字段要求：

- `id`、`user_input` 必填且在 version 内唯一；
- `reference` 可选，但缺失时 Context Recall、Factual Correctness 等参考答案指标不可运行；
- `reference_contexts` / `reference_context_ids` 可选，用于确定性检索指标；
- `history` 一期允许为空或问题字符串列表，但仍只评价单次当前回答；
- `expected_abstention` 用于资料不足/越界问题；
- `tags`、`metadata` 只允许受限大小和深度，供切片分析，不进入 prompt，除非指标显式声明。

### 5.4 导入 adapter 边界

内部只接受上述 canonical model。文件上传、表格、JSON/JSONL、压缩包、手工表单或 API 都是 `EvalDatasetImporter` adapter：

```go
type EvalDatasetImporter interface {
    Validate(ctx context.Context, source ImportSource) (ImportReport, error)
    Normalize(ctx context.Context, source ImportSource) (CanonicalDataset, error)
}
```

最终支持哪些格式、字段映射 UI 和压缩包布局留待专项讨论。首个实现不得把某一种外部文件列名写死进 store 或 runner。

### 5.5 发布校验

Dataset Version 发布前必须校验：

- case ID、document ID 唯一；
- 必填字段、长度、数量、UTF-8、JSON 深度和文件大小上限；
- reference context ID 引用存在；
- 指标所需字段覆盖率；
- 重复问题和完全重复文档提示；
- 空 corpus、空 case、不可识别 media type 拒绝；
- 导入错误按行/样本返回，不回显服务器对象键、凭据或内部路径。

## 6. 实验执行模型

### 6.1 两种运行模式

#### Full Pipeline

用于评价索引参数或做最终候选验证：

```text
Corpus → shadow parse/chunk/embed/index → cases → retrieve/answer → score
```

如果 corpus version + ingestion profile fingerprint 已有 READY 且未过期的 eval generation，可以复用；复用必须记录 generation ID，不能仅靠调用者声明。

#### Online Only

固定一个 READY generation，只改变在线参数：

```text
existing shadow generation → cases → retrieve/answer → score
```

适合快速扫描 `topN`、`candidateTopK`、`minScore`、Rewrite/HyDE、temperature 和 RAG prompt，避免每次重付解析/Embedding 成本。

### 6.2 Baseline/Candidate 公平性

- 同一对比必须使用同一 Dataset Version；
- 在线参数对比必须使用同一 Index Generation；
- 索引参数对比可以使用不同 generation，但回答模型、评测模型和指标版本默认固定；
- 如果同时修改索引和在线参数，UI 必须标注“无法单独归因”；
- 发布某个参数子集前，系统应支持用该子集再跑一次 confirmation run，验证收益不依赖未发布参数。

### 6.3 Run 状态机

```text
DRAFT
  → QUEUED
  → PREPARING_DATA
  → BUILDING_INDEX（online-only 跳过）
  → RUNNING_PIPELINE
  → SCORING
  → AGGREGATING
  → SUCCEEDED

任一执行态 → CANCELLING → CANCELLED
任一执行态 → FAILED
部分指标失败但 pipeline 完成 → SUCCEEDED_WITH_WARNINGS
```

状态更新使用 durable claim/lease/fence。进程重启后可重领未完成任务；同一个 fence 失效后不得继续写结果或切换 generation。

### 6.4 实际 pipeline 与 trace

测评编排器调用内部服务边界，不通过浏览器端点循环调用自身，也不复制生产逻辑。需要从当前代码抽出：

- `SearchOptions`：显式携带 topN、candidateTopK、minScore、Rewrite/HyDE 开关、reranker 策略；
- `SearchTrace`：原始 query、rewrite、HyDE 是否使用、每个 dense route、召回排名/分数、rerank 分数、过滤原因、耗时和降级；
- `AnswerOptions`：模型、temperature、maxTokens、prompt bundle version；
- `AnswerTrace`：最终 response、引用、模型/usage、延迟和错误；
- `ExecutionSnapshot`：代码版本、配置版本、KB/eval generation、provider/model 和 prompt 版本。

普通生产调用从 active runtime policy 构造 options；评测调用从 immutable Experiment Profile 构造 options。二者最终进入同一实现。

评测回答不得写入 `rag_chat_turns`，评测 token/费用记入独立 eval usage ledger，避免污染普通用户用量与聊天历史。

## 7. Shadow Index 与指纹复用

### 7.1 物理 collection key

`vector.Store` 当前参数名虽是 `kbID`，实际需要演进为不透明 `collectionKey`：

```text
生产：rag_kb_<kb-id>_g_<generation-id>
评测：rag_eval_<dataset-version-id>_g_<generation-id>
```

调用者先完成权限检查，再从 SQL 解析被授权的 generation 和 collection key。客户端不得直接提交物理 collection key。

### 7.2 Generation 指纹

评测索引复用指纹至少包含：

- corpus version + 每个原件 sha256；
- parser protocol/engine/version；
- parseMode；
- DocumentAI provider/model/prompt version；
- enrichment provider/model/prompt version；
- chunkSize/chunkOverlap/tokenizer/index format version；
- embedding endpoint contract fingerprint/model/dims；
- Milvus schema/index version。

任何影响 SearchContent、chunk 边界或向量的字段变化都必须生成新 fingerprint。凭据本身不进入明文指纹，只进入稳定、不可逆的 provider contract fingerprint。

### 7.3 生命周期

- BUILDING generation 只有所属 run/worker 可写；
- READY eval generation 可被相同 fingerprint 的后续 run 引用；
- ref_count > 0 或仍被保留 run 引用时不得 GC；
- 到期后先标记 DELETING，再 fenced drop collection 和清理 staging/object；
- 任何失败都不得把不完整 generation 标为 READY。

## 8. Ragas 评分 sidecar

### 8.1 部署与依赖

新增 `services/rag-evaluator`：

- Python 3.12；
- 使用锁文件固定 Ragas 及其传递依赖；
- FastAPI/Uvicorn；
- 非 root、read-only rootfs、`tmpfs /tmp`、cap-drop、no-new-privileges；
- 只连接 `rag-evaluator-internal` 网络；
- 不映射宿主端口；
- 暴露 `/healthz` 和内部 `/v1/evaluate`；
- 镜像、协议和 metric bundle 均有独立版本。

使用 Ragas 当前推荐的 collections-based metric API；不得基于已标记 legacy/deprecated 的全局 metric singleton 编写新代码。

### 8.2 请求协议

Go 按受限批次发送：

```json
{
  "requestId": "evb_...",
  "metricBundleVersion": "rag-core-v1",
  "metrics": [
    "context_precision",
    "context_recall",
    "faithfulness",
    "response_relevancy",
    "factual_correctness"
  ],
  "samples": [
    {
      "caseId": "case-001",
      "userInput": "...",
      "retrievedContexts": ["..."],
      "retrievedContextIds": ["..."],
      "response": "...",
      "reference": "...",
      "referenceContexts": ["..."]
    }
  ]
}
```

响应按样本、按指标返回：

```json
{
  "requestId": "evb_...",
  "ragasVersion": "pinned-version",
  "metricBundleVersion": "rag-core-v1",
  "results": [
    {
      "caseId": "case-001",
      "metrics": {
        "faithfulness": {
          "status": "ok",
          "value": 0.92,
          "reason": "..."
        }
      }
    }
  ]
}
```

协议要求：

- `requestId` 幂等；同一 ID + 同一 body hash 返回同结果，body 不同则 409；
- metric 名称是闭集，sidecar 不接受上传任意 Python/Prompt；
- 缺少必需字段时只把对应 metric 标为 `skipped_missing_input`，不伪造 0 分；
- 单个指标失败不抹掉其它指标结果；
- 限制 samples、contexts、单 context 字节数、总 body 和响应 reason 长度；
- Go 负责最终聚合，sidecar 返回的 aggregate 仅供诊断，不能成为权威数据。

### 8.3 评测模型与凭据

回答模型和评测 judge 必须是两个显式角色。Run snapshot 同时记录：

- answer provider/model/prompt；
- evaluator LLM provider/model；
- evaluator embeddings provider/model；
- Ragas/metric bundle/prompt 版本。

一期建议部署级配置一个专用 evaluator LLM + embeddings，凭据通过环境变量或 Docker secret 注入 sidecar，不随任务 payload 传输。是否允许超级管理员在 UI 选择多个 judge、是否复用现有 provider 凭据，列为后续决策。

任何外部 evaluator 都会接收问题、回答和命中的知识库文本。启用前必须在 UI/部署文档标明数据外发边界；私有部署应能选择内网兼容端点。

## 9. 指标体系

### 9.1 首期核心指标

| 类别 | 指标 | 必需输入 | 含义 |
|---|---|---|---|
| 检索 | Context Precision | question + retrieved contexts，通常需要 reference | 高排名 chunk 是否相关 |
| 检索 | Context Recall | retrieved contexts + reference | 标准答案所需信息是否被召回 |
| 检索 | Hit@K / Recall@K | reference context IDs | 正确 context 是否进入 top-K |
| 检索 | MRR / nDCG | reference context IDs/graded labels | 正确 context 的排序质量 |
| 生成 | Faithfulness | response + retrieved contexts | 回答陈述能否从资料推出 |
| 生成 | Response Relevancy | question + response | 是否直接回答问题 |
| 生成 | Factual Correctness | response + reference | 与标准答案相比是否正确完整 |
| 产品 | Citation Precision | response citations + hits | 引用是否支持对应陈述 |
| 产品 | Citation Coverage | response claims + citations | 需要引用的陈述是否被覆盖 |
| 产品 | Abstention Accuracy | expected_abstention + response/hits | 资料不足时是否正确拒答 |
| 工程 | latency p50/p95 | trace | 各阶段性能 |
| 工程 | error/empty/fallback rate | trace | 稳定性和降级情况 |
| 工程 | token/cost | usage ledger | 质量提升成本 |

Hit@K、MRR、nDCG、引用和拒答指标由 Go 或本项目自定义 metric bundle 确定性计算；不强行包装为 Ragas LLM metric。

### 9.2 二级诊断指标

首期可保留扩展位但不默认运行：

- Context Entities Recall：实体密集型数据集；
- Noise Sensitivity：诊断无关 chunk 对答案的影响；
- 自定义业务 rubric；
- 多模态 Faithfulness/Relevance：只有未来回答模型真正接收视觉输入时才启用；当前“视觉解析、文字回答”不适用。

### 9.3 展示与聚合规则

- 聚合显示 mean、median、分布、样本数、skipped 数、error 数；
- Baseline/Candidate 显示绝对 delta 和相对 delta；
- 可按 dataset tag、文档、问题难度、预期拒答等切片；
- 逐样本展示问题、reference、response、contexts、召回/重排分数、过滤原因、引用和 metric reason；
- `minScore` 页面展示 threshold–precision–recall、空召回率和错误拒答率曲线；
- 不把 `skipped` 当 0，也不在失败样本缺失时用较小分母制造虚假提升；
- 对随机 judge 的 delta 可显示 bootstrap confidence interval；初版若不实现，至少显示样本数并禁止小样本自动发布。

### 9.4 发布门禁

门禁是策略版本的一部分，不写死在代码：

```json
{
  "requiredMetrics": {
    "faithfulness": {"min": 0.90, "maxRegression": 0.01},
    "context_recall": {"min": 0.88, "maxRegression": 0.02}
  },
  "maxP95LatencyRegression": 0.15,
  "maxErrorRate": 0.01,
  "minScoredCases": 100
}
```

首期默认阈值尚未确定。即使全部门禁通过，也只允许超级管理员手动发布，不自动上线。

## 10. 参数模型与发布边界

### 10.1 Experiment Profile：完整可调

实验允许：

**索引参数**

- chunkSize、chunkOverlap；
- parseMode、文本增强；
- Embedding endpoint contract/model/dims；
- DocumentAI vision/text model、prompt/version；
- parser/index format 版本（只由受信任系统选项暴露）。

**在线检索参数**

- topN、candidateTopK；
- reranker 开关、model、timeout、minScore、failure policy；
- Query Rewrite 开关；
- HyDE 开关；
- 未来可显式暴露的 RRF/route 参数。

**回答参数**

- answer model；
- temperature、maxTokens；
- RAG system/user prompt bundle version。

### 10.2 生产发布分类

| 分类 | 参数 | 发布语义 |
|---|---|---|
| 立即发布 | topN、candidateTopK、minScore、temperature、maxTokens、RAG prompt bundle | 生成新 RuntimePolicy，原子切换，新请求生效 |
| 初期仅实验 | Rewrite/HyDE、reranker failure policy、回答/重排模型切换 | 保留扩展位；稳定和门禁校准后才进入立即发布白名单 |
| 索引策略 | chunk、parse/enrichment、DocumentAI prompt/model、Embedding contract | 生成新 IngestionPolicy；只自动用于新 KB |
| 不可直接发布 | 任意未知字段、底层协议/schema 版本 | 需要代码发布或迁移，不由 UI 改动 |

发布 Candidate 的参数子集时必须产生 confirmation run 或明确警告“实验收益由未发布参数共同造成”。

### 10.3 RuntimePolicy

```json
{
  "version": 12,
  "topN": 5,
  "candidateTopK": 20,
  "minScore": 0.56,
  "temperature": 0.2,
  "maxTokens": 4096,
  "ragPromptBundleVersion": "rag-answer-v3"
}
```

- immutable revision；
- SQL 保存 `active_version` 指针；
- 进程内使用 atomic snapshot；每个请求开始时捕获一次，不在请求中途读取新值；
- 发布先写完整 revision，再 CAS active pointer，再广播/轮询刷新多 pod 缓存；
- 旧 revision 保留，可一键回滚；
- 回滚只影响新请求，在途请求完成其原快照；
- active 变更写审计日志并带来源 eval run、操作者和备注。

`minScore` UI 必须称为“最低 reranker 分数”，不能宣传为已经校准的概率。reranker 失败时是否继续当前 RRF fallback 必须在 trace 和结果页显式显示。

### 10.4 IngestionPolicy

```json
{
  "version": 3,
  "chunkSize": 768,
  "chunkOverlap": 96,
  "parseMode": "auto",
  "enrichmentEnabled": true,
  "documentAI": {
    "visionModel": "...",
    "visionPromptVersion": "vision-v4",
    "enrichmentPromptVersion": "enrichment-v2"
  },
  "embedding": {
    "contractFingerprint": "...",
    "model": "bge-m3",
    "dims": 1024
  }
}
```

- immutable revision；
- 平台有一个 active default policy；
- 新 KB 创建事务中 pin 当前 active policy version；
- 旧 KB 不自动跟随新 default，也不在后续上传时静默继承；
- KB 内所有可见文档来自同一 generation，generation 绑定唯一 policy version；
- 已发布 policy 不允许原地编辑 prompt 内容；修改产生新 version。

## 11. KB 策略同步与整库 Generation

### 11.1 为什么不能逐文档激活

现有 `rag_documents.active_version` 允许文档独立 reindex 并逐个激活。它适合“同一 KB 策略不变时的单文档重试/更新”，不满足整库策略升级：如果同步期间逐文档切换，在线检索会同时看到 v2 和 v3 分块/Embedding 结果。

因此新增 KB 级 generation 作为策略升级的可见性边界。逐文档 active version 继续服务于 generation 内部构建和普通同策略文档更新，但搜索策略升级时只读取 `rag_kb_index_generations` 中 active generation 的精确文档版本映射。

### 11.2 Generation 结构

```text
KB
  active_generation_id ──▶ generation 4 / policy v2 / ACTIVE
                           generation 5 / policy v3 / BUILDING

generation 5
  ├── doc A → doc_version 7
  ├── doc B → doc_version 4
  └── doc C → doc_version 9
```

每个 generation 有独立 physical collection，`rag_kb_generation_documents` 保存其精确 corpus membership 与 doc_version。Search 先鉴权逻辑 KB，再读取 active generation mapping 构造 active versions/filter。

### 11.3 同步流程

1. KB 所有者在详情页看到“策略可更新”；
2. UI 获取当前/最新 policy diff 与成本估算；
3. 用户二次确认，超大 KB 可要求输入 KB 名称；
4. 服务端创建固定目标 policy version 的 BUILDING generation 和 durable sync task；
5. 同步期间查询继续读取旧 ACTIVE generation；
6. 一期暂停该 KB 的上传、删除、普通 reindex 和再次同步，避免 corpus 漂移；
7. worker 对 generation corpus 的全部文档建立目标版本；
8. 校验文档数、chunk 数、向量维度、对象/SQL 映射和抽样检索；
9. 单个 SQL 事务/CAS 把新 generation 标为 ACTIVE、旧 generation 标为 RETIRED，并更新 KB pointer/pinned policy；
10. 恢复 KB 写操作；旧 generation 在回滚窗口内保留；
11. 窗口过后 fenced GC。

如果同步期间平台发布 v4，本任务仍完成其创建时捕获的 v3。完成后 KB 可以继续显示 v4 可用。

### 11.4 失败、取消和回滚

- 激活前失败/取消：drop shadow collection，旧 generation 不变；
- lease 丢失：旧 worker 不得继续写或切换；
- 激活后发现问题：在旧 generation 尚未 GC 时 CAS 回滚 pointer；
- 回滚恢复旧 pinned policy；新 generation 转 RETIRED，不做文档级反向迁移；
- 任何状态都不能出现两个 ACTIVE generation；数据库唯一约束/事务必须保证。

### 11.5 KB 详情页

普通 KB 所有者可见：

- 当前 policy version、最新平台 version、状态（最新/可更新/同步中/失败）；
- 当前和目标参数 diff；
- 文档数、总大小、预计页数；
- 预计耗时、DocumentAI/Embedding 调用量、费用和额外临时存储；
- “同步最新策略”按钮及二次确认；
- 同步进度、错误、取消和允许时的回滚。

“策略过时”只是提示，不影响 KB health，也不反复弹阻断式对话框。

## 12. 数据模型

以下为逻辑表；具体字段沿用三方 SQL 方言、UTC 时间、closed status 和现有 migration helper。

### 12.1 测评数据

**rag_eval_datasets**

- `id, name, description, created_by, created_at, updated_at, deleted_at`

**rag_eval_dataset_versions**

- `id, dataset_id, version, status(DRAFT/VALIDATING/READY/FAILED), source_type, manifest_object_key, corpus_sha256, case_count, document_count, total_bytes, validation_report_json, created_by, created_at, ready_at`
- unique `(dataset_id, version)`；READY 后 immutable。

**rag_eval_corpus_documents**

- `id, dataset_version_id, external_id, file_name, media_type, size_bytes, sha256, object_key, metadata_json`
- unique `(dataset_version_id, external_id)`。

**rag_eval_cases**

- `id, dataset_version_id, external_id, user_input, reference, reference_contexts_json, reference_context_ids_json, history_json, expected_abstention, tags_json, metadata_json`
- unique `(dataset_version_id, external_id)`。

### 12.2 实验与结果

**rag_eval_profiles**

- `id, name, profile_json, fingerprint, created_by, created_at`
- profile immutable；修改复制新 profile。

**rag_eval_runs**

- `id, dataset_version_id, baseline_run_id, mode, profile_id, status, stage, progress_json, execution_snapshot_json, index_generation_id, requested_metrics_json, error_code, error_message, created_by, created_at, started_at, finished_at, lease_owner, lease_until, fence_token, cancel_requested_at`

**rag_eval_case_results**

- `run_id, case_id, response, contexts_json 或 trace_object_key, citations_json, search_trace_json, answer_trace_json, status, error_code, error_message, latency_ms, usage_json`
- unique `(run_id, case_id)`；批次幂等 upsert 受 fence 保护。

**rag_eval_metric_results**

- `run_id, case_id, metric_name, metric_version, status, value, reason, details_json`
- unique `(run_id, case_id, metric_name, metric_version)`。

**rag_eval_run_aggregates**

- `run_id, metric_name, slice_key, slice_value, count, scored_count, skipped_count, error_count, mean, median, p50, p95, details_json`

**rag_eval_usage**

- `run_id, case_id, stage, provider, model, input_tokens, output_tokens, estimated_cost_usd, actual_cost_usd, idempotency_key`

### 12.3 策略与 generation

**rag_ingestion_policies**

- `version, policy_json, fingerprint, status(DRAFT/ACTIVE/RETIRED), source_eval_run_id, created_by, note, created_at, activated_at`

**rag_runtime_policies**

- 同类字段；另有 singleton active pointer 或 system setting 保存 active version。

**rag_kb_index_generations**

- `id, kb_id, policy_version, collection_key, embedding_model, embedding_dims, status(BUILDING/READY/ACTIVE/RETIRED/FAILED/DELETING), document_count, chunk_count, error_code, error_message, created_by, created_at, activated_at, retired_at, rollback_until, lease/fence fields`
- 每 KB 最多一个 ACTIVE，由方言兼容的事务/CAS 和约束保证。

**rag_kb_generation_documents**

- `generation_id, doc_id, doc_version, status, error_code, error_message`
- unique `(generation_id, doc_id)`。

**rag_kb_policy_sync_tasks**

- `id, kb_id, source_generation_id, target_generation_id, target_policy_version, status, progress_json, estimate_json, requested_by, cancel_requested_at, lease/fence/retry/error/timestamps`

**rag_policy_audit_log**

- `id, policy_kind, from_version, to_version, action(PUBLISH/ROLLBACK/KB_SYNC/KB_ROLLBACK), actor_id, source_eval_run_id, target_kb_id, note, created_at`

### 12.4 敏感数据

- 对象键、provider fingerprint、judge reason 只向授权调用者返回必要字段；
- 数据库 error_message 必须清洗，不保存 API key、完整 DSN、内部临时路径；
- 大 context/trace 可进入受控对象存储，SQL 只存摘要和 key；
- 删除 Dataset/Run 采用 tombstone + 异步 GC，不能留下孤儿 shadow collection。

## 13. HTTP API 与权限

### 13.1 超级管理员测评 API

一期 UI/API 仅接受 `super_admin` 浏览器会话。现有 `requireSuperAdmin` 同时接受 `type=admin` API key，因此应增加明确的 `requireSuperAdminSession`，不要误把通用 admin key 自动扩大为测评数据访问权。后续 CI 自动化使用独立 `rag_eval` scope 再设计。

建议端点：

```text
GET    /api/admin/rag-evals/capabilities
GET    /api/admin/rag-evals/datasets
POST   /api/admin/rag-evals/datasets
GET    /api/admin/rag-evals/datasets/{id}
POST   /api/admin/rag-evals/datasets/{id}/versions
POST   /api/admin/rag-evals/dataset-versions/{id}/validate
GET    /api/admin/rag-evals/dataset-versions/{id}/validation

GET    /api/admin/rag-evals/profiles
POST   /api/admin/rag-evals/profiles

GET    /api/admin/rag-evals/runs
POST   /api/admin/rag-evals/runs
GET    /api/admin/rag-evals/runs/{id}
POST   /api/admin/rag-evals/runs/{id}/cancel
GET    /api/admin/rag-evals/runs/{id}/cases
GET    /api/admin/rag-evals/runs/{id}/compare/{baselineId}
GET    /api/admin/rag-evals/runs/{id}/export

GET    /api/admin/rag-policies
POST   /api/admin/rag-policies/runtime/promotions
POST   /api/admin/rag-policies/runtime/rollback
POST   /api/admin/rag-policies/ingestion/promotions
```

所有 list 使用 cursor pagination 和 bounded limit；run/case filter 使用闭集字段；创建、取消和发布端点接受 idempotency key。

### 13.2 KB 所有者 API

```text
GET  /api/rag/kbs/{id}/index-policy
POST /api/rag/kbs/{id}/index-policy/estimate-sync
POST /api/rag/kbs/{id}/index-policy/sync
GET  /api/rag/kbs/{id}/index-policy/sync/{taskId}
POST /api/rag/kbs/{id}/index-policy/sync/{taskId}/cancel
POST /api/rag/kbs/{id}/index-policy/rollback
```

沿用现有 KB owner/super_admin 权限。普通用户只能操作自己的 KB；策略内容返回可展示字段，不返回 endpoint、API key 或 provider secret。

## 14. Web UI

### 14.1 管理员入口

- `app-sidebar.tsx` 的管理员 Agent 分组在“知识库”附近增加“RAG 测评”；
- 路由 `/admin/rag-evals/` 自动落入管理员路径门禁；
- 概览页增加快捷卡片：最近一次回归、是否通过门禁、失败指标和“开始测评”；
- 普通用户的导航、路由预取和页面 bundle 不展示业务入口，但安全仍以服务端为准。

### 14.2 测评页面

四个主标签：

1. **数据集**：列表、版本、corpus/case 预览、字段覆盖率、校验错误；
2. **测评运行**：选择 Dataset Version、Full/Online-only、Baseline/Candidate、指标、预算并启动；
3. **结果分析**：聚合卡片、分布、切片、逐样本、trace、threshold 曲线和 baseline delta；
4. **参数策略**：Experiment Profile、可发布参数提取、confirmation run、Runtime/Ingestion policy 历史、发布与回滚。

运行详情通过有界轮询获取状态；后续可换 SSE，但一期不为进度引入额外长连接基础设施。

### 14.3 结果页面原则

- 首屏显示质量、性能、成本三组，不只显示 Ragas 分数；
- skipped/error 与 scored 分母同时显示；
- 点击退化指标直接筛出贡献最大的失败 case；
- 每个 case 可对照 Baseline/Candidate 的 contexts、排序、回答和评分理由；
- 发布按钮只在门禁、权限、confirmation 条件满足时可用；disabled 状态解释原因。

### 14.4 KB 详情页策略提示

- `latest`：不显示干扰性提示；
- `outdated`：非阻断 banner，展示新版本与 diff；
- `syncing`：进度、旧索引仍在线、写操作暂不可用；
- `failed`：旧索引正常、显示清洗错误和重试；
- 同步确认框展示影响和估算，绝不使用含糊的“立即应用”。

## 15. 并发、成本与资源隔离

- eval runner 使用独立 worker queue/concurrency，不占满普通 RAG index workers；
- parser、DocumentAI、embedding、reranker、answer judge 均有分阶段并发上限；
- 创建 run 前生成粗估，启动时设置最大文档/页数/case/token/cost budget；
- 达到预算时停止发起新外部调用，run 进入明确的 budget-exceeded 失败或警告状态；
- 同一 dataset fingerprint 的 generation 构建使用 singleflight + SQL claim，不能并发重复付费；
- 同一 KB 同时只能有一个 policy sync task；
- KB sync 与删除互斥，用户删除/停用仍有更高优先级并能使 lease 心跳失败；
- 生产查询与 KB sync 使用独立 collection；同步不得降低旧索引的读可用性；
- sidecar 和 runner 都设置 CPU、内存、pids、body、timeout 和 cancellation 限制。

Phase H 的开发/单机部署基线采用：eval worker 并发 1、单 run 最多 1,000 cases / 2,000,000 tokens / 25 USD / 6 小时，run retention 90 天、dataset/result retention 365 天、generation rollback retention 30 天。所有数值均为可配置上限；生产放量前仍须按实际模型单价、数据规模和压测结果复核，不能把这组开发基线视为无限容量承诺。

## 16. 可复现性与审计

每个 run 保存：

- Dataset Version 和 corpus manifest hash；
- Experiment Profile JSON/fingerprint；
- index generation/fingerprint；
- bkcrab commit、主镜像 digest、parser/evaluator 镜像和协议版本；
- answer/evaluator/embedding/reranker/DocumentAI provider+model contract；
- Ragas version、metric bundle 和 prompt version；
- Runtime/Ingestion policy version；
- 每个 case 的上下文 ID、排名、分数、回答、usage、错误和 metric reason；
- 发起人、时间、取消/重试、promotion/rollback 链路。

Ragas/LLM judge 可能有随机性。默认使用可用的最低 temperature；若 provider 不保证确定性，UI 不得宣称 bit-for-bit 可复现，只能保证输入、配置和版本可复查。

## 17. 错误与降级

| 场景 | 行为 |
|---|---|
| Dataset 校验失败 | version 保持 FAILED/DRAFT，返回逐项报告，不创建 run |
| Shadow index 部分失败 | generation FAILED，不进入 READY；可按 durable task 规则重试 |
| Reranker 失败 | 按 profile 的 closed failure policy；当前兼容策略为 RRF fallback，并在 trace 标记 |
| 单 case pipeline 失败 | 保存 case error，继续其它 case；聚合显示失败率 |
| 单 metric 失败 | 其它 metric 保留；该 metric 标 error，不记 0 |
| evaluator sidecar 不健康 | capabilities 禁止新 run 或评分阶段可重试；不影响生产 RAG |
| judge provider 限流 | 指数退避、预算/截止时间约束；不可无限重试 |
| Run 取消 | 停止新调用，等待/取消在途任务，清理无引用 shadow artifact |
| KB 同步失败 | 继续读旧 generation，恢复写操作，用户可重试 |
| KB 激活 CAS 失败 | 新 generation 不可见，重新读取状态后决定 retire/重试 |
| 多 pod 策略刷新失败 | 未确认 revision 的 pod 继续旧快照并报警；不得加载半个 policy |

## 18. 安全与隐私

- 测评 UI、数据集、结果和策略发布仅 super_admin session；
- KB 策略同步仅 KB owner 或 super_admin；
- 所有对象下载、结果导出和逐 case trace 都重新鉴权，不依赖前端隐藏；
- sidecar 网络入口只允许 bkcrab 服务网络，协议有 service token/mTLS 扩展位；
- sidecar 不获取 Milvus、S3、用户 provider、生产数据库凭据；
- 导入文件按现有 RAG 上传限制和 parser 安全协议处理，禁止路径穿越、SSRF 和压缩炸弹；
- retrieved context 属于不可信文档数据；metric prompt 必须把它包裹为数据，防止文档 prompt injection 改写 judge 任务；
- evaluator 数据外发需要部署者明确配置，日志不记录原文、API key 或完整回答；
- 删除 run/dataset 后按 retention/tombstone 异步清理 SQL、对象和 shadow collection；
- policy publish、rollback、KB sync 全部审计。

## 19. 测试策略

### 19.1 单元测试

- canonical dataset validation、字段覆盖率和 import adapter；
- profile/policy JSON closed schema、fingerprint、secret omission；
- deterministic IR metrics、citation/abstention；
- aggregate 对 skipped/error 的分母规则；
- RuntimePolicy atomic snapshot；
- generation 状态机、唯一 ACTIVE、CAS、取消/回滚；
- Ragas client protocol、idempotency、body limit、partial metric error；
- auth：普通用户、只读 actAs、admin API key 都不能访问一期 eval API。

### 19.2 集成测试

- fake parser/embed/vector/reranker/LLM 的 Full Pipeline run；
- 同 fingerprint generation 复用和不同 fingerprint 强制新建；
- baseline/candidate 使用同一 dataset/index 的断言；
- runner 重启、lease 丢失、旧 fence 不能写结果；
- KB v2 → shadow v3 → 全部完成 → atomic switch；
- v3 构建中查询始终只见 v2；
- 同步失败/取消后旧 generation 和写操作恢复；
- Embedding dims 变化创建新 physical collection；
- SQLite/MySQL/Postgres 方言迁移与事务行为。

### 19.3 Sidecar 测试

- pinned Ragas API smoke；
- 每个 metric 的字段要求、skip、partial failure；
- fake evaluator LLM/embeddings，不在默认测试访问网络；
- request idempotency、batch/bytes/timeout/cancel；
- prompt injection corpus 不能改变 metric 输出 schema；
- 非 root、read-only、healthcheck 和无公开端口部署断言。

### 19.4 E2E 黄金集

最小黄金集覆盖：

- 可回答精确事实；
- 多 chunk 才能完整回答；
- 应拒答问题；
- topN/candidateTopK/minScore 改变产生可解释排名差异；
- chunk 参数改变产生不同 index generation；
- DocumentAI prompt/version 进入 snapshot；
- baseline/candidate 逐样本 diff；
- promotion → RuntimePolicy 生效 → rollback；
- IngestionPolicy 发布 → 新 KB pin 新版 → 旧 KB 只提示 → 用户同步整库切换。

真实 Ragas/provider/Milvus 集成测试使用显式环境变量门控，不进入默认离线测试。

## 20. 发布与迁移

1. 数据库先增加 eval/policy/generation 表，不改变现有查询行为；
2. 为现有 KB 创建 generation 0/1，绑定由现有 KB + document version snapshots 合成的 legacy policy；
3. backfill active generation mapping，双读校验后才切换 Search 到 generation resolver；
4. 引入 RuntimePolicy 时，以当前常量/配置生成 version 1，保证行为不变：topN=5、temperature=0.2、当前 candidateTopK/minScore/prompt；
5. 部署 evaluator sidecar但 feature flag 默认关闭；健康和 golden smoke 通过后只对 super_admin 开启；
6. 首批只开放数据集、运行、结果，不开放 promotion；
7. 完成门禁、审计、多 pod 刷新和 rollback 后开放 RuntimePolicy 发布；
8. 完成 KB generation shadow rebuild、写互斥、估算和回滚后开放 IngestionPolicy 发布及用户同步；
9. 任何阶段都不自动重建存量 KB。

## 21. 验收标准

1. 普通用户无法看到或访问 RAG 测评后台；超级管理员可以从主界面进入。
2. 一个 immutable Dataset Version 能完整跑过真实索引与知识库问答 pipeline，并保存可复查 snapshot。
3. Ragas sidecar 不实现检索、不公开端口，单 metric 失败不会丢失其它结果。
4. 结果页同时展示检索、回答、工程指标、分布、失败数和逐样本 trace。
5. Baseline/Candidate 对比能证明使用同一 Dataset Version；online-only 对比使用同一 generation。
6. 生产参数不能从实验表单直接上线；必须 promotion、门禁、审计和原子 policy revision。
7. RuntimePolicy 发布只影响新请求，多 pod 最终收敛且可回滚。
8. 新 IngestionPolicy 只自动用于新建 KB；旧 KB 查询和写入行为不被自动改变。
9. 旧 KB 详情页显示非阻断的过时提示和参数 diff，所有者可自主同步。
10. KB 同步期间查询始终读取完整旧 generation；新 generation 全部成功后一次切换，任何时刻不混用策略。
11. 同步失败、取消或进程重启不损坏旧索引；Embedding 维度变化能通过新 collection 完成迁移。
12. 数据集、run、generation、policy 和对象均有有界生命周期，无孤儿 collection 和未授权原文泄漏。

## 22. 待后续决策

以下不阻塞架构和底层契约，但在对应 UI/发布阶段前必须确认：

1. 首期导入格式：JSONL/JSON、CSV/XLSX、目录/ZIP、手工录入分别支持到什么程度；
2. corpus 与 cases 的外部文件布局、字段映射和错误修复体验；
3. 专用 evaluator LLM/embeddings 的模型、endpoint、数据外发政策和费用单价；
4. 默认 metric bundle、业务 rubric 和发布门禁阈值；
5. 生产环境是否需要覆盖 Phase H 的 run 90 天、dataset/result 365 天、generation 30 天开发基线，以及除 JSON/CSV 外的导出格式；
6. 生产放量是否需要覆盖 Phase H 的 worker=1、单 run 1,000 cases / 2,000,000 tokens / 25 USD / 6 小时开发基线；
7. 首期是否展示置信区间、是否支持重复 judge 投票；
8. 未来 CI 自动化需要的独立 API key scope；
9. 是否以及何时开放 Ragas synthetic testset generation。

## 23. 被否决方案

### 23.1 Ragas 自己调用一套检索实现

会产生与生产 pipeline 不一致的伪测评，拒绝。

### 23.2 在 Go 主进程嵌入 Python/Ragas

依赖、崩溃域和发布复杂度不可控，拒绝；采用受限 sidecar。

### 23.3 参数滑块直接修改线上配置

缺少基线、门禁、版本和回滚，拒绝；必须 promotion 到 immutable policy。

### 23.4 新索引策略只用于旧 KB 的后续文档

会让同一 KB 混用策略，拒绝。旧 KB 要么完整保留旧 generation，要么整库同步后一次切换。

### 23.5 发布索引策略后自动重建全平台 KB

成本、时延和故障面不可接受，拒绝；新 KB 自动用新版，旧 KB 由所有者渐进同步。

### 23.6 KB 同步时逐文档切 active version

同步窗口会对查询暴露混合策略，拒绝；使用 KB 级 generation pointer。

### 23.7 只存平均分

无法定位失败、会隐藏 skipped/error 和切片退化，拒绝；保存逐样本结果和完整分母。

## 24. 预计代码落点

```text
internal/config/
  config.go                     # evaluator、eval limits、feature gate
  env.go

internal/store/
  rag_eval.go                   # dataset/run/result/usage
  rag_policy.go                 # policy/generation/sync/audit
  database.go
  database_mysql.go

internal/rag/eval/
  model.go                      # canonical dataset/profile/result
  validate.go
  importer.go
  runner.go
  aggregate.go
  metrics.go                    # deterministic metrics
  ragas_client.go
  fingerprint.go

internal/rag/
  search.go                     # SearchOptions/SearchTrace shared boundary
  answer.go                     # shared knowledge answer boundary
  generation.go                 # generation resolver/build/switch/GC
  policy.go                     # runtime/ingestion snapshot
  pipeline.go                   # explicit target collection/generation
  vector/vector.go              # opaque collection key

internal/setup/
  handlers_rag_eval.go          # super-admin eval API
  handlers_rag_policy.go        # promotion + KB sync API
  server.go

services/rag-evaluator/
  Dockerfile
  pyproject.toml
  uv.lock
  app/main.py
  app/protocol.py
  app/metrics.py
  tests/

web/src/app/admin/rag-evals/
  page.tsx
  components/

web/src/app/knowledge/
  page.tsx                      # policy drift/sync UI

web/src/lib/api.ts
web/src/components/app-sidebar.tsx

deploy/docker/
  docker-compose.rag.yml
  .env.example
```

## 25. 参考

- [Ragas Evaluation Dataset](https://docs.ragas.io/en/stable/concepts/components/eval_dataset/)
- [Ragas available metrics](https://docs.ragas.io/en/stable/concepts/metrics/available_metrics/)
- [Ragas evaluation schema](https://docs.ragas.io/en/latest/references/evaluation_schema/)
- [Ragas evaluate a RAG application](https://docs.ragas.io/en/stable/howtos/applications/evaluate-and-improve-rag/)
- 现有实现：`internal/rag/search.go`、`internal/rag/pipeline.go`、`internal/rag/snapshot.go`、`internal/store/rag.go`
- 现有 sidecar：`services/rag-parser`、`deploy/docker/docker-compose.rag.yml`
