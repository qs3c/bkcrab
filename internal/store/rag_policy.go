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
	RAGPolicyIngestion    = "ingestion"
	RAGPolicyRuntime      = "runtime"
	RAGPolicyDraft        = "DRAFT"
	RAGPolicyActive       = "ACTIVE"
	RAGPolicyRetired      = "RETIRED"
	RAGGenerationBuilding = "BUILDING"
	RAGGenerationReady    = "READY"
	RAGGenerationActive   = "ACTIVE"
	RAGGenerationRetired  = "RETIRED"
	RAGGenerationFailed   = "FAILED"
	RAGGenerationDeleting = "DELETING"

	RAGGenerationDocumentPending = "PENDING"
	RAGGenerationDocumentReady   = "READY"
	RAGGenerationDocumentFailed  = "FAILED"

	RAGPolicySyncQueued    = "QUEUED"
	RAGPolicySyncRunning   = "RUNNING"
	RAGPolicySyncSucceeded = "SUCCEEDED"
	RAGPolicySyncFailed    = "FAILED"
	RAGPolicySyncCancelled = "CANCELLED"

	RAGPolicyAuditPublish    = "PUBLISH"
	RAGPolicyAuditRollback   = "ROLLBACK"
	RAGPolicyAuditKBSync     = "KB_SYNC"
	RAGPolicyAuditKBRollback = "KB_ROLLBACK"
)

type RAGPolicyRecord struct {
	Kind                                                              string
	Version                                                           int64
	PolicyJSON, Fingerprint, Status, SourceEvalRunID, CreatedBy, Note string
	CreatedAt                                                         time.Time
	ActivatedAt                                                       sql.NullTime
}
type RAGKBGenerationRecord struct {
	ID, KBID                                          string
	PolicyVersion                                     int64
	CollectionKey, EmbeddingModel                     string
	EmbeddingDims                                     int
	Status                                            string
	DocumentCount, ChunkCount                         int64
	ErrorCode, ErrorMessage, CreatedBy                string
	CreatedAt                                         time.Time
	ActivatedAt, RetiredAt, RollbackUntil, LeaseUntil sql.NullTime
	LeaseOwner                                        string
	FenceToken                                        int64
}
type RAGGenerationDocumentRecord struct {
	GenerationID, DocID             string
	DocVersion                      int64
	Status, ErrorCode, ErrorMessage string
}
type RAGPolicySyncTaskRecord struct {
	ID, KBID, SourceGenerationID, TargetGenerationID     string
	TargetPolicyVersion                                  int64
	Status, ProgressJSON, EstimateJSON, RequestedBy      string
	CancelRequestedAt, LeaseUntil, StartedAt, FinishedAt sql.NullTime
	LeaseOwner                                           string
	FenceToken, RetryCount                               int64
	ErrorCode, ErrorMessage                              string
	CreatedAt                                            time.Time
}
type RAGPolicySyncFence struct {
	TaskID, KBID, TargetGenerationID, LeaseOwner string
	TargetPolicyVersion                          int64
	FenceToken                                   int64
}

func validRAGPolicyStatus(status string) bool {
	return status == RAGPolicyDraft || status == RAGPolicyActive || status == RAGPolicyRetired
}

func validRAGGenerationStatus(status string) bool {
	switch status {
	case RAGGenerationBuilding, RAGGenerationReady, RAGGenerationActive, RAGGenerationRetired, RAGGenerationFailed, RAGGenerationDeleting:
		return true
	default:
		return false
	}
}

func validRAGGenerationDocumentStatus(status string) bool {
	return status == RAGGenerationDocumentPending || status == RAGGenerationDocumentReady || status == RAGGenerationDocumentFailed
}

func validRAGPolicySyncStatus(status string) bool {
	switch status {
	case RAGPolicySyncQueued, RAGPolicySyncRunning, RAGPolicySyncSucceeded, RAGPolicySyncFailed, RAGPolicySyncCancelled:
		return true
	default:
		return false
	}
}

