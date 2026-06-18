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

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/privacy"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

// MemoryStore 是一个可选的接口，用于基于数据库的内存持久化。
// userID 是聊天者——聊天的 MEMORY.md / USER.md 更新落在该用户的每个用户
// 覆盖行中，这样它们不会污染代理所有者通过自定义页面编辑的共享模板。
//
// GetWorkspaceFile 与 GetWorkspaceFileExact：
//   - GetWorkspaceFile 首先选择调用者的行，当调用者没有时回退到代理
//     所有者的行。用于共享身份文件（SOUL/IDENTITY/AGENTS/...）：聊天者
//     继承所有者配置的任何内容。
//   - GetWorkspaceFileExact 仅返回调用者的行，或 ErrNotFound。用于每个
//     聊天者的文件（USER.md, MEMORY.md）：全新访客必须看到空白的个人资料/
//     记忆，绝不泄漏所有者的。
type MemoryStore interface {
	GetMemory(ctx context.Context, agentID, userID string) (string, error)
	SaveMemory(ctx context.Context, agentID, userID, content string) error
	GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error)
	SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error
}

type Memory struct {
	workspace string
	store     MemoryStore
	userID    string
	agentID   string
}

func NewMemory(workspace string) *Memory {
	return &Memory{workspace: workspace}
}

// NewMemoryWithStoreForUser 是用户作用域的构造函数。userID 必须是从
// 认证解析出的真实 users.id。
func NewMemoryWithStoreForUser(workspace string, st MemoryStore, userID, agentID string) *Memory {
	if userID == "" {
		panic("agent.NewMemoryWithStoreForUser: userID is required")
	}
	return &Memory{workspace: workspace, store: st, userID: userID, agentID: agentID}
}

// UserID 返回此 Memory 绑定到的 userID（通过 NewMemoryWithStoreForUser /
// WithUserID 设置）。由代理循环的 autoPersist 门控使用，用于查询每个
// 聊天者的用户消息计数，而无需通过入站消息重新解析 chatterUID。
func (m *Memory) UserID() string { return m.userID }

// WithUserID 返回绑定到不同 userID 的浅拷贝。
// 允许每个轮次的调用者将 MEMORY.md / USER.md 的读写重新绑定到聊天者
// （而不是代理所有者），而不会变异其他并发轮次可能正在读取的共享代理
// 作用域 Memory。当 m 为 nil 时返回 nil，使调用者无需进行 nil 保护。
func (m *Memory) WithUserID(uid string) *Memory {
	if m == nil {
		return nil
	}
	out := *m
	out.userID = uid
	return &out
}

// ctx 返回一个标记了此 Memory 的用户标签的上下文，使存储层中的 SQL 查询
// 正确作用域。当上下文中没有用户时，存储回退到 DefaultUserID，但通过此处
// 是显式的，并防止调用者意外地写入 "" 下。
func (m *Memory) ctx() context.Context {
	if m.userID == "" {
		return context.Background()
	}
	return config.WithUserID(context.Background(), m.userID)
}

// memoryPath 返回 MEMORY.md 的路径。
func (m *Memory) memoryPath() string {
	return filepath.Join(m.workspace, "MEMORY.md")
}

// historyPath 返回 HISTORY.md 的路径。
func (m *Memory) historyPath() string {
	return filepath.Join(m.workspace, "HISTORY.md")
}

