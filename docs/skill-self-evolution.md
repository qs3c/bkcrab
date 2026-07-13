# Skill 自动提炼（自进化）：Canonical 设计与当前实现

> 状态：当前实现基线（2026-07-13）
> 适用范围：`main` 与 `learner-skill-asset-isolation` 多批次改动收敛后的统一语义。旧的 hardening、lifecycle、`skill_manage`、asset isolation 计划文档只保留历史背景；发生冲突时以本文和代码测试为准。

## 1. 结论

当前实现已经与目标设计的四个核心方向基本一致：

1. 主动线路只允许 owner 查看并更新已有 learner skill；不暴露 create/delete。新 skill 只由后台节拍线路创建。
2. 节拍线路按同一 session 内已完成 turn 的工具调用数累计，默认累计到 10 次后冻结完整 `sessions.messages`，再由有界 LLM 工具循环决定 skip、update 或 create。
3. 所有模型侧 learner skill 变更都通过受能力约束的 `skill_manage`，底层统一使用专属 `skills.Manager`；learner 资产与普通、安装、个人 skill 完全分目录、分对象存储前缀。
4. learner skill 是 agent 资产。只有 agent owner 能触发提炼和维护；使用该 agent 的其他用户能加载这些资产，但不能影响提炼或生命周期账本。
5. 生命周期具有 active catalog 上限、持久资产上限、衰减排序、保护窗口和自动清理，并使用 agent 级单调时钟，不依赖 Pod 本地计数。

实现还保留三类需要继续决策或强化的边界：删除 session 时是否取消已经冻结的后台 job、终态 job/receipt 的保留周期，以及 host `exec` 对 learner 目录的强隔离必须依靠沙箱挂载边界。这些不改变上述主流程语义，详见“后续工作”。

## 2. 术语与不变量

- **learner skill**：只指自动提炼机制创建或更新的 skill。
- **普通 skill**：安装、手工创建、agent 自带、用户个人等非 learner 资产。
- **owner**：`agents.user_id`，不是当前承载 agent 的 UserSpace 用户，也不是任意聊天参与者。
- **foreground/主动线路**：owner 的正常直接对话回合。
- **cadence/节拍线路**：回合完成后，按持久化 checkpoint 异步执行的提炼 job。
- **catalog exposure**：agent 模式系统 prompt 向 owner 暴露一次可用 skill catalog；它是生命周期时钟的一个单位。

必须长期保持的不变量：

- 前台不能 create 或 delete learner skill。
- 后台 cadence 不能 delete learner skill；一次 job 最多成功写一次。
- lifecycle 是唯一能自动 delete learner skill 的内部调用方。
- learner skill 的模型写入口只有 `skill_manage`，不能通过通用文件工具绕过。
- learner skill 的所有权键是 `agent_id`；它不属于触发提炼的某个 session 或某个访问者。
- checkpoint 只在 job 成功完成或明确终态失败时前移；普通可重试失败不能吞掉窗口。
- 一旦生成 durable mutation receipt，后续恢复只重放该意图，不能再次把会话交给模型选择第二个意图。

## 3. 触发时机与触发方式

### 3.1 主动线路

owner 的直接用户回合会在系统 prompt 中得到 learner skill 维护指令。模型只有在实际使用某个 learner skill 后发现可复现的错误、遗漏或低效步骤时，才应：

1. `list`/`read` 找到已有 skill；
2. 基于完整旧内容合并修正；
3. 使用刚才 `read` 返回的 `content_hash` 作为 `expected_hash` 调用 `update`。

主动线路的能力是 `list + read + update`。create/delete 不在工具 schema 中；即使模型伪造调用，executor 仍会拒绝。非 owner、群聊、系统事件、公共 agent 的访客都看不到该工具，也不会收到维护指令。

这条线路没有单 turn 自动创建的回退路径。缺少持久化 store 或 turn anchor 时，cadence 创建直接关闭，而不是退回“本 turn 工具调用达到阈值就创建”。

### 3.2 节拍线路

默认阈值是 `skillsLearner.minToolCalls = 10`，可配置。计数范围是：

