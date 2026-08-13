package fairqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type schedulerTestTokens struct {
	mu     sync.Mutex
	values []string
	err    error
	calls  int
}

func (s *schedulerTestTokens) Next() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	if len(s.values) == 0 {
		return "", errors.New("scheduler test token source exhausted")
	}
	value := s.values[0]
	s.values = s.values[1:]
	return value, nil
}

type schedulerTestAdmission struct {
	mu      sync.Mutex
	permits []*schedulerTestPermit
	calls   int
}

func (a *schedulerTestAdmission) tryReserve(string) (schedulerWorkerPermit, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if len(a.permits) == 0 {
		return nil, false
	}
	permit := a.permits[0]
	a.permits = a.permits[1:]
	return permit, true
}

type schedulerTestPermit struct {
	mu sync.Mutex

	fence      ResourceFence
	generation uint64
	startErr   error
	started    []workerEnvelope
	aborts     int
	reports    []error
	onStart    func(workerEnvelope)
	onReport   func(error)
}

func (p *schedulerTestPermit) gateSnapshot() (ResourceFence, uint64) {
	return p.fence, p.generation
}

func (p *schedulerTestPermit) start(envelope workerEnvelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.startErr != nil {
		return p.startErr
	}
	p.started = append(p.started, envelope)
	if p.onStart != nil {
		p.onStart(envelope)
	}
	return nil
}

func (p *schedulerTestPermit) abort() {
	p.mu.Lock()
	p.aborts++
	p.mu.Unlock()
}

func (p *schedulerTestPermit) reportCoordinationFailure(err error) {
	p.mu.Lock()
	p.reports = append(p.reports, err)
	onReport := p.onReport
	p.mu.Unlock()
	if onReport != nil {
		onReport(err)
	}
}

type schedulerTestCoordinator struct {
	Coordinator
	nextFn         func(context.Context, string, ResourceFence, ProcessingTurnToken, time.Duration) (ProcessingTurn, bool, error)
	rotateFn       func(context.Context, string, ResourceFence, ProcessingTurnToken, uint64, bool) error
	acquireFn      func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error)
	releaseFn      func(context.Context, string, ResourceFence, string, string) error
	ensureActiveFn func(context.Context, string, ResourceFence, string) error
}

func (c schedulerTestCoordinator) NextTurn(ctx context.Context, resource string, fence ResourceFence, token ProcessingTurnToken, ttl time.Duration) (ProcessingTurn, bool, error) {
	return c.nextFn(ctx, resource, fence, token, ttl)
}

func (c schedulerTestCoordinator) RotateOrDeactivate(ctx context.Context, resource string, fence ResourceFence, token ProcessingTurnToken, generation uint64, hasReady bool) error {
	return c.rotateFn(ctx, resource, fence, token, generation, hasReady)
}

func (c schedulerTestCoordinator) AcquireProvisional(ctx context.Context, resource string, fence ResourceFence, tenant, attemptID string, limits CapacityLimits, ttl time.Duration) (ReservationDecision, error) {
	return c.acquireFn(ctx, resource, fence, tenant, attemptID, limits, ttl)
}

func (c schedulerTestCoordinator) Release(ctx context.Context, resource string, fence ResourceFence, tenant, token string) error {
	return c.releaseFn(ctx, resource, fence, tenant, token)
}

func (c schedulerTestCoordinator) EnsureActive(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	return c.ensureActiveFn(ctx, resource, fence, tenant)
}

type schedulerTestRabbit struct {
	RabbitClient
	getFn   func(context.Context, string, string) (Delivery, bool, error)
	depthFn func(context.Context, string, string) (int64, error)
}

func (r schedulerTestRabbit) GetOne(ctx context.Context, resource, tenant string) (Delivery, bool, error) {
	return r.getFn(ctx, resource, tenant)
}

func (r schedulerTestRabbit) ReadyDepth(ctx context.Context, resource, tenant string) (int64, error) {
	return r.depthFn(ctx, resource, tenant)
}

type schedulerTestDelivery struct {
	request PrepareRequest

	mu       sync.Mutex
	acks     int
	nacks    int
	requeues []bool
	nackFn   func(context.Context, bool) error
	ackFn    func(context.Context) error
}

func (d *schedulerTestDelivery) Request() PrepareRequest { return clonePrepareRequest(d.request) }

func (d *schedulerTestDelivery) Ack(ctx context.Context) error {
	d.mu.Lock()
	d.acks++
	d.mu.Unlock()
	if d.ackFn != nil {
		return d.ackFn(ctx)
	}
	return nil
}

