package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	ErrRAGDocumentAIBudgetExceeded = errors.New("store: RAG DocumentAI budget exceeded")
	ErrRAGDocumentAIUsageConflict  = errors.New("store: RAG DocumentAI idempotency key conflict")
	ErrRAGDocumentAIInvalidFence   = errors.New("store: stale RAG DocumentAI index fence")
	ErrRAGDocumentAILedgerCorrupt  = errors.New("store: corrupt RAG DocumentAI usage ledger")
)

// RAGDocumentAILimits is the current user-period allowance supplied by the
// runtime configuration snapshot. Task limits live in the durable task budget
// row; user limits are deliberately not copied into the aggregate row.
type RAGDocumentAILimits struct {
	MaxRequests     int64
	MaxTokens       int64
	MaxCostMicroUSD int64
}

// ragDocumentAIBudgetTx lets SQLite use BEGIN IMMEDIATE while the server
// databases use database/sql transactions and SELECT FOR UPDATE. SQLite has a
// single connection, so holding this connection also prevents another writer
// from entering between the aggregate checks and usage insertion.
type ragDocumentAIBudgetTx struct {
	exec     ragExecutor
	commit   func() error
	rollback func() error
	done     bool
}

type ragSQLiteBudgetConn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	Close() error
}

func commitRAGSQLiteBudgetTx(conn ragSQLiteBudgetConn) error {
	_, commitErr := conn.ExecContext(context.Background(), "COMMIT")
	if commitErr != nil {
		// database/sql cannot see the transaction started by the raw BEGIN
		// IMMEDIATE statement. A failed COMMIT can therefore leave the pooled
		// connection in a transaction unless we explicitly roll it back before
		// returning it to the (single-connection) SQLite pool.
		_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
		closeErr := conn.Close()
		return errors.Join(commitErr, rollbackErr, closeErr)
	}
	return conn.Close()
}

func rollbackRAGSQLiteBudgetTx(conn ragSQLiteBudgetConn) error {
	_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
	closeErr := conn.Close()
	return errors.Join(rollbackErr, closeErr)
}

type ragDocumentAIReconcileCandidate struct {
	key   string
	state string
}

type ragDocumentAIReconcileRows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close() error
}

