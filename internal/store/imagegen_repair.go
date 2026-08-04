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

const ImageGenerationResource = "image.generate"

var (
	ErrImageTaskDispatchGuard           = errors.New("store: invalid image task dispatch guard")
	ErrImageTaskDispatchStale           = errors.New("store: stale image task dispatch guard")
	ErrImageDispatchGenerationExhausted = errors.New("store: image task dispatch generation exhausted")
)

type ImageTaskDispatchGuard struct {
	TaskID             string
	SequenceID         int64
	BatchID            string
	UserID             string
	Status             ImageGenerationTaskStatus
	RetryCount         int
	ClaimGeneration    int64
	DispatchGeneration int64
	LeaseOwner         string
	NextRunAt          *time.Time
	LeaseUntil         *time.Time
	DispatchedAt       *time.Time
}

type ImageTaskDispatchCandidate struct {
	Task  ImageTaskDispatchRecord
	Guard ImageTaskDispatchGuard
}

// ImageTaskDispatchRecord is deliberately prompt-free. It is the only task
// shape allowed to cross from the MySQL domain into dispatch/recovery code.
type ImageTaskDispatchRecord struct {
	ID                 string
	SequenceID         int64
	BatchID            string
	UserID             string
	Status             ImageGenerationTaskStatus
	RetryCount         int
	MaxRetry           int
	ClaimGeneration    int64
	DispatchGeneration int64
	LeaseOwner         string
	LeaseUntil         *time.Time
	HeartbeatAt        *time.Time
	NextRunAt          *time.Time
	DispatchedAt       *time.Time
}

type ImageRunningTaskSnapshot struct {
	Task          ImageTaskDispatchRecord
	ObservedDBNow time.Time
}

type ImagePoisonRepairLocator struct {
	TaskID     string
	Generation int64
}

type ImagePoisonRepairDisposition string

const (
	ImagePoisonRepairRearmed     ImagePoisonRepairDisposition = "rearmed"
	ImagePoisonRepairStale       ImagePoisonRepairDisposition = "stale-noop"
	ImagePoisonRepairUnlocatable ImagePoisonRepairDisposition = "unlocatable"
)

func cloneImageTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

func imageTaskDispatchRecord(task ImageGenerationTaskRecord) ImageTaskDispatchRecord {
	return ImageTaskDispatchRecord{
		ID: task.ID, SequenceID: task.SequenceID, BatchID: task.BatchID, UserID: task.UserID,
		Status: task.Status, RetryCount: task.RetryCount, MaxRetry: task.MaxRetry,
		ClaimGeneration: task.ClaimGeneration, DispatchGeneration: task.DispatchGeneration,
		LeaseOwner: task.LeaseOwner, LeaseUntil: cloneImageTime(task.LeaseUntil),
		HeartbeatAt: cloneImageTime(task.HeartbeatAt), NextRunAt: cloneImageTime(task.NextRunAt),
		DispatchedAt: cloneImageTime(task.DispatchedAt),
	}
}

func newImageTaskDispatchCandidate(task ImageTaskDispatchRecord) ImageTaskDispatchCandidate {
	return ImageTaskDispatchCandidate{Task: task, Guard: ImageTaskDispatchGuard{
		TaskID: task.ID, SequenceID: task.SequenceID, BatchID: task.BatchID, UserID: task.UserID,
		Status: task.Status, RetryCount: task.RetryCount, ClaimGeneration: task.ClaimGeneration,
		DispatchGeneration: task.DispatchGeneration, LeaseOwner: task.LeaseOwner,
		NextRunAt: cloneImageTime(task.NextRunAt), LeaseUntil: cloneImageTime(task.LeaseUntil),
		DispatchedAt: cloneImageTime(task.DispatchedAt),
	}}
}

