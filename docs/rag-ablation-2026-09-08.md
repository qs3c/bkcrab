# RAG Planner / Reranker 消融实验：准备与基线审计

日期：2026-09-08。状态：四组 Profile 已创建；第一组已实际运行，但因上游模型错误取消。尚无有效消融质量结果，不能据此决定关闭任一步骤。

后续 provider 排查更新：已用相同账号/端点做只改变请求头的真实对照。DeepSeek/Kimi 加入 OpenCode 会话头后返回 HTTP 200，证明其 400 为 BkCrab 接入适配缺失；GPT 5.6 Luna 仍为 500。无需先认定必须更换供应商。详细证据和修复范围见 [provider 排查报告](provider-opencode-go-2026-09-08.md)。运行容器尚未修复，消融仍未恢复。

## 要回答的问题与实验组

在同一批问题、同一份索引和相同回答设置下，测量每个昂贵步骤带来的质量增益及延迟代价。

| 组 | Rewrite | HyDE | Reranker | 最低 reranker 分数 | 用途 |
| --- | --- | --- | --- | --- | --- |
| A 全开复验 | 开 | 开 | 开 | 0.5 | 当日对照，检查历史结果漂移 |
| B 关闭 Planner | 关 | 关 | 开 | 0.5 | 与 A 配对，衡量 Planner 整体收益 |
| C 关闭 Reranker | 开 | 开 | 关 | 不生效 | 与 A 配对，衡量重排与过滤整体收益 |
| D 两者关闭 | 关 | 关 | 关 | 不生效 | 最低成本方案；检查两步是否互相依赖 |
| E（后续诊断）只关闭过滤 | 开 | 开 | 开 | 0 | 区分排序收益与阈值过滤收益，尚未创建 |

所有组保持候选 `candidateTopK=20`、最终 `topN=5`、相同文本语料/分块/embedding/稀疏索引、回答温度 0.2、回答长度 4096 和 `rag-answer-v1`。C、D 直接取 RRF 前 5 项；不能把 0.5 阈值套在 RRF 分数上。B 必须同时关闭两个开关，否则仍会调用合并的 Planner。

新增四组的 `rerankerTimeoutMs` 统一记为当前运行配置 180000。历史 50 题 Profile 写的是 5000，但运行曾成功耗时 60 秒以上；当前评测调用复用全局 reranker 客户端，没有按 Profile 的该字段重新构造客户端。因此该字段不能作为已执行的超时证明。不要用这个历史元数据差异解释质量变化。

## 已选定的主实验

- 数据：Open RAGBench（Vectara），arxiv / TEXT_RAG，50 题、200 篇文档，seed 42。
- Dataset version：`rdv_c4fa198f-af95-4798-8d15-bc28efd81762`。
- 已有全开基线：`rer_4a417c4a398f1f77eb4f2108fca0a8c3`，2026-08-27，50 题回答完成、无 case 错误。
- 固定索引：`reg_13014a14cc8c4bca8989692e69ca10d7`，12829 chunks，READY。
- 原回答模型：`opencode-go/deepseek-v4-flash`。
- 模式：保持 `FULL_PIPELINE`，利用完全兼容 generation 的自动复用，以便平台与旧基线配对。每次启动都核对 `generationReused=true` 且 generation ID 相同。首个新运行已验证这一点，索引准备仅 40 ms。

原则上四组都从同一 READY generation 执行 `ONLINE_ONLY` 也可以，但平台要求配对运行的 mode 相同，旧 FULL_PIPELINE 基线无法直接与 ONLINE_ONLY 候选配对。此次选择保留旧模式并核对真实复用。

四组 Profile 已保存到本机测评平台，未发布生产策略：

| 名称 | Profile ID |
| --- | --- |
| 消融20260908-A-全开复验 | `rep_46f71f9dc4a15915015b64ccb88f4a69` |
| 消融20260908-B-关闭Planner | `rep_032488bbcb89b4c02c9013952cfc9221` |
| 消融20260908-C-关闭Reranker | `rep_0a049edf2f8c0a9f1c00e1ed8038f7e1` |
| 消融20260908-D-两者关闭 | `rep_44ea138eaefe3ff9d153fb38516a79ec` |

## 历史基线实测

直接从既有 case trace 和 metric records 聚合；延迟单位秒，p50/p95 使用线性插值。只描述全开表现，尚不能证明组件的因果收益。

