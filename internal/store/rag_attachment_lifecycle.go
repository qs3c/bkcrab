package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (d *DBStore) ragAttachmentReferenceSQL(alias string) string {
	return `EXISTS (SELECT 1 FROM rag_version_attachments va
		JOIN rag_document_versions v ON v.doc_id=va.doc_id AND v.doc_version=va.doc_version
		JOIN rag_documents doc ON doc.id=va.doc_id
		WHERE va.attachment_id=` + alias + `.id AND va.doc_id=` + alias + `.doc_id
		AND (v.status IN ('PENDING','RUNNING','DONE','RETIRED') OR doc.active_version=va.doc_version)) OR
	EXISTS (SELECT 1 FROM rag_chunk_assets ca
		JOIN rag_document_versions v ON v.doc_id=ca.doc_id AND v.doc_version=ca.doc_version
		JOIN rag_documents doc ON doc.id=ca.doc_id
		WHERE ca.attachment_id=` + alias + `.id
		AND (v.status IN ('PENDING','RUNNING','DONE','RETIRED') OR doc.active_version=ca.doc_version)) OR
	EXISTS (SELECT 1 FROM rag_chat_turns t WHERE ` + d.ragTextContains("t.sources", alias+".id") + `) OR
	EXISTS (SELECT 1 FROM session_messages m WHERE m.role='assistant' AND ` +
		d.ragTextContains("m.metadata", alias+".id") + `)`
}

func (d *DBStore) ListRAGStagingAttachmentCleanupCandidates(
	ctx context.Context,
	staleFor time.Duration,
	limit int,
) ([]RAGAttachmentRecord, error) {
	if staleFor < 0 {
		return nil, errors.New("store: RAG attachment stale duration must not be negative")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragAttachmentColumns+`
		FROM rag_attachments a WHERE a.updated_at<=%s AND NOT (%s)
		ORDER BY a.updated_at,a.doc_id,a.id LIMIT %s`, d.ph(1),
		d.ragAttachmentReferenceSQL("a"), d.ph(2)),
		now.Add(-staleFor).UTC(), ragCleanupListLimit(limit))
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

func (d *DBStore) ClaimRAGStagingAttachmentCleanup(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
	attachmentID string,
) (*RAGAttachmentCleanupClaim, bool, error) {
	attachmentID = strings.TrimSpace(attachmentID)
	if strings.TrimSpace(fence.DocID) == "" || attachmentID == "" {
		return nil, false, nil
	}
	route, err := d.ragOwnershipRoute(ctx, fence.DocID)
	if errors.Is(err, ErrNotFound) {
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
	_, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, fence.DocID, route)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRAGLifecycleInactive) {
		return nil, false, nil
	}
	if err != nil || !active {
		return nil, false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	valid, err := d.ragDocumentMaintenanceFenceValidInTx(ctx, tx, fence, now)
	if err != nil || !valid {
		return nil, false, err
	}
	attachment, err := scanRAGAttachment(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragAttachmentColumns+` FROM rag_attachments WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), attachmentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if attachment.DocID != fence.DocID {
		return nil, false, nil
	}
	var references int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_attachments a
		WHERE a.id=%s AND (%s)`, d.ph(1), d.ragAttachmentReferenceSQL("a")),
		attachmentID).Scan(&references); err != nil {
		return nil, false, err
	}
	if references != 0 {
		return nil, false, nil
	}

	objectKey := strings.TrimSpace(attachment.ObjectKey)
	claim := &RAGAttachmentCleanupClaim{
		AttachmentID: attachment.ID,
		DocID:        attachment.DocID,
	}
	if objectKey != "" {
		handleID := ragObjectWriteHandleID(objectKey)
		current, scanErr := scanRAGObjectWrite(tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT `+ragObjectWriteColumns+` FROM rag_object_write_staging
			 WHERE handle_id=%s%s`, d.ph(1), d.ragLockSuffix()), handleID))
		if errors.Is(scanErr, sql.ErrNoRows) {
			current = &RAGObjectWriteFence{
				HandleID: handleID, UserID: route.UserID, KBID: route.KBID,
				DocID: attachment.DocID, ObjectKind: RAGObjectKindAssetAttachment,
				ObjectKey: objectKey, ReferenceKey: attachment.ID,
				Generation: 1, Status: ragObjectWriteDeleting, UpdatedAt: now,
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_object_write_staging
				(handle_id,user_id,kb_id,doc_id,object_kind,object_key,reference_key,
				 generation,status,created_at,updated_at)
				VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
				d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11)),
				current.HandleID, current.UserID, current.KBID, current.DocID,
				current.ObjectKind, current.ObjectKey, current.ReferenceKey,
				current.Generation, current.Status, now, now); err != nil {
				return nil, false, err
			}
		} else {
			if scanErr != nil {
				return nil, false, scanErr
			}
			if current.UserID != route.UserID || current.KBID != route.KBID ||
				current.DocID != attachment.DocID ||
				current.ObjectKind != RAGObjectKindAssetAttachment ||
				current.ObjectKey != objectKey ||
				current.ReferenceKey != attachment.ID {
				return nil, false, ErrRAGDocumentVersionMismatch
			}
			switch current.Status {
			case ragObjectWritePublished:
				nextGeneration := current.Generation + 1
				result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging SET
					generation=%s,status='DELETING',updated_at=%s WHERE handle_id=%s
					AND generation=%s AND status='PUBLISHED'`,
					d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
					nextGeneration, now, current.HandleID, current.Generation)
				if err != nil {
					return nil, false, err
				}
				if updated, err := ragRowsAffected(result); err != nil || !updated {
					return nil, false, err
				}
				current.Generation = nextGeneration
				current.Status = ragObjectWriteDeleting
				current.UpdatedAt = now
			case ragObjectWriteDeleting:
			default:
				return nil, false, ErrRAGDocumentVersionConflict
			}
		}
		claim.ObjectWrites = append(claim.ObjectWrites, *current)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM rag_version_attachments WHERE attachment_id=`+
		d.ph(1), attachment.ID); err != nil {
		return nil, false, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM rag_attachments WHERE id=`+
		d.ph(1)+` AND doc_id=`+d.ph(2), attachment.ID, attachment.DocID)
	if err != nil {
		return nil, false, err
	}
	if deleted, err := ragRowsAffected(result); err != nil || !deleted {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return claim, true, nil
}
