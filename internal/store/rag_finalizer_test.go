package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestRAGFinalizersRequireDurableDeletingTombstones(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	doc, _ := seedRAGTaskDocument(t, st, "doc_finalizer_tombstone", 3)
	if err := st.DeleteRAGDocument(ctx, doc.ID); !errors.Is(err, ErrRAGLifecycleInactive) {
		t.Fatalf("active document finalizer err=%v", err)
	}
	if _, err := st.GetRAGDocument(ctx, doc.ID); err != nil {
		t.Fatalf("active document was removed: %v", err)
	}
	if err := st.DeleteRAGKB(ctx, doc.KBID); !errors.Is(err, ErrRAGLifecycleInactive) {
		t.Fatalf("active KB finalizer err=%v", err)
	}
	if _, err := st.GetRAGKB(ctx, doc.KBID); err != nil {
		t.Fatalf("active KB was removed: %v", err)
	}
	if err := tombstoneAndDeleteRAGKBForTest(ctx, st, doc.KBID); err != nil {
		t.Fatalf("tombstoned KB finalizer: %v", err)
	}
}

func TestAppendRAGChatTurnRejectsInactiveKnowledgeBase(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_chat_inactive", "active")
	kb := &RAGKBRecord{
		ID: "kb_chat_inactive", UserID: "u_chat_inactive", Name: "inactive chat",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard,
		Status: "active", CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkRAGKBDeleting(ctx, kb.ID); err != nil {
		t.Fatal(err)
	}
	turn := &RAGChatTurnRecord{
		ID: "turn_after_tombstone", UserID: kb.UserID, KBID: kb.ID,
		SessionID: "session", Question: "q", Answer: "a",
	}
	if err := st.AppendRAGChatTurn(ctx, turn); !errors.Is(err, ErrRAGLifecycleInactive) {
		t.Fatalf("append after KB tombstone err=%v, want lifecycle inactive", err)
	}
	var count int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rag_chat_turns WHERE kb_id=?`, kb.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("chat turns after rejected append = %d, want 0", count)
	}
}

func TestDeleteRAGKBSerializesWithConcurrentChatAppend(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_chat_finalize", "active")

	for iteration := 0; iteration < 16; iteration++ {
		kbID := fmt.Sprintf("kb_chat_finalize_%02d", iteration)
		kb := &RAGKBRecord{
			ID: kbID, UserID: "u_chat_finalize", Name: "concurrent chat",
			EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
			ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard,
			Status: "active", CreatedAt: time.Now().UTC(),
		}
		if err := st.CreateRAGKB(ctx, kb); err != nil {
			t.Fatal(err)
		}
		turn := &RAGChatTurnRecord{
			ID: fmt.Sprintf("turn_finalize_%02d", iteration), UserID: kb.UserID,
			KBID: kb.ID, SessionID: "session", Question: "q", Answer: "a",
		}
		start := make(chan struct{})
		appendResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		go func() {
			<-start
			appendResult <- st.AppendRAGChatTurn(ctx, turn)
		}()
		go func() {
			<-start
			deleteResult <- tombstoneAndDeleteRAGKBForTest(ctx, st, kb.ID)
		}()
		close(start)

		appendErr := <-appendResult
		deleteErr := <-deleteResult
		if deleteErr != nil {
			t.Fatalf("iteration %d delete KB: %v", iteration, deleteErr)
		}
		if appendErr != nil && !errors.Is(appendErr, ErrRAGLifecycleInactive) {
			t.Fatalf("iteration %d append err=%v", iteration, appendErr)
		}
		var count int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM rag_chat_turns WHERE kb_id=?`, kb.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("iteration %d left %d orphan chat turns", iteration, count)
		}
	}
}

func TestDeleteRAGDocumentSerializesWithConcurrentAssetWrite(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()

	for iteration := 0; iteration < 12; iteration++ {
		docID := fmt.Sprintf("doc_finalize_%02d", iteration)
		doc, _ := seedRAGTaskDocument(t, st, docID, 3)
		asset := &RAGAssetRecord{
			ID: fmt.Sprintf("asset_finalize_%02d", iteration), DocID: doc.ID,
			ContentSHA256: fmt.Sprintf("%064x", iteration+1), SourceKind: "image",
			SourceMIME: "image/png", DisplayMIME: "image/png",
			SourceObjectKey: "source/" + doc.ID, DisplayObjectKey: "display/" + doc.ID,
			ThumbnailObjectKey: "thumb/" + doc.ID, DisplayStatus: "ready",
			DisplaySHA256:   "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			ThumbnailSHA256: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
			ByteSize:        1, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
		}
		start := make(chan struct{})
		writeResult := make(chan error, 1)
		deleteResult := make(chan error, 1)
		go func() {
			<-start
			writeResult <- st.UpsertRAGAsset(ctx, asset)
		}()
		go func() {
			<-start
			deleteResult <- tombstoneAndDeleteRAGDocumentForTest(ctx, st, doc.ID)
		}()
		close(start)

		writeErr := <-writeResult
		deleteErr := <-deleteResult
		if deleteErr != nil {
			t.Fatalf("iteration %d delete document: %v", iteration, deleteErr)
		}
		if writeErr != nil && !errors.Is(writeErr, ErrRAGLifecycleInactive) &&
			!errors.Is(writeErr, ErrNotFound) {
			t.Fatalf("iteration %d asset write err=%v", iteration, writeErr)
		}
		for _, table := range []string{"rag_documents", "rag_assets"} {
			var count int
			if err := st.db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM "+table+" WHERE "+map[string]string{
					"rag_documents": "id", "rag_assets": "doc_id",
				}[table]+"=?", doc.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("iteration %d left %d rows in %s", iteration, count, table)
			}
		}
	}
}

