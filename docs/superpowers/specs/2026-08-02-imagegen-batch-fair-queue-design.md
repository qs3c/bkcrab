# Imagegen Batch 与多用户公平调度设计

日期：2026-08-02
状态：已实施（2026-08-04）
前置：

- [RAG 多用户公平调度队列设计](2026-08-01-rag-fair-queue-design.md)
- 参考实现：`E:\fromGithub\fair-scheduling-queue`
- 当前工具入口：`internal/agent/tools/image_gen.go`
- 当前 provider 实现：`internal/toolproviders/imagegen`

> 本设计把当前同步、单次最多 4 张的 `image_gen` 改造成持久化的 `image_gen_batch`。工具可以一次请求最多 16 张图片，服务端按同 prompt、单任务最多 4 张拆分，通过通用 `internal/fairqueue` 按用户公平调度。图片字节写入现有 workspace/object store，MySQL 保存 batch/task 业务状态，RabbitMQ 与 Redis 只承担可重建的传输和调度职责。

## 1. 背景与当前实现

当前链路是同步工具调用：

```text
模型调用 image_gen(prompt,size,n)
  -> tools.RegisterImageGenChain
  -> toolproviders.Chain.Execute
  -> OpenAI / Fal / Replicate
  -> provider 返回 Markdown
       ├─ 远程 URL
       └─ data:image/...;base64,...
  -> Markdown 作为 tool result 回到模型
```

现状具备基础 provider chain 和自动 fallback，但有以下限制：

1. `n` 在 provider 公共参数解析中被静默截断到 4，模型不能表达真正的批量请求。
2. 一个工具调用直接占用 Agent turn；provider 单次内部超时 120 秒，IM 外层任务默认 300 秒。
3. provider 返回的是拼接好的 Markdown，不是结构化 artifact；领域层无法可靠判断图片数量、MIME、尺寸和对象键。
4. OpenAI base64 会进入 tool result 和模型上下文；Fal/Replicate URL 可能是临时链接，未持久化就可能失效。
5. 没有 durable batch/task、claim、lease、heartbeat、retry、cancel 和崩溃恢复。
6. 多用户并发调用时，每个请求直接打到 provider，没有用户公平、集群全局并发和 provider 子并发控制。
7. provider chain 配置是 Agent/UserSpace 解析后的内存快照；异步 worker 重启后不能依赖提交进程内的闭包和 API key。

因此 batch 改造不能只是循环调用旧工具。需要把当前 provider 调用下沉为结构化 `ImageGenerationService.Generate()`，并在其上增加持久 batch、任务拆分、公平队列、对象持久化和可恢复状态机。

## 2. 已确认的目标与边界

### 2.1 目标

1. 对模型只暴露 `image_gen_batch`；启用 batch 模式后隐藏旧 `image_gen`。
2. 单 batch 总图片数为 1～16，超过上限直接拒绝，不允许静默截断。
3. create 同时支持“单 prompt + count”和“items 多 prompt”两种输入。
4. 同一个逻辑 generation task 只处理一个 prompt，最多生成 4 张图片。
5. `image.generate` 使用独立于 `rag.index` 的容量：默认 global=4、base=2、burst=4、允许借用。
6. tenant 是实际发起调用的用户；共享 Agent 场景下，公平用户与 provider 凭据来源可以不同。
7. MySQL 是 batch/task 真相源；RabbitMQ 至少一次投递；Redis 是可重建公平协调层。
8. 工具采用“先持久化、有限同步等待”：默认等待 180 秒，最大 240 秒，未完成时返回 batch ID。
9. 同一个工具支持 create/status/cancel；不要求模型在同一轮高频轮询。
10. 图片必须写入 workspace/object store 后才可成为成功 artifact；不把 provider 临时 URL 当最终结果。
11. provider/model fallback 保留并增强，API key、token、完整图片和 prompt 不进入 Rabbit/Redis/普通日志。
12. 只增加两张 Imagegen 业务表；artifact 元数据以内联 JSON 保存于 task，图片字节不进 MySQL。
13. 一期只实现 MySQL；公平模式不为 SQLite/PostgreSQL 提供等价 claim/恢复合约。

### 2.2 明确不做

- 不实现图片编辑、局部重绘、参考图、ControlNet 或图生图；一期只有 text-to-image。
- 不实现超过 16 张的超大离线作业。
- 不实现用户权重、VIP 优先级、抢占或按图片像素加权的 WFQ。
- 不在一期实现主动完成通知；超出同步等待后通过同一工具的 status 查询。
- 不把图片 base64、远程响应原文或 API key 存入 MySQL、RabbitMQ、Redis、session message 或结构化日志。
- 不新增 `image_generation_artifacts` 表；若未来需要逐图审批、分享、搜索或独立生命周期，再单独评审。
- 不复用 RAG 的 `rag_index_tasks`；Imagegen 使用自己的 batch/task 表。
- 不依赖 RabbitMQ delayed-message 插件；延迟重试由 MySQL `next_run_at` 驱动。
- 不让 provider limiter 替代用户公平队列；两者分别控制租户公平和第三方并发。
- 不在 batch 开启时保留一条绕过 fairqueue 的同步 `image_gen` 路径。
- 不实现精确费用结算；一期记录 provider/model、调用次数、成功图片数和字节数指标。

### 2.3 关键决策

| 决策点 | 结论 |
|---|---|
| 工具 | 单一 `image_gen_batch`，`action=create/status/cancel` |
| 总上限 | 每个 batch 1～16 张，服务端严格拒绝越界 |
| 拆分单元 | 同 prompt 的一个逻辑 `Generate` task，`requested_count<=4` |
| Tenant | 实际 chatter/user ID，写入 batch/task `user_id` |
| 凭据解析 | 保存 `config_user_id + agent_owner_user_id + agent_id` 和无密钥 chain snapshot，执行时重新解析当前 secret |
| 公平资源 | `image.generate`，不占用 `rag.index` 槽位 |
| 默认容量 | global=4、per-user base=2、burst=4、borrow=true |
| provider 子限流 | 每次物理 provider HTTP 调用单独取得 provider lease |
| 持久状态 | MySQL 两张业务表 |
| 图片存储 | 现有 `workspace.Store`；多实例必须使用共享后端 |
| Rabbit | 只传 task reference、tenant、task type、稳定 DispatchToken |
| Redis | 复用 fairqueue resource epoch、ring、processing turn 和 reservation lease |
| ACK | MySQL exact claim 提交且 Redis provisional promote 成功后 ACK |
| fallback | `ImageGenerationService` 内按冻结 chain 顺序、当前凭据执行 |
| 同步等待 | 默认 180 秒、最大 240 秒；等待超时不取消 batch |
| 部分成功 | batch 可终态 `PARTIAL`，已成功 task/artifact 不回滚 |
| 取消 | best effort；待执行任务立即取消，运行任务通过 heartbeat/fence 观察取消 |
| 交付 | 模型文本使用 workspace 引用；可信 typed metadata 携带结构化 artifact refs |

## 3. 术语与身份

### 3.1 Batch、Item、Task、Artifact

- **Batch**：一次 `action=create` 产生的顶层请求，总图片数不超过 16。
- **Item**：一个 prompt 及其 count、size、label；单 prompt 模式等价于只有一个 item。
- **Task**：公平队列调度单元，只包含一个 item 的一个 chunk，count 为 1～4。
- **Artifact**：已经持久化到 workspace/object store 的一张图片及其结构化元数据。

示例：

