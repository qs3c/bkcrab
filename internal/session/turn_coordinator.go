package session

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/qs3c/bkclaw/internal/provider"
)

// TurnKey identifies the conversation turn state shared by normal turn handling
// and steer entrypoints.
type TurnKey struct {
	UserID     string
	AgentID    string
	SessionKey string
}

func (k TurnKey) String() string {
	return k.UserID + "\x00" + k.AgentID + "\x00" + k.SessionKey
}

// TurnLease is held by the worker that owns the current turn.
type TurnLease struct {
	Key    TurnKey
	TurnID string
	Token  string

	lastSteerID string
	recordValue string
	stopRenew   context.CancelFunc
}

// TurnCoordinator owns the active-turn marker and steer buffer for a session.
// A local implementation preserves single-process behavior; a Redis-backed
// implementation lets any pod accept steer messages for the active owner.
type TurnCoordinator interface {
	BeginTurn(ctx context.Context, key TurnKey) (*TurnLease, bool, error)
	PushSteer(ctx context.Context, key TurnKey, msg provider.Message) (bool, error)
	DrainSteer(ctx context.Context, lease *TurnLease) ([]provider.Message, error)
	EndTurn(ctx context.Context, lease *TurnLease) ([]provider.Message, error)
	Close() error
}

type localTurnState struct {
	depth  int
	turnID string
	token  string
	buf    []provider.Message
}

// LocalTurnCoordinator stores active turn state in process memory.
type LocalTurnCoordinator struct {
	mu     sync.Mutex
	states map[string]*localTurnState
}

func NewLocalTurnCoordinator() *LocalTurnCoordinator {
	return &LocalTurnCoordinator{states: make(map[string]*localTurnState)}
}

func (c *LocalTurnCoordinator) BeginTurn(ctx context.Context, key TurnKey) (*TurnLease, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	mapKey := key.String()
	st := c.states[mapKey]
	if st == nil {
		st = &localTurnState{
			turnID: uuid.NewString(),
			token:  uuid.NewString(),
		}
		c.states[mapKey] = st
	}
	st.depth++
	return &TurnLease{Key: key, TurnID: st.turnID, Token: st.token}, true, nil
}

func (c *LocalTurnCoordinator) PushSteer(ctx context.Context, key TurnKey, msg provider.Message) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.states[key.String()]
	if st == nil || st.depth == 0 {
		return false, nil
	}
	st.buf = append(st.buf, msg)
	return true, nil
}

func (c *LocalTurnCoordinator) DrainSteer(ctx context.Context, lease *TurnLease) ([]provider.Message, error) {
	if lease == nil {
		return nil, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.states[lease.Key.String()]
	if st == nil || len(st.buf) == 0 {
		return nil, nil
	}
	out := append([]provider.Message(nil), st.buf...)
	st.buf = nil
	return out, nil
}

func (c *LocalTurnCoordinator) EndTurn(ctx context.Context, lease *TurnLease) ([]provider.Message, error) {
	if lease == nil {
		return nil, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	mapKey := lease.Key.String()
	st := c.states[mapKey]
	if st == nil {
		return nil, nil
	}
	if st.depth > 0 {
		st.depth--
	}
	if st.depth > 0 {
		return nil, nil
	}
	out := append([]provider.Message(nil), st.buf...)
	delete(c.states, mapKey)
	return out, nil
}

func (c *LocalTurnCoordinator) Close() error { return nil }

// TurnKey returns the stable key used by TurnCoordinator implementations.
func (s *Session) TurnKey() TurnKey {
	return TurnKey{UserID: s.userID, AgentID: s.agentID, SessionKey: s.sessionKey}
}
