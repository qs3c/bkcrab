package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
)

// slashResult 保存斜杠命令的结果。
//
// continuationQueued 标记将后续消息推送到 bus.Inbound 的斜杠命令
// （目前为 /goal foo 和 /goal resume）。HandleMessage 使用它来
// 发出 `turn_pending` 事件而不是 `done`，使调用者的 SSE 流
// 保持打开直到继续自己的 `done` 到达——这样在模型思考间隙期间
// 打字指示器保持可见。
type slashResult struct {
	handled            bool
	reply              string
	continuationQueued bool
}

// handleSlashCommand 检查消息是否为斜杠命令并处理它。ctx 透传给需要发起
// LLM 调用的子命令（目前是 /compact 的摘要调用），其余子命令忽略它。
func (a *Agent) handleSlashCommand(ctx context.Context, msg bus.InboundMessage) slashResult {
	text := strings.TrimSpace(msg.Text)
	if !strings.HasPrefix(text, "/") {
		return slashResult{}
	}

	parts := strings.Fields(text)
	cmd := strings.ToLower(parts[0])
	// 剥离 @botname 后缀：/status@mybot → /status
	if idx := strings.Index(cmd, "@"); idx > 0 {
		cmd = cmd[:idx]
	}
	args := parts[1:]

	// 仅限拥有者的写命令门控。只读检查（/status、/usage、/insights、
	// /help、/version、/start、/whoami）保持开放，使任何群组成员都能
	// 自助获取信息。修改代理运行时状态（model、personality）或会话
	// 历史（new/reset/undo/retry/compact）的变更操作限制为代理拥有者
	// + 按频道管理员白名单——没有此门控，Discord 公会中的任何人
	// 都可以 `/model haiku` 并静默降级所有其他人的共享代理。
	if writeSlashCommands[cmd] && !a.isAdminChatter(msg) {
		return slashResult{
			handled: true,
			reply:   fmt.Sprintf("🔒 `%s` 只有 agent owner / admin 能用。让 owner 把你的 platform 用户 ID 加进 agent.json 的 `admins.%s` 里(用 `/whoami` 查自己的 ID)。", cmd, msg.Channel),
		}
	}

	switch cmd {
	case "/start":
		return slashResult{
			handled: true,
			reply:   fmt.Sprintf("👋 Hi! I'm %s, your AI assistant.\n\nJust send me a message to chat. Use /help to see available commands.", a.name),
		}

	case "/new", "/reset":
		// 清除附加到旧 session_key 的任何目标——设计
		// §6 选择了"新会话 = 干净状态"而非"目标跟随
		// 聊天"。在 web 短路之前运行，所以前端驱动的
		// /new 也会收割目标行。
		if a.goalStore != nil {
			oldKey := a.resolveSessionKey(msg)
			a.clearGoalForSession(oldKey)
		}
		if msg.Channel == "web" {
			// 对于 web 渠道，不删除会话文件——前端处理新会话创建
			return slashResult{handled: true, reply: "__NEW_SESSION__"}
		}
		// 在相同的 (channel, account, chat) 三元组下生成新会话，
		// 使此对话线程从空白开始但保留前一线程为历史。
		// 后续入站消息通过 Manager.Get 的活跃会话查找
		// 解析到新的（max updated_at）行。
		a.sessions.OpenNewSession(msg.Channel, msg.AccountID, msg.ChatID)
		return slashResult{handled: true, reply: "🔄 New session started. Previous conversation kept as history."}

	case "/retry":
		return a.slashRetry(msg)

	case "/undo":
		return a.slashUndo(msg)

	case "/compact":
		return a.slashCompact(ctx, msg, strings.Join(args, " "))

	case "/status":
		return a.slashStatus(msg)

	case "/usage":
		return a.slashUsage(msg)

	case "/insights":
		days := 7
		if len(args) > 0 {
			fmt.Sscanf(args[0], "%d", &days)
		}
		return a.slashInsights(msg, days)

	case "/personality":
		if len(args) == 0 {
			return a.slashPersonalityList(msg)
		}
		return a.slashPersonalitySet(msg, args[0])

	case "/model":
		if len(args) == 0 {
			return slashResult{handled: true, reply: fmt.Sprintf("Current model: `%s`\n\nUsage: /model <model-name>\nExample: /model gpt-4o-mini", a.model)}
		}
		return a.slashModel(msg, args[0])

	case "/goal":
		return a.slashGoal(msg, args)

	case "/plan":
		return a.slashPlan(msg, args)

	case "/help":
		return slashResult{handled: true, reply: a.slashHelp()}

	case "/version":
		return slashResult{handled: true, reply: fmt.Sprintf("⚡ BkCrab\nAgent: %s\nModel: %s", a.name, a.model)}

	case "/whoami":
		return slashResult{
			handled: true,
			reply: fmt.Sprintf("Channel: `%s`\nYour user ID: `%s`\nSender name: `%s`\n\n(Add this ID to `admins.%s` in the agent config to grant write-slash access.)",
				msg.Channel, msg.UserID, msg.SenderName, msg.Channel),
		}

	default:
		return slashResult{}
	}
}

