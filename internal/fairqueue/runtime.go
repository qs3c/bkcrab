package fairqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

const defaultRuntimeShutdownGrace = 30 * time.Second

var (
	errRuntimeRunning      = errors.New("fairqueue: runtime is already running")
	errRuntimeShuttingDown = errors.New("fairqueue: runtime is shutting down")
	errRuntimeNotFound     = errors.New("fairqueue: runtime resource is not registered")
)

// RuntimeCoordinator is the shared Redis-backed coordinator owned by Runtime.
// Per-resource schedulers never close this shared client themselves.
type RuntimeCoordinator interface {
	Coordinator
	Close() error
}

type RuntimeOptions struct {
	SchedulerIdleInterval time.Duration
	BackoffInitial        time.Duration
	BackoffMax            time.Duration
	CleanupTimeout        time.Duration
	ShutdownGrace         time.Duration
}

func (o RuntimeOptions) withDefaults() (RuntimeOptions, error) {
	if o.SchedulerIdleInterval == 0 {
		o.SchedulerIdleInterval = defaultSchedulerIdleInterval
	}
	if o.BackoffInitial == 0 {
		o.BackoffInitial = defaultSchedulerBackoffInitial
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = defaultSchedulerBackoffMaximum
	}
	if o.ShutdownGrace == 0 {
		o.ShutdownGrace = defaultRuntimeShutdownGrace
	}
	values := []struct {
		name      string
		value     time.Duration
		allowZero bool
	}{
		{"scheduler idle interval", o.SchedulerIdleInterval, false},
		{"initial backoff", o.BackoffInitial, false},
		{"maximum backoff", o.BackoffMax, false},
		{"cleanup timeout", o.CleanupTimeout, true},
		{"shutdown grace", o.ShutdownGrace, false},
	}
	for _, item := range values {
		if item.value == 0 && item.allowZero {
			continue
		}
		if item.value <= 0 || item.value > maxResourceDuration {
			return RuntimeOptions{}, fmt.Errorf("fairqueue: runtime %s must be in (0,%s]", item.name, maxResourceDuration)
		}
	}
	if o.BackoffInitial > o.BackoffMax {
		return RuntimeOptions{}, errors.New("fairqueue: runtime initial backoff exceeds maximum backoff")
	}
	return o, nil
}

// ResourceRegistration contains only domain-neutral dependencies. Runtime
// constructs the scheduler and an initially closed dispatcher around them.
type ResourceRegistration struct {
	Config             ResourceConfig
	Preparer           TaskPreparer
	DispatchSource     DispatchSource
	ExpiredRearmSource ExpiredRearmSource
}

type runtimeTokenSource interface {
	Next() (string, error)
}

type cryptoRuntimeTokenSource struct{}

func (cryptoRuntimeTokenSource) Next() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

type workerEnvelope struct {
	resource        string
	tenant          string
	fence           ResourceFence
	gateGeneration  uint64
	turn            ProcessingTurn
	provisionalID   string
	prepareCtx      context.Context
	prepareDeadline time.Time
	cancelPrepare   context.CancelFunc
	request         PrepareRequest
	delivery        Delivery
}

type Runtime struct {
	rabbit      RabbitClient
	coordinator RuntimeCoordinator
	options     RuntimeOptions
	tokens      runtimeTokenSource

	mu            sync.Mutex
	resources     map[string]*runtimeResource
	running       bool
	shuttingDown  bool
	runCancel     context.CancelFunc
	componentDone <-chan struct{}
	fatalErr      error
	fatalCh       chan struct{}
	fatalOnce     sync.Once

	shutdownOnce  sync.Once
	shutdownStart sync.Mutex
	shutdownDone  chan struct{}
	shutdownErr   error
}

type runtimeResource struct {
	runtime    *Runtime
	config     ResourceConfig
	preparer   TaskPreparer
	dispatcher *Dispatcher
	scheduler  *Scheduler

	gateOps        sync.Mutex
	mu             sync.Mutex
	gateOpen       bool
	fence          ResourceFence
	gateGeneration uint64
	prepareCount   int
	prepareDrained chan struct{}
	directCount    int
	directDrained  chan struct{}
	slotsInUse     int
	runningTasks   map[uint64]*runtimeRunningTask
	nextRunningID  uint64
	runningDrained chan struct{}
	cleanupTimeout time.Duration
	workCh         chan runtimeWork
	workStop       chan struct{}
	workStopOnce   sync.Once
}

type runtimeWork struct {
	permit   *runtimeWorkerPermit
	envelope workerEnvelope
}

type runtimeRunningTask struct {
	id          uint64
	cancel      context.CancelFunc
	tenant      string
	stableToken string
}

type runtimePermitState uint8

const (
	runtimePermitReserved runtimePermitState = iota
	runtimePermitStarted
	runtimePermitRunning
	runtimePermitFinished
)

type runtimeWorkerPermit struct {
	runtime        *Runtime
	resource       *runtimeResource
	fence          ResourceFence
	gateGeneration uint64

	mu        sync.Mutex
	state     runtimePermitState
	runningID uint64
}

func NewRuntime(rabbit RabbitClient, coordinator RuntimeCoordinator, options RuntimeOptions) (*Runtime, error) {
	if rabbit == nil {
		return nil, errors.New("fairqueue: runtime Rabbit client is required")
	}
	if coordinator == nil {
		return nil, errors.New("fairqueue: runtime coordinator is required")
	}
	normalized, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Runtime{
		rabbit: rabbit, coordinator: coordinator, options: normalized,
		tokens: cryptoRuntimeTokenSource{}, resources: make(map[string]*runtimeResource),
		fatalCh: make(chan struct{}), shutdownDone: make(chan struct{}),
	}, nil
}

