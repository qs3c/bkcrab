package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// IndexFence is the immutable capability held by one claimed index attempt.
// Every worker mutation must match all fields and an unexpired database lease.
type IndexFence struct {
	TaskID          int64
	DocID           string
	DocVersion      int64
	ClaimGeneration int64
	LeaseOwner      string
}

type RAGIndexClaim struct {
	Task    RAGIndexTaskRecord
	Version RAGDocumentVersionRecord
	Fence   IndexFence
}

type RAGIndexProgress struct {
	Stage   string
	Current int
	Total   int
	Unit    string
}

type RAGIndexActivation struct {
	VersionResult RAGDocumentVersionResult
	ChunkCount    int
	TokenCount    int
}

// RAGLegacyTaskSnapshotBuilder is supplied only after the RAG service and its
// immutable provider/config snapshot dependencies have been assembled.
type RAGLegacyTaskSnapshotBuilder func(
	context.Context,
	*RAGDocumentRecord,
	int64,
) (*RAGDocumentVersionRecord, error)

func (d *DBStore) ragDBNow(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (time.Time, error) {
	var raw any
	if err := queryer.QueryRowContext(ctx, "SELECT "+d.ragNowExpr()).Scan(&raw); err != nil {
		return time.Time{}, err
	}
	switch value := raw.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseRAGDBTime(value)
	case []byte:
		return parseRAGDBTime(string(value))
	default:
		return time.Time{}, fmt.Errorf("store: unsupported database time %T", raw)
	}
}

func parseRAGDBTime(value string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("store: parse database time %q", value)
}

func (d *DBStore) ragLockSuffix() string {
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		return " FOR UPDATE"
	}
	return ""
}

func (d *DBStore) ragNowExpr() string {
	switch d.dialect {
	case "postgres":
		return "(clock_timestamp() AT TIME ZONE 'UTC')"
	case mysqlDialect:
		return "UTC_TIMESTAMP(6)"
	default:
		return "CURRENT_TIMESTAMP"
	}
}

func ragDocumentSourceHash(current, snapshot string) (normalized string, fill bool, err error) {
	normalized = snapshot
	if !ragCanonicalSHA256(normalized) {
		return "", false, ErrRAGDocumentVersionMismatch
	}
	current = strings.ToLower(strings.TrimSpace(current))
	if current == "" {
		return normalized, true, nil
	}
	if current != normalized {
		return "", false, ErrRAGDocumentSourceConflict
	}
	return normalized, false, nil
}

