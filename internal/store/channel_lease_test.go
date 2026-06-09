package store

import (
	"context"
	"testing"
	"time"
)

func TestChannelLease_AcquireAndRenew(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// First acquire wins (fresh row).
	ok, err := db.AcquireChannelLease(ctx, "wechat", "acct1", "holderA", 30*time.Second)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !ok {
		t.Fatal("expected first acquire to succeed on a fresh (channel, account) pair")
	}

	// Same holder calling again is a renew-via-acquire — must succeed
	// (otherwise the lease guard's retry loop would self-evict).
	ok, err = db.AcquireChannelLease(ctx, "wechat", "acct1", "holderA", 30*time.Second)
	if err != nil {
		t.Fatalf("same-holder reacquire: %v", err)
	}
	if !ok {
		t.Fatal("same holder must be allowed to reacquire its own lease")
	}

	// Renew on the held lease succeeds and bumps expires_at.
	ok, err = db.RenewChannelLease(ctx, "wechat", "acct1", "holderA", 60*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if !ok {
		t.Fatal("renew on held lease must succeed")
	}
}

func TestChannelLease_DifferentHolderBlocked(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Holder A grabs a long lease.
	if _, err := db.AcquireChannelLease(ctx, "telegram", "bot1", "holderA", 30*time.Second); err != nil {
		t.Fatalf("A acquire: %v", err)
	}

	// Holder B tries while A's lease is still fresh — must lose.
	ok, err := db.AcquireChannelLease(ctx, "telegram", "bot1", "holderB", 30*time.Second)
	if err != nil {
		t.Fatalf("B acquire: %v", err)
	}
	if ok {
		t.Fatal("B must not be able to steal a live lease")
	}

	// And B's Renew must fail (it never held the row in the first place).
	ok, err = db.RenewChannelLease(ctx, "telegram", "bot1", "holderB", 30*time.Second)
	if err != nil {
		t.Fatalf("B renew: %v", err)
	}
	if ok {
		t.Fatal("B must not be able to renew a lease it doesn't hold")
	}
}

func TestChannelLease_TakeoverAfterExpiry(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Holder A takes a lease that's already expired.
	if _, err := db.AcquireChannelLease(ctx, "discord", "guild1", "holderA", -1*time.Second); err != nil {
		t.Fatalf("A acquire (expired): %v", err)
	}

	// Holder B should now be able to steal.
	ok, err := db.AcquireChannelLease(ctx, "discord", "guild1", "holderB", 30*time.Second)
	if err != nil {
		t.Fatalf("B steal: %v", err)
	}
	if !ok {
		t.Fatal("B must be able to take over an expired lease")
	}

	// After steal, A's Renew must fail (the row now points at B).
	ok, err = db.RenewChannelLease(ctx, "discord", "guild1", "holderA", 30*time.Second)
	if err != nil {
		t.Fatalf("A renew after steal: %v", err)
	}
	if ok {
		t.Fatal("A's renew must report lease-lost after takeover")
	}
}

func TestChannelLease_Release(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if _, err := db.AcquireChannelLease(ctx, "slack", "team1", "holderA", 30*time.Second); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	if err := db.ReleaseChannelLease(ctx, "slack", "team1", "holderA"); err != nil {
		t.Fatalf("A release: %v", err)
	}

	// After release, B can acquire without waiting for TTL.
	ok, err := db.AcquireChannelLease(ctx, "slack", "team1", "holderB", 30*time.Second)
	if err != nil {
		t.Fatalf("B acquire after release: %v", err)
	}
	if !ok {
		t.Fatal("B must be able to acquire after holder A's release")
	}

	// Stale Release from A (now no longer the holder) must not evict B.
	if err := db.ReleaseChannelLease(ctx, "slack", "team1", "holderA"); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	ok, err = db.RenewChannelLease(ctx, "slack", "team1", "holderB", 30*time.Second)
	if err != nil {
		t.Fatalf("B renew after stale release: %v", err)
	}
	if !ok {
		t.Fatal("stale Release from a non-holder must not invalidate the current holder")
	}
}
