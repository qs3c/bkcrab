package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/session"
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

func TestIsContextLimitError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context length exceeded", err: errors.New("context_length_exceeded"), want: true},
		{name: "maximum context length", err: errors.New("maximum context length is 128000 tokens"), want: true},
		{name: "prompt too long", err: errors.New("prompt too long"), want: true},
		{name: "prompt is too long", err: errors.New("prompt is too long"), want: true},
		{name: "too many tokens", err: errors.New("too many tokens"), want: true},
		{name: "too many request tokens", err: errors.New("too many tokens in request"), want: true},
		{name: "input exceeds window", err: errors.New("input length exceeds context window"), want: true},
		{name: "request too large", err: errors.New("request too large"), want: true},
		{name: "rate limit", err: errors.New("rate limit exceeded"), want: false},
		{name: "rate limit tokens per minute", err: errors.New("rate limit: too many tokens per minute"), want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContextLimitError(tc.err); got != tc.want {
				t.Fatalf("isContextLimitError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestEmergencyCompactionUsesReactiveSummary(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "OLD_USER_SHOULD_DROP " + strings.Repeat("o", 160), Origin: provider.OriginUser},
		{Role: "assistant", Content: "old assistant " + strings.Repeat("a", 160)},
		{Role: "user", Content: "old middle user " + strings.Repeat("m", 160), Origin: provider.OriginUser},
		{Role: "assistant", Content: "old middle assistant " + strings.Repeat("b", 160)},
		{Role: "user", Content: "old tool user " + strings.Repeat("t", 160), Origin: provider.OriginUser},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:       "old_call",
				Type:     "function",
				Function: provider.FunctionCall{Name: "lookup", Arguments: "{}"},
			}},
		},
		{Role: "tool", ToolCallID: "old_call", Name: "lookup", Content: "old tool result"},
		{Role: "user", Content: "recent setup user " + strings.Repeat("r", 160), Origin: provider.OriginUser},
		{Role: "assistant", Content: "recent setup assistant"},
		{Role: "user", Content: "KEEP_RECENT_USER_TURN " + strings.Repeat("k", 160), Origin: provider.OriginUser},
		{
			Role: "assistant",
			ToolCalls: []provider.ToolCall{{
				ID:       "keep_call",
				Type:     "function",
				Function: provider.FunctionCall{Name: "read_file", Arguments: "{}"},
			}},
		},
		{Role: "tool", ToolCallID: "keep_call", Name: "read_file", Content: "kept tool result"},
		{Role: "assistant", Content: "final recent assistant"},
	}
	f := &flakySummarizer{}

	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeEmergency,
		Workspace:       t.TempDir(),
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   120,
		MaxOutputTokens: 20,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("summary calls = %d, want 1 for emergency compaction", f.calls)
	}
	if !out.Pruned {
		t.Fatal("expected emergency compaction to prune")
	}
	if len(out.Messages) == 0 {
		t.Fatal("expected emergency compaction output")
	}
	first := out.Messages[0]
	if first.Role != "user" {
		t.Fatalf("first role = %q, want user", first.Role)
	}
	for _, marker := range []string{"[Reactive Context Summary]", "llm summary"} {
		if !strings.Contains(first.Content, marker) {
			t.Fatalf("first message missing marker %q: %s", marker, first.Content)
		}
	}
	joined := messagesText(out.Messages)
	if strings.Contains(joined, "OLD_USER_SHOULD_DROP") {
		t.Fatalf("oldest user turn was not dropped:\n%s", joined)
	}
	if !strings.Contains(joined, "KEEP_RECENT_USER_TURN") {
		t.Fatalf("recent user turn was not preserved:\n%s", joined)
	}
	assertValidToolPairs(t, out.Messages)
	if len(out.Messages) > 1 && out.Messages[1].Role == "tool" {
		t.Fatalf("preserved tail starts with orphan tool result: %+v", out.Messages[1])
	}
}

