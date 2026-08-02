// Package rag is the RAG module facade. It owns knowledge-base management,
// document ingestion, and retrieval; callers never access the backing stores
// directly.
package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/rerank"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

var (
	ErrNotFound  = errors.New("知识库或文档不存在")
	ErrForbidden = errors.New("无权访问该知识库")
	ErrQuota     = errors.New("超出配额限制")
)

// UserEmbedCfgFn resolves a user-level embedding override. A false return
// means the system embedding configuration should be used.
type UserEmbedCfgFn func(ctx context.Context, userID string) (config.RAGEmbeddingCfg, bool)

// QueryLLMFn is the narrow LLM boundary used by retrieval query planning.
// The gateway resolves the effective user model and provider; the RAG package
// owns the prompt, output validation, and fallback behavior.
type QueryLLMFn func(ctx context.Context, userID, systemPrompt, userPrompt string) (string, error)

// WorkerMode is an immutable startup choice. Dependency outages never change
// it at runtime: a fair or paused service must not fall back to legacy claims.
type WorkerMode string

const (
	WorkerModeLegacy WorkerMode = "legacy"
	WorkerModePaused WorkerMode = "paused"
	WorkerModeFair   WorkerMode = "fair"
)

func (m WorkerMode) Valid() bool {
	return m == WorkerModeLegacy || m == WorkerModePaused || m == WorkerModeFair
}

func normalizeWorkerMode(mode WorkerMode) WorkerMode {
	if mode == "" {
		return WorkerModeLegacy
	}
	if !mode.Valid() {
		// New cannot return a configuration error. Treat an invalid value as the
		// non-claiming state so a typo can never start an unsafe legacy worker.
		return WorkerModePaused
	}
	return mode
}

// TaskNotifier is the best-effort low-latency handoff to the durable fair
// dispatcher. A periodic MySQL scan remains authoritative after any error.
type TaskNotifier interface {
	TryDispatch(ctx context.Context, taskID int64) error
}

// FairStore is the expected-writer-bound facade used by API lifecycle writes
// and DocumentAI accounting while fair execution is enabled.
type FairStore interface {
	ExpectedWriterFingerprint() string
	GetConfigByName(context.Context, string, string, string, string) (*store.ConfigRecord, error)
	GetRAGKBForLifecycle(context.Context, string) (*store.RAGKBRecord, error)
	GetRAGDocumentForLifecycle(context.Context, string) (*store.RAGDocumentRecord, error)
	ListRAGDocumentsByKBForLifecycle(context.Context, string) ([]store.RAGDocumentRecord, error)
	GetUserForRAGLifecycle(context.Context, string) (*store.UserRecord, error)
	BeginOriginalRAGObjectWrite(context.Context, store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error)
	MarkOriginalRAGObjectWriteReady(context.Context, store.RAGObjectWriteFence) (bool, error)
	CreateRAGDocumentWithVersionAndIndexTask(context.Context, *store.RAGDocumentRecord, *store.RAGDocumentVersionRecord, int) (int64, error)
	CreateRAGDocumentWithVersionAndIndexTaskPolicy(context.Context, *store.RAGDocumentRecord, *store.RAGDocumentVersionRecord, int, store.RAGAdvancedEnqueuePolicy) (int64, error)
	AdvanceDocumentVersionAndCreateTask(context.Context, int64, *store.RAGDocumentVersionRecord) (*store.RAGIndexTaskRecord, error)
	AdvanceDocumentVersionAndCreateTaskPolicy(context.Context, int64, *store.RAGDocumentVersionRecord, store.RAGAdvancedEnqueuePolicy) (*store.RAGIndexTaskRecord, error)
	MarkRAGDocumentDeleting(context.Context, string) (*store.RAGDocumentRecord, error)
	MarkRAGKBDeleting(context.Context, string) (*store.RAGKBRecord, error)
	CreateRAGDocumentAITaskBudgetForIndex(context.Context, store.IndexFence, *store.RAGDocumentAITaskBudgetRecord) error
	GetRAGDocumentAIUsage(context.Context, string) (*store.RAGDocumentAIUsageRecord, error)
	ReserveRAGDocumentAIUsage(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error)
	MarkSentRAGDocumentAIUsage(context.Context, string, store.IndexFence) (bool, error)
	CommitRAGDocumentAIUsage(context.Context, string, int64, int64, int64, bool) (bool, error)
	ReleaseRAGDocumentAIUsage(context.Context, string) (bool, error)
	ReconcileRAGDocumentAIUsage(context.Context, time.Time, time.Time, int) (int, error)
}

// FairExecutionStore is the Task 9 live-fence surface implemented by DBStore.
// It is separate from FairStore because reads and catalog mutations validate a
// claim fence on the raw store, while lifecycle/budget operations use the
// expected-writer-bound facade.
type FairExecutionStore interface {
	GetRAGDocumentForIndex(context.Context, store.IndexFence) (*store.RAGDocumentRecord, error)
	GetRAGKBForIndex(context.Context, store.IndexFence, string) (*store.RAGKBRecord, error)
	GetRAGDocumentVersionForIndex(context.Context, store.IndexFence, string, int64) (*store.RAGDocumentVersionRecord, error)
	ListRAGAssetsByIDsForIndex(context.Context, store.IndexFence, []string) ([]store.RAGAssetRecord, error)
	ListRAGAttachmentsByIDsForIndex(context.Context, store.IndexFence, []string) ([]store.RAGAttachmentRecord, error)
	PutRAGChunksForIndex(context.Context, store.IndexFence, []store.RAGChunkRecord) (bool, error)
	PutRAGChunkAssetsForIndex(context.Context, store.IndexFence, []store.RAGChunkAssetRecord) (bool, error)
	BeginRAGObjectWriteForIndex(context.Context, store.IndexFence, store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error)
	MarkRAGObjectWriteReadyForIndex(context.Context, store.IndexFence, store.RAGObjectWriteFence) (bool, error)
	RegisterRAGCacheObjectForIndex(context.Context, store.IndexFence, store.RAGCacheObjectRecord) error
	PublishRAGAssetsForIndex(context.Context, store.IndexFence, []store.RAGAssetRecord, []string) (bool, error)
	PublishRAGAssetsAndAttachmentsForIndex(context.Context, store.IndexFence, []store.RAGAssetRecord, []string, []store.RAGAttachmentRecord, []string) (bool, error)
}

