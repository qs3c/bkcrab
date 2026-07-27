package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func sameImmutableRAGAttachment(a, b *RAGAttachmentRecord) bool {
	return a.ID == b.ID && a.DocID == b.DocID &&
		a.ContentSHA256 == b.ContentSHA256 && a.Kind == b.Kind &&
		a.FileName == b.FileName && a.MIMEType == b.MIMEType &&
		a.ObjectKey == b.ObjectKey && a.ByteSize == b.ByteSize
}

func (d *DBStore) UpsertRAGAttachment(ctx context.Context, attachment *RAGAttachmentRecord) error {
	if attachment == nil || strings.TrimSpace(attachment.ID) == "" ||
		strings.TrimSpace(attachment.DocID) == "" ||
		!ragCanonicalSHA256(attachment.ContentSHA256) ||
		strings.TrimSpace(attachment.Kind) == "" ||
		strings.TrimSpace(attachment.FileName) == "" ||
		strings.TrimSpace(attachment.MIMEType) == "" ||
		strings.TrimSpace(attachment.ObjectKey) == "" ||
		attachment.ByteSize < 1 || attachment.FirstSeenVersion < 1 ||
		attachment.LastSeenVersion < attachment.FirstSeenVersion {
		return ErrRAGAttachmentConflict
	}
	route, err := d.ragOwnershipRoute(ctx, attachment.DocID)
	if errors.Is(err, ErrNotFound) {
		return ErrRAGLifecycleInactive
	}
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, attachment.DocID, route); err != nil {
		return err
	} else if !active {
		return ErrRAGLifecycleInactive
	}
	if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, attachment.DocID); err != nil {
		return err
	}
	if err := d.publishRAGAttachmentInTx(ctx, tx, attachment); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) publishRAGAttachmentInTx(
	ctx context.Context,
	tx *sql.Tx,
	attachment *RAGAttachmentRecord,
) error {
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return err
	}
	attachment.CreatedAt = now
	attachment.UpdatedAt = now
	if err := d.consumeRAGObjectWritesInTx(ctx, tx, attachment.DocID, attachment.ObjectKey); err != nil {
		return err
	}
	query := fmt.Sprintf(`INSERT INTO rag_attachments (
		id, doc_id, content_sha256, kind, file_name, mime_type, object_key,
		byte_size, first_seen_version, last_seen_version, created_at, updated_at)
		VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE id=id`
	} else {
		query += ` ON CONFLICT DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, query,
		attachment.ID, attachment.DocID, attachment.ContentSHA256,
		attachment.Kind, attachment.FileName, attachment.MIMEType,
		attachment.ObjectKey, attachment.ByteSize, attachment.FirstSeenVersion,
		attachment.LastSeenVersion, attachment.CreatedAt, attachment.UpdatedAt,
	); err != nil {
		return err
	}
	existing, err := scanRAGAttachment(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragAttachmentColumns+` FROM rag_attachments
		 WHERE doc_id=%s AND content_sha256=%s`, d.ph(1), d.ph(2)),
		attachment.DocID, attachment.ContentSHA256))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRAGAttachmentConflict
	}
	if err != nil {
		return err
	}
	if !sameImmutableRAGAttachment(existing, attachment) {
		return ErrRAGAttachmentConflict
	}
	updatedAt, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_attachments SET
		first_seen_version=CASE WHEN first_seen_version < %s THEN first_seen_version ELSE %s END,
		last_seen_version=CASE WHEN last_seen_version > %s THEN last_seen_version ELSE %s END,
		updated_at=%s WHERE id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		attachment.FirstSeenVersion, attachment.FirstSeenVersion,
		attachment.LastSeenVersion, attachment.LastSeenVersion,
		updatedAt, existing.ID)
	if err := ragMutationResult(result, err); err != nil {
		return err
	}
	attachment.FirstSeenVersion = minInt64(existing.FirstSeenVersion, attachment.FirstSeenVersion)
	attachment.LastSeenVersion = maxInt64(existing.LastSeenVersion, attachment.LastSeenVersion)
	attachment.CreatedAt = existing.CreatedAt
	attachment.UpdatedAt = updatedAt
	return nil
}

func (d *DBStore) GetRAGAttachment(ctx context.Context, id string) (*RAGAttachmentRecord, error) {
	attachment, err := scanRAGAttachment(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragAttachmentColumns+` FROM rag_attachments WHERE id=%s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return attachment, nil
}

