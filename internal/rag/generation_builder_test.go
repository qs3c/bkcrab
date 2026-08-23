package rag

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeEvaluationGenerationStore struct {
	mu            sync.Mutex
	byFingerprint map[string]*store.RAGEvalGenerationRecord
	refs          map[string]string
	versions      map[string]*store.RAGEvalDatasetVersionRecord
	documents     map[string][]store.RAGEvalCorpusDocumentRecord
	gcFence       *store.RAGEvalGenerationFence
}

func newFakeEvaluationGenerationStore() *fakeEvaluationGenerationStore {
	return &fakeEvaluationGenerationStore{byFingerprint: map[string]*store.RAGEvalGenerationRecord{}, refs: map[string]string{},
		versions: map[string]*store.RAGEvalDatasetVersionRecord{}, documents: map[string][]store.RAGEvalCorpusDocumentRecord{}}
}

func (s *fakeEvaluationGenerationStore) AcquireRAGEvalGenerationForRun(_ context.Context, request store.RAGEvalGenerationAcquireRequest) (*store.RAGEvalGenerationAcquireResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if request.PreferredGenerationID != "" {
		for _, generation := range s.byFingerprint {
			if generation.ID == request.PreferredGenerationID && generation.Status == store.RAGEvalGenerationReady {
				s.refs[request.RunID] = generation.ID
				generation.RefCount++
				return &store.RAGEvalGenerationAcquireResult{Generation: generation, Reused: true}, nil
			}
		}
	}
	if generation := s.byFingerprint[request.Fingerprint]; generation != nil {
		s.refs[request.RunID] = generation.ID
		generation.RefCount++
		if generation.Status == store.RAGEvalGenerationReady {
			return &store.RAGEvalGenerationAcquireResult{Generation: generation, Reused: true}, nil
		}
		return &store.RAGEvalGenerationAcquireResult{Generation: generation}, nil
	}
	generation := &store.RAGEvalGenerationRecord{
		ID: request.NewGenerationID, DatasetVersionID: request.DatasetVersionID, Fingerprint: request.Fingerprint,
		CorpusFingerprint: request.CorpusFingerprint, IngestionFingerprint: request.IngestionFingerprint,
		CollectionKey: request.CollectionKey, ObjectPrefix: request.ObjectPrefix, EmbeddingModel: request.EmbeddingModel,
		EmbeddingDims: int64(request.EmbeddingDims), Status: store.RAGEvalGenerationBuilding,
		LeaseOwner: request.Worker, FenceToken: 1, RefCount: 1,
	}
	s.byFingerprint[request.Fingerprint], s.refs[request.RunID] = generation, generation.ID
	return &store.RAGEvalGenerationAcquireResult{Generation: generation, Claimed: true,
		Fence: &store.RAGEvalGenerationFence{GenerationID: generation.ID, LeaseOwner: request.Worker,
			CollectionKey: request.CollectionKey, ObjectPrefix: request.ObjectPrefix, FenceToken: 1}}, nil
}
func (s *fakeEvaluationGenerationStore) GetRAGEvalDatasetVersion(_ context.Context, id string) (*store.RAGEvalDatasetVersionRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.versions[id]
	if item == nil {
		return nil, store.ErrNotFound
	}
	copy := *item
	return &copy, nil
}

