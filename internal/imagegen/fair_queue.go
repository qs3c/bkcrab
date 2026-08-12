package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

const ImageGenerationTaskType = "image_generation"

var imageFairTaskIDPattern = regexp.MustCompile(`^imgt_[a-z0-9]{16,64}$`)

func ImageFairQueueResourceConfig(cfg config.ImagegenBatchCfg) fairqueue.ResourceConfig {
	return fairqueue.ResourceConfig{
		Key: store.ImageGenerationResource, ValidateTaskID: imageFairTaskIDPattern.MatchString,
		LocalWorkers: cfg.LocalWorkers, GlobalConcurrency: cfg.GlobalConcurrency,
		PerUserBaseConcurrency: cfg.PerUserBaseConcurrency, PerUserBurstConcurrency: cfg.PerUserBurstConcurrency,
		BorrowEnabled: cfg.BorrowEnabled, ReconcileInterval: cfg.ReconcileInterval,
		ExpiredRunningSweepInterval: cfg.ExpiredSweepInterval, ReconcilePageSize: cfg.ReconcilePageSize,
		ReservationTTL: cfg.ReservationTTL, ReservationHeartbeat: cfg.ReservationHeartbeat,
		PrepareTimeout: cfg.PrepareTimeout, ProvisionalTTL: cfg.ProvisionalTTL,
		ProcessingTurnTTL: cfg.ProcessingTurnTTL, RecoveryDrainTimeout: cfg.RecoveryDrainTimeout,
		DispatchInterval: cfg.DispatchInterval, PublishAttemptTimeout: cfg.PublishAttemptTimeout,
	}
}

type ImageFairQueueStore interface {
	ListDispatchableImageTasksPage(context.Context, int64, int) ([]store.ImageTaskDispatchCandidate, int64, error)
	GetDispatchableImageTaskByID(context.Context, string) (*store.ImageTaskDispatchCandidate, error)
	MarkImageTaskDispatched(context.Context, store.ImageTaskDispatchCandidate, int64) (bool, error)
	SweepExpiredImageGenerationTasks(context.Context, int64, int, time.Duration) ([]store.ImageTaskDispatchCandidate, int64, error)
	ClaimImageGenerationTaskByID(context.Context, string, string, int64, string, time.Duration, store.ImageGenerationClaimLimits) (store.ImageGenerationClaimResult, error)
	RepairPoisonImageCandidate(context.Context, store.ImagePoisonRepairLocator, string, string) (*store.ImageTaskDispatchCandidate, store.ImagePoisonRepairDisposition, error)
	HeartbeatImageGenerationTask(context.Context, store.ImageGenerationFence, time.Duration) (store.ImageGenerationHeartbeatDisposition, error)
	FinishImageGenerationTaskDone(context.Context, store.ImageGenerationFence, store.ImageTaskDoneResult) (*store.ImageGenerationBatchRecord, bool, error)
	FinishImageGenerationTaskRetry(context.Context, store.ImageGenerationFence, string, time.Time) (bool, error)
	FinishImageGenerationTaskFailed(context.Context, store.ImageGenerationFence, string) (bool, error)
	FinishImageGenerationTaskCanceled(context.Context, store.ImageGenerationFence) (bool, error)
}

type ImageGenerationRunner interface {
	Generate(context.Context, ExecutionIdentity, SafeProviderPlan, GenerateRequest) (ProviderResult, error)
}

type ImageArtifactRuntime interface {
	Salvage(context.Context, ArtifactSalvageRequest) (ArtifactManifest, bool, error)
	Publish(context.Context, ArtifactPublishRequest) (ArtifactManifest, error)
	DeleteClaimArtifacts(context.Context, ArtifactManifest) error
}

