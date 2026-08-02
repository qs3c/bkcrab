package fairqueue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	defaultRecoveryLockTTL    = 30 * time.Second
	defaultRecoveryCleanup    = 250 * time.Millisecond
	defaultRecoveryOperation  = 5 * time.Second
	defaultRecoveryBackoff    = 100 * time.Millisecond
	defaultRecoveryBackoffMax = 5 * time.Second
)

// RecoveryRuntime is the local admission boundary used by recovery. Closing a
// resource prevents new scheduler and publisher attempts, while the drain
// deliberately excludes long-running business tasks.
type RecoveryRuntime interface {
	CloseResource(resource string) error
	WaitForAttemptDrain(ctx context.Context, resource string) error
	OpenResource(resource string, fence ResourceFence) error
}

// RecoveryOptions contains process-local recovery timings. Page sizes,
// attempt deadlines, drain bounds, and reconciliation cadence remain resource
// policy and are therefore read from ResourceConfig.
type RecoveryOptions struct {
	LockTTL           time.Duration
	LockRenewInterval time.Duration
	CleanupInterval   time.Duration
	OperationTimeout  time.Duration
	BackoffInitial    time.Duration
	BackoffMax        time.Duration
}

func (o RecoveryOptions) withDefaults() (RecoveryOptions, error) {
	if o.LockTTL == 0 {
		o.LockTTL = defaultRecoveryLockTTL
	}
	if o.LockRenewInterval == 0 {
		o.LockRenewInterval = o.LockTTL / 3
	}
	if o.CleanupInterval == 0 {
		o.CleanupInterval = defaultRecoveryCleanup
	}
	if o.OperationTimeout == 0 {
		o.OperationTimeout = defaultRecoveryOperation
	}
	if o.BackoffInitial == 0 {
		o.BackoffInitial = defaultRecoveryBackoff
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = defaultRecoveryBackoffMax
	}
	values := []struct {
		name  string
		value time.Duration
	}{
		{"lock TTL", o.LockTTL},
		{"lock renew interval", o.LockRenewInterval},
		{"cleanup interval", o.CleanupInterval},
		{"operation timeout", o.OperationTimeout},
		{"initial backoff", o.BackoffInitial},
		{"maximum backoff", o.BackoffMax},
	}
	for _, value := range values {
		if value.value <= 0 || value.value > maxResourceDuration {
			return RecoveryOptions{}, fmt.Errorf("fairqueue: recovery %s must be in (0,%s]", value.name, maxResourceDuration)
		}
	}
	if o.LockRenewInterval >= o.LockTTL {
		return RecoveryOptions{}, errors.New("fairqueue: recovery lock renew interval must be shorter than lock TTL")
	}
	if o.BackoffInitial > o.BackoffMax {
		return RecoveryOptions{}, errors.New("fairqueue: recovery initial backoff exceeds maximum backoff")
	}
	return o, nil
}

// RecoveryCoordinator owns the generic Redis rebuild and canonical
// reconciliation protocol. It never interprets domain guards and never owns
// the shared Rabbit or Redis client lifecycle.
type RecoveryCoordinator struct {
	coordinator Coordinator
	rabbit      RabbitClient
	runtime     RecoveryRuntime
	options     RecoveryOptions
	tokens      runtimeTokenSource
}

func NewRecoveryCoordinator(
	coordinator Coordinator,
	rabbit RabbitClient,
	runtime RecoveryRuntime,
	options RecoveryOptions,
) (*RecoveryCoordinator, error) {
	if coordinator == nil {
		return nil, errors.New("fairqueue: recovery coordinator is required")
	}
	if rabbit == nil {
		return nil, errors.New("fairqueue: recovery Rabbit client is required")
	}
	normalized, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &RecoveryCoordinator{
		coordinator: coordinator,
		rabbit:      rabbit,
		runtime:     runtime,
		options:     normalized,
		tokens:      cryptoRuntimeTokenSource{},
	}, nil
}

