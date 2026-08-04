package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/store"
)

type fairQueueAdminRunner interface {
	Contract(context.Context, bool, bool) (store.RAGFairQueueContractReport, error)
	RabbitRepair(context.Context, string, bool, fairqueue.RabbitRepairAttestation) (fairqueue.RabbitRepairReport, error)
	WriterRebind(context.Context, string, string, bool, fairqueue.WriterRebindAttestation) (fairqueue.WriterRebindReport, error)
	RedisForceRebuild(context.Context, string, bool, fairqueue.ForceRebuildAttestation) (fairqueue.ForceRebuildReport, error)
}

func adminFairQueueCmd() *cobra.Command {
	return adminFairQueueCmdWithRunner(productionFairQueueAdminRunner{})
}

func adminFairQueueCmdWithRunner(runner fairQueueAdminRunner) *cobra.Command {
	cmd := &cobra.Command{Use: "fairqueue", Short: "Inspect or repair durable fair scheduling state"}
	cmd.AddCommand(
		adminFairQueueContractCmd(runner),
		adminFairQueueRabbitRepairCmd(runner),
		adminFairQueueWriterRebindCmd(runner),
		adminFairQueueRedisRebuildCmd(runner),
	)
	return cmd
}

func adminFairQueueContractCmd(runner fairQueueAdminRunner) *cobra.Command {
	var apply, confirmed bool
	cmd := &cobra.Command{
		Use: "contract-migrate", Short: "Check or apply the RAG fairqueue schema contract", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runner == nil {
				return errors.New("fairqueue admin runner is unavailable")
			}
			if apply && !confirmed {
				return errors.New("--apply requires --confirm-all-writers-dual-write")
			}
			report, err := runner.Contract(cmd.Context(), apply, confirmed)
			if err != nil {
				return err
			}
			cmd.Printf("mode=%s contracted=%t expand_ready=%t user_id_nullable=%t tasks=%d remaining=%d pages=%d changed=%d owner_violations=%d generation_violations=%d\n",
				fairQueueCommandMode(apply), report.Contracted, report.ExpandSchemaReady, report.UserIDNullable,
				report.TaskCount, report.RemainingCount, report.PagesScanned, report.RowsChanged,
				report.MissingUserIDCount+report.UnresolvedOwnerCount+report.OwnerMismatchCount,
				report.NonPositiveGenerationCount+report.ExhaustedGenerationCount+report.PendingGenerationMismatchCount+report.RunningGenerationMismatchCount,
			)
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the contract (default is read-only dry-run)")
	cmd.Flags().BoolVar(&confirmed, "confirm-all-writers-dual-write", false, "attest that every older writer is stopped and all current writers dual-write")
	return cmd
}

func adminFairQueueRabbitRepairCmd(runner fairQueueAdminRunner) *cobra.Command {
	var resource string
	var apply, oldBrokerIsolated, publishersPaused bool
	cmd := &cobra.Command{
		Use: "rabbit-disaster-repair", Short: "Rearm canonical work after confirmed RabbitMQ data loss", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runner == nil {
				return errors.New("fairqueue admin runner is unavailable")
			}
			if err := validateFairQueueCLIResource(resource); err != nil {
				return err
			}
			attestation := fairqueue.RabbitRepairAttestation{OldBrokerIsolated: oldBrokerIsolated, PublishersPaused: publishersPaused}
			if apply {
				if err := attestation.Validate(); err != nil {
					return err
				}
			}
			report, err := runner.RabbitRepair(cmd.Context(), resource, apply, attestation)
			if err != nil {
				return err
			}
			cmd.Printf("mode=%s resource=%s candidates=%d pages=%d operation_present=%t operation_kind=%s operation_phase=%s\n",
				fairQueueCommandMode(apply), report.Resource, report.CandidateCount, report.PagesScanned,
				report.Operation.Present, report.Operation.Kind, report.Operation.Phase)
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "registered resource (required)")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply repair (default is read-only dry-run)")
	cmd.Flags().BoolVar(&oldBrokerIsolated, "confirm-old-broker-isolated", false, "attest that the old broker cannot deliver late messages")
	cmd.Flags().BoolVar(&publishersPaused, "confirm-publishers-paused", false, "attest that publishers remain paused during repair")
	return cmd
}

func adminFairQueueWriterRebindCmd(runner fairQueueAdminRunner) *cobra.Command {
	var resource, expectedOld string
	var apply, oldFenced, runtimesStopped, newAuthoritative bool
	cmd := &cobra.Command{
		Use: "rebind-writer", Short: "Rebind one stopped resource to a new authoritative MySQL writer", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runner == nil {
				return errors.New("fairqueue admin runner is unavailable")
			}
			if err := validateFairQueueCLIResource(resource); err != nil {
				return err
			}
			if len(expectedOld) != 64 || strings.Trim(expectedOld, "0123456789abcdef") != "" {
				return errors.New("--expected-old-writer-fingerprint must be 64 lowercase hex characters")
			}
			attestation := fairqueue.WriterRebindAttestation{OldWriterFenced: oldFenced, ResourceRuntimesStopped: runtimesStopped, NewWriterAuthoritative: newAuthoritative}
			if apply {
				if err := attestation.Validate(); err != nil {
					return err
				}
			}
			report, err := runner.WriterRebind(cmd.Context(), resource, expectedOld, apply, attestation)
			if err != nil {
				return err
			}
			cmd.Printf("mode=%s resource=%s target_ready=%t valid_running=%d operation_present=%t operation_kind=%s operation_phase=%s\n",
				fairQueueCommandMode(apply), report.Resource, report.Readiness.Ready(), report.ValidRunningCount,
				report.Operation.Present, report.Operation.Kind, report.Operation.Phase)
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "registered resource (required)")
	cmd.Flags().StringVar(&expectedOld, "expected-old-writer-fingerprint", "", "expected previous database-bound writer fingerprint (required)")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply rebind (default is read-only dry-run)")
	cmd.Flags().BoolVar(&oldFenced, "confirm-old-writer-fenced", false, "attest that the previous writer cannot accept writes")
	cmd.Flags().BoolVar(&runtimesStopped, "confirm-resource-runtimes-stopped", false, "attest that all resource runtimes/recovery coordinators remain stopped")
	cmd.Flags().BoolVar(&newAuthoritative, "confirm-new-writer-authoritative", false, "attest that the new writer is authoritative")
	return cmd
}

func adminFairQueueRedisRebuildCmd(runner fairQueueAdminRunner) *cobra.Command {
	var resource string
	var apply, discard bool
	cmd := &cobra.Command{
		Use: "redis-force-rebuild", Short: "Force a fenced rebuild of corrupt Redis coordination state", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if runner == nil {
				return errors.New("fairqueue admin runner is unavailable")
			}
			if err := validateFairQueueCLIResource(resource); err != nil {
				return err
			}
			attestation := fairqueue.ForceRebuildAttestation{DiscardRedisCoordinationState: discard}
			if apply {
				if err := attestation.Validate(); err != nil {
					return err
				}
			}
			report, err := runner.RedisForceRebuild(cmd.Context(), resource, apply, attestation)
			if err != nil {
				return err
			}
			cmd.Printf("mode=%s resource=%s redis_standalone=%t writer_verified=%t rabbit_verified=%t rebuildable=%d pages=%d operation_present=%t operation_kind=%s operation_phase=%s\n",
				fairQueueCommandMode(apply), report.Resource, report.StandaloneRedis, report.CurrentWriterVerified,
				report.RabbitTruthSourceVerified, report.RebuildableKeyCount, report.PagesScanned,
				report.Operation.Present, report.Operation.Kind, report.Operation.Phase)
			return nil
		},
	}
	cmd.Flags().StringVar(&resource, "resource", "", "registered resource (required)")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply rebuild (default is read-only dry-run)")
	cmd.Flags().BoolVar(&discard, "confirm-discard-redis-coordination-state", false, "attest that this resource's Redis coordination state may be discarded")
	return cmd
}

