package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/store"
)

type gatewayFairQueueTestRuntime struct {
	mu            sync.Mutex
	events        *[]string
	run           chan struct{}
	release       chan struct{}
	runErr        error
	shutdownErr   error
	dispatch      bool
	runCount      int
	shutdownCount int
	failCount     int
}

func (r *gatewayFairQueueTestRuntime) RegisterResource(fairqueue.ResourceRegistration) error {
	if r.events != nil {
		*r.events = append(*r.events, "register")
	}
	return nil
}

func (r *gatewayFairQueueTestRuntime) Run(ctx context.Context) error {
	r.mu.Lock()
	r.runCount++
	r.mu.Unlock()
	if r.events != nil {
		*r.events = append(*r.events, "run")
	}
	if r.run != nil {
		close(r.run)
	}
	if r.release != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.release:
		}
	}
	return r.runErr
}

func (r *gatewayFairQueueTestRuntime) TryDispatch(context.Context, string, string) (bool, error) {
	return r.dispatch, nil
}

func (r *gatewayFairQueueTestRuntime) Shutdown(context.Context) error {
	r.mu.Lock()
	r.shutdownCount++
	r.mu.Unlock()
	if r.events != nil {
		*r.events = append(*r.events, "shutdown")
	}
	return r.shutdownErr
}

func (r *gatewayFairQueueTestRuntime) FailAuthoritative(error) error {
	r.mu.Lock()
	r.failCount++
	r.mu.Unlock()
	return nil
}

func (r *gatewayFairQueueTestRuntime) counts() (run, shutdown, fail int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runCount, r.shutdownCount, r.failCount
}

type gatewayFairQueueTestRabbit struct {
	events *[]string
	probe  fairqueue.RabbitResourceProbe
	err    error
	health fairqueue.RabbitHealthSnapshot
}

func (r *gatewayFairQueueTestRabbit) ProbeResourceTopology(context.Context, string) (fairqueue.RabbitResourceProbe, error) {
	*r.events = append(*r.events, "rabbit-probe")
	return r.probe, r.err
}
func (r *gatewayFairQueueTestRabbit) EnsureTenantTopology(context.Context, string, string) error {
	return nil
}
func (r *gatewayFairQueueTestRabbit) PublishMandatoryConfirmed(context.Context, fairqueue.Message) (fairqueue.PublishReceipt, error) {
	return fairqueue.PublishReceipt{}, nil
}
func (r *gatewayFairQueueTestRabbit) GetOne(context.Context, string, string) (fairqueue.Delivery, bool, error) {
	return nil, false, nil
}
func (r *gatewayFairQueueTestRabbit) ReadyDepth(context.Context, string, string) (int64, error) {
	return 0, nil
}
func (r *gatewayFairQueueTestRabbit) PublishDeadLetterConfirmed(context.Context, fairqueue.DeadLetterRequest) (fairqueue.PublishReceipt, error) {
	return fairqueue.PublishReceipt{}, nil
}
func (r *gatewayFairQueueTestRabbit) Close() error {
	*r.events = append(*r.events, "rabbit-close")
	return nil
}
func (r *gatewayFairQueueTestRabbit) HealthSnapshot() fairqueue.RabbitHealthSnapshot {
	return r.health
}

type gatewayFairQueueTestCoordinator struct {
	fairqueue.RuntimeCoordinator
	events *[]string
}

func (c *gatewayFairQueueTestCoordinator) Close() error {
	*c.events = append(*c.events, "redis-close")
	return nil
}

func TestBuildRAGFairQueueRuntimeProbesBeforeRegisterAndRun(t *testing.T) {
	t.Parallel()

	events := make([]string, 0, 8)
	runtime := &gatewayFairQueueTestRuntime{events: &events}
	rabbit := &gatewayFairQueueTestRabbit{
		events: &events,
		probe:  fairqueue.RabbitResourceProbe{Resource: "rag.index"},
	}
	coordinator := &gatewayFairQueueTestCoordinator{events: &events}
	journal := &gatewayFairQueueTestJournal{}
	builder := ragFairQueueRuntimeBuilder{
		newCoordinator: func(context.Context) (fairqueue.RuntimeCoordinator, error) {
			events = append(events, "redis-probe")
			return coordinator, nil
		},
		newRabbit: func() (ragFairQueueRabbit, error) {
			events = append(events, "rabbit-new")
			return rabbit, nil
		},
		newRuntime: func(gotRabbit fairqueue.RabbitClient, gotCoordinator fairqueue.RuntimeCoordinator, gotJournal fairqueue.OperationJournal) (ragFairQueueRuntime, error) {
			events = append(events, "runtime-new")
			if gotRabbit != rabbit || gotCoordinator != coordinator || gotJournal != journal {
				t.Fatal("runtime dependencies were not injected losslessly")
			}
			return runtime, nil
		},
		journal:      journal,
		registration: fairqueue.ResourceRegistration{Config: fairqueue.ResourceConfig{Key: "rag.index"}},
	}

	got, err := builder.Build(context.Background())
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if got != runtime {
		t.Fatal("Build() returned a different runtime")
	}
	want := []string{"redis-probe", "rabbit-new", "rabbit-probe", "runtime-new", "register"}
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("events = %v, want %v", events, want)
		}
	}
}

