package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/provider"
)

// fakeSummarizer captures the summarize-call prompt so tests can
// assert what compaction actually ships off to the LLM. The
// returned Response is whatever the test wants — we don't care
// about the summary content, only the input.
type fakeSummarizer struct {
	gotSummaryRequest string
}

func (f *fakeSummarizer) Chat(_ context.Context, msgs []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	// compressOlderMessages builds the user-role prompt as the
	// second message; the older-history text lives in its Content
	// after the "Summarize this conversation:\n\n" prefix.
	if len(msgs) >= 2 {
		f.gotSummaryRequest = msgs[1].Content
	}
	return &provider.Response{Content: "[fake summary]"}, nil
}

func (f *fakeSummarizer) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	return nil, nil
}

type failingSummarizer struct {
	calls int
}

func (f *failingSummarizer) Chat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	f.calls++
	return nil, errors.New("prompt too long")
}

func (f *failingSummarizer) ChatStream(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.StreamReader, error) {
	return nil, nil
}

// TestCompactionDropsGoalContextFromSummary pins design §5.3 (b):
// when compaction folds older messages, runtime-injected
// goal_context messages must be excluded from the summary — their
// content is synthetic audit scaffolding and the latest one is
// already preserved verbatim in the recent tail.
func TestCompactionDropsGoalContextFromSummary(t *testing.T) {
	// Build a history that's longer than PruneTurnAge so
	// compression actually runs. Interleave goal_context messages
	// among real user/assistant turns.
	var msgs []provider.Message
	for i := 0; i < PruneTurnAge+5; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "real user message", Origin: provider.OriginUser},
			provider.Message{Role: "user", Content: "RUNTIME_AUDIT_PROMPT", Origin: provider.OriginGoalContext},
			provider.Message{Role: "assistant", Content: "real assistant reply", Origin: provider.OriginUser},
		)
	}

	f := &fakeSummarizer{}
	out, err := compressOlderMessages(msgs, CompactOptions{Provider: f, Model: "fake-model"})
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if !strings.Contains(f.gotSummaryRequest, "real user message") {
		t.Errorf("summary input lost real user content: %s", f.gotSummaryRequest)
	}
	if strings.Contains(f.gotSummaryRequest, "RUNTIME_AUDIT_PROMPT") {
		t.Errorf("summary input included runtime-injected goal_context — should have been filtered:\n%s",
			f.gotSummaryRequest)
	}
	// The recent tail must still carry whatever was there; in
	// particular if the tail contained a goal_context the model
	// still needs it for the next audit.
	tailHasContext := false
	for _, m := range out[1:] /* skip the summary prepended at [0] */ {
		if m.Origin == provider.OriginGoalContext {
			tailHasContext = true
			break
		}
	}
	if !tailHasContext {
		t.Error("recent tail should still carry the live goal_context message")
	}
}

// TestCompactionPreservesContentWhenShortCircuits: when the input
// is already under PruneTurnAge, compressOlderMessages returns it
// unchanged. Goal_context filtering shouldn't change that fast path.
func TestCompactionPreservesContentWhenShortCircuits(t *testing.T) {
	in := []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	}
	out, err := compressOlderMessages(in, CompactOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("short input should pass through; got %d messages", len(out))
	}
}

func TestManualCompactionRunsBelowProactiveThreshold(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "context turn", Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: "assistant reply", Origin: provider.OriginUser},
		)
	}
	f := &fakeSummarizer{}

	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:              CompactModeManual,
		Workspace:         t.TempDir(),
		Provider:          f,
		Model:             "fake-model",
		ContextWindow:     1000000,
		MaxOutputTokens:   1000,
		Focus:             "focus on filesystem changes",
		MinTailTurns:      2,
		SummaryMaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("manual compaction should force a checkpoint summary")
	}
	if !strings.Contains(f.gotSummaryRequest, "Manual compaction focus:\nfocus on filesystem changes") {
		t.Fatalf("manual focus missing from summary request: %s", f.gotSummaryRequest)
	}
}

