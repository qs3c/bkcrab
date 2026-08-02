# Imagegen Batch 与多用户公平调度实施计划

> **For implementation:** 按任务逐项执行，使用 checkbox（`- [ ]`）跟踪。每个任务先写失败测试，再实现，再跑局部与相关回归。除显式标注的 MySQL/Redis/RabbitMQ/object-store 集成测试外，普通单测使用 fake，不依赖外部服务。

**Goal:** 把当前同步、单 prompt、最多 4 张且直接返回 URL/base64 Markdown 的 `image_gen`，改造成最多 16 张、支持两种 create 输入、持久可恢复并按用户公平执行的 `image_gen_batch`；图片落入现有 workspace/object store，工具同时支持 create/status/cancel。

**Architecture:** `image_gen_batch` 先在 MySQL 事务中创建 `image_generation_batches` 和按 prompt/chunk 拆分的 `image_generation_tasks`。通用 `internal/fairqueue` 以 `image.generate` resource 按用户 Round Robin 调度 task；Redis 做公平预约，MySQL exact claim/heartbeat 在同一 resource advisory lock 下提供 global=4、user burst=4 最终闸门。`ImageGenerationService.Generate(count<=4)` 使用冻结的无密钥 provider plan 和执行时解析的当前 secret，执行 capability-aware fallback；每个物理 provider call 还受 provider Redis lease 限制。worker 把完整图片写入 generation-specific workspace prefix，最后写 manifest，再 fenced finalize task 和聚合 batch。工具有限等待，超时只返回 batch ID，不取消后台任务。

**Tech Stack:** Go；MySQL 8；已落地的 `internal/fairqueue`（RabbitMQ + Redis）；现有 `workspace.Store` LocalFS/S3；现有 tool provider 配置和 Agent tool registry。

**Spec:** [`docs/superpowers/specs/2026-08-02-imagegen-batch-fair-queue-design.md`](../specs/2026-08-02-imagegen-batch-fair-queue-design.md)

**前置条件:** [`RAG 公平队列实施计划`](2026-08-01-rag-fair-queue.md) 中通用 `internal/fairqueue` 的 message/DispatchToken、mandatory publish + DLQ、resource epoch、processing-turn lease、两阶段 reservation、dispatcher、runtime 和 recovery 合约已经实现并通过组件测试。Imagegen 不复制或弱化这些协议。

**范围:** 本计划覆盖 Imagegen 领域模型、MySQL 两表、provider typed service/fallback、配置重解析、artifact/manifest、provider 子限流、fairqueue adapter、工具协议、网关装配、健康检查、部署与集成验收。RAG 业务代码不在本计划；仅允许对通用 fairqueue 做领域无关的必要接口补齐。

**兼容:** `BKCRAB_IMAGEGEN_BATCH_MODE=legacy|drain|fair`，默认 legacy。legacy 只注册现有同步 `image_gen`；fair 只注册 action-aware `image_gen_batch` 并允许 create/status/cancel；drain 保持 batch runtime与 status/cancel、拒绝新 create且不恢复旧同步工具。公平模式一期只支持 MySQL authoritative single writer；多实例必须使用共享 workspace backend。

**安全闸门:** Rabbit 消息不得含 prompt/provider secret/artifact；MySQL/Redis/log/tool text 不得含 base64；status/cancel 必须校验 canonical batch owner；provider URL 必须经过受限 downloader；旧二进制的同步 `image_gen` 会绕过公平队列，因此切换时禁止新旧工具混跑。

---

## 文件结构总览

```text
internal/config/config.go
internal/config/env.go
internal/config/config_defaults_test.go
internal/config/imagegen_test.go

internal/imagegen/model.go
internal/imagegen/model_test.go
internal/imagegen/planner.go
internal/imagegen/planner_test.go
internal/imagegen/service.go
internal/imagegen/service_test.go
internal/imagegen/artifacts.go
internal/imagegen/artifacts_test.go
internal/imagegen/artifacts_integration_test.go
internal/imagegen/batch.go
internal/imagegen/batch_test.go
internal/imagegen/provider_limiter.go
internal/imagegen/provider_limiter_test.go
internal/imagegen/fair_queue.go
internal/imagegen/fair_queue_test.go
internal/imagegen/integration_test.go
internal/imagegen/fair_queue_integration_test.go
internal/imagegen/integration_harness_test.go

internal/toolproviders/registry.go
internal/toolproviders/registry_imagegen_test.go
internal/toolproviders/imagegen/imagegen.go
internal/toolproviders/imagegen/openai.go
internal/toolproviders/imagegen/fal.go
internal/toolproviders/imagegen/replicate.go
internal/toolproviders/imagegen/*_test.go

internal/store/database.go
internal/store/database_mysql.go
internal/store/store.go
internal/store/imagegen.go
internal/store/imagegen_repair.go
internal/store/imagegen_claim.go
internal/store/imagegen_test.go
internal/store/imagegen_claim_test.go
internal/store/imagegen_mysql_test.go
internal/store/fairqueue_mysql_fence.go
internal/store/fairqueue_mysql_fence_test.go
internal/store/rag_task_claim.go
internal/store/rag_task_claim_test.go

internal/agent/tools/image_gen.go
internal/agent/tools/image_gen_batch.go
internal/agent/tools/image_gen_batch_test.go
internal/agent/tools/result.go
internal/agent/tools/result_test.go
internal/agent/loop.go
internal/agent/loop_imagegen_metadata_test.go

internal/gateway/gateway.go
internal/gateway/userspace.go
internal/gateway/imagegen_provider_resolver.go
internal/gateway/imagegen_provider_resolver_test.go
internal/gateway/imagegen_test.go

internal/setup/handlers_tools.go
internal/setup/handlers_health.go
internal/setup/handlers_health_test.go
internal/setup/imagegen_deployment_test.go

cmd/bkcrab/cmd_admin_fairqueue.go
cmd/bkcrab/cmd_admin_fairqueue_test.go

deploy/docker/docker-compose.yml
deploy/docker/.env.example
deploy/docker/README.md
deploy/multi-pod/docker-compose.yaml
deploy/multi-pod/README.md
deploy/helm/bkcrab/values.yaml
deploy/helm/bkcrab/templates/configmap.yaml
deploy/helm/bkcrab/templates/gateway.yaml
deploy/k8s/bkcrab.yaml

docs/database.md
docs/imagegen-batch-operations.md
docs/rag-fair-queue-operations.md
```

依赖方向：

```text
agent tool
  -> imagegen.BatchService

gateway
  ├─ provider config resolver
  ├─ imagegen BatchService
  ├─ imagegen fair adapter
  └─ fairqueue.Runtime

imagegen
  ├─ store interfaces
  ├─ workspace.Store
  ├─ typed image providers
  ├─ provider limiter
  └─ fairqueue domain-neutral interfaces

toolproviders/imagegen
  -> HTTP/provider protocol only

fairqueue
  - must not import imagegen/store
```

---

## Phase A：固定工具契约、配置和领域模型

### Task 1: 配置、身份上下文、输入模型与确定性拆分

**Files:**

- Modify: `internal/config/config.go`
- Modify: `internal/config/env.go`
- Modify: `internal/config/config_defaults_test.go`
- Create: `internal/config/imagegen_test.go`
- Create: `internal/imagegen/model.go`
- Create: `internal/imagegen/model_test.go`
- Create: `internal/imagegen/planner.go`
- Create: `internal/imagegen/planner_test.go`
- Modify: `internal/agent/tools/registry.go`
- Modify: `internal/agent/tools/registry_forturn_test.go`

- [ ] **Step 1: 写配置失败测试**

固定默认值：

```text
mode=legacy
maxImagesPerBatch=16
maxImagesPerTask=4
toolWaitDefault=180s
toolWaitMax=240s
promptMaxRunes=8000
requestMaxBytes=128KiB
imageMaxBytes=20MiB
batchMaxBytes=128MiB
localWorkers=4
global=4
base=2
burst=4
borrow=true
taskLease=180s
taskHeartbeat=30s
reservationTTL=180s
reservationHeartbeat=30s
prepareTimeout=10s
provisionalTTL=15s
processingTurnTTL=15s
publishAttemptTimeout=15s
recoveryDrainTimeout=2m
dispatchInterval=1s
reconcileInterval=30s
expiredSweepInterval=15s
reconcilePageSize=200
maxRetries=3
providerCallTimeout=120s
providerConcurrencyDefault=4
```

