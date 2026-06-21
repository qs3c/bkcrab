package session

import (
	"context"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
)

func TestLocalTurnCoordinatorRequiresActiveTurn(t *testing.T) {
	ctx := context.Background()
	coord := NewLocalTurnCoordinator()
	key := TurnKey{UserID: "user-1", AgentID: "agent-1", SessionKey: "session-1"}

	if ok, err := coord.PushSteer(ctx, key, provider.Message{Role: "user", Content: "before"}); err != nil || ok {
		t.Fatalf("PushSteer before BeginTurn = (%v, %v), want (false, nil)", ok, err)
	}

	lease, ok, err := coord.BeginTurn(ctx, key)
	if err != nil || !ok {
		t.Fatalf("BeginTurn = (%v, %v), want active lease", ok, err)
	}

	if ok, err := coord.PushSteer(ctx, key, provider.Message{Role: "user", Content: "during"}); err != nil || !ok {
		t.Fatalf("PushSteer during active turn = (%v, %v), want (true, nil)", ok, err)
	}

	drained, err := coord.DrainSteer(ctx, lease)
	if err != nil {
		t.Fatalf("DrainSteer returned error: %v", err)
	}
	if len(drained) != 1 || drained[0].Content != "during" {
		t.Fatalf("DrainSteer = %#v, want one buffered steer", drained)
	}

	if drained, err := coord.DrainSteer(ctx, lease); err != nil || len(drained) != 0 {
		t.Fatalf("second DrainSteer = (%#v, %v), want empty", drained, err)
	}

	leftover, err := coord.EndTurn(ctx, lease)
	if err != nil || len(leftover) != 0 {
		t.Fatalf("EndTurn after drain = (%#v, %v), want empty", leftover, err)
	}

	if ok, err := coord.PushSteer(ctx, key, provider.Message{Role: "user", Content: "after"}); err != nil || ok {
		t.Fatalf("PushSteer after EndTurn = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestLocalTurnCoordinatorNestedTurnsFlushOnOuterEnd(t *testing.T) {
	ctx := context.Background()
	coord := NewLocalTurnCoordinator()
	key := TurnKey{UserID: "user-1", AgentID: "agent-1", SessionKey: "session-1"}

	outer, ok, err := coord.BeginTurn(ctx, key)
	if err != nil || !ok {
		t.Fatalf("outer BeginTurn = (%v, %v), want active lease", ok, err)
	}
	inner, ok, err := coord.BeginTurn(ctx, key)
	if err != nil || !ok {
		t.Fatalf("inner BeginTurn = (%v, %v), want active lease", ok, err)
	}

	if ok, err := coord.PushSteer(ctx, key, provider.Message{Role: "user", Content: "leftover"}); err != nil || !ok {
		t.Fatalf("PushSteer during nested turn = (%v, %v), want (true, nil)", ok, err)
	}

	leftover, err := coord.EndTurn(ctx, inner)
	if err != nil {
		t.Fatalf("inner EndTurn returned error: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("inner EndTurn should not flush while outer turn is active, got %#v", leftover)
	}

	leftover, err = coord.EndTurn(ctx, outer)
	if err != nil {
		t.Fatalf("outer EndTurn returned error: %v", err)
	}
	if len(leftover) != 1 || leftover[0].Content != "leftover" {
		t.Fatalf("outer EndTurn = %#v, want leftover steer", leftover)
	}
}
