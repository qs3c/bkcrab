package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func ensureRAGLifecycleUser(t *testing.T, st *DBStore, id, status string) {
	t.Helper()
	ctx := context.Background()
	if existing, err := st.GetUser(ctx, id); err == nil && existing != nil {
		if existing.Status != status {
			existing.Status = status
			if err := st.UpdateUser(ctx, existing); err != nil {
				t.Fatalf("update lifecycle user: %v", err)
			}
		}
		return
	}
	now := time.Now().UTC()
	if err := st.CreateUser(ctx, &UserRecord{
		ID: id, Username: id, Email: id + "@example.invalid", DisplayName: id, Role: "user", Status: status,
		AgentQuota: -1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create lifecycle user: %v", err)
	}
}

func activateRAGLifecycleVersion(t *testing.T, st *DBStore, worker string, grace time.Duration) *RAGIndexClaim {
	t.Helper()
	claim, err := st.ClaimRAGIndexTask(context.Background(), worker, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim lifecycle version: %+v, %v", claim, err)
	}
	ok, err := st.ActivateAndFinishRAGIndexTask(context.Background(), claim.Fence,
		RAGIndexActivation{VersionResult: RAGDocumentVersionResult{Status: RAGDocumentVersionDone}}, grace)
	if err != nil || !ok {
		t.Fatalf("activate lifecycle version: ok=%v err=%v", ok, err)
	}
	return claim
}

func TestRAGDocumentDeleteTombstoneRevokesClaimAndPreservesLedger(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, taskID := seedRAGTaskDocument(t, st, "doc_delete_tombstone", 3)
	ctx := context.Background()
	if err := st.CreateRAGDocumentAITaskBudget(ctx, &RAGDocumentAITaskBudgetRecord{
		TaskID: taskID, UserID: "u_claim", MaxRequests: 5, MaxTokens: 100,
		MaxCostMicroUSD: 1_000,
	}); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.MarkRAGDocumentDeleting(ctx, doc.ID)
	if err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	if deleted.Status != RAGDocumentStatusDeleting || deleted.ActiveVersion != 0 {
		t.Fatalf("deleting document = %+v", deleted)
	}
	task, err := st.GetRAGIndexTask(ctx, taskID)
	if err != nil || task.Status != "SUPERSEDED" || task.LeaseOwner != "" {
		t.Fatalf("task after tombstone = %+v, %v", task, err)
	}
	version, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || version.Status != RAGDocumentVersionSuperseded {
		t.Fatalf("version after tombstone = %+v, %v", version, err)
	}
	if claim, err := st.ClaimRAGIndexTask(ctx, "worker-after-delete", time.Minute); err != nil || claim != nil {
		t.Fatalf("claim after tombstone = %+v, %v", claim, err)
	}
	if _, err := st.GetRAGDocumentAITaskBudget(ctx, taskID); err != nil {
		t.Fatalf("delete tombstone removed DocumentAI ledger: %v", err)
	}
}

func TestRAGDocumentCleanupWaitsForRevokedLeaseOrAcknowledgement(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, st, "doc_cleanup_quiescence", 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), "remote-worker", 80*time.Millisecond)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	if _, err := st.MarkRAGDocumentDeleting(context.Background(), doc.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.IsRAGDocumentCleanupReady(context.Background(), doc.ID); err != nil || ready {
		t.Fatalf("cleanup before quiescence ready=%v err=%v", ready, err)
	}
	if err := st.DeleteRAGDocument(context.Background(), doc.ID); !errors.Is(err, ErrRAGCleanupNotReady) {
		t.Fatalf("finalizer crossed revoked worker lease: %v", err)
	}
	if ok, err := st.AcknowledgeRAGIndexTaskQuiesced(context.Background(), claim.Fence); err != nil || !ok {
		t.Fatalf("quiescence acknowledgement ok=%v err=%v", ok, err)
	}
	if ready, err := st.IsRAGDocumentCleanupReady(context.Background(), doc.ID); err != nil || !ready {
		t.Fatalf("cleanup after acknowledgement ready=%v err=%v", ready, err)
	}

	doc2, _ := seedRAGTaskDocument(t, st, "doc_cleanup_lease_expiry", 3)
	claim2, err := st.ClaimRAGIndexTask(context.Background(), "crashed-worker", time.Minute)
	if err != nil || claim2 == nil {
		t.Fatalf("second claim=%+v err=%v", claim2, err)
	}
	if _, err := st.MarkRAGDocumentDeleting(context.Background(), doc2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE rag_index_tasks SET lease_until=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), claim2.Task.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.IsRAGDocumentCleanupReady(context.Background(), doc2.ID); err != nil || !ready {
		t.Fatalf("cleanup after crashed lease expiry ready=%v err=%v", ready, err)
	}
}

