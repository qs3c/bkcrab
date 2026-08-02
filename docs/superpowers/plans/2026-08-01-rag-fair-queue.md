# RAG 多用户公平调度队列实施计划

> **For implementation:** 按任务逐项执行，使用 checkbox（`- [ ]`）跟踪；每个任务先写失败测试，再实现，再跑局部与相关回归。除显式标注的 MySQL/Redis/RabbitMQ 集成测试外，普通单测不得依赖外部服务。

**Goal:** 在不改写现有 RAG 索引正确性流水线的前提下，为 bkcrab 增加可复用的 RabbitMQ + Redis 公平调度基础设施，并让 RAG 索引按用户 Round Robin、集群全局 4 并发、用户基础 2 并发、独占时可借用到 4 并发执行。

**Architecture:** MySQL `rag_index_tasks` 仍是业务真相源，只增加 `user_id`、`dispatched_at` 与 `dispatch_generation`，用“当前 generation + `dispatched_at`”作为 durable publish marker，不新增 outbox/jobs/tenant 表；另增加每 resource 一行的 `fairqueue_resource_operations` 安全 journal，防止特殊恢复在 Redis 丢失后被误判完成。通用 `internal/fairqueue` 只传递 task reference 和稳定 `DispatchToken(resource, taskID, dispatch_generation)`；RabbitMQ 使用 mandatory publish、publisher confirm、durable per-resource/per-user queue 和 durable DLQ，Redis 使用带 cluster epoch/state 的 active ring、processing-turn lease 与逐任务 reservation lease。worker 根据 Rabbit task ID 与 dispatch token 执行现有 MySQL 精确 claim；claim 与 MySQL heartbeat 共用同一个短时 advisory lock，使有效 RUNNING 的 global/burst 最终校验不会漏掉并发续租。Redis 调度状态可重建，special intent/progress 由 MySQL journal 防丢；启动、故障恢复及正常运行都从 MySQL/Rabbit 做有界校正。

**Tech Stack:** Go；MySQL 8；`github.com/rabbitmq/amqp091-go`；`github.com/redis/go-redis/v9`；RabbitMQ publisher confirms；Redis Lua；现有 RAG store/pipeline/telemetry。

**Spec:** [`docs/superpowers/specs/2026-08-01-rag-fair-queue-design.md`](../specs/2026-08-01-rag-fair-queue-design.md)

**范围:** 本计划覆盖通用 fairqueue 基础设施、RAG 接入、MySQL schema、Docker/Helm/Kubernetes 接线、健康检查、故障恢复和集成验收。imagegen batch 不在本计划，只要求通用接口不绑定 RAG。

**基线与兼容:** fairqueue feature flag 默认关闭；关闭时现有 in-process RAG worker 行为不变。公平模式一期只允许 MySQL。SQLite/PostgreSQL 不实现公平 claim/恢复合约；为保持仓库编译和基础 schema 测试，可同步扩展 canonical DDL 的无行为列，但不得宣称完成了其它方言支持。

**安全闸门:** Redis/Rabbit 不可用时不得静默启动旧无公平 worker；公平模式下禁止两套 index worker 同时运行。legacy→fair 不做不可靠的运行时 Redis/MySQL mode fence，而采用维护窗口 `legacy → paused（在 paused 阶段 drain）→ fair` 两阶段切换；`paused` 继续服务 API/创建 durable task，但不启动任何 index claimant。消息不包含原文、对象 key、provider key 或凭据。所有 tenant ID 来自 MySQL task，Rabbit payload tenant 只作交叉校验。

---

## 文件结构总览

```text
go.mod / go.sum

internal/config/config.go
internal/config/env.go
internal/config/config_defaults_test.go
internal/config/rag_test.go

internal/fairqueue/model.go
internal/fairqueue/model_test.go
internal/fairqueue/config.go
internal/fairqueue/rabbit.go
internal/fairqueue/rabbit_test.go
internal/fairqueue/redis.go
internal/fairqueue/redis_test.go
internal/fairqueue/policy.go
internal/fairqueue/policy_test.go
internal/fairqueue/dispatcher.go
internal/fairqueue/dispatcher_test.go
internal/fairqueue/scheduler.go
internal/fairqueue/scheduler_test.go
internal/fairqueue/runtime.go
internal/fairqueue/runtime_test.go
internal/fairqueue/recovery.go
internal/fairqueue/recovery_test.go
internal/fairqueue/rabbit_disaster_repair.go
internal/fairqueue/rabbit_disaster_repair_test.go
internal/fairqueue/recovery_operators.go
internal/fairqueue/recovery_operators_test.go
internal/fairqueue/telemetry.go
internal/fairqueue/telemetry_test.go
internal/fairqueue/integration_test.go

internal/store/database.go
internal/store/database_mysql.go
internal/store/store.go
internal/store/rag.go
internal/store/rag_task_claim.go
internal/store/rag_task_claim_test.go
internal/store/rag_task_migration_test.go
internal/store/rag_fair_queue_mysql_test.go
internal/store/rag_fair_queue_contract.go
internal/store/rag_fair_queue_contract_test.go
internal/store/rag_fair_queue_repair.go
internal/store/fairqueue_operation.go
internal/store/fairqueue_operation_test.go

internal/rag/service.go
internal/rag/pipeline.go
internal/rag/pipeline_test.go
internal/rag/fair_queue.go
internal/rag/fair_queue_test.go
internal/rag/fair_queue_integration_test.go

internal/gateway/gateway.go
internal/gateway/rag_fair_queue_test.go

internal/setup/server.go
internal/setup/handlers_health.go
internal/setup/handlers_health_test.go
internal/setup/rag_deployment_test.go

cmd/bkcrab/cmd_admin.go
cmd/bkcrab/cmd_admin_fairqueue.go
cmd/bkcrab/cmd_admin_fairqueue_test.go

deploy/docker/docker-compose.yml
deploy/docker/docker-compose.rag.yml
deploy/docker/.env.example
deploy/docker/README.md
deploy/multi-pod/docker-compose.yaml
deploy/multi-pod/README.md
deploy/helm/bkcrab/values.yaml
deploy/helm/bkcrab/templates/configmap.yaml
deploy/helm/bkcrab/templates/secrets.yaml
deploy/helm/bkcrab/templates/gateway.yaml
deploy/helm/bkcrab/templates/redis.yaml
deploy/helm/bkcrab/templates/rabbitmq.yaml
deploy/k8s/bkcrab.yaml
docs/database.md
docs/rag-fair-queue-operations.md
```

依赖方向：

```text
gateway
  ├─ fairqueue.Runtime
  └─ rag.Service / rag fair adapter

fairqueue
  ├─ RabbitClient interface
  ├─ RedisCoordinator interface
  └─ domain-neutral Source/Handler/Recovery interfaces

rag fair adapter
  └─ store.Store + existing runClaim + store-private operation journal bridge
```

`internal/fairqueue` 不 import `internal/rag` 或 `internal/store`；gateway 负责把实现装配到一起。

---

## Phase A：固定依赖、配置和 MySQL 合约

### Task 1: 引入 RabbitMQ/Redis 依赖并固定部署级配置

**Files:**

- Modify: `go.mod`, `go.sum`
- Modify: `internal/config/config.go`
- Modify: `internal/config/env.go`
- Modify: `internal/config/config_defaults_test.go`
- Modify: `internal/config/rag_test.go`

- [x] **Step 1: 写配置失败测试**

覆盖：

```text
TestFairQueueDefaults
  enabled=false
  redisMode=standalone
  rag workerMode=legacy
  exchange=bkcrab.fair.task
  deadLetterExchange=bkcrab.fair.dlx
  keyPrefix=bkcrab:
  mysqlWriterTopology=""
  rag localWorkers=4
  rag global=4
  rag base=2
  rag burst=4
  borrow=true
  reconcileInterval=30s
  expiredRunningSweepInterval=15s
  reconcilePageSize=200
  reservationTTL=60s
  heartbeat=20s
  prepareTimeout=10s
  provisionalTTL=15s
  processingTurnTTL=15s
  recoveryDrainTimeout=2m
  dispatchInterval=1s
  publishAttemptTimeout=15s（覆盖 READY check、mandatory confirm/return 与 Mark；所有 attempt 必须有硬 deadline）

TestFairQueueValidation
  workerMode 只能是 legacy|paused|fair
  workerMode=fair 必须 enabled=true；legacy/paused 不反向要求 enabled=false，因为后续 image.generate 可独立使用共享 fairqueue；paused 不启动 RAG claimant
  base>0；localWorkers>0
  base <= burst <= global
  heartbeat >0 且 < reservationTTL
  prepareTimeout/provisionalTTL/processingTurnTTL >0；prepareTimeout < provisionalTTL；provisionalTTL/processingTurnTTL 小于 recovery drain timeout
  dispatch/reconcile/sweep interval、publishAttemptTimeout和page size均为正数且有部署级上限；一期统一duration上限为24h、reconcilePageSize上限为10000；publishAttemptTimeout < recoveryDrainTimeout，零值不得表示无deadline
  enabled 时 Redis/Rabbit 地址、exchange、deadLetterExchange与非空keyPrefix必填
  redisMode 一期只能是 standalone；Redis client 启动握手发现 cluster_enabled=1 时 fail closed
  RAG workerMode=fair + storage!=mysql 时失败
  mode=fair 时 mysqlWriterTopology 必须恰为 single；这里只校验部署声明，writer `@@server_uuid` 查询与跨实例 fingerprint fence 在 Task 9/11 覆盖

TestFairQueueEnvSecretsAreScrubbed
  Rabbit URL / Redis password 进入 boot secret scrub 列表
```

- [x] **Step 2: 跑测试确认失败**

```bash
go test ./internal/config -run 'TestFairQueue' -v
```

- [x] **Step 3: 增加配置类型**

建议结构；字段名可按现有 config 命名规范微调：

```go
type FairQueueCfg struct {
    Enabled      bool
    RedisAddr   string
    RedisPassword string
    RedisDB     int
    RedisMode   string // standalone only in v1
    RabbitMQURL string
    Exchange    string
    DeadLetterExchange string
    KeyPrefix   string
    MySQLWriterTopology string
    RAGIndex    FairQueueResourceCfg
}

type FairQueueResourceCfg struct {
    WorkerMode              string // legacy|paused|fair
    LocalWorkers            int
    GlobalConcurrency       int
    PerUserBaseConcurrency  int
    PerUserBurstConcurrency int
    BorrowEnabled           bool
    ReconcileInterval       time.Duration
    ExpiredRunningSweepInterval time.Duration
    ReconcilePageSize       int
    ReservationTTL          time.Duration
    ReservationHeartbeat    time.Duration
    PrepareTimeout          time.Duration
    ProvisionalTTL          time.Duration
    ProcessingTurnTTL       time.Duration
    RecoveryDrainTimeout    time.Duration
    DispatchInterval        time.Duration
    PublishAttemptTimeout   time.Duration
}
```

部署环境变量使用 spec 中固定名称。基础设施配置只来自 `EnvConfig`，不进入用户/agent scope 合并。`Enabled` 是共享基础设施开关，不是 RAG fair mode 的别名；gateway 按已注册且非 legacy 的 resource 决定是否需要共享 clients。

- [x] **Step 4: 引入依赖**

```bash
go get github.com/rabbitmq/amqp091-go@v1.13.0
go get github.com/redis/go-redis/v9@v9.20.1
```

以上版本已于 2026-08-01 对照两个上游官方 release 页复核；它们高于参考项目的 v1.10.0/v9.5.1，且 bkcrab 的 Go 1.25 满足其最低 Go 版本。Task 1 尚无生产代码 import 这两个模块，因此此处保留显式 direct require；在 Task 4/5 首次真实 import 后运行 `go mod tidy`，并让现有依赖解析和项目漏洞扫描通过，不使用漂移的 `@latest`，也不为保留依赖制造 blank import。

- [x] **Step 5: 验证**

```bash
go test ./internal/config -run 'TestFairQueue' -v
go build ./...
```

- [x] **Step 6: Commit**

```bash
git add go.mod go.sum internal/config
git commit -m "build(queue): add rabbitmq redis fair-queue configuration"
```

---

### Task 2: 向后兼容的 MySQL expand schema 与 generation-aware publish marker

**Files:**

- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/rag.go`
- Modify: `internal/store/rag_task_claim.go`
- Modify: `internal/store/rag_task_migration_test.go`
- Create: `internal/store/rag_fair_queue_mysql_test.go`
- Create: `internal/store/rag_fair_queue_contract.go`
- Create: `internal/store/rag_fair_queue_contract_test.go`
- Create: `internal/store/rag_fair_queue_repair.go`
- Create: `internal/store/fairqueue_operation.go`
- Create: `internal/store/fairqueue_operation_test.go`

- [x] **Step 1: 写 MySQL migration/dispatch 失败测试**

使用现有 MySQL env-gated 测试模式，覆盖：

```text
expand 后，无论 upgrade ALTER 还是空库 fresh/startup CREATE，rag_index_tasks 都有 nullable user_id/dispatched_at 和 `dispatch_generation BIGINT NOT NULL DEFAULT 1`
旧版 INSERT 不提供新列时在 expand 阶段仍成功
新版本在 fair flag 关闭时也为所有新 task 写 user_id
通过 task -> document -> KB 回填 user_id
回填使用 `(id)` keyset 分页、可断点重入，完成后零缺失
expand/startup migration 不执行 user_id NOT NULL contract
创建 upload/reindex/supersede replacement task 时 user_id 等于 KB owner
新任务 status=PENDING, dispatched_at=NULL
新任务 `dispatch_generation=1, claim_generation=0`；兼容版本在 fair disabled 时也 dual-write generation
旧行按状态 keyset backfill：PENDING `dispatch_generation=claim_generation+1, marker=NULL`；RUNNING `dispatch_generation=claim_generation`
fair readiness 拒绝可调度 PENDING 的 `dispatch_generation<=claim_generation` 与未 rearm RUNNING 的 generation 不等
publisher confirm 把完整 candidate（stable DispatchToken + opaque Guard）交回，CAS 标记 dispatched_at
迟到的旧 token 不能标记已经 claim/retry/reclaim 的新 epoch
同一 dispatch generation 下 status/claim generation/retry count/next_run_at/lease snapshot 任一变化，旧 Guard 也不能 Mark
retry、expired-RUNNING rearm、Rabbit 灾难/poison canonical reset 都在同一事务中 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1` + `dispatched_at=NULL`
claim 只接受 `message generation == row dispatch generation > row claim generation`；成功把 claim generation 直接设为 dispatch generation，并用 `COALESCE(dispatched_at, DB_NOW)` 封住 publish-confirm/mark 竞态；终态不再 dispatch
fresh/upgrade 都创建通用 `fairqueue_resource_operations`；每个 resource 只允许一个 CAS-protected current operation
special operation ACTIVE/READY_COMMITTED/COMPLETED phase、随机 operation ID、kind/writer/old-target、repair high-water/pass、force not-before/delete-pass 可中断重入且参数不可被另一 kind 覆盖
ACTIVE/READY_COMMITTED journal在store reopen后仍完整可读；operation ID 不匹配零 mutation
operation-start使用同一pinned connection的MySQL session `GET_LOCK`；连接断开自动释放
stale expected record/version/kind/writer更新journal零mutation
operation-start lock name稳定、独立于capacity lock且<64 bytes；callback后恰好一次RELEASE_LOCK，0/NULL/error/timeout物理discard连接
```

建议门控变量沿用现有 `BKCRAB_TEST_MYSQL_DSN` 或当前 store 测试统一变量。

- [x] **Step 2: 跑测试确认失败**

```bash
go test ./internal/store -run 'TestRAGFairQueue.*MySQL' -v
```

- [x] **Step 3: 扩展 record/scanner/DDL**

`RAGIndexTaskRecord` 增加：

```go
UserID       string
DispatchedAt *time.Time
DispatchGeneration int64
```

expand 期间数据库行仍可能 `user_id=NULL`；scanner 必须用 `sql.NullString`/等价 nullable 中间值再映射，legacy 读取不能因 NULL 崩溃。只有 fair readiness 与 fair dispatch source 拒绝未回填行，不能提前把 Go `string` 直接作为 SQL scan 目标。

MySQL 本兼容 release 的 fresh canonical table 与在线 expand migration 都增加三列且必须把 `user_id` 保持 nullable，以兼容仍会省略该列的旧 writer；`dispatch_generation` 用 `NOT NULL DEFAULT 1` 让旧 INSERT 安全。不能因为是空库就由 startup CREATE 偷跑 contract；只有独立 contract CLI 完成、兼容窗口关闭后的后续版本才可把 canonical CREATE 默认改为 NOT NULL。增加索引：

```text
(status, dispatched_at, next_run_at, id)
(status, lease_until, next_run_at, id)
(user_id, id)
(user_id, status, lease_until)
```

