package channels

import (
	"context"
	"sync"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

// WebChannel is the in-process fan-out for web chat clients. It satisfies
// the Channel interface so channels.Manager can route bus.Outbound
// messages with Channel="web" through it like any other channel — but
// instead of pushing to an external service (Telegram / Discord / Slack)
// it forwards to per-(agentID, sessionID) subscribers held open by SSE
// handlers.
//
// This is what fixes "WARN unknown outbound channel key=web:" for cron-
// fired replies: the cron scheduler enqueues an outbound on the bus,
// the channels manager finds WebChannel registered at "web:", and
// WebChannel.SendMessage fans out to whichever browser tabs are
// subscribed for that chat. Tabs that aren't subscribed (user closed
// the page) silently drop — the message is already persisted on the
// session row by the agent loop, so they'll see it on next reload.
type WebChannel struct {
	mu          sync.RWMutex
	subscribers map[string][]chan bus.OutboundMessage
}

// NewWebChannel returns a fresh WebChannel with no subscribers.
func NewWebChannel() *WebChannel {
	return &WebChannel{
		subscribers: make(map[string][]chan bus.OutboundMessage),
	}
}

// Subscribe registers a channel to receive every OutboundMessage whose
// (AgentID, ChatID) matches. Returns the channel and a cleanup func the
// caller MUST defer to remove its slot — without it the slice grows
// unbounded across reconnects.
//
// Buffer size is intentionally small: cron messages arrive at human
// pace, not high frequency, and falling behind is preferable to
// unbounded memory growth on a stuck client. Drops are logged at the
// send site.
func (w *WebChannel) Subscribe(agentID, chatID string) (<-chan bus.OutboundMessage, func()) {
	key := webKey(agentID, chatID)
	ch := make(chan bus.OutboundMessage, 8)
	w.mu.Lock()
	w.subscribers[key] = append(w.subscribers[key], ch)
	w.mu.Unlock()
	cleanup := func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		list := w.subscribers[key]
		for i, c := range list {
			if c == ch {
				w.subscribers[key] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(w.subscribers[key]) == 0 {
			delete(w.subscribers, key)
		}
		close(ch)
	}
	return ch, cleanup
}

// Name returns "web".
func (w *WebChannel) Name() string { return "web" }

// AccountID returns "" — web is a global channel, not per-bot.
func (w *WebChannel) AccountID() string { return "" }

// BotUsername returns "" — n/a for the web channel.
func (w *WebChannel) BotUsername() string { return "" }

// Start blocks until ctx is cancelled. There is no inbound side: web
// chat requests come in through the dashboard SSE / OpenAI-compat
// endpoints, not via this channel.
func (w *WebChannel) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Send is unused for the web channel — outbound deliveries always
// arrive via SendMessage which carries the full OutboundMessage shape.
// Implemented to satisfy the Channel interface; a stray caller would
// have no way to address a specific session.
func (w *WebChannel) Send(chatID, text string) error {
	return w.SendMessage(bus.OutboundMessage{
		Channel: "web",
		ChatID:  chatID,
		Text:    text,
	})
}

// SendMessage fans out msg to every subscriber bound to (msg.AgentID,
// msg.ChatID). Subscribers whose buffer is full are skipped (not
// blocked) so a single stuck client can't stall the cron scheduler.
func (w *WebChannel) SendMessage(msg bus.OutboundMessage) error {
	key := webKey(msg.AgentID, msg.ChatID)
	w.mu.RLock()
	subs := append([]chan bus.OutboundMessage(nil), w.subscribers[key]...)
	w.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
			// buffer full — client is stuck; skip rather than block.
		}
	}
	return nil
}

// SendTyping is a no-op for web — typing indicators are driven by the
// dashboard's own UI state, not by a server signal.
func (w *WebChannel) SendTyping(chatID string) error { return nil }

func webKey(agentID, chatID string) string {
	return agentID + ":" + chatID
}