func TestBuildRAGFairQueueRuntimePublishesFailedRabbitProbeHealth(t *testing.T) {
	t.Parallel()

	events := []string{}
	probeErr := errors.Join(fairqueue.ErrDependencyUnavailable, errors.New("rabbit down"))
	rabbit := &gatewayFairQueueTestRabbit{
		events: &events, err: probeErr,
		health: fairqueue.RabbitHealthSnapshot{Status: fairqueue.DependencyStatusUnavailable},
	}
	var observed fairqueue.RabbitHealthSnapshot
	builder := ragFairQueueRuntimeBuilder{
		newCoordinator: func(context.Context) (fairqueue.RuntimeCoordinator, error) {
			return &gatewayFairQueueTestCoordinator{events: &events}, nil
		},
		newRabbit: func() (ragFairQueueRabbit, error) { return rabbit, nil },
		newRuntime: func(fairqueue.RabbitClient, fairqueue.RuntimeCoordinator, fairqueue.OperationJournal) (ragFairQueueRuntime, error) {
			t.Fatal("failed Rabbit probe reached runtime construction")
			return nil, nil
		},
		onRabbitHealth: func(snapshot fairqueue.RabbitHealthSnapshot) { observed = snapshot },
		journal:        &gatewayFairQueueTestJournal{},
		registration:   fairqueue.ResourceRegistration{Config: fairqueue.ResourceConfig{Key: rag.RAGFairQueueResource}},
	}
	if _, err := builder.Build(context.Background()); !errors.Is(err, probeErr) {
		t.Fatalf("Build() error = %v, want Rabbit probe failure", err)
	}
	if observed.Status != fairqueue.DependencyStatusUnavailable {
		t.Fatalf("observed Rabbit status = %q, want unavailable", observed.Status)
	}
}

