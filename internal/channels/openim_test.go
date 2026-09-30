package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

type openIMTestInbox struct {
	mu       sync.Mutex
	payloads map[string]string
	err      error
}

func (s *openIMTestInbox) SaveChannelInbox(_ context.Context, account, id, payload string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.payloads == nil {
		s.payloads = map[string]string{}
	}
	s.payloads[account+":"+id] = payload
	return nil
}
func (*openIMTestInbox) ClaimChannelInbox(context.Context, string, time.Duration) (*store.ChannelInboxRecord, error) {
	return nil, sql.ErrNoRows
}
func (*openIMTestInbox) CompleteChannelInbox(context.Context, string, string) error { return nil }
func (*openIMTestInbox) PruneChannelInbox(context.Context, string, time.Time) error { return nil }

func openIMTestConfig() config.OpenIMConfig {
	return config.OpenIMConfig{APIURL: "https://im.example.test", AdminUserID: "imAdmin", AdminSecret: "server-secret", BotUserID: "bot", AllowedGroupIDs: []string{"group-1"}}
}
func openIMTextEvent() OpenIMEvent {
	return OpenIMEvent{CallbackCommand: OpenIMAfterSingle, SendID: "u_external", RecvID: "bot", ServerMsgID: "msg-1", SessionType: 1, ContentType: 101, Content: `{"content":"hello"}`}
}

func TestOpenIMInboundFiltering(t *testing.T) {
	cases := []struct {
		name     string
		change   func(*OpenIMEvent)
		accepted bool
		invalid  bool
	}{
		{"single", func(*OpenIMEvent) {}, true, false},
		{"self", func(e *OpenIMEvent) { e.SendID = "bot" }, false, false},
		{"other recipient", func(e *OpenIMEvent) { e.RecvID = "other" }, false, false},
		{"notification", func(e *OpenIMEvent) { e.ContentType = 1501 }, false, false},
		{"image", func(e *OpenIMEvent) { e.ContentType = 102 }, false, false},
		{"missing id", func(e *OpenIMEvent) { e.ServerMsgID = "" }, false, true},
		{"client id fallback", func(e *OpenIMEvent) { e.ServerMsgID = ""; e.ClientMsgID = "client-1" }, true, false},
		{"bad content", func(e *OpenIMEvent) { e.Content = "not json" }, false, true},
		{"group mention", func(e *OpenIMEvent) {
			e.CallbackCommand = OpenIMAfterGroup
			e.SessionType = 3
			e.GroupID = "group-1"
			e.AtUserList = []string{"bot"}
			e.ContentType = 106
			e.Content = `{"text":"@bot help"}`
		}, true, false},
		{"group no mention", func(e *OpenIMEvent) { e.CallbackCommand = OpenIMAfterGroup; e.SessionType = 3; e.GroupID = "group-1" }, false, false},
		{"group not allowed", func(e *OpenIMEvent) {
			e.CallbackCommand = OpenIMAfterGroup
			e.SessionType = 3
			e.GroupID = "other"
			e.AtUserList = []string{"bot"}
		}, false, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			inbox := &openIMTestInbox{}
			o, err := NewOpenIM(openIMTestConfig(), bus.New(), inbox)
			if err != nil {
				t.Fatal(err)
			}
			e := openIMTextEvent()
			tt.change(&e)
			body, _ := json.Marshal(e)
			err = o.HandleWebhook(context.Background(), e.CallbackCommand, body)
			if (err != nil) != tt.invalid {
				t.Fatalf("error: %v", err)
			}
			if (len(inbox.payloads) == 1) != tt.accepted {
				t.Fatalf("accepted: %v", inbox.payloads)
			}
			for _, payload := range inbox.payloads {
				var msg bus.InboundMessage
				if err := json.Unmarshal([]byte(payload), &msg); err != nil {
					t.Fatal(err)
				}
				if msg.AccountID != o.AccountID() || msg.Channel != "openim" || msg.UserID == e.SendID || msg.MessageID == "" {
					t.Fatalf("invalid mapping: %+v", msg)
				}
				if e.SessionType == 3 && (msg.PeerKind != "group" || len(msg.Mentions) != 1 || msg.Mentions[0] != "bot") {
					t.Fatalf("missing mention: %+v", msg)
				}
			}
		})
	}
}

func TestOpenIMInstanceIsolationAndValidation(t *testing.T) {
	c := openIMTestConfig()
	a, _ := NewOpenIM(c, nil, nil)
	c.APIURL += "/"
	normalized, err := NormalizeOpenIMConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if OpenIMAccountID(normalized) != a.AccountID() {
		t.Fatal("trailing slash changes identity")
	}
	c.APIURL = "https://other.example.test"
	b, _ := NewOpenIM(c, nil, nil)
	if a.AccountID() == b.AccountID() || a.chatID("dm", "u") == b.chatID("dm", "u") {
		t.Fatal("cross-instance collision")
	}
	if err := a.Send(b.chatID("dm", "u"), "hello"); err == nil {
		t.Fatal("cross-instance send allowed")
	}
	c = openIMTestConfig()
	c.BotUserID = "other-bot"
	other, _ := NewOpenIM(c, nil, nil)
	if a.WebhookSecret() != other.WebhookSecret() || a.AccountID() == other.AccountID() {
		t.Fatal("bot identity/instance webhook mismatch")
	}
	for _, u := range []string{"file:///tmp", "http://user:password@localhost", "https://x/?secret=x", "https://x/#fragment"} {
		c.APIURL = u
		if _, err := NormalizeOpenIMConfig(c); err == nil {
			t.Errorf("accepted invalid URL %q", u)
		}
	}
	if _, err := ParseOpenIMEvent("beforeSend", []byte(`{}`)); err == nil {
		t.Fatal("accepted before callback")
	}
}

