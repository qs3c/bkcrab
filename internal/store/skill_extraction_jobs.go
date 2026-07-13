package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

const skillExtractionJobColumns = `id, owner_user_id, agent_id, session_key,
	chatter_user_id, after_seq, through_seq, tool_call_count, snapshot_json,
	snapshot_sha256, status, outcome, mutation_count, slugs_json, attempt_count,
	last_error, lease_owner, lease_expires_at, next_attempt_at, created_at, updated_at, completed_at`

const qualifiedSkillExtractionJobColumns = `j.id, j.owner_user_id, j.agent_id, j.session_key,
	j.chatter_user_id, j.after_seq, j.through_seq, j.tool_call_count, j.snapshot_json,
	j.snapshot_sha256, j.status, j.outcome, j.mutation_count, j.slugs_json, j.attempt_count,
	j.last_error, j.lease_owner, j.lease_expires_at, j.next_attempt_at, j.created_at, j.updated_at, j.completed_at`

const (
	skillExtractionMaxAttempts = 5
	skillExtractionRetryBase   = time.Minute
	skillExtractionRetryMax    = 8 * time.Minute
)

func scanSkillExtractionJob(scanner interface{ Scan(dest ...any) error }) (*SkillExtractionJob, error) {
	var job SkillExtractionJob
	var snapshot, slugs string
	var leaseExpires, nextAttempt, completed sql.NullTime
	if err := scanner.Scan(
		&job.ID, &job.OwnerUserID, &job.AgentID, &job.SessionKey,
		&job.ChatterUserID, &job.AfterSeq, &job.ThroughSeq, &job.ToolCallCount,
		&snapshot, &job.SnapshotSHA256, &job.Status, &job.Outcome,
		&job.MutationCount, &slugs, &job.AttemptCount, &job.LastError,
		&job.LeaseOwner, &leaseExpires, &nextAttempt, &job.CreatedAt, &job.UpdatedAt,
		&completed,
	); err != nil {
		return nil, err
	}
	job.SnapshotJSON = json.RawMessage(append([]byte(nil), snapshot...))
	if slugs != "" {
		if err := json.Unmarshal([]byte(slugs), &job.Slugs); err != nil {
			return nil, fmt.Errorf("decode skill extraction job %s slugs: %w", job.ID, err)
		}
	}
	if leaseExpires.Valid {
		t := leaseExpires.Time
		job.LeaseExpiresAt = &t
	}
	if nextAttempt.Valid {
		t := nextAttempt.Time
		job.NextAttemptAt = &t
	}
	if completed.Valid {
		t := completed.Time
		job.CompletedAt = &t
	}
	return &job, nil
}

func (d *DBStore) getSkillExtractionJobTx(ctx context.Context, tx *sql.Tx, jobID string, lock bool) (*SkillExtractionJob, error) {
	query := `SELECT ` + skillExtractionJobColumns + ` FROM skill_extraction_jobs WHERE id=` + d.ph(1)
	if lock && (d.dialect == "postgres" || d.dialect == mysqlDialect) {
		query += ` FOR UPDATE`
	}
	job, err := scanSkillExtractionJob(tx.QueryRowContext(ctx, query, jobID))
	if err != nil {
		return nil, scanErr(err)
	}
	return job, nil
}

