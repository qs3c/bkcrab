package fairqueue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	defaultSchedulerIdleInterval   = 25 * time.Millisecond
	defaultSchedulerBackoffInitial = 50 * time.Millisecond
	defaultSchedulerBackoffMaximum = 5 * time.Second
)

var (
	errSchedulerRunning     = errors.New("fairqueue: scheduler is already running")
	errSchedulerTokenSource = errors.New("fairqueue: scheduler token source failed")
)

// SchedulerOptions bounds idle polling, retries, and best-effort settlement.
// Zero values select conservative defaults; negative values are rejected.
type SchedulerOptions struct {
	IdleInterval   time.Duration
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	CleanupTimeout time.Duration
	Telemetry      TelemetrySink
}

func (o SchedulerOptions) withDefaults(config ResourceConfig) (SchedulerOptions, error) {
	if o.IdleInterval == 0 {
		o.IdleInterval = defaultSchedulerIdleInterval
	}
	if o.BackoffInitial == 0 {
		o.BackoffInitial = defaultSchedulerBackoffInitial
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = defaultSchedulerBackoffMaximum
	}
	if o.CleanupTimeout == 0 {
		o.CleanupTimeout = defaultSchedulerCleanupTimeout(config)
	}
	values := []struct {
		name  string
		value time.Duration
	}{
		{name: "idle interval", value: o.IdleInterval},
		{name: "initial backoff", value: o.BackoffInitial},
		{name: "maximum backoff", value: o.BackoffMax},
		{name: "cleanup timeout", value: o.CleanupTimeout},
	}
	for _, item := range values {
		if item.value <= 0 || item.value > maxResourceDuration {
			return SchedulerOptions{}, fmt.Errorf("fairqueue: scheduler %s must be in (0,%s]", item.name, maxResourceDuration)
		}
	}
	if o.BackoffInitial > o.BackoffMax {
		return SchedulerOptions{}, errors.New("fairqueue: scheduler initial backoff exceeds maximum backoff")
	}
	// prepareCount starts before the bounded NextTurn call. The longest legal
	// attempt can therefore consume one cleanup timeout for NextTurn, the one
	// absolute PrepareTimeout, then four fresh settlement timeouts (DLQ/NACK,
	// activation, release, and turn settlement). Keep that complete upper bound
	// strictly inside the recovery drain fence used before shared-client Close.
	attemptUpperBound := config.PrepareTimeout + 5*o.CleanupTimeout
	if attemptUpperBound >= config.RecoveryDrainTimeout {
		return SchedulerOptions{}, errors.New("fairqueue: prepare plus five cleanup steps must fit within recovery drain timeout")
	}
	return o, nil
}

func defaultSchedulerCleanupTimeout(config ResourceConfig) time.Duration {
	cleanup := config.PrepareTimeout
	remaining := config.RecoveryDrainTimeout - config.PrepareTimeout
	if remaining <= 1 {
		return 0
	}
	// Subtract one nanosecond before integer division to preserve the strict
	// upper-bound inequality even for synthetic sub-millisecond test configs.
	if aggregateBound := (remaining - 1) / 5; cleanup > aggregateBound {
		cleanup = aggregateBound
	}
	return cleanup
}

type schedulerStep uint8

const (
	schedulerStepProgress schedulerStep = iota
	schedulerStepIdle
	schedulerStepBackoff
)

// Scheduler grants one processing turn at a time for one registered resource.
// WorkerRuntime owns the admission gate and bounded local slots; taking a
// permit before NextTurn ensures a full local pool causes no Redis or RabbitMQ
// side effects.
type Scheduler struct {
	admission   schedulerWorkerAdmission
	resource    string
	config      ResourceConfig
	rabbit      RabbitClient
	coordinator Coordinator
	tokens      runtimeTokenSource
	options     SchedulerOptions
	health      *resourceHealth
	telemetry   TelemetrySink

	runMu   sync.Mutex
	running bool
}

type schedulerWorkerAdmission interface {
	tryReserve(resource string) (schedulerWorkerPermit, bool)
}

type schedulerWorkerPermit interface {
	gateSnapshot() (ResourceFence, uint64)
	start(workerEnvelope) error
	abort()
	reportCoordinationFailure(error)
}

type runtimeSchedulerAdmission struct{ runtime *Runtime }

func (a runtimeSchedulerAdmission) tryReserve(resource string) (schedulerWorkerPermit, bool) {
	permit, ok := a.runtime.tryReserveWorker(resource)
	if !ok {
		return nil, false
	}
	return permit, true
}

