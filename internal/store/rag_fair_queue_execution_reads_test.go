package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedRAGExecutionReadCatalog(
	t *testing.T,
) (*DBStore, *RAGDocumentRecord, *RAGIndexClaim, string, string, string, string) {
	t.Helper()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_execution_reads", 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), "execution-read-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	ctx := context.Background()
	asset := &RAGAssetRecord{
		ID: "asset_execution_read", DocID: doc.ID,
		ContentSHA256: strings.Repeat("a", 64), SourceKind: "embedded_original",
		SourceMIME: "image/png", DisplayStatus: "unavailable", ByteSize: 1,
		Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatalf("upsert owned asset: %v", err)
	}
	attachment := &RAGAttachmentRecord{
		ID: "attachment_execution_read", DocID: doc.ID,
		ContentSHA256: strings.Repeat("b", 64), Kind: "source", FileName: "source.pdf",
		MIMEType:  "application/pdf",
		ObjectKey: "rag/u_claim/" + doc.KBID + "/" + doc.ID + "/attachments/source.pdf",
		ByteSize:  1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAttachment(ctx, attachment); err != nil {
		t.Fatalf("upsert owned attachment: %v", err)
	}

	foreignDoc, _ := seedRAGTaskDocument(t, st, "doc_execution_reads_foreign", 3)
	foreignAsset := &RAGAssetRecord{
		ID: "asset_execution_read_foreign", DocID: foreignDoc.ID,
		ContentSHA256: strings.Repeat("c", 64), SourceKind: "embedded_original",
		SourceMIME: "image/png", DisplayStatus: "unavailable", ByteSize: 1,
		Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAsset(ctx, foreignAsset); err != nil {
		t.Fatalf("upsert foreign asset: %v", err)
	}
	foreignAttachment := &RAGAttachmentRecord{
		ID: "attachment_execution_read_foreign", DocID: foreignDoc.ID,
		ContentSHA256: strings.Repeat("d", 64), Kind: "source", FileName: "foreign.pdf",
		MIMEType:  "application/pdf",
		ObjectKey: "rag/u_claim/" + foreignDoc.KBID + "/" + foreignDoc.ID + "/attachments/foreign.pdf",
		ByteSize:  1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAttachment(ctx, foreignAttachment); err != nil {
		t.Fatalf("upsert foreign attachment: %v", err)
	}
	return st, doc, claim, asset.ID, attachment.ID, foreignAsset.ID, foreignAttachment.ID
}

func TestRAGExecutionReadTxCoresStayWithinLockedDocument(t *testing.T) {
	st, doc, claim, assetID, attachmentID, foreignAssetID, foreignAttachmentID :=
		seedRAGExecutionReadCatalog(t)
	ctx := context.Background()

	changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			gotDoc, err := st.getRAGDocumentForIndexInTx(tx, locked)
			if err != nil || gotDoc.ID != doc.ID {
				t.Fatalf("document=%+v err=%v", gotDoc, err)
			}
			kb, err := st.getRAGKBForIndexInTx(ctx, tx, locked, doc.KBID)
			if err != nil || kb.ID != doc.KBID || kb.UserID != claim.Task.UserID {
				t.Fatalf("kb=%+v err=%v", kb, err)
			}
			version, err := st.getRAGDocumentVersionForIndexInTx(
				ctx, tx, locked, doc.ID, claim.Fence.DocVersion,
			)
			if err != nil || version.DocID != doc.ID || version.DocVersion != claim.Fence.DocVersion {
				t.Fatalf("version=%+v err=%v", version, err)
			}
			assets, err := st.listRAGAssetsByIDsForIndexInTx(ctx, tx, locked, []string{assetID})
			if err != nil || len(assets) != 1 || assets[0].ID != assetID {
				t.Fatalf("assets=%+v err=%v", assets, err)
			}
			attachments, err := st.listRAGAttachmentsByIDsForIndexInTx(
				ctx, tx, locked, []string{attachmentID},
			)
			if err != nil || len(attachments) != 1 || attachments[0].ID != attachmentID {
				t.Fatalf("attachments=%+v err=%v", attachments, err)
			}
			return true, nil
		})
	if err != nil || !changed {
		t.Fatalf("legal read transaction changed=%v err=%v", changed, err)
	}

	checks := []struct {
		name string
		fn   func(*sql.Tx, *ragLockedIndexFence) error
	}{
		{"wrong kb", func(tx *sql.Tx, locked *ragLockedIndexFence) error {
			_, err := st.getRAGKBForIndexInTx(ctx, tx, locked, "kb_other")
			return err
		}},
		{"wrong document version", func(tx *sql.Tx, locked *ragLockedIndexFence) error {
			_, err := st.getRAGDocumentVersionForIndexInTx(
				ctx, tx, locked, doc.ID, claim.Fence.DocVersion+1,
			)
			return err
		}},
		{"foreign asset", func(tx *sql.Tx, locked *ragLockedIndexFence) error {
			_, err := st.listRAGAssetsByIDsForIndexInTx(
				ctx, tx, locked, []string{assetID, foreignAssetID},
			)
			return err
		}},
		{"foreign attachment", func(tx *sql.Tx, locked *ragLockedIndexFence) error {
			_, err := st.listRAGAttachmentsByIDsForIndexInTx(
				ctx, tx, locked, []string{attachmentID, foreignAttachmentID},
			)
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
				func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
					if err := check.fn(tx, locked); err != nil {
						return false, err
					}
					return true, nil
				})
			if changed || !errors.Is(err, ErrRAGDocumentVersionMismatch) {
				t.Fatalf("out-of-scope read changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestRAGExecutionVersionReadAllowsOnlyCurrentOrActiveVersion(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_execution_active_version", 3)
	ctx := context.Background()
	first, err := st.ClaimRAGIndexTask(ctx, "execution-active-v1", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim v1=%+v err=%v", first, err)
	}
	if ok, err := st.ActivateAndFinishRAGIndexTask(ctx, first.Fence,
		RAGIndexActivation{VersionResult: RAGDocumentVersionResult{Status: RAGDocumentVersionDone}},
		time.Hour); err != nil || !ok {
		t.Fatalf("activate v1=%v err=%v", ok, err)
	}
	created, err := st.AdvanceDocumentVersionAndCreateTask(
		ctx, 1, testRAGVersion(doc.ID, 0),
	)
	if err != nil || created == nil || created.DocVersion != 2 {
		t.Fatalf("create v2 task=%+v err=%v", created, err)
	}
	second, err := st.ClaimRAGIndexTask(ctx, "execution-active-v2", time.Minute)
	if err != nil || second == nil || second.Fence.DocVersion != 2 {
		t.Fatalf("claim v2=%+v err=%v", second, err)
	}

	changed, err := st.withLiveRAGIndexFenceTx(ctx, second.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			active, err := st.getRAGDocumentVersionForIndexInTx(
				ctx, tx, locked, doc.ID, 1,
			)
			if err != nil || active.DocVersion != 1 || active.Status != RAGDocumentVersionDone {
				t.Fatalf("active version=%+v err=%v", active, err)
			}
			if _, err := st.getRAGDocumentVersionForIndexInTx(
				ctx, tx, locked, doc.ID, 3,
			); !errors.Is(err, ErrRAGDocumentVersionMismatch) {
				t.Fatalf("unrelated history error=%v", err)
			}
			return true, nil
		})
	if err != nil || !changed {
		t.Fatalf("active version read changed=%v err=%v", changed, err)
	}
}

func TestRAGExecutionReadsRejectMissingWriterAndStaleFenceWithoutSnapshots(t *testing.T) {
	st, doc, claim, assetID, attachmentID, _, _ := seedRAGExecutionReadCatalog(t)
	ctx := context.Background()

	if got, err := st.GetRAGDocumentForIndex(ctx, claim.Fence); got != nil ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("document without writer=%+v err=%v", got, err)
	}
	if got, err := st.GetRAGKBForIndex(ctx, claim.Fence, doc.KBID); got != nil ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("kb without writer=%+v err=%v", got, err)
	}
	if got, err := st.GetRAGDocumentVersionForIndex(
		ctx, claim.Fence, doc.ID, claim.Fence.DocVersion,
	); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("version without writer=%+v err=%v", got, err)
	}
	if got, err := st.ListRAGAssetsByIDsForIndex(ctx, claim.Fence, []string{assetID}); got != nil ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("assets without writer=%+v err=%v", got, err)
	}
	if got, err := st.ListRAGAttachmentsByIDsForIndex(
		ctx, claim.Fence, []string{attachmentID},
	); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("attachments without writer=%+v err=%v", got, err)
	}

	if _, err := st.db.ExecContext(ctx, `UPDATE rag_index_tasks SET lease_until=? WHERE id=?`,
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), claim.Fence.TaskID); err != nil {
		t.Fatal(err)
	}
	called := false
	var snapshot *RAGDocumentRecord
	changed, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			called = true
			var readErr error
			snapshot, readErr = st.getRAGDocumentForIndexInTx(tx, locked)
			return snapshot != nil, readErr
		})
	if err != nil || changed || called || snapshot != nil {
		t.Fatalf("stale read changed=%v called=%v snapshot=%+v err=%v",
			changed, called, snapshot, err)
	}
}

func TestRAGFairQueueStoreTaskReadRejectsMissingWriterWithoutSnapshot(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, taskID := seedRAGTaskDocument(t, st, "doc_execution_task_read", 3)
	facade := &RAGFairQueueStore{store: st}
	task, err := facade.GetRAGIndexTask(context.Background(), taskID)
	if task != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("task without writer=%+v err=%v", task, err)
	}
}