type ImageFairQueueRecoveryStore interface {
	CaptureImageFairQueueHighWater(context.Context) (int64, error)
	ListCanonicalImageTenants(context.Context, int64, string, int) ([]string, string, error)
	ListDispatchedImageTasks(context.Context, int64, int64, int) ([]store.ImageTaskDispatchRecord, int64, error)
	ListValidRunningImageTasks(context.Context, int64, int64, int) ([]store.ImageRunningTaskSnapshot, int64, error)
	CaptureImageBrokerRepairHighWater(context.Context) (int64, error)
	ListBrokerBackedImageCandidates(context.Context, int64, int64, int) ([]store.ImageTaskDispatchCandidate, int64, error)
	RearmImageCandidateAfterBrokerLoss(context.Context, store.ImageTaskDispatchCandidate) (*store.ImageTaskDispatchCandidate, bool, error)
}

type ImageFairQueueWriterStore interface {
	ReadImageFairQueueWriterIdentity(context.Context) (string, error)
	CheckImageGenerationSchema(context.Context) error
	ImageGenerationInvariantCounts(context.Context) (int64, int64, error)
	CountValidRunningImageGenerationTasks(context.Context) (int64, error)
}

type FairQueueAdapterOptions struct {
	Store              ImageFairQueueStore
	Recovery           ImageFairQueueRecoveryStore
	WorkerID           string
	TaskLease          time.Duration
	TaskHeartbeat      time.Duration
	ExpiredLockTimeout time.Duration
	ClaimLimits        store.ImageGenerationClaimLimits
	Generation         ImageGenerationRunner
	Artifacts          ImageArtifactRuntime
	RetryDelay         time.Duration
}

type FairQueueAdapter struct {
	options FairQueueAdapterOptions
}

var (
	_ fairqueue.DispatchSource     = (*FairQueueAdapter)(nil)
	_ fairqueue.ExpiredRearmSource = (*FairQueueAdapter)(nil)
	_ fairqueue.TaskPreparer       = (*FairQueueAdapter)(nil)
	_ fairqueue.RecoverySource     = (*FairQueueAdapter)(nil)
	_ fairqueue.BrokerRepairSource = (*FairQueueAdapter)(nil)
	_ fairqueue.WriterRebindSource = (*FairQueueAdapter)(nil)
	_ ImageFairQueueStore          = (*store.ImageFairQueueStore)(nil)
	_ ImageFairQueueRecoveryStore  = (*store.ImageFairQueueStore)(nil)
	_ ImageFairQueueWriterStore    = (*store.ImageFairQueueStore)(nil)
)

func (a *FairQueueAdapter) writerStore() (ImageFairQueueWriterStore, error) {
	writer, ok := a.options.Store.(ImageFairQueueWriterStore)
	if !ok || writer == nil {
		return nil, fairqueue.ErrDependencyUnavailable
	}
	return writer, nil
}

func (a *FairQueueAdapter) ReadWriterIdentity(ctx context.Context) (fairqueue.WriterIdentity, error) {
	writer, err := a.writerStore()
	if err != nil {
		return fairqueue.WriterIdentity{}, err
	}
	fingerprint, err := writer.ReadImageFairQueueWriterIdentity(ctx)
	if err != nil {
		return fairqueue.WriterIdentity{}, err
	}
	identity := fairqueue.WriterIdentity{Fingerprint: fingerprint}
	if err := identity.Validate(); err != nil {
		return fairqueue.WriterIdentity{}, fairqueue.ErrAuthoritativeWriterMismatch
	}
	return identity, nil
}

func (a *FairQueueAdapter) CheckSchemaAndInvariants(ctx context.Context) (fairqueue.WriterReadinessReport, error) {
	writer, err := a.writerStore()
	if err != nil {
		return fairqueue.WriterReadinessReport{}, err
	}
	identity, err := a.ReadWriterIdentity(ctx)
	if err != nil {
		return fairqueue.WriterReadinessReport{}, err
	}
	if err := writer.CheckImageGenerationSchema(ctx); err != nil {
		return fairqueue.WriterReadinessReport{}, err
	}
	owner, generation, err := writer.ImageGenerationInvariantCounts(ctx)
	if err != nil || owner < 0 || generation < 0 {
		return fairqueue.WriterReadinessReport{}, fairqueue.ErrAuthoritativeStateCorrupt
	}
	confirm, err := a.ReadWriterIdentity(ctx)
	if err != nil || confirm != identity {
		return fairqueue.WriterReadinessReport{}, fairqueue.ErrAuthoritativeWriterMismatch
	}
	return fairqueue.WriterReadinessReport{Writer: identity, SchemaReady: true, OwnerInvariantViolationCount: owner, GenerationViolationCount: generation}, nil
}

