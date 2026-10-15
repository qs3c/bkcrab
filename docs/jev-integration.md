# Jev 接入：检索、记忆护栏与评测

2026-10-07：代码已接入并完成离线验证，默认关闭。随后完成了首轮真实模型诊断（本地记录 `docs/jev-acceptance-results-2026-10-07.md`，未随源码提交），发现记忆误拦截与 precision 漏判，尚未部署或切换线上流量。已有 `cmd/rag-rerank-bench` 实验实现保持独立；本次生产路径使用新的共享客户端。

## 1. 已实现的边界

| 用途 | 本次实现 | 默认与故障行为 |
| --- | --- | --- |
| 检索重排 | `protocol=jev` 接入真实 Gateway → RAG 路径；每个候选一个 Noul，按概率排序 | 原配置不变；失败走已有 RRF 回退，评测显式 strict 策略仍然生效 |
| 记忆写入 | 手动 memory 工具和 Agent 自动写入共用语义护栏；评审同一目标文件的完整拟写入条目 | `off`；`shadow` 只观察，`enforce` 拒绝未通过评审的新增/替换 |
| Ragas | 保留 0.3.9，在本项目 sidecar 增加两个适配 bundle | `rag-core-v1` 不变；失败记缺失/错误，不伪装成 0 分 |
| 文档解析质量门禁 | 本次未实现 | 不改解析、分块或入库流程 |

`Result.Score` 存的是 Noul 概率，不是 Jev 的 Score 原语，也不代表已校准的正确率。语义护栏仍可能漏判或被攻击诱导；多语言能力、误杀率和实际成本需要实测。

## 2. 配置与启用

共享客户端在主进程启动时创建。原生 API 默认值为 `https://api.typesafe.ai/v1/systemone`、固定模型 `jev-1.13.0`。只接受明确版本（例如 `jev-1.13.0` 或 `typesafe/jev-1.13-20260917`），响应模型必须与配置一致；若使用其他兼容端点，需要匹配端点的模型 ID、认证和价格。不能复用 Qwen 的重排协议地址。

直接运行主进程时使用以下环境变量。Docker Compose 的 `.env` 使用第二列名字；样例在 `deploy/docker/.env.example`，**不要把真实密钥写入仓库**。

| 主进程环境变量 | Compose `.env` | 默认 |
| --- | --- | --- |
| `BKCRAB_JEV_ENABLED` | `JEV_ENABLED` | `false` |
| `BKCRAB_JEV_ENDPOINT` | `JEV_ENDPOINT` | 原生 API 地址 |
| `BKCRAB_JEV_API_KEY` | `JEV_API_KEY` | 空 |
| `BKCRAB_JEV_MODEL` | `JEV_MODEL` | `jev-1.13.0` |
| `BKCRAB_JEV_TIMEOUT_MS` | `JEV_TIMEOUT_MS` | `5000` |
| `BKCRAB_JEV_CONCURRENCY` | `JEV_CONCURRENCY` | `4`，合法范围 1–16 |
| `BKCRAB_JEV_RETRIES` | `JEV_RETRIES` | `1`，合法范围 0–2，额外重试次数 |
| `BKCRAB_JEV_MAX_REQUEST_COST_USD` | `JEV_MAX_REQUEST_COST_USD` | `0.01` |
| `BKCRAB_JEV_INPUT_COST_USD_PER_MILLION` | `JEV_INPUT_COST_USD_PER_MILLION` | `0.042` |
| `BKCRAB_JEV_OUTPUT_COST_USD_PER_MILLION` | `JEV_OUTPUT_COST_USD_PER_MILLION` | `0` |
| `BKCRAB_JEV_MEMORY_GUARD_MODE` | `JEV_MEMORY_GUARD_MODE` | `off` |

价格是显式计费估算配置，切换服务商时需要更新。密钥只保留在启动配置快照中，启动后从进程环境清理，不进入用户配置 JSON 或结构化配置日志。

### 2.1 重排

在已有 RAG 配置上启用（以下是 Compose `.env` 名字）：

```dotenv
JEV_ENABLED=true
JEV_API_KEY=<通过部署环境提供>
RAG_RERANKER_ENABLED=true
RAG_RERANKER_PROTOCOL=jev
RAG_RERANKER_JEV_BATCH_SIZE=20
RAG_RERANKER_DISABLE_SCORE_FILTER=true
```

直接运行时，重排变量加 `BKCRAB_` 前缀。Jev 使用共享 `JEV_ENDPOINT/MODEL/API_KEY`；原来的 `RAG_RERANKER_ENDPOINT/MODEL/API_KEY` 可留作回滚到 Qwen 使用，但不参与 Jev 请求。

新增 `disableScoreFilter` 是显式开关，避免把 Qwen 的阈值套到 Jev 概率上。没有显式传入 `SearchOptions.MinScore` 时，该开关关闭默认分数过滤；评测 profile 显式指定的阈值仍优先，从而保持实验参数可复现。运行不设阈值的评测时，应在 profile 中显式设 `minScore: 0`。