在 real MySQL fixture 上为 dispatch、expired rearm、known/dispatched/running high-water page 与 global/per-user valid-RUNNING count 固定 `EXPLAIN ANALYZE`/rows-examined 断言；`LIMIT` 不能被当作底层有界扫描的证明。`(user_id,id)` 服务 known-tenant keyset/high-water，`(user_id,status,lease_until)` 服务 burst capacity count；若基于基数调整索引列序，测试中的访问上界仍必须成立。

同一 expand migration 创建 `fairqueue_resource_operations`，至少包含：`resource` 主键、128-bit random `operation_id`、`kind`、`phase=ACTIVE|READY_COMMITTED|COMPLETED`、current/original/target writer fingerprint、repair high-water/pass、force not-before/delete-pass、CAS version 与时间戳。该表只记录 special recovery safety intent，不充当 task outbox；NORMAL rebuild 不写表。所有 API 都使用 expected version/operation ID/kind/writer 的单条 CAS，读取与写入均走 expected-writer pinned connection。

`user_id NOT NULL` 是发布末尾的独立 contract migration，不得藏在自动 startup migration 中。如果现有 SQLite 单测共用 `ragIndexTaskColumns`，fresh canonical DDL 同步增加三列以保持编译/扫描兼容；公平运行模式仍通过配置拒绝 SQLite/PostgreSQL，不为两者实现并发合约。

- [x] **Step 4: 在所有 task 创建路径写入 user_id**

检查并覆盖：

- 初次上传 `CreateRAGDocumentWithVersionAndIndexTaskPolicy`；
- reindex `AdvanceDocumentVersionAndCreateTaskPolicy`；
- provider snapshot 漂移产生 replacement version/task；
- legacy unversioned-task contract/repair 中把 survivor 写回 PENDING 或重置 claim generation 的路径；
- lifecycle/orphan/inactive-owner/stale-version 的 canonical terminal/retry repair；
- 在线 keyset backfill；
- 测试 fixture 直接创建 task 的辅助路径。

`user_id` 必须来自事务内锁定的 KB owner，不能信任外层消息参数。

用 `rg "UPDATE rag_index_tasks|INSERT INTO rag_index_tasks|claim_generation=0|status='PENDING'" internal/store` 建立写路径清单并在测试中逐项覆盖；不能只修改正常 upload/retry 路径而遗漏迁移/repair SQL。

兼容 release 还必须更新 legacy claim/retry/reset 路径：即使 fair flag 关闭，legacy 成功 claim 也先计算 `new_generation=GREATEST(dispatch_generation,claim_generation+1)` 并把两个 generation 都设为它，任何形成下一次执行义务的 retry/reset 再按 `GREATEST(dispatch_generation,claim_generation)+1` 推进。fair exact claim 则只在 row dispatch generation 严格更大时把 claim generation 设为 message/row dispatch generation。只有这样，旧二进制退出后的最终 backfill 才不会继续被新旧格式写入打穿。

- [x] **Step 5: 增加最小发布 store 接口**

通用 fairqueue 使用稳定 token，但 generation 的推进由领域 source 的事务负责。建议 store 接口：

```go
ListDispatchableRAGIndexTasksPage(ctx, afterID, limit) -> records + next
GetDispatchableRAGIndexTaskByID(ctx, taskID) -> record
MarkRAGIndexTaskDispatched(ctx, candidateRecord) -> bool
ArmExpiredRAGIndexTasksPage(ctx, afterID, limit)
CaptureRAGFairQueueHighWater(ctx) -> maxTaskID
ListCanonicalRAGTenantsPage(ctx, highWater, afterUserID, limit)
ListDispatchedRAGIndexTasksPage(ctx, highWater, afterTaskID, limit)
ListValidRunningRAGIndexTasksPage(ctx, highWater, afterTaskID, limit)
CaptureRAGBrokerRepairHighWater(ctx) -> maxTaskID
ListBrokerBackedRAGCandidatesPage(ctx, highWater, afterTaskID, limit)
RearmRAGCandidateAfterBrokerLoss(ctx, originalGuard) -> new candidate / stale-noop
```

实现要求：

- 使用数据库时间决定 `next_run_at`/lease 是否到期；
- always-on expired reaper 以 `(id, lease expired, next_run due, dispatch_generation=claim_generation)` CAS 执行 `dispatch_generation=claim_generation+1, dispatched_at=NULL`，只“武装”重发，保持 task/version 为 RUNNING，不修改 retry/claim generation；general dispatcher 随后只选择 `marker NULL + next_run due + dispatch_generation>claim_generation` 的 PENDING 或 expired RUNNING；该循环不依赖 Redis outage/recovery，marker 是否非空不参与 rearm 正确性；
- dispatch/rearm/broker/running page 用稳定 task `(id)` keyset；known tenant page 用稳定 `(user_id)` cursor，并全部显式限制 `id<=highWater`（实时 dispatch/rearm 除外）。所有列表有界、不用 OFFSET，也不一次把全表读入内存；
- by-ID fast path 必须重新从 source 读取 canonical row/token，调用方不能自己拼 message；
- source record 除 stable token 外必须携带不可变的 store guard：`status + claim_generation + retry_count + next_run_at + lease_until/due snapshot`；RAG adapter 把它编码为 fairqueue 不解释的 `DispatchCandidate.Guard`，Mark 再原样解回 store candidate；
- mark 以 `(resource, taskID, dispatch_generation)` 加完整 guard 做单条 CAS，并重新用 DB_NOW 验证 PENDING due 或 RUNNING expired；exact claim 也要求 row dispatch generation 大于 claim generation并校验两种 status 的 `next_run_at` 与 RUNNING lease due；token 相同但 guard 已变化、迟到 mark或旧 delivery都返回 false；
- claim 成功把 `claim_generation=dispatch_generation, dispatched_at=COALESCE(dispatched_at, DB_NOW), next_run_at=NULL`，因此 confirm 后 mark 前已被消费也不会被反复扫描；retry、maintenance/poison canonical reset 与 expired rearm 各自在 canonical 事务里以 `GREATEST(dispatch_generation,claim_generation)+1` 建立新发布义务并清 marker；
- **不得**把过期 RUNNING 改回 PENDING，也不得在 sweeper 里复制 retry/version 修复；Rabbit 新 generation 再投递后复用现有 expired-RUNNING reclaim 的 retry/version 状态机，并把 claim generation 直接推进到 dispatch generation。
- `rag_fair_queue_repair.go` 实现 high-water broker-backed scan 与 original-Guard CAS：只对仍满足原 canonical status/generation/marker/due 的 candidate 执行 `GREATEST(dispatch_generation,claim_generation)+1` 并清 marker，返回新 candidate；stale/终态/no-longer-due 必须 no-op。real-MySQL 测试证明 source 层而非 adapter 才负责 `id<=highWater`、canonical known tenant 与 guarded repair。

`fairqueue_operation.go` 提供不耦合 Rabbit/Redis 的窄 journal API：读取/创建 ACTIVE special、首次写 repair high-water、标记 repair/delete pass、CAS READY_COMMITTED、补记 COMPLETED。Task 2 只定义 store-private `FairQueueOperationRecord/StartSession` 与 raw API，不 import 尚未创建的 `internal/fairqueue`；Task 10 的 RAG adapter（以及后续 Image adapter）把它转换为 Task 3 的 `OperationJournal`，因此 Task 2 commit 可独立编译。store `WithFairQueueOperationStartFence`必须在同一expected-writer pinned connection验证identity，取得独立于capacity lock且稳定小于64 bytes的database/resource session `GET_LOCK`，并让callback内的journal start读写复用该connection；连接断开自动释放。callback结束用独立bounded cleanup context执行恰好一次`RELEASE_LOCK`；返回0/NULL/error/timeout时必须物理discard底层connection，不能只`sql.Conn.Close`回池。调用方在该callback内再取得/续约Redis raw lock并preflight；MySQL start lock是journal CAS的权威串行fence。创建/恢复ACTIVE后必须再次Check raw lock，成功才用同一owner/operation ID执行BeginWithLock。raw TTL若恰在跨存储CAS窗口过期，可留下ACTIVE，但必须零Begin、零Redis control/progress mutation、零业务mutation（raw lock acquire/renew/compare-delete cleanup除外），由同kind恢复；所有路径固定MySQL→Redis锁序。READY_COMMITTED只在source-specific progress已由journal证明且Redis通用passes已复验后写入。每次mutation接收完整expected record（resource/opID/version/kind/writer）并返回新record；stale record零mutation。重复同参数操作幂等，不同kind/ID/old-target/deadline拒绝。writer-rebind记录写入**新 authoritative writer**；Rabbit/force记录写入当前writer。

- [x] **Step 6: 实现并测试独立 contract migration API**

在 `internal/store/rag_fair_queue_contract.go` 提供不进入通用 `Store` 大接口的窄 API `CheckRAGFairQueueContract(ctx) -> Report` 与幂等 `ApplyRAGFairQueueContract(ctx, attestation)`；CLI 在 Task 15 装配。它只允许专用 `AutoMigrate=false` MySQL opener 调用，不由旧版可执行文件会触发的 startup auto-migrate 执行：

```text
dry-run 报告 NULL/旧 generation/无法解析 owner 的 aggregate remaining/page count，不输出原始 task-ID cursor且不修改数据库；apply 可从头幂等重跑
缺少“所有旧 writer 已归零、剩余 writer 已 dual-write”的显式 attestation 时 apply 拒绝；CLI 不伪装成能从单库自动证明 Pod rollout
带 attestation 的 apply 先在 authoritative writer 上反复执行可中断/可重入 keyset backfill，完整 pass 零差异且 generation invariant 闭合后才 ALTER user_id NOT NULL；这一步收敛早期 backfill 后旧 writer又产生的旧格式行
contract 重试幂等；回滚不删除列、不放宽已验证数据
apply 前后都从 INFORMATION_SCHEMA 验证 `user_id NOT NULL`，并重新跑 NULL/owner/generation invariant 检查
```

- [x] **Step 7: 验证相关 store 回归**

```bash
go test ./internal/store -run 'TestRAG(Task|Lifecycle|Budget|Enqueue|FairQueue)' -v
go test ./internal/store -run 'TestRAG.*Migration' -v
```

- [x] **Step 8: Commit**

```bash
git add internal/store
git commit -m "feat(rag): persist tenant and rabbit dispatch marker on index tasks"
```

---

## Phase B：通用 fairqueue 基础设施

### Task 3: 通用消息、资源接口与纯策略

**Files:**

- Create: `internal/fairqueue/model.go`
- Create: `internal/fairqueue/model_test.go`
- Create: `internal/fairqueue/config.go`
- Create: `internal/fairqueue/policy.go`
- Create: `internal/fairqueue/policy_test.go`

- [x] **Step 1: 写失败测试**

覆盖：

```text
message v1 JSON round trip（含 stable dispatch_generation）
拒绝空 resource/tenant/task type/task ID/dispatch token
dispatch token 对 fairqueue 是 opaque，序列化后原样交回 source/handler
DispatchCandidate 同时携带 Message 与 opaque Guard；page/by-ID/Mark 全链路原样保留 Guard
RecoveryPage 与 TenantRef/DispatchedRef/RunningLease/ReservationRef/RedisKeyRef cursor DTO round trip、limit/cursor validation
WriterIdentity/WriterReadinessReport；`RecoveryKind`、固定 `RecoveryPass`、`RecoveryProgress`/三类 operation-specific progress；RabbitRepair/WriterRebind/ForceRebuild attestation/report 与 journal phase/record validation
tenant queue name 使用完整 SHA-256(resource+NUL+tenant) 小写 hex，只包含安全字符、对同一 tenant 稳定且不使用短截断
base/burst/global config validation
resource-specific bounded task-ID validator：rag.index 接受正十进制，通用 transport 不硬编码数值 ID，未知 resource/validator 拒绝
借用规则：
  tenant<base -> regular
  tenant>=base 且 sole active -> borrow
  tenant>=base 且有竞争 active user -> deny
  global full -> deny
```

- [x] **Step 2: 定义领域无关接口**

接口至少分离：

```text
DispatchSource
ExpiredRearmSource
TaskPreparer / PreparedTask
RecoverySource
BrokerRepairSource
RabbitClient
Coordinator
```

`PreparedTask` 边界必须允许 runtime 在“持久 claim 成功”和“长任务 Run”之间 ACK Rabbit：

```go
type PrepareRequest struct {
    Message            *Message // 仅在 body/header/context 全部一致时非 nil
    BodyCandidate      *Message // transport 独立解析/限形，仅用于 repair locator
    HeaderToken        *DispatchToken
    RegisteredResource string
    QueueTenantHash    string
    PublishAttemptID   string
    RawBody            []byte
    DecodeErrorCode    string
}

type TaskPreparer interface {
    Prepare(ctx context.Context, req PrepareRequest) (PreparedTask, PrepareResult, error)
}

type PreparedTask interface {
    Run(ctx context.Context) error
}
```

`DispatchCandidate{Message, Guard}` 必须携带稳定的 `DispatchToken{Resource, TaskID, Generation}` 与 source 产生的 opaque CAS snapshot。`DispatchSource` 同时提供返回完整 candidate 的 keyset page 与 `GetDispatchableByID`，fast path 也只能使用后者；`MarkDispatched(ctx, candidate)` 必须收到原 candidate 并把 Guard 原样交回领域 CAS，不能退化成 token 或 `(taskID,status)` CAS。每次 AMQP publish 另生成随机 `PublishAttemptID` 关联这一次 `basic.return`/confirm；它不是 stable dispatch token，不能进入 MySQL CAS。

`PrepareRequest` 保留 raw delivery、独立解析的 body candidate、四个 stable header parse facts、repair-only header token、可信 queue context 与 property parse 结果。runtime 只有在 body/完整 v1 headers/context 一致，且相等的 `message_id/correlation_id` 都是 canonical 128-bit 小写 hex 时才填 `Message`；反过来，全部 executable facts 已一致时 `Message` 必须存在且等于 candidate，验证器拒绝静默降级。可执行 body 不超过 64 KiB，raw body 必须 strict-decode 为同一个 candidate，body/header/property error code 都为空。`BodyCandidate`/`HeaderToken` 是通过当前 `Registry` 的权威 locator：即使是 repair-only request，只要 `BodyCandidate` 非 nil，也必须由 retained、≤64 KiB raw body strict-decode 得到且 body error 为空；HeaderToken 非 nil 必须匹配独立 header facts。locator 缺失时对应 error code 非空；通用语法合法但 unknown resource/resource-specific task ID 非法时允许 locator nil，不能由 registry-blind 校验反向提升。大 body、omitted body 和 decode failure 均不得携带 candidate。protocol version 损坏时其余三个已通过 Registry 的合法 header 仍可留下 repair locator，但不能授权执行。body-only、header-only 或 mismatch poison 都只能用各自受约束 locator 做 canonical repair，绝不能重建为可执行 Message。mismatch 不选择“看起来更可信”的一边：每个能独立通过 resource、queue tenant、MySQL canonical/current-generation/due CAS 的候选（最多两个）都要 repair，重复/额外发布由 exact claim 去重。为让超过协议上限的 poison 仍能 confirmed DLQ，transport 可保留最多 1 MiB raw body；再大的 delivery 丢弃原文，仅保留 size、SHA-256、受限 header/property 事实和 error codes，并继续交给显式 confirmed DLQ。raw body 只交 transport 校验和 confirmed DLQ，adapter 不得自行重解析，不进入领域表/日志。

Task 3 同时固定跨 Rabbit/Redis/Coordinator 的错误分类：dependency unavailable、unsupported topology、resource not ready、resource fence mismatch、stale recovery owner、corrupt coordination state、publish unroutable 与 publish unconfirmed。后续实现必须包装对应 sentinel，runtime 只用 `errors.Is`/`errors.As` 分支，不解析依赖或 Lua 错误文本。

Task 3 必须在 `model.go` 一次定义后续接口签名所需的全部领域无关 DTO：泛型 `RecoveryPage[T]`、`TenantRef`、`DispatchedRef`、`RunningLease`、`ReservationRef`、`RedisKeyRef`、`WriterIdentity`、`WriterReadinessReport`、`RecoveryLock`、只读 `RecoveryControlSnapshot`、`RecoveryKind`、固定 `RecoveryPass`、`RecoveryProgress`、Rabbit/Writer/Force operation-specific progress、journal `OperationPhase/RecoveryOperationRecord`，以及三类 operator report/attestation 类型。Task 3 的 commit 必须能独立编译；后续 Task 5/8 只能实现/消费这些合约，不能到 Task 5/8 才补类型。

建议最小接口固定为：