func (a *FairQueueAdapter) CountValidRunning(ctx context.Context) (int64, error) {
	writer, err := a.writerStore()
	if err != nil {
		return 0, err
	}
	count, err := writer.CountValidRunningImageGenerationTasks(ctx)
	if err != nil || count < 0 {
		if err != nil {
			return 0, err
		}
		return 0, fairqueue.ErrAuthoritativeStateCorrupt
	}
	return count, nil
}

func NewFairQueueAdapter(options FairQueueAdapterOptions) *FairQueueAdapter {
	if options.Recovery == nil {
		if recovery, ok := options.Store.(ImageFairQueueRecoveryStore); ok {
			options.Recovery = recovery
		}
	}
	if options.TaskHeartbeat <= 0 {
		options.TaskHeartbeat = options.TaskLease / 3
	}
	if options.ExpiredLockTimeout <= 0 {
		options.ExpiredLockTimeout = 5 * time.Second
	}
	if options.RetryDelay <= 0 {
		options.RetryDelay = 5 * time.Second
	}
	return &FairQueueAdapter{options: options}
}

func recoveryCursor(value int64) string { return strconv.FormatInt(value, 10) }

func recoveryPageCursor(next int64, count, limit int) (string, bool) {
	if count < limit {
		return "", true
	}
	return recoveryCursor(next), false
}

func (a *FairQueueAdapter) CaptureHighWater(ctx context.Context) (string, error) {
	if a == nil || a.options.Recovery == nil {
		return "", fairqueue.ErrDependencyUnavailable
	}
	highWater, err := a.options.Recovery.CaptureImageFairQueueHighWater(ctx)
	if err != nil {
		return "", err
	}
	return recoveryCursor(highWater), nil
}

func (a *FairQueueAdapter) ListKnownTenants(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.TenantRef], error) {
	high, err := parseImageCursor(highWater)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, err
	}
	tenants, next, err := a.options.Recovery.ListCanonicalImageTenants(ctx, high, after, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.TenantRef]{}, err
	}
	items := make([]fairqueue.TenantRef, len(tenants))
	for i, tenant := range tenants {
		items[i] = fairqueue.TenantRef{TenantID: tenant}
	}
	done := len(items) < limit
	if done {
		next = ""
	}
	return fairqueue.RecoveryPage[fairqueue.TenantRef]{Items: items, NextCursor: next, Done: done}, nil
}

func (a *FairQueueAdapter) ListDispatched(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.DispatchedRef], error) {
	high, err := parseImageCursor(highWater)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, err
	}
	cursor, err := parseImageCursor(after)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, err
	}
	tasks, next, err := a.options.Recovery.ListDispatchedImageTasks(ctx, high, cursor, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{}, err
	}
	items := make([]fairqueue.DispatchedRef, len(tasks))
	for i, task := range tasks {
		items[i] = fairqueue.DispatchedRef{TenantID: task.UserID, Token: fairqueue.DispatchToken{Resource: store.ImageGenerationResource, TaskID: task.ID, Generation: uint64(task.DispatchGeneration)}}
	}
	nextCursor, done := recoveryPageCursor(next, len(items), limit)
	return fairqueue.RecoveryPage[fairqueue.DispatchedRef]{Items: items, NextCursor: nextCursor, Done: done}, nil
}