func collectRAGDocumentAIReconcileCandidates(
	rows ragDocumentAIReconcileRows,
) ([]ragDocumentAIReconcileCandidate, error) {
	var candidates []ragDocumentAIReconcileCandidate
	for rows.Next() {
		var item ragDocumentAIReconcileCandidate
		if err := rows.Scan(&item.key, &item.state); err != nil {
			_ = rows.Close()
			return nil, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (d *DBStore) beginRAGDocumentAIBudgetTx(ctx context.Context) (*ragDocumentAIBudgetTx, error) {
	if d.dialect != "sqlite" {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		return &ragDocumentAIBudgetTx{
			exec:     tx,
			commit:   tx.Commit,
			rollback: tx.Rollback,
		}, nil
	}

	conn, err := d.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &ragDocumentAIBudgetTx{
		exec:     conn,
		commit:   func() error { return commitRAGSQLiteBudgetTx(conn) },
		rollback: func() error { return rollbackRAGSQLiteBudgetTx(conn) },
	}, nil
}

func (tx *ragDocumentAIBudgetTx) Commit() error {
	if tx.done {
		return nil
	}
	tx.done = true
	return tx.commit()
}

func (tx *ragDocumentAIBudgetTx) Rollback() {
	if tx == nil || tx.done {
		return
	}
	tx.done = true
	_ = tx.rollback()
}

func (d *DBStore) upsertRAGDocumentAIUserBudgetTx(
	ctx context.Context,
	tx ragExecutor,
	userID string,
	periodStartUTC time.Time,
) error {
	query := fmt.Sprintf(`INSERT INTO rag_document_ai_user_budgets
		(user_id,period_start_utc,charged_requests,charged_tokens,charged_cost_microusd,updated_at)
		VALUES (%s,%s,0,0,0,%s)`, d.ph(1), d.ph(2), d.ragNowExpr())
	if d.dialect == mysqlDialect {
		query += ` ON DUPLICATE KEY UPDATE user_id=user_id`
	} else {
		query += ` ON CONFLICT (user_id,period_start_utc) DO NOTHING`
	}
	_, err := tx.ExecContext(ctx, query, userID, ragPeriodDate(periodStartUTC))
	return err
}

func (d *DBStore) lockRAGDocumentAIUserBudgetTx(
	ctx context.Context,
	tx ragExecutor,
	userID string,
	periodStartUTC time.Time,
) (*RAGDocumentAIUserBudgetRecord, error) {
	return scanRAGDocumentAIUserBudget(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT user_id,period_start_utc,charged_requests,charged_tokens,
		 charged_cost_microusd,updated_at FROM rag_document_ai_user_budgets
		 WHERE user_id=%s AND period_start_utc=%s%s`, d.ph(1), d.ph(2), d.ragLockSuffix()),
		userID, ragPeriodDate(periodStartUTC)))
}

func (d *DBStore) lockRAGDocumentAITaskBudgetTx(
	ctx context.Context,
	tx ragExecutor,
	taskID int64,
) (*RAGDocumentAITaskBudgetRecord, error) {
	return scanRAGDocumentAITaskBudget(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT task_id,user_id,max_requests,max_tokens,max_cost_microusd,
		 charged_requests,charged_tokens,charged_cost_microusd,updated_at
		 FROM rag_document_ai_task_budgets WHERE task_id=%s%s`, d.ph(1), d.ragLockSuffix()), taskID))
}

func (d *DBStore) lockRAGDocumentAIIndexTaskTx(
	ctx context.Context,
	tx ragExecutor,
	taskID int64,
) (*RAGIndexTaskRecord, error) {
	return scanRAGIndexTask(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), taskID))
}

func (d *DBStore) lockRAGDocumentAIUsageTx(
	ctx context.Context,
	tx ragExecutor,
	idempotencyKey string,
) (*RAGDocumentAIUsageRecord, error) {
	return scanRAGDocumentAIUsage(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentAIUsageColumns+` FROM rag_document_ai_usage
		 WHERE idempotency_key=%s%s`, d.ph(1), d.ragLockSuffix()), idempotencyKey))
}

func (d *DBStore) currentRAGDocumentAIFenceTx(
	ctx context.Context,
	tx ragExecutor,
	fence IndexFence,
) (bool, error) {
	task, err := d.lockRAGDocumentAIIndexTaskTx(ctx, tx, fence.TaskID)
	if ragIsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return false, err
	}
	return task.DocID == fence.DocID &&
		task.DocVersion == fence.DocVersion &&
		task.ClaimGeneration == fence.ClaimGeneration &&
		task.LeaseOwner == fence.LeaseOwner &&
		task.Status == "RUNNING" &&
		task.LeaseUntil != nil && task.LeaseUntil.After(now), nil
}

func ragDocumentAIUsageMatchesFence(usage *RAGDocumentAIUsageRecord, fence IndexFence) bool {
	return usage.TaskID == fence.TaskID && usage.DocID == fence.DocID &&
		usage.DocVersion == fence.DocVersion &&
		usage.ClaimGeneration == fence.ClaimGeneration &&
		usage.LeaseOwner == fence.LeaseOwner
}

