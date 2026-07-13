package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const skillExtractionMutationColumns = `job_id, agent_id, action, slug,
	before_hash, after_hash, desired_content, status, last_error,
	created_at, updated_at, resolved_at`

func scanSkillExtractionMutation(scanner interface{ Scan(dest ...any) error }) (*SkillExtractionMutationReceipt, error) {
	var receipt SkillExtractionMutationReceipt
	var resolved sql.NullTime
	if err := scanner.Scan(
		&receipt.JobID, &receipt.AgentID, &receipt.Action, &receipt.Slug,
		&receipt.BeforeHash, &receipt.AfterHash, &receipt.DesiredContent,
		&receipt.Status, &receipt.LastError, &receipt.CreatedAt,
		&receipt.UpdatedAt, &resolved,
	); err != nil {
		return nil, err
	}
	if resolved.Valid {
		t := resolved.Time
		receipt.ResolvedAt = &t
	}
	return &receipt, nil
}

func (d *DBStore) getSkillExtractionMutationTx(ctx context.Context, tx *sql.Tx, jobID string, lock bool) (*SkillExtractionMutationReceipt, error) {
	query := `SELECT ` + skillExtractionMutationColumns + ` FROM skill_extraction_mutations WHERE job_id=` + d.ph(1)
	if lock && (d.dialect == "postgres" || d.dialect == mysqlDialect) {
		query += ` FOR UPDATE`
	}
	receipt, err := scanSkillExtractionMutation(tx.QueryRowContext(ctx, query, jobID))
	if err != nil {
		return nil, scanErr(err)
	}
	return receipt, nil
}

// GetSkillExtractionMutation returns the outbox/receipt for a cadence job.
func (d *DBStore) GetSkillExtractionMutation(ctx context.Context, jobID string) (*SkillExtractionMutationReceipt, error) {
	if strings.TrimSpace(jobID) == "" {
		return nil, errors.New("store: GetSkillExtractionMutation requires job_id")
	}
	receipt, err := scanSkillExtractionMutation(d.db.QueryRowContext(ctx,
		`SELECT `+skillExtractionMutationColumns+` FROM skill_extraction_mutations WHERE job_id=`+d.ph(1), jobID))
	if err != nil {
		return nil, scanErr(err)
	}
	return receipt, nil
}

func normalizeSkillExtractionMutationIntent(intent SkillExtractionMutationIntent) (SkillExtractionMutationIntent, error) {
	intent.Action = strings.ToLower(strings.TrimSpace(intent.Action))
	intent.Slug = strings.TrimSpace(intent.Slug)
	intent.BeforeHash = strings.ToLower(strings.TrimSpace(intent.BeforeHash))
	intent.AfterHash = strings.ToLower(strings.TrimSpace(intent.AfterHash))
	if intent.Action != "create" && intent.Action != "update" {
		return intent, fmt.Errorf("store: skill extraction mutation action %q is not create or update", intent.Action)
	}
	if intent.Slug == "" || strings.TrimSpace(intent.DesiredContent) == "" {
		return intent, errors.New("store: skill extraction mutation requires slug and desired_content")
	}
	if intent.Action == "create" && intent.BeforeHash != "" {
		return intent, errors.New("store: create mutation must have an empty before_hash")
	}
	if intent.Action == "update" {
		if !validSkillContentHash(intent.BeforeHash) {
			return intent, errors.New("store: update mutation requires a 64-character hexadecimal before_hash")
		}
	}
	if !validSkillContentHash(intent.AfterHash) {
		return intent, errors.New("store: skill extraction mutation requires a 64-character hexadecimal after_hash")
	}
	if got := HashSkillContent(intent.DesiredContent); got != intent.AfterHash {
		return intent, fmt.Errorf("store: desired_content hash %s does not match after_hash %s", got, intent.AfterHash)
	}
	if intent.Action == "update" && intent.BeforeHash == intent.AfterHash {
		return intent, errors.New("store: no-op update mutation is not allowed")
	}
	return intent, nil
}