// Mirrors the real keyset query: WHERE dataset_version_id=? AND id>? ORDER BY id LIMIT ?
func (s *fakeEvaluationGenerationStore) ListRAGEvalCorpusDocuments(_ context.Context, datasetVersionID, cursor string, limit int) ([]store.RAGEvalCorpusDocumentRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	page := []store.RAGEvalCorpusDocumentRecord{}
	for _, document := range s.documents[datasetVersionID] {
		if document.ID > cursor {
			page = append(page, document)
		}
	}
	sort.Slice(page, func(i, j int) bool { return page[i].ID < page[j].ID })
	if limit > 0 && len(page) > limit {
		page = page[:limit]
	}
	return page, nil
}
func (s *fakeEvaluationGenerationStore) ListReusableRAGEvalGenerations(_ context.Context, ingestionFingerprint, embeddingModel string, embeddingDims, _ int) ([]store.RAGEvalGenerationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []store.RAGEvalGenerationRecord{}
	for _, generation := range s.byFingerprint {
		if generation.Status == store.RAGEvalGenerationReady && generation.IngestionFingerprint == ingestionFingerprint &&
			generation.EmbeddingModel == embeddingModel && generation.EmbeddingDims == int64(embeddingDims) {
			out = append(out, *generation)
		}
	}
	return out, nil
}
func (s *fakeEvaluationGenerationStore) seed(request EvaluationGenerationBuildRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version := *request.DatasetVersion
	s.versions[version.ID] = &version
	s.documents[version.ID] = append([]store.RAGEvalCorpusDocumentRecord(nil), request.Documents...)
}
func (s *fakeEvaluationGenerationStore) HeartbeatRAGEvalGeneration(context.Context, store.RAGEvalGenerationFence, time.Duration) (bool, error) {
	return true, nil
}
func (s *fakeEvaluationGenerationStore) MarkRAGEvalGenerationReady(_ context.Context, fence store.RAGEvalGenerationFence, docs, chunks int64, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, generation := range s.byFingerprint {
		if generation.ID == fence.GenerationID && generation.Status == store.RAGEvalGenerationBuilding {
			generation.Status, generation.DocumentCount, generation.ChunkCount = store.RAGEvalGenerationReady, docs, chunks
			return true, nil
		}
	}
	return false, nil
}
func (s *fakeEvaluationGenerationStore) MarkRAGEvalGenerationFailed(_ context.Context, fence store.RAGEvalGenerationFence, _, _ string, _ time.Duration) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, generation := range s.byFingerprint {
		if generation.ID == fence.GenerationID && generation.Status == store.RAGEvalGenerationBuilding {
			generation.Status = store.RAGEvalGenerationFailed
			return true, nil
		}
	}
	return false, nil
}
func (s *fakeEvaluationGenerationStore) ReleaseRAGEvalGenerationForRun(_ context.Context, runID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	generationID, ok := s.refs[runID]
	if !ok {
		return false, nil
	}
	delete(s.refs, runID)
	for _, generation := range s.byFingerprint {
		if generation.ID == generationID {
			generation.RefCount--
		}
	}
	return true, nil
}
func (s *fakeEvaluationGenerationStore) ClaimRAGEvalGenerationGC(context.Context, time.Time, string, time.Duration) (*store.RAGEvalGenerationFence, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gcFence == nil {
		return nil, false, nil
	}
	fence := *s.gcFence
	s.gcFence = nil
	return &fence, true, nil
}
func (s *fakeEvaluationGenerationStore) FinishRAGEvalGenerationGC(context.Context, store.RAGEvalGenerationFence) (bool, error) {
	return true, nil
}

type fakeEvaluationPipeline struct {
	mu         sync.Mutex
	buildCalls int
	dropCalls  int
	fail       bool
	targets    []PipelineTarget
}

func (p *fakeEvaluationPipeline) BuildEvaluationGeneration(_ context.Context, request EvaluationPipelineRequest) (EvaluationPipelineResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buildCalls++
	p.targets = append(p.targets, request.Target)
	if p.fail {
		return EvaluationPipelineResult{}, errors.New("injected pipeline failure")
	}
	return EvaluationPipelineResult{DocumentCount: int64(len(request.Documents)), ChunkCount: 4}, nil
}
func (p *fakeEvaluationPipeline) DropEvaluationGeneration(_ context.Context, target PipelineTarget) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropCalls++
	p.targets = append(p.targets, target)
	return nil
}

func evaluationGenerationFixture(runID string) EvaluationGenerationBuildRequest {
	const sha = "f7196c7739a42cc9e9e6be4ba11d3752fb1d26c3b91cd73b6eb18bb9b38c493d"
	policy := config.RAGIngestionPolicyData{
		Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard,
		Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "contract", Model: "embed", Dims: 3},
	}
	return EvaluationGenerationBuildRequest{
		OwnerID: "admin", RunID: runID,
		DatasetVersion:               &store.RAGEvalDatasetVersionRecord{ID: "rdv_one", Status: store.RAGEvalDatasetReady, CorpusSHA256: "corpus", DocumentCount: 1},
		Documents:                    []store.RAGEvalCorpusDocumentRecord{{ID: "rdc_one", DatasetVersionID: "rdv_one", ExternalID: "doc-one", FileName: "one.md", ObjectKey: "rag-eval/datasets/d/versions/1/corpus/doc-one/one.md", SHA256: sha, SizeBytes: 12}},
		Ingestion:                    policy,
		Contract:                     rageval.GenerationContract{ParserProtocolVersion: "parser-v1", ParserEngineVersion: "engine-v1", TokenizerVersion: "token-v1", SplitterVersion: "split-v1", ArtifactSchemaVersion: 1, VectorSchemaVersion: "vector-v1", IndexFormatVersion: 1},
		Embedding:                    config.RAGEmbeddingCfg{Endpoint: "https://embedding.invalid/v1", Model: "embed", Dims: 3},
		EmbeddingContractFingerprint: "contract",
	}
}

