package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/store"
)

func promotionStore(t *testing.T) (*store.DBStore, *RuntimePolicySnapshot, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.NewDBStore("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared", uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	active := config.RAGRuntimePolicyData{Version: 1, TopN: 5, CandidateTopK: 20, MinScore: .4, Temperature: .2, MaxTokens: 1000, RAGPromptBundleVersion: RAGAnswerPromptBundleV1}
	raw, _ := json.Marshal(active)
	fingerprint, _ := rageval.Fingerprint(active)
	record := &store.RAGPolicyRecord{Kind: store.RAGPolicyRuntime, Version: 1, PolicyJSON: string(raw), Fingerprint: fingerprint, CreatedBy: "admin"}
	if err = st.CreateRAGPolicy(ctx, record); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateRAGPolicy(ctx, store.RAGPolicyRuntime, 0, 1, "admin", "", "bootstrap", store.RAGPolicyAuditPublish); err != nil || !ok {
		t.Fatalf("activate=%v %v", ok, err)
	}
	snapshot, _ := NewRuntimePolicySnapshot(active)
	profile := config.RAGEvalProfileData{Ingestion: config.RAGIngestionPolicyData{Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard, Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "embed-contract", Model: "embed", Dims: 8}}, Runtime: active, RewriteEnabled: true, HyDEEnabled: true, RerankerEnabled: true, RerankerFailurePolicy: config.RAGRerankerFallbackRRF, AnswerModel: "provider/model"}
	profile.Runtime.MinScore = .7
	execution := rageval.ExecutionSnapshot{Version: 1, Profile: profile, CreatedAt: nowUTC()}
	executionJSON, _ := json.Marshal(execution)
	metricsJSON := `["faithfulness"]`
	run := &store.RAGEvalRunRecord{DatasetVersionID: "dataset", Mode: store.RAGEvalRunModeOnlineOnly, ProfileID: "profile", IndexGenerationID: "generation", RequestedMetricsJSON: metricsJSON, ExecutionSnapshotJSON: string(executionJSON), CreatedBy: "admin"}
	if err = st.CreateRAGEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`UPDATE rag_eval_runs SET status=?,finished_at=CURRENT_TIMESTAMP WHERE id=?`, store.RAGEvalRunSucceeded, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO rag_eval_metric_results(run_id,case_id,metric_name,metric_version,status,value,reason,details_json) VALUES(?,?,?,?,?,?,?,?)`, run.ID, "case", "faithfulness", rageval.MetricBundleV1, store.RAGEvalMetricOK, .9, "", "{}"); err != nil {
		t.Fatal(err)
	}
	if _, err = st.DB().Exec(`INSERT INTO rag_eval_case_results(run_id,case_id,response,contexts_json,citations_json,search_trace_json,answer_trace_json,status,error_code,error_message,latency_ms,usage_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, run.ID, "case", "answer", "[]", "[]", "{}", "{}", store.RAGEvalCaseOK, "", "", 50, "{}"); err != nil {
		t.Fatal(err)
	}
	return st, snapshot, run.ID
}

func nowUTC() time.Time { return time.Now().UTC() }