func newScheduler(
	runtime *Runtime,
	config ResourceConfig,
	rabbit RabbitClient,
	coordinator Coordinator,
	tokens runtimeTokenSource,
	options SchedulerOptions,
) (*Scheduler, error) {
	if runtime == nil {
		return nil, errors.New("fairqueue: scheduler runtime is required")
	}
	return newSchedulerWithAdmission(
		runtimeSchedulerAdmission{runtime: runtime}, config, rabbit, coordinator, tokens, options,
	)
}

func newSchedulerWithAdmission(
	admission schedulerWorkerAdmission,
	config ResourceConfig,
	rabbit RabbitClient,
	coordinator Coordinator,
	tokens runtimeTokenSource,
	options SchedulerOptions,
) (*Scheduler, error) {
	if admission == nil {
		return nil, errors.New("fairqueue: scheduler worker admission is required")
	}
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("fairqueue: scheduler resource config: %w", err)
	}
	if rabbit == nil {
		return nil, errors.New("fairqueue: scheduler Rabbit client is required")
	}
	if coordinator == nil {
		return nil, errors.New("fairqueue: scheduler coordinator is required")
	}
	if tokens == nil {
		return nil, errors.New("fairqueue: scheduler token source is required")
	}
	normalized, err := options.withDefaults(config)
	if err != nil {
		return nil, err
	}
	return &Scheduler{
		admission: admission, resource: config.Key, config: config,
		rabbit: rabbit, coordinator: coordinator, tokens: tokens, options: normalized, telemetry: normalized.Telemetry,
	}, nil
}

