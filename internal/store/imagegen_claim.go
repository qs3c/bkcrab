package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/fairqueue"
)

type ImageGenerationClaimDisposition string

const (
	ImageGenerationClaimed               ImageGenerationClaimDisposition = "claimed"
	ImageGenerationClaimCapacityDeferred ImageGenerationClaimDisposition = "capacity-deferred"
	ImageGenerationClaimDuplicateStale   ImageGenerationClaimDisposition = "duplicate-stale"
	ImageGenerationClaimBatchCanceled    ImageGenerationClaimDisposition = "batch-canceled"
)

type ImageGenerationClaimLimits struct {
	GlobalConcurrency       int
	PerUserBurstConcurrency int
	AdvisoryLockTimeout     time.Duration
}

func (l ImageGenerationClaimLimits) normalized() (ImageGenerationClaimLimits, error) {
	if l.GlobalConcurrency <= 0 || l.PerUserBurstConcurrency <= 0 || l.PerUserBurstConcurrency > l.GlobalConcurrency {
		return ImageGenerationClaimLimits{}, errors.New("store: invalid image generation capacity limits")
	}
	if l.AdvisoryLockTimeout == 0 {
		l.AdvisoryLockTimeout = 5 * time.Second
	}
	if l.AdvisoryLockTimeout <= 0 {
		return ImageGenerationClaimLimits{}, errors.New("store: invalid image generation advisory lock timeout")
	}
	return l, nil
}

type ImageGenerationFence struct {
	TaskID                    string
	BatchID                   string
	UserID                    string
	ClaimGeneration           int64
	LeaseOwner                string
	ExpectedWriterFingerprint string
}

type ImageGenerationTaskClaim struct {
	Task                    ImageGenerationTaskRecord
	Batch                   ImageGenerationBatchRecord
	Fence                   ImageGenerationFence
	PreviousClaimGeneration int64
}

type ImageGenerationClaimResult struct {
	Disposition ImageGenerationClaimDisposition
	Claim       *ImageGenerationTaskClaim
}

type ImageGenerationHeartbeatDisposition string

const (
	ImageGenerationHeartbeatExtended ImageGenerationHeartbeatDisposition = "extended"
	ImageGenerationHeartbeatCanceled ImageGenerationHeartbeatDisposition = "canceled"
	ImageGenerationHeartbeatStale    ImageGenerationHeartbeatDisposition = "stale"
)

var ErrImageGenerationCapacityLockUnavailable = errors.New("store: image generation capacity lock unavailable")

// ImageFairQueueStore prevents fair-mode callers from accidentally issuing an
// execution-affecting read or mutation without an authoritative writer fence.
type ImageFairQueueStore struct {
	store          *DBStore
	expectedWriter string
}

func (d *DBStore) BindImageFairQueueWriter(expected string) (*ImageFairQueueStore, error) {
	if !lowerHex64Pattern.MatchString(expected) {
		return nil, ErrFairQueueWriterMismatch
	}
	if d == nil || d.db == nil || d.dialect != mysqlDialect {
		return nil, ErrFairQueueMySQLRequired
	}
	return &ImageFairQueueStore{store: d, expectedWriter: expected}, nil
}

func (s *ImageFairQueueStore) ExpectedWriterFingerprint() string {
	if s == nil {
		return ""
	}
	return s.expectedWriter
}

func (s *ImageFairQueueStore) validate() error {
	if s == nil || !lowerHex64Pattern.MatchString(s.expectedWriter) {
		return ErrFairQueueWriterMismatch
	}
	if s.store == nil || s.store.db == nil || s.store.dialect != mysqlDialect {
		return ErrFairQueueMySQLRequired
	}
	return nil
}

func (s *ImageFairQueueStore) withResourceTx(ctx context.Context, lockTimeout time.Duration, fn func(*sql.Tx) error) error {
	if err := s.validate(); err != nil {
		return err
	}
	return s.store.withFairQueueResourceLock(ctx, s.expectedWriter, ImageGenerationResource, lockTimeout,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, err)
			}
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, rollbackErr)
				}
			}()
			if err := fn(tx); err != nil {
				return err
			}
			return commitImageGenerationTx(ctx, tx, identity)
		})
}

