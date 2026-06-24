package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
)

type countingSummarizer struct {
	calls int
}

func (f *countingSummarizer) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	f.calls++
	return &provider.Response{Content: "summary"}, nil
}

func (f *countingSummarizer) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

func TestProactiveCompactionUsesPercentOfContextWindow(t *testing.T) {
	msgs := make([]provider.Message, 0, 40)
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", 70)},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", 70)},
		)
	}

	opts := CompactOptions{
		Mode:            CompactModeProactive,
		Workspace:       t.TempDir(),
		Model:           "fake-model",
		ContextWindow:   1200,
		MaxOutputTokens: 400,
		TriggerPercent:  75,
		TargetPercent:   55,
	}
	normalized := normalizeCompactOptions(opts)
	requestTokens := EstimateRequestTokens(msgs, nil)
	if requestTokens <= compactTriggerLimit(normalized) {
		t.Fatalf("fixture broken: request tokens = %d, want above subtracting-output trigger %d", requestTokens, compactTriggerLimit(normalized))
	}
	if requestTokens >= percentOf(normalized.ContextWindow, normalized.TriggerPercent) {
		t.Fatalf("fixture broken: request tokens = %d, want below full-window trigger %d", requestTokens, percentOf(normalized.ContextWindow, normalized.TriggerPercent))
	}

	f := &countingSummarizer{}
	opts.Provider = f
	out, err := CompactMessagesWithOptions(msgs, opts)
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("expected proactive compaction to trigger")
	}
	if f.calls != 1 {
		t.Fatalf("summary calls = %d, want 1", f.calls)
	}
}

func TestCompactInputBudgetUsesMinimumWhenOutputConsumesWindow(t *testing.T) {
	opts := normalizeCompactOptions(CompactOptions{
		ContextWindow:   1000,
		MaxOutputTokens: 1200,
		TriggerPercent:  75,
		TargetPercent:   55,
	})

	if got := compactInputBudget(opts); got != 1 {
		t.Fatalf("input budget = %d, want 1 when max output consumes context window", got)
	}
}

func TestCompactWithProgressEmitsEventsForEmergencyCompaction(t *testing.T) {
	msgs := make([]provider.Message, 0, 24)
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", 200), Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", 200), Origin: provider.OriginUser},
		)
	}

	events := make(chan ChatEvent, 4)
	ctx := ContextWithChatEvents(context.Background(), events)
	a := &Agent{}
	out, err := a.compactWithProgress(ctx, msgs, CompactOptions{
		Mode:            CompactModeEmergency,
		Workspace:       t.TempDir(),
		ContextWindow:   1200,
		MaxOutputTokens: 400,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if out == nil || !out.Pruned {
		t.Fatal("expected emergency compaction to change history")
	}

	var got []bool
	for {
		select {
		case evt := <-events:
			if evt.Type == "compaction" {
				active, ok := evt.Data["active"].(bool)
				if !ok {
					t.Fatalf("compaction event active = %#v, want bool", evt.Data["active"])
				}
				got = append(got, active)
			}
		default:
			if len(got) != 2 || !got[0] || got[1] {
				t.Fatalf("compaction active events = %v, want [true false]", got)
			}
			return
		}
	}
}

func TestCompactWithProgressEmitsEstimatedUsageAfterCompaction(t *testing.T) {
	msgs := make([]provider.Message, 0, 24)
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", 200), Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", 200), Origin: provider.OriginUser},
		)
	}

	events := make(chan ChatEvent, 8)
	ctx := ContextWithChatEvents(context.Background(), events)
	a := &Agent{
		model:         "fake-model",
		contextWindow: 1200,
		maxTokens:     400,
	}
	out, err := a.compactWithProgress(ctx, msgs, CompactOptions{
		Mode:            CompactModeEmergency,
		Workspace:       t.TempDir(),
		ContextWindow:   1200,
		MaxOutputTokens: 400,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if out == nil || !out.Pruned {
		t.Fatal("expected emergency compaction to change history")
	}

	var sawInactive bool
	for {
		select {
		case evt := <-events:
			if evt.Type == "compaction" && evt.Data["active"] == false {
				sawInactive = true
				continue
			}
			if evt.Type != "usage" {
				continue
			}
			if !sawInactive {
				t.Fatal("usage event arrived before compaction inactive event")
			}
			usage, ok := evt.Data["usage"].(map[string]any)
			if !ok {
				t.Fatalf("usage event payload = %#v, want usage map", evt.Data)
			}
			if got := usage["source"]; got != "estimate" {
				t.Fatalf("usage source = %#v, want estimate", got)
			}
			used, ok := usage["usedTokens"].(int)
			if !ok || used <= 0 {
				t.Fatalf("usedTokens = %#v, want positive int estimate", usage["usedTokens"])
			}
			if got := usage["budgetTokens"]; got != used {
				t.Fatalf("budgetTokens = %#v, want same estimate as usedTokens %d", got, used)
			}
			return
		default:
			t.Fatal("expected usage event after compaction")
		}
	}
}

func TestTriggeredCompactionDoesNotClaimPrunedWhenHistoryCannotChange(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "short"},
		{Role: "assistant", Content: "also short"},
	}
	overhead := []provider.Message{
		{Role: "system", Content: strings.Repeat("s", 8000)},
	}

	f := &countingSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:             CompactModeProactive,
		Workspace:        t.TempDir(),
		Provider:         f,
		Model:            "fake-model",
		ContextWindow:    2000,
		MaxOutputTokens:  200,
		TriggerPercent:   75,
		TargetPercent:    55,
		OverheadMessages: overhead,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if out.Pruned {
		t.Fatal("short triggered compaction claimed Pruned=true even though no prune or summary occurred")
	}
	if f.calls != 0 {
		t.Fatalf("summary calls = %d, want 0 for short history", f.calls)
	}
	if len(out.Messages) != len(msgs) {
		t.Fatalf("message count = %d, want unchanged %d", len(out.Messages), len(msgs))
	}
}

