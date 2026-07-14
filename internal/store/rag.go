package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RAGKBRecord is one user-owned knowledge base. The embedding provider,
// model, and dimensions are snapshotted when the KB is created and are not
// changed by UpdateRAGKB.
type RAGKBRecord struct {
	ID            string
	UserID        string
	Name          string
	Description   string
	EmbedProvider string
	EmbedModel    string
	EmbedDims     int
	ChunkSize     int
	ChunkOverlap  int
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RAGDocumentRecord tracks an uploaded source document and its current index
// version. Chunks themselves live in the vector store rather than this table.
type RAGDocumentRecord struct {
	ID         string
	KBID       string
	FileName   string
	FileType   string
	FileSize   int64
	ObjectKey  string
	Status     string
	ErrorMsg   string
	ChunkCount int
	TokenCount int
	Version    int
	UploadedAt time.Time
	IndexedAt  *time.Time
}

// RAGIndexTaskRecord is the durable recovery record for asynchronous document
// indexing. PENDING and RUNNING rows are both recoverable after a restart.
type RAGIndexTaskRecord struct {
	ID         int64
	DocID      string
	Status     string
	RetryCount int
	MaxRetry   int
	ErrorMsg   string
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
}

const ragKBColumns = `id, user_id, name, description, embed_provider, embed_model,
	embed_dims, chunk_size, chunk_overlap, status, created_at, updated_at`

const ragDocumentColumns = `id, kb_id, file_name, file_type, file_size, object_key,
	status, error_msg, chunk_count, token_count, version, uploaded_at, indexed_at`

const ragIndexTaskColumns = `id, doc_id, status, retry_count, max_retry, error_msg,
	created_at, started_at, finished_at`

type ragScanner interface {
	Scan(dest ...any) error
}

type ragExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func scanRAGKB(scanner ragScanner) (*RAGKBRecord, error) {
	var kb RAGKBRecord
	if err := scanner.Scan(
		&kb.ID, &kb.UserID, &kb.Name, &kb.Description, &kb.EmbedProvider,
		&kb.EmbedModel, &kb.EmbedDims, &kb.ChunkSize, &kb.ChunkOverlap,
		&kb.Status, &kb.CreatedAt, &kb.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &kb, nil
}

func scanRAGDocument(scanner ragScanner) (*RAGDocumentRecord, error) {
	var doc RAGDocumentRecord
	var indexedAt sql.NullTime
	if err := scanner.Scan(
		&doc.ID, &doc.KBID, &doc.FileName, &doc.FileType, &doc.FileSize,
		&doc.ObjectKey, &doc.Status, &doc.ErrorMsg, &doc.ChunkCount,
		&doc.TokenCount, &doc.Version, &doc.UploadedAt, &indexedAt,
	); err != nil {
		return nil, err
	}
	if indexedAt.Valid {
		doc.IndexedAt = &indexedAt.Time
	}
	return &doc, nil
}

func scanRAGIndexTask(scanner ragScanner) (*RAGIndexTaskRecord, error) {
	var task RAGIndexTaskRecord
	var startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&task.ID, &task.DocID, &task.Status, &task.RetryCount, &task.MaxRetry,
		&task.ErrorMsg, &task.CreatedAt, &startedAt, &finishedAt,
	); err != nil {
		return nil, err
	}
	if startedAt.Valid {
		task.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		task.FinishedAt = &finishedAt.Time
	}
	return &task, nil
}

func (d *DBStore) CreateRAGKB(ctx context.Context, kb *RAGKBRecord) error {
	now := time.Now().UTC()
	if kb.CreatedAt.IsZero() {
		kb.CreatedAt = now
	}
	kb.UpdatedAt = now
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kbs
		(id, user_id, name, description, embed_provider, embed_model, embed_dims,
		 chunk_size, chunk_overlap, status, created_at, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12)),
		kb.ID, kb.UserID, kb.Name, kb.Description, kb.EmbedProvider,
		kb.EmbedModel, kb.EmbedDims, kb.ChunkSize, kb.ChunkOverlap, kb.Status,
		kb.CreatedAt, kb.UpdatedAt)
	return err
}

