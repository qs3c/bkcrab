package imagegen

import (
	"encoding/json"
	"strings"
	"testing"
)

func testLimits() RequestLimits {
	return RequestLimits{MaxImagesPerBatch: 16, MaxItems: 16, PromptMaxRunes: 8000, RequestMaxBytes: 128 * 1024, WaitDefaultSeconds: 180, WaitMaxSeconds: 240}
}

func TestNormalizeCreateRequests(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want NormalizedRequest
	}{
		{name: "default action count size and wait", raw: `{"prompt":"cat"}`, want: NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, WaitSeconds: 180, Items: []NormalizedItem{{Index: 0, Label: "item-0", Prompt: "cat", Size: SizeSquare, Count: 1}}}},
		{name: "single prompt", raw: `{"action":"create","prompt":"cat","count":5,"size":" LANDSCAPE ","wait_seconds":0}`, want: NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, WaitSeconds: 0, Items: []NormalizedItem{{Index: 0, Label: "item-0", Prompt: "cat", Size: SizeLandscape, Count: 5}}}},
		{name: "items", raw: `{"action":"create","items":[{"label":" cover ","prompt":"A","count":2,"size":"portrait"},{"prompt":"B"}]}`, want: NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, WaitSeconds: 180, Items: []NormalizedItem{{Index: 0, Label: "cover", Prompt: "A", Size: SizePortrait, Count: 2}, {Index: 1, Label: "item-1", Prompt: "B", Size: SizeSquare, Count: 1}}}},
		{name: "status", raw: `{"action":"status","batch_id":"imgb_0123456789abcdef"}`, want: NormalizedRequest{Action: ActionStatus, BatchID: "imgb_0123456789abcdef"}},
		{name: "cancel", raw: `{"action":"cancel","batch_id":"imgb_0123456789abcdef"}`, want: NormalizedRequest{Action: ActionCancel, BatchID: "imgb_0123456789abcdef"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeRequest(json.RawMessage(tt.raw), testLimits())
			if err != nil {
				t.Fatalf("NormalizeRequest() error = %v", err)
			}
			if got.Version != tt.want.Version || string(got.Action) != string(tt.want.Action) || got.BatchID != tt.want.BatchID || got.WaitSeconds != tt.want.WaitSeconds || len(got.Items) != len(tt.want.Items) {
				t.Fatalf("NormalizeRequest() = %#v, want %#v", got, tt.want)
			}
			for i := range got.Items {
				if got.Items[i] != tt.want.Items[i] {
					t.Fatalf("item %d = %#v, want %#v", i, got.Items[i], tt.want.Items[i])
				}
			}
		})
	}
}

func TestNormalizeRequestRejectsInvalidInput(t *testing.T) {
	longPrompt := strings.Repeat("界", 8001)
	tests := []struct {
		name string
		raw  string
	}{
		{name: "unknown field", raw: `{"prompt":"cat","tenant_id":"u1"}`},
		{name: "trailing JSON value", raw: `{"prompt":"cat"} 5`},
		{name: "unknown action", raw: `{"action":"delete","batch_id":"imgb_0123456789abcdef"}`},
		{name: "missing prompt and items", raw: `{}`},
		{name: "prompt and items", raw: `{"prompt":"cat","items":[{"prompt":"dog"}]}`},
		{name: "empty prompt", raw: `{"prompt":"  \n "}`},
		{name: "explicit zero count", raw: `{"prompt":"cat","count":0}`},
		{name: "total above limit", raw: `{"prompt":"cat","count":17}`},
		{name: "too many items", raw: `{"items":[{"prompt":"1"},{"prompt":"2"},{"prompt":"3"},{"prompt":"4"},{"prompt":"5"},{"prompt":"6"},{"prompt":"7"},{"prompt":"8"},{"prompt":"9"},{"prompt":"10"},{"prompt":"11"},{"prompt":"12"},{"prompt":"13"},{"prompt":"14"},{"prompt":"15"},{"prompt":"16"},{"prompt":"17"}]}`},
		{name: "duplicate normalized label", raw: `{"items":[{"label":"cover","prompt":"A"},{"label":" cover ","prompt":"B"}]}`},
		{name: "bad size", raw: `{"prompt":"cat","size":"1024x1024"}`},
		{name: "wait negative", raw: `{"prompt":"cat","wait_seconds":-1}`},
		{name: "wait above max", raw: `{"prompt":"cat","wait_seconds":241}`},
		{name: "status with prompt", raw: `{"action":"status","batch_id":"imgb_0123456789abcdef","prompt":"cat"}`},
		{name: "cancel without batch", raw: `{"action":"cancel"}`},
		{name: "bad batch id", raw: `{"action":"status","batch_id":"other"}`},
		{name: "prompt rune limit", raw: string(mustJSON(t, map[string]any{"prompt": longPrompt}))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NormalizeRequest(json.RawMessage(tt.raw), testLimits()); err == nil {
				t.Fatalf("invalid input accepted: %s", tt.raw)
			}
		})
	}

	limits := testLimits()
	limits.RequestMaxBytes = 40
	if _, err := NormalizeRequest(json.RawMessage(`{"prompt":"this normalized request is too large"}`), limits); err == nil {
		t.Fatal("normalized request byte limit was not enforced")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestExecutionIdentityValidation(t *testing.T) {
	valid := ExecutionIdentity{UserID: "u", ConfigUserID: "cfg", AgentOwnerUserID: "owner", AgentID: "agent", WorkspaceProjectID: "project", WorkspaceSessionID: "session", MessageChannel: "web"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid identity rejected: %v", err)
	}
	for _, field := range []string{"UserID", "ConfigUserID", "AgentOwnerUserID", "AgentID"} {
		copy := valid
		switch field {
		case "UserID":
			copy.UserID = ""
		case "ConfigUserID":
			copy.ConfigUserID = ""
		case "AgentOwnerUserID":
			copy.AgentOwnerUserID = ""
		case "AgentID":
			copy.AgentID = ""
		}
		if err := copy.Validate(); err == nil {
			t.Fatalf("identity missing %s accepted", field)
		}
	}
}
