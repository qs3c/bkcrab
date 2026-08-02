# RAG 多用户公平调度队列设计

日期：2026-08-01
状态：修订草案（待复审，尚未实施）
前置：

- [RAG 板块设计：用户级知识库 + agent 受限检索](2026-07-13-rag-module-design.md)
- [RAG 多模态文档解析与结构化分块设计](2026-07-20-multimodal-document-rag-design.md)
- 参考实现：`E:\fromGithub\fair-scheduling-queue`

> 本设计只改变 RAG 索引任务的发布、用户公平选择、集群并发控制和崩溃恢复方式。现有 document/version snapshot、MySQL claim/lease/fence、解析、分块、DocumentAI 预算、embedding、Milvus staging/activation 和 GC 语义继续作为正确性基础。

## 1. 背景与现状

当前 RAG 异步索引已经具备持久化任务、精确 document version、数据库 lease、heartbeat、claim generation、失败重试和原子 active-version 切换，但调度仍然是进程内 worker 从 MySQL 领取“全局最老的到期任务”：

```text
上传/重建索引
  -> MySQL 创建 rag_index_tasks
  -> 进程内 wake channel
  -> worker 调用 ClaimRAGIndexTask
  -> MySQL 按 created_at,id 选择全局最老任务
  -> 执行完整索引流水线
```

该模式可以恢复崩溃任务，但不能提供用户公平性。用户 A 批量上传大量文件后，用户 B 后上传的少量文件可能长期排在 A 的任务之后；多个 bkcrab 实例还会各自启动本地 worker，缺少集群统一的全局并发额度。

本设计引入通用 `internal/fairqueue` 基础设施，并以 RAG 索引作为第一个接入资源：

- RabbitMQ 保存按资源、按用户隔离的待执行消息；
- Redis 保存活跃用户轮转环、activation generation 和有时限的并发 lease；
- MySQL 继续保存业务任务和执行正确性状态；
- scheduler 每次轮转一个用户，而不是从全局 FIFO 直接取下一条消息；
- 只有一个用户有 backlog 时允许其借用空闲容量；有竞争用户时恢复基础公平份额。

## 2. 已确认的边界与关键决策

### 2.1 目标

1. 用户是唯一 tenant 边界；`tenant_id` 永远由服务端根据 KB 所有者确定。
2. RAG 索引集群全局最多同时运行 4 个任务。
3. 每个有竞争需求的用户拥有 2 个基础并发槽。
4. 只有一个用户有待执行任务时，该用户可以借用空闲容量，最多运行 4 个任务。
5. 多用户同时有 backlog 时采用用户间 Round Robin；同一用户内部保持 RabbitMQ 的**实际入队顺序**。多 publisher、fast path、补扫和 at-least-once 重发意味着该顺序不承诺等同于 MySQL `created_at` 严格顺序，重复/旧 generation delivery 由精确 claim 丢弃。
6. 多实例共同使用 Redis 中的集群额度，并由 MySQL claim 做最终并发安全校验；不能把本地 goroutine semaphore 当作全局并发控制。
7. RabbitMQ 或 Redis 短暂故障不能丢任务；MySQL 始终是恢复依据。
8. RabbitMQ 消息允许至少一次投递；MySQL 精确 claim 和 fence 阻止重复执行产生错误结果。
9. 复用现有 `rag_index_tasks`，不新增通用 jobs 表，也不新增单独 outbox 表；只增加每 resource 一行、用于特殊恢复防误完成的通用安全 journal。
10. 通用 fairqueue 不依赖 RAG 领域类型，后续可由 imagegen 复用。

### 2.2 明确不做

- 本期不实现 imagegen batch；只固定未来接入契约。
- 不实现 SQLite/PostgreSQL 的公平队列 store 合约和集成测试；公平模式一期只支持 MySQL。
- 不实现用户权重、VIP 优先级、任务抢占或按文件页数/运行时间计费的 Weighted Fair Queue。
- 不拆分一个 RAG 文档索引任务内部的 parse/chunk/embed/index 阶段；一个文档版本仍是不可抢占的调度单元。
- 不在 Redis 故障时退化为无公平、无全局额度的继续领取；Redis 故障期间停止启动新任务。
- 不让 RabbitMQ 成为业务状态真相源，不把完整 document snapshot 或 prompt 放入消息。
- 不创建 RabbitMQ 延迟队列插件依赖；延迟重试继续由 MySQL `next_run_at` 驱动。
- 不在一期实现空闲 RabbitMQ 用户队列的自动回收策略；先保证可靠性和恢复语义。

### 2.3 关键决策

| 决策点 | 结论 |
|---|---|
| Tenant | `rag_kbs.user_id`，创建任务时冗余到 `rag_index_tasks.user_id` |
| 调度资源 | 固定资源标识 `rag.index`；仅用于配置、Rabbit routing 和 Redis key，不写入任务表 |
| 全局并发 | `globalConcurrency=4`；Redis 负责预约；MySQL 的精确 claim 与 heartbeat 共用 resource advisory lock，对有效 RUNNING 数做最终安全闸门 |
| 用户基础并发 | `perUserBaseConcurrency=2` |
| 用户突发并发 | `perUserBurstConcurrency=4`，仅在当前用户是唯一 active user 时借用 |
| 本地 worker | 每实例默认 4；只是候选执行 goroutine 数，不代表集群额度 |
| 用户轮转 | Redis LIST Round Robin；Redis SET 保持 active user 唯一性 |
| backlog 用户 | Redis SET 保存已知用户和当前有 Rabbit ready message 的 active user；Rabbit queue depth 用于校正 |
| 并发状态 | Redis ZSET 保存每个 reservation 的独立过期时间，不使用聚合计数器的一小时 TTL |
| 业务正确性 | MySQL task claim + lease + heartbeat + claim generation + index fence |
| Rabbit 语义 | publish 前声明并绑定 durable topology，`mandatory=true`、`basic.return`、publisher confirm、persistent message、at-least-once、durable DLQ |
| MySQL 发布凭据 | 当前 task execution epoch 的 `dispatched_at IS NULL`、`next_run_at` 已到期且 `dispatch_generation > claim_generation`；PENDING 与 lease 已过期的 RUNNING 都可成为候选，不建 outbox 表 |
| 延迟重试 | MySQL `next_run_at` 到期后重新发布新消息；不立即 Rabbit requeue |
| Rabbit ACK | worker 成功完成 MySQL 精确 claim并把 provisional reservation 原子 promote 后 ACK，长任务恢复依赖 MySQL lease |
| Redis 故障 | fail closed：不启动新任务；运行中任务继续；恢复后从 MySQL 重建 |
| Rabbit 故障 | 接受 MySQL 任务，保留未发布标记，后台 dispatcher 恢复后发布 |
| 数据库范围 | 一期公平模式只允许 MySQL；其他方言启用时配置校验失败 |

## 3. 术语

### 3.1 Resource

Resource 是一组共享同一调度容量的工作。RAG 第一个资源固定为：

```text
rag.index
```

它不是数据库实体，只是稳定内部标识：

- `rag` 表示业务领域；
- `index` 表示文档索引操作；
- Redis key、Rabbit exchange/routing 和配置均由该标识派生；
- 未来 imagegen 使用独立的 `image.generate`，不会占用 RAG 的 4 个槽位。

### 3.2 Active user 与 Round Robin

Active user 指 RabbitMQ 中至少还有一条 ready 消息等待调度的用户。Redis 同时保存：

```text
active ring:  有顺序的 LIST，用于决定下一轮服务谁
active users: 无顺序的 SET，即“当前 backlog 用户集合”，用于唯一性和借用判断
known users:  无顺序的 SET，记录声明过 Rabbit 用户队列的用户，供周期校正使用
```

假设 A、B、C 都有任务：

```text
初始 ring: [A, B, C]
取 A 后仍有任务: [B, C, A]
取 B 后仍有任务: [C, A, B]
```

没有 ready 消息的用户从 active users 和 ring 中移除。后续新消息确认发布时重新激活。

### 3.3 MySQL claim/lease 与 Redis inflight lease

二者职责不同：

- MySQL claim/lease 确定“哪个 worker 有权执行并提交这个 RAG task”；
- Redis inflight lease 确定“这个执行占用了多少集群并发容量”。

MySQL claim 成功时设置 `RUNNING + lease_owner + lease_until + claim_generation`。worker 通过现有 heartbeat 延长 `lease_until`；fence 丢失时取消流水线，阻止陈旧 worker 激活结果。

Redis inflight lease 是按 reservation 独立保存的临时容量许可证。scheduler 在看到 Rabbit delivery 前只能创建 provisional token；MySQL claim 成功后必须把它原子提升为由 `taskID + claimGeneration` 派生的 stable token，worker 后续只续约 stable token。worker 崩溃后只有自己的 token 到期，不会像聚合计数器一样留下无法识别的脏计数。

Redis reservation 是快速公平调度层，不单独承担最终正确性。真正把 task 从 `PENDING` 改为 `RUNNING` 或 reclaim 过期 `RUNNING` 前，MySQL 还要在一个短时、数据库级串行化临界区内统计有效 `RUNNING + lease_until > DB_NOW`：全 resource 已到 4 或当前用户已到 burst 4 时，本次 claim 返回 capacity-deferred，消息 NACK requeue 并重新激活用户。精确 claim 与所有可能把有效 lease 延长到未来的 heartbeat 必须取得同一个 resource advisory lock，并在释放锁前提交；因此 claim 的计数不会漏掉一个并发提交的 heartbeat。这一层防止 Redis reservation 过期、部分网络分区或恢复竞态造成实际 RUNNING 超发，且不需要新建额度表。

## 4. 总体架构

```text
上传/重建索引
      │
      ▼
┌──────────────────────────────────────────────┐
│ MySQL 事务                                   │
│ - document/version/index task                │
│ - task.user_id                               │
│ - status=PENDING, dispatched_at=NULL         │
└───────────────────┬──────────────────────────┘
                    │ commit 后立即尝试，失败由轮询修复
                    ▼
┌──────────────────────────────────────────────┐
│ Fair Dispatcher                              │
│ - 扫描 due + undispatched task               │
│ - Rabbit publisher confirm                   │
│ - 标记 dispatched_at                         │
│ - Redis activate(user)                       │
└───────────────────┬──────────────────────────┘
                    ▼
┌──────────────────────────────────────────────┐
│ RabbitMQ                                     │
│ exchange: bkcrab.fair.task                   │
│ queue: resource + user hash                  │
│ payload: resource/user/task type/task id     │
└───────────────────┬──────────────────────────┘
                    │
                    ▼
┌──────────────────────────────────────────────┐
│ Redis Fair Coordinator                       │
│ active ring/set + activation generation      │
│ global/user reservation ZSETs                │
│ base=2, burst=4, global=4                     │
└───────────────────┬──────────────────────────┘
                    │ reservation + delivery
                    ▼
┌──────────────────────────────────────────────┐
│ RAG Worker                                   │
│ ClaimRAGIndexTaskByID                        │
│ ACK Rabbit after claim                       │
│ existing runClaim pipeline                   │
│ MySQL heartbeat + Redis reservation renew    │
└───────────────────┬──────────────────────────┘
                    ▼
       DONE / PENDING(next_run_at) / FAILED /
       SUPERSEDED + release reservation
```

