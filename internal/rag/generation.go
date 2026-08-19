package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/config"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

type GenerationResolutionMode string

const (
	GenerationResolutionLegacy        GenerationResolutionMode = "legacy"
	GenerationResolutionShadow        GenerationResolutionMode = "shadow"
	GenerationResolutionAuthoritative GenerationResolutionMode = "authoritative"
)

type GenerationStore interface {
	ResolveActiveRAGKBGeneration(ctx context.Context, kbID string) (*store.RAGKBGenerationRecord, []store.RAGGenerationDocumentRecord, error)
	ListRAGDocumentsByKB(ctx context.Context, kbID string) ([]store.RAGDocumentRecord, error)
}

type GenerationMismatchReporter func(ctx context.Context, kbID, reason string)

// GenerationResolver keeps the legacy path as a safe fallback while shadow
// reads validate that SQL's active document versions and generation mapping
// describe the same retrieval-visible corpus.
type GenerationResolver struct {
	store  GenerationStore
	mode   GenerationResolutionMode
	report GenerationMismatchReporter
}

func NewGenerationResolver(st GenerationStore, mode GenerationResolutionMode, report GenerationMismatchReporter) *GenerationResolver {
	if mode != GenerationResolutionShadow && mode != GenerationResolutionAuthoritative {
		mode = GenerationResolutionLegacy
	}
	if report == nil {
		report = func(ctx context.Context, kbID, reason string) {
			slog.WarnContext(ctx, "rag generation mapping mismatch; using legacy collection", "kb_id", kbID, "reason", reason)
		}
	}
	return &GenerationResolver{store: st, mode: mode, report: report}
}

func (r *GenerationResolver) ResolveCollection(ctx context.Context, kbID string) (vector.CollectionKey, error) {
	legacy, err := vector.LegacyCollectionKey(kbID)
	if err != nil {
		return "", err
	}
	if r == nil || r.mode == GenerationResolutionLegacy || r.store == nil {
		return legacy, nil
	}
	generation, mapped, err := r.store.ResolveActiveRAGKBGeneration(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return legacy, nil
	}
	if err != nil {
		return "", err
	}
	if r.mode == GenerationResolutionAuthoritative {
		if generation == nil || generation.KBID != kbID || generation.Status != store.RAGGenerationActive {
			r.report(ctx, kbID, "active generation identity/status conflict")
			return legacy, nil
		}
		key, keyErr := vector.CollectionKeyFromPersistence(generation.CollectionKey)
		if keyErr != nil {
			r.report(ctx, kbID, "invalid persisted collection key")
			return legacy, nil
		}
		return key, nil
	}
	documents, err := r.store.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		r.report(ctx, kbID, "legacy active-version read failed: "+err.Error())
		return legacy, nil
	}
	activeVersions := make(map[string]int64, len(documents))
	for _, document := range documents {
		if document.ActiveVersion > 0 && !strings.EqualFold(document.Status, store.RAGDocumentStatusDeleting) {
			activeVersions[document.ID] = document.ActiveVersion
		}
	}
	if reason := validateGenerationMapping(kbID, generation, mapped, activeVersions); reason != "" {
		r.report(ctx, kbID, reason)
	}
	return legacy, nil
}

func (r *GenerationResolver) ResolveSearchCollection(ctx context.Context, kbID string, activeVersions map[string]int64) (vector.CollectionKey, error) {
	legacy, err := vector.LegacyCollectionKey(kbID)
	if err != nil {
		return "", err
	}
	if r == nil || r.mode == GenerationResolutionLegacy || r.store == nil {
		return legacy, nil
	}
	generation, mapped, err := r.store.ResolveActiveRAGKBGeneration(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return legacy, nil
	}
	if err != nil {
		return "", err
	}
	return r.resolveValidated(ctx, kbID, legacy, generation, mapped, activeVersions)
}

func (r *GenerationResolver) resolveValidated(ctx context.Context, kbID string, legacy vector.CollectionKey, generation *store.RAGKBGenerationRecord, mapped []store.RAGGenerationDocumentRecord, activeVersions map[string]int64) (vector.CollectionKey, error) {
	if reason := validateGenerationMapping(kbID, generation, mapped, activeVersions); reason != "" {
		r.report(ctx, kbID, reason)
		return legacy, nil
	}
	if r.mode != GenerationResolutionAuthoritative {
		return legacy, nil
	}
	key, err := vector.CollectionKeyFromPersistence(generation.CollectionKey)
	if err != nil {
		r.report(ctx, kbID, "invalid persisted collection key")
		return legacy, nil
	}
	return key, nil
}

