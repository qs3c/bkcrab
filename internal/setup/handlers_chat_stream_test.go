package setup

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent"
)

func TestForwardChatStreamEventWritesDoneBeforeReturning(t *testing.T) {
	rr := httptest.NewRecorder()
	turnPending, done := forwardChatStreamEvent(rr, rr, agent.EventEnvelope{
		Seq: 12,
		Event: agent.ChatEvent{
			Type: "done",
			Data: map[string]any{
				"usage": map[string]any{"usedTokens": 42},
			},
		},
	})

	if turnPending {
		t.Fatal("turnPending = true, want false")
	}
	if !done {
		t.Fatal("done = false, want true")
	}
	body := rr.Body.String()
	if !strings.Contains(body, "id: 12") {
		t.Fatalf("SSE body missing id line: %q", body)
	}
	if !strings.Contains(body, `"type":"done"`) {
		t.Fatalf("SSE body missing done event: %q", body)
	}
	if !strings.Contains(body, `"usedTokens":42`) {
		t.Fatalf("SSE body missing usage payload: %q", body)
	}
}
