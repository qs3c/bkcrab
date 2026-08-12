package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/fairqueue"
)

func openImagegenMySQLTestStore(t *testing.T) *DBStore {
	t.Helper()
	dsn := os.Getenv("BKCRAB_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("BKCRAB_TEST_MYSQL_DSN is not set")
	}
	st, err := NewDBStore(mysqlDialect, dsn)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("migrate MySQL: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func imagegenTestID(t *testing.T, prefix string) string {
	t.Helper()
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return prefix + hex.EncodeToString(raw[:])
}

func cleanupImageBatch(t *testing.T, st *DBStore, batchIDs ...string) {
	t.Helper()
	for _, id := range batchIDs {
		_, _ = st.db.Exec(`DELETE FROM image_generation_tasks WHERE batch_id=?`, id)
		_, _ = st.db.Exec(`DELETE FROM image_generation_batches WHERE id=?`, id)
	}
}

func TestImageGenerationMySQLCreateReadAuthorizationAndRollback(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	taskA := imagegenTestID(t, "imgt_")
	taskB := imagegenTestID(t, "imgt_")
	rollbackBatchID := imagegenTestID(t, "imgb_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID, rollbackBatchID) })

	request := testCreateImageBatchRequest(batchID, taskA, taskB)
	batch, tasks, err := st.CreateImageGenerationBatch(ctx, request)
	if err != nil {
		t.Fatalf("create batch: %v", err)
	}
	if batch.Status != ImageGenerationBatchPending || batch.RequestedCount != 5 || len(tasks) != 2 ||
		tasks[0].SequenceID <= 0 || tasks[1].SequenceID <= tasks[0].SequenceID ||
		tasks[0].Status != ImageGenerationTaskPending || tasks[0].DispatchGeneration != 1 ||
		tasks[0].ClaimGeneration != 0 || tasks[0].DispatchedAt != nil {
		t.Fatalf("created batch/tasks = %+v / %+v", batch, tasks)
	}
	got, err := st.GetImageGenerationBatchForPrincipal(ctx, "user-a", "agent-a", batchID)
	if err != nil || got.ID != batchID {
		t.Fatalf("authorized get = %+v, %v", got, err)
	}
	for _, principal := range [][2]string{{"other", "agent-a"}, {"user-a", "other-agent"}} {
		if _, err := st.GetImageGenerationBatchForPrincipal(ctx, principal[0], principal[1], batchID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unauthorized get (%v) error = %v", principal, err)
		}
	}
	listed, err := st.ListImageGenerationTasks(ctx, batchID)
	if err != nil || len(listed) != 2 || listed[0].ItemIndex != 0 || listed[1].ItemIndex != 1 {
		t.Fatalf("listed tasks = %+v, %v", listed, err)
	}

	rollback := testCreateImageBatchRequest(rollbackBatchID, taskA)
	if _, _, err := st.CreateImageGenerationBatch(ctx, rollback); err == nil {
		t.Fatal("task primary-key collision did not fail")
	}
	var count int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM image_generation_batches WHERE id=?`, rollbackBatchID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back batch count = %d, err=%v", count, err)
	}
}

func TestImageGenerationMySQLSchemaShape(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	ctx := context.Background()
	for table, wantColumns := range map[string]int{"image_generation_batches": 21, "image_generation_tasks": 31} {
		var columns int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if columns != wantColumns {
			t.Fatalf("%s columns = %d, want %d", table, columns, wantColumns)
		}
	}
	var dataType, extra string
	if err := st.db.QueryRowContext(ctx, `SELECT data_type,extra FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='image_generation_tasks' AND column_name='sequence_id'`).Scan(&dataType, &extra); err != nil {
		t.Fatal(err)
	}
	if dataType != "bigint" || !strings.Contains(extra, "auto_increment") {
		t.Fatalf("sequence_id shape = %q %q", dataType, extra)
	}
	for _, index := range []string{"uq_image_generation_tasks_sequence", "uq_image_generation_tasks_chunk", "idx_image_generation_tasks_dispatch", "idx_image_generation_tasks_user_running"} {
		var count int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='image_generation_tasks' AND index_name=?`, index).Scan(&count); err != nil || count == 0 {
			t.Fatalf("index %s count=%d err=%v", index, count, err)
		}
	}
}