func validateFairQueueCLIResource(resource string) error {
	if resource == "" {
		return errors.New("--resource is required")
	}
	if resource != rag.RAGFairQueueResource {
		return fmt.Errorf("unknown fairqueue resource %q", resource)
	}
	return nil
}

func fairQueueCommandMode(apply bool) string {
	if apply {
		return "apply"
	}
	return "dry-run"
}

type productionFairQueueAdminRunner struct{}

func (productionFairQueueAdminRunner) Contract(ctx context.Context, apply, confirmed bool) (store.RAGFairQueueContractReport, error) {
	env := config.LoadEnv()
	admin, err := store.OpenFairQueueAdminStore(store.StorageConfig{Type: store.StorageMySQL, DSN: env.Storage.DSN, AutoMigrate: false})
	if err != nil {
		return store.RAGFairQueueContractReport{}, err
	}
	defer admin.Close()
	if !apply {
		return admin.CheckRAGFairQueueContract(ctx)
	}
	return admin.ApplyRAGFairQueueContract(ctx, store.RAGFairQueueContractAttestation{AllWritersDualWrite: confirmed})
}

type fairQueueAdminDependencies struct {
	config       fairqueue.ResourceConfig
	writer       string
	adapter      *rag.RAGFairQueueAdapter
	recovery     *fairqueue.RecoveryCoordinator
	operators    *fairqueue.RecoveryOperators
	rabbitRepair *fairqueue.RabbitDisasterRepair
	redis        *fairqueue.Redis
	rabbit       *fairqueue.Rabbit
	admin        *store.FairQueueAdminStore
	db           *store.DBStore
}