func TestRAGFairQueueSupervisorRetriesDependencyWithoutFallback(t *testing.T) {
	t.Parallel()

	runtime := &gatewayFairQueueTestRuntime{
		run: make(chan struct{}), release: make(chan struct{}), dispatch: true,
	}
	var attempts int
	var waits []time.Duration
	supervisor := newRAGFairQueueSupervisor(func(context.Context) (ragFairQueueRuntime, error) {
		attempts++
		if attempts < 3 {
			return nil, fairqueue.ErrDependencyUnavailable
		}
		return runtime, nil
	}, ragFairQueueSupervisorOptions{
		BackoffInitial: time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		Wait: func(_ context.Context, delay time.Duration) error {
			waits = append(waits, delay)
			return nil
		},
	})

	if _, err := supervisor.TryDispatch(context.Background(), "rag.index", "1"); !errors.Is(err, fairqueue.ErrResourceNotReady) {
		t.Fatalf("TryDispatch before runtime error = %v, want resource not ready", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	select {
	case <-runtime.run:
	case <-time.After(time.Second):
		t.Fatal("runtime did not start after dependency recovery")
	}
	if attempts != 3 || len(waits) != 2 || waits[0] != time.Millisecond || waits[1] != 2*time.Millisecond {
		t.Fatalf("attempts/waits = %d/%v", attempts, waits)
	}
	if dispatched, err := supervisor.TryDispatch(context.Background(), "rag.index", "1"); err != nil || !dispatched {
		t.Fatalf("TryDispatch after recovery = %v, %v", dispatched, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() after cancel error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
}

func TestRAGFairQueueSupervisorDoesNotRetrySafetyFailure(t *testing.T) {
	t.Parallel()

	attempts := 0
	supervisor := newRAGFairQueueSupervisor(func(context.Context) (ragFairQueueRuntime, error) {
		attempts++
		return nil, fairqueue.ErrAuthoritativeWriterMismatch
	}, ragFairQueueSupervisorOptions{})
	err := supervisor.Run(context.Background())
	if !errors.Is(err, fairqueue.ErrAuthoritativeWriterMismatch) || attempts != 1 {
		t.Fatalf("Run() = %v, attempts=%d; want terminal writer mismatch once", err, attempts)
	}
	if supervisor.Status() != fairQueueStatusFailed {
		t.Fatalf("status = %q, want failed", supervisor.Status())
	}
}

func TestRAGFairQueueSupervisorRejectsRuntimeBuiltAfterTerminalFailure(t *testing.T) {
	t.Parallel()

	buildStarted := make(chan struct{})
	releaseBuild := make(chan struct{})
	runtime := &gatewayFairQueueTestRuntime{}
	supervisor := newRAGFairQueueSupervisor(func(context.Context) (ragFairQueueRuntime, error) {
		close(buildStarted)
		<-releaseBuild
		return runtime, nil
	}, ragFairQueueSupervisorOptions{})
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(context.Background()) }()
	select {
	case <-buildStarted:
	case <-time.After(time.Second):
		t.Fatal("runtime build did not start")
	}
	terminal := errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, errors.New("writer changed"))
	if err := supervisor.FailAuthoritative(terminal); err != nil {
		t.Fatalf("FailAuthoritative() error = %v", err)
	}
	close(releaseBuild)
	select {
	case err := <-done:
		if !errors.Is(err, fairqueue.ErrAuthoritativeWriterMismatch) {
			t.Fatalf("Run() error = %v, want terminal writer mismatch", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not reject the late runtime")
	}
	runs, shutdowns, failures := runtime.counts()
	if runs != 0 || shutdowns != 1 || failures != 1 {
		t.Fatalf("late runtime calls run/shutdown/fail = %d/%d/%d, want 0/1/1", runs, shutdowns, failures)
	}
}

func TestRAGFairQueueSupervisorAlwaysShutsDownBuiltRuntime(t *testing.T) {
	t.Parallel()

	runtime := &gatewayFairQueueTestRuntime{run: make(chan struct{}), release: make(chan struct{})}
	supervisor := newRAGFairQueueSupervisor(func(context.Context) (ragFairQueueRuntime, error) {
		return runtime, nil
	}, ragFairQueueSupervisorOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	select {
	case <-runtime.run:
	case <-time.After(time.Second):
		t.Fatal("runtime did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop")
	}
	_, shutdowns, _ := runtime.counts()
	if shutdowns != 1 {
		t.Fatalf("Shutdown() calls = %d, want 1", shutdowns)
	}
}

func TestRAGFairQueueSupervisorDoesNotReplaceRuntimeAfterShutdownFailure(t *testing.T) {
	t.Parallel()

	closeErr := errors.New("dependency generation close failed")
	runtime := &gatewayFairQueueTestRuntime{
		runErr: fairqueue.ErrDependencyUnavailable, shutdownErr: closeErr,
	}
	attempts := 0
	waits := 0
	supervisor := newRAGFairQueueSupervisor(func(context.Context) (ragFairQueueRuntime, error) {
		attempts++
		return runtime, nil
	}, ragFairQueueSupervisorOptions{
		Wait: func(context.Context, time.Duration) error {
			waits++
			return errors.New("unexpected retry wait")
		},
	})
	err := supervisor.Run(context.Background())
	if !errors.Is(err, closeErr) || attempts != 1 || waits != 0 ||
		supervisor.Status() != fairQueueStatusFailed {
		t.Fatalf("Run()=%v attempts=%d waits=%d status=%s; want terminal close failure",
			err, attempts, waits, supervisor.Status())
	}
}

// The concrete adapter supplies the real journal. This minimal implementation
// lets the builder test assert identity rather than weakening the constructor.
type gatewayFairQueueTestJournal struct{ fairqueue.OperationJournal }

type gatewayFairQueueProbeJournal struct {
	fairqueue.OperationJournal
	record  fairqueue.RecoveryOperationRecord
	present bool
	err     error
}

func (j gatewayFairQueueProbeJournal) Read(context.Context, string, string) (fairqueue.RecoveryOperationRecord, bool, error) {
	return j.record, j.present, j.err
}

func TestInitializeRAGFairQueueJournalHealthKeepsTransientFailureDegraded(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair,
	})
	state.markMySQL(fairqueue.MySQLStatusOK, true)
	err := initializeRAGFairQueueJournalHealth(context.Background(), gatewayFairQueueProbeJournal{
		err: errors.New("transient admin journal read failure"),
	}, strings.Repeat("a", 64), state)
	if err != nil {
		t.Fatalf("initializeRAGFairQueueJournalHealth() error = %v, want degraded startup", err)
	}
	state.mu.RLock()
	journalKnown := state.journalKnown
	state.mu.RUnlock()
	if journalKnown {
		t.Fatal("transient startup journal failure retained known state")
	}
	if snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded); snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusOK ||
		snapshot.FairQueue.Status != fairqueue.HealthStatusDegraded {
		t.Fatalf("transient startup journal health = %+v", snapshot.FairQueue)
	}
}

func TestInitializeRAGFairQueueJournalHealthRejectsSafetyFailure(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair,
	})
	err := initializeRAGFairQueueJournalHealth(context.Background(), gatewayFairQueueProbeJournal{
		err: errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, fairqueue.ErrInvalidOperationRecord),
	}, strings.Repeat("a", 64), state)
	if !errors.Is(err, fairqueue.ErrAuthoritativeStateCorrupt) {
		t.Fatalf("initializeRAGFairQueueJournalHealth() error = %v, want authoritative corruption", err)
	}
	if snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded); snapshot.FairQueue.Status != fairqueue.HealthStatusFailed {
		t.Fatalf("startup journal safety health = %+v, want failed", snapshot.FairQueue)
	}
}

