package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	ParserEvalRunDraft     = "DRAFT"
	ParserEvalRunQueued    = "QUEUED"
	ParserEvalRunRunning   = "RUNNING"
	ParserEvalRunSucceeded = "SUCCEEDED"
	ParserEvalRunPartial   = "PARTIAL"
	ParserEvalRunFailed    = "FAILED"
	ParserEvalRunCancelled = "CANCELLED"

	ParserEvalRunStageUploading   = "UPLOADING"
	ParserEvalRunStageValidating  = "VALIDATING"
	ParserEvalRunStageRendering   = "RENDERING"
	ParserEvalRunStageParsing     = "PARSING"
	ParserEvalRunStageScoring     = "SCORING"
	ParserEvalRunStageAggregating = "AGGREGATING"

	ParserEvalDocumentUploaded  = "UPLOADED"
	ParserEvalDocumentRunning   = "RUNNING"
	ParserEvalDocumentSucceeded = "SUCCEEDED"
	ParserEvalDocumentPartial   = "PARTIAL"
	ParserEvalDocumentFailed    = "FAILED"

	ParserEvalDocumentStageValidating = "VALIDATING"
	ParserEvalDocumentStageRendering  = "RENDERING"
	ParserEvalDocumentStageParsing    = "PARSING"
	ParserEvalDocumentStageScoring    = "SCORING"
)

var (
	ErrParserEvalImmutable  = errors.New("store: parser evaluation record is immutable")
	ErrParserEvalLimit      = errors.New("store: parser evaluation limit exceeded")
	ErrParserEvalIncomplete = errors.New("store: parser evaluation upload is incomplete")
	ErrParserEvalActive     = errors.New("store: parser evaluation run is active")
)

type ParserEvalRunRecord struct {
	ID                    string       `json:"id"`
	Status                string       `json:"status"`
	Stage                 string       `json:"stage"`
	ProgressJSON          string       `json:"progressJson"`
	ExecutionSnapshotJSON string       `json:"executionSnapshotJson"`
	SummaryJSON           string       `json:"summaryJson"`
	CreatedBy             string       `json:"createdBy"`
	ErrorCode             string       `json:"errorCode,omitempty"`
	ErrorMessage          string       `json:"errorMessage,omitempty"`
	CreatedAt             time.Time    `json:"createdAt"`
	UpdatedAt             time.Time    `json:"updatedAt"`
	StartedAt             sql.NullTime `json:"startedAt,omitempty"`
	FinishedAt            sql.NullTime `json:"finishedAt,omitempty"`
	ExpiresAt             time.Time    `json:"expiresAt"`
	LeaseOwner            string       `json:"-"`
	LeaseUntil            sql.NullTime `json:"-"`
	FenceToken            int64        `json:"-"`
	CancelRequestedAt     sql.NullTime `json:"cancelRequestedAt,omitempty"`
}