func (a *FairQueueAdapter) ListValidRunning(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.RunningLease], error) {
	high, err := parseImageCursor(highWater)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, err
	}
	cursor, err := parseImageCursor(after)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, err
	}
	snapshots, next, err := a.options.Recovery.ListValidRunningImageTasks(ctx, high, cursor, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, err
	}
	items := make([]fairqueue.RunningLease, len(snapshots))
	for i, snapshot := range snapshots {
		if snapshot.Task.LeaseUntil == nil {
			return fairqueue.RecoveryPage[fairqueue.RunningLease]{}, fairqueue.ErrAuthoritativeStateCorrupt
		}
		items[i] = fairqueue.RunningLease{TenantID: snapshot.Task.UserID, TaskID: snapshot.Task.ID,
			ClaimGeneration: uint64(snapshot.Task.ClaimGeneration), LeaseUntil: *snapshot.Task.LeaseUntil, ObservedDBNow: snapshot.ObservedDBNow}
	}
	nextCursor, done := recoveryPageCursor(next, len(items), limit)
	return fairqueue.RecoveryPage[fairqueue.RunningLease]{Items: items, NextCursor: nextCursor, Done: done}, nil
}

func (a *FairQueueAdapter) CaptureRepairHighWater(ctx context.Context) (string, error) {
	if a == nil || a.options.Recovery == nil {
		return "", fairqueue.ErrDependencyUnavailable
	}
	high, err := a.options.Recovery.CaptureImageBrokerRepairHighWater(ctx)
	return recoveryCursor(high), err
}

func (a *FairQueueAdapter) ListBrokerBackedCandidates(ctx context.Context, highWater, after string, limit int) (fairqueue.RecoveryPage[fairqueue.DispatchCandidate], error) {
	high, err := parseImageCursor(highWater)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	cursor, err := parseImageCursor(after)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	candidates, next, err := a.options.Recovery.ListBrokerBackedImageCandidates(ctx, high, cursor, limit)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	items, err := convertImageCandidates(candidates)
	if err != nil {
		return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{}, err
	}
	nextCursor, done := recoveryPageCursor(next, len(items), limit)
	return fairqueue.RecoveryPage[fairqueue.DispatchCandidate]{Items: items, NextCursor: nextCursor, Done: done}, nil
}

func (a *FairQueueAdapter) RearmAfterBrokerLoss(ctx context.Context, candidate fairqueue.DispatchCandidate) (fairqueue.DispatchCandidate, bool, error) {
	decoded, err := decodeImageCandidate(candidate)
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	rearmed, changed, err := a.options.Recovery.RearmImageCandidateAfterBrokerLoss(ctx, decoded)
	if err != nil || !changed {
		return fairqueue.DispatchCandidate{}, changed, err
	}
	converted, err := encodeImageCandidate(*rearmed)
	return converted, err == nil, err
}

func parseImageCursor(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 || strconv.FormatInt(parsed, 10) != value {
		return 0, errors.New("imagegen: invalid fairqueue cursor")
	}
	return parsed, nil
}

func imageCursor(value int64, count int) string {
	if count == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func imageMessage(candidate store.ImageTaskDispatchCandidate) fairqueue.Message {
	return fairqueue.Message{
		Version: fairqueue.MessageVersion1, Resource: store.ImageGenerationResource,
		TenantID: candidate.Task.UserID, TaskType: ImageGenerationTaskType, TaskID: candidate.Task.ID,
		DispatchToken: fairqueue.DispatchToken{Resource: store.ImageGenerationResource,
			TaskID: candidate.Task.ID, Generation: uint64(candidate.Task.DispatchGeneration)},
	}
}

func encodeImageCandidate(candidate store.ImageTaskDispatchCandidate) (fairqueue.DispatchCandidate, error) {
	if !imageFairTaskIDPattern.MatchString(candidate.Task.ID) || candidate.Task.SequenceID <= 0 ||
		candidate.Task.UserID == "" || candidate.Task.DispatchGeneration <= candidate.Task.ClaimGeneration {
		return fairqueue.DispatchCandidate{}, fairqueue.ErrAuthoritativeStateCorrupt
	}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return fairqueue.DispatchCandidate{}, err
	}
	converted := fairqueue.DispatchCandidate{Message: imageMessage(candidate), Guard: string(raw)}
	if err := converted.Validate(); err != nil {
		return fairqueue.DispatchCandidate{}, errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	}
	return converted, nil
}

