package agent

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/agent/goal"
	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/bus"
)

// newAgentForWireTest builds the minimal Agent skeleton the wiring
// tests need — registry / hooks / messageBus / agentID / ownerUserID.
func newAgentForWireTest(t *testing.T) *Agent {
	t.Helper()
	return &Agent{
		name:        "agent-test",
		ownerUserID: "user-1",
		registry:    tools.NewRegistry("", ""),
		hooks:       NewHookRegistry(),
		messageBus:  bus.New(),
	}
}

// TestWireGoalsNilStoreIsNoOp pins the contract that a nil store
// silently turns the feature off. Legacy single-user installs hit
// this path; without it they'd panic on agent boot.
func TestWireGoalsNilStoreIsNoOp(t *testing.T) {
	a := newAgentForWireTest(t)
	a.WireGoals(nil)
	if a.goalStore != nil {
		t.Error("goalStore should remain nil after WireGoals(nil)")
	}
	if a.registry.GetFunc("update_goal") != nil {
		t.Error("update_goal should not be registered when store is nil")
	}
}

// TestWireGoalsRegistersToolAndHook is the happy-path smoke test:
// after WireGoals returns, update_goal is registered and the
// AfterModelCall hook is present.
func TestWireGoalsRegistersToolAndHook(t *testing.T) {
	a := newAgentForWireTest(t)
	st := &memGoalStore{}
	a.WireGoals(st)

	if a.goalStore != st {
		t.Errorf("goalStore = %v, want %v", a.goalStore, st)
	}
	if a.registry.GetFunc("update_goal") == nil {
		t.Error("update_goal not registered")
	}
	if len(a.hooks.hooks[AfterModelCall]) < 1 {
		t.Errorf("AfterModelCall hook count = %d, want ≥1", len(a.hooks.hooks[AfterModelCall]))
	}
	if len(a.hooks.hooks[PostTurn]) < 1 {
		t.Errorf("PostTurn hook count = %d, want ≥1", len(a.hooks.hooks[PostTurn]))
	}
}

// Compile-time check that the in-package mem store satisfies goal.Store.
var _ goal.Store = (*memGoalStore)(nil)

// TestGoalTriggerHookGatesOnSource: the PostTurn trigger must fire
// only on allowed sources (user + goal_context). Cron / heartbeat /
// sub-agent turns must NOT chain a continuation or we'd loop.
func TestGoalTriggerHookGatesOnSource(t *testing.T) {
	a := newAgentForWireTest(t)
	st := &memGoalStore{}
	_ = st.CreateGoal(t.Context(), &goal.Goal{
		ID:         "g-fixture",
		AgentID:    a.name,
		SessionKey: "s-user",
		Channel:    "web",
		ChatID:     "c",
		Status:     goal.StatusActive,
	})
	a.WireGoals(st)

	hook := a.goalTriggerHook(allowedContinuationSources)
	// Non-allowed sources must short-circuit at the Source gate.
	for _, src := range []string{
		bus.SourceCron, bus.SourceHeartbeat, bus.SourceSubAgent,
	} {
		hook(t.Context(), &HookContext{
			Source:         src,
			GoalSessionKey: "s-user",
		})
	}
	select {
	case msg := <-a.messageBus.Inbound:
		t.Errorf("non-allowed source produced an inbound: %+v", msg)
	default:
	}

	// A genuine user turn fires a continuation onto the bus.
	hook(t.Context(), &HookContext{
		Source:         bus.SourceUser,
		GoalSessionKey: "s-user",
	})
	select {
	case msg := <-a.messageBus.Inbound:
		if msg.Source != bus.SourceGoalContext {
			t.Errorf("Source = %q, want goal_context", msg.Source)
		}
	default:
		t.Error("user-source PostTurn should have published a continuation")
	}
}

// TestGoalTriggerHookAllowsContinuationChain: goal_context must be in
// the allowed set — otherwise the loop wouldn't chain itself past the
// first continuation.
func TestGoalTriggerHookAllowsContinuationChain(t *testing.T) {
	a := newAgentForWireTest(t)
	st := &memGoalStore{}
	_ = st.CreateGoal(t.Context(), &goal.Goal{
		ID:         "g-fixture",
		AgentID:    a.name,
		SessionKey: "s-1",
		Channel:    "web",
		ChatID:     "c",
		Status:     goal.StatusActive,
	})
	a.WireGoals(st)

	hook := a.goalTriggerHook(allowedContinuationSources)
	hook(t.Context(), &HookContext{
		Source:         bus.SourceGoalContext,
		GoalSessionKey: "s-1",
	})
	select {
	case msg := <-a.messageBus.Inbound:
		if msg.Source != bus.SourceGoalContext {
			t.Errorf("Source = %q, want goal_context", msg.Source)
		}
	default:
		t.Error("goal_context source should have chained the loop")
	}
}

// TestGoalTriggerHookNoOpWithoutSessionKey is the safety net for
// boot-time / out-of-chat callbacks: no GoalSessionKey → no work.
func TestGoalTriggerHookNoOpWithoutSessionKey(t *testing.T) {
	a := newAgentForWireTest(t)
	a.WireGoals(&memGoalStore{})

	hook := a.goalTriggerHook(allowedContinuationSources)
	hook(t.Context(), &HookContext{Source: bus.SourceUser, GoalSessionKey: ""})
	select {
	case msg := <-a.messageBus.Inbound:
		t.Errorf("empty session key should not publish; got %+v", msg)
	default:
	}
}

// TestGoalTriggerHookNoOpInPlanMode: plan-mode pauses for human
// review; auto-firing the next continuation behind the user would
// defeat the point.
func TestGoalTriggerHookNoOpInPlanMode(t *testing.T) {
	a := newAgentForWireTest(t)
	st := &memGoalStore{}
	_ = st.CreateGoal(t.Context(), &goal.Goal{
		ID:         "g-fixture",
		AgentID:    a.name,
		SessionKey: "s-plan",
		Channel:    "web",
		ChatID:     "c",
		Status:     goal.StatusActive,
	})
	a.WireGoals(st)

	hook := a.goalTriggerHook(allowedContinuationSources)
	hook(t.Context(), &HookContext{
		Source:         bus.SourceUser,
		GoalSessionKey: "s-plan",
		IsPlanMode:     true,
	})
	select {
	case msg := <-a.messageBus.Inbound:
		t.Errorf("plan-mode turn should not auto-continue; got %+v", msg)
	default:
	}
}