func (d *schedulerTestDelivery) Nack(ctx context.Context, requeue bool) error {
	d.mu.Lock()
	d.nacks++
	d.requeues = append(d.requeues, requeue)
	d.mu.Unlock()
	if d.nackFn != nil {
		return d.nackFn(ctx, requeue)
	}
	return nil
}

type schedulerCallLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *schedulerCallLog) add(call string) {
	l.mu.Lock()
	l.calls = append(l.calls, call)
	l.mu.Unlock()
}

func (l *schedulerCallLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.calls...)
}

func schedulerTestConfig() ResourceConfig {
	return ResourceConfig{
		Key: "rag.index", ValidateTaskID: ValidateRAGIndexTaskID,
		LocalWorkers: 2, GlobalConcurrency: 4,
		PerUserBaseConcurrency: 2, PerUserBurstConcurrency: 4, BorrowEnabled: true,
		ReconcileInterval: time.Second, ExpiredRunningSweepInterval: time.Second,
		ReconcilePageSize: 10, ReservationTTL: time.Second, ReservationHeartbeat: 100 * time.Millisecond,
		PrepareTimeout: 100 * time.Millisecond, ProvisionalTTL: 200 * time.Millisecond,
		ProcessingTurnTTL: 200 * time.Millisecond, RecoveryDrainTimeout: 2 * time.Second,
		DispatchInterval: 100 * time.Millisecond, PublishAttemptTimeout: 200 * time.Millisecond,
	}
}

func schedulerTestFence() ResourceFence {
	return ResourceFence{Epoch: strings.Repeat("a", 32), WriterFingerprint: strings.Repeat("b", 64)}
}

func schedulerTestToken(index int) string { return fmt.Sprintf("%032x", index+1) }

func schedulerTestRequest(t *testing.T, tenant, taskID string, generation uint64) PrepareRequest {
	t.Helper()
	message := Message{
		Version: MessageVersion1, Resource: "rag.index", TenantID: tenant,
		TaskType: "rag.index", TaskID: taskID,
		DispatchToken: DispatchToken{Resource: "rag.index", TaskID: taskID, Generation: generation},
	}
	body := message
	header := message.DispatchToken
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := TenantHash(message.Resource, tenant)
	if err != nil {
		t.Fatal(err)
	}
	version := int32(MessageVersion1)
	resource := message.Resource
	headerTaskID := taskID
	headerGeneration := int64(generation)
	request := PrepareRequest{
		Message: &message, BodyCandidate: &body, HeaderToken: &header,
		HeaderFacts: StableHeaderFacts{
			ProtocolVersion: &version, Resource: &resource, TaskID: &headerTaskID,
			DispatchGeneration: &headerGeneration,
		},
		RegisteredResource: message.Resource, QueueTenantHash: hash,
		PublishAttemptID: schedulerTestToken(int(generation) + 100), RawBody: raw,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("test request Validate() error = %v", err)
	}
	return request
}

func newSchedulerTestSubject(
	t *testing.T,
	admission schedulerWorkerAdmission,
	coordinator Coordinator,
	rabbit RabbitClient,
	tokens runtimeTokenSource,
	options SchedulerOptions,
) *Scheduler {
	t.Helper()
	scheduler, err := newSchedulerWithAdmission(admission, schedulerTestConfig(), rabbit, coordinator, tokens, options)
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}