| 阶段 | 平均 | p50 | p95 |
| --- | ---: | ---: | ---: |
| Planner | 6.91 | 6.63 | 13.05 |
| Reranker | 48.42 | 45.83 | 83.83 |
| 检索总耗时 | 55.89 | 53.17 | 91.11 |

检索加回答平均 66.32 秒，不含 judge 评分。Planner 回退 1/50，reranker 成功 50/50，返回空 context 0/50。不能将各阶段 p95 相加，或直接从总 p95 中扣去阶段 p95。

| 质量指标 | 均值 | 有效分母 |
| --- | ---: | ---: |
| 文档 Hit@K | 1.0000 | 50 |
| Context Precision | 0.9205 | 50 |
| Context Recall | 0.9600 | 50 |
| Faithfulness | 0.9173 | 50 |
| Response Relevancy | 0.8834 | 50 |
| Factual Correctness | 0.2565 | 49；另 1 题 metric timeout |

文档级四项检索指标都已达到 1.0，可能掩盖同文档内片段选取质量的差异。回答事实正确性明显更低，应逐题检查原回答、参考答案与 judge 理由，不能把忠实度等同于答案正确率，也不能仅凭命中文档判定检索充分。

阈值 0.5 从保存的重排 Top5 共 250 项中移除 18 项，影响 7/50 题。被移除项中 10 项来自非 gold 文档、8 项来自 gold 文档。这里的 relevant 标记仅由 document ID 产生；来自 gold 文档的片段不一定包含正确证据，不能把 8 项直接称为误删。全组无应拒答题，因此无法评估阈值对真实无答案问题的收益。

## 为什么还需要多轮集

主实验 50 题均无 history、均非应拒答题，无法充分测量 Planner 的指代消解能力。应另用现有 MultiDoc2Dial v5（20 题、488 文档，其中 18 题有 history）复制它自己的基线配置再做同样四组，不与 Open RAGBench 合并计算均值。

该集旧基线 `rer_927ac631712d12a079a02f65ed43aa42` 的 Planner 回退达 8/20；平均 Planner 14.37 秒、reranker 39.69 秒、总检索 54.65 秒。报告应同时给出全样本实际服务表现和回退分布；仅分析 Planner 成功子集会产生选择偏差，最多作为诊断。

该 MultiDoc2Dial 运行复用了另一个 dataset version 所属 generation，当前 ONLINE_ONLY 创建校验不允许这种组合；保持 FULL_PIPELINE 并核对复用更合适。

还有一条显示名为 TAT-QA、30 题/98 文档的旧运行 `rer_132c60b01a7a1466320e17a5c1ac9e6d`，其 immutable source_config 实际为 Vectara Open RAGBench，不能作为表格问答测评证据。

## 如何判断值得保留

1. 对同 case ID 的有效配对结果比较 Context Precision、Context Recall、Factual Correctness；Faithfulness、Relevancy、引用与拒答作为辅助指标。没有 gold chunk IDs，不计算伪造的 chunk Recall@K。
2. 每个指标报告 A/B/C/D 均值、相对 A 的绝对差、配对分母、改善/持平/退化题数和配对 bootstrap 95% 区间；多轮数据集按对话聚类抽样，避免同对话题被当成独立样本。50/20 题是探索性实验，区间宽时增加固定样本或独立复验，不能以“不显著”断言没有收益。
3. 报告单题检索与检索加回答的 mean/p50/p95、Planner/reranker 阶段耗时、空结果率、case 错误率、judge 错误率和回退率。失败不计为 0 分，必须单列，否则容易得到虚假的提速。
4. 分别看 A−B（reranker 开时 Planner 收益）与 C−D（reranker 关时 Planner 收益）；A−C 与 B−D 同理。不假设两步收益可以直接相加。
5. 同时复查退化最明显的题，确认原因是漏召回、排序、过滤、回答生成还是 judge。可把每增加 0.01 的主质量分数所需延迟作为描述，但不预设适合业务的可接受降分幅度。
6. 一次只启动一个组，保留当前 case concurrency=2；当前 worker concurrency=4，若同时提交四组会争抢本机 reranker/embedding，延迟不再可比。要测单请求服务延迟，应另固定 case concurrency=1 做小规模时延复验，不把它与当前并发条件混在一起。

费用记录目前包含回答/评分计量，不完整覆盖 Planner 和本机模型计算；配置价格也不等于真实账单。本次应优先比较耗时与 token，并把费用覆盖范围说明清楚。当前每 run 上限为 200 万 token / 6 小时，费用中断关闭；这不是自动批准无限量重试。

## 本次实际执行与阻塞

