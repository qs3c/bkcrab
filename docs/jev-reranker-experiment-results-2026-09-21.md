# Jev 替代 Qwen3 重排：实验进展

**当前不是最终结论。** 100 题、三轮重排正在服务器运行。原 OpenCode 密钥的 HTTP 401 阻塞已解除：用户指定改用 DeepSeek 官方 API，新密钥已配置到正常 provider 存储。未切换生产默认重排。

新绑定是 `deepseek-official/deepseek-v4-flash`，当前评测用户默认模型已指向该绑定，已热重载并确认服务 healthy。官方 API 的实测返回 model 为 `deepseek-flash`；[官方说明](https://api-docs.deepseek.com/quick_start/pricing/)表明旧模型名现由 V4.1 Flash 承接。15 份 smoke 答案已重新生成成功。现有 judge 代理不保留强制 tool_choice，故改用独立 Docker 评审直连官方 API、关闭 judge thinking，复用相同 Ragas 指标与 prompt。针对 Ragas 跨事件循环的调用隔离 HTTP 客户端后，完整 5 题×3 组的 75 项指标已全部评分成功。旧失败记录保留，不能与恢复后的指标混算。

首轮英文 50 题重排已齐：Qwen3 50/50 成功、平均 31.021 秒；Jev 36/50 成功、成功请求平均 2.085 秒，其余 9 次 HTTP 503、5 次 HTTP 529。这是首轮中间结果，已显示不能仅凭成功请求的速度就替换生产模型。

## 已得到的真实结果

以下为同一服务器发起、同样 Top20 候选的 **5 题 smoke**，不包含预热；不作为正式 100 题结果。

| 重排方式 | 成功数 | 平均耗时 | P50 | P95 |
|---|---:|---:|---:|---:|
| 当前 Qwen3-Reranker-0.6B | 5/5 | 33.584 秒 | 34.734 秒 | 39.547 秒 |
| OpenRouter Jev，5 段/批、并发 2 | 5/5 | 2.004 秒 | 1.802 秒 | 2.706 秒 |
| 保留原召回顺序 | 5/5 | <1 毫秒 | <1 毫秒 | <1 毫秒 |

这个小样本中，Jev 平均重排耗时减少约 94.0%（约 16.8 倍）。它证明提速潜力，**尚不能证明质量持平或线上可靠性达标**。smoke 的 5 次 Jev 预热中有一次 HTTP 503；随后正式任务的预热也出现失败，全部保留在原始数据中。最终必须同时报告成功延迟、失败耗时和成功率。

独立中文校准集 10 题：每批 5 段/并发 2 平均 1.800 秒，逐段/并发 4 平均 3.745 秒；两种方式均 10/10 成功、标注 gold 均排第一。因此在正式结果出现前选定批量方式。该校准集不计入正式 100 题。

5 题三组的文档级召回均为 100%，但候选主要来自同一参考论文，指标已经饱和，不能当成答案质量或 chunk 相关性证明。初次 15 次回答均因旧渠道鉴权失败；更换官方渠道后 15/15 成功。最终质量指标仍在验证与采集中，不能将缺失记为零。

## 固定的实验条件

- 服务器 `100.93.173.10`，CPU 为 AMD Ryzen 7 8745H；复用现有 Qwen3 Docker 服务：Q4_K_M、2 threads、2 parallel slots、1600MiB 内存上限。
- 实验使用独立 Docker 容器，case concurrency=1；同题随机交错 Qwen3、Jev、原始排序，5 次预热单列，正式三轮。Qwen3 直接复用 bkcrab 的客户端。
- 英文：Open RAGBench/arxiv 50 题，复用 READY generation `reg_13014a14cc8c4bca8989692e69ca10d7`，12829 chunks。新冻结 Top20，不使用历史 Top5，不补 gold；关闭 rewrite、HyDE 和分数过滤。
- 中文：CMRC2018 40 道 gold 插入诊断题、10 道移除证据的题，另有 10 道校准题。候选由字符二元组 BM25 和人为置位构成，**不是 bkcrab 的真实中文 RRF**；与英文分开报告，不把这种弱初排用于宣传线上收益。移除证据题也不是人工认证的真实不可回答题。
- Jev：OpenRouter Decisions API，请求 `typesafe/jev-1.13`，已观察实际返回 `typesafe/jev-1.13-20260917`，每批 5 段、并发 2、60 秒超时，不自动重试、不回退。
- 回答固定三组使用 `deepseek-official/deepseek-v4-flash`、Top5、temperature=0、maxTokens=4096、`rag-answer-v1`；官方默认 thinking 模式。Judge 使用同一官方服务、thinking disabled、8192 max tokens，保留函数调用参数。旧 OpenCode 结果单独标记，不参与正式质量比较。
- Jev 正式过程保守费用上限 $4.50，预留 $0.50 给校准/探针；这里只记录 API 返回费用。失败或被取消请求可能缺少账单数据，因此不能把已返回费用当完整账单。官方 DeepSeek 评审按当前峰时 cache-miss 单价估算：输入 $0.30/M、输出 $1.20/M；这不是实际账单。正式回答上限 1.5M tokens；正式 judge 上限 6M tokens、5 小时、$4 保守费用估算，并保留单题余量。小样本和修复探针另记。

## 工件与复现

- 服务器目录：`/home/xavier/bkcrab-experiments/jev-20260921/`。
- 正式容器：`jev-bench-formal`；逐条追加 `formal-ranks.jsonl`，配置在同名 `.manifest.json`。
- 合并输入 SHA-256：`371926e0bf3c7027884e3b8b6c9635abb191666f0c562ac8696e4a6661808483`。
- 实验二进制 SHA-256：`01a6faeeeb905e5913244118c8a8e2b1e81f4313d17d6ee1d1cbbee5ed522b32`；客户端/runner 源码提交 `715261a`。
- 停止后由 `finish_ranking.py` 自动产生按语言拆分的重排统计；它不会将缺失的回答质量伪装成完成。
- [实验运行说明](../scripts/jev-reranker/README.md)、[设计口径](superpowers/specs/2026-09-21-jev-reranker-experiment-design.md)。原始候选、分数、探针、manifest 保存在忽略目录中，凭据不提交。

已通过：Go 客户端/runner 测试，Python 分位数、失败分母、配对聚类 bootstrap 测试。原始数据失败不计质量零分；重复轮次不是额外独立问题。

## 待完成

1. 收齐正式重排与延迟记录，检查实际模型版本、失败率和尾部延迟。
2. 官方渠道 5 题三组 smoke 已通过；等待正式重排首轮快照后生成正式三组答案与 Ragas 评审。
3. 给出按语言的质量均值、配对差值与置信区间、具体退化案例和采用建议。

当前不推荐据此直接替换默认重排。真实在线端到端、并发负载、过滤阈值、多证据/条件否定和真实业务资料仍须单独验证；冻结回放各阶段耗时之和不会标成在线端到端实测。

数据来源：[CMRC2018 官方仓库](https://github.com/ymcui/cmrc2018)，revision `c0eb1b6ba219847457e6af3180da722bbeb656af`；英文来源与版本见现有[历史公开集评测](rag-ablation-results-2026-09-09.md)。
