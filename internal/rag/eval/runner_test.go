package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeGenerationProvider struct {
	generation store.RAGEvalGenerationRecord
	releases   int
}

func TestProgressJSONKeepsIndependentCounters(t *testing.T) {
	raw, err := json.Marshal(Progress{Total: 4, Completed: 3, Failed: 1, Scored: 2, Tokens: 9, CostUSD: 1.25})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"total": 4, "completed": 3, "failed": 1, "scored": 2, "tokens": 9, "costUsd": 1.25}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("progress %s=%v, want %v; json=%s", key, got[key], value, raw)
		}
	}
}

func (p *fakeGenerationProvider) Ensure(context.Context, *store.RAGEvalRunRecord, ExecutionSnapshot) (*store.RAGEvalGenerationRecord, error) {
	return &p.generation, nil
}
func (p *fakeGenerationProvider) Release(context.Context, string) error { p.releases++; return nil }

type fakeCasePipeline struct {
	mu       sync.Mutex
	calls    map[string]int
	failures map[string]error
	history  map[string][]string
}

func (p *fakeCasePipeline) Execute(_ context.Context, request CaseExecutionRequest) (CaseExecutionResult, error) {
	p.mu.Lock()
	p.calls[request.Case.ID]++
	p.history[request.Case.ID] = append([]string(nil), request.Case.History...)
	err := p.failures[request.Case.ID]
	p.mu.Unlock()
	result := CaseExecutionResult{Response: "answer [1]", Contexts: []string{"ctx"}, ContextIDs: []string{"doc:0"}, DocumentIDs: []string{"doc"}, Citations: []string{"1"}, SearchTrace: map[string]any{"ok": true}, AnswerTrace: map[string]any{"mode": "evaluation"}, Latency: time.Millisecond, Usage: Usage{Stage: "answer", Provider: "fake", Model: "fake/model", InputTokens: 2, OutputTokens: 3}}
	return result, err
}

type fakeBatchScorer struct {
	mu       sync.Mutex
	failures int
	calls    int
	usage    EvaluatorUsage
}

func (s *fakeBatchScorer) Evaluate(_ context.Context, request EvaluateRequest) (EvaluateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.failures > 0 {
		s.failures--
		return EvaluateResponse{}, errors.New("judge unavailable")
	}
	response := EvaluateResponse{RequestID: request.RequestID, RagasVersion: ExpectedRagasVersion, MetricBundleVersion: MetricBundleV1, Usage: s.usage}
	for _, sample := range request.Samples {
		value := .8
		response.Results = append(response.Results, CaseMetricResults{CaseID: sample.CaseID, Metrics: map[string]MetricResult{"faithfulness": {Status: MetricOK, Value: &value}}})
	}
	return response, nil
}

func runnerDB(t *testing.T) *store.DBStore {
	t.Helper()
	st, err := store.NewDBStore("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func runnerProfile() config.RAGEvalProfileData {
	return config.RAGEvalProfileData{Ingestion: config.RAGIngestionPolicyData{Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard, Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "contract", Model: "embed", Dims: 8}}, Runtime: config.RAGRuntimePolicyData{Version: 1, TopN: 2, CandidateTopK: 4, MinScore: .1, Temperature: .1, MaxTokens: 100, RAGPromptBundleVersion: "rag-answer-v1"}, RerankerFailurePolicy: config.RAGRerankerFallbackRRF, AnswerModel: "fake/model"}
}

