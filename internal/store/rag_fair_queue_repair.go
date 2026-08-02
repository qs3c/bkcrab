package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const maxRAGFairQueueStorePageSize = 10_000

var (
	ErrRAGIndexTaskDispatchStale  = errors.New("store: stale RAG index dispatch candidate")
	ErrRAGIndexTaskDispatchGuard  = errors.New("store: invalid RAG index dispatch guard")
	ErrRAGFairQueueCanonicalOwner = errors.New("store: RAG fair queue canonical owner invariant violated")
)

// RAGIndexTaskTimestampGuard is the exact database representation of one
// nullable timestamp. An explicit null bit makes a missing JSON field invalid
// instead of silently treating it as a legitimate SQL NULL snapshot.
type RAGIndexTaskTimestampGuard struct {
	IsNull bool   `json:"is_null"`
	Raw    string `json:"raw"`
}

// RAGIndexTaskDispatchGuard is the immutable SQL snapshot that authorizes one
// publish marker or repair CAS. It is deliberately exported and JSON-stable so
// the RAG adapter can round-trip it through fairqueue's opaque Guard field.
type RAGIndexTaskDispatchGuard struct {
	TaskID             int64                      `json:"task_id"`
	DocID              string                     `json:"doc_id"`
	DocVersion         int64                      `json:"doc_version"`
	UserID             string                     `json:"user_id"`
	Status             string                     `json:"status"`
	DispatchGeneration int64                      `json:"dispatch_generation"`
	ClaimGeneration    int64                      `json:"claim_generation"`
	RetryCount         int                        `json:"retry_count"`
	LeaseOwner         string                     `json:"lease_owner"`
	NextRunAt          *time.Time                 `json:"next_run_at,omitempty"`
	LeaseUntil         *time.Time                 `json:"lease_until,omitempty"`
	DispatchedAt       *time.Time                 `json:"dispatched_at,omitempty"`
	NextRunAtRaw       RAGIndexTaskTimestampGuard `json:"next_run_at_raw"`
	LeaseUntilRaw      RAGIndexTaskTimestampGuard `json:"lease_until_raw"`
	DispatchedAtRaw    RAGIndexTaskTimestampGuard `json:"dispatched_at_raw"`
}

// RAGIndexTaskDispatchCandidate keeps the canonical task reference separate
// from the immutable guard returned by the source read. Mutations reject a
// candidate whose task and guard have been mixed or changed in memory.
type RAGIndexTaskDispatchCandidate struct {
	Task  RAGIndexTaskRecord        `json:"task"`
	Guard RAGIndexTaskDispatchGuard `json:"guard"`
}

// RAGIndexTaskRunningSnapshot carries the database clock observed by the same
// SELECT that proved the claim lease valid. Recovery must derive relative
// reservation TTLs from ObservedDBNow, never from an application host clock.
type RAGIndexTaskRunningSnapshot struct {
	Task          RAGIndexTaskRecord `json:"task"`
	ObservedDBNow time.Time          `json:"observed_db_now"`
}

const ragFairQueueTaskColumns = `t.id, t.doc_id, t.doc_version, t.user_id,
	t.status, t.retry_count, t.max_retry, t.dispatch_generation, t.claim_generation,
	t.dispatched_at, t.lease_owner, t.lease_until, t.heartbeat_at, t.next_run_at,
	t.error_msg, t.created_at, t.started_at, t.finished_at`

const ragFairQueueCanonicalJoin = ` FROM rag_index_tasks t
	JOIN rag_documents d ON d.id=t.doc_id
	JOIN rag_kbs kb ON kb.id=d.kb_id AND kb.user_id=t.user_id`

const ragFairQueueValidRunningPredicate = `t.status='RUNNING'
	AND t.dispatch_generation=t.claim_generation AND t.claim_generation>0
	AND t.lease_owner<>'' AND t.lease_until>rag_fair_clock.observed_db_now
	AND t.heartbeat_at IS NOT NULL AND t.next_run_at IS NULL
	AND v.status='RUNNING' AND d.version=t.doc_version
	AND UPPER(d.status)='PROCESSING'
	AND LOWER(kb.status)='active' AND LOWER(u.status)='active'`

func cloneRAGTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copyValue := *value
	return &copyValue
}

type ragIndexTaskRawTimestamps struct {
	nextRunAt, leaseUntil, dispatchedAt sql.NullString
}

