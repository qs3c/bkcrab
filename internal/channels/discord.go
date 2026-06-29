package channels

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/qs3c/bkcrab/internal/bus"
)

var (
	discordMentionRe      = regexp.MustCompile(`<@!?(\d+)>`)
	discordPlainMentionRe = regexp.MustCompile(`@(\w+)`)
)

// Discord 实现了 Discord bot 的 Channel 接口。
type Discord struct {
	session       *discordgo.Session
	bus           *bus.MessageBus
	accountID     string
	botUserID     string
	botUsername   string
	botGlobalName string
}

// NewDiscord 创建新的 Discord 渠道实例。
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

// Start 连接到 Discord 网关并阻塞直到 ctx 被取消。
func (d *Discord) Start(ctx context.Context) error {
	if err := d.session.Open(); err != nil {
		return err
	}
	defer d.session.Close()

	// 缓存 bot 用户信息
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

// registerCommands 将 bot 的斜杠命令集发布到 Discord，以便原生
// `/` 自动补全选择器显示它们（镜像 telegram.go 的 registerCommands）。
// 没有它，用户必须将 `/new` 作为纯文本输入——Discord 不会建议它。
//
// 注册为全局命令（空 guild ID），以便在 DM 和 bot 所在的每个
// guild 中都可用。全局命令在首次发布时可能需要几分钟才能在
// Discord 缓存中传播；之后通过 BulkOverwrite 编辑通常立即可见。
//
// 交互处理程序（onInteractionCreate）合成一个文本为 `/<cmd> <args>`
// 的 InboundMessage，以便现有的斜杠处理程序在 agent/slash.go 中
// 不变运行——此侧无重复斜杠逻辑。
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

// onInteractionCreate 处理来自 Discord 自动补全选择器的原生斜杠命令点击。
// Discord 将这些作为 Interaction 传递，而非 MessageCreate 事件——没有此
// 处理程序，点击 `/new` 会在 UI 中显示 "The application did not respond"，
// 且斜杠永远无法到达 agent 的 handleSlashCommand。
//
// 流程：在 3 秒窗口内临时确认交互（仅点击者可见），然后推送一个文本为
// `/<cmd> <args>` 的合成 InboundMessage，使标准斜杠路径运行并将回复作为
// 正常渠道消息发布。临时确认简短且自我解释 bot 正在做什么；真实回复
// 照常落在渠道中。
//
// 对于群组（guild）交互，我们将 bot 的用户名注入 Mentions，以便
// routing.go 的 agentByMention 将此 bot 解析为目标——点击 bot 自身
// 的命令在意图上等同于 @提及它。
func (d *Discord) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	data := i.ApplicationCommandData()

	// 构建文本表示 `/<cmd> <arg1> <arg2>`，以便斜杠处理程序通过
	// 与输入命令相同的 strings.Fields 路径解析。选项顺序遵循
	// 注册的 schema（目前每个命令单个参数），因此这只是空格连接选项值。
	var b strings.Builder
	b.WriteString("/")
	b.WriteString(data.Name)
	for _, opt := range data.Options {
		b.WriteString(" ")
		fmt.Fprintf(&b, "%v", opt.Value)
	}
	text := b.String()

	// 确定 peer 类型 + 发送者身份。在 guild 中，用户信息嵌套在 Member 下；
	// 在 DM 中，在顶层。
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

	// 群组路由要求 bot 在 msg.Mentions 中，以便 agentByMention 选择此 bot。
	// 点击 bot 自身的斜杠命令在意图上等同——注入用户名，以便网关将合成
	// 消息路由给我们，而非将其丢弃为未寻址的群组消息。
	var mentions []string
	if peerKind == "group" {
		mentions = []string{d.botUsername}
	}

	// 临时确认交互，使 Discord 立即清除 "正在思考..." 旋转器。
	// 真实回复通过出站路径作为正常 bot 消息落在渠道中。
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

	// MessageID 故意留空。斜杠命令作为 Discord *交互* 到达，而非渠道
	// 消息——i.ID 是交互 id，不能用作 MessageReference 目标
	// （Discord 会以 MESSAGE_REFERENCE_UNKNOWN_MESSAGE 返回 400）。
	// 出站路径通过 ReplyToMsgID != "" 判断"作为回复发送"，因此将其
	// 留空使我们发送普通渠道消息而非引用消息。群组去重使用
	// chatID+userID+text 哈希而非 MessageID，因此省略它不会破坏防重放。
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

// Send 向 Discord 渠道发送消息。
func (d *Discord) Send(chatID string, text string) error {
	// Discord 每条消息有 2000 字符限制；需要时拆分
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

// SendMessage 将文本 + 任何预解析的 MediaItems 投递到 Discord。
// Discord 原生渲染标准 markdown（粗体/斜体/代码/列表），因此 msg.Text
// 原样通过。MediaItems 作为消息附件上传——Discord 自动内联渲染图片。
// 单次 ChannelMessageSendComplex 调用同时承载两者，但如果正文较长
// 需要分块，我们先发送分块的正文，最后一块附带文件。
//
// 当 msg.ReplyToMsgID 设置时，第一条出站消息附加 MessageReference，
// 使 Discord 将我们的回复渲染为原生的 "Replying to @sender" 引用气泡
// 并通知对方——没有这个，多用户渠道中每个 bot 回复回答哪个问题就成了
// 猜谜游戏。后续分块不带引用发送，这样分块答案不会产生 N 个引用气泡
// + N 次通知。FailIfNotExists 保持零值（false），以便过时/已删除的源消息
// 降级为普通发送而非丢弃回复。
func (d *Discord) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text != "" {
		// Discord 2000 字符每条消息限制。先发送 N-1 个无文件块，
		// 然后在最后一个块附带文件，使嵌入预览出现在对话末尾。
		// 首先展平表格——Discord 原生渲染粗体/斜体/代码/列表但忽略
		// GFM 表格（管道显示为原始文本）；FlattenMarkdownTables 将每行
		// 转换为清晰的 "label: value" / 中点块。
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
					// 深度防御：MessageReference 只是外观上的（渲染 "Replying to @sender"
					// 引用气泡）。如果 Discord 拒绝引用的 id——删除消息上的
					// MESSAGE_REFERENCE_UNKNOWN_MESSAGE、泄漏的交互 id、缺少
					// 渠道读取权限等——则不带引用重试同一块，使回复仍然落在
					// 渠道中。仅在确实附加了引用时重试：普通发送上的错误是
					// 真实的并传播到警告日志。
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
		// 优先在段落断开处拆分，避免撕裂句子。
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

// SendTyping 向 Discord 渠道发送输入指示器。
func (d *Discord) SendTyping(chatID string) error {
	return d.session.ChannelTyping(chatID)
}

func (d *Discord) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// 忽略自身消息
	if m.Author.ID == d.botUserID {
		return
	}

	// 确定 peer 类型
	peerKind := "dm"
	if m.GuildID != "" {
		peerKind = "group"
	}

	// 检查发送者是否为 bot
	isBot := m.Author.Bot

	// 清理消息文本：将 <@ID> 提及替换为 @username
	text := m.Content
	for _, u := range m.Mentions {
		text = strings.ReplaceAll(text, "<@"+u.ID+">", "@"+u.Username)
		text = strings.ReplaceAll(text, "<@!"+u.ID+">", "@"+u.Username)
	}

	// 收集 @提及。Discord 仅为自动补全选择器生成的正式 `<@USER_ID>` 标记
	// 填充 m.Mentions；移动端用户或跳过弹窗的用户直接输入 "@DisplayName"，
	// 原封不动地出现在 m.Content 中。Telegram/Slack 已经对文本做了正则
	// 扫描——在此匹配以便下游网关无论哪种方式都能看到 bot 提及。
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
	// 如果 bot 被以显示名称（GlobalName）而非其小写用户名提及，
	// 也注入用户名，以便网关对 BotUsername() 的严格相等匹配解析 bot。
	if d.botGlobalName != "" {
		if _, hit := seen[d.botGlobalName]; hit {
			addMention(d.botUsername)
		}
	}

	// 优先使用显示名称（GlobalName）而非唯一句柄，以便聊天面板渲染
	// "idoubi"（Discord 各处显示的内容），而非用户名大修后的
	// 小写句柄 "idoubicc"。GlobalName 未设置时回退到 Username
	//（旧版 bot、刚迁移的账号等）。
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
