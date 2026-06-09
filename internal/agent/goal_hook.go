package agent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/fastclaw-ai/fastclaw/internal/agent/goal"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// NewTokenAccountingHook returns an AfterModelCall hook that folds
// the call's Usage into the active goal for the in-flight session
// and persists the result. Returns nil when st is nil — callers can
// register the result unconditionally without a guard.
//
// Gates before doing any work:
//   - HookContext.GoalSessionKey must be non-empty (turn happened
//     inside a chat context)
//   - HookContext.Error must be nil (a failed call has no usage
//     worth folding, even when the provider helpfully returns one)
//   - Response.Usage must have at least one non-zero count (zero
//     value means the provider didn't report)
//
// Past those gates the call routes through goal.FoldUsage and then
// st.UpdateGoal. Errors are logged at warn and swallowed — a store
// failure should not leak into the agent's response path; the next
// call will see the same delta and retry.
//
// When the fold flips a goal to BudgetLimited, this hook publishes
// the budget_limit prompt directly. The transition is observed
// exactly once: FoldUsage's "non-active goals are skipped" gate
// prevents the next call from re-publishing.
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
		// Treat the zero-value Usage as "provider didn't report" — same
		// as the old nil check before provider.Usage became a value
		// type. Budget enforcement is only meaningful when we have at
		// least one non-zero count.
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
			// Continuation turns for budget_limited / complete goals
			// still fire AfterModelCall; FoldUsage's own gate would
			// reject them anyway, but skipping here saves a store
			// round-trip and keeps the log line below honest.
			return
		}

		delta, exhausted := goal.FoldUsage(g, int64(u.InputTokens), int64(u.OutputTokens))
		if delta == 0 && !exhausted {
			// Nothing changed (e.g. all-cached prompt). Skip the
			// persist round-trip — we'd just rewrite the same row.
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