func (d *fairQueueAdminDependencies) Close() {
	if d == nil {
		return
	}
	if d.rabbit != nil {
		_ = d.rabbit.Close()
	}
	if d.redis != nil {
		_ = d.redis.Close()
	}
	if d.admin != nil {
		_ = d.admin.Close()
	}
	if d.db != nil {
		_ = d.db.Close()
	}
}

type adminNoopRAGRunner struct{}

func (adminNoopRAGRunner) RunFairClaim(context.Context, *store.RAGIndexClaim) error {
	return errors.New("admin fairqueue runner cannot execute business tasks")
}

type adminStoppedRecoveryRuntime struct{}

func (adminStoppedRecoveryRuntime) CloseResource(string) error                         { return nil }
func (adminStoppedRecoveryRuntime) WaitForAttemptDrain(context.Context, string) error  { return nil }
func (adminStoppedRecoveryRuntime) OpenResource(string, fairqueue.ResourceFence) error { return nil }

type adminCurrentWriterVerifier struct{ adapter *rag.RAGFairQueueAdapter }

func (v adminCurrentWriterVerifier) VerifyCurrentWriter(ctx context.Context, resource string) (fairqueue.WriterIdentity, bool, error) {
	if resource != rag.RAGFairQueueResource || v.adapter == nil {
		return fairqueue.WriterIdentity{}, false, fairqueue.ErrInvalidModel
	}
	identity, err := v.adapter.ReadWriterIdentity(ctx)
	return identity, err == nil, err
}

type adminRabbitTruthVerifier struct{ rabbit *fairqueue.Rabbit }

func (v adminRabbitTruthVerifier) VerifyRabbitTruthSource(ctx context.Context, resource string) (bool, error) {
	if v.rabbit == nil {
		return false, fairqueue.ErrDependencyUnavailable
	}
	_, err := v.rabbit.ProbeResourceTopology(ctx, resource)
	return err == nil, err
}

