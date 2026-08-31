package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/dialogue"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeGenerationProvider struct {
	generation store.RAGEvalGenerationRecord
	releases   int
}

type blockingGenerationProvider struct {
	started chan struct{}
	once    sync.Once
}

func (p *blockingGenerationProvider) Ensure(ctx context.Context, _ *store.RAGEvalRunRecord, _ ExecutionSnapshot, _ func(GenerationProgress) error) (*store.RAGEvalGenerationRecord, error) {
	p.once.Do(func() { close(p.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*blockingGenerationProvider) Release(context.Context, string) error { return nil }

func TestProgressJSONKeepsIndependentCounters(t *testing.T) {
	raw, err := json.Marshal(Progress{Total: 4, Completed: 3, Failed: 1, Scored: 2, Tokens: 9, CostUSD: 1.25,
		DocumentsTotal: 8, DocumentsCompleted: 5, ChunksCompleted: 13,
		EvaluationStartedAt: "2026-08-18T15:10:00Z", LastActivityAt: "2026-08-18T16:10:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"total": 4, "completed": 3, "failed": 1, "scored": 2, "tokens": 9, "costUsd": 1.25,
		"documentsTotal": 8, "documentsCompleted": 5, "chunksCompleted": 13}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("progress %s=%v, want %v; json=%s", key, got[key], value, raw)
		}
	}
}

func TestRunnerDurationBudgetExcludesGenerationPreparationAndSurvivesRetry(t *testing.T) {
	t.Run("run creation time does not consume the online evaluation budget", func(t *testing.T) {
		runner, st, _, _, runID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
		run, err := st.GetRAGEvalRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot ExecutionSnapshot
		if err = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot); err != nil {
			t.Fatal(err)
		}
		snapshot.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
		snapshot.Budgets.MaxDurationSec = 1
		raw, _ := json.Marshal(snapshot)
		if _, err = st.DB().Exec(`UPDATE rag_eval_runs SET execution_snapshot_json=? WHERE id=?`, string(raw), runID); err != nil {
			t.Fatal(err)
		}

		if err = runner.Run(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		run, err = st.GetRAGEvalRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		var progress Progress
		if err = json.Unmarshal([]byte(run.ProgressJSON), &progress); err != nil {
			t.Fatal(err)
		}
		if run.Status != store.RAGEvalRunSucceeded || progress.EvaluationStartedAt == "" {
			t.Fatalf("status=%s progress=%+v", run.Status, progress)
		}
	})

	t.Run("persisted online evaluation start still enforces the budget after retry", func(t *testing.T) {
		runner, st, pipeline, _, runID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
		run, err := st.GetRAGEvalRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot ExecutionSnapshot
		if err = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot); err != nil {
			t.Fatal(err)
		}
		snapshot.Budgets.MaxDurationSec = 1
		snapshotRaw, _ := json.Marshal(snapshot)
		progressRaw, _ := json.Marshal(Progress{EvaluationStartedAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)})
		if _, err = st.DB().Exec(`UPDATE rag_eval_runs SET execution_snapshot_json=?,progress_json=? WHERE id=?`, string(snapshotRaw), string(progressRaw), runID); err != nil {
			t.Fatal(err)
		}

		if err = runner.Run(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		run, err = st.GetRAGEvalRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != store.RAGEvalRunBudgetExceeded || run.ErrorCode != "duration_budget_exceeded" {
			t.Fatalf("status=%s code=%s", run.Status, run.ErrorCode)
		}
		if len(pipeline.calls) != 0 {
			t.Fatalf("expired retry executed %d cases", len(pipeline.calls))
		}
	})
}

func TestRunnerRetryCreatesFreshQueuedRun(t *testing.T) {
	runner, st, _, _, sourceRunID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
	source, err := st.GetRAGEvalRun(context.Background(), sourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE rag_eval_runs SET status=?,stage='finished',error_code='duration_budget_exceeded',finished_at=? WHERE id=?`,
		store.RAGEvalRunBudgetExceeded, time.Now().UTC(), sourceRunID); err != nil {
		t.Fatal(err)
	}

	retried, err := runner.RetryRun(context.Background(), sourceRunID, "rer_retry", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != "rer_retry" || retried.Status != store.RAGEvalRunQueued || retried.DatasetVersionID != source.DatasetVersionID ||
		retried.ProfileID != source.ProfileID || retried.Mode != source.Mode {
		t.Fatalf("retried run=%+v", retried)
	}
	var sourceSnapshot, retrySnapshot ExecutionSnapshot
	if err = json.Unmarshal([]byte(source.ExecutionSnapshotJSON), &sourceSnapshot); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(retried.ExecutionSnapshotJSON), &retrySnapshot); err != nil {
		t.Fatal(err)
	}
	if !retrySnapshot.CreatedAt.After(sourceSnapshot.CreatedAt) || retrySnapshot.Budgets.MaxDurationSec != runner.cfg.MaxRunDurationSec {
		t.Fatalf("source snapshot=%+v retry snapshot=%+v", sourceSnapshot, retrySnapshot)
	}
	if _, err = runner.RetryRun(context.Background(), retried.ID, "rer_invalid", "admin"); !errors.Is(err, ErrRunNotRetryable) {
		t.Fatalf("queued retry error=%v", err)
	}
}

func (p *fakeGenerationProvider) Ensure(_ context.Context, _ *store.RAGEvalRunRecord, snapshot ExecutionSnapshot, report func(GenerationProgress) error) (*store.RAGEvalGenerationRecord, error) {
	if report != nil {
		if err := report(GenerationProgress{Stage: "building_generation", DocumentsCompleted: snapshot.DatasetVersion.DocumentCount,
			DocumentsTotal: snapshot.DatasetVersion.DocumentCount, ChunksCompleted: p.generation.ChunkCount}); err != nil {
			return nil, err
		}
	}
	return &p.generation, nil
}
func (p *fakeGenerationProvider) Release(context.Context, string) error { p.releases++; return nil }

type fakeCasePipeline struct {
	mu       sync.Mutex
	calls    map[string]int
	failures map[string]error
	history  map[string][]dialogue.Turn
}

func (p *fakeCasePipeline) Execute(_ context.Context, request CaseExecutionRequest) (CaseExecutionResult, error) {
	p.mu.Lock()
	p.calls[request.Case.ID]++
	p.history[request.Case.ID] = dialogue.Clone(request.Case.History)
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

type idempotentRetryScorer struct {
	mu          sync.Mutex
	bodies      map[string]string
	caseCalls   map[string]int
	requestIDs  []string
	failCaseID  string
	failedBatch bool
}

type permanentErrorScorer struct{}

func (permanentErrorScorer) Evaluate(context.Context, EvaluateRequest) (EvaluateResponse, error) {
	return EvaluateResponse{}, &evaluatorHTTPError{StatusCode: http.StatusConflict, Detail: "requestId body mismatch"}
}

func (s *idempotentRetryScorer) Evaluate(_ context.Context, request EvaluateRequest) (EvaluateResponse, error) {
	s.mu.Lock()
	body, _ := json.Marshal(request)
	if previous, ok := s.bodies[request.RequestID]; ok && previous != string(body) {
		s.mu.Unlock()
		return EvaluateResponse{}, &evaluatorHTTPError{StatusCode: http.StatusConflict, Detail: "requestId body mismatch"}
	}
	s.requestIDs = append(s.requestIDs, request.RequestID)
	shouldFail := !s.failedBatch
	if shouldFail {
		shouldFail = false
		for _, sample := range request.Samples {
			if sample.CaseID == s.failCaseID {
				shouldFail = true
				break
			}
		}
	}
	if shouldFail {
		s.failedBatch = true
		s.mu.Unlock()
		// Let the sibling batch finish and persist before errgroup cancels it.
		time.Sleep(50 * time.Millisecond)
		return EvaluateResponse{}, errors.New("judge unavailable")
	}
	s.bodies[request.RequestID] = string(body)
	response := EvaluateResponse{RequestID: request.RequestID, RagasVersion: ExpectedRagasVersion, MetricBundleVersion: MetricBundleV1}
	for _, sample := range request.Samples {
		s.caseCalls[sample.CaseID]++
		value := .8
		response.Results = append(response.Results, CaseMetricResults{CaseID: sample.CaseID, Metrics: map[string]MetricResult{"faithfulness": {Status: MetricOK, Value: &value}}})
	}
	s.mu.Unlock()
	return response, nil
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
	version := &store.RAGEvalDatasetVersionRecord{DatasetID: dataset.ID, Version: 1, SourceType: "canonical", Track: store.RAGEvalTrackTextRAG,
		CreatedBy: "admin", CaseCount: int64(caseCount), DocumentCount: 1, CorpusSHA256: strings.Repeat("a", 64)}
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
	pipeline := &fakeCasePipeline{calls: map[string]int{}, failures: failures, history: map[string][]dialogue.Turn{}}
	generations := &fakeGenerationProvider{generation: store.RAGEvalGenerationRecord{ID: "generation", DatasetVersionID: version.ID,
		Status: store.RAGEvalGenerationReady, DocumentCount: 1, ChunkCount: 3}}
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
	var progress Progress
	if err := json.Unmarshal([]byte(run.ProgressJSON), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.DocumentsCompleted != 1 || progress.DocumentsTotal != 1 || progress.ChunksCompleted != 3 ||
		progress.ParserEngine != "canonical-text (parser bypassed)" || progress.LastActivityAt == "" {
		t.Fatalf("progress=%+v", progress)
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
		if len(history) != 1 || history[0] != (dialogue.Turn{Role: dialogue.RoleUser, Content: "earlier-user-question"}) {
			t.Fatalf("case %s history=%v", caseID, history)
		}
	}
}

func TestRunnerMarksAllCaseFailuresAsFailed(t *testing.T) {
	runner, st, pipeline, _, runID := runnerFixture(t, 2, &fakeBatchScorer{}, map[string]error{})
	run, err := st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	datasetCases, err := st.ListRAGEvalCases(context.Background(), run.DatasetVersionID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range datasetCases {
		pipeline.failures[item.ID] = errors.New("reranker unavailable")
	}
	if err = runner.Run(context.Background(), runID); err == nil {
		t.Fatal("all-case failure must return a terminal error")
	}
	run, err = st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RAGEvalRunFailed || run.ErrorCode != "all_cases_failed" {
		t.Fatalf("status=%s code=%s", run.Status, run.ErrorCode)
	}
	var progress Progress
	if err = json.Unmarshal([]byte(run.ProgressJSON), &progress); err != nil {
		t.Fatal(err)
	}
	if progress.Completed != 2 || progress.Failed != 2 || progress.Scored != 0 {
		t.Fatalf("progress=%+v", progress)
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

func TestRunnerScoringRetrySkipsCompletedMetricsWhenCaseSetChanges(t *testing.T) {
	runner, st, pipeline, _, runID := runnerFixture(t, 5, &fakeBatchScorer{}, map[string]error{})
	run, err := st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	cases, err := st.ListRAGEvalCases(context.Background(), run.DatasetVersionID, "", 10)
	if err != nil || len(cases) != 5 {
		t.Fatalf("cases=%d err=%v", len(cases), err)
	}
	pipeline.failures[cases[1].ID] = errors.New("transient answer failure")
	scorer := &idempotentRetryScorer{bodies: map[string]string{}, caseCalls: map[string]int{}, failCaseID: cases[0].ID}
	runner.scorer = scorer
	runner.cfg.ScoreConcurrency = 2
	runner.lease = 80 * time.Millisecond

	if err = runner.Run(context.Background(), runID); err == nil {
		t.Fatal("first scoring attempt should fail")
	}
	run, err = st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var firstProgress Progress
	if err = json.Unmarshal([]byte(run.ProgressJSON), &firstProgress); err != nil {
		t.Fatal(err)
	}
	if firstProgress.Scored != 2 || run.Stage != "scoring_retry" {
		t.Fatalf("partial progress=%+v stage=%s", firstProgress, run.Stage)
	}
	completedBeforeRetry := map[string]int{}
	for caseID, calls := range scorer.caseCalls {
		completedBeforeRetry[caseID] = calls
	}
	if len(completedBeforeRetry) != 2 {
		t.Fatalf("completed first-wave cases=%v", completedBeforeRetry)
	}

	delete(pipeline.failures, cases[1].ID)
	time.Sleep(100 * time.Millisecond)
	if err = runner.Run(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	run, err = st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var finalProgress Progress
	if err = json.Unmarshal([]byte(run.ProgressJSON), &finalProgress); err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RAGEvalRunSucceeded || finalProgress.Scored != 5 {
		t.Fatalf("run status=%s progress=%+v", run.Status, finalProgress)
	}
	for caseID := range completedBeforeRetry {
		if scorer.caseCalls[caseID] != 1 {
			t.Fatalf("completed case %s rescored %d times", caseID, scorer.caseCalls[caseID])
		}
	}
	for _, requestID := range scorer.requestIDs {
		if !strings.HasPrefix(requestID, runID+":score:") {
			t.Fatalf("request id is not content-addressed: %s", requestID)
		}
	}
	metrics, err := st.ListRAGEvalMetricResults(context.Background(), runID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	faithfulness := 0
	for _, metric := range metrics {
		if metric.MetricName == "faithfulness" {
			faithfulness++
		}
	}
	if faithfulness != 5 {
		t.Fatalf("faithfulness results=%d", faithfulness)
	}
}

func TestRunnerStopsRetryingPermanentEvaluatorRejection(t *testing.T) {
	runner, st, _, _, runID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
	runner.scorer = permanentErrorScorer{}

	if err := runner.Run(context.Background(), runID); err == nil {
		t.Fatal("permanent evaluator rejection must fail the run")
	}
	run, err := st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RAGEvalRunFailed || run.Stage != "finished" || run.ErrorCode != "evaluator_request_rejected" {
		t.Fatalf("status=%s stage=%s code=%s", run.Status, run.Stage, run.ErrorCode)
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
	t.Run("deployment cost kill switch", func(t *testing.T) {
		scorer := &fakeBatchScorer{usage: EvaluatorUsage{LLMInputTokens: 3, LLMOutputTokens: 2, LLMEstimatedCostUSD: .02}}
		runner, st, _, _, runID := runnerFixture(t, 1, scorer, nil)
		run, _ := st.GetRAGEvalRun(context.Background(), runID)
		var snapshot ExecutionSnapshot
		_ = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &snapshot)
		snapshot.Budgets.MaxCostUSD = .01
		raw, _ := json.Marshal(snapshot)
		_, _ = st.DB().Exec(`UPDATE rag_eval_runs SET execution_snapshot_json=? WHERE id=?`, string(raw), runID)
		runner.cfg.CostBudgetDisabled = true
		if err := runner.Run(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		run, _ = st.GetRAGEvalRun(context.Background(), runID)
		if run.Status != store.RAGEvalRunSucceeded {
			t.Fatalf("status=%s", run.Status)
		}
	})
}

func TestRunnerCancellationInterruptsGeneration(t *testing.T) {
	runner, st, _, _, runID := runnerFixture(t, 1, &fakeBatchScorer{}, nil)
	blocking := &blockingGenerationProvider{started: make(chan struct{})}
	runner.generations = blocking
	runner.lease = 90 * time.Millisecond

	result := make(chan error, 1)
	go func() { result <- runner.Run(context.Background(), runID) }()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("generation did not start")
	}
	if ok, err := st.RequestCancelRAGEvalRun(context.Background(), runID); err != nil || !ok {
		t.Fatalf("request cancellation: ok=%v error=%v", ok, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("generation did not stop after cancellation")
	}
	run, err := st.GetRAGEvalRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RAGEvalRunCancelled || !run.FinishedAt.Valid {
		t.Fatalf("run status=%s finished=%v", run.Status, run.FinishedAt.Valid)
	}
}