func TestRAGKBTombstoneAtomicallyRevokesAllDocuments(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc1, task1 := seedRAGTaskDocument(t, st, "doc_kb_delete_1", 3)
	// Move the second document into the same KB created by the first fixture.
	doc2 := &RAGDocumentRecord{
		ID: "doc_kb_delete_2", KBID: doc1.KBID, FileName: "two.md", FileType: "md",
		FileSize: 3, ObjectKey: "rag/u/kb/two.md", Status: "PENDING", Version: 1,
		SourceSHA256:       testRAGVersion("doc_kb_delete_2", 1).SourceSHA256,
		IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: time.Now().UTC(),
	}
	task2, err := st.CreateRAGDocumentWithVersionAndIndexTask(context.Background(), doc2,
		testRAGVersion(doc2.ID, 1), 3)
	if err != nil {
		t.Fatal(err)
	}

	kb, err := st.MarkRAGKBDeleting(context.Background(), doc1.KBID)
	if err != nil {
		t.Fatal(err)
	}
	if kb.Status != RAGKBStatusDeleting {
		t.Fatalf("KB status=%q", kb.Status)
	}
	for _, id := range []string{doc1.ID, doc2.ID} {
		doc, err := st.GetRAGDocument(context.Background(), id)
		if err != nil || doc.Status != RAGDocumentStatusDeleting || doc.ActiveVersion != 0 {
			t.Fatalf("document %s after KB tombstone = %+v, %v", id, doc, err)
		}
	}
	for _, id := range []int64{task1, task2} {
		task, err := st.GetRAGIndexTask(context.Background(), id)
		if err != nil || task.Status != "SUPERSEDED" {
			t.Fatalf("task %d after KB tombstone = %+v, %v", id, task, err)
		}
	}
	docs, err := st.ListDeletingRAGDocuments(context.Background(), "", 10)
	if err != nil || len(docs) != 2 {
		t.Fatalf("deleting docs = %+v, %v", docs, err)
	}
	kbs, err := st.ListDeletingRAGKBs(context.Background(), "", 10)
	if err != nil || len(kbs) != 1 || kbs[0].ID != doc1.KBID {
		t.Fatalf("deleting KBs = %+v, %v", kbs, err)
	}
}

