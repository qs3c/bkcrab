package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// toAPIMessagesReasoning mirrors the message shape that the agent loop
// persists after `maybeRecoverToolCalls` rescues a tool call from leaked
// DSML/XML text: RawAssistant is cleared (the stream carried no structured
// tool_calls), while ToolCalls and Thinking hold the recovered call and the
// model's reasoning. Look at the resulting wire JSON so the assertions pin
// exactly what DeepSeek receives.
func toAPIMessagesReasoning(t *testing.T, msgs []Message) []string {
	t.Helper()
	raw := toAPIMessages(msgs)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, string(r))
	}
	return out
}

// TestToAPIMessagesReplaysThinkingAsReasoningContent pins the production 400
// from trial django__django-14122__kpsnf5L: an assistant turn whose tool calls
// were recovered from text had RawAssistant cleared and no `reasoning_content`
// on the wire, so the next DeepSeek request in thinking mode failed with
// `The reasoning_content in the thinking mode must be passed back to the API.`
// Rebuilding from Message.Thinking must restore the field.
func TestToAPIMessagesReplaysThinkingAsReasoningContent(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "fix the ordering bug"},
		{
			Role:     "assistant",
			Content:  "Let me inspect the field.",
			Thinking: "The user wants the GROUP BY issue fixed; first locate _meta_ordering.",
			ToolCalls: []ToolCall{{
				ID:   "recovered_0",
				Type: "function",
				Function: FunctionCall{
					Name:      "exec",
					Arguments: `{"command":"grep -rn _meta_ordering django"}`,
				},
			}},
		},
		{Role: "tool", ToolCallID: "recovered_0", Content: "django/db/models/sql/query.py"},
	}

	wire := toAPIMessagesReasoning(t, msgs)
	if len(wire) != 3 {
		t.Fatalf("toAPIMessages returned %d messages, want 3", len(wire))
	}

	assistant := wire[1]
	for _, want := range []string{`"role":"assistant"`, `"reasoning_content"`, `"tool_calls"`, `"recovered_0"`} {
		if !strings.Contains(assistant, want) {
			t.Errorf("wire assistant message missing %s:\n%s", want, assistant)
		}
	}

	// Parse back so we assert on the field, not an incidental substring.
	var parsed struct {
		ReasoningContent string `json:"reasoning_content"`
		ToolCalls        []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal([]byte(assistant), &parsed); err != nil {
		t.Fatalf("wire assistant message is not valid JSON: %v\n%s", err, assistant)
	}
	if parsed.ReasoningContent != "The user wants the GROUP BY issue fixed; first locate _meta_ordering." {
		t.Errorf("reasoning_content = %q, want the persisted thinking text", parsed.ReasoningContent)
	}
	if len(parsed.ToolCalls) != 1 || parsed.ToolCalls[0].ID != "recovered_0" {
		t.Errorf("tool_calls = %+v, want the single recovered call", parsed.ToolCalls)
	}

	// A recovered turn must not be treated as an orphan: its tool reply has
	// to survive toAPIMessages or DeepSeek rejects the dangling tool result.
	orphanAssist, orphanTool := findOrphanToolCalls(msgs)
	if orphanAssist[1] || orphanTool[2] {
		t.Errorf("recovered turn wrongly flagged orphan: assistant=%v tool=%v", orphanAssist[1], orphanTool[2])
	}
}

// TestToAPIMessagesOmitsEmptyReasoningContent keeps the non-thinking path
// byte-identical: providers that never emit reasoning must not gain a stray
// empty `reasoning_content` field (pure OpenAI ignores unknown fields, but
// prompt-cache prefixes are byte-sensitive).
func TestToAPIMessagesOmitsEmptyReasoningContent(t *testing.T) {
	wire := toAPIMessagesReasoning(t, []Message{
		{Role: "assistant", Content: "done", ToolCalls: []ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "exec", Arguments: "{}"}}}},
	})
	if len(wire) != 1 {
		t.Fatalf("toAPIMessages returned %d messages, want 1", len(wire))
	}
	if strings.Contains(wire[0], "reasoning_content") {
		t.Errorf("assistant message without Thinking must omit reasoning_content:\n%s", wire[0])
	}
}

// TestToAPIMessagesPrefersRawAssistantReasoning guards the complement: when
// the provider captured a verbatim RawAssistant (the normal streaming path),
// it already carries reasoning_content and must be forwarded unchanged.
func TestToAPIMessagesPrefersRawAssistantReasoning(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant","content":"hi","reasoning_content":"original reasoning","tool_calls":[]}`)
	wire := toAPIMessagesReasoning(t, []Message{
		{Role: "assistant", Content: "hi", Thinking: "ignored because raw wins", RawAssistant: raw},
	})
	if len(wire) != 1 {
		t.Fatalf("toAPIMessages returned %d messages, want 1", len(wire))
	}
	if wire[0] != string(raw) {
		t.Errorf("RawAssistant should be forwarded verbatim:\ngot  %s\nwant %s", wire[0], string(raw))
	}
}
