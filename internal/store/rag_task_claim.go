package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path"
	"strings"
	"time"
)

// IndexFence is the immutable capability held by one claimed index attempt.
// Every worker mutation must match all fields and an unexpired database lease.
type IndexFence struct {
	TaskID                    int64
	DocID                     string
	DocVersion                int64
	ClaimGeneration           int64
	LeaseOwner                string
	ExpectedWriterFingerprint string
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

type RAGFairQueueClaimDisposition string

const (
	RAGFairQueueClaimed                RAGFairQueueClaimDisposition = "claimed"
	RAGFairQueueClaimCapacityDeferred  RAGFairQueueClaimDisposition = "capacity-deferred"
	RAGFairQueueClaimDuplicateStale    RAGFairQueueClaimDisposition = "duplicate-stale-terminal"
	RAGFairQueueClaimCanonicalTerminal RAGFairQueueClaimDisposition = "canonical-repaired-terminal"
	RAGFairQueueClaimCanonicalRetry    RAGFairQueueClaimDisposition = "canonical-repaired-retry"
	RAGFairQueueClaimPoison            RAGFairQueueClaimDisposition = "poison"
	RAGFairQueueClaimPoisonRepaired    RAGFairQueueClaimDisposition = "poison-repaired"
)

type RAGFairQueueClaimLimits struct {
	GlobalConcurrency       int
	PerUserBurstConcurrency int
	AdvisoryLockTimeout     time.Duration
	MaintenanceRetryDelay   time.Duration
}

func (l RAGFairQueueClaimLimits) normalized() (RAGFairQueueClaimLimits, error) {
	if l.GlobalConcurrency <= 0 || l.PerUserBurstConcurrency <= 0 ||
		l.PerUserBurstConcurrency > l.GlobalConcurrency {
		return RAGFairQueueClaimLimits{}, errors.New("store: invalid RAG fair queue capacity limits")
	}
	if l.AdvisoryLockTimeout == 0 {
		l.AdvisoryLockTimeout = 5 * time.Second
	}
	if l.MaintenanceRetryDelay == 0 {
		l.MaintenanceRetryDelay = time.Second
	}
	if l.AdvisoryLockTimeout <= 0 || l.MaintenanceRetryDelay <= 0 {
		return RAGFairQueueClaimLimits{}, errors.New("store: invalid RAG fair queue claim timing")
	}
	return l, nil
}

type RAGFairQueueClaimResult struct {
	Disposition RAGFairQueueClaimDisposition
	Claim       *RAGIndexClaim
}

type ragIndexClaimMode struct {
	expectedUserID             string
	expectedDispatchGeneration int64
	expectedWriterFingerprint  string
	limits                     *RAGFairQueueClaimLimits
	sessionIdentity            *fairQueueMySQLIdentity
	capacityLockName           string
}

var ErrRAGFairQueueCapacityLockUnavailable = errors.New("store: RAG fair queue capacity lock unavailable")

func (m ragIndexClaimMode) fair() bool { return m.limits != nil }

func (d *DBStore) commitRAGIndexClaimTx(
	ctx context.Context,
	tx *sql.Tx,
	mode ragIndexClaimMode,
) error {
	if mode.sessionIdentity != nil {
		if mode.capacityLockName == "" {
			return ErrFairQueueUnsafeConnection
		}
		if err := verifyFairQueueMySQLSession(
			ctx, tx, *mode.sessionIdentity, mode.capacityLockName,
		); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		if mode.sessionIdentity != nil {
			return errors.Join(ErrFairQueueUnsafeConnection, err)
		}
		return err
	}
	return nil
}

func fairQueueCapacityLockName(database, resource string) string {
	digest := sha256.Sum256([]byte(database + "\x00" + resource))
	return "bkcrab:fq:" + hex.EncodeToString(digest[:])[:48]
}

func (s *RAGFairQueueStore) ClaimRAGIndexTaskByID(
	ctx context.Context,
	taskID int64,
	expectedUserID string,
	expectedDispatchGeneration int64,
	workerID string,
	leaseDuration time.Duration,
	limits RAGFairQueueClaimLimits,
) (result RAGFairQueueClaimResult, err error) {
	result.Disposition = RAGFairQueueClaimDuplicateStale
	if s == nil || s.store == nil {
		return result, ErrFairQueueMySQLRequired
	}
	if taskID <= 0 || strings.TrimSpace(expectedUserID) == "" || expectedDispatchGeneration <= 0 {
		return result, errors.New("store: invalid RAG fair queue delivery identity")
	}
	if !validRAGWorkerID(workerID) {
		return result, errors.New("store: RAG worker id must be 1..96 trimmed bytes")
	}
	if leaseDuration <= 0 {
		return result, errors.New("store: RAG lease duration must be positive")
	}
	limits, err = limits.normalized()
	if err != nil {
		return result, err
	}
	err = s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			lockName := fairQueueCapacityLockName(identity.database, "rag.index")
			timeoutSeconds := limits.AdvisoryLockTimeout.Seconds()
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline).Seconds()
				if remaining <= 0 {
					return context.DeadlineExceeded
				}
				if remaining < timeoutSeconds {
					timeoutSeconds = remaining
				}
			}
			var acquired sql.NullInt64
			if lockErr := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, lockName, timeoutSeconds).
				Scan(&acquired); lockErr != nil {
				return errors.Join(ErrRAGFairQueueCapacityLockUnavailable,
					ErrFairQueueUnsafeConnection, lockErr)
			}
			if !acquired.Valid {
				return errors.Join(ErrRAGFairQueueCapacityLockUnavailable, ErrFairQueueUnsafeConnection)
			}
			if acquired.Int64 != 1 {
				return ErrRAGFairQueueCapacityLockUnavailable
			}

			lockHeld := true
			defer func() {
				if !lockHeld {
					return
				}
				cleanupCtx, cancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
				defer cancel()
				var released sql.NullInt64
				releaseErr := conn.QueryRowContext(cleanupCtx, `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
				lockHeld = false
				if releaseErr != nil || !released.Valid || released.Int64 != 1 {
					callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, releaseErr)
				}
			}()
			if verifyErr := verifyFairQueueMySQLSession(ctx, conn, identity, lockName); verifyErr != nil {
				lockHeld = false
				return errors.Join(verifyErr, discardFairQueueSQLConn(conn))
			}

			var docID string
			lookupErr := conn.QueryRowContext(ctx,
				`SELECT doc_id FROM rag_index_tasks WHERE id=?`, taskID).Scan(&docID)
			if errors.Is(lookupErr, sql.ErrNoRows) {
				return nil
			}
			if lookupErr != nil {
				return lookupErr
			}
			var route ragOwnershipRoute
			routeErr := conn.QueryRowContext(ctx, `SELECT d.kb_id,kb.user_id
				FROM rag_documents d JOIN rag_kbs kb ON kb.id=d.kb_id WHERE d.id=?`, docID).
				Scan(&route.KBID, &route.UserID)
			if errors.Is(routeErr, sql.ErrNoRows) {
				routeErr = ErrNotFound
			}
			tx, beginErr := conn.BeginTx(ctx, nil)
			if beginErr != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, beginErr)
			}
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil &&
					!errors.Is(rollbackErr, sql.ErrTxDone) {
					callbackErr = errors.Join(
						callbackErr, ErrFairQueueUnsafeConnection, rollbackErr,
					)
				}
			}()
			claimResult, _, claimErr := s.store.claimRAGIndexTaskIDInTx(
				ctx, tx, taskID, docID, route, routeErr, workerID, leaseDuration,
				ragIndexClaimMode{
					expectedUserID: expectedUserID, expectedDispatchGeneration: expectedDispatchGeneration,
					expectedWriterFingerprint: s.expectedWriter, limits: &limits,
					sessionIdentity: &identity, capacityLockName: lockName,
				},
			)
			if claimErr == nil {
				result = claimResult
			}
			return claimErr
		})
	if err != nil {
		return RAGFairQueueClaimResult{
			Disposition: RAGFairQueueClaimDuplicateStale,
		}, err
	}
	return result, nil
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
		return "STRFTIME('%Y-%m-%d %H:%M:%f','NOW')"
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
		leaseDue := task.LeaseUntil == nil || !task.LeaseUntil.After(now)
		nextRunDue := task.NextRunAt == nil || !task.NextRunAt.After(now)
		return leaseDue && nextRunDue
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
	nowExpr := d.ragNowExpr()
	var taskID int64
	var docID string
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,doc_id FROM rag_index_tasks t
		WHERE doc_version IS NOT NULL AND doc_version > 0 AND (
			(status='PENDING' AND (next_run_at IS NULL OR next_run_at <= %s)) OR
			(status='RUNNING' AND (lease_until IS NULL OR lease_until <= %s)
				AND (next_run_at IS NULL OR next_run_at <= %s)))
		AND NOT EXISTS (SELECT 1 FROM rag_document_maintenance_leases m
			WHERE m.doc_id=t.doc_id AND m.lease_until IS NOT NULL AND m.lease_until>%s)
		ORDER BY created_at,id LIMIT 1`, nowExpr, nowExpr, nowExpr, nowExpr)).Scan(&taskID, &docID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	route, routeErr := d.ragOwnershipRoute(ctx, docID)
	if routeErr != nil && !errors.Is(routeErr, ErrNotFound) {
		return nil, false, routeErr
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	result, consumed, err := d.claimRAGIndexTaskIDInTx(
		ctx, tx, taskID, docID, route, routeErr, workerID, leaseDuration, ragIndexClaimMode{},
	)
	return result.Claim, consumed, err
}

func (d *DBStore) claimRAGIndexTaskIDInTx(
	ctx context.Context,
	tx *sql.Tx,
	taskID int64,
	docID string,
	route ragOwnershipRoute,
	routeErr error,
	workerID string,
	leaseDuration time.Duration,
	mode ragIndexClaimMode,
) (RAGFairQueueClaimResult, bool, error) {
	duplicate := RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimDuplicateStale}
	if tx == nil {
		return duplicate, false, errors.New("store: nil RAG claim transaction")
	}
	if mode.fair() {
		if strings.TrimSpace(mode.expectedUserID) == "" || mode.expectedDispatchGeneration <= 0 ||
			!lowerHex64Pattern.MatchString(mode.expectedWriterFingerprint) {
			return duplicate, false, errors.New("store: invalid RAG fair queue claim identity")
		}
		normalized, err := mode.limits.normalized()
		if err != nil {
			return duplicate, false, err
		}
		mode.limits = &normalized
	}
	nowExpr := d.ragNowExpr()
	if routeErr != nil && !errors.Is(routeErr, ErrNotFound) {
		return duplicate, false, routeErr
	}
	if errors.Is(routeErr, ErrNotFound) {
		task, taskErr := d.ragTaskInTx(ctx, tx, taskID)
		if ragIsNoRows(taskErr) {
			return duplicate, true, nil
		}
		if taskErr != nil {
			return duplicate, false, taskErr
		}
		if task.DocID != docID {
			return duplicate, true, nil
		}
		now, nowErr := d.ragDBNow(ctx, tx)
		if nowErr != nil {
			return duplicate, false, nowErr
		}
		if mode.fair() && (task.DispatchGeneration != mode.expectedDispatchGeneration ||
			task.DispatchGeneration <= task.ClaimGeneration || !ragTaskDue(task, now)) {
			return duplicate, true, nil
		}
		result, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
			status='FAILED',error_msg='orphan index task',finished_at=%s,
			lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
			WHERE id=%s AND doc_id=%s AND doc_version=%s AND dispatch_generation=%s
			AND claim_generation=%s AND status IN ('PENDING','RUNNING')`, nowExpr,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), taskID, task.DocID,
			task.DocVersion, task.DispatchGeneration, task.ClaimGeneration)
		if updateErr != nil {
			return duplicate, false, updateErr
		}
		changed, rowsErr := ragRowsAffected(result)
		if rowsErr != nil {
			return duplicate, false, rowsErr
		}
		if !changed {
			return duplicate, true, nil
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
	}

	_, ownershipActive, err := d.lockRAGKBOwnerTx(ctx, tx, route.KBID, route.UserID)
	if err != nil {
		return duplicate, false, err
	}
	doc, err := d.ragDocumentInTx(ctx, tx, docID)
	if errors.Is(err, sql.ErrNoRows) {
		return duplicate, true, nil
	}
	if err != nil {
		return duplicate, false, err
	}
	if doc.KBID != route.KBID {
		return duplicate, true, nil
	}
	maintenanceActive := false
	if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, doc.ID); err != nil {
		if errors.Is(err, ErrRAGDocumentMaintenanceActive) {
			maintenanceActive = true
		} else {
			return duplicate, false, err
		}
	}
	task, err := d.ragTaskInTx(ctx, tx, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return duplicate, true, nil
	}
	if err != nil {
		return duplicate, false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return duplicate, false, err
	}
	if task.DocID != doc.ID || !ragTaskDue(task, now) {
		return duplicate, true, nil
	}
	if strings.TrimSpace(task.UserID) != "" && task.UserID != route.UserID {
		return duplicate, false, ErrRAGFairQueueCanonicalOwner
	}
	if mode.fair() {
		if task.UserID == "" {
			return duplicate, false, ErrRAGFairQueueCanonicalOwner
		}
		if task.DispatchGeneration != mode.expectedDispatchGeneration ||
			task.DispatchGeneration <= task.ClaimGeneration {
			return duplicate, true, nil
		}
		if mode.expectedUserID != task.UserID {
			return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimPoison}, true, nil
		}
	}
	if maintenanceActive {
		if !mode.fair() {
			return duplicate, true, nil
		}
		changed, repairErr := d.deferRAGFairQueueMaintenanceInTx(
			ctx, tx, task, now, mode.limits.MaintenanceRetryDelay,
		)
		if repairErr != nil {
			return duplicate, false, repairErr
		}
		if !changed {
			return duplicate, true, nil
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalRetry}, true, nil
	}
	if !ownershipActive {
		if err := d.supersedeRunnableRAGTasksInTx(ctx, tx, doc.ID); err != nil {
			return duplicate, false, err
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
	}
	if doc.Version != task.DocVersion {
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
			"stale task does not match document target version"); err != nil {
			return duplicate, false, err
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
	}
	version, err := d.ragVersionInTx(ctx, tx, task.DocID, task.DocVersion)
	if errors.Is(err, sql.ErrNoRows) {
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task, "missing immutable version snapshot"); err != nil {
			return duplicate, false, err
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
	}
	if err != nil {
		return duplicate, false, err
	}
	if validationErr := d.validateRAGVersionSnapshotForDocument(ctx, tx, doc, version); validationErr != nil {
		if !errors.Is(validationErr, ErrRAGDocumentVersionIncomplete) &&
			!errors.Is(validationErr, ErrRAGDocumentVersionMismatch) &&
			!errors.Is(validationErr, ErrRAGDocumentSourceConflict) &&
			!errors.Is(validationErr, ErrNotFound) {
			return duplicate, false, validationErr
		}
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
			"invalid immutable version snapshot: "+validationErr.Error()); err != nil {
			return duplicate, false, err
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, doc, version.SourceSHA256); err != nil {
		if errors.Is(err, ErrRAGDocumentVersionMismatch) || errors.Is(err, ErrRAGDocumentSourceConflict) {
			if failErr := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
				"invalid immutable version source: "+err.Error()); failErr != nil {
				return duplicate, false, failErr
			}
			if commitErr := d.commitRAGIndexClaimTx(ctx, tx, mode); commitErr != nil {
				return duplicate, false, commitErr
			}
			return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
		}
		return duplicate, false, err
	}

	if mode.fair() {
		available, capacityErr := d.ragFairQueueCapacityAvailableInTx(
			ctx, tx, task.UserID, *mode.limits,
		)
		if capacityErr != nil {
			return duplicate, false, capacityErr
		}
		if !available {
			return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCapacityDeferred}, true, nil
		}
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
			if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task,
				"index lease expired after retry budget exhausted"); err != nil {
				return duplicate, false, err
			}
			if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
				return duplicate, false, err
			}
			return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
		}
		retryCount++
		allocateFresh = true
	default:
		if err := d.failInvalidRAGTaskInTx(ctx, tx, doc, task, "invalid task/version state"); err != nil {
			return duplicate, false, err
		}
		if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
			return duplicate, false, err
		}
		return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimCanonicalTerminal}, true, nil
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
			return duplicate, false, err
		}
		if ok, err := ragRowsAffected(result); err != nil || !ok {
			return duplicate, true, err
		}
		newVersion, err := d.nextRAGDocumentVersionInTx(ctx, tx, doc)
		if err != nil {
			return duplicate, false, err
		}
		copyVersion := *version
		copyVersion.DocVersion = newVersion
		copyVersion.CreatedAt = now
		copyVersion.UpdatedAt = now
		prepareNewRAGDocumentVersion(&copyVersion)
		copyVersion.Status = RAGDocumentVersionRunning
		if err := d.createRAGDocumentVersion(ctx, tx, &copyVersion); err != nil {
			return duplicate, false, err
		}
		version = &copyVersion
		task.DocVersion = newVersion
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			version=%s,status='PROCESSING',error_msg='',processing_stage='claimed',
			progress_current=0,progress_total=0,progress_unit='',degraded=FALSE,warning_count=0
			WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3)),
			newVersion, doc.ID, doc.Version); err != nil {
			return duplicate, false, err
		}
		doc.Version = newVersion
	} else {
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
			status='RUNNING',updated_at=%s WHERE doc_id=%s AND doc_version=%s AND status='PENDING'`,
			nowExpr, d.ph(1), d.ph(2)), task.DocID, task.DocVersion)
		if err != nil {
			return duplicate, false, err
		}
		if ok, err := ragRowsAffected(result); err != nil || !ok {
			return duplicate, true, err
		}
		version.Status = RAGDocumentVersionRunning
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
			status='PROCESSING',error_msg='',processing_stage='claimed'
			WHERE id=%s AND version=%s`, d.ph(1), d.ph(2)), doc.ID, task.DocVersion); err != nil {
			return duplicate, false, err
		}
	}

	leaseUntil := now.Add(leaseDuration)
	var newGeneration int64
	if mode.fair() {
		newGeneration = mode.expectedDispatchGeneration
	} else {
		if task.ClaimGeneration == math.MaxInt64 {
			return duplicate, false, ErrRAGDispatchGenerationExhausted
		}
		newGeneration = task.ClaimGeneration + 1
		if task.DispatchGeneration > newGeneration {
			newGeneration = task.DispatchGeneration
		}
	}
	condition := "status='PENDING' AND (next_run_at IS NULL OR next_run_at <= " + nowExpr + ")"
	if oldStatus == "RUNNING" {
		condition = "status='RUNNING' AND (lease_until IS NULL OR lease_until <= " + nowExpr + ")" +
			" AND (next_run_at IS NULL OR next_run_at <= " + nowExpr + ")"
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		doc_version=%s,user_id=%s,status='RUNNING',retry_count=%s,
		dispatch_generation=%s,claim_generation=%s,
		dispatched_at=COALESCE(dispatched_at,%s),lease_owner=%s,lease_until=%s,
		heartbeat_at=%s,next_run_at=NULL,error_msg='',
		started_at=COALESCE(started_at,%s),finished_at=NULL
		WHERE id=%s AND doc_version=%s AND dispatch_generation=%s AND claim_generation=%s
		AND (user_id IS NULL OR TRIM(user_id)='' OR user_id=%s) AND %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), nowExpr, d.ph(6), d.ph(7),
		nowExpr, nowExpr, d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), condition),
		task.DocVersion, route.UserID, retryCount, newGeneration, newGeneration,
		workerID, leaseUntil, task.ID, oldVersion, task.DispatchGeneration,
		task.ClaimGeneration, task.UserID)
	if err != nil {
		return duplicate, false, err
	}
	if ok, err := ragRowsAffected(result); err != nil || !ok {
		return duplicate, true, err
	}
	if err := d.commitRAGIndexClaimTx(ctx, tx, mode); err != nil {
		return duplicate, false, err
	}
	task.Status = "RUNNING"
	task.UserID = route.UserID
	task.RetryCount = retryCount
	task.DispatchGeneration = newGeneration
	task.ClaimGeneration = newGeneration
	if task.DispatchedAt == nil {
		task.DispatchedAt = &now
	}
	task.LeaseOwner = workerID
	task.LeaseUntil = &leaseUntil
	task.HeartbeatAt = &now
	task.NextRunAt = nil
	task.ErrorMsg = ""
	fence := IndexFence{
		TaskID: task.ID, DocID: task.DocID, DocVersion: task.DocVersion,
		ClaimGeneration: task.ClaimGeneration, LeaseOwner: task.LeaseOwner,
		ExpectedWriterFingerprint: mode.expectedWriterFingerprint,
	}
	claim := &RAGIndexClaim{Task: *task, Version: *version, Fence: fence}
	return RAGFairQueueClaimResult{Disposition: RAGFairQueueClaimed, Claim: claim}, true, nil
}

func (d *DBStore) ragFairQueueCapacityAvailableInTx(
	ctx context.Context,
	tx *sql.Tx,
	userID string,
	limits RAGFairQueueClaimLimits,
) (bool, error) {
	base := ` FROM rag_index_tasks t
		JOIN rag_documents d ON d.id=t.doc_id
		JOIN rag_kbs kb ON kb.id=d.kb_id AND kb.user_id=t.user_id
		JOIN users u ON u.id=kb.user_id
		JOIN rag_document_versions v ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
		WHERE t.status='RUNNING' AND t.dispatch_generation=t.claim_generation
		AND t.claim_generation>0 AND t.lease_owner<>'' AND t.lease_until>` + d.ragNowExpr() + `
		AND t.heartbeat_at IS NOT NULL AND t.next_run_at IS NULL
		AND v.status='RUNNING' AND d.version=t.doc_version
		AND UPPER(d.status)='PROCESSING' AND LOWER(kb.status)='active' AND LOWER(u.status)='active'`
	var global int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+base).Scan(&global); err != nil {
		return false, err
	}
	if global >= limits.GlobalConcurrency {
		return false, nil
	}
	var tenant int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*)`+base+` AND t.user_id=`+d.ph(1), userID).Scan(&tenant); err != nil {
		return false, err
	}
	return tenant < limits.PerUserBurstConcurrency, nil
}