func (d *DBStore) reconcileRAGDocumentSourceHash(
	ctx context.Context,
	tx *sql.Tx,
	doc *RAGDocumentRecord,
	snapshotSource string,
) error {
	normalized, fill, err := ragDocumentSourceHash(doc.SourceSHA256, snapshotSource)
	if err != nil {
		return err
	}
	if !fill {
		return nil
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET source_sha256=%s
		WHERE id=%s AND source_sha256=''`, d.ph(1), d.ph(2)), normalized, doc.ID)
	if err != nil {
		return err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return err
	}
	doc.SourceSHA256 = normalized
	return nil
}

func ragRowsAffected(result sql.Result) (bool, error) {
	if result == nil {
		return false, nil
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func ragIsNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound)
}

func (d *DBStore) ragDocumentInTx(ctx context.Context, tx *sql.Tx, docID string) (*RAGDocumentRecord, error) {
	return scanRAGDocument(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), docID))
}

func (d *DBStore) ragTaskInTx(ctx context.Context, tx *sql.Tx, taskID int64) (*RAGIndexTaskRecord, error) {
	return scanRAGIndexTask(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), taskID))
}

func (d *DBStore) ragVersionInTx(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
	docVersion int64,
) (*RAGDocumentVersionRecord, error) {
	return scanRAGDocumentVersion(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentVersionColumns+` FROM rag_document_versions
		 WHERE doc_id=%s AND doc_version=%s%s`, d.ph(1), d.ph(2), d.ragLockSuffix()),
		docID, docVersion))
}

func ragTaskDue(task *RAGIndexTaskRecord, now time.Time) bool {
	switch task.Status {
	case "PENDING":
		return task.NextRunAt == nil || !task.NextRunAt.After(now)
	case "RUNNING":
		return task.LeaseUntil == nil || !task.LeaseUntil.After(now)
	default:
		return false
	}
}

func validRAGWorkerID(workerID string) bool {
	return workerID != "" && workerID == strings.TrimSpace(workerID) && len([]byte(workerID)) <= 96
}

// ClaimRAGIndexTask claims the oldest due task. A nil claim and nil error mean
// no work is currently due. Candidate selection is only a hint; all state is
// re-read under document -> task -> version locks and the final task update is
// a database-time compare-and-set.
func (d *DBStore) ClaimRAGIndexTask(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (*RAGIndexClaim, error) {
	if !validRAGWorkerID(workerID) {
		return nil, errors.New("store: RAG worker id must be 1..96 trimmed bytes")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("store: RAG lease duration must be positive")
	}
	for scanned := 0; scanned < 64; scanned++ {
		claim, consumed, err := d.claimOneRAGIndexTask(ctx, workerID, leaseDuration)
		if err != nil {
			if d.dialect == "sqlite" && ragSQLiteBusy(err) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(5 * time.Millisecond):
					continue
				}
			}
			return nil, err
		}
		if claim != nil || !consumed {
			return claim, nil
		}
	}
	return nil, errors.New("store: too many invalid RAG index tasks while claiming")
}

func ragSQLiteBusy(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "database is locked") ||
		strings.Contains(message, "sqlite_busy") || strings.Contains(message, "sqlite_locked")
}

func (d *DBStore) claimOneRAGIndexTask(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (*RAGIndexClaim, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	nowExpr := d.ragNowExpr()
	var taskID int64
	var docID string
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,doc_id FROM rag_index_tasks
		WHERE doc_version IS NOT NULL AND doc_version > 0 AND (
			(status='PENDING' AND (next_run_at IS NULL OR next_run_at <= %s)) OR
			(status='RUNNING' AND (lease_until IS NULL OR lease_until <= %s)))
		ORDER BY created_at,id LIMIT 1`, nowExpr, nowExpr)).Scan(&taskID, &docID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}

	doc, err := d.ragDocumentInTx(ctx, tx, docID)
	if errors.Is(err, sql.ErrNoRows) {
		if _, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
			status='FAILED',error_msg='orphan index task',finished_at=%s,
			lease_owner='',lease_until=NULL,heartbeat_at=NULL
			WHERE id=%s AND status IN ('PENDING','RUNNING')`, nowExpr, d.ph(1)), taskID); updateErr != nil {
			return nil, false, updateErr
		}
		return nil, true, tx.Commit()
	}
	if err != nil {
		return nil, false, err
	}
	task, err := d.ragTaskInTx(ctx, tx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if task.DocID != doc.ID || !ragTaskDue(task, now) {
		return nil, true, nil
	}
	if doc.Version != task.DocVersion {
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
			"stale task does not match document target version"); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	version, err := d.ragVersionInTx(ctx, tx, task.DocID, task.DocVersion)
	if errors.Is(err, sql.ErrNoRows) {
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task, "missing immutable version snapshot"); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	if err != nil {
		return nil, false, err
	}
	if validationErr := d.validateRAGVersionSnapshotForDocument(ctx, tx, doc, version); validationErr != nil {
		if !errors.Is(validationErr, ErrRAGDocumentVersionIncomplete) &&
			!errors.Is(validationErr, ErrRAGDocumentVersionMismatch) &&
			!errors.Is(validationErr, ErrRAGDocumentSourceConflict) &&
			!errors.Is(validationErr, ErrNotFound) {
			return nil, false, validationErr
		}
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
			"invalid immutable version snapshot: "+validationErr.Error()); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, doc, version.SourceSHA256); err != nil {
		if errors.Is(err, ErrRAGDocumentVersionMismatch) || errors.Is(err, ErrRAGDocumentSourceConflict) {
			if failErr := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
				"invalid immutable version source: "+err.Error()); failErr != nil {
				return nil, false, failErr
			}
			return nil, true, tx.Commit()
		}
		return nil, false, err
	}

	oldStatus := task.Status
	oldVersion := task.DocVersion
	retryCount := task.RetryCount
	allocateFresh := false
	switch {
	case task.Status == "PENDING" && version.Status == RAGDocumentVersionPending:
	case task.Status == "PENDING" && version.Status == RAGDocumentVersionFailed:
		allocateFresh = true
	case task.Status == "RUNNING" && version.Status == RAGDocumentVersionRunning:
		if retryCount >= task.MaxRetry {
			if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task, "index lease expired after retry budget exhausted"); err != nil {
				return nil, false, err
			}
			return nil, true, tx.Commit()
		}
		retryCount++
		allocateFresh = true
	default:
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task, "invalid task/version state"); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}

	if allocateFresh {
		terminalStatus := RAGDocumentVersionFailed
		if oldStatus == "RUNNING" {
			terminalStatus = RAGDocumentVersionSuperseded
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
			status=%s,updated_at=%s WHERE doc_id=%s AND doc_version=%s AND status=%s`,
			d.ph(1), nowExpr, d.ph(2), d.ph(3), d.ph(4)), terminalStatus,
			task.DocID, oldVersion, version.Status)
		if err != nil {
			return nil, false, err
		}
		if ok, err := ragRowsAffected(result); err != nil || !ok {
			return nil, true, err
		}
		newVersion, err := d.nextRAGDocumentVersionInTx(ctx, tx, doc)
		if err != nil {
			return nil, false, err
		}
		copyVersion := *version
		copyVersion.DocVersion = newVersion
		copyVersion.CreatedAt = now
		copyVersion.UpdatedAt = now
		prepareNewRAGDocumentVersion(&copyVersion)
		copyVersion.Status = RAGDocumentVersionRunning
		if err := d.createRAGDocumentVersion(ctx, tx, &copyVersion); err != nil {
			return nil, false, err
		}
		version = &copyVersion
		task.DocVersion = newVersion
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			version=%s,status='PROCESSING',error_msg='',processing_stage='claimed',
			progress_current=0,progress_total=0,progress_unit='',degraded=FALSE,warning_count=0
			WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3)),
			newVersion, doc.ID, doc.Version); err != nil {
			return nil, false, err
		}
		doc.Version = newVersion
	} else {
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
			status='RUNNING',updated_at=%s WHERE doc_id=%s AND doc_version=%s AND status='PENDING'`,
			nowExpr, d.ph(1), d.ph(2)), task.DocID, task.DocVersion)
		if err != nil {
			return nil, false, err
		}
		if ok, err := ragRowsAffected(result); err != nil || !ok {
			return nil, true, err
		}
		version.Status = RAGDocumentVersionRunning
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			status='PROCESSING',error_msg='',processing_stage='claimed'
			WHERE id=%s AND version=%s`, d.ph(1), d.ph(2)), doc.ID, task.DocVersion); err != nil {
			return nil, false, err
		}
	}

	leaseUntil := now.Add(leaseDuration)
	newGeneration := task.ClaimGeneration + 1
	condition := "status='PENDING' AND (next_run_at IS NULL OR next_run_at <= " + nowExpr + ")"
	if oldStatus == "RUNNING" {
		condition = "status='RUNNING' AND (lease_until IS NULL OR lease_until <= " + nowExpr + ")"
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		doc_version=%s,status='RUNNING',retry_count=%s,claim_generation=%s,
		lease_owner=%s,lease_until=%s,heartbeat_at=%s,next_run_at=NULL,error_msg='',
		started_at=COALESCE(started_at,%s),finished_at=NULL
		WHERE id=%s AND doc_version=%s AND claim_generation=%s AND %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), nowExpr, nowExpr,
		d.ph(6), d.ph(7), d.ph(8), condition), task.DocVersion, retryCount,
		newGeneration, workerID, leaseUntil, task.ID, oldVersion,
		task.ClaimGeneration)
	if err != nil {
		return nil, false, err
	}
	if ok, err := ragRowsAffected(result); err != nil || !ok {
		return nil, true, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	task.Status = "RUNNING"
	task.RetryCount = retryCount
	task.ClaimGeneration = newGeneration
	task.LeaseOwner = workerID
	task.LeaseUntil = &leaseUntil
	task.HeartbeatAt = &now
	task.NextRunAt = nil
	task.ErrorMsg = ""
	fence := IndexFence{
		TaskID: task.ID, DocID: task.DocID, DocVersion: task.DocVersion,
		ClaimGeneration: task.ClaimGeneration, LeaseOwner: task.LeaseOwner,
	}
	return &RAGIndexClaim{Task: *task, Version: *version, Fence: fence}, true, nil
}

