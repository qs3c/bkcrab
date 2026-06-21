package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

func TestSanitizeToolPairsDropsOrphanToolResult(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "before"},
		{Role: "tool", ToolCallID: "call_orphan", Content: "orphan result"},
		{Role: "assistant", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("unexpected sanitized messages: %+v", got)
	}
}

func TestSanitizeToolPairsDropsIncompleteAssistantToolCallGroup(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "before"},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{ID: "call_a", Function: provider.FunctionCall{Name: "a"}},
				{ID: "call_b", Function: provider.FunctionCall{Name: "b"}},
			},
		},
		{Role: "tool", ToolCallID: "call_a", Content: "a result"},
		{Role: "user", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Content != "before" || got[1].Content != "after" {
		t.Fatalf("unexpected sanitized messages: %+v", got)
	}
}

func TestSanitizeToolPairsKeepsCompleteParsedToolCallGroupUnchanged(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "before"},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{ID: "call_a", Function: provider.FunctionCall{Name: "a"}},
				{ID: "call_b", Function: provider.FunctionCall{Name: "b"}},
			},
		},
		{Role: "tool", ToolCallID: "call_a", Content: "a result"},
		{Role: "tool", ToolCallID: "call_b", Content: "b result"},
		{Role: "assistant", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != len(msgs) {
		t.Fatalf("message count = %d, want %d: %+v", len(got), len(msgs), got)
	}
	for i := range msgs {
		if got[i].Role != msgs[i].Role || got[i].Content != msgs[i].Content || got[i].ToolCallID != msgs[i].ToolCallID {
			t.Fatalf("message %d changed: got %+v want %+v", i, got[i], msgs[i])
		}
		if len(got[i].ToolCalls) != len(msgs[i].ToolCalls) {
			t.Fatalf("message %d tool call count = %d, want %d", i, len(got[i].ToolCalls), len(msgs[i].ToolCalls))
		}
	}
}

