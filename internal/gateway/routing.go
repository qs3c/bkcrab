package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/agent/tools"
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// chatKey is the per-conversation serialization key used by the task
// queue so messages for one chat run sequentially. Includes accountID
// because two bots of the same channel type can have a colliding
// chat_id (e.g. Telegram chat 12345 on bot A is unrelated to chat 12345
// on bot B) — without it those would serialize against each other and
// one bot's slow turn would block the other.
func chatKey(channel, accountID, chatID string) string {
	return channel + ":" + accountID + ":" + chatID
}

// processInbound consumes the message bus and routes each message to the
// correct user's agent. Identity resolution order:
//   1. msg.OwnerUserID set explicitly (cron, webhook with user_id)
//   2. lookup the receiving channel's row in the channels table — its
//      (scope, scope_id) tells us which user owns this conversation
// If neither yields a user_id the message is dropped, never silently
// routed to a default identity.
func (g *Gateway) processInbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-g.bus.Inbound:
			ownerID := msg.OwnerUserID
			if ownerID == "" {
				ownerID = g.resolveChannelOwner(ctx, msg)
			}
			if ownerID == "" {
				slog.Warn("dropping inbound: cannot resolve owner",
					"channel", msg.Channel, "chat_id", msg.ChatID, "account", msg.AccountID)
				continue
			}
			msg.OwnerUserID = ownerID

			// Dedup runs BEFORE chatter resolution so a duplicate
			// inbound doesn't pay the EnsureAppUser cost (and doesn't
			// spuriously bump that user's last-seen if we ever add
			// such tracking). Covers both DM and group — see
			// isDuplicate for the per-kind keying.
			if g.isDuplicate(msg) {
				slog.Info("dropping duplicate inbound",
					"channel", msg.Channel, "chat_id", msg.ChatID,
					"message_id", msg.MessageID, "peer_kind", msg.PeerKind,
					"account", msg.AccountID)
				continue
			}

			// Normalize msg.UserID into a fastclaw `u_xxx` id. IM channels
			// (wechat, telegram, line, discord, feishu, slack) emit the raw
			// platform-side identifier, which doesn't match the key that
			// per-chatter data (USER.md, MEMORY.md, per-user skills) is
			// stored under — so without translation the agent ends up with
			// an empty chatter profile every turn. See resolveChatter for
			// the lazy-mint semantics.
			if chatterID := g.resolveChatter(ctx, ownerID, msg); chatterID != "" {
				msg.UserID = chatterID
			}

			if msg.PeerKind != "group" {
				g.routeDM(ctx, msg)
				continue
			}
			slog.Info("group message accepted",
				"message_id", msg.MessageID, "account", msg.AccountID,
				"chat_id", msg.ChatID, "is_bot", msg.IsBotMessage, "owner", ownerID)
			g.routeGroup(ctx, msg)
		}
	}
}

// resolveChannelOwner looks up the channels table for the inbound's
// receiving channel and returns the owning user_id, or "" if not found
// or scope==system (system channels have no individual owner).
func (g *Gateway) resolveChannelOwner(ctx context.Context, msg bus.InboundMessage) string {
	if g.store == nil {
		return ""
	}
	rec, err := g.store.LookupChannelByCredential(ctx, msg.Channel, msg.AccountID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("channel lookup failed", "channel", msg.Channel, "error", err)
		}
		return ""
	}
	// channel rows now carry user_id directly — the binder, not the
	// agent owner indirection. The previous "scope=agent → look up
	// agent.user_id" branch is gone because every channel row written
	// by handleConnect* persists the resolved user_id (owner or
	// non-owner) at insert time.
	if rec.UserID != "" {
		return rec.UserID
	}
	// System-level rows (user_id='') still happen in dev installs that
	// pre-seed a global bot. Fall back to the agent owner via agent_id
	// when present so those rows route somewhere sensible.
	if rec.AgentID != "" {
		all, err := g.store.ListAllAgents(ctx)
		if err != nil {
			return ""
		}
		for _, ar := range all {
			if ar.ID == rec.AgentID {
				return ar.UserID
			}
		}
	}
	return ""
}

