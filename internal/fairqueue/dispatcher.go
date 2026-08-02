package fairqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultDispatcherPageSize       = 100
	defaultDispatchInterval         = time.Second
	defaultExpiredRunningSweep      = 5 * time.Second
	defaultPublishAttemptTimeout    = 15 * time.Second
	defaultDispatcherBackoffInitial = 100 * time.Millisecond
	defaultDispatcherBackoffMaximum = 5 * time.Second
)

var (
	errPublisherGateClosed = errors.New("fairqueue: publisher gate is closed")
	errPublisherNotDrained = errors.New("fairqueue: publisher attempts have not drained")
	errDispatcherRunning   = errors.New("fairqueue: dispatcher is already running")
)

// stalePublisherSourceFailure preserves the authoritative safety category for
// diagnostics while telling Runtime that this particular source read belongs
// to a gate generation that was already closed and reopened. Only source
// reads outside the publisher-attempt registry can produce this wrapper.
type stalePublisherSourceFailure struct{ err error }

func (e stalePublisherSourceFailure) Error() string { return e.err.Error() }
func (e stalePublisherSourceFailure) Unwrap() error { return e.err }

func isStalePublisherSourceFailure(err error) bool {
	var stale stalePublisherSourceFailure
	return errors.As(err, &stale)
}

// DispatcherOptions bounds every page, publish attempt, retry, and idle wait.
// Zero values select conservative defaults; negative values are rejected.
type DispatcherOptions struct {
	PageSize                    int
	DispatchInterval            time.Duration
	ExpiredRunningSweepInterval time.Duration
	PublishAttemptTimeout       time.Duration
	BackoffInitial              time.Duration
	BackoffMax                  time.Duration
}

func (o DispatcherOptions) withDefaults() (DispatcherOptions, error) {
	if o.PageSize == 0 {
		o.PageSize = defaultDispatcherPageSize
	}
	if o.DispatchInterval == 0 {
		o.DispatchInterval = defaultDispatchInterval
	}
	if o.ExpiredRunningSweepInterval == 0 {
		o.ExpiredRunningSweepInterval = defaultExpiredRunningSweep
	}
	if o.PublishAttemptTimeout == 0 {
		o.PublishAttemptTimeout = defaultPublishAttemptTimeout
	}
	if o.BackoffInitial == 0 {
		o.BackoffInitial = defaultDispatcherBackoffInitial
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = defaultDispatcherBackoffMaximum
	}

	if err := ValidatePageLimit(o.PageSize); err != nil {
		return DispatcherOptions{}, fmt.Errorf("fairqueue: dispatcher page size: %w", err)
	}
	durations := []struct {
		name  string
		value time.Duration
	}{
		{"dispatch interval", o.DispatchInterval},
		{"expired-running sweep interval", o.ExpiredRunningSweepInterval},
		{"publish-attempt timeout", o.PublishAttemptTimeout},
		{"initial backoff", o.BackoffInitial},
		{"maximum backoff", o.BackoffMax},
	}
	for _, item := range durations {
		if item.value <= 0 || item.value > maxResourceDuration {
			return DispatcherOptions{}, fmt.Errorf("fairqueue: dispatcher %s must be in (0,%s]", item.name, maxResourceDuration)
		}
	}
	if o.BackoffInitial > o.BackoffMax {
		return DispatcherOptions{}, errors.New("fairqueue: dispatcher initial backoff exceeds maximum backoff")
	}
	return o, nil
}

// Dispatcher durably projects canonical source candidates into RabbitMQ. It
// owns no persistent queue: source generations and dispatch markers remain the
// only publication authority.
type Dispatcher struct {
	resource    string
	source      DispatchSource
	rearm       ExpiredRearmSource
	rabbit      RabbitClient
	coordinator Coordinator
	options     DispatcherOptions

	gateMu   sync.Mutex
	gateOpen bool
	fence    ResourceFence
	gateGen  uint64
	inFlight int
	drained  chan struct{}

	runMu   sync.Mutex
	running bool
}

// NewDispatcher stages a validated fence but leaves the publisher gate closed.
// Runtime recovery or READY reconciliation must explicitly open it before
// dispatch can begin. NewClosedDispatcher is used when startup has not yet
// established any authoritative READY fence.
func NewDispatcher(
	resource string,
	fence ResourceFence,
	source DispatchSource,
	rearm ExpiredRearmSource,
	rabbit RabbitClient,
	coordinator Coordinator,
	options DispatcherOptions,
) (*Dispatcher, error) {
	if err := fence.Validate(); err != nil {
		return nil, err
	}
	dispatcher, err := NewClosedDispatcher(resource, source, rearm, rabbit, coordinator, options)
	if err != nil {
		return nil, err
	}
	dispatcher.fence = fence
	return dispatcher, nil
}