func (r *Runtime) RegisterResource(registration ResourceRegistration) error {
	if r == nil {
		return errors.New("fairqueue: nil runtime")
	}
	if err := registration.Config.Validate(); err != nil {
		return fmt.Errorf("fairqueue: register resource: %w", err)
	}
	if registration.Preparer == nil || registration.DispatchSource == nil || registration.ExpiredRearmSource == nil {
		return errors.New("fairqueue: resource preparer, dispatch source, and expired rearm source are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running || r.shuttingDown {
		return errRuntimeShuttingDown
	}
	if _, exists := r.resources[registration.Config.Key]; exists {
		return fmt.Errorf("fairqueue: resource %q is already registered", registration.Config.Key)
	}
	dispatcherOptions := DispatcherOptions{
		PageSize:                    registration.Config.ReconcilePageSize,
		DispatchInterval:            registration.Config.DispatchInterval,
		ExpiredRunningSweepInterval: registration.Config.ExpiredRunningSweepInterval,
		PublishAttemptTimeout:       registration.Config.PublishAttemptTimeout,
		BackoffInitial:              r.options.BackoffInitial,
		BackoffMax:                  r.options.BackoffMax,
	}
	dispatcher, err := NewClosedDispatcher(registration.Config.Key, registration.DispatchSource,
		registration.ExpiredRearmSource, r.rabbit, r.coordinator, dispatcherOptions)
	if err != nil {
		return err
	}
	resource := &runtimeResource{
		runtime: r, config: registration.Config, preparer: registration.Preparer,
		dispatcher: dispatcher, gateGeneration: 1,
		runningTasks:   make(map[uint64]*runtimeRunningTask),
		cleanupTimeout: r.options.CleanupTimeout,
		workCh:         make(chan runtimeWork, registration.Config.LocalWorkers),
		workStop:       make(chan struct{}),
	}
	if resource.cleanupTimeout == 0 {
		resource.cleanupTimeout = defaultSchedulerCleanupTimeout(registration.Config)
	}
	resource.prepareDrained = closedRuntimeChannel()
	resource.directDrained = closedRuntimeChannel()
	resource.runningDrained = closedRuntimeChannel()
	schedulerOptions := SchedulerOptions{
		IdleInterval:   r.options.SchedulerIdleInterval,
		BackoffInitial: r.options.BackoffInitial,
		BackoffMax:     r.options.BackoffMax,
		CleanupTimeout: resource.cleanupTimeout,
	}
	scheduler, err := newScheduler(r, registration.Config, r.rabbit, r.coordinator, r.tokens, schedulerOptions)
	if err != nil {
		return err
	}
	resource.scheduler = scheduler

	r.resources[registration.Config.Key] = resource
	return nil
}

func closedRuntimeChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (r *Runtime) resource(resource string) (*runtimeResource, error) {
	if r == nil {
		return nil, errors.New("fairqueue: nil runtime")
	}
	if err := ValidateResource(resource); err != nil {
		return nil, err
	}
	r.mu.Lock()
	entry := r.resources[resource]
	r.mu.Unlock()
	if entry == nil {
		return nil, fmt.Errorf("%w: %s", errRuntimeNotFound, resource)
	}
	return entry, nil
}

// OpenResource installs one reconciled READY fence in both local gates. It
// never overlaps a previous prepare or publisher attempt generation.
func (r *Runtime) OpenResource(resource string, fence ResourceFence) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	entry, err := r.resource(resource)
	if err != nil {
		return err
	}
	entry.gateOps.Lock()
	defer entry.gateOps.Unlock()
	r.mu.Lock()
	stopping := r.shuttingDown || r.fatalErr != nil
	r.mu.Unlock()
	if stopping {
		return errRuntimeShuttingDown
	}
	entry.mu.Lock()
	if entry.prepareCount != 0 {
		entry.mu.Unlock()
		return errors.Join(ErrResourceNotReady, errors.New("fairqueue: prepare attempts have not drained"))
	}
	entry.mu.Unlock()
	if err := entry.dispatcher.OpenPublisherGate(fence); err != nil {
		return err
	}
	entry.mu.Lock()
	if entry.prepareCount != 0 {
		entry.mu.Unlock()
		entry.dispatcher.ClosePublisherGate()
		return errors.Join(ErrResourceNotReady, errors.New("fairqueue: prepare admission changed while opening"))
	}
	entry.fence = fence
	entry.gateOpen = true
	entry.gateGeneration++
	entry.mu.Unlock()
	return nil
}

// CloseResource synchronously prevents new turns and new publish attempts.
// Already admitted bounded work retains its immutable fence snapshot.
func (r *Runtime) CloseResource(resource string) error {
	entry, err := r.resource(resource)
	if err != nil {
		return err
	}
	entry.closeGates()
	return nil
}

func (entry *runtimeResource) closeGates() {
	entry.gateOps.Lock()
	entry.mu.Lock()
	entry.gateOpen = false
	entry.gateGeneration++
	entry.mu.Unlock()
	entry.dispatcher.ClosePublisherGate()
	entry.gateOps.Unlock()
}

