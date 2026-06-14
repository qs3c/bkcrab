# BkClaw 用户级扩展隔离运行时设计

## 1. 状态

本文记录 2026-06-14 已确认的设计决策，覆盖 BkClaw 的 MCP、原生插件和
OpenClaw 插件从系统级进程迁移到用户级安装模型，并通过每用户独立的
Extension Gateway Docker 容器运行不可信常驻扩展。

对应 Gateway 侧协议设计位于：

`E:\fromGithub\mcp-gateway\docs\superpowers\specs\2026-06-14-extension-gateway-plugin-runtime-design.md`

## 2. 目标

- stdio MCP 和插件不再运行在 BkClaw 容器内。
- 每个 BkClaw 用户拥有独立的 Extension Gateway 容器和持久卷。
- MCP、插件的安装、配置、密钥和启用状态归属于用户。
- 用户安装的扩展默认可用于该用户能访问的所有 Agent，Agent 可显式禁用。
- 访问公开或获授权的他人 Agent 时，仍使用当前聊天用户自己的扩展与凭据。
- Tool/Hook/Provider/Channel 能力使用统一的用户级扩展解析规则。
- 普通扩展按需启动；`alwaysOn` 扩展持续在线。
- HTTP MCP 保持由 BkClaw 直接访问，但改用相同的用户级安装和 Agent 覆盖模型。
- 内置工具和会话 Sandbox 的行为保持不变。

## 3. 非目标

- 首期不实现 Kubernetes Pod/CRD 扩展运行时。
- 首期不实现每个扩展独立容器。
- 首期不实现域名级出网白名单。
- 首期不实现独立的扩展市场服务。
- 首期不让 Gateway 访问 Docker socket 或创建容器。
- 首期不允许 Gateway 成为扩展配置的持久化事实源。

## 4. 核心架构

```text
BkClaw
├─ Extension Catalog
│  ├─ 内置插件包
│  └─ 可安装 MCP/插件描述
├─ Extension Installation Store
│  ├─ 用户安装记录
│  └─ Agent 覆盖记录
├─ ExtensionService
│  ├─ 能力解析
│  ├─ 调用路由
│  ├─ 配置 reconcile
│  └─ 主动事件校验
├─ ExtensionRuntimePool
│  └─ userID -> 独立 Extension Gateway Docker
└─ Session Sandbox Pool
   └─ agent/project/session -> 临时执行 Sandbox
```

运行时隔离边界为：

```text
userID
├─ 独立容器
├─ 独立持久卷
├─ 独立 Bearer Token
└─ 该用户安装的多个扩展子进程
```

同一用户的扩展首期共享一个容器。不同用户不得共享容器、卷或鉴权令牌。

## 5. 所有权与继承

### 5.1 系统级

系统级只保存可安装目录和内置扩展包，不直接代表任何用户已安装或已启用。

插件包目录为：

```text
$BKCLAW_HOME/extensions/catalog/<catalog-id>/<version>/
```

现有 `$BKCLAW_HOME/plugins` 在迁移期间作为旧版内置目录读取，迁移完成后不再
作为系统级运行目录。

### 5.2 用户级

以下数据归属于用户：

- 安装记录
- 版本和包摘要
- 启用状态
- 配置与扩展密钥
- `alwaysOn`
- 网络策略
- 期望运行状态

### 5.3 Agent 级

用户安装扩展后，默认对该用户能访问的所有 Agent 可用。Agent 覆盖只保存例外：

```text
(userID, agentID, installationID) -> disabled
```

删除覆盖记录即恢复默认启用。Agent 覆盖不复制扩展配置或密钥。

### 5.4 公开或共享 Agent

扩展始终属于当前聊天用户，而不是 Agent 所有者：

- 用户调用自己的 Agent：使用该用户的扩展。
- 用户调用他人的公开或获授权 Agent：仍使用聊天用户自己的扩展。
- Agent 所有者的扩展和密钥不会自动暴露给其他聊天用户。
- 覆盖键仍为当前聊天用户和目标 Agent 的组合。

