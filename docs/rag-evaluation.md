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
```

启动 RAG overlay 时显式启用 profile：

```sh
docker compose \
  -f deploy/docker/docker-compose.yml \
  -f deploy/docker/docker-compose.rag.yml \
  --profile rag-evaluation up -d rag-evaluator
```

不启用该 profile 或保持 `RAG_EVAL_ENABLED=false` 时，bkcrab 不依赖 evaluator 启动。不要给 evaluator 增加宿主机端口映射。

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

## 指标语义

Ragas 固定指标为 Context Precision、Context Recall、Faithfulness、Response Relevancy 和 Factual Correctness。Hit@K、Recall@K、MRR、nDCG、Citation Precision、Citation Coverage 与 Abstention Accuracy 在 Go 中确定性计算。

确定性指标只比较持久化的 context ID，不以文本相似度替代 ground truth。引用一期只识别回答中的 `[n]`：越界编号计入 precision 的错误分母并记录 reason；coverage 是带至少一个有效 `[n]` 的非空句子数除以非空句子总数。无 reference context ID 时检索指标标记为 `skipped_missing_input`，不伪造 0 分。

聚合输出 count、scored、skipped、error、mean、median/p50 与 p95。Baseline/Candidate 只对相同 case 的有效成对分数计算 delta，并明确列出缺失样例。

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

任何涉及真实 judge、Embedding 或大 KB shadow rebuild 的 smoke 都应使用受控数据集和明确费用预算。部署到 `/home/csb` 是独立授权步骤；开发与测试不会自动连接或修改服务器。

## 回滚

- RuntimePolicy 回滚只切换 immutable active pointer，新请求生效，在途请求继续使用已捕获快照。
- IngestionPolicy 发布只自动用于新 KB；旧 KB 保持 pinned policy，直到所有者主动同步。
- KB 同步全程读取旧 active generation；新 generation 完整校验后才 CAS 切换。
- generation 仍在 rollback window 且完整时才能回滚；GC 必须检查 active/ref/rollback 引用。