func ragDocumentAIReservationsEqual(left, right *RAGDocumentAIUsageRecord) bool {
	return left.IdempotencyKey == right.IdempotencyKey &&
		left.LogicalRequestKey == right.LogicalRequestKey &&
		left.UserID == right.UserID && left.DocID == right.DocID &&
		left.TaskID == right.TaskID && left.DocVersion == right.DocVersion &&
		left.ClaimGeneration == right.ClaimGeneration && left.LeaseOwner == right.LeaseOwner &&
		left.Operation == right.Operation && left.ProviderFingerprint == right.ProviderFingerprint &&
		ragPeriodDate(left.PeriodStartUTC) == ragPeriodDate(right.PeriodStartUTC) &&
		left.ReservedInputTokens == right.ReservedInputTokens &&
		left.ReservedOutputTokens == right.ReservedOutputTokens &&
		left.EstimatedCostMicroUSD == right.EstimatedCostMicroUSD
}

func ragDocumentAITokenTotal(input, output int64) (int64, error) {
	if input < 0 || output < 0 || input > math.MaxInt64-output {
		return 0, errors.New("store: invalid RAG DocumentAI token reservation")
	}
	return input + output, nil
}

func ragDocumentAIAdd(current, delta int64) (int64, bool) {
	if delta > 0 && current > math.MaxInt64-delta {
		return 0, false
	}
	if delta < 0 && current < -delta {
		return 0, false
	}
	return current + delta, true
}

func ragDocumentAIWithinLimits(requests, tokens, cost int64, limits RAGDocumentAILimits) bool {
	return limits.MaxRequests >= 0 && limits.MaxTokens >= 0 && limits.MaxCostMicroUSD >= 0 &&
		requests <= limits.MaxRequests && tokens <= limits.MaxTokens && cost <= limits.MaxCostMicroUSD
}

func (d *DBStore) updateRAGDocumentAIChargesTx(
	ctx context.Context,
	tx ragExecutor,
	userBudget *RAGDocumentAIUserBudgetRecord,
	taskBudget *RAGDocumentAITaskBudgetRecord,
	requestDelta, tokenDelta, costDelta int64,
) error {
	if requestDelta == 0 && tokenDelta == 0 && costDelta == 0 {
		return nil
	}
	userRequests, ok := ragDocumentAIAdd(userBudget.ChargedRequests, requestDelta)
	if !ok {
		return ErrRAGDocumentAILedgerCorrupt
	}
	userTokens, ok := ragDocumentAIAdd(userBudget.ChargedTokens, tokenDelta)
	if !ok {
		return ErrRAGDocumentAILedgerCorrupt
	}
	userCost, ok := ragDocumentAIAdd(userBudget.ChargedCostMicroUSD, costDelta)
	if !ok {
		return ErrRAGDocumentAILedgerCorrupt
	}
	taskRequests, ok := ragDocumentAIAdd(taskBudget.ChargedRequests, requestDelta)
	if !ok {
		return ErrRAGDocumentAILedgerCorrupt
	}
	taskTokens, ok := ragDocumentAIAdd(taskBudget.ChargedTokens, tokenDelta)
	if !ok {
		return ErrRAGDocumentAILedgerCorrupt
	}
	taskCost, ok := ragDocumentAIAdd(taskBudget.ChargedCostMicroUSD, costDelta)
	if !ok {
		return ErrRAGDocumentAILedgerCorrupt
	}

	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_ai_user_budgets SET
		charged_requests=%s,charged_tokens=%s,charged_cost_microusd=%s,updated_at=%s
		WHERE user_id=%s AND period_start_utc=%s`, d.ph(1), d.ph(2), d.ph(3),
		d.ragNowExpr(), d.ph(4), d.ph(5)), userRequests, userTokens, userCost,
		userBudget.UserID, ragPeriodDate(userBudget.PeriodStartUTC))
	if err != nil {
		return err
	}
	if changed, err := ragRowsAffected(result); err != nil || !changed {
		if err != nil {
			return err
		}
		return ErrRAGDocumentAILedgerCorrupt
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_ai_task_budgets SET
		charged_requests=%s,charged_tokens=%s,charged_cost_microusd=%s,updated_at=%s
		WHERE task_id=%s AND user_id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ragNowExpr(),
		d.ph(4), d.ph(5)), taskRequests, taskTokens, taskCost, taskBudget.TaskID, taskBudget.UserID)
	if err != nil {
		return err
	}
	if changed, err := ragRowsAffected(result); err != nil || !changed {
		if err != nil {
			return err
		}
		return ErrRAGDocumentAILedgerCorrupt
	}
	return nil
}

func (d *DBStore) insertRAGDocumentAIUsageTx(
	ctx context.Context,
	tx ragExecutor,
	usage *RAGDocumentAIUsageRecord,
) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_document_ai_usage (
		idempotency_key,logical_request_key,user_id,doc_id,task_id,doc_version,
		claim_generation,lease_owner,operation,provider_fingerprint,period_start_utc,
		reserved_input_tokens,reserved_output_tokens,actual_input_tokens,actual_output_tokens,
		estimated_cost_microusd,state,reservation_expires_at,sent_at,usage_estimated,created_at,updated_at)
		VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,0,0,%s,'RESERVED',%s,NULL,FALSE,%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8),
		d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15),
		d.ragNowExpr(), d.ragNowExpr()), usage.IdempotencyKey, usage.LogicalRequestKey,
		usage.UserID, usage.DocID, usage.TaskID, usage.DocVersion, usage.ClaimGeneration,
		usage.LeaseOwner, usage.Operation, usage.ProviderFingerprint,
		ragPeriodDate(usage.PeriodStartUTC), usage.ReservedInputTokens,
		usage.ReservedOutputTokens, usage.EstimatedCostMicroUSD, usage.ReservationExpiresAt)
	return err
}

func (d *DBStore) hasRAGDocumentAIOverrunTx(
	ctx context.Context,
	tx ragExecutor,
	usage *RAGDocumentAIUsageRecord,
) (bool, error) {
	var key string
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT idempotency_key
		FROM rag_document_ai_usage WHERE state='OVERRUN' AND
		(task_id=%s OR (user_id=%s AND period_start_utc=%s))
		ORDER BY updated_at LIMIT 1%s`, d.ph(1), d.ph(2), d.ph(3), d.ragLockSuffix()),
		usage.TaskID, usage.UserID, ragPeriodDate(usage.PeriodStartUTC)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ReserveRAGDocumentAIUsage atomically reserves one request and its
