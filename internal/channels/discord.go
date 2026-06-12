package channels

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/qs3c/bkclaw/internal/bus"
)

var (
	discordMentionRe      = regexp.MustCompile(`<@!?(\d+)>`)
	discordPlainMentionRe = regexp.MustCompile(`@(\w+)`)
)

// Discord implements the Channel interface for Discord bots.
type Discord struct {
	session       *discordgo.Session
	bus           *bus.MessageBus
	accountID     string
	botUserID     string
	botUsername   string
	botGlobalName string
}

// NewDiscord creates a new Discord channel instance.
func NewDiscord(botToken string, accountID string, mb *bus.MessageBus) (*Discord, error) {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return nil, err
	}

	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

	d := &Discord{
		session:   dg,
		bus:       mb,
		accountID: accountID,
	}

	dg.AddHandler(d.onMessageCreate)
	dg.AddHandler(d.onInteractionCreate)

	return d, nil
}

func (d *Discord) Name() string {
	return "discord"
}

func (d *Discord) AccountID() string {
	return d.accountID
}

func (d *Discord) BotUsername() string {
	return d.botUsername
}

// Start connects to Discord gateway and blocks until ctx is cancelled.
func (d *Discord) Start(ctx context.Context) error {
	if err := d.session.Open(); err != nil {
		return err
	}
	defer d.session.Close()

	// Cache bot user info
	d.botUserID = d.session.State.User.ID
	d.botUsername = d.session.State.User.Username
	d.botGlobalName = d.session.State.User.GlobalName

	slog.Info("discord bot connected",
		"username", d.botUsername,
		"global_name", d.botGlobalName,
		"user_id", d.botUserID,
		"account", d.accountID,
	)

	d.registerCommands()

	<-ctx.Done()
	return nil
}

// registerCommands publishes the bot's slash-command set to Discord so the
// native `/` autocomplete picker surfaces them (mirrors telegram.go's
// registerCommands). Without this, users have to type `/new` as plain text
// — Discord won't suggest it.
//
// Registered as GLOBAL commands (empty guild ID) so they're available in
// DMs as well as every guild the bot is in. Global commands can take a
// few minutes to propagate across Discord's cache on first publish; after
// that, edits via BulkOverwrite are usually instant.
//
// The interaction handler (onInteractionCreate) synthesizes an
// InboundMessage with text `/<cmd> <args>` so the existing slash handler
// in agent/slash.go runs unchanged — no duplicate slash logic on this side.
func (d *Discord) registerCommands() {
	appID := d.session.State.User.ID
	if appID == "" {
		slog.Warn("discord skipping command registration: empty app id")
		return
	}
	cmds := []*discordgo.ApplicationCommand{
		{Name: "start", Description: "Start the bot"},
		{Name: "new", Description: "Start a new conversation"},
		{Name: "retry", Description: "Re-run the last message"},
		{Name: "undo", Description: "Undo the last turn"},
		{Name: "compact", Description: "Compress context window"},
		{Name: "status", Description: "Show agent status"},
		{Name: "usage", Description: "Session turn & token stats"},
		{
			Name:        "insights",
			Description: "Activity insights (last 7 days)",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionInteger,
					Name:        "days",
					Description: "Number of days (default 7)",
					Required:    false,
				},
			},
		},
		{
			Name:        "personality",
			Description: "List or switch personality",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "name",
					Description: "Personality name (omit to list)",
					Required:    false,
				},
			},
		},
		{
			Name:        "model",
			Description: "Switch LLM model",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "name",
					Description: "Model name (e.g. claude-opus-4-7)",
					Required:    true,
				},
			},
		},
		{Name: "help", Description: "Show available commands"},
		{Name: "version", Description: "Show version"},
		{Name: "whoami", Description: "Show your platform user ID (for admin allowlist)"},
	}
	if _, err := d.session.ApplicationCommandBulkOverwrite(appID, "", cmds); err != nil {
		slog.Warn("discord command registration failed", "account", d.accountID, "error", err)
		return
	}
	slog.Info("discord commands registered", "account", d.accountID, "count", len(cmds))
}