type fairIndexFenceContextKey struct{}

func withFairIndexFence(ctx context.Context, fence store.IndexFence) context.Context {
	return context.WithValue(ctx, fairIndexFenceContextKey{}, fence)
}

func fairIndexFenceFromContext(ctx context.Context) (store.IndexFence, bool) {
	if ctx == nil {
		return store.IndexFence{}, false
	}
	fence, ok := ctx.Value(fairIndexFenceContextKey{}).(store.IndexFence)
	return fence, ok
}

type modeAwareRAGStore struct {
	store.Store
	mode          WorkerMode
	fairStore     FairStore
	fairExecution FairExecutionStore
}

func (s *modeAwareRAGStore) requireFairStore(fence *store.IndexFence) (FairStore, error) {
	if s == nil || s.fairStore == nil {
		return nil, store.ErrFairQueueMySQLRequired
	}
	if fence != nil && s.fairStore.ExpectedWriterFingerprint() != fence.ExpectedWriterFingerprint {
		return nil, store.ErrFairQueueWriterMismatch
	}
	return s.fairStore, nil
}

func (s *modeAwareRAGStore) requireFairExecution() (FairExecutionStore, error) {
	if s == nil || s.fairExecution == nil {
		return nil, store.ErrFairQueueMySQLRequired
	}
	return s.fairExecution, nil
}

func (s *modeAwareRAGStore) CreateRAGDocumentWithVersionAndIndexTask(
	ctx context.Context,
	doc *store.RAGDocumentRecord,
	version *store.RAGDocumentVersionRecord,
	maxRetry int,
) (int64, error) {
	if s.mode != WorkerModeFair {
		return s.Store.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return 0, err
	}
	return fair.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry)
}

func (s *modeAwareRAGStore) CreateRAGDocumentWithVersionAndIndexTaskPolicy(
	ctx context.Context,
	doc *store.RAGDocumentRecord,
	version *store.RAGDocumentVersionRecord,
	maxRetry int,
	policy store.RAGAdvancedEnqueuePolicy,
) (int64, error) {
	if s.mode != WorkerModeFair {
		return s.Store.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, version, maxRetry, policy)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return 0, err
	}
	return fair.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, version, maxRetry, policy)
}

func (s *modeAwareRAGStore) AdvanceDocumentVersionAndCreateTask(
	ctx context.Context,
	expectedVersion int64,
	snapshot *store.RAGDocumentVersionRecord,
) (*store.RAGIndexTaskRecord, error) {
	if s.mode != WorkerModeFair {
		return s.Store.AdvanceDocumentVersionAndCreateTask(ctx, expectedVersion, snapshot)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return nil, err
	}
	return fair.AdvanceDocumentVersionAndCreateTask(ctx, expectedVersion, snapshot)
}

func (s *modeAwareRAGStore) AdvanceDocumentVersionAndCreateTaskPolicy(
	ctx context.Context,
	expectedVersion int64,
	snapshot *store.RAGDocumentVersionRecord,
	policy store.RAGAdvancedEnqueuePolicy,
) (*store.RAGIndexTaskRecord, error) {
	if s.mode != WorkerModeFair {
		return s.Store.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, expectedVersion, snapshot, policy)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return nil, err
	}
	return fair.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, expectedVersion, snapshot, policy)
}

func (s *modeAwareRAGStore) MarkRAGDocumentDeleting(
	ctx context.Context,
	id string,
) (*store.RAGDocumentRecord, error) {
	if s.mode != WorkerModeFair {
		return s.Store.MarkRAGDocumentDeleting(ctx, id)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return nil, err
	}
	return fair.MarkRAGDocumentDeleting(ctx, id)
}

func (s *modeAwareRAGStore) MarkRAGKBDeleting(
	ctx context.Context,
	id string,
) (*store.RAGKBRecord, error) {
	if s.mode != WorkerModeFair {
		return s.Store.MarkRAGKBDeleting(ctx, id)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return nil, err
	}
	return fair.MarkRAGKBDeleting(ctx, id)
}

func (s *modeAwareRAGStore) GetRAGDocument(
	ctx context.Context,
	id string,
) (*store.RAGDocumentRecord, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			fair, err := s.requireFairStore(nil)
			if err != nil {
				return nil, err
			}
			return fair.GetRAGDocumentForLifecycle(ctx, id)
		}
		return s.Store.GetRAGDocument(ctx, id)
	}
	if id == "" || id != fence.DocID {
		return nil, store.ErrRAGDocumentVersionMismatch
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return nil, err
	}
	record, err := fair.GetRAGDocumentForIndex(ctx, fence)
	if err == nil {
		if record == nil {
			return nil, errIndexFenceLost
		}
		if record.ID != id {
			return nil, store.ErrRAGDocumentVersionMismatch
		}
	}
	return record, err
}

func (s *modeAwareRAGStore) GetRAGKB(
	ctx context.Context,
	id string,
) (*store.RAGKBRecord, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			fair, err := s.requireFairStore(nil)
			if err != nil {
				return nil, err
			}
			return fair.GetRAGKBForLifecycle(ctx, id)
		}
		return s.Store.GetRAGKB(ctx, id)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return nil, err
	}
	record, err := fair.GetRAGKBForIndex(ctx, fence, id)
	if err == nil && record == nil {
		return nil, errIndexFenceLost
	}
	return record, err
}

func (s *modeAwareRAGStore) ListRAGDocumentsByKB(
	ctx context.Context,
	kbID string,
) ([]store.RAGDocumentRecord, error) {
	if s.mode != WorkerModeFair {
		return s.Store.ListRAGDocumentsByKB(ctx, kbID)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return nil, err
	}
	return fair.ListRAGDocumentsByKBForLifecycle(ctx, kbID)
}

func (s *modeAwareRAGStore) GetUser(
	ctx context.Context,
	id string,
) (*store.UserRecord, error) {
	if s.mode != WorkerModeFair {
		return s.Store.GetUser(ctx, id)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return nil, err
	}
	return fair.GetUserForRAGLifecycle(ctx, id)
}