func TestEmergencyCompactionUsesReactiveSummaryAndFifteenMessageTail(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 20; i++ {
		suffix := string(rune('a' + i))
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "user turn " + suffix, Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: "assistant reply " + suffix, Origin: provider.OriginUser},
		)
	}

	f := &fakeSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeEmergency,
		Workspace:       t.TempDir(),
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   1000000,
		MaxOutputTokens: 1000,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("emergency compaction should produce a compacted message set")
	}
	if len(out.Messages) != 17 {
		t.Fatalf("message count = %d, want summary + 16-message dynamic tail", len(out.Messages))
	}
	if !strings.HasPrefix(out.Messages[0].Content, "[Reactive Context Summary]\n[fake summary]") {
		t.Fatalf("first message should be a reactive summary, got: %q", out.Messages[0].Content)
	}
	if !strings.Contains(f.gotSummaryRequest, "user turn a") {
		t.Fatalf("summary request lost older content: %s", f.gotSummaryRequest)
	}
	if strings.Contains(f.gotSummaryRequest, "user turn t") {
		t.Fatalf("summary request included preserved tail content: %s", f.gotSummaryRequest)
	}
	if got := out.Messages[1].Content; got != "user turn m" {
		t.Fatalf("tail should start at the closest complete turn to 15 messages, got %q", got)
	}
}

func TestEmergencyCompactionFallsBackToDeterministicSummaryWithFifteenMessageTail(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 20; i++ {
		suffix := string(rune('a' + i))
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "user turn " + suffix, Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: "assistant reply " + suffix, Origin: provider.OriginUser},
		)
	}

	f := &failingSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeEmergency,
		Workspace:       t.TempDir(),
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   1000000,
		MaxOutputTokens: 1000,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if f.calls != 1 {
		t.Fatalf("emergency summary attempts = %d, want 1", f.calls)
	}
	if len(out.Messages) != 17 {
		t.Fatalf("message count = %d, want fallback summary + 16-message dynamic tail", len(out.Messages))
	}
	assertContainsAll(t, out.Messages[0].Content,
		"[Reactive Context Summary]",
		"deterministic fallback",
		"user turn a",
	)
	if got := out.Messages[1].Content; got != "user turn m" {
		t.Fatalf("tail should use the emergency dynamic tail, got %q", got)
	}
}

func TestEmergencyCompactionSummarizesSingleHugeTurnWithoutTail(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "huge turn request", Origin: provider.OriginUser},
	}
	for i := 1; i < 100; i++ {
		msgs = append(msgs, provider.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("assistant segment %03d", i),
			Origin:  provider.OriginUser,
		})
	}

	f := &fakeSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeEmergency,
		Workspace:       t.TempDir(),
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   1000000,
		MaxOutputTokens: 1000,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("message count = %d, want summary only with zero tail", len(out.Messages))
	}
	if !strings.Contains(f.gotSummaryRequest, "huge turn request") ||
		!strings.Contains(f.gotSummaryRequest, "assistant segment 099") {
		t.Fatalf("summary request should cover the whole huge turn:\n%s", f.gotSummaryRequest)
	}
}

func TestCompactionTailStartTargetsTwentyMessagesWithCompleteTurns(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 12; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "user turn", Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: "assistant reply", Origin: provider.OriginUser},
		)
	}

	got := compactionTailStart(msgs, CompactOptions{})
	if got != 4 {
		t.Fatalf("tail start = %d, want 4 to preserve last 10 two-message turns", got)
	}
	if tailLen := len(msgs) - got; tailLen != DefaultTailTargetMessages {
		t.Fatalf("tail messages = %d, want %d", tailLen, DefaultTailTargetMessages)
	}
}

func TestCompactionTailStartRelaxesMinimumTurnsWhenItWouldBlockCompression(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "first user", Origin: provider.OriginUser},
		{Role: "assistant", Content: "first assistant", Origin: provider.OriginUser},
		{Role: "user", Content: "second user", Origin: provider.OriginUser},
		{Role: "assistant", Content: "second assistant", Origin: provider.OriginUser},
	}

	got := compactionTailStart(msgs, CompactOptions{
		TailTargetMessages: 2,
		MinTailTurns:       2,
	})
	if got != 2 {
		t.Fatalf("tail start = %d, want 2 after relaxing the soft minimum turn preference", got)
	}
}

func TestCompactionTailStartUsesZeroTailWhenSingleTurnExceedsTarget(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "single huge turn", Origin: provider.OriginUser},
	}
	for i := 1; i < 100; i++ {
		msgs = append(msgs, provider.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("assistant segment %03d", i),
			Origin:  provider.OriginUser,
		})
	}

	got := compactionTailStart(msgs, CompactOptions{
		TailTargetMessages: 20,
		ContextWindow:      1000,
		TargetPercent:      55,
	})
	if got != len(msgs) {
		t.Fatalf("tail start = %d, want %d to summarize the whole oversized turn", got, len(msgs))
	}
}

func TestCompactionTailStartUsesZeroTailWhenSingleMessageExceedsTarget(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: strings.Repeat("large message ", 200), Origin: provider.OriginUser},
	}

	got := compactionTailStart(msgs, CompactOptions{
		TailTargetMessages: 20,
		ContextWindow:      1000,
		TargetPercent:      55,
	})
	if got != len(msgs) {
		t.Fatalf("tail start = %d, want %d to summarize the whole oversized message", got, len(msgs))
	}
}