func TestSanitizeToolPairsKeepsTextFromIncompleteAssistantToolCallGroup(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","content":"working","tool_calls":[{"id":"call_a"}]}`)
	msgs := []provider.Message{
		{
			Role:         "assistant",
			Content:      "working",
			Thinking:     "thought",
			RawAssistant: raw,
			ToolCalls: []provider.ToolCall{
				{ID: "call_a", Function: provider.FunctionCall{Name: "a"}},
				{ID: "call_b", Function: provider.FunctionCall{Name: "b"}},
			},
		},
		{Role: "tool", ToolCallID: "call_a", Content: "a result"},
		{Role: "user", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Role != "assistant" || got[0].Content != "working" || got[0].Thinking != "thought" {
		t.Fatalf("assistant text was not preserved: %+v", got[0])
	}
	if len(got[0].ToolCalls) != 0 {
		t.Fatalf("assistant tool calls were not cleared: %+v", got[0].ToolCalls)
	}
	if len(got[0].RawAssistant) != 0 {
		t.Fatalf("assistant raw payload was not cleared: %s", string(got[0].RawAssistant))
	}
}

func TestSanitizeToolPairsKeepsCompleteRawAssistantOnlyToolCallGroup(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call_raw"}]}`)
	msgs := []provider.Message{
		{Role: "assistant", RawAssistant: raw},
		{Role: "tool", ToolCallID: "call_raw", Content: "raw result"},
		{Role: "user", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != len(msgs) {
		t.Fatalf("message count = %d, want %d: %+v", len(got), len(msgs), got)
	}
	if string(got[0].RawAssistant) != string(raw) {
		t.Fatalf("raw assistant changed: got %s want %s", string(got[0].RawAssistant), string(raw))
	}
	if got[1].Role != "tool" || got[1].ToolCallID != "call_raw" {
		t.Fatalf("tool result was not preserved: %+v", got[1])
	}
}

func TestSanitizeToolPairsClearsIncompleteRawAssistantOnlyGroupWhenTextRemains(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","content":"working","tool_calls":[{"id":"call_raw"},{"id":"call_missing"}]}`)
	msgs := []provider.Message{
		{Role: "assistant", Content: "working", RawAssistant: raw},
		{Role: "tool", ToolCallID: "call_raw", Content: "raw result"},
		{Role: "user", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 2 {
		t.Fatalf("message count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Role != "assistant" || got[0].Content != "working" {
		t.Fatalf("assistant text was not preserved: %+v", got[0])
	}
	if len(got[0].RawAssistant) != 0 {
		t.Fatalf("assistant raw payload was not cleared: %s", string(got[0].RawAssistant))
	}
	if len(got[0].ToolCalls) != 0 {
		t.Fatalf("assistant parsed tool calls were not cleared: %+v", got[0].ToolCalls)
	}
	if got[1].Role != "user" || got[1].Content != "after" {
		t.Fatalf("following user message was not preserved: %+v", got[1])
	}
}

func TestSanitizeToolPairsDropsIncompleteRawAssistantOnlyGroupWithoutText(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call_raw"},{"id":"call_missing"}]}`)
	msgs := []provider.Message{
		{Role: "assistant", RawAssistant: raw},
		{Role: "tool", ToolCallID: "call_raw", Content: "raw result"},
		{Role: "user", Content: "after"},
	}

	got := sanitizeToolPairs(msgs)

	if len(got) != 1 {
		t.Fatalf("message count = %d, want 1: %+v", len(got), got)
	}
	if got[0].Role != "user" || got[0].Content != "after" {
		t.Fatalf("unexpected sanitized messages: %+v", got)
	}
}

func TestPruneOldToolResultsSummarizesTerminalResultLocally(t *testing.T) {
	content := "Exit code: 2\nOutput:\nfirst line\nsecond line\n" + strings.Repeat("x", 2200)
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_terminal",
					Function: provider.FunctionCall{
						Name:      "terminal",
						Arguments: `{"command":"go test ./internal/agent","timeout":30}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_terminal",
			Name:       "terminal",
			Content:    content,
			Metadata:   map[string]any{"sandbox": true},
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if !changed {
		t.Fatal("expected old terminal tool result to be summarized")
	}
	summary := got[1].Content
	assertContainsAll(t, summary,
		"[Tool Result Summary]",
		"tool: terminal",
		"command: go test ./internal/agent",
		"exit_code: 2",
		fmt.Sprintf("output_lines: %d", testLineCount(content)),
		fmt.Sprintf("output_chars: %d", len([]rune(content))),
	)
	if strings.Contains(summary, "memory logs") {
		t.Fatalf("summary must not mention memory logs: %q", summary)
	}
	if got[1].ToolCallID != "call_terminal" || got[1].Name != "terminal" {
		t.Fatalf("tool identity changed: %+v", got[1])
	}
	if got[1].Metadata != nil {
		t.Fatalf("metadata was not cleared: %+v", got[1].Metadata)
	}
}

func TestPruneOldToolResultsKeepsResultsAtTwoThousandBytes(t *testing.T) {
	content := strings.Repeat("x", 2000)
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_terminal",
					Function: provider.FunctionCall{
						Name:      "terminal",
						Arguments: `{"command":"go test ./internal/agent"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_terminal",
			Name:       "terminal",
			Content:    content,
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if changed {
		t.Fatal("tool result at 2000 bytes should be preserved")
	}
	if got[1].Content != content {
		t.Fatalf("tool result content changed: got %q want %q", got[1].Content, content)
	}
}

func TestPruneOldToolResultsArchivesOriginalAndKeepsHeadTailSnippets(t *testing.T) {
	contentLines := []string{
		"Exit code: 0",
		"Output:",
		"head one",
		"head two",
		"head three",
	}
	for i := 0; i < 50; i++ {
		contentLines = append(contentLines, fmt.Sprintf("middle filler line %02d with enough text to make the result large", i))
	}
	contentLines = append(contentLines, "tail one", "tail two", "tail three")
	content := strings.Join(contentLines, "\n")
	archiveStore := &recordingContextArchiveStore{}
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_archive",
					Function: provider.FunctionCall{
						Name:      "exec",
						Arguments: `{"command":"go test ./internal/agent"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_archive",
			Name:       "exec",
			Content:    content,
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs, CompactOptions{
		ArchiveStore:      archiveStore,
		ArchiveUserID:     "user-a",
		ArchiveAgentID:    "agent-a",
		ArchiveSessionKey: "session-a",
	})
	if !changed {
		t.Fatal("expected old tool result to be summarized")
	}
	if len(archiveStore.records) != 1 {
		t.Fatalf("archive record count = %d, want 1", len(archiveStore.records))
	}
	rec := archiveStore.records[0]
	if rec.Content != content {
		t.Fatalf("archived content changed: got %q want %q", rec.Content, content)
	}
	if rec.AgentID != "agent-a" || rec.SessionKey != "session-a" || rec.ToolCallID != "call_archive" || rec.ToolName != "exec" {
		t.Fatalf("archive scope/metadata mismatch: %+v", rec)
	}
	if rec.ID == "" {
		t.Fatal("archive id was empty")
	}

	summary := got[1].Content
	assertContainsAll(t, summary,
		"archive_id: "+rec.ID,
		"retrieve_compacted_tool_result",
		"output_head:",
		"Exit code: 0",
		"Output:",
		"head one",
		"output_tail:",
		"tail one",
		"tail two",
		"tail three",
	)
	if strings.Contains(summary, "middle filler line 10") {
		t.Fatalf("summary retained middle output instead of snippets: %q", summary)
	}
}

func TestPruneOldToolResultsSummarizesReadFileResult(t *testing.T) {
	content := strings.Repeat("package agent\n", 160)
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_read",
					Function: provider.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"internal/agent/compaction.go"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_read",
			Name:       "read_file",
			Content:    content,
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if !changed {
		t.Fatal("expected old read_file tool result to be summarized")
	}
	assertContainsAll(t, got[1].Content,
		"tool: read_file",
		"path: internal/agent/compaction.go",
		fmt.Sprintf("output_chars: %d", len([]rune(content))),
	)
}

func TestPruneOldToolResultsSummarizesSearchAndGenericResults(t *testing.T) {
	searchContent := strings.Repeat("match\n", 400)
	genericContent := strings.Repeat("row\n", 700)
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_search",
					Function: provider.FunctionCall{
						Name:      "grep",
						Arguments: `{"pattern":"TODO","directory":"internal/agent","extra":"ignored"}`,
					},
				},
				{
					ID: "call_generic",
					Function: provider.FunctionCall{
						Name:      "custom_tool",
						Arguments: `{"alpha":"one","beta":2,"gamma":"ignored"}`,
					},
				},
			},
		},
		{Role: "tool", ToolCallID: "call_search", Name: "grep", Content: searchContent},
		{Role: "tool", ToolCallID: "call_generic", Name: "custom_tool", Content: genericContent},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if !changed {
		t.Fatal("expected old search and generic tool results to be summarized")
	}
	assertContainsAll(t, got[1].Content,
		"tool: grep",
		"query: TODO",
		"path: internal/agent",
		fmt.Sprintf("output_lines: %d", testLineCount(searchContent)),
		fmt.Sprintf("output_chars: %d", len([]rune(searchContent))),
	)
	assertContainsAll(t, got[2].Content,
		"tool: custom_tool",
		"arg.alpha: one",
		"arg.beta: 2",
		fmt.Sprintf("output_lines: %d", testLineCount(genericContent)),
		fmt.Sprintf("output_chars: %d", len([]rune(genericContent))),
	)
	if strings.Contains(got[2].Content, "gamma") {
		t.Fatalf("generic summary kept more than two args: %q", got[2].Content)
	}
}

func TestPruneOldToolResultsTruncatesLargeArgumentValues(t *testing.T) {
	largeContent := strings.Repeat("PATCH_LINE_", 80)
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_patch",
					Function: provider.FunctionCall{
						Name: "apply_patch",
						Arguments: mustJSON(t, map[string]any{
							"patch": largeContent,
							"note":  "short",
						}),
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_patch",
			Name:       "apply_patch",
			Content:    strings.Repeat("ok\n", 700),
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if !changed {
		t.Fatal("expected old apply_patch tool result to be summarized")
	}
	summary := got[1].Content
	assertContainsAll(t, summary,
		"tool: apply_patch",
		"arg.patch:",
		fmt.Sprintf("[truncated, chars=%d]", len([]rune(largeContent))),
		"arg.note: short",
	)
	if strings.Contains(summary, largeContent) {
		t.Fatalf("summary included the full large argument: %q", summary)
	}
}

func TestPruneOldToolResultsBindsDuplicateToolCallIDsByLocalGroup(t *testing.T) {
	msgs := append([]provider.Message{
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_reused",
					Function: provider.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"old.txt"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_reused",
			Name:       "read_file",
			Content:    strings.Repeat("old result\n", 200),
		},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{
				{
					ID: "call_reused",
					Function: provider.FunctionCall{
						Name:      "read_file",
						Arguments: `{"path":"new.txt"}`,
					},
				},
			},
		},
		{
			Role:       "tool",
			ToolCallID: "call_reused",
			Name:       "read_file",
			Content:    strings.Repeat("new result\n", 200),
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if !changed {
		t.Fatal("expected old duplicate-id tool results to be summarized")
	}
	assertContainsAll(t, got[1].Content,
		"tool: read_file",
		"path: old.txt",
	)
	if strings.Contains(got[1].Content, "new.txt") {
		t.Fatalf("first duplicate-id summary used later args: %q", got[1].Content)
	}
	assertContainsAll(t, got[3].Content,
		"tool: read_file",
		"path: new.txt",
	)
}

func TestPruneOldToolResultsUsesRawAssistantOnlyArguments(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","tool_calls":[{"id":"call_raw_args","type":"function","function":{"name":"web_fetch","arguments":"{\"url\":\"https://example.com/doc\"}"}}]}`)
	msgs := append([]provider.Message{
		{Role: "assistant", RawAssistant: raw},
		{
			Role:       "tool",
			ToolCallID: "call_raw_args",
			Name:       "web_fetch",
			Content:    strings.Repeat("fetched text\n", 180),
		},
	}, compactionFillerMessages(PruneTurnAge)...)

	got, changed := pruneOldToolResultsWithChange(msgs)
	if !changed {
		t.Fatal("expected old raw assistant tool result to be summarized")
	}
	assertContainsAll(t, got[1].Content,
		"tool: web_fetch",
		"url: https://example.com/doc",
	)
}

func compactionFillerMessages(n int) []provider.Message {
	msgs := make([]provider.Message, n)
	for i := range msgs {
		msgs[i] = provider.Message{Role: "user", Content: "recent filler", Origin: provider.OriginUser}
	}
	return msgs
}

func assertContainsAll(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("summary %q does not contain %q", haystack, needle)
		}
	}
}

func testLineCount(s string) int {
	if s == "" {
		return 0
	}
	lines := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		lines++
	}
	return lines
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(b)
}

type recordingContextArchiveStore struct {
	records []store.ContextArchiveRecord
}

func (s *recordingContextArchiveStore) SaveContextArchive(ctx context.Context, rec *store.ContextArchiveRecord) error {
	if rec != nil {
		s.records = append(s.records, *rec)
	}
	return nil
}

func TestCompactionKeepsDynamicTailNearTargetMessages(t *testing.T) {
	var msgs []provider.Message
	for i := 1; i <= 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "user turn", Origin: provider.OriginUser},
			provider.Message{Role: "user", Content: "runtime context", Origin: provider.OriginGoalContext},
			provider.Message{Role: "assistant", Content: "assistant reply", Origin: provider.OriginUser},
		)
	}

	got := compactionTailStart(msgs, CompactOptions{})

	// Runtime-injected user messages (OriginGoalContext) anchor turn
	// boundaries too, so the cutoff can land on one. This keeps cutoff
	// granularity fine even during /goal runs with few real user turns.
	// With 36 messages and a 20-message target, the closest boundary is
	// index 16, yielding an exact 20-message tail.
	if got != 16 {
		t.Fatalf("tail start = %d, want 16 for a 20-message tail at target", got)
	}
	if tailLen := len(msgs) - got; tailLen != DefaultTailTargetMessages {
		t.Fatalf("tail messages = %d, want %d", tailLen, DefaultTailTargetMessages)
	}
}
