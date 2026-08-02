package fairqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type runtimeTestDispatchSource struct {
	candidate  *DispatchCandidate
	getErr     error
	markResult bool
	markErr    error
}

func (runtimeTestDispatchSource) ListDispatchCandidates(context.Context, string, int) ([]DispatchCandidate, string, error) {
	return nil, "", nil
}
func (s runtimeTestDispatchSource) GetDispatchableByID(context.Context, string) (DispatchCandidate, bool, error) {
	if s.getErr != nil {
		return DispatchCandidate{}, false, s.getErr
	}
	if s.candidate == nil {
		return DispatchCandidate{}, false, nil
	}
	return *s.candidate, true, nil
}
func (s runtimeTestDispatchSource) MarkDispatched(context.Context, DispatchCandidate) (bool, error) {
	return s.markResult, s.markErr
}

type runtimeTestFailingDispatchSource struct {
	fail   <-chan struct{}
	failed chan struct{}
	once   sync.Once
}

func (s *runtimeTestFailingDispatchSource) ListDispatchCandidates(context.Context, string, int) ([]DispatchCandidate, string, error) {
	select {
	case <-s.fail:
		s.once.Do(func() { close(s.failed) })
		return nil, "", ErrAuthoritativeWriterMismatch
	default:
		return nil, "", nil
	}
}
func (*runtimeTestFailingDispatchSource) GetDispatchableByID(context.Context, string) (DispatchCandidate, bool, error) {
	return DispatchCandidate{}, false, nil
}
func (*runtimeTestFailingDispatchSource) MarkDispatched(context.Context, DispatchCandidate) (bool, error) {
	return false, nil
}

type runtimeTestStaleGenerationSource struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	restarted    chan struct{}
	calls        atomic.Int32
}