func TestRAGIndexClaimFailsClosedForInactiveOwnershipChain(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *DBStore, *RAGDocumentRecord)
	}{
		{"document deleting", func(t *testing.T, st *DBStore, doc *RAGDocumentRecord) {
			if _, err := st.MarkRAGDocumentDeleting(context.Background(), doc.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"KB deleting", func(t *testing.T, st *DBStore, doc *RAGDocumentRecord) {
			if _, err := st.MarkRAGKBDeleting(context.Background(), doc.KBID); err != nil {
				t.Fatal(err)
			}
		}},
		{"user disabled", func(t *testing.T, st *DBStore, _ *RAGDocumentRecord) {
			ensureRAGLifecycleUser(t, st, "u_claim", "disabled")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := openRAGTaskClaimStore(t)
			ensureRAGLifecycleUser(t, st, "u_claim", "active")
			doc, taskID := seedRAGTaskDocument(t, st, "doc_inactive_chain", 3)
			test.mutate(t, st, doc)
			claim, err := st.ClaimRAGIndexTask(context.Background(), "worker-inactive", time.Minute)
			if err != nil || claim != nil {
				t.Fatalf("inactive chain claim = %+v, %v", claim, err)
			}
			task, err := st.GetRAGIndexTask(context.Background(), taskID)
			if err != nil || task.Status == "PENDING" || task.Status == "RUNNING" {
				t.Fatalf("inactive chain left runnable task = %+v, %v", task, err)
			}
		})
	}
}

func TestRAGIndexGCLeaseFenceAndExactFinish(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, st, "doc_gc_exact", 3)
	first := activateRAGLifecycleVersion(t, st, "index-one", 0)
	if first.Fence.DocVersion != 1 {
		t.Fatalf("first version=%d", first.Fence.DocVersion)
	}
	next, err := st.AdvanceDocumentVersionAndCreateTask(context.Background(), 1, testRAGVersion(doc.ID, 0))
	if err != nil || next.DocVersion != 2 {
		t.Fatalf("advance = %+v, %v", next, err)
	}
	second := activateRAGLifecycleVersion(t, st, "index-two", 0)
	if second.Fence.DocVersion != 2 {
		t.Fatalf("second version=%d", second.Fence.DocVersion)
	}

	now := time.Now().UTC()
	chunks := []RAGChunkRecord{
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, RawContent: "old", SearchContent: "old", CreatedAt: now},
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: 2, ChunkIndex: 0, RawContent: "new", SearchContent: "new", CreatedAt: now},
	}
	if err := st.PutRAGChunks(context.Background(), chunks); err != nil {
		t.Fatal(err)
	}
	asset := &RAGAssetRecord{
		ID: "ast_11111111111111111111111111111111", DocID: doc.ID,
		ContentSHA256: "1111111111111111111111111111111111111111111111111111111111111111",
		SourceKind:    "embedded_original", SourceMIME: "image/png", DisplayMIME: "image/webp",
		SourceObjectKey: "source", DisplayObjectKey: "display", ThumbnailObjectKey: "thumb",
		DisplayStatus: "ready", DisplaySHA256: "2222222222222222222222222222222222222222222222222222222222222222",
		ThumbnailSHA256: "3333333333333333333333333333333333333333333333333333333333333333",
		ByteSize:        3, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 2,
	}
	if err := st.UpsertRAGAsset(context.Background(), asset); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRAGChunkAssets(context.Background(), []RAGChunkAssetRecord{
		{DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, AssetID: asset.ID, Ordinal: 0},
		{DocID: doc.ID, DocVersion: 2, ChunkIndex: 0, AssetID: asset.ID, Ordinal: 0},
	}); err != nil {
		t.Fatal(err)
	}
	gc, err := st.ClaimRAGIndexGCTask(context.Background(), "gc-one", time.Minute)
	if err != nil || gc == nil || gc.KBID != doc.KBID || gc.Fence.RetiredVersion != 1 {
		t.Fatalf("GC claim = %+v, %v", gc, err)
	}
	stale := gc.Fence
	stale.ClaimGeneration++
	if ok, err := st.HeartbeatRAGIndexGCTask(context.Background(), stale, time.Minute); err != nil || ok {
		t.Fatalf("stale GC heartbeat ok=%v err=%v", ok, err)
	}
	if ok, err := st.FinishRAGIndexGCTask(context.Background(), stale); err != nil || ok {
		t.Fatalf("stale GC finish ok=%v err=%v", ok, err)
	}
	if ok, err := st.FinishRAGIndexGCTask(context.Background(), gc.Fence); err != nil || !ok {
		t.Fatalf("GC finish ok=%v err=%v", ok, err)
	}
	if old, err := st.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 1); err != nil || len(old) != 0 {
		t.Fatalf("old chunks = %+v, %v", old, err)
	}
	if current, err := st.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 2); err != nil || len(current) != 1 {
		t.Fatalf("current chunks = %+v, %v", current, err)
	}
	if mappings, err := st.ListRAGChunkAssetsByRefs(context.Background(), []RAGChunkRef{
		{DocID: doc.ID, DocVersion: 1, ChunkIndex: 0},
		{DocID: doc.ID, DocVersion: 2, ChunkIndex: 0},
	}); err != nil || len(mappings) != 1 || mappings[0].DocVersion != 2 {
		t.Fatalf("mappings after exact GC = %+v, %v", mappings, err)
	}
	version, err := st.GetRAGDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil || version.Status != RAGDocumentVersionGCED {
		t.Fatalf("GCED tombstone = %+v, %v", version, err)
	}
	finished, err := st.GetRAGIndexGCTask(context.Background(), gc.Task.ID)
	if err != nil || finished.Status != "DONE" {
		t.Fatalf("GC task after finish = %+v, %v", finished, err)
	}
}