// Run continuously schedules the configured resource until ctx is canceled.
// Dependency and capacity failures use bounded exponential backoff; an empty
// ring or a closed/full local admission gate uses the idle interval.
func (s *Scheduler) Run(ctx context.Context) error {
	if s == nil {
		return errors.New("fairqueue: nil scheduler")
	}
	if ctx == nil {
		return errors.New("fairqueue: nil scheduler context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.runMu.Lock()
	if s.running {
		s.runMu.Unlock()
		return errSchedulerRunning
	}
	s.running = true
	s.runMu.Unlock()
	defer func() {
		s.runMu.Lock()
		s.running = false
		s.runMu.Unlock()
	}()

	backoff := s.options.BackoffInitial
	for {
		step, err := s.runOne(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && schedulerFatal(err) {
			return err
		}
		if err == nil {
			s.health.markLoopSuccess(loopScheduler)
		}

		var delay time.Duration
		switch {
		case err != nil || step == schedulerStepBackoff:
			delay = backoff
			backoff = nextSchedulerBackoff(backoff, s.options.BackoffMax)
		case step == schedulerStepIdle:
			delay = s.options.IdleInterval
			backoff = s.options.BackoffInitial
		default:
			backoff = s.options.BackoffInitial
			continue
		}
		if err := waitSchedulerContext(ctx, delay); err != nil {
			return err
		}
	}
}

// runOne performs at most one tenant turn. It is intentionally kept as a
// separate method so ordering, cleanup, and handoff can be verified without a
// timing-sensitive long-running loop.
func (s *Scheduler) runOne(ctx context.Context) (schedulerStep, error) {
	if s == nil {
		return schedulerStepBackoff, errors.New("fairqueue: nil scheduler")
	}
	if ctx == nil {
		return schedulerStepBackoff, errors.New("fairqueue: nil scheduler context")
	}
	if err := ctx.Err(); err != nil {
		return schedulerStepBackoff, err
	}

	// The runtime atomically checks the resource gate, reserves one bounded
	// local worker slot, and registers a prepare attempt. A false result means
	// either closed admission or a full local pool; both must be side-effect
	// free with respect to Redis and RabbitMQ.
	permit, ok := s.admission.tryReserve(s.resource)
	if !ok {
		EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetrySchedulerGate, Resource: s.resource, Outcome: "paused", Dependency: "runtime"})
		return schedulerStepIdle, nil
	}
	permitOwned := true
	defer func() {
		if permitOwned {
			permit.abort()
		}
	}()

	fence, gateGeneration := permit.gateSnapshot()
	turnID, err := s.nextToken("processing turn")
	if err != nil {
		return schedulerStepBackoff, err
	}
	turnCtx, cancelTurn := context.WithTimeout(ctx, s.options.CleanupTimeout)
	turn, found, err := s.coordinator.NextTurn(
		turnCtx, s.resource, fence, ProcessingTurnToken(turnID), s.config.ProcessingTurnTTL,
	)
	cancelTurn()
	if err != nil {
		EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryProcessingTurn, Resource: s.resource, Outcome: "error", ReservationKind: "processing", Dependency: "redis"})
		permit.reportCoordinationFailure(err)
		return schedulerStepBackoff, fmt.Errorf("fairqueue: acquire processing turn: %w", err)
	}
	if !found {
		EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryProcessingTurn, Resource: s.resource, Outcome: "empty", ReservationKind: "processing", Dependency: "redis"})
		return schedulerStepIdle, nil
	}
	EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryProcessingTurn, Resource: s.resource, Outcome: "granted", ReservationKind: "processing", Dependency: "redis"})
	if err := turn.Validate(); err != nil || turn.Token != ProcessingTurnToken(turnID) || turn.TenantID == "" {
		permit.reportCoordinationFailure(ErrCoordinationCorrupt)
		return schedulerStepBackoff, fmt.Errorf("%w: coordinator returned an invalid processing turn", ErrInvalidModel)
	}

	provisionalID, err := s.nextToken("provisional reservation")
	if err != nil {
		cleanupErr := s.rotateAndRelease(ctx, permit, turn, true, "", false)
		return schedulerStepBackoff, errors.Join(err, cleanupErr)
	}

	// One absolute deadline begins before Acquire and is transferred unchanged
	// to the worker. Provisional expiry itself remains authoritative Redis TIME;
	// this local context only bounds the complete prepare/bind window.
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, s.config.PrepareTimeout)
	prepareDeadline, _ := prepareCtx.Deadline()
	decision, err := s.coordinator.AcquireProvisional(
		prepareCtx,
		s.resource,
		fence,
		turn.TenantID,
		provisionalID,
		s.config.CapacityLimits(),
		s.config.ProvisionalTTL,
	)
	if err != nil {
		EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryReservation, Resource: s.resource, Outcome: "error", ReservationKind: "provisional", Dependency: "redis"})
		cancelPrepare()
		permit.reportCoordinationFailure(err)
		cleanupErr := s.rotateAndRelease(ctx, permit, turn, true, provisionalID, true)
		return schedulerStepBackoff, errors.Join(fmt.Errorf("fairqueue: acquire provisional reservation: %w", err), cleanupErr)
	}
	granted, decisionErr := classifyReservationDecision(decision)
	if decisionErr != nil {
		cancelPrepare()
		permit.reportCoordinationFailure(ErrCoordinationCorrupt)
		cleanupErr := s.rotateAndRelease(ctx, permit, turn, true, "", false)
		return schedulerStepBackoff, errors.Join(decisionErr, cleanupErr)
	}
	if !granted {
		EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryReservation, Resource: s.resource, Outcome: "denied", ReservationKind: "provisional", Dependency: "redis"})
		cancelPrepare()
		cleanupErr := s.rotateAndRelease(ctx, permit, turn, true, "", false)
		return schedulerStepBackoff, cleanupErr
	}
	EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryReservation, Resource: s.resource, Outcome: "granted", ReservationKind: "provisional", Dependency: "redis"})

	delivery, got, getErr := s.rabbit.GetOne(prepareCtx, s.resource, turn.TenantID)
	if errors.Is(getErr, ErrUnsupportedTopology) {
		// A permanent Rabbit topology incompatibility is resource-local. Close
		// this immutable gate generation, then let Run remain alive and idle so
		// unrelated resources sharing the Runtime are not torn down.
		permit.reportCoordinationFailure(getErr)
	}
	if delivery != nil && (getErr != nil || !got) {
		cancelPrepare()
		contractErr := getErr
		if contractErr == nil {
			contractErr = fmt.Errorf("%w: Rabbit client returned an unreported delivery", ErrInvalidModel)
		}
		cleanupErr := s.cleanupUnexpectedDelivery(ctx, permit, turn, provisionalID, delivery)
		return schedulerStepBackoff, errors.Join(
			fmt.Errorf("fairqueue: invalid tenant delivery result: %w", contractErr), cleanupErr,
		)
	}
	if getErr != nil {
		cancelPrepare()
		cleanupErr := s.rotateAndRelease(ctx, permit, turn, true, provisionalID, true)
		return schedulerStepBackoff, errors.Join(fmt.Errorf("fairqueue: get tenant delivery: %w", getErr), cleanupErr)
	}
	if got && delivery == nil {
		cancelPrepare()
		cleanupErr := s.rotateAndRelease(ctx, permit, turn, true, provisionalID, true)
		return schedulerStepBackoff, errors.Join(
			fmt.Errorf("%w: Rabbit client returned a nil delivery", ErrInvalidModel), cleanupErr,
		)
	}
	var releaseErr error
	if !got {
		// Empty basic.get must relinquish capacity before potentially slow depth
		// inspection. Release and turn settlement get independent bounded
		// contexts so a slow release cannot consume Rotate's entire opportunity.
		releaseCtx, cancelRelease := s.cleanupContext(ctx)
		releaseErr = s.releaseProvisionalWithContext(releaseCtx, permit, turn.TenantID, provisionalID)
		cancelRelease()
	}

	depth, depthErr := s.rabbit.ReadyDepth(prepareCtx, s.resource, turn.TenantID)
	depthOutcome := "ok"
	if depthErr != nil {
		depthOutcome = "error"
	}
	EmitTelemetry(ctx, s.telemetry, TelemetryEvent{Name: TelemetryRabbitDepth, Resource: s.resource, Outcome: depthOutcome, Dependency: "rabbitmq", Value: depth})
	if errors.Is(depthErr, ErrUnsupportedTopology) {
		permit.reportCoordinationFailure(depthErr)
	}
	hasReady := depthErr != nil || depth > 0
	if depth < 0 {
		hasReady = true
		depthErr = errors.Join(depthErr, fmt.Errorf("%w: Rabbit ready depth is negative", ErrInvalidModel))
	}
	rotateCtx := prepareCtx
	cancelRotate := func() {}
	if !got {
		rotateCtx, cancelRotate = s.cleanupContext(ctx)
	}
	rotateErr := s.rotateTurnWithContext(rotateCtx, permit, turn, hasReady)
	cancelRotate()

	if !got {
		cancelPrepare()
		if depthErr != nil || rotateErr != nil || releaseErr != nil {
			return schedulerStepBackoff, errors.Join(depthErr, rotateErr, releaseErr)
		}
		return schedulerStepIdle, nil
	}

	// Once basic.get returned a delivery, depth/rotate failure must not drop it.
	// The already admitted bounded worker owns canonical Prepare and settlement,
	// even if the same coordination error has just closed admission for new work.
	envelope := workerEnvelope{
		resource:        s.resource,
		tenant:          turn.TenantID,
		fence:           fence,
		gateGeneration:  gateGeneration,
		turn:            turn,
		provisionalID:   provisionalID,
		prepareCtx:      prepareCtx,
		prepareDeadline: prepareDeadline,
		cancelPrepare:   cancelPrepare,
		request:         delivery.Request(),
		delivery:        delivery,
	}
	if err := permit.start(envelope); err != nil {
		cancelPrepare()
		cleanupErr := s.cleanupFailedHandoff(ctx, permit, turn.TenantID, provisionalID, delivery)
		return schedulerStepBackoff, errors.Join(fmt.Errorf("fairqueue: hand off delivery: %w", err), depthErr, rotateErr, cleanupErr)
	}
	permitOwned = false
	// Runtime now owns both the prepare context and the permit. It ends the
	// prepare attempt after bind/settlement and releases the local slot only
	// after Run plus stable-reservation cleanup.
	if depthErr != nil || rotateErr != nil {
		return schedulerStepBackoff, errors.Join(depthErr, rotateErr)
	}
	return schedulerStepProgress, nil
}

