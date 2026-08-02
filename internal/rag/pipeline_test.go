package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/chunktext"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

type pipelineStore struct {
	*store.DBStore
	claimFn             func(context.Context, string, time.Duration) (*store.RAGIndexClaim, error)
	heartbeatFn         func(context.Context, store.IndexFence, time.Duration) (bool, error)
	supersedeFn         func(context.Context, store.IndexFence, *store.RAGDocumentVersionRecord) (*store.RAGIndexTaskRecord, bool, error)
	activateFn          func(context.Context, store.IndexFence, store.RAGIndexActivation, time.Duration) (bool, error)
	acknowledgeFn       func(context.Context, store.IndexFence) (bool, error)
	retryFn             func(context.Context, store.IndexFence, string, time.Duration) (bool, error)
	failFn              func(context.Context, store.IndexFence, string) (bool, error)
	putChunksFn         func(context.Context, []store.RAGChunkRecord) error
	putChunkAssetsFn    func(context.Context, []store.RAGChunkAssetRecord) error
	putChunksForIndexFn func(context.Context, store.IndexFence, []store.RAGChunkRecord) (bool, error)
	beginObjectFn       func(context.Context, store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error)
	markObjectFn        func(context.Context, store.RAGObjectWriteFence) (bool, error)
	getDocumentForIndex func(context.Context, store.IndexFence) (*store.RAGDocumentRecord, error)
	createBudgetFn      func(context.Context, *store.RAGDocumentAITaskBudgetRecord) error
	reserveUsageFn      func(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error)
	getUsageFn          func(context.Context, string) (*store.RAGDocumentAIUsageRecord, error)
	markUsageFn         func(context.Context, string, store.IndexFence) (bool, error)
	commitUsageFn       func(context.Context, string, int64, int64, int64, bool) (bool, error)
	releaseUsageFn      func(context.Context, string) (bool, error)
	registerCacheFn     func(context.Context, store.RAGCacheObjectRecord) error
	registerCacheFairFn func(context.Context, store.IndexFence, store.RAGCacheObjectRecord) error
	checkFenceFn        func(context.Context, store.IndexFence) (bool, error)
	progressFn          func(context.Context, store.IndexFence, store.RAGIndexProgress) (bool, error)
	reconcileFn         func(context.Context, time.Time, time.Time, int) (int, error)
}

func (s *pipelineStore) PutRAGChunks(ctx context.Context, chunks []store.RAGChunkRecord) error {
	if s.putChunksFn != nil {
		return s.putChunksFn(ctx, chunks)
	}
	return s.DBStore.PutRAGChunks(ctx, chunks)
}

func (s *pipelineStore) PutRAGChunkAssets(
	ctx context.Context,
	mappings []store.RAGChunkAssetRecord,
) error {
	if s.putChunkAssetsFn != nil {
		return s.putChunkAssetsFn(ctx, mappings)
	}
	return s.DBStore.PutRAGChunkAssets(ctx, mappings)
}

func (s *pipelineStore) BeginRAGObjectWrite(
	ctx context.Context,
	request store.RAGObjectWriteRequest,
) (*store.RAGObjectWriteFence, error) {
	if s.beginObjectFn != nil {
		return s.beginObjectFn(ctx, request)
	}
	return s.DBStore.BeginRAGObjectWrite(ctx, request)
}

func (s *pipelineStore) MarkRAGObjectWriteReady(
	ctx context.Context,
	fence store.RAGObjectWriteFence,
) (bool, error) {
	if s.markObjectFn != nil {
		return s.markObjectFn(ctx, fence)
	}
	return s.DBStore.MarkRAGObjectWriteReady(ctx, fence)
}

func (s *pipelineStore) PutRAGChunksForIndex(
	ctx context.Context,
	fence store.IndexFence,
	chunks []store.RAGChunkRecord,
) (bool, error) {
	if s.putChunksForIndexFn != nil {
		return s.putChunksForIndexFn(ctx, fence, chunks)
	}
	return s.DBStore.PutRAGChunksForIndex(ctx, fence, chunks)
}

func (s *pipelineStore) GetRAGDocumentForIndex(
	ctx context.Context,
	fence store.IndexFence,
) (*store.RAGDocumentRecord, error) {
	if s.getDocumentForIndex != nil {
		return s.getDocumentForIndex(ctx, fence)
	}
	return s.DBStore.GetRAGDocumentForIndex(ctx, fence)
}

func (s *pipelineStore) CreateRAGDocumentAITaskBudget(
	ctx context.Context,
	budget *store.RAGDocumentAITaskBudgetRecord,
) error {
	if s.createBudgetFn != nil {
		return s.createBudgetFn(ctx, budget)
	}
	return s.DBStore.CreateRAGDocumentAITaskBudget(ctx, budget)
}

func (s *pipelineStore) ReserveRAGDocumentAIUsage(
	ctx context.Context,
	fence store.IndexFence,
	usage *store.RAGDocumentAIUsageRecord,
	limits store.RAGDocumentAILimits,
) (bool, error) {
	if s.reserveUsageFn != nil {
		return s.reserveUsageFn(ctx, fence, usage, limits)
	}
	return s.DBStore.ReserveRAGDocumentAIUsage(ctx, fence, usage, limits)
}

func (s *pipelineStore) GetRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (*store.RAGDocumentAIUsageRecord, error) {
	if s.getUsageFn != nil {
		return s.getUsageFn(ctx, idempotencyKey)
	}
	return s.DBStore.GetRAGDocumentAIUsage(ctx, idempotencyKey)
}

func (s *pipelineStore) MarkSentRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	fence store.IndexFence,
) (bool, error) {
	if s.markUsageFn != nil {
		return s.markUsageFn(ctx, idempotencyKey, fence)
	}
	return s.DBStore.MarkSentRAGDocumentAIUsage(ctx, idempotencyKey, fence)
}

func (s *pipelineStore) CommitRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	inputTokens, outputTokens, costMicroUSD int64,
	estimated bool,
) (bool, error) {
	if s.commitUsageFn != nil {
		return s.commitUsageFn(ctx, idempotencyKey, inputTokens, outputTokens, costMicroUSD, estimated)
	}
	return s.DBStore.CommitRAGDocumentAIUsage(
		ctx, idempotencyKey, inputTokens, outputTokens, costMicroUSD, estimated,
	)
}

func (s *pipelineStore) ReleaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (bool, error) {
	if s.releaseUsageFn != nil {
		return s.releaseUsageFn(ctx, idempotencyKey)
	}
	return s.DBStore.ReleaseRAGDocumentAIUsage(ctx, idempotencyKey)
}

func (s *pipelineStore) RegisterRAGCacheObject(
	ctx context.Context,
	record store.RAGCacheObjectRecord,
) error {
	if s.registerCacheFn != nil {
		return s.registerCacheFn(ctx, record)
	}
	return s.DBStore.RegisterRAGCacheObject(ctx, record)
}

func (s *pipelineStore) RegisterRAGCacheObjectForIndex(
	ctx context.Context,
	fence store.IndexFence,
	record store.RAGCacheObjectRecord,
) error {
	if s.registerCacheFairFn != nil {
		return s.registerCacheFairFn(ctx, fence, record)
	}
	return s.DBStore.RegisterRAGCacheObjectForIndex(ctx, fence, record)
}

func (s *pipelineStore) CheckRAGIndexFence(ctx context.Context, fence store.IndexFence) (bool, error) {
	if s.checkFenceFn != nil {
		return s.checkFenceFn(ctx, fence)
	}
	return s.DBStore.CheckRAGIndexFence(ctx, fence)
}

func (s *pipelineStore) UpdateProgressRAGIndexTask(
	ctx context.Context,
	fence store.IndexFence,
	progress store.RAGIndexProgress,
) (bool, error) {
	if s.progressFn != nil {
		return s.progressFn(ctx, fence, progress)
	}
	return s.DBStore.UpdateProgressRAGIndexTask(ctx, fence, progress)
}

func (s *pipelineStore) ClaimRAGIndexTask(ctx context.Context, workerID string, lease time.Duration) (*store.RAGIndexClaim, error) {
	if s.claimFn != nil {
		return s.claimFn(ctx, workerID, lease)
	}
	return s.DBStore.ClaimRAGIndexTask(ctx, workerID, lease)
}

func (s *pipelineStore) HeartbeatRAGIndexTask(ctx context.Context, fence store.IndexFence, lease time.Duration) (bool, error) {
	if s.heartbeatFn != nil {
		return s.heartbeatFn(ctx, fence, lease)
	}
	return s.DBStore.HeartbeatRAGIndexTask(ctx, fence, lease)
}

func (s *pipelineStore) AcknowledgeRAGIndexTaskQuiesced(
	ctx context.Context,
	fence store.IndexFence,
) (bool, error) {
	if s.acknowledgeFn != nil {
		return s.acknowledgeFn(ctx, fence)
	}
	return s.DBStore.AcknowledgeRAGIndexTaskQuiesced(ctx, fence)
}

func (s *pipelineStore) RetryRAGIndexTask(
	ctx context.Context,
	fence store.IndexFence,
	message string,
	delay time.Duration,
) (bool, error) {
	if s.retryFn != nil {
		return s.retryFn(ctx, fence, message, delay)
	}
	return s.DBStore.RetryRAGIndexTask(ctx, fence, message, delay)
}

func (s *pipelineStore) FailRAGIndexTask(
	ctx context.Context,
	fence store.IndexFence,
	message string,
) (bool, error) {
	if s.failFn != nil {
		return s.failFn(ctx, fence, message)
	}
	return s.DBStore.FailRAGIndexTask(ctx, fence, message)
}

func (s *pipelineStore) ReconcileRAGDocumentAIUsage(
	ctx context.Context,
	reservedBefore, sentBefore time.Time,
	limit int,
) (int, error) {
	if s.reconcileFn != nil {
		return s.reconcileFn(ctx, reservedBefore, sentBefore, limit)
	}
	return s.DBStore.ReconcileRAGDocumentAIUsage(ctx, reservedBefore, sentBefore, limit)
}

func (s *pipelineStore) SupersedeRAGIndexTaskAndCreateVersion(
	ctx context.Context,
	fence store.IndexFence,
	snapshot *store.RAGDocumentVersionRecord,
) (*store.RAGIndexTaskRecord, bool, error) {
	if s.supersedeFn != nil {
		return s.supersedeFn(ctx, fence, snapshot)
	}
	return s.DBStore.SupersedeRAGIndexTaskAndCreateVersion(ctx, fence, snapshot)
}

func (s *pipelineStore) ActivateAndFinishRAGIndexTask(
	ctx context.Context,
	fence store.IndexFence,
	activation store.RAGIndexActivation,
	grace time.Duration,
) (bool, error) {
	if s.activateFn != nil {
		return s.activateFn(ctx, fence, activation, grace)
	}
	return s.DBStore.ActivateAndFinishRAGIndexTask(ctx, fence, activation, grace)
}

type pipelineVector struct {
	*vector.Fake
	mu        sync.Mutex
	upserts   []vector.ChunkData
	upsertErr error
	onUpsert  func([]vector.ChunkData)
}

func (v *pipelineVector) UpsertChunks(ctx context.Context, kbID string, chunks []vector.ChunkData) error {
	v.mu.Lock()
	err := v.upsertErr
	v.mu.Unlock()
	if err != nil {
		return err
	}
	if err := v.Fake.UpsertChunks(ctx, kbID, chunks); err != nil {
		return err
	}
	v.mu.Lock()
	v.upserts = append(v.upserts, chunks...)
	onUpsert := v.onUpsert
	v.mu.Unlock()
	if onUpsert != nil {
		onUpsert(append([]vector.ChunkData(nil), chunks...))
	}
	return nil
}

func (v *pipelineVector) setUpsertError(err error) {
	v.mu.Lock()
	v.upsertErr = err
	v.mu.Unlock()
}

func (v *pipelineVector) chunks() []vector.ChunkData {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]vector.ChunkData(nil), v.upserts...)
}

const (
	pipelineEmbedOK int32 = iota
	pipelineEmbedWrongDimensions
	pipelineEmbedBlock
)

type pipelineEmbeddingServer struct {
	server    *httptest.Server
	mode      atomic.Int32
	calls     atomic.Int64
	entered   chan struct{}
	canceled  chan struct{}
	onceIn    sync.Once
	onceOut   sync.Once
	mu        sync.Mutex
	inputs    []string
	onRequest func([]string)
}

func newPipelineEmbeddingServer(t *testing.T) *pipelineEmbeddingServer {
	t.Helper()
	server := &pipelineEmbeddingServer{
		entered:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	server.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		server.calls.Add(1)
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		server.mu.Lock()
		server.inputs = append(server.inputs, request.Input...)
		onRequest := server.onRequest
		server.mu.Unlock()
		if onRequest != nil {
			onRequest(append([]string(nil), request.Input...))
		}
		server.onceIn.Do(func() { close(server.entered) })
		if server.mode.Load() == pipelineEmbedBlock {
			<-r.Context().Done()
			server.onceOut.Do(func() { close(server.canceled) })
			return
		}
		dims := 4
		if server.mode.Load() == pipelineEmbedWrongDimensions {
			dims = 3
		}
		response := struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}{}
		for index := range request.Input {
			response.Data = append(response.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: make([]float32, dims), Index: index})
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.server.Close)
	return server
}