func (s *modeAwareRAGStore) GetRAGDocumentVersion(
	ctx context.Context,
	docID string,
	docVersion int64,
) (*store.RAGDocumentVersionRecord, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		return s.Store.GetRAGDocumentVersion(ctx, docID, docVersion)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return nil, err
	}
	record, err := fair.GetRAGDocumentVersionForIndex(ctx, fence, docID, docVersion)
	if err == nil && record == nil {
		return nil, errIndexFenceLost
	}
	return record, err
}

func (s *modeAwareRAGStore) ListRAGAssetsByIDs(
	ctx context.Context,
	ids []string,
) ([]store.RAGAssetRecord, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		return s.Store.ListRAGAssetsByIDs(ctx, ids)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return nil, err
	}
	records, err := fair.ListRAGAssetsByIDsForIndex(ctx, fence, ids)
	if err == nil && records == nil {
		return nil, errIndexFenceLost
	}
	return records, err
}

func (s *modeAwareRAGStore) ListRAGAttachmentsByIDs(
	ctx context.Context,
	ids []string,
) ([]store.RAGAttachmentRecord, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		return s.Store.ListRAGAttachmentsByIDs(ctx, ids)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return nil, err
	}
	records, err := fair.ListRAGAttachmentsByIDsForIndex(ctx, fence, ids)
	if err == nil && records == nil {
		return nil, errIndexFenceLost
	}
	return records, err
}

func (s *modeAwareRAGStore) PutRAGChunks(ctx context.Context, chunks []store.RAGChunkRecord) error {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			return store.ErrRAGDocumentVersionMismatch
		}
		return s.Store.PutRAGChunks(ctx, chunks)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return err
	}
	written, err := fair.PutRAGChunksForIndex(ctx, fence, chunks)
	if err != nil {
		return err
	}
	if !written {
		return errIndexFenceLost
	}
	return nil
}

func (s *modeAwareRAGStore) PutRAGChunkAssets(
	ctx context.Context,
	mappings []store.RAGChunkAssetRecord,
) error {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			return store.ErrRAGDocumentVersionMismatch
		}
		return s.Store.PutRAGChunkAssets(ctx, mappings)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return err
	}
	written, err := fair.PutRAGChunkAssetsForIndex(ctx, fence, mappings)
	if err != nil {
		return err
	}
	if !written {
		return errIndexFenceLost
	}
	return nil
}

func (s *modeAwareRAGStore) BeginRAGObjectWrite(
	ctx context.Context,
	request store.RAGObjectWriteRequest,
) (*store.RAGObjectWriteFence, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			if request.ObjectKind != store.RAGObjectKindOriginal {
				return nil, store.ErrRAGDocumentVersionMismatch
			}
			fair, err := s.requireFairStore(nil)
			if err != nil {
				return nil, err
			}
			return fair.BeginOriginalRAGObjectWrite(ctx, request)
		}
		return s.Store.BeginRAGObjectWrite(ctx, request)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return nil, err
	}
	return fair.BeginRAGObjectWriteForIndex(ctx, fence, request)
}

func (s *modeAwareRAGStore) MarkRAGObjectWriteReady(
	ctx context.Context,
	objectFence store.RAGObjectWriteFence,
) (bool, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			if objectFence.ObjectKind != store.RAGObjectKindOriginal {
				return false, store.ErrRAGDocumentVersionMismatch
			}
			fair, err := s.requireFairStore(nil)
			if err != nil {
				return false, err
			}
			return fair.MarkOriginalRAGObjectWriteReady(ctx, objectFence)
		}
		return s.Store.MarkRAGObjectWriteReady(ctx, objectFence)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return false, err
	}
	return fair.MarkRAGObjectWriteReadyForIndex(ctx, fence, objectFence)
}

func (s *modeAwareRAGStore) RegisterRAGCacheObject(
	ctx context.Context,
	record store.RAGCacheObjectRecord,
) error {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		if s.mode == WorkerModeFair {
			return store.ErrRAGDocumentVersionMismatch
		}
		return s.Store.RegisterRAGCacheObject(ctx, record)
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return err
	}
	return fair.RegisterRAGCacheObjectForIndex(ctx, fence, record)
}

func (s *modeAwareRAGStore) PublishRAGAssetsForIndex(
	ctx context.Context,
	fence store.IndexFence,
	assets []store.RAGAssetRecord,
	assetIDs []string,
) (bool, error) {
	if fence.ExpectedWriterFingerprint == "" {
		return s.Store.PublishRAGAssetsForIndex(ctx, fence, assets, assetIDs)
	}
	contextFence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext || contextFence != fence {
		return false, store.ErrRAGDocumentVersionMismatch
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return false, err
	}
	return fair.PublishRAGAssetsForIndex(ctx, fence, assets, assetIDs)
}

func (s *modeAwareRAGStore) PublishRAGAssetsAndAttachmentsForIndex(
	ctx context.Context,
	fence store.IndexFence,
	assets []store.RAGAssetRecord,
	assetIDs []string,
	attachments []store.RAGAttachmentRecord,
	attachmentIDs []string,
) (bool, error) {
	if fence.ExpectedWriterFingerprint == "" {
		return s.Store.PublishRAGAssetsAndAttachmentsForIndex(
			ctx, fence, assets, assetIDs, attachments, attachmentIDs,
		)
	}
	contextFence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext || contextFence != fence {
		return false, store.ErrRAGDocumentVersionMismatch
	}
	fair, err := s.requireFairExecution()
	if err != nil {
		return false, err
	}
	return fair.PublishRAGAssetsAndAttachmentsForIndex(
		ctx, fence, assets, assetIDs, attachments, attachmentIDs,
	)
}

func (s *modeAwareRAGStore) CreateRAGDocumentAITaskBudget(
	ctx context.Context,
	budget *store.RAGDocumentAITaskBudgetRecord,
) error {
	if _, fairContext := fairIndexFenceFromContext(ctx); fairContext {
		return store.ErrRAGDocumentAIInvalidFence
	}
	return s.Store.CreateRAGDocumentAITaskBudget(ctx, budget)
}

