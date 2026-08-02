package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func claimRAGExecutionFixture(t *testing.T, docID string) (*DBStore, *RAGDocumentRecord, *RAGIndexClaim) {
	t.Helper()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, docID, 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), "execution-fence-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	return st, doc, claim
}

func TestRAGLiveIndexFenceTxPreservesLegacyEmptyWriter(t *testing.T) {
	st, doc, claim := claimRAGExecutionFixture(t, "doc_execution_live_tx")
	ctx := context.Background()

	changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked == nil || locked.doc.ID != doc.ID || locked.task.ID != claim.Fence.TaskID {
				t.Fatalf("locked fence=%+v", locked)
			}
			result, err := tx.ExecContext(ctx, `UPDATE rag_documents SET
				processing_stage='legacy-live-tx' WHERE id=? AND version=?`,
				claim.Fence.DocID, claim.Fence.DocVersion)
			if err != nil {
				return false, err
			}
			return ragRowsAffected(result)
		})
	if err != nil || !changed {
		t.Fatalf("legacy live tx changed=%v err=%v", changed, err)
	}
	current, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || current.ProcessingStage != "legacy-live-tx" {
		t.Fatalf("document after commit=%+v err=%v", current, err)
	}

	changed, err = st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
		func(tx *sql.Tx, _ *ragLockedIndexFence) (bool, error) {
			if _, err := tx.ExecContext(ctx, `UPDATE rag_documents SET
				processing_stage='must-rollback' WHERE id=?`, doc.ID); err != nil {
				return false, err
			}
			return false, nil
		})
	if err != nil || changed {
		t.Fatalf("rollback live tx changed=%v err=%v", changed, err)
	}
	current, err = st.GetRAGDocument(ctx, doc.ID)
	if err != nil || current.ProcessingStage != "legacy-live-tx" {
		t.Fatalf("false callback committed mutation: %+v err=%v", current, err)
	}
}

func TestRAGIndexCatalogBatchCoreIsAtomicUnderLiveFence(t *testing.T) {
	st, doc, claim := claimRAGExecutionFixture(t, "doc_execution_catalog")
	ctx := context.Background()
	chunks := []RAGChunkRecord{
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: claim.Fence.DocVersion, ChunkIndex: 0,
			RawContent: "zero", SearchContent: "zero", TokenCount: 1},
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: claim.Fence.DocVersion, ChunkIndex: 1,
			RawContent: "one", SearchContent: "one", TokenCount: 1},
	}

	t.Run("mixed chunk batch rolls back", func(t *testing.T) {
		mixed := append([]RAGChunkRecord(nil), chunks...)
		mixed[1].DocID = "another-document"
		changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				if err := st.putRAGChunksForIndexInTx(ctx, tx, locked, claim.Fence, mixed); err != nil {
					return false, err
				}
				return true, nil
			})
		if changed || !errors.Is(err, ErrRAGDocumentVersionMismatch) {
			t.Fatalf("mixed chunks changed=%v err=%v", changed, err)
		}
		stored, listErr := st.ListRAGChunksByDocumentVersion(ctx, doc.ID, claim.Fence.DocVersion)
		if listErr != nil || len(stored) != 0 {
			t.Fatalf("mixed batch stored=%+v err=%v", stored, listErr)
		}
	})

	changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if err := st.putRAGChunksForIndexInTx(ctx, tx, locked, claim.Fence, chunks); err != nil {
				return false, err
			}
			return true, nil
		})
	if err != nil || !changed {
		t.Fatalf("valid chunks changed=%v err=%v", changed, err)
	}

	t.Run("mixed chunk asset batch rolls back", func(t *testing.T) {
		mappings := []RAGChunkAssetRecord{
			{DocID: doc.ID, DocVersion: claim.Fence.DocVersion, ChunkIndex: 0,
				AssetID: "asset-zero", Ordinal: 0},
			{DocID: doc.ID, DocVersion: claim.Fence.DocVersion + 1, ChunkIndex: 1,
				AssetID: "asset-one", Ordinal: 0},
		}
		changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				if err := st.putRAGChunkAssetsForIndexInTx(ctx, tx, locked, claim.Fence, mappings); err != nil {
					return false, err
				}
				return true, nil
			})
		if changed || !errors.Is(err, ErrRAGDocumentVersionMismatch) {
			t.Fatalf("mixed mappings changed=%v err=%v", changed, err)
		}
		stored, listErr := st.ListRAGChunkAssetsByRefs(ctx, []RAGChunkRef{
			{DocID: doc.ID, DocVersion: claim.Fence.DocVersion, ChunkIndex: 0},
		})
		if listErr != nil || len(stored) != 0 {
			t.Fatalf("mixed mapping batch stored=%+v err=%v", stored, listErr)
		}
	})
}

