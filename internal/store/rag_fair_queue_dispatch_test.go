package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRAGFairQueueTaskCreationWritesOwnerAndInitialDispatchEpoch(t *testing.T) {
	assertInitial := func(label string, task *RAGIndexTaskRecord, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if task.UserID != "u_claim" || task.DispatchGeneration != 1 ||
			task.ClaimGeneration != 0 || task.DispatchedAt != nil || task.Status != "PENDING" {
			t.Fatalf("%s task = %+v", label, task)
		}
	}
	t.Run("upload and reindex", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		doc, taskID := seedRAGTaskDocument(t, st, "doc_fair_create", 3)
		upload, err := st.GetRAGIndexTask(ctx, taskID)
		assertInitial("upload", upload, err)

		reindexed, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 1, testRAGVersion(doc.ID, 0))
		assertInitial("reindex", reindexed, err)
	})

	t.Run("provider replacement", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		providerDoc, providerTaskID := seedRAGTaskDocument(t, st, "doc_fair_provider", 3)
		claim, err := st.ClaimRAGIndexTask(ctx, "provider-worker", time.Minute)
		if err != nil || claim == nil || claim.Fence.TaskID != providerTaskID {
			t.Fatalf("provider claim = %+v, %v", claim, err)
		}
		replacement, changed, err := st.SupersedeRAGIndexTaskAndCreateVersion(
			ctx, claim.Fence, testRAGVersion(providerDoc.ID, 0),
		)
		if err != nil || !changed || replacement == nil {
			t.Fatalf("provider replacement = %+v changed=%v err=%v", replacement, changed, err)
		}
		assertInitial("provider replacement", replacement, nil)
	})
}

func TestRAGFairQueueLegacyClaimAndRetryDualWriteDispatchEpoch(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	_, taskID := seedRAGTaskDocument(t, st, "doc_fair_legacy_epoch", 3)

	first, err := st.ClaimRAGIndexTask(ctx, "legacy-first", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if first.Task.DispatchGeneration != 1 || first.Task.ClaimGeneration != 1 ||
		first.Task.DispatchedAt == nil {
		t.Fatalf("first claim task = %+v", first.Task)
	}
	if ok, err := st.RetryRAGIndexTask(ctx, first.Fence, "retry", 0); err != nil || !ok {
		t.Fatalf("retry = %v, %v", ok, err)
	}
	retrying, err := st.GetRAGIndexTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if retrying.Status != "PENDING" || retrying.DispatchGeneration != 2 ||
		retrying.ClaimGeneration != 1 || retrying.DispatchedAt != nil {
		t.Fatalf("retry task = %+v", retrying)
	}

	second, err := st.ClaimRAGIndexTask(ctx, "legacy-second", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("second claim = %+v, %v", second, err)
	}
	if second.Task.DispatchGeneration != 2 || second.Task.ClaimGeneration != 2 ||
		second.Task.DispatchedAt == nil {
		t.Fatalf("second claim task = %+v", second.Task)
	}
}

func TestRAGFairQueueLegacyClaimNormalizesMissingOwnerWithoutRegressingDispatchEpoch(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		userID string
	}{
		{name: "empty", userID: ""},
		{name: "whitespace", userID: "  \t"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			st := openRAGTaskClaimStore(t)
			ctx := context.Background()
			_, taskID := seedRAGTaskDocument(t, st, "doc_fair_legacy_owner_"+fixture.name, 3)
			if _, err := st.db.ExecContext(ctx, `UPDATE rag_index_tasks
				SET user_id=?,dispatch_generation=7,claim_generation=2,dispatched_at=NULL
				WHERE id=?`, fixture.userID, taskID); err != nil {
				t.Fatal(err)
			}

			claim, err := st.ClaimRAGIndexTask(ctx, "legacy-owner-worker", time.Minute)
			if err != nil || claim == nil || claim.Fence.TaskID != taskID {
				t.Fatalf("claim = %+v, %v", claim, err)
			}
			if claim.Task.UserID != "u_claim" || claim.Task.DispatchGeneration != 7 ||
				claim.Task.ClaimGeneration != 7 || claim.Task.DispatchedAt == nil {
				t.Fatalf("normalized claim task = %+v", claim.Task)
			}
		})
	}
}

