package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/memory"
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

// AutoPersistMemory uses the LLM to extract add/replace/remove operations for
// MEMORY.md and USER.md, then applies them through the managed memory engine.
func AutoPersistMemory(ctx context.Context, mgr *memory.Manager, prov provider.Provider, model string, groups []store.TurnGroup) error {
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

	userList := mgr.List(ctx, memory.TargetUser)
	memList := mgr.List(ctx, memory.TargetMemory)

	extractPrompt := fmt.Sprintf(`You maintain two long-term memory files for ONE chatter. Review the recent conversation and decide what to persist.

Rules:
- USER.md = who the chatter is (name, role, stable preferences/profile).
- MEMORY.md = facts/decisions/context worth holding across sessions.
- Per file, output operations:
  - add: a new entry (one fact). Skip if already present below.
  - replace: correct/refresh an existing entry. Set old_text to the EXACT verbatim text of the target entry shown below (the body under "--- entry N ---", NOT the marker line). content = the new text.
  - remove: delete an entry the chatter asked to forget or that is now wrong. Set old_text to the exact verbatim entry text.
- Stay within the character budget shown. If near the limit, prefer replace/remove to compress rather than only adding.
- If nothing should change for a file, return an empty array for it.

Current USER.md (usage %s):
%s

Current MEMORY.md (usage %s):
%s

Recent conversation:
%s

%sOutput JSON only (no markdown fences):
{"memory_ops":[{"action":"add|replace|remove","old_text":"...","content":"..."}],"user_ops":[{"action":"add|replace|remove","old_text":"...","content":"..."}]}`,
		userList.Usage,
		formatEntriesForExtract(userList),
		memList.Usage,
		formatEntriesForExtract(memList),
		sb.String(),
		compactionPressureNote(memList, userList),
	)

	resp, err := prov.Chat(ctx, []provider.Message{
		{Role: "user", Content: extractPrompt},
	}, nil, model, 2048, 0.3)
	if err != nil {
		// Warn（不是 Debug）——这里的隐形失败正是那种事后调试起来
		// 很痛苦的"我打开了开关但什么也没持久化"的体验。
		slog.Warn("auto-persist: LLM call failed", "error", err, "model", model)
		return err
	}

	type extractedOp struct {
		Action  string `json:"action"`
		OldText string `json:"old_text,omitempty"`
		Content string `json:"content,omitempty"`
	}
	var parsed struct {
		MemoryOps []extractedOp `json:"memory_ops"`
		UserOps   []extractedOp `json:"user_ops"`
	}
	// 在解析之前去除 markdown 代码围栏——许多调优过的模型
	// （Sonnet 4.x, Opus, …）会反射性地将结构化输出包装在
	// ```json … ``` 中，即使提示词要求"没有 markdown 围栏"。
	cleaned := stripJSONFence(resp.Content)
	if err := json.Unmarshal([]byte(cleaned), &parsed); err != nil {
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

	toOps := func(in []extractedOp) []memory.Operation {
		ops := make([]memory.Operation, 0, len(in))
		for _, o := range in {
			action := memory.Action(strings.TrimSpace(o.Action))
			if action == "" || action == memory.ActionList {
				continue
			}
			ops = append(ops, memory.Operation{
				Action:  action,
				Content: o.Content,
				OldText: o.OldText,
			})
		}
		return ops
	}

	memOps := toOps(parsed.MemoryOps)
	userOps := toOps(parsed.UserOps)
	slog.Info("auto-persist: extracted",
		"model", model,
		"memory_ops", len(memOps),
		"user_ops", len(userOps))

	// Apply atomically through the managed engine. Each target gets a compaction
	// fallback so a file already pinned at its character limit can still land the
	// model's deletions/corrections instead of freezing forever.
	applyCadenceOps(ctx, mgr, memory.TargetMemory, memOps, model)
	applyCadenceOps(ctx, mgr, memory.TargetUser, userOps, model)
	return nil
}

// applyCadenceOps applies a cadence batch through the managed engine. It tries the
// whole batch atomically first (the common case). If that is rejected — because the
// result would exceed the character limit, or because one op carried a stale or
// ambiguous old_text — it retries best-effort: each op on its own, compacting ops
// (remove / replace) before adds. A remove or a size-reducing replace applied alone
// always fits, so it lands and frees space; only the ops that genuinely don't fit
// (or no longer match) are skipped. This keeps a memory file pinned at its limit
// from freezing — every round still lands the model's deletions and corrections —
// and also salvages the good ops when the model returns one bad op in a batch.
func applyCadenceOps(ctx context.Context, mgr *memory.Manager, target memory.Target, ops []memory.Operation, model string) {
	if len(ops) == 0 {
		return
	}
	if res := mgr.Apply(ctx, target, ops); res.Success {
		slog.Info("auto-persist: applied", "model", model, "target", target, "ops", len(ops))
		return
	}
	applied := 0
	for _, op := range compactingFirst(ops) {
		if mgr.Apply(ctx, target, []memory.Operation{op}).Success {
			applied++
		}
	}
	slog.Info("auto-persist: applied best-effort after batch rejection",
		"model", model, "target", target, "applied", applied, "of", len(ops))
}

// compactingFirst orders remove/replace ops ahead of adds, so that when ops are
// applied one at a time the space-freeing ones run before the space-consuming ones.
func compactingFirst(ops []memory.Operation) []memory.Operation {
	out := make([]memory.Operation, 0, len(ops))
	for _, op := range ops {
		if op.Action != memory.ActionAdd {
			out = append(out, op)
		}
	}
	for _, op := range ops {
		if op.Action == memory.ActionAdd {
			out = append(out, op)
		}
	}
	return out
}

// compactionPressureThreshold is the fraction of a file's character limit at which
// the cadence extraction prompt starts actively pushing the model to prune.
const compactionPressureThreshold = 0.8

// compactionPressureNote is the primary defense against a memory file filling up:
// when a file is at/over 80% of its limit, it injects a prompt block steering the
// model to prune (remove/replace stale or unimportant entries) instead of only
// adding. Importance is the model's call; entries are shown oldest-first (incremental
// append), so earlier ones are the natural eviction targets. Returns "" — no extra
// pressure — while both files still have room. (applyCadenceOps is the mechanical
// backstop for when the model over-fills anyway.)
func compactionPressureNote(mem, user memory.Result) string {
	var tight []string
	if nearLimit(mem) {
		tight = append(tight, "MEMORY.md ("+mem.Usage+")")
	}
	if nearLimit(user) {
		tight = append(tight, "USER.md ("+user.Usage+")")
	}
	if len(tight) == 0 {
		return ""
	}
	return "SPACE PRESSURE: " + strings.Join(tight, " and ") +
		" near the character limit. Before adding anything, prune: remove or replace entries that are " +
		"unimportant, redundant, or outdated. Entries are listed oldest-first (incremental append), so " +
		"earlier ones are usually the best to evict — but keep older entries that are still important; you " +
		"judge by importance. This round should NOT grow the file: prefer remove/replace over add, and add " +
		"only if you free at least as much space.\n\n"
}

func nearLimit(r memory.Result) bool {
	return r.Limit > 0 && float64(r.Size) >= compactionPressureThreshold*float64(r.Limit)
}

func formatEntriesForExtract(res memory.Result) string {
	if len(res.Entries) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for i, entry := range res.Entries {
		fmt.Fprintf(&b, "--- entry %d ---\n%s\n", i+1, entry)
	}
	return b.String()
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