func openFairQueueAdminDependencies(ctx context.Context, resource string) (*fairQueueAdminDependencies, error) {
	if err := validateFairQueueCLIResource(resource); err != nil {
		return nil, err
	}
	env := config.LoadEnv()
	db, err := store.NewDBStore("mysql", env.Storage.DSN)
	if err != nil {
		return nil, err
	}
	d := &fairQueueAdminDependencies{db: db}
	initialized := false
	defer func() {
		if !initialized {
			d.Close()
		}
	}()
	identity, err := db.ReadFairQueueWriterIdentity(ctx)
	if err != nil {
		return nil, err
	}
	d.writer = identity.Fingerprint
	live, err := db.BindRAGFairQueueWriter(d.writer)
	if err != nil {
		return nil, err
	}
	d.admin, err = store.OpenFairQueueAdminStore(store.StorageConfig{Type: store.StorageMySQL, DSN: env.Storage.DSN, AutoMigrate: false})
	if err != nil {
		return nil, err
	}
	if d.admin.WriterFingerprint() != d.writer {
		return nil, fairqueue.ErrAuthoritativeWriterMismatch
	}
	adminSource, err := d.admin.BindRAGFairQueueSource(d.writer)
	if err != nil {
		return nil, err
	}
	journalStore, err := rag.NewRAGFairQueueStoreJournal(adminSource)
	if err != nil {
		return nil, err
	}
	d.config = fairQueueCLIResourceConfig(env.FairQueue.RAGIndex)
	d.adapter, err = rag.NewRAGFairQueueAdapter(live, adminNoopRAGRunner{}, adminSource, journalStore, rag.RAGFairQueueAdapterOptions{
		WorkerID: "rag-fair-admin", LeaseDuration: d.config.ReservationTTL,
		ClaimLimits: store.RAGFairQueueClaimLimits{GlobalConcurrency: d.config.GlobalConcurrency, PerUserBurstConcurrency: d.config.PerUserBurstConcurrency, AdvisoryLockTimeout: time.Second, MaintenanceRetryDelay: d.config.DispatchInterval},
	})
	if err != nil {
		return nil, err
	}
	registry, err := fairqueue.NewRegistry(d.config)
	if err != nil {
		return nil, err
	}
	d.redis, err = fairqueue.NewRedis(ctx, fairqueue.RedisOptions{Addr: env.FairQueue.RedisAddr, Password: env.FairQueue.RedisPassword, DB: env.FairQueue.RedisDB, KeyPrefix: env.FairQueue.KeyPrefix, OperationTimeout: d.config.PublishAttemptTimeout})
	if err != nil {
		return nil, err
	}
	d.rabbit, err = fairqueue.NewRabbit(fairqueue.RabbitOptions{URL: env.FairQueue.RabbitMQURL, Exchange: env.FairQueue.Exchange, DeadLetterExchange: env.FairQueue.DeadLetterExchange, OperationTimeout: d.config.PublishAttemptTimeout}, registry)
	if err != nil {
		return nil, err
	}
	d.recovery, err = fairqueue.NewRecoveryCoordinator(d.redis, d.rabbit, adminStoppedRecoveryRuntime{}, fairqueue.RecoveryOptions{})
	if err != nil {
		return nil, err
	}
	d.operators, err = fairqueue.NewRecoveryOperators(d.redis, d.recovery, d.adapter.OperationJournal(), d.redis,
		adminCurrentWriterVerifier{d.adapter}, adminRabbitTruthVerifier{d.rabbit}, fairqueue.RecoveryOperatorOptions{
			ResourceConfig: d.config, RecoveryLockTTL: 30 * time.Second, RecoveryLockRenewInterval: 10 * time.Second, ForceRebuildMinimumDelay: d.config.RecoveryDrainTimeout,
		})
	if err != nil {
		return nil, err
	}
	d.rabbitRepair, err = fairqueue.NewRabbitDisasterRepair(d.config, fairqueue.WriterIdentity{Fingerprint: d.writer}, d.adapter, d.adapter, d.adapter.OperationJournal(), d.redis, d.recovery, fairqueue.RabbitDisasterRepairOptions{})
	if err != nil {
		return nil, err
	}
	initialized = true
	return d, nil
}