func (s *modeAwareRAGStore) CreateRAGDocumentAITaskBudgetForIndex(
	ctx context.Context,
	fence store.IndexFence,
	budget *store.RAGDocumentAITaskBudgetRecord,
) error {
	fair, err := s.requireFairStore(&fence)
	if err != nil {
		return err
	}
	return fair.CreateRAGDocumentAITaskBudgetForIndex(ctx, fence, budget)
}

func (s *modeAwareRAGStore) GetRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (*store.RAGDocumentAIUsageRecord, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		return s.Store.GetRAGDocumentAIUsage(ctx, idempotencyKey)
	}
	fair, err := s.requireFairStore(&fence)
	if err != nil {
		return nil, err
	}
	return fair.GetRAGDocumentAIUsage(ctx, idempotencyKey)
}

func (s *modeAwareRAGStore) ReserveRAGDocumentAIUsage(
	ctx context.Context,
	fence store.IndexFence,
	usage *store.RAGDocumentAIUsageRecord,
	limits store.RAGDocumentAILimits,
) (bool, error) {
	if fence.ExpectedWriterFingerprint == "" {
		return s.Store.ReserveRAGDocumentAIUsage(ctx, fence, usage, limits)
	}
	fair, err := s.requireFairStore(&fence)
	if err != nil {
		return false, err
	}
	return fair.ReserveRAGDocumentAIUsage(ctx, fence, usage, limits)
}

func (s *modeAwareRAGStore) MarkSentRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	fence store.IndexFence,
) (bool, error) {
	if fence.ExpectedWriterFingerprint == "" {
		return s.Store.MarkSentRAGDocumentAIUsage(ctx, idempotencyKey, fence)
	}
	fair, err := s.requireFairStore(&fence)
	if err != nil {
		return false, err
	}
	return fair.MarkSentRAGDocumentAIUsage(ctx, idempotencyKey, fence)
}

func (s *modeAwareRAGStore) CommitRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	inputTokens, outputTokens, costMicroUSD int64,
	usageEstimated bool,
) (bool, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		return s.Store.CommitRAGDocumentAIUsage(
			ctx, idempotencyKey, inputTokens, outputTokens, costMicroUSD, usageEstimated,
		)
	}
	fair, err := s.requireFairStore(&fence)
	if err != nil {
		return false, err
	}
	return fair.CommitRAGDocumentAIUsage(
		ctx, idempotencyKey, inputTokens, outputTokens, costMicroUSD, usageEstimated,
	)
}

func (s *modeAwareRAGStore) ReleaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (bool, error) {
	fence, fairContext := fairIndexFenceFromContext(ctx)
	if !fairContext {
		return s.Store.ReleaseRAGDocumentAIUsage(ctx, idempotencyKey)
	}
	fair, err := s.requireFairStore(&fence)
	if err != nil {
		return false, err
	}
	return fair.ReleaseRAGDocumentAIUsage(ctx, idempotencyKey)
}

func (s *modeAwareRAGStore) ReconcileRAGDocumentAIUsage(
	ctx context.Context,
	reservedBefore, sentBefore time.Time,
	limit int,
) (int, error) {
	if s.mode != WorkerModeFair {
		return s.Store.ReconcileRAGDocumentAIUsage(ctx, reservedBefore, sentBefore, limit)
	}
	fair, err := s.requireFairStore(nil)
	if err != nil {
		return 0, err
	}
	return fair.ReconcileRAGDocumentAIUsage(ctx, reservedBefore, sentBefore, limit)
}

type fairExecutionCacheCatalog struct {
	legacy store.RAGCacheCatalog
	fair   FairExecutionStore
	mode   WorkerMode
}

type fairTaskBudgetLedger struct {
	fair  FairStore
	fence store.IndexFence
}

func (l *fairTaskBudgetLedger) validate() error {
	if l == nil || l.fair == nil || !validFairWriterFingerprint(l.fence.ExpectedWriterFingerprint) {
		return store.ErrFairQueueWriterMismatch
	}
	if l.fair.ExpectedWriterFingerprint() != l.fence.ExpectedWriterFingerprint {
		return store.ErrFairQueueWriterMismatch
	}
	return nil
}

func (l *fairTaskBudgetLedger) validateFence(fence store.IndexFence) error {
	if fence != l.fence {
		return store.ErrRAGDocumentAIInvalidFence
	}
	return l.validate()
}

func (l *fairTaskBudgetLedger) CreateRAGDocumentAITaskBudget(
	context.Context,
	*store.RAGDocumentAITaskBudgetRecord,
) error {
	return store.ErrRAGDocumentAIInvalidFence
}

func (l *fairTaskBudgetLedger) CreateRAGDocumentAITaskBudgetForIndex(
	ctx context.Context,
	fence store.IndexFence,
	budget *store.RAGDocumentAITaskBudgetRecord,
) error {
	if err := l.validateFence(fence); err != nil {
		return err
	}
	return l.fair.CreateRAGDocumentAITaskBudgetForIndex(ctx, fence, budget)
}

func (l *fairTaskBudgetLedger) GetRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (*store.RAGDocumentAIUsageRecord, error) {
	if err := l.validate(); err != nil {
		return nil, err
	}
	return l.fair.GetRAGDocumentAIUsage(ctx, idempotencyKey)
}

func (l *fairTaskBudgetLedger) ReserveRAGDocumentAIUsage(
	ctx context.Context,
	fence store.IndexFence,
	usage *store.RAGDocumentAIUsageRecord,
	limits store.RAGDocumentAILimits,
) (bool, error) {
	if err := l.validateFence(fence); err != nil {
		return false, err
	}
	return l.fair.ReserveRAGDocumentAIUsage(ctx, fence, usage, limits)
}

func (l *fairTaskBudgetLedger) MarkSentRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	fence store.IndexFence,
) (bool, error) {
	if err := l.validateFence(fence); err != nil {
		return false, err
	}
	return l.fair.MarkSentRAGDocumentAIUsage(ctx, idempotencyKey, fence)
}

