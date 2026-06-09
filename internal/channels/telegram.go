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

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

var mentionRe = regexp.MustCompile(`@(\w+)`)

// markdownV2Escaper escapes special characters for Telegram MarkdownV2.
var markdownV2SpecialChars = []string{"_", "*", "[", "]", "(", ")", "~", "`", ">", "#", "+", "-", "=", "|", "{", "}", ".", "!"}

// Telegram implements the Channel interface for Telegram Bot API.
type Telegram struct {
	bot         *tgbotapi.BotAPI
	bus         *bus.MessageBus
	accountID   string
	botUsername string
}

// NewTelegram creates a new Telegram channel instance for the given account.
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

// BotUsername returns the Telegram bot's username (without @).
func (t *Telegram) BotUsername() string {
	return t.botUsername
}

// Start begins long polling for Telegram updates.
func (t *Telegram) Start(ctx context.Context) error {
	// Register bot commands so users see them in the / menu
	t.registerCommands()

	// Reclaim the bot before polling so a stray webhook or a previous
	// holder's in-flight getUpdates doesn't lock us into the 3s retry
	// spam loop inside tgbotapi.
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
	// Handle callback queries (inline keyboard button presses)
	if update.CallbackQuery != nil {
		t.handleCallbackQuery(update.CallbackQuery)
		return
	}

	// Handle edited messages - treat like new messages
	msg := update.Message
	if msg == nil {
		msg = update.EditedMessage
	}
	if msg == nil {
		return
	}

	// Build inbound message
	inbound := t.buildInboundMessage(msg)
	if inbound == nil {
		return
	}

	t.bus.Inbound <- *inbound
}