// Run is the long-lived per-resource startup barrier and READY reconciliation
// loop. Ordinary dependency failures leave both local gates closed and retry;
// safety failures and operator-owned special recovery return to the Runtime.
func (c *RecoveryCoordinator) Run(
	ctx context.Context,
	config ResourceConfig,
	writer string,
	source RecoverySource,
	journal OperationJournal,
) error {
	if ctx == nil {
		return errors.New("fairqueue: nil recovery context")
	}
	if err := c.validateBarrierInputs(config, writer, source, journal); err != nil {
		return err
	}
	defer func() { _ = c.runtime.CloseResource(config.Key) }()

	backoff := c.options.BackoffInitial
	for {
		fence, err := c.EnsureResourceReady(ctx, config, writer, source, journal)
		if err != nil {
			_ = c.runtime.CloseResource(config.Key)
			if recoveryTerminalError(err) || ctx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			if err := waitRecovery(ctx, backoff); err != nil {
				return err
			}
			backoff = growRecoveryBackoff(backoff, c.options.BackoffMax)
			continue
		}

		backoff = c.options.BackoffInitial
		timer := time.NewTimer(config.ReconcileInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
		if err := c.ReconcileReady(ctx, config, fence, source); err != nil {
			_ = c.runtime.CloseResource(config.Key)
			if recoveryTerminalError(err) {
				return err
			}
		}
	}
}

func recoveryTerminalError(err error) bool {
	return errors.Is(err, ErrAuthoritativeWriterMismatch) ||
		errors.Is(err, ErrUnsupportedTopology) ||
		errors.Is(err, ErrRecoveryOperatorRequired)
}

func growRecoveryBackoff(value, maximum time.Duration) time.Duration {
	if value >= maximum || value > maximum/2 {
		return maximum
	}
	return value * 2
}

func waitRecovery(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *RecoveryCoordinator) validateBarrierInputs(
	config ResourceConfig,
	writer string,
	source RecoverySource,
	journal OperationJournal,
) error {
	if c == nil || c.coordinator == nil || c.rabbit == nil {
		return errors.New("fairqueue: invalid recovery coordinator")
	}
	if c.runtime == nil {
		return errors.New("fairqueue: recovery runtime is required for startup barrier")
	}
	if err := config.Validate(); err != nil {
		return fmt.Errorf("fairqueue: recovery resource config: %w", err)
	}
	if err := (WriterIdentity{Fingerprint: writer}).Validate(); err != nil {
		return fmt.Errorf("fairqueue: recovery writer: %w", err)
	}
	if source == nil || journal == nil {
		return errors.New("fairqueue: recovery source and operation journal are required")
	}
	return nil
}

// EnsureResourceReady is a single startup/reconnect barrier. A caller that
// wants health-transition retries should use Run.
func (c *RecoveryCoordinator) EnsureResourceReady(
	ctx context.Context,
	config ResourceConfig,
	writer string,
	source RecoverySource,
	journal OperationJournal,
) (ResourceFence, error) {
	if ctx == nil {
		return ResourceFence{}, errors.New("fairqueue: nil recovery context")
	}
	if err := c.validateBarrierInputs(config, writer, source, journal); err != nil {
		return ResourceFence{}, err
	}
	if err := c.runtime.CloseResource(config.Key); err != nil {
		return ResourceFence{}, err
	}

	record, present, err := c.readJournal(ctx, journal, config.Key, writer)
	if err != nil {
		return ResourceFence{}, err
	}
	if present && record.Phase == OperationActive {
		return ResourceFence{}, operatorRequired("special recovery journal is ACTIVE")
	}

	readyFence, observeErr := c.observeReady(ctx, config.Key, writer)
	if observeErr == nil {
		if present && record.Phase == OperationReadyCommitted {
			if err := c.reconcileReadyCommitted(ctx, journal, record, writer); err != nil {
				return ResourceFence{}, err
			}
		}
		if err := c.ReconcileReady(ctx, config, readyFence, source); err == nil {
			if err := c.runtime.OpenResource(config.Key, readyFence); err != nil {
				return ResourceFence{}, err
			}
			return readyFence, nil
		} else if !errors.Is(err, ErrCoordinationCorrupt) {
			return ResourceFence{}, err
		}
	} else {
		if present && record.Phase == OperationReadyCommitted {
			return ResourceFence{}, operatorRequired("READY_COMMITTED journal has no matching READY control")
		}
		if !errors.Is(observeErr, ErrResourceNotReady) &&
			!errors.Is(observeErr, ErrFenceMismatch) &&
			!errors.Is(observeErr, ErrCoordinationCorrupt) {
			return ResourceFence{}, observeErr
		}
	}

	recoveryFence, err := c.beginNormalRecovery(ctx, config.Key, writer, journal)
	if err != nil {
		return ResourceFence{}, err
	}
	if err := c.RunRecovery(ctx, config, recoveryFence, source); err != nil {
		return ResourceFence{}, err
	}
	readyFence, err = c.FinishRecovery(ctx, config, recoveryFence)
	if err != nil {
		return ResourceFence{}, err
	}
	if err := c.ReconcileReady(ctx, config, readyFence, source); err != nil {
		return ResourceFence{}, err
	}
	if err := c.runtime.OpenResource(config.Key, readyFence); err != nil {
		return ResourceFence{}, err
	}
	return readyFence, nil
}

func operatorRequired(reason string) error {
	return errors.Join(ErrRecoveryOperatorRequired, ErrResourceNotReady,
		fmt.Errorf("fairqueue: %s", reason))
}

func (c *RecoveryCoordinator) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, c.options.OperationTimeout)
}

func (c *RecoveryCoordinator) readJournal(
	ctx context.Context,
	journal OperationJournal,
	resource, writer string,
) (RecoveryOperationRecord, bool, error) {
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	record, present, err := journal.Read(opCtx, resource, writer)
	if err != nil {
		return RecoveryOperationRecord{}, false, err
	}
	if !present {
		return RecoveryOperationRecord{}, false, nil
	}
	if err := record.ValidatePersisted(); err != nil || record.Resource != resource {
		return RecoveryOperationRecord{}, false, errors.Join(ErrCoordinationCorrupt, ErrInvalidOperationRecord)
	}
	if record.CurrentWriterFingerprint != writer {
		return RecoveryOperationRecord{}, false, ErrAuthoritativeWriterMismatch
	}
	return record, true, nil
}

func (c *RecoveryCoordinator) observeReady(ctx context.Context, resource, writer string) (ResourceFence, error) {
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	return c.coordinator.ObserveReadyFence(opCtx, resource, writer)
}

