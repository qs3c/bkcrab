package session

import (
	"context"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

func TestSessionAppendTurnAnchorReturnsSeq(t *testing.T) {
	dir := t.TempDir()
	db, err := store.NewDBStore("sqlite", "file:"+dir+"/t.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	adapter := NewStoreAdapter(db, "u1")
	s := &Session{store: adapter, agentID: "agentA", sessionKey: "sess1"}

	seq0, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor 0: %v", err)
	}
	seq1, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "again"})
	if err != nil {
		t.Fatalf("anchor 1: %v", err)
	}
	if seq0 != 0 || seq1 != 1 {
		t.Fatalf("seq want 0,1 got %d,%d", seq0, seq1)
	}
}
