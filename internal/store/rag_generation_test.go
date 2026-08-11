package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRAGGenerationSyncFenceActivationRollbackAndCleanup(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_generation", "active")
	kb := &RAGKBRecord{
		ID: "kb_generation", UserID: "u_generation", Name: "generation",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatal(err)
	}

	gen1, task1 := createRAGGenerationSyncFixture(t, st, kb.ID, 1, "one", []string{"doc-a", "doc-b"})
	if err := st.CreateRAGPolicySyncTask(ctx, &RAGPolicySyncTaskRecord{KBID: kb.ID, TargetGenerationID: gen1.ID, TargetPolicyVersion: 1, RequestedBy: "admin"}); err == nil {
		t.Fatal("second active sync task for one KB was accepted")
	}
	fence1 := claimRAGPolicySyncFixture(t, st, task1.ID, "worker-1")
	for _, docID := range []string{"doc-a", "doc-b"} {
		if ok, err := st.UpdateRAGGenerationDocument(ctx, fence1, docID, RAGGenerationDocumentReady, "", ""); err != nil || !ok {
			t.Fatalf("ready %s=%v %v", docID, ok, err)
		}
	}
	if ok, err := st.MarkRAGKBGenerationReady(ctx, fence1, 2, 4); err != nil || !ok {
		t.Fatalf("mark generation one ready=%v %v", ok, err)
	}
	if ok, err := st.ActivateRAGKBGeneration(ctx, fence1, "", "admin", "initial", time.Hour); err != nil || !ok {
		t.Fatalf("activate generation one=%v %v", ok, err)
	}
	assertActiveRAGGeneration(t, st, kb.ID, gen1.ID, 1)
	resolved, documents, err := st.ResolveActiveRAGKBGeneration(ctx, kb.ID)
	if err != nil || resolved.ID != gen1.ID || len(documents) != 2 {
		t.Fatalf("resolved generation=%+v documents=%+v err=%v", resolved, documents, err)
	}

	gen2, task2 := createRAGGenerationSyncFixture(t, st, kb.ID, 2, "two", []string{"doc-a", "doc-b"})
	staleFence := claimRAGPolicySyncFixture(t, st, task2.ID, "worker-old")
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_policy_sync_tasks SET lease_until=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), task2.ID); err != nil {
		t.Fatal(err)
	}
	currentFence := claimRAGPolicySyncFixture(t, st, task2.ID, "worker-new")
	if currentFence.FenceToken <= staleFence.FenceToken {
		t.Fatalf("reclaim fence token=%d, stale=%d", currentFence.FenceToken, staleFence.FenceToken)
	}
	if ok, err := st.UpdateRAGGenerationDocument(ctx, staleFence, "doc-a", RAGGenerationDocumentReady, "", ""); err != nil || ok {
		t.Fatalf("stale document write=%v %v", ok, err)
	}
	for _, docID := range []string{"doc-a", "doc-b"} {
		if ok, err := st.UpdateRAGGenerationDocument(ctx, currentFence, docID, RAGGenerationDocumentReady, "", ""); err != nil || !ok {
			t.Fatalf("current ready %s=%v %v", docID, ok, err)
		}
	}
	if ok, err := st.MarkRAGKBGenerationReady(ctx, staleFence, 2, 4); err != nil || ok {
		t.Fatalf("stale mark ready=%v %v", ok, err)
	}
	if ok, err := st.MarkRAGKBGenerationReady(ctx, currentFence, 2, 4); err != nil || !ok {
		t.Fatalf("current mark ready=%v %v", ok, err)
	}
	if ok, err := st.ActivateRAGKBGeneration(ctx, staleFence, gen1.ID, "admin", "stale", time.Hour); err != nil || ok {
		t.Fatalf("stale activation=%v %v", ok, err)
	}
	if ok, err := st.ActivateRAGKBGeneration(ctx, currentFence, gen1.ID, "admin", "upgrade", time.Hour); err != nil || !ok {
		t.Fatalf("activate generation two=%v %v", ok, err)
	}
	assertActiveRAGGeneration(t, st, kb.ID, gen2.ID, 2)
	if ok, err := st.RollbackRAGKBGeneration(ctx, kb.ID, gen1.ID, gen2.ID, "admin", "rollback", time.Hour); err != nil || !ok {
		t.Fatalf("rollback generation one=%v %v", ok, err)
	}
	assertActiveRAGGeneration(t, st, kb.ID, gen1.ID, 1)

	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_generation_documents SET status=? WHERE generation_id=? AND doc_id=?`, RAGGenerationDocumentFailed, gen2.ID, "doc-a"); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RollbackRAGKBGeneration(ctx, kb.ID, gen2.ID, gen1.ID, "admin", "incomplete", time.Hour); err != nil || ok {
		t.Fatalf("incomplete rollback=%v %v", ok, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_generation_documents SET status=? WHERE generation_id=?`, RAGGenerationDocumentReady, gen2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_index_generations SET rollback_until=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), gen2.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.RollbackRAGKBGeneration(ctx, kb.ID, gen2.ID, gen1.ID, "admin", "expired", time.Hour); err != nil || ok {
		t.Fatalf("expired rollback=%v %v", ok, err)
	}

	_, task3 := createRAGGenerationSyncFixture(t, st, kb.ID, 3, "three", nil)
	fence3 := claimRAGPolicySyncFixture(t, st, task3.ID, "worker-cancel")
	if ok, err := st.RequestCancelRAGPolicySyncTask(ctx, task3.ID); err != nil || !ok {
		t.Fatalf("request cancel=%v %v", ok, err)
	}
	if ok, err := st.FinishRAGPolicySyncTask(ctx, fence3, RAGPolicySyncCancelled, "cancelled", "cancelled by admin"); err != nil || !ok {
		t.Fatalf("finish cancelled=%v %v", ok, err)
	}
	assertActiveRAGGeneration(t, st, kb.ID, gen1.ID, 1)

	gen4, task4 := createRAGGenerationSyncFixture(t, st, kb.ID, 4, "four", nil)
	fence4 := claimRAGPolicySyncFixture(t, st, task4.ID, "worker-fail")
	if ok, err := st.MarkRAGKBGenerationFailed(ctx, fence4, "build_failed", "failed"); err != nil || !ok {
		t.Fatalf("mark generation failed=%v %v", ok, err)
	}
	if ok, err := st.FinishRAGPolicySyncTask(ctx, fence4, RAGPolicySyncFailed, "build_failed", "failed"); err != nil || !ok {
		t.Fatalf("finish failed=%v %v", ok, err)
	}
	latestTask, err := st.LatestRAGPolicySyncTaskForKB(ctx, kb.ID)
	if err != nil || latestTask.ID != task4.ID || latestTask.Status != RAGPolicySyncFailed {
		t.Fatalf("latest policy sync task=%+v err=%v", latestTask, err)
	}
	assertActiveRAGGeneration(t, st, kb.ID, gen1.ID, 1)
	candidates, err := st.ListRAGKBGenerationGCCandidates(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	foundFailed := false
	for _, candidate := range candidates {
		if candidate.ID == gen4.ID {
			foundFailed = true
		}
	}
	if !foundFailed {
		t.Fatalf("failed generation missing from GC candidates: %+v", candidates)
	}
	if ok, err := st.DeleteRAGKBGenerationIfCollectible(ctx, gen4.ID); err != nil || !ok {
		t.Fatalf("collect failed generation=%v err=%v", ok, err)
	}
	if _, err := st.GetRAGKBGeneration(ctx, gen4.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed generation remained after GC: %v", err)
	}

	_, queuedTask := createRAGGenerationSyncFixture(t, st, kb.ID, 5, "five", nil)
	if ok, err := st.RequestCancelRAGPolicySyncTask(ctx, queuedTask.ID); err != nil || !ok {
		t.Fatalf("cancel queued=%v %v", ok, err)
	}
	if _, ok, err := st.ClaimRAGPolicySyncTask(ctx, queuedTask.ID, "worker", time.Minute); err != nil || ok {
		t.Fatalf("claim cancelled queued task=%v %v", ok, err)
	}

	if _, err := st.MarkRAGKBDeleting(ctx, kb.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteRAGKB(ctx, kb.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"rag_kb_generation_documents", "rag_kb_policy_sync_tasks", "rag_kb_index_generations"} {
		var count int
		if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s rows after KB cleanup=%d", table, count)
		}
	}
}

