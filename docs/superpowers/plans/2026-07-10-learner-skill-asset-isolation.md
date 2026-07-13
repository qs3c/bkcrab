# learner 技能资产隔离实施计划

> **已取代：** 本文记录历史实施过程，当前行为与不变量以 [Skill 自动提炼（自进化）Canonical 设计与实现](../../skill-self-evolution.md) 为准。

**目标：** 将自动提炼产生的技能建模为 agent 资产：所有使用该 agent 的用户都能加载和使用；只有 agent owner 能看到 `skill_manage`、管理 learner 技能并触发自动提炼；learner 技能与安装/手工创建的技能在本地和对象存储中完全分层。

**核心设计：** 不引入通用 `SkillService`。沿用现有 `skills.Manager`、`SkillsLoader` 和 `workspace.Store`，只增加 learner 专用目录、对象存储前缀与窄接线。`skill_manage` 仍是 learner 管理的唯一模型入口；`write_file("skills/...")`、skill-creator 和安装流程继续管理非 learner 技能，互不替代。

## 已确认的语义

- learner 技能归 agent，不归发起提炼的某个用户。
- owner 和访客都能在技能目录中看到、通过 `load_skill` 加载 learner 技能。
- 访客不能看到或执行 `skill_manage`，也不能触发任何自动提炼 LLM 调用。
- owner 可在主对话中调用 `skill_manage`；后台自动提炼也只在 owner 回合运行。
- `skill_manage` 的 create/update/read/list/delete 只作用于 learner 目录，不能修改安装技能、agent 手工技能、团队技能或个人技能。
- learner 与任一非 learner 技能同 slug 时，非 learner 技能优先；learner 不覆盖显式安装或手工维护的资产。
- `bkcrab-skill-learner` 只是后台提炼提示词，不出现在正常技能目录，也不常驻普通对话 prompt。
- learner 生命周期账本继续使用 `origin='learner'`；这一点不作为问题处理。

## 存储布局

本地：

```text
<agent-home>/skills/<slug>/...          # 安装/手工维护的 agent 技能
<agent-home>/learner-skills/<slug>/...  # 自动提炼技能，skill_manage 唯一管理目标
<bkcrab-home>/users/<uid>/skills/...    # 个人/skill-creator 技能
```

对象存储（owner 仍为 agent ID）：

```text
<agent-id>/skills/<slug>/...             # 既有 agent 技能
<agent-id>/learner-skills/<slug>/...     # learner 技能
```

## 不变量

1. owner 判定使用 `agent.user_id`，不能拿当前 UserSpace、会话 owner 或空字符串做放行条件。
2. `skill_manage` 采用双重保护：工具定义不向访客暴露；即使绕过定义直接 Execute，执行器也拒绝访客。
3. 自动提炼在领取 cadence batch、创建 goroutine 或调用 provider 之前完成 owner 判定。
   cadence 领取本身也必须按真实 owner 过滤，不能由后续 owner 回合认领先前 guest 的 turn。
4. 所有正常对话都加载 agent 的 learner 目录，不以 chatter user ID 分桶。
5. learner CRUD 成功后同步 learner 对象存储命名空间；删除和生命周期清理也同步远端。
6. 读取到的完整 `SKILL.md` 只返回给工具调用模型，不写入 Info/Debug 日志。
7. 远端同步/水合必须限制在 learner 前缀和 learner 本地根目录，不能修剪 `<agent-home>/skills`。

---

## Task 1：固定 owner 身份链路和权限谓词

**文件：**
- `internal/agent/loop.go`
- `internal/agent/manager.go`
- `internal/agent/tools/registry.go`
- 对应单元测试

