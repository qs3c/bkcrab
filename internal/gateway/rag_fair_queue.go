package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/rag"
	ragtelemetry "github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/store"
)

type fairQueueStatus string

const (
	fairQueueStatusHealthy  fairQueueStatus = "healthy"
	fairQueueStatusDegraded fairQueueStatus = "degraded"
	fairQueueStatusFailed   fairQueueStatus = "failed"
)

type ragFairQueuePlan struct {
	Mode                rag.WorkerMode
	StartLegacyClaimant bool
	StartFairClaimant   bool
}

func planRAGFairQueue(cfg config.FairQueueCfg, storageType string) (ragFairQueuePlan, error) {
	if err := cfg.Validate(storageType); err != nil {
		return ragFairQueuePlan{}, fmt.Errorf("invalid fair queue configuration: %w", err)
	}
	switch cfg.RAGIndex.WorkerMode {
	case config.FairQueueWorkerModeLegacy:
		return ragFairQueuePlan{Mode: rag.WorkerModeLegacy, StartLegacyClaimant: true}, nil
	case config.FairQueueWorkerModePaused:
		return ragFairQueuePlan{Mode: rag.WorkerModePaused}, nil
	case config.FairQueueWorkerModeFair:
		return ragFairQueuePlan{Mode: rag.WorkerModeFair, StartFairClaimant: true}, nil
	default:
		return ragFairQueuePlan{}, errors.New("gateway: unsupported RAG fair queue worker mode")
	}
}

func makeRAGFairQueueResourceConfig(cfg config.FairQueueResourceCfg) fairqueue.ResourceConfig {
	return fairqueue.ResourceConfig{
		Key:                         rag.RAGFairQueueResource,
		ValidateTaskID:              fairqueue.ValidateRAGIndexTaskID,
		LocalWorkers:                cfg.LocalWorkers,
		GlobalConcurrency:           cfg.GlobalConcurrency,
		PerUserBaseConcurrency:      cfg.PerUserBaseConcurrency,
		PerUserBurstConcurrency:     cfg.PerUserBurstConcurrency,
		BorrowEnabled:               cfg.BorrowEnabled,
		ReconcileInterval:           cfg.ReconcileInterval,
		ExpiredRunningSweepInterval: cfg.ExpiredRunningSweepInterval,
		ReconcilePageSize:           cfg.ReconcilePageSize,
		ReservationTTL:              cfg.ReservationTTL,
		ReservationHeartbeat:        cfg.ReservationHeartbeat,
		PrepareTimeout:              cfg.PrepareTimeout,
		ProvisionalTTL:              cfg.ProvisionalTTL,
		ProcessingTurnTTL:           cfg.ProcessingTurnTTL,
		RecoveryDrainTimeout:        cfg.RecoveryDrainTimeout,
		DispatchInterval:            cfg.DispatchInterval,
		PublishAttemptTimeout:       cfg.PublishAttemptTimeout,
	}
}

// applyRAGFairQueueServicePolicy keeps the legacy service constructor contract
// byte-for-byte compatible: fair-only worker counts and lease policy are
// injected only when the fair claimant is selected. Paused and legacy modes
// retain rag.New's historical defaults for maintenance and legacy work.
func applyRAGFairQueueServicePolicy(
	deps *rag.Deps,
	plan ragFairQueuePlan,
	cfg config.FairQueueResourceCfg,
) {
	if deps == nil {
		return
	}
	deps.WorkerMode = plan.Mode
	if !plan.StartFairClaimant {
		return
	}
	deps.Workers = cfg.LocalWorkers
	deps.LeaseDuration = cfg.ReservationTTL
	deps.HeartbeatInterval = cfg.ReservationHeartbeat
}

type ragFairQueueRunnerProxy struct {
	mu      sync.RWMutex
	service *rag.Service
}

