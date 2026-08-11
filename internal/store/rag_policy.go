package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
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
type RAGPolicyAuditRecord struct {
	ID, PolicyKind, Action, ActorID, SourceEvalRunID, TargetKBID, Note string
	FromVersion, ToVersion                                             int64
	CreatedAt                                                          time.Time
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

var ErrRAGLegacyGenerationConflict = errors.New("store: legacy RAG generation snapshot conflict")
var ErrRAGKBPolicySyncActive = errors.New("store: RAG knowledge base policy sync is active")

type legacyIngestionPolicySnapshot struct {
	SchemaVersion string                         `json:"schemaVersion"`
	Source        string                         `json:"source"`
	KB            legacyIngestionKBSnapshot      `json:"kb"`
	Documents     []legacyIngestionDocumentState `json:"documents"`
}

type legacyIngestionKBSnapshot struct {
	ID                string `json:"id"`
	EmbeddingProvider string `json:"embeddingProvider"`
	EmbeddingModel    string `json:"embeddingModel"`
	EmbeddingDims     int    `json:"embeddingDims"`
	ChunkSize         int    `json:"chunkSize"`
	ChunkOverlap      int    `json:"chunkOverlap"`
	ParseMode         string `json:"parseMode"`
	EnrichmentEnabled bool   `json:"enrichmentEnabled"`
}

type legacyIngestionDocumentState struct {
	ID                 string                          `json:"id"`
	ActiveVersion      int64                           `json:"activeVersion"`
	SourceSHA256       string                          `json:"sourceSha256,omitempty"`
	IndexFormatVersion int                             `json:"indexFormatVersion"`
	SnapshotMissing    bool                            `json:"snapshotMissing,omitempty"`
	Snapshot           *legacyIngestionVersionSnapshot `json:"snapshot,omitempty"`
}

type legacyIngestionVersionSnapshot struct {
	ParseMode                    string `json:"parseMode"`
	ChunkSize                    int    `json:"chunkSize"`
	ChunkOverlap                 int    `json:"chunkOverlap"`
	ParserVersion                string `json:"parserVersion"`
	SplitterVersion              string `json:"splitterVersion"`
	ParseFingerprint             string `json:"parseFingerprint"`
	IndexFingerprint             string `json:"indexFingerprint"`
	VisionModel                  string `json:"visionModel,omitempty"`
	VisionProviderFingerprint    string `json:"visionProviderFingerprint,omitempty"`
	VisionPromptVersion          string `json:"visionPromptVersion,omitempty"`
	TextModel                    string `json:"textModel,omitempty"`
	TextProviderFingerprint      string `json:"textProviderFingerprint,omitempty"`
	EnrichmentPromptVersion      string `json:"enrichmentPromptVersion,omitempty"`
	EnrichmentEnabled            bool   `json:"enrichmentEnabled"`
	EmbeddingProvider            string `json:"embeddingProvider"`
	EmbeddingModel               string `json:"embeddingModel"`
	EmbeddingDimensions          int    `json:"embeddingDimensions"`
	EmbeddingContractFingerprint string `json:"embeddingContractFingerprint"`
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

func (d *DBStore) GetRAGPolicy(ctx context.Context, kind string, version int64) (*RAGPolicyRecord, error) {
	table, err := policyTable(kind)
	if err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, errors.New("policy version must be positive")
	}
	item := &RAGPolicyRecord{Kind: kind}
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT version,policy_json,fingerprint,status,COALESCE(source_eval_run_id,''),created_by,note,created_at,activated_at FROM %s WHERE version=%s`, table, d.ph(1)), version).Scan(&item.Version, &item.PolicyJSON, &item.Fingerprint, &item.Status, &item.SourceEvalRunID, &item.CreatedBy, &item.Note, &item.CreatedAt, &item.ActivatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !validRAGPolicyStatus(item.Status) {
		return nil, fmt.Errorf("invalid stored RAG policy status %q", item.Status)
	}
	return item, nil
}

func (d *DBStore) ListRAGPolicyAudits(ctx context.Context, kind string, limit int) ([]RAGPolicyAuditRecord, error) {
	if _, err := policyTable(kind); err != nil {
		return nil, err
	}
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id,policy_kind,from_version,to_version,action,actor_id,COALESCE(source_eval_run_id,''),COALESCE(target_kb_id,''),note,created_at FROM rag_policy_audit_log WHERE policy_kind=%s ORDER BY created_at DESC,id DESC LIMIT %s`, d.ph(1), d.ph(2)), kind, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGPolicyAuditRecord{}
	for rows.Next() {
		var item RAGPolicyAuditRecord
		if err := rows.Scan(&item.ID, &item.PolicyKind, &item.FromVersion, &item.ToVersion, &item.Action, &item.ActorID, &item.SourceEvalRunID, &item.TargetKBID, &item.Note, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
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

// BackfillLegacyRAGGenerations materializes an immutable description of each
// active legacy KB and points it at the collection that already exists. It does
// not create, copy, or mutate vector data. Deterministic identities plus
// conflict verification make repeated and concurrent startup calls idempotent.
func (d *DBStore) BackfillLegacyRAGGenerations(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx, `SELECT id FROM rag_kbs WHERE LOWER(status)='active' AND active_generation_id IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	var kbIDs []string
	for rows.Next() {
		var kbID string
		if err := rows.Scan(&kbID); err != nil {
			rows.Close()
			return err
		}
		kbIDs = append(kbIDs, kbID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, kbID := range kbIDs {
		if err := d.backfillLegacyRAGGeneration(ctx, kbID); err != nil {
			return fmt.Errorf("backfill legacy RAG generation %s: %w", kbID, err)
		}
	}
	return nil
}

func (d *DBStore) backfillLegacyRAGGeneration(ctx context.Context, kbID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Acquire the per-KB write lock before reading the snapshot. On SQLite the
	// no-op update also prevents two deferred transactions from racing while
	// upgrading a read snapshot to a writer.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET updated_at=updated_at WHERE id=%s AND LOWER(status)='active' AND active_generation_id IS NULL`, d.ph(1)), kbID); err != nil {
		return err
	}
	kbQuery := fmt.Sprintf(`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s AND LOWER(status)='active'`, d.ph(1))
	if d.dialect != "sqlite" {
		kbQuery += " FOR UPDATE"
	}
	kb, err := scanRAGKB(tx.QueryRowContext(ctx, kbQuery, kbID))
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if kb.ActiveGenerationID.Valid && strings.TrimSpace(kb.ActiveGenerationID.String) != "" {
		return tx.Commit()
	}

	documents, chunkCount, err := d.legacyGenerationDocumentsTx(ctx, tx, kb.ID)
	if err != nil {
		return err
	}
	policy := legacyIngestionPolicySnapshot{
		SchemaVersion: "legacy-ingestion-v1",
		Source:        "legacy-backfill",
		KB: legacyIngestionKBSnapshot{
			ID: kb.ID, EmbeddingProvider: kb.EmbedProvider, EmbeddingModel: kb.EmbedModel,
			EmbeddingDims: kb.EmbedDims, ChunkSize: kb.ChunkSize, ChunkOverlap: kb.ChunkOverlap,
			ParseMode: kb.ParseMode, EnrichmentEnabled: kb.EnrichmentEnabled,
		},
		Documents: documents,
	}
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(policyJSON)
	fingerprint := fmt.Sprintf("%x", digest[:])
	policyVersion := int64(binary.BigEndian.Uint64(digest[:8]) & uint64(^uint64(0)>>1))
	if policyVersion == 0 {
		policyVersion = 1
	}
	generationDigest := sha256.Sum256([]byte("legacy-rag-generation\x00" + kb.ID))
	generationID := fmt.Sprintf("rkg_legacy_%x", generationDigest[:16])
	now := time.Now().UTC()

	if err := d.insertLegacyIngestionPolicyTx(ctx, tx, policyVersion, string(policyJSON), fingerprint, now); err != nil {
		return err
	}
	if err := d.insertLegacyGenerationTx(ctx, tx, &RAGKBGenerationRecord{
		ID: generationID, KBID: kb.ID, PolicyVersion: policyVersion, CollectionKey: kb.ID,
		EmbeddingModel: kb.EmbedModel, EmbeddingDims: kb.EmbedDims, Status: RAGGenerationActive,
		DocumentCount: int64(len(documents)), ChunkCount: chunkCount,
		CreatedBy: "system:legacy-generation-backfill", CreatedAt: now,
	}); err != nil {
		return err
	}
	for _, document := range documents {
		if err := d.insertLegacyGenerationDocumentTx(ctx, tx, generationID, document.ID, document.ActiveVersion); err != nil {
			return err
		}
	}
	if err := d.verifyLegacyGenerationDocumentsTx(ctx, tx, generationID, documents); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET active_generation_id=%s,pinned_policy_version=%s,updated_at=%s WHERE id=%s AND active_generation_id IS NULL AND LOWER(status)='active'`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), generationID, policyVersion, now, kb.ID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrRAGLegacyGenerationConflict
	}
	return tx.Commit()
}

func (d *DBStore) legacyGenerationDocumentsTx(ctx context.Context, tx *sql.Tx, kbID string) ([]legacyIngestionDocumentState, int64, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE kb_id=%s AND active_version>0 AND LOWER(status)<>LOWER(%s) ORDER BY id`, d.ph(1), d.ph(2)), kbID, RAGDocumentStatusDeleting)
	if err != nil {
		return nil, 0, err
	}
	var active []RAGDocumentRecord
	for rows.Next() {
		document, err := scanRAGDocument(rows)
		if err != nil {
			rows.Close()
			return nil, 0, err
		}
		active = append(active, *document)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}

	documents := make([]legacyIngestionDocumentState, 0, len(active))
	var chunkCount int64
	for _, document := range active {
		item := legacyIngestionDocumentState{
			ID: document.ID, ActiveVersion: document.ActiveVersion, SourceSHA256: document.SourceSHA256,
			IndexFormatVersion: document.IndexFormatVersion,
		}
		version, err := scanRAGDocumentVersion(tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT `+ragDocumentVersionColumns+` FROM rag_document_versions WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), document.ID, document.ActiveVersion))
		if errors.Is(err, sql.ErrNoRows) {
			item.SnapshotMissing = true
		} else if err != nil {
			return nil, 0, err
		} else {
			item.Snapshot = &legacyIngestionVersionSnapshot{
				ParseMode: version.ParseMode, ChunkSize: version.ChunkSize, ChunkOverlap: version.ChunkOverlap,
				ParserVersion: version.ParserVersion, SplitterVersion: version.SplitterVersion,
				ParseFingerprint: version.ParseFingerprint, IndexFingerprint: version.IndexFingerprint,
				VisionModel: version.VisionModel, VisionProviderFingerprint: version.VisionProviderFingerprint,
				VisionPromptVersion: version.VisionPromptVersion, TextModel: version.TextModel,
				TextProviderFingerprint: version.TextProviderFingerprint,
				EnrichmentPromptVersion: version.EnrichmentPromptVersion, EnrichmentEnabled: version.EnrichmentEnabled,
				EmbeddingProvider: version.EmbeddingProvider, EmbeddingModel: version.EmbeddingModel,
				EmbeddingDimensions:          version.EmbeddingDimensions,
				EmbeddingContractFingerprint: version.EmbeddingContractFingerprint,
			}
		}
		documents = append(documents, item)
		chunkCount += int64(document.ChunkCount)
	}
	return documents, chunkCount, nil
}

