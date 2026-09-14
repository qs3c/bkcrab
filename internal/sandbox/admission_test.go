package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type admissionPool struct {
	mu      sync.Mutex
	fail    bool
	owners  []string
	removed int
}

func (p *admissionPool) Get(ctx context.Context, a, b, c string) (Executor, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fail {
		return nil, errors.New("create failed")
	}
	p.owners = append(p.owners, UserIDFromContext(ctx))
	return &fakeExecutor{}, nil
}
func (p *admissionPool) Release(a, b, c string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed++
	return nil
}
func (p *admissionPool) CloseAll()       {}
func (p *admissionPool) Backend() string { return "fake" }

func TestAdmissionGlobalAndUserLimit(t *testing.T) {
	for _, tc := range []struct {
		name         string
		global, user int
		owner        string
	}{{"global", 1, 0, "other"}, {"same user across agents", 4, 1, "alice"}} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewLifecyclePool(&admissionPool{}, time.Hour, time.Hour)
			p.SetLimits(Limits{MaxContainers: tc.global, MaxPerUser: tc.user, MaxQueued: 1, QueueTimeout: 20 * time.Millisecond})
			e, _, err := p.acquire(context.Background(), sandboxScope{agentID: "a"}, "alice")
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = p.acquire(context.Background(), sandboxScope{agentID: "b"}, tc.owner)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected bounded wait, got %v", err)
			}
			p.finish(e)
			// Capacity pressure reclaims an idle slot without waiting for TTL.
			next, _, err := p.acquire(context.Background(), sandboxScope{agentID: "b"}, tc.owner)
			if err != nil {
				t.Fatal(err)
			}
			p.finish(next)
			p.CloseAll()
		})
	}
}
func TestAdmissionFailedCreationReleasesReservation(t *testing.T) {
	inner := &admissionPool{fail: true}
	p := NewLifecyclePool(inner, time.Hour, time.Hour)
	p.SetLimits(Limits{MaxContainers: 1})
	if _, _, err := p.acquire(context.Background(), sandboxScope{agentID: "a"}, "alice"); err == nil {
		t.Fatal("expected failure")
	}
	inner.fail = false
	e, _, err := p.acquire(context.Background(), sandboxScope{agentID: "b"}, "bob")
	if err != nil {
		t.Fatal(err)
	}
	p.finish(e)
	p.CloseAll()
}
func TestAdmissionBusyNeverEvicted(t *testing.T) {
	inner := &admissionPool{}
	p := NewLifecyclePool(inner, time.Millisecond, time.Hour)
	e, _, err := p.acquire(context.Background(), sandboxScope{agentID: "a"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	p.evictIdle() // lastUsed is zero, but the operation owns the reservation.
	if inner.removed != 0 {
		t.Fatal("active sandbox evicted")
	}
	if err := p.Release("a", "", ""); err == nil {
		t.Fatal("active explicit release must fail")
	}
	p.finish(e)
	p.CloseAll()
}
func TestAdmissionCancellationAndLazyOwner(t *testing.T) {
	inner := &admissionPool{}
	p := NewLifecyclePool(inner, time.Hour, time.Hour)
	p.SetLimits(Limits{MaxContainers: 1, QueueTimeout: time.Second})
	e, _, err := p.acquire(context.Background(), sandboxScope{agentID: "a"}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := p.acquire(ctx, sandboxScope{agentID: "b"}, "bob"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	p.finish(e)
	lazy, _ := p.Get(WithUserID(context.Background(), "bob"), "b", "", "")
	if _, err := lazy.Exec(context.Background(), "true", time.Second); err != nil {
		t.Fatal(err)
	}
	if inner.owners[len(inner.owners)-1] != "bob" {
		t.Fatal("lost lazy owner")
	}
	p.CloseAll()
}