func (c *RecoveryCoordinator) reconcileReadyCommitted(
	ctx context.Context,
	journal OperationJournal,
	record RecoveryOperationRecord,
	writer string,
) error {
	var exact bool
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	err := journal.WithStartFence(opCtx, record.Resource, writer, func(session OperationStartSession) error {
		current, present, err := session.Read(opCtx)
		if err != nil {
			return err
		}
		if !present || current.ValidatePersisted() != nil || current.OperationID != record.OperationID ||
			current.Kind != record.Kind || current.Phase != OperationReadyCommitted ||
			current.CurrentWriterFingerprint != writer {
			return operatorRequired("READY_COMMITTED journal changed during terminal reconciliation")
		}
		owner, err := c.tokens.Next()
		if err != nil {
			return fmt.Errorf("fairqueue: generate recovery owner: %w", err)
		}
		lock, err := c.acquireRawLock(opCtx, record.Resource, owner)
		if err != nil {
			return err
		}
		defer func() { _ = c.releaseRawLock(context.WithoutCancel(opCtx), record.Resource, lock) }()
		snapshot, err := c.inspectRecoveryStart(opCtx, record.Resource, lock)
		if err != nil {
			return err
		}
		exact = snapshot.Present && snapshot.State == ResourceReady &&
			snapshot.WriterFingerprint == writer &&
			snapshot.LastCompletedOperationID == record.OperationID
		if !exact {
			return operatorRequired("READY_COMMITTED journal does not match Redis terminal control")
		}
		if err := c.checkRawLock(opCtx, record.Resource, lock); err != nil {
			return err
		}
		completed, err := journal.Complete(opCtx, current)
		if err != nil {
			return err
		}
		if err := completed.ValidatePersisted(); err != nil || completed.Phase != OperationCompleted ||
			completed.OperationID != current.OperationID {
			return errors.Join(ErrCoordinationCorrupt, ErrInvalidOperationRecord)
		}
		return c.checkRawLock(opCtx, record.Resource, lock)
	})
	if err != nil {
		return err
	}
	if !exact {
		return operatorRequired("READY_COMMITTED terminal reconciliation was not proven")
	}
	return nil
}

func (c *RecoveryCoordinator) beginNormalRecovery(
	ctx context.Context,
	resource, writer string,
	journal OperationJournal,
) (RecoveryFence, error) {
	owner, err := c.tokens.Next()
	if err != nil || !lowerHex32Pattern.MatchString(owner) {
		return RecoveryFence{}, fmt.Errorf("%w: invalid recovery owner source", ErrCoordinationCorrupt)
	}
	var fence RecoveryFence
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	err = journal.WithStartFence(opCtx, resource, writer, func(session OperationStartSession) error {
		record, present, err := session.Read(opCtx)
		if err != nil {
			return err
		}
		if present {
			if err := record.ValidatePersisted(); err != nil || record.Resource != resource {
				return errors.Join(ErrCoordinationCorrupt, ErrInvalidOperationRecord)
			}
			if record.CurrentWriterFingerprint != writer {
				return ErrAuthoritativeWriterMismatch
			}
			if record.Phase != OperationCompleted {
				return operatorRequired("unfinished special recovery blocks NORMAL")
			}
		}

		lock, err := c.acquireRawLock(opCtx, resource, owner)
		if err != nil {
			return err
		}
		begun := false
		defer func() {
			if !begun {
				_ = c.releaseRawLock(context.WithoutCancel(opCtx), resource, lock)
			}
		}()
		snapshot, err := c.inspectRecoveryStart(opCtx, resource, lock)
		if err != nil {
			return err
		}
		if snapshot.Present {
			if snapshot.WriterFingerprint != writer {
				return ErrAuthoritativeWriterMismatch
			}
			if snapshot.State == ResourceRecovering && snapshot.Kind != RecoveryNormal {
				return operatorRequired("special Redis recovery blocks NORMAL")
			}
		}
		// Re-read the authoritative journal while still holding the MySQL start
		// fence and raw Redis lock. A special intent cannot slip between this
		// check and Begin.
		recheck, recheckPresent, err := session.Read(opCtx)
		if err != nil {
			return err
		}
		if recheckPresent && (recheck.ValidatePersisted() != nil ||
			recheck.CurrentWriterFingerprint != writer || recheck.Phase != OperationCompleted) {
			return operatorRequired("special recovery appeared during NORMAL start")
		}
		if err := c.renewRawLock(opCtx, resource, lock); err != nil {
			return err
		}
		if err := c.checkRawLock(opCtx, resource, lock); err != nil {
			return err
		}
		fence, err = c.coordinator.BeginRecoveryWithLock(opCtx, resource, writer, lock, c.options.LockTTL)
		if err != nil {
			return err
		}
		begun = true
		return nil
	})
	if err != nil {
		return RecoveryFence{}, err
	}
	if err := fence.Validate(); err != nil || fence.Kind != RecoveryNormal ||
		fence.WriterFingerprint != writer {
		return RecoveryFence{}, errors.Join(ErrCoordinationCorrupt, ErrInvalidRecoveryState)
	}
	return fence, nil
}

func (c *RecoveryCoordinator) acquireRawLock(ctx context.Context, resource, owner string) (RecoveryLock, error) {
	return c.coordinator.AcquireRecoveryLock(ctx, resource, owner, c.options.LockTTL)
}

func (c *RecoveryCoordinator) renewRawLock(ctx context.Context, resource string, lock RecoveryLock) error {
	return c.coordinator.RenewRecoveryLock(ctx, resource, lock, c.options.LockTTL)
}

func (c *RecoveryCoordinator) checkRawLock(ctx context.Context, resource string, lock RecoveryLock) error {
	return c.coordinator.CheckRecoveryLock(ctx, resource, lock)
}

func (c *RecoveryCoordinator) releaseRawLock(ctx context.Context, resource string, lock RecoveryLock) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, c.options.OperationTimeout)
	defer cancel()
	return c.coordinator.ReleaseRecoveryLock(cleanupCtx, resource, lock)
}

func (c *RecoveryCoordinator) inspectRecoveryStart(
	ctx context.Context,
	resource string,
	lock RecoveryLock,
) (RecoveryControlSnapshot, error) {
	snapshot, err := c.coordinator.InspectRecoveryStart(ctx, resource, lock)
	// InspectRecoveryStart already owns and validates the raw lock. At this
	// boundary its FENCE result therefore denotes an incompatible persisted
	// protocol, not an epoch race; NORMAL must never overwrite it.
	if errors.Is(err, ErrFenceMismatch) {
		return RecoveryControlSnapshot{}, errors.Join(ErrUnsupportedTopology, err)
	}
	return snapshot, err
}

