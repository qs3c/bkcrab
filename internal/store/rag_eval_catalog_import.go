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

const ragEvalCatalogImportColumns = `id,dataset_id,target_version,catalog_id,request_json,status,stage,progress_json,dataset_version_id,error_code,error_message,created_by,created_at,started_at,finished_at,lease_owner,lease_until,fence_token,cancel_requested_at`

func scanRAGEvalCatalogImport(scanner interface{ Scan(...any) error }) (*RAGEvalCatalogImportRecord, error) {
	var item RAGEvalCatalogImportRecord
	var datasetVersion sql.NullString
	err := scanner.Scan(&item.ID, &item.DatasetID, &item.TargetVersion, &item.CatalogID, &item.RequestJSON, &item.Status, &item.Stage,
		&item.ProgressJSON, &datasetVersion, &item.ErrorCode, &item.ErrorMessage, &item.CreatedBy, &item.CreatedAt, &item.StartedAt,
		&item.FinishedAt, &item.LeaseOwner, &item.LeaseUntil, &item.FenceToken, &item.CancelRequestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	item.DatasetVersionID = datasetVersion.String
	if !validRAGEvalRunStatus(item.Status) || item.Status == RAGEvalRunBudgetExceeded {
		return nil, fmt.Errorf("invalid stored RAG evaluation catalog import status %q", item.Status)
	}
	return &item, nil
}

// CreateRAGEvalCatalogImport reserves the next immutable version number while
// holding the dataset row lock. Failed/cancelled jobs keep their reservation;
// version numbers are never silently reused for different source selectors.
func (d *DBStore) CreateRAGEvalCatalogImport(ctx context.Context, record *RAGEvalCatalogImportRecord) error {
	if record == nil || strings.TrimSpace(record.DatasetID) == "" || strings.TrimSpace(record.CatalogID) == "" ||
		strings.TrimSpace(record.CreatedBy) == "" || !json.Valid([]byte(record.RequestJSON)) {
		return errors.New("valid dataset, catalog request, and creator are required")
	}
	if record.ID == "" {
		record.ID = "rei_" + uuid.NewString()
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lockQuery := fmt.Sprintf(`SELECT id FROM rag_eval_datasets WHERE id=%s AND deleted_at IS NULL`, d.ph(1))
	if d.dialect != "sqlite" {
		lockQuery += " FOR UPDATE"
	}
	var datasetID string
	if err := tx.QueryRowContext(ctx, lockQuery, record.DatasetID).Scan(&datasetID); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var versionMax, importMax int64
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(version),0) FROM rag_eval_dataset_versions WHERE dataset_id=%s`, d.ph(1)), record.DatasetID).Scan(&versionMax); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(target_version),0) FROM rag_eval_catalog_imports WHERE dataset_id=%s`, d.ph(1)), record.DatasetID).Scan(&importMax); err != nil {
		return err
	}
	if importMax > versionMax {
		versionMax = importMax
	}
	record.TargetVersion = versionMax + 1
	record.Status, record.Stage = RAGEvalRunQueued, "queued"
	if !json.Valid([]byte(emptyJSON(record.ProgressJSON))) {
		return errors.New("catalog import progress JSON is invalid")
	}
	record.ProgressJSON = emptyJSON(record.ProgressJSON)
	record.CreatedAt = time.Now().UTC()
	_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_eval_catalog_imports(id,dataset_id,target_version,catalog_id,request_json,status,stage,progress_json,dataset_version_id,error_code,error_message,created_by,created_at,lease_owner) VALUES(%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14)),
		record.ID, record.DatasetID, record.TargetVersion, record.CatalogID, record.RequestJSON, record.Status, record.Stage,
		record.ProgressJSON, nil, "", "", record.CreatedBy, record.CreatedAt, "")
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) GetRAGEvalCatalogImport(ctx context.Context, id string) (*RAGEvalCatalogImportRecord, error) {
	return scanRAGEvalCatalogImport(d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_catalog_imports WHERE id=%s`, ragEvalCatalogImportColumns, d.ph(1)), id))
}

func (d *DBStore) ListRAGEvalCatalogImports(ctx context.Context, cursor string, limit int) ([]RAGEvalCatalogImportRecord, error) {
	limit = boundedRAGEvalListLimit(limit)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM rag_eval_catalog_imports WHERE id>%s ORDER BY id LIMIT %s`, ragEvalCatalogImportColumns, d.ph(1), d.ph(2)), cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RAGEvalCatalogImportRecord{}
	for rows.Next() {
		item, scanErr := scanRAGEvalCatalogImport(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, *item)
	}
	return out, rows.Err()
}

