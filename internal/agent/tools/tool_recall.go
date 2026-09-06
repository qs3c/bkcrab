package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/store"
)

const (
	// recallDefaultLimit 是一次回溯默认返回的字符数。定得比压缩的
	// 裁剪阈值(2000)大一个量级,让"取回来看一眼"通常一次到位;又远小于
	// 上下文预算,免得一次回溯就把刚压下去的空间重新吃满。
	recallDefaultLimit = 8000
	// recallMaxLimit 是硬上限,模型传再大也截到这里。没有这道闸,一条
	// 500KB 的 exec 输出被原样拉回上下文会当场触发下一轮压缩——压缩再把它
	// 裁掉、再给一个 ref,循环往复。
	recallMaxLimit = 32000
	// recallGrepContext 是 grep 命中行上下各保留的行数。
	recallGrepContext = 2
	// recallMaxGrepMatches 限制 grep 模式返回的命中块数量。
	recallMaxGrepMatches = 40
	// recallListLimit 限制列表模式返回的行数。超出时只报总数,让模型
	// 用 tool 参数收窄,而不是把几百行索引灌进上下文。
	recallListLimit = 60
)

type recallToolResultArgs struct {
	// Ref 是工具消息在 session_messages 里的 seq。指针类型是为了区分
	// "没传 ref"(列表模式)和"传了 ref=0"(会话第一条消息,合法值)。
	Ref    *int64 `json:"ref"`
	Grep   string `json:"grep"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
	Tool   string `json:"tool"`
}

func registerToolRecall(r *Registry) {
	r.Register(
		"recall_tool_result",
		"Read back the full original output of a tool result that context compaction replaced with a summary. "+
			"Pass the ref shown as `msg_ref:` in a [Tool Result Summary]. "+
			"Omit ref to list which tool results in this conversation can be recalled. "+
			"Scoped to the current conversation only.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ref": map[string]any{
					"type":        "integer",
					"description": "The msg_ref number from a [Tool Result Summary]. Omit to list recallable tool results instead.",
				},
				"grep": map[string]any{
					"type":        "string",
					"description": "Return only lines containing this substring (case-insensitive), with 2 lines of context. Much cheaper than reading the whole output — prefer it when you know what you are looking for.",
				},
				"offset": map[string]any{
					"type":        "integer",
					"description": "Character offset to start reading from. In grep mode, this is an offset into the filtered text including line numbers. Default 0. Use the continuation arguments from a truncated read, keeping the same grep pattern.",
				},
				"limit": map[string]any{
					"type": "integer",
					"description": fmt.Sprintf(
						"Max body characters to return, including in grep mode. Default %d, hard cap %d.", recallDefaultLimit, recallMaxLimit),
				},
				"tool": map[string]any{
					"type":        "string",
					"description": "List mode only: show only entries for this tool name.",
				},
			},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args recallToolResultArgs
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &args); err != nil {
					return "", fmt.Errorf("parse args: %w", err)
				}
			}
			scope, err := r.recallScope()
			if err != nil {
				return "", err
			}
			if args.Ref == nil {
				return scope.list(ctx, strings.TrimSpace(args.Tool))
			}
			return scope.get(ctx, *args.Ref, args)
		},
	)
}

// recallScope 是一次回溯调用的全部权限边界,在工具执行开始时从 registry
// 的每回合状态快照下来。模型的参数进不到这里面任何一个字段——它只能提供
// seq 和分页/过滤参数。
type recallScope struct {
	st         ToolRecallStore
	userID     string
	agentID    string
	sessionKey string
	// chatterUserID 非空时把查询进一步限制在本回合发言人自己的行上。
	// 见 recallScope() 里的取值规则。
	chatterUserID string
}

// recallScope 组装本回合的作用域。
//
// 聊天者隔离规则:UserSpace 拥有者看整个会话——他本来就能在 UI 里翻完整
// 归档(WebChatHistory 按 user_id 取全量),拦他没有意义;其他聊天者只看
// 自己的行。这道门只在共享 agent / 群聊——即同一个 session_key 下有多个
// 发言人——时才有实际作用,单人会话两条路走出来的结果一样。
//
// 刻意不看 callerIsAdmin:频道管理员管的是身份文件那一类 agent 配置,他
// 并没有读取归档的既有途径,把工具输出一并开给他是净新增的越权面。
func (r *Registry) recallScope() (*recallScope, error) {
	if r.toolRecallStore == nil {
		return nil, errors.New("tool recall is unavailable in this deployment (no persistent session archive)")
	}
	sessionKey := r.recallSessionKey
	if sessionKey == "" {
		sessionKey = r.sessionID
	}
	if r.userID == "" || r.agentID == "" || sessionKey == "" {
		return nil, errors.New("tool recall scope is unavailable for this turn")
	}
	scope := &recallScope{
		st:         r.toolRecallStore,
		userID:     r.userID,
		agentID:    r.agentID,
		sessionKey: sessionKey,
	}
	if r.chatterUserID != "" && r.chatterUserID != r.userID {
		scope.chatterUserID = r.chatterUserID
	}
	return scope, nil
}

func (s *recallScope) list(ctx context.Context, toolFilter string) (string, error) {
	refs, err := s.st.ListSessionToolRefs(ctx, s.userID, s.agentID, s.sessionKey, s.chatterUserID)
	if err != nil {
		return "", err
	}
	if toolFilter != "" {
		filtered := refs[:0:0]
		for _, ref := range refs {
			if strings.EqualFold(ref.Name, toolFilter) {
				filtered = append(filtered, ref)
			}
		}
		refs = filtered
	}
	slog.Info("recall_tool_result list",
		"agent", s.agentID, "session", s.sessionKey,
		"chatter_scoped", s.chatterUserID != "", "tool_filter", toolFilter, "rows", len(refs))

	if len(refs) == 0 {
		if toolFilter != "" {
			return fmt.Sprintf("No recallable tool results for tool %q in this conversation.", toolFilter), nil
		}
		return "No recallable tool results in this conversation.", nil
	}

	var b strings.Builder
	b.WriteString("[Recallable Tool Results]\n")
	// 从尾部开始展示:被裁剪的通常是较早的消息,但模型多数时候找的是
	// "刚才那次",最近的更可能命中。
	shown := refs
	if len(shown) > recallListLimit {
		shown = shown[len(shown)-recallListLimit:]
		fmt.Fprintf(&b, "showing the %d most recent of %d entries; pass \"tool\" to narrow\n",
			len(shown), len(refs))
	}
	b.WriteString("ref\ttool\tchars\n")
	for _, ref := range shown {
		name := ref.Name
		if name == "" {
			name = "unknown"
		}
		fmt.Fprintf(&b, "%d\t%s\t%d\n", ref.Seq, name, ref.Chars)
	}
	b.WriteString("\ncall recall_tool_result with {\"ref\": N} to read one; add \"grep\" to search inside it.")
	return b.String(), nil
}

func (s *recallScope) get(ctx context.Context, seq int64, args recallToolResultArgs) (string, error) {
	if seq < 0 {
		return "", fmt.Errorf("ref must be a non-negative message number, got %d", seq)
	}
	rec, err := s.st.GetSessionToolMessage(ctx, s.userID, s.agentID, s.sessionKey, s.chatterUserID, seq)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 刻意不区分"不存在"和"存在但不属于你":两者对模型是同一件事,
			// 而分开说等于把别人会话的 seq 分布泄露成一个探针。
			return "", fmt.Errorf("no recallable tool result with ref %d in this conversation "+
				"(call recall_tool_result with no ref to list what is available)", seq)
		}
		return "", err
	}

	body, note := renderRecallBody(rec.Content, args)
	slog.Info("recall_tool_result get",
		"agent", s.agentID, "session", s.sessionKey, "ref", seq,
		"tool", rec.Name, "chatter_scoped", s.chatterUserID != "",
		"total_chars", utf8.RuneCountInString(rec.Content), "returned_chars", utf8.RuneCountInString(body))

	var b strings.Builder
	b.WriteString("[Recalled Tool Result]\n")
	fmt.Fprintf(&b, "ref: %d\n", rec.Seq)
	if rec.Name != "" {
		b.WriteString("tool: " + rec.Name + "\n")
	}
	if rec.ToolCallID != "" {
		b.WriteString("tool_call_id: " + rec.ToolCallID + "\n")
	}
	fmt.Fprintf(&b, "total_chars: %d\n", utf8.RuneCountInString(rec.Content))
	if note != "" {
		b.WriteString(note + "\n")
	}
	b.WriteString("\n")
	b.WriteString(body)
	return b.String(), nil
}

// renderRecallBody 把原文切成这次要返回的那一段,并返回一行描述该切法的
// 注记(范围 / 命中数 / next_offset)。
func renderRecallBody(content string, args recallToolResultArgs) (body, note string) {
	if grep := strings.TrimSpace(args.Grep); grep != "" {
		content, note = grepRecallBody(content, grep)
	}
	body, pageNote := paginateRecallBody(content, args)
	if note == "" {
		return body, pageNote
	}
	if pageNote != "" {
		note += "\n" + pageNote
	}
	return body, note
}

// paginateRecallBody 对普通原文和 grep 结果统一按 Unicode 字符分页。
func paginateRecallBody(content string, args recallToolResultArgs) (body, note string) {
	limit := args.Limit
	if limit <= 0 {
		limit = recallDefaultLimit
	}
	if limit > recallMaxLimit {
		limit = recallMaxLimit
	}
	runes := []rune(content)
	offset := args.Offset
	if offset < 0 {
		offset = 0
	}
	if offset == 0 && len(runes) == 0 {
		return "", ""
	}
	if offset >= len(runes) {
		return "", fmt.Sprintf("range: offset %d is past the end (%d chars)", offset, len(runes))
	}
	end := offset + limit
	if end >= len(runes) {
		if offset == 0 {
			return string(runes[offset:]), ""
		}
		return string(runes[offset:]), fmt.Sprintf("range: chars %d-%d of %d", offset, len(runes), len(runes))
	}
	next := map[string]any{"offset": end, "limit": limit}
	if args.Ref != nil {
		next["ref"] = *args.Ref
	}
	if grep := strings.TrimSpace(args.Grep); grep != "" {
		next["grep"] = grep
	}
	continuation, _ := json.Marshal(next)
	return string(runes[offset:end]), fmt.Sprintf(
		"range: chars %d-%d of %d (truncated)\nnext_offset: %d\ncontinue with %s",
		offset, end, len(runes), end, continuation)
}

// grepRecallBody 只返回命中行及其上下文。这是回溯里最省 token 的用法——
// 多数时候模型要的是原文里一个具体的值,而不是整篇输出。
func grepRecallBody(content, pattern string) (body, note string) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	needle := strings.ToLower(pattern)
	keep := make(map[int]bool)
	matches := 0
	for i, line := range lines {
		if !strings.Contains(strings.ToLower(line), needle) {
			continue
		}
		matches++
		if matches > recallMaxGrepMatches {
			break
		}
		for j := i - recallGrepContext; j <= i+recallGrepContext; j++ {
			if j >= 0 && j < len(lines) {
				keep[j] = true
			}
		}
	}
	if matches == 0 {
		return "", fmt.Sprintf("grep %q: no matching lines in %d lines", pattern, len(lines))
	}

	var b strings.Builder
	prev := -1
	for i := 0; i < len(lines); i++ {
		if !keep[i] {
			continue
		}
		if prev >= 0 && i > prev+1 {
			b.WriteString("  ...\n")
		}
		// 行号按 1 起算,与模型看惯的编辑器/grep 输出一致。
		fmt.Fprintf(&b, "%d: %s\n", i+1, lines[i])
		prev = i
	}
	note = fmt.Sprintf("grep %q: %d matching lines (of %d)", pattern, matches, len(lines))
	if matches > recallMaxGrepMatches {
		note = fmt.Sprintf("grep %q: showing first %d matches (of %d lines); narrow the pattern for the rest",
			pattern, recallMaxGrepMatches, len(lines))
	}
	return b.String(), note
}