func (s *runtimeTestStaleGenerationSource) ListDispatchCandidates(ctx context.Context, _ string, _ int) ([]DispatchCandidate, string, error) {
	switch s.calls.Add(1) {
	case 1:
		close(s.firstStarted)
		select {
		case <-s.releaseFirst:
			return nil, "", ErrAuthoritativeWriterMismatch
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	case 2:
		close(s.restarted)
	}
	return nil, "", nil
}

func (*runtimeTestStaleGenerationSource) GetDispatchableByID(context.Context, string) (DispatchCandidate, bool, error) {
	return DispatchCandidate{}, false, nil
}

func (*runtimeTestStaleGenerationSource) MarkDispatched(context.Context, DispatchCandidate) (bool, error) {
	return false, nil
}

type runtimeTestStaleDirectSource struct {
	firstStarted chan struct{}
	releaseFirst chan struct{}
	getCalls     atomic.Int32
}

func (*runtimeTestStaleDirectSource) ListDispatchCandidates(context.Context, string, int) ([]DispatchCandidate, string, error) {
	return nil, "", nil
}

func (s *runtimeTestStaleDirectSource) GetDispatchableByID(ctx context.Context, _ string) (DispatchCandidate, bool, error) {
	if s.getCalls.Add(1) == 1 {
		close(s.firstStarted)
		select {
		case <-s.releaseFirst:
			return DispatchCandidate{}, false, ErrAuthoritativeWriterMismatch
		case <-ctx.Done():
			return DispatchCandidate{}, false, ctx.Err()
		}
	}
	return DispatchCandidate{}, false, nil
}

func (*runtimeTestStaleDirectSource) MarkDispatched(context.Context, DispatchCandidate) (bool, error) {
	return false, nil
}

type runtimeTestRearmSource struct{}

func (runtimeTestRearmSource) RearmExpiredPage(context.Context, string, int) ([]DispatchCandidate, string, error) {
	return nil, "", nil
}

type runtimeTestJournal struct{ OperationJournal }

type runtimeTestDelivery struct {
	request     PrepareRequest
	ackErr      error
	nackErr     error
	events      *runtimeTestEvents
	ackStarted  chan struct{}
	ackRelease  <-chan struct{}
	ackFinished chan struct{}
	acked       atomic.Int32
	nacked      atomic.Int32
}

func (d *runtimeTestDelivery) Request() PrepareRequest { return clonePrepareRequest(d.request) }
func (d *runtimeTestDelivery) Ack(ctx context.Context) error {
	d.events.add("ack")
	d.acked.Add(1)
	if d.ackStarted != nil {
		close(d.ackStarted)
	}
	if d.ackFinished != nil {
		defer close(d.ackFinished)
	}
	if d.ackRelease != nil {
		select {
		case <-d.ackRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return d.ackErr
}
func (d *runtimeTestDelivery) Nack(_ context.Context, requeue bool) error {
	if !requeue {
		return errors.New("test delivery was nacked without requeue")
	}
	d.events.add("nack")
	d.nacked.Add(1)
	return d.nackErr
}

type runtimeTestEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *runtimeTestEvents) add(value string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.values = append(e.values, value)
	e.mu.Unlock()
}

func (e *runtimeTestEvents) snapshot() []string {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

type runtimeTestRabbit struct {
	RabbitClient
	delivery Delivery
	events   *runtimeTestEvents
	getOnce  atomic.Bool

	dlqErr       error
	dlqCalls     atomic.Int32
	publishCalls atomic.Int32
	getCalls     atomic.Int32
	closeCalls   atomic.Int32
}

type runtimeAutomaticRecoveryRabbit struct{ runtimeTestRabbit }

func (*runtimeAutomaticRecoveryRabbit) EnsureTenantTopology(context.Context, string, string) error {
	return nil
}

func (*runtimeAutomaticRecoveryRabbit) ReadyDepth(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (r *runtimeTestRabbit) PublishMandatoryConfirmed(context.Context, Message) (PublishReceipt, error) {
	r.publishCalls.Add(1)
	return PublishReceipt{AttemptID: "11111111111111111111111111111111"}, nil
}
func (r *runtimeTestRabbit) GetOne(context.Context, string, string) (Delivery, bool, error) {
	r.getCalls.Add(1)
	if !r.getOnce.CompareAndSwap(false, true) {
		return nil, false, nil
	}
	return r.delivery, r.delivery != nil, nil
}
func (*runtimeTestRabbit) ReadyDepth(context.Context, string, string) (int64, error) {
	return 0, nil
}
func (r *runtimeTestRabbit) PublishDeadLetterConfirmed(context.Context, DeadLetterRequest) (PublishReceipt, error) {
	r.events.add("dlq")
	r.dlqCalls.Add(1)
	return PublishReceipt{AttemptID: "22222222222222222222222222222222"}, r.dlqErr
}
func (r *runtimeTestRabbit) Close() error {
	r.closeCalls.Add(1)
	return nil
}

type runtimeTestCoordinator struct {
	Coordinator

	checkErr             error
	bindErr              error
	bindWaitForContext   bool
	bindStarted          chan struct{}
	activateStarted      chan struct{}
	activateRelease      <-chan struct{}
	events               *runtimeTestEvents
	releaseMu            sync.Mutex
	releaseTokens        []string
	bindObservedCanceled atomic.Bool

	checkCalls    atomic.Int32
	nextCalls     atomic.Int32
	acquireCalls  atomic.Int32
	bindCalls     atomic.Int32
	renewCalls    atomic.Int32
	releaseCalls  atomic.Int32
	ensureCalls   atomic.Int32
	activateCalls atomic.Int32
	closeCalls    atomic.Int32
}

type runtimeAutomaticRecoveryCoordinator struct {
	*recoveryTestCoordinator
	closeCalls    atomic.Int32
	ensureOnce    sync.Once
	ensureStarted chan struct{}
	ensureRelease <-chan struct{}
}

func (c *runtimeAutomaticRecoveryCoordinator) Close() error {
	c.closeCalls.Add(1)
	return nil
}

func (c *runtimeAutomaticRecoveryCoordinator) EnsureKnownTenant(
	ctx context.Context,
	resource string,
	fence ResourceFence,
	tenant string,
) error {
	if c.ensureStarted != nil {
		c.ensureOnce.Do(func() { close(c.ensureStarted) })
	}
	if c.ensureRelease != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ensureRelease:
		}
	}
	return c.recoveryTestCoordinator.EnsureKnownTenant(ctx, resource, fence, tenant)
}

func (c *runtimeTestCoordinator) CheckReadyFence(context.Context, string, ResourceFence) error {
	c.checkCalls.Add(1)
	return c.checkErr
}
func (c *runtimeTestCoordinator) NextTurn(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
	if c.nextCalls.Add(1) != 1 {
		return ProcessingTurn{}, false, nil
	}
	return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 1}, true, nil
}
func (*runtimeTestCoordinator) RotateOrDeactivate(context.Context, string, ResourceFence, ProcessingTurnToken, uint64, bool) error {
	return nil
}
func (c *runtimeTestCoordinator) AcquireProvisional(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
	c.acquireCalls.Add(1)
	return ReservationRegular, nil
}
func (c *runtimeTestCoordinator) BindReservation(ctx context.Context, _ string, _ ResourceFence, _, _, _ string, _ time.Duration) error {
	c.events.add("bind")
	c.bindCalls.Add(1)
	if c.bindStarted != nil {
		close(c.bindStarted)
	}
	if c.bindWaitForContext {
		<-ctx.Done()
		c.bindObservedCanceled.Store(true)
	}
	return c.bindErr
}
func (c *runtimeTestCoordinator) RenewStable(context.Context, string, ResourceFence, string, string, time.Duration) error {
	c.events.add("renew")
	c.renewCalls.Add(1)
	return nil
}
func (c *runtimeTestCoordinator) Release(_ context.Context, _ string, _ ResourceFence, _, token string) error {
	c.events.add("release")
	c.releaseCalls.Add(1)
	c.releaseMu.Lock()
	c.releaseTokens = append(c.releaseTokens, token)
	c.releaseMu.Unlock()
	return nil
}
func (c *runtimeTestCoordinator) EnsureActive(context.Context, string, ResourceFence, string) error {
	c.events.add("ensure-active")
	c.ensureCalls.Add(1)
	return nil
}
func (c *runtimeTestCoordinator) Activate(ctx context.Context, _ string, _ ResourceFence, _ string) error {
	c.ensureCalls.Add(1)
	c.activateCalls.Add(1)
	if c.activateStarted != nil {
		close(c.activateStarted)
	}
	if c.activateRelease != nil {
		select {
		case <-c.activateRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}
func (c *runtimeTestCoordinator) Close() error {
	c.closeCalls.Add(1)
	return nil
}

func (c *runtimeTestCoordinator) releasedTokens() []string {
	c.releaseMu.Lock()
	defer c.releaseMu.Unlock()
	return append([]string(nil), c.releaseTokens...)
}

type runtimeTestPreparer struct {
	prepared PreparedTask
	result   PrepareResult
	err      error
	panicV   any
	calls    atomic.Int32
}

func (p *runtimeTestPreparer) Prepare(context.Context, PrepareRequest) (PreparedTask, PrepareResult, error) {
	p.calls.Add(1)
	if p.panicV != nil {
		panic(p.panicV)
	}
	return p.prepared, p.result, p.err
}

type runtimeTestTask struct {
	err      error
	panicV   any
	events   *runtimeTestEvents
	started  chan struct{}
	release  chan struct{}
	runs     atomic.Int32
	canceled atomic.Bool
}

type runtimeTestIgnoringCancelTask struct {
	started chan struct{}
	finish  chan struct{}
}

func (t *runtimeTestIgnoringCancelTask) Run(context.Context) error {
	close(t.started)
	<-t.finish
	return nil
}

func (t *runtimeTestTask) Run(ctx context.Context) error {
	t.events.add("run")
	t.runs.Add(1)
	if t.started != nil {
		select {
		case <-t.started:
		default:
			close(t.started)
		}
	}
	if t.panicV != nil {
		panic(t.panicV)
	}
	if t.release != nil {
		select {
		case <-t.release:
		case <-ctx.Done():
			t.canceled.Store(true)
			return ctx.Err()
		}
	}
	return t.err
}

func runtimeTestConfig() ResourceConfig {
	return ResourceConfig{
		Key: "rag.index", ValidateTaskID: ValidateRAGIndexTaskID,
		LocalWorkers: 1, GlobalConcurrency: 1, PerUserBaseConcurrency: 1,
		PerUserBurstConcurrency: 1, BorrowEnabled: true,
		ReconcileInterval: 50 * time.Millisecond, ExpiredRunningSweepInterval: 50 * time.Millisecond,
		ReconcilePageSize: 10, ReservationTTL: 300 * time.Millisecond,
		ReservationHeartbeat: 20 * time.Millisecond, PrepareTimeout: 100 * time.Millisecond,
		ProvisionalTTL: 200 * time.Millisecond, ProcessingTurnTTL: 200 * time.Millisecond,
		RecoveryDrainTimeout: time.Second, DispatchInterval: 50 * time.Millisecond,
		PublishAttemptTimeout: 100 * time.Millisecond,
	}
}

func runtimeTestFence(seed byte) ResourceFence {
	return ResourceFence{Epoch: string(make([]byte, 0)) + repeatRuntimeHex(seed, 32), WriterFingerprint: repeatRuntimeHex(seed, 64)}
}

func runtimeTestRequestForResource(t *testing.T, resource, tenant, taskID string, generation uint64) PrepareRequest {
	t.Helper()
	message := Message{
		Version: MessageVersion1, Resource: resource, TenantID: tenant,
		TaskType: "rag.index", TaskID: taskID,
		DispatchToken: DispatchToken{Resource: resource, TaskID: taskID, Generation: generation},
	}
	body := message
	header := message.DispatchToken
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := TenantHash(resource, tenant)
	if err != nil {
		t.Fatal(err)
	}
	version := int32(MessageVersion1)
	headerResource := resource
	headerTaskID := taskID
	headerGeneration := int64(generation)
	request := PrepareRequest{
		Message: &message, BodyCandidate: &body, HeaderToken: &header,
		HeaderFacts: StableHeaderFacts{
			ProtocolVersion: &version, Resource: &headerResource, TaskID: &headerTaskID,
			DispatchGeneration: &headerGeneration,
		},
		RegisteredResource: resource, QueueTenantHash: hash,
		PublishAttemptID: schedulerTestToken(int(generation) + 200), RawBody: raw,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("runtime test request Validate() error = %v", err)
	}
	return request
}

func repeatRuntimeHex(seed byte, count int) string {
	digit := byte('a' + seed%6)
	value := make([]byte, count)
	for i := range value {
		value[i] = digit
	}
	return string(value)
}

func runtimeClaimedResult(request PrepareRequest) PrepareResult {
	message := request.Message
	return PrepareResult{
		Disposition: PrepareClaimed, DeliveryAction: DeliveryPromoteThenAckRun,
		CanonicalEffect: CanonicalClaimCommitted,
		Claim:           &ClaimRef{TenantID: message.TenantID, TaskID: message.TaskID, ClaimGeneration: message.DispatchToken.Generation},
	}
}

func runtimeTestOptions() RuntimeOptions {
	return RuntimeOptions{
		SchedulerIdleInterval: 5 * time.Millisecond,
		BackoffInitial:        5 * time.Millisecond,
		BackoffMax:            20 * time.Millisecond,
		CleanupTimeout:        50 * time.Millisecond,
		ShutdownGrace:         150 * time.Millisecond,
	}
}

func newRuntimeTestHarness(t *testing.T, preparer *runtimeTestPreparer, delivery *runtimeTestDelivery) (*Runtime, *runtimeTestRabbit, *runtimeTestCoordinator) {
	return newRuntimeTestHarnessWithSource(t, preparer, delivery, runtimeTestDispatchSource{})
}

func newRuntimeTestHarnessWithSource(t *testing.T, preparer *runtimeTestPreparer, delivery *runtimeTestDelivery, source DispatchSource) (*Runtime, *runtimeTestRabbit, *runtimeTestCoordinator) {
	t.Helper()
	rabbit := &runtimeTestRabbit{delivery: delivery}
	coordinator := &runtimeTestCoordinator{}
	runtime, err := NewRuntime(rabbit, coordinator, &runtimeTestJournal{}, runtimeTestOptions())
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	err = runtime.RegisterResource(ResourceRegistration{
		Config: runtimeTestConfig(), DispatchSource: source,
		ExpiredRearmSource: runtimeTestRearmSource{}, Preparer: preparer,
	})
	if err != nil {
		t.Fatalf("RegisterResource() error = %v", err)
	}
	return runtime, rabbit, coordinator
}

func startRuntimeTest(t *testing.T, runtime *Runtime) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	return cancel, done
}

func waitRuntimeTest(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}

func assertRuntimeTestReleasedBoundAndProvisional(t *testing.T, coordinator *runtimeTestCoordinator, stableToken string) {
	t.Helper()
	tokens := coordinator.releasedTokens()
	if len(tokens) != 2 {
		t.Fatalf("released tokens = %v, want one provisional and stable %q", tokens, stableToken)
	}
	stableCount := 0
	provisionalCount := 0
	for _, token := range tokens {
		switch token {
		case stableToken:
			stableCount++
		case "":
			t.Fatal("released an empty reservation token")
		default:
			provisionalCount++
		}
	}
	if stableCount != 1 || provisionalCount != 1 {
		t.Fatalf("released tokens = %v, want one provisional and stable %q", tokens, stableToken)
	}
}

func TestRuntimeRegistrationStartsFailClosedAndRejectsDuplicates(t *testing.T) {
	runtime, _, _ := newRuntimeTestHarness(t, &runtimeTestPreparer{}, nil)
	if _, err := runtime.TryDispatch(context.Background(), "rag.index", "42"); err == nil {
		t.Fatal("TryDispatch() before OpenResource error = nil")
	}
	registration := ResourceRegistration{
		Config: runtimeTestConfig(), DispatchSource: runtimeTestDispatchSource{},
		ExpiredRearmSource: runtimeTestRearmSource{}, Preparer: &runtimeTestPreparer{},
	}
	if err := runtime.RegisterResource(registration); err == nil {
		t.Fatal("duplicate RegisterResource() error = nil")
	}
}

func TestRuntimeRequiresAuthoritativeOperationJournal(t *testing.T) {
	_, err := NewRuntime(&runtimeTestRabbit{}, &runtimeTestCoordinator{}, nil, runtimeTestOptions())
	if err == nil {
		t.Fatal("NewRuntime() without OperationJournal error = nil")
	}
}

func TestRuntimeRegistrationRequiresCompleteRecoveryIdentity(t *testing.T) {
	runtime, _, _ := newRuntimeTestHarness(t, &runtimeTestPreparer{}, nil)
	registration := ResourceRegistration{
		Config: runtimeTestConfig(), DispatchSource: runtimeTestDispatchSource{},
		ExpiredRearmSource: runtimeTestRearmSource{}, Preparer: &runtimeTestPreparer{},
		WriterFingerprint: runtimeTestFence(0).WriterFingerprint,
	}
	registration.Config.Key = "rag.embed"
	if err := runtime.RegisterResource(registration); err == nil {
		t.Fatal("RegisterResource() with writer but no RecoverySource error = nil")
	}
}

func TestRuntimeAutomaticRecoveryOpensOnlyAfterCanonicalBarrier(t *testing.T) {
	config := recoveryTestConfig()
	config.RecoveryDrainTimeout = 500 * time.Millisecond
	fence := recoveryTestFence()
	ensureStarted := make(chan struct{})
	ensureRelease := make(chan struct{})
	coordinator := &runtimeAutomaticRecoveryCoordinator{
		recoveryTestCoordinator: newRecoveryTestCoordinator(fence),
		ensureStarted:           ensureStarted, ensureRelease: ensureRelease,
	}
	rabbit := &runtimeAutomaticRecoveryRabbit{}
	runtime, err := NewRuntime(rabbit, coordinator, fakeOperationJournal{}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	source := &recoveryTestSource{
		highWater: "1",
		known:     []TenantRef{{TenantID: "tenant-before-open"}},
	}
	if err := runtime.RegisterResource(ResourceRegistration{
		Config: config, Preparer: &runtimeTestPreparer{},
		DispatchSource: runtimeTestDispatchSource{}, ExpiredRearmSource: runtimeTestRearmSource{},
		RecoverySource: source, WriterFingerprint: fence.WriterFingerprint,
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := runtime.resource(config.Key)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, open := entry.readySnapshot(); open {
		t.Fatal("resource gate was open before the startup recovery barrier")
	}

	cancel, done := startRuntimeTest(t, runtime)
	released := false
	defer func() {
		if !released {
			close(ensureRelease)
		}
		cancel()
	}()
	select {
	case <-ensureStarted:
	case <-time.After(time.Second):
		t.Fatal("canonical reconciliation did not reach its blocked write")
	}
	if _, _, open := entry.readySnapshot(); open || entry.dispatcher.PublisherGateOpen() {
		t.Fatal("resource gate opened before canonical reconciliation completed")
	}
	if dispatched, dispatchErr := runtime.TryDispatch(context.Background(), config.Key, "42"); dispatched || !errors.Is(dispatchErr, ErrResourceNotReady) {
		t.Fatalf("TryDispatch() during startup barrier = %v, %v", dispatched, dispatchErr)
	}
	close(ensureRelease)
	released = true
	waitRuntimeTest(t, func() bool {
		_, _, open := entry.readySnapshot()
		coordinator.mu.Lock()
		known := append([]string(nil), coordinator.known...)
		coordinator.mu.Unlock()
		return open && len(known) != 0 && known[0] == "tenant-before-open"
	}, "automatic recovery canonical pass and gate open")
	cancel()
	select {
	case runErr := <-done:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Runtime.Run did not stop")
	}
}

func TestRuntimeClaimedBindsThenAcksThenRunsAndReleases(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	events := &runtimeTestEvents{}
	delivery := &runtimeTestDelivery{request: request, events: events}
	task := &runtimeTestTask{events: events}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	coordinator.events = events
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatalf("OpenResource() error = %v", err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool { return task.runs.Load() == 1 && coordinator.releaseCalls.Load() > 0 }, "claimed task run and release")
	if coordinator.bindCalls.Load() != 1 || delivery.acked.Load() != 1 || delivery.nacked.Load() != 0 {
		t.Fatalf("bind/ack/nack = %d/%d/%d", coordinator.bindCalls.Load(), delivery.acked.Load(), delivery.nacked.Load())
	}
	wantEvents := []string{"bind", "ack", "run", "release"}
	if got := events.snapshot(); !equalRuntimeEvents(got, wantEvents) {
		t.Fatalf("worker order = %v, want %v", got, wantEvents)
	}
	cancel()
	<-done
}

func equalRuntimeEvents(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func TestRuntimeBindFailureAcksWithoutRunning(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	task := &runtimeTestTask{}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	coordinator.bindErr = ErrFenceMismatch
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool { return delivery.acked.Load() == 1 && coordinator.releaseCalls.Load() > 0 }, "bind-failure settlement")
	if task.runs.Load() != 0 || delivery.nacked.Load() != 0 {
		t.Fatalf("runs/nacks = %d/%d, want 0/0", task.runs.Load(), delivery.nacked.Load())
	}
	cancel()
	<-done
}

func TestRuntimeAckFailureAfterBindStillRuns(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request, ackErr: ErrDependencyUnavailable}
	task := &runtimeTestTask{}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool { return task.runs.Load() == 1 && coordinator.releaseCalls.Load() > 0 }, "run after ACK failure")
	if coordinator.bindCalls.Load() != 1 || delivery.acked.Load() != 1 {
		t.Fatalf("bind/ack = %d/%d", coordinator.bindCalls.Load(), delivery.acked.Load())
	}
	cancel()
	<-done
}

func TestRuntimeTransientNacksReactivatesAndReleases(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	preparer := &runtimeTestPreparer{result: PrepareResult{
		Disposition: PrepareTransientInfrastructure, DeliveryAction: DeliveryNackRequeue,
		CanonicalEffect: CanonicalNone,
	}}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool {
		return delivery.nacked.Load() == 1 && coordinator.ensureCalls.Load() > 0 && coordinator.releaseCalls.Load() > 0
	}, "transient NACK, activation, and release")
	if delivery.acked.Load() != 0 || coordinator.bindCalls.Load() != 0 {
		t.Fatalf("ack/bind = %d/%d, want 0/0", delivery.acked.Load(), coordinator.bindCalls.Load())
	}
	cancel()
	<-done
}

func TestRuntimeDuplicateAcksAndReleasesWithoutRunning(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	preparer := &runtimeTestPreparer{result: PrepareResult{
		Disposition: PrepareDuplicateStaleTerminal, DeliveryAction: DeliveryAckRelease,
		CanonicalEffect: CanonicalNone,
	}}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool { return delivery.acked.Load() == 1 && coordinator.releaseCalls.Load() > 0 }, "duplicate ACK and release")
	if delivery.nacked.Load() != 0 || coordinator.bindCalls.Load() != 0 {
		t.Fatalf("nack/bind = %d/%d, want 0/0", delivery.nacked.Load(), coordinator.bindCalls.Load())
	}
	cancel()
	<-done
}

func TestRuntimePoisonConfirmsDLQBeforeAckAndRequeuesOnDLQFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		dlqErr   error
		wantAck  int32
		wantNack int32
	}{
		{name: "confirmed", wantAck: 1},
		{name: "unconfirmed", dlqErr: ErrPublishUnconfirmed, wantNack: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := schedulerTestRequest(t, "tenant-a", "42", 7)
			delivery := &runtimeTestDelivery{request: request}
			preparer := &runtimeTestPreparer{result: PrepareResult{
				Disposition: PreparePoisonPermanentInvalidMessage, DeliveryAction: DeliveryConfirmDLQThenAck,
				CanonicalEffect: CanonicalPoisonRepairSettled,
			}}
			runtime, rabbit, coordinator := newRuntimeTestHarness(t, preparer, delivery)
			rabbit.dlqErr = test.dlqErr
			if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
				t.Fatal(err)
			}
			cancel, done := startRuntimeTest(t, runtime)
			waitRuntimeTest(t, func() bool {
				return rabbit.dlqCalls.Load() == 1 && delivery.acked.Load()+delivery.nacked.Load() == 1 && coordinator.releaseCalls.Load() > 0
			}, "poison settlement")
			if delivery.acked.Load() != test.wantAck || delivery.nacked.Load() != test.wantNack {
				t.Fatalf("ack/nack = %d/%d, want %d/%d", delivery.acked.Load(), delivery.nacked.Load(), test.wantAck, test.wantNack)
			}
			cancel()
			<-done
		})
	}
}

