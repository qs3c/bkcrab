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

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

var slackMentionRe = regexp.MustCompile(`<@(\w+)>`)

// Slack implements the Channel interface for Slack bots via Socket Mode.
type Slack struct {
	client      *slack.Client
	socketMode  *socketmode.Client
	bus         *bus.MessageBus
	accountID   string
	botUserID   string
	botUsername string
}

// NewSlack creates a new Slack channel instance using Socket Mode.
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

// Start connects to Slack via Socket Mode and blocks until ctx is cancelled.
func (s *Slack) Start(ctx context.Context) error {
	// Get bot user info
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

// Send sends a message to a Slack channel.
func (s *Slack) Send(chatID string, text string) error {
	_, _, err := s.client.PostMessage(chatID, slack.MsgOptionText(text, false))
	return err
}

// SendMessage delivers text + MediaItems to Slack. Text uses Slack's
// mrkdwn (auto-renders *bold* / _italic_ / `code` / lists). MediaItems
// upload as files via files.uploadV2 — Slack auto-previews images.
// Send text first so it appears above the file preview in the channel.
//
// Slack's mrkdwn doesn't render GFM tables — `|cell|cell|` ships as
// raw text. Flatten tables to "label: value" / middle-dot lines first
// so the chatter sees structured prose instead of pipe soup.
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

// SendTyping sends a typing indicator. Slack Socket Mode does not support this directly.
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
	// Ignore bot's own messages
	if ev.User == s.botUserID {
		return
	}
	// Ignore message subtypes (edits, deletes, etc.) except empty subtype (normal messages)
	if ev.SubType != "" {
		return
	}

	// Determine peer kind
	peerKind := "dm"
	if ev.ChannelType == "channel" || ev.ChannelType == "group" {
		peerKind = "group"
	}

	// Parse @mentions from text
	var mentions []string
	matches := slackMentionRe.FindAllStringSubmatch(ev.Text, -1)
	for _, m := range matches {
		userID := m[1]
		// Try to resolve username
		info, err := s.client.GetUserInfo(userID)
		if err == nil {
			mentions = append(mentions, info.Name)
		} else {
			mentions = append(mentions, userID)
		}
	}

	// Clean text: replace <@USERID> with @username
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