```text
items:
  cover: prompt=A, count=5
  avatar: prompt=B, count=3

tasks:
  task 0: item=cover, chunk=0, count=4
  task 1: item=cover, chunk=1, count=1
  task 2: item=avatar, chunk=0, count=3
```

不同 prompt 不得为了凑满 4 张而合并到同一个 task。

### 3.2 三种用户/Agent 身份

共享 Agent 和 IM 多发送者场景下必须区分：

| 字段 | 含义 |
|---|---|
| `user_id` | 实际发起请求的用户；用于公平、访问控制、状态查询 |
| `config_user_id` | 当前 UserSpace/配置解析主体；用于重建 tool provider 配置 |
| `agent_owner_user_id` | Agent 所有者；用于复现 owner overlay/share 配置语义 |
| `agent_id` | 执行该工具的 Agent；用于配置和 workspace 路由 |

普通自有 Agent 场景这几个用户 ID 通常相同，但 schema 和接口不能据此合并。API key 永远通过上述引用在执行时解析，不复制到 batch/task。

### 3.3 逻辑任务与物理 provider call

一个公平 task 是一次逻辑 `Generate(prompt,count<=4)`。通常对应一次 provider HTTP 请求；若 fallback provider 的单次能力小于 requested count，service 可以在同一个逻辑 task 内做至多 4 次物理调用以补齐，但：

- 每次物理调用都必须单独取得 provider 子并发 lease；
- task 仍只占一个 `image.generate` 公平 reservation；
- service 只有拿到完整 requested count 才返回成功；
- 任一 candidate 最多产生 4 张，不能在 service 内扩展成无界循环。

这保留了现有“单次工具调用最多 4 张”的逻辑边界，同时允许不同 provider 能力降级。

## 4. `image_gen_batch` 工具协议

### 4.1 Create：单 prompt 模式

```json
{
  "action": "create",
  "prompt": "一只在雨夜霓虹街道上的橘猫",
  "count": 12,
  "size": "square",
  "wait_seconds": 180
}
```

### 4.2 Create：items 模式

```json
{
  "action": "create",
  "items": [
    {
      "label": "cover",
      "prompt": "科幻小说封面，冷色调",
      "count": 2,
      "size": "portrait"
    },
    {
      "label": "avatar",
      "prompt": "圆形构图的机器人头像",
      "count": 4,
      "size": "square"
    }
  ],
  "wait_seconds": 180
}
```

### 4.3 Status 与 Cancel

```json
{"action":"status","batch_id":"imgb_xxx"}
```

```json
{"action":"cancel","batch_id":"imgb_xxx"}
```

### 4.4 校验规则

- `action` 缺省为 `create`，只允许 `create/status/cancel`。
- create 时 `prompt` 与 `items` 必须且只能出现一个。
- 单 prompt 的 `count` 默认 1；item 的 `count` 默认 1。
- 所有 count 求和必须为 1～16；任何越界都返回参数错误，不创建 batch。
- items 数量为 1～16；label 可选、batch 内规范化后必须唯一。
- prompt trim 后不能为空；每个 prompt 默认最多 8,000 个 Unicode code point。
- 规范化后的请求 JSON 默认不得超过 128 KiB。
- `wait_seconds` 取值 0～240；缺省 180。0 表示只提交不等待。
- `size` 由应用定义为 provider-neutral preset；一期至少支持 `square/landscape/portrait`，provider adapter 映射为具体尺寸/比例。
- status/cancel 必须校验 batch `user_id`；不得仅凭 batch ID 访问。
- cancel 是幂等操作；对终态 batch 返回当前终态，不报错。

### 4.5 Create 返回语义

create 必须先在一个 MySQL 事务中写 batch 和全部 task，再尝试 fast dispatch。之后：

1. `wait_seconds=0`：立即返回 batch ID 和 PENDING 状态。
2. 在等待窗口内进入终态：返回完整状态、成功 artifact 和失败摘要。
3. 等待窗口结束仍未终态：返回 batch ID、当前进度和 status 调用示例。
4. 工具 ctx 被取消：停止等待但不取消持久 batch；只要响应通道尚可写，返回“已提交、仍在执行”。

工具结果不得返回 base64。模型可见文本使用短摘要和 canonical workspace Markdown；同时通过可信 `ToolResult.Metadata` 传递结构化 artifact refs，避免模型遗漏、篡改或重复图片引用。

## 5. 总体架构

```text
模型 / 用户
    │ image_gen_batch(create)
    ▼
┌────────────────────────────────────────────┐
│ Batch Service                              │
│ - 校验总数<=16                             │
│ - 解析 tenant/config/agent/workspace scope │
│ - 冻结无密钥 provider chain plan           │
│ - 事务创建 batch + tasks                   │
└───────────────────┬────────────────────────┘
                    │ fast path + durable scan
                    ▼
┌────────────────────────────────────────────┐
│ internal/fairqueue                         │
│ resource=image.generate                    │
│ Rabbit durable tenant queue                │
│ Redis RR + processing turn + reservations  │
└───────────────────┬────────────────────────┘
                    │ exact task delivery
                    ▼
┌────────────────────────────────────────────┐
│ Imagegen Fair Adapter                      │
│ - MySQL exact claim/lease/fence             │
│ - global=4 / user burst=4 final guard       │
│ - claim 后 promote + ACK                    │
└───────────────────┬────────────────────────┘
                    ▼
┌────────────────────────────────────────────┐
│ ImageGenerationService.Generate            │
│ - resolve current credentials              │
│ - provider capability + sub-limit          │
│ - fallback/error classification            │
│ - structured bytes/result                  │
└───────────────────┬────────────────────────┘
                    ▼
┌────────────────────────────────────────────┐
│ Artifact Publisher                         │
│ - decode/download/validate                 │
│ - immutable objects + manifest last        │
│ - fenced MySQL task completion             │
│ - aggregate batch counters/status          │
└───────────────────┬────────────────────────┘
                    ▼
       image_gen_batch wait/status/cancel
```

### 5.1 包边界

建议领域边界：

```text
internal/imagegen
  - batch/model/planner/service
  - artifact publisher
  - fairqueue adapter
  - provider limiter

internal/toolproviders/imagegen
  - OpenAI/Fal/Replicate typed provider adapters
  - capability/error conversion

internal/store
  - image batch/task MySQL records
  - dispatch/claim/heartbeat/finalize/cancel/recovery

internal/agent/tools
  - image_gen_batch model-facing adapter

internal/fairqueue
  - remains domain-neutral
```

`internal/fairqueue` 不得 import `internal/imagegen` 或 `internal/store`。Imagegen adapter 实现通用 `DispatchSource`、`ExpiredRearmSource`、`TaskPreparer/PreparedTask`、`RecoverySource`、`BrokerRepairSource` 与 `WriterRebindSource`；所有接口沿用 RAG 计划中已经固定的领域无关 DTO，不能另建 Imagegen 特例或内存全量 snapshot。

## 6. ImageGenerationService

### 6.1 结构化接口

概念接口：

```go
type GenerateRequest struct {
    Prompt string
    Size   string
    Count  int // 1..4
}

type GeneratedImage struct {
    Bytes      []byte
    SourceURL  string
    MIMEType   string
    Width      int
    Height     int
}

type GenerateResult struct {
    Images   []GeneratedImage
    Provider string
    Model    string
    Attempts []AttemptSummary
}

type ImageGenerationService interface {
    Generate(ctx context.Context, plan ProviderPlan, req GenerateRequest) (GenerateResult, error)
}
```