- 同一个 `owner_user_id + agent_id + session_key + chatter_user_id`；
- `turn_status='done'`；
- 位于 checkpoint 的 `consumed_through_seq` 之后；
- 对这些 turn 的 `tool_call_count` 求和，不设历史 32 turn 的截断。

当前 owner gate 比“只考虑 web 单用户”更严格：只有经过身份解析的 agent owner 直接用户会话会启动 cadence；group session 不启动，以免完整 session 快照混入其他参与者的消息。

达到阈值时，enqueue 事务会：

1. 锁定对应的 `sessions` 行；
2. 确认观测范围内没有 running turn；
3. 复制完整 `sessions.messages`，计算 SHA-256；
4. 以最新 completed turn 的 `seq` 作为 `through_seq`；
5. 只在该最新 turn 上写 `skill_extraction_id` 标记；
6. 创建一个 pending job，并把 checkpoint 指向它。

因此提炼素材是冻结时刻的会话全景，而不是最近几个 turn。后续 session 被压缩、继续对话或修改，都不会改变该 job 的输入。

### 3.3 提炼执行形态

当前选择的是一个最多 4 轮的有界工具循环，而不是单次裸 JSON，也不是通用子 agent：

- 工具能力：`list + read + create + update`；
- 每个 job 最多一次成功 mutation；
- 模型可判断没有价值而不调用写工具，结果为 `skip`；
- 相关 workflow 已存在时应先 read，再 CAS update；
- 新 workflow 确实独立、可复用且有成功证据时才 create；
- 校验错误可作为工具结果反馈给模型修正；
- durable intent 之后的基础设施错误不反馈给模型，而是立即退出并由 worker 恢复。

如果 4 轮都只有无效调用且没有成功 mutation，结果是 `validation_exhausted`。内容相同的 update 是结构化 no-op，不计 mutation，也不写账本。

### 3.4 Job 与 receipt 状态机

```text
completed turns
      |
      v
checkpoint --threshold reached--> pending job --lease--> running
                                              |
                         no durable receipt --+--> LLM
                                              |      | skip
                                              |      +------> completed
                                              |      |
                                              |      + prepare immutable intent
                                              |                |
                                              v                v
                                      prepared receipt --> reconcile asset
                                                            |       |
                                                            |       + divergence
                                                            |             |
                                                            v             v
                                                     applied receipt   conflict receipt
                                                            |             |
                                                            v             v
                                                        completed       failed
```

普通 provider/store/object-store 故障采用 1、2、4、8 分钟退避，最多 5 次。没有 receipt 的 job 达到上限会以 `retry_exhausted` 终态释放 checkpoint；已有 prepared/applied receipt 的 job 不受该上限截断，必须继续完成重放或明确进入 conflict。

receipt 把数据库外的文件/对象存储写入变成可恢复的 outbox：

- `prepared`：已持久化唯一 action、slug、before/after hash 和期望内容；原会话快照立即清空。
- `applied`：本地/远端资产已达到 after hash，且生命周期账本与 receipt 在一个数据库事务中提交；期望内容清空。
- `conflict`：权威资产已出现不满足 `absent -> after` 或 `before -> after` 的分叉；不覆盖，不写账本，清空期望内容并让 job 失败。

worker 启动后先读 receipt：applied 直接完成 job，prepared 直接恢复，不存在 receipt 才校验快照 SHA 并调用模型。这覆盖了进程在本地 rename、对象存储 PUT、账本提交或 job complete 之间崩溃的窗口。

## 4. 专属管理工具

### 4.1 能力矩阵

| 调用上下文 | list | read | create | update | delete |
|---|---:|---:|---:|---:|---:|
| owner 前台回合 | 是 | 是 | 否 | 是 | 否 |
| cadence 提炼 job | 是 | 是 | 是 | 是 | 否 |
| lifecycle 内部清理 | 否 | 否 | 否 | 否 | 是 |
| 非 owner/访客 | 否 | 否 | 否 | 否 | 否 |

schema 投影和 executor 都检查 capability，避免只隐藏 schema 却能伪造 action。`skill_manage` 是保留 builtin 名称，不能被插件或 MCP 覆盖。

### 4.2 Manager 边界