// EnqueueSkillExtractionJob implements the durable cadence boundary described
// by Store. It deliberately has no turn cap: a long prefix of low-count turns
// cannot starve a later threshold crossing.
func (d *DBStore) EnqueueSkillExtractionJob(ctx context.Context, ownerUserID, agentID, sessionKey, chatterUserID string, minTotal int) (*SkillExtractionJob, error) {
	if ownerUserID == "" || agentID == "" || sessionKey == "" || chatterUserID == "" {
		return nil, errors.New("store: EnqueueSkillExtractionJob requires owner_user_id, agent_id, session_key, chatter_user_id")
	}
	if minTotal <= 0 {
		return nil, errors.New("store: EnqueueSkillExtractionJob requires a positive threshold")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()

	// The sessions row is the serialization point for the snapshot boundary.
	// Lock and copy it before inspecting turn anchors so a later SaveSession
	// cannot publish messages into the window selected by this transaction.
	snapshotSQL := fmt.Sprintf(`SELECT messages FROM sessions
		WHERE user_id=%s AND agent_id=%s AND session_key=%s
		  AND COALESCE(NULLIF(chatter_user_id,''), user_id)=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		snapshotSQL += ` FOR UPDATE`
	}
	var snapshot string
	if err := tx.QueryRowContext(ctx, snapshotSQL, ownerUserID, agentID, sessionKey, chatterUserID).Scan(&snapshot); err != nil {
		return nil, scanErr(err)
	}
	if !json.Valid([]byte(snapshot)) {
		return nil, errors.New("store: sessions.messages contains invalid JSON")
	}

	insertCheckpoint := fmt.Sprintf(`INSERT INTO skill_extraction_checkpoints
		(owner_user_id, agent_id, session_key, chatter_user_id, consumed_through_seq, created_at, updated_at)
		VALUES (%s,%s,%s,%s,-1,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6))
	switch d.dialect {
	case mysqlDialect:
		insertCheckpoint = `INSERT IGNORE INTO skill_extraction_checkpoints
			(owner_user_id, agent_id, session_key, chatter_user_id, consumed_through_seq, created_at, updated_at)
			VALUES (?,?,?,?,-1,?,?)`
	case "postgres":
		insertCheckpoint += ` ON CONFLICT (owner_user_id, agent_id, session_key, chatter_user_id) DO NOTHING`
	default:
		insertCheckpoint = `INSERT OR IGNORE INTO skill_extraction_checkpoints
			(owner_user_id, agent_id, session_key, chatter_user_id, consumed_through_seq, created_at, updated_at)
			VALUES (?,?,?,?,-1,?,?)`
	}
	if _, err := tx.ExecContext(ctx, insertCheckpoint, ownerUserID, agentID, sessionKey, chatterUserID, now, now); err != nil {
		return nil, err
	}

	checkpointSQL := fmt.Sprintf(`SELECT consumed_through_seq, COALESCE(inflight_job_id,'')
		FROM skill_extraction_checkpoints
		WHERE owner_user_id=%s AND agent_id=%s AND session_key=%s AND chatter_user_id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		checkpointSQL += ` FOR UPDATE`
	}
	var consumed int64
	var inflight string
	if err := tx.QueryRowContext(ctx, checkpointSQL, ownerUserID, agentID, sessionKey, chatterUserID).Scan(&consumed, &inflight); err != nil {
		return nil, err
	}
	if inflight != "" {
		job, err := d.getSkillExtractionJobTx(ctx, tx, inflight, false)
		if err != nil {
			return nil, fmt.Errorf("load checkpoint inflight job: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return job, nil
	}

	// A sessions snapshot is a full-session view rather than a per-turn slice.
	// If any newer anchor is still running, that blob may already contain a
	// partial turn (the user message but not its answer), so defer the entire
	// cadence boundary until every observed turn is complete.
	runningSQL := fmt.Sprintf(`SELECT seq FROM session_messages
		WHERE user_id=%s AND agent_id=%s AND session_key=%s
		  AND COALESCE(NULLIF(chatter_user_id,''), user_id)=%s
		  AND turn_status='running' AND seq>%s
		ORDER BY seq LIMIT 1`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		runningSQL += ` FOR UPDATE`
	}
	var runningSeq int64
	if err := tx.QueryRowContext(ctx, runningSQL, ownerUserID, agentID, sessionKey, chatterUserID, consumed).Scan(&runningSeq); err == nil {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	aggregateSQL := fmt.Sprintf(`SELECT COALESCE(SUM(tool_call_count),0), MAX(seq)
		FROM session_messages
		WHERE user_id=%s AND agent_id=%s AND session_key=%s
		  AND COALESCE(NULLIF(chatter_user_id,''), user_id)=%s
		  AND turn_status='done' AND seq>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
	var total int
	var through sql.NullInt64
	if err := tx.QueryRowContext(ctx, aggregateSQL, ownerUserID, agentID, sessionKey, chatterUserID, consumed).Scan(&total, &through); err != nil {
		return nil, err
	}
	if !through.Valid || total < minTotal {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}

	sum := sha256.Sum256([]byte(snapshot))
	job := &SkillExtractionJob{
		ID: uuid.NewString(), OwnerUserID: ownerUserID, AgentID: agentID,
		SessionKey: sessionKey, ChatterUserID: chatterUserID,
		AfterSeq: consumed, ThroughSeq: through.Int64, ToolCallCount: total,
		SnapshotJSON:   json.RawMessage(append([]byte(nil), snapshot...)),
		SnapshotSHA256: hex.EncodeToString(sum[:]), Status: "pending",
		Slugs: []string{}, CreatedAt: now, UpdatedAt: now,
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO skill_extraction_jobs
		(id, owner_user_id, agent_id, session_key, chatter_user_id, after_seq, through_seq,
		 tool_call_count, snapshot_json, snapshot_sha256, status, outcome, mutation_count,
		 slugs_json, attempt_count, last_error, lease_owner, created_at, updated_at)
		VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,'',0,'[]',0,'','',%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8),
		d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)),
		job.ID, ownerUserID, agentID, sessionKey, chatterUserID, consumed, through.Int64,
		total, snapshot, job.SnapshotSHA256, job.Status, now, now); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE session_messages SET skill_extraction_id=%s
		WHERE user_id=%s AND agent_id=%s AND session_key=%s AND seq=%s
		  AND COALESCE(NULLIF(chatter_user_id,''), user_id)=%s
		  AND turn_status='done' AND skill_extraction_id IS NULL`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		job.ID, ownerUserID, agentID, sessionKey, through.Int64, chatterUserID)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("store: newest skill cadence anchor was already claimed")
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_checkpoints
		SET inflight_job_id=%s, inflight_through_seq=%s, updated_at=%s
		WHERE owner_user_id=%s AND agent_id=%s AND session_key=%s AND chatter_user_id=%s
		  AND inflight_job_id IS NULL`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		job.ID, through.Int64, now, ownerUserID, agentID, sessionKey, chatterUserID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func skillExtractionRetryDelay(attemptCount int) time.Duration {
	if attemptCount < 1 {
		attemptCount = 1
	}
	delay := skillExtractionRetryBase
	for i := 1; i < attemptCount && delay < skillExtractionRetryMax; i++ {
		delay *= 2
	}
	if delay > skillExtractionRetryMax {
		return skillExtractionRetryMax
	}
	return delay
}

