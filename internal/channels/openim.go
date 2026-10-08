package channels

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	OpenIMAfterSingle = "callbackAfterSendSingleMsgCommand"
	OpenIMAfterGroup  = "callbackAfterSendGroupMsgCommand"
)

type OpenIM struct {
	cfg          config.OpenIMConfig
	bus          *bus.MessageBus
	inbox        store.ChannelInbox
	inboxAccount string
	client       *http.Client
	tokenMu      sync.Mutex
	token        string
	tokenExpires time.Time
	stop         chan struct{}
	stopOnce     sync.Once
}

func NewOpenIM(c config.OpenIMConfig, mb *bus.MessageBus, inbox store.ChannelInbox) (*OpenIM, error) {
	c, err := NormalizeOpenIMConfig(c)
	if err != nil {
		return nil, err
	}
	return &OpenIM{cfg: c, bus: mb, inbox: inbox, stop: make(chan struct{}), client: &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("openim: redirects are disabled") },
	}}, nil
}

func (o *OpenIM) Name() string            { return "openim" }
func (o *OpenIM) AccountID() string       { return OpenIMAccountID(o.cfg) }
func (o *OpenIM) BotUsername() string     { return o.cfg.BotUserID }
func (o *OpenIM) InstanceID() string      { return OpenIMInstanceID(o.cfg) }
func (o *OpenIM) WebhookSecret() string   { return OpenIMWebhookSecret(o.cfg) }
func (o *OpenIM) Stop()                   { o.stopOnce.Do(func() { close(o.stop) }) }
func (o *OpenIM) Done() <-chan struct{}   { return o.stop }
func (o *OpenIM) SendTyping(string) error { return nil }

// SetBinding scopes pending ingress to its owner and agent. A bot attached to
// another owner's agent must not replay the previous binding's pending inbox.
// Call before registering the adapter with the manager.
func (o *OpenIM) SetBinding(ownerID, agentID string) {
	o.inboxAccount = o.AccountID() + "-" + openIMHash(ownerID + "\x00" + agentID)[:32]
}

func (o *OpenIM) inboxKey() string {
	if o.inboxAccount != "" {
		return o.inboxAccount
	}
	return o.AccountID()
}

// Start drains SQL ingress. Claims fence simultaneous workers and survive
// process restarts; stopping a binding leaves pending records for reconnection.
func (o *OpenIM) Start(ctx context.Context) error {
	if o.inbox == nil || o.bus == nil {
		return errors.New("openim: durable inbox and message bus required")
	}
	select {
	case <-o.stop:
		return nil
	default:
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if o.cfg.WSURL != "" {
			o.runPresence(ctx)
		}
	}()
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastPrune := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-o.stop:
			return nil
		default:
		}
		if time.Since(lastPrune) > time.Hour {
			if err := o.inbox.PruneChannelInbox(ctx, o.inboxKey(), time.Now().Add(-7*24*time.Hour)); err != nil {
				slog.Warn("openim inbox prune failed", "error", err)
			}
			lastPrune = time.Now()
		}
		rec, err := o.inbox.ClaimChannelInbox(ctx, o.inboxKey(), 30*time.Second)
		if err == nil && rec != nil {
			var msg bus.InboundMessage
			if err := json.Unmarshal([]byte(rec.Payload), &msg); err != nil {
				slog.Error("openim inbox payload invalid", "id", rec.ID)
			} else {
				timer := time.NewTimer(2 * time.Second)
				delivered := false
				select {
				case o.bus.Inbound <- msg:
					delivered = true
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return nil
				case <-o.stop:
					timer.Stop()
					return nil
				}
				timer.Stop()
				if delivered {
					if err := o.inbox.CompleteChannelInbox(ctx, rec.ID, rec.ClaimToken); err != nil {
						slog.Warn("openim inbox completion failed", "id", rec.ID, "error", err)
					}
					continue
				}
			}
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, store.ErrNotFound) {
			slog.Warn("openim inbox claim failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-o.stop:
			return nil
		case <-ticker.C:
		}
	}
}

type OpenIMEvent struct {
	CallbackCommand string   `json:"callbackCommand"`
	SendID          string   `json:"sendID"`
	RecvID          string   `json:"recvID"`
	GroupID         string   `json:"groupID"`
	ServerMsgID     string   `json:"serverMsgID"`
	ClientMsgID     string   `json:"clientMsgID"`
	SessionType     int      `json:"sessionType"`
	ContentType     int      `json:"contentType"`
	Content         string   `json:"content"`
	AtUserList      []string `json:"atUserList"`
	SenderNickname  string   `json:"senderNickname"`
	FaceURL         string   `json:"faceURL"`
}