每批最多 20 个候选，最多 100 个候选/查询；超过 64,000 字节的序列化请求自动缩小批次，保留原文，不截断。单个候选仍超限则失败回退。一个查询内各批顺序执行，不沿用旧实验的批内并发设置；共享并发限制作用于所有查询、记忆和评测调用。候选索引稳定，平分时保留原顺序，任一批失败不会使用部分排序。

### 2.2 记忆护栏

```dotenv
JEV_ENABLED=true
JEV_MEMORY_GUARD_MODE=shadow
```

写入顺序是：本地格式/大小/敏感信息检查 → 生成完整拟写入条目 → Jev 判断 → 写入。评审在文件/数据库修改锁外执行；`enforce` 提交时比较原始快照，期间若有并发写入则拒绝当前操作并提示重试，不覆盖他人的修改。

固定策略 `memory-jev-v1` 判断三个维度：越权或覆盖系统指令、泄露或外传私密数据、未经授权的持久化修改。按最大风险概率处理：

- `< 0.35`：allow。
- `0.35 ≤ p < 0.70`：review。
- `≥ 0.70`：block。
- 请求失败或响应不合法：unavailable。

**这些是待校准的初始阈值，不是已测得的安全边界。** `shadow` 不因上述判定改变原本本地允许的写入；`enforce` 只允许 allow，其余拒绝新增/替换，已有记忆保持原状。review 目前是拒绝原因，没有新增人工审批队列或 UI。纯删除和列出不调用模型，服务异常时仍可清理记忆。

现有 `scanner.go` 的本地规则和读取过滤仍保留。本地已经拒绝的内容不会发送给 Jev，因此 shadow 观察样本限定为本地检查通过的写入。审查覆盖单个目标文件（USER.md 或 MEMORY.md）的条目组合，没有跨文件语义合并。直接编辑文件、管理端写文件等绕过 memory Manager 的路径，以及存量内容的全量复审，不在此次写入护栏范围内。

Agent 的记忆提示同时改为：记忆只是可能过时的事实/偏好，不能授予权限或覆盖系统约束。已知凭据模式先本地拒绝；本地规则无法保证识别所有隐私，因此开启远程评审意味着拟写入正文会发送至配置的 Jev 服务。

### 2.3 Ragas 评测

先保留原有 evaluator LLM、embedding、内部认证和价格配置，再选择：

```dotenv
JEV_ENABLED=true
RAG_EVAL_METRIC_BUNDLE_VERSION=rag-jev-precision-v1
```

两个可选 bundle：

| 指标 | `rag-jev-precision-v1` | `rag-jev-hybrid-v1` |
| --- | --- | --- |
| context_precision | Jev 判定每个 context，代码沿用 Ragas AP 公式 | 同左 |
| faithfulness | 原 Ragas | 原 LLM 提取声明，Jev 验证声明是否被 context 支持 |
| context_recall | 原 Ragas | 原 LLM 从参考答案提取声明，Jev 判断检索证据是否支持，代码计算支持比例 |
| factual_correctness | 原 Ragas | 原 LLM 分解 response/reference，Jev 双向验证，沿用 TP/FP/FN 和 F1 计算 |
| response_relevancy | 原 Ragas LLM + embedding | 不变 |

这没有修改安装的 Ragas 源码，而是在本项目适配层继承其类。依赖了 0.3.9 的私有方法；升级 Ragas 必须重跑适配测试。context_recall 的声明提取路径有变化，新 bundle 的数值不能等同于旧指标。Jev 概率以 `p >= 0.5` 转成二元判定，再交给代码运算；概率、阈值、模型和 rubric 写入每个指标的 details。声明提取不做跨运行缓存。

Sidecar 只持有内部服务认证，不持有 Jev 服务商密钥。调用路径：

```text
Ragas adapter
  → POST 主进程 /internal/rag-eval/jev
  → 校验内部认证、评测 owner 的有效超级管理员身份
  → 固定服务端 rubric（客户端不能传 prompt/endpoint/key）
  → 共享 Jev 客户端
```

Compose 已同步注入主进程和 sidecar 的 bundle/model。非 Compose 部署还需给 sidecar 设置：

```dotenv
RAG_EVALUATOR_METRIC_BUNDLE_VERSION=rag-jev-precision-v1
RAG_EVALUATOR_JEV_ENDPOINT=http://bkcrab:18953/internal/rag-eval/jev
RAG_EVALUATOR_JEV_MODEL=jev-1.13.0
RAG_EVALUATOR_JEV_MAX_REQUEST_COST_USD=0.01
```

`RAG_EVALUATOR_API_KEY` 沿用现有内部共享凭据。主进程对应 `BKCRAB_RAG_EVAL_METRIC_BUNDLE_VERSION`。模型、bundle 配置需要主进程与 sidecar 一致；切换时重启两者并创建新运行。