func TestSchedulerHealthRecordsOnlySuccessfulOpenGateIterations(t *testing.T) {
	admission := &schedulerTestAdmission{}
	scheduler := newSchedulerTestSubject(t, admission, schedulerTestCoordinator{}, schedulerTestRabbit{},
		&schedulerTestTokens{}, SchedulerOptions{IdleInterval: time.Millisecond})
	now := time.Date(2026, 8, 3, 3, 0, 0, 0, time.UTC)
	health := newResourceHealth(func() time.Time { return now })
	scheduler.health = health

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()

	deadline := time.Now().Add(time.Second)
	for {
		admission.mu.Lock()
		calls := admission.calls
		admission.mu.Unlock()
		if calls >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not complete closed-gate idle iterations")
		}
		time.Sleep(time.Millisecond)
	}
	if got := health.snapshot().Loops.Scheduler.LastSuccessAt; got != nil {
		t.Fatalf("closed-gate scheduler invented success at %v", *got)
	}

	health.markGateOpen()
	deadline = time.Now().Add(time.Second)
	for health.snapshot().Loops.Scheduler.LastSuccessAt == nil {
		if time.Now().After(deadline) {
			t.Fatal("open-gate successful scheduler iteration was not recorded")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSchedulerRoundRobinAndStrictTurnOrder(t *testing.T) {
	tenants := []string{"tenant-a", "tenant-b", "tenant-c", "tenant-a", "tenant-b"}
	log := &schedulerCallLog{}
	fence := schedulerTestFence()
	permits := make([]*schedulerTestPermit, len(tenants))
	tokenValues := make([]string, 0, len(tenants)*2)
	for index := range tenants {
		permits[index] = &schedulerTestPermit{fence: fence, generation: 7}
		tokenValues = append(tokenValues, schedulerTestToken(index*2), schedulerTestToken(index*2+1))
	}
	admission := &schedulerTestAdmission{permits: permits}
	turnIndex := 0
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			log.add("next:" + tenants[turnIndex])
			turn := ProcessingTurn{Token: token, TenantID: tenants[turnIndex], ObservedActivationGeneration: uint64(turnIndex + 1)}
			turnIndex++
			return turn, true, nil
		},
		acquireFn: func(_ context.Context, _ string, _ ResourceFence, tenant, _ string, _ CapacityLimits, _ time.Duration) (ReservationDecision, error) {
			log.add("acquire:" + tenant)
			return ReservationRegular, nil
		},
		rotateFn: func(_ context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, _ uint64, hasReady bool) error {
			if !hasReady {
				t.Fatal("successful round-robin turn was not rotated")
			}
			log.add("rotate")
			return nil
		},
		releaseFn:      func(context.Context, string, ResourceFence, string, string) error { return nil },
		ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
	}
	deliveryIndex := 0
	rabbit := schedulerTestRabbit{
		getFn: func(_ context.Context, _ string, tenant string) (Delivery, bool, error) {
			log.add("get:" + tenant)
			deliveryIndex++
			return &schedulerTestDelivery{request: schedulerTestRequest(t, tenant, fmt.Sprint(deliveryIndex), uint64(deliveryIndex))}, true, nil
		},
		depthFn: func(_ context.Context, _ string, tenant string) (int64, error) {
			log.add("depth:" + tenant)
			return 1, nil
		},
	}
	scheduler := newSchedulerTestSubject(t, admission, coordinator, rabbit, &schedulerTestTokens{values: tokenValues}, SchedulerOptions{})
	for range tenants {
		step, err := scheduler.runOne(context.Background())
		if err != nil || step != schedulerStepProgress {
			t.Fatalf("runOne() = (%v,%v), want progress", step, err)
		}
	}

	started := make([]string, 0, len(tenants))
	for _, permit := range permits {
		if len(permit.started) != 1 || permit.aborts != 0 {
			t.Fatalf("permit started=%d aborts=%d", len(permit.started), permit.aborts)
		}
		started = append(started, permit.started[0].tenant)
	}
	if !reflect.DeepEqual(started, tenants) {
		t.Fatalf("started tenants = %v, want %v", started, tenants)
	}
	if got := log.snapshot(); len(got) != len(tenants)*5 {
		t.Fatalf("call count = %d, want %d: %v", len(got), len(tenants)*5, got)
	}
}

func TestSchedulerReservationDenyRotatesWithoutRabbit(t *testing.T) {
	permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 3}
	admission := &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}
	getCalls := 0
	rotateReady := false
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 9}, true, nil
		},
		acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
			return ReservationDeniedGlobalFull, nil
		},
		rotateFn: func(_ context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, generation uint64, hasReady bool) error {
			rotateReady = hasReady
			if generation != 9 {
				t.Fatalf("generation = %d, want 9", generation)
			}
			return nil
		},
		releaseFn: func(context.Context, string, ResourceFence, string, string) error {
			t.Fatal("denied reservation was released despite never being acquired")
			return nil
		},
		ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
	}
	rabbit := schedulerTestRabbit{
		getFn: func(context.Context, string, string) (Delivery, bool, error) { getCalls++; return nil, false, nil },
		depthFn: func(context.Context, string, string) (int64, error) {
			t.Fatal("depth called after deny")
			return 0, nil
		},
	}
	scheduler := newSchedulerTestSubject(t, admission, coordinator, rabbit, &schedulerTestTokens{values: []string{schedulerTestToken(1), schedulerTestToken(2)}}, SchedulerOptions{})
	step, err := scheduler.runOne(context.Background())
	if err != nil || step != schedulerStepBackoff {
		t.Fatalf("runOne() = (%v,%v), want backoff", step, err)
	}
	if getCalls != 0 || !rotateReady || permit.aborts != 1 || len(permit.started) != 0 {
		t.Fatalf("get=%d rotateReady=%v aborts=%d starts=%d", getCalls, rotateReady, permit.aborts, len(permit.started))
	}
}