`skill_manage` 只接受 root 为专用 `learner-skills` 的 `skills.Manager`。Manager 负责：

- slug、frontmatter、大小和内容结构校验；
- 安全/隐私扫描；
- CRLF 到 LF 的规范化；
- 临时文件 + rename 的原子写；
- 只管理 `<slug>/SKILL.md`，不触碰普通 skill 目录。

create/update/delete 还经过：

- agent 级跨 Pod 可续租 lease；
- slug 级本地锁；
- agent 删除 tombstone 检查；
- update 的本地与 agent-global ledger 双重 hash CAS；
- create 的全局精确内容去重和 `assetMax` 容量检查；
- 对象存储同步；
- foreground 的失败回滚，或 cadence 的 durable receipt 恢复。

通用 read/write/edit/list/apply-patch 文件工具会拒绝解析到 learner root 的路径，包括规范化后的 symlink 路径。host `exec` 目前只有显式路径检测，不是完整安全边界；变量、glob 或运行时创建的链接无法靠字符串检查完全封堵。生产环境要把 agent home/learner root 排除在 host executor 挂载之外，或强制使用隔离沙箱。

## 5. 资产归属与隔离

### 5.1 所有权与使用权

- learner skill 归 `agent_id` 所有，由 agent owner 使用过程中产生。
- 只有 `agents.user_id` 对应的 owner 能触发 cadence 或看到前台 `skill_manage`。
- 使用公共/他人 agent 的访客不会触发提炼，不会更新 lifecycle load 账本，也不会推动 owner 的生命周期时钟。
- owner 和访客都能在该 agent 的 skill catalog 中看到并通过 `load_skill` 使用 owner 的 learner skill。
- guest UserSpace 使用 agent owner 的 lifecycle policy 来决定 catalog 展示，但无权改变该 policy 或资产状态。

### 5.2 存储布局

本地：

```text
<agent-home>/learner-skills/<slug>/SKILL.md
```

对象存储：

```text
<agent-id>/learner-skills/.namespace
<agent-id>/learner-skills/<slug>/SKILL.md
```

`.namespace` 标记用于区分“远端 learner namespace 尚未初始化”和“已初始化但当前为空”。删除最后一个 skill 后保留标记，使其他 Pod 下次 hydrate 时能删除本地陈旧副本，而不是把它重新上传。

普通 agent/global/user/安装 skill 继续位于各自的 `skills` 路径。发生同 slug 时，显式维护的非 learner skill 优先，learner 不覆盖它。`bkcrab-skill-learner` 只是提炼策略资产；运行时 cadence prompt 使用编译进程序的 canonical 版本，不允许 agent/manual/learner 目录里的同名文件覆盖安全策略。

旧版本曾写入普通 skills 目录的 learner 资产会在 agent 级 lease 内进行 hash 校验迁移；先 hydrate 已隔离 namespace，再迁移 legacy，最后 hydrate 普通 skill。即使后来关闭 learner 提炼，已有 legacy learner 仍会迁入隔离目录，以免继续污染普通资产。

### 5.3 删除一致性

agent 删除先写 durable `agent_deletions` tombstone，再删除关系数据、本地 learner root 和远端 learner namespace。`SaveAgent` 与删除共享 `agent_mutation_locks` 行，防止删除过程中同 ID 被重新创建；Manager 构建、load、mutation、migration 都会检查 tombstone。

这保证“删掉 agent 后后台 job 或旧 Pod 又把 skill 写回来”的路径 fail-closed。

## 6. 生命周期管理

### 6.1 账本与时钟

`skill_usage` 以 `(agent_id, slug)` 记录 learner 来源、创建/编辑序号、load 次数、显式 load 信号、最后 load 序号和内容 hash。`agent_skill_lifecycle` 保存 agent 级单调时钟。

时钟只在 owner 的 agent 模式 catalog 真正暴露时前进：

- 访客查看不前进；
- chatbot/customize 模式不展示 catalog，因此不前进；
- 单纯 `load_skill` 不额外推进时钟，只记录该时钟下的 load 信号。

这样生命周期年龄表示真实的“被模型看见并有机会选择”的次数，而不是 Pod 本地 turn 数或墙钟时间。

