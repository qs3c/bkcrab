package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func seedRAGObjectWriteOwner(t *testing.T, st *DBStore, withDocument bool) (*RAGKBRecord, *RAGDocumentRecord) {
	t.Helper()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_object_stage", "active")
	kb := &RAGKBRecord{
		ID: "kb_object_stage", UserID: "u_object_stage", Name: "object staging",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 8,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatalf("create KB: %v", err)
	}
	doc := &RAGDocumentRecord{
		ID: "doc_object_stage", KBID: kb.ID, FileName: "source.md", FileType: "md", FileSize: 4,
		ObjectKey: "rag/u_object_stage/kb_object_stage/doc_object_stage/original/source.md",
		Status:    "PENDING", Version: 1, IndexFormatVersion: 1, ProcessingStage: "queued",
		UploadedAt: time.Now().UTC(),
	}
	if withDocument {
		if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, testRAGDocumentVersion(doc.ID, 1), 3); err != nil {
			t.Fatalf("create document: %v", err)
		}
	}
	return kb, doc
}

func TestRAGOriginalWriteHandleIsConsumedAtomicallyByDocumentCreate(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	kb, doc := seedRAGObjectWriteOwner(t, st, false)
	fence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, ObjectKind: RAGObjectKindOriginal,
		ObjectKey: doc.ObjectKey, ReferenceKey: doc.ID,
	})
	if err != nil {
		t.Fatalf("begin original write: %v", err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
		t.Fatalf("mark original ready: ready=%v err=%v", ready, err)
	}
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, testRAGDocumentVersion(doc.ID, 1), 3); err != nil {
		t.Fatalf("publish document: %v", err)
	}
	var status string
	if err := st.db.QueryRowContext(ctx, `SELECT status FROM rag_object_write_staging WHERE handle_id=?`, fence.HandleID).Scan(&status); err != nil || status != ragObjectWritePublished {
		t.Fatalf("published original registry status=%q err=%v", status, err)
	}
}

func TestRAGAssetWriteHandlesSurviveFailureAndBlockLatePublicationAfterGCClaim(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	kb, doc := seedRAGObjectWriteOwner(t, st, true)
	key := "rag/u_object_stage/kb_object_stage/doc_object_stage/assets/aa/source.png"
	fence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, ObjectKind: RAGObjectKindAssetSource,
		ObjectKey: key, ReferenceKey: "ast_object_stage",
	})
	if err != nil {
		t.Fatalf("begin asset write: %v", err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
		t.Fatalf("mark asset ready: ready=%v err=%v", ready, err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=? WHERE handle_id=?`,
		time.Now().UTC().Add(-48*time.Hour), fence.HandleID); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ListRAGObjectWriteCleanupCandidates(ctx, 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("cleanup candidates=%+v err=%v", candidates, err)
	}
	cleanup, claimed, err := st.ClaimRAGObjectWriteCleanup(ctx, candidates[0])
	if err != nil || !claimed {
		t.Fatalf("claim cleanup=%+v claimed=%v err=%v", cleanup, claimed, err)
	}
	asset := &RAGAssetRecord{
		ID: "ast_object_stage", DocID: doc.ID,
		ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceKind:    "embedded_original", SourceMIME: "image/png", SourceObjectKey: key,
		DisplayStatus: "unavailable", ByteSize: 4, Width: 1, Height: 1,
		FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAsset(ctx, asset); !errors.Is(err, ErrRAGLifecycleInactive) {
		t.Fatalf("late asset publication crossed cleanup claim: %v", err)
	}
	if _, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, ObjectKind: RAGObjectKindAssetSource,
		ObjectKey: key, ReferenceKey: asset.ID,
	}); !errors.Is(err, ErrRAGLifecycleInactive) {
		t.Fatalf("late writer revived deleting handle: %v", err)
	}
	if finished, err := st.FinishRAGObjectWriteCleanup(ctx, *cleanup); err != nil || !finished {
		t.Fatalf("finish cleanup: finished=%v err=%v", finished, err)
	}
}

func TestRAGAssetUpsertConsumesAllReadyObjectHandles(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	kb, doc := seedRAGObjectWriteOwner(t, st, true)
	asset := &RAGAssetRecord{
		ID: "ast_ready", DocID: doc.ID,
		ContentSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SourceKind:    "embedded_original", SourceMIME: "image/png", DisplayMIME: "image/webp",
		SourceObjectKey:    "rag/u_object_stage/kb_object_stage/doc_object_stage/assets/bb/source.png",
		DisplayObjectKey:   "rag/u_object_stage/kb_object_stage/doc_object_stage/assets/bb/display.webp",
		ThumbnailObjectKey: "rag/u_object_stage/kb_object_stage/doc_object_stage/assets/bb/thumbnail.webp",
		DisplayStatus:      "ready",
		DisplaySHA256:      "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ThumbnailSHA256:    "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ByteSize:           4, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	for _, item := range []struct{ kind, key string }{
		{RAGObjectKindAssetSource, asset.SourceObjectKey},
		{RAGObjectKindAssetDisplay, asset.DisplayObjectKey},
		{RAGObjectKindAssetThumbnail, asset.ThumbnailObjectKey},
	} {
		fence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
			UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, ObjectKind: item.kind,
			ObjectKey: item.key, ReferenceKey: asset.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ready, err := st.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
			t.Fatalf("ready=%v err=%v", ready, err)
		}
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatalf("upsert asset: %v", err)
	}
	var count int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_object_write_staging
		WHERE doc_id=? AND status='PUBLISHED'`, doc.ID).Scan(&count); err != nil || count != 3 {
		t.Fatalf("asset publication registry count=%d err=%v", count, err)
	}
	if _, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID, ObjectKind: RAGObjectKindAssetSource,
		ObjectKey: asset.SourceObjectKey, ReferenceKey: asset.ID,
	}); !errors.Is(err, ErrRAGDocumentVersionConflict) {
		t.Fatalf("published registry reopened as writer: %v", err)
	}
}