func (p *ragFairQueueRunnerProxy) Bind(service *rag.Service) error {
	if p == nil || service == nil || service.WorkerMode() != rag.WorkerModeFair {
		return errors.New("gateway: fair queue runner requires a fair RAG service")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.service != nil && p.service != service {
		return errors.New("gateway: fair queue runner is already bound")
	}
	p.service = service
	return nil
}

func (p *ragFairQueueRunnerProxy) RunFairClaim(ctx context.Context, claim *store.RAGIndexClaim) error {
	if p == nil {
		return fairqueue.ErrResourceNotReady
	}
	p.mu.RLock()
	service := p.service
	p.mu.RUnlock()
	if service == nil {
		return fairqueue.ErrResourceNotReady
	}
	return service.RunFairClaim(ctx, claim)
}

type ragFairQueueAssembly struct {
	mainStore      *store.DBStore
	fairStore      *store.RAGFairQueueStore
	adminStore     *store.FairQueueAdminStore
	adminSource    *store.RAGFairQueueAdminSource
	adapter        *rag.RAGFairQueueAdapter
	runner         *ragFairQueueRunnerProxy
	supervisor     *ragFairQueueSupervisor
	notifier       *rag.RAGFairQueueNotifier
	resourceConfig fairqueue.ResourceConfig
	writer         string
	health         *ragFairQueueHealthState
}

type ragFairQueueHealthOptions struct {
	Enabled          bool
	Mode             rag.WorkerMode
	WriterTopology   string
	Writer           string
	ConnectionSafety func() store.FairQueueConnectionSafetySnapshot
	LoopFreshness    ragFairQueueLoopFreshness
}

type ragFairQueueLoopFreshness struct {
	SchedulerSeconds  int64
	DispatcherSeconds int64
	SweeperSeconds    int64
	ReconcilerSeconds int64
}

// ragFairQueueHealthState owns cached dependency facts. HTTP handlers only
// call snapshot; all Redis/MySQL/Rabbit I/O happens in startup or background
// probes and is converted to sanitized values here.
type ragFairQueueHealthState struct {
	mu sync.RWMutex

	options               ragFairQueueHealthOptions
	mysqlStatus           string
	schemaReady           bool
	journal               fairqueue.RecoveryOperationRecord
	journalSet            bool
	journalKnown          bool
	rabbit                fairqueue.RabbitHealthSnapshot
	redis                 fairqueue.RedisResourceHealthProbe
	redisSet              bool
	redisOperatorRequired bool
	authoritativeFailed   bool
	writerMismatch        bool
}

func newRAGFairQueueHealthState(options ragFairQueueHealthOptions) *ragFairQueueHealthState {
	state := &ragFairQueueHealthState{
		options: options, mysqlStatus: fairqueue.MySQLStatusOK, schemaReady: true,
		rabbit: fairqueue.RabbitHealthSnapshot{Status: fairqueue.DependencyStatusUnavailable},
	}
	return state
}

func (s *ragFairQueueHealthState) updateJournal(record fairqueue.RecoveryOperationRecord, present bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.journal = record
	s.journalSet = present
	s.journalKnown = true
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) markJournalUnavailable() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.journalKnown = false
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) updateRedis(probe fairqueue.RedisResourceHealthProbe) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.redis = probe
	s.redisSet = true
	s.redisOperatorRequired = false
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) markRedisUnavailable(err error) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.redis = fairqueue.RedisResourceHealthProbe{}
	s.redisSet = false
	if errors.Is(err, fairqueue.ErrCoordinationCorrupt) {
		s.redisOperatorRequired = true
	}
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) updateRabbit(snapshot fairqueue.RabbitHealthSnapshot) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.rabbit = cloneRabbitHealth(snapshot)
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) markMySQL(status string, schemaReady bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.writerMismatch {
		s.mysqlStatus = fairqueue.MySQLStatusMismatch
	} else {
		s.mysqlStatus = status
	}
	s.schemaReady = schemaReady
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) failAuthoritativeWriter() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.authoritativeFailed = true
	s.writerMismatch = true
	s.mysqlStatus = fairqueue.MySQLStatusMismatch
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) failAuthoritativeState() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.authoritativeFailed = true
	s.mu.Unlock()
}

func (s *ragFairQueueHealthState) applyJournalProbe(
	record fairqueue.RecoveryOperationRecord,
	present bool,
	err error,
) error {
	if s == nil {
		return nil
	}
	if err == nil {
		s.updateJournal(record, present)
		return nil
	}
	if errors.Is(err, fairqueue.ErrAuthoritativeWriterMismatch) {
		s.failAuthoritativeWriter()
		return err
	}
	if errors.Is(err, fairqueue.ErrAuthoritativeStateCorrupt) ||
		errors.Is(err, fairqueue.ErrInvalidOperationRecord) {
		fatal := errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
		s.failAuthoritativeState()
		return fatal
	}
	// The journal uses a separate authoritative/admin pool. Its availability
	// controls scheduler health, never the API store's MySQL readiness facts.
	s.markJournalUnavailable()
	return nil
}

func (s *ragFairQueueHealthState) runMySQLProbe(
	ctx context.Context,
	dbStore *store.DBStore,
	expectedWriter string,
	onMismatch func(error),
) {
	if s == nil || ctx == nil || dbStore == nil {
		return
	}
	probe := func() {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if expectedWriter == "" {
			if err := dbStore.DB().PingContext(probeCtx); err != nil {
				s.markMySQL(fairqueue.MySQLStatusUnavailable, true)
				return
			}
			s.markMySQL(fairqueue.MySQLStatusOK, true)
			return
		}
		identity, err := dbStore.ReadFairQueueWriterIdentity(probeCtx)
		if err == nil && identity.Fingerprint == expectedWriter {
			s.markMySQL(fairqueue.MySQLStatusOK, true)
			return
		}
		safety := dbStore.ReadFairQueueConnectionSafetySnapshot()
		if err == nil || safety.SessionAffinity == store.FairQueueSessionAffinityMismatch ||
			errors.Is(err, store.ErrFairQueueWriterMismatch) {
			mismatchErr := errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, err)
			s.failAuthoritativeWriter()
			if onMismatch != nil {
				onMismatch(mismatchErr)
			}
			return
		}
		s.markMySQL(fairqueue.MySQLStatusUnavailable, true)
	}
	probe()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			probe()
		}
	}
}