func TestImageGenerationMySQLDispatchMarkGuardAndExpiredRearm(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	taskID := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID) })
	_, tasks, err := st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(batchID, taskID))
	if err != nil {
		t.Fatal(err)
	}

	page, next, err := st.ListDispatchableImageTasksPage(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var candidate ImageTaskDispatchCandidate
	for _, item := range page {
		if item.Task.ID == taskID {
			candidate = item
		}
	}
	if candidate.Task.ID == "" || next < tasks[0].SequenceID {
		t.Fatalf("dispatch page = %+v next=%d", page, next)
	}
	stale := candidate
	stale.Guard.RetryCount++
	if changed, err := st.MarkImageTaskDispatched(ctx, stale, stale.Guard.DispatchGeneration); err == nil || changed {
		t.Fatalf("stale mark = %v, %v", changed, err)
	}
	if changed, err := st.MarkImageTaskDispatched(ctx, candidate, candidate.Guard.DispatchGeneration+1); err == nil || changed {
		t.Fatalf("wrong generation mark = %v, %v", changed, err)
	}
	if changed, err := st.MarkImageTaskDispatched(ctx, candidate, candidate.Guard.DispatchGeneration); err != nil || !changed {
		t.Fatalf("mark = %v, %v", changed, err)
	}
	if changed, err := st.MarkImageTaskDispatched(ctx, candidate, candidate.Guard.DispatchGeneration); err == nil || changed {
		t.Fatalf("duplicate mark = %v, %v", changed, err)
	}
	highWater, err := st.CaptureImageFairQueueHighWater(ctx)
	if err != nil || highWater < tasks[0].SequenceID {
		t.Fatalf("high water = %d, %v", highWater, err)
	}
	tenants, _, err := st.ListCanonicalImageTenants(ctx, highWater, "", 10)
	if err != nil || len(tenants) == 0 || tenants[0] != "user-a" {
		t.Fatalf("canonical tenants = %v, %v", tenants, err)
	}
	dispatched, _, err := st.ListDispatchedImageTasks(ctx, highWater, 0, 10)
	if err != nil || len(dispatched) != 1 || dispatched[0].ID != taskID {
		t.Fatalf("dispatched page = %+v, %v", dispatched, err)
	}
	broker, _, err := st.ListBrokerBackedImageCandidates(ctx, highWater, 0, 10)
	if err != nil || len(broker) != 1 || broker[0].Task.ID != taskID {
		t.Fatalf("broker page = %+v, %v", broker, err)
	}
	repaired, changed, err := st.RearmImageCandidateAfterBrokerLoss(ctx, broker[0])
	if err != nil || !changed || repaired.Task.DispatchGeneration != 2 || repaired.Task.DispatchedAt != nil {
		t.Fatalf("broker rearm = %+v changed=%v err=%v", repaired, changed, err)
	}
	if _, changed, err := st.RearmImageCandidateAfterBrokerLoss(ctx, broker[0]); err != nil || changed {
		t.Fatalf("stale broker rearm = %v, %v", changed, err)
	}
	current, err := st.GetDispatchableImageTaskByID(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := st.MarkImageTaskDispatched(ctx, *current, current.Guard.DispatchGeneration); err != nil || !changed {
		t.Fatalf("mark repaired = %v, %v", changed, err)
	}
	tenantHash, err := fairqueue.TenantHash(ImageGenerationResource, "user-a")
	if err != nil {
		t.Fatal(err)
	}
	poisonRepaired, disposition, err := st.RepairPoisonImageCandidate(ctx, ImagePoisonRepairLocator{TaskID: taskID, Generation: 2}, ImageGenerationResource, tenantHash)
	if err != nil || disposition != ImagePoisonRepairRearmed || poisonRepaired.Task.DispatchGeneration != 3 {
		t.Fatalf("poison repair = %+v disposition=%s err=%v", poisonRepaired, disposition, err)
	}

	future := time.Now().UTC().Add(time.Minute)
	if _, err := st.db.Exec(`UPDATE image_generation_tasks SET status='RUNNING',claim_generation=3,dispatch_generation=3,
		lease_owner='worker',lease_until=?,heartbeat_at=?,next_run_at=NULL,dispatched_at=? WHERE id=?`, future, time.Now().UTC(), time.Now().UTC(), taskID); err != nil {
		t.Fatal(err)
	}
	running, _, err := st.ListValidRunningImageTasks(ctx, highWater, 0, 10)
	if err != nil || len(running) != 1 || running[0].Task.ID != taskID || running[0].ObservedDBNow.IsZero() {
		t.Fatalf("running page = %+v, %v", running, err)
	}

	past := time.Now().UTC().Add(-time.Minute)
	if _, err := st.db.Exec(`UPDATE image_generation_tasks SET status='RUNNING',claim_generation=3,dispatch_generation=3,
		lease_owner='worker',lease_until=?,heartbeat_at=?,next_run_at=NULL,dispatched_at=? WHERE id=?`, past, past, past, taskID); err != nil {
		t.Fatal(err)
	}
	armed, _, err := st.ArmExpiredImageTasks(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var rearmed ImageTaskDispatchCandidate
	for _, item := range armed {
		if item.Task.ID == taskID {
			rearmed = item
		}
	}
	if rearmed.Task.ID == "" || rearmed.Task.Status != ImageGenerationTaskRunning || rearmed.Task.DispatchGeneration != 4 || rearmed.Task.ClaimGeneration != 3 || rearmed.Task.DispatchedAt != nil {
		t.Fatalf("rearmed candidate = %+v", rearmed)
	}
	again, _, err := st.ArmExpiredImageTasks(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range again {
		if item.Task.ID == taskID {
			t.Fatalf("expired task generation advanced twice: %+v", item)
		}
	}
}

func TestImageGenerationMySQLFinalizeAggregateAndCancelIdempotent(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	ctx := context.Background()
	partialBatch := imagegenTestID(t, "imgb_")
	doneTask := imagegenTestID(t, "imgt_")
	failedTask := imagegenTestID(t, "imgt_")
	cancelBatch := imagegenTestID(t, "imgb_")
	pendingTask := imagegenTestID(t, "imgt_")
	runningTask := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, partialBatch, cancelBatch) })

	partialRequest := testCreateImageBatchRequest(partialBatch, doneTask, failedTask)
	partialRequest.ArtifactByteLimit = 4
	_, _, err := st.CreateImageGenerationBatch(ctx, partialRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := st.FinalizeImageTaskDone(ctx, doneTask, ImageTaskDoneResult{Provider: "openai", Model: "gpt-image", ManifestKey: "imagegen/manifest.json", ArtifactsJSON: jsonRaw(`[{"path":"imagegen/only-one.png","size":1}]`)}); err == nil || changed {
		t.Fatalf("incomplete artifact finalize = %v, %v", changed, err)
	}
	artifacts := jsonRaw(`[{"path":"imagegen/0.png","size":1},{"path":"imagegen/1.png","size":1},{"path":"imagegen/2.png","size":1},{"path":"imagegen/3.png","size":1}]`)
	if _, changed, err := st.FinalizeImageTaskDone(ctx, doneTask, ImageTaskDoneResult{Provider: "openai", Model: "gpt-image", ManifestKey: "imagegen/manifest.json", ArtifactsJSON: artifacts}); err != nil || !changed {
		t.Fatalf("finalize done = %v, %v", changed, err)
	}
	if _, changed, err := st.FinalizeImageTaskDone(ctx, doneTask, ImageTaskDoneResult{Provider: "openai", Model: "gpt-image", ManifestKey: "imagegen/manifest.json", ArtifactsJSON: artifacts}); err != nil || changed {
		t.Fatalf("duplicate finalize done = %v, %v", changed, err)
	}
	batch, changed, err := st.FinalizeImageTaskFailed(ctx, failedTask, "PERMANENT", "bounded error")
	if err != nil || !changed || batch.Status != ImageGenerationBatchPartial || batch.SucceededCount != 4 || batch.FailedCount != 1 {
		t.Fatalf("partial aggregate = %+v changed=%v err=%v", batch, changed, err)
	}

	_, cancelTasks, err := st.CreateImageGenerationBatch(ctx, testCreateImageBatchRequest(cancelBatch, pendingTask, runningTask))
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Minute)
	if _, err := st.db.Exec(`UPDATE image_generation_tasks SET status='RUNNING',claim_generation=1,dispatch_generation=1,lease_owner='worker',lease_until=?,heartbeat_at=?,dispatched_at=? WHERE id=?`, future, time.Now().UTC(), time.Now().UTC(), runningTask); err != nil {
		t.Fatal(err)
	}
	batch, tasks, err := st.RequestImageBatchCancel(ctx, "user-a", "agent-a", cancelBatch)
	if err != nil || batch.Status != ImageGenerationBatchCanceling || !batch.CancelRequested {
		t.Fatalf("cancel request = %+v tasks=%+v err=%v", batch, tasks, err)
	}
	byID := map[string]ImageGenerationTaskRecord{}
	for _, task := range tasks {
		byID[task.ID] = task
	}
	if byID[pendingTask].Status != ImageGenerationTaskCanceled || byID[runningTask].Status != ImageGenerationTaskRunning || byID[pendingTask].DispatchGeneration <= cancelTasks[0].DispatchGeneration {
		t.Fatalf("cancel task states = %+v", byID)
	}
	batch, _, err = st.RequestImageBatchCancel(ctx, "user-a", "agent-a", cancelBatch)
	if err != nil || batch.Status != ImageGenerationBatchCanceling {
		t.Fatalf("repeat cancel = %+v, %v", batch, err)
	}
	batch, changed, err = st.FinalizeImageTaskCanceled(ctx, runningTask)
	if err != nil || !changed || batch.Status != ImageGenerationBatchCanceled || batch.CanceledCount != 5 {
		t.Fatalf("cancel aggregate = %+v changed=%v err=%v", batch, changed, err)
	}
	if _, _, err := st.RequestImageBatchCancel(ctx, "other", "agent-a", cancelBatch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unauthorized cancel error = %v", err)
	}
}

