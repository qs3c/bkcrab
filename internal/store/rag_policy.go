package store

import (
	"context"
	"database/sql"
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
	if record == nil || record.Version <= 0 || record.PolicyJSON == "" || record.Fingerprint == "" || record.CreatedBy == "" {
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
	if action != "PUBLISH" && action != "ROLLBACK" {
		return false, errors.New("invalid policy audit action")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM %s WHERE version=%s`, table, d.ph(1)), current).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
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
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=%s WHERE version=%s`, table, d.ph(1), d.ph(2)), RAGPolicyRetired, expected); err != nil {
			return false, err
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status=%s,activated_at=%s WHERE version=%s`, table, d.ph(1), d.ph(2), d.ph(3)), RAGPolicyActive, now, current); err != nil {
		return false, err
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
	return item, err
}

func (d *DBStore) CreateRAGKBGeneration(ctx context.Context, record *RAGKBGenerationRecord, documents []RAGGenerationDocumentRecord) error {
	if record == nil || record.KBID == "" || record.PolicyVersion <= 0 || record.CollectionKey == "" || record.EmbeddingModel == "" || record.EmbeddingDims <= 0 {
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
		if doc.DocID == "" || doc.DocVersion <= 0 {
			return errors.New("invalid generation document")
		}
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kb_generation_documents(generation_id,doc_id,doc_version,status,error_code,error_message) VALUES(%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), record.ID, doc.DocID, doc.DocVersion, doc.Status, doc.ErrorCode, doc.ErrorMessage)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DBStore) MarkRAGKBGenerationReady(ctx context.Context, id string, documentCount, chunkCount int64) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,document_count=%s,chunk_count=%s WHERE id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), RAGGenerationReady, documentCount, chunkCount, id, RAGGenerationBuilding)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (d *DBStore) ActivateRAGKBGeneration(ctx context.Context, kbID, targetID string, expectedActiveID string, policyVersion int64, actor, note string, rollbackWindow time.Duration) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET active_generation_id=%s,pinned_policy_version=%s,updated_at=%s WHERE id=%s AND ((active_generation_id IS NULL AND %s IS NULL) OR active_generation_id=%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), targetID, policyVersion, now, kbID, nullString(expectedActiveID), expectedActiveID)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	if expectedActiveID != "" {
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,retired_at=%s,rollback_until=%s WHERE id=%s AND kb_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), RAGGenerationRetired, now, now.Add(rollbackWindow), expectedActiveID, kbID, RAGGenerationActive); err != nil {
			return false, err
		}
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,activated_at=%s WHERE id=%s AND kb_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), RAGGenerationActive, now, targetID, kbID, RAGGenerationReady)
	if err != nil {
		return false, err
	}
	changed, _ = result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_policy_audit_log(id,policy_kind,from_version,to_version,action,actor_id,source_eval_run_id,target_kb_id,note,created_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)), "rpa_"+uuid.NewString(), RAGPolicyIngestion, 0, policyVersion, "KB_SYNC", actor, nil, kbID, note, now)
	if err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func OpaqueRAGCollectionKey(prefix, identity string) string {
	clean := strings.ToLower(strings.TrimSpace(prefix))
	if clean == "" {
		clean = "rag"
	}
	return fmt.Sprintf("%s_%s", clean, strings.ReplaceAll(uuid.NewSHA1(uuid.NameSpaceOID, []byte(identity+"|"+uuid.NewString())).String(), "-", ""))
}