func (s *ragFairQueueHealthState) snapshot(
	runtime fairqueue.HealthSnapshot,
	supervisorStatus fairQueueStatus,
) fairqueue.HealthSnapshot {
	if s == nil {
		return fairqueue.HealthSnapshot{FairQueue: fairqueue.FairQueueHealthSnapshot{
			Status: fairqueue.HealthStatusFailed,
			MySQL: fairqueue.MySQLHealthSnapshot{Status: fairqueue.MySQLStatusUnavailable,
				SessionAffinity: fairqueue.SessionAffinityUnknown},
		}}
	}
	s.mu.RLock()
	options := s.options
	mysqlStatus := s.mysqlStatus
	schemaReady := s.schemaReady
	journal := s.journal
	journalSet := s.journalSet
	journalKnown := s.journalKnown
	rabbitSnapshot := cloneRabbitHealth(s.rabbit)
	redisProbe := s.redis
	redisSet := s.redisSet
	redisOperatorRequired := s.redisOperatorRequired
	authoritativeFailed := s.authoritativeFailed
	writerMismatch := s.writerMismatch
	s.mu.RUnlock()

	health := runtime.FairQueue
	if health.Status == "" {
		health = fairqueue.FairQueueHealthSnapshot{
			Status:   fairqueue.HealthStatusDegraded,
			Recovery: fairqueue.RecoveryHealthSnapshot{Startup: fairqueue.RecoveryStartupPending},
			Loops: fairqueue.LoopHealthSnapshots{
				Scheduler: fairqueue.SchedulerLoopHealthSnapshot{State: fairqueue.LoopStatePaused},
			},
		}
	}
	health.Enabled = options.Enabled
	health.Mode = string(options.Mode)
	health.MySQL = fairqueue.MySQLHealthSnapshot{
		Status: mysqlStatus, SchemaReady: schemaReady,
		WriterTopology:    options.WriterTopology,
		WriterFingerprint: shortRAGFairQueueFingerprint(options.Writer),
		SessionAffinity:   fairqueue.SessionAffinityUnknown,
		OperationJournal: fairqueue.OperationJournalHealthSnapshot{
			Phase: string(fairqueue.RecoveryNone), Kind: string(fairqueue.RecoveryNone),
		},
	}
	if options.ConnectionSafety != nil {
		safety := options.ConnectionSafety()
		health.MySQL.SessionAffinity = string(safety.SessionAffinity)
		if !safety.LastSuccessfulVerifiedAt.IsZero() {
			verifiedAt := safety.LastSuccessfulVerifiedAt.UTC()
			health.MySQL.LastConnectionIdentityVerifiedAt = &verifiedAt
		}
		if safety.SessionAffinity == store.FairQueueSessionAffinityMismatch {
			health.MySQL.Status = fairqueue.MySQLStatusMismatch
		}
	}
	if journalSet {
		health.MySQL.OperationJournal.Phase = string(journal.Phase)
		health.MySQL.OperationJournal.Kind = string(journal.Kind)
		health.MySQL.OperationJournal.OperationIDFingerprint = optionalShortRAGFairQueueFingerprint(journal.OperationID)
	}
	health.Rabbit = rabbitSnapshot
	health.Redis = fairqueue.RedisHealthSnapshot{Status: fairqueue.DependencyStatusUnavailable}
	control := fairqueue.RecoveryControlSnapshot{}
	if redisSet {
		control = redisProbe.Control
		health.Redis.Status = fairqueue.DependencyStatusOK
		health.Redis.Mode = string(redisProbe.Topology.Mode)
		health.Redis.ClusterEnabled = redisProbe.Topology.Mode == fairqueue.RedisDeploymentCluster
		health.Redis.ActiveCount = redisProbe.ActiveCount
		health.Redis.RingCount = redisProbe.RingCount
		health.Redis.RingMemberCount = redisProbe.RingMemberCount
		health.Redis.GlobalInflight = redisProbe.GlobalInflight
		health.Redis.ProvisionalCount = redisProbe.ProvisionalCount
		health.Redis.StableCount = redisProbe.StableCount
		health.Redis.ProcessingCount = redisProbe.ProcessingCount
		if control.Present {
			health.Redis.ResourceState = string(control.State)
			health.Redis.OperationKind = string(control.Kind)
			health.Redis.OperationIDFingerprint = optionalShortRAGFairQueueFingerprint(control.OperationID)
			health.Redis.LastCompletedOperationIDFingerprint = optionalShortRAGFairQueueFingerprint(control.LastCompletedOperationID)
			health.Redis.EpochFingerprint = shortRAGFairQueueFingerprint(control.Epoch)
			if control.State == fairqueue.ResourceRecovering {
				health.Redis.Status = fairqueue.DependencyStatusRecovering
			}
		}
	}
	operatorRequired, controlMatch := ragFairQueueOperatorRequired(
		options.Writer, journal, journalSet, control,
	)
	operatorRequired = operatorRequired || redisOperatorRequired
	health.Redis.OperatorRequired = operatorRequired
	health.MySQL.ControlFingerprintMatch = controlMatch
	if writerMismatch {
		health.MySQL.Status = fairqueue.MySQLStatusMismatch
	}

	switch {
	case supervisorStatus == fairQueueStatusFailed || health.Fatal || authoritativeFailed || operatorRequired ||
		health.MySQL.Status != fairqueue.MySQLStatusOK || !health.MySQL.SchemaReady:
		health.Status = fairqueue.HealthStatusFailed
	case !options.Enabled && options.Mode != rag.WorkerModeFair:
		health.Status = fairqueue.HealthStatusHealthy
	case ragFairQueueHealthy(health, options, journalKnown):
		health.Status = fairqueue.HealthStatusHealthy
	default:
		health.Status = fairqueue.HealthStatusDegraded
	}
	return fairqueue.HealthSnapshot{FairQueue: health}
}

func ragFairQueueOperatorRequired(
	expectedWriter string,
	journal fairqueue.RecoveryOperationRecord,
	journalSet bool,
	control fairqueue.RecoveryControlSnapshot,
) (operatorRequired, controlMatch bool) {
	writerMatch := !control.Present || control.WriterFingerprint == expectedWriter
	controlMatch = writerMatch
	if !writerMatch {
		return true, false
	}
	if journalSet {
		if journal.CurrentWriterFingerprint != expectedWriter {
			return true, false
		}
		switch journal.Phase {
		case fairqueue.OperationActive:
			return true, control.Present && control.State == fairqueue.ResourceRecovering &&
				control.Kind == journal.Kind && control.OperationID == journal.OperationID
		case fairqueue.OperationReadyCommitted:
			exact := control.Present && control.State == fairqueue.ResourceReady &&
				control.LastCompletedOperationID == journal.OperationID &&
				control.LastCompletedOperationKind == journal.Kind
			return !exact, exact
		case fairqueue.OperationCompleted:
			// A completed journal does not block a normal Redis rebuild.
		default:
			return true, false
		}
	}
	if expectedWriter != "" && !control.Present {
		return false, false
	}
	if control.Present && control.State == fairqueue.ResourceRecovering && control.Kind != fairqueue.RecoveryNormal {
		return true, false
	}
	return false, controlMatch
}