func TestPublishRAGAssetsForIndexRejectsExpiredFence(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_asset_publish_fence", 1)
	ctx := context.Background()
	claim, err := st.ClaimRAGIndexTask(ctx, "asset-publish-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	asset := RAGAssetRecord{
		ID: "ast_publish_fence", DocID: doc.ID,
		ContentSHA256: strings.Repeat("f", 64), SourceKind: "embedded_original", SourceMIME: "image/png",
		SourceObjectKey: "rag/u_claim/" + doc.KBID + "/" + doc.ID + "/assets/" + strings.Repeat("f", 64) +
			"/versions/1/source.png",
		DisplayStatus: "unavailable", ByteSize: 4, Width: 1, Height: 1,
		FirstSeenVersion: claim.Fence.DocVersion, LastSeenVersion: claim.Fence.DocVersion,
	}
	writeFence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID, ObjectKind: RAGObjectKindAssetSource,
		ObjectKey: asset.SourceObjectKey, ReferenceKey: asset.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *writeFence); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_index_tasks SET lease_until=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute), claim.Fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if published, err := st.PublishRAGAssetsForIndex(ctx, claim.Fence, []RAGAssetRecord{asset}, []string{asset.ID}); err != nil || published {
		t.Fatalf("expired fence publication=%v err=%v", published, err)
	}
	if _, err := st.GetRAGAsset(ctx, asset.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired fence created asset catalog: %v", err)
	}
	var pins int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_version_assets
		WHERE doc_id=? AND doc_version=?`, doc.ID, claim.Fence.DocVersion).Scan(&pins); err != nil || pins != 0 {
		t.Fatalf("expired fence pins=%d err=%v", pins, err)
	}
}

func TestRAGArtifactWriteCleanupProtectsRunnableVersionThenCollectsFailure(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_staged_artifact_reference", 1)
	ctx := context.Background()
	claim, err := st.ClaimRAGIndexTask(ctx, "staged-artifact-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	artifactKey := "rag/u_claim/" + doc.KBID + "/" + doc.ID + "/artifacts/" +
		claim.Version.ParseFingerprint + "/parsed.json"
	if ok, err := st.RecordRAGDocumentParseArtifact(ctx, claim.Fence, artifactKey); err != nil || !ok {
		t.Fatalf("record artifact handle ok=%v err=%v", ok, err)
	}
	fence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindParsedArtifact, ObjectKey: artifactKey,
		ReferenceKey: artifactKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_object_write_staging
		SET updated_at=? WHERE handle_id=?`, old, fence.HandleID); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ListRAGObjectWriteCleanupCandidates(ctx, 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("runnable candidates=%+v err=%v", candidates, err)
	}
	if cleanup, claimed, err := st.ClaimRAGObjectWriteCleanup(ctx, candidates[0]); err != nil || claimed || cleanup != nil {
		t.Fatalf("runnable artifact cleanup=%+v claimed=%v err=%v", cleanup, claimed, err)
	}
	if ok, err := st.FailRAGIndexTask(ctx, claim.Fence, "fail after publish"); err != nil || !ok {
		t.Fatalf("fail task ok=%v err=%v", ok, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_object_write_staging
		SET updated_at=? WHERE handle_id=?`, old, fence.HandleID); err != nil {
		t.Fatal(err)
	}
	candidates, err = st.ListRAGObjectWriteCleanupCandidates(ctx, 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("failed candidates=%+v err=%v", candidates, err)
	}
	cleanup, claimed, err := st.ClaimRAGObjectWriteCleanup(ctx, candidates[0])
	if err != nil || !claimed || cleanup == nil || cleanup.Status != ragObjectWriteDeleting {
		t.Fatalf("failed artifact cleanup=%+v claimed=%v err=%v", cleanup, claimed, err)
	}
	immediate, err := st.ListRAGObjectWriteCleanupCandidates(ctx, 24*time.Hour, 10)
	if err != nil || len(immediate) != 0 {
		t.Fatalf("in-flight delete was immediately reclaimed: %+v err=%v", immediate, err)
	}
}

func TestRAGActivationAtomicallyConsumesArtifactWriteHandles(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_staged_artifact_activation", 1)
	ctx := context.Background()
	claim, err := st.ClaimRAGIndexTask(ctx, "artifact-activation-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	prefix := "rag/u_claim/" + doc.KBID + "/" + doc.ID + "/artifacts/" +
		claim.Version.ParseFingerprint + "/"
	artifactKey := prefix + "parsed.json"
	if ok, err := st.RecordRAGDocumentParseArtifact(ctx, claim.Fence, artifactKey); err != nil || !ok {
		t.Fatalf("record handle ok=%v err=%v", ok, err)
	}
	for _, item := range []struct{ kind, key string }{
		{RAGObjectKindNormalized, prefix + "normalized.md"},
		{RAGObjectKindParsedArtifact, artifactKey},
	} {
		fence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
			UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID, ObjectKind: item.kind,
			ObjectKey: item.key, ReferenceKey: artifactKey,
		})
		if err != nil {
			t.Fatal(err)
		}
		if ready, err := st.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
			t.Fatalf("ready=%v err=%v", ready, err)
		}
	}
	ok, err := st.ActivateAndFinishRAGIndexTask(ctx, claim.Fence, RAGIndexActivation{
		VersionResult: RAGDocumentVersionResult{
			Status: RAGDocumentVersionDone, ParseArtifactKey: artifactKey,
		},
	}, 0)
	if err != nil || !ok {
		t.Fatalf("activate ok=%v err=%v", ok, err)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_object_write_staging
		WHERE doc_id=? AND status='PUBLISHED'`, doc.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("activation artifact registry count=%d err=%v", count, err)
	}
}

