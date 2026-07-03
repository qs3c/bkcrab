# SkillsLearner 加固设计(方案 B 修订版)

日期:2026-07-02
分支:`worktree-skills-learner-hardening`
状态:已经用户确认(2026-07-02 增补第 5-6 节跨 turn 累计触发,亦已用户确认)

## 背景与目标

bkcrab 的技能自动提炼(`SkillsLearner`,internal/agent/skills_learner.go)当前存在五个问题:

1. 默认关闭(`Enabled bool` 零值 = false),功能实际未启用;
2. 提取门槛过低(3 次工具调用),琐碎对话也会触发提取判定;
3. LLM 生成的 SKILL.md 直接 `os.WriteFile` 落盘,无任何 frontmatter/大小校验、无安全扫描;
4. slug 已存在时直接跳过,技能一次生成后永不更新,过期内容只能烂在盘上;
5. 触发判定是单 turn 的内存态判断(`toolCallCount >= minToolCalls`):跨多个 turn 完成的多步流程永远凑不满门槛,一个技能横跨几个 turn 的完整操作序列也进不了提取输入;且计数不落盘,实例重启即丢,多实例部署下依赖 ingress 粘性路由的正确性——而粘性只该保护流式连接的连续性,不该承载状态正确性。

本设计对照 hermes-agent 的 skill_manage / skills_guard 机制补齐上述缺口,但**最大化复用 bkcrab 已有设施**(`internal/privacy/scanner.go` 威胁扫描器、`internal/memory/manager.go` 的先扫后写与原子写模式),而非照搬 hermes。

## 范围

| # | 改动 | 说明 |
|---|---|---|
| 1 | `Enabled *bool`,nil = 默认开启 | 沿用 bkcrab `AutoPersist *bool` 惯例 |
| 2 | `minToolCalls` 默认 3 → 10 | 配置仍可覆盖 |
| 3 | frontmatter / 大小校验 | 写盘前强制 |
| 4 | 安全扫描 | 复用并扩展 `internal/privacy` |
| 5 | 技能管理器 + learner 支持 update | 撞名时二阶段 LLM 决策 |
| 6 | 跨 turn 累计触发 | 复用记忆提取的 cadence-claim 模式,累计状态入库(MySQL/SQLite),`MinToolCalls` 语义改为同 session 累计 |

**明确不做**(留给后续独立任务):

- agent 可调用的 `skill_manage` 工具、fuzzy patch 引擎、技能自动删除/清理;
- **技能产物的共享存储**:learner 提取的 SKILL.md 仍写实例本地 `workspace/skills/`。多实例下 A 实例提取的技能 B 实例不可见——已知限制。出路已有雏形:`Agent.workspaceStore`(workspace.Store 对象存储)已经让全局技能目录每回合水合、跨副本可见,learner 产物后续可走同一通道,独立课题;
- 跨 session 的技能累计、按 chatter 维度聚合(YAGNI,session 内累计已覆盖目标场景)。

## 架构

```
internal/privacy/scanner.go        ← 改:新增 ScanSkillStrict()
internal/skills/manager.go         ← 新:受控写入层(放入现有 skills 包)
internal/agent/skills_learner.go   ← 改:两阶段提取,写入走 manager;新增按回放 turn 批提取入口
internal/agent/loop.go             ← 改:用 IsEnabled() 判断;runPostTurn 接线 cadence 认领路径
internal/config/config.go          ← 改:Enabled *bool + IsEnabled();MinToolCalls 注释改累计语义
internal/store/store.go            ← 改:FinishTurn 增参;新增 ClaimSkillBatch / ResetSkillExtraction
internal/store/database.go         ← 改:锚点行加列迁移;实现新接口(database_mysql.go 建表同步)
skills/bkcrab-skill-learner/SKILL.md ← 改:提示词支持 update 决策,去掉硬编码次数
```

依赖方向:`agent → skills → privacy`,单向无环(agent 已依赖 skills;memory 已依赖 privacy,skills 新增依赖 privacy 同向)。

