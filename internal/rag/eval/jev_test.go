package eval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

func TestJevBundleWireValidation(t *testing.T) {
	c, _ := NewRagasClient("http://localhost", "test", 0, 2, 20, 65536)
	c.ConfigureBundle("rag-jev-hybrid-v1", "jev-1.13.0")
	r := EvaluateRequest{OwnerID: "admin", RequestID: "id", MetricBundleVersion: "rag-jev-hybrid-v1", JevModel: "jev-1.13.0", Metrics: []string{"faithfulness"}, Samples: []EvaluationSample{{CaseID: "c", UserInput: "q", Response: "a", RetrievedContexts: []string{"ctx"}, RetrievedContextIDs: []string{"1"}}}}
	if e := c.validateEvaluateRequest(&r); e != nil {
		t.Fatal(e)
	}
	r.MetricBundleVersion = MetricBundleV1
	if e := c.validateEvaluateRequest(&r); e == nil {
		t.Fatal("legacy request accepted")
	}
	r.MetricBundleVersion = "rag-jev-hybrid-v1"
	wire := wireEvaluateResponse{RequestID: r.RequestID, RagasVersion: ExpectedRagasVersion, MetricBundleVersion: MetricBundleV1, Usage: &EvaluatorUsage{}}
	if _, e := validateWireResponse(r, wire); e == nil {
		t.Fatal("mismatched response accepted")
	}
}
func TestJevRunnerPersistsBundleAndSeparatesUsage(t *testing.T) {
	scorer := &fakeBatchScorer{usage: EvaluatorUsage{JevModel: "jev-1.13.0", JevInputTokens: 11, JevOutputTokens: 2, JevEstimatedCostUSD: .003, LLMInputTokens: 5, LLMEstimatedCostUSD: .01}}
	runner, st, _, _, oldID := runnerFixture(t, 1, scorer, map[string]error{})
	old, _ := st.GetRAGEvalRun(context.Background(), oldID)
	runner.cfg.Sidecar.MetricBundleVersion = "rag-jev-hybrid-v1"
	runner.cfg.Sidecar.JevModel = "jev-1.13.0"
	created, e := runner.CreateRun(context.Background(), CreateRunRequest{DatasetVersionID: old.DatasetVersionID, ProfileID: old.ProfileID, Mode: RunModeFull, Metrics: []string{"faithfulness", "hit_at_k"}, CreatedBy: "admin"})
	if e != nil {
		t.Fatal(e)
	}
	if e = runner.Run(context.Background(), created.ID); e != nil {
		t.Fatal(e)
	}
	metrics, e := st.ListRAGEvalMetricResults(context.Background(), created.ID, "", 20)
	if e != nil || len(metrics) != 2 {
		t.Fatalf("%v %v", metrics, e)
	}
	for _, v := range metrics {
		if v.MetricVersion != "rag-jev-hybrid-v1" {
			t.Fatal("stored old version", v)
		}
	}
	var jevTokens, judgeTokens int64
	if e = st.DB().QueryRow(`SELECT input_tokens+output_tokens FROM rag_eval_usage WHERE run_id=? AND stage='judge_jev'`, created.ID).Scan(&jevTokens); e != nil || jevTokens != 13 {
		t.Fatalf("Jev usage not recorded separately: tokens=%d err=%v", jevTokens, e)
	}
	if e = st.DB().QueryRow(`SELECT input_tokens+output_tokens FROM rag_eval_usage WHERE run_id=? AND stage='judge'`, created.ID).Scan(&judgeTokens); e != nil || judgeTokens != 5 {
		t.Fatalf("LLM usage not recorded separately: tokens=%d err=%v", judgeTokens, e)
	}
	var snap ExecutionSnapshot
	json.Unmarshal([]byte(created.ExecutionSnapshotJSON), &snap)
	if snap.JevModel != "jev-1.13.0" {
		t.Fatal("model not frozen")
	}
	if _, e = (AnalysisService{Store: st}).CompareRunMetric(context.Background(), oldID, created.ID, "faithfulness"); e == nil {
		t.Fatal("cross-bundle comparison allowed")
	}
	if e = runner.Run(context.Background(), oldID); e != nil && !strings.Contains(e.Error(), "evaluator") {
		t.Fatal(e)
	}
	old, _ = st.GetRAGEvalRun(context.Background(), oldID)
	if old.Status != store.RAGEvalRunFailed {
		t.Fatal("old bundle resumed", old.Status)
	}
}
