package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func seedLiveRAGIndexObjectFence(
	t *testing.T,
	docID string,
) (*DBStore, *RAGDocumentRecord, *RAGIndexClaim) {
	t.Helper()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, docID, 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), docID+"-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	return st, doc, claim
}

func ragIndexObjectWriteRequest(doc *RAGDocumentRecord) RAGObjectWriteRequest {
	return RAGObjectWriteRequest{
		UserID: "u_claim", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind:   RAGObjectKindAssetSource,
		ObjectKey:    fmt.Sprintf("rag/u_claim/%s/%s/assets/aa/source.png", doc.KBID, doc.ID),
		ReferenceKey: "asset-index-write",
	}
}

func ragIndexCacheRecord(doc *RAGDocumentRecord, fingerprint string) RAGCacheObjectRecord {
	key := strings.Repeat("a", 64)
	return RAGCacheObjectRecord{
		DocID: doc.ID, CacheKind: RAGCacheKindPage, CacheKey: key,
		ObjectKey:       fmt.Sprintf("rag/u_claim/%s/%s/cache/pages/%s.json", doc.KBID, doc.ID, key),
		FingerprintKind: RAGCacheFingerprintParse, Fingerprint: fingerprint,
	}
}

func TestRAGIndexObjectAndCacheTxCoresRequireLiveCanonicalIdentity(t *testing.T) {
	t.Run("legal live fence", func(t *testing.T) {
		st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_object_live")
		ctx := context.Background()
		request := ragIndexObjectWriteRequest(doc)
		var objectFence *RAGObjectWriteFence
		ok, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				var coreErr error
				objectFence, coreErr = st.beginRAGObjectWriteForIndexInTx(
					ctx, tx, locked, request,
				)
				return objectFence != nil, coreErr
			})
		if err != nil || !ok || objectFence == nil {
			t.Fatalf("begin live object write=%+v ok=%v err=%v", objectFence, ok, err)
		}

		ok, err = st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				return st.markRAGObjectWriteReadyForIndexInTx(
					ctx, tx, locked, *objectFence,
				)
			})
		if err != nil || !ok {
			t.Fatalf("mark live object ready ok=%v err=%v", ok, err)
		}

		record := ragIndexCacheRecord(doc, claim.Version.ParseFingerprint)
		ok, err = st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				if err := st.registerRAGCacheObjectForIndexInTx(
					ctx, tx, locked, record,
				); err != nil {
					return false, err
				}
				return true, nil
			})
		if err != nil || !ok {
			t.Fatalf("register live cache ok=%v err=%v", ok, err)
		}
	})

	for _, test := range []struct {
		name   string
		mutate func(*RAGObjectWriteRequest)
	}{
		{name: "wrong document", mutate: func(request *RAGObjectWriteRequest) {
			request.DocID = "doc_other"
			request.ObjectKey = fmt.Sprintf("rag/%s/%s/%s/assets/aa/source.png",
				request.UserID, request.KBID, request.DocID)
		}},
		{name: "wrong knowledge base", mutate: func(request *RAGObjectWriteRequest) {
			request.KBID = "kb_other"
			request.ObjectKey = fmt.Sprintf("rag/%s/%s/%s/assets/aa/source.png",
				request.UserID, request.KBID, request.DocID)
		}},
		{name: "wrong user", mutate: func(request *RAGObjectWriteRequest) {
			request.UserID = "u_other"
			request.ObjectKey = fmt.Sprintf("rag/%s/%s/%s/assets/aa/source.png",
				request.UserID, request.KBID, request.DocID)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_object_"+strings.ReplaceAll(test.name, " ", "_"))
			ctx := context.Background()
			request := ragIndexObjectWriteRequest(doc)
			test.mutate(&request)
			ok, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
				func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
					created, coreErr := st.beginRAGObjectWriteForIndexInTx(
						ctx, tx, locked, request,
					)
					return created != nil, coreErr
				})
			if ok || !errors.Is(err, ErrRAGDocumentVersionMismatch) {
				t.Fatalf("wrong identity begin ok=%v err=%v", ok, err)
			}
			var count int
			if err := st.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM rag_object_write_staging WHERE object_key=?`,
				request.ObjectKey).Scan(&count); err != nil || count != 0 {
				t.Fatalf("wrong identity staging count=%d err=%v", count, err)
			}
		})
	}

	t.Run("wrong cache route", func(t *testing.T) {
		st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_cache_wrong_route")
		ctx := context.Background()
		record := ragIndexCacheRecord(doc, claim.Version.ParseFingerprint)
		record.ObjectKey = strings.Replace(record.ObjectKey, "rag/u_claim/", "rag/u_other/", 1)
		ok, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				if err := st.registerRAGCacheObjectForIndexInTx(ctx, tx, locked, record); err != nil {
					return false, err
				}
				return true, nil
			})
		if ok || !errors.Is(err, ErrRAGDocumentVersionMismatch) {
			t.Fatalf("wrong cache route ok=%v err=%v", ok, err)
		}
		var count int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM rag_cache_objects WHERE doc_id=?`, doc.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("wrong route cache count=%d err=%v", count, err)
		}
	})

	t.Run("wrong ready fence cannot target another route", func(t *testing.T) {
		st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_ready_wrong_route")
		ctx := context.Background()
		request := ragIndexObjectWriteRequest(doc)
		var objectFence *RAGObjectWriteFence
		ok, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				var coreErr error
				objectFence, coreErr = st.beginRAGObjectWriteForIndexInTx(
					ctx, tx, locked, request,
				)
				return objectFence != nil, coreErr
			})
		if err != nil || !ok || objectFence == nil {
			t.Fatalf("seed object fence=%+v ok=%v err=%v", objectFence, ok, err)
		}
		wrong := *objectFence
		wrong.KBID = "kb_other"
		ok, err = st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				return st.markRAGObjectWriteReadyForIndexInTx(ctx, tx, locked, wrong)
			})
		if ok || !errors.Is(err, ErrRAGDocumentVersionMismatch) {
			t.Fatalf("wrong ready route ok=%v err=%v", ok, err)
		}
		var status string
		if err := st.db.QueryRowContext(ctx,
			`SELECT status FROM rag_object_write_staging WHERE handle_id=?`,
			objectFence.HandleID).Scan(&status); err != nil || status != ragObjectWriteWriting {
			t.Fatalf("wrong ready status=%q err=%v", status, err)
		}
	})

	t.Run("stale fence never enters write callback", func(t *testing.T) {
		st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_object_stale")
		ctx := context.Background()
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET lease_until=? WHERE id=?`,
			time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), claim.Fence.TaskID); err != nil {
			t.Fatal(err)
		}
		called := false
		ok, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
			func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
				called = true
				created, coreErr := st.beginRAGObjectWriteForIndexInTx(
					ctx, tx, locked, ragIndexObjectWriteRequest(doc),
				)
				return created != nil, coreErr
			})
		if err != nil || ok || called {
			t.Fatalf("stale fence ok=%v called=%v err=%v", ok, called, err)
		}
		var count int
		if err := st.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM rag_object_write_staging WHERE doc_id=?`, doc.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("stale staging count=%d err=%v", count, err)
		}
	})
}

