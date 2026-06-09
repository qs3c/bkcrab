package goal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/bkclaw-ai/bkclaw/internal/bus"
)

// memStore is the in-memory goal.Store used by continue_test.go to
// exercise each gate of TryFireContinuation in isolation. Keyed by
// (agentID, sessionKey).
type memStore struct {
	mu      sync.Mutex
	rows    map[string]*Goal
	getErr  error // when non-nil, GetGoalBySession returns this
	saveErr error
}

func newMemStore() *memStore { return &memStore{rows: map[string]*Goal{}} }

func (m *memStore) key(a, s string) string { return a + "|" + s }

func (m *memStore) CreateGoal(_ context.Context, g *Goal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.key(g.AgentID, g.SessionKey)
	if _, ok := m.rows[k]; ok {
		return ErrAlreadyExists
	}
	clone := *g
	m.rows[k] = &clone
	return nil
}

func (m *memStore) GetGoalBySession(_ context.Context, agentID, sessionKey string) (*Goal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return nil, m.getErr
	}
	g, ok := m.rows[m.key(agentID, sessionKey)]
	if !ok {
		return nil, ErrNotFound
	}
	clone := *g
	return &clone, nil
}

func (m *memStore) UpdateGoal(_ context.Context, g *Goal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.saveErr != nil {
		return m.saveErr
	}
	clone := *g
	m.rows[m.key(g.AgentID, g.SessionKey)] = &clone
	return nil
}

func (m *memStore) DeleteGoal(_ context.Context, _ string) error { return nil }

func seedActive(t *testing.T, st *memStore) *Goal {
	t.Helper()
	g := &Goal{
		ID:          "g-1",
		AgentID:     "agent-A",
		SessionKey:  "s-1",
		OwnerUserID: "user-1",
		Channel:     "web",
		ChatID:      "chat-1",
		Objective:   "translate",
		Status:      StatusActive,
	}
	if err := st.CreateGoal(context.Background(), g); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return g
}

// TestTryFireContinuationActivePublishes — the happy path: an Active
// goal with routing info publishes a goal_context-tagged inbound onto
// the bus, carrying the rendered continuation prompt and the goal's
// routing tuple.
func TestTryFireContinuationActivePublishes(t *testing.T) {
	st := newMemStore()
	g := seedActive(t, st)
	mb := bus.New()

	TryFireContinuation(context.Background(), st, mb, g.AgentID, g.SessionKey)

	select {
	case got := <-mb.Inbound:
		if got.Source != bus.SourceGoalContext {
			t.Errorf("Source = %q, want %q", got.Source, bus.SourceGoalContext)
		}
		if got.Channel != "web" || got.ChatID != "chat-1" {
			t.Errorf("routing = (%q, %q), want (web, chat-1)", got.Channel, got.ChatID)
		}
		if got.AgentID != g.AgentID || got.OwnerUserID != g.OwnerUserID {
			t.Errorf("identity = (%q, %q), want (%q, %q)",
				got.AgentID, got.OwnerUserID, g.AgentID, g.OwnerUserID)
		}
		if !strings.Contains(got.Text, "<goal_context>") || !strings.Contains(got.Text, "translate") {
			t.Errorf("prompt body looks wrong:\n%s", got.Text)
		}
	default:
		t.Fatal("Active goal with routing must publish a continuation")
	}
}

// TestTryFireContinuationNoGoal — no row for (agent, session) → no
// publish, no error. This is the common case for sessions that
// never set a goal.
func TestTryFireContinuationNoGoal(t *testing.T) {
	st := newMemStore()
	mb := bus.New()
	TryFireContinuation(context.Background(), st, mb, "agent-A", "s-nope")
	assertNoPublish(t, mb)
}

// TestTryFireContinuationNonActiveStatusSkips — paused / budget_limited
// / complete goals must not chain a continuation. Without this gate
// a /goal pause leaves a goroutine looping forever.
func TestTryFireContinuationNonActiveStatusSkips(t *testing.T) {
	for _, status := range []Status{StatusPaused, StatusBudgetLimited, StatusComplete} {
		t.Run(status, func(t *testing.T) {
			st := newMemStore()
			g := seedActive(t, st)
			g.Status = status
			_ = st.UpdateGoal(context.Background(), g)
			mb := bus.New()

			TryFireContinuation(context.Background(), st, mb, g.AgentID, g.SessionKey)
			assertNoPublish(t, mb)
		})
	}
}

// TestTryFireContinuationNoRoutingSkips — a legacy goal row created
// before the routing migration has Channel="" / ChatID="". Publishing
// such a message would land on the bus with no channel adapter able
// to deliver it; the gate must skip.
func TestTryFireContinuationNoRoutingSkips(t *testing.T) {
	st := newMemStore()
	g := seedActive(t, st)
	g.Channel = ""
	g.ChatID = ""
	_ = st.UpdateGoal(context.Background(), g)
	mb := bus.New()

	TryFireContinuation(context.Background(), st, mb, g.AgentID, g.SessionKey)
	assertNoPublish(t, mb)
}

// TestTryFireContinuationStoreErrorSwallowed — when the store returns
// a transient error (network, timeout), TryFireContinuation must NOT
// panic or propagate; the next PostTurn will retry. Bus stays empty.
func TestTryFireContinuationStoreErrorSwallowed(t *testing.T) {
	st := newMemStore()
	st.getErr = errors.New("transient db blip")
	mb := bus.New()

	TryFireContinuation(context.Background(), st, mb, "agent-A", "s-1")
	assertNoPublish(t, mb)
}

// TestPublishReturnsFalseWhenBusFull — Publish is best-effort: if the
// bus.Inbound channel is at capacity, the function must return false
// rather than block or panic. The caller (TryFireContinuation, the
// budget-exhaust hook) logs and moves on.
func TestPublishReturnsFalseWhenBusFull(t *testing.T) {
	mb := bus.New()
	// Fill the 100-deep buffer.
	for i := 0; i < cap(mb.Inbound); i++ {
		mb.Inbound <- bus.InboundMessage{Text: "filler"}
	}
	g := &Goal{Channel: "web", ChatID: "c", AgentID: "a"}
	if Publish(mb, g, "prompt") {
		t.Error("expected false when bus is full")
	}
}

// TestPublishNilGuards — nil bus or nil goal: return false rather
// than panic. Defensive but used: code paths in tests sometimes pass
// nil mb when they only care about the gate logic, not the publish.
func TestPublishNilGuards(t *testing.T) {
	if Publish(nil, &Goal{}, "x") {
		t.Error("nil bus should return false")
	}
	if Publish(bus.New(), nil, "x") {
		t.Error("nil goal should return false")
	}
}

func assertNoPublish(t *testing.T, mb *bus.MessageBus) {
	t.Helper()
	select {
	case msg := <-mb.Inbound:
		t.Errorf("expected silent skip; got %+v", msg)
	default:
	}
}