func TestRAGIndexGCRetryPreservesRetiredVersion(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, st, "doc_gc_retry", 3)
	activateRAGLifecycleVersion(t, st, "index-one", 0)
	if _, err := st.AdvanceDocumentVersionAndCreateTask(context.Background(), 1, testRAGVersion(doc.ID, 0)); err != nil {
		t.Fatal(err)
	}
	activateRAGLifecycleVersion(t, st, "index-two", 0)
	gc, err := st.ClaimRAGIndexGCTask(context.Background(), "gc-retry-one", time.Minute)
	if err != nil || gc == nil {
		t.Fatalf("first GC claim = %+v, %v", gc, err)
	}
	if ok, err := st.RetryRAGIndexGCTask(context.Background(), gc.Fence, 0); err != nil || !ok {
		t.Fatalf("retry GC ok=%v err=%v", ok, err)
	}
	version, err := st.GetRAGDocumentVersion(context.Background(), doc.ID, 1)
	if err != nil || version.Status != RAGDocumentVersionRetired {
		t.Fatalf("retry changed retired version = %+v, %v", version, err)
	}
	reclaimed, err := st.ClaimRAGIndexGCTask(context.Background(), "gc-retry-two", time.Minute)
	if err != nil || reclaimed == nil || reclaimed.Fence.ClaimGeneration <= gc.Fence.ClaimGeneration {
		t.Fatalf("reclaimed GC = %+v, %v", reclaimed, err)
	}
}