校验测试：

```text
1 <= maxImagesPerBatch <= 16
1 <= maxImagesPerTask <= 4
base <= burst <= global
heartbeat < taskLease
reservationHeartbeat < reservationTTL
prepareTimeout < provisionalTTL；provisionalTTL/processingTurnTTL/publishAttemptTimeout < recoveryDrainTimeout
所有duration/page size为正且有部署级上限；一期统一duration上限为24h、page size上限为10000；零publishAttemptTimeout不得表示无deadline
waitDefault <= waitMax <= 240s
batchMaxBytes >= imageMaxBytes
fair/drain + storage!=mysql -> error
fair/drain + fairqueue disabled 或 writerTopology!=single -> error
mode 非 legacy|drain|fair -> error
```

- [ ] **Step 2: 增加配置类型与 env**

建议：

```go
type ImagegenBatchCfg struct {
    Mode                       ImagegenBatchMode // legacy|drain|fair
    MaxImagesPerBatch          int
    MaxImagesPerTask           int
    ToolWaitDefault            time.Duration
    ToolWaitMax                time.Duration
    PromptMaxRunes             int
    RequestMaxBytes            int64
    ImageMaxBytes              int64
    BatchMaxBytes              int64
    LocalWorkers               int
    GlobalConcurrency          int
    PerUserBaseConcurrency     int
    PerUserBurstConcurrency    int
    BorrowEnabled              bool
    TaskLease                  time.Duration
    TaskHeartbeat              time.Duration
    ReservationTTL             time.Duration
    ReservationHeartbeat       time.Duration
    PrepareTimeout             time.Duration
    ProvisionalTTL             time.Duration
    ProcessingTurnTTL          time.Duration
    PublishAttemptTimeout      time.Duration
    RecoveryDrainTimeout       time.Duration
    DispatchInterval           time.Duration
    ReconcileInterval          time.Duration
    ExpiredSweepInterval       time.Duration
    ReconcilePageSize          int
    MaxRetries                 int
    ProviderCallTimeout        time.Duration
    ProviderConcurrencyDefault int
    ProviderConcurrency        map[string]int
}
```

Imagegen 调度配置是部署级约束，不允许普通用户/Agent scope 覆盖 global/base/burst；provider chain/credential 继续沿用现有 scope。

- [ ] **Step 3: 定义领域 DTO 和严格解析**

覆盖：

```text
action 缺省 create
prompt/items 二选一
count 默认1
总数 1/4/5/16
17 拒绝且不创建任何记录
items 1..16
label 规范化与去重
prompt rune/JSON byte limit
size preset 规范化
wait 0..240
status/cancel 只接收 batch_id
未知字段是否拒绝按工具现有策略固定
```

- [ ] **Step 4: 实现确定性 planner**

测试：

```text
count=1  -> [1]
count=4  -> [4]
count=5  -> [4,1]
count=9  -> [4,4,1]
count=16 -> [4,4,4,4]
多 item 不跨 prompt 合并
排序固定 item_index/chunk_index
request fingerprint 稳定
```

- [ ] **Step 5: 暴露只读 turn identity/scope**

为 batch tool 提供明确 getter/context struct，至少包含：

```text
tenant/chatter user ID
config/runtime user ID
agent owner user ID
agent ID
workspace project ID
workspace session ID
message channel（仅审计/未来通知）
```

`Registry.ForTurn` 必须正确复制不可变身份并隔离每回合 workspace scope。不能让工具从模型参数获取这些字段。

- [ ] **Step 6: 验证**

```bash
go test ./internal/config -run 'TestImagegenBatch' -v
go test ./internal/imagegen -run 'Test(Model|Normalize|Planner)' -v
go test ./internal/agent/tools -run 'TestRegistryForTurn.*Image' -v
```

- [ ] **Step 7: Commit**

```bash
git add internal/config internal/imagegen/model* internal/imagegen/planner* internal/agent/tools/registry*
git commit -m "feat(imagegen): define batch limits identity and deterministic planning"
```

---

### Task 2: MySQL 两表、batch/task CRUD 与 generation-aware 发布状态

**Files:**

- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Modify: `internal/store/store.go`
- Create: `internal/store/imagegen.go`
- Create: `internal/store/imagegen_repair.go`
- Create: `internal/store/imagegen_test.go`
- Create: `internal/store/imagegen_mysql_test.go`

- [ ] **Step 1: 写 MySQL schema 失败测试**

使用现有 `BKCRAB_TEST_MYSQL_DSN` 门控模式，断言：

```text
image_generation_batches 存在且字段/索引正确
image_generation_tasks 存在且字段/索引正确
UNIQUE(batch_id,item_index,chunk_index)
task sequence_id 为单调 AUTO_INCREMENT UNIQUE，领域 task ID 不承担顺序语义
requested_count 代码校验 1..4
JSON 字段可扫描
时间统一 UTC DATETIME(6)
```

Imagegen 只新增这两张业务表，不增加 artifact/outbox/jobs/tenant 表；复用 RAG 前置已经创建的 `fairqueue_resource_operations` special-recovery safety journal。

- [ ] **Step 2: 事务创建 batch + tasks**

新增概念接口：

```go
CreateImageGenerationBatch(ctx, CreateBatchRequest) (*BatchRecord, []TaskRecord, error)
```

测试：

- batch 与全部 task 同事务提交；
- 任一 task 写失败全部回滚；
- task.user_id 等于 batch.user_id；
- requested_count 等于 task count 总和；
- 初始 batch/task PENDING；
- task dispatch_generation=1、dispatched_at=NULL；
- request/provider plan JSON 不含 secret；
- prompt 只存在 MySQL 领域表，不复制进 dispatch DTO。

- [ ] **Step 3: 读取与授权查询**

实现：

```text
GetImageGenerationBatchForPrincipal(userID,agentID)
ListImageGenerationTasks
GetImageGenerationTask
```

canonical owner 同时匹配 `user_id + agent_id`；同 Agent 可跨 session 查询，另一 Agent 即使 user 相同也统一返回 not found/forbidden，错误不能泄露 batch 是否存在。task 结果按 item/chunk 排序。

- [ ] **Step 4: Durable dispatch source**

实现：

```text
ListDispatchableImageTasksPage(afterSequenceID,limit)
GetDispatchableImageTaskByID(taskID)
MarkImageTaskDispatched(candidateGuard,DispatchToken)
ArmExpiredImageTasks(sequenceCursor,limit)
CaptureImageFairQueueHighWater() -> maxSequenceID
ListCanonicalImageTenants(highWater,afterUserID,limit)
ListDispatchedImageTasks(highWater,afterSequenceID,limit)
ListValidRunningImageTasks(highWater,afterSequenceID,limit)
CaptureImageBrokerRepairHighWater() -> maxSequenceID
ListBrokerBackedImageCandidates(highWater,afterSequenceID,limit)
RearmImageCandidateAfterBrokerLoss(originalGuard) -> new candidate / stale-noop
RepairPoisonImageCandidate(repairLocator,registeredResource,queueTenantHash) -> new candidate / stale-noop / unlocatable
```

严格复用 fairqueue generation/Guard 语义：

- due PENDING 或已 rearm 的 due/expired RUNNING，且 `dispatch_generation>claim_generation`、marker NULL；
- 未 rearm 的 expired RUNNING 仅由常驻 sweeper以 `dispatch_generation=claim_generation` Guard CAS 武装为 `claim_generation+1`；
- Mark CAS 匹配 status/claim/retry/next_run/generation/due；
- retry/canonical/broker repair 都写 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1`、marker NULL；sweeper 的 exact-equality CAS 写 `claim_generation+1`；
- late confirm/old delivery 不能写新 epoch。

所有 recovery/broker page 在 store SQL 层显式限制 `sequence_id<=highWater`，不用 adapter 事后过滤；task page 按 sequence_id keyset，known tenant 按 user_id keyset，均有界且不用 OFFSET。`imagegen_repair.go` 的 broker-loss apply只对仍匹配 original Guard、非终态、batch未取消且仍需执行的row推进generation；poison repair从受限locator加载canonical batch/task，验证registered resource、canonical tenant hash、current generation/due与未取消后用同一GREATEST CAS返回新candidate，不能信任payload tenant或修改owner。real-MySQL测试用 `EXPLAIN ANALYZE`/rows-examined固定dispatch、rearm、known/dispatched/running、broker/poison repair与global/per-user count的索引访问上界。

- [ ] **Step 5: Batch 聚合与终态幂等**

实现 store 内事务 helper：

```text
MarkBatchStarted
FinalizeImageTaskDone
FinalizeImageTaskFailed
FinalizeImageTaskCanceled
Recompute/UpdateBatchAggregate
```

覆盖重复 finalize、并发两个 task 终态、DONE/PARTIAL/FAILED/CANCELED 组合。计数不得因重复消息增加两次。

- [ ] **Step 6: Cancel store 合约**

```text
RequestImageBatchCancel(user,agent,batch)
  -> lock batch
  -> cancel_requested=true
  -> PENDING tasks generation-aware CANCELED
  -> RUNNING 保持，batch CANCELING
  -> 无 RUNNING 时 CANCELED
