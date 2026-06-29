package channels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

// Feishu（飞书）bot 适配器。Webhook 驱动：入站消息通过飞书开放平台
// HTTPS POST 到 bkcrab 的 webhook 路由到达（在 internal/setup/server.go
// 中设置）；出站回复通过 /open-apis/im/v1/messages 发送，使用我们按需
// 获取并缓存的 tenant_access_token。
//
// 飞书也提供长连接（WebSocket），但使用 Protobuf 帧格式——没有官方 SDK
// 手写协议面积太大。Webhook 仅 JSON 且与现有 bkcrab HTTP 服务器集成。
// 权衡：需要公共可达 URL。
//
// AppID 是 credential_key + accountID。AppSecret 存储在 AccountConfig.BotToken
// 中（语义匹配："bot 使用的秘密凭据"）。验证令牌存储在
// AccountConfig.UserID 中（匹配该字段"额外账户范围标识符"的注释）。

const (
	feishuBaseURL          = "https://open.feishu.cn"
	feishuTokenURL         = feishuBaseURL + "/open-apis/auth/v3/tenant_access_token/internal"
	feishuSendURL          = feishuBaseURL + "/open-apis/im/v1/messages"
	feishuBotInfoURL       = feishuBaseURL + "/open-apis/bot/v3/info"
	feishuSendTimeout      = 15 * time.Second
	feishuTokenTimeout     = 10 * time.Second
	feishuTokenRefreshSkew = 60 * time.Second
)

// Feishu 实现飞书 / 飞书自定义应用的 Channel 接口。
type Feishu struct {
	bus               *bus.MessageBus
	accountID         string // == app_id
	appID             string
	appSecret         string
	verificationToken string
	// encryptKey 非空时表示飞书 app 已配置"加密策略"。
	// 入站 webhook 请求体以 {"encrypt": "<b64>"} 到达，必须在 JSON 解析前
	// 用 AES-256-CBC 解密（key = sha256(encryptKey)，IV = 密文前 16 字节）。
	encryptKey string
	// useLongConn 将入站切换到飞书的 WebSocket/长连接路径（无需公共 URL）。
	// 为 true 时，Start() 启动 SDK ws 客户端 + startLongConn() 中的调度器；
	// 为 false 时，入站通过公共 HTTP webhook 到达，在 HandleWebhook() 中处理。
	useLongConn bool

	httpClient *http.Client

	mu           sync.Mutex
	accessTok    string
	accessTokExp time.Time
	botName      string // 在 Start 时通过 /bot/v3/info 填充；尽力获取
	botOpenID    string
}

// NewFeishu 创建飞书适配器。verificationToken 与飞书开发者控制台
// "Event Subscriptions → Verification Token" 中配置的值匹配；
// 我们用它来验证入站 webhook 载荷。
func NewFeishu(appID, appSecret, verificationToken, encryptKey string, useLongConn bool, accountID string, mb *bus.MessageBus) (*Feishu, error) {
	if appID == "" || appSecret == "" {
		return nil, errors.New("feishu: appID and appSecret required")
	}
	if accountID == "" {
		accountID = appID
	}
	return &Feishu{
		bus:               mb,
		accountID:         accountID,
		appID:             appID,
		appSecret:         appSecret,
		verificationToken: verificationToken,
		encryptKey:        encryptKey,
		useLongConn:       useLongConn,
		httpClient:        &http.Client{Timeout: feishuSendTimeout},
	}, nil
}

func (l *Feishu) Name() string        { return "feishu" }
func (l *Feishu) AccountID() string   { return l.accountID }
func (l *Feishu) BotUsername() string { return l.botName }