func (s *ImageFairQueueStore) CreateImageGenerationBatch(ctx context.Context, request CreateImageGenerationBatchRequest) (*ImageGenerationBatchRecord, []ImageGenerationTaskRecord, error) {
	if err := validateCreateImageGenerationBatch(request); err != nil {
		return nil, nil, err
	}
	var batch *ImageGenerationBatchRecord
	var tasks []ImageGenerationTaskRecord
	err := s.withResourceTx(ctx, 5*time.Second, func(tx *sql.Tx) (err error) {
		batch, tasks, err = s.store.createImageGenerationBatchTx(ctx, tx, request)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, tasks, nil
}

func (s *ImageFairQueueStore) GetImageGenerationBatchForPrincipal(ctx context.Context, userID, agentID, batchID string) (*ImageGenerationBatchRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var batch *ImageGenerationBatchRecord
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) (err error) {
		batch, err = scanImageBatch(conn.QueryRowContext(ctx, `SELECT `+imageBatchColumns+` FROM image_generation_batches WHERE id=? AND user_id=? AND agent_id=?`, batchID, userID, agentID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

func (s *ImageFairQueueStore) ListImageGenerationTasks(ctx context.Context, batchID string) ([]ImageGenerationTaskRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var tasks []ImageGenerationTaskRecord
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		rows, err := conn.QueryContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE batch_id=? ORDER BY item_index,chunk_index,id`, batchID)
		if err != nil {
			return err
		}
		tasks, err = scanImageTaskRows(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *ImageFairQueueStore) GetImageGenerationTask(ctx context.Context, taskID string) (*ImageGenerationTaskRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var task *ImageGenerationTaskRecord
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) (err error) {
		task, err = scanImageTask(conn.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=?`, taskID))
		return err
	})
	if err != nil {
		return nil, err
	}
	return task, nil
}

func (s *ImageFairQueueStore) RequestImageBatchCancel(ctx context.Context, userID, agentID, batchID string) (*ImageGenerationBatchRecord, []ImageGenerationTaskRecord, error) {
	var batch *ImageGenerationBatchRecord
	var tasks []ImageGenerationTaskRecord
	err := s.withResourceTx(ctx, 5*time.Second, func(tx *sql.Tx) (err error) {
		batch, tasks, err = s.store.requestImageBatchCancelTx(ctx, tx, userID, agentID, batchID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return batch, tasks, nil
}

func (s *ImageFairQueueStore) HeartbeatImageGenerationTask(ctx context.Context, fence ImageGenerationFence, leaseDuration time.Duration) (ImageGenerationHeartbeatDisposition, error) {
	if err := s.validate(); err != nil {
		return ImageGenerationHeartbeatStale, err
	}
	if fence.ExpectedWriterFingerprint != s.expectedWriter {
		return ImageGenerationHeartbeatStale, ErrFairQueueWriterMismatch
	}
	return s.store.HeartbeatImageGenerationTask(ctx, fence, leaseDuration)
}

func (s *ImageFairQueueStore) FinishImageGenerationTaskDone(ctx context.Context, fence ImageGenerationFence, result ImageTaskDoneResult) (*ImageGenerationBatchRecord, bool, error) {
	if err := s.validate(); err != nil {
		return nil, false, err
	}
	if fence.ExpectedWriterFingerprint != s.expectedWriter {
		return nil, false, ErrFairQueueWriterMismatch
	}
	return s.store.FinishImageGenerationTaskDone(ctx, fence, result)
}

func (s *ImageFairQueueStore) FinishImageGenerationTaskRetry(ctx context.Context, fence ImageGenerationFence, errorCode string, nextRun time.Time) (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	if fence.ExpectedWriterFingerprint != s.expectedWriter {
		return false, ErrFairQueueWriterMismatch
	}
	return s.store.FinishImageGenerationTaskRetry(ctx, fence, errorCode, nextRun)
}

func (s *ImageFairQueueStore) FinishImageGenerationTaskFailed(ctx context.Context, fence ImageGenerationFence, errorCode string) (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	if fence.ExpectedWriterFingerprint != s.expectedWriter {
		return false, ErrFairQueueWriterMismatch
	}
	return s.store.FinishImageGenerationTaskFailed(ctx, fence, errorCode)
}

func (s *ImageFairQueueStore) FinishImageGenerationTaskCanceled(ctx context.Context, fence ImageGenerationFence) (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	if fence.ExpectedWriterFingerprint != s.expectedWriter {
		return false, ErrFairQueueWriterMismatch
	}
	return s.store.FinishImageGenerationTaskCanceled(ctx, fence)
}

func (s *ImageFairQueueStore) ListDispatchableImageTasksPage(ctx context.Context, afterSequenceID int64, limit int) ([]ImageTaskDispatchCandidate, int64, error) {
	if err := s.validate(); err != nil {
		return nil, afterSequenceID, err
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, err
	}
	var candidates []ImageTaskDispatchCandidate
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		query := `SELECT ` + qualifiedImageTaskColumns("t") + ` FROM image_generation_tasks t
			JOIN image_generation_batches b ON b.id=t.batch_id
			WHERE t.sequence_id>? AND t.status IN ('PENDING','RUNNING')
			AND t.dispatch_generation>t.claim_generation AND t.dispatched_at IS NULL
			AND b.cancel_requested=FALSE AND ` + imageTaskDuePredicate("t") + `
			ORDER BY t.sequence_id LIMIT ?`
		rows, err := conn.QueryContext(ctx, query, afterSequenceID, limit)
		if err != nil {
			return err
		}
		candidates, err = scanImageCandidateRows(rows)
		return err
	})
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(candidates) > 0 {
		next = candidates[len(candidates)-1].Task.SequenceID
	}
	return candidates, next, nil
}

func (s *ImageFairQueueStore) GetDispatchableImageTaskByID(ctx context.Context, taskID string) (*ImageTaskDispatchCandidate, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	var candidate *ImageTaskDispatchCandidate
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		query := `SELECT ` + qualifiedImageTaskColumns("t") + ` FROM image_generation_tasks t
			JOIN image_generation_batches b ON b.id=t.batch_id
			WHERE t.id=? AND t.status IN ('PENDING','RUNNING')
			AND t.dispatch_generation>t.claim_generation AND t.dispatched_at IS NULL
			AND b.cancel_requested=FALSE AND ` + imageTaskDuePredicate("t")
		task, err := scanImageTask(conn.QueryRowContext(ctx, query, taskID))
		if err != nil {
			return err
		}
		value := newImageTaskDispatchCandidate(imageTaskDispatchRecord(*task))
		candidate = &value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

func (s *ImageFairQueueStore) MarkImageTaskDispatched(ctx context.Context, candidate ImageTaskDispatchCandidate, dispatchGeneration int64) (changed bool, err error) {
	if !validImageTaskDispatchCandidate(candidate) || candidate.Guard.DispatchedAt != nil ||
		candidate.Guard.DispatchGeneration <= candidate.Guard.ClaimGeneration || dispatchGeneration != candidate.Guard.DispatchGeneration {
		return false, ErrImageTaskDispatchGuard
	}
	guard := candidate.Guard
	err = s.withResourceTx(ctx, 5*time.Second, func(tx *sql.Tx) error {
		result, updateErr := tx.ExecContext(ctx, `UPDATE image_generation_tasks t
			JOIN image_generation_batches b ON b.id=t.batch_id
			SET t.dispatched_at=UTC_TIMESTAMP(6),t.updated_at=UTC_TIMESTAMP(6)
			WHERE t.id=? AND t.sequence_id=? AND t.batch_id=? AND t.user_id=? AND t.status=?
			AND t.retry_count=? AND t.claim_generation=? AND t.dispatch_generation=?
			AND COALESCE(t.lease_owner,'')=? AND t.next_run_at<=>? AND t.lease_until<=>?
			AND t.dispatched_at IS NULL AND t.dispatch_generation>t.claim_generation
			AND b.cancel_requested=FALSE AND `+imageTaskDuePredicate("t"), guard.TaskID,
			guard.SequenceID, guard.BatchID, guard.UserID, guard.Status, guard.RetryCount,
			guard.ClaimGeneration, guard.DispatchGeneration, guard.LeaseOwner, guard.NextRunAt, guard.LeaseUntil)
		if updateErr != nil {
			return updateErr
		}
		count, countErr := result.RowsAffected()
		if countErr != nil {
			return countErr
		}
		if count != 1 {
			return ErrImageTaskDispatchStale
		}
		changed = true
		return nil
	})
	return changed, err
}

func (s *ImageFairQueueStore) CaptureImageFairQueueHighWater(ctx context.Context) (int64, error) {
	var highWater int64
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		return conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence_id),0) FROM image_generation_tasks`).Scan(&highWater)
	})
	return highWater, err
}

func (s *ImageFairQueueStore) ListCanonicalImageTenants(ctx context.Context, highWater int64, afterUserID string, limit int) ([]string, string, error) {
	if highWater < 0 || limit < 1 || limit > 10_000 {
		return nil, afterUserID, errors.New("store: invalid image tenant page")
	}
	var tenants []string
	next := afterUserID
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		rows, err := conn.QueryContext(ctx, `SELECT DISTINCT user_id FROM image_generation_tasks WHERE sequence_id<=? AND user_id>? ORDER BY user_id LIMIT ?`, highWater, afterUserID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var userID string
			if err := rows.Scan(&userID); err != nil {
				return err
			}
			tenants = append(tenants, userID)
			next = userID
		}
		return rows.Err()
	})
	if err != nil {
		return nil, afterUserID, err
	}
	return tenants, next, nil
}

func (s *ImageFairQueueStore) ListDispatchedImageTasks(ctx context.Context, highWater, afterSequenceID int64, limit int) ([]ImageTaskDispatchRecord, int64, error) {
	if highWater < 0 || validateImagePage(afterSequenceID, limit) != nil {
		return nil, afterSequenceID, errors.New("store: invalid dispatched image task page")
	}
	var tasks []ImageTaskDispatchRecord
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		rows, err := conn.QueryContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks
			WHERE sequence_id>? AND sequence_id<=? AND dispatched_at IS NOT NULL
			AND ((status='PENDING' AND dispatch_generation>claim_generation) OR (status='RUNNING' AND dispatch_generation>=claim_generation))
			ORDER BY sequence_id LIMIT ?`, afterSequenceID, highWater, limit)
		if err != nil {
			return err
		}
		full, err := scanImageTaskRows(rows)
		if err != nil {
			return err
		}
		for _, task := range full {
			tasks = append(tasks, imageTaskDispatchRecord(task))
		}
		return nil
	})
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(tasks) > 0 {
		next = tasks[len(tasks)-1].SequenceID
	}
	return tasks, next, nil
}