func TestRAGFairQueueMarkDispatchedUsesCompleteGuard(t *testing.T) {
	mutations := []struct {
		name string
		sql  string
		args []any
	}{
		{name: "status", sql: `UPDATE rag_index_tasks SET status='RUNNING' WHERE id=?`},
		{name: "user", sql: `UPDATE rag_index_tasks SET user_id='u_other' WHERE id=?`},
		{name: "dispatch generation", sql: `UPDATE rag_index_tasks SET dispatch_generation=dispatch_generation+1 WHERE id=?`},
		{name: "claim generation", sql: `UPDATE rag_index_tasks SET claim_generation=claim_generation+1 WHERE id=?`},
		{name: "retry count", sql: `UPDATE rag_index_tasks SET retry_count=retry_count+1 WHERE id=?`},
		{name: "next run", sql: `UPDATE rag_index_tasks SET next_run_at='2099-01-01 00:00:00' WHERE id=?`},
		{name: "lease", sql: `UPDATE rag_index_tasks SET lease_until='2099-01-01 00:00:00' WHERE id=?`},
		{name: "marker", sql: `UPDATE rag_index_tasks SET dispatched_at=CURRENT_TIMESTAMP WHERE id=?`},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			st := openRAGTaskClaimStore(t)
			ctx := context.Background()
			_, taskID := seedRAGTaskDocument(t, st, "doc_fair_guard_"+fairQueueTestSlug(mutation.name), 3)
			candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			args := append(append([]any(nil), mutation.args...), taskID)
			if _, err := st.db.ExecContext(ctx, mutation.sql, args...); err != nil {
				t.Fatal(err)
			}
			changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate)
			if changed || !errors.Is(err, ErrRAGIndexTaskDispatchStale) {
				t.Fatalf("mark after %s = %v, %v; want stale", mutation.name, changed, err)
			}
		})
	}

	t.Run("success then duplicate", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_mark_success", 3)
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); err != nil || !changed {
			t.Fatalf("mark = %v, %v", changed, err)
		}
		task, err := st.GetRAGIndexTask(ctx, taskID)
		if err != nil || task.DispatchedAt == nil || task.DispatchGeneration != 1 {
			t.Fatalf("marked task = %+v, %v", task, err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); changed ||
			!errors.Is(err, ErrRAGIndexTaskDispatchStale) {
			t.Fatalf("duplicate mark = %v, %v", changed, err)
		}
	})
}

