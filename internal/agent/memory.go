package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

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

// 提取提示的默认字符预算(可被 autoPersist.perMessageChars / .maxPromptChars 覆盖)。
// 单条消息截断与整批总量都按 rune(字符)计——按字节切会在中文等多字节字符中间断开。
// 默认值取得宽松(现代模型动辄 100K+ 上下文,没必要扣扣嗖嗖),只在真的塞不下时才截。
const (
	defaultPerMessageChars = 4000
	defaultMaxPromptChars  = 200000
)

// extractMemoryToolName 是节拍提取走工具通道时使用的工具名。
const extractMemoryToolName = "persist_memory"

// extractionMemoryTool 是节拍提取的结构化工具:让模型把 add/replace/remove 操作作为
// 类型化工具入参返回(memory_ops → MEMORY.md, user_ops → USER.md),而非塞在正文 JSON 里——
// 入参走独立通道、不经正文,免去从散文里捞 JSON 那一整类失败。其 schema 与正文回退路径
// 解析的 {memory_ops, user_ops} 同形,因此工具通道与回退共用同一套下游逻辑。
func extractionMemoryTool() provider.Tool {
	op := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":   map[string]any{"type": "string", "enum": []string{"add", "replace", "remove"}},
			"old_text": map[string]any{"type": "string", "description": "Exact verbatim text of the entry to replace or remove."},
			"content":  map[string]any{"type": "string", "description": "Entry text for add, or new text for replace."},
		},
		"required": []string{"action"},
	}
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        extractMemoryToolName,
			Description: "Persist long-term memory: emit add/replace/remove operations for MEMORY.md (memory_ops) and USER.md (user_ops). Pass an empty array for a file that needs no change.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"memory_ops": map[string]any{"type": "array", "items": op, "description": "Operations for MEMORY.md (long-term facts)."},
					"user_ops":   map[string]any{"type": "array", "items": op, "description": "Operations for USER.md (chatter profile)."},
				},
			},
		},
	}
}

// firstToolCall 返回 resp 中第一个名为 name 的工具调用;名称不匹配但确实调了某个工具时
// (我们只 offer 了一个工具)退而用第一个。没有任何工具调用返回 (_, false)。
func firstToolCall(resp *provider.Response, name string) (provider.ToolCall, bool) {
	if resp == nil {
		return provider.ToolCall{}, false
	}
	for _, c := range resp.ToolCalls {
		if c.Function.Name == name {
			return c, true
		}
	}
	if len(resp.ToolCalls) > 0 {
		return resp.ToolCalls[0], true
	}
	return provider.ToolCall{}, false
}

// AutoPersistMemory uses the LLM to extract add/replace/remove operations for
// MEMORY.md and USER.md, then applies them through the managed memory engine.
// perMessageChars / maxPromptChars 是提取输入的字符(rune)预算;<=0 时回退到默认值。
func AutoPersistMemory(ctx context.Context, mgr *memory.Manager, prov provider.Provider, model string, groups []store.TurnGroup, perMessageChars, maxPromptChars int) error {
	if perMessageChars <= 0 {
		perMessageChars = defaultPerMessageChars
	}
	if maxPromptChars <= 0 {
		maxPromptChars = defaultMaxPromptChars
	}
	// 为 LLM 构建提取输入:按 session 分节(### Session),每条消息按 rune 截断到
	// perMessageChars,并对整批设 maxPromptChars 的 rune 总量上限,避免积压追赶时一次
	// 塞进过多 turn 把 prompt 撑爆(只约束输入拼装,与 max_tokens 控制的输出无关)。
	var sb strings.Builder
	total := 0
	truncated := false
buildPrompt:
	for _, g := range groups {
		header := fmt.Sprintf("### Session %s\n", g.SessionKey)
		sb.WriteString(header)
		total += utf8.RuneCountInString(header)
		for _, m := range g.Messages {
			if m.Role == "system" || m.Origin != "" {
				continue // 跳过 system 与 goal_context 等合成注入行
			}
			line := fmt.Sprintf("[%s]: %s\n", m.Role, truncateRunes(m.Content, perMessageChars))
			lineRunes := utf8.RuneCountInString(line)
			if total+lineRunes > maxPromptChars {
				truncated = true
				break buildPrompt
			}
			sb.WriteString(line)
			total += lineRunes
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

%sCall the persist_memory tool with your operations (memory_ops for MEMORY.md, user_ops for USER.md). Pass an empty array for a file that needs no change. The tool input shape is:
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
	}, []provider.Tool{extractionMemoryTool()}, model, 2048, 0.3)
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
	// 优先从工具通道读结构化入参:模型走 persist_memory 工具时,ops 作为类型化的工具
	// 入参从独立字段返回、不经正文,免去从散文里捞 JSON。未调工具(便宜模型/弱后端可能
	// 直接用正文回)则回退解析正文——沿用 stripJSONFence 剥模型反射性包裹的 ```json 围栏。
	rawOps := ""
	if call, ok := firstToolCall(resp, extractMemoryToolName); ok {
		rawOps = call.Function.Arguments
	} else {
		rawOps = stripJSONFence(resp.Content)
	}
	if err := json.Unmarshal([]byte(rawOps), &parsed); err != nil {
		// 与上面相同的 Warn 升级——静默跳过会隐藏"模型返回了不合规 JSON,解析失败"。
		preview := rawOps
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

// truncateRunes 按 rune(而非字节)将 s 截断到最多 max 个字符,超出时追加 "..."。
// 旧的 s[:N] 按字节切会在多字节字符(如中文,3 字节/字)中间断开,产生半个非法 UTF-8
// 字节;按 rune 切保证边界落在完整字符上。max<=0 返回空串。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max]) + "..."
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