func (s *pipelineEmbeddingServer) recordedInputs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inputs...)
}

type recordingPipelineObjects struct {
	objects.Store
	onPut func(string)
}

func (s *recordingPipelineObjects) Put(
	ctx context.Context,
	key string,
	reader io.Reader,
	size int64,
	contentType string,
) error {
	if err := s.Store.Put(ctx, key, reader, size, contentType); err != nil {
		return err
	}
	if s.onPut != nil {
		s.onPut(key)
	}
	return nil
}

type pipelineHarness struct {
	service *Service
	store   *pipelineStore
	objects objects.Store
	vector  *pipelineVector
	embed   *pipelineEmbeddingServer
	kb      *store.RAGKBRecord
	events  *pipelineTelemetryRecorder
}

type pipelineTelemetryRecorder struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (r *pipelineTelemetryRecorder) Record(_ context.Context, event telemetry.Event) {
	r.mu.Lock()
	r.events = append(r.events, event)
	r.mu.Unlock()
}

func (r *pipelineTelemetryRecorder) snapshot() []telemetry.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]telemetry.Event(nil), r.events...)
}

type recordingPipelineParser struct {
	calls    atomic.Int32
	cleanup  atomic.Int32
	warnings []document.ParseWarning
}

func (p *recordingPipelineParser) Parse(
	_ context.Context,
	source document.Source,
	options parse.ParseOptions,
) (*document.ParsedDocument, error) {
	p.calls.Add(1)
	if err := source.Validate(); err != nil {
		return nil, err
	}
	return document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser: document.ParserInfo{
			Name: "pipeline-test", Version: options.ParserVersion,
		},
		Units: []document.MarkdownUnit{{
			ID:       "unit_document_0000",
			Location: document.SourceLocation{Kind: document.LocationDocument},
			Markdown: "# Facade\n\nstreaming parser output\n",
		}},
		Warnings: append([]document.ParseWarning(nil), p.warnings...),
	}, nil, func() error {
		p.cleanup.Add(1)
		return nil
	}), nil
}

type pipelineStageRecorder struct {
	mu     sync.Mutex
	stages []string
}

func (r *pipelineStageRecorder) add(stage string) {
	r.mu.Lock()
	r.stages = append(r.stages, stage)
	r.mu.Unlock()
}

func (r *pipelineStageRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stages...)
}

type stagedPipelineParser struct {
	recorder *pipelineStageRecorder
	markdown string
	calls    atomic.Int32
	mu       sync.Mutex
	budgets  []*vision.TaskDocumentAIBudget
	images   []vision.ImageTranscriber
}

func (p *stagedPipelineParser) Parse(
	_ context.Context,
	source document.Source,
	options parse.ParseOptions,
) (*document.ParsedDocument, error) {
	p.calls.Add(1)
	if p.recorder != nil {
		p.recorder.add("parse")
	}
	p.mu.Lock()
	p.budgets = append(p.budgets, options.DocumentAIBudget)
	p.images = append(p.images, options.ImageTranscriber)
	p.mu.Unlock()
	return document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser:        document.ParserInfo{Name: "pipeline-staged", Version: options.ParserVersion},
		Units: []document.MarkdownUnit{{
			ID: "unit_document_0000", Location: document.SourceLocation{Kind: document.LocationDocument},
			Markdown: p.markdown,
		}},
	}, nil, nil), nil
}

func (p *stagedPipelineParser) firstImageTranscriber() vision.ImageTranscriber {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.images) == 0 {
		return nil
	}
	return p.images[0]
}

type stagedPipelineImageVision struct{}

func (*stagedPipelineImageVision) DescribeImage(
	context.Context,
	vision.NormalizedImageInput,
	*vision.TaskDocumentAIBudget,
) (vision.ImageDescription, error) {
	return vision.ImageDescription{Kind: "other", Caption: "unused", Confidence: 1}, nil
}

func (p *stagedPipelineParser) firstBudget() *vision.TaskDocumentAIBudget {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.budgets) == 0 {
		return nil
	}
	return p.budgets[0]
}

type stagedPipelineEnricher struct {
	recorder *pipelineStageRecorder
	mu       sync.Mutex
	budgets  []*vision.TaskDocumentAIBudget
}

type recordingPipelineTokenizer struct {
	mu     sync.Mutex
	values []string
}

func (t *recordingPipelineTokenizer) CountTokens(_ context.Context, value string) (int, error) {
	t.mu.Lock()
	t.values = append(t.values, value)
	t.mu.Unlock()
	return split.EstimateTokens(value), nil
}

func (t *recordingPipelineTokenizer) saw(value string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, candidate := range t.values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (e *stagedPipelineEnricher) Enrich(
	_ context.Context,
	block enrich.EnrichableBlock,
	budget *vision.TaskDocumentAIBudget,
) (enrich.Enhancement, error) {
	if e.recorder != nil {
		e.recorder.add("enrich")
	}
	e.mu.Lock()
	e.budgets = append(e.budgets, budget)
	e.mu.Unlock()
	if block.Kind != enrich.BlockTable {
		return enrich.Enhancement{}, fmt.Errorf("unexpected enrichment block %q", block.Kind)
	}
	return enrich.Enhancement{Kind: enrich.BlockTable, Table: &enrich.TableEnhancement{
		Topic: "service latency", Columns: []enrich.ColumnMeaning{{Name: "p95", Meaning: "95th percentile"}},
		KeyEntities: []string{"checkout"}, Units: []string{"ms"}, Summary: "checkout p95 is 42 ms",
	}}, nil
}

func (e *stagedPipelineEnricher) firstBudget() *vision.TaskDocumentAIBudget {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.budgets) == 0 {
		return nil
	}
	return e.budgets[0]
}

func newPipelineHarness(t *testing.T) *pipelineHarness {
	t.Helper()
	db := newRAGTestStore(t)
	st := &pipelineStore{DBStore: db}
	embedding := newPipelineEmbeddingServer(t)
	objectStore := objects.NewLocalFS(t.TempDir())
	vec := &pipelineVector{Fake: vector.NewFake()}
	events := &pipelineTelemetryRecorder{}
	service := New(Deps{
		Store: st, Vector: vec, Objects: objectStore,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.server.URL, Model: "embed-v1", Dims: 4},
		},
		Telemetry: events, Workers: 1,
	})
	// Race instrumentation can make a single SQLite transaction take well over
	// one second. Keep the lease comfortably above that bound and avoid a
	// high-frequency lifecycle/heartbeat write storm that would turn these
	// pipeline-contract tests into timing tests.
	service.pollInterval = 50 * time.Millisecond
	service.leaseDuration = 30 * time.Second
	service.heartbeatInterval = 100 * time.Millisecond

	kb := &store.RAGKBRecord{
		ID: "kb_" + strings.ReplaceAll(t.Name(), "/", "_"), UserID: "u_pipeline", Name: "pipeline",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 4,
		ChunkSize: 16, ChunkOverlap: 2, ParseMode: store.RAGParseModeStandard, Status: "active",
	}
	if err := st.CreateRAGKB(context.Background(), kb); err != nil {
		t.Fatalf("create pipeline KB: %v", err)
	}
	return &pipelineHarness{
		service: service, store: st, objects: objectStore, vector: vec, embed: embedding,
		kb: kb, events: events,
	}
}

type recordingTaskNotifier struct {
	calls chan int64
	err   error
}

type pipelineFairStore struct {
	FairStore
	writer              string
	getConfigFn         func(context.Context, string, string, string, string) (*store.ConfigRecord, error)
	getLifecycleKBFn    func(context.Context, string) (*store.RAGKBRecord, error)
	getLifecycleDocFn   func(context.Context, string) (*store.RAGDocumentRecord, error)
	listLifecycleDocsFn func(context.Context, string) ([]store.RAGDocumentRecord, error)
	getLifecycleUserFn  func(context.Context, string) (*store.UserRecord, error)
	beginOriginalFn     func(context.Context, store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error)
	markOriginalFn      func(context.Context, store.RAGObjectWriteFence) (bool, error)
	createBudgetFn      func(context.Context, store.IndexFence, *store.RAGDocumentAITaskBudgetRecord) error
	reserveUsageFn      func(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error)
	getUsageFn          func(context.Context, string) (*store.RAGDocumentAIUsageRecord, error)
	markUsageFn         func(context.Context, string, store.IndexFence) (bool, error)
	commitUsageFn       func(context.Context, string, int64, int64, int64, bool) (bool, error)
	releaseUsageFn      func(context.Context, string) (bool, error)
	reconcileFn         func(context.Context, time.Time, time.Time, int) (int, error)
}

func (s *pipelineFairStore) ExpectedWriterFingerprint() string { return s.writer }

func (s *pipelineFairStore) GetConfigByName(
	ctx context.Context,
	kind, userID, agentID, name string,
) (*store.ConfigRecord, error) {
	if s.getConfigFn == nil {
		return nil, store.ErrNotFound
	}
	return s.getConfigFn(ctx, kind, userID, agentID, name)
}

func (s *pipelineFairStore) GetRAGKBForLifecycle(
	ctx context.Context,
	id string,
) (*store.RAGKBRecord, error) {
	return s.getLifecycleKBFn(ctx, id)
}

func (s *pipelineFairStore) GetRAGDocumentForLifecycle(
	ctx context.Context,
	id string,
) (*store.RAGDocumentRecord, error) {
	return s.getLifecycleDocFn(ctx, id)
}

func (s *pipelineFairStore) ListRAGDocumentsByKBForLifecycle(
	ctx context.Context,
	kbID string,
) ([]store.RAGDocumentRecord, error) {
	return s.listLifecycleDocsFn(ctx, kbID)
}

func (s *pipelineFairStore) GetUserForRAGLifecycle(
	ctx context.Context,
	id string,
) (*store.UserRecord, error) {
	return s.getLifecycleUserFn(ctx, id)
}

func (s *pipelineFairStore) BeginOriginalRAGObjectWrite(
	ctx context.Context,
	request store.RAGObjectWriteRequest,
) (*store.RAGObjectWriteFence, error) {
	return s.beginOriginalFn(ctx, request)
}

func (s *pipelineFairStore) MarkOriginalRAGObjectWriteReady(
	ctx context.Context,
	fence store.RAGObjectWriteFence,
) (bool, error) {
	return s.markOriginalFn(ctx, fence)
}

func (s *pipelineFairStore) CreateRAGDocumentAITaskBudgetForIndex(
	ctx context.Context,
	fence store.IndexFence,
	budget *store.RAGDocumentAITaskBudgetRecord,
) error {
	return s.createBudgetFn(ctx, fence, budget)
}

func (s *pipelineFairStore) ReserveRAGDocumentAIUsage(
	ctx context.Context,
	fence store.IndexFence,
	usage *store.RAGDocumentAIUsageRecord,
	limits store.RAGDocumentAILimits,
) (bool, error) {
	return s.reserveUsageFn(ctx, fence, usage, limits)
}

func (s *pipelineFairStore) ReconcileRAGDocumentAIUsage(
	ctx context.Context,
	reservedBefore, sentBefore time.Time,
	limit int,
) (int, error) {
	return s.reconcileFn(ctx, reservedBefore, sentBefore, limit)
}

func (s *pipelineFairStore) GetRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (*store.RAGDocumentAIUsageRecord, error) {
	return s.getUsageFn(ctx, idempotencyKey)
}

func (s *pipelineFairStore) MarkSentRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	fence store.IndexFence,
) (bool, error) {
	return s.markUsageFn(ctx, idempotencyKey, fence)
}

func (s *pipelineFairStore) CommitRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	inputTokens, outputTokens, costMicroUSD int64,
	estimated bool,
) (bool, error) {
	return s.commitUsageFn(ctx, idempotencyKey, inputTokens, outputTokens, costMicroUSD, estimated)
}

func (s *pipelineFairStore) ReleaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (bool, error) {
	return s.releaseUsageFn(ctx, idempotencyKey)
}

type pipelineFairExecutionStore struct {
	FairExecutionStore
	publishAssetsFn func(context.Context, store.IndexFence, []store.RAGAssetRecord, []string) (bool, error)
	publishAllFn    func(context.Context, store.IndexFence, []store.RAGAssetRecord, []string, []store.RAGAttachmentRecord, []string) (bool, error)
}

func (s *pipelineFairExecutionStore) PublishRAGAssetsForIndex(
	ctx context.Context,
	fence store.IndexFence,
	assets []store.RAGAssetRecord,
	assetIDs []string,
) (bool, error) {
	return s.publishAssetsFn(ctx, fence, assets, assetIDs)
}

func (s *pipelineFairExecutionStore) PublishRAGAssetsAndAttachmentsForIndex(
	ctx context.Context,
	fence store.IndexFence,
	assets []store.RAGAssetRecord,
	assetIDs []string,
	attachments []store.RAGAttachmentRecord,
	attachmentIDs []string,
) (bool, error) {
	return s.publishAllFn(ctx, fence, assets, assetIDs, attachments, attachmentIDs)
}