func (entry *runtimeResource) closeGatesFor(fence ResourceFence, generation uint64) {
	entry.gateOps.Lock()
	entry.mu.Lock()
	matched := entry.gateOpen && entry.fence == fence && entry.gateGeneration == generation
	if matched {
		entry.gateOpen = false
		entry.gateGeneration++
	}
	entry.mu.Unlock()
	if matched {
		entry.dispatcher.ClosePublisherGate()
	}
	entry.gateOps.Unlock()
}

// WaitForAttemptDrain waits for scheduler prepare attempts and dispatcher
// attempts. Long-running PreparedTask.Run calls are deliberately excluded.
func (r *Runtime) WaitForAttemptDrain(ctx context.Context, resource string) error {
	if ctx == nil {
		return errors.New("fairqueue: nil runtime drain context")
	}
	entry, err := r.resource(resource)
	if err != nil {
		return err
	}
	entry.mu.Lock()
	prepareDrained := entry.prepareDrained
	entry.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-prepareDrained:
	}
	return entry.dispatcher.WaitForPublisherDrain(ctx)
}

func (r *Runtime) TryDispatch(ctx context.Context, resource, taskID string) (bool, error) {
	entry, err := r.resource(resource)
	if err != nil {
		return false, err
	}
	if !entry.config.ValidateTaskID(taskID) {
		return false, fmt.Errorf("%w: task ID is invalid for resource %q", ErrInvalidModel, resource)
	}
	if err := r.beginDirectDispatch(entry); err != nil {
		return false, err
	}
	defer entry.finishDirectDispatch()
	dispatched, err := entry.dispatcher.TryDispatch(ctx, taskID)
	if errors.Is(err, ErrAuthoritativeWriterMismatch) && !isStalePublisherSourceFailure(err) {
		r.failAuthoritativeWriter(err)
	}
	return dispatched, err
}

func (r *Runtime) beginDirectDispatch(entry *runtimeResource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.shuttingDown || r.fatalErr != nil {
		return errRuntimeShuttingDown
	}
	entry.mu.Lock()
	if entry.directCount == 0 {
		entry.directDrained = make(chan struct{})
	}
	entry.directCount++
	entry.mu.Unlock()
	return nil
}

func (entry *runtimeResource) finishDirectDispatch() {
	entry.mu.Lock()
	if entry.directCount > 0 {
		entry.directCount--
		if entry.directCount == 0 {
			close(entry.directDrained)
		}
	}
	entry.mu.Unlock()
}

