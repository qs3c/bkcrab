package channels

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"github.com/qs3c/bkcrab/internal/bus"
)

var mentionRe = regexp.MustCompile(`@(\w+)`)

// markdownV2Escaper 为 Telegram MarkdownV2 转义特殊字符。
var markdownV2SpecialChars = []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}

// Telegram 实现了 Telegram Bot API 的 Channel 接口。
type Telegram struct {
	bot         *tgbotapi.BotAPI
	bus         *bus.MessageBus
	accountID   string
	botUsername string
}

// NewTelegram 为给定账号创建新的 Telegram 渠道实例。
func NewTelegram(botToken string, accountID string, mb *bus.MessageBus) (*Telegram, error) {
	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return nil, fmt.Errorf("create telegram bot: %w", err)
	}

	slog.Info("telegram bot authorized", "username", bot.Self.UserName, "account", accountID)

	return &Telegram{
		bot:         bot,
		bus:         mb,
		accountID:   accountID,
		botUsername: bot.Self.UserName,
	}, nil
}

func (t *Telegram) Name() string {
	return "telegram"
}

func (t *Telegram) AccountID() string {
	return t.accountID
}

// BotUsername 返回 Telegram bot 的用户名（不带 @）。
func (t *Telegram) BotUsername() string {
	return t.botUsername
}

// Start 开始长轮询 Telegram 更新。
func (t *Telegram) Start(ctx context.Context) error {
	// 注册 bot 命令以便用户在 / 菜单中看到它们
	t.registerCommands()

	// 在轮询之前回收 bot，以便残留的 webhook 或前一个持有者
	// 进行中的 getUpdates 不会将我们锁定在 tgbotapi 内部的
	// 3 秒重试垃圾循环中。
	t.claimBot()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := t.bot.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			t.bot.StopReceivingUpdates()
			return nil
		case update := <-updates:
			t.handleUpdate(update)
		}
	}
}

func (t *Telegram) handleUpdate(update tgbotapi.Update) {
	// 处理回调查询（内联键盘按钮点击）
	if update.CallbackQuery != nil {
		t.handleCallbackQuery(update.CallbackQuery)
		return
	}

	// 处理编辑消息 - 视同新消息
	msg := update.Message
	if msg == nil {
		msg = update.EditedMessage
	}
	if msg == nil {
		return
	}

	// 构建入站消息
	inbound := t.buildInboundMessage(msg)
	if inbound == nil {
		return
	}

	t.bus.Inbound <- *inbound
}

func (t *Telegram) buildInboundMessage(msg *tgbotapi.Message) *bus.InboundMessage {
	// 处理照片
	var photoURL string
	if msg.Photo != nil && len(msg.Photo) > 0 {
		// 使用最大照片（数组中最后一个）
		largest := msg.Photo[len(msg.Photo)-1]
		fileURL, err := t.bot.GetFileDirectURL(largest.FileID)
		if err != nil {
			slog.Warn("telegram get photo URL", "error", err)
		} else {
			photoURL = fileURL
		}
	}

	// 跳过没有文本和照片的消息
	text := msg.Text
	if msg.Caption != "" {
		text = msg.Caption
	}
	if text == "" && photoURL == "" {
		// 不支持的消息类型（贴纸、语音等）- 跳过
		slog.Debug("telegram skipping unsupported message type",
			"chat_id", msg.Chat.ID,
			"from", msg.From.UserName,
		)
		return nil
	}

	peerKind := "dm"
	if msg.Chat.IsGroup() || msg.Chat.IsSuperGroup() {
		peerKind = "group"
	}

	senderName := msg.From.UserName
	if senderName == "" {
		senderName = msg.From.FirstName
	}

	// 从消息文本中解析 @提及
	var mentions []string
	matches := mentionRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		mentions = append(mentions, m[1])
	}

	isBot := msg.From.IsBot

	// 跟踪回复
	var replyToMsgID string
	if msg.ReplyToMessage != nil {
		replyToMsgID = strconv.Itoa(msg.ReplyToMessage.MessageID)
	}

	slog.Info("telegram message received",
		"from", senderName,
		"chat_id", msg.Chat.ID,
		"account", t.accountID,
		"peer_kind", peerKind,
		"is_bot", isBot,
		"mentions", mentions,
		"has_photo", photoURL != "",
	)

	return &bus.InboundMessage{
		Channel:      "telegram",
		AccountID:    t.accountID,
		ChatID:       strconv.FormatInt(msg.Chat.ID, 10),
		UserID:       strconv.FormatInt(msg.From.ID, 10),
		MessageID:    strconv.Itoa(msg.MessageID),
		Text:         text,
		PeerKind:     peerKind,
		SenderName:   senderName,
		Mentions:     mentions,
		IsBotMessage: isBot,
		PhotoURL:     photoURL,
		ReplyToMsgID: replyToMsgID,
	}
}