// writeSlashCommands 是修改代理运行时状态或会话历史
// 并因此需要拥有者/管理员门控的斜杠命令。不在本集合中的
// 任何命令被视为只读并无限制运行。
var writeSlashCommands = map[string]bool{
	"/new":         true,
	"/reset":       true,
	"/undo":        true,
	"/retry":       true,
	"/compact":     true,
	"/model":       true,
	"/personality": true,
}

// isAdminChatter 判断聊天者是否被允许在此频道运行写模式斜杠命令。
//
// Web / api：聊天者的 UserID 是 BkCrab 用户 UUID——拥有者通过
// 与代理的 ownerUserID 直接相等来识别。不需要每平台白名单。
//
// IM 渠道（discord、telegram、slack 等）：UserID 是平台自身的
// 用户 ID（Discord 雪花、Telegram 数字 ID 等），与代理的 BkCrab
// 拥有者没有固有联系。拥有者在 agent.json 的 `admins[channel]`
// 中注册平台 ID 以授予访问权限——并且为了防止单用户开发
// 安装被锁定在自己的代理之外，渠道为空/缺失的白名单会回退到
// "任何人都可以运行"（旧版行为）。关心群聊保护的运维人员填充
// 列表以锁定。
func (a *Agent) isAdminChatter(msg bus.InboundMessage) bool {
	// Web / api 直接携带 BkCrab UUID；拥有者检查即可。
	if msg.Channel == "web" || msg.Channel == "api" {
		return msg.UserID != "" && msg.UserID == a.ownerUserID
	}
	list, ok := a.admins[msg.Channel]
	if !ok || len(list) == 0 {
		// 此渠道未配置白名单 → 保留旧版不受限行为。
		// 运维人员通过填充 admins[channel] 选择加入群聊保护。
		return true
	}
	for _, id := range list {
		if id == msg.UserID {
			return true
		}
	}
	return false
}

// slashRetry 重新运行上一条用户消息，丢弃上一条助手回复。
func (a *Agent) slashRetry(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	msgs := sess.GetMessages()

	// 查找最后一条用户消息
	lastUserIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return slashResult{handled: true, reply: "No previous message to retry."}
	}

	// 保存快照以便撤销
	sess.Snapshot()

	// 裁剪到上一条用户消息之前
	sess.ReplaceMessages(msgs[:lastUserIdx])

	// 将用户消息作为新入站重新注入
	lastUserText := msgs[lastUserIdx].Content
	retryMsg := msg
	retryMsg.Text = lastUserText

	// 信号表示我们要重新处理此消息（返回 not-handled 让网关重试）
	// 但我们在这里返回 handled 以避免双重处理——网关应重新发送
	return slashResult{
		handled: true,
		reply:   fmt.Sprintf("🔁 Retrying: *%s*", truncateSlash(lastUserText, 80)),
	}
}

