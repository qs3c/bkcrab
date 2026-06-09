package store

import (
	"context"
	"errors"
	"testing"
)

func TestGoalSQLiteRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	budget := int64(200_000)

	g := &GoalRecord{
		ID:          "goal-1",
		AgentID:     "agent-A",
		SessionKey:  "s-12345-abcdef",
		OwnerUserID: "user-1",
		Objective:   "translate README to English",
		Status:      "active",
		TokenBudget: &budget,
	}

	if err := db.CreateGoal(ctx, g); err != nil {
		t.Fatalf("create: %v", err)
	}

	// UNIQUE (agent_id, session_key) must reject a second goal on the
	// same pair — this is the contract slash and REST handlers rely on
	// to give users a clean "clear the existing goal first" error.
	dup := *g
	dup.ID = "goal-1-dup"
	if err := db.CreateGoal(ctx, &dup); !errors.Is(err, ErrGoalAlreadyExists) {
		t.Fatalf("expected ErrGoalAlreadyExists, got %v", err)
	}

	got, err := db.GetGoalBySession(ctx, "agent-A", "s-12345-abcdef")
	if err != nil {
		t.Fatalf("get by session: %v", err)
	}
	if got.Objective != "translate README to English" {
		t.Errorf("objective round-trip mismatch: %q", got.Objective)
	}
	if got.TokenBudget == nil || *got.TokenBudget != 200_000 {
		t.Errorf("token budget round-trip mismatch: %v", got.TokenBudget)
	}

	// Update via UpdateGoal — exercises the "tokens accumulated, status
	// flipped" path that the continuation hook uses every turn.
	got.TokensUsed = 50_000
	got.Status = "budget_limited"
	if err := db.UpdateGoal(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, err := db.GetGoalBySession(ctx, "agent-A", "s-12345-abcdef")
	if err != nil {
		t.Fatalf("re-get: %v", err)
	}
	if again.TokensUsed != 50_000 || again.Status != "budget_limited" {
		t.Errorf("update didn't round-trip: %+v", again)
	}

	if err := db.DeleteGoal(ctx, got.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetGoalBySession(ctx, "agent-A", "s-12345-abcdef"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestGoalUnboundedBudget(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	g := &GoalRecord{
		ID:          "goal-2",
		AgentID:     "agent-B",
		SessionKey:  "s-no-budget",
		OwnerUserID: "user-1",
		Objective:   "open-ended exploration",
		Status:      "active",
		// TokenBudget intentionally nil — exercises the nullable column path
	}
	if err := db.CreateGoal(ctx, g); err != nil {
		t.Fatalf("create unbounded: %v", err)
	}
	got, err := db.GetGoalBySession(ctx, "agent-B", "s-no-budget")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TokenBudget != nil {
		t.Errorf("expected nil TokenBudget, got %v", got.TokenBudget)
	}
}