func (s *ImageFairQueueStore) ListValidRunningImageTasks(ctx context.Context, highWater, afterSequenceID int64, limit int) ([]ImageRunningTaskSnapshot, int64, error) {
	if highWater < 0 || validateImagePage(afterSequenceID, limit) != nil {
		return nil, afterSequenceID, errors.New("store: invalid running image task page")
	}
	var snapshots []ImageRunningTaskSnapshot
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		rows, err := conn.QueryContext(ctx, `SELECT `+imageTaskColumns+`,UTC_TIMESTAMP(6) FROM image_generation_tasks
			WHERE sequence_id>? AND sequence_id<=? AND status='RUNNING' AND dispatch_generation=claim_generation AND claim_generation>0
			AND COALESCE(lease_owner,'')<>'' AND lease_until>UTC_TIMESTAMP(6) AND heartbeat_at IS NOT NULL AND next_run_at IS NULL
			ORDER BY sequence_id LIMIT ?`, afterSequenceID, highWater, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			task, observed, err := scanImageRunningTask(rows)
			if err != nil {
				return err
			}
			snapshots = append(snapshots, ImageRunningTaskSnapshot{Task: imageTaskDispatchRecord(*task), ObservedDBNow: observed})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(snapshots) > 0 {
		next = snapshots[len(snapshots)-1].Task.SequenceID
	}
	return snapshots, next, nil
}

func (s *ImageFairQueueStore) CaptureImageBrokerRepairHighWater(ctx context.Context) (int64, error) {
	return s.CaptureImageFairQueueHighWater(ctx)
}

func (s *ImageFairQueueStore) ListBrokerBackedImageCandidates(ctx context.Context, highWater, afterSequenceID int64, limit int) ([]ImageTaskDispatchCandidate, int64, error) {
	if highWater < 0 || validateImagePage(afterSequenceID, limit) != nil {
		return nil, afterSequenceID, errors.New("store: invalid broker image task page")
	}
	var candidates []ImageTaskDispatchCandidate
	err := s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		rows, err := conn.QueryContext(ctx, `SELECT `+qualifiedImageTaskColumns("t")+` FROM image_generation_tasks t
			JOIN image_generation_batches b ON b.id=t.batch_id
			WHERE t.sequence_id>? AND t.sequence_id<=? AND t.status IN ('PENDING','RUNNING')
			AND t.dispatch_generation>t.claim_generation AND t.dispatched_at IS NOT NULL
			AND b.cancel_requested=FALSE AND `+imageTaskDuePredicate("t")+` ORDER BY t.sequence_id LIMIT ?`, afterSequenceID, highWater, limit)
		if err != nil {
			return err
		}
		candidates, err = scanImageCandidateRows(rows)
		return err
	})
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(candidates) > 0 {
		next = candidates[len(candidates)-1].Task.SequenceID
	}
	return candidates, next, nil
}

