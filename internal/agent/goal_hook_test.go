package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bkclaw-ai/bkclaw/internal/agent/goal"
	"github.com/bkclaw-ai/bkclaw/internal/bus"
	"github.com/bkclaw-ai/bkclaw/internal/provider"
)

// memGoalStore is the in-memory goal.Store the hook tests use.
// Separate copy from internal/agent/tools so the two packages don't
// depend on each other for test fixtures.
type memGoalStore struct {
	mu      sync.Mutex
	row     *goal.Goal
	getErr  error
	saveErr error
}

func (m *memGoalStore) CreateGoal(_ context.Context, g *goal.Goal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row != nil {
		return goal.ErrAlreadyExists
	}
	clone := *g
	m.row = &clone
	return nil
}
func (m *memGoalStore) GetGoalBySession(_ context.Context, agentID, sessionKey string) (*goal.Goal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.row == nil || m.row.AgentID != agentID || m.row.SessionKey != sessionKey {
		return nil, goal.ErrNotFound
	}
	clone := *m.row
	return &clone, nil
}
func (m *memGoalStore) UpdateGoal(_ context.Context, g *goal.Goal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	clone := *g
	m.row = &clone
	return nil
}
func (m *memGoalStore) DeleteGoal(_ context.Context, goalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row != nil && m.row.ID == goalID {
		m.row = nil
	}
	return nil
}
// seedActiveGoal places a fresh active goal in the store. Returns
// the agentID + sessionKey the hook context should reference.
func seedActiveGoal(t *testing.T, st *memGoalStore, budget int64) (agentID, sessionKey string) {
	t.Helper()
	b := budget
	g := &goal.Goal{
		ID:          "g-test",
		AgentID:     "agent-A",
		SessionKey:  "s-test",
		OwnerUserID: "user-1",
		Objective:   "x",
		Status:      goal.StatusActive,
		TokenBudget: &b,
	}
	if err := st.CreateGoal(context.Background(), g); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return g.AgentID, g.SessionKey
}

// seedActiveGoalWithRouting is seedActiveGoal + the routing tuple
// the budget_limit publish path needs. Existing tests that don't
// exercise the publish path keep using seedActiveGoal; the bus
// publish gate (g.Channel == "" && g.ChatID == "") would otherwise
// swallow the call silently and the new assertions would pass for
// the wrong reason.
func seedActiveGoalWithRouting(t *testing.T, st *memGoalStore, budget int64) (agentID, sessionKey string) {
	t.Helper()
	b := budget
	g := &goal.Goal{
		ID:          "g-test",
		AgentID:     "agent-A",
		SessionKey:  "s-test",
		OwnerUserID: "user-1",
		Channel:     "web",
		ChatID:      "chat-1",
		Objective:   "x",
		Status:      goal.StatusActive,
		TokenBudget: &b,
	}
	if err := st.CreateGoal(context.Background(), g); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return g.AgentID, g.SessionKey
}

func makeAfterModelCall(sessionKey string, u provider.Usage) *HookContext {
	return &HookContext{
		Point:          AfterModelCall,
		Response:       &provider.Response{Usage: u},
		GoalSessionKey: sessionKey,
	}
}

func TestTokenAccountingHookFoldsUsage(t *testing.T) {
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoal(t, st, 1_000_000)
	hook := NewTokenAccountingHook(st, nil, agentID)

	hook(context.Background(), makeAfterModelCall(sessionKey, provider.Usage{
		InputTokens: 200, CacheReadTokens: 50, OutputTokens: 30,
	}))
	// provider.Usage.InputTokens is already the uncached billable
	// portion (Anthropic excludes cache_read by definition; OpenAI's
	// adapter subtracts cached_tokens). So delta = 200 + 30 = 230.
	g, _ := st.GetGoalBySession(context.Background(), agentID, sessionKey)
	if g.TokensUsed != 230 {
		t.Errorf("TokensUsed = %d, want 230", g.TokensUsed)
	}
	if g.Status != goal.StatusActive {
		t.Errorf("status = %q, want active (1M budget not yet hit)", g.Status)
	}
}