func TestRAGFairIndexCatalogEntryPointsRequireWriterFingerprint(t *testing.T) {
	st, doc, claim := claimRAGExecutionFixture(t, "doc_execution_requires_writer")
	ctx := context.Background()
	chunk := []RAGChunkRecord{{
		KBID: doc.KBID, DocID: doc.ID, DocVersion: claim.Fence.DocVersion,
		ChunkIndex: 0, RawContent: "content", SearchContent: "content", TokenCount: 1,
	}}
	if changed, err := st.PutRAGChunksForIndex(ctx, claim.Fence, chunk); changed ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("chunks without writer changed=%v err=%v", changed, err)
	}
	if changed, err := st.PutRAGChunkAssetsForIndex(ctx, claim.Fence, nil); changed ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("chunk assets without writer changed=%v err=%v", changed, err)
	}
}

func TestRAGExecutionFenceRejectsMalformedWriterWithoutLegacyFallback(t *testing.T) {
	st, _, claim := claimRAGExecutionFixture(t, "doc_execution_malformed_writer")
	ctx := context.Background()
	malformed := claim.Fence
	malformed.ExpectedWriterFingerprint = "not-a-writer-fingerprint"

	checks := []struct {
		name string
		fn   func() (bool, error)
	}{
		{"check", func() (bool, error) { return st.CheckRAGIndexFence(ctx, malformed) }},
		{"progress", func() (bool, error) {
			return st.UpdateProgressRAGIndexTask(ctx, malformed, RAGIndexProgress{Stage: "bad-writer"})
		}},
		{"acknowledge", func() (bool, error) { return st.AcknowledgeRAGIndexTaskQuiesced(ctx, malformed) }},
	}
	for _, check := range checks {
		if changed, err := check.fn(); changed || !errors.Is(err, ErrFairQueueWriterMismatch) {
			t.Errorf("%s malformed writer changed=%v err=%v", check.name, changed, err)
		}
	}
}

func TestRAGFairExecutionFenceRequiresCurrentProcessingDocumentAndCanonicalUser(t *testing.T) {
	st, doc, claim := claimRAGExecutionFixture(t, "doc_execution_strict_state")
	ctx := context.Background()
	fence := claim.Fence
	fence.ExpectedWriterFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	route, err := st.ragOwnershipRoute(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}

	assertValidity := func(want bool) {
		t.Helper()
		valid, err := st.checkRAGIndexFenceOn(ctx, st.db, fence, true)
		if err != nil || valid != want {
			t.Fatalf("strict check valid=%v want=%v err=%v", valid, want, err)
		}
		tx, err := st.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		_, valid, err = st.lockRAGIndexFence(ctx, tx, fence, route)
		if err != nil || valid != want {
			t.Fatalf("strict lock valid=%v want=%v err=%v", valid, want, err)
		}
	}

	assertValidity(true)
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_documents SET status='READY' WHERE id=?`, doc.ID); err != nil {
		t.Fatal(err)
	}
	assertValidity(false)
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_documents SET status='PROCESSING',version=version+1 WHERE id=?`, doc.ID); err != nil {
		t.Fatal(err)
	}
	assertValidity(false)
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_documents SET version=? WHERE id=?`, fence.DocVersion, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_index_tasks SET user_id='' WHERE id=?`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	assertValidity(false)
}