func (s *ImageFairQueueStore) RearmImageCandidateAfterBrokerLoss(ctx context.Context, original ImageTaskDispatchCandidate) (rearmed *ImageTaskDispatchCandidate, changed bool, err error) {
	err = s.withResourceTx(ctx, 5*time.Second, func(tx *sql.Tx) (inner error) {
		rearmed, changed, inner = s.store.rearmImageCandidateOn(ctx, tx, original)
		return inner
	})
	return rearmed, changed, err
}

func (s *ImageFairQueueStore) RepairPoisonImageCandidate(ctx context.Context, locator ImagePoisonRepairLocator, registeredResource, queueTenantHash string) (rearmed *ImageTaskDispatchCandidate, disposition ImagePoisonRepairDisposition, err error) {
	disposition = ImagePoisonRepairUnlocatable
	if registeredResource != ImageGenerationResource || !imageTaskIDPattern.MatchString(locator.TaskID) || locator.Generation <= 0 {
		return nil, disposition, nil
	}
	err = s.withResourceTx(ctx, 5*time.Second, func(tx *sql.Tx) error {
		query := `SELECT ` + qualifiedImageTaskColumns("t") + ` FROM image_generation_tasks t
			JOIN image_generation_batches b ON b.id=t.batch_id
			WHERE t.id=? AND t.status IN ('PENDING','RUNNING') AND t.dispatch_generation>t.claim_generation
			AND b.cancel_requested=FALSE AND ` + imageTaskDuePredicate("t") + ` FOR UPDATE`
		task, readErr := scanImageTask(tx.QueryRowContext(ctx, query, locator.TaskID))
		if errors.Is(readErr, ErrNotFound) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		hash, hashErr := fairqueue.TenantHash(registeredResource, task.UserID)
		if hashErr != nil || hash != queueTenantHash || task.DispatchGeneration != locator.Generation || task.DispatchedAt == nil {
			disposition = ImagePoisonRepairStale
			return nil
		}
		original := newImageTaskDispatchCandidate(imageTaskDispatchRecord(*task))
		rearmed, _, readErr = s.store.rearmImageCandidateOn(ctx, tx, original)
		if readErr != nil {
			return readErr
		}
		disposition = ImagePoisonRepairRearmed
		return nil
	})
	return rearmed, disposition, err
}