func TestRuntimeRunExitAndPanicBothReleaseStableReservation(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		panicV any
	}{
		{name: "error", err: errors.New("run failed")},
		{name: "panic", panicV: "run panic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := schedulerTestRequest(t, "tenant-a", "42", 7)
			delivery := &runtimeTestDelivery{request: request}
			task := &runtimeTestTask{err: test.err, panicV: test.panicV}
			preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
			runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
			if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
				t.Fatal(err)
			}
			cancel, done := startRuntimeTest(t, runtime)
			waitRuntimeTest(t, func() bool { return task.runs.Load() == 1 && coordinator.releaseCalls.Load() > 0 }, "run exit release")
			cancel()
			<-done
		})
	}
}

func TestRuntimeRenewsStableReservationWhileTaskRuns(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	releaseRun := make(chan struct{})
	task := &runtimeTestTask{started: make(chan struct{}), release: releaseRun}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	waitRuntimeTest(t, func() bool { return coordinator.renewCalls.Load() > 0 }, "stable reservation heartbeat")
	close(releaseRun)
	waitRuntimeTest(t, func() bool { return coordinator.releaseCalls.Load() > 0 }, "stable reservation release")
	cancel()
	<-done
}

func TestRuntimeFullLocalSlotDoesNotTouchRedisOrRabbit(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	releaseRun := make(chan struct{})
	task := &runtimeTestTask{started: make(chan struct{}), release: releaseRun}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, rabbit, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancel, done := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	nextCalls := coordinator.nextCalls.Load()
	getCalls := rabbit.getCalls.Load()
	time.Sleep(40 * time.Millisecond)
	if coordinator.nextCalls.Load() != nextCalls || rabbit.getCalls.Load() != getCalls {
		t.Fatalf("full slot caused NextTurn/Get: before=%d/%d after=%d/%d", nextCalls, getCalls, coordinator.nextCalls.Load(), rabbit.getCalls.Load())
	}
	close(releaseRun)
	cancel()
	<-done
}

