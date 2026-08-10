package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRAGEvalMigrationAndDatasetLifecycle(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	for _, table := range []string{"rag_eval_datasets", "rag_eval_runs", "rag_runtime_policies", "rag_kb_index_generations"} {
		var name string
		if err := st.DB().QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name); err != nil {
			t.Fatalf("missing %s: %v", table, err)
		}
	}
	dataset := &RAGEvalDatasetRecord{Name: "golden", CreatedBy: "admin"}
	if err := st.CreateRAGEvalDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListRAGEvalDatasets(ctx, "", 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("list=%+v err=%v", items, err)
	}
	version := &RAGEvalDatasetVersionRecord{DatasetID: dataset.ID, Version: 1, SourceType: "canonical", CreatedBy: "admin"}
	if err := st.CreateRAGEvalDatasetVersion(ctx, version); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRAGEvalDatasetVersion(ctx, &RAGEvalDatasetVersionRecord{DatasetID: dataset.ID, Version: 1, SourceType: "canonical", CreatedBy: "admin"}); err == nil {
		t.Fatal("duplicate dataset version accepted")
	}
	document := &RAGEvalCorpusDocumentRecord{DatasetVersionID: version.ID, ExternalID: "doc-1", FileName: "one.txt", MediaType: "text/plain", SizeBytes: 3, SHA256: strings.Repeat("a", 64), MetadataJSON: `{}`}
	if err := st.PutRAGEvalCorpusDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	document.FileName = "updated.txt"
	if err := st.PutRAGEvalCorpusDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	documents, err := st.ListRAGEvalCorpusDocuments(ctx, version.ID, "", 500)
	if err != nil || len(documents) != 1 || documents[0].FileName != "updated.txt" {
		t.Fatalf("documents=%+v err=%v", documents, err)
	}
	caseRecord := &RAGEvalCaseRecord{DatasetVersionID: version.ID, ExternalID: "case-1", UserInput: "question", ReferenceContextsJSON: `[]`, ReferenceContextIDsJSON: `[]`, HistoryJSON: `[]`, TagsJSON: `[]`, MetadataJSON: `{}`}
	if err := st.PutRAGEvalCase(ctx, caseRecord); err != nil {
		t.Fatal(err)
	}
	cases, err := st.ListRAGEvalCases(ctx, version.ID, "", 10)
	if err != nil || len(cases) != 1 || cases[0].ExternalID != "case-1" {
		t.Fatalf("cases=%+v err=%v", cases, err)
	}
	profile := &RAGEvalProfileRecord{ID: "profile-immutable", Name: "baseline", ProfileJSON: `{}`, Fingerprint: strings.Repeat("b", 64), CreatedBy: "admin"}
	if err := st.CreateRAGEvalProfile(ctx, profile); err != nil {
		t.Fatal(err)
	}
	profile.Name = "mutated"
	if err := st.CreateRAGEvalProfile(ctx, profile); err == nil {
		t.Fatal("immutable profile id was overwritten")
	}
	ok, err := st.TransitionRAGEvalDatasetVersion(ctx, version.ID, RAGEvalDatasetDraft, RAGEvalDatasetValidating, "{}")
	if err != nil || !ok {
		t.Fatalf("validate transition=%v %v", ok, err)
	}
	ok, err = st.TransitionRAGEvalDatasetVersion(ctx, version.ID, RAGEvalDatasetValidating, RAGEvalDatasetReady, "{}")
	if err != nil || !ok {
		t.Fatalf("ready transition=%v %v", ok, err)
	}
	if _, err = st.TransitionRAGEvalDatasetVersion(ctx, version.ID, RAGEvalDatasetReady, RAGEvalDatasetDraft, "{}"); err == nil {
		t.Fatal("READY version was mutable")
	}
	if err := st.PutRAGEvalCorpusDocument(ctx, document); !errors.Is(err, ErrRAGEvalImmutable) {
		t.Fatalf("READY corpus document mutation error = %v", err)
	}
	if err := st.PutRAGEvalCase(ctx, caseRecord); !errors.Is(err, ErrRAGEvalImmutable) {
		t.Fatalf("READY case mutation error = %v", err)
	}
	gotVersion, err := st.GetRAGEvalDatasetVersion(ctx, version.ID)
	if err != nil || gotVersion.Status != RAGEvalDatasetReady || !gotVersion.ReadyAt.Valid {
		t.Fatalf("version=%+v err=%v", gotVersion, err)
	}
	if changed, err := st.TombstoneRAGEvalDataset(ctx, dataset.ID); err != nil || !changed {
		t.Fatalf("tombstone dataset=%v %v", changed, err)
	}
	items, err = st.ListRAGEvalDatasets(ctx, "", 10)
	if err != nil || len(items) != 0 {
		t.Fatalf("tombstoned dataset still listed: %+v err=%v", items, err)
	}
}