func validImageWorkerID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 96 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

func commitImageGenerationTx(ctx context.Context, tx *sql.Tx, identity fairQueueMySQLIdentity) error {
	if err := verifyFairQueueMySQLSession(ctx, tx, identity, fairQueueCapacityLockName(identity.database, ImageGenerationResource)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(ErrFairQueueUnsafeConnection, err)
	}
	return nil
}

func (s *ImageFairQueueStore) ClaimImageGenerationTaskByID(
	ctx context.Context,
	taskID, expectedUserID string,
	expectedDispatchGeneration int64,
	workerID string,
	leaseDuration time.Duration,
	limits ImageGenerationClaimLimits,
) (result ImageGenerationClaimResult, err error) {
	result.Disposition = ImageGenerationClaimDuplicateStale
	if err := s.validate(); err != nil {
		return result, err
	}
	limits, err = limits.normalized()
	if err != nil {
		return result, err
	}
	if !imageTaskIDPattern.MatchString(taskID) || strings.TrimSpace(expectedUserID) == "" ||
		expectedDispatchGeneration <= 0 || !validImageWorkerID(workerID) || leaseDuration <= 0 {
		return result, errors.New("store: invalid image generation delivery identity")
	}
	err = s.store.withFairQueueResourceLock(ctx, s.expectedWriter, ImageGenerationResource, limits.AdvisoryLockTimeout,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			var batchID string
			if lookupErr := conn.QueryRowContext(ctx, `SELECT batch_id FROM image_generation_tasks WHERE id=?`, taskID).Scan(&batchID); errors.Is(lookupErr, sql.ErrNoRows) {
				return nil
			} else if lookupErr != nil {
				return lookupErr
			}
			tx, beginErr := conn.BeginTx(ctx, nil)
			if beginErr != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, beginErr)
			}
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, rollbackErr)
				}
			}()
			batch, readErr := s.store.getImageGenerationBatch(ctx, tx, batchID, true)
			if readErr != nil {
				if errors.Is(readErr, ErrNotFound) {
					return nil
				}
				return readErr
			}
			task, readErr := scanImageTask(tx.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=? FOR UPDATE`, taskID))
			if readErr != nil {
				if errors.Is(readErr, ErrNotFound) {
					return nil
				}
				return readErr
			}
			if task.BatchID != batch.ID || task.UserID != batch.UserID || task.UserID != expectedUserID ||
				task.DispatchGeneration != expectedDispatchGeneration || task.DispatchGeneration <= task.ClaimGeneration {
				return nil
			}
			if batch.CancelRequested || imageBatchTerminal(batch.Status) {
				result.Disposition = ImageGenerationClaimBatchCanceled
				return nil
			}
			var now time.Time
			if readErr = tx.QueryRowContext(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&now); readErr != nil {
				return readErr
			}
			due := task.NextRunAt == nil || !task.NextRunAt.After(now)
			runnable := task.Status == ImageGenerationTaskPending && due
			if task.Status == ImageGenerationTaskRunning {
				runnable = task.LeaseUntil != nil && !task.LeaseUntil.After(now) && task.DispatchGeneration > task.ClaimGeneration
			}
			if !runnable {
				return nil
			}
			if task.RetryCount >= task.MaxRetry && task.RetryCount > 0 {
				if _, updateErr := tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='FAILED',error_code='RETRY_EXHAUSTED',error_msg='retry limit exhausted',lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=? AND status IN ('PENDING','RUNNING')`, task.ID); updateErr != nil {
					return updateErr
				}
				if _, readErr = s.store.recomputeImageBatchOn(ctx, tx, batch); readErr != nil {
					return readErr
				}
				return commitImageGenerationTx(ctx, tx, identity)
			}
			var globalRunning, userRunning int
			if readErr = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM image_generation_tasks WHERE status='RUNNING' AND lease_until>UTC_TIMESTAMP(6) AND dispatch_generation=claim_generation`).Scan(&globalRunning); readErr != nil {
				return readErr
			}
			if readErr = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM image_generation_tasks WHERE user_id=? AND status='RUNNING' AND lease_until>UTC_TIMESTAMP(6) AND dispatch_generation=claim_generation`, expectedUserID).Scan(&userRunning); readErr != nil {
				return readErr
			}
			if globalRunning >= limits.GlobalConcurrency || userRunning >= limits.PerUserBurstConcurrency {
				result.Disposition = ImageGenerationClaimCapacityDeferred
				return nil
			}
			previousClaimGeneration := task.ClaimGeneration
			leaseUntil := now.Add(leaseDuration)
			updated, updateErr := tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='RUNNING',claim_generation=dispatch_generation,lease_owner=?,lease_until=?,heartbeat_at=?,next_run_at=NULL,dispatched_at=COALESCE(dispatched_at,?),started_at=COALESCE(started_at,?),updated_at=? WHERE id=? AND user_id=? AND dispatch_generation=? AND dispatch_generation>claim_generation AND status=?`, workerID, leaseUntil, now, now, now, now, task.ID, expectedUserID, expectedDispatchGeneration, task.Status)
			if updateErr != nil {
				return updateErr
			}
			rows, rowsErr := updated.RowsAffected()
			if rowsErr != nil {
				return rowsErr
			}
			if rows != 1 {
				return nil
			}
			if _, updateErr = tx.ExecContext(ctx, `UPDATE image_generation_batches SET status='RUNNING',started_at=COALESCE(started_at,?),updated_at=? WHERE id=? AND status IN ('PENDING','RUNNING')`, now, now, batch.ID); updateErr != nil {
				return updateErr
			}
			task, readErr = scanImageTask(tx.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=?`, task.ID))
			if readErr != nil {
				return readErr
			}
			batch, readErr = s.store.getImageGenerationBatch(ctx, tx, batch.ID, false)
			if readErr != nil {
				return readErr
			}
			if commitErr := commitImageGenerationTx(ctx, tx, identity); commitErr != nil {
				return commitErr
			}
			result.Disposition = ImageGenerationClaimed
			result.Claim = &ImageGenerationTaskClaim{
				Task: *task, Batch: *batch, PreviousClaimGeneration: previousClaimGeneration,
				Fence: ImageGenerationFence{TaskID: task.ID, BatchID: batch.ID, UserID: task.UserID,
					ClaimGeneration: task.ClaimGeneration, LeaseOwner: workerID,
					ExpectedWriterFingerprint: s.expectedWriter},
			}
			return nil
		})
	if errors.Is(err, ErrFairQueueStartLockUnavailable) {
		return result, errors.Join(ErrImageGenerationCapacityLockUnavailable, err)
	}
	return result, err
}