func ragTimestampGuard(raw sql.NullString) RAGIndexTaskTimestampGuard {
	if !raw.Valid {
		return RAGIndexTaskTimestampGuard{IsNull: true}
	}
	return RAGIndexTaskTimestampGuard{Raw: raw.String}
}

func ragRawTimestampsFromGuard(guard RAGIndexTaskDispatchGuard) ragIndexTaskRawTimestamps {
	return ragIndexTaskRawTimestamps{
		nextRunAt: sql.NullString{
			String: guard.NextRunAtRaw.Raw,
			Valid:  !guard.NextRunAtRaw.IsNull,
		},
		leaseUntil: sql.NullString{
			String: guard.LeaseUntilRaw.Raw,
			Valid:  !guard.LeaseUntilRaw.IsNull,
		},
		dispatchedAt: sql.NullString{
			String: guard.DispatchedAtRaw.Raw,
			Valid:  !guard.DispatchedAtRaw.IsNull,
		},
	}
}

func newRAGIndexTaskDispatchCandidate(
	task RAGIndexTaskRecord,
	raw ragIndexTaskRawTimestamps,
) RAGIndexTaskDispatchCandidate {
	return RAGIndexTaskDispatchCandidate{
		Task: task,
		Guard: RAGIndexTaskDispatchGuard{
			TaskID:             task.ID,
			DocID:              task.DocID,
			DocVersion:         task.DocVersion,
			UserID:             task.UserID,
			Status:             task.Status,
			DispatchGeneration: task.DispatchGeneration,
			ClaimGeneration:    task.ClaimGeneration,
			RetryCount:         task.RetryCount,
			LeaseOwner:         task.LeaseOwner,
			NextRunAt:          cloneRAGTime(task.NextRunAt),
			LeaseUntil:         cloneRAGTime(task.LeaseUntil),
			DispatchedAt:       cloneRAGTime(task.DispatchedAt),
			NextRunAtRaw:       ragTimestampGuard(raw.nextRunAt),
			LeaseUntilRaw:      ragTimestampGuard(raw.leaseUntil),
			DispatchedAtRaw:    ragTimestampGuard(raw.dispatchedAt),
		},
	}
}

func ragGuardTimesEqual(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func ragTimestampGuardMatchesTime(raw RAGIndexTaskTimestampGuard, value *time.Time) bool {
	if raw.IsNull {
		return raw.Raw == "" && value == nil
	}
	if raw.Raw == "" || value == nil {
		return false
	}
	parsed, err := parseRAGDBTime(raw.Raw)
	if err != nil {
		parsed, err = time.Parse("2006-01-02 15:04:05.999999999 -0700 MST", raw.Raw)
	}
	return err == nil && parsed.Equal(*value)
}

func validRAGIndexTaskDispatchCandidate(candidate RAGIndexTaskDispatchCandidate) bool {
	task, guard := candidate.Task, candidate.Guard
	return guard.TaskID > 0 && guard.UserID != "" &&
		guard.UserID == strings.TrimSpace(guard.UserID) &&
		(task.Status == "PENDING" || task.Status == "RUNNING") &&
		task.ID == guard.TaskID && task.DocID == guard.DocID &&
		task.DocVersion == guard.DocVersion && task.UserID == guard.UserID &&
		task.Status == guard.Status && task.DispatchGeneration == guard.DispatchGeneration &&
		task.ClaimGeneration == guard.ClaimGeneration && task.RetryCount == guard.RetryCount &&
		task.LeaseOwner == guard.LeaseOwner &&
		ragGuardTimesEqual(task.NextRunAt, guard.NextRunAt) &&
		ragGuardTimesEqual(task.LeaseUntil, guard.LeaseUntil) &&
		ragGuardTimesEqual(task.DispatchedAt, guard.DispatchedAt) &&
		ragTimestampGuardMatchesTime(guard.NextRunAtRaw, guard.NextRunAt) &&
		ragTimestampGuardMatchesTime(guard.LeaseUntilRaw, guard.LeaseUntil) &&
		ragTimestampGuardMatchesTime(guard.DispatchedAtRaw, guard.DispatchedAt)
}

func validateRAGFairQueueIDPage(afterID int64, limit int) error {
	if afterID < 0 {
		return errors.New("store: RAG fair queue cursor must be non-negative")
	}
	if limit <= 0 || limit > maxRAGFairQueueStorePageSize {
		return fmt.Errorf("store: RAG fair queue page size must be in 1..%d", maxRAGFairQueueStorePageSize)
	}
	return nil
}

func validateRAGFairQueueHighWaterPage(highWater, afterID int64, limit int) error {
	if highWater < 0 {
		return errors.New("store: RAG fair queue high water must be non-negative")
	}
	return validateRAGFairQueueIDPage(afterID, limit)
}

func (d *DBStore) ragIndexTaskIDWindow(
	ctx context.Context,
	afterID int64,
	highWater *int64,
	limit int,
) ([]int64, int64, error) {
	query, args := d.ragIndexTaskIDWindowQuery(afterID, highWater, limit)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterID, err
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	nextAfterID := afterID
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, afterID, err
		}
		ids = append(ids, id)
		nextAfterID = id
	}
	if err := rows.Err(); err != nil {
		return nil, afterID, err
	}
	if len(ids) == 0 && highWater != nil && afterID < *highWater {
		nextAfterID = *highWater
	}
	return ids, nextAfterID, nil
}

