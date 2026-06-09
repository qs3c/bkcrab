package channels

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// LINE Messaging API adapter. Webhook-driven inbound + REST outbound,
// same shape as the Feishu adapter but with two key differences:
//
//  1. Inbound auth is HMAC-SHA256(channel_secret, raw_body) compared
//     against the `x-line-signature` header — stricter than Feishu's
//     plaintext verification token, so the webhook handler must hand
//     us the raw body bytes alongside the signature.
//  2. Outbound has TWO send endpoints — `reply` (uses a per-event
//     `replyToken`, free, single-use within ~5 minutes of the inbound)
//     and `push` (no token, consumes the bot's monthly free quota).
//     We cache the most recent replyToken per chatID so the FIRST
//     outbound after an inbound goes through reply (free); subsequent
//     messages or messages after token expiry fall back to push.
//
// AccountID is the bot's `userId` (stable per channel, returned by
// /v2/bot/info). AccountConfig.BotToken stores channel_access_token,
// AccountConfig.UserID stores channel_secret (matches the field's
// "extra account-scoped identifier" comment).

const (
	lineAPIBase         = "https://api.line.me"
	lineReplyURL        = lineAPIBase + "/v2/bot/message/reply"
	linePushURL         = lineAPIBase + "/v2/bot/message/push"
	lineBotInfoURL      = lineAPIBase + "/v2/bot/info"
	lineSendTimeout     = 15 * time.Second
	lineReplyTokenTTL   = 4 * time.Minute // server-side limit is ~5min; refresh under to avoid races
)

// LINE implements the Channel interface for a LINE Messaging API bot.
type LINE struct {
	bus            *bus.MessageBus
	accountID      string // == bot userId (Uxxxxxxxxxxxxxxxx)
	channelToken   string
	channelSecret  string

	httpClient *http.Client

	mu        sync.Mutex
	botName   string
	basicID   string // "@xxx" handle, surfaced for display
	// replyTokens caches the most recent inbound replyToken per chat.
	// Single-use, ~5min TTL. First outbound after an inbound pops the
	// token; subsequent messages in the same turn use the push API
	// (which costs the bot's monthly free quota).
	replyTokens map[string]lineReplyToken
}

type lineReplyToken struct {
	token   string
	expires time.Time
}

// NewLINE creates a LINE channel adapter from a stored credential pair.
func NewLINE(channelToken, channelSecret, accountID string, mb *bus.MessageBus) (*LINE, error) {
	if channelToken == "" {
		return nil, errors.New("line: channelToken required")
	}
	return &LINE{
		bus:           mb,
		accountID:     accountID,
		channelToken:  channelToken,
		channelSecret: channelSecret,
		httpClient:    &http.Client{Timeout: lineSendTimeout},
		replyTokens:   make(map[string]lineReplyToken),
	}, nil
}

func (l *LINE) Name() string      { return "line" }
func (l *LINE) AccountID() string { return l.accountID }
func (l *LINE) BotUsername() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.basicID
}

// Start fetches /v2/bot/info to surface the bot's display name + basicId,
// then blocks until ctx is done — events arrive via webhook, not poll.
// Failure of /v2/bot/info doesn't break the channel: outbound still
// works, the username is just empty.
func (l *LINE) Start(ctx context.Context) error {
	if name, basicID, err := l.fetchBotInfo(ctx); err != nil {
		slog.Warn("line bot info fetch failed", "account", l.accountID, "error", err)
	} else {
		l.mu.Lock()
		l.botName = name
		l.basicID = basicID
		l.mu.Unlock()
		slog.Info("line bot connected", "account", l.accountID, "name", name, "basic_id", basicID)
	}
	<-ctx.Done()
	return nil
}

