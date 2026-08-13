# RAG 测评

RAG 测评是默认关闭的超级管理员能力，用固定 corpus/cases 对 bkcrab 的真实索引、检索和知识库回答链路进行可复现实验。Python `rag-evaluator` 只运行固定 Ragas 指标，不读取 Milvus、对象存储或生产凭据，也不实现另一套 RAG pipeline。

完整产品边界与实施顺序见：

- `docs/superpowers/specs/2026-08-05-rag-evaluation-design.md`
- `docs/superpowers/plans/2026-08-05-rag-evaluation.md`

## 安全边界

- 测评 API 只接受未处于 `actAs` 状态的 `super_admin` 浏览器会话。现有 `type=admin` API key 不具备访问权。
- evaluator 会接收问题、生成回答与检索命中文本。启用前必须确认 judge/embedding 端点允许处理这类数据。
- evaluator 没有宿主机端口，运行在独立 internal network，使用非 root 用户、只读根文件系统、`tmpfs /tmp`、`cap_drop: ALL` 和 `no-new-privileges`。
- evaluator、judge 或单个指标失败只会使测评不可用或产生 `error`；生产 RAG 继续运行。
- `skipped_missing_input` 与 `error` 不计入评分分母，也不会转换为 0 分。
- Dataset Version、Experiment Profile 和 Policy revision 发布后不可原地编辑。物理 collection key 不暴露给普通用户。

### 数据外发与网络

一次 judge 请求包含 `question`、bkcrab 生成的 `response`、命中的 `context`，并可能包含 reference answer/context；这四类内容都必须按原文外发处理。管理员必须在启用前确认数据分类、地域、供应商留存/训练条款和密钥归属，不能因为 evaluator 与生产链路隔离就默认允许外发。

默认 Compose 只把 evaluator 连接到 `rag-evaluator-internal`，因此不会直接访问互联网。推荐把获批的私有 judge/embedding 服务接入该 internal network；如必须访问外部供应商，应由部署者增加一个受审计、域名 allow-list、TLS 校验和请求日志脱敏的 egress proxy，再让 evaluator 只访问该 proxy。不要为解决出网问题把 evaluator 直接挂到所有业务网络或映射宿主端口。

## Docker 启用

在 `deploy/docker/.env` 中至少配置：

```dotenv
RAG_EVAL_ENABLED=true
RAG_EVALUATOR_API_KEY=<随机长令牌>
RAG_EVALUATOR_LLM_ENDPOINT=https://approved-judge.example/v1
RAG_EVALUATOR_LLM_API_KEY=<judge key>
RAG_EVALUATOR_LLM_MODEL=<judge model>
RAG_EVALUATOR_EMBEDDING_ENDPOINT=https://approved-embedding.example/v1
RAG_EVALUATOR_EMBEDDING_API_KEY=<embedding key>
RAG_EVALUATOR_EMBEDDING_MODEL=<embedding model>
RAG_EVAL_ANSWER_INPUT_COST_USD_PER_MILLION=<answer input price>
RAG_EVAL_ANSWER_OUTPUT_COST_USD_PER_MILLION=<answer output price>
RAG_EVALUATOR_LLM_INPUT_COST_USD_PER_MILLION=<judge input price>
RAG_EVALUATOR_LLM_OUTPUT_COST_USD_PER_MILLION=<judge output price>
RAG_EVALUATOR_EMBEDDING_COST_USD_PER_MILLION=<embedding input price>
```

启动 RAG overlay 时显式启用 profile：

```sh
docker compose \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  --profile rag-evaluation up -d rag-evaluator
```

不启用该 profile 或保持 `RAG_EVAL_ENABLED=false` 时，bkcrab 不依赖 evaluator 启动。不要给 evaluator 增加宿主机端口映射。

在 `/home/csb` 部署时，源码、`.env` 与 Compose 文件均放在该目录下，并从该目录执行 Compose。开发任务只生成镜像/配置和 smoke 结果，不会自动连接服务器、停止现有实例或替换 `/home/csb`；实际切换必须作为单独授权步骤执行。

## Generation 迁移门禁

启动迁移会为存量 KB 合成 immutable legacy ingestion policy 与 active generation mapping，只引用现有 Milvus collection，不复制或重建向量。发布时按以下顺序推进：