### 6.2 默认策略

| 配置 | 默认值 | 语义 |
|---|---:|---|
| `activeMax` | 10 | 最多向模型 catalog 暴露的 learner skill 数；保护项和未跟踪项也占名额 |
| `assetMax` | 50 | 持久 learner 资产硬上限；create 达到上限时拒绝 |
| `halfLifeLoads` | 32 | 活跃度指数衰减半衰期 |
| `protectLoads` | 20 | 新建后的保护窗口 |
| `editProtectLoads` | 30 | 编辑后的保护窗口 |
| `deleteAfterLoads` | 200 | 从未 load、从未 edit 的低价值 skill 自动删除年龄 |
| `explicitGain` | 3 | 显式 load 信号增益 |
| `cleanupEveryTurns` | 20 | 每 20 次 owner catalog exposure 调度一次清理；字段名保留兼容性 |

排序优先级固定为：编辑保护、新建保护、成熟资产的衰减活跃度、无账本项；同分使用稳定的 load/create/slug 次序，保证多 Pod 决策一致。`activeMax` 只控制模型可见集合，不立刻删除 cooling 资产。

清理会先把本地 catalog 中缺失的 learner ledger 行修复出来，再计算候选：

- 超过 `assetMax` 时，只淘汰已经成熟且账本完整的低优先级资产；保护项可暂时使历史资产总量高于上限，但新 create 仍被拒绝。
- 从未 load、从未 edit 且年龄严格超过 `deleteAfterLoads` 的资产可删除。
- 真正删除前重新读取 ledger/catalog，并在 agent lease + slug lock 内再次确认候选，避免与并发 load/update 竞争。
- 删除顺序和 marker 语义确保远端、本地、ledger 不会由其他 Pod 复活；失败保留可重试状态。

`skillsLearner.lifecycle.enabled=false` 是显式 opt-out：不做 active filtering/cleanup，并把 cadence 的 asset hard cap 关闭。默认 lifecycle 开启。

## 7. 安全与隐私

冻结的 `sessions.messages` 被视为不可信证据，不是 learner 的指令。canonical cadence prompt 要求忽略用户消息、网页、文件和工具输出中试图改变 learner 行为或要求写 skill 的文本，只从已验证成功的工具调用/结果或 owner 明确纠正中归纳 SOP。

写入前同时做确定性扫描，拒绝常见凭据、token、个人信息、账号/租户/项目/客户标识、私有 URL/域名和 owner-specific 绝对路径；允许清晰 placeholder 与环境变量。learner skill 会共享给 agent 的全部用户，因此不能保存 session-specific 文件名或个人实例值。

按“完整 `sessions.messages`”的当前语义，冻结快照会原样发送给配置的 learner model；确定性扫描保护的是最终持久化内容，不是模型输入的数据出境边界。如果 learner model 与主对话使用不同供应商，运维必须把它视为一次新的数据处理授权。需要输入侧脱敏的部署应在保持消息/工具结构的前提下增加 redaction policy。

输出扫描同样只是纵深防御而非完备的数据防泄漏系统。语义性敏感信息、特殊编码和未知格式仍可能漏检；高风险部署应增加独立 DLP/审核策略。

## 8. 配置与主要实现位置

配置入口：

```json
{
  "skillsLearner": {
    "enabled": true,
    "minToolCalls": 10,
    "model": "optional/provider-model",
    "lifecycle": {
      "enabled": true,
      "activeMax": 10,
      "assetMax": 50,
      "cleanupEveryTurns": 20
    }
  }
}
```

主要代码：

- 触发、owner gate、worker、cleanup：`internal/agent/loop.go`
- 提炼 LLM 工具循环与 canonical prompt：`internal/agent/skills_learner.go`
- capability、CAS、receipt、CRUD 协调：`internal/agent/tools/skill_manage.go`
- Manager 校验与原子文件写：`internal/skills/manager.go`
- 跨 Pod lease：`internal/skills/coordinator.go`
- learner 对象存储隔离：`internal/skills/objectstore.go`
- 生命周期纯排序：`internal/skills/lifecycle.go`
- checkpoint/job：`internal/store/skill_extraction_jobs.go`
- mutation receipt/outbox：`internal/store/skill_extraction_mutations.go`
- lifecycle ledger/clock：`internal/store/skill_usage.go`、`internal/store/skill_lifecycle.go`
- session 最终快照一致性：`internal/session/manager.go`
- 隐私扫描：`internal/privacy/scrub.go`