func (d *DBStore) nextRAGDocumentVersionInTx(
	ctx context.Context,
	tx *sql.Tx,
	doc *RAGDocumentRecord,
) (int64, error) {
	var maximum int64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(known_version),0) FROM (
		SELECT doc_version AS known_version FROM rag_document_versions WHERE doc_id=%s
		UNION ALL
		SELECT doc_version AS known_version FROM rag_index_tasks
			WHERE doc_id=%s AND doc_version IS NOT NULL
	) known_versions`, d.ph(1), d.ph(2)), doc.ID, doc.ID).Scan(&maximum); err != nil {
		return 0, err
	}
	if doc.Version > maximum {
		maximum = doc.Version
	}
	return maximum + 1, nil
}

func (d *DBStore) failInvalidRAGTaskInTx(
	ctx context.Context,
	tx *sql.Tx,
	doc *RAGDocumentRecord,
	task *RAGIndexTaskRecord,
	reason string,
) error {
	nowExpr := d.ragNowExpr()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='FAILED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status IN ('PENDING','RUNNING')`, nowExpr, d.ph(1), d.ph(2)),
		task.DocID, task.DocVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status='FAILED',error_msg=%s,finished_at=%s,lease_owner='',lease_until=NULL,
		heartbeat_at=NULL,next_run_at=NULL WHERE id=%s AND doc_version=%s
		AND claim_generation=%s AND status IN ('PENDING','RUNNING')`,
		d.ph(1), nowExpr, d.ph(2), d.ph(3), d.ph(4)), reason, task.ID,
		task.DocVersion, task.ClaimGeneration); err != nil {
		return err
	}
	if doc.Version == task.DocVersion {
		_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			status='FAILED',error_msg=%s,processing_stage='failed'
			WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3)),
			reason, doc.ID, doc.Version)
		return err
	}
	return nil
}

func (d *DBStore) CheckRAGIndexFence(ctx context.Context, fence IndexFence) (bool, error) {
	var present int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT 1 FROM rag_index_tasks
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND claim_generation=%s
		AND lease_owner=%s AND status='RUNNING' AND lease_until > %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ragNowExpr()),
		fence.TaskID, fence.DocID, fence.DocVersion, fence.ClaimGeneration,
		fence.LeaseOwner).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (d *DBStore) HeartbeatRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	leaseDuration time.Duration,
) (bool, error) {
	if leaseDuration <= 0 {
		return false, errors.New("store: RAG lease duration must be positive")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		lease_until=%s,heartbeat_at=%s WHERE id=%s AND doc_id=%s AND doc_version=%s
		AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
		AND lease_until > %s`, d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5), d.ph(6), d.ragNowExpr()), now.Add(leaseDuration),
		fence.TaskID, fence.DocID, fence.DocVersion, fence.ClaimGeneration,
		fence.LeaseOwner)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

type ragLockedIndexFence struct {
	doc     *RAGDocumentRecord
	task    *RAGIndexTaskRecord
	version *RAGDocumentVersionRecord
	now     time.Time
}