func TestEstimateTokensIncludesTextContentParts(t *testing.T) {
	msgs := []provider.Message{
		{
			Role: "user",
			ContentParts: []provider.ContentPart{
				{Type: "text", Text: strings.Repeat("p", 1200)},
			},
		},
	}

	if got := EstimateTokens(msgs); got != 300 {
		t.Fatalf("estimated tokens = %d, want 300 from text content parts", got)
	}
}

func TestProactiveCompactionIncludesRequestOverheadAndToolDefs(t *testing.T) {
	msgs := make([]provider.Message, 0, 24)
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("x", 100)},
			provider.Message{Role: "assistant", Content: strings.Repeat("y", 100)},
		)
	}
	overhead := []provider.Message{
		{Role: "system", Content: strings.Repeat("s", 1800)},
	}
	tools := []provider.Tool{
		{
			Type: "function",
			Function: provider.ToolFunction{
				Name:        "large_context_tool",
				Description: strings.Repeat("tool ", 1700),
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	f := &countingSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:             CompactModeProactive,
		Workspace:        t.TempDir(),
		Provider:         f,
		Model:            "fake-model",
		ContextWindow:    4600,
		MaxOutputTokens:  600,
		TriggerPercent:   75,
		TargetPercent:    55,
		OverheadMessages: overhead,
		ToolDefs:         tools,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("expected overhead and tool definitions to push request over the proactive threshold")
	}
	if f.calls != 1 {
		t.Fatalf("summary calls = %d, want 1 so compaction actually changed history", f.calls)
	}
	if len(out.Messages) >= len(msgs) {
		t.Fatalf("message count = %d, want less than original %d after summary", len(out.Messages), len(msgs))
	}

	if withoutToolDefs := EstimateRequestTokens(append(append([]provider.Message{}, overhead...), msgs...), nil); withoutToolDefs >= compactTriggerLimit(normalizeCompactOptions(CompactOptions{
		ContextWindow:   4600,
		MaxOutputTokens: 600,
		TriggerPercent:  75,
		TargetPercent:   55,
	})) {
		t.Fatalf("fixture broken: messages plus overhead should stay below trigger without tool definitions")
	}
}