```

重复 cancel 幂等；所有 batch/task 多行事务固定 batch→task 锁序；终态不回退；stale Rabbit delivery 无法重新 claim 已取消 task。

- [ ] **Step 7: 验证**

```bash
go test ./internal/store -run 'TestImageGeneration' -v
go test ./internal/store -run 'TestImagegen.*MySQL' -v
```

- [ ] **Step 8: Commit**

```bash
git add internal/store
git commit -m "feat(imagegen): persist batches tasks and dispatch epochs in mysql"
```

---

## Phase B：结构化 provider service 与 artifact

### Task 3: 可重建的有效 provider plan 与凭据解析

**Files:**

- Create: `internal/gateway/imagegen_provider_resolver.go`
- Create: `internal/gateway/imagegen_provider_resolver_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/userspace.go`
- Modify: `internal/toolproviders/registry.go`
- Create: `internal/toolproviders/registry_imagegen_test.go`

- [ ] **Step 1: 写配置作用域失败测试**

覆盖：

```text
system -> user -> agent 正常覆盖
foreign/shared Agent owner overlay
shareModelConfig/tool-provider sharing 语义
viewer 显式配置优先级
config_user_id 与 tenant_user_id 不同
Agent owner 与 runtime user 不同
热更新/轮换 key 后 worker 解析当前 secret
batch chain 顺序仍使用提交时 snapshot
```

- [ ] **Step 2: 定义 resolver 接口**

Imagegen 领域只依赖：

```go
type ProviderPlanResolver interface {
    Snapshot(ctx context.Context, identity ExecutionIdentity) (SafeProviderPlan, error)
    Resolve(ctx context.Context, identity ExecutionIdentity, plan SafeProviderPlan) (ResolvedProviderPlan, error)
}
```

`SafeProviderPlan` 可进 MySQL，`ResolvedProviderPlan` 含 secret 且只能在内存短暂存在。

- [ ] **Step 3: 抽取共享作用域逻辑**

不要在后台 worker 复制一份与 `loadUserSpace/EnsureAgent` 不同的覆盖规则。把有效 tool chain 解析提取成可测试 helper，由当前同步注册和异步 resolver 共用。

如果现有 foreign Agent owner overlay 不能通过单一 `config_user_id` 复现，`ExecutionIdentity` 必须同时携带 runtime user、agent owner、agent ID 和 share policy；不得把最终 API key 持久化来规避设计问题。

- [ ] **Step 4: Secret scanner**

序列化 `SafeProviderPlan` 后测试不得出现：

```text
apiKey
Authorization
Bearer
secretKey
access token
resolved endpoint credential query
```

允许 provider/model ref、fallback 开关、schema version、非敏感 capability 选择。

- [ ] **Step 5: 验证**

```bash
go test ./internal/gateway -run 'TestImagegenProviderResolver' -v
go test ./internal/toolproviders -run 'Test.*Resolve' -v
```

- [ ] **Step 6: Commit**

```bash
git add internal/gateway/gateway.go internal/gateway/userspace.go internal/gateway/imagegen_provider_resolver.go internal/gateway/imagegen_provider_resolver_test.go internal/toolproviders/registry.go internal/toolproviders/registry_imagegen_test.go
git commit -m "refactor(tools): resolve durable image provider plans without storing secrets"
```

---

### Task 4: Typed provider adapters、capability 与自动 fallback

**Files:**

- Modify: `internal/toolproviders/imagegen/imagegen.go`
- Modify: `internal/toolproviders/imagegen/openai.go`
- Modify: `internal/toolproviders/imagegen/fal.go`
- Modify: `internal/toolproviders/imagegen/replicate.go`
- Create: `internal/toolproviders/imagegen/imagegen_test.go`
- Create: `internal/toolproviders/imagegen/openai_test.go`
- Create: `internal/toolproviders/imagegen/fal_test.go`
- Create: `internal/toolproviders/imagegen/replicate_test.go`
- Create: `internal/imagegen/service.go`
- Create: `internal/imagegen/service_test.go`

- [ ] **Step 1: 先写 typed adapter 测试**

每个 provider 使用 `httptest.Server`，覆盖：

```text
count/size 正确映射
URL 响应解析为 SourceURL
base64 响应解码为 bytes
返回数量不足
空结果
429 / timeout / 5xx
400 invalid request
401/403 auth config
safety rejection
malformed JSON/MIME
ctx cancel
```

测试中不得访问真实网络。

- [ ] **Step 2: 定义 capability 与 typed backend**

```go
type Backend interface {
    Name() string
    Capability(model string) Capability
    Generate(ctx context.Context, cfg ProviderConfig, req GenerateRequest) (ProviderResult, error)
}
```

provider adapter 不再生成最终 Markdown。过渡期旧 generic `Execute` 可调用 typed backend + legacy renderer，以保持 batch feature disabled 时兼容。

- [ ] **Step 3: 统一错误分类**

实现并测试：

```text
INVALID_REQUEST
SAFETY_REJECTED
AUTH_CONFIG
RATE_LIMITED
UPSTREAM_TRANSIENT
MODEL_UNAVAILABLE
EMPTY_RESULT
INCOMPLETE_RESULT
MALFORMED_RESULT
```

安全拒绝不允许换 provider 绕过；auth/config 可尝试显式 fallback；429/5xx/timeout/empty 可 fallback 并在全部耗尽后 retry。

- [ ] **Step 4: 实现 ImageGenerationService**

固定：

- 只接受 count 1..4；
- 使用提交时 plan 顺序；
- 每 candidate 执行时解析当前 config；
- capability 不支持 size/count 时跳过或在候选内部做有界物理拆分；
- 每次物理调用经 ProviderCallGate（Task 7 注入；当前先 fake/pass-through）；
- 只有准确返回 requested count 才成功；
- attempts summary 有界且不含 secret/响应正文；
- 总 ctx deadline 和每 provider timeout 都可取消。

- [ ] **Step 5: 旧 image_gen 回归**

mode=legacy 时，现有 `image_gen` 仍能通过 typed adapter 返回原 Markdown；`n>4` 的旧行为暂不改变，避免本任务夹带工具协议变更。

- [ ] **Step 6: 验证**

```bash
go test ./internal/toolproviders/imagegen -v
go test ./internal/imagegen -run 'TestGenerationService' -v
```

- [ ] **Step 7: Commit**

```bash
git add internal/toolproviders/imagegen internal/imagegen/service*
git commit -m "refactor(imagegen): return typed images with classified provider fallback"
```

---

### Task 5: 安全下载、workspace 对象、manifest 与 salvage

**Files:**

- Create: `internal/imagegen/artifacts.go`
- Create: `internal/imagegen/artifacts_test.go`
- Create: `internal/imagegen/artifacts_integration_test.go`（LocalFS always；S3由现有test env门控）

- [ ] **Step 1: 写安全边界失败测试**

覆盖：

```text
data bytes + valid image magic
HTTPS URL download
HTTP redirect limit
private/link-local/loopback SSRF reject
trusted custom endpoint exception is explicit
wrong Content-Type + valid/invalid magic
single image byte cap
batch byte cap
dimension decode
path traversal reject
signed URL query 不进入日志/error
```

- [ ] **Step 2: 实现 canonical object key builder**

```text
imagegen/<batch>/<task>/claims/<claimGeneration>/
  image-<index>-<sha256>.<ext>
  manifest.json
