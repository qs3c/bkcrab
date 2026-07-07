# Learner 技能生命周期管理设计

日期:2026-07-07
状态:已经用户确认(brainstorming 全程逐节确认)
前置:[SkillsLearner 加固设计(2026-07-02)](2026-07-02-skills-learner-hardening-design.md)——本设计实现其"明确不做"里预留的"技能自动删除/清理"独立任务。

## 背景与目标

`SkillsLearner`(internal/agent/skills_learner.go)会在一段较长的工具调用任务后触发 review 式提取,自动生成 SKILL.md 写入 `workspace/skills/`。当前机制"管生不管养":

1. **产物无生命周期**。learner 技能写进 agent 层(skills.go 优先级最高层),无条件进入系统提示的 `<skill_catalog>`(skills.go:293),目录只增不减、没有任何出口。
2. **粗糙的价值信号**。"任务做得长"被当作"经验值得沉淀"的代理信号,容易产出大量低价值技能。
3. **程序层挡不住语义重复**。撞名可挡(manager.Create 已拒重名),但语义相似、名字不同的重复技能挡不住,只靠 LLM 自觉。
4. **膨胀污染选择**。catalog 随技能数增长,加重模型的选择负担,影响命中率和稳定性。

**核心目标**:不禁止自动生成,而是给 learner 生成的技能(且仅这些)加一层"竞争上岗"——用**真实使用信号**把 catalog 里的 learner 技能稳定在一个小规模,淘汰基于机会次数而非墙钟时间。

**三态**(全部**派生**,不落状态列):

- `active`:进入 catalog(系统提示)。= 竞争前 K 名 + 保护期新技能 + 手改保护技能。
- `cooling`:落选,不进 catalog,但仍在磁盘、`load_skill` 仍可命中——用户点名即复活。
- `deleted`:唯一持久化动作。仅针对"从生成起从未被任何人用过、且已历经 D 次加载机会仍无人问津"的 learner 技能,直接 `Manager.Delete` 删目录。

**用机会次数,不用时间**:一个全局"加载序号"作为时钟,只在 `load_skill` 成功加载时推进。闲置 90 天不惩罚,纯闲聊 turn 也不推进时钟——只有别的技能持续被选中而某技能一直没被选中时,它才会衰减掉。这正是"竞争上岗"语义。

## 范围

**做**:

| # | 改动 | 说明 |
|---|---|---|
| 1 | `skill_usage` 新表(MySQL+SQLite 双方言) | 每行 = 一个 (agent, learner 技能) 使用账本 |
| 2 | 指数衰减活跃度打分 + 派生三态 | 纯函数 `skills/lifecycle.go` |
| 3 | `load_skill` 记账 + `invoked_by_user` 参数 | 模型自陈述用户是否点名;手改检测 |
| 4 | catalog 构建按排名过滤 learner 技能 | skills.go:BuildSkillsSummary 前置过滤 |
| 5 | post-turn 顺风车做 deleted 清理 | 搭现有 cadence goroutine |
| 6 | learner 建账本行 + 提取 prompt 注入现有 slug 清单 | 撞名引导走 update(便宜的半程序化去重) |
| 7 | `SkillsLearnerCfg` 加 `Lifecycle` 子配置 | 参数全可覆盖 |

**明确不做**(YAGNI / 留后续):

- **语义去重全套**(embedding/相似度)。只做便宜缓解:阶段一提取 prompt 注入现有 learner `slug — description` 清单,引导撞名走既有 update 路径。
- **`$技能名` 显式调用语法**。曾评估,最终由"`load_skill` 加 `invoked_by_user` 参数、把点名判断交给模型"替代——召回更全、成本更低、下限安全(见 §7.3 取舍)。想要 `$` 作为用户 UX 快捷入口是独立产品特性,与 explicit 统计解耦,未来单独做。
- **无持久化 store 的降级路径**。bkcrab 是后端项目,生产必有 MySQL、开发/测试有 SQLite,不存在无 store 场景。(learner 现有的"无 store 回退单 turn MaybeExtract"路径保持不动,与生命周期正交。)
- **删除的软删/回收区/宽限**。直接 `Manager.Delete`(用户确认)。
- **跨 agent 的序号可比、跨 session 聚合**。序号只需单个 agent 内单调。