func (d *DBStore) repairRAGFairQueuePoisonInTx(
	ctx context.Context,
	tx *sql.Tx,
	task *RAGIndexTaskRecord,
	now time.Time,
	maintenanceActive bool,
	maintenanceDelay time.Duration,
) (bool, error) {
	base := task.DispatchGeneration
	if task.ClaimGeneration > base {
		base = task.ClaimGeneration
	}
	if base == math.MaxInt64 {
		return false, ErrRAGDispatchGenerationExhausted
	}
	var nextRunAt any = task.NextRunAt
	if maintenanceActive {
		nextRunAt = now.Add(maintenanceDelay)
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		dispatch_generation=%s,dispatched_at=NULL,next_run_at=%s
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND user_id=%s AND status=%s
		AND dispatch_generation=%s AND claim_generation=%s AND retry_count=%s
		AND dispatch_generation>claim_generation`, d.ph(1), d.ph(2), d.ph(3), d.ph(4),
		d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)), base+1, nextRunAt,
		task.ID, task.DocID, task.DocVersion, task.UserID, task.Status,
		task.DispatchGeneration, task.ClaimGeneration, task.RetryCount)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) deferRAGFairQueueMaintenanceInTx(
	ctx context.Context,
	tx *sql.Tx,
	task *RAGIndexTaskRecord,
	now time.Time,
	delay time.Duration,
) (bool, error) {
	base := task.DispatchGeneration
	if task.ClaimGeneration > base {
		base = task.ClaimGeneration
	}
	if base == math.MaxInt64 {
		return false, ErrRAGDispatchGenerationExhausted
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		dispatch_generation=%s,dispatched_at=NULL,next_run_at=%s
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND user_id=%s AND status=%s
		AND dispatch_generation=%s AND claim_generation=%s AND retry_count=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7),
		d.ph(8), d.ph(9), d.ph(10)), base+1, now.Add(delay), task.ID, task.DocID,
		task.DocVersion, task.UserID, task.Status, task.DispatchGeneration,
		task.ClaimGeneration, task.RetryCount)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
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
		AND dispatch_generation=%s AND claim_generation=%s
		AND status IN ('PENDING','RUNNING')`, d.ph(1), nowExpr, d.ph(2), d.ph(3),
		d.ph(4), d.ph(5)), reason, task.ID, task.DocVersion,
		task.DispatchGeneration, task.ClaimGeneration); err != nil {
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
	if fence.ExpectedWriterFingerprint != "" {
		if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
			return false, ErrFairQueueWriterMismatch
		}
		var valid bool
		err := d.withFairQueueExpectedWriterConn(ctx, fence.ExpectedWriterFingerprint,
			func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
				var err error
				valid, err = d.checkRAGIndexFenceOn(ctx, conn, fence, true)
				return err
			})
		if err != nil {
			return false, err
		}
		return valid, nil
	}
	return d.checkRAGIndexFenceOn(ctx, d.db, fence, false)
}