// Send is the simple text path used by tools / tests.
func (l *LINE) Send(chatID, text string) error {
	return l.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage delivers Text to a LINE chat. Uses replyToken when one is
// cached and unexpired (free); otherwise falls back to the push API
// (consumes the bot's free quota — 200 msgs/month/account at the time
// of writing). MediaItems are deferred — LINE supports image messages
// but they require a public CDN URL or upload via /v2/bot/message/...,
// neither of which we plumb yet.
func (l *LINE) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	if msg.Text == "" {
		slog.Debug("line send: media-only message dropped (image upload not implemented)",
			"account", l.accountID, "chat", msg.ChatID)
		return nil
	}

	// LINE renders plain text only — no markdown anywhere. GFM tables
	// would arrive as raw `|cell|cell|`; FlattenMarkdownTables collapses
	// them to label:value or middle-dot lines first.
	text := FlattenMarkdownTables(msg.Text)

	// Pop a cached replyToken if present + unexpired. Single-use, so
	// this also clears the slot — concurrent sends in the same turn
	// only get one shot at the free reply path.
	if tok := l.popReplyToken(msg.ChatID); tok != "" {
		if err := l.postReply(tok, text); err == nil {
			return nil
		} else {
			// reply can fail (token already consumed by a parallel
			// reply, or expired in flight). Fall through to push so
			// the user still gets the message; log so the cause is
			// visible if it becomes a pattern.
			slog.Debug("line reply failed, falling back to push",
				"account", l.accountID, "chat", msg.ChatID, "error", err)
		}
	}
	return l.postPush(msg.ChatID, text)
}

// SendTyping is a no-op. LINE doesn't expose a typing indicator API
// for bots; the "loading animation" feature is paid + chat-scoped and
// not worth wiring for a 5s polling loop.
func (l *LINE) SendTyping(_ string) error { return nil }

// --- Inbound webhook ---

// LINEEventEnvelope is the webhook body shape.
type LINEEventEnvelope struct {
	Destination string      `json:"destination"`
	Events      []LINEEvent `json:"events"`
}

type LINEEvent struct {
	Type       string         `json:"type"` // "message" | "follow" | "join" | "leave" | ...
	Mode       string         `json:"mode,omitempty"`
	Timestamp  int64          `json:"timestamp"`
	ReplyToken string         `json:"replyToken,omitempty"`
	Source     LINESource     `json:"source"`
	Message    *LINEMessage   `json:"message,omitempty"`
	WebhookEventID string     `json:"webhookEventId,omitempty"`
}

type LINESource struct {
	Type    string `json:"type"` // "user" | "group" | "room"
	UserID  string `json:"userId,omitempty"`
	GroupID string `json:"groupId,omitempty"`
	RoomID  string `json:"roomId,omitempty"`
}

type LINEMessage struct {
	Type string `json:"type"` // "text" | "sticker" | "image" | ...
	ID   string `json:"id"`
	Text string `json:"text,omitempty"`
}

// HandleWebhook validates the HMAC signature against `body` (raw bytes
// — Go's json.Decode would re-encode and break the comparison) and
// dispatches each event. Returns response body + HTTP status for the
// caller to write back. LINE expects 200 with any body to ack;
// non-2xx triggers up to ~5 retries.
func (l *LINE) HandleWebhook(body []byte, signature string) (responseBody []byte, status int, err error) {
	if l.channelSecret != "" {
		mac := hmac.New(sha256.New, []byte(l.channelSecret))
		mac.Write(body)
		expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(expected), []byte(signature)) {
			return nil, http.StatusUnauthorized, errors.New("line signature mismatch")
		}
	}
	var env LINEEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parse: %w", err)
	}
	for _, ev := range env.Events {
		l.dispatchEvent(ev)
	}
	return []byte(`{"ok":true}`), http.StatusOK, nil
}