func (r *Runtime) tryReserveWorker(resource string) (*runtimeWorkerPermit, bool) {
	entry, err := r.resource(resource)
	if err != nil {
		return nil, false
	}
	r.mu.Lock()
	stopping := r.shuttingDown || r.fatalErr != nil
	r.mu.Unlock()
	if stopping {
		return nil, false
	}
	// The dispatcher closes its own publisher gate synchronously when an
	// authoritative READY check fails. Coupling admission to that gate makes
	// the same failure stop new scheduler turns without a polling window. A
	// close racing after this check merely leaves one already-admitted bounded
	// attempt, which is the intended check-before-transition rule.
	if !entry.dispatcher.PublisherGateOpen() {
		return nil, false
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if !entry.gateOpen || entry.slotsInUse >= entry.config.LocalWorkers {
		return nil, false
	}
	if entry.prepareCount == 0 {
		entry.prepareDrained = make(chan struct{})
	}
	entry.prepareCount++
	entry.slotsInUse++
	return &runtimeWorkerPermit{
		runtime: r, resource: entry, fence: entry.fence,
		gateGeneration: entry.gateGeneration, state: runtimePermitReserved,
	}, true
}

func (p *runtimeWorkerPermit) gateSnapshot() (ResourceFence, uint64) {
	if p == nil {
		return ResourceFence{}, 0
	}
	return p.fence, p.gateGeneration
}

func (p *runtimeWorkerPermit) reportCoordinationFailure(err error) {
	if p == nil || err == nil {
		return
	}
	if errors.Is(err, ErrAuthoritativeWriterMismatch) {
		p.runtime.failAuthoritativeWriter(err)
		return
	}
	if runtimeCoordinationGateError(err) {
		p.resource.closeGatesFor(p.fence, p.gateGeneration)
	}
}

func runtimeCoordinationGateError(err error) bool {
	return errors.Is(err, ErrDependencyUnavailable) || errors.Is(err, ErrUnsupportedTopology) ||
		errors.Is(err, ErrResourceNotReady) || errors.Is(err, ErrFenceMismatch) ||
		errors.Is(err, ErrRecoveryOwnerStale) || errors.Is(err, ErrCoordinationCorrupt)
}

func (p *runtimeWorkerPermit) start(envelope workerEnvelope) error {
	if p == nil || envelope.delivery == nil || envelope.prepareCtx == nil || envelope.cancelPrepare == nil ||
		envelope.resource != p.resource.config.Key || envelope.fence != p.fence ||
		envelope.gateGeneration != p.gateGeneration || envelope.tenant == "" || envelope.provisionalID == "" {
		return fmt.Errorf("%w: invalid worker handoff", ErrInvalidModel)
	}
	p.mu.Lock()
	if p.state != runtimePermitReserved {
		p.mu.Unlock()
		return errors.New("fairqueue: worker permit is already consumed")
	}
	p.state = runtimePermitStarted
	p.mu.Unlock()
	select {
	case p.resource.workCh <- runtimeWork{permit: p, envelope: envelope}:
		return nil
	case <-p.resource.workStop:
		p.mu.Lock()
		p.state = runtimePermitReserved
		p.mu.Unlock()
		return errRuntimeShuttingDown
	}
}

func (p *runtimeWorkerPermit) abort() {
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.state != runtimePermitReserved {
		p.mu.Unlock()
		return
	}
	p.state = runtimePermitFinished
	p.mu.Unlock()
	p.resource.finishPrepareAndSlot()
}

func (entry *runtimeResource) finishPrepareLocked() {
	if entry.prepareCount <= 0 {
		return
	}
	entry.prepareCount--
	if entry.prepareCount == 0 {
		close(entry.prepareDrained)
	}
}

func (entry *runtimeResource) finishPrepareAndSlot() {
	entry.mu.Lock()
	entry.finishPrepareLocked()
	if entry.slotsInUse > 0 {
		entry.slotsInUse--
	}
	entry.mu.Unlock()
}

func (p *runtimeWorkerPermit) finishWithoutRun() {
	p.mu.Lock()
	if p.state != runtimePermitStarted {
		p.mu.Unlock()
		return
	}
	p.state = runtimePermitFinished
	p.mu.Unlock()
	p.resource.finishPrepareAndSlot()
}

func (p *runtimeWorkerPermit) beginRun(
	prepareCtx context.Context,
	cancel context.CancelFunc,
	tenant, stableToken string,
) (uint64, bool) {
	p.mu.Lock()
	if p.state != runtimePermitStarted {
		p.mu.Unlock()
		return 0, false
	}
	// Serialize the running-registry handoff with publication of the global
	// authoritative-writer fatal state. Therefore fatal cancellation either
	// sees this task in runningTasks, or this transition observes fatalErr and
	// refuses to start it; there is no snapshot gap between those outcomes.
	p.runtime.mu.Lock()
	if p.runtime.fatalErr != nil || prepareCtx == nil || prepareCtx.Err() != nil {
		p.runtime.mu.Unlock()
		p.mu.Unlock()
		return 0, false
	}
	p.resource.mu.Lock()
	p.resource.nextRunningID++
	id := p.resource.nextRunningID
	if len(p.resource.runningTasks) == 0 {
		p.resource.runningDrained = make(chan struct{})
	}
	p.resource.runningTasks[id] = &runtimeRunningTask{
		id: id, cancel: cancel, tenant: tenant, stableToken: stableToken,
	}
	p.resource.finishPrepareLocked()
	p.resource.mu.Unlock()
	p.runtime.mu.Unlock()
	p.state = runtimePermitRunning
	p.runningID = id
	p.mu.Unlock()
	return id, true
}

// finishAfterPanic makes the outer worker boundary leak-proof even if an
// injected adapter panics after the permit has transitioned to running.
func (p *runtimeWorkerPermit) finishAfterPanic() {
	if p == nil {
		return
	}
	p.mu.Lock()
	state := p.state
	id := p.runningID
	if state != runtimePermitStarted && state != runtimePermitRunning {
		p.mu.Unlock()
		return
	}
	p.state = runtimePermitFinished
	p.mu.Unlock()
	if state == runtimePermitStarted {
		p.resource.finishPrepareAndSlot()
		return
	}

	p.resource.mu.Lock()
	running := p.resource.runningTasks[id]
	delete(p.resource.runningTasks, id)
	if len(p.resource.runningTasks) == 0 {
		close(p.resource.runningDrained)
	}
	if p.resource.slotsInUse > 0 {
		p.resource.slotsInUse--
	}
	p.resource.mu.Unlock()
	if running != nil {
		running.cancel()
	}
}

func (p *runtimeWorkerPermit) finishRun() {
	p.mu.Lock()
	if p.state != runtimePermitRunning {
		p.mu.Unlock()
		return
	}
	id := p.runningID
	p.state = runtimePermitFinished
	p.mu.Unlock()
	p.resource.mu.Lock()
	delete(p.resource.runningTasks, id)
	if len(p.resource.runningTasks) == 0 {
		close(p.resource.runningDrained)
	}
	if p.resource.slotsInUse > 0 {
		p.resource.slotsInUse--
	}
	p.resource.mu.Unlock()
}

func (p *runtimeWorkerPermit) stateSnapshot() runtimePermitState {
	if p == nil {
		return runtimePermitFinished
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (r *Runtime) executeWorker(permit *runtimeWorkerPermit, envelope workerEnvelope) {
	defer func() {
		envelope.cancelPrepare()
		if recover() != nil {
			if permit.stateSnapshot() == runtimePermitStarted {
				// A panicking adapter must not strand an unacked delivery or its
				// provisional capacity in an otherwise healthy long-lived process.
				// Once running, however, the delivery was already ACKed and must
				// never be NACKed a second time.
				permit.resource.closeGatesFor(envelope.fence, envelope.gateGeneration)
				func() {
					defer func() { _ = recover() }()
					r.nackActivateRelease(permit, envelope)
				}()
			}
			permit.finishAfterPanic()
		}
	}()

	if err := validateWorkerEnvelopeContext(permit.resource.config, envelope); err != nil {
		permit.resource.closeGatesFor(envelope.fence, envelope.gateGeneration)
		r.nackActivateRelease(permit, envelope)
		permit.finishWithoutRun()
		return
	}

	prepared, result, prepareErr := permit.resource.preparer.Prepare(envelope.prepareCtx, envelope.request)
	if prepareErr != nil {
		if errors.Is(prepareErr, ErrAuthoritativeWriterMismatch) {
			r.failAuthoritativeWriter(prepareErr)
		}
		r.nackActivateRelease(permit, envelope)
		permit.finishWithoutRun()
		return
	}
	if err := result.ValidateFor(envelope.request, prepared); err != nil {
		permit.resource.closeGatesFor(envelope.fence, envelope.gateGeneration)
		r.nackActivateRelease(permit, envelope)
		permit.finishWithoutRun()
		return
	}

	if result.Disposition == PrepareClaimed {
		if envelope.prepareCtx.Err() != nil {
			r.ackReleaseProvisional(permit, envelope)
			permit.finishWithoutRun()
			return
		}
		stableToken, err := StableReservationToken(
			envelope.resource, result.Claim.TaskID, result.Claim.ClaimGeneration,
		)
		if err != nil {
			permit.resource.closeGatesFor(envelope.fence, envelope.gateGeneration)
			r.ackReleaseProvisional(permit, envelope)
			permit.finishWithoutRun()
			return
		}
		if err := r.coordinator.BindReservation(
			envelope.prepareCtx, envelope.resource, envelope.fence, envelope.tenant,
			envelope.provisionalID, stableToken, permit.resource.config.ReservationTTL,
		); err != nil {
			if envelope.prepareCtx.Err() == nil {
				permit.reportCoordinationFailure(err)
			}
			// Bind is atomic, but a transport error cannot tell the caller whether
			// the reply or the commit was lost. Releasing both names is safe and
			// prevents either outcome from retaining capacity until TTL expiry.
			r.ackReleaseTokens(permit, envelope, envelope.provisionalID, stableToken)
			permit.finishWithoutRun()
			return
		}
		// Bind may have committed just as the one absolute prepare deadline (or
		// shutdown cancellation) fired. The stable reservation must be released
		// and the canonical claim consumed, but work that crossed that deadline
		// is never allowed to enter Run.
		if envelope.prepareCtx.Err() != nil {
			r.ackReleaseTokens(permit, envelope, envelope.provisionalID, stableToken)
			permit.finishWithoutRun()
			return
		}

		// A confirmed bind makes the canonical claim runnable. Rabbit ACK failure
		// only causes a duplicate delivery; exact claim fencing handles it.
		cleanupCtx, cancelCleanup := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
		_ = envelope.delivery.Ack(cleanupCtx)
		cancelCleanup()
		if envelope.prepareCtx.Err() != nil {
			r.releaseReservationTokens(permit, envelope, envelope.provisionalID, stableToken)
			permit.finishWithoutRun()
			return
		}
		runCtx, cancelRun := context.WithCancel(context.WithoutCancel(envelope.prepareCtx))
		if _, ok := permit.beginRun(envelope.prepareCtx, cancelRun, envelope.tenant, stableToken); !ok {
			cancelRun()
			r.releaseReservationTokens(permit, envelope, envelope.provisionalID, stableToken)
			permit.finishWithoutRun()
			return
		}
		envelope.cancelPrepare()
		r.runPrepared(permit, envelope, prepared, stableToken, runCtx, cancelRun)
		return
	}

	switch result.DeliveryAction {
	case DeliveryNackRequeue:
		r.nackActivateRelease(permit, envelope)
	case DeliveryAckRelease:
		r.ackReleaseProvisional(permit, envelope)
	case DeliveryConfirmDLQThenAck:
		r.deadLetterSettleRelease(permit, envelope)
	default:
		permit.resource.closeGatesFor(envelope.fence, envelope.gateGeneration)
		r.nackActivateRelease(permit, envelope)
	}
	permit.finishWithoutRun()
}

func validateWorkerEnvelopeContext(config ResourceConfig, envelope workerEnvelope) error {
	if err := envelope.request.Validate(); err != nil {
		return err
	}
	locators := []*Message{envelope.request.Message, envelope.request.BodyCandidate}
	for _, locator := range locators {
		if locator != nil && !config.ValidateTaskID(locator.TaskID) {
			return fmt.Errorf("%w: delivery task ID is invalid for the registered resource", ErrInvalidModel)
		}
	}
	if envelope.request.HeaderToken != nil && !config.ValidateTaskID(envelope.request.HeaderToken.TaskID) {
		return fmt.Errorf("%w: delivery header task ID is invalid for the registered resource", ErrInvalidModel)
	}
	if envelope.request.RegisteredResource != envelope.resource {
		return fmt.Errorf("%w: delivery registered resource does not match selected resource", ErrInvalidModel)
	}
	queueTenantHash, err := TenantHash(envelope.resource, envelope.tenant)
	if err != nil {
		return err
	}
	if envelope.request.QueueTenantHash != queueTenantHash {
		return fmt.Errorf("%w: delivery queue context does not match selected tenant", ErrInvalidModel)
	}
	return nil
}

func (r *Runtime) workerCleanupContext(entry *runtimeResource, parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), entry.cleanupTimeout)
}

func runtimeBudgetStep(budget context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(budget)
}

func (p *runtimeWorkerPermit) reportCleanupFailure(ctx context.Context, err error) {
	if err != nil && ctx.Err() == nil {
		p.reportCoordinationFailure(err)
	}
}

func (r *Runtime) nackActivateRelease(permit *runtimeWorkerPermit, envelope workerEnvelope) {
	nackCtx, cancelNack := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
	nackErr := envelope.delivery.Nack(nackCtx, true)
	cancelNack()
	if nackErr == nil {
		activeCtx, cancelActive := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
		if err := r.coordinator.EnsureActive(activeCtx, envelope.resource, envelope.fence, envelope.tenant); err != nil {
			permit.reportCleanupFailure(activeCtx, err)
		}
		cancelActive()
	}
	releaseCtx, cancelRelease := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
	if err := r.coordinator.Release(
		releaseCtx, envelope.resource, envelope.fence, envelope.tenant, envelope.provisionalID,
	); err != nil {
		permit.reportCleanupFailure(releaseCtx, err)
	}
	cancelRelease()
}

func (r *Runtime) ackReleaseProvisional(permit *runtimeWorkerPermit, envelope workerEnvelope) {
	r.ackReleaseTokens(permit, envelope, envelope.provisionalID)
}

func (r *Runtime) ackReleaseTokens(permit *runtimeWorkerPermit, envelope workerEnvelope, tokens ...string) {
	ackCtx, cancelAck := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
	_ = envelope.delivery.Ack(ackCtx)
	cancelAck()
	r.releaseReservationTokens(permit, envelope, tokens...)
}

func (r *Runtime) releaseReservationTokens(permit *runtimeWorkerPermit, envelope workerEnvelope, tokens ...string) {
	for _, token := range tokens {
		if token == "" {
			continue
		}
		releaseCtx, cancelRelease := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
		if err := r.coordinator.Release(
			releaseCtx, envelope.resource, envelope.fence, envelope.tenant, token,
		); err != nil {
			permit.reportCleanupFailure(releaseCtx, err)
		}
		cancelRelease()
	}
}

func (r *Runtime) deadLetterSettleRelease(permit *runtimeWorkerPermit, envelope workerEnvelope) {
	publishCtx, cancelPublish := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
	receipt, publishErr := r.rabbit.PublishDeadLetterConfirmed(publishCtx, DeadLetterRequest{
		Delivery: envelope.request, ReasonCode: "poison-permanent-invalid-message",
	})
	cancelPublish()
	if publishErr == nil {
		publishErr = receipt.Validate()
	}
	if publishErr == nil {
		ackCtx, cancelAck := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
		_ = envelope.delivery.Ack(ackCtx)
		cancelAck()
	} else {
		nackCtx, cancelNack := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
		nackErr := envelope.delivery.Nack(nackCtx, true)
		cancelNack()
		if nackErr == nil {
			activeCtx, cancelActive := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
			if err := r.coordinator.EnsureActive(activeCtx, envelope.resource, envelope.fence, envelope.tenant); err != nil {
				permit.reportCleanupFailure(activeCtx, err)
			}
			cancelActive()
		}
	}
	releaseCtx, cancelRelease := r.workerCleanupContext(permit.resource, envelope.prepareCtx)
	if err := r.coordinator.Release(
		releaseCtx, envelope.resource, envelope.fence, envelope.tenant, envelope.provisionalID,
	); err != nil {
		permit.reportCleanupFailure(releaseCtx, err)
	}
	cancelRelease()
}

func (r *Runtime) runPrepared(
	permit *runtimeWorkerPermit,
	envelope workerEnvelope,
	prepared PreparedTask,
	stableToken string,
	runCtx context.Context,
	cancelRun context.CancelFunc,
) {
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(permit.resource.config.ReservationHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				fence, generation, open := permit.resource.readySnapshot()
				if !open {
					continue
				}
				heartbeatCtx, cancel := r.workerCleanupContext(permit.resource, runCtx)
				err := r.coordinator.RenewStable(
					heartbeatCtx, envelope.resource, fence, envelope.tenant,
					stableToken, permit.resource.config.ReservationTTL,
				)
				cancel()
				if err != nil {
					r.handleCoordinationFailure(permit.resource, fence, generation, err)
				}
			}
		}
	}()

	runErr := runPreparedTaskSafely(prepared, runCtx)
	if errors.Is(runErr, ErrAuthoritativeWriterMismatch) {
		r.failAuthoritativeWriter(runErr)
	}
	cancelRun()
	<-heartbeatDone

	fence, generation, _ := permit.resource.latestFence(envelope.fence, envelope.gateGeneration)
	releaseCtx, cancelRelease := r.workerCleanupContext(permit.resource, runCtx)
	if err := r.coordinator.Release(
		releaseCtx, envelope.resource, fence, envelope.tenant, stableToken,
	); err != nil {
		r.handleCoordinationFailure(permit.resource, fence, generation, err)
	}
	cancelRelease()
	permit.finishRun()
}

