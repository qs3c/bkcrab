package fairqueue

import (
	"sync"
	"time"
)

const (
	HealthStatusHealthy  = "healthy"
	HealthStatusDegraded = "degraded"
	HealthStatusFailed   = "failed"

	MySQLStatusOK          = "ok"
	MySQLStatusUnavailable = "unavailable"
	MySQLStatusMismatch    = "mismatch"

	DependencyStatusOK          = "ok"
	DependencyStatusRecovering  = "recovering"
	DependencyStatusUnavailable = "unavailable"

	SessionAffinityVerified = "verified"
	SessionAffinityUnknown  = "unknown"
	SessionAffinityMismatch = "mismatch"

	RecoveryStartupPending  = "pending"
	RecoveryStartupRunning  = "running"
	RecoveryStartupComplete = "complete"
	RecoveryStartupFailed   = "failed"

	LoopStateStarting = "starting"
	LoopStateRunning  = "running"
	LoopStatePaused   = "paused"
)

// HealthSnapshot is the complete, serialization-safe operational response.
// It intentionally carries fingerprints rather than raw recovery operation
// IDs, epochs, owners, or tokens. Runtime fills the execution facts while the
// gateway may merge dependency-specific cached facts before publishing it.
type HealthSnapshot struct {
	FairQueue FairQueueHealthSnapshot `json:"fairQueue"`
}

type FairQueueHealthSnapshot struct {
	Enabled      bool                   `json:"enabled"`
	Status       string                 `json:"status"`
	Mode         string                 `json:"mode"`
	GateOpen     bool                   `json:"gateOpen"`
	Fatal        bool                   `json:"fatal"`
	ShuttingDown bool                   `json:"shuttingDown"`
	MySQL        MySQLHealthSnapshot    `json:"mysql"`
	Rabbit       RabbitHealthSnapshot   `json:"rabbit"`
	Redis        RedisHealthSnapshot    `json:"redis"`
	Recovery     RecoveryHealthSnapshot `json:"recovery"`
	Loops        LoopHealthSnapshots    `json:"loops"`
}

type MySQLHealthSnapshot struct {
	Status                           string                         `json:"status"`
	SchemaReady                      bool                           `json:"schemaReady"`
	WriterTopology                   string                         `json:"writerTopology"`
	WriterFingerprint                string                         `json:"writerFingerprint"`
	ControlFingerprintMatch          bool                           `json:"controlFingerprintMatch"`
	LastConnectionIdentityVerifiedAt *time.Time                     `json:"lastConnectionIdentityVerifiedAt"`
	SessionAffinity                  string                         `json:"sessionAffinity"`
	OperationJournal                 OperationJournalHealthSnapshot `json:"operationJournal"`
}

type OperationJournalHealthSnapshot struct {
	Phase                  string  `json:"phase"`
	Kind                   string  `json:"kind"`
	OperationIDFingerprint *string `json:"operationIdFingerprint"`
}

type RabbitHealthSnapshot struct {
	Status           string     `json:"status"`
	ReadyDepthSample int64      `json:"readyDepthSample"`
	DLQDepthSample   int64      `json:"dlqDepthSample"`
	LastConfirmAt    *time.Time `json:"lastConfirmAt"`
	LastReturnAt     *time.Time `json:"lastReturnAt"`
}

type RedisHealthSnapshot struct {
	Status                              string  `json:"status"`
	Mode                                string  `json:"mode"`
	ClusterEnabled                      bool    `json:"clusterEnabled"`
	ResourceState                       string  `json:"resourceState"`
	OperationKind                       string  `json:"operationKind"`
	OperatorRequired                    bool    `json:"operatorRequired"`
	OperationIDFingerprint              *string `json:"operationIdFingerprint"`
	LastCompletedOperationIDFingerprint *string `json:"lastCompletedOperationIdFingerprint"`
	EpochFingerprint                    string  `json:"epochFingerprint"`
	ProvisionalCount                    int64   `json:"provisionalCount"`
	ProcessingCount                     int64   `json:"processingCount"`
}