func (t *Telegram) buildInboundMessage(msg *tgbotapi.Message) *bus.InboundMessage {
	// Handle photos
	var photoURL string
	if msg.Photo != nil && len(msg.Photo) > 0 {
		// Use the largest photo (last in the array)
		largest := msg.Photo[len(msg.Photo)-1]
		fileURL, err := t.bot.GetFileDirectURL(largest.FileID)
		if err != nil {
			slog.Warn("telegram get photo URL", "error", err)
		} else {
			photoURL = fileURL
		}
	}

	// Skip messages with no text and no photo
	text := msg.Text
	if msg.Caption != "" {
		text = msg.Caption
	}
	if text == "" && photoURL == "" {
		// Unsupported message type (sticker, voice, etc.) - skip
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

	// Parse @mentions from message text
	var mentions []string
	matches := mentionRe.FindAllStringSubmatch(text, -1)
	for _, m := range matches {
		mentions = append(mentions, m[1])
	}

	isBot := msg.From.IsBot

	// Track reply-to
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
	// Acknowledge the callback
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

// claimBot clears any leftover webhook and steals the long-poll lock
// from any previous getUpdates holder, so we enter the polling loop in
// a clean state. Telegram lets at most one client long-poll a bot at a
// time — issuing a fresh getUpdates terminates whatever request is
// in-flight on the other side. We use offset=-1, timeout=0 so this
// returns immediately and we don't consume real updates.
func (t *Telegram) claimBot() {
	if _, err := t.bot.Request(tgbotapi.DeleteWebhookConfig{DropPendingUpdates: false}); err != nil {
		slog.Warn("telegram delete webhook on startup", "account", t.accountID, "error", err)
	}
	if _, err := t.bot.GetUpdates(tgbotapi.UpdateConfig{Offset: -1, Timeout: 0, Limit: 1}); err != nil {
		slog.Warn("telegram claim long-poll lock", "account", t.accountID, "error", err)
	}
}

// registerCommands sets the bot command menu visible to users.
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

// Send sends a plain text message to a Telegram chat.
func (t *Telegram) Send(chatID string, text string) error {
	return t.SendMessage(bus.OutboundMessage{
		ChatID: chatID,
		Text:   text,
	})
}

// SendMessage sends a rich outbound message with formatting, reply-to, buttons, etc.
func (t *Telegram) SendMessage(msg bus.OutboundMessage) error {
	id, err := strconv.ParseInt(msg.ChatID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse chat ID: %w", err)
	}

	// Edit existing message
	if msg.EditMsgID != "" {
		return t.editMessage(id, msg)
	}

	// Default to legacy Markdown — Telegram's MarkdownV2 is too strict
	// (every special char needs escaping), and our agents emit standard
	// GFM. The legacy "Markdown" parse mode renders *bold*, _italic_,
	// `code`, ```fenced```, and [links](url) without making us escape
	// every brace/bracket. Headers and tables don't render in either
	// mode, so we strip ###/## prefixes pre-send. Caller can still
	// override via msg.ParseMode.
	if msg.ParseMode == "" {
		msg.ParseMode = "Markdown"
	}
	// Flatten GFM tables first — Telegram (every parse mode) renders
	// `|cell|cell|` rows as literal text. FlattenMarkdownTables turns
	// each into "label: value" lines that read like normal prose.
	text := FlattenMarkdownTables(msg.Text)
	body := convertMarkdownForTelegram(text, msg.ParseMode)

	// Send the text body first (chunked if long).
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

	// Then upload any pre-resolved attachments (image-tool output etc.).
	// Telegram has four distinct media APIs and the right one is picked
	// from MediaItem.ContentType (set by the gateway) or the filename
	// extension. Sending a PDF/MP3/MP4 through NewPhoto — which the old
	// code did unconditionally — gets rejected with `PHOTO_INVALID_DIMENSIONS`
	// or similar.
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

// telegramMediaKind picks photo / video / audio / document for a
// MediaItem. Preference: explicit ContentType (set by the gateway from
// the file's mime/ext) → filename extension lookup → "document" as the
// safe fallback. Telegram is lax about document uploads (any file
// works), so anything we can't classify confidently still goes through
// rather than being dropped.
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

// convertMarkdownForTelegram does a lightweight pass over GFM text so
// the legacy `Markdown` parse mode at least renders something useful:
//   - `### header` / `## header` / `# header` → `*header*` (bold)
//   - `**bold**` → `*bold*` (legacy mode uses single asterisk)
//   - tables and other GFM-only syntax fall through unchanged (Telegram
//     just shows them as plain text)
//
// MarkdownV2 callers get the existing escaper applied later.
func convertMarkdownForTelegram(text, mode string) string {
	if text == "" {
		return text
	}
	if mode == "MarkdownV2" {
		// V2 path: caller's existing escaper does the work. Pre-strip
		// header markers though so they don't end up as literal `\#\#\#`.
		text = stripMarkdownHeaders(text)
		text = strings.ReplaceAll(text, "**", "*")
		return text
	}
	if mode != "Markdown" {
		return text
	}
	text = stripMarkdownHeaders(text)
	// Legacy Markdown bold is `*X*`, not `**X**`. Convert paired `**`.
	text = strings.ReplaceAll(text, "**", "*")
	return text
}

// stripMarkdownHeaders rewrites lines that start with `### `, `## `, or
// `# ` (with any leading whitespace) into bold lines. Telegram doesn't
// have heading support in either parse mode; bolding the line is the
// closest approximation.
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

	// Set parse mode with fallback
	if msg.ParseMode != "" {
		if msg.ParseMode == "MarkdownV2" {
			tgMsg.Text = escapeMarkdownV2(text)
		}
		tgMsg.ParseMode = msg.ParseMode
	}

	// Reply-to (only on first chunk)
	if isFirst && msg.ReplyToMsgID != "" {
		replyID, err := strconv.Atoi(msg.ReplyToMsgID)
		if err == nil {
			tgMsg.ReplyToMessageID = replyID
		}
	}

	// Inline keyboard (only on last chunk, but we set on first for single messages)
	if isFirst && len(msg.Buttons) > 0 {
		tgMsg.ReplyMarkup = buildInlineKeyboard(msg.Buttons)
	}

	_, err := t.bot.Send(tgMsg)
	if err != nil && msg.ParseMode == "MarkdownV2" {
		// Fallback to HTML
		slog.Warn("telegram MarkdownV2 failed, trying HTML", "error", err)
		tgMsg.ParseMode = "HTML"
		tgMsg.Text = text // use original text for HTML
		_, err = t.bot.Send(tgMsg)
		if err != nil {
			// Fallback to plain text
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
		// Fallback to HTML then plain
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

// SendTyping sends a typing indicator to the chat.
func (t *Telegram) SendTyping(chatID string) error {
	id, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return fmt.Errorf("parse chat ID: %w", err)
	}
	action := tgbotapi.NewChatAction(id, tgbotapi.ChatTyping)
	_, err = t.bot.Send(action)
	return err
}

// escapeMarkdownV2 escapes special characters for Telegram MarkdownV2 format.
func escapeMarkdownV2(text string) string {
	for _, ch := range markdownV2SpecialChars {
		text = strings.ReplaceAll(text, ch, "\\"+ch)
	}
	return text
}

// splitTelegramMessage splits a message that exceeds Telegram's 4096 char limit
// at paragraph boundaries.
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

		// Try to split at paragraph boundary
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

// buildInlineKeyboard converts OutboundButton rows to a Telegram InlineKeyboardMarkup.
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