func TestRAGIndexObjectAndCachePublicAPIsRejectMissingWriterBeforeMutation(t *testing.T) {
	st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_object_missing_writer")
	ctx := context.Background()
	request := ragIndexObjectWriteRequest(doc)
	if created, err := st.BeginRAGObjectWriteForIndex(ctx, claim.Fence, request); created != nil ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("begin without writer=%+v err=%v", created, err)
	}
	objectFence := RAGObjectWriteFence{
		HandleID: ragObjectWriteHandleID(request.ObjectKey), UserID: request.UserID,
		KBID: request.KBID, DocID: request.DocID, ObjectKind: request.ObjectKind,
		ObjectKey: request.ObjectKey, ReferenceKey: request.ReferenceKey,
		Generation: 1, Status: ragObjectWriteWriting,
	}
	if ready, err := st.MarkRAGObjectWriteReadyForIndex(ctx, claim.Fence, objectFence); ready ||
		!errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("mark without writer=%v err=%v", ready, err)
	}
	if err := st.RegisterRAGCacheObjectForIndex(
		ctx, claim.Fence, ragIndexCacheRecord(doc, claim.Version.ParseFingerprint),
	); !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("register without writer err=%v", err)
	}
	var staging, cache int
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rag_object_write_staging WHERE doc_id=?`, doc.ID).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rag_cache_objects WHERE doc_id=?`, doc.ID).Scan(&cache); err != nil {
		t.Fatal(err)
	}
	if staging != 0 || cache != 0 {
		t.Fatalf("missing-writer mutation staging=%d cache=%d", staging, cache)
	}
}

func TestRAGObjectAndCacheLegacyAPIsRemainAvailable(t *testing.T) {
	st, doc, claim := seedLiveRAGIndexObjectFence(t, "doc_index_object_legacy")
	ctx := context.Background()
	request := ragIndexObjectWriteRequest(doc)
	objectFence, err := st.BeginRAGObjectWrite(ctx, request)
	if err != nil || objectFence == nil {
		t.Fatalf("legacy begin=%+v err=%v", objectFence, err)
	}
	if ready, err := st.MarkRAGObjectWriteReady(ctx, *objectFence); err != nil || !ready {
		t.Fatalf("legacy mark=%v err=%v", ready, err)
	}
	if err := st.RegisterRAGCacheObject(
		ctx, ragIndexCacheRecord(doc, claim.Version.ParseFingerprint),
	); err != nil {
		t.Fatalf("legacy cache register err=%v", err)
	}
}
