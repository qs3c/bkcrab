package session

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/qs3c/bkclaw/internal/provider"
)

func TestRedisTurnCoordinatorSharesSteerAcrossInstances(t *testing.T) {
	ctx := context.Background()
	srv := miniredis.RunT(t)
	ownerClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	otherClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() {
		_ = ownerClient.Close()
		_ = otherClient.Close()
	})

	owner := NewRedisTurnCoordinator(ownerClient, RedisTurnOptions{
		KeyPrefix: "test",
		OwnerID:   "pod-a",
		LeaseTTL:  time.Minute,
	})
	other := NewRedisTurnCoordinator(otherClient, RedisTurnOptions{
		KeyPrefix: "test",
		OwnerID:   "pod-b",
		LeaseTTL:  time.Minute,
	})

	key := TurnKey{UserID: "user-1", AgentID: "agent-1", SessionKey: "session-1"}
	lease, ok, err := owner.BeginTurn(ctx, key)
	if err != nil || !ok {
		t.Fatalf("owner BeginTurn = (%v, %v), want active lease", ok, err)
	}

	if _, ok, err := other.BeginTurn(ctx, key); err != nil || ok {
		t.Fatalf("other BeginTurn during active lease = (%v, %v), want false nil", ok, err)
	}
	if ok, err := other.PushSteer(ctx, key, provider.Message{Role: "user", Content: "from another pod"}); err != nil || !ok {
		t.Fatalf("other PushSteer = (%v, %v), want true nil", ok, err)
	}

	drained, err := owner.DrainSteer(ctx, lease)
	if err != nil {
		t.Fatalf("owner DrainSteer returned error: %v", err)
	}
	if len(drained) != 1 || drained[0].Content != "from another pod" {
		t.Fatalf("owner DrainSteer = %#v, want cross-instance steer", drained)
	}
	if drained, err := owner.DrainSteer(ctx, lease); err != nil || len(drained) != 0 {
		t.Fatalf("second owner DrainSteer = (%#v, %v), want empty", drained, err)
	}

	if leftover, err := owner.EndTurn(ctx, lease); err != nil || len(leftover) != 0 {
		t.Fatalf("owner EndTurn = (%#v, %v), want empty nil", leftover, err)
	}
	if ok, err := other.PushSteer(ctx, key, provider.Message{Role: "user", Content: "after"}); err != nil || ok {
		t.Fatalf("other PushSteer after EndTurn = (%v, %v), want false nil", ok, err)
	}
}

func TestRedisTurnCoordinatorNestedTurnsFlushOnOuterEnd(t *testing.T) {
	ctx := context.Background()
	srv := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	otherClient := redis.NewClient(&redis.Options{Addr: srv.Addr()})
	t.Cleanup(func() {
		_ = client.Close()
		_ = otherClient.Close()
	})

	coord := NewRedisTurnCoordinator(client, RedisTurnOptions{
		KeyPrefix: "test",
		OwnerID:   "pod-a",
		LeaseTTL:  time.Minute,
	})
	other := NewRedisTurnCoordinator(otherClient, RedisTurnOptions{
		KeyPrefix: "test",
		OwnerID:   "pod-b",
		LeaseTTL:  time.Minute,
	})
	key := TurnKey{UserID: "user-1", AgentID: "agent-1", SessionKey: "session-1"}

	outer, ok, err := coord.BeginTurn(ctx, key)
	if err != nil || !ok {
		t.Fatalf("outer BeginTurn = (%v, %v), want active lease", ok, err)
	}
	inner, ok, err := coord.BeginTurn(ctx, key)
	if err != nil || !ok {
		t.Fatalf("inner BeginTurn = (%v, %v), want re-entrant lease", ok, err)
	}

	if ok, err := other.PushSteer(ctx, key, provider.Message{Role: "user", Content: "leftover"}); err != nil || !ok {
		t.Fatalf("PushSteer during nested turn = (%v, %v), want true nil", ok, err)
	}
	if leftover, err := coord.EndTurn(ctx, inner); err != nil || len(leftover) != 0 {
		t.Fatalf("inner EndTurn = (%#v, %v), want empty nil", leftover, err)
	}
	if ok, err := other.PushSteer(ctx, key, provider.Message{Role: "user", Content: "still active"}); err != nil || !ok {
		t.Fatalf("PushSteer after inner EndTurn = (%v, %v), want true nil", ok, err)
	}

	leftover, err := coord.EndTurn(ctx, outer)
	if err != nil {
		t.Fatalf("outer EndTurn returned error: %v", err)
	}
	if len(leftover) != 2 || leftover[0].Content != "leftover" || leftover[1].Content != "still active" {
		t.Fatalf("outer EndTurn = %#v, want both leftovers", leftover)
	}
}