func (n *recordingTaskNotifier) TryDispatch(_ context.Context, taskID int64) error {
	if n.calls != nil {
		n.calls <- taskID
	}
	return n.err
}

func TestServiceStartWorkerModesKeepMaintenanceButOnlyLegacyClaims(t *testing.T) {
	for _, test := range []struct {
		name       string
		mode       WorkerMode
		wantClaims bool
	}{
		{name: "legacy", mode: WorkerModeLegacy, wantClaims: true},
		{name: "paused", mode: WorkerModePaused},
		{name: "fair", mode: WorkerModeFair},
		{name: "unknown fails closed", mode: WorkerMode("unknown")},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPipelineHarness(t)
			h.service.workerMode = test.mode
			h.service.pollInterval = 5 * time.Millisecond
			var claims atomic.Int64
			var reconciles atomic.Int64
			h.store.claimFn = func(context.Context, string, time.Duration) (*store.RAGIndexClaim, error) {
				claims.Add(1)
				return nil, nil
			}
			h.store.reconcileFn = func(context.Context, time.Time, time.Time, int) (int, error) {
				reconciles.Add(1)
				return 0, nil
			}

			ctx, cancel := context.WithCancel(context.Background())
			h.service.Start(ctx)
			defer cancel()
			deadline := time.Now().Add(time.Second)
			for reconciles.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if reconciles.Load() == 0 {
				t.Fatal("maintenance did not start")
			}
			if test.wantClaims {
				for claims.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if claims.Load() == 0 {
					t.Fatal("legacy index worker did not claim")
				}
				return
			}
			time.Sleep(30 * time.Millisecond)
			if got := claims.Load(); got != 0 {
				t.Fatalf("mode %q started legacy claim loop: %d", test.mode, got)
			}
		})
	}
}

func TestTaskNotificationFollowsWorkerMode(t *testing.T) {
	for _, test := range []struct {
		name         string
		mode         WorkerMode
		wantLocal    bool
		wantDispatch bool
	}{
		{name: "legacy", mode: WorkerModeLegacy, wantLocal: true},
		{name: "paused", mode: WorkerModePaused},
		{name: "fair", mode: WorkerModeFair, wantDispatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newPipelineHarness(t)
			h.service.workerMode = test.mode
			h.service.tasks = make(chan int64, 1)
			notifier := &recordingTaskNotifier{calls: make(chan int64, 1), err: errors.New("fast path unavailable")}
			h.service.taskNotifier = notifier

			h.service.notifyTask(context.Background(), 42)
			select {
			case taskID := <-h.service.tasks:
				if !test.wantLocal || taskID != 42 {
					t.Fatalf("unexpected local notification %d", taskID)
				}
			default:
				if test.wantLocal {
					t.Fatal("legacy task was not scheduled locally")
				}
			}
			select {
			case taskID := <-notifier.calls:
				if !test.wantDispatch || taskID != 42 {
					t.Fatalf("unexpected fair dispatch %d", taskID)
				}
			default:
				if test.wantDispatch {
					t.Fatal("fair task was not offered to dispatcher")
				}
			}
		})
	}
}

func TestFairEmbeddingConfigLookupUsesExpectedWriterFacade(t *testing.T) {
	h := newPipelineHarness(t)
	fingerprint := strings.Repeat("5", 64)
	var legacyCalls atomic.Int64
	var fairCalls atomic.Int64
	h.service.workerMode = WorkerModeFair
	h.service.userCfg = func(context.Context, string) (config.RAGEmbeddingCfg, bool) {
		legacyCalls.Add(1)
		return config.RAGEmbeddingCfg{
			Endpoint: "https://wrong-writer.example/v1", Model: "wrong", Dims: 8,
		}, true
	}
	h.service.fairStore = &pipelineFairStore{
		writer: fingerprint,
		getConfigFn: func(_ context.Context, kind, userID, agentID, name string) (*store.ConfigRecord, error) {
			fairCalls.Add(1)
			if kind != store.KindSetting || userID != "user-fair-config" || agentID != "" || name != "rag" {
				t.Fatalf("config lookup=%q/%q/%q/%q", kind, userID, agentID, name)
			}
			return nil, store.ErrFairQueueWriterMismatch
		},
	}
	_, err := h.service.embeddingConfigForKB(context.Background(), &store.RAGKBRecord{
		ID: "kb-fair-config", UserID: "user-fair-config", EmbedProvider: "user",
	})
	if !errors.Is(err, store.ErrFairQueueWriterMismatch) {
		t.Fatalf("pre-claim embeddingConfigForKB error=%v, want writer mismatch", err)
	}
	fence := store.IndexFence{
		TaskID: 80, DocID: "doc_fair_config", DocVersion: 1, ClaimGeneration: 1,
		LeaseOwner: "fair-worker", ExpectedWriterFingerprint: fingerprint,
	}
	_, err = h.service.embeddingConfigForKB(withFairIndexFence(context.Background(), fence), &store.RAGKBRecord{
		ID: "kb-fair-config", UserID: "user-fair-config", EmbedProvider: "user",
	})
	if !errors.Is(err, store.ErrFairQueueWriterMismatch) {
		t.Fatalf("embeddingConfigForKB error=%v, want writer mismatch", err)
	}
	if got := legacyCalls.Load(); got != 0 {
		t.Fatalf("fair config lookup fell back to legacy pool %d times", got)
	}
	if got := fairCalls.Load(); got != 2 {
		t.Fatalf("expected-writer config lookup calls=%d, want 2", got)
	}
	h.service.fairStore = &pipelineFairStore{
		writer: fingerprint,
		getConfigFn: func(context.Context, string, string, string, string) (*store.ConfigRecord, error) {
			return &store.ConfigRecord{Data: map[string]any{"embedding": map[string]any{
				"endpoint": "https://right-writer.example/v1", "model": "embed-right", "dims": 12,
			}}}, nil
		},
	}
	cfg, err := h.service.embeddingConfigForKB(withFairIndexFence(context.Background(), fence), &store.RAGKBRecord{
		ID: "kb-fair-config", UserID: "user-fair-config", EmbedProvider: "user",
	})
	if err != nil || cfg.Endpoint != "https://right-writer.example/v1" || cfg.Model != "embed-right" || cfg.Dims != 12 {
		t.Fatalf("pinned embedding config=%+v err=%v", cfg, err)
	}
	if got := legacyCalls.Load(); got != 0 {
		t.Fatalf("successful fair config lookup used legacy pool %d times", got)
	}
}