func (d *DBStore) ListRAGAttachmentsByIDs(ctx context.Context, ids []string) ([]RAGAttachmentRecord, error) {
	if len(ids) == 0 {
		return []RAGAttachmentRecord{}, nil
	}
	if len(ids) > maxRAGBatchRecords {
		return nil, ErrRAGBatchTooLarge
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = d.ph(i + 1)
		args[i] = id
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragAttachmentColumns+
		` FROM rag_attachments WHERE id IN (`+strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGAttachmentRecord, 0, len(ids))
	for rows.Next() {
		attachment, err := scanRAGAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *attachment)
	}
	return out, rows.Err()
}

func (d *DBStore) ListRAGAttachmentsByDocument(ctx context.Context, docID string) ([]RAGAttachmentRecord, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragAttachmentColumns+
		` FROM rag_attachments WHERE doc_id=`+d.ph(1)+` ORDER BY id`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGAttachmentRecord, 0)
	for rows.Next() {
		attachment, err := scanRAGAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *attachment)
	}
	return out, rows.Err()
}

func (d *DBStore) DeleteRAGAttachmentsByDocument(ctx context.Context, docID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rag_version_attachments WHERE doc_id=`+d.ph(1), docID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rag_attachments WHERE doc_id=`+d.ph(1), docID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) IsRAGAttachmentInVersion(
	ctx context.Context,
	docID string,
	docVersion int64,
	attachmentID string,
) (bool, error) {
	if strings.TrimSpace(docID) == "" || docVersion < 1 ||
		strings.TrimSpace(attachmentID) == "" {
		return false, nil
	}
	var count int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
		FROM rag_version_attachments WHERE doc_id=%s AND doc_version=%s
		AND attachment_id=%s`, d.ph(1), d.ph(2), d.ph(3)),
		docID, docVersion, attachmentID).Scan(&count)
	return count > 0, err
}

func (d *DBStore) touchRAGVersionAttachmentsBeforeRemoval(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
	docVersion int64,
) error {
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_attachments SET updated_at=%s
		WHERE doc_id=%s AND id IN (SELECT attachment_id FROM rag_version_attachments
			WHERE doc_id=%s AND doc_version=%s)`, d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3)),
		docID, docID, docVersion)
	return err
}

func (d *DBStore) replaceRAGVersionAttachmentsInTx(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
	docVersion int64,
	attachmentIDs []string,
) error {
	if err := d.touchRAGVersionAttachmentsBeforeRemoval(ctx, tx, docID, docVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_attachments
		WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), docID, docVersion); err != nil {
		return err
	}
	if len(attachmentIDs) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO rag_version_attachments
		(doc_id,doc_version,attachment_id) VALUES (%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3)))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, id := range attachmentIDs {
		if _, err := stmt.ExecContext(ctx, docID, docVersion, id); err != nil {
			return err
		}
	}
	return nil
}

func (d *DBStore) ReplaceRAGVersionAttachments(
	ctx context.Context,
	docID string,
	docVersion int64,
	attachmentIDs []string,
) error {
	docID = strings.TrimSpace(docID)
	if docID == "" || docVersion < 1 || len(attachmentIDs) > maxRAGBatchRecords {
		return ErrRAGDocumentVersionMismatch
	}
	unique := make([]string, 0, len(attachmentIDs))
	seen := make(map[string]struct{}, len(attachmentIDs))
	for _, id := range attachmentIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			return ErrRAGAttachmentConflict
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	route, err := d.ragOwnershipRoute(ctx, docID)
	if errors.Is(err, ErrNotFound) {
		return ErrRAGLifecycleInactive
	}
	if err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, docID, route); err != nil {
		return err
	} else if !active {
		return ErrRAGLifecycleInactive
	}
	if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, docID); err != nil {
		return err
	}
	if err := d.replaceRAGVersionAttachmentsInTx(ctx, tx, docID, docVersion, unique); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishRAGAssetsAndAttachmentsForIndex atomically publishes both binary
