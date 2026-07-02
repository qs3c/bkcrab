# SkillsLearner 加固设计(方案 B 修订版)

日期:2026-07-02
分支:`worktree-skills-learner-hardening`
状态:已经用户确认

## 背景与目标

bkcrab 的技能自动提炼(`SkillsLearner`,internal/agent/skills_learner.go)当前存在四个问题:

1. 默认关闭(`Enabled bool` 零值 = false),功能实际未启用;
2. 提取门槛过低(3 次工具调用),琐碎对话也会触发提取判定;
3. LLM 生成的 SKILL.md 直接 `os.WriteFile` 落盘,无任何 frontmatter/大小校验、无安全扫描;
4. slug 已存在时直接跳过,技能一次生成后永不更新,过期内容只能烂在盘上。

本设计对照 hermes-agent 的 skill_manage / skills_guard 机制补齐上述缺口,但**最大化复用 bkcrab 已有设施**(`internal/privacy/scanner.go` 威胁扫描器、`internal/memory/manager.go` 的先扫后写与原子写模式),而非照搬 hermes。

## 范围

| # | 改动 | 说明 |
|---|---|---|
| 1 | `Enabled *bool`,nil = 默认开启 | 沿用 bkcrab `AutoPersist *bool` 惯例 |
| 2 | `minToolCalls` 默认 3 → 10 | 配置仍可覆盖 |
| 3 | frontmatter / 大小校验 | 写盘前强制 |
| 4 | 安全扫描 | 复用并扩展 `internal/privacy` |
| 5 | 技能管理器 + learner 支持 update | 撞名时二阶段 LLM 决策 |

**明确不做**(留给后续独立任务):agent 可调用的 `skill_manage` 工具、fuzzy patch 引擎、技能自动删除/清理。

## 架构

```
internal/privacy/scanner.go        ← 改:新增 ScanSkillStrict()
internal/skills/manager.go         ← 新:受控写入层(放入现有 skills 包)
internal/agent/skills_learner.go   ← 改:两阶段提取,写入走 manager
internal/agent/loop.go             ← 改:用 IsEnabled() 判断
internal/config/config.go          ← 改:Enabled *bool + IsEnabled()
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

- 阶段二仅在撞名时发生,额外 LLM 调用成本可控(温度同为 0.3,maxTokens 适当放大以容纳全文)。
- 提示词(SKILL.md 与 fallbackExtractionPrompt)中硬编码的「3+ tool calls」删除,门槛由代码强制,提示词只描述「多步、可重复」,避免数字两处漂移。
- `skills/bkcrab-skill-learner/SKILL.md` 增补阶段二的 update 决策指引与输出格式。

## 错误处理

姿态不变:**learner 永远不打断主对话**,仍在 `runPostTurn` 的 goroutine 内异步执行。

- 扫描拒绝 → `slog.Warn`(命中模式类型摘要)
- LLM 返回非法 JSON → 静默放弃(Debug 日志,与现状一致)
- 校验失败 / 写盘失败 → 错误日志,不重试

## 测试

- **privacy**:`ScanSkillStrict` 增量模式表驱动测试,每类至少一正一负样例;确认 `ScanMemoryStrict` 原有用例不受影响
- **manager**:`t.TempDir` 上测 Create(新建/已存在拒)/Update(更新/不存在拒)/slug 非法拒/frontmatter 缺失拒/超大小拒/扫描命中拒
- **learner**:fake provider 注入脚本化 JSON,覆盖四条路径:门槛不足直接返回;新建;撞名→update;撞名→skip
- **config**:`IsEnabled()` 的 nil/true/false 三态
- 收尾 `go test ./...` 全绿
