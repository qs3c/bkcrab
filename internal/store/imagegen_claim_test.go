package store

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func imagegenBoundFairStore(t *testing.T, st *DBStore) *ImageFairQueueStore {
	t.Helper()
	identity, err := st.ReadFairQueueWriterIdentity(context.Background())
	if err != nil {
		t.Fatalf("read writer identity: %v", err)
	}
	fair, err := st.BindImageFairQueueWriter(identity.Fingerprint)
	if err != nil {
		t.Fatalf("bind image fair store: %v", err)
	}
	return fair
}

func imagegenClaimLimits() ImageGenerationClaimLimits {
	return ImageGenerationClaimLimits{
		GlobalConcurrency: 4, PerUserBurstConcurrency: 4,
		AdvisoryLockTimeout: time.Second,
	}
}

func TestImageGenerationExactClaimGenerationTenantAndDuplicate(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	fair := imagegenBoundFairStore(t, st)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	taskID := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID) })
	_, _, err := st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(batchID, taskID))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := st.GetDispatchableImageTaskByID(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkImageTaskDispatched(ctx, *candidate, 1); err != nil || !changed {
		t.Fatalf("mark dispatched changed=%v err=%v", changed, err)
	}

	for _, delivery := range []struct {
		user       string
		generation int64
	}{{"other-user", 1}, {"user-a", 2}} {
		result, err := fair.ClaimImageGenerationTaskByID(ctx, taskID, delivery.user, delivery.generation, "worker-a", time.Minute, imagegenClaimLimits())
		if err != nil || result.Disposition != ImageGenerationClaimDuplicateStale || result.Claim != nil {
			t.Fatalf("stale delivery %+v result=%+v err=%v", delivery, result, err)
		}
	}

	result, err := fair.ClaimImageGenerationTaskByID(ctx, taskID, "user-a", 1, "worker-a", time.Minute, imagegenClaimLimits())
	if err != nil || result.Disposition != ImageGenerationClaimed || result.Claim == nil {
		t.Fatalf("claim result=%+v err=%v", result, err)
	}
	if result.Claim.PreviousClaimGeneration != 0 || result.Claim.Fence.ClaimGeneration != 1 ||
		result.Claim.Fence.ExpectedWriterFingerprint != fair.ExpectedWriterFingerprint() ||
		result.Claim.Task.DispatchGeneration != 1 || result.Claim.Task.ClaimGeneration != 1 {
		t.Fatalf("claim fence/result=%+v", result.Claim)
	}
	duplicate, err := fair.ClaimImageGenerationTaskByID(ctx, taskID, "user-a", 1, "worker-b", time.Minute, imagegenClaimLimits())
	if err != nil || duplicate.Disposition != ImageGenerationClaimDuplicateStale || duplicate.Claim != nil {
		t.Fatalf("duplicate result=%+v err=%v", duplicate, err)
	}
}