### 4.1 通用包边界

新增 `internal/fairqueue`，只认识以下通用概念：

```go
type Message struct {
    Version       int
    Resource      string
    TenantID      string
    TaskType      string
    TaskID        string
    DispatchToken DispatchToken
}

// DispatchToken 是跨重试稳定且可由领域持久状态校验的逻辑投递 epoch。
type DispatchToken struct {
    Resource   string
    TaskID     string
    Generation uint64
}

// DispatchCandidate 由领域 adapter 产生。Guard 是不进入 Rabbit 的不透明
// CAS 快照；fairqueue 只能原样交回 MarkDispatched。
type DispatchCandidate struct {
    Message Message
    Guard   string
}

// PrepareRequest 保留 transport 事实。只有 Message 非 nil 且 body/header/context
// 已互相一致时才允许执行；HeaderToken 不能被提升成可执行 Message。
type PrepareRequest struct {
    Message          *Message // only executable form
    BodyCandidate    *Message // independently parsed/shape-validated, repair-only unless Message is set
    HeaderToken      *DispatchToken // independently parsed, repair-only unless Message is set
    RegisteredResource string
    QueueTenantHash  string
    PublishAttemptID string
    RawBody          []byte // 最多 1 MiB，仅供 confirmed DLQ
    DecodeErrorCode  string
}

type ResourceConfig struct {
    Key                    string
    ValidateTaskID         func(string) bool // registered resource-specific bounded syntax
    LocalWorkers           int
    GlobalConcurrency      int
    PerUserBaseConcurrency int
    PerUserBurstConcurrency int
    BorrowEnabled          bool
}
```

通用包负责 Rabbit、Redis、dispatcher/scheduler 生命周期和恢复协调，不 import `internal/rag`。领域通过窄接口接入：

```text
DispatchSource:
  ListDispatchCandidates(after, limit) -> page, next
  GetDispatchableByID(taskID)
  MarkDispatched(candidate)

TaskPreparer:
  Prepare(request) -> PreparedTask + taskID + newClaimGeneration + disposition

PreparedTask:
  Run(ctx)

RecoverySource:
  CaptureHighWater() -> stable upper bound
  ListKnownTenants(highWater, afterUserID, limit) -> page, next
  ListDispatched(highWater, afterTaskID, limit) -> page, next
  ListValidRunning(highWater, afterTaskID, limit) -> page, next

ExpiredRearmSource:
  RearmExpiredPage(afterTaskID, limit) -> newly armed candidate page, next

BrokerRepairSource:
  CaptureRepairHighWater() -> stable upper bound
  ListBrokerBackedCandidates(highWater, afterTaskID, limit) -> candidate page, next
  RearmAfterBrokerLoss(candidate) -> new DispatchCandidate / stale-noop
```

`DispatchToken=(resource, taskID, dispatch_generation)` 是通用、稳定的逻辑投递 epoch；同 generation 的重复 publish 使用同一个 token。每次实际 AMQP publish 另生成随机 UUID/128-bit `PublishAttemptID`，写入 AMQP `message_id`，只用于将 confirm、`basic.return`、timeout、日志和 DLQ 关联到同一次 wire attempt。两者不得混用，随机 attempt ID 不能充当数据库 fence。

`Guard` 在 RAG adapter 中至少绑定 `dispatch_generation + status + claim_generation + retry_count + next_run_at + lease/due 条件`；`MarkDispatched` 必须以这些值、`dispatch_generation > claim_generation`、`dispatched_at IS NULL` 和数据库当前时间做单条 CAS。消息消费与精确 claim 也必须匹配 token generation。这样上一轮 retry/reclaim 的迟到 confirm 或 Rabbit stale delivery 不能作用于下一 dispatch epoch。

`PrepareRequest.Message` 只有在不超过 64 KiB 的 JSON body、stable headers、registered resource 与 queue tenant hash 已通过 transport 级一致性校验时才非 nil；此时 `RawBody` 必须仍能 strict-decode 为同一个 `BodyCandidate`，且不能同时携带 decode error。可独立解析且字段受限的 body 先放 `BodyCandidate`，header 独立放 `HeaderToken`：body 损坏时可用 header、header 缺失/类型坏时可用 body、两者 mismatch 时分别作为 repair locator 与 MySQL/queue context 交叉验证，但三种情况都不得返回 claimed。mismatch 时不能猜“body 或 header 谁才是真的”：对每个能独立通过 registered resource、queue tenant、MySQL canonical row、当前 generation 与 due CAS 验证的候选（最多两个）分别执行 generation repair；额外形成的发布义务由 exact claim 去重。只有 body 与 header 都无法形成受约束 locator 时才走不可定位 poison disposition。为让超过协议上限的无效消息仍能 confirmed DLQ，transport 可保留最多 1 MiB raw body；超过该保留上限的 delivery 在 transport 边界拒绝。raw body 只供 DLQ，不进入业务日志/表，也不能由 adapter 再解析来绕过 transport 校验。

Rabbit/Redis/Coordinator 边界错误必须包装稳定类别，至少区分 dependency unavailable、unsupported topology、resource not ready、resource fence mismatch、stale recovery owner、corrupt coordination state、publish unroutable 与 publish unconfirmed；scheduler/runtime 只能用 `errors.Is`/`errors.As` 分类，不能解析依赖或 Lua 错误字符串。

接口名称允许实施时按 Go 依赖方向微调，但不得让 fairqueue 查询 RAG 私有表、解释 `Guard` 或 import RAG 类型。所有 list 接口必须是有界、稳定 keyset 分页，不能一次把全表读入内存。high-water 必须在 bounded publish/prepare drain 后由 source 捕获；每个 page 显式携带它，不能只把上界留在 recovery loop 的注释里。

## 5. 最小 MySQL 数据模型

### 5.1 不新增业务任务/outbox表

本期不创建 `fair_queue_jobs`、`fair_queue_outbox` 或 `fair_queue_tenants`。调度状态在 Redis，业务状态在现有 `rag_index_tasks`。

### 5.2 `rag_index_tasks` 增量

最终模型新增：

```sql
user_id       VARCHAR(120) NOT NULL
dispatched_at DATETIME(6) NULL
dispatch_generation BIGINT NOT NULL DEFAULT 1
```

`user_id` 从任务对应文档的 KB 所有者写入。迁移现有数据时通过：

```text
rag_index_tasks.doc_id
  -> rag_documents.kb_id
  -> rag_kbs.user_id
```

迁移必须拆成可滚动的 expand/backfill/contract，不能在一次启动 migration 中直接增加 `NOT NULL`：

1. expand release 增加 nullable `user_id`、nullable `dispatched_at`、`dispatch_generation BIGINT NOT NULL DEFAULT 1` 和兼容索引；该 release 的 upgrade ALTER 与 fresh/startup CREATE 都必须保持 `user_id` nullable，DEFAULT 1 使仍省略新列的旧 INSERT 安全；
2. 部署兼容版本，所有新任务显式写 `user_id` 和 `dispatch_generation=1`，读取时仍容忍旧行的 `user_id=NULL`，且 fair mode 保持关闭；
3. 兼容版本必须在 fair mode 关闭时也 dual-write generation：legacy 成功 claim 计算 `new_generation=GREATEST(dispatch_generation,claim_generation+1)` 并把两个 generation 都设为该值，retry/reset 再建立更大的 dispatch generation；可先用同一窄 backfill API keyset 小批量回填 `user_id` 并按状态校正历史 generation（待下一次 claim 的 PENDING 至少为 `claim_generation+1`，RUNNING 未 rearm 时等于 `claim_generation`）。全部旧实例退出并给出 attestation 后，contract apply **必须再次从头运行该可中断/可重入 backfill 直到零差异**，从而收敛早期扫描后又被旧 writer 写入的行；不依赖 startup 一次性扫描或未定义的后台 loop；
4. readiness 校验 `user_id` 零 NULL、零无法解析 owner、generation 均大于 0，并保证每个可调度 PENDING 都有 `dispatch_generation > claim_generation`、每个尚未 rearm 的 RUNNING 有 `dispatch_generation = claim_generation`；
5. contract migration 在独立发布/显式运维步骤中把 `user_id` 收紧为 `NOT NULL`；禁止由旧新实例都可能执行的启动自动迁移或 fresh canonical DDL 提前 contract。只有兼容窗口关闭后的后续代码版本才可把默认 CREATE 模板改为 NOT NULL；
6. contract 完成并验证后，公平模式才具备 schema readiness。

contract/backfill 的唯一支持入口是 `bkcrab admin fairqueue contract-migrate`，默认只做 dry-run/check并输出 aggregate remaining/page count、owner/invariant 与 schema 报告，不输出原始 task-ID cursor；真正 mutation 必须显式使用 `--apply --confirm-all-writers-dual-write`。apply 先在 pinned authoritative writer 上以 bounded keyset pages 完成最终 backfill/校正，直到完整 pass 零差异，再在执行 DDL 前后重新验证零 NULL、零无法解析 owner、generation invariant、MySQL writer identity 和 schema。该命令必须使用 `AutoMigrate=false` 的专用 MySQL opener，不得借启动 gateway 顺带 contract；操作员确认代表所有旧 writer 已归零且剩余 writer 已 dual-write。中断后从头幂等收敛，重复 apply 必须幂等。

新增索引以最终 MySQL 查询计划为准，至少覆盖：

```text
(status, dispatched_at, next_run_at, id)
(status, lease_until, next_run_at, id)
(user_id, id)
(user_id, status, lease_until)
```

`(user_id,id)` 支持受 high-water 限制的 distinct known-tenant keyset scan；`(user_id,status,lease_until)` 支持每用户 valid-RUNNING 容量计数。最终列顺序可按真实数据分布调整，但必须用 real-MySQL `EXPLAIN ANALYZE`/rows-examined 证明 dispatch、expired-rearm、known/dispatched/running recovery page 与 global/per-user capacity query 都使用有界索引访问；`LIMIT` 只限制返回行数，不等于底层扫描有界。

同一个 expand release 还要创建通用、authoritative 的 `fairqueue_resource_operations` journal。它不是任务 outbox，而是每个 resource 一行的安全恢复记录，至少持久化 `resource`（主键）、随机 `operation_id`、`kind=RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD`、`phase=ACTIVE|READY_COMMITTED|COMPLETED`、当前/原/目标 writer fingerprint、repair high-water/pass、force not-before/delete-pass、CAS version 与时间戳。普通 `NORMAL` rebuild 不写该表。所有 recovery start 都先在正确的 pinned authoritative writer connection 验证 identity，并持有按 database/resource 派生的 session-level MySQL `GET_LOCK`；随后才取得并持续 renew/check 该resource的通用 Redis recovery raw lock（尚不改变control/业务状态）。MySQL session start lock是journal start读写的权威串行fence：另一start路径在它释放前不能取得raw lock或Begin。NORMAL在双锁内复查journal无未完成special才Begin；special可在start lock内CAS创建/恢复`ACTIVE`，随后必须复验raw lock仍属于本owner，才可用同一owner/operation ID Begin并最终改变业务generation。Redis TTL与MySQL CAS无法原子线性化：若进程暂停使raw lock恰在journal CAS前后过期，允许留下安全的ACTIVE record，但必须零Begin、零Redis control切换、零业务mutation，由同kind命令恢复；不得承诺“过期raw owner零journal写”。MySQL连接断开会自动释放session lock。所有启动路径锁顺序都固定为MySQL start lock→Redis raw lock，避免死锁；writer rebind在已验证的新writer取start lock，且前置已物理fence旧writer/暂停runtime。已有ACTIVE不可被另一kind/参数覆盖。Rabbit repair的high-water/pass、writer rebind的old/target、force的not-before/delete-pass以该journal为权威，Redis progress只是带fence的镜像；两者不一致一律fail closed。