func TestTokenAccountingHookFlipsBudgetLimited(t *testing.T) {
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoal(t, st, 100)
	hook := NewTokenAccountingHook(st, nil, agentID)

	hook(context.Background(), makeAfterModelCall(sessionKey, provider.Usage{
		InputTokens: 50, OutputTokens: 60,
	}))
	g, _ := st.GetGoalBySession(context.Background(), agentID, sessionKey)
	if g.Status != goal.StatusBudgetLimited {
		t.Errorf("status = %q, want budget_limited (used %d > budget 100)",
			g.Status, g.TokensUsed)
	}
}

// TestTokenAccountingHookPublishesBudgetLimit pins the
// transition-edge publish: when the hook flips a goal to
// BudgetLimited, the budget_limit prompt must be queued on the bus
// so the next inbound turn lets the model wrap up gracefully.
// Without this, a budget_limited goal would just stop silently mid-
// turn with no closing message.
func TestTokenAccountingHookPublishesBudgetLimit(t *testing.T) {
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoalWithRouting(t, st, 100)
	mb := bus.New()
	hook := NewTokenAccountingHook(st, mb, agentID)

	hook(context.Background(), makeAfterModelCall(sessionKey, provider.Usage{
		InputTokens: 50, OutputTokens: 60,
	}))

	select {
	case msg := <-mb.Inbound:
		if msg.Source != bus.SourceGoalContext {
			t.Errorf("Source = %q, want goal_context", msg.Source)
		}
		if !strings.Contains(msg.Text, "budget_limited") {
			t.Errorf("budget_limit prompt missing status word:\n%s", msg.Text)
		}
		if msg.Channel != "web" || msg.ChatID != "chat-1" {
			t.Errorf("routing mismatch: channel=%q chat=%q (want web/chat-1)",
				msg.Channel, msg.ChatID)
		}
	default:
		t.Fatal("hook flipped to budget_limited but didn't publish a wrap-up prompt")
	}
}

// TestTokenAccountingHookSilentOnNonExhaustingCall is the inverse:
// a normal call that doesn't cross the budget threshold must NOT
// publish anything. Bus traffic = "hook saw exhaustion edge", not
// "hook ran".
func TestTokenAccountingHookSilentOnNonExhaustingCall(t *testing.T) {
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoalWithRouting(t, st, 100_000)
	mb := bus.New()
	hook := NewTokenAccountingHook(st, mb, agentID)

	hook(context.Background(), makeAfterModelCall(sessionKey, provider.Usage{
		InputTokens: 50, OutputTokens: 60,
	}))

	select {
	case msg := <-mb.Inbound:
		t.Fatalf("non-exhausting call must not publish; got %+v", msg)
	default:
	}
}

func TestTokenAccountingHookSkipsWhenNoGoalSession(t *testing.T) {
	st := &memGoalStore{}
	hook := NewTokenAccountingHook(st, nil, "agent-A")
	// Empty GoalSessionKey — agent ran outside a chat context (e.g.
	// boot-time warmup). Hook must no-op rather than crash on a
	// missing row.
	hook(context.Background(), &HookContext{
		Point:    AfterModelCall,
		Response: &provider.Response{Usage: provider.Usage{OutputTokens: 100}},
	})
	// No-op — store should still be empty.
	if st.row != nil {
		t.Errorf("hook persisted a goal when no session_key was set")
	}
}

func TestTokenAccountingHookSkipsWhenNoUsage(t *testing.T) {
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoal(t, st, 1000)
	hook := NewTokenAccountingHook(st, nil, agentID)

	// Response.Usage is the zero value — provider didn't report
	// (e.g. local Ollama). Don't error; just skip folding so a
	// budget-bound goal doesn't get billed against air.
	hook(context.Background(), &HookContext{
		Point:          AfterModelCall,
		Response:       &provider.Response{},
		GoalSessionKey: sessionKey,
	})
	g, _ := st.GetGoalBySession(context.Background(), agentID, sessionKey)
	if g.TokensUsed != 0 {
		t.Errorf("TokensUsed mutated to %d on zero-Usage hook", g.TokensUsed)
	}
}

