package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
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

type loopGuardProvider struct {
	calls int
}

func (p *loopGuardProvider) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("unexpected chat call")
}

func (p *loopGuardProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	p.calls++
	ch := make(chan provider.StreamChunk, 1)
	if p.calls <= 3 {
		ch <- provider.StreamChunk{
			ToolCalls: []provider.ToolCall{{
				ID:   []string{"call_repeat_1", "call_repeat_2", "call_repeat_3"}[p.calls-1],
				Type: "function",
				Function: provider.FunctionCall{
					Name:      "test_tool",
					Arguments: `{"value":"same"}`,
				},
			}},
			Done: true,
		}
		close(ch)
		return provider.NewStreamReader(ch), nil
	}
	if p.calls == 4 {
		ch <- provider.StreamChunk{Content: "final after loop guard", Done: true}
		close(ch)
		return provider.NewStreamReader(ch), nil
	}
	close(ch)
	return nil, errors.New("unexpected chat stream call")
}

const (
	reactProgressToolID     = "call_progress_1"
	reactProgressToolResult = "tool result visible to the next model call"
)

type reactProgressProvider struct {
	chatCalls           int
	streamCalls         int
	chatSawToolResult   bool
	streamSawToolResult bool
}

func (p *reactProgressProvider) Chat(_ context.Context, messages []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.chatCalls++
	switch p.chatCalls {
	case 1:
		return &provider.Response{ToolCalls: []provider.ToolCall{reactProgressToolCall()}}, nil
	case 2:
		p.chatSawToolResult = requestIncludesReactProgress(messages)
		return &provider.Response{Content: "done after tool result"}, nil
	default:
		return nil, errors.New("unexpected chat call")
	}
}

func (p *reactProgressProvider) ChatStream(_ context.Context, messages []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	if p.chatCalls >= 2 {
		ch := make(chan provider.StreamChunk, 1)
		ch <- provider.StreamChunk{Content: "done after tool result", Done: true}
		close(ch)
		return provider.NewStreamReader(ch), nil
	}

	p.streamCalls++
	ch := make(chan provider.StreamChunk, 1)
	switch p.streamCalls {
	case 1:
		ch <- provider.StreamChunk{ToolCalls: []provider.ToolCall{reactProgressToolCall()}, Done: true}
	case 2:
		p.streamSawToolResult = requestIncludesReactProgress(messages)
		ch <- provider.StreamChunk{Content: "done after tool result", Done: true}
	default:
		close(ch)
		return nil, errors.New("unexpected chat stream call")
	}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func reactProgressToolCall() provider.ToolCall {
	return provider.ToolCall{
		ID:   reactProgressToolID,
		Type: "function",
		Function: provider.FunctionCall{
			Name:      "test_tool",
			Arguments: `{"value":"progress"}`,
		},
	}
}

func requestIncludesReactProgress(messages []provider.Message) bool {
	sawAssistantToolCall := false
	sawToolResult := false
	for _, msg := range messages {
		if msg.Role == "assistant" {
			for _, tc := range msg.ToolCalls {
				if tc.ID == reactProgressToolID {
					sawAssistantToolCall = true
					break
				}
			}
		}
		if msg.Role == "tool" && msg.ToolCallID == reactProgressToolID && msg.Content == reactProgressToolResult {
			sawToolResult = true
		}
	}
	return sawAssistantToolCall && sawToolResult
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

func TestHandleMessageCarriesToolResultIntoNextModelCall(t *testing.T) {
	tmp := t.TempDir()
	prov := &reactProgressProvider{}
	ag := NewAgent(config.ResolvedAgent{
		ID:                "react-progress-agent",
		UserID:            "owner",
		Home:              filepath.Join(tmp, "home"),
		Workspace:         filepath.Join(tmp, "workspace"),
		Model:             "fake/model",
		MaxTokens:         100,
		ContextWindow:     100000,
		MaxToolIterations: 3,
	}, prov, nil, tmp)
	ag.ToolRegistry().Register("test_tool", "test tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) {
		return reactProgressToolResult, nil
	})

	reply := ag.HandleMessage(context.Background(), bus.InboundMessage{
		Channel: "test",
		ChatID:  "session-react-progress",
		UserID:  "user-1",
		Text:    "please use a tool",
	})
	if reply != "done after tool result" {
		t.Fatalf("reply = %q, want done after tool result", reply)
	}
	if !prov.streamSawToolResult {
		t.Fatal("second ChatStream request did not include the previous assistant tool call and tool result")
	}
}

func TestHandleMessageStreamCarriesToolResultIntoNextModelCall(t *testing.T) {
	tmp := t.TempDir()
	prov := &reactProgressProvider{}
	ag := NewAgent(config.ResolvedAgent{
		ID:                "react-progress-stream-agent",
		UserID:            "owner",
		Home:              filepath.Join(tmp, "home"),
		Workspace:         filepath.Join(tmp, "workspace"),
		Model:             "fake/model",
		MaxTokens:         100,
		ContextWindow:     100000,
		MaxToolIterations: 3,
	}, prov, nil, tmp)
	ag.ToolRegistry().Register("test_tool", "test tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) {
		return reactProgressToolResult, nil
	})

	sr := ag.HandleMessageStream(context.Background(), bus.InboundMessage{
		Channel: "test",
		ChatID:  "session-react-progress-stream",
		UserID:  "user-1",
		Text:    "please use a tool",
	})
	var reply strings.Builder
	for {
		chunk, ok := sr.Next()
		if !ok {
			break
		}
		reply.WriteString(chunk.Content)
	}
	if got := reply.String(); got != "done after tool result" {
		t.Fatalf("reply = %q, want done after tool result", got)
	}
	if !prov.chatSawToolResult {
		t.Fatal("second Chat request did not include the previous assistant tool call and tool result")
	}
}