```go
type DispatchCandidate struct {
    Message Message
    Guard   string
}

type DispatchSource interface {
    ListDispatchCandidates(ctx context.Context, after string, limit int) ([]DispatchCandidate, string, error)
    GetDispatchableByID(ctx context.Context, taskID string) (DispatchCandidate, bool, error)
    MarkDispatched(ctx context.Context, candidate DispatchCandidate) (bool, error)
}

type ExpiredRearmSource interface {
    RearmExpiredPage(ctx context.Context, after string, limit int) ([]DispatchCandidate, string, error)
}

type RecoverySource interface {
    CaptureHighWater(ctx context.Context) (string, error)
    ListKnownTenants(ctx context.Context, highWater, after string, limit int) (RecoveryPage[TenantRef], error)
    ListDispatched(ctx context.Context, highWater, after string, limit int) (RecoveryPage[DispatchedRef], error)
    ListValidRunning(ctx context.Context, highWater, after string, limit int) (RecoveryPage[RunningLease], error)
}

type BrokerRepairSource interface {
    CaptureRepairHighWater(ctx context.Context) (string, error)
    ListBrokerBackedCandidates(ctx context.Context, highWater, after string, limit int) (RecoveryPage[DispatchCandidate], error)
    RearmAfterBrokerLoss(ctx context.Context, candidate DispatchCandidate) (DispatchCandidate, bool, error)
}

type WriterRebindSource interface {
    RecoverySource
    ReadWriterIdentity(ctx context.Context) (WriterIdentity, error)
    CheckSchemaAndInvariants(ctx context.Context) (WriterReadinessReport, error)
    CountValidRunning(ctx context.Context) (int64, error)
}

type OperationStartSession interface {
    Read(ctx context.Context) (RecoveryOperationRecord, bool, error)
    BeginSpecial(ctx context.Context, expected *RecoveryOperationRecord, proposal RecoveryOperationRecord) (RecoveryOperationRecord, error)
}

type OperationJournal interface {
    Read(ctx context.Context, resource, expectedWriter string) (RecoveryOperationRecord, bool, error)
    WithStartFence(ctx context.Context, resource, expectedWriter string, fn func(OperationStartSession) error) error
    SetRepairHighWater(ctx context.Context, expected RecoveryOperationRecord, highWater string) (RecoveryOperationRecord, error)
    MarkRepairPassComplete(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
    MarkForceDeletePassComplete(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
    CommitReady(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
    Complete(ctx context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error)
}
```

`PrepareResult` 必须把 Rabbit 动作与 canonical MySQL 修复结果分开并固定为可测试枚举：

```text
claimed                         -> promote reservation，ACK，Run
capacity-deferred               -> NACK requeue，重新 activation，release
transient-infrastructure        -> NACK requeue，重新 activation，release
duplicate/stale/terminal        -> ACK，release（canonical row 已能证明无需执行）
canonical-repaired-terminal     -> ACK，release（orphan/inactive/stale version 等修复已提交）
canonical-repaired-retry        -> ACK，release（含 maintenance；DB 已保留原 status、写 future next_run、推进 generation 并清 marker）
poison/permanent-invalid-message -> canonical task 可定位时先 generation repair；再显式 confirmed publish 到 durable DLQ；两步成功后 ACK 原 delivery
```

maintenance 不能只返回“本轮没领取”并 ACK 已发布消息；source 必须在同一 canonical 事务中形成 durable retry 结果：PENDING 与 expired RUNNING 都保持原 status，以 DB time 设置有界 future `next_run_at`，执行 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1, dispatched_at=NULL`，再返回 `canonical-repaired-retry`。dispatcher、Mark 与 exact claim 对两种 status 都服从该时间门，避免 Rabbit hot loop。消息 tenant/token 被篡改属于 poison delivery，不得把仍合法的 canonical task 标成失败。

- [x] **Step 3: 实现纯策略并跑测试**

```bash
go test ./internal/fairqueue -run 'Test(Policy|Message|QueueName)' -v
```

- [x] **Step 4: Commit**

```bash
git add internal/fairqueue
git commit -m "feat(queue): define domain-neutral fair scheduling contracts"
```

---

### Task 4: RabbitMQ durable transport

**Files:**

- Create: `internal/fairqueue/rabbit.go`
- Create: `internal/fairqueue/rabbit_test.go`

- [x] **Step 1: 用 fake AMQP boundary 写失败测试**

覆盖：

- durable direct exchange；
- publish 前声明 durable exchange、durable per-resource/per-user queue 并完成 binding；
- durable dead-letter exchange + per-resource DLQ，并提供 application-level confirmed DLQ publish；tenant queue 可带 DLX 作 broker 级兜底，但应用不得把它当作确认协议；
- persistent delivery mode；
- 每条 canonical v1 publish 都写精确类型的 `x-bkcrab-protocol-version/resource/task-id/dispatch-generation` stable headers，并与 body token 一致；
- mandatory publish 的 confirm ACK 且未收到匹配 `basic.return` 才算成功；
- unroutable 消息即使收到 positive confirm 也返回失败，不允许 mark dispatched；
- publisher confirm NACK/超时、basic.return、return/confirm 乱序与 reconnect；
- reconnect 后重新声明 topology；
- `basic.get` 空队列、合法消息、body 损坏但 stable headers 可解析的消息、body/header 都损坏的外部消息；
- ACK、NACK requeue，以及显式 persistent + mandatory + confirm 的 dead-letter publish；
- channel/connection error 不 panic，下一次操作可懒重连。

不要让业务层直接依赖 `amqp.Delivery`；封装 transport delivery interface，便于 scheduler 单测。

transport delivery 必须保留 bounded raw body（超过 1 MiB 时只保留 size + SHA-256）、body 的独立 schema/shape parse result、四个 header 的独立 parse result、registered consumer resource、queue tenant hash，以及相等且 canonical 时的 PublishAttemptID。body/header/property 三类错误独立记录；任一一致性失败不能丢掉仍受约束的 BodyCandidate/HeaderToken，也不能把其中一个合成正常 Message；runtime 用这些独立事实构造 `PrepareRequest`。

- [x] **Step 2: 实现 client**

要求：

```text
exchange=bkcrab.fair.task（可配置）
queue/routing=<resource>.<tenantHash>
dead-letter exchange=bkcrab.fair.dlx；durable queue=bkcrab.fair.dlq.<registered-resource>；DLQ routing 只用 consumer context，不信任 poison payload
每个连接世代 publish 前 ExchangeDeclare -> QueueDeclare(args: DLX) -> QueueBind
mandatory=true；persistent delivery；每次发布生成随机 PublishAttemptID 写 AMQP message_id/correlation_id
stable identity 同时写入四个固定 AMQP headers；consumer 只接受 exact AMQP type、已注册 resource、通过该 resource 注册 validator 的受限 task ID/正 generation，且 headers 不能决定 topology
publish context timeout；底层库的 publish 调用不保证替调用方消费 context，必须由 confirm/return 等待和世代失效显式兑现 timeout
按 channel publish sequence + PublishAttemptID 关联 confirm/basic.return；ACK 且无 return 才成功；重试生成新 attempt ID 但保留同一 stable DispatchToken
一旦 attempt ID 已分配，后续 publish/return/NACK/timeout 失败也返回对应非零 receipt
连接和 channel 并发安全
shutdown 时停止新操作并关闭连接
```

topology cache 只在当前 connection/channel 世代有效；重连后必须清空并重新 declare/bind。禁止仅凭 positive publisher confirm 判断 routed，因为 Rabbit 对 unroutable mandatory message 也可能 positive confirm。

- [x] **Step 3: 增加 env-gated Rabbit 集成测试**

使用 `BKCRAB_TEST_RABBITMQ_URL`：

```text
publish -> confirm -> get -> ack
删除 binding 后 mandatory publish -> basic.return + positive confirm -> 发布失败且 source 未 mark
body 完全损坏但 stable headers 合法 -> transport 保留 raw body/headers 交给 canonical repair，而不是丢 identity
body 合法但 header 缺失/类型坏 -> Message=nil、BodyCandidate 保留，canonical repair 后 DLQ
body/header 各自合法但 identity mismatch -> Message=nil、两个 locator 都保留并分别对 queue/MySQL 交叉验证；每个独立验证成功的 canonical candidate 都获得新 generation 发布义务，绝不执行原 delivery
header-only delivery 构造的 PrepareRequest.Message 必须为 nil；无论 adapter 返回什么，runtime 都拒绝把它当 claimed 执行
poison delivery -> canonical identity 可定位时先 repair，再显式 mandatory/confirmed publish 到 durable resource DLQ -> ACK 原 delivery；DLQ 失败则 NACK requeue；body/header 都不可定位的非协议消息 confirmed DLQ 后 ACK；重启后仍可检查/重放
超过 1 MiB 的 poison delivery -> transport 不返回原文，只返回 size + SHA-256 + bounded facts -> 显式 confirmed DLQ；成功后 ACK，失败则 NACK requeue
连接重建后 durable message 仍在
同 resource 不同 tenant 队列隔离
```

- [x] **Step 4: 验证**

```bash
go test ./internal/fairqueue -run 'TestRabbit' -v
```

- [x] **Step 5: Commit**

```bash
git add internal/fairqueue/rabbit*
git commit -m "feat(queue): add durable rabbitmq tenant transport"
```

---

### Task 5: Redis resource epoch、fenced ring/turn 与两阶段 reservation

**Files:**

- Create: `internal/fairqueue/redis.go`
- Create: `internal/fairqueue/redis_test.go`

- [x] **Step 1: 先固定 Lua 合约测试**

使用 env-gated real Redis（`BKCRAB_TEST_REDIS_ADDR`）覆盖：

```text
Control/fencing:
  client 启动先读取 INFO cluster 的 cluster_enabled；standalone=0通过，cluster=1返回typed unsupported并且不执行任何Lua/SCAN
  control key 缺失、state!=READY、expected epoch 或 writer fingerprint 不匹配时所有普通 Lua fail closed
  recovery owner 用随机 lock token 取得锁后生成新的不可预测 128-bit resource epoch 并置 RECOVERING；epoch 不是进程本地计数
  首次 control 绑定 `SHA-256(@@server_uuid+NUL+databaseName)`；后续启动/recovery 不得覆盖不同 fingerprint
  recovery lock 续约/提交必须同时匹配 lock value/owner token + 当前随机 epoch + writer fingerprint
  owner 停顿至 lock 过期后，即使恢复执行也不能覆盖新 owner 的 rebuild/READY

Activate:
  SADD known user + HINCR activation generation
  首条消息 SADD active + RPUSH，并原子写 ring_members
  同用户后续消息以及并发 EnsureActive 不重复入 ring

Turn/Rotate/Deactivate:
  NextTurn 原子 LPOP、SREM ring_members、建立带 TTL 的 processing turn lease
  scheduler 死在 LPOP 后，过期 turn 被 reaper 恢复到 ring，且只恢复一次
  stale turn token/epoch 不能 rotate 或删除新 turn
  Rabbit ready depth>0 时回 ring 尾部
  ready depth=0 且 generation 未变化时 SREM active
  ready depth=0 但 generation 已变化时回 ring
  publish 与空队列判断交错不丢 wakeup

Reservation:
  global 永不超过4
  tenant<2 regular
  sole active tenant 可借用到4
  出现第二 active tenant 后禁止新的 borrow
  随机 provisional token 独立过期
  claim 后 Bind/Promote 原子把 provisional 换成 `r:<SHA-256(resource\x00taskID\x00claimGeneration)>`，不出现计数空档或双计数；stable expiry 用 Redis TIME + 完整 reservation TTL，不能继承临近过期的 provisional TTL
  重复 Bind 幂等；错误 provisional/tenant/epoch 不能生成 stable token
  heartbeat 只延长当前 stable reservation
  release 幂等

Recovery drain:
  RECOVERING 中普通 reaper fail closed，owner 专用 cleanup 仍须同时通过 lock/epoch/writer fence
  recovery cleanup 只按 Redis TIME 删除已到期 provisional/processing，并同步清除 global/tenant/meta 与 turn 双向索引
  未过期 member 不强删；损坏 metadata typed failure；重复 cleanup 幂等，最终集合物理为空
  RestoreKnownTenant 不激活；RestoreActiveTenant 才进入 active/ring，known-only tenant 不影响 sole-active borrow
```

- [x] **Step 2: 实现 key builder 和 Lua scripts**

所有脚本使用 Redis server `TIME`，不使用各 bkcrab 节点本地时钟决定 expiry。所有 key 带统一 prefix，并用同一 `{resource}` hash tag 保证一个 resource 的 Lua keys 位于同一 slot。至少包含：

```text
control HASH(epoch=<opaque 128-bit>,state,operation_kind=NONE|NORMAL|RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD,operation_id,last_completed_operation_id,protocol_version,writer_fingerprint)
recovery_lock STRING(random owner/fence token, TTL)
ring LIST + ring_members SET + active_users SET + known_users SET
activation_gen HASH
processing_turns ZSET + turn_to_tenant HASH + tenant_to_turn HASH
inflight global/tenant ZSET + provisional ZSET + reservation metadata HASH
recovery_progress HASH(operation_kind, operation_id, high_water, known/dispatched/running completion, convergence cycle/diff, repair_high_water/complete, rebind_original_old/target_new, force-rebuild progress)
```

每个普通读改写Lua都接收`expectedEpoch+expectedWriterFingerprint`，并验证`state=READY+operation_kind=NONE`；control缺失、RECOVERING、special kind、epoch/protocol/writer不同都typed fail closed。recovery/reset/restore/finish脚本要求`state=RECOVERING`且recovery lock token、resource epoch、writer fingerprint、expected operation kind 与 special operation ID 全部匹配。不能只依赖会过期的TTL lock；旧owner恢复后会被新lock value/operation ID/epoch拒绝。

Coordinator API 至少包括：

```go
type ResourceFence struct { Epoch, WriterFingerprint string }
type RecoveryLock struct { OwnerToken string }
type RecoveryFence struct { ResourceFence; OwnerToken string; Kind RecoveryKind; OperationID string }

ObserveReadyFence(ctx, resource, writerFingerprint) ResourceFence
CheckReadyFence(ctx, resource, fence) // read-only/side-effect-free，Redis unavailable或非READY即拒绝
Activate(ctx, resource, fence, tenant)
EnsureKnownTenant(ctx, resource, fence, tenant) // 只补 known，不激活
NextTurn(ctx, resource, fence, turnToken, ttl) // tenant + observed activation generation
RotateOrDeactivate(ctx, resource, fence, turnToken, observedGeneration, hasReady)
EnsureActive(ctx, resource, fence, tenant)
AcquireProvisional(ctx, resource, fence, tenant, randomToken, limits, ttl)
BindReservation(ctx, resource, fence, tenant, provisionalToken, stableToken, ttl)
RenewStable(ctx, resource, fence, tenant, stableToken, ttl)
Release(ctx, resource, fence, tenant, token)
ListReadyStableInflight(ctx, resource, fence, cursor, limit) RecoveryPage[ReservationRef]
EnsureReadyStableInflight(ctx, resource, fence, tenant, stableToken, remainingTTL)
ReapExpiredTurnsAndProvisionals(ctx, resource, fence, limit)