func (t *Telegram) handleCallbackQuery(cq *tgbotapi.CallbackQuery) {
	// 确认回调
	callback := tgbotapi.NewCallback(cq.ID, "")
	if _, err := t.bot.Request(callback); err != nil {
		slog.Warn("telegram callback ack failed", "error", err)
	}

	if cq.Message == nil || cq.Data == "" {
		return
	}

	peerKind := "dm"
	if cq.Message.Chat.IsGroup() || cq.Message.Chat.IsSuperGroup() {
		peerKind = "group"
	}

	senderName := cq.From.UserName
	if senderName == "" {
		senderName = cq.From.FirstName
	}

	t.bus.Inbound <- bus.InboundMessage{
		Channel:      "telegram",
		AccountID:    t.accountID,
		ChatID:       strconv.FormatInt(cq.Message.Chat.ID, 10),
		UserID:       strconv.FormatInt(cq.From.ID, 10),
		MessageID:    strconv.Itoa(cq.Message.MessageID),
		Text:         cq.Data,
		PeerKind:     peerKind,
		SenderName:   senderName,
		IsBotMessage: false,
	}
}

// claimBot 清除残留的 webhook 并从任何前一个 getUpdates 持有者
// 窃取长轮询锁，以便我们以干净状态进入轮询循环。Telegram 最多
// 允许一个客户端同时长轮询一个 bot——发出新的 getUpdates 会终止
// 对端进行中的请求。我们使用 offset=-1, timeout=0 使其立即返回，
// 且不消耗真实更新。
func (t *Telegram) claimBot() {
	if _, err := t.bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false}); err != nil {
		slog.Warn("telegram delete webhook on startup", "account", t.accountID, "error", err)
	}
	if _, err := t.bot.GetUpdates(tgbotapi.UpdateConfig{Offset: -1, Timeout: 0, Limit: 1}); err != nil {
		slog.Warn("telegram claim long-poll lock", "account", t.accountID, "error", err)
	}
}

// registerCommands 设置用户可见的 bot 命令菜单。
func (t *Telegram) registerCommands() {
	commands := []tgbotapi.BotCommand{
		{Command: "start", Description: "Start the bot"},
		{Command: "new", Description: "Start a new conversation"},
		{Command: "retry", Description: "Re-run the last message"},
		{Command: "undo", Description: "Undo the last turn"},
		{Command: "compact", Description: "Compress context window"},
		{Command: "status", Description: "Show agent status"},
		{Command: "usage", Description: "Session turn & token stats"},
		{Command: "insights", Description: "Activity insights (last 7 days)"},
		{Command: "personality", Description: "List or switch personality"},
		{Command: "model", Description: "Switch LLM model"},
		{Command: "help", Description: "Show available commands"},
		{Command: "version", Description: "Show version"},
		{Command: "whoami", Description: "Show your platform user ID"},
	}
	cfg := tgbotapi.NewSetMyCommands(commands...)
	if _, err := t.bot.Request(cfg); err != nil {
		slog.Warn("failed to set bot commands", "error", err)
	} else {
		slog.Info("registered bot commands", "account", t.accountID, "count", len(commands))
	}
}

// Send 向 Telegram 聊天发送纯文本消息。
func (t *Telegram) Send(chatID string, text string) error {
	return t.SendMessage(bus.OutboundMessage{
		ChatID: chatID,
		Text:   text,
	})
}

