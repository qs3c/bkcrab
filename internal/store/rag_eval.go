package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	RAGEvalDatasetDraft      = "DRAFT"
	RAGEvalDatasetValidating = "VALIDATING"
	RAGEvalDatasetReady      = "READY"
	RAGEvalDatasetFailed     = "FAILED"
	RAGEvalRunQueued         = "QUEUED"
	RAGEvalRunRunning        = "RUNNING"
	RAGEvalRunSucceeded      = "SUCCEEDED"
	RAGEvalRunFailed         = "FAILED"
	RAGEvalRunCancelled      = "CANCELLED"
	RAGEvalRunBudgetExceeded = "BUDGET_EXCEEDED"
)

var (
	ErrRAGEvalImmutable = errors.New("store: immutable RAG evaluation record")
	ErrRAGEvalFenceLost = errors.New("store: RAG evaluation fence lost")
)

type RAGEvalDatasetRecord struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	CreatedBy   string       `json:"createdBy"`
	CreatedAt   time.Time    `json:"createdAt"`
	UpdatedAt   time.Time    `json:"updatedAt"`
	DeletedAt   sql.NullTime `json:"-"`
}
type RAGEvalDatasetVersionRecord struct {
	ID, DatasetID                                       string
	Version                                             int64
	Status, SourceType, ManifestObjectKey, CorpusSHA256 string
	CaseCount, DocumentCount, TotalBytes                int64
	ValidationReportJSON, CreatedBy                     string
	CreatedAt                                           time.Time
	ReadyAt                                             sql.NullTime
}
type RAGEvalProfileRecord struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	ProfileJSON string    `json:"profileJson"`
	Fingerprint string    `json:"fingerprint"`
	CreatedBy   string    `json:"createdBy"`
	CreatedAt   time.Time `json:"createdAt"`
}
type RAGEvalRunRecord struct {
	ID                    string       `json:"id"`
	DatasetVersionID      string       `json:"datasetVersionId"`
	BaselineRunID         string       `json:"baselineRunId,omitempty"`
	Mode                  string       `json:"mode"`
	ProfileID             string       `json:"profileId"`
	Status                string       `json:"status"`
	Stage                 string       `json:"stage"`
	ProgressJSON          string       `json:"progressJson"`
	ExecutionSnapshotJSON string       `json:"executionSnapshotJson"`
	IndexGenerationID     string       `json:"indexGenerationId,omitempty"`
	RequestedMetricsJSON  string       `json:"requestedMetricsJson"`
	ErrorCode             string       `json:"errorCode,omitempty"`
	ErrorMessage          string       `json:"errorMessage,omitempty"`
	CreatedBy             string       `json:"createdBy"`
	CreatedAt             time.Time    `json:"createdAt"`
	StartedAt             sql.NullTime `json:"startedAt,omitempty"`
	FinishedAt            sql.NullTime `json:"finishedAt,omitempty"`
	LeaseUntil            sql.NullTime `json:"-"`
	CancelRequestedAt     sql.NullTime `json:"cancelRequestedAt,omitempty"`
	DeletedAt             sql.NullTime `json:"-"`
	LeaseOwner            string       `json:"-"`
	FenceToken            int64        `json:"-"`
}
type RAGEvalRunFence struct {
	RunID, LeaseOwner string
	FenceToken        int64
}
type RAGEvalCaseResultRecord struct {
	RunID, CaseID, Response, ContextsJSON, CitationsJSON, SearchTraceJSON, AnswerTraceJSON, Status, ErrorCode, ErrorMessage, UsageJSON string
	LatencyMS                                                                                                                          int64
}
type RAGEvalMetricResultRecord struct {
	RunID, CaseID, MetricName, MetricVersion, Status string
	Value                                            sql.NullFloat64
	Reason, DetailsJSON                              string
}
type RAGEvalUsageRecord struct {
	ID, RunID, CaseID, Stage, Provider, Model, IdempotencyKey string
	InputTokens, OutputTokens                                 int64
	EstimatedCostUSD, ActualCostUSD                           float64
	CreatedAt                                                 time.Time
}

func (d *DBStore) CreateRAGEvalDataset(ctx context.Context, record *RAGEvalDatasetRecord) error {
	if record == nil || strings.TrimSpace(record.Name) == "" || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("dataset name and creator are required")
	}
	if record.ID == "" {
		record.ID = "rds_" + uuid.NewString()
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	record.UpdatedAt = now
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_datasets(id,name,description,created_by,created_at,updated_at) VALUES(%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), record.ID, strings.TrimSpace(record.Name), record.Description, record.CreatedBy, now, now)
	return err
}

