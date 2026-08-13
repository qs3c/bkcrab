package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	RAGEvalGenerationBuilding = "BUILDING"
	RAGEvalGenerationReady    = "READY"
	RAGEvalGenerationFailed   = "FAILED"
	RAGEvalGenerationDeleting = "DELETING"
)

var ErrRAGEvalGenerationConflict = errors.New("store: RAG evaluation generation conflict")

type RAGEvalGenerationRecord struct {
	ID, DatasetVersionID, Fingerprint, CorpusFingerprint, IngestionFingerprint string
	CollectionKey, ObjectPrefix, EmbeddingModel, Status                        string
	EmbeddingDims, DocumentCount, ChunkCount, RefCount, FenceToken             int64
	ErrorCode, ErrorMessage, OwnerRunID, LeaseOwner                            string
	CreatedAt, ExpiresAt                                                       time.Time
	ReadyAt, LeaseUntil                                                        sql.NullTime
}

type RAGEvalGenerationFence struct {
	GenerationID, LeaseOwner, CollectionKey, ObjectPrefix string
	FenceToken                                            int64
}

type RAGEvalGenerationAcquireRequest struct {
	RunID, DatasetVersionID, Fingerprint, CorpusFingerprint, IngestionFingerprint string
	NewGenerationID, CollectionKey, ObjectPrefix, EmbeddingModel, Worker          string
	EmbeddingDims                                                                 int
	Lease, TTL                                                                    time.Duration
}

type RAGEvalGenerationAcquireResult struct {
	Generation *RAGEvalGenerationRecord
	Fence      *RAGEvalGenerationFence
	Claimed    bool
	Reused     bool
}

const ragEvalGenerationColumns = `id,dataset_version_id,fingerprint,corpus_fingerprint,ingestion_fingerprint,collection_key,object_prefix,embedding_model,embedding_dims,status,document_count,chunk_count,ref_count,error_code,error_message,owner_run_id,created_at,ready_at,expires_at,lease_owner,lease_until,fence_token`

func scanRAGEvalGeneration(scanner interface{ Scan(...any) error }) (*RAGEvalGenerationRecord, error) {
	var item RAGEvalGenerationRecord
	err := scanner.Scan(&item.ID, &item.DatasetVersionID, &item.Fingerprint, &item.CorpusFingerprint,
		&item.IngestionFingerprint, &item.CollectionKey, &item.ObjectPrefix, &item.EmbeddingModel,
		&item.EmbeddingDims, &item.Status, &item.DocumentCount, &item.ChunkCount, &item.RefCount,
		&item.ErrorCode, &item.ErrorMessage, &item.OwnerRunID, &item.CreatedAt, &item.ReadyAt,
		&item.ExpiresAt, &item.LeaseOwner, &item.LeaseUntil, &item.FenceToken)
	return &item, err
}

