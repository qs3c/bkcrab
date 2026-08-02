package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testRAGFairQueueWriterFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func claimRAGFairQueueCoreForTest(
	t *testing.T,
	st *DBStore,
	taskID int64,
	expectedUserID string,
	expectedGeneration int64,
	workerID string,
	limits RAGFairQueueClaimLimits,
) RAGFairQueueClaimResult {
	t.Helper()
	ctx := context.Background()
	task, err := st.GetRAGIndexTask(ctx, taskID)
	if err != nil {
		t.Fatalf("get task %d: %v", taskID, err)
	}
	route, routeErr := st.ragOwnershipRoute(ctx, task.DocID)
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin exact-claim test transaction: %v", err)
	}
	defer tx.Rollback()
	result, _, err := st.claimRAGIndexTaskIDInTx(ctx, tx, task.ID, task.DocID, route, routeErr,
		workerID, time.Minute, ragIndexClaimMode{
			expectedUserID: expectedUserID, expectedDispatchGeneration: expectedGeneration,
			expectedWriterFingerprint: testRAGFairQueueWriterFingerprint, limits: &limits,
		})
	if err != nil {
		t.Fatalf("claim exact task %d: %v", taskID, err)
	}
	return result
}

func TestRAGFairQueueCapacityLockNameIsBoundedAndDomainSeparated(t *testing.T) {
	first := fairQueueCapacityLockName("bkcrab", "rag.index")
	second := fairQueueCapacityLockName("bkcrab2", "rag.index")
	otherResource := fairQueueCapacityLockName("bkcrab", "image.generate")
	if len(first) != 58 || !strings.HasPrefix(first, "bkcrab:fq:") {
		t.Fatalf("capacity lock name = %q (len=%d)", first, len(first))
	}
	if first == second || first == otherResource || first == fairQueueOperationStartLockName("bkcrab", "rag.index") {
		t.Fatalf("capacity lock names are not domain separated: %q %q %q", first, second, otherResource)
	}
}

func TestRAGFairQueueExactClaimSelectsOnlyDeliveredTaskAndCarriesWriterFence(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, firstID := seedRAGTaskDocument(t, st, "doc_exact_first", 2)
	_, secondID := seedRAGTaskDocument(t, st, "doc_exact_second", 2)

	result := claimRAGFairQueueCoreForTest(t, st, secondID, "u_claim", 1, "fair-worker", RAGFairQueueClaimLimits{
		GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
	})
	if result.Disposition != RAGFairQueueClaimed || result.Claim == nil {
		t.Fatalf("exact claim result = %+v", result)
	}
	if result.Claim.Fence.TaskID != secondID ||
		result.Claim.Fence.ExpectedWriterFingerprint != testRAGFairQueueWriterFingerprint {
		t.Fatalf("exact claim fence = %+v", result.Claim.Fence)
	}
	first, err := st.GetRAGIndexTask(context.Background(), firstID)
	if err != nil || first.Status != "PENDING" || first.ClaimGeneration != 0 {
		t.Fatalf("undelivered task changed: task=%+v err=%v", first, err)
	}
}

func TestRAGFairQueueExactClaimRejectsStaleGenerationWithoutMutation(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, taskID := seedRAGTaskDocument(t, st, "doc_exact_stale", 2)
	result := claimRAGFairQueueCoreForTest(t, st, taskID, "u_claim", 2, "fair-worker", RAGFairQueueClaimLimits{
		GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
	})
	if result.Disposition != RAGFairQueueClaimDuplicateStale || result.Claim != nil {
		t.Fatalf("stale exact claim result = %+v", result)
	}
	task, err := st.GetRAGIndexTask(context.Background(), taskID)
	if err != nil || task.Status != "PENDING" || task.DispatchGeneration != 1 || task.ClaimGeneration != 0 {
		t.Fatalf("stale delivery mutated task: task=%+v err=%v", task, err)
	}
}

