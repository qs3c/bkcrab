# 三数据集 RAG 基准目录实施计划

日期：2026-08-18
状态：实施中
依赖：[RAG 测评设计](../specs/2026-08-05-rag-evaluation-design.md)

## 目标

把现有面向开发者的 canonical JSON 上传入口升级为超级管理员可直接使用的基准目录。用户只选择数据集、样本范围和实验 Profile；bkcrab 在服务端下载、缓存、规范化、校验并冻结数据版本，然后用生产共用的 RAG 组件运行实验。

首期只支持三个数据集：

1. IBM MultiDoc2Dial：多文档、多轮指代检索；
2. next-tat/TAT-QA：段落、表格和数值问答；
3. Vectara Open RAGBench：预处理文本主轨，以及原始 PDF 端到端补充轨。

Galileo RAGBench、Judge Calibration 和任意 Hugging Face URL 自动推断明确不在本期范围。Ragas/Judge 仍可作为语义评分器，但不是被测系统。

## 产品边界

### 测评轨道

- `TEXT_RAG`：Adapter 产出的规范化 UTF-8 文本直接进入 splitter、embedding、index、retrieve、reranker 和 answer；Parser 不进入主榜耗时。
- `PDF_E2E`：仅 Open RAGBench 原始 PDF 使用。运行完整 parser/DocumentAI/RAG 链路，解析耗时和 RAG 耗时分栏展示，不与文本主榜聚合。
- 现有 `ONLINE_ONLY` 继续作为同一 READY generation 上的快速在线参数实验，不是数据集类型。

### 数据集生命周期

```text
Catalog preset
  -> server cache (dataset + pinned revision + path + sha256)
  -> import job
  -> canonical corpus/cases
  -> validation
  -> immutable READY Dataset Version
  -> one or more eval runs
```

同一服务器上的公开 Hugging Face 原件按内容寻址共享；不同用户不会重复下载相同 revision。逻辑数据集、权限、运行和用量仍按创建者隔离。READY 版本不可原地改写；修正 Adapter、split、sample seed 或 evidence filter 时创建新版本。

### 用户交互

测评页面拆成“数据集库 / 实验配置 / 运行任务 / 结果对比”：

- 数据集库显示三个经过测试的 preset，不暴露外部表结构；
- 导入表单只展示 split、sample size、seed，以及 Open RAGBench evidence type/text-or-PDF track；
- 后台异步显示 download/cache/normalize/validate/publish 进度；
- READY 详情展示 source revision、hash、许可、文档/Case 数、指标覆盖率、随机样例和错误；
- Run 固定引用 Dataset Version、Profile 和 case selector；Baseline/Candidate 必须使用相同 selector fingerprint。

自定义数据一期继续支持 canonical JSON；后续增加 `Corpus ZIP + Cases JSONL` 和字段映射，但不阻塞三个内置 Adapter。

## 规范化契约

### Dataset Version 来源

版本快照新增：

- catalog dataset ID、source URL、pinned revision；
- Adapter ID 与 Adapter contract version；
- split、sample size、seed、evidence type filter；
- track（`TEXT_RAG`/`PDF_E2E`）；
- source file checksums、license 和 normalization summary；
- selector fingerprint 与完整 canonical fingerprint。

这些字段进入 source manifest 和版本 fingerprint，不包含凭据、临时 URL、对象键或下载时间。

### Case

在现有 Case 上新增 `reference_document_ids`。外部 qrels 在 Adapter 中映射为稳定文档 ID；`reference_context_ids` 只用于确实拥有稳定 chunk/section anchor 的数据，不把外部 section ID 冒充 bkcrab 动态 chunk index。

`history` 只保存当前问题之前的用户问题，不保存历史 AI 回复。评测执行必须与生产问答一致，把 history 分别传给 query planner 和 answer prompt，且不得写普通聊天历史。

### 文档级确定性指标

从检索命中中按首次出现顺序去重 `DocID`，计算：

- `doc_hit_at_k`；
- `doc_recall_at_k`；
- `doc_mrr`；
- `doc_ndcg`。

现有 chunk-level `hit_at_k/recall_at_k/mrr/ndcg` 保留，但没有稳定 `reference_context_ids` 时明确 skip。