func (l *fairTaskBudgetLedger) CommitRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	inputTokens, outputTokens, costMicroUSD int64,
	estimated bool,
) (bool, error) {
	if err := l.validate(); err != nil {
		return false, err
	}
	return l.fair.CommitRAGDocumentAIUsage(
		ctx, idempotencyKey, inputTokens, outputTokens, costMicroUSD, estimated,
	)
}

func (l *fairTaskBudgetLedger) ReleaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (bool, error) {
	if err := l.validate(); err != nil {
		return false, err
	}
	return l.fair.ReleaseRAGDocumentAIUsage(ctx, idempotencyKey)
}

// NewFairExecutionCacheCatalog permanently binds provider-cache writes to the
// configured worker mode. Fair calls require the claim fence in context; a
// lost marker can never select the legacy pool.
func NewFairExecutionCacheCatalog(
	legacy store.RAGCacheCatalog,
	fair FairExecutionStore,
	mode WorkerMode,
) store.RAGCacheCatalog {
	return &fairExecutionCacheCatalog{legacy: legacy, fair: fair, mode: mode}
}

func (c *fairExecutionCacheCatalog) RegisterRAGCacheObject(
	ctx context.Context,
	record store.RAGCacheObjectRecord,
) error {
	if c == nil {
		return errors.New("rag: cache catalog is unavailable")
	}
	fence, fairContext := fairIndexFenceFromContext(ctx)
	switch c.mode {
	case WorkerModeFair:
		if !fairContext {
			return store.ErrRAGDocumentVersionMismatch
		}
		if c.fair == nil {
			return store.ErrFairQueueMySQLRequired
		}
		return c.fair.RegisterRAGCacheObjectForIndex(ctx, fence, record)
	case WorkerModeLegacy:
		if fairContext {
			return store.ErrRAGDocumentVersionMismatch
		}
		if c.legacy == nil {
			return errors.New("rag: cache catalog is unavailable")
		}
		return c.legacy.RegisterRAGCacheObject(ctx, record)
	default:
		return store.ErrRAGDocumentVersionMismatch
	}
}

type Deps struct {
	Store         store.Store
	FairStore     FairStore
	FairExecution FairExecutionStore
	Vector        vector.Store
	Objects       objects.Store
	Cfg           config.RAGCfg
	UserEmbedCfg  UserEmbedCfgFn
	QueryLLM      QueryLLMFn
	Reranker      rerank.Reranker
	Parser        parse.Parser
	Primitives    parse.PrimitiveExtractor
	PageVision    vision.PageTranscriber
	ImageVision   vision.ImageTranscriber
	Enricher      enrich.Enricher
	Tokenizer     enrich.Tokenizer
	// Telemetry receives privacy-safe, closed-schema operational events. When
	// omitted, the service uses the default structured logger.
	Telemetry telemetry.Recorder
	// OfficeAvailable reads the background-probed, three-golden-gated
	// capability snapshot. Upload paths must not synchronously probe sidecar.
	OfficeAvailable func() bool
	Workers         int
	WorkerMode      WorkerMode
	Notifier        TaskNotifier
}

type Service struct {
	st              store.Store
	vec             vector.Store
	obj             objects.Store
	cfg             config.RAGCfg
	userCfg         UserEmbedCfgFn
	queryLLM        QueryLLMFn
	reranker        rerank.Reranker
	parser          parse.Parser
	primitives      parse.PrimitiveExtractor
	pageVision      vision.PageTranscriber
	imageVision     vision.ImageTranscriber
	enricher        enrich.Enricher
	tokenizer       enrich.Tokenizer
	telemetry       telemetry.Recorder
	officeAvailable func() bool
	tasks           chan int64
	workerCount     int
	workerID        string
	workerMode      WorkerMode
	taskNotifier    TaskNotifier
	fairStore       FairStore
	fairExecution   FairExecutionStore

	// The in-memory channel is only a latency hint. SQL claim/lease state is
	// authoritative and pollInterval guarantees recovery after a dropped hint.
	pollInterval                    time.Duration
	leaseDuration                   time.Duration
	heartbeatInterval               time.Duration
	gcGracePeriod                   time.Duration
	stagingArtifactTTL              time.Duration
	maxCacheFingerprintsPerDocument int
	cacheSweepCursorMu              sync.Mutex
	cacheSweepCursor                string
	deletingDocumentSweepCursor     string
	deletingKBSweepCursor           string
	startOnce                       sync.Once
	kbLocks                         sync.Map // map[string]*sync.RWMutex; deletion waits for in-flight KB work
	docLocks                        sync.Map // map[string]*sync.Mutex; one index mutation per document
}

