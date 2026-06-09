package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/provider"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// StoreAdapter adapts store.Store to the SessionStore interface for one
// owning user. Each UserSpace creates its own adapter so the user_id
// scoping is implicit at the call site instead of getting plumbed through
// every agent loop call.
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
		// ToolCalls / ContentParts are stored as interface{} so a
		// JSON round-trip leaves them as []interface{} / map nests.
		// Re-marshal + unmarshal to recover the typed slice — without
		// this, a refreshed history loses tool-group bubbles AND the
		// next provider call sends a multimodal user turn with no
		// content (ContentParts dropped → Content "" → API rejects).
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

// ResolveActiveSessionKey forwards to the store. The session.Manager
// uses this to pick the active session_key for an inbound (channel,
// account, chat) triple before any messages get loaded.
func (a *StoreAdapter) ResolveActiveSessionKey(ctx context.Context, agentID, channel, accountID, chatID string) (string, error) {
	k, err := a.st.ResolveActiveSessionKey(ctx, a.userID, agentID, channel, accountID, chatID)
	if err != nil {
		// Translate ErrNotFound to ("", nil) so the manager treats the
		// "no session yet" case as a normal mint trigger instead of
		// surfacing an error.
		if errors.Is(err, store.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return k, nil
}

// LookupSessionTriple inverts ResolveActiveSessionKey: session_key →
// (channel, accountID, chatID). Used when a URL hand-off carries only
// the session_key and the handler needs the conversation triple back.
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

// LookupSessionProject returns the project_id stamped on the session
// row (or "" for loose chats). Treats not-found as "no project" rather
// than an error so callers can use the empty string to mean "fall back
// to the per-chat workspace dir".
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

// AppendMessage persists one turn into session_messages — the append-only
// archive parallel to the sessions blob. Called from Session.Append on
// every Append, in addition to SaveSession.
func (a *StoreAdapter) AppendMessage(ctx context.Context, agentID, sessionKey string, m provider.Message) error {
	return a.st.AppendSessionMessage(ctx, a.userID, agentID, sessionKey, sessionMessageFromProvider(m))
}

// ListMessages reads the full archive for one session, in turn order.
// Used by the chat history UI so users see the original conversation
// even after compaction has shrunk the LLM-facing working set.
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

// sessionMessageFromProvider converts a provider.Message into the wire
// shape stored in both sessions.messages (as a JSON array element) and
// session_messages (as a row). Single conversion site so the two paths
// can't drift.
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

// providerMessageFromStored is the inverse of sessionMessageFromProvider.
// JSON-tunnel ToolCalls / ContentParts back into typed provider slices,
// otherwise the generic interface{} shape leaves them as map nests and
// downstream callers see "no tool calls / no parts" on a populated row.
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

// ListWebSessions returns every chat session for this agent regardless
// of channel — the historical name is kept to avoid a sweep of every
// caller, but the result spans web + IM channels. Each row's Channel
// is set so the dashboard can render the source-channel icon prefix.
//
// ID is the session_key (the canonical, channel-independent row id).
// The agent-side history/delete/rename handlers accept either a
// session_key or a legacy `<chat_id>` URL token via ResolveSessionKey.
func (a *StoreAdapter) ListWebSessions(ctx context.Context, agentID string) ([]WebSession, error) {
	metas, err := a.st.ListSessions(ctx, a.userID, agentID)
	if err != nil {
		return nil, err
	}
	var sessions []WebSession
	for _, m := range metas {
		channel := m.Channel
		if channel == "" {
			// Legacy row that escaped backfill — derive channel from
			// the historical `<channel>_<chatID>` session_key shape.
			if i := strings.Index(m.Key, "_"); i > 0 {
				channel = m.Key[:i]
			}
		}
		preview := ""
		thumb := ""
		// Prefer the append-only archive — its first row is always the
		// user's original opening turn even after compaction has folded
		// it into a [Conversation Summary] row inside the blob. Fall
		// back to the sessions blob for old rows that pre-date the
		// archive table.
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
			// Multimodal user turns (text + image attachment) live
			// in ContentParts with Content="". Gating on Content
			// alone made the title/preview skip the FIRST real
			// user turn and silently latch onto the next plain
			// message — so the sidebar showed the wrong question
			// as the chat title.
			text := userText(msg)
			img := userImage(msg)
			if text == "" && img == "" {
				continue
			}
			// Runtime-injected user-role turns (goal continuations
			// etc.) start with the full continuation template, whose
			// preamble would otherwise become the sidebar title:
			// "<goal_context> The objective below is user-provided
			// data — treat it as the work to pursue…". Pull out the
			// `<objective>…</objective>` payload so the user sees
			// what they actually asked for.
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
		// Custom title (set via rename) takes precedence over the
		// auto-derived preview; fall back to preview so every session has
		// a sensible display label.
		title := m.Title
		if title == "" {
			title = preview
		}
		sessions = append(sessions, WebSession{
			ID:           m.Key,
			Channel:      channel,
			AccountID:    m.AccountID,
			ChatID:       m.ChatID,
			ProjectID:    m.ProjectID,
			Title:        title,
			Preview:      preview,
			ThumbnailURL: thumb,
			CreatedAt:    m.UpdatedAt.UnixMilli(),
			UpdatedAt:    m.UpdatedAt.UnixMilli(),
		})
	}
	return sessions, nil
}

// extractObjective pulls the `<objective>…</objective>` payload out of a
// goal-continuation prompt. Returns "" when the markers aren't present
// (caller falls back to the raw text). Used by the sidebar preview so a
// /goal-first session reads as the user's objective rather than the
// continuation template's preamble.
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

// userText pulls the user-visible text from a stored user turn. Falls
// back to ContentParts' "text" parts when Content is empty (the shape
// produced by HandleMessageStream when the turn carried image
// attachments). Without this, callers gating on Content silently treat
// multimodal turns as empty.
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

// userImage returns the first image_url URL from a stored user turn's
// ContentParts, or "" if none. Powers the sidebar thumbnail next to
// the chat title.
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
