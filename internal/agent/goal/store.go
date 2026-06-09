package goal

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// ErrAlreadyExists is returned by CreateGoal when the (agent,
// session_key) pair already has a goal. Aliased to the underlying
// store error so callers writing errors.Is(err, goal.ErrAlreadyExists)
// catch both the DB layer's native error and any package-local
// shorthand.
var ErrAlreadyExists = store.ErrGoalAlreadyExists

// ErrNotFound is returned by GetGoalBySession when no goal exists for
// the requested (agent, session_key). Aliased to the store error for
// the same reason as ErrAlreadyExists.
var ErrNotFound = store.ErrNotFound

// Store is the narrow persistence interface the slash handlers,
// continuation helper, and accounting hook depend on. It's a subset
// of store.Store; production code wires store.Store in directly
// (DBStore satisfies this implicitly) — the interface exists to keep
// tests testable with an in-memory fake.
type Store interface {
	CreateGoal(ctx context.Context, g *Goal) error
	GetGoalBySession(ctx context.Context, agentID, sessionKey string) (*Goal, error)
	UpdateGoal(ctx context.Context, g *Goal) error
	DeleteGoal(ctx context.Context, goalID string) error
}
