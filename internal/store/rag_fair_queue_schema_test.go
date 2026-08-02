package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
	"time"
)

func TestRAGFairQueueSQLiteFreshSchemaIsExpandCompatible(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()

	userID, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "user_id")
	if !ok || userID.notNull {
		t.Fatalf("fresh user_id = %+v, exists=%v; want nullable", userID, ok)
	}
	dispatchedAt, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "dispatched_at")
	if !ok || dispatchedAt.notNull {
		t.Fatalf("fresh dispatched_at = %+v, exists=%v; want nullable", dispatchedAt, ok)
	}
	dispatchGeneration, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "dispatch_generation")
	if !ok || !dispatchGeneration.notNull || !dispatchGeneration.defaultVal.Valid || dispatchGeneration.defaultVal.String != "1" {
		t.Fatalf("fresh dispatch_generation = %+v, exists=%v; want NOT NULL DEFAULT 1", dispatchGeneration, ok)
	}

	// An old binary omits all three expand columns. Its INSERT must remain valid
	// throughout the compatibility release, with the generation default arming
	// the first publication epoch and both nullable fields left uncontracted.
	result, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id,doc_version,status,retry_count,max_retry,claim_generation,lease_owner,error_msg,created_at)
		VALUES ('doc_old_writer',1,'PENDING',0,3,0,'','',CURRENT_TIMESTAMP)`)
	if err != nil {
		t.Fatalf("old-writer insert on fresh schema: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var gotUser sql.NullString
	var gotDispatched sql.NullTime
	var gotGeneration int64
	if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatched_at,dispatch_generation
		FROM rag_index_tasks WHERE id=?`, id).Scan(&gotUser, &gotDispatched, &gotGeneration); err != nil {
		t.Fatal(err)
	}
	if gotUser.Valid || gotDispatched.Valid || gotGeneration != 1 {
		t.Fatalf("old-writer defaults user=%v dispatched=%v generation=%d", gotUser, gotDispatched, gotGeneration)
	}

	for pass := 0; pass < 2; pass++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("idempotent fresh expand pass %d: %v", pass+1, err)
		}
	}
	userID, ok = ragTaskMigrationColumn(t, st, "rag_index_tasks", "user_id")
	if !ok || userID.notNull {
		t.Fatalf("startup migration contracted user_id: %+v, exists=%v", userID, ok)
	}
}

