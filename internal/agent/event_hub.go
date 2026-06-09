package agent

import (
	"context"
	"sync"
)

// EventEnvelope is a ChatEvent stamped with the persistent seq the
// store assigned at append time. Subscribers use Seq to dedup against
// events they've already replayed via ListSessionEventsSince.
type EventEnvelope struct {
	Seq   int64
	Event ChatEvent
}

// EventHub is the in-process pub/sub for live chat events. Subscribers
// (the SSE chat-subscribe handler) register per (userID, agentID,
// sessionKey); publishers (emitEvent on the agent loop, fanned out to
// the hub) push envelopes that include the persisted seq so reconnect
// resume can stitch back together cleanly.
//
// In-memory only — multi-pod deploys need to swap this for redis
// pub/sub or similar (same shape as the WebChannel limitation called
// out elsewhere).
type EventHub struct {
	mu   sync.RWMutex
	subs map[string][]chan EventEnvelope
}

// NewEventHub returns an empty hub.
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[string][]chan EventEnvelope)}
}

// Subscribe registers a buffered channel for one (user, agent,
// session) tuple. The cleanup func MUST be deferred — without it the
// hub leaks goroutines and channels on reconnect churn.
func (h *EventHub) Subscribe(userID, agentID, sessionKey string) (<-chan EventEnvelope, func()) {
	key := hubKey(userID, agentID, sessionKey)
	ch := make(chan EventEnvelope, 32)
	h.mu.Lock()
	h.subs[key] = append(h.subs[key], ch)
	h.mu.Unlock()
	cleanup := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[key]
		for i, c := range list {
			if c == ch {
				h.subs[key] = append(list[:i], list[i+1:]...)
				close(ch)
				break
			}
		}
		if len(h.subs[key]) == 0 {
			delete(h.subs, key)
		}
	}
	return ch, cleanup
}

// Publish fans an envelope out to every current subscriber. Slow
// consumers (full buffer) are skipped, not blocked — a stuck client
// can't stall the agent loop.
func (h *EventHub) Publish(userID, agentID, sessionKey string, env EventEnvelope) {
	key := hubKey(userID, agentID, sessionKey)
	h.mu.RLock()
	subs := append([]chan EventEnvelope(nil), h.subs[key]...)
	h.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- env:
		default:
		}
	}
}

func hubKey(userID, agentID, sessionKey string) string {
	return userID + "/" + agentID + "/" + sessionKey
}

// EventSink is the persistence side of the chat-events pipeline. The
// store.Store interface's AppendSessionEvent satisfies this exactly, so
// the gateway can pass its store as-is.
type EventSink interface {
	AppendSessionEvent(ctx context.Context, userID, agentID, sessionKey, eventType string, data []byte) (int64, error)
}

// streamCtx carries the per-turn handles emitEvent reaches for:
// the legacy in-memory ChatEvent channel (consumed by handleChatStream
// while the client is connected), the persistent sink, the hub, and
// the address keys (userID, agentID, sessionKey) — these last three
// can't be derived from the agent struct because the agent runs on
// behalf of the chatter, not its owner.
type streamCtx struct {
	channel    chan<- ChatEvent
	sink       EventSink
	hub        *EventHub
	userID     string
	agentID    string
	sessionKey string
}

type streamCtxKey struct{}

// ContextWithStream attaches the streaming pipeline to ctx. emitEvent
// reads it and persists / publishes / forwards to the legacy channel
// in one place.
func ContextWithStream(ctx context.Context, channel chan<- ChatEvent, sink EventSink, hub *EventHub, userID, agentID, sessionKey string) context.Context {
	return context.WithValue(ctx, streamCtxKey{}, &streamCtx{
		channel:    channel,
		sink:       sink,
		hub:        hub,
		userID:     userID,
		agentID:    agentID,
		sessionKey: sessionKey,
	})
}

func streamFromContext(ctx context.Context) *streamCtx {
	s, _ := ctx.Value(streamCtxKey{}).(*streamCtx)
	return s
}
