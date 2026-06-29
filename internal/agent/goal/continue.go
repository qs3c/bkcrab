package goal

import (
	"context"
	"errors"
	"log/slog"

	"github.com/qs3c/bkcrab/internal/bus"
)

// TryFireContinuation 为一个（代理、会话）运行门级联
// 如果成功，则将继续提示发布回总线上。
// 可以安全地从 PostTurn/slash 处理程序同步调用 — 门
// 成本低廉（一次索引读取），任何失败都是静默无操作。
//
// 大门（按顺序）：
// - 存在此目标（代理、会话）
// - 目标状态为活动（已暂停/预算有限/完全无操作）
// - 目标携带路由信息（遗留行缺少路由字段
// 不能自动继续；发布将发出不可路由的
// 入境）
//
// 错误落在警告级别而不是阻止调用者 -
// 继续是尽力而为，下一个 PostTurn 将重试。
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

// 发布推送目标上下文提示（继续或预算限制
// 总结）到公共汽车上。标记为bus.SourceGoalContext所以
// 代理循环可以区分运行时注入的目标提示和真实的目标提示
// 用户输入并使用 OriginGoalContext 标记结果消息。
// 排队时返回 true，总线已满时返回 false。
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