func TestBackfillRAGFairQueueTasksPageRejectsExhaustedGeneration(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	base := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)

	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_provider,embed_model,embed_dims,chunk_size,chunk_overlap,status,created_at,updated_at)
		VALUES ('kb_fq_exhausted','u_fq_exhausted','fair queue','','system','embed-v1',3,512,64,'active',?,?)`, base, base); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
		(id,kb_id,file_name,file_type,file_size,object_key,status,error_msg,chunk_count,token_count,version,uploaded_at)
		VALUES ('doc_fq_exhausted','kb_fq_exhausted','a.md','md',1,'rag/a.md','PENDING','',0,0,1,?)`, base); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id,doc_version,user_id,status,retry_count,max_retry,dispatch_generation,
		 claim_generation,lease_owner,error_msg,created_at)
		VALUES ('doc_fq_exhausted',1,'u_fq_exhausted','PENDING',0,3,1,?,'','',?)`,
		math.MaxInt64, base); err != nil {
		t.Fatal(err)
	}

	nextID, changed, done, err := st.backfillRAGFairQueueTasksPage(ctx, st.db, 0, 1)
	if !errors.Is(err, ErrRAGDispatchGenerationExhausted) || nextID <= 0 || changed != 0 || done {
		t.Fatalf("backfill exhausted generation next=%d changed=%d done=%v err=%v", nextID, changed, done, err)
	}
	var dispatchGeneration int64
	if err := st.db.QueryRowContext(ctx, `SELECT dispatch_generation FROM rag_index_tasks
		WHERE doc_id='doc_fq_exhausted'`).Scan(&dispatchGeneration); err != nil {
		t.Fatal(err)
	}
	if dispatchGeneration != 1 {
		t.Fatalf("overflowing backfill mutated dispatch_generation to %d", dispatchGeneration)
	}
}

func TestBackfillRAGFairQueueTasksPageUsesLastScannedIDAndIsIdempotent(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	base := time.Date(2026, 8, 2, 3, 4, 5, 0, time.UTC)

	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_provider,embed_model,embed_dims,chunk_size,chunk_overlap,status,created_at,updated_at)
		VALUES ('kb_fq_backfill','u_fq_owner','fair queue','','system','embed-v1',3,512,64,'active',?,?)`, base, base); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
		(id,kb_id,file_name,file_type,file_size,object_key,status,error_msg,chunk_count,token_count,version,uploaded_at)
		VALUES ('doc_fq_backfill','kb_fq_backfill','a.md','md',1,'rag/a.md','PENDING','',0,0,1,?)`, base); err != nil {
		t.Fatal(err)
	}

	insertTask := func(id int64, docID, status string, claim, dispatch int64, user any, marker any) {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
			(id,doc_id,doc_version,user_id,status,retry_count,max_retry,dispatch_generation,
			 claim_generation,dispatched_at,lease_owner,error_msg,created_at)
			VALUES (?,?,?,?,?,0,3,?,?,?,'','',?)`,
			id, docID, id, user, status, dispatch, claim, marker, base); err != nil {
			t.Fatalf("insert task %d: %v", id, err)
		}
	}
	// The first scanned row is deliberately unresolvable. A zero-change page
	// must still advance its cursor and must not claim to be done.
	insertTask(1, "missing_document", "PENDING", 0, 1, nil, nil)
	insertTask(2, "doc_fq_backfill", "DONE", 7, 1, nil, base)
	insertTask(3, "doc_fq_backfill", "PENDING", 3, 1, "wrong_owner", base)
	insertTask(4, "doc_fq_backfill", "RUNNING", 5, 9, nil, base)
	insertTask(5, "doc_fq_backfill", "FAILED", 2, 1, "u_fq_owner", nil)

	afterID := int64(0)
	wantChanged := []int64{0, 1, 1, 1, 0}
	for page, want := range wantChanged {
		nextID, changed, done, err := st.backfillRAGFairQueueTasksPage(ctx, st.db, afterID, 1)
		if err != nil {
			t.Fatalf("backfill page %d: %v", page+1, err)
		}
		if done || nextID <= afterID || changed != want {
			t.Fatalf("page %d next=%d changed=%d done=%v; after=%d wantChanged=%d",
				page+1, nextID, changed, done, afterID, want)
		}
		afterID = nextID
	}
	nextID, changed, done, err := st.backfillRAGFairQueueTasksPage(ctx, st.db, afterID, 1)
	if err != nil || !done || nextID != afterID || changed != 0 {
		t.Fatalf("empty terminal page next=%d changed=%d done=%v err=%v", nextID, changed, done, err)
	}

	assertTask := func(id int64, wantUser string, wantGeneration int64, wantMarker bool) {
		t.Helper()
		var user sql.NullString
		var marker sql.NullTime
		var generation int64
		if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatch_generation,dispatched_at
			FROM rag_index_tasks WHERE id=?`, id).Scan(&user, &generation, &marker); err != nil {
			t.Fatal(err)
		}
		if user.String != wantUser || user.Valid != (wantUser != "") || generation != wantGeneration || marker.Valid != wantMarker {
			t.Fatalf("task %d user=%v generation=%d marker=%v; want user=%q generation=%d marker=%v",
				id, user, generation, marker, wantUser, wantGeneration, wantMarker)
		}
	}
	assertTask(1, "", 1, false)
	assertTask(2, "u_fq_owner", 1, true) // terminal generation/marker stay untouched
	assertTask(3, "u_fq_owner", 4, false)
	assertTask(4, "u_fq_owner", 5, true) // legacy RUNNING marker remains sealed
	assertTask(5, "u_fq_owner", 1, false)

	// A complete second pass is a no-op. done is still reported only by the
	// first empty page after the last scanned row, never by a short/nonempty page.
	afterID = 0
	for {
		nextID, pageChanged, pageDone, pageErr := st.backfillRAGFairQueueTasksPage(ctx, st.db, afterID, 2)
		if pageErr != nil {
			t.Fatal(pageErr)
		}
		if pageChanged != 0 {
			t.Fatalf("idempotent pass changed %d row(s) after id %d", pageChanged, afterID)
		}
		if pageDone {
			break
		}
		if nextID <= afterID {
			t.Fatalf("idempotent pass cursor did not advance: %d -> %d", afterID, nextID)
		}
		afterID = nextID
	}
}

func TestSQLiteRAGIndexTaskRebuildPreservesFairQueueExpandColumns(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	marker := time.Date(2026, 8, 2, 6, 7, 8, 0, time.UTC)

	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(id,doc_id,doc_version,user_id,status,retry_count,max_retry,dispatch_generation,
		 claim_generation,dispatched_at,lease_owner,error_msg,created_at)
		VALUES (101,'doc_rebuild',9,'u_rebuild','RUNNING',1,3,12,11,?,'worker','','2026-08-02 06:00:00')`, marker); err != nil {
		t.Fatal(err)
	}
	if err := st.rebuildRAGIndexTasksSQLite(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	var user string
	var generation int64
	var gotMarker sql.NullTime
	if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatch_generation,dispatched_at
		FROM rag_index_tasks WHERE id=101`).Scan(&user, &generation, &gotMarker); err != nil {
		t.Fatal(err)
	}
	if user != "u_rebuild" || generation != 12 || !gotMarker.Valid || !gotMarker.Time.Equal(marker) {
		t.Fatalf("rebuild lost fair fields: user=%q generation=%d marker=%v", user, generation, gotMarker)
	}
}