func (d *DBStore) ragIndexTaskIDWindowQuery(
	afterID int64,
	highWater *int64,
	limit int,
) (string, []any) {
	query := `SELECT id FROM rag_index_tasks WHERE id>` + d.ph(1)
	args := []any{afterID}
	if highWater != nil {
		query += ` AND id<=` + d.ph(2)
		args = append(args, *highWater)
	}
	query += ` ORDER BY id LIMIT ` + d.ph(len(args)+1)
	args = append(args, limit)
	return query, args
}

func (d *DBStore) ragIndexTaskIDWindowPredicate(column string, ids []int64) (string, []any) {
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		placeholders[index] = d.ph(index + 1)
		args[index] = id
	}
	return column + ` IN (` + strings.Join(placeholders, ",") + `)`, args
}

func (d *DBStore) ragFairQueueDuePredicate(alias string) string {
	now := d.ragNowExpr()
	return fmt.Sprintf(`((%[1]s.status='PENDING'
		AND (%[1]s.next_run_at IS NULL OR %[1]s.next_run_at <= %[2]s))
		OR (%[1]s.status='RUNNING'
		AND (%[1]s.lease_until IS NULL OR %[1]s.lease_until <= %[2]s)
		AND (%[1]s.next_run_at IS NULL OR %[1]s.next_run_at <= %[2]s)))`, alias, now)
}

func (d *DBStore) ragTimestampRawExpression(column string) string {
	switch d.dialect {
	case mysqlDialect:
		return "CAST(" + column + " AS CHAR(26))"
	case "postgres":
		return "to_char(" + column + ", 'YYYY-MM-DD HH24:MI:SS.US')"
	default:
		return "CAST(" + column + " AS TEXT)"
	}
}

func (d *DBStore) ragFairQueueRawTimestampColumns(alias string) string {
	return ", " + d.ragTimestampRawExpression(alias+".next_run_at") +
		", " + d.ragTimestampRawExpression(alias+".lease_until") +
		", " + d.ragTimestampRawExpression(alias+".dispatched_at")
}

func (d *DBStore) ragNullSafeRawTimestampEqual(column string, placeholder int) string {
	expression := d.ragTimestampRawExpression(column)
	parameter := d.ph(placeholder)
	switch d.dialect {
	case mysqlDialect:
		return expression + " <=> CAST(" + parameter + " AS CHAR(26))"
	case "postgres":
		return expression + " IS NOT DISTINCT FROM CAST(" + parameter + " AS TEXT)"
	default:
		return "(" + expression + " IS CAST(" + parameter + " AS TEXT)" +
			" AND (" + column + " IS NULL OR typeof(" + column + ")='text'))"
	}
}

func ragTimestampGuardArgument(value RAGIndexTaskTimestampGuard) any {
	if value.IsNull {
		return nil
	}
	return value.Raw
}

func ragCanonicalTaskOwnerExists(alias string) string {
	return `EXISTS (SELECT 1 FROM rag_documents canonical_doc
		JOIN rag_kbs canonical_kb ON canonical_kb.id=canonical_doc.kb_id
		WHERE canonical_doc.id=` + alias + `.doc_id
		AND canonical_kb.user_id=` + alias + `.user_id)`
}

func scanRAGIndexTaskRecords(rows *sql.Rows, afterID int64) ([]RAGIndexTaskRecord, int64, error) {
	defer rows.Close()
	records := make([]RAGIndexTaskRecord, 0)
	nextAfterID := afterID
	for rows.Next() {
		task, err := scanRAGIndexTask(rows)
		if err != nil {
			return nil, afterID, err
		}
		records = append(records, *task)
		nextAfterID = task.ID
	}
	if err := rows.Err(); err != nil {
		return nil, afterID, err
	}
	return records, nextAfterID, nil
}

