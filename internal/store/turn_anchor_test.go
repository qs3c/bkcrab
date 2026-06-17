package store

import (
	"context"
	"testing"
)

func TestAppendTurnAnchorReturnsSeqAndStatus(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"

	seq0, err := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor 0: %v", err)
	}
	seq1, err := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "again"})
	if err != nil {
		t.Fatalf("anchor 1: %v", err)
	}
	if seq0 != 0 || seq1 != 1 {
		t.Fatalf("seq want 0,1 got %d,%d", seq0, seq1)
	}
	var status string
	if err := db.db.QueryRowContext(ctx,
		`SELECT turn_status FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`,
		uid, agent, sk, seq0).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "running" {
		t.Fatalf("turn_status want running got %q", status)
	}
}