```

所有 ID/path segment 必须由服务端 canonical validator 生成。不能使用 prompt、label、provider URL 文件名。

- [ ] **Step 3: 实现 publish 顺序**

```text
validate exact image count
  -> decode/download
  -> validate mime/magic/size/dimensions
  -> Put immutable images
  -> Put manifest last
  -> return ArtifactManifest
```

manifest 带 task/batch/claim generation/request fingerprint/provider/model/artifacts。

- [ ] **Step 4: 实现 salvage**

新 claim 调 provider 前：

- 只检查exact claim返回的有界`PreviousClaimGeneration` manifest；禁止用current generation-1推测；
- 验证 request fingerprint 和 artifact count；
- Stat/Get artifact 并核对大小/hash；
- batch cancel 时不 salvage 为 DONE；
- 校验成功返回 recovered manifest；
- 任何不一致按 miss 处理并告警，不信任 object 内容修改 task identity。

- [ ] **Step 5: 清理边界**

提供 best-effort：

```text
DeleteClaimArtifacts(manifest)
DeleteBatchArtifacts(batch)
List/cleanup stale claim prefixes（有界、可取消）
```

失败 cleanup 不得回滚已经 fenced 完成的业务状态；记录指标并由后续 GC 重试。

- [ ] **Step 6: 验证**

```bash
go test ./internal/imagegen -run 'TestArtifact' -v
go test ./internal/workspace/...
```

- [ ] **Step 7: Commit**

```bash
git add internal/imagegen/artifacts* internal/workspace
git commit -m "feat(imagegen): persist validated image artifacts with recoverable manifests"
```

---

## Phase C：Batch 编排与 provider 子并发

### Task 6: BatchService create/status/cancel、有限等待与聚合 DTO

**Files:**

- Create: `internal/imagegen/batch.go`
- Create: `internal/imagegen/batch_test.go`
- Modify: `internal/imagegen/model.go`

- [ ] **Step 1: 定义依赖接口和 fake 测试**

`BatchService` 依赖窄接口：

```text
BatchStore
DispatchNotifier/TryDispatcher
ProviderPlanResolver
Clock/WaitWakeup
ArtifactURLResolver (optional signed URL)
```

不直接依赖 gateway/agent/tool registry。

- [ ] **Step 2: Create 失败测试**

覆盖：

- 参数失败不触碰 DB/provider；
- snapshot provider plan 失败不创建 batch；
- batch+tasks 原子创建；
- tenant/config/owner/agent/workspace scope 来自 trusted identity；
- fast dispatch 失败仍返回已提交 batch；
- max16 与 planner task 次序；
- create ctx 取消发生在 commit 后不删除 batch；
- plan JSON 不含 secret。

- [ ] **Step 3: Status 失败测试**

覆盖：

```text
same user+agent（含跨 session）-> ordered result
不同 user 或同 user 的不同 agent -> not found/forbidden
PENDING/RUNNING progress
DONE/PARTIAL/FAILED/CANCELED
bounded per-task errors
artifact origin scope
signed URL unsupported fallback
```

- [ ] **Step 4: Cancel 失败测试**

覆盖：

- canonical user+agent owner 校验；
- PENDING 全取消；
- RUNNING -> CANCELING；
- mixed DONE/RUNNING/PENDING；
- 重复 cancel；
- terminal no-op；
- cancel 后 dispatcher 不再选择 task。
- RUNNING worker 在观察 cancel 前崩溃：expired-cancel sweeper 最终聚合 CANCELED，且不产生新 provider call。

- [ ] **Step 5: 有限等待**

实现：

```text
wait=0 immediate
default=180s
max=240s
DB authoritative read
local wake/pubsub only optimization
jittered bounded polling fallback
deadline 返回 current status，不 cancel
```

单测使用 fake clock，不能真实 sleep 180 秒。

- [ ] **Step 6: 验证**

```bash
go test ./internal/imagegen -run 'TestBatchService' -v
```

- [ ] **Step 7: Commit**

```bash
git add internal/imagegen/batch*
git commit -m "feat(imagegen): orchestrate durable image batches and bounded waits"
```

---

### Task 7: Redis provider 级物理调用 limiter

**Files:**

- Create: `internal/imagegen/provider_limiter.go`
- Create: `internal/imagegen/provider_limiter_test.go`
- Modify: `internal/imagegen/service.go`
- Modify: `internal/imagegen/service_test.go`

- [ ] **Step 1: 固定 limiter 合约**

```go
type ProviderCallGate interface {
    Acquire(ctx, provider, model, token string, limit int, ttl time.Duration) (Lease, bool, error)
    Renew(ctx, lease Lease, ttl time.Duration) error
    Release(ctx, lease Lease) error
}
```

物理 provider call 每次单独 acquire/release；fallback 从 A 切到 B 时不能复用 A 的 token。

- [ ] **Step 2: 写 Redis Lua 集成测试**

使用 `BKCRAB_TEST_REDIS_ADDR`：

```text
provider count 不超过配置
不同 provider key 隔离
独立 token expiry
renew 只延长自己
release 幂等
Redis TIME
错误 token 不能释放其它 lease
```

- [ ] **Step 3: Service 集成行为**

```text
permit deny -> 尝试下一个 fallback
全部 deny -> RATE_LIMITED
不在 fair worker 内无限等待
provider call ctx cancel -> release
panic -> release
长调用 renew
```

provider limiter 是可用性/第三方保护，不是 MySQL 业务 fence。

- [ ] **Step 4: 验证**

```bash
go test ./internal/imagegen -run 'TestProviderLimiter' -v
go test ./internal/imagegen -run 'TestGenerationService.*Limit' -v
```

- [ ] **Step 5: Commit**

```bash
git add internal/imagegen/provider_limiter* internal/imagegen/service*
git commit -m "feat(imagegen): limit physical provider calls across fallback"
```

---

## Phase D：MySQL fenced execution 与 fairqueue 接入

### Task 8: Image task exact claim、heartbeat、retry/reclaim 与 cancel fence

**Files:**

- Create: `internal/store/imagegen_claim.go`
- Create: `internal/store/imagegen_claim_test.go`
- Create: `internal/store/fairqueue_mysql_fence.go`
- Create: `internal/store/fairqueue_mysql_fence_test.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/imagegen.go`
- Modify: `internal/store/imagegen_mysql_test.go`
- Modify: `internal/store/rag_task_claim.go`
- Modify: `internal/store/rag_task_claim_test.go`

- [ ] **Step 1: 写 exact claim 失败测试**

覆盖：

```text
只 claim 指定 task
dispatch token generation 必须匹配
expected tenant 必须匹配 canonical task.user_id
batch 不存在/owner mismatch
batch cancel requested -> cancel disposition
PENDING future 不可 claim
PENDING due 可 claim
RUNNING valid lease 不可 claim
RUNNING expired 但 `dispatch_generation=claim_generation` 时旧 duplicate 不可 reclaim；sweeper arm 且 message/row generation 严格更大后才可 reclaim
retry exhausted -> FAILED
重复 delivery 只有一个 claim
claim 要求 message generation == row dispatch_generation > row claim_generation
claim 直接令 claim_generation=dispatch_generation，并写 dispatched_at=COALESCE(dispatched_at,DB_NOW)
generation 从3安全跳到7时 claim=7；返回事务覆盖前真实 PreviousClaimGeneration=3，不能用7-1推测 salvage prefix
claim 返回 execution fence 的 expected writer fingerprint
```

- [ ] **Step 2: 实现 resource advisory lock**

使用与 RAG fairqueue 完全相同的：

```text
lock name = bkcrab:fq: + first48hex(SHA256(databaseName + NUL + resource))
resource = image.generate
```

把RAG prerequisite中的writer/session校验与lock-name/pinned transaction逻辑抽到`fairqueue_mysql_fence.go`，RAG与Imagegen共用而不复制。同一pinned writer `*sql.Conn`上先读取并校验`@@server_uuid,DATABASE(),CONNECTION_ID()`，再执行GET_LOCK、复验同一session identity、事务、RELEASE_LOCK；获取锁前不得开始事务/snapshot。identity mismatch、session affinity不成立或RELEASE_LOCK状态不明时物理discard连接，返回fatal safety error并让runtime关闭gate/取消pipeline，不能只`sql.Conn.Close()`后继续。

本Task还把Task2/6已写的batch create/status/cancel、dispatch list/by-ID/Mark、expired rearm/cancel、poison/broker repair，以及Task9将消费的recovery high-water/每个page与continuous reconciliation全部改走expected-writer pinned read/transaction helper；read方法也在返回DTO前验证identity。real-MySQL测试对每条路径注入transparent writer switch，断言不返回candidate/snapshot、不提交且触发同一fatal gate。

- [ ] **Step 3: 最终容量闸门并发测试**

两个 store 实例、8 个并发 claim：

```text
valid RUNNING global <= 4
same user valid RUNNING <= burst 4
Redis reservation 缺失仍拒绝第5个
capacity full -> capacity-deferred，task 保持可重试状态
advisory lock timeout -> no mutation
```

统计使用 authoritative writer DB_NOW，不走 replica。

- [ ] **Step 4: Heartbeat**

`HeartbeatImageGenerationTask` 必须取得同一 resource lock：

- CAS task ID + claim generation + lease owner + RUNNING；
- 读取 batch cancel_requested；
- 未取消时仅在 `lease_until>DB_NOW` 且 `dispatch_generation=claim_generation` 时延长 lease并保留 dispatched marker；
- 取消时返回 cancel disposition，不再续租；
- claim/heartbeat 对有效 lease 写入存在全序。

- [ ] **Step 5: Finish/retry/cancel fence**

实现：

```text
FinishDone(claim,manifest)
FinishRetry(claim,class,nextRun)
FinishFailed(claim,errorCode)
FinishCanceled(claim)
```

所有 execution-owned mutation 先用 claim 携带的 expected writer fingerprint 在 pinned connection 上复验 writer/session identity，再在同一事务中按固定 batch→task 锁序校验 RUNNING、claim generation、owner、`lease_until>DB_NOW` 与 `dispatch_generation=claim_generation`。Retry 原子 retry+1、`dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1`、marker NULL、PENDING+future next_run；永久失败/取消聚合 batch。salvage finalize 使用 exact claim 返回的 `PreviousClaimGeneration` 验证 manifest，再以当前 execution fence 提交。测试覆盖 cancel-vs-finalize、cancel-vs-heartbeat、writer transparent switch、lease恰好过期、rearm 后迟到 finalize、死锁重试与终态唯一性。

- [ ] **Step 6: Expired sweeper/reclaim**

常驻 sweeper 在 writer-verified resource lock 下分页处理 expired RUNNING。若 batch 未取消，只对 `dispatch_generation=claim_generation` 的 original Guard CAS 写 `dispatch_generation=claim_generation+1, dispatched_at=NULL`，status 保持 RUNNING；已 rearm 的 `dispatch_generation>claim_generation` 不重复推进。若 batch 已 cancel，则不 rearm：以 expired claim/generation/lease fence 转 CANCELED并聚合 batch，覆盖 worker崩溃与已 rearm未 claim 两种情况。exact reclaim 再决定 retry、使用 `PreviousClaimGeneration` salvage 或失败；heartbeat与 sweeper 只有一个能赢。

- [ ] **Step 7: 验证**

```bash
go test ./internal/store -run 'TestImageGeneration.*Claim' -v
go test ./internal/store -run 'TestImageGeneration.*(Heartbeat|Retry|Cancel|Capacity)' -v
go test ./internal/store -run 'Test.*FairQueue.*Writer|TestRAG.*Claim' -v
```

- [ ] **Step 8: Commit**

```bash
git add internal/store/imagegen.go internal/store/imagegen_repair.go internal/store/imagegen_claim.go internal/store/imagegen_claim_test.go internal/store/imagegen_mysql_test.go internal/store/fairqueue_mysql_fence.go internal/store/fairqueue_mysql_fence_test.go internal/store/rag_task_claim.go internal/store/rag_task_claim_test.go internal/store/store.go
git commit -m "feat(imagegen): fence exact claims heartbeats and retries in mysql"
```

---

### Task 9: `image.generate` fairqueue adapter 与 PreparedTask

**Files:**

- Create: `internal/imagegen/fair_queue.go`
- Create: `internal/imagegen/fair_queue_test.go`
- Modify: `internal/imagegen/service.go`
- Modify: `internal/imagegen/artifacts.go`
- Modify: `cmd/bkcrab/cmd_admin_fairqueue.go`
- Modify: `cmd/bkcrab/cmd_admin_fairqueue_test.go`

- [ ] **Step 1: 写 DispatchSource adapter 测试**

覆盖：

```text
resource=image.generate
task_type=image_generation
ResourceConfig 注册 `^imgt_[a-z0-9]{16,64}$` bounded task-ID validator；其它格式/超长值在transport拒绝
message 只有 ids/tenant/DispatchToken
prompt/provider plan/artifact 不进 message
keyset due scan
GetDispatchableByID fast path
generation-aware MarkDispatched
ExpiredRearmSource 映射 writer-fenced store rearm/expired-cancel page
canonical reconciliation tenant
RecoverySource/BrokerRepairSource/WriterRebindSource 都由同一 adapter 映射，fairqueue 不 import imagegen/store
```

- [ ] **Step 2: 写 Prepare disposition 测试**

```text
claimed
capacity-deferred
duplicate/stale
retry-not-due
batch-canceled
permanent-invalid
temporary DB error
body损坏+合法 header -> Message=nil，header locator repair-only
body合法+header缺失/坏类型 -> Message=nil，body locator repair-only
body/header mismatch -> Message=nil；每个独立通过 registered resource、queue tenant、canonical user+agent/batch/task、current generation、due且 batch未取消验证的候选都 generation repair
header-only 无论 adapter 返回什么都不能成为 claimed；terminal/canceled candidate 只返回 no-op disposition，不能 rearm
可定位 poison/mismatch -> canonical repair提交后 confirmed DLQ再 ACK；DLQ失败 requeue
两个 locator 都不可定位 -> confirmed DLQ 后 ACK，且证明不是 canonical v1 publisher output
```

映射遵循通用 fairqueue 契约，不能为 Imagegen 私自 ACK 不可判断的消息。

- [ ] **Step 3: PreparedTask.Run**

执行顺序：

```text
check/salvage exact claim 返回的 PreviousClaimGeneration manifest（不能用 current-1 推测）
  -> resolve provider plan/current credentials
  -> Generate
  -> publish artifacts + manifest
  -> fenced FinishDone + batch aggregate