func validateImageGenerationFence(fence ImageGenerationFence) error {
	if !imageTaskIDPattern.MatchString(fence.TaskID) || !imageBatchIDPattern.MatchString(fence.BatchID) ||
		strings.TrimSpace(fence.UserID) == "" || fence.ClaimGeneration <= 0 ||
		!validImageWorkerID(fence.LeaseOwner) || !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return errors.New("store: invalid image generation execution fence")
	}
	return nil
}

func (d *DBStore) withLiveImageGenerationFenceTx(
	ctx context.Context,
	fence ImageGenerationFence,
	lockTimeout time.Duration,
	fn func(*sql.Tx, *ImageGenerationBatchRecord, *ImageGenerationTaskRecord, time.Time) (bool, error),
) (changed bool, err error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return false, err
	}
	if err := validateImageGenerationFence(fence); err != nil {
		if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
			return false, ErrFairQueueWriterMismatch
		}
		return false, err
	}
	if lockTimeout <= 0 {
		lockTimeout = 5 * time.Second
	}
	err = d.withFairQueueResourceLock(ctx, fence.ExpectedWriterFingerprint, ImageGenerationResource, lockTimeout,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			tx, beginErr := conn.BeginTx(ctx, nil)
			if beginErr != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, beginErr)
			}
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
					callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, rollbackErr)
				}
			}()
			batch, readErr := d.getImageGenerationBatch(ctx, tx, fence.BatchID, true)
			if readErr != nil {
				if errors.Is(readErr, ErrNotFound) {
					return nil
				}
				return readErr
			}
			task, readErr := scanImageTask(tx.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=? FOR UPDATE`, fence.TaskID))
			if readErr != nil {
				if errors.Is(readErr, ErrNotFound) {
					return nil
				}
				return readErr
			}
			var now time.Time
			if readErr = tx.QueryRowContext(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&now); readErr != nil {
				return readErr
			}
			if task.BatchID != batch.ID || task.UserID != batch.UserID || task.UserID != fence.UserID ||
				task.Status != ImageGenerationTaskRunning || task.ClaimGeneration != fence.ClaimGeneration ||
				task.DispatchGeneration != task.ClaimGeneration || task.LeaseOwner != fence.LeaseOwner ||
				task.LeaseUntil == nil || !task.LeaseUntil.After(now) {
				return nil
			}
			changed, readErr = fn(tx, batch, task, now)
			if readErr != nil || !changed {
				return readErr
			}
			return commitImageGenerationTx(ctx, tx, identity)
		})
	return changed, err
}

func (d *DBStore) HeartbeatImageGenerationTask(ctx context.Context, fence ImageGenerationFence, leaseDuration time.Duration) (disposition ImageGenerationHeartbeatDisposition, err error) {
	disposition = ImageGenerationHeartbeatStale
	if leaseDuration <= 0 {
		return disposition, errors.New("store: image generation heartbeat lease must be positive")
	}
	_, err = d.withLiveImageGenerationFenceTx(ctx, fence, 5*time.Second,
		func(tx *sql.Tx, batch *ImageGenerationBatchRecord, task *ImageGenerationTaskRecord, now time.Time) (bool, error) {
			if batch.CancelRequested {
				disposition = ImageGenerationHeartbeatCanceled
				// Commit no mutation is unnecessary; returning false preserves the
				// exact cancel observation while the resource lock orders the read.
				return false, nil
			}
			result, updateErr := tx.ExecContext(ctx, `UPDATE image_generation_tasks SET lease_until=?,heartbeat_at=?,updated_at=? WHERE id=? AND status='RUNNING' AND claim_generation=? AND dispatch_generation=claim_generation AND lease_owner=? AND lease_until>?`, now.Add(leaseDuration), now, now, task.ID, fence.ClaimGeneration, fence.LeaseOwner, now)
			if updateErr != nil {
				return false, updateErr
			}
			rows, rowsErr := result.RowsAffected()
			if rowsErr != nil || rows != 1 {
				return false, rowsErr
			}
			disposition = ImageGenerationHeartbeatExtended
			return true, nil
		})
	return disposition, err
}

func (d *DBStore) FinishImageGenerationTaskRetry(ctx context.Context, fence ImageGenerationFence, errorCode string, nextRun time.Time) (bool, error) {
	return d.withLiveImageGenerationFenceTx(ctx, fence, 5*time.Second,
		func(tx *sql.Tx, batch *ImageGenerationBatchRecord, task *ImageGenerationTaskRecord, now time.Time) (bool, error) {
			if batch.CancelRequested {
				return false, nil
			}
			if !nextRun.After(now) {
				return false, errors.New("store: image generation retry must be scheduled in the future")
			}
			if task.RetryCount >= task.MaxRetry {
				return d.finishLiveImageGenerationTaskTx(ctx, tx, batch, task, imageFinalizeFailed, ImageTaskDoneResult{}, "RETRY_EXHAUSTED")
			}
			result, err := tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='PENDING',retry_count=retry_count+1,dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1,dispatched_at=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=?,error_code=?,error_msg=NULL,updated_at=? WHERE id=? AND status='RUNNING' AND claim_generation=? AND dispatch_generation=claim_generation AND lease_owner=?`, nextRun.UTC(), boundedImageError(errorCode, 64), now, task.ID, fence.ClaimGeneration, fence.LeaseOwner)
			if err != nil {
				return false, err
			}
			rows, err := result.RowsAffected()
			return rows == 1, err
		})
}