func (d *DBStore) checkRAGIndexFenceOn(
	ctx context.Context,
	exec ragExecutor,
	fence IndexFence,
	strictFair bool,
) (bool, error) {
	versionJoin := ""
	strictConditions := ""
	if strictFair {
		versionJoin = ` JOIN rag_document_versions v
			ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version`
		strictConditions = ` AND t.user_id=kb.user_id AND d.version=t.doc_version
			AND UPPER(d.status)='PROCESSING' AND v.status='RUNNING'`
	}
	var present int
	err := exec.QueryRowContext(ctx, fmt.Sprintf(`SELECT 1 FROM rag_index_tasks t
		JOIN rag_documents d ON d.id=t.doc_id JOIN rag_kbs kb ON kb.id=d.kb_id
		JOIN users u ON u.id=kb.user_id%s
		WHERE t.id=%s AND t.doc_id=%s AND t.doc_version=%s AND t.claim_generation=%s
		AND t.lease_owner=%s AND t.status='RUNNING' AND t.lease_until > %s
		AND t.dispatch_generation=t.claim_generation
		AND UPPER(d.status)<>'DELETING' AND LOWER(kb.status)='active' AND LOWER(u.status)='active'%s`,
		versionJoin, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ragNowExpr(),
		strictConditions),
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
	if fence.ExpectedWriterFingerprint != "" {
		return d.heartbeatRAGIndexTaskFair(ctx, fence, leaseDuration)
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		lease_until=%s,heartbeat_at=%s WHERE id=%s AND doc_id=%s AND doc_version=%s
		AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
		AND dispatch_generation=claim_generation AND lease_until > %s
		AND EXISTS (SELECT 1 FROM rag_documents d
			JOIN rag_kbs kb ON kb.id=d.kb_id JOIN users u ON u.id=kb.user_id
			WHERE d.id=%s AND UPPER(d.status)<>'DELETING'
			AND LOWER(kb.status)='active' AND LOWER(u.status)='active')`,
		d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ragNowExpr(), d.ph(7)), now.Add(leaseDuration),
		fence.TaskID, fence.DocID, fence.DocVersion, fence.ClaimGeneration,
		fence.LeaseOwner, fence.DocID)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) heartbeatRAGIndexTaskFair(
	ctx context.Context,
	fence IndexFence,
	leaseDuration time.Duration,
) (renewed bool, err error) {
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return false, ErrFairQueueWriterMismatch
	}
	err = d.withFairQueueExpectedWriterConn(ctx, fence.ExpectedWriterFingerprint,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			lockName := fairQueueCapacityLockName(identity.database, "rag.index")
			timeoutSeconds := fairQueueOperationStartLockTimeout.Seconds()
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline).Seconds()
				if remaining <= 0 {
					return context.DeadlineExceeded
				}
				if remaining < timeoutSeconds {
					timeoutSeconds = remaining
				}
			}
			var acquired sql.NullInt64
			if lockErr := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, lockName, timeoutSeconds).
				Scan(&acquired); lockErr != nil {
				return errors.Join(ErrRAGFairQueueCapacityLockUnavailable,
					ErrFairQueueUnsafeConnection, lockErr)
			}
			if !acquired.Valid {
				return errors.Join(ErrRAGFairQueueCapacityLockUnavailable, ErrFairQueueUnsafeConnection)
			}
			if acquired.Int64 != 1 {
				return ErrRAGFairQueueCapacityLockUnavailable
			}
			lockHeld := true
			defer func() {
				if !lockHeld {
					return
				}
				cleanupCtx, cancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
				defer cancel()
				var released sql.NullInt64
				releaseErr := conn.QueryRowContext(cleanupCtx, `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
				lockHeld = false
				if releaseErr != nil || !released.Valid || released.Int64 != 1 {
					callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, releaseErr)
				}
			}()
			if verifyErr := verifyFairQueueMySQLSession(ctx, conn, identity, lockName); verifyErr != nil {
				lockHeld = false
				return errors.Join(verifyErr, discardFairQueueSQLConn(conn))
			}
			tx, beginErr := conn.BeginTx(ctx, nil)
			if beginErr != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, beginErr)
			}
			defer func() {
				if rollbackErr := tx.Rollback(); rollbackErr != nil &&
					!errors.Is(rollbackErr, sql.ErrTxDone) {
					callbackErr = errors.Join(
						callbackErr, ErrFairQueueUnsafeConnection, rollbackErr,
					)
				}
			}()
			now, nowErr := d.ragDBNow(ctx, tx)
			if nowErr != nil {
				return nowErr
			}
			result, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
				lease_until=%s,heartbeat_at=%s WHERE id=%s AND doc_id=%s AND doc_version=%s
				AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
				AND dispatch_generation=claim_generation AND lease_until > %s
				AND EXISTS (SELECT 1 FROM rag_documents d
					JOIN rag_kbs kb ON kb.id=d.kb_id JOIN users u ON u.id=kb.user_id
					JOIN rag_document_versions v
						ON v.doc_id=d.id AND v.doc_version=%s
					WHERE d.id=%s AND d.version=%s AND UPPER(d.status)='PROCESSING'
					AND v.status='RUNNING' AND rag_index_tasks.user_id=kb.user_id
					AND LOWER(kb.status)='active' AND LOWER(u.status)='active')`,
				d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
				d.ragNowExpr(), d.ph(7), d.ph(8), d.ph(9)), now.Add(leaseDuration), fence.TaskID,
				fence.DocID, fence.DocVersion, fence.ClaimGeneration, fence.LeaseOwner,
				fence.DocVersion, fence.DocID, fence.DocVersion)
			if updateErr != nil {
				return updateErr
			}
			changed, rowsErr := ragRowsAffected(result)
			if rowsErr != nil {
				return rowsErr
			}
			if !changed {
				return nil
			}
			if verifyErr := verifyFairQueueMySQLSession(ctx, tx, identity, lockName); verifyErr != nil {
				return verifyErr
			}
			if commitErr := tx.Commit(); commitErr != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, commitErr)
			}
			renewed = true
			return nil
		})
	if err != nil {
		return false, err
	}
	return renewed, nil
}

// AcknowledgeRAGIndexTaskQuiesced lets a worker that observed a durable
// supersession release the tombstone's fallback lease wait as soon as all of
// its external writes have stopped. A crashed/uncooperative worker simply
// leaves the original deadline in place for bounded cleanup recovery.
func (d *DBStore) AcknowledgeRAGIndexTaskQuiesced(
	ctx context.Context,
	fence IndexFence,
) (bool, error) {
	if fence.ExpectedWriterFingerprint == "" {
		return d.acknowledgeRAGIndexTaskQuiescedOn(ctx, d.db, fence)
	}
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return false, ErrFairQueueWriterMismatch
	}
	var acknowledged bool
	fairStore := &RAGFairQueueStore{
		store: d, expectedWriter: fence.ExpectedWriterFingerprint,
	}
	err := fairStore.withExpectedWriterTx(ctx, func(tx *sql.Tx) error {
		changed, err := d.acknowledgeRAGIndexTaskQuiescedOn(ctx, tx, fence)
		if err != nil || !changed {
			return err
		}
		acknowledged = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return acknowledged, nil
}

func (d *DBStore) acknowledgeRAGIndexTaskQuiescedOn(
	ctx context.Context,
	exec ragExecutor,
	fence IndexFence,
) (bool, error) {
	result, err := exec.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		lease_owner='',lease_until=NULL,heartbeat_at=NULL
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND claim_generation=%s
		AND dispatch_generation=claim_generation AND lease_owner=%s
		AND status='SUPERSEDED'`, d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5)), fence.TaskID, fence.DocID, fence.DocVersion,
		fence.ClaimGeneration, fence.LeaseOwner)
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
	route ragOwnershipRoute,
) (*ragLockedIndexFence, bool, error) {
	_, ownershipActive, err := d.lockRAGKBOwnerTx(ctx, tx, route.KBID, route.UserID)
	if err != nil || !ownershipActive {
		return nil, false, err
	}
	doc, err := d.ragDocumentInTx(ctx, tx, fence.DocID)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if doc.KBID != route.KBID || strings.EqualFold(doc.Status, RAGDocumentStatusDeleting) {
		return nil, false, nil
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
		task.DispatchGeneration == task.ClaimGeneration &&
		task.Status == "RUNNING" && task.LeaseUntil != nil && task.LeaseUntil.After(now) &&
		version.Status == RAGDocumentVersionRunning
	if fence.ExpectedWriterFingerprint != "" {
		valid = valid && task.UserID == route.UserID && doc.Version == fence.DocVersion &&
			strings.EqualFold(doc.Status, "PROCESSING")
	}
	if !valid {
		return nil, false, nil
	}
	return &ragLockedIndexFence{doc: doc, task: task, version: version, now: now}, true, nil
}

func (d *DBStore) ragOwnershipRouteOn(
	ctx context.Context,
	exec ragExecutor,
	docID string,
) (ragOwnershipRoute, error) {
	var route ragOwnershipRoute
	err := exec.QueryRowContext(ctx, fmt.Sprintf(`SELECT d.kb_id,kb.user_id
		FROM rag_documents d JOIN rag_kbs kb ON kb.id=d.kb_id WHERE d.id=%s`, d.ph(1)), docID).
		Scan(&route.KBID, &route.UserID)
	if err != nil {
		return ragOwnershipRoute{}, scanErr(err)
	}
	return route, nil
}

func (d *DBStore) withLiveRAGIndexFenceTx(
	ctx context.Context,
	fence IndexFence,
	fn func(*sql.Tx, *ragLockedIndexFence) (bool, error),
) (bool, error) {
	if fn == nil {
		return false, errors.New("store: nil RAG live fence transaction callback")
	}
	if fence.ExpectedWriterFingerprint == "" {
		route, err := d.ragOwnershipRoute(ctx, fence.DocID)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return false, err
		}
		defer func() { _ = tx.Rollback() }()
		locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence, route)
		if err != nil || !ok {
			return false, err
		}
		commit, err := fn(tx, locked)
		if err != nil || !commit {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return false, ErrFairQueueWriterMismatch
	}
	var committed bool
	err := d.withFairQueueExpectedWriterConn(ctx, fence.ExpectedWriterFingerprint,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			route, err := d.ragOwnershipRouteOn(ctx, conn, fence.DocID)
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			tx, err := conn.BeginTx(ctx, nil)
			if err != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, err)
			}
			txCommitted := false
			defer func() {
				if txCommitted {
					return
				}
				if rollbackErr := tx.Rollback(); rollbackErr != nil &&
					!errors.Is(rollbackErr, sql.ErrTxDone) {
					callbackErr = errors.Join(
						callbackErr, ErrFairQueueUnsafeConnection, rollbackErr,
					)
				}
			}()
			locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence, route)
			if err != nil || !ok {
				return err
			}
			commit, err := fn(tx, locked)
			if err != nil || !commit {
				return err
			}
			if err := verifyFairQueueMySQLSession(ctx, tx, identity, ""); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, err)
			}
			txCommitted = true
			committed = true
			return nil
		})
	if err != nil {
		return false, err
	}
	return committed, nil
}