func ragFairQueueHealthy(
	health fairqueue.FairQueueHealthSnapshot,
	options ragFairQueueHealthOptions,
	journalKnown bool,
) bool {
	controlReady := health.Redis.Status == fairqueue.DependencyStatusOK &&
		health.Redis.ResourceState == string(fairqueue.ResourceReady) &&
		health.Redis.OperationKind == string(fairqueue.RecoveryNone) &&
		health.MySQL.ControlFingerprintMatch
	return options.Enabled && options.Mode == rag.WorkerModeFair && journalKnown &&
		health.GateOpen && !health.Fatal && !health.ShuttingDown &&
		health.MySQL.Status == fairqueue.MySQLStatusOK && health.MySQL.SchemaReady &&
		health.MySQL.SessionAffinity == fairqueue.SessionAffinityVerified &&
		health.Rabbit.Status == fairqueue.DependencyStatusOK && controlReady &&
		health.Recovery.Startup == fairqueue.RecoveryStartupComplete &&
		health.Recovery.Converged && health.Recovery.OperationPassComplete &&
		ragFairQueueLoopsFresh(health.Loops, options.LoopFreshness)
}

func ragFairQueueLoopsFresh(loops fairqueue.LoopHealthSnapshots, limits ragFairQueueLoopFreshness) bool {
	limits = limits.withDefaults()
	return loops.Scheduler.State == fairqueue.LoopStateRunning &&
		loopFresh(loops.Scheduler.LastSuccessAt, loops.Scheduler.LagSeconds, limits.SchedulerSeconds) &&
		loopFresh(loops.Dispatcher.LastSuccessAt, loops.Dispatcher.LagSeconds, limits.DispatcherSeconds) &&
		loopFresh(loops.Sweeper.LastSuccessAt, loops.Sweeper.LagSeconds, limits.SweeperSeconds) &&
		loopFresh(loops.Reconciler.LastSuccessAt, loops.Reconciler.LagSeconds, limits.ReconcilerSeconds)
}

func (l ragFairQueueLoopFreshness) withDefaults() ragFairQueueLoopFreshness {
	const defaultSeconds = int64(60)
	if l.SchedulerSeconds <= 0 {
		l.SchedulerSeconds = defaultSeconds
	}
	if l.DispatcherSeconds <= 0 {
		l.DispatcherSeconds = defaultSeconds
	}
	if l.SweeperSeconds <= 0 {
		l.SweeperSeconds = defaultSeconds
	}
	if l.ReconcilerSeconds <= 0 {
		l.ReconcilerSeconds = defaultSeconds
	}
	return l
}

func loopFresh(last *time.Time, lagSeconds, maximumSeconds int64) bool {
	return last != nil && lagSeconds >= 0 && lagSeconds <= maximumSeconds
}

func shortRAGFairQueueFingerprint(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:6])
}

func optionalShortRAGFairQueueFingerprint(value string) *string {
	if value == "" {
		return nil
	}
	fingerprint := shortRAGFairQueueFingerprint(value)
	return &fingerprint
}

func cloneRabbitHealth(source fairqueue.RabbitHealthSnapshot) fairqueue.RabbitHealthSnapshot {
	result := source
	if source.LastConfirmAt != nil {
		value := source.LastConfirmAt.UTC()
		result.LastConfirmAt = &value
	}
	if source.LastReturnAt != nil {
		value := source.LastReturnAt.UTC()
		result.LastReturnAt = &value
	}
	return result
}

func initializeRAGFairQueueJournalHealth(
	ctx context.Context,
	journal fairqueue.OperationJournal,
	writer string,
	health *ragFairQueueHealthState,
) error {
	if ctx == nil || journal == nil || writer == "" || health == nil {
		return errors.New("gateway: invalid initial RAG fair queue journal probe")
	}
	record, present, readErr := journal.Read(ctx, rag.RAGFairQueueResource, writer)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if fatal := health.applyJournalProbe(record, present, readErr); fatal != nil {
		return fmt.Errorf("gateway: read RAG fair queue operation journal: %w", fatal)
	}
	return nil
}

