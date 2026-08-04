package gateway

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	redis "github.com/redis/go-redis/v9"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	imagegendomain "github.com/qs3c/bkcrab/internal/imagegen"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type imageFairQueueAssembly struct {
	mainStore      *store.DBStore
	fairStore      *store.ImageFairQueueStore
	adminStore     *store.FairQueueAdminStore
	supervisor     *ragFairQueueSupervisor
	batchService   *imagegendomain.BatchService
	limiterClient  *redis.Client
	resourceConfig fairqueue.ResourceConfig
	writer         string
	health         *ragFairQueueHealthState
	closeOnce      sync.Once
	closeErr       error
}

func imageBatchRuntimeEnabled(mode config.ImagegenBatchMode) bool {
	return mode == config.ImagegenBatchModeFair || mode == config.ImagegenBatchModeDrain
}

type imageBatchDispatchTarget interface {
	TryDispatch(context.Context, string, string) (bool, error)
}

type imageBatchNotifier struct {
	store interface {
		ListImageGenerationTasks(context.Context, string) ([]store.ImageGenerationTaskRecord, error)
	}
	target imageBatchDispatchTarget
}

func (n imageBatchNotifier) TryDispatch(ctx context.Context, batchID string) error {
	if n.store == nil || n.target == nil {
		return fairqueue.ErrDependencyUnavailable
	}
	tasks, err := n.store.ListImageGenerationTasks(ctx, batchID)
	if err != nil {
		return err
	}
	var dispatchErr error
	for _, task := range tasks {
		if _, err := n.target.TryDispatch(ctx, store.ImageGenerationResource, task.ID); err != nil {
			dispatchErr = errors.Join(dispatchErr, err)
		}
	}
	return dispatchErr
}