// NewClosedDispatcher creates a fail-closed dispatcher without inventing an
// epoch or writer identity before startup recovery has established READY.
func NewClosedDispatcher(
	resource string,
	source DispatchSource,
	rearm ExpiredRearmSource,
	rabbit RabbitClient,
	coordinator Coordinator,
	options DispatcherOptions,
) (*Dispatcher, error) {
	if err := ValidateResource(resource); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("fairqueue: dispatcher source is required")
	}
	if rearm == nil {
		return nil, errors.New("fairqueue: expired rearm source is required")
	}
	if rabbit == nil {
		return nil, errors.New("fairqueue: dispatcher Rabbit client is required")
	}
	if coordinator == nil {
		return nil, errors.New("fairqueue: dispatcher coordinator is required")
	}
	normalized, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	drained := make(chan struct{})
	close(drained)
	return &Dispatcher{
		resource:    resource,
		source:      source,
		rearm:       rearm,
		rabbit:      rabbit,
		coordinator: coordinator,
		options:     normalized,
		gateGen:     1,
		drained:     drained,
	}, nil
}

// ClosePublisherGate synchronously prevents admission of new publish
// attempts. Attempts already admitted retain their snapshotted fence and hard
// deadline; callers can wait for them with WaitForPublisherDrain.
func (d *Dispatcher) ClosePublisherGate() {
	if d == nil {
		return
	}
	d.gateMu.Lock()
	d.gateOpen = false
	d.gateGen++
	d.gateMu.Unlock()
}

// OpenPublisherGate installs a new READY fence. Reopening is deliberately
// rejected while an old attempt is in flight so recovery cannot overlap two
// resource epochs.
func (d *Dispatcher) OpenPublisherGate(fence ResourceFence) error {
	if d == nil {
		return errors.New("fairqueue: nil dispatcher")
	}
	if err := fence.Validate(); err != nil {
		return err
	}
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	if d.inFlight != 0 {
		return errors.Join(ErrResourceNotReady, errPublisherNotDrained)
	}
	d.fence = fence
	d.gateOpen = true
	d.gateGen++
	return nil
}

func (d *Dispatcher) PublisherGateOpen() bool {
	if d == nil {
		return false
	}
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	return d.gateOpen
}

