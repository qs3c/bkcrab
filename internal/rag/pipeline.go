package rag

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/qs3c/bkcrab/internal/config"
	ragassets "github.com/qs3c/bkcrab/internal/rag/assets"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

var errIndexFenceLost = errors.New("RAG index fence lost")

const (
	indexTaskPendingLimitCode       = "pending_limit"
	indexTaskReindexRateLimitCode   = "reindex_rate_limit"
	indexTaskRejectedTelemetryState = "rejected"
	fairClaimFinalizeTimeout        = 5 * time.Second
)

type PipelineTargetKind string

const PipelineTargetEvaluation PipelineTargetKind = "evaluation"

// PipelineTarget is the explicit authorization and isolation boundary for a
// non-production indexing execution. It never carries a synthetic KB ID.
type PipelineTarget struct {
	Kind             PipelineTargetKind
	OwnerID          string
	RunID            string
	DatasetVersionID string
	GenerationID     string
	CollectionKey    vector.CollectionKey
	ObjectPrefix     string
}

func NewEvaluationPipelineTarget(ownerID, runID, datasetVersionID, generationID string) (PipelineTarget, error) {
	for _, value := range []string{ownerID, runID, datasetVersionID, generationID} {
		if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) || strings.ContainsAny(value, `/\\`) || len(value) > 255 {
			return PipelineTarget{}, errors.New("invalid evaluation pipeline target identity")
		}
	}
	collectionKey, err := vector.EvaluationGenerationCollectionKey(datasetVersionID, generationID)
	if err != nil {
		return PipelineTarget{}, err
	}
	return PipelineTarget{
		Kind: PipelineTargetEvaluation, OwnerID: ownerID, RunID: runID, DatasetVersionID: datasetVersionID,
		GenerationID: generationID, CollectionKey: collectionKey,
		ObjectPrefix: path.Join("rag-eval", "generations", generationID),
	}, nil
}

func (t PipelineTarget) ValidateEvaluation() error {
	if t.Kind != PipelineTargetEvaluation {
		return errors.New("pipeline target is not an evaluation target")
	}
	expected, err := NewEvaluationPipelineTarget(t.OwnerID, t.RunID, t.DatasetVersionID, t.GenerationID)
	if err != nil {
		return err
	}
	if t.CollectionKey != expected.CollectionKey || t.ObjectPrefix != expected.ObjectPrefix {
		return errors.New("evaluation pipeline target physical namespace mismatch")
	}
	return nil
}

// BuildEvaluationGeneration runs the same concrete parser, splitter,
// enrichment, embedding and vector components owned by the production RAG
// service, but writes only to the explicit evaluation target.
func (s *Service) BuildEvaluationGeneration(ctx context.Context, request EvaluationPipelineRequest) (EvaluationPipelineResult, error) {
	if s == nil || s.parser == nil || s.vec == nil || s.obj == nil {
		return EvaluationPipelineResult{}, errors.New("evaluation pipeline dependencies are unavailable")
	}
	if err := request.Target.ValidateEvaluation(); err != nil {
		return EvaluationPipelineResult{}, err
	}
	if err := request.Ingestion.Validate(); err != nil {
		return EvaluationPipelineResult{}, err
	}
	if err := request.Contract.Validate(); err != nil {
		return EvaluationPipelineResult{}, err
	}
	request.Ingestion.ParserEngine = strings.ToLower(strings.TrimSpace(request.Ingestion.ParserEngine))
	if request.Ingestion.ParserEngine == "" {
		request.Ingestion.ParserEngine = strings.ToLower(strings.TrimSpace(s.cfg.ParserSidecar.Engine))
	}
	if request.Ingestion.ParserEngine != "markitdown" && request.Ingestion.ParserEngine != "anydoc" {
		return EvaluationPipelineResult{}, fmt.Errorf("evaluation parser engine %q is not supported", request.Ingestion.ParserEngine)
	}
	for _, sourceDocument := range request.Documents {
		if !parse.SupportedExtForParser(sourceDocument.FileName, request.Ingestion.ParserEngine) {
			return EvaluationPipelineResult{}, fmt.Errorf("evaluation parser %s does not support %s", request.Ingestion.ParserEngine, sourceDocument.FileName)
		}
		extension := strings.TrimPrefix(strings.ToLower(path.Ext(sourceDocument.FileName)), ".")
		if extension != "md" && extension != "markdown" && extension != "txt" && s.parserAvailable != nil && !s.parserAvailable(request.Ingestion.ParserEngine) {
			return EvaluationPipelineResult{}, fmt.Errorf("evaluation parser %s is currently unavailable", request.Ingestion.ParserEngine)
		}
	}
	if strings.TrimSpace(request.Embedding.Endpoint) == "" || request.Embedding.Model != request.Ingestion.Embedding.Model ||
		request.Embedding.Dims != request.Ingestion.Embedding.Dims {
		return EvaluationPipelineResult{}, errors.New("evaluation embedding binding does not match ingestion profile")
	}
	if request.Ingestion.DocumentAI.VisionModel != "" && request.Ingestion.DocumentAI.VisionModel != strings.TrimSpace(s.cfg.DocumentAI.VisionModel) {
		return EvaluationPipelineResult{}, errors.New("evaluation vision model is not available in the resolved service contract")
	}
	if request.Ingestion.EnrichmentEnabled && (request.Ingestion.DocumentAI.TextModel == "" ||
		request.Ingestion.DocumentAI.TextModel != strings.TrimSpace(s.cfg.DocumentAI.TextModel)) {
		return EvaluationPipelineResult{}, errors.New("evaluation enrichment model is not available in the resolved service contract")
	}
	stageStarted := time.Now()
	if err := s.vec.EnsureCollection(ctx, request.Target.CollectionKey, request.Ingestion.Embedding.Dims); err != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, Operation: "eval_milvus", Outcome: "error", Duration: time.Since(stageStarted)})
		return EvaluationPipelineResult{}, fmt.Errorf("prepare evaluation collection: %w", err)
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, Operation: "eval_milvus", Outcome: "ok", Duration: time.Since(stageStarted)})
	embedder := embed.New(request.Embedding.Endpoint, request.Embedding.APIKey, request.Embedding.Model, request.Embedding.Dims)
	var documentCount atomic.Int64
	var chunkCount atomic.Int64
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(max(1, request.DocumentConcurrency))
	for _, sourceDocument := range request.Documents {
		sourceDocument := sourceDocument
		group.Go(func() error {
			chunks, err := s.buildEvaluationDocument(groupCtx, request, sourceDocument, embedder)
			if err != nil {
				return err
			}
			documentCount.Add(1)
			chunkCount.Add(int64(chunks))
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return EvaluationPipelineResult{DocumentCount: documentCount.Load(), ChunkCount: chunkCount.Load()}, err
	}
	result := EvaluationPipelineResult{DocumentCount: documentCount.Load(), ChunkCount: chunkCount.Load()}
	manifest, err := json.Marshal(struct {
		GenerationID string `json:"generationId"`
		Documents    int64  `json:"documents"`
		Chunks       int64  `json:"chunks"`
	}{request.Target.GenerationID, result.DocumentCount, result.ChunkCount})
	if err != nil {
		return result, err
	}
	manifestKey := path.Join(request.Target.ObjectPrefix, "manifest.json")
	if err := s.obj.Put(ctx, manifestKey, bytes.NewReader(manifest), int64(len(manifest)), "application/json"); err != nil {
		return result, fmt.Errorf("write evaluation generation manifest: %w", err)
	}
	return result, nil
}

func (s *Service) buildEvaluationDocument(ctx context.Context, request EvaluationPipelineRequest, sourceDocument EvaluationPipelineDocument, embedder *embed.Client) (int, error) {
	if strings.TrimSpace(sourceDocument.ID) == "" || strings.TrimSpace(sourceDocument.ObjectKey) == "" || sourceDocument.SizeBytes < 0 {
		return 0, errors.New("evaluation pipeline document is invalid")
	}
	stageStarted := time.Now()
	artifact, artifactErr := s.loadOrBuildEvaluationArtifact(ctx, request, sourceDocument)
	if artifactErr != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, DocID: sourceDocument.ID, Operation: "eval_parser", Outcome: "error", Duration: time.Since(stageStarted)})
		return 0, artifactErr
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, DocID: sourceDocument.ID, Operation: "eval_parser", Outcome: "ok", Duration: time.Since(stageStarted), ItemCount: 1})
	chunks, _, artifactErr := s.buildSearchChunks(ctx, artifact, searchChunkBuildOptions{
		ChunkSize: request.Ingestion.ChunkSize, ChunkOverlap: request.Ingestion.ChunkOverlap,
		EnrichmentEnabled: request.Ingestion.EnrichmentEnabled, TextModel: request.Ingestion.DocumentAI.TextModel,
		Scope: enrich.CacheScope{UserID: request.Target.OwnerID, KBID: "eval_" + request.Target.GenerationID,
			DocID: sourceDocument.ID, IndexFingerprint: request.Target.GenerationID},
		Budget: request.DocumentAIBudget,
	})
	if artifactErr != nil {
		return 0, fmt.Errorf("build evaluation document %s chunks: %w", sourceDocument.ID, artifactErr)
	}
	texts := make([]string, len(chunks))
	for index := range chunks {
		texts[index] = chunks[index].SearchContent
	}
	stageStarted = time.Now()
	vectors, artifactErr := embedder.Embed(ctx, texts)
	if artifactErr != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, DocID: sourceDocument.ID, Operation: "eval_embedding", Outcome: "error", Duration: time.Since(stageStarted), ItemCount: len(texts), Model: request.Embedding.Model})
		return 0, fmt.Errorf("embed evaluation document %s: %w", sourceDocument.ID, artifactErr)
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, DocID: sourceDocument.ID, Operation: "eval_embedding", Outcome: "ok", Duration: time.Since(stageStarted), ItemCount: len(texts), Model: request.Embedding.Model})
	if len(vectors) != len(chunks) {
		return 0, fmt.Errorf("evaluation embedding vector count mismatch for %s", sourceDocument.ID)
	}
	vectorChunks := make([]vector.ChunkData, len(chunks))
	for index, chunk := range chunks {
		pageNum := 0
		if chunk.Location.Kind == document.LocationPage {
			pageNum = chunk.Location.Index
		}
		vectorChunks[index] = vector.ChunkData{
			DocID: sourceDocument.ID, Index: chunk.Index, Content: chunk.RawContent,
			SearchContent: chunk.SearchContent, SectionTitle: chunk.SectionTitle, PageNum: pageNum,
			DocVersion: 1, Vector: vectors[index],
		}
	}
	stageStarted = time.Now()
	for start := 0; start < len(vectorChunks); start += pipelineStageBatchSize {
		end := min(start+pipelineStageBatchSize, len(vectorChunks))
		if err := s.vec.UpsertChunks(ctx, request.Target.CollectionKey, vectorChunks[start:end]); err != nil {
			telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, DocID: sourceDocument.ID, Operation: "eval_milvus", Outcome: "error", Duration: time.Since(stageStarted), ItemCount: len(vectorChunks)})
			return 0, fmt.Errorf("index evaluation document %s: %w", sourceDocument.ID, err)
		}
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventEvalStage, telemetry.Fields{RunID: request.Target.RunID, DocID: sourceDocument.ID, Operation: "eval_milvus", Outcome: "ok", Duration: time.Since(stageStarted), ItemCount: len(vectorChunks)})
	return len(chunks), nil
}