func validImageTaskDispatchCandidate(candidate ImageTaskDispatchCandidate) bool {
	task, guard := candidate.Task, candidate.Guard
	return imageTaskIDPattern.MatchString(task.ID) && task.ID == guard.TaskID &&
		task.SequenceID == guard.SequenceID && task.BatchID == guard.BatchID && task.UserID == guard.UserID &&
		task.Status == guard.Status && task.RetryCount == guard.RetryCount &&
		task.ClaimGeneration == guard.ClaimGeneration && task.DispatchGeneration == guard.DispatchGeneration &&
		task.LeaseOwner == guard.LeaseOwner && equalImageTime(task.NextRunAt, guard.NextRunAt) &&
		equalImageTime(task.LeaseUntil, guard.LeaseUntil) && equalImageTime(task.DispatchedAt, guard.DispatchedAt)
}

func equalImageTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func validateImagePage(after int64, limit int) error {
	if after < 0 || limit < 1 || limit > 10_000 {
		return fmt.Errorf("store: invalid image task page after=%d limit=%d", after, limit)
	}
	return nil
}

func imageTaskDuePredicate(alias string) string {
	return fmt.Sprintf(`(%[1]s.next_run_at IS NULL OR %[1]s.next_run_at<=UTC_TIMESTAMP(6))
		AND (%[1]s.status='PENDING' OR %[1]s.lease_until IS NULL OR %[1]s.lease_until<=UTC_TIMESTAMP(6))`, alias)
}

func qualifiedImageTaskColumns(alias string) string {
	parts := strings.Split(imageTaskColumns, ",")
	for index := range parts {
		parts[index] = alias + "." + strings.TrimSpace(parts[index])
	}
	return strings.Join(parts, ",")
}

func scanImageCandidateRows(rows *sql.Rows) ([]ImageTaskDispatchCandidate, error) {
	tasks, err := scanImageTaskRows(rows)
	if err != nil {
		return nil, err
	}
	result := make([]ImageTaskDispatchCandidate, len(tasks))
	for i := range tasks {
		result[i] = newImageTaskDispatchCandidate(imageTaskDispatchRecord(tasks[i]))
	}
	return result, nil
}

func (d *DBStore) ListDispatchableImageTasksPage(ctx context.Context, afterSequenceID int64, limit int) ([]ImageTaskDispatchCandidate, int64, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, afterSequenceID, err
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, err
	}
	query := `SELECT ` + qualifiedImageTaskColumns("t") + ` FROM image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		WHERE t.sequence_id>? AND t.status IN ('PENDING','RUNNING')
		AND t.dispatch_generation>t.claim_generation AND t.dispatched_at IS NULL
		AND b.cancel_requested=FALSE AND ` + imageTaskDuePredicate("t") + `
		ORDER BY t.sequence_id LIMIT ?`
	rows, err := d.db.QueryContext(ctx, query, afterSequenceID, limit)
	if err != nil {
		return nil, afterSequenceID, err
	}
	candidates, err := scanImageCandidateRows(rows)
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(candidates) > 0 {
		next = candidates[len(candidates)-1].Task.SequenceID
	}
	return candidates, next, nil
}

func (d *DBStore) GetDispatchableImageTaskByID(ctx context.Context, taskID string) (*ImageTaskDispatchCandidate, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, err
	}
	query := `SELECT ` + qualifiedImageTaskColumns("t") + ` FROM image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		WHERE t.id=? AND t.status IN ('PENDING','RUNNING')
		AND t.dispatch_generation>t.claim_generation AND t.dispatched_at IS NULL
		AND b.cancel_requested=FALSE AND ` + imageTaskDuePredicate("t")
	task, err := scanImageTask(d.db.QueryRowContext(ctx, query, taskID))
	if err != nil {
		return nil, err
	}
	candidate := newImageTaskDispatchCandidate(imageTaskDispatchRecord(*task))
	return &candidate, nil
}