func TestRuntimeOldPermitFailureCannotCloseSameFenceAfterReopen(t *testing.T) {
	runtime, _, _ := newRuntimeTestHarness(t, &runtimeTestPreparer{}, nil)
	fence := runtimeTestFence(0)
	if err := runtime.OpenResource("rag.index", fence); err != nil {
		t.Fatal(err)
	}
	permit, ok := runtime.tryReserveWorker("rag.index")
	if !ok {
		t.Fatal("tryReserveWorker() = false")
	}
	runtime.CloseResource("rag.index")
	permit.abort()
	if err := runtime.WaitForAttemptDrain(context.Background(), "rag.index"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.OpenResource("rag.index", fence); err != nil {
		t.Fatal(err)
	}
	permit.reportCoordinationFailure(ErrFenceMismatch)
	current, ok := runtime.tryReserveWorker("rag.index")
	if !ok {
		t.Fatal("stale permit closed a same-fence reopened generation")
	}
	current.abort()
}

func TestRuntimeShutdownDeadlineCancelsRunningTaskAndReleases(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	task := &runtimeTestTask{started: make(chan struct{}), release: make(chan struct{})}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, rabbit, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = runtime.Shutdown(shutdownCtx)
	waitRuntimeTest(t, func() bool { return coordinator.releaseCalls.Load() > 0 }, "release after shutdown cancellation")
	waitRuntimeTest(t, task.canceled.Load, "PreparedTask.Run to observe shutdown cancellation")
	if rabbit.closeCalls.Load() != 1 || coordinator.closeCalls.Load() != 1 {
		t.Fatalf("close calls = %d/%d, want 1/1", rabbit.closeCalls.Load(), coordinator.closeCalls.Load())
	}
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after Shutdown")
	}
}