func TestRAGObjectWriteCleanupSurvivesMissingOwnershipChain(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	kb, doc := seedRAGObjectWriteOwner(t, st, true)
	objectKey := "rag/" + kb.UserID + "/" + kb.ID + "/" + doc.ID +
		"/artifacts/" + strings.Repeat("9", 64) + "/versions/1/parsed.json"
	writeFence, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: kb.UserID, KBID: kb.ID, DocID: doc.ID,
		ObjectKind: RAGObjectKindParsedArtifact, ObjectKey: objectKey, ReferenceKey: objectKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tombstoneAndDeleteRAGDocumentForTest(ctx, st, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err := tombstoneAndDeleteRAGKBForTest(ctx, st, kb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkUserDeleting(ctx, kb.UserID); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUser(ctx, kb.UserID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, old, writeFence.HandleID); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ListRAGObjectWriteCleanupCandidates(ctx, 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("missing-owner candidates=%+v err=%v", candidates, err)
	}
	cleanup, claimed, err := st.ClaimRAGObjectWriteCleanup(ctx, candidates[0])
	if err != nil || !claimed || cleanup == nil || cleanup.Status != ragObjectWriteDeleting {
		t.Fatalf("missing-owner claim=%+v claimed=%v err=%v", cleanup, claimed, err)
	}
	if finished, err := st.FinishRAGObjectWriteCleanup(ctx, *cleanup); err != nil || !finished {
		t.Fatalf("finish missing-owner cleanup=%v err=%v", finished, err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, time.Now().UTC().Add(-time.Minute), writeFence.HandleID); err != nil {
		t.Fatal(err)
	}
	candidates, err = st.ListRAGObjectWriteCleanupCandidates(ctx, 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 || candidates[0].Status != ragObjectWriteDeleting {
		t.Fatalf("durable missing-owner re-sweep candidates=%+v err=%v", candidates, err)
	}
}