func (d *DBStore) failSkillExtractionJobTx(ctx context.Context, tx *sql.Tx, job *SkillExtractionJob, lastError, outcome string, now time.Time) error {
	if outcome == "" {
		outcome = "failed"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_jobs
		SET status='failed', outcome=%s, mutation_count=0, slugs_json='[]',
		    snapshot_json='[]', last_error=%s, lease_owner='', lease_expires_at=NULL,
		    next_attempt_at=NULL, updated_at=%s, completed_at=%s WHERE id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), outcome, lastError, now, now, job.ID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_checkpoints
		SET consumed_through_seq=CASE WHEN consumed_through_seq < %s THEN %s ELSE consumed_through_seq END,
		    inflight_job_id=NULL, inflight_through_seq=NULL, updated_at=%s
		WHERE owner_user_id=%s AND agent_id=%s AND session_key=%s AND chatter_user_id=%s
		  AND inflight_job_id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
		job.ThroughSeq, job.ThroughSeq, now, job.OwnerUserID, job.AgentID, job.SessionKey, job.ChatterUserID, job.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return errors.New("store: skill extraction checkpoint no longer owns job")
	}
	job.Status = "failed"
	job.Outcome = outcome
	job.MutationCount = 0
	job.Slugs = []string{}
	job.SnapshotJSON = json.RawMessage("[]")
	job.LastError = lastError
	job.LeaseOwner = ""
	job.LeaseExpiresAt = nil
	job.NextAttemptAt = nil
	job.UpdatedAt = now
	job.CompletedAt = &now
	return nil
}

