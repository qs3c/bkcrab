# Jev Reranker Experiment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 Qwen3、Jev、不重排三组在冻结候选与真实回答上的质量和服务器延迟对比。

**Architecture:** 用独立实验工具复用现有 Go Reranker、评测召回和 GenerateAnswer 组件。先冻结候选，逐组产生完整分数、回答与计时，再复用 Ragas 评测和确定性指标。Jev 作为实验适配器接入，不默认切换生产流量。

**Tech Stack:** Go、Docker、现有 MySQL/Milvus/MinIO 评测数据、HTTP Jev API、Python 离线统计、现有 Ragas evaluator。

**Spec:** `docs/superpowers/specs/2026-09-21-jev-reranker-experiment-design.md`

## Global Constraints

- 服务器：`100.93.173.10`，通过 Tailscale SSH；实验从服务器 Docker 环境发请求。
- 查询候选最多 Top20、回答 Top5；主对照 Planner 关闭、先不应用过滤。
- A/B/C 使用同题同候选；Qwen3 复用 `NewQwen3HTTP`，不重写其打分公式。
- 主时延 case concurrency=1，预热 5 次、正式重排 3 轮；并发=2 的压力对照单列。
- 分数、过滤、模型错误、服务回退分别记录；只在有真数据后输出正式结果。
- Jev 推理首轮估算费用上限 $5，充值不在授权范围。
- 私密凭据不进入仓库、console 输出、manifest 或评测数据。
- 用户已授权实验并指定本轮先出方案；OpenRouter 密钥已提供，当前外部依赖为服务器恢复在线，随后需验证模型和余额。

## Task 1: 接入与数据预检

**Files:** 阅读 `docs/rag-evaluation.md`、最新部署记录；生成 `.tmp/jev-reranker-20260921/preflight.json`。

- [x] 检查本地代码、历史结果、评测执行绑定和 TopN trace 限制。
- [x] 尝试 SSH；Tailscale 显示服务器 offline，已向用户说明。
- [x] 确认 Jev 渠道为 OpenRouter，用户已提供密钥；密钥不写入任何方案或仓库文件。
- [ ] 服务器恢复后执行只读检查，记录源版本、镜像 digest、运行中评测、模型线程/slots/上下文/资源配额。

```powershell
tailscale status
ssh -o BatchMode=yes -o ConnectTimeout=10 xavier@100.93.173.10 "hostname; git -C /home/xavier/Desktop/github/bkcrab rev-parse HEAD; docker ps --format '{{.Names}} {{.Status}}'"
```

- [ ] 从运行网关的 Compose labels 确认实际配置列表；不得直接打印完整 docker inspect/environment，避免输出密钥。
- [ ] 核实历史 dataset/generation 是否 READY、是否存续、回答模型和 judge 是否可用。历史 ID 只用作查找线索，不假定存在。
- [ ] 对已授权 Jev 渠道发起一个公开文本 Noul 请求，验证实际模型 ID、余额、usage、返回结构；保存去密钥后的结果。
- [ ] 记录答案和 judge 价格/订阅配额；任何一项未知都在预检报告中明确，不能伪造成本。

## Task 2: 最小实验客户端与采集工具

**Files:** 新增 `internal/rag/rerank/jev.go`、`internal/rag/rerank/jev_test.go`、`cmd/rag-rerank-bench/main.go`、对应测试；独立工具优先，不先扩展配置 UI。

**Interfaces:** Jev 实现现有 `Rerank(context.Context, string, []string, int) ([]rerank.Result, error)`；实验调用传 `topN=len(documents)` 获取全分数。本轮使用 OpenRouter `/api/alpha/decisions`，初始模型 `typesafe/jev-1.13`；Task 1 必须验证实际返回 model ID。仅从运行环境 `OPENROUTER_API_KEY` 读取密钥。

- [ ] 先写 httptest 合同测试：候选映射和分数排序、返回缺项/非法概率报错、超时取消、全局并发上限、429 不静默重放、错误不泄露 key。测试中的固定输入为 query=`查询退款条件`，documents=`[退款需在七日内申请, 门店营业时间为九点]`，返回概率 `0.9/0.1`，预期排名 index=`0/1`。
- [ ] 实现 channel 已确认的 Jev Noul 适配，保留实际 model ID 与 usage 的实验记录；不将 mock 成功当 live 测试。
- [ ] 工具读入冻结候选，按 case/arm/repeat 调用 Qwen3、Jev 或 RRF；原始结果追加写入 JSONL。每条至少有 caseId、arm、repeat、attempt、model、inputHash、startedAt、durationMs、status、scores、usage。
- [ ] 输入验证拒绝重复 caseId、重复 contextId、缺少原文、非有限召回分数；稳定排序并列保留召回次序。
- [ ] mock 测试验证失败不自动替换为 RRF 成功、恢复运行不重复已完成试验、不同 manifest hash 不混写输出。