func buildRAGFairQueueAssembly(
	ctx context.Context,
	env *config.EnvConfig,
	st store.Store,
) (*ragFairQueueAssembly, error) {
	if ctx == nil || env == nil {
		return nil, errors.New("gateway: invalid fair queue assembly inputs")
	}
	dbStore, ok := st.(*store.DBStore)
	if !ok || dbStore == nil {
		return nil, store.ErrFairQueueMySQLRequired
	}
	identity, err := dbStore.ReadFairQueueWriterIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: read fair queue writer identity: %w", err)
	}
	fairStore, err := dbStore.BindRAGFairQueueWriter(identity.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("gateway: bind live RAG fair queue store: %w", err)
	}

	adminStore, err := store.OpenFairQueueAdminStore(store.StorageConfig{
		Type: store.StorageMySQL, DSN: env.Storage.DSN, AutoMigrate: false,
	})
	if err != nil {
		return nil, fmt.Errorf("gateway: open RAG fair queue admin store: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = adminStore.Close()
		}
	}()
	if adminStore.WriterFingerprint() != identity.Fingerprint {
		return nil, errors.Join(fairqueue.ErrAuthoritativeWriterMismatch,
			errors.New("gateway: live and admin stores resolved different writers"))
	}
	adminSource, err := adminStore.BindRAGFairQueueSource(identity.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("gateway: bind RAG fair queue admin source: %w", err)
	}
	contract, err := adminSource.CheckSchemaAndInvariants(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: inspect RAG fair queue contract: %w", err)
	}
	if !contract.Contracted {
		return nil, errors.New("gateway: RAG fair queue schema/contract is not ready")
	}
	journalStore, err := rag.NewRAGFairQueueStoreJournal(adminSource)
	if err != nil {
		return nil, err
	}
	runner := &ragFairQueueRunnerProxy{}
	resourceConfig := makeRAGFairQueueResourceConfig(env.FairQueue.RAGIndex)
	fairTelemetry := ragtelemetry.NewFairQueueSink(ragtelemetry.NewSlogRecorder(nil))
	adapter, err := rag.NewRAGFairQueueAdapter(
		fairStore, runner, adminSource, journalStore,
		rag.RAGFairQueueAdapterOptions{
			WorkerID:      "rag-fair-" + uuid.NewString(),
			LeaseDuration: resourceConfig.ReservationTTL,
			Telemetry:     fairTelemetry,
			ClaimLimits: store.RAGFairQueueClaimLimits{
				GlobalConcurrency:       resourceConfig.GlobalConcurrency,
				PerUserBurstConcurrency: resourceConfig.PerUserBurstConcurrency,
				AdvisoryLockTimeout:     boundedRAGFairQueueAdvisoryTimeout(resourceConfig.PrepareTimeout),
				MaintenanceRetryDelay:   resourceConfig.DispatchInterval,
			},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("gateway: create RAG fair queue adapter: %w", err)
	}
	readiness, err := adapter.CheckSchemaAndInvariants(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: verify RAG fair queue writer readiness: %w", err)
	}
	if !readiness.Ready() {
		return nil, errors.New("gateway: RAG fair queue writer invariants are not ready")
	}
	health := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: env.FairQueue.Enabled, Mode: rag.WorkerModeFair,
		WriterTopology: env.FairQueue.MySQLWriterTopology, Writer: identity.Fingerprint,
		ConnectionSafety: dbStore.ReadFairQueueConnectionSafetySnapshot,
		LoopFreshness:    makeRAGFairQueueLoopFreshness(resourceConfig),
	})
	if err := initializeRAGFairQueueJournalHealth(
		ctx, adapter.OperationJournal(), identity.Fingerprint, health,
	); err != nil {
		return nil, err
	}
	registration, err := adapter.ResourceRegistration(resourceConfig)
	if err != nil {
		return nil, fmt.Errorf("gateway: register RAG fair queue resource config: %w", err)
	}
	registry, err := fairqueue.NewRegistry(resourceConfig)
	if err != nil {
		return nil, err
	}
	builder := ragFairQueueRuntimeBuilder{
		newCoordinator: func(probeCtx context.Context) (fairqueue.RuntimeCoordinator, error) {
			coordinator, err := fairqueue.NewRedis(probeCtx, fairqueue.RedisOptions{
				Addr: env.FairQueue.RedisAddr, Password: env.FairQueue.RedisPassword,
				DB: env.FairQueue.RedisDB, KeyPrefix: env.FairQueue.KeyPrefix,
				OperationTimeout: resourceConfig.PublishAttemptTimeout, Telemetry: fairTelemetry,
			})
			if err != nil {
				health.markRedisUnavailable(err)
				return nil, err
			}
			probe, err := coordinator.ProbeResourceHealth(probeCtx, rag.RAGFairQueueResource)
			if err != nil {
				health.markRedisUnavailable(err)
				_ = coordinator.Close()
				return nil, err
			}
			health.updateRedis(probe)
			return coordinator, nil
		},
		newRabbit: func() (ragFairQueueRabbit, error) {
			return fairqueue.NewRabbit(fairqueue.RabbitOptions{
				URL: env.FairQueue.RabbitMQURL, Exchange: env.FairQueue.Exchange,
				DeadLetterExchange: env.FairQueue.DeadLetterExchange,
				OperationTimeout:   resourceConfig.PublishAttemptTimeout,
				Telemetry:          fairTelemetry,
			}, registry)
		},
		newRuntime: func(
			rabbitClient fairqueue.RabbitClient,
			coordinator fairqueue.RuntimeCoordinator,
			journal fairqueue.OperationJournal,
		) (ragFairQueueRuntime, error) {
			runtime, err := fairqueue.NewRuntime(rabbitClient, coordinator, journal, fairqueue.RuntimeOptions{
				ShutdownGrace: resourceConfig.RecoveryDrainTimeout, Telemetry: fairTelemetry,
			})
			if err != nil {
				return nil, err
			}
			rabbitHealthClient, rabbitOK := rabbitClient.(ragFairQueueRabbit)
			redisConcrete, redisOK := coordinator.(*fairqueue.Redis)
			if !rabbitOK || !redisOK {
				return nil, errors.New("gateway: fair queue dependency generation lost concrete health boundary")
			}
			health.updateRabbit(rabbitHealthClient.HealthSnapshot())
			return &ragFairQueueManagedRuntime{
				Runtime: runtime, redis: redisConcrete, rabbit: rabbitHealthClient,
				journal: journal, writer: identity.Fingerprint, health: health,
				probeTimeout:  resourceConfig.PublishAttemptTimeout,
				probeInterval: boundedRAGFairQueueHealthInterval(resourceConfig.ReconcileInterval),
			}, nil
		},
		onRabbitHealth: health.updateRabbit,
		journal:        adapter.OperationJournal(), registration: registration,
	}
	supervisor := newRAGFairQueueSupervisor(builder.Build, ragFairQueueSupervisorOptions{
		BuildTimeout: resourceConfig.RecoveryDrainTimeout,
	})
	notifier, err := rag.NewRAGFairQueueNotifier(supervisor)
	if err != nil {
		return nil, err
	}
	closeOnError = false
	return &ragFairQueueAssembly{
		mainStore: dbStore, fairStore: fairStore, adminStore: adminStore,
		adminSource: adminSource, adapter: adapter, runner: runner,
		supervisor: supervisor, notifier: notifier,
		resourceConfig: resourceConfig, writer: identity.Fingerprint,
		health: health,
	}, nil
}