func TestRAGFairQueueTimestampGuardUsesExactDatabaseRepresentation(t *testing.T) {
	t.Run("sub-millisecond mutation is stale", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_sub_ms_guard", 3)
		original := "2000-01-01 00:00:00.123100"
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET next_run_at=? WHERE id=?`, original, taskID); err != nil {
			t.Fatal(err)
		}
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if candidate.Guard.NextRunAtRaw.IsNull || candidate.Guard.NextRunAtRaw.Raw != original {
			t.Fatalf("raw next_run_at = %v, want %q", candidate.Guard.NextRunAtRaw, original)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET next_run_at='2000-01-01 00:00:00.123200' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); changed ||
			!errors.Is(err, ErrRAGIndexTaskDispatchStale) {
			t.Fatalf("sub-millisecond stale mark = %v, %v", changed, err)
		}
	})

	t.Run("malformed value is not treated as null", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_malformed_guard", 3)
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if !candidate.Guard.NextRunAtRaw.IsNull || candidate.Guard.NextRunAtRaw.Raw != "" {
			t.Fatalf("NULL next_run_at raw = %v", candidate.Guard.NextRunAtRaw)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET next_run_at='' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); changed ||
			!errors.Is(err, ErrRAGIndexTaskDispatchStale) {
			t.Fatalf("NULL-to-malformed stale mark = %v, %v", changed, err)
		}
		var marker any
		if err := st.db.QueryRowContext(ctx,
			`SELECT dispatched_at FROM rag_index_tasks WHERE id=?`, taskID).Scan(&marker); err != nil {
			t.Fatal(err)
		}
		if marker != nil {
			t.Fatalf("malformed stale guard wrote marker %v", marker)
		}
		if _, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID); err == nil {
			t.Fatal("malformed timestamp unexpectedly produced a dispatch candidate")
		}
	})

	t.Run("JSON round trip preserves exact guard", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_json_guard", 3)
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET next_run_at='2000-01-01 00:00:00.123456789' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		var decoded RAGIndexTaskDispatchCandidate
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded.Guard.NextRunAtRaw != candidate.Guard.NextRunAtRaw {
			t.Fatalf("raw guard changed across JSON: before=%+v after=%+v",
				candidate.Guard.NextRunAtRaw, decoded.Guard.NextRunAtRaw)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, decoded); err != nil || !changed {
			t.Fatalf("mark after JSON round trip=%v, %v", changed, err)
		}
	})

	t.Run("broker marker sub-millisecond mutation is stale", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_broker_sub_ms_guard", 3)
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); err != nil || !changed {
			t.Fatalf("mark=%v, %v", changed, err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET dispatched_at='2000-01-01 00:00:00.123100' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		page, _, err := st.ListBrokerBackedRAGCandidatesPage(ctx, taskID, 0, 10)
		if err != nil || len(page) != 1 {
			t.Fatalf("broker page=%+v err=%v", page, err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET dispatched_at='2000-01-01 00:00:00.123200' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		if repaired, changed, err := st.RearmRAGCandidateAfterBrokerLoss(ctx, page[0]); err != nil || changed || repaired != nil {
			t.Fatalf("broker stale marker repair=%+v, %v, %v", repaired, changed, err)
		}
	})
}

func TestRAGFairQueueTimestampGuardDialects(t *testing.T) {
	tests := []struct {
		name, dialect, dsn string
	}{
		{name: "sqlite", dialect: "sqlite", dsn: "file:" + filepath.Join(t.TempDir(), "fair-guard.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"},
		{name: "postgres", dialect: "postgres", dsn: os.Getenv("BKCRAB_TEST_POSTGRES_DSN")},
		{name: "mysql", dialect: mysqlDialect, dsn: os.Getenv("BKCRAB_TEST_MYSQL_DSN")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.dsn == "" {
				t.Skip("BKCRAB_TEST_" + strings.ToUpper(test.name) + "_DSN is not set")
			}
			st, err := NewDBStore(test.dialect, test.dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			ctx := context.Background()
			if err := st.Migrate(ctx); err != nil {
				t.Fatal(err)
			}
			suffix := fairQueueTestSlug(test.name + fmt.Sprint(time.Now().UnixNano()))
			doc, taskID := seedRAGTaskDocument(t, st, "doc_fair_guard_"+suffix, 3)
			if test.dialect != "sqlite" {
				t.Cleanup(func() {
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					for _, cleanup := range []struct {
						query string
						arg   any
					}{
						{query: `DELETE FROM rag_index_tasks WHERE doc_id=?`, arg: doc.ID},
						{query: `DELETE FROM rag_document_versions WHERE doc_id=?`, arg: doc.ID},
						{query: `DELETE FROM rag_documents WHERE id=?`, arg: doc.ID},
						{query: `DELETE FROM rag_kbs WHERE id=?`, arg: doc.KBID},
					} {
						if _, err := st.db.ExecContext(cleanupCtx, cleanup.query, cleanup.arg); err != nil {
							t.Errorf("cleanup %s fixture: %v", test.name, err)
						}
					}
				})
			}
			original := time.Date(2000, 1, 1, 0, 0, 0, 123100000, time.UTC)
			if _, err := st.db.ExecContext(ctx, fmt.Sprintf(
				`UPDATE rag_index_tasks SET next_run_at=%s WHERE id=%s`, st.ph(1), st.ph(2)),
				original, taskID); err != nil {
				t.Fatal(err)
			}
			candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if candidate.Guard.NextRunAtRaw.IsNull || candidate.Guard.NextRunAtRaw.Raw == "" {
				t.Fatal("database timestamp representation is missing")
			}
			if _, err := st.db.ExecContext(ctx, fmt.Sprintf(
				`UPDATE rag_index_tasks SET next_run_at=%s WHERE id=%s`, st.ph(1), st.ph(2)),
				original.Add(100*time.Microsecond), taskID); err != nil {
				t.Fatal(err)
			}
			if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); changed ||
				!errors.Is(err, ErrRAGIndexTaskDispatchStale) {
				t.Fatalf("sub-millisecond stale mark = %v, %v", changed, err)
			}
		})
	}
}

func TestRAGFairQueueDispatchPageUsesStableIDCursorAndDatabaseDueTime(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	_, firstID := seedRAGTaskDocument(t, st, "doc_fair_page_1", 3)
	_, secondID := seedRAGTaskDocument(t, st, "doc_fair_page_2", 3)
	_, futureID := seedRAGTaskDocument(t, st, "doc_fair_page_future", 3)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET next_run_at='2099-01-01 00:00:00' WHERE id=?`, futureID); err != nil {
		t.Fatal(err)
	}

	page1, next, err := st.ListDispatchableRAGIndexTasksPage(ctx, 0, 1)
	if err != nil || len(page1) != 1 || page1[0].Task.ID != firstID || next != firstID {
		t.Fatalf("page1=%+v next=%d err=%v", page1, next, err)
	}
	page2, next, err := st.ListDispatchableRAGIndexTasksPage(ctx, next, 1)
	if err != nil || len(page2) != 1 || page2[0].Task.ID != secondID || next != secondID {
		t.Fatalf("page2=%+v next=%d err=%v", page2, next, err)
	}
	page3, next, err := st.ListDispatchableRAGIndexTasksPage(ctx, next, 1)
	if err != nil || len(page3) != 0 || next != futureID {
		t.Fatalf("page3=%+v next=%d err=%v", page3, next, err)
	}
	if _, err := st.GetDispatchableRAGIndexTaskByID(ctx, futureID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("future task lookup err=%v, want ErrNotFound", err)
	}
}