func (s *Service) loadOrBuildEvaluationArtifact(ctx context.Context, request EvaluationPipelineRequest, sourceDocument EvaluationPipelineDocument) (*document.ParsedArtifact, error) {
	fingerprint, err := rageval.CorpusArtifactFingerprint(rageval.GenerationDocumentFingerprint{
		ID: sourceDocument.ID, FileName: sourceDocument.FileName, MediaType: sourceDocument.MediaType,
		SHA256: sourceDocument.SHA256, SizeBytes: sourceDocument.SizeBytes,
	}, request.Ingestion, request.Contract)
	if err != nil {
		return nil, err
	}
	artifactKey := path.Join("rag-eval", "artifacts", fingerprint+".json")
	reader, err := s.obj.Get(ctx, artifactKey)
	if err == nil {
		artifact, decodeErr := document.DecodeArtifact(reader, int64(max(1, s.cfg.Limits.MaxExtractedBytes)))
		closeErr := reader.Close()
		if decodeErr != nil || closeErr != nil {
			return nil, errors.Join(decodeErr, closeErr)
		}
		if artifact.Source.DocID != sourceDocument.ID || artifact.Source.SHA256 != strings.ToLower(sourceDocument.SHA256) || artifact.Source.FileName != sourceDocument.FileName {
			return nil, errors.New("cached evaluation artifact source mismatch")
		}
		return artifact, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read evaluation artifact cache: %w", err)
	}
	format := strings.TrimPrefix(strings.ToLower(path.Ext(sourceDocument.FileName)), ".")
	source := document.Source{
		DocID: sourceDocument.ID, FileName: sourceDocument.FileName, Format: format,
		ParserEngine: request.Ingestion.ParserEngine,
		Size:         sourceDocument.SizeBytes, SHA256: strings.ToLower(sourceDocument.SHA256),
		Open: func(openCtx context.Context) (io.ReadCloser, error) {
			return s.obj.Get(openCtx, sourceDocument.ObjectKey)
		},
	}
	parsed, err := s.parser.Parse(ctx, source, parse.ParseOptions{
		Mode: request.Ingestion.ParseMode, ParserVersion: request.Contract.ParserEngineVersion,
		PageTranscriber: s.pageVision, ImageTranscriber: s.imageVision, DocumentAIBudget: request.DocumentAIBudget,
		VisionScope: vision.CacheScope{UserID: request.Target.OwnerID, KBID: "eval_" + request.Target.GenerationID,
			DocID: sourceDocument.ID, ParseFingerprint: fingerprint},
	})
	if err != nil {
		if parsed != nil {
			err = errors.Join(err, parsed.Close())
		}
		return nil, err
	}
	if parsed == nil {
		return nil, errors.New("evaluation parser returned nil document")
	}
	defer parsed.Close()
	if err := normalizeParsedDocument(parsed); err != nil {
		return nil, err
	}
	artifact, err := document.Canonicalize(parsed, "图片（未进行视觉识别）")
	if err != nil {
		return nil, err
	}
	encoded, err := document.EncodeArtifact(artifact, int64(max(1, s.cfg.Limits.MaxExtractedBytes)))
	if err != nil {
		return nil, err
	}
	if err := s.obj.Put(ctx, artifactKey, bytes.NewReader(encoded), int64(len(encoded)), "application/json"); err != nil {
		return nil, fmt.Errorf("write evaluation artifact cache: %w", err)
	}
	return artifact, nil
}

func (s *Service) DropEvaluationGeneration(ctx context.Context, target PipelineTarget) error {
	if s == nil || s.vec == nil || s.obj == nil || target.Kind != PipelineTargetEvaluation ||
		!strings.HasPrefix(string(target.CollectionKey), "eval_") || target.GenerationID == "" ||
		target.ObjectPrefix != path.Join("rag-eval", "generations", target.GenerationID) {
		return errors.New("unsafe evaluation generation drop target")
	}
	if err := s.vec.DropCollection(ctx, target.CollectionKey); err != nil {
		return err
	}
	return s.obj.DeletePrefix(ctx, target.ObjectPrefix+"/")
}

// Start launches bounded durable workers. The in-process channel only reduces
// latency; every wake makes a SQL claim, and the periodic pump makes a dropped
// wake (including a full channel) recover without a process restart.
func (s *Service) Start(ctx context.Context) {
	s.startOnce.Do(func() {
		if s.workerMode == WorkerModeLegacy {
			for i := 0; i < s.workerCount; i++ {
				go s.worker(ctx)
			}
			go s.taskPump(ctx)
			s.wakeWorkers()
		}
		go s.documentAIReconcileLoop(ctx)
		go s.lifecycleLoop(ctx)
		go s.policySyncLoop(ctx)
	})
}

func (s *Service) taskPump(ctx context.Context) {
	interval := s.pollInterval
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.wakeWorkers()
		}
	}
}

func (s *Service) wakeWorkers() {
	for i := 0; i < s.workerCount; i++ {
		s.scheduleTask(0)
	}
}

// recoverTasks is retained as a compatibility shim for callers/tests from the
// pre-lease worker. Recovery is now a durable SQL claim, not a one-time list.
func (s *Service) recoverTasks(context.Context) { s.wakeWorkers() }

func (s *Service) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.tasks:
			s.claimAvailable(ctx)
		}
	}
}

