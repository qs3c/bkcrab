package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/channels"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

type openIMGatewayInbox struct{ saved map[string]int }

func (s *openIMGatewayInbox) SaveChannelInbox(_ context.Context, account, id, payload string) error {
	s.saved[account]++
	return nil
}
func (*openIMGatewayInbox) ClaimChannelInbox(context.Context, string, time.Duration) (*store.ChannelInboxRecord, error) {
	return nil, store.ErrNotFound
}
func (*openIMGatewayInbox) CompleteChannelInbox(context.Context, string, string) error { return nil }
func (*openIMGatewayInbox) PruneChannelInbox(context.Context, string, time.Time) error { return nil }

func TestOpenIMInstanceWebhookDispatch(t *testing.T) {
	inbox := &openIMGatewayInbox{saved: map[string]int{}}
	mb := bus.New()
	manager := channels.NewManager(mb)
	g := &Gateway{chanMgr: manager}
	c := config.OpenIMConfig{APIURL: "http://im.example.test:10002", AdminUserID: "admin", AdminSecret: "secret", BotUserID: "bot-a", AllowedGroupIDs: []string{"g"}}
	a, err := channels.NewOpenIM(c, mb, inbox)
	if err != nil {
		t.Fatal(err)
	}
	manager.Register(a)
	c.BotUserID = "bot-b"
	b, _ := channels.NewOpenIM(c, mb, inbox)
	manager.Register(b)
	e := channels.OpenIMEvent{CallbackCommand: channels.OpenIMAfterGroup, SendID: "human", GroupID: "g", ServerMsgID: "m", SessionType: 3, ContentType: 106, Content: `{"text":"help"}`, AtUserList: []string{"bot-a", "bot-b"}}
	body, _ := json.Marshal(e)
	if status, _ := g.DispatchOpenIMWebhook(context.Background(), a.InstanceID(), "wrong", e.CallbackCommand, body); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized: %d", status)
	}
	if len(inbox.saved) != 0 {
		t.Fatal("unauthenticated callback persisted")
	}
	if status, err := g.DispatchOpenIMWebhook(context.Background(), a.InstanceID(), a.WebhookSecret(), e.CallbackCommand, body); status != 200 || err != nil {
		t.Fatalf("dispatch: %d %v", status, err)
	}
	if inbox.saved[a.AccountID()] != 1 || inbox.saved[b.AccountID()] != 1 {
		t.Fatalf("fanout: %+v", inbox.saved)
	}
	e.SendID = "bot-a"
	body, _ = json.Marshal(e)
	if status, err := g.DispatchOpenIMWebhook(context.Background(), a.InstanceID(), a.WebhookSecret(), e.CallbackCommand, body); status != 200 || err != nil {
		t.Fatal(err)
	}
	if inbox.saved[b.AccountID()] != 1 {
		t.Fatal("bot loop was not filtered")
	}
	if status, _ := g.DispatchOpenIMWebhook(context.Background(), a.InstanceID(), a.WebhookSecret(), channels.OpenIMAfterSingle, body); status != 400 {
		t.Fatalf("mismatched callback: %d", status)
	}
	manager.Unregister("openim", a.AccountID())
	manager.Unregister("openim", b.AccountID())
	if status, _ := g.DispatchOpenIMWebhook(context.Background(), a.InstanceID(), a.WebhookSecret(), e.CallbackCommand, body); status != 401 {
		t.Fatalf("disconnected instance: %d", status)
	}
}

func TestOpenIMGroupDedupUsesMessageID(t *testing.T) {
	g := &Gateway{}
	m := bus.InboundMessage{Channel: "openim", AccountID: "a", ChatID: "g", PeerKind: "group", UserID: "u", Text: "same", MessageID: "one"}
	if g.isDuplicate(m) || !g.isDuplicate(m) {
		t.Fatal("same callback not deduplicated")
	}
	m.MessageID = "two"
	if g.isDuplicate(m) {
		t.Fatal("distinct message with same text dropped")
	}
	m.AccountID = "b"
	if g.isDuplicate(m) {
		t.Fatal("other bot dropped")
	}
	if got := g.replyAccountID(nil, "agent", m); got != "b" {
		t.Fatalf("wrong reply bot: %q", got)
	}
}