func (d *DBStore) insertLegacyIngestionPolicyTx(ctx context.Context, tx *sql.Tx, version int64, policyJSON, fingerprint string, now time.Time) error {
	query := fmt.Sprintf(`INSERT INTO rag_ingestion_policies(version,policy_json,fingerprint,status,source_eval_run_id,created_by,note,created_at,activated_at) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9))
	if d.dialect == mysqlDialect {
		query = strings.Replace(query, "INSERT INTO", "INSERT IGNORE INTO", 1)
	} else {
		query += " ON CONFLICT(version) DO NOTHING"
	}
	if _, err := tx.ExecContext(ctx, query, version, policyJSON, fingerprint, RAGPolicyRetired, nil, "system:legacy-generation-backfill", "historical legacy snapshot; not the active platform policy", now, nil); err != nil {
		return err
	}
	var storedJSON, storedFingerprint string
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT policy_json,fingerprint FROM rag_ingestion_policies WHERE version=%s`, d.ph(1)), version).Scan(&storedJSON, &storedFingerprint); err != nil {
		return err
	}
	if storedJSON != policyJSON || storedFingerprint != fingerprint {
		return ErrRAGLegacyGenerationConflict
	}
	return nil
}

func (d *DBStore) insertLegacyGenerationTx(ctx context.Context, tx *sql.Tx, record *RAGKBGenerationRecord) error {
	query := fmt.Sprintf(`INSERT INTO rag_kb_index_generations(id,kb_id,policy_version,collection_key,embedding_model,embedding_dims,status,document_count,chunk_count,error_code,error_message,created_by,created_at,activated_at,lease_owner,fence_token) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15), d.ph(16))
	if d.dialect == mysqlDialect {
		query = strings.Replace(query, "INSERT INTO", "INSERT IGNORE INTO", 1)
	} else {
		query += " ON CONFLICT(id) DO NOTHING"
	}
	if _, err := tx.ExecContext(ctx, query, record.ID, record.KBID, record.PolicyVersion, record.CollectionKey, record.EmbeddingModel, record.EmbeddingDims, record.Status, record.DocumentCount, record.ChunkCount, "", "", record.CreatedBy, record.CreatedAt, record.CreatedAt, "", 0); err != nil {
		return err
	}
	stored, err := scanRAGKBGeneration(tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_kb_index_generations WHERE id=%s`, ragKBGenerationColumns, d.ph(1)), record.ID))
	if err != nil {
		return err
	}
	if stored.KBID != record.KBID || stored.PolicyVersion != record.PolicyVersion || stored.CollectionKey != record.CollectionKey || stored.EmbeddingModel != record.EmbeddingModel || stored.EmbeddingDims != record.EmbeddingDims || stored.Status != RAGGenerationActive || stored.DocumentCount != record.DocumentCount || stored.ChunkCount != record.ChunkCount {
		return ErrRAGLegacyGenerationConflict
	}
	return nil
}