func ragDatabaseTimeValue(raw any) (time.Time, error) {
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

func (d *DBStore) scanRAGIndexTaskDispatchCandidate(
	scanner ragScanner,
) (*RAGIndexTaskDispatchCandidate, error) {
	var raw ragIndexTaskRawTimestamps
	task, err := scanRAGIndexTaskWithExtras(
		scanner, &raw.nextRunAt, &raw.leaseUntil, &raw.dispatchedAt,
	)
	if err != nil {
		return nil, err
	}
	candidate := newRAGIndexTaskDispatchCandidate(*task, raw)
	if !validRAGIndexTaskDispatchCandidate(candidate) {
		return nil, fmt.Errorf("%w: task=%d next=%+v lease=%+v dispatched=%+v",
			ErrRAGIndexTaskDispatchGuard, task.ID, candidate.Guard.NextRunAtRaw,
			candidate.Guard.LeaseUntilRaw, candidate.Guard.DispatchedAtRaw)
	}
	return &candidate, nil
}

func (d *DBStore) scanRAGIndexTaskDispatchCandidates(
	rows *sql.Rows,
	afterID int64,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	defer rows.Close()
	candidates := make([]RAGIndexTaskDispatchCandidate, 0)
	nextAfterID := afterID
	for rows.Next() {
		candidate, err := d.scanRAGIndexTaskDispatchCandidate(rows)
		if err != nil {
			return nil, afterID, err
		}
		candidates = append(candidates, *candidate)
		nextAfterID = candidate.Task.ID
	}
	if err := rows.Err(); err != nil {
		return nil, afterID, err
	}
	return candidates, nextAfterID, nil
}

func (d *DBStore) ragDispatchableRAGIndexTasksByIDsQuery(ids []int64) (string, []any) {
	idPredicate, args := d.ragIndexTaskIDWindowPredicate("t.id", ids)
	query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+d.ragFairQueueRawTimestampColumns("t")+
		ragFairQueueCanonicalJoin+` WHERE %s AND t.user_id IS NOT NULL
		AND t.dispatched_at IS NULL AND t.dispatch_generation>t.claim_generation`, idPredicate)
	query += ` AND ` + d.ragFairQueueDuePredicate("t") + ` ORDER BY t.id`
	return query, args
}

func (d *DBStore) ListDispatchableRAGIndexTasksPage(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	if err := validateRAGFairQueueIDPage(afterID, limit); err != nil {
		return nil, afterID, err
	}
	ids, nextAfterID, err := d.ragIndexTaskIDWindow(ctx, afterID, nil, limit)
	if err != nil || len(ids) == 0 {
		return nil, nextAfterID, err
	}
	query, args := d.ragDispatchableRAGIndexTasksByIDsQuery(ids)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterID, err
	}
	candidates, _, err := d.scanRAGIndexTaskDispatchCandidates(rows, afterID)
	if err != nil {
		return nil, afterID, err
	}
	return candidates, nextAfterID, nil
}

func (d *DBStore) GetDispatchableRAGIndexTaskByID(
	ctx context.Context,
	taskID int64,
) (*RAGIndexTaskDispatchCandidate, error) {
	if taskID <= 0 {
		return nil, ErrNotFound
	}
	query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+d.ragFairQueueRawTimestampColumns("t")+
		ragFairQueueCanonicalJoin+` WHERE t.id=%s
		AND t.user_id IS NOT NULL AND t.dispatched_at IS NULL
		AND t.dispatch_generation>t.claim_generation`, d.ph(1))
	query += ` AND ` + d.ragFairQueueDuePredicate("t")
	candidate, err := d.scanRAGIndexTaskDispatchCandidate(d.db.QueryRowContext(ctx, query, taskID))
	if err != nil {
		return nil, scanErr(err)
	}
	return candidate, nil
}