func (d *DBStore) MarkImageTaskDispatched(ctx context.Context, candidate ImageTaskDispatchCandidate, dispatchGeneration int64) (bool, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return false, err
	}
	if !validImageTaskDispatchCandidate(candidate) || candidate.Guard.DispatchedAt != nil ||
		candidate.Guard.DispatchGeneration <= candidate.Guard.ClaimGeneration ||
		dispatchGeneration != candidate.Guard.DispatchGeneration {
		return false, ErrImageTaskDispatchGuard
	}
	guard := candidate.Guard
	result, err := d.db.ExecContext(ctx, `UPDATE image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		SET t.dispatched_at=UTC_TIMESTAMP(6),t.updated_at=UTC_TIMESTAMP(6)
		WHERE t.id=? AND t.sequence_id=? AND t.batch_id=? AND t.user_id=? AND t.status=?
		AND t.retry_count=? AND t.claim_generation=? AND t.dispatch_generation=?
		AND COALESCE(t.lease_owner,'')=? AND t.next_run_at<=>? AND t.lease_until<=>?
		AND t.dispatched_at IS NULL AND t.dispatch_generation>t.claim_generation
		AND b.cancel_requested=FALSE AND `+imageTaskDuePredicate("t"), guard.TaskID,
		guard.SequenceID, guard.BatchID, guard.UserID, guard.Status, guard.RetryCount,
		guard.ClaimGeneration, guard.DispatchGeneration, guard.LeaseOwner,
		guard.NextRunAt, guard.LeaseUntil)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count != 1 {
		return false, ErrImageTaskDispatchStale
	}
	return true, nil
}

func (d *DBStore) ArmExpiredImageTasks(ctx context.Context, afterSequenceID int64, limit int) ([]ImageTaskDispatchCandidate, int64, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, afterSequenceID, err
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, afterSequenceID, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+qualifiedImageTaskColumns("t")+` FROM image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		WHERE t.sequence_id>? AND t.status='RUNNING' AND t.dispatch_generation=t.claim_generation
		AND (t.lease_until IS NULL OR t.lease_until<=UTC_TIMESTAMP(6))
		AND (t.next_run_at IS NULL OR t.next_run_at<=UTC_TIMESTAMP(6))
		AND b.cancel_requested=FALSE ORDER BY t.sequence_id LIMIT ?`, afterSequenceID, limit)
	if err != nil {
		return nil, afterSequenceID, err
	}
	originals, err := scanImageCandidateRows(rows)
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(originals) > 0 {
		next = originals[len(originals)-1].Task.SequenceID
	}
	armed := make([]ImageTaskDispatchCandidate, 0, len(originals))
	for _, original := range originals {
		guard := original.Guard
		if guard.ClaimGeneration == math.MaxInt64 {
			return nil, afterSequenceID, ErrImageDispatchGenerationExhausted
		}
		result, err := tx.ExecContext(ctx, `UPDATE image_generation_tasks t
			JOIN image_generation_batches b ON b.id=t.batch_id
			SET t.dispatch_generation=t.claim_generation+1,t.dispatched_at=NULL,t.updated_at=UTC_TIMESTAMP(6)
			WHERE t.id=? AND t.sequence_id=? AND t.batch_id=? AND t.user_id=? AND t.status='RUNNING'
			AND t.retry_count=? AND t.claim_generation=? AND t.dispatch_generation=?
			AND COALESCE(t.lease_owner,'')=? AND t.next_run_at<=>? AND t.lease_until<=>?
			AND t.dispatched_at<=>?
			AND t.dispatch_generation=t.claim_generation
			AND (t.lease_until IS NULL OR t.lease_until<=UTC_TIMESTAMP(6))
			AND (t.next_run_at IS NULL OR t.next_run_at<=UTC_TIMESTAMP(6))
			AND b.cancel_requested=FALSE`, guard.TaskID, guard.SequenceID, guard.BatchID,
			guard.UserID, guard.RetryCount, guard.ClaimGeneration, guard.DispatchGeneration,
			guard.LeaseOwner, guard.NextRunAt, guard.LeaseUntil, guard.DispatchedAt)
		if err != nil {
			return nil, afterSequenceID, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return nil, afterSequenceID, err
		}
		if count == 1 {
			original.Task.DispatchGeneration = original.Task.ClaimGeneration + 1
			original.Task.DispatchedAt = nil
			armed = append(armed, newImageTaskDispatchCandidate(original.Task))
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, afterSequenceID, err
	}
	return armed, next, nil
}