func (s *Scheduler) nextToken(kind string) (string, error) {
	token, err := s.tokens.Next()
	if err != nil {
		return "", fmt.Errorf("%w: allocate %s: %v", errSchedulerTokenSource, kind, err)
	}
	if !lowerHex32Pattern.MatchString(token) {
		return "", errors.Join(
			errSchedulerTokenSource,
			fmt.Errorf("%w: %s token is not canonical 128-bit lowercase hex", ErrInvalidModel, kind),
		)
	}
	return token, nil
}

func classifyReservationDecision(decision ReservationDecision) (bool, error) {
	switch decision {
	case ReservationRegular, ReservationBorrowed:
		return true, nil
	case ReservationDeniedGlobalFull, ReservationDeniedTenantBurst,
		ReservationDeniedCompetition, ReservationDeniedBorrowOff:
		return false, nil
	default:
		return false, fmt.Errorf("%w: coordinator returned an unknown reservation decision", ErrInvalidModel)
	}
}

func (s *Scheduler) rotateAndRelease(
	ctx context.Context,
	permit schedulerWorkerPermit,
	turn ProcessingTurn,
	hasReady bool,
	provisionalID string,
	release bool,
) error {
	rotateCtx, cancelRotate := s.cleanupContext(ctx)
	rotateErr := s.rotateTurnWithContext(rotateCtx, permit, turn, hasReady)
	cancelRotate()
	if !release {
		return rotateErr
	}
	releaseCtx, cancelRelease := s.cleanupContext(ctx)
	releaseErr := s.releaseProvisionalWithContext(releaseCtx, permit, turn.TenantID, provisionalID)
	cancelRelease()
	return errors.Join(rotateErr, releaseErr)
}