func New(d Deps) *Service {
	d.Cfg.ApplyDefaults()
	recorder := d.Telemetry
	if recorder == nil {
		recorder = telemetry.NewSlogRecorder(nil)
	}
	if d.Workers <= 0 {
		d.Workers = 2
	}
	if d.Parser == nil {
		d.Parser = parse.NewLocalParser(
			d.Primitives, d.Cfg.Limits.MaxPagesPerDocument, d.Cfg.Limits.MaxExtractedBytes,
		)
	}
	if local, ok := d.Parser.(*parse.LocalParser); ok {
		// Parser routing limits are system policy, not mutable document input.
		// Fill zero values even for a caller-supplied LocalParser so gateway and
		// tests cannot accidentally bypass the configured document ceilings.
		if local.MaxPages <= 0 {
			local.MaxPages = d.Cfg.Limits.MaxPagesPerDocument
		}
		if local.MaxAssets <= 0 {
			local.MaxAssets = d.Cfg.Limits.MaxAssetsPerDocument
		}
		if local.MaxVisionPages <= 0 {
			local.MaxVisionPages = d.Cfg.Limits.MaxVisionPagesPerDocument
		}
		if local.MaxVisionAssets <= 0 {
			local.MaxVisionAssets = d.Cfg.Limits.MaxVisionAssetsPerDocument
		}
		if local.MaxExtractedBytes <= 0 {
			local.MaxExtractedBytes = d.Cfg.Limits.MaxExtractedBytes
		}
		if local.MaxAssetBytes <= 0 {
			local.MaxAssetBytes = d.Cfg.Limits.MaxAssetBytes
		}
		if local.MaxVisionInputBytes <= 0 {
			local.MaxVisionInputBytes = d.Cfg.Limits.MaxVisionInputBytes
		}
		if local.MaxImagePixels <= 0 {
			local.MaxImagePixels = d.Cfg.Limits.MaxImagePixels
		}
		if local.VisionImageMaxEdge <= 0 {
			local.VisionImageMaxEdge = d.Cfg.Limits.DisplayMaxEdge
		}
	}
	// Components that expose the narrow recorder hook share the same sink as
	// the orchestrator. This keeps one correlation stream without widening any
	// parser/provider interface to accept arbitrary log attributes.
	for _, component := range []any{d.Parser, d.Primitives, d.PageVision, d.ImageVision, d.Enricher} {
		if observable, ok := component.(interface{ SetRecorder(telemetry.Recorder) }); ok {
			observable.SetRecorder(recorder)
		}
	}
	mode := normalizeWorkerMode(d.WorkerMode)
	fairExecution := d.FairExecution
	if fairExecution == nil {
		fairExecution, _ = d.Store.(FairExecutionStore)
	}
	serviceStore := d.Store
	if serviceStore != nil && mode == WorkerModeFair {
		serviceStore = &modeAwareRAGStore{
			Store: serviceStore, mode: mode,
			fairStore: d.FairStore, fairExecution: fairExecution,
		}
	}
	return &Service{
		st:                              serviceStore,
		vec:                             d.Vector,
		obj:                             d.Objects,
		cfg:                             d.Cfg,
		userCfg:                         d.UserEmbedCfg,
		queryLLM:                        d.QueryLLM,
		reranker:                        d.Reranker,
		parser:                          d.Parser,
		primitives:                      d.Primitives,
		pageVision:                      d.PageVision,
		imageVision:                     d.ImageVision,
		enricher:                        d.Enricher,
		tokenizer:                       d.Tokenizer,
		telemetry:                       recorder,
		officeAvailable:                 d.OfficeAvailable,
		tasks:                           make(chan int64, 256),
		workerCount:                     d.Workers,
		workerID:                        "rag-" + uuid.NewString(),
		workerMode:                      mode,
		taskNotifier:                    d.Notifier,
		fairStore:                       d.FairStore,
		fairExecution:                   fairExecution,
		pollInterval:                    time.Second,
		leaseDuration:                   time.Minute,
		heartbeatInterval:               20 * time.Second,
		gcGracePeriod:                   time.Duration(d.Cfg.Limits.IndexGCGracePeriod) * time.Second,
		stagingArtifactTTL:              time.Duration(d.Cfg.Limits.StagingArtifactTTL) * time.Second,
		maxCacheFingerprintsPerDocument: d.Cfg.Limits.MaxCacheFingerprintsPerDocument,
	}
}

// WorkerMode returns the immutable startup mode selected for this service.
func (s *Service) WorkerMode() WorkerMode {
	if s == nil {
		return WorkerModePaused
	}
	return s.workerMode
}

// newTaskDocumentAIBudget binds one immutable version snapshot and claim fence
// to the durable façade shared by page vision, Office image vision, repairs and
// text enrichment. It owns no process-local spend counters.
func (s *Service) newTaskDocumentAIBudget(
	claim *store.RAGIndexClaim,
	userID string,
) (*vision.TaskDocumentAIBudget, error) {
	if s == nil || s.st == nil || claim == nil || strings.TrimSpace(userID) == "" {
		return nil, errors.New("RAG DocumentAI budget requires store, claim, and user")
	}
	var ledger vision.BudgetLedger = s.st
	if claim.Fence.ExpectedWriterFingerprint != "" {
		if !validFairWriterFingerprint(claim.Fence.ExpectedWriterFingerprint) {
			return nil, store.ErrFairQueueWriterMismatch
		}
		if s.fairStore == nil {
			return nil, store.ErrFairQueueMySQLRequired
		}
		if s.fairStore.ExpectedWriterFingerprint() != claim.Fence.ExpectedWriterFingerprint {
			return nil, store.ErrFairQueueWriterMismatch
		}
		ledger = &fairTaskBudgetLedger{fair: s.fairStore, fence: claim.Fence}
	}
	version := claim.Version
	return vision.NewTaskDocumentAIBudget(ledger, vision.TaskBudgetConfig{
		Fence: claim.Fence, UserID: userID,
		TaskLimits: store.RAGDocumentAILimits{
			MaxRequests:     int64(version.MaxDocumentAIRequests),
			MaxTokens:       version.MaxDocumentAITokens,
			MaxCostMicroUSD: version.MaxDocumentAICostMicroUSD,
		},
		UserLimits: store.RAGDocumentAILimits{
			MaxRequests:     int64(s.cfg.Limits.MaxDocumentAIRequestsPerUserPerDay),
			MaxTokens:       s.cfg.Limits.MaxDocumentAITokensPerUserPerDay,
			MaxCostMicroUSD: microUSD(s.cfg.Limits.MaxEstimatedDocumentAICostPerUserPerDayUSD),
		},
		ReservationTTL: time.Duration(s.cfg.DocumentAI.TimeoutMS)*time.Millisecond + time.Minute,
		Recorder:       s.telemetry,
	}), nil
}