// dispatchEvent translates a LINE event into a bus.InboundMessage.
// Drops non-text events (sticker/image/file/follow/etc.) until we
// support them. ChatID resolution prefers the most-specific identifier
// (groupId / roomId / userId) so DMs and groups end up in distinct
// session keys.
func (l *LINE) dispatchEvent(ev LINEEvent) {
	if ev.Type != "message" || ev.Message == nil {
		// Non-message events (follow, unfollow, postback, …) — skip.
		return
	}
	if ev.Message.Type != "text" || ev.Message.Text == "" {
		slog.Debug("line non-text message skipped",
			"account", l.accountID, "type", ev.Message.Type)
		return
	}

	chatID, peerKind := lineChatKey(ev.Source)
	if chatID == "" {
		slog.Debug("line event without identifiable source", "account", l.accountID)
		return
	}

	// Stash the replyToken so the FIRST outbound for this chat in the
	// next ~5min uses the free reply path. Per-chat slot — multi-turn
	// conversations naturally roll the slot forward each inbound.
	if ev.ReplyToken != "" {
		l.mu.Lock()
		l.replyTokens[chatID] = lineReplyToken{
			token:   ev.ReplyToken,
			expires: time.Now().Add(lineReplyTokenTTL),
		}
		l.mu.Unlock()
	}

	slog.Info("line message received",
		"account", l.accountID,
		"from", ev.Source.UserID,
		"chat", chatID,
		"len", len(ev.Message.Text))

	l.bus.Inbound <- bus.InboundMessage{
		Channel:   "line",
		AccountID: l.accountID,
		ChatID:    chatID,
		UserID:    ev.Source.UserID,
		MessageID: ev.Message.ID,
		Text:      ev.Message.Text,
		PeerKind:  peerKind,
	}
}

// lineChatKey picks the most-specific chat identifier from a source
// block and returns it alongside fastclaw's peerKind tag. LINE has
// three chat scopes: 1:1 with a user, multi-person room, and group.
// We collapse room/group → "group" since fastclaw doesn't distinguish
// the two further down the pipeline.
func lineChatKey(s LINESource) (chatID, peerKind string) {
	switch s.Type {
	case "group":
		return s.GroupID, "group"
	case "room":
		return s.RoomID, "group"
	case "user":
		return s.UserID, "dm"
	}
	return "", ""
}

func (l *LINE) popReplyToken(chatID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.replyTokens[chatID]
	if !ok {
		return ""
	}
	delete(l.replyTokens, chatID)
	if time.Now().After(t.expires) {
		return ""
	}
	return t.token
}

// --- HTTP plumbing ---

func (l *LINE) postReply(replyToken, text string) error {
	body, _ := json.Marshal(map[string]any{
		"replyToken": replyToken,
		"messages":   []map[string]string{{"type": "text", "text": text}},
	})
	return l.postJSON(lineReplyURL, body)
}

func (l *LINE) postPush(chatID, text string) error {
	body, _ := json.Marshal(map[string]any{
		"to":       chatID,
		"messages": []map[string]string{{"type": "text", "text": text}},
	})
	return l.postJSON(linePushURL, body)
}

func (l *LINE) postJSON(url string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), lineSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+l.channelToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("contact line: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("line %s HTTP %d: %s", url, resp.StatusCode, string(respBody))
	}
	return nil
}

// fetchBotInfo calls /v2/bot/info to capture the bot's display name +
// basicId. Best-effort.
func (l *LINE) fetchBotInfo(ctx context.Context) (name, basicID string, err error) {
	ctx, cancel := context.WithTimeout(ctx, lineSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, lineBotInfoURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+l.channelToken)
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		UserID      string `json:"userId"`
		BasicID     string `json:"basicId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	return out.DisplayName, out.BasicID, nil
}

// LINEValidateCredentials is the connect-handler validation step:
// hits /v2/bot/info to confirm the channel access token is good and
// captures the bot's userId + display name. Returns (userId,
// displayName, basicId, error).
func LINEValidateCredentials(ctx context.Context, channelToken string) (userID, displayName, basicID string, err error) {
	stub := &LINE{
		channelToken: channelToken,
		httpClient:   &http.Client{Timeout: lineSendTimeout},
	}
	ctx, cancel := context.WithTimeout(ctx, lineSendTimeout)
	defer cancel()
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, lineBotInfoURL, nil)
	if rerr != nil {
		return "", "", "", rerr
	}
	req.Header.Set("Authorization", "Bearer "+channelToken)
	resp, derr := stub.httpClient.Do(req)
	if derr != nil {
		return "", "", "", fmt.Errorf("contact line: %w", derr)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("line bot info HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		UserID      string `json:"userId"`
		BasicID     string `json:"basicId"`
		DisplayName string `json:"displayName"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", "", err
	}
	if out.UserID == "" {
		return "", "", "", errors.New("line /bot/info returned empty userId")
	}
	return out.UserID, out.DisplayName, out.BasicID, nil
}
