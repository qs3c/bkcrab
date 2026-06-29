package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

// chatKey 是任务队列使用的每会话序列化键，以便一个聊天的消息顺序运行。包含 accountID
// 因为同一通道类型的两个 bot 可能有冲突的 chat_id（例如 bot A 上的 Telegram 聊天 12345
// 与 bot B 上的聊天 12345 无关）— 没有它，这些会相互序列化，一个 bot 的慢轮次会阻塞另一个。
func chatKey(channel, accountID, chatID string) string {
	return channel + ":" + accountID + ":" + chatID
}

// processInbound 消费消息总线并将每条消息路由到正确的用户代理。身份解析顺序：
//  1. msg.OwnerUserID 显式设置（cron、带 user_id 的 webhook）
//  2. 在 channels 表中查找接收通道的行 — 其 (scope, scope_id) 告诉我们哪个用户拥有此对话
//
// 如果两者都无法产生 user_id，则消息被丢弃，永远不会静默路由到默认身份。
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

			// 去重在聊天者解析之前运行，这样重复的入站消息不会产生 EnsureAppUser 成本
			//（并且如果我们添加此类跟踪，不会错误地增加该用户的上次看到时间）。涵盖 DM 和群组 —
			// 每种类型的键策略见 isDuplicate。
			if g.isDuplicate(msg) {
				slog.Info("dropping duplicate inbound",
					"channel", msg.Channel, "chat_id", msg.ChatID,
					"message_id", msg.MessageID, "peer_kind", msg.PeerKind,
					"account", msg.AccountID)
				continue
			}

			// 将 msg.UserID 归一化为 bkcrab `u_xxx` id。IM 通道
			//（微信、Telegram、LINE、Discord、飞书、Slack）发出原始的平台侧标识符，
			// 这与每个聊天者数据（USER.md、MEMORY.md、每个用户的技能）存储的键不匹配 —
			// 因此没有转换，代理每轮都会得到一个空的聊天者配置文件。延迟铸造语义见 resolveChatter。
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

// resolveChannelOwner 在 channels 表中查找入站消息的接收通道，返回拥有的 user_id，
// 如果未找到或 scope==system（系统通道没有个人所有者）则返回 ""。
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
	// 通道行现在直接携带 user_id — 绑定者，而不是代理所有者间接引用。
	// 之前的 "scope=agent → 查找 agent.user_id" 分支已消失，
	// 因为每个由 handleConnect* 写入的通道行在插入时持久化了已解析的 user_id（所有者或非所有者）。
	if rec.UserID != "" {
		return rec.UserID
	}
	// 系统级行（user_id=''）仍然会出现在预配置了全局 bot 的开发安装中。
	// 当存在时通过 agent_id 回退到代理所有者，以便这些行路由到合理的位置。
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

// resolveChatter 将 msg.UserID 归一化为 bkcrab `u_xxx` id。IM 通道传递平台侧标识符
//（微信 openid、Telegram 数字 id 等），代理循环然后将每个聊天者文件
//（USER.md、MEMORY.md、每个用户的技能）以该原始字符串为键存储 — 这与仪表盘写入的 u_xxx 行从不匹配。
// 在路由接缝处转换一次，保持 msg.UserID 的每个下游消费者一致，无需让每个消费者了解 IM 侧命名空间。
//
// 解析：
//   - 空的 UserID → ""（调用者留空槽位；chatterUserID 将回退到代理所有者）。
//   - 已经以 `u_` 为前缀 → 假定已经是规范形式，保持不变。
//   - 通道所有者是 app_user（有 apikey_id）→ 延迟铸造一个以 (apikey_id, "<channel>:<msg.UserID>")
//     为键的 app_user，以便每个不同的 IM 发送者获得自己的稳定 u_xxx。通道名称被前缀化，
//     这样跨两个通道类型的数字 id 冲突（telegram 聊天 123、line 用户 123）不能合并为一行。
//   - 通道所有者是普通用户（没有 apikey_id）→ 视为单用户 dogfood/个人 bot，
//     将聊天者固定到所有者。这保留了简单的"我将自己的微信注册到自己的代理"流程，
//     无需强制所有者每次对话都以新的空白 USER.md 重新开始。
//
// 当原始 msg.UserID 应保持不变时返回 ""
//（空输入、已经是规范形式或任何错误路径）— 调用者将 "" 视为"不重写"。
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
		// 个人/dogfood 安装 — 每个 IM 发送者被视为通道所有者，因此操作员自己的 USER.md 适用。
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