func (d *DBStore) ListRAGEvalDatasets(ctx context.Context, cursor string, limit int) ([]RAGEvalDatasetRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id,name,description,created_by,created_at,updated_at,deleted_at FROM rag_eval_datasets WHERE deleted_at IS NULL AND id>%s ORDER BY id LIMIT %s`, d.ph(1), d.ph(2)), cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalDatasetRecord{}
	for rows.Next() {
		var item RAGEvalDatasetRecord
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.CreatedBy, &item.CreatedAt, &item.UpdatedAt, &item.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DBStore) CreateRAGEvalDatasetVersion(ctx context.Context, record *RAGEvalDatasetVersionRecord) error {
	if record == nil || record.DatasetID == "" || record.Version <= 0 {
		return errors.New("dataset version identity is required")
	}
	if record.ID == "" {
		record.ID = "rdv_" + uuid.NewString()
	}
	if record.Status == "" {
		record.Status = RAGEvalDatasetDraft
	}
	if record.Status != RAGEvalDatasetDraft {
		return errors.New("new dataset version must be DRAFT")
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_dataset_versions(id,dataset_id,version,status,source_type,manifest_object_key,corpus_sha256,case_count,document_count,total_bytes,validation_report_json,created_by,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)), record.ID, record.DatasetID, record.Version, record.Status, record.SourceType, record.ManifestObjectKey, record.CorpusSHA256, record.CaseCount, record.DocumentCount, record.TotalBytes, emptyJSON(record.ValidationReportJSON), record.CreatedBy, now)
	return err
}

func validDatasetTransition(from, to string) bool {
	switch from {
	case RAGEvalDatasetDraft:
		return to == RAGEvalDatasetValidating
	case RAGEvalDatasetValidating:
		return to == RAGEvalDatasetReady || to == RAGEvalDatasetFailed
	}
	return false
}
func (d *DBStore) TransitionRAGEvalDatasetVersion(ctx context.Context, id, from, to, reportJSON string) (bool, error) {
	if !validDatasetTransition(from, to) {
		return false, fmt.Errorf("invalid dataset transition %s -> %s", from, to)
	}
	ready := any(nil)
	if to == RAGEvalDatasetReady {
		ready = time.Now().UTC()
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_dataset_versions SET status=%s,validation_report_json=%s,ready_at=%s WHERE id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), to, emptyJSON(reportJSON), ready, id, from)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) CreateRAGEvalProfile(ctx context.Context, record *RAGEvalProfileRecord) error {
	if record == nil || record.Name == "" || record.ProfileJSON == "" || record.Fingerprint == "" {
		return errors.New("complete profile is required")
	}
	if !json.Valid([]byte(record.ProfileJSON)) {
		return errors.New("profile JSON is invalid")
	}
	if record.ID == "" {
		record.ID = "rep_" + uuid.NewString()
	}
	record.CreatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_profiles(id,name,profile_json,fingerprint,created_by,created_at) VALUES(%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), record.ID, record.Name, record.ProfileJSON, record.Fingerprint, record.CreatedBy, record.CreatedAt)
	return err
}

func (d *DBStore) ListRAGEvalProfiles(ctx context.Context, cursor string, limit int) ([]RAGEvalProfileRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id,name,profile_json,fingerprint,created_by,created_at FROM rag_eval_profiles WHERE id>%s ORDER BY id LIMIT %s`, d.ph(1), d.ph(2)), cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalProfileRecord{}
	for rows.Next() {
		var item RAGEvalProfileRecord
		if err := rows.Scan(&item.ID, &item.Name, &item.ProfileJSON, &item.Fingerprint, &item.CreatedBy, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DBStore) CreateRAGEvalRun(ctx context.Context, record *RAGEvalRunRecord) error {
	if record == nil || record.DatasetVersionID == "" || record.ProfileID == "" || (record.Mode != "FULL_PIPELINE" && record.Mode != "ONLINE_ONLY") {
		return errors.New("valid dataset, profile, and mode are required")
	}
	if record.ID == "" {
		record.ID = "rer_" + uuid.NewString()
	}
	record.Status = RAGEvalRunQueued
	if record.Stage == "" {
		record.Stage = "queued"
	}
	record.CreatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_runs(id,dataset_version_id,baseline_run_id,mode,profile_id,status,stage,progress_json,execution_snapshot_json,index_generation_id,requested_metrics_json,error_code,error_message,created_by,created_at,lease_owner) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15), d.ph(16)), record.ID, record.DatasetVersionID, nullString(record.BaselineRunID), record.Mode, record.ProfileID, record.Status, record.Stage, emptyJSON(record.ProgressJSON), emptyJSON(record.ExecutionSnapshotJSON), nullString(record.IndexGenerationID), emptyJSON(record.RequestedMetricsJSON), "", "", record.CreatedBy, record.CreatedAt, "")
	return err
}