func TestRAGFairQueueFilteredPagesAdvanceAcrossEmptyRawIDWindows(t *testing.T) {
	t.Run("dispatch and expired rearm", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, futureID := seedRAGTaskDocument(t, st, "doc_fair_window_future", 3)
		_, dueID := seedRAGTaskDocument(t, st, "doc_fair_window_due", 3)
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET next_run_at='2099-01-01 00:00:00' WHERE id=?`, futureID); err != nil {
			t.Fatal(err)
		}
		page, next, err := st.ListDispatchableRAGIndexTasksPage(ctx, 0, 1)
		if err != nil || len(page) != 0 || next != futureID {
			t.Fatalf("empty dispatch window=%+v next=%d err=%v", page, next, err)
		}
		page, next, err = st.ListDispatchableRAGIndexTasksPage(ctx, next, 1)
		if err != nil || len(page) != 1 || page[0].Task.ID != dueID || next != dueID {
			t.Fatalf("next dispatch window=%+v next=%d err=%v", page, next, err)
		}

		st = openRAGTaskClaimStore(t)
		_, unexpiredID := seedRAGTaskDocument(t, st, "doc_fair_window_unexpired", 3)
		first, err := st.ClaimRAGIndexTask(ctx, "unexpired-worker", 10*time.Minute)
		if err != nil || first == nil || first.Task.ID != unexpiredID {
			t.Fatalf("first claim=%+v err=%v", first, err)
		}
		_, expiredID := seedRAGTaskDocument(t, st, "doc_fair_window_expired", 3)
		second, err := st.ClaimRAGIndexTask(ctx, "expired-worker", 10*time.Minute)
		if err != nil || second == nil || second.Task.ID != expiredID {
			t.Fatalf("second claim=%+v err=%v", second, err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET lease_until='2000-01-01 00:00:00' WHERE id=?`, expiredID); err != nil {
			t.Fatal(err)
		}
		armed, next, err := st.ArmExpiredRAGIndexTasksPage(ctx, 0, 1)
		if err != nil || len(armed) != 0 || next != unexpiredID {
			t.Fatalf("empty rearm window=%+v next=%d err=%v", armed, next, err)
		}
		armed, next, err = st.ArmExpiredRAGIndexTasksPage(ctx, next, 1)
		if err != nil || len(armed) != 1 || armed[0].Task.ID != expiredID || next != expiredID {
			t.Fatalf("next rearm window=%+v next=%d err=%v", armed, next, err)
		}
	})

	t.Run("high-water recovery and broker pages", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, terminalID := seedRAGTaskDocument(t, st, "doc_fair_window_terminal", 3)
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET status='DONE' WHERE id=?`, terminalID); err != nil {
			t.Fatal(err)
		}
		_, runningID := seedRAGTaskDocument(t, st, "doc_fair_window_running", 3)
		claim, err := st.ClaimRAGIndexTask(ctx, "window-running", 10*time.Minute)
		if err != nil || claim == nil || claim.Task.ID != runningID {
			t.Fatalf("running claim=%+v err=%v", claim, err)
		}
		_, brokerID := seedRAGTaskDocument(t, st, "doc_fair_window_broker", 3)
		broker, err := st.GetDispatchableRAGIndexTaskByID(ctx, brokerID)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *broker); err != nil || !changed {
			t.Fatalf("mark broker=%v err=%v", changed, err)
		}
		highWater, err := st.CaptureRAGFairQueueHighWater(ctx)
		if err != nil {
			t.Fatal(err)
		}

		dispatched, next, err := st.ListDispatchedRAGIndexTasksPage(ctx, highWater, 0, 1)
		if err != nil || len(dispatched) != 0 || next != terminalID {
			t.Fatalf("empty dispatched window=%+v next=%d err=%v", dispatched, next, err)
		}
		dispatched, next, err = st.ListDispatchedRAGIndexTasksPage(ctx, highWater, next, 1)
		if err != nil || len(dispatched) != 1 || dispatched[0].ID != runningID || next != runningID {
			t.Fatalf("next dispatched window=%+v next=%d err=%v", dispatched, next, err)
		}

		running, next, err := st.ListValidRunningRAGIndexTasksPage(ctx, highWater, 0, 1)
		if err != nil || len(running) != 0 || next != terminalID {
			t.Fatalf("empty running window=%+v next=%d err=%v", running, next, err)
		}
		running, next, err = st.ListValidRunningRAGIndexTasksPage(ctx, highWater, next, 1)
		if err != nil || len(running) != 1 || running[0].Task.ID != runningID || next != runningID {
			t.Fatalf("next running window=%+v next=%d err=%v", running, next, err)
		}

		brokerPage, next, err := st.ListBrokerBackedRAGCandidatesPage(ctx, highWater, 0, 1)
		if err != nil || len(brokerPage) != 0 || next != terminalID {
			t.Fatalf("empty broker window=%+v next=%d err=%v", brokerPage, next, err)
		}
		brokerPage, next, err = st.ListBrokerBackedRAGCandidatesPage(ctx, highWater, next, 1)
		if err != nil || len(brokerPage) != 0 || next != runningID {
			t.Fatalf("second empty broker window=%+v next=%d err=%v", brokerPage, next, err)
		}
		brokerPage, next, err = st.ListBrokerBackedRAGCandidatesPage(ctx, highWater, next, 1)
		if err != nil || len(brokerPage) != 1 || brokerPage[0].Task.ID != brokerID || next != brokerID {
			t.Fatalf("next broker window=%+v next=%d err=%v", brokerPage, next, err)
		}
	})

	t.Run("empty deleted high-water tail reaches barrier", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_window_deleted_tail", 3)
		highWater, err := st.CaptureRAGFairQueueHighWater(ctx)
		if err != nil || highWater != taskID {
			t.Fatalf("high water=%d err=%v", highWater, err)
		}
		if _, err := st.db.ExecContext(ctx, `DELETE FROM rag_index_tasks WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		page, next, err := st.ListDispatchedRAGIndexTasksPage(ctx, highWater, 0, 1)
		if err != nil || len(page) != 0 || next != highWater {
			t.Fatalf("deleted high-water tail=%+v next=%d err=%v", page, next, err)
		}
	})
}