// Start 基本不做操作——飞书通过 webhook 路由推送事件而非我们轮询。
// 我们会先调用一次 /bot/v3/info 获取 bot 的显示名称，然后阻塞直到 ctx 完成。
// 获取 bot 信息失败不会使渠道失败：出站仍然有效，用户名只是为空
// （cron 绑定回退已经容许此情况）。
func (l *Feishu) Start(ctx context.Context) error {
	if name, openID, err := l.fetchBotInfo(ctx); err != nil {
		slog.Warn("feishu bot info fetch failed", "account", l.accountID, "error", err)
	} else {
		l.mu.Lock()
		l.botName = name
		l.botOpenID = openID
		l.mu.Unlock()
		slog.Info("feishu bot connected", "account", l.accountID, "name", name)
	}
	if l.useLongConn {
		// 长连接模式：向外建立到飞书的 WS 出站连接，无需公共 URL。
		// startLongConn() 阻塞直到 ctx 完成或 SDK 客户端返回致命错误。
		// 实现位于 feishu_ws.go 以保持 SDK 导入范围限定。
		return l.startLongConn(ctx)
	}
	<-ctx.Done()
	return nil
}

// Send 发送纯文本。用于不携带富负载的工具/测试路径。
func (l *Feishu) Send(chatID, text string) error {
	return l.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage 投递 Text +（可选）MediaItems。飞书的文本形状是在
// `content` 字段内 JSON 字符串化的 `{"text":"..."}`。MediaItems 暂缓——
// 发送图片需要先通过 /im/v1/images 上传到飞书 CDN，这是一个我们
// 还不需要的额外步骤，直到用户反馈。
func (l *Feishu) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	if msg.Text == "" {
		// 仅有 MediaItems 但无上传路径——跳过而非发送空气泡。
		// 记录日志以便在实际情况中可以调试。
		slog.Debug("feishu send: media-only message dropped (image upload not implemented)",
			"account", l.accountID, "chat", msg.ChatID)
		return nil
	}
	tok, err := l.tenantAccessToken(context.Background())
	if err != nil {
		return fmt.Errorf("feishu token: %w", err)
	}
	// 飞书的 `msg_type:"text"` 路径不渲染 markdown——GFM 表格会以
	// 原始 `|cell|cell|` 行到达。先将它们折叠为 label:value 或中点行。
	text := FlattenMarkdownTables(msg.Text)
	contentJSON, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return fmt.Errorf("feishu marshal content: %w", err)
	}
	payload := map[string]string{
		"receive_id": msg.ChatID,
		"content":    string(contentJSON),
		"msg_type":   "text",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("feishu marshal: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost,
		feishuSendURL+"?receive_id_type=chat_id",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("feishu send: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("feishu send HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var apiResp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return fmt.Errorf("feishu send parse: %w", err)
	}
	if apiResp.Code != 0 {
		return fmt.Errorf("feishu send: code=%d msg=%s", apiResp.Code, apiResp.Msg)
	}
	return nil
}

// SendTyping 是空操作。飞书开放平台不暴露自定义 app bot 的输入指示器 API——
// 只有第一方 app 才有。网关的输入中继仍然每 5 秒触发一次，但退化为廉价的空操作调用。
func (l *Feishu) SendTyping(_ string) error { return nil }

// --- 入站（webhook 处理入口）---

// FeishuEventEnvelope 是飞书用于事件订阅的 v2 schema。
// 我们匹配 header.event_type == "im.message.receive_v1"。
type FeishuEventEnvelope struct {
	Schema string            `json:"schema"`
	Header FeishuEventHeader `json:"header"`
	Event  json.RawMessage   `json:"event"`

	// v1 url_verification 挑战字段（也在初始订阅时握手的风头上浮出；
	// 飞书 v2 事件在新 app 上也使用 header.event_type == "url_verification"，
	// 但旧流程仍发送旧版顶层形状）。
	Type      string `json:"type,omitempty"`
	Challenge string `json:"challenge,omitempty"`
	Token     string `json:"token,omitempty"`
}

type FeishuEventHeader struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	CreateTime string `json:"create_time"`
	Token      string `json:"token"`
	AppID      string `json:"app_id"`
	TenantKey  string `json:"tenant_key,omitempty"`
}

