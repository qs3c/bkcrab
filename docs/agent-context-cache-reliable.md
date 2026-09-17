# Agent 上下文共享缓存（可靠通知实现）

提供可恢复的共享上下文缓存、会话版本校验和版本化技能发布。Redis 加速默认关闭，启用后默认保留缓存 30 分钟。

## 行为

- MD/USER/MEMORY：精确作用域缓存；受控写入后立即失效。owner fallback 分开读取精确记录，不缓存混合身份。一次回合的系统提示和附加提醒共用资料快照。
- 会话：Redis 命中返回完整工作集；miss 回源 `sessions.messages`，不重放完整归档。写入先提交 SQL，再从已提交数据同步 Redis。消息正文、工具调用、多模态字段、元数据、原始助手响应及时间戳都保留。
- 会话并发：缓存返回不存在时，回源取得同一状态下的工作集与 revision（包括删除后的 tombstone），避免把新会话的 revision 赋给空消息。仅修改标题不会推进工作集 revision；消息或上下文字段变化仍推进版本。会话管理器携带持久化 revision；过期的写入被拒绝，停止继续请求模型/执行工具，要求重新加载。它不自动重放工具，也不替代跨实例执行调度。
- 技能：完整文件树上传为按 SHA-256 寻址的对象，再提交数据库中的 manifest。切换发布记录后失效清单缓存；本地按版本下载并校验 hash，通过原子 symlink 切换。技能摘要、load_skill 与沙箱使用同一回合固定的版本视图。
- 沙箱：识别已发布技能的 symlink；相同版本复用实例，下一回合版本变化时重建实例。Docker 工作区通过原挂载保留；云沙箱切换前完整保存工作区，保存失败不销毁原实例。重建会重置进程、浏览器及 shell 内存状态；独立会话不受影响。
- 技能生命周期状态：Redis 缓存 `ListSkillUsage` 的结果；写入/加载统计/删除触发通知。配置开关和环境门控仍使用当前配置，不长期缓存可变门控结果。
- 热技能加载：不重复对象存储 LIST/下载；已发布目录的解析结果按目录变化缓存。未发布的本地技能维持原有发现方式。版本目录不会在活跃回合中删除。

缓存减少上下文来源读取，并不消除消息持久化、鉴权、用户设置、目标状态和会话路由所需的 SQL，也不会减少模型输入 token。本实现不缓存整个 messages 请求、Registry、执行器或回合锁。

## 通知与一致性

迁移新增 `context_cache_changes` 与 `skill_publications`。前者兼作版本表、删除 tombstone 和可合并 outbox：每个实体只有一行，包含 revision/applied/dirty。数据库触发器在源表 INSERT/UPDATE/DELETE 的同一事务里推进 revision；业务回滚时通知一起回滚。

覆盖 `agent_files`、`sessions`、`skill_usage`、`agents`、`skill_publications`。相比在 HTTP handler 各写一次通知，统一触发器还覆盖直接 SQL 内容更新、后台任务、CLI 和旧写入进程。源记录的主键不应通过直接 SQL 原地改名；使用受控迁移/删除创建接口。

受控写入提交后同步操作 Redis，失败不撤销已提交的 SQL：记录告警并降级。后台每秒处理最多 100 个 dirty 实体；多个实例可以重复处理，同步和条件确认是幂等的。补偿读取当前版本，不重放旧事件的消息正文。dirty 索引用于定位待处理项。

Redis 单个 hash 包含随机 epoch、固定宽度 revision 和 payload。失效撤销旧回填资格；回填只允许写到原 epoch。缓存淘汰、过期、重启后，旧填充不能创建新 epoch。Lua 用定长十进制字符串比较版本，避免 64 位整数经过 double 丢精度。内容格式有 schema 校验；损坏值回源。

正常情况下写入完成同步后，下一次读取得到新值或 miss。Redis 故障时受影响实例读取 SQL；恢复后先补偿再恢复缓存读取。这是可恢复的最终一致性，不是 SQL/Redis 分布式事务：局部分区时，其他仍能访问旧 Redis 的实例可能短暂读旧数据，受补偿与固定 TTL 限制。会话写入的 SQL revision 检查仍拒绝覆盖新的工作集。Pub/Sub 不参与正确性保证。

## 技能发布与外部修改