func (d *DBStore) UpdateProgressRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	progress RAGIndexProgress,
) (bool, error) {
	return d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked.doc.Version != fence.DocVersion {
				return false, nil
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
				processing_stage=%s,progress_current=%s,progress_total=%s,progress_unit=%s
				WHERE id=%s AND version=%s AND UPPER(status)<>'DELETING'`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
				progress.Stage, progress.Current, progress.Total, progress.Unit,
				fence.DocID, fence.DocVersion); err != nil {
				return false, err
			}
			return true, nil
		})
}

func (d *DBStore) UpdateWarningRAGIndexTask(
	ctx context.Context,
	fence IndexFence,
	degraded bool,
	warningCount int,
) (bool, error) {
	return d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
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
			return true, nil
		})
}

// RecordRAGDocumentParseArtifact persists the deterministic object-store
// cleanup handle before the parser is allowed to publish any artifact bytes.
// The handle is immutable for a physical document version and remains on
// FAILED/SUPERSEDED/GCED tombstones so orphan cleanup can always find it.
func (d *DBStore) RecordRAGDocumentParseArtifact(
	ctx context.Context,
	fence IndexFence,
	artifactKey string,
) (bool, error) {
	artifactKey = strings.TrimSpace(artifactKey)
	if artifactKey == "" || strings.ContainsRune(artifactKey, '\x00') ||
		artifactKey != strings.ReplaceAll(artifactKey, "\\", "/") {
		return false, ErrRAGDocumentVersionMismatch
	}
	return d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked.version.ParseArtifactKey != "" && locked.version.ParseArtifactKey != artifactKey {
				return false, ErrRAGDocumentVersionConflict
			}
			var deletingHandles int
			if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
				FROM rag_object_write_staging WHERE doc_id=%s AND reference_key=%s
				AND status='DELETING'`, d.ph(1), d.ph(2)), fence.DocID, artifactKey).
				Scan(&deletingHandles); err != nil {
				return false, err
			}
			if deletingHandles != 0 {
				return false, ErrRAGLifecycleInactive
			}
			result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
				parse_artifact_key=%s,updated_at=%s WHERE doc_id=%s AND doc_version=%s
				AND status='RUNNING' AND (parse_artifact_key='' OR parse_artifact_key=%s)`,
				d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4)), artifactKey,
				fence.DocID, fence.DocVersion, artifactKey)
			if err != nil {
				return false, err
			}
			if updated, err := ragRowsAffected(result); err != nil || !updated {
				return false, err
			}
			return true, nil
		})
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
	return d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
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
			dispatchGeneration := locked.task.DispatchGeneration
			var dispatchedAt any
			if locked.task.DispatchedAt != nil {
				dispatchedAt = *locked.task.DispatchedAt
			}
			if retry {
				taskStatus = "PENDING"
				docStatus = "PENDING"
				stage = "retry_wait"
				if nextRunDelay > 0 {
					next := locked.now.Add(nextRunDelay)
					nextRunAt = &next
				}
				retryCount++
				baseGeneration := locked.task.DispatchGeneration
				if locked.task.ClaimGeneration > baseGeneration {
					baseGeneration = locked.task.ClaimGeneration
				}
				if baseGeneration == math.MaxInt64 {
					return false, ErrRAGDispatchGenerationExhausted
				}
				dispatchGeneration = baseGeneration + 1
				dispatchedAt = nil
			}
			result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status=%s,retry_count=%s,error_msg=%s,next_run_at=%s,
		dispatch_generation=%s,dispatched_at=%s,
		lease_owner='',lease_until=NULL,heartbeat_at=NULL,finished_at=%s
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND dispatch_generation=%s
		AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
		AND dispatch_generation=claim_generation AND lease_until > %s`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7),
				d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13),
				d.ragNowExpr()), taskStatus, retryCount, errMsg, nextRunAt,
				dispatchGeneration, dispatchedAt, func() any {
					if retry {
						return nil
					}
					return finished
				}(), fence.TaskID, fence.DocID, fence.DocVersion, locked.task.DispatchGeneration,
				fence.ClaimGeneration, fence.LeaseOwner)
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
			return true, nil
		})
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
	return d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked.doc.Version != fence.DocVersion {
				return false, nil
			}
			if err := d.reconcileRAGDocumentSourceHash(ctx, tx, locked.doc, locked.version.SourceSHA256); err != nil {
				return false, err
			}
			if locked.version.ParseArtifactKey != "" &&
				locked.version.ParseArtifactKey != activation.VersionResult.ParseArtifactKey {
				return false, ErrRAGDocumentVersionConflict
			}
			if artifactKey := strings.TrimSpace(activation.VersionResult.ParseArtifactKey); artifactKey != "" {
				normalizedKey := path.Join(path.Dir(artifactKey), "normalized.md")
				if err := d.consumeRAGObjectWritesInTx(
					ctx, tx, fence.DocID, normalizedKey, artifactKey,
				); err != nil {
					return false, err
				}
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
		AND dispatch_generation=%s AND claim_generation=%s AND lease_owner=%s
		AND status='RUNNING' AND dispatch_generation=claim_generation
		AND lease_until > %s`, d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3),
				d.ph(4), d.ph(5), d.ph(6), d.ragNowExpr()), fence.TaskID, fence.DocID,
				fence.DocVersion, locked.task.DispatchGeneration, fence.ClaimGeneration,
				fence.LeaseOwner)
			if err != nil {
				return false, err
			}
			if updated, err := ragRowsAffected(result); err != nil || !updated {
				return false, err
			}
			return true, nil
		})
}

func (d *DBStore) AdvanceDocumentVersionAndCreateTask(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
) (*RAGIndexTaskRecord, error) {
	return d.advanceDocumentVersionAndCreateTask(ctx, expectedVersion, snapshot, nil)
}

func (d *DBStore) AdvanceDocumentVersionAndCreateTaskPolicy(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
	policy RAGAdvancedEnqueuePolicy,
) (*RAGIndexTaskRecord, error) {
	return d.advanceDocumentVersionAndCreateTask(ctx, expectedVersion, snapshot, &policy)
}

func (d *DBStore) advanceDocumentVersionAndCreateTask(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
	policy *RAGAdvancedEnqueuePolicy,
) (*RAGIndexTaskRecord, error) {
	if snapshot == nil || snapshot.DocID == "" {
		return nil, ErrRAGDocumentVersionMismatch
	}
	// This read only discovers the lock-order routing key. Authorization and
	// liveness are re-evaluated after the user and KB rows are locked below.
	preflightDoc, err := d.GetRAGDocument(ctx, snapshot.DocID)
	if err != nil {
		return nil, err
	}
	preflightKB, err := d.GetRAGKB(ctx, preflightDoc.KBID)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrRAGLifecycleInactive
	}
	if err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	route := ragOwnershipRoute{KBID: preflightDoc.KBID, UserID: preflightKB.UserID}
	task, err := d.advanceDocumentVersionAndCreateTaskInTx(
		ctx, tx, expectedVersion, snapshot, policy, route,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

func (d *DBStore) advanceDocumentVersionAndCreateTaskInTx(
	ctx context.Context,
	tx *sql.Tx,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
	policy *RAGAdvancedEnqueuePolicy,
	route ragOwnershipRoute,
) (*RAGIndexTaskRecord, error) {
	if tx == nil || snapshot == nil || snapshot.DocID == "" {
		return nil, ErrRAGDocumentVersionMismatch
	}
	expectedOwner := route.UserID
	if policy != nil {
		expectedOwner = policy.UserID
	}
	if _, err := d.lockActiveRAGKBOwnerTx(ctx, tx, route.KBID, expectedOwner); err != nil {
		return nil, err
	}
	doc, err := d.ragDocumentInTx(ctx, tx, snapshot.DocID)
	if ragIsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if doc.KBID != route.KBID {
		return nil, ErrRAGDocumentVersionMismatch
	}
	if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, doc.ID); err != nil {
		return nil, err
	}
	if doc.Version != expectedVersion {
		return nil, ErrRAGDocumentVersionConflict
	}
	tombstoned, err := d.ragDeletionTombstonedInTx(ctx, tx, doc)
	if err != nil {
		return nil, err
	}
	if tombstoned {
		return nil, ErrRAGLifecycleInactive
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, doc, snapshot.SourceSHA256); err != nil {
		return nil, err
	}
	if err := d.enforceRAGAdvancedEnqueuePolicyTx(
		ctx, tx, doc.KBID, doc.ID, snapshot, policy, true,
	); err != nil {
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
			finished_at=%s,lease_owner=%s,lease_until=%s,heartbeat_at=NULL,next_run_at=NULL
			WHERE id=%s AND doc_version=%s AND dispatch_generation=%s AND claim_generation=%s
			AND status IN ('PENDING','RUNNING')`, d.ragNowExpr(), d.ph(1), d.ph(2),
			d.ph(3), d.ph(4), d.ph(5), d.ph(6)), task.LeaseOwner, task.LeaseUntil,
			task.ID, task.DocVersion, task.DispatchGeneration, task.ClaimGeneration); err != nil {
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
	var created *RAGIndexTaskRecord
	changed, err := d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked.doc.Version != fence.DocVersion {
				return false, nil
			}
			if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, locked.doc.ID); err != nil {
				return false, err
			}
			if err := d.reconcileRAGDocumentSourceHash(ctx, tx, locked.doc, snapshot.SourceSHA256); err != nil {
				return false, err
			}
			newVersion, err := d.nextRAGDocumentVersionInTx(ctx, tx, locked.doc)
			if err != nil {
				return false, err
			}
			copySnapshot := *snapshot
			copySnapshot.DocVersion = newVersion
			copySnapshot.CreatedAt = time.Time{}
			copySnapshot.UpdatedAt = time.Time{}
			prepareNewRAGDocumentVersion(&copySnapshot)
			if err := d.validateRAGVersionSnapshotForDocument(ctx, tx, locked.doc, &copySnapshot); err != nil {
				return false, err
			}
			result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='SUPERSEDED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status='RUNNING'`, d.ragNowExpr(), d.ph(1), d.ph(2)),
				fence.DocID, fence.DocVersion)
			if err != nil {
				return false, err
			}
			if updated, err := ragRowsAffected(result); err != nil || !updated {
				return false, err
			}
			result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status='SUPERSEDED',error_msg='provider snapshot changed',finished_at=%s,
		lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND dispatch_generation=%s
		AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
		AND dispatch_generation=claim_generation AND lease_until > %s`,
				d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
				d.ragNowExpr()), fence.TaskID, fence.DocID, fence.DocVersion,
				locked.task.DispatchGeneration, fence.ClaimGeneration, fence.LeaseOwner)
			if err != nil {
				return false, err
			}
			if updated, err := ragRowsAffected(result); err != nil || !updated {
				return false, err
			}
			if err := d.createRAGDocumentVersion(ctx, tx, &copySnapshot); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		version=%s,status='PENDING',error_msg='',processing_stage='queued',
		progress_current=0,progress_total=0,progress_unit='',degraded=FALSE,warning_count=0
		WHERE id=%s AND version=%s`, d.ph(1), d.ph(2), d.ph(3)),
				newVersion, fence.DocID, fence.DocVersion); err != nil {
				return false, err
			}
			taskID, err := d.createRAGIndexTaskForVersion(ctx, tx, fence.DocID, newVersion, locked.task.MaxRetry)
			if err != nil {
				return false, err
			}
			task, err := d.ragTaskInTx(ctx, tx, taskID)
			if err != nil {
				return false, err
			}
			created = task
			return true, nil
		})
	if err != nil || !changed {
		return nil, false, err
	}
	return created, true, nil
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
	route, err := d.ragOwnershipRoute(ctx, doc.ID)
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lockedDoc, _, err := d.lockRAGDocumentHierarchyTx(ctx, tx, doc.ID, route)
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
			doc_version=%s,user_id=%s,status='FAILED',dispatch_generation=1,
			claim_generation=0,dispatched_at=NULL,lease_owner='',lease_until=NULL,
			heartbeat_at=NULL,next_run_at=NULL,error_msg=%s,finished_at=%s
			WHERE id=%s AND doc_version IS NULL`, d.ph(1), d.ph(2), d.ph(3),
			d.ragNowExpr(), d.ph(4)), nextVersion, route.UserID, reason, survivorID); err != nil {
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
		doc_version=%s,user_id=%s,status='PENDING',dispatch_generation=1,
		claim_generation=0,dispatched_at=NULL,lease_owner='',lease_until=NULL,
		heartbeat_at=NULL,next_run_at=NULL,error_msg='',started_at=NULL,finished_at=NULL
		WHERE id=%s AND doc_version IS NULL`, d.ph(1), d.ph(2), d.ph(3)),
		nextVersion, route.UserID, survivorID); err != nil {
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