```

错误：

- retriable -> fenced FinishRetry；
- permanent -> FinishFailed；
- cancel -> FinishCanceled；
- lost MySQL fence -> 丢弃结果、best-effort cleanup，不覆盖新 claim；
- object manifest complete + DB error -> 留待 salvage；
- panic/context cancel -> 分类并 release。

- [ ] **Step 4: Heartbeat 双 lease**

Run 期间：

```text
MySQL task heartbeat
Redis stable fair reservation renew
provider physical lease renew（仅调用期间）
```

任一业务 fence 丢失取消 provider ctx。Redis epoch 变化按通用 recovery identity 刷新；不能拿 provisional token 跑长任务。

- [ ] **Step 5: RecoverySource**

实现通用 paged contract，而不是一次性数组 snapshot：

```text
CaptureHighWater() -> MAX(sequence_id)，在 bounded publish/prepare drain 后调用
ListKnownTenants(highWater,afterUserID,limit)
ListDispatched(highWater,afterSequenceID,limit)
ListValidRunning(highWater,afterSequenceID,limit) -> lease expiry + 同查询 DB_NOW
```

所有 SQL page 在 source 层限定 `sequence_id<=highWater`，各自稳定 keyset cursor且有界。stable token identity 使用 taskID + 实际 claimGeneration；以 `lease_until-DB_NOW` 计算 remaining，再由通用 coordinator 用 Redis TIME 恢复。known-only tenant 不 active；valid-RUNNING 与 READY/recovery stable list/delete 做双向 zero-diff convergence，progress 持久化并由 FinishRecovery复验。Redis recovery 只重建 `image.generate`，RECOVERING 中 rearm/dispatcher/publisher gate关闭，READY 后常驻 loops 才补 due work。

同一adapter还要把store-private `FairQueueOperationRecord/StartSession`与通用`RecoveryOperationRecord/OperationJournal`无损桥接：所有Read显式传expected writer，mutation交回完整expected record/version CAS，`WithStartFence` callback保持同一pinned session及异常物理discard语义。runtime启动/重连先读`image.generate` journal；ACTIVE或无匹配READY control的READY_COMMITTED保持operator-required，匹配last-completed ID才可补记COMPLETED。

- [ ] **Step 6: 注册通用运维 source**

在 admin fairqueue resource registry 显式注册 `image.generate` 的 `BrokerRepairSource`、`WriterRebindSource`、`RecoverySource`和`OperationJournal`，使三个通用命令可用。测试要求unknown resource仍在连接前拒绝；三类apply都复用MySQL start fence→Redis raw lock/preflight→journal CAS→recheck→Begin顺序和operation ID。Rabbit repair排除canceled/terminal，并覆盖ACTIVE+READY/NONE pre-Begin恢复；writer rebind复验schema/generation/valid-RUNNING=0并要求所有image runtime/recovery coordinator保持停止到COMPLETED；force只允许卡住NORMAL/same-ID/missing+未完成journal且只触碰`{image.generate}`keys。READY_COMMITTED+missing需重做Redis通用passes；READY_COMMITTED+READY/last-ID同值只CAS COMPLETED。缺确认、不允许起态、stale expected record、TOCTOU或中断都保持非READY且零越权mutation；raw在journal窗口失效可留下ACTIVE，但必须零Begin、零Redis control/progress mutation、零业务mutation（raw lock维护除外），Redis全丢由同ID journal resume。

- [ ] **Step 7: 验证**

```bash
go test ./internal/imagegen -run 'TestFairQueue' -v
go test ./internal/fairqueue -run 'Test.*Image' -v
go test ./cmd/bkcrab -run 'TestAdminFairQueue.*Image' -v
```

- [ ] **Step 8: Commit**

```bash
git add internal/imagegen/fair_queue.go internal/imagegen/fair_queue_test.go internal/imagegen/service.go internal/imagegen/artifacts.go cmd/bkcrab/cmd_admin_fairqueue.go cmd/bkcrab/cmd_admin_fairqueue_test.go
git commit -m "feat(imagegen): execute image tasks through the fairqueue runtime"
```

---

## Phase E：模型工具、结果交付与网关装配

### Task 10: `image_gen_batch` create/status/cancel 与可信 artifact metadata

**Files:**

- Create: `internal/agent/tools/image_gen_batch.go`
- Create: `internal/agent/tools/image_gen_batch_test.go`
- Modify: `internal/agent/tools/image_gen.go`
- Modify: `internal/agent/tools/result.go`
- Modify: `internal/agent/tools/result_test.go`
- Modify: `internal/agent/loop.go`
- Create: `internal/agent/loop_imagegen_metadata_test.go`

- [ ] **Step 1: 固定 JSON schema/人工校验测试**

覆盖：

```text
create prompt mode
create items mode
status batch_id
cancel batch_id
action default create
prompt/items conflict
count total17 reject
wait max240
unknown/malformed args
```

工具 description 明确“提交后可能返回 batch ID，不要在同一轮高频轮询”。

- [ ] **Step 2: 注册模式**

```text
mode=legacy:
  provider chain 可用 -> 只注册旧 image_gen；none 时隐藏旧工具