func (d *DBStore) lockRAGIndexFence(
	ctx context.Context,
	tx *sql.Tx,
	fence IndexFence,
) (*ragLockedIndexFence, bool, error) {
	doc, err := d.ragDocumentInTx(ctx, tx, fence.DocID)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	task, err := d.ragTaskInTx(ctx, tx, fence.TaskID)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	version, err := d.ragVersionInTx(ctx, tx, fence.DocID, fence.DocVersion)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	valid := task.DocID == fence.DocID && task.DocVersion == fence.DocVersion &&
		task.ClaimGeneration == fence.ClaimGeneration && task.LeaseOwner == fence.LeaseOwner &&
		task.Status == "RUNNING" && task.LeaseUntil != nil && task.LeaseUntil.After(now) &&
		version.Status == RAGDocumentVersionRunning
	if !valid {
		return nil, false, nil
	}
	return &ragLockedIndexFence{doc: doc, task: task, version: version, now: now}, true, nil
}

func (d *DBStore) UpdateProgressRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	progress RAGIndexProgress,
) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		processing_stage=%s,progress_current=%s,progress_total=%s,progress_unit=%s
		WHERE id=%s AND version=%s AND EXISTS (SELECT 1 FROM rag_index_tasks t
			WHERE t.id=%s AND t.doc_id=%s AND t.doc_version=%s
			AND t.claim_generation=%s AND t.lease_owner=%s AND t.status='RUNNING'
			AND t.lease_until > %s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4),
		d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11),
		d.ragNowExpr()), progress.Stage, progress.Current, progress.Total, progress.Unit,
		fence.DocID, fence.DocVersion, fence.TaskID, fence.DocID, fence.DocVersion,
		fence.ClaimGeneration, fence.LeaseOwner)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) UpdateWarningRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	degraded bool,
	warningCount int,
) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence)
	if err != nil || !ok {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		degraded=%s,warning_count=%s,updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status='RUNNING'`, d.ph(1), d.ph(2), d.ragNowExpr(), d.ph(3), d.ph(4)),
		degraded, warningCount, fence.DocID, fence.DocVersion); err != nil {
		return false, err
	}
	if locked.doc.Version == fence.DocVersion {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			degraded=%s,warning_count=%s WHERE id=%s AND version=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)), degraded, warningCount,
			fence.DocID, fence.DocVersion); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (d *DBStore) RetryRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	errMsg string,
	nextRunDelay time.Duration,
) (bool, error) {
	if nextRunDelay < 0 {
		nextRunDelay = 0
	}
	return d.finishOrRetryRAGIndexTask(ctx, fence, errMsg, true, nextRunDelay)
}

func (d *DBStore) FailRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	errMsg string,
) (bool, error) {
	return d.finishOrRetryRAGIndexTask(ctx, fence, errMsg, false, 0)
}

func (d *DBStore) finishOrRetryRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	errMsg string,
	transient bool,
	nextRunDelay time.Duration,
) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence)
	if err != nil || !ok {
		return false, err
	}
	retry := transient && locked.task.RetryCount < locked.task.MaxRetry
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='FAILED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status='RUNNING'`, d.ragNowExpr(), d.ph(1), d.ph(2)),
		fence.DocID, fence.DocVersion); err != nil {
		return false, err
	}
	taskStatus := "FAILED"
	docStatus := "FAILED"
	stage := "failed"
	finished := locked.now
	var nextRunAt *time.Time
	retryCount := locked.task.RetryCount
	if retry {
		taskStatus = "PENDING"
		docStatus = "PENDING"
		stage = "retry_wait"
		if nextRunDelay > 0 {
			next := locked.now.Add(nextRunDelay)
			nextRunAt = &next
		}
		retryCount++
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status=%s,retry_count=%s,error_msg=%s,next_run_at=%s,
		lease_owner='',lease_until=NULL,heartbeat_at=NULL,finished_at=%s
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND claim_generation=%s
		AND lease_owner=%s AND status='RUNNING' AND lease_until > %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7),
		d.ph(8), d.ph(9), d.ph(10), d.ragNowExpr()), taskStatus, retryCount,
		errMsg, nextRunAt, func() any {
			if retry {
				return nil
			}
			return finished
		}(), fence.TaskID, fence.DocID, fence.DocVersion, fence.ClaimGeneration,
		fence.LeaseOwner)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}
	if locked.doc.Version == fence.DocVersion {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			status=%s,error_msg=%s,processing_stage=%s WHERE id=%s AND version=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), docStatus, errMsg,
			stage, fence.DocID, fence.DocVersion); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func (d *DBStore) ActivateAndFinishRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	activation RAGIndexActivation,
	gcGracePeriod time.Duration,
) (bool, error) {
	if gcGracePeriod < 0 {
		gcGracePeriod = 0
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence)
	if err != nil || !ok {
		return false, err
	}
	if locked.doc.Version != fence.DocVersion {
		return false, nil
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, locked.doc, locked.version.SourceSHA256); err != nil {
		return false, err
	}
	activation.VersionResult.Status = RAGDocumentVersionDone
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='DONE',parse_artifact_key=%s,page_count=%s,asset_count=%s,degraded=%s,
		warning_count=%s,updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status='RUNNING'`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5),
		d.ragNowExpr(), d.ph(6), d.ph(7)), activation.VersionResult.ParseArtifactKey,
		activation.VersionResult.PageCount, activation.VersionResult.AssetCount,
		activation.VersionResult.Degraded, activation.VersionResult.WarningCount,
		fence.DocID, fence.DocVersion)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}

	previousActive := locked.doc.ActiveVersion
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		active_version=%s,status='DONE',error_msg='',chunk_count=%s,token_count=%s,
		index_format_version=1,processing_stage='done',progress_current=progress_total,degraded=%s,
		warning_count=%s,indexed_at=%s WHERE id=%s AND version=%s`, d.ph(1),
		d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ragNowExpr(), d.ph(6), d.ph(7)),
		fence.DocVersion, activation.ChunkCount, activation.TokenCount,
		activation.VersionResult.Degraded, activation.VersionResult.WarningCount,
		fence.DocID, fence.DocVersion)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}

	if previousActive > 0 && previousActive != fence.DocVersion {
		result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
			status='RETIRED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
			AND status='DONE'`, d.ragNowExpr(), d.ph(1), d.ph(2)),
			fence.DocID, previousActive)
		if err != nil {
			return false, err
		}
		if updated, err := ragRowsAffected(result); err != nil || !updated {
			return false, err
		}
		notBefore := locked.now.Add(gcGracePeriod)
		query := fmt.Sprintf(`INSERT INTO rag_index_gc_tasks (
			doc_id,retired_version,retired_at,not_before,status,claim_generation,
			lease_owner,lease_until,heartbeat_at,attempt_count,next_run_at,created_at)
			VALUES (%s,%s,%s,%s,'PENDING',0,'',NULL,NULL,0,%s,%s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6))
		if d.dialect == mysqlDialect {
			query += ` ON DUPLICATE KEY UPDATE id=id`
		} else {
			query += ` ON CONFLICT (doc_id,retired_version) DO NOTHING`
		}
		if _, err := tx.ExecContext(ctx, query, fence.DocID, previousActive,
			locked.now, notBefore, notBefore, locked.now); err != nil {
			return false, err
		}
	}

	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status='DONE',error_msg='',finished_at=%s,lease_owner='',lease_until=NULL,
		heartbeat_at=NULL,next_run_at=NULL WHERE id=%s AND doc_id=%s AND doc_version=%s
		AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
		AND lease_until > %s`, d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5), d.ragNowExpr()), fence.TaskID, fence.DocID,
		fence.DocVersion, fence.ClaimGeneration, fence.LeaseOwner)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) AdvanceDocumentVersionAndCreateTask(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
) (*RAGIndexTaskRecord, error) {
	if snapshot == nil || snapshot.DocID == "" {
		return nil, ErrRAGDocumentVersionMismatch
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	doc, err := d.ragDocumentInTx(ctx, tx, snapshot.DocID)
	if ragIsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if doc.Version != expectedVersion {
		return nil, ErrRAGDocumentVersionConflict
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, doc, snapshot.SourceSHA256); err != nil {
		return nil, err
	}
	newVersion, err := d.nextRAGDocumentVersionInTx(ctx, tx, doc)
	if err != nil {
		return nil, err
	}
	copySnapshot := *snapshot
	copySnapshot.DocID = doc.ID
	copySnapshot.DocVersion = newVersion
	copySnapshot.CreatedAt = time.Time{}
	copySnapshot.UpdatedAt = time.Time{}
	prepareNewRAGDocumentVersion(&copySnapshot)
	if err := d.validateRAGVersionSnapshotForDocument(ctx, tx, doc, &copySnapshot); err != nil {
		return nil, err
	}
	if err := d.supersedeRunnableRAGTasksInTx(ctx, tx, doc.ID); err != nil {
		return nil, err
	}
	if err := d.createRAGDocumentVersion(ctx, tx, &copySnapshot); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		version=%s,status='PENDING',error_msg='',processing_stage='queued',
		progress_current=0,progress_total=0,progress_unit='',degraded=FALSE,
		warning_count=0,indexed_at=indexed_at WHERE id=%s AND version=%s`,
		d.ph(1), d.ph(2), d.ph(3)), newVersion, doc.ID, expectedVersion)
	if err != nil {
		return nil, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		if err == nil {
			err = ErrRAGDocumentVersionConflict
		}
		return nil, err
	}
	taskID, err := d.createRAGIndexTaskForVersion(ctx, tx, doc.ID, newVersion, 3)
	if err != nil {
		return nil, err
	}
	task, err := d.ragTaskInTx(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

func (d *DBStore) supersedeRunnableRAGTasksInTx(ctx context.Context, tx *sql.Tx, docID string) error {
	query := fmt.Sprintf(`SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks
		WHERE doc_id=%s AND status IN ('PENDING','RUNNING') ORDER BY id%s`,
		d.ph(1), d.ragLockSuffix())
	rows, err := tx.QueryContext(ctx, query, docID)
	if err != nil {
		return err
	}
	var tasks []RAGIndexTaskRecord
	for rows.Next() {
		task, err := scanRAGIndexTask(rows)
		if err != nil {
			rows.Close()
			return err
		}
		tasks = append(tasks, *task)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, task := range tasks {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
			status='SUPERSEDED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
			AND status IN ('PENDING','RUNNING')`, d.ragNowExpr(), d.ph(1), d.ph(2)),
			task.DocID, task.DocVersion); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
			status='SUPERSEDED',error_msg='superseded by newer document version',
			finished_at=%s,lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
			WHERE id=%s AND doc_version=%s AND claim_generation=%s
			AND status IN ('PENDING','RUNNING')`, d.ragNowExpr(), d.ph(1), d.ph(2),
			d.ph(3)), task.ID, task.DocVersion, task.ClaimGeneration); err != nil {
			return err
		}
	}
	return nil
}