func runnerFixture(t *testing.T, caseCount int, scorer *fakeBatchScorer, failures map[string]error) (*Runner, *store.DBStore, *fakeCasePipeline, *fakeGenerationProvider, string) {
	t.Helper()
	ctx := context.Background()
	st := runnerDB(t)
	dataset := &store.RAGEvalDatasetRecord{Name: "runner", CreatedBy: "admin"}
	if err := st.CreateRAGEvalDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	version := &store.RAGEvalDatasetVersionRecord{DatasetID: dataset.ID, Version: 1, SourceType: "canonical", CreatedBy: "admin", CaseCount: int64(caseCount), CorpusSHA256: strings.Repeat("a", 64)}
	if err := st.CreateRAGEvalDatasetVersion(ctx, version); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < caseCount; i++ {
		item := &store.RAGEvalCaseRecord{DatasetVersionID: version.ID, ExternalID: fmt.Sprintf("case-%d", i), UserInput: "question", ReferenceAnswer: "reference", ReferenceContextsJSON: `["ctx"]`, ReferenceContextIDsJSON: `["doc:0"]`, ReferenceDocumentIDsJSON: `["doc"]`, HistoryJSON: `["earlier-user-question"]`, TagsJSON: `["smoke"]`, MetadataJSON: `{}`}
		if err := st.PutRAGEvalCase(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := st.TransitionRAGEvalDatasetVersion(ctx, version.ID, store.RAGEvalDatasetDraft, store.RAGEvalDatasetValidating, `{}`); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := st.TransitionRAGEvalDatasetVersion(ctx, version.ID, store.RAGEvalDatasetValidating, store.RAGEvalDatasetReady, `{}`); err != nil || !ok {
		t.Fatal(err)
	}
	profileData := runnerProfile()
	raw, _ := json.Marshal(profileData)
	profile := &store.RAGEvalProfileRecord{Name: "frozen", ProfileJSON: string(raw), Fingerprint: strings.Repeat("b", 64), CreatedBy: "admin"}
	if err := st.CreateRAGEvalProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	cfg := config.RAGEvaluationCfg{WorkerConcurrency: 2, MaxBatchSize: 2, MaxRunCases: 10, MaxRunTokens: 1000, MaxRunCostUSD: 10, MaxRunDurationSec: 60}
	cfg.ApplyDefaults()
	pipeline := &fakeCasePipeline{calls: map[string]int{}, failures: failures, history: map[string][]string{}}
	generations := &fakeGenerationProvider{generation: store.RAGEvalGenerationRecord{ID: "generation", DatasetVersionID: version.ID, Status: store.RAGEvalGenerationReady}}
	runner, err := NewRunner(st, generations, pipeline, scorer, cfg, "runner")
	if err != nil {
		t.Fatal(err)
	}
	record, err := runner.CreateRun(ctx, CreateRunRequest{DatasetVersionID: version.ID, ProfileID: profile.ID, Mode: RunModeFull, Metrics: []string{"faithfulness", "hit_at_k"}, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	return runner, st, pipeline, generations, record.ID
}

func TestRunnerFreezesSnapshotAndPersistsPartialCaseFailure(t *testing.T) {
	scorer := &fakeBatchScorer{}
	runner, st, pipeline, generations, runID := runnerFixture(t, 2, scorer, map[string]error{})
	cases, _ := st.ListRAGEvalCases(context.Background(), "", "", 10)
	_ = cases
	// Fail one durable case without aborting the other case or scoring batch.
	all, _ := st.GetRAGEvalRun(context.Background(), runID)
	datasetCases, _ := st.ListRAGEvalCases(context.Background(), all.DatasetVersionID, "", 10)
	pipeline.failures[datasetCases[0].ID] = errors.New("answer failed")
	if err := runner.Run(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	run, _ := st.GetRAGEvalRun(context.Background(), runID)
	if run.Status != store.RAGEvalRunSucceeded {
		t.Fatalf("status=%s", run.Status)
	}
	var snapshot ExecutionSnapshot
	if err := json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Profile.AnswerModel != "fake/model" || snapshot.DatasetVersion.Status != store.RAGEvalDatasetReady {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	results, _ := st.ListRAGEvalCaseResults(context.Background(), runID, "", 10)
	if len(results) != 2 {
		t.Fatalf("results=%d", len(results))
	}
	statuses := map[string]int{}
	for _, result := range results {
		statuses[result.Status]++
	}
	if statuses[store.RAGEvalCaseError] != 1 || statuses[store.RAGEvalCaseOK] != 1 {
		t.Fatalf("statuses=%v", statuses)
	}
	if generations.releases != 1 {
		t.Fatalf("releases=%d", generations.releases)
	}
	pipeline.mu.Lock()
	defer pipeline.mu.Unlock()
	for caseID, history := range pipeline.history {
		if len(history) != 1 || history[0] != "earlier-user-question" {
			t.Fatalf("case %s history=%v", caseID, history)
		}
	}
}

func TestRunnerResumeScoringDoesNotRepeatAnswer(t *testing.T) {
	scorer := &fakeBatchScorer{failures: 1}
	runner, st, pipeline, _, runID := runnerFixture(t, 1, scorer, nil)
	runner.lease = 80 * time.Millisecond
	if err := runner.Run(context.Background(), runID); err == nil {
		t.Fatal("first scoring failure expected")
	}
	time.Sleep(100 * time.Millisecond)
	if err := runner.Run(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	run, _ := st.GetRAGEvalRun(context.Background(), runID)
	if run.Status != store.RAGEvalRunSucceeded {
		t.Fatalf("status=%s", run.Status)
	}
	pipeline.mu.Lock()
	defer pipeline.mu.Unlock()
	for id, calls := range pipeline.calls {
		if calls != 1 {
			t.Fatalf("case %s answered %d times", id, calls)
		}
	}
	if scorer.calls != 2 {
		t.Fatalf("scorer calls=%d", scorer.calls)
	}
}

func TestRunnerCancellationAndBudgetStopNewCases(t *testing.T) {
	t.Run("cancel", func(t *testing.T) {
		runner, st, pipeline, _, runID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
		if ok, err := st.RequestCancelRAGEvalRun(context.Background(), runID); err != nil || !ok {
			t.Fatal(err)
		}
		if err := runner.Run(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		run, _ := st.GetRAGEvalRun(context.Background(), runID)
		if run.Status != store.RAGEvalRunCancelled {
			t.Fatalf("status=%s", run.Status)
		}
		if len(pipeline.calls) != 0 {
			t.Fatal("cancelled run executed a case")
		}
	})
	t.Run("tokens", func(t *testing.T) {
		runner, st, pipeline, _, runID := runnerFixture(t, 2, &fakeBatchScorer{}, nil)
		run, _ := st.GetRAGEvalRun(context.Background(), runID)
		var snapshot ExecutionSnapshot
		_ = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot)
		snapshot.Budgets.MaxTokens = 5
		raw, _ := json.Marshal(snapshot)
		_, _ = st.DB().Exec(`UPDATE rag_eval_runs SET execution_snapshot_json=? WHERE id=?`, string(raw), runID)
		if err := runner.Run(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		run, _ = st.GetRAGEvalRun(context.Background(), runID)
		if run.Status != store.RAGEvalRunBudgetExceeded {
			t.Fatalf("status=%s", run.Status)
		}
		if len(pipeline.calls) != 1 {
			t.Fatalf("calls=%d", len(pipeline.calls))
		}
	})
	t.Run("judge and embedding usage", func(t *testing.T) {
		scorer := &fakeBatchScorer{usage: EvaluatorUsage{LLMInputTokens: 3, LLMOutputTokens: 2, LLMEstimatedCostUSD: .02, EmbeddingInputTokens: 4, EmbeddingEstimatedCostUSD: .01}}
		runner, st, _, _, runID := runnerFixture(t, 1, scorer, nil)
		run, _ := st.GetRAGEvalRun(context.Background(), runID)
		var snapshot ExecutionSnapshot
		_ = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot)
		snapshot.Budgets.MaxTokens = 10
		raw, _ := json.Marshal(snapshot)
		_, _ = st.DB().Exec(`UPDATE rag_eval_runs SET execution_snapshot_json=? WHERE id=?`, string(raw), runID)
		if err := runner.Run(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		run, _ = st.GetRAGEvalRun(context.Background(), runID)
		if run.Status != store.RAGEvalRunBudgetExceeded {
			t.Fatalf("status=%s", run.Status)
		}
		tokens, cost, err := st.RAGEvalUsageTotals(context.Background(), runID)
		if err != nil || tokens != 14 || math.Abs(cost-.03) > 1e-9 {
			t.Fatalf("tokens=%d cost=%v err=%v", tokens, cost, err)
		}
	})
}
