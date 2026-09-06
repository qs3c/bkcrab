package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/session"
)

// ErrTurnActive is returned before accepting a duplicate Web submission.
var ErrTurnActive = errors.New("this session already has an active turn; wait or send a steering message")

type turnAddress struct{ channel, account, chat string }
type turnContextKey struct{}

type TurnState string

const (
	TurnIdle      TurnState = "idle"
	TurnStarting  TurnState = "starting"
	TurnRunning   TurnState = "running"
	TurnFinishing TurnState = "finishing"
	TurnStopping  TurnState = "stopping"
)

// SteerResult distinguishes an idle session from a busy turn closing its inbox.
// Only idle permits the caller to fall back to a normal submission.
type SteerResult struct {
	Buffered bool      `json:"buffered"`
	State    TurnState `json:"state"`
}

type activeTurn struct {
	agent   *Agent
	address turnAddress
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}

	// All mutable fields belong to this reservation and are guarded by turns.mu.
	state         TurnState
	steer         []provider.Message
	session       *session.Session
	projectID     string
	chatterUserID string
}

// turnControl serializes a conversation, while unrelated conversations run freely.
// Reservations stay held until the worker exits, including after cancellation.
type turnControl struct {
	mu     sync.Mutex
	active map[turnAddress]*activeTurn
}

func (a *Agent) acquireTurn(ctx context.Context, key turnAddress, projectID string, wait bool) (context.Context, func(), error) {
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
			turn := &activeTurn{
				agent: a, address: key, ctx: turnCtx, cancel: cancel, done: make(chan struct{}),
				state: TurnStarting, projectID: projectID, chatterUserID: a.ownerUserID,
			}
			if a.turns.active == nil {
				a.turns.active = make(map[turnAddress]*activeTurn)
			}
			a.turns.active[key] = turn
			a.turns.mu.Unlock()
			var once sync.Once
			finish := func() { once.Do(func() { a.finishTurn(turn) }) }
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
	ctx, finish, err := a.acquireTurn(ctx, turnAddress{msg.Channel, msg.AccountID, msg.ChatID}, msg.ProjectID, true)
	if err != nil {
		return nil, nil, err
	}
	turn := ctx.Value(turnContextKey{}).(*activeTurn)
	a.turns.mu.Lock()
	turn.chatterUserID = a.chatterUserID(msg)
	a.turns.mu.Unlock()
	return ctx, finish, nil
}

// bindTurnSession attaches persistence after initialization. The reservation has
// accepted steering since acquisition; loading a Session does not open a second gate.
func (a *Agent) bindTurnSession(ctx context.Context, sess *session.Session) {
	turn := ctx.Value(turnContextKey{}).(*activeTurn)
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	turn.session = sess
	if turn.state == TurnStarting {
		turn.state = TurnRunning
	}
}

// stateLocked also accounts for parent cancellation/deadlines, not just StopWebTurn.
func (t *activeTurn) stateLocked() TurnState {
	if t.ctx.Err() != nil && t.state != TurnFinishing {
		t.state = TurnStopping
	}
	return t.state
}

func (a *Agent) pushTurnSteer(key turnAddress, msg provider.Message) SteerResult {
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	turn := a.turns.active[key]
	if turn == nil {
		return SteerResult{State: TurnIdle}
	}
	state := turn.stateLocked()
	if state != TurnStarting && state != TurnRunning {
		return SteerResult{State: state}
	}
	turn.steer = append(turn.steer, msg)
	return SteerResult{Buffered: true, State: state}
}

// drainTurnSteer transfers inbox ownership to the loop. At its final checkpoint,
// an empty inbox and closing intake happen under the same lock: a racing steer is
// either drained for another iteration or explicitly rejected as finishing.
func (a *Agent) drainTurnSteer(ctx context.Context, finishIfEmpty bool) []provider.Message {
	turn := ctx.Value(turnContextKey{}).(*activeTurn)
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	steer := turn.steer
	turn.steer = nil
	if finishIfEmpty && len(steer) == 0 && turn.stateLocked() != TurnStopping {
		turn.state = TurnFinishing
	}
	return steer
}

// finishTurn closes intake, repairs history and persists any accepted but unused
// steering before releasing the slot. Error, cancellation, plan mode and final
// streaming all use this same cleanup. Persistence never holds turns.mu.
func (a *Agent) finishTurn(turn *activeTurn) {
	a.turns.mu.Lock()
	if turn.stateLocked() != TurnStopping {
		turn.state = TurnFinishing
	}
	leftover, sess := turn.steer, turn.session
	turn.steer = nil
	projectID, chatterUserID := turn.projectID, turn.chatterUserID
	a.turns.mu.Unlock()
	defer func() {
		turn.cancel()
		a.turns.mu.Lock()
		delete(a.turns.active, turn.address)
		close(turn.done)
		a.turns.mu.Unlock()
	}()
	if sess == nil && len(leftover) > 0 {
		// Cancellation or a slash command can exit before the worker loads history.
		key := turn.address
		sess = a.sessions.Get(key.channel, key.account, key.chat, projectID)
		sess.SetChatter(chatterUserID)
	}
	if sess != nil {
		padOrphanToolResults(sess)
		for _, msg := range leftover {
			sess.Append(msg)
		}
	}
	if len(leftover) > 0 {
		slog.Warn("unconsumed steering saved for the next turn", "agent", a.name, "count", len(leftover))
	}
}

func (a *Agent) webTurnAddress(sessionID string) turnAddress {
	if sessionID == "" {
		sessionID = "web-ui"
	}
	channel, account, chat, _ := a.recoverWebTriple(sessionID)
	return turnAddress{channel, account, chat}
}

// ReserveWebTurn rejects overlapping POSTs before they attach to the same SSE hub.
func (a *Agent) ReserveWebTurn(ctx context.Context, sessionID, projectIDHint string) (context.Context, func(), error) {
	if sessionID == "" {
		sessionID = "web-ui"
	}
	channel, account, chat, projectID := a.recoverWebTriple(sessionID)
	if projectID == "" {
		projectID = projectIDHint
	}
	return a.acquireTurn(ctx, turnAddress{channel, account, chat}, projectID, false)
}

func (a *Agent) StopWebTurn(sessionID string) bool {
	key := a.webTurnAddress(sessionID)
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	if turn := a.turns.active[key]; turn != nil {
		turn.state = TurnStopping
		turn.cancel()
		return true
	}
	return false
}

func (a *Agent) WebTurnActive(sessionID string) bool {
	return a.WebTurnState(sessionID) != TurnIdle
}

func (a *Agent) WebTurnState(sessionID string) TurnState {
	key := a.webTurnAddress(sessionID)
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	if turn := a.turns.active[key]; turn != nil {
		return turn.stateLocked()
	}
	return TurnIdle
}