RedisInspector.InspectRedisTopology(ctx) RedisTopology // standalone/cluster + writable-primary，只读
AcquireRecoveryLock(ctx, resource, randomOwnerToken, ttl) RecoveryLock // 仅SET NX，不改变control/progress
CheckRecoveryLock(ctx, resource, recoveryLock) // side-effect-free；stale owner typed拒绝
RenewRecoveryLock(ctx, resource, recoveryLock, ttl)
InspectRecoveryStart(ctx, resource, recoveryLock) RecoveryControlSnapshot // 持raw lock的只读preflight
ComputeForceRebuildDeadlineWithLock(ctx, resource, recoveryLock, minimumDelay) ForceRebuildDeadline // 同一Lua校验raw lock并读Redis TIME，deadline向上取整到毫秒
ReleaseRecoveryLock(ctx, resource, recoveryLock) // compare-delete，仅用于Begin前拒绝路径
BeginRecoveryWithLock(ctx, resource, writerFingerprint, recoveryLock, ttl) RecoveryFence
BeginRabbitRepairWithLock(ctx, resource, writerFingerprint, operationID, recoveryLock, ttl) RecoveryFence
BeginWriterRebindWithLock(ctx, resource, expectedOldFingerprint, newFingerprint, operationID, recoveryLock, ttl) RecoveryFence
BeginForceRebuildWithLock(ctx, resource, writerFingerprint, operationID string, forceNotBeforeUnixMS int64, recoveryLock RecoveryLock, ttl time.Duration) RecoveryFence
RenewRecovery(ctx, resource, recoveryFence, ttl)
RecoveryReapExpired(ctx, resource, recoveryFence, limit)
ResetResource(ctx, resource, recoveryFence)
SetRecoveryHighWater(ctx, resource, recoveryFence, highWater)
SetRabbitRepairHighWater(ctx, resource, recoveryFence, repairHighWater)
MarkRabbitRepairPassComplete(ctx, resource, recoveryFence)
MarkForceDeletePassComplete(ctx, resource, recoveryFence) // only after a full zero-remaining owned-key scan cycle
RestoreKnownTenant(ctx, resource, recoveryFence, tenant)
RestoreActiveTenant(ctx, resource, recoveryFence, tenant)
RestoreInflight(ctx, resource, recoveryFence, tenant, stableToken, remainingTTL)
ListRecoveryStableInflight(ctx, resource, recoveryFence, cursor, limit) RecoveryPage[ReservationRef]
DeleteRecoveryStableInflight(ctx, resource, recoveryFence, reservation)
MarkRecoveryPass(ctx, resource, recoveryFence, pass, cycle, complete, diffCount)
ListOwnedResourceKeys(ctx, resource, recoveryFence, cursor, limit) RecoveryPage[RedisKeyRef]
DeleteOwnedResourceKeys(ctx, resource, recoveryFence, keys) // exact prefix/hash-tag validated, control/lock excluded
FinishRecovery(ctx, resource, recoveryFence)
```

`RecoveryKind=NORMAL|RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD`持久化在control/progress并进入fence；special还持久化MySQL journal给出的随机operation ID。NORMAL与special都先进入MySQL `WithStartFence`，再`AcquireRecoveryLock`；在journal CAS前必须`CheckRecoveryLock+InspectRecoveryStart`并验证kind-specific允许矩阵，journal CAS后、BeginWithLock前再次Check。Rabbit允许matching READY/NONE且无未完成journal的首次开始，也明确允许`READY/NONE + 同kind/同参数ACTIVE journal`恢复CAS后/Begin前崩溃；另允许同ID RABBIT takeover或control missing时按未完成journal rehydrate。Writer允许READY expected-old首次/ACTIVE pre-Begin恢复、target+同ID WRITER takeover或missing+未完成journal rehydrate。Force只允许旧owner已失效的RECOVERING+NORMAL首次接管、同ID FORCE takeover或未完成journal+missing rehydrate，拒绝READY。三类还共有terminal reconcile：`READY_COMMITTED + READY/NONE + writer/last-completed ID/参数精确匹配`只CAS COMPLETED，零Begin。READY_COMMITTED rehydrate必须重做丢失的Redis通用passes再Finish。任一其它special、writer/protocol/ID冲突都必须在journal mutation前拒绝；Begin Lua重复同一矩阵。NORMAL持双锁复查journal无未完成special后才Begin。普通Begin不得覆盖special。Writer rebind镜像old/target；Rabbit repair镜像high-water/pass；force镜像journal中由Redis TIME计算且不可缩短的not-before/delete pass。operator从头有界SCAN/delete，完成一整轮`remaining owned rebuildable keys=0`后才可`MarkForceDeletePassComplete`；半删崩溃的takeover重新扫描。`SetRecoveryHighWater`保留所有special progress。Finish验证通用passes；RABBIT/FORCE还必须与journal authoritative progress匹配，不能信任本地bool或仅Redis mirror。

`FinishRecovery`的Lua必须在校验owner token/operation ID后同一原子操作把control operation kind清为NONE、完成READY、将special ID写入`last_completed_operation_id`、删除progress与compare-and-delete当前recovery lock；不能先清kind再READY，READY+special kind必须被视为corrupt/fail closed。special调用方须先把MySQL journal CAS到READY_COMMITTED；Finish成功后再best-effort COMPLETED。不得在返回后裸`DEL recovery_lock`。测试覆盖各special kind成功后READY无残留kind、错误ID拒绝、READY_COMMITTED/control ID对账，以及finish成功后另一owner立即取得锁、旧owner迟到cleanup不能删除新锁。

- [x] **Step 3: 故障测试**

```text
Redis timeout 返回 typed unavailable error
脚本执行一半不产生可见部分状态（Lua 原子）
key 不存在时 release/consume 可重试
错误 tenant 不能释放另一 tenant reservation
READY epoch 改变后旧 runtime 的 Activate/NextTurn/Acquire/Renew/Release 全部 fail closed
每个 publish attempt 开始前 CheckReadyFence；check 后 BeginRecovery 的竞态只留下受 PublishAttemptTimeout 硬上限约束的 in-flight attempt
RECOVERING 期间旧 scheduler 不能靠延迟 Rotate 把用户写回新 ring
两个 runtime 使用不同 writer fingerprint 时后加入者及其 recovery/普通 Lua 全部 fail closed
known-only tenant 不进入 active/ring，也不阻止唯一真正 active tenant 借用
READY EnsureKnownTenant 只补 known；List/Ensure stable 受 epoch/writer fence，能在 global-cap 有界 page 中删除 orphan、补入缺失 token且不要求 provisional
provisional/processing 仅 score 到期不会自动消失；RecoveryReapExpired 必须清完所有索引后 Finish 才能 READY
扫描期间 MySQL task 结束 -> recovery-fenced stable list/delete 移除 orphan，连续完整 zero-diff cycle 后才 Finish
任一 high-water pass 未持久标记 complete、high-water 不一致或 progress 属于旧 epoch -> Finish 拒绝
BeginWriterRebind在ACTIVE journal+READY expected-old时允许幂等完成尚未发生的首次old→new CAS；RECOVERING target+同ID才走takeover，missing control按同ID journal rehydrate；其它分支拒绝
normal runtime遇到lock过期但operation_kind=RABBIT_REPAIR/WRITER_REBIND/FORCE_REBUILD时不能takeover或Finish；同kind operator从持久high-water/progress幂等续跑
RecoveryFence.Kind必须与control/progress及API expected kind一致；把RABBIT_REPAIR fence用于NORMAL/WRITER_REBIND/FORCE_REBUILD方法全部typed拒绝且零mutation
MySQL WithStartFence→Redis raw lock→preflight→journal check/create→recheck raw lock→BeginWithLock 顺序固定；start lock串行journal写，raw在CAS窗口过期最多留下ACTIVE，且零Begin、零Redis control/progress mutation、零业务mutation（raw lock维护除外）
special operation ID 在control/progress/fence任一不一致均typed拒绝；Redis mirror全丢只能由同kind operator按journal原ID rehydrate
raw lock可在Begin前renew/check；TTL过期后的owner不能Begin或改变Redis/业务状态，但已由MySQL start fence串行提交的ACTIVE允许保留并由同kind恢复
Rabbit/Writer/Force各自start-state矩阵在preflight与Begin Lua双重验证；不允许起态时journal零mutation，writer pre-Begin ACTIVE+READY-old可安全重试首次CAS
Rabbit journal ACTIVE提交后、Begin前kill时，READY/NONE+同kind/同参数可按原ID继续；不同ID/参数零mutation
READY_COMMITTED+READY/last-completed同ID的terminal-reconcile只CAS COMPLETED，不调用任何Begin/重建
force rebuild 的 key list/delete 逐 key 验证统一 prefix 与精确 `{resource}` hash-tag，永不返回/删除 control、当前 recovery lock 或其它 resource
普通 recovery reset 也通过 recovery-fenced ListOwnedResourceKeys/DeleteOwnedResourceKeys 有界清所有动态 tenant ZSET/metadata；即使 known/meta 部分缺失，tenant-only orphan 也不能残留
```

- [x] **Step 4: 验证**

```bash
go test ./internal/fairqueue -run 'TestRedis' -v
```

- [x] **Step 5: Commit**

```bash
git add internal/fairqueue/redis*
git commit -m "feat(queue): add redis fair ring and renewable reservations"
```

---

### Task 6: Durable dispatcher

**Files:**

- Create: `internal/fairqueue/dispatcher.go`
- Create: `internal/fairqueue/dispatcher_test.go`

- [x] **Step 1: 写 fake source/rabbit/coordinator 失败测试**

严格固定顺序：

```text
ListDispatchCandidates / GetDispatchableByID
  -> CheckReadyFence(expected epoch/writer；失败不碰 Rabbit)
  -> 在 PublishAttemptTimeout 总 deadline 内 PublishMandatoryConfirmed(stable DispatchToken, random PublishAttemptID) + Mark
  -> MarkDispatched(original DispatchCandidate with Guard) CAS
  -> Activate/EnsureActive Redis
```

覆盖故障窗口：

- declare/bind 或 publish 失败：不 mark、不 activate；
- READY check/Redis unavailable：不碰 Rabbit、不 mark并关闭本地 publisher gate；confirm NACK/总 attempt deadline 超时或 basic.return（即使 confirm ACK）：不 mark；允许 READY 后下轮用新 PublishAttemptID 重复 publish；
- publish 成功、mark 前崩溃：重试产生重复消息；
- mark 前 task 已被 claim：claim 的 `claim_generation=dispatch_generation`、`COALESCE(dispatched_at, DB_NOW)` 与 generation 校验使迟到 mark CAS=false；
- publish 后、mark 前同 generation 的 status/claim/retry/next_run/lease snapshot 任一变化：token 虽相同，旧 Guard 仍使 Mark=false；dispatcher 不得重载后拼一个新 Guard 来替换原 candidate；
- mark 前 task 已 retry/rearm 到新 generation：旧 token CAS=false，绝不覆盖新 epoch；EnsureActive 让已入 Rabbit 的 stale delivery 被安全消费；
- mark 成功、Redis unavailable：保持已发布，交给 recovery 激活；
- inline `TryDispatch(taskID)` 必须调用 source `GetDispatchableByID`，与周期 keyset 扫描共享单项实现；
- always-on expired reaper 即使 Redis 一直健康也持续 keyset 扫描，仅对 `lease expired + next_run due + dispatch_generation=claim_generation` CAS arm `RUNNING` redispatch 后交给同一 publisher；marker 不参与 rearm 判定；
- reaper 只通过领域 `ExpiredRearmSource.RearmExpiredPage` 取得新 candidate；通用 dispatcher 不查业务表。arm 后、返回前崩溃仍由普通 dispatch scan 恢复；
- reaper 与两个实例并发、reaper 在 arm 后崩溃均幂等；reclaim message Mark 后因 `dispatch_generation>claim_generation` 不会被下一轮再次 arm；旧 duplicate 也不能在 rearm 前 reclaim。不把 RUNNING 改 PENDING、不创建 version、不增加 retry；
- backoff 有上限且 context cancel 能退出。

- [x] **Step 2: 实现 dispatcher**

dispatcher 不持久化自己的队列；source 的业务 task generation/marker 就是发布依据。公平包不查询 MySQL、不解析/重建 Guard，并在 publish 到 Mark 之间保存原始 candidate。批量 scanner/reaper 每页有界并使用 source 返回的 keyset cursor；一轮结束后等待 interval，不允许空页 busy loop。

- [x] **Step 3: 验证**

```bash
go test ./internal/fairqueue -run 'TestDispatcher' -v
```

- [x] **Step 4: Commit**

```bash
git add internal/fairqueue/dispatcher*
git commit -m "feat(queue): dispatch durable business tasks with publisher confirms"
```

---

### Task 7: Scheduler、worker runtime 与 ACK 边界

**Files:**

- Create: `internal/fairqueue/scheduler.go`
- Create: `internal/fairqueue/scheduler_test.go`
- Create: `internal/fairqueue/runtime.go`
- Create: `internal/fairqueue/runtime_test.go`

- [x] **Step 1: 写调度失败测试**

覆盖：

```text
Round Robin A/B/C/A/B
quota deny 时 tenant 回到 ring，不读取 Rabbit 消息
NextTurn 后 scheduler 崩溃：turn lease 到期后 tenant 回 ring，ring_members 不重复
provisional reservation 成功但 Rabbit empty 时立即 release 并修复 active 状态
Rabbit empty 判断与并发 publish 交错时 generation 防止丢 wakeup
NACK requeue 成功后重新 activation
basic.get 后、claim 前进程崩溃时，reconciliation 最终重新激活 ready queue
损坏消息先显式 confirmed DLQ，成功后 ACK + release
body 完全损坏但 stable headers 合法时，以 queue context + header + MySQL 定位 canonical task，generation repair 后再 confirmed DLQ；不得直接 ACK 留下已 Mark task
body 与 stable headers 都不可定位的非协议消息只在 confirmed DLQ 后 ACK
message tenant/resource 与选中队列不一致时先做 canonical repair，再 confirmed DLQ，最后 ACK + release
Prepare claimed -> Bind provisional 到 `r:<SHA-256(resource\x00taskID\x00claimGeneration)>` -> ACK -> Run
claim 后 Redis 转 RECOVERING/不可用导致 Bind 失败：ACK canonical claim、停止新调度且**不 Run**；等待 MySQL lease 过期后由 reaper 重发/reclaim
Prepare duplicate/stale -> ACK + release，不 Run
Prepare canonical repair -> ACK + release；poison repair -> runtime 显式 confirmed DLQ，成功后 ACK，失败则 NACK requeue
Prepare 暂时 DB error -> NACK requeue + release
Run 完成/失败/panic/cancel 都 release
Run 期间按 20s heartbeat Redis stable reservation
任一 normal Lua 返回 stale epoch/RECOVERING 时 runtime 立即关闭本地 scheduler gate 与新 dispatcher/publisher attempt gate；仅让已经进入的 bounded attempt 完成 confirm/return/Mark
global worker channel 有界，不能无限 goroutine
```

Task 7 在通用 runtime 层锁定 delivery settlement、Bind/ACK/Run、gate、heartbeat 与 shutdown 边界；`basic.get` 后进程崩溃的持续 reconciliation 由 Task 8 证明，RAG 精确 claim/rearm 的端到端重发与不双跑由 Task 9 证明。

- [x] **Step 2: 实现 scheduler loop**

每个 resource 独立 loop：

```text
NextTurn(resource fence, random turn token)
  -> AcquireProvisional(resource fence, random pretoken, regular/borrow)
  -> Rabbit GetOne
  -> 查询 Rabbit ready depth
  -> RotateOrDeactivate(resource fence, turn token, observed generation)
  -> bounded worker channel