func TestSchedulerEmptyDeliveryUsesDepthThenRotateAndRelease(t *testing.T) {
	for _, test := range []struct {
		name         string
		depth        int64
		depthErr     error
		wantHasReady bool
		wantStep     schedulerStep
		wantError    bool
	}{
		{name: "empty", depth: 0, wantHasReady: false, wantStep: schedulerStepIdle},
		{name: "concurrent publish", depth: 1, wantHasReady: true, wantStep: schedulerStepIdle},
		{name: "unknown depth", depthErr: errors.New("depth unavailable"), wantHasReady: true, wantStep: schedulerStepBackoff, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 1}
			calls := &schedulerCallLog{}
			coordinator := schedulerTestCoordinator{
				nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
					return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 4}, true, nil
				},
				acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
					return ReservationRegular, nil
				},
				rotateFn: func(_ context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, generation uint64, hasReady bool) error {
					calls.add("rotate")
					if generation != 4 || hasReady != test.wantHasReady {
						t.Fatalf("rotate generation/ready = %d/%v", generation, hasReady)
					}
					return nil
				},
				releaseFn: func(_ context.Context, _ string, _ ResourceFence, tenant, token string) error {
					calls.add("release")
					if tenant != "tenant-a" || token != schedulerTestToken(2) {
						t.Fatalf("release identity = %q/%q", tenant, token)
					}
					return nil
				},
				ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
			}
			rabbit := schedulerTestRabbit{
				getFn: func(context.Context, string, string) (Delivery, bool, error) {
					calls.add("get")
					return nil, false, nil
				},
				depthFn: func(context.Context, string, string) (int64, error) {
					calls.add("depth")
					return test.depth, test.depthErr
				},
			}
			scheduler := newSchedulerTestSubject(t, &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}, coordinator, rabbit, &schedulerTestTokens{values: []string{schedulerTestToken(1), schedulerTestToken(2)}}, SchedulerOptions{})
			step, err := scheduler.runOne(context.Background())
			if step != test.wantStep || (err != nil) != test.wantError {
				t.Fatalf("runOne() = (%v,%v)", step, err)
			}
			if got, want := calls.snapshot(), []string{"get", "release", "depth", "rotate"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("calls = %v, want %v", got, want)
			}
			if permit.aborts != 1 || len(permit.started) != 0 {
				t.Fatalf("aborts=%d starts=%d", permit.aborts, len(permit.started))
			}
		})
	}
}

func TestSchedulerGetErrorFreshRotatesAndReleases(t *testing.T) {
	getErr := errors.New("rabbit unavailable")
	permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 5}
	rotated := false
	released := false
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			return ProcessingTurn{Token: token, TenantID: "selected-tenant", ObservedActivationGeneration: 3}, true, nil
		},
		acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
			return ReservationRegular, nil
		},
		rotateFn: func(ctx context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, _ uint64, hasReady bool) error {
			if ctx.Err() != nil || !hasReady {
				t.Fatalf("rotate ctx/hasReady = %v/%v", ctx.Err(), hasReady)
			}
			rotated = true
			return nil
		},
		releaseFn: func(ctx context.Context, _ string, _ ResourceFence, tenant, token string) error {
			if ctx.Err() != nil || tenant != "selected-tenant" || token != schedulerTestToken(4) {
				t.Fatalf("release ctx/identity = %v %q/%q", ctx.Err(), tenant, token)
			}
			released = true
			return nil
		},
		ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
	}
	rabbit := schedulerTestRabbit{
		getFn: func(context.Context, string, string) (Delivery, bool, error) { return nil, false, getErr },
		depthFn: func(context.Context, string, string) (int64, error) {
			t.Fatal("depth called after get error")
			return 0, nil
		},
	}
	scheduler := newSchedulerTestSubject(t, &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}, coordinator, rabbit, &schedulerTestTokens{values: []string{schedulerTestToken(3), schedulerTestToken(4)}}, SchedulerOptions{})
	step, err := scheduler.runOne(context.Background())
	if step != schedulerStepBackoff || !errors.Is(err, getErr) || !rotated || !released || permit.aborts != 1 {
		t.Fatalf("step=%v err=%v rotated=%v released=%v aborts=%d", step, err, rotated, released, permit.aborts)
	}
}

