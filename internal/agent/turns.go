package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/qs3c/bkcrab/internal/bus"
)

// ErrTurnActive is returned before accepting a duplicate Web submission.
var ErrTurnActive = errors.New("this session already has an active turn; wait or send a steering message")

type turnAddress struct{ channel, account, chat string }
type turnContextKey struct{}
type activeTurn struct {
	agent   *Agent
	address turnAddress
	cancel  context.CancelFunc
	done    chan struct{}
}

// turnControl serializes a conversation, while unrelated conversations run freely.
// Reservations stay held until the worker exits, including after cancellation.
type turnControl struct {
	mu     sync.Mutex
	active map[turnAddress]*activeTurn
}

func (a *Agent) acquireTurn(ctx context.Context, key turnAddress, wait bool) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if current, ok := ctx.Value(turnContextKey{}).(*activeTurn); ok && current.agent == a && current.address == key {
		a.turns.mu.Lock()
		reserved := a.turns.active[key] == current
		a.turns.mu.Unlock()
		if reserved {
			return ctx, func() {}, nil
		} // HTTP reserved this turn before subscribing.
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		a.turns.mu.Lock()
		current := a.turns.active[key]
		if current == nil {
			turnCtx, cancel := context.WithCancel(ctx)
			turn := &activeTurn{agent: a, address: key, cancel: cancel, done: make(chan struct{})}
			if a.turns.active == nil {
				a.turns.active = make(map[turnAddress]*activeTurn)
			}
			a.turns.active[key] = turn
			a.turns.mu.Unlock()
			var once sync.Once
			finish := func() {
				once.Do(func() {
					cancel()
					a.turns.mu.Lock()
					delete(a.turns.active, key)
					close(turn.done)
					a.turns.mu.Unlock()
				})
			}
			return context.WithValue(turnCtx, turnContextKey{}, turn), finish, nil
		}
		a.turns.mu.Unlock()
		if !wait {
			return nil, nil, ErrTurnActive
		}
		select {
		case <-current.done:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
}

func (a *Agent) messageTurn(ctx context.Context, msg bus.InboundMessage) (context.Context, func(), error) {
	return a.acquireTurn(ctx, turnAddress{msg.Channel, msg.AccountID, msg.ChatID}, true)
}

func (a *Agent) webTurnAddress(sessionID string) turnAddress {
	if sessionID == "" {
		sessionID = "web-ui"
	}
	channel, account, chat, _ := a.recoverWebTriple(sessionID)
	return turnAddress{channel, account, chat}
}

// ReserveWebTurn rejects overlapping POSTs before they attach to the same SSE hub.
func (a *Agent) ReserveWebTurn(ctx context.Context, sessionID string) (context.Context, func(), error) {
	return a.acquireTurn(ctx, a.webTurnAddress(sessionID), false)
}

func (a *Agent) StopWebTurn(sessionID string) bool {
	key := a.webTurnAddress(sessionID)
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	if turn := a.turns.active[key]; turn != nil {
		turn.cancel()
		return true
	}
	return false
}

func (a *Agent) WebTurnActive(sessionID string) bool {
	key := a.webTurnAddress(sessionID)
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	return a.turns.active[key] != nil
}
