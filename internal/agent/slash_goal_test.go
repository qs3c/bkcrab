package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/agent/goal"
	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/session"
)

// newSlashTestAgent builds an Agent wired enough to exercise the
// slash_goal.go handlers: store + a real session.Manager (file-backed
// in a temp dir) so resolveSessionKey returns a stable key without
// dragging in the rest of NewAgent's machinery.
func newSlashTestAgent(t *testing.T) *Agent {
	t.Helper()
	a := &Agent{
		name:        "agent-test",
		ownerUserID: "user-1",
		registry:    tools.NewRegistry("", ""),
		hooks:       NewHookRegistry(),
		messageBus:  bus.New(),
		sessions:    session.NewManager(t.TempDir()),
	}
	a.WireGoals(&memGoalStore{})
	return a
}

// webMsg is the canonical InboundMessage shape the slash tests use —
// web channel, fixed account/chat so resolveSessionKey returns a
// reproducible session_key across calls within a single test.
func webMsg() bus.InboundMessage {
	return bus.InboundMessage{
		Channel:   "web",
		AccountID: "",
		ChatID:    "chat-1",
	}
}

// strongObjective is the canonical objective most slash tests use
// when they don't care about objective content — anything non-empty
// would do, but a stable string keeps test reply assertions diffable.
// (Name is historical: there used to be a weak-objective heuristic
// this string was specifically shaped to bypass; that heuristic was
// removed, but the name kept its callers stable.)
const strongObjective = "Translate fixture data into output.json; verify rows match expected count"

// strongArgs splits strongObjective on whitespace — slash handlers
// receive their objective as []string, the way the dispatcher in
// slash.go feeds them.
func strongArgs() []string { return strings.Fields(strongObjective) }

func TestSlashGoalShowEmptySession(t *testing.T) {
	a := newSlashTestAgent(t)
	res := a.slashGoal(webMsg(), nil)
	if !res.handled {
		t.Fatal("expected handled=true")
	}
	if !strings.Contains(res.reply, "No goal set") {
		t.Errorf("reply doesn't read like an empty-state message:\n%s", res.reply)
	}
}

func TestSlashGoalCreateHappyPath(t *testing.T) {
	a := newSlashTestAgent(t)
	res := a.slashGoal(webMsg(), strongArgs())
	// Successful create is silent — goal is transparent at the
	// chat surface, the continuation streaming back IS the
	// conversational reply.
	if !res.handled || res.reply != "" {
		t.Fatalf("expected silent success; got handled=%v reply=%q", res.handled, res.reply)
	}

	// Verify the row landed with the right routing tuple.
	key := a.resolveSessionKey(webMsg())
	g, _ := a.goalStore.GetGoalBySession(context.Background(), a.name, key)
	if g == nil {
		t.Fatal("goal not persisted")
	}
	if g.Objective != strongObjective {
		t.Errorf("objective = %q, want %q", g.Objective, strongObjective)
	}
	if g.Channel != "web" || g.ChatID != "chat-1" {
		t.Errorf("routing not stamped: channel=%q chat=%q", g.Channel, g.ChatID)
	}
	if g.Status != goal.StatusActive {
		t.Errorf("status = %q, want active", g.Status)
	}
}