func TestSchedulerDeliverySurvivesDepthAndRotateFailures(t *testing.T) {
	for _, test := range []struct {
		name       string
		depthErr   error
		rotateErr  error
		wantReady  bool
		wantReport bool
	}{
		{name: "depth unknown", depthErr: errors.New("depth unavailable"), wantReady: true},
		{name: "unsupported depth", depthErr: ErrUnsupportedTopology, wantReady: true, wantReport: true},
		{name: "rotate fenced", rotateErr: ErrResourceNotReady, wantReady: false, wantReport: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 11}
			delivery := &schedulerTestDelivery{request: schedulerTestRequest(t, "selected-tenant", "42", 1)}
			coordinator := schedulerTestCoordinator{
				nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
					return ProcessingTurn{Token: token, TenantID: "selected-tenant", ObservedActivationGeneration: 8}, true, nil
				},
				acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
					return ReservationRegular, nil
				},
				rotateFn: func(_ context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, _ uint64, hasReady bool) error {
					if hasReady != test.wantReady {
						t.Fatalf("hasReady = %v, want %v", hasReady, test.wantReady)
					}
					return test.rotateErr
				},
				releaseFn:      func(context.Context, string, ResourceFence, string, string) error { return nil },
				ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
			}
			rabbit := schedulerTestRabbit{
				getFn:   func(context.Context, string, string) (Delivery, bool, error) { return delivery, true, nil },
				depthFn: func(context.Context, string, string) (int64, error) { return 0, test.depthErr },
			}
			scheduler := newSchedulerTestSubject(t, &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}, coordinator, rabbit, &schedulerTestTokens{values: []string{schedulerTestToken(5), schedulerTestToken(6)}}, SchedulerOptions{})
			step, err := scheduler.runOne(context.Background())
			if step != schedulerStepBackoff || err == nil {
				t.Fatalf("runOne() = (%v,%v), want handed-off backoff error", step, err)
			}
			if len(permit.started) != 1 || permit.aborts != 0 {
				t.Fatalf("starts=%d aborts=%d", len(permit.started), permit.aborts)
			}
			if permit.started[0].tenant != "selected-tenant" || permit.started[0].delivery != delivery {
				t.Fatal("handoff did not preserve trusted selected tenant and delivery")
			}
			if got := len(permit.reports); (got > 0) != test.wantReport {
				t.Fatalf("coordination reports = %d, wantReport=%v", got, test.wantReport)
			}
		})
	}
}

func TestSchedulerUnsupportedGetWithDeliveryReportsAndSettles(t *testing.T) {
	permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 21}
	delivery := &schedulerTestDelivery{request: schedulerTestRequest(t, "tenant-a", "15", 1)}
	calls := &schedulerCallLog{}
	delivery.nackFn = func(_ context.Context, requeue bool) error {
		if !requeue {
			t.Fatal("unsupported Get delivery was not requeued")
		}
		calls.add("nack")
		return nil
	}
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 12}, true, nil
		},
		acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
			return ReservationRegular, nil
		},
		ensureActiveFn: func(context.Context, string, ResourceFence, string) error {
			calls.add("activate")
			return nil
		},
		releaseFn: func(context.Context, string, ResourceFence, string, string) error {
			calls.add("release")
			return nil
		},
		rotateFn: func(_ context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, _ uint64, hasReady bool) error {
			if !hasReady {
				t.Fatal("unsupported Get delivery deactivated its tenant")
			}
			calls.add("rotate")
			return nil
		},
	}
	rabbit := schedulerTestRabbit{
		getFn: func(context.Context, string, string) (Delivery, bool, error) {
			return delivery, true, ErrUnsupportedTopology
		},
		depthFn: func(context.Context, string, string) (int64, error) {
			t.Fatal("ReadyDepth called after Get returned an error")
			return 0, nil
		},
	}
	scheduler := newSchedulerTestSubject(
		t,
		&schedulerTestAdmission{permits: []*schedulerTestPermit{permit}},
		coordinator,
		rabbit,
		&schedulerTestTokens{values: []string{schedulerTestToken(20), schedulerTestToken(21)}},
		SchedulerOptions{},
	)
	step, err := scheduler.runOne(context.Background())
	if step != schedulerStepBackoff || !errors.Is(err, ErrUnsupportedTopology) {
		t.Fatalf("runOne() = (%v,%v), want unsupported backoff", step, err)
	}
	if len(permit.reports) == 0 || !errors.Is(permit.reports[0], ErrUnsupportedTopology) {
		t.Fatalf("coordination reports = %v, want unsupported topology", permit.reports)
	}
	if got, want := calls.snapshot(), []string{"nack", "activate", "release", "rotate"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settlement calls = %v, want %v", got, want)
	}
	if delivery.nacks != 1 || len(permit.started) != 0 || permit.aborts != 1 {
		t.Fatalf("nacks=%d starts=%d aborts=%d", delivery.nacks, len(permit.started), permit.aborts)
	}
}

