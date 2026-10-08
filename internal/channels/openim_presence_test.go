package channels

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qs3c/bkcrab/internal/bus"
)

func TestOpenIMPresenceConfig(t *testing.T) {
	c := openIMTestConfig()
	original := OpenIMAccountID(c)
	for _, raw := range []string{"https://im.test", "ws://u:pass@im.test", "ws://im.test/?token=x", "ws://im.test/#x"} {
		c.WSURL = raw
		if _, err := NormalizeOpenIMConfig(c); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	c.WSURL = " wss://IM.TEST/gateway "
	normalized, err := NormalizeOpenIMConfig(c)
	if err != nil || normalized.WSURL != "wss://im.test/gateway" || OpenIMAccountID(normalized) != original {
		t.Fatalf("normalization: %+v %v", normalized, err)
	}
}

func TestOpenIMPresenceReconnectAndStop(t *testing.T) {
	var tokens, connections atomic.Int32
	connected := make(chan struct{}, 8)
	pong := make(chan struct{}, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/get_admin_token":
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"token":"admin","expireTimeSeconds":3600}}`))
		case "/auth/get_user_token":
			var req struct {
				UserID     string
				PlatformID int
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			if req.UserID != "bot" || req.PlatformID != 7 || r.Header.Get("token") != "admin" {
				t.Errorf("bad token request: %+v", req)
			}
			tokens.Add(1)
			_, _ = w.Write([]byte(`{"errCode":0,"data":{"token":"private-user-token","expireTimeSeconds":3600}}`))
		case "/ws":
			q := r.URL.Query()
			if q.Get("sendID") != "bot" || q.Get("platformID") != "7" || q.Get("token") != "private-user-token" || q.Get("sdkType") != "js" {
				t.Error("bad handshake")
			}
			conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			connections.Add(1)
			connected <- struct{}{}
			if connections.Load() == 1 {
				return
			}
			conn.SetPongHandler(func(string) error { pong <- struct{}{}; return nil })
			_ = conn.WriteControl(websocket.PingMessage, []byte("test"), time.Now().Add(time.Second))
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	c := openIMTestConfig()
	c.APIURL = server.URL
	c.WSURL = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	o, err := NewOpenIM(c, bus.New(), &openIMTestInbox{})
	if err != nil {
		t.Fatal(err)
	}
	run := func(ctx context.Context) <-chan struct{} {
		done := make(chan struct{})
		go func() { defer close(done); _ = o.Start(ctx) }()
		return done
	}
	wait := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(4 * time.Second):
			t.Fatal("lifecycle timed out")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := run(ctx)
	wait(connected)
	wait(connected)
	wait(pong)
	cancel()
	wait(done)
	// A lost lease may restart the same adapter; permanent Stop may not.
	done = run(context.Background())
	wait(connected)
	o.Stop()
	wait(done)
	done = run(context.Background())
	wait(done)
	if tokens.Load() != 3 {
		t.Fatalf("token refresh per connection: %d", tokens.Load())
	}
}