- 安装/ZIP 上传先写临时目录，上传完整包后发布；失败不会发布半个包。
- `skill_manage`、技能学习器和技能清理器经 Manager 发布；主机和沙箱的 `write_file` / `edit_file` 对 `skills/<slug>/...` 采用副本编辑并发布，避免覆盖旧版本；直接写入已发布版本或其绝对路径会被拒绝，并提示使用相对技能路径发布。受控发布成功后，健康实例的下一个 turn 会看到新增或更新的技能，不需要等待 30 分钟 TTL；已运行的 turn 保持原有技能视图。
- legacy `skills/<slug>/...` 对象首次读取时导入。导入只创建不存在的发布记录，不能覆盖并发发布的新包；`_initialized` 标记避免每回合重新 LIST。
- 无需另建“上传中”任务：提交 manifest 前的上传失败/崩溃只留下未引用 blob；旧版本继续可用。客户端可重试，完整上传内容按 hash 复用。这与原计划中的发布任务表不同，但具有相同的“不暴露半成品”边界。
- Gateway 每 30 秒对已注册本地技能根目录进行后台内容校验，发现外部修改或新目录后发布；未完整写完 SKILL.md 的目录延后处理。任意 exec 外部编辑不承诺立即可见，也不承诺对恶意直接改写版本目录的操作保持历史字节不变。
- 直接修改 MinIO 的 legacy 路径不等于新版本发布；受控应用发布为权威入口。暂未接入 MinIO 事件或 binlog CDC。直接 SQL 更新受触发器捕获，但传播通常由后台补偿完成。
- 旧版本目录及未引用 blob 暂保留，尚未自动 GC，以保护已运行的回合。运维需要按留存策略清理；不能简单删除所有 `.versions`/`.views`。

## 配置

```dotenv
BKCRAB_CONTEXT_CACHE_ENABLED=true
BKCRAB_CONTEXT_CACHE_REDIS_ADDR=redis:6379
BKCRAB_CONTEXT_CACHE_REDIS_PASSWORD=...
BKCRAB_CONTEXT_CACHE_REDIS_DB=0
BKCRAB_CONTEXT_CACHE_PREFIX=bkcrab:agentctx:v3:
BKCRAB_CONTEXT_CACHE_TTL_SECONDS=1800
BKCRAB_CONTEXT_CACHE_TIMEOUT_MS=200
```

默认关闭；TTL 默认 1800 秒（30 分钟），超时默认 200 毫秒。读取不会延长 TTL，因此不会因持续活跃而永久保留漏失效的数据。连接参数和开关与 fair queue 独立；缓存前缀必须位于 `bkcrab:agentctx:` 下。凭证不写入日志或本说明。

### Redis Insight 中的 key

默认格式为 `bkcrab:agentctx:v3:<kind>:<sha256>`，kind 为 `session`（会话工作集）、`file`（身份/记忆文件）、`skillcatalog`（技能发布清单）、`skillstate`（技能生命周期）或 `agent`（所有者查找）。文件增加文件名层，如 `bkcrab:agentctx:v3:file:USER.md:<sha256>`；文件名使用 URL query escaping，冒号、斜杠和百分号分别编码为 `%3A`、`%2F`、`%25`，空格编码为 `+`，避免引入额外层级。技能清单增加作用域层：`skillcatalog:global:<sha256>` 对应 `_global`，`skillcatalog:user:<sha256>` 对应 `_user_<uid>`，`skillcatalog:agent:<sha256>` 对应 Agent 所有者。每个清单是该作用域的技能集合，不是单个技能；`_initialized` 是已完成导入检查的内部标记，不计为实际技能。

健康探针单独使用 `bkcrab:agentctx:v3:health`。摘要仍由完整 `[kind, s1, s2, s3]` JSON 数组计算，避免分隔符歧义，用户/会话标识不会直接出现在 key 中；摘要不是正文 hash，也不是加密保护。Hash 内的 `epoch`、`revision`、`payload` 以及失效协议不变。可以用 `bkcrab:agentctx:v3:file:MEMORY.md:*` 等模式筛选，不能仅凭大小判断缓存类型。`Found:false, Value:null` 表示缓存了该作用域下文件不存在的查询结果；正常写入会失效这个负缓存。

从 v1/v2 升级需统一停止旧版缓存读写实例，再启动使用同一 v3 前缀的新版本；不要让不同 key 格式或命名空间的实例并行消费同一 SQL outbox，因为已确认的通知不会自动在另一个命名空间重放。显式设置过 `BKCRAB_CONTEXT_CACHE_PREFIX` 的环境需同步更新。新版本冷读回源，不复用旧版内容，旧 key 自然到期即可。回滚也必须先停止新版实例并使用一个未用过的回滚前缀，或在确认无旧进程后仅清理目标上下文缓存命名空间；不能直接重用可能陈旧的旧版缓存，更不能清空调度 Redis。

生产先运行数据库迁移；关闭 AutoMigrate 的部署须提前部署 DDL，启用缓存时会检查表。新版本会话持久化依赖版本表，不能跳过迁移。此次修复还会将 sessions 的 UPDATE 触发器升级到 v2：先创建新触发器，再移除旧触发器；已有数据库也需要执行迁移。迁移账户需要创建表、索引及 TRIGGER（PostgreSQL 还需函数）权限；运行账户需要源表与通知表读写权限。MySQL 8.4 和 SQLite 有实际执行验证；PostgreSQL 生成对应触发器/函数，但本次未用 PostgreSQL 实例做集成验证。