func validateGenerationMapping(kbID string, generation *store.RAGKBGenerationRecord, mapped []store.RAGGenerationDocumentRecord, activeVersions map[string]int64) string {
	if generation == nil || generation.KBID != kbID || generation.Status != store.RAGGenerationActive {
		return "active generation identity/status conflict"
	}
	if len(activeVersions) != len(mapped) {
		return fmt.Sprintf("document count conflict: legacy=%d generation=%d", len(activeVersions), len(mapped))
	}
	seen := make(map[string]struct{}, len(mapped))
	for _, document := range mapped {
		if document.Status != store.RAGGenerationDocumentReady {
			return "generation contains non-ready document " + document.DocID
		}
		if _, duplicate := seen[document.DocID]; duplicate {
			return "generation contains duplicate document " + document.DocID
		}
		seen[document.DocID] = struct{}{}
		if activeVersions[document.DocID] != document.DocVersion {
			return fmt.Sprintf("active version conflict for %s: legacy=%d generation=%d", document.DocID, activeVersions[document.DocID], document.DocVersion)
		}
	}
	return ""
}

var ErrEvaluationGenerationBuilding = errors.New("evaluation generation is already building")

type EvaluationPipelineDocument struct {
	ID, FileName, MediaType, ObjectKey, SHA256 string
	SizeBytes                                  int64
}

type EvaluationGenerationProgress struct {
	Stage              string
	DocumentsCompleted int64
	DocumentsTotal     int64
	ChunksCompleted    int64
	Reused             bool
}

type EvaluationPipelineRequest struct {
	Target              PipelineTarget
	Documents           []EvaluationPipelineDocument
	DocumentConcurrency int
	Ingestion           config.RAGIngestionPolicyData
	Contract            rageval.GenerationContract
	Embedding           config.RAGEmbeddingCfg
	DocumentAIBudget    *vision.TaskDocumentAIBudget
	BypassParser        bool
	Progress            func(EvaluationGenerationProgress) error
}

type EvaluationPipelineResult struct {
	DocumentCount int64
	ChunkCount    int64
}

// EvaluationGenerationPipeline is deliberately evaluation-specific. Its drop
// operation accepts only an explicit evaluation target, preventing callers
// from reaching the production KB lifecycle path with a guessed identifier.
type EvaluationGenerationPipeline interface {
	BuildEvaluationGeneration(context.Context, EvaluationPipelineRequest) (EvaluationPipelineResult, error)
	DropEvaluationGeneration(context.Context, PipelineTarget) error
}

type EvaluationGenerationStore interface {
	AcquireRAGEvalGenerationForRun(context.Context, store.RAGEvalGenerationAcquireRequest) (*store.RAGEvalGenerationAcquireResult, error)
	HeartbeatRAGEvalGeneration(context.Context, store.RAGEvalGenerationFence, time.Duration) (bool, error)
	MarkRAGEvalGenerationReady(context.Context, store.RAGEvalGenerationFence, int64, int64, time.Duration) (bool, error)
	MarkRAGEvalGenerationFailed(context.Context, store.RAGEvalGenerationFence, string, string, time.Duration) (bool, error)
	ReleaseRAGEvalGenerationForRun(context.Context, string) (bool, error)
	ClaimRAGEvalGenerationGC(context.Context, time.Time, string, time.Duration) (*store.RAGEvalGenerationFence, bool, error)
	FinishRAGEvalGenerationGC(context.Context, store.RAGEvalGenerationFence) (bool, error)
}

type EvaluationGenerationBuilder struct {
	store        EvaluationGenerationStore
	pipeline     EvaluationGenerationPipeline
	worker       string
	lease        time.Duration
	readyTTL     time.Duration
	failureTTL   time.Duration
	heartbeatMin time.Duration
}

func NewEvaluationGenerationBuilder(generationStore EvaluationGenerationStore, pipeline EvaluationGenerationPipeline, worker string, lease, readyTTL time.Duration) (*EvaluationGenerationBuilder, error) {
	worker = strings.TrimSpace(worker)
	if generationStore == nil || pipeline == nil || worker == "" || lease <= 0 || readyTTL <= 0 {
		return nil, errors.New("evaluation generation builder dependencies are invalid")
	}
	return &EvaluationGenerationBuilder{
		store: generationStore, pipeline: pipeline, worker: worker, lease: lease, readyTTL: readyTTL,
		failureTTL: time.Hour, heartbeatMin: 50 * time.Millisecond,
	}, nil
}

type EvaluationGenerationBuildRequest struct {
	OwnerID             string
	RunID               string
	DatasetVersion      *store.RAGEvalDatasetVersionRecord
	Documents           []store.RAGEvalCorpusDocumentRecord
	DocumentConcurrency int
	Ingestion           config.RAGIngestionPolicyData
	Contract            rageval.GenerationContract
	Embedding           config.RAGEmbeddingCfg
	DocumentAIBudget    *vision.TaskDocumentAIBudget
	BypassParser        bool
	Progress            func(EvaluationGenerationProgress) error
	// EmbeddingContractFingerprint is returned by the trusted provider
	// resolver together with Embedding; it is never derived from API input.
	EmbeddingContractFingerprint string
}