// slashUndo 撤销上一条助手回复。
func (a *Agent) slashUndo(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)

	if !sess.HasSnapshot() {
		// 无快照——尝试手动删除最后的 user+assistant 轮次
		msgs := sess.GetMessages()
		if len(msgs) < 2 {
			return slashResult{handled: true, reply: "Nothing to undo."}
		}
		// 裁剪尾部的助手消息及之前的用户消息
		end := len(msgs)
		for end > 0 && msgs[end-1].Role == "assistant" {
			end--
		}
		if end > 0 && msgs[end-1].Role == "user" {
			end--
		}
		sess.ReplaceMessages(msgs[:end])
		return slashResult{handled: true, reply: "↩️ Undid last turn."}
	}

	if sess.Undo() {
		return slashResult{handled: true, reply: "↩️ Undid last action."}
	}
	return slashResult{handled: true, reply: "Nothing to undo."}
}

func (a *Agent) slashCompact(ctx context.Context, msg bus.InboundMessage, focus string) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	sessionMsgs := sess.GetMessages()

	if len(sessionMsgs) == 0 {
		return slashResult{handled: true, reply: "No messages to compact."}
	}

	opts := a.compactionOptions(CompactModeManual, nil, nil, sess.SessionKey())
	opts.Focus = focus
	if a.registry != nil {
		// 用回合私有副本，避免在共享 a.registry 上就地改写每回合状态——
		// /compact 与其它会话的回合可能并发。归档本身已由 opts.ArchiveSessionKey
		// （= sess.SessionKey()）作用域化；这里设 archive key 仅为与回合路径对齐，
		// DefinitionsForMode 只是读取工具清单。
		reg := a.registry.ForTurn()
		reg.SetContextArchiveSessionKey(sess.SessionKey())
		opts.ToolDefs = reg.DefinitionsForMode(builtinAllowForMode(a.promptMode))
	}
	result, err := a.compactWithProgress(ctx, sessionMsgs, opts)
	if err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Compaction error: %v", err)}
	}
	if result != nil && result.Pruned {
		sess.ReplaceMessages(result.Messages)
		return slashResult{handled: true, reply: fmt.Sprintf("Compacted checkpoint: %d -> %d messages.", len(sessionMsgs), len(result.Messages))}
	}
	return slashResult{handled: true, reply: "Nothing to compact."}
}

func (a *Agent) slashStatus(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	sessionMsgs := sess.GetMessages()

	memContent := a.memory.LoadMemory()
	memLines := 0
	if memContent != "" {
		memLines = strings.Count(memContent, "\n") + 1
	}

	soul := a.loadSoulName()

	status := fmt.Sprintf("⚡ BkCrab Status\n"+
		"─────────────────\n"+
		"Agent:       %s\n"+
		"Model:       %s\n"+
		"Personality: %s\n"+
		"Max Tokens:  %d\n"+
		"Temperature: %.1f\n"+
		"Max Iter:    %d\n"+
		"Session Msgs:%d\n"+
		"Memory:      %d lines\n"+
		"Workspace:   %s",
		a.name, a.model, soul,
		a.maxTokens, a.temperature, a.maxToolIterations,
		len(sessionMsgs), memLines, a.homePath,
	)
	return slashResult{handled: true, reply: status}
}