// SendMessage 发送带格式、回复、按钮等功能的富出站消息。
func (t *Telegram) SendMessage(msg bus.OutboundMessage) error {
	id, err := strconv.ParseInt(msg.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse chat ID: %w", err)
	}

	// 编辑现有消息
	if msg.EditMsgID != "" {
		return t.editMessage(id, msg)
	}

	// 默认使用旧版 Markdown——Telegram 的 MarkdownV2 过于严格
	// （每个特殊字符都需要转义），且我们的 agent 输出标准 GFM。
	// 旧版 "Markdown" 解析模式渲染 *粗体*、_斜体_、`代码`、
	// ```围栏代码```和[链接](url)，无需我们转义每个大括号/方括号。
	// 标题和表格在两种模式下都不渲染，因此我们在发送前去除 ###/## 前缀。
	// 调用方仍可通过 msg.ParseMode 覆盖。
	if msg.ParseMode == "" {
		msg.ParseMode = "Markdown"
	}
	// 先展平 GFM 表格——Telegram（任何解析模式）将 `|cell|cell|` 行
	// 渲染为纯文本。FlattenMarkdownTables 将每行转换为 "label: value" 行，
	// 读起来像普通散文。
	text := FlattenMarkdownTables(msg.Text)
	body := convertMarkdownForTelegram(text, msg.ParseMode)

	// 首先发送文本主体（如果较长则分块）。
	if body != "" {
		chunks := splitTelegramMessage(body)
		for i, chunk := range chunks {
			if err := t.sendSingleMessage(id, chunk, msg, i == 0); err != nil {
				slog.Warn("telegram send chunk failed", "i", i, "error", err)
			}
			if i < len(chunks)-1 {
				time.Sleep(100 * time.Millisecond)
			}
		}
	}

	// 然后上传任何预解析的附件（图片工具输出等）。
	// Telegram 有四个不同的媒体 API，正确的那个由 MediaItem.ContentType
	//（由网关设置）或文件名扩展选择。通过 NewPhoto 发送 PDF/MP3/MP4——
	// 旧代码无条件这样做——会被拒绝并报 PHOTO_INVALID_DIMENSIONS 或类似错误。
	for _, item := range msg.MediaItems {
		fb := tgbotapi.FileBytes{Name: item.Filename, Bytes: item.Bytes}
		var c tgbotapi.Chattable
		switch telegramMediaKind(item) {
		case "photo":
			c = tgbotapi.NewPhoto(id, fb)
		case "video":
			c = tgbotapi.NewVideo(id, fb)
		case "audio":
			c = tgbotapi.NewAudio(id, fb)
		default: // "document"
			c = tgbotapi.NewDocument(id, fb)
		}
		if _, err := t.bot.Send(c); err != nil {
			slog.Warn("telegram media upload failed",
				"filename", item.Filename, "content_type", item.ContentType, "error", err)
		}
	}
	return nil
}

// telegramMediaKind 为 MediaItem 选择 photo / video / audio / document。
// 优先级：显式 ContentType（由网关从文件的 mime/ext 设置）→ 文件名
// 扩展名查找 → "document" 作为安全回退。Telegram 对文档上传很宽松
// （任何文件都可以），所以我们无法有把握分类的内容仍然通过而非丢弃。
func telegramMediaKind(item bus.MediaItem) string {
	ct := strings.ToLower(item.ContentType)
	if ct == "" {
		ct = strings.ToLower(mime.TypeByExtension(filepath.Ext(item.Filename)))
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "photo"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case strings.HasPrefix(ct, "audio/"):
		return "audio"
	}
	return "document"
}

// convertMarkdownForTelegram 对 GFM 文本做轻量级处理，使旧版
// `Markdown` 解析模式至少渲染有用的内容：
//   - `### 标题` / `## 标题` / `# 标题` → `*标题*`（粗体）
//   - `**粗体**` → `*粗体*`（旧版模式使用单个星号）
//   - 表格和其他仅 GFM 语法直接不变通过（Telegram 只显示为纯文本）
//
// MarkdownV2 调用方稍后会应用现有的转义器。
func convertMarkdownForTelegram(text, mode string) string {
	if text == "" {
		return text
	}
	if mode == "MarkdownV2" {
		// V2 路径：调用方现有的转义器完成工作。但先剥离标题标记，
		// 以免它们变成纯文本 `\#\#\#`。
		text = stripMarkdownHeaders(text)
		text = strings.ReplaceAll(text, "**", "*")
		return text
	}
	if mode != "Markdown" {
		return text
	}
	text = stripMarkdownHeaders(text)
	// 旧版 Markdown 粗体是 `*X*`，不是 `**X**`。转换成对的 `**`。
	text = strings.ReplaceAll(text, "**", "*")
	return text
}