func (d *DBStore) GetRAGEvalGeneration(ctx context.Context, id string) (*RAGEvalGenerationRecord, error) {
	item, err := scanRAGEvalGeneration(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_index_generations WHERE id=%s`, ragEvalGenerationColumns, d.ph(1)), id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return item, err
}

// AttachReadyRAGEvalGenerationForRun binds an ONLINE_ONLY run to one explicit
// READY generation. Dataset identity, run state, generation state and the
// refcount update are checked in the same transaction.
func (d *DBStore) AttachReadyRAGEvalGenerationForRun(ctx context.Context, runID, generationID string) (*RAGEvalGenerationRecord, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(generationID) == "" {
		return nil, errors.New("run and generation are required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	runQuery := fmt.Sprintf(`SELECT dataset_version_id,mode,status,index_generation_id FROM rag_eval_runs WHERE id=%s AND deleted_at IS NULL`, d.ph(1))
	if d.dialect != "sqlite" {
		runQuery += " FOR UPDATE"
	}
	var datasetID, mode, status string
	var current sql.NullString
	if err = tx.QueryRowContext(ctx, runQuery, runID).Scan(&datasetID, &mode, &status, &current); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if mode != RAGEvalRunModeOnlineOnly || (status != RAGEvalRunQueued && status != RAGEvalRunRunning) || (current.Valid && current.String != generationID) {
		return nil, ErrRAGEvalGenerationConflict
	}
	generation, err := scanRAGEvalGeneration(tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_index_generations WHERE id=%s`, ragEvalGenerationColumns, d.ph(1)), generationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if generation.DatasetVersionID != datasetID || generation.Status != RAGEvalGenerationReady {
		return nil, ErrRAGEvalGenerationConflict
	}
	if err = d.attachRAGEvalGenerationRefTx(ctx, tx, runID, generationID, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	generation.RefCount++
	return generation, nil
}

func validateRAGEvalGenerationAcquire(request RAGEvalGenerationAcquireRequest) error {
	for _, value := range []string{request.RunID, request.DatasetVersionID, request.Fingerprint, request.CorpusFingerprint,
		request.IngestionFingerprint, request.NewGenerationID, request.CollectionKey, request.ObjectPrefix,
		request.EmbeddingModel, request.Worker} {
		if strings.TrimSpace(value) == "" {
			return errors.New("evaluation generation acquire identity is required")
		}
	}
	if request.EmbeddingDims < 1 || request.Lease <= 0 || request.TTL <= 0 {
		return errors.New("evaluation generation acquire limits are invalid")
	}
	return nil
}

// AcquireRAGEvalGenerationForRun binds the run reference and either reuses a
// READY generation or claims the unique fingerprint for one BUILDING worker.
// The reference insert and ref_count increment share the run transaction.
func (d *DBStore) AcquireRAGEvalGenerationForRun(ctx context.Context, request RAGEvalGenerationAcquireRequest) (*RAGEvalGenerationAcquireResult, error) {
	if err := validateRAGEvalGenerationAcquire(request); err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	runQuery := fmt.Sprintf(`SELECT dataset_version_id,index_generation_id,status FROM rag_eval_runs WHERE id=%s AND deleted_at IS NULL`, d.ph(1))
	if d.dialect != "sqlite" {
		runQuery += " FOR UPDATE"
	}
	var runDataset, runStatus string
	var runGeneration sql.NullString
	if err = tx.QueryRowContext(ctx, runQuery, request.RunID).Scan(&runDataset, &runGeneration, &runStatus); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	if runDataset != request.DatasetVersionID || (runStatus != RAGEvalRunQueued && runStatus != RAGEvalRunRunning) {
		return nil, ErrRAGEvalGenerationConflict
	}
	var datasetStatus string
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM rag_eval_dataset_versions WHERE id=%s`, d.ph(1)), request.DatasetVersionID).Scan(&datasetStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if datasetStatus != RAGEvalDatasetReady {
		return nil, ErrRAGEvalGenerationConflict
	}

	var generation *RAGEvalGenerationRecord
	if runGeneration.Valid {
		generation, err = scanRAGEvalGeneration(tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_index_generations WHERE id=%s`, ragEvalGenerationColumns, d.ph(1)), runGeneration.String))
	} else {
		query := fmt.Sprintf(`SELECT %s FROM rag_eval_index_generations WHERE fingerprint=%s`, ragEvalGenerationColumns, d.ph(1))
		if d.dialect != "sqlite" {
			query += " FOR UPDATE"
		}
		generation, err = scanRAGEvalGeneration(tx.QueryRowContext(ctx, query, request.Fingerprint))
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		candidate := &RAGEvalGenerationRecord{
			ID: request.NewGenerationID, DatasetVersionID: request.DatasetVersionID, Fingerprint: request.Fingerprint,
			CorpusFingerprint: request.CorpusFingerprint, IngestionFingerprint: request.IngestionFingerprint,
			CollectionKey: request.CollectionKey, ObjectPrefix: request.ObjectPrefix, EmbeddingModel: request.EmbeddingModel,
			EmbeddingDims: int64(request.EmbeddingDims), Status: RAGEvalGenerationBuilding, OwnerRunID: request.RunID,
			CreatedAt: now, ExpiresAt: now.Add(request.TTL), LeaseOwner: request.Worker,
			LeaseUntil: sql.NullTime{Time: now.Add(request.Lease), Valid: true}, FenceToken: 1,
		}
		insertSQL := fmt.Sprintf(`INSERT INTO rag_eval_index_generations(%s) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,0,0,0,'','',%s,%s,NULL,%s,%s,%s,1)`,
			ragEvalGenerationColumns, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15))
		if d.dialect == mysqlDialect {
			insertSQL += ` ON DUPLICATE KEY UPDATE fingerprint=fingerprint`
		} else {
			insertSQL += ` ON CONFLICT DO NOTHING`
		}
		_, err = tx.ExecContext(ctx, insertSQL,
			candidate.ID, candidate.DatasetVersionID, candidate.Fingerprint, candidate.CorpusFingerprint,
			candidate.IngestionFingerprint, candidate.CollectionKey, candidate.ObjectPrefix, candidate.EmbeddingModel,
			candidate.EmbeddingDims, candidate.Status, candidate.OwnerRunID, candidate.CreatedAt, candidate.ExpiresAt,
			candidate.LeaseOwner, candidate.LeaseUntil.Time)
		if err != nil {
			return nil, err
		}
		generation, err = scanRAGEvalGeneration(tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_index_generations WHERE fingerprint=%s`, ragEvalGenerationColumns, d.ph(1)), request.Fingerprint))
		if err != nil {
			return nil, ErrRAGEvalGenerationConflict
		}
	} else if generation.DatasetVersionID != request.DatasetVersionID || generation.Fingerprint != request.Fingerprint ||
		generation.EmbeddingModel != request.EmbeddingModel || generation.EmbeddingDims != int64(request.EmbeddingDims) {
		return nil, ErrRAGEvalGenerationConflict
	}

	if err = d.attachRAGEvalGenerationRefTx(ctx, tx, request.RunID, generation.ID, now); err != nil {
		return nil, err
	}
	result := &RAGEvalGenerationAcquireResult{Generation: generation}
	switch generation.Status {
	case RAGEvalGenerationReady:
		result.Reused = true
	case RAGEvalGenerationBuilding:
		if generation.LeaseOwner == request.Worker && generation.LeaseUntil.Valid && generation.LeaseUntil.Time.After(now) {
			result.Claimed = true
		} else if !generation.LeaseUntil.Valid || !generation.LeaseUntil.Time.After(now) {
			generation.FenceToken++
			generation.LeaseOwner = request.Worker
			generation.LeaseUntil = sql.NullTime{Time: now.Add(request.Lease), Valid: true}
			changed, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET lease_owner=%s,lease_until=%s,fence_token=%s,owner_run_id=%s WHERE id=%s AND status=%s AND fence_token=%s`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), request.Worker,
				generation.LeaseUntil.Time, generation.FenceToken, request.RunID, generation.ID,
				RAGEvalGenerationBuilding, generation.FenceToken-1)
			if updateErr != nil {
				return nil, updateErr
			}
			if rows, _ := changed.RowsAffected(); rows != 1 {
				return nil, ErrRAGEvalFenceLost
			}
			result.Claimed = true
		}
	case RAGEvalGenerationFailed:
		generation.Status = RAGEvalGenerationBuilding
		generation.FenceToken++
		generation.LeaseOwner = request.Worker
		generation.LeaseUntil = sql.NullTime{Time: now.Add(request.Lease), Valid: true}
		changed, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET status=%s,error_code='',error_message='',lease_owner=%s,lease_until=%s,fence_token=%s,owner_run_id=%s WHERE id=%s AND status=%s AND fence_token=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
			RAGEvalGenerationBuilding, request.Worker, generation.LeaseUntil.Time, generation.FenceToken,
			request.RunID, generation.ID, RAGEvalGenerationFailed, generation.FenceToken-1)
		if updateErr != nil {
			return nil, updateErr
		}
		if rows, _ := changed.RowsAffected(); rows != 1 {
			return nil, ErrRAGEvalFenceLost
		}
		result.Claimed = true
	default:
		return nil, ErrRAGEvalGenerationConflict
	}
	if result.Claimed {
		result.Fence = &RAGEvalGenerationFence{GenerationID: generation.ID, LeaseOwner: request.Worker,
			CollectionKey: generation.CollectionKey, ObjectPrefix: generation.ObjectPrefix, FenceToken: generation.FenceToken}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (d *DBStore) attachRAGEvalGenerationRefTx(ctx context.Context, tx *sql.Tx, runID, generationID string, now time.Time) error {
	var existingGeneration string
	var released sql.NullTime
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT generation_id,released_at FROM rag_eval_generation_refs WHERE run_id=%s`, d.ph(1)), runID).Scan(&existingGeneration, &released)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_generation_refs(run_id,generation_id,created_at,released_at) VALUES(%s,%s,%s,NULL)`, d.ph(1), d.ph(2), d.ph(3)), runID, generationID, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET ref_count=ref_count+1 WHERE id=%s`, d.ph(1)), generationID); err != nil {
			return err
		}
	case err != nil:
		return err
	case existingGeneration != generationID:
		return ErrRAGEvalGenerationConflict
	case released.Valid:
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_generation_refs SET released_at=NULL,created_at=%s WHERE run_id=%s AND released_at IS NOT NULL`, d.ph(1), d.ph(2)), now, runID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET ref_count=ref_count+1 WHERE id=%s`, d.ph(1)), generationID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_runs SET index_generation_id=%s WHERE id=%s AND (index_generation_id IS NULL OR index_generation_id=%s)`, d.ph(1), d.ph(2), d.ph(3)), generationID, runID, generationID)
	return err
}

