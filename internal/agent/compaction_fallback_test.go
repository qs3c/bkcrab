package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
)

type flakySummarizer struct {
	failuresBeforeSuccess int
	calls                 int
}

func (f *flakySummarizer) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	f.calls++
	if f.calls <= f.failuresBeforeSuccess {
		return nil, errors.New("summary failed")
	}
	return &provider.Response{Content: "llm summary"}, nil
}

func (f *flakySummarizer) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

func longConversation() []provider.Message {
	msgs := make([]provider.Message, 0, 70)
	for i := 0; i < 25; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "real user message in older history", Origin: provider.OriginUser},
			provider.Message{Role: "user", Content: "RUNTIME_GOAL_CONTEXT_SHOULD_NOT_APPEAR", Origin: provider.OriginGoalContext},
			provider.Message{Role: "assistant", Content: "real assistant reply in older history", Origin: provider.OriginUser},
		)
	}
	return msgs
}

func TestSummaryRetriesThenSucceeds(t *testing.T) {
	f := &flakySummarizer{failuresBeforeSuccess: 2}

	out, err := compressOlderMessages(longConversation(), CompactOptions{Provider: f, Model: "fake-model"})
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("summary calls = %d, want 3", f.calls)
	}
	if len(out) == 0 || !strings.Contains(out[0].Content, "llm summary") {
		t.Fatalf("first output message should contain LLM summary, got: %+v", out)
	}
}

func TestSummaryFallsBackAfterThreeFailures(t *testing.T) {
	f := &flakySummarizer{failuresBeforeSuccess: 3}

	out, err := compressOlderMessages(longConversation(), CompactOptions{Provider: f, Model: "fake-model"})
	if err != nil {
		t.Fatalf("compress should fall back without error: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("summary calls = %d, want 3", f.calls)
	}
	if len(out) == 0 || !strings.Contains(out[0].Content, "deterministic fallback") {
		t.Fatalf("first output message should contain deterministic fallback, got: %+v", out)
	}
	if strings.Contains(out[0].Content, "RUNTIME_GOAL_CONTEXT_SHOULD_NOT_APPEAR") {
		t.Fatalf("fallback summary included goal_context content: %s", out[0].Content)
	}
}