func TestRAGFairQueueExpiredRearmKeepsRunningAndIsIdempotent(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	_, taskID := seedRAGTaskDocument(t, st, "doc_fair_rearm", 3)
	claim, err := st.ClaimRAGIndexTask(ctx, "legacy-rearm", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET lease_until='2000-01-01 00:00:00' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}

	armed, next, err := st.ArmExpiredRAGIndexTasksPage(ctx, 0, 10)
	if err != nil || len(armed) != 1 || next != taskID {
		t.Fatalf("armed=%+v next=%d err=%v", armed, next, err)
	}
	got := armed[0].Task
	if got.Status != "RUNNING" || got.DispatchGeneration != 2 || got.ClaimGeneration != 1 ||
		got.RetryCount != claim.Task.RetryCount || got.DocVersion != claim.Task.DocVersion ||
		got.DispatchedAt != nil {
		t.Fatalf("armed task = %+v", got)
	}
	if again, _, err := st.ArmExpiredRAGIndexTasksPage(ctx, 0, 10); err != nil || len(again) != 0 {
		t.Fatalf("second arm=%+v err=%v", again, err)
	}
	if changed, err := st.MarkRAGIndexTaskDispatched(ctx, armed[0]); err != nil || !changed {
		t.Fatalf("mark armed = %v, %v", changed, err)
	}
	if again, _, err := st.ArmExpiredRAGIndexTasksPage(ctx, 0, 10); err != nil || len(again) != 0 {
		t.Fatalf("arm published epoch=%+v err=%v", again, err)
	}
}

func TestRAGFairQueueBrokerRepairUsesHighWaterAndOriginalGuard(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	_, taskID := seedRAGTaskDocument(t, st, "doc_fair_broker_repair", 3)
	candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); err != nil || !changed {
		t.Fatalf("mark = %v, %v", changed, err)
	}
	highWater, err := st.CaptureRAGBrokerRepairHighWater(ctx)
	if err != nil || highWater != taskID {
		t.Fatalf("high water=%d err=%v", highWater, err)
	}

	_, laterID := seedRAGTaskDocument(t, st, "doc_fair_broker_later", 3)
	later, err := st.GetDispatchableRAGIndexTaskByID(ctx, laterID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *later); err != nil || !changed {
		t.Fatalf("mark later = %v, %v", changed, err)
	}

	page, next, err := st.ListBrokerBackedRAGCandidatesPage(ctx, highWater, 0, 10)
	if err != nil || len(page) != 1 || page[0].Task.ID != taskID || next != taskID {
		t.Fatalf("broker page=%+v next=%d err=%v", page, next, err)
	}
	rearmed, changed, err := st.RearmRAGCandidateAfterBrokerLoss(ctx, page[0])
	if err != nil || !changed || rearmed == nil {
		t.Fatalf("repair=%+v changed=%v err=%v", rearmed, changed, err)
	}
	if rearmed.Task.DispatchGeneration != 2 || rearmed.Task.ClaimGeneration != 0 ||
		rearmed.Task.DispatchedAt != nil {
		t.Fatalf("repaired task = %+v", rearmed.Task)
	}
	if duplicate, changed, err := st.RearmRAGCandidateAfterBrokerLoss(ctx, page[0]); err != nil || changed || duplicate != nil {
		t.Fatalf("duplicate repair=%+v changed=%v err=%v", duplicate, changed, err)
	}
}