func (a *Agent) slashUsage(msg bus.InboundMessage) slashResult {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	msgs := sess.GetMessages()

	userTurns, asstTurns, toolTurns := 0, 0, 0
	for _, m := range msgs {
		switch m.Role {
		case "user":
			userTurns++
		case "assistant":
			asstTurns++
		case "tool":
			toolTurns++
		}
	}

	reply := fmt.Sprintf("📊 Session Usage\n"+
		"User turns:      %d\n"+
		"Assistant turns: %d\n"+
		"Tool calls:      %d\n"+
		"Total messages:  %d",
		userTurns, asstTurns, toolTurns, len(msgs),
	)

	// 从 SDK 引擎追加成本跟踪信息
	if a.costTracker != nil {
		stats := a.costTracker.Stats()
		reply += fmt.Sprintf("\n─────────────────\n"+
			"Cost:            %s\n"+
			"Input tokens:    %v\n"+
			"Output tokens:   %v\n"+
			"API duration:    %vms\n"+
			"Tool duration:   %vms",
			a.costTracker.FormatCost(),
			stats["totalInputTokens"],
			stats["totalOutputTokens"],
			stats["totalAPIDurationMs"],
			stats["totalToolDurationMs"],
		)
	}

	return slashResult{handled: true, reply: reply}
}

func (a *Agent) slashInsights(msg bus.InboundMessage, days int) slashResult {
	logDir := filepath.Join(a.homePath, "memory", "logs")
	cutoff := time.Now().AddDate(0, 0, -days)

	files, _ := filepath.Glob(filepath.Join(logDir, "*.jsonl"))
	totalFiles, recentFiles := 0, 0
	for _, f := range files {
		totalFiles++
		info, err := os.Stat(f)
		if err == nil && info.ModTime().After(cutoff) {
			recentFiles++
		}
	}

	reply := fmt.Sprintf("🔍 Insights (last %d days)\n"+
		"─────────────────────────\n"+
		"Log files:       %d total, %d recent\n"+
		"Memory file:     %s\n"+
		"Workspace:       %s\n\n"+
		"Tip: Use /status for session info, /usage for token stats.",
		days, totalFiles, recentFiles,
		func() string {
			info, err := os.Stat(filepath.Join(a.homePath, "MEMORY.md"))
			if err != nil {
				return "not found"
			}
			return fmt.Sprintf("%.1f KB, updated %s", float64(info.Size())/1024, info.ModTime().Format("2006-01-02 15:04"))
		}(),
		a.homePath,
	)
	return slashResult{handled: true, reply: reply}
}

// slashPersonalityList 列出可用的 SOUL.md 预设。
func (a *Agent) slashPersonalityList(msg bus.InboundMessage) slashResult {
	presets := a.listPersonalities()
	if len(presets) == 0 {
		return slashResult{handled: true, reply: "No personality presets found.\n\nCreate files named SOUL-<name>.md in your workspace to add presets.\nExample: SOUL-assistant.md, SOUL-dev.md"}
	}
	current := a.loadSoulName()
	var sb strings.Builder
	sb.WriteString("🎭 Personalities\n")
	sb.WriteString("─────────────────\n")
	for _, p := range presets {
		if p == current {
			sb.WriteString(fmt.Sprintf("• %s ← current\n", p))
		} else {
			sb.WriteString(fmt.Sprintf("• %s\n", p))
		}
	}
	sb.WriteString("\nUsage: /personality <name>")
	return slashResult{handled: true, reply: sb.String()}
}

// slashPersonalitySet 切换活动的 SOUL.md。
func (a *Agent) slashPersonalitySet(msg bus.InboundMessage, name string) slashResult {
	// 在工作区中查找 SOUL-<name>.md
	srcPath := filepath.Join(a.homePath, fmt.Sprintf("SOUL-%s.md", name))
	if _, err := os.Stat(srcPath); os.IsNotExist(err) {
		return slashResult{handled: true, reply: fmt.Sprintf("Personality '%s' not found.\nExpected: %s", name, srcPath)}
	}

	data, err := os.ReadFile(srcPath)
	if err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error reading personality: %v", err)}
	}

	destPath := filepath.Join(a.homePath, "SOUL.md")
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		return slashResult{handled: true, reply: fmt.Sprintf("Error applying personality: %v", err)}
	}

	return slashResult{handled: true, reply: fmt.Sprintf("🎭 Personality set to: **%s**\nSOUL.md updated. Takes effect on the next message.", name)}
}