// resolveChatter normalizes msg.UserID into a fastclaw `u_xxx` id. IM
// channels deliver the platform-side identifier (wechat openid, telegram
// numeric id, …) and the agent loop then keys per-chatter files (USER.md,
// MEMORY.md, per-user skills) under that raw string — which never matches
// the u_xxx rows the dashboard wrote. Translating once at the routing seam
// keeps every downstream consumer of msg.UserID aligned without having to
// teach each one about the IM-side namespace.
//
// Resolution:
//   - empty UserID → "" (caller leaves the slot empty; chatterUserID will
//     fall back to the agent owner).
//   - already `u_`-prefixed → assume it's already canonical, leave alone.
//   - channel owner is an app_user (has apikey_id) → lazy-mint an
//     app_user keyed by (apikey_id, "<channel>:<msg.UserID>") so every
//     distinct IM sender gets a stable u_xxx of their own. Channel name
//     is prefixed so a numeric id colliding across two channel types
//     (telegram chat 123, line user 123) can't merge into one row.
//   - channel owner is a regular user (no apikey_id) → treat as a single-
//     user dogfood/personal bot and pin the chatter to the owner. This
//     preserves the simple "I registered my own wechat to my own agent"
//     flow without forcing the owner to start over with a fresh empty
//     USER.md every conversation.
//
// Returns "" when the original msg.UserID should be kept unchanged
// (empty input, already canonical, or any error path) — the caller treats
// "" as "no rewrite".
func (g *Gateway) resolveChatter(ctx context.Context, ownerID string, msg bus.InboundMessage) string {
	if msg.UserID == "" {
		return ""
	}
	if strings.HasPrefix(msg.UserID, "u_") {
		return ""
	}
	if g.store == nil || g.accounts == nil {
		return ""
	}
	owner, err := g.store.GetUser(ctx, ownerID)
	if err != nil {
		slog.Warn("resolveChatter: owner lookup failed",
			"owner", ownerID, "channel", msg.Channel, "error", err)
		return ""
	}
	if owner.APIKeyID == "" {
		// Personal / dogfood install — every IM sender is treated as the
		// channel owner so the operator's own USER.md applies.
		return ownerID
	}
	extID := msg.Channel + ":" + msg.UserID
	acc, err := g.accounts.EnsureAppUser(ctx, owner.APIKeyID, extID, "")
	if err != nil {
		slog.Warn("resolveChatter: EnsureAppUser failed",
			"apikey", owner.APIKeyID, "ext", extID, "error", err)
		return ""
	}
	return acc.ID
}

// trySteer diverts msg into target's currently in-flight turn instead of
// queuing a separate one. `text` is the body the Submit path would have
// delivered. Returns true when the message was folded into a running
// turn — the caller must then NOT also Submit. False means no turn is
// active; fall back to taskQueue.Submit.
func (g *Gateway) trySteer(target *agent.Agent, msg bus.InboundMessage, text string) bool {
	if target == nil || !target.SteerInbound(msg, text) {
		return false
	}
	slog.Info("message steered into in-flight turn",
		"agent", target.Name(), "channel", msg.Channel, "chat_id", msg.ChatID)
	return true
}

// groupSteerText mirrors the `\[name\]: body` sender labeling
// buildUserMessage applies to queued group turns, so a steered message
// gives the model the same speaker context a normal group turn would.
func groupSteerText(msg bus.InboundMessage) string {
	if msg.SenderName == "" {
		return msg.Text
	}
	return fmt.Sprintf("\\[%s\\]: %s", msg.SenderName, msg.Text)
}