func TestSchedulerRunTreatsUnsupportedTopologyAsResourceLocal(t *testing.T) {
	reported := make(chan error, 1)
	permit := &schedulerTestPermit{
		fence: schedulerTestFence(), generation: 22,
		onReport: func(err error) {
			select {
			case reported <- err:
			default:
			}
		},
	}
	admission := &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 2}, true, nil
		},
		acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
			return ReservationRegular, nil
		},
		rotateFn:       func(context.Context, string, ResourceFence, ProcessingTurnToken, uint64, bool) error { return nil },
		releaseFn:      func(context.Context, string, ResourceFence, string, string) error { return nil },
		ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
	}
	rabbit := schedulerTestRabbit{
		getFn: func(context.Context, string, string) (Delivery, bool, error) {
			return nil, false, ErrUnsupportedTopology
		},
		depthFn: func(context.Context, string, string) (int64, error) {
			t.Fatal("ReadyDepth called after unsupported Get")
			return 0, nil
		},
	}
	scheduler := newSchedulerTestSubject(
		t, admission, coordinator, rabbit,
		&schedulerTestTokens{values: []string{schedulerTestToken(22), schedulerTestToken(23)}},
		SchedulerOptions{
			IdleInterval: time.Millisecond, BackoffInitial: time.Millisecond,
			BackoffMax: 2 * time.Millisecond, CleanupTimeout: 20 * time.Millisecond,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	select {
	case err := <-reported:
		if !errors.Is(err, ErrUnsupportedTopology) {
			t.Fatalf("reported error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("unsupported topology was not reported through the permit")
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned resource-local unsupported topology as terminal: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	admission.mu.Lock()
	admissionCalls := admission.calls
	admission.mu.Unlock()
	if admissionCalls < 2 {
		t.Fatalf("admission calls = %d, scheduler did not remain alive and idle after gate closure", admissionCalls)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after cancellation")
	}
}

func TestSchedulerAbsolutePrepareDeadlineIsNotRefreshedAtHandoff(t *testing.T) {
	config := schedulerTestConfig()
	config.PrepareTimeout = 80 * time.Millisecond
	config.ProvisionalTTL = 200 * time.Millisecond
	permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 4}
	var acquireDeadline, getDeadline, depthDeadline, rotateDeadline time.Time
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 1}, true, nil
		},
		acquireFn: func(ctx context.Context, _ string, _ ResourceFence, _ string, _ string, _ CapacityLimits, _ time.Duration) (ReservationDecision, error) {
			acquireDeadline, _ = ctx.Deadline()
			time.Sleep(15 * time.Millisecond)
			return ReservationRegular, nil
		},
		rotateFn: func(ctx context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, _ uint64, _ bool) error {
			rotateDeadline, _ = ctx.Deadline()
			return nil
		},
		releaseFn:      func(context.Context, string, ResourceFence, string, string) error { return nil },
		ensureActiveFn: func(context.Context, string, ResourceFence, string) error { return nil },
	}
	delivery := &schedulerTestDelivery{request: schedulerTestRequest(t, "tenant-a", "7", 1)}
	rabbit := schedulerTestRabbit{
		getFn: func(ctx context.Context, _, _ string) (Delivery, bool, error) {
			getDeadline, _ = ctx.Deadline()
			return delivery, true, nil
		},
		depthFn: func(ctx context.Context, _, _ string) (int64, error) {
			depthDeadline, _ = ctx.Deadline()
			return 0, nil
		},
	}
	scheduler, err := newSchedulerWithAdmission(
		&schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}, config, rabbit, coordinator,
		&schedulerTestTokens{values: []string{schedulerTestToken(7), schedulerTestToken(8)}}, SchedulerOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if step, err := scheduler.runOne(context.Background()); err != nil || step != schedulerStepProgress {
		t.Fatalf("runOne() = (%v,%v)", step, err)
	}
	if len(permit.started) != 1 {
		t.Fatalf("starts = %d", len(permit.started))
	}
	envelope := permit.started[0]
	defer envelope.cancelPrepare()
	contextDeadline, ok := envelope.prepareCtx.Deadline()
	if !ok {
		t.Fatal("worker prepare context has no deadline")
	}
	for name, deadline := range map[string]time.Time{
		"acquire": acquireDeadline, "get": getDeadline, "depth": depthDeadline,
		"rotate": rotateDeadline, "envelope": envelope.prepareDeadline, "context": contextDeadline,
	} {
		if !deadline.Equal(acquireDeadline) {
			t.Fatalf("%s deadline = %v, want %v", name, deadline, acquireDeadline)
		}
	}
	if remaining := time.Until(envelope.prepareDeadline); remaining >= config.PrepareTimeout-5*time.Millisecond {
		t.Fatalf("handoff refreshed prepare timeout: remaining=%s timeout=%s", remaining, config.PrepareTimeout)
	}
}

