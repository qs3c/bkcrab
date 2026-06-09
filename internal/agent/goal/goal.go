// Package goal implements persisted thread goals — a `/goal <objective>`
// becomes a long-running, audit-driven loop where the runtime keeps
// injecting continuation prompts until the model marks the goal
// complete, the token budget runs out, or the user pauses or clears
// it.
//
// The design is modeled on OpenAI Codex CLI's /goal (codex-rs/core/src/
// goals.rs). See docs/design/goal.md for the rationale.
package goal

import "github.com/bkclaw-ai/bkclaw/internal/store"

// Goal is the persisted record of an active or finished goal. One goal
// per (agent, session) — enforced by a UNIQUE index on the underlying
// table. The domain type is an alias to store.GoalRecord; there's no
// separate set of fields to keep in sync.
type Goal = store.GoalRecord

// Status is the lifecycle state of a goal, aliased to plain string so
// fields on Goal (= store.GoalRecord) carry it directly. Four values;
// "unmet" is intentionally absent — a goal that cannot complete simply
// stays Active until the user pauses or clears it.
type Status = string

const (
	StatusActive        Status = "active"
	StatusPaused        Status = "paused"
	StatusBudgetLimited Status = "budget_limited"
	StatusComplete      Status = "complete"
)

// RemainingTokens returns budget − used (≥0). When the goal has no
// budget, ok is false. Used only by the prompt renderer.
func RemainingTokens(g *Goal) (remaining int64, ok bool) {
	if g.TokenBudget == nil {
		return 0, false
	}
	return max(0, *g.TokenBudget-g.TokensUsed), true
}