func (s *Service) claimAvailable(ctx context.Context) {
	for ctx.Err() == nil {
		claim, err := s.st.ClaimRAGIndexTask(ctx, s.workerID, s.leaseDuration)
		if err != nil {
			telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
				Transition: "claim", Outcome: "error", ErrorCode: "store_error",
			})
			if ctx.Err() == nil {
				slog.Error("rag: durable index claim failed", "worker", s.workerID, "error", err)
			}
			return
		}
		if claim == nil {
			return
		}
		if err := s.runClaim(ctx, claim); err != nil && ctx.Err() == nil {
			slog.Error("rag: index claim execution failed", "task", claim.Fence.TaskID, "error", err)
		}
	}
}

// UploadDocument validates and persists an original document, its immutable
// version snapshot, and its durable task in one relational transaction.
func (s *Service) UploadDocument(ctx context.Context, ownerID, kbID, fileName string, r io.Reader, size int64) (*store.RAGDocumentRecord, error) {
	return s.UploadDocumentWithParser(ctx, ownerID, kbID, fileName, s.cfg.ParserSidecar.Engine, r, size)
}

// UploadDocumentWithParser pins the selected conversion engine to the source
// document. Reindexing therefore reproduces the same parser contract instead
// of silently following a later system-default change.
func (s *Service) UploadDocumentWithParser(ctx context.Context, ownerID, kbID, fileName, parserEngine string, r io.Reader, size int64) (*store.RAGDocumentRecord, error) {
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
	if err := s.ensureNoPolicySync(ctx, kbID); err != nil {
		return nil, err
	}
	if kb.Status != "active" {
		return nil, errors.New("知识库正在删除中")
	}
	fileName = strings.TrimSpace(fileName)
	parserEngine = strings.ToLower(strings.TrimSpace(parserEngine))
	if parserEngine == "" {
		parserEngine = s.cfg.ParserSidecar.Engine
	}
	if parserEngine != "markitdown" && parserEngine != "anydoc" {
		return nil, fmt.Errorf("不支持的文档解析器 %q", parserEngine)
	}
	if !parse.SupportedExtForParser(fileName, parserEngine) {
		return nil, fmt.Errorf("解析器 %s 不支持该文件类型", parserEngine)
	}
	fileType := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
	if fileType == "markdown" {
		fileType = "md"
	}
	if fileType != "md" && fileType != "txt" && fileType != "pdf" {
		available := false
		if s.parserAvailable != nil {
			available = s.parserAvailable(parserEngine)
		} else if parserEngine == s.cfg.ParserSidecar.Engine && s.officeAvailable != nil {
			available = s.officeAvailable()
		}
		if !available {
			return nil, errors.New("文档转换能力当前不可用")
		}
	}
	if size < 0 {
		return nil, errors.New("文件大小不能为负数")
	}
	maxBytes := int64(s.cfg.Limits.MaxFileMB) * 1024 * 1024
	if size > maxBytes {
		return nil, fmt.Errorf("%w: 单文件上限 %dMB", ErrQuota, s.cfg.Limits.MaxFileMB)
	}
	docs, err := s.st.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		return nil, err
	}
	if len(docs) >= s.cfg.Limits.MaxDocsPerKB {
		return nil, fmt.Errorf("%w: 每知识库最多 %d 篇文档", ErrQuota, s.cfg.Limits.MaxDocsPerKB)
	}

	docID := "doc_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	key := objects.Key(kb.UserID, kbID, docID, fileName)
	contentType := mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	objectFence, err := s.st.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kbID, DocID: docID,
		ObjectKind: store.RAGObjectKindOriginal, ObjectKey: key, ReferenceKey: docID,
	})
	if err != nil {
		return nil, fmt.Errorf("register original object write: %w", err)
	}
	hasher := sha256.New()
	if err := s.obj.Put(ctx, key, io.TeeReader(r, hasher), size, contentType); err != nil {
		return nil, fmt.Errorf("保存原件: %w", err)
	}
	if ready, err := s.st.MarkRAGObjectWriteReady(ctx, *objectFence); err != nil {
		return nil, fmt.Errorf("mark original object ready: %w", err)
	} else if !ready {
		return nil, fmt.Errorf("mark original object ready: %w", store.ErrRAGLifecycleInactive)
	}
	doc := &store.RAGDocumentRecord{
		ID:                 docID,
		KBID:               kbID,
		FileName:           filepath.Base(fileName),
		FileType:           fileType,
		ParserEngine:       parserEngine,
		FileSize:           size,
		ObjectKey:          key,
		Status:             "PENDING",
		Version:            1,
		SourceSHA256:       hex.EncodeToString(hasher.Sum(nil)),
		ActiveVersion:      0,
		IndexFormatVersion: 1,
		ProcessingStage:    "queued",
		UploadedAt:         time.Now().UTC(),
	}
	snapshot, err := s.BuildVersionSnapshot(ctx, doc)
	if err != nil {
		_ = s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID))
		return nil, err
	}
	snapshot.DocVersion = doc.Version
	taskID, err := s.st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, snapshot, 3, store.RAGAdvancedEnqueuePolicy{
		UserID:             kb.UserID,
		MaxPendingTasks:    s.cfg.Limits.MaxPendingAdvancedTasksPerUser,
		MinReindexInterval: time.Duration(s.cfg.Limits.MinAdvancedReindexInterval) * time.Second,
	})
	if err != nil {
		_ = s.obj.DeletePrefix(ctx, fmt.Sprintf("rag/%s/%s/%s/", kb.UserID, kbID, docID))
		s.emitIndexTaskPolicyRejection(ctx, doc.ID, doc.Version, "enqueue", err)
		return nil, err
	}
	s.notifyTask(ctx, taskID)
	return doc, nil
}

func (s *Service) ReindexDocument(ctx context.Context, ownerID, kbID, docID string) error {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(docID)
	docLock.Lock()
	defer docLock.Unlock()

	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return err
	}
	if err := s.requireActiveUser(ctx, kb.UserID); err != nil {
		return err
	}
	if err := s.ensureNoPolicySync(ctx, kbID); err != nil {
		return err
	}
	if kb.Status != "active" {
		return errors.New("知识库正在删除中")
	}
	doc, err := s.GetDocument(ctx, ownerID, kbID, docID)
	if err != nil {
		return err
	}
	snapshot, err := s.BuildVersionSnapshot(ctx, doc)
	if err != nil {
		return err
	}
	snapshot.DocVersion = 0 // assigned atomically by the store
	task, err := s.st.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, doc.Version, snapshot, store.RAGAdvancedEnqueuePolicy{
		UserID:             kb.UserID,
		MaxPendingTasks:    s.cfg.Limits.MaxPendingAdvancedTasksPerUser,
		MinReindexInterval: time.Duration(s.cfg.Limits.MinAdvancedReindexInterval) * time.Second,
	})
	if err != nil {
		s.emitIndexTaskPolicyRejection(ctx, doc.ID, doc.Version, "reindex", err)
		return err
	}
	s.notifyTask(ctx, task.ID)
	return nil
}

func (s *Service) emitIndexTaskPolicyRejection(
	ctx context.Context,
	docID string,
	docVersion int64,
	transition string,
	err error,
) {
	errorCode := ""
	switch {
	case errors.Is(err, store.ErrRAGAdvancedPendingLimit):
		errorCode = indexTaskPendingLimitCode
	case errors.Is(err, store.ErrRAGAdvancedReindexRateLimit):
		errorCode = indexTaskReindexRateLimitCode
	default:
		return
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
		DocID: docID, DocVersion: docVersion, Transition: transition,
		Outcome: indexTaskRejectedTelemetryState, ErrorCode: errorCode,
	})
}

func (s *Service) DeleteDocument(ctx context.Context, ownerID, kbID, docID string) error {
	kbLock := s.kbMutex(kbID)
	kbLock.RLock()
	defer kbLock.RUnlock()

	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return err
	}
	if err := s.ensureNoPolicySync(ctx, kbID); err != nil {
		return err
	}
	if _, err := s.GetDocument(ctx, ownerID, kbID, docID); err != nil {
		return err
	}
	// Persist the tombstone before waiting for an in-process index worker. SQL
	// search/claim snapshots and both asset authorization paths therefore fail
	// closed immediately, while cleanup remains safely retryable.
	doc, err := s.st.MarkRAGDocumentDeleting(ctx, docID)
	if err != nil {
		return err
	}
	docLock := s.docMutex(docID)
	docLock.Lock()
	defer docLock.Unlock()
	return s.cleanupDeletingDocument(ctx, doc)
}