func TestSchedulerNeverDropsInconsistentNonNilDelivery(t *testing.T) {
	for _, test := range []struct {
		name   string
		got    bool
		getErr error
	}{
		{name: "error with delivery", got: true, getErr: errors.New("ambiguous get")},
		{name: "unreported delivery", got: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 13}
			delivery := &schedulerTestDelivery{request: schedulerTestRequest(t, "tenant-a", "12", 1)}
			calls := &schedulerCallLog{}
			delivery.nackFn = func(_ context.Context, requeue bool) error {
				if !requeue {
					t.Fatal("unexpected delivery was not requeued")
				}
				calls.add("nack")
				return nil
			}
			coordinator := schedulerTestCoordinator{
				nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
					return ProcessingTurn{Token: token, TenantID: "tenant-a", ObservedActivationGeneration: 6}, true, nil
				},
				acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
					return ReservationRegular, nil
				},
				ensureActiveFn: func(context.Context, string, ResourceFence, string) error { calls.add("activate"); return nil },
				releaseFn:      func(context.Context, string, ResourceFence, string, string) error { calls.add("release"); return nil },
				rotateFn: func(_ context.Context, _ string, _ ResourceFence, _ ProcessingTurnToken, _ uint64, hasReady bool) error {
					if !hasReady {
						t.Fatal("unexpected delivery turn was deactivated")
					}
					calls.add("rotate")
					return nil
				},
			}
			rabbit := schedulerTestRabbit{
				getFn: func(context.Context, string, string) (Delivery, bool, error) { return delivery, test.got, test.getErr },
				depthFn: func(context.Context, string, string) (int64, error) {
					t.Fatal("depth called for inconsistent get")
					return 0, nil
				},
			}
			scheduler := newSchedulerTestSubject(t, &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}, coordinator, rabbit, &schedulerTestTokens{values: []string{schedulerTestToken(12), schedulerTestToken(13)}}, SchedulerOptions{})
			step, err := scheduler.runOne(context.Background())
			if step != schedulerStepBackoff || err == nil {
				t.Fatalf("runOne() = (%v,%v)", step, err)
			}
			if got, want := calls.snapshot(), []string{"nack", "activate", "release", "rotate"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("cleanup calls = %v, want %v", got, want)
			}
			if delivery.nacks != 1 || len(permit.started) != 0 || permit.aborts != 1 {
				t.Fatalf("nacks=%d starts=%d aborts=%d", delivery.nacks, len(permit.started), permit.aborts)
			}
		})
	}
}

func TestSchedulerClosedOrFullAdmissionHasNoDependencySideEffects(t *testing.T) {
	admission := &schedulerTestAdmission{}
	tokens := &schedulerTestTokens{values: []string{schedulerTestToken(1)}}
	coordinator := schedulerTestCoordinator{
		nextFn: func(context.Context, string, ResourceFence, ProcessingTurnToken, time.Duration) (ProcessingTurn, bool, error) {
			t.Fatal("NextTurn called without a worker permit")
			return ProcessingTurn{}, false, nil
		},
	}
	rabbit := schedulerTestRabbit{
		getFn: func(context.Context, string, string) (Delivery, bool, error) {
			t.Fatal("GetOne called without a worker permit")
			return nil, false, nil
		},
		depthFn: func(context.Context, string, string) (int64, error) {
			t.Fatal("ReadyDepth called without a worker permit")
			return 0, nil
		},
	}
	scheduler := newSchedulerTestSubject(t, admission, coordinator, rabbit, tokens, SchedulerOptions{})
	step, err := scheduler.runOne(context.Background())
	if err != nil || step != schedulerStepIdle || tokens.calls != 0 || admission.calls != 1 {
		t.Fatalf("runOne=(%v,%v) tokenCalls=%d admissionCalls=%d", step, err, tokens.calls, admission.calls)
	}
}