mode=fair:
  始终只注册 image_gen_batch，不以 provider/Rabbit/Redis 当前 readiness 隐藏整个工具
  create 在调用时校验 mode、MySQL/schema 与 provider plan；Rabbit/Redis degraded 仍先 durable commit，后台恢复后 dispatch
  status/cancel 只依赖 MySQL + canonical user/agent owner

mode=drain:
  仍只注册 image_gen_batch；create 返回 typed draining error
  status/cancel 与处理既有 batch 的 runtime 保持
```

≤4 张 create 仍创建一个 durable task，不允许走同步 shortcut。

- [ ] **Step 3: Tool handler**

handler：

- 从 turn registry 获取 trusted ExecutionIdentity；
- 调 BatchService Create/Status/Cancel；
- action-aware mode gate 保证 drain 只拒绝 create，provider/runtime degraded 不会让 status/cancel 消失；
- 参数错误返回可理解错误；
- create commit 后 dispatch/wait 错误不能误报“未提交”；
- wait timeout 返回 batch ID 和进度；
- status 返回排序 artifacts/错误摘要；
- 不输出 base64/provider 原始 URL query。

- [ ] **Step 4: Typed metadata validator**

扩展 `ToolResult.Metadata`：

```text
key=imageArtifacts
producer=builtin image_gen_batch only
count<=16
bounded bytes
canonical batch/task/path
image MIME
origin agent/project/session scope
stable ordering
```

插件/MCP/其它 builtin 的同名 metadata 拒绝并安全日志。

- [ ] **Step 5: Agent/gateway-facing delivery accumulation**

Agent loop 应从可信 metadata 收集 artifact refs，并在最终结果交付时保留；不能依赖模型逐字复制 Markdown。覆盖：

- 同一 tool result 多图；
- 多次 status 去重；
- 模型遗漏 Markdown 仍交付；
- 模型伪造 workspace path 不进入可信 metadata；
- origin workspace scope 与当前 status 会话不同；
- Web streaming 不重复渲染正文；
- IM 最终 MediaItem 次序稳定。

- [ ] **Step 6: 验证**

```bash
go test ./internal/agent/tools -run 'TestImageGenBatch' -v
go test ./internal/agent -run 'TestImagegen.*Metadata' -v
```

- [ ] **Step 7: Commit**

```bash
git add internal/agent
git commit -m "feat(tools): expose durable image_gen_batch actions and artifact metadata"
```

---

### Task 11: Gateway 生命周期、provider resolver、健康状态与模式切换

**Files:**

- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/userspace.go`
- Create: `internal/gateway/imagegen_test.go`
- Modify: `internal/setup/handlers_tools.go`
- Modify: `internal/setup/handlers_health.go`
- Modify: `internal/setup/handlers_health_test.go`

- [ ] **Step 1: 写装配失败测试**

覆盖：

```text
mode=legacy 不启动 image resource runtime，只注册旧同步工具（provider chain有效时）
mode=fair/drain 启动 image resource runtime；writer topology非single或schema不兼容 fail closed
mode=fair 在 Rabbit/Redis recovering时仍保留 batch tool：create可durable commit，status/cancel可用
mode=drain 拒绝create但继续 existing batch worker/status/cancel，绝不注册旧 image_gen
启动顺序：store -> resolver -> service -> adapter -> runtime -> tool exposure
关闭顺序停止 create/scheduler 后等待 running
依赖失败不回退旧同步工具
每个 UserSpace/Agent 注册正确 tool
shared Agent identity 传递正确
从 authoritative writer 取得 expected fingerprint；所有 image source/claim/heartbeat/finalize页逐 pinned-connection复验，transparent switch关闭gate并取消pipeline
把image adapter的OperationJournal注入resource runtime；ACTIVE/READY_COMMITTED与Redis last-completed ID不一致时operator-required且调度gate关闭
共享 Redis handshake发现 cluster_enabled=1时拒绝image resource；RAG legacy不妨碍image fair使用全局fairqueue clients
```

- [ ] **Step 2: Gateway 组件装配**

`ImageGenerationService`、BatchService、provider limiter 和 fair adapter 应是可测试依赖，不能在 tool handler 内临时构造 Rabbit/Redis/MySQL clients。

fairqueue Runtime 按 resource 注册 Imagegen source/preparer与OperationJournal adapter；RAG 与 Imagegen 共用连接池/基础 client时必须各自 namespace/配置，关闭只执行一次。共享 `FairQueue.Enabled` 只控制基础设施，RAG/image各自 mode决定resource loop；不能因 RAG=legacy误停 image=fair。

- [ ] **Step 3: Tool catalog/UI**

管理 UI 继续配置 category `image_gen` 的 provider chain；模型工具名称变为 `image_gen_batch`。说明：

- provider 配置分类名不必迁移；
- legacy 中 `none` 隐藏旧工具；fair/drain 中仍保留 batch tool 的 status/cancel，create按 action返回 no-provider/draining；
- batch mode 是部署级三态开关；
- UI 不把 16 张上限误当 provider 单次 n。

- [ ] **Step 4: 健康状态**

暴露：