func (d *DBStore) GetRAGKB(ctx context.Context, id string) (*RAGKBRecord, error) {
	kb, err := scanRAGKB(d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id = %s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return kb, nil
}

func (d *DBStore) ListRAGKBsByUser(ctx context.Context, userID string) ([]RAGKBRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragKBColumns+` FROM rag_kbs WHERE user_id = %s ORDER BY created_at, id`,
		d.ph(1)), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RAGKBRecord
	for rows.Next() {
		kb, err := scanRAGKB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *kb)
	}
	return out, rows.Err()
}

func (d *DBStore) UpdateRAGKB(ctx context.Context, kb *RAGKBRecord) error {
	kb.UpdatedAt = time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET
		name=%s, description=%s, chunk_size=%s, chunk_overlap=%s, status=%s,
		updated_at=%s WHERE id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		kb.Name, kb.Description, kb.ChunkSize, kb.ChunkOverlap, kb.Status,
		kb.UpdatedAt, kb.ID)
	return ragMutationResult(result, err)
}

func (d *DBStore) DeleteRAGKB(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM rag_index_tasks WHERE doc_id IN (SELECT id FROM rag_documents WHERE kb_id = %s)`,
		d.ph(1)), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_documents WHERE kb_id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_kbs WHERE id = %s`, d.ph(1)), id)
	if err != nil {
		return err
	}
	if err := ragMutationResult(result, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) CreateRAGDocument(ctx context.Context, doc *RAGDocumentRecord) error {
	return d.createRAGDocument(ctx, d.db, doc)
}

func (d *DBStore) createRAGDocument(ctx context.Context, exec ragExecutor, doc *RAGDocumentRecord) error {
	if doc.UploadedAt.IsZero() {
		doc.UploadedAt = time.Now().UTC()
	}
	_, err := exec.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_documents
		(id, kb_id, file_name, file_type, file_size, object_key, status, error_msg,
		 chunk_count, token_count, version, uploaded_at, indexed_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7),
		d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)),
		doc.ID, doc.KBID, doc.FileName, doc.FileType, doc.FileSize, doc.ObjectKey,
		doc.Status, doc.ErrorMsg, doc.ChunkCount, doc.TokenCount, doc.Version,
		doc.UploadedAt, doc.IndexedAt)
	return err
}

// CreateRAGDocumentWithIndexTask atomically persists the uploaded document and
// its durable recovery task. A process crash can therefore never leave a
// PENDING document without a task row.
func (d *DBStore) CreateRAGDocumentWithIndexTask(ctx context.Context, doc *RAGDocumentRecord, maxRetry int) (int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := d.createRAGDocument(ctx, tx, doc); err != nil {
		return 0, err
	}
	taskID, err := d.createRAGIndexTask(ctx, tx, doc.ID, maxRetry)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return taskID, nil
}