func decodeImageCandidate(candidate fairqueue.DispatchCandidate) (store.ImageTaskDispatchCandidate, error) {
	if err := candidate.Validate(); err != nil {
		return store.ImageTaskDispatchCandidate{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(candidate.Guard))
	decoder.DisallowUnknownFields()
	var decoded store.ImageTaskDispatchCandidate
	if err := decoder.Decode(&decoded); err != nil {
		return decoded, errors.Join(fairqueue.ErrAuthoritativeStateCorrupt, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return decoded, fairqueue.ErrAuthoritativeStateCorrupt
	}
	if imageMessage(decoded) != candidate.Message {
		return decoded, fairqueue.ErrAuthoritativeStateCorrupt
	}
	return decoded, nil
}

func convertImageCandidates(candidates []store.ImageTaskDispatchCandidate) ([]fairqueue.DispatchCandidate, error) {
	result := make([]fairqueue.DispatchCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		converted, err := encodeImageCandidate(candidate)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func (a *FairQueueAdapter) ListDispatchCandidates(ctx context.Context, after string, limit int) ([]fairqueue.DispatchCandidate, string, error) {
	if a == nil || a.options.Store == nil {
		return nil, after, errors.New("imagegen: fairqueue adapter is not configured")
	}
	cursor, err := parseImageCursor(after)
	if err != nil {
		return nil, after, err
	}
	candidates, next, err := a.options.Store.ListDispatchableImageTasksPage(ctx, cursor, limit)
	if err != nil {
		return nil, after, err
	}
	converted, err := convertImageCandidates(candidates)
	return converted, imageCursor(next, len(converted)), err
}

func (a *FairQueueAdapter) GetDispatchableByID(ctx context.Context, taskID string) (fairqueue.DispatchCandidate, bool, error) {
	if !imageFairTaskIDPattern.MatchString(taskID) {
		return fairqueue.DispatchCandidate{}, false, fairqueue.ErrInvalidModel
	}
	candidate, err := a.options.Store.GetDispatchableImageTaskByID(ctx, taskID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && candidate == nil) {
		return fairqueue.DispatchCandidate{}, false, nil
	}
	if err != nil {
		return fairqueue.DispatchCandidate{}, false, err
	}
	converted, err := encodeImageCandidate(*candidate)
	return converted, err == nil, err
}

func (a *FairQueueAdapter) MarkDispatched(ctx context.Context, candidate fairqueue.DispatchCandidate) (bool, error) {
	decoded, err := decodeImageCandidate(candidate)
	if err != nil {
		return false, err
	}
	changed, err := a.options.Store.MarkImageTaskDispatched(ctx, decoded, int64(candidate.Message.DispatchToken.Generation))
	if errors.Is(err, store.ErrImageTaskDispatchStale) {
		return false, nil
	}
	return changed, err
}

func (a *FairQueueAdapter) RearmExpiredPage(ctx context.Context, after string, limit int) ([]fairqueue.DispatchCandidate, string, error) {
	cursor, err := parseImageCursor(after)
	if err != nil {
		return nil, after, err
	}
	candidates, next, err := a.options.Store.SweepExpiredImageGenerationTasks(ctx, cursor, limit, a.options.ExpiredLockTimeout)
	if err != nil {
		return nil, after, err
	}
	converted, err := convertImageCandidates(candidates)
	return converted, imageCursor(next, len(converted)), err
}

type imagePreparedTask struct {
	adapter *FairQueueAdapter
	claim   *store.ImageGenerationTaskClaim
}

func (p *imagePreparedTask) Run(ctx context.Context) error {
	if p == nil || p.adapter == nil || p.claim == nil {
		return fairqueue.ErrAuthoritativeStateCorrupt
	}
	return p.adapter.runClaim(ctx, p.claim)
}

func imagePrepareResult(disposition fairqueue.PrepareDisposition) fairqueue.PrepareResult {
	switch disposition {
	case fairqueue.PrepareCapacityDeferred:
		return fairqueue.PrepareResult{Disposition: disposition, DeliveryAction: fairqueue.DeliveryNackRequeue, CanonicalEffect: fairqueue.CanonicalNone}
	case fairqueue.PrepareDuplicateStaleTerminal:
		return fairqueue.PrepareResult{Disposition: disposition, DeliveryAction: fairqueue.DeliveryAckRelease, CanonicalEffect: fairqueue.CanonicalNone}
	case fairqueue.PreparePoisonPermanentInvalidMessage:
		return fairqueue.PrepareResult{Disposition: disposition, DeliveryAction: fairqueue.DeliveryConfirmDLQThenAck, CanonicalEffect: fairqueue.CanonicalNone}
	default:
		return fairqueue.PrepareResult{Disposition: disposition, DeliveryAction: fairqueue.DeliveryNackRequeue, CanonicalEffect: fairqueue.CanonicalNone}
	}
}

func (a *FairQueueAdapter) Prepare(ctx context.Context, request fairqueue.PrepareRequest) (fairqueue.PreparedTask, fairqueue.PrepareResult, error) {
	if err := request.Validate(); err != nil {
		return nil, fairqueue.PrepareResult{}, err
	}
	if request.RegisteredResource != store.ImageGenerationResource {
		return nil, fairqueue.PrepareResult{}, fairqueue.ErrAuthoritativeStateCorrupt
	}
	if request.Message == nil || request.Message.TaskType != ImageGenerationTaskType {
		return a.preparePoison(ctx, request)
	}
	message := *request.Message
	claim, err := a.options.Store.ClaimImageGenerationTaskByID(ctx, message.TaskID, message.TenantID,
		int64(message.DispatchToken.Generation), a.options.WorkerID, a.options.TaskLease, a.options.ClaimLimits)
	if err != nil {
		return nil, imagePrepareResult(fairqueue.PrepareTransientInfrastructure), err
	}
	switch claim.Disposition {
	case store.ImageGenerationClaimed:
		if claim.Claim == nil || claim.Claim.Task.ID != message.TaskID || claim.Claim.Fence.ClaimGeneration != int64(message.DispatchToken.Generation) {
			return nil, fairqueue.PrepareResult{}, fairqueue.ErrAuthoritativeStateCorrupt
		}
		prepared := &imagePreparedTask{adapter: a, claim: claim.Claim}
		return prepared, fairqueue.PrepareResult{Disposition: fairqueue.PrepareClaimed,
			DeliveryAction: fairqueue.DeliveryPromoteThenAckRun, CanonicalEffect: fairqueue.CanonicalClaimCommitted,
			Claim: &fairqueue.ClaimRef{TenantID: message.TenantID, TaskID: message.TaskID, ClaimGeneration: message.DispatchToken.Generation}}, nil
	case store.ImageGenerationClaimCapacityDeferred:
		return nil, imagePrepareResult(fairqueue.PrepareCapacityDeferred), nil
	case store.ImageGenerationClaimDuplicateStale, store.ImageGenerationClaimBatchCanceled:
		return nil, imagePrepareResult(fairqueue.PrepareDuplicateStaleTerminal), nil
	default:
		return nil, fairqueue.PrepareResult{}, fmt.Errorf("imagegen: unknown claim disposition %q", claim.Disposition)
	}
}

func (a *FairQueueAdapter) preparePoison(ctx context.Context, request fairqueue.PrepareRequest) (fairqueue.PreparedTask, fairqueue.PrepareResult, error) {
	seen := make(map[store.ImagePoisonRepairLocator]struct{})
	for _, token := range []*fairqueue.DispatchToken{request.HeaderToken, func() *fairqueue.DispatchToken {
		if request.BodyCandidate == nil {
			return nil
		}
		return &request.BodyCandidate.DispatchToken
	}()} {
		if token == nil || token.Resource != store.ImageGenerationResource || !imageFairTaskIDPattern.MatchString(token.TaskID) || token.Generation == 0 {
			continue
		}
		locator := store.ImagePoisonRepairLocator{TaskID: token.TaskID, Generation: int64(token.Generation)}
		if _, duplicate := seen[locator]; duplicate {
			continue
		}
		seen[locator] = struct{}{}
		if _, _, err := a.options.Store.RepairPoisonImageCandidate(ctx, locator, request.RegisteredResource, request.QueueTenantHash); err != nil {
			return nil, imagePrepareResult(fairqueue.PrepareTransientInfrastructure), err
		}
	}
	result := imagePrepareResult(fairqueue.PreparePoisonPermanentInvalidMessage)
	if len(seen) > 0 {
		result.CanonicalEffect = fairqueue.CanonicalPoisonRepairSettled
	}
	return nil, result, nil
}

type imageHeartbeatOutcome struct {
	disposition store.ImageGenerationHeartbeatDisposition
	err         error
}

func (a *FairQueueAdapter) runClaim(ctx context.Context, claim *store.ImageGenerationTaskClaim) error {
	if a.options.Generation == nil || a.options.Artifacts == nil || claim == nil {
		return errors.New("imagegen: fairqueue execution dependencies are not configured")
	}
	identity := ExecutionIdentity{
		UserID: claim.Batch.UserID, ConfigUserID: claim.Batch.ConfigUserID,
		AgentOwnerUserID: claim.Batch.AgentOwnerUserID, AgentID: claim.Batch.AgentID,
		WorkspaceProjectID: claim.Batch.WorkspaceProjectID, WorkspaceSessionID: claim.Batch.WorkspaceSessionID,
	}
	var plan SafeProviderPlan
	if err := json.Unmarshal(claim.Batch.ProviderPlanJSON, &plan); err != nil || plan.Validate() != nil || !plan.MatchesIdentity(identity) {
		_, finishErr := a.options.Store.FinishImageGenerationTaskFailed(context.WithoutCancel(ctx), claim.Fence, "INVALID_PROVIDER_PLAN")
		return errors.Join(errors.New("imagegen: stored provider plan is invalid"), finishErr)
	}
	scope := ArtifactScope{AgentID: identity.AgentID, ProjectID: identity.WorkspaceProjectID, SessionID: identity.WorkspaceSessionID}
	runCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	heartbeatOutcome := make(chan imageHeartbeatOutcome, 1)
	interval := a.options.TaskHeartbeat
	if interval <= 0 {
		interval = a.options.TaskLease / 3
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				disposition, err := a.options.Store.HeartbeatImageGenerationTask(runCtx, claim.Fence, a.options.TaskLease)
				if err != nil || disposition != store.ImageGenerationHeartbeatExtended {
					heartbeatOutcome <- imageHeartbeatOutcome{disposition: disposition, err: err}
					cancel()
					return
				}
			}
		}
	}()
	stop := func() {
		cancel()
		<-heartbeatDone
	}
	defer stop()
	checkHeartbeat := func() (bool, error) {
		select {
		case outcome := <-heartbeatOutcome:
			if outcome.disposition == store.ImageGenerationHeartbeatCanceled {
				_, finishErr := a.options.Store.FinishImageGenerationTaskCanceled(context.WithoutCancel(ctx), claim.Fence)
				return true, finishErr
			}
			if outcome.err != nil {
				return true, outcome.err
			}
			return true, fairqueue.ErrAuthoritativeStateCorrupt
		default:
			return false, nil
		}
	}

	if salvaged, ok, err := a.options.Artifacts.Salvage(runCtx, ArtifactSalvageRequest{
		Scope: scope, BatchID: claim.Batch.ID, TaskID: claim.Task.ID,
		PreviousClaimGeneration: claim.PreviousClaimGeneration,
		RequestFingerprint:      claim.Task.RequestFingerprint, ExpectedCount: claim.Task.RequestedCount,
		CancelRequested: claim.Batch.CancelRequested,
	}); err != nil {
		if stopped, heartbeatErr := checkHeartbeat(); stopped {
			return heartbeatErr
		}
		return a.retryClaim(ctx, claim.Fence, "ARTIFACT_SALVAGE", err)
	} else if ok {
		if stopped, heartbeatErr := checkHeartbeat(); stopped {
			return heartbeatErr
		}
		return a.finishManifest(ctx, claim, salvaged)
	}

	providerResult, generateErr := a.options.Generation.Generate(runCtx, identity, plan, GenerateRequest{
		Prompt: claim.Task.Prompt, Size: claim.Task.Size, Count: claim.Task.RequestedCount,
	})
	if stopped, heartbeatErr := checkHeartbeat(); stopped {
		return heartbeatErr
	}
	if generateErr != nil {
		kind := ProviderErrorKind(generateErr)
		switch kind {
		case ErrorInvalidRequest, ErrorSafetyRejected, ErrorAuthConfig, ErrorModelUnavailable, ErrorEmptyResult, ErrorIncompleteResult:
			_, finishErr := a.options.Store.FinishImageGenerationTaskFailed(context.WithoutCancel(ctx), claim.Fence, string(kind))
			return errors.Join(generateErr, finishErr)
		default:
			return a.retryClaim(ctx, claim.Fence, string(kind), generateErr)
		}
	}
	manifest, err := a.options.Artifacts.Publish(runCtx, ArtifactPublishRequest{
		Scope: scope, BatchID: claim.Batch.ID, TaskID: claim.Task.ID,
		ClaimGeneration: claim.Fence.ClaimGeneration, RequestFingerprint: claim.Task.RequestFingerprint,
		Provider: providerResult.Provider, Model: providerResult.Model,
		ExpectedCount: claim.Task.RequestedCount, Images: providerResult.Images,
	})
	if err != nil {
		if stopped, heartbeatErr := checkHeartbeat(); stopped {
			return heartbeatErr
		}
		return a.retryClaim(ctx, claim.Fence, "ARTIFACT_PUBLISH", err)
	}
	if stopped, heartbeatErr := checkHeartbeat(); stopped {
		return heartbeatErr
	}
	return a.finishManifest(ctx, claim, manifest)
}

func (a *FairQueueAdapter) retryClaim(ctx context.Context, fence store.ImageGenerationFence, code string, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	changed, err := a.options.Store.FinishImageGenerationTaskRetry(cleanupCtx, fence, code, time.Now().UTC().Add(a.options.RetryDelay))
	if err != nil {
		return errors.Join(cause, err)
	}
	if !changed {
		return errors.Join(cause, fairqueue.ErrAuthoritativeStateCorrupt)
	}
	return cause
}

func (a *FairQueueAdapter) finishManifest(ctx context.Context, claim *store.ImageGenerationTaskClaim, manifest ArtifactManifest) error {
	artifacts, err := json.Marshal(manifest.Artifacts)
	if err != nil {
		return a.retryClaim(ctx, claim.Fence, "MANIFEST_ENCODE", err)
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, changed, err := a.options.Store.FinishImageGenerationTaskDone(cleanupCtx, claim.Fence, store.ImageTaskDoneResult{
		Provider: manifest.Provider, Model: manifest.Model, ManifestKey: manifest.ManifestKey, ArtifactsJSON: artifacts,
	})
	if err == nil && changed {
		return nil
	}
	if changed && errors.Is(err, store.ErrImageGenerationBatchArtifactLimit) {
		deleteErr := a.options.Artifacts.DeleteClaimArtifacts(cleanupCtx, manifest)
		return errors.Join(err, deleteErr)
	}
	if !changed {
		_ = a.options.Artifacts.DeleteClaimArtifacts(cleanupCtx, manifest)
		return errors.Join(err, fairqueue.ErrAuthoritativeStateCorrupt)
	}
	// A complete manifest with an uncertain DB outcome is intentionally left in
	// place for the next exact claim's PreviousClaimGeneration salvage.
	return err
}