```text
legacy / drain / fair
ready
rabbit degraded
redis paused/recovering
mysql unavailable
mysql writer fingerprint/session mismatch (fatal gate closed)
mysql operation journal phase/kind/operation-ID短指纹
redis operation kind/current与last-completed operation-ID短指纹
operator-required（ACTIVE、无匹配READY control的READY_COMMITTED或未完成journal/control ID/kind/writer不一致；COMPLETED不阻止NORMAL rebuild）
unsupported redis cluster mode
object store unavailable
no provider plan
unsupported localfs multi-instance
running/pending batch counts（有界）
```

raw operation ID、secret/DSN/endpoint credential 不进入 health detail。

probe 语义与共享 fairqueue 一致：`/livez` 只表示进程存活；`/readyz` 表示 API/MySQL schema 可服务 durable create/status/cancel，不因 image Redis RECOVERING、Rabbit或provider暂时不可用而摘掉整个 API Pod；image scheduler/create的细粒度 gate在受保护 health detail与action错误中体现。MySQL整体不可用或schema不兼容可使readiness失败，单个resource recovering不可以。

- [ ] **Step 5: 旧工具切换闸门**

所有实例先以legacy部署兼容版本并确认更旧binary归零。正向必须分两次全量rollout：legacy→drain，确认legacy Pod与旧同步调用归零；再drain→fair。禁止直接rolling legacy→fair。fair/drain中依赖故障保持batch tool已注册，禁止回退旧工具。回滚全量fair→drain，待无非终态batch后再drain→legacy；任一阶段平台不能证明旧ReplicaSet归零就保持drain。

- [ ] **Step 6: 验证**

```bash
go test ./internal/gateway -run 'TestImagegenBatch' -v
go test ./internal/setup -run 'Test.*(Tools|Health).*Image' -v
go build ./...
```

- [ ] **Step 7: Commit**

```bash
git add internal/gateway internal/setup
git commit -m "feat(gateway): wire image batch scheduling and health gates"
```

---

## Phase F：部署、集成验收与发布

### Task 12: Docker/Helm/Kubernetes 配置与共享 workspace 约束

**Files:**

- Modify: `deploy/docker/docker-compose.yml`
- Modify: `deploy/docker/.env.example`
- Modify: `deploy/docker/README.md`
- Modify: `deploy/multi-pod/docker-compose.yaml`
- Modify: `deploy/multi-pod/README.md`
- Modify: `deploy/helm/bkcrab/values.yaml`
- Modify: `deploy/helm/bkcrab/templates/configmap.yaml`
- Modify: `deploy/helm/bkcrab/templates/gateway.yaml`
- Modify: `deploy/k8s/bkcrab.yaml`
- Create: `internal/setup/imagegen_deployment_test.go`

- [ ] **Step 1: 接入全部 Imagegen env**

默认 `BKCRAB_IMAGEGEN_BATCH_MODE=legacy`。fair/drain profile显式开启共享 fairqueue、Redis standalone 与 MySQL writer topology=single；secret仍由现有 tool provider config管理，不把provider key放进ConfigMap或示例明文。

- [ ] **Step 2: 单实例 profile**

允许：

```text
MySQL + Rabbit + Redis
workspace LocalFS
replicas=1
```

文档明确 LocalFS 只支持单实例 image worker。

- [ ] **Step 3: 多实例 profile**

要求：

```text
shared authoritative MySQL writer；所有 Pod 观察相同 server_uuid+database fingerprint且endpoint有session affinity
shared Rabbit/Redis
workspace type=S3/compatible
same object prefix/bucket credentials
same fairqueue/image config
```

worker 不需要本地共享磁盘。

- [ ] **Step 4: Helm/K8s 校验**

- image batch mode=legacy|drain|fair和值可覆盖；
- Secret/ConfigMap 分离；
- image resource scheduler/dispatcher/inline-publish路径在recovery时fail closed；fair模式create仍先MySQL durable commit、仅延后dispatch，API`/readyz`保留create/status/cancel语义；
- termination grace 足够 cancel/stop heartbeat；
- 多 replica 示例默认共享 object store；
- 不输出 provider secret。
- 模板/manifest拒绝Redis Cluster并固定writerTopology=single；正向切换必须两个独立全量rollout `legacy→drain→fair`并逐阶段证明旧ReplicaSet归零，回滚`fair→drain→legacy`。

- [ ] **Step 5: 验证**

```bash
docker compose -f deploy/docker/docker-compose.yml config
docker compose -f deploy/multi-pod/docker-compose.yaml config
helm template bkcrab deploy/helm/bkcrab
go test ./internal/setup -run 'TestImagegen.*(Deployment|Rollout|Probe)' -v
```

- [ ] **Step 6: Commit**

```bash
git add deploy internal/setup/imagegen_deployment_test.go
git commit -m "deploy(imagegen): configure durable fair image batches"
```

---

### Task 13: 端到端公平、fallback、artifact、取消与故障测试

**Files:**

- Create: `internal/imagegen/integration_test.go`
- Create: `internal/imagegen/fair_queue_integration_test.go`
- Create: `internal/imagegen/integration_harness_test.go`
- Modify: `internal/setup/imagegen_deployment_test.go`

- [ ] **Step 1: env-gated harness**

```text
BKCRAB_TEST_MYSQL_DSN
BKCRAB_TEST_REDIS_ADDR
BKCRAB_TEST_RABBITMQ_URL
BKCRAB_TEST_OBJECT_STORE_* 或 isolated LocalFS temp root
```

每个测试使用唯一 Rabbit exchange/key prefix/workspace scope，并只清理自己的 namespace。

- [ ] **Step 2: Planner/tool 场景**

```text
single prompt count=16 -> 4 tasks
items A5/B3/C1 -> A4,A1,B3,C1，共 4 tasks
count=17 -> no batch rows
wait=0 immediate
wait deadline -> batch continues
status returns ordered artifacts
same user+agent cross-session status succeeds；same user different agent is denied
```

- [ ] **Step 3: 公平与借用**

可控 blocking fake provider：

```text
A count16, no competitor -> A runs4
A already4, B count1 -> release one A, B starts next
A/B backlog -> converge 2/2
A/B/C no starvation
two runtimes total valid RUNNING<=4
same user<=4
delete Redis reservations -> MySQL still blocks fifth
```

- [ ] **Step 4: Provider/fallback**

```text
primary success
primary 429 -> fallback success
primary auth -> explicit fallback success
safety reject -> no cross-provider bypass
all transient -> delayed retry
incomplete count -> no partial task commit
provider physical concurrency limit
```

- [ ] **Step 5: Artifact/crash**

```text
base64/URL -> persisted workspace object
manifest written last
kill after image objects before manifest -> retry
kill after manifest before DB finalize -> salvage, provider called once
generation 3→7跳号 reclaim 使用 PreviousClaimGeneration=3 salvage，不猜 current-1
stale worker cannot overwrite new claim path/finalize
wrong MIME/oversize/SSRF reject
cross-Pod status reads shared object
```

- [ ] **Step 6: Partial/cancel**

```text
3 tasks DONE + 1 FAILED -> batch PARTIAL
all failed -> FAILED
cancel pending -> no provider call
cancel running -> provider ctx canceled/fence rejects DONE
cancel running后worker在观察前崩溃 -> expired-cancel sweeper最终CANCELED、无新provider call
cancel after partial success -> CANCELED with completed artifacts retained
repeat cancel idempotent
```

- [ ] **Step 7: 基础设施故障**