func buildImageFairQueueAssembly(ctx context.Context, env *config.EnvConfig, st store.Store, ws workspace.Store, resolver imagegendomain.ProviderPlanResolver) (*imageFairQueueAssembly, error) {
	if env == nil || resolver == nil || ws == nil {
		return nil, errors.New("gateway: image fair queue dependencies are required")
	}
	dbStore, ok := st.(*store.DBStore)
	if !ok || dbStore.Dialect() != "mysql" {
		return nil, errors.New("gateway: image fair queue requires authoritative MySQL store")
	}
	identity, err := dbStore.ReadFairQueueWriterIdentity(ctx)
	if err != nil {
		return nil, fmt.Errorf("gateway: read image fair queue writer identity: %w", err)
	}
	fairStore, err := dbStore.BindImageFairQueueWriter(identity.Fingerprint)
	if err != nil {
		return nil, fmt.Errorf("gateway: bind image fair queue writer: %w", err)
	}
	if err := fairStore.CheckImageGenerationSchema(ctx); err != nil {
		return nil, fmt.Errorf("gateway: image generation schema is not ready: %w", err)
	}

	adminStore, err := store.OpenFairQueueAdminStore(store.StorageConfig{Type: store.StorageMySQL, DSN: env.Storage.DSN, AutoMigrate: false})
	if err != nil {
		return nil, fmt.Errorf("gateway: open image fair queue admin store: %w", err)
	}
	closeAdmin := true
	defer func() {
		if closeAdmin {
			_ = adminStore.Close()
		}
	}()
	if adminStore.WriterFingerprint() != identity.Fingerprint {
		return nil, errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, errors.New("gateway: image live and admin stores resolved different writers"))
	}
	adminSource, err := adminStore.BindRAGFairQueueSource(identity.Fingerprint)
	if err != nil {
		return nil, err
	}
	journalStore, err := rag.NewRAGFairQueueStoreJournal(adminSource)
	if err != nil {
		return nil, err
	}
	journal, err := rag.NewRAGFairQueueOperationJournal(journalStore)
	if err != nil {
		return nil, err
	}

	resourceConfig := imagegendomain.ImageFairQueueResourceConfig(env.ImagegenBatch)
	telemetry := fairqueue.NopTelemetrySink()
	limiterClient := redis.NewClient(&redis.Options{Addr: env.FairQueue.RedisAddr, Password: env.FairQueue.RedisPassword, DB: env.FairQueue.RedisDB})
	closeLimiter := true
	defer func() {
		if closeLimiter {
			_ = limiterClient.Close()
		}
	}()
	limiter, err := imagegendomain.NewRedisProviderLimiter(limiterClient, env.FairQueue.KeyPrefix)
	if err != nil {
		return nil, err
	}
	generation := imagegendomain.NewGenerationService(resolver, toolProviderRegistry, limiter, env.ImagegenBatch.ProviderCallTimeout).
		ConfigureProviderLimits(env.ImagegenBatch.ProviderConcurrencyDefault, env.ImagegenBatch.ProviderConcurrency, env.ImagegenBatch.ReservationTTL)
	artifacts, err := imagegendomain.NewArtifactPublisher(imagegendomain.ArtifactPublisherOptions{
		Store:  ws,
		Limits: imagegendomain.ArtifactLimits{MaxImageBytes: env.ImagegenBatch.ImageMaxBytes, MaxBatchBytes: env.ImagegenBatch.BatchMaxBytes},
	})
	if err != nil {
		return nil, err
	}
	adapter := imagegendomain.NewFairQueueAdapter(imagegendomain.FairQueueAdapterOptions{
		Store: fairStore, WorkerID: "image-fair-" + uuid.NewString(), TaskLease: env.ImagegenBatch.TaskLease,
		TaskHeartbeat:      env.ImagegenBatch.TaskHeartbeat,
		ExpiredLockTimeout: boundedRAGFairQueueAdvisoryTimeout(env.ImagegenBatch.PrepareTimeout),
		ClaimLimits: store.ImageGenerationClaimLimits{
			GlobalConcurrency:       env.ImagegenBatch.GlobalConcurrency,
			PerUserBurstConcurrency: env.ImagegenBatch.PerUserBurstConcurrency,
			AdvisoryLockTimeout:     boundedRAGFairQueueAdvisoryTimeout(env.ImagegenBatch.PrepareTimeout),
		},
		Generation: generation, Artifacts: artifacts, RetryDelay: env.ImagegenBatch.DispatchInterval,
	})
	registration := fairqueue.ResourceRegistration{
		Config: resourceConfig, Preparer: adapter, DispatchSource: adapter, ExpiredRearmSource: adapter,
		RecoverySource: adapter, WriterFingerprint: identity.Fingerprint,
	}
	registry, err := fairqueue.NewRegistry(resourceConfig)
	if err != nil {
		return nil, err
	}
	health := newRAGFairQueueHealthState(ragFairQueueHealthOptions{
		Enabled: true, Mode: rag.WorkerMode(env.ImagegenBatch.Mode), WriterTopology: env.FairQueue.MySQLWriterTopology,
		Writer: identity.Fingerprint, ConnectionSafety: dbStore.ReadFairQueueConnectionSafetySnapshot,
		LoopFreshness: makeRAGFairQueueLoopFreshness(resourceConfig),
	})
	if err := initializeFairQueueJournalHealth(ctx, journal, store.ImageGenerationResource, identity.Fingerprint, health); err != nil {
		return nil, err
	}
	builder := ragFairQueueRuntimeBuilder{
		newCoordinator: func(probeCtx context.Context) (fairqueue.RuntimeCoordinator, error) {
			coordinator, err := fairqueue.NewRedis(probeCtx, fairqueue.RedisOptions{
				Addr: env.FairQueue.RedisAddr, Password: env.FairQueue.RedisPassword, DB: env.FairQueue.RedisDB,
				KeyPrefix: env.FairQueue.KeyPrefix, OperationTimeout: resourceConfig.PublishAttemptTimeout, Telemetry: telemetry,
			})
			if err != nil {
				health.markRedisUnavailable(err)
				return nil, err
			}
			probe, err := coordinator.ProbeResourceHealth(probeCtx, store.ImageGenerationResource)
			if err != nil {
				health.markRedisUnavailable(err)
				_ = coordinator.Close()
				return nil, err
			}
			health.updateRedis(probe)
			return coordinator, nil
		},
		newRabbit: func() (ragFairQueueRabbit, error) {
			return fairqueue.NewRabbit(fairqueue.RabbitOptions{URL: env.FairQueue.RabbitMQURL, Exchange: env.FairQueue.Exchange,
				DeadLetterExchange: env.FairQueue.DeadLetterExchange, OperationTimeout: resourceConfig.PublishAttemptTimeout, Telemetry: telemetry}, registry)
		},
		newRuntime: func(rabbit fairqueue.RabbitClient, coordinator fairqueue.RuntimeCoordinator, operationJournal fairqueue.OperationJournal) (ragFairQueueRuntime, error) {
			runtime, err := fairqueue.NewRuntime(rabbit, coordinator, operationJournal, fairqueue.RuntimeOptions{ShutdownGrace: resourceConfig.RecoveryDrainTimeout, Telemetry: telemetry})
			if err != nil {
				return nil, err
			}
			rabbitHealth, rabbitOK := rabbit.(ragFairQueueRabbit)
			redisRuntime, redisOK := coordinator.(*fairqueue.Redis)
			if !rabbitOK || !redisOK {
				return nil, errors.New("gateway: image fair queue dependency lost concrete health boundary")
			}
			return &ragFairQueueManagedRuntime{Runtime: runtime, redis: redisRuntime, rabbit: rabbitHealth, journal: operationJournal,
				writer: identity.Fingerprint, health: health, probeTimeout: resourceConfig.PublishAttemptTimeout,
				probeInterval: boundedRAGFairQueueHealthInterval(resourceConfig.ReconcileInterval), resource: store.ImageGenerationResource}, nil
		},
		onRabbitHealth: health.updateRabbit, journal: journal, registration: registration,
	}
	supervisor := newRAGFairQueueSupervisor(builder.Build, ragFairQueueSupervisorOptions{BuildTimeout: resourceConfig.RecoveryDrainTimeout})
	notifier := imageBatchNotifier{store: fairStore, target: supervisor}
	batchService := imagegendomain.NewBatchService(imagegendomain.BatchServiceOptions{
		Store: fairStore, ProviderPlans: resolver, Dispatcher: notifier,
		MaxImagesPerBatch: env.ImagegenBatch.MaxImagesPerBatch, MaxImagesPerTask: env.ImagegenBatch.MaxImagesPerTask,
		MaxItems: 16, PromptMaxRunes: env.ImagegenBatch.PromptMaxRunes, WaitMaxSeconds: int(env.ImagegenBatch.ToolWaitMax.Seconds()),
		MaxRetries: env.ImagegenBatch.MaxRetries, PollInterval: env.ImagegenBatch.DispatchInterval,
	})
	closeAdmin, closeLimiter = false, false
	return &imageFairQueueAssembly{mainStore: dbStore, fairStore: fairStore, adminStore: adminStore, supervisor: supervisor,
		batchService: batchService, limiterClient: limiterClient, resourceConfig: resourceConfig, writer: identity.Fingerprint, health: health}, nil
}

func (a *imageFairQueueAssembly) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		if a.limiterClient != nil {
			a.closeErr = errors.Join(a.closeErr, a.limiterClient.Close())
		}
		if a.adminStore != nil {
			a.closeErr = errors.Join(a.closeErr, a.adminStore.Close())
		}
	})
	return a.closeErr
}

func (a *imageFairQueueAssembly) InstallSafetyObserver(other *ragFairQueueAssembly) {
	if a == nil || a.mainStore == nil {
		return
	}
	a.mainStore.SetFairQueueSafetyFailureObserver(func() {
		failure := errors.Join(fairqueue.ErrAuthoritativeWriterMismatch, store.ErrFairQueueWriterMismatch)
		if a.health != nil {
			a.health.failAuthoritativeWriter()
		}
		if a.supervisor != nil {
			_ = a.supervisor.FailAuthoritative(failure)
		}
		if other != nil {
			if other.health != nil {
				other.health.failAuthoritativeWriter()
			}
			if other.supervisor != nil {
				_ = other.supervisor.FailAuthoritative(failure)
			}
		}
	})
}

var _ imagegendomain.DispatchNotifier = imageBatchNotifier{}