operation-start lock name 必须独立于 claim/capacity lock、只由 database/resource 的固定哈希派生且小于 MySQL 64-byte 上限。callback 结束使用独立 bounded cleanup context执行恰好一次 `RELEASE_LOCK`；返回0/NULL/error或超时时物理淘汰底层connection，不能只把`sql.Conn`放回池。journal每次mutation都以完整expected resource/operation ID/version/kind/writer做CAS并返回新record；旧record或错误writer零mutation。

完成顺序也必须跨存储 fail closed：special source pass 与 journal progress 收敛后运行 Redis 通用 rebuild；全部验证通过时先 CAS journal `ACTIVE→READY_COMMITTED`，再由携同一 `operation_id` 的 Redis Finish 原子置 READY 并写 `last_completed_operation_id`，最后 best-effort CAS 为 `COMPLETED`。崩溃在前两步之间时仍保持 RECOVERING/operator-required；崩溃在 Redis Finish 之后时，`READY_COMMITTED + READY control` 只有在 operation ID、kind/参数和 writer 全匹配时才可开放 gate并补记 COMPLETED。除runtime补记外，重跑同kind CLI也必须有terminal-reconcile分支：重新验证attestation，在start fence/raw lock下看到该精确组合时只CAS journal为COMPLETED，零Begin/重建/业务mutation。Redis control/progress 全丢时，runtime 必须先查 journal：ACTIVE 或无匹配 READY control 的 READY_COMMITTED 都禁止 NORMAL rebuild，只能由重新验证全部 attestation 的同 kind operator按原 operation ID重建 RECOVERING mirror并从头幂等续跑。这样 Rabbit repair 半页后再丢 Redis 也不会遗失未扫描行。

### 5.3 存储状态与概念状态

为了不扩散新的业务 status，保留现有任务状态集合：

```text
PENDING / RUNNING / DONE / FAILED / SUPERSEDED
```

`DISPATCHED` 和 `RETRY_WAIT` 是由现有列组合表达的概念状态：

| 概念状态 | MySQL 表达 |
|---|---|
| 待立即发布 | `status=PENDING, dispatched_at IS NULL, next_run_at IS NULL OR <= DB_NOW` |
| 延迟重试等待 | `status=PENDING, dispatched_at IS NULL, next_run_at > DB_NOW` |
| 已发布等待调度 | `status=PENDING, dispatched_at IS NOT NULL` |
| 运行中 | `status=RUNNING` + 有效 MySQL lease + claimed marker 非空 |
| 崩溃待 rearm | `status=RUNNING` + lease 已过期 + `dispatch_generation = claim_generation` |
| 崩溃已 rearm 待发布 | `status=RUNNING` + lease 已过期 + `dispatch_generation > claim_generation` + `dispatched_at IS NULL` |
| 崩溃恢复消息已发布 | `status=RUNNING` + lease 已过期 + `dispatch_generation > claim_generation` + `dispatched_at IS NOT NULL` |
| maintenance 延迟等待 | `PENDING` 或 lease 已过期的 `RUNNING` + `dispatched_at IS NULL, next_run_at > DB_NOW` |
| 终态 | `DONE/FAILED/SUPERSEDED` |

`dispatched_at` 表示当前 `dispatch_generation` 的 durable publish/consume marker，不是 task 一生只写一次的标志。task 初建 `dispatch_generation=1, claim_generation=0`；每次 retry、expired-RUNNING rearm、Rabbit 灾难重置或 poison/mismatch canonical reset，都必须在一个 MySQL CAS 事务中执行 `dispatch_generation=GREATEST(dispatch_generation, claim_generation)+1, dispatched_at=NULL`。精确 claim 只接受 `message.dispatch_generation == row.dispatch_generation` 且 `row.dispatch_generation > row.claim_generation`，成功后把 `claim_generation` **直接设置为**该 dispatch generation，并写 `dispatched_at=COALESCE(dispatched_at, DB_NOW)`，封住“delivery 已被 claim、publisher confirm 的 Mark 尚未来得及落库”的竞态；不能用 `max(old+1, message generation)` 掩盖损坏的 invariant。generation 可以跳号但必须单调。heartbeat 保持该 claimed marker，不清空。由此未 rearm 的 RUNNING 满足两个 generation 相等，已 rearm/已发布 reclaim 的 RUNNING 满足 dispatch generation 更大，sweeper 不会反复使仍在 Rabbit 排队的 reclaim message 失效。

## 6. RabbitMQ 协议

### 6.1 Topology

```text
durable direct exchange: bkcrab.fair.task
durable queue:           bkcrab.fair.q.<resource>.<tenantHash>
routing key:             <resource>.<tenantHash>
delivery mode:           persistent
durable DLX/queue:        bkcrab.fair.dlx / bkcrab.fair.dlq.<registered-resource>
```

`tenantHash` 固定为小写 hex SHA-256(`resource + NUL + userID`) 的完整 64 字符，避免原始用户 ID 中的特殊字符进入 topology 名称及不可审查的短截断碰撞。消息体仍带原始 `tenant_id`，worker 必须验证它与 MySQL task 的 `user_id` 一致。任何 publish 前都必须幂等声明 exchange、目标 durable queue 及准确 binding；不能假设另一个进程已声明 topology。

### 6.2 Message

```json
{
  "version": 1,
  "resource": "rag.index",
  "tenant_id": "user_xxx",
  "task_type": "rag_index",
  "task_id": "12345",
  "dispatch_token": {
    "resource": "rag.index",
    "task_id": "12345",
    "generation": 7
  }
}
```

AMQP properties 另带随机 `message_id=<PublishAttemptID>`，并把 stable identity 冗余到固定 headers：

```text
x-bkcrab-protocol-version: int32(1)
x-bkcrab-resource:         longstr("rag.index")
x-bkcrab-task-id:          longstr("12345")
x-bkcrab-dispatch-generation: int64(7)
```

所有 fair publisher 都必须同时写 body 与这四个 header。consumer 只接受上述精确 AMQP 类型、已注册 resource、受限长度且通过该 resource 注册 validator 的 task ID 与正 generation；`rag.index` validator 接受 canonical 正十进制 ID，后续 `image.generate` 可接受受限 `imgt_...` ID，通用层不能硬编码“task ID 必须为数值”。正常执行还要求 body、header、当前 queue context 和 MySQL canonical row 全部一致。header 只是 body 无法解码时定位 canonical task 并执行 generation repair 的冗余信封，绝不能单独授权业务执行、选择 topology 或覆盖 canonical owner。若 body 与 header 都无法给出受约束且可由 MySQL 交叉验证的 identity，则该 delivery 不可能是本协议 publisher 产生的 canonical v1 消息，可以 confirmed DLQ 后 ACK。消息不保存原文、对象存储 key、document snapshot、provider key 或其它敏感载荷。

### 6.3 发布语义

1. MySQL 事务先创建 task；commit 后走一次 inline fast path，后台 dispatcher 与常驻 expired-RUNNING sweeper 周期补扫；
2. adapter 从 task 当前 `dispatch_generation` 生成稳定 `DispatchToken` 和绑定当前 epoch 的 `Guard`；publisher 为每次 wire attempt 另生成 `PublishAttemptID`；
3. publisher 幂等 declare exchange/queue/binding，在 confirm channel 上以 persistent、`mandatory=true` 发布，并用 AMQP `message_id=PublishAttemptID` 关联 `basic.return`、confirm、timeout 和 channel close；
4. 只有目标消息收到 positive confirm 且未收到 `basic.return` 才算“已路由”。Rabbit 对 unroutable 消息也可能 positive confirm，因此缺任一条件、return、nack、timeout 或 channel close 都不得写 `dispatched_at`；
5. 已路由后调用领域 `MarkDispatched(candidate)`；RAG CAS 必须同时匹配 `id`、token 的 `dispatch_generation`、`dispatch_generation > claim_generation`、`dispatched_at IS NULL`、候选的 `status/claim_generation/retry_count/next_run_at`，并重新以 `DB_NOW` 验证 PENDING due 或 RUNNING lease expired，且两种 status 的 `next_run_at` 均已到期；
6. CAS 成功后调用 epoch-gated Redis activation。CAS 失败表示消息已成为 stale/duplicate，仍执行基于 MySQL canonical tenant 的 reconciliation 提示，但绝不能把迟到 mark 写入新 retry/reclaim epoch；
7. confirm 后、MySQL mark 前崩溃会重复发布；mark 后、Redis activate 前崩溃由周期性 MySQL dispatched-tenant reconciliation 修复；
8. 任一步进程崩溃都允许重复发布，由精确 claim 去重。

Rabbit confirm 成功但 MySQL 标记前崩溃时会重复发布；这是有意接受的 at-least-once 语义，不尝试用分布式事务消除。每个 confirm tracker 在完成 success 前必须已处理 broker 保证先于 confirm 发送的 return；实现不得只等待 confirm channel。

### 6.4 ACK/NACK