func fairQueueCLIResourceConfig(c config.FairQueueResourceCfg) fairqueue.ResourceConfig {
	return fairqueue.ResourceConfig{
		Key: rag.RAGFairQueueResource, ValidateTaskID: fairqueue.ValidateRAGIndexTaskID,
		LocalWorkers: c.LocalWorkers, GlobalConcurrency: c.GlobalConcurrency,
		PerUserBaseConcurrency: c.PerUserBaseConcurrency, PerUserBurstConcurrency: c.PerUserBurstConcurrency,
		BorrowEnabled: c.BorrowEnabled, ReconcileInterval: c.ReconcileInterval,
		ExpiredRunningSweepInterval: c.ExpiredRunningSweepInterval, ReconcilePageSize: c.ReconcilePageSize,
		ReservationTTL: c.ReservationTTL, ReservationHeartbeat: c.ReservationHeartbeat,
		PrepareTimeout: c.PrepareTimeout, ProvisionalTTL: c.ProvisionalTTL, ProcessingTurnTTL: c.ProcessingTurnTTL,
		RecoveryDrainTimeout: c.RecoveryDrainTimeout, DispatchInterval: c.DispatchInterval, PublishAttemptTimeout: c.PublishAttemptTimeout,
	}
}

func (productionFairQueueAdminRunner) RabbitRepair(ctx context.Context, resource string, apply bool, attestation fairqueue.RabbitRepairAttestation) (fairqueue.RabbitRepairReport, error) {
	d, err := openFairQueueAdminDependencies(ctx, resource)
	if err != nil {
		return fairqueue.RabbitRepairReport{}, err
	}
	defer d.Close()
	report, err := d.rabbitRepair.Check(ctx)
	if err != nil || !apply {
		return report, err
	}
	if err := d.rabbitRepair.Apply(ctx, attestation); err != nil {
		return fairqueue.RabbitRepairReport{}, err
	}
	return d.rabbitRepair.Check(ctx)
}

func (productionFairQueueAdminRunner) WriterRebind(ctx context.Context, resource, expectedOld string, apply bool, attestation fairqueue.WriterRebindAttestation) (fairqueue.WriterRebindReport, error) {
	d, err := openFairQueueAdminDependencies(ctx, resource)
	if err != nil {
		return fairqueue.WriterRebindReport{}, err
	}
	defer d.Close()
	report, err := d.operators.CheckWriterRebind(ctx, resource, expectedOld, d.adapter)
	if err != nil || !apply {
		return report, err
	}
	if err := d.operators.ApplyWriterRebind(ctx, resource, expectedOld, attestation, d.adapter); err != nil {
		return fairqueue.WriterRebindReport{}, err
	}
	return d.operators.CheckWriterRebind(ctx, resource, expectedOld, d.adapter)
}

func (productionFairQueueAdminRunner) RedisForceRebuild(ctx context.Context, resource string, apply bool, attestation fairqueue.ForceRebuildAttestation) (fairqueue.ForceRebuildReport, error) {
	d, err := openFairQueueAdminDependencies(ctx, resource)
	if err != nil {
		return fairqueue.ForceRebuildReport{}, err
	}
	defer d.Close()
	report, err := d.operators.CheckRedisForceRebuild(ctx, resource, d.adapter)
	if err != nil || !apply {
		return report, err
	}
	if err := d.operators.ApplyRedisForceRebuild(ctx, resource, attestation, d.adapter); err != nil {
		return fairqueue.ForceRebuildReport{}, err
	}
	return d.operators.CheckRedisForceRebuild(ctx, resource, d.adapter)
}