func TestTokenAccountingHookSkipsOnError(t *testing.T) {
	// If the model call itself errored, the response is meaningless
	// and any usage on it shouldn't count.
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoal(t, st, 1000)
	hook := NewTokenAccountingHook(st, nil, agentID)

	hook(context.Background(), &HookContext{
		Point:          AfterModelCall,
		Response:       &provider.Response{Usage: provider.Usage{OutputTokens: 500}},
		Error:          errors.New("rate limited"),
		GoalSessionKey: sessionKey,
	})
	g, _ := st.GetGoalBySession(context.Background(), agentID, sessionKey)
	if g.TokensUsed != 0 {
		t.Errorf("TokensUsed = %d, want 0 on errored call", g.TokensUsed)
	}
}

func TestTokenAccountingHookSkipsWhenNoGoalRow(t *testing.T) {
	// A turn happens on a session with no goal — hook must not error
	// loudly. ErrNotFound is the expected path here.
	st := &memGoalStore{}
	hook := NewTokenAccountingHook(st, nil, "agent-A")
	hook(context.Background(), makeAfterModelCall("s-no-goal", provider.Usage{OutputTokens: 100}))
	// No row to inspect — just confirming no panic.
}

func TestTokenAccountingHookOnlyFiresOnAfterModelCall(t *testing.T) {
	// The hook is registered against AfterModelCall, but the same
	// closure could be reused at other hook points by mistake. Guard
	// at the entry so a stray registration doesn't bill the same
	// turn twice.
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoal(t, st, 1000)
	hook := NewTokenAccountingHook(st, nil, agentID)

	for _, point := range []HookPoint{BeforeModelCall, BeforeToolCall, AfterToolCall, PostTurn} {
		hook(context.Background(), &HookContext{
			Point:          point,
			Response:       &provider.Response{Usage: provider.Usage{OutputTokens: 100}},
			GoalSessionKey: sessionKey,
		})
	}
	g, _ := st.GetGoalBySession(context.Background(), agentID, sessionKey)
	if g.TokensUsed != 0 {
		t.Errorf("TokensUsed = %d, want 0 (hook should only fire on AfterModelCall)", g.TokensUsed)
	}
}

func TestTokenAccountingHookNilStoreReturnsNil(t *testing.T) {
	// Lets the caller register the hook unconditionally during agent
	// boot — when goal feature isn't wired, NewTokenAccountingHook
	// just returns a nil func and the registration is a no-op (the
	// hook registry's Run skips nil entries).
	if h := NewTokenAccountingHook(nil, nil, "agent"); h != nil {
		t.Errorf("expected nil HookFunc for nil store, got %T", h)
	}
}

func TestTokenAccountingHookSkipsZeroDelta(t *testing.T) {
	// An all-cached prompt with no output produces 0 delta — the
	// hook must skip the persist round-trip rather than rewrite an
	// unchanged row. We poke the saveErr fuse: if the hook reaches
	// UpdateGoal, the test fails on the row check.
	st := &memGoalStore{}
	agentID, sessionKey := seedActiveGoal(t, st, 1000)
	st.saveErr = errors.New("UpdateGoal should not be called for zero-delta")

	hook := NewTokenAccountingHook(st, nil, agentID)
	// CacheReadTokens > 0 keeps the "is Usage entirely zero" gate
	// from short-circuiting; InputTokens=0 + OutputTokens=0 makes
	// the actual goal-token delta zero.
	hook(context.Background(), makeAfterModelCall(sessionKey, provider.Usage{
		CacheReadTokens: 100,
	}))
	g, _ := st.GetGoalBySession(context.Background(), agentID, sessionKey)
	if g.TokensUsed != 0 {
		t.Errorf("TokensUsed = %d, want 0 on zero-delta call", g.TokensUsed)
	}
}