func (d *DBStore) finishLiveImageGenerationTaskTx(ctx context.Context, tx *sql.Tx, batch *ImageGenerationBatchRecord, task *ImageGenerationTaskRecord, kind imageTaskFinalizeKind, done ImageTaskDoneResult, errorCode string) (bool, error) {
	if kind == imageFinalizeDone {
		count, valid := imageArtifactCount(done.ArtifactsJSON)
		if !valid || count != task.RequestedCount || strings.TrimSpace(done.ManifestKey) == "" {
			return false, fmt.Errorf("store: DONE image task requires exactly %d artifacts and a manifest", task.RequestedCount)
		}
	}
	var err error
	switch kind {
	case imageFinalizeDone:
		_, err = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='DONE',provider=?,model=?,manifest_key=?,artifacts_json=?,error_code='',error_msg=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?`, done.Provider, done.Model, done.ManifestKey, []byte(done.ArtifactsJSON), task.ID)
	case imageFinalizeFailed:
		_, err = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='FAILED',error_code=?,error_msg=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?`, boundedImageError(errorCode, 64), task.ID)
	case imageFinalizeCanceled:
		_, err = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='CANCELED',dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1,dispatched_at=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=UTC_TIMESTAMP(6),updated_at=UTC_TIMESTAMP(6) WHERE id=?`, task.ID)
	}
	if err != nil {
		return false, err
	}
	_, err = d.recomputeImageBatchOn(ctx, tx, batch)
	return err == nil, err
}

func (d *DBStore) FinishImageGenerationTaskDone(ctx context.Context, fence ImageGenerationFence, done ImageTaskDoneResult) (*ImageGenerationBatchRecord, bool, error) {
	var output *ImageGenerationBatchRecord
	changed, err := d.withLiveImageGenerationFenceTx(ctx, fence, 5*time.Second,
		func(tx *sql.Tx, batch *ImageGenerationBatchRecord, task *ImageGenerationTaskRecord, _ time.Time) (bool, error) {
			changed, finishErr := d.finishLiveImageGenerationTaskTx(ctx, tx, batch, task, imageFinalizeDone, done, "")
			if finishErr == nil && changed {
				output, finishErr = d.getImageGenerationBatch(ctx, tx, batch.ID, false)
			}
			return changed, finishErr
		})
	return output, changed, err
}

func (d *DBStore) FinishImageGenerationTaskFailed(ctx context.Context, fence ImageGenerationFence, errorCode string) (bool, error) {
	return d.withLiveImageGenerationFenceTx(ctx, fence, 5*time.Second,
		func(tx *sql.Tx, batch *ImageGenerationBatchRecord, task *ImageGenerationTaskRecord, _ time.Time) (bool, error) {
			return d.finishLiveImageGenerationTaskTx(ctx, tx, batch, task, imageFinalizeFailed, ImageTaskDoneResult{}, errorCode)
		})
}

func (d *DBStore) FinishImageGenerationTaskCanceled(ctx context.Context, fence ImageGenerationFence) (bool, error) {
	return d.withLiveImageGenerationFenceTx(ctx, fence, 5*time.Second,
		func(tx *sql.Tx, batch *ImageGenerationBatchRecord, task *ImageGenerationTaskRecord, _ time.Time) (bool, error) {
			return d.finishLiveImageGenerationTaskTx(ctx, tx, batch, task, imageFinalizeCanceled, ImageTaskDoneResult{}, "")
		})
}

// SweepExpiredImageGenerationTasks serializes reclaim with exact claim and
// heartbeat. Uncanceled work is armed exactly once; canceled work is finalized
// instead of creating another broker delivery.
func (s *ImageFairQueueStore) SweepExpiredImageGenerationTasks(ctx context.Context, afterSequenceID int64, limit int, lockTimeout time.Duration) (armed []ImageTaskDispatchCandidate, next int64, err error) {
	if err := s.validate(); err != nil {
		return nil, afterSequenceID, err
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, err
	}
	next = afterSequenceID
	err = s.store.withFairQueueResourceLock(ctx, s.expectedWriter, ImageGenerationResource, lockTimeout,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			rows, queryErr := conn.QueryContext(ctx, `SELECT `+qualifiedImageTaskColumns("t")+` FROM image_generation_tasks t
				WHERE t.sequence_id>? AND t.status='RUNNING' AND (t.lease_until IS NULL OR t.lease_until<=UTC_TIMESTAMP(6))
				ORDER BY t.sequence_id LIMIT ?`, afterSequenceID, limit)
			if queryErr != nil {
				return queryErr
			}
			originals, queryErr := scanImageCandidateRows(rows)
			if queryErr != nil {
				return queryErr
			}
			if len(originals) > 0 {
				next = originals[len(originals)-1].Task.SequenceID
			}
			for _, original := range originals {
				tx, beginErr := conn.BeginTx(ctx, nil)
				if beginErr != nil {
					return errors.Join(ErrFairQueueUnsafeConnection, beginErr)
				}
				batch, readErr := s.store.getImageGenerationBatch(ctx, tx, original.Task.BatchID, true)
				if readErr != nil {
					_ = tx.Rollback()
					if errors.Is(readErr, ErrNotFound) {
						continue
					}
					return readErr
				}
				task, readErr := scanImageTask(tx.QueryRowContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks WHERE id=? FOR UPDATE`, original.Task.ID))
				if readErr != nil {
					_ = tx.Rollback()
					return readErr
				}
				var now time.Time
				if readErr = tx.QueryRowContext(ctx, `SELECT UTC_TIMESTAMP(6)`).Scan(&now); readErr != nil {
					_ = tx.Rollback()
					return readErr
				}
				if task.Status != ImageGenerationTaskRunning || task.LeaseUntil == nil || task.LeaseUntil.After(now) ||
					task.ClaimGeneration != original.Guard.ClaimGeneration || task.LeaseOwner != original.Guard.LeaseOwner {
					_ = tx.Rollback()
					continue
				}
				if batch.CancelRequested {
					if _, readErr = tx.ExecContext(ctx, `UPDATE image_generation_tasks SET status='CANCELED',dispatch_generation=GREATEST(dispatch_generation,claim_generation)+1,dispatched_at=NULL,lease_owner=NULL,lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,finished_at=?,updated_at=? WHERE id=? AND status='RUNNING' AND claim_generation=? AND lease_owner=? AND lease_until<=?`, now, now, task.ID, task.ClaimGeneration, task.LeaseOwner, now); readErr == nil {
						_, readErr = s.store.recomputeImageBatchOn(ctx, tx, batch)
					}
				} else if task.DispatchGeneration == task.ClaimGeneration {
					if task.ClaimGeneration == math.MaxInt64 {
						_ = tx.Rollback()
						return ErrImageDispatchGenerationExhausted
					}
					result, updateErr := tx.ExecContext(ctx, `UPDATE image_generation_tasks SET dispatch_generation=claim_generation+1,dispatched_at=NULL,updated_at=? WHERE id=? AND status='RUNNING' AND claim_generation=? AND dispatch_generation=claim_generation AND lease_owner=? AND lease_until<=?`, now, task.ID, task.ClaimGeneration, task.LeaseOwner, now)
					readErr = updateErr
					if updateErr == nil {
						if count, countErr := result.RowsAffected(); countErr != nil {
							readErr = countErr
						} else if count == 1 {
							task.DispatchGeneration++
							task.DispatchedAt = nil
							armed = append(armed, newImageTaskDispatchCandidate(imageTaskDispatchRecord(*task)))
						}
					}
				}
				if readErr != nil {
					_ = tx.Rollback()
					return readErr
				}
				if commitErr := commitImageGenerationTx(ctx, tx, identity); commitErr != nil {
					_ = tx.Rollback()
					return commitErr
				}
			}
			return nil
		})
	if errors.Is(err, ErrFairQueueStartLockUnavailable) {
		err = errors.Join(ErrImageGenerationCapacityLockUnavailable, err)
	}
	return armed, next, err
}