// TestSlashGoalCreatePublishesContinuation pins the end-to-end:
// /goal foo must push the first continuation onto bus.Inbound so
// the gateway picks it up the same way it picks up cron messages.
// If this regresses, the user types /goal and sees the slash reply
// (silent now) followed by nothing at all — because nothing ever
// hits the bus and the gateway never has work to dispatch.
func TestSlashGoalCreatePublishesContinuation(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())

	select {
	case got := <-a.messageBus.Inbound:
		if got.Source != bus.SourceGoalContext {
			t.Errorf("Source = %q, want %q", got.Source, bus.SourceGoalContext)
		}
		if got.Channel != "web" || got.ChatID != "chat-1" {
			t.Errorf("routing not stamped on continuation msg: channel=%q chat=%q",
				got.Channel, got.ChatID)
		}
		if got.AgentID != a.name {
			t.Errorf("AgentID = %q, want %q (gateway routeDM needs it)", got.AgentID, a.name)
		}
		if got.OwnerUserID != a.ownerUserID {
			t.Errorf("OwnerUserID = %q, want %q (gateway needs it to resolve user space)",
				got.OwnerUserID, a.ownerUserID)
		}
		if !strings.Contains(got.Text, "<goal_context>") {
			t.Errorf("Text doesn't look like an audit prompt:\n%s", got.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no continuation published to bus.Inbound within 2s — TryFireContinuation path is broken")
	}
}

func TestSlashGoalCreateWithoutObjectiveFails(t *testing.T) {
	a := newSlashTestAgent(t)
	// `/goal pause` with no goal — that's not a create; it dispatches
	// to slashGoalPause. To get the "objective required" path, the
	// first arg has to be something other than pause/resume/clear/"".
	// But that's harder to trigger via slashGoal. The actual "no
	// objective" path is slashGoalCreate("") — exercise it directly.
	res := a.slashGoalCreate(webMsg(), "   ")
	if !strings.Contains(res.reply, "Usage:") {
		t.Errorf("expected usage hint on blank objective; got %s", res.reply)
	}
}

func TestSlashGoalCreateRejectsDuplicate(t *testing.T) {
	a := newSlashTestAgent(t)
	if r := a.slashGoal(webMsg(), strongArgs()); !r.handled {
		t.Fatalf("seed not handled: %s", r.reply)
	}
	res := a.slashGoal(webMsg(), strongArgs())
	if !strings.Contains(res.reply, "already exists") {
		t.Errorf("expected duplicate-rejection message, got %s", res.reply)
	}
}

func TestSlashGoalPauseResumeCycle(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())

	if r := a.slashGoal(webMsg(), []string{"pause"}); !r.handled || r.reply != "" {
		t.Fatalf("pause: handled=%v reply=%q (want silent)", r.handled, r.reply)
	}
	key := a.resolveSessionKey(webMsg())
	g, _ := a.goalStore.GetGoalBySession(context.Background(), a.name, key)
	if g.Status != goal.StatusPaused {
		t.Errorf("status after pause = %q, want paused", g.Status)
	}

	if r := a.slashGoal(webMsg(), []string{"resume"}); !r.handled || r.reply != "" {
		t.Fatalf("resume: handled=%v reply=%q (want silent)", r.handled, r.reply)
	}
	g, _ = a.goalStore.GetGoalBySession(context.Background(), a.name, key)
	if g.Status != goal.StatusActive {
		t.Errorf("status after resume = %q, want active", g.Status)
	}
}

func TestSlashGoalPauseRejectsWrongState(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())
	a.slashGoal(webMsg(), []string{"pause"})

	// Already paused — second pause must fail loudly so the user
	// knows nothing changed.
	res := a.slashGoal(webMsg(), []string{"pause"})
	if !strings.Contains(res.reply, "Not active") {
		t.Errorf("expected wrong-state hint; got %s", res.reply)
	}
}

// TestSlashGoalResumeRejectsWrongState mirrors the pause test on the
// other side: /goal resume on an already-active goal must reject and
// NOT publish a continuation (a successful resume would).
func TestSlashGoalResumeRejectsWrongState(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())
	drainInbound(a.messageBus) // discard the create-time continuation

	res := a.slashGoal(webMsg(), []string{"resume"})
	if !strings.Contains(res.reply, "Not paused") {
		t.Errorf("expected wrong-state hint; got %s", res.reply)
	}
	select {
	case msg := <-a.messageBus.Inbound:
		t.Errorf("resume on active should not publish; got %+v", msg)
	default:
	}
}

// TestSlashGoalResumeOnNoGoal: /goal resume on a session with no
// goal must reply "No goal set" and stay silent on the bus.
func TestSlashGoalResumeOnNoGoal(t *testing.T) {
	a := newSlashTestAgent(t)
	res := a.slashGoal(webMsg(), []string{"resume"})
	if !strings.Contains(res.reply, "No goal set") {
		t.Errorf("expected no-goal reply; got %s", res.reply)
	}
}