func (s *Service) scheduleTask(taskID int64) {
	select {
	case s.tasks <- taskID:
	default:
		// The durable row is authoritative. A later poll will create another
		// wake after capacity becomes available.
		if taskID != 0 {
			slog.Warn("rag: index wake queue full; durable poller will recover", "task", taskID)
		}
	}
}

func (s *Service) notifyTask(ctx context.Context, taskID int64) {
	if s == nil || taskID <= 0 {
		return
	}
	switch s.workerMode {
	case WorkerModeLegacy:
		s.scheduleTask(taskID)
	case WorkerModeFair:
		if s.taskNotifier == nil {
			slog.Warn("rag: fair task notifier unavailable; durable dispatcher scan will recover", "task", taskID)
			return
		}
		if err := s.taskNotifier.TryDispatch(ctx, taskID); err != nil {
			slog.Warn("rag: fair task fast-path dispatch failed; durable dispatcher scan will recover",
				"task", taskID, "error", err)
		}
	case WorkerModePaused:
		// Durable SQL is authoritative. A later fair or legacy rollout will
		// discover the task after the deployment drain completes.
	default:
		// Unknown values fail closed and never wake an index claimant.
	}
}

// runTask remains for package-level compatibility tests. The id is only a
// hint: correctness requires claiming the next due row from SQL.
func (s *Service) runTask(ctx context.Context, _ int64) {
	claim, err := s.st.ClaimRAGIndexTask(ctx, s.workerID, s.leaseDuration)
	if err != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
			Transition: "claim", Outcome: "error", ErrorCode: "store_error",
		})
		slog.Error("rag: durable index claim failed", "worker", s.workerID, "error", err)
		return
	}
	if claim != nil {
		if err := s.runClaim(ctx, claim); err != nil && ctx.Err() == nil {
			slog.Error("rag: index claim execution failed", "task", claim.Fence.TaskID, "error", err)
		}
	}
}

// RunFairClaim is the sole pipeline entry point exposed to the fair adapter.
// The delivered claim supplies the expected writer identity used by every
// context-aware read/catalog mutation and by the fenced budget facade.
func (s *Service) RunFairClaim(parent context.Context, claim *store.RAGIndexClaim) error {
	if s == nil || s.workerMode != WorkerModeFair || s.fairExecution == nil || s.fairStore == nil {
		return store.ErrFairQueueMySQLRequired
	}
	if claim == nil || !validFairWriterFingerprint(claim.Fence.ExpectedWriterFingerprint) {
		return store.ErrFairQueueWriterMismatch
	}
	if s.fairStore.ExpectedWriterFingerprint() != claim.Fence.ExpectedWriterFingerprint {
		return store.ErrFairQueueWriterMismatch
	}
	err := s.runClaim(withFairIndexFence(parent, claim.Fence), claim)
	if isFatalFairClaimError(claim, err) {
		return err
	}
	if errors.Is(err, errIndexFenceLost) {
		// The exact lease is already gone. Its Rabbit delivery is stale and may
		// be acknowledged; the canonical row decides whether another generation
		// is due.
		return nil
	}
	return err
}

func validFairWriterFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isFairStoreSafetyError(err error) bool {
	return errors.Is(err, store.ErrFairQueueWriterMismatch) ||
		errors.Is(err, store.ErrFairQueueUnsafeConnection)
}

func isFatalFairClaimError(claim *store.RAGIndexClaim, err error) bool {
	return claim != nil && claim.Fence.ExpectedWriterFingerprint != "" &&
		(isFairStoreSafetyError(err) || errors.Is(err, store.ErrRAGDocumentVersionMismatch) ||
			errors.Is(err, store.ErrRAGDocumentVersionConflict) ||
			errors.Is(err, store.ErrRAGDocumentSourceConflict) ||
			errors.Is(err, store.ErrRAGDocumentAILedgerCorrupt))
}

func isFairFenceLossError(claim *store.RAGIndexClaim, err error) bool {
	return claim != nil && claim.Fence.ExpectedWriterFingerprint != "" &&
		(errors.Is(err, errIndexFenceLost) ||
			errors.Is(err, store.ErrRAGDocumentAIInvalidFence) ||
			errors.Is(err, store.ErrRAGLifecycleInactive))
}

func (s *Service) runClaim(parent context.Context, claim *store.RAGIndexClaim) (resultErr error) {
	if claim == nil {
		return nil
	}
	defer func() {
		ackCtx := parent
		if claim.Fence.ExpectedWriterFingerprint != "" {
			var cancel context.CancelFunc
			ackCtx, cancel = context.WithTimeout(context.WithoutCancel(parent), fairClaimFinalizeTimeout)
			defer cancel()
		}
		if _, err := s.st.AcknowledgeRAGIndexTaskQuiesced(ackCtx, claim.Fence); err != nil {
			if parent.Err() == nil || isFatalFairClaimError(claim, err) {
				resultErr = errors.Join(resultErr, err)
				slog.Warn("rag: failed to acknowledge superseded worker quiescence",
					"task", claim.Fence.TaskID, "error", err)
			}
		}
	}()
	claimStarted := time.Now()
	telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
		claim, "claim", "ok", "",
	))
	workCtx, cancelWork := context.WithCancel(parent)
	defer cancelWork()
	heartbeatCtx, stopHeartbeat := context.WithCancel(parent)
	var leaseLost atomic.Bool
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- s.heartbeatLoop(heartbeatCtx, claim.Fence, &leaseLost, cancelWork)
	}()
	heartbeatStopped := false
	var heartbeatErr error
	stopAndWaitHeartbeat := func() error {
		if !heartbeatStopped {
			heartbeatStopped = true
			stopHeartbeat()
			heartbeatErr = <-heartbeatDone
		}
		return heartbeatErr
	}
	defer stopAndWaitHeartbeat()

	doc, err := s.st.GetRAGDocument(workCtx, claim.Fence.DocID)
	previousActive := int64(0)
	if err == nil {
		previousActive = doc.ActiveVersion
	}
	var embeddingBinding config.RAGEmbeddingCfg
	if err == nil {
		var current *store.RAGDocumentVersionRecord
		current, embeddingBinding, err = s.buildVersionSnapshotAndBinding(workCtx, doc)
		if err == nil && !sameRuntimeProviderContracts(&claim.Version, current) {
			current.DocVersion = 0
			created, ok, supersedeErr := s.st.SupersedeRAGIndexTaskAndCreateVersion(workCtx, claim.Fence, current)
			heartbeatErr := stopAndWaitHeartbeat()
			if supersedeErr != nil {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "error", "store_error",
				))
				slog.Error("rag: supersede provider-mismatched index task", "task", claim.Fence.TaskID, "error", supersedeErr)
				return errors.Join(supersedeErr, heartbeatErr)
			}
			if heartbeatErr != nil {
				return heartbeatErr
			}
			if ok && (created == nil || created.ID <= 0 || created.DocID != claim.Fence.DocID ||
				created.DocVersion <= claim.Fence.DocVersion) {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "error", "store_error",
				))
				return store.ErrRAGDocumentVersionMismatch
			}
			if ok {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "ok", "",
				))
				s.notifyTask(parent, created.ID)
			} else {
				telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
					claim, "supersede", "rejected", "fence_lost",
				))
			}
			if !ok {
				return errIndexFenceLost
			}
			return nil
		}
	}
	if err != nil {
		heartbeatErr := stopAndWaitHeartbeat()
		if heartbeatErr != nil {
			return heartbeatErr
		}
		if isFatalFairClaimError(claim, err) {
			cancelWork()
			return err
		}
		if isFairFenceLossError(claim, err) {
			return errIndexFenceLost
		}
		return s.finishClaimFailure(parent, claim, err, leaseLost.Load())
	}

	activation, err := s.indexClaim(workCtx, claim, embeddingBinding)
	if errors.Is(err, errIndexFenceLost) {
		cancelWork()
	}
	heartbeatErr = stopAndWaitHeartbeat()
	if heartbeatErr != nil {
		return heartbeatErr
	}
	if err != nil {
		if isFatalFairClaimError(claim, err) {
			cancelWork()
			return err
		}
		if isFairFenceLossError(claim, err) {
			return errIndexFenceLost
		}
		return s.finishClaimFailure(parent, claim, err, leaseLost.Load())
	}
	if leaseLost.Load() || parent.Err() != nil {
		return parent.Err()
	}
	ok, err := s.st.ActivateAndFinishRAGIndexTask(parent, claim.Fence, activation, s.gcGracePeriod)
	if err != nil {
		telemetry.Emit(parent, s.telemetry, telemetry.EventActiveVersionSwitch, telemetry.Fields{
			DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID, DocVersion: claim.Fence.DocVersion,
			PreviousVersion: previousActive, ClaimGeneration: claim.Fence.ClaimGeneration,
			Transition: "activate", Outcome: "error", ErrorCode: "store_error",
			Duration: time.Since(claimStarted),
		})
		slog.Error("rag: atomic index activation failed", "task", claim.Fence.TaskID, "error", err)
		return err
	}
	if !ok {
		telemetry.Emit(parent, s.telemetry, telemetry.EventActiveVersionSwitch, telemetry.Fields{
			DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID, DocVersion: claim.Fence.DocVersion,
			PreviousVersion: previousActive, ClaimGeneration: claim.Fence.ClaimGeneration,
			Transition: "activate", Outcome: "rejected", ErrorCode: "fence_lost",
			Duration: time.Since(claimStarted),
		})
		slog.Info("rag: skipped activation after index fence was lost", "task", claim.Fence.TaskID,
			"doc_version", claim.Fence.DocVersion, "generation", claim.Fence.ClaimGeneration)
		return errIndexFenceLost
	}
	retiredVersion := int64(0)
	if previousActive > 0 && previousActive != claim.Fence.DocVersion {
		retiredVersion = previousActive
	}
	telemetry.Emit(parent, s.telemetry, telemetry.EventActiveVersionSwitch, telemetry.Fields{
		DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID, DocVersion: claim.Fence.DocVersion,
		PreviousVersion: previousActive, RetiredVersion: retiredVersion,
		ClaimGeneration: claim.Fence.ClaimGeneration, Transition: "activate", Outcome: "ok",
		Duration: time.Since(claimStarted),
	})
	return nil
}

