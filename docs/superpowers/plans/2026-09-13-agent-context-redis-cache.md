# Agent 上下文 Redis 缓存执行计划

状态：原始完整设计。2026-09-13 用户指定 ponytail 后，首版实施收敛为共享读缓存、写入前后失效、会话写后回填、技能对象列表缓存和 60 秒固定 TTL；未引入 outbox/revision/技能发布系统。实际实现与边界见 [Agent 上下文缓存](../../agent-context-cache.md)。下文保留为需要更强一致性时的扩展计划，不代表全部已实现。

## 1. 目标与边界

用户近期连续使用 Agent 时，组装模型请求所需的身份文件、用户记忆、技能目录、会话工作集优先从 Redis 取得，减少重复 MySQL 查询、对象存储 LIST 和本地扫描。每回合仍组装动态提示和消息数组；不缓存整个 provider 请求，也不缓存回合私有 Registry、执行器、ctx、锁。

- MySQL 仍是身份/记忆文件、会话消息和回合状态的持久化来源。
- 技能包文件以已发布的对象存储版本为共享来源；本地磁盘保留执行所需文件。无对象存储的本地模式保留兼容路径。
- MD/记忆更新：提交持久化后，主动失效相应缓存，下一次读取回源。
- 会话工作集更新：先成功保存 MySQL，再同步更新 Redis；缓存不存在、过期或不可用时从 `sessions.messages` 恢复。
- Redis 配置与 fair queue 调度配置分离。可以使用同一服务地址，但 key namespace、连接生命周期、开关必须独立；不得清理调度 Redis key。缓存淘汰策略不能破坏同实例的调度状态，必要时独立部署实例，仅分逻辑 DB 并不隔离内存压力。
- 热读目标是“这些上下文数据不重复回源”，不是“一次聊天完全没有 MySQL 操作”：鉴权、回合锚点、消息持久化、目标状态等仍可能访问 MySQL。

## 2. 已核实的入口

| 职责 | 当前代码 | 需要改变的行为 |
|---|---|---|
| 组装主循环输入 | `internal/agent/loop.go`：HandleMessage、HandleMessageStream、buildRequestOverhead、refreshSkillsFromStore | 两条入口共用快照加载器；计划模式也保持文件失效一致性 |
| 系统提示 | `internal/agent/context.go`：BuildSystemPromptAs、loadFileForUser | 将资料读取与字符串渲染分开；回合内不重复读 USER/MEMORY |
| 会话加载 | `internal/session/manager.go`：getByKey | 当前命中 Session 对象后仍调用 GetSession；将其导向共享缓存，不能直接信任 Pod 本地旧对象 |
| 会话持久化 | 同文件：Append、AppendTurnAnchor、ReplaceMessages、Clear、Undo | 所有修改采用统一持久化/缓存语义；Undo 当前仅 rewriteFile，要单独修正存储模式行为 |
| 会话存储适配 | `internal/session/store_adapter.go` | 保存完整消息字段，传播写入错误与 revision |
| 会话 SQL | `internal/store/database.go`：GetSession、SaveSession、DeleteSession、MoveSession、RenameSession | 修订号、持久化通知，以及对应缓存更新/失效 |
| 文件 SQL | 同文件：GetAgentFile、GetAgentFileExact、SaveAgentFile、MutateAgentFile、DeleteAgentFile | 在共享读写边界接入缓存，覆盖所有调用者而非只改 loop |
| 记忆适配 | `internal/agent/memory_store_adapter.go` | memory 工具和后台提取最终进入 Save/MutateAgentFile；保留 Exact 读取隔离 |
| 技能加载 | `internal/agent/skills.go`：LoadSkills、FilterActive、BuildSkillsSummary | 按版本复用目录和解析结果；生命周期状态也属于依赖 |
| 技能同步 | `internal/skills/objectstore.go`：SyncSkillUp、DeleteSkillUp、MirrorSkillsUp、HydrateSkillsDown | 发布完成后通知；使用 manifest/hash，不用文件大小判断内容相等 |
| 技能写入 | `internal/setup/skill_install.go`、`handlers_skills.go`；`internal/agent/tools/file.go`、`skill_manage.go`；`skills_learner.go`；loop 中技能清理 | 统一发布入口，补齐本地 Manager 写入尚未发布的路径 |
| 技能状态 | `internal/store/skill_usage.go`：UpsertSkillUsage、RecordSkillLoad、DeleteSkillUsage | 更新生命周期依赖版本 |
| 注入接线 | `internal/gateway/userspace.go` 及服务启动配置 | Agent、管理接口、后台任务使用同一缓存与通知实现 |

## 3. 读取与缓存结构