**取舍说明**:frontmatter 解析 bkcrab 已有 `agent.SplitSkillFrontmatter`,但位于 agent 包,manager(skills 包)不能反向依赖。选择在 manager 内实现约 30 行的独立校验器(yaml 解析 name/description + 正文非空),不搬迁 agent 包的解析函数(会波及 handlers_skills.go 等调用方,超出范围)。

## 组件设计

### 1. config:默认开启

```go
type SkillsLearnerCfg struct {
    Enabled      *bool  `json:"enabled,omitempty"`  // nil = 默认开启
    MinToolCalls int    `json:"minToolCalls,omitempty"`
    Model        string `json:"model,omitempty"`
}

func (c SkillsLearnerCfg) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }
```

- loop.go 的 `if fullCfg.SkillsLearner.Enabled` 改为 `IsEnabled()`。
- 显式设 `false` 者行为不变;从未配置者升级后自动启用。
- 设置存储(namespace `skillsLearner`)序列化 `*bool` 无兼容问题;web 前端无此设置页,无前端影响。

### 2. privacy:ScanSkillStrict

新增 `ScanSkillStrict(text string) []Threat` = `ScanMemoryStrict` 全部模式 + 技能特有增量模式(约 8 条,参照 hermes skills_guard):

- 凭据目录引用:`~/.ssh`、`~/.aws`、`~/.kube`、`~/.docker`、`~/.gnupg`(hermes: *_dir_access)
- 环境变量密钥外传:`curl/wget/fetch` 插值 `$KEY/$TOKEN/$SECRET/$PASSWORD` 类环境变量
- 混淆:`base64` 与 env 组合出现

复用现有 `appendThreatMatches`/`snippet`/`dedupeThreats` 机制,新类型沿用现有 `ThreatType`(exfiltration/persistence_abuse 等),必要时增加 `ThreatObfuscation`。

**策略:零容忍。** 任何命中 = 拒写 + `slog.Warn`(含威胁类型摘要)。不引入 hermes 的三级 verdict(safe/caution/dangerous)——学习器场景无人可问,分级无意义,且与 memory 防线口径保持一致。

### 3. skills.Manager:受控写入层

放入现有 `internal/skills` 包。风格对齐 `memory.Manager`(Config + DefaultConfig + Result)。

```go
type ManagerConfig struct {
    MaxContentChars     int // 默认 100_000
    MaxDescriptionChars int // 默认 1024
    MaxSlugChars        int // 默认 64
}

type Manager struct { root string; config ManagerConfig } // root = workspace/skills

func (m *Manager) Create(slug, content string) error  // 已存在则拒
func (m *Manager) Update(slug, content string) error  // 不存在则拒
func (m *Manager) Delete(slug string) error           // 为将来 agent 工具预留,learner 不调用
```

写入流水线(Create/Update 共用):

1. **slug 校验**:`^[a-z0-9][a-z0-9._-]*$`,≤64 字符(阻断路径遍历)
2. **frontmatter 校验**:以 `---` 开头、yaml 可解析、含非空 `name`/`description`(≤1024 字符)、frontmatter 后正文非空
3. **大小校验**:全文 ≤100k 字符
4. **安全扫描**:`privacy.ScanSkillStrict(content)`,命中即拒
5. **原子写**:`os.CreateTemp` + `os.Rename`,按路径 `sync.Mutex`(仿 memory.mutateFile)

**先扫后写,无备份、无回滚**(hermes 是写盘→扫目录→回滚;memory 的先扫内容字符串更简单,采用后者)。

### 4. learner:两阶段提取

```
MaybeExtract(ctx, messages, toolCallCount)
  ├─ toolCallCount < minToolCalls(默认10) → 返回
  ├─ 阶段一:LLM 判定 + 产出 {extract, skill{name, slug, description, content}}(与现状相同)
  ├─ slug 不存在 → manager.Create → 结束
  └─ slug 已存在 → 阶段二:
        输入:旧 SKILL.md 全文 + 新提取内容
        输出:{"update": true, "content": 合并后全文} 或 {"update": false}
        → update=true → manager.Update;否则跳过
```