func (s *Service) heartbeatLoop(
	ctx context.Context,
	fence store.IndexFence,
	leaseLost *atomic.Bool,
	cancelWork context.CancelFunc,
) error {
	interval := s.heartbeatInterval
	if interval <= 0 || interval >= s.leaseDuration {
		interval = s.leaseDuration / 3
	}
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			ok, err := s.st.HeartbeatRAGIndexTask(ctx, fence, s.leaseDuration)
			if isFairStoreSafetyError(err) {
				telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
					DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
					ClaimGeneration: fence.ClaimGeneration, Transition: "heartbeat",
					Outcome: "error", ErrorCode: "store_error",
				})
				leaseLost.Store(true)
				cancelWork()
				slog.Error("rag: index heartbeat safety failure; canceling work",
					"task", fence.TaskID, "error", err)
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			if err != nil || !ok {
				errorCode := "fence_lost"
				if err != nil {
					errorCode = "store_error"
				}
				telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
					DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
					ClaimGeneration: fence.ClaimGeneration, Transition: "heartbeat",
					Outcome: "error", ErrorCode: errorCode,
				})
				leaseLost.Store(true)
				cancelWork()
				if err != nil && ctx.Err() == nil {
					slog.Error("rag: index heartbeat failed; canceling work", "task", fence.TaskID, "error", err)
				}
				if err != nil {
					return err
				}
				return errIndexFenceLost
			}
			telemetry.Emit(ctx, s.telemetry, telemetry.EventIndexTask, telemetry.Fields{
				DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
				ClaimGeneration: fence.ClaimGeneration, Transition: "heartbeat", Outcome: "ok",
			})
		}
	}
}

func (s *Service) finishClaimFailure(parent context.Context, claim *store.RAGIndexClaim, err error, leaseLost bool) error {
	if err == nil || claim == nil || leaseLost || parent.Err() != nil || errors.Is(err, errIndexFenceLost) {
		return nil
	}
	if isFatalFairClaimError(claim, err) || isFairFenceLossError(claim, err) {
		return err
	}
	transient := isTransientIndexError(err)
	message := safeIndexErrorMessage(err, transient)
	if transient && claim.Task.RetryCount < claim.Task.MaxRetry {
		delay := indexRetryDelay(claim.Task.RetryCount + 1)
		ok, retryErr := s.st.RetryRAGIndexTask(parent, claim.Fence, message, delay)
		if retryErr != nil {
			telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
				claim, "retry", "error", "store_error",
			))
			slog.Error("rag: persist transient index retry", "task", claim.Fence.TaskID, "error", retryErr)
			return retryErr
		} else if ok {
			fields := indexTaskTelemetryFields(claim, "retry", "scheduled", "")
			fields.RetryCount = claim.Task.RetryCount + 1
			telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, fields)
			slog.Warn("rag: transient index failure scheduled for retry", "task", claim.Fence.TaskID,
				"retry", claim.Task.RetryCount+1, "delay", delay, "error", message)
		} else {
			telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
				claim, "retry", "rejected", "fence_lost",
			))
			return errIndexFenceLost
		}
		return nil
	}
	ok, failErr := s.st.FailRAGIndexTask(parent, claim.Fence, message)
	if failErr != nil {
		telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
			claim, "finish", "error", "store_error",
		))
		slog.Error("rag: persist permanent index failure", "task", claim.Fence.TaskID, "error", failErr)
		return failErr
	} else if ok {
		telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
			claim, "finish", "error", "permanent_failure",
		))
		slog.Error("rag: document indexing failed permanently", "task", claim.Fence.TaskID, "error", message)
	} else {
		telemetry.Emit(parent, s.telemetry, telemetry.EventIndexTask, indexTaskTelemetryFields(
			claim, "finish", "rejected", "fence_lost",
		))
		return errIndexFenceLost
	}
	return nil
}

func indexTaskTelemetryFields(
	claim *store.RAGIndexClaim,
	transition, outcome, errorCode string,
) telemetry.Fields {
	if claim == nil {
		return telemetry.Fields{Transition: transition, Outcome: outcome, ErrorCode: errorCode}
	}
	return telemetry.Fields{
		DocID: claim.Fence.DocID, TaskID: claim.Fence.TaskID,
		DocVersion: claim.Fence.DocVersion, ClaimGeneration: claim.Fence.ClaimGeneration,
		RetryCount: claim.Task.RetryCount, Transition: transition, Outcome: outcome,
		ErrorCode: errorCode,
	}
}

func safeIndexErrorMessage(err error, transient bool) string {
	switch {
	case errors.Is(err, parse.ErrEmptyContent):
		return parse.ErrEmptyContent.Error()
	case errors.Is(err, parse.ErrDocumentLimitExceeded), errors.Is(err, sidecar.ErrBundleLimitExceeded),
		errors.Is(err, sidecar.ErrSourceLimitExceeded):
		return "文档超过解析硬限制"
	case errors.Is(err, parse.ErrSourceIntegrity), errors.Is(err, sidecar.ErrSourceIntegrity):
		return "文档原件与不可变快照不一致"
	case errors.Is(err, parse.ErrInvalidDocument):
		return "文档格式或内容无效"
	case errors.Is(err, sidecar.ErrInvalidBundle):
		return "文档解析服务返回不兼容结果"
	case errors.Is(err, sidecar.ErrCapabilityUnavailable):
		return "所需文档解析能力当前不可用"
	}
	var statusErr interface{ HTTPStatus() int }
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("文档索引依赖返回 HTTP %d", statusErr.HTTPStatus())
	}
	if transient {
		return "文档索引暂时失败，稍后重试"
	}
	return "文档索引失败"
}

func indexRetryDelay(retry int) time.Duration {
	if retry < 1 {
		retry = 1
	}
	if retry > 8 {
		retry = 8
	}
	return time.Duration(1<<(retry-1)) * time.Second
}

func isTransientIndexError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, parse.ErrEmptyContent) || errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, parse.ErrInvalidDocument) || errors.Is(err, parse.ErrDocumentLimitExceeded) ||
		errors.Is(err, parse.ErrSourceIntegrity) || errors.Is(err, sidecar.ErrCapabilityUnavailable) ||
		errors.Is(err, sidecar.ErrInvalidBundle) || errors.Is(err, sidecar.ErrBundleLimitExceeded) ||
		errors.Is(err, sidecar.ErrSourceLimitExceeded) || errors.Is(err, sidecar.ErrSourceIntegrity) ||
		errors.Is(err, store.ErrRAGDocumentAIBudgetExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return true
	}
	var statusErr interface{ HTTPStatus() int }
	if errors.As(err, &statusErr) {
		status := statusErr.HTTPStatus()
		switch {
		case status == http.StatusRequestTimeout,
			status == http.StatusTooEarly,
			status == http.StatusTooManyRequests,
			status >= 500 && status <= 599:
			return true
		case status >= 400 && status <= 499:
			return false
		}
	}
	message := strings.ToLower(err.Error())
	for _, permanent := range []string{
		"不支持的文件类型", "分块结果为空", "维度不符", "非法 index", "重复 index",
		"schema", "validation", "unsupported", "exceeds", "上限", "不能为空",
		"配置不可用", "endpoint 不可用", "knowledge base is not active",
	} {
		if strings.Contains(message, permanent) {
			return false
		}
	}
	if strings.Contains(message, "返回 429") {
		return true
	}
	for status := 400; status <= 499; status++ {
		if status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests {
			continue
		}
		if strings.Contains(message, fmt.Sprintf("返回 %d", status)) {
			return false
		}
	}
	for status := 500; status <= 599; status++ {
		if strings.Contains(message, fmt.Sprintf("返回 %d", status)) {
			return true
		}
	}
	// SQL, object-store and vector-store failures are retryable unless they
	// matched a deterministic validation/corruption condition above.
	return true
}

