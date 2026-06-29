package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/provider"
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
	DefaultTailTargetPercent        = 30
	MinimumTailTurns                = 2
	DefaultSummaryMaxRetries        = 3
	toolResultPruneThresholdBytes   = 2000
	fallbackSummaryMaxRunes         = 12000
	fallbackSnippetMaxRunes         = 220
)

const (
	// PruneTurnAge is kept for compatibility with older tests/callers. The
	// active pruning boundary now comes from the shared dynamic tail policy.
	PruneTurnAge = 20
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
	TailTargetPercent int
	// Deprecated: tail selection is token-budget based; use TailTargetPercent.
	TailTargetMessages int
	MinTailTurns       int
	Focus              string
	OverheadMessages   []provider.Message
	ToolDefs           []provider.Tool
	SummaryMaxRetries  int
	ArchiveStore       contextArchiveStore
	ArchiveUserID      string
	ArchiveAgentID     string
	ArchiveSessionKey  string
	// Ctx is the context passed to Provider.Chat for the summarizer LLM call.
	// Callers thread the turn's context here so the summary inherits its
	// request-scoped values (chatter user id, trace) and deadline. It must be
	// non-nil for OpenAI-compatible providers: http.NewRequestWithContext
	// rejects a nil context, so a nil ctx makes every summary attempt fail and
	// silently degrades compaction to deterministicSummaryFallback.
	// summarizeWithRetries falls back to context.Background() when this is unset
	// (deterministic-output tests, emergency mode which never summarizes).
	Ctx context.Context
	// OnTriggered, when set, is called once at the start of an actual
	// compaction path: proactive mode over the trigger threshold, manual
	// compaction, or emergency compaction after a context-limit error. It is
	// never called on the no-op sanitize path taken by ordinary under-threshold
	// turns. Callers use it to surface a "compacting context…" indicator for
	// the synchronous wait that follows. It runs synchronously on the compaction
	// goroutine, so keep it cheap and non-blocking.
	OnTriggered func()
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
			sanitized, changed := sanitizeToolPairsWithChange(messages)
			return &CompactResult{Messages: sanitized, Pruned: changed}, nil
		}
		return compactMessagesTriggered(messages, opts, tokens)
	case CompactModeManual:
		return compactMessagesTriggered(messages, opts, tokens)
	case CompactModeEmergency:
		if opts.OnTriggered != nil {
			opts.OnTriggered()
		}
		return emergencyCompactMessages(messages, opts, tokens), nil
	default:
		if tokens < compactTriggerLimit(opts) {
			sanitized, changed := sanitizeToolPairsWithChange(messages)
			return &CompactResult{Messages: sanitized, Pruned: changed}, nil
		}
		return compactMessagesTriggered(messages, opts, tokens)
	}
}

func isContextLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if isRateLimitLikeError(msg) {
		return false
	}
	for _, marker := range []string{
		"context_length_exceeded",
		"maximum context length",
		"prompt too long",
		"prompt is too long",
		"too many tokens",
		"too many tokens in request",
		"input length exceeds context window",
		"request too large",
		"eof",
		"server closed idle connection",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func isRateLimitLikeError(msg string) bool {
	for _, marker := range []string{
		"rate limit",
		"rate_limit",
		"tokens per minute",
		"requests per minute",
		"quota",
		"throttle",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
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
	if opts.TailTargetPercent <= 0 {
		opts.TailTargetPercent = DefaultTailTargetPercent
	}
	if opts.TailTargetPercent > 100 {
		opts.TailTargetPercent = 100
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

func compactTailTargetLimit(opts CompactOptions) int {
	return percentOf(compactInputBudget(opts), opts.TailTargetPercent)
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

func emergencyCompactMessages(messages []provider.Message, opts CompactOptions, beforeTokens int) *CompactResult {
	slog.Info(
		"emergency context compaction triggered",
		"tokens", beforeTokens,
		"message_count", len(messages),
	)

	logFile, err := writeHistoryLog(messages, opts.Workspace)
	if err != nil {
		slog.Warn("failed to write emergency history log", "error", err)
	}

	sanitized, sanitizedChanged := sanitizeToolPairsWithChange(messages)
	if len(sanitized) == 0 {
		return &CompactResult{Messages: messages, Pruned: false, LogFile: logFile}
	}
	if len(sanitized) == 1 {
		return &CompactResult{Messages: sanitized, Pruned: sanitizedChanged, LogFile: logFile}
	}

	pruned, prunedChanged := pruneOldToolResultsWithChange(sanitized, opts)
	changed := sanitizedChanged || prunedChanged
	cutoff := emergencyCompactionTailStart(pruned, opts)
	if cutoff <= 0 {
		return &CompactResult{Messages: pruned, Pruned: changed, LogFile: logFile}
	}

	compressed, err := compressOlderMessages(pruned, opts)
	if err != nil {
		slog.Warn("emergency compression failed, using pruned messages", "error", err)
		return &CompactResult{Messages: pruned, Pruned: changed, LogFile: logFile}
	}
	compressed, _ = sanitizeToolPairsWithChange(compressed)

	slog.Info(
		"after emergency compression",
		"tokens_after", EstimateRequestTokens(compactionRequestMessages(compressed, opts.OverheadMessages), opts.ToolDefs),
		"tail_target_percent", opts.TailTargetPercent,
	)
	return &CompactResult{
		Messages: compressed,
		Pruned:   true,
		LogFile:  logFile,
	}
}

func emergencyCompactionTailStart(messages []provider.Message, opts CompactOptions) int {
	cutoff := compactionTailStart(messages, opts)
	if cutoff > 0 {
		return cutoff
	}
	return len(messages)
}

func compactMessagesTriggered(messages []provider.Message, opts CompactOptions, tokens int) (*CompactResult, error) {
	slog.Info(
		"context compaction triggered",
		"tokens", tokens,
		"threshold", compactTriggerLimit(opts),
		"message_count", len(messages),
		"mode", opts.Mode,
	)

	if opts.OnTriggered != nil {
		opts.OnTriggered()
	}

	logFile, err := writeHistoryLog(messages, opts.Workspace)
	if err != nil {
		slog.Warn("failed to write history log", "error", err)
	}

	sanitized, sanitizedChanged := sanitizeToolPairsWithChange(messages)
	pruned, prunedChanged := pruneOldToolResultsWithChange(sanitized, opts)
	changed := sanitizedChanged || prunedChanged
	prunedTokens := EstimateRequestTokens(compactionRequestMessages(pruned, opts.OverheadMessages), opts.ToolDefs)

	slog.Info("after pruning", "tokens_before", tokens, "tokens_after", prunedTokens)

	if opts.Mode != CompactModeManual && prunedTokens < compactTargetLimit(opts) {
		return &CompactResult{
			Messages: pruned,
			Pruned:   changed,
			LogFile:  logFile,
		}, nil
	}

	cutoff := compactionTailStart(pruned, opts)
	if cutoff <= 0 {
		return &CompactResult{
			Messages: pruned,
			Pruned:   changed,
			LogFile:  logFile,
		}, nil
	}

	compressed, err := compressOlderMessages(pruned, opts)
	if err != nil {
		slog.Warn("compression failed, using pruned messages", "error", err)
		return &CompactResult{
			Messages: pruned,
			Pruned:   changed,
			LogFile:  logFile,
		}, nil
	}
	compressed, _ = sanitizeToolPairsWithChange(compressed)

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

func compactionTailStart(messages []provider.Message, opts CompactOptions) int {
	opts = normalizeCompactOptions(opts)
	minTailTurns := opts.MinTailTurns
	if len(messages) == 0 {
		return 0
	}

	includeZeroTail := opts.Mode == CompactModeEmergency
	if includeZeroTail {
		minTailTurns = 0
	}

	complete := completeTurnTailCandidate(messages, compactTailTargetLimit(opts), minTailTurns, includeZeroTail)
	if complete.ok {
		return complete.cutoff
	}
	return 0
}

type tailCandidate struct {
	cutoff   int
	distance int
	ok       bool
}

func completeTurnTailCandidate(messages []provider.Message, targetTokens, minKeepTurns int, includeZeroTail bool) tailCandidate {
	if minKeepTurns < 1 {
		if includeZeroTail {
			minKeepTurns = 0
		} else {
			minKeepTurns = 1
		}
	}
	// Anchor turn boundaries on every user-role message, including
	// runtime-injected ones (e.g. /goal continuation context, tagged
	// OriginGoalContext). An autonomous /goal run can have few or zero
	// real user messages; filtering injected turns out would collapse the
	// whole run into one giant turn, leaving no interior cutoff and
	// forcing all-or-nothing (zero-tail or no compaction). Counting
	// injected user turns preserves cutoff granularity. This is
	// independent of summary-content filtering — compressOlderMessages
	// still drops OriginGoalContext from the summary text itself.
	userStarts := make([]int, 0)
	for i, msg := range messages {
		if msg.Role == "user" {
			userStarts = append(userStarts, i)
		}
	}
	bestCutoff := -1
	bestDistance := 0
	bestTailTokens := 0
	consider := func(cutoff int) {
		if cutoff < 0 || cutoff > len(messages) {
			return
		}
		tailTokens := EstimateTokens(messages[cutoff:])
		distance := absInt(tailTokens - targetTokens)
		prefer := tailTokens > bestTailTokens
		if includeZeroTail {
			prefer = tailTokens < bestTailTokens
		}
		if bestCutoff < 0 || distance < bestDistance || (distance == bestDistance && prefer) {
			bestCutoff = cutoff
			bestDistance = distance
			bestTailTokens = tailTokens
		}
	}

	if includeZeroTail {
		consider(len(messages))
	}

	for i := len(userStarts) - 1; i >= 0; i-- {
		keepTurns := len(userStarts) - i
		if keepTurns < minKeepTurns {
			continue
		}
		cutoff := safeCompactionCutoff(messages, userStarts[i])
		if cutoff <= 0 {
			continue
		}
		consider(cutoff)
	}
	if bestCutoff < 0 {
		return tailCandidate{}
	}
	return tailCandidate{
		cutoff:   bestCutoff,
		distance: bestDistance,
		ok:       true,
	}
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// pruneOldToolResults strips large tool result bodies from older messages.
func pruneOldToolResults(messages []provider.Message) []provider.Message {
	result, _ := pruneOldToolResultsWithChange(messages)
	return result
}

func pruneOldToolResultsWithChange(messages []provider.Message, optList ...CompactOptions) ([]provider.Message, bool) {
	var opts CompactOptions
	if len(optList) > 0 {
		opts = optList[0]
	}
	cutoff := compactionTailStart(messages, opts)
	if cutoff <= 0 {
		return messages, false
	}

	infoByIndex := buildToolResultInfoByIndex(messages)
	result := make([]provider.Message, len(messages))
	copy(result, messages)

	changed := false
	for i := 0; i < cutoff; i++ {
		if result[i].Role == "tool" && len(result[i].Content) > toolResultPruneThresholdBytes {
			info := infoByIndex[i]
			archiveID, err := archiveToolResult(opts, result[i], info)
			if err != nil {
				slog.Warn("failed to archive compacted tool result", "error", err)
			}
			result[i] = summarizeToolResultWithInfo(result[i], info, archiveID)
			changed = true
		}
	}

	return result, changed
}

// compressOlderMessages asks the LLM to summarize older messages.
func compressOlderMessages(messages []provider.Message, opts CompactOptions) ([]provider.Message, error) {
	opts = normalizeCompactOptions(opts)
	cutoff := compactionTailStart(messages, opts)
	if opts.Mode == CompactModeEmergency {
		cutoff = emergencyCompactionTailStart(messages, opts)
	}
	if cutoff <= 0 {
		return messages, nil
	}
	olderMessages := messages[:cutoff]
	summaryLabel := "[Conversation Summary]"
	systemPrompt := "You are a conversation summarizer. Summarize the following conversation history into a compact summary that preserves key facts, decisions, and context. Be concise but don't lose important details."
	requestTitle := "Summarize this conversation"
	if opts.Mode == CompactModeEmergency {
		summaryLabel = "[Reactive Context Summary]"
		systemPrompt = "You are an emergency conversation summarizer. Summarize the older conversation history after a context-limit error. Preserve active tasks, user preferences, decisions, constraints, and unresolved work. Be concise."
		requestTitle = "Summarize this conversation after a context-limit error"
	}

	var text string
	for _, m := range olderMessages {
		if m.Origin != provider.OriginUser {
			continue
		}
		text += fmt.Sprintf("[%s] %s\n", m.Role, m.Content)
	}

	var userPrompt strings.Builder
	if opts.Mode == CompactModeManual {
		if focus := strings.TrimSpace(opts.Focus); focus != "" {
			userPrompt.WriteString("Manual compaction focus:\n")
			userPrompt.WriteString(focus)
			userPrompt.WriteString("\n\n")
		}
	}
	userPrompt.WriteString(requestTitle)
	userPrompt.WriteString(":\n\n")
	userPrompt.WriteString(text)

	summaryPrompt := []provider.Message{
		{
			Role:    "system",
			Content: systemPrompt,
		},
		{
			Role:    "user",
			Content: userPrompt.String(),
		},
	}

	summary, err := summarizeWithRetries(opts, summaryPrompt)
	if err != nil {
		slog.Warn("summary failed after retries, using deterministic fallback", "error", err)
		summary = deterministicSummaryFallback(olderMessages)
	}

	compressed := make([]provider.Message, 0, len(messages)-cutoff+1)
	compressed = append(compressed, provider.Message{
		Role:    "user",
		Content: fmt.Sprintf("%s\n%s", summaryLabel, summary),
	})
	compressed = append(compressed, messages[cutoff:]...)

	return compressed, nil
}

func summarizeWithRetries(opts CompactOptions, prompt []provider.Message) (string, error) {
	opts = normalizeCompactOptions(opts)
	if opts.Provider == nil {
		return "", fmt.Errorf("summarize conversation: provider is nil")
	}

	ctx := opts.Ctx
	if ctx == nil {
		ctx = context.Background()
	}

	var lastErr error
	for attempt := 0; attempt < opts.SummaryMaxRetries; attempt++ {
		resp, err := opts.Provider.Chat(ctx, prompt, nil, opts.Model, 2048, 0.3)
		if err != nil {
			lastErr = err
			continue
		}
		if resp == nil {
			lastErr = fmt.Errorf("summary response is nil")
			continue
		}
		if strings.TrimSpace(resp.Content) == "" {
			lastErr = fmt.Errorf("summary response content is empty")
			continue
		}
		return resp.Content, nil
	}

	return "", fmt.Errorf("summarize conversation failed after %d attempts: %w", opts.SummaryMaxRetries, lastErr)
}

func deterministicSummaryFallback(messages []provider.Message) string {
	const marker = "deterministic fallback: LLM summary failed after retries. Older messages were compacted without an LLM."

	lines := []string{marker}
	totalRunes := runeCount(marker)
	for _, m := range messages {
		if m.Origin != provider.OriginUser {
			continue
		}
		text := strings.TrimSpace(m.TextContent())
		if text == "" {
			continue
		}
		line := fmt.Sprintf("[%s] %s", m.Role, snippetForFallback(text))
		nextRunes := totalRunes + 1 + runeCount(line)
		if nextRunes > fallbackSummaryMaxRunes {
			lines = append(lines, "[fallback summary truncated]")
			break
		}
		lines = append(lines, line)
		totalRunes = nextRunes
	}

	return strings.Join(lines, "\n")
}

func snippetForFallback(text string) string {
	normalized := strings.Join(strings.Fields(text), " ")
	runes := []rune(normalized)
	if len(runes) <= fallbackSnippetMaxRunes {
		return normalized
	}
	return string(runes[:fallbackSnippetMaxRunes]) + "..."
}

func runeCount(s string) int {
	return len([]rune(s))
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
