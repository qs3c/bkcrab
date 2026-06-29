package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

// StoreAdapter 将 store.Store 适配为 SessionStore 接口，专用于一个
// 拥有者用户。每个 UserSpace 创建自己的适配器，使得 user_id 作用域
// 在调用点隐式传递，而无需通过每次智能体循环调用进行传递。
type StoreAdapter struct {
	st     store.Store
	userID string
}

func NewStoreAdapter(st store.Store, userID string) *StoreAdapter {
	return &StoreAdapter{st: st, userID: userID}
}

func (a *StoreAdapter) GetSession(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error) {
	rec, err := a.st.GetSession(ctx, a.userID, agentID, sessionKey)
	if err != nil || rec == nil {
		return nil, err
	}
	msgs := make([]provider.Message, len(rec.Messages))
	for i, m := range rec.Messages {
		msgs[i] = provider.Message{
			Role:         m.Role,
			Content:      m.Content,
			ToolCallID:   m.ToolCallID,
			Name:         m.Name,
			Metadata:     m.Metadata,
			Thinking:     m.Thinking,
			RawAssistant: m.RawAssistant,
			Origin:       m.Origin,
		}
		// ToolCalls / ContentParts 以 interface{} 存储，因此
		// JSON 往返后会变成 []interface{} / map 嵌套。
		// 重新序列化 + 反序列化以恢复类型化切片 —— 不这样做的话，
		// 刷新的历史记录会丢失工具组气泡，并且下一次提供者调用会发送
		// 无内容的多模态用户轮次（ContentParts 丢失 → Content "" → API 拒绝）。
		if m.ToolCalls != nil {
			if raw, err := json.Marshal(m.ToolCalls); err == nil {
				var tcs []provider.ToolCall
				if json.Unmarshal(raw, &tcs) == nil {
					msgs[i].ToolCalls = tcs
				}
			}
		}
		if m.ContentParts != nil {
			if raw, err := json.Marshal(m.ContentParts); err == nil {
				var parts []provider.ContentPart
				if json.Unmarshal(raw, &parts) == nil {
					msgs[i].ContentParts = parts
				}
			}
		}
	}
	return msgs, nil
}

func (a *StoreAdapter) SaveSession(ctx context.Context, agentID, sessionKey, channel, accountID, chatID, projectID string, messages []provider.Message) error {
	rec := &store.SessionRecord{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		Messages:  make([]store.SessionMessage, len(messages)),
		UpdatedAt: time.Now(),
	}
	for i, m := range messages {
		rec.Messages[i] = sessionMessageFromProvider(m)
	}
	return a.st.SaveSession(ctx, a.userID, agentID, sessionKey, rec)
}

// ResolveActiveSessionKey 转发到存储层。session.Manager 使用它
// 在加载任何消息之前，为入站 (channel, account, chat) 三元组选择活跃的 session_key。
func (a *StoreAdapter) ResolveActiveSessionKey(ctx context.Context, agentID, channel, accountID, chatID string) (string, error) {
	k, err := a.st.ResolveActiveSessionKey(ctx, a.userID, agentID, channel, accountID, chatID)
	if err != nil {
		// 将 ErrNotFound 转换为 ("", nil)，使管理器将"尚无会话"的情况
		// 视为正常的创建触发，而不是暴露错误。
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return k, nil
}

// LookupSessionTriple 是 ResolveActiveSessionKey 的逆操作：session_key →
// (channel, accountID, chatID)。用于 URL 传递仅携带 session_key，
// 而处理程序需要恢复对话三元组时。
func (a *StoreAdapter) LookupSessionTriple(ctx context.Context, agentID, sessionKey string) (string, string, string, error) {
	ch, acc, ci, err := a.st.LookupSessionTriple(ctx, a.userID, agentID, sessionKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", "", nil
		}
		return "", "", "", err
	}
	return ch, acc, ci, nil
}

