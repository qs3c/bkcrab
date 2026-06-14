package goal

// FoldUsage 将一个模型调用的令牌计数就地应用到目标。
// 返回添加到 TokensUsed 的增量以及目标是否刚刚
// 超出了预算。
//
// 跳过非活动目标（暂停/预算有限/完成不
// 收取费用）。缓存的输入被排除——调用者应该
// 传递未缓存的输入令牌（提供者适配器已经剥离
// 在此点之前缓存命中）。
//
// 改变 g 而不是返回副本，因为调用者坚持
// 他们折叠到同一个记录中。
func FoldUsage(g *Goal, inputTokens, outputTokens int64) (delta int64, exhausted bool) {
	if g == nil || g.Status != StatusActive {
		return 0, false
	}
	delta = max(0, inputTokens) + max(0, outputTokens)
	if delta == 0 {
		return 0, false
	}
	g.TokensUsed += delta
	if g.TokenBudget != nil && g.TokensUsed >= *g.TokenBudget {
		g.Status = StatusBudgetLimited
		exhausted = true
	}
	return delta, exhausted
}
