package agent

import (
	"encoding/json"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
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

func TestCompactionKeepsRecentFourUserTurns(t *testing.T) {
	var msgs []provider.Message
	for i := 1; i <= 6; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "user turn", Origin: provider.OriginUser},
			provider.Message{Role: "user", Content: "runtime context", Origin: provider.OriginGoalContext},
			provider.Message{Role: "assistant", Content: "assistant reply", Origin: provider.OriginUser},
		)
	}

	got := compactionTailStart(msgs, CompactOptions{})

	if got != 6 {
		t.Fatalf("tail start = %d, want 6 (the third real user turn)", got)
	}

	userTurns := 0
	for _, msg := range msgs[got:] {
		if msg.Role == "user" && msg.Origin == provider.OriginUser {
			userTurns++
		}
	}
	if userTurns != DefaultTailTurns {
		t.Fatalf("tail has %d real user turns, want %d", userTurns, DefaultTailTurns)
	}
}