func TestRuntimeShutdownGraceAllowsRunningTaskToFinish(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	releaseRun := make(chan struct{})
	task := &runtimeTestTask{started: make(chan struct{}), release: releaseRun}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		shutdownDone <- runtime.Shutdown(ctx)
	}()
	time.Sleep(20 * time.Millisecond)
	close(releaseRun)
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after graceful Run completion")
	}
	if task.canceled.Load() {
		t.Fatal("graceful shutdown canceled a task that finished within grace")
	}
	if coordinator.releaseCalls.Load() == 0 {
		t.Fatal("graceful shutdown did not release stable reservation")
	}
	<-runDone
}

func TestRuntimeAuthoritativeWriterMismatchFailsRun(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	preparer := &runtimeTestPreparer{err: ErrAuthoritativeWriterMismatch}
	runtime, _, _ := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, done := startRuntimeTest(t, runtime)
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthoritativeWriterMismatch) {
			t.Fatalf("Run() error = %v, want ErrAuthoritativeWriterMismatch", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not fail on authoritative writer mismatch")
	}
}

func TestRuntimeAuthoritativeStateCorruptionFailsRun(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	preparer := &runtimeTestPreparer{err: ErrAuthoritativeStateCorrupt}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, done := startRuntimeTest(t, runtime)
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthoritativeStateCorrupt) {
			t.Fatalf("Run() error = %v, want ErrAuthoritativeStateCorrupt", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not fail on authoritative state corruption")
	}
	if delivery.nacked.Load() != 0 || coordinator.ensureCalls.Load() != 0 {
		t.Fatalf("authoritative corruption nack/ensure-active = %d/%d, want 0/0",
			delivery.nacked.Load(), coordinator.ensureCalls.Load())
	}
	if coordinator.releaseCalls.Load() == 0 {
		t.Fatal("authoritative corruption did not release its provisional reservation")
	}
}

func TestRuntimeMixedAuthoritativeCorruptionIsNotStalePublisherExempt(t *testing.T) {
	runtime, _, _ := newRuntimeTestHarness(t, &runtimeTestPreparer{}, nil)
	entry, err := runtime.resource("rag.index")
	if err != nil {
		t.Fatal(err)
	}
	secondErr := errors.New("component retried after mixed authoritative failure")
	var calls atomic.Int32
	results := make(chan error, 1)
	go runtime.runComponent(context.Background(), results, entry, func(context.Context) error {
		if calls.Add(1) == 1 {
			return stalePublisherSourceFailure{err: errors.Join(
				ErrAuthoritativeWriterMismatch,
				ErrAuthoritativeStateCorrupt,
			)}
		}
		return secondErr
	})
	select {
	case got := <-results:
		if !errors.Is(got, ErrAuthoritativeStateCorrupt) || errors.Is(got, secondErr) {
			t.Fatalf("component error = %v, want mixed authoritative corruption", got)
		}
	case <-time.After(time.Second):
		t.Fatal("component did not report mixed authoritative corruption")
	}
	if calls.Load() != 1 {
		t.Fatalf("component calls = %d, want 1", calls.Load())
	}
	runtime.mu.Lock()
	fatalErr := runtime.fatalErr
	runtime.mu.Unlock()
	if !errors.Is(fatalErr, ErrAuthoritativeStateCorrupt) {
		t.Fatalf("runtime fatal error = %v, want ErrAuthoritativeStateCorrupt", fatalErr)
	}
}

func TestRuntimePreparedTaskAuthoritativeStateCorruptionIsGlobalFatal(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	task := &runtimeTestTask{err: ErrAuthoritativeStateCorrupt}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, _ := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, done := startRuntimeTest(t, runtime)
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthoritativeStateCorrupt) {
			t.Fatalf("Run() error = %v, want ErrAuthoritativeStateCorrupt", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not fail after PreparedTask authoritative state corruption")
	}
}

func TestRuntimeShutdownClosesSharedClientsOnce(t *testing.T) {
	runtime, rabbit, coordinator := newRuntimeTestHarness(t, &runtimeTestPreparer{}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runtime.Shutdown(ctx)
		}()
	}
	wg.Wait()
	if rabbit.closeCalls.Load() != 1 || coordinator.closeCalls.Load() != 1 {
		t.Fatalf("Rabbit/Redis Close calls = %d/%d, want 1/1", rabbit.closeCalls.Load(), coordinator.closeCalls.Load())
	}
}

