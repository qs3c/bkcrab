package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
)

// The provider checks the actual requests after three distinct failures, a
// fourth failure, recovery, and another three failures. No model API is used.
type failurePolicyProvider struct {
	t     *testing.T
	calls int
}

func (p *failurePolicyProvider) Chat(_ context.Context, messages []provider.Message, tools []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	p.calls++
	if p.calls > 9 {
		return nil, errors.New("unexpected extra request")
	}
	if len(tools) == 0 {
		p.t.Errorf("tools disabled at request %d", p.calls)
	}
	warnings := 0
	for _, m := range messages {
		if strings.Contains(m.Content, "Stop calling tools") {
			p.t.Error("obsolete all-failed stop instruction")
		}
		if strings.Contains(m.Content, "Tools remain available") {
			warnings++
		}
	}
	want := 0
	if p.calls >= 4 {
		want = 1
	}
	if p.calls >= 9 {
		want = 2
	}
	if warnings != want {
		p.t.Errorf("request %d: reminders=%d, want %d", p.calls, warnings, want)
	}
	if p.calls == 9 {
		return &provider.Response{Content: "recovered successfully"}, nil
	}
	return &provider.Response{ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", p.calls), Type: "function", Function: provider.FunctionCall{Name: "test_tool", Arguments: fmt.Sprintf(`{"step":%d}`, p.calls)}}}}, nil
}

func (p *failurePolicyProvider) ChatStream(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, tokens int, temp float64) (*provider.StreamReader, error) {
	// HandleMessageStream asks for a final stream after its ninth Chat response.
	if p.calls == 9 {
		ch := make(chan provider.StreamChunk, 1)
		ch <- provider.StreamChunk{Content: "recovered successfully", Done: true}
		close(ch)
		return provider.NewStreamReader(ch), nil
	}
	r, err := p.Chat(ctx, messages, tools, model, tokens, temp)
	if err != nil {
		return nil, err
	}
	ch := make(chan provider.StreamChunk, 1)
	ch <- provider.StreamChunk{Content: r.Content, ToolCalls: r.ToolCalls, Done: true}
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func policyTestAgent(t *testing.T, p provider.Provider) *Agent {
	t.Helper()
	tmp := t.TempDir()
	return NewAgent(config.ResolvedAgent{ID: "policy", UserID: "owner", Home: filepath.Join(tmp, "home"), Workspace: filepath.Join(tmp, "workspace"), Model: "fake/model", MaxTokens: 100, ContextWindow: 100000, MaxToolIterations: 12}, p, nil, tmp)
}

func TestFailuresRemindWithoutDisablingTools(t *testing.T) {
	for _, mode := range []string{"main", "stream", "subagent"} {
		t.Run(mode, func(t *testing.T) {
			p := &failurePolicyProvider{t: t}
			a := policyTestAgent(t, p)
			executed := 0
			a.ToolRegistry().Register("test_tool", "test", map[string]any{"type": "object"}, func(_ context.Context, args json.RawMessage) (string, error) {
				executed++
				if executed == 5 {
					return "dependency fixed", nil
				}
				if executed == 4 {
					return "HTTP 503 unavailable", nil // Failure can be encoded in content without a Go error.
				}
				return "", fmt.Errorf("different environment failure %s", args)
			})
			var reply string
			if mode == "subagent" {
				var err error
				reply, err = a.runSubagentLoop(context.Background(), "fix the environment", 12)
				if err != nil {
					t.Fatal(err)
				}
			} else if mode == "stream" {
				sr := a.HandleMessageStream(context.Background(), bus.InboundMessage{Channel: "test", ChatID: "policy-stream", UserID: "owner", Text: "fix the environment"})
				for {
					chunk, ok := sr.Next()
					if !ok {
						break
					}
					reply += chunk.Content
				}
				if err := sr.Err(); err != nil {
					t.Fatal(err)
				}
			} else {
				reply = a.HandleWebChatStream(context.Background(), "policy-session", "", "owner", "fix the environment", nil, nil, make(chan ChatEvent, 256))
			}
			if reply != "recovered successfully" || executed != 8 {
				t.Fatalf("reply=%q executions=%d", reply, executed)
			}
		})
	}
}

type disabledRecoveryProvider struct {
	calls       int
	native      bool
	streamFinal bool
}

func (p *disabledRecoveryProvider) Chat(ctx context.Context, msgs []provider.Message, tools []provider.Tool, model string, tokens int, temp float64) (*provider.Response, error) {
	sr, err := p.ChatStream(ctx, msgs, tools, model, tokens, temp)
	if err != nil {
		return nil, err
	}
	c, _ := sr.Next()
	return &provider.Response{Content: c.Content, ToolCalls: c.ToolCalls}, nil
}
func (p *disabledRecoveryProvider) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	if p.streamFinal && p.calls == 4 {
		ch := make(chan provider.StreamChunk, 1)
		ch <- provider.StreamChunk{Content: "Partial result.", Done: true}
		close(ch)
		return provider.NewStreamReader(ch), nil
	}
	p.calls++
	ch := make(chan provider.StreamChunk, 1)
	c := provider.StreamChunk{Done: true}
	if p.calls <= 3 {
		c.ToolCalls = []provider.ToolCall{{ID: fmt.Sprintf("repeat_%d", p.calls), Type: "function", Function: provider.FunctionCall{Name: "test_tool", Arguments: `{"value":"same"}`}}}
	} else if p.calls == 4 {
		c.Content = "Partial result."
		if p.native {
			c.ToolCalls = []provider.ToolCall{{ID: "bypass", Type: "function", Function: provider.FunctionCall{Name: "test_tool", Arguments: `{"value":"different"}`}}}
		} else {
			c.Content += `<DSML><invoke name="test_tool"><parameter name="value">different</parameter></invoke></DSML>`
		}
	} else {
		return nil, errors.New("guard was bypassed")
	}
	ch <- c
	close(ch)
	return provider.NewStreamReader(ch), nil
}

func TestDisabledToolsCannotBeRecoveredOrExecuted(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/native=%v", stream, native), func(t *testing.T) {
				p := &disabledRecoveryProvider{native: native, streamFinal: stream}
				a := policyTestAgent(t, p)
				executed := 0
				a.ToolRegistry().Register("test_tool", "test", map[string]any{"type": "object"}, func(context.Context, json.RawMessage) (string, error) { executed++; return "ok", nil })
				var reply string
				if stream {
					sr := a.HandleMessageStream(context.Background(), bus.InboundMessage{Channel: "test", ChatID: "disabled-stream", UserID: "owner", Text: "repeat"})
					for {
						chunk, ok := sr.Next()
						if !ok {
							break
						}
						reply += chunk.Content
					}
					if err := sr.Err(); err != nil {
						t.Fatal(err)
					}
				} else {
					reply = a.HandleWebChatStream(context.Background(), "disabled-session", "", "owner", "repeat", nil, nil, make(chan ChatEvent, 256))
				}
				if executed != 2 || p.calls != 4 || reply != "Partial result." {
					t.Fatalf("executions=%d requests=%d reply=%q", executed, p.calls, reply)
				}
			})
		}
	}
}