func policyTable(kind string) (string, error) {
	switch kind {
	case RAGPolicyIngestion:
		return "rag_ingestion_policies", nil
	case RAGPolicyRuntime:
		return "rag_runtime_policies", nil
	default:
		return "", fmt.Errorf("invalid policy kind %q", kind)
	}
}

func (d *DBStore) CreateRAGPolicy(ctx context.Context, record *RAGPolicyRecord) error {
	if record == nil || record.Version <= 0 || !json.Valid([]byte(record.PolicyJSON)) || strings.TrimSpace(record.Fingerprint) == "" || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("complete policy revision is required")
	}
	table, err := policyTable(record.Kind)
	if err != nil {
		return err
	}
	if record.Status == "" {
		record.Status = RAGPolicyDraft
	}
	if record.Status != RAGPolicyDraft {
		return errors.New("new policy must be DRAFT")
	}
	record.CreatedAt = time.Now().UTC()
	_, err = d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s(version,policy_json,fingerprint,status,source_eval_run_id,created_by,note,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s)`, table, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)), record.Version, record.PolicyJSON, record.Fingerprint, record.Status, nullString(record.SourceEvalRunID), record.CreatedBy, record.Note, record.CreatedAt)
	return err
}

// ActivateRAGPolicy writes the audit event and performs an active-pointer CAS
// in one transaction. Revisions remain immutable; rollback is the same pointer
// operation with a different audit action.
func (d *DBStore) ActivateRAGPolicy(ctx context.Context, kind string, expected, current int64, actor, sourceRun, note, action string) (bool, error) {
	table, err := policyTable(kind)
	if err != nil {
		return false, err
	}
	if action != RAGPolicyAuditPublish && action != RAGPolicyAuditRollback {
		return false, errors.New("invalid policy audit action")
	}
	if current <= 0 || current == expected || strings.TrimSpace(actor) == "" || (action == RAGPolicyAuditRollback && expected == 0) {
		return false, errors.New("invalid policy activation request")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT status FROM %s WHERE version=%s`, table, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	var status string
	if err = tx.QueryRowContext(ctx, query, current).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if !validRAGPolicyStatus(status) {
		return false, fmt.Errorf("invalid stored RAG policy status %q", status)
	}
	wantTargetStatus := RAGPolicyDraft
	if action == RAGPolicyAuditRollback {
		wantTargetStatus = RAGPolicyRetired
	}
	if status != wantTargetStatus {
		return false, nil
	}
	now := time.Now().UTC()
	var result sql.Result
	if expected == 0 {
		if d.dialect == mysqlDialect {
			result, err = tx.ExecContext(ctx, `INSERT IGNORE INTO rag_policy_active_pointers(policy_kind,active_version,updated_at) VALUES(?,?,?)`, kind, current, now)
		} else {
			result, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_policy_active_pointers(policy_kind,active_version,updated_at) VALUES(%s,%s,%s) ON CONFLICT(policy_kind) DO NOTHING`, d.ph(1), d.ph(2), d.ph(3)), kind, current, now)
		}
	} else {
		result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_policy_active_pointers SET active_version=%s,updated_at=%s WHERE policy_kind=%s AND active_version=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), current, now, kind, expected)
	}
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	if expected > 0 {
		result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=%s WHERE version=%s AND status=%s`, table, d.ph(1), d.ph(2), d.ph(3)), RAGPolicyRetired, expected, RAGPolicyActive)
		if err != nil {
			return false, err
		}
		if changed, _ = result.RowsAffected(); changed != 1 {
			return false, nil
		}
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=%s,activated_at=%s WHERE version=%s AND status=%s`, table, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), RAGPolicyActive, now, current, wantTargetStatus)
	if err != nil {
		return false, err
	}
	if changed, _ = result.RowsAffected(); changed != 1 {
		return false, nil
	}
	auditID := "rpa_" + uuid.NewString()
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_policy_audit_log(id,policy_kind,from_version,to_version,action,actor_id,source_eval_run_id,target_kb_id,note,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)), auditID, kind, expected, current, action, actor, nullString(sourceRun), nil, note, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) ActiveRAGPolicy(ctx context.Context, kind string) (*RAGPolicyRecord, error) {
	table, err := policyTable(kind)
	if err != nil {
		return nil, err
	}
	item := &RAGPolicyRecord{Kind: kind}
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT p.version,p.policy_json,p.fingerprint,p.status,COALESCE(p.source_eval_run_id,''),p.created_by,p.note,p.created_at,p.activated_at FROM %s p JOIN rag_policy_active_pointers a ON a.active_version=p.version AND a.policy_kind=%s`, table, d.ph(1)), kind).Scan(&item.Version, &item.PolicyJSON, &item.Fingerprint, &item.Status, &item.SourceEvalRunID, &item.CreatedBy, &item.Note, &item.CreatedAt, &item.ActivatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if item.Status != RAGPolicyActive {
		return nil, fmt.Errorf("active RAG policy pointer references status %q", item.Status)
	}
	return item, nil
}