func TestRuntimePromotionGatesWhitelistCASRefreshAndRollback(t *testing.T) {
	st, snapshot, runID := promotionStore(t)
	service := &PolicyPromotionService{Store: st, Snapshot: snapshot, Gates: PromotionGates{MinimumMetricMean: map[string]float64{"faithfulness": .8}, MinimumScoredCases: 1, MaximumP95LatencyMS: 100}, Environment: RuntimePromotionEnvironment{RewriteEnabled: true, HyDEEnabled: true, RerankerEnabled: true, AnswerModel: "provider/model"}}
	result, err := service.PromoteRuntime(context.Background(), RuntimePromotionRequest{RunID: runID, ProfileID: "profile", ConfirmationRunID: runID, ActorID: "admin", Note: "promote min score", Fields: []string{"minScore"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision.Version != 2 || !result.GateReport.Passed || snapshot.Current().Version != 2 || snapshot.Current().MinScore != .7 {
		t.Fatalf("result=%+v snapshot=%+v", result, snapshot.Current())
	}
	if err = service.RollbackRuntime(context.Background(), 2, 1, "admin", "rollback"); err != nil {
		t.Fatal(err)
	}
	if snapshot.Current().Version != 1 {
		t.Fatalf("snapshot=%+v", snapshot.Current())
	}
	audits, err := st.ListRAGPolicyAudits(context.Background(), store.RAGPolicyRuntime, 10)
	hasRollback := false
	for _, audit := range audits {
		if audit.Action == store.RAGPolicyAuditRollback && audit.FromVersion == 2 && audit.ToVersion == 1 {
			hasRollback = true
		}
	}
	if err != nil || len(audits) != 3 || !hasRollback {
		t.Fatalf("audits=%+v err=%v", audits, err)
	}
	bad := config.RAGRuntimePolicyData{Version: 3, TopN: 5, CandidateTopK: 20, MinScore: .8, Temperature: .2, MaxTokens: 1000, RAGPromptBundleVersion: RAGAnswerPromptBundleV1}
	raw, _ := json.Marshal(bad)
	if err = st.CreateRAGPolicy(context.Background(), &store.RAGPolicyRecord{Kind: store.RAGPolicyRuntime, Version: 3, PolicyJSON: string(raw), Fingerprint: "wrong", CreatedBy: "admin"}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateRAGPolicy(context.Background(), store.RAGPolicyRuntime, 1, 3, "admin", "", "bad", store.RAGPolicyAuditPublish); err != nil || !ok {
		t.Fatalf("activate bad=%v %v", ok, err)
	}
	refresher := RuntimePolicyRefresher{Store: st, Snapshot: snapshot}
	if err = refresher.RefreshOnce(context.Background()); err == nil {
		t.Fatal("fingerprint mismatch refreshed")
	}
	if snapshot.Current().Version != 1 {
		t.Fatal("invalid revision replaced last good snapshot")
	}
}

func TestRuntimePromotionRejectsGateFailureUnknownAndUnpublishedDependencies(t *testing.T) {
	t.Run("unconfigured", func(t *testing.T) {
		st, snapshot, runID := promotionStore(t)
		service := &PolicyPromotionService{Store: st, Snapshot: snapshot}
		_, err := service.PromoteRuntime(context.Background(), RuntimePromotionRequest{RunID: runID, ProfileID: "profile", ConfirmationRunID: runID, ActorID: "admin", Fields: []string{"minScore"}})
		if !errors.Is(err, ErrPromotionGatesUnconfigured) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		st, snapshot, runID := promotionStore(t)
		service := &PolicyPromotionService{Store: st, Snapshot: snapshot, Gates: PromotionGates{MinimumMetricMean: map[string]float64{"faithfulness": .8}, MinimumScoredCases: 1}}
		_, err := service.PromoteRuntime(context.Background(), RuntimePromotionRequest{RunID: runID, ProfileID: "profile", ConfirmationRunID: runID, ActorID: "admin", Fields: []string{"answerModel"}})
		if err == nil || !strings.Contains(err.Error(), "not publishable") {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("quality", func(t *testing.T) {
		st, snapshot, runID := promotionStore(t)
		service := &PolicyPromotionService{Store: st, Snapshot: snapshot, Gates: PromotionGates{MinimumMetricMean: map[string]float64{"faithfulness": .95}, MinimumScoredCases: 1}, Environment: RuntimePromotionEnvironment{RewriteEnabled: true, HyDEEnabled: true, RerankerEnabled: true, AnswerModel: "provider/model"}}
		_, err := service.PromoteRuntime(context.Background(), RuntimePromotionRequest{RunID: runID, ProfileID: "profile", ConfirmationRunID: runID, ActorID: "admin", Fields: []string{"minScore"}})
		if !errors.Is(err, ErrPromotionGateFailed) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("unpublished", func(t *testing.T) {
		st, snapshot, runID := promotionStore(t)
		run, _ := st.GetRAGEvalRun(context.Background(), runID)
		var execution rageval.ExecutionSnapshot
		_ = json.Unmarshal([]byte(run.ExecutionSnapshotJSON), &execution)
		execution.Profile.Runtime.Temperature = .5
		raw, _ := json.Marshal(execution)
		_, _ = st.DB().Exec(`UPDATE rag_eval_runs SET execution_snapshot_json=? WHERE id=?`, string(raw), runID)
		service := &PolicyPromotionService{Store: st, Snapshot: snapshot, Gates: PromotionGates{MinimumMetricMean: map[string]float64{"faithfulness": .8}, MinimumScoredCases: 1}, Environment: RuntimePromotionEnvironment{RewriteEnabled: true, HyDEEnabled: true, RerankerEnabled: true, AnswerModel: "provider/model"}}
		_, err := service.PromoteRuntime(context.Background(), RuntimePromotionRequest{RunID: runID, ProfileID: "profile", ConfirmationRunID: runID, ActorID: "admin", Fields: []string{"minScore"}})
		if err == nil || !strings.Contains(err.Error(), "unpublished") {
			t.Fatalf("err=%v", err)
		}
	})
}