// stripMarkdownHeaders 将以 `### `、`## ` 或 `# ` 开头（允许前导空白）
// 的行改写为粗体行。Telegram 在两种解析模式下都不支持标题；将行加粗是
// 最接近的近似。
func stripMarkdownHeaders(text string) string {
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		trimmed := strings.TrimLeft(ln, " \t")
		for _, prefix := range []string{"### ", "## ", "# "} {
			if strings.HasPrefix(trimmed, prefix) {
				rest := strings.TrimPrefix(trimmed, prefix)
				lines[i] = "*" + rest + "*"
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func (t *Telegram) sendSingleMessage(chatID int64, text string, msg bus.OutboundMessage, isFirst bool) error {
	tgMsg := tgbotapi.NewMessage(chatID, text)

	// 设置解析模式并带回退
	if msg.ParseMode != "" {
		if msg.ParseMode == "MarkdownV2" {
			tgMsg.Text = escapeMarkdownV2(text)
		}
		tgMsg.ParseMode = msg.ParseMode
	}

	// 仅回复（仅在第一个块上）
	if isFirst && msg.ReplyToMsgID != "" {
		replyID, err := strconv.Atoi(msg.ReplyToMsgID)
		if err == nil {
			tgMsg.ReplyToMessageID = replyID
		}
	}

	// 内联键盘（仅在最后一个块上，但对于单条消息设置在第一个块上）
	if isFirst && len(msg.Buttons) > 0 {
		tgMsg.ReplyMarkup = buildInlineKeyboard(msg.Buttons)
	}

	_, err := t.bot.Send(tgMsg)
	if err != nil && msg.ParseMode == "MarkdownV2" {
		// 回退到 HTML
		slog.Warn("telegram MarkdownV2 failed, trying HTML", "error", err)
		tgMsg.ParseMode = "HTML"
		tgMsg.Text = text // 使用原始文本进行 HTML
		_, err = t.bot.Send(tgMsg)
		if err != nil {
			// 回退到纯文本
			slog.Warn("telegram HTML failed, sending plain", "error", err)
			tgMsg.ParseMode = ""
			tgMsg.Text = text
			_, err = t.bot.Send(tgMsg)
		}
	}
	return err
}

func (t *Telegram) editMessage(chatID int64, msg bus.OutboundMessage) error {
	editMsgID, err := strconv.Atoi(msg.EditMsgID)
	if err != nil {
		return fmt.Errorf("parse edit message ID: %w", err)
	}

	edit := tgbotapi.NewEditMessageText(chatID, editMsgID, msg.Text)
	if msg.ParseMode != "" {
		if msg.ParseMode == "MarkdownV2" {
			edit.Text = escapeMarkdownV2(msg.Text)
		}
		edit.ParseMode = msg.ParseMode
	}

	if len(msg.Buttons) > 0 {
		kb := buildInlineKeyboard(msg.Buttons)
		edit.ReplyMarkup = &kb
	}

	_, err = t.bot.Send(edit)
	if err != nil && msg.ParseMode == "MarkdownV2" {
		// 然后回退到 HTML 再到纯文本
		edit.ParseMode = "HTML"
		edit.Text = msg.Text
		_, err = t.bot.Send(edit)
		if err != nil {
			edit.ParseMode = ""
			edit.Text = msg.Text
			_, err = t.bot.Send(edit)
		}
	}
	return err
}

// SendTyping 向聊天发送输入指示器。
func (t *Telegram) SendTyping(chatID string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse chat ID: %w", err)
	}
	action := tgbotapi.NewChatAction(id, tgbotapi.ChatTyping)
	_, err = t.bot.Send(action)
	return err
}

// escapeMarkdownV2 为 Telegram MarkdownV2 格式转义特殊字符。
func escapeMarkdownV2(text string) string {
	for _, ch := range markdownV2SpecialChars {
		text = strings.ReplaceAll(text, ch, "\\"+ch)
	}
	return text
}

// splitTelegramMessage 在超过 Telegram 4096 字符限制时按段落边界拆分消息。
func splitTelegramMessage(text string) []string {
	const maxLen = 4096

	if len(text) <= maxLen {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= maxLen {
			chunks = append(chunks, text)
			break
		}

		// 尝试在段落边界处拆分
		cutAt := maxLen
		if idx := strings.LastIndex(text[:maxLen], "\n\n"); idx > 0 {
			cutAt = idx + 2
		} else if idx := strings.LastIndex(text[:maxLen], "\n"); idx > 0 {
			cutAt = idx + 1
		}

		chunks = append(chunks, text[:cutAt])
		text = text[cutAt:]
	}
	return chunks
}

// buildInlineKeyboard 将 OutboundButton 行转换为 Telegram InlineKeyboardMarkup。
func buildInlineKeyboard(buttons [][]bus.OutboundButton) tgbotapi.InlineKeyboardMarkup {
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, row := range buttons {
		var tgRow []tgbotapi.InlineKeyboardButton
		for _, btn := range row {
			if btn.URL != "" {
				tgRow = append(tgRow, tgbotapi.NewInlineKeyboardButtonURL(btn.Text, btn.URL))
			} else {
				tgRow = append(tgRow, tgbotapi.NewInlineKeyboardButtonData(btn.Text, btn.CallbackData))
			}
		}
		if len(tgRow) > 0 {
			rows = append(rows, tgRow)
		}
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}
