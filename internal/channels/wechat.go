package channels

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
)

// WeChat 实现了 iLink（微信）bot 平台的 Channel 接口。
// 模式镜像 telegram.go：单一文件拥有 HTTP 客户端、长轮询循环和出站发送。
// 我们故意不导入更高级的库——将协议面保持在项目内使其易于与 bkcrab
// 自身的消息类型一起演进，并避免对树外项目的 Go 模块依赖。

// iLink 协议常量。匹配上游微信 bot API。
const (
	wechatDefaultBaseURL    = "https://ilinkai.weixin.qq.com"
	wechatLongPollTimeout   = 35 * time.Second
	wechatSendTimeout       = 15 * time.Second
	wechatErrSessionExpired = -14 // 服务端同步令牌已轮换；重置并重新轮询

	wechatMsgTypeUser = 1
	wechatMsgTypeBot  = 2

	wechatMsgStateFinish = 2

	wechatItemTypeText  = 1
	wechatItemTypeImage = 2
	wechatItemTypeVoice = 3
	wechatItemTypeFile  = 4
	wechatItemTypeVideo = 5

	wechatBackoffInitial = 3 * time.Second
	wechatBackoffMax     = 60 * time.Second

	// /ilink/bot/sendtyping 状态值。
	wechatTypingStatusTyping = 1
	wechatTypingStatusCancel = 2

	wechatTypingTimeout = 8 * time.Second

	// 图片/文件上传的 CDN 常量。镜像上游 weclaw 守护进程：
	// iLink 通过 /ilink/bot/getuploadurl 生成一次性上传 URL，
	// bot 用 AES-128-ECB 加密字节后 POST 密文到 CDN，
	// 并获取返回的 X-Encrypted-Param 头，该头被输入到
	// ImageItem.media.encrypt_query_param 中。
	wechatCDNBaseURL        = "https://novac2c.cdn.weixin.qq.com/c2c"
	wechatCDNMediaTypeImage = 1
	wechatCDNMediaTypeVideo = 2
	wechatCDNMediaTypeFile  = 3
	wechatCDNEncryptType    = 1 // AES-128-ECB

	// 媒体发送超时。覆盖 getuploadurl 往返 + CDN POST + 第二次 sendmessage。
	// 比我们 chatSendTimeout 长，因为 CDN 环节对较大图片可能较慢。
	wechatMediaSendTimeout = 90 * time.Second

	// 连续空 buf SessionExpired 响应的阈值，超过后我们声明 bot token 已死
	// 并触发 onExpired。iLink 在提供的 get_updates_buf 缺失或过期时返回
	// SessionExpired——包括合法的"刚重新扫描账号尚未收到第一条消息"的情况。
	// 将第一次出现视为终端会导致每次健康账号重启时被清除（我们以前就是这样做的）。
	// 结合 calcBackoff 在 wechatBackoffMax（通常 60 秒）处封顶，20 次连续失败
	// 大约给出 15-20 分钟的重试时间——对于刚重新扫描的 bot 来说足够长以收到
	// 第一条消息并毕业到真实 buf，对于真正被撤销的 token 来说足够短以不会永远循环。
	wechatEmptyBufExpiredThreshold = 20
)

// WeChat 是一个已登录微信 bot 的 iLink 长轮询适配器。
type WeChat struct {
	bus       *bus.MessageBus
	accountID string // ilink_bot_id，路由用来查找所有者

	// HTTP 凭据（QR 确认时一次性，持久化在 configs 中）：
	botToken    string
	baseURL     string
	ilinkUserID string

	httpClient *http.Client
	wechatUIN  string // 每进程随机生成；iLink 希望一个稳定的头

	// 长轮询游标。iLink 的 `get_updates_buf` 每轮递增，持久化到
	// `bufPath` 路径的磁盘上，以便进程重启不会以空 buf 轮询
	//（iLink 会用 SessionExpired 回应空 buf，旧代码误认为
	// "bot token 已死"——参见 wechatEmptyBufExpiredThreshold 的
	// 对应软启发式处理）。
	getUpdatesBuf string
	bufPath       string
	failures      int

	// emptyBufExpiredCount 统计连续返回 SessionExpired 且 get_updates_buf 已为空的响应次数。
	// 在达到 wechatEmptyBufExpiredThreshold 之前不宣布 bot token 死亡，
	// 这样合法的"新进程 / buf 文件丢失"的首次调用不会被误判为永久过期。
	// 任何成功响应时重置为 0。
	emptyBufExpiredCount int

	// 每聊天 ContextToken 缓存。生成 typing_ticket 的 /ilink/bot/getconfig 调用
	// 需要用户入站消息中最新的 context_token；SendTyping(chatID) 不会提供，
	// 因此我们记住每个入站中最新的 token，并在返回时使用。
	// 允许空字符串（getconfig 将其视为可选）——缓存是尽力而为，非硬性前提。
	ctxTokensMu sync.Mutex
	ctxTokens   map[string]string

	// onExpired 在 iLink 服务端确认 bot token 已死亡时触发一次（操作员必须重新扫码）。
	// 由网关设置，以便可以禁用配置行并注销适配器；否则循环会永远每隔 5 秒记录一次相同警告。
	onExpired func(accountID string)
}