func TestRAGFairQueueWorkerModeIsTheSoleClaimantSelector(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		enabled bool
		mode    string
		legacy  bool
		fair    bool
		wantErr bool
	}{
		{name: "disabled legacy", mode: config.FairQueueWorkerModeLegacy, legacy: true},
		{name: "enabled legacy", enabled: true, mode: config.FairQueueWorkerModeLegacy, legacy: true},
		{name: "enabled paused", enabled: true, mode: config.FairQueueWorkerModePaused},
		{name: "enabled fair", enabled: true, mode: config.FairQueueWorkerModeFair, fair: true},
		{name: "disabled fair", mode: config.FairQueueWorkerModeFair, wantErr: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			cfg := config.DefaultFairQueueCfg()
			cfg.Enabled = test.enabled
			cfg.RAGIndex.WorkerMode = test.mode
			cfg.MySQLWriterTopology = config.FairQueueMySQLWriterTopologySingle
			if test.enabled {
				cfg.RedisAddr = "redis:6379"
				cfg.RabbitMQURL = "amqp://rabbit/"
			}
			plan, err := planRAGFairQueue(cfg, "mysql")
			if test.wantErr {
				if err == nil {
					t.Fatal("planRAGFairQueue() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("planRAGFairQueue() error = %v", err)
			}
			if plan.StartLegacyClaimant != test.legacy || plan.StartFairClaimant != test.fair ||
				(plan.StartLegacyClaimant && plan.StartFairClaimant) {
				t.Fatalf("claimant plan = %+v, want legacy=%v fair=%v", plan, test.legacy, test.fair)
			}
		})
	}
}

func TestRAGFairQueuePlanRejectsFairNonMySQLBeforeRuntime(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultFairQueueCfg()
	cfg.Enabled = true
	cfg.RedisAddr = "redis:6379"
	cfg.RabbitMQURL = "amqp://rabbit/"
	cfg.MySQLWriterTopology = config.FairQueueMySQLWriterTopologySingle
	cfg.RAGIndex.WorkerMode = config.FairQueueWorkerModeFair
	if _, err := planRAGFairQueue(cfg, "sqlite"); err == nil {
		t.Fatal("planRAGFairQueue(fair, sqlite) error = nil")
	}
}