第一版采用 Redis 共享缓存 + 回合内不可变快照。暂不引入跨回合的 MD/会话 L1 缓存，避免额外依赖广播来保证各 Pod 一致。技能本地文件和解析结果必须校验依赖版本。

建议键的逻辑结构（实际实现统一编码标识符，不拼接未转义用户输入）：

| 类型 | 逻辑 key | 内容 |
|---|---|---|
| 精确文件 | `agentctx:v1:file:<agentID>:<subjectUserID>:<filename>` | revision、found、content、loadedAt |
| 会话 | `agentctx:v1:session:<ownerUserID>:<agentID>:<sessionKey>` | revision、完整 SessionRecord、loadedAt |
| 技能发布清单 | `agentctx:v1:skillmanifest:<scope>:<scopeID>` | 发布 revision、技能文件 hash/不可变对象引用 |
| 技能可见性状态 | `agentctx:v1:skillstate:<agentID>` | usage/lifecycle revision 和所需状态 |
| 派生技能目录 | `agentctx:v1:skillcatalog:<dependencyHash>` | 名称、描述、always-load 正文、来源与包版本；不共享 Pod 绝对路径 |

文件 owner fallback 先查精确用户记录，再查精确 owner 记录；不缓存丢失依赖信息的“最终合并结果”。USER.md/MEMORY.md 的 Exact 路径始终禁止 owner fallback。不存在的数据用短期 found=false 表示，不能把读取错误缓存为空文件。

Skill dependencyHash 必须覆盖全局/Agent/用户/团队及本地配置实际参与的技能来源、开关、生命周期状态、内置技能版本、解析器版本及影响可用性的环境条件。目录路径在当前 Pod 根据 scope 映射，不能从其他 Pod 复制路径。

读取流程：

1. 短超时读取 Redis；命中时校验格式、版本有效性、绝对年龄和身份作用域。
2. 未命中由同 key 的 singleflight 合并本进程请求；跨进程可用有期限的加载令牌减少击穿，Redis 错误不能阻止回源。
3. 文件/会话回源 MySQL；技能清单和包回源对象存储，生命周期回源 MySQL。
4. 条件回填 Redis：不得让较旧结果覆盖更新结果，也不得复活已删除的数据。
5. 将返回内容复制为回合私有快照，后续渲染不修改缓存对象。

TTL 初始建议：会话空闲 30 分钟，文件/目录 10 分钟，负缓存 15 秒，增加抖动；这些是待负载验证的初值。刷新空闲 TTL 不得刷新 loadedAt，另设绝对最大缓存年龄，避免不断访问使漏失效永久存活。正常更新靠主动通知，不靠等待 TTL。

## 4. 更新通知：由写入方发出

不在每次请求里重新 SELECT updated_at 检测变化。既然写入经过应用，就在持久化成功的写入边界发出变更。

### 4.1 文件和记忆

```text
管理页面 / write_file / memory 工具 / 后台记忆提取 / CLI
    → SaveAgentFile / MutateAgentFile / DeleteAgentFile
    → 同一 MySQL 事务：数据变更 + revision 推进 + outbox 事件
    → commit
    → 立即尝试 Redis 版本推进和对应 payload 失效
    → 后台补偿失败/未确认事件
```

MutateAgentFile 保留现有事务内读改写，不能改为读取 Redis 再写回 MySQL。删除也产生 revision/tombstone，不能因源行删除而把版本重置为 0。共享身份文件更新只影响引用该精确文件的快照，不扫描删除所有用户的缓存。

### 4.2 会话工作集

```text
追加消息 / 压缩 / 清空 / 撤销
    → 保存 MySQL sessions.messages + revision + outbox
    → commit
    → Redis 条件写入完整最新 SessionRecord
```

- 覆盖用户、助手、工具结果、steer、中断补齐、最终交付等所有 Append 路径。
- 一次 Append 的归档与工作集持久化失败必须可识别；当前忽略 SaveSession 错误的路径先处理，不能在 MySQL 未成功时发布 Redis 新工作集。
- 保留 ToolCalls、ContentParts、RawAssistant、Thinking、Metadata、Origin 和时间戳，避免缓存往返改变 provider 协议或历史显示。
- 普通追加仍写 `session_messages` 归档；压缩仅替换工作集，不删归档。缓存失效恢复工作集时直接读 `sessions.messages`，不擅自用全部归档重建。
- 会话删除发 tombstone；迁移项目、重命名等修改若属于缓存 SessionRecord 或路由缓存，也必须通知。
- 活跃会话映射 ResolveActiveSessionKey 和 Web triple/project 查询不属于 messages 本身。先计量，若热路径仍反复查询则加入独立缓存；覆盖 /new、删除和迁移事件，不能靠旧映射选错会话。
- Redis payload CAS 只防缓存写入乱序，不解决两个 Pod 用旧会话覆盖 MySQL。增加持久化工作集 revision 的 expectedRevision 检查；冲突必须重载/终止该写入，不能静默覆盖或自动重放有副作用的工具。跨 Pod turn 执行控制保持独立议题，不宣称本地 turnControl 已解决它。