## 架构

改动落点:

```
internal/store/store.go            ← 改:Store 接口加 skill_usage 相关方法 + SkillUsageRow 类型
internal/store/database.go         ← 改:建 skill_usage 表(SQLite);实现新方法;DecayFactor helper
internal/store/database_mysql.go   ← 改:建 skill_usage 表(MySQL)同步
internal/skills/lifecycle.go       ← 新:纯函数 Rank(rows,nowSeq,cfg)→(active,deletable),无 IO
internal/agent/skills_learner.go   ← 改:persistExtracted 建/更账本行(origin+content_hash);阶段一 prompt 注入现有 slug 清单
internal/agent/skills.go           ← 改:BuildSkillsSummary 前用 lifecycle.Rank 过滤 learner 技能;skillsDirective 补 invoked_by_user 说明
internal/agent/tools/load_skill.go ← 改:注册注入 store+agentID;加 invoked_by_user 参数;加载成功异步记账(含手改检测)
internal/agent/loop.go             ← 改:post-turn goroutine 顺带跑 deleted 清理
internal/config/config.go          ← 改:SkillsLearnerCfg 加 Lifecycle 子结构
skills/bkcrab-skill-learner/SKILL.md ← 改:提示词注入现有 slug 清单的位置约定
```

**依赖方向**:`agent → skills → store`。skills 新增对 store 的依赖(拿 `SkillUsageRow` 类型 + `DecayFactor`),与现有 `agent → skills → privacy` 同向。**需确认 store 不反向 import skills**(预期不依赖,store 是持久层叶子)——实现前 grep 核对。

**为何 lifecycle 抽成纯函数**:排名/删除判定输入一组账本行 + 当前序号 + 配置,输出 active/deletable 名单,不碰 IO。表驱动可穷举衰减、保护期、切分、删除条件的边界,不必装配 store/agent。

## 数据模型

一张新表 `skill_usage`,**每行 = 一个 (agent, learner 技能) 的使用账本**。全局序号不另开单行表,取该 agent `MAX(last_load_seq)` 即当前时钟值。

```
skill_usage
────────────────────────────────────────────────────────────────
  agent_id        VARCHAR(64)  NOT NULL   -- 与技能目录同维度隔离
  slug            VARCHAR(64)  NOT NULL   -- 技能 slug
  origin          VARCHAR(16)  NOT NULL   -- 'learner'(唯一入账来源;手工技能不建行)
  activity        DOUBLE       NOT NULL DEFAULT 0   -- 衰减活跃度(惰性:写在加载时)
  last_load_seq   BIGINT       NOT NULL DEFAULT 0   -- 上次加载时的全局序号
  total_loads     BIGINT       NOT NULL DEFAULT 0   -- 累计加载次数(删除闸门:==0 才可删)
  explicit_uses   BIGINT       NOT NULL DEFAULT 0   -- 其中 invoked_by_user=true 的次数(仅观测)
  created_seq     BIGINT       NOT NULL DEFAULT 0   -- 建行时的时钟值(保护期+删除阈值基准)
  edited_seq      BIGINT       NOT NULL DEFAULT 0   -- 检测到用户手改时的时钟值(0=从未;手改保护基准)
  content_hash    CHAR(64)     NOT NULL DEFAULT ''  -- learner 最后一次写盘内容的 sha256
  created_at      ...                    -- 审计
  updated_at      ...                    -- 审计
  PRIMARY KEY (agent_id, slug)
```

字段语义:

| 字段 | 作用 | 谁写 |
|---|---|---|
| `activity`+`last_load_seq` | 排名全部输入;读时现算 `activity·0.5^((now−last)/H)` | 加载记账 |
| `total_loads` | 删除闸门(`==0` 才可删) | 加载记账 +1 |
| `explicit_uses` | 观测/未来 UI;**不进排名公式**(点名权重已折进 activity 的 gain) | 加载记账(命中时 +1) |
| `created_seq` | 保护期(`now−created<G` 不淘汰)+ 删除阈值(`now−created>D` 才删) | 建行时写一次 |
| `edited_seq` | 手改保护(`edited>0 && now−edited<P` 不淘汰);到期后回归竞争 | 手改检测置位 |
| `content_hash` | learner 每次写盘刷新;与盘上不符=用户手改 | learner 写 / load 时比对 |

**不加新索引**:查询走主键 `(agent_id, slug)` 或 `WHERE agent_id=?`,生产单 agent 的 learner 技能数十个量级,过滤成本可忽略——与近期清理冗余索引方向一致。

**建行/更行时机**(learner 成功写盘后 upsert):
- 首次 `Create` 一个 slug:插入行,`origin='learner'`,`created_seq = 当前时钟`,`content_hash = sha256(内容)`,其余计数 0。
- 后续 `Update` 同一 slug:**只刷 `content_hash`**,不重置 `created_seq`/`edited_seq`(learner 自身演进不续命、不清手改保护)。刷 hash 使 learner 的合法更新不会被 load 误判成用户手改。

**无账本行的技能一律永久 active**:手工/bundled/team/user 层技能,以及历史存量 learner 技能(升级前已存在、还没被 learner 再写过或首次记账),都没有账本行 → 排名阶段直通 active、不计数、不参与删除。**上线无需回填**。

## 打分与排名

### 公式(`skills/lifecycle.go` 纯函数)

**加载记账**(load_skill 成功,时钟推进到 `s`):

```
gain          = ExplicitGain(默认3) if invoked_by_user else 1
decay         = 0.5^((s − last_load_seq)/H)
activity      = activity·decay + gain
last_load_seq = s
total_loads  += 1
explicit_uses += (invoked_by_user ? 1 : 0)
```

**排名**(每回合构建 catalog,纯读、不写库):

```
effective(row, now_seq) = activity · 0.5^((now_seq − last_load_seq)/H)
```

learner 账本行按 `effective` 降序。半衰期 `H`(默认 32):32 次加载机会后活跃度减半。配置存 `H`(整数,加载机会数),不存 λ,避免浮点魔数。

**衰减因子唯一实现** `DecayFactor(dt int64, halfLifeLoads int) float64 = math.Pow(0.5, float64(dt)/float64(H))` 定义在 store 包(叶子),写路径(store 记账)与读路径(skills.Rank)共用同一函数,杜绝公式漂移。`H<=0` 视为默认 32(防 0 除)。

### 关键不变量

两次加载之间,所有行的 `effective` 同乘 `0.5^(Δseq/H)`,**相对顺序恒定**。⇒ 排名只在某技能被加载的瞬间变动 ⇒ active/cooling 边界在无事发生时不抖动。这是"派生状态不需要迁移代码"的数学依据。

### 数值防御

- 排序前对 `effective` 做有限性钳制:`NaN`/`+Inf` → 视为 0(排到末尾),避免异常账本行污染排序。
- `dt` 可能因并发拿到相同 seq 而为 0 或极小,`decay=1`,无害。
- `dt` 极大时 `decay→0`,`effective→0`,自然沉底——不会溢出。

## 三态与派生逻辑

`skills.Rank(rows, nowSeq, cfg)` 返回 `(activeSlugs map[string]bool, deletable []string)`。catalog 构建只吃 learner 技能里 active 的那部分;非 learner 技能在调用 Rank 前已直通。