func TestRAGFairQueueResourceConfigMapsEverySafetyTiming(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultFairQueueCfg().RAGIndex
	cfg.LocalWorkers = 7
	cfg.GlobalConcurrency = 11
	cfg.PerUserBaseConcurrency = 3
	cfg.PerUserBurstConcurrency = 5
	cfg.BorrowEnabled = false
	got := makeRAGFairQueueResourceConfig(cfg)
	if got.Key != rag.RAGFairQueueResource || got.LocalWorkers != cfg.LocalWorkers ||
		got.GlobalConcurrency != cfg.GlobalConcurrency || got.PerUserBaseConcurrency != cfg.PerUserBaseConcurrency ||
		got.PerUserBurstConcurrency != cfg.PerUserBurstConcurrency || got.BorrowEnabled != cfg.BorrowEnabled ||
		got.ReconcileInterval != cfg.ReconcileInterval ||
		got.ExpiredRunningSweepInterval != cfg.ExpiredRunningSweepInterval ||
		got.ReconcilePageSize != cfg.ReconcilePageSize || got.ReservationTTL != cfg.ReservationTTL ||
		got.ReservationHeartbeat != cfg.ReservationHeartbeat || got.PrepareTimeout != cfg.PrepareTimeout ||
		got.ProvisionalTTL != cfg.ProvisionalTTL || got.ProcessingTurnTTL != cfg.ProcessingTurnTTL ||
		got.RecoveryDrainTimeout != cfg.RecoveryDrainTimeout || got.DispatchInterval != cfg.DispatchInterval ||
		got.PublishAttemptTimeout != cfg.PublishAttemptTimeout {
		t.Fatalf("resource config did not map losslessly: got=%+v source=%+v", got, cfg)
	}
}

func TestRAGFairQueueServicePolicyPreservesLegacyDefaults(t *testing.T) {
	t.Parallel()

	cfg := config.DefaultFairQueueCfg().RAGIndex
	cfg.LocalWorkers = 9
	cfg.ReservationTTL = 7 * time.Minute
	cfg.ReservationHeartbeat = 3 * time.Minute

	legacy := rag.Deps{}
	applyRAGFairQueueServicePolicy(&legacy, ragFairQueuePlan{Mode: rag.WorkerModeLegacy}, cfg)
	if legacy.WorkerMode != rag.WorkerModeLegacy || legacy.Workers != 0 ||
		legacy.LeaseDuration != 0 || legacy.HeartbeatInterval != 0 {
		t.Fatalf("legacy deps changed by fair policy: %+v", legacy)
	}

	fair := rag.Deps{}
	applyRAGFairQueueServicePolicy(&fair, ragFairQueuePlan{Mode: rag.WorkerModeFair, StartFairClaimant: true}, cfg)
	if fair.WorkerMode != rag.WorkerModeFair || fair.Workers != cfg.LocalWorkers ||
		fair.LeaseDuration != cfg.ReservationTTL || fair.HeartbeatInterval != cfg.ReservationHeartbeat {
		t.Fatalf("fair deps did not receive immutable resource policy: %+v", fair)
	}
}

func TestRAGFairQueueHealthSanitizesControlAndCombinesOperatorState(t *testing.T) {
	t.Parallel()
	const (
		writer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		opID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		epoch  = "cccccccccccccccccccccccccccccccc"
	)
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair, WriterTopology: "single", Writer: writer,
		ConnectionSafety: func() store.FairQueueConnectionSafetySnapshot {
			return store.FairQueueConnectionSafetySnapshot{
				SessionAffinity:          store.FairQueueSessionAffinityVerified,
				LastSuccessfulVerifiedAt: time.Unix(100, 0).UTC(),
			}
		},
	})
	state.updateJournal(fairqueue.RecoveryOperationRecord{
		Resource: rag.RAGFairQueueResource, OperationID: opID,
		Kind: fairqueue.RecoveryRabbitRepair, Phase: fairqueue.OperationReadyCommitted,
		CurrentWriterFingerprint: writer, Version: 2,
		CreatedAt: time.Unix(90, 0).UTC(), UpdatedAt: time.Unix(95, 0).UTC(),
		RepairHighWater: pointerString("7"), RepairPassComplete: true,
	}, true)
	state.updateRedis(fairqueue.RedisResourceHealthProbe{
		Resource: rag.RAGFairQueueResource,
		Topology: fairqueue.RedisTopology{Mode: fairqueue.RedisDeploymentStandalone, WritablePrimary: true},
		Control: fairqueue.RecoveryControlSnapshot{
			Present: true, State: fairqueue.ResourceReady, Epoch: epoch,
			ProtocolVersion: fairqueue.MessageVersion1, WriterFingerprint: writer,
			Kind: fairqueue.RecoveryNone, LastCompletedOperationID: opID,
			LastCompletedOperationKind: fairqueue.RecoveryRabbitRepair,
		},
	})
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if snapshot.FairQueue.Redis.OperatorRequired || !snapshot.FairQueue.MySQL.ControlFingerprintMatch {
		t.Fatalf("exact READY_COMMITTED match was not accepted: %+v", snapshot.FairQueue)
	}
	blob, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{writer, opID, epoch} {
		if strings.Contains(string(blob), raw) {
			t.Fatalf("health JSON leaked raw control identity %q: %s", raw, blob)
		}
	}

	state.updateRedis(fairqueue.RedisResourceHealthProbe{
		Resource: rag.RAGFairQueueResource,
		Topology: fairqueue.RedisTopology{Mode: fairqueue.RedisDeploymentStandalone, WritablePrimary: true},
		Control: fairqueue.RecoveryControlSnapshot{
			Present: true, State: fairqueue.ResourceReady, Epoch: epoch,
			ProtocolVersion: fairqueue.MessageVersion1, WriterFingerprint: writer,
			Kind: fairqueue.RecoveryNone, LastCompletedOperationID: "dddddddddddddddddddddddddddddddd",
			LastCompletedOperationKind: fairqueue.RecoveryRabbitRepair,
		},
	})
	snapshot = state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if !snapshot.FairQueue.Redis.OperatorRequired || snapshot.FairQueue.MySQL.ControlFingerprintMatch {
		t.Fatalf("mismatched READY_COMMITTED control did not fail closed: %+v", snapshot.FairQueue)
	}
}