### 4.3 技能

文件存储不能与 MySQL outbox 放在同一事务，必须有可恢复的发布过程：

1. 接受安装/更新/删除操作，登记持久化发布任务和幂等 ID。
2. 上传完整不可变技能包及 hash manifest；未完成版本不对读取者可见。
3. 通过发布任务切换到新版本，记录其对象引用、递增 scope revision，并事务写 outbox。
4. 通知 Redis 技能清单、派生目录失效；新回合按版本同步本地文件。
5. 发布失败可重试；进程重启可恢复任务。删除发布 tombstone，空技能集也必须能正确同步。

技能正文继续来自对象存储；MySQL 发布记录仅承担控制信息和可靠通知，不把脚本树搬入 sessions 或 Redis。skill_manage、SkillsLearner、清理器目前包含本地 Manager 路径，必须接入发布服务后才能移除每回合同步兜底。

运行中已经绑定的技能目录保留其包版本，避免另一回合更新时覆盖正在执行的脚本；新回合获取新版本。通过 exec/npx 在任意目录直接改文件，应用无法天然知道所有变化：注册技能目录 watcher + 后台 hash 对账处理，先确认发布完成再通知。不要把请求热路径全量扫描当长期方案。

绕过应用直接改 MySQL 或 MinIO 也不会自动进入 outbox。初版支持应用内更新立即通知；外部写入通过显式发布/刷新入口和后台对账覆盖。若以后允许任意外部数据库写入且要求立即传播，再引入 binlog CDC；MinIO 外部事件也需去重并触发重新校验，不能只收到单文件事件就暴露半个技能包。

## 5. 并发和失败语义

### 5.1 不能只做一次 DEL 或 SET

必须测试两个竞争：读 A 回源拿到旧值，写 B 提交并失效后 A 才回填；写 A 的旧 SET 因网络延迟晚于写 B 的新 SET 到达。

实现原语至少包括 `GetOrAcquireFill`、`InvalidateAtRevision`、`PutIfCurrent`。Redis 脚本原子比较源 revision 和本次加载令牌；失效推进版本并撤销旧加载资格；回填/发布只能在仍有效的 token/epoch 下执行。版本协调记录缺失、淘汰或 Redis 重启时按 cold miss 处理，重新读取持久化版本，不能接受未知时期的迟到写入。删除后的 tombstone 与协调信息也要覆盖这个规则。

不能只“比较当前 payload 的版本”：payload 刚被 DEL 时没有版本，会让旧结果复活。版本高水位/epoch 与内容的生命周期须由同一协议管理；在协议竞争测试通过前不启用缓存。

### 5.2 MySQL 已提交、Redis 更新失败

MySQL 中在同一事务记录 outbox，写入者提交后立即同步缓存，后台 worker 重试遗漏事件。事件至少包含 eventID、entityKind、完整 scope、revision、operation；会话事件不保存重复的全量历史，补偿时读取当前源版本，而非用旧事件重放旧内容。

- worker 可多副本运行，使用事务认领、租约和重试；处理幂等、允许乱序、可观测积压。
- 正常路径：写操作完成缓存同步后，后续请求看到新值或 miss。
- 故障路径：MySQL 成功不能谎称 Redis 同步成功。返回/记录 committed-but-cache-pending 状态；当前执行保留刚提交的内存快照，受影响实例回源，补偿恢复缓存。
- 保留必要的最小 revision 传播能力，使携带已知写入版本的后续读取拒绝更旧缓存。它只保证携带该条件的读取，不自动保证别的客户端。
- Redis 不可用时读取 MySQL，业务持久化照常。恢复时先检查待补偿通知/缓存 epoch，再重新开放该实例缓存；持续保留短超时、熔断和降级指标。
- 明确一致性边界：没有跨 MySQL/Redis 事务，outbox 提供可恢复的最终一致性。局部分区、写入进程提交后崩溃时，尚未得知故障的其他 Pod 可能短暂读旧值；单机熔断不构成全局禁止读取。严格要求“任意故障下任何 Pod 零陈旧读取”需额外强协调或权威版本校验，不能一边承诺完全不读 MySQL，一边声称普通 outbox 已做到强一致。

Pub/Sub 可以用于可选 L1 的快速通知，不能成为唯一可靠机制；订阅断线会丢消息。第一版的正确性不依赖每个 Pod 都收到广播。参考：https://redis.io/docs/latest/develop/pubsub/ 。