func (g *Gateway) routeDM(ctx context.Context, msg bus.InboundMessage) {
	space, err := g.users.getOrLoad(ctx, msg.OwnerUserID)
	if err != nil {
		slog.Warn("user space load failed", "user", msg.OwnerUserID, "error", err)
		return
	}
	ag := g.matchAgent(ctx, space, msg)
	if ag == nil {
		slog.Warn("no agent matched for DM, dropping",
			"user", msg.OwnerUserID, "channel", msg.Channel,
			"account", msg.AccountID, "chat_id", msg.ChatID)
		return
	}
	slog.Info("routing DM",
		"user", msg.OwnerUserID, "channel", msg.Channel,
		"chat_id", msg.ChatID, "agent", ag.Name())
	if g.trySteer(ag, msg, msg.Text) {
		return
	}
	g.taskQueue.Submit(ag.Name(), chatKey(msg.Channel, msg.AccountID, msg.ChatID), msg, msg.AccountID)
}

func (g *Gateway) routeGroup(ctx context.Context, msg bus.InboundMessage) {
	space, err := g.users.getOrLoad(ctx, msg.OwnerUserID)
	if err != nil {
		slog.Warn("user space load failed", "user", msg.OwnerUserID, "error", err)
		return
	}
	boundAgents := g.agentsBoundToMessage(ctx, space, msg)
	if len(boundAgents) == 0 {
		slog.Warn("no agents bound for group message, dropping",
			"user", msg.OwnerUserID, "chat_id", msg.ChatID)
		return
	}
	if msg.IsBotMessage {
		for _, ag := range boundAgents {
			ag.InjectGroupMessage(ctx, msg)
		}
		if len(msg.Mentions) > 0 {
			if target := g.agentByMention(space, msg, boundAgents); target != nil {
				triggerMsg := msg
				triggerMsg.Text = fmt.Sprintf("\\[%s\\]: %s", msg.SenderName, msg.Text)
				triggerMsg.IsBotMessage = false
				if !g.trySteer(target, triggerMsg, triggerMsg.Text) {
					g.taskQueue.Submit(target.Name(), chatKey(triggerMsg.Channel, triggerMsg.AccountID, triggerMsg.ChatID), triggerMsg, g.accountIDForAgent(space, target.Name(), triggerMsg.Channel))
				}
			}
		}
		return
	}
	if len(msg.Mentions) > 0 {
		if target := g.agentByMention(space, msg, boundAgents); target != nil {
			for _, ag := range boundAgents {
				if ag.Name() != target.Name() {
					ag.InjectGroupMessage(ctx, msg)
				}
			}
			slog.Info("routing group mention",
				"user", msg.OwnerUserID, "channel", msg.Channel,
				"chat_id", msg.ChatID, "agent", target.Name())
			if !g.trySteer(target, msg, groupSteerText(msg)) {
				g.taskQueue.Submit(target.Name(), chatKey(msg.Channel, msg.AccountID, msg.ChatID), msg, g.accountIDForAgent(space, target.Name(), msg.Channel))
			}
			return
		}
	}
	behavior, defaultAgentID := groupBehaviorFor(space, boundAgents)
	switch behavior {
	case "default-agent":
		target := space.Agents.AgentByID(defaultAgentID)
		if target == nil {
			target = boundAgents[0]
		}
		for _, ag := range boundAgents {
			if ag.Name() != target.Name() {
				ag.InjectGroupMessage(ctx, msg)
			}
		}
		if !g.trySteer(target, msg, groupSteerText(msg)) {
			g.taskQueue.Submit(target.Name(), chatKey(msg.Channel, msg.AccountID, msg.ChatID), msg, g.accountIDForAgent(space, target.Name(), msg.Channel))
		}
	default:
		for _, ag := range boundAgents {
			ag.InjectGroupMessage(ctx, msg)
		}
	}
}