func TestRAGVersionOrphanSweepIsExactAndFair(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, st, "doc_orphan_sweep", 3)
	activateRAGLifecycleVersion(t, st, "index-active", 0)
	failed := testRAGVersion(doc.ID, 2)
	if err := st.CreateRAGDocumentVersion(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE rag_document_versions SET status='FAILED',parse_artifact_key=?,updated_at=?
		 WHERE doc_id=? AND doc_version=?`, "rag/u/kb/doc/artifacts/failed/parsed.json",
		time.Now().UTC().Add(-48*time.Hour), doc.ID, int64(2)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRAGChunks(context.Background(), []RAGChunkRecord{
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, RawContent: "active", SearchContent: "active"},
		{KBID: doc.KBID, DocID: doc.ID, DocVersion: 2, ChunkIndex: 0, RawContent: "orphan", SearchContent: "orphan"},
	}); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ListRAGVersionCleanupCandidates(context.Background(), 24*time.Hour, 10)
	if err != nil || len(candidates) != 1 || candidates[0].DocVersion != 2 || candidates[0].KBID != doc.KBID {
		t.Fatalf("version candidates = %+v, %v", candidates, err)
	}
	if referenced, err := st.IsRAGParseArtifactReferenced(context.Background(), doc.ID, candidates[0].ParseArtifactKey); err != nil || referenced {
		t.Fatalf("terminal-only artifact referenced=%v err=%v", referenced, err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_document_versions
		SET parse_artifact_key=? WHERE doc_id=? AND doc_version=1`, candidates[0].ParseArtifactKey, doc.ID); err != nil {
		t.Fatal(err)
	}
	if referenced, err := st.IsRAGParseArtifactReferenced(context.Background(), doc.ID, candidates[0].ParseArtifactKey); err != nil || !referenced {
		t.Fatalf("active shared artifact referenced=%v err=%v", referenced, err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_document_versions
		SET parse_artifact_key='' WHERE doc_id=? AND doc_version=1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	before := candidates[0].UpdatedAt
	if ok, err := st.MarkRAGDocumentVersionGCED(context.Background(), doc.ID, 2); err != nil || !ok {
		t.Fatalf("mark orphan GCED ok=%v err=%v", ok, err)
	}
	if chunks, err := st.ListRAGChunksByDocumentVersion(context.Background(), doc.ID, 2); err != nil || len(chunks) != 0 {
		t.Fatalf("orphan chunks remain = %+v, %v", chunks, err)
	}
	if active, err := st.MarkRAGDocumentVersionGCED(context.Background(), doc.ID, 1); err != nil || active {
		t.Fatalf("active version cleanup ok=%v err=%v", active, err)
	}
	tombstone, err := st.GetRAGDocumentVersion(context.Background(), doc.ID, 2)
	if err != nil || tombstone.Status != RAGDocumentVersionGCED || !tombstone.UpdatedAt.After(before) {
		t.Fatalf("orphan tombstone = %+v, %v", tombstone, err)
	}
	firstUpdated := tombstone.UpdatedAt
	time.Sleep(2 * time.Millisecond)
	if ok, err := st.MarkRAGDocumentVersionGCED(context.Background(), doc.ID, 2); err != nil || !ok {
		t.Fatalf("repeat GCED sweep ok=%v err=%v", ok, err)
	}
	tombstone, err = st.GetRAGDocumentVersion(context.Background(), doc.ID, 2)
	if err != nil || !tombstone.UpdatedAt.After(firstUpdated) {
		t.Fatalf("repeat sweep did not refresh fairness timestamp: %+v, %v", tombstone, err)
	}
}

func TestRAGStagingAssetCandidatesProtectLiveAndHistoryReferences(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, st, "doc_asset_sweep", 3)
	activateRAGLifecycleVersion(t, st, "index-assets", 0)
	failed := testRAGVersion(doc.ID, 2)
	if err := st.CreateRAGDocumentVersion(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_document_versions
		SET status='FAILED' WHERE doc_id=? AND doc_version=2`, doc.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	assets := []RAGAssetRecord{
		{ID: "ast_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContentSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{ID: "ast_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ContentSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		{ID: "ast_cccccccccccccccccccccccccccccccc", ContentSHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"},
		{ID: "ast_dddddddddddddddddddddddddddddddd", ContentSHA256: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"},
	}
	for i := range assets {
		assets[i].DocID = doc.ID
		assets[i].SourceKind = "embedded_original"
		assets[i].SourceMIME = "image/png"
		assets[i].DisplayMIME = "image/webp"
		assets[i].SourceObjectKey = "source/" + assets[i].ID
		assets[i].DisplayObjectKey = "display/" + assets[i].ID
		assets[i].ThumbnailObjectKey = "thumb/" + assets[i].ID
		assets[i].DisplayStatus = "ready"
		assets[i].DisplaySHA256 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
		assets[i].ThumbnailSHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
		assets[i].ByteSize = 1
		assets[i].Width = 1
		assets[i].Height = 1
		assets[i].FirstSeenVersion = 2
		assets[i].LastSeenVersion = 2
		if i == 0 {
			assets[i].FirstSeenVersion = 1
		}
		if err := st.UpsertRAGAsset(context.Background(), &assets[i]); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_assets SET updated_at=? WHERE id=?`, old, assets[i].ID); err != nil {
			t.Fatal(err)
		}
	}
	// The first asset is decorative: it belongs to the active v1 artifact but
	// intentionally has no chunk mapping. A failed v2 saw the same stable asset,
	// which advanced last_seen_version; exact membership must still let v1 pin it.
	if err := st.ReplaceRAGVersionAssets(context.Background(), doc.ID, 1, []string{assets[0].ID}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRAGVersionAssets(context.Background(), doc.ID, 2, []string{assets[0].ID}); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.MarkRAGDocumentVersionGCED(context.Background(), doc.ID, 2); err != nil || !ok {
		t.Fatalf("GC failed v2 mapping ok=%v err=%v", ok, err)
	}
	var activeMapping, failedMapping int
	if err := st.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM rag_version_assets
		WHERE doc_id=? AND doc_version=1 AND asset_id=?`, doc.ID, assets[0].ID).Scan(&activeMapping); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(context.Background(), `SELECT COUNT(*) FROM rag_version_assets
		WHERE doc_id=? AND doc_version=2 AND asset_id=?`, doc.ID, assets[0].ID).Scan(&failedMapping); err != nil {
		t.Fatal(err)
	}
	if activeMapping != 1 || failedMapping != 0 {
		t.Fatalf("exact mapping cleanup active=%d failed=%d", activeMapping, failedMapping)
	}
	turnSources, _ := json.Marshal([]map[string]any{{"assets": []map[string]string{{"id": assets[1].ID}}}})
	if err := st.AppendRAGChatTurn(context.Background(), &RAGChatTurnRecord{
		ID: "turn_asset_pin", UserID: "u_claim", KBID: doc.KBID, SessionID: "chat",
		Question: "q", Answer: "a", Sources: turnSources,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `INSERT INTO session_messages
		(user_id,agent_id,session_key,seq,role,metadata) VALUES (?,?,?,?,?,?)`,
		"u_claim", "agent", "session", 0, "assistant", `{"ragResources":[{"asset":{"id":"`+assets[2].ID+`"}}]}`); err != nil {
		t.Fatal(err)
	}

	candidates, err := st.ListRAGStagingAssetCleanupCandidates(context.Background(), 24*time.Hour, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ID != assets[3].ID {
		t.Fatalf("staging candidates = %+v", candidates)
	}
	for _, asset := range assets[:3] {
		referenced, err := st.IsRAGAssetReferenced(context.Background(), asset.ID)
		if err != nil || !referenced {
			t.Fatalf("asset %s referenced=%v err=%v", asset.ID, referenced, err)
		}
	}
	if referenced, err := st.IsRAGAssetReferenced(context.Background(), assets[3].ID); err != nil || referenced {
		t.Fatalf("orphan asset referenced=%v err=%v", referenced, err)
	}
	if err := st.DeleteRAGAsset(context.Background(), assets[3].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetRAGAsset(context.Background(), assets[3].ID); err == nil {
		t.Fatal("orphan asset row still exists")
	}
}

func TestRAGVersionAssetUnpinStartsStagingTTLAtRemoval(t *testing.T) {
	tests := []struct {
		name  string
		unpin func(*testing.T, *DBStore, *RAGDocumentRecord, *RAGAssetRecord)
	}{
		{
			name: "replace mapping",
			unpin: func(t *testing.T, st *DBStore, doc *RAGDocumentRecord, _ *RAGAssetRecord) {
				t.Helper()
				if err := st.ReplaceRAGVersionAssets(context.Background(), doc.ID, 1, nil); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "version catalog GC",
			unpin: func(t *testing.T, st *DBStore, doc *RAGDocumentRecord, _ *RAGAssetRecord) {
				t.Helper()
				if ok, err := st.MarkRAGDocumentVersionGCED(context.Background(), doc.ID, 2); err != nil || !ok {
					t.Fatalf("mark GCED ok=%v err=%v", ok, err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openRAGTaskClaimStore(t)
			ensureRAGLifecycleUser(t, st, "u_claim", "active")
			doc, _ := seedRAGTaskDocument(t, st, "doc_asset_unpin_"+strings.ReplaceAll(tt.name, " ", "_"), 3)
			activateRAGLifecycleVersion(t, st, "asset-unpin-index", 0)
			version := int64(1)
			if tt.name == "version catalog GC" {
				version = 2
				failed := testRAGVersion(doc.ID, version)
				if err := st.CreateRAGDocumentVersion(context.Background(), failed); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_document_versions
					SET status='FAILED' WHERE doc_id=? AND doc_version=?`, doc.ID, version); err != nil {
					t.Fatal(err)
				}
			}
			asset := &RAGAssetRecord{
				ID: "ast_" + strings.Repeat("e", 28) + fmt.Sprintf("%04d", version), DocID: doc.ID,
				ContentSHA256: strings.Repeat("e", 64), SourceKind: "embedded_original", SourceMIME: "image/png",
				SourceObjectKey: "source/unpin.png", DisplayStatus: "unavailable",
				ByteSize: 1, Width: 1, Height: 1, FirstSeenVersion: version, LastSeenVersion: version,
			}
			if err := st.UpsertRAGAsset(context.Background(), asset); err != nil {
				t.Fatal(err)
			}
			if err := st.ReplaceRAGVersionAssets(context.Background(), doc.ID, version, []string{asset.ID}); err != nil {
				t.Fatal(err)
			}
			old := time.Now().UTC().Add(-48 * time.Hour)
			if _, err := st.DB().ExecContext(context.Background(),
				`UPDATE rag_assets SET updated_at=? WHERE id=?`, old, asset.ID); err != nil {
				t.Fatal(err)
			}
			cutoff := time.Now().UTC().Add(-time.Hour)
			tt.unpin(t, st, doc, asset)

			fresh, err := st.GetRAGAsset(context.Background(), asset.ID)
			if err != nil || !fresh.UpdatedAt.After(cutoff) {
				t.Fatalf("asset timestamp did not start at unpin: %+v err=%v cutoff=%v", fresh, err, cutoff)
			}
			candidates, err := st.ListRAGStagingAssetCleanupCandidates(context.Background(), time.Hour, 10)
			if err != nil || len(candidates) != 0 {
				t.Fatalf("freshly unpinned candidate = %+v err=%v", candidates, err)
			}
			if _, err := st.DB().ExecContext(context.Background(),
				`UPDATE rag_assets SET updated_at=? WHERE id=?`, old, asset.ID); err != nil {
				t.Fatal(err)
			}
			candidates, err = st.ListRAGStagingAssetCleanupCandidates(context.Background(), time.Hour, 10)
			if err != nil || len(candidates) != 1 || candidates[0].ID != asset.ID {
				t.Fatalf("expired unpinned candidate = %+v err=%v", candidates, err)
			}
		})
	}
}