func scanRAGEvalRun(scanner interface{ Scan(...any) error }) (*RAGEvalRunRecord, error) {
	var item RAGEvalRunRecord
	var baseline, indexGeneration sql.NullString
	err := scanner.Scan(&item.ID, &item.DatasetVersionID, &baseline, &item.Mode, &item.ProfileID, &item.Status, &item.Stage, &item.ProgressJSON, &item.ExecutionSnapshotJSON, &indexGeneration, &item.RequestedMetricsJSON, &item.ErrorCode, &item.ErrorMessage, &item.CreatedBy, &item.CreatedAt, &item.StartedAt, &item.FinishedAt, &item.LeaseOwner, &item.LeaseUntil, &item.FenceToken, &item.CancelRequestedAt, &item.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	item.BaselineRunID = baseline.String
	item.IndexGenerationID = indexGeneration.String
	return &item, nil
}

const ragEvalRunColumns = `id,dataset_version_id,baseline_run_id,mode,profile_id,status,stage,progress_json,execution_snapshot_json,index_generation_id,requested_metrics_json,error_code,error_message,created_by,created_at,started_at,finished_at,lease_owner,lease_until,fence_token,cancel_requested_at,deleted_at`

func (d *DBStore) GetRAGEvalRun(ctx context.Context, id string) (*RAGEvalRunRecord, error) {
	return scanRAGEvalRun(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_runs WHERE id=%s AND deleted_at IS NULL`, ragEvalRunColumns, d.ph(1)), id))
}
func (d *DBStore) ListRAGEvalRuns(ctx context.Context, cursor string, limit int) ([]RAGEvalRunRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_runs WHERE id>%s AND deleted_at IS NULL ORDER BY id LIMIT %s`, ragEvalRunColumns, d.ph(1), d.ph(2)), cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalRunRecord{}
	for rows.Next() {
		item, err := scanRAGEvalRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

func (d *DBStore) ClaimRAGEvalRun(ctx context.Context, runID, worker string, lease time.Duration) (*RAGEvalRunFence, bool, error) {
	if worker == "" || lease <= 0 {
		return nil, false, errors.New("worker and lease are required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var status string
	var fence int64
	var leaseUntil sql.NullTime
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status,fence_token,lease_until FROM rag_eval_runs WHERE id=%s`, d.ph(1)), runID).Scan(&status, &fence, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	if status != RAGEvalRunQueued && !(status == RAGEvalRunRunning && (!leaseUntil.Valid || leaseUntil.Time.Before(now))) {
		return nil, false, nil
	}
	next := fence + 1
	until := now.Add(lease)
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET status=%s,stage=%s,lease_owner=%s,lease_until=%s,fence_token=%s,started_at=COALESCE(started_at,%s) WHERE id=%s AND fence_token=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)), RAGEvalRunRunning, "running", worker, until, next, now, runID, fence)
	if err != nil {
		return nil, false, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return nil, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &RAGEvalRunFence{runID, worker, next}, true, nil
}

func (d *DBStore) HeartbeatRAGEvalRun(ctx context.Context, fence RAGEvalRunFence, lease time.Duration) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET lease_until=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), time.Now().UTC().Add(lease), fence.RunID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (d *DBStore) RequestCancelRAGEvalRun(ctx context.Context, id string) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET cancel_requested_at=%s WHERE id=%s AND status IN (%s,%s) AND cancel_requested_at IS NULL`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), time.Now().UTC(), id, RAGEvalRunQueued, RAGEvalRunRunning)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
func (d *DBStore) FinishRAGEvalRun(ctx context.Context, fence RAGEvalRunFence, status, errorCode, errorMessage string) (bool, error) {
	if status != RAGEvalRunSucceeded && status != RAGEvalRunFailed && status != RAGEvalRunCancelled && status != RAGEvalRunBudgetExceeded {
		return false, errors.New("invalid terminal run status")
	}
	if len(errorMessage) > 2048 {
		errorMessage = errorMessage[:2048]
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET status=%s,stage=%s,error_code=%s,error_message=%s,finished_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9)), status, "finished", errorCode, errorMessage, time.Now().UTC(), fence.RunID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) PutRAGEvalCaseResult(ctx context.Context, fence RAGEvalRunFence, record RAGEvalCaseResultRecord) (bool, error) {
	valid, err := d.checkRAGEvalFence(ctx, fence)
	if err != nil || !valid {
		return false, err
	}
	if record.RunID != fence.RunID || record.CaseID == "" {
		return false, errors.New("case result identity mismatch")
	}
	if len(record.ErrorMessage) > 2048 {
		record.ErrorMessage = record.ErrorMessage[:2048]
	}
	if d.dialect == mysqlDialect {
		_, err = d.db.ExecContext(ctx, `INSERT INTO rag_eval_case_results(run_id,case_id,response,contexts_json,citations_json,search_trace_json,answer_trace_json,status,error_code,error_message,latency_ms,usage_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE response=VALUES(response),contexts_json=VALUES(contexts_json),citations_json=VALUES(citations_json),search_trace_json=VALUES(search_trace_json),answer_trace_json=VALUES(answer_trace_json),status=VALUES(status),error_code=VALUES(error_code),error_message=VALUES(error_message),latency_ms=VALUES(latency_ms),usage_json=VALUES(usage_json)`, record.RunID, record.CaseID, record.Response, emptyJSON(record.ContextsJSON), emptyJSON(record.CitationsJSON), emptyJSON(record.SearchTraceJSON), emptyJSON(record.AnswerTraceJSON), record.Status, record.ErrorCode, record.ErrorMessage, record.LatencyMS, emptyJSON(record.UsageJSON))
		return err == nil, err
	}
	_, err = d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_case_results(run_id,case_id,response,contexts_json,citations_json,search_trace_json,answer_trace_json,status,error_code,error_message,latency_ms,usage_json) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s) ON CONFLICT(run_id,case_id) DO UPDATE SET response=excluded.response,contexts_json=excluded.contexts_json,citations_json=excluded.citations_json,search_trace_json=excluded.search_trace_json,answer_trace_json=excluded.answer_trace_json,status=excluded.status,error_code=excluded.error_code,error_message=excluded.error_message,latency_ms=excluded.latency_ms,usage_json=excluded.usage_json`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12)), record.RunID, record.CaseID, record.Response, emptyJSON(record.ContextsJSON), emptyJSON(record.CitationsJSON), emptyJSON(record.SearchTraceJSON), emptyJSON(record.AnswerTraceJSON), record.Status, record.ErrorCode, record.ErrorMessage, record.LatencyMS, emptyJSON(record.UsageJSON))
	return err == nil, err
}

func (d *DBStore) PutRAGEvalMetricResult(ctx context.Context, fence RAGEvalRunFence, record RAGEvalMetricResultRecord) (bool, error) {
	valid, err := d.checkRAGEvalFence(ctx, fence)
	if err != nil || !valid {
		return false, err
	}
	if record.RunID != fence.RunID || record.CaseID == "" || record.MetricName == "" || record.MetricVersion == "" {
		return false, errors.New("metric result identity is incomplete")
	}
	value := any(nil)
	if record.Value.Valid {
		value = record.Value.Float64
	}
	if d.dialect == mysqlDialect {
		_, err = d.db.ExecContext(ctx, `INSERT INTO rag_eval_metric_results(run_id,case_id,metric_name,metric_version,status,value,reason,details_json) VALUES(?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE status=VALUES(status),value=VALUES(value),reason=VALUES(reason),details_json=VALUES(details_json)`, record.RunID, record.CaseID, record.MetricName, record.MetricVersion, record.Status, value, record.Reason, emptyJSON(record.DetailsJSON))
		return err == nil, err
	}
	_, err = d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_metric_results(run_id,case_id,metric_name,metric_version,status,value,reason,details_json) VALUES(%s,%s,%s,%s,%s,%s,%s,%s) ON CONFLICT(run_id,case_id,metric_name,metric_version) DO UPDATE SET status=excluded.status,value=excluded.value,reason=excluded.reason,details_json=excluded.details_json`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)), record.RunID, record.CaseID, record.MetricName, record.MetricVersion, record.Status, value, record.Reason, emptyJSON(record.DetailsJSON))
	return err == nil, err
}