func TestFairLifecycleReadsUseExpectedWriterFacade(t *testing.T) {
	h := newPipelineHarness(t)
	var calls atomic.Int64
	fair := &pipelineFairStore{
		writer: strings.Repeat("6", 64),
		getLifecycleKBFn: func(_ context.Context, id string) (*store.RAGKBRecord, error) {
			calls.Add(1)
			return &store.RAGKBRecord{ID: id, UserID: "fair-user", Name: "pinned-kb"}, nil
		},
		getLifecycleDocFn: func(_ context.Context, id string) (*store.RAGDocumentRecord, error) {
			calls.Add(1)
			return &store.RAGDocumentRecord{ID: id, KBID: h.kb.ID, FileName: "pinned.md"}, nil
		},
		listLifecycleDocsFn: func(_ context.Context, kbID string) ([]store.RAGDocumentRecord, error) {
			calls.Add(1)
			return []store.RAGDocumentRecord{{ID: "doc-pinned", KBID: kbID}}, nil
		},
		getLifecycleUserFn: func(_ context.Context, id string) (*store.UserRecord, error) {
			calls.Add(1)
			return &store.UserRecord{ID: id, Status: "active", DisplayName: "pinned-user"}, nil
		},
	}
	wrapped := &modeAwareRAGStore{Store: h.store, mode: WorkerModeFair, fairStore: fair}
	kb, err := wrapped.GetRAGKB(context.Background(), h.kb.ID)
	if err != nil || kb.Name != "pinned-kb" {
		t.Fatalf("pinned KB=%+v err=%v", kb, err)
	}
	doc, err := wrapped.GetRAGDocument(context.Background(), "doc-pinned")
	if err != nil || doc.FileName != "pinned.md" {
		t.Fatalf("pinned document=%+v err=%v", doc, err)
	}
	docs, err := wrapped.ListRAGDocumentsByKB(context.Background(), h.kb.ID)
	if err != nil || len(docs) != 1 || docs[0].ID != "doc-pinned" {
		t.Fatalf("pinned documents=%+v err=%v", docs, err)
	}
	user, err := wrapped.GetUser(context.Background(), "fair-user")
	if err != nil || user.DisplayName != "pinned-user" {
		t.Fatalf("pinned user=%+v err=%v", user, err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("pinned lifecycle read calls=%d, want 4", got)
	}

	fair.getLifecycleKBFn = func(context.Context, string) (*store.RAGKBRecord, error) {
		return nil, store.ErrFairQueueWriterMismatch
	}
	if record, err := wrapped.GetRAGKB(context.Background(), h.kb.ID); record != nil ||
		!errors.Is(err, store.ErrFairQueueWriterMismatch) {
		t.Fatalf("writer-switch KB=%+v err=%v", record, err)
	}
}

func TestFairPipelineStagesChunksThroughAtomicFencedStore(t *testing.T) {
	h := newPipelineHarness(t)
	h.service.st = &modeAwareRAGStore{
		Store: h.store, mode: WorkerModeFair, fairExecution: h.store,
	}
	fence := store.IndexFence{
		TaskID: 71, DocID: "doc_fair_stage", DocVersion: 4, ClaimGeneration: 5,
		LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("a", 64),
	}
	h.store.checkFenceFn = func(context.Context, store.IndexFence) (bool, error) {
		return false, errors.New("fair staging used CheckFence TOCTOU")
	}
	h.store.putChunksFn = func(context.Context, []store.RAGChunkRecord) error {
		return errors.New("fair staging used legacy pool mutation")
	}
	var fencedCalls atomic.Int64
	h.store.putChunksForIndexFn = func(_ context.Context, got store.IndexFence, chunks []store.RAGChunkRecord) (bool, error) {
		if got != fence || len(chunks) != 1 || chunks[0].DocID != fence.DocID || chunks[0].DocVersion != fence.DocVersion {
			t.Fatalf("fenced stage got fence=%+v chunks=%+v", got, chunks)
		}
		fencedCalls.Add(1)
		return true, nil
	}

	_, err := h.service.stageIndexVersion(
		withFairIndexFence(context.Background(), fence), fence, h.kb.ID, fence.DocID,
		[]split.Chunk{{Index: 0, RawContent: "source", SearchContent: "source", Tokens: 1}},
		[][]float32{{1, 2, 3, 4}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := fencedCalls.Load(); got != 1 {
		t.Fatalf("fenced chunk writes=%d, want 1", got)
	}
}

func TestFairExecutionWritesRejectMissingClaimContext(t *testing.T) {
	h := newPipelineHarness(t)
	var legacyCalls atomic.Int64
	h.store.putChunksFn = func(context.Context, []store.RAGChunkRecord) error {
		legacyCalls.Add(1)
		return nil
	}
	h.store.putChunkAssetsFn = func(context.Context, []store.RAGChunkAssetRecord) error {
		legacyCalls.Add(1)
		return nil
	}
	h.store.registerCacheFn = func(context.Context, store.RAGCacheObjectRecord) error {
		legacyCalls.Add(1)
		return nil
	}
	h.store.beginObjectFn = func(_ context.Context, request store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error) {
		legacyCalls.Add(1)
		return &store.RAGObjectWriteFence{ObjectKind: request.ObjectKind}, nil
	}
	h.store.markObjectFn = func(context.Context, store.RAGObjectWriteFence) (bool, error) {
		legacyCalls.Add(1)
		return true, nil
	}
	var fairLifecycleCalls atomic.Int64
	fairStore := &pipelineFairStore{
		writer: strings.Repeat("a", 64),
		beginOriginalFn: func(_ context.Context, request store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error) {
			fairLifecycleCalls.Add(1)
			return &store.RAGObjectWriteFence{ObjectKind: request.ObjectKind}, nil
		},
		markOriginalFn: func(context.Context, store.RAGObjectWriteFence) (bool, error) {
			fairLifecycleCalls.Add(1)
			return true, nil
		},
	}
	wrapped := &modeAwareRAGStore{
		Store: h.store, mode: WorkerModeFair, fairStore: fairStore, fairExecution: h.store,
	}
	ctx := context.Background()
	assertFenceError := func(name string, err error) {
		t.Helper()
		if !errors.Is(err, store.ErrRAGDocumentVersionMismatch) {
			t.Fatalf("%s error=%v, want missing fair claim fence", name, err)
		}
	}
	assertFenceError("chunks", wrapped.PutRAGChunks(ctx, []store.RAGChunkRecord{{DocID: "doc"}}))
	assertFenceError("chunk assets", wrapped.PutRAGChunkAssets(ctx, []store.RAGChunkAssetRecord{{DocID: "doc"}}))
	assertFenceError("cache", wrapped.RegisterRAGCacheObject(ctx, store.RAGCacheObjectRecord{DocID: "doc"}))
	_, err := wrapped.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{ObjectKind: store.RAGObjectKindAssetSource})
	assertFenceError("asset object begin", err)
	_, err = wrapped.MarkRAGObjectWriteReady(ctx, store.RAGObjectWriteFence{ObjectKind: store.RAGObjectKindAssetSource})
	assertFenceError("asset object ready", err)
	if got := legacyCalls.Load(); got != 0 {
		t.Fatalf("missing fair claim context reached legacy pool %d times", got)
	}

	// Upload's original object staging precedes creation of a claim, so it uses
	// the expected-writer-bound fair lifecycle facade rather than a claim fence.
	original, err := wrapped.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{ObjectKind: store.RAGObjectKindOriginal})
	if err != nil || original == nil {
		t.Fatalf("original object begin=%+v,%v", original, err)
	}
	if ok, err := wrapped.MarkRAGObjectWriteReady(ctx, *original); err != nil || !ok {
		t.Fatalf("original object ready=%v,%v", ok, err)
	}
	if got := legacyCalls.Load(); got != 0 {
		t.Fatalf("fair original staging reached legacy pool %d times", got)
	}
	if got := fairLifecycleCalls.Load(); got != 2 {
		t.Fatalf("fair original staging calls=%d, want 2", got)
	}
}

func TestFairExecutionDocumentReadRejectsMismatchedRequestedID(t *testing.T) {
	h := newPipelineHarness(t)
	fence := store.IndexFence{
		TaskID: 74, DocID: "doc_expected", DocVersion: 1, ClaimGeneration: 1,
		LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("c", 64),
	}
	var calls atomic.Int64
	h.store.getDocumentForIndex = func(context.Context, store.IndexFence) (*store.RAGDocumentRecord, error) {
		calls.Add(1)
		return &store.RAGDocumentRecord{ID: fence.DocID}, nil
	}
	wrapped := &modeAwareRAGStore{Store: h.store, mode: WorkerModeFair, fairExecution: h.store}
	_, err := wrapped.GetRAGDocument(withFairIndexFence(context.Background(), fence), "doc_other")
	if !errors.Is(err, store.ErrRAGDocumentVersionMismatch) {
		t.Fatalf("GetRAGDocument error=%v, want identity mismatch", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("mismatched ID reached fair store %d times", got)
	}
}

func TestFairExecutionPublishesAssetsThroughExplicitFencedSurface(t *testing.T) {
	h := newPipelineHarness(t)
	fence := store.IndexFence{
		TaskID: 75, DocID: "doc_assets", DocVersion: 3, ClaimGeneration: 4,
		LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("d", 64),
	}
	var assetCalls atomic.Int64
	var allCalls atomic.Int64
	fair := &pipelineFairExecutionStore{
		publishAssetsFn: func(_ context.Context, got store.IndexFence, _ []store.RAGAssetRecord, ids []string) (bool, error) {
			if got != fence || !reflect.DeepEqual(ids, []string{"asset-1"}) {
				t.Fatalf("asset publish fence=%+v ids=%v", got, ids)
			}
			assetCalls.Add(1)
			return true, nil
		},
		publishAllFn: func(_ context.Context, got store.IndexFence, _ []store.RAGAssetRecord, assetIDs []string, _ []store.RAGAttachmentRecord, attachmentIDs []string) (bool, error) {
			if got != fence || !reflect.DeepEqual(assetIDs, []string{"asset-1"}) || !reflect.DeepEqual(attachmentIDs, []string{"attachment-1"}) {
				t.Fatalf("combined publish fence=%+v assets=%v attachments=%v", got, assetIDs, attachmentIDs)
			}
			allCalls.Add(1)
			return true, nil
		},
	}
	wrapped := &modeAwareRAGStore{Store: h.store, mode: WorkerModeFair, fairExecution: fair}
	ctx := withFairIndexFence(context.Background(), fence)
	if ok, err := wrapped.PublishRAGAssetsForIndex(ctx, fence, nil, []string{"asset-1"}); err != nil || !ok {
		t.Fatalf("PublishRAGAssetsForIndex=%v,%v", ok, err)
	}
	if ok, err := wrapped.PublishRAGAssetsAndAttachmentsForIndex(
		ctx, fence, nil, []string{"asset-1"}, nil, []string{"attachment-1"},
	); err != nil || !ok {
		t.Fatalf("PublishRAGAssetsAndAttachmentsForIndex=%v,%v", ok, err)
	}
	if assetCalls.Load() != 1 || allCalls.Load() != 1 {
		t.Fatalf("fenced publish calls assets=%d all=%d", assetCalls.Load(), allCalls.Load())
	}
}

func TestFairExecutionCacheCatalogRoutesByClaimContext(t *testing.T) {
	h := newPipelineHarness(t)
	fence := store.IndexFence{
		TaskID: 79, DocID: "doc_cache", DocVersion: 2, ClaimGeneration: 3,
		LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("4", 64),
	}
	record := store.RAGCacheObjectRecord{DocID: fence.DocID}
	var legacyCalls atomic.Int64
	var fairCalls atomic.Int64
	h.store.registerCacheFn = func(_ context.Context, got store.RAGCacheObjectRecord) error {
		if got.DocID != record.DocID {
			t.Fatalf("legacy cache record=%+v", got)
		}
		legacyCalls.Add(1)
		return nil
	}
	h.store.registerCacheFairFn = func(_ context.Context, gotFence store.IndexFence, got store.RAGCacheObjectRecord) error {
		if gotFence != fence || got.DocID != record.DocID {
			t.Fatalf("fair cache fence=%+v record=%+v", gotFence, got)
		}
		fairCalls.Add(1)
		return nil
	}
	catalog := NewFairExecutionCacheCatalog(h.store, h.store, WorkerModeFair)
	if err := catalog.RegisterRAGCacheObject(context.Background(), record); !errors.Is(err, store.ErrRAGDocumentVersionMismatch) {
		t.Fatalf("missing fair cache context error=%v", err)
	}
	if err := catalog.RegisterRAGCacheObject(withFairIndexFence(context.Background(), fence), record); err != nil {
		t.Fatal(err)
	}
	if legacyCalls.Load() != 0 || fairCalls.Load() != 1 {
		t.Fatalf("cache calls legacy=%d fair=%d", legacyCalls.Load(), fairCalls.Load())
	}
	legacyCatalog := NewFairExecutionCacheCatalog(h.store, h.store, WorkerModeLegacy)
	if err := legacyCatalog.RegisterRAGCacheObject(context.Background(), record); err != nil {
		t.Fatal(err)
	}

	h.store.registerCacheFairFn = func(context.Context, store.IndexFence, store.RAGCacheObjectRecord) error {
		return store.ErrFairQueueUnsafeConnection
	}
	if err := catalog.RegisterRAGCacheObject(
		withFairIndexFence(context.Background(), fence), record,
	); !errors.Is(err, store.ErrFairQueueUnsafeConnection) {
		t.Fatalf("fair cache safety error=%v", err)
	}
	missingFair := NewFairExecutionCacheCatalog(h.store, nil, WorkerModeFair)
	if err := missingFair.RegisterRAGCacheObject(
		withFairIndexFence(context.Background(), fence), record,
	); !errors.Is(err, store.ErrFairQueueMySQLRequired) {
		t.Fatalf("missing fair cache error=%v", err)
	}
	if legacyCalls.Load() != 1 {
		t.Fatalf("fair cache fallback used legacy catalog %d times", legacyCalls.Load())
	}
}

func TestTaskDocumentAIBudgetRoutesLegacyAndFairCreates(t *testing.T) {
	newClaim := func(fingerprint string) *store.RAGIndexClaim {
		return &store.RAGIndexClaim{
			Fence: store.IndexFence{
				TaskID: 76, DocID: "doc_budget", DocVersion: 2, ClaimGeneration: 3,
				LeaseOwner: "budget-worker", ExpectedWriterFingerprint: fingerprint,
			},
			Version: store.RAGDocumentVersionRecord{
				MaxDocumentAIRequests: 10, MaxDocumentAITokens: 100,
				MaxDocumentAICostMicroUSD: 100,
			},
		}
	}
	request := vision.AttemptRequest{
		LogicalRequestKey: "budget-route", Operation: vision.OperationPage,
		ProviderFingerprint: strings.Repeat("e", 64), Attempt: 1, InputTokens: 1,
	}

	t.Run("legacy unchanged", func(t *testing.T) {
		h := newPipelineHarness(t)
		var creates atomic.Int64
		var reserves atomic.Int64
		h.store.createBudgetFn = func(context.Context, *store.RAGDocumentAITaskBudgetRecord) error {
			creates.Add(1)
			return nil
		}
		h.store.reserveUsageFn = func(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error) {
			reserves.Add(1)
			return true, nil
		}
		claim := newClaim("")
		budget, err := h.service.newTaskDocumentAIBudget(claim, "u_pipeline")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := budget.Reserve(context.Background(), claim.Fence, request); err != nil {
			t.Fatal(err)
		}
		if creates.Load() != 1 || reserves.Load() != 1 {
			t.Fatalf("legacy budget calls create=%d reserve=%d", creates.Load(), reserves.Load())
		}
	})

	t.Run("fair uses fenced facade", func(t *testing.T) {
		h := newPipelineHarness(t)
		fingerprint := strings.Repeat("f", 64)
		claim := newClaim(fingerprint)
		var creates atomic.Int64
		var reserves atomic.Int64
		fair := &pipelineFairStore{
			writer: fingerprint,
			createBudgetFn: func(_ context.Context, got store.IndexFence, record *store.RAGDocumentAITaskBudgetRecord) error {
				if got != claim.Fence || record.TaskID != claim.Fence.TaskID || record.UserID != "u_pipeline" {
					t.Fatalf("fair budget create fence=%+v record=%+v", got, record)
				}
				creates.Add(1)
				return nil
			},
			reserveUsageFn: func(_ context.Context, got store.IndexFence, _ *store.RAGDocumentAIUsageRecord, _ store.RAGDocumentAILimits) (bool, error) {
				if got != claim.Fence {
					t.Fatalf("fair reserve fence=%+v", got)
				}
				reserves.Add(1)
				return true, nil
			},
		}
		h.service.st = &modeAwareRAGStore{
			Store: h.store, mode: WorkerModeFair, fairStore: fair, fairExecution: h.store,
		}
		h.service.fairStore = fair
		budget, err := h.service.newTaskDocumentAIBudget(claim, "u_pipeline")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := budget.Reserve(context.Background(), claim.Fence, request); err != nil {
			t.Fatal(err)
		}
		if creates.Load() != 1 || reserves.Load() != 1 {
			t.Fatalf("fair budget calls create=%d reserve=%d", creates.Load(), reserves.Load())
		}
	})

	t.Run("fair ledger remains bound across root contexts", func(t *testing.T) {
		h := newPipelineHarness(t)
		fingerprint := strings.Repeat("0", 64)
		claim := newClaim(fingerprint)
		forbidLegacy := func(name string) {
			t.Fatalf("fair budget called legacy %s", name)
		}
		h.store.createBudgetFn = func(context.Context, *store.RAGDocumentAITaskBudgetRecord) error {
			forbidLegacy("create")
			return nil
		}
		h.store.reserveUsageFn = func(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error) {
			forbidLegacy("reserve")
			return false, nil
		}
		h.store.getUsageFn = func(context.Context, string) (*store.RAGDocumentAIUsageRecord, error) {
			forbidLegacy("get")
			return nil, nil
		}
		h.store.markUsageFn = func(context.Context, string, store.IndexFence) (bool, error) {
			forbidLegacy("mark sent")
			return false, nil
		}
		h.store.commitUsageFn = func(context.Context, string, int64, int64, int64, bool) (bool, error) {
			forbidLegacy("commit")
			return false, nil
		}
		h.store.releaseUsageFn = func(context.Context, string) (bool, error) {
			forbidLegacy("release")
			return false, nil
		}
		var reserveCalls atomic.Int64
		var getCalls atomic.Int64
		var markCalls atomic.Int64
		var commitCalls atomic.Int64
		var releaseCalls atomic.Int64
		fair := &pipelineFairStore{
			writer: fingerprint,
			createBudgetFn: func(context.Context, store.IndexFence, *store.RAGDocumentAITaskBudgetRecord) error {
				return nil
			},
			reserveUsageFn: func(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error) {
				return reserveCalls.Add(1) > 1, nil
			},
			getUsageFn: func(_ context.Context, key string) (*store.RAGDocumentAIUsageRecord, error) {
				getCalls.Add(1)
				return &store.RAGDocumentAIUsageRecord{IdempotencyKey: key, State: store.RAGDocumentAIUsageReserved}, nil
			},
			markUsageFn: func(context.Context, string, store.IndexFence) (bool, error) {
				markCalls.Add(1)
				return true, nil
			},
			commitUsageFn: func(context.Context, string, int64, int64, int64, bool) (bool, error) {
				commitCalls.Add(1)
				return true, nil
			},
			releaseUsageFn: func(context.Context, string) (bool, error) {
				releaseCalls.Add(1)
				return true, nil
			},
		}
		h.service.st = &modeAwareRAGStore{
			Store: h.store, mode: WorkerModeFair, fairStore: fair, fairExecution: h.store,
		}
		h.service.fairStore = fair
		budget, err := h.service.newTaskDocumentAIBudget(claim, "u_pipeline")
		if err != nil {
			t.Fatal(err)
		}
		first, err := budget.Reserve(context.Background(), claim.Fence, request)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
		secondRequest := request
		secondRequest.LogicalRequestKey = "budget-route-second"
		second, err := budget.Reserve(context.Background(), claim.Fence, secondRequest)
		if err != nil {
			t.Fatal(err)
		}
		if err := second.MarkSent(context.Background(), claim.Fence); err != nil {
			t.Fatal(err)
		}
		if err := second.Commit(context.Background(), vision.Usage{InputTokens: 1}); err != nil {
			t.Fatal(err)
		}
		if reserveCalls.Load() != 2 || getCalls.Load() != 1 || markCalls.Load() != 1 ||
			commitCalls.Load() != 1 || releaseCalls.Load() != 1 {
			t.Fatalf("fair ledger calls reserve=%d get=%d mark=%d commit=%d release=%d",
				reserveCalls.Load(), getCalls.Load(), markCalls.Load(), commitCalls.Load(), releaseCalls.Load())
		}
	})

	for _, safetyErr := range []error{store.ErrFairQueueWriterMismatch, store.ErrFairQueueUnsafeConnection} {
		t.Run(safetyErr.Error(), func(t *testing.T) {
			h := newPipelineHarness(t)
			fingerprint := strings.Repeat("1", 64)
			claim := newClaim(fingerprint)
			fair := &pipelineFairStore{
				writer: fingerprint,
				createBudgetFn: func(context.Context, store.IndexFence, *store.RAGDocumentAITaskBudgetRecord) error {
					return safetyErr
				},
				reserveUsageFn: func(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error) {
					t.Fatal("reserve ran after fenced create failed")
					return false, nil
				},
			}
			h.service.st = &modeAwareRAGStore{
				Store: h.store, mode: WorkerModeFair, fairStore: fair, fairExecution: h.store,
			}
			h.service.fairStore = fair
			budget, err := h.service.newTaskDocumentAIBudget(claim, "u_pipeline")
			if err != nil {
				t.Fatal(err)
			}
			ctx := withFairIndexFence(context.Background(), claim.Fence)
			if _, err := budget.Reserve(ctx, claim.Fence, request); !errors.Is(err, safetyErr) {
				t.Fatalf("Reserve error=%v, want %v", err, safetyErr)
			}
		})
	}
}

func TestDocumentAIReconcileUsesBoundFacadeOnlyInFairMode(t *testing.T) {
	t.Run("fair", func(t *testing.T) {
		h := newPipelineHarness(t)
		fingerprint := strings.Repeat("2", 64)
		fair := &pipelineFairStore{
			writer: fingerprint,
			reconcileFn: func(context.Context, time.Time, time.Time, int) (int, error) {
				return 0, store.ErrFairQueueUnsafeConnection
			},
		}
		wrapped := &modeAwareRAGStore{Store: h.store, mode: WorkerModeFair, fairStore: fair}
		if _, err := wrapped.ReconcileRAGDocumentAIUsage(
			context.Background(), time.Now(), time.Now(), 10,
		); !errors.Is(err, store.ErrFairQueueUnsafeConnection) {
			t.Fatalf("fair reconcile error=%v", err)
		}
	})

	t.Run("legacy", func(t *testing.T) {
		h := newPipelineHarness(t)
		var legacyCalls atomic.Int64
		h.store.reconcileFn = func(context.Context, time.Time, time.Time, int) (int, error) {
			legacyCalls.Add(1)
			return 7, nil
		}
		wrapped := &modeAwareRAGStore{Store: h.store, mode: WorkerModeLegacy}
		count, err := wrapped.ReconcileRAGDocumentAIUsage(
			context.Background(), time.Now(), time.Now(), 10,
		)
		if err != nil || count != 7 || legacyCalls.Load() != 1 {
			t.Fatalf("legacy reconcile=%d,%v calls=%d", count, err, legacyCalls.Load())
		}
	})
}

func TestRunFairClaimPropagatesWriterMismatchWithoutRetry(t *testing.T) {
	h := newPipelineHarness(t)
	h.service.workerMode = WorkerModeFair
	h.service.heartbeatInterval = time.Hour
	fair := &pipelineFairStore{writer: strings.Repeat("b", 64)}
	h.service.fairStore = fair
	h.service.st = &modeAwareRAGStore{
		Store: h.store, mode: WorkerModeFair, fairStore: fair, fairExecution: h.store,
	}
	h.store.getDocumentForIndex = func(context.Context, store.IndexFence) (*store.RAGDocumentRecord, error) {
		return nil, store.ErrFairQueueWriterMismatch
	}
	h.store.acknowledgeFn = func(context.Context, store.IndexFence) (bool, error) { return true, nil }
	var finalizes atomic.Int64
	h.store.retryFn = func(context.Context, store.IndexFence, string, time.Duration) (bool, error) {
		finalizes.Add(1)
		return true, nil
	}
	h.store.failFn = func(context.Context, store.IndexFence, string) (bool, error) {
		finalizes.Add(1)
		return true, nil
	}
	claim := &store.RAGIndexClaim{
		Fence: store.IndexFence{
			TaskID: 72, DocID: "doc_writer_mismatch", DocVersion: 1, ClaimGeneration: 2,
			LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("b", 64),
		},
		Task: store.RAGIndexTaskRecord{ID: 72, MaxRetry: 3},
	}
	if err := h.service.RunFairClaim(context.Background(), claim); !errors.Is(err, store.ErrFairQueueWriterMismatch) {
		t.Fatalf("RunFairClaim error=%v, want writer mismatch", err)
	}
	if got := finalizes.Load(); got != 0 {
		t.Fatalf("writer mismatch was persisted as ordinary retry/failure %d times", got)
	}
}

func TestFinishFairClaimTreatsCanonicalCorruptionAsFatal(t *testing.T) {
	for _, domainErr := range []error{
		store.ErrRAGDocumentAILedgerCorrupt,
		store.ErrRAGDocumentVersionConflict,
		store.ErrRAGDocumentSourceConflict,
	} {
		t.Run(domainErr.Error(), func(t *testing.T) {
			h := newPipelineHarness(t)
			var finalizes atomic.Int64
			h.store.retryFn = func(context.Context, store.IndexFence, string, time.Duration) (bool, error) {
				finalizes.Add(1)
				return true, nil
			}
			h.store.failFn = func(context.Context, store.IndexFence, string) (bool, error) {
				finalizes.Add(1)
				return true, nil
			}
			claim := &store.RAGIndexClaim{Fence: store.IndexFence{
				TaskID: 79, DocID: "doc_canonical_corrupt", DocVersion: 1, ClaimGeneration: 1,
				LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("4", 64),
			}}
			err := h.service.finishClaimFailure(context.Background(), claim, domainErr, false)
			if !errors.Is(err, domainErr) {
				t.Fatalf("finishClaimFailure error=%v, want %v", err, domainErr)
			}
			if got := finalizes.Load(); got != 0 {
				t.Fatalf("canonical corruption was persisted as ordinary retry/failure %d times", got)
			}
		})
	}
}

func TestRunFairClaimDoesNotNormalizeJoinedSafetyErrorAsFenceLoss(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("parent-canceled-%t", canceled), func(t *testing.T) {
			h := newPipelineHarness(t)
			h.service.workerMode = WorkerModeFair
			h.service.heartbeatInterval = time.Hour
			fingerprint := strings.Repeat("3", 64)
			fair := &pipelineFairStore{writer: fingerprint}
			h.service.fairStore = fair
			h.service.st = &modeAwareRAGStore{
				Store: h.store, mode: WorkerModeFair, fairStore: fair, fairExecution: h.store,
			}
			h.store.getDocumentForIndex = func(context.Context, store.IndexFence) (*store.RAGDocumentRecord, error) {
				return nil, errIndexFenceLost
			}
			h.store.acknowledgeFn = func(ctx context.Context, _ store.IndexFence) (bool, error) {
				if err := ctx.Err(); err != nil {
					return false, err
				}
				return false, store.ErrFairQueueUnsafeConnection
			}
			ctx, cancel := context.WithCancel(context.Background())
			if canceled {
				cancel()
			} else {
				defer cancel()
			}
			claim := &store.RAGIndexClaim{Fence: store.IndexFence{
				TaskID: 78, DocID: "doc_joined_safety", DocVersion: 1, ClaimGeneration: 1,
				LeaseOwner: "fair-worker", ExpectedWriterFingerprint: fingerprint,
			}}
			if err := h.service.RunFairClaim(ctx, claim); !errors.Is(err, store.ErrFairQueueUnsafeConnection) {
				t.Fatalf("RunFairClaim error=%v, want unsafe connection", err)
			}
		})
	}
}