func ParseOpenIMEvent(command string, body []byte) (OpenIMEvent, error) {
	var e OpenIMEvent
	if command != OpenIMAfterSingle && command != OpenIMAfterGroup {
		return e, errors.New("openim: unsupported callback command")
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return e, errors.New("openim: invalid callback JSON")
	}
	if e.CallbackCommand != command || e.SendID == "" {
		return e, errors.New("openim: invalid callback command or sender")
	}
	if len(e.SendID) > 64 || len(e.RecvID) > 64 || len(e.GroupID) > 64 {
		return e, errors.New("openim: user and group IDs must not exceed 64 bytes")
	}
	if (command == OpenIMAfterSingle && (e.SessionType != 1 || e.RecvID == "")) || (command == OpenIMAfterGroup && (e.SessionType != 3 || e.GroupID == "")) {
		return e, errors.New("openim: invalid callback conversation")
	}
	return e, nil
}

func (o *OpenIM) chatID(kind, target string) string {
	return o.AccountID() + "." + kind + "." + base64.RawURLEncoding.EncodeToString([]byte(target))
}

func (o *OpenIM) HandleWebhook(ctx context.Context, command string, body []byte) error {
	e, err := ParseOpenIMEvent(command, body)
	if err != nil {
		return err
	}
	return o.AcceptEvent(ctx, e)
}

// AcceptEvent is called only after the gateway verifies the instance capability.
func (o *OpenIM) AcceptEvent(ctx context.Context, e OpenIMEvent) error {
	if e.SendID == o.cfg.BotUserID {
		return nil
	}
	kind, target := "dm", e.SendID
	if e.SessionType == 1 {
		if e.RecvID != o.cfg.BotUserID {
			return nil
		}
	} else if e.SessionType == 3 {
		if !slices.Contains(o.cfg.AllowedGroupIDs, e.GroupID) || !slices.Contains(e.AtUserList, o.cfg.BotUserID) {
			return nil
		}
		kind, target = "group", e.GroupID
	} else {
		return nil
	}
	var content struct {
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	if e.ContentType != 101 && e.ContentType != 106 {
		return nil
	}
	if err := json.Unmarshal([]byte(e.Content), &content); err != nil {
		return errors.New("openim: invalid text content")
	}
	text := content.Content
	if e.ContentType == 106 {
		text = content.Text
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	id := e.ServerMsgID
	if id == "" {
		id = e.ClientMsgID
	}
	if id == "" {
		return errors.New("openim: message ID required")
	}
	msg := bus.InboundMessage{Channel: "openim", AccountID: o.AccountID(), ChatID: o.chatID(kind, target), UserID: o.InstanceID() + ":" + openIMHash(e.SendID), MessageID: id, Text: text, PeerKind: kind, SenderName: e.SenderNickname, SenderAvatarURL: e.FaceURL}
	if kind == "group" {
		msg.Mentions = []string{o.cfg.BotUserID}
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if o.inbox == nil {
		return errors.New("openim: durable inbox unavailable")
	}
	return o.inbox.SaveChannelInbox(ctx, o.inboxKey(), msg.ChatID+":"+id, string(payload))
}

func (o *OpenIM) Send(chatID, text string) error {
	return o.SendMessage(bus.OutboundMessage{ChatID: chatID, Text: text})
}

func (o *OpenIM) SendMessage(msg bus.OutboundMessage) error {
	parts := strings.Split(msg.ChatID, ".")
	if len(parts) != 3 || parts[0] != o.AccountID() || (parts[1] != "dm" && parts[1] != "group") {
		return errors.New("openim: invalid outbound conversation")
	}
	target, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(target) == 0 {
		return errors.New("openim: invalid outbound recipient")
	}
	if parts[1] == "group" && !slices.Contains(o.cfg.AllowedGroupIDs, string(target)) {
		return errors.New("openim: group is not allowed")
	}
	text := msg.Text
	if len(msg.MediaItems) > 0 || len(msg.MediaPaths) > 0 {
		text += "\n[当前 OpenIM 渠道暂不支持发送附件，请在 bkcrab 网页中查看。]"
	}
	for _, row := range msg.Buttons {
		for _, button := range row {
			if button.URL != "" {
				text += "\n" + button.Text + ": " + button.URL
			}
		}
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if msg.EditMsgID != "" {
		return errors.New("openim: message editing is not supported")
	}
	input := map[string]any{"sendID": o.cfg.BotUserID, "senderPlatformID": 10, "contentType": 101, "sessionType": 1, "content": map[string]string{"content": FlattenMarkdownTables(text)}, "isOnlineOnly": false, "notOfflinePush": false}
	if parts[1] == "group" {
		input["groupID"] = string(target)
		input["sessionType"] = 3
	} else {
		input["recvID"] = string(target)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return o.api(ctx, "/msg/send_msg", input, nil)
}