## 6. 执行拆分

### Task 1：锁定读写入口与基线

- 给文件、会话、技能准备阶段增加计数/耗时；区分 cache hit、miss、source read。
- 用现有聊天/工具测试固定语义，补充真实工作集字段往返测试。
- 覆盖 setup/CLI/后台任务/两条 Agent 入口及直接 DBStore 内部写入；盘点配置、时区、所有者变更带来的依赖。
- 交付：读写矩阵、冷热请求 source-read 基线、无敏感正文的指标。

### Task 2：缓存基础与可靠通知

- 新增独立 AgentContextCache 配置和 `internal/contextcache/`；复用已有 go-redis 依赖，不复用 fairqueue key 协议。
- 数据库迁移加入所需持久化 revision/tombstone 和 cache_outbox；兼容受支持数据库方言，缓存禁用时保持原读取路径。
- 实现条件填充/失效/更新、超时、singleflight、绝对年龄与容量策略、恢复/补偿 worker。
- 将数据与 outbox 写入放在同一事务；仅在外层装饰器“写成功后再插事件”不满足崩溃恢复要求。
- 交付：乱序、回填竞争、删除、淘汰、重启、DB rollback、Redis 故障测试通过。

### Task 3：MD/记忆读缓存与回合快照

- 在统一存储读写边界接入，而不是只包 MemoryStoreAdapter。
- 引入 PromptInputsSnapshot；BuildSystemPrompt 与 buildRequestOverhead 共用读取结果。
- 动态时间、渠道信息每回合生成，工具 Registry 仍回合私有。
- 交付：所有文件更新路径跨实例生效，Exact 与 owner fallback 隔离测试通过，热回合文件/记忆无重复 MySQL 读取。

### Task 4：会话工作集读写同步

- GetSession Redis 优先；Save/Replace/Clear/Undo/Delete 等持久化成功后同步缓存。
- 传播写入错误、工作集 revision 和乐观冲突，不能缓存未提交状态。
- 检查路由映射的剩余查询，按需纳入同一失效体系。
- 交付：第二回合可跨实例看到上一回合结果；压缩后 miss 从 sessions.messages 恢复正确工作集。

### Task 5：技能版本与发布通知

- 统一安装、文件工具、skill_manage、后台学习和清理入口；添加可恢复发布任务。
- manifest/hash 取代 size-only 判断，按版本保存本地包；原有技能平滑建立初始 manifest。
- 缓存目录/摘要及 lifecycle 依赖；后台处理未经过受控入口的本地变更。
- 交付：热回合无重复对象存储 LIST/本地全量解析，同长度改动、最后一个技能删除、跨 Pod 安装、权限变更都生效。

### Task 6：集成验收与启用

- 使用真实 Redis + MySQL、测试对象存储和两个独立服务实例做集成验证；本地 mock 不替代并发与网络故障语义测试。
- 运行受影响 package 测试以及选定的 race 测试；仅将相关改动纳入检查。
- 分文件/会话/技能三个开关启用；先只观测再灰度。回滚关闭读缓存即可回源，不能回滚丢掉未处理 outbox 或清空其他业务 Redis 数据。
- schema 变更和开关开启前需所有写入实例支持新通知协议；旧进程继续不带通知地写数据时不得承诺缓存新鲜度。发布命名空间 epoch，防止回滚后旧通知写入新缓存空间。

## 7. 验收清单

- 冷请求：回源并建立缓存；同依赖热请求：上下文文件/会话不再反复读 MySQL，技能不再每回合 LIST。
- 更新 SOUL/USER/MEMORY 后，正常同步完成的下一回合立即读取新内容；不同用户互不串资料。
- memory 工具与自动提取都能失效；删除与不存在负缓存正确处理。
- 全局/Agent/用户技能安装、同长度正文更新、删除、生命周期变更分别验证；新 Pod 缺本地文件时仍能执行技能。
- user/assistant/tool/steer 追加和中断补齐后，Redis 与 MySQL 已提交 revision 和完整工作集一致。
- 压缩、Clear、Undo、会话删除、重命名、项目迁移均不复活旧数据。
- Redis key 过期/淘汰/数据损坏时，从 sessions.messages 恢复；MySQL 失败不发布伪成功缓存。
- 读旧值回填、乱序通知、重复通知、写后进程退出、Redis 重启与局部分区有确定测试结果；故障期间陈旧窗口被记录，不伪称强一致。
- 降级时仍能完成聊天并持久化；恢复后补偿队列清空，旧 revision 无法覆盖新 revision。
- 指标展示 source-read 次数、命中率、上下文准备延迟、outbox 最大延迟、回填拒绝数、持久化失败和降级次数。性能收益用实测报告，不预设百分比。
