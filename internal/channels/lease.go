package channels

import (
	"context"
	"log/slog"
	"time"
)

// Leaser is the cross-process singleton primitive a Manager uses to
// gate polling / persistent-connection adapters (WeChat, Telegram,
// Discord, Slack, Feishu long-conn). Without a leaser, two replicas
// sharing the same bot token would both long-poll the upstream and
// the user would receive every reply twice.
//
// Acquire returns true when the caller (identified by holderID) is now
// the leaseholder for (channel, accountID); false means another live
// holder owns it. Renew returns false when the lease was lost — the
// caller MUST stop polling immediately. Release voluntarily drops the
// row so a peer can take over without waiting for TTL.
type Leaser interface {
	Acquire(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	Renew(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error)
	Release(ctx context.Context, channel, accountID, holderID string) error
}

// Lease lifecycle constants. The TTL is the freshness window the
// leaseholder writes into channel_leases.expires_at; renew fires at
// 1/3 of TTL so two consecutive renewal failures still leave time for
// the third before the row goes stale. Retry is how often a peer
// re-tries Acquire while waiting for the active holder to die.
const (
	leaseTTL           = 30 * time.Second
	leaseRenewInterval = 10 * time.Second
	leaseRetryInterval = 10 * time.Second
)

// NopLeaser is the no-singleton implementation: every Acquire wins,
// Renew always succeeds, Release is a no-op. Used by tests and by
// installs where no leaser is wired (single-instance fallback). Safe
// to share — all methods are stateless.
type NopLeaser struct{}

func (NopLeaser) Acquire(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (NopLeaser) Renew(context.Context, string, string, string, time.Duration) (bool, error) {
	return true, nil
}
func (NopLeaser) Release(context.Context, string, string, string) error { return nil }

// runWithLease wraps a Channel's Start in a cross-instance singleton
// gate. Lifecycle:
//
//  1. Try to Acquire the (channel, accountID) lease. If another
//     instance holds it, sleep and retry until ctx ends or we win.
//  2. While held, spawn ch.Start(childCtx) and a renewal ticker.
//     Renewal failure (lease lost or DB error) cancels childCtx —
//     the Start goroutine exits, we Release locally (best-effort),
//     and we loop back to step 1.
//  3. On parent ctx cancellation we cancel the Start goroutine,
//     wait for it to return, then Release before exiting.
//
// `holderID` is the per-process instance identifier — typically a UUID
// generated once at Manager construction. It MUST be stable across
// renewals of the same process or RenewChannelLease will return false
// on every tick.
func runWithLease(ctx context.Context, ch Channel, leaser Leaser, holderID string) {
	chName := ch.Name()
	accountID := ch.AccountID()
	logCtx := []any{"channel", chName, "account", accountID, "holder", holderID}

	for {
		if ctx.Err() != nil {
			return
		}
		ok, err := leaser.Acquire(ctx, chName, accountID, holderID, leaseTTL)
		if err != nil {
			slog.Warn("channel lease acquire failed, retrying", append(logCtx, "error", err)...)
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
			continue
		}
		if !ok {
			// Another live instance holds the lease — quiet retry. We
			// intentionally don't log here per-tick to avoid spamming
			// the standby replica's logs forever; the holder logs its
			// own "starting channel" once on acquisition.
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
			continue
		}

		slog.Info("channel lease acquired, starting adapter", logCtx...)
		childCtx, childCancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := ch.Start(childCtx); err != nil {
				slog.Error("channel stopped with error", append(logCtx, "error", err)...)
			}
		}()
		renewExit := renewUntilLost(childCtx, leaser, chName, accountID, holderID)

		// Wait for either: parent ctx ends, the adapter exits on its
		// own (e.g. wechat token-expired), or the renew loop reports
		// the lease was lost / errored.
		select {
		case <-ctx.Done():
			childCancel()
			<-done
			// Best-effort release on graceful shutdown so a peer can
			// take over within seconds instead of waiting for TTL.
			if err := leaser.Release(context.Background(), chName, accountID, holderID); err != nil {
				slog.Debug("channel lease release failed on shutdown", append(logCtx, "error", err)...)
			}
			return
		case <-done:
			// Adapter exited on its own. Drop the lease so a peer can
			// pick it up (or it'll re-acquire on the next loop iter if
			// the exit was transient).
			childCancel()
			<-renewExit
			if err := leaser.Release(context.Background(), chName, accountID, holderID); err != nil {
				slog.Debug("channel lease release failed after adapter exit", append(logCtx, "error", err)...)
			}
			// Fall through to the for-loop: next iteration tries to
			// re-acquire and restart. If the adapter exited because
			// of a permanent condition (wechat onExpired callback
			// unregisters the channel from the manager), the manager
			// will eventually tear down this goroutine via ctx.
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
		case <-renewExit:
			// Renewal failed: either the DB rejected us (peer stole
			// the lease — should be impossible under healthy TTLs but
			// guard anyway) or a transient DB error chewed through
			// the full TTL. Either way stop the adapter and re-loop.
			slog.Warn("channel lease lost, stopping adapter", logCtx...)
			childCancel()
			<-done
			if !sleepOrDone(ctx, leaseRetryInterval) {
				return
			}
		}
	}
}

// renewUntilLost ticks every leaseRenewInterval until either ctx ends
// (parent told us to stop) or Renew reports the lease was lost. The
// returned channel closes when the goroutine exits — callers select
// on it to learn when to tear down the adapter.
func renewUntilLost(ctx context.Context, leaser Leaser, channel, accountID, holderID string) <-chan struct{} {
	exit := make(chan struct{})
	go func() {
		defer close(exit)
		ticker := time.NewTicker(leaseRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ok, err := leaser.Renew(ctx, channel, accountID, holderID, leaseTTL)
				if err != nil {
					// Transient DB error: log and keep trying. The
					// lease expires after leaseTTL of no successful
					// renew — at that point a peer steals and our
					// next Renew returns ok=false, hitting the exit
					// branch below.
					slog.Warn("channel lease renew error",
						"channel", channel, "account", accountID, "error", err)
					continue
				}
				if !ok {
					return
				}
			}
		}
	}()
	return exit
}

// sleepOrDone returns true after a full sleep, false if ctx ended
// first. Lets the caller bail out of a retry loop cleanly without
// reaching for a select boilerplate each time.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