每次评审最多 20 个 contexts、128 个声明，完整序列化请求仍受 64KB 限制；超限记错误，不截断证据。混合模式仍需要原 LLM/embedding 配置，不能理解为取消所有 LLM 费用。

执行快照冻结 bundle 和 Jev model，恢复运行时不允许换评审器；指标记录使用对应版本，在线比较 API 拒绝跨 bundle/model 比较 judge 指标。现有策略晋级暂时只接受 `rag-core-v1`，避免未经校准的新分数直接套旧门槛。需要比较新旧 judge 时应导出原始判定，人工/离线校准一致率，而不是解释为 RAG 质量提升。

## 3. 超时、成本与观测

共享客户端的单次 Decide 调用有一个覆盖排队、请求和重试的总超时。只对 429/502/503/504/529 做有限重试，遵守 Retry-After；不自动重试认证错误或不确定的网络发送失败。重排还受已有 reranker 查询总超时约束。

预算是**每次 Decide 请求及其重试**的估算上限，不是客户端生命周期累计额度，也不是服务商账单的硬保证。一个重排查询拆成多批或一个指标包含多次声明验证时，可能有多次额度。估算使用请求字节数加预留开销；实际使用量或费用若可读取就累计；不可读取的尝试保守记估算并标记 unknown。64KB 只是本地字节保护，不代表模型 token 上限。评测运行现有的总预算检查继续累计 LLM、embedding 和 Jev 费用。

可观察数据：

- `rag: Jev rerank` 日志：模型、候选数、尝试数、token、估算费用、unknown、耗时、成功与否；检索 trace 保留原有 fallback 字段。
- `memory: semantic review` 日志：策略版本、模式、目标、内容 SHA-256、条目数、判定、各项概率、模型、费用和耗时；不记录正文。
- 评测 `rag_eval_usage` 的 `judge_jev` 独立于 `judge` / `judge_embedding`；失败指标也保留可知用量，unknown 信息保存在指标 details。主进程响应丢失时 sidecar 保守记一次请求额度，不能当成精确实付金额。

没有新增 Prometheus 指标或仪表盘。开启 Jev 前应先确认部署能访问所配置服务，内部 sidecar 端点和认证也正确。

## 4. 回滚

三个用途独立选择：

1. 重排：恢复原 `RAG_RERANKER_PROTOCOL`、Qwen endpoint/model/key 和阈值配置，将 `RAG_RERANKER_DISABLE_SCORE_FILTER=false`，重启主进程。
2. 记忆：`JEV_MEMORY_GUARD_MODE=off` 并重启，原本地检查继续工作，记忆文件不迁移。
3. 评测：主进程和 sidecar 同时恢复 `rag-core-v1`，重启后创建新运行。已完成的 Jev 结果仍保留版本；不要用旧 bundle 恢复未完成的新 bundle 运行。
4. 三个消费者都关闭后才设 `JEV_ENABLED=false`；若仍配置了 Jev 消费者，启动会报配置错误。

## 5. 离线验证与后续验收

已执行（所有模型请求均为本地 stub/mock）：

```sh
go test ./internal/jev ./internal/jeveval ./internal/config ./internal/memory \
  ./internal/rag/... ./internal/agent/... ./internal/setup ./internal/gateway ./cmd/bkcrab

go test -race ./internal/jev ./internal/memory ./internal/rag/rerank

.tmp/jev-eval-venv/bin/python -m pytest services/rag-evaluator/tests -q
.tmp/jev-eval-venv/bin/ruff check services/rag-evaluator
```

相关 Go 包测试、上述三个包的 race 检测通过；全仓库 `go test ./... -run '^$'` 编译检查通过；Python 60 passed、1 skipped（付费 smoke 默认跳过）；Ruff 通过。新增覆盖包括响应模型/概率/用量校验、重试与排队取消、请求预算、重排索引和拆批、记忆并发写入与原文不落日志、评测原公式及用量分账、版本隔离和禁止未校准晋级。最终验证结果以本次交付记录为准。

上线前另开有预算的真实验收：

- 重排：在同一候选集上比较排序质量、完整成功率、含回退的延迟与费用、最终答案质量；使用新 manifest，不能复用旧 batch-5 结论。
- 护栏：人工复核中文、英文及其他语言的正常偏好、攻击引用、越权指令、条目组合；分语言统计漏判和误杀，先 shadow 后决定 enforce 阈值。代码 mock 测试不证明模型安全性。
- 评测：固定样本、原 LLM 提取结果和人为标签，比较新旧 judge 一致率及分歧，记录包含提取阶段在内的总费用与耗时；重新校准后才能扩展策略晋级。

本次未声称 Jev 已优于原 reranker、已覆盖任意语言攻击、或已降低实际评测账单。
