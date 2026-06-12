package goal

import (
	"context"
	"errors"
	"log/slog"

	"github.com/qs3c/bkclaw/internal/bus"
)

// TryFireContinuation runs the gate cascade for one (agent, session)
// and, on success, publishes a continuation prompt back onto the bus.
// Safe to call synchronously from PostTurn / slash handlers — gates
// are cheap (one indexed read) and any failure is a silent no-op.
//
// Gates (in order):
//   - goal exists for this (agent, session)
//   - goal status is Active (Paused / BudgetLimited / Complete no-op)
//   - goal carries routing info (legacy rows missing routing fields
//     can't be auto-continued; the publish would emit an unroutable
//     inbound)
//
// Errors land at warn level rather than blocking the caller —
// continuation is best-effort, and the next PostTurn will retry.
func TryFireContinuation(ctx context.Context, st Store, mb *bus.MessageBus, agentID, sessionKey string) {
	g, err := st.GetGoalBySession(ctx, agentID, sessionKey)
	if errors.Is(err, ErrNotFound) {
		return
	}
	if err != nil {
		slog.Warn("goal continue: load goal failed",
			"agent_id", agentID, "session_key", sessionKey, "error", err)
		return
	}
	if g.Status != StatusActive {
		return
	}
	if g.Channel == "" && g.ChatID == "" {
		slog.Warn("goal continue: skipping — goal has no routing info",
			"agent_id", agentID, "session_key", sessionKey, "goal_id", g.ID)
		return
	}
	if !Publish(mb, g, ContinuationPrompt(g)) {
		slog.Warn("goal continue: bus full, dropped continuation",
			"agent_id", agentID, "session_key", sessionKey)
	}
}

// Publish pushes a goal-context prompt (continuation or budget-limit
// wrap-up) onto the bus. Tagged with bus.SourceGoalContext so the
// agent loop can distinguish runtime-injected goal prompts from real
// user input and tag the resulting message with OriginGoalContext.
// Returns true when queued, false when the bus is full.
func Publish(mb *bus.MessageBus, g *Goal, prompt string) bool {
	if mb == nil || g == nil {
		return false
	}
	msg := bus.InboundMessage{
		Channel:     g.Channel,
		AccountID:   g.AccountID,
		ChatID:      g.ChatID,
		ProjectID:   g.ProjectID,
		UserID:      "goal",
		OwnerUserID: g.OwnerUserID,
		AgentID:     g.AgentID,
		Text:        prompt,
		PeerKind:    "dm",
		Source:      bus.SourceGoalContext,
	}
	select {
	case mb.Inbound <- msg:
		return true
	default:
		return false
	}
}
