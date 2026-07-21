package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func tombstoneAndDeleteRAGKBForTest(ctx context.Context, st *DBStore, id string) error {
	if _, err := st.MarkRAGKBDeleting(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return st.DeleteRAGKB(ctx, id)
}

func tombstoneAndDeleteRAGDocumentForTest(ctx context.Context, st *DBStore, id string) error {
	if _, err := st.MarkRAGDocumentDeleting(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return st.DeleteRAGDocument(ctx, id)
}

func TestRAGKBCRUD(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_1", "active")

	kb := &RAGKBRecord{
		ID: "kb_test1", UserID: "u_1", Name: "产品手册",
		EmbedProvider: "system", EmbedModel: "text-embedding-v3", EmbedDims: 1024,
		ChunkSize: 512, ChunkOverlap: 64, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetRAGKB(ctx, kb.ID)
	if err != nil || got.Name != "产品手册" || got.EmbedDims != 1024 {
		t.Fatalf("get: %+v err=%v", got, err)
	}
	list, err := st.ListRAGKBsByUser(ctx, "u_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	got.Name = "新版产品手册"
	got.Status = "deleting"
	got.EmbedModel = "must-not-change"
	if err := st.UpdateRAGKB(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = st.GetRAGKB(ctx, kb.ID)
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "新版产品手册" || got.Status != "active" {
		t.Fatalf("mutable fields not updated: %+v", got)
	}
	if got.EmbedModel != "text-embedding-v3" {
		t.Fatalf("embedding snapshot changed through UpdateRAGKB: %q", got.EmbedModel)
	}

	if err := tombstoneAndDeleteRAGKBForTest(ctx, st, kb.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.GetRAGKB(ctx, kb.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestRAGDocumentAndTaskLifecycle(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()

	doc, taskID := seedRAGTaskDocument(t, st, "doc_1", 3)
	task, err := st.GetRAGIndexTask(ctx, taskID)
	if err != nil || task.DocID != doc.ID || task.Status != "PENDING" || task.MaxRetry != 3 {
		t.Fatalf("get task: %+v err=%v", task, err)
	}
	claim, err := st.ClaimRAGIndexTask(ctx, "worker-lifecycle", time.Minute)
	if err != nil || claim == nil || claim.Fence.TaskID != taskID {
		t.Fatalf("claim task: %+v, %v", claim, err)
	}
	pend, err := st.ListRunnableRAGIndexTasks(ctx)
	if err != nil || len(pend) != 1 || pend[0].DocID != doc.ID || pend[0].StartedAt == nil {
		t.Fatalf("runnable: %+v err=%v", pend, err)
	}
	if ok, err := st.FailRAGIndexTask(ctx, claim.Fence, "boom"); err != nil || !ok {
		t.Fatalf("fail task: changed=%v err=%v", ok, err)
	}
	pend, err = st.ListRunnableRAGIndexTasks(ctx)
	if err != nil || len(pend) != 0 {
		t.Fatalf("FAILED should not be runnable: %+v err=%v", pend, err)
	}
	task, err = st.GetRAGIndexTask(ctx, taskID)
	if err != nil || task.FinishedAt == nil || task.ErrorMsg != "boom" {
		t.Fatalf("finished task: %+v err=%v", task, err)
	}

	docs, err := st.ListRAGDocumentsByKB(ctx, doc.KBID)
	if err != nil || len(docs) != 1 || docs[0].Status != "FAILED" || docs[0].Version != 1 {
		t.Fatalf("list docs: %+v err=%v", docs, err)
	}

	if err := tombstoneAndDeleteRAGDocumentForTest(ctx, st, doc.ID); err != nil {
		t.Fatalf("delete doc: %v", err)
	}
	if _, err := st.GetRAGDocument(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted doc: want ErrNotFound, got %v", err)
	}
	if _, err := st.GetRAGIndexTask(ctx, taskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("document delete should cascade task, got %v", err)
	}
}

func TestRAGDocumentAndIndexTaskAtomicWrites(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_atomic", "active")
	if err := st.CreateRAGKB(ctx, &RAGKBRecord{
		ID: "kb_atomic", UserID: "u_atomic", Name: "atomic", EmbedProvider: "system",
		EmbedModel: "embed-v1", EmbedDims: 3, ChunkSize: 512, ChunkOverlap: 64,
		ParseMode: RAGParseModeStandard, Status: "active", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	doc := &RAGDocumentRecord{
		ID: "doc_atomic", KBID: "kb_atomic", FileName: "atomic.txt", FileType: "txt",
		FileSize: 12, ObjectKey: "rag/u/kb_atomic/doc_atomic/atomic.txt",
		Status: "PENDING", Version: 1, SourceSHA256: testRAGVersion("doc_atomic", 1).SourceSHA256,
	}
	taskV1, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, testRAGVersion(doc.ID, 1), 3)
	if err != nil {
		t.Fatalf("atomic create: %v", err)
	}
	created, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || created.Version != 1 {
		t.Fatalf("created document = %+v, err=%v", created, err)
	}
	createdTask, err := st.GetRAGIndexTask(ctx, taskV1)
	if err != nil || createdTask.DocID != doc.ID || createdTask.Status != "PENDING" {
		t.Fatalf("created task = %+v, err=%v", createdTask, err)
	}

	taskV2Record, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 1, testRAGVersion(doc.ID, 0))
	if err != nil {
		t.Fatalf("atomic update: %v", err)
	}
	taskV2 := taskV2Record.ID
	if taskV2 == taskV1 {
		t.Fatalf("reindex reused task id %d", taskV1)
	}
	updated, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || updated.Version != 2 {
		t.Fatalf("updated document = %+v, err=%v", updated, err)
	}
	updatedTask, err := st.GetRAGIndexTask(ctx, taskV2)
	if err != nil || updatedTask.DocID != doc.ID || updatedTask.Status != "PENDING" {
		t.Fatalf("updated task = %+v, err=%v", updatedTask, err)
	}
	if _, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 1, testRAGVersion(doc.ID, 0)); !errors.Is(err, ErrRAGDocumentVersionConflict) {
		t.Fatalf("stale advance err=%v, want version conflict", err)
	}

	// Force the second statement in each transaction to fail. SQLite triggers
	// make both rollback checks deterministic without changing production code.
	if _, err := st.db.ExecContext(ctx, `CREATE TRIGGER fail_atomic_rag_task
		BEFORE INSERT ON rag_index_tasks
		WHEN NEW.doc_id IN ('doc_atomic', 'doc_atomic_create_failure')
		BEGIN SELECT RAISE(ABORT, 'forced task insert failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	failedCreate := &RAGDocumentRecord{
		ID: "doc_atomic_create_failure", KBID: "kb_atomic", FileName: "failure.txt", FileType: "txt",
		ObjectKey: "rag/u/kb_atomic/doc_atomic_create_failure/failure.txt",
		Status:    "PENDING", Version: 1, SourceSHA256: testRAGVersion("doc_atomic_create_failure", 1).SourceSHA256,
	}
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, failedCreate,
		testRAGVersion(failedCreate.ID, 1), 3); err == nil {
		t.Fatal("atomic create unexpectedly succeeded when task insert failed")
	}
	if _, err := st.GetRAGDocument(ctx, failedCreate.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed atomic create left a document row: %v", err)
	}

	if _, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 2, testRAGVersion(doc.ID, 0)); err == nil {
		t.Fatal("atomic update unexpectedly succeeded when task insert failed")
	}
	rolledBack, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Version != 2 {
		t.Fatalf("failed atomic update persisted version %d, want rollback to 2", rolledBack.Version)
	}
}

func TestDeleteRAGKBCascadesRows(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_1", "active")

	kb := &RAGKBRecord{
		ID: "kb_cascade", UserID: "u_1", Name: "cascade",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 8,
		ChunkSize: 32, ChunkOverlap: 4, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatal(err)
	}
	doc := &RAGDocumentRecord{
		ID: "doc_cascade", KBID: kb.ID, FileName: "a.txt", FileType: "txt",
		ObjectKey: "rag/u_1/kb_cascade/doc_cascade/a.txt", Status: "PENDING", Version: 1,
		SourceSHA256: testRAGDocumentVersion("doc_cascade", 1).SourceSHA256,
	}
	taskID, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc,
		testRAGDocumentVersion(doc.ID, 1), 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := tombstoneAndDeleteRAGKBForTest(ctx, st, kb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRAGDocument(ctx, doc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("document survived KB delete: %v", err)
	}
	if _, err := st.GetRAGIndexTask(ctx, taskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("task survived KB delete: %v", err)
	}
}
