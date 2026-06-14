package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/agent/goal"
	"github.com/qs3c/bkclaw/internal/bus"
)

// slashGoal 将 `/goal …` 分派到子处理器。参数语法遵循设计文档 § 6：
//
//	/goal <objective>          → 创建
//	/goal                      → 显示当前目标
// /目标暂停|简历 |清除
//
// `/goal budget <N>` 被故意省略——Codex 没有附带它，
// 且飞行中的预算修改语义模糊（已花费的 token 是否计入？）。
// 改为在创建时设置预算。
func (a *Agent) slashGoal(msg bus.InboundMessage, args []string) slashResult {
	if a.goalStore == nil {
		return slashResult{
			handled: true,
			reply:   "The /goal feature isn't enabled on this install (no data store configured).",
		}
	}

	// 第一个参数可能是子命令。其他任何内容被视为创建路径的目标文本。
	// 否则 `/goal pause objective` 会有歧义，但 pause/resume/clear 是
	// 没有人会用作目标开头的短关键字。
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
	// 默认：将整个剩余部分视为目标文本。
	objective := strings.Join(args, " ")
	return a.slashGoalCreate(msg, objective)
}

// resolveSessionKey 返回进行中轮次的持久 session_key，
// 如果没有会话匹配入站消息的 (channel, account, chat, project) 元组
// 则返回 ""。斜杠处理器在 "" 时降级为清晰的错误消息。
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
	// 纯文本状态——无 emoji 前缀或支架。/goal 是唯一成功时返回
	// 可见文本的命令；pause / resume / clear / create 都保持静默。
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

	// 立即从用户自己的 /goal 轮次触发第一次继续。静默成功——
	// 继续流回本身就是对话回复，与用户直接输入目标相同。
	// Goal 在聊天界面上是透明的；没有支架文本。
	goal.TryFireContinuation(context.Background(), a.goalStore, a.messageBus, a.name, key)
	return slashResult{handled: true, reply: "", continuationQueued: true}
}

func (a *Agent) slashGoalPause(msg bus.InboundMessage) slashResult {
	// 静默转换。错误状态/无目标情况仍然会暴露。
	return a.transitionGoal(msg, goal.StatusActive, goal.StatusPaused, "Not active.")
}

func (a *Agent) slashGoalResume(msg bus.InboundMessage) slashResult {
	res := a.transitionGoal(msg, goal.StatusPaused, goal.StatusActive, "Not paused.")
	// 空回复 == 成功路径；非空 == wrongStateMsg 或
	// 错误。仅在成功时触发下一次继续。
	if res.handled && res.reply == "" {
		key := a.resolveSessionKey(msg)
		goal.TryFireContinuation(context.Background(), a.goalStore, a.messageBus, a.name, key)
		res.continuationQueued = true
	}
	return res
}

// transitionGoal 集中处理"加载目标 → 检查处于预期源状态 →
// 翻转 → 持久化"的暂停/恢复模式。成功时回复为静默（""）；
// 错误状态时返回 wrongStateMsg；存储错误时返回格式化错误。
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

// clearGoalForSession 移除附加在指定 session_key 上的目标。
// 由 /new 和 /reset 调用，使旧会话的目标不会泄漏到同一聊天上的
// 全新对话线程。尽力而为：存储错误不会暴露——/new 不应该因为
// 一个残留的目标行而失败。
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