// trySteer 将 msg 转入目标当前正在进行的轮次，而不是排队单独的轮次。`text` 是 Submit 路径会传递的主体。
// 当消息被合并到正在运行的轮次中时返回 true — 调用者随后不得再 Submit。
// false 表示没有活跃的轮次；回退到 taskQueue.Submit。
func (g *Gateway) trySteer(target *agent.Agent, msg bus.InboundMessage, text string) bool {
	if target == nil || !target.SteerInbound(msg, text) {
		return false
	}
	slog.Info("message steered into in-flight turn",
		"agent", target.Name(), "channel", msg.Channel, "chat_id", msg.ChatID)
	return true
}

// groupSteerText 镜像了 buildUserMessage 应用于排队群组轮次的 `\[name\]: body` 发送者标签，
// 以便被转向的消息为模型提供与普通群组轮次相同的说话者上下文。
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
	// 显式代理目标优先。Cron 任务、web 聊天和子代理生成都在源头知道代理 —
	// 没有这个，没有 web/cron 绑定的多代理用户会回退到 DefaultAgent()，
	// 当管理器持有多个代理时返回 nil，消息被丢弃并显示"no agent matched for DM, dropping"，
	// 即使 cron 行中有 AgentID。
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
		// 绑定指向用户不拥有且尚未延迟附加到此空间的代理 — 发生在多用户通道绑定流程中，
		// 用户将自己的 bot 绑定到公共代理。尝试 EnsureAgent 并重新检查；
		// 缺失的代理（已删除/错误的 id）直接跳过。
		if err := g.ensureForeignAgent(ctx, space, b.AgentID); err == nil {
			if ag := space.Agents.AgentByID(b.AgentID); ag != nil {
				return ag
			}
		}
	}
	return space.Agents.DefaultAgent()
}

// ensureForeignAgent 延迟附加一个不在用户自己拥有集合中的代理。UserSpace.EnsureAgent 的包装器，
// 从 Gateway 获取共享的 store/bus/workspace，因此调用者无需传递它们。
// 幂等的：当代理已加载时为空操作。
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
			// 延迟附加外部代理（多用户通道绑定）。
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

// agentByMention 根据收到此入站消息的 bot 是否被 @-提及来选择应处理群组消息的候选代理。
// 群聊中的提及时只针对该聊天中存在的 bot，对于任何给定的入站消息恰好有一个适配器是"我们" —
// `msg.Channel` + `msg.AccountID` 已经命名了该 bot，因此我们通过通道管理器解析其用户名并直接比较。
//
// 先前的实现从用户拥有的每个绑定构建了一个扁平的 agentID→username 映射。
// 这对于连接到多个通道的代理（例如 Telegram + Discord 在同一代理上）静默失败：
// 第二个绑定覆盖了第一个，因此在"失败"通道上提及 bot 从不匹配。
// 如果你倾向于重新引入每个代理的缓存，请参见 git 历史。
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

// groupBehaviorFor 返回团队的 groupBehavior + defaultAgent，
// 如果没有团队则返回 ("mention-only", "")。
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

// gatewaySubAgentSpawner 实现了 tools.SubAgentSpawner。子代理始终在*同一个*用户的代理管理器内运行 —
// 没有跨租户的代理调用。
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

// webhookAgentHandler 将 webhook 负载路由到已解析用户空间内的命名代理。
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