type RecoveryHealthSnapshot struct {
	Startup               string `json:"startup"`
	PagesCompleted        int64  `json:"pagesCompleted"`
	Converged             bool   `json:"converged"`
	OperationPassComplete bool   `json:"operationPassComplete"`
}

type LoopHealthSnapshots struct {
	Scheduler  SchedulerLoopHealthSnapshot `json:"scheduler"`
	Dispatcher LoopHealthSnapshot          `json:"dispatcher"`
	Sweeper    LoopHealthSnapshot          `json:"sweeper"`
	Reconciler LoopHealthSnapshot          `json:"reconciler"`
}

type SchedulerLoopHealthSnapshot struct {
	State         string     `json:"state"`
	LastSuccessAt *time.Time `json:"lastSuccessAt"`
	LagSeconds    int64      `json:"lagSeconds"`
}

type LoopHealthSnapshot struct {
	LastSuccessAt *time.Time `json:"lastSuccessAt"`
	LagSeconds    int64      `json:"lagSeconds"`
}

type healthLoop uint8

const (
	loopScheduler healthLoop = iota
	loopDispatcher
	loopSweeper
	loopReconciler
)

// resourceHealth is shared by one Runtime resource and all of its loops. All
// writes happen at the actual state transition or successful operation site;
// Snapshot performs no dependency I/O.
type resourceHealth struct {
	mu  sync.RWMutex
	now func() time.Time

	gateOpen     bool
	fatal        bool
	shuttingDown bool
	recovery     RecoveryHealthSnapshot
	loops        LoopHealthSnapshots
}

func newResourceHealth(now func() time.Time) *resourceHealth {
	if now == nil {
		now = time.Now
	}
	return &resourceHealth{
		now:      now,
		recovery: RecoveryHealthSnapshot{Startup: RecoveryStartupPending},
		loops:    LoopHealthSnapshots{Scheduler: SchedulerLoopHealthSnapshot{State: LoopStatePaused}},
	}
}

func (h *resourceHealth) markGateOpen() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.gateOpen = true
	h.loops.Scheduler.State = LoopStateRunning
	h.mu.Unlock()
}

func (h *resourceHealth) markGateClosed() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.gateOpen = false
	h.loops.Scheduler.State = LoopStatePaused
	h.mu.Unlock()
}

func (h *resourceHealth) markShuttingDown() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.shuttingDown = true
	h.mu.Unlock()
}

func (h *resourceHealth) markAuthoritativeFatal() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.fatal = true
	if h.recovery.Startup != RecoveryStartupComplete {
		h.recovery.Startup = RecoveryStartupFailed
	}
	h.mu.Unlock()
}

func (h *resourceHealth) markRecoveryRunning() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recovery = RecoveryHealthSnapshot{Startup: RecoveryStartupRunning}
	h.mu.Unlock()
}

func (h *resourceHealth) markRecoveryPageComplete() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.recovery.Startup == RecoveryStartupRunning {
		h.recovery.PagesCompleted++
	}
	h.mu.Unlock()
}

func (h *resourceHealth) markRecoveryConverged() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recovery.Converged = true
	h.mu.Unlock()
}

func (h *resourceHealth) markRecoveryPassComplete() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recovery.OperationPassComplete = true
	h.mu.Unlock()
}

func (h *resourceHealth) markRecoveryComplete() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recovery.Startup = RecoveryStartupComplete
	h.recovery.Converged = true
	h.recovery.OperationPassComplete = true
	h.mu.Unlock()
}

func (h *resourceHealth) markRecoveryFailed() {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recovery.Startup = RecoveryStartupFailed
	h.mu.Unlock()
}