func (g *Gateway) matchAgent(ctx context.Context, space *UserSpace, msg bus.InboundMessage) *agent.Agent {
	if space == nil {
		return nil
	}
	// Explicit agent target wins. Cron jobs, web chat, and sub-agent
	// spawns all know the agent at the source — without this, multi-
	// agent users with no web/cron binding fell back to DefaultAgent()
	// which returns nil whenever the manager holds more than one
	// agent, and the message got dropped with "no agent matched for
	// DM, dropping" even though the cron row had AgentID right there.
	if msg.AgentID != "" {
		if ag := space.Agents.AgentByID(msg.AgentID); ag != nil {
			return ag
		}
	}
	bindings := space.Config.Bindings
	if len(bindings) == 0 {
		return space.Agents.DefaultAgent()
	}
	for _, b := range bindings {
		if !matchBinding(b.Match, msg) {
			continue
		}
		if ag := space.Agents.AgentByID(b.AgentID); ag != nil {
			return ag
		}
		// Binding points to an agent the user doesn't own and hasn't
		// been lazy-attached to this space yet — happens with the
		// multi-user channel binding flow where a user binds their
		// own bot to a public agent. Try EnsureAgent and re-check;
		// missing agents (deleted / wrong id) just fall through.
		if err := g.ensureForeignAgent(ctx, space, b.AgentID); err == nil {
			if ag := space.Agents.AgentByID(b.AgentID); ag != nil {
				return ag
			}
		}
	}
	return space.Agents.DefaultAgent()
}

// ensureForeignAgent lazy-attaches an agent that's not in the user's
// own owned set. Wrapper around UserSpace.EnsureAgent that pulls the
// shared store/bus/workspace from the Gateway so callers don't have to
// thread them through. Idempotent: a no-op when the agent is already
// loaded.
func (g *Gateway) ensureForeignAgent(ctx context.Context, space *UserSpace, agentID string) error {
	if space == nil || agentID == "" {
		return nil
	}
	return space.EnsureAgent(ctx, g.store, g.bus, g.workspace, agentID)
}

func matchBinding(m config.Match, msg bus.InboundMessage) bool {
	if m.Channel != "" && m.Channel != msg.Channel {
		return false
	}
	if m.AccountID != "" && m.AccountID != msg.AccountID {
		return false
	}
	if m.Peer != nil {
		if m.Peer.Kind != "" && m.Peer.Kind != msg.PeerKind {
			return false
		}
		if m.Peer.ID != "" && m.Peer.ID != msg.ChatID {
			return false
		}
	}
	return true
}

func (g *Gateway) agentsBoundToMessage(ctx context.Context, space *UserSpace, msg bus.InboundMessage) []*agent.Agent {
	if space == nil {
		return nil
	}
	bindings := space.Config.Bindings
	if len(bindings) == 0 {
		if def := space.Agents.DefaultAgent(); def != nil {
			return []*agent.Agent{def}
		}
		return nil
	}
	seen := make(map[string]bool)
	var out []*agent.Agent
	for _, b := range bindings {
		if !matchBinding(b.Match, msg) || seen[b.AgentID] {
			continue
		}
		ag := space.Agents.AgentByID(b.AgentID)
		if ag == nil {
			// Lazy-attach foreign agent (multi-user channel binding).
			if err := g.ensureForeignAgent(ctx, space, b.AgentID); err == nil {
				ag = space.Agents.AgentByID(b.AgentID)
			}
		}
		if ag != nil {
			seen[b.AgentID] = true
			out = append(out, ag)
		}
	}
	return out
}

