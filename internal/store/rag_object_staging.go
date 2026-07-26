package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	RAGObjectKindOriginal       = "original"
	RAGObjectKindAssetSource    = "asset_source"
	RAGObjectKindAssetDisplay   = "asset_display"
	RAGObjectKindAssetThumbnail = "asset_thumbnail"
	RAGObjectKindAssetAttachment = "asset_attachment"
	RAGObjectKindNormalized     = "normalized"
	RAGObjectKindParsedArtifact = "parsed_artifact"

	ragObjectWriteWriting   = "WRITING"
	ragObjectWriteReady     = "READY"
	ragObjectWritePublished = "PUBLISHED"
	ragObjectWriteDeleting  = "DELETING"

	ragObjectWriteCleanupRetryDelay = 30 * time.Second
)

// RAGObjectWriteRequest creates a durable write-ahead record before an
// external object is written. ReferenceKey groups objects which are published
// by one later SQL reference (for example normalized.md and parsed.json).
type RAGObjectWriteRequest struct {
	UserID       string
	KBID         string
	DocID        string
	ObjectKind   string
	ObjectKey    string
	ReferenceKey string
}

// RAGObjectWriteFence prevents a delayed writer from publishing an object
// after a lifecycle worker has claimed the same durable handle for deletion.
type RAGObjectWriteFence struct {
	HandleID     string
	UserID       string
	KBID         string
	DocID        string
	ObjectKind   string
	ObjectKey    string
	ReferenceKey string
	Generation   int64
	Status       string
	UpdatedAt    time.Time
}

func ragObjectWriteHandleID(objectKey string) string {
	sum := sha256.Sum256([]byte(objectKey))
	return hex.EncodeToString(sum[:])
}

func validRAGObjectKind(kind string) bool {
	switch kind {
	case RAGObjectKindOriginal, RAGObjectKindAssetSource, RAGObjectKindAssetDisplay,
		RAGObjectKindAssetThumbnail, RAGObjectKindAssetAttachment,
		RAGObjectKindNormalized, RAGObjectKindParsedArtifact:
		return true
	default:
		return false
	}
}

func normalizeRAGObjectWriteRequest(request RAGObjectWriteRequest) (RAGObjectWriteRequest, error) {
	request.UserID = strings.TrimSpace(request.UserID)
	request.KBID = strings.TrimSpace(request.KBID)
	request.DocID = strings.TrimSpace(request.DocID)
	request.ObjectKind = strings.TrimSpace(request.ObjectKind)
	request.ObjectKey = strings.TrimSpace(request.ObjectKey)
	request.ReferenceKey = strings.TrimSpace(request.ReferenceKey)
	prefix := fmt.Sprintf("rag/%s/%s/%s/", request.UserID, request.KBID, request.DocID)
	if request.UserID == "" || request.KBID == "" || request.DocID == "" ||
		!validRAGObjectKind(request.ObjectKind) || request.ObjectKey == "" ||
		strings.Contains(request.ObjectKey, "\\") || !strings.HasPrefix(request.ObjectKey, prefix) {
		return RAGObjectWriteRequest{}, ErrRAGDocumentVersionMismatch
	}
	return request, nil
}

const ragObjectWriteColumns = `handle_id,user_id,kb_id,doc_id,object_kind,object_key,reference_key,generation,status,created_at,updated_at`

func scanRAGObjectWrite(scanner ragScanner) (*RAGObjectWriteFence, error) {
	var record RAGObjectWriteFence
	var createdAt time.Time
	if err := scanner.Scan(&record.HandleID, &record.UserID, &record.KBID, &record.DocID,
		&record.ObjectKind, &record.ObjectKey, &record.ReferenceKey, &record.Generation,
		&record.Status, &createdAt, &record.UpdatedAt); err != nil {
		return nil, err
	}
	return &record, nil
}

