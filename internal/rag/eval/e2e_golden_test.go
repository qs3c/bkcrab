package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

type phaseHGoldenBundle struct {
	Dataset   CanonicalDataset   `json:"dataset"`
	Documents map[string]string  `json:"documents"`
	Expected  phaseHGoldenExpect `json:"expected"`
}

type phaseHGoldenExpect struct {
	BaselineScore, CandidateScore, PairedDelta float64
	ScoredCases                                int `json:"scoredCases"`
}

type goldenBatchScorer struct{ value float64 }

type goldenCasePipeline struct{}

func (goldenCasePipeline) Execute(_ context.Context, _ CaseExecutionRequest) (CaseExecutionResult, error) {
	return CaseExecutionResult{Response: "A corpus greeting. [1]", Contexts: []string{"hello corpus"}, ContextIDs: []string{"doc-1#0"},
		Citations: []string{"1"}, SearchTrace: map[string]any{"golden": true}, AnswerTrace: map[string]any{"mode": "evaluation"},
		Usage: Usage{Stage: "answer", Provider: "fake", Model: "golden", InputTokens: 2, OutputTokens: 3}}, nil
}

func (s goldenBatchScorer) Evaluate(_ context.Context, request EvaluateRequest) (EvaluateResponse, error) {
	response := EvaluateResponse{RequestID: request.RequestID, RagasVersion: ExpectedRagasVersion, MetricBundleVersion: MetricBundleV1}
	for _, sample := range request.Samples {
		value := s.value
		response.Results = append(response.Results, CaseMetricResults{CaseID: sample.CaseID, Metrics: map[string]MetricResult{
			"faithfulness": {Status: MetricOK, Value: &value},
		}})
	}
	return response, nil
}

func loadPhaseHGolden(t *testing.T) phaseHGoldenBundle {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "rag-eval", "e2e_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var bundle phaseHGoldenBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestPhaseHGoldenImportFullRunsScoreAndCompareE2E(t *testing.T) {
	ctx := context.Background()
	bundle := loadPhaseHGolden(t)
	st := runnerDB(t)
	objectStore := objects.NewLocalFS(t.TempDir())
	datasets, err := NewDatasetService(st, objectStore)
	if err != nil {
		t.Fatal(err)
	}
	dataset := &store.RAGEvalDatasetRecord{Name: bundle.Dataset.Name, Description: bundle.Dataset.Description, CreatedBy: "admin"}
	if err := st.CreateRAGEvalDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(bundle.Dataset)
	documentBody := bundle.Documents["doc-1"]
	imported, err := datasets.ImportCanonical(ctx, DatasetImportRequest{DatasetID: dataset.ID, Version: 1, CreatedBy: "admin",
		Manifest: bytes.NewReader(manifest), Documents: []DatasetDocumentUpload{{ExternalID: "doc-1", FileName: "guide.txt", MediaType: "text/plain", SizeBytes: int64(len(documentBody)), Reader: strings.NewReader(documentBody)}}})
	if err != nil || imported.Version.Status != store.RAGEvalDatasetReady || !imported.Report.Valid {
		t.Fatalf("import=%+v err=%v", imported, err)
	}

	profileData := runnerProfile()
	profileJSON, _ := json.Marshal(profileData)
	profile := &store.RAGEvalProfileRecord{Name: "phase-h-golden", ProfileJSON: string(profileJSON), Fingerprint: strings.Repeat("c", 64), CreatedBy: "admin"}
	if err := st.CreateRAGEvalProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	cfg := config.RAGEvaluationCfg{WorkerConcurrency: 1, MaxBatchSize: 1, MaxRunCases: 10, MaxRunTokens: 1000, MaxRunCostUSD: 10, MaxRunDurationSec: 60}
	cfg.ApplyDefaults()
	pipeline := goldenCasePipeline{}
	generation := &fakeGenerationProvider{generation: store.RAGEvalGenerationRecord{ID: "phase-h-generation", DatasetVersionID: imported.Version.ID, Status: store.RAGEvalGenerationReady}}

	runGolden := func(score float64, baseline string) string {
		runner, createErr := NewRunner(st, generation, pipeline, goldenBatchScorer{value: score}, cfg, "phase-h-runner")
		if createErr != nil {
			t.Fatal(createErr)
		}
		run, createErr := runner.CreateRun(ctx, CreateRunRequest{DatasetVersionID: imported.Version.ID, ProfileID: profile.ID,
			Mode: RunModeFull, BaselineRunID: baseline, Metrics: []string{"faithfulness"}, CreatedBy: "admin"})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if runErr := runner.Run(ctx, run.ID); runErr != nil {
			t.Fatal(runErr)
		}
		finished, getErr := st.GetRAGEvalRun(ctx, run.ID)
		if getErr != nil || finished.Status != store.RAGEvalRunSucceeded {
			t.Fatalf("run=%+v err=%v", finished, getErr)
		}
		return run.ID
	}

	baselineID := runGolden(bundle.Expected.BaselineScore, "")
	candidateID := runGolden(bundle.Expected.CandidateScore, baselineID)
	delta, err := (AnalysisService{Store: st}).CompareRunMetric(ctx, baselineID, candidateID, "faithfulness")
	if err != nil || delta.Pairs != bundle.Expected.ScoredCases || delta.AbsoluteDelta == nil ||
		math.Abs(*delta.AbsoluteDelta-bundle.Expected.PairedDelta) > 1e-9 {
		t.Fatalf("delta=%+v err=%v", delta, err)
	}
	if tokens, _, err := st.RAGEvalUsageTotals(ctx, candidateID); err != nil || tokens != 5 {
		t.Fatalf("candidate usage tokens=%d err=%v", tokens, err)
	}
}