- 上图入口处的单 turn 门槛判定仅保留在**回退路径**(无持久化 store/无锚点,见第 6 节);cadence 路径的门槛在认领事务内由 `SUM(tool_call_count) >= N` 强制,认领成功后从「阶段一」进入同一条流水线。
- 阶段二仅在撞名时发生,额外 LLM 调用成本可控(温度同为 0.3,maxTokens 适当放大以容纳全文)。
- 提示词(SKILL.md 与 fallbackExtractionPrompt)中硬编码的「3+ tool calls」删除,门槛由代码强制,提示词只描述「多步、可重复」,避免数字两处漂移。
- `skills/bkcrab-skill-learner/SKILL.md` 增补阶段二的 update 决策指引与输出格式。

### 5. 存储层:跨 turn 累计(cadence-claim,复刻记忆提取模式)

触发判定从"本 turn 工具调用数"改为"同 session 内已完成且未被技能提取消费的 turn 的工具调用数总和 ≥ N"。累计状态放 `session_messages` 锚点行,判定与认领在单个 DB 事务内原子完成——正确性锚在数据库行锁上,与实例数、粘性路由无关。

**锚点行加两列**(幂等迁移,照 `migrateSessionMessagesAddTurnColumns` 模式;`database.go` 与 `database_mysql.go` 的建表语句同步):

- `tool_call_count`(INT NOT NULL DEFAULT 0):本 turn 的工具调用数,`FinishTurn` 翻 done 时一并写入。历史存量行默认 0——只会少算不会误触发,无需回填。
- `skill_extraction_id`(TEXT/VARCHAR(64) NULL):NULL = 未被技能提取消费。与记忆提取的 `extraction_id` 平行、互不干扰,同一批 turn 可分别被两条提取线各消费一次。

**接口改动**(internal/store/store.go):

```go
// FinishTurn 增参:翻 done 的同时落本 turn 的工具调用数(调用点 runPostTurn 手上就有)。
FinishTurn(ctx, userID, agentID, sessionKey string, seq int64, toolCallCount int) error

// ClaimSkillBatch 在单个写事务内:选出该 (agent, session) 下 turn_status='done' 且
// skill_extraction_id IS NULL 的锚点(按 created_at,seq 至多 batchCap 条,MySQL/PG 加
// FOR UPDATE),若 SUM(tool_call_count) >= minTotal 则生成 uuid、整批置位
// skill_extraction_id 并返回 (uuid, TurnRef 列表);不足返回 ("", nil, nil)。
// 事务保证并发收尾(同实例异步 post-turn / 直连入口 / 跨实例)不会重复认领。
ClaimSkillBatch(ctx, agentID, sessionKey string, minTotal, batchCap int) (string, []TurnRef, error)

// ResetSkillExtraction 把某次认领的 skill_extraction_id 重置回 NULL(补偿回滚)。
ResetSkillExtraction(ctx, skillExtractionID string) error
```

- 回放复用现有 `LoadTurnMessages`,无需改动。
- **不加新索引**:查询走现有 (agent_id, session_key) 路径,生产(MySQL)单 session 最多几百行、锚点数十个,过滤成本可忽略;与近期清理冗余索引的方向一致。
- batchCap 取 32(防御超长 session,正常远达不到);语义与 `ClaimCadenceBatch` 一致——先截取再判定。
- SQLite 方言无 FOR UPDATE,与 `ClaimCadenceBatch` 同款处理(靠单写者保证)。

### 6. loop 接线与并发语义

`runPostTurn` 中技能学习分支改为双路径:

```
a.finishTurnAndMaybeExtract(ctx, chatterMem, anchor, toolCallCount)   // FinishTurn 顺带落 tool_call_count
if a.skillsLearner != nil {
    有 dataStore 且有 anchor(cadence 路径):
        ClaimSkillBatch(agent, session, MinToolCalls, 32)
          ├─ 未达标 → 空手返回,什么也不做
          └─ 认领成功 → goroutine 内 LoadTurnMessages 回放整批 turn
                         → 拍平为按 seq 升序的对话摘要 → 走第 4 节两阶段提取 + Manager 写入
    无 dataStore 或无 anchor(计划模式/无持久化 store):
        回退现状——单 turn 内存判定 MaybeExtract(messages, toolCallCount),本地模式不退化
}
```

- **认领在 FinishTurn 之后**同步执行(本 turn 的计数才可见),LLM 提取在 goroutine 内异步执行,姿态与记忆提取一致:`context.WithoutCancel` + 提取 5 分钟上限,补偿重置用独立短 ctx——避免流式路径下请求 ctx 取消导致批次永久卡死。
- **消费语义与记忆提取不同**:LLM 判定"不值得提取"、frontmatter 校验拒绝、安全扫描拒绝均视为**已消费**(不 reset)——否则同一批 turn 每次都被重复判定。仅基础设施错误(回放失败、LLM 超时/网络错误、DB 故障)才 `ResetSkillExtraction` 放回待提取池。
- **并发窗口如实陈述**:走 taskqueue 的渠道单实例内同一聊天严格串行,但(1)流式路径 post-turn 在耗尽 SSE 流的后台 goroutine 里拖尾,可与下一 turn 重叠;(2)OpenAI 兼容 API 与 webhook server 直连 `HandleMessage(Stream)`,不经 per-chat 队列;(3)per-chat 队列是进程内的,多实例间不串行。三者均由 claim 事务的原子性覆盖——这不是额外防御,"SUM 判定 + 打标"本就必须原子。
- 累计状态全在 DB,实例内不新增共享可写状态,不触碰每回合 Registry 隔离不变量。
- **配置语义**:`MinToolCalls` 含义从"单 turn ≥ N"改为"同 session 累计 ≥ N",默认仍 10,不加新字段。单 turn 达标被累计语义自然涵盖,旧配置无需迁移。

## 错误处理

姿态不变:**learner 永远不打断主对话**,仍在 `runPostTurn` 的 goroutine 内异步执行。

- 扫描拒绝 → `slog.Warn`(命中模式类型摘要),批次已消费不放回
- LLM 返回非法 JSON → 静默放弃(Debug 日志,与现状一致),批次已消费不放回
- 校验失败 / 写盘失败 → 错误日志,不重试,批次已消费不放回
- 回放失败 / LLM 超时或网络错误 / DB 故障 → 错误日志 + `ResetSkillExtraction` 补偿,批次回到待提取池
- `ClaimSkillBatch` 自身失败 → `slog.Warn`,本 turn 不触发,累计仍在库里、下一 turn 收尾时重试判定

## 测试

- **privacy**:`ScanSkillStrict` 增量模式表驱动测试,每类至少一正一负样例;确认 `ScanMemoryStrict` 原有用例不受影响
- **manager**:`t.TempDir` 上测 Create(新建/已存在拒)/Update(更新/不存在拒)/slug 非法拒/frontmatter 缺失拒/超大小拒/扫描命中拒
- **learner**:fake provider 注入脚本化 JSON,覆盖四条路径:门槛不足直接返回;新建;撞名→update;撞名→skip
- **config**:`IsEnabled()` 的 nil/true/false 三态
- **store(新增)**:`FinishTurn` 正确落 `tool_call_count`;`ClaimSkillBatch` 累计不足不认领 / 达标整批认领 / 并发无双认领(照 `TestClaimCadenceBatchNoDoubleClaim`)/ 与记忆 `extraction_id` 互不干扰(同批 turn 两条线各消费一次);`ResetSkillExtraction` 放回后可再次认领
- **agent(新增)**:cadence 路径 claim→回放→提取全链路(fake provider);"判定不提取"不放回、基础设施错误放回;无 store/无 anchor 回退单 turn 判定
- 收尾 `go test ./...` 全绿