// SetOnExpired 注册一个回调，在 bot token 被确认死亡时触发。
// 回调运行一次；之后 Start 退出。
func (w *WeChat) SetOnExpired(fn func(accountID string)) {
	w.onExpired = fn
}

// NewWeChat 从已连接账号的存储凭据创建新的微信渠道适配器。
func NewWeChat(botToken, baseURL, ilinkUserID, accountID string, mb *bus.MessageBus) (*WeChat, error) {
	if botToken == "" || accountID == "" {
		return nil, fmt.Errorf("wechat: botToken and accountID required")
	}
	if baseURL == "" {
		baseURL = wechatDefaultBaseURL
	}
	slog.Info("wechat bot authorized", "account", accountID)
	return &WeChat{
		bus:         mb,
		accountID:   accountID,
		botToken:    botToken,
		baseURL:     baseURL,
		ilinkUserID: ilinkUserID,
		httpClient:  &http.Client{},
		wechatUIN:   wechatGenerateUIN(),
		ctxTokens:   make(map[string]string),
		bufPath:     wechatBufPath(accountID),
	}, nil
}

// wechatBufPath 返回此账号持久化 get_updates_buf 在磁盘上的位置。
// AccountID 包含 `@`（例如 `4090de018d12@im.bot`），在我们所支持的每个
// 操作系统上都是文件系统安全的，但以防 iLink 传回路径分隔符，我们防御性地
// 替换它们。HomeDir() 失败时返回 ""——调用方将其视为"持久化已禁用，回退到进程内状态"。
func wechatBufPath(accountID string) string {
	home, err := config.HomeDir()
	if err != nil || home == "" {
		return ""
	}
	safe := strings.ReplaceAll(accountID, "/", "_")
	safe = strings.ReplaceAll(safe, string(os.PathSeparator), "_")
	return filepath.Join(home, "state", "wechat", safe+".json")
}

// loadBuf 从磁盘加载 getUpdatesBuf。文件不存在→空操作
//（此账号首次运行，或状态目录被清空）。文件损坏→记录日志并忽略
//（我们只会从 "" 同步一次，这没问题——宽松的过期阈值防止单次空 buf 回复清除账号）。
func (w *WeChat) loadBuf() {
	if w.bufPath == "" {
		return
	}
	data, err := os.ReadFile(w.bufPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("wechat loadBuf failed",
				"account", w.accountID, "path", w.bufPath, "error", err)
		}
		return
	}
	var s struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		slog.Warn("wechat loadBuf parse failed — discarding",
			"account", w.accountID, "path", w.bufPath, "error", err)
		return
	}
	w.getUpdatesBuf = s.GetUpdatesBuf
	if s.GetUpdatesBuf != "" {
		slog.Info("wechat loaded persisted sync buf",
			"account", w.accountID, "path", w.bufPath)
	}
}

// saveBuf 将当前的 getUpdatesBuf 写入磁盘。尽力而为：错误会被记录但不会中止轮询循环——
// 丢失 buf 只会在下次启动时多一次全新同步，宽松的过期阈值防止其触发清除。
func (w *WeChat) saveBuf() {
	if w.bufPath == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(w.bufPath), 0o700); err != nil {
		slog.Warn("wechat saveBuf mkdir failed",
			"account", w.accountID, "path", w.bufPath, "error", err)
		return
	}
	data, _ := json.Marshal(struct {
		GetUpdatesBuf string `json:"get_updates_buf"`
	}{GetUpdatesBuf: w.getUpdatesBuf})
	if err := os.WriteFile(w.bufPath, data, 0o600); err != nil {
		slog.Warn("wechat saveBuf write failed",
			"account", w.accountID, "path", w.bufPath, "error", err)
	}
}

// clearBuf 移除磁盘上的 buf 文件。在宣布 token 死亡后调用，
// 以便手动重新扫码并重新标记的账号不会在下次进程启动时继承已死会话的过期游标。
func (w *WeChat) clearBuf() {
	if w.bufPath == "" {
		return
	}
	if err := os.Remove(w.bufPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("wechat clearBuf failed",
			"account", w.accountID, "path", w.bufPath, "error", err)
	}
}

func (w *WeChat) Name() string        { return "wechat" }
func (w *WeChat) AccountID() string   { return w.accountID }
func (w *WeChat) BotUsername() string { return w.accountID }

