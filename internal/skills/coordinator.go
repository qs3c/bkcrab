package skills

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	learnerSkillLeaseChannel = "learner-skill"
	learnerSkillLeaseTTL     = 30 * time.Second
)

// MutationLeaser is the durable exclusion subset implemented by store.DBStore
// through its existing channel-lease table. The fixed channel plus agent/slug
// account key serializes learner mutations across Pods.
type MutationLeaser interface {
	AcquireChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	RenewChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	ReleaseChannelLease(ctx context.Context, channel, accountID, holderID string) error
}

var (
	ErrLearnerSkillBusy      = errors.New("learner skill mutation is busy")
	ErrLearnerSkillLeaseLost = errors.New("learner skill mutation lease was lost")
)

// LearnerSkillLease is a renewable, cross-Pod learner mutation guard. All
// storage and ledger work performed while holding it must use Context(); a
// failed renewal cancels that context so the stale holder cannot continue a
// write after another Pod has acquired the expired lease.
type LearnerSkillLease struct {
	ctx        context.Context
	cancel     context.CancelCauseFunc
	leaser     MutationLeaser
	baseCtx    context.Context
	account    string
	holder     string
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	mu         sync.Mutex
	leaseErr   error
	releaseErr error
}

func (l *LearnerSkillLease) Context() context.Context {
	if l == nil {
		return context.Background()
	}
	return l.ctx
}

func (l *LearnerSkillLease) setLeaseError(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.leaseErr == nil {
		l.leaseErr = err
	}
}

func (l *LearnerSkillLease) renew() {
	defer close(l.done)
	ticker := time.NewTicker(learnerSkillLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			renewCtx, cancel := context.WithTimeout(context.WithoutCancel(l.baseCtx), learnerSkillLeaseTTL/3)
			ok, err := l.leaser.RenewChannelLease(renewCtx, learnerSkillLeaseChannel, l.account, l.holder, learnerSkillLeaseTTL)
			cancel()
			if err != nil {
				lost := fmt.Errorf("%w: renew failed: %v", ErrLearnerSkillLeaseLost, err)
				l.setLeaseError(lost)
				l.cancel(lost)
				return
			}
			if !ok {
				lost := fmt.Errorf("%w: holder no longer owns agent lease", ErrLearnerSkillLeaseLost)
				l.setLeaseError(lost)
				l.cancel(lost)
				return
			}
		}
	}
}

// Release stops renewal before relinquishing the durable lease. It reports a
// renewal loss even when the best-effort database release itself succeeds.
func (l *LearnerSkillLease) Release() error {
	if l == nil {
		return nil
	}
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		l.cancel(context.Canceled)
		var releaseErr error
		if l.leaser != nil {
			releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(l.baseCtx), 5*time.Second)
			releaseErr = l.leaser.ReleaseChannelLease(releaseCtx, learnerSkillLeaseChannel, l.account, l.holder)
			cancel()
		}
		l.mu.Lock()
		l.releaseErr = errors.Join(l.leaseErr, releaseErr)
		l.mu.Unlock()
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.releaseErr
}

// AcquireLearnerSkillLease acquires the cross-Pod half of learner asset
// coordination. A nil leaser keeps local-only deployments working. Callers
// must invoke the returned release function.
func AcquireLearnerSkillLease(ctx context.Context, leaser MutationLeaser, agentID, slug string) (func() error, error) {
	lease, err := AcquireLearnerSkillLeaseGuard(ctx, leaser, agentID, slug)
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

// AcquireLearnerSkillLeaseGuard is the context-aware form used by production
// mutation paths. The compatibility wrapper above remains for callers that
// only need short-lived exclusion.
func AcquireLearnerSkillLeaseGuard(ctx context.Context, leaser MutationLeaser, agentID, slug string) (*LearnerSkillLease, error) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	if leaser == nil {
		done := make(chan struct{})
		close(done)
		return &LearnerSkillLease{
			ctx: leaseCtx, cancel: cancel, baseCtx: ctx,
			stop: make(chan struct{}), done: done,
		}, nil
	}
	if agentID == "" {
		cancel(context.Canceled)
		return nil, fmt.Errorf("agent id is required for learner skill coordination")
	}
	if err := ValidateSlug(slug); err != nil {
		cancel(context.Canceled)
		return nil, err
	}
	holderBytes := make([]byte, 16)
	if _, err := rand.Read(holderBytes); err != nil {
		cancel(context.Canceled)
		return nil, fmt.Errorf("create learner skill lease holder: %w", err)
	}
	holder := hex.EncodeToString(holderBytes)
	// Use one agent-wide lease. Learner mutations are low-frequency and this
	// lets permanent agent deletion quiesce all slugs with the same primitive,
	// including a create whose slug did not exist when cleanup enumerated state.
	account := agentID
	ok, err := leaser.AcquireChannelLease(ctx, learnerSkillLeaseChannel, account, holder, learnerSkillLeaseTTL)
	if err != nil {
		cancel(context.Canceled)
		return nil, fmt.Errorf("acquire learner skill mutation lease: %w", err)
	}
	if !ok {
		cancel(context.Canceled)
		return nil, fmt.Errorf("%w: learner skill %q; retry the operation", ErrLearnerSkillBusy, slug)
	}
	lease := &LearnerSkillLease{
		ctx: leaseCtx, cancel: cancel, leaser: leaser, baseCtx: ctx,
		account: account, holder: holder,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
	go lease.renew()
	return lease, nil
}

// WaitForLearnerAgentLease retries only ordinary contention until ctx expires.
// Storage errors still fail immediately. It is used by permanent deletion,
// which must wait for an already-running mutation before removing the entire
// namespace.
func WaitForLearnerAgentLease(ctx context.Context, leaser MutationLeaser, agentID string) (func() error, error) {
	lease, err := WaitForLearnerAgentLeaseGuard(ctx, leaser, agentID)
	if err != nil {
		return nil, err
	}
	return lease.Release, nil
}

// WaitForLearnerAgentLeaseGuard waits for contention and returns a renewable
// guard whose Context must be used for the protected operation.
func WaitForLearnerAgentLeaseGuard(ctx context.Context, leaser MutationLeaser, agentID string) (*LearnerSkillLease, error) {
	if leaser == nil {
		return AcquireLearnerSkillLeaseGuard(ctx, nil, agentID, "deletion-lock")
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		lease, err := AcquireLearnerSkillLeaseGuard(ctx, leaser, agentID, "deletion-lock")
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, ErrLearnerSkillBusy) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