- Rabbit/网络暂时错误：dispatcher 保持 MySQL 未发布状态并重试；
- worker 在 MySQL 精确 claim 成功且 provisional reservation 已原子 promote 后立即 ACK；
- task 已终态或同 epoch 的重复 delivery：ACK 丢弃，释放 Redis reservation；
- MySQL 暂时不可用、无法判断 task 状态：NACK requeue，释放 reservation；
- capacity-deferred：task 不变，NACK requeue、重新 activation 并采用有界 backoff；
- document maintenance 冲突：不得只 ACK 丢弃。领域事务把 PENDING 或 lease 已过期的 RUNNING 保持原 status，原子递增 dispatch generation、清 marker，并以 DB time 写入有界的 future `next_run_at`；随后 ACK 当前 delivery。dispatcher 与精确 claim 对两种 status 都必须服从该时间门，避免 Rabbit hot loop；
- 格式/版本/resource/task type 非法的 poison message，以及 body/header/tenant/routing 与 MySQL canonical task 不匹配的消息，都不得执行或静默 ACK。优先使用经类型/长度校验的 stable headers，并以注册 queue context 和 MySQL row 交叉验证；即使 body 完全损坏也必须能定位协议自产的 canonical v1 task。若能定位 canonical task，必须先完成下面的 generation repair；随后把有界原消息、稳定 `dispatch_token`、随机 `PublishAttemptID`、原因和哈希 tenant 以 persistent + mandatory + confirm 方式**显式发布**到 durable DLQ，repair 已提交且 DLQ confirm 成功后才 ACK 原 delivery。DLQ routing/resource 必须来自当前已注册 consumer/queue context，不能使用未验证 payload/header 创建 topology。repair 暂时失败或 DLQ 不可用时 NACK requeue；不能把 application NACK/DLX 当作已确认的 DLQ 写入；只有 body 与 stable headers 都无法定位 canonical identity 的外部/损坏消息才可在 confirmed DLQ 后直接 ACK；
- mismatch canonical repair CAS：不能任意选择 body 或 header locator；对每个独立通过 registered resource、queue tenant、MySQL canonical row、当前 generation 与 due 条件验证的候选（最多两个）都尝试 repair。只有该 locator 的消息 generation 仍等于 task 当前 generation且 task 当前仍是 due PENDING 或 expired RUNNING 时，才原子 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1、dispatched_at=NULL`，再从 MySQL canonical owner 生成新 token 走完整发布流程；若 generation 已变化、已 RUNNING-valid 或已终态则记录 no-op disposition。不得依据不可信消息改 `user_id` 或清空新 epoch 的 `dispatched_at`。同一 canonical row 被两个 locator 指向时第二个 CAS自然 no-op；两个不同合法 row 都形成新发布义务时，额外重发由 exact claim 安全去重。repair 后、DLQ ACK 前崩溃允许原 poison delivery 重现和 DLQ 重复，但不会丢失合法 task 的新发布义务。

DLQ 是 Rabbit 基础设施，不是业务表；本设计仍不增加 jobs/outbox/tenant 表。DLQ consumer、保留期和告警是运维职责，默认不会自动把 poison message 投回业务队列。

## 7. Redis 数据模型与原子操作

### 7.1 Keys

以 `rag.index` 为例：

```text
fair:{rag.index}:ring                         LIST
fair:{rag.index}:ring_members                 SET
fair:{rag.index}:active_users                 SET
fair:{rag.index}:known_users                  SET
fair:{rag.index}:activation_gen               HASH userID -> monotonically increasing generation
fair:{rag.index}:processing_turns             ZSET turnToken -> pop lease expiry
fair:{rag.index}:turn_to_tenant               HASH turnToken -> userID
fair:{rag.index}:tenant_to_turn               HASH userID -> turnToken
fair:{rag.index}:inflight                     ZSET reservation -> expiresAtMs
fair:{rag.index}:inflight:tenant:<tenantHash> ZSET reservation -> expiresAtMs
fair:{rag.index}:reservation_meta             HASH reservation -> tenant/type
fair:{rag.index}:provisional                  ZSET provisional token -> expiresAtMs
fair:{rag.index}:resource                     HASH state=RECOVERING|READY, operation_kind=NONE|NORMAL|RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD, operation_id/last_completed_operation_id, epoch=<opaque>, protocol_version, writer_fingerprint
fair:{rag.index}:recovery_lock                STRING owner/fence token + TTL
fair:{rag.index}:recovery_progress            HASH operation_kind/high_water/pass completion/convergence + operation-specific progress
```

Redis key 必须配置统一前缀，避免与同一 Redis 实例的其它应用冲突。

`resource epoch`是每次进入recovery时生成的不可预测128-bit值。每个实例从authoritative writer生成fingerprint。所有普通Lua必须接收expected epoch/writer，并仅在`state=READY && operation_kind=NONE && epoch/writer匹配`时修改；缺key、RECOVERING、special kind或identity不符都fail closed。所有recovery Lua则必须同时校验`state=RECOVERING + expected operation_kind + operation_id + epoch + recovery lock token + writer_fingerprint`；kind只可由对应Begin/Finish原子改变。special operation ID/参数还必须与 MySQL journal 精确一致，不能因 Redis key 丢失而生成新的 intent。

启动缓存只用于 Redis fence，不能证明连接池后来仍连同一 mysqld。每次 claim/heartbeat/capacity 临界区取得 pinned physical connection 后，都必须在 `GET_LOCK` 前从**同一 connection**重新读取 `@@server_uuid, DATABASE(), CONNECTION_ID()` 并与 runtime expected fingerprint/database 比较；取得 named lock 后再次验证 session identity 未变化。连接建立 hook 可提前筛错，但不能替代逐临界区验证。identity 不符或 endpoint 不具备 session affinity 时，在任何业务事务前物理 discard 该连接、关闭本地 scheduler/dispatcher gate并广播取消所有运行中 pipeline；后续 heartbeat/finalize fail closed。该错误是安全合约失败，不得按普通 Redis/Rabbit outage 让任务继续。调用方遇到 Redis/control mismatch 同样停止并告警；有意替换 writer 只能走 10.7 的运维 CAS，不能由普通重连自动 rebind。

该逐物理连接 fence 覆盖**所有 fair-mode authoritative MySQL 访问**，不只 claim/heartbeat：DispatchSource 的 list/by-ID/Mark、ExpiredRearmSource、poison canonical repair、RecoverySource 的 CaptureHighWater/每一页、BrokerRepairSource、continuous reconciliation 与所有 execution mutation 都必须经统一 expected-writer pinned read/transaction helper；读操作也要在返回 candidate/snapshot 前验证 identity。任一路径 mismatch 都返回同一 fatal safety error、物理 discard并关闭 resource gates；recovery 不能只在 Redis restore 前后验 fence，却从未经验证的新 writer读取页面后置 READY。

### 7.2 Activate Lua

publisher confirm 和 MySQL 标记成功后执行 epoch-gated 原子 activation：

```text
assert resource.state == READY
       && resource.epoch == expectedEpoch
       && resource.writer_fingerprint == expectedWriterFingerprint
SADD known_users user
HINCRBY activation_gen user 1
SADD active_users user
if user 不在 ring_members 且不在 processing:
    SADD ring_members user
    RPUSH ring user
```

重复 Rabbit publish 可能产生重复 delivery，但 MySQL 精确 claim 会拒绝重复执行。Redis 不保存业务任务数，也不把队列深度当作完成依据；它只保存“谁需要获得下一轮”的可重建调度状态。这一点尽量复刻参考项目的 active tenant ring/flag 做法。

### 7.3 Rotate/deactivate 与丢唤醒保护

scheduler 不得裸 `LPOP`。`PopForProcessing` Lua 校验 resource epoch 后，从 ring 弹出候选、同步 `SREM ring_members`，并为该 user 写入唯一 pop token 和短 processing lease；遇到历史重复 LIST 项时，只有能建立 processing ownership 的一项返回，其余丢弃。publisher 在 processing 期间只递增 activation generation，不重复入 ring。

无论取到消息与否，都查询该用户队列的 ready depth，并携带 pop token 执行 `RotateOrDeactivate(user, observedGeneration, hasReady)` Lua：

```text
assert READY/epoch/writer fingerprint 且 turn_to_tenant[popToken] == user、tenant_to_turn[user] == popToken
删除 processing_turns member 与两侧 turn 映射
if hasReady or activation_gen[user] != observedGeneration:
    SADD active_users user
    if SADD ring_members user == 1: RPUSH ring user
else:
    SREM active_users user
```

generation 用于关闭“scheduler 看到空队列”与“publisher 刚好发布新消息”之间的竞态：若期间发生新 activation，scheduler 不能删除这个用户，而要把它放回 ring。Rabbit 查询失败时保持 active 并放回 ring，不得把未知状态当作空队列。常驻 processing reaper 用同一 epoch-gated Lua 回收过期 pop lease：仍 active 的 user 通过 `ring_members` 唯一地放回 ring。由此 scheduler 在 pop 后崩溃不会永久丢 user，重复 LIST 项也不会造成同一 user 同时占多个轮次。

若 delivery 被 NACK requeue，NACK 成功后必须再次 activation。若进程在 `basic.get` 后、claim 前直接崩溃，Rabbit 会在连接关闭后自动把 unacked delivery 放回 ready queue，但进程已无法补 activation；因此每个 resource 还要运行低频 reconciliation：先遍历 Redis `known_users`，再从 MySQL keyset 分页读取**所有 task 的 distinct canonical `user_id`**补齐 known/topology（不能只看已 dispatched task），并另行分页扫描当前 PENDING/RUNNING 且 `dispatched_at IS NOT NULL` 的 canonical tenant 作为 active 候选。查询 Rabbit ready depth 后，对 depth 大于 0、却不在 ring/processing 的用户执行 `EnsureActive`。MySQL 两路分别修复 Redis 全丢/新 tenant 和 mark 成功后、Redis activate 前崩溃，不能只依赖 Redis `known_users`。

### 7.4 Acquire/renew/release Lua

reservation 申请先校验 READY/epoch，按 Redis server time 清理全局、tenant 和 provisional 索引中已过期成员，再检查：

```text
globalCount < 4

if tenantCount < 2:
    正常申请
else if borrowEnabled
     and tenantCount < 4
     and 当前 tenant 是唯一 active user:
    借用申请
else:
    拒绝本轮申请