func TestDeleteRAGDocumentCleansDerivedRowsAndRetainsAuditLedgers(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	doc, taskID := seedRAGTaskDocument(t, st, "doc_finalizer_ledgers", 3)
	now := time.Now().UTC()
	asset := &RAGAssetRecord{
		ID: "asset_finalizer_ledgers", DocID: doc.ID,
		ContentSHA256: "1111111111111111111111111111111111111111111111111111111111111111",
		SourceKind:    "image", SourceMIME: "image/png", DisplayMIME: "image/png",
		SourceObjectKey: "source/ledger", DisplayObjectKey: "display/ledger",
		ThumbnailObjectKey: "thumb/ledger", DisplayStatus: "ready",
		DisplaySHA256:   "2222222222222222222222222222222222222222222222222222222222222222",
		ThumbnailSHA256: "3333333333333333333333333333333333333333333333333333333333333333",
		ByteSize:        1, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceRAGVersionAssets(ctx, doc.ID, 1, []string{asset.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_document_maintenance_leases
		(doc_id,generation,lease_owner,lease_until) VALUES (?,?,?,NULL)`, doc.ID, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID:       "u_claim",
		KBID:         doc.KBID,
		DocID:        doc.ID,
		ObjectKind:   RAGObjectKindParsedArtifact,
		ObjectKey:    "rag/u_claim/" + doc.KBID + "/" + doc.ID + "/staging/orphan.json",
		ReferenceKey: "orphan-finalizer-test",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_document_ai_task_budgets
		(task_id,user_id,max_requests,max_tokens,max_cost_microusd,charged_requests,
		 charged_tokens,charged_cost_microusd,updated_at) VALUES (?,?,?,?,?,?,?,?,?)`,
		taskID, "u_claim", 1, 1, 1, 0, 0, 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_document_ai_user_budgets
		(user_id,period_start_utc,charged_requests,charged_tokens,charged_cost_microusd,updated_at)
		VALUES (?,?,?,?,?,?)`, "u_claim", "2026-07-01", 0, 0, 0, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_document_ai_usage
		(idempotency_key,logical_request_key,user_id,doc_id,task_id,doc_version,
		 claim_generation,lease_owner,operation,provider_fingerprint,period_start_utc,
		 reserved_input_tokens,reserved_output_tokens,actual_input_tokens,actual_output_tokens,
		 estimated_cost_microusd,state,reservation_expires_at,sent_at,usage_estimated,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"1111111111111111111111111111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222222222222222222222222222",
		"u_claim", doc.ID, taskID, 1, 0, "worker", "vision",
		"3333333333333333333333333333333333333333333333333333333333333333",
		"2026-07-01", 1, 1, 0, 0, 1, "FINALIZED", nil, nil, false, now, now); err != nil {
		t.Fatal(err)
	}

	if err := tombstoneAndDeleteRAGDocumentForTest(ctx, st, doc.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"rag_documents", "rag_index_tasks", "rag_document_versions", "rag_assets",
		"rag_version_assets", "rag_document_maintenance_leases",
	} {
		var count int
		column := "doc_id"
		if table == "rag_documents" {
			column = "id"
		}
		if err := st.db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE "+column+"=?", doc.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("derived table %s retained %d rows", table, count)
		}
	}
	var stagingStatus string
	if err := st.db.QueryRowContext(ctx, `SELECT status FROM rag_object_write_staging
		WHERE doc_id=?`, doc.ID).Scan(&stagingStatus); err != nil || stagingStatus != ragObjectWriteWriting {
		t.Fatalf("unacknowledged object tombstone status=%q err=%v", stagingStatus, err)
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM rag_document_ai_task_budgets WHERE task_id=?`,
		`SELECT COUNT(*) FROM rag_document_ai_user_budgets WHERE user_id=?`,
		`SELECT COUNT(*) FROM rag_document_ai_usage WHERE doc_id=?`,
	} {
		argument := any(doc.ID)
		if query == `SELECT COUNT(*) FROM rag_document_ai_task_budgets WHERE task_id=?` {
			argument = taskID
		} else if query == `SELECT COUNT(*) FROM rag_document_ai_user_budgets WHERE user_id=?` {
			argument = "u_claim"
		}
		var count int
		if err := st.db.QueryRowContext(ctx, query, argument).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("audit ledger query %q count=%d, want 1", query, count)
		}
	}
}