func (d *DBStore) CaptureImageFairQueueHighWater(ctx context.Context) (int64, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return 0, err
	}
	var result int64
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence_id),0) FROM image_generation_tasks`).Scan(&result)
	return result, err
}

func (d *DBStore) ListCanonicalImageTenants(ctx context.Context, highWater int64, afterUserID string, limit int) ([]string, string, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, afterUserID, err
	}
	if highWater < 0 || limit < 1 || limit > 10_000 {
		return nil, afterUserID, errors.New("store: invalid image tenant page")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT DISTINCT user_id FROM image_generation_tasks
		WHERE sequence_id<=? AND user_id>? ORDER BY user_id LIMIT ?`, highWater, afterUserID, limit)
	if err != nil {
		return nil, afterUserID, err
	}
	defer rows.Close()
	var tenants []string
	next := afterUserID
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, afterUserID, err
		}
		tenants = append(tenants, userID)
		next = userID
	}
	return tenants, next, rows.Err()
}

func (d *DBStore) ListDispatchedImageTasks(ctx context.Context, highWater, afterSequenceID int64, limit int) ([]ImageTaskDispatchRecord, int64, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, afterSequenceID, err
	}
	if highWater < 0 {
		return nil, afterSequenceID, errors.New("store: invalid dispatched image task page")
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, errors.New("store: invalid dispatched image task page")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+imageTaskColumns+` FROM image_generation_tasks
		WHERE sequence_id>? AND sequence_id<=? AND dispatched_at IS NOT NULL
		AND ((status='PENDING' AND dispatch_generation>claim_generation) OR
			(status='RUNNING' AND dispatch_generation>=claim_generation))
		ORDER BY sequence_id LIMIT ?`, afterSequenceID, highWater, limit)
	if err != nil {
		return nil, afterSequenceID, err
	}
	fullTasks, err := scanImageTaskRows(rows)
	if err != nil {
		return nil, afterSequenceID, err
	}
	tasks := make([]ImageTaskDispatchRecord, len(fullTasks))
	for index := range fullTasks {
		tasks[index] = imageTaskDispatchRecord(fullTasks[index])
	}
	next := afterSequenceID
	if len(tasks) > 0 {
		next = tasks[len(tasks)-1].SequenceID
	}
	return tasks, next, nil
}

func (d *DBStore) ListValidRunningImageTasks(ctx context.Context, highWater, afterSequenceID int64, limit int) ([]ImageRunningTaskSnapshot, int64, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, afterSequenceID, err
	}
	if highWater < 0 {
		return nil, afterSequenceID, errors.New("store: invalid running image task page")
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, errors.New("store: invalid running image task page")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+imageTaskColumns+`,UTC_TIMESTAMP(6)
		FROM image_generation_tasks WHERE sequence_id>? AND sequence_id<=? AND status='RUNNING'
		AND dispatch_generation=claim_generation AND claim_generation>0
		AND COALESCE(lease_owner,'')<>'' AND lease_until>UTC_TIMESTAMP(6) AND heartbeat_at IS NOT NULL
		AND next_run_at IS NULL
		ORDER BY sequence_id LIMIT ?`, afterSequenceID, highWater, limit)
	if err != nil {
		return nil, afterSequenceID, err
	}
	defer rows.Close()
	var result []ImageRunningTaskSnapshot
	next := afterSequenceID
	for rows.Next() {
		task, observed, err := scanImageRunningTask(rows)
		if err != nil {
			return nil, afterSequenceID, err
		}
		result = append(result, ImageRunningTaskSnapshot{Task: imageTaskDispatchRecord(*task), ObservedDBNow: observed})
		next = task.SequenceID
	}
	return result, next, rows.Err()
}