- [x] 给 `Agent` 保存真实 `agentOwnerUserID`，从 `config.ResolvedAgent.UserID` 无条件装配。
- [x] 将 `Registry.SetAgentOwnerUserID(rc.UserID)` 移出 memory-store 条件分支，保证所有部署模式一致。
- [x] 将 owner 判断收敛成 fail-closed 谓词：真实 owner 和 chatter 均非空且相等才允许 learner 管理。
- [x] 明确 legacy 单用户构造器的兼容装配，由构造时显式设置 owner/chatter；不在权限函数中用空值自动放行。
- [x] 测试 owner、guest、空 owner、空 chatter、`ForTurn` 复制五种情况。

## Task 2：完整隐藏并保护 skill_manage

**文件：**
- `internal/agent/tools/registry.go`
- `internal/agent/tools/skill_manage.go`
- `internal/agent/tools/skill_manage_test.go`

- [x] `DefinitionsForMode` 对非 owner 移除 `skill_manage`，不论 prompt mode 的 builtin allowlist 是否包含它。
- [x] 执行门控覆盖 create/update/read/delete/list 全部动作，而不只是写动作。
- [x] 后台使用同一个工具执行器，但只有 owner 自动提炼路径可到达；后台仍禁 delete。
- [x] 更新注释，删除“无权限门控”的误导表述，明确这是结构化工具执行路径，不是绕过工具直接调用 Manager。
- [x] 测试 guest 不可见且五动作直调均拒绝；owner 可见且正常执行。

## Task 3：增加 learner 专用本地层和远端命名空间

**文件：**
- `internal/skills/objectstore.go`
- `internal/skills/objectstore_test.go`
- `internal/agent/skills.go`
- `internal/agent/skills_test.go`
- `internal/agent/skills_learner.go`

- [x] 在 `internal/skills` 定义统一的 `LearnerSkillsDirName`/路径 helper，禁止各处散落字符串。
- [x] 将对象存储通用内部实现参数化为 prefix，保留现有 `skills/*` API 行为。
- [x] 新增 learner 专用 Sync/Delete/Hydrate/Mirror API，固定使用 `learner-skills/*`。
- [x] `SkillsLearner` 的 Manager 根目录改为 `<agent-home>/learner-skills`。
- [x] `SkillsLoader.LoadSkills` 水合并扫描 learner 层，layer 标记为 `learner`。
- [x] learner 层优先级低于所有显式安装/手工层；`AllSkillDirs` 的搜索顺序与 catalog 合并优先级一致。
- [x] 生命周期过滤只过滤 `layer == "learner"`，不能误伤同 slug 的 agent/manual 技能。
- [x] `load_skill` 仅在实际命中 learner 目录时记录生命周期活跃度；高优先级 manual 同 slug 不能刷新 learner 账本。
- [x] 测试 owner 与 guest loader 都能看到 learner；同 slug 时 manual 胜出；两个远端前缀互不影响。

## Task 4：让 learner CRUD 持久化且保持边界

**文件：**
- `internal/agent/tools/skill_manage.go`
- `internal/agent/tools/registry.go`
- `internal/agent/skills_learner.go`
- `internal/agent/skills_lifecycle.go`（以实际清理位置为准）
- 对应测试

- [x] 给 `SkillManageDeps` 增加 learner 根目录和 `workspace.Store`，create/update 后同步 learner 前缀。
- [x] delete 先删除 learner 远端对象，再删除 learner 本地目录和账本；失败必须向调用方返回。
- [x] 后台提炼执行器接入同一远端同步依赖。
- [x] 生命周期清理删除 learner 远端对象，不能只删当前 Pod 本地副本。
- [x] 保留账本失败 best-effort 语义，但对象存储失败不能伪装成成功。
- [x] 日志只记录 action/slug/agent，不记录工具 result、content 或 arguments。

## Task 5：关闭访客自动提炼

**文件：**
- `internal/agent/loop.go`
- `internal/agent/skills_cadence_test.go`