func TestImageGenerationClaimRequiresSweeperGenerationAndReturnsPrevious(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	fair := imagegenBoundFairStore(t, st)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	taskID := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID) })
	_, _, err := st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(batchID, taskID))
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if _, err := st.db.Exec(`UPDATE image_generation_tasks SET status='RUNNING',retry_count=1,
		claim_generation=3,dispatch_generation=3,lease_owner='old-worker',lease_until=?,heartbeat_at=?,dispatched_at=? WHERE id=?`, past, past, past, taskID); err != nil {
		t.Fatal(err)
	}
	old, err := fair.ClaimImageGenerationTaskByID(ctx, taskID, "user-a", 3, "worker-new", time.Minute, imagegenClaimLimits())
	if err != nil || old.Disposition != ImageGenerationClaimDuplicateStale {
		t.Fatalf("unswept expired claim=%+v err=%v", old, err)
	}
	if _, err := st.db.Exec(`UPDATE image_generation_tasks SET dispatch_generation=7,dispatched_at=UTC_TIMESTAMP(6) WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := fair.ClaimImageGenerationTaskByID(ctx, taskID, "user-a", 7, "worker-new", time.Minute, imagegenClaimLimits())
	if err != nil || reclaimed.Disposition != ImageGenerationClaimed || reclaimed.Claim == nil {
		t.Fatalf("reclaim=%+v err=%v", reclaimed, err)
	}
	if reclaimed.Claim.PreviousClaimGeneration != 3 || reclaimed.Claim.Fence.ClaimGeneration != 7 {
		t.Fatalf("generation jump=%+v", reclaimed.Claim)
	}
}

func TestImageGenerationClaimCapacityIsAuthoritativeAcrossStores(t *testing.T) {
	first := openImagegenMySQLTestStore(t)
	second := openImagegenMySQLTestStore(t)
	fairA := imagegenBoundFairStore(t, first)
	identity := fairA.ExpectedWriterFingerprint()
	fairB, err := second.BindImageFairQueueWriter(identity)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	var taskIDs []string
	for i := 0; i < 8; i++ {
		taskIDs = append(taskIDs, imagegenTestID(t, "imgt_"))
	}
	t.Cleanup(func() { cleanupImageBatch(t, first, batchID) })
	request := testCreateImageBatchRequest(batchID, taskIDs...)
	request.RequestedCount = len(taskIDs)
	for i := range request.Tasks {
		request.Tasks[i].RequestedCount = 1
		request.Tasks[i].RequestFingerprint = strings.Repeat("a", 64)
	}
	_, _, err = first.CreateImageGenerationBatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, taskID := range taskIDs {
		candidate, getErr := first.GetDispatchableImageTaskByID(ctx, taskID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if changed, markErr := first.MarkImageTaskDispatched(ctx, *candidate, 1); markErr != nil || !changed {
			t.Fatalf("mark %s changed=%v err=%v", taskID, changed, markErr)
		}
	}
	limits := imagegenClaimLimits()
	var wg sync.WaitGroup
	dispositions := make(chan ImageGenerationClaimDisposition, len(taskIDs))
	errs := make(chan error, len(taskIDs))
	for i, taskID := range taskIDs {
		wg.Add(1)
		go func(index int, id string) {
			defer wg.Done()
			fair := fairA
			if index%2 == 1 {
				fair = fairB
			}
			result, claimErr := fair.ClaimImageGenerationTaskByID(ctx, id, "user-a", 1, "worker-"+id, time.Minute, limits)
			if claimErr != nil {
				errs <- claimErr
				return
			}
			dispositions <- result.Disposition
		}(i, taskID)
	}
	wg.Wait()
	close(errs)
	for claimErr := range errs {
		t.Fatal(claimErr)
	}
	close(dispositions)
	claimed, deferred := 0, 0
	for disposition := range dispositions {
		switch disposition {
		case ImageGenerationClaimed:
			claimed++
		case ImageGenerationClaimCapacityDeferred:
			deferred++
		default:
			t.Fatalf("unexpected disposition %q", disposition)
		}
	}
	if claimed != 4 || deferred != 4 {
		t.Fatalf("capacity claimed=%d deferred=%d", claimed, deferred)
	}
}

func TestImageGenerationHeartbeatRetryAndLateFinalizeFences(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	fair := imagegenBoundFairStore(t, st)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	taskID := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID) })
	_, _, err := st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(batchID, taskID))
	if err != nil {
		t.Fatal(err)
	}
	candidate, _ := st.GetDispatchableImageTaskByID(ctx, taskID)
	_, _ = st.MarkImageTaskDispatched(ctx, *candidate, 1)
	result, err := fair.ClaimImageGenerationTaskByID(ctx, taskID, "user-a", 1, "worker-a", time.Minute, imagegenClaimLimits())
	if err != nil || result.Claim == nil {
		t.Fatalf("claim=%+v err=%v", result, err)
	}
	fence := result.Claim.Fence
	if disposition, err := st.HeartbeatImageGenerationTask(ctx, fence, time.Minute); err != nil || disposition != ImageGenerationHeartbeatExtended {
		t.Fatalf("heartbeat=%s err=%v", disposition, err)
	}
	nextRun := time.Now().UTC().Add(time.Minute)
	if changed, err := st.FinishImageGenerationTaskRetry(ctx, fence, "UPSTREAM_TRANSIENT", nextRun); err != nil || !changed {
		t.Fatalf("retry changed=%v err=%v", changed, err)
	}
	if changed, err := st.FinishImageGenerationTaskFailed(ctx, fence, "LATE"); err != nil || changed {
		t.Fatalf("late finalize changed=%v err=%v", changed, err)
	}
	task, err := st.GetImageGenerationTask(ctx, taskID)
	if err != nil || task.Status != ImageGenerationTaskPending || task.RetryCount != 1 ||
		task.DispatchGeneration != 2 || task.ClaimGeneration != 1 || task.DispatchedAt != nil {
		t.Fatalf("retried task=%+v err=%v", task, err)
	}
}

func TestImageGenerationHeartbeatObservesCancelAndWriterMismatch(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	fair := imagegenBoundFairStore(t, st)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	taskID := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID) })
	_, _, _ = st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(batchID, taskID))
	candidate, _ := st.GetDispatchableImageTaskByID(ctx, taskID)
	_, _ = st.MarkImageTaskDispatched(ctx, *candidate, 1)
	result, _ := fair.ClaimImageGenerationTaskByID(ctx, taskID, "user-a", 1, "worker-a", time.Minute, imagegenClaimLimits())
	_, _, _ = st.RequestImageBatchCancel(ctx, "user-a", "agent-a", batchID)
	disposition, err := st.HeartbeatImageGenerationTask(ctx, result.Claim.Fence, time.Minute)
	if err != nil || disposition != ImageGenerationHeartbeatCanceled {
		t.Fatalf("cancel heartbeat=%s err=%v", disposition, err)
	}
	bad := result.Claim.Fence
	bad.ExpectedWriterFingerprint = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if _, err := st.HeartbeatImageGenerationTask(ctx, bad, time.Minute); !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("writer mismatch err=%v", err)
	}
}

func TestImageGenerationExpiredSweepRearmsOnceAndFinalizesCanceled(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	fair := imagegenBoundFairStore(t, st)
	ctx := context.Background()
	rearmBatch := imagegenTestID(t, "imgb_")
	rearmTask := imagegenTestID(t, "imgt_")
	cancelBatch := imagegenTestID(t, "imgb_")
	cancelTask := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, rearmBatch, cancelBatch) })
	_, _, _ = st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(rearmBatch, rearmTask))
	_, _, _ = st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(cancelBatch, cancelTask))
	past := time.Now().UTC().Add(-time.Minute)
	for _, taskID := range []string{rearmTask, cancelTask} {
		if _, err := st.db.Exec(`UPDATE image_generation_tasks SET status='RUNNING',claim_generation=2,dispatch_generation=2,lease_owner='dead-worker',lease_until=?,heartbeat_at=?,dispatched_at=? WHERE id=?`, past, past, past, taskID); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.RequestImageBatchCancel(ctx, "user-a", "agent-a", cancelBatch); err != nil {
		t.Fatal(err)
	}
	armed, _, err := fair.SweepExpiredImageGenerationTasks(ctx, 0, 100, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(armed) != 1 || armed[0].Task.ID != rearmTask || armed[0].Task.DispatchGeneration != 3 {
		t.Fatalf("armed=%+v", armed)
	}
	again, _, err := fair.SweepExpiredImageGenerationTasks(ctx, 0, 100, time.Second)
	if err != nil || len(again) != 0 {
		t.Fatalf("duplicate sweep=%+v err=%v", again, err)
	}
	canceled, err := st.GetImageGenerationTask(ctx, cancelTask)
	if err != nil || canceled.Status != ImageGenerationTaskCanceled {
		t.Fatalf("canceled task=%+v err=%v", canceled, err)
	}
	batch, err := st.GetImageGenerationBatchForPrincipal(ctx, "user-a", "agent-a", cancelBatch)
	if err != nil || batch.Status != ImageGenerationBatchCanceled {
		t.Fatalf("canceled batch=%+v err=%v", batch, err)
	}
}
