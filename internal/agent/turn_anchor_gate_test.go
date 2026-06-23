package agent

import (
	"context"
	"testing"

	"github.com/qs3c/bkclaw/internal/bus"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/session"
	"github.com/qs3c/bkclaw/internal/store"
)

// TestIsUserTurnOnlyTrueForUserSource pins the cadence-anchor policy: only a real
// user turn (Source unset == bus.SourceUser) anchors the extraction cadence. Every
// runtime-injected source (goal_context continuation, cron, heartbeat, subagent)
// drives work but is not chatter dialogue and must not advance the every-N-turns
// cadence (memory-extraction-trigger-design §9).
func TestIsUserTurnOnlyTrueForUserSource(t *testing.T) {
	cases := []struct {
		source string
		want   bool
	}{
		{bus.SourceUser, true},
		{bus.SourceGoalContext, false},
		{bus.SourceCron, false},
		{bus.SourceHeartbeat, false},
		{bus.SourceSubAgent, false},
	}
	for _, tc := range cases {
		if got := isUserTurn(tc.source); got != tc.want {
			t.Errorf("isUserTurn(%q) = %v, want %v", tc.source, got, tc.want)
		}
	}
}

// TestBeginTurnAnchorOnlyAnchorsUserTurns reproduces the bug: goal_context
// continuation turns must NOT be registered as cadence anchors, yet their message
// must still land in the working set so the model sees the continuation prompt.
// A user turn must anchor and be claimable; goal_context turns interleaved with it
// must be invisible to ClaimCadenceBatch.
func TestBeginTurnAnchorOnlyAnchorsUserTurns(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.NewDBStore("sqlite", "file:"+dir+"/t.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	adapter := session.NewStoreAdapter(db, "u1")
	sm := session.NewManagerWithStoreForUser(dir, adapter, "u1", "agentA")
	sess := sm.Get("web", "", "c1", "")
	sess.SetChatter("c1")
	sk := sess.SessionKey()

	a := &Agent{name: "t"}
	const n = 2

	// Two goal_context continuation turns: not anchored, but still in the working set.
	for i := range 2 {
		gc := provider.Message{Role: "user", Content: "continue", Origin: provider.OriginGoalContext}
		if anchor := a.beginTurnAnchor(sess, gc, bus.SourceGoalContext); anchor != nil {
			t.Fatalf("goal_context turn %d anchored (seq %d); runtime injections must not anchor", i, anchor.seq)
		}
	}

	// One real user turn: anchored and finished.
	anchor := a.beginTurnAnchor(sess, provider.Message{Role: "user", Content: "Q1"}, bus.SourceUser)
	if anchor == nil {
		t.Fatal("user turn did not anchor")
	}
	if err := db.FinishTurn(ctx, "u1", "agentA", sk, anchor.seq); err != nil {
		t.Fatalf("finish: %v", err)
	}

	// The continuation prompts must be visible to the model.
	var continues int
	for _, m := range sess.GetMessages() {
		if m.Content == "continue" {
			continues++
		}
	}
	if continues != 2 {
		t.Fatalf("working set has %d continuation prompts, want 2", continues)
	}

	// Only ONE real anchor exists, so an N=2 cadence claims nothing — proof the two
	// goal_context turns did not advance the cadence.
	id, refs, err := db.ClaimCadenceBatch(ctx, "agentA", "c1", n, 3*n)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if id != "" {
		t.Fatalf("cadence claimed %d turns from goal_context noise; want none", len(refs))
	}

	// A second real user turn pushes the real-anchor count to N → now claimable,
	// and the batch is exactly the two user turns.
	anchor2 := a.beginTurnAnchor(sess, provider.Message{Role: "user", Content: "Q2"}, bus.SourceUser)
	if anchor2 == nil {
		t.Fatal("second user turn did not anchor")
	}
	if err := db.FinishTurn(ctx, "u1", "agentA", sk, anchor2.seq); err != nil {
		t.Fatalf("finish 2: %v", err)
	}
	id2, refs2, err := db.ClaimCadenceBatch(ctx, "agentA", "c1", n, 3*n)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if id2 == "" || len(refs2) != 2 {
		t.Fatalf("want 2 user turns claimed, got id=%q refs=%d", id2, len(refs2))
	}
}