func (s *Service) indexClaim(
	ctx context.Context,
	claim *store.RAGIndexClaim,
	embeddingBinding config.RAGEmbeddingCfg,
) (activation store.RAGIndexActivation, resultErr error) {
	fence := claim.Fence
	version := &claim.Version

	initial, err := s.st.GetRAGDocument(ctx, fence.DocID)
	if err != nil {
		return activation, err
	}
	kbLock := s.kbMutex(initial.KBID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(fence.DocID)
	docLock.Lock()
	defer docLock.Unlock()

	doc, err := s.st.GetRAGDocument(ctx, fence.DocID)
	if err != nil {
		return activation, err
	}
	if strings.EqualFold(doc.Status, "deleting") ||
		doc.Version != fence.DocVersion || version.DocVersion != fence.DocVersion {
		return activation, errIndexFenceLost
	}
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return activation, err
	}
	if kb.Status != "active" {
		return activation, errors.New("knowledge base is not active")
	}
	parseMode := config.ParseMode(version.ParseMode)
	if !parseMode.Valid() {
		return activation, fmt.Errorf("unsupported parse mode %q", version.ParseMode)
	}
	if version.ChunkSize <= 0 || version.ChunkOverlap < 0 || version.ChunkOverlap >= version.ChunkSize {
		return activation, errors.New("invalid immutable chunk contract")
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "loading"}); err != nil {
		return activation, err
	}
	budget, err := s.newTaskDocumentAIBudget(claim, kb.UserID)
	if err != nil {
		return activation, err
	}
	artifact, _, artifactKey, err := s.loadOrParseArtifact(ctx, claim, kb, doc, parseMode, budget)
	if err != nil {
		return activation, err
	}
	chunks, enrichmentWarnings, err := s.buildSearchChunks(ctx, artifact, searchChunkBuildOptions{
		ChunkSize: version.ChunkSize, ChunkOverlap: version.ChunkOverlap,
		EnrichmentEnabled: version.EnrichmentEnabled, TextModel: version.TextModel,
		Scope:  enrich.CacheScope{UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, IndexFingerprint: version.IndexFingerprint},
		Budget: budget,
		Progress: func(progressCtx context.Context, progress store.RAGIndexProgress) error {
			return s.fencedProgress(progressCtx, fence, progress)
		},
	})
	if err != nil {
		return activation, err
	}

	warningCount := len(artifact.Warnings) + len(enrichmentWarnings)
	degraded := len(enrichmentWarnings) > 0
	for _, warning := range artifact.Warnings {
		degraded = degraded || warning.Degraded
	}
	if err := s.fencedWarnings(ctx, fence, degraded, warningCount); err != nil {
		return activation, err
	}

	vectors, totalTokens, err := s.embedChunks(ctx, fence, kb.ID, version, embeddingBinding, chunks)
	if err != nil {
		return activation, err
	}
	vectorChunks, err := s.stageIndexVersion(ctx, fence, kb.ID, doc.ID, chunks, vectors)
	if err != nil {
		return activation, err
	}
	if err := s.upsertIndexVersion(ctx, fence, kb.ID, vectorChunks); err != nil {
		return activation, err
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "finalizing", Current: len(chunks), Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return activation, err
	}
	activation = store.RAGIndexActivation{
		VersionResult: store.RAGDocumentVersionResult{
			Status: store.RAGDocumentVersionDone, ParseArtifactKey: artifactKey,
			PageCount: parsedArtifactPageCount(artifact), AssetCount: len(artifact.Assets),
			Degraded: degraded, WarningCount: warningCount,
		},
		ChunkCount: len(chunks), TokenCount: totalTokens,
	}
	return activation, nil
}

const pipelineStageBatchSize = 200

func (s *Service) parsedArtifactPersister() *ragassets.Persister {
	maxArtifactBytes := s.cfg.Limits.MaxExtractedBytes
	if maxArtifactBytes <= 0 {
		maxArtifactBytes = 200 << 20
	}
	return &ragassets.Persister{
		Objects: s.obj, Catalog: s.st,
		Limits: ragassets.Limits{
			MaxAssets: s.cfg.Limits.MaxAssetsPerDocument, MaxAssetBytes: s.cfg.Limits.MaxAssetBytes,
			MaxExtractedBytes: s.cfg.Limits.MaxExtractedBytes, MaxImagePixels: s.cfg.Limits.MaxImagePixels,
			MaxArtifactBytes: maxArtifactBytes, DisplayMaxEdge: s.cfg.Limits.DisplayMaxEdge,
			ThumbnailMaxEdge: s.cfg.Limits.ThumbnailMaxEdge,
		},
	}
}

