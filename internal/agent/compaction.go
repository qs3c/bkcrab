package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/qs3c/bkclaw/internal/provider"
)

const (
	// DefaultTokenThreshold is kept for compatibility with older callers/tests.
	DefaultTokenThreshold = 80000
	DefaultContextWindow  = 128000
)

type CompactMode string

const (
	CompactModeProactive CompactMode = "proactive"
	CompactModeManual    CompactMode = "manual"
	CompactModeEmergency CompactMode = "emergency"

	DefaultCompactionTriggerPercent = 75
	DefaultCompactionTargetPercent  = 55
	DefaultTailTurns                = 4
	MinimumTailTurns                = 2
	DefaultSummaryMaxRetries        = 3
)

const (
	// PruneTurnAge is the number of most-recent messages preserved verbatim by
	// the legacy compaction path. Later tasks replace this with turn-aware tail
	// selection, so Task 2 intentionally keeps the existing behavior.
	PruneTurnAge = 20

	truncatedPlaceholder = "[Result truncated - see memory logs]"
)

type CompactOptions struct {
	Mode              CompactMode
	Workspace         string
	Provider          provider.Provider
	Model             string
	ContextWindow     int
	MaxOutputTokens   int
	TriggerPercent    int
	TargetPercent     int
	TailTurns         int
	MinTailTurns      int
	Focus             string
	OverheadMessages  []provider.Message
	ToolDefs          []provider.Tool
	SummaryMaxRetries int
}

// EstimateTokens provides a rough token estimate: chars/4.
func EstimateTokens(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		content := m.Content
		if content == "" && len(m.ContentParts) > 0 {
			content = m.TextContent()
		}
		total += len(content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
			total += len(tc.Function.Name) / 4
		}
	}
	return total
}

func EstimateRequestTokens(messages []provider.Message, tools []provider.Tool) int {
	total := EstimateTokens(messages)
	for _, tool := range tools {
		if b, err := json.Marshal(tool); err == nil {
			total += len(b) / 4
			continue
		}
		total += len(tool.Type) / 4
		total += len(tool.Function.Name) / 4
		total += len(tool.Function.Description) / 4
	}
	return total
}

// CompactResult stores the result of a compaction attempt.
type CompactResult struct {
	Messages []provider.Message
	Pruned   bool
	LogFile  string
}

// CompactMessages keeps the original API and delegates to the options-based
// implementation with proactive defaults.
func CompactMessages(messages []provider.Message, workspace string, prov provider.Provider, model string) (*CompactResult, error) {
	return CompactMessagesWithOptions(messages, CompactOptions{
		Mode:              CompactModeProactive,
		Workspace:         workspace,
		Provider:          prov,
		Model:             model,
		ContextWindow:     DefaultContextWindow,
		TriggerPercent:    DefaultCompactionTriggerPercent,
		TargetPercent:     DefaultCompactionTargetPercent,
		TailTurns:         DefaultTailTurns,
		MinTailTurns:      MinimumTailTurns,
		SummaryMaxRetries: DefaultSummaryMaxRetries,
	})
}

func CompactMessagesWithOptions(messages []provider.Message, opts CompactOptions) (*CompactResult, error) {
	opts = normalizeCompactOptions(opts)
	tokens := EstimateRequestTokens(compactionRequestMessages(messages, opts.OverheadMessages), opts.ToolDefs)

	switch opts.Mode {
	case CompactModeProactive:
		if tokens < compactTriggerLimit(opts) {
			// Task 3 adds tool-pair sanitization. For Task 2, the fast path must
			// preserve the original slice exactly.
			return &CompactResult{Messages: messages}, nil
		}
		return compactMessagesTriggered(messages, opts, tokens)
	case CompactModeManual:
		return compactMessagesTriggered(messages, opts, tokens)
	case CompactModeEmergency:
		return compactMessagesEmergencyPlaceholder(messages, opts, tokens)
	default:
		if tokens < compactTriggerLimit(opts) {
			return &CompactResult{Messages: messages}, nil
		}
		return compactMessagesTriggered(messages, opts, tokens)
	}
}