// onInteractionCreate handles native slash-command clicks from Discord's
// autocomplete picker. Discord delivers these as Interactions, not as
// MessageCreate events — without this handler, clicking `/new` shows
// "The application did not respond" in the UI and the slash never reaches
// the agent's handleSlashCommand.
//
// Flow: ACK the interaction ephemerally (only the clicker sees it) within
// the 3-second window, then push a synthetic InboundMessage with text
// `/<cmd> <args>` so the standard slash path runs and posts the reply as
// a normal channel message. The ephemeral ACK is brief and self-explains
// what the bot is doing; the real reply lands in the channel as usual.
//
// For group (guild) interactions we inject the bot's username into
// Mentions so routing.go's agentByMention resolves THIS bot as the
// target — clicking the bot's own command is intent-equivalent to
// @-mentioning it.
func (d *Discord) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()

	// Build the text representation `/<cmd> <arg1> <arg2>` so the slash
	// handler parses it via the same strings.Fields path as a typed
	// command. Option order follows the registered schema (single arg per
	// command today), so this is just space-joining option values.
	var b strings.Builder
	b.WriteString("/")
	b.WriteString(data.Name)
	for _, opt := range data.Options {
		b.WriteString(" ")
		fmt.Fprintf(&b, "%v", opt.Value)
	}
	text := b.String()

	// Determine peer kind + sender identity. In guilds, the user info is
	// nested under Member; in DMs, it's at the top level.
	peerKind := "dm"
	if i.GuildID != "" {
		peerKind = "group"
	}
	var u *discordgo.User
	if i.Member != nil && i.Member.User != nil {
		u = i.Member.User
	} else if i.User != nil {
		u = i.User
	}
	if u == nil {
		slog.Warn("discord interaction missing user", "name", data.Name)
		return
	}
	senderName := u.GlobalName
	if senderName == "" {
		senderName = u.Username
	}

	// Group routing requires the bot to be in msg.Mentions for
	// agentByMention to pick THIS bot. Clicking the bot's own slash
	// command is intent-equivalent — inject the username so the
	// gateway routes the synthetic message to us instead of dropping
	// it as an unaddressed group message.
	var mentions []string
	if peerKind == "group" {
		mentions = []string{d.botUsername}
	}

	// ACK the interaction ephemerally so Discord clears the
	// "thinking..." spinner immediately. The real reply lands in the
	// channel as a normal bot message via the outbound path.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("⚡ Running `%s`…", text),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		slog.Warn("discord interaction ack failed", "cmd", data.Name, "error", err)
	}

	slog.Info("discord slash command received",
		"cmd", data.Name,
		"channel_id", i.ChannelID,
		"guild_id", i.GuildID,
		"peer_kind", peerKind,
		"from", u.Username,
	)

	// MessageID is intentionally left empty. Slash commands arrive as
	// Discord *interactions*, not channel messages — i.ID is the
	// interaction id, which can't be used as a MessageReference target
	// (Discord 400s with MESSAGE_REFERENCE_UNKNOWN_MESSAGE). The
	// outbound path keys "send as reply" off ReplyToMsgID != "", so
	// leaving it blank makes us send a plain channel message instead
	// of a referenced one. Group dedup uses chatID+userID+text hash,
	// not MessageID, so omitting it doesn't break anti-replay.
	d.bus.Inbound <- bus.InboundMessage{
		Channel:    "discord",
		AccountID:  d.accountID,
		ChatID:     i.ChannelID,
		UserID:     u.ID,
		Text:       text,
		PeerKind:   peerKind,
		SenderName: senderName,
		Mentions:   mentions,
	}
}