func TestSchedulerFailedHandoffNacksActivatesThenReleasesWithFreshContexts(t *testing.T) {
	startErr := errors.New("runtime stopping")
	permit := &schedulerTestPermit{fence: schedulerTestFence(), generation: 2, startErr: startErr}
	delivery := &schedulerTestDelivery{request: schedulerTestRequest(t, "poison-body-tenant", "9", 1)}
	callLog := &schedulerCallLog{}
	delivery.nackFn = func(ctx context.Context, requeue bool) error {
		callLog.add("nack")
		if !requeue || ctx.Err() != nil {
			t.Fatalf("Nack requeue/context = %v/%v", requeue, ctx.Err())
		}
		return nil
	}
	coordinator := schedulerTestCoordinator{
		nextFn: func(_ context.Context, _ string, _ ResourceFence, token ProcessingTurnToken, _ time.Duration) (ProcessingTurn, bool, error) {
			return ProcessingTurn{Token: token, TenantID: "selected-tenant", ObservedActivationGeneration: 1}, true, nil
		},
		acquireFn: func(context.Context, string, ResourceFence, string, string, CapacityLimits, time.Duration) (ReservationDecision, error) {
			return ReservationRegular, nil
		},
		rotateFn: func(context.Context, string, ResourceFence, ProcessingTurnToken, uint64, bool) error { return nil },
		ensureActiveFn: func(ctx context.Context, _ string, _ ResourceFence, tenant string) error {
			callLog.add("activate")
			if ctx.Err() != nil || tenant != "selected-tenant" {
				t.Fatalf("EnsureActive context/tenant = %v/%q", ctx.Err(), tenant)
			}
			return nil
		},
		releaseFn: func(ctx context.Context, _ string, _ ResourceFence, tenant, token string) error {
			callLog.add("release")
			if ctx.Err() != nil || tenant != "selected-tenant" || token != schedulerTestToken(10) {
				t.Fatalf("Release context/identity = %v %q/%q", ctx.Err(), tenant, token)
			}
			return nil
		},
	}
	rabbit := schedulerTestRabbit{
		getFn:   func(context.Context, string, string) (Delivery, bool, error) { return delivery, true, nil },
		depthFn: func(context.Context, string, string) (int64, error) { return 0, nil },
	}
	scheduler := newSchedulerTestSubject(t, &schedulerTestAdmission{permits: []*schedulerTestPermit{permit}}, coordinator, rabbit, &schedulerTestTokens{values: []string{schedulerTestToken(9), schedulerTestToken(10)}}, SchedulerOptions{})
	step, err := scheduler.runOne(context.Background())
	if step != schedulerStepBackoff || !errors.Is(err, startErr) {
		t.Fatalf("runOne() = (%v,%v)", step, err)
	}
	if got, want := callLog.snapshot(), []string{"nack", "activate", "release"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup calls = %v, want %v", got, want)
	}
	if permit.aborts != 1 {
		t.Fatalf("permit aborts = %d, want 1", permit.aborts)
	}
}

func TestSchedulerRunBackoffHelpersAndCancellation(t *testing.T) {
	if got := []time.Duration{
		nextSchedulerBackoff(time.Millisecond, 4*time.Millisecond),
		nextSchedulerBackoff(2*time.Millisecond, 4*time.Millisecond),
		nextSchedulerBackoff(4*time.Millisecond, 4*time.Millisecond),
	}; !reflect.DeepEqual(got, []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}) {
		t.Fatalf("backoff sequence = %v", got)
	}

	scheduler := newSchedulerTestSubject(
		t, &schedulerTestAdmission{}, schedulerTestCoordinator{}, schedulerTestRabbit{},
		&schedulerTestTokens{values: []string{schedulerTestToken(1)}},
		SchedulerOptions{IdleInterval: time.Hour, BackoffInitial: time.Hour, BackoffMax: time.Hour, CleanupTimeout: 100 * time.Millisecond},
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestSchedulerCleanupBudgetFitsCompleteAttemptDrain(t *testing.T) {
	config := schedulerTestConfig()
	atLimit := (config.RecoveryDrainTimeout - config.PrepareTimeout) / 5
	if _, err := (SchedulerOptions{CleanupTimeout: atLimit}).withDefaults(config); err == nil {
		t.Fatal("cleanup budget reaching recovery drain limit was accepted")
	}
	if _, err := (SchedulerOptions{CleanupTimeout: atLimit - time.Nanosecond}).withDefaults(config); err != nil {
		t.Fatalf("cleanup budget strictly inside recovery drain was rejected: %v", err)
	}

	tight := config
	tight.RecoveryDrainTimeout = tight.PrepareTimeout + 2*time.Nanosecond
	if got := defaultSchedulerCleanupTimeout(tight); got != 0 {
		t.Fatalf("default cleanup for an unsafely tight drain = %v, want zero", got)
	}
}
