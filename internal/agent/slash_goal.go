package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bkclaw-ai/bkclaw/internal/agent/goal"
	"github.com/bkclaw-ai/bkclaw/internal/bus"
)

// slashGoal dispatches `/goal …` to one of the sub-handlers. The
// argument grammar mirrors the design doc § 6:
//
//	/goal <objective>          → create
//	/goal                      → show current
//	/goal pause | resume | clear
//
// `/goal budget <N>` is deliberately absent — Codex doesn't ship it
// and mid-flight budget changes have ambiguous semantics (do tokens
// already spent count?). Set the budget at create time instead.
func (a *Agent) slashGoal(msg bus.InboundMessage, args []string) slashResult {
	if a.goalStore == nil {
		return slashResult{
			handled: true,
			reply:   "The /goal feature isn't enabled on this install (no data store configured).",
		}
	}

	// First arg may be a sub-command. Anything else is treated as
	// objective text for the create path. `/goal pause objective`
	// would otherwise be ambiguous, but pause/resume/clear are short
	// keywords nobody would use as an objective opener.
	sub := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
	}
	switch sub {
	case "":
		return a.slashGoalShow(msg)
	case "pause":
		return a.slashGoalPause(msg)
	case "resume":
		return a.slashGoalResume(msg)
	case "clear":
		return a.slashGoalClear(msg)
	}
	// Default: treat the entire remainder as objective text.
	objective := strings.Join(args, " ")
	return a.slashGoalCreate(msg, objective)
}

// resolveSessionKey returns the persistent session_key for the
// in-flight turn, or "" if no session matches the inbound's
// (channel, account, chat, project) tuple. Slash handlers downgrade
// to a clean error message on "".
func (a *Agent) resolveSessionKey(msg bus.InboundMessage) string {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	if sess == nil {
		return ""
	}
	return sess.SessionKey()
}

func (a *Agent) slashGoalShow(msg bus.InboundMessage) slashResult {
	key := a.resolveSessionKey(msg)
	g, err := a.goalStore.GetGoalBySession(context.Background(), a.name, key)
	if errors.Is(err, goal.ErrNotFound) || g == nil {
		return slashResult{handled: true, reply: "No goal set."}
	}
	if err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error reading goal: %v", err)}
	}
	// Plain-text status — no emoji prefix or scaffolding. /goal is
	// the only command that returns visible text on success; pause /
	// resume / clear / create all stay silent.
	return slashResult{handled: true, reply: fmt.Sprintf("%s: %s", g.Status, g.Objective)}
}

func (a *Agent) slashGoalCreate(msg bus.InboundMessage, objective string) slashResult {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return slashResult{handled: true, reply: "Usage: `/goal <objective>`"}
	}

	key := a.resolveSessionKey(msg)
	if key == "" {
		return slashResult{handled: true, reply: "No session context."}
	}

	g := &goal.Goal{
		ID:          goal.NewID(),
		AgentID:     a.name,
		SessionKey:  key,
		OwnerUserID: a.ownerUserID,
		Channel:     msg.Channel,
		AccountID:   msg.AccountID,
		ChatID:      msg.ChatID,
		ProjectID:   msg.ProjectID,
		Objective:   objective,
		Status:      goal.StatusActive,
	}
	if err := a.goalStore.CreateGoal(context.Background(), g); err != nil {
		if errors.Is(err, goal.ErrAlreadyExists) {
			return slashResult{handled: true, reply: "Goal already exists; `/goal clear` first."}
		}
		return slashResult{handled: true, reply: fmt.Sprintf("Error creating goal: %v", err)}
	}

	// Fire the first continuation immediately off the user's own /goal
	// turn. Silent success — the continuation streaming back IS the
	// conversational reply, same as if the user had typed the
	// objective directly. Goal is transparent at the chat surface; no
	// scaffolding text.
	goal.TryFireContinuation(context.Background(), a.goalStore, a.messageBus, a.name, key)
	return slashResult{handled: true, reply: "", continuationQueued: true}
}

func (a *Agent) slashGoalPause(msg bus.InboundMessage) slashResult {
	// Silent transition. Wrong-state / no-goal cases still surface.
	return a.transitionGoal(msg, goal.StatusActive, goal.StatusPaused, "Not active.")
}

func (a *Agent) slashGoalResume(msg bus.InboundMessage) slashResult {
	res := a.transitionGoal(msg, goal.StatusPaused, goal.StatusActive, "Not paused.")
	// Empty reply == success path; non-empty == wrongStateMsg or
	// error. Fire the next continuation only on success.
	if res.handled && res.reply == "" {
		key := a.resolveSessionKey(msg)
		goal.TryFireContinuation(context.Background(), a.goalStore, a.messageBus, a.name, key)
		res.continuationQueued = true
	}
	return res
}

// transitionGoal centralizes the "load goal → check it's in the
// expected source state → flip → persist" pattern for pause/resume.
// On success the reply is silent (""); on wrong state, returns
// wrongStateMsg; on store errors, returns a formatted error.
func (a *Agent) transitionGoal(msg bus.InboundMessage, from, to goal.Status, wrongStateMsg string) slashResult {
	key := a.resolveSessionKey(msg)
	g, err := a.goalStore.GetGoalBySession(context.Background(), a.name, key)
	if errors.Is(err, goal.ErrNotFound) || g == nil {
		return slashResult{handled: true, reply: "No goal set."}
	}
	if err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error reading goal: %v", err)}
	}
	if g.Status != from {
		return slashResult{handled: true, reply: wrongStateMsg}
	}
	g.Status = to
	if err := a.goalStore.UpdateGoal(context.Background(), g); err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error updating goal: %v", err)}
	}
	return slashResult{handled: true, reply: ""}
}

func (a *Agent) slashGoalClear(msg bus.InboundMessage) slashResult {
	key := a.resolveSessionKey(msg)
	g, err := a.goalStore.GetGoalBySession(context.Background(), a.name, key)
	if errors.Is(err, goal.ErrNotFound) || g == nil {
		return slashResult{handled: true, reply: "No goal set."}
	}
	if err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error reading goal: %v", err)}
	}
	if err := a.goalStore.DeleteGoal(context.Background(), g.ID); err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error clearing goal: %v", err)}
	}
	return slashResult{handled: true, reply: ""}
}

// clearGoalForSession removes any goal attached to the named
// session_key. Called by /new and /reset so an old session's goal
// doesn't leak into a brand-new conversation thread on the same
// chat. Best-effort: store errors are not surfaced — /new shouldn't
// fail because of a stray goal row.
func (a *Agent) clearGoalForSession(sessionKey string) {
	if a.goalStore == nil || sessionKey == "" {
		return
	}
	g, err := a.goalStore.GetGoalBySession(context.Background(), a.name, sessionKey)
	if err != nil || g == nil {
		return
	}
	_ = a.goalStore.DeleteGoal(context.Background(), g.ID)
}