// DrainAttempts closes both local admission gates, waits for their registries,
// and also waits at least one complete distributed publish-attempt deadline.
// It is exported so special recovery operators use the same ordering.
func (c *RecoveryCoordinator) DrainAttempts(ctx context.Context, config ResourceConfig) error {
	if c == nil || ctx == nil {
		return errors.New("fairqueue: invalid recovery attempt drain")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	started := time.Now()
	drainCtx, cancel := context.WithTimeout(ctx, config.RecoveryDrainTimeout)
	defer cancel()
	if c.runtime != nil {
		if err := c.runtime.CloseResource(config.Key); err != nil {
			return err
		}
		if err := c.runtime.WaitForAttemptDrain(drainCtx, config.Key); err != nil {
			return err
		}
	}
	remaining := config.PublishAttemptTimeout - time.Since(started)
	if remaining > 0 {
		if err := waitRecovery(drainCtx, remaining); err != nil {
			return err
		}
	}
	return nil
}

// RunRecovery performs the common high-water rebuild for an already-created
// NORMAL or special recovery fence. It deliberately does not Finish Redis and
// does not mutate the authoritative special-operation journal.
func (c *RecoveryCoordinator) RunRecovery(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
) error {
	if err := c.validateRecoveryRun(ctx, config, fence, source); err != nil {
		return err
	}
	return c.withRecoveryRenewal(ctx, config.Key, fence, func(runCtx context.Context) error {
		if err := c.DrainAttempts(runCtx, config); err != nil {
			return err
		}
		if err := c.drainRecoveryLeases(runCtx, config, fence); err != nil {
			return err
		}

		snapshot, err := c.inspectRecoveryFence(runCtx, config.Key, fence)
		if err != nil {
			return err
		}
		highWater := snapshot.Progress.HighWater
		if highWater == "" {
			highWater, err = c.captureRecoveryHighWater(runCtx, config.Key, fence, source)
			if err != nil {
				return err
			}
			if err := c.recoveryMutation(runCtx, config.Key, fence, func(opCtx context.Context) error {
				return c.coordinator.SetRecoveryHighWater(opCtx, config.Key, fence, highWater)
			}); err != nil {
				return err
			}
		}
		if err := ValidateHighWater(highWater); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		if err := c.recoveryMutation(runCtx, config.Key, fence, func(opCtx context.Context) error {
			return c.coordinator.ResetResource(opCtx, config.Key, fence)
		}); err != nil {
			return err
		}
		if err := c.deleteOwnedResourceKeys(runCtx, config, fence); err != nil {
			return err
		}

		cycle := uint64(1)
		if err := c.restoreKnownRecovery(runCtx, config, fence, source, highWater, cycle); err != nil {
			return err
		}
		if err := c.restoreRunningRecovery(runCtx, config, fence, source, highWater); err != nil {
			return err
		}
		if err := c.restoreDispatchedRecovery(runCtx, config, fence, source, highWater, cycle); err != nil {
			return err
		}

		for {
			diff, err := c.reconcileRecoveryStable(runCtx, config, fence, source, highWater)
			if err != nil {
				return err
			}
			if err := c.recoveryMutation(runCtx, config.Key, fence, func(opCtx context.Context) error {
				return c.coordinator.MarkRecoveryPass(opCtx, config.Key, fence,
					RecoveryPassRunning, cycle, true, diff)
			}); err != nil {
				return err
			}
			if diff == 0 {
				break
			}
			if cycle == math.MaxUint64 {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: recovery cycle overflow"))
			}
			cycle++
		}

		// A final canonical topology/ready-depth pass closes races with the
		// stable identity convergence cycle.
		if cycle == math.MaxUint64 {
			return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: recovery cycle overflow"))
		}
		cycle++
		if err := c.restoreKnownRecovery(runCtx, config, fence, source, highWater, cycle); err != nil {
			return err
		}
		return c.restoreDispatchedRecovery(runCtx, config, fence, source, highWater, cycle)
	})
}

func (c *RecoveryCoordinator) validateRecoveryRun(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
) error {
	if c == nil || c.coordinator == nil || c.rabbit == nil || ctx == nil {
		return errors.New("fairqueue: invalid recovery run")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	if err := fence.Validate(); err != nil || fence.Kind == RecoveryNone || source == nil {
		return fmt.Errorf("%w: invalid common recovery inputs", ErrInvalidModel)
	}
	return nil
}

func (c *RecoveryCoordinator) withRecoveryRenewal(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	fn func(context.Context) error,
) error {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if err := c.renewRecovery(runCtx, resource, fence); err != nil {
		return err
	}
	renewDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(c.options.LockRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				renewDone <- nil
				return
			case <-ticker.C:
				if err := c.renewRecovery(runCtx, resource, fence); err != nil {
					cancel(err)
					renewDone <- err
					return
				}
			}
		}
	}()
	err := fn(runCtx)
	cancel(err)
	renewErr := <-renewDone
	if renewErr != nil {
		return renewErr
	}
	return err
}

func (c *RecoveryCoordinator) renewRecovery(ctx context.Context, resource string, fence RecoveryFence) error {
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	return c.coordinator.RenewRecovery(opCtx, resource, fence, c.options.LockTTL)
}

func (c *RecoveryCoordinator) recoveryMutation(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	fn func(context.Context) error,
) error {
	if err := c.renewRecovery(ctx, resource, fence); err != nil {
		return err
	}
	opCtx, cancel := c.operationContext(ctx)
	err := fn(opCtx)
	cancel()
	if err != nil {
		return err
	}
	return c.renewRecovery(ctx, resource, fence)
}