func TestHeartbeatLoopReturnsSafetyErrorAndCancelsProviderContext(t *testing.T) {
	h := newPipelineHarness(t)
	h.service.heartbeatInterval = time.Millisecond
	h.service.leaseDuration = time.Second
	h.store.heartbeatFn = func(context.Context, store.IndexFence, time.Duration) (bool, error) {
		return false, store.ErrFairQueueUnsafeConnection
	}
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()
	var leaseLost atomic.Bool
	err := h.service.heartbeatLoop(
		context.Background(), store.IndexFence{TaskID: 73}, &leaseLost, cancelWork,
	)
	if !errors.Is(err, store.ErrFairQueueUnsafeConnection) {
		t.Fatalf("heartbeat error=%v, want unsafe connection", err)
	}
	if !leaseLost.Load() {
		t.Fatal("safety failure did not mark lease lost")
	}
	select {
	case <-workCtx.Done():
	default:
		t.Fatal("safety failure did not cancel provider context")
	}
}

func TestHeartbeatLoopDoesNotHideConcurrentSafetyErrorBehindCancellation(t *testing.T) {
	h := newPipelineHarness(t)
	h.service.heartbeatInterval = time.Millisecond
	h.service.leaseDuration = time.Second
	heartbeatCtx, cancelHeartbeat := context.WithCancel(context.Background())
	h.store.heartbeatFn = func(context.Context, store.IndexFence, time.Duration) (bool, error) {
		cancelHeartbeat()
		return false, store.ErrFairQueueWriterMismatch
	}
	var leaseLost atomic.Bool
	err := h.service.heartbeatLoop(heartbeatCtx, store.IndexFence{TaskID: 77}, &leaseLost, func() {})
	if !errors.Is(err, store.ErrFairQueueWriterMismatch) {
		t.Fatalf("heartbeat error=%v, want writer mismatch", err)
	}
}

func TestPipelineUsesInjectedStreamingParserAndClosesDocument(t *testing.T) {
	h := newPipelineHarness(t)
	parserFacade := &recordingPipelineParser{}
	h.service.parser = parserFacade
	doc, _ := h.seedDocument(t, "streaming_parser", "legacy parser input must not be indexed", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE"
	})
	if parserFacade.calls.Load() != 1 || parserFacade.cleanup.Load() != 1 {
		t.Fatalf("parser calls=%d cleanup=%d, want 1/1", parserFacade.calls.Load(), parserFacade.cleanup.Load())
	}
	chunks := h.vector.chunks()
	if len(chunks) == 0 || !strings.Contains(chunks[0].Content, "streaming parser output") {
		t.Fatalf("pipeline did not index Parser facade output: %+v", chunks)
	}
	for _, chunk := range chunks {
		if strings.Contains(chunk.Content, "legacy parser input") {
			t.Fatalf("pipeline called legacy parser path: %+v", chunks)
		}
	}
}