func TestEvaluationGenerationBuilderReusesExactFingerprintAndIsolatesTarget(t *testing.T) {
	generationStore := newFakeEvaluationGenerationStore()
	pipeline := &fakeEvaluationPipeline{}
	builder, err := NewEvaluationGenerationBuilder(generationStore, pipeline, "worker", 5*time.Second, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	firstRequest := evaluationGenerationFixture("run-one")
	generationStore.seed(firstRequest)
	first, reused, err := builder.Build(context.Background(), firstRequest)
	if err != nil || reused || first.Status != store.RAGEvalGenerationReady || pipeline.buildCalls != 1 {
		t.Fatalf("first=%+v reused=%v builds=%d err=%v", first, reused, pipeline.buildCalls, err)
	}
	if !strings.HasPrefix(first.CollectionKey, "eval_") || strings.Contains(first.CollectionKey, "kb_") ||
		!strings.HasPrefix(first.ObjectPrefix, "rag-eval/generations/") {
		t.Fatalf("evaluation target escaped isolation: %+v", first)
	}
	if _, err := builder.ReleaseRun(context.Background(), "run-one"); err != nil {
		t.Fatal(err)
	}
	secondRequest := evaluationGenerationFixture("run-two")
	generationStore.seed(secondRequest)
	second, reused, err := builder.Build(context.Background(), secondRequest)
	if err != nil || !reused || second.ID != first.ID || pipeline.buildCalls != 1 {
		t.Fatalf("reuse=%+v reused=%v builds=%d err=%v", second, reused, pipeline.buildCalls, err)
	}
	changed := evaluationGenerationFixture("run-three")
	generationStore.seed(changed)
	changed.Ingestion.ChunkSize++
	third, reused, err := builder.Build(context.Background(), changed)
	if err != nil || reused || third.ID == first.ID || pipeline.buildCalls != 2 {
		t.Fatalf("changed=%+v reused=%v builds=%d err=%v", third, reused, pipeline.buildCalls, err)
	}
	crossVersion := evaluationGenerationFixture("run-four")
	crossVersion.DatasetVersion.ID = "rdv_two"
	crossVersion.DatasetVersion.CorpusSHA256 = "different-question-set"
	for index := range crossVersion.Documents {
		crossVersion.Documents[index].ID = "rdc_two"
		crossVersion.Documents[index].DatasetVersionID = crossVersion.DatasetVersion.ID
	}
	generationStore.seed(crossVersion)
	fourth, reused, err := builder.Build(context.Background(), crossVersion)
	if err != nil || !reused || fourth.ID != first.ID || pipeline.buildCalls != 2 {
		t.Fatalf("cross-version=%+v reused=%v builds=%d err=%v", fourth, reused, pipeline.buildCalls, err)
	}
}

func TestEvaluationGenerationBuilderFailureAndEvalOnlyGC(t *testing.T) {
	generationStore := newFakeEvaluationGenerationStore()
	pipeline := &fakeEvaluationPipeline{fail: true}
	builder, _ := NewEvaluationGenerationBuilder(generationStore, pipeline, "worker", 5*time.Second, time.Hour)
	request := evaluationGenerationFixture("run-fail")
	generationStore.seed(request)
	generation, reused, err := builder.Build(context.Background(), request)
	if err == nil || reused || generation.Status != store.RAGEvalGenerationFailed {
		t.Fatalf("failed generation=%+v reused=%v err=%v", generation, reused, err)
	}
	generationStore.gcFence = &store.RAGEvalGenerationFence{GenerationID: generation.ID, LeaseOwner: "worker", FenceToken: 2,
		CollectionKey: generation.CollectionKey, ObjectPrefix: generation.ObjectPrefix}
	cleaned, err := builder.GarbageCollect(context.Background(), time.Now(), 1)
	if err != nil || cleaned != 1 || pipeline.dropCalls != 1 {
		t.Fatalf("GC cleaned=%d drops=%d err=%v", cleaned, pipeline.dropCalls, err)
	}
}

func TestEvaluationPipelineTargetRejectsProductionNamespace(t *testing.T) {
	target, err := NewEvaluationPipelineTarget("admin", "run", "dataset-version", "reg_one")
	if err != nil {
		t.Fatal(err)
	}
	target.CollectionKey = "production-kb"
	if err := target.ValidateEvaluation(); err == nil {
		t.Fatal("production collection accepted as evaluation target")
	}
}