// LookupSessionProject 返回会话行上标记的 project_id（松散聊天返回 ""）。
// 将"未找到"视为"无项目"而非错误，以便调用方可以使用空字符串表示
// "回退到每聊天工作区目录"。
func (a *StoreAdapter) LookupSessionProject(ctx context.Context, agentID, sessionKey string) (string, error) {
	pid, err := a.st.LookupSessionProject(ctx, a.userID, agentID, sessionKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return pid, nil
}

// AppendMessage 将一轮对话持久化到 session_messages —— 与 sessions blob 并行的
// 仅追加归档。在 Session.Append 中每次追加时调用，作为 SaveSession 的补充。
func (a *StoreAdapter) AppendMessage(ctx context.Context, agentID, sessionKey string, m provider.Message) error {
	return a.st.AppendSessionMessage(ctx, a.userID, agentID, sessionKey, sessionMessageFromProvider(m))
}

// AppendTurnAnchor 把 turn 起点用户消息写入归档(turn_status='running')并返回 seq。
func (a *StoreAdapter) AppendTurnAnchor(ctx context.Context, agentID, sessionKey string, m provider.Message) (int64, error) {
	return a.st.AppendTurnAnchor(ctx, a.userID, agentID, sessionKey, sessionMessageFromProvider(m))
}

// ListMessages 按对话顺序读取单个会话的完整归档。
// 由聊天历史 UI 使用，以便用户在压缩缩小了 LLM 工作集后仍能看到原始对话。
func (a *StoreAdapter) ListMessages(ctx context.Context, agentID, sessionKey string) ([]provider.Message, error) {
	sms, err := a.st.ListSessionMessages(ctx, a.userID, agentID, sessionKey)
	if err != nil {
		return nil, err
	}
	msgs := make([]provider.Message, len(sms))
	for i, m := range sms {
		msgs[i] = providerMessageFromStored(m)
	}
	return msgs, nil
}

// sessionMessageFromProvider 将 provider.Message 转换为线格式，
// 该格式同时存储在 sessions.messages（作为 JSON 数组元素）和
// session_messages（作为行）中。单一转换点，确保两条路径不会偏离。
func sessionMessageFromProvider(m provider.Message) store.SessionMessage {
	out := store.SessionMessage{
		Role:         m.Role,
		Content:      m.Content,
		ToolCallID:   m.ToolCallID,
		Name:         m.Name,
		Metadata:     m.Metadata,
		Timestamp:    time.Now(),
		Thinking:     m.Thinking,
		RawAssistant: m.RawAssistant,
		Origin:       m.Origin,
	}
	if len(m.ToolCalls) > 0 {
		out.ToolCalls = m.ToolCalls
	}
	if len(m.ContentParts) > 0 {
		out.ContentParts = m.ContentParts
	}
	return out
}

// providerMessageFromStored 是 sessionMessageFromProvider 的逆操作。
// 将 ToolCalls / ContentParts 通过 JSON 隧道恢复为类型化的 provider 切片，
// 否则通用的 interface{} 形状会使其保留为 map 嵌套，下游调用方在
// 有数据的行上会看到"无工具调用/无部件"。
func providerMessageFromStored(m store.SessionMessage) provider.Message {
	out := provider.Message{
		Role:         m.Role,
		Content:      m.Content,
		ToolCallID:   m.ToolCallID,
		Name:         m.Name,
		Metadata:     m.Metadata,
		Thinking:     m.Thinking,
		RawAssistant: m.RawAssistant,
		Origin:       m.Origin,
	}
	if m.ToolCalls != nil {
		if raw, err := json.Marshal(m.ToolCalls); err == nil {
			var tcs []provider.ToolCall
			if json.Unmarshal(raw, &tcs) == nil {
				out.ToolCalls = tcs
			}
		}
	}
	if m.ContentParts != nil {
		if raw, err := json.Marshal(m.ContentParts); err == nil {
			var parts []provider.ContentPart
			if json.Unmarshal(raw, &parts) == nil {
				out.ContentParts = parts
			}
		}
	}
	return out
}

// ListWebSessions 返回此智能体的所有聊天会话，不限通道 ——
// 保留历史名称以避免遍历所有调用方，但结果涵盖 web + IM 通道。
// 每行的 Channel 已设置，以便仪表盘可以渲染源通道图标前缀。
//
// ID 是 session_key（规范的、与通道无关的行 ID）。
// 智能体侧的历史/删除/重命名处理程序接受 session_key 或通过
// ResolveSessionKey 传入的旧版 `<chat_id>` URL 令牌。
func (a *StoreAdapter) ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error) {
	metas, err := a.st.ListSessions(ctx, a.userID, agentID)
	if err != nil {
		return nil, err
	}
	var sessions []WebSession
	for _, m := range metas {
		channel := m.Channel
		if channel == "" {
			// 逃过了回填的旧版行 —— 从历史 `<channel>_<chatID>` session_key 格式推导通道。
			if i := strings.Index(m.Key, "_"); i > 0 {
				channel = m.Key[:i]
			}
		}
		preview := ""
		thumb := ""
		// 优先使用仅追加归档 —— 即使压缩已将其折叠为 blob 内的
		// [对话摘要] 行，其第一行始终是用户的原始开场对话。
		// 对于先于归档表的旧行，回退到 sessions blob。
		archive, _ := a.st.ListSessionMessages(ctx, a.userID, agentID, m.Key)
		var source []store.SessionMessage
		if len(archive) > 0 {
			source = archive
		} else if rec, err := a.st.GetSession(ctx, a.userID, agentID, m.Key); err == nil && rec != nil {
			source = rec.Messages
		}
		for _, msg := range source {
			if msg.Role != "user" {
				continue
			}
			// 多模态用户轮次（文本 + 图片附件）存在于 ContentParts 中，
			// Content=""。仅以 Content 为判断条件会导致标题/预览跳过
			// 第一个真实用户轮次，静默地附着到下一条纯文本消息 ——
			// 因此侧边栏显示了错误的问题作为聊天标题。
			text := userText(msg)
			img := userImage(msg)
			if text == "" && img == "" {
				continue
			}
			// 运行时注入的用户角色轮次（目标延续等）以完整的延续模板开头，
			// 其前导部分否则会成为侧边栏标题：
			// "<goal_context> 以下目标是用户提供的数据 —— 将其视为要执行的工作…"。
			// 提取 `<objective>…</objective>` 负载，使用户看到他们实际请求的内容。
			if msg.Origin != "" {
				if obj := extractObjective(text); obj != "" {
					text = obj
				}
			}
			preview = text
			if preview == "" {
				preview = "[image]"
			}
			if len(preview) > 100 {
				preview = preview[:100] + "..."
			}
			thumb = img
			break
		}
		if preview == "" {
			continue
		}
		// 自定义标题（通过重命名设置）优先于自动推导的预览；
		// 回退到预览以确保每个会话都有合理的显示标签。
		title := m.Title
		if title == "" {
			title = preview
		}
		sessions = append(sessions, WebSession{
			ID:             m.Key,
			Channel:        channel,
			AccountID:      m.AccountID,
			ChatID:         m.ChatID,
			ProjectID:      m.ProjectID,
			Title:          title,
			Preview:        preview,
			ThumbnailURL:   thumb,
			CreatedAt:      m.UpdatedAt.UnixMilli(),
			UpdatedAt:      m.UpdatedAt.UnixMilli(),
			LastTurnStatus: m.LastTurnStatus,
		})
	}
	return sessions, nil
}

