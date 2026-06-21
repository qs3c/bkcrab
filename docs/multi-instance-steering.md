# Multi-instance Steering

## 背景

单实例时，agent loop 通过 `Session.turnDepth` 判断某个 session 是否有正在运行的 turn，并把飞行中到达的用户消息放进 `Session.steerBuf`。这在一个进程内成立，但多 pod 部署时请求可能打到任意实例，非 owner pod 看不到 owner pod 内存里的 `turnDepth/steerBuf`。

因此多实例不能只把 `turnDepth` 复制到 Redis。真正需要共享的是一组 turn 协调语义：

- 当前 `(user, agent, session)` 是否有 active turn。
- 哪个实例拥有这个 turn。
- steer 消息可以从任意实例写入。
- owner 实例可以在 loop 间隙 drain steer。
- 结束 turn 时有 fencing token，避免误删新 owner 的 lease。

## 方案

新增 `session.TurnCoordinator` 接口：

- `BeginTurn(ctx, key)`：尝试获取当前 turn 的 lease。
- `PushSteer(ctx, key, msg)`：仅 active turn 存在时写入 steer。
- `DrainSteer(ctx, lease)`：owner 读取并推进 steer offset。
- `EndTurn(ctx, lease)`：最终 drain 并释放 lease。

默认实现是 `LocalTurnCoordinator`，行为等价于旧的 `turnDepth + steerBuf`。设置 `BKCLAW_REDIS_ADDR` 后，Gateway 会启用 `RedisTurnCoordinator`。

Redis key 设计：

- `bkclaw:turn:<user>:<agent>:<session>`：active turn lease，value 包含 `ownerId / turnId / token`，带 TTL。
- `bkclaw:steer:<user>:<agent>:<session>:<turnId>`：Redis Stream，保存飞行中 steer 消息。

`BeginTurn` 使用 `SET NX PX` 抢 lease。`PushSteer` 先读 active lease，再 `XADD` 到对应 turn 的 stream。owner loop 在工具迭代之间用 `XRANGE` 增量读取。`EndTurn` 会用完整 lease value 做 fencing 校验后删除 turn key，并给 stream 设置过期时间。

## 请求流

1. 正常 `HandleMessage` 进入后先构造用户消息并调用 `BeginTurn`。
2. 如果抢到 lease，继续执行该 turn。
3. 如果没有抢到 lease，说明已有 active turn；当前用户消息会被写入 steer stream，当前请求发出 `turn_pending/done` 后结束。
4. `/api/chat/steer` 和 IM `trySteer` 都调用 `PushSteer`，因此请求打到任意 pod 都能进入 owner turn。
5. owner turn 在模型无工具返回前、工具轮结束后、turn 结束时 drain steer。

## 配置

本地/单实例不需要 Redis。多实例启用：

```bash
BKCLAW_REDIS_ADDR=redis:6379
BKCLAW_REDIS_PASSWORD=
BKCLAW_REDIS_DB=0
BKCLAW_REDIS_KEY_PREFIX=bkclaw
BKCLAW_TURN_LEASE_TTL_SECONDS=7200
BKCLAW_STEER_STREAM_TTL_SECONDS=86400
```

`BKCLAW_TURN_OWNER_ID` 可选；未设置时使用 Gateway 启动时生成的 holder id。

## 当前边界

这个改造解决 active turn 和 steer buffer 的跨 pod 可见性。`EventHub` 仍是进程内 SSE hub，因此用户已有的 SSE 连接仍应通过 sticky session 留在原 pod；Helm Service 里已有 `sessionAffinity: ClientIP`。后续如果要做到 SSE 完全跨 pod，需要再引入 Redis pub/sub 或独立事件总线。