func (d *DBStore) insertLegacyGenerationDocumentTx(ctx context.Context, tx *sql.Tx, generationID, docID string, docVersion int64) error {
	query := fmt.Sprintf(`INSERT INTO rag_kb_generation_documents(generation_id,doc_id,doc_version,status,error_code,error_message) VALUES(%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6))
	if d.dialect == mysqlDialect {
		query = strings.Replace(query, "INSERT INTO", "INSERT IGNORE INTO", 1)
	} else {
		query += " ON CONFLICT(generation_id,doc_id) DO NOTHING"
	}
	_, err := tx.ExecContext(ctx, query, generationID, docID, docVersion, RAGGenerationDocumentReady, "", "")
	return err
}

func (d *DBStore) verifyLegacyGenerationDocumentsTx(ctx context.Context, tx *sql.Tx, generationID string, expected []legacyIngestionDocumentState) error {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT doc_id,doc_version,status FROM rag_kb_generation_documents WHERE generation_id=%s ORDER BY doc_id`, d.ph(1)), generationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		var docID, status string
		var docVersion int64
		if err := rows.Scan(&docID, &docVersion, &status); err != nil {
			return err
		}
		if index >= len(expected) || docID != expected[index].ID || docVersion != expected[index].ActiveVersion || status != RAGGenerationDocumentReady {
			return ErrRAGLegacyGenerationConflict
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != len(expected) {
		return ErrRAGLegacyGenerationConflict
	}
	return nil
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
	var indexTasks int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_index_tasks t JOIN rag_documents d ON d.id=t.doc_id WHERE d.kb_id=%s AND t.status IN ('PENDING','RUNNING')`, d.ph(1)), record.KBID).Scan(&indexTasks); err != nil {
		return err
	}
	if indexTasks != 0 {
		return errors.New("knowledge base has active document mutations")
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
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET lease_until=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s AND cancel_requested_at IS NULL`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), now.Add(lease), fence.TaskID, RAGPolicySyncRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) UpdateRAGPolicySyncProgress(ctx context.Context, fence RAGPolicySyncFence, progressJSON string) (bool, error) {
	if !json.Valid([]byte(progressJSON)) {
		return false, errors.New("policy sync progress JSON is invalid")
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_policy_sync_tasks SET progress_json=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s AND cancel_requested_at IS NULL`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), progressJSON, fence.TaskID, RAGPolicySyncRunning, fence.LeaseOwner, fence.FenceToken, time.Now().UTC())
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

func (d *DBStore) LatestRAGPolicySyncTaskForKB(ctx context.Context, kbID string) (*RAGPolicySyncTaskRecord, error) {
	return scanRAGPolicySyncTask(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_kb_policy_sync_tasks WHERE kb_id=%s ORDER BY created_at DESC,id DESC LIMIT 1`, ragPolicySyncTaskColumns, d.ph(1)), kbID))
}