## 6. 数据模型

新增专用表，不将扩展运行模型塞入通用 `configs` JSON。

### 6.1 `extension_installations`

建议字段：

| 字段 | 含义 |
| --- | --- |
| `id` | 安装记录 UUID |
| `user_id` | 所属 BkClaw 用户 |
| `extension_id` | 用户空间内稳定的扩展标识 |
| `catalog_id` | 可选的目录来源标识 |
| `type` | `mcp-stdio`、`mcp-http`、`bkclaw-plugin`、`openclaw-plugin` |
| `name` | 展示名称 |
| `version` | 安装版本 |
| `package_digest` | 插件包 SHA-256；无包类型可为空 |
| `manifest_json` | 规范化部署描述和能力声明 |
| `config_json` | 非敏感用户配置 |
| `secret_json` | 扩展环境变量和敏感配置 |
| `enabled` | 用户期望是否启用 |
| `always_on` | 是否阻止用户容器空闲停止 |
| `network_policy` | 扩展期望的 `bridge` 或 `none`；实际按用户容器合并 |
| `revision` | 每次有效更新递增 |
| `created_at` | 创建时间 |
| `updated_at` | 更新时间 |

唯一约束：

```text
UNIQUE(user_id, extension_id)
```

`secret_json` 首期沿用 BkClaw 现有凭据存储安全级别，但所有 API 响应必须掩码。
后续可替换为 KMS 或应用层加密，不改变表的所有权模型。

### 6.2 `extension_agent_overrides`

建议字段：

| 字段 | 含义 |
| --- | --- |
| `id` | 覆盖记录 UUID |
| `user_id` | 当前聊天用户 |
| `agent_id` | 目标 Agent |
| `installation_id` | 用户安装记录 |
| `enabled` | 首期实际只持久化 `false` 例外 |
| `created_at` | 创建时间 |
| `updated_at` | 更新时间 |

唯一约束：

```text
UNIQUE(user_id, agent_id, installation_id)
```

写入前必须验证用户能访问目标 Agent，且安装记录属于该用户。

### 6.3 `extension_runtime_leases`

多副本 BkClaw 使用专用租约表选举每个用户唯一的插件 SSE event pump。

建议字段：

```text
user_id, holder_id, expires_at, updated_at
```

唯一约束为 `user_id`。租约只协调主动事件消费，不拥有扩展配置。

## 7. 统一扩展描述

BkClaw 内部使用规范化 `ExtensionSpec`：

```json
{
  "id": "echo-plugin",
  "type": "bkclaw-plugin",
  "command": "python3",
  "args": ["plugin.py"],
  "workingDir": ".",
  "env": {
    "API_KEY": "secret"
  },
  "capabilities": ["tool"],
  "restartPolicy": "on-failure",
  "networkPolicy": "bridge",
  "persistentData": true,
  "alwaysOn": false
}
```

旧 `plugin.json` 的命令字符串由 BkClaw 规范化为 `command + args`。Gateway
直接执行 argv，不经过 shell。无法安全解析或包含 shell 操作符的命令拒绝安装。

插件目录由 BkClaw 打包上传。首期不支持 Git URL、任意远程压缩包或上传 UI。

`networkPolicy` 是 BkClaw 安装模型中的期望值，仅用于
`ExtensionRuntimePool` 计算用户容器的有效网络模式，不作为单扩展字段发送给
Gateway。

## 8. ExtensionService

新增 `ExtensionService` 作为 BkClaw 内部唯一扩展入口，职责包括：

- 查询用户安装记录和 Agent 覆盖。
- 计算当前 `userID + agentID` 的有效扩展集合。
- 调用 `ExtensionRuntimePool.EnsureUserRuntime`。
- 将期望安装与 Gateway 实际状态做幂等 reconcile。
- 获取并缓存扩展能力描述。
- 路由 MCP、插件工具、Provider、Hook 和 Channel 调用。
- 消费插件主动事件并验证用户、Agent、会话和聊天归属。
- 更新最后使用时间和可观测状态。