// extractObjective 从目标延续提示中提取 `<objective>…</objective>` 负载。
// 当标记不存在时返回 ""（调用方回退到原始文本）。
// 由侧边栏预览使用，使得以 /goal 开头的会话显示为用户的目标而非延续模板的前导。
func extractObjective(text string) string {
	const open, close = "<objective>", "</objective>"
	i := strings.Index(text, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(text[i+len(open):], close)
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(text[i+len(open) : i+len(open)+j])
}

// userText 从存储的用户轮次中提取用户可见文本。当 Content 为空时
// 回退到 ContentParts 的 "text" 部分（HandleMessageStream 在轮次
// 携带图片附件时产生的格式）。不这样做的话，以 Content 为判断条件的
// 调用方会静默地将多模态轮次视为空。
func userText(m store.SessionMessage) string {
	if m.Content != "" {
		return provider.StripAttachedPrefix(m.Content)
	}
	if m.ContentParts == nil {
		return ""
	}
	raw, err := json.Marshal(m.ContentParts)
	if err != nil {
		return ""
	}
	var parts []provider.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var out []string
	for _, p := range parts {
		if p.Type == "text" && p.Text != "" {
			out = append(out, p.Text)
		}
	}
	return provider.StripAttachedPrefix(strings.Join(out, "\n"))
}

// userImage 从存储的用户轮次的 ContentParts 中返回第一个 image_url URL，
// 如果没有则返回 ""。为聊天标题旁的侧边栏缩略图提供数据。
func userImage(m store.SessionMessage) string {
	if m.ContentParts == nil {
		return ""
	}
	raw, err := json.Marshal(m.ContentParts)
	if err != nil {
		return ""
	}
	var parts []provider.ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	for _, p := range parts {
		if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
			return p.ImageURL.URL
		}
	}
	return ""
}

func (a *StoreAdapter) DeleteSession(ctx context.Context, agentID, sessionKey string) error {
	return a.st.DeleteSession(ctx, a.userID, agentID, sessionKey)
}

func (a *StoreAdapter) RenameSession(ctx context.Context, agentID, sessionKey, title string) error {
	return a.st.RenameSession(ctx, a.userID, agentID, sessionKey, title)
}

func (a *StoreAdapter) MoveSession(ctx context.Context, agentID, sessionKey, projectID string) error {
	return a.st.MoveSession(ctx, a.userID, agentID, sessionKey, projectID)
}