func validSkillContentHash(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, ch := range hash {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func sameSkillExtractionMutationIntent(receipt *SkillExtractionMutationReceipt, intent SkillExtractionMutationIntent) bool {
	return receipt != nil && receipt.Action == intent.Action && receipt.Slug == intent.Slug &&
		strings.EqualFold(receipt.BeforeHash, intent.BeforeHash) &&
		strings.EqualFold(receipt.AfterHash, intent.AfterHash)
}

func validateSkillExtractionMutationJob(job *SkillExtractionJob, workerID string, now time.Time) error {
	if job == nil {
		return ErrNotFound
	}
	if strings.TrimSpace(workerID) == "" {
		return errors.New("store: skill extraction mutation requires worker_id")
	}
	if job.Status != "running" || job.LeaseOwner != workerID {
		return fmt.Errorf("store: skill extraction job %q is not leased by worker %q", job.ID, workerID)
	}
	if job.LeaseExpiresAt == nil || !job.LeaseExpiresAt.After(now) {
		return fmt.Errorf("store: skill extraction job %q lease has expired", job.ID)
	}
	return nil
}

func (d *DBStore) ensureSkillExtractionMutationAgentActiveTx(ctx context.Context, tx *sql.Tx, agentID string) error {
	var deleting int
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM agent_deletions WHERE agent_id=%s`, d.ph(1)),
		agentID).Scan(&deleting); err != nil {
		return fmt.Errorf("check learner agent deletion marker: %w", err)
	}
	if deleting > 0 {
		return fmt.Errorf("store: agent %q is being deleted; mutation intent refused", agentID)
	}
	return nil
}

// PrepareSkillExtractionMutation establishes the durable intent before any
// non-transactional learner asset is written. Once this succeeds, recovery
// must reconcile this receipt and must never ask the model to choose again.
func (d *DBStore) PrepareSkillExtractionMutation(ctx context.Context, jobID, workerID string, rawIntent SkillExtractionMutationIntent) (*SkillExtractionMutationReceipt, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, errors.New("store: PrepareSkillExtractionMutation requires job_id")
	}
	intent, err := normalizeSkillExtractionMutationIntent(rawIntent)
	if err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return nil, err
	}
	if err := validateSkillExtractionMutationJob(job, workerID, now); err != nil {
		return nil, err
	}
	if err := d.ensureSkillExtractionMutationAgentActiveTx(ctx, tx, job.AgentID); err != nil {
		return nil, err
	}

	existing, err := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	if err == nil {
		if !sameSkillExtractionMutationIntent(existing, intent) {
			return nil, fmt.Errorf("store: skill extraction job %q already prepared a different mutation (%s %q)", jobID, existing.Action, existing.Slug)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_jobs
			SET snapshot_json='[]', updated_at=%s WHERE id=%s`, d.ph(1), d.ph(2)), now, jobID); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO skill_extraction_mutations
		(job_id, agent_id, action, slug, before_hash, after_hash, desired_content,
		 status, last_error, created_at, updated_at)
		VALUES (%s,%s,%s,%s,%s,%s,%s,%s,'',%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)),
		jobID, job.AgentID, intent.Action, intent.Slug, intent.BeforeHash,
		intent.AfterHash, intent.DesiredContent, SkillExtractionMutationPrepared,
		now, now); err != nil {
		return nil, err
	}
	// The original conversation snapshot is no longer needed after the model
	// has chosen an immutable intent. Scrubbing it also ensures recovery cannot
	// accidentally feed the same snapshot to the model again.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_jobs
		SET snapshot_json='[]', updated_at=%s WHERE id=%s`, d.ph(1), d.ph(2)), now, jobID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &SkillExtractionMutationReceipt{
		JobID: jobID, AgentID: job.AgentID, Action: intent.Action, Slug: intent.Slug,
		BeforeHash: intent.BeforeHash, AfterHash: intent.AfterHash,
		DesiredContent: intent.DesiredContent, Status: SkillExtractionMutationPrepared,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// CommitSkillExtractionMutation closes the relational half of the outbox. The
// caller must already have reconciled the authoritative asset to AfterHash.
func (d *DBStore) CommitSkillExtractionMutation(ctx context.Context, jobID, workerID string) (*SkillExtractionMutationReceipt, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, errors.New("store: CommitSkillExtractionMutation requires job_id")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return nil, err
	}
	if err := validateSkillExtractionMutationJob(job, workerID, now); err != nil {
		return nil, err
	}
	if err := d.ensureSkillExtractionMutationAgentActiveTx(ctx, tx, job.AgentID); err != nil {
		return nil, err
	}
	receipt, err := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	if err != nil {
		return nil, err
	}
	switch receipt.Status {
	case SkillExtractionMutationApplied:
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return receipt, nil
	case SkillExtractionMutationPrepared:
		// Continue below.
	case SkillExtractionMutationConflict:
		return nil, fmt.Errorf("store: skill extraction mutation %q is already in conflict: %s", jobID, receipt.LastError)
	default:
		return nil, fmt.Errorf("store: skill extraction mutation %q has unknown status %q", jobID, receipt.Status)
	}
	if err := d.upsertSkillUsageTx(ctx, tx, receipt.AgentID, receipt.Slug, receipt.AfterHash, receipt.Action == "create"); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_mutations
		SET status=%s, desired_content='', last_error='', updated_at=%s, resolved_at=%s
		WHERE job_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		SkillExtractionMutationApplied, now, now, jobID, SkillExtractionMutationPrepared)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store: skill extraction mutation %q was not prepared", jobID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	receipt.Status = SkillExtractionMutationApplied
	receipt.DesiredContent = ""
	receipt.LastError = ""
	receipt.UpdatedAt = now
	receipt.ResolvedAt = &now
	return receipt, nil
}

// ConflictSkillExtractionMutation records a fail-closed reconciliation result.
// It intentionally leaves skill_usage untouched: a divergent current asset is
// evidence that the prepared intent cannot be declared applied.
func (d *DBStore) ConflictSkillExtractionMutation(ctx context.Context, jobID, workerID, reason string) (*SkillExtractionMutationReceipt, error) {
	jobID = strings.TrimSpace(jobID)
	reason = strings.TrimSpace(reason)
	if jobID == "" || reason == "" {
		return nil, errors.New("store: ConflictSkillExtractionMutation requires job_id and reason")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	job, err := d.getSkillExtractionJobTx(ctx, tx, jobID, true)
	if err != nil {
		return nil, err
	}
	if err := validateSkillExtractionMutationJob(job, workerID, now); err != nil {
		return nil, err
	}
	receipt, err := d.getSkillExtractionMutationTx(ctx, tx, jobID, true)
	if err != nil {
		return nil, err
	}
	switch receipt.Status {
	case SkillExtractionMutationConflict:
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return receipt, nil
	case SkillExtractionMutationApplied:
		return nil, fmt.Errorf("store: applied skill extraction mutation %q cannot be marked conflict", jobID)
	case SkillExtractionMutationPrepared:
		// Continue below.
	default:
		return nil, fmt.Errorf("store: skill extraction mutation %q has unknown status %q", jobID, receipt.Status)
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE skill_extraction_mutations
		SET status=%s, desired_content='', last_error=%s, updated_at=%s, resolved_at=%s
		WHERE job_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		SkillExtractionMutationConflict, reason, now, now, jobID, SkillExtractionMutationPrepared)
	if err != nil {
		return nil, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("store: skill extraction mutation %q was not prepared", jobID)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	receipt.Status = SkillExtractionMutationConflict
	receipt.DesiredContent = ""
	receipt.LastError = reason
	receipt.UpdatedAt = now
	receipt.ResolvedAt = &now
	return receipt, nil
}