// Start 运行长轮询循环直到 ctx 被取消。镜像上游 weclaw monitor 的重试/会话恢复语义：
//   - 任何 GetUpdates 错误 → 指数退避最长 60 秒
//   - errcode -14（会话过期）→ 重置同步 buf 并重试；如果同步 buf 已为空，则 bot token 本身已死亡
//     （操作员需要重新扫码）。
func (w *WeChat) Start(ctx context.Context) error {
	w.loadBuf()
	slog.Info("wechat long-poll loop starting",
		"account", w.accountID, "buf_present", w.getUpdatesBuf != "")
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		resp, err := w.getUpdates(ctx, w.getUpdatesBuf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			w.failures++
			backoff := w.calcBackoff()
			slog.Warn("wechat getUpdates error",
				"account", w.accountID, "failures", w.failures, "backoff", backoff, "error", err)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil
			}
			continue
		}
		w.failures = 0

		if resp.ErrCode == wechatErrSessionExpired {
			if w.getUpdatesBuf != "" {
				slog.Info("wechat session expired, resetting sync buf", "account", w.accountID)
				w.getUpdatesBuf = ""
				w.saveBuf()
				select {
				case <-time.After(5 * time.Second):
				case <-ctx.Done():
					return nil
				}
				continue
			}
			// Sync buf already empty. This is ambiguous — it could mean
			// "first poll after restart, server doesn't know us yet" or
			// "bot token has been revoked." Treating the first occurrence
			// as terminal (the old behavior) caused every restart of a
			// healthy account to purge itself before iLink had a chance
			// to mint a fresh buf. Mirror upstream weclaw: keep retrying
			// with exponential backoff, and only declare the token dead
			// after wechatEmptyBufExpiredThreshold consecutive failures.
			w.emptyBufExpiredCount++
			if w.emptyBufExpiredCount < wechatEmptyBufExpiredThreshold {
				// Only the first attempt warns — subsequent retries log
				// at Debug so a slow-to-warm-up account doesn't fill the
				// log with N copies of the same message before either
				// recovering (counter resets, see below) or hitting
				// threshold (which logs its own terminal Warn).
				if w.emptyBufExpiredCount == 1 {
					slog.Warn("wechat session expired with empty buf — will retry up to threshold",
						"account", w.accountID,
						"threshold", wechatEmptyBufExpiredThreshold)
				} else {
					slog.Debug("wechat session expired with empty buf — retrying",
						"account", w.accountID,
						"attempt", w.emptyBufExpiredCount,
						"threshold", wechatEmptyBufExpiredThreshold)
				}
				w.failures = w.emptyBufExpiredCount
				backoff := w.calcBackoff()
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return nil
				}
				continue
			}
			// Threshold tripped: token is dead for real. Wipe the on-disk
			// buf so a freshly-rescanned account doesn't inherit the
			// stale cursor on the next process start, then fire onExpired
			// (the gateway disables the configs row + unregisters us)
			// and exit.
			slog.Warn("wechat bot token expired — user must rescan QR",
				"account", w.accountID, "attempts", w.emptyBufExpiredCount)
			w.clearBuf()
			if w.onExpired != nil {
				w.onExpired(w.accountID)
			}
			return nil
		}
		if resp.Ret != 0 && resp.ErrCode != 0 {
			slog.Warn("wechat server error",
				"account", w.accountID, "ret", resp.Ret, "errcode", resp.ErrCode, "errmsg", resp.ErrMsg)
			continue
		}
		// Any non-SessionExpired success resets the empty-buf counter —
		// we got a real response from the server, the token is alive.
		if w.emptyBufExpiredCount > 0 {
			slog.Info("wechat session recovered after empty-buf retries",
				"account", w.accountID, "attempts", w.emptyBufExpiredCount)
			w.emptyBufExpiredCount = 0
		}
		if resp.GetUpdatesBuf != "" && resp.GetUpdatesBuf != w.getUpdatesBuf {
			w.getUpdatesBuf = resp.GetUpdatesBuf
			w.saveBuf()
		}
		for _, m := range resp.Msgs {
			w.dispatchInbound(m)
		}
	}
}

// dispatchInbound 将 iLink 消息展平为 bus.InboundMessage。
// 过滤规则：
//   - 丢弃来自 bot 自身的消息（MessageType=2）。它们是我们自己发送的回声。
//   - 丢弃正在进行的流式消息（MessageState != finish）；
//     iLink 在语音转录期间发送部分增量，我们只想要最终结果。
//   - 文本 + 图片被展示；语音以 iLink 已提供的语音转文字转录展示；
//     视频/文件被丢弃（我们尚未支持下载/解密——添加需要 AES-128-ECB CDN 处理，已推迟）。
func (w *WeChat) dispatchInbound(m wechatMessage) {
	if m.MessageType != wechatMsgTypeUser {
		return
	}
	if m.MessageState != wechatMsgStateFinish {
		return
	}

	var text string
	for _, item := range m.ItemList {
		switch item.Type {
		case wechatItemTypeText:
			if item.TextItem != nil && item.TextItem.Text != "" {
				text = item.TextItem.Text
			}
		case wechatItemTypeVoice:
			// iLink ships speech-to-text transcription alongside the
			// audio bytes — use it directly so the agent sees the
			// user's spoken request as text without us having to
			// download + transcribe ourselves.
			if item.VoiceItem != nil && item.VoiceItem.Text != "" {
				text = item.VoiceItem.Text
			}
		}
		if text != "" {
			break
		}
	}
	if text == "" {
		slog.Debug("wechat skipping unsupported message",
			"account", w.accountID, "from", m.FromUserID, "items", len(m.ItemList))
		return
	}

	// iLink doesn't distinguish DM vs group at the protocol level the
	// way Telegram does — every message has a from_user_id and a
	// to_user_id (the bot). Treat all as DM for now; group support
	// would require parsing room_id which the current iLink response
	// shape doesn't expose.
	slog.Info("wechat message received",
		"account", w.accountID, "from", m.FromUserID, "len", len(text))

	// Remember this user's most recent ContextToken so a subsequent
	// SendTyping(chatID) can mint a typing_ticket without round-trip-
	// owning the original message. Cache is per-chat; we just overwrite
	// — the freshest token is the most likely to validate.
	if m.FromUserID != "" {
		w.ctxTokensMu.Lock()
		w.ctxTokens[m.FromUserID] = m.ContextToken
		w.ctxTokensMu.Unlock()
	}

	w.bus.Inbound <- bus.InboundMessage{
		Channel:   "wechat",
		AccountID: w.accountID,
		ChatID:    m.FromUserID, // 1:1 — sender is also the chat key
		UserID:    m.FromUserID,
		MessageID: strconv.FormatInt(m.MessageID, 10),
		Text:      text,
		PeerKind:  "dm",
	}
}

