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

	"github.com/qs3c/bkcrab/internal/bus"
)

// LINE Messaging API 适配器。Webhook 驱动的入站 + REST 出站，
// 与飞书适配器形状相同，但有两个关键区别：
//
//  1. 入站认证是 HMAC-SHA256(channel_secret, raw_body) 与
//     `x-line-signature` 头比较——比飞书的明文验证令牌更严格，
//     因此 webhook 处理程序必须将原始正文字节与签名一起传入。
//  2. 出站有两个发送端点——`reply`（使用每个事件的 `replyToken`，
//     免费，在入站后约 5 分钟内单次使用）和 `push`（无令牌，
//     消耗 bot 的月度免费配额）。我们缓存每个 chatID 最近的
//     replyToken，以便入站后的第一个出站通过 reply（免费）发送；
//     后续消息或令牌过期后的消息回退到 push。
//
// AccountID 是 bot 的 `userId`（每个渠道稳定，由 /v2/bot/info 返回）。
// AccountConfig.BotToken 存储 channel_access_token，
// AccountConfig.UserID 存储 channel_secret（匹配该字段
// "额外账户范围标识符"的注释）。

const (
	lineAPIBase       = "https://api.line.me"
	lineReplyURL      = lineAPIBase + "/v2/bot/message/reply"
	linePushURL       = lineAPIBase + "/v2/bot/message/push"
	lineBotInfoURL    = lineAPIBase + "/v2/bot/info"
	lineSendTimeout   = 15 * time.Second
	lineReplyTokenTTL = 4 * time.Minute // 服务端限制约 5 分钟；提前刷新以避免竞态
)

// LINE 实现 LINE Messaging API bot 的 Channel 接口。
type LINE struct {
	bus           *bus.MessageBus
	accountID     string // == bot userId (Uxxxxxxxxxxxxxxxx)
	channelToken  string
	channelSecret string

	httpClient *http.Client

	mu      sync.Mutex
	botName string
	basicID string // "@xxx" 句柄，用于显示
	// replyTokens 缓存每个聊天最近的入站 replyToken。单次使用，
	// 约 5 分钟 TTL。入站后的第一个出站弹出令牌；同一轮次中后续消息
	// 使用 push API（消耗 bot 的月度免费配额）。
	replyTokens map[string]lineReplyToken
}

type lineReplyToken struct {
	token   string
	expires time.Time
}

// NewLINE 从存储的凭据对创建 LINE 渠道适配器。
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

// Start 获取 /v2/bot/info 以展示 bot 的显示名称 + basicId，
// 然后阻塞直到 ctx 完成——事件通过 webhook 到达，而非轮询。
// /v2/bot/info 失败不会破坏渠道：出站仍然有效，只是用户名为空。
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

// Send 是工具/测试使用的简单文本路径。
func (l *LINE) Send(chatID, text string) error {
	return l.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage 将 Text 投递到 LINE 聊天。有缓存的未过期 replyToken 时
// 使用 replyToken（免费）；否则回退到 push API（消耗 bot 的月度免费配额——
// 目前为每月 200 条消息/账号）。MediaItems 暂不支持——LINE 支持图片消息
// 但需要公共 CDN URL 或通过 /v2/bot/message/... 上传，两者我们都尚未对接。
func (l *LINE) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	if msg.Text == "" {
		slog.Debug("line send: media-only message dropped (image upload not implemented)",
			"account", l.accountID, "chat", msg.ChatID)
		return nil
	}

	// LINE 仅渲染纯文本——无处使用 markdown。GFM 表格会以原始
	// `|cell|cell|` 到达；FlattenMarkdownTables 先将它们折叠为
	// label:value 或中点行。
	text := FlattenMarkdownTables(msg.Text)

	// 弹出缓存的 replyToken（如果存在且未过期）。单次使用，
	// 因此这也会清除槽位——同一轮次中的并发发送只有一次
	// 免费回复路径的机会。
	if tok := l.popReplyToken(msg.ChatID); tok != "" {
		if err := l.postReply(tok, text); err == nil {
			return nil
		} else {
			// 回复可能失败（令牌已被并行回复消耗，或在传输中过期）。
			// 回退到 push，以便用户仍然收到消息；记录日志以便在
			// 成为模式时可见原因。
			slog.Debug("line reply failed, falling back to push",
				"account", l.accountID, "chat", msg.ChatID, "error", err)
		}
	}
	return l.postPush(msg.ChatID, text)
}

// SendTyping 是空操作。LINE 不暴露 bot 的输入指示器 API；
// "加载动画"功能是付费 + 聊天范围的，不值得为 5 秒轮询间隔接入。
func (l *LINE) SendTyping(_ string) error { return nil }

// --- 入站 webhook ---

// LINEEventEnvelope 是 webhook 请求体结构。
type LINEEventEnvelope struct {
	Destination string      `json:"destination"`
	Events      []LINEEvent `json:"events"`
}

type LINEEvent struct {
	Type           string       `json:"type"` // "message" | "follow" | "join" | "leave" | ...
	Mode           string       `json:"mode,omitempty"`
	Timestamp      int64        `json:"timestamp"`
	ReplyToken     string       `json:"replyToken,omitempty"`
	Source         LINESource   `json:"source"`
	Message        *LINEMessage `json:"message,omitempty"`
	WebhookEventID string       `json:"webhookEventId,omitempty"`
}

type LINESource struct {
	Type    string `json:"type"`    // "user" | "group" | "room"
	UserID  string `json:"userId,omitempty"`
	GroupID string `json:"groupId,omitempty"`
	RoomID  string `json:"roomId,omitempty"`
}

type LINEMessage struct {
	Type string `json:"type"` // "text" | "sticker" | "image" | ...
	ID   string `json:"id"`
	Text string `json:"text,omitempty"`
}

// HandleWebhook 对照 `body`（原始字节——Go 的 json.Decode 会重新编码
// 并破坏比较）验证 HMAC 签名，并分派每个事件。返回响应体 + HTTP 状态码
// 供调用方写回。LINE 期望 200 状态和任意响应体来确认接收；
// 非 2xx 会触发约 5 次重试。
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

// dispatchEvent 将 LINE 事件转换为 bus.InboundMessage。
// 丢弃非文本事件（贴纸/图片/文件/关注/等）直到我们支持它们。
// ChatID 解析优先选择最具体的标识符（groupId / roomId / userId），
// 以便 DM 和群组最终作为不同的会话键。
func (l *LINE) dispatchEvent(ev LINEEvent) {
	if ev.Type != "message" || ev.Message == nil {
		// 非消息事件（关注、取关、回发等）——跳过。
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

	// 缓存 replyToken，以便此聊天在接下来约 5 分钟内的第一个出站
	// 使用免费回复路径。每个聊天一个槽位——多轮对话自然在每次
	// 入站时向前滚动槽位。
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

// lineChatKey 从源块中选择最具体的聊天标识符，并与 bkcrab 的
// peerKind 标签一起返回。LINE 有三种聊天范围：用户 1:1、多人房间、
// 群组。我们将 room/group 折叠为 "group"，因为 bkcrab 不会在
// 下游进一步区分两者。
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

// --- HTTP 管线 ---

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

// fetchBotInfo 调用 /v2/bot/info 获取 bot 的显示名称 + basicId。
// 尽力获取；失败不会破坏渠道。
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

// LINEValidateCredentials 是连接处理程序验证步骤：
// 获取 tenant_access_token 以确认 channel_access_token/app_secret 可用，
// 然后获取 /bot/v3/info 以捕获 bot 的显示名称。不创建适配器状态——
// 调用方持久化并热注册。
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