- [x] 在 `runPostTurn` 中以本回合实际 chatter 和真实 agent owner 判定是否允许 learner。
- [x] guest 回合在 cadence claim 和 fallback `MaybeExtract` 之前立即返回。
- [x] `ClaimSkillBatch` 按 `(agent, session, owner chatter)` 过滤，只统计/认领 owner 的锚点。
- [x] 提炼素材用已认领 owner turns 的归档回放，不再读取可能混有 guest 内容的整份 `sessions.messages` 快照。
- [x] 群聊/无法可靠识别 chatter 的回合 fail-closed，不进行 learner 管理或自动提炼。
- [x] owner 回合保持既有 cadence 和无持久化 fallback 行为。
- [x] 测试 guest 不 claim、不创建后台提炼 goroutine、不调用 provider；owner 正常调用且素材不含 guest turns。
- [x] 检查流式、非流式和后台回合都经过同一门控点。

## Task 6：将 learner 提示词变成内部资源

**文件：**
- `skills/bkcrab-skill-learner/SKILL.md`
- `internal/agent/skills.go`
- 对应 metadata/loader 测试

- [x] 增加 `metadata.bkcrab.internal: true`（或等价的明确内部标志），删除 `always: true`。
- [x] 正常 `SkillsLoader` 跳过 internal skill，不加入 catalog、summary 或常驻 prompt。
- [x] `SkillsLearner.loadSkillLearnerPrompt` 仍可从内部技能文件直接读取提示词。
- [x] 测试普通对话不看到该技能，后台 learner 能读取它；缺失时 fallback prompt 仍可工作。

## Task 7：兼容旧 learner 数据

**文件：**
- learner 装配/迁移 helper（以最小依赖位置为准）
- 迁移测试

- [x] 根据 `skill_usage.origin='learner'` 识别旧 `<agent-home>/skills/<slug>` 候选。
- [x] 仅当磁盘内容 hash 与账本 `content_hash` 一致、且目录只含常规 `SKILL.md` 时，幂等迁移到 `learner-skills`；其余情况保留原位并告警。
- [x] 迁移先同步新 learner 远端前缀，再删除旧本地副本；不得迁移无 learner 账本的安装/手工技能。
- [x] 新目录已有同 slug 且内容不一致时不覆盖，记录冲突并保留旧数据。
- [x] 最终取舍：旧实现没有把 `skill_manage` 写入同步到普通远端前缀，因此不自动删除含义不明的远端 `skills/*`；只迁移可验证的本地来源，并用 namespace marker 区分“远端从未初始化”和“远端已删除至空”。

## Task 8：验证与交付

- [x] 运行 learner、loader、tools、objectstore、lifecycle 的定向测试。
- [x] 运行 `go test -race` 覆盖 owner/guest 并发回合和 loader 水合。
- [x] 运行 `go test ./... -count=1`、`go vet ./...`、`go build ./...`、`git diff --check`。
- [x] 复核日志中无完整 SKILL.md，复核 guest 工具定义和 provider 调用计数。
- [x] 更新本计划 checkbox 与最终迁移决策，准备实现总结和剩余风险。

## 验收矩阵

| 场景 | learner 可加载 | skill_manage 可见/可执行 | 自动提炼 | 写入位置 |
|---|---:|---:|---:|---|
| owner 使用自己的 agent | 是 | 是 | 是 | agent/learner-skills |
| guest 使用他人的 agent | 是 | 否 | 否 | 不允许 |
| 用户用 skill-creator 创建个人技能 | 是 | 不相关 | 不相关 | users/<uid>/skills |
| owner 安装/手工创建 agent 技能 | 是 | 不相关 | 不相关 | agent/skills |
| learner 与 manual 同 slug | manual 生效 | 仅能管理 learner 副本 | owner only | 两目录并存 |

## 明确不做

- 不引入统一、泛化的 `SkillService`。
- 不禁止 `write_file("skills/...")`、skill-creator 或现有安装工具。
- 不把 learner 技能改成用户私产，也不按 chatter user ID 分桶。
- 不改变 `skill_usage` 的 agent 级主键和 `origin='learner'` 生命周期语义。