func (d *DBStore) AcquireSkillExtractionJob(ctx context.Context, jobID, workerID string, leaseDuration time.Duration) (*SkillExtractionJob, bool, error) {
	if jobID == "" || workerID == "" || leaseDuration <= 0 {
		return nil, false, errors.New("store: AcquireSkillExtractionJob requires job_id, worker_id, and a positive lease")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	pendingReady := job.Status == "pending" && (job.NextAttemptAt == nil || !job.NextAttemptAt.After(now))
	expiredRunning := job.Status == "running" && (job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now))
	eligible := pendingReady || expiredRunning
	if !eligible {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return job, false, nil
	}
	_, receiptErr := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	hasMutationReceipt := receiptErr == nil
	if receiptErr != nil && !errors.Is(receiptErr, ErrNotFound) {
		return nil, false, receiptErr
	}
	if job.AttemptCount >= skillExtractionMaxAttempts && !hasMutationReceipt {
		lastError := job.LastError
		if lastError == "" {
			lastError = "maximum skill extraction attempts exhausted"
		}
		if expiredRunning {
			lastError = fmt.Sprintf("%s (lease expired after attempt %d)", lastError, job.AttemptCount)
		}
		if err := d.failSkillExtractionJobTx(ctx, tx, job, lastError, "retry_exhausted", now); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return job, false, nil
	}
	expires := now.Add(leaseDuration)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_jobs
		SET status='running', lease_owner=%s, lease_expires_at=%s,
		    next_attempt_at=NULL, attempt_count=attempt_count+1, updated_at=%s
		WHERE id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), workerID, expires, now, jobID); err != nil {
		return nil, false, err
	}
	job.Status = "running"
	job.LeaseOwner = workerID
	job.LeaseExpiresAt = &expires
	job.NextAttemptAt = nil
	job.AttemptCount++
	job.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return job, true, nil
}

func (d *DBStore) CompleteSkillExtractionJob(ctx context.Context, jobID, workerID, outcome string, mutationCount int, slugs []string) error {
	if jobID == "" || workerID == "" || outcome == "" || mutationCount < 0 {
		return errors.New("store: CompleteSkillExtractionJob requires job_id, worker_id, outcome, and non-negative mutation_count")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return err
	}
	if job.Status == "completed" {
		return tx.Commit()
	}
	if job.Status != "running" || job.LeaseOwner != workerID {
		return errors.New("store: skill extraction job is not leased by this worker")
	}
	receipt, receiptErr := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	switch {
	case receiptErr == nil && receipt.Status == SkillExtractionMutationApplied:
		if outcome != receipt.Action || mutationCount != 1 || len(slugs) != 1 || slugs[0] != receipt.Slug {
			return fmt.Errorf("store: completed mutation result must match applied receipt %s %q", receipt.Action, receipt.Slug)
		}
		// Reassign from the durable receipt so the persisted result never trusts
		// an in-memory model/tool summary.
		outcome, mutationCount, slugs = receipt.Action, 1, []string{receipt.Slug}
	case receiptErr == nil && receipt.Status == SkillExtractionMutationPrepared:
		return errors.New("store: prepared skill extraction mutation must be reconciled before job completion")
	case receiptErr == nil && receipt.Status == SkillExtractionMutationConflict:
		return errors.New("store: conflicted skill extraction mutation must fail rather than complete")
	case receiptErr != nil && !errors.Is(receiptErr, ErrNotFound):
		return receiptErr
	case errors.Is(receiptErr, ErrNotFound):
		if mutationCount != 0 || len(slugs) != 0 || outcome == "create" || outcome == "update" {
			return errors.New("store: mutation result requires an applied skill extraction receipt")
		}
	}
	if slugs == nil {
		slugs = []string{}
	}
	slugsJSON, err := json.Marshal(slugs)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_jobs
		SET status='completed', outcome=%s, mutation_count=%s, slugs_json=%s,
		    snapshot_json='[]', lease_owner='', lease_expires_at=NULL, next_attempt_at=NULL,
		    last_error='', updated_at=%s, completed_at=%s
		WHERE id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		outcome, mutationCount, string(slugsJSON), now, now, jobID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_checkpoints
		SET consumed_through_seq=CASE WHEN consumed_through_seq < %s THEN %s ELSE consumed_through_seq END,
		    inflight_job_id=NULL, inflight_through_seq=NULL, updated_at=%s
		WHERE owner_user_id=%s AND agent_id=%s AND session_key=%s AND chatter_user_id=%s
		  AND inflight_job_id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
		job.ThroughSeq, job.ThroughSeq, now, job.OwnerUserID, job.AgentID, job.SessionKey, job.ChatterUserID, job.ID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return errors.New("store: skill extraction checkpoint no longer owns job")
	}
	return tx.Commit()
}