func (s *Service) loadOrParseArtifact(
	ctx context.Context,
	claim *store.RAGIndexClaim,
	kb *store.RAGKBRecord,
	doc *store.RAGDocumentRecord,
	parseMode config.ParseMode,
	budget *vision.TaskDocumentAIBudget,
) (*document.ParsedArtifact, bool, string, error) {
	version, fence := &claim.Version, claim.Fence
	persister := s.parsedArtifactPersister()
	logicalArtifactKey, err := document.ArtifactJSONKey(
		kb.UserID, kb.ID, doc.ID, version.ParseFingerprint,
	)
	if err != nil {
		return nil, false, "", err
	}
	artifactKey := strings.TrimSpace(version.ParseArtifactKey)
	if artifactKey == "" {
		artifactKey, err = document.VersionedObjectKey(logicalArtifactKey, fence.DocVersion)
		if err != nil {
			return nil, false, "", err
		}
		// A chunk-only reindex reuses the active generation's immutable parse
		// artifact. Validate it before pinning the new version so a missing or
		// corrupt old object can fall back to this version's own physical key.
		if doc.ActiveVersion > 0 && doc.ActiveVersion != fence.DocVersion {
			active, activeErr := s.st.GetRAGDocumentVersion(ctx, doc.ID, doc.ActiveVersion)
			if activeErr != nil && !errors.Is(activeErr, store.ErrNotFound) {
				return nil, false, "", activeErr
			}
			if activeErr == nil && active.ParseFingerprint == version.ParseFingerprint &&
				strings.TrimSpace(active.ParseArtifactKey) != "" {
				reuseKey := strings.TrimSpace(active.ParseArtifactKey)
				reuseRequest := ragassets.CacheRequest{
					UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion,
					ParseFingerprint: version.ParseFingerprint, ArtifactObjectKey: reuseKey,
					IndexFence: &fence,
					ExpectedSource: &document.ParsedSource{
						DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType,
						ByteSize: doc.FileSize, SHA256: version.SourceSHA256,
					},
				}
				if reused, hit, loadErr := persister.LoadParsedArtifact(ctx, reuseRequest); loadErr != nil {
					return nil, false, "", fmt.Errorf("load active parsed artifact cache: %w", loadErr)
				} else if hit && parsedArtifactMatchesSource(reused, doc, version) &&
					!parsedArtifactHasDegradedWarnings(reused) {
					recorded, recordErr := s.st.RecordRAGDocumentParseArtifact(ctx, fence, reuseKey)
					if recordErr != nil {
						return nil, false, "", fmt.Errorf("record reused parsed artifact: %w", recordErr)
					}
					if !recorded {
						return nil, false, "", errIndexFenceLost
					}
					telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
						DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
						ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
						CacheStatus: "hit", Outcome: "ok",
					})
					return reused, true, reuseKey, nil
				}
			}
		}
	}
	recorded, err := s.st.RecordRAGDocumentParseArtifact(ctx, fence, artifactKey)
	if err != nil {
		return nil, false, "", fmt.Errorf("record parsed artifact cleanup handle: %w", err)
	}
	if !recorded {
		return nil, false, "", errIndexFenceLost
	}
	expectedSource := document.ParsedSource{
		DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType,
		ByteSize: doc.FileSize, SHA256: version.SourceSHA256,
	}
	cacheRequest := ragassets.CacheRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion,
		ParseFingerprint: version.ParseFingerprint, ExpectedSource: &expectedSource,
		ArtifactObjectKey: artifactKey, IndexFence: &fence,
	}
	artifact, hit, err := persister.LoadParsedArtifact(ctx, cacheRequest)
	if err != nil {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
			DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
			ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
			CacheStatus: "error", Outcome: "error", ErrorCode: "store_error",
		})
		return nil, false, "", fmt.Errorf("load parsed artifact cache: %w", err)
	}
	if hit && parsedArtifactMatchesSource(artifact, doc, version) &&
		!parsedArtifactHasDegradedWarnings(artifact) {
		telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
			DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
			ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
			CacheStatus: "hit", Outcome: "ok",
		})
		return artifact, true, artifactKey, nil
	}
	cacheStatus := "miss"
	if hit {
		cacheStatus = "stale"
	}
	telemetry.Emit(ctx, s.telemetry, telemetry.EventResultCache, telemetry.Fields{
		DocID: doc.ID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
		ClaimGeneration: fence.ClaimGeneration, CacheKind: "parse_artifact",
		CacheStatus: cacheStatus, Outcome: "ok",
	})
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{Stage: "parsing"}); err != nil {
		return nil, false, "", err
	}
	source := document.Source{
		DocID: doc.ID, FileName: doc.FileName, Format: doc.FileType, ParserEngine: doc.ParserEngine,
		Size: doc.FileSize, SHA256: version.SourceSHA256,
		Open: func(openCtx context.Context) (io.ReadCloser, error) {
			reader, openErr := s.obj.Get(openCtx, doc.ObjectKey)
			if openErr != nil {
				return nil, fmt.Errorf("读原件: %w", openErr)
			}
			return reader, nil
		},
	}
	parsed, parseErr := s.parser.Parse(ctx, source, parse.ParseOptions{
		Mode: parseMode, ParserVersion: version.ParserVersion,
		PageTranscriber: s.pageVision, ImageTranscriber: s.imageVision,
		DocumentAIBudget: budget,
		VisionScope: vision.CacheScope{
			UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID,
			ParseFingerprint: version.ParseFingerprint,
		},
		Progress: func(progressCtx context.Context, progress parse.ParseProgress) error {
			stage := strings.TrimSpace(progress.Stage)
			if stage == "" {
				stage = "parsing"
			}
			return s.fencedProgress(progressCtx, fence, store.RAGIndexProgress{
				Stage: stage, Current: progress.Current, Total: progress.Total, Unit: progress.Unit,
			})
		},
	})
	if parseErr != nil {
		if parsed != nil {
			parseErr = errors.Join(parseErr, parsed.Close())
		}
		return nil, false, "", parseErr
	}
	if parsed == nil {
		return nil, false, "", errors.New("parser returned a nil document")
	}
	if err := normalizeParsedDocument(parsed); err != nil {
		return nil, false, "", errors.Join(err, parsed.Close())
	}
	valid, err := s.st.CheckRAGIndexFence(ctx, fence)
	if err != nil {
		return nil, false, "", errors.Join(err, parsed.Close())
	}
	if !valid {
		return nil, false, "", errors.Join(errIndexFenceLost, parsed.Close())
	}
	artifact, err = persister.PersistParsedDocument(ctx, ragassets.PersistRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, DocVersion: fence.DocVersion,
		ParseFingerprint: version.ParseFingerprint, NeutralCaption: "图片（未进行视觉识别）",
		ArtifactObjectKey:   artifactKey,
		NormalizedObjectKey: path.Join(path.Dir(artifactKey), "normalized.md"),
		IndexFence:          &fence,
		Document:            parsed,
	})
	if err != nil {
		return nil, false, "", fmt.Errorf("persist parsed assets and artifact: %w", err)
	}
	return artifact, false, artifactKey, nil
}

func parsedArtifactMatchesSource(
	artifact *document.ParsedArtifact,
	doc *store.RAGDocumentRecord,
	version *store.RAGDocumentVersionRecord,
) bool {
	if artifact == nil || doc == nil || version == nil {
		return false
	}
	return artifact.Source.DocID == doc.ID && artifact.Source.FileName == doc.FileName &&
		strings.EqualFold(strings.TrimPrefix(artifact.Source.Format, "."), strings.TrimPrefix(doc.FileType, ".")) &&
		artifact.Source.ByteSize == doc.FileSize && artifact.Source.SHA256 == version.SourceSHA256
}

func parsedArtifactHasDegradedWarnings(artifact *document.ParsedArtifact) bool {
	if artifact == nil {
		return false
	}
	for _, warning := range artifact.Warnings {
		if warning.Degraded {
			return true
		}
	}
	return false
}

func normalizeParsedDocument(parsed *document.ParsedDocument) error {
	if parsed == nil {
		return errors.New("parsed document is nil")
	}
	occurrences := make(map[string]document.AssetOccurrence, len(parsed.Occurrences))
	for _, occurrence := range parsed.Occurrences {
		if _, duplicate := occurrences[occurrence.ID]; duplicate {
			return fmt.Errorf("duplicate parser occurrence %q", occurrence.ID)
		}
		occurrences[occurrence.ID] = occurrence
	}
	units, warnings, err := parse.NormalizeMarkdown(parsed.Units, occurrences, true)
	if err != nil {
		return fmt.Errorf("normalize parsed Markdown: %w", err)
	}
	parsed.Units = units
	parsed.Warnings = append(parsed.Warnings, warnings...)
	if err := parsed.Validate(); err != nil {
		return fmt.Errorf("validate normalized parsed document: %w", err)
	}
	return nil
}

type searchChunkBuildOptions struct {
	ChunkSize, ChunkOverlap int
	EnrichmentEnabled       bool
	TextModel               string
	Scope                   enrich.CacheScope
	Budget                  *vision.TaskDocumentAIBudget
	Progress                func(context.Context, store.RAGIndexProgress) error
}