type ragFairQueueManagedRuntime struct {
	*fairqueue.Runtime
	redis         *fairqueue.Redis
	rabbit        ragFairQueueRabbit
	journal       fairqueue.OperationJournal
	writer        string
	health        *ragFairQueueHealthState
	probeTimeout  time.Duration
	probeInterval time.Duration
}

func (r *ragFairQueueManagedRuntime) Run(ctx context.Context) error {
	if r == nil || r.Runtime == nil || r.redis == nil || r.rabbit == nil || r.health == nil {
		return errors.New("gateway: invalid managed RAG fair queue runtime")
	}
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	var monitorWG sync.WaitGroup
	monitorWG.Add(1)
	go func() {
		defer monitorWG.Done()
		r.runHealthProbe(monitorCtx)
	}()
	err := r.Runtime.Run(ctx)
	cancelMonitor()
	monitorWG.Wait()
	r.health.updateRabbit(r.rabbit.HealthSnapshot())
	return err
}

func (r *ragFairQueueManagedRuntime) runHealthProbe(ctx context.Context) {
	r.refreshHealth(ctx)
	ticker := time.NewTicker(r.probeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshHealth(ctx)
		}
	}
}

func (r *ragFairQueueManagedRuntime) refreshHealth(ctx context.Context) {
	if ctx == nil || ctx.Err() != nil {
		return
	}
	r.health.updateRabbit(r.rabbit.HealthSnapshot())
	redisCtx, cancelRedis := context.WithTimeout(ctx, r.probeTimeout)
	redisProbe, redisErr := r.redis.ProbeResourceHealth(redisCtx, rag.RAGFairQueueResource)
	cancelRedis()
	if redisErr == nil {
		r.health.updateRedis(redisProbe)
	} else {
		r.health.markRedisUnavailable(redisErr)
	}
	journalCtx, cancelJournal := context.WithTimeout(ctx, r.probeTimeout)
	journalRecord, journalPresent, journalErr := r.journal.Read(
		journalCtx, rag.RAGFairQueueResource, r.writer,
	)
	cancelJournal()
	if fatal := r.health.applyJournalProbe(journalRecord, journalPresent, journalErr); fatal != nil {
		_ = r.Runtime.FailAuthoritative(fatal)
	}
}

func makeRAGFairQueueLoopFreshness(config fairqueue.ResourceConfig) ragFairQueueLoopFreshness {
	return ragFairQueueLoopFreshness{
		SchedulerSeconds:  durationFreshnessSeconds(5 * time.Second),
		DispatcherSeconds: durationFreshnessSeconds(3 * config.DispatchInterval),
		SweeperSeconds:    durationFreshnessSeconds(3 * config.ExpiredRunningSweepInterval),
		ReconcilerSeconds: durationFreshnessSeconds(3 * config.ReconcileInterval),
	}
}

func durationFreshnessSeconds(value time.Duration) int64 {
	const minimum = 5 * time.Second
	if value < minimum {
		value = minimum
	}
	return int64((value + time.Second - 1) / time.Second)
}

func boundedRAGFairQueueHealthInterval(reconcile time.Duration) time.Duration {
	const maximum = 5 * time.Second
	if reconcile <= 0 || reconcile > maximum {
		return maximum
	}
	return reconcile
}

func boundedRAGFairQueueAdvisoryTimeout(prepare time.Duration) time.Duration {
	const maximum = 5 * time.Second
	if prepare <= 0 || prepare > maximum {
		return maximum
	}
	return prepare
}

func (a *ragFairQueueAssembly) BindService(service *rag.Service) error {
	if a == nil || a.runner == nil {
		return errors.New("gateway: invalid RAG fair queue assembly")
	}
	return a.runner.Bind(service)
}

func (a *ragFairQueueAssembly) InstallSafetyObserver() {
	if a == nil || a.mainStore == nil || a.health == nil || a.supervisor == nil {
		return
	}
	a.mainStore.SetFairQueueSafetyFailureObserver(func() {
		a.health.failAuthoritativeWriter()
		failure := errors.Join(
			fairqueue.ErrAuthoritativeWriterMismatch,
			store.ErrFairQueueWriterMismatch,
		)
		_ = a.supervisor.FailAuthoritative(failure)
	})
}

func (a *ragFairQueueAssembly) Close() error {
	if a == nil || a.adminStore == nil {
		return nil
	}
	return a.adminStore.Close()
}

type ragFairQueueRabbit interface {
	fairqueue.RabbitClient
	ProbeResourceTopology(context.Context, string) (fairqueue.RabbitResourceProbe, error)
	HealthSnapshot() fairqueue.RabbitHealthSnapshot
}

type ragFairQueueRuntime interface {
	RegisterResource(fairqueue.ResourceRegistration) error
	Run(context.Context) error
	TryDispatch(context.Context, string, string) (bool, error)
	Shutdown(context.Context) error
}

type ragFairQueueRuntimeHealth interface {
	HealthSnapshot(string) (fairqueue.HealthSnapshot, error)
}

type ragFairQueueAuthoritativeFailureRuntime interface {
	FailAuthoritative(error) error
}