`GeneratedImage` 在 provider adapter 与 artifact publisher 之间短暂存在。完成对象写入后应尽快释放 Bytes/SourceURL，不得把它序列化进业务表。

### 6.2 ProviderPlan

batch 创建时冻结：

- 有序 `provider/model` refs；
- auto fallback 开关；
- provider-neutral size/request 参数版本；
- plan schema version；
- 配置解析主体和 Agent 所有者引用。

不冻结：

- API key/token；
- secret header；
- 当前 endpoint secret；
- provider 临时 URL；
- 内存 provider 实例。

worker 每次执行都用冻结的 refs 和身份引用重新解析当前配置。用户在 batch 执行期间改变链顺序不会改变该 batch；轮换 API key 会被后续 attempt 使用。

### 6.3 Provider capability

每个 provider/model adapter 必须声明至少：

```go
type Capability struct {
    MaxImagesPerCall int
    SupportedSizes   []string
    SupportsSeed     bool
}
```

一期工具不暴露 seed，但 capability 仍用于校验和未来扩展。adapter 不得假设所有 provider 都支持 4 张或相同 `size` 语义。

### 6.4 错误分类与 fallback

统一分类：

| 类别 | 示例 | 行为 |
|---|---|---|
| INVALID_REQUEST | prompt/size/count 无效 | task 永久失败，不 fallback |
| SAFETY_REJECTED | provider 内容安全拒绝 | task 永久失败，不跨 provider 绕过安全 |
| AUTH_CONFIG | key 缺失、401/403、模型无权限 | 记录配置错误，允许尝试显式 fallback |
| RATE_LIMITED | 429、provider concurrency | fallback；全部耗尽后延迟重试 |
| UPSTREAM_TRANSIENT | timeout、网络、5xx | fallback；全部耗尽后延迟重试 |
| MODEL_UNAVAILABLE | 模型不存在/临时不可用 | fallback；无候选时按可重试性决定 |
| EMPTY_RESULT | 成功响应但无图片 | fallback；全部耗尽后延迟重试 |
| INCOMPLETE_RESULT | 图片数少于 requested count | 本次 candidate 失败，不提交部分对象 |
| MALFORMED_RESULT | 非图片、超限、解码失败 | fallback 或永久失败，按来源分类 |

service 只在返回 `len(images)==requested_count` 时成功。这样一个 task 要么提交完整 1～4 张，要么重试/失败；batch 的 PARTIAL 来自 task 之间的成功与失败，不在 task 内引入复杂的部分提交状态。

### 6.5 Provider 子限流

公平 reservation 控制逻辑 task；每次物理 provider 调用还必须取得：

```text
image:provider:<provider>[:<model>] lease
```

provider lease 使用 Redis server time、独立 token、TTL/renew/release。若无 permit：

- 不长时间阻塞并占住 fair worker；
- 可尝试下一个显式 fallback；
- 无 candidate 时返回 RATE_LIMITED，task 进入 MySQL 延迟重试。

provider limiter 丢失不会改变业务 task 真相；Redis 不可用时 fair scheduler 本身已经停止新任务。

## 7. 拆分与确定性

### 7.1 规范化

create 先把两种输入统一为：

```go
type NormalizedItem struct {
    Index  int
    Label  string
    Prompt string
    Size   string
    Count  int
}
```

`label` 为空时生成稳定 `item-<index>`。prompt 保持用户文本，但 request fingerprint 使用规范化 UTF-8、size、count 和 schema version。

### 7.2 确定性拆分

每个 item 按 count 依次拆成最多 4 张：

```text
count=1 -> [1]
count=4 -> [4]
count=5 -> [4,1]
count=9 -> [4,4,1]
count=16 -> [4,4,4,4]
```

task 顺序键：

```text
(item_index, chunk_index, task_id)
```

同一 batch 重试创建请求时必须通过 idempotency key 或调用侧唯一 create nonce 防止重复 batch。模型工具一期可由服务端生成 nonce 并只保证单次 handler 调用事务原子；若未来开放 HTTP batch API，必须要求客户端 idempotency key。

### 7.3 输出顺序

最终 artifact 按：

```text
item_index, chunk_index, image_index
```

排序。provider 完成顺序不能改变用户看到的顺序。

## 8. 最小 MySQL 数据模型

### 8.1 Imagegen只新增两张业务表

```text
image_generation_batches
image_generation_tasks
```

Imagegen 不创建新的通用 jobs/outbox/tenant 表，也不创建 artifact 表；它复用 RAG 前置已经 expand 的 `fairqueue_resource_operations` 通用安全 journal。fairqueue 发布凭据仍直接使用 task 的 generation-aware marker，journal 只保存 special recovery intent/progress，不充当任务 outbox。

### 8.2 `image_generation_batches`

概念字段：

```sql
id                       VARCHAR(120) PRIMARY KEY
user_id                  VARCHAR(120) NOT NULL
config_user_id           VARCHAR(120) NOT NULL
agent_owner_user_id      VARCHAR(120) NOT NULL
agent_id                 VARCHAR(120) NOT NULL
workspace_project_id     VARCHAR(120) NOT NULL DEFAULT ''
workspace_session_id     VARCHAR(191) NOT NULL DEFAULT ''
request_json             JSON NOT NULL
provider_plan_json       JSON NOT NULL
status                   VARCHAR(32) NOT NULL
requested_count          INTEGER NOT NULL
succeeded_count          INTEGER NOT NULL DEFAULT 0
failed_count             INTEGER NOT NULL DEFAULT 0
canceled_count           INTEGER NOT NULL DEFAULT 0
cancel_requested         BOOLEAN NOT NULL DEFAULT FALSE
error_msg                TEXT NULL
created_at               DATETIME(6) NOT NULL
started_at               DATETIME(6) NULL
finished_at              DATETIME(6) NULL
updated_at               DATETIME(6) NOT NULL
```

索引至少覆盖：

```text
(user_id, created_at, id)
(status, updated_at, id)
(agent_id, workspace_session_id, created_at)
```

`request_json` 和 `provider_plan_json` 有 schema version；provider plan 必须通过 secret scanner 测试。

### 8.3 `image_generation_tasks`

概念字段：

```sql
id                  VARCHAR(120) PRIMARY KEY
sequence_id         BIGINT NOT NULL AUTO_INCREMENT UNIQUE
batch_id            VARCHAR(120) NOT NULL
user_id             VARCHAR(120) NOT NULL
item_index           INTEGER NOT NULL
chunk_index          INTEGER NOT NULL
label                VARCHAR(191) NOT NULL DEFAULT ''
prompt               LONGTEXT NOT NULL
size                 VARCHAR(64) NOT NULL
requested_count      INTEGER NOT NULL
request_fingerprint  CHAR(64) NOT NULL
status               VARCHAR(32) NOT NULL
retry_count          INTEGER NOT NULL DEFAULT 0
max_retry            INTEGER NOT NULL
claim_generation     BIGINT NOT NULL DEFAULT 0
dispatch_generation  BIGINT NOT NULL DEFAULT 1
lease_owner          VARCHAR(191) NULL
lease_until          DATETIME(6) NULL
heartbeat_at         DATETIME(6) NULL
next_run_at          DATETIME(6) NULL
dispatched_at        DATETIME(6) NULL
provider             VARCHAR(120) NOT NULL DEFAULT ''
model                VARCHAR(191) NOT NULL DEFAULT ''
manifest_key         VARCHAR(1024) NOT NULL DEFAULT ''
artifacts_json       JSON NULL
error_code           VARCHAR(64) NOT NULL DEFAULT ''
error_msg            TEXT NULL
created_at           DATETIME(6) NOT NULL
started_at           DATETIME(6) NULL
finished_at          DATETIME(6) NULL
updated_at           DATETIME(6) NOT NULL
```