func (d *DBStore) ClaimNextRAGEvalCatalogImport(ctx context.Context, worker string, lease time.Duration) (*RAGEvalCatalogImportFence, bool, error) {
	if strings.TrimSpace(worker) == "" || lease <= 0 {
		return nil, false, errors.New("worker and lease are required")
	}
	now := time.Now().UTC()
	var id string
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT id FROM rag_eval_catalog_imports WHERE
		(status=%s OR (status=%s AND (lease_until IS NULL OR lease_until<=%s))) ORDER BY created_at,id LIMIT 1`,
		d.ph(1), d.ph(2), d.ph(3)), RAGEvalRunQueued, RAGEvalRunRunning, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return d.ClaimRAGEvalCatalogImport(ctx, id, worker, lease)
}

func (d *DBStore) ClaimRAGEvalCatalogImport(ctx context.Context, id, worker string, lease time.Duration) (*RAGEvalCatalogImportFence, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var status string
	var token int64
	var leaseUntil, cancelAt sql.NullTime
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status,fence_token,lease_until,cancel_requested_at FROM rag_eval_catalog_imports WHERE id=%s`, d.ph(1)), id).Scan(&status, &token, &leaseUntil, &cancelAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	if cancelAt.Valid || (status != RAGEvalRunQueued && !(status == RAGEvalRunRunning && (!leaseUntil.Valid || !leaseUntil.Time.After(now)))) {
		return nil, false, nil
	}
	next := token + 1
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_catalog_imports SET status=%s,stage=%s,lease_owner=%s,lease_until=%s,fence_token=%s,started_at=COALESCE(started_at,%s) WHERE id=%s AND fence_token=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)), RAGEvalRunRunning, "preparing", worker, now.Add(lease), next, now, id, token)
	if err != nil {
		return nil, false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return nil, false, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &RAGEvalCatalogImportFence{ImportID: id, LeaseOwner: worker, FenceToken: next}, true, nil
}

func (d *DBStore) HeartbeatRAGEvalCatalogImport(ctx context.Context, fence RAGEvalCatalogImportFence, lease time.Duration) (bool, error) {
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_catalog_imports SET lease_until=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), now.Add(lease), fence.ImportID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) UpdateRAGEvalCatalogImport(ctx context.Context, fence RAGEvalCatalogImportFence, stage, progressJSON string) (bool, error) {
	stage = truncateRAGEvalText(strings.TrimSpace(stage), 64)
	if stage == "" || !json.Valid([]byte(emptyJSON(progressJSON))) {
		return false, errors.New("valid catalog import stage and progress are required")
	}
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_catalog_imports SET stage=%s,progress_json=%s WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)), stage, emptyJSON(progressJSON), fence.ImportID,
		RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) RequestCancelRAGEvalCatalogImport(ctx context.Context, id string) (bool, error) {
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_catalog_imports SET cancel_requested_at=%s,
		status=CASE WHEN status=%s THEN %s ELSE status END,stage=CASE WHEN status=%s THEN %s ELSE stage END,
		finished_at=CASE WHEN status=%s THEN %s ELSE finished_at END
		WHERE id=%s AND status IN (%s,%s) AND cancel_requested_at IS NULL`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)),
		now, RAGEvalRunQueued, RAGEvalRunCancelled, RAGEvalRunQueued, "finished", RAGEvalRunQueued, now, id, RAGEvalRunQueued, RAGEvalRunRunning)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}

func (d *DBStore) FinishRAGEvalCatalogImport(ctx context.Context, fence RAGEvalCatalogImportFence, status, datasetVersionID, errorCode, errorMessage string) (bool, error) {
	if status != RAGEvalRunSucceeded && status != RAGEvalRunFailed && status != RAGEvalRunCancelled {
		return false, errors.New("invalid catalog import terminal status")
	}
	errorCode, errorMessage = sanitizeRAGEvalError(errorCode, errorMessage)
	now := time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_eval_catalog_imports SET status=%s,stage=%s,dataset_version_id=%s,
		error_code=%s,error_message=%s,finished_at=%s,lease_owner='',lease_until=NULL WHERE id=%s AND status=%s AND lease_owner=%s AND fence_token=%s AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11)),
		status, "finished", nullString(datasetVersionID), errorCode, errorMessage, now, fence.ImportID, RAGEvalRunRunning, fence.LeaseOwner, fence.FenceToken, now)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}
