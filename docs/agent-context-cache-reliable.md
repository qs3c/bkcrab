# Agent 上下文共享缓存（可靠通知实现）

本实现位于 `codex/agent-context-cache-reliable`，从 main 的 `1a765d1` 独立开发，与 `codex/agent-context-cache` 是候选方案关系。默认关闭 Redis 加速，未更改现有服务部署。

## 行为

- MD/USER/MEMORY：精确作用域缓存；受控写入后立即失效。owner fallback 分开读取精确记录，不缓存混合身份。一次回合的系统提示和附加提醒共用资料快照。
- 会话：Redis 命中返回完整工作集；miss 回源 `sessions.messages`，不重放完整归档。写入先提交 SQL，再从已提交数据同步 Redis。消息正文、工具调用、多模态字段、元数据、原始助手响应及时间戳都保留。
- 会话并发：会话管理器携带持久化 revision；过期的写入被拒绝，停止继续请求模型/执行工具，要求重新加载。它不自动重放工具，也不替代跨实例执行调度。
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
- `skill_manage`、技能学习器和技能清理器经 Manager 发布；文件工具对已发布文件采用副本编辑，避免覆盖旧版本。
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
BKCRAB_CONTEXT_CACHE_PREFIX=bkcrab:agentctx:v1:
BKCRAB_CONTEXT_CACHE_TTL_SECONDS=600
BKCRAB_CONTEXT_CACHE_TIMEOUT_MS=200
```

默认关闭；TTL 默认 600 秒，超时默认 200 毫秒。读取不会延长 TTL，因此不会因持续活跃而永久保留漏失效的数据。连接参数和开关与 fair queue 独立；缓存前缀必须位于 `bkcrab:agentctx:` 下。凭证不写入日志或本说明。

生产先运行数据库迁移；关闭 AutoMigrate 的部署须提前部署 DDL，启用缓存时会检查表。新版本会话持久化依赖版本表，不能跳过迁移。迁移账户需要创建表、索引及 TRIGGER（PostgreSQL 还需函数）权限；运行账户需要源表与通知表读写权限。MySQL 8.4 和 SQLite 有实际执行验证；PostgreSQL 生成对应触发器/函数，但本次未用 PostgreSQL 实例做集成验证。

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

## 本分支验证记录

- 全仓库 `go test ./... -run '^$'`：编译通过（不代表全仓库测试都执行）。
- store/session/contextcache/skills/config/agent/agent-tools/gateway/setup 的包内完整测试通过；sandbox 完整测试通过。
- 隔离 Redis 7.4 + MySQL 8.4 上的跨实例、故障补偿和并发测试通过 `-race`；SQLite 路径也有执行验证。
- Docker 使用本地 redis:7.4-alpine 测试镜像，验证真实容器的旧/新技能只读挂载、同版本复用及工作区保留。测试通过 `BKCRAB_CACHE_TEST_DOCKER_IMAGE` 显式开启；生产沙箱镜像、E2B/Boxlite 云端未做端到端验证。