func (d *DBStore) CreateRAGKBGeneration(ctx context.Context, record *RAGKBGenerationRecord, documents []RAGGenerationDocumentRecord) error {
	if record == nil || strings.TrimSpace(record.KBID) == "" || record.PolicyVersion <= 0 || strings.TrimSpace(record.CollectionKey) == "" || strings.TrimSpace(record.EmbeddingModel) == "" || record.EmbeddingDims <= 0 || strings.TrimSpace(record.CreatedBy) == "" {
		return errors.New("complete generation is required")
	}
	if record.ID == "" {
		record.ID = "rkg_" + uuid.NewString()
	}
	if record.Status == "" {
		record.Status = RAGGenerationBuilding
	}
	if record.Status != RAGGenerationBuilding {
		return errors.New("new generation must be BUILDING")
	}
	record.CreatedAt = time.Now().UTC()
	seenDocuments := make(map[string]struct{}, len(documents))
	for index := range documents {
		doc := &documents[index]
		if strings.TrimSpace(doc.DocID) == "" || doc.DocVersion <= 0 {
			return errors.New("invalid generation document")
		}
		if _, exists := seenDocuments[doc.DocID]; exists {
			return fmt.Errorf("duplicate generation document %q", doc.DocID)
		}
		seenDocuments[doc.DocID] = struct{}{}
		if doc.Status == "" {
			doc.Status = RAGGenerationDocumentPending
		}
		if !validRAGGenerationDocumentStatus(doc.Status) {
			return fmt.Errorf("invalid generation document status %q", doc.Status)
		}
		doc.ErrorCode, doc.ErrorMessage = sanitizeRAGEvalError(doc.ErrorCode, doc.ErrorMessage)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kb_index_generations(id,kb_id,policy_version,collection_key,embedding_model,embedding_dims,status,document_count,chunk_count,error_code,error_message,created_by,created_at,lease_owner) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14)), record.ID, record.KBID, record.PolicyVersion, record.CollectionKey, record.EmbeddingModel, record.EmbeddingDims, record.Status, len(documents), 0, "", "", record.CreatedBy, record.CreatedAt, "")
	if err != nil {
		return err
	}
	for _, doc := range documents {
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kb_generation_documents(generation_id,doc_id,doc_version,status,error_code,error_message) VALUES(%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), record.ID, doc.DocID, doc.DocVersion, doc.Status, doc.ErrorCode, doc.ErrorMessage)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanRAGKBGeneration(scanner interface{ Scan(...any) error }) (*RAGKBGenerationRecord, error) {
	var item RAGKBGenerationRecord
	err := scanner.Scan(&item.ID, &item.KBID, &item.PolicyVersion, &item.CollectionKey, &item.EmbeddingModel, &item.EmbeddingDims, &item.Status, &item.DocumentCount, &item.ChunkCount, &item.ErrorCode, &item.ErrorMessage, &item.CreatedBy, &item.CreatedAt, &item.ActivatedAt, &item.RetiredAt, &item.RollbackUntil, &item.LeaseOwner, &item.LeaseUntil, &item.FenceToken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validRAGGenerationStatus(item.Status) {
		return nil, fmt.Errorf("invalid stored RAG generation status %q", item.Status)
	}
	return &item, nil
}

const ragKBGenerationColumns = `id,kb_id,policy_version,collection_key,embedding_model,embedding_dims,status,document_count,chunk_count,error_code,error_message,created_by,created_at,activated_at,retired_at,rollback_until,lease_owner,lease_until,fence_token`

func (d *DBStore) GetRAGKBGeneration(ctx context.Context, id string) (*RAGKBGenerationRecord, error) {
	return scanRAGKBGeneration(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_kb_index_generations WHERE id=%s`, ragKBGenerationColumns, d.ph(1)), id))
}

func (d *DBStore) ListRAGKBGenerationDocuments(ctx context.Context, generationID string) ([]RAGGenerationDocumentRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT generation_id,doc_id,doc_version,status,error_code,error_message FROM rag_kb_generation_documents WHERE generation_id=%s ORDER BY doc_id`, d.ph(1)), generationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGGenerationDocumentRecord{}
	for rows.Next() {
		var item RAGGenerationDocumentRecord
		if err := rows.Scan(&item.GenerationID, &item.DocID, &item.DocVersion, &item.Status, &item.ErrorCode, &item.ErrorMessage); err != nil {
			return nil, err
		}
		if !validRAGGenerationDocumentStatus(item.Status) {
			return nil, fmt.Errorf("invalid stored RAG generation document status %q", item.Status)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ResolveActiveRAGKBGeneration is the authoritative generation mapping read:
// the KB pointer and generation status must agree before document versions are
// returned to a future generation-aware Search implementation.
func (d *DBStore) ResolveActiveRAGKBGeneration(ctx context.Context, kbID string) (*RAGKBGenerationRecord, []RAGGenerationDocumentRecord, error) {
	generation, err := scanRAGKBGeneration(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_kb_index_generations g JOIN rag_kbs k ON k.active_generation_id=g.id WHERE k.id=%s AND g.kb_id=k.id AND g.status=%s`, "g."+strings.ReplaceAll(ragKBGenerationColumns, ",", ",g."), d.ph(1), d.ph(2)), kbID, RAGGenerationActive))
	if err != nil {
		return nil, nil, err
	}
	documents, err := d.ListRAGKBGenerationDocuments(ctx, generation.ID)
	if err != nil {
		return nil, nil, err
	}
	return generation, documents, nil
}

func (d *DBStore) CreateRAGPolicySyncTask(ctx context.Context, record *RAGPolicySyncTaskRecord) error {
	if record == nil || strings.TrimSpace(record.KBID) == "" || strings.TrimSpace(record.TargetGenerationID) == "" || record.TargetPolicyVersion <= 0 || strings.TrimSpace(record.RequestedBy) == "" {
		return errors.New("complete RAG policy sync task is required")
	}
	if !json.Valid([]byte(emptyJSON(record.ProgressJSON))) || !json.Valid([]byte(emptyJSON(record.EstimateJSON))) {
		return errors.New("policy sync task JSON is invalid")
	}
	if record.ID == "" {
		record.ID = "rps_" + uuid.NewString()
	}
	record.Status = RAGPolicySyncQueued
	record.CreatedAt = time.Now().UTC()
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kbQuery := fmt.Sprintf(`SELECT active_generation_id FROM rag_kbs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		kbQuery += " FOR UPDATE"
	}
	var active sql.NullString
	if err = tx.QueryRowContext(ctx, kbQuery, record.KBID).Scan(&active); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var targetKB string
	var targetPolicy int64
	var targetStatus string
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT kb_id,policy_version,status FROM rag_kb_index_generations WHERE id=%s`, d.ph(1)), record.TargetGenerationID).Scan(&targetKB, &targetPolicy, &targetStatus); err != nil {
		return scanErr(err)
	}
	if targetKB != record.KBID || targetPolicy != record.TargetPolicyVersion || targetStatus != RAGGenerationBuilding {
		return errors.New("policy sync target generation mismatch")
	}
	var activeTasks int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_kb_policy_sync_tasks WHERE kb_id=%s AND status IN (%s,%s)`, d.ph(1), d.ph(2), d.ph(3)), record.KBID, RAGPolicySyncQueued, RAGPolicySyncRunning).Scan(&activeTasks); err != nil {
		return err
	}
	if activeTasks != 0 {
		return errors.New("policy sync already active for knowledge base")
	}
	if record.SourceGenerationID == "" {
		record.SourceGenerationID = active.String
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kb_policy_sync_tasks(id,kb_id,source_generation_id,target_generation_id,target_policy_version,status,progress_json,estimate_json,requested_by,lease_owner,error_code,error_message,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)), record.ID, record.KBID, nullString(record.SourceGenerationID), record.TargetGenerationID, record.TargetPolicyVersion, record.Status, emptyJSON(record.ProgressJSON), emptyJSON(record.EstimateJSON), record.RequestedBy, "", "", "", record.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ClaimRAGPolicySyncTask(ctx context.Context, taskID, worker string, lease time.Duration) (*RAGPolicySyncFence, bool, error) {
	if strings.TrimSpace(worker) == "" || lease <= 0 {
		return nil, false, errors.New("worker and positive lease are required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var task RAGPolicySyncTaskRecord
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT id,kb_id,target_generation_id,target_policy_version,status,lease_until,fence_token,cancel_requested_at FROM rag_kb_policy_sync_tasks WHERE id=%s`, d.ph(1)), taskID).Scan(&task.ID, &task.KBID, &task.TargetGenerationID, &task.TargetPolicyVersion, &task.Status, &task.LeaseUntil, &task.FenceToken, &task.CancelRequestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if !validRAGPolicySyncStatus(task.Status) {
		return nil, false, fmt.Errorf("invalid stored RAG policy sync status %q", task.Status)
	}
	now := time.Now().UTC()
	claimable := task.Status == RAGPolicySyncQueued || (task.Status == RAGPolicySyncRunning && (!task.LeaseUntil.Valid || !task.LeaseUntil.Time.After(now)))
	if !claimable || task.CancelRequestedAt.Valid {
		return nil, false, nil
	}
	nextFence := task.FenceToken + 1
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET status=%s,lease_owner=%s,lease_until=%s,fence_token=%s,started_at=COALESCE(started_at,%s) WHERE id=%s AND fence_token=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), RAGPolicySyncRunning, worker, now.Add(lease), nextFence, now, taskID, task.FenceToken)
	if err != nil {
		return nil, false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return nil, false, nil
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return &RAGPolicySyncFence{TaskID: task.ID, KBID: task.KBID, TargetGenerationID: task.TargetGenerationID, TargetPolicyVersion: task.TargetPolicyVersion, LeaseOwner: worker, FenceToken: nextFence}, true, nil
}

func (d *DBStore) HeartbeatRAGPolicySyncTask(ctx context.Context, fence RAGPolicySyncFence, lease time.Duration) (bool, error) {
	if lease <= 0 {
		return false, errors.New("positive lease is required")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET lease_until=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), now.Add(lease), fence.TaskID, RAGPolicySyncRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) RequestCancelRAGPolicySyncTask(ctx context.Context, id string) (bool, error) {
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET status=%s,cancel_requested_at=%s,finished_at=%s WHERE id=%s AND status=%s AND cancel_requested_at IS NULL`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), RAGPolicySyncCancelled, now, now, id, RAGPolicySyncQueued)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 1 {
		return changed == 1, err
	}
	result, err = d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET cancel_requested_at=%s WHERE id=%s AND status=%s AND cancel_requested_at IS NULL`, d.ph(1), d.ph(2), d.ph(3)), now, id, RAGPolicySyncRunning)
	if err != nil {
		return false, err
	}
	changed, err = result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) FinishRAGPolicySyncTask(ctx context.Context, fence RAGPolicySyncFence, status, errorCode, errorMessage string) (bool, error) {
	if status != RAGPolicySyncFailed && status != RAGPolicySyncCancelled {
		return false, errors.New("invalid terminal policy sync status")
	}
	errorCode, errorMessage = sanitizeRAGEvalError(errorCode, errorMessage)
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET status=%s,error_code=%s,error_message=%s,finished_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9)), status, errorCode, errorMessage, now, fence.TaskID, RAGPolicySyncRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func scanRAGPolicySyncTask(scanner interface{ Scan(...any) error }) (*RAGPolicySyncTaskRecord, error) {
	var item RAGPolicySyncTaskRecord
	var source sql.NullString
	err := scanner.Scan(&item.ID, &item.KBID, &source, &item.TargetGenerationID, &item.TargetPolicyVersion, &item.Status, &item.ProgressJSON, &item.EstimateJSON, &item.RequestedBy, &item.CancelRequestedAt, &item.LeaseOwner, &item.LeaseUntil, &item.FenceToken, &item.RetryCount, &item.ErrorCode, &item.ErrorMessage, &item.CreatedAt, &item.StartedAt, &item.FinishedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validRAGPolicySyncStatus(item.Status) {
		return nil, fmt.Errorf("invalid stored RAG policy sync status %q", item.Status)
	}
	item.SourceGenerationID = source.String
	return &item, nil
}

const ragPolicySyncTaskColumns = `id,kb_id,source_generation_id,target_generation_id,target_policy_version,status,progress_json,estimate_json,requested_by,cancel_requested_at,lease_owner,lease_until,fence_token,retry_count,error_code,error_message,created_at,started_at,finished_at`

func (d *DBStore) GetRAGPolicySyncTask(ctx context.Context, id string) (*RAGPolicySyncTaskRecord, error) {
	return scanRAGPolicySyncTask(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_kb_policy_sync_tasks WHERE id=%s`, ragPolicySyncTaskColumns, d.ph(1)), id))
}