func (d *DBStore) MarkRAGIndexTaskDispatched(
	ctx context.Context,
	candidate RAGIndexTaskDispatchCandidate,
) (bool, error) {
	if !validRAGIndexTaskDispatchCandidate(candidate) || candidate.Guard.DispatchedAt != nil ||
		candidate.Guard.DispatchGeneration <= candidate.Guard.ClaimGeneration {
		return false, ErrRAGIndexTaskDispatchGuard
	}
	guard := candidate.Guard
	query := fmt.Sprintf(`UPDATE rag_index_tasks SET dispatched_at=%s
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND user_id=%s AND status=%s
		AND dispatch_generation=%s AND claim_generation=%s AND retry_count=%s
		AND lease_owner=%s AND %s AND %s AND dispatched_at IS NULL
		AND dispatch_generation>claim_generation AND %s`,
		d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9),
		d.ragNullSafeRawTimestampEqual("next_run_at", 10),
		d.ragNullSafeRawTimestampEqual("lease_until", 11),
		ragCanonicalTaskOwnerExists("rag_index_tasks"))
	query += ` AND ` + d.ragFairQueueDuePredicate("rag_index_tasks")
	result, err := d.db.ExecContext(ctx, query,
		guard.TaskID, guard.DocID, guard.DocVersion, guard.UserID, guard.Status,
		guard.DispatchGeneration, guard.ClaimGeneration, guard.RetryCount,
		guard.LeaseOwner, ragTimestampGuardArgument(guard.NextRunAtRaw),
		ragTimestampGuardArgument(guard.LeaseUntilRaw))
	if err != nil {
		return false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil {
		return false, err
	}
	if !updated {
		return false, ErrRAGIndexTaskDispatchStale
	}
	return true, nil
}

func (d *DBStore) ragExpiredRAGIndexTasksByIDsQuery(ids []int64) (string, []any) {
	idPredicate, args := d.ragIndexTaskIDWindowPredicate("t.id", ids)
	query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+d.ragFairQueueRawTimestampColumns("t")+
		ragFairQueueCanonicalJoin+` WHERE %s AND t.user_id IS NOT NULL
		AND t.status='RUNNING' AND t.dispatch_generation=t.claim_generation
		AND (t.lease_until IS NULL OR t.lease_until<=%s)
		AND (t.next_run_at IS NULL OR t.next_run_at<=%s)
		ORDER BY t.id`, idPredicate, d.ragNowExpr(), d.ragNowExpr())
	return query, args
}

func (d *DBStore) ArmExpiredRAGIndexTasksPage(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	if err := validateRAGFairQueueIDPage(afterID, limit); err != nil {
		return nil, afterID, err
	}
	ids, nextAfterID, err := d.ragIndexTaskIDWindow(ctx, afterID, nil, limit)
	if err != nil || len(ids) == 0 {
		return nil, nextAfterID, err
	}
	query, args := d.ragExpiredRAGIndexTasksByIDsQuery(ids)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterID, err
	}
	candidates, _, err := d.scanRAGIndexTaskDispatchCandidates(rows, afterID)
	if err != nil {
		return nil, afterID, err
	}
	armed := make([]RAGIndexTaskDispatchCandidate, 0, len(candidates))
	for i := range candidates {
		candidate, changed, err := d.armExpiredRAGIndexTask(ctx, candidates[i])
		if err != nil {
			return nil, afterID, err
		}
		if changed {
			armed = append(armed, *candidate)
		}
	}
	return armed, nextAfterID, nil
}

func (d *DBStore) armExpiredRAGIndexTask(
	ctx context.Context,
	original RAGIndexTaskDispatchCandidate,
) (*RAGIndexTaskDispatchCandidate, bool, error) {
	if !validRAGIndexTaskDispatchCandidate(original) || original.Task.Status != "RUNNING" ||
		original.Task.DispatchGeneration != original.Task.ClaimGeneration {
		return nil, false, ErrRAGIndexTaskDispatchGuard
	}
	task := original.Task
	if task.ClaimGeneration == math.MaxInt64 {
		return nil, false, ErrRAGDispatchGenerationExhausted
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks
		SET dispatch_generation=claim_generation+1,dispatched_at=NULL
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND user_id=%s
		AND status='RUNNING' AND dispatch_generation=%s AND claim_generation=%s
		AND retry_count=%s AND lease_owner=%s AND %s AND %s
		AND dispatch_generation=claim_generation
		AND (lease_until IS NULL OR lease_until<=%s)
		AND (next_run_at IS NULL OR next_run_at<=%s) AND %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8),
		d.ragNullSafeRawTimestampEqual("next_run_at", 9),
		d.ragNullSafeRawTimestampEqual("lease_until", 10), d.ragNowExpr(), d.ragNowExpr(),
		ragCanonicalTaskOwnerExists("rag_index_tasks")),
		task.ID, task.DocID, task.DocVersion, task.UserID, task.DispatchGeneration,
		task.ClaimGeneration, task.RetryCount, task.LeaseOwner,
		ragTimestampGuardArgument(original.Guard.NextRunAtRaw),
		ragTimestampGuardArgument(original.Guard.LeaseUntilRaw))
	if err != nil {
		return nil, false, err
	}
	changed, err := ragRowsAffected(result)
	if err != nil || !changed {
		return nil, false, err
	}
	task.DispatchGeneration = task.ClaimGeneration + 1
	task.DispatchedAt = nil
	raw := ragRawTimestampsFromGuard(original.Guard)
	raw.dispatchedAt = sql.NullString{}
	candidate := newRAGIndexTaskDispatchCandidate(task, raw)
	return &candidate, true, nil
}