func TestRAGGenerationUserDeletionCleansPolicySyncChildren(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_generation_delete", "active")
	kb := &RAGKBRecord{
		ID: "kb_generation_delete", UserID: "u_generation_delete", Name: "delete",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatal(err)
	}
	_, _ = createRAGGenerationSyncFixture(t, st, kb.ID, 1, "user-delete", []string{"doc-a"})
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO rag_policy_audit_log(id,policy_kind,from_version,to_version,action,actor_id,source_eval_run_id,target_kb_id,note,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, "audit-user-delete", RAGPolicyIngestion, 0, 1, RAGPolicyAuditKBSync, kb.UserID, nil, kb.ID, "cleanup", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, kb.UserID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"rag_kb_generation_documents", "rag_kb_policy_sync_tasks", "rag_kb_index_generations", "rag_policy_audit_log"} {
		var count int
		if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s rows after user cleanup=%d", table, count)
		}
	}
}

func createRAGGenerationSyncFixture(t *testing.T, st *DBStore, kbID string, policyVersion int64, suffix string, docIDs []string) (*RAGKBGenerationRecord, *RAGPolicySyncTaskRecord) {
	t.Helper()
	documents := make([]RAGGenerationDocumentRecord, 0, len(docIDs))
	for index, docID := range docIDs {
		documents = append(documents, RAGGenerationDocumentRecord{DocID: docID, DocVersion: int64(index + 1)})
	}
	generation := &RAGKBGenerationRecord{ID: "gen_" + suffix, KBID: kbID, PolicyVersion: policyVersion, CollectionKey: "collection_" + suffix, EmbeddingModel: "embed-v1", EmbeddingDims: 3, CreatedBy: "admin"}
	if err := st.CreateRAGKBGeneration(context.Background(), generation, documents); err != nil {
		t.Fatal(err)
	}
	task := &RAGPolicySyncTaskRecord{ID: "sync_" + suffix, KBID: kbID, TargetGenerationID: generation.ID, TargetPolicyVersion: policyVersion, RequestedBy: "admin"}
	if err := st.CreateRAGPolicySyncTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return generation, task
}

func claimRAGPolicySyncFixture(t *testing.T, st *DBStore, taskID, worker string) RAGPolicySyncFence {
	t.Helper()
	fence, ok, err := st.ClaimRAGPolicySyncTask(context.Background(), taskID, worker, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim %s=%+v %v %v", taskID, fence, ok, err)
	}
	return *fence
}

func assertActiveRAGGeneration(t *testing.T, st *DBStore, kbID, wantGenerationID string, wantPolicyVersion int64) {
	t.Helper()
	ctx := context.Background()
	var count int
	var activeID string
	var pinned int64
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_kb_index_generations WHERE kb_id=? AND status=?`, kbID, RAGGenerationActive).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT active_generation_id,pinned_policy_version FROM rag_kbs WHERE id=?`, kbID).Scan(&activeID, &pinned); err != nil {
		t.Fatal(err)
	}
	if count != 1 || activeID != wantGenerationID || pinned != wantPolicyVersion {
		t.Fatalf("active generation count/id/policy=%d/%q/%d, want 1/%q/%d", count, activeID, pinned, wantGenerationID, wantPolicyVersion)
	}
}