```

Acquire 成功时生成 `p:<random>` provisional token，把它写入全局/用户 ZSET、provisional ZSET 和 metadata，score 为 Redis `TIME + provisional TTL`。provisional token 只覆盖带独立 `prepare timeout` 的 `basic.get -> MySQL claim` 短窗口，不能被长任务续约；prepare context 到期必须停止 claim 并按 disposition 归还/NACK delivery，禁止无界 DB 调用跨过 provisional TTL。

MySQL claim 返回 `taskID + new claimGeneration` 后，必须立即执行 `Promote` Lua：校验 READY/epoch、provisional owner/tenant/未过期，原子删除 provisional，并把相同容量转换成稳定 token `r:<SHA-256(resource\x00taskID\x00claimGeneration)>`，其 expiry 以本次 Redis `TIME + 完整 reservation TTL` 重新计算，不能继承可能只剩数毫秒的 provisional TTL。stable token 的 identity 与 recovery 使用的 identity 完全一致。Promote/renew/release 都幂等且校验 metadata，重复 release 不得删除别的 generation。worker 只有 promote 成功后才 ACK 并启动长流水线；若 claim 已提交但 promote 最终失败，worker 不启动外部工作、不续 MySQL lease，ACK 当前 delivery 并让该 RUNNING lease 到期后由常驻 sweeper 重发，不能把 task 简单改回 PENDING。

worker 续约 stable token；Redis epoch 变化时先刷新 epoch，recovery 已按同一 stable identity 重建后再续约。MySQL heartbeat/fence 始终是业务执行权，Redis token 只表示容量。

RECOVERING 期间普通 Acquire/Release/reaper 都会被 fence 拒绝，因此 recovery owner 必须使用独立的 `RecoveryReapExpired` Lua。该脚本同时校验 recovery lock token、epoch、writer fingerprint，并以 Redis `TIME` 在有界 batch 内找出**已经到期**的 provisional 与 processing member：provisional 要同步从 provisional/global/对应 tenant ZSET 和 metadata 删除；processing 要同步删除 processing ZSET 与双向 token 映射。未过期 member 绝不能被强删。脚本重复执行直到没有已到期 member；若 metadata 损坏则返回 typed corruption、保持 RECOVERING 并告警，不能假装 drain 成功。Redis ZSET score 到期本身不会删除 member，所有“等待到期”都必须以该 fenced cleanup 收尾。

### 7.5 借用语义

初始配置下，只有一个用户 A 有 backlog：

```text
A 可以从 base=2 借用到 burst=4
```

B 新增 backlog 时：

```text
已有 A=4 不抢占
A 不再获得新 reservation
下一个释放槽位给 B
最终在双方持续有 backlog 时收敛为 A=2, B=2
```

B 的最后一条 ready 消息被 scheduler 取走后，B 从 active users 移除；若 B 只有一个运行任务且无更多 backlog，A 可以再次借用闲置槽，形成 A=3, B=1。

## 8. RAG 接入

### 8.1 精确 claim

新增 `ClaimRAGIndexTaskByID(ctx, taskID, expectedUserID, expectedDispatchGeneration, workerID, leaseDuration, limits)`。它必须复用现有 claim 的完整锁序和校验：

- user → KB → document → maintenance lease check → task → immutable version 的现有行锁序；
- KB 所有权仍 active；
- task user 与消息 tenant 一致；
- task `dispatch_generation` 与消息 `DispatchToken.Generation` 一致；
- document target version 与 task version 一致；
- maintenance lease 不冲突；
- `PENDING` 已到 `next_run_at`，或 `RUNNING` lease 已过期；两种 status 都必须满足 `next_run_at IS NULL OR next_run_at <= DB_NOW`；
- message generation 等于 row dispatch generation，且严格大于当前 claim generation；成功 claim 把 claim generation 推进到 message generation，允许 canonical repair 导致的安全跳号；
- PENDING 首次 claim 沿用当前 version；过期 RUNNING reclaim 必须走现有 reclaim 状态机：增加 retry，终结旧 RUNNING version，分配并复制新的 immutable version，再写回 task 的新 `doc_version`；
- 成功后把 claim generation **直接设置为 message/row dispatch generation**、写 lease owner/until，并以 `dispatched_at=COALESCE(dispatched_at, DB_NOW)` 封住当前 generation，再返回现有 `RAGIndexClaim`。

进入任何行锁和事务前，精确 claim 使用 MySQL `GET_LOCK` 取得 resource advisory lock。锁名固定为 `bkcrab:fq:` + `SHA-256(databaseName + "\x00" + resource)` 的前 48 个小写 hex 字符，总长 58 字节，不得拼接不受控原文且必须小于 MySQL 64 字节限制。

完整外层锁序固定为：取得 pinned writer `*sql.Conn` → 同 conn 校验 `@@server_uuid + DATABASE() + CONNECTION_ID()` → `GET_LOCK` 成功并复验 session identity → 在同一 conn 开事务 → user → KB → document → maintenance lease check → task → immutable version → 容量检查/状态更新 → commit/rollback → `RELEASE_LOCK` → 归还 conn。获取 advisory lock 之前不得开始事务或建立 repeatable-read snapshot。临界区内使用同一 writer 的 MySQL DB time 和能看到锁获取前最新提交的读取统计有效 RUNNING：

```text
resource running < global 4
current user running < burst 4
```

不在 MySQL 重复实现“有竞争时 base=2”的判断；这仍由 Redis active-user + reservation 策略负责。MySQL 只提供全局 4 和单用户绝对上限 4 的最后防线。advisory lock 只覆盖检查与 claim/heartbeat 事务，不覆盖长时间 RAG pipeline；超时或 capacity-deferred 都不得修改 task。

`HeartbeatRAGIndexTask` 也必须在同一 pinned writer conn 上取得**完全相同的 resource lock**，再以 fence CAS 延长 lease并提交，最后才 release；heartbeat 保留 claim 写下的 durable marker。heartbeat UPDATE 必须同时匹配 `RUNNING + lease_owner + doc/version + expected claim_generation + lease_until > DB_NOW + dispatch_generation=claim_generation`。任一条件不符即永久丢失本次执行 fence并取消 worker，不能再次续回；特别是 sweeper 一旦把 dispatch generation 推进，旧 worker 就不能把已过期 lease 重新续成有效。反过来 heartbeat 若先提交，sweeper 的 expired-lease CAS 必须失败。claim 与 heartbeat 都只在 commit 后释放 lock；因此 claim 计数与 heartbeat 延期存在全序。所有会把 lease 从无效变有效或向未来延长的写路径都必须纳入该锁，不能保留旁路 UPDATE。

fair claim 返回的 execution fence 还必须携带 expected writer fingerprint。成功激活/完成、retry、fail、supersede/replacement、cancel/quiesce acknowledgement 等所有由该 RUNNING execution 触发的 mutation，都必须经统一 helper 在 pinned connection 上验证 `@@server_uuid + DATABASE() + CONNECTION_ID()`，再在同一 connection/transaction 中执行现有 owner/version/claim/lease fence CAS；透明 writer mismatch 时物理 discard并拒绝提交。它们只会终结/释放当前 lease，不需要额外取得容量 advisory lock，但绝不能绕回未经 identity 校验的 pool-level `d.db` mutation。

`GET_LOCK` timeout/NULL/error一律 fail closed。`RELEASE_LOCK` 使用独立于已取消 request 的短时 cleanup context，并必须返回恰好 1；返回 0/NULL、报错或 cleanup context 超时时，必须通过 driver 支持的 discard/`ErrBadConn` 路径**物理淘汰底层连接**，不能只调用会把连接放回池中的 `sql.Conn.Close`。连接物理关闭会释放遗留 named lock；该路径必须有故障注入测试与告警。

该闸门只在所有写入 task claim/lease 的实例连接同一个 authoritative writer mysqld 且 endpoint 保持 session affinity 时成立。公平模式禁止多 primary/按 tenant 分片 writer，也禁止 heartbeat 与 claim 走不同写端点；读副本不能用于容量计数、CAS 或 DB time。部署合约、Redis control fingerprint 与逐 pinned-connection identity/session 校验缺一不可，并在 health detail 暴露安全指纹/最近验证状态。

不得在 Rabbit 已选出 task 后再次调用“领取全局最老任务”的 `ClaimRAGIndexTask`，否则会破坏 Rabbit 中的用户公平顺序。

### 8.2 运行与 heartbeat

精确 claim 成功后复用现有 `runClaim`：

- Rabbit delivery 立即 ACK；
- MySQL heartbeat/fence 继续决定 task 所有权；
- fairqueue worker 按 reservation TTL 周期续约 Redis；
- Redis 短暂不可用时不启动新任务，当前运行任务继续依赖 MySQL fence 完成；MySQL 最终 claim 闸门仍把有效 RUNNING 限制在 4；
- Redis 恢复后 recovery 从有效 MySQL `RUNNING` lease 重建 inflight reservations。

这里的“立即 ACK”严格指 MySQL claim 已提交且 provisional 已成功 promote 之后；promote 失败的处理遵循 7.4，不启动流水线。

### 8.3 Retry

现有 `RetryRAGIndexTask` 继续把任务写回 `PENDING + next_run_at`，并在同一 fence 事务中新增：

```text
dispatched_at = NULL
dispatch_generation = GREATEST(dispatch_generation, claim_generation) + 1
```

当前 Rabbit delivery 已在 claim 后 ACK，不执行立即 requeue。到期前 task 不在 Rabbit ready queue 中；到期后 dispatcher 重新发布，避免未到期消息反复占用 Round Robin 轮次。

### 8.4 常驻 expired-RUNNING sweeper

expired-RUNNING 恢复不是 Redis recovery 的附带步骤。每个健康集群始终运行低频、可多实例并行且靠 MySQL generation CAS 收敛的 sweeper，keyset 分页扫描：

```text
status=RUNNING
AND (lease_until IS NULL OR lease_until <= DB_NOW)
AND (next_run_at IS NULL OR next_run_at <= DB_NOW)
AND dispatch_generation = claim_generation
```

多实例 sweeper 可以并行扫描，不需要依赖单实例 leader 才保证正确。对每个观察到的过期行，以读到的 `dispatch_generation + claim_generation + retry_count + lease` 做 rearm CAS：保持 `RUNNING` 不变，只在 lease 仍 expired、`next_run_at` 已到期且 **`dispatch_generation = claim_generation`** 时执行 `dispatch_generation=claim_generation+1, dispatched_at=NULL`。marker 不参与 rearm 正确性，因而 claim/Mark 竞态或历史空 marker 不会卡死。只有一个 CAS 成功；后续由通用 dispatcher 以**同一新 generation**发布/重试。发布并 Mark 后 `dispatch_generation > claim_generation`，sweeper 不会在消息等待消费期间再次 rearm；精确 reclaim 成功后才把 claim generation 推进到它。旧 duplicate delivery 的 generation 与当前 claim generation 相等，只能 ACK stale，不能绕过 rearm 直接 reclaim。

sweeper **不得**执行 `RUNNING -> PENDING`，不得增加 retry，也不得自行创建 version；这些动作只由收到新 generation 消息后的现有精确 reclaim claim 在同一事务中完成。迟到 confirm、并发 heartbeat 和 stale delivery 由 generation CAS、resource lock 与 claim fence 收敛。

### 8.5 本地模式兼容

fairqueue 默认关闭，RAG index worker mode 默认 `legacy`。关闭且 mode=`legacy` 时，现有 in-process wake channel + `ClaimRAGIndexTask` 行为保持不变，便于开发和回滚。mode=`paused` 时 API、任务创建和其它维护循环继续，但 legacy 与 fair 两种 index claim loop 都不启动；它是部署切换的 drain 中间态，不是依赖故障时自动进入的 fallback。

启用 fairqueue 时：

- 必须使用 MySQL；
- 必须配置 RabbitMQ 与 Redis；
- 禁止同时启动旧的 SQL 全局领取 worker；
- RAG lifecycle、DocumentAI reconcile、GC 等非索引后台循环继续运行；
- 上传和 reindex 只创建 durable task 并通知 dispatcher，不直接向旧 worker channel 投递。

legacy 模式不得为了此次迁移新增 Redis/MySQL mode-fence 依赖；否则基础设施故障会改变现有回滚路径，而且旧二进制也不会遵守新 fence。因此切换采用集群级维护窗口和部署编排硬门槛：先完成兼容版本滚动并核对实例清单，再把**全部**实例滚动到 `worker_mode=paused`；legacy→paused 混合期只有 legacy 会 claim。确认旧实例为零后，在 paused 阶段等待已领取任务完成或 MySQL lease 到期并核对无 legacy claimant；随后把 paused 实例滚动到 `worker_mode=fair`，paused→fair 混合期只有 fair 会 claim，完成 startup rebuild 后解除调度维护窗口。任务创建/API 始终可以继续。禁止从 legacy 直接滚动到 fair。回滚按 `fair → paused（在 paused 阶段 drain）→ legacy` 的相反顺序执行；任何时刻只能有一种 claim loop 存活。

## 9. 生命周期状态机

```text
创建任务
  -> PENDING, dispatched_at=NULL
  -> Rabbit confirm
  -> PENDING, dispatched_at!=NULL       # 概念 DISPATCHED
  -> Redis reservation + MySQL claim
  -> RUNNING
      ├─ 成功 -> DONE
      ├─ 永久失败 -> FAILED
      ├─ 被新版本替代 -> SUPERSEDED
      └─ 暂时失败 -> PENDING, next_run_at=future, dispatched_at=NULL
                         -> 到期重新发布
      └─ lease 过期 -> 保持 RUNNING，dispatch generation = claim generation
                         -> sweeper generation=claim generation+1、dispatched_at=NULL
                         -> dispatcher 发布并 Mark reclaim
                         -> 精确 claim 终结旧 version、分配新 version、
                            retry+1、claim_generation=dispatch_generation，仍为 RUNNING