func normalizeCompactOptions(opts CompactOptions) CompactOptions {
	switch opts.Mode {
	case "", CompactModeProactive:
		opts.Mode = CompactModeProactive
	case CompactModeManual, CompactModeEmergency:
	default:
		opts.Mode = CompactModeProactive
	}
	if opts.ContextWindow <= 0 {
		opts.ContextWindow = DefaultContextWindow
	}
	if opts.MaxOutputTokens < 0 {
		opts.MaxOutputTokens = 0
	}
	if opts.TriggerPercent <= 0 {
		opts.TriggerPercent = DefaultCompactionTriggerPercent
	}
	if opts.TriggerPercent > 100 {
		opts.TriggerPercent = 100
	}
	if opts.TargetPercent <= 0 {
		opts.TargetPercent = DefaultCompactionTargetPercent
	}
	if opts.TargetPercent > 100 {
		opts.TargetPercent = 100
	}
	if opts.TailTurns <= 0 {
		opts.TailTurns = DefaultTailTurns
	}
	if opts.MinTailTurns <= 0 {
		opts.MinTailTurns = MinimumTailTurns
	}
	if opts.SummaryMaxRetries <= 0 {
		opts.SummaryMaxRetries = DefaultSummaryMaxRetries
	}
	return opts
}

func compactTriggerLimit(opts CompactOptions) int {
	return percentOf(compactInputBudget(opts), opts.TriggerPercent)
}

func compactTargetLimit(opts CompactOptions) int {
	return percentOf(compactInputBudget(opts), opts.TargetPercent)
}

func compactInputBudget(opts CompactOptions) int {
	budget := opts.ContextWindow - opts.MaxOutputTokens
	if budget <= 0 {
		return 1
	}
	return budget
}

func percentOf(n, percent int) int {
	if n <= 0 || percent <= 0 {
		return 0
	}
	return n * percent / 100
}

func compactionRequestMessages(messages, overhead []provider.Message) []provider.Message {
	request := make([]provider.Message, 0, len(overhead)+len(messages))
	request = append(request, overhead...)
	request = append(request, messages...)
	return request
}

func compactMessagesEmergencyPlaceholder(messages []provider.Message, opts CompactOptions, tokens int) (*CompactResult, error) {
	return compactMessagesTriggered(messages, opts, tokens)
}

func compactMessagesTriggered(messages []provider.Message, opts CompactOptions, tokens int) (*CompactResult, error) {
	slog.Info(
		"context compaction triggered",
		"tokens", tokens,
		"threshold", compactTriggerLimit(opts),
		"message_count", len(messages),
		"mode", opts.Mode,
	)

	logFile, err := writeHistoryLog(messages, opts.Workspace)
	if err != nil {
		slog.Warn("failed to write history log", "error", err)
	}

	pruned, prunedChanged := pruneOldToolResultsWithChange(messages)
	prunedTokens := EstimateRequestTokens(compactionRequestMessages(pruned, opts.OverheadMessages), opts.ToolDefs)

	slog.Info("after pruning", "tokens_before", tokens, "tokens_after", prunedTokens)

	if prunedTokens < compactTargetLimit(opts) {
		return &CompactResult{
			Messages: pruned,
			Pruned:   prunedChanged,
			LogFile:  logFile,
		}, nil
	}

	if len(pruned) <= PruneTurnAge {
		return &CompactResult{
			Messages: pruned,
			Pruned:   prunedChanged,
			LogFile:  logFile,
		}, nil
	}

	compressed, err := compressOlderMessages(pruned, opts.Provider, opts.Model)
	if err != nil {
		slog.Warn("compression failed, using pruned messages", "error", err)
		return &CompactResult{
			Messages: pruned,
			Pruned:   prunedChanged,
			LogFile:  logFile,
		}, nil
	}

	slog.Info(
		"after compression",
		"tokens_before", prunedTokens,
		"tokens_after", EstimateRequestTokens(compactionRequestMessages(compressed, opts.OverheadMessages), opts.ToolDefs),
	)

	return &CompactResult{
		Messages: compressed,
		Pruned:   true,
		LogFile:  logFile,
	}, nil
}