type feishuMessageEvent struct {
	Sender struct {
		SenderID struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id,omitempty"`
			UnionID string `json:"union_id,omitempty"`
		} `json:"sender_id"`
		SenderType string `json:"sender_type"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		RootID      string `json:"root_id,omitempty"`
		ParentID    string `json:"parent_id,omitempty"`
		CreateTime  string `json:"create_time"`
		ChatID      string `json:"chat_id"`
		ChatType    string `json:"chat_type"` // "p2p" | "group"
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
	} `json:"message"`
}

// HandleWebhook 由接收飞书 POST 的 HTTP 路由调用。它针对配置的
// 验证令牌验证 `header.token`，处理一次性 URL 验证挑战，
// 并将 im.message.receive_v1 事件分派到总线。
//
// 返回处理程序应写回的 JSON 体和 HTTP 状态码。处理程序故意小而同步，
// 以便单个 goroutine 将一个 webhook 驱动到总线入队完成——飞书在非 200 时
// 重试，因此我们宁愿短暂阻塞也不愿提前确认然后在 panic 时丢弃。
func (l *Feishu) HandleWebhook(body []byte) (responseBody []byte, status int, err error) {
	// 如果加密策略已开启，请求体以 {"encrypt": "<b64>"} 到达，必须在
	// 进一步解析前解密为明文 JSON。通过窥探检测加密形状——具有非空
	// "encrypt" 字段但未配置 encryptKey 的请求体是我们想要暴露的配置
	// 错误（否则飞书只看到不透明的"Challenge code 没有返回"而不知道原因）。
	var peek struct {
		Encrypt string `json:"encrypt"`
	}
	_ = json.Unmarshal(body, &peek)
	if peek.Encrypt != "" {
		if l.encryptKey == "" {
			return nil, http.StatusBadRequest, errors.New("feishu webhook is encrypted but no encryptKey configured (set 加密策略 → Encrypt Key in bkcrab connect dialog, or clear it in feishu console)")
		}
		plain, derr := decryptFeishuPayload(l.encryptKey, peek.Encrypt)
		if derr != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("decrypt: %w", derr)
		}
		body = plain
	}

	var env FeishuEventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, http.StatusBadRequest, fmt.Errorf("parse: %w", err)
	}

	// URL 验证——飞书在订阅配置时发送一次。回显挑战值以使其认为 URL 有效。
	// 两种形状并存：旧版顶层 {type, challenge, token} 和 v2
	// {schema, header.event_type=url_verification, event.{challenge}}。两者都处理。
	if env.Type == "url_verification" || env.Header.EventType == "url_verification" {
		token := env.Token
		if token == "" {
			token = env.Header.Token
		}
		// 未配置验证令牌时安全失败。webhook URL 是公开的；没有共享密钥可以比对，
		// 知道 /api/feishu/webhook/<appId> 的任何人都可以驱动机器人。操作员必须
		// 在飞书开发者控制台设置验证令牌*并*将其粘贴到 bkcrab 连接对话框中。
		// 使用常量时间比较以避免令牌的时序泄漏。
		if l.verificationToken == "" {
			return nil, http.StatusUnauthorized,
				errors.New("feishu webhook rejected: no verification token configured — set it in the Feishu console and bkcrab connect dialog")
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(l.verificationToken)) != 1 {
			return nil, http.StatusUnauthorized, errors.New("verification token mismatch")
		}
		challenge := env.Challenge
		if challenge == "" {
			// v2 形状：challenge 嵌套在 event 中
			var inner struct {
				Challenge string `json:"challenge"`
			}
			_ = json.Unmarshal(env.Event, &inner)
			challenge = inner.Challenge
		}
		out, _ := json.Marshal(map[string]string{"challenge": challenge})
		return out, http.StatusOK, nil
	}

	// 真实事件。验证令牌，然后分派。与上面 url_verification 相同的安全
	// 关闭姿态：未设置的令牌曾意味着"跳过检查"，使公开的 webhook URL
	// 与"任何知道我 app_id 的人都可以在此伪造用户消息"无法区分。
	// 使用常量时间比较以将令牌保持在时序攻击范围之外。
	if l.verificationToken == "" {
		return nil, http.StatusUnauthorized,
			errors.New("feishu webhook rejected: no verification token configured — set it in the Feishu console and bkcrab connect dialog")
	}
	if subtle.ConstantTimeCompare([]byte(env.Header.Token), []byte(l.verificationToken)) != 1 {
		return nil, http.StatusUnauthorized, errors.New("verification token mismatch")
	}

	switch env.Header.EventType {
	case "im.message.receive_v1":
		var ev feishuMessageEvent
		if err := json.Unmarshal(env.Event, &ev); err != nil {
			return nil, http.StatusBadRequest, fmt.Errorf("parse event: %w", err)
		}
		l.dispatchInbound(ev)
	default:
		// 未知 event_type——以 200 确认以免飞书重试，但记录日志
		// 以便错误配置的订阅可见。
		slog.Debug("feishu unhandled event", "event_type", env.Header.EventType, "event_id", env.Header.EventID)
	}
	return []byte(`{"ok":true}`), http.StatusOK, nil
}

// dispatchInbound 将飞书消息事件转换为 bus.InboundMessage。
// 丢弃自身发送的消息（sender_type != "user"）和非文本消息。
// 飞书的 `content` 是事件 JSON 内的 JSON 编码字符串——
// `{"text":"hello"}`——我们需要单独重新解码。
func (l *Feishu) dispatchInbound(ev feishuMessageEvent) {
	if ev.Sender.SenderType != "user" {
		return
	}
	if ev.Message.MessageType != "text" {
		// V1 仅支持文本。飞书的 "post" / "image" / "file" 类型各有自己的
		// 内容形状；推迟直到用户有需求。
		slog.Debug("feishu non-text message skipped",
			"account", l.accountID, "type", ev.Message.MessageType)
		return
	}
	var content struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(ev.Message.Content), &content); err != nil {
		slog.Debug("feishu content parse failed", "error", err)
		return
	}
	if content.Text == "" {
		return
	}

	peerKind := "dm"
	if ev.Message.ChatType == "group" {
		peerKind = "group"
	}

	// 使用稳定的每消息 ID，以便网关去重可以消除重试
	//（飞书在非 2xx 回复时重发事件）。
	msgID := ev.Message.MessageID
	if msgID == "" {
		msgID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}

	slog.Info("feishu message received",
		"account", l.accountID,
		"from", ev.Sender.SenderID.OpenID,
		"chat", ev.Message.ChatID,
		"len", len(content.Text))

	l.bus.Inbound <- bus.InboundMessage{
		Channel:   "feishu",
		AccountID: l.accountID,
		ChatID:    ev.Message.ChatID,
		UserID:    ev.Sender.SenderID.OpenID,
		MessageID: msgID,
		Text:      content.Text,
		PeerKind:  peerKind,
	}
}

// --- HTTP 管线 ---

// tenantAccessToken 返回缓存的飞书租户令牌，在过期（或即将过期——
// RefreshSkew）时刷新。通过结构体互斥锁限制一次飞行中的刷新；
// 并发调用者等待。
func (l *Feishu) tenantAccessToken(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.accessTok != "" && time.Now().Before(l.accessTokExp.Add(-feishuTokenRefreshSkew)) {
		return l.accessTok, nil
	}
	tok, ttl, err := l.fetchTenantAccessToken(ctx)
	if err != nil {
		return "", err
	}
	l.accessTok = tok
	l.accessTokExp = time.Now().Add(time.Duration(ttl) * time.Second)
	return tok, nil
}

func (l *Feishu) fetchTenantAccessToken(ctx context.Context) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, feishuTokenTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]string{
		"app_id":     l.appID,
		"app_secret": l.appSecret,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, feishuTokenURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("contact feishu: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("feishu token HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	var out struct {
		Code              int    `json:"code"`
		Msg               string `json:"msg"`
		TenantAccessToken string `json:"tenant_access_token"`
		Expire            int    `json:"expire"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return "", 0, fmt.Errorf("feishu token parse: %w", err)
	}
	if out.Code != 0 {
		return "", 0, fmt.Errorf("feishu token: code=%d msg=%s", out.Code, out.Msg)
	}
	if out.TenantAccessToken == "" {
		return "", 0, errors.New("feishu token: empty tenant_access_token")
	}
	if out.Expire == 0 {
		out.Expire = 7200 // 文档默认值
	}
	return out.TenantAccessToken, out.Expire, nil
}