// Send 发送纯文本消息——简单形式。由不需要富格式的工具使用。
func (w *WeChat) Send(chatID, text string) error {
	return w.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

// SendMessage 向 iLink 用户发送回复。iLink 没有原生的 markdown、内联键盘或消息编辑，
// 因此大部分 OutboundMessage 字段被有意忽略——我们只认 Text、ChatID、MediaItems 以及
// 从上一次入站缓存的每聊天 ContextToken（SendTyping 和图片发送路径都使用）。
//
// 文本和图片作为独立的 iLink 消息发送：先发送纯文本 sendmessage（如果有文本），
// 然后每个图片上传到 iLink CDN 后各发送一条 sendmessage。单个图片的失败会被记录
// 但不会中止其余回复——部分投递优于因一次上传失败而丢弃整个轮次。
//
// 多气泡回复：当 agent 发出 SplitMessageMarker 时，文本被拆分为 N 个气泡，
// 每个作为独立的 sendmessage 发送。一个块失败会停止链式发送——部分投递优于
// 静默丢弃后续气泡，但如果 iLink 本身出错，我们不会继续猛打 API。
func (w *WeChat) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text == "" && len(msg.MediaItems) == 0 {
		return nil
	}
	// iLink 需要去除 markdown——客户端渲染纯文本，会直接显示 *bold* / [link](url) 语法。
	// 尽力去除，方式与 weclaw 的 MarkdownToPlainText 辅助函数相同。
	// FlattenMarkdownTables 先运行，使 GFM 表格在 wechatStripMarkdown 丢弃其余 markdown
	// 之前折叠为 "label: value" / 中点行——在之后运行会留下更糟糕的裸 `|cell|cell|` 块。
	// 在 SplitMessageMarker 处拆分发生在调度器层（internal/channels/manager.go: routeOutbound），
	// 因此所有 IM 适配器统一处理——到 SendMessage 被调用时，msg.Text 已经是一个气泡的内容。
	// 当 AllowSplit 关闭时，调度器还将杂散标记折叠为换行，因此我们在这里永远不会看到它们。
	plain := wechatStripMarkdown(FlattenMarkdownTables(msg.Text))
	// 当正文在 markdown 去除后没有可见内容时跳过文本发送——捕获了多气泡拆分产生的块
	// 仅包含空白或 markdown 标点的情况，否则 `sendTextOnly` 会将其作为空白气泡随附件一起发送。
	if strings.TrimSpace(plain) != "" {
		if err := w.sendTextOnly(msg.ChatID, plain); err != nil {
			return err
		}
	}
	for _, item := range msg.MediaItems {
		if len(item.Bytes) == 0 {
			continue
		}
		if err := w.sendMedia(msg.ChatID, item); err != nil {
			slog.Warn("wechat send media failed",
				"account", w.accountID, "chat", msg.ChatID,
				"filename", item.Filename, "error", err)
		}
	}
	return nil
}

