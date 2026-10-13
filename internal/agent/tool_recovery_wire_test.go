package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
)

// 覆盖完整路径：流式 DSML 响应 -> 恢复 -> 执行 -> 下一轮 HTTP 请求。
func TestRecoveredToolReasoningRoundTrip(t *testing.T) {
	var requests atomic.Int32
	const thinking = "preserve this provider reasoning exactly"
	const content = `<DSML><invoke name="test_tool"><parameter name="value">check</parameter></invoke></DSML>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []map[string]any `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "bad request", 400)
			return
		}
		n := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		var delta map[string]any
		switch n {
		case 1:
			delta = map[string]any{"role": "assistant", "content": content, "reasoning_content": thinking}
		case 2:
			msgs := request.Messages
			if len(msgs) < 2 {
				t.Error("missing tool exchange")
				return
			}
			assistant, result := msgs[len(msgs)-2], msgs[len(msgs)-1]
			if assistant["reasoning_content"] != thinking {
				t.Errorf("recovered assistant lost reasoning: %#v", assistant)
			}
			calls, ok := assistant["tool_calls"].([]any)
			if !ok || len(calls) != 1 {
				t.Errorf("missing recovered call: %#v", assistant)
			} else if calls[0].(map[string]any)["id"] != result["tool_call_id"] {
				t.Error("unpaired tool result")
			}
			if result["role"] != "tool" || result["content"] != "local check complete" {
				t.Errorf("unexpected result: %#v", result)
			}
			delta = map[string]any{"role": "assistant", "content": "done"}
		default:
			t.Errorf("unexpected request %d", n)
			return
		}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer srv.Close()
	a := policyTestAgent(t, provider.NewOpenAI("test-key", srv.URL))
	a.ToolRegistry().Register("test_tool", "test", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) { return "local check complete", nil })
	reply := a.HandleWebChatStream(context.Background(), "wire-session", "", "owner", "check locally", nil, nil, make(chan ChatEvent, 128))
	if reply != "done" || requests.Load() != 2 {
		t.Fatalf("reply=%q requests=%d", reply, requests.Load())
	}
}