```
对每个 learner 账本行:
  ├─ edited_seq>0 && now−edited_seq < P(手改保护,默认30) → active(独占名额)
  ├─ now−created_seq < G(创建保护,默认20)               → active(独占名额)
  └─ 其余(成熟技能)进入竞争池
成熟池按 effective 降序,前 K(默认10)→ active,其余 → cooling(不列入 active)

deletable(与 active 判定并行,同一次 Rank 产出):
  origin=learner && edited_seq==0 && total_loads==0 && (now−created_seq) > D(默认200)
  → 加入 deletable
```

- **保护期技能独占名额,不占前 K**:短时间内新建 K 个技能不会挤掉原本该在 catalog 的成熟技能。极端下 catalog 里 learner 技能可能多于 K,但多出的都是"刚生成"或"用户刚手改"的,属有意保留。
- **cooling 不是标记,就是"没进 active"这个事实**。cooling 技能目录仍在 skillDirs(load_skill.go:42 与 catalog 解耦)→ 用户点名 → load → activity 跳升 → 下回合自然回前 K。复活是自动的。
- **deleted 是唯一持久动作**。只删"从生成起 total_loads==0(从没被任何人加载过)、未被手改、且已历经 D 次加载机会"的技能。用过哪怕一次(`total_loads>=1`)→ 永远最多停在 cooling,绝不删。
- **零使用零清理**(用户确认接受):一个从不用技能的 agent,时钟不推进,其 learner 垃圾技能永远删不掉——但它们本就在 cooling、不进 prompt,无实际痛感。

## 组件设计

### 7.1 store:skill_usage 表 + 接口

新增 `SkillUsageRow` 类型(导出,供 skills.Rank 消费)与 Store 接口方法:

```go
type SkillUsageRow struct {
    Slug         string
    Origin       string
    Activity     float64
    LastLoadSeq  int64
    TotalLoads   int64
    ExplicitUses int64
    CreatedSeq   int64
    EditedSeq    int64
    ContentHash  string
}

// UpsertSkillUsage 建/更 learner 技能账本行。firstCreate=true 时写 created_seq
// (=当前时钟)并初始化计数;false 时只刷 content_hash(learner Update 路径)。
UpsertSkillUsage(ctx, agentID, slug, contentHash string, firstCreate bool) error

// RecordSkillLoad 在单个写事务内记一次加载:seq = MAX(last_load_seq)+1 该 agent;
// 读该行(无行→(nil,nil),非 learner 不记);算 decay(用 DecayFactor,halfLife 传入)、
// activity = activity·decay + gain、total_loads+1、explicit_uses(+1 if invokedByUser)、
// last_load_seq=seq;若 diskHash != content_hash 置 edited_seq=seq;UPDATE。返回快照供日志。
RecordSkillLoad(ctx, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*SkillUsageRow, error)

// ListSkillUsage 返回该 agent 全部 learner 账本行(排名/清理用一次查询)。
ListSkillUsage(ctx, agentID string) ([]SkillUsageRow, error)

// DeleteSkillUsage 删账本行(deleted 清理时,与 Manager.Delete 目录配对)。
DeleteSkillUsage(ctx, agentID, slug string) error
```

- **不做严格事务性 read-modify-write**:同一 (agent,slug) 并发加载(仅跨实例可能)丢一次更新 → activity 少涨一点、total_loads 少加一次,对本系统完全可容忍;不为此加行锁复杂度。
- 序号 `MAX(last_load_seq)+1` 只需单调不需唯一:两个技能并发拿到相同 seq 无害(排名用差值,删除用阈值,都不要求唯一)。
- SQLite 与 MySQL 建表语句同步(照 `migrateSessionMessagesAddTurnColumns` 的幂等迁移模式建表)。

### 7.2 skills/lifecycle.go:纯排名

