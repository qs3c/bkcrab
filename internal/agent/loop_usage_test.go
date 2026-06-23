package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/provider"
)

type usageEventProvider struct {
	calls int
}

func (p *usageEventProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("unexpected chat call")
}

func (p *usageEventProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	p.calls++
	ch := make(chan provider.StreamChunk, 1)
	switch p.calls {
	case 1:
		ch <- provider.StreamChunk{
			ToolCalls: []provider.ToolCall{{
				ID:   "call_test",
				Type: "function",
				Function: provider.FunctionCall{
					Name:      "test_tool",
					Arguments: `{"value":"x"}`,
				},
			}},
			Usage: provider.Usage{InputTokens: 111, OutputTokens: 5},
			Done:  true,
		}
	case 2:
		ch <- provider.StreamChunk{Content: "done", Usage: provider.Usage{InputTokens: 222, OutputTokens: 7}, Done: true}
	default:
		close(ch)
		return nil, errors.New("unexpected chat stream call")
	}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestHandleMessageEmitsUsageAfterEachModelCall(t *testing.T) {
	tmp := t.TempDir()
	ag := NewAgent(config.ResolvedAgent{
		ID:                "usage-agent",
		UserID:            "owner",
		Home:              filepath.Join(tmp, "home"),
		Workspace:         filepath.Join(tmp, "workspace"),
		Model:             "fake/model",
		MaxTokens:         100,
		ContextWindow:     100000,
		MaxToolIterations: 3,
	}, &usageEventProvider{}, nil, tmp)
	ag.ToolRegistry().Register("test_tool", "test tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	})

	events := make(chan ChatEvent, 16)
	reply := ag.HandleWebChatStream(context.Background(), "session-usage", "", "user-1", "please use a tool", nil, nil, events)
	if reply != "done" {
		t.Fatalf("reply = %q, want done", reply)
	}
	close(events)

	for evt := range events {
		if evt.Type == "done" {
			t.Fatal("received done before the first usage event")
		}
		if evt.Type != "usage" {
			continue
		}
		usage, ok := evt.Data["usage"].(map[string]any)
		if !ok {
			t.Fatalf("usage event payload = %#v, want usage map", evt.Data)
		}
		if got := usage["usedTokens"]; got != 111 {
			t.Fatalf("first usage usedTokens = %#v, want 111", got)
		}
		if got := usage["contextWindow"]; got != 100000 {
			t.Fatalf("first usage contextWindow = %#v, want 100000", got)
		}
		return
	}
	t.Fatal("expected a usage event before the terminal done event")
}