```powershell
go test ./internal/rag/rerank ./cmd/rag-rerank-bench
git diff --check
```

## Task 3: 冻结候选与 pilot

**Files:** 在独立工具中复用 `internal/rag/evaluation_runner.go` 的召回组件；生成实验目录下 `manifest.json`、`candidates.jsonl`、`calibration.jsonl`。

- [ ] 将候选导出固定为 `SearchEvaluationWithOptions`，profile RerankerEnabled=false、TopN=20、CandidateTopK=20、rewrite/HyDE=false；不得以旧 Top5 导出代替。
- [ ] 每条保留 query/history、原召回顺序、contextId/documentId、SearchContent、AnswerText、参考答案和标签、generation/contract hash。模型请求构造时排除所有 gold 字段。
- [ ] 校准集与正式集按来源/问题族隔离；补充公开中文 50 题并明确数据来源和合成标签。
- [ ] 用 10～20 个独立校准问题比较单候选并发和 5 候选小批量，冻结提示词、并发、模型版本和阈值。
- [ ] 运行 5 题三组回答/judge smoke，记录实际 usage、错误、费用估算，随后在 manifest 写入正式 token/cost/duration 上限。
- [ ] 对 manifest/candidates/calibration 分别计算 SHA-256，后续正式运行只接受相同指纹。

## Task 4: 正式质量与延迟运行

**Files:** 实验工具、原始实验 JSONL；如需容器构建新增专用实验 Dockerfile，不改生产默认重排。

- [ ] 复用现有 Qwen3 服务，独立低负载工具容器连接所需模型/评测网络；不额外占用线上 HTTP 端口。
- [ ] 从服务器发起预热和 3 轮重排，按题随机交错 A/B/C；保存所有 Top20 分数，不启用客户端答案缓存。
- [ ] 在相同回答绑定下调用 `GenerateAnswer`，保持 AnswerText、引用 ID、提示词和参数一致；取 Top5 进行三组无过滤主比较。
- [ ] 将校准后的过滤方案作为单独比较；模型异常不可冒充空证据或正确拒答。
- [ ] 调用现有 evaluator 评估答案，保存成功/失败/missing；无 chunk gold 不输出伪造 chunk recall。
- [ ] 另采集真实检索到回答完成的完整计时和 20 题并发=2 对照。不能以历史时间与新回放时间拼成“实测端到端”。
- [ ] 检查实际 model ID、prompt hash 和 provider 绑定一致；漂移时切分实验，禁止混成一个 arm。

## Task 5: 离线统计和交付

**Files:** 新增 `scripts/jev-reranker/analyze.py`、`scripts/jev-reranker/test_analyze.py`；生成 spec 指定报告和 JSON。

- [ ] 统计测试使用已知序列 `[1,2,3,4,5]` 验证 mean=3、P50=3、线性 P95=4.8；空集统计应为 null 而非 0。
- [ ] 用一成功一失败的 fixture 验证成功率=0.5、成功延迟分母=1、全请求分母=2；失败不计质量 0。
- [ ] 用两个 case 的三次重复验证 bootstrap 聚类单位仍为 case/来源，不能变成六个独立 case。
- [ ] 复用 `scripts/rag-ablation/compare_completed.py` 的配对原则；对不同数据/模型指纹、重复 attempt、缺失 arm 显式报错或标记未配对。
- [ ] 输出各组质量、时延、有效样本数、错误/回退、费用、中文/英文分层和退化案例。
- [ ] 依照 spec 的非劣界值和延迟目标作出“适合替换 / 限定场景适用 / 不适合 / 证据不足”的结论。
- [ ] 结果审查后提交代码与报告；私密运行文件留在忽略目录。向用户交付真实比较，不把工具完成当实验完成。

## 恢复检查点

当前按用户要求完成方案，停在 Task 1。渠道已确认，服务器待用户中午唤醒；本轮未调用模型、未实施客户端、未修改或部署生产服务。
下一次接手先读 spec 和本计划，再查看用户最新回复及 preflight 文件，不需要再次要求用户批准已授权的实验。