func (c *RecoveryCoordinator) inspectRecoveryFence(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
) (RecoveryControlSnapshot, error) {
	if err := c.renewRecovery(ctx, resource, fence); err != nil {
		return RecoveryControlSnapshot{}, err
	}
	opCtx, cancel := c.operationContext(ctx)
	snapshot, err := c.coordinator.InspectRecoveryStart(opCtx, resource,
		RecoveryLock{OwnerToken: fence.OwnerToken})
	cancel()
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	if !snapshot.Present || snapshot.State != ResourceRecovering || snapshot.Epoch != fence.Epoch ||
		snapshot.WriterFingerprint != fence.WriterFingerprint || snapshot.Kind != fence.Kind ||
		snapshot.OperationID != fence.OperationID || snapshot.Progress == nil {
		return RecoveryControlSnapshot{}, ErrFenceMismatch
	}
	if err := c.renewRecovery(ctx, resource, fence); err != nil {
		return RecoveryControlSnapshot{}, err
	}
	return snapshot, nil
}

func (c *RecoveryCoordinator) drainRecoveryLeases(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
) error {
	drainCtx, cancel := context.WithTimeout(ctx, config.RecoveryDrainTimeout)
	defer cancel()
	for {
		var result RecoveryCleanupResult
		err := c.recoveryMutation(drainCtx, config.Key, fence, func(opCtx context.Context) error {
			var err error
			result, err = c.coordinator.RecoveryReapExpired(opCtx, config.Key, fence, config.ReconcilePageSize)
			return err
		})
		if err != nil {
			return err
		}
		if err := result.Validate(); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		if result.RemainingProvisionals == 0 && result.RemainingTurns == 0 {
			return nil
		}
		if err := waitRecovery(drainCtx, c.options.CleanupInterval); err != nil {
			return errors.Join(ErrResourceNotReady, err)
		}
	}
}

func (c *RecoveryCoordinator) captureRecoveryHighWater(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	source RecoverySource,
) (string, error) {
	if err := c.renewRecovery(ctx, resource, fence); err != nil {
		return "", err
	}
	opCtx, cancel := c.operationContext(ctx)
	highWater, err := source.CaptureHighWater(opCtx)
	cancel()
	if err != nil {
		return "", err
	}
	if err := ValidateHighWater(highWater); err != nil {
		return "", errors.Join(ErrCoordinationCorrupt, err)
	}
	if err := c.renewRecovery(ctx, resource, fence); err != nil {
		return "", err
	}
	return highWater, nil
}

func (c *RecoveryCoordinator) deleteOwnedResourceKeys(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
) error {
	after := ""
	for {
		if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
			return err
		}
		opCtx, cancel := c.operationContext(ctx)
		page, err := c.coordinator.ListOwnedResourceKeys(opCtx, config.Key, fence, after, config.ReconcilePageSize)
		cancel()
		if err != nil {
			return err
		}
		if err := page.Validate(after, config.ReconcilePageSize); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
			return c.coordinator.DeleteOwnedResourceKeys(opCtx, config.Key, fence, page.Items)
		}); err != nil {
			return err
		}
		if page.Done {
			return nil
		}
		after = page.NextCursor
	}
}

func (c *RecoveryCoordinator) restoreKnownRecovery(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater string,
	cycle uint64,
) error {
	after := ""
	for {
		page, err := c.listKnownRecoveryPage(ctx, config, fence, source, highWater, after)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := c.recoveryRabbitCall(ctx, config.Key, fence, func(opCtx context.Context) error {
				return c.rabbit.EnsureTenantTopology(opCtx, config.Key, item.TenantID)
			}); err != nil {
				return err
			}
			if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
				return c.coordinator.RestoreKnownTenant(opCtx, config.Key, fence, item.TenantID)
			}); err != nil {
				return err
			}
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}
	return c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
		return c.coordinator.MarkRecoveryPass(opCtx, config.Key, fence,
			RecoveryPassKnownTenants, cycle, true, 0)
	})
}

func (c *RecoveryCoordinator) listKnownRecoveryPage(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater, after string,
) (RecoveryPage[TenantRef], error) {
	if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
		return RecoveryPage[TenantRef]{}, err
	}
	opCtx, cancel := c.operationContext(ctx)
	page, err := source.ListKnownTenants(opCtx, highWater, after, config.ReconcilePageSize)
	cancel()
	if err != nil {
		return RecoveryPage[TenantRef]{}, err
	}
	if err := page.Validate(after, config.ReconcilePageSize); err != nil {
		return RecoveryPage[TenantRef]{}, errors.Join(ErrCoordinationCorrupt, err)
	}
	if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
		return RecoveryPage[TenantRef]{}, err
	}
	return page, nil
}

func (c *RecoveryCoordinator) restoreRunningRecovery(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater string,
) error {
	after := ""
	count := 0
	for {
		page, err := c.listRunningRecoveryPage(ctx, config, fence, source, highWater, after)
		if err != nil {
			return err
		}
		for _, lease := range page.Items {
			count++
			if count > config.GlobalConcurrency {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: valid RUNNING exceeds global capacity"))
			}
			token, ttl, err := recoveryLeaseIdentity(config, lease)
			if err != nil {
				return err
			}
			if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
				return c.coordinator.RestoreInflight(opCtx, config.Key, fence, lease.TenantID, token, ttl)
			}); err != nil {
				return err
			}
		}
		if page.Done {
			return nil
		}
		after = page.NextCursor
	}
}

func (c *RecoveryCoordinator) listRunningRecoveryPage(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater, after string,
) (RecoveryPage[RunningLease], error) {
	if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
		return RecoveryPage[RunningLease]{}, err
	}
	opCtx, cancel := c.operationContext(ctx)
	page, err := source.ListValidRunning(opCtx, highWater, after, config.ReconcilePageSize)
	cancel()
	if err != nil {
		return RecoveryPage[RunningLease]{}, err
	}
	if err := page.Validate(after, config.ReconcilePageSize); err != nil {
		return RecoveryPage[RunningLease]{}, errors.Join(ErrCoordinationCorrupt, err)
	}
	for _, lease := range page.Items {
		if !config.ValidateTaskID(lease.TaskID) {
			return RecoveryPage[RunningLease]{}, errors.Join(ErrCoordinationCorrupt,
				errors.New("fairqueue: recovery source returned invalid resource task ID"))
		}
	}
	if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
		return RecoveryPage[RunningLease]{}, err
	}
	return page, nil
}