约束/索引：

```text
UNIQUE(batch_id,item_index,chunk_index)
CHECK(requested_count BETWEEN 1 AND 4)  # MySQL 版本支持时；代码仍必须校验
(status,dispatched_at,next_run_at,lease_until,id)
(user_id,status,lease_until,id)
(user_id,sequence_id)
(status,dispatched_at,sequence_id)
(status,lease_until,sequence_id)
(batch_id,status,item_index,chunk_index)
```

`user_id` 从 batch 冗余，供公平发布、claim 容量统计和恢复分页；task 创建后不可修改。领域 `id` 可能是非单调字符串，因此 recovery/broker-repair high-water 与 task page cursor 必须使用 MySQL 单调 `sequence_id`，不能使用 `MAX(id)` 或假定 UUID/随机 ID 有创建顺序。索引最终列序必须在 real MySQL 上用 `EXPLAIN ANALYZE`/rows-examined 证明 dispatch、expired rearm、known/dispatched/running recovery page、broker repair 与 global/per-user capacity count 都是有界索引访问。

### 8.4 状态

Batch：

```text
PENDING / RUNNING / DONE / PARTIAL / FAILED / CANCELING / CANCELED
```

Task：

```text
PENDING / RUNNING / DONE / FAILED / CANCELED
```

`DISPATCHED` 和 `RETRY_WAIT` 仍是组合概念状态：

| 概念状态 | Task MySQL 表达 |
|---|---|
| due 待发布 | PENDING + marker NULL + next_run 到期 |
| 延迟重试 | PENDING + marker NULL + future next_run |
| 已发布 | PENDING + dispatched 非空 |
| 运行中 | RUNNING + 有效 lease |
| 崩溃待 rearm | RUNNING + lease expired + `dispatch_generation=claim_generation` |
| 已 rearm 待发布 | RUNNING + lease expired + `dispatch_generation>claim_generation` + marker NULL |
| reclaim 消息已发布 | RUNNING + lease expired + `dispatch_generation>claim_generation` + marker 非空 |
| 终态 | DONE/FAILED/CANCELED |

task 初建 `dispatch_generation=1, claim_generation=0`。每次 retry、expired reclaim rearm、Rabbit 灾难恢复或 canonical repair 都必须原子 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1、dispatched_at=NULL`。精确 claim 只接受 `message generation == row dispatch_generation > row claim_generation`，成功后把 `claim_generation` **直接设置为** dispatch generation，并写 `dispatched_at=COALESCE(dispatched_at,DB_NOW)`；heartbeat 保留该 marker。这样 delivery 已 claim 但 publisher Mark 迟到时，任务不会因 marker 被清空而再次形成同 generation 发布义务，canonical repair 导致的安全跳号也不会被 `claim_generation++` 破坏。

### 8.5 Batch 聚合

task 首次进入 RUNNING 时 CAS 把 batch PENDING 变为 RUNNING 并设置 started_at。每个 task 首次进入终态时，在同一事务内锁定 batch 并重新聚合或幂等增量：

```text
all tasks DONE                         -> DONE
some DONE, some FAILED                -> PARTIAL
all terminal and no DONE, has FAILED  -> FAILED
cancel requested and no RUNNING       -> CANCELED
otherwise                             -> RUNNING/CANCELING
```

重复 finalize 不能重复增加计数。batch 的 requested_count 永远等于 task requested_count 总和。

## 9. Artifact 持久化

### 9.1 对象布局

图片与 manifest 存入 batch 创建时固定的 workspace scope：

```text
imagegen/<batchID>/<taskID>/claims/<claimGeneration>/
  image-0-<sha256>.<ext>
  image-1-<sha256>.<ext>
  ...
  manifest.json
