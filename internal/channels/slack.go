package channels

import (
	"bytes"
	"context"
	"log/slog"
	"regexp"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"

	"github.com/qs3c/bkcrab/internal/bus"
)

var slackMentionRe = regexp.MustCompile(`<@(\w+)>`)

// Slack 通过 Socket Mode 实现 Channel 接口。
type Slack struct {
	client      *slack.Client
	socketMode  *socketmode.Client
	bus         *bus.MessageBus
	accountID   string
	botUserID   string
	botUsername string
}

// NewSlack 使用 Socket Mode 创建新的 Slack 渠道实例。
func NewSlack(botToken, appToken, accountID string, mb *bus.MessageBus) (*Slack, error) {
	api := slack.New(
		botToken,
		slack.OptionAppLevelToken(appToken),
	)

	sm := socketmode.New(api)

	s := &Slack{
		client:     api,
		socketMode: sm,
		bus:        mb,
		accountID:  accountID,
	}

	return s, nil
}

func (s *Slack) Name() string {
	return "slack"
}

func (s *Slack) AccountID() string {
	return s.accountID
}

func (s *Slack) BotUsername() string {
	return s.botUsername
}

// Start 通过 Socket Mode 连接到 Slack 并阻塞直到 ctx 被取消。
func (s *Slack) Start(ctx context.Context) error {
	// 获取 bot 用户信息
	authResp, err := s.client.AuthTest()
	if err != nil {
		return err
	}
	s.botUserID = authResp.UserID
	s.botUsername = authResp.User

	slog.Info("slack bot connected",
		"username", s.botUsername,
		"user_id", s.botUserID,
		"account", s.accountID,
	)

	go s.handleEvents(ctx)

	return s.socketMode.RunContext(ctx)
}

// Send 向 Slack 渠道发送消息。
func (s *Slack) Send(chatID string, text string) error {
	_, _, err := s.client.PostMessage(chatID, slack.MsgOptionText(text, false))
	return err
}

// SendMessage 将文本 + MediaItems 投递到 Slack。文本使用 Slack 的
// mrkdwn（自动渲染 *粗体* / _斜体_ / `代码` / 列表）。MediaItems
// 通过 files.uploadV2 上传为文件——Slack 自动预览图片。先发送文本，
// 使其出现在渠道中的文件预览上方。
//
// Slack 的 mrkdwn 不渲染 GFM 表格——`|cell|cell|` 以纯文本显示。
// 先将表格展平为 "label: value" / 中点行，以便聊天者看到结构化散文
// 而非管道杂烩。
func (s *Slack) SendMessage(msg bus.OutboundMessage) error {
	if msg.Text != "" {
		text := FlattenMarkdownTables(msg.Text)
		if err := s.Send(msg.ChatID, text); err != nil {
			slog.Warn("slack text send failed", "error", err)
		}
	}
	for _, item := range msg.MediaItems {
		params := slack.UploadFileParameters{
			Channel:  msg.ChatID,
			Filename: item.Filename,
			Reader:   bytes.NewReader(item.Bytes),
		}
		if _, err := s.client.UploadFile(params); err != nil {
			slog.Warn("slack file upload failed", "filename", item.Filename, "error", err)
		}
	}
	return nil
}

// SendTyping 发送输入指示器。Slack Socket Mode 不直接支持此功能。
func (s *Slack) SendTyping(_ string) error {
	return nil
}

func (s *Slack) handleEvents(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-s.socketMode.Events:
			switch evt.Type {
			case socketmode.EventTypeEventsAPI:
				eventsAPIEvent, ok := evt.Data.(slackevents.EventsAPIEvent)
				if !ok {
					continue
				}
				s.socketMode.Ack(*evt.Request)
				s.handleEventsAPI(eventsAPIEvent)
			}
		}
	}
}

func (s *Slack) handleEventsAPI(event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			s.handleMessage(ev)
		}
	}
}

func (s *Slack) handleMessage(ev *slackevents.MessageEvent) {
	// 忽略 bot 自身的消息
	if ev.User == s.botUserID {
		return
	}
	// 忽略消息子类型（编辑、删除等），空子类型除外（正常消息）
	if ev.SubType != "" {
		return
	}

	// 确定 peer 类型
	peerKind := "dm"
	if ev.ChannelType == "channel" || ev.ChannelType == "group" {
		peerKind = "group"
	}

	// 从文本中解析 @提及
	var mentions []string
	matches := slackMentionRe.FindAllStringSubmatch(ev.Text, -1)
	for _, m := range matches {
		userID := m[1]
		// 尝试解析用户名
		info, err := s.client.GetUserInfo(userID)
		if err == nil {
			mentions = append(mentions, info.Name)
		} else {
			mentions = append(mentions, userID)
		}
	}

	// 清理文本：将 <@USERID> 替换为 @username
	text := ev.Text
	for _, m := range matches {
		userID := m[1]
		info, err := s.client.GetUserInfo(userID)
		if err == nil {
			text = strings.ReplaceAll(text, m[0], "@"+info.Name)
		}
	}

	isBot := ev.BotID != ""

	slog.Info("slack message received",
		"from", ev.User,
		"channel", ev.Channel,
		"peer_kind", peerKind,
		"is_bot", isBot,
	)

	d := bus.InboundMessage{
		Channel:      "slack",
		AccountID:    s.accountID,
		ChatID:       ev.Channel,
		UserID:       ev.User,
		MessageID:    ev.TimeStamp,
		Text:         text,
		PeerKind:     peerKind,
		SenderName:   ev.User,
		Mentions:     mentions,
		IsBotMessage: isBot,
	}
	s.bus.Inbound <- d
}