func recoveryLeaseIdentity(config ResourceConfig, lease RunningLease) (string, time.Duration, error) {
	if err := lease.Validate(); err != nil || !config.ValidateTaskID(lease.TaskID) {
		return "", 0, errors.Join(ErrCoordinationCorrupt, ErrInvalidModel)
	}
	token, err := StableReservationToken(config.Key, lease.TaskID, lease.ClaimGeneration)
	if err != nil {
		return "", 0, errors.Join(ErrCoordinationCorrupt, err)
	}
	ttl := lease.LeaseUntil.Sub(lease.ObservedDBNow)
	if ttl <= 0 || ttl > maxResourceDuration {
		return "", 0, errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: invalid RUNNING remaining lease"))
	}
	return token, ttl, nil
}

func (c *RecoveryCoordinator) restoreDispatchedRecovery(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater string,
	cycle uint64,
) error {
	after := ""
	for {
		page, err := c.listDispatchedRecoveryPage(ctx, config, fence, source, highWater, after)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := c.recoveryRabbitCall(ctx, config.Key, fence, func(opCtx context.Context) error {
				return c.rabbit.EnsureTenantTopology(opCtx, config.Key, item.TenantID)
			}); err != nil {
				return err
			}
			var depth int64
			if err := c.recoveryRabbitCall(ctx, config.Key, fence, func(opCtx context.Context) error {
				var err error
				depth, err = c.rabbit.ReadyDepth(opCtx, config.Key, item.TenantID)
				return err
			}); err != nil {
				return err
			}
			if depth < 0 {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: Rabbit returned negative ready depth"))
			}
			if depth > 0 {
				if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
					return c.coordinator.RestoreActiveTenant(opCtx, config.Key, fence, item.TenantID)
				}); err != nil {
					return err
				}
			}
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}
	return c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
		return c.coordinator.MarkRecoveryPass(opCtx, config.Key, fence,
			RecoveryPassDispatched, cycle, true, 0)
	})
}

func (c *RecoveryCoordinator) listDispatchedRecoveryPage(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater, after string,
) (RecoveryPage[DispatchedRef], error) {
	if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
		return RecoveryPage[DispatchedRef]{}, err
	}
	opCtx, cancel := c.operationContext(ctx)
	page, err := source.ListDispatched(opCtx, highWater, after, config.ReconcilePageSize)
	cancel()
	if err != nil {
		return RecoveryPage[DispatchedRef]{}, err
	}
	if err := page.Validate(after, config.ReconcilePageSize); err != nil {
		return RecoveryPage[DispatchedRef]{}, errors.Join(ErrCoordinationCorrupt, err)
	}
	for _, item := range page.Items {
		if item.Token.Resource != config.Key || !config.ValidateTaskID(item.Token.TaskID) {
			return RecoveryPage[DispatchedRef]{}, errors.Join(ErrCoordinationCorrupt,
				errors.New("fairqueue: recovery source returned invalid dispatched token"))
		}
	}
	if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
		return RecoveryPage[DispatchedRef]{}, err
	}
	return page, nil
}

func (c *RecoveryCoordinator) recoveryRabbitCall(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	fn func(context.Context) error,
) error {
	if err := c.renewRecovery(ctx, resource, fence); err != nil {
		return err
	}
	opCtx, cancel := c.operationContext(ctx)
	err := fn(opCtx)
	cancel()
	if err != nil {
		return err
	}
	return c.renewRecovery(ctx, resource, fence)
}

type canonicalStableLease struct {
	tenant string
	ttl    time.Duration
}

func (c *RecoveryCoordinator) reconcileRecoveryStable(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater string,
) (int64, error) {
	canonical, err := c.loadRecoveryCanonicalStable(ctx, config, fence, source, highWater)
	if err != nil {
		return 0, err
	}
	redisStable, err := c.loadRecoveryRedisStable(ctx, config, fence)
	if err != nil {
		return 0, err
	}
	var diff int64
	for token, ref := range redisStable {
		lease, ok := canonical[token]
		if ok && lease.tenant == ref.TenantID {
			continue
		}
		diff++
		ref := ref
		if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
			return c.coordinator.DeleteRecoveryStableInflight(opCtx, config.Key, fence, ref)
		}); err != nil {
			return 0, err
		}
	}
	for token, lease := range canonical {
		ref, ok := redisStable[token]
		if !ok || ref.TenantID != lease.tenant {
			diff++
		}
		token, lease := token, lease
		if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
			return c.coordinator.RestoreInflight(opCtx, config.Key, fence, lease.tenant, token, lease.ttl)
		}); err != nil {
			return 0, err
		}
	}
	return diff, nil
}

func (c *RecoveryCoordinator) loadRecoveryCanonicalStable(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
	source RecoverySource,
	highWater string,
) (map[string]canonicalStableLease, error) {
	result := make(map[string]canonicalStableLease, config.GlobalConcurrency)
	after := ""
	for {
		page, err := c.listRunningRecoveryPage(ctx, config, fence, source, highWater, after)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			token, ttl, err := recoveryLeaseIdentity(config, item)
			if err != nil {
				return nil, err
			}
			if _, duplicate := result[token]; duplicate {
				return nil, errors.Join(ErrCoordinationCorrupt,
					errors.New("fairqueue: duplicate canonical stable identity"))
			}
			if len(result) >= config.GlobalConcurrency {
				return nil, errors.Join(ErrCoordinationCorrupt,
					errors.New("fairqueue: canonical stable identities exceed capacity"))
			}
			result[token] = canonicalStableLease{tenant: item.TenantID, ttl: ttl}
		}
		if page.Done {
			return result, nil
		}
		after = page.NextCursor
	}
}