func (d *DBStore) ragObjectWriteStagingTableSQL() string {
	return `CREATE TABLE IF NOT EXISTS rag_object_write_staging (
		handle_id TEXT PRIMARY KEY,
		user_id TEXT NOT NULL,
		kb_id TEXT NOT NULL,
		doc_id TEXT NOT NULL,
		object_kind TEXT NOT NULL,
		object_key TEXT NOT NULL,
		reference_key TEXT NOT NULL,
		generation BIGINT NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		created_at TIMESTAMP NOT NULL,
		updated_at TIMESTAMP NOT NULL
	)`
}

// BeginRAGObjectWrite durably registers one immutable generation-owned object
// before Put. A physical key is never superseded: callers must allocate a new
// versioned key for a later indexing generation.
func (d *DBStore) BeginRAGObjectWrite(ctx context.Context, request RAGObjectWriteRequest) (*RAGObjectWriteFence, error) {
	request, err := normalizeRAGObjectWriteRequest(request)
	if err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := d.lockActiveRAGKBOwnerTx(ctx, tx, request.KBID, request.UserID); err != nil {
		return nil, err
	}
	doc, docErr := d.ragDocumentInTx(ctx, tx, request.DocID)
	switch {
	case errors.Is(docErr, sql.ErrNoRows) && request.ObjectKind == RAGObjectKindOriginal:
		// The original object is necessarily staged before its document row.
	case errors.Is(docErr, sql.ErrNoRows):
		return nil, ErrRAGLifecycleInactive
	case docErr != nil:
		return nil, docErr
	case doc.KBID != request.KBID || strings.EqualFold(doc.Status, RAGDocumentStatusDeleting):
		return nil, ErrRAGLifecycleInactive
	default:
		if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, request.DocID); err != nil {
			return nil, err
		}
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	handleID := ragObjectWriteHandleID(request.ObjectKey)
	existing, err := scanRAGObjectWrite(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragObjectWriteColumns+` FROM rag_object_write_staging WHERE handle_id=%s%s`,
		d.ph(1), d.ragLockSuffix()), handleID))
	generation := int64(1)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_object_write_staging
			(handle_id,user_id,kb_id,doc_id,object_kind,object_key,reference_key,generation,status,created_at,updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3),
			d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11)),
			handleID, request.UserID, request.KBID, request.DocID, request.ObjectKind,
			request.ObjectKey, request.ReferenceKey, generation, ragObjectWriteWriting, now, now)
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	case existing.UserID != request.UserID || existing.KBID != request.KBID ||
		existing.DocID != request.DocID || existing.ObjectKind != request.ObjectKind ||
		existing.ObjectKey != request.ObjectKey || existing.ReferenceKey != request.ReferenceKey:
		return nil, ErrRAGDocumentVersionMismatch
	case existing.Status == ragObjectWritePublished:
		// PUBLISHED is an immutable object registry, not a reusable writer
		// permit. Callers which reuse a stable asset must do so read-only.
		return nil, ErrRAGDocumentVersionConflict
	case existing.Status == ragObjectWriteDeleting:
		return nil, ErrRAGLifecycleInactive
	default:
		return nil, ErrRAGDocumentVersionConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RAGObjectWriteFence{HandleID: handleID, UserID: request.UserID, KBID: request.KBID,
		DocID: request.DocID, ObjectKind: request.ObjectKind, ObjectKey: request.ObjectKey,
		ReferenceKey: request.ReferenceKey, Generation: generation, Status: ragObjectWriteWriting,
		UpdatedAt: now}, nil
}

func (d *DBStore) MarkRAGObjectWriteReady(ctx context.Context, fence RAGObjectWriteFence) (bool, error) {
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging SET
		status='READY',updated_at=%s WHERE handle_id=%s AND generation=%s AND status='WRITING'`,
		d.ph(1), d.ph(2), d.ph(3)), now, fence.HandleID, fence.Generation)
	if err != nil {
		return false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || updated {
		return updated, err
	}
	var count int
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_object_write_staging
		WHERE handle_id=%s AND generation=%s AND status='PUBLISHED'`, d.ph(1), d.ph(2)),
		fence.HandleID, fence.Generation).Scan(&count)
	return count == 1, err
}