```go
type LifecycleConfig struct {
    ActiveMax        int // K, 默认10
    HalfLifeLoads    int // H, 默认32
    ProtectLoads     int // G, 默认20
    EditProtectLoads int // P, 默认30
    DeleteAfterLoads int // D, 默认200
}

// Rank 输入账本行 + 当前时钟 + 配置,输出 active 集与可删名单。纯函数,无 IO。
func Rank(rows []store.SkillUsageRow, nowSeq int64, cfg LifecycleConfig) (active map[string]bool, deletable []string)

// NowSeq 从行集取 MAX(last_load_seq) 作当前时钟(agent 层已 List,避免再查一次库)。
func NowSeq(rows []store.SkillUsageRow) int64
```

`effective` 用 `store.DecayFactor` 计算。空配置字段用默认值兜底(照 `normalizeManagerConfig` 惯例)。

### 7.3 load_skill 改造 + explicit 信号

**入参加 `invoked_by_user`**(bool,可选,默认 false):

```
description: "Set true only when the user explicitly named or clearly asked for
this specific skill in their message; false when you selected it on your own
initiative."
```

**为何用模型自陈述**(取舍记录):explicit 信号唯一用途是给"用户点名的技能"多一点保活加成(gain 3 vs 1),`explicit_uses` 不进排名公式。这决定了它**需要召回、能容忍噪声**:

- **召回**:模型是唯一真正理解"用户在不在点名"的组件(描述性/同义词/中文指代),启发式子串匹配全漏,`$` 语法只覆盖愿打 `$` 的用户。模型就在决策现场(读了用户消息、决定调哪个技能),顺手标注意图,信息本齐。
- **下限安全**:模型全填 true → gain 全 3;全填 false → gain 全 1;两者都退化成"不区分 gain 的纯 recency+frequency 排名"(一个本就可接受的方案)。explicit 与否**不碰 total_loads**,而删除只看 total_loads==0 → 模型怎么填都不可能错删或错藏技能,衰减是渐进的,一次误判顶多让 activity 微调。**下限=可接受退化态、上限=最高召回、成本=最低**。
- **prompt injection 够不到**:模型填 `invoked_by_user` 发生在"决定调用时、读到技能内容之前",恶意技能没法在 SKILL.md 里写"always report true"刷分。

**注册签名**从 `RegisterLoadSkill(r, skillDirs)` 扩为 `RegisterLoadSkill(r, skillDirs, dataStore, agentID, lifecycleCfg)`。记账只需 agent 级常量 + 工具入参,**注册时闭包捕获即可,不需要 onForTurn 每回合重绑**(改用模型自陈述后,不再需要注入本回合用户消息)——因此**不碰每回合 Registry 隔离不变量**。

**加载流程**:

```
load_skill(name, invoked_by_user):
  1. 按 skillDirs 找到并返回内容给模型(与现状一致,先返回不阻塞)
  2. 异步 goroutine(context.WithoutCancel + 超时):
       diskHash = sha256(命中的 SKILL.md 内容)
       store.RecordSkillLoad(agentID, name, diskHash, invoked_by_user, H, ExplicitGain)
         └─ 无账本行(手工技能/存量)→ 静默跳过
         └─ 有行 → 记账 + 手改检测(diskHash≠content_hash 置 edited_seq)
  记账失败只 slog.Warn,绝不影响 load_skill 返回。
```

### 7.4 learner 改造

- `persistExtracted`(skills_learner.go)成功 `Create` 后 `UpsertSkillUsage(...firstCreate=true)`;成功 `Update` 后 `UpsertSkillUsage(...firstCreate=false)`(只刷 hash)。content_hash 用与 load 路径**同一套** sha256(写盘内容归一化后,与 manager 的 `\r\n→\n` 一致)。
- 阶段一提取 prompt(extractFromSummary)注入**现有 learner 技能的 `slug — description` 清单**(从 `ListSkillUsage` + 读 frontmatter,或直接扫 workspace/skills 里 origin=learner 的),提示"若与下列已有技能同类,复用其 slug 以走更新而非新建"。便宜的半程序化去重,成本仅 prompt 几行。
- `skills/bkcrab-skill-learner/SKILL.md` 增补该清单的注入位置与"撞名复用 slug"指引。