func (d *DBStore) CaptureRAGFairQueueHighWater(ctx context.Context) (int64, error) {
	var highWater int64
	err := d.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM rag_index_tasks`).Scan(&highWater)
	return highWater, err
}

func (d *DBStore) ragTenantOwnerWindowQuery(
	afterUserID string,
	limit int,
) (string, []any) {
	from := `users`
	if d.dialect == mysqlDialect {
		from += ` FORCE INDEX (PRIMARY)`
	}
	query := fmt.Sprintf(`SELECT id FROM %s WHERE id>%s ORDER BY id LIMIT %s`,
		from, d.ph(1), d.ph(2))
	return query, []any{afterUserID, limit}
}

func (d *DBStore) ragTenantOwnerWindow(
	ctx context.Context,
	afterUserID string,
	limit int,
) ([]string, string, error) {
	query, args := d.ragTenantOwnerWindowQuery(afterUserID, limit)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterUserID, err
	}
	defer rows.Close()
	owners := make([]string, 0, limit)
	nextUserID := afterUserID
	for rows.Next() {
		var userID string
		if err := rows.Scan(&userID); err != nil {
			return nil, afterUserID, err
		}
		owners = append(owners, userID)
		nextUserID = userID
	}
	if err := rows.Err(); err != nil {
		return nil, afterUserID, err
	}
	return owners, nextUserID, nil
}

func (d *DBStore) ragCanonicalTenantFirstTasksQuery(
	highWater int64,
	ownerIDs []string,
) (string, []any) {
	placeholders := make([]string, len(ownerIDs))
	args := make([]any, 0, len(ownerIDs)+1)
	args = append(args, highWater)
	for index, ownerID := range ownerIDs {
		placeholders[index] = d.ph(index + 2)
		args = append(args, ownerID)
	}
	ownerPredicate := `candidate_user.id IN (` + strings.Join(placeholders, ",") + `)`
	if d.dialect == mysqlDialect {
		return `SELECT candidate_user.id,first_task.id,first_task.user_id,d.id,kb.id,kb.user_id
			FROM users candidate_user
			LEFT JOIN LATERAL (
				SELECT t.id FROM rag_index_tasks t FORCE INDEX (idx_rag_index_tasks_user_id)
				WHERE t.user_id=candidate_user.id AND t.id<=?
				ORDER BY t.user_id,t.id LIMIT 1
			) first_owner_task ON TRUE
			LEFT JOIN rag_index_tasks first_task ON first_task.id=first_owner_task.id
			LEFT JOIN rag_documents d ON d.id=first_task.doc_id
			LEFT JOIN rag_kbs kb ON kb.id=d.kb_id
			WHERE ` + ownerPredicate + `
			ORDER BY candidate_user.id`, args
	}
	return `SELECT candidate_user.id,first_task.id,first_task.user_id,d.id,kb.id,kb.user_id
		FROM users candidate_user
		LEFT JOIN rag_index_tasks first_task ON first_task.id=(
			SELECT t.id FROM rag_index_tasks t
			WHERE t.user_id=candidate_user.id AND t.id<=` + d.ph(1) + `
			ORDER BY t.id LIMIT 1
		)
		LEFT JOIN rag_documents d ON d.id=first_task.doc_id
		LEFT JOIN rag_kbs kb ON kb.id=d.kb_id
		WHERE ` + ownerPredicate + `
		ORDER BY candidate_user.id`, args
}

func (d *DBStore) ListCanonicalRAGTenantsPage(
	ctx context.Context,
	highWater int64,
	afterUserID string,
	limit int,
) ([]string, string, error) {
	if highWater < 0 {
		return nil, afterUserID, errors.New("store: RAG fair queue high water must be non-negative")
	}
	if limit <= 0 || limit > maxRAGFairQueueStorePageSize {
		return nil, afterUserID, fmt.Errorf("store: RAG fair queue page size must be in 1..%d", maxRAGFairQueueStorePageSize)
	}
	owners, nextUserID, err := d.ragTenantOwnerWindow(ctx, afterUserID, limit)
	if err != nil || len(owners) == 0 {
		return []string{}, nextUserID, err
	}
	query, args := d.ragCanonicalTenantFirstTasksQuery(highWater, owners)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterUserID, err
	}
	defer rows.Close()
	tenants := make([]string, 0, len(owners))
	for rows.Next() {
		var userID string
		var taskID sql.NullInt64
		var taskUserID, documentID, kbID, kbOwnerID sql.NullString
		if err := rows.Scan(
			&userID, &taskID, &taskUserID, &documentID, &kbID, &kbOwnerID,
		); err != nil {
			return nil, afterUserID, err
		}
		if !taskID.Valid {
			continue
		}
		if !taskUserID.Valid || taskUserID.String != userID || !documentID.Valid || !kbID.Valid ||
			!kbOwnerID.Valid || kbOwnerID.String != userID {
			return nil, afterUserID, fmt.Errorf("%w: user %q first task %d",
				ErrRAGFairQueueCanonicalOwner, userID, taskID.Int64)
		}
		tenants = append(tenants, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, afterUserID, err
	}
	return tenants, nextUserID, nil
}

func (d *DBStore) ragDispatchedRAGIndexTasksByIDsQuery(ids []int64) (string, []any) {
	idPredicate, args := d.ragIndexTaskIDWindowPredicate("t.id", ids)
	query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+
		ragFairQueueCanonicalJoin+` WHERE %s
		AND t.dispatched_at IS NOT NULL AND ((t.status='PENDING'
		AND t.dispatch_generation>t.claim_generation) OR (t.status='RUNNING'
		AND t.dispatch_generation>=t.claim_generation)) ORDER BY t.id`, idPredicate)
	return query, args
}

func (d *DBStore) ListDispatchedRAGIndexTasksPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskRecord, int64, error) {
	if err := validateRAGFairQueueHighWaterPage(highWater, afterTaskID, limit); err != nil {
		return nil, afterTaskID, err
	}
	ids, nextAfterID, err := d.ragIndexTaskIDWindow(ctx, afterTaskID, &highWater, limit)
	if err != nil || len(ids) == 0 {
		return nil, nextAfterID, err
	}
	query, args := d.ragDispatchedRAGIndexTasksByIDsQuery(ids)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterTaskID, err
	}
	records, _, err := scanRAGIndexTaskRecords(rows, afterTaskID)
	if err != nil {
		return nil, afterTaskID, err
	}
	return records, nextAfterID, nil
}

func (d *DBStore) ragValidRunningRAGIndexTasksByIDsQuery(ids []int64) (string, []any) {
	idPredicate, args := d.ragIndexTaskIDWindowPredicate("t.id", ids)
	materialized := ""
	if d.dialect == "postgres" {
		materialized = "MATERIALIZED "
	}
	query := fmt.Sprintf(`WITH rag_fair_clock AS %s(SELECT %s AS observed_db_now)
		SELECT `+ragFairQueueTaskColumns+`,rag_fair_clock.observed_db_now
		FROM rag_index_tasks t
		JOIN rag_documents d ON d.id=t.doc_id
		JOIN rag_kbs kb ON kb.id=d.kb_id AND kb.user_id=t.user_id
		JOIN users u ON u.id=kb.user_id
		JOIN rag_document_versions v ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
		CROSS JOIN rag_fair_clock
		WHERE %s AND `+ragFairQueueValidRunningPredicate+`
		ORDER BY t.id`, materialized, d.ragNowExpr(), idPredicate)
	return query, args
}

func (d *DBStore) ListValidRunningRAGIndexTasksPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskRunningSnapshot, int64, error) {
	if err := validateRAGFairQueueHighWaterPage(highWater, afterTaskID, limit); err != nil {
		return nil, afterTaskID, err
	}
	ids, nextAfterID, err := d.ragIndexTaskIDWindow(ctx, afterTaskID, &highWater, limit)
	if err != nil || len(ids) == 0 {
		return nil, nextAfterID, err
	}
	query, args := d.ragValidRunningRAGIndexTasksByIDsQuery(ids)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterTaskID, err
	}
	defer rows.Close()
	snapshots := make([]RAGIndexTaskRunningSnapshot, 0)
	for rows.Next() {
		var rawObserved any
		task, err := scanRAGIndexTaskWithExtras(rows, &rawObserved)
		if err != nil {
			return nil, afterTaskID, err
		}
		observed, err := ragDatabaseTimeValue(rawObserved)
		if err != nil {
			return nil, afterTaskID, err
		}
		snapshots = append(snapshots, RAGIndexTaskRunningSnapshot{
			Task: *task, ObservedDBNow: observed,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, afterTaskID, err
	}
	return snapshots, nextAfterID, nil
}

func (d *DBStore) CaptureRAGBrokerRepairHighWater(ctx context.Context) (int64, error) {
	return d.CaptureRAGFairQueueHighWater(ctx)
}

func (d *DBStore) ragBrokerBackedRAGCandidatesByIDsQuery(ids []int64) (string, []any) {
	idPredicate, args := d.ragIndexTaskIDWindowPredicate("t.id", ids)
	query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+d.ragFairQueueRawTimestampColumns("t")+
		ragFairQueueCanonicalJoin+` WHERE %s
		AND t.dispatched_at IS NOT NULL AND t.dispatch_generation>t.claim_generation`,
		idPredicate)
	query += ` AND ` + d.ragFairQueueDuePredicate("t") + ` ORDER BY t.id`
	return query, args
}

func (d *DBStore) ListBrokerBackedRAGCandidatesPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	if err := validateRAGFairQueueHighWaterPage(highWater, afterTaskID, limit); err != nil {
		return nil, afterTaskID, err
	}
	ids, nextAfterID, err := d.ragIndexTaskIDWindow(ctx, afterTaskID, &highWater, limit)
	if err != nil || len(ids) == 0 {
		return nil, nextAfterID, err
	}
	query, args := d.ragBrokerBackedRAGCandidatesByIDsQuery(ids)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, afterTaskID, err
	}
	candidates, _, err := d.scanRAGIndexTaskDispatchCandidates(rows, afterTaskID)
	if err != nil {
		return nil, afterTaskID, err
	}
	return candidates, nextAfterID, nil
}

func (d *DBStore) RearmRAGCandidateAfterBrokerLoss(
	ctx context.Context,
	original RAGIndexTaskDispatchCandidate,
) (*RAGIndexTaskDispatchCandidate, bool, error) {
	if !validRAGIndexTaskDispatchCandidate(original) || original.Guard.DispatchedAt == nil ||
		original.Guard.DispatchGeneration <= original.Guard.ClaimGeneration {
		return nil, false, ErrRAGIndexTaskDispatchGuard
	}
	guard := original.Guard
	baseGeneration := guard.DispatchGeneration
	if guard.ClaimGeneration > baseGeneration {
		baseGeneration = guard.ClaimGeneration
	}
	if baseGeneration == math.MaxInt64 {
		return nil, false, ErrRAGDispatchGenerationExhausted
	}
	newGeneration := baseGeneration + 1
	query := fmt.Sprintf(`UPDATE rag_index_tasks
		SET dispatch_generation=%s,dispatched_at=NULL
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND user_id=%s AND status=%s
		AND dispatch_generation=%s AND claim_generation=%s AND retry_count=%s
		AND lease_owner=%s AND %s AND %s AND %s
		AND dispatch_generation>claim_generation AND %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8),
		d.ph(9), d.ph(10), d.ragNullSafeRawTimestampEqual("next_run_at", 11),
		d.ragNullSafeRawTimestampEqual("lease_until", 12),
		d.ragNullSafeRawTimestampEqual("dispatched_at", 13),
		ragCanonicalTaskOwnerExists("rag_index_tasks"))
	query += ` AND ` + d.ragFairQueueDuePredicate("rag_index_tasks")
	result, err := d.db.ExecContext(ctx, query,
		newGeneration, guard.TaskID, guard.DocID, guard.DocVersion, guard.UserID,
		guard.Status, guard.DispatchGeneration, guard.ClaimGeneration, guard.RetryCount,
		guard.LeaseOwner, ragTimestampGuardArgument(guard.NextRunAtRaw),
		ragTimestampGuardArgument(guard.LeaseUntilRaw),
		ragTimestampGuardArgument(guard.DispatchedAtRaw))
	if err != nil {
		return nil, false, err
	}
	changed, err := ragRowsAffected(result)
	if err != nil || !changed {
		return nil, false, err
	}
	updated := original.Task
	updated.DispatchGeneration = newGeneration
	updated.DispatchedAt = nil
	raw := ragRawTimestampsFromGuard(original.Guard)
	raw.dispatchedAt = sql.NullString{}
	candidate := newRAGIndexTaskDispatchCandidate(updated, raw)
	return &candidate, true, nil
}