func (d *DBStore) retrySkillExtractionJob(ctx context.Context, jobID, workerID, lastError string) error {
	if jobID == "" || workerID == "" {
		return errors.New("store: releasing skill extraction job requires job_id and worker_id")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return err
	}
	if job.Status == "completed" || job.Status == "failed" {
		return tx.Commit()
	}
	if job.Status == "pending" {
		return tx.Commit()
	}
	if job.Status != "running" || job.LeaseOwner != workerID {
		return errors.New("store: skill extraction job is not leased by this worker")
	}
	now := time.Now().UTC()
	_, receiptErr := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	hasMutationReceipt := receiptErr == nil
	if receiptErr != nil && !errors.Is(receiptErr, ErrNotFound) {
		return receiptErr
	}
	if job.AttemptCount >= skillExtractionMaxAttempts && !hasMutationReceipt {
		if err := d.failSkillExtractionJobTx(ctx, tx, job, lastError, "retry_exhausted", now); err != nil {
			return err
		}
		return tx.Commit()
	}
	nextAttempt := now.Add(skillExtractionRetryDelay(job.AttemptCount))
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_jobs
		SET status=%s, last_error=%s, lease_owner='', lease_expires_at=NULL,
		    next_attempt_at=%s, updated_at=%s
		WHERE id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		"pending", lastError, nextAttempt, now, jobID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) RetrySkillExtractionJob(ctx context.Context, jobID, workerID, lastError string) error {
	return d.retrySkillExtractionJob(ctx, jobID, workerID, lastError)
}

func (d *DBStore) FailSkillExtractionJob(ctx context.Context, jobID, workerID, lastError string) error {
	if jobID == "" || workerID == "" {
		return errors.New("store: failing skill extraction job requires job_id and worker_id")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return err
	}
	if job.Status == "failed" || job.Status == "completed" {
		return tx.Commit()
	}
	if job.Status != "running" || job.LeaseOwner != workerID {
		return errors.New("store: skill extraction job is not leased by this worker")
	}
	receipt, receiptErr := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	if receiptErr == nil {
		switch receipt.Status {
		case SkillExtractionMutationConflict:
			// Reconciliation already made the fail-closed terminal decision.
		case SkillExtractionMutationPrepared:
			return errors.New("store: prepared skill extraction mutation must be reconciled rather than failed")
		case SkillExtractionMutationApplied:
			return errors.New("store: applied skill extraction mutation must complete rather than fail")
		default:
			return fmt.Errorf("store: skill extraction mutation has unknown status %q", receipt.Status)
		}
	} else if !errors.Is(receiptErr, ErrNotFound) {
		return receiptErr
	}
	now := time.Now().UTC()
	if err := d.failSkillExtractionJobTx(ctx, tx, job, lastError, "failed", now); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ListRecoverableSkillExtractionJobs(ctx context.Context, agentID string, limit int) ([]SkillExtractionJob, error) {
	if agentID == "" {
		return nil, errors.New("store: ListRecoverableSkillExtractionJobs requires agent_id")
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	now := time.Now().UTC()
	query := fmt.Sprintf(`SELECT `+qualifiedSkillExtractionJobColumns+`
		FROM skill_extraction_jobs j
		JOIN skill_extraction_checkpoints c
		  ON c.owner_user_id=j.owner_user_id AND c.agent_id=j.agent_id
		 AND c.session_key=j.session_key AND c.chatter_user_id=j.chatter_user_id
		 AND c.inflight_job_id=j.id
		WHERE j.agent_id=%s AND (
		  (j.status='pending' AND (j.next_attempt_at IS NULL OR j.next_attempt_at<=%s)) OR
		  (j.status='running' AND (j.lease_expires_at IS NULL OR j.lease_expires_at<=%s))
		)
		ORDER BY j.created_at, j.id LIMIT %d`, d.ph(1), d.ph(2), d.ph(3), limit)
	rows, err := d.db.QueryContext(ctx, query, agentID, now, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]SkillExtractionJob, 0)
	for rows.Next() {
		job, err := scanSkillExtractionJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, *job)
	}
	return jobs, rows.Err()
}
