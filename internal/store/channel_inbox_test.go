package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestChannelInboxDedupClaimRecovery(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	for range 2 {
		if err := db.SaveChannelInbox(ctx, "bot-a", "msg-1", "hello"); err != nil {
			t.Fatal(err)
		}
	}
	first, err := db.ClaimChannelInbox(ctx, "bot-a", -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.ClaimChannelInbox(ctx, "bot-a", time.Minute)
	if err != nil || second.ID != first.ID || second.ClaimToken == first.ClaimToken {
		t.Fatalf("recovery: %+v %v", second, err)
	}
	if err := db.CompleteChannelInbox(ctx, first.ID, first.ClaimToken); err != nil {
		t.Fatal(err)
	}
	var done int64
	if err := db.db.QueryRow(`SELECT done_ms FROM channel_inbox WHERE id=?`, first.ID).Scan(&done); err != nil || done != 0 {
		t.Fatalf("stale claim completed: %d %v", done, err)
	}
	if err := db.CompleteChannelInbox(ctx, second.ID, second.ClaimToken); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveChannelInbox(ctx, "bot-a", "msg-1", "duplicate"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimChannelInbox(ctx, "bot-a", time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("duplicate delivered: %v", err)
	}
	if err := db.SaveChannelInbox(ctx, "bot-b", "msg-1", "other bot"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimChannelInbox(ctx, "bot-b", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestChannelInboxConcurrentClaim(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	if err := db.SaveChannelInbox(ctx, "bot", "message", "payload"); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := db.ClaimChannelInbox(ctx, "bot", time.Minute)
			if err == nil && rec != nil {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("winners: %d", wins.Load())
	}
}