// WaitForPublisherDrain waits through the original-candidate Mark of every
// admitted publish attempt. Post-Mark activation is independently fenced and
// bounded. This method is normally called after closing the gate.
func (d *Dispatcher) WaitForPublisherDrain(ctx context.Context) error {
	if d == nil {
		return errors.New("fairqueue: nil dispatcher")
	}
	if ctx == nil {
		return errors.New("fairqueue: nil dispatcher context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.gateMu.Lock()
	drained := d.drained
	d.gateMu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-drained:
		return nil
	}
}

func (d *Dispatcher) publisherFence() (ResourceFence, uint64, error) {
	if d == nil {
		return ResourceFence{}, 0, errors.New("fairqueue: nil dispatcher")
	}
	d.gateMu.Lock()
	defer d.gateMu.Unlock()
	if !d.gateOpen {
		return ResourceFence{}, 0, errors.Join(ErrResourceNotReady, errPublisherGateClosed)
	}
	return d.fence, d.gateGen, nil
}

func (d *Dispatcher) beginPublisherAttempt() (ResourceFence, uint64, func(), error) {
	if d == nil {
		return ResourceFence{}, 0, nil, errors.New("fairqueue: nil dispatcher")
	}
	d.gateMu.Lock()
	if !d.gateOpen {
		d.gateMu.Unlock()
		return ResourceFence{}, 0, nil, errors.Join(ErrResourceNotReady, errPublisherGateClosed)
	}
	if d.inFlight == 0 {
		d.drained = make(chan struct{})
	}
	d.inFlight++
	fence := d.fence
	gateGen := d.gateGen
	d.gateMu.Unlock()

	var once sync.Once
	finish := func() {
		once.Do(func() {
			d.gateMu.Lock()
			d.inFlight--
			if d.inFlight == 0 {
				close(d.drained)
			}
			d.gateMu.Unlock()
		})
	}
	return fence, gateGen, finish, nil
}

func (d *Dispatcher) closePublisherGateFor(fence ResourceFence, gateGen uint64) {
	d.gateMu.Lock()
	if d.fence == fence && d.gateGen == gateGen {
		d.gateOpen = false
		d.gateGen++
	}
	d.gateMu.Unlock()
}

func (d *Dispatcher) classifyFatalSourceFailure(fence ResourceFence, gateGen uint64, err error) error {
	if !authoritativeFatalError(err) {
		return err
	}
	d.gateMu.Lock()
	if d.fence == fence && d.gateGen == gateGen {
		d.gateOpen = false
		d.gateGen++
		d.gateMu.Unlock()
		return err
	}
	reopened := d.gateOpen && (d.fence != fence || d.gateGen != gateGen)
	d.gateMu.Unlock()
	if reopened && errors.Is(err, ErrAuthoritativeWriterMismatch) &&
		!errors.Is(err, ErrAuthoritativeStateCorrupt) {
		return stalePublisherSourceFailure{err: err}
	}
	return err
}

// TryDispatch reloads the canonical candidate by ID and then uses exactly the
// same single-candidate path as the periodic scanner. The boolean reports only
// whether the source returned a candidate.
func (d *Dispatcher) TryDispatch(ctx context.Context, taskID string) (bool, error) {
	if ctx == nil {
		return false, errors.New("fairqueue: nil dispatcher context")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	sourceFence, sourceGateGen, err := d.publisherFence()
	if err != nil {
		return false, err
	}
	if err := ValidateTaskID(taskID); err != nil {
		return false, err
	}
	candidate, ok, err := d.source.GetDispatchableByID(ctx, taskID)
	if err != nil {
		err = d.classifyFatalSourceFailure(sourceFence, sourceGateGen, err)
		return false, fmt.Errorf("fairqueue: get dispatch candidate: %w", err)
	}
	if !ok {
		return false, nil
	}
	if candidate.Message.TaskID != taskID {
		return true, fmt.Errorf("%w: dispatch source returned another task", ErrInvalidModel)
	}
	if err := d.dispatchCandidate(ctx, candidate); err != nil {
		return true, err
	}
	return true, nil
}

// DispatchPage scans one bounded canonical keyset page and publishes each
// candidate in source order. On a candidate failure it returns the input cursor
// so the caller cannot accidentally skip the failed durable obligation.
func (d *Dispatcher) DispatchPage(ctx context.Context, after string) (string, error) {
	next, _, err := d.dispatchPage(ctx, after)
	return next, err
}

func (d *Dispatcher) dispatchPage(ctx context.Context, after string) (string, bool, error) {
	if ctx == nil {
		return after, false, errors.New("fairqueue: nil dispatcher context")
	}
	if err := ctx.Err(); err != nil {
		return after, false, err
	}
	if err := ValidateCursor(after); err != nil {
		return after, false, err
	}
	sourceFence, sourceGateGen, err := d.publisherFence()
	if err != nil {
		return after, false, err
	}
	candidates, next, err := d.source.ListDispatchCandidates(ctx, after, d.options.PageSize)
	if err != nil {
		err = d.classifyFatalSourceFailure(sourceFence, sourceGateGen, err)
		return after, false, fmt.Errorf("fairqueue: list dispatch candidates: %w", err)
	}
	next, err = normalizeDispatcherPage(len(candidates), next, after, d.options.PageSize)
	if err != nil {
		return after, false, err
	}
	for _, candidate := range candidates {
		if err := d.dispatchCandidate(ctx, candidate); err != nil {
			return after, false, err
		}
	}
	return next, len(candidates) == 0, nil
}

// RearmPage lets the domain source atomically arm expired RUNNING rows, then
// sends the returned new candidates through the shared publication path. A
// crash after source mutation is recovered by the ordinary dispatch scan.
func (d *Dispatcher) RearmPage(ctx context.Context, after string) (string, error) {
	next, _, err := d.rearmPage(ctx, after)
	return next, err
}

func (d *Dispatcher) rearmPage(ctx context.Context, after string) (string, bool, error) {
	if ctx == nil {
		return after, false, errors.New("fairqueue: nil dispatcher context")
	}
	if err := ctx.Err(); err != nil {
		return after, false, err
	}
	if err := ValidateCursor(after); err != nil {
		return after, false, err
	}

	// Rearming mutates canonical DB state, so it has its own fenced, bounded
	// admission before the source call. Each returned candidate is checked
	// again by dispatchCandidate before any Rabbit operation.
	fence, gateGen, finish, err := d.beginPublisherAttempt()
	if err != nil {
		return after, false, err
	}
	preflightCtx, cancel := context.WithTimeout(ctx, d.options.PublishAttemptTimeout)
	if err := d.coordinator.CheckReadyFence(preflightCtx, d.resource, fence); err != nil {
		cancel()
		finish()
		d.closePublisherGateFor(fence, gateGen)
		return after, false, fmt.Errorf("fairqueue: check rearm fence: %w", err)
	}
	if err := preflightCtx.Err(); err != nil {
		cancel()
		finish()
		d.closePublisherGateFor(fence, gateGen)
		return after, false, err
	}
	candidates, next, err := d.rearm.RearmExpiredPage(preflightCtx, after, d.options.PageSize)
	contextErr := preflightCtx.Err()
	cancel()
	finish()
	if err != nil {
		d.closePublisherGateOnFatalSource(fence, gateGen, err)
		return after, false, fmt.Errorf("fairqueue: rearm expired candidates: %w", err)
	}
	if contextErr != nil {
		return after, false, contextErr
	}
	next, err = normalizeDispatcherPage(len(candidates), next, after, d.options.PageSize)
	if err != nil {
		return after, false, err
	}
	for _, candidate := range candidates {
		if err := d.dispatchCandidate(ctx, candidate); err != nil {
			return after, false, err
		}
	}
	return next, len(candidates) == 0, nil
}

func normalizeDispatcherPage(items int, next, after string, limit int) (string, error) {
	if items < 0 || items > limit {
		return next, fmt.Errorf("%w: dispatcher source returned an oversized page", ErrInvalidModel)
	}
	if err := ValidateCursor(next); err != nil {
		return next, fmt.Errorf("%w: dispatcher next cursor: %v", ErrInvalidModel, err)
	}
	if next != "" && next == after {
		// Store-backed keyset sources may retain the input cursor to signal an
		// empty terminal page. Normalize that representation to the dispatcher's
		// explicit end-of-round cursor, but reject non-empty non-advancing pages.
		if items == 0 {
			return "", nil
		}
		return next, fmt.Errorf("%w: dispatcher cursor did not advance", ErrInvalidModel)
	}
	return next, nil
}

func (d *Dispatcher) dispatchCandidate(ctx context.Context, candidate DispatchCandidate) error {
	if err := candidate.Validate(); err != nil {
		return err
	}
	if candidate.Message.Resource != d.resource {
		return fmt.Errorf("%w: dispatch candidate belongs to another resource", ErrInvalidModel)
	}

	// Keep this value untouched until MarkDispatched. In particular, never
	// reload or synthesize its opaque Guard after the Rabbit confirm.
	original := candidate
	fence, gateGen, finish, err := d.beginPublisherAttempt()
	if err != nil {
		return err
	}
	defer finish()

	attemptCtx, cancel := context.WithTimeout(ctx, d.options.PublishAttemptTimeout)
	defer cancel()
	if err := d.coordinator.CheckReadyFence(attemptCtx, d.resource, fence); err != nil {
		d.closePublisherGateFor(fence, gateGen)
		return fmt.Errorf("fairqueue: check publisher fence: %w", err)
	}
	if err := attemptCtx.Err(); err != nil {
		d.closePublisherGateFor(fence, gateGen)
		return err
	}

	receipt, err := d.rabbit.PublishMandatoryConfirmed(attemptCtx, original.Message)
	if err != nil {
		return fmt.Errorf("fairqueue: publish candidate: %w", err)
	}
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := attemptCtx.Err(); err != nil {
		return err
	}

	marked, markErr := d.source.MarkDispatched(attemptCtx, original)
	attemptErr := attemptCtx.Err()
	if authoritativeFatalError(markErr) {
		d.closePublisherGateFor(fence, gateGen)
	}

	// The bounded publisher attempt ends at the original-candidate Mark. Redis
	// activation is a separately bounded, fenced repair of scheduling state.
	finish()
	if authoritativeFatalError(markErr) {
		// The confirmed delivery remains recoverable from MySQL/Rabbit during the
		// next authoritative rebuild. Never derive a Redis mutation from a source
		// result whose writer identity or canonical state is no longer trusted.
		return fmt.Errorf("fairqueue: mark dispatched candidate: %w", markErr)
	}
	if markErr != nil || attemptErr != nil {
		_ = d.activateAfterPublish(ctx, fence, gateGen, original, false)
		if markErr != nil {
			return fmt.Errorf("fairqueue: mark dispatched candidate: %w", markErr)
		}
		return attemptErr
	}
	if err := d.activateAfterPublish(ctx, fence, gateGen, original, marked); err != nil {
		return err
	}
	return nil
}

func (d *Dispatcher) activateAfterPublish(ctx context.Context, fence ResourceFence, gateGen uint64, original DispatchCandidate, marked bool) error {
	// A confirmed delivery still needs a bounded activation attempt even when
	// the caller or the publish-attempt child context has just been canceled.
	activationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d.options.PublishAttemptTimeout)
	defer cancel()
	var err error
	if marked {
		err = d.coordinator.Activate(activationCtx, d.resource, fence, original.Message.TenantID)
	} else {
		// Tenant identity is immutable within a DispatchCandidate Guard contract;
		// never reload a candidate merely to choose a tenant for stale delivery.
		err = d.coordinator.EnsureActive(activationCtx, d.resource, fence, original.Message.TenantID)
	}
	if err != nil {
		d.closePublisherGateFor(fence, gateGen)
		return fmt.Errorf("fairqueue: activate dispatched tenant: %w", err)
	}
	if err := activationCtx.Err(); err != nil {
		d.closePublisherGateFor(fence, gateGen)
		return err
	}
	return nil
}

func (d *Dispatcher) closePublisherGateOnFatalSource(fence ResourceFence, gateGen uint64, err error) {
	if authoritativeFatalError(err) {
		d.closePublisherGateFor(fence, gateGen)
	}
}

// Run starts the periodic dispatch scanner and the independent always-on
// expired-RUNNING reaper. Dependency failures retry with bounded backoff; a
// complete keyset round waits for its configured interval, including empty
// rounds, so neither loop can busy-spin.
func (d *Dispatcher) Run(ctx context.Context) error {
	if d == nil {
		return errors.New("fairqueue: nil dispatcher")
	}
	if ctx == nil {
		return errors.New("fairqueue: nil dispatcher context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.runMu.Lock()
	if d.running {
		d.runMu.Unlock()
		return errDispatcherRunning
	}
	d.running = true
	d.runMu.Unlock()
	defer func() {
		d.runMu.Lock()
		d.running = false
		d.runMu.Unlock()
	}()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- d.runDispatchLoop(runCtx) }()
	go func() { results <- d.runRearmLoop(runCtx) }()

	first := <-results
	cancel()
	second := <-results
	// An authoritative source failure is a safety event, not ordinary
	// cancellation. Preserve it even when parent shutdown races with the source
	// result so Runtime can immediately cancel all authoritative pipelines. The
	// only exemption is a pure writer mismatch from an obsolete gate generation;
	// corruption is never stale-exempted, including a joined mixed error.
	if authoritativeFatalError(first) && !stalePublisherWriterMismatchOnly(first) {
		return first
	}
	if authoritativeFatalError(second) && !stalePublisherWriterMismatchOnly(second) {
		return second
	}
	if authoritativeFatalError(first) {
		return first
	}
	if authoritativeFatalError(second) {
		return second
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if first != nil {
		return first
	}
	return second
}

func (d *Dispatcher) runDispatchLoop(ctx context.Context) error {
	cursor := ""
	backoff := d.options.BackoffInitial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, _, err := d.dispatchPage(ctx, cursor)
		if err != nil {
			if authoritativeFatalError(err) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := waitDispatcherContext(ctx, backoff); err != nil {
				return err
			}
			backoff = nextDispatcherBackoff(backoff, d.options.BackoffMax)
			continue
		}
		backoff = d.options.BackoffInitial
		cursor = next
		if cursor == "" {
			if err := waitDispatcherContext(ctx, d.options.DispatchInterval); err != nil {
				return err
			}
		}
	}
}

func (d *Dispatcher) runRearmLoop(ctx context.Context) error {
	cursor := ""
	backoff := d.options.BackoffInitial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		next, _, err := d.rearmPage(ctx, cursor)
		if err != nil {
			if authoritativeFatalError(err) {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := waitDispatcherContext(ctx, backoff); err != nil {
				return err
			}
			backoff = nextDispatcherBackoff(backoff, d.options.BackoffMax)
			continue
		}
		backoff = d.options.BackoffInitial
		cursor = next
		if cursor == "" {
			if err := waitDispatcherContext(ctx, d.options.ExpiredRunningSweepInterval); err != nil {
				return err
			}
		}
	}
}

func waitDispatcherContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextDispatcherBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum {
		return maximum
	}
	if current > maximum-current {
		return maximum
	}
	return current * 2
}