// ragFairQueueRuntimeBuilder constructs one single-use dependency generation.
// Registration is completed before the runtime is returned to the supervisor,
// so no scheduler or dispatcher loop can race ahead of topology readiness.
type ragFairQueueRuntimeBuilder struct {
	newCoordinator func(context.Context) (fairqueue.RuntimeCoordinator, error)
	newRabbit      func() (ragFairQueueRabbit, error)
	newRuntime     func(fairqueue.RabbitClient, fairqueue.RuntimeCoordinator, fairqueue.OperationJournal) (ragFairQueueRuntime, error)
	onRabbitHealth func(fairqueue.RabbitHealthSnapshot)
	journal        fairqueue.OperationJournal
	registration   fairqueue.ResourceRegistration
}

func (b ragFairQueueRuntimeBuilder) Build(ctx context.Context) (ragFairQueueRuntime, error) {
	if ctx == nil || b.newCoordinator == nil || b.newRabbit == nil || b.newRuntime == nil || b.journal == nil {
		return nil, errors.New("gateway: invalid RAG fair queue runtime builder")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	coordinator, err := b.newCoordinator(ctx)
	if err != nil {
		return nil, err
	}
	if coordinator == nil {
		return nil, errors.New("gateway: fair queue coordinator constructor returned nil")
	}
	rabbit, err := b.newRabbit()
	if err != nil {
		_ = coordinator.Close()
		return nil, err
	}
	if rabbit == nil {
		_ = coordinator.Close()
		return nil, errors.New("gateway: fair queue Rabbit constructor returned nil")
	}
	if b.onRabbitHealth != nil {
		b.onRabbitHealth(rabbit.HealthSnapshot())
	}
	if _, err := rabbit.ProbeResourceTopology(ctx, b.registration.Config.Key); err != nil {
		if b.onRabbitHealth != nil {
			b.onRabbitHealth(rabbit.HealthSnapshot())
		}
		_ = rabbit.Close()
		_ = coordinator.Close()
		return nil, err
	}
	if b.onRabbitHealth != nil {
		b.onRabbitHealth(rabbit.HealthSnapshot())
	}
	runtime, err := b.newRuntime(rabbit, coordinator, b.journal)
	if err != nil {
		_ = rabbit.Close()
		_ = coordinator.Close()
		return nil, err
	}
	if runtime == nil {
		_ = rabbit.Close()
		_ = coordinator.Close()
		return nil, errors.New("gateway: fair queue runtime constructor returned nil")
	}
	if err := runtime.RegisterResource(b.registration); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = runtime.Shutdown(shutdownCtx)
		cancel()
		return nil, err
	}
	return runtime, nil
}

type ragFairQueueRuntimeFactory func(context.Context) (ragFairQueueRuntime, error)

type ragFairQueueSupervisorOptions struct {
	BuildTimeout   time.Duration
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	Wait           func(context.Context, time.Duration) error
}

func (o ragFairQueueSupervisorOptions) withDefaults() ragFairQueueSupervisorOptions {
	if o.BuildTimeout <= 0 {
		o.BuildTimeout = 20 * time.Second
	}
	if o.BackoffInitial <= 0 {
		o.BackoffInitial = 250 * time.Millisecond
	}
	if o.BackoffMax <= 0 {
		o.BackoffMax = 5 * time.Second
	}
	if o.BackoffMax < o.BackoffInitial {
		o.BackoffMax = o.BackoffInitial
	}
	if o.Wait == nil {
		o.Wait = waitRAGFairQueueRetry
	}
	return o
}

type ragFairQueueSupervisor struct {
	factory ragFairQueueRuntimeFactory
	options ragFairQueueSupervisorOptions

	mu      sync.RWMutex
	runtime ragFairQueueRuntime
	status  fairQueueStatus
	lastErr error
}

func newRAGFairQueueSupervisor(factory ragFairQueueRuntimeFactory, options ragFairQueueSupervisorOptions) *ragFairQueueSupervisor {
	return &ragFairQueueSupervisor{
		factory: factory, options: options.withDefaults(), status: fairQueueStatusDegraded,
	}
}

func (s *ragFairQueueSupervisor) Status() fairQueueStatus {
	if s == nil {
		return fairQueueStatusFailed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

func (s *ragFairQueueSupervisor) setAttempt(runtime ragFairQueueRuntime, status fairQueueStatus, err error) {
	s.mu.Lock()
	if s.status == fairQueueStatusFailed && status != fairQueueStatusFailed &&
		(s.lastErr != nil && !ragFairQueueRetryable(s.lastErr)) {
		s.mu.Unlock()
		return
	}
	s.runtime = runtime
	s.status = status
	s.lastErr = err
	s.mu.Unlock()
}

// bindRuntime atomically checks the terminal latch and publishes a newly built
// dependency generation. A writer-safety failure that races construction must
// never leave an untracked runtime able to enter recovery or open its gates.
func (s *ragFairQueueSupervisor) bindRuntime(runtime ragFairQueueRuntime) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == fairQueueStatusFailed && s.lastErr != nil && !ragFairQueueRetryable(s.lastErr) {
		return s.lastErr
	}
	s.runtime = runtime
	s.status = fairQueueStatusDegraded
	s.lastErr = nil
	return nil
}

func (s *ragFairQueueSupervisor) clearRuntime(runtime ragFairQueueRuntime, err error) {
	s.mu.Lock()
	if s.runtime == runtime {
		s.runtime = nil
	}
	if !(s.status == fairQueueStatusFailed && s.lastErr != nil && !ragFairQueueRetryable(s.lastErr)) {
		s.status = fairQueueStatusDegraded
		s.lastErr = err
	}
	s.mu.Unlock()
}

func shutdownRAGFairQueueRuntime(runtime ragFairQueueRuntime) error {
	if runtime == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return runtime.Shutdown(shutdownCtx)
}

func rejectRAGFairQueueRuntime(runtime ragFairQueueRuntime, cause error) error {
	var failErr error
	if failureRuntime, ok := runtime.(ragFairQueueAuthoritativeFailureRuntime); ok &&
		(errors.Is(cause, fairqueue.ErrAuthoritativeWriterMismatch) ||
			errors.Is(cause, fairqueue.ErrAuthoritativeStateCorrupt)) {
		failErr = failureRuntime.FailAuthoritative(cause)
	}
	return errors.Join(failErr, shutdownRAGFairQueueRuntime(runtime))
}

func (s *ragFairQueueSupervisor) FailAuthoritative(err error) error {
	if s == nil || (!errors.Is(err, fairqueue.ErrAuthoritativeWriterMismatch) &&
		!errors.Is(err, fairqueue.ErrAuthoritativeStateCorrupt)) {
		return errors.New("gateway: fair queue authoritative failure is required")
	}
	s.mu.Lock()
	runtime := s.runtime
	s.status = fairQueueStatusFailed
	s.lastErr = err
	s.mu.Unlock()
	if failureRuntime, ok := runtime.(ragFairQueueAuthoritativeFailureRuntime); ok {
		return failureRuntime.FailAuthoritative(err)
	}
	if runtime != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		shutdownErr := runtime.Shutdown(shutdownCtx)
		cancel()
		return shutdownErr
	}
	return nil
}