func TestCompactionTailStartUsesFewerTurnsWhenRecentTurnsAreToolHeavy(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 4; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "old user", Origin: provider.OriginUser},
			provider.Message{Role: "assistant", Content: "old assistant", Origin: provider.OriginUser},
		)
	}
	for i := 0; i < 3; i++ {
		msgs = append(msgs, toolHeavyTurn("recent_call")...)
	}

	got := compactionTailStart(msgs, CompactOptions{})
	tailTurns := realUserTurns(msgs[got:])
	if tailTurns != 2 {
		t.Fatalf("tail real user turns = %d, want 2 for tool-heavy recent turns", tailTurns)
	}
	if tailLen := len(msgs) - got; tailLen != 18 {
		t.Fatalf("tail messages = %d, want 18 as closest complete-turn tail to 20", tailLen)
	}
}

func TestPruneOldToolResultsUsesDynamicTurnAwareTail(t *testing.T) {
	oldBody := strings.Repeat("OLD_BODY ", 80)
	recentBody := strings.Repeat("RECENT_BODY ", 80)
	msgs := []provider.Message{
		{Role: "user", Content: "old user", Origin: provider.OriginUser},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "old_call", Type: "function", Function: provider.FunctionCall{Name: "lookup", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "old_call", Name: "lookup", Content: oldBody},
		{Role: "user", Content: "recent user", Origin: provider.OriginUser},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "recent_call", Type: "function", Function: provider.FunctionCall{Name: "lookup", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "recent_call", Name: "lookup", Content: recentBody},
		{Role: "user", Content: "current user", Origin: provider.OriginUser},
		{Role: "assistant", Content: "current assistant", Origin: provider.OriginUser},
	}

	got, changed := pruneOldToolResultsWithChange(msgs, CompactOptions{
		TailTargetMessages: 5,
		MinTailTurns:       2,
	})
	if !changed {
		t.Fatal("expected old tool result before dynamic tail to be pruned")
	}
	if got[2].Content == oldBody || !strings.Contains(got[2].Content, "[Tool Result Summary]") {
		t.Fatalf("old tool result was not summarized: %s", got[2].Content)
	}
	if !strings.Contains(got[5].Content, "RECENT_BODY") {
		t.Fatalf("recent tool result inside dynamic tail was pruned: %s", got[5].Content)
	}
}

func toolHeavyTurn(callPrefix string) []provider.Message {
	return []provider.Message{
		{Role: "user", Content: "tool-heavy user", Origin: provider.OriginUser},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: callPrefix + "_1", Type: "function", Function: provider.FunctionCall{Name: "lookup", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: callPrefix + "_1", Name: "lookup", Content: "tool result"},
		{Role: "assistant", Content: "tool result noted", Origin: provider.OriginUser},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: callPrefix + "_2", Type: "function", Function: provider.FunctionCall{Name: "read", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: callPrefix + "_2", Name: "read", Content: "tool result"},
		{Role: "assistant", Content: "second result noted", Origin: provider.OriginUser},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: callPrefix + "_3", Type: "function", Function: provider.FunctionCall{Name: "write", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: callPrefix + "_3", Name: "write", Content: "tool result"},
	}
}

func realUserTurns(messages []provider.Message) int {
	count := 0
	for _, msg := range messages {
		if msg.Role == "user" && msg.Origin == provider.OriginUser {
			count++
		}
	}
	return count
}

// --- safeCompactionCutoff coverage ---
//
// The cutoff guard is the load-bearing fix for the OpenAI 400
// "Messages with role 'tool' must be a response to a preceding
// message with 'tool_calls'" — exhaustively pin its behavior, with
// a final end-to-end assertion that the compressed output never
// starts with a tool message.