不要对现有调度 Redis 执行 FLUSHDB/FLUSHALL，也不要为了缓存将共享实例改成可能淘汰调度 key 的策略。缓存与调度可使用独立 Redis 实例；仅分逻辑 DB 不隔离内存和淘汰。

关闭 `BKCRAB_CONTEXT_CACHE_ENABLED` 即可回到 SQL 读取；已存在的技能发布记录仍保持权威，不退回旧 legacy 文件。回滚到不支持 skill_publications 的旧二进制之前，必须导出已发布技能到旧格式并安排发布切换，不能直接降级读取旧对象。未完成通知保留供重新开启缓存时补偿。

## 验证方式与观测

`DBStore.ContextCacheStats()` 返回 hit/miss/failure/rejected/sourceReads 计数。sourceReads 统计缓存包装的数据回源，不包含写入同步、版本查询和其他业务 SQL。日志报告持久化失败与未完成同步；检查 dirty 行可判断补偿积压。此版未增加独立管理页面或 Prometheus endpoint。

实际集成测试用两个独立 DBStore/Redis 客户端访问同一数据库，模拟跨实例读取。测试依赖必须是隔离实例：

```bash
BKCRAB_CACHE_TEST_REDIS=127.0.0.1:16389 \
BKCRAB_CACHE_TEST_MYSQL='root@tcp(127.0.0.1:13369)/context_cache_test?parseTime=true' \
go test -race ./internal/store ./internal/contextcache ./internal/skills ./internal/session ./internal/agent ./internal/sandbox \
  -run 'Test(ContextCache|FillFenced|ConcurrentFill|RevisionBeyond|Publication|SessionVersion|PromptSnapshot|PublishedSkill)' -count=1
```

省略 MYSQL 变量时，集成用例使用临时 SQLite；不设置 REDIS 变量则跳过实际 Redis 测试。技能包测试使用真实临时文件系统对象存储和两个发布器，包含上传故障注入；没有对生产 MinIO 执行测试。

关键用例：热读 sourceReads 不增加；跨实例更新/删除和 Exact 隔离；事务回滚；Redis 写入失败后的 outbox 补偿；回填与失效竞争；淘汰后迟到回填；大整数 revision；会话冲突、清空和撤销；同长度技能更新、发布失败、最后一个技能删除、旧版本固定及路径校验；摘要与执行版本一致；沙箱延迟获取保留回合视图；云工作区同长度修改保存；实际 Docker 切换挂载并保留工作区。

尚未进行生产流量压测或端到端延迟对比，因此不承诺具体性能提升百分比。已确认收益是热读路径不再反复读取这些源数据；可靠通知和版本控制增加了持久化写入成本。

## 验证记录

- 全仓库 `go test ./... -run '^$'`：编译通过（不代表全仓库测试都执行）。
- store/session/contextcache/skills/config/agent/agent-tools/gateway/setup 的包内完整测试通过；sandbox 完整测试通过。
- 隔离 Redis 7.4 + MySQL 8.4 上的跨实例、故障补偿和并发测试通过 `-race`；SQLite 路径也有执行验证。
- Docker 使用本地 redis:7.4-alpine 测试镜像，验证真实容器的旧/新技能只读挂载、同版本复用及工作区保留。测试通过 `BKCRAB_CACHE_TEST_DOCKER_IMAGE` 显式开启；生产沙箱镜像、E2B/Boxlite 云端未做端到端验证。

## 合并前修复验证

回归测试覆盖：陈旧负缓存与并发创建不会丢失已有消息；不存在/删除后的会话快照拒绝后到的冲突写入，清空后并发重建的消息仍保留；活动回合改标题后仍可保存回答；旧触发器升级及重复迁移；主机和沙箱编辑技能不改变旧视图，发布失败不切换；跨实例在热缓存尚未过期时发布新增技能，下一个 turn 的摘要与执行目录均刷新；初始技能目录不存在时也会固定空视图，首次新增不会泄漏到旧 turn。

TTL 是 Redis 缓存保留时间，不限制 turn 时长。当前 turn 已固定的技能版本不会因 Redis 过期被清除；正常发布通过失效通知即时推进下一回合的可见版本。

## 单机部署整合补充

`docker-compose.context-cache.yml` 可启用独立 Redis 缓存实例（30 分钟 TTL、256 MiB 缓存上限），与调度 Redis 的淘汰空间隔离。技能发布器位于配额包装器外层，文件上传经过配额检查；每用户技能对象与其 Agent 工作区合并计量，全局技能使用单独的受限作用域。生命周期准入包装器在已有执行器上也会检查回合技能版本，换版不额外占用名额。

开启二进制日志的 MySQL 可能在普通账号创建触发器时报 1419；部署时应先用具备相应权限的迁移账号安装触发器，应用仍以普通账号运行。本次未修改全局 log_bin_trust_function_creators，也未给应用账号增加 SUPER 权限。详见 [部署记录](deployments/2026-09-14-context-cache-reliable.md) 与 [面试讲解稿](agent-context-cache-interview.md)。