调用方不得直接访问 Docker、Gateway 管理 API 或插件进程。

## 9. Agent 运行时装配

当前 MCP 与插件在 Agent 或系统 Gateway 启动时静态创建进程。迁移后：

- Agent 不持有用户专属扩展进程。
- UserSpace 使用当前聊天用户的 `ExtensionService` 视图。
- Agent 构建时注册动态扩展代理，不注册本地进程句柄。
- 每轮开始或 UserSpace 失效重载时，按 `userID + agentID` 刷新工具定义。
- 工具代理执行时再次校验安装、覆盖和 revision，避免使用过期授权。
- 插件 Hook 在触发点动态解析当前用户的有效插件。
- Provider 插件按当前用户动态路由，凭据不缓存在插件进程之外。
- Channel 插件由 `alwaysOn` reconcile 启动，并通过唯一 SSE event pump 接收事件。

能力定义缓存键为：

```text
installationID + revision
```

首次发现能力时可按需启动扩展。缓存命中后，工具列表生成不要求容器持续运行；
实际执行会重新确保 Runtime 和扩展进程在线。更新配置或版本必须递增 revision 并
使缓存失效。

## 10. MCP 路由

### 10.1 stdio MCP

```text
Agent tool proxy
-> ExtensionService
-> 用户 Extension Gateway
-> MCP Streamable HTTP endpoint
-> 容器内 stdio MCP
```

BkClaw 不再调用 `os/exec` 启动 stdio MCP。

### 10.2 HTTP MCP

HTTP MCP 改为用户级安装记录，并遵守相同 Agent 覆盖规则，但仍由 BkClaw 直接
访问远端服务：

```text
Agent tool proxy
-> ExtensionService
-> BkClaw HTTP MCP client
-> 远端 MCP
```

HTTP MCP 不创建 Extension Gateway 容器，也不影响其 idle 计时。

## 11. 插件路由

### 11.1 同步调用

```text
BkClaw
-> POST /extensions/{id}/rpc
-> Gateway
-> 插件 stdin
-> 插件 stdout response
-> HTTP response
```

适用于：

- `initialize`
- `shutdown`
- `tool.list`
- `tool.execute`
- `provider.list`
- `provider.execute`
- `hook.register`
- `hook.fire`
- `channel.send`

### 11.2 主动通知

```text
插件 stdout notification
-> Gateway 有界事件缓冲
-> GET /extensions/{id}/events
-> BkClaw event pump
-> 身份与归属校验
-> bus.Inbound / bus.Outbound
```

适用于：

- `message.inbound`
- `chat.send`

事件中的用户身份不由插件声明。用户身份来自 Runtime 容器和 Bearer Token。
插件提供的 `agentId`、`sessionId`、`chatId` 必须由 BkClaw 查询数据库验证。

## 12. ExtensionRuntimePool

`ExtensionRuntimePool` 按用户管理 Docker Runtime：

```go
type ExtensionRuntimePool interface {
    EnsureUserRuntime(ctx context.Context, userID string) (ExtensionRuntime, error)
    ReconcileUser(ctx context.Context, userID string) error
    Touch(userID string)
    Status(ctx context.Context, userID string) (RuntimeStatus, error)
    StopIdle(ctx context.Context) error
    RemoveUser(ctx context.Context, userID string, removeVolume bool) error
    Close() error
}
```

首期提供两个后端：

- `docker`：生产隔离模式。
- `local`：显式开发兼容模式。

配置规则：

```text
BKCLAW_EXTENSION_RUNTIME_MODE=docker|local
```

不提供 `auto`。`docker` 模式失败时明确报错，不回退本地执行。

## 13. Docker 生命周期