func runPreparedTaskSafely(prepared PreparedTask, ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("fairqueue: prepared task panic: %v", recovered)
		}
	}()
	return prepared.Run(ctx)
}

func (entry *runtimeResource) readySnapshot() (ResourceFence, uint64, bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.fence, entry.gateGeneration, entry.gateOpen
}

func (entry *runtimeResource) latestFence(fallback ResourceFence, fallbackGeneration uint64) (ResourceFence, uint64, bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.fence.Validate() == nil {
		return entry.fence, entry.gateGeneration, entry.gateOpen
	}
	return fallback, fallbackGeneration, false
}

func (r *Runtime) handleCoordinationFailure(entry *runtimeResource, fence ResourceFence, generation uint64, err error) {
	if err == nil {
		return
	}
	if errors.Is(err, ErrAuthoritativeWriterMismatch) {
		r.failAuthoritativeWriter(err)
		return
	}
	if runtimeCoordinationGateError(err) {
		entry.closeGatesFor(fence, generation)
	}
}

// Run starts fixed per-resource worker pools before the scheduler and durable
// dispatcher loops. Any terminal component error shuts the shared runtime down.
func (r *Runtime) Run(ctx context.Context) error {
	if r == nil {
		return errors.New("fairqueue: nil runtime")
	}
	if ctx == nil {
		return errors.New("fairqueue: nil runtime context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return errRuntimeRunning
	}
	if r.shuttingDown {
		err := r.fatalErr
		r.mu.Unlock()
		if err != nil {
			return err
		}
		return errRuntimeShuttingDown
	}
	r.running = true
	runCtx, runCancel := context.WithCancel(ctx)
	r.runCancel = runCancel
	resources := make([]*runtimeResource, 0, len(r.resources))
	for _, entry := range r.resources {
		resources = append(resources, entry)
	}
	componentCount := len(resources) * 2
	componentDone := make(chan struct{})
	r.componentDone = componentDone
	var componentWG sync.WaitGroup
	componentWG.Add(componentCount)
	if componentCount == 0 {
		close(componentDone)
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.running = false
		r.runCancel = nil
		r.componentDone = nil
		r.mu.Unlock()
	}()

	for _, entry := range resources {
		entry.startWorkers()
	}
	results := make(chan error, max(1, componentCount))
	if componentCount != 0 {
		go func() {
			componentWG.Wait()
			close(componentDone)
		}()
	}
	for _, entry := range resources {
		entry := entry
		go func() {
			defer componentWG.Done()
			r.runComponent(runCtx, results, entry, entry.scheduler.Run)
		}()
		go func() {
			defer componentWG.Done()
			r.runComponent(runCtx, results, entry, entry.dispatcher.Run)
		}()
	}

	var cause error
	received := 0
	if componentCount == 0 {
		select {
		case <-ctx.Done():
			cause = ctx.Err()
		case <-runCtx.Done():
			cause = runCtx.Err()
		case <-r.fatalCh:
			r.mu.Lock()
			cause = r.fatalErr
			r.mu.Unlock()
		}
	} else {
		select {
		case <-ctx.Done():
			cause = ctx.Err()
		case <-runCtx.Done():
			cause = runCtx.Err()
		case <-r.fatalCh:
			r.mu.Lock()
			cause = r.fatalErr
			r.mu.Unlock()
		case cause = <-results:
			received++
		}
	}
	r.initiateShutdown()
	<-r.shutdownDone
	runCancel()
	// Every component observes runCtx cancellation. Drain their result sends so
	// no lifecycle goroutine is left behind when Run returns.
	drainTimer := time.NewTimer(r.options.ShutdownGrace)
	defer drainTimer.Stop()
	for received < componentCount {
		select {
		case <-results:
			received++
		case <-drainTimer.C:
			received = componentCount
		}
	}
	r.mu.Lock()
	fatalErr := r.fatalErr
	r.mu.Unlock()
	if fatalErr != nil {
		return fatalErr
	}
	if cause != nil {
		return cause
	}
	return r.shutdownErr
}

func (r *Runtime) runComponent(
	ctx context.Context,
	results chan<- error,
	entry *runtimeResource,
	run func(context.Context) error,
) {
	for {
		err := run(ctx)
		// Source reads are deliberately outside the bounded publisher-attempt
		// counter. If recovery closed and reopened the same fence while one old
		// read was in flight, Dispatcher leaves the new generation open; do not
		// let that stale result tear the reopened Runtime down. Restart the local
		// component so it observes the new generation. A current-generation
		// writer mismatch always closes the dispatcher gate before reaching here.
		if errors.Is(err, ErrAuthoritativeWriterMismatch) &&
			isStalePublisherSourceFailure(err) && ctx.Err() == nil {
			continue
		}
		if errors.Is(err, ErrAuthoritativeWriterMismatch) {
			r.failAuthoritativeWriter(err)
		}
		results <- err
		return
	}
}

func (entry *runtimeResource) startWorkers() {
	for index := 0; index < entry.config.LocalWorkers; index++ {
		go func() {
			for {
				select {
				case work := <-entry.workCh:
					if work.permit != nil {
						entry.runtime.executeWorker(work.permit, work.envelope)
					}
				case <-entry.workStop:
					return
				}
			}
		}()
	}
}

func (r *Runtime) failAuthoritativeWriter(err error) {
	if err == nil || !errors.Is(err, ErrAuthoritativeWriterMismatch) {
		return
	}
	r.fatalOnce.Do(func() {
		r.mu.Lock()
		r.fatalErr = err
		resources := make([]*runtimeResource, 0, len(r.resources))
		for _, entry := range r.resources {
			resources = append(resources, entry)
		}
		r.mu.Unlock()
		// initiateShutdown synchronously closes both admission gates before
		// component cancellation. A writer mismatch additionally cancels every
		// already-running task immediately instead of granting normal grace.
		r.initiateShutdown()
		for _, entry := range resources {
			entry.cancelRunning()
		}
		close(r.fatalCh)
	})
}

func (entry *runtimeResource) cancelRunning() {
	entry.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(entry.runningTasks))
	for _, task := range entry.runningTasks {
		cancels = append(cancels, task.cancel)
	}
	entry.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (r *Runtime) initiateShutdown() {
	if r == nil {
		return
	}
	// Serialize the transition, gate close, and sequence launch. Otherwise a
	// second caller could launch shutdownSequence while the first caller was
	// still closing a later resource's publisher gate.
	r.shutdownStart.Lock()
	r.mu.Lock()
	first := !r.shuttingDown
	r.shuttingDown = true
	resources := make([]*runtimeResource, 0, len(r.resources))
	if first {
		for _, entry := range r.resources {
			resources = append(resources, entry)
		}
	}
	r.mu.Unlock()
	if first {
		for _, entry := range resources {
			entry.closeGates()
		}
	}
	r.shutdownOnce.Do(func() { go r.shutdownSequence() })
	r.shutdownStart.Unlock()
}

// Shutdown starts one shared close sequence. Repeated callers wait for that
// same sequence and never close Rabbit or Redis more than once.
func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return errors.New("fairqueue: nil runtime")
	}
	if ctx == nil {
		return errors.New("fairqueue: nil runtime shutdown context")
	}
	r.initiateShutdown()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.shutdownDone:
		return r.shutdownErr
	}
}