func (h *resourceHealth) markLoopSuccess(loop healthLoop) {
	if h == nil {
		return
	}
	now := h.now().UTC()
	h.mu.Lock()
	defer h.mu.Unlock()
	switch loop {
	case loopScheduler:
		// A closed-gate idle poll is not scheduler progress.
		if h.gateOpen {
			h.loops.Scheduler.LastSuccessAt = cloneHealthTime(now)
		}
	case loopDispatcher:
		h.loops.Dispatcher.LastSuccessAt = cloneHealthTime(now)
	case loopSweeper:
		h.loops.Sweeper.LastSuccessAt = cloneHealthTime(now)
	case loopReconciler:
		h.loops.Reconciler.LastSuccessAt = cloneHealthTime(now)
	}
}

func (h *resourceHealth) snapshot() FairQueueHealthSnapshot {
	if h == nil {
		return defaultFairQueueHealthSnapshot()
	}
	h.mu.RLock()
	result := defaultFairQueueHealthSnapshot()
	result.GateOpen = h.gateOpen
	result.Fatal = h.fatal
	result.ShuttingDown = h.shuttingDown
	result.Recovery = h.recovery
	result.Loops = cloneLoopHealth(h.loops)
	h.mu.RUnlock()

	switch {
	case result.Fatal || result.Recovery.Startup == RecoveryStartupFailed:
		result.Status = HealthStatusFailed
	case result.GateOpen && !result.ShuttingDown:
		result.Status = HealthStatusHealthy
	default:
		result.Status = HealthStatusDegraded
	}
	now := h.now().UTC()
	result.Loops.Scheduler.LagSeconds = healthLagSeconds(now, result.Loops.Scheduler.LastSuccessAt)
	result.Loops.Dispatcher.LagSeconds = healthLagSeconds(now, result.Loops.Dispatcher.LastSuccessAt)
	result.Loops.Sweeper.LagSeconds = healthLagSeconds(now, result.Loops.Sweeper.LastSuccessAt)
	result.Loops.Reconciler.LagSeconds = healthLagSeconds(now, result.Loops.Reconciler.LastSuccessAt)
	return result
}

func defaultFairQueueHealthSnapshot() FairQueueHealthSnapshot {
	return FairQueueHealthSnapshot{
		Status: HealthStatusDegraded,
		MySQL: MySQLHealthSnapshot{
			Status: MySQLStatusUnavailable, SessionAffinity: SessionAffinityUnknown,
			OperationJournal: OperationJournalHealthSnapshot{Phase: string(RecoveryNone), Kind: string(RecoveryNone)},
		},
		Rabbit:   RabbitHealthSnapshot{Status: DependencyStatusUnavailable},
		Redis:    RedisHealthSnapshot{Status: DependencyStatusUnavailable},
		Recovery: RecoveryHealthSnapshot{Startup: RecoveryStartupPending},
		Loops:    LoopHealthSnapshots{Scheduler: SchedulerLoopHealthSnapshot{State: LoopStatePaused}},
	}
}

func cloneLoopHealth(source LoopHealthSnapshots) LoopHealthSnapshots {
	result := source
	result.Scheduler.LastSuccessAt = cloneHealthTimePtr(source.Scheduler.LastSuccessAt)
	result.Dispatcher.LastSuccessAt = cloneHealthTimePtr(source.Dispatcher.LastSuccessAt)
	result.Sweeper.LastSuccessAt = cloneHealthTimePtr(source.Sweeper.LastSuccessAt)
	result.Reconciler.LastSuccessAt = cloneHealthTimePtr(source.Reconciler.LastSuccessAt)
	return result
}

func cloneHealthTime(value time.Time) *time.Time {
	copy := value
	return &copy
}

func cloneHealthTimePtr(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return cloneHealthTime(*value)
}

func healthLagSeconds(now time.Time, last *time.Time) int64 {
	if last == nil || now.Before(*last) {
		return 0
	}
	return int64(now.Sub(*last) / time.Second)
}