func TestRAGFairQueueHealthRequiresEveryHealthyInvariant(t *testing.T) {
	t.Parallel()
	const (
		writer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		epoch  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	now := time.Now().UTC()
	verified := store.FairQueueConnectionSafetySnapshot{
		SessionAffinity:          store.FairQueueSessionAffinityVerified,
		LastSuccessfulVerifiedAt: now,
	}
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair, WriterTopology: "single", Writer: writer,
		ConnectionSafety: func() store.FairQueueConnectionSafetySnapshot { return verified },
	})
	state.updateJournal(fairqueue.RecoveryOperationRecord{}, false)
	state.updateRedis(fairqueue.RedisResourceHealthProbe{
		Resource: rag.RAGFairQueueResource,
		Topology: fairqueue.RedisTopology{Mode: fairqueue.RedisDeploymentStandalone, WritablePrimary: true},
		Control: fairqueue.RecoveryControlSnapshot{
			Present: true, State: fairqueue.ResourceReady, Epoch: epoch,
			ProtocolVersion: fairqueue.MessageVersion1, WriterFingerprint: writer,
			Kind: fairqueue.RecoveryNone,
		},
	})
	state.updateRabbit(fairqueue.RabbitHealthSnapshot{Status: fairqueue.DependencyStatusOK})
	runtime := healthyRAGFairQueueRuntimeSnapshot(now)
	if got := state.snapshot(runtime, fairQueueStatusDegraded).FairQueue.Status; got != fairqueue.HealthStatusHealthy {
		t.Fatalf("complete healthy facts status = %q", got)
	}

	tests := []struct {
		name   string
		mutate func(*fairqueue.HealthSnapshot)
	}{
		{name: "gate closed", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.GateOpen = false }},
		{name: "recovery pending", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Recovery.Startup = fairqueue.RecoveryStartupPending }},
		{name: "recovery not converged", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Recovery.Converged = false }},
		{name: "recovery pass incomplete", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Recovery.OperationPassComplete = false }},
		{name: "scheduler never succeeded", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Scheduler.LastSuccessAt = nil }},
		{name: "dispatcher never succeeded", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Dispatcher.LastSuccessAt = nil }},
		{name: "sweeper never succeeded", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Sweeper.LastSuccessAt = nil }},
		{name: "reconciler never succeeded", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Reconciler.LastSuccessAt = nil }},
		{name: "scheduler stale", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Scheduler.LagSeconds = 3600 }},
		{name: "dispatcher stale", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Dispatcher.LagSeconds = 3600 }},
		{name: "sweeper stale", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Sweeper.LagSeconds = 3600 }},
		{name: "reconciler stale", mutate: func(h *fairqueue.HealthSnapshot) { h.FairQueue.Loops.Reconciler.LagSeconds = 3600 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := runtime
			test.mutate(&candidate)
			if got := state.snapshot(candidate, fairQueueStatusDegraded).FairQueue.Status; got == fairqueue.HealthStatusHealthy {
				t.Fatalf("incomplete runtime facts reported healthy: %+v", candidate.FairQueue)
			}
		})
	}

	verified.SessionAffinity = store.FairQueueSessionAffinityUnknown
	if got := state.snapshot(runtime, fairQueueStatusDegraded).FairQueue.Status; got == fairqueue.HealthStatusHealthy {
		t.Fatal("unknown writer session affinity reported healthy")
	}
}