func (r *Runtime) shutdownSequence() {
	r.mu.Lock()
	runCancel := r.runCancel
	componentDone := r.componentDone
	resources := make([]*runtimeResource, 0, len(r.resources))
	for _, entry := range r.resources {
		resources = append(resources, entry)
	}
	r.mu.Unlock()
	if runCancel != nil {
		runCancel()
	}

	// Attempts have their own drain budget, so a slow confirm or Prepare call
	// cannot consume the grace reserved for already-running business tasks.
	// ResourceConfig already proves every prepare/publish deadline is shorter
	// than RecoveryDrainTimeout; use the largest registered value because all
	// resources drain concurrently while this loop observes them.
	attemptDrainTimeout := r.options.ShutdownGrace
	for _, entry := range resources {
		if entry.config.RecoveryDrainTimeout > attemptDrainTimeout {
			attemptDrainTimeout = entry.config.RecoveryDrainTimeout
		}
	}
	attemptCtx, cancelAttempts := context.WithTimeout(context.Background(), attemptDrainTimeout)
	for _, entry := range resources {
		_ = r.waitEntryAttempts(attemptCtx, entry)
	}
	if componentDone != nil {
		select {
		case <-componentDone:
		case <-attemptCtx.Done():
		}
	}
	cancelAttempts()

	runGraceCtx, cancelRunGrace := context.WithTimeout(context.Background(), r.options.ShutdownGrace)
	for _, entry := range resources {
		if err := entry.waitRunning(runGraceCtx); err != nil {
			break
		}
	}
	cancelRunGrace()
	r.forceReleaseRunning(resources)
	for _, entry := range resources {
		entry.stopWorkersWhenDrained()
	}
	r.shutdownErr = errors.Join(r.rabbit.Close(), r.coordinator.Close())
	close(r.shutdownDone)
}