// TestSlashGoalResumePublishesContinuation: the symmetric of the
// create-time publish — after pause+resume, the next continuation
// must hit the bus so the loop actually picks back up.
func TestSlashGoalResumePublishesContinuation(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())
	a.slashGoal(webMsg(), []string{"pause"})
	drainInbound(a.messageBus)

	res := a.slashGoal(webMsg(), []string{"resume"})
	if res.reply != "" {
		t.Fatalf("resume reply = %q, want silent", res.reply)
	}
	select {
	case got := <-a.messageBus.Inbound:
		if got.Source != bus.SourceGoalContext {
			t.Errorf("Source = %q, want goal_context", got.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("resume must publish a continuation onto the bus")
	}
}

func TestSlashGoalClearRemovesRow(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())
	key := a.resolveSessionKey(webMsg())

	res := a.slashGoal(webMsg(), []string{"clear"})
	// Clear is silent — goal is transparent at the chat surface.
	if !res.handled || res.reply != "" {
		t.Errorf("clear: handled=%v reply=%q (want silent)", res.handled, res.reply)
	}
	if g, _ := a.goalStore.GetGoalBySession(context.Background(), a.name, key); g != nil {
		t.Error("goal still present after clear")
	}
}

func TestSlashGoalShowFormatsActive(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())
	res := a.slashGoal(webMsg(), nil)
	if !strings.Contains(res.reply, "active") {
		t.Errorf("missing status:\n%s", res.reply)
	}
	if !strings.Contains(res.reply, strongObjective) {
		t.Errorf("missing objective:\n%s", res.reply)
	}
}

// TestSlashGoalDisabledWithoutStore: the slash dispatch must
// degrade cleanly when the agent was built without a data store
// (legacy single-user installs). No panic, friendly message.
func TestSlashGoalDisabledWithoutStore(t *testing.T) {
	a := &Agent{
		name:     "agent-test",
		registry: tools.NewRegistry("", ""),
		hooks:    NewHookRegistry(),
		sessions: session.NewManager(t.TempDir()),
	}
	res := a.slashGoal(webMsg(), []string{"x"})
	if !strings.Contains(res.reply, "isn't enabled") {
		t.Errorf("expected feature-off message; got %s", res.reply)
	}
}

// TestPlanModeGatesTrigger: when the turn ran in plan-mode, the
// trigger hook must NOT publish a continuation. Otherwise plan-mode's
// "let the user review before more work" promise breaks.
func TestPlanModeGatesTrigger(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), strongArgs())
	key := a.resolveSessionKey(webMsg())
	drainInbound(a.messageBus)

	hook := a.goalTriggerHook(allowedContinuationSources)
	hook(context.Background(), &HookContext{
		Source:         bus.SourceUser,
		GoalSessionKey: key,
		IsPlanMode:     true,
	})
	select {
	case msg := <-a.messageBus.Inbound:
		t.Errorf("plan-mode turn published a continuation: %+v", msg)
	default:
	}
}

// TestTriggerSkipsSessionsWithoutGoal: a turn on a session with no
// goal must NOT publish anything onto the bus.
func TestTriggerSkipsSessionsWithoutGoal(t *testing.T) {
	a := newSlashTestAgent(t)
	hook := a.goalTriggerHook(allowedContinuationSources)
	hook(context.Background(), &HookContext{
		Source:         bus.SourceUser,
		GoalSessionKey: "s-no-goal-here",
	})
	select {
	case msg := <-a.messageBus.Inbound:
		t.Errorf("triggers on session without a goal should not publish; got %+v", msg)
	default:
	}
}

// drainInbound clears any pending messages the slash-create path put
// on the bus, so a later assertion only sees the explicit publish under
// test.
func drainInbound(mb *bus.MessageBus) {
	for {
		select {
		case <-mb.Inbound:
		default:
			return
		}
	}
}

// TestNewSessionClearsGoal: /new on a session that has a goal must
// delete the goal row so the fresh session starts clean. Otherwise
// a stale goal would re-arm itself on the user's next message in
// the new conversation (different sessionKey, but same chat from the
// channel adapter's POV).
func TestNewSessionClearsGoal(t *testing.T) {
	a := newSlashTestAgent(t)
	a.slashGoal(webMsg(), []string{"obj"})
	oldKey := a.resolveSessionKey(webMsg())

	a.clearGoalForSession(oldKey)
	if g, _ := a.goalStore.GetGoalBySession(context.Background(), a.name, oldKey); g != nil {
		t.Errorf("/new should have cleared goal for session %q", oldKey)
	}
}
