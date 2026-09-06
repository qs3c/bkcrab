package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestCompactionDoesNotWriteHistoryFiles(t *testing.T) {
	for _, mode := range []CompactMode{CompactModeProactive, CompactModeManual, CompactModeEmergency} {
		t.Run(string(mode), func(t *testing.T) {
			t.Chdir(t.TempDir())
			msgs := append([]provider.Message{
				{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call", Function: provider.FunctionCall{Name: "exec"}}}},
				{Role: "tool", ToolCallID: "call", Name: "exec", Content: strings.Repeat("original output\n", 1000)},
			}, compactionFillerMessages(PruneTurnAge)...)
			result, err := CompactMessagesWithOptions(msgs, CompactOptions{
				Mode: mode, ContextWindow: 2000, MaxOutputTokens: 200,
				Provider: &fakeSummarizer{}, Model: "fake",
			})
			if err != nil || !result.Pruned {
				t.Fatalf("compaction did not complete: result=%+v err=%v", result, err)
			}
			entries, err := os.ReadDir(".")
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("compaction created files: %v", entries)
			}
		})
	}
}

func TestPruneOldToolResultsRecallsDuplicateIDsByOriginal(t *testing.T) {
	st, err := store.NewDBStore("sqlite", filepath.Join(t.TempDir(), "recall.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var msgs []provider.Message
	for _, label := range []string{"old", "new"} {
		msgs = append(msgs,
			provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "call_reused", Function: provider.FunctionCall{Name: "read_file", Arguments: `{"path":"` + label + `.txt"}`}}}},
			provider.Message{Role: "tool", ToolCallID: "call_reused", Name: "read_file", Content: strings.Repeat(label+" result\n", 250)},
		)
	}
	for _, msg := range msgs {
		if err := st.AppendSessionMessage(ctx, "u", "a", "s", store.SessionMessage{Role: msg.Role, ToolCallID: msg.ToolCallID, Name: msg.Name, Content: msg.Content}); err != nil {
			t.Fatal(err)
		}
	}
	opts := CompactOptions{ToolRefStore: st, RecallUserID: "u", RecallAgentID: "a", RecallSessionKey: "s"}
	registry := tools.NewRegistry("", "")
	registry.SetToolRecallStore(st, "a")
	registry.SetOwnerUserID("u")
	registry = registry.ForTurn()
	registry.SetRecallSessionKey("s")
	recall := registry.GetFunc("recall_tool_result")
	if recall == nil {
		t.Fatal("database recall tool is missing")
	}
	for _, start := range []int{0, 2} {
		t.Run(fmt.Sprintf("working_set_from_%d", start), func(t *testing.T) {
			working := append(append([]provider.Message(nil), msgs[start:]...), compactionFillerMessages(PruneTurnAge)...)
			got, changed := pruneOldToolResultsWithChange(working, opts)
			if !changed {
				t.Fatal("pruning did not run")
			}
			for i := 1; i < len(msgs)-start; i += 2 {
				seq := int64(start + i)
				assertContainsAll(t, got[i].Content, fmt.Sprintf("msg_ref: %d\n", seq))
				recalled, err := recall(ctx, json.RawMessage(fmt.Sprintf(`{"ref":%d}`, seq)))
				if err != nil {
					t.Fatal(err)
				}
				_, body, ok := strings.Cut(recalled, "\n\n")
				if !ok || body != msgs[start+i].Content {
					t.Fatal("summary points to another tool result")
				}
			}
		})
	}
}

type duplicateToolRefStore struct {
	fakeToolRefStore
	rows map[int64]store.SessionToolMessage
	err  error
}

func (s *duplicateToolRefStore) GetSessionToolMessage(ctx context.Context, userID, agentID, sessionKey, chatterUserID string, seq int64) (*store.SessionToolMessage, error) {
	if s.err != nil && seq == 2 {
		return nil, s.err
	}
	rec, ok := s.rows[seq]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &rec, nil
}

func TestPruneOldToolResultsDuplicateIDRequiresUniqueOriginal(t *testing.T) {
	content := strings.Repeat("original output\n", 200)
	for _, tc := range []struct {
		name      string
		otherName string
		otherBody string
		missing   bool
		readErr   error
		wantRef   bool
	}{
		{name: "same_body_different_tool", otherName: "exec", otherBody: content, wantRef: true},
		{name: "same_tool_and_body", otherName: "read_file", otherBody: content},
		{name: "missing_original", otherName: "read_file", otherBody: "different", missing: true},
		{name: "partial_read_failure", otherName: "read_file", otherBody: "different", readErr: errors.New("archive unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &duplicateToolRefStore{
				fakeToolRefStore: fakeToolRefStore{refs: []store.ToolMsgRef{
					{Seq: 1, ToolCallID: "reused"}, {Seq: 2, ToolCallID: "reused"},
				}},
				rows: map[int64]store.SessionToolMessage{
					1: {Seq: 1, ToolCallID: "reused", Name: "read_file", Content: content},
					2: {Seq: 2, ToolCallID: "reused", Name: tc.otherName, Content: tc.otherBody},
				},
				err: tc.readErr,
			}
			if tc.missing {
				st.rows[1] = store.SessionToolMessage{Seq: 1, ToolCallID: "reused", Name: "read_file", Content: "also different"}
			}
			msgs := append([]provider.Message{
				{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "reused", Function: provider.FunctionCall{Name: "read_file"}}}},
				{Role: "tool", ToolCallID: "reused", Name: "read_file", Content: content},
			}, compactionFillerMessages(PruneTurnAge)...)
			got, changed := pruneOldToolResultsWithChange(msgs, CompactOptions{
				ToolRefStore: st, RecallUserID: "u", RecallAgentID: "a", RecallSessionKey: "s",
			})
			if !changed {
				t.Fatal("pruning did not run")
			}
			if tc.wantRef {
				assertContainsAll(t, got[1].Content, "msg_ref: 1\n")
			} else if strings.Contains(got[1].Content, "msg_ref:") {
				t.Fatalf("ambiguous original advertised a ref: %s", got[1].Content)
			}
		})
	}
}