func TestRAGAssetReferenceSQLNeverUsesLastSeenAsVersionMembership(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", mysqlDialect} {
		referenceSQL := (&DBStore{dialect: dialect}).ragAssetReferenceSQL("a")
		if strings.Contains(referenceSQL, "last_seen_version") {
			t.Fatalf("%s asset reference SQL uses last_seen_version: %s", dialect, referenceSQL)
		}
		for _, token := range []string{"rag_version_assets", "va.asset_id=a.id", "va.doc_version"} {
			if !strings.Contains(referenceSQL, token) {
				t.Errorf("%s asset reference SQL missing %q: %s", dialect, token, referenceSQL)
			}
		}
	}
}

func TestRAGDocumentMaintenanceLeaseFencesSecondStoreWrites(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.Join(t.TempDir(), "maintenance-lease.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	first, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	ensureRAGLifecycleUser(t, first, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, first, "doc_maintenance_fence", 3)
	activateRAGLifecycleVersion(t, first, "maintenance-index", 0)
	asset := &RAGAssetRecord{
		ID: "ast_maintenance_fence", DocID: doc.ID,
		ContentSHA256: strings.Repeat("9", 64), SourceKind: "embedded_original", SourceMIME: "image/png",
		SourceObjectKey: "source/maintenance.png", DisplayStatus: "unavailable",
		ByteSize: 1, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := first.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}

	fence, err := first.ClaimRAGDocumentMaintenance(ctx, doc.ID, "maintenance-a", time.Minute)
	if err != nil || fence == nil {
		t.Fatalf("first maintenance claim = %+v err=%v", fence, err)
	}
	if valid, err := second.CheckRAGDocumentMaintenance(ctx, *fence); err != nil || !valid {
		t.Fatalf("second store sees fence valid=%v err=%v", valid, err)
	}
	if _, err := second.AdvanceDocumentVersionAndCreateTask(ctx, 1, testRAGVersion(doc.ID, 0)); !errors.Is(err, ErrRAGDocumentMaintenanceActive) {
		t.Fatalf("reindex during maintenance err=%v", err)
	}
	assetRetry := *asset
	if err := second.UpsertRAGAsset(ctx, &assetRetry); !errors.Is(err, ErrRAGDocumentMaintenanceActive) {
		t.Fatalf("asset upsert during maintenance err=%v", err)
	}
	if err := second.ReplaceRAGVersionAssets(ctx, doc.ID, 1, []string{asset.ID}); !errors.Is(err, ErrRAGDocumentMaintenanceActive) {
		t.Fatalf("version asset publish during maintenance err=%v", err)
	}

	if _, err := first.DB().ExecContext(ctx, `UPDATE rag_document_maintenance_leases
		SET lease_until=? WHERE doc_id=?`, time.Now().UTC().Add(-time.Hour), doc.ID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := second.ClaimRAGDocumentMaintenance(ctx, doc.ID, "maintenance-b", time.Minute)
	if err != nil || reclaimed == nil || reclaimed.Generation <= fence.Generation {
		t.Fatalf("reclaimed maintenance = %+v old=%+v err=%v", reclaimed, fence, err)
	}
	if valid, err := first.CheckRAGDocumentMaintenance(ctx, *fence); err != nil || valid {
		t.Fatalf("stale maintenance valid=%v err=%v", valid, err)
	}
	if released, err := first.ReleaseRAGDocumentMaintenance(ctx, *fence); err != nil || released {
		t.Fatalf("stale release released=%v err=%v", released, err)
	}
	if released, err := second.ReleaseRAGDocumentMaintenance(ctx, *reclaimed); err != nil || !released {
		t.Fatalf("current release released=%v err=%v", released, err)
	}
	if err := second.UpsertRAGAsset(ctx, &assetRetry); err != nil {
		t.Fatalf("asset upsert after release: %v", err)
	}
}

func TestRAGDocumentMaintenanceWaitsForSupersededWorkerLease(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	doc, _ := seedRAGTaskDocument(t, st, "doc_maintenance_quiescence", 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), "late-writer", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("index claim = %+v err=%v", claim, err)
	}
	if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_index_tasks
		SET status='SUPERSEDED',finished_at=CURRENT_TIMESTAMP WHERE id=?`, claim.Task.ID); err != nil {
		t.Fatal(err)
	}

	maintenance, err := st.ClaimRAGDocumentMaintenance(
		context.Background(), doc.ID, "maintenance-quiescence", time.Minute,
	)
	if err != nil || maintenance != nil {
		t.Fatalf("maintenance crossed live superseded worker lease: %+v err=%v", maintenance, err)
	}
	if ok, err := st.AcknowledgeRAGIndexTaskQuiesced(context.Background(), claim.Fence); err != nil || !ok {
		t.Fatalf("worker quiescence acknowledgement ok=%v err=%v", ok, err)
	}
	maintenance, err = st.ClaimRAGDocumentMaintenance(
		context.Background(), doc.ID, "maintenance-quiescence", time.Minute,
	)
	if err != nil || maintenance == nil {
		t.Fatalf("maintenance after worker quiescence = %+v err=%v", maintenance, err)
	}
}

func TestRAGDocumentFinalizerWaitsForMaintenanceLease(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, taskID := seedRAGTaskDocument(t, st, "doc_finalizer_maintenance", 3)
	ctx := context.Background()
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_index_tasks SET status='FAILED'
		WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	maintenance, err := st.ClaimRAGDocumentMaintenance(
		ctx, doc.ID, "finalizer-maintenance", time.Minute,
	)
	if err != nil || maintenance == nil {
		t.Fatalf("maintenance=%+v err=%v", maintenance, err)
	}
	if _, err := st.MarkRAGDocumentDeleting(ctx, doc.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := st.IsRAGDocumentCleanupReady(ctx, doc.ID); err != nil || ready {
		t.Fatalf("cleanup crossed maintenance ready=%v err=%v", ready, err)
	}
	if err := st.DeleteRAGDocument(ctx, doc.ID); !errors.Is(err, ErrRAGCleanupNotReady) {
		t.Fatalf("finalizer crossed maintenance lease: %v", err)
	}
	if released, err := st.ReleaseRAGDocumentMaintenance(ctx, *maintenance); err != nil || !released {
		t.Fatalf("release maintenance=%v err=%v", released, err)
	}
	if err := st.DeleteRAGDocument(ctx, doc.ID); err != nil {
		t.Fatalf("finalize after maintenance release: %v", err)
	}
}