func TestRuntimeDispatcherWriterMismatchImmediatelyCancelsRunningAndClosesGates(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	task := &runtimeTestTask{started: make(chan struct{}), release: make(chan struct{})}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	rabbit := &runtimeTestRabbit{delivery: delivery}
	coordinator := &runtimeTestCoordinator{}
	runtime, err := NewRuntime(rabbit, coordinator, &runtimeTestJournal{}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	fail := make(chan struct{})
	failed := make(chan struct{})
	source := &runtimeTestFailingDispatchSource{fail: fail, failed: failed}
	if err := runtime.RegisterResource(ResourceRegistration{
		Config: runtimeTestConfig(), DispatchSource: source,
		ExpiredRearmSource: runtimeTestRearmSource{}, Preparer: preparer,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	close(fail)
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not observe injected writer mismatch")
	}
	cancelDeadline := time.Now().Add(50 * time.Millisecond)
	for !task.canceled.Load() && time.Now().Before(cancelDeadline) {
		time.Sleep(time.Millisecond)
	}
	if !task.canceled.Load() {
		t.Fatal("dispatcher writer mismatch did not immediately cancel running task")
	}
	select {
	case err := <-runDone:
		if !errors.Is(err, ErrAuthoritativeWriterMismatch) {
			t.Fatalf("Run() error = %v, want ErrAuthoritativeWriterMismatch", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after dispatcher writer mismatch")
	}
	if _, err := runtime.TryDispatch(context.Background(), "rag.index", "42"); err == nil {
		t.Fatal("TryDispatch succeeded after fatal dispatcher source mismatch")
	}
}

func TestRuntimeShutdownSynchronouslyStopsNewSchedulerAndPublisherAdmission(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	releaseRun := make(chan struct{})
	task := &runtimeTestTask{started: make(chan struct{}), release: releaseRun}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	configEntry, err := runtime.resource("rag.index")
	if err != nil {
		t.Fatal(err)
	}
	configEntry.config.LocalWorkers = 2
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown(context.Background()) }()
	waitRuntimeTest(t, func() bool {
		_, dispatchErr := runtime.TryDispatch(context.Background(), "rag.index", "42")
		return dispatchErr != nil
	}, "publisher gate closure during shutdown")
	nextCalls := coordinator.nextCalls.Load()
	time.Sleep(30 * time.Millisecond)
	if coordinator.nextCalls.Load() != nextCalls {
		t.Fatalf("NextTurn calls grew after Shutdown admission close: %d -> %d", nextCalls, coordinator.nextCalls.Load())
	}
	close(releaseRun)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not complete")
	}
	<-runDone
}

func TestRuntimeShutdownReleasesAndClosesWhenRunIgnoresCancellation(t *testing.T) {
	options := runtimeTestOptions()
	options.ShutdownGrace = 30 * time.Millisecond
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	finishRun := make(chan struct{})
	task := &runtimeTestIgnoringCancelTask{started: make(chan struct{}), finish: finishRun}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	rabbit := &runtimeTestRabbit{delivery: delivery}
	coordinator := &runtimeTestCoordinator{}
	runtime, err := NewRuntime(rabbit, coordinator, &runtimeTestJournal{}, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.RegisterResource(ResourceRegistration{
		Config: runtimeTestConfig(), DispatchSource: runtimeTestDispatchSource{},
		ExpiredRearmSource: runtimeTestRearmSource{}, Preparer: preparer,
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if coordinator.releaseCalls.Load() == 0 {
		t.Error("Shutdown did not best-effort Release stable token for a cancellation-ignoring Run")
	}
	if rabbit.closeCalls.Load() != 1 || coordinator.closeCalls.Load() != 1 {
		t.Errorf("close calls = %d/%d, want 1/1", rabbit.closeCalls.Load(), coordinator.closeCalls.Load())
	}
	close(finishRun)
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after ignored task was released")
	}
}

func TestRuntimeTryDispatchWriterMismatchIsGlobalFatalAndCancelsRunning(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	task := &runtimeTestTask{started: make(chan struct{}), release: make(chan struct{})}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	source := runtimeTestDispatchSource{getErr: ErrAuthoritativeWriterMismatch}
	runtime, _, _ := newRuntimeTestHarnessWithSource(t, preparer, delivery, source)
	otherConfig := runtimeTestConfig()
	otherConfig.Key = "rag.embed"
	if err := runtime.RegisterResource(ResourceRegistration{
		Config: otherConfig, DispatchSource: runtimeTestDispatchSource{},
		ExpiredRearmSource: runtimeTestRearmSource{}, Preparer: &runtimeTestPreparer{},
	}); err != nil {
		t.Fatal(err)
	}
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		t.Fatal("task did not start")
	}
	if err := runtime.OpenResource("rag.embed", runtimeTestFence(1)); err != nil {
		t.Fatal(err)
	}
	dispatched, err := runtime.TryDispatch(context.Background(), "rag.index", "42")
	if dispatched || !errors.Is(err, ErrAuthoritativeWriterMismatch) {
		t.Fatalf("TryDispatch() = %v, %v; want false, ErrAuthoritativeWriterMismatch", dispatched, err)
	}
	waitRuntimeTest(t, task.canceled.Load, "running task cancellation after TryDispatch writer mismatch")
	for _, resource := range []string{"rag.index", "rag.embed"} {
		entry, resourceErr := runtime.resource(resource)
		if resourceErr != nil {
			t.Fatal(resourceErr)
		}
		entry.mu.Lock()
		gateOpen := entry.gateOpen
		entry.mu.Unlock()
		if gateOpen || entry.dispatcher.PublisherGateOpen() {
			t.Errorf("resource %q retained an open gate after global fatal", resource)
		}
	}
	select {
	case runErr := <-runDone:
		if !errors.Is(runErr, ErrAuthoritativeWriterMismatch) {
			t.Fatalf("Run() error = %v, want ErrAuthoritativeWriterMismatch", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after TryDispatch writer mismatch")
	}
}

func TestRuntimePublisherFenceFailureStopsSchedulerBeforeNextTurn(t *testing.T) {
	candidate := dispatcherTestCandidate("42", "guard-42", 7)
	source := runtimeTestDispatchSource{candidate: &candidate}
	runtime, _, coordinator := newRuntimeTestHarnessWithSource(t, &runtimeTestPreparer{}, nil, source)
	coordinator.checkErr = ErrFenceMismatch
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	dispatched, err := runtime.TryDispatch(context.Background(), "rag.index", "42")
	if !dispatched || !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("TryDispatch() = %v, %v; want true, ErrFenceMismatch", dispatched, err)
	}
	if coordinator.checkCalls.Load() != 1 {
		t.Fatalf("CheckReadyFence calls = %d, want 1", coordinator.checkCalls.Load())
	}
	entry, err := runtime.resource("rag.index")
	if err != nil {
		t.Fatal(err)
	}
	if entry.dispatcher.PublisherGateOpen() {
		t.Fatal("publisher gate remained open after CheckReadyFence failure")
	}
	cancelRun, runDone := startRuntimeTest(t, runtime)
	time.Sleep(40 * time.Millisecond)
	if coordinator.nextCalls.Load() != 0 {
		t.Fatalf("NextTurn calls after publisher gate close = %d, want 0", coordinator.nextCalls.Load())
	}
	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRuntimeRejectsDeliveryOutsideSelectedQueueContextBeforePrepare(t *testing.T) {
	tests := []struct {
		name    string
		request func(*testing.T) PrepareRequest
	}{
		{
			name: "registered resource",
			request: func(t *testing.T) PrepareRequest {
				return runtimeTestRequestForResource(t, "rag.other", "tenant-a", "42", 7)
			},
		},
		{
			name: "queue tenant hash",
			request: func(t *testing.T) PrepareRequest {
				return runtimeTestRequestForResource(t, "rag.index", "tenant-b", "42", 7)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := test.request(t)
			delivery := &runtimeTestDelivery{request: request}
			preparer := &runtimeTestPreparer{}
			runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
			if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
				t.Fatal(err)
			}
			cancelRun, runDone := startRuntimeTest(t, runtime)
			waitRuntimeTest(t, func() bool {
				return delivery.nacked.Load() == 1 && coordinator.ensureCalls.Load() == 1 && coordinator.releaseCalls.Load() == 1
			}, "mismatched delivery cleanup")
			if preparer.calls.Load() != 0 {
				t.Fatalf("Prepare calls = %d, want 0", preparer.calls.Load())
			}
			if delivery.acked.Load() != 0 {
				t.Fatalf("Ack calls = %d, want 0", delivery.acked.Load())
			}
			entry, err := runtime.resource("rag.index")
			if err != nil {
				t.Fatal(err)
			}
			entry.mu.Lock()
			gateOpen := entry.gateOpen
			entry.mu.Unlock()
			if gateOpen || entry.dispatcher.PublisherGateOpen() {
				t.Fatal("queue-context mismatch did not close both resource gates")
			}
			cancelRun()
			select {
			case <-runDone:
			case <-time.After(time.Second):
				t.Fatal("Run did not stop after cancellation")
			}
		})
	}
}

func TestRuntimePreparerPanicCleansDeliveryReservationAndWorkerSlot(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	preparer := &runtimeTestPreparer{panicV: "prepare boom"}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancelRun, runDone := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool {
		return delivery.nacked.Load() == 1 && coordinator.ensureCalls.Load() == 1 && coordinator.releaseCalls.Load() == 1
	}, "Preparer panic cleanup")
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	defer cancelDrain()
	if err := runtime.WaitForAttemptDrain(drainCtx, "rag.index"); err != nil {
		t.Fatalf("WaitForAttemptDrain() error = %v", err)
	}
	if preparer.calls.Load() != 1 || delivery.acked.Load() != 0 {
		t.Fatalf("Prepare/Ack calls = %d/%d, want 1/0", preparer.calls.Load(), delivery.acked.Load())
	}
	entry, err := runtime.resource("rag.index")
	if err != nil {
		t.Fatal(err)
	}
	entry.mu.Lock()
	prepareCount, slotsInUse, gateOpen := entry.prepareCount, entry.slotsInUse, entry.gateOpen
	entry.mu.Unlock()
	if prepareCount != 0 || slotsInUse != 0 {
		t.Fatalf("prepare attempts/worker slots after panic = %d/%d, want 0/0", prepareCount, slotsInUse)
	}
	if gateOpen || entry.dispatcher.PublisherGateOpen() {
		t.Fatal("Preparer panic did not close both resource gates")
	}
	select {
	case runErr := <-runDone:
		t.Fatalf("Run exited on resource-local Preparer panic: %v", runErr)
	default:
	}
	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRuntimeStaleSourceMismatchRestartsComponentWithoutClosingReopenedGeneration(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	releaseTask := make(chan struct{})
	task := &runtimeTestTask{started: make(chan struct{}), release: releaseTask}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	source := &runtimeTestStaleGenerationSource{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
		restarted:    make(chan struct{}),
	}
	runtime, _, coordinator := newRuntimeTestHarnessWithSource(t, preparer, delivery, source)
	fence := runtimeTestFence(0)
	if err := runtime.OpenResource("rag.index", fence); err != nil {
		t.Fatal(err)
	}
	cancelRun, runDone := startRuntimeTest(t, runtime)
	select {
	case <-source.firstStarted:
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("old-generation ListDispatchCandidates did not start")
	}
	select {
	case <-task.started:
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("task did not start while old-generation source read was in flight")
	}
	if err := runtime.CloseResource("rag.index"); err != nil {
		cancelRun()
		t.Fatal(err)
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), time.Second)
	if err := runtime.WaitForAttemptDrain(drainCtx, "rag.index"); err != nil {
		cancelDrain()
		cancelRun()
		t.Fatalf("WaitForAttemptDrain() error = %v", err)
	}
	cancelDrain()
	if err := runtime.OpenResource("rag.index", fence); err != nil {
		cancelRun()
		t.Fatal(err)
	}
	close(source.releaseFirst)
	select {
	case <-source.restarted:
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("dispatcher component did not restart for the reopened generation")
	}
	time.Sleep(20 * time.Millisecond)
	entry, err := runtime.resource("rag.index")
	if err != nil {
		cancelRun()
		t.Fatal(err)
	}
	entry.mu.Lock()
	gateOpen := entry.gateOpen
	entry.mu.Unlock()
	if !gateOpen || !entry.dispatcher.PublisherGateOpen() {
		cancelRun()
		t.Fatal("stale source mismatch closed the reopened generation")
	}
	if task.canceled.Load() {
		cancelRun()
		t.Fatal("stale source mismatch canceled an existing running task")
	}
	select {
	case runErr := <-runDone:
		t.Fatalf("Run terminated on stale source mismatch: %v", runErr)
	default:
	}
	close(releaseTask)
	waitRuntimeTest(t, func() bool { return coordinator.releaseCalls.Load() > 0 }, "running task release")
	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRuntimeBindSuccessAfterPrepareDeadlineAcksReleasesBothTokensAndDoesNotRun(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	task := &runtimeTestTask{}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	runtime, _, coordinator := newRuntimeTestHarness(t, preparer, delivery)
	coordinator.bindWaitForContext = true
	coordinator.bindStarted = make(chan struct{})
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	cancelRun, runDone := startRuntimeTest(t, runtime)
	select {
	case <-coordinator.bindStarted:
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("BindReservation did not start")
	}
	waitRuntimeTest(t, func() bool {
		return delivery.acked.Load() == 1 && coordinator.releaseCalls.Load() == 2
	}, "post-deadline bind settlement")
	if !coordinator.bindObservedCanceled.Load() {
		t.Fatal("BindReservation did not return nil after observing prepare context expiry")
	}
	if task.runs.Load() != 0 || delivery.nacked.Load() != 0 {
		t.Fatalf("Run/Nack calls = %d/%d, want 0/0", task.runs.Load(), delivery.nacked.Load())
	}
	stableToken, err := StableReservationToken("rag.index", "42", 7)
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeTestReleasedBoundAndProvisional(t, coordinator, stableToken)
	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRuntimeWriterMismatchWhileAckBlockedCannotCrossFatalBeginRunBoundary(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	ackRelease := make(chan struct{})
	delivery := &runtimeTestDelivery{
		request: request, ackStarted: make(chan struct{}), ackRelease: ackRelease, ackFinished: make(chan struct{}),
	}
	task := &runtimeTestTask{}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	source := runtimeTestDispatchSource{getErr: ErrAuthoritativeWriterMismatch}
	runtime, _, coordinator := newRuntimeTestHarnessWithSource(t, preparer, delivery, source)
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	select {
	case <-delivery.ackStarted:
	case <-time.After(time.Second):
		t.Fatal("delivery ACK did not block after successful Bind")
	}
	dispatched, err := runtime.TryDispatch(context.Background(), "rag.index", "42")
	if dispatched || !errors.Is(err, ErrAuthoritativeWriterMismatch) {
		t.Fatalf("TryDispatch() = %v, %v; want false, ErrAuthoritativeWriterMismatch", dispatched, err)
	}
	close(ackRelease)
	select {
	case <-delivery.ackFinished:
	case <-time.After(time.Second):
		t.Fatal("delivery ACK did not finish")
	}
	waitRuntimeTest(t, func() bool { return coordinator.releaseCalls.Load() == 2 }, "fatal post-ACK token release")
	if task.runs.Load() != 0 {
		t.Fatalf("PreparedTask.Run calls = %d, want 0", task.runs.Load())
	}
	stableToken, err := StableReservationToken("rag.index", "42", 7)
	if err != nil {
		t.Fatal(err)
	}
	assertRuntimeTestReleasedBoundAndProvisional(t, coordinator, stableToken)
	select {
	case runErr := <-runDone:
		if !errors.Is(runErr, ErrAuthoritativeWriterMismatch) {
			t.Fatalf("Run() error = %v, want ErrAuthoritativeWriterMismatch", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not return after writer mismatch")
	}
}

func TestRuntimeShutdownWaitsForDirectDispatchPostMarkActivationBeforeCoordinatorClose(t *testing.T) {
	candidate := dispatcherTestCandidate("42", "guard-42", 7)
	activateRelease := make(chan struct{})
	source := runtimeTestDispatchSource{candidate: &candidate, markResult: true}
	runtime, rabbit, coordinator := newRuntimeTestHarnessWithSource(t, &runtimeTestPreparer{}, nil, source)
	coordinator.activateStarted = make(chan struct{})
	coordinator.activateRelease = activateRelease
	if err := runtime.OpenResource("rag.index", runtimeTestFence(0)); err != nil {
		t.Fatal(err)
	}
	type dispatchResult struct {
		dispatched bool
		err        error
	}
	dispatchDone := make(chan dispatchResult, 1)
	go func() {
		dispatched, err := runtime.TryDispatch(context.Background(), "rag.index", "42")
		dispatchDone <- dispatchResult{dispatched: dispatched, err: err}
	}()
	select {
	case <-coordinator.activateStarted:
	case <-time.After(time.Second):
		t.Fatal("post-Mark Activate did not start")
	}
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- runtime.Shutdown(context.Background()) }()
	waitRuntimeTest(t, func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.shuttingDown
	}, "Shutdown admission close")
	time.Sleep(20 * time.Millisecond)
	if coordinator.closeCalls.Load() != 0 {
		t.Fatal("coordinator closed while direct post-Mark Activate was still running")
	}
	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before post-Mark Activate completed: %v", err)
	default:
	}
	close(activateRelease)
	select {
	case result := <-dispatchDone:
		if !result.dispatched || result.err != nil {
			t.Fatalf("TryDispatch() = %v, %v; want true, nil", result.dispatched, result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("TryDispatch did not finish after Activate release")
	}
	select {
	case err := <-shutdownDone:
		if err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Shutdown did not finish after direct dispatch drained")
	}
	if coordinator.activateCalls.Load() != 1 || coordinator.closeCalls.Load() != 1 || rabbit.closeCalls.Load() != 1 {
		t.Fatalf("Activate/coordinator Close/Rabbit Close calls = %d/%d/%d, want 1/1/1",
			coordinator.activateCalls.Load(), coordinator.closeCalls.Load(), rabbit.closeCalls.Load())
	}
}

func TestRuntimeZeroResourceRunIsWokenByExternalShutdown(t *testing.T) {
	rabbit := &runtimeTestRabbit{}
	coordinator := &runtimeTestCoordinator{}
	runtime, err := NewRuntime(rabbit, coordinator, &runtimeTestJournal{}, runtimeTestOptions())
	if err != nil {
		t.Fatal(err)
	}
	_, runDone := startRuntimeTest(t, runtime)
	waitRuntimeTest(t, func() bool {
		runtime.mu.Lock()
		defer runtime.mu.Unlock()
		return runtime.running && runtime.runCancel != nil
	}, "zero-resource Runtime.Run startup")
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), time.Second)
	defer cancelShutdown()
	if err := runtime.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	select {
	case runErr := <-runDone:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("Run() error = %v, want context.Canceled", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("zero-resource Run was not woken by external Shutdown")
	}
	if rabbit.closeCalls.Load() != 1 || coordinator.closeCalls.Load() != 1 {
		t.Fatalf("Rabbit/coordinator Close calls = %d/%d, want 1/1", rabbit.closeCalls.Load(), coordinator.closeCalls.Load())
	}
}

func TestRuntimeStaleDirectSourceMismatchDoesNotFatalReopenedGeneration(t *testing.T) {
	request := schedulerTestRequest(t, "tenant-a", "42", 7)
	delivery := &runtimeTestDelivery{request: request}
	releaseTask := make(chan struct{})
	task := &runtimeTestTask{started: make(chan struct{}), release: releaseTask}
	preparer := &runtimeTestPreparer{prepared: task, result: runtimeClaimedResult(request)}
	source := &runtimeTestStaleDirectSource{
		firstStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}),
	}
	runtime, _, coordinator := newRuntimeTestHarnessWithSource(t, preparer, delivery, source)
	fence := runtimeTestFence(0)
	if err := runtime.OpenResource("rag.index", fence); err != nil {
		t.Fatal(err)
	}
	cancelRun, runDone := startRuntimeTest(t, runtime)
	select {
	case <-task.started:
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("task did not start")
	}
	type dispatchResult struct {
		dispatched bool
		err        error
	}
	directCtx, cancelDirect := context.WithCancel(context.Background())
	defer cancelDirect()
	firstDone := make(chan dispatchResult, 1)
	go func() {
		dispatched, err := runtime.TryDispatch(directCtx, "rag.index", "42")
		firstDone <- dispatchResult{dispatched: dispatched, err: err}
	}()
	select {
	case <-source.firstStarted:
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("old-generation GetDispatchableByID did not start")
	}
	if err := runtime.CloseResource("rag.index"); err != nil {
		cancelRun()
		t.Fatal(err)
	}
	if err := runtime.OpenResource("rag.index", fence); err != nil {
		cancelRun()
		t.Fatal(err)
	}
	close(source.releaseFirst)
	select {
	case result := <-firstDone:
		if result.dispatched || !errors.Is(result.err, ErrAuthoritativeWriterMismatch) {
			t.Fatalf("stale TryDispatch() = %v, %v; want false, ErrAuthoritativeWriterMismatch", result.dispatched, result.err)
		}
	case <-time.After(time.Second):
		cancelRun()
		t.Fatal("stale TryDispatch did not return")
	}
	entry, err := runtime.resource("rag.index")
	if err != nil {
		cancelRun()
		t.Fatal(err)
	}
	entry.mu.Lock()
	gateOpen := entry.gateOpen
	entry.mu.Unlock()
	runtime.mu.Lock()
	fatalErr := runtime.fatalErr
	runtime.mu.Unlock()
	if fatalErr != nil {
		cancelRun()
		t.Fatalf("stale direct source mismatch became Runtime fatal: %v", fatalErr)
	}
	if !gateOpen || !entry.dispatcher.PublisherGateOpen() {
		cancelRun()
		t.Fatal("stale direct source mismatch closed the reopened generation")
	}
	if task.canceled.Load() {
		cancelRun()
		t.Fatal("stale direct source mismatch canceled an existing running task")
	}
	dispatched, err := runtime.TryDispatch(context.Background(), "rag.index", "42")
	if dispatched || err != nil {
		cancelRun()
		t.Fatalf("TryDispatch() on reopened generation = %v, %v; want false, nil", dispatched, err)
	}
	if source.getCalls.Load() != 2 {
		cancelRun()
		t.Fatalf("GetDispatchableByID calls = %d, want 2", source.getCalls.Load())
	}
	select {
	case runErr := <-runDone:
		t.Fatalf("Run terminated on stale direct source mismatch: %v", runErr)
	default:
	}
	close(releaseTask)
	waitRuntimeTest(t, func() bool { return coordinator.releaseCalls.Load() > 0 }, "running task release")
	cancelRun()
	select {
	case <-runDone:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}