```

### 9.1 三方职责

| 概念状态 | MySQL | RabbitMQ | Redis |
|---|---|---|---|
| 待发布 | PENDING + dispatched NULL | 无或处于发布竞态 | 无 |
| 已发布 | PENDING + dispatched 非空 | durable ready message | active user/ring（可重建） |
| 已取消息待 claim | 仍为 PENDING | basic.get delivery | reservation 已占用 |
| 运行中 | RUNNING + DB lease/fence + claimed marker 非空，且 dispatch generation = claim generation | 已 ACK | stable inflight reservation |
| 过期待 reclaim | RUNNING + expired lease；发布后 dispatched 非空 | reclaim message | provisional，claim 后提升为新 generation stable token |
| 延迟重试 | PENDING + future next_run + dispatched NULL | 无 | reservation 已释放 |
| 终态 | DONE/FAILED/SUPERSEDED | 无 | 无 |

## 10. 故障与恢复

### 10.1 RabbitMQ 不可用

- MySQL 仍可接受上传和 reindex；
- task 保持 `PENDING + dispatched_at=NULL`；
- dispatcher 指数退避并重连；
- Rabbit 恢复后重新声明 topology、publisher confirm 后发布；
- 健康状态标记 degraded，不把任务标记为永久失败。

### 10.2 Redis 不可用

- scheduler 停止领取新任务；
- 已运行 RAG task 继续执行和 MySQL heartbeat；
- Rabbit 消息保持 ready；
- dispatcher/publisher attempt gate 同 scheduler 一起关闭；每个 attempt 的 READY check因 Redis unavailable失败后不碰Rabbit、不写MySQL marker，due task留在MySQL，Redis恢复并READY后由常驻dispatcher补发；
- Redis恢复后先读取control：若同一runtime只是连接抖动且仍看到protocol/writer匹配的READY epoch，则加入该epoch并在重开本地gate前完成一次有界MySQL authoritative activation/stable reconciliation；只有control缺失（典型为Redis重启丢state），或在protocol/writer均匹配时检测到可安全重建的scheduling divergence，才取得fenced recovery lock、切新epoch RECOVERING并完整重建后READY；已存在control的writer mismatch一律fail closed并要求显式rebind，protocol mismatch要求兼容升级，二者都不能由NORMAL覆盖；
- 不启动无公平 fallback worker。

### 10.3 Worker 崩溃

- claim 前崩溃：Rabbit delivery 重新出现，reservation 到期；
- claim 后已 ACK 崩溃：MySQL RUNNING lease 和 Redis reservation 到期；
- 常驻 expired-RUNNING sweeper 保持 task 为 RUNNING，只 rearm 新 dispatch generation 并重新发布；
- 新 worker 的精确 reclaim claim 才增加 retry、终结旧 version、分配新 version 和 claim generation；
- 原 worker 若恢复也因 claim generation/fence 变化不能提交旧结果。任何恢复路径都禁止直接把过期 RUNNING 改为 PENDING。

### 10.4 重复消息

- 两个 worker都可能收到同一 task ID；
- 只有一个能通过 MySQL 精确 claim；
- 失败者 ACK 重复消息并释放自己的 reservation；
- 重复消息可能浪费一个短暂轮次，但不得造成重复激活或双重结果。

### 10.5 Redis 重建

正常进程启动先在 pinned authoritative writer 读取 `fairqueue_resource_operations`，再执行 `EnsureResourceReady` barrier。若 journal 无 ACTIVE/待完成 special 且已有 protocol/writer都匹配的READY control，则加入现有epoch，并在开放本地scheduler前完成一次有界MySQL authoritative activation/stable-reservation reconciliation；**不**为了每个扩容/滚更Pod把全局resource重新切RECOVERING。journal 为 READY_COMMITTED 时还要求 READY control 的 `last_completed_operation_id` 精确匹配，才可开放 gate并补记 COMPLETED。control 缺失，或protocol/writer均匹配时发现可安全重建的scheduling divergence，且journal不含未完成special时，才可竞选NORMAL recovery；已存在control的writer mismatch一律fail closed并要求显式rebind，protocol mismatch要求兼容升级。存在 ACTIVE 或无匹配 READY control 的 READY_COMMITTED 时保持 operator-required，只有同 kind operator能恢复 mirror并续跑。未完成 join/rebuild 的实例不启动scheduler。

重建顺序：

1. normal rebuild先在 pinned authoritative writer connection 取得resource operation-start `GET_LOCK`，再以随机owner/fence token`SET NX PX`只取得通用Redis recovery raw lock，尚不改变control；在双锁内复查 journal 没有 ACTIVE/待完成 special并再次Check raw lock后，才用单个Lua校验同一lock、生成新的随机resource epoch，把`operation_kind=NORMAL`持久写入control/progress并置RECOVERING，随后释放短时MySQL start lock。Redis已有special或journal存在未完成`RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD`时，普通Begin不得覆盖或Finish，并compare-delete自己仍持有的raw lock。special采用相同双锁顺序；MySQL start lock权威串行journal CAS，CAS后必须再次Check raw lock才Begin。若暂停使raw TTL过期，允许journal ACTIVE保留，但零Begin、零Redis control/progress mutation、零业务mutation（raw lock acquire/renew/compare-delete cleanup除外）；同kind operator随后按允许矩阵恢复。只有携对应确认/attestation及原 journal operation ID的同kind operator可takeover/rehydrate。owner在Begin后定期续recovery lock；续锁失败立即停止，后续recovery Lua因token/operation ID/epoch不符也必须拒绝旧owner；
2. 所有普通 Lua 因 RECOVERING fail closed；每个 inline/scan publish attempt 在碰 Rabbit 前必须用 expected epoch/writer 调 side-effect-free READY check，Redis unavailable也拒绝。runtime 观察新 epoch/state 后同时关闭该 resource 的 scheduler gate 与新 dispatcher/publisher attempt gate。check-before-transition 的竞态只留下已经进入的 bounded attempt；其统一 context deadline覆盖 READY check、confirm/return 与原 candidate Mark。owner 在 capture high-water 前至少等待一个完整 publish-attempt timeout（并等待本地 registry归零），使其它实例无法无限续发；不得在 RECOVERING 中持续发起新 attempt；
3. 不强删未过期 provisional/processing lease。等待它们到期期间持续续 recovery lock，并反复调用 recovery-fenced cleanup Lua，以 Redis `TIME` 原子清除已经到期成员及其所有索引；已有 claim 会出现在 MySQL RUNNING 扫描中。drain 条件是 cleanup 后两类物理集合都为空；bounded timeout 时只要仍有未过期成员、损坏 metadata 或集合非空，recovery 就失败并保持非 READY；
4. 等 bounded 的旧 dispatcher publish attempt 也已结束后，通过 `RecoverySource.CaptureHighWater` 取得 `recovery_high_water=MAX(task.id)`；由 fenced recovery Lua 把 high-water 写入 `recovery_progress`、清零 known/dispatched/running pass 与 convergence marker。随后用 recovery-fenced、bounded `ListOwnedResourceKeys/DeleteOwnedResourceKeys` 在 standalone primary 上按精确 prefix/hash-tag 枚举并清掉所有可重建 scheduling keys，包括无法从损坏 known/meta 反推的 tenant-only ZSET；control、recovery lock/progress 必须排除。再重建 ring、ring_members、processing、active、known 和 stable inflight。后续每个 source page 都显式携带 `highWater` 并限定 `task.id <= high_water`，使 API 持续创建的新任务不会让 barrier 永不结束；
5. 以 task `id` keyset 分页读取 high-water 内 MySQL 有效 `RUNNING + lease_until > DB_NOW`，同一查询返回 `DB_NOW`；计算 `remaining=lease_until-DB_NOW`，再以 Redis `TIME + remaining` 写 token score，禁止把两台机器的绝对时钟直接比较。用与 Promote 相同的 shared helper 生成 `r:<SHA-256(resource\x00taskID\x00claimGeneration)>` 并重建 global/tenant token；每页有界且不用 OFFSET；
6. 先按 `user_id` keyset 分页读取 high-water 内所有 task 的 distinct canonical tenant，重建 known users 并幂等声明 topology；再按 task `id` 分页读取 high-water 内 PENDING 或 RUNNING 且 `dispatched_at IS NOT NULL` 的 canonical task，按 Rabbit ready depth 重建 active/ring；
7. recovery 只记录 high-water 内 due-undispatched 与未 rearm expired-RUNNING 的有界计数/oldest-age 供 READY 后校正，不在 RECOVERING 中调用普通 sweeper/dispatcher或发起 publish。置 READY 后常驻 sweeper先用 generation CAS 武装 expired RUNNING，dispatcher 再发布它们与 due PENDING；绝不改为 PENDING。这样 recovery barrier 与“RECOVERING 禁止新 publish attempt”保持同一合约；
8. 因扫描期间 task/lease 仍可能变化，重复 high-water 内 MySQL valid-RUNNING identity 双向 pass。Coordinator 必须提供 recovery-fenced、有界 cursor 的 stable inflight 枚举，以及按 metadata 同步删除 global/tenant/meta 的 stale stable token、补入缺失 token和刷新 score的原子操作；不能只会 Restore 而无法删除。直到连续完整一轮 identity 无差异；lease expiry 延长本身不算 identity 差异，但每次观察都刷新 score。再完成一轮 high-water dispatched-tenant/Rabbit-depth pass；每个 pass 完成和零差异 cycle 都以 owner/epoch/writer-fenced Lua 写入 `recovery_progress`，所有修复都校验同一 fence；
9. 最终先以Redis`TIME`再执行一次同样的recovery cleanup，并校验recovery lock token/operation ID/epoch/writer fingerprint、fence中的operation kind与持久progress一致、该kind所有专属pass完成、provisional/processing物理为空、通用high-water一致、known/dispatched/running三类pass已完成且valid-RUNNING最近完整cycle为零identity diff。NORMAL 可直接执行最终 Lua；special 必须先在同一 authoritative writer 复验 journal progress并 CAS `ACTIVE→READY_COMMITTED`。随后最终 Lua **在同一原子脚本中**把control operation kind清为`NONE`、置READY、把 special operation ID写为`last_completed_operation_id`、删除progress并仅在value仍等于本owner token时删除recovery lock；不能先清kind再READY。成功后 best-effort 把 journal 置 COMPLETED，失败则由精确 READY/journal 对账补做。READY invariant要求无special kind；禁止Finish返回后unconditional DEL，以免删掉刚取得的新owner lock。实例刷新epoch且完成 journal/control 对账后才启动scheduler。调用方本地bool不能替代 Redis fenced progress或 MySQL journal。high-water之后创建/发布的任务不可能在RECOVERING期间被claim，READY后由永久reconciliation补齐。

分页 cursor 必须采用唯一稳定顺序（task 用 `id`，distinct tenant 用 `user_id`）；每页独立短查询，不持有跨页事务，不一次加载全量任务/tenant。中断后可从头幂等重跑。

READY 后仍永久运行 MySQL authoritative reconciliation，而不是只遍历 Redis：READY epoch/writer-fenced `EnsureKnownTenant` 只补 known/topology 而不激活；有界 `ListReadyStableInflight` 与 `EnsureReadyStableInflight` 对照 MySQL valid RUNNING，exact Release orphan、补入缺失 token并刷新相对 TTL；另运行 expired RUNNING sweeper、due-undispatched dispatcher以及 dispatched canonical tenants/Rabbit depth 校正。普通 list/ensure/release 任一 fence 变化都停止本轮，不能跨 epoch 写。Redis中的调度状态始终可重建；special recovery intent/progress另由MySQL journal防丢。正常健康状态下的worker crash和mark→activate crash都由这些循环修复。

### 10.6 Rabbit 数据灾难恢复

普通Rabbit重启依赖durable queue/message自行恢复。若broker数据被清空，唯一支持的入口是`bkcrab admin fairqueue rabbit-disaster-repair --resource rag.index`（默认dry-run）；真正修改必须追加`--apply --confirm-old-broker-isolated --confirm-publishers-paused`，命令使用专用non-auto-migrate MySQL opener且不得启动gateway。apply按统一顺序取得当前authoritative writer的MySQL start lock与Redis raw lock，先只读preflight：允许matching READY/NONE且无未完成journal的首次开始，也明确允许`READY/NONE + 同kind/同参数ACTIVE journal`恢复CAS后/Begin前崩溃；另允许同operation ID的RABBIT_REPAIR takeover、control missing时按未完成journal rehydrate，以及通用terminal-reconcile。其它special、protocol/writer/ID/参数冲突在journal mutation前拒绝。随后在双锁内于journal CAS建立/恢复随机operation ID的ACTIVE/RABBIT_REPAIR，复验raw lock后再用同一owner/ID的`BeginRabbitRepairWithLock`重复对应Begin矩阵并把kind、新epoch与owner fence镜像到control/progress，停止scheduler与普通publisher；terminal分支则只Complete、零Begin。bounded attempts drain后首次捕获repair high-water并写入journal及Redis mirror，确认旧broker/queue不再提供业务delivery，再对该high-water内仍可调度的PENDING或expired RUNNING逐页CAS `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1、dispatched_at=NULL`。完整pass结束才在journal标记repair pass并同步fenced mirror，随后重建topology/Redis通用recovery状态。

operator在RECOVERING中**不发布消息**；`FinishRecovery`除通用passes外硬验operation kind/ID及journal中的repair high-water/pass，按通用journal提交顺序置READY，普通READY-fenced DispatchSource再扫描所有marker=NULL row并用新token发布/Mark/Activate。rearm半页后崩溃时，即使 recovery lock过期且 Redis control/progress 同时全丢，MySQL journal 仍为 ACTIVE：普通runtime/normal recovery不得takeover、重置progress或Finish，只能由带同样双确认的Rabbit-repair operator按原 operation ID重建 mirror，复用原high-water并从头幂等重扫。已rearm row仍是普通dispatcher的durable义务，未扫描row仍被special pass保护，二者都不丢。不得仅清时间戳、不得改变RUNNING status，也不得周期性盲目重发所有老DISPATCHED task。新generation让灾难前消息即使随后重新出现也无法通过claim。

### 10.7 MySQL writer 合法切换/rebind

普通进程永不自动覆盖resource control的writer fingerprint。计划内failover/restore后使用`bkcrab admin fairqueue rebind-writer --resource rag.index`，默认只读dry-run；apply必须追加`--apply --expected-old-writer-fingerprint <64hex> --confirm-old-writer-fenced --confirm-resource-runtimes-stopped --confirm-new-writer-authoritative`。命令强制MySQL、`AutoMigrate=false`，并要求：旧writer已物理fencing；所有该resource claimant、publisher、scheduler与recovery coordinator已由平台停止且从首次apply一直保持到journal COMPLETED；操作员/DBA已从平台复制位点、restore校验或明确数据损失边界证明新writer是authoritative truth source；schema/owner/generation invariant完整；有效RUNNING为零。journal只写新writer，故仅暂停claim/publish不足以隔离仍可能从旧writer发起NORMAL的coordinator。CLI不能从单库自动证明scale-zero或复制完整性；core attestation缺任一bool零mutation。

apply按统一顺序在**新 authoritative writer**取得MySQL start lock、再取得Redis raw lock，在任何journal mutation前只读preflight允许四种分支：`READY+NONE+writer=expected-old`为首次开始（即使ACTIVE journal已先提交后崩溃，也仍属于可重试的pre-Begin）；`RECOVERING+WRITER_REBIND+writer=target-new+同operation ID`为takeover；control missing且未完成journal参数精确匹配时为rehydrate；`journal=READY_COMMITTED + READY/NONE + writer=target-new + last_completed_operation_id=同ID`为terminal reconcile。其它state/kind/old-target/ID冲突均拒绝。terminal分支重验全部attestation后只CAS journal COMPLETED并释放双锁，零Begin/重建；其它分支在双锁内CAS建立/恢复未完成record并持久original-old/target-new，复验raw lock后通过同一owner/ID的`BeginWriterRebindWithLock`原子重复对应Begin矩阵：pre-Begin执行唯一一次old→new CAS并切新epoch，takeover/rehydrate保持target并切新owner/epoch；READY_COMMITTED rehydrate还必须重做已丢失的Redis通用passes后再Finish。每次resume都重验新writer identity/schema/invariant/valid-RUNNING。按journal提交顺序收敛到COMPLETED后才允许平台恢复resource runtimes。不得让两个writer轮流覆盖control。

### 10.8 Redis 损坏后的 force rebuild

普通recovery发现provisional/processing metadata损坏时保持RECOVERING。force入口默认dry-run，apply需确认flag。一期只支持standalone writable primary，且只作为卡住的`RECOVERING+NORMAL`逃生口，不允许从健康READY主动force。operator 通过独立 `RedisInspector` 重验 topology，并在取得 MySQL start lock 与通用 Redis raw lock 后调用 side-effect-free、同时校验 lock value 的 `ComputeForceRebuildDeadlineWithLock`；该调用使用 Redis `TIME`，返回 observed time 与按等待窗口向上取整到毫秒的、绝不缩短的 `force_not_before`，禁止用节点本地时钟。journal mutation前的只读preflight只允许旧owner已失效的`RECOVERING+NORMAL`首次接管、同operation ID FORCE takeover，或已有未完成journal且control missing时rehydrate，READY/其它special/ID-writer冲突均零mutation拒绝。在允许起态下于journal CAS建立/恢复未完成FORCE_REBUILD record与该`force_not_before`，复验raw lock后再用同一owner/operation ID的`BeginForceRebuildWithLock`原子重复矩阵并切新epoch；READY_COMMITTED rehydrate仍须重做丢失的Redis reset/rebuild passes。takeover复用journal原值。达到not-before且旧attempt结束后，在standalone primary按精确prefix/hash-tag有界SCAN/delete全部可重建keys，保留control/lock/progress。只有完成一整轮zero remaining owned rebuildable keys，才在journal标记`force_delete_pass_complete=true`并同步fenced mirror；半删崩溃或 Redis 全丢时同kindoperator按journal从头幂等重扫/重建。随后`SetRecoveryHighWater`保留force字段并完整rebuild。`FinishRecovery(FORCE)`硬验Redis TIME不早于journal not-before、journal/delete mirror及通用passes，再按通用journal提交顺序完成；普通runtime不得接管/Finish。operator必须显式持current-writer、Rabbit truth source、standalone Redis与timing config verifier，不能从裸RecoverySource假设这些事实。

清理后从 authoritative MySQL/Rabbit 执行完整 high-water rebuild、valid-RUNNING 双向收敛和最终 cleanup，再置 READY。命令中断可由新 owner从头重入；若无法证明 key 归属、等待窗口或 MySQL/Rabbit 真相源完整，保持 RECOVERING并拒绝 force-ready。

## 11. 配置

公平队列是部署级基础设施，不允许用户 scope 覆盖。建议在 `EnvConfig` 增加共享配置，并为 RAG 注册资源策略：

```text
BKCRAB_FAIR_QUEUE_ENABLED=false
BKCRAB_FAIR_QUEUE_REDIS_ADDR=redis:6379
BKCRAB_FAIR_QUEUE_REDIS_PASSWORD=
BKCRAB_FAIR_QUEUE_REDIS_DB=0
BKCRAB_FAIR_QUEUE_REDIS_MODE=standalone
BKCRAB_FAIR_QUEUE_RABBITMQ_URL=amqp://guest:guest@rabbitmq:5672/
BKCRAB_FAIR_QUEUE_EXCHANGE=bkcrab.fair.task
BKCRAB_FAIR_QUEUE_DEAD_LETTER_EXCHANGE=bkcrab.fair.dlx
BKCRAB_FAIR_QUEUE_KEY_PREFIX=bkcrab:
BKCRAB_FAIR_QUEUE_MYSQL_WRITER_TOPOLOGY= # fair mode requires literal single