func TestHandleMessageToolLoopMarksLoopGuardNotIterationCap(t *testing.T) {
	tmp := t.TempDir()
	ag := NewAgent(config.ResolvedAgent{
		ID:                "loop-guard-agent",
		UserID:            "owner",
		Home:              filepath.Join(tmp, "home"),
		Workspace:         filepath.Join(tmp, "workspace"),
		Model:             "fake/model",
		MaxTokens:         100,
		ContextWindow:     100000,
		MaxToolIterations: 200,
	}, &loopGuardProvider{}, nil, tmp)
	ag.ToolRegistry().Register("test_tool", "test tool", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) {
		return "ok", nil
	})

	events := make(chan ChatEvent, 64)
	reply := ag.HandleWebChatStream(context.Background(), "session-loop-guard", "", "user-1", "repeat a tool", nil, nil, events)
	if reply != "final after loop guard" {
		t.Fatalf("reply = %q, want final after loop guard", reply)
	}
	close(events)

	sawFinalContent := false
	for evt := range events {
		if evt.Type != "content" || evt.Data["content"] != "final after loop guard" {
			continue
		}
		sawFinalContent = true
		metadata, ok := evt.Data["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("final loop guard content missing metadata: %#v", evt.Data)
		}
		if reached, _ := metadata["iterationCapReached"].(bool); reached {
			t.Fatalf("tool loop content was marked as iteration cap reached: %#v", metadata)
		}
		if reached, _ := metadata["loopGuardReached"].(bool); !reached {
			t.Fatalf("loopGuardReached = %#v, want true", metadata["loopGuardReached"])
		}
		if got := metadata["loopGuardTool"]; got != "test_tool" {
			t.Fatalf("loopGuardTool = %#v, want test_tool", got)
		}
	}
	if !sawFinalContent {
		t.Fatal("expected final content event")
	}
}

func TestContextUsageDataSeparatesProviderUsageFromBudgetEstimate(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: strings.Repeat("s", 2000)},
		{Role: "user", Content: strings.Repeat("u", 2000)},
	}
	ag := &Agent{
		model:         "openai/fake-model",
		contextWindow: 8000,
		maxTokens:     1000,
	}

	usage := ag.contextUsageData(provider.Usage{InputTokens: 1234, CacheReadTokens: 100}, msgs, nil)

	if got := usage["usedTokens"]; got != 1334 {
		t.Fatalf("usedTokens = %#v, want provider input + cache tokens 1334", got)
	}
	if got := usage["source"]; got != "provider" {
		t.Fatalf("source = %#v, want provider", got)
	}
	if got := usage["budgetTokens"]; got != 1000 {
		t.Fatalf("budgetTokens = %#v, want raw request estimate 1000", got)
	}
}

func TestContextUsageDataUsesLongLivedCalibrationForEstimateFallback(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: strings.Repeat("u", 4000)},
	}
	ag := &Agent{
		model:         "openai/fake-model",
		contextWindow: 8000,
		maxTokens:     1000,
	}

	ag.contextUsageData(provider.Usage{InputTokens: 2000}, msgs, nil)
	usage := ag.contextUsageData(provider.Usage{}, msgs, nil)

	if got := usage["source"]; got != "estimate" {
		t.Fatalf("source = %#v, want estimate", got)
	}
	if got := usage["usedTokens"]; got != 1100 {
		t.Fatalf("usedTokens = %#v, want calibrated estimate 1100", got)
	}
	if got := usage["budgetTokens"]; got != 1100 {
		t.Fatalf("budgetTokens = %#v, want calibrated estimate 1100", got)
	}
}
