package goal

import "testing"

func TestFoldUsageActiveGoalAccumulates(t *testing.T) {
	budget := int64(1_000_000)
	g := &Goal{
		Status:      StatusActive,
		TokenBudget: &budget,
		TokensUsed:  100,
	}
	// Caller passes already-uncached input + output; cache hits don't
	// enter the calc.
	delta, exhausted := FoldUsage(g, 200, 30)
	if delta != 230 {
		t.Errorf("delta = %d, want 230", delta)
	}
	if exhausted {
		t.Errorf("not exhausted (used %d, budget %d)", g.TokensUsed, *g.TokenBudget)
	}
	if g.TokensUsed != 330 {
		t.Errorf("TokensUsed = %d, want 330 (100 + 230)", g.TokensUsed)
	}
}

func TestFoldUsageNonActiveSkipped(t *testing.T) {
	// Continuation turns fire AfterModelCall too; FoldUsage must not
	// keep billing tokens against a budget_limited or complete goal.
	for _, s := range []Status{StatusPaused, StatusBudgetLimited, StatusComplete} {
		g := &Goal{Status: s, TokensUsed: 100}
		delta, _ := FoldUsage(g, 500, 100)
		if delta != 0 {
			t.Errorf("status=%s: delta = %d, want 0 (skipped)", s, delta)
		}
		if g.TokensUsed != 100 {
			t.Errorf("status=%s: TokensUsed mutated from 100 to %d", s, g.TokensUsed)
		}
	}
}

func TestFoldUsageNilSafe(t *testing.T) {
	// nil goal must be a no-op rather than a panic.
	if delta, _ := FoldUsage(nil, 10, 0); delta != 0 {
		t.Errorf("nil goal: delta = %d, want 0", delta)
	}
}

func TestFoldUsageZeroDeltaSkipsBookkeeping(t *testing.T) {
	// An all-cached prompt-only call has uncached input = 0 and output
	// = 0. TokensUsed shouldn't move.
	g := &Goal{Status: StatusActive, TokensUsed: 50}
	delta, _ := FoldUsage(g, 0, 0)
	if delta != 0 {
		t.Errorf("delta = %d, want 0 (all input cached, no output)", delta)
	}
	if g.TokensUsed != 50 {
		t.Errorf("zero-delta should not bump TokensUsed (got %d)", g.TokensUsed)
	}
}

func TestFoldUsageFlipsToBudgetLimitedAtCap(t *testing.T) {
	budget := int64(100)
	g := &Goal{Status: StatusActive, TokenBudget: &budget, TokensUsed: 90}
	_, exhausted := FoldUsage(g, 5, 7)
	// delta=12 → used=102 ≥ 100 → exhausted
	if !exhausted {
		t.Fatal("expected exhausted=true")
	}
	if g.Status != StatusBudgetLimited {
		t.Errorf("status = %q, want budget_limited", g.Status)
	}
	if g.TokensUsed != 102 {
		t.Errorf("TokensUsed = %d, want 102 (90 + 12)", g.TokensUsed)
	}
}

func TestFoldUsageUnboundedNeverExhausts(t *testing.T) {
	g := &Goal{Status: StatusActive, TokensUsed: 1_000_000} // TokenBudget intentionally nil
	_, exhausted := FoldUsage(g, 0, 999_999)
	if exhausted {
		t.Fatal("unbounded goal should never report exhausted")
	}
	if g.Status != StatusActive {
		t.Errorf("status = %q, want active (unbounded)", g.Status)
	}
}

func TestFoldUsageExactBoundaryExhausts(t *testing.T) {
	// "tokens_used >= token_budget" — equality also exhausts.
	budget := int64(100)
	g := &Goal{Status: StatusActive, TokenBudget: &budget, TokensUsed: 95}
	_, exhausted := FoldUsage(g, 0, 5)
	if !exhausted {
		t.Errorf("used=%d, budget=%d should exhaust", g.TokensUsed, *g.TokenBudget)
	}
}

// TestFoldUsageClampsNegativeInputs pins the defensive clamp: a buggy
// provider adapter (or a future cumulative-style refactor that
// subtracts a stale baseline) could yield a negative input or output
// count. FoldUsage must treat that as zero rather than DECREMENT the
// goal's TokensUsed and silently extend the budget.
func TestFoldUsageClampsNegativeInputs(t *testing.T) {
	budget := int64(1000)
	g := &Goal{Status: StatusActive, TokenBudget: &budget, TokensUsed: 500}
	delta, exhausted := FoldUsage(g, -100, -50)
	if delta != 0 {
		t.Errorf("delta = %d, want 0 (negatives clamped)", delta)
	}
	if exhausted {
		t.Error("clamped-zero delta cannot exhaust a budget")
	}
	if g.TokensUsed != 500 {
		t.Errorf("TokensUsed mutated from 500 to %d on negative inputs", g.TokensUsed)
	}
}