BKCRAB_RAG_INDEX_WORKER_MODE=legacy # legacy|paused|fair
BKCRAB_RAG_INDEX_LOCAL_WORKERS=4
BKCRAB_RAG_INDEX_GLOBAL_CONCURRENCY=4
BKCRAB_RAG_INDEX_PER_USER_BASE_CONCURRENCY=2
BKCRAB_RAG_INDEX_PER_USER_BURST_CONCURRENCY=4
BKCRAB_RAG_INDEX_BORROW_ENABLED=true
BKCRAB_RAG_INDEX_RECONCILE_INTERVAL=30s
BKCRAB_RAG_INDEX_RESERVATION_TTL=60s
BKCRAB_RAG_INDEX_RESERVATION_HEARTBEAT=20s
BKCRAB_RAG_INDEX_PREPARE_TIMEOUT=10s
BKCRAB_RAG_INDEX_PROVISIONAL_TTL=15s
BKCRAB_RAG_INDEX_DISPATCH_INTERVAL=1s
BKCRAB_RAG_INDEX_PUBLISH_ATTEMPT_TIMEOUT=15s
BKCRAB_RAG_INDEX_EXPIRED_SWEEP_INTERVAL=15s
BKCRAB_RAG_INDEX_PROCESSING_TTL=15s
BKCRAB_RAG_INDEX_RECONCILE_PAGE_SIZE=200
BKCRAB_RAG_INDEX_RECOVERY_DRAIN_TIMEOUT=2m
```

`BKCRAB_FAIR_QUEUE_REDIS_ADDR` 一期只接受 standalone/单-primary endpoint；`REDIS_MODE` 只接受 `standalone`，client 启动握手还必须读取 Redis cluster info并在 `cluster_enabled=1` 时 fail closed。配置校验、health与部署文档明确拒绝 Redis Cluster mode，避免 Lua 多 key 与 force-rebuild SCAN只覆盖部分 primary。

校验规则：

- `base > 0`；
- `base <= burst <= global`；
- `localWorkers > 0`；
- heartbeat 大于 0 且小于 reservation TTL；
- prepare/provisional/processing TTL都大于0，prepare timeout小于provisional TTL，provisional TTL与processing TTL都小于recovery drain timeout；
- fairqueue enabled 时 Redis/Rabbit 地址、exchange、dead-letter exchange与非空key prefix非空；
- worker mode 只能是 `legacy|paused|fair`；RAG `mode=fair` 要求共享 `fairqueue.enabled=true`，但 legacy/paused 不反向要求 enabled=false，因为后续 `image.generate` 可独立使用共享基础设施。mode=fair时storage必须MySQL；mode=legacy只启动RAG legacy loop，mode=paused不启动任何RAG index claim/dispatch loop；共享Rabbit/Redis client是否存在由至少一个registered resource需要fairqueue决定；
- recovery page size、dispatch/reconcile/sweep interval与publish attempt timeout必须为正且有部署级上限；一期统一duration上限为24h、recovery page size上限为10000；publish attempt timeout小于recovery drain timeout，零值不得表示无deadline；
- fair mode 要求 `MYSQL_WRITER_TOPOLOGY=single` 这一显式部署声明，并在启动时从 writer 查询/缓存 `@@server_uuid + DATABASE()`，生成 `writer_fingerprint`；claim、heartbeat、DB time 和计数全部走该 writer。fingerprint 写入 Redis resource control，所有实例加入 READY/recovery时必须一致；每个 critical pinned connection 还必须逐次复验 server/database/session affinity。两个实例看到不同 identity、control/connection 不匹配或查询失败都 fail closed并取消运行 pipeline；fair 启动前部署编排还必须确认 legacy claimant 已全部 drain。

RAG启动真值表固定为：mode=legacy只启RAG legacy loop，mode=paused不启RAG claimant/dispatcher/scheduler，mode=fair只启RAG fair loop且要求enabled=true；`enabled=false,mode=fair`是唯一mode/enabled非法组合。enabled=true配legacy/paused可为其它resource启动共享clients/runtime，但绝不能启动RAG fair loop。`worker_mode`是RAG唯一loop选择器，代码不得再用裸enabled决定RAG worker。

## 12. 可观测性与安全

### 12.1 指标/结构化日志

至少暴露：

- resource Rabbit publish 成功/失败/confirm latency；
- undispatched due task 数和最老等待时间；
- active/known user 数、Redis ring 长度、Rabbit ready depth 抽样；
- global/base/borrowed inflight 数；
- reservation acquire deny 原因；
- task queue wait、claim、run duration；
- duplicate/stale delivery 数；
- MySQL/Redis lease renewal 失败数；
- recovery 扫描、重建和修复数量。
- resource/sweeper lock takeover、dispatch generation rearm、Rabbit return、DLQ 与 MySQL reconciliation lag。

日志中可以记录 task ID、resource 和哈希后的 tenant 标识；默认不输出文档名、prompt、原文、凭据或完整用户 ID。

### 12.2 健康状态

公平模式启用后保留现有探针路径并区分契约：

- `/livez`：只判断进程事件循环是否存活；不探测 MySQL/Rabbit/Redis，不因 RECOVERING 返回失败；
- `/readyz`：判断实例是否可安全接收 API 流量。MySQL writer 不可用、schema 不满足或配置非法时返回 503。Rabbit/Redis 暂时不可用时 MySQL 仍能持久接收任务，因此 API readiness 可保持 200，但内部状态为 degraded/scheduler-not-ready。legacy claimant 是否已归零由部署编排/发布检查证明，不虚构进程内可观测的 runtime fence；
- `/healthz`：保留现有轻量兼容语义，不新增敏感细节；其结果不得替代 `/livez` 与 `/readyz` 的明确用途；
- 受现有admin middleware保护的`/api/admin/health/fairqueue`返回整体状态、依赖、writer identity、部署模式、resource state与epoch短指纹、`operationKind=NONE|NORMAL|RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD`、`operatorRequired`和各pass complete安全布尔、recovery/lag/Rabbit状态。special operation中断时必须让运维者知道需重跑同kind命令；不暴露high-water/cursor、raw epoch/owner token、tenant或凭据。

`healthy` 表示 MySQL/Rabbit/Redis 可用、resource READY 且所有循环新鲜；`degraded` 表示仍可持久接收任务但调度受阻；`failed` 表示 MySQL、schema/config 或 mode 安全合约不满足。探针消费者不得把 live、API ready 与 scheduler ready 混为一个布尔值。

## 13. 测试与验收

### 13.1 单元/组件测试

- Redis activation/decrement 不丢 wakeup；
- Round Robin 顺序 A/B/C/A/B；
- global=4 永不突破；
- 并发精确 claim 即使在 Redis reservation 缺失/过期时，有效 MySQL RUNNING 也不突破 global=4、user burst=4；
- base=2、burst=4 借用和竞争恢复；
- reservation 独立到期和 heartbeat；
- Rabbit 重复消息只成功 claim 一次；
- next_run_at 未到期任务不发布；
- publisher confirm 前不标记 dispatched；
- positive confirm + `basic.return` 不标记 dispatched，confirm/return 按 PublishAttemptID 正确关联；
- 迟到 mark、旧 dispatch generation 和旧 retry/reclaim delivery 均无法 claim 新 epoch；
- expired RUNNING 的旧 duplicate 在 sweeper arm 前不能 reclaim；reclaim message 已 Mark 后多轮 sweeper 不再推进 generation；
- task user 与消息 tenant 不一致时拒绝执行；
- publisher 固定写 stable identity headers；body 完全损坏时仍从受约束 header + queue context + MySQL 定位 canonical task，先 generation repair，再 confirmed durable DLQ，最后 ACK；body/header 都不可定位的非协议消息只能 confirmed DLQ 后 ACK；
- heartbeat 与 claim 并发、临近 lease 边界时 global=4 仍不突破；heartbeat 与 sweeper 竞态时只有先提交者胜出，generation rearm 后旧 worker 永久丢 fence；RELEASE_LOCK 失败物理淘汰连接；
- pop 后崩溃由 processing lease 放回，ring_members 阻止重复轮次；provisional promote 与 recovery stable token identity 一致；
- RECOVERING 中只由 owner/epoch/writer-fenced Lua 按 Redis TIME 清理已过期 provisional/processing 的全部索引；未过期不强删，最终 READY 前物理集合必须为空；
- 每个 claim/heartbeat pinned connection 在 GET_LOCK 前后验证 server UUID/database/session affinity；透明 writer 切换会物理 discard、关闭 gate并取消运行 pipeline；
- writer rebind 与 Redis metadata force rebuild 都有默认 dry-run、显式确认、fenced CAS、全量 rebuild 和中断重入测试；
- claim 已提交但 promote 失败时 ACK/no-run/no-heartbeat，lease 到期后可恢复；maintenance future `next_run_at` 同时门控 PENDING 与 expired RUNNING；
- fair mode 不调用旧全局 MySQL claim worker。

### 13.2 MySQL + RabbitMQ + Redis 集成验收

1. A 独占 backlog 时观察 A 最多运行 4 个；
2. A 已运行 4 个后 B 提交，A 不再获得新槽，B 在下一次释放后启动；
3. A/B 都持续有 backlog 时稳定收敛为 2/2；
4. A/B/C 都有 backlog 时用户启动顺序持续轮转且无饥饿；
5. 断开 Rabbit，上传仍成功，恢复后任务发布；
6. 重启 Redis，运行中任务不重复提交，恢复后 ring/inflight 重建；
7. kill worker，lease 到期后 task 被重新发布并由新 generation 执行；
8. transient failure 在 next_run_at 前不产生 ready 消息，到期后重新发布；
9. 两个 bkcrab 实例共同运行时全局 inflight 仍不超过 4；
10. 正常启动遇到已有匹配READY control时加入现epoch并完成有界reconciliation而不扰动其它实例；首次control缺失/Redis重启丢state才触发分页rebuild。大量task下内存有界，recovery takeover后旧owner的Lua全被epoch/fence拒绝；
11. confirm 后 mark 成功、activate 前 kill dispatcher，MySQL dispatched-tenant reconciliation 恢复 active ring；
12. expand 期间旧 INSERT 可用，contract 前 NULL 与 generation invariant 回填闭合；全量 `paused` 阶段 legacy/fair 都不 claim，legacy→paused→fair 的两个滚动阶段从不混跑 claim loop；
13. 全量 RAG fence、budget、lifecycle、GC 回归继续通过。

## 14. 发布与回滚

1. 上线 nullable `user_id/dispatched_at` 与 `dispatch_generation BIGINT NOT NULL DEFAULT 1` 的 expand migration，fairqueue 保持关闭；
2. 滚动部署能在 legacy 模式 dual-write 新列/generation 的兼容版本；等待旧二进制为零，keyset 回填并验证 user/generation invariant 后再独立 contract `user_id` 为 NOT NULL；
3. 部署 RabbitMQ/Redis并验证 topology、mandatory return、confirm、DLQ、持久卷、告警和备份；
4. 进入调度维护窗口，先把全部兼容实例从 `legacy` 滚动到 `paused`；确认旧实例为零并等待既有 claim 完成/lease 到期后，再从 `paused` 滚动到 `fair` 完成 startup rebuild，最后解除窗口。禁止直接 legacy→fair；
5. fair deployment 先限制实例数观察 publish/claim/duplicate/sweeper/recovery 指标，再扩容；
6. 回滚时先进入维护窗口并把全部实例从 `fair` 滚动到 `paused`，等待/终止运行任务；按 generation 协议处理 Rabbit/undispatched 状态并确认 fair claimant 为零后，才从 `paused` 滚动到**兼容 dual-write release**的 `legacy`；
7. expand 列暂不删除，回滚不做 destructive migration。`user_id NOT NULL` contract 完成后最低可运行版本就是兼容 release，pre-expand/省略 user_id 的旧 binary 禁止启动；若必须回到它，只能在 contract 前回滚。

## 15. Imagegen 后续接入边界

本期不实现 imagegen，但通用接口必须允许后续注册 `image.generate`：

- 对模型只暴露 `image_gen_batch`，总图片数上限 16；
- 支持单 prompt + count，以及 items 数组两种互斥输入；
- batch 拆成 provider 单次能力允许的多个 `image_tasks`；
- `ImageGenerationService.Generate()` 返回结构化 artifacts，并支持可分类的 provider/model fallback；
- imagegen 使用自己的 batch/task 表与 artifact 持久化模型，不复用 RAG task 表；
- 公平队列仍只传 resource/tenant/task type/task ID/dispatch token，不传业务载荷；
- `image.generate` 拥有独立全局/用户并发和 provider 子限流，不占用 `rag.index` 槽位。

## 16. 参考

- `E:\fromGithub\fair-scheduling-queue\README.md`
- `E:\fromGithub\fair-scheduling-queue\internal\scheduler\fair_scheduler.go`
- `E:\fromGithub\fair-scheduling-queue\internal\scheduler\quota.go`
- `E:\fromGithub\fair-scheduling-queue\internal\dispatch\dispatcher.go`
- `internal/rag/pipeline.go`
- `internal/store/rag_task_claim.go`
- `internal/store/rag.go`
- `internal/config/config.go`
- `internal/config/env.go`