C 组运行：`rer_2c568a81c935d425ef0fcc908edc64f9`。

- 已确认复用同一个索引，关闭 reranker 的开关实际生效。
- 第一批样例的 Planner 和回答请求均失败，错误为 `HTTP 400 MissingSessionID`，上游明确要求 `x-opencode-session`。
- 在发现连续失败后取消；最终 `CANCELLED`，completed=10、failed=10、scored=0。
- 没有把失败运行纳入质量或延迟消融结论；A/B/D 未启动。

[OpenCode Go 官方接入要求](https://opencode.ai/docs/go/#where-can-i-use-it)说明客户端应发送自己的 User-Agent，并为每个会话发送稳定的 `x-opencode-session`。当前 OpenAI-compatible provider 请求只设置 Content-Type 和 Authorization。恢复模型接入后需要先做小规模调用验证，再重试 C，并执行 A/B/D。若换模型，四组必须统一重跑，历史全开仅作背景参考。

Planner 与内部 judge 解析当前 owner 的默认模型，并非完全由 immutable Profile 固定；实验期间应保持默认模型/供应端不变并记录实际绑定。只更换 Profile.answerModel 不能解决 Planner 与 judge 仍使用失败默认模型的问题。

用户随后选择改用其他已配置模型。检查模型页发现只有一个供应商 `opencode-go`；已通过平台的“测试连接”实际验证全部三个模型：

| 已配置模型 | 2026-09-08 连接测试 |
| --- | --- |
| `opencode-go/kimi-k3` | HTTP 400，MissingSessionID |
| `opencode-go/gpt-5.6-luna` | HTTP 500，Internal server error |
| `opencode-go/deepseek-v4-flash` | HTTP 400，MissingSessionID |

修复前不存在通过连接测试的已配置替代模型。后续请求头对照已证明原 DeepSeek 和 Kimi 可通过补齐会话头恢复 HTTP 200，详见 [provider 排查与修复记录](provider-opencode-go-2026-09-08.md)。用户已授权修复、提交推送、重新部署并继续测试；因此保留原默认模型和四组 Profile，部署后先验证真实调用，再恢复消融。Planner/judge 仍依赖 owner 默认模型，实验期间保持不变。

## 修复后恢复执行

provider 修复 `73c45fd` 已提交推送并部署到本机主服务，健康检查及 DeepSeek 连接、Planner、回答、judge 工具调用复验通过。详见 [部署与真实调用记录](provider-opencode-go-2026-09-08.md)。

正式新 A 组：`rer_6079b384a5999588eca5994615d413f8`，2026-09-08 16:10:15 UTC 启动。保留原 50 题、12 项指标、`FULL_PIPELINE` 和 DeepSeek 模型，绑定历史全开基线。确认 `generationReused=true`、generation `reg_13014a14cc8c4bca8989692e69ca10d7`、准备耗时 19 ms。

执行顺序 A → B → C → D，每次仅运行一组，维持 case concurrency=2。A 同时用于检查历史基线到当前模型服务的漂移，B/C/D 以新 A 为主要对照。若遇系统性 provider 错误、索引复用不符或预算耗尽，应暂停后续组并记录原因，不能把失败当作提速。

### 2026-09-09 04:42 UTC 跟进

A 已于 2026-09-08 17:10:08 UTC 结束（本机时间 9 月 8 日 12:10:08），耗时约 59 分 53 秒。服务显示 `SUCCEEDED`，但只表示运行执行完毕，不表示每题或每项评分成功。上轮未建立后台自动接续，B/D 尚未创建运行，C 只有修复前取消的失败运行；没有四组结果。

- 回答：49/50 成功；1 题 `empty_response`，回答输出计量 4094 tokens、预算 4096。新运行未出现 `MissingSessionID`。
- Planner：11/50 回退；reranker：50/50 成功。
- 全部 50 题的 Planner 平均 7.82 秒，reranker 平均 48.12 秒，检索平均 56.41 秒。尚无新消融组，不能由此推断质量收益。

| judge 指标 | 有效评分 | 错误 | 有效样本均值 |
| --- | ---: | ---: | ---: |
| Context Precision | 47 | 2 | 0.8944 |
| Context Recall | 47 | 2 | 0.9149 |
| Faithfulness | 19 | 30 | 0.8717 |
| Response Relevancy | 49 | 0 | 0.8664 |
| Factual Correctness | 23 | 26 | 0.2965 |

评分分母缺失严重，不能将这些均值直接用于质量结论。60 条评分错误中，58 条为结构化工具调用缺失，2 条为 JSON 输出中途截断；59 条记录的 completion tokens 为 1024，另 1 条为 1026。运行中 Ragas 0.3.9 的 `InstructorModelArgs.max_tokens` 默认为 1024，而 sidecar 的 `llm_factory` 未显式覆盖，提供了输出预算不足的强线索。另发现 judge 代理未保留上游 finish reason，不能根据其返回的 `stop` 排除截断。

已限定到原公开 Open RAGBench/arxiv 两题、相同内部 judge 代理，完成 1024/8192 token 小范围对照；没有改写既有评分记录。第一题在 1024 下 faithfulness 再次失败；改为 8192 后，同题 faithfulness=0.7778（30.68 秒）、factual correctness=0.4（35.56 秒），第二题 faithfulness=1.0（14.38 秒），三项均成功。该对照支持增大 judge 输出预算，但不保证所有复杂样例都能在该预算与超时内完成。

修复将 `RAG_EVALUATOR_LLM_MAX_TOKENS` 配置化，默认 8192，并保持 run 总预算 200 万 tokens / 6 小时。真实 Ragas/Instructor 到 HTTP 的离线测试确认该设置确实进入请求；完整评分服务回归 35 passed、1 个显式联网 smoke skipped。本次上述真实样例诊断已单独执行。新四组统一采用 8192；部署时增加可选 `docker-compose.rag-serial-eval.yml` 将 run worker 限制为 1，允许一次提交四组，由持久队列顺序接续。

### 修复后四组持久队列（2026-09-09）

修复提交 `3cbc66a` 已推送并部署为评分镜像 `bkcrab/rag-evaluator:3cbc66a`（同时使用现有 `0.1.0` 本地标签），旧镜像保留为 `bkcrab/rag-evaluator:before-judge-budget-20260909`。主服务仍运行已修复会话头的 `73c45fd` 二进制，仅把 run worker 配置改为 1；case/score 并发与模型服务保持原设置。两服务健康检查通过，主服务成功识别评分协议。部署后经真实 `/v1/evaluate` 复验同一公开失败样例，faithfulness `ok`，耗时 34.76 秒、LLM 输出 3877 tokens，说明请求确实突破原先 1024 上限。

| 组 | 新 Run ID | 入队后的检查 |
| --- | --- | --- |
| A 全开 | `rer_74edcb129e6a9a252f5c4fdc6c8b0969` | RUNNING；2/50 回答成功，0 错误；原 generation 已复用，准备 19 ms |
| B 关闭 Planner | `rer_992d50417cb34dab71da4024e0ef4007` | QUEUED；rewrite=false、hyde=false、reranker=true |
| C 关闭 Reranker | `rer_b2eb9facbdb6e3cbfb1b79fcd66f9b94` | QUEUED；rewrite=true、hyde=true、reranker=false |
| D 两者关闭 | `rer_8db2c5d1cd82d94d79f58e8f6a29cbb1` | QUEUED；三个开关均 false |

四组均冻结同一数据版本、原 Profile 和全部 12 项指标。B/C/D 尚未开始，generation 需要在各自启动后核对。原 A 和修复前失败 C 保留为诊断记录，不替代新四组。

因创建时新 A 尚未成功，平台不允许将它设为其它运行的 `baselineRunId`。为使四组能够一次进入持久串行队列，新四组均未预绑定旧基线；完成后按同 case ID 对新 A/B/C/D 做配对分析，或调用现有 run compare 接口明确指定新 A。不能把旧 A 的残缺评分与新 judge 设置混作主要对照。持久队列负责逐组执行，不依赖 Codex 定时唤醒；当前尚无完整新消融结论。

## 最终结果（2026-09-09）

A 的独立补评分已于 2026-09-09 14:01:45 UTC 完成 48/48 题，与原 2 题合并后覆盖 50 题。四组现可进行完整的处理覆盖比较，仍需按每个指标的有效分母分析：A 的 Faithfulness 为 41/50，Factual Correctness 为 47/50，其余三项为 50/50。平台原 A 的超时状态保留，补评分没有回写数据库。

完整结果、置信区间和适用范围见 [RAG 消融结果报告](rag-ablation-results-2026-09-09.md)。关闭 Planner 平均节省 8.28 秒检索时间，未显示明确质量下降；关闭 Reranker 平均节省 48.06 秒，但同题 Context Precision 下降约 10.84 / 100 分。最终答案质量收益证据仍不充分，不能据此宣称关闭阶段后质量等价。

## 本地材料

### 2026-09-09 13:14 UTC 实际完成情况与 A 组补评分

持久队列已执行完四组的检索和回答。四组均实际复用同一 generation，开关与预设一致。

| 组 | 平台状态 | 回答成功 | 五项 LLM 指标已处理题数 | 完成时间 UTC |
| --- | --- | ---: | ---: | --- |
| A 全开 | BUDGET_EXCEEDED | 50/50 | 2/50 | 10:54:06 |
| B 关闭 Planner | SUCCEEDED | 50/50 | 50/50 | 06:31:29 |
| C 关闭 Reranker | SUCCEEDED | 49/50 | 49/49 | 07:12:55 |
| D 两者关闭 | SUCCEEDED | 50/50 | 50/50 | 07:54:06 |

C 有 1 题 `empty_response`。B/C/D 的“已处理”包含单指标错误，不等于每项有效评分均齐全：Faithfulness 有效数为 46/48/45，Factual Correctness 为 48/47/47。错误不能计作零分。A 的 50 份原始回答、检索 trace 和每题 7 项确定性指标均已持久化；最后进度显示 0 是租约恢复时重置展示进度，不能据此判定结果丢失。

A 卡住的直接日志是 `metric "factual_correctness": metric reason exceeds byte limit`。评分服务 `_safe_reason` 按 Python 字符数截断至 2048，而 Go 消费方限制 UTF-8 字节数为 2048；多字节错误文字使整个评分批次无法入库。主服务将该响应校验错误当作可恢复错误，反复获取同一幂等缓存结果，最后触发原运行的 6 小时时长上限。此时不是 provider 的 MissingSessionID 再次发生。

修复将错误信息按 UTF-8 字节截断并丢弃末尾不完整码点，保持评分算法、模型与输出预算不变。完整评分服务回归 **50 passed, 1 gated real-provider smoke skipped**。修复镜像 `bkcrab/rag-evaluator:reason-byte-fix` 已部署到现用 `0.1.0` 标签；旧镜像保留为 `before-reason-byte-20260909`。主服务未重启。

为避免重新生成答案，启动 `scripts/rag-ablation/supplement_public_a.py`：只读投影指定公开 Open RAGBench/arxiv 版本和 A 组保存的回答、contexts、reference，只补缺失的 48 题。沿用原本机内部 judge、8192 输出预算和 Ragas 0.3.9，每次一题、最多 200 万 tokens / 3 小时；结果逐题 fsync 到 `.tmp/rag-ablation-20260908/a-supplement.jsonl`，可续跑且不重复已记录题目（包括单指标错误）。**补评分独立于平台运行记录，未修改数据库或把 A 标成成功。** 主服务的协议错误重试策略尚未修改，未来应单独完善错误分类与进度恢复。

补评分完成后需将原 A 的 2 题 LLM 分数与补充记录合并，再按 case ID 对四组做有效交集的配对比较，注明有效分母、失败率和置信区间。当前尚不能提供完整 A 对照下的质量结论。

检索计时已齐全，以下每组均为同 50 题的实际 trace；检索耗时不含回答和 judge：

| 组 | Planner 均值（秒） | Reranker 均值（秒） | 检索均值（秒） | 检索 P95（秒） |
| --- | ---: | ---: | ---: | ---: |
| A 全开 | 7.43 | 46.99 | 54.91 | 92.63 |
| B 关闭 Planner | 0 | 46.45 | 46.63 | 80.84 |
| C 关闭 Reranker | 6.49 | 0 | 6.86 | 10.18 |
| D 两者关闭 | 0 | 0 | 0.18 | 0.25 |

计时支持 Reranker 是主要延迟来源；是否值得该成本仍需完成质量比较。A/C 的 Planner 回退分别为 13/50 和 16/50，不能把开启 Planner 等同于每题改写/HyDE 都成功。

- 只读投影导出：`.tmp/rag-ablation-20260908/baseline.jsonl`。
- 聚合结果：`.tmp/rag-ablation-20260908/summary.json`。
- 取消状态与错误记录：`.tmp/rag-ablation-20260908/failure.jsonl`。
- 重算命令：`python3 scripts/rag-ablation/summarize.py .tmp/rag-ablation-20260908/baseline.jsonl .tmp/rag-ablation-20260908/summary.json`。

脚本只读取本地导出，不访问网络或调用模型，不会用旧 Top5 假装还原完整 Top20。此次业务代码修改仅为 provider 请求元数据及调用链会话传递，未修改检索、重排或过滤算法。