func scanImageRunningTask(scanner imageTaskScanner) (*ImageGenerationTaskRecord, time.Time, error) {
	var record ImageGenerationTaskRecord
	var leaseOwner sql.NullString
	var leaseUntil, heartbeatAt, nextRunAt, dispatchedAt, startedAt, finishedAt sql.NullTime
	var artifacts []byte
	var errorMessage sql.NullString
	var observed time.Time
	err := scanner.Scan(&record.ID, &record.SequenceID, &record.BatchID, &record.UserID,
		&record.ItemIndex, &record.ChunkIndex, &record.Label, &record.Prompt, &record.Size,
		&record.RequestedCount, &record.RequestFingerprint, &record.Status, &record.RetryCount,
		&record.MaxRetry, &record.ClaimGeneration, &record.DispatchGeneration, &leaseOwner,
		&leaseUntil, &heartbeatAt, &nextRunAt, &dispatchedAt, &record.Provider, &record.Model,
		&record.ManifestKey, &artifacts, &record.ErrorCode, &errorMessage, &record.CreatedAt,
		&startedAt, &finishedAt, &record.UpdatedAt, &observed)
	if err != nil {
		return nil, time.Time{}, scanErr(err)
	}
	if leaseOwner.Valid {
		record.LeaseOwner = leaseOwner.String
	}
	if leaseUntil.Valid {
		value := leaseUntil.Time
		record.LeaseUntil = &value
	}
	if heartbeatAt.Valid {
		value := heartbeatAt.Time
		record.HeartbeatAt = &value
	}
	if nextRunAt.Valid {
		value := nextRunAt.Time
		record.NextRunAt = &value
	}
	if dispatchedAt.Valid {
		value := dispatchedAt.Time
		record.DispatchedAt = &value
	}
	if startedAt.Valid {
		value := startedAt.Time
		record.StartedAt = &value
	}
	if finishedAt.Valid {
		value := finishedAt.Time
		record.FinishedAt = &value
	}
	if artifacts != nil {
		record.ArtifactsJSON = append([]byte(nil), artifacts...)
	}
	if errorMessage.Valid {
		record.ErrorMessage = errorMessage.String
	}
	return &record, observed, nil
}

func (d *DBStore) CaptureImageBrokerRepairHighWater(ctx context.Context) (int64, error) {
	return d.CaptureImageFairQueueHighWater(ctx)
}

func (d *DBStore) ListBrokerBackedImageCandidates(ctx context.Context, highWater, afterSequenceID int64, limit int) ([]ImageTaskDispatchCandidate, int64, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, afterSequenceID, err
	}
	if highWater < 0 {
		return nil, afterSequenceID, errors.New("store: invalid broker image task page")
	}
	if err := validateImagePage(afterSequenceID, limit); err != nil {
		return nil, afterSequenceID, errors.New("store: invalid broker image task page")
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+qualifiedImageTaskColumns("t")+` FROM image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		WHERE t.sequence_id>? AND t.sequence_id<=? AND t.status IN ('PENDING','RUNNING')
		AND t.dispatch_generation>t.claim_generation AND t.dispatched_at IS NOT NULL
		AND b.cancel_requested=FALSE AND `+imageTaskDuePredicate("t")+`
		ORDER BY t.sequence_id LIMIT ?`, afterSequenceID, highWater, limit)
	if err != nil {
		return nil, afterSequenceID, err
	}
	candidates, err := scanImageCandidateRows(rows)
	if err != nil {
		return nil, afterSequenceID, err
	}
	next := afterSequenceID
	if len(candidates) > 0 {
		next = candidates[len(candidates)-1].Task.SequenceID
	}
	return candidates, next, nil
}