// catalogs and their exact version memberships under one live index fence.
func (d *DBStore) PublishRAGAssetsAndAttachmentsForIndex(
	ctx context.Context,
	fence IndexFence,
	assets []RAGAssetRecord,
	assetIDs []string,
	attachments []RAGAttachmentRecord,
	attachmentIDs []string,
) (bool, error) {
	if len(assets) > maxRAGBatchRecords || len(assetIDs) > maxRAGBatchRecords ||
		len(attachments) > maxRAGBatchRecords || len(attachmentIDs) > maxRAGBatchRecords {
		return false, ErrRAGBatchTooLarge
	}
	assetSet := make(map[string]struct{}, len(assets))
	for i := range assets {
		asset := &assets[i]
		asset.ID = strings.TrimSpace(asset.ID)
		asset.DocID = strings.TrimSpace(asset.DocID)
		if asset.ID == "" || asset.DocID != fence.DocID || asset.FirstSeenVersion < 1 ||
			asset.FirstSeenVersion > fence.DocVersion || asset.LastSeenVersion < fence.DocVersion {
			return false, ErrRAGAssetConflict
		}
		if _, duplicate := assetSet[asset.ID]; duplicate {
			return false, ErrRAGAssetConflict
		}
		assetSet[asset.ID] = struct{}{}
	}
	uniqueAssetIDs, err := exactRAGPublicationIDs(assetIDs, assetSet, ErrRAGAssetConflict)
	if err != nil {
		return false, err
	}
	attachmentSet := make(map[string]struct{}, len(attachments))
	for i := range attachments {
		attachment := &attachments[i]
		attachment.ID = strings.TrimSpace(attachment.ID)
		attachment.DocID = strings.TrimSpace(attachment.DocID)
		if attachment.ID == "" || attachment.DocID != fence.DocID ||
			attachment.FirstSeenVersion < 1 ||
			attachment.FirstSeenVersion > fence.DocVersion ||
			attachment.LastSeenVersion < fence.DocVersion {
			return false, ErrRAGAttachmentConflict
		}
		if _, duplicate := attachmentSet[attachment.ID]; duplicate {
			return false, ErrRAGAttachmentConflict
		}
		attachmentSet[attachment.ID] = struct{}{}
	}
	uniqueAttachmentIDs, err := exactRAGPublicationIDs(
		attachmentIDs, attachmentSet, ErrRAGAttachmentConflict)
	if err != nil {
		return false, err
	}

	tx, _, ok, err := d.beginRAGIndexFenceTx(ctx, fence)
	if err != nil || !ok {
		return false, err
	}
	defer tx.Rollback()
	if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, fence.DocID); err != nil {
		return false, err
	}
	for i := range assets {
		if err := d.publishRAGAssetInTx(ctx, tx, &assets[i]); err != nil {
			return false, err
		}
	}
	for i := range attachments {
		if err := d.publishRAGAttachmentInTx(ctx, tx, &attachments[i]); err != nil {
			return false, err
		}
	}
	if err := d.touchRAGVersionAssetsBeforeRemoval(ctx, tx, fence.DocID, fence.DocVersion); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_assets
		WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), fence.DocID, fence.DocVersion); err != nil {
		return false, err
	}
	if len(uniqueAssetIDs) != 0 {
		stmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`INSERT INTO rag_version_assets
			(doc_id,doc_version,asset_id) VALUES (%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3)))
		if err != nil {
			return false, err
		}
		for _, id := range uniqueAssetIDs {
			if _, err := stmt.ExecContext(ctx, fence.DocID, fence.DocVersion, id); err != nil {
				stmt.Close()
				return false, err
			}
		}
		if err := stmt.Close(); err != nil {
			return false, err
		}
	}
	if err := d.replaceRAGVersionAttachmentsInTx(
		ctx, tx, fence.DocID, fence.DocVersion, uniqueAttachmentIDs); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func exactRAGPublicationIDs(ids []string, records map[string]struct{}, conflict error) ([]string, error) {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if _, exists := records[id]; !exists {
			return nil, conflict
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if len(unique) != len(records) {
		return nil, conflict
	}
	return unique, nil
}
