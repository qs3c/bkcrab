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
	// DefaultTokenThreshold是压缩触发的默认阈值（ 80K令牌）。
	DefaultTokenThreshold = 80000
	// PruneTurnAge是保持不变的最近匝数；较旧的邮件将被修剪。
	PruneTurnAge = 20
	// truncatedPlaceholder替换已修剪的工具结果。
	truncatedPlaceholder = "[Result truncated - see memory logs]"
)

// EstimateTokens提供粗略的令牌估计： chars/4。
func EstimateTokens(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Arguments) / 4
			total += len(tc.Function.Name) / 4
		}
	}
	return total
}

// CompactResult保存压缩操作的结果。
type CompactResult struct {
	Messages []provider.Message
	Pruned   bool
	LogFile  string
}

// CompactMessages在消息历史记录超过令牌阈值时修剪并可选地压缩该历史记录。
// 第1步（修剪） ：对于早于PruneTurnAge的消息，剥离工具结果内容。
// 第2步（压缩） ：如果修剪后仍超过阈值，请总结较旧的消息
// 使用LLM并将完整历史记录写入日志文件。
func CompactMessages(messages []provider.Message, workspace string, prov provider.Provider, model string) (*CompactResult, error) {
	tokens := EstimateTokens(messages)
	if tokens < DefaultTokenThreshold {
		return &CompactResult{Messages: messages}, nil
	}

	slog.Info("context compaction triggered", "tokens", tokens, "threshold", DefaultTokenThreshold, "message_count", len(messages))

	// 在进行任何修改之前，将完整历史记录写入日志文件
	logFile, err := writeHistoryLog(messages, workspace)
	if err != nil {
		slog.Warn("failed to write history log", "error", err)
	}

	// 第1步：修剪-从旧消息中删除工具结果
	pruned := pruneOldToolResults(messages)
	prunedTokens := EstimateTokens(pruned)

	slog.Info("after pruning", "tokens_before", tokens, "tokens_after", prunedTokens)

	if prunedTokens < DefaultTokenThreshold {
		return &CompactResult{
			Messages: pruned,
			Pruned:   true,
			LogFile:  logFile,
		}, nil
	}

	// 第2步：压缩-总结较早的消息
	compressed, err := compressOlderMessages(pruned, prov, model)
	if err != nil {
		slog.Warn("compression failed, using pruned messages", "error", err)
		return &CompactResult{
			Messages: pruned,
			Pruned:   true,
			LogFile:  logFile,
		}, nil
	}

	slog.Info("after compression", "tokens_before", prunedTokens, "tokens_after", EstimateTokens(compressed))

	return &CompactResult{
		Messages: compressed,
		Pruned:   true,
		LogFile:  logFile,
	}, nil
}

// safeCompactionCutoff将截止向前推进超过任何领先的工具
// 消息，因此最近的尾部永远不会以“工具”角色开始。如果我们
// 已将表单[summary_user, tool,...]的列表发送到
// 提供程序， OpenAI兼容的API将拒绝：
//
// "角色为'tool'的消息必须是对前一个
// 带有“TOOL_CALLS”的消息"
//
// —之前“工具”正在应答的assistant.tool_calls得到了
// 吞咽到摘要本身的截止点。人择
// 不会有400个，但合同是一样的：一个工具回复
// 没有其父调用是语义垃圾。
//
// 我们只需要跳过领先的工具消息。如果尾巴开始
// 使用助手（ tool_calls ） ，没关系—它的工具会回复（如果有的话）
// 在尾巴后面跟着它。
//
// 纯函数；无分配；安全，具有任何值截止。
func safeCompactionCutoff(messages []provider.Message, cutoff int) int {
	if cutoff < 0 {
		cutoff = 0
	}
	for cutoff < len(messages) && messages[cutoff].Role == "tool" {
		cutoff++
	}
	return cutoff
}

// pruneOldToolResults从早于PruneTurnAge的消息中剥离工具结果内容。
func pruneOldToolResults(messages []provider.Message) []provider.Message {
	if len(messages) <= PruneTurnAge {
		return messages
	}

	cutoff := len(messages) - PruneTurnAge
	result := make([]provider.Message, len(messages))
	copy(result, messages)

	for i := 0; i < cutoff; i++ {
		if result[i].Role == "tool" && len(result[i].Content) > 200 {
			result[i] = provider.Message{
				Role:       "tool",
				Content:    truncatedPlaceholder,
				ToolCallID: result[i].ToolCallID,
				Name:       result[i].Name,
			}
		}
	}

	return result
}

// compressOlderMessages要求LLM将旧消息汇总为压缩摘要。
func compressOlderMessages(messages []provider.Message, prov provider.Provider, model string) ([]provider.Message, error) {
	if len(messages) <= PruneTurnAge {
		return messages, nil
	}

	cutoff := safeCompactionCutoff(messages, len(messages)-PruneTurnAge)
	olderMessages := messages[:cutoff]

	// 构建用于总结的旧消息的文本表示形式。
	// 跳过运行时注入的消息（当前仅GOAL_CONTEXT
	// 延续） ：其内容是合成审计脚手架，
	// 不值得总结的对话—最新的一个是
	// 已经逐字保留在下面最近的尾巴中，所以
	// 模型永远不会丢失当前的审计上下文。这是
	// 固定头保护设计§ 5.3 (b)要求：旧
	// goal_context消息完全从
	// 压缩输出;实时一个骑通过不变。
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

	// 生成新消息列表：摘要+最近的消息
	compressed := make([]provider.Message, 0, PruneTurnAge+1)
	compressed = append(compressed, provider.Message{
		Role:    "user",
		Content: fmt.Sprintf("[Conversation Summary]\n%s", resp.Content),
	})
	compressed = append(compressed, messages[cutoff:]...)

	return compressed, nil
}

// writeHistoryLog将完整的消息历史记录写入JSONL日志文件。
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
