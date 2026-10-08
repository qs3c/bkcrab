package channels

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Linux is platform 7 in OpenIM v3.8. The connection is presence-only:
// callbacks remain the sole message producer, including after reconnect.
const openIMPlatform = 7

func (o *OpenIM) runPresence(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := o.presenceSession(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(started) > time.Minute {
			delay = time.Second
		}
		slog.Warn("openim presence reconnecting", "account", o.AccountID(), "error", err, "delay", delay)
		if !sleepOrDone(ctx, delay) {
			return
		}
		delay = min(2*delay, 30*time.Second)
	}
}

func (o *OpenIM) presenceSession(ctx context.Context) error {
	var data struct {
		Token             string      `json:"token"`
		ExpireTimeSeconds json.Number `json:"expireTimeSeconds"`
	}
	if err := o.api(ctx, "/auth/get_user_token", map[string]any{"userID": o.cfg.BotUserID, "platformID": openIMPlatform}, &data); err != nil {
		return err
	}
	seconds, err := data.ExpireTimeSeconds.Int64()
	if err != nil || seconds <= 0 || data.Token == "" {
		return errors.New("openim: invalid user token response")
	}
	u, _ := url.Parse(o.cfg.WSURL)
	q := u.Query()
	q.Set("sendID", o.cfg.BotUserID)
	q.Set("token", data.Token)
	q.Set("platformID", "7")
	q.Set("sdkType", "js")
	q.Set("isBackground", "false")
	q.Set("operationID", uuid.NewString())
	u.RawQuery = q.Encode()
	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 10 * time.Second
	conn, resp, err := dialer.DialContext(ctx, u.String(), nil)
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	if err != nil {
		return errors.New("openim: presence connection failed")
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	conn.SetReadLimit(8 << 20)
	refreshDeadline := func() error { return conn.SetReadDeadline(time.Now().Add(60 * time.Second)) }
	_ = refreshDeadline()
	conn.SetPongHandler(func(string) error { return refreshDeadline() })
	conn.SetPingHandler(func(data string) error {
		if err := refreshDeadline(); err != nil {
			return err
		}
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(5*time.Second))
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var envelope struct {
				ReqIdentifier int `json:"reqIdentifier"`
				ErrCode       int `json:"errCode"`
			}
			if json.Unmarshal(body, &envelope) == nil && (envelope.ErrCode != 0 || envelope.ReqIdentifier == 2002 || envelope.ReqIdentifier == 2003) {
				return
			}
		}
	}()
	defer func() { _ = conn.Close(); <-done }()
	slog.Info("openim presence connected", "account", o.AccountID())
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	// Clamp before converting to a duration; refresh long-lived tokens daily.
	lifetime := time.Duration(min(seconds, int64(86400))) * time.Second
	refresh := time.NewTimer(lifetime - lifetime/10)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-done:
			return errors.New("openim: presence disconnected")
		case <-refresh.C:
			return errors.New("openim: refreshing presence token")
		case <-ticker.C:
			if conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)) != nil {
				return errors.New("openim: presence heartbeat failed")
			}
		}
	}
}