// fetchBotInfo 调用 /bot/v3/info 获取 bot 的显示名称 + open_id。
// 尽力获取；失败不会破坏渠道。
func (l *Feishu) fetchBotInfo(ctx context.Context) (name, openID string, err error) {
	tok, err := l.tenantAccessToken(ctx)
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, feishuTokenTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feishuBotInfoURL, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
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
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Bot  struct {
			ActivateStatus int    `json:"activate_status"`
			AppName        string `json:"app_name"`
			OpenID         string `json:"open_id"`
		} `json:"bot"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", err
	}
	if out.Code != 0 {
		return "", "", fmt.Errorf("code=%d msg=%s", out.Code, out.Msg)
	}
	return out.Bot.AppName, out.Bot.OpenID, nil
}

// FeishuValidateCredentials 是连接处理程序验证步骤：
// 获取 tenant_access_token 以确认 app_id/app_secret 可用，
// 然后获取 /bot/v3/info 以捕获 bot 的显示名称。不创建适配器
// 状态——调用方持久化并热注册。
func FeishuValidateCredentials(ctx context.Context, appID, appSecret string) (botName, botOpenID string, err error) {
	stub := &Feishu{
		appID:      appID,
		appSecret:  appSecret,
		httpClient: &http.Client{Timeout: feishuSendTimeout},
	}
	if _, err := stub.tenantAccessToken(ctx); err != nil {
		return "", "", err
	}
	return stub.fetchBotInfo(ctx)
}

// decryptFeishuPayload 从飞书 webhook 的 `encrypt` 字段解密 base64 编码的
// 密文。方案（根据飞书文档）：
//   - aesKey = sha256(encryptKey)             // 32 字节 → AES-256
//   - raw = base64-decode(b64ciphertext)
//   - iv = raw[:16], ciphertext = raw[16:]
//   - plain = AES-256-CBC-decrypt(ciphertext, aesKey, iv)，PKCS7 去填充
//
// 返回 HandleWebhook 其余部分可作为普通 FeishuEventEnvelope 解组的
// JSON 明文请求体。
func decryptFeishuPayload(encryptKey, b64ciphertext string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64ciphertext)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	if len(raw) < aes.BlockSize {
		return nil, fmt.Errorf("ciphertext too short (%d bytes)", len(raw))
	}
	keySum := sha256.Sum256([]byte(encryptKey))
	block, err := aes.NewCipher(keySum[:])
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	iv, ct := raw[:aes.BlockSize], raw[aes.BlockSize:]
	if len(ct)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not block-aligned (%d bytes)", len(ct))
	}
	plain := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ct)
	// PKCS7 去填充。最后字节 = 填充长度 (1..blockSize)。在修剪前验证
	// 以免格式错误的载荷产生垃圾数据。
	if len(plain) == 0 {
		return nil, errors.New("empty plaintext")
	}
	pad := int(plain[len(plain)-1])
	if pad < 1 || pad > aes.BlockSize || pad > len(plain) {
		return nil, fmt.Errorf("bad padding (pad=%d, len=%d)", pad, len(plain))
	}
	for _, b := range plain[len(plain)-pad:] {
		if int(b) != pad {
			return nil, errors.New("bad padding bytes")
		}
	}
	return plain[:len(plain)-pad], nil
}