func TestRAGEvalRunFenceAndUsageIdempotency(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	run := &RAGEvalRunRecord{DatasetVersionID: "dv", ProfileID: "p", Mode: RAGEvalRunModeOnlineOnly, CreatedBy: "admin"}
	if err := st.CreateRAGEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	fence, ok, err := st.ClaimRAGEvalRun(ctx, run.ID, "worker-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim=%+v %v %v", fence, ok, err)
	}
	stale := *fence
	stale.FenceToken--
	if ok, err := st.PutRAGEvalMetricResult(ctx, stale, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "mrr", MetricVersion: "v1", Status: RAGEvalMetricOK, Value: sql.NullFloat64{Float64: 1, Valid: true}}); err != nil || ok {
		t.Fatalf("stale write=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalMetricResult(ctx, *fence, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "mrr", MetricVersion: "v1", Status: RAGEvalMetricOK, Value: sql.NullFloat64{Float64: 1, Valid: true}}); err != nil || !ok {
		t.Fatalf("metric write=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalMetricResult(ctx, *fence, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "mrr", MetricVersion: "v1", Status: RAGEvalMetricOK, Value: sql.NullFloat64{Float64: .5, Valid: true}}); err != nil || !ok {
		t.Fatalf("metric idempotent update=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalMetricResult(ctx, *fence, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "mrr", MetricVersion: "v1", Status: RAGEvalMetricOK, Value: sql.NullFloat64{Float64: .5, Valid: true}}); err != nil || !ok {
		t.Fatalf("metric identical retry=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalMetricResult(ctx, *fence, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "invalid", MetricVersion: "v1", Status: "made_up"}); err == nil || ok {
		t.Fatalf("unknown metric status=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalCaseResult(ctx, *fence, RAGEvalCaseResultRecord{RunID: run.ID, CaseID: "c", Status: RAGEvalCaseOK, Response: "first"}); err != nil || !ok {
		t.Fatalf("case write=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalCaseResult(ctx, *fence, RAGEvalCaseResultRecord{RunID: run.ID, CaseID: "c", Status: RAGEvalCaseError, Response: "second", ErrorCode: strings.Repeat("x", 200), ErrorMessage: strings.Repeat("坏", 3000)}); err != nil || !ok {
		t.Fatalf("case idempotent update=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalCaseResult(ctx, *fence, RAGEvalCaseResultRecord{RunID: run.ID, CaseID: "c", Status: RAGEvalCaseError, Response: "second", ErrorCode: strings.Repeat("x", 200), ErrorMessage: strings.Repeat("坏", 3000)}); err != nil || !ok {
		t.Fatalf("case identical retry=%v %v", ok, err)
	}
	var caseCount, metricCount int
	var response, caseErrorCode, caseErrorMessage string
	var metricValue float64
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*),MAX(response),MAX(error_code),MAX(error_message) FROM rag_eval_case_results WHERE run_id=? AND case_id=?`, run.ID, "c").Scan(&caseCount, &response, &caseErrorCode, &caseErrorMessage); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*),MAX(value) FROM rag_eval_metric_results WHERE run_id=? AND case_id=? AND metric_name=? AND metric_version=?`, run.ID, "c", "mrr", "v1").Scan(&metricCount, &metricValue); err != nil {
		t.Fatal(err)
	}
	if caseCount != 1 || response != "second" || len(caseErrorCode) > 128 || len(caseErrorMessage) > 2048 || metricCount != 1 || metricValue != .5 {
		t.Fatalf("idempotent results: cases=%d response=%q code=%d message=%d metrics=%d value=%v", caseCount, response, len(caseErrorCode), len(caseErrorMessage), metricCount, metricValue)
	}
	usage := &RAGEvalUsageRecord{RunID: run.ID, CaseID: "c", Stage: "judge", IdempotencyKey: "same"}
	if ok, err := st.RecordRAGEvalUsage(ctx, usage); err != nil || !ok {
		t.Fatalf("first usage=%v %v", ok, err)
	}
	usage.ID = ""
	if ok, err := st.RecordRAGEvalUsage(ctx, usage); err != nil || ok {
		t.Fatalf("duplicate usage=%v %v", ok, err)
	}

	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_eval_runs SET lease_until=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), run.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.HeartbeatRAGEvalRun(ctx, *fence, time.Minute); err != nil || ok {
		t.Fatalf("expired heartbeat=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalCaseResult(ctx, *fence, RAGEvalCaseResultRecord{RunID: run.ID, CaseID: "late", Status: RAGEvalCaseOK}); err != nil || ok {
		t.Fatalf("expired case write=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalMetricResult(ctx, *fence, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "late", MetricName: "mrr", MetricVersion: "v1", Status: RAGEvalMetricOK, Value: sql.NullFloat64{Float64: 1, Valid: true}}); err != nil || ok {
		t.Fatalf("expired metric write=%v %v", ok, err)
	}
	if ok, err := st.FinishRAGEvalRun(ctx, *fence, RAGEvalRunSucceeded, "", ""); err != nil || ok {
		t.Fatalf("expired finish=%v %v", ok, err)
	}
	nextFence, ok, err := st.ClaimRAGEvalRun(ctx, run.ID, "worker-b", time.Minute)
	if err != nil || !ok || nextFence.FenceToken <= fence.FenceToken {
		t.Fatalf("reclaim=%+v %v %v", nextFence, ok, err)
	}
	if ok, err := st.FinishRAGEvalRun(ctx, *nextFence, RAGEvalRunSucceeded, strings.Repeat("e", 200), strings.Repeat("坏", 3000)); err != nil || !ok {
		t.Fatalf("finish=%v %v", ok, err)
	}
	finished, err := st.GetRAGEvalRun(ctx, run.ID)
	if err != nil || len(finished.ErrorCode) > 128 || len(finished.ErrorMessage) > 2048 {
		t.Fatalf("sanitized finish=%+v %v", finished, err)
	}
	if changed, err := st.TombstoneRAGEvalRun(ctx, run.ID); err != nil || !changed {
		t.Fatalf("tombstone run=%v %v", changed, err)
	}
}

func TestRAGEvalStoredStatusScanIsClosed(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	run := &RAGEvalRunRecord{DatasetVersionID: "dv", ProfileID: "p", Mode: RAGEvalRunModeOnlineOnly, CreatedBy: "admin"}
	if err := st.CreateRAGEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_eval_runs SET status='UNKNOWN' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRAGEvalRun(ctx, run.ID); err == nil || !strings.Contains(err.Error(), "invalid stored") {
		t.Fatalf("unknown stored status scan error = %v", err)
	}
}

func TestRAGPolicyCASAndGenerationActivation(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	for version := int64(1); version <= 2; version++ {
		if err := st.CreateRAGPolicy(ctx, &RAGPolicyRecord{Kind: RAGPolicyRuntime, Version: version, PolicyJSON: "{}", Fingerprint: "f", CreatedBy: "admin"}); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 0, 1, "admin", "", "first", "PUBLISH"); err != nil || !ok {
		t.Fatalf("publish1=%v %v", ok, err)
	}
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 0, 2, "admin", "", "stale", "PUBLISH"); err != nil || ok {
		t.Fatalf("stale CAS=%v %v", ok, err)
	}
	if ok, err := st.ActivateRAGPolicy(ctx, RAGPolicyRuntime, 1, 2, "admin", "", "next", "PUBLISH"); err != nil || !ok {
		t.Fatalf("publish2=%v %v", ok, err)
	}
	active, err := st.ActiveRAGPolicy(ctx, RAGPolicyRuntime)
	if err != nil || active.Version != 2 {
		t.Fatalf("active=%+v %v", active, err)
	}
}