```

claim generation 进入路径，防止失去 fence 的旧 worker 覆盖新 worker 对象。图片名包含内容哈希，manifest 最后写入。

### 9.2 Manifest

manifest 至少包含：

```json
{
  "version": 1,
  "batch_id": "imgb_x",
  "task_id": "imgt_0123456789abcdef",
  "claim_generation": 3,
  "request_fingerprint": "...",
  "provider": "openai",
  "model": "gpt-image-1",
  "artifacts": [
    {
      "path": "imagegen/.../image-0-....png",
      "mime_type": "image/png",
      "byte_size": 12345,
      "sha256": "...",
      "width": 1024,
      "height": 1024,
      "image_index": 0
    }
  ]
}
```

worker 顺序固定：

1. 验证 provider 返回数量、MIME、尺寸、单图和 batch 字节限制；
2. 写所有 immutable image objects；
3. 写 manifest；
4. 用 `taskID + claimGeneration + leaseOwner + lease_until>DB_NOW + dispatch_generation=claim_generation` 做 MySQL fenced finalize；
5. 更新 batch 聚合；
6. release Redis reservation。

### 9.3 崩溃后避免重复付费

若图片/manifest 已写成功但 MySQL finalize 前崩溃，新 claim 在再次调用 provider 前检查 exact reclaim 返回的 `PreviousClaimGeneration` manifest：

- task ID、batch ID、request fingerprint、requested count 完全匹配；
- artifact 对象存在且哈希/大小符合 manifest；
- batch 未取消；

则可以把该 manifest 作为 recovered result 走当前 claim 的 fenced finalize，不再次生成。`PreviousClaimGeneration` 来自事务覆盖前的 row，不用当前 generation 减一推测，因为 canonical repair 允许安全跳号。manifest 不完整或校验失败时才重新调用 provider。

### 9.4 远程 URL 与 base64

- base64 在 provider adapter 内解码为 bytes，严禁进入 tool text。
- 远程 URL 必须使用受限 downloader 获取后持久化。
- downloader 只允许 HTTP(S)，限制重定向、响应大小、总超时和 Content-Type。
- 默认拒绝 private/link-local/loopback 目标，除非来自管理员明确配置的受信自托管 endpoint。
- 单图默认上限和 batch 总字节上限可配置；超限视为 MALFORMED_RESULT。
- MIME 必须为允许的 image 类型，并使用 magic bytes 验证，不能只信响应 header/文件扩展名。

### 9.5 Workspace 后端

单实例可使用 LocalFS。多个 bkcrab 实例共同消费 `image.generate` 时必须使用共享 workspace 后端（S3/兼容对象存储）；否则 worker A 写出的图片无法被 gateway B/status 工具读取。

启用 imagegen fair mode 时 health 必须暴露 workspace backend。部署副本数大于 1 而使用 LocalFS 属于不受支持配置。

## 10. 公平队列接入

### 10.1 Resource 与消息

```text
resource = image.generate
task_type = image_generation
tenant_id = task.user_id
```

这里的 resource 是一类任务共用的调度与容量池标识。`image.generate` 表示“图片生成”资源池；它拥有独立的 Rabbit 路由、Redis 公平状态和 global/user 并发计数，不与 `rag.index` 的文档索引容量相互挤占。它不是模型名、provider 名或数据库表名。

Rabbit payload 只包含：

```json
{
  "version": 1,
  "resource": "image.generate",
  "tenant_id": "u_x",
  "task_type": "image_generation",
  "task_id": "imgt_0123456789abcdef",
  "dispatch_token": {
    "resource": "image.generate",
    "task_id": "imgt_0123456789abcdef",
    "generation": 2
  }
}
```

不包含 prompt、size、provider plan、workspace key、API key 或 artifact。

注册 `image.generate` 时必须提供通用 ResourceConfig task-ID validator：canonical task ID固定为 `imgt_` + 16～64个小写字母/数字（总长有界），其它前缀、大小写、分隔符或超长值在transport层拒绝。通用fairqueue仍不硬编码该规则。

Rabbit topology、mandatory publish、publisher confirm、basic.return、durable DLQ、PublishAttemptID、四个固定 stable identity AMQP headers、body 损坏时的 canonical repair 与 stable DispatchToken/Guard 语义完全复用 RAG fairqueue 设计。consumer 只在 body/header/registered-resource/queue-tenant 全部一致时填 `PrepareRequest.Message`；body-only、header-only 或 mismatch 只能作为 repair locator，绝不能执行。mismatch 时对每个能独立通过 resource、queue tenant、MySQL canonical batch/task、当前 generation、due 且 batch 未取消校验的候选（最多两个）分别 generation repair，不能猜哪一边可信；额外重发由 exact claim 去重。Imagegen adapter 不得绕过通用 transport 自行发布缺 header 的 v1 消息或重新解析 raw body。

Imagegen store 提供窄 `RepairPoisonImageCandidate`：只接收transport已限形的locator、registered resource与queue tenant hash，在expected-writer pinned事务中加载canonical batch/task并执行上述CAS，返回新DispatchCandidate或stale/unlocatable disposition。它不得接收/信任payload owner来改row；batch canceled/终态/valid RUNNING只no-op。TaskPreparer逐locator调用该API，通用runtime负责repair成功后confirmed DLQ再ACK。

### 10.2 发布候选

Imagegen DispatchSource 选择完整 `DispatchCandidate{Message, Guard}`；Guard 至少绑定 status、dispatch/claim generation、retry、next_run 和 lease/due snapshot。候选条件为：

```text
status IN (PENDING, RUNNING)
AND dispatch_generation > claim_generation
AND dispatched_at IS NULL
AND (next_run_at IS NULL OR next_run_at <= DB_NOW)
AND (status=PENDING OR lease_until IS NULL OR lease_until <= DB_NOW)
AND batch.cancel_requested = false
```

常驻 expired-RUNNING sweeper 只在 `lease expired + next_run due + dispatch_generation=claim_generation` 时，以完整 row snapshot CAS 武装为 `dispatch_generation=claim_generation+1、dispatched_at=NULL`，不直接把 task 改成 PENDING。通用 dispatcher 随后才能选中该 RUNNING candidate；真正 retry/reclaim 只在 exact claim 事务内发生。Mark 必须接收发布前的原始 Guard并重新以 DB_NOW 校验 due，不能只凭 token 写 marker。

### 10.3 Redis 公平状态

`image.generate` 使用自己的一组：

- resource state/epoch；
- known/active/ring/ring_members；
- processing turn lease；
- global/user provisional + stable reservation；
- recovery lock；
- reconciliation。

它与 `rag.index` key namespace 和并发槽完全隔离。Round Robin（轮询）的含义是：有积压任务的用户轮流获得下一次调度机会。例如 A、B 都有待执行任务时，顺序近似为 `A1 -> B1 -> A2 -> B2`，而不是先跑完 A 的全部任务。轮询粒度是 task，不是 batch；因此 A 的 4 个 task 不会阻塞 B 后到的 1 个 task。

### 10.4 Exact claim 与最终容量闸门

新增领域方法概念：

```go
ClaimImageGenerationTaskByID(
    ctx,
    taskID,
    dispatchToken,
    expectedUserID,
    workerID,
    leaseDuration,
    limits,
) (claim, disposition, error)
```

完整锁序：

```text
pinned writer conn
  -> verify @@server_uuid + DATABASE() + CONNECTION_ID()
  -> GET_LOCK(resource advisory lock)
  -> verify same session identity again
  -> BEGIN
  -> batch row
  -> task row
  -> validate tenant/token/due/cancel
  -> count valid image_generation_tasks RUNNING
  -> global/burst capacity guard
  -> claim/reclaim + claim_generation=dispatch_generation
  -> COMMIT
  -> RELEASE_LOCK
```

advisory lock 名称算法必须与通用RAG设计一致，仅resource为`image.generate`。每次claim/heartbeat/capacity临界区都在同一pinned physical connection上按通用协议校验expected writer fingerprint和session affinity；mismatch必须物理discard、关闭本地gate并取消pipeline，不得继续使用进程启动时缓存的fingerprint。相同expected-writer pinned read/transaction helper还覆盖batch create/status/cancel、DispatchSource list/by-ID/Mark、expired-cancel/rearm、poison/broker repair、RecoverySource CaptureHighWater/每页、continuous reconciliation与所有finalize；任何读页也不能在未验证连接上返回snapshot。统计只看authoritative writer的`RUNNING + lease_until > DB_NOW`：

```text
resource valid running < global 4
tenant valid running   < burst 4
```

base=2 与借用判断仍由 Redis active-user policy 负责。MySQL 只提供 global/burst 最终安全上限。expired RUNNING reclaim 还要返回覆盖前的 `PreviousClaimGeneration`，供 worker 精确检查上一实际 claim 的 manifest；不能假设它总是 `newClaimGeneration-1`，因为 generation repair 允许安全跳号。

### 10.5 Heartbeat、ACK 与 PreparedTask

- claim 与 heartbeat 使用同一个 resource advisory lock和 authoritative writer；writer identity/Redis resource fence 复用通用协议。
- claim 提交后把 provisional reservation promote 为 `taskID + claimGeneration` stable token。
- promote 成功后 ACK Rabbit，随后运行 provider 调用。
- promote 失败则不调用 provider、不续 MySQL lease；ACK 当前 delivery，等待 lease 到期重发。
- worker 同时 heartbeat MySQL task lease 和 Redis stable reservation。MySQL heartbeat CAS 必须匹配 RUNNING、owner、claim generation、`lease_until>DB_NOW` 与 `dispatch_generation=claim_generation`，保留 dispatched marker；sweeper 一旦推进 generation，旧 worker 永久丢 fence，不能把过期 lease 续回。
- heartbeat 观察 batch cancel_requested；命中取消后 cancel provider ctx，停止续租并走 fenced cancel。
- claim 返回的 execution fence 携 expected writer fingerprint。FinishDone/Retry/Failed/Canceled、batch 聚合以及 salvage finalize 都必须先在 pinned connection 上重新验证 writer/session identity，再在同一事务内校验 RUNNING、owner、claim generation、未过期 lease 与 `dispatch_generation=claim_generation`；stale worker、expired lease 或 rearm 后的旧 worker不能提交 artifact/aggregate。Retry 原子写 `dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1`、marker NULL、PENDING 与 future next_run。
- PreparedTask.Run 无论成功、失败、panic 或 cancel 都必须 release reservation。

## 11. 生命周期状态机

### 11.1 正常成功

```text
batch/create transaction
  -> batch PENDING
  -> task PENDING, dispatch_generation=1
  -> Rabbit confirm + mark dispatched
  -> Redis reservation
  -> MySQL exact claim
  -> task RUNNING, batch RUNNING
  -> provider + artifact manifest
  -> task DONE
  -> all task terminal
      ├─ all DONE -> batch DONE
      ├─ some DONE + some FAILED -> batch PARTIAL
      └─ all FAILED -> batch FAILED
