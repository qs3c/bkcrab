# RAG 测评 Phase H 发布闸门记录

记录日期：2026-08-11

## 结论

本地确定性发布闸门通过，RAG 测评必须继续保持 `RAG_EVAL_ENABLED=false`。真实 evaluator provider 与真实 Milvus 闸门均已实现为显式 opt-in 测试，但本机没有获得相应的专用测试配置，因此本次结果是 `SKIP/NOT RUN`，不能解释为真实集成已通过。未执行服务器部署或生产放量。

## 可复现标识

| 项目 | 记录值 |
|---|---|
| Golden dataset | `testdata/rag-eval/e2e_golden.json` |
| Dataset SHA-256 | `4e19db77ff7911d019c0693b4df9f54a48f41f6ef974e297b63af85b5a6aab61` |
| 确定性 judge/model | `goldenBatchScorer` / `golden`，固定 baseline `0.7`、candidate `0.9` |
| Ragas | `0.3.9` |
| Metric bundle | `rag-core-v1` |
| 主镜像 | `bkcrab/bkcrab@sha256:6129bdc9f9763e2fd0a77cd19552907526402e7ddc94ef077e8b10111d33e9b4` |
| Evaluator 镜像 | `bkcrab/rag-evaluator@sha256:55431577163e2912eb066bfca20444dda3faf599c52e01dfbac1b86ad512bed7` |
| 真实 LLM/Embedding 模型 | `NOT RUN`；未向测试进程提供 evaluator 专用 endpoint/key/model |
| 真实 Milvus | `NOT RUN`；未设置 `RAG_TEST_MILVUS_ADDR` |

镜像 digest 和 Ragas 版本均从本地构建镜像重新读取，不从 tag 或锁文件推断。

## 验证结果

| 闸门 | 结果 | 证据 |
|---|---|---|
| Import → Full baseline/candidate → score → compare | PASS | Go/Python/Web 共用同一 golden；paired delta `0.2`，scored denominator `1` |
| Legacy generation migration | PASS | 空 KB、多文档、历史版本、pending、deleting、8 路并发和重复 backfill |
| Runtime/Ingestion promotion、旧 KB drift、原子切换与 rollback | PASS | 全量 Go 回归覆盖 CAS、fence、旧 generation 可见性和回滚 |
| parser/embed/Milvus/reranker/answer/judge/heartbeat 故障 | PASS（fake/local） | pipeline、sidecar、embed、search、runner 与 store 故障/lease 测试随 `go test ./...` 通过 |
| restart/cancel/budget/cleanup | PASS | runner resume、cancel/budget、cleanup interruption/refcount/GC 测试通过 |
| 真实 Ragas/provider | SKIP | 只有显式 `RAG_EVALUATOR_REAL_SMOKE=1` 且六项 evaluator provider 配置齐全时才会产生计费调用 |
| 真实 Milvus 4/8 dims、pointer、rollback | SKIP | 门控测试已编译；只有提供 `RAG_TEST_MILVUS_ADDR` 才创建和清理两套物理 collection |

完整命令结果：

- `go test ./...`：PASS；所有 Go packages 通过。
- Web `test`：PASS，48/48。
- Web `lint`：PASS，0 errors；保留 20 个与本次 Phase H 无关的既有 warnings。
- Web `build`：PASS；Next.js production build 和 46 个静态页面生成完成。
- `uv run pytest`：PASS，26 passed、1 gated skip。
- `uv run ruff check .`：PASS。
- 默认 Compose 与 `--profile rag-eval`：使用 `deploy/docker/.env.example` 插值后均 PASS。

仓库要求 pnpm `10.15.0`，但本机 Corepack 缓存没有该版本且 npm registry 连接超时，故 Web 三个脚本由现有 npm 安装从同一 `web/package.json` 执行。此差异不改变脚本内容；在联网 CI/发布机仍应使用锁定的 pnpm 命令复验。

## 自审

- 安全：API 仍由未 actAs 的 super-admin 权限保护；请求不能注入 metric、prompt、endpoint 或物理 collection key；错误与 telemetry 不记录 question/context/response 或凭据。
- 隐私：只有显式启用 evaluator 并完成允许的数据边界配置后，question、response、contexts 才能发送至 judge；sidecar 不读取 Milvus、对象存储或生产 provider 凭据。
- 成本：worker 并发、case/token/USD/duration 上限和 durable usage ledger 均生效；真实 smoke 需要单独 opt-in，避免因环境中偶然存在通用 API key 而计费。
- 回滚：RuntimePolicy 使用版本化 CAS；IngestionPolicy 新建 generation，在校验完成前始终读旧 generation；旧 collection 在 rollback window 内保留并可切回。
- 孤儿资源：run/dataset/generation 使用 tombstone、refcount、fence 和 retention GC；中断后 cleanup 可重入；只清理 SQL 明确标记且 fence 匹配的物理对象。

## 放量前仍需执行

在受控、非敏感数据和明确费用预算下运行真实 provider smoke；在目标 Milvus 上运行 4/8 维 generation 切换/回滚测试；确认模型名、endpoint、镜像 digest 和本次记录一致。两项通过前不得开启 feature flag，也不得向普通用户显示 KB 同步入口。