func (b *EvaluationGenerationBuilder) Build(ctx context.Context, request EvaluationGenerationBuildRequest) (*store.RAGEvalGenerationRecord, bool, error) {
	if b == nil || b.store == nil || b.pipeline == nil || request.DatasetVersion == nil ||
		request.DatasetVersion.Status != store.RAGEvalDatasetReady || strings.TrimSpace(request.OwnerID) == "" || strings.TrimSpace(request.RunID) == "" {
		return nil, false, errors.New("evaluation generation build request is invalid")
	}
	if int64(len(request.Documents)) != request.DatasetVersion.DocumentCount {
		return nil, false, errors.New("evaluation generation corpus count does not match READY dataset version")
	}
	documentFingerprints := make([]rageval.GenerationDocumentFingerprint, len(request.Documents))
	pipelineDocuments := make([]EvaluationPipelineDocument, len(request.Documents))
	for index, document := range request.Documents {
		if document.DatasetVersionID != request.DatasetVersion.ID || strings.TrimSpace(document.ObjectKey) == "" || document.SizeBytes < 0 {
			return nil, false, errors.New("evaluation generation corpus document is invalid")
		}
		documentFingerprints[index] = rageval.GenerationDocumentFingerprint{
			ID: document.ExternalID, FileName: document.FileName, MediaType: document.MediaType,
			SHA256: document.SHA256, SizeBytes: document.SizeBytes,
		}
		pipelineDocuments[index] = EvaluationPipelineDocument{
			ID: document.ExternalID, FileName: document.FileName, MediaType: document.MediaType,
			ObjectKey: document.ObjectKey, SHA256: document.SHA256, SizeBytes: document.SizeBytes,
		}
	}
	effectiveIngestion := request.Ingestion
	effectiveContract := request.Contract
	if request.BypassParser {
		effectiveIngestion.ParseMode = config.ParseModeStandard
		effectiveIngestion.ParserEngine = ""
		effectiveIngestion.DocumentAI.VisionModel = ""
		effectiveIngestion.DocumentAI.VisionPromptVersion = ""
		effectiveContract.ParserProtocolVersion = "canonical-text-v1"
		effectiveContract.ParserEngineVersion = "canonical-text-v1"
	}
	ingestionFingerprint, err := rageval.IngestionFingerprint(effectiveIngestion)
	if err != nil {
		return nil, false, err
	}
	fingerprint, err := rageval.GenerationFingerprint(request.DatasetVersion.ID, request.DatasetVersion.CorpusSHA256, documentFingerprints, effectiveIngestion, effectiveContract)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(request.Embedding.Endpoint) == "" || strings.TrimSpace(request.Embedding.Model) != strings.TrimSpace(request.Ingestion.Embedding.Model) ||
		request.Embedding.Dims != request.Ingestion.Embedding.Dims ||
		strings.TrimSpace(request.EmbeddingContractFingerprint) != strings.TrimSpace(request.Ingestion.Embedding.ContractFingerprint) {
		return nil, false, errors.New("resolved embedding binding does not match ingestion contract")
	}
	generationID := "reg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	target, err := NewEvaluationPipelineTarget(request.OwnerID, request.RunID, request.DatasetVersion.ID, generationID)
	if err != nil {
		return nil, false, err
	}
	acquired, err := b.store.AcquireRAGEvalGenerationForRun(ctx, store.RAGEvalGenerationAcquireRequest{
		RunID: request.RunID, DatasetVersionID: request.DatasetVersion.ID, Fingerprint: fingerprint,
		CorpusFingerprint: request.DatasetVersion.CorpusSHA256, IngestionFingerprint: ingestionFingerprint,
		NewGenerationID: generationID, CollectionKey: string(target.CollectionKey), ObjectPrefix: target.ObjectPrefix,
		EmbeddingModel: effectiveIngestion.Embedding.Model, EmbeddingDims: effectiveIngestion.Embedding.Dims,
		Worker: b.worker, Lease: b.lease, TTL: b.readyTTL,
	})
	if err != nil {
		return nil, false, err
	}
	if acquired == nil || acquired.Generation == nil {
		return nil, false, errors.New("evaluation generation acquire returned no generation")
	}
	if acquired.Reused {
		if request.Progress != nil {
			if err := request.Progress(EvaluationGenerationProgress{Stage: "reusing_generation", DocumentsCompleted: acquired.Generation.DocumentCount,
				DocumentsTotal: request.DatasetVersion.DocumentCount, ChunksCompleted: acquired.Generation.ChunkCount, Reused: true}); err != nil {
				_, _ = b.store.ReleaseRAGEvalGenerationForRun(context.Background(), request.RunID)
				return acquired.Generation, true, err
			}
		}
		return acquired.Generation, true, nil
	}
	if !acquired.Claimed || acquired.Fence == nil {
		_, _ = b.store.ReleaseRAGEvalGenerationForRun(context.Background(), request.RunID)
		return acquired.Generation, false, ErrEvaluationGenerationBuilding
	}
	target, err = NewEvaluationPipelineTarget(request.OwnerID, request.RunID, request.DatasetVersion.ID, acquired.Generation.ID)
	if err != nil || string(target.CollectionKey) != acquired.Generation.CollectionKey || target.ObjectPrefix != acquired.Generation.ObjectPrefix {
		_, _ = b.store.ReleaseRAGEvalGenerationForRun(context.Background(), request.RunID)
		if err == nil {
			err = errors.New("persisted evaluation generation namespace mismatch")
		}
		return acquired.Generation, false, err
	}

	buildCtx, cancelBuild := context.WithCancel(ctx)
	var fenceLost atomic.Bool
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		interval := b.lease / 3
		if interval < b.heartbeatMin {
			interval = b.heartbeatMin
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-buildCtx.Done():
				return
			case <-ticker.C:
				ok, heartbeatErr := b.store.HeartbeatRAGEvalGeneration(buildCtx, *acquired.Fence, b.lease)
				if heartbeatErr != nil || !ok {
					fenceLost.Store(true)
					cancelBuild()
					return
				}
			}
		}
	}()
	buildResult, buildErr := b.pipeline.BuildEvaluationGeneration(buildCtx, EvaluationPipelineRequest{
		Target: target, Documents: pipelineDocuments, DocumentConcurrency: request.DocumentConcurrency, Ingestion: effectiveIngestion,
		Contract: effectiveContract, Embedding: request.Embedding, DocumentAIBudget: request.DocumentAIBudget,
		BypassParser: request.BypassParser, Progress: request.Progress,
	})
	cancelBuild()
	<-heartbeatDone
	if fenceLost.Load() {
		buildErr = errors.Join(buildErr, store.ErrRAGEvalFenceLost)
	}
	if buildErr != nil {
		_, _ = b.store.MarkRAGEvalGenerationFailed(context.Background(), *acquired.Fence, "pipeline_error", buildErr.Error(), b.failureTTL)
		_, _ = b.store.ReleaseRAGEvalGenerationForRun(context.Background(), request.RunID)
		return acquired.Generation, false, buildErr
	}
	ready, err := b.store.MarkRAGEvalGenerationReady(ctx, *acquired.Fence, buildResult.DocumentCount, buildResult.ChunkCount, b.readyTTL)
	if err != nil || !ready {
		_, _ = b.store.ReleaseRAGEvalGenerationForRun(context.Background(), request.RunID)
		if err == nil {
			err = store.ErrRAGEvalFenceLost
		}
		return acquired.Generation, false, err
	}
	acquired.Generation.Status = store.RAGEvalGenerationReady
	acquired.Generation.DocumentCount = buildResult.DocumentCount
	acquired.Generation.ChunkCount = buildResult.ChunkCount
	return acquired.Generation, false, nil
}