func TestSafeCompactionCutoffAdvancesPastLeadingTool(t *testing.T) {
	// History tail looks like [..., assistant(tool_calls), tool, tool, assistant_text, user]
	// with cutoff landing on the first `tool` — must advance to the
	// `assistant_text` position so the resulting tail is valid.
	msgs := []provider.Message{
		{Role: "user", Content: "ask"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1", Content: "r1"},
		{Role: "tool", ToolCallID: "t2", Content: "r2"},
		{Role: "assistant", Content: "ok"},
		{Role: "user", Content: "next"},
	}
	got := safeCompactionCutoff(msgs, 2) // points at first "tool"
	if msgs[got].Role != "assistant" || msgs[got].Content != "ok" {
		t.Errorf("expected cutoff to land on assistant_text; landed on %+v", msgs[got])
	}
}

func TestSafeCompactionCutoffNoAdvanceOnUser(t *testing.T) {
	msgs := []provider.Message{
		{Role: "assistant", Content: "x"},
		{Role: "user", Content: "y"},
	}
	if got := safeCompactionCutoff(msgs, 1); got != 1 {
		t.Errorf("cutoff = %d, want 1 (user is a valid tail start)", got)
	}
}

func TestSafeCompactionCutoffNoAdvanceOnAssistant(t *testing.T) {
	// An assistant message with tool_calls is a valid tail start —
	// its tool replies follow it inside the preserved tail.
	msgs := []provider.Message{
		{Role: "user", Content: "x"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1"}}},
		{Role: "tool", ToolCallID: "t1"},
	}
	if got := safeCompactionCutoff(msgs, 1); got != 1 {
		t.Errorf("cutoff = %d, want 1 (assistant w/ tool_calls is a valid tail start)", got)
	}
}

func TestSafeCompactionCutoffAdvancesToEnd(t *testing.T) {
	// Degenerate: every message from cutoff to end is a tool. The
	// guard advances past all of them — the tail ends up empty and
	// the caller emits just [summary], which is valid.
	msgs := []provider.Message{
		{Role: "user"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "t1"}, {ID: "t2"}}},
		{Role: "tool", ToolCallID: "t1"},
		{Role: "tool", ToolCallID: "t2"},
	}
	if got := safeCompactionCutoff(msgs, 2); got != len(msgs) {
		t.Errorf("cutoff = %d, want %d (entire tail was tool messages)", got, len(msgs))
	}
}

func TestSafeCompactionCutoffNegativeIsClamped(t *testing.T) {
	msgs := []provider.Message{{Role: "user"}}
	if got := safeCompactionCutoff(msgs, -5); got != 0 {
		t.Errorf("cutoff = %d, want 0 (negative input clamped)", got)
	}
}

// TestCompressOlderMessagesNeverStartsTailWithTool is the end-to-end
// assertion that closes the loop. Build a history where the naive
// cutoff lands squarely on a tool reply and verify the compressed
// output's first non-summary message is never a "tool" role. This
// mirrors the shape that was producing the OpenAI 400 in production
// /goal sessions.
func TestCompressOlderMessagesNeverStartsTailWithTool(t *testing.T) {
	// Rounds of [assistant(2 tool_calls), tool, tool] — 3 messages
	// each. 7 rounds = 21 messages. With 5 user fillers in front,
	// total len = 26 and naive cutoff = 26-PruneTurnAge = 6, which
	// indexes a tool reply (assistant at 5, tool at 6, tool at 7).
	var msgs []provider.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, provider.Message{Role: "user", Content: "filler"})
	}
	for i := 0; i < 7; i++ {
		msgs = append(msgs,
			provider.Message{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "ta"}, {ID: "tb"}}},
			provider.Message{Role: "tool", ToolCallID: "ta", Content: "ra"},
			provider.Message{Role: "tool", ToolCallID: "tb", Content: "rb"},
		)
	}
	// Pin the fixture: without the fix, the tail would start with a
	// "tool" message and OpenAI would 400.
	naive := len(msgs) - PruneTurnAge
	if msgs[naive].Role != "tool" {
		t.Fatalf("fixture broken: naive cutoff lands on %q (idx %d, len %d), want tool",
			msgs[naive].Role, naive, len(msgs))
	}

	f := &fakeSummarizer{}
	out, err := compressOlderMessages(msgs, CompactOptions{Provider: f, Model: "fake-model"})
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if len(out) < 2 {
		t.Fatalf("expected summary + tail, got %d messages", len(out))
	}
	if out[1].Role == "tool" {
		t.Errorf("compressed tail still starts with a tool message — the fix didn't take:\n%+v", out[1])
	}
	// Stronger invariant: every "tool" in the output must be preceded
	// somewhere upstream by an assistant.tool_calls. Spot-check by
	// looking for any tool that doesn't follow an assistant directly
	// (or after another tool from the same round).
	for i := 1; i < len(out); i++ {
		if out[i].Role != "tool" {
			continue
		}
		// Walk backwards skipping prior tools in the same round.
		j := i - 1
		for j >= 0 && out[j].Role == "tool" {
			j--
		}
		if j < 0 || out[j].Role != "assistant" || len(out[j].ToolCalls) == 0 {
			t.Errorf("tool at idx %d has no parent assistant.tool_calls in output", i)
		}
	}
}