func TestPipelineStageOrderSharedBudgetAndExactSearchContent(t *testing.T) {
	h := newPipelineHarness(t)
	recorder := &pipelineStageRecorder{}
	parserFacade := &stagedPipelineParser{recorder: recorder, markdown: strings.TrimSpace(`
# Metrics

| service | p95 |
| --- | ---: |
| checkout | 42 ms |
`) + "\n"}
	enricher := &stagedPipelineEnricher{recorder: recorder}
	imageVision := &stagedPipelineImageVision{}
	tokenizer := &recordingPipelineTokenizer{}
	h.service.parser = parserFacade
	h.service.imageVision = imageVision
	h.service.enricher = enricher
	h.service.tokenizer = tokenizer
	h.service.cfg.Features.TextEnrichmentEnabled = true
	h.service.cfg.DocumentAI.TextModel = "text-v1"
	h.kb.ChunkSize = 128
	h.kb.ChunkOverlap = 16
	h.kb.EnrichmentEnabled = true
	if err := h.store.UpdateRAGKB(context.Background(), h.kb); err != nil {
		t.Fatalf("enable test enrichment: %v", err)
	}

	recordingObjects := &recordingPipelineObjects{Store: h.objects, onPut: func(key string) {
		switch {
		case strings.HasSuffix(key, "/normalized.md"):
			recorder.add("persist-normalized")
		case strings.HasSuffix(key, "/parsed.json"):
			recorder.add("persist-artifact")
		}
	}}
	h.objects = recordingObjects
	h.service.obj = recordingObjects
	h.embed.onRequest = func([]string) { recorder.add("embed") }
	h.store.putChunksFn = func(ctx context.Context, chunks []store.RAGChunkRecord) error {
		recorder.add("stage-sql")
		return h.store.DBStore.PutRAGChunks(ctx, chunks)
	}
	h.vector.onUpsert = func([]vector.ChunkData) { recorder.add("upsert-milvus") }
	h.store.activateFn = func(
		ctx context.Context,
		fence store.IndexFence,
		activation store.RAGIndexActivation,
		grace time.Duration,
	) (bool, error) {
		recorder.add("activate")
		return h.store.DBStore.ActivateAndFinishRAGIndexTask(ctx, fence, activation, grace)
	}
	var progressMu sync.Mutex
	var progressStages []string
	h.store.progressFn = func(
		ctx context.Context,
		fence store.IndexFence,
		progress store.RAGIndexProgress,
	) (bool, error) {
		progressMu.Lock()
		progressStages = append(progressStages, progress.Stage)
		progressMu.Unlock()
		return h.store.DBStore.UpdateProgressRAGIndexTask(ctx, fence, progress)
	}

	doc, _ := h.seedDocument(t, "stage_order", "source is opened through its bounded stream", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	finished := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion == 1
	})
	if finished.Degraded || finished.WarningCount != 0 {
		t.Fatalf("successful enriched pipeline marked degraded: %+v", finished)
	}

	wantOrder := []string{
		"parse", "persist-normalized", "persist-artifact", "enrich", "embed",
		"stage-sql", "upsert-milvus", "activate",
	}
	if got := recorder.snapshot(); fmt.Sprint(got) != fmt.Sprint(wantOrder) {
		t.Fatalf("pipeline stage order = %v, want %v", got, wantOrder)
	}
	progressMu.Lock()
	gotProgress := append([]string(nil), progressStages...)
	progressMu.Unlock()
	wantProgress := []string{"loading", "parsing", "chunking", "enriching", "embedding", "indexing", "finalizing"}
	if fmt.Sprint(gotProgress) != fmt.Sprint(wantProgress) {
		t.Fatalf("progress stages = %v, want %v", gotProgress, wantProgress)
	}
	if parserFacade.firstBudget() == nil || parserFacade.firstBudget() != enricher.firstBudget() {
		t.Fatalf("parser and enricher did not receive one shared task budget: parse=%p enrich=%p",
			parserFacade.firstBudget(), enricher.firstBudget())
	}
	if parserFacade.firstImageTranscriber() != imageVision {
		t.Fatalf("pipeline did not inject Office image transcriber: got=%T want=%T",
			parserFacade.firstImageTranscriber(), imageVision)
	}

	catalog, err := h.store.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	inputs := h.embed.recordedInputs()
	vectors := h.vector.chunks()
	if len(catalog) == 0 || len(catalog) != len(inputs) || len(catalog) != len(vectors) {
		t.Fatalf("stage payload counts catalog=%d inputs=%d vectors=%d", len(catalog), len(inputs), len(vectors))
	}
	enrichedCount := 0
	for i := range catalog {
		if catalog[i].SearchContent != inputs[i] || catalog[i].SearchContent != vectors[i].SearchContent {
			t.Fatalf("chunk %d SearchContent drift: sql=%q embed=%q vector=%q",
				i, catalog[i].SearchContent, inputs[i], vectors[i].SearchContent)
		}
		if catalog[i].RawContent != vectors[i].Content {
			t.Fatalf("chunk %d raw content drift: sql=%q vector=%q", i, catalog[i].RawContent, vectors[i].Content)
		}
		if !tokenizer.saw(catalog[i].SearchContent) {
			t.Fatalf("provider tokenizer did not validate final chunk %d SearchContent", i)
		}
		if catalog[i].Enhancement != "" {
			enrichedCount++
			answer := chunktext.Answer(catalog[i].RawContent, catalog[i].Enhancement)
			if !strings.Contains(catalog[i].SearchContent, answer) ||
				!strings.Contains(answer, "语义辅助（可能有误，原文优先）：") {
				t.Fatalf("chunk %d violated subordinate Answer contract: %+v", i, catalog[i])
			}
		}
	}
	if enrichedCount != 1 {
		t.Fatalf("enriched chunks = %d, want one table chunk: %+v", enrichedCount, catalog)
	}
}

func TestPipelineCacheReuseAndParseFingerprintInvalidation(t *testing.T) {
	h := newPipelineHarness(t)
	parserFacade := &recordingPipelineParser{}
	h.service.parser = parserFacade
	doc, _ := h.seedDocument(t, "cache_invalidation", "stable source body for reindex", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	first := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > 0
	})
	v1, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, first.ActiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if v1.SplitterVersion != "search-content-v2" {
		t.Fatalf("splitter/search-content contract was not bumped: %q", v1.SplitterVersion)
	}

	h.kb.ChunkSize = 32
	h.kb.ChunkOverlap = 4
	if err := h.store.UpdateRAGKB(context.Background(), h.kb); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReindexDocument(context.Background(), "u_pipeline", h.kb.ID, doc.ID); err != nil {
		t.Fatalf("queue chunk-only reindex: %v", err)
	}
	second := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > first.ActiveVersion
	})
	v2, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, second.ActiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if parserFacade.calls.Load() != 1 || v2.ParseFingerprint != v1.ParseFingerprint ||
		v2.IndexFingerprint == v1.IndexFingerprint {
		t.Fatalf("chunk-only reindex did not reuse artifact: calls=%d v1=%+v v2=%+v",
			parserFacade.calls.Load(), v1, v2)
	}

	h.service.cfg.DocumentAI.VisionPromptVersion = "vision-v4"
	if err := h.service.ReindexDocument(context.Background(), "u_pipeline", h.kb.ID, doc.ID); err != nil {
		t.Fatalf("queue parse-contract reindex: %v", err)
	}
	third := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > second.ActiveVersion
	})
	v3, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, third.ActiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if parserFacade.calls.Load() != 2 || v3.ParseFingerprint == v2.ParseFingerprint {
		t.Fatalf("parse prompt change did not force reparse: calls=%d v2=%+v v3=%+v",
			parserFacade.calls.Load(), v2, v3)
	}

	h.service.cfg.Limits.MaxVisionInputBytes++
	if err := h.service.ReindexDocument(context.Background(), "u_pipeline", h.kb.ID, doc.ID); err != nil {
		t.Fatalf("queue parse-limit reindex: %v", err)
	}
	fourth := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > third.ActiveVersion
	})
	v4, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, fourth.ActiveVersion)
	if err != nil {
		t.Fatal(err)
	}
	if parserFacade.calls.Load() != 3 || v4.ParseFingerprint == v3.ParseFingerprint {
		t.Fatalf("parse-affecting limit did not invalidate artifact: calls=%d v3=%+v v4=%+v",
			parserFacade.calls.Load(), v3, v4)
	}

	var cacheMiss, cacheHit, claimEvent, activeSwitch bool
	for _, event := range h.events.snapshot() {
		fields := event.Fields
		switch event.Name {
		case telemetry.EventResultCache:
			cacheMiss = cacheMiss || fields.CacheKind == "parse_artifact" && fields.CacheStatus == "miss"
			cacheHit = cacheHit || fields.CacheKind == "parse_artifact" && fields.CacheStatus == "hit"
		case telemetry.EventIndexTask:
			claimEvent = claimEvent || fields.Transition == "claim" && fields.Outcome == "ok" && fields.TaskID > 0
		case telemetry.EventActiveVersionSwitch:
			activeSwitch = activeSwitch || fields.Outcome == "ok" && fields.DocVersion == v2.DocVersion &&
				fields.PreviousVersion == v1.DocVersion && fields.RetiredVersion == v1.DocVersion
		}
	}
	if !cacheMiss || !cacheHit || !claimEvent || !activeSwitch {
		t.Fatalf("missing orchestration telemetry miss=%v hit=%v claim=%v switch=%v events=%+v",
			cacheMiss, cacheHit, claimEvent, activeSwitch, h.events.snapshot())
	}
}

func TestPipelineDegradedParseArtifactIsNotReused(t *testing.T) {
	h := newPipelineHarness(t)
	parserFacade := &recordingPipelineParser{
		warnings: []document.ParseWarning{{
			Code: "pdf_vision_page_failed", Message: "vision fallback", Degraded: true,
		}},
	}
	h.service.parser = parserFacade
	doc, _ := h.seedDocument(t, "degraded_cache_retry", "stable source body for retry", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)

	first := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > 0
	})
	if !first.Degraded || parserFacade.calls.Load() != 1 {
		t.Fatalf("initial degraded parse = %+v calls=%d", first, parserFacade.calls.Load())
	}
	if err := h.service.ReindexDocument(context.Background(), "u_pipeline", h.kb.ID, doc.ID); err != nil {
		t.Fatalf("queue degraded reindex: %v", err)
	}
	second := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > first.ActiveVersion
	})
	if !second.Degraded || parserFacade.calls.Load() != 2 {
		t.Fatalf("degraded artifact was reused: result=%+v calls=%d", second, parserFacade.calls.Load())
	}
}

func TestIndexFingerprintIncludesEnrichmentSchemaVersion(t *testing.T) {
	input := indexFingerprintInput{
		ParseFingerprint: strings.Repeat("a", 64), ChunkSize: 512, ChunkOverlap: 64,
		SplitterSchemaVersion: splitterSchemaVersion, EmbeddingModel: "embed-v1", EmbeddingDimensions: 4,
		EmbeddingContract: strings.Repeat("b", 64), EnrichmentEnabled: true,
		TextProviderFingerprint: strings.Repeat("c", 64), TextModel: "text-v1",
		EnrichmentPromptVersion: "prompt-v1", EnrichmentSchemaVersion: enrich.EnrichmentSchemaVersion,
	}
	base := buildIndexFingerprint(input)
	input.EnrichmentSchemaVersion = "text-enrichment-v2"
	if changed := buildIndexFingerprint(input); changed == base {
		t.Fatalf("enrichment schema version did not change index fingerprint: %q", base)
	}
}

func TestOfficeParseFingerprintContractIsFormatScoped(t *testing.T) {
	officeParser, markItDown := parseContractVersions(".DOCX")
	if officeParser != parse.OfficeParserVersion || markItDown != parse.OfficeMarkItDownVersion ||
		!strings.Contains(officeParser, parse.OfficeWrapperVersion) {
		t.Fatalf("Office parse contract=%q/%q", officeParser, markItDown)
	}
	for _, format := range []string{"md", "txt", "pdf"} {
		parserVersion, converterVersion := parseContractVersions(format)
		if parserVersion != parse.LocalParserVersion || converterVersion != "none" {
			t.Fatalf("non-Office %s contract=%q/%q", format, parserVersion, converterVersion)
		}
	}
	input := document.ParseFingerprintInput{
		SourceSHA256: strings.Repeat("a", 64), ParseMode: string(config.ParseModeStandard),
		ParserVersion: officeParser, MarkItDownVersion: markItDown,
		PDFRenderDPI: 180, PDFRoutingVersion: parse.PDFAutoRoutingVersion,
		MaxPages: 300, MaxVisionPages: 100, MaxVisionAssets: 100, MaxAssets: 500,
		MaxAssetBytes: 20 << 20, MaxExtractedBytes: 200 << 20,
		MaxVisionInputBytes: 8 << 20, MaxImagePixels: 40_000_000,
		DisplayMaxEdge: 2400, ThumbnailMaxEdge: 480,
		PageSchemaVersion: vision.PageSchemaVersion, ImageSchemaVersion: vision.ImageDescriptionSchemaVersion,
	}
	base, err := document.ParseFingerprint(input)
	if err != nil {
		t.Fatal(err)
	}
	input.MarkItDownVersion = "0.1.7"
	changed, err := document.ParseFingerprint(input)
	if err != nil {
		t.Fatal(err)
	}
	if changed == base {
		t.Fatal("MarkItDown contract change did not invalidate Office parse fingerprint")
	}
}

func TestPipelineProgressMarksUnavailableEnrichmentDegraded(t *testing.T) {
	h := newPipelineHarness(t)
	h.service.parser = &stagedPipelineParser{markdown: strings.TrimSpace(`
| item | value |
| --- | ---: |
| source | 7 |
`) + "\n"}
	h.service.cfg.Features.TextEnrichmentEnabled = true
	h.service.cfg.DocumentAI.TextModel = "text-v1"
	h.kb.ChunkSize = 128
	h.kb.ChunkOverlap = 16
	h.kb.EnrichmentEnabled = true
	if err := h.store.UpdateRAGKB(context.Background(), h.kb); err != nil {
		t.Fatal(err)
	}
	doc, _ := h.seedDocument(t, "enrichment_degraded", "source remains authoritative", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	finished := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion == 1
	})
	if !finished.Degraded || finished.WarningCount != 1 || finished.ProcessingStage != "done" {
		t.Fatalf("degraded progress/result = %+v, want done with one warning", finished)
	}
	chunks, err := h.store.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("degraded chunks=%+v err=%v", chunks, err)
	}
	if chunks[0].Enhancement != "" || !strings.Contains(chunks[0].RawContent, "source") ||
		chunks[0].SearchContent != chunks[0].RawContent {
		t.Fatalf("degraded enrichment did not retain exact source text: %+v", chunks[0])
	}
}