```

若 global 满或 Redis 不可用，使用 context-aware backoff；不得回退到无协调执行。

- [x] **Step 3: 实现 worker envelope**

envelope 持有：

```text
PrepareRequest（可执行 Message 可 nil）
Delivery（bounded raw body/header parse/registered queue context）
Reservation token
Resource epoch / writer fingerprint / processing turn token
Resource/Tenant
Raw delivery + validated stable headers（body parse 失败时仍保留）
```

worker 必须把随机 provisional token 原子 Bind/Promote 为共享 helper 生成的 `r:<SHA-256(resource\x00taskID\x00claimGeneration)>`，再 ACK 并启动长任务；promote 必须在同一 Lua 中替换 global/tenant ZSET member，不能先 release 再 acquire。Rabbit ACK 失败记录并继续以 MySQL claim 为准，可能的 redelivery 由下一次 Prepare 判为 duplicate。claim 已提交但 promote 因 unavailable/recovery fence 失败时，ACK 已消费 delivery 但**不得调用 Run**；让已持久 RUNNING lease 自然过期，always-on reaper 递增 dispatch generation、重新发布，再由 existing reclaim 取得新 claim generation。该路径必须有测试证明旧 fence 不能执行、任务最终重发且不双跑。

`basic.get -> Prepare/claim` 必须使用独立 bounded prepare context，deadline 小于 provisional TTL；超时后不得继续一个失去 provisional 容量所有权的 DB claim。若 commit 结果已不确定，必须先按 canonical task 查询归类，不能盲目 NACK 后再次执行。

- [x] **Step 4: graceful shutdown 测试**

```text
停止 scheduler 后不再 basic.get
给运行任务有限 shutdown grace
超时后 cancel Run
release 尽力执行
Rabbit/Redis client Close 只执行一次
```

- [x] **Step 5: 验证**

```bash
go test ./internal/fairqueue -run 'Test(Scheduler|Runtime|Worker)' -v
```

- [x] **Step 6: Commit**

```bash
git add internal/fairqueue/scheduler* internal/fairqueue/runtime*
git commit -m "feat(queue): run fair tenant schedulers with bounded workers"
```

---

### Task 8: 启动恢复、fenced rebuild 与持续 canonical 校正

**Files:**

- Create: `internal/fairqueue/recovery.go`
- Create: `internal/fairqueue/recovery_test.go`
- Create: `internal/fairqueue/rabbit_disaster_repair.go`
- Create: `internal/fairqueue/rabbit_disaster_repair_test.go`
- Create: `internal/fairqueue/recovery_operators.go`
- Create: `internal/fairqueue/recovery_operators_test.go`
- Modify: `internal/fairqueue/runtime.go`

- [x] **Step 1: 写恢复失败测试**

覆盖：

```text
每次 fair runtime 启动：scheduler gate 默认关闭，先 rebuild/加入 READY epoch，再开放本实例
runtime 必须注入 authoritative `OperationJournal`；journal ACTIVE 或无匹配 READY control 的 READY_COMMITTED 时只报告 operator-required，绝不走 NORMAL
Redis healthy -> unavailable：scheduler 停止，running 不被 cancel
Redis unavailable -> healthy：control仍为匹配READY时join现epoch并做bounded authoritative reconciliation；只有control缺失，或protocol/writer均匹配时存在可安全重建的scheduling divergence才完整rebuild；writer mismatch只允许显式rebind、protocol mismatch要求升级，任一路径完成前本地scheduler不恢复
同 resource 只有同时持有随机 owner token + 当前随机 epoch 的 recovery lock owner 可重建
旧 owner lock 过期后恢复执行：所有 reset/restore/READY 操作均被 fence 拒绝
resource control 已绑定其它 writer fingerprint：startup/recovery 全部 fail closed，不能覆盖
进入 RECOVERING 后所有实例停止新 turn；先有界 drain processing turn/provisional claim 窗口，再取 snapshot
进入 RECOVERING 后新 dispatcher/publisher attempt 同样停止；每个实例在 attempt 开始前做 authoritative READY epoch/writer check，只允许 check-before-transition race 中已在途且受总 deadline约束的 attempt完成 confirm/return/原 Guard Mark；owner 在 CaptureHighWater 前至少等待一个完整 PublishAttemptTimeout
provisional/processing 只有 ZSET score 到期：owner 用 recovery-fenced cleanup 同步删全索引；未过期不强删，metadata 损坏保持非 READY
Reset resource 后先按 MySQL `distinct user_id` keyset snapshot 恢复全部 known/topology，再按 dispatched canonical candidate + Rabbit ready depth 恢复 active
只有 known、没有 ready/dispatched backlog 的历史 tenant 不进入 active/ring，也不影响 sole-active borrow
有效 RUNNING task 用同一 shared helper 的 `r:<SHA-256(resource\x00taskID\x00claimGeneration)>` 恢复 inflight reservation
过期 RUNNING 若 `dispatch_generation=claim_generation` 则由 always-on reaper arm；若已大于且 marker 为空则由 dispatcher 重发；recovery 不把它改 PENDING
重复 recovery 幂等
Rabbit reconnect 后 dispatcher 继续 publish
正常运行的 reconciliation 从 MySQL keyset 分页发现全部 canonical tenant 与 dispatched active candidate；即使 Redis known_users 从未写入，也能 declare/bind、检查 Rabbit depth 并重新激活
持续校正补回缺失 stable RUNNING reservation、移除与 MySQL generation 不匹配的 orphan token，并回收过期 turn/provisional
recovery high-water 让持续创建 task 时 barrier 仍有界；READY 前 valid-RUNNING identity 连续一轮零 diff
FinishRecovery 在 provisional/processing 非空、任一 high-water pass 未完成或 convergence marker 非零时拒绝 READY
所有恢复固定 MySQL WithStartFence→Acquire/renew/check Redis raw lock→control preflight→journal check/create→recheck→BeginWithLock；并发start由MySQL lock串行，raw在journal窗口失效时留下可恢复ACTIVE，且零Begin、零Redis control/progress mutation、零业务mutation（raw lock维护除外）
Rabbit repair rearm半页后连同Redis control/progress全丢：journal ACTIVE仍阻止NORMAL，同kind operator按原ID重建mirror并完成剩余row
special READY_COMMITTED后、Redis Finish前崩溃保持operator-required；Finish后、journal COMPLETED前崩溃仅在last_completed_operation_id精确匹配时允许join并补记
Rabbit ACTIVE journal提交后、BeginWithLock前崩溃：重跑同kind命令从READY/NONE按原ID完成Begin；不同参数拒绝
用数万 fake rows/小 page 验证 startup rebuild 始终按 keyset page 拉取、峰值内存只与 page/global cap 有关
recovery owner 在中页失锁，新 owner takeover 后完成；旧 owner 后续每个 restore/finish 都被拒绝
Mark 成功、Activate 前进程退出：READY 后 MySQL dispatched reconciliation 最终声明 topology/检查 depth/激活
```

- [x] **Step 2: 实现 Task 3 已定义的 paged snapshot contract**

```go
CaptureHighWater() string
ListKnownTenants(highWater, afterUserID, limit) RecoveryPage[TenantRef]
ListDispatched(highWater, afterTaskID, limit)   RecoveryPage[DispatchedRef]
ListValidRunning(highWater, afterTaskID, limit) RecoveryPage[RunningLease]
```

high-water 在 bounded publish/prepare drain 后由 source 捕获，并显式传给每个 page；三个 cursor 独立推进，禁止把全表聚合成一个 snapshot。known tenants 来自 `id<=highWater` 的所有 canonical task distinct `user_id` keyset 页；`DispatchedRef` 包含 tenant 与 stable dispatch token；`RunningLease` 包含 task ID、tenant、claim generation、MySQL lease expiry，以及同一查询返回的 DB now。Promote、renew/release 与 recovery 必须调用唯一 shared helper，固定生成 `r:<SHA-256(resource\x00taskID\x00claimGeneration)>`，并用固定 test vector 证明字节级一致，避免双计数。恢复时先算 `remaining=lease_until-DB_NOW`，再用 Redis `TIME + remaining` 写 score，禁止直接比较两台机器的绝对时间。所有 MySQL page 使用稳定 keyset cursor 与 DB time。

- [x] **Step 3: 实现 recovery loop**

要求：

- runtime构造器必须注入`OperationJournal`，startup先用expected-writer pinned connection读journal再走`EnsureResourceReady(writerFingerprint)`：若control缺失且journal无未完成special，才在`WithStartFence` callback内Acquire/Check raw lock、复查journal并`BeginRecoveryWithLock`；若已有control，protocol/writer任一不符立即fail closed；已有READY epoch也要对账journal phase/`last_completed_operation_id`并至少完成一次MySQL-based activation/reservation reconciliation后才开放本地scheduler；
- Redis health transition 后先走 EnsureResourceReady：matching READY则join现epoch并bounded reconcile；只有control缺失，或protocol/writer均匹配时发现可安全重建的scheduling divergence才走NORMAL rebuild。已存在control的writer mismatch只允许显式rebind，protocol mismatch要求兼容升级，二者均fail closed；不在每次普通超时都重建；
- recovery owner 生成不可预测 owner/fence token，先只取得带TTL的通用lock并续约；持锁完成journal检查/创建后才调用对应BeginWithLock，后者另生成不可预测128-bit resource epoch。每一页restore前后验证lock value + operation ID + epoch + writer fingerprint，失锁/identity mismatch立即退出；Begin前拒绝只可compare-delete自己仍持有的lock；
- BeginRecoveryWithLock 原子置 `RECOVERING` 并替换 resource epoch；所有实例看到 fence 后停止新 turn与新 dispatcher/publisher attempt，只允许已在途 bounded attempt 完成 confirm/return/原 Guard Mark。等待 processing turn/provisional lease drain，循环调用 owner/epoch/writer-fenced `RecoveryReapExpired`，只按 Redis `TIME` 同步删除**已经到期**成员的全部索引；bounded drain timeout 到达时若仍有未过期、不一致或损坏 metadata 的 provisional/turn，当前 recovery 失败并保持非 READY，绝不能强删后继续 snapshot。Rabbit unacked delivery 由对应 prepare timeout NACK 或连接关闭回队；已 claim 的 RUNNING 由下一次成功 recovery 的 DB snapshot 捕获；
- BeginRecovery 后等待至少一个完整 `PublishAttemptTimeout`（同时等待本地 attempt registry归零），使所有 check-before-RECOVERING 的分布式 bounded attempt完成/超时，再调用 source 捕获 `recovery_high_water=MAX(task.id)`；`SetRecoveryHighWater` 初始化本 epoch 持久 progress，所有 rebuild pages显式接收 highWater并限定 `id<=high_water`；新任务可继续持久创建但不能让 barrier 无限增长；
- reset 先用 recovery-fenced `ListOwnedResourceKeys/DeleteOwnedResourceKeys` 有界 SCAN/逐 key 归属验证，清掉当前 `{resource}` 的固定与动态 tenant 调度 keys（包括 tenant-only orphan），但保留 control/recovery lock/progress；再按 `user_id` keyset 页调用 `RestoreKnownTenant` 恢复 high-water 内 known/topology、按 task id 页恢复 valid RUNNING stable reservations，并只对 dispatched candidate 且 Rabbit ready depth>0 的 tenant 调用 `RestoreActiveTenant`；known-only tenant 不激活；每类完整结束才 fenced 标记 pass complete；
- restore 后重复 high-water 内 valid-RUNNING identity 双向 pass：用 recovery-fenced stable cursor list 找 Redis orphan并原子删 global/tenant/meta，补 MySQL 缺失并刷新 expiry，直到连续完整一轮零差异；expiry 变化只刷新 score，不算 identity 差异。每轮以 cycle ID/diffCount 写 progress，再完成 high-water known/dispatched/Rabbit-depth pass；
- `FinishRecovery` 先再执行一次 recovery cleanup，只有在 owner/operation ID/epoch/writer、provisional/processing 物理为空、Redis progress 中 high-water 匹配、三类 pass 完成且最近完整 valid-RUNNING cycle 零差异时才可继续；NORMAL直接CAS READY。special先从authoritative journal复验kind/ID/parameters/pass并CAS ACTIVE→READY_COMMITTED，再执行Redis Finish写`last_completed_operation_id`，成功后best-effort COMPLETED；任一跨存储中断按journal/control状态幂等续跑，不能传本地bool绕过。其它实例对账后才能开gate，high-water后的任务由READY后持续校正补齐；
- 正常运行也周期执行 MySQL-based reconciliation，`known_users` 只作缓存/优化，绝不能成为唯一遍历源。每页有界、每页可取消，完整一轮后 cursor 归零；
- 连续校正对最多 global concurrency 数量的 stable token 与 MySQL valid RUNNING 做双向核对；修复操作也校验 READY epoch；
- dispatcher 的 expired RUNNING reaper loop 独立常驻但受 resource state/epoch gate；RECOVERING 中停止 mutation，只有 READY 后恢复武装与 dispatch，recovery loop 内不直接改 task/version/retry。
- Rabbit数据确认丢失时由`rabbit_disaster_repair.go`独立operator编排`BrokerRepairSource+OperationJournal`：dry-run报告broker-backed candidate；apply在`WithStartFence` callback内Acquire/renew/check raw lock，先按Rabbit允许矩阵preflight，再创建/恢复ACTIVE journal，recheck后用同一owner/ID BeginRabbitRepairWithLock。在bounded attempts drain后首次把repair high-water写journal并镜像Redis，再对original Guard逐页`RearmAfterBrokerLoss`，CAS成功形成marker=NULL发布义务；完整pass先标journal再镜像Redis。随后只重建topology与Redis通用high-water状态，**不在RECOVERING中publish**。Finish按READY_COMMITTED→Redis READY/last ID→COMPLETED顺序，普通dispatcher再重发。若rearm半页后Redis也全丢，normal runtime从journal看到ACTIVE只能等待/告警；带双确认的operator按原ID rehydrate并从头幂等重扫。旧broker必须先隔离；fairqueue不import RAG/store。Cobra入口在Task15只做参数/确认/装配。

- [x] **Step 4: 实现 writer rebind 与 Redis force-rebuild operators**

`recovery_operators.go` 提供 dry-run/report 与 apply API，显式接收 attestation；不把安全确认只留在 Cobra：

```go
CheckWriterRebind(ctx, resource, expectedOld string, source WriterRebindSource, journal OperationJournal) (WriterRebindReport, error)
ApplyWriterRebind(ctx, resource, expectedOld string, attestation WriterRebindAttestation, source WriterRebindSource, journal OperationJournal) error
CheckRedisForceRebuild(ctx, resource string, source RecoverySource, journal OperationJournal) (ForceRebuildReport, error)
ApplyRedisForceRebuild(ctx, resource string, attestation ForceRebuildAttestation, source RecoverySource, journal OperationJournal) error // receiver还持Coordinator/config/current-writer/Rabbit/Redis verifiers
```

`WriterRebindAttestation`至少含旧writer已fence、全部该resource runtime/recovery coordinator已由平台停止并保持到COMPLETED、新writer authoritative三个外部bool。dry-run只供审阅。writer apply重验new-writer readiness，在**新 authoritative writer**的`WithStartFence` callback内取得raw lock、preflight Writer矩阵、写/恢复未完成journal并BeginWithLock；ACTIVE+READY expected-old明确允许完成pre-Begin首次CAS，READY_COMMITTED+READY target/last-ID同值则只Complete。force operator receiver必须显式持Coordinator、OperationJournal、resource timing config、current-writer verifier、Rabbit truth-source verifier与standalone-Redis inspector；裸`RecoverySource`只提供MySQL rebuild pages，不能承担这些检查。force apply重验current writer fence/Redis/Rabbit/attestation，只允许卡住的RECOVERING+NORMAL/same-ID FORCE/missing+未完成journal三种Begin起态，并按统一双锁序执行；READY/其它special拒绝（匹配terminal-reconcile除外）。提前Finish、半删/Redis全丢、zero-remaining完整pass、READY_COMMITTED中断/terminal-reconcile与takeover均有测试。

- [x] **Step 5: 验证**

```bash
go test ./internal/fairqueue -run 'Test(Recovery|RabbitDisasterRepair|WriterRebind|RedisForceRebuild)' -v
```

- [x] **Step 6: Commit**

```bash
git add internal/fairqueue/recovery* internal/fairqueue/rabbit_disaster_repair* internal/fairqueue/runtime.go
git commit -m "feat(queue): rebuild fair scheduling state after dependency outages"
```

---

## Phase C：RAG 精确 claim 与公平执行接入

### Task 9: 按 Rabbit task ID 的完整 fenced claim

**Files:**

- Modify: `internal/store/database.go`
- Modify: `internal/store/fairqueue_operation.go`
- Modify: `internal/store/rag.go`
- Modify: `internal/store/rag_attachments.go`
- Modify: `internal/store/rag_budget.go`
- Modify: `internal/store/rag_cache.go`
- Modify: `internal/store/rag_lifecycle.go`
- Modify: `internal/store/rag_object_staging.go`
- Modify: `internal/store/rag_task_claim.go`
- Modify: `internal/store/rag_fair_queue_repair.go`
- Modify: `internal/store/rag_fair_queue_mysql_test.go`
- Create: `internal/store/fairqueue_writer_identity_test.go`
- Create: `internal/store/rag_fair_queue_budget.go`
- Create: `internal/store/rag_fair_queue_budget_test.go`
- Create: `internal/store/rag_fair_queue_capacity.go`
- Create: `internal/store/rag_fair_queue_capacity_test.go`
- Create: `internal/store/rag_fair_queue_claim_test.go`
- Create: `internal/store/rag_fair_queue_execution_reads.go`
- Create: `internal/store/rag_fair_queue_execution_reads_test.go`
- Create: `internal/store/rag_fair_queue_execution_test.go`
- Create: `internal/store/rag_fair_queue_lifecycle.go`
- Create: `internal/store/rag_fair_queue_lifecycle_test.go`
- Create: `internal/store/rag_fair_queue_store.go`
- Create: `internal/store/rag_fair_queue_store_test.go`
- Create: `internal/store/rag_fair_queue_poison.go`
- Create: `internal/store/rag_index_object_fence_test.go`

- [x] **Step 1: 写失败测试**

新增测试覆盖：

```text
ClaimRAGIndexTaskByID 显式接收 expected writer fingerprint 与 expectedDispatchGeneration（或由显式 expected-writer-bound facade 提供前者），只领取指定 task，并校验 message dispatch_generation 与 due canonical row
expectedUserID 与 task.user_id 不一致时拒绝且不修改 task
重复 delivery 只有一个 claim 成功
8 个并发精确 claim 在两个 store 实例下，有效 RUNNING 始终不超过 global=4
同一用户有效 RUNNING 始终不超过 burst=4
Redis reservation 已缺失/过期时，MySQL 最终闸门仍拒绝第 5 个 RUNNING
容量已满返回 capacity-deferred，task 保持 PENDING
advisory lock 超时不修改 task，并返回可重试错误
PENDING future next_run_at 不可 claim
PENDING due 可 claim
RUNNING lease 未过期不可 claim
RUNNING lease 过期但 `dispatch_generation=claim_generation` 时旧 duplicate 只能 stale ACK；必须先由 sweeper arm 到 dispatch generation 更大才能 reclaim
claim 只接受 `message generation == row dispatch generation > row claim generation`，成功在同一事务设置 `claim_generation=dispatch_generation, dispatched_at=COALESCE(dispatched_at, DB_NOW), next_run_at=NULL`
orphan/inactive owner/stale version 返回已提交的 canonical terminal repair
maintenance 返回已提交的 canonical retry repair：保持 PENDING/expired RUNNING 原 status，future next_run + 新 dispatch generation + marker NULL，不会 ACK 后永久丢任务或 hot loop
poison tenant/token delivery 修复 canonical dispatch generation 后进入 DLQ，不污染合法 task 终态
完整锁序仍是 advisory resource -> user -> KB -> document -> maintenance check -> task -> version
从 claim/heartbeat/count 使用的 authoritative writer 查询 `@@server_uuid` 并生成绑定 databaseName 的 fingerprint；查询失败 fail closed
每个 claim/heartbeat pinned conn 在 GET_LOCK 前与成功后复验 `@@server_uuid + DATABASE() + CONNECTION_ID()`；transparent switch/session 不粘连时无业务 mutation
fair execution 的 finish/retry/fail/supersede/cancel/quiesce 等写路径也验证 expected writer identity，不走未验证 pool mutation
pipeline 的 chunk/chunk-asset/object-staging/cache catalog/DocumentAI budget 等执行期写入不得保留 `CheckFence -> pool mutation` TOCTOU；必须改为同一 expected-writer pinned transaction 内复验 execution fence 后写入
fair mode的durable task create/read/cancel入口也走expected-writer pinned helper；transparent switch时create事务不提交且不触发notifier
Dispatch list/by-ID/Mark、expired rearm、poison repair、Recovery CaptureHighWater/每页、BrokerRepair与continuous reconciliation 的 read/write 全部使用 expected-writer pinned helper；透明切换不能产生 candidate/snapshot或置READY
真实 MySQL 中 RELEASE_LOCK 返回异常/连接状态不明时物理 discard，后续 pool borrower 不继承 named lock
```

- [x] **Step 2: 抽取共享 claim 内核**

避免复制两套 300 行锁/fence 逻辑：

```text
旧 ClaimRAGIndexTask：只负责找到全局候选 task ID
新 ClaimRAGIndexTaskByID：使用调用方 task ID
二者进入同一个 claimRAGIndexTaskIDInTx 内核
```

旧方法必须保持现有测试通过，供 fairqueue disabled 模式使用。

共享内核显式接收 claim mode/expected dispatch generation：legacy wrapper 计算 `new_generation=GREATEST(dispatch_generation,claim_generation+1)` 并同时写两个 generation；fair wrapper 要求 canonical/message dispatch generation 相等且严格大于旧 claim generation，再把 claim generation设为它。不得为了复用而让 legacy 伪造 Rabbit token，也不得让 fair 退回无 token 的 `+1` 路径。

- [x] **Step 3: 增加 MySQL 最终并发闸门**

先实现并单测统一 lock-name builder。lock name 必须用参数传给 MySQL、在同一 server 上按 database + resource 隔离、确定性且不超过 MySQL 64-byte 限制；固定为 `bkcrab:fq:` + SHA-256(`database + NUL + resource`) 的前 48 个小写 hex（总长 58），不要直接截断可能碰撞的长 database/resource 字符串。

fair mode 的精确 claim 先从 `*sql.DB` 获取 pinned `*sql.Conn`，在同一连接上严格先验证 identity/session、再 lock 后 transaction：

```text
SELECT @@server_uuid, DATABASE(), CONNECTION_ID() -> expected identity
  -> GET_LOCK(lockNameFor(database, "rag.index"), bounded timeout)
  -> recheck server/database/connection ID unchanged
  -> BEGIN
  -> 用 DB_NOW 统计有效 RUNNING 全局数和当前 user 数
  -> global>=4 或 user>=burst 时 rollback，返回 capacity-deferred
  -> 执行共享 fenced claim 内核
  -> COMMIT/ROLLBACK
  -> RELEASE_LOCK（defer，失败记录高优先级日志）