func TestOpenIMAPIValidationRefreshAndSend(t *testing.T) {
	var tokens, sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("operationID") == "" {
			t.Error("missing operationID")
		}
		switch r.URL.Path {
		case "/auth/get_admin_token":
			tokens++
			var in map[string]string
			json.NewDecoder(r.Body).Decode(&in)
			if in["secret"] != "server-secret" || in["userID"] != "imAdmin" {
				t.Errorf("incorrect auth fields")
			}
			json.NewEncoder(w).Encode(map[string]any{"errCode": 0, "data": map[string]any{"token": "token", "expireTimeSeconds": "3600"}})
		case "/user/get_users_info":
			if r.Header.Get("token") != "token" {
				t.Error("missing token")
			}
			ioData := `{"errCode":0,"data":{"usersInfo":[{"userID":"bot","nickname":"Assistant"}]}}`
			w.Write([]byte(ioData))
		case "/msg/send_msg":
			sends++
			if sends == 1 {
				w.Write([]byte(`{"errCode":1501}`))
				return
			}
			var in map[string]any
			json.NewDecoder(r.Body).Decode(&in)
			if in["sendID"] != "bot" || in["contentType"] != float64(101) {
				t.Errorf("incorrect send: %+v", in)
			}
			if sends == 2 && (in["recvID"] != "u_external" || in["sessionType"] != float64(1)) {
				t.Errorf("incorrect DM: %+v", in)
			}
			if sends == 3 && (in["groupID"] != "group-1" || in["sessionType"] != float64(3) || in["recvID"] != nil) {
				t.Errorf("incorrect group: %+v", in)
			}
			w.Write([]byte(`{"errCode":0,"data":{"serverMsgID":"out-1"}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	c := openIMTestConfig()
	c.APIURL = server.URL
	o, _ := NewOpenIM(c, nil, nil)
	name, err := o.Validate(context.Background())
	if err != nil || name != "Assistant" {
		t.Fatalf("validate: %s %v", name, err)
	}
	if err := o.Send(o.chatID("dm", "u_external"), "reply"); err != nil {
		t.Fatal(err)
	}
	if tokens != 2 || sends != 2 {
		t.Fatalf("tokens=%d sends=%d", tokens, sends)
	}
	if err := o.Send(o.chatID("group", "group-1"), "group reply"); err != nil {
		t.Fatal(err)
	}
	if tokens != 2 || sends != 3 {
		t.Fatalf("group token cache: tokens=%d sends=%d", tokens, sends)
	}
	if err := o.Send(o.chatID("group", "other-group"), "denied"); err == nil {
		t.Fatal("sent to a non-allowed group")
	}
}

func TestOpenIMDoesNotRetryAmbiguousSend(t *testing.T) {
	var sends int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends++; w.WriteHeader(500) }))
	defer server.Close()
	c := openIMTestConfig()
	c.APIURL = server.URL
	o, _ := NewOpenIM(c, nil, nil)
	o.token = "token"
	o.tokenExpires = time.Now().Add(time.Hour)
	if err := o.Send(o.chatID("dm", "user"), "reply"); err == nil {
		t.Fatal("expected send error")
	}
	if sends != 1 {
		t.Fatalf("ambiguous send retried %d times", sends)
	}
}

func TestOpenIMInboxFailureAndStop(t *testing.T) {
	inbox := &openIMTestInbox{err: errors.New("database unavailable")}
	o, _ := NewOpenIM(openIMTestConfig(), bus.New(), inbox)
	if err := o.AcceptEvent(context.Background(), openIMTextEvent()); err == nil {
		t.Fatal("acknowledged without persistence")
	}
	done := make(chan error, 1)
	go func() { done <- o.Start(context.Background()) }()
	o.Stop()
	o.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestOpenIMDurableIngressDrainsAfterStart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "inbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	mb := bus.New()
	o, err := NewOpenIM(openIMTestConfig(), mb, db)
	if err != nil {
		t.Fatal(err)
	}
	o.SetBinding("owner-a", "agent")
	e := openIMTextEvent()
	if err := o.AcceptEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if len(mb.Inbound) != 0 {
		t.Fatal("webhook bypassed durable queue")
	}
	other, _ := NewOpenIM(openIMTestConfig(), mb, db)
	other.SetBinding("owner-b", "agent")
	if _, err := db.ClaimChannelInbox(ctx, other.inboxKey(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old owner's pending inbox visible: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- o.Start(ctx) }()
	select {
	case msg := <-mb.Inbound:
		if msg.Text != "hello" || msg.AccountID != o.AccountID() {
			t.Fatalf("wrong delivery: %+v", msg)
		}
	case <-ctx.Done():
		t.Fatal("persisted message was not drained")
	}
	o.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not stop")
	}
	if err := o.AcceptEvent(ctx, e); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimChannelInbox(ctx, o.inboxKey(), time.Minute); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed callback replayed: %v", err)
	}
}
