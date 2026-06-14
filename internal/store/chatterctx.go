package store

import "context"

// chatterUserIDCtxKey 使用已解析的每轮聊天者用户ID标记ctx，
// 以便DBStore写入操作（AppendSessionMessage / SaveSession /
// AppendSessionEvent）可以持久化该ID，而无需更改每个调用点的
// 签名。代理循环在HandleMessage / HandleMessageStream顶部设置此值；
// 下游所有操作仅需传播ctx。
type chatterUserIDCtxKey struct{}

// WithChatterUserID 返回标记了每轮聊天者用户ID的ctx。
// 与config.WithUserID（由中间件解析的已认证用户）和sandbox.WithUserID
// （执行器挂载目标）不同——当IM通道将每个发送者的app_user路由到
// 通道拥有者的UserSpace时，这两个ID会携带不同的值。
// 空的uid为无操作，因此调用者无需进行防护检查。
func WithChatterUserID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, chatterUserIDCtxKey{}, uid)
}

// ChatterUserIDFromContext 返回由WithChatterUserID设置的聊天者用户ID，
// 如果没有则返回""。存储实现应将结果合并到会话写入的chatter_user_id列中；
// 空值（后台ctx，未标记的代码路径）写入''，读取时在查询中回退到user_id。
func ChatterUserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(chatterUserIDCtxKey{}).(string)
	return v
}