```

identity queries、`GET_LOCK`、BEGIN/查询/COMMIT 与 `RELEASE_LOCK` 必须都使用该 pinned connection；不得从 pool 上混用 `d.db`。连接建立 hook 可提前验证，但逐临界区复验不可省略。事务隔离级别/首次读必须保证看见取得 advisory lock 前已经提交的 heartbeat。advisory lock 不得跨越 `runClaim`。连接获取、identity/session mismatch、lock timeout、context cancel、begin/commit failure 和 release failure 都要有测试。identity mismatch 与 release/connection 状态不明都必须通过 driver bad-connection/物理 discard 移出 pool，并以 typed fatal safety error关闭 runtime gate/取消 pipeline——不能假设 `sql.Conn.Close()` 会销毁物理连接。释放使用独立于已取消 request 的 bounded cleanup context并必须恰好返回 1。该逻辑只在 MySQL fair mode 使用，不为 SQLite/PostgreSQL 实现等价物。

- [x] **Step 4: 把 MySQL heartbeat 纳入同一最终闸门**

`HeartbeatRAGIndexTask` 也必须在 pinned connection 上取得**完全相同 resource lock name**，再 BEGIN、按 fence/generation CAS 延长 lease、COMMIT、RELEASE_LOCK。heartbeat UPDATE 必须同时匹配 RUNNING、owner、doc/version、expected claim generation、`lease_until > DB_NOW` 和 `dispatch_generation=claim_generation`；否则永久丢 fence并取消 worker。这样 claim 的有效 RUNNING 计数不会在 MVCC snapshot 中漏掉一个尚未提交的 lease 延长，sweeper rearm 也不会被旧 worker 续回。覆盖：

```text
heartbeat 已开始但未提交时 claim 等待同一 advisory lock
heartbeat 提交并释放后 claim 的 DB-time count 看见续期 lease，拒绝第 5 个 RUNNING
heartbeat lock timeout/fence lost 不续租，worker 按现有语义取消
heartbeat 先提交时 sweeper expired CAS=false；sweeper 先推进 dispatch generation 时旧 heartbeat CAS=false且以后不能恢复
不同 database/resource lock name 不互相阻塞
```

上述 advisory-lock、heartbeat/sweeper 排序、逐 connection identity/session 校验和物理 discard 不能只用 sqlmock；在 `rag_fair_queue_mysql_test.go` 增加 `BKCRAB_TEST_MYSQL_DSN` 门控的两个真实 `*sql.DB`/多连接测试，并用可控 connector 注入透明 server UUID/connection ID 变化验证 transaction 前 fail closed。

- [x] **Step 5: 增加 tenant/dispatch/fence 校验与 canonical disposition**

task `user_id` 与锁定 KB owner、Rabbit expected tenant 三者必须一致；message dispatch generation 必须等于 row dispatch generation 且严格大于 row claim generation，成功后直接令二者相等。把 existing claim 内核的“领取成功 / 容量不足 / 暂时错误 / 已终态 / 已修复终态 / 已修复 retry / poison”显式返回给 adapter。maintenance repair 必须 durable，并让 PENDING/expired RUNNING 都受 future `next_run_at` 门控；可定位的 poison delivery先在 canonical 事务中按 `GREATEST(dispatch_generation,claim_generation)+1` 推进并清 marker，再显式 confirmed publish 到 DLQ，保证合法 task 会由 dispatcher 发出新 token。

fair claim/execution fence携expected writer fingerprint。抽取同一identity-checked pinned read/transaction helper：除当前pipeline的`ActivateAndFinishRAGIndexTask`、`SupersedeRAGIndexTaskAndCreateVersion`、`RetryRAGIndexTask`、`FailRAGIndexTask`、cancel/`AcknowledgeRAGIndexTaskQuiesced`外，还覆盖chunk/chunk-asset/object-staging/cache catalog/DocumentAI budget等执行期写入、fair-mode durable task create/read/cancel、Task2的dispatch list/by-ID/Mark、expired rearm/poison/broker repair，以及recovery high-water/每个page与continuous reconciliation。禁止保留先`CheckRAGIndexFence`再通过pool裸写的TOCTOU路径；读操作在返回DTO前也验证connection identity；每条mutation仍匹配原owner/version/claim/lease fence。逐项测试mismatch时不返回可发布/可恢复snapshot、不提交create/cancel/执行期数据、不触发notifier、不激活结果、不建立replacement发布义务并返回fatal safety error；终结/释放路径不取得容量lock。Task 9 同时导出同一`DBStore`上的窄writer discovery API及线程安全的最近connection identity安全快照（只含成功验证时间和verified/unknown/mismatch状态，不暴露server UUID/database/connection ID），供Task 11装配与health真实读取。

- [x] **Step 6: 验证**

```bash
go test ./internal/store -run 'TestRAG.*Claim' -v
go test ./internal/store -run 'TestRAG.*(Lifecycle|Budget|Fence)' -v
```

- [x] **Step 7: Commit**

```bash
git add internal/store
git commit -m "feat(rag): claim exact rabbit-delivered index tasks with fences"
```

---

### Task 10: RAG fairqueue adapter 与 pipeline 三态模式

**Files:**

- Create: `internal/rag/fair_queue.go`
- Create: `internal/rag/fair_queue_test.go`
- Modify: `internal/rag/service.go`
- Modify: `internal/rag/pipeline.go`
- Modify: `internal/rag/pipeline_test.go`
- Modify: `internal/rag/parse/pdf_auto.go`
- Modify: `internal/rag/parse/pdf_auto_test.go`
- Modify: `internal/rag/parse/office.go`
- Modify: `internal/rag/parse/office_test.go`
- Modify: `internal/rag/enrich/enrich.go`
- Modify: `internal/rag/enrich/enrich_test.go`
- Modify: `internal/rag/enrich/openai.go`
- Modify: `internal/rag/enrich/openai_test.go`
- Modify: `internal/rag/enrich/telemetry_test.go`
- Modify: `internal/rag/vision/openai.go`
- Modify: `internal/rag/vision/openai_test.go`
- Modify: `internal/rag/vision/budget.go`
- Modify: `internal/rag/vision/budget_test.go`
- Modify: `internal/fairqueue/model.go`
- Modify: `internal/fairqueue/model_test.go`
- Modify: `internal/fairqueue/dispatcher.go`
- Modify: `internal/fairqueue/dispatcher_test.go`
- Modify: `internal/fairqueue/runtime.go`
- Modify: `internal/fairqueue/runtime_test.go`
- Modify: `internal/fairqueue/recovery.go`
- Modify: `internal/fairqueue/recovery_test.go`
- Modify: `internal/store/rag_fair_queue_contract.go`
- Modify: `internal/store/rag_fair_queue_contract_test.go`
- Modify: `internal/store/rag_fair_queue_store.go`
- Modify: `internal/store/rag_fair_queue_store_test.go`
- Modify: `internal/store/rag_fair_queue_lifecycle.go`
- Modify: `internal/store/rag_fair_queue_lifecycle_test.go`
- Modify: `internal/store/rag.go`
- Modify: `internal/store/rag_object_staging.go`
- Modify: `internal/store/rag_object_staging_test.go`

- [x] **Step 1: 写 adapter 失败测试**

覆盖：

```text
ListDispatchCandidates/GetDispatchableByID 映射成 resource=rag.index + stable DispatchToken message + opaque store Guard
MarkDispatched 把原始 candidate/Guard 交回 task ID + dispatch_generation + status/claim/retry/next_run/lease store CAS
ExpiredRearmSource 映射 store `ArmExpiredRAGIndexTasksPage`，返回新 candidate；通用层不查询 RAG 表
Prepare 只在 `PrepareRequest.Message!=nil` 时用已一致的 body token 调用 ClaimRAGIndexTaskByID
body-only/header-only/mismatch poison 使用 BodyCandidate 和/或 HeaderToken + registered queue context 定位 canonical task并 repair，永远不能返回 claimed；mismatch 对每个独立通过 resource/queue/MySQL/current-generation/due 验证的候选（最多两个）分别 generation-advance，不能任选一边；两个 locator 都不可定位才返回 explicit unlocatable poison
capacity-deferred 映射为 NACK requeue + activation，而不是失败任务
duplicate/stale claim 返回 no-run disposition
maintenance/orphan/inactive/stale-version 映射到已提交的 canonical repair disposition
poison 映射到 canonical generation reset + durable DLQ；body 损坏时用受约束 stable headers + queue context 定位，headers 不授权执行
claimed 返回 PreparedTask，Run 复用 runClaim
RecoverySource 先 CaptureHighWater，再用显式 highWater 的独立 keyset pages 返回全部 distinct canonical tenant、已发布 PENDING/expired RUNNING 和 valid RUNNING
BrokerRepairSource用独立repair high-water分页列出broker-backed candidate，并把原Guard交给store generation-advance CAS；stale返回no-op，新candidate留下marker=NULL义务，Finish READY后由普通dispatcher重发
WriterRebindSource 映射专用 non-auto-migrate store 的 writer identity、schema/owner/generation invariant、valid-RUNNING count 与同一 high-water RecoverySource；apply 锁前/锁后重读能发现 TOCTOU
OperationJournal把store-private FairQueueOperationRecord与common RecoveryOperationRecord无损双向映射；Read显式传expected writer，所有mutation交回完整expected record/version做CAS
OperationJournal.WithStartFence桥接store pinned-session callback；callback内start read/BeginSpecial复用同一物理连接，MySQL lock释放/异常discard语义不被adapter吞掉
有效 RUNNING 转 RunningLease
expired RUNNING 仅在 lease/next_run due 且 `dispatch_generation=claim_generation` 时 rearm 为 `dispatch_generation=claim_generation+1`/清 marker并保持 RUNNING；重新 delivery 复用 existing reclaim
```

- [x] **Step 2: 把索引 worker 模式显式化**

RAG `Service.Start` 中拆分：

```text
始终启动：DocumentAI reconcile、lifecycle/GC、其它维护循环
legacy mode：现有 local workers + taskPump
paused mode：不启动任何 index worker/taskPump/dispatcher/scheduler；创建任务仍只落 MySQL
fair mode：不启动 legacy index workers，由 fairqueue Runtime 调用 adapter
```

不得通过 Redis/Rabbit 健康状态动态切到 legacy mode；运行模式只由启动配置决定。`paused` 只用于部署 drain，切换必须走 legacy→paused→fair 或反向流程。

- [x] **Step 3: 修改创建任务后的通知**

```text
legacy mode -> scheduleTask(taskID)
paused mode -> 不发进程内通知；durable MySQL task 等后续所选模式扫描
fair mode   -> dispatcher.TryDispatch(taskID) fast path
```

fast path 失败只记录 warn；MySQL `dispatched_at=NULL + dispatch_generation>claim_generation + due` 保证后台扫描恢复。所有产生 replacement/reindex task 的内部路径也必须通知。

- [x] **Step 4: Retry/finish 合约**

确认 `RetryRAGIndexTask`：

```text
status=PENDING
next_run_at=future
dispatch_generation=GREATEST(dispatch_generation, claim_generation)+1
dispatched_at=NULL
release MySQL lease
```

PreparedTask.Run 返回后由 fair runtime 无条件 release Redis reservation；RAG 终态仍由现有 fenced store 方法写入。

PreparedTask 持有 claim 返回的 expected writer fingerprint；pipeline 的成功、retry、fail、supersede/replacement、cancel/quiesce 每条 store mutation 都必须走 Task 9 的 identity-checked execution helper。typed writer mismatch 立即 cancel provider/embedding context并上报 runtime fatal safety state，不能降级为普通 retry。

`runClaim`/heartbeat/finalize 当前的日志型错误边界必须改为可向`PreparedTask.Run`传播typed fatal safety error；adapter在Prepare、Run、dispatch/recovery/journal所有边界集中把store的writer mismatch/unsafe-connection错误映射为`fairqueue.ErrAuthoritativeWriterMismatch`。WriterRebindSource需要的identity重读、schema/invariant、valid-RUNNING count与RecoverySource必须由专用non-auto-migrate store窄接口完整提供，不能从启动缓存伪造或退化为普通`Store`读取。

fair `TaskDocumentAIBudget` 必须优先调用 Task 9 的 `CreateRAGDocumentAITaskBudgetForIndex`，把完整 `IndexFence` 传入同一 pinned live-fence transaction；缺少该窄接口时 fail closed，不能回退到无 fence 的 legacy Create。canonical owner/domain corruption 映射为独立的 `fairqueue.ErrAuthoritativeStateCorrupt`，与 writer mismatch 一样同步关闭 gates、取消全部 PreparedTask并进入 operator-required fatal 状态，不能 NACK hot-loop。

`ErrAuthoritativeStateCorrupt` 的 fatal 边界必须覆盖 Runtime、dispatcher（含 confirm 后 Mark）、expired rearm 与 recovery 的全部 source loop；只有纯 stale-publisher writer mismatch 可以使用旧 generation 豁免，混合携带 corruption 的错误绝不能被豁免。Prepare 检出 authoritative fatal 后不得主动 NACK/EnsureActive 形成跨实例 hot-loop，只做有界 reservation cleanup，由连接关闭保持 delivery unsettled。fair DocumentAI ledger 在构造时按 claim fence 永久绑定 expected-writer facade，Get/Commit/Release 不能依赖调用方继续携带 context marker后退到 legacy pool。

fair pipeline 构造/比对 immutable snapshot 时读取 user embedding 配置必须走 claim expected-writer 绑定的 pinned facade；writer/connection mismatch 时不得创建 replacement、通知 dispatcher 或发出 provider 请求。所有 fair execution-only chunk/cache/object mutation 在 context fence 丢失时 fail closed，不能回退到 legacy pool；共享 cache catalog 在构造时永久绑定 worker mode。

fair API 在 claim 创建前构造 Upload/Reindex immutable snapshot 时，KB/document/user/quota reads 和 user embedding config 也必须走 expected-writer pinned facade；session switch 不得返回 DTO、写 object 或创建/通知 task。original object 的 Begin/READY 使用无 claim 的 expected-writer lifecycle transaction，fair create 必须消费 exact `READY|PUBLISHED` original staging locator；缺行、WRITING/DELETING 或任一 user/KB/doc/kind/key/reference/generation 不一致均零 document/task mutation。仅 legacy create 保留“无 staging 行视为 pre-existing object”的兼容语义。

provider snapshot supersede 只有在 store 返回 `ok=true` 且 replacement task ID/doc/version 完整有效时才可 ACK，并且必须通知所选 worker mode；`ok=true` 但 replacement 缺失/畸形按 authoritative state corruption fail closed。

DocumentAI provider 的 Release/MarkSent/Commit settlement error 不得丢弃，必须与 primary error 合并返回；vision、Office parse 与 enrichment 的 graceful degradation 只能吞普通 provider 错误，writer mismatch、unsafe connection 和 ledger corruption 必须原样上冒，最终映射成 runtime authoritative fatal。

- [x] **Step 5: 验证**

```bash
go test ./internal/rag -run 'Test.*(FairQueue|Pipeline|Retry|Recovery)' -v
go test ./internal/rag/...
go test ./internal/fairqueue ./internal/store
```

- [x] **Step 6: Commit**

```bash
git add docs/superpowers/plans/2026-08-01-rag-fair-queue.md internal/rag internal/fairqueue internal/store
git commit -m "feat(rag): execute index pipeline through fair queue adapter"
```

---

### Task 11: Gateway 装配、启动顺序和健康状态

**Files:**

- Modify: `internal/gateway/gateway.go`
- Create: `internal/gateway/rag_fair_queue.go`
- Create: `internal/gateway/rag_fair_queue_test.go`
- Modify: `internal/fairqueue/rabbit.go`
- Modify: `internal/fairqueue/rabbit_test.go`
- Modify: `internal/fairqueue/redis.go`
- Modify: `internal/fairqueue/runtime.go`
- Modify: `internal/fairqueue/runtime_test.go`
- Modify: `internal/fairqueue/recovery.go`
- Modify: `internal/fairqueue/recovery_test.go`
- Modify: `internal/setup/server.go`
- Create: `internal/setup/handlers_health.go`
- Create: `internal/setup/handlers_health_test.go`
- Modify: `cmd/bkcrab/main.go`

- [ ] **Step 1: 写启动/配置失败测试**

覆盖：

```text
fair disabled + worker mode=legacy：不创建 Rabbit/Redis client，rag legacy Start
worker mode=paused：不创建 claimant，API/其它 RAG maintenance 仍启动并可持久创建 task
worker mode=fair + non-MySQL：启动校验失败
worker mode=fair：先创建 clients/runtime、注册 rag.index，再启动 loops
worker mode=fair：scheduler 默认关闭，startup rebuild/reconcile 完成后才开
worker mode=fair：Rabbit resource/base topology readiness 与 Redis standalone/writable-primary readiness 都在开放resource gate前被证明；任一依赖初始不可用时API以degraded启动并由有界supervisor重试，不能让同步client构造失败杀死gateway，也不能在空库时绕过Rabbit探测开放gate
worker mode=fair：从 claim/heartbeat 所用 MySQL writer 查询 `@@server_uuid`，生成绑定 databaseName 的 fingerprint；查询失败返回启动错误
worker mode=fair：把RAG adapter的OperationJournal注入Runtime；journal ACTIVE/不匹配READY_COMMITTED时scheduler/dispatcher关闭并报告operator-required
READY_COMMITTED + Redis READY last_completed_operation_id精确匹配时可补记COMPLETED；ID/kind/writer任一不符fail closed
Redis resource control 已绑定不同 writer fingerprint：实例状态 failed、scheduler/dispatcher gate 保持关闭，绝不覆盖 control
两个实例同 Redis/Rabbit 但报告不同 writer fingerprint：至多匹配 control 的一组可进入 READY，另一组 fail closed
运行中任一 store 操作报告 pinned-connection writer/session mismatch：状态立即 failed，关闭 scheduler/dispatcher gate、取消全部 PreparedTask；不按 Rabbit/Redis degraded 继续
Redis/Rabbit 初始不可用：gateway 可启动 API degraded 并接受 MySQL task，但 scheduler 保持关闭且不 fallback
shutdown：先停 scheduler/dispatcher，再停 clients
同一 rag.Service 不启动 legacy + fair 两套 index workers
部署测试证明 legacy→paused rollout 完成且 drain 前不启动任何 fair Pod；paused→fair 后不存在 legacy worker；反向回滚同理
enabled=false+mode=fair启动失败；enabled=true+legacy/paused允许供其它registered resource使用共享clients但绝不启动RAG fair loop；gateway只能switch `workerMode`决定RAG loop
```

- [ ] **Step 2: 装配 Runtime**

gateway 负责：

```text
从 EnvConfig 创建 Rabbit/Redis client
从 authoritative writer 读取/缓存 expected identity，并把 fingerprint 传给所有 coordinator/recovery/store execution fence；缓存不替代 Task 9 每连接复验
创建 fairqueue.Runtime
用 RAG adapter 注册 rag.index 的source/preparer与OperationJournal，并把journal传给Runtime/EnsureResourceReady
向 RAG Service 注入 fair notifier/mode
启动 runtime 和 RAG maintenance loops
```

当前Redis client构造会同步执行INFO/ROLE，而Rabbit只提供per-tenant topology方法；实现时需增加可重试的fair-runtime supervisor和resource/base topology readiness probe（或等价的lazy/probed client边界）。supervisor在依赖未就绪时保持scheduler/dispatcher gate关闭但不阻止MySQL-backed API启动；探测必须在startup barrier开放resource前完成。fairqueue Runtime/Recovery还需提供不含敏感token的只读health snapshot，setup handler不得凭启动时间或本地布尔伪造resource/journal/loop状态。

不实现 Redis/MySQL 运行时 mode fence，因为旧二进制不会遵守它，容易制造虚假的安全感。从 legacy 切 fair 必须采用维护窗口和三态本地配置：先将所有兼容实例从 legacy 滚动到 paused（混合期只有 legacy claim），确认旧 Pod/进程为零并在 paused 阶段等待有效 RUNNING 完成或 lease 到期；再从 paused 滚动到 fair（混合期只有 fair claim）。禁止直接 fair/legacy canary 混跑；回滚严格走 fair→paused（在 paused 阶段 drain）→**兼容 dual-write release**的 legacy。contract 后禁止启动 pre-expand binary。

若现有 gateway 构造函数过大，抽取 `buildFairQueueRuntime` 私有工厂并用接口 fake 测试，不把连接细节散落到 handler。

- [ ] **Step 3: 分离 liveness、readiness 与依赖健康状态**

保留现有路径兼容性，但把 liveness/readiness 从共用 handler 中分离并写 handler/probe 测试：

```text
/livez  ：仅进程/event-loop 存活；Rabbit/Redis/MySQL 短时故障不杀 Pod
/readyz ：HTTP/API 已初始化、MySQL 可接受 durable task、配置/schema 合法；
          Rabbit/Redis degraded 不撤掉 API endpoint，但 startup schema 不兼容返回 503