// agentByMention picks the candidate agent that should handle a group
// message based on whether the bot that *received* this inbound was
// @-mentioned. Mentions in a group chat only ever address bots present
// in that chat, and exactly one of our adapters is "us" for any given
// inbound — `msg.Channel` + `msg.AccountID` already names that bot, so
// we resolve its username via the channel manager and compare directly.
//
// Previous implementation built a flat agentID→username map from every
// binding the user owned. That silently broke for agents wired up to
// more than one channel (e.g. Telegram + Discord on the same agent):
// the second binding overwrote the first, so mentioning the bot on the
// "loser" channel never matched. See git history if you're tempted to
// reintroduce a per-agent cache.
func (g *Gateway) agentByMention(space *UserSpace, msg bus.InboundMessage, candidates []*agent.Agent) *agent.Agent {
	if g.chanMgr == nil {
		return nil
	}
	botUsername := g.chanMgr.BotUsername(msg.Channel, msg.AccountID)
	slog.Info("agentByMention probe",
		"channel", msg.Channel,
		"account", msg.AccountID,
		"bot_username", botUsername,
		"mentions", msg.Mentions,
		"candidates", agentNames(candidates))
	if botUsername == "" {
		return nil
	}
	var addressed bool
	for _, m := range msg.Mentions {
		if m == botUsername {
			addressed = true
			break
		}
	}
	if !addressed {
		return nil
	}
	for _, b := range space.Config.Bindings {
		if b.Match.Channel != msg.Channel || b.Match.AccountID != msg.AccountID {
			continue
		}
		for _, ag := range candidates {
			if ag.Name() == b.AgentID {
				return ag
			}
		}
	}
	return nil
}

func agentNames(ags []*agent.Agent) []string {
	out := make([]string, 0, len(ags))
	for _, a := range ags {
		out = append(out, a.Name())
	}
	return out
}

// groupBehaviorFor returns the team's groupBehavior + defaultAgent for the
// given candidate agents, or ("mention-only", "") when there's no team.
func groupBehaviorFor(space *UserSpace, agents []*agent.Agent) (string, string) {
	if space == nil {
		return "mention-only", ""
	}
	for _, team := range space.Config.Teams {
		matching := 0
		for _, ag := range agents {
			for _, member := range team.Agents {
				if member == ag.Name() {
					matching++
					break
				}
			}
		}
		if matching == len(agents) && matching > 0 {
			behavior := team.GroupBehavior
			if behavior == "" {
				behavior = "mention-only"
			}
			return behavior, team.DefaultAgent
		}
	}
	return "mention-only", ""
}

func (g *Gateway) accountIDForAgent(space *UserSpace, agentID, channel string) string {
	for _, b := range space.Config.Bindings {
		if b.AgentID == agentID && b.Match.Channel == channel && b.Match.AccountID != "" {
			return b.Match.AccountID
		}
	}
	return ""
}

// gatewaySubAgentSpawner implements tools.SubAgentSpawner. Sub-agents
// always run inside the *same* user's agent manager — there's no cross-
// tenant agent invocation.
type gatewaySubAgentSpawner struct {
	gateway *Gateway
	userID  string
}

func (s *gatewaySubAgentSpawner) SpawnSubAgent(ctx context.Context, agentID string, msg bus.InboundMessage) string {
	space, err := s.gateway.users.getOrLoad(ctx, s.userID)
	if err != nil {
		return fmt.Sprintf("Error: load user space: %v", err)
	}
	ag := space.Agents.AgentByID(agentID)
	if ag == nil {
		return fmt.Sprintf("Error: agent %q not found", agentID)
	}
	return ag.HandleMessage(ctx, msg)
}

var _ tools.SubAgentSpawner = (*gatewaySubAgentSpawner)(nil)

// webhookAgentHandler routes a webhook payload to the named agent within
// the resolved user's space.
type webhookAgentHandler struct {
	gateway *Gateway
}

func (h *webhookAgentHandler) HandleMessage(ctx context.Context, agentID string, msg bus.InboundMessage) (string, error) {
	if msg.OwnerUserID == "" {
		return "", fmt.Errorf("webhook: owner user_id required")
	}
	space, err := h.gateway.users.getOrLoad(ctx, msg.OwnerUserID)
	if err != nil {
		return "", err
	}
	ag := space.Agents.AgentByID(agentID)
	if ag == nil {
		return "", fmt.Errorf("agent %q not found for user %q", agentID, msg.OwnerUserID)
	}
	return ag.HandleMessage(ctx, msg), nil
}