```

### 11.2 Retry

```text
RUNNING
  -> retriable provider/service error
  -> PENDING
     retry_count+1
     next_run_at=future
     dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1
     dispatched_at=NULL
     clear lease
  -> due 后重新发布
```

不执行 Rabbit immediate requeue，避免未到期任务反复占用用户轮次。

### 11.3 Worker 崩溃

```text
RUNNING + lease expired
  -> sweeper generation-aware rearm
  -> publish reclaim delivery
  -> exact reclaim
     ├─ 找到完整 manifest -> salvage finalize
     ├─ retry_count 未耗尽 -> new claim + provider
     └─ retry_count 耗尽 -> FAILED
```

### 11.4 三方职责

| 状态 | MySQL | RabbitMQ | Redis |
|---|---|---|---|
| batch/task 创建 | durable PENDING | 无 | 无 |
| 已发布 | task marker 非空 | durable tenant message | active ring |
| 已取待 claim | task 仍 PENDING | unacked delivery | provisional reservation |
| 运行 | RUNNING + lease/fence | 已 ACK | stable reservation |
| 延迟重试 | PENDING + future next_run | 无 | 已 release |
| 终态 | DONE/FAILED/CANCELED + artifact refs | 无业务消息 | 无 reservation |

## 12. Cancel 语义

`action=cancel` 在 MySQL 事务中：

1. 锁定并验证 batch owner；
2. 终态 batch 原样返回；
3. 设置 `cancel_requested=true`；
4. 把当前 PENDING tasks 通过 generation-aware CAS 改为 CANCELED；
5. RUNNING tasks 保持 RUNNING，batch 进入 CANCELING；
6. 聚合后若无 RUNNING，batch 进入 CANCELED。

RUNNING worker：

- heartbeat 或 provider ctx 观察取消；
- 尝试取消 HTTP/polling；
- provider 不支持取消时允许物理请求继续，但结果不得越过 MySQL cancel fence 成为 DONE；
- 已写但未提交的 claim-generation objects 由清理流程回收；
- task fenced mark CANCELED，release provider/fair reservations。

取消不能依赖活 worker才收敛。常驻 expired-task sweeper 在同一 `image.generate` resource advisory lock 与 writer/session fence 下，先锁 batch再锁 task；发现 `batch.cancel_requested=true + task RUNNING + lease_until<=DB_NOW` 时，不再 rearm/publish，而是以原 claim/generation/lease Guard 原子转为 CANCELED、清 lease并聚合 batch。它也覆盖已经 rearm 但尚未重新 claim 的 expired RUNNING；迟到 delivery 随后只能得到 terminal/stale ACK。heartbeat 与 expired-cancel 由同一锁全序化：heartbeat 先赢则继续由 worker观察 cancel，expired-cancel 先赢则旧 worker永久丢 fence。崩溃 worker 必须最终使 batch CANCELED，且不会发起新的 provider call。

batch CANCELED 可以保留取消前已完成 artifacts，并在 status 中返回；状态仍为 CANCELED，不改为 PARTIAL。

## 13. Tool 等待、Status 与结果交付

### 13.1 有限等待

create 提交后等待器只观察 MySQL：

- 同进程事件/Redis pubsub 可以作为唤醒优化；
- 每次醒来必须回读 MySQL batch/task；
- pubsub 丢失不能导致永久等待；
- 使用带抖动的有界轮询，ctx 或 wait deadline 到期立即退出。

等待不占用 fairqueue worker，只占当前 Agent tool goroutine。180/240 秒上限低于 IM 默认 300 秒外层 task timeout。

### 13.2 Status 返回

status 返回：

```text
batch_id
status
requested/succeeded/failed/canceled
created/started/finished
ordered artifacts
per-item/task bounded error summaries
```

错误不得包含 provider 原始响应、secret、完整 URL query 或 prompt。模型可见 artifact 使用 workspace 相对路径或短期 signed URL。

### 13.3 Typed result metadata

扩展可信元数据：

```text
ImageArtifactsMetadataKey = "imageArtifacts"
```

只有 builtin `image_gen_batch` 可以生产。每个 ref 至少带 batch/task identity、origin workspace scope、path、MIME、尺寸和排序索引。Registry 必须限制数量为 16、总 metadata 字节数，并验证 path 位于 canonical `imagegen/<batchID>/...`。

Agent/gateway 使用该 side channel 交付图片，不能从任意 tool text 正则提取内部 object key。模型仍看到简短 Markdown，方便 Web 渲染和自然回复。

### 13.4 跨会话 status

batch canonical owner 是 `user_id + agent_id`：同 Agent 可以在另一会话查询状态，另一 Agent 即使属于同一 user 也一期拒绝。artifact metadata 带原始 workspace project/session scope，交付层按 origin scope 读取；不得错误地用当前会话 scope 查找同名路径。

若使用 S3，可附带短期 signed URL；LocalFS 由 gateway 读取 bytes 后作为 MediaItem 发送。跨 Agent 查询一期拒绝，避免 workspace 授权扩散。

## 14. 故障与恢复

### 14.1 RabbitMQ 不可用

- create 事务仍成功；
- task 保持 due + marker NULL；
- dispatcher 退避重连；
- Rabbit 恢复后声明 topology、mandatory publish、confirm 后 mark；
- tool 等待可能超时并返回 batch ID，不把 batch 标成失败。

### 14.2 Redis 不可用

- scheduler 停止领取新 task；
- 已 RUNNING task 继续依赖 MySQL fence；provider limiter 获取失败的尚未调用 task 进入重试；
- Redis 恢复后按 fairqueue recovery epoch 重建 image resource active/inflight；
- 不回退到旧同步 `image_gen`。

### 14.3 MySQL 不可用

- create 失败，不向 Rabbit 发送孤儿消息；
- claim 前无法判断状态时 NACK requeue 并 release provisional；
- RUNNING worker heartbeat 失败时取消 provider ctx，禁止无 fence finalize；
- 对象已写、DB finalize 失败时 manifest 供后续 salvage。

### 14.4 Provider 超时/限流

- service 按显式 chain fallback；
- 全部 candidate 耗尽后分类为 delayed retry 或 permanent failure；
- retry 只重做当前 task，不重做同 batch 已 DONE tasks；
- max retry 到期后 task FAILED，batch 可能 PARTIAL。

### 14.5 重复/迟到 Rabbit delivery

- stable DispatchToken generation 必须匹配 task 当前 generation；
- 只有一个 exact claim 成功；
- stale/duplicate delivery 按 fairqueue disposition ACK 或进入 DLQ/canonical repair；
- 任何消息 tenant 不可信，canonical `task.user_id` 才是依据。

### 14.6 Redis 重建

RecoverySource 提供：

- bounded attempts drain 后捕获 `high_water=MAX(sequence_id)`；
- 所有 canonical task 的 distinct known tenant keyset page；
- dispatched canonical task/tenant 的独立 keyset page，结合 Rabbit ready depth 才恢复 active；
- valid RUNNING task 的 stable token identity keyset page；
- READY 后由独立 `ExpiredRearmSource` 与 `DispatchSource` 常驻 loop 补齐 expired/due work；它们不属于 RecoverySource，也不在 RECOVERING 中 mutation/publish。

所有 page 都显式接收 `highWater`，以 `sequence_id<=highWater`、稳定唯一 keyset cursor 和有界内存查询；known-only tenant 只恢复 topology，不进入 active/ring。valid-RUNNING query 同时返回 MySQL DB_NOW，stable identity 使用 task ID + 实际 claim generation；剩余 lease 转为 Redis TIME 相对 TTL。每次runtime启动/Redis重连先在expected-writer pinned connection读取`resource=image.generate`的通用MySQL operation journal，再决定join或rebuild；ACTIVE、无匹配READY control的READY_COMMITTED、未完成journal与control kind/operation ID/writer不一致都保持operator-required，NORMAL不能覆盖。READY_COMMITTED只有与Redis READY的`last_completed_operation_id`精确匹配才可开放gate并补记COMPLETED；COMPLETED视为无未完成special，Redis control丢失时可按通用NORMAL协议重建，不要求历史ID被新NORMAL control继承。其余recovery gate、持久progress、provisional/processing cleanup、stable-token双向收敛和READY前检查完全复用通用协议，完成前scheduler/dispatcher/publisher不开放。重建只操作`image.generate`namespace，不影响RAG。

### 14.7 Rabbit 数据灾难恢复

复用 `bkcrab admin fairqueue rabbit-disaster-repair --resource image.generate` 的通用 fenced operator；Imagegen 提供自己的 repair source与operation-journal adapter。apply仍要求旧broker已隔离、publisher已暂停的双确认，并严格复用“MySQL session start lock→Redis raw lock renew/check→kind-specific control preflight→MySQL ACTIVE journal CAS→同owner/operation ID BeginWithLock”的顺序与Rabbit允许起态矩阵；其中ACTIVE journal+READY/NONE同参数必须能恢复pre-Begin崩溃，READY_COMMITTED+READY/last-ID同值只Complete。随后才对仍需执行且batch未取消的image task执行：

```text
dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1
dispatched_at=NULL
```

operator在RECOVERING中只完成generation rearm与topology/recovery rebuild，不publish；repair high-water/pass先持久化到MySQL journal再镜像Redis，Finish严格按`ACTIVE→READY_COMMITTED`、Redis READY/last-completed ID、`COMPLETED`三阶段提交。rearm半页后Redis同时全丢，ACTIVE journal仍阻止NORMAL；同kind operator按原ID rehydrate并从头幂等收敛。READY后普通READY-fenced dispatcher扫描marker=NULL row重发。不得盲目重发终态或取消batch的task。

`image.generate` 还必须注册到通用 `rebind-writer` 与 `redis-force-rebuild` operator。writer rebind使用Imagegen `WriterRebindSource`校验新writer/schema/generation invariant/valid-RUNNING=0，并要求旧writer物理fence、所有`image.generate` runtime/recovery coordinator从首次apply保持停止到journal COMPLETED；ACTIVE journal+READY expected-old允许重试尚未发生的首次CAS。force只允许从卡住的RECOVERING+NORMAL、same-ID FORCE或未完成journal+missing起态，并只删除`{image.generate}`可重建Redis keys；READY_COMMITTED rehydrate要重做Redis通用passes。两者复用通用start fence、operation ID、journal三阶段提交、Redis全丢resume、dry-run/attestation及standalone限制，不能只为`rag.index`注册CLI source。

### 14.8 Object store 故障

- provider 成功但对象写失败：不把 task 标 DONE，按可重试错误处理；
- 图片对象写完、manifest 失败：不 finalize，重试可覆盖该 claim prefix；
- manifest 完成、MySQL 失败：下一 claim salvage；
- object store 长期不可用最终使 task FAILED/PARTIAL，但不在 MySQL 存 base64 兜底。

## 15. 配置

建议部署级配置：

```text
BKCRAB_IMAGEGEN_BATCH_MODE=legacy # legacy|drain|fair
BKCRAB_IMAGEGEN_MAX_IMAGES_PER_BATCH=16
BKCRAB_IMAGEGEN_MAX_IMAGES_PER_TASK=4
BKCRAB_IMAGEGEN_TOOL_WAIT_DEFAULT=180s
BKCRAB_IMAGEGEN_TOOL_WAIT_MAX=240s
BKCRAB_IMAGEGEN_PROMPT_MAX_RUNES=8000
BKCRAB_IMAGEGEN_REQUEST_MAX_BYTES=131072
BKCRAB_IMAGEGEN_IMAGE_MAX_BYTES=20971520
BKCRAB_IMAGEGEN_BATCH_MAX_BYTES=134217728