// safeCompactionCutoff advances a cutoff beyond any leading tool messages so
// the preserved tail never starts with a tool result without its parent
// assistant.tool_calls message.
func safeCompactionCutoff(messages []provider.Message, cutoff int) int {
	if cutoff < 0 {
		cutoff = 0
	}
	for cutoff < len(messages) && messages[cutoff].Role == "tool" {
		cutoff++
	}
	return cutoff
}

// pruneOldToolResults strips large tool result bodies from older messages.
func pruneOldToolResults(messages []provider.Message) []provider.Message {
	result, _ := pruneOldToolResultsWithChange(messages)
	return result
}

func pruneOldToolResultsWithChange(messages []provider.Message) ([]provider.Message, bool) {
	if len(messages) <= PruneTurnAge {
		return messages, false
	}

	cutoff := len(messages) - PruneTurnAge
	result := make([]provider.Message, len(messages))
	copy(result, messages)

	changed := false
	for i := 0; i < cutoff; i++ {
		if result[i].Role == "tool" && len(result[i].Content) > 200 {
			result[i] = provider.Message{
				Role:       "tool",
				Content:    truncatedPlaceholder,
				ToolCallID: result[i].ToolCallID,
				Name:       result[i].Name,
			}
			changed = true
		}
	}

	return result, changed
}

// compressOlderMessages asks the LLM to summarize older messages.
func compressOlderMessages(messages []provider.Message, prov provider.Provider, model string) ([]provider.Message, error) {
	if len(messages) <= PruneTurnAge {
		return messages, nil
	}

	cutoff := safeCompactionCutoff(messages, len(messages)-PruneTurnAge)
	olderMessages := messages[:cutoff]

	var text string
	for _, m := range olderMessages {
		if m.Origin != provider.OriginUser {
			continue
		}
		text += fmt.Sprintf("[%s] %s\n", m.Role, m.Content)
	}

	summaryPrompt := []provider.Message{
		{
			Role:    "system",
			Content: "You are a conversation summarizer. Summarize the following conversation history into a compact summary that preserves key facts, decisions, and context. Be concise but don't lose important details.",
		},
		{
			Role:    "user",
			Content: fmt.Sprintf("Summarize this conversation:\n\n%s", text),
		},
	}

	resp, err := prov.Chat(nil, summaryPrompt, nil, model, 2048, 0.3)
	if err != nil {
		return nil, fmt.Errorf("summarize conversation: %w", err)
	}

	compressed := make([]provider.Message, 0, PruneTurnAge+1)
	compressed = append(compressed, provider.Message{
		Role:    "user",
		Content: fmt.Sprintf("[Conversation Summary]\n%s", resp.Content),
	})
	compressed = append(compressed, messages[cutoff:]...)

	return compressed, nil
}

// writeHistoryLog writes full message history to a JSONL log.
func writeHistoryLog(messages []provider.Message, workspace string) (string, error) {
	logDir := filepath.Join(workspace, "memory", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", fmt.Errorf("create log dir: %w", err)
	}

	timestamp := time.Now().Format("20060102_150405")
	logFile := filepath.Join(logDir, fmt.Sprintf("history_%s.jsonl", timestamp))

	f, err := os.Create(logFile)
	if err != nil {
		return "", fmt.Errorf("create log file: %w", err)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for _, m := range messages {
		if err := enc.Encode(m); err != nil {
			return logFile, fmt.Errorf("encode message: %w", err)
		}
	}

	slog.Info("wrote history log", "file", logFile, "messages", len(messages))
	return logFile, nil
}
