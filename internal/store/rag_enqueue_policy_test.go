package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func advancedEnqueueTestKB(t *testing.T, st *DBStore, kbID, userID string) {
	t.Helper()
	if err := st.CreateRAGKB(context.Background(), &RAGKBRecord{
		ID: kbID, UserID: userID, Name: "advanced gate", EmbedProvider: "system",
		EmbedModel: "embed-v1", EmbedDims: 3, ChunkSize: 512, ChunkOverlap: 64,
		ParseMode: RAGParseModeAuto, EnrichmentEnabled: true, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
}

func advancedEnqueueTestDocument(docID, kbID string) (*RAGDocumentRecord, *RAGDocumentVersionRecord) {
	version := testRAGVersion(docID, 1)
	version.ParseMode = RAGParseModeAuto
	doc := &RAGDocumentRecord{
		ID: docID, KBID: kbID, FileName: docID + ".pdf", FileType: "pdf",
		FileSize: 12, ObjectKey: "rag/u/" + kbID + "/" + docID + "/source.pdf",
		Status: "PENDING", Version: 1, SourceSHA256: version.SourceSHA256,
		IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: time.Now().UTC(),
	}
	return doc, version
}

func TestRAGAdvancedEnqueuePolicyLimitsPendingTasksAtomically(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	advancedEnqueueTestKB(t, st, "kb_advanced_pending", "u_advanced_pending")
	policy := RAGAdvancedEnqueuePolicy{UserID: "u_advanced_pending", MaxPendingTasks: 1, MinReindexInterval: time.Minute}

	first, firstVersion := advancedEnqueueTestDocument("doc_advanced_first", "kb_advanced_pending")
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, first, firstVersion, 3, policy); err != nil {
		t.Fatalf("first advanced enqueue: %v", err)
	}
	second, secondVersion := advancedEnqueueTestDocument("doc_advanced_second", "kb_advanced_pending")
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, second, secondVersion, 3, policy); !errors.Is(err, ErrRAGAdvancedPendingLimit) {
		t.Fatalf("second advanced enqueue error=%v", err)
	}
	if _, err := st.GetRAGDocument(ctx, second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected enqueue left a document row: %v", err)
	}

	standard, standardVersion := advancedEnqueueTestDocument("doc_standard_not_counted", "kb_advanced_pending")
	standard.FileType = "txt"
	standard.FileName = "standard.txt"
	standardVersion.ParseMode = RAGParseModeStandard
	standardVersion.EnrichmentEnabled = false
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, standard, standardVersion, 3, policy); err != nil {
		t.Fatalf("standard enqueue was incorrectly rate limited: %v", err)
	}
}

func TestRAGAdvancedEnqueuePolicyEnforcesMinimumReindexInterval(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	advancedEnqueueTestKB(t, st, "kb_advanced_reindex", "u_advanced_reindex")
	policy := RAGAdvancedEnqueuePolicy{UserID: "u_advanced_reindex", MaxPendingTasks: 10, MinReindexInterval: time.Hour}
	doc, version := advancedEnqueueTestDocument("doc_advanced_reindex", "kb_advanced_reindex")
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, version, 3, policy); err != nil {
		t.Fatal(err)
	}

	next := testRAGVersion(doc.ID, 0)
	next.ParseMode = RAGParseModeAuto
	if _, err := st.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, 1, next, policy); !errors.Is(err, ErrRAGAdvancedReindexRateLimit) {
		t.Fatalf("immediate reindex error=%v", err)
	}
	unchanged, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || unchanged.Version != 1 {
		t.Fatalf("rate-limited reindex mutated document: %+v err=%v", unchanged, err)
	}

	if _, err := st.db.ExecContext(ctx, `UPDATE rag_index_tasks SET created_at=? WHERE doc_id=?`, time.Now().UTC().Add(-2*time.Hour), doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, 1, next, policy); err != nil {
		t.Fatalf("reindex after interval: %v", err)
	}
}

func TestRAGAdvancedEnqueuePolicySerializesTwoSQLiteConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "advanced-enqueue.db")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	firstStore, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer firstStore.Close()
	secondStore, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	if err := firstStore.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	advancedEnqueueTestKB(t, firstStore, "kb_advanced_concurrent", "u_advanced_concurrent")
	policy := RAGAdvancedEnqueuePolicy{UserID: "u_advanced_concurrent", MaxPendingTasks: 1, MinReindexInterval: time.Minute}

	stores := []*DBStore{firstStore, secondStore}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	var wg sync.WaitGroup
	for index, st := range stores {
		index, st := index, st
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			doc, version := advancedEnqueueTestDocument("doc_advanced_concurrent_"+string(rune('a'+index)), "kb_advanced_concurrent")
			_, enqueueErr := st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(context.Background(), doc, version, 3, policy)
			results <- enqueueErr
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	succeeded, limited := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrRAGAdvancedPendingLimit):
			limited++
		default:
			t.Fatalf("unexpected concurrent enqueue error: %v", err)
		}
	}
	if succeeded != 1 || limited != 1 {
		t.Fatalf("concurrent outcomes success=%d limited=%d", succeeded, limited)
	}
}
