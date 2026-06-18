package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkclaw/internal/store"
)

func TestRetrieveCompactedToolResultReturnsOriginalByArchiveID(t *testing.T) {
	ctx := context.Background()
	fakeStore := &fakeContextArchiveStore{
		rec: &store.ContextArchiveRecord{
			ID:         "ctxar_test",
			UserID:     "user-a",
			AgentID:    "agent-a",
			SessionKey: "session-a",
			ToolCallID: "call-a",
			ToolName:   "exec",
			Content:    "original tool output\nline two",
			CreatedAt:  time.Now().UTC(),
		},
	}
	r := NewRegistry("", "")
	r.SetWorkspaceStore(nil, "agent-a")
	r.SetSessionID("session-a")
	r.SetContextArchiveStore(fakeStore)

	fn := r.GetFunc("retrieve_compacted_tool_result")
	if fn == nil {
		t.Fatal("retrieve_compacted_tool_result was not registered")
	}
	got, err := fn(ctx, json.RawMessage(`{"id":"ctxar_test"}`))
	if err != nil {
		t.Fatalf("retrieve archived tool result: %v", err)
	}
	if fakeStore.agentID != "agent-a" || fakeStore.sessionKey != "session-a" || fakeStore.id != "ctxar_test" {
		t.Fatalf("archive lookup scope mismatch: agent=%q session=%q id=%q", fakeStore.agentID, fakeStore.sessionKey, fakeStore.id)
	}
	assertToolOutputContains(t, got,
		"[Compacted Tool Result]",
		"archive_id: ctxar_test",
		"tool: exec",
		"tool_call_id: call-a",
		"original tool output",
		"line two",
	)
}

type fakeContextArchiveStore struct {
	rec        *store.ContextArchiveRecord
	agentID    string
	sessionKey string
	id         string
}

func (s *fakeContextArchiveStore) GetContextArchive(ctx context.Context, agentID, sessionKey, id string) (*store.ContextArchiveRecord, error) {
	s.agentID = agentID
	s.sessionKey = sessionKey
	s.id = id
	if s.rec == nil || s.rec.ID != id {
		return nil, store.ErrNotFound
	}
	return s.rec, nil
}

func assertToolOutputContains(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("tool output %q does not contain %q", haystack, needle)
		}
	}
}