1. 保持两个开关关闭，确认 backfill 完成；
2. 设置 `BKCRAB_RAG_GENERATION_SHADOW_READ_ENABLED=true`，观察旧 `activeVersions` 与 generation mapping 的不一致告警；
3. 完成线上数据抽样且所有 KB 一致后，设置 `BKCRAB_RAG_GENERATION_RESOLVER_AUTHORITATIVE=true`；
4. 若出现不一致，resolver 仍回退 legacy collection。未完成抽样前不得删除该 fallback。

两个开关默认均为 `false`。authoritative 模式也会在每次检索时使用同一份 `activeVersions` 快照做校验，校验失败时不会切换物理 collection。

## 配置

主服务使用 `BKCRAB_RAG_EVAL_*` 引导变量；Compose 从较短的 `RAG_EVAL_*` / `RAG_EVALUATOR_*` 变量映射。关键限制包括：

- worker concurrency：默认 1；
- batch size：默认 16；
- 单次运行样例：默认最多 1000；
- 单次运行费用：默认最多 25 USD；
- 请求体：默认最多 4 MiB；
- metric bundle：固定 `rag-core-v1`；
- run/dataset/generation retention：分别默认 90/365/30 天。

凭据不会进入 capabilities DTO、指纹、validation report 或结构化日志。回答模型与 evaluator judge/embedding 是三个明确角色，不能把 evaluator key 放入任务 payload。

## 容量、费用与策略发布

- 创建 run 前按 cases、token 和配置上限给出预算；到达 case/token/cost/duration 任一上限后不再发起新的昂贵调用。
- evaluator CPU、内存、pids、tmpfs、batch 和 context bytes 都有硬上限。扩容时优先保守提高单项限制并观察 p95/错误率，不要同时提高 concurrency、batch 和 context。
- RuntimePolicy publish/rollback 只切换版本化指针，不重建向量；成本主要来自发布前 eval。
- IngestionPolicy publish 只影响此后创建的新 KB，不会静默重建旧 KB。旧 KB 的 opt-in sync 会重新 parse/enrich/embed 并写一个完整新 collection，成本与整库大小相关；切换前查询仍使用旧 generation。
- sync 失败或取消会保留旧 active generation。成功切换后，旧 generation 在 30 天开发基线 rollback window 内保留；回滚是 fenced CAS，窗口到期且无引用后才 GC。回滚不会退还已发生的模型费用。

## 备份与恢复

需要作为一个恢复点共同保护：MySQL（dataset/run/result/policy/active pointer）、MinIO（corpus、manifest、generation artifacts）和 Milvus/etcd（评测与生产 collection）。`rag-evaluator` 容器和 `/tmp` 不含权威状态，无需备份。

一致性备份建议先停止新 run、KB sync 和索引写入，等待 RUNNING 任务结束或取消，然后依次保存 MySQL 逻辑备份、MinIO bucket 副本，以及 `milvus-data`/`milvus-etcd-data` 的同一时间点快照。仅备份其中一个数据面不能保证恢复后的 SQL pointer 与物理 collection 一致。

恢复顺序为 MySQL/MinIO/Milvus-etcd/Milvus 数据，再启动 bkcrab，保持 `RAG_EVAL_ENABLED=false` 验证 `/readyz`、对象读取和生产 RAG；之后启用 evaluator profile，检查 `/healthz` 与 capabilities。若 SQL 指向的 generation 缺失，保持 eval 关闭并从备份恢复，不得创建同名空 collection 伪装修复。

## 指标语义

Ragas 固定指标为 Context Precision、Context Recall、Faithfulness、Response Relevancy 和 Factual Correctness。Hit@K、Recall@K、MRR、nDCG、Citation Precision、Citation Coverage 与 Abstention Accuracy 在 Go 中确定性计算。

确定性指标只比较持久化的 context ID，不以文本相似度替代 ground truth。引用一期只识别回答中的 `[n]`：越界编号计入 precision 的错误分母并记录 reason；coverage 是带至少一个有效 `[n]` 的非空句子数除以非空句子总数。无 reference context ID 时检索指标标记为 `skipped_missing_input`，不伪造 0 分。