func (d *DBStore) SupersedeRAGIndexTaskAndCreateVersion(
	ctx context.Context,
	fence IndexFence,
	snapshot *RAGDocumentVersionRecord,
) (*RAGIndexTaskRecord, bool, error) {
	if snapshot == nil || snapshot.DocID != fence.DocID {
		return nil, false, ErrRAGDocumentVersionMismatch
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence)
	if err != nil || !ok {
		return nil, false, err
	}
	if locked.doc.Version != fence.DocVersion {
		return nil, false, nil
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, locked.doc, snapshot.SourceSHA256); err != nil {
		return nil, false, err
	}
	newVersion, err := d.nextRAGDocumentVersionInTx(ctx, tx, locked.doc)
	if err != nil {
		return nil, false, err
	}
	copySnapshot := *snapshot
	copySnapshot.DocVersion = newVersion
	copySnapshot.CreatedAt = time.Time{}
	copySnapshot.UpdatedAt = time.Time{}
	prepareNewRAGDocumentVersion(&copySnapshot)
	if err := d.validateRAGVersionSnapshotForDocument(ctx, tx, locked.doc, &copySnapshot); err != nil {
		return nil, false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='SUPERSEDED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status='RUNNING'`, d.ragNowExpr(), d.ph(1), d.ph(2)),
		fence.DocID, fence.DocVersion)
	if err != nil {
		return nil, false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return nil, false, err
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status='SUPERSEDED',error_msg='provider snapshot changed',finished_at=%s,
		lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND claim_generation=%s
		AND lease_owner=%s AND status='RUNNING' AND lease_until > %s`,
		d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5),
		d.ragNowExpr()), fence.TaskID, fence.DocID, fence.DocVersion,
		fence.ClaimGeneration, fence.LeaseOwner)
	if err != nil {
		return nil, false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return nil, false, err
	}
	if err := d.createRAGDocumentVersion(ctx, tx, &copySnapshot); err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		version=%s,status='PENDING',error_msg='',processing_stage='queued',
		progress_current=0,progress_total=0,progress_unit='',degraded=FALSE,warning_count=0
		WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3)),
		newVersion, fence.DocID, fence.DocVersion); err != nil {
		return nil, false, err
	}
	taskID, err := d.createRAGIndexTaskForVersion(ctx, tx, fence.DocID, newVersion, locked.task.MaxRetry)
	if err != nil {
		return nil, false, err
	}
	task, err := d.ragTaskInTx(ctx, tx, taskID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return task, true, nil
}

// MigrateLegacyRAGIndexTasks performs the runtime backfill after old workers
// have been stopped and a complete immutable SnapshotBuilder is available.
// Per-document builder failures are converted to auditable FAILED target/task
// rows; they do not leave legacy work runnable or abort migration of other docs.
func (d *DBStore) MigrateLegacyRAGIndexTasks(
	ctx context.Context,
	builder RAGLegacyTaskSnapshotBuilder,
	allowBackfill bool,
) error {
	contracted, err := d.ragIndexTaskDocVersionContracted(ctx)
	if err != nil {
		return err
	}
	if contracted {
		if err := d.validateRAGIndexTaskContract(ctx); err != nil {
			return err
		}
		return d.ensureRAGIndexTaskIndexes(ctx)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id,doc_id,status,retry_count,max_retry,
		error_msg,created_at,started_at,finished_at FROM rag_index_tasks
		WHERE doc_version IS NULL ORDER BY doc_id,created_at,id`)
	if err != nil {
		return err
	}
	legacyByDoc := make(map[string][]RAGIndexTaskRecord)
	var docOrder []string
	for rows.Next() {
		var task RAGIndexTaskRecord
		var startedAt, finishedAt sql.NullTime
		if err := rows.Scan(&task.ID, &task.DocID, &task.Status, &task.RetryCount,
			&task.MaxRetry, &task.ErrorMsg, &task.CreatedAt, &startedAt, &finishedAt); err != nil {
			rows.Close()
			return err
		}
		if startedAt.Valid {
			task.StartedAt = &startedAt.Time
		}
		if finishedAt.Valid {
			task.FinishedAt = &finishedAt.Time
		}
		if _, exists := legacyByDoc[task.DocID]; !exists {
			docOrder = append(docOrder, task.DocID)
		}
		legacyByDoc[task.DocID] = append(legacyByDoc[task.DocID], task)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(legacyByDoc) > 0 && !allowBackfill {
		// Backfill/contract is an offline operation. Refuse it before deleting,
		// failing, or otherwise changing even a terminal legacy row unless the
		// deployment explicitly acknowledges the maintenance window.
		return ErrRAGLegacyTaskMigrationRequired
	}
	if builder == nil {
		for _, tasks := range legacyByDoc {
			for _, task := range tasks {
				if task.Status == "PENDING" || task.Status == "RUNNING" {
					// This is a preflight guard: do not archive, fail, or otherwise
					// mutate any legacy row when a runnable survivor cannot receive
					// its complete runtime snapshot.
					return ErrRAGLegacySnapshotBuilder
				}
			}
		}
	}

	for _, docID := range docOrder {
		tasks := legacyByDoc[docID]
		var survivor *RAGIndexTaskRecord
		for i := range tasks {
			if tasks[i].Status == "PENDING" || tasks[i].Status == "RUNNING" {
				survivor = &tasks[i]
			}
		}
		if survivor == nil {
			if err := d.deleteLegacyRAGTasks(ctx, tasks, 0); err != nil {
				return err
			}
			continue
		}
		doc, err := d.GetRAGDocument(ctx, docID)
		if errors.Is(err, ErrNotFound) {
			if err := d.deleteLegacyRAGTasks(ctx, tasks, 0); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		var maximum int64
		if err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(known_version),0) FROM (
			SELECT doc_version AS known_version FROM rag_document_versions WHERE doc_id=%s
			UNION ALL
			SELECT doc_version AS known_version FROM rag_index_tasks
				WHERE doc_id=%s AND doc_version IS NOT NULL
		) known_versions`, d.ph(1), d.ph(2)), docID, docID).Scan(&maximum); err != nil {
			return err
		}
		if doc.Version > maximum {
			maximum = doc.Version
		}
		nextVersion := maximum + 1
		var snapshot *RAGDocumentVersionRecord
		var buildErr error
		if builder == nil {
			buildErr = errors.New("legacy RAG task snapshot builder is unavailable")
		} else {
			snapshot, buildErr = builder(ctx, doc, nextVersion)
			if buildErr == nil && (snapshot == nil || snapshot.DocID != docID || snapshot.DocVersion != nextVersion) {
				buildErr = ErrRAGDocumentVersionMismatch
			}
		}
		if err := d.migrateOneLegacyRAGTask(ctx, doc, tasks, survivor.ID,
			nextVersion, snapshot, buildErr); err != nil {
			return err
		}
	}

	if err := d.validateRAGIndexTaskContract(ctx); err != nil {
		return err
	}
	if err := d.contractRAGIndexTaskDocVersion(ctx); err != nil {
		return err
	}
	return d.ensureRAGIndexTaskIndexes(ctx)
}

func (d *DBStore) validateRAGIndexTaskContract(ctx context.Context) error {
	var nullCount, duplicateCount, multipleRunnableCount int
	if err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rag_index_tasks WHERE doc_version IS NULL`).Scan(&nullCount); err != nil {
		return err
	}
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT doc_id,doc_version FROM rag_index_tasks GROUP BY doc_id,doc_version HAVING COUNT(*)>1
	) duplicate_versions`).Scan(&duplicateCount); err != nil {
		return err
	}
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT doc_id FROM rag_index_tasks WHERE status IN ('PENDING','RUNNING')
		GROUP BY doc_id HAVING COUNT(*)>1
	) multiple_runnable`).Scan(&multipleRunnableCount); err != nil {
		return err
	}
	if nullCount != 0 || duplicateCount != 0 || multipleRunnableCount != 0 {
		return fmt.Errorf("store: RAG task migration validation failed: null=%d duplicate=%d multiple_runnable=%d",
			nullCount, duplicateCount, multipleRunnableCount)
	}

	rows, err := d.db.QueryContext(ctx, `SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks
		WHERE status IN ('PENDING','RUNNING') ORDER BY doc_id,id`)
	if err != nil {
		return err
	}
	var runnable []RAGIndexTaskRecord
	for rows.Next() {
		task, err := scanRAGIndexTask(rows)
		if err != nil {
			rows.Close()
			return err
		}
		runnable = append(runnable, *task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, task := range runnable {
		doc, err := d.GetRAGDocument(ctx, task.DocID)
		if err != nil {
			return fmt.Errorf("store: runnable RAG task %d document: %w", task.ID, err)
		}
		if doc.Version != task.DocVersion {
			return fmt.Errorf("store: runnable RAG task %d does not match document target version", task.ID)
		}
		version, err := d.GetRAGDocumentVersion(ctx, task.DocID, task.DocVersion)
		if err != nil {
			return fmt.Errorf("store: runnable RAG task %d snapshot: %w", task.ID, err)
		}
		if err := d.validateRAGVersionSnapshotForDocument(ctx, d.db, doc, version); err != nil {
			return fmt.Errorf("store: runnable RAG task %d snapshot: %w", task.ID, err)
		}
		switch task.Status {
		case "PENDING":
			if version.Status != RAGDocumentVersionPending && version.Status != RAGDocumentVersionFailed {
				return fmt.Errorf("store: PENDING RAG task %d has version status %s", task.ID, version.Status)
			}
			if task.LeaseOwner != "" || task.LeaseUntil != nil {
				return fmt.Errorf("store: PENDING RAG task %d retains a lease", task.ID)
			}
		case "RUNNING":
			if version.Status != RAGDocumentVersionRunning {
				return fmt.Errorf("store: RUNNING RAG task %d has version status %s", task.ID, version.Status)
			}
			if task.ClaimGeneration <= 0 || !validRAGWorkerID(task.LeaseOwner) || task.LeaseUntil == nil {
				return fmt.Errorf("store: RUNNING RAG task %d has an incomplete fence", task.ID)
			}
		}
	}
	return nil
}

func (d *DBStore) deleteLegacyRAGTasks(
	ctx context.Context,
	tasks []RAGIndexTaskRecord,
	keepID int64,
) error {
	for _, task := range tasks {
		if task.ID == keepID {
			continue
		}
		if _, err := d.db.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM rag_index_tasks WHERE id=%s AND doc_version IS NULL`, d.ph(1)),
			task.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *DBStore) migrateOneLegacyRAGTask(
	ctx context.Context,
	doc *RAGDocumentRecord,
	tasks []RAGIndexTaskRecord,
	survivorID int64,
	nextVersion int64,
	snapshot *RAGDocumentVersionRecord,
	buildErr error,
) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lockedDoc, err := d.ragDocumentInTx(ctx, tx, doc.ID)
	if err != nil {
		return scanErr(err)
	}
	var copySnapshot RAGDocumentVersionRecord
	if buildErr == nil {
		if snapshot == nil || snapshot.DocID != doc.ID || snapshot.DocVersion != nextVersion {
			buildErr = ErrRAGDocumentVersionMismatch
		} else {
			copySnapshot = *snapshot
			prepareNewRAGDocumentVersion(&copySnapshot)
			copySnapshot.CreatedAt = time.Time{}
			copySnapshot.UpdatedAt = time.Time{}
			if err := d.reconcileRAGDocumentSourceHash(ctx, tx, lockedDoc, copySnapshot.SourceSHA256); err != nil {
				buildErr = err
			} else if err := d.validateRAGVersionSnapshotForDocument(ctx, tx, lockedDoc, &copySnapshot); err != nil {
				buildErr = err
			}
		}
	}
	for _, task := range tasks {
		if task.ID == survivorID {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM rag_index_tasks WHERE id=%s AND doc_version IS NULL`, d.ph(1)),
			task.ID); err != nil {
			return err
		}
	}
	terminalStatus := RAGDocumentVersionSuperseded
	if buildErr != nil {
		terminalStatus = RAGDocumentVersionFailed
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status=%s,updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status IN ('PENDING','RUNNING')`, d.ph(1), d.ragNowExpr(), d.ph(2),
		d.ph(3)), terminalStatus, doc.ID, lockedDoc.Version); err != nil {
		return err
	}

	if buildErr != nil {
		reason := "legacy index task snapshot failed: " + buildErr.Error()
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
			doc_version=%s,status='FAILED',claim_generation=0,lease_owner='',
			lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,error_msg=%s,
			finished_at=%s WHERE id=%s AND doc_version IS NULL`, d.ph(1), d.ph(2),
			d.ragNowExpr(), d.ph(3)), nextVersion, reason, survivorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			version=%s,status='FAILED',error_msg=%s,processing_stage='needsReindex'
			WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
			nextVersion, reason, doc.ID, lockedDoc.Version); err != nil {
			return err
		}
		return tx.Commit()
	}

	if err := d.createRAGDocumentVersion(ctx, tx, &copySnapshot); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		doc_version=%s,status='PENDING',claim_generation=0,lease_owner='',
		lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL,error_msg='',
		started_at=NULL,finished_at=NULL WHERE id=%s AND doc_version IS NULL`,
		d.ph(1), d.ph(2)), nextVersion, survivorID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		version=%s,status='PENDING',error_msg='',processing_stage='queued',
		progress_current=0,progress_total=0,progress_unit='',degraded=FALSE,warning_count=0
		WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3)),
		nextVersion, doc.ID, lockedDoc.Version); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ragIndexTaskDocVersionContracted(ctx context.Context) (bool, error) {
	switch d.dialect {
	case "postgres":
		var nullable string
		err := d.db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
			WHERE table_schema=current_schema() AND table_name='rag_index_tasks'
			AND column_name='doc_version'`).Scan(&nullable)
		return nullable == "NO", err
	case mysqlDialect:
		var nullable string
		err := d.db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
			WHERE table_schema=DATABASE() AND table_name='rag_index_tasks'
			AND column_name='doc_version'`).Scan(&nullable)
		return nullable == "NO", err
	default:
		rows, err := d.db.QueryContext(ctx, `PRAGMA table_info(rag_index_tasks)`)
		if err != nil {
			return false, err
		}
		defer rows.Close()
		for rows.Next() {
			var cid, notNull, primaryKey int
			var name, columnType string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
				return false, err
			}
			if name == "doc_version" {
				return notNull != 0, nil
			}
		}
		return false, rows.Err()
	}
}

func (d *DBStore) contractRAGIndexTaskDocVersion(ctx context.Context) error {
	switch d.dialect {
	case "postgres":
		_, err := d.db.ExecContext(ctx,
			`ALTER TABLE rag_index_tasks ALTER COLUMN doc_version SET NOT NULL`)
		return err
	case mysqlDialect:
		_, err := d.db.ExecContext(ctx,
			`ALTER TABLE rag_index_tasks MODIFY COLUMN doc_version BIGINT NOT NULL`)
		return err
	default:
		return d.rebuildRAGIndexTasksSQLite(ctx)
	}
}