// consumeRAGObjectWritesInTx is called from the SQL transaction which creates
// the durable reference. READY -> PUBLISHED is the writer acknowledgement;
// the row remains as a durable immutable-key registry until whole-document
// finalization. A DELETING/WRITING row makes publication fail closed.
func (d *DBStore) consumeRAGObjectWritesInTx(ctx context.Context, tx *sql.Tx, docID string, objectKeys ...string) error {
	seen := make(map[string]struct{}, len(objectKeys))
	for _, objectKey := range objectKeys {
		objectKey = strings.TrimSpace(objectKey)
		if objectKey == "" {
			continue
		}
		handleID := ragObjectWriteHandleID(objectKey)
		if _, duplicate := seen[handleID]; duplicate {
			continue
		}
		seen[handleID] = struct{}{}
		var rowDocID, storedKey, status string
		var generation int64
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT doc_id,object_key,generation,status
			FROM rag_object_write_staging WHERE handle_id=%s%s`, d.ph(1), d.ragLockSuffix()),
			handleID).Scan(&rowDocID, &storedKey, &generation, &status)
		if errors.Is(err, sql.ErrNoRows) {
			continue // legacy/pre-existing object, not an in-flight external write
		}
		if err != nil {
			return err
		}
		if rowDocID != docID || storedKey != objectKey ||
			(status != ragObjectWriteReady && status != ragObjectWritePublished) {
			return ErrRAGLifecycleInactive
		}
		if status == ragObjectWriteReady {
			result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging
				SET status='PUBLISHED',updated_at=%s WHERE handle_id=%s AND generation=%s
				AND status='READY'`, d.ragNowExpr(), d.ph(1), d.ph(2)), handleID, generation)
			if err != nil {
				return err
			}
			if published, err := ragRowsAffected(result); err != nil || !published {
				return err
			}
		}
	}
	return nil
}

func (d *DBStore) ListRAGObjectWriteCleanupCandidates(ctx context.Context, staleFor time.Duration, limit int) ([]RAGObjectWriteFence, error) {
	if staleFor <= 0 {
		return nil, errors.New("store: RAG object write staging TTL must be positive")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return nil, err
	}
	cutoff := now.Add(-staleFor)
	retryCutoff := now.Add(-ragObjectWriteCleanupRetryDelay)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragObjectWriteColumns+`
		FROM rag_object_write_staging
		WHERE (status IN ('WRITING','READY') AND updated_at<=%s)
		OR (status='DELETING' AND updated_at<=%s)
		ORDER BY updated_at,handle_id LIMIT %s`, d.ph(1), d.ph(2), d.ph(3)),
		cutoff, retryCutoff, ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGObjectWriteFence, 0)
	for rows.Next() {
		record, err := scanRAGObjectWrite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *record)
	}
	return out, rows.Err()
}

