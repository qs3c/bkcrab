package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/qs3c/bkclaw/internal/provider"
)

// HookPoint 标识代理循环中钩子触发的位置。
type HookPoint int

const (
	BeforeSystemPrompt HookPoint = iota
	AfterSystemPrompt
	BeforeModelCall
	AfterModelCall
	BeforeToolCall
	AfterToolCall
	PostTurn // fires after a complete agent turn (response + all tool calls)
)

// HookContext 携带每个钩子点处的钩子可用的数据。
type HookContext struct {
	AgentName     string
	Point         HookPoint
	Messages      []provider.Message
	ToolName      string // for tool-related hooks
	ToolArgs      string // for BeforeToolCall
	ToolResult    string // for AfterToolCall
	Response      *provider.Response
	Error         error
	StartTime     time.Time // set at BeforeModelCall/BeforeToolCall for timing
	TurnCount     int       // incremented each agent turn (for PostTurn)
	ToolCallCount int       // total tool calls in this turn (for PostTurn)
	Workspace     string    // agent workspace path (for PostTurn)
	UserID        string    // owning user ID for multi-user namespace isolation
	ChatID        string    // used by the plugin hook adapter
	// Channel + AccountID 完成总线路由三元组。插件
	// 在 hook.fire 有效负载中读取这些内容可以将它们回显
	// chat.send 以便后续消息到达相同的聊天
	// 刚刚收到agent的回复。
	Channel   string
	AccountID string
	// 源镜像bus.InboundMessage.Source，因此PostTurn钩子可以
	// 区分真实用户轮次和 cron/心跳/子代理
	// / 目标上下文转向。空表示用户。钩子应该只
	// 在用户发起的回合（特别是目标触发器）门上触发
	// 关于这一点。
	Source string

	// GoalSessionKey 是飞行中的持久 session_key
	// 转动。目标会计钩子读取它以查找活动的
	// 本次会议的目标（如果有）。转弯时为空
	// 在聊天上下文之外。
	GoalSessionKey string

	// IsPlanMode 报告此回合是否在计划模式下运行（模型
	// 制定计划，但不采取行动）。目标触发钩子门在此所以
	// 仅计划转弯不会自动触发后面的延续
	// 用户的背影——计划模式正是为了让用户回顾而存在的
	// 在进行更多工作之前。
	IsPlanMode bool
}

// HookFunc 是一个在钩子点运行的函数。
// 它可以检查和修改 HookContext。
type HookFunc func(ctx context.Context, hc *HookContext)

// HookRegistry 存储每个钩子点已注册的钩子。
type HookRegistry struct {
	hooks map[HookPoint][]HookFunc
}

// NewHookRegistry 创建一个新的钩子注册表。
func NewHookRegistry() *HookRegistry {
	return &HookRegistry{
		hooks: make(map[HookPoint][]HookFunc),
	}
}

// Register为给定的钩子点添加一个钩子函数。
func (hr *HookRegistry) Register(point HookPoint, fn HookFunc) {
	hr.hooks[point] = append(hr.hooks[point], fn)
}

// Run 执行在给定点注册的所有钩子。
func (hr *HookRegistry) Run(ctx context.Context, hc *HookContext) {
	for _, fn := range hr.hooks[hc.Point] {
		fn(ctx, hc)
	}
}

// LoggingHook 返回一个记录计时信息的钩子函数。
func LoggingHook() HookFunc {
	return func(ctx context.Context, hc *HookContext) {
		switch hc.Point {
		case BeforeModelCall:
			hc.StartTime = time.Now()
			slog.Info("hook: before model call", "agent", hc.AgentName)
		case AfterModelCall:
			elapsed := time.Since(hc.StartTime)
			hasTools := hc.Response != nil && hc.Response.HasToolCalls()
			slog.Info("hook: after model call",
				"agent", hc.AgentName,
				"elapsed", elapsed,
				"has_tool_calls", hasTools,
			)
		case BeforeToolCall:
			hc.StartTime = time.Now()
			slog.Info("hook: before tool call",
				"agent", hc.AgentName,
				"tool", hc.ToolName,
			)
		case AfterToolCall:
			elapsed := time.Since(hc.StartTime)
			slog.Info("hook: after tool call",
				"agent", hc.AgentName,
				"tool", hc.ToolName,
				"elapsed", elapsed,
				"error", hc.Error,
			)
		case BeforeSystemPrompt:
			slog.Debug("hook: before system prompt", "agent", hc.AgentName)
		case AfterSystemPrompt:
			slog.Debug("hook: after system prompt", "agent", hc.AgentName)
		}
	}
}