func TestRAGFairQueueRecoveryPagesUseHighWaterAndCanonicalTenant(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	_, runningID := seedRAGTaskDocument(t, st, "doc_fair_recovery_running", 3)
	running, err := st.ClaimRAGIndexTask(ctx, "legacy-recovery", time.Minute)
	if err != nil || running == nil || running.Task.ID != runningID {
		t.Fatalf("running claim = %+v, %v", running, err)
	}
	_, pendingID := seedRAGTaskDocument(t, st, "doc_fair_recovery_pending", 3)
	pending, err := st.GetDispatchableRAGIndexTaskByID(ctx, pendingID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *pending); err != nil || !changed {
		t.Fatalf("mark pending = %v, %v", changed, err)
	}
	highWater, err := st.CaptureRAGFairQueueHighWater(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, laterID := seedRAGTaskDocument(t, st, "doc_fair_recovery_later", 3)

	tenants, nextTenant, err := st.ListCanonicalRAGTenantsPage(ctx, highWater, "", 10)
	if err != nil || len(tenants) != 1 || tenants[0] != "u_claim" || nextTenant != "u_claim" {
		t.Fatalf("tenants=%v next=%q err=%v", tenants, nextTenant, err)
	}
	dispatched, nextTask, err := st.ListDispatchedRAGIndexTasksPage(ctx, highWater, 0, 10)
	if err != nil || len(dispatched) != 2 || dispatched[0].ID != runningID ||
		dispatched[1].ID != pendingID || nextTask != pendingID {
		t.Fatalf("dispatched=%+v next=%d err=%v", dispatched, nextTask, err)
	}
	valid, nextTask, err := st.ListValidRunningRAGIndexTasksPage(ctx, highWater, 0, 10)
	if err != nil || len(valid) != 1 || valid[0].Task.ID != runningID || nextTask != pendingID ||
		valid[0].ObservedDBNow.IsZero() {
		t.Fatalf("valid=%+v next=%d err=%v", valid, nextTask, err)
	}
	for _, task := range dispatched {
		if task.ID == laterID {
			t.Fatalf("dispatched page crossed high water %d to task %d", highWater, laterID)
		}
	}
	for _, snapshot := range valid {
		if snapshot.Task.ID == laterID {
			t.Fatalf("running page crossed high water %d to task %d", highWater, laterID)
		}
	}
}

func TestRAGFairQueueCanonicalTenantPageAdvancesPastOwnersWithoutTasks(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	ownerIDs := []string{
		"u_raw_window_001",
		"u_raw_window_002",
		"u_raw_window_003",
	}
	for _, ownerID := range ownerIDs {
		ensureRAGLifecycleUser(t, st, ownerID, "active")
	}
	doc, highWater := seedRAGTaskDocument(t, st, "doc_fair_raw_owner", 3)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET user_id=? WHERE id=?`, ownerIDs[2], highWater); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_kbs SET user_id=? WHERE id=?`, ownerIDs[2], doc.KBID); err != nil {
		t.Fatal(err)
	}

	tenants, next, err := st.ListCanonicalRAGTenantsPage(
		ctx, highWater, "u_raw_window_000", 2,
	)
	if err != nil || len(tenants) != 0 || next != ownerIDs[1] {
		t.Fatalf("no-task page tenants=%v next=%q err=%v", tenants, next, err)
	}
	tenants, next, err = st.ListCanonicalRAGTenantsPage(ctx, highWater, next, 2)
	if err != nil || len(tenants) != 1 || tenants[0] != ownerIDs[2] || next != ownerIDs[2] {
		t.Fatalf("canonical page tenants=%v next=%q err=%v", tenants, next, err)
	}
}