func (s *Service) documentAIReconcileLoop(ctx context.Context) {
	if s == nil || s.st == nil {
		return
	}
	timeout := time.Duration(s.cfg.DocumentAI.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	interval := timeout / 2
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	if interval > 5*time.Minute {
		interval = 5 * time.Minute
	}
	reconcile := func() {
		now := time.Now().UTC()
		count, err := s.st.ReconcileRAGDocumentAIUsage(ctx, now, now.Add(-timeout-time.Minute), 100)
		fields := telemetry.Fields{
			Operation: "usage_reconcile", Transition: "reconcile", Outcome: "ok", SuccessCount: count,
		}
		if err != nil {
			fields.Outcome = "error"
			switch {
			case errors.Is(err, context.Canceled):
				fields.ErrorCode = "canceled"
			case errors.Is(err, context.DeadlineExceeded):
				fields.ErrorCode = "timeout"
			default:
				fields.ErrorCode = "store_error"
			}
		}
		telemetry.Emit(ctx, s.telemetry, telemetry.EventDocumentAIBudget, fields)
	}
	reconcile()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}

func (s *Service) MaxFileMB() int { return s.cfg.Limits.MaxFileMB }

// Config returns the immutable system RAG configuration snapshot captured at
// service construction. Callers must not mutate slice fields on the result.
func (s *Service) Config() config.RAGCfg { return s.cfg }

// Close releases optional backend resources. Worker shutdown is controlled by
// the context passed to Start; vector backends such as Milvus additionally own
// a client connection that should be closed during gateway shutdown.
func (s *Service) Close(ctx context.Context) error {
	closer, ok := s.vec.(interface{ Close(context.Context) error })
	if !ok {
		return nil
	}
	return closer.Close(ctx)
}

func (s *Service) kbMutex(kbID string) *sync.RWMutex {
	lock, _ := s.kbLocks.LoadOrStore(kbID, &sync.RWMutex{})
	return lock.(*sync.RWMutex)
}

func (s *Service) docMutex(docID string) *sync.Mutex {
	lock, _ := s.docLocks.LoadOrStore(docID, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *Service) resolveEmbedding(ctx context.Context, userID string) (config.RAGEmbeddingCfg, string) {
	if s.userCfg != nil {
		if c, ok := s.userCfg(ctx, userID); ok && c.Endpoint != "" && c.Model != "" && c.Dims > 0 {
			return c, "user"
		}
	}
	return s.cfg.Embedding, "system"
}

func (s *Service) embeddingConfigForKB(ctx context.Context, kb *store.RAGKBRecord) (config.RAGEmbeddingCfg, error) {
	if kb == nil {
		return config.RAGEmbeddingCfg{}, errors.New("embedding KB is nil")
	}
	var cfg config.RAGEmbeddingCfg
	switch kb.EmbedProvider {
	case "user":
		var ok bool
		fence, fairContext := fairIndexFenceFromContext(ctx)
		if s.workerMode == WorkerModeFair || fairContext {
			if s.fairStore == nil {
				return cfg, store.ErrFairQueueMySQLRequired
			}
			expectedWriter := s.fairStore.ExpectedWriterFingerprint()
			if !validFairWriterFingerprint(expectedWriter) ||
				(fairContext && (!validFairWriterFingerprint(fence.ExpectedWriterFingerprint) ||
					expectedWriter != fence.ExpectedWriterFingerprint)) {
				return cfg, store.ErrFairQueueWriterMismatch
			}
			record, err := s.fairStore.GetConfigByName(
				ctx, store.KindSetting, kb.UserID, "", "rag",
			)
			if errors.Is(err, store.ErrNotFound) {
				err = nil
			}
			if err != nil {
				return cfg, err
			}
			cfg, ok, err = embeddingConfigFromRecord(record)
			if err != nil {
				return cfg, err
			}
		} else {
			if s.userCfg == nil {
				return cfg, errors.New("KB 绑定的用户 embedding 配置不可用")
			}
			cfg, ok = s.userCfg(ctx, kb.UserID)
		}
		if !ok {
			return cfg, errors.New("KB 绑定的用户 embedding 配置不可用")
		}
	default:
		cfg = s.cfg.Embedding
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return cfg, errors.New("KB 绑定的 embedding endpoint 不可用")
	}
	return cfg, nil
}

func embeddingConfigFromRecord(record *store.ConfigRecord) (config.RAGEmbeddingCfg, bool, error) {
	if record == nil || len(record.Data) == 0 {
		return config.RAGEmbeddingCfg{}, false, nil
	}
	blob, err := json.Marshal(record.Data)
	if err != nil {
		return config.RAGEmbeddingCfg{}, false, err
	}
	var wrapper struct {
		Embedding config.RAGEmbeddingCfg `json:"embedding"`
	}
	if err := json.Unmarshal(blob, &wrapper); err != nil {
		return config.RAGEmbeddingCfg{}, false, err
	}
	cfg := wrapper.Embedding
	// Preserve the legacy API's direct embedding-object compatibility.
	if cfg.Endpoint == "" && cfg.Model == "" && cfg.Dims == 0 {
		if err := json.Unmarshal(blob, &cfg); err != nil {
			return config.RAGEmbeddingCfg{}, false, err
		}
	}
	if cfg.Endpoint == "" || cfg.Model == "" || cfg.Dims <= 0 {
		return config.RAGEmbeddingCfg{}, false, nil
	}
	return cfg, true, nil
}

func (s *Service) embedderForKB(ctx context.Context, kb *store.RAGKBRecord) (*embed.Client, error) {
	cfg, err := s.embeddingConfigForKB(ctx, kb)
	if err != nil {
		return nil, err
	}
	return embed.New(cfg.Endpoint, cfg.APIKey, kb.EmbedModel, kb.EmbedDims), nil
}

func (s *Service) requireActiveUser(ctx context.Context, userID string) error {
	user, err := s.st.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(user.Status, "active") {
		return ErrForbidden
	}
	return nil
}

type KBParsingOptions struct {
	ParseMode         config.ParseMode
	EnrichmentEnabled bool
}

func (s *Service) CreateKB(ctx context.Context, userID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	return s.CreateKBWithOptions(ctx, userID, name, description, chunkSize, chunkOverlap, KBParsingOptions{
		ParseMode: config.ParseModeStandard,
	})
}

func (s *Service) CreateKBWithOptions(
	ctx context.Context,
	userID, name, description string,
	chunkSize, chunkOverlap int,
	options KBParsingOptions,
) (*store.RAGKBRecord, error) {
	name = strings.TrimSpace(name)
	if userID == "" {
		return nil, ErrForbidden
	}
	if err := s.requireActiveUser(ctx, userID); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("知识库名称不能为空")
	}
	if !options.ParseMode.Valid() {
		return nil, errors.New("parseMode 必须是 standard 或 auto")
	}
	existing, err := s.st.ListRAGKBsByUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if len(existing) >= s.cfg.Limits.MaxKBsPerUser {
		return nil, fmt.Errorf("%w: 每用户最多 %d 个知识库", ErrQuota, s.cfg.Limits.MaxKBsPerUser)
	}
	embedCfg, provider := s.resolveEmbedding(ctx, userID)
	if embedCfg.Endpoint == "" || embedCfg.Model == "" || embedCfg.Dims <= 0 {
		return nil, errors.New("embedding 未配置，请先在系统或用户设置中配置")
	}
	if chunkSize <= 0 {
		chunkSize = 512
	}
	if chunkOverlap <= 0 || chunkOverlap >= chunkSize {
		chunkOverlap = min(64, chunkSize/8)
		if chunkOverlap <= 0 {
			chunkOverlap = 1
		}
	}

	kb := &store.RAGKBRecord{
		ID:                "kb_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
		UserID:            userID,
		Name:              name,
		Description:       strings.TrimSpace(description),
		EmbedProvider:     provider,
		EmbedModel:        embedCfg.Model,
		EmbedDims:         embedCfg.Dims,
		ChunkSize:         chunkSize,
		ChunkOverlap:      chunkOverlap,
		ParseMode:         string(options.ParseMode),
		EnrichmentEnabled: options.EnrichmentEnabled,
		Status:            store.RAGKBStatusProvisioning,
	}
	kbLock := s.kbMutex(kb.ID)
	kbLock.Lock()
	defer kbLock.Unlock()
	fence, err := s.st.BeginRAGKBProvisioning(
		ctx, kb, s.workerID+"-kb", s.leaseDuration, s.cfg.Limits.MaxKBsPerUser,
	)
	if err != nil {
		return nil, mapKBProvisioningStoreError(err, s.cfg.Limits.MaxKBsPerUser)
	}
	if err := s.ensureProvisionedKBCollection(ctx, kb, *fence); err != nil {
		if cleanupErr := s.abandonKBProvisioning(ctx, *fence); cleanupErr != nil {
			logLifecycleFailure("abandon_kb_provision", kb.ID, cleanupErr)
		}
		return nil, fmt.Errorf("创建向量 collection: %w", err)
	}
	if err := ctx.Err(); err != nil {
		if cleanupErr := s.abandonKBProvisioning(ctx, *fence); cleanupErr != nil {
			logLifecycleFailure("cancel_kb_provision", kb.ID, cleanupErr)
		}
		return nil, err
	}
	active, activated, err := s.st.ActivateRAGKBProvisioning(ctx, *fence)
	if err != nil || !activated {
		if cleanupErr := s.abandonKBProvisioning(ctx, *fence); cleanupErr != nil {
			logLifecycleFailure("finalize_kb_provision", kb.ID, cleanupErr)
		}
		if err != nil {
			return nil, mapKBProvisioningStoreError(err, s.cfg.Limits.MaxKBsPerUser)
		}
		return nil, errRAGKBProvisionFenceLost
	}
	return active, nil
}

// GetKB enforces ownership. An empty ownerID is reserved for explicitly
// privileged internal/admin paths and skips the ownership check.
func (s *Service) GetKB(ctx context.Context, ownerID, kbID string) (*store.RAGKBRecord, error) {
	kb, err := s.st.GetRAGKB(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ownerID != "" && kb.UserID != ownerID {
		return nil, ErrForbidden
	}
	return kb, nil
}

func (s *Service) ListKBs(ctx context.Context, userID string) ([]store.RAGKBRecord, error) {
	return s.st.ListRAGKBsByUser(ctx, userID)
}

func (s *Service) UpdateKB(ctx context.Context, ownerID, kbID, name, description string, chunkSize, chunkOverlap int) (*store.RAGKBRecord, error) {
	return s.updateKB(ctx, ownerID, kbID, name, description, chunkSize, chunkOverlap, nil)
}

func (s *Service) UpdateKBWithOptions(
	ctx context.Context,
	ownerID, kbID, name, description string,
	chunkSize, chunkOverlap int,
	options KBParsingOptions,
) (*store.RAGKBRecord, error) {
	if !options.ParseMode.Valid() {
		return nil, errors.New("parseMode 必须是 standard 或 auto")
	}
	return s.updateKB(ctx, ownerID, kbID, name, description, chunkSize, chunkOverlap, &options)
}

func (s *Service) updateKB(
	ctx context.Context,
	ownerID, kbID, name, description string,
	chunkSize, chunkOverlap int,
	options *KBParsingOptions,
) (*store.RAGKBRecord, error) {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return nil, err
	}
	if err := s.requireActiveUser(ctx, kb.UserID); err != nil {
		return nil, err
	}
	if !strings.EqualFold(kb.Status, "active") {
		return nil, errors.New("知识库正在删除中")
	}
	if strings.TrimSpace(name) != "" {
		kb.Name = strings.TrimSpace(name)
	}
	kb.Description = strings.TrimSpace(description)
	if chunkSize > 0 {
		kb.ChunkSize = chunkSize
	}
	if chunkOverlap > 0 && chunkOverlap < kb.ChunkSize {
		kb.ChunkOverlap = chunkOverlap
	}
	if kb.ChunkOverlap >= kb.ChunkSize {
		return nil, errors.New("chunkOverlap 必须小于 chunkSize")
	}
	if options != nil {
		kb.ParseMode = string(options.ParseMode)
		kb.EnrichmentEnabled = options.EnrichmentEnabled
	}
	if err := s.st.UpdateRAGKB(ctx, kb); err != nil {
		return nil, err
	}
	return kb, nil
}

func (s *Service) DeleteKB(ctx context.Context, ownerID, kbID string) error {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	return s.deleteKBRecord(ctx, kb)
}

func (s *Service) GetDocument(ctx context.Context, ownerID, kbID, docID string) (*store.RAGDocumentRecord, error) {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return nil, err
	}
	doc, err := s.st.GetRAGDocument(ctx, docID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && doc.KBID != kbID) {
		return nil, ErrNotFound
	}
	return doc, err
}

func (s *Service) ListDocuments(ctx context.Context, ownerID, kbID string) ([]store.RAGDocumentRecord, error) {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return nil, err
	}
	return s.st.ListRAGDocumentsByKB(ctx, kbID)
}