- 容器名和卷名使用 `userID` 的稳定哈希，不暴露原始用户标识。
- 首次需要 stdio MCP/插件或安装 `alwaysOn` 扩展时懒创建容器。
- 容器端口由 Docker 动态发布，BkClaw 通过 inspect 获取。
- 容器停止时保留容器和卷。
- 普通容器空闲 10 分钟后停止。
- 存在启用的 `alwaysOn` 扩展时不参与空闲回收。
- Channel 插件默认 `alwaysOn=true`，其他类型默认 `false`。
- 下次调用自动启动容器并执行完整 reconcile。
- 用户删除时移除容器和卷；管理员可显式执行同样清理。

多副本 BkClaw 必须连接同一个 Docker daemon。每个用户只有租约持有者维持插件
SSE event pump，其他副本仍可发起同步 RPC。

首期网络隔离边界是用户容器，不是单个扩展。只要同一用户有一个已启用的
Gateway 扩展要求 `bridge`，该用户容器的有效网络策略就是 `bridge`；只有全部
已启用 Gateway 扩展均为 `none` 时才能使用 `none`。策略变化需要重建容器但保留
用户卷。管理 API 必须展示扩展期望策略和用户容器有效策略，不能声称同一容器内
的扩展具有彼此独立的网络隔离。

## 14. 容器安全

BkClaw 是唯一容器控制方。Gateway 不获得 Docker socket。

每个用户容器默认使用：

- 独立持久卷挂载到 `/data`
- `--cap-drop ALL`
- `no-new-privileges`
- 只读根文件系统
- 独立 `/tmp` tmpfs
- 将 `HOME` 和包管理器缓存目录指向 `/data/runtime` 下的用户专属路径
- CPU、内存和 PID 限制
- Docker bridge 网络，或显式 `none`
- 不挂载 BkClaw 数据目录、数据库文件或对象存储目录

容器仅获得：

- 由运维主密钥和 userID 派生的 Bearer Token
- Gateway 监听地址和数据目录配置
- 必要的代理环境变量

扩展子进程仅获得最小基础环境和该安装记录白名单中的 env，不继承 Gateway 或
BkClaw 环境。

## 15. 鉴权与事件代际

Gateway API 全部使用每用户 Bearer Token。Token 由 BkClaw 使用运维主密钥和
userID 派生，不额外保存随机明文 Token。

Gateway 每次启动生成新的 `runtimeEpoch`。SSE 事件包含：

```json
{
  "extensionId": "echo-plugin",
  "runtimeEpoch": "opaque-epoch",
  "eventId": 42,
  "message": {
    "jsonrpc": "2.0",
    "method": "chat.send",
    "params": {}
  }
}
```

BkClaw 拒绝旧 epoch、未知安装、错误用户和归属校验失败的事件，并记录审计日志。

## 16. 管理 API

BkClaw 对外提供用户级 API：

```text
GET    /api/extensions/catalog
GET    /api/extensions
POST   /api/extensions
PATCH  /api/extensions/{id}
DELETE /api/extensions/{id}
PUT    /api/extensions/{id}/agents/{agentId}
GET    /api/extensions/runtime
POST   /api/extensions/runtime/reconcile
```

语义：

- `POST` 创建用户安装，不代表容器必须持续运行。
- `PATCH` 更新配置、密钥、启用状态或 `alwaysOn`，并递增 revision。
- `DELETE` 先请求停止扩展，再删除安装记录；停止失败时保留记录并返回可重试错误。
- Agent 覆盖接口写入显式禁用或删除覆盖。
- Runtime 状态明确区分 `desired` 与 `runtime`。
- 密钥始终掩码返回；掩码值回写表示保留旧值。
- 安装、更新和删除后使当前 UserSpace 与能力缓存失效。

## 17. Reconcile

BkClaw 数据库是期望状态的唯一事实源。Gateway 状态可随时重建。

每次以下事件触发幂等 reconcile：

- 用户容器首次创建
- 已停止容器恢复
- 安装、更新、启用、禁用或删除扩展
- BkClaw 启动时恢复 `alwaysOn` 用户
- 管理员手动请求 reconcile
- Gateway 返回状态丢失或 epoch 改变