func (b *EvaluationGenerationBuilder) ReleaseRun(ctx context.Context, runID string) (bool, error) {
	return b.store.ReleaseRAGEvalGenerationForRun(ctx, runID)
}

// GarbageCollect uses only the evaluation pipeline drop boundary. A
// production KB collection cannot be represented by this target type.
func (b *EvaluationGenerationBuilder) GarbageCollect(ctx context.Context, before time.Time, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	cleaned := 0
	for cleaned < limit {
		fence, claimed, err := b.store.ClaimRAGEvalGenerationGC(ctx, before, b.worker, b.lease)
		if err != nil || !claimed {
			return cleaned, err
		}
		target := PipelineTarget{Kind: PipelineTargetEvaluation, GenerationID: fence.GenerationID,
			CollectionKey: vector.CollectionKey(fence.CollectionKey), ObjectPrefix: fence.ObjectPrefix}
		// GC reconstructs no logical owner/run identity; it validates the
		// persisted physical namespace directly before invoking eval-only drop.
		if !strings.HasPrefix(target.ObjectPrefix, "rag-eval/generations/") || !strings.HasPrefix(string(target.CollectionKey), "eval_") {
			return cleaned, errors.New("unsafe persisted evaluation generation namespace")
		}
		if err := b.pipeline.DropEvaluationGeneration(ctx, target); err != nil {
			return cleaned, err
		}
		finished, err := b.store.FinishRAGEvalGenerationGC(ctx, *fence)
		if err != nil || !finished {
			if err == nil {
				err = store.ErrRAGEvalFenceLost
			}
			return cleaned, err
		}
		cleaned++
	}
	return cleaned, nil
}