func TestRAGFairQueueHealthAuthoritativeFailureIsSticky(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{Mode: rag.WorkerModeFair})
	state.failAuthoritativeWriter()
	state.markMySQL(fairqueue.MySQLStatusOK, true)
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusFailed)
	if snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusMismatch ||
		snapshot.FairQueue.Status != fairqueue.HealthStatusFailed {
		t.Fatalf("authoritative failure recovered without restart: %+v", snapshot.FairQueue)
	}
}

func TestRAGFairQueueHealthMarksRedisCorruptionOperatorRequired(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair,
	})
	state.markRedisUnavailable(fairqueue.ErrCoordinationCorrupt)
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if !snapshot.FairQueue.Redis.OperatorRequired || snapshot.FairQueue.Status != fairqueue.HealthStatusFailed {
		t.Fatalf("Redis corruption health = %+v, want failed/operator-required", snapshot.FairQueue)
	}
}

func healthyRAGFairQueueRuntimeSnapshot(now time.Time) fairqueue.HealthSnapshot {
	return fairqueue.HealthSnapshot{FairQueue: fairqueue.FairQueueHealthSnapshot{
		Status: fairqueue.HealthStatusHealthy, GateOpen: true,
		Recovery: fairqueue.RecoveryHealthSnapshot{
			Startup: fairqueue.RecoveryStartupComplete, Converged: true, OperationPassComplete: true,
		},
		Loops: fairqueue.LoopHealthSnapshots{
			Scheduler:  fairqueue.SchedulerLoopHealthSnapshot{State: fairqueue.LoopStateRunning, LastSuccessAt: &now},
			Dispatcher: fairqueue.LoopHealthSnapshot{LastSuccessAt: &now},
			Sweeper:    fairqueue.LoopHealthSnapshot{LastSuccessAt: &now},
			Reconciler: fairqueue.LoopHealthSnapshot{LastSuccessAt: &now},
		},
	}}
}

func TestLegacyFairQueueHealthIsAPIReadyWithoutDependencyClients(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Mode: rag.WorkerModeLegacy,
	})
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if snapshot.FairQueue.Status != fairqueue.HealthStatusHealthy || snapshot.FairQueue.Enabled ||
		snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusOK ||
		!snapshot.FairQueue.MySQL.SchemaReady {
		t.Fatalf("legacy health = %+v", snapshot.FairQueue)
	}
}

func TestEnabledLegacyFairQueueHealthDoesNotInventHealthyDependencies(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeLegacy,
	})
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if snapshot.FairQueue.Status != fairqueue.HealthStatusDegraded ||
		snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusOK ||
		!snapshot.FairQueue.MySQL.SchemaReady {
		t.Fatalf("enabled legacy health = %+v", snapshot.FairQueue)
	}
}

func TestRAGFairQueueJournalProbeDoesNotOverwriteAPIMySQLReadiness(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair,
	})
	state.markMySQL(fairqueue.MySQLStatusUnavailable, true)
	if fatal := state.applyJournalProbe(fairqueue.RecoveryOperationRecord{}, false, nil); fatal != nil {
		t.Fatalf("successful journal probe returned fatal error: %v", fatal)
	}
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusUnavailable {
		t.Fatalf("successful journal probe overwrote API MySQL status: %+v", snapshot.FairQueue.MySQL)
	}

	state.markMySQL(fairqueue.MySQLStatusOK, true)
	for _, journalErr := range []error{
		errors.Join(fairqueue.ErrDependencyUnavailable, errors.New("journal pool unavailable")),
		errors.New("raw driver read failure"),
	} {
		if fatal := state.applyJournalProbe(fairqueue.RecoveryOperationRecord{}, false, journalErr); fatal != nil {
			t.Fatalf("journal dependency outage returned fatal error: %v", fatal)
		}
		snapshot = state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
		if snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusOK ||
			snapshot.FairQueue.Status != fairqueue.HealthStatusDegraded {
			t.Fatalf("journal outage overwrote API readiness or stayed healthy: %+v", snapshot.FairQueue)
		}
		state.mu.RLock()
		journalKnown := state.journalKnown
		state.mu.RUnlock()
		if journalKnown {
			t.Fatal("journal outage retained a stale known journal snapshot")
		}
	}
}

