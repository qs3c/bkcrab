package sandbox

import "context"

// userIDCtxKey 通过 ctx 将当前聊天者的 userID 传递到沙箱创建路径。
// 通过上下文传递（而不是扩宽 ExecutorPool.Get 的签名）
// 保持现有的 4 元组池键不变，并让不了解聊天者的站点
//（cron 刷新、管理员重新加载触发器）继续以现有方式调用 Get()——
// 它们只是不会获得按用户挂载，这是正确的回退行为。
type userIDCtxKey struct{}

// WithUserID 返回标记了聊天者 userID 的 ctx。空 uid 是空操作
//（返回未更改的 ctx），因此调用站点在包装前无需进行 nil 检查。
func WithUserID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, userIDCtxKey{}, uid)
}

// UserIDFromContext 提取由 WithUserID 设置的聊天者 userID，
// 如果没有包装则返回 ""。沙箱后端使用空值情况完全跳过
// 按用户技能绑定挂载。
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}