/healthz：保留现有轻量兼容响应，不承载依赖详情，也不作为 liveness/readiness 的替代品
/api/admin/health/fairqueue：经 requireSuperAdmin 保护的详细 operational JSON
```

admin detail 内容：

```json
{
  "fairQueue": {
    "enabled": true,
    "status": "healthy|degraded|failed",
    "mode": "legacy|paused|fair",
    "mysql": {
      "status": "ok|unavailable|mismatch",
      "schemaReady": true,
      "writerTopology": "single",
      "writerFingerprint": "short-non-secret-hash",
      "controlFingerprintMatch": true,
      "lastConnectionIdentityVerifiedAt": "RFC3339|null",
      "sessionAffinity": "verified|unknown|mismatch",
      "operationJournal": {
        "phase": "NONE|ACTIVE|READY_COMMITTED|COMPLETED",
        "kind": "NONE|RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD",
        "operationIdFingerprint": "short-non-secret-hash|null"
      }
    },
    "rabbit": {
      "status": "ok|unavailable",
      "readyDepthSample": 4,
      "dlqDepthSample": 0,
      "lastConfirmAt": "RFC3339|null",
      "lastReturnAt": "RFC3339|null"
    },
    "redis": {
      "status": "ok|recovering|unavailable",
      "mode": "standalone",
      "clusterEnabled": false,
      "resourceState": "READY|RECOVERING",
      "operationKind": "NONE|NORMAL|RABBIT_REPAIR|WRITER_REBIND|FORCE_REBUILD",
      "operatorRequired": false,
      "operationIdFingerprint": "short-non-secret-hash|null",
      "lastCompletedOperationIdFingerprint": "short-non-secret-hash|null",
      "epochFingerprint": "short-non-secret-hash",
      "provisionalCount": 0,
      "processingCount": 0
    },
    "recovery": {
      "startup": "pending|running|complete|failed",
      "pagesCompleted": 12,
      "converged": true,
      "operationPassComplete": true
    },
    "loops": {
      "scheduler": {"state": "starting|running|paused", "lastSuccessAt": "RFC3339|null", "lagSeconds": 0},
      "dispatcher": {"lastSuccessAt": "RFC3339|null", "lagSeconds": 0},
      "sweeper": {"lastSuccessAt": "RFC3339|null", "lagSeconds": 0},
      "reconciler": {"lastSuccessAt": "RFC3339|null", "lagSeconds": 3}
    }
  }
}
```

`operatorRequired=true`覆盖journal ACTIVE、无匹配READY control的READY_COMMITTED，以及未完成journal/control kind/ID/writer任一不一致；COMPLETED视为无未完成special，Redis丢失后可NORMAL rebuild。不能只看Redis kind。不可输出raw operation ID/resource epoch、recovery owner/token、DSN、password、Rabbit URL credentials、tenant/task标识。Kubernetes liveness/readiness probes必须分别指向`/livez`与`/readyz`，不能用admin detail或`/healthz`造成依赖故障时反复重启。

- [ ] **Step 4: 验证**

```bash
go test ./internal/gateway -run 'Test.*FairQueue' -v
go test ./internal/setup -run 'Test.*Health' -v
go build ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/gateway internal/setup
git commit -m "feat(gateway): wire rag fair queue runtime and health state"
```

---

## Phase D：可观测性、部署、集成验收和发布闸门

### Task 12: Fairqueue telemetry 与告警合约

**Files:**

- Create: `internal/fairqueue/telemetry.go`
- Create: `internal/fairqueue/telemetry_test.go`
- Modify: `internal/fairqueue/dispatcher.go`
- Modify: `internal/fairqueue/dispatcher_test.go`
- Modify: `internal/fairqueue/rabbit.go`
- Modify: `internal/fairqueue/rabbit_test.go`
- Modify: `internal/fairqueue/redis.go`
- Modify: `internal/fairqueue/redis_test.go`
- Modify: `internal/fairqueue/scheduler.go`
- Modify: `internal/fairqueue/scheduler_test.go`
- Modify: `internal/fairqueue/runtime.go`
- Modify: `internal/fairqueue/runtime_test.go`
- Modify: `internal/fairqueue/recovery.go`
- Modify: `internal/fairqueue/recovery_test.go`
- Modify: `internal/fairqueue/recovery_operators.go`
- Modify: `internal/fairqueue/recovery_operators_test.go`
- Modify: `internal/rag/fair_queue.go`
- Modify: `internal/rag/fair_queue_test.go`
- Modify: `internal/rag/telemetry/telemetry.go`
- Modify: `internal/rag/telemetry/telemetry_test.go`
- Modify: `internal/store/rag_task_claim.go`
- Modify: `internal/store/rag_task_claim_test.go`
- Modify: `internal/gateway/rag_fair_queue_test.go`

- [ ] **Step 1: 先写 telemetry 失败测试**

固定低基数标签（resource、outcome、reservation kind、dependency），禁止 tenant/task/token/URL 成为 metric label，并覆盖：

```text
dispatch scan count/oldest due age、publish confirm/return/timeout、mark CAS stale
resource Rabbit ready-depth sample、task queue-wait/claim/run duration
expired RUNNING armed/redispatched/oldest expired age
active/ring/ring_members、processing turn、provisional/stable inflight 数
reservation granted/denied/renew/release/promote outcome
MySQL claim advisory-lock wait+timeout、claim capacity deny、heartbeat fence lost
DLQ routed count/depth sample、duplicate/stale/canonical repair disposition
resource epoch/state/writer-fingerprint mismatch、scheduler gate、recovery duration/pages/fence-lost、continuous correction count
Rabbit/Redis availability transition；日志会 scrub credential/raw epoch/owner/reservation token
```

- [ ] **Step 2: 实现窄 telemetry sink 并接线**

fairqueue 核心依赖无业务类型的 sink/no-op 实现；Task 4/5/6/7/8 创建的 dispatcher、Rabbit、Redis、scheduler/runtime/recovery/operator 路径都在本任务显式注入同一 sink，RAG adapter 与 MySQL claim helper 映射到现有 telemetry，gateway 负责生命周期装配。指标采集失败不得影响 claim/ACK/fence。结构化日志可带 resource、outcome、task ID、dispatch generation、随机 PublishAttemptID、哈希 tenant 和安全错误类别，以关联 confirm/return/DLQ；这些值不得成为 metric label。日志不带原始 tenant ID、task payload、raw resource epoch、reservation/recovery owner token 或凭据。

- [ ] **Step 3: 验证告警所需信号**

用 fake sink 断言 failure path 恰好计数，至少能基于以下条件告警：oldest undispatched/expired age 持续增长、DLQ 非零、scheduler 长期 paused、recovery fence lost/反复 rebuild、advisory-lock timeout、Redis stable reservation 与 MySQL RUNNING 持续不一致。

- [ ] **Step 4: 验证并 Commit**

```bash
go test ./internal/fairqueue -run 'TestTelemetry' -v
go test ./internal/rag/telemetry -run 'Test.*FairQueue' -v
go test ./internal/rag -run 'Test.*FairQueue.*Telemetry' -v
go test ./internal/store -run 'TestRAG.*(Claim|Telemetry)' -v
go test ./internal/gateway -run 'TestRAG.*FairQueue' -v
git add internal/fairqueue internal/rag/fair_queue.go internal/rag/fair_queue_test.go internal/rag/telemetry internal/store/rag_task_claim.go internal/store/rag_task_claim_test.go internal/gateway/rag_fair_queue_test.go
git commit -m "feat(queue): expose fair scheduling telemetry"
```

---

### Task 13: Docker/多 Pod/Helm/Kubernetes 接线

**Files:**

- Modify: `deploy/docker/docker-compose.yml`
- Modify: `deploy/docker/docker-compose.rag.yml`
- Modify: `deploy/docker/.env.example`
- Modify: `deploy/docker/README.md`
- Modify: `deploy/multi-pod/docker-compose.yaml`
- Modify: `deploy/helm/bkcrab/values.yaml`
- Modify: `deploy/helm/bkcrab/templates/configmap.yaml`
- Modify: `deploy/helm/bkcrab/templates/secrets.yaml`
- Modify: `deploy/helm/bkcrab/templates/gateway.yaml`
- Create: `deploy/helm/bkcrab/templates/redis.yaml`
- Create: `deploy/helm/bkcrab/templates/rabbitmq.yaml`
- Modify: `deploy/k8s/bkcrab.yaml`
- Modify: `internal/setup/rag_deployment_test.go`

- [ ] **Step 1: Docker Compose**

增加：

```text
Redis（持久卷、healthcheck）
RabbitMQ management（持久卷、healthcheck、非默认生产凭据提示）
bkcrab 环境变量和 depends_on healthy
fair profile 显式 `BKCRAB_FAIR_QUEUE_MYSQL_WRITER_TOPOLOGY=single`；claim/heartbeat/count 使用同一个 MySQL writer DSN
```

开发默认仍可通过 `BKCRAB_FAIR_QUEUE_ENABLED=false` 不启动公平调度；RAG 公平 profile 显式开启。

- [ ] **Step 2: multi-pod 验证拓扑**

两个 bkcrab gateway 共享：

```text
同一 MySQL
同一 Redis
同一 RabbitMQ vhost/exchange
不同进程 worker IDs
两个 Pod 查询到相同 `@@server_uuid`/database-bound fingerprint；不支持多 primary 或按 tenant 分片 writer
```

- [ ] **Step 3: Helm/K8s**

values 同时支持：

- chart 内置 Redis/Rabbit；
- 外部 Redis/Rabbit endpoint；
- secrets 与普通 config 分离；
- PVC、resource requests、readiness/liveness；
- Rabbit/Redis 不对公网暴露。
- `mysqlWriterTopology=single` 与 worker mode 一起渲染；不提供误导性的 multi-writer fair 选项。

chart/manifest 必须显式暴露 `ragIndex.workerMode=legacy|paused|fair`，默认 legacy。运维文档与渲染测试必须证明切换分两次 rollout：先全量 legacy→paused 并在 paused 阶段 drain，再 paused→fair；绝不能在一次 RollingUpdate 中直接把 legacy 改成 fair。回滚使用 fair→paused（在 paused 阶段 drain）→兼容 dual-write release 的 legacy；contract 后镜像约束阻止 pre-expand binary。若部署平台无法证明每一阶段旧 ReplicaSet 已归零，则保持 paused 并阻止下一阶段。

- [ ] **Step 4: 渲染/配置验证**

```bash
docker compose -f deploy/docker/docker-compose.yml config
docker compose -f deploy/multi-pod/docker-compose.yaml config
helm template bkcrab deploy/helm/bkcrab
# 本任务同步扩展 internal/setup/rag_deployment_test.go，解析 direct manifest并校验 env/probe/rollout gate；Task 14 再扩展跨组件场景
go test ./internal/setup -run 'TestRAG.*(Deployment|Rollout)' -v
```

检查输出不含未替换 secret placeholder，不把测试 guest 凭据写成生产默认；fair profile 必须同时渲染 `workerMode=fair`、enabled=true 和 writer topology=single，paused/legacy profile 不得创建 claimant。

- [ ] **Step 5: Commit**

```bash
git add deploy internal/setup/rag_deployment_test.go
git commit -m "deploy(queue): add redis rabbitmq for fair scheduling"
```

---

### Task 14: 端到端公平、借用、重试和故障测试

**Files:**

- Create: `internal/fairqueue/integration_test.go`
- Create: `internal/rag/fair_queue_integration_test.go`
- Modify: `internal/setup/rag_deployment_test.go`
- Test directly parses Task 13 的 `deploy/docker/*`、`deploy/multi-pod/docker-compose.yaml`、Helm templates 与 `deploy/k8s/bkcrab.yaml`；不另建未命名 fixture

- [ ] **Step 1: 建立 env-gated harness**

需要：

```text
BKCRAB_TEST_MYSQL_DSN
BKCRAB_TEST_REDIS_ADDR
BKCRAB_TEST_RABBITMQ_URL
```

每个 test 使用唯一 exchange/key prefix，结束后只清理自己的 namespace。

- [ ] **Step 2: 公平性验收**

使用可控 blocking fake RAG executor：

```text
Scenario A：A 8 tasks，B/C 无 backlog
  断言 A 可以运行4（借用）

Scenario B：A 已运行4，B 新增4
  释放一个 A 后 B 首先启动
  持续释放后收敛 A=2/B=2

Scenario C：A/B/C 各持续 backlog
  断言启动用户序列无饥饿
  任意时刻 global<=4
  单用户有竞争时不继续获得 borrowed slot
```

- [ ] **Step 3: delivery/claim/retry 验收**

```text
重复 publish 同一 task -> 只有一个 claim generation 运行
next_run_at 前 Rabbit 没有新的 ready 消息
到期后重新发布并重新进入 ring
tenant payload 篡改 -> task 不运行
body 完全损坏但 stable headers 可定位 -> canonical generation repair、confirmed DLQ、ACK，合法 task 以新 generation 重发
body 合法但 header 缺失/坏类型，以及 body/header identity mismatch -> Message=nil、受约束 locator repair、confirmed DLQ，永不执行；mismatch 断言所有独立验证成功的候选最终都有新 generation 发布义务
body/header 都不可定位 -> confirmed DLQ 后 ACK，且测试证明它不是 fair publisher 可产生的 canonical v1
claim 后 ACK，worker crash -> lease expiry 后恢复
expired RUNNING 的旧 duplicate 在 sweeper arm 前不能 reclaim；reclaim message 已 Mark 后多轮 sweeper 不再推进 generation
claim 已提交但 Redis promote 失败 -> ACK、不 Run、不 heartbeat；lease 到期后最终由新 generation reclaim
maintenance 覆盖 PENDING 与 expired RUNNING：future next_run 前无 Rabbit hot loop，到期后恢复
poison/mismatch 先 canonical repair、再 confirmed DLQ、最后 ACK；DLQ 失败时原 delivery requeue
publish 成功后、Mark 前修改同 generation guard 字段 -> Mark false；不得覆盖新 snapshot
heartbeat/sweeper 竞态 -> heartbeat 先赢则 sweeper 不 rearm，sweeper 先赢则旧 heartbeat 永久 fence lost
```

- [ ] **Step 4: 故障注入**

```text
停 Rabbit：上传成功，dispatched_at 为空；恢复后发布
停 Redis：不启动新任务；running 继续
清 Redis：恢复后 active/inflight 从 MySQL 重建
启动时以数万 task、很小 page size 做 high-water rebuild：查询均为 keyset、有界 page，峰值内存不随全表线性增长；持续创建新 task 也能 READY
recovery owner 扫描中途失锁：新 owner takeover 完成，旧 owner 的后续 restore/finish 全被 fence
RECOVERING 前遗留 provisional/processing：未到期不强删，到期后 owner cleanup 清掉所有索引并最终 READY；metadata 损坏保持非 READY
metadata 损坏后 redis-force-rebuild dry-run/缺确认零 mutation；apply 等待窗口、只删本 resource、完整 rebuild，失锁 takeover 可重入
Rabbit repair rearm半页后同时清空Redis：NORMAL启动被MySQL ACTIVE journal挡住；同kind命令按原operation ID rehydrate、完成余下row并在READY后重发
special在READY_COMMITTED→Redis Finish前后分别崩溃：前者保持operator-required，后者仅凭匹配last-completed ID补记COMPLETED；writer runtime保持停止时重跑CLI走terminal-reconcile且零Begin，不误开/永久卡gate
kill 一个 bkcrab 实例：另一个实例在 lease 到期后接管
publisher confirm 后模拟 mark 失败：重复消息不双执行
Mark 成功、Activate 前 kill dispatcher：startup/continuous MySQL dispatched reconciliation 最终把有 ready depth 的 tenant 激活
Rabbit 数据清空运维 reset 总是推进 generation，灾难前迟到消息不能 claim
writer 透明切换/connection ID 不粘连 -> claim/heartbeat及所有 execution mutation 前 fail closed、物理 discard、runtime取消 pipeline；rebind 缺确认/TOCTOU拒绝，满足前置后 expected-old CAS + rebuild
Redis rebuild 用 DB_NOW 剩余时长映射 Redis TIME，注入节点时钟偏差仍不提前释放 stable token
```

- [ ] **Step 5: 多实例全局约束**

同时启动两个 runtime，各 `localWorkers=4`，断言合计 running 永不超过 4。另用同一 Redis control 注入两个不同 writer fingerprint，断言 identity mismatch 的 runtime 在任何 claim/publish 前 fail closed。

再人为删除 Redis inflight keys 并并发触发新调度，验证 MySQL 精确 claim 的最终闸门仍使有效 `RUNNING` 保持 global<=4、user burst<=4。

- [ ] **Step 6: migration 与两阶段 rollout 验收**

在隔离 MySQL/部署 fixture 上覆盖：expand 后旧 INSERT 省略新列仍成功；兼容 writer dual-write 与 keyset backfill 收敛；contract dry-run 在 NULL/缺 attestation 时拒绝、满足后 apply 并由 INFORMATION_SCHEMA 证明 `user_id IS_NULLABLE='NO'`。渲染并执行 `legacy -> paused（在 paused 阶段 drain） -> fair` 两次独立 rollout，断言任何时刻都不存在 legacy claimant 与 fair claimant 混跑；回滚路径同理。

- [ ] **Step 7: 验证**

```bash
go test ./internal/fairqueue -run TestIntegration -v
go test ./internal/rag -run TestFairQueueIntegration -v
go test ./internal/setup -run 'TestRAG.*(Deployment|Rollout|FairQueue)' -v
```

- [ ] **Step 8: Commit**

```bash
git add internal/fairqueue/integration_test.go internal/rag/fair_queue_integration_test.go internal/setup/rag_deployment_test.go
git commit -m "test(queue): cover multi-tenant fairness and crash recovery"
```

---

### Task 15: 运维 CLI、文档、全量回归、滚动迁移与发布检查

**Files:**

- Modify: `cmd/bkcrab/cmd_admin.go`
- Create: `cmd/bkcrab/cmd_admin_fairqueue.go`
- Create: `cmd/bkcrab/cmd_admin_fairqueue_test.go`
- Modify: `docs/database.md`
- Create: `docs/rag-fair-queue-operations.md`
- Modify: `deploy/docker/README.md`
- Modify: `deploy/multi-pod/README.md`
- Modify: this plan checkboxes during implementation

- [ ] **Step 1: 实现受确认保护的运维 CLI**

在现有 `bkcrab admin` 下注册：

```text
bkcrab admin fairqueue contract-migrate [--apply] [--confirm-all-writers-dual-write]
bkcrab admin fairqueue rabbit-disaster-repair --resource <registered-resource> [--apply]
  [--confirm-old-broker-isolated] [--confirm-publishers-paused]
bkcrab admin fairqueue rebind-writer --resource <registered-resource> [--apply]
  [--expected-old-writer-fingerprint <64hex>] [--confirm-old-writer-fenced]
  [--confirm-resource-runtimes-stopped] [--confirm-new-writer-authoritative]
bkcrab admin fairqueue redis-force-rebuild --resource <registered-resource> [--apply]
  [--confirm-discard-redis-coordination-state]
```

不带 `--apply` 一律为只读 dry-run/check；apply 缺少对应全部确认 flag 时在任何 mutation 前失败。三个 resource 命令的 `--resource` 都是必填 flag，并且只能选择 CLI 已注册对应 source/operator/journal adapter 的 resource；本 RAG 任务完成时只有 `rag.index`，未知值在连接/写入前拒绝，后续 Imagegen 可显式注册 `image.generate`。rebind的`--confirm-resource-runtimes-stopped`表示claimant/publisher/scheduler/recovery coordinator已由平台停止且保持到journal COMPLETED；CLI不能自行推断。命令使用专用 MySQL-only/non-auto-migrate opener及所需 Redis/Rabbit client，不复用当前 `openStoreFromEnv()`，也不启动 gateway。Cobra 层只做 `Args: cobra.NoArgs`、flag/attestation、依赖装配和安全输出：contract 调 Task 2 API，Rabbit/rebind/force 命令调 Task 8 operator 与 Task10 RAG sources/journal；core apply 自身必须重验，不能信任 CLI dry-run report。测试用 injected runner 固定默认 dry-run、required resource、unknown resource、缺确认零 mutation、context cancel、TOCTOU、错误退出和幂等重入；输出不得含 DSN/凭据/raw operation ID/epoch/tenant/task identity。

- [ ] **Step 2: 更新数据库文档**

记录：

- `rag_index_tasks.user_id` 来源与不可变约束；
- `dispatched_at` 的 durable publish marker 语义；
- `fairqueue_resource_operations`每resource安全journal的字段、phase/CAS与“不是outbox”语义；
- PENDING/RUNNING 与 `next_run_at`、lease 的组合状态；
- MySQL-only fairqueue enablement；
- broker 数据灾难时必须在 recovery fence 下推进 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1` 并清 marker；明确禁止只重置 dispatched marker。

- [ ] **Step 3: 更新运维文档**

覆盖：

```text
Redis/Rabbit 依赖与容量
legacy→paused（在 paused 阶段 drain）→fair 与反向回滚顺序
healthy/degraded 判读
Redis rebuild 操作
Rabbit 数据丢失重发操作
四个 admin fairqueue 命令的 dry-run/apply 示例、确认门槛、中断重入和预期 health/telemetry
special operation崩溃后MySQL journal与Redis operationKind/ID的对账、operatorRequired判读；Redis全丢仍只可重跑同kind/同ID命令，normal runtime不会代为Finish
固定MySQL start lock→Redis raw lock→control preflight→journal CAS→Begin顺序，以及三类operator允许起态矩阵
writer rebind从首次apply到COMPLETED保持该resource runtime/recovery coordinator停止的操作步骤
计划内 writer failover/rebind 与 Redis corrupt-metadata force rebuild；无法证明前置时保持 failed/RECOVERING
每一 rollout 阶段验证旧 ReplicaSet/进程归零、防止双 worker 的步骤
contract 后 rollback floor：只允许回到已支持 nullable/NOT NULL schema且持续 dual-write 的兼容 release，禁止回到 pre-expand binary
```

- [ ] **Step 4: 全量静态与单元回归**

```bash
go vet ./...
go build ./...
go test ./...
```

- [ ] **Step 5: 真实依赖集成回归**

```text
MySQL 8
Redis configured persistence
RabbitMQ durable queues/messages
单实例
双实例
```

保存关键验收证据：global max、A 独占借用、B 到来后的下一槽优先、2/2 收敛、Redis/Rabbit 故障恢复、分页 rebuild 内存界、writer fingerprint fence、heartbeat/sweeper 排序和 mark→activate crash 修复。

- [ ] **Step 6: 执行并验证 expand/backfill/contract 里程碑**

发布顺序必须留下可审计证据：先在 legacy 模式部署兼容 expand/dual-write release，验证旧 INSERT 兼容并确认所有更旧 writer 已归零；运行 `contract-migrate` dry-run保存 aggregate remaining/page-count/invariant报告，再带 attestation apply，由apply自己从头完成最终 bounded keyset backfill至零差异后contract；最后直接从 INFORMATION_SCHEMA验证 `rag_index_tasks.user_id IS_NULLABLE='NO'`，并重跑零NULL/owner/generation检查。任何一步失败都停止在兼容legacy/paused，不开启fair；contract不由startup auto-migrate代做。contract一旦完成，最低可回滚版本就是该兼容dual-write release；若必须回pre-expand binary，只能在contract前做出决定。

- [ ] **Step 7: 灰度发布闸门**

全部满足后才能开启：

- migration expand/backfill/contract 已完成，`user_id NOT NULL` 已从 INFORMATION_SCHEMA 验证，历史 user_id 零缺失且 generation invariant 校验通过；
- Rabbit/Redis 健康与持久卷已验证；
- `undispatched oldest age`、duplicate delivery、recovery count 有可观测数据；
- 兼容 legacy 全量完成后，另一次 rollout 到 paused 并 drain；只有旧 ReplicaSet/claimant 为零才做下一次 paused→fair rollout。单实例 fair 启动无 task 丢失/双执行；不得与 legacy 做 canary；
- 回滚步骤演练完成。
- contract 后的 rollback floor 已写入镜像/Helm pinning 与 runbook，演练不会启动 pre-expand binary。

- [ ] **Step 8: Commit**

```bash
git add cmd/bkcrab/cmd_admin.go cmd/bkcrab/cmd_admin_fairqueue.go cmd/bkcrab/cmd_admin_fairqueue_test.go docs/database.md docs/rag-fair-queue-operations.md deploy/docker/README.md deploy/multi-pod/README.md docs/superpowers/plans/2026-08-01-rag-fair-queue.md docs/superpowers/specs/2026-08-01-rag-fair-queue-design.md
# 禁止 `git add docs` 或把其它 untracked 文档带入提交
git commit -m "docs(queue): document rag fair scheduling operations"
```

---

## 实施顺序与并行边界

强依赖主干：

```text
Task 1 -> Task 2 -> Task 3 -> Task 4/5 -> Task 6/7 -> Task 8
       -> Task 9 -> Task 10 -> Task 11 -> Task 12/13 -> Task 14 -> Task 15
```

允许并行但合并前必须对齐接口：

- Task 4 Rabbit 与 Task 5 Redis 可在 Task 3 契约固定后并行；
- Task 9 MySQL exact claim 可与 Task 6/7 通用 runtime 并行；
- Task 13 部署清单可在配置名由 Task 1 固定后开始，但必须等 Task 11 健康语义定稿后收尾；
- Task 14 必须在 RAG adapter 与 recovery 都完成后执行；
- Task 15 的 contract milestone 必须在任何 fair rollout 前完成，Rabbit disaster CLI 在演练环境通过后才进入 runbook。

## 自审清单

- [ ] 没有新增通用 jobs/outbox/tenant 业务表；仅有每 resource 一行的 special-recovery safety journal。
- [ ] `rag_index_tasks` 只新增 spec 约定的最小持久字段。
- [ ] fairqueue 包不 import RAG/store 领域类型。
- [ ] Rabbit payload 不含业务大对象或凭据。
- [ ] publisher confirm 前绝不标记 dispatched。
- [ ] Redis 是调度协调层，MySQL 是恢复和业务正确性来源。
- [ ] Redis 失效时不启动无公平 fallback worker。
- [ ] fair mode 与 legacy index worker 不会同时运行。
- [ ] exact claim 复用完整 document/task/version lock/fence 语义。
- [ ] Redis reservation 是逐任务/逐 reservation 可续约 lease，不是聚合 TTL 计数器。
- [ ] global=4、base=2、burst=4、borrow=true 有自动化验收。
- [ ] retry 在 next_run_at 前不产生 Rabbit hot loop。
- [ ] 两实例合计 inflight 不超过4。
- [ ] imagegen 可通过新 resource/source/handler 接口接入，无需修改 fairqueue 核心。