func TestRAGFairQueueCanonicalTenantPageRejectsNonCanonicalFirstTask(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	ctx := context.Background()
	ownerID := "u_raw_corrupt_001"
	afterUserID := "u_raw_corrupt_000"
	ensureRAGLifecycleUser(t, st, ownerID, "active")

	_, firstTaskID := seedRAGTaskDocument(t, st, "doc_fair_raw_corrupt_first", 3)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET user_id=? WHERE id=?`, ownerID, firstTaskID); err != nil {
		t.Fatal(err)
	}
	laterDoc, highWater := seedRAGTaskDocument(t, st, "doc_fair_raw_corrupt_later", 3)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET user_id=? WHERE id=?`, ownerID, highWater); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_kbs SET user_id=? WHERE id=?`, ownerID, laterDoc.KBID); err != nil {
		t.Fatal(err)
	}

	tenants, next, err := st.ListCanonicalRAGTenantsPage(ctx, highWater, afterUserID, 1)
	if !errors.Is(err, ErrRAGFairQueueCanonicalOwner) || tenants != nil || next != afterUserID {
		t.Fatalf("corrupt first task tenants=%v next=%q err=%v", tenants, next, err)
	}
}

func TestRAGFairQueueValidRunningSnapshotsUseDatabaseClockAndRejectPseudoRunning(t *testing.T) {
	t.Run("database clock survives application skew and legacy null marker", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		_, taskID := seedRAGTaskDocument(t, st, "doc_fair_running_clock", 3)
		claim, err := st.ClaimRAGIndexTask(ctx, "clock-worker", 10*time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("claim=%+v err=%v", claim, err)
		}
		// Compat-era RUNNING rows may have no marker and are still authoritative
		// capacity reservations while their claim lease is valid.
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET dispatched_at=NULL WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		highWater, err := st.CaptureRAGFairQueueHighWater(ctx)
		if err != nil {
			t.Fatal(err)
		}
		snapshots, _, err := st.ListValidRunningRAGIndexTasksPage(ctx, highWater, 0, 10)
		if err != nil || len(snapshots) != 1 {
			t.Fatalf("snapshots=%+v err=%v", snapshots, err)
		}
		snapshot := snapshots[0]
		if snapshot.Task.ID != taskID || snapshot.ObservedDBNow.IsZero() ||
			snapshot.Task.LeaseUntil == nil || !snapshot.Task.LeaseUntil.After(snapshot.ObservedDBNow) {
			t.Fatalf("running snapshot=%+v", snapshot)
		}
		skewedApplicationNow := snapshot.ObservedDBNow.Add(24 * time.Hour)
		if snapshot.Task.LeaseUntil.After(skewedApplicationNow) {
			t.Fatalf("fixture does not demonstrate app clock skew: %+v", snapshot)
		}
	})

	mutations := []struct {
		name   string
		mutate func(*testing.T, *DBStore, *RAGDocumentRecord, int64)
	}{
		{name: "empty lease owner", mutate: func(t *testing.T, st *DBStore, _ *RAGDocumentRecord, taskID int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_index_tasks SET lease_owner='' WHERE id=?`, taskID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "zero claim generation", mutate: func(t *testing.T, st *DBStore, _ *RAGDocumentRecord, taskID int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_index_tasks SET dispatch_generation=0,claim_generation=0 WHERE id=?`, taskID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing heartbeat", mutate: func(t *testing.T, st *DBStore, _ *RAGDocumentRecord, taskID int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_index_tasks SET heartbeat_at=NULL WHERE id=?`, taskID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-null next run", mutate: func(t *testing.T, st *DBStore, _ *RAGDocumentRecord, taskID int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_index_tasks SET next_run_at='2000-01-01 00:00:00' WHERE id=?`, taskID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "non-running canonical version", mutate: func(t *testing.T, st *DBStore, doc *RAGDocumentRecord, _ int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_document_versions SET status='PENDING' WHERE doc_id=? AND doc_version=1`, doc.ID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "document version mismatch", mutate: func(t *testing.T, st *DBStore, doc *RAGDocumentRecord, _ int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_documents SET version=version+1 WHERE id=?`, doc.ID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "document status mismatch", mutate: func(t *testing.T, st *DBStore, doc *RAGDocumentRecord, _ int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_documents SET status='PENDING' WHERE id=?`, doc.ID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "inactive knowledge base", mutate: func(t *testing.T, st *DBStore, doc *RAGDocumentRecord, _ int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE rag_kbs SET status='inactive' WHERE id=?`, doc.KBID)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "inactive owner", mutate: func(t *testing.T, st *DBStore, _ *RAGDocumentRecord, _ int64) {
			_, err := st.db.ExecContext(context.Background(), `UPDATE users SET status='inactive' WHERE id='u_claim'`)
			if err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			st := openRAGTaskClaimStore(t)
			ctx := context.Background()
			doc, taskID := seedRAGTaskDocument(t, st,
				"doc_fair_pseudo_"+fairQueueTestSlug(mutation.name), 3)
			claim, err := st.ClaimRAGIndexTask(ctx, "pseudo-worker", 10*time.Minute)
			if err != nil || claim == nil {
				t.Fatalf("claim=%+v err=%v", claim, err)
			}
			mutation.mutate(t, st, doc, taskID)
			highWater, err := st.CaptureRAGFairQueueHighWater(ctx)
			if err != nil {
				t.Fatal(err)
			}
			snapshots, _, err := st.ListValidRunningRAGIndexTasksPage(ctx, highWater, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshots) != 0 {
				t.Fatalf("pseudo RUNNING restored: %+v", snapshots)
			}
		})
	}
}