func TestEmergencyCompactionPrunesShortHistory(t *testing.T) {
	cases := []struct {
		name string
		msgs []provider.Message
	}{
		{
			name: "two messages",
			msgs: []provider.Message{
				{Role: "assistant", Content: "OLD_ASSISTANT_SHOULD_DROP"},
				{Role: "user", Content: "KEEP_RECENT_USER_TURN", Origin: provider.OriginUser},
			},
		},
		{
			name: "three messages",
			msgs: []provider.Message{
				{Role: "user", Content: "OLD_USER_SHOULD_DROP", Origin: provider.OriginUser},
				{Role: "assistant", Content: "old assistant should drop"},
				{Role: "user", Content: "KEEP_RECENT_USER_TURN", Origin: provider.OriginUser},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := CompactMessagesWithOptions(tc.msgs, CompactOptions{
				Mode:            CompactModeEmergency,
				Workspace:       t.TempDir(),
				ContextWindow:   120,
				MaxOutputTokens: 20,
			})
			if err != nil {
				t.Fatalf("compact: %v", err)
			}
			if !out.Pruned {
				t.Fatal("expected short emergency compaction to prune")
			}
			if len(out.Messages) < 2 {
				t.Fatalf("expected marker plus recent history, got %+v", out.Messages)
			}
			assertContainsAll(t, out.Messages[0].Content,
				"[Reactive Context Summary]",
				"deterministic fallback",
			)
			tailText := messagesText(out.Messages[1:])
			if strings.Contains(tailText, "OLD_") {
				t.Fatalf("old message was preserved in raw tail:\n%s", tailText)
			}
			joined := messagesText(out.Messages)
			if !strings.Contains(joined, "KEEP_RECENT_USER_TURN") {
				t.Fatalf("recent user turn was not preserved:\n%s", joined)
			}
			assertValidToolPairs(t, out.Messages)
		})
	}
}

func TestEmergencyRetryRetriesWithinSameIteration(t *testing.T) {
	mgr := session.NewManager(t.TempDir())
	sess := mgr.Get("test", "", "chat", "")
	sess.ReplaceMessages([]provider.Message{
		{Role: "user", Content: "OLD_USER_SHOULD_DROP", Origin: provider.OriginUser},
		{Role: "assistant", Content: "old assistant should drop"},
		{Role: "user", Content: "KEEP_RECENT_USER_TURN", Origin: provider.OriginUser},
	})

	a := &Agent{
		homePath:      t.TempDir(),
		model:         "fake-model",
		contextWindow: 120,
		maxTokens:     20,
	}
	overhead := []provider.Message{{Role: "system", Content: "system prompt"}}
	messages := compactionRequestMessages(sess.GetMessages(), overhead)
	attempts := 0

	resp, rebuilt, retried, err := a.callLLMWithEmergencyRetry(sess, overhead, nil, messages, nil, false, func(request []provider.Message, tools []provider.Tool) (*provider.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("too many tokens")
		}
		joined := messagesText(request)
		if !strings.Contains(joined, "[Reactive Context Summary]") {
			t.Fatalf("retry request missing emergency summary:\n%s", joined)
		}
		if !strings.Contains(joined, "KEEP_RECENT_USER_TURN") {
			t.Fatalf("retry request dropped recent user turn:\n%s", joined)
		}
		return &provider.Response{Content: "ok"}, nil
	})
	if err != nil {
		t.Fatalf("retry call returned error: %v", err)
	}
	if resp == nil || resp.Content != "ok" {
		t.Fatalf("response = %+v, want ok content", resp)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want original call plus one retry", attempts)
	}
	if !retried {
		t.Fatal("expected emergency retry to be reported")
	}
	if !strings.Contains(messagesText(rebuilt), "[Reactive Context Summary]") {
		t.Fatalf("rebuilt canonical messages missing reactive summary:\n%s", messagesText(rebuilt))
	}
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

func messagesText(messages []provider.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		if msg.Content != "" {
			b.WriteString(msg.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func assertValidToolPairs(t *testing.T, messages []provider.Message) {
	t.Helper()

	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == "tool" {
			t.Fatalf("tool message at index %d has no preceding assistant tool_calls: %+v", i, msg)
		}
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			continue
		}

		expected := make(map[string]struct{}, len(msg.ToolCalls))
		for _, tc := range msg.ToolCalls {
			expected[tc.ID] = struct{}{}
		}

		j := i + 1
		for ; j < len(messages) && messages[j].Role == "tool"; j++ {
			if _, ok := expected[messages[j].ToolCallID]; !ok {
				t.Fatalf("unexpected tool result at index %d for assistant index %d: %+v", j, i, messages[j])
			}
			delete(expected, messages[j].ToolCallID)
		}
		if len(expected) > 0 {
			t.Fatalf("assistant tool_calls at index %d are missing tool results: %+v", i, expected)
		}
		i = j - 1
	}
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