// Send sends a message to a Discord channel.
func (d *Discord) Send(chatID string, text string) error {
	// Discord has a 2000 char limit; split if needed
	for len(text) > 0 {
		chunk := text
		if len(chunk) > 2000 {
			chunk = text[:2000]
			text = text[2000:]
		} else {
			text = ""
		}
		if _, err := d.session.ChannelMessageSend(chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// SendMessage delivers text + any pre-resolved MediaItems to Discord.
// Discord renders standard markdown natively (bold/italic/code/lists),
// so msg.Text goes through unchanged. MediaItems upload as message
// attachments — Discord auto-renders images inline. Single
// ChannelMessageSendComplex call carries both, but if there's a long
// body that needs chunking we send the body chunked first and the
// files on the last chunk.
//
// When msg.ReplyToMsgID is set the first outbound message attaches a
// MessageReference so Discord renders our reply as a native "Replying
// to @sender" quote bubble and pings them — without this, multi-user
// channels turn into a guessing game of which question each bot reply
// is answering. Subsequent chunks ship without the reference so a
// chunked answer doesn't produce N quote bubbles + N pings.
// FailIfNotExists stays at the zero value (false) so a stale / deleted
// source message degrades to a plain send instead of dropping the
// reply on the floor.
func (d *Discord) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text != "" {
		// Discord 2000-char per-message limit. Send N-1 chunks
		// without files, then the final chunk with files attached so
		// the embedded preview lands at the end of the conversation.
		// Tables get flattened first — Discord renders bold/italic/
		// code/lists natively but ignores GFM tables (pipes show as
		// raw text); FlattenMarkdownTables turns each into a plain
		// "label: value" / middle-dot block that scans cleanly.
		text := FlattenMarkdownTables(msg.Text)
		chunks := splitDiscordMessage(text)
		for i, chunk := range chunks {
			var ref *discordgo.MessageReference
			if i == 0 && msg.ReplyToMsgID != "" {
				ref = &discordgo.MessageReference{
					MessageID: msg.ReplyToMsgID,
					ChannelID: msg.ChatID,
				}
			}
			isLast := i == len(chunks)-1
			if !isLast || len(msg.MediaItems) == 0 {
				if _, err := d.session.ChannelMessageSendComplex(msg.ChatID, &discordgo.MessageSend{
					Content:   chunk,
					Reference: ref,
				}); err != nil {
					// Defense-in-depth: the MessageReference is purely
					// cosmetic (renders the "Replying to @sender" quote
					// bubble). If Discord rejects the referenced id —
					// MESSAGE_REFERENCE_UNKNOWN_MESSAGE on a deleted
					// source, an interaction id that slipped through,
					// missing read perms on the channel, … — retry the
					// same chunk without the reference so the reply
					// still lands in the channel. Only retries when we
					// actually attached a ref: errors on a plain send
					// are real and propagate to the warn log.
					if ref != nil {
						if _, retryErr := d.session.ChannelMessageSendComplex(msg.ChatID, &discordgo.MessageSend{
							Content: chunk,
						}); retryErr == nil {
							slog.Info("discord retry without reference succeeded",
								"chat", msg.ChatID, "first_error", err)
							continue
						} else {
							slog.Warn("discord chunk send failed (retry also failed)",
								"i", i, "error", err, "retry_error", retryErr)
							continue
						}
					}
					slog.Warn("discord chunk send failed", "i", i, "error", err)
				}
				continue
			}
			if err := d.sendWithFiles(msg.ChatID, chunk, msg.MediaItems, ref); err != nil {
				if ref != nil {
					if retryErr := d.sendWithFiles(msg.ChatID, chunk, msg.MediaItems, nil); retryErr == nil {
						slog.Info("discord retry without reference succeeded (with files)",
							"chat", msg.ChatID, "first_error", err)
						continue
					} else {
						slog.Warn("discord final chunk+files failed (retry also failed)",
							"error", err, "retry_error", retryErr)
						continue
					}
				}
				slog.Warn("discord final chunk+files failed", "error", err)
			}
		}
		return nil
	}
	if len(msg.MediaItems) > 0 {
		var ref *discordgo.MessageReference
		if msg.ReplyToMsgID != "" {
			ref = &discordgo.MessageReference{
				MessageID: msg.ReplyToMsgID,
				ChannelID: msg.ChatID,
			}
		}
		if err := d.sendWithFiles(msg.ChatID, "", msg.MediaItems, ref); err != nil {
			if ref != nil {
				if retryErr := d.sendWithFiles(msg.ChatID, "", msg.MediaItems, nil); retryErr == nil {
					slog.Info("discord media-only retry without reference succeeded",
						"chat", msg.ChatID, "first_error", err)
					return nil
				}
			}
			return err
		}
		return nil
	}
	return nil
}

func (d *Discord) sendWithFiles(chatID, text string, items []bus.MediaItem, ref *discordgo.MessageReference) error {
	files := make([]*discordgo.File, 0, len(items))
	for _, it := range items {
		ct := it.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		files = append(files, &discordgo.File{
			Name:        it.Filename,
			ContentType: ct,
			Reader:      bytes.NewReader(it.Bytes),
		})
	}
	_, err := d.session.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{
		Content:   text,
		Files:     files,
		Reference: ref,
	})
	return err
}

func splitDiscordMessage(text string) []string {
	if len(text) <= 2000 {
		return []string{text}
	}
	var out []string
	for len(text) > 0 {
		if len(text) <= 2000 {
			out = append(out, text)
			break
		}
		// Prefer a paragraph break so we don't tear sentences apart.
		cut := strings.LastIndex(text[:2000], "\n\n")
		if cut < 1000 {
			cut = strings.LastIndex(text[:2000], "\n")
		}
		if cut < 1000 {
			cut = 2000
		}
		out = append(out, text[:cut])
		text = strings.TrimLeft(text[cut:], "\n")
	}
	return out
}

// SendTyping sends a typing indicator to the Discord channel.
func (d *Discord) SendTyping(chatID string) error {
	return d.session.ChannelTyping(chatID)
}

func (d *Discord) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore own messages
	if m.Author.ID == d.botUserID {
		return
	}

	// Determine peer kind
	peerKind := "dm"
	if m.GuildID != "" {
		peerKind = "group"
	}

	// Check if sender is a bot
	isBot := m.Author.Bot

	// Clean message text: replace <@ID> mentions with @username
	text := m.Content
	for _, u := range m.Mentions {
		text = strings.ReplaceAll(text, "<@"+u.ID+">", "@"+u.Username)
		text = strings.ReplaceAll(text, "<@!"+u.ID+">", "@"+u.Username)
	}

	// Collect @mentions. Discord only populates m.Mentions for the
	// formal `<@USER_ID>` markup produced by the autocomplete picker;
	// users on mobile or who skip the popup just type "@DisplayName"
	// literally, which lands in m.Content untouched. Telegram/Slack
	// already do a regex pass over text — match that here so the
	// downstream gateway sees the bot mention either way.
	var mentions []string
	seen := make(map[string]struct{})
	addMention := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		mentions = append(mentions, name)
	}
	for _, u := range m.Mentions {
		addMention(u.Username)
	}
	for _, mm := range discordPlainMentionRe.FindAllStringSubmatch(text, -1) {
		addMention(mm[1])
	}
	// If the bot was addressed by display name (GlobalName) rather than
	// its lowercase Username, also inject the Username so the gateway's
	// strict-equality match against BotUsername() resolves the bot.
	if d.botGlobalName != "" {
		if _, hit := seen[d.botGlobalName]; hit {
			addMention(d.botUsername)
		}
	}

	// Prefer the display name (GlobalName) over the unique handle so
	// the chat panel renders "idoubi" (what Discord shows everywhere)
	// rather than the post-username-overhaul lowercase handle
	// "idoubicc". Falls back to Username when GlobalName is unset
	// (legacy bots, freshly-migrated accounts, etc.).
	senderName := m.Author.GlobalName
	if senderName == "" {
		senderName = m.Author.Username
	}
	avatarURL := m.Author.AvatarURL("256")

	slog.Info("discord message received",
		"from", m.Author.Username,
		"channel_id", m.ChannelID,
		"guild_id", m.GuildID,
		"peer_kind", peerKind,
		"is_bot", isBot,
	)

	d.bus.Inbound <- bus.InboundMessage{
		Channel:         "discord",
		AccountID:       d.accountID,
		ChatID:          m.ChannelID,
		UserID:          m.Author.ID,
		MessageID:       m.ID,
		Text:            text,
		PeerKind:        peerKind,
		SenderName:      senderName,
		SenderAvatarURL: avatarURL,
		Mentions:        mentions,
		IsBotMessage:    isBot,
	}
}
