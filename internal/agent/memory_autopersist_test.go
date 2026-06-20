package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

type recordingMemoryProvider struct {
	prompt string
}

func (p *recordingMemoryProvider) Chat(_ context.Context, msgs []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	if len(msgs) > 0 {
		p.prompt = msgs[0].Content
	}
	return &provider.Response{Content: `{"memory_facts":[],"user_notes":[]}`}, nil
}

func (p *recordingMemoryProvider) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

func TestAutoPersistMemoryPromptUsesOnlyUserAndAssistantText(t *testing.T) {
	prov := &recordingMemoryProvider{}
	mem := NewMemory(t.TempDir())

	groups := []store.TurnGroup{{
		SessionKey: "sess1",
		Messages: []store.SessionMessage{
			{Role: "system", Content: "SYSTEM_SHOULD_NOT_APPEAR"},
			{Role: "user", Content: "USER_SHOULD_APPEAR"},
			{Role: "assistant", Content: "ASSISTANT_SHOULD_APPEAR"},
			{Role: "assistant", Content: ""},
			{Role: "tool", Content: "TOOL_SHOULD_NOT_APPEAR"},
			{Role: "user", Content: "GOAL_CONTEXT_SHOULD_NOT_APPEAR", Origin: provider.OriginGoalContext},
		},
	}}

	if err := AutoPersistMemory(context.Background(), mem, prov, "fake-model", groups); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	for _, want := range []string{"USER_SHOULD_APPEAR", "ASSISTANT_SHOULD_APPEAR"} {
		if !strings.Contains(prov.prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prov.prompt)
		}
	}
	for _, forbidden := range []string{
		"SYSTEM_SHOULD_NOT_APPEAR",
		"TOOL_SHOULD_NOT_APPEAR",
		"GOAL_CONTEXT_SHOULD_NOT_APPEAR",
		"[assistant]: \n",
	} {
		if strings.Contains(prov.prompt, forbidden) {
			t.Fatalf("prompt included %q:\n%s", forbidden, prov.prompt)
		}
	}
}