type runtimeForcedRelease struct {
	entry       *runtimeResource
	cancel      context.CancelFunc
	tenant      string
	stableToken string
}

func (r *Runtime) forceReleaseRunning(resources []*runtimeResource) {
	releases := make([]runtimeForcedRelease, 0)
	for _, entry := range resources {
		entry.mu.Lock()
		for _, task := range entry.runningTasks {
			releases = append(releases, runtimeForcedRelease{
				entry: entry, cancel: task.cancel, tenant: task.tenant, stableToken: task.stableToken,
			})
		}
		entry.mu.Unlock()
	}
	if len(releases) == 0 {
		return
	}

	// A task may ignore cancellation indefinitely. Stable capacity still gets
	// one total bounded cleanup budget, with a fresh child context per token,
	// before the shared clients are closed.
	budget, cancelBudget := context.WithTimeout(context.Background(), r.options.ShutdownGrace)
	defer cancelBudget()
	for _, release := range releases {
		release.cancel()
		fence, _, _ := release.entry.latestFence(ResourceFence{}, 0)
		if fence.Validate() != nil || release.tenant == "" || release.stableToken == "" {
			continue
		}
		releaseCtx, cancelRelease := runtimeBudgetStep(budget)
		_ = r.coordinator.Release(
			releaseCtx, release.entry.config.Key, fence, release.tenant, release.stableToken,
		)
		cancelRelease()
	}
}

func (r *Runtime) waitEntryAttempts(ctx context.Context, entry *runtimeResource) error {
	entry.mu.Lock()
	prepareDrained := entry.prepareDrained
	entry.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-prepareDrained:
	}
	if err := entry.dispatcher.WaitForPublisherDrain(ctx); err != nil {
		return err
	}
	// Dispatcher publisher drain intentionally ends at canonical Mark, while
	// activation is a separate bounded operation. Direct TryDispatch calls are
	// not owned by a background component, so join their full method lifetime
	// before closing the shared coordinator.
	entry.mu.Lock()
	directDrained := entry.directDrained
	entry.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-directDrained:
		return nil
	}
}

func (entry *runtimeResource) waitRunning(ctx context.Context) error {
	entry.mu.Lock()
	drained := entry.runningDrained
	entry.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-drained:
		return nil
	}
}

func (entry *runtimeResource) stopWorkersWhenDrained() {
	entry.mu.Lock()
	drained := entry.prepareDrained
	entry.mu.Unlock()
	go func() {
		<-drained
		entry.workStopOnce.Do(func() { close(entry.workStop) })
	}()
}
