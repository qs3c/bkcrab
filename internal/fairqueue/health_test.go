package fairqueue

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResourceHealthSnapshotStartsFailClosedAndRecordsActualProgress(t *testing.T) {
	now := time.Date(2026, 8, 3, 1, 2, 3, 0, time.UTC)
	health := newResourceHealth(func() time.Time { return now })

	initial := health.snapshot()
	if initial.Status != HealthStatusDegraded || initial.GateOpen || initial.Fatal || initial.ShuttingDown {
		t.Fatalf("initial runtime health = %+v, want degraded and closed", initial)
	}
	if initial.Recovery.Startup != RecoveryStartupPending || initial.Loops.Scheduler.State != LoopStatePaused {
		t.Fatalf("initial recovery/loops = %+v/%+v", initial.Recovery, initial.Loops)
	}
	if initial.Loops.Scheduler.LastSuccessAt != nil || initial.Loops.Dispatcher.LastSuccessAt != nil ||
		initial.Loops.Sweeper.LastSuccessAt != nil || initial.Loops.Reconciler.LastSuccessAt != nil {
		t.Fatalf("initial loop success was invented: %+v", initial.Loops)
	}

	health.markRecoveryRunning()
	health.markRecoveryPageComplete()
	health.markRecoveryPageComplete()
	health.markGateOpen()
	health.markLoopSuccess(loopScheduler)
	health.markLoopSuccess(loopDispatcher)
	health.markLoopSuccess(loopSweeper)
	health.markLoopSuccess(loopReconciler)
	health.markRecoveryComplete()
	// Continuous READY reconciles happen after startup completion; they must
	// not inflate the startup recovery page count.
	health.markRecoveryPageComplete()

	now = now.Add(2500 * time.Millisecond)
	ready := health.snapshot()
	if ready.Status != HealthStatusHealthy || !ready.GateOpen || ready.Recovery.Startup != RecoveryStartupComplete ||
		ready.Recovery.PagesCompleted != 2 || !ready.Recovery.Converged || !ready.Recovery.OperationPassComplete {
		t.Fatalf("ready health = %+v", ready)
	}
	if ready.Loops.Scheduler.State != LoopStateRunning || ready.Loops.Scheduler.LastSuccessAt == nil ||
		ready.Loops.Scheduler.LagSeconds != 2 || ready.Loops.Dispatcher.LagSeconds != 2 ||
		ready.Loops.Sweeper.LagSeconds != 2 || ready.Loops.Reconciler.LagSeconds != 2 {
		t.Fatalf("loop progress = %+v", ready.Loops)
	}
	*ready.Loops.Scheduler.LastSuccessAt = time.Time{}
	if defensive := health.snapshot().Loops.Scheduler.LastSuccessAt; defensive == nil || !defensive.Equal(now.Add(-2500*time.Millisecond)) {
		t.Fatalf("mutating returned timestamp changed cached health: %v", defensive)
	}

	health.markAuthoritativeFatal()
	health.markGateClosed()
	failed := health.snapshot()
	if failed.Status != HealthStatusFailed || !failed.Fatal || failed.GateOpen ||
		failed.Loops.Scheduler.State != LoopStatePaused {
		t.Fatalf("fatal health = %+v", failed)
	}
}

func TestResourceHealthSnapshotSupportsConcurrentReadersAndWriters(t *testing.T) {
	health := newResourceHealth(time.Now)
	health.markGateOpen()
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < 200; iteration++ {
				_ = health.snapshot()
			}
		}()
		go func(loop healthLoop) {
			defer wait.Done()
			<-start
			for iteration := 0; iteration < 200; iteration++ {
				health.markLoopSuccess(loop)
				health.markRecoveryPageComplete()
			}
		}(healthLoop(index % 4))
	}
	close(start)
	wait.Wait()
	if snapshot := health.snapshot(); snapshot.Status != HealthStatusHealthy {
		t.Fatalf("concurrent health snapshot = %+v", snapshot)
	}
}

func TestHealthSnapshotJSONContainsPlanShapeWithoutRawSafetyTokens(t *testing.T) {
	now := time.Date(2026, 8, 3, 2, 0, 0, 0, time.UTC)
	snapshot := HealthSnapshot{FairQueue: FairQueueHealthSnapshot{
		Enabled: true, Status: HealthStatusDegraded, Mode: "fair",
		MySQL: MySQLHealthSnapshot{
			Status: MySQLStatusOK, SchemaReady: true, WriterTopology: "single",
			WriterFingerprint: "writer-short", ControlFingerprintMatch: true,
			LastConnectionIdentityVerifiedAt: &now, SessionAffinity: SessionAffinityVerified,
			OperationJournal: OperationJournalHealthSnapshot{
				Phase: "ACTIVE", Kind: "WRITER_REBIND", OperationIDFingerprint: ptrString("operation-short"),
			},
		},
		Rabbit: RabbitHealthSnapshot{Status: DependencyStatusUnavailable},
		Redis: RedisHealthSnapshot{
			Status: DependencyStatusRecovering, Mode: "standalone", ResourceState: "RECOVERING",
			OperationKind: "NORMAL", OperatorRequired: true, EpochFingerprint: "epoch-short",
		},
		Recovery: RecoveryHealthSnapshot{Startup: RecoveryStartupRunning},
		Loops:    LoopHealthSnapshots{Scheduler: SchedulerLoopHealthSnapshot{State: LoopStatePaused}},
	}}

	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, field := range []string{`"fairQueue"`, `"mysql"`, `"operationJournal"`, `"rabbit"`, `"redis"`, `"recovery"`, `"loops"`, `"operationIdFingerprint"`} {
		if !strings.Contains(text, field) {
			t.Fatalf("health JSON %s missing %s", text, field)
		}
	}
	for _, forbidden := range []string{"raw-operation-id", "raw-resource-epoch", "owner-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("health JSON leaked %q: %s", forbidden, text)
		}
	}
}

func ptrString(value string) *string { return &value }