reconcile 比较 `extension_id + revision + package_digest`，执行部署、更新、启动、
停止或删除。单个扩展失败不阻断同一用户的其他扩展。

## 18. 迁移

数据库迁移必须幂等，并使用迁移版本或等价标记保证只执行一次数据搬迁。

迁移规则：

- 系统目录中的插件继续作为内置扩展目录。
- 当前系统级已启用插件迁移为 `super_admin` 的用户安装。
- 当前系统级 stdio MCP 迁移为 `super_admin` 的用户安装。
- 当前系统级 HTTP MCP 迁移为 `super_admin` 的用户级 HTTP MCP 安装。
- 现有 Agent 插件开关迁移为 `super_admin` 对对应 Agent 的覆盖记录。
- 不自动给所有普通用户安装系统扩展。
- 普通用户之后自行安装，或由管理员代用户安装。
- 数据迁移成功后，系统 Gateway 启动不再调用插件 `StartAll`，Agent 构建不再
  启动 stdio MCP。

迁移必须保留旧配置直到新记录完整写入；迁移失败时不得启用新的运行路径。

## 19. 错误处理

- Runtime 未配置：返回明确配置错误。
- Docker、Gateway 或扩展启动失败：返回隔离运行时错误，不本地回退。
- 扩展调用超时：取消 HTTP 请求，Gateway 负责清理 pending RPC。
- 扩展崩溃：按重启策略处理，并更新状态；不使 BkClaw 进程崩溃。
- SSE 断线：使用 `Last-Event-ID` 重连。
- 事件缓冲溢出：记录丢失计数，断开消费者并要求重新同步。
- revision 冲突：重新读取安装记录并 reconcile。
- 单个扩展失败：不影响同一用户的其他扩展。

## 20. 测试与验收

### 20.1 BkClaw 单元和集成测试

- 专用表在 MySQL、PostgreSQL 和 SQLite 测试路径上幂等迁移。
- 安装记录严格按 userID 隔离。
- Agent 覆盖默认启用、显式禁用和删除恢复。
- 公开 Agent 使用聊天用户扩展，不使用 Agent 所有者扩展。
- 密钥 API 掩码和掩码回写保留。
- revision 更新使能力缓存失效。
- stdio MCP、插件、Hook、Provider 和 Channel 均通过 ExtensionService 路由。
- HTTP MCP 使用用户级配置但不启动 Docker Runtime。
- `docker` 模式失败不回退；`local` 模式显式可用。

### 20.2 Docker 生命周期测试

- 不同用户创建不同容器和卷。
- 同一用户并发 Ensure 只创建一个容器。
- 普通容器空闲 10 分钟停止并可恢复。
- `alwaysOn` 阻止空闲停止。
- 恢复后自动 reconcile。
- 多副本租约只产生一个 SSE event pump。
- 用户删除清理容器和卷。

### 20.3 端到端验收

- stdio MCP 不在 BkClaw 容器运行。
- 原生插件和 OpenClaw bridge 不在 BkClaw 容器运行。
- 同一用户不同会话共享扩展进程。
- 不同用户无法读取彼此扩展数据。
- 扩展无法读取 BkClaw 数据库和对象存储凭据。
- `message.inbound` 和 `chat.send` 能到达正确用户、Agent 和会话。
- Extension Gateway 异常不导致 BkClaw 崩溃。
- 内置工具 Session Sandbox 保持兼容。

## 21. 实施分解

本设计应拆为两个按顺序执行、各自可测试的实施计划：

1. **用户级扩展模型**
   - 专用表、Store、管理 API、目录、迁移、Agent 覆盖和动态 ExtensionService。
   - 先以 `local` Runtime 验证所有权与动态路由。

2. **隔离运行时**
   - 扩展 mcp-gateway、实现 Docker ExtensionRuntimePool、SSE event pump、
     reconcile、安全配置和端到端测试。

第二阶段不得绕过第一阶段直接把现有系统级插件进程搬入单个容器。
