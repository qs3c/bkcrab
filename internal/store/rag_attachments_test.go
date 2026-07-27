package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRAGAttachmentCatalogAndChunkMapping(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_attachment_catalog", 1)
	ctx := context.Background()
	hash := strings.Repeat("1", 64)
	attachmentID := "att_" + strings.Repeat("2", 32)
	objectKey := "rag/u_claim/" + doc.KBID + "/" + doc.ID +
		"/attachments/" + hash + "/versions/1/source.vsdx"
	fence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindAssetAttachment, ObjectKey: objectKey,
		ReferenceKey: attachmentID,
	})
	if err != nil {
		t.Fatalf("begin attachment write: %v", err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
		t.Fatalf("mark attachment ready=%v err=%v", ready, err)
	}
	attachment := &RAGAttachmentRecord{
		ID: attachmentID, DocID: doc.ID, ContentSHA256: hash,
		Kind: "visio_source", FileName: "diagram.vsdx",
		MIMEType:  "application/vnd.ms-visio.drawing",
		ObjectKey: objectKey, ByteSize: 1234,
		FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAttachment(ctx, attachment); err != nil {
		t.Fatalf("upsert attachment: %v", err)
	}
	if err := st.ReplaceRAGVersionAttachments(ctx, doc.ID, 1, []string{attachmentID}); err != nil {
		t.Fatalf("replace version attachments: %v", err)
	}

	assetID := "ast_" + strings.Repeat("3", 32)
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO rag_assets (
		id,doc_id,content_sha256,source_kind,source_mime,display_mime,
		source_object_key,display_object_key,thumbnail_object_key,display_status,
		display_sha256,thumbnail_sha256,byte_size,width,height,first_seen_version,
		last_seen_version,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`,
		assetID, doc.ID, strings.Repeat("4", 64), "embedded_preview", "image/png",
		"image/png", "source", "display", "thumbnail", "ready",
		strings.Repeat("5", 64), strings.Repeat("6", 64), 10, 1, 1, 1, 1); err != nil {
		t.Fatalf("seed image asset: %v", err)
	}
	if err := st.PutRAGChunkAssets(ctx, []RAGChunkAssetRecord{{
		DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, AssetID: assetID,
		AttachmentID: attachmentID, Ordinal: 0,
		LocationJSON: `{"kind":"document"}`,
	}}); err != nil {
		t.Fatalf("put chunk attachment mapping: %v", err)
	}
	mappings, err := st.ListRAGChunkAssetsByRefs(ctx, []RAGChunkRef{{
		DocID: doc.ID, DocVersion: 1, ChunkIndex: 0,
	}})
	if err != nil || len(mappings) != 1 ||
		mappings[0].AttachmentID != attachmentID {
		t.Fatalf("mappings=%+v err=%v", mappings, err)
	}
	got, err := st.GetRAGAttachment(ctx, attachmentID)
	if err != nil || got.ObjectKey != objectKey || got.FileName != "diagram.vsdx" {
		t.Fatalf("attachment=%+v err=%v", got, err)
	}
}

func TestRAGAttachmentOrphanCleanupUsesMaintenanceAndObjectFence(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_attachment_cleanup", 1)
	activateRAGLifecycleVersion(t, st, "attachment-cleanup-index", 0)
	ctx := context.Background()
	hash := strings.Repeat("7", 64)
	attachmentID := "att_" + strings.Repeat("8", 32)
	objectKey := "rag/u_claim/" + doc.KBID + "/" + doc.ID +
		"/attachments/" + hash + "/versions/1/source.vsdx"
	write, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindAssetAttachment, ObjectKey: objectKey,
		ReferenceKey: attachmentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *write); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	attachment := &RAGAttachmentRecord{
		ID: attachmentID, DocID: doc.ID, ContentSHA256: hash,
		Kind: "visio_source", FileName: "cleanup.vsdx",
		MIMEType:  "application/vnd.ms-visio.drawing",
		ObjectKey: objectKey, ByteSize: 99,
		FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAttachment(ctx, attachment); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRAGVersionAttachments(ctx, doc.ID, 1, []string{attachmentID}); err != nil {
		t.Fatal(err)
	}
	if candidates, err := st.ListRAGStagingAttachmentCleanupCandidates(
		ctx, 0, 10); err != nil || len(candidates) != 0 {
		t.Fatalf("live attachment candidates=%+v err=%v", candidates, err)
	}
	if err := st.ReplaceRAGVersionAttachments(ctx, doc.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := st.DB().ExecContext(ctx,
		`UPDATE rag_attachments SET updated_at=? WHERE id=?`, old, attachmentID); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ListRAGStagingAttachmentCleanupCandidates(
		ctx, 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 || candidates[0].ID != attachmentID {
		t.Fatalf("orphan attachment candidates=%+v err=%v", candidates, err)
	}
	maintenance, err := st.ClaimRAGDocumentMaintenance(
		ctx, doc.ID, "attachment-cleaner", time.Minute)
	if err != nil || maintenance == nil {
		t.Fatalf("maintenance=%+v err=%v", maintenance, err)
	}
	claim, claimed, err := st.ClaimRAGStagingAttachmentCleanup(
		ctx, *maintenance, attachmentID)
	if err != nil || !claimed || claim == nil ||
		len(claim.ObjectWrites) != 1 ||
		claim.ObjectWrites[0].ObjectKind != RAGObjectKindAssetAttachment ||
		claim.ObjectWrites[0].Status != ragObjectWriteDeleting {
		t.Fatalf("claim=%+v claimed=%v err=%v", claim, claimed, err)
	}
	if _, err := st.GetRAGAttachment(ctx, attachmentID); err == nil {
		t.Fatal("claimed orphan attachment row still exists")
	}
	if finished, err := st.FinishRAGObjectWriteCleanup(
		ctx, claim.ObjectWrites[0]); err != nil || !finished {
		t.Fatalf("finish=%v err=%v", finished, err)
	}
}