func (d *DBStore) rearmImageCandidate(ctx context.Context, original ImageTaskDispatchCandidate) (*ImageTaskDispatchCandidate, bool, error) {
	if !validImageTaskDispatchCandidate(original) || original.Guard.DispatchedAt == nil ||
		original.Guard.DispatchGeneration <= original.Guard.ClaimGeneration ||
		original.Guard.DispatchGeneration == math.MaxInt64 {
		return nil, false, ErrImageTaskDispatchGuard
	}
	guard := original.Guard
	result, err := d.db.ExecContext(ctx, `UPDATE image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		SET t.dispatch_generation=GREATEST(t.dispatch_generation,t.claim_generation)+1,
			t.dispatched_at=NULL,t.updated_at=UTC_TIMESTAMP(6)
		WHERE t.id=? AND t.sequence_id=? AND t.batch_id=? AND t.user_id=? AND t.status=?
		AND t.retry_count=? AND t.claim_generation=? AND t.dispatch_generation=?
		AND COALESCE(t.lease_owner,'')=? AND t.next_run_at<=>? AND t.lease_until<=>?
		AND t.dispatched_at<=>? AND t.status IN ('PENDING','RUNNING')
		AND t.dispatch_generation>t.claim_generation AND b.cancel_requested=FALSE
		AND `+imageTaskDuePredicate("t"), guard.TaskID, guard.SequenceID, guard.BatchID,
		guard.UserID, guard.Status, guard.RetryCount, guard.ClaimGeneration,
		guard.DispatchGeneration, guard.LeaseOwner, guard.NextRunAt, guard.LeaseUntil,
		guard.DispatchedAt)
	if err != nil {
		return nil, false, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return nil, false, err
	}
	original.Task.DispatchGeneration = maxImageGeneration(original.Task.DispatchGeneration, original.Task.ClaimGeneration) + 1
	original.Task.DispatchedAt = nil
	candidate := newImageTaskDispatchCandidate(original.Task)
	return &candidate, true, nil
}

func maxImageGeneration(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (d *DBStore) RearmImageCandidateAfterBrokerLoss(ctx context.Context, original ImageTaskDispatchCandidate) (*ImageTaskDispatchCandidate, bool, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, false, err
	}
	return d.rearmImageCandidate(ctx, original)
}

func (d *DBStore) RepairPoisonImageCandidate(ctx context.Context, locator ImagePoisonRepairLocator, registeredResource, queueTenantHash string) (*ImageTaskDispatchCandidate, ImagePoisonRepairDisposition, error) {
	if err := d.requireImagegenMySQL(); err != nil {
		return nil, ImagePoisonRepairUnlocatable, err
	}
	if registeredResource != ImageGenerationResource {
		return nil, ImagePoisonRepairUnlocatable, errors.New("store: poison repair resource mismatch")
	}
	if !imageTaskIDPattern.MatchString(locator.TaskID) || locator.Generation <= 0 {
		return nil, ImagePoisonRepairUnlocatable, nil
	}
	query := `SELECT ` + qualifiedImageTaskColumns("t") + ` FROM image_generation_tasks t
		JOIN image_generation_batches b ON b.id=t.batch_id
		WHERE t.id=? AND t.status IN ('PENDING','RUNNING') AND t.dispatch_generation>t.claim_generation
		AND b.cancel_requested=FALSE AND ` + imageTaskDuePredicate("t")
	task, err := scanImageTask(d.db.QueryRowContext(ctx, query, locator.TaskID))
	if errors.Is(err, ErrNotFound) {
		return nil, ImagePoisonRepairUnlocatable, nil
	}
	if err != nil {
		return nil, ImagePoisonRepairUnlocatable, err
	}
	hash, err := fairqueue.TenantHash(registeredResource, task.UserID)
	if err != nil || hash != queueTenantHash || task.DispatchGeneration != locator.Generation {
		return nil, ImagePoisonRepairStale, nil
	}
	original := newImageTaskDispatchCandidate(imageTaskDispatchRecord(*task))
	if original.Task.DispatchedAt == nil {
		return nil, ImagePoisonRepairStale, nil
	}
	rearmed, changed, err := d.rearmImageCandidate(ctx, original)
	if err != nil {
		return nil, ImagePoisonRepairStale, err
	}
	if !changed {
		return nil, ImagePoisonRepairStale, nil
	}
	return rearmed, ImagePoisonRepairRearmed, nil
}
