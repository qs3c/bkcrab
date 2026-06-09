package agent

import (
	"context"
	"log/slog"
	"time"

	"github.com/bkclaw-ai/bkclaw/internal/provider"
)

// HookPoint identifies where in the agent loop a hook fires.
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

// HookContext carries data available to hooks at each hook point.
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
	// Channel + AccountID complete the bus routing triple. Plugins
	// reading these in a hook.fire payload can echo them back to
	// chat.send so a follow-up message reaches the same chat that
	// just got the agent's reply.
	Channel   string
	AccountID string
	// Source mirrors bus.InboundMessage.Source so PostTurn hooks can
	// distinguish a real user turn from a cron / heartbeat / sub-agent
	// / goal-context turn. Empty means user. Hooks that should only
	// fire on user-originated turns (notably the goal trigger) gate
	// on this.
	Source string

	// GoalSessionKey is the persistent session_key for the in-flight
	// turn. The goal-accounting hook reads it to look up the active
	// goal (if any) for this session. Empty when the turn happened
	// outside a chat context.
	GoalSessionKey string

	// IsPlanMode reports whether this turn ran in plan-mode (model
	// emits a plan, doesn't act). Goal trigger hooks gate on this so
	// a plan-only turn doesn't auto-fire a continuation behind the
	// user's back — plan mode exists precisely to let the user review
	// before more work happens.
	IsPlanMode bool
}

// HookFunc is a function that runs at a hook point.
// It can inspect and modify the HookContext.
type HookFunc func(ctx context.Context, hc *HookContext)

// HookRegistry stores registered hooks per hook point.
type HookRegistry struct {
	hooks map[HookPoint][]HookFunc
}

// NewHookRegistry creates a new hook registry.
func NewHookRegistry() *HookRegistry {
	return &HookRegistry{
		hooks: make(map[HookPoint][]HookFunc),
	}
}

// Register adds a hook function for the given hook point.
func (hr *HookRegistry) Register(point HookPoint, fn HookFunc) {
	hr.hooks[point] = append(hr.hooks[point], fn)
}

// Run executes all hooks registered at the given point.
func (hr *HookRegistry) Run(ctx context.Context, hc *HookContext) {
	for _, fn := range hr.hooks[hc.Point] {
		fn(ctx, hc)
	}
}

// LoggingHook returns a hook function that logs timing information.
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