func TestPipelineFailureAfterSQLStageDoesNotReachMilvusOrActivate(t *testing.T) {
	h := newPipelineHarness(t)
	sqlStaged := make(chan struct{})
	fenceChecked := make(chan struct{})
	var staged atomic.Bool
	var stageOnce sync.Once
	var checkOnce sync.Once
	h.store.putChunksFn = func(ctx context.Context, chunks []store.RAGChunkRecord) error {
		if err := h.store.DBStore.PutRAGChunks(ctx, chunks); err != nil {
			return err
		}
		staged.Store(true)
		stageOnce.Do(func() { close(sqlStaged) })
		return nil
	}
	h.store.checkFenceFn = func(ctx context.Context, fence store.IndexFence) (bool, error) {
		if staged.Load() {
			checkOnce.Do(func() { close(fenceChecked) })
			return false, nil
		}
		return h.store.DBStore.CheckRAGIndexFence(ctx, fence)
	}
	doc, _ := h.seedDocument(t, "stale_before_milvus", "fenced SQL staging", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	select {
	case <-sqlStaged:
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline did not stage SQL chunks")
	}
	select {
	case <-fenceChecked:
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline did not check the fence before Milvus")
	}
	if got := h.vector.chunks(); len(got) != 0 {
		t.Fatalf("stale worker wrote vectors after losing fence: %+v", got)
	}
	stagedChunks, err := h.store.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil || len(stagedChunks) == 0 {
		t.Fatalf("SQL staging boundary not reached: chunks=%+v err=%v", stagedChunks, err)
	}
	current, err := h.store.GetRAGDocument(context.Background(), doc.ID)
	if err != nil || current.ActiveVersion != 0 {
		t.Fatalf("stale worker activated staged version: doc=%+v err=%v", current, err)
	}
}

func TestPipelineFailureAfterMilvusWritePreservesOldActiveVersion(t *testing.T) {
	h := newPipelineHarness(t)
	doc, _ := h.seedDocument(t, "activation_failure", "active version one", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	initial := waitPipelineDocument(t, h.store, doc.ID, func(record *store.RAGDocumentRecord) bool {
		return record.Status == "DONE" && record.ActiveVersion > 0
	})

	activationAttempted := make(chan struct{})
	var once sync.Once
	h.store.activateFn = func(
		context.Context,
		store.IndexFence,
		store.RAGIndexActivation,
		time.Duration,
	) (bool, error) {
		once.Do(func() { close(activationAttempted) })
		return false, errors.New("injected activation transaction failure")
	}
	if err := h.service.ReindexDocument(context.Background(), "u_pipeline", h.kb.ID, doc.ID); err != nil {
		t.Fatalf("queue activation-failure reindex: %v", err)
	}
	select {
	case <-activationAttempted:
	case <-time.After(3 * time.Second):
		t.Fatal("pipeline did not reach activation after Milvus write")
	}
	current, err := h.store.GetRAGDocument(context.Background(), doc.ID)
	if err != nil || current.ActiveVersion != initial.ActiveVersion || current.Version <= initial.Version {
		t.Fatalf("activation failure replaced old active version: doc=%+v err=%v", current, err)
	}
	failedVersion := current.Version
	stagedChunks, err := h.store.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, failedVersion)
	if err != nil || len(stagedChunks) == 0 {
		t.Fatalf("new SQL version was not staged before activation failure: chunks=%+v err=%v", stagedChunks, err)
	}
	wroteFailedVersion := false
	for _, chunk := range h.vector.chunks() {
		wroteFailedVersion = wroteFailedVersion || chunk.DocVersion == failedVersion
	}
	if !wroteFailedVersion {
		t.Fatal("test did not reach the Milvus-write boundary")
	}
}

func (h *pipelineHarness) seedDocument(t *testing.T, suffix, body string, version int64) (*store.RAGDocumentRecord, int64) {
	t.Helper()
	docID := "doc_" + suffix
	key := "rag/u_pipeline/" + h.kb.ID + "/" + docID + "/source.md"
	if err := h.objects.Put(context.Background(), key, strings.NewReader(body), int64(len(body)), "text/markdown"); err != nil {
		t.Fatalf("put source: %v", err)
	}
	sum := sha256.Sum256([]byte(body))
	doc := &store.RAGDocumentRecord{
		ID: docID, KBID: h.kb.ID, FileName: "source.md", FileType: "md", FileSize: int64(len(body)),
		ObjectKey: key, Status: "PENDING", Version: version, SourceSHA256: hex.EncodeToString(sum[:]),
		IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: time.Now().UTC(),
	}
	snapshot, err := h.service.BuildVersionSnapshot(context.Background(), doc)
	if err != nil {
		t.Fatalf("build snapshot: %v", err)
	}
	if snapshot.ParserVersion != parse.LocalParserVersion {
		t.Fatalf("snapshot parser version=%q, want %q", snapshot.ParserVersion, parse.LocalParserVersion)
	}
	snapshot.DocVersion = version
	taskID, err := h.store.CreateRAGDocumentWithVersionAndIndexTask(context.Background(), doc, snapshot, 3)
	if err != nil {
		t.Fatalf("create durable document task: %v", err)
	}
	return doc, taskID
}

func waitPipelineDocument(t *testing.T, st store.Store, docID string, predicate func(*store.RAGDocumentRecord) bool) *store.RAGDocumentRecord {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			doc, _ := st.GetRAGDocument(context.Background(), docID)
			t.Fatalf("pipeline document did not reach expected state: %+v", doc)
		case <-ticker.C:
			doc, err := st.GetRAGDocument(context.Background(), docID)
			if err == nil && predicate(doc) {
				return doc
			}
		}
	}
}

