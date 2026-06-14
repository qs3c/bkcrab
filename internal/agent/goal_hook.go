package agent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/qs3c/bkclaw/internal/agent/goal"
	"github.com/qs3c/bkclaw/internal/bus"
)

// NewTokenAccountingHook 返回一个 AfterModelCall 钩子，将调用的
// Usage 计入进行中会话的活跃目标并持久化结果。当 st 为 nil 时返回 nil
// ——调用者可以无保护地无条件注册结果。
//
// 执行任何工作前的门控：
//   - HookContext.GoalSessionKey 必须非空（轮次发生在聊天上下文中）
//   - HookContext.Error 必须为 nil（即使提供者有用地返回了一个，
//     失败的调用也没有值得归集的用量）
//   - Response.Usage 必须至少有一个非零计数（零值表示提供者未报告）
//
// 通过这些门控后，调用路由到 goal.FoldUsage 然后 st.UpdateGoal。
// 错误以 warn 级别记录并被吞没——存储失败不应泄漏到代理的响应路径中；
// 下一次调用会看到相同的 delta 并重试。
//
// 当归集将目标翻转为 BudgetLimited 时，此钩子直接发布 budget_limit
// 提示。该转换被精确观察一次：FoldUsage 的"非活跃目标被跳过"门控
// 阻止下一次调用重新发布。
func NewTokenAccountingHook(st goal.Store, mb *bus.MessageBus, agentID string) HookFunc {
	if st == nil {
		return nil
	}
	return func(ctx context.Context, hc *HookContext) {
		if hc.Point != AfterModelCall {
			return
		}
		if hc.Error != nil {
			return
		}
		if hc.GoalSessionKey == "" {
			return
		}
		if hc.Response == nil {
			return
		}
		// 将零值 Usage 视为"提供者未报告"——与之前的 nil 检查相同。
		// 预算强制执行仅在至少有一个非零计数时才有意义。
		u := hc.Response.Usage
		if u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadTokens == 0 && u.CacheCreationTokens == 0 {
			return
		}

		g, err := st.GetGoalBySession(ctx, agentID, hc.GoalSessionKey)
		if errors.Is(err, goal.ErrNotFound) {
			return
		}
		if err != nil {
			slog.Warn("goal accounting: load goal failed",
				"agent", agentID, "session_key", hc.GoalSessionKey, "error", err)
			return
		}
		if g.Status != goal.StatusActive {
			// budget_limited / complete 目标的延续轮次仍然会触发
			// AfterModelCall；FoldUsage 自身的门控本就会拒绝它们，
			// 但在此处跳过可节省一次存储往返并保持下面的日志行诚实。
			return
		}

		delta, exhausted := goal.FoldUsage(g, int64(u.InputTokens), int64(u.OutputTokens))
		if delta == 0 && !exhausted {
			// 没有任何变化（例如全缓存提示）。跳过持久化往返
			// ——我们只会重写同一行。
			return
		}

		if err := st.UpdateGoal(ctx, g); err != nil {
			slog.Warn("goal accounting: persist failed",
				"agent", agentID, "session_key", hc.GoalSessionKey,
				"delta", delta, "exhausted", exhausted, "error", err)
			return
		}
		if exhausted {
			slog.Info("goal budget exhausted",
				"agent", agentID, "session_key", hc.GoalSessionKey,
				"tokens_used", g.TokensUsed, "token_budget", *g.TokenBudget)
			prompt := goal.BudgetLimitPrompt(g)
			if !goal.Publish(mb, g, prompt) {
				slog.Warn("goal accounting: bus full, budget_limit prompt dropped",
					"agent", agentID, "session_key", hc.GoalSessionKey)
			}
		}
	}
}