func (d *DBStore) RecordRAGEvalUsage(ctx context.Context, record *RAGEvalUsageRecord) (bool, error) {
	if record == nil || record.RunID == "" || record.IdempotencyKey == "" {
		return false, errors.New("usage run and idempotency key are required")
	}
	if record.ID == "" {
		record.ID = "reu_" + uuid.NewString()
	}
	record.CreatedAt = time.Now().UTC()
	query := fmt.Sprintf(`INSERT INTO rag_eval_usage(id,run_id,case_id,stage,provider,model,input_tokens,output_tokens,estimated_cost_usd,actual_cost_usd,idempotency_key,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12))
	if d.dialect == mysqlDialect {
		query = "INSERT IGNORE" + strings.TrimPrefix(query, "INSERT")
	} else {
		query += " ON CONFLICT(idempotency_key) DO NOTHING"
	}
	result, err := d.db.ExecContext(ctx, query, record.ID, record.RunID, record.CaseID, record.Stage, record.Provider, record.Model, record.InputTokens, record.OutputTokens, record.EstimatedCostUSD, record.ActualCostUSD, record.IdempotencyKey, record.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) checkRAGEvalFence(ctx context.Context, fence RAGEvalRunFence) (bool, error) {
	var count int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_eval_runs WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), fence.RunID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, time.Now().UTC()).Scan(&count)
	return count == 1, err
}

func emptyJSON(value string) string {
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	return value
}
func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