func (c *RecoveryCoordinator) loadRecoveryRedisStable(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
) (map[string]ReservationRef, error) {
	result := make(map[string]ReservationRef, config.GlobalConcurrency)
	after := ""
	for {
		if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
			return nil, err
		}
		opCtx, cancel := c.operationContext(ctx)
		page, err := c.coordinator.ListRecoveryStableInflight(opCtx, config.Key, fence,
			after, config.ReconcilePageSize)
		cancel()
		if err != nil {
			return nil, err
		}
		if err := page.Validate(after, config.ReconcilePageSize); err != nil {
			return nil, errors.Join(ErrCoordinationCorrupt, err)
		}
		for _, item := range page.Items {
			if _, duplicate := result[item.StableToken]; duplicate {
				return nil, errors.Join(ErrCoordinationCorrupt,
					errors.New("fairqueue: duplicate Redis stable identity"))
			}
			if len(result) >= config.GlobalConcurrency {
				return nil, errors.Join(ErrCoordinationCorrupt,
					errors.New("fairqueue: Redis stable identities exceed capacity"))
			}
			result[item.StableToken] = item
		}
		if err := c.renewRecovery(ctx, config.Key, fence); err != nil {
			return nil, err
		}
		if page.Done {
			return result, nil
		}
		after = page.NextCursor
	}
}

// FinishRecovery performs the final physical cleanup and Redis atomic READY
// transition. Special-operation callers must CommitReady in their journal
// before this method and Complete it afterwards.
func (c *RecoveryCoordinator) FinishRecovery(
	ctx context.Context,
	config ResourceConfig,
	fence RecoveryFence,
) (ResourceFence, error) {
	if c == nil || ctx == nil || config.Validate() != nil || fence.Validate() != nil {
		return ResourceFence{}, fmt.Errorf("%w: invalid recovery finish", ErrInvalidModel)
	}
	var cleanup RecoveryCleanupResult
	if err := c.recoveryMutation(ctx, config.Key, fence, func(opCtx context.Context) error {
		var err error
		cleanup, err = c.coordinator.RecoveryReapExpired(opCtx, config.Key, fence, config.ReconcilePageSize)
		return err
	}); err != nil {
		return ResourceFence{}, err
	}
	if err := cleanup.Validate(); err != nil {
		return ResourceFence{}, errors.Join(ErrCoordinationCorrupt, err)
	}
	if cleanup.RemainingProvisionals != 0 || cleanup.RemainingTurns != 0 {
		return ResourceFence{}, ErrResourceNotReady
	}
	opCtx, cancel := c.operationContext(ctx)
	err := c.coordinator.FinishRecovery(opCtx, config.Key, fence)
	cancel()
	if err != nil {
		return ResourceFence{}, err
	}
	ready, err := c.observeReady(ctx, config.Key, fence.WriterFingerprint)
	if err != nil {
		return ResourceFence{}, err
	}
	if ready != fence.ResourceFence {
		return ResourceFence{}, ErrFenceMismatch
	}
	return ready, nil
}

// ReconcileReady performs one bounded MySQL-authoritative READY pass. It uses
// independent high-water keyset cursors and never treats Redis known_users as
// the canonical tenant universe.
func (c *RecoveryCoordinator) ReconcileReady(
	ctx context.Context,
	config ResourceConfig,
	fence ResourceFence,
	source RecoverySource,
) error {
	if c == nil || ctx == nil || source == nil || config.Validate() != nil || fence.Validate() != nil {
		return fmt.Errorf("%w: invalid READY reconciliation", ErrInvalidModel)
	}
	if err := c.checkReady(ctx, config.Key, fence); err != nil {
		return err
	}
	opCtx, cancel := c.operationContext(ctx)
	highWater, err := source.CaptureHighWater(opCtx)
	cancel()
	if err != nil {
		return err
	}
	if err := ValidateHighWater(highWater); err != nil {
		return errors.Join(ErrCoordinationCorrupt, err)
	}
	if err := c.checkReady(ctx, config.Key, fence); err != nil {
		return err
	}
	if err := c.reconcileReadyKnown(ctx, config, fence, source, highWater); err != nil {
		return err
	}
	if err := c.reconcileReadyStable(ctx, config, fence, source, highWater); err != nil {
		return err
	}
	if err := c.reconcileReadyDispatched(ctx, config, fence, source, highWater); err != nil {
		return err
	}
	for {
		var cleanup RecoveryCleanupResult
		opCtx, cancel := c.operationContext(ctx)
		cleanup, err = c.coordinator.ReapExpiredTurnsAndProvisionals(opCtx, config.Key, fence,
			config.ReconcilePageSize)
		cancel()
		if err != nil {
			return err
		}
		if err := cleanup.Validate(); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		if cleanup.RemovedProvisionals+cleanup.RemovedTurns < int64(config.ReconcilePageSize) {
			break
		}
	}
	return c.checkReady(ctx, config.Key, fence)
}

func (c *RecoveryCoordinator) checkReady(ctx context.Context, resource string, fence ResourceFence) error {
	opCtx, cancel := c.operationContext(ctx)
	defer cancel()
	return c.coordinator.CheckReadyFence(opCtx, resource, fence)
}