func (d *DBStore) lockRAGPolicySyncFenceTx(ctx context.Context, tx *sql.Tx, fence RAGPolicySyncFence) (*RAGPolicySyncTaskRecord, bool, error) {
	query := fmt.Sprintf(`SELECT %s FROM rag_kb_policy_sync_tasks WHERE id=%s`, ragPolicySyncTaskColumns, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	task, err := scanRAGPolicySyncTask(tx.QueryRowContext(ctx, query, fence.TaskID))
	if errors.Is(err, ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	valid := task.Status == RAGPolicySyncRunning && task.LeaseOwner == fence.LeaseOwner && task.FenceToken == fence.FenceToken && task.LeaseUntil.Valid && task.LeaseUntil.Time.After(time.Now().UTC()) && !task.CancelRequestedAt.Valid && task.KBID == fence.KBID && task.TargetGenerationID == fence.TargetGenerationID && task.TargetPolicyVersion == fence.TargetPolicyVersion
	return task, valid, nil
}

func (d *DBStore) UpdateRAGGenerationDocument(ctx context.Context, fence RAGPolicySyncFence, docID, status, errorCode, errorMessage string) (bool, error) {
	if strings.TrimSpace(docID) == "" || (status != RAGGenerationDocumentReady && status != RAGGenerationDocumentFailed) {
		return false, errors.New("invalid generation document update")
	}
	errorCode, errorMessage = sanitizeRAGEvalError(errorCode, errorMessage)
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, valid, err := d.lockRAGPolicySyncFenceTx(ctx, tx, fence)
	if err != nil || !valid {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_generation_documents SET status=%s,error_code=%s,error_message=%s WHERE generation_id=%s AND doc_id=%s AND status IN (%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), status, errorCode, errorMessage, fence.TargetGenerationID, docID, RAGGenerationDocumentPending, RAGGenerationDocumentFailed)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	return true, tx.Commit()
}

func (d *DBStore) generationCompleteTx(ctx context.Context, tx *sql.Tx, generationID string, expectedDocuments int64) (bool, error) {
	var total, ready int64
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*),COALESCE(SUM(CASE WHEN status=%s THEN 1 ELSE 0 END),0) FROM rag_kb_generation_documents WHERE generation_id=%s`, d.ph(1), d.ph(2)), RAGGenerationDocumentReady, generationID).Scan(&total, &ready)
	if err != nil {
		return false, err
	}
	return total == expectedDocuments && ready == total, nil
}

func (d *DBStore) MarkRAGKBGenerationReady(ctx context.Context, fence RAGPolicySyncFence, documentCount, chunkCount int64) (bool, error) {
	if documentCount < 0 || chunkCount < 0 {
		return false, errors.New("generation counts must be non-negative")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, valid, err := d.lockRAGPolicySyncFenceTx(ctx, tx, fence)
	if err != nil || !valid {
		return false, err
	}
	complete, err := d.generationCompleteTx(ctx, tx, fence.TargetGenerationID, documentCount)
	if err != nil || !complete {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,document_count=%s,chunk_count=%s WHERE id=%s AND kb_id=%s AND policy_version=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), RAGGenerationReady, documentCount, chunkCount, fence.TargetGenerationID, fence.KBID, fence.TargetPolicyVersion, RAGGenerationBuilding)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	return true, tx.Commit()
}

func (d *DBStore) ActivateRAGKBGeneration(ctx context.Context, fence RAGPolicySyncFence, expectedActiveID, actor, note string, rollbackWindow time.Duration) (bool, error) {
	if strings.TrimSpace(actor) == "" || rollbackWindow < 0 {
		return false, errors.New("actor and non-negative rollback window are required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	task, valid, err := d.lockRAGPolicySyncFenceTx(ctx, tx, fence)
	if err != nil || !valid {
		return false, err
	}
	kbQuery := fmt.Sprintf(`SELECT active_generation_id FROM rag_kbs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		kbQuery += " FOR UPDATE"
	}
	var active sql.NullString
	if err = tx.QueryRowContext(ctx, kbQuery, fence.KBID).Scan(&active); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if active.String != expectedActiveID || task.SourceGenerationID != expectedActiveID {
		return false, nil
	}
	targetQuery := fmt.Sprintf(`SELECT %s FROM rag_kb_index_generations WHERE id=%s`, ragKBGenerationColumns, d.ph(1))
	if d.dialect != "sqlite" {
		targetQuery += " FOR UPDATE"
	}
	target, err := scanRAGKBGeneration(tx.QueryRowContext(ctx, targetQuery, fence.TargetGenerationID))
	if err != nil {
		return false, err
	}
	if target.KBID != fence.KBID || target.PolicyVersion != fence.TargetPolicyVersion || target.Status != RAGGenerationReady {
		return false, nil
	}
	complete, err := d.generationCompleteTx(ctx, tx, target.ID, target.DocumentCount)
	if err != nil || !complete {
		return false, err
	}
	now := time.Now().UTC()
	fromPolicyVersion := int64(0)
	if expectedActiveID != "" {
		var oldStatus string
		if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status,policy_version FROM rag_kb_index_generations WHERE id=%s AND kb_id=%s`, d.ph(1), d.ph(2)), expectedActiveID, fence.KBID).Scan(&oldStatus, &fromPolicyVersion); err != nil {
			return false, scanErr(err)
		}
		if oldStatus != RAGGenerationActive {
			return false, nil
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,retired_at=%s,rollback_until=%s WHERE id=%s AND kb_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), RAGGenerationRetired, now, now.Add(rollbackWindow), expectedActiveID, fence.KBID, RAGGenerationActive)
		if err != nil {
			return false, err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return false, nil
		}
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,activated_at=%s,retired_at=NULL,rollback_until=NULL WHERE id=%s AND kb_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), RAGGenerationActive, now, target.ID, fence.KBID, RAGGenerationReady)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET active_generation_id=%s,pinned_policy_version=%s,updated_at=%s WHERE id=%s AND ((active_generation_id IS NULL AND %s IS NULL) OR active_generation_id=%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), target.ID, target.PolicyVersion, now, fence.KBID, nullString(expectedActiveID), expectedActiveID)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET status=%s,finished_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), RAGPolicySyncSucceeded, now, fence.TaskID, RAGPolicySyncRunning, fence.LeaseOwner, fence.FenceToken)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	if err = d.appendRAGPolicyAuditTx(ctx, tx, RAGPolicyIngestion, fromPolicyVersion, target.PolicyVersion, RAGPolicyAuditKBSync, actor, "", fence.KBID, note, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) RollbackRAGKBGeneration(ctx context.Context, kbID, targetRetiredID, expectedActiveID, actor, note string, rollbackWindow time.Duration) (bool, error) {
	if kbID == "" || targetRetiredID == "" || expectedActiveID == "" || targetRetiredID == expectedActiveID || strings.TrimSpace(actor) == "" || rollbackWindow < 0 {
		return false, errors.New("invalid generation rollback request")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	kbQuery := fmt.Sprintf(`SELECT active_generation_id FROM rag_kbs WHERE id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		kbQuery += " FOR UPDATE"
	}
	var active sql.NullString
	if err = tx.QueryRowContext(ctx, kbQuery, kbID).Scan(&active); err != nil {
		return false, scanErr(err)
	}
	if active.String != expectedActiveID {
		return false, nil
	}
	now := time.Now().UTC()
	var targetStatus string
	var targetPolicy, targetDocumentCount int64
	var rollbackUntil sql.NullTime
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status,policy_version,document_count,rollback_until FROM rag_kb_index_generations WHERE id=%s AND kb_id=%s`, d.ph(1), d.ph(2)), targetRetiredID, kbID).Scan(&targetStatus, &targetPolicy, &targetDocumentCount, &rollbackUntil); err != nil {
		return false, scanErr(err)
	}
	if targetStatus != RAGGenerationRetired || !rollbackUntil.Valid || !rollbackUntil.Time.After(now) {
		return false, nil
	}
	complete, err := d.generationCompleteTx(ctx, tx, targetRetiredID, targetDocumentCount)
	if err != nil || !complete {
		return false, err
	}
	var currentStatus string
	var currentPolicy int64
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status,policy_version FROM rag_kb_index_generations WHERE id=%s AND kb_id=%s`, d.ph(1), d.ph(2)), expectedActiveID, kbID).Scan(&currentStatus, &currentPolicy); err != nil {
		return false, scanErr(err)
	}
	if currentStatus != RAGGenerationActive {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,retired_at=%s,rollback_until=%s WHERE id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), RAGGenerationRetired, now, now.Add(rollbackWindow), expectedActiveID, RAGGenerationActive)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,activated_at=%s,retired_at=NULL,rollback_until=NULL WHERE id=%s AND status=%s AND rollback_until>%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), RAGGenerationActive, now, targetRetiredID, RAGGenerationRetired, now)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET active_generation_id=%s,pinned_policy_version=%s,updated_at=%s WHERE id=%s AND active_generation_id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), targetRetiredID, targetPolicy, now, kbID, expectedActiveID)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	if err = d.appendRAGPolicyAuditTx(ctx, tx, RAGPolicyIngestion, currentPolicy, targetPolicy, RAGPolicyAuditKBRollback, actor, "", kbID, note, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) appendRAGPolicyAuditTx(ctx context.Context, tx *sql.Tx, kind string, fromVersion, toVersion int64, action, actor, sourceRun, kbID, note string, now time.Time) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_policy_audit_log(id,policy_kind,from_version,to_version,action,actor_id,source_eval_run_id,target_kb_id,note,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)), "rpa_"+uuid.NewString(), kind, fromVersion, toVersion, action, actor, nullString(sourceRun), nullString(kbID), note, now)
	return err
}

func OpaqueRAGCollectionKey(prefix, identity string) string {
	clean := strings.ToLower(strings.TrimSpace(prefix))
	if clean == "" {
		clean = "rag"
	}
	return fmt.Sprintf("%s_%s", clean, strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity+"|"+uuid.NewString())).String(), "-", ""))
}