## 三个 Adapter

### MultiDoc2Dial

- 固定公开 revision；
- 文档规范化为 Markdown；
- 对话 Case 只保留之前的 user questions 作为 history；
- 保留 domain、dialogue ID、turn ID 等切片标签；
- 生成 reference document IDs，并在没有可靠 section-to-chunk anchor 时不生成伪 chunk qrels。

### TAT-QA

- 固定公开 revision；
- paragraph 与 table 确定性渲染为 Markdown；
- 数字、scale、answer type、derivation 保存在受限 metadata；
- 添加 normalized exact match、numeric answer/scale 所需输入；
- 不通过 Office/PDF Parser，属于 `TEXT_RAG`。

### Open RAGBench

- `TEXT_RAG` 使用官方预处理 corpus；
- evidence types 区分 text、text-table、text-image、text-table-image；文本主榜默认 text，table 单列，依赖 image 的 Case 不混入无视觉能力的主榜；
- `PDF_E2E` 下载官方 PDF，解析与 RAG 阶段分别计时；
- qrels 的 doc ID 映射为 `reference_document_ids`，section ID 仅保存在 metadata，直到 chunk provenance 能稳定携带 source section anchor。

## 导入与缓存

- 使用 Hub 的普通 HTTPS 文件接口和固定 revision，不执行数据集仓库中的远程代码；
- host、redirect、响应大小、文件数、总字节数、超时和 checksum 全部有界；
- 原件先流式写 content-addressed staging，校验后原子发布到共享 cache；
- import job 使用 durable claim/lease/fence/cancel，HTTP 请求只创建任务；
- cache 使用引用计数/最后访问时间清理，不能删除仍被 READY version 引用的 blob；
- 内置公开数据的网络与磁盘成本只发生一次，重新抽样只创建小型 manifest/cases 快照。

## 分阶段计时

Generation 记录每个文档及聚合的：normalize/bypass parser、parse（PDF only）、split/enrich、embedding、vector write/finalize。Case 记录 query planning、query embedding、dense/sparse recall、fusion、reranker、answer 和 judge。

主榜至少展示：

- ingestion wall time 与文档/字符/chunk throughput；
- retrieval p50/p95；
- reranker p50/p95；
- answer p50/p95、tokens 和 cost；
- PDF 轨额外展示 parser/DocumentAI p50/p95，不计入文本主榜。

## 固定样本与公平比较

- sample seed 只控制 Case 选择，不声称固定 LLM 输出；
- 使用稳定 hash ranking（dataset version + case external ID + seed）选样，避免依赖存储返回顺序；
- selector 和选中 Case ID 列表进入 run snapshot/fingerprint；
- paired compare 必须验证 Dataset Version 与 selector fingerprint 相同；
- 支持按 split、tag、evidence type 和 deterministic shard 选择，不复制 corpus。

## 实施顺序

1. Case/source/run contract、数据库迁移、文档级指标与 selector；
2. history 生产链路对齐和阶段 trace；
3. import job、共享 cache、Adapter registry 与安全下载器；
4. 三个 Adapter 及固定小型 golden fixtures；
5. catalog/import/preview/run UI；
6. 单元、迁移、前端、Compose 和真实小样本 smoke；
7. 本机从当前 `main` 重建部署并验证服务健康。

## 验收条件

- 三个 preset 均可从 UI 创建异步导入；同 revision 第二次导入不产生网络下载；
- READY version 能展示来源、hash、许可、覆盖率和样例；
- MultiDoc2Dial 的历史 AI 回复不会进入 planner/answer；
- 文档 qrels 与动态 chunk ID 不再直接比较，doc metrics 有确定性单测；
- 相同 seed 选出相同 Case，不同 seed 可稳定生成不同样本；
- TEXT_RAG 报告不包含 Parser 时间，Open RAGBench PDF 报告单列 Parser；
- Baseline/Candidate 可验证同版本同样本，结果能够 paired compare；
- `go test ./internal/config ./internal/store ./internal/rag/... ./internal/setup`、evaluator pytest、前端 lint/build 与 Compose config 全部通过；
- 本机部署镜像来自当前 `main` 工作树，健康检查通过且评测、Reranker、公平队列、parser sidecars 保持启用。