func (c *RecoveryCoordinator) reconcileReadyKnown(
	ctx context.Context,
	config ResourceConfig,
	fence ResourceFence,
	source RecoverySource,
	highWater string,
) error {
	after := ""
	for {
		if err := c.checkReady(ctx, config.Key, fence); err != nil {
			return err
		}
		opCtx, cancel := c.operationContext(ctx)
		page, err := source.ListKnownTenants(opCtx, highWater, after, config.ReconcilePageSize)
		cancel()
		if err != nil {
			return err
		}
		if err := page.Validate(after, config.ReconcilePageSize); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		if err := c.checkReady(ctx, config.Key, fence); err != nil {
			return err
		}
		for _, item := range page.Items {
			if err := c.readyRabbitCall(ctx, config.Key, fence, func(opCtx context.Context) error {
				return c.rabbit.EnsureTenantTopology(opCtx, config.Key, item.TenantID)
			}); err != nil {
				return err
			}
			opCtx, cancel := c.operationContext(ctx)
			err := c.coordinator.EnsureKnownTenant(opCtx, config.Key, fence, item.TenantID)
			cancel()
			if err != nil {
				return err
			}
		}
		if page.Done {
			return nil
		}
		after = page.NextCursor
	}
}

func (c *RecoveryCoordinator) readyRabbitCall(
	ctx context.Context,
	resource string,
	fence ResourceFence,
	fn func(context.Context) error,
) error {
	if err := c.checkReady(ctx, resource, fence); err != nil {
		return err
	}
	opCtx, cancel := c.operationContext(ctx)
	err := fn(opCtx)
	cancel()
	if err != nil {
		return err
	}
	return c.checkReady(ctx, resource, fence)
}

func (c *RecoveryCoordinator) reconcileReadyDispatched(
	ctx context.Context,
	config ResourceConfig,
	fence ResourceFence,
	source RecoverySource,
	highWater string,
) error {
	after := ""
	for {
		if err := c.checkReady(ctx, config.Key, fence); err != nil {
			return err
		}
		opCtx, cancel := c.operationContext(ctx)
		page, err := source.ListDispatched(opCtx, highWater, after, config.ReconcilePageSize)
		cancel()
		if err != nil {
			return err
		}
		if err := page.Validate(after, config.ReconcilePageSize); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		for _, item := range page.Items {
			if item.Token.Resource != config.Key || !config.ValidateTaskID(item.Token.TaskID) {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: invalid READY dispatched token"))
			}
			if err := c.readyRabbitCall(ctx, config.Key, fence, func(opCtx context.Context) error {
				return c.rabbit.EnsureTenantTopology(opCtx, config.Key, item.TenantID)
			}); err != nil {
				return err
			}
			var depth int64
			if err := c.readyRabbitCall(ctx, config.Key, fence, func(opCtx context.Context) error {
				var err error
				depth, err = c.rabbit.ReadyDepth(opCtx, config.Key, item.TenantID)
				return err
			}); err != nil {
				return err
			}
			if depth < 0 {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: negative READY depth"))
			}
			if depth > 0 {
				opCtx, cancel := c.operationContext(ctx)
				err := c.coordinator.EnsureActive(opCtx, config.Key, fence, item.TenantID)
				cancel()
				if err != nil {
					return err
				}
			}
		}
		if err := c.checkReady(ctx, config.Key, fence); err != nil {
			return err
		}
		if page.Done {
			return nil
		}
		after = page.NextCursor
	}
}

func (c *RecoveryCoordinator) reconcileReadyStable(
	ctx context.Context,
	config ResourceConfig,
	fence ResourceFence,
	source RecoverySource,
	highWater string,
) error {
	canonical := make(map[string]canonicalStableLease, config.GlobalConcurrency)
	after := ""
	for {
		if err := c.checkReady(ctx, config.Key, fence); err != nil {
			return err
		}
		opCtx, cancel := c.operationContext(ctx)
		page, err := source.ListValidRunning(opCtx, highWater, after, config.ReconcilePageSize)
		cancel()
		if err != nil {
			return err
		}
		if err := page.Validate(after, config.ReconcilePageSize); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		for _, item := range page.Items {
			token, ttl, err := recoveryLeaseIdentity(config, item)
			if err != nil {
				return err
			}
			if _, exists := canonical[token]; exists || len(canonical) >= config.GlobalConcurrency {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: invalid READY stable identity set"))
			}
			canonical[token] = canonicalStableLease{tenant: item.TenantID, ttl: ttl}
		}
		if err := c.checkReady(ctx, config.Key, fence); err != nil {
			return err
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}

	redisStable := make(map[string]ReservationRef, config.GlobalConcurrency)
	after = ""
	for {
		opCtx, cancel := c.operationContext(ctx)
		page, err := c.coordinator.ListReadyStableInflight(opCtx, config.Key, fence, after,
			config.ReconcilePageSize)
		cancel()
		if err != nil {
			return err
		}
		if err := page.Validate(after, config.ReconcilePageSize); err != nil {
			return errors.Join(ErrCoordinationCorrupt, err)
		}
		for _, item := range page.Items {
			if _, exists := redisStable[item.StableToken]; exists || len(redisStable) >= config.GlobalConcurrency {
				return errors.Join(ErrCoordinationCorrupt, errors.New("fairqueue: invalid Redis READY stable set"))
			}
			redisStable[item.StableToken] = item
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}

	for token, ref := range redisStable {
		lease, ok := canonical[token]
		if ok && lease.tenant == ref.TenantID {
			continue
		}
		opCtx, cancel := c.operationContext(ctx)
		err := c.coordinator.Release(opCtx, config.Key, fence, ref.TenantID, ref.StableToken)
		cancel()
		if err != nil {
			return err
		}
	}
	for token, lease := range canonical {
		opCtx, cancel := c.operationContext(ctx)
		err := c.coordinator.EnsureReadyStableInflight(opCtx, config.Key, fence,
			lease.tenant, token, lease.ttl)
		cancel()
		if err != nil {
			return err
		}
	}
	return c.checkReady(ctx, config.Key, fence)
}