type ParserEvalDocumentRecord struct {
	ID                   string    `json:"id"`
	RunID                string    `json:"runId"`
	Ordinal              int64     `json:"ordinal"`
	FileName             string    `json:"fileName"`
	Format               string    `json:"format"`
	MediaType            string    `json:"mediaType"`
	SizeBytes            int64     `json:"sizeBytes"`
	SHA256               string    `json:"sha256"`
	SourceObjectKey      string    `json:"sourceObjectKey"`
	Status               string    `json:"status"`
	Stage                string    `json:"stage"`
	TruthJSON            string    `json:"truthJson"`
	MarkItDownResultJSON string    `json:"markitdownResultJson"`
	AnyDocResultJSON     string    `json:"anydocResultJson"`
	JudgeResultJSON      string    `json:"judgeResultJson"`
	ErrorCode            string    `json:"errorCode,omitempty"`
	ErrorMessage         string    `json:"errorMessage,omitempty"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type ParserEvalLease struct {
	RunID      string
	LeaseOwner string
	FenceToken int64
}

type ParserEvalDocumentUpdate struct {
	DocumentID           string
	Status               string
	Stage                string
	TruthJSON            string
	MarkItDownResultJSON string
	AnyDocResultJSON     string
	JudgeResultJSON      string
	ErrorCode            string
	ErrorMessage         string
}

type ParserEvalRunFinish struct {
	Status       string
	Stage        string
	ProgressJSON string
	SummaryJSON  string
	ErrorCode    string
	ErrorMessage string
}

const parserEvalRunColumns = `id,status,stage,progress_json,execution_snapshot_json,summary_json,created_by,error_code,error_message,created_at,updated_at,started_at,finished_at,expires_at,lease_owner,lease_until,fence_token,cancel_requested_at`
const parserEvalDocumentColumns = `id,run_id,ordinal,file_name,format,media_type,size_bytes,sha256,source_object_key,status,stage,truth_json,markitdown_result_json,anydoc_result_json,judge_result_json,error_code,error_message,created_at,updated_at`

var parserEvalSHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func validParserEvalRunStatus(value string) bool {
	switch value {
	case ParserEvalRunDraft, ParserEvalRunQueued, ParserEvalRunRunning, ParserEvalRunSucceeded, ParserEvalRunPartial, ParserEvalRunFailed, ParserEvalRunCancelled:
		return true
	default:
		return false
	}
}

func parserEvalRunTerminal(value string) bool {
	return value == ParserEvalRunSucceeded || value == ParserEvalRunPartial || value == ParserEvalRunFailed || value == ParserEvalRunCancelled
}

func validParserEvalRunStage(value string) bool {
	switch value {
	case ParserEvalRunStageUploading, ParserEvalRunStageValidating, ParserEvalRunStageRendering, ParserEvalRunStageParsing, ParserEvalRunStageScoring, ParserEvalRunStageAggregating:
		return true
	default:
		return false
	}
}

func validParserEvalDocumentStatus(value string) bool {
	switch value {
	case ParserEvalDocumentUploaded, ParserEvalDocumentRunning, ParserEvalDocumentSucceeded, ParserEvalDocumentPartial, ParserEvalDocumentFailed:
		return true
	default:
		return false
	}
}

func validParserEvalDocumentStage(value string) bool {
	switch value {
	case ParserEvalDocumentStageValidating, ParserEvalDocumentStageRendering, ParserEvalDocumentStageParsing, ParserEvalDocumentStageScoring:
		return true
	default:
		return false
	}
}

func validParserEvalJSON(values ...string) bool {
	for _, value := range values {
		if value == "" || !json.Valid([]byte(value)) {
			return false
		}
	}
	return true
}

func scanParserEvalRun(scanner interface{ Scan(...any) error }) (*ParserEvalRunRecord, error) {
	var record ParserEvalRunRecord
	err := scanner.Scan(
		&record.ID, &record.Status, &record.Stage, &record.ProgressJSON, &record.ExecutionSnapshotJSON,
		&record.SummaryJSON, &record.CreatedBy, &record.ErrorCode, &record.ErrorMessage, &record.CreatedAt,
		&record.UpdatedAt, &record.StartedAt, &record.FinishedAt, &record.ExpiresAt, &record.LeaseOwner,
		&record.LeaseUntil, &record.FenceToken, &record.CancelRequestedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validParserEvalRunStatus(record.Status) || !validParserEvalRunStage(record.Stage) ||
		!validParserEvalJSON(record.ProgressJSON, record.ExecutionSnapshotJSON, record.SummaryJSON) {
		return nil, errors.New("store: invalid parser evaluation run row")
	}
	return &record, nil
}

func scanParserEvalDocument(scanner interface{ Scan(...any) error }) (*ParserEvalDocumentRecord, error) {
	var record ParserEvalDocumentRecord
	err := scanner.Scan(
		&record.ID, &record.RunID, &record.Ordinal, &record.FileName, &record.Format, &record.MediaType,
		&record.SizeBytes, &record.SHA256, &record.SourceObjectKey, &record.Status, &record.Stage,
		&record.TruthJSON, &record.MarkItDownResultJSON, &record.AnyDocResultJSON, &record.JudgeResultJSON,
		&record.ErrorCode, &record.ErrorMessage, &record.CreatedAt, &record.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validParserEvalDocumentStatus(record.Status) || !validParserEvalDocumentStage(record.Stage) ||
		!validParserEvalJSON(record.TruthJSON, record.MarkItDownResultJSON, record.AnyDocResultJSON, record.JudgeResultJSON) {
		return nil, errors.New("store: invalid parser evaluation document row")
	}
	return &record, nil
}

func (d *DBStore) CreateParserEvalRun(ctx context.Context, record *ParserEvalRunRecord) error {
	if record == nil || strings.TrimSpace(record.ID) == "" || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("parser evaluation run identity is required")
	}
	if record.Status == "" {
		record.Status = ParserEvalRunDraft
	}
	if record.Stage == "" {
		record.Stage = ParserEvalRunStageUploading
	}
	if !validParserEvalRunStatus(record.Status) || record.Status != ParserEvalRunDraft || !validParserEvalRunStage(record.Stage) {
		return errors.New("new parser evaluation run must be a DRAFT")
	}
	if record.ProgressJSON == "" {
		record.ProgressJSON = `{}`
	}
	if record.ExecutionSnapshotJSON == "" {
		record.ExecutionSnapshotJSON = `{}`
	}
	if record.SummaryJSON == "" {
		record.SummaryJSON = `{}`
	}
	if !validParserEvalJSON(record.ProgressJSON, record.ExecutionSnapshotJSON, record.SummaryJSON) {
		return errors.New("parser evaluation run JSON is invalid")
	}
	now := time.Now().UTC()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = now
	} else {
		record.CreatedAt = record.CreatedAt.UTC()
	}
	if record.UpdatedAt.IsZero() {
		record.UpdatedAt = record.CreatedAt
	}
	if record.ExpiresAt.IsZero() {
		record.ExpiresAt = record.CreatedAt.Add(90 * 24 * time.Hour)
	}
	record.ErrorCode, record.ErrorMessage = sanitizeRAGEvalError(record.ErrorCode, record.ErrorMessage)
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO parser_eval_runs(
		id,status,stage,progress_json,execution_snapshot_json,summary_json,created_by,error_code,error_message,
		created_at,updated_at,expires_at,lease_owner,fence_token) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14)),
		record.ID, record.Status, record.Stage, record.ProgressJSON, record.ExecutionSnapshotJSON, record.SummaryJSON,
		record.CreatedBy, record.ErrorCode, record.ErrorMessage, record.CreatedAt, record.UpdatedAt, record.ExpiresAt, "", 0)
	return err
}

func (d *DBStore) GetParserEvalRun(ctx context.Context, id string) (*ParserEvalRunRecord, error) {
	return scanParserEvalRun(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM parser_eval_runs WHERE id=%s`, parserEvalRunColumns, d.ph(1)), id))
}