func (s *Scheduler) rotateTurnWithContext(ctx context.Context, permit schedulerWorkerPermit, turn ProcessingTurn, hasReady bool) error {
	fence, _ := permit.gateSnapshot()
	err := s.coordinator.RotateOrDeactivate(
		ctx,
		s.resource,
		fence,
		turn.Token,
		turn.ObservedActivationGeneration,
		hasReady,
	)
	if err != nil {
		permit.reportCoordinationFailure(err)
		return fmt.Errorf("fairqueue: settle processing turn: %w", err)
	}
	return nil
}

func (s *Scheduler) releaseProvisionalWithContext(ctx context.Context, permit schedulerWorkerPermit, tenant, provisionalID string) error {
	fence, _ := permit.gateSnapshot()
	err := s.coordinator.Release(ctx, s.resource, fence, tenant, provisionalID)
	if err != nil {
		permit.reportCoordinationFailure(err)
		return fmt.Errorf("fairqueue: release provisional reservation: %w", err)
	}
	return nil
}

func (s *Scheduler) cleanupFailedHandoff(
	ctx context.Context,
	permit schedulerWorkerPermit,
	tenant, provisionalID string,
	delivery Delivery,
) error {
	fence, _ := permit.gateSnapshot()

	var result error
	nackCtx, cancelNack := s.cleanupContext(ctx)
	if err := delivery.Nack(nackCtx, true); err != nil {
		if errors.Is(err, ErrUnsupportedTopology) {
			permit.reportCoordinationFailure(err)
		}
		result = errors.Join(result, fmt.Errorf("fairqueue: requeue failed handoff: %w", err))
		cancelNack()
	} else {
		cancelNack()
		activateCtx, cancelActivate := s.cleanupContext(ctx)
		if err := s.coordinator.EnsureActive(
			activateCtx, s.resource, fence, tenant,
		); err != nil {
			permit.reportCoordinationFailure(err)
			result = errors.Join(result, fmt.Errorf("fairqueue: reactivate requeued tenant: %w", err))
		}
		cancelActivate()
	}
	releaseCtx, cancelRelease := s.cleanupContext(ctx)
	if err := s.coordinator.Release(
		releaseCtx, s.resource, fence, tenant, provisionalID,
	); err != nil {
		permit.reportCoordinationFailure(err)
		result = errors.Join(result, fmt.Errorf("fairqueue: release failed handoff reservation: %w", err))
	}
	cancelRelease()
	return result
}

func (s *Scheduler) cleanupUnexpectedDelivery(
	ctx context.Context,
	permit schedulerWorkerPermit,
	turn ProcessingTurn,
	provisionalID string,
	delivery Delivery,
) error {
	fence, _ := permit.gateSnapshot()

	var result error
	nackCtx, cancelNack := s.cleanupContext(ctx)
	nackErr := delivery.Nack(nackCtx, true)
	cancelNack()
	if nackErr != nil {
		if errors.Is(nackErr, ErrUnsupportedTopology) {
			permit.reportCoordinationFailure(nackErr)
		}
		result = errors.Join(result, fmt.Errorf("fairqueue: requeue unexpected delivery: %w", nackErr))
	} else {
		activateCtx, cancelActivate := s.cleanupContext(ctx)
		if err := s.coordinator.EnsureActive(activateCtx, s.resource, fence, turn.TenantID); err != nil {
			permit.reportCoordinationFailure(err)
			result = errors.Join(result, fmt.Errorf("fairqueue: reactivate unexpected delivery: %w", err))
		}
		cancelActivate()
	}
	releaseCtx, cancelRelease := s.cleanupContext(ctx)
	if err := s.releaseProvisionalWithContext(releaseCtx, permit, turn.TenantID, provisionalID); err != nil {
		result = errors.Join(result, err)
	}
	cancelRelease()
	rotateCtx, cancelRotate := s.cleanupContext(ctx)
	if err := s.rotateTurnWithContext(rotateCtx, permit, turn, true); err != nil {
		result = errors.Join(result, err)
	}
	cancelRotate()
	return result
}

func (s *Scheduler) cleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), s.options.CleanupTimeout)
}

func schedulerFatal(err error) bool {
	return errors.Is(err, errSchedulerTokenSource) ||
		errors.Is(err, ErrAuthoritativeWriterMismatch)
}

func waitSchedulerContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextSchedulerBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum-current {
		return maximum
	}
	return current * 2
}