func (d *DBStore) IsRAGKBPolicySyncActive(ctx context.Context, kbID string) (bool, error) {
	var count int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_kb_policy_sync_tasks WHERE kb_id=%s AND status IN (%s,%s)`, d.ph(1), d.ph(2), d.ph(3)), kbID, RAGPolicySyncQueued, RAGPolicySyncRunning).Scan(&count)
	return count > 0, err
}

func (d *DBStore) rejectActiveRAGKBPolicySyncTx(ctx context.Context, tx *sql.Tx, kbID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_kb_policy_sync_tasks WHERE kb_id=%s AND status IN (%s,%s)`, d.ph(1), d.ph(2), d.ph(3)), kbID, RAGPolicySyncQueued, RAGPolicySyncRunning).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrRAGKBPolicySyncActive
	}
	return nil
}

func (d *DBStore) ClaimNextRAGPolicySyncTask(ctx context.Context, worker string, lease time.Duration) (*RAGPolicySyncFence, bool, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id FROM rag_kb_policy_sync_tasks WHERE status=%s OR (status=%s AND (lease_until IS NULL OR lease_until<=%s)) ORDER BY created_at,id LIMIT 16`, d.ph(1), d.ph(2), d.ragNowExpr()), RAGPolicySyncQueued, RAGPolicySyncRunning)
	if err != nil {
		return nil, false, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, false, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, false, err
	}
	for _, id := range ids {
		fence, claimed, claimErr := d.ClaimRAGPolicySyncTask(ctx, id, worker, lease)
		if claimErr != nil || claimed {
			return fence, claimed, claimErr
		}
	}
	return nil, false, nil
}

// CreateRAGGenerationDocumentVersion persists a target-policy snapshot without
// changing rag_documents.version or active_version. The sync fence and exact
// generation mapping authorize this otherwise invisible version.
func (d *DBStore) CreateRAGGenerationDocumentVersion(ctx context.Context, fence RAGPolicySyncFence, version *RAGDocumentVersionRecord) (bool, error) {
	if err := validateRunnableRAGVersionSnapshot(version); err != nil {
		return false, err
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
	var mappedVersion int64
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT doc_version FROM rag_kb_generation_documents WHERE generation_id=%s AND doc_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3)), fence.TargetGenerationID, version.DocID, RAGGenerationDocumentPending).Scan(&mappedVersion); err != nil {
		return false, scanErr(err)
	}
	if mappedVersion != version.DocVersion {
		return false, nil
	}
	var kbID string
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT kb_id FROM rag_documents WHERE id=%s`, d.ph(1)), version.DocID).Scan(&kbID); err != nil {
		return false, scanErr(err)
	}
	if kbID != fence.KBID {
		return false, nil
	}
	var existingFingerprint, existingStatus string
	existingErr := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT index_fingerprint,status FROM rag_document_versions WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), version.DocID, version.DocVersion).Scan(&existingFingerprint, &existingStatus)
	if existingErr == nil {
		if existingFingerprint != version.IndexFingerprint || (existingStatus != RAGDocumentVersionPending && existingStatus != RAGDocumentVersionDone) {
			return false, ErrRAGDocumentVersionConflict
		}
		return true, tx.Commit()
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return false, existingErr
	}
	prepareNewRAGDocumentVersion(version)
	if err = d.createRAGDocumentVersion(ctx, tx, version); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) CompleteRAGGenerationDocument(ctx context.Context, fence RAGPolicySyncFence, docID string, chunkCount int) (bool, error) {
	if strings.TrimSpace(docID) == "" || chunkCount < 0 {
		return false, errors.New("invalid generation document completion")
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
	var docVersion int64
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT doc_version FROM rag_kb_generation_documents WHERE generation_id=%s AND doc_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3)), fence.TargetGenerationID, docID, RAGGenerationDocumentPending).Scan(&docVersion); err != nil {
		return false, scanErr(err)
	}
	var storedChunks int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_chunks WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), docID, docVersion).Scan(&storedChunks); err != nil {
		return false, err
	}
	if storedChunks != chunkCount {
		return false, errors.New("generation SQL chunk validation failed")
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET status=%s,updated_at=%s WHERE doc_id=%s AND doc_version=%s AND status=%s`, d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4)), RAGDocumentVersionDone, docID, docVersion, RAGDocumentVersionPending)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kb_generation_documents SET status=%s,error_code='',error_message='' WHERE generation_id=%s AND doc_id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), RAGGenerationDocumentReady, fence.TargetGenerationID, docID, RAGGenerationDocumentPending)
	if err != nil {
		return false, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return false, nil
	}
	return true, tx.Commit()
}

func (d *DBStore) MarkRAGKBGenerationFailed(ctx context.Context, fence RAGPolicySyncFence, code, message string) (bool, error) {
	code, message = sanitizeRAGEvalError(code, message)
	query := fmt.Sprintf(`UPDATE rag_kb_index_generations SET status=%s,error_code=%s,error_message=%s,lease_owner='',lease_until=NULL WHERE id=%s AND kb_id=%s AND policy_version=%s AND status IN (%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8))
	args := []any{RAGGenerationFailed, code, message, fence.TargetGenerationID, fence.KBID, fence.TargetPolicyVersion, RAGGenerationBuilding, RAGGenerationReady}
	if fence.LeaseOwner != "" {
		query += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM rag_kb_policy_sync_tasks t WHERE t.id=%s AND t.target_generation_id=rag_kb_index_generations.id AND t.status=%s AND t.lease_owner=%s AND t.fence_token=%s AND t.lease_until>%s)`, d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ragNowExpr())
		args = append(args, fence.TaskID, RAGPolicySyncRunning, fence.LeaseOwner, fence.FenceToken)
	} else {
		query += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM rag_kb_policy_sync_tasks t WHERE t.id=%s AND t.target_generation_id=rag_kb_index_generations.id AND t.status=%s)`, d.ph(9), d.ph(10))
		args = append(args, fence.TaskID, RAGPolicySyncCancelled)
	}
	result, err := d.db.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) ListRAGKBGenerationGCCandidates(ctx context.Context, limit int) ([]RAGKBGenerationRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_kb_index_generations g
		WHERE (g.status=%s OR (g.status=%s AND g.rollback_until IS NOT NULL AND g.rollback_until<=%s))
		AND NOT EXISTS (SELECT 1 FROM rag_kbs k WHERE k.active_generation_id=g.id)
		AND NOT EXISTS (SELECT 1 FROM rag_kb_policy_sync_tasks t WHERE t.status IN (%s,%s) AND (t.source_generation_id=g.id OR t.target_generation_id=g.id))
		ORDER BY g.created_at,g.id LIMIT %s`, "g."+strings.ReplaceAll(ragKBGenerationColumns, ",", ",g."), d.ph(1), d.ph(2), d.ragNowExpr(), d.ph(3), d.ph(4), d.ph(5)), RAGGenerationFailed, RAGGenerationRetired, RAGPolicySyncQueued, RAGPolicySyncRunning, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RAGKBGenerationRecord
	for rows.Next() {
		item, scanErr := scanRAGKBGeneration(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

func (d *DBStore) DeleteRAGKBGenerationIfCollectible(ctx context.Context, generationID string) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`SELECT %s FROM rag_kb_index_generations WHERE id=%s`, ragKBGenerationColumns, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	generation, err := scanRAGKBGeneration(tx.QueryRowContext(ctx, query, generationID))
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	collectible := generation.Status == RAGGenerationFailed || (generation.Status == RAGGenerationRetired && generation.RollbackUntil.Valid && !generation.RollbackUntil.Time.After(now))
	if !collectible {
		return false, nil
	}
	var refs int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT
		(SELECT COUNT(*) FROM rag_kbs WHERE active_generation_id=%s)+
		(SELECT COUNT(*) FROM rag_kb_policy_sync_tasks WHERE status IN (%s,%s) AND (source_generation_id=%s OR target_generation_id=%s))`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), generationID, RAGPolicySyncQueued, RAGPolicySyncRunning, generationID, generationID).Scan(&refs); err != nil || refs != 0 {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT generation_id,doc_id,doc_version,status,error_code,error_message FROM rag_kb_generation_documents WHERE generation_id=%s ORDER BY doc_id`, d.ph(1)), generationID)
	if err != nil {
		return false, err
	}
	var documents []RAGGenerationDocumentRecord
	for rows.Next() {
		var document RAGGenerationDocumentRecord
		if err = rows.Scan(&document.GenerationID, &document.DocID, &document.DocVersion, &document.Status, &document.ErrorCode, &document.ErrorMessage); err != nil {
			rows.Close()
			return false, err
		}
		documents = append(documents, document)
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	for _, document := range documents {
		var otherRefs int
		if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_kb_generation_documents WHERE doc_id=%s AND doc_version=%s AND generation_id<>%s`, d.ph(1), d.ph(2), d.ph(3)), document.DocID, document.DocVersion, generationID).Scan(&otherRefs); err != nil {
			return false, err
		}
		if otherRefs == 0 {
			if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_chunks WHERE doc_id=%s AND doc_version=%s AND NOT EXISTS (SELECT 1 FROM rag_documents d WHERE d.id=%s AND d.active_version=%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), document.DocID, document.DocVersion, document.DocID, document.DocVersion); err != nil {
				return false, err
			}
			if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_document_versions WHERE doc_id=%s AND doc_version=%s AND NOT EXISTS (SELECT 1 FROM rag_documents d WHERE d.id=%s AND d.active_version=%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)), document.DocID, document.DocVersion, document.DocID, document.DocVersion); err != nil {
				return false, err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_kb_generation_documents WHERE generation_id=%s`, d.ph(1)), generationID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_kb_index_generations WHERE id=%s`, d.ph(1)), generationID)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	return true, tx.Commit()
}

func (d *DBStore) AbandonUnreferencedRAGKBGeneration(ctx context.Context, generationID string) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM rag_kb_index_generations WHERE id=%s`, d.ph(1)), generationID).Scan(&status); errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil || status != RAGGenerationBuilding {
		return false, err
	}
	var refs int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_kb_policy_sync_tasks WHERE target_generation_id=%s`, d.ph(1)), generationID).Scan(&refs); err != nil || refs != 0 {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_kb_generation_documents WHERE generation_id=%s`, d.ph(1)), generationID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_kb_index_generations WHERE id=%s AND status=%s`, d.ph(1), d.ph(2)), generationID, RAGGenerationBuilding)
	if err != nil {
		return false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return false, nil
	}
	return true, tx.Commit()
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