// buildSearchChunks is shared by production and evaluation indexing. It is
// the single split/enrich/finalize implementation, with only target-specific
// progress and cache scopes injected by the caller.
func (s *Service) buildSearchChunks(ctx context.Context, artifact *document.ParsedArtifact, options searchChunkBuildOptions) ([]split.Chunk, []enrich.Warning, error) {
	if options.Progress != nil {
		if err := options.Progress(ctx, store.RAGIndexProgress{Stage: "chunking"}); err != nil {
			return nil, nil, err
		}
	}
	chunks := split.SplitArtifact(artifact, split.Config{
		ChunkSize: options.ChunkSize, ChunkOverlap: options.ChunkOverlap,
		EnhancementReserveTokens: func() int {
			if options.EnrichmentEnabled {
				return options.ChunkSize / 5
			}
			return 0
		}(),
	})
	if len(chunks) == 0 {
		return nil, nil, errors.New("分块结果为空")
	}
	finalizeConfig := enrich.FinalizeConfig{
		ChunkSize: options.ChunkSize, MaxSearchContentBytes: s.cfg.Limits.MaxSearchContentBytes,
		CollectionMaxLength: config.RAGMilvusContentMaxLength, ProviderTokenizer: s.tokenizer,
	}
	warnings := []enrich.Warning(nil)
	if options.EnrichmentEnabled {
		if options.Progress != nil {
			if err := options.Progress(ctx, store.RAGIndexProgress{Stage: "enriching", Current: 0, Total: len(chunks), Unit: "chunks"}); err != nil {
				return nil, nil, err
			}
		}
		if s.enricher == nil || !s.cfg.Features.TextEnrichmentEnabled || strings.TrimSpace(options.TextModel) == "" {
			warnings = append(warnings, enrich.Warning{ChunkIndex: -1, Code: "enrichment_unavailable",
				Message: "text enrichment was unavailable; source text retained"})
		} else {
			processor := enrich.NewProcessor(s.enricher)
			processor.SetRecorder(s.telemetry)
			enriched, enrichmentWarnings, enrichmentErr := processor.EnrichChunks(ctx, chunks, enrich.ProcessConfig{
				SystemEnabled: true, TextModel: options.TextModel, KBEnabled: true,
				MaxBlocks: s.cfg.Limits.MaxEnrichmentBlocksPerDocument, Finalize: finalizeConfig,
				Scope: options.Scope,
			}, options.Budget)
			if enrichmentErr != nil {
				return nil, nil, enrichmentErr
			}
			chunks = enriched
			warnings = append(warnings, enrichmentWarnings...)
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
	}
	chunks, err := enrich.FinalizeChunks(ctx, chunks, finalizeConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("finalize searchable chunks: %w", err)
	}
	if len(chunks) == 0 {
		return nil, nil, errors.New("分块结果为空")
	}
	return chunks, warnings, nil
}

func (s *Service) embedChunks(
	ctx context.Context,
	fence store.IndexFence,
	kbID string,
	version *store.RAGDocumentVersionRecord,
	binding config.RAGEmbeddingCfg,
	chunks []split.Chunk,
) ([][]float32, int, error) {
	texts := make([]string, len(chunks))
	totalTokens := 0
	for i := range chunks {
		texts[i] = chunks[i].SearchContent
		if strings.TrimSpace(texts[i]) == "" {
			return nil, 0, fmt.Errorf("empty SearchContent at chunk %d", chunks[i].Index)
		}
		if !s.cfg.Limits.SearchContentWithinLimit(texts[i]) ||
			split.EstimateTokens(texts[i]) > version.ChunkSize {
			return nil, 0, fmt.Errorf("SearchContent exceeds final boundary at chunk %d", chunks[i].Index)
		}
		totalTokens += chunks[i].Tokens
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "embedding", Current: 0, Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return nil, 0, err
	}
	collectionKey, err := s.resolveCollection(ctx, kbID)
	if err != nil {
		return nil, 0, fmt.Errorf("resolve vector collection: %w", err)
	}
	if err := s.vec.EnsureCollection(ctx, collectionKey, version.EmbeddingDimensions); err != nil {
		return nil, 0, fmt.Errorf("准备向量 collection: %w", err)
	}
	embedder := embed.New(binding.Endpoint, binding.APIKey,
		version.EmbeddingModel, version.EmbeddingDimensions)
	vectors, err := embedder.Embed(ctx, texts)
	if err != nil {
		return nil, 0, err
	}
	if len(vectors) != len(chunks) {
		return nil, 0, fmt.Errorf("embedding vector count %d does not match chunk count %d", len(vectors), len(chunks))
	}
	return vectors, totalTokens, nil
}

func (s *Service) stageIndexVersion(
	ctx context.Context,
	fence store.IndexFence,
	kbID, docID string,
	chunks []split.Chunk,
	vectors [][]float32,
) ([]vector.ChunkData, error) {
	if len(chunks) != len(vectors) {
		return nil, errors.New("cannot stage mismatched chunks and vectors")
	}
	now := time.Now().UTC()
	vectorChunks := make([]vector.ChunkData, len(chunks))
	sqlChunks := make([]store.RAGChunkRecord, len(chunks))
	mappings := make([]store.RAGChunkAssetRecord, 0)
	for i, chunk := range chunks {
		location := chunk.Location
		if location.Kind == "" {
			location = document.SourceLocation{Kind: document.LocationDocument}
		}
		locationJSON, err := json.Marshal(location)
		if err != nil {
			return nil, fmt.Errorf("encode chunk %d location: %w", chunk.Index, err)
		}
		pageNum := 0
		if location.Kind == document.LocationPage {
			pageNum = location.Index
		}
		vectorChunks[i] = vector.ChunkData{
			DocID: docID, Index: chunk.Index, Content: chunk.RawContent,
			SearchContent: chunk.SearchContent, SectionTitle: chunk.SectionTitle,
			PageNum: pageNum, DocVersion: fence.DocVersion, Vector: vectors[i],
		}
		sqlChunks[i] = store.RAGChunkRecord{
			KBID: kbID, DocID: docID, DocVersion: fence.DocVersion, ChunkIndex: chunk.Index,
			SectionTitle: chunk.SectionTitle, LocationJSON: string(locationJSON), RawContent: chunk.RawContent,
			Enhancement: chunk.Enhancement, SearchContent: chunk.SearchContent,
			TokenCount: chunk.Tokens, CreatedAt: now,
		}
		for ordinal, binding := range chunk.AssetBindings {
			assetLocationJSON, err := json.Marshal(binding.Asset.Location)
			if err != nil {
				return nil, fmt.Errorf("encode chunk %d asset location: %w", chunk.Index, err)
			}
			mappings = append(mappings, store.RAGChunkAssetRecord{
				DocID: docID, DocVersion: fence.DocVersion, ChunkIndex: chunk.Index,
				AssetID: binding.Asset.ID, AttachmentID: attachmentRefID(binding.Asset.Attachment),
				Ordinal: ordinal, LocationJSON: string(assetLocationJSON),
				Caption: binding.Asset.Caption, OCRText: binding.OCRText,
			})
		}
	}
	_, fairExecution := fairIndexFenceFromContext(ctx)
	for start := 0; start < len(sqlChunks); start += pipelineStageBatchSize {
		if !fairExecution {
			valid, err := s.st.CheckRAGIndexFence(ctx, fence)
			if err != nil {
				return nil, err
			}
			if !valid {
				return nil, errIndexFenceLost
			}
		}
		end := min(start+pipelineStageBatchSize, len(sqlChunks))
		if err := s.st.PutRAGChunks(ctx, sqlChunks[start:end]); err != nil {
			return nil, fmt.Errorf("stage chunk catalog: %w", err)
		}
	}
	for start := 0; start < len(mappings); start += pipelineStageBatchSize {
		if !fairExecution {
			valid, err := s.st.CheckRAGIndexFence(ctx, fence)
			if err != nil {
				return nil, err
			}
			if !valid {
				return nil, errIndexFenceLost
			}
		}
		end := min(start+pipelineStageBatchSize, len(mappings))
		if err := s.st.PutRAGChunkAssets(ctx, mappings[start:end]); err != nil {
			return nil, fmt.Errorf("stage chunk assets: %w", err)
		}
	}
	return vectorChunks, nil
}

func attachmentRefID(attachment *document.AttachmentRef) string {
	if attachment == nil {
		return ""
	}
	return attachment.ID
}

func (s *Service) upsertIndexVersion(
	ctx context.Context,
	fence store.IndexFence,
	kbID string,
	chunks []vector.ChunkData,
) error {
	collectionKey, err := s.resolveCollection(ctx, kbID)
	if err != nil {
		return fmt.Errorf("resolve vector collection: %w", err)
	}
	if err := s.fencedProgress(ctx, fence, store.RAGIndexProgress{
		Stage: "indexing", Current: 0, Total: len(chunks), Unit: "chunks",
	}); err != nil {
		return err
	}
	for start := 0; start < len(chunks); start += pipelineStageBatchSize {
		valid, err := s.st.CheckRAGIndexFence(ctx, fence)
		if err != nil {
			return err
		}
		if !valid {
			return errIndexFenceLost
		}
		end := min(start+pipelineStageBatchSize, len(chunks))
		if err := s.vec.UpsertChunks(ctx, collectionKey, chunks[start:end]); err != nil {
			return fmt.Errorf("写入向量库: %w", err)
		}
	}
	return nil
}

func parsedArtifactPageCount(artifact *document.ParsedArtifact) int {
	if artifact == nil {
		return 0
	}
	pageCount := 0
	for _, unit := range artifact.Units {
		if unit.Location.Kind == document.LocationDocument && pageCount == 0 {
			pageCount = 1
		}
		if unit.Location.Kind != document.LocationDocument && unit.Location.Index > pageCount {
			pageCount = unit.Location.Index
		}
	}
	for _, warning := range artifact.Warnings {
		if warning.Location != nil && warning.Location.Kind != document.LocationDocument && warning.Location.Index > pageCount {
			pageCount = warning.Location.Index
		}
	}
	return pageCount
}

func (s *Service) fencedWarnings(ctx context.Context, fence store.IndexFence, degraded bool, warningCount int) error {
	ok, err := s.st.UpdateWarningRAGIndexTask(ctx, fence, degraded, warningCount)
	if err != nil {
		return err
	}
	if !ok {
		return errIndexFenceLost
	}
	return nil
}

func (s *Service) fencedProgress(ctx context.Context, fence store.IndexFence, progress store.RAGIndexProgress) error {
	ok, err := s.st.UpdateProgressRAGIndexTask(ctx, fence, progress)
	if err != nil {
		return err
	}
	if !ok {
		return errIndexFenceLost
	}
	return nil
}