func TestPipelinePollerRecoversDurableTaskAfterFullWakeChannel(t *testing.T) {
	h := newPipelineHarness(t)
	h.service.tasks = make(chan int64, 1)
	firstClaimEntered := make(chan struct{})
	releaseFirstClaim := make(chan struct{})
	var first atomic.Bool
	h.store.claimFn = func(ctx context.Context, worker string, lease time.Duration) (*store.RAGIndexClaim, error) {
		if first.CompareAndSwap(false, true) {
			close(firstClaimEntered)
			select {
			case <-releaseFirstClaim:
				return nil, nil // the initial sweep observed no runnable task
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return h.store.DBStore.ClaimRAGIndexTask(ctx, worker, lease)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	select {
	case <-firstClaimEntered:
	case <-time.After(time.Second):
		t.Fatal("initial durable claim did not start")
	}
	one, oneTask := h.seedDocument(t, "poller_one", "first durable task", 1)
	two, twoTask := h.seedDocument(t, "poller_two", "second durable task", 1)
	h.service.scheduleTask(oneTask)
	h.service.scheduleTask(twoTask) // full wake channel: this hint is deliberately dropped
	close(releaseFirstClaim)

	wantDone := func(doc *store.RAGDocumentRecord) bool { return doc.Status == "DONE" && doc.ActiveVersion == 1 }
	waitPipelineDocument(t, h.store, one.ID, wantDone)
	waitPipelineDocument(t, h.store, two.ID, wantDone)
}

func TestPipelineLeaseHeartbeatFailureCancelsWorkImmediately(t *testing.T) {
	h := newPipelineHarness(t)
	h.embed.mode.Store(pipelineEmbedBlock)
	doc, _ := h.seedDocument(t, "lease_cancel", "content that reaches embedding", 1)
	h.store.heartbeatFn = func(ctx context.Context, fence store.IndexFence, lease time.Duration) (bool, error) {
		select {
		case <-h.embed.entered:
			return false, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	select {
	case <-h.embed.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("lost heartbeat fence did not cancel the in-flight embedding request")
	}
	current, err := h.store.GetRAGDocument(context.Background(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActiveVersion != 0 || current.Status == "DONE" {
		t.Fatalf("lease-lost worker activated document: %+v", current)
	}
	for _, event := range h.events.snapshot() {
		if event.Name == telemetry.EventIndexTask && event.Fields.Transition == "heartbeat" &&
			event.Fields.Outcome == "error" && event.Fields.ErrorCode == "fence_lost" {
			return
		}
	}
	t.Fatalf("lease loss was not observable through the closed event schema: %+v", h.events.snapshot())
}

func TestPipelineTelemetryRecordsRetryWithoutProviderDetails(t *testing.T) {
	h := newPipelineHarness(t)
	_, _ = h.seedDocument(t, "telemetry_retry", "retryable source", 1)
	claim, err := h.store.ClaimRAGIndexTask(context.Background(), h.service.workerID, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	const canary = "CANARY-provider-endpoint-and-secret"
	h.service.finishClaimFailure(context.Background(), claim, errors.New(canary), false)
	for _, event := range h.events.snapshot() {
		if fmt.Sprint(event) != "" && strings.Contains(fmt.Sprint(event), canary) {
			t.Fatalf("telemetry leaked provider detail: %+v", event)
		}
		if event.Name == telemetry.EventIndexTask && event.Fields.Transition == "retry" &&
			event.Fields.Outcome == "scheduled" && event.Fields.RetryCount == 1 {
			return
		}
	}
	t.Fatalf("retry transition was not observable: %+v", h.events.snapshot())
}

func TestPipelineVersionClaimUsesImmutableSnapshotAndInt64Fence(t *testing.T) {
	h := newPipelineHarness(t)
	const physicalVersion int64 = 1 << 40
	body := strings.Repeat("alpha beta gamma delta epsilon ", 30)
	doc, _ := h.seedDocument(t, "int64_snapshot", body, physicalVersion)

	// A queued version owns its chunking contract. Later KB edits may make the
	// UI report needsReindex, but must not mutate this claim's physical epoch.
	h.kb.ChunkSize = 1000
	h.kb.ChunkOverlap = 10
	if err := h.store.UpdateRAGKB(context.Background(), h.kb); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	current := waitPipelineDocument(t, h.store, doc.ID, func(doc *store.RAGDocumentRecord) bool {
		return doc.Status == "DONE"
	})
	if current.ActiveVersion != physicalVersion {
		t.Fatalf("active version = %d, want %d", current.ActiveVersion, physicalVersion)
	}
	chunks := h.vector.chunks()
	if len(chunks) < 2 {
		t.Fatalf("worker used mutable KB chunk size instead of claimed snapshot: %d chunks", len(chunks))
	}
	for _, chunk := range chunks {
		if chunk.DocVersion != physicalVersion {
			t.Fatalf("vector doc_version = %d, want int64 %d", chunk.DocVersion, physicalVersion)
		}
	}
}

func TestPipelineActiveVersionSurvivesPermanentTargetFailure(t *testing.T) {
	h := newPipelineHarness(t)
	doc, _ := h.seedDocument(t, "active_failure", "version one content", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	waitPipelineDocument(t, h.store, doc.ID, func(doc *store.RAGDocumentRecord) bool {
		return doc.Status == "DONE" && doc.ActiveVersion == 1
	})

	h.vector.setUpsertError(errors.New("validation: vector store rejected staged chunks"))
	if err := h.service.ReindexDocument(context.Background(), "u_pipeline", h.kb.ID, doc.ID); err != nil {
		t.Fatalf("queue reindex: %v", err)
	}
	target, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, 2)
	if err != nil || target.EmbeddingDimensions != 4 {
		t.Fatalf("invalid failure-test setup: version=%+v err=%v", target, err)
	}
	failed := waitPipelineDocument(t, h.store, doc.ID, func(doc *store.RAGDocumentRecord) bool {
		return doc.Status == "FAILED" && doc.Version == 2
	})
	if failed.ActiveVersion != 1 {
		t.Fatalf("failed target replaced old active version: %+v", failed)
	}
	version, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, 2)
	if err != nil || version.Status != store.RAGDocumentVersionFailed {
		t.Fatalf("failed target snapshot = %+v, %v", version, err)
	}
	staged, err := h.store.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 2)
	if err != nil || len(staged) == 0 {
		t.Fatalf("Milvus failure did not occur after SQL staging: chunks=%+v err=%v", staged, err)
	}
}

func TestPipelineActivationUsesSingleAtomicStoreTransition(t *testing.T) {
	h := newPipelineHarness(t)
	doc, _ := h.seedDocument(t, "atomic_activate", "atomic activation content", 1)
	beforeActivate := make(chan store.IndexFence, 1)
	releaseActivate := make(chan struct{})
	h.store.activateFn = func(
		ctx context.Context,
		fence store.IndexFence,
		activation store.RAGIndexActivation,
		grace time.Duration,
	) (bool, error) {
		beforeActivate <- fence
		select {
		case <-releaseActivate:
			return h.store.DBStore.ActivateAndFinishRAGIndexTask(ctx, fence, activation, grace)
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	var fence store.IndexFence
	select {
	case fence = <-beforeActivate:
	case <-time.After(2 * time.Second):
		t.Fatal("pipeline never reached activation")
	}
	before, err := h.store.GetRAGDocument(context.Background(), doc.ID)
	if err != nil || before.ActiveVersion != 0 {
		t.Fatalf("document activated before atomic store call: %+v, %v", before, err)
	}
	runningVersion, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, fence.DocVersion)
	if err != nil || runningVersion.Status != store.RAGDocumentVersionRunning {
		t.Fatalf("version was partially finished before activation: %+v, %v", runningVersion, err)
	}
	close(releaseActivate)
	finished := waitPipelineDocument(t, h.store, doc.ID, func(doc *store.RAGDocumentRecord) bool {
		return doc.ActiveVersion == fence.DocVersion && doc.Status == "DONE"
	})
	if finished.Version != fence.DocVersion {
		t.Fatalf("document target/active diverged after activation: %+v", finished)
	}
	task, err := h.store.GetRAGIndexTask(context.Background(), fence.TaskID)
	if err != nil || task.Status != "DONE" {
		t.Fatalf("task did not finish atomically: %+v, %v", task, err)
	}
}

func TestPipelineVersionProviderFingerprintMismatchSupersedesBeforeEndpoint(t *testing.T) {
	h := newPipelineHarness(t)
	doc, taskID := h.seedDocument(t, "provider_guard", "provider guard content", 1)
	version, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	version.EmbeddingContractFingerprint = strings.Repeat("0", 64)
	// Input snapshot fields are immutable, so the adversarial stale fingerprint
	// is seeded with SQL exactly as a pre-fix/legacy row would be encountered.
	if _, err := h.store.DB().ExecContext(context.Background(),
		`UPDATE rag_document_versions SET embedding_contract_fingerprint=? WHERE doc_id=? AND doc_version=?`,
		version.EmbeddingContractFingerprint, doc.ID, int64(1)); err != nil {
		t.Fatal(err)
	}

	superseded := make(chan *store.RAGIndexTaskRecord, 1)
	h.store.supersedeFn = func(
		ctx context.Context,
		fence store.IndexFence,
		snapshot *store.RAGDocumentVersionRecord,
	) (*store.RAGIndexTaskRecord, bool, error) {
		created, ok, err := h.store.DBStore.SupersedeRAGIndexTaskAndCreateVersion(ctx, fence, snapshot)
		if err == nil && ok {
			superseded <- created
		}
		return created, ok, err
	}
	var claimCount atomic.Int64
	secondClaimBlocked := make(chan struct{})
	h.store.claimFn = func(ctx context.Context, worker string, lease time.Duration) (*store.RAGIndexClaim, error) {
		if claimCount.Add(1) == 1 {
			return h.store.DBStore.ClaimRAGIndexTask(ctx, worker, lease)
		}
		close(secondClaimBlocked)
		<-ctx.Done()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.service.Start(ctx)
	var created *store.RAGIndexTaskRecord
	select {
	case created = <-superseded:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("provider mismatch was not superseded")
	}
	if created == nil || created.DocVersion != 2 || created.ID == taskID {
		cancel()
		t.Fatalf("replacement task = %+v", created)
	}
	if h.embed.calls.Load() != 0 {
		cancel()
		t.Fatalf("stale provider snapshot made %d outbound embedding calls", h.embed.calls.Load())
	}
	oldVersion, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil || oldVersion.Status != store.RAGDocumentVersionSuperseded {
		cancel()
		t.Fatalf("old provider version = %+v, %v", oldVersion, err)
	}
	newVersion, err := h.store.GetRAGDocumentVersion(context.Background(), doc.ID, 2)
	if err != nil || newVersion.EmbeddingContractFingerprint == version.EmbeddingContractFingerprint {
		cancel()
		t.Fatalf("replacement snapshot = %+v, %v", newVersion, err)
	}
	cancel()
	select {
	case <-secondClaimBlocked:
	case <-time.After(time.Second):
	}
}

func TestPipelineProviderSupersedeRejectsMissingReplacement(t *testing.T) {
	h := newPipelineHarness(t)
	doc, _ := h.seedDocument(t, "provider_missing_replacement", "provider guard content", 1)
	if _, err := h.store.DB().ExecContext(context.Background(),
		`UPDATE rag_document_versions SET embedding_contract_fingerprint=? WHERE doc_id=? AND doc_version=?`,
		strings.Repeat("0", 64), doc.ID, int64(1)); err != nil {
		t.Fatal(err)
	}
	h.store.supersedeFn = func(
		context.Context,
		store.IndexFence,
		*store.RAGDocumentVersionRecord,
	) (*store.RAGIndexTaskRecord, bool, error) {
		return nil, true, nil
	}
	claim, err := h.store.ClaimRAGIndexTask(context.Background(), "contract-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if err := h.service.runClaim(context.Background(), claim); !errors.Is(err, store.ErrRAGDocumentVersionMismatch) {
		t.Fatalf("runClaim error=%v, want authoritative replacement mismatch", err)
	}
	if h.embed.calls.Load() != 0 {
		t.Fatalf("missing replacement made %d outbound embedding calls", h.embed.calls.Load())
	}
}

func TestPipelineProviderGuardUsesSameResolvedBindingForOutboundRequest(t *testing.T) {
	h := newPipelineHarness(t)
	h.kb.EmbedProvider = "user"
	// UpdateRAGKB deliberately keeps the embedding contract immutable; this
	// fixture models a KB that was created with a user binding directly.
	if _, err := h.store.DB().ExecContext(context.Background(),
		`UPDATE rag_kbs SET embed_provider='user' WHERE id=?`, h.kb.ID); err != nil {
		t.Fatal(err)
	}

	var wrongEndpointCalls atomic.Int64
	wrongEndpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrongEndpointCalls.Add(1)
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response := struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}{}
		for index := range request.Input {
			response.Data = append(response.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: make([]float32, 4), Index: index})
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer wrongEndpoint.Close()

	var resolutions atomic.Int64
	h.service.userCfg = func(context.Context, string) (config.RAGEmbeddingCfg, bool) {
		if resolutions.Add(1) <= 2 {
			return config.RAGEmbeddingCfg{
				Endpoint: h.embed.server.URL, Model: "embed-v1", Dims: 4,
			}, true
		}
		return config.RAGEmbeddingCfg{
			Endpoint: wrongEndpoint.URL, Model: "embed-v1", Dims: 4,
		}, true
	}
	doc, _ := h.seedDocument(t, "binding_toctou", "same binding must reach the endpoint", 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.service.Start(ctx)
	waitPipelineDocument(t, h.store, doc.ID, func(doc *store.RAGDocumentRecord) bool {
		return doc.Status == "DONE"
	})
	if got := resolutions.Load(); got != 2 {
		t.Fatalf("embedding binding resolved %d times, want snapshot + one claim resolution", got)
	}
	if got := wrongEndpointCalls.Load(); got != 0 {
		t.Fatalf("provider changed after guard and received %d outbound calls", got)
	}
	if got := h.embed.calls.Load(); got == 0 {
		t.Fatal("guarded embedding binding did not receive the outbound call")
	}
}

func TestPipelineVersionProviderContractIncludesModelsAndPrompts(t *testing.T) {
	base := store.RAGDocumentVersionRecord{
		ParseMode:                    store.RAGParseModeAuto,
		ParseFingerprint:             strings.Repeat("d", 64),
		SplitterVersion:              splitterSchemaVersion,
		EnrichmentEnabled:            true,
		VisionProviderFingerprint:    strings.Repeat("a", 64),
		VisionModel:                  "vision-v1",
		VisionPromptVersion:          "vision-prompt-v1",
		TextProviderFingerprint:      strings.Repeat("b", 64),
		TextModel:                    "text-v1",
		EnrichmentPromptVersion:      "enrich-schema-v1",
		EmbeddingProvider:            "system",
		EmbeddingModel:               "embed-v1",
		EmbeddingDimensions:          4,
		EmbeddingContractFingerprint: strings.Repeat("c", 64),
	}
	if !sameRuntimeProviderContracts(&base, &base) {
		t.Fatal("identical provider contracts did not match")
	}
	mutations := []struct {
		name   string
		mutate func(*store.RAGDocumentVersionRecord)
	}{
		{"parser version", func(v *store.RAGDocumentVersionRecord) { v.ParserVersion = "local-parser-v2" }},
		{"parse fingerprint", func(v *store.RAGDocumentVersionRecord) { v.ParseFingerprint = strings.Repeat("e", 64) }},
		{"splitter schema", func(v *store.RAGDocumentVersionRecord) { v.SplitterVersion = "search-content-v3" }},
		{"vision model", func(v *store.RAGDocumentVersionRecord) { v.VisionModel = "vision-v2" }},
		{"vision prompt", func(v *store.RAGDocumentVersionRecord) { v.VisionPromptVersion = "vision-prompt-v2" }},
		{"text model", func(v *store.RAGDocumentVersionRecord) { v.TextModel = "text-v2" }},
		{"text schema", func(v *store.RAGDocumentVersionRecord) { v.EnrichmentPromptVersion = "enrich-schema-v2" }},
		{"embedding model", func(v *store.RAGDocumentVersionRecord) { v.EmbeddingModel = "embed-v2" }},
		{"embedding dimensions", func(v *store.RAGDocumentVersionRecord) { v.EmbeddingDimensions = 8 }},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			test.mutate(&changed)
			if sameRuntimeProviderContracts(&base, &changed) {
				t.Fatalf("provider contract change %q was accepted", test.name)
			}
		})
	}
}

func TestPipelineRetryClassifierAndBackoff(t *testing.T) {
	permanent := []error{
		parse.ErrEmptyContent,
		fmt.Errorf("invalid PDF: %w", parse.ErrInvalidDocument),
		fmt.Errorf("too many pages: %w", parse.ErrDocumentLimitExceeded),
		fmt.Errorf("source changed: %w", parse.ErrSourceIntegrity),
		fmt.Errorf("sidecar unavailable: %w", sidecar.ErrCapabilityUnavailable),
		fmt.Errorf("sidecar schema: %w", sidecar.ErrInvalidBundle),
		fmt.Errorf("sidecar bundle quota: %w", sidecar.ErrBundleLimitExceeded),
		fmt.Errorf("sidecar source quota: %w", sidecar.ErrSourceLimitExceeded),
		fmt.Errorf("sidecar source changed: %w", sidecar.ErrSourceIntegrity),
		fmt.Errorf("reserve DocumentAI usage: %w", store.ErrRAGDocumentAIBudgetExceeded),
		errors.New("embedding response 维度不符"),
		errors.New("unsupported document container"),
		errors.New("embeddings 端点返回 400: validation failed"),
	}
	for _, err := range permanent {
		if isTransientIndexError(err) {
			t.Errorf("classified permanent error as transient: %v", err)
		}
	}
	transient := []error{
		&url.Error{Op: "POST", URL: "https://embedding.invalid", Err: &net.DNSError{IsTimeout: true}},
		errors.New("embeddings 端点返回 429: rate limited"),
		errors.New("embeddings 端点返回 503: unavailable"),
	}
	for _, err := range transient {
		if !isTransientIndexError(err) {
			t.Errorf("classified transient error as permanent: %v", err)
		}
	}
	httpCases := []struct {
		status        int
		wantTransient bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusForbidden, false},
		{http.StatusNotFound, false},
		{http.StatusUnprocessableEntity, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooEarly, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
	}
	for _, test := range httpCases {
		t.Run(fmt.Sprintf("embedding HTTP %d", test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, http.StatusText(test.status), test.status)
			}))
			defer server.Close()
			_, err := embed.New(server.URL, "", "model", 1).Embed(
				context.Background(), []string{"input"},
			)
			if err == nil {
				t.Fatalf("embedding HTTP %d unexpectedly succeeded", test.status)
			}
			if got := isTransientIndexError(err); got != test.wantTransient {
				t.Fatalf("embedding HTTP %d transient = %v, want %v: %v",
					test.status, got, test.wantTransient, err)
			}
		})
	}
	wants := map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		8: 128 * time.Second,
		9: 128 * time.Second,
	}
	for retry, want := range wants {
		if got := indexRetryDelay(retry); got != want {
			t.Errorf("retry %d delay = %s, want %s", retry, got, want)
		}
	}
}

func TestSafeIndexErrorMessageDoesNotExposeUntrustedDetails(t *testing.T) {
	t.Parallel()
	const canary = "CANARY-document-body-and-temp-path"
	for _, test := range []struct {
		err       error
		transient bool
	}{
		{fmt.Errorf("%w: %s", parse.ErrInvalidDocument, canary), false},
		{fmt.Errorf("%w: entry %q", sidecar.ErrInvalidBundle, canary), false},
		{fmt.Errorf("temporary path %s unavailable", canary), true},
	} {
		message := safeIndexErrorMessage(test.err, test.transient)
		if strings.Contains(message, canary) {
			t.Fatalf("safe error message leaked canary: %q", message)
		}
		if strings.TrimSpace(message) == "" {
			t.Fatal("safe error message is empty")
		}
	}
}