```text
Rabbit down: create succeeds, recovery dispatches
Redis down: no new start, no legacy fallback
Redis flush: rebuild active/stable reservations
MySQL down before create: no Rabbit orphan
MySQL down after manifest: salvage later
object store down: no DONE without artifact
duplicate/stale/poison Rabbit delivery
body损坏/header-only、body-only、双locator mismatch：原delivery永不执行；所有独立合法candidate repair，confirmed DLQ后ACK
Rabbit durable数据清空：通用CLI dry-run零mutation；带双确认apply在 RECOVERING 中仅按 repair high-water/original Guard rearm，零 publish且排除 canceled/terminal；Finish 置 READY 后由普通 dispatcher 重发
Rabbit repair半页后同时Redis flush：MySQL ACTIVE journal阻止NORMAL；同kind/同ID命令rehydrate并完成，未扫描task不丢
special READY_COMMITTED跨Redis Finish崩溃：前侧operator-required，后侧只在last-completed ID匹配时补记COMPLETED
Rabbit ACTIVE journal提交后、Begin前崩溃：同参数按原ID完成Begin，不同参数拒绝；writer terminal-reconcile由CLI在runtime保持停止时只Complete
数万task小page recovery：sequence high-water在持续create下有界；owner中页失锁takeover；provisional/processing drain；stable orphan/missing双向zero-diff
Redis tenant-only orphan key在普通reset中被bounded owned-key scan删除；Cluster handshake拒绝
两个runtime writer fingerprint不同及transparent writer switch：publish/source/claim/finalize前fatal fail closed
mode正向legacy→drain→fair与回滚fair→drain→legacy：逐阶段旧ReplicaSet归零；drain拒绝create但status/cancel/既有worker可用，绝无legacy/fair混跑
```

- [ ] **Step 8: 安全扫描**

在 Rabbit payload、Redis values、MySQL provider plan、tool result、logs fixture 中搜索：

```text
prompt sentinel
api key sentinel
Authorization
data:image
base64 sentinel
signed URL secret query
```

只允许 prompt sentinel 出现在预期 MySQL task prompt/request 列，不得出现在其它系统。

- [ ] **Step 9: 验证**

```bash
go test ./internal/imagegen -run 'TestIntegration' -v
go test ./internal/imagegen -run 'TestFairQueueIntegration' -v
go test ./internal/setup -run 'TestImagegen.*(Deployment|Rollout|Probe)' -v
go test ./cmd/bkcrab -run 'TestAdminFairQueue.*Image' -v
```

- [ ] **Step 10: Commit**

```bash
git add internal/imagegen/integration_test.go internal/imagegen/fair_queue_integration_test.go internal/imagegen/integration_harness_test.go internal/setup/imagegen_deployment_test.go
git commit -m "test(imagegen): cover fair batches fallback artifacts and recovery"
```

---

### Task 14: 数据库文档、全量回归、灰度与自审

**Files:**

- Modify: `docs/database.md`
- Create: `docs/imagegen-batch-operations.md`
- Modify: `docs/rag-fair-queue-operations.md`（在 registered resource 表增加 image.generate）
- Modify: this plan checkboxes as work completes

- [ ] **Step 1: 文档**

记录：

- 两张表字段/状态；
- `dispatch_generation/dispatched_at` 语义；
- object layout/manifest；
- provider plan 不含 secret；
- tool create/status/cancel；
- LocalFS 单实例/S3 多实例；
- cancel、PARTIAL、retry、salvage；
- legacy/fair/drain/rollback runbook；三条通用admin命令的 image.generate dry-run/apply前置与故障恢复。

- [ ] **Step 2: 局部回归**

```bash
go test ./internal/toolproviders/imagegen -v
go test ./internal/imagegen/... -v
go test ./internal/store -run 'TestImageGeneration' -v
go test ./internal/agent/tools -run 'TestImageGen' -v
go test ./internal/agent -run 'TestImagegen' -v
go test ./internal/gateway -run 'TestImagegen' -v
go test ./internal/setup -run 'Test.*Image' -v
```

- [ ] **Step 3: 全量验证**

```bash
go test ./...
go vet ./...
go build ./...
```

若仓库已有 lint/前端测试命令，按 CI 配置补跑；不得猜测并写入不存在的命令。

- [ ] **Step 4: 灰度证据**

保存：

```text
max batch16/task4
A 独占4
B 到来后的下一槽
2/2 收敛
provider fallback
manifest salvage provider-call-once
PARTIAL
cancel fence
Rabbit/Redis/object recovery
no base64/secret leakage
```

- [ ] **Step 5: 发布顺序**

```text
1. fairqueue prerequisite complete
2. schema expand
3. compatible code deployed everywhere, mode=legacy；更旧binary=0
4. 全量legacy→drain，确认legacy Pod和旧同步调用=0
5. 全量drain→fair；只在应用/流量层对测试用户灰度，不能做legacy/fair Pod canary
6. observe then widen application traffic
7. multi-instance only after shared object-store validation
```

- [ ] **Step 6: 最终自审**

- [ ] Imagegen 只新增两张业务表，并复用前置通用 special-recovery safety journal。
- [ ] 没有新增 artifact/outbox/jobs/tenant 业务表。
- [ ] image bytes/base64 不进 MySQL/Rabbit/Redis/tool text/log。
- [ ] Rabbit 不含 prompt/provider plan。
- [ ] count>16 明确拒绝，不截断。
- [ ] ≤4 也走 durable fair task。
- [ ] user/config user/agent owner 身份没有混用。
- [ ] provider plan 可重建且不含 secret。
- [ ] exact claim 与 heartbeat 共用 resource lock。
- [ ] claim/heartbeat/finalize/source/recovery每页都逐pinned-connection验证writer identity；mismatch fatal fail closed。
- [ ] claim令claim_generation=dispatch_generation并保留marker；retry/rearm/repair遵守GREATEST invariant，salvage使用PreviousClaimGeneration。
- [ ] global=4、base=2、burst=4、borrow=true 有验收。
- [ ] provider 物理调用有独立 limiter。
- [ ] manifest salvage 避免主要崩溃窗口重复付费。
- [ ] cancel/stale worker 不能越过 fence 提交。
- [ ] PARTIAL 保留成功 artifact。
- [ ] batch mode 不暴露旧同步 `image_gen`。
- [ ] 多实例要求共享 workspace。
- [ ] status/cancel 校验 canonical owner。
- [ ] status/cancel校验user+agent，允许同Agent跨session且拒绝跨Agent。
- [ ] worker崩溃后的cancel由expired-cancel sweeper收敛，不永久CANCELING。
- [ ] RecoverySource使用sequence_id high-water/keyset/DB_NOW，stable token双向zero-diff后才READY。
- [ ] image.generate已注册rabbit-disaster-repair/rebind-writer/redis-force-rebuild。
- [ ] image runtime注入OperationJournal；Redis全丢不抹掉special intent，三阶段Finish按operation ID对账。
- [ ] rebind期间所有image runtime/recovery coordinator保持停止到journal COMPLETED；force仅允许卡住NORMAL/same-ID/missing+未完成journal起态。
- [ ] fair→drain→legacy期间batch管理入口不断且旧同步工具不混跑。
- [ ] 正向也严格legacy→drain→fair两阶段，每阶段旧ReplicaSet归零后才继续。

- [ ] **Step 7: Commit**

```bash
git add docs/database.md docs/imagegen-batch-operations.md docs/rag-fair-queue-operations.md docs/superpowers/plans/2026-08-02-imagegen-batch-fair-queue.md docs/superpowers/specs/2026-08-02-imagegen-batch-fair-queue-design.md
# 禁止 `git add docs`，避免把其它 untracked 设计稿带入提交
git commit -m "docs(imagegen): document fair batch operations and rollout"
```

---

## 实施顺序与并行边界

```text
Fairqueue prerequisite
        │
Task 1 -> Task 2
   │        │
   ├-> Task 3 -> Task 4 -> Task 5
   │                       │
   └-----------> Task 6 <--┘
                    │
Task 7 ------------>│
Task 8 ------------>Task 9
                    │
                 Task 10
                    │
                 Task 11 -> Task 12
                    │
                 Task 13 -> Task 14
```

可并行：

- Task 2 MySQL schema/store 可与 Task 3 resolver 并行，但 DTO/identity 先由 Task 1 固定。
- Task 4 typed providers 可与 Task 2 并行。
- Task 5 artifact publisher 可在 Task 4 typed result 定稿后独立进行。
- Task 7 provider limiter 可与 Task 8 MySQL exact claim 并行。
- Task 12 部署清单可在 Task 1 配置名稳定后开始，必须等 Task 11 health 语义收尾。

不可提前：

- Task 6 不应在 store/plan/resolver DTO 未定时临时拼接 map。
- Task 9 不应在通用 fairqueue prerequisite 未完成时复制一套 scheduler。
- Task 10 不应在 durable create/status/cancel 可用前暴露工具。
- Task 11 不应通过依赖故障自动回退旧同步工具。
- Task 13 必须覆盖真实 MySQL/Rabbit/Redis/object store 的跨组件窗口。