func TestImageGenerationMySQLBatchArtifactLimitIsAtomicAcrossTasks(t *testing.T) {
	st := openImagegenMySQLTestStore(t)
	ctx := context.Background()
	batchID := imagegenTestID(t, "imgb_")
	firstTask := imagegenTestID(t, "imgt_")
	secondTask := imagegenTestID(t, "imgt_")
	t.Cleanup(func() { cleanupImageBatch(t, st, batchID) })
	request := testCreateImageBatchRequest(batchID, firstTask, secondTask)
	request.ArtifactByteLimit = 5
	if _, _, err := st.CreateImageGenerationBatch(ctx, request); err != nil {
		t.Fatal(err)
	}
	first := jsonRaw(`[{"size":1},{"size":1},{"size":1},{"size":1}]`)
	if _, changed, err := st.FinalizeImageTaskDone(ctx, firstTask, ImageTaskDoneResult{ManifestKey: "imagegen/first.json", ArtifactsJSON: first}); err != nil || !changed {
		t.Fatalf("first finalize changed=%t err=%v", changed, err)
	}
	second := jsonRaw(`[{"size":2}]`)
	batch, changed, err := st.FinalizeImageTaskDone(ctx, secondTask, ImageTaskDoneResult{ManifestKey: "imagegen/second.json", ArtifactsJSON: second})
	if !errors.Is(err, ErrImageGenerationBatchArtifactLimit) || !changed || batch == nil ||
		batch.Status != ImageGenerationBatchPartial || batch.SucceededCount != 4 || batch.FailedCount != 1 {
		t.Fatalf("limited finalize batch=%+v changed=%t err=%v", batch, changed, err)
	}
	task, err := st.GetImageGenerationTask(ctx, secondTask)
	if err != nil || task.Status != ImageGenerationTaskFailed || task.ErrorCode != "ARTIFACT_BATCH_LIMIT" || len(task.ArtifactsJSON) != 0 {
		t.Fatalf("limited task=%+v err=%v", task, err)
	}
}

func jsonRaw(value string) []byte { return []byte(strings.TrimSpace(value)) }