func (d *DBStore) ClaimRAGObjectWriteCleanup(ctx context.Context, candidate RAGObjectWriteFence) (*RAGObjectWriteFence, bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	kb, _, err := d.lockRAGKBOwnerTx(ctx, tx, candidate.KBID, candidate.UserID)
	if err != nil {
		return nil, false, err
	}
	if kb != nil {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET version=version
			WHERE id=%s`, d.ph(1)), candidate.DocID); err != nil {
			return nil, false, err
		}
		var docKBID string
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT kb_id FROM rag_documents
			WHERE id=%s%s`, d.ph(1), d.ragLockSuffix()), candidate.DocID).Scan(&docKBID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, err
		}
		if err == nil && docKBID != candidate.KBID {
			return nil, false, ErrRAGLifecycleInactive
		}
	}
	current, err := scanRAGObjectWrite(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragObjectWriteColumns+` FROM rag_object_write_staging
		WHERE handle_id=%s%s`, d.ph(1), d.ragLockSuffix()), candidate.HandleID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if current.Generation != candidate.Generation || current.Status != candidate.Status ||
		current.UserID != candidate.UserID || current.KBID != candidate.KBID ||
		current.DocID != candidate.DocID || current.ObjectKind != candidate.ObjectKind ||
		current.ObjectKey != candidate.ObjectKey || current.ReferenceKey != candidate.ReferenceKey {
		return nil, false, nil
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	referenced, err := d.ragObjectWriteReferencedInTx(ctx, tx, *current)
	if err != nil {
		return nil, false, err
	}
	if referenced {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging
			SET updated_at=%s WHERE handle_id=%s AND generation=%s AND status=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)), now, current.HandleID,
			current.Generation, current.Status); err != nil {
			return nil, false, err
		}
		return nil, false, tx.Commit()
	}
	nextGeneration := candidate.Generation + 1
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging SET
		generation=%s,status='DELETING',updated_at=%s WHERE handle_id=%s AND generation=%s
		AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)), nextGeneration,
		now, candidate.HandleID, candidate.Generation, candidate.Status)
	if err != nil {
		return nil, false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || !updated {
		return nil, false, err
	}
	candidate.Generation = nextGeneration
	candidate.Status = ragObjectWriteDeleting
	candidate.UpdatedAt = now
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return &candidate, true, nil
}

func (d *DBStore) ragObjectWriteReferencedInTx(
	ctx context.Context,
	tx *sql.Tx,
	record RAGObjectWriteFence,
) (bool, error) {
	var count int
	var query string
	var args []any
	switch record.ObjectKind {
	case RAGObjectKindOriginal:
		query = fmt.Sprintf(`SELECT COUNT(*) FROM rag_documents
			WHERE id=%s AND object_key=%s`, d.ph(1), d.ph(2))
		args = []any{record.DocID, record.ObjectKey}
	case RAGObjectKindAssetSource:
		query = fmt.Sprintf(`SELECT COUNT(*) FROM rag_assets
			WHERE doc_id=%s AND source_object_key=%s`, d.ph(1), d.ph(2))
		args = []any{record.DocID, record.ObjectKey}
	case RAGObjectKindAssetDisplay:
		query = fmt.Sprintf(`SELECT COUNT(*) FROM rag_assets
			WHERE doc_id=%s AND display_object_key=%s`, d.ph(1), d.ph(2))
		args = []any{record.DocID, record.ObjectKey}
	case RAGObjectKindAssetThumbnail:
		query = fmt.Sprintf(`SELECT COUNT(*) FROM rag_assets
			WHERE doc_id=%s AND thumbnail_object_key=%s`, d.ph(1), d.ph(2))
		args = []any{record.DocID, record.ObjectKey}
	case RAGObjectKindAssetAttachment:
		query = fmt.Sprintf(`SELECT COUNT(*) FROM rag_attachments
			WHERE doc_id=%s AND object_key=%s`, d.ph(1), d.ph(2))
		args = []any{record.DocID, record.ObjectKey}
	case RAGObjectKindNormalized, RAGObjectKindParsedArtifact:
		query = fmt.Sprintf(`SELECT COUNT(*) FROM rag_document_versions v
			JOIN rag_documents doc ON doc.id=v.doc_id
			WHERE v.doc_id=%s AND v.parse_artifact_key=%s AND
			(v.status IN ('PENDING','RUNNING','DONE','RETIRED') OR doc.active_version=v.doc_version)`,
			d.ph(1), d.ph(2))
		args = []any{record.DocID, record.ReferenceKey}
	default:
		return false, ErrRAGDocumentVersionMismatch
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (d *DBStore) FinishRAGObjectWriteCleanup(ctx context.Context, fence RAGObjectWriteFence) (bool, error) {
	// Keep the tombstone and periodically delete the immutable generation key
	// again. A storage server may complete an old Put after this acknowledgement;
	// retaining the row is what makes that late orphan discoverable forever.
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging
		SET updated_at=%s WHERE handle_id=%s AND generation=%s AND status='DELETING'`,
		d.ragNowExpr(), d.ph(1), d.ph(2)), fence.HandleID, fence.Generation)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}