func TestRAGFairQueueExactClaimHonorsDueAndRearmEpochs(t *testing.T) {
	t.Run("future pending is stale", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		_, taskID := seedRAGTaskDocument(t, st, "doc_exact_future", 2)
		future := time.Now().UTC().Add(time.Hour)
		if _, err := st.db.ExecContext(context.Background(),
			`UPDATE rag_index_tasks SET next_run_at=? WHERE id=?`, future, taskID); err != nil {
			t.Fatal(err)
		}
		result := claimRAGFairQueueCoreForTest(t, st, taskID, "u_claim", 1, "fair-worker", RAGFairQueueClaimLimits{
			GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
		})
		if result.Disposition != RAGFairQueueClaimDuplicateStale || result.Claim != nil {
			t.Fatalf("future task claim result=%+v", result)
		}
	})

	t.Run("expired running requires sweeper rearm", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		_, taskID := seedRAGTaskDocument(t, st, "doc_exact_rearm", 3)
		first := claimRAGFairQueueCoreForTest(t, st, taskID, "u_claim", 1, "fair-worker-1", RAGFairQueueClaimLimits{
			GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
		})
		if first.Claim == nil {
			t.Fatalf("first claim=%+v", first)
		}
		if _, err := st.db.ExecContext(context.Background(),
			`UPDATE rag_index_tasks SET lease_until=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), taskID); err != nil {
			t.Fatal(err)
		}
		stale := claimRAGFairQueueCoreForTest(t, st, taskID, "u_claim", 1, "fair-worker-2", RAGFairQueueClaimLimits{
			GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
		})
		if stale.Disposition != RAGFairQueueClaimDuplicateStale || stale.Claim != nil {
			t.Fatalf("unarmed expired claim=%+v", stale)
		}
		armed, _, err := st.ArmExpiredRAGIndexTasksPage(context.Background(), 0, 100)
		if err != nil || len(armed) != 1 || armed[0].Task.DispatchGeneration != 2 {
			t.Fatalf("armed=%+v err=%v", armed, err)
		}
		reclaimed := claimRAGFairQueueCoreForTest(t, st, taskID, "u_claim", 2, "fair-worker-2", RAGFairQueueClaimLimits{
			GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
		})
		if reclaimed.Disposition != RAGFairQueueClaimed || reclaimed.Claim == nil ||
			reclaimed.Claim.Fence.ClaimGeneration != 2 {
			t.Fatalf("reclaimed=%+v", reclaimed)
		}
	})
}

func TestRAGFairQueueExactClaimRepairsActiveMaintenanceDurably(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, taskID := seedRAGTaskDocument(t, st, "doc_exact_maintenance", 2)
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(context.Background(), `INSERT INTO rag_document_maintenance_leases
		(doc_id,generation,lease_owner,lease_until) VALUES (?,?,?,?)`,
		doc.ID, 1, "maintenance-worker", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	result := claimRAGFairQueueCoreForTest(t, st, taskID, "u_claim", 1, "fair-worker", RAGFairQueueClaimLimits{
		GlobalConcurrency: 4, PerUserBurstConcurrency: 4, MaintenanceRetryDelay: 2 * time.Minute,
	})
	if result.Disposition != RAGFairQueueClaimCanonicalRetry || result.Claim != nil {
		t.Fatalf("maintenance result=%+v", result)
	}
	task, err := st.GetRAGIndexTask(context.Background(), taskID)
	if err != nil || task.Status != "PENDING" || task.DispatchGeneration != 2 ||
		task.ClaimGeneration != 0 || task.DispatchedAt != nil || task.NextRunAt == nil ||
		!task.NextRunAt.After(now.Add(time.Minute)) {
		t.Fatalf("maintenance-repaired task=%+v err=%v", task, err)
	}
}

func TestRAGFairQueueExactClaimRejectsTenantPoisonWithoutMutation(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, taskID := seedRAGTaskDocument(t, st, "doc_exact_poison", 2)
	result := claimRAGFairQueueCoreForTest(t, st, taskID, "u_tampered", 1, "fair-worker", RAGFairQueueClaimLimits{
		GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
	})
	if result.Disposition != RAGFairQueueClaimPoison || result.Claim != nil {
		t.Fatalf("poison exact claim result = %+v", result)
	}
	task, err := st.GetRAGIndexTask(context.Background(), taskID)
	if err != nil || task.Status != "PENDING" || task.UserID != "u_claim" ||
		task.DispatchGeneration != 1 || task.ClaimGeneration != 0 || task.DispatchedAt != nil {
		t.Fatalf("poison delivery mutated task: task=%+v err=%v", task, err)
	}
}

func TestRAGFairQueuePoisonRepairUsesCapturedCanonicalGuardOnce(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, taskID := seedRAGTaskDocument(t, st, "doc_poison_repair", 2)
	ctx := context.Background()

	dispatchable, err := st.GetDispatchableRAGIndexTaskByID(ctx, taskID)
	if err != nil {
		t.Fatalf("get dispatchable poison candidate: %v", err)
	}
	if marked, err := st.MarkRAGIndexTaskDispatched(ctx, *dispatchable); err != nil || !marked {
		t.Fatalf("mark poison candidate dispatched: marked=%v err=%v", marked, err)
	}

	first, err := st.getRAGPoisonRepairCandidateOn(ctx, st.db, taskID, 1)
	if err != nil {
		t.Fatalf("capture first poison candidate: %v", err)
	}
	second := *first
	if first.Guard.DispatchedAt == nil {
		t.Fatal("poison candidate must snapshot the current publish marker")
	}
	if first.Task.UserID != "u_claim" {
		t.Fatalf("poison candidate canonical user=%q", first.Task.UserID)
	}
	if candidate, err := st.getRAGPoisonRepairCandidateOn(ctx, st.db, taskID, 2); !errors.Is(err, ErrNotFound) || candidate != nil {
		t.Fatalf("stale generation located canonical task: candidate=%+v err=%v", candidate, err)
	}

	updated, changed, err := st.rearmRAGPoisonCandidateOn(ctx, st.db, *first)
	if err != nil || !changed || updated == nil || updated.Task.DispatchGeneration != 2 || updated.Task.DispatchedAt != nil {
		t.Fatalf("first poison repair: updated=%+v changed=%v err=%v", updated, changed, err)
	}
	if updated, changed, err := st.rearmRAGPoisonCandidateOn(ctx, st.db, second); err != nil || changed || updated != nil {
		t.Fatalf("stale duplicate poison repair advanced twice: updated=%+v changed=%v err=%v", updated, changed, err)
	}
	task, err := st.GetRAGIndexTask(ctx, taskID)
	if err != nil || task.Status != "PENDING" || task.DispatchGeneration != 2 ||
		task.ClaimGeneration != 0 || task.DispatchedAt != nil {
		t.Fatalf("poison-repaired task = %+v err=%v", task, err)
	}
}

func TestRAGFairQueueExactClaimCapacityDeferredLeavesTaskPending(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, firstID := seedRAGTaskDocument(t, st, "doc_capacity_first", 2)
	first := claimRAGFairQueueCoreForTest(t, st, firstID, "u_claim", 1, "fair-worker-1", RAGFairQueueClaimLimits{
		GlobalConcurrency: 1, PerUserBurstConcurrency: 1,
	})
	if first.Disposition != RAGFairQueueClaimed {
		t.Fatalf("first capacity claim = %+v", first)
	}
	_, secondID := seedRAGTaskDocument(t, st, "doc_capacity_second", 2)
	second := claimRAGFairQueueCoreForTest(t, st, secondID, "u_claim", 1, "fair-worker-2", RAGFairQueueClaimLimits{
		GlobalConcurrency: 1, PerUserBurstConcurrency: 1,
	})
	if second.Disposition != RAGFairQueueClaimCapacityDeferred || second.Claim != nil {
		t.Fatalf("capacity-deferred result = %+v", second)
	}
	task, err := st.GetRAGIndexTask(context.Background(), secondID)
	if err != nil || task.Status != "PENDING" || task.ClaimGeneration != 0 {
		t.Fatalf("capacity-deferred task changed: task=%+v err=%v", task, err)
	}
}

func TestRAGFairQueueWriterBindingRequiresMySQLAndCanonicalFingerprint(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	if _, err := st.BindRAGFairQueueWriter(testRAGFairQueueWriterFingerprint); !errors.Is(err, ErrFairQueueMySQLRequired) {
		t.Fatalf("bind SQLite fair writer error = %v", err)
	}
	if _, err := st.BindRAGFairQueueWriter("not-a-fingerprint"); !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("bind invalid fingerprint error = %v", err)
	}
}
