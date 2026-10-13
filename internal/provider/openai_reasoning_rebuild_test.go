package provider

import (
	"encoding/json"
	"testing"
)

func TestRebuiltAssistantPreservesReasoning(t *testing.T) {
	call := ToolCall{ID: "recovered_0", Type: "function", Function: FunctionCall{Name: "exec", Arguments: `{"command":"pwd"}`}}
	for _, withCall := range []bool{false, true} {
		messages := []Message{{Role: "assistant", Content: "Checking locally.", Thinking: "original reasoning"}}
		if withCall {
			messages[0].ToolCalls = []ToolCall{call}
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: "/testbed"})
		}
		var got apiMessage
		if err := json.Unmarshal(toAPIMessages(messages)[0], &got); err != nil {
			t.Fatal(err)
		}
		if got.ReasoningContent != "original reasoning" {
			t.Fatalf("withCall=%v: reasoning lost: %#v", withCall, got)
		}
		if withCall && (len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != call.ID) {
			t.Fatalf("tool pairing lost: %#v", got)
		}
	}
}

func TestAssistantRawReplayUnchanged(t *testing.T) {
	raw := json.RawMessage(`{"role":"assistant", "content":"answer", "reasoning_content":"original", "vendor_field":true}`)
	got := toAPIMessages([]Message{{Role: "assistant", Content: "different", Thinking: "different", RawAssistant: raw}})
	if string(got[0]) != string(raw) {
		t.Fatalf("raw replay changed: %s", got[0])
	}
}
