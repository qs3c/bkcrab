# OpenIM 接入

当前支持 OpenIM 单聊文字消息（101）、@文字消息（106）和完整文字回复。群聊必须在 bkcrab 的允许列表中，并明确 @ 对应机器人；@全体不触发。OpenIM Server、SDK、客户端无需修改源码。

## 连接前准备

1. 在 OpenIM 注册一个普通用户，例如 `bkcrab_assistant`，设置昵称和头像。管理员账号与机器人账号分开。
2. 用户按当前 OpenIM 好友策略添加机器人；需要群聊时，将机器人加入对应群。连接操作只验证已有账号，不会自动注册、加好友或入群。
3. 准备 OpenIM API 地址、管理员 userID、服务端 secret、机器人 userID，以及可选群 ID 列表。机器人、聊天用户和群 ID 当前限制为 64 字节以内。
4. 在 bkcrab 的「智能体 → 渠道 → OpenIM」填写信息，点击「验证并连接」。凭据会用于 `/auth/get_admin_token` 和 `/user/get_users_info` 验证，保存后热加载，无需重启 bkcrab。

同一个用户在同一个智能体上只能绑定一个 OpenIM 机器人，与现有 channel 配置表约束一致。需要替换时先断开；同一 OpenIM 实例可在不同智能体上绑定多个机器人。所有绑定应使用完全一致的 API 基地址和管理员凭据。不同 DNS 别名会被视作不同实例。

## 配置 OpenIM 回调

连接成功后，界面会给出专用回调基础 URL 和 YAML 配置。将它们合并到 OpenIM `config/webhooks.yml`，例如：

```yaml
url: "http://bkcrab:18790/api/openim/webhook/INSTANCE_ID/WEBHOOK_SECRET"
afterSendSingleMsg:
  enable: true
  timeout: 5
  attentionIds: ["bkcrab_assistant"]
afterSendGroupMsg:
  enable: true
  timeout: 5
  attentionIds: ["your_group_id"]
```

示例端口仅为说明，应换成实际 bkcrab HTTP 监听端口。使用连接界面生成的 INSTANCE_ID 和 WEBHOOK_SECRET；不要把 OpenIM 管理员 secret 直接写进 URL。

OpenIM 会自行在基础 URL 后追加 `callbackAfterSendSingleMsgCommand` 或 `callbackAfterSendGroupMsgCommand`，不要在 `url` 中提前追加命令。只有单聊需求时，将 `afterSendGroupMsg.enable` 保持为 false。

同一 OpenIM 实例只有一个回调基础地址，多机器人共用它。将所有机器人的 userID 合并进单聊 attentionIds，将目标群合并进群聊 attentionIds；bkcrab 内部仍按接收机器人、群允许列表和 @ 列表过滤。OpenIM 已有其他业务回调时，不能直接覆盖其地址，应在现有业务回调服务或反向代理按回调命令分发到 bkcrab，其余回调继续交给原服务。

配置需要按实际 OpenIM Docker/Compose 部署方式重新加载相关服务。本次代码实现不包含对运行中 OpenIM 或 bkcrab 的重启。

## Docker 网络与凭据

- bkcrab → OpenIM：填写容器可达的 API 地址，而不是宿主机视角的 localhost。
- OpenIM → bkcrab：同一 Docker 网络可用服务名和容器监听端口；跨服务器使用可达地址或反向代理。Tailscale 可用于宿主机互通，但仍需确认容器出站能访问该地址。
- 反向代理须保留完整 webhook 路径。跨非可信网络使用 HTTPS；回调 URL 是访问凭据，应限制传播并对访问日志中的密钥路径脱敏。
- 管理员 secret 持久化于服务端 channel 配置，管理列表不返回它。它用于获取可代表机器人发送消息的管理员 token，不能下发给 IM 客户端。
- token 在后端缓存并提前刷新。明确收到 OpenIM 1501/1502 token 拒绝时刷新并重试一次；超时或其他不确定发送失败不自动重发，以免重复消息。
- 同实例绑定采用一致的管理员凭据。轮换管理员 secret 时，先断开该实例全部绑定，再用新凭据连接并更新 OpenIM webhook URL。

## 入站可靠性与范围

新迁移会创建 `channel_inbox`。通过鉴权及路由筛选的文字消息先持久化，再返回 OpenIM 成功响应；worker 领取消息后交给现有 message bus。入站 ID 在共享数据库中去重，未完成领取可在 30 秒后恢复，已投递记录的消息正文立即清除，去重记录保留七天并定期清理。

入站队列按实例、机器人、绑定用户和智能体隔离。断开绑定会停止其 worker；同一绑定重新连接可继续处理尚未投递的消息，更换所有者或智能体不会重放前一绑定的队列。已断开且不再恢复的绑定可能留下待处理记录，应按实际保留策略清理。

这里的「已投递」指交给 bkcrab 内存总线，不表示模型处理完成或用户已收到回复。进程在总线交接到 Agent 执行期间崩溃仍可能丢失工作，交接成功但数据库标记失败则可能重放；不提供端到端 exactly-once 保证。当前 OpenIM 后置回调也不能当作持久化可靠重试队列，回调未到达 bkcrab 的情况需要历史补拉或独立可靠事件机制。本版没有实现历史补拉。

不接收图片、语音、文件等非文字输入；不提供流式输出、消息编辑、互动审批按钮、自动 SSO 绑定。文字中的 Markdown 不保证由普通 OpenIM 客户端渲染。输出附件会给出网页查看提示，URL 按钮转为文本链接；账号与聊天者的权限/记忆归属沿用 bkcrab 当前 channel 语义。

## 联调验收

1. 单聊机器人发送文字，确认收到一次完整回复。
2. 在允许群中 @机器人，确认回复回到原群；不 @、@全体、未允许群均不触发。
3. 连续发送相同文字但不同消息 ID，应分别进入处理；重放同一个回调 ID，不应重复进入。
4. 同实例两个机器人分别绑定不同智能体，核对单聊及群聊 @ 的目标和发送账号。
5. 使用错误 webhook 密钥，应返回 401；机器人自己的消息和已绑定机器人之间的消息应被忽略。
6. 在入站数据已保存、worker 尚未完成交接时重启 bkcrab，验证待处理记录能够恢复。不要据此推断模型执行阶段的崩溃也已覆盖。
7. 本地自动化检查：`go test ./internal/channels ./internal/store ./internal/gateway ./internal/setup`；在 `web` 运行 `npx tsc --noEmit`。

核对过的 OpenIM Server 源码版本：`175a7bb0673eca18e9d1b10bff4f728da6b1b513`。

2026-09-30 已在 OpenIM Server `v3.8.3-patch.12`、Chat `v1.8.4-patch.2` 和 MySQL 存储的 bkcrab 上完成单聊联调：真实后置回调进入 bkcrab，智能体生成回复，OpenIM WebSocket 收到对应回复；错误回调密钥返回 401。运行时 `cmd/bkcrab` 的 `apiResolver` 必须转发 `DispatchOpenIMWebhook`，仅在 Gateway 实现该方法会使 HTTP 入口返回 503。此联调未覆盖群聊、多机器人及故障恢复验收项。
