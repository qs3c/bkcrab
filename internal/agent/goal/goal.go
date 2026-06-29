// 包目标实现持久线程目标 - `/goal <objective>`
// 成为一个长期运行的、审计驱动的循环，运行时保持
// 注入继续提示，直到模型标记目标
// 完成、代币预算用完、或者用户暂停或清除
// 它。
//
// 该设计以 OpenAI Codex CLI 的 /goal (codex-rs/core/src/
// 目标.rs）。请参阅 docs/design/goal.md 了解基本原理。
package goal

import "github.com/qs3c/bkcrab/internal/store"

// 目标是活动或已完成目标的持久记录。一球
// per (agent, session) — 由基础上的 UNIQUE 索引强制执行
// 桌子。域类型是store.GoalRecord的别名；没有
// 单独的一组字段以保持同步。
type Goal = store.GoalRecord

// 状态是目标的生命周期状态，别名为纯字符串，因此
// Goal 上的字段（= store.GoalRecord）直接携带它。四个价值观；
// “unmet”是故意缺席的——一个无法简单完成的目标
// 保持活动状态，直到用户暂停或清除它。
type Status = string

const (
	StatusActive        Status = "active"
	StatusPaused        Status = "paused"
	StatusBudgetLimited Status = "budget_limited"
	StatusComplete      Status = "complete"
)

// RemainingTokens 返回预算 - 已使用 (≥0)。当目标没有的时候
// 预算，好吧是假的。仅由提示渲染器使用。
func RemainingTokens(g *Goal) (remaining int64, ok bool) {
	if g.TokenBudget == nil {
		return 0, false
	}
	return max(0, *g.TokenBudget-g.TokensUsed), true
}