func (d *DBStore) HeartbeatRAGEvalGeneration(ctx context.Context, fence RAGEvalGenerationFence, lease time.Duration) (bool, error) {
	if lease <= 0 {
		return false, errors.New("evaluation generation lease must be positive")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET lease_until=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), now.Add(lease), fence.GenerationID,
		RAGEvalGenerationBuilding, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) MarkRAGEvalGenerationReady(ctx context.Context, fence RAGEvalGenerationFence, documentCount, chunkCount int64, ttl time.Duration) (bool, error) {
	if documentCount < 0 || chunkCount < 0 || ttl <= 0 {
		return false, errors.New("evaluation generation result is invalid")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET status=%s,document_count=%s,chunk_count=%s,ready_at=%s,expires_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)),
		RAGEvalGenerationReady, documentCount, chunkCount, now, now.Add(ttl), fence.GenerationID,
		RAGEvalGenerationBuilding, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) MarkRAGEvalGenerationFailed(ctx context.Context, fence RAGEvalGenerationFence, code, message string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return false, errors.New("evaluation generation failure TTL must be positive")
	}
	code, message = sanitizeRAGEvalError(code, message)
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET status=%s,error_code=%s,error_message=%s,expires_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
		RAGEvalGenerationFailed, code, message, now.Add(ttl), fence.GenerationID,
		RAGEvalGenerationBuilding, fence.LeaseOwner, fence.FenceToken)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (d *DBStore) ReleaseRAGEvalGenerationForRun(ctx context.Context, runID string) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var generationID string
	var released sql.NullTime
	query := fmt.Sprintf(`SELECT generation_id,released_at FROM rag_eval_generation_refs WHERE run_id=%s`, d.ph(1))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	if err = tx.QueryRowContext(ctx, query, runID).Scan(&generationID, &released); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if released.Valid {
		return false, nil
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_generation_refs SET released_at=%s WHERE run_id=%s AND released_at IS NULL`, d.ph(1), d.ph(2)), now, runID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET ref_count=ref_count-1 WHERE id=%s AND ref_count>0`, d.ph(1)), generationID); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ClaimRAGEvalGenerationGC fences deletion only after expiry, zero active
// references, and absence of any retained (non-tombstoned) run reference.
func (d *DBStore) ClaimRAGEvalGenerationGC(ctx context.Context, before time.Time, worker string, lease time.Duration) (*RAGEvalGenerationFence, bool, error) {
	if strings.TrimSpace(worker) == "" || lease <= 0 {
		return nil, false, errors.New("evaluation generation GC worker and lease are required")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	query := fmt.Sprintf(`SELECT %s FROM rag_eval_index_generations g WHERE ((g.status IN (%s,%s) AND g.expires_at<=%s AND g.ref_count=0 AND NOT EXISTS (
		SELECT 1 FROM rag_eval_runs r WHERE r.index_generation_id=g.id AND r.deleted_at IS NULL)) OR (g.status=%s AND g.lease_until<=%s)) ORDER BY g.expires_at,g.id LIMIT 1`,
		ragEvalGenerationColumns, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
	if d.dialect != "sqlite" {
		query += " FOR UPDATE"
	}
	item, err := scanRAGEvalGeneration(tx.QueryRowContext(ctx, query, RAGEvalGenerationReady, RAGEvalGenerationFailed, before, RAGEvalGenerationDeleting, now))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	nextFence := item.FenceToken + 1
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_index_generations SET status=%s,lease_owner=%s,lease_until=%s,fence_token=%s WHERE id=%s AND fence_token=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), RAGEvalGenerationDeleting,
		worker, now.Add(lease), nextFence, item.ID, item.FenceToken)
	if err != nil {
		return nil, false, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return nil, false, nil
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return &RAGEvalGenerationFence{GenerationID: item.ID, LeaseOwner: worker, CollectionKey: item.CollectionKey,
		ObjectPrefix: item.ObjectPrefix, FenceToken: nextFence}, true, nil
}

func (d *DBStore) FinishRAGEvalGenerationGC(ctx context.Context, fence RAGEvalGenerationFence) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	var count int
	if err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_eval_index_generations g WHERE g.id=%s AND g.status=%s AND g.lease_owner=%s AND g.fence_token=%s AND g.lease_until>%s AND g.ref_count=0 AND NOT EXISTS (
		SELECT 1 FROM rag_eval_runs r WHERE r.index_generation_id=g.id AND r.deleted_at IS NULL)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), fence.GenerationID, RAGEvalGenerationDeleting,
		fence.LeaseOwner, fence.FenceToken, now).Scan(&count); err != nil {
		return false, err
	}
	if count != 1 {
		return false, nil
	}
	if _, err = tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_eval_generation_refs WHERE generation_id=%s`, d.ph(1)), fence.GenerationID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_eval_index_generations WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), fence.GenerationID, RAGEvalGenerationDeleting, fence.LeaseOwner, fence.FenceToken)
	if err != nil {
		return false, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return false, nil
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
