package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func validateRAGExecutionReadLock(
	tx *sql.Tx,
	locked *ragLockedIndexFence,
) error {
	if tx == nil || locked == nil || locked.doc == nil || locked.task == nil ||
		locked.version == nil || locked.doc.ID != locked.task.DocID ||
		locked.doc.Version != locked.task.DocVersion ||
		locked.version.DocID != locked.task.DocID ||
		locked.version.DocVersion != locked.task.DocVersion ||
		locked.doc.KBID == "" || locked.task.UserID == "" {
		return ErrRAGDocumentVersionMismatch
	}
	return nil
}

func (d *DBStore) getRAGDocumentForIndexInTx(
	tx *sql.Tx,
	locked *ragLockedIndexFence,
) (*RAGDocumentRecord, error) {
	if err := validateRAGExecutionReadLock(tx, locked); err != nil {
		return nil, err
	}
	document := *locked.doc
	return &document, nil
}

func (d *DBStore) getRAGKBForIndexInTx(
	ctx context.Context,
	tx *sql.Tx,
	locked *ragLockedIndexFence,
	kbID string,
) (*RAGKBRecord, error) {
	if err := validateRAGExecutionReadLock(tx, locked); err != nil {
		return nil, err
	}
	if kbID == "" || kbID != locked.doc.KBID {
		return nil, ErrRAGDocumentVersionMismatch
	}
	kb, err := scanRAGKB(tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s`, d.ph(1)), kbID))
	if err != nil {
		return nil, scanErr(err)
	}
	if kb.UserID != locked.task.UserID {
		return nil, ErrRAGDocumentVersionMismatch
	}
	return kb, nil
}

func (d *DBStore) getRAGDocumentVersionForIndexInTx(
	ctx context.Context,
	tx *sql.Tx,
	locked *ragLockedIndexFence,
	docID string,
	docVersion int64,
) (*RAGDocumentVersionRecord, error) {
	if err := validateRAGExecutionReadLock(tx, locked); err != nil {
		return nil, err
	}
	if docID == "" || docID != locked.doc.ID || docVersion <= 0 {
		return nil, ErrRAGDocumentVersionMismatch
	}
	if docVersion == locked.task.DocVersion {
		version := *locked.version
		return &version, nil
	}
	if locked.doc.ActiveVersion <= 0 || docVersion != locked.doc.ActiveVersion {
		return nil, ErrRAGDocumentVersionMismatch
	}
	version, err := d.ragVersionInTx(ctx, tx, docID, docVersion)
	if ragIsNoRows(err) {
		return nil, ErrRAGDocumentVersionMismatch
	}
	if err != nil {
		return nil, err
	}
	if version.Status != RAGDocumentVersionDone {
		return nil, ErrRAGDocumentVersionMismatch
	}
	return version, nil
}

func ragExecutionReadIDArguments(
	d *DBStore,
	ids []string,
) (string, []any, error) {
	if len(ids) > maxRAGBatchRecords {
		return "", nil, ErrRAGBatchTooLarge
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for index, id := range ids {
		if id == "" || id != strings.TrimSpace(id) {
			return "", nil, ErrRAGDocumentVersionMismatch
		}
		placeholders[index] = d.ph(index + 1)
		args[index] = id
	}
	return strings.Join(placeholders, ","), args, nil
}

func (d *DBStore) listRAGAssetsByIDsForIndexInTx(
	ctx context.Context,
	tx *sql.Tx,
	locked *ragLockedIndexFence,
	ids []string,
) ([]RAGAssetRecord, error) {
	if err := validateRAGExecutionReadLock(tx, locked); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []RAGAssetRecord{}, nil
	}
	predicate, args, err := ragExecutionReadIDArguments(d, ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+ragAssetColumns+
		` FROM rag_assets WHERE id IN (`+predicate+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	assets := make([]RAGAssetRecord, 0, len(ids))
	for rows.Next() {
		asset, err := scanRAGAsset(rows)
		if err != nil {
			return nil, err
		}
		if asset.DocID != locked.doc.ID {
			return nil, ErrRAGDocumentVersionMismatch
		}
		assets = append(assets, *asset)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return assets, nil
}

func (d *DBStore) listRAGAttachmentsByIDsForIndexInTx(
	ctx context.Context,
	tx *sql.Tx,
	locked *ragLockedIndexFence,
	ids []string,
) ([]RAGAttachmentRecord, error) {
	if err := validateRAGExecutionReadLock(tx, locked); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []RAGAttachmentRecord{}, nil
	}
	predicate, args, err := ragExecutionReadIDArguments(d, ids)
	if err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+ragAttachmentColumns+
		` FROM rag_attachments WHERE id IN (`+predicate+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attachments := make([]RAGAttachmentRecord, 0, len(ids))
	for rows.Next() {
		attachment, err := scanRAGAttachment(rows)
		if err != nil {
			return nil, err
		}
		if attachment.DocID != locked.doc.ID {
			return nil, ErrRAGDocumentVersionMismatch
		}
		attachments = append(attachments, *attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return attachments, nil
}

func (d *DBStore) GetRAGDocumentForIndex(
	ctx context.Context,
	fence IndexFence,
) (*RAGDocumentRecord, error) {
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return nil, ErrFairQueueWriterMismatch
	}
	var document *RAGDocumentRecord
	committed, err := d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			var readErr error
			document, readErr = d.getRAGDocumentForIndexInTx(tx, locked)
			return document != nil, readErr
		})
	if err != nil || !committed {
		return nil, err
	}
	return document, nil
}

func (d *DBStore) GetRAGKBForIndex(
	ctx context.Context,
	fence IndexFence,
	kbID string,
) (*RAGKBRecord, error) {
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return nil, ErrFairQueueWriterMismatch
	}
	var kb *RAGKBRecord
	committed, err := d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			var readErr error
			kb, readErr = d.getRAGKBForIndexInTx(ctx, tx, locked, kbID)
			return kb != nil, readErr
		})
	if err != nil || !committed {
		return nil, err
	}
	return kb, nil
}

func (d *DBStore) GetRAGDocumentVersionForIndex(
	ctx context.Context,
	fence IndexFence,
	docID string,
	docVersion int64,
) (*RAGDocumentVersionRecord, error) {
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return nil, ErrFairQueueWriterMismatch
	}
	var version *RAGDocumentVersionRecord
	committed, err := d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			var readErr error
			version, readErr = d.getRAGDocumentVersionForIndexInTx(
				ctx, tx, locked, docID, docVersion,
			)
			return version != nil, readErr
		})
	if err != nil || !committed {
		return nil, err
	}
	return version, nil
}

func (d *DBStore) ListRAGAssetsByIDsForIndex(
	ctx context.Context,
	fence IndexFence,
	ids []string,
) ([]RAGAssetRecord, error) {
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return nil, ErrFairQueueWriterMismatch
	}
	var assets []RAGAssetRecord
	committed, err := d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			var readErr error
			assets, readErr = d.listRAGAssetsByIDsForIndexInTx(ctx, tx, locked, ids)
			return readErr == nil, readErr
		})
	if err != nil || !committed {
		return nil, err
	}
	return assets, nil
}

func (d *DBStore) ListRAGAttachmentsByIDsForIndex(
	ctx context.Context,
	fence IndexFence,
	ids []string,
) ([]RAGAttachmentRecord, error) {
	if !lowerHex64Pattern.MatchString(fence.ExpectedWriterFingerprint) {
		return nil, ErrFairQueueWriterMismatch
	}
	var attachments []RAGAttachmentRecord
	committed, err := d.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			var readErr error
			attachments, readErr = d.listRAGAttachmentsByIDsForIndexInTx(
				ctx, tx, locked, ids,
			)
			return readErr == nil, readErr
		})
	if err != nil || !committed {
		return nil, err
	}
	return attachments, nil
}

func (d *DBStore) getRAGIndexTaskOn(
	ctx context.Context,
	exec ragExecutor,
	id int64,
) (*RAGIndexTaskRecord, error) {
	if id <= 0 {
		return nil, ErrNotFound
	}
	task, err := scanRAGIndexTask(exec.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks WHERE id=%s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return task, nil
}

// GetRAGIndexTask is an identity-only authoritative task read used by the RAG
// adapter to classify uncertain deliveries without claiming or mutating them.
func (s *RAGFairQueueStore) GetRAGIndexTask(
	ctx context.Context,
	id int64,
) (*RAGIndexTaskRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, ErrNotFound
	}
	var task *RAGIndexTaskRecord
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var readErr error
			task, readErr = s.store.getRAGIndexTaskOn(ctx, conn, id)
			return readErr
		})
	if err != nil {
		return nil, err
	}
	return task, nil
}
