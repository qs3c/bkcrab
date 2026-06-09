package store

import (
	"context"
	"testing"
)

// TestSessionMessageOriginRoundTrip pins the persistence contract:
// AppendSessionMessage must write the Origin column, and
// ListSessionMessages must read it back. Without this round-trip the
// WebChatHistory filter for goal_context messages silently fails on
// post-restart sessions (the in-memory working set has the marker, the
// archive doesn't, and history rendering shows synthetic prompts).
func TestSessionMessageOriginRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	uid, agent, key := "user-1", "agent-A", "s-origin-test"

	// One real user turn, one runtime-injected goal_context, one
	// assistant reply — the canonical interleaving once /goal is
	// running.
	msgs := []SessionMessage{
		{Role: "user", Content: "translate the README"},
		{Role: "user", Content: "[runtime audit prompt]", Origin: "goal_context"},
		{Role: "assistant", Content: "translation done"},
	}
	for _, m := range msgs {
		if err := db.AppendSessionMessage(ctx, uid, agent, key, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	got, err := db.ListSessionMessages(ctx, uid, agent, key)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(got))
	}

	wantOrigins := []string{"", "goal_context", ""}
	for i, m := range got {
		if m.Origin != wantOrigins[i] {
			t.Errorf("message[%d] origin = %q, want %q", i, m.Origin, wantOrigins[i])
		}
	}
}