func TestRAGFairQueueMutationCASRechecksCanonicalOwner(t *testing.T) {
	changeOwner := func(t *testing.T, st *DBStore, kbID string) {
		t.Helper()
		ensureRAGLifecycleUser(t, st, "u_fair_new_owner", "active")
		if _, err := st.db.ExecContext(context.Background(),
			`UPDATE rag_kbs SET user_id='u_fair_new_owner' WHERE id=?`, kbID); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("mark", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		doc, taskID := seedRAGTaskDocument(t, st, "doc_fair_owner_mark", 3)
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		changeOwner(t, st, doc.KBID)
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); changed ||
			!errors.Is(err, ErrRAGIndexTaskDispatchStale) {
			t.Fatalf("mark after owner move=%v, %v", changed, err)
		}
	})

	t.Run("expired rearm", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		doc, taskID := seedRAGTaskDocument(t, st, "doc_fair_owner_expired", 3)
		claim, err := st.ClaimRAGIndexTask(ctx, "owner-expired", time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("claim=%+v err=%v", claim, err)
		}
		if _, err := st.db.ExecContext(ctx,
			`UPDATE rag_index_tasks SET lease_until='2000-01-01 00:00:00' WHERE id=?`, taskID); err != nil {
			t.Fatal(err)
		}
		query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+st.ragFairQueueRawTimestampColumns("t")+
			ragFairQueueCanonicalJoin+` WHERE t.id=%s`, st.ph(1))
		candidate, err := st.scanRAGIndexTaskDispatchCandidate(st.db.QueryRowContext(ctx, query, taskID))
		if err != nil {
			t.Fatal(err)
		}
		changeOwner(t, st, doc.KBID)
		if armed, changed, err := st.armExpiredRAGIndexTask(ctx, *candidate); err != nil || changed || armed != nil {
			t.Fatalf("expired rearm after owner move=%+v, %v, %v", armed, changed, err)
		}
	})

	t.Run("broker repair", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		ctx := context.Background()
		doc, taskID := seedRAGTaskDocument(t, st, "doc_fair_owner_broker", 3)
		candidate, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := st.MarkRAGIndexTaskDispatched(ctx, *candidate); err != nil || !changed {
			t.Fatalf("mark=%v, %v", changed, err)
		}
		page, _, err := st.ListBrokerBackedRAGCandidatesPage(ctx, taskID, 0, 10)
		if err != nil || len(page) != 1 {
			t.Fatalf("broker page=%+v err=%v", page, err)
		}
		changeOwner(t, st, doc.KBID)
		if repaired, changed, err := st.RearmRAGCandidateAfterBrokerLoss(ctx, page[0]); err != nil || changed || repaired != nil {
			t.Fatalf("broker repair after owner move=%+v, %v, %v", repaired, changed, err)
		}
	})
}

func TestRAGFairQueueCanonicalTenantMySQLUsesBoundedTwoPhaseLookup(t *testing.T) {
	store := &DBStore{dialect: mysqlDialect}
	windowQuery, windowArgs := store.ragTenantOwnerWindowQuery("u_after", 8)
	for _, required := range []string{
		"SELECT id FROM users FORCE INDEX (PRIMARY)", "WHERE id>?", "ORDER BY id LIMIT ?",
	} {
		if !strings.Contains(windowQuery, required) {
			t.Fatalf("MySQL raw tenant window query missing %q:\n%s", required, windowQuery)
		}
	}
	if len(windowArgs) != 2 || windowArgs[0] != "u_after" || windowArgs[1] != 8 ||
		strings.Contains(windowQuery, "JOIN") || strings.Contains(windowQuery, "rag_index_tasks") ||
		strings.Contains(windowQuery, "DISTINCT") {
		t.Fatalf("MySQL raw tenant window query is not independently bounded: args=%v\n%s",
			windowArgs, windowQuery)
	}

	query, args := store.ragCanonicalTenantFirstTasksQuery(42, []string{"u_one", "u_two"})
	for _, required := range []string{
		"SELECT candidate_user.id,first_task.id,first_task.user_id,d.id,kb.id,kb.user_id",
		"FROM users candidate_user",
		"candidate_user.id IN (?,?)",
		"LEFT JOIN LATERAL (",
		"SELECT t.id FROM rag_index_tasks t FORCE INDEX (idx_rag_index_tasks_user_id)",
		"t.user_id=candidate_user.id AND t.id<=?",
		"ORDER BY t.user_id,t.id LIMIT 1",
		") first_owner_task ON TRUE",
		"LEFT JOIN rag_index_tasks first_task ON first_task.id=first_owner_task.id",
		"LEFT JOIN rag_documents d ON d.id=first_task.doc_id",
		"LEFT JOIN rag_kbs kb ON kb.id=d.kb_id",
		"ORDER BY candidate_user.id",
	} {
		if !strings.Contains(query, required) {
			t.Fatalf("MySQL canonical tenant filter query missing %q:\n%s", required, query)
		}
	}
	if len(args) != 3 || args[0] != int64(42) || args[1] != "u_one" || args[2] != "u_two" ||
		strings.Contains(query, "EXISTS") || strings.Contains(query, "GROUP BY") {
		t.Fatalf("MySQL canonical tenant filter must be bounded by the raw owner set: args=%v\n%s",
			args, query)
	}
}

func fairQueueTestSlug(value string) string {
	return fmt.Sprintf("%x", []byte(value))
}