旧的 `ClaimSkillBatch`/`ResetSkillExtraction` 仅作为迁移兼容 helper 保留；生产 cadence 已不调用。旧的单 turn `MaybeExtract` 路径已移除。

## 9. 与原始四点构想的对照

### 9.1 触发时机以及怎么触发

符合。主动线路只 update；节拍线路同 session 跨 turn 累积，默认 10 次；使用完整 `sessions.messages`；最新 turn 标记；由模型决定 skip/update/create。实现选择有界多轮工具循环，原因是它能先 read 再 merge、接收校验反馈，同时成本和写次数可控。

### 9.2 如何编辑

符合。模型只能通过 `skill_manage` 调用 learner 专属 Manager；不同上下文有独立 capability。不存在裸 JSON 决策后由外部代码任意落盘的生产路径。

### 9.3 资产归属与隔离

符合。资产归 agent；owner 产生和维护，访客共享使用但不触发；本地和对象存储均有独立 learner namespace；删除和旧资产迁移有一致性保护。

### 9.4 生命周期管理

符合核心目标。已有 active catalog 上限、asset hard cap、衰减/保护/清理、跨 Pod 单调时钟和并发删除复核。仍需要补终态历史数据保留策略和更强的语义重复治理。

## 10. 后续工作

### P0：合并前必须持续验证

1. 在 SQLite 之外运行 MySQL 真实集成测试，覆盖 job lease、receipt、agent 删除与 `RowsAffected` 语义；若生产支持 PostgreSQL，也补同构测试。
2. 增加故障注入矩阵：prepare 后、本地 rename 后、marker 后、对象 PUT 后、receipt commit 回包丢失后、job complete 前逐点 crash。
3. 给 enqueue、retry、reconcile、conflict、create/update/no-op、cleanup 增加指标与结构化审计，便于确认没有重复提炼或卡死 checkpoint。

### P1：需要产品决策

1. **session 删除语义**：当前冻结 job 与 checkpoint 在删除 session 后仍保留并可完成，优点是删除会话不会破坏已认领工作；风险是用户删除会话后仍可能产生共享 skill。若产品期望“删除即撤回”，需要设计 `cancel_requested`、prepared update 的 before-content/补偿策略，以及对象存储半完成状态的恢复，不能只删 job 行。
2. **终态保留期**：当前 completed/failed job 和 applied/conflict receipt 长期保留。应增加按时间或每 agent 最近 N 条的 GC；绝不能清理 running/pending/prepared 或 checkpoint 正在引用的行。
3. **可见性与审核**：决定是否给 owner 提供 learner skill 的来源、最近修改原因、生命周期状态和手工“保留/删除”管理界面。前台模型仍不应获得 create/delete。
4. **访客使用信号**：当前访客可以使用 owner 的 learner skill，但其 load 不写 owner 账本，避免非 owner 操纵淘汰排序。若产品希望“被访客高频使用”也能保护资产，需要设计限流、聚合或防刷的使用信号，而不是直接放开访客写账本。
5. **learner model 数据边界**：决定是否强制与主对话同供应商/同地域，或在送入模型前对冻结快照做结构保真的敏感值脱敏；输出侧扫描不能替代这项策略。

### P2：质量与安全增强

1. exact normalized-content 去重已经确定性执行；语义近似重复目前主要依赖模型 list/read 判断。可增加离线相似度聚类和 owner 审核的 merge 建议，避免自动误合并。
2. 将 host executor 的 learner 路径隔离下沉到沙箱/挂载策略，移除对命令字符串检测的安全依赖。
3. 扩充敏感信息 corpus、编码变体和多语言 DLP 测试；必要时为高风险 agent 增加人工批准。
4. IM 单 session 多用户的 checkpoint、素材授权和 owner 消息分离明确不在当前范围；引入前需单独设计，不能直接复用 web 单用户假设。