// slashModel 切换此代理会话的活动模型。
func (a *Agent) slashModel(msg bus.InboundMessage, model string) slashResult {
	old := a.model
	a.model = model
	a.contextWindow = config.ResolveContextWindow(a.providerConfigs, a.model, a.maxTokens)
	return slashResult{handled: true, reply: fmt.Sprintf("🤖 Model switched: `%s` → `%s`", old, model)}
}

// listPersonalities 在工作区中查找 SOUL-<name>.md 文件。
func (a *Agent) listPersonalities() []string {
	pattern := filepath.Join(a.homePath, "SOUL-*.md")
	files, _ := filepath.Glob(pattern)
	var names []string
	for _, f := range files {
		base := filepath.Base(f)
		// SOUL-<名称>.md → <名称>
		name := strings.TrimPrefix(base, "SOUL-")
		name = strings.TrimSuffix(name, ".md")
		names = append(names, name)
	}
	return names
}

// loadSoulName 返回当前人格名称（如果标准 SOUL.md 则为 default）。
func (a *Agent) loadSoulName() string {
	// 检查当前 SOUL.md 是否为已知预设
	for _, p := range a.listPersonalities() {
		srcPath := filepath.Join(a.homePath, fmt.Sprintf("SOUL-%s.md", p))
		soulPath := filepath.Join(a.homePath, "SOUL.md")
		srcData, err1 := os.ReadFile(srcPath)
		soulData, err2 := os.ReadFile(soulPath)
		if err1 == nil && err2 == nil && string(srcData) == string(soulData) {
			return p
		}
	}
	return "default"
}

func (a *Agent) slashHelp() string {
	return `⚡ BkCrab Commands

Conversation
  /new, /reset    — Clear session history
  /retry          — Re-run last message
  /undo           — Undo last turn

Context
  /compact [focus] — Create a compact context checkpoint
  /status         — Agent status & memory info
  /usage          — Session token/turn stats
  /insights [N]   — Activity insights (last N days, default 7)

Personality & Model
  /personality        — List available personalities
  /personality <name> — Switch personality (SOUL-<name>.md)
  /model <name>       — Switch LLM model

Goal (persistent multi-turn objective)
  /goal <objective> — Create a goal; agent self-continues until done
  /goal             — Show current goal status
  /goal pause       — Pause continuation
  /goal resume      — Resume a paused goal
  /goal clear       — Delete the goal

Plan
  /plan <task>      — Run <task> in plan mode: emit a numbered plan, no tool calls

Info
  /help           — Show this help
  /version        — Show version
  /whoami         — Show your platform user ID

🔒 Write commands (/new /reset /undo /retry /compact /model /personality)
   in IM channels are restricted to the agent owner + admins listed in
   agent.json's "admins" field. Use /whoami to find your ID.`
}

// slashPlan 处理 `/plan <task>`：将消息的剩余部分重新发布到
// bus.Inbound 并设置 planMode=true，使常规 HandleMessage 路径
// 将其路由到 handlePlanMode。这是自动计划启发式的手动替代——
// 用户按轮次显式选择，而非服务器从消息形状猜测。
func (a *Agent) slashPlan(msg bus.InboundMessage, args []string) slashResult {
	task := strings.TrimSpace(strings.Join(args, " "))
	if task == "" {
		return slashResult{handled: true, reply: "Usage: `/plan <task>`"}
	}

	// 克隆入站消息，使路由字段（channel、account、chat、
	// project、user、sender、owner）原样传递。仅重写
	// Text 和 Params——plan-mode 标志是 handlePlanMode 的关键
	// （参见 loop.go 中的 isPlanMode）。
	out := msg
	out.Text = task
	params := map[string]any{}
	for k, v := range msg.Params {
		params[k] = v
	}
	params["planMode"] = true
	out.Params = params

	select {
	case a.messageBus.Inbound <- out:
		return slashResult{handled: true, reply: "", continuationQueued: true}
	default:
		return slashResult{handled: true, reply: "Bus full, try again."}
	}
}

func truncateSlash(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