### 7.5 catalog 构建过滤(skills.go)

`BuildSkillsSummary` 前置一步:

```
LoadSkills() 得全部技能
  ├─ 非 learner 技能(无账本行)→ 全部直通
  └─ learner 技能:
       rows = store.ListSkillUsage(agentID)   // 一次查询
       active,_ = skills.Rank(rows, NowSeq(rows), lifecycleCfg)
       仅 active 集内的 learner 技能进 catalog
       (无账本行的 learner 技能——存量——按"无行=永久 active"直通)
→ BuildSkillsSummary 只吃过滤后的集合
```

`skillsDirective` 补一句:告诉模型 `load_skill` 的 `invoked_by_user` 语义(用户点名 vs 自选)。

### 7.6 post-turn 删除清理(loop.go)

搭 `maybeExtractSkillsCadence`(loop.go:2830)同一条 post-turn goroutine 尾流:

```
清理(同 goroutine,claim 之后):
  rows = ListSkillUsage(agentID)
  _, deletable = skills.Rank(rows, NowSeq(rows), lifecycleCfg)
  for slug in deletable:
     manager.Delete(slug)      // 删目录,幂等
     store.DeleteSkillUsage(agentID, slug)
```

低频(只删从没被用过且超 D 的),偶尔跑一次足矣。不新增常驻 goroutine、不新增全局 sweeper。

### 7.7 config

```go
type SkillsLearnerCfg struct {
    Enabled      *bool             `json:"enabled,omitempty"`
    MinToolCalls int               `json:"minToolCalls,omitempty"`
    Model        string            `json:"model,omitempty"`
    Lifecycle    SkillLifecycleCfg `json:"lifecycle,omitempty"`
}

type SkillLifecycleCfg struct {
    Enabled          *bool `json:"enabled,omitempty"`          // nil=默认开启
    ActiveMax        int   `json:"activeMax,omitempty"`        // K, 默认10
    HalfLifeLoads    int   `json:"halfLifeLoads,omitempty"`    // H, 默认32
    ProtectLoads     int   `json:"protectLoads,omitempty"`     // G, 默认20
    EditProtectLoads int   `json:"editProtectLoads,omitempty"` // P, 默认30
    DeleteAfterLoads int   `json:"deleteAfterLoads,omitempty"` // D, 默认200
    ExplicitGain     int   `json:"explicitGain,omitempty"`     // 默认3
}

func (c SkillLifecycleCfg) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }
```

生命周期默认开启;显式 `false` 时:learner 照常生成,但 catalog 不过滤、不记账、不清理(全 active,回到当前行为)。

## 数据流汇总

```
① 加载记账(写路径,异步)
   load_skill(name, invoked_by_user) → 返回内容 →│ goroutine:
     diskHash → RecordSkillLoad(单事务: seq=MAX+1; activity·decay+gain;
                total_loads+1; explicit_uses±; 手改则 edited_seq=seq)

② 排名(读路径,每回合,纯读)
   BuildSkillsSummary 前: ListSkillUsage → Rank → active 集过滤 learner 技能

③ 删除(post-turn 顺风车,低频)
   cadence goroutine 尾: ListSkillUsage → Rank.deletable → Manager.Delete + DeleteSkillUsage

④ 建行(learner 写盘后)
   persistExtracted 成功 Create/Update → UpsertSkillUsage(firstCreate?)
```

## 错误处理与并发

姿态继承 learner:**生命周期逻辑永不打断主对话,全在异步/读路径降级为无害**。