// conservative token/cost maxima. true means this call inserted a new
// reservation. An exact duplicate returns false,nil without charging;
// conflicting reuse of the idempotency key fails. Durable result-cache
// existence is intentionally owned by the object cache, not inferred from a
// settled usage row: a provider response may be charged even when persisting
// its cache object failed or the worker crashed before doing so.
func (d *DBStore) ReserveRAGDocumentAIUsage(
	ctx context.Context,
	fence IndexFence,
	usage *RAGDocumentAIUsageRecord,
	userLimits RAGDocumentAILimits,
) (bool, error) {
	if usage == nil || usage.IdempotencyKey == "" || usage.UserID == "" ||
		usage.PeriodStartUTC.IsZero() || usage.EstimatedCostMicroUSD < 0 {
		return false, errors.New("store: invalid RAG DocumentAI reservation")
	}
	reservedTokens, err := ragDocumentAITokenTotal(usage.ReservedInputTokens, usage.ReservedOutputTokens)
	if err != nil {
		return false, err
	}
	if !ragDocumentAIUsageMatchesFence(usage, fence) {
		return false, ErrRAGDocumentAIInvalidFence
	}
	if userLimits.MaxRequests < 0 || userLimits.MaxTokens < 0 || userLimits.MaxCostMicroUSD < 0 {
		return false, errors.New("store: invalid RAG DocumentAI user limits")
	}

	tx, err := d.beginRAGDocumentAIBudgetTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	dbNow, err := d.ragDBNow(ctx, tx.exec)
	if err != nil {
		return false, err
	}
	currentPeriod := time.Date(dbNow.UTC().Year(), dbNow.UTC().Month(), dbNow.UTC().Day(), 0, 0, 0, 0, time.UTC)
	if ragPeriodDate(usage.PeriodStartUTC) != ragPeriodDate(currentPeriod) {
		return false, errors.New("store: RAG DocumentAI period must be the current database UTC day")
	}
	if err := d.upsertRAGDocumentAIUserBudgetTx(ctx, tx.exec, usage.UserID, usage.PeriodStartUTC); err != nil {
		return false, err
	}
	userBudget, err := d.lockRAGDocumentAIUserBudgetTx(ctx, tx.exec, usage.UserID, usage.PeriodStartUTC)
	if err != nil {
		return false, err
	}
	taskBudget, err := d.lockRAGDocumentAITaskBudgetTx(ctx, tx.exec, usage.TaskID)
	if ragIsNoRows(err) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if taskBudget.UserID != usage.UserID {
		return false, ErrRAGDocumentAIUsageConflict
	}
	currentFence, err := d.currentRAGDocumentAIFenceTx(ctx, tx.exec, fence)
	if err != nil {
		return false, err
	}
	existing, err := d.lockRAGDocumentAIUsageTx(ctx, tx.exec, usage.IdempotencyKey)
	if err == nil {
		if !ragDocumentAIReservationsEqual(existing, usage) {
			return false, ErrRAGDocumentAIUsageConflict
		}
		return false, nil
	}
	if !ragIsNoRows(err) {
		return false, err
	}
	if !currentFence {
		return false, ErrRAGDocumentAIInvalidFence
	}
	overrun, err := d.hasRAGDocumentAIOverrunTx(ctx, tx.exec, usage)
	if err != nil {
		return false, err
	}
	if overrun {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	userRequests, ok := ragDocumentAIAdd(userBudget.ChargedRequests, 1)
	if !ok {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	userTokens, ok := ragDocumentAIAdd(userBudget.ChargedTokens, reservedTokens)
	if !ok {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	userCost, ok := ragDocumentAIAdd(userBudget.ChargedCostMicroUSD, usage.EstimatedCostMicroUSD)
	if !ok || !ragDocumentAIWithinLimits(userRequests, userTokens, userCost, userLimits) {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	taskRequests, ok := ragDocumentAIAdd(taskBudget.ChargedRequests, 1)
	if !ok {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	taskTokens, ok := ragDocumentAIAdd(taskBudget.ChargedTokens, reservedTokens)
	if !ok {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	taskCost, ok := ragDocumentAIAdd(taskBudget.ChargedCostMicroUSD, usage.EstimatedCostMicroUSD)
	if !ok || !ragDocumentAIWithinLimits(taskRequests, taskTokens, taskCost, RAGDocumentAILimits{
		MaxRequests: taskBudget.MaxRequests, MaxTokens: taskBudget.MaxTokens,
		MaxCostMicroUSD: taskBudget.MaxCostMicroUSD,
	}) {
		return false, ErrRAGDocumentAIBudgetExceeded
	}
	if err := d.updateRAGDocumentAIChargesTx(ctx, tx.exec, userBudget, taskBudget,
		1, reservedTokens, usage.EstimatedCostMicroUSD); err != nil {
		return false, err
	}
	if err := d.insertRAGDocumentAIUsageTx(ctx, tx.exec, usage); err != nil {
		tx.Rollback()
		existing, lookupErr := d.GetRAGDocumentAIUsage(ctx, usage.IdempotencyKey)
		if lookupErr == nil {
			if ragDocumentAIReservationsEqual(existing, usage) {
				return false, nil
			}
			return false, ErrRAGDocumentAIUsageConflict
		}
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) updateRAGDocumentAIUsageStateTx(
	ctx context.Context,
	tx ragExecutor,
	idempotencyKey, expectedState, state string,
) (bool, error) {
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_ai_usage SET
		state=%s,updated_at=%s WHERE idempotency_key=%s AND state=%s`, d.ph(1),
		d.ragNowExpr(), d.ph(2), d.ph(3)), state, idempotencyKey, expectedState)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) loadRAGDocumentAIBudgetLocks(
	ctx context.Context,
	tx *ragDocumentAIBudgetTx,
	usage *RAGDocumentAIUsageRecord,
) (*RAGDocumentAIUserBudgetRecord, *RAGDocumentAITaskBudgetRecord, error) {
	userBudget, err := d.lockRAGDocumentAIUserBudgetTx(ctx, tx.exec, usage.UserID, usage.PeriodStartUTC)
	if err != nil {
		if ragIsNoRows(err) {
			return nil, nil, fmt.Errorf("%w: missing user aggregate", ErrRAGDocumentAILedgerCorrupt)
		}
		return nil, nil, err
	}
	taskBudget, err := d.lockRAGDocumentAITaskBudgetTx(ctx, tx.exec, usage.TaskID)
	if err != nil {
		if ragIsNoRows(err) {
			return nil, nil, fmt.Errorf("%w: missing task aggregate", ErrRAGDocumentAILedgerCorrupt)
		}
		return nil, nil, err
	}
	if taskBudget.UserID != usage.UserID {
		return nil, nil, ErrRAGDocumentAILedgerCorrupt
	}
	return userBudget, taskBudget, nil
}

func (d *DBStore) lockRAGDocumentAIIndexTaskIfPresent(
	ctx context.Context,
	tx ragExecutor,
	taskID int64,
) error {
	_, err := d.lockRAGDocumentAIIndexTaskTx(ctx, tx, taskID)
	if ragIsNoRows(err) {
		return nil
	}
	return err
}

// MarkSentRAGDocumentAIUsage is the final gate before network I/O. true is
// returned only for the one RESERVED -> SENT transition that is authorized to
// send. If the reservation's own fence is stale, it is released and refunded.
func (d *DBStore) MarkSentRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	fence IndexFence,
) (bool, error) {
	preflight, err := d.GetRAGDocumentAIUsage(ctx, idempotencyKey)
	if err != nil {
		return false, err
	}
	if !ragDocumentAIUsageMatchesFence(preflight, fence) {
		return false, ErrRAGDocumentAIInvalidFence
	}
	tx, err := d.beginRAGDocumentAIBudgetTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	userBudget, taskBudget, err := d.loadRAGDocumentAIBudgetLocks(ctx, tx, preflight)
	if err != nil {
		return false, err
	}
	currentFence, err := d.currentRAGDocumentAIFenceTx(ctx, tx.exec, fence)
	if err != nil {
		return false, err
	}
	usage, err := d.lockRAGDocumentAIUsageTx(ctx, tx.exec, idempotencyKey)
	if err != nil {
		return false, scanErr(err)
	}
	if !ragDocumentAIUsageMatchesFence(usage, fence) {
		return false, ErrRAGDocumentAIInvalidFence
	}
	if usage.State != RAGDocumentAIUsageReserved {
		return false, nil
	}
	if !currentFence {
		reservedTokens, err := ragDocumentAITokenTotal(usage.ReservedInputTokens, usage.ReservedOutputTokens)
		if err != nil {
			return false, ErrRAGDocumentAILedgerCorrupt
		}
		if err := d.updateRAGDocumentAIChargesTx(ctx, tx.exec, userBudget, taskBudget,
			-1, -reservedTokens, -usage.EstimatedCostMicroUSD); err != nil {
			return false, err
		}
		updated, err := d.updateRAGDocumentAIUsageStateTx(ctx, tx.exec, idempotencyKey,
			RAGDocumentAIUsageReserved, RAGDocumentAIUsageReleased)
		if err != nil || !updated {
			if err != nil {
				return false, err
			}
			return false, ErrRAGDocumentAILedgerCorrupt
		}
		return false, tx.Commit()
	}

	result, err := tx.exec.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_ai_usage SET
		state='SENT',sent_at=%s,updated_at=%s WHERE idempotency_key=%s AND state='RESERVED'`,
		d.ragNowExpr(), d.ragNowExpr(), d.ph(1)), idempotencyKey)
	if err != nil {
		return false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || !updated {
		if err != nil {
			return false, err
		}
		return false, ErrRAGDocumentAILedgerCorrupt
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) releaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	requireInvalidFence bool,
) (bool, error) {
	preflight, err := d.GetRAGDocumentAIUsage(ctx, idempotencyKey)
	if err != nil {
		return false, err
	}
	tx, err := d.beginRAGDocumentAIBudgetTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	userBudget, taskBudget, err := d.loadRAGDocumentAIBudgetLocks(ctx, tx, preflight)
	if err != nil {
		return false, err
	}
	currentFence := false
	if requireInvalidFence {
		currentFence, err = d.currentRAGDocumentAIFenceTx(ctx, tx.exec, IndexFence{
			TaskID: preflight.TaskID, DocID: preflight.DocID, DocVersion: preflight.DocVersion,
			ClaimGeneration: preflight.ClaimGeneration, LeaseOwner: preflight.LeaseOwner,
		})
		if err != nil {
			return false, err
		}
	} else if err := d.lockRAGDocumentAIIndexTaskIfPresent(ctx, tx.exec, preflight.TaskID); err != nil {
		return false, err
	}
	usage, err := d.lockRAGDocumentAIUsageTx(ctx, tx.exec, idempotencyKey)
	if err != nil {
		return false, scanErr(err)
	}
	if usage.State != RAGDocumentAIUsageReserved {
		return false, nil
	}
	if requireInvalidFence && currentFence {
		return false, nil
	}
	reservedTokens, err := ragDocumentAITokenTotal(usage.ReservedInputTokens, usage.ReservedOutputTokens)
	if err != nil {
		return false, ErrRAGDocumentAILedgerCorrupt
	}
	if err := d.updateRAGDocumentAIChargesTx(ctx, tx.exec, userBudget, taskBudget,
		-1, -reservedTokens, -usage.EstimatedCostMicroUSD); err != nil {
		return false, err
	}
	updated, err := d.updateRAGDocumentAIUsageStateTx(ctx, tx.exec, idempotencyKey,
		RAGDocumentAIUsageReserved, RAGDocumentAIUsageReleased)
	if err != nil || !updated {
		if err != nil {
			return false, err
		}
		return false, ErrRAGDocumentAILedgerCorrupt
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ReleaseRAGDocumentAIUsage refunds a reservation only while it is known not
// to have crossed the SENT boundary. Explicit callers use this after proving
// that no network write occurred; the reconciler additionally requires the
// reservation's IndexFence to be stale under the same transaction locks.
func (d *DBStore) ReleaseRAGDocumentAIUsage(ctx context.Context, idempotencyKey string) (bool, error) {
	return d.releaseRAGDocumentAIUsage(ctx, idempotencyKey, false)
}

// CommitRAGDocumentAIUsage settles a SENT attempt. It intentionally does not
// validate the current IndexFence: a late response after reclaim still owns
// and must settle this idempotency key. The expected SENT state makes retries
// idempotent. estimated_cost_microusd becomes the latest actual/estimated cost.
func (d *DBStore) CommitRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	actualInputTokens, actualOutputTokens, actualCostMicroUSD int64,
	usageEstimated bool,
) (bool, error) {
	actualTokens, err := ragDocumentAITokenTotal(actualInputTokens, actualOutputTokens)
	if err != nil || actualCostMicroUSD < 0 {
		return false, errors.New("store: invalid RAG DocumentAI settlement")
	}
	preflight, err := d.GetRAGDocumentAIUsage(ctx, idempotencyKey)
	if err != nil {
		return false, err
	}
	tx, err := d.beginRAGDocumentAIBudgetTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	userBudget, taskBudget, err := d.loadRAGDocumentAIBudgetLocks(ctx, tx, preflight)
	if err != nil {
		return false, err
	}
	if err := d.lockRAGDocumentAIIndexTaskIfPresent(ctx, tx.exec, preflight.TaskID); err != nil {
		return false, err
	}
	usage, err := d.lockRAGDocumentAIUsageTx(ctx, tx.exec, idempotencyKey)
	if err != nil {
		return false, scanErr(err)
	}
	if usage.State != RAGDocumentAIUsageSent {
		return false, nil
	}
	reservedTokens, err := ragDocumentAITokenTotal(usage.ReservedInputTokens, usage.ReservedOutputTokens)
	if err != nil {
		return false, ErrRAGDocumentAILedgerCorrupt
	}
	if err := d.updateRAGDocumentAIChargesTx(ctx, tx.exec, userBudget, taskBudget,
		0, actualTokens-reservedTokens, actualCostMicroUSD-usage.EstimatedCostMicroUSD); err != nil {
		return false, err
	}
	state := RAGDocumentAIUsageCommitted
	if actualInputTokens > usage.ReservedInputTokens ||
		actualOutputTokens > usage.ReservedOutputTokens ||
		actualCostMicroUSD > usage.EstimatedCostMicroUSD {
		state = RAGDocumentAIUsageOverrun
	}
	result, err := tx.exec.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_ai_usage SET
		state=%s,actual_input_tokens=%s,actual_output_tokens=%s,estimated_cost_microusd=%s,
		usage_estimated=%s,updated_at=%s WHERE idempotency_key=%s AND state='SENT'`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ragNowExpr(), d.ph(6)),
		state, actualInputTokens, actualOutputTokens, actualCostMicroUSD,
		usageEstimated, idempotencyKey)
	if err != nil {
		return false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || !updated {
		if err != nil {
			return false, err
		}
		return false, ErrRAGDocumentAILedgerCorrupt
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ReconcileRAGDocumentAIUsage releases expired, never-sent reservations and
// conservatively settles abandoned SENT attempts. It uses the public CAS
// transitions so a late worker racing the reconciler can win at most once.
func (d *DBStore) ReconcileRAGDocumentAIUsage(
	ctx context.Context,
	reservedBefore, sentBefore time.Time,
	limit int,
) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT idempotency_key,state
		FROM rag_document_ai_usage WHERE
		(state='RESERVED' AND reservation_expires_at IS NOT NULL
		 AND reservation_expires_at <= %s AND updated_at <= %s) OR
		(state='SENT' AND sent_at IS NOT NULL AND sent_at <= %s)
		ORDER BY updated_at,idempotency_key LIMIT %s`, d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3)),
		reservedBefore.UTC(), sentBefore.UTC(), limit)
	if err != nil {
		return 0, err
	}
	candidates, err := collectRAGDocumentAIReconcileCandidates(rows)
	if err != nil {
		return 0, err
	}

	transitioned := 0
	for _, item := range candidates {
		switch item.state {
		case RAGDocumentAIUsageReserved:
			ok, err := d.releaseRAGDocumentAIUsage(ctx, item.key, true)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return transitioned, err
			}
			if ok {
				transitioned++
			}
		case RAGDocumentAIUsageSent:
			usage, err := d.GetRAGDocumentAIUsage(ctx, item.key)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return transitioned, err
			}
			ok, err := d.CommitRAGDocumentAIUsage(ctx, item.key,
				usage.ReservedInputTokens, usage.ReservedOutputTokens,
				usage.EstimatedCostMicroUSD, true)
			if err != nil {
				return transitioned, err
			}
			if ok {
				transitioned++
			}
		}
	}
	return transitioned, nil
}
