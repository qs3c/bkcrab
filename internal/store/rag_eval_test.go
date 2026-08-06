package store

import (
	"context"
	"database/sql"
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
}

func TestRAGEvalRunFenceAndUsageIdempotency(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	run := &RAGEvalRunRecord{DatasetVersionID: "dv", ProfileID: "p", Mode: "ONLINE_ONLY", CreatedBy: "admin"}
	if err := st.CreateRAGEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	fence, ok, err := st.ClaimRAGEvalRun(ctx, run.ID, "worker-a", time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim=%+v %v %v", fence, ok, err)
	}
	stale := *fence
	stale.FenceToken--
	if ok, err := st.PutRAGEvalMetricResult(ctx, stale, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "mrr", MetricVersion: "v1", Status: "ok", Value: sql.NullFloat64{Float64: 1, Valid: true}}); err != nil || ok {
		t.Fatalf("stale write=%v %v", ok, err)
	}
	if ok, err := st.PutRAGEvalMetricResult(ctx, *fence, RAGEvalMetricResultRecord{RunID: run.ID, CaseID: "c", MetricName: "mrr", MetricVersion: "v1", Status: "ok", Value: sql.NullFloat64{Float64: 1, Valid: true}}); err != nil || !ok {
		t.Fatalf("metric write=%v %v", ok, err)
	}
	usage := &RAGEvalUsageRecord{RunID: run.ID, CaseID: "c", Stage: "judge", IdempotencyKey: "same"}
	if ok, err := st.RecordRAGEvalUsage(ctx, usage); err != nil || !ok {
		t.Fatalf("first usage=%v %v", ok, err)
	}
	usage.ID = ""
	if ok, err := st.RecordRAGEvalUsage(ctx, usage); err != nil || ok {
		t.Fatalf("duplicate usage=%v %v", ok, err)
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