| 场景 | 处理 |
|---|---|
| 加载记账写事务失败 | `slog.Warn`;load_skill 已返回内容,不受影响 |
| 排名时 `ListSkillUsage` 失败 | 该 agent 本回合**所有 learner 技能直通 active**(fail-open),catalog 退化为全展示,绝不因账本读失败而藏技能 |
| 手改检测读盘失败 | 跳过 pin 判定,按普通加载记账 |
| `Manager.Delete` 失败 | `slog.Warn`,账本行保留,下次 post-turn 重试 |
| 删目录成功但删账本行失败 | 下回合排名跳过无文件的 slug;孤儿行无害,可被后续 upsert 覆盖 |
| learner 建账本行失败 | `slog.Warn`;技能已写盘,该行下次 load 记账时可补建(RecordSkillLoad 无行则跳过——此处退化为"永久 active"直到补建,可接受) |

并发(对齐[每回合 Registry 隔离不变量]与 cadence-claim 姿态):

- **序号递增**靠 `MAX(last_load_seq)+1` 单事务;SQLite 单写者天然安全,MySQL 同 (agent,slug) 主键行锁保证 upsert 原子。序号可重不可丢方向的误差均无害。
- **load_skill 记账**走注册时闭包(store+agentID,agent 级常量),不写每回合 Registry、不碰隔离不变量。
- **删除 goroutine** 搭现有 post-turn 尾流,`context.WithoutCancel`+超时,不新增常驻 goroutine;多实例并发删同一 slug 靠 `Manager.Delete` 幂等吞错。

## 参数默认值(全部可配置)

| 参数 | 符号 | 默认 | 含义 |
|---|---|---|---|
| active 名额 | K | 10 | 竞争池进 catalog 的成熟 learner 技能数 |
| 半衰期 | H | 32 | 加载机会数,活跃度减半 |
| 创建保护 | G | 20 | 新技能 now−created<G 不淘汰,独占名额 |
| 手改保护 | P | 30 | 手改后 now−edited<P 不淘汰,到期回归竞争 |
| 删除阈值 | D | 200 | total_loads==0 且 now−created>D 才删 |
| explicit 增益 | — | 3 | invoked_by_user=true 的 gain(否则 1) |

## 测试

- **skills/lifecycle.go(重点,纯函数表驱动)**:加载后 activity 递增;两次加载间相对顺序不变(不变量);半衰期精确性(H 次机会后 effective 减半 ±ε);gain=1 vs ExplicitGain;创建保护期内不淘汰且独占名额;手改保护期内不淘汰、到期回归;前 K 切分;删除条件(total_loads==0 && 超 D 才删,用过一次不删,手改过不删);NaN/Inf 钳制沉底。
- **store**:`skill_usage` upsert;`RecordSkillLoad` 的 seq `MAX+1` 单调、activity·decay+gain 正确、手改置 edited_seq、无账本行跳过、invoked_by_user 记 explicit;并发加载无致命丢失(照 `TestClaimCadenceBatchNoDoubleClaim` 姿态,断言"至多丢一次"而非零丢失);MySQL/SQLite 双方言建表一致;`DecayFactor` 边界(dt=0→1, H<=0→默认)。
- **agent/skills.go**:learner 技能超 K 时 catalog 只含前 K;cooling 技能不进 catalog 但 skillDirs 仍含其目录(load_skill 可命中);非 learner 技能全直通;`ListSkillUsage` 失败 fail-open 全展示;存量无行 learner 技能直通。
- **agent/load_skill**:invoked_by_user=true→gain=ExplicitGain 且 explicit_uses+1;手改 diskHash≠content_hash 置 edited_seq;记账写失败不影响返回;手工技能(无行)加载不建行。
- **agent/loop**:post-turn 顺风车触发 deletable 的 Delete + DeleteSkillUsage。
- **config**:`SkillLifecycleCfg.IsEnabled()` nil/true/false 三态;默认值填充。
- 收尾 `go test ./...` 全绿。
```