// sendTextOnly 是 SendMessage 在有纯文本发送时使用的简单文本消息路径。
// 与 sendImage 保持独立，以便每条路径可以有各自的超时 + 载荷结构。
func (w *WeChat) sendTextOnly(chatID, plain string) error {
	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	body := wechatSendRequest{
		Msg: wechatSendMsg{
			FromUserID:   w.accountID,
			ToUserID:     chatID,
			ClientID:     uuid.NewString(),
			MessageType:  wechatMsgTypeBot,
			MessageState: wechatMsgStateFinish,
			ItemList: []wechatItem{
				{
					Type:     wechatItemTypeText,
					TextItem: &wechatTextItem{Text: plain},
				},
			},
			ContextToken: contextToken,
		},
		BaseInfo: wechatBaseInfo{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), wechatSendTimeout)
	defer cancel()
	var resp wechatSendResponse
	if err := w.doPost(ctx, "/ilink/bot/sendmessage", body, &resp); err != nil {
		return fmt.Errorf("wechat send: %w", err)
	}
	if resp.Ret != 0 {
		return fmt.Errorf("wechat send: ret=%d errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	return nil
}

// SendTyping 在 agent 处理轮次时向用户的微信显示"对方正在输入..."指示器。
// iLink 需要两次调用：
//
//  1. /ilink/bot/getconfig，使用接收者的 ilink_user_id（可选地加上他们最近的 context_token）来生成 typing_ticket；
//  2. /ilink/bot/sendtyping，使用该 ticket 和 status=1。
//
// 网关在轮次持续期间每 5 秒 ping 一次此函数——与 Telegram 的 sendChatAction 相同节奏。
// 错误以 Debug 级别记录并返回，但网关将其视为尽力而为，因此小故障不会导致用户可见的回复失败。
func (w *WeChat) SendTyping(chatID string) error {
	if chatID == "" {
		return nil
	}
	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), wechatTypingTimeout)
	defer cancel()

	cfgBody := wechatGetConfigRequest{
		ILinkUserID:  chatID,
		ContextToken: contextToken,
	}
	var cfgResp wechatGetConfigResponse
	if err := w.doPost(ctx, "/ilink/bot/getconfig", cfgBody, &cfgResp); err != nil {
		slog.Warn("wechat getconfig failed", "account", w.accountID, "chat", chatID, "error", err)
		return fmt.Errorf("wechat getconfig: %w", err)
	}
	if cfgResp.Ret != 0 {
		slog.Warn("wechat getconfig non-zero ret",
			"account", w.accountID, "chat", chatID, "ret", cfgResp.Ret, "errmsg", cfgResp.ErrMsg)
		return fmt.Errorf("wechat getconfig: ret=%d errmsg=%s", cfgResp.Ret, cfgResp.ErrMsg)
	}
	if cfgResp.TypingTicket == "" {
		slog.Info("wechat getconfig returned empty typing_ticket — typing disabled for this account",
			"account", w.accountID, "chat", chatID)
		return nil
	}

	typingBody := wechatSendTypingRequest{
		ILinkUserID:  chatID,
		TypingTicket: cfgResp.TypingTicket,
		Status:       wechatTypingStatusTyping,
	}
	var typingResp wechatSendTypingResponse
	if err := w.doPost(ctx, "/ilink/bot/sendtyping", typingBody, &typingResp); err != nil {
		slog.Warn("wechat sendtyping failed", "account", w.accountID, "chat", chatID, "error", err)
		return fmt.Errorf("wechat sendtyping: %w", err)
	}
	if typingResp.Ret != 0 {
		slog.Warn("wechat sendtyping non-zero ret",
			"account", w.accountID, "chat", chatID, "ret", typingResp.Ret, "errmsg", typingResp.ErrMsg)
		return fmt.Errorf("wechat sendtyping: ret=%d errmsg=%s", typingResp.Ret, typingResp.ErrMsg)
	}
	slog.Debug("wechat typing sent", "account", w.accountID, "chat", chatID)
	return nil
}

// --- HTTP 管线 ---

