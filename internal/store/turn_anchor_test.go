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

func TestFinishTurnFlipsStatus(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"
	seq, err := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := db.FinishTurn(ctx, uid, agent, sk, seq); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var status string
	db.db.QueryRowContext(ctx,
		`SELECT turn_status FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`,
		uid, agent, sk, seq).Scan(&status)
	if status != "done" {
		t.Fatalf("turn_status want done got %q", status)
	}
}

func TestListSessionsIncludesLatestTurnStatus(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent := "u1", "agentA"

	if err := db.SaveSession(ctx, uid, agent, "done-session", &SessionRecord{
		Messages: []SessionMessage{{Role: "user", Content: "finished"}},
	}); err != nil {
		t.Fatalf("save done session: %v", err)
	}
	doneSeq, err := db.AppendTurnAnchor(ctx, uid, agent, "done-session", SessionMessage{Role: "user", Content: "finished"})
	if err != nil {
		t.Fatalf("append done anchor: %v", err)
	}
	if err := db.FinishTurn(ctx, uid, agent, "done-session", doneSeq); err != nil {
		t.Fatalf("finish done session: %v", err)
	}

	if err := db.SaveSession(ctx, uid, agent, "running-session", &SessionRecord{
		Messages: []SessionMessage{{Role: "user", Content: "still running"}},
	}); err != nil {
		t.Fatalf("save running session: %v", err)
	}
	if _, err := db.AppendTurnAnchor(ctx, uid, agent, "running-session", SessionMessage{Role: "user", Content: "still running"}); err != nil {
		t.Fatalf("append running anchor: %v", err)
	}

	metas, err := db.ListSessions(ctx, uid, agent)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	statuses := map[string]string{}
	for _, m := range metas {
		statuses[m.Key] = m.LastTurnStatus
	}
	if statuses["done-session"] != "done" {
		t.Fatalf("done-session status = %q; want done", statuses["done-session"])
	}
	if statuses["running-session"] != "running" {
		t.Fatalf("running-session status = %q; want running", statuses["running-session"])
	}
}