func (d *DBStore) GetRAGDocument(ctx context.Context, id string) (*RAGDocumentRecord, error) {
	doc, err := scanRAGDocument(d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE id = %s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return doc, nil
}

func (d *DBStore) ListRAGDocumentsByKB(ctx context.Context, kbID string) ([]RAGDocumentRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE kb_id = %s ORDER BY uploaded_at, id`,
		d.ph(1)), kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RAGDocumentRecord
	for rows.Next() {
		doc, err := scanRAGDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *doc)
	}
	return out, rows.Err()
}

func (d *DBStore) UpdateRAGDocument(ctx context.Context, doc *RAGDocumentRecord) error {
	return d.updateRAGDocument(ctx, d.db, doc)
}

func (d *DBStore) updateRAGDocument(ctx context.Context, exec ragExecutor, doc *RAGDocumentRecord) error {
	result, err := exec.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		file_name=%s, file_type=%s, file_size=%s, object_key=%s, status=%s,
		error_msg=%s, chunk_count=%s, token_count=%s, version=%s,
		uploaded_at=%s, indexed_at=%s WHERE id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12)),
		doc.FileName, doc.FileType, doc.FileSize, doc.ObjectKey, doc.Status,
		doc.ErrorMsg, doc.ChunkCount, doc.TokenCount, doc.Version, doc.UploadedAt,
		doc.IndexedAt, doc.ID)
	return ragMutationResult(result, err)
}

// UpdateRAGDocumentIfVersion applies worker state only while the document is
// still on the version that worker loaded. It prevents an older task running in
// another process from rolling a newer reindex version back.
func (d *DBStore) UpdateRAGDocumentIfVersion(ctx context.Context, doc *RAGDocumentRecord, expectedVersion int) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		file_name=%s, file_type=%s, file_size=%s, object_key=%s, status=%s,
		error_msg=%s, chunk_count=%s, token_count=%s, version=%s,
		uploaded_at=%s, indexed_at=%s WHERE id=%s AND version=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)),
		doc.FileName, doc.FileType, doc.FileSize, doc.ObjectKey, doc.Status,
		doc.ErrorMsg, doc.ChunkCount, doc.TokenCount, doc.Version, doc.UploadedAt,
		doc.IndexedAt, doc.ID, expectedVersion)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// UpdateRAGDocumentWithIndexTask atomically advances a document version and
// creates the corresponding durable task. This closes the crash window between
// the two writes during reindex.
func (d *DBStore) UpdateRAGDocumentWithIndexTask(ctx context.Context, doc *RAGDocumentRecord, maxRetry int) (int64, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := d.updateRAGDocument(ctx, tx, doc); err != nil {
		return 0, err
	}
	taskID, err := d.createRAGIndexTask(ctx, tx, doc.ID, maxRetry)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return taskID, nil
}

func (d *DBStore) DeleteRAGDocument(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_index_tasks WHERE doc_id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_documents WHERE id = %s`, d.ph(1)), id)
	if err != nil {
		return err
	}
	if err := ragMutationResult(result, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) CreateRAGIndexTask(ctx context.Context, docID string, maxRetry int) (int64, error) {
	return d.createRAGIndexTask(ctx, d.db, docID, maxRetry)
}

func (d *DBStore) createRAGIndexTask(ctx context.Context, exec ragExecutor, docID string, maxRetry int) (int64, error) {
	if maxRetry <= 0 {
		maxRetry = 3
	}
	now := time.Now().UTC()
	if d.dialect == "postgres" {
		var id int64
		err := exec.QueryRowContext(ctx, fmt.Sprintf(`INSERT INTO rag_index_tasks
			(doc_id, status, retry_count, max_retry, error_msg, created_at)
			VALUES (%s, 'PENDING', 0, %s, '', %s) RETURNING id`,
			d.ph(1), d.ph(2), d.ph(3)), docID, maxRetry, now).Scan(&id)
		return id, err
	}

	result, err := exec.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id, status, retry_count, max_retry, error_msg, created_at)
		VALUES (?, 'PENDING', 0, ?, '', ?)`, docID, maxRetry, now)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (d *DBStore) GetRAGIndexTask(ctx context.Context, id int64) (*RAGIndexTaskRecord, error) {
	task, err := scanRAGIndexTask(d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks WHERE id = %s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return task, nil
}

func (d *DBStore) UpdateRAGIndexTask(ctx context.Context, id int64, status string, retryCount int, errMsg string) error {
	now := time.Now().UTC()
	var query string
	var args []any
	switch status {
	case "RUNNING":
		query = fmt.Sprintf(`UPDATE rag_index_tasks SET status=%s, retry_count=%s,
			error_msg=%s, started_at=%s, finished_at=NULL WHERE id=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
		args = []any{status, retryCount, errMsg, now, id}
	case "DONE", "FAILED":
		query = fmt.Sprintf(`UPDATE rag_index_tasks SET status=%s, retry_count=%s,
			error_msg=%s, finished_at=%s WHERE id=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
		args = []any{status, retryCount, errMsg, now, id}
	default:
		query = fmt.Sprintf(`UPDATE rag_index_tasks SET status=%s, retry_count=%s,
			error_msg=%s, finished_at=NULL WHERE id=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4))
		args = []any{status, retryCount, errMsg, id}
	}
	result, err := d.db.ExecContext(ctx, query, args...)
	return ragMutationResult(result, err)
}

func ragMutationResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DBStore) ListRunnableRAGIndexTasks(ctx context.Context) ([]RAGIndexTaskRecord, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragIndexTaskColumns+`
		FROM rag_index_tasks WHERE status IN ('PENDING', 'RUNNING') ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RAGIndexTaskRecord
	for rows.Next() {
		task, err := scanRAGIndexTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *task)
	}
	return out, rows.Err()
}
