# Agent 上下文缓存

设置环境变量后启用；不改表、不启用 fair queue、不迁移业务数据：

```bash
BKCRAB_CONTEXT_CACHE_REDIS_URL=redis://redis:6379/1
BKCRAB_CONTEXT_CACHE_NAMESPACE=bkcrab-production
```

地址按部署修改，支持 go-redis 的 redis/rediss URL。URL 可包含凭证，不要提交真实密码。所有连接同一业务数据库的网关、管理 CLI 和后台写入进程必须配置相同 URL 和 namespace；不同业务数据库/环境使用不同 namespace。namespace 未设置时从存储类型和 DSN 派生，相同数据库使用不同凭证或地址时应显式设置。Docker Compose 已透传这两个变量；Helm 可用现有 extraEnv/Secret 注入机制。

清空 URL 并重启可关闭缓存。关闭读缓存的实例仍直接读写数据库；不要让未配置缓存的旧写入实例与已启用缓存的实例长期混跑，否则写入只能等 TTL 后被发现。

## 实际行为

- 精确身份文件、USER.md、MEMORY.md、技能生命周期记录以及完整 SessionRecord 优先读取 Redis。
- 文件 Save/Mutate/Delete 在写入前使旧缓存失效、写入后再失效。记忆工具、后台提取、管理页面共享同一 SQL 入口。
- owner fallback 先查询精确用户文件，再查 owner 的精确文件；Exact 不会继承 owner 资料。不存在和空文件可缓存，SQL 错误不缓存成空内容。
- SaveSession 先保存 MySQL，再读取已提交行回填 Redis。额外的这次写后读取保留 upsert 的真实 channel/project 信息；它没有消除写入 I/O，优化的是热请求重复读取工作集。SQL 失败不发布新缓存。
- 追加、压缩、清空、撤销、删除、项目迁移经过统一持久化入口；归档继续保存在 session_messages。Redis 丢失时从 sessions.messages 取得当前工作集，不用全部归档替代压缩结果。
- 每回合的 Memory 快照让系统提示与附加提醒共用 USER/MEMORY 读取结果。时间、请求参数、渠道提示仍按回合组装。
- 对象存储的 Agent 级 LIST 缓存减少技能刷新时的远程列举；Put/Delete/Move 后失效。缓存是存储包装器，管理界面、技能和工具使用同一实例。LocalFS 本身不使用共享 Redis 列表。
- skill_manage、后台学习和清理共用的 Skills Manager 现在同步对象存储；安装和文件工具继续通过已有同步函数失效列表。发布失败向调用方返回错误；尚无持久化发布任务。
- 技能本地同步记录 ETag/大小/修改时间，识别同长度内容更新；用临时文件替换下载结果。远程最后一个技能删除后，只移除已记录为远程同步的目录，保留本地独有目录。

## 简化与一致性边界

按 ponytail 收敛了原执行计划：没有新增 outbox、数据库 revision、技能发布事务、文件监听器或 CDC。没有缓存整个 messages 请求或 Pod 私有对象；本地技能解析和摘要组装仍执行，不引入另一个摘要缓存层。

缓存固定 60 秒过期，命中不续期；单项最多缓存 2 MiB。Redis 操作使用短超时，失败后本实例绕过缓存，等待旧数据过期。SQL 和对象存储仍是持久化来源，Redis 故障不阻止其正常读写。

加载令牌和内容保存在同一个过期 key 中：失效、淘汰、重启都会撤销旧加载器的回填资格，防止已失效的旧请求晚到后复活缓存。写入期间的读取回源；普通写入完成后新的读取看到提交结果或 miss。重叠写入可以产生额外 miss，这不是跨 Pod 执行锁，不能修复应用既有的并发工作集覆盖问题。

这是有界 TTL 的最终一致性缓存，不是 MySQL/Redis 跨库事务。局部分区或写后进程退出时，其他实例可能在已缓存项剩余 TTL 内读旧值；本实例降级不等于全局禁读。60 秒约束的是 Redis 条目的寿命，不是承诺所有外部修改都在 60 秒内完成同步。绕过应用直接改数据库、MinIO 或本地目录没有即时通知；请求回源后才发现远程变化，既有“同名本地技能不自动上传”的限制仍存在。

技能多文件上传也不保证原子发布；需要严格版本切换、外部改动即时通知或故障下更强一致性时，再实施原计划的 outbox/发布版本机制。

删除用户或 Agent 会清理该数据库的 Agent 缓存命名空间；这个少见操作使用 SCAN，不使用 FLUSHDB，也不触及 fair-queue key。若和调度 Redis 共用实例，不要为缓存改变其全局淘汰策略；逻辑 DB 不隔离内存压力。

## 验证

使用独立测试 Redis 和空白 MySQL 数据库，不能指向生产数据库。测试会运行迁移并写入测试数据。

```bash
export BKCRAB_TEST_CONTEXT_REDIS_URL=redis://127.0.0.1:16379/0
export BKCRAB_TEST_CONTEXT_MYSQL_DSN='root:TEST_PASSWORD@tcp(127.0.0.1:13306)/agentctx?parseTime=true'

go test ./internal/contextcache ./internal/store ./internal/skills \
  -run 'TestCache|TestLateFill|TestExpiry|TestContextCacheStore|TestSkillHydration' -race -count=1
go test ./internal/session ./internal/agent ./internal/workspace ./internal/gateway ./cmd/bkcrab
```

测试覆盖热读不回源、失效与迟到回填竞争、TTL 不续期、Redis 不可用、用户 Exact 隔离、owner 更新、事务失败、消息字段保留、压缩后回源、会话删除和项目变更，以及技能同长度更新/最后一个远程技能删除。没有配置测试 Redis/MySQL 时相应集成用例明确跳过。

Debug 日志记录缓存回源耗时和失败状态，不记录消息正文；Redis 降级有 warning。上线后比较上下文准备时间和 MySQL 查询量，不能把减少输入准备 I/O 等同于减少模型输入 token。
