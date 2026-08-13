package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"

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

	RAGEvalRunModeFullPipeline = "FULL_PIPELINE"
	RAGEvalRunModeOnlineOnly   = "ONLINE_ONLY"

	RAGEvalCaseOK    = "ok"
	RAGEvalCaseError = "error"

	RAGEvalMetricOK                  = "ok"
	RAGEvalMetricSkippedMissingInput = "skipped_missing_input"
	RAGEvalMetricError               = "error"
)

var (
	ErrRAGEvalImmutable  = errors.New("store: immutable RAG evaluation record")
	ErrRAGEvalFenceLost  = errors.New("store: RAG evaluation fence lost")
	ErrRAGEvalReferenced = errors.New("store: RAG evaluation resource is referenced by a retained run")
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
type RAGEvalCorpusDocumentRecord struct {
	ID, DatasetVersionID, ExternalID, FileName, MediaType, SHA256, ObjectKey, MetadataJSON string
	SizeBytes                                                                              int64
}
type RAGEvalCaseRecord struct {
	ID, DatasetVersionID, ExternalID, UserInput, ReferenceAnswer, ReferenceContextsJSON, ReferenceContextIDsJSON, HistoryJSON, TagsJSON, MetadataJSON string
	ExpectedAbstention                                                                                                                                bool
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

func validRAGEvalDatasetStatus(status string) bool {
	switch status {
	case RAGEvalDatasetDraft, RAGEvalDatasetValidating, RAGEvalDatasetReady, RAGEvalDatasetFailed:
		return true
	default:
		return false
	}
}

func validRAGEvalRunStatus(status string) bool {
	switch status {
	case RAGEvalRunQueued, RAGEvalRunRunning, RAGEvalRunSucceeded, RAGEvalRunFailed, RAGEvalRunCancelled, RAGEvalRunBudgetExceeded:
		return true
	default:
		return false
	}
}

func validRAGEvalRunMode(mode string) bool {
	return mode == RAGEvalRunModeFullPipeline || mode == RAGEvalRunModeOnlineOnly
}

func validRAGEvalCaseStatus(status string) bool {
	return status == RAGEvalCaseOK || status == RAGEvalCaseError
}

func validRAGEvalMetricStatus(status string) bool {
	return status == RAGEvalMetricOK || status == RAGEvalMetricSkippedMissingInput || status == RAGEvalMetricError
}

func truncateRAGEvalText(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "")
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func sanitizeRAGEvalError(code, message string) (string, string) {
	return truncateRAGEvalText(strings.TrimSpace(code), 128), truncateRAGEvalText(message, 2048)
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
	limit = boundedRAGEvalListLimit(limit)
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

func (d *DBStore) GetRAGEvalDataset(ctx context.Context, id string) (*RAGEvalDatasetRecord, error) {
	var item RAGEvalDatasetRecord
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,name,description,created_by,created_at,updated_at,deleted_at FROM rag_eval_datasets WHERE id=%s AND deleted_at IS NULL`, d.ph(1)), id).
		Scan(&item.ID, &item.Name, &item.Description, &item.CreatedBy, &item.CreatedAt, &item.UpdatedAt, &item.DeletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &item, err
}

func (d *DBStore) ListRAGEvalDatasetVersions(ctx context.Context, datasetID, cursor string, limit int) ([]RAGEvalDatasetVersionRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_dataset_versions WHERE dataset_id=%s AND id>%s ORDER BY id LIMIT %s`, ragEvalDatasetVersionColumns, d.ph(1), d.ph(2), d.ph(3)), datasetID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []RAGEvalDatasetVersionRecord{}
	for rows.Next() {
		item, scanErr := scanRAGEvalDatasetVersion(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

// TombstoneRAGEvalDataset makes the logical dataset invisible without deleting
// version rows or object-store artifacts in the request transaction.
func (d *DBStore) TombstoneRAGEvalDataset(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, errors.New("dataset id is required")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_datasets SET deleted_at=%s,updated_at=%s WHERE id=%s AND deleted_at IS NULL`, d.ph(1), d.ph(2), d.ph(3)), now, now, id)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) ListRAGEvalDatasetStagingCandidates(ctx context.Context, before time.Time, limit int) ([]RAGEvalDatasetVersionRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_dataset_versions
		WHERE status IN (%s,%s,%s) AND created_at<=%s ORDER BY created_at,id LIMIT %s`,
		ragEvalDatasetVersionColumns, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		RAGEvalDatasetDraft, RAGEvalDatasetFailed, RAGEvalDatasetReady, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []RAGEvalDatasetVersionRecord{}
	for rows.Next() {
		item, err := scanRAGEvalDatasetVersion(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (d *DBStore) ListRAGEvalDatasetGCCandidates(ctx context.Context, before time.Time, limit int) ([]string, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT d.id FROM rag_eval_datasets d
		WHERE d.deleted_at IS NOT NULL AND d.deleted_at<=%s AND NOT EXISTS (
			SELECT 1 FROM rag_eval_dataset_versions v JOIN rag_eval_runs r ON r.dataset_version_id=v.id WHERE v.dataset_id=d.id
		) ORDER BY d.deleted_at,d.id LIMIT %s`, d.ph(1), d.ph(2)), before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		items = append(items, id)
	}
	return items, rows.Err()
}

// PurgeRAGEvalDataset removes only an already tombstoned, unreferenced
// dataset. The reference check and child-row deletion share one transaction.
func (d *DBStore) PurgeRAGEvalDataset(ctx context.Context, id string) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT deleted_at FROM rag_eval_datasets WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var deletedAt sql.NullTime
	if err = tx.QueryRowContext(ctx, query, id).Scan(&deletedAt); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !deletedAt.Valid {
		return false, errors.New("store: active evaluation dataset cannot be purged")
	}
	var references int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_eval_runs r JOIN rag_eval_dataset_versions v
		ON v.id=r.dataset_version_id WHERE v.dataset_id=%s`, d.ph(1)), id).Scan(&references); err != nil {
		return false, err
	}
	if references > 0 {
		return false, ErrRAGEvalReferenced
	}
	for _, statement := range []string{
		`DELETE FROM rag_eval_cases WHERE dataset_version_id IN (SELECT id FROM rag_eval_dataset_versions WHERE dataset_id=%s)`,
		`DELETE FROM rag_eval_corpus_documents WHERE dataset_version_id IN (SELECT id FROM rag_eval_dataset_versions WHERE dataset_id=%s)`,
		`DELETE FROM rag_eval_dataset_versions WHERE dataset_id=%s`,
		`DELETE FROM rag_eval_datasets WHERE id=%s AND deleted_at IS NOT NULL`,
	} {
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(statement, d.ph(1)), id); err != nil {
			return false, err
		}
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) CreateRAGEvalDatasetVersion(ctx context.Context, record *RAGEvalDatasetVersionRecord) error {
	if record == nil || record.DatasetID == "" || record.Version <= 0 || strings.TrimSpace(record.SourceType) == "" || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("dataset version identity is required")
	}
	if record.ValidationReportJSON != "" && !json.Valid([]byte(record.ValidationReportJSON)) {
		return errors.New("validation report JSON is invalid")
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
	var datasetExists int
	if err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_eval_datasets WHERE id=%s AND deleted_at IS NULL`, d.ph(1)), record.DatasetID).Scan(&datasetExists); err != nil {
		return err
	}
	if datasetExists != 1 {
		return ErrNotFound
	}
	now := time.Now().UTC()
	record.CreatedAt = now
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_dataset_versions(id,dataset_id,version,status,source_type,manifest_object_key,corpus_sha256,case_count,document_count,total_bytes,validation_report_json,created_by,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)), record.ID, record.DatasetID, record.Version, record.Status, record.SourceType, record.ManifestObjectKey, record.CorpusSHA256, record.CaseCount, record.DocumentCount, record.TotalBytes, emptyJSON(record.ValidationReportJSON), record.CreatedBy, now)
	return err
}

func scanRAGEvalDatasetVersion(scanner interface{ Scan(...any) error }) (*RAGEvalDatasetVersionRecord, error) {
	var item RAGEvalDatasetVersionRecord
	err := scanner.Scan(&item.ID, &item.DatasetID, &item.Version, &item.Status, &item.SourceType, &item.ManifestObjectKey, &item.CorpusSHA256, &item.CaseCount, &item.DocumentCount, &item.TotalBytes, &item.ValidationReportJSON, &item.CreatedBy, &item.CreatedAt, &item.ReadyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validRAGEvalDatasetStatus(item.Status) {
		return nil, fmt.Errorf("invalid stored RAG evaluation dataset status %q", item.Status)
	}
	return &item, nil
}

const ragEvalDatasetVersionColumns = `id,dataset_id,version,status,source_type,manifest_object_key,corpus_sha256,case_count,document_count,total_bytes,validation_report_json,created_by,created_at,ready_at`

func (d *DBStore) GetRAGEvalDatasetVersion(ctx context.Context, id string) (*RAGEvalDatasetVersionRecord, error) {
	return scanRAGEvalDatasetVersion(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_dataset_versions WHERE id=%s`, ragEvalDatasetVersionColumns, d.ph(1)), id))
}

func validDatasetTransition(from, to string) bool {
	switch from {
	case RAGEvalDatasetDraft:
		return to == RAGEvalDatasetValidating || to == RAGEvalDatasetFailed
	case RAGEvalDatasetValidating:
		return to == RAGEvalDatasetReady || to == RAGEvalDatasetFailed
	}
	return false
}
func (d *DBStore) TransitionRAGEvalDatasetVersion(ctx context.Context, id, from, to, reportJSON string) (bool, error) {
	if !validDatasetTransition(from, to) {
		return false, fmt.Errorf("invalid dataset transition %s -> %s", from, to)
	}
	if !json.Valid([]byte(emptyJSON(reportJSON))) {
		return false, errors.New("validation report JSON is invalid")
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

func (d *DBStore) beginMutableRAGEvalDatasetVersion(ctx context.Context, id string) (*sql.Tx, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`SELECT status FROM rag_eval_dataset_versions WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	if err = tx.QueryRowContext(ctx, query, id).Scan(&status); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if !validRAGEvalDatasetStatus(status) {
		_ = tx.Rollback()
		return nil, fmt.Errorf("invalid stored RAG evaluation dataset status %q", status)
	}
	if status != RAGEvalDatasetDraft {
		_ = tx.Rollback()
		return nil, ErrRAGEvalImmutable
	}
	return tx, nil
}

func (d *DBStore) PutRAGEvalCorpusDocument(ctx context.Context, record *RAGEvalCorpusDocumentRecord) error {
	if record == nil || record.DatasetVersionID == "" || strings.TrimSpace(record.ExternalID) == "" || record.SizeBytes < 0 || !json.Valid([]byte(emptyJSON(record.MetadataJSON))) {
		return errors.New("valid corpus document is required")
	}
	if record.ID == "" {
		record.ID = "red_" + uuid.NewString()
	}
	tx, err := d.beginMutableRAGEvalDatasetVersion(ctx, record.DatasetVersionID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_eval_corpus_documents(id,dataset_version_id,external_id,file_name,media_type,size_bytes,sha256,object_key,metadata_json) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9))
	if d.dialect == mysqlDialect {
		query += ` ON DUPLICATE KEY UPDATE file_name=VALUES(file_name),media_type=VALUES(media_type),size_bytes=VALUES(size_bytes),sha256=VALUES(sha256),object_key=VALUES(object_key),metadata_json=VALUES(metadata_json)`
	} else {
		query += ` ON CONFLICT(dataset_version_id,external_id) DO UPDATE SET file_name=excluded.file_name,media_type=excluded.media_type,size_bytes=excluded.size_bytes,sha256=excluded.sha256,object_key=excluded.object_key,metadata_json=excluded.metadata_json`
	}
	if _, err = tx.ExecContext(ctx, query, record.ID, record.DatasetVersionID, strings.TrimSpace(record.ExternalID), record.FileName, record.MediaType, record.SizeBytes, record.SHA256, record.ObjectKey, emptyJSON(record.MetadataJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ListRAGEvalCorpusDocuments(ctx context.Context, datasetVersionID, cursor string, limit int) ([]RAGEvalCorpusDocumentRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id,dataset_version_id,external_id,file_name,media_type,size_bytes,sha256,object_key,metadata_json FROM rag_eval_corpus_documents WHERE dataset_version_id=%s AND id>%s ORDER BY id LIMIT %s`, d.ph(1), d.ph(2), d.ph(3)), datasetVersionID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalCorpusDocumentRecord{}
	for rows.Next() {
		var item RAGEvalCorpusDocumentRecord
		if err := rows.Scan(&item.ID, &item.DatasetVersionID, &item.ExternalID, &item.FileName, &item.MediaType, &item.SizeBytes, &item.SHA256, &item.ObjectKey, &item.MetadataJSON); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DBStore) PutRAGEvalCase(ctx context.Context, record *RAGEvalCaseRecord) error {
	if record == nil || record.DatasetVersionID == "" || strings.TrimSpace(record.ExternalID) == "" || strings.TrimSpace(record.UserInput) == "" {
		return errors.New("valid evaluation case is required")
	}
	for _, value := range []string{record.ReferenceContextsJSON, record.ReferenceContextIDsJSON, record.HistoryJSON, record.TagsJSON, record.MetadataJSON} {
		if !json.Valid([]byte(emptyJSONArray(value))) {
			return errors.New("evaluation case JSON is invalid")
		}
	}
	if record.ID == "" {
		record.ID = "rec_" + uuid.NewString()
	}
	tx, err := d.beginMutableRAGEvalDatasetVersion(ctx, record.DatasetVersionID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_eval_cases(id,dataset_version_id,external_id,user_input,reference_answer,reference_contexts_json,reference_context_ids_json,history_json,expected_abstention,tags_json,metadata_json) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11))
	if d.dialect == mysqlDialect {
		query += ` ON DUPLICATE KEY UPDATE user_input=VALUES(user_input),reference_answer=VALUES(reference_answer),reference_contexts_json=VALUES(reference_contexts_json),reference_context_ids_json=VALUES(reference_context_ids_json),history_json=VALUES(history_json),expected_abstention=VALUES(expected_abstention),tags_json=VALUES(tags_json),metadata_json=VALUES(metadata_json)`
	} else {
		query += ` ON CONFLICT(dataset_version_id,external_id) DO UPDATE SET user_input=excluded.user_input,reference_answer=excluded.reference_answer,reference_contexts_json=excluded.reference_contexts_json,reference_context_ids_json=excluded.reference_context_ids_json,history_json=excluded.history_json,expected_abstention=excluded.expected_abstention,tags_json=excluded.tags_json,metadata_json=excluded.metadata_json`
	}
	if _, err = tx.ExecContext(ctx, query, record.ID, record.DatasetVersionID, strings.TrimSpace(record.ExternalID), record.UserInput, record.ReferenceAnswer, emptyJSONArray(record.ReferenceContextsJSON), emptyJSONArray(record.ReferenceContextIDsJSON), emptyJSONArray(record.HistoryJSON), record.ExpectedAbstention, emptyJSONArray(record.TagsJSON), emptyJSON(record.MetadataJSON)); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ListRAGEvalCases(ctx context.Context, datasetVersionID, cursor string, limit int) ([]RAGEvalCaseRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id,dataset_version_id,external_id,user_input,reference_answer,reference_contexts_json,reference_context_ids_json,history_json,expected_abstention,tags_json,metadata_json FROM rag_eval_cases WHERE dataset_version_id=%s AND id>%s ORDER BY id LIMIT %s`, d.ph(1), d.ph(2), d.ph(3)), datasetVersionID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalCaseRecord{}
	for rows.Next() {
		var item RAGEvalCaseRecord
		if err := rows.Scan(&item.ID, &item.DatasetVersionID, &item.ExternalID, &item.UserInput, &item.ReferenceAnswer, &item.ReferenceContextsJSON, &item.ReferenceContextIDsJSON, &item.HistoryJSON, &item.ExpectedAbstention, &item.TagsJSON, &item.MetadataJSON); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DBStore) CreateRAGEvalProfile(ctx context.Context, record *RAGEvalProfileRecord) error {
	if record == nil || strings.TrimSpace(record.Name) == "" || record.ProfileJSON == "" || strings.TrimSpace(record.Fingerprint) == "" || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("complete profile is required")
	}
	if !json.Valid([]byte(record.ProfileJSON)) {
		return errors.New("profile JSON is invalid")
	}
	if record.ID == "" {
		record.ID = "rep_" + uuid.NewString()
	}
	record.CreatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_profiles(id,name,profile_json,fingerprint,created_by,created_at) VALUES(%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), record.ID, strings.TrimSpace(record.Name), record.ProfileJSON, strings.TrimSpace(record.Fingerprint), record.CreatedBy, record.CreatedAt)
	return err
}

func (d *DBStore) ListRAGEvalProfiles(ctx context.Context, cursor string, limit int) ([]RAGEvalProfileRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
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

func (d *DBStore) GetRAGEvalProfile(ctx context.Context, id string) (*RAGEvalProfileRecord, error) {
	var item RAGEvalProfileRecord
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,name,profile_json,fingerprint,created_by,created_at FROM rag_eval_profiles WHERE id=%s`, d.ph(1)), id).
		Scan(&item.ID, &item.Name, &item.ProfileJSON, &item.Fingerprint, &item.CreatedBy, &item.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &item, err
}

func (d *DBStore) CreateRAGEvalRun(ctx context.Context, record *RAGEvalRunRecord) error {
	if record == nil || record.DatasetVersionID == "" || record.ProfileID == "" || !validRAGEvalRunMode(record.Mode) || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("valid dataset, profile, and mode are required")
	}
	if record.Mode == RAGEvalRunModeOnlineOnly && strings.TrimSpace(record.IndexGenerationID) == "" {
		return errors.New("online-only run requires a generation")
	}
	if record.Mode == RAGEvalRunModeFullPipeline && strings.TrimSpace(record.IndexGenerationID) != "" {
		return errors.New("full pipeline run cannot select a generation")
	}
	for _, value := range []string{record.ProgressJSON, record.ExecutionSnapshotJSON, record.RequestedMetricsJSON} {
		if !json.Valid([]byte(emptyJSON(value))) {
			return errors.New("run JSON payload is invalid")
		}
	}
	if record.ID == "" {
		record.ID = "rer_" + uuid.NewString()
	}
	record.Status = RAGEvalRunQueued
	if record.Stage == "" {
		record.Stage = "queued"
	}
	record.CreatedAt = time.Now().UTC()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if record.Mode == RAGEvalRunModeOnlineOnly {
		generationQuery := fmt.Sprintf(`SELECT dataset_version_id,status FROM rag_eval_index_generations WHERE id=%s`, d.ph(1))
		if d.dialect != "sqlite" {
			generationQuery += " FOR UPDATE"
		}
		var datasetID, status string
		if err = tx.QueryRowContext(ctx, generationQuery, record.IndexGenerationID).Scan(&datasetID, &status); errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if datasetID != record.DatasetVersionID || status != RAGEvalGenerationReady {
			return ErrRAGEvalGenerationConflict
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_runs(id,dataset_version_id,baseline_run_id,mode,profile_id,status,stage,progress_json,execution_snapshot_json,index_generation_id,requested_metrics_json,error_code,error_message,created_by,created_at,lease_owner) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15), d.ph(16)), record.ID, record.DatasetVersionID, nullString(record.BaselineRunID), record.Mode, record.ProfileID, record.Status, record.Stage, emptyJSON(record.ProgressJSON), emptyJSON(record.ExecutionSnapshotJSON), nullString(record.IndexGenerationID), emptyJSON(record.RequestedMetricsJSON), "", "", record.CreatedBy, record.CreatedAt, ""); err != nil {
		return err
	}
	if record.Mode == RAGEvalRunModeOnlineOnly {
		if err = d.attachRAGEvalGenerationRefTx(ctx, tx, record.ID, record.IndexGenerationID, record.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	if !validRAGEvalRunMode(item.Mode) || !validRAGEvalRunStatus(item.Status) {
		return nil, fmt.Errorf("invalid stored RAG evaluation run mode/status %q/%q", item.Mode, item.Status)
	}
	return &item, nil
}

const ragEvalRunColumns = `id,dataset_version_id,baseline_run_id,mode,profile_id,status,stage,progress_json,execution_snapshot_json,index_generation_id,requested_metrics_json,error_code,error_message,created_by,created_at,started_at,finished_at,lease_owner,lease_until,fence_token,cancel_requested_at,deleted_at`

func (d *DBStore) GetRAGEvalRun(ctx context.Context, id string) (*RAGEvalRunRecord, error) {
	return scanRAGEvalRun(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_runs WHERE id=%s AND deleted_at IS NULL`, ragEvalRunColumns, d.ph(1)), id))
}
func (d *DBStore) ListRAGEvalRuns(ctx context.Context, cursor string, limit int) ([]RAGEvalRunRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
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

// ClaimNextRAGEvalRun finds a durable candidate and delegates to the fenced
// claim operation. A competing worker may win between the two statements; in
// that case callers simply poll again.
func (d *DBStore) ClaimNextRAGEvalRun(ctx context.Context, worker string, lease time.Duration) (*RAGEvalRunFence, bool, error) {
	if strings.TrimSpace(worker) == "" || lease <= 0 {
		return nil, false, errors.New("worker and lease are required")
	}
	var id string
	now := time.Now().UTC()
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id FROM rag_eval_runs WHERE deleted_at IS NULL AND
		(status=%s OR (status=%s AND (lease_until IS NULL OR lease_until<=%s)))
		ORDER BY created_at,id LIMIT 1`, d.ph(1), d.ph(2), d.ph(3)), RAGEvalRunQueued, RAGEvalRunRunning, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return d.ClaimRAGEvalRun(ctx, id, worker, lease)
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
	if !validRAGEvalRunStatus(status) {
		return nil, false, fmt.Errorf("invalid stored RAG evaluation run status %q", status)
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
	if lease <= 0 {
		return false, errors.New("positive lease is required")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET lease_until=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), now.Add(lease), fence.RunID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) UpdateRAGEvalRunProgress(ctx context.Context, fence RAGEvalRunFence, stage, progressJSON string) (bool, error) {
	stage = truncateRAGEvalText(strings.TrimSpace(stage), 64)
	if stage == "" || !json.Valid([]byte(emptyJSON(progressJSON))) {
		return false, errors.New("valid run stage and progress are required")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET stage=%s,progress_json=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), stage, emptyJSON(progressJSON), fence.RunID,
		RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, now)
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
	errorCode, errorMessage = sanitizeRAGEvalError(errorCode, errorMessage)
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET status=%s,stage=%s,error_code=%s,error_message=%s,finished_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)), status, "finished", errorCode, errorMessage, now, fence.RunID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) TombstoneRAGEvalRun(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, errors.New("run id is required")
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET deleted_at=%s WHERE id=%s AND deleted_at IS NULL AND status IN (%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), time.Now().UTC(), id, RAGEvalRunSucceeded, RAGEvalRunFailed, RAGEvalRunCancelled, RAGEvalRunBudgetExceeded)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

// ListRAGEvalRunGCCandidates returns only explicitly tombstoned terminal runs.
// A retained candidate run may still name an older run as its baseline, so a
// referenced baseline is not eligible until that candidate is also removed.
func (d *DBStore) ListRAGEvalRunGCCandidates(ctx context.Context, before time.Time, limit int) ([]string, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT r.id FROM rag_eval_runs r WHERE r.deleted_at IS NOT NULL AND r.deleted_at<=%s AND NOT EXISTS (
		SELECT 1 FROM rag_eval_runs candidate WHERE candidate.baseline_run_id=r.id AND candidate.deleted_at IS NULL
	) ORDER BY r.deleted_at,r.id LIMIT %s`, d.ph(1), d.ph(2)), before.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PurgeRAGEvalRun atomically releases its generation reference and deletes all
// SQL-owned results. Physical generation deletion is deliberately separate and
// remains protected by the generation GC fence.
func (d *DBStore) PurgeRAGEvalRun(ctx context.Context, id string) (bool, error) {
	if strings.TrimSpace(id) == "" {
		return false, errors.New("run id is required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var deletedAt sql.NullTime
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT deleted_at FROM rag_eval_runs WHERE id=%s`, d.ph(1)), id).Scan(&deletedAt); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if !deletedAt.Valid {
		return false, ErrRAGEvalReferenced
	}
	var retainedBaselineRefs int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_eval_runs WHERE baseline_run_id=%s AND deleted_at IS NULL`, d.ph(1)), id).Scan(&retainedBaselineRefs); err != nil {
		return false, err
	}
	if retainedBaselineRefs != 0 {
		return false, ErrRAGEvalReferenced
	}
	var generationID string
	var released sql.NullTime
	refErr := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT generation_id,released_at FROM rag_eval_generation_refs WHERE run_id=%s`, d.ph(1)), id).Scan(&generationID, &released)
	if refErr != nil && !errors.Is(refErr, sql.ErrNoRows) {
		return false, refErr
	}
	if refErr == nil && !released.Valid {
		result, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET ref_count=ref_count-1 WHERE id=%s AND ref_count>0`, d.ph(1)), generationID)
		if updateErr != nil {
			return false, updateErr
		}
		if rows, _ := result.RowsAffected(); rows != 1 {
			return false, ErrRAGEvalReferenced
		}
	}
	for _, statement := range []string{
		`DELETE FROM rag_eval_run_aggregates WHERE run_id=%s`,
		`DELETE FROM rag_eval_metric_results WHERE run_id=%s`,
		`DELETE FROM rag_eval_case_results WHERE run_id=%s`,
		`DELETE FROM rag_eval_usage WHERE run_id=%s`,
		`DELETE FROM rag_eval_generation_refs WHERE run_id=%s`,
	} {
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(statement, d.ph(1)), id); err != nil {
			return false, err
		}
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_eval_runs WHERE id=%s AND deleted_at IS NOT NULL`, d.ph(1)), id)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows != 1 {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) PutRAGEvalCaseResult(ctx context.Context, fence RAGEvalRunFence, record RAGEvalCaseResultRecord) (bool, error) {
	if record.RunID != fence.RunID || record.CaseID == "" || record.LatencyMS < 0 || !validRAGEvalCaseStatus(record.Status) {
		return false, errors.New("case result identity mismatch")
	}
	for _, value := range []string{record.ContextsJSON, record.CitationsJSON, record.SearchTraceJSON, record.AnswerTraceJSON, record.UsageJSON} {
		if !json.Valid([]byte(emptyJSON(value))) {
			return false, errors.New("case result JSON is invalid")
		}
	}
	record.ErrorCode, record.ErrorMessage = sanitizeRAGEvalError(record.ErrorCode, record.ErrorMessage)
	tx, valid, err := d.beginRAGEvalFenceWrite(ctx, fence)
	if err != nil || !valid {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_eval_case_results(run_id,case_id,response,contexts_json,citations_json,search_trace_json,answer_trace_json,status,error_code,error_message,latency_ms,usage_json) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12))
	if d.dialect == mysqlDialect {
		query += ` ON DUPLICATE KEY UPDATE response=VALUES(response),contexts_json=VALUES(contexts_json),citations_json=VALUES(citations_json),search_trace_json=VALUES(search_trace_json),answer_trace_json=VALUES(answer_trace_json),status=VALUES(status),error_code=VALUES(error_code),error_message=VALUES(error_message),latency_ms=VALUES(latency_ms),usage_json=VALUES(usage_json)`
	} else {
		query += ` ON CONFLICT(run_id,case_id) DO UPDATE SET response=excluded.response,contexts_json=excluded.contexts_json,citations_json=excluded.citations_json,search_trace_json=excluded.search_trace_json,answer_trace_json=excluded.answer_trace_json,status=excluded.status,error_code=excluded.error_code,error_message=excluded.error_message,latency_ms=excluded.latency_ms,usage_json=excluded.usage_json`
	}
	if _, err = tx.ExecContext(ctx, query, record.RunID, record.CaseID, record.Response, emptyJSON(record.ContextsJSON), emptyJSON(record.CitationsJSON), emptyJSON(record.SearchTraceJSON), emptyJSON(record.AnswerTraceJSON), record.Status, record.ErrorCode, record.ErrorMessage, record.LatencyMS, emptyJSON(record.UsageJSON)); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) GetRAGEvalCaseResult(ctx context.Context, runID, caseID string) (*RAGEvalCaseResultRecord, error) {
	var item RAGEvalCaseResultRecord
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT run_id,case_id,response,contexts_json,citations_json,search_trace_json,answer_trace_json,status,error_code,error_message,latency_ms,usage_json FROM rag_eval_case_results WHERE run_id=%s AND case_id=%s`, d.ph(1), d.ph(2)), runID, caseID).
		Scan(&item.RunID, &item.CaseID, &item.Response, &item.ContextsJSON, &item.CitationsJSON, &item.SearchTraceJSON, &item.AnswerTraceJSON, &item.Status, &item.ErrorCode, &item.ErrorMessage, &item.LatencyMS, &item.UsageJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &item, err
}

func (d *DBStore) ListRAGEvalCaseResults(ctx context.Context, runID, cursor string, limit int) ([]RAGEvalCaseResultRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT run_id,case_id,response,contexts_json,citations_json,search_trace_json,answer_trace_json,status,error_code,error_message,latency_ms,usage_json FROM rag_eval_case_results WHERE run_id=%s AND case_id>%s ORDER BY case_id LIMIT %s`, d.ph(1), d.ph(2), d.ph(3)), runID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalCaseResultRecord{}
	for rows.Next() {
		var item RAGEvalCaseResultRecord
		if err := rows.Scan(&item.RunID, &item.CaseID, &item.Response, &item.ContextsJSON, &item.CitationsJSON, &item.SearchTraceJSON, &item.AnswerTraceJSON, &item.Status, &item.ErrorCode, &item.ErrorMessage, &item.LatencyMS, &item.UsageJSON); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (d *DBStore) PutRAGEvalMetricResult(ctx context.Context, fence RAGEvalRunFence, record RAGEvalMetricResultRecord) (bool, error) {
	if record.RunID != fence.RunID || record.CaseID == "" || record.MetricName == "" || record.MetricVersion == "" || !validRAGEvalMetricStatus(record.Status) {
		return false, errors.New("metric result identity is incomplete")
	}
	if !json.Valid([]byte(emptyJSON(record.DetailsJSON))) {
		return false, errors.New("metric details JSON is invalid")
	}
	if record.Status == RAGEvalMetricOK && (!record.Value.Valid || math.IsNaN(record.Value.Float64) || math.IsInf(record.Value.Float64, 0) || record.Value.Float64 < 0 || record.Value.Float64 > 1) {
		return false, errors.New("ok metric result requires a finite value between 0 and 1")
	}
	if record.Status != RAGEvalMetricOK {
		record.Value = sql.NullFloat64{}
	}
	record.Reason = truncateRAGEvalText(record.Reason, 2048)
	value := any(nil)
	if record.Value.Valid {
		value = record.Value.Float64
	}
	tx, valid, err := d.beginRAGEvalFenceWrite(ctx, fence)
	if err != nil || !valid {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_eval_metric_results(run_id,case_id,metric_name,metric_version,status,value,reason,details_json) VALUES(%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8))
	if d.dialect == mysqlDialect {
		query += ` ON DUPLICATE KEY UPDATE status=VALUES(status),value=VALUES(value),reason=VALUES(reason),details_json=VALUES(details_json)`
	} else {
		query += ` ON CONFLICT(run_id,case_id,metric_name,metric_version) DO UPDATE SET status=excluded.status,value=excluded.value,reason=excluded.reason,details_json=excluded.details_json`
	}
	if _, err = tx.ExecContext(ctx, query, record.RunID, record.CaseID, record.MetricName, record.MetricVersion, record.Status, value, record.Reason, emptyJSON(record.DetailsJSON)); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (d *DBStore) ListRAGEvalMetricResults(ctx context.Context, runID, cursor string, limit int) ([]RAGEvalMetricResultRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	cursorParts := strings.SplitN(cursor, ":", 3)
	cursorCase, cursorMetric, cursorVersion := "", "", ""
	if len(cursorParts) > 0 {
		cursorCase = cursorParts[0]
	}
	if len(cursorParts) > 1 {
		cursorMetric = cursorParts[1]
	}
	if len(cursorParts) > 2 {
		cursorVersion = cursorParts[2]
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT run_id,case_id,metric_name,metric_version,status,value,reason,details_json FROM rag_eval_metric_results WHERE run_id=%s AND (case_id>%s OR (case_id=%s AND (metric_name>%s OR (metric_name=%s AND metric_version>%s)))) ORDER BY case_id,metric_name,metric_version LIMIT %s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), runID, cursorCase, cursorCase, cursorMetric, cursorMetric, cursorVersion, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalMetricResultRecord{}
	for rows.Next() {
		var item RAGEvalMetricResultRecord
		if err := rows.Scan(&item.RunID, &item.CaseID, &item.MetricName, &item.MetricVersion, &item.Status, &item.Value, &item.Reason, &item.DetailsJSON); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func RAGEvalMetricCursor(record RAGEvalMetricResultRecord) string {
	return record.CaseID + ":" + record.MetricName + ":" + record.MetricVersion
}

func (d *DBStore) RAGEvalUsageTotals(ctx context.Context, runID string) (tokens int64, cost float64, err error) {
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(SUM(input_tokens+output_tokens),0),COALESCE(SUM(CASE WHEN actual_cost_usd>0 THEN actual_cost_usd ELSE estimated_cost_usd END),0) FROM rag_eval_usage WHERE run_id=%s`, d.ph(1)), runID).Scan(&tokens, &cost)
	return
}

func (d *DBStore) RecordRAGEvalUsageFenced(ctx context.Context, fence RAGEvalRunFence, record *RAGEvalUsageRecord) (bool, error) {
	if record == nil || record.RunID != fence.RunID || strings.TrimSpace(record.IdempotencyKey) == "" || len(record.IdempotencyKey) > 255 {
		return false, errors.New("usage run and idempotency key are required")
	}
	if record.InputTokens < 0 || record.OutputTokens < 0 || math.IsNaN(record.EstimatedCostUSD) || math.IsInf(record.EstimatedCostUSD, 0) || record.EstimatedCostUSD < 0 || math.IsNaN(record.ActualCostUSD) || math.IsInf(record.ActualCostUSD, 0) || record.ActualCostUSD < 0 {
		return false, errors.New("usage tokens and costs must be finite and non-negative")
	}
	record.Stage = truncateRAGEvalText(strings.TrimSpace(record.Stage), 64)
	record.Provider = truncateRAGEvalText(strings.TrimSpace(record.Provider), 128)
	record.Model = truncateRAGEvalText(strings.TrimSpace(record.Model), 255)
	record.IdempotencyKey = strings.TrimSpace(record.IdempotencyKey)
	if record.ID == "" {
		record.ID = "reu_" + uuid.NewString()
	}
	record.CreatedAt = time.Now().UTC()
	tx, valid, err := d.beginRAGEvalFenceWrite(ctx, fence)
	if err != nil || !valid {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_eval_usage(id,run_id,case_id,stage,provider,model,input_tokens,output_tokens,estimated_cost_usd,actual_cost_usd,idempotency_key,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12))
	if d.dialect == mysqlDialect {
		query = "INSERT IGNORE" + strings.TrimPrefix(query, "INSERT")
	} else {
		query += " ON CONFLICT(idempotency_key) DO NOTHING"
	}
	result, err := tx.ExecContext(ctx, query, record.ID, record.RunID, record.CaseID, record.Stage, record.Provider, record.Model, record.InputTokens, record.OutputTokens, record.EstimatedCostUSD, record.ActualCostUSD, record.IdempotencyKey, record.CreatedAt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}

func (d *DBStore) RecordRAGEvalUsage(ctx context.Context, record *RAGEvalUsageRecord) (bool, error) {
	if record == nil || record.RunID == "" || strings.TrimSpace(record.IdempotencyKey) == "" || len(record.IdempotencyKey) > 255 {
		return false, errors.New("usage run and idempotency key are required")
	}
	if record.InputTokens < 0 || record.OutputTokens < 0 || math.IsNaN(record.EstimatedCostUSD) || math.IsInf(record.EstimatedCostUSD, 0) || record.EstimatedCostUSD < 0 || math.IsNaN(record.ActualCostUSD) || math.IsInf(record.ActualCostUSD, 0) || record.ActualCostUSD < 0 {
		return false, errors.New("usage tokens and costs must be finite and non-negative")
	}
	record.Stage = truncateRAGEvalText(strings.TrimSpace(record.Stage), 64)
	record.Provider = truncateRAGEvalText(strings.TrimSpace(record.Provider), 128)
	record.Model = truncateRAGEvalText(strings.TrimSpace(record.Model), 255)
	record.IdempotencyKey = strings.TrimSpace(record.IdempotencyKey)
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

func (d *DBStore) beginRAGEvalFenceWrite(ctx context.Context, fence RAGEvalRunFence) (*sql.Tx, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	query := fmt.Sprintf(`SELECT status,lease_owner,fence_token,lease_until FROM rag_eval_runs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status, owner string
	var token int64
	var leaseUntil sql.NullTime
	err = tx.QueryRowContext(ctx, query, fence.RunID).Scan(&status, &owner, &token, &leaseUntil)
	if errors.Is(err, sql.ErrNoRows) {
		_ = tx.Rollback()
		return nil, false, nil
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, false, err
	}
	if !validRAGEvalRunStatus(status) {
		_ = tx.Rollback()
		return nil, false, fmt.Errorf("invalid stored RAG evaluation run status %q", status)
	}
	if status != RAGEvalRunRunning || owner != fence.LeaseOwner || token != fence.FenceToken || !leaseUntil.Valid || !leaseUntil.Time.After(time.Now().UTC()) {
		_ = tx.Rollback()
		return nil, false, nil
	}
	return tx, true, nil
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

func emptyJSONArray(value string) string {
	if strings.TrimSpace(value) == "" {
		return "[]"
	}
	return value
}

func boundedRAGEvalListLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