BKCRAB_IMAGEGEN_LOCAL_WORKERS=4
BKCRAB_IMAGEGEN_GLOBAL_CONCURRENCY=4
BKCRAB_IMAGEGEN_PER_USER_BASE_CONCURRENCY=2
BKCRAB_IMAGEGEN_PER_USER_BURST_CONCURRENCY=4
BKCRAB_IMAGEGEN_BORROW_ENABLED=true
BKCRAB_IMAGEGEN_TASK_LEASE=180s
BKCRAB_IMAGEGEN_TASK_HEARTBEAT=30s
BKCRAB_IMAGEGEN_RESERVATION_TTL=180s
BKCRAB_IMAGEGEN_RESERVATION_HEARTBEAT=30s
BKCRAB_IMAGEGEN_PREPARE_TIMEOUT=10s
BKCRAB_IMAGEGEN_PROVISIONAL_TTL=15s
BKCRAB_IMAGEGEN_PROCESSING_TTL=15s
BKCRAB_IMAGEGEN_PUBLISH_ATTEMPT_TIMEOUT=15s
BKCRAB_IMAGEGEN_RECOVERY_DRAIN_TIMEOUT=2m
BKCRAB_IMAGEGEN_DISPATCH_INTERVAL=1s
BKCRAB_IMAGEGEN_RECONCILE_INTERVAL=30s
BKCRAB_IMAGEGEN_EXPIRED_SWEEP_INTERVAL=15s
BKCRAB_IMAGEGEN_RECONCILE_PAGE_SIZE=200
BKCRAB_IMAGEGEN_MAX_RETRIES=3
BKCRAB_IMAGEGEN_PROVIDER_CALL_TIMEOUT=120s
BKCRAB_IMAGEGEN_PROVIDER_CONCURRENCY_DEFAULT=4
```

共享 Rabbit/Redis 地址、exchange、resource epoch、processing turn 和 reservation 配置复用 fairqueue 基础配置。

校验：

- max batch 必须在 1～16；一期固定上限不允许配置扩大到 16 以上；
- max task 必须在 1～4；
- base > 0 且 base <= burst <= global；
- heartbeat < task lease；
- reservation heartbeat >0且小于reservation TTL；prepare/provisional/processing/publish-attempt timeout均>0，prepare<provisional且provisional/processing/publish-attempt timeout<recovery drain timeout；所有interval/page size有部署级上限，一期统一duration上限为24h、page size上限为10000；零publish-attempt timeout不得表示无deadline；
- tool wait max < 外层支持的最小 turn/task timeout；
- `legacy` 不启动 image fair runtime，只注册旧同步 `image_gen`；`fair` 启动 runtime并允许 batch create/status/cancel；`drain` 保持 runtime与 status/cancel、拒绝新 create且绝不重新注册旧同步工具；
- fair/drain 要求共享 `fairqueue.enabled=true`，只支持MySQL authoritative single-writer topology并复用通用逐pinned-connection writer identity fence；Rabbit/Redis暂时不可用使调度degraded/recovering，但不让已有batch的status/cancel action消失；仅在fair模式，create仍可先完成只需MySQL的durable commit，drain始终拒绝create；
- 多实例 + LocalFS 明确 unsupported；
- provider plan 不可解析时 fair/drain 仍注册 `image_gen_batch` 供 status/cancel；create 返回 typed unavailable，不能通过隐藏整个工具切断已有 batch 的管理入口。

## 16. 可观测性与安全

### 16.1 指标

- batch create/terminal/status/cancel 数；
- batch requested/succeeded/failed/canceled images；
- task queue wait、claim、provider、artifact write、total duration；
- Round Robin active users、global/base/borrowed inflight；
- provider/model attempt、fallback、429、timeout、5xx、safety rejection；
- provider limiter acquire/deny/lease expiry；
- object bytes、download failures、manifest salvage；
- retry、expired reclaim、duplicate/stale delivery、DLQ；
- tool wait completed/timeout/canceled；
- batch DONE/PARTIAL/FAILED/CANCELED 比例。

### 16.2 日志

允许记录：

```text
batch_id/task_id
user_id 的安全标识或哈希
agent_id
provider/model
count/size
status/error_code
duration/bytes
claim/dispatch generation
```

禁止记录：

- prompt 原文；
- provider 响应正文；
- API key、Authorization header；
- base64；
- 带签名 query 的 URL；
- object store secret；
- 用户不可见的配置 JSON。

### 16.3 访问控制

- create 的 user ID 只从 turn context 获取，不接受模型参数传 tenant。
- status/cancel 同时校验 canonical batch `user_id + agent_id`，允许同 Agent 跨 session，不允许同 user 跨 Agent。
- task message tenant 只作交叉校验。
- artifact path 必须由服务端生成并做 prefix/path traversal 校验。
- typed metadata 只接受 builtin image tool，插件/MCP 不能伪造内部 artifact refs。
- provider URL downloader 必须防 SSRF、zip bomb/超大响应和错误 MIME。

### 16.4 健康状态

`/health` 或 admin health detail 至少区分：

```text
legacy / drain / fair
ready
degraded: rabbit publish unavailable
degraded: redis scheduler paused
degraded: object store unavailable
degraded: no configured image provider
recovering: resource epoch rebuild
blocked: writer fingerprint/session mismatch
blocked: non-MySQL or unsupported multi-instance LocalFS
```

## 17. 测试与验收

### 17.1 单元/组件

- 两种 create 输入规范化与互斥；
- count 总和 1/4/5/16 边界及 17 拒绝；
- 确定性 task 拆分与结果排序；
- provider capability/size 映射；
- fallback 错误分类；
- service 必须返回准确 requested count；
- provider limiter 独立 lease；
- artifact MIME/大小/hash/manifest；
- manifest salvage；
- batch/task 状态聚合和幂等 finalize；
- cancel PENDING/RUNNING/terminal；
- status owner 和跨 session origin scope；
- secret 不进入 plan/Rabbit/log/tool result；
- legacy `image_gen` 在 batch mode 不注册。

### 17.2 MySQL + Rabbit + Redis + object store 集成

1. A 单 prompt count=16，拆成 4 个 task，独占时同时运行 4。
2. A 已运行 4 个 task，B 提交 1 个；A 释放下一个槽后 B 优先启动。
3. A/B 持续 backlog 收敛到基础 2/2，global 永不超过 4。
4. 两个 bkcrab 实例合计 valid RUNNING 不超过 4。
5. 删除 Redis reservation 后，MySQL final guard 仍拒绝第 5 个 valid RUNNING。
6. 同一 task 重复 publish 只成功 claim/finalize 一次。
7. claim/Mark 同 generation 竞态不产生新的发布义务；迟到 Guard 不能覆盖新状态。
8. heartbeat 与 sweeper 只有先提交者胜出；rearm 后旧 provider worker 无法 heartbeat/finalize。
9. provider 成功、manifest 写完、DB finalize 前 kill worker；恢复后不二次调用 provider。
10. provider 429/5xx 触发 fallback；全部失败后只重试当前 task。
11. 一个 task 永久失败、其它成功，batch 为 PARTIAL 且成功图片可读取。
12. wait 180 秒到期只返回 batch ID，不取消任务；后续 status 返回结果。
13. cancel 后 PENDING 不运行，RUNNING 不能越过 fence 提交 DONE。
14. Rabbit/Redis/object store 分别停机恢复，batch 不丢且状态收敛。
15. 16 张图片不向模型上下文写入 base64。

### 17.3 验收不变量

```text
每 batch requested_count <= 16
每 task requested_count <= 4
task.user_id == batch.user_id
Rabbit 不含 prompt/secret/artifact bytes
DONE task artifact 数 == requested_count
batch counters == terminal task/artifact 聚合
有效 RUNNING global <= 4
有效 RUNNING per-user <= 4
同 task/claim generation 只 finalize 一次
finalize 必须持有未过期 lease 且 dispatch_generation=claim_generation
取消 batch 不产生新的 provider call
```

## 18. 发布、兼容与回滚

### 18.1 发布

1. 先完成并部署通用 fairqueue，保持 `BKCRAB_IMAGEGEN_BATCH_MODE=legacy`。
2. expand migration 创建两张新表；旧同步工具不读取这些表。
3. 以mode=legacy全量部署typed provider service、batch store、artifact publisher和adapter；确认所有实例都认识`image.generate`/新表且更旧binary归零，此时仍只暴露旧同步工具。
4. 第一次全量rollout `legacy→drain`：混合期legacy Pod仍可完成旧同步调用，drain Pod隐藏旧工具且拒绝batch create；确认legacy Pod/旧调用全部归零。
5. 第二次全量rollout `drain→fair`：混合期两边都不暴露旧同步工具，只有fair Pod接受batch create，因此不会绕过公平容量。
6. 在全量fair部署内先用测试用户/人工流量灰度，观察fallback、queue wait、artifact、PARTIAL和DLQ；不能用legacy/fair Pod canary做灰度。
7. 多实例前切换共享workspace backend，并验证跨Pod status读取。

旧二进制/legacy mode的`image_gen`会绕过公平队列，因此不能直接rolling legacy→fair。drain是正向与回滚都必须经过的安全桥；平台无法证明每阶段旧ReplicaSet归零时保持drain。

### 18.2 回滚

- 先把所有实例全量切到 `drain`：停止新的 batch create，status/cancel 与 batch runtime 保持可用，旧同步工具仍隐藏；
- 已创建 batch 继续 drain，或由管理员显式 cancel；
- 无 PENDING/RUNNING/CANCELING batch 且 valid RUNNING 归零后才允许全量切回 `legacy` 恢复旧同步工具；禁止 fair 与 legacy 混跑；
- schema 和对象保留，不做破坏性回滚；
- 回滚不得把 PENDING batch 悄悄转成同步直接调用。

## 19. 后续扩展

在不改变 fairqueue 核心的前提下可继续增加：

- 主动完成通知和 durable notification progress；
- batch HTTP/API 与客户端 idempotency key；
- 逐图 regenerate/approve/delete；
- 图片编辑、参考图和图生图；
- provider 成本/配额计费；
- 逐 provider 动态速率限制；
- artifact 独立表与分享权限；
- 更大的离线 batch resource；
- UI batch 进度、取消和画廊。

## 20. 参考代码

- `internal/agent/tools/image_gen.go`
- `internal/toolproviders/registry.go`
- `internal/toolproviders/imagegen/imagegen.go`
- `internal/toolproviders/imagegen/openai.go`
- `internal/toolproviders/imagegen/fal.go`
- `internal/toolproviders/imagegen/replicate.go`
- `internal/workspace/workspace.go`
- `internal/gateway/gateway.go`
- `internal/agent/tools/result.go`
- `internal/store/database_mysql.go`
- `docs/superpowers/specs/2026-08-01-rag-fair-queue-design.md`
