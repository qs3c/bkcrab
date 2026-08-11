package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedReadyEvalVersionAndRun(t *testing.T, st *DBStore, suffix string) (*RAGEvalDatasetVersionRecord, *RAGEvalRunRecord) {
	t.Helper()
	ctx := context.Background()
	dataset := &RAGEvalDatasetRecord{Name: "generation-" + suffix, CreatedBy: "admin"}
	if err := st.CreateRAGEvalDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	version := &RAGEvalDatasetVersionRecord{DatasetID: dataset.ID, Version: 1, SourceType: "canonical-json", CorpusSHA256: string(make([]byte, 64)), CreatedBy: "admin"}
	if err := st.CreateRAGEvalDatasetVersion(ctx, version); err != nil {
		t.Fatal(err)
	}
	if changed, err := st.TransitionRAGEvalDatasetVersion(ctx, version.ID, RAGEvalDatasetDraft, RAGEvalDatasetValidating, "{}"); err != nil || !changed {
		t.Fatalf("validate=%v err=%v", changed, err)
	}
	if changed, err := st.TransitionRAGEvalDatasetVersion(ctx, version.ID, RAGEvalDatasetValidating, RAGEvalDatasetReady, "{}"); err != nil || !changed {
		t.Fatalf("ready=%v err=%v", changed, err)
	}
	run := &RAGEvalRunRecord{DatasetVersionID: version.ID, ProfileID: "profile", Mode: RAGEvalRunModeFullPipeline, CreatedBy: "admin"}
	if err := st.CreateRAGEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	return version, run
}

func evalGenerationAcquireRequest(runID, versionID, fingerprint, generationID, worker string) RAGEvalGenerationAcquireRequest {
	return RAGEvalGenerationAcquireRequest{
		RunID: runID, DatasetVersionID: versionID, Fingerprint: fingerprint,
		CorpusFingerprint: "corpus", IngestionFingerprint: "ingestion", NewGenerationID: generationID,
		CollectionKey: "eval_1234567890abcdef_g_" + generationID, ObjectPrefix: "rag-eval/generations/" + generationID,
		EmbeddingModel: "embed", EmbeddingDims: 3, Worker: worker, Lease: time.Minute, TTL: time.Hour,
	}
}

func TestRAGEvalGenerationSQLSingleflightReuseRefAndFencedGC(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	version, firstRun := seedReadyEvalVersionAndRun(t, st, "first")
	_, secondRun := seedReadyEvalVersionAndRun(t, st, "second")
	// Both runs must point at the same dataset version for fingerprint reuse.
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_eval_runs SET dataset_version_id=? WHERE id=?`, version.ID, secondRun.ID); err != nil {
		t.Fatal(err)
	}
	first, err := st.AcquireRAGEvalGenerationForRun(ctx, evalGenerationAcquireRequest(firstRun.ID, version.ID, "fingerprint-a", "reg_one", "worker-a"))
	if err != nil || !first.Claimed || first.Reused || first.Fence == nil {
		t.Fatalf("first acquire=%+v err=%v", first, err)
	}
	second, err := st.AcquireRAGEvalGenerationForRun(ctx, evalGenerationAcquireRequest(secondRun.ID, version.ID, "fingerprint-a", "reg_unused", "worker-b"))
	if err != nil || second.Claimed || second.Reused || second.Generation.ID != first.Generation.ID {
		t.Fatalf("singleflight acquire=%+v err=%v", second, err)
	}
	if ready, err := st.MarkRAGEvalGenerationReady(ctx, *first.Fence, 2, 7, time.Hour); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if released, err := st.ReleaseRAGEvalGenerationForRun(ctx, firstRun.ID); err != nil || !released {
		t.Fatalf("release first=%v err=%v", released, err)
	}
	if released, err := st.ReleaseRAGEvalGenerationForRun(ctx, secondRun.ID); err != nil || !released {
		t.Fatalf("release second=%v err=%v", released, err)
	}
	if released, err := st.ReleaseRAGEvalGenerationForRun(ctx, secondRun.ID); err != nil || released {
		t.Fatalf("idempotent release=%v err=%v", released, err)
	}
	_, thirdRun := seedReadyEvalVersionAndRun(t, st, "third")
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_eval_runs SET dataset_version_id=? WHERE id=?`, version.ID, thirdRun.ID); err != nil {
		t.Fatal(err)
	}
	third, err := st.AcquireRAGEvalGenerationForRun(ctx, evalGenerationAcquireRequest(thirdRun.ID, version.ID, "fingerprint-a", "reg_unused_2", "worker-c"))
	if err != nil || !third.Reused || third.Claimed || third.Generation.ID != first.Generation.ID {
		t.Fatalf("READY reuse=%+v err=%v", third, err)
	}
	if released, err := st.ReleaseRAGEvalGenerationForRun(ctx, thirdRun.ID); err != nil || !released {
		t.Fatalf("release third=%v err=%v", released, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_eval_runs SET deleted_at=? WHERE index_generation_id=?`, time.Now().UTC(), first.Generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_eval_index_generations SET expires_at=? WHERE id=?`, time.Now().Add(-time.Hour), first.Generation.ID); err != nil {
		t.Fatal(err)
	}
	gc, claimed, err := st.ClaimRAGEvalGenerationGC(ctx, time.Now(), "gc-worker", time.Minute)
	if err != nil || !claimed || gc.GenerationID != first.Generation.ID || gc.CollectionKey != first.Generation.CollectionKey {
		t.Fatalf("GC claim=%+v claimed=%v err=%v", gc, claimed, err)
	}
	stale := *gc
	stale.FenceToken--
	if finished, err := st.FinishRAGEvalGenerationGC(ctx, stale); err != nil || finished {
		t.Fatalf("stale GC finish=%v err=%v", finished, err)
	}
	if finished, err := st.FinishRAGEvalGenerationGC(ctx, *gc); err != nil || !finished {
		t.Fatalf("GC finish=%v err=%v", finished, err)
	}
	if _, err := st.GetRAGEvalGeneration(ctx, first.Generation.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("generation survived GC: %v", err)
	}
}

func TestRAGEvalGenerationFailureNeverBecomesReady(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	version, run := seedReadyEvalVersionAndRun(t, st, "failed")
	acquired, err := st.AcquireRAGEvalGenerationForRun(context.Background(), evalGenerationAcquireRequest(run.ID, version.ID, "fingerprint-failed", "reg_failed", "worker"))
	if err != nil || acquired.Fence == nil {
		t.Fatalf("acquire=%+v err=%v", acquired, err)
	}
	if failed, err := st.MarkRAGEvalGenerationFailed(context.Background(), *acquired.Fence, "pipeline_error", "secret-safe", time.Hour); err != nil || !failed {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	if ready, err := st.MarkRAGEvalGenerationReady(context.Background(), *acquired.Fence, 1, 1, time.Hour); err != nil || ready {
		t.Fatalf("FAILED generation became READY: ready=%v err=%v", ready, err)
	}
	got, err := st.GetRAGEvalGeneration(context.Background(), acquired.Generation.ID)
	if err != nil || got.Status != RAGEvalGenerationFailed {
		t.Fatalf("generation=%+v err=%v", got, err)
	}
}