聚合输出 count、scored、skipped、error、mean、median/p50 与 p95。Baseline/Candidate 只对相同 case 的有效成对分数计算 delta，并明确列出缺失样例。

## Dataset 一期格式与生命周期

一期 import adapter 固定为 `canonical-json`，不接受 ZIP 或其它归档格式。manifest 使用严格、有界 JSON 解码；corpus 原件按 manifest 声明的字节数流式写入 staging，同时计算 SHA-256，不整文件读入内存。文件名必须是单一安全文件名，同一版本内大小写不敏感地禁止重名。

版本按 `DRAFT → VALIDATING → READY` 发布，校验或对象提交失败进入 `FAILED`。只有 READY 版本可供后续 run 使用，READY 后 case/document 不可覆写；修改数据必须创建更高版本。DRAFT/FAILED 以及 READY 后遗留的 staging 可按 TTL 清理。删除 dataset 先 tombstone；仍被 run 引用时禁止物理清理，retention 到期且引用为零后才删除 SQL 与版本对象前缀。

## Eval shadow generation

Full Pipeline 使用显式的 `owner/run/dataset-version/generation` target 调用生产共用的 parser、splitter、enrichment、embedding 和 vector 组件，不创建或伪造生产 KB。评测 collection 使用 `eval_…` 物理前缀，对象只写入 `rag-eval/generations/<generation-id>/`；删除也只能经过 eval 专用 drop 边界。

generation fingerprint 覆盖 corpus version、原件文件契约与 SHA-256、完整 ingestion profile、parser/tokenizer/splitter/artifact/vector/index contract。只有精确 fingerprint 的 READY generation 可复用；parse-only artifact 可在解析契约相同时跨 chunk/embedding generation 复用。BUILDING 由 SQL lease/fence 单飞，run 引用与 refcount 在同一事务绑定；过期且无活动引用、无保留 run 引用时，先 fenced 标记 DELETING，再清理 eval collection 和对象前缀。

`minScore` 在 UI 和文档中称为“最低 reranker 分数”；它不是概率。reranker 失败时的 RRF fallback 必须保留在 trace 中。

## 验证

```powershell
$env:GOTMPDIR = 'D:\gotmp'
go test ./internal/config ./internal/store ./internal/rag/... ./internal/setup

Push-Location services/rag-evaluator
uv sync --frozen --extra dev --python 3.12
uv run pytest
uv run ruff check .
Pop-Location

pnpm --dir web lint
pnpm --dir web build

docker compose -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml config
```

离线 Docker smoke（不会调用真实 judge）还应执行：

```powershell
docker compose --env-file deploy/docker/.env.example `
  -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml config --quiet
docker compose --env-file deploy/docker/.env.example `
  -f deploy/docker/docker-compose.yml -f deploy/docker/docker-compose.rag.yml `
  --profile rag-evaluation config --quiet
docker build -t bkcrab/bkcrab:phase-h .
docker build -t bkcrab/rag-evaluator:phase-h services/rag-evaluator
```

启动后先保持 `RAG_EVAL_ENABLED=false`，确认 `bkcrab /readyz` 正常且 evaluator profile 未启动。随后启用 profile，使用 `docker compose exec bkcrab wget -qO- http://rag-evaluator:8080/healthz` 验证内部健康；在已登录的 super-admin UI 检查 capabilities。再停止 `rag-evaluator`，确认 `/readyz` 和生产 RAG 仍正常、capabilities 变为 unavailable。健康检查不会调用 judge；真实 provider smoke 留给受控发布闸门。

任何涉及真实 judge、Embedding 或大 KB shadow rebuild 的 smoke 都应使用受控数据集和明确费用预算。部署到 `/home/csb` 是独立授权步骤；开发与测试不会自动连接或修改服务器。

## 回滚

- RuntimePolicy 回滚只切换 immutable active pointer，新请求生效，在途请求继续使用已捕获快照。
- IngestionPolicy 发布只自动用于新 KB；旧 KB 保持 pinned policy，直到所有者主动同步。
- KB 同步全程读取旧 active generation；新 generation 完整校验后才 CAS 切换。
- generation 仍在 rollback window 且完整时才能回滚；GC 必须检查 active/ref/rollback 引用。