func TestRAGFairQueuePersistedJournalCorruptionIsStickySafetyFailure(t *testing.T) {
	t.Parallel()
	state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerModeFair,
	})
	fatal := state.applyJournalProbe(fairqueue.RecoveryOperationRecord{}, false,
		fairqueue.ErrInvalidOperationRecord)
	if !errors.Is(fatal, fairqueue.ErrInvalidOperationRecord) ||
		!errors.Is(fatal, fairqueue.ErrAuthoritativeStateCorrupt) {
		t.Fatalf("journal corruption fatal = %v, want invalid + authoritative corruption", fatal)
	}
	state.markMySQL(fairqueue.MySQLStatusOK, true)
	snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded)
	if snapshot.FairQueue.Status != fairqueue.HealthStatusFailed ||
		snapshot.FairQueue.MySQL.Status != fairqueue.MySQLStatusOK {
		t.Fatalf("journal corruption health = %+v, want failed scheduler with API MySQL intact", snapshot.FairQueue)
	}
}

func TestRAGFairQueueHealthFailsWhenAPIMySQLOrSchemaSafetyIsNotMet(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		mysqlStatus string
		schemaReady bool
	}{
		{name: "mysql unavailable", mysqlStatus: fairqueue.MySQLStatusUnavailable, schemaReady: true},
		{name: "schema incompatible", mysqlStatus: fairqueue.MySQLStatusOK, schemaReady: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			state := newRAGFairQueueHealthState(ragFairQueueHealthOptions{Mode: rag.WorkerModeLegacy})
			state.markMySQL(test.mysqlStatus, test.schemaReady)
			if snapshot := state.snapshot(fairqueue.HealthSnapshot{}, fairQueueStatusDegraded); snapshot.FairQueue.Status != fairqueue.HealthStatusFailed {
				t.Fatalf("unsafe API MySQL health = %+v, want failed", snapshot.FairQueue)
			}
		})
	}
}

func TestRAGFairQueueOperatorRequiredCombinesJournalAndControl(t *testing.T) {
	t.Parallel()
	const (
		writer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		opID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	ready := fairqueue.RecoveryControlSnapshot{
		Present: true, State: fairqueue.ResourceReady, WriterFingerprint: writer,
		LastCompletedOperationID: opID, LastCompletedOperationKind: fairqueue.RecoveryRabbitRepair,
	}
	recovering := fairqueue.RecoveryControlSnapshot{
		Present: true, State: fairqueue.ResourceRecovering, WriterFingerprint: writer,
		Kind: fairqueue.RecoveryRabbitRepair, OperationID: opID,
	}
	record := fairqueue.RecoveryOperationRecord{
		CurrentWriterFingerprint: writer, Kind: fairqueue.RecoveryRabbitRepair, OperationID: opID,
	}
	tests := []struct {
		name     string
		present  bool
		phase    fairqueue.OperationPhase
		control  fairqueue.RecoveryControlSnapshot
		operator bool
		match    bool
	}{
		{name: "none with ready control", control: ready, match: true},
		{name: "active exact control", present: true, phase: fairqueue.OperationActive, control: recovering, operator: true, match: true},
		{name: "active without control", present: true, phase: fairqueue.OperationActive, operator: true},
		{name: "ready committed exact", present: true, phase: fairqueue.OperationReadyCommitted, control: ready, match: true},
		{name: "ready committed missing", present: true, phase: fairqueue.OperationReadyCommitted, operator: true},
		{name: "ready committed wrong id", present: true, phase: fairqueue.OperationReadyCommitted, control: func() fairqueue.RecoveryControlSnapshot {
			value := ready
			value.LastCompletedOperationID = "cccccccccccccccccccccccccccccccc"
			return value
		}(), operator: true},
		{name: "ready committed wrong kind", present: true, phase: fairqueue.OperationReadyCommitted, control: func() fairqueue.RecoveryControlSnapshot {
			value := ready
			value.LastCompletedOperationKind = fairqueue.RecoveryWriterRebind
			return value
		}(), operator: true},
		{name: "completed permits missing redis rebuild", present: true, phase: fairqueue.OperationCompleted},
		{name: "completed does not excuse special redis control", present: true, phase: fairqueue.OperationCompleted, control: recovering, operator: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			current := record
			current.Phase = test.phase
			operator, match := ragFairQueueOperatorRequired(writer, current, test.present, test.control)
			if operator != test.operator || match != test.match {
				t.Fatalf("operator/match = %v/%v, want %v/%v", operator, match, test.operator, test.match)
			}
		})
	}
}

func pointerString(value string) *string { return &value }