func (d *DBStore) ListParserEvalRuns(ctx context.Context, cursor string, limit int) ([]ParserEvalRunRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := fmt.Sprintf(`SELECT %s FROM parser_eval_runs`, parserEvalRunColumns)
	args := []any{}
	if strings.TrimSpace(cursor) != "" {
		var created time.Time
		if err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT created_at FROM parser_eval_runs WHERE id=%s`, d.ph(1)), cursor).Scan(&created); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		} else if err != nil {
			return nil, err
		}
		query += fmt.Sprintf(` WHERE (created_at<%s OR (created_at=%s AND id<%s))`, d.ph(1), d.ph(2), d.ph(3))
		args = append(args, created, created, cursor)
	}
	query += fmt.Sprintf(` ORDER BY created_at DESC,id DESC LIMIT %s`, d.ph(len(args)+1))
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ParserEvalRunRecord{}
	for rows.Next() {
		record, err := scanParserEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func (d *DBStore) lockParserEvalDraftRun(ctx context.Context, tx *sql.Tx, runID string) error {
	query := fmt.Sprintf(`SELECT status FROM parser_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	if err := tx.QueryRowContext(ctx, query, runID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status != ParserEvalRunDraft {
		return ErrParserEvalImmutable
	}
	return nil
}

func (d *DBStore) ReserveParserEvalDocument(ctx context.Context, record *ParserEvalDocumentRecord, maxFiles int, maxBytes int64) error {
	if record == nil || record.ID == "" || record.RunID == "" || strings.TrimSpace(record.FileName) == "" ||
		(record.Format != "docx" && record.Format != "pptx" && record.Format != "xlsx") || strings.TrimSpace(record.MediaType) == "" ||
		record.SizeBytes <= 0 || !parserEvalSHA256Pattern.MatchString(record.SHA256) || maxFiles <= 0 || maxFiles > 50 || maxBytes <= 0 {
		return errors.New("valid parser evaluation document reservation is required")
	}
	if record.Status == "" {
		record.Status = ParserEvalDocumentUploaded
	}
	if record.Stage == "" {
		record.Stage = ParserEvalDocumentStageValidating
	}
	if !validParserEvalDocumentStatus(record.Status) || !validParserEvalDocumentStage(record.Stage) {
		return errors.New("invalid parser evaluation document state")
	}
	for target, fallback := range map[*string]string{
		&record.TruthJSON:            `{"status":"PENDING"}`,
		&record.MarkItDownResultJSON: `{"status":"PENDING"}`,
		&record.AnyDocResultJSON:     `{"status":"PENDING"}`,
		&record.JudgeResultJSON:      `{"status":"PENDING"}`,
	} {
		if *target == "" {
			*target = fallback
		}
	}
	if !validParserEvalJSON(record.TruthJSON, record.MarkItDownResultJSON, record.AnyDocResultJSON, record.JudgeResultJSON) {
		return errors.New("parser evaluation document JSON is invalid")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := d.lockParserEvalDraftRun(ctx, tx, record.RunID); err != nil {
		return err
	}
	var count, total, maxOrdinal int64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*),COALESCE(SUM(size_bytes),0),COALESCE(MAX(ordinal),0) FROM parser_eval_documents WHERE run_id=%s`, d.ph(1)), record.RunID).Scan(&count, &total, &maxOrdinal); err != nil {
		return err
	}
	if count >= int64(maxFiles) || record.SizeBytes > maxBytes-total {
		return ErrParserEvalLimit
	}
	if record.Ordinal == 0 {
		record.Ordinal = maxOrdinal + 1
	}
	now := time.Now().UTC()
	record.CreatedAt, record.UpdatedAt, record.SourceObjectKey = now, now, ""
	record.ErrorCode, record.ErrorMessage = sanitizeRAGEvalError(record.ErrorCode, record.ErrorMessage)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO parser_eval_documents(
		id,run_id,ordinal,file_name,format,media_type,size_bytes,sha256,source_object_key,status,stage,truth_json,
		markitdown_result_json,anydoc_result_json,judge_result_json,error_code,error_message,created_at,updated_at)
		VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15), d.ph(16), d.ph(17), d.ph(18), d.ph(19)),
		record.ID, record.RunID, record.Ordinal, record.FileName, record.Format, record.MediaType, record.SizeBytes,
		record.SHA256, "", record.Status, record.Stage, record.TruthJSON, record.MarkItDownResultJSON,
		record.AnyDocResultJSON, record.JudgeResultJSON, record.ErrorCode, record.ErrorMessage, now, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) CompleteParserEvalDocumentUpload(ctx context.Context, runID, documentID, sourceObjectKey string) (bool, error) {
	if strings.TrimSpace(sourceObjectKey) == "" || len(sourceObjectKey) > 1024 {
		return false, errors.New("source object key is required")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_documents SET source_object_key=%s,updated_at=%s
		WHERE id=%s AND run_id=%s AND source_object_key='' AND EXISTS(SELECT 1 FROM parser_eval_runs WHERE id=%s AND status=%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), sourceObjectKey, now, documentID, runID, runID, ParserEvalRunDraft)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) AbortParserEvalDocumentUpload(ctx context.Context, runID, documentID string) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM parser_eval_documents WHERE id=%s AND run_id=%s AND source_object_key='' AND EXISTS(SELECT 1 FROM parser_eval_runs WHERE id=%s AND status=%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), documentID, runID, runID, ParserEvalRunDraft)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) DeleteParserEvalDraftDocument(ctx context.Context, runID, documentID string) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM parser_eval_documents WHERE id=%s AND run_id=%s AND EXISTS(SELECT 1 FROM parser_eval_runs WHERE id=%s AND status=%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), documentID, runID, runID, ParserEvalRunDraft)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) GetParserEvalDocument(ctx context.Context, runID, documentID string) (*ParserEvalDocumentRecord, error) {
	return scanParserEvalDocument(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM parser_eval_documents WHERE run_id=%s AND id=%s`, parserEvalDocumentColumns, d.ph(1), d.ph(2)), runID, documentID))
}