func (s *ragFairQueueSupervisor) Run(ctx context.Context) error {
	if s == nil || s.factory == nil {
		return errors.New("gateway: invalid RAG fair queue supervisor")
	}
	if ctx == nil {
		return errors.New("gateway: nil RAG fair queue supervisor context")
	}
	backoff := s.options.BackoffInitial
	for {
		if err := ctx.Err(); err != nil {
			s.setAttempt(nil, fairQueueStatusDegraded, nil)
			return nil
		}
		s.mu.RLock()
		terminalStatus, terminalErr := s.status, s.lastErr
		s.mu.RUnlock()
		if terminalStatus == fairQueueStatusFailed && terminalErr != nil && !ragFairQueueRetryable(terminalErr) {
			return terminalErr
		}
		buildCtx, cancel := context.WithTimeout(ctx, s.options.BuildTimeout)
		runtime, err := s.factory(buildCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if !ragFairQueueRetryable(err) {
				s.setAttempt(nil, fairQueueStatusFailed, err)
				return err
			}
			s.setAttempt(nil, fairQueueStatusDegraded, err)
			if err := s.options.Wait(ctx, backoff); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
			backoff = growRAGFairQueueBackoff(backoff, s.options.BackoffMax)
			continue
		}
		if ctx.Err() != nil {
			_ = shutdownRAGFairQueueRuntime(runtime)
			return nil
		}

		// Runtime recovery still owns the startup gate. Binding here only enables
		// the notifier to ask that gate; it cannot open it or select legacy work.
		if terminalErr := s.bindRuntime(runtime); terminalErr != nil {
			_ = rejectRAGFairQueueRuntime(runtime, terminalErr)
			return terminalErr
		}
		err = runtime.Run(ctx)
		shutdownErr := shutdownRAGFairQueueRuntime(runtime)
		s.clearRuntime(runtime, err)
		if shutdownErr != nil {
			terminalErr := errors.Join(err, shutdownErr)
			s.setAttempt(nil, fairQueueStatusFailed, terminalErr)
			return terminalErr
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return nil
		}
		if !ragFairQueueRetryable(err) {
			s.setAttempt(nil, fairQueueStatusFailed, err)
			return err
		}
		if err := s.options.Wait(ctx, backoff); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		backoff = growRAGFairQueueBackoff(backoff, s.options.BackoffMax)
	}
}

func (s *ragFairQueueSupervisor) TryDispatch(ctx context.Context, resource, taskID string) (bool, error) {
	if s == nil || ctx == nil {
		return false, errors.Join(fairqueue.ErrResourceNotReady, errors.New("gateway: fair queue runtime is unavailable"))
	}
	s.mu.RLock()
	runtime := s.runtime
	status := s.status
	s.mu.RUnlock()
	if runtime == nil || status == fairQueueStatusFailed {
		return false, errors.Join(fairqueue.ErrResourceNotReady, fairqueue.ErrDependencyUnavailable)
	}
	return runtime.TryDispatch(ctx, resource, taskID)
}

func (s *ragFairQueueSupervisor) RuntimeHealthSnapshot(resource string) (fairqueue.HealthSnapshot, bool) {
	if s == nil {
		return fairqueue.HealthSnapshot{}, false
	}
	s.mu.RLock()
	runtime := s.runtime
	s.mu.RUnlock()
	provider, ok := runtime.(ragFairQueueRuntimeHealth)
	if !ok || provider == nil {
		return fairqueue.HealthSnapshot{}, false
	}
	snapshot, err := provider.HealthSnapshot(resource)
	return snapshot, err == nil
}

func ragFairQueueRetryable(err error) bool {
	return errors.Is(err, fairqueue.ErrDependencyUnavailable) ||
		errors.Is(err, context.DeadlineExceeded)
}

func growRAGFairQueueBackoff(value, maximum time.Duration) time.Duration {
	if value >= maximum || value > maximum/2 {
		return maximum
	}
	return value * 2
}

func waitRAGFairQueueRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *ragFairQueueSupervisor) String() string {
	if s == nil {
		return "rag-fair-queue(nil)"
	}
	return fmt.Sprintf("rag-fair-queue(%s)", s.Status())
}
