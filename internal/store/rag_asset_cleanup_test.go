package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func ragAssetCleanupObjectWrite(t *testing.T, st *DBStore, objectKey string) *RAGObjectWriteFence {
	t.Helper()
	record, err := scanRAGObjectWrite(st.DB().QueryRowContext(context.Background(),
		`SELECT `+ragObjectWriteColumns+` FROM rag_object_write_staging WHERE handle_id=?`,
		ragObjectWriteHandleID(objectKey)))
	if err != nil {
		t.Fatalf("get cleanup object write %q: %v", objectKey, err)
	}
	return record
}

func ragAssetCleanupFixture(t *testing.T, name, assetID string, withDisplay bool) (
	*DBStore, *RAGDocumentRecord, *RAGAssetRecord, *RAGObjectWriteFence,
) {
	t.Helper()
	ctx := context.Background()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_asset_cleanup_"+name, 3)
	activateRAGLifecycleVersion(t, st, "asset-cleanup-index-"+name, 0)
	prefix := "rag/u_claim/" + doc.KBID + "/" + doc.ID + "/assets/" + strings.Repeat("1", 64)
	sourceKey := prefix + "/versions/1/source.png"
	sourceFence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindAssetSource, ObjectKey: sourceKey, ReferenceKey: assetID,
	})
	if err != nil {
		t.Fatalf("begin source object write: %v", err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *sourceFence); err != nil || !ready {
		t.Fatalf("mark source ready=%v err=%v", ready, err)
	}
	asset := &RAGAssetRecord{
		ID: assetID, DocID: doc.ID, ContentSHA256: strings.Repeat("1", 64),
		SourceKind: "embedded_original", SourceMIME: "image/png",
		SourceObjectKey: sourceKey, DisplayStatus: "unavailable",
		ByteSize: 1, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if withDisplay {
		asset.DisplayMIME = "image/webp"
		asset.DisplayObjectKey = prefix + "/versions/1/display.webp"
		asset.DisplayStatus = "ready"
		asset.DisplaySHA256 = strings.Repeat("2", 64)
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatalf("upsert cleanup asset: %v", err)
	}
	return st, doc, asset, sourceFence
}

func TestClaimRAGStagingAssetCleanupFencesObjectsAndDeletesCatalogAtomically(t *testing.T) {
	ctx := context.Background()
	st, doc, asset, sourceFence := ragAssetCleanupFixture(
		t, "claim", "ast_cleanup_claim", true,
	)
	if source := ragAssetCleanupObjectWrite(t, st, asset.SourceObjectKey); source.Status != ragObjectWritePublished {
		t.Fatalf("source status before claim=%q", source.Status)
	}

	failed := testRAGVersion(doc.ID, 2)
	if err := st.CreateRAGDocumentVersion(ctx, failed); err != nil {
		t.Fatalf("create terminal version: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_document_versions SET status='FAILED'
		WHERE doc_id=? AND doc_version=?`, doc.ID, 2); err != nil {
		t.Fatalf("fail terminal version: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO rag_version_assets
		(doc_id,doc_version,asset_id) VALUES (?,?,?)`, doc.ID, 2, asset.ID); err != nil {
		t.Fatalf("pin terminal asset: %v", err)
	}
	maintenance, err := st.ClaimRAGDocumentMaintenance(ctx, doc.ID, "asset-cleanup-claim", time.Minute)
	if err != nil || maintenance == nil {
		t.Fatalf("maintenance claim=%+v err=%v", maintenance, err)
	}

	claim, ok, err := st.ClaimRAGStagingAssetCleanup(ctx, *maintenance, asset.ID)
	if err != nil || !ok || claim == nil {
		t.Fatalf("asset cleanup claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if claim.AssetID != asset.ID || claim.DocID != doc.ID || len(claim.ObjectWrites) != 2 {
		t.Fatalf("asset cleanup claim=%+v", claim)
	}
	if _, err := st.GetRAGAsset(ctx, asset.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("asset catalog survived claim: %v", err)
	}
	var pins int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_version_assets WHERE asset_id=?`, asset.ID).
		Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("terminal version pins=%d err=%v", pins, err)
	}

	source := ragAssetCleanupObjectWrite(t, st, asset.SourceObjectKey)
	if source.Status != ragObjectWriteDeleting || source.Generation != sourceFence.Generation+1 ||
		source.ObjectKind != RAGObjectKindAssetSource || source.ReferenceKey != asset.ID {
		t.Fatalf("published source tombstone=%+v", source)
	}
	display := ragAssetCleanupObjectWrite(t, st, asset.DisplayObjectKey)
	if display.Status != ragObjectWriteDeleting || display.Generation != 1 ||
		display.ObjectKind != RAGObjectKindAssetDisplay || display.ReferenceKey != asset.ID ||
		display.UserID != "u_claim" || display.KBID != doc.KBID || display.DocID != doc.ID {
		t.Fatalf("legacy display tombstone=%+v", display)
	}
}

func TestClaimRAGStagingAssetCleanupRetainsReferencedAsset(t *testing.T) {
	ctx := context.Background()
	st, doc, asset, _ := ragAssetCleanupFixture(
		t, "referenced", "ast_cleanup_referenced", false,
	)
	if err := st.ReplaceRAGVersionAssets(ctx, doc.ID, 1, []string{asset.ID}); err != nil {
		t.Fatalf("pin active asset: %v", err)
	}
	maintenance, err := st.ClaimRAGDocumentMaintenance(ctx, doc.ID, "asset-cleanup-referenced", time.Minute)
	if err != nil || maintenance == nil {
		t.Fatalf("maintenance claim=%+v err=%v", maintenance, err)
	}

	claim, ok, err := st.ClaimRAGStagingAssetCleanup(ctx, *maintenance, asset.ID)
	if err != nil || ok || claim != nil {
		t.Fatalf("referenced cleanup claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := st.GetRAGAsset(ctx, asset.ID); err != nil {
		t.Fatalf("referenced asset removed: %v", err)
	}
	if source := ragAssetCleanupObjectWrite(t, st, asset.SourceObjectKey); source.Status != ragObjectWritePublished {
		t.Fatalf("referenced source status=%q", source.Status)
	}
}

func TestClaimRAGStagingAssetCleanupFailsClosedAndRollsBack(t *testing.T) {
	ctx := context.Background()
	st, doc, asset, _ := ragAssetCleanupFixture(
		t, "rollback", "ast_cleanup_rollback", true,
	)
	if _, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindAssetDisplay, ObjectKey: asset.DisplayObjectKey,
		ReferenceKey: asset.ID,
	}); err != nil {
		t.Fatalf("begin delayed display write: %v", err)
	}
	maintenance, err := st.ClaimRAGDocumentMaintenance(ctx, doc.ID, "asset-cleanup-rollback", time.Minute)
	if err != nil || maintenance == nil {
		t.Fatalf("maintenance claim=%+v err=%v", maintenance, err)
	}

	claim, ok, err := st.ClaimRAGStagingAssetCleanup(ctx, *maintenance, asset.ID)
	if !errors.Is(err, ErrRAGDocumentVersionConflict) || ok || claim != nil {
		t.Fatalf("unready cleanup claim=%+v ok=%v err=%v", claim, ok, err)
	}
	if _, err := st.GetRAGAsset(ctx, asset.ID); err != nil {
		t.Fatalf("failed claim removed asset: %v", err)
	}
	if source := ragAssetCleanupObjectWrite(t, st, asset.SourceObjectKey); source.Status != ragObjectWritePublished {
		t.Fatalf("failed claim leaked source transition: %+v", source)
	}
}