// LoadMemory 读取此 Memory 用户的长期记忆。当配置了存储时，我们绝不
// 回退到磁盘上的工作空间 MEMORY.md——该文件是代理所有者的副本，会泄漏
// 给任何其行尚不存在的非所有者聊天者。FS 读取仅在无存储的旧版单用户
// 安装上触发。
func (m *Memory) LoadMemory() string {
	if m.store != nil {
		content, err := m.store.GetMemory(m.ctx(), m.agentID, m.userID)
		if err == nil {
			return content
		}
		return ""
	}
	data, err := os.ReadFile(m.memoryPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// SaveMemory 覆盖长期记忆。
func (m *Memory) SaveMemory(content string) error {
	if m.store != nil {
		return m.store.SaveMemory(m.ctx(), m.agentID, m.userID, content)
	}
	os.MkdirAll(m.workspace, 0o755)
	return os.WriteFile(m.memoryPath(), []byte(content), 0o644)
}

// AppendHistory 向历史日志添加一条条目。
func (m *Memory) AppendHistory(entry string) error {
	os.MkdirAll(m.workspace, 0o755)
	f, err := os.OpenFile(m.historyPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	timestamp := time.Now().Format("2006-01-02 15:04:05")
	_, err = fmt.Fprintf(f, "- [%s] %s\n", timestamp, entry)
	return err
}

// LoadHistory 读取历史日志。
func (m *Memory) LoadHistory() string {
	data, err := os.ReadFile(m.historyPath())
	if err != nil {
		return ""
	}
	return string(data)
}

// ReviewAndUpdateMemory 扫描最近的历史条目并将新的关键事实附加到 MEMORY.md。
// 由心跳调用以保持长期记忆的新鲜度。
func (m *Memory) ReviewAndUpdateMemory(workspace string) {
	history := m.LoadHistory()
	if history == "" {
		return
	}

	// 获取要审查的最后 N 行历史
	lines := strings.Split(strings.TrimSpace(history), "\n")
	reviewCount := 50
	if len(lines) < reviewCount {
		reviewCount = len(lines)
	}
	recentLines := lines[len(lines)-reviewCount:]

	// 从最近的历史中提取关键事实（简单的基于关键字的提取）
	currentMemory := m.LoadMemory()
	var newFacts []string

	for _, line := range recentLines {
		lower := strings.ToLower(line)
		// 查找包含重要关键字的行
		if containsAny(lower, []string{
			"learned", "discovered", "user prefers", "important",
			"remember", "note:", "key fact", "decision",
			"preference", "configured", "set up",
		}) {
			// 提取时间戳之后的内容
			if idx := strings.Index(line, "] "); idx >= 0 {
				fact := strings.TrimSpace(line[idx+2:])
				if fact != "" && !strings.Contains(currentMemory, fact) {
					newFacts = append(newFacts, fact)
				}
			}
		}
	}

	if len(newFacts) == 0 {
		slog.Debug("memory review: no new facts to add")
		return
	}

	// 将新事实追加到 MEMORY.md
	var sb strings.Builder
	sb.WriteString(currentMemory)
	if currentMemory != "" && !strings.HasSuffix(currentMemory, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("\n## Auto-updated: %s\n", time.Now().Format("2006-01-02 15:04")))
	for _, fact := range newFacts {
		sb.WriteString(fmt.Sprintf("- %s\n", fact))
	}

	if err := m.SaveMemory(sb.String()); err != nil {
		slog.Warn("failed to update memory", "error", err)
		return
	}

	slog.Info("memory updated", "new_facts", len(newFacts))
}

func containsAny(s string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(s, kw) {
			return true
		}
	}
	return false
}

// SaveMemoryWithScan 在写入 MEMORY.md 之前扫描内容中的威胁。
// 对任何检测到的威胁记录警告，但仍然写入（以避免数据丢失）。
func (m *Memory) SaveMemoryWithScan(content string) error {
	if threats := privacy.Scan(content); len(threats) > 0 {
		for _, t := range threats {
			slog.Warn("memory safety threat detected in MEMORY.md write",
				"type", t.Type,
				"pattern", t.Pattern,
				"context", t.Context,
			)
		}
	}
	return m.SaveMemory(content)
}

// SaveUserFile 使用威胁扫描写入 USER.md。
func (m *Memory) SaveUserFile(content string) error {
	if threats := privacy.Scan(content); len(threats) > 0 {
		for _, t := range threats {
			slog.Warn("memory safety threat detected in USER.md write",
				"type", t.Type,
				"pattern", t.Pattern,
				"context", t.Context,
			)
		}
	}
	if m.store != nil {
		return m.store.SaveWorkspaceFile(m.ctx(), m.agentID, m.userID, "USER.md", []byte(content))
	}
	os.MkdirAll(m.workspace, 0o755)
	return os.WriteFile(filepath.Join(m.workspace, "USER.md"), []byte(content), 0o644)
}

// LoadUserFile 读取此 Memory 用户的 USER.md 文件。与 LoadMemory 相同
// 的原理：USER.md 是每个聊天者的（访客的个人资料，不是代理所有者的），
// 所以我们通过绕过 SQL 所有者回退覆盖层的 Exact 路径读取它，并在配置了
// 存储时跳过磁盘回退，以避免将所有者的工作空间副本泄漏给没有自己行的
// 聊天者。
func (m *Memory) LoadUserFile() string {
	if m.store != nil {
		data, err := m.store.GetWorkspaceFileExact(m.ctx(), m.agentID, m.userID, "USER.md")
		if err == nil {
			return string(data)
		}
		return ""
	}
	data, err := os.ReadFile(filepath.Join(m.workspace, "USER.md"))
	if err != nil {
		return ""
	}
	return string(data)
}

// AutoPersistMemory 使用 LLM 从最近的消息中提取事实并追加到 MEMORY.md
// 和 USER.md。每 N 轮调用一次。
func AutoPersistMemory(ctx context.Context, mem *Memory, prov provider.Provider, model string, groups []store.TurnGroup) error {
	// 为 LLM 构建提取输入:按 session 分节(### Session),每条消息截断到 300 字符,
	// 并对整批设总量上限,避免积压追赶时一次塞进过多 turn 把 prompt 撑爆
	//(只约束输入拼装,与 max_tokens 控制的输出无关)。
	const maxPromptChars = 12000
	var sb strings.Builder
	truncated := false
buildPrompt:
	for _, g := range groups {
		sb.WriteString(fmt.Sprintf("### Session %s\n", g.SessionKey))
		for _, m := range g.Messages {
			if m.Role == "system" || m.Origin != "" {
				continue // 跳过 system 与 goal_context 等合成注入行
			}
			content := m.Content
			if len(content) > 300 {
				content = content[:300] + "..."
			}
			line := fmt.Sprintf("[%s]: %s\n", m.Role, content)
			if sb.Len()+len(line) > maxPromptChars {
				truncated = true
				break buildPrompt
			}
			sb.WriteString(line)
		}
	}
	if truncated {
		sb.WriteString("…(超出上限,后续内容已省略)\n")
	}

	currentMemory := mem.LoadMemory()
	currentUser := mem.LoadUserFile()

	extractPrompt := fmt.Sprintf(`Analyze this conversation and extract:
1. Key facts, decisions, or learnings worth remembering (for MEMORY.md)
2. User preferences, profile details, or work style notes (for USER.md)

Current MEMORY.md:
%s

Current USER.md:
%s

Recent conversation:
%s

Output JSON only (no markdown fences):
{"memory_facts": ["fact1", "fact2"], "user_notes": ["note1"]}
If nothing worth saving, output: {"memory_facts": [], "user_notes": []}`,
		truncateStr(currentMemory, 500),
		truncateStr(currentUser, 500),
		sb.String(),
	)

	resp, err := prov.Chat(ctx, []provider.Message{
		{Role: "user", Content: extractPrompt},
	}, nil, model, 200, 0.3)
	if err != nil {
		// Warn（不是 Debug）——这里的隐形失败正是那种事后调试起来
		// 很痛苦的"我打开了开关但什么也没持久化"的体验。
		slog.Warn("auto-persist: LLM call failed", "error", err, "model", model)
		return err
	}

	var result struct {
		MemoryFacts []string `json:"memory_facts"`
		UserNotes   []string `json:"user_notes"`
	}
	// 在解析之前去除 markdown 代码围栏——许多调优过的模型
	// （Sonnet 4.x, Opus, …）会反射性地将结构化输出包装在
	// ```json … ``` 中，即使提示词要求"没有 markdown 围栏"。
	cleaned := stripJSONFence(resp.Content)
	if err := json.Unmarshal([]byte(cleaned), &result); err != nil {
		// 与上面相同的 Warn 升级——这里静默跳过隐藏了实际环境中
		// "Sonnet 返回了包装的 JSON，解析失败"的问题。
		preview := cleaned
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		slog.Warn("auto-persist: failed to parse LLM response",
			"error", err, "model", model, "preview", preview)
		return fmt.Errorf("auto-persist parse: %w", err)
	}
	slog.Info("auto-persist: extracted",
		"model", model,
		"memory_facts", len(result.MemoryFacts),
		"user_notes", len(result.UserNotes))

	// 追加新的记忆事实
	if len(result.MemoryFacts) > 0 {
		var memSB strings.Builder
		memSB.WriteString(currentMemory)
		if currentMemory != "" && !strings.HasSuffix(currentMemory, "\n") {
			memSB.WriteString("\n")
		}
		memSB.WriteString(fmt.Sprintf("\n## Auto-persisted: %s\n", time.Now().Format("2006-01-02 15:04")))
		for _, fact := range result.MemoryFacts {
			memSB.WriteString(fmt.Sprintf("- %s\n", fact))
		}
		if err := mem.SaveMemoryWithScan(memSB.String()); err != nil {
			slog.Warn("auto-persist: failed to save MEMORY.md", "error", err)
		} else {
			slog.Info("auto-persist: updated MEMORY.md", "facts", len(result.MemoryFacts))
		}
	}

	// 追加用户笔记
	if len(result.UserNotes) > 0 {
		var userSB strings.Builder
		userSB.WriteString(currentUser)
		if currentUser != "" && !strings.HasSuffix(currentUser, "\n") {
			userSB.WriteString("\n")
		}
		userSB.WriteString(fmt.Sprintf("\n## Auto-persisted: %s\n", time.Now().Format("2006-01-02 15:04")))
		for _, note := range result.UserNotes {
			userSB.WriteString(fmt.Sprintf("- %s\n", note))
		}
		if err := mem.SaveUserFile(userSB.String()); err != nil {
			slog.Warn("auto-persist: failed to save USER.md", "error", err)
		} else {
			slog.Info("auto-persist: updated USER.md", "notes", len(result.UserNotes))
		}
	}
	return nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// stripJSONFence 从 LLM 响应中去除前导的 ```json（或 ```）和尾部的
// ``` 包装。调优过的聊天模型即使提示词要求原始 JSON，也会例行公事地
// 包装结构化输出。当没有围栏时返回原始（修剪后的）字符串。
func stripJSONFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	// 丢弃开头的围栏（```json\n 或 ```\n）——前导反引号后直到第一个
	// 换行符之间的任何内容。
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	} else {
		s = strings.TrimPrefix(s, "```")
	}
	s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	return strings.TrimSpace(s)
}