// getUpdates 是长轮询。服务端保持请求打开最长 `longpolling_timeout_ms`（通常 30 秒），
// 返回待处理消息或空的 Msgs 切片。我们给请求在服务端超时基础上增加 5 秒的松弛时间，
// 以便客户端取消能够与服务端空批次区分开。
func (w *WeChat) getUpdates(ctx context.Context, buf string) (*wechatGetUpdatesResponse, error) {
	body := wechatGetUpdatesRequest{
		GetUpdatesBuf: buf,
		BaseInfo:      wechatBaseInfo{ChannelVersion: "1.0.0"},
	}
	ctx, cancel := context.WithTimeout(ctx, wechatLongPollTimeout+5*time.Second)
	defer cancel()
	var resp wechatGetUpdatesResponse
	if err := w.doPost(ctx, "/ilink/bot/getupdates", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (w *WeChat) doPost(ctx context.Context, path string, body, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.baseURL+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("AuthorizationType", "ilink_bot_token")
	req.Header.Set("Authorization", "Bearer "+w.botToken)
	req.Header.Set("X-WECHAT-UIN", w.wechatUIN)

	resp, err := w.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return json.Unmarshal(respBody, result)
}

func (w *WeChat) calcBackoff() time.Duration {
	d := wechatBackoffInitial
	for i := 1; i < w.failures; i++ {
		d *= 2
		if d > wechatBackoffMax {
			return wechatBackoffMax
		}
	}
	return d
}

// wechatGenerateUIN 生成 iLink 在 X-WECHAT-UIN 头部中需要的随机化 base64 字符串。
// 上游协议将其记录为"每进程大致稳定即可"；我们在适配器构造时生成一次。
func wechatGenerateUIN() string {
	var n uint32
	_ = binary.Read(rand.Reader, binary.LittleEndian, &n)
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%d", n)))
}

// wechatStripMarkdown 是一个小型尽力而为的纯文本转换器，
// 使 LLM 发出的 markdown 不会在微信中显示为原始的 `*foo*` / `### bar`。
// 非完整解析器——我们只处理常见的违规项。
func wechatStripMarkdown(text string) string {
	if text == "" {
		return ""
	}
	out := text
	// 去除行首的 ATX 标题
	for _, prefix := range []string{"### ", "## ", "# "} {
		out = bytesReplaceAtLineStart(out, prefix, "")
	}
	// 粗体/斜体标记——删除标记本身
	out = bytesReplaceAll(out, "**", "")
	out = bytesReplaceAll(out, "__", "")
	// 内联代码反引号——去除
	out = bytesReplaceAll(out, "```", "")
	out = bytesReplaceAll(out, "`", "")
	return out
}

func bytesReplaceAll(s, old, new string) string {
	if old == "" {
		return s
	}
	for {
		i := indexOf(s, old)
		if i < 0 {
			return s
		}
		s = s[:i] + new + s[i+len(old):]
	}
}

func bytesReplaceAtLineStart(s, prefix, replacement string) string {
	if prefix == "" {
		return s
	}
	out := make([]byte, 0, len(s))
	atLineStart := true
	for i := 0; i < len(s); {
		if atLineStart && i+len(prefix) <= len(s) && s[i:i+len(prefix)] == prefix {
			out = append(out, replacement...)
			i += len(prefix)
			atLineStart = false
			continue
		}
		out = append(out, s[i])
		atLineStart = s[i] == '\n'
		i++
	}
	return string(out)
}

func indexOf(s, sub string) int {
	if sub == "" {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// --- 线路类型（iLink 协议结构，对此包保持私有） ---

type wechatBaseInfo struct {
	ChannelVersion string `json:"channel_version,omitempty"`
}

type wechatGetUpdatesRequest struct {
	GetUpdatesBuf string         `json:"get_updates_buf"`
	BaseInfo      wechatBaseInfo `json:"base_info"`
}

type wechatGetUpdatesResponse struct {
	Ret           int             `json:"ret"`
	ErrCode       int             `json:"errcode,omitempty"`
	ErrMsg        string          `json:"errmsg,omitempty"`
	Msgs          []wechatMessage `json:"msgs"`
	GetUpdatesBuf string          `json:"get_updates_buf"`
}

type wechatMessage struct {
	Seq          int          `json:"seq,omitempty"`
	MessageID    int64        `json:"message_id,omitempty"`
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ItemList     []wechatItem `json:"item_list"`
	ContextToken string       `json:"context_token"`
}

type wechatItem struct {
	Type      int              `json:"type"`
	TextItem  *wechatTextItem  `json:"text_item,omitempty"`
	ImageItem *wechatImageItem `json:"image_item,omitempty"`
	VoiceItem *wechatVoiceItem `json:"voice_item,omitempty"`
	VideoItem *wechatVideoItem `json:"video_item,omitempty"`
	FileItem  *wechatFileItem  `json:"file_item,omitempty"`
}

type wechatVideoItem struct {
	Media     *wechatMediaInfo `json:"media,omitempty"`
	VideoSize int              `json:"video_size,omitempty"` // ciphertext size
}

type wechatFileItem struct {
	Media    *wechatMediaInfo `json:"media,omitempty"`
	FileName string           `json:"file_name,omitempty"`
	Len      string           `json:"len,omitempty"` // plaintext size, as a string (iLink quirk)
}

type wechatTextItem struct {
	Text string `json:"text"`
}

type wechatVoiceItem struct {
	Text     string `json:"text,omitempty"`     // STT transcription
	Playtime int    `json:"playtime,omitempty"` // ms
}

type wechatSendRequest struct {
	Msg      wechatSendMsg  `json:"msg"`
	BaseInfo wechatBaseInfo `json:"base_info"`
}

type wechatSendMsg struct {
	FromUserID   string       `json:"from_user_id"`
	ToUserID     string       `json:"to_user_id"`
	ClientID     string       `json:"client_id"`
	MessageType  int          `json:"message_type"`
	MessageState int          `json:"message_state"`
	ItemList     []wechatItem `json:"item_list"`
	ContextToken string       `json:"context_token,omitempty"`
}

type wechatSendResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

type wechatGetConfigRequest struct {
	ILinkUserID  string         `json:"ilink_user_id"`
	ContextToken string         `json:"context_token,omitempty"`
	BaseInfo     wechatBaseInfo `json:"base_info"`
}

type wechatGetConfigResponse struct {
	Ret          int    `json:"ret"`
	ErrMsg       string `json:"errmsg,omitempty"`
	TypingTicket string `json:"typing_ticket,omitempty"`
}

type wechatSendTypingRequest struct {
	ILinkUserID  string         `json:"ilink_user_id"`
	TypingTicket string         `json:"typing_ticket"`
	Status       int            `json:"status"`
	BaseInfo     wechatBaseInfo `json:"base_info"`
}

type wechatSendTypingResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

// --- CDN 图片上传 + 发送（镜像 weclaw/messaging/cdn.go + media.go） ---
//
// iLink 的图片流程分为两步：
//   1. POST /ilink/bot/getuploadurl 生成一次性 CDN 上传 URL
//      （bot 提供随机的 filekey + AES-128 密钥 + 明文 md5；服务端返回完整 URL
//      或仅返回查询参数以附加到已知的 CDN 端点）。
//   2. 将 AES-128-ECB 加密的字节 POST 到该 URL；服务端回复 X-Encrypted-Param 头部，
//      该头部成为最终 sendmessage 的 ImageItem.media.encrypt_query_param。
//
// AES 密钥线路格式是 base64 编码的 *十六进制字符串*（不是原始的 16 字节）——
// iLink 协议的特性，此处保留以与上游守护进程兼容。

type wechatImageItem struct {
	URL     string           `json:"url,omitempty"`
	Media   *wechatMediaInfo `json:"media,omitempty"`
	MidSize int              `json:"mid_size,omitempty"` // ciphertext size
}

type wechatMediaInfo struct {
	EncryptQueryParam string `json:"encrypt_query_param"`
	AESKey            string `json:"aes_key"`      // base64(hex(raw_key))
	EncryptType       int    `json:"encrypt_type"` // 1 = AES-128-ECB
}

type wechatGetUploadURLRequest struct {
	FileKey     string         `json:"filekey"`
	MediaType   int            `json:"media_type"`
	ToUserID    string         `json:"to_user_id"`
	RawSize     int            `json:"rawsize"`
	RawFileMD5  string         `json:"rawfilemd5"`
	FileSize    int            `json:"filesize"`
	NoNeedThumb bool           `json:"no_need_thumb"`
	AESKey      string         `json:"aeskey"`
	BaseInfo    wechatBaseInfo `json:"base_info"`
}

type wechatGetUploadURLResponse struct {
	Ret           int    `json:"ret"`
	ErrMsg        string `json:"errmsg,omitempty"`
	UploadParam   string `json:"upload_param"`
	UploadFullURL string `json:"upload_full_url,omitempty"`
}

// wechatUploadedFile 是上传后的句柄：足够为后续 sendmessage 创建
// MediaInfo 引用（图片/视频/文件）。
type wechatUploadedFile struct {
	DownloadParam string
	AESKeyHex     string
	FileSize      int // 明文大小——FileItem.Len 需要
	CipherSize    int
}

// sendMedia 将 MediaItem 的字节上传到 iLink CDN 并发布引用结果的 sendmessage。
// MediaItem 的 ContentType / Filename 选择线路形状：图片（type=2）、视频（type=5）
// 或其他所有类型为文件（type=4）（包括音频——出站语音项需要我们没有可靠获得的
// 编解码器/采样率元数据，将音频作为文件发送仍然可以在微信中内联播放）。
// 镜像上游 weclaw/messaging/media.go 中的调度器。
func (w *WeChat) sendMedia(chatID string, item bus.MediaItem) error {
	cdnMediaType, itemType := classifyWeChatMedia(item)

	ctx, cancel := context.WithTimeout(context.Background(), wechatMediaSendTimeout)
	defer cancel()

	uploaded, err := w.uploadToCDN(ctx, chatID, item.Bytes, cdnMediaType)
	if err != nil {
		return fmt.Errorf("upload: %w", err)
	}

	w.ctxTokensMu.Lock()
	contextToken := w.ctxTokens[chatID]
	w.ctxTokensMu.Unlock()

	media := &wechatMediaInfo{
		EncryptQueryParam: uploaded.DownloadParam,
		AESKey:            base64.StdEncoding.EncodeToString([]byte(uploaded.AESKeyHex)),
		EncryptType:       wechatCDNEncryptType,
	}

	var sendItem wechatItem
	switch itemType {
	case wechatItemTypeImage:
		sendItem = wechatItem{
			Type: wechatItemTypeImage,
			ImageItem: &wechatImageItem{
				Media:   media,
				MidSize: uploaded.CipherSize,
			},
		}
	case wechatItemTypeVideo:
		sendItem = wechatItem{
			Type: wechatItemTypeVideo,
			VideoItem: &wechatVideoItem{
				Media:     media,
				VideoSize: uploaded.CipherSize,
			},
		}
	default: // wechatItemTypeFile
		fileName := item.Filename
		if fileName == "" {
			fileName = "file"
		}
		sendItem = wechatItem{
			Type: wechatItemTypeFile,
			FileItem: &wechatFileItem{
				Media:    media,
				FileName: fileName,
				Len:      strconv.Itoa(uploaded.FileSize),
			},
		}
	}

	body := wechatSendRequest{
		Msg: wechatSendMsg{
			FromUserID:   w.accountID,
			ToUserID:     chatID,
			ClientID:     uuid.NewString(),
			MessageType:  wechatMsgTypeBot,
			MessageState: wechatMsgStateFinish,
			ItemList:     []wechatItem{sendItem},
			ContextToken: contextToken,
		},
		BaseInfo: wechatBaseInfo{},
	}
	var resp wechatSendResponse
	if err := w.doPost(ctx, "/ilink/bot/sendmessage", body, &resp); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	if resp.Ret != 0 {
		return fmt.Errorf("send: ret=%d errmsg=%s", resp.Ret, resp.ErrMsg)
	}
	slog.Debug("wechat media sent",
		"account", w.accountID, "chat", chatID,
		"filename", item.Filename, "kind", itemType, "bytes", len(item.Bytes))
	return nil
}

// classifyWeChatMedia 决定如何在 iLink 上发送 MediaItem：图片、视频或文件（默认）。
// 优先使用 MediaItem.ContentType（已设置时）；否则从文件扩展名推断。
// 音频归入文件——匹配上游 weclaw 的 classifyMedia 行为。
func classifyWeChatMedia(item bus.MediaItem) (cdnMediaType int, itemType int) {
	ct := strings.ToLower(item.ContentType)
	if ct == "" {
		ct = strings.ToLower(mime.TypeByExtension(filepath.Ext(item.Filename)))
	}
	if strings.HasPrefix(ct, "image/") || isWeChatImageExt(item.Filename) {
		return wechatCDNMediaTypeImage, wechatItemTypeImage
	}
	if strings.HasPrefix(ct, "video/") || isWeChatVideoExt(item.Filename) {
		return wechatCDNMediaTypeVideo, wechatItemTypeVideo
	}
	return wechatCDNMediaTypeFile, wechatItemTypeFile
}

func isWeChatImageExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

func isWeChatVideoExt(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".webm", ".mkv", ".avi":
		return true
	}
	return false
}

// uploadToCDN 处理任何媒体类型的 AES 加密 CDN 上传环节。
// `mediaType` 是 wechatCDNMediaType{Image,Video,File} 之一，
// 决定 iLink 的 CDN 随后如何分类和提供这些字节。
func (w *WeChat) uploadToCDN(ctx context.Context, toUserID string, data []byte, mediaType int) (*wechatUploadedFile, error) {
	filekey := make([]byte, 16)
	aeskey := make([]byte, 16)
	if _, err := rand.Read(filekey); err != nil {
		return nil, fmt.Errorf("filekey: %w", err)
	}
	if _, err := rand.Read(aeskey); err != nil {
		return nil, fmt.Errorf("aeskey: %w", err)
	}
	filekeyHex := hex.EncodeToString(filekey)
	aeskeyHex := hex.EncodeToString(aeskey)

	hash := md5.Sum(data)
	rawMD5 := hex.EncodeToString(hash[:])
	cipherSize := wechatAESECBPaddedSize(len(data))

	upReq := wechatGetUploadURLRequest{
		FileKey:     filekeyHex,
		MediaType:   mediaType,
		ToUserID:    toUserID,
		RawSize:     len(data),
		RawFileMD5:  rawMD5,
		FileSize:    cipherSize,
		NoNeedThumb: true,
		AESKey:      aeskeyHex,
		BaseInfo:    wechatBaseInfo{},
	}
	var upResp wechatGetUploadURLResponse
	if err := w.doPost(ctx, "/ilink/bot/getuploadurl", upReq, &upResp); err != nil {
		return nil, fmt.Errorf("getuploadurl: %w", err)
	}
	if upResp.Ret != 0 {
		return nil, fmt.Errorf("getuploadurl ret=%d errmsg=%s", upResp.Ret, upResp.ErrMsg)
	}

	encrypted, err := wechatAESECBEncrypt(data, aeskey)
	if err != nil {
		return nil, fmt.Errorf("encrypt: %w", err)
	}

	// 服务端可能返回完整上传 URL 或仅返回查询参数；
	// 后者情况下根据已知的 CDN 主机构造 URL。
	cdnURL := strings.TrimSpace(upResp.UploadFullURL)
	if cdnURL == "" {
		if upResp.UploadParam == "" {
			return nil, fmt.Errorf("getuploadurl returned no URL")
		}
		cdnURL = fmt.Sprintf("%s/upload?encrypted_query_param=%s&filekey=%s",
			wechatCDNBaseURL, url.QueryEscape(upResp.UploadParam), url.QueryEscape(filekeyHex))
	}

	downloadParam, err := wechatUploadCDNBytes(ctx, encrypted, cdnURL)
	if err != nil {
		return nil, fmt.Errorf("cdn upload: %w", err)
	}
	return &wechatUploadedFile{
		DownloadParam: downloadParam,
		AESKeyHex:     aeskeyHex,
		FileSize:      len(data),
		CipherSize:    cipherSize,
	}, nil
}

// wechatUploadCDNBytes 将 AES 加密的载荷 POST 到 CDN，
// 并从响应中返回 X-Encrypted-Param 头部——bot 稍后嵌入为 encrypt_query_param 的不透明令牌，
// 以便接收者的微信客户端可以获取并解密。
func wechatUploadCDNBytes(ctx context.Context, encrypted []byte, cdnURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cdnURL, bytes.NewReader(encrypted))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	downloadParam := resp.Header.Get("X-Encrypted-Param")
	if downloadParam == "" {
		return "", fmt.Errorf("missing X-Encrypted-Param header")
	}
	return downloadParam, nil
}

func wechatAESECBEncrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padLen := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := make([]byte, len(plaintext)+padLen)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	encrypted := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(encrypted[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return encrypted, nil
}

func wechatAESECBPaddedSize(plaintextSize int) int {
	return (plaintextSize/aes.BlockSize + 1) * aes.BlockSize
}