func (d *DBStore) ListParserEvalDocuments(ctx context.Context, runID string) ([]ParserEvalDocumentRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM parser_eval_documents WHERE run_id=%s ORDER BY ordinal,id`, parserEvalDocumentColumns, d.ph(1)), runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ParserEvalDocumentRecord{}
	for rows.Next() {
		record, err := scanParserEvalDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func (d *DBStore) StartParserEvalRun(ctx context.Context, id, actor, snapshotJSON, progressJSON string) (bool, error) {
	if strings.TrimSpace(actor) == "" || !validParserEvalJSON(snapshotJSON, progressJSON) {
		return false, errors.New("valid parser evaluation start payload is required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT status,created_by FROM parser_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status, createdBy string
	if err := tx.QueryRowContext(ctx, query, id).Scan(&status, &createdBy); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	} else if err != nil {
		return false, err
	}
	if status != ParserEvalRunDraft || createdBy != actor {
		return false, ErrParserEvalImmutable
	}
	var documents, incomplete int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN source_object_key='' THEN 1 ELSE 0 END),0) FROM parser_eval_documents WHERE run_id=%s`, d.ph(1)), id).Scan(&documents, &incomplete); err != nil {
		return false, err
	}
	if documents < 1 || documents > 50 || incomplete != 0 {
		return false, ErrParserEvalIncomplete
	}
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET status=%s,stage=%s,progress_json=%s,execution_snapshot_json=%s,updated_at=%s,error_code='',error_message='' WHERE id=%s AND status=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), ParserEvalRunQueued, ParserEvalRunStageValidating, progressJSON, snapshotJSON, now, id, ParserEvalRunDraft)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) ClaimParserEvalRun(ctx context.Context, worker string, now time.Time, lease time.Duration) (*ParserEvalLease, bool, error) {
	if strings.TrimSpace(worker) == "" || lease <= 0 {
		return nil, false, errors.New("worker and positive lease are required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	var runID string
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id FROM parser_eval_runs WHERE status=%s OR (status=%s AND (lease_until IS NULL OR lease_until<=%s)) ORDER BY created_at,id LIMIT 1`,
		d.ph(1), d.ph(2), d.ph(3)), ParserEvalRunQueued, ParserEvalRunRunning, now).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT status,fence_token,lease_until FROM parser_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	var fence int64
	var leaseUntil sql.NullTime
	if err := tx.QueryRowContext(ctx, query, runID).Scan(&status, &fence, &leaseUntil); err != nil {
		return nil, false, scanErr(err)
	}
	if status != ParserEvalRunQueued && !(status == ParserEvalRunRunning && (!leaseUntil.Valid || !leaseUntil.Time.After(now))) {
		return nil, false, nil
	}
	next := fence + 1
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET status=%s,lease_owner=%s,lease_until=%s,fence_token=%s,started_at=COALESCE(started_at,%s),updated_at=%s WHERE id=%s AND fence_token=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)), ParserEvalRunRunning, worker, now.Add(lease), next, now, now, runID, fence)
	if err != nil {
		return nil, false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &ParserEvalLease{RunID: runID, LeaseOwner: worker, FenceToken: next}, true, nil
}

func (d *DBStore) HeartbeatParserEvalRun(ctx context.Context, lease ParserEvalLease, now time.Time, duration time.Duration) (bool, error) {
	if duration <= 0 {
		return false, errors.New("positive lease is required")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET lease_until=%s,updated_at=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), now.Add(duration), now, lease.RunID, ParserEvalRunRunning, lease.LeaseOwner, lease.FenceToken, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) UpdateParserEvalRunProgress(ctx context.Context, lease ParserEvalLease, stage, progressJSON string, now time.Time) (bool, error) {
	if !validParserEvalRunStage(stage) || !validParserEvalJSON(progressJSON) {
		return false, errors.New("invalid parser evaluation progress")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET stage=%s,progress_json=%s,updated_at=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
		stage, progressJSON, now, lease.RunID, ParserEvalRunRunning, lease.LeaseOwner, lease.FenceToken, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func parserEvalSlotSucceeded(raw string) bool {
	var status struct {
		Status string `json:"status"`
	}
	return json.Unmarshal([]byte(raw), &status) == nil && status.Status == "SUCCEEDED"
}

func (d *DBStore) PutParserEvalDocumentResults(ctx context.Context, lease ParserEvalLease, update ParserEvalDocumentUpdate) (bool, error) {
	if update.DocumentID == "" || (update.Status != "" && !validParserEvalDocumentStatus(update.Status)) ||
		(update.Stage != "" && !validParserEvalDocumentStage(update.Stage)) {
		return false, errors.New("invalid parser evaluation document update")
	}
	for _, raw := range []string{update.TruthJSON, update.MarkItDownResultJSON, update.AnyDocResultJSON, update.JudgeResultJSON} {
		if raw != "" && !json.Valid([]byte(raw)) {
			return false, errors.New("parser evaluation result JSON is invalid")
		}
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var live int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM parser_eval_runs WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), lease.RunID, ParserEvalRunRunning, lease.LeaseOwner, lease.FenceToken, time.Now().UTC()).Scan(&live); err != nil {
		return false, err
	}
	if live != 1 {
		return false, nil
	}
	query := fmt.Sprintf(`SELECT status,stage,truth_json,markitdown_result_json,anydoc_result_json,judge_result_json FROM parser_eval_documents WHERE id=%s AND run_id=%s`, d.ph(1), d.ph(2))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status, stage, truth, markitdown, anydoc, judge string
	if err := tx.QueryRowContext(ctx, query, update.DocumentID, lease.RunID).Scan(&status, &stage, &truth, &markitdown, &anydoc, &judge); err != nil {
		return false, scanErr(err)
	}
	merge := func(current, next string) string {
		if next == "" || parserEvalSlotSucceeded(current) {
			return current
		}
		return next
	}
	truth = merge(truth, update.TruthJSON)
	markitdown = merge(markitdown, update.MarkItDownResultJSON)
	anydoc = merge(anydoc, update.AnyDocResultJSON)
	judge = merge(judge, update.JudgeResultJSON)
	if update.Status != "" {
		status = update.Status
	}
	if update.Stage != "" {
		stage = update.Stage
	}
	code, message := sanitizeRAGEvalError(update.ErrorCode, update.ErrorMessage)
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_documents SET status=%s,stage=%s,truth_json=%s,markitdown_result_json=%s,anydoc_result_json=%s,judge_result_json=%s,error_code=%s,error_message=%s,updated_at=%s WHERE id=%s AND run_id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11)),
		status, stage, truth, markitdown, anydoc, judge, code, message, time.Now().UTC(), update.DocumentID, lease.RunID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) FinishParserEvalRun(ctx context.Context, lease ParserEvalLease, finish ParserEvalRunFinish, now time.Time) (bool, error) {
	if !parserEvalRunTerminal(finish.Status) || finish.Status == ParserEvalRunCancelled && finish.ErrorCode != "" ||
		!validParserEvalRunStage(finish.Stage) || !validParserEvalJSON(finish.ProgressJSON, finish.SummaryJSON) {
		return false, errors.New("invalid parser evaluation finish")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	code, message := sanitizeRAGEvalError(finish.ErrorCode, finish.ErrorMessage)
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET status=%s,stage=%s,progress_json=%s,summary_json=%s,error_code=%s,error_message=%s,updated_at=%s,finished_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12)),
		finish.Status, finish.Stage, finish.ProgressJSON, finish.SummaryJSON, code, message, now, now,
		lease.RunID, ParserEvalRunRunning, lease.LeaseOwner, lease.FenceToken)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) RequestCancelParserEvalRun(ctx context.Context, id string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT status,cancel_requested_at FROM parser_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	var requested sql.NullTime
	if err := tx.QueryRowContext(ctx, query, id).Scan(&status, &requested); err != nil {
		return false, scanErr(err)
	}
	if status == ParserEvalRunCancelled || requested.Valid {
		return true, tx.Commit()
	}
	if parserEvalRunTerminal(status) {
		return false, ErrParserEvalImmutable
	}
	if status == ParserEvalRunDraft {
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET status=%s,cancel_requested_at=%s,finished_at=%s,updated_at=%s WHERE id=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), ParserEvalRunCancelled, now, now, now, id)
	} else {
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET cancel_requested_at=%s,updated_at=%s WHERE id=%s`, d.ph(1), d.ph(2), d.ph(3)), now, now, id)
	}
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) RequeueParserEvalFailures(ctx context.Context, id string, now time.Time) (bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT status FROM parser_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	if err := tx.QueryRowContext(ctx, query, id).Scan(&status); err != nil {
		return false, scanErr(err)
	}
	if status != ParserEvalRunPartial && status != ParserEvalRunFailed {
		return false, ErrParserEvalImmutable
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_documents SET status=%s,stage=%s,error_code='',error_message='',updated_at=%s WHERE run_id=%s AND status<>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), ParserEvalDocumentUploaded, ParserEvalDocumentStageValidating, now, id, ParserEvalDocumentSucceeded); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE parser_eval_runs SET status=%s,stage=%s,progress_json='{}',error_code='',error_message='',updated_at=%s,finished_at=NULL,lease_owner='',lease_until=NULL,cancel_requested_at=NULL WHERE id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), ParserEvalRunQueued, ParserEvalRunStageValidating, now, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) ListExpiredParserEvalRuns(ctx context.Context, now time.Time, limit int) ([]ParserEvalRunRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM parser_eval_runs WHERE expires_at<=%s AND status IN (%s,%s,%s,%s) ORDER BY expires_at,id LIMIT %s`,
		parserEvalRunColumns, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), now, ParserEvalRunSucceeded, ParserEvalRunPartial, ParserEvalRunFailed, ParserEvalRunCancelled, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ParserEvalRunRecord{}
	for rows.Next() {
		record, err := scanParserEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func (d *DBStore) PurgeParserEvalRun(ctx context.Context, id string) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT status FROM parser_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	if err := tx.QueryRowContext(ctx, query, id).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !parserEvalRunTerminal(status) {
		return false, ErrParserEvalActive
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM parser_eval_documents WHERE run_id=%s`, d.ph(1)), id); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM parser_eval_runs WHERE id=%s`, d.ph(1)), id)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
