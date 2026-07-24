package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RAGIndexGCFence is the independent lease capability for one exact-version
// cleanup attempt. Unlike IndexFence, it never advances rag_documents.version.
type RAGIndexGCFence struct {
	TaskID          int64
	DocID           string
	RetiredVersion  int64
	ClaimGeneration int64
	LeaseOwner      string
}

type RAGIndexGCClaim struct {
	Task  RAGIndexGCTaskRecord
	KBID  string
	Fence RAGIndexGCFence
}

// RAGDocumentMaintenanceFence is a generation-fenced, database-clock lease
// used by cross-instance orphan and staging cleanup. It serializes external
// deletion with new index work and parsed-asset publication for one document.
type RAGDocumentMaintenanceFence struct {
	DocID      string
	Generation int64
	LeaseOwner string
	LeaseUntil time.Time
}

// RAGAssetCleanupClaim is the durable catalog/object handoff for one staging
// asset. The catalog row and every terminal version pin are removed in the
// same transaction which fences its immutable object keys for deletion.
type RAGAssetCleanupClaim struct {
	AssetID      string
	DocID        string
	ObjectWrites []RAGObjectWriteFence
}

type RAGAttachmentCleanupClaim struct {
	AttachmentID string
	DocID        string
	ObjectWrites []RAGObjectWriteFence
}

// lockActiveRAGUserTx serializes every new RAG ownership mutation with the
// durable user tombstone. The no-op UPDATE is intentional: it acquires the
// user row/write lock on all supported dialects before status is inspected.
// Thus either the mutation commits first and user cleanup will discover it,
// or the tombstone commits first and this mutation fails closed.
func (d *DBStore) lockRAGUserTx(ctx context.Context, tx *sql.Tx, userID string) (string, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return "", false, nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE users SET updated_at=updated_at
		WHERE id=%s`, d.ph(1)), userID); err != nil {
		return "", false, err
	}
	var status string
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM users WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), userID).
		Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return status, true, nil
}

func (d *DBStore) lockActiveRAGUserTx(ctx context.Context, tx *sql.Tx, userID string) error {
	status, exists, err := d.lockRAGUserTx(ctx, tx, userID)
	if err != nil {
		return err
	}
	if !exists || !strings.EqualFold(status, "active") {
		return ErrRAGLifecycleInactive
	}
	return nil
}

// lockRAGKBOwnerTx takes locks in the global user -> KB order and uses current
// locking reads on MySQL/PostgreSQL. expectedUserID is a routing key discovered
// before the transaction starts; requiring it avoids establishing a MySQL RR
// snapshot before the serialization locks are acquired.
func (d *DBStore) lockRAGKBOwnerTx(
	ctx context.Context,
	tx *sql.Tx,
	kbID, expectedUserID string,
) (*RAGKBRecord, bool, error) {
	ownerID := strings.TrimSpace(expectedUserID)
	if ownerID == "" {
		return nil, false, ErrRAGLifecycleInactive
	}
	userStatus, userExists, err := d.lockRAGUserTx(ctx, tx, ownerID)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET updated_at=updated_at
		WHERE id=%s`, d.ph(1)), kbID); err != nil {
		return nil, false, err
	}
	kb, err := d.ragKBInTx(ctx, tx, kbID)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if kb.UserID != ownerID {
		return nil, false, ErrRAGLifecycleInactive
	}
	active := userExists && strings.EqualFold(userStatus, "active") &&
		strings.EqualFold(kb.Status, "active")
	return kb, active, nil
}

func (d *DBStore) lockActiveRAGKBOwnerTx(
	ctx context.Context,
	tx *sql.Tx,
	kbID, expectedUserID string,
) (*RAGKBRecord, error) {
	kb, active, err := d.lockRAGKBOwnerTx(ctx, tx, kbID, expectedUserID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrRAGLifecycleInactive
	}
	return kb, nil
}

type ragOwnershipRoute struct {
	KBID   string
	UserID string
}

func (d *DBStore) ragOwnershipRoute(ctx context.Context, docID string) (ragOwnershipRoute, error) {
	var route ragOwnershipRoute
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT d.kb_id,kb.user_id
		FROM rag_documents d JOIN rag_kbs kb ON kb.id=d.kb_id WHERE d.id=%s`, d.ph(1)), docID).
		Scan(&route.KBID, &route.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return ragOwnershipRoute{}, ErrNotFound
	}
	return route, err
}

// lockRAGDocumentHierarchyTx follows the global user -> KB -> document order.
// The routing read must happen before the transaction; every ownership value
// is then revalidated after the serialization locks are held.
func (d *DBStore) lockRAGDocumentHierarchyTx(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
	route ragOwnershipRoute,
) (*RAGDocumentRecord, bool, error) {
	_, active, err := d.lockRAGKBOwnerTx(ctx, tx, route.KBID, route.UserID)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET version=version
		WHERE id=%s`, d.ph(1)), docID); err != nil {
		return nil, false, err
	}
	doc, err := d.ragDocumentInTx(ctx, tx, docID)
	if ragIsNoRows(err) {
		return nil, false, ErrNotFound
	}
	if err != nil {
		return nil, false, err
	}
	if doc.KBID != route.KBID {
		return nil, false, ErrRAGLifecycleInactive
	}
	if strings.EqualFold(doc.Status, RAGDocumentStatusDeleting) {
		active = false
	}
	return doc, active, nil
}

func (d *DBStore) ragDocumentMaintenanceActiveInTx(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
	now time.Time,
) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
		FROM rag_document_maintenance_leases
		WHERE doc_id=%s AND lease_until IS NOT NULL AND lease_until>%s`,
		d.ph(1), d.ph(2)), docID, now).Scan(&count)
	return count > 0, err
}

func (d *DBStore) rejectActiveRAGDocumentMaintenanceInTx(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
) error {
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return err
	}
	active, err := d.ragDocumentMaintenanceActiveInTx(ctx, tx, docID, now)
	if err != nil {
		return err
	}
	if active {
		return ErrRAGDocumentMaintenanceActive
	}
	return nil
}

func (d *DBStore) ragDocumentMaintenanceFenceValidInTx(
	ctx context.Context,
	tx *sql.Tx,
	fence RAGDocumentMaintenanceFence,
	now time.Time,
) (bool, error) {
	var count int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
		FROM rag_document_maintenance_leases WHERE doc_id=%s AND generation=%s
		AND lease_owner=%s AND lease_until IS NOT NULL AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), fence.DocID, fence.Generation,
		fence.LeaseOwner, now).Scan(&count)
	return count == 1, err
}

// ClaimRAGDocumentMaintenance acquires a per-document cleanup lease only when
// the ownership chain is active and no index task can still publish data. New
// enqueue paths take the same document row lock and reject the active lease.
func (d *DBStore) ClaimRAGDocumentMaintenance(
	ctx context.Context,
	docID, leaseOwner string,
	leaseDuration time.Duration,
) (*RAGDocumentMaintenanceFence, error) {
	if !validRAGWorkerID(leaseOwner) {
		return nil, errors.New("store: RAG maintenance owner must be 1..96 trimmed bytes")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("store: RAG maintenance lease duration must be positive")
	}
	route, err := d.ragOwnershipRoute(ctx, docID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, docID, route)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, nil
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	var runnable int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_index_tasks
		WHERE doc_id=%s AND status IN ('PENDING','RUNNING')`, d.ph(1)), docID).Scan(&runnable); err != nil {
		return nil, err
	}
	if runnable != 0 {
		return nil, nil
	}
	quiesced, err := d.ragDocumentCleanupReadyAt(ctx, tx, docID, now)
	if err != nil {
		return nil, err
	}
	if !quiesced {
		return nil, nil
	}
	var generation int64
	var currentOwner string
	var currentUntil sql.NullTime
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT generation,lease_owner,lease_until
		FROM rag_document_maintenance_leases WHERE doc_id=%s%s`,
		d.ph(1), d.ragLockSuffix()), docID).Scan(&generation, &currentOwner, &currentUntil)
	leaseUntil := now.Add(leaseDuration)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		generation = 1
		_, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_document_maintenance_leases
			(doc_id,generation,lease_owner,lease_until) VALUES (%s,%s,%s,%s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)), docID, generation, leaseOwner, leaseUntil)
	case err != nil:
		return nil, err
	case currentUntil.Valid && currentUntil.Time.After(now):
		return nil, nil
	default:
		oldGeneration := generation
		generation++
		result, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_maintenance_leases
			SET generation=%s,lease_owner=%s,lease_until=%s
			WHERE doc_id=%s AND generation=%s AND (lease_until IS NULL OR lease_until<=%s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), generation,
			leaseOwner, leaseUntil, docID, oldGeneration, now)
		if updateErr != nil {
			return nil, updateErr
		}
		if updated, updateErr := ragRowsAffected(result); updateErr != nil || !updated {
			return nil, updateErr
		}
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RAGDocumentMaintenanceFence{
		DocID: docID, Generation: generation, LeaseOwner: leaseOwner, LeaseUntil: leaseUntil,
	}, nil
}

func (d *DBStore) CheckRAGDocumentMaintenance(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
) (bool, error) {
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	var count int
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
		FROM rag_document_maintenance_leases WHERE doc_id=%s AND generation=%s
		AND lease_owner=%s AND lease_until IS NOT NULL AND lease_until>%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4)), fence.DocID, fence.Generation,
		fence.LeaseOwner, now).Scan(&count)
	return count == 1, err
}

func (d *DBStore) HeartbeatRAGDocumentMaintenance(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
	leaseDuration time.Duration,
) (bool, error) {
	if leaseDuration <= 0 {
		return false, errors.New("store: RAG maintenance lease duration must be positive")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_maintenance_leases
		SET lease_until=%s WHERE doc_id=%s AND generation=%s AND lease_owner=%s
		AND lease_until IS NOT NULL AND lease_until>%s`, d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5)), now.Add(leaseDuration), fence.DocID, fence.Generation,
		fence.LeaseOwner, now)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) ReleaseRAGDocumentMaintenance(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_maintenance_leases
		SET lease_owner='',lease_until=NULL WHERE doc_id=%s AND generation=%s AND lease_owner=%s`,
		d.ph(1), d.ph(2), d.ph(3)), fence.DocID, fence.Generation, fence.LeaseOwner)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) ragKBInTx(ctx context.Context, tx *sql.Tx, kbID string) (*RAGKBRecord, error) {
	return scanRAGKB(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), kbID))
}

func (d *DBStore) ragGCTaskInTx(ctx context.Context, tx *sql.Tx, taskID int64) (*RAGIndexGCTaskRecord, error) {
	return scanRAGIndexGCTask(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragIndexGCTaskColumns+` FROM rag_index_gc_tasks WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), taskID))
}

func (d *DBStore) ragOwnershipActiveInTx(
	ctx context.Context,
	tx *sql.Tx,
	doc *RAGDocumentRecord,
) (bool, error) {
	if doc == nil || strings.EqualFold(doc.Status, RAGDocumentStatusDeleting) {
		return false, nil
	}
	var kbStatus string
	var userStatus sql.NullString
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT kb.status,u.status FROM rag_kbs kb
		LEFT JOIN users u ON u.id=kb.user_id WHERE kb.id=%s`, d.ph(1)), doc.KBID).
		Scan(&kbStatus, &userStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return strings.EqualFold(kbStatus, "active") && userStatus.Valid &&
		strings.EqualFold(userStatus.String, "active"), nil
}

func (d *DBStore) ragDeletionTombstonedInTx(
	ctx context.Context,
	tx *sql.Tx,
	doc *RAGDocumentRecord,
) (bool, error) {
	if doc == nil || strings.EqualFold(doc.Status, RAGDocumentStatusDeleting) {
		return true, nil
	}
	var status string
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM rag_kbs WHERE id=%s`, d.ph(1)), doc.KBID).
		Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !strings.EqualFold(status, "active"), nil
}

// MarkRAGDocumentDeleting is the first, authoritative deletion transaction.
// It revokes retrieval visibility and every runnable index lease before any
// caller attempts vector or object-store cleanup.
func (d *DBStore) MarkRAGDocumentDeleting(ctx context.Context, id string) (*RAGDocumentRecord, error) {
	route, err := d.ragOwnershipRoute(ctx, id)
	if err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	doc, _, err := d.lockRAGDocumentHierarchyTx(ctx, tx, id, route)
	if err != nil {
		return nil, err
	}
	if err := d.supersedeRunnableRAGTasksInTx(ctx, tx, doc.ID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		status=%s,active_version=0,processing_stage='deleting',progress_current=0,
		progress_total=0,progress_unit='' WHERE id=%s`, d.ph(1), d.ph(2)),
		RAGDocumentStatusDeleting, doc.ID); err != nil {
		return nil, err
	}
	updated, err := scanRAGDocument(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE id=%s`, d.ph(1)), doc.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// MarkRAGKBDeleting tombstones the KB and every child document in one SQL
// transaction. Runnable versions/tasks are fenced before the transaction is
// committed, so another instance cannot claim or activate them afterwards.
func (d *DBStore) MarkRAGKBDeleting(ctx context.Context, id string) (*RAGKBRecord, error) {
	// Resolve the route before opening the transaction so MySQL does not create
	// a repeatable-read snapshot before the global user -> KB lock sequence.
	var expectedUserID string
	if err := d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT user_id FROM rag_kbs WHERE id=%s`, d.ph(1)), id).Scan(&expectedUserID); err != nil {
		return nil, scanErr(err)
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	kb, _, err := d.lockRAGKBOwnerTx(ctx, tx, id, expectedUserID)
	if kb == nil && err == nil {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET status=%s,
		updated_at=%s WHERE id=%s`, d.ph(1), d.ragNowExpr(), d.ph(2)),
		RAGKBStatusDeleting, kb.ID); err != nil {
		return nil, err
	}

	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragDocumentColumns+`
		FROM rag_documents WHERE kb_id=%s ORDER BY id%s`, d.ph(1), d.ragLockSuffix()), kb.ID)
	if err != nil {
		return nil, err
	}
	docs := make([]RAGDocumentRecord, 0)
	for rows.Next() {
		doc, err := scanRAGDocument(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		docs = append(docs, *doc)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range docs {
		if err := d.supersedeRunnableRAGTasksInTx(ctx, tx, docs[i].ID); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_documents SET
		status=%s,active_version=0,processing_stage='deleting',progress_current=0,
		progress_total=0,progress_unit='' WHERE kb_id=%s`, d.ph(1), d.ph(2)),
		RAGDocumentStatusDeleting, kb.ID); err != nil {
		return nil, err
	}
	updated, err := scanRAGKB(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s`, d.ph(1)), kb.ID))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

func ragCleanupListLimit(limit int) int {
	if limit <= 0 {
		return 100
	}
	if limit > maxRAGBatchRecords {
		return maxRAGBatchRecords
	}
	return limit
}

// IsRAGDocumentCleanupReady keeps a tombstoned document's external prefix in
// place until every worker lease that was revoked by the tombstone has expired.
// Heartbeats fail immediately after supersession and cancel cooperative work;
// the retained lease deadline is the durable cross-process quiescence bound.
func (d *DBStore) IsRAGDocumentCleanupReady(ctx context.Context, docID string) (bool, error) {
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	return d.ragDocumentCleanupReadyAt(ctx, d.db, docID, now)
}

func (d *DBStore) ragDocumentCleanupReadyAt(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	docID string,
	now time.Time,
) (bool, error) {
	var count int
	err := queryer.QueryRowContext(ctx, fmt.Sprintf(`SELECT
		(SELECT COUNT(*) FROM rag_index_tasks WHERE doc_id=%s AND status='SUPERSEDED'
		 AND lease_until IS NOT NULL AND lease_until>%s) +
		(SELECT COUNT(*) FROM rag_document_maintenance_leases WHERE doc_id=%s
		 AND lease_until IS NOT NULL AND lease_until>%s)`, d.ph(1), d.ph(2),
		d.ph(3), d.ph(4)), docID, now, docID, now).Scan(&count)
	return count == 0, err
}

func (d *DBStore) ListDeletingRAGDocuments(
	ctx context.Context,
	afterID string,
	limit int,
) ([]RAGDocumentRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragDocumentColumns+`
		FROM rag_documents WHERE UPPER(status)='DELETING' AND id>%s
		ORDER BY id LIMIT %s`, d.ph(1), d.ph(2)), strings.TrimSpace(afterID),
		ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGDocumentRecord, 0)
	for rows.Next() {
		doc, err := scanRAGDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *doc)
	}
	return out, rows.Err()
}

func (d *DBStore) ListDeletingRAGKBs(
	ctx context.Context,
	afterID string,
	limit int,
) ([]RAGKBRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragKBColumns+`
		FROM rag_kbs WHERE LOWER(status)='deleting' AND id>%s
		ORDER BY id LIMIT %s`, d.ph(1), d.ph(2)), strings.TrimSpace(afterID),
		ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGKBRecord, 0)
	for rows.Next() {
		kb, err := scanRAGKB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *kb)
	}
	return out, rows.Err()
}

func ragGCTaskDue(task *RAGIndexGCTaskRecord, now time.Time) bool {
	if task.NotBefore.After(now) {
		return false
	}
	switch task.Status {
	case "PENDING":
		return task.NextRunAt == nil || !task.NextRunAt.After(now)
	case "RUNNING":
		return task.LeaseUntil == nil || !task.LeaseUntil.After(now)
	default:
		return false
	}
}

// ClaimRAGIndexGCTask claims one due exact-version cleanup task. Reclaim only
// advances the GC task's own generation; it never allocates an index version.
func (d *DBStore) ClaimRAGIndexGCTask(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (*RAGIndexGCClaim, error) {
	if !validRAGWorkerID(workerID) {
		return nil, errors.New("store: RAG GC worker id must be 1..96 trimmed bytes")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("store: RAG GC lease duration must be positive")
	}
	for scanned := 0; scanned < 64; scanned++ {
		claim, consumed, err := d.claimOneRAGIndexGCTask(ctx, workerID, leaseDuration)
		if err != nil {
			if d.dialect == "sqlite" && ragSQLiteBusy(err) {
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(5 * time.Millisecond):
					continue
				}
			}
			return nil, err
		}
		if claim != nil || !consumed {
			return claim, nil
		}
	}
	return nil, errors.New("store: too many invalid RAG GC tasks while claiming")
}

func (d *DBStore) claimOneRAGIndexGCTask(
	ctx context.Context,
	workerID string,
	leaseDuration time.Duration,
) (*RAGIndexGCClaim, bool, error) {
	// SQLite drivers can encode bound TIMESTAMP values differently from the
	// CURRENT_TIMESTAMP text expression. Read a bounded scheduler page and do
	// due comparison against one parsed database clock value in Go. Candidate
	// and ownership reads intentionally happen before the transaction: they are
	// routing hints only, while the transaction below takes the canonical
	// user -> KB -> document -> GC task -> version lock order and revalidates
	// every value before its row CAS.
	hintNow, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return nil, false, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragIndexGCTaskColumns+`
		FROM rag_index_gc_tasks g WHERE g.status IN ('PENDING','RUNNING')
		AND EXISTS (SELECT 1 FROM rag_documents d
			JOIN rag_kbs kb ON kb.id=d.kb_id JOIN users u ON u.id=kb.user_id
			WHERE d.id=g.doc_id AND UPPER(d.status)<>'DELETING'
			AND LOWER(kb.status)='active' AND LOWER(u.status)='active')
		ORDER BY not_before,COALESCE(next_run_at,not_before),created_at,id LIMIT 64`)
	if err != nil {
		return nil, false, err
	}
	candidates := make([]RAGIndexGCTaskRecord, 0, 64)
	for rows.Next() {
		candidate, err := scanRAGIndexGCTask(rows)
		if err != nil {
			rows.Close()
			return nil, false, err
		}
		candidates = append(candidates, *candidate)
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	var hint *RAGIndexGCTaskRecord
	for i := range candidates {
		if ragGCTaskDue(&candidates[i], hintNow) {
			hint = &candidates[i]
			break
		}
	}
	if hint == nil {
		return nil, false, nil
	}
	route, err := d.ragOwnershipRoute(ctx, hint.DocID)
	if errors.Is(err, ErrNotFound) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	doc, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, hint.DocID, route)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRAGLifecycleInactive) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !active {
		return nil, true, nil
	}
	task, err := d.ragGCTaskInTx(ctx, tx, hint.ID)
	if ragIsNoRows(err) {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if task.DocID != doc.ID || !ragGCTaskDue(task, now) {
		return nil, true, nil
	}
	version, err := d.ragVersionInTx(ctx, tx, task.DocID, task.RetiredVersion)
	if ragIsNoRows(err) {
		if _, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
			status='FAILED',lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
			WHERE id=%s AND claim_generation=%s AND status IN ('PENDING','RUNNING')`,
			d.ph(1), d.ph(2)), task.ID, task.ClaimGeneration); updateErr != nil {
			return nil, false, updateErr
		}
		return nil, true, tx.Commit()
	}
	if err != nil {
		return nil, false, err
	}
	if version.Status == RAGDocumentVersionGCED {
		if _, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
			status='DONE',lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
			WHERE id=%s AND claim_generation=%s AND status IN ('PENDING','RUNNING')`,
			d.ph(1), d.ph(2)), task.ID, task.ClaimGeneration); updateErr != nil {
			return nil, false, updateErr
		}
		return nil, true, tx.Commit()
	}
	if version.Status != RAGDocumentVersionRetired || doc.ActiveVersion == task.RetiredVersion {
		if _, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
			status='FAILED',lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
			WHERE id=%s AND claim_generation=%s AND status IN ('PENDING','RUNNING')`,
			d.ph(1), d.ph(2)), task.ID, task.ClaimGeneration); updateErr != nil {
			return nil, false, updateErr
		}
		return nil, true, tx.Commit()
	}
	oldStatus := task.Status
	condition := "status='PENDING'"
	if oldStatus == "RUNNING" {
		condition = "status='RUNNING'"
	}
	leaseUntil := now.Add(leaseDuration)
	newGeneration := task.ClaimGeneration + 1
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
		status='RUNNING',claim_generation=%s,lease_owner=%s,lease_until=%s,
		heartbeat_at=%s,attempt_count=attempt_count+1,next_run_at=NULL
		WHERE id=%s AND doc_id=%s AND retired_version=%s AND claim_generation=%s AND %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ragNowExpr(), d.ph(4), d.ph(5), d.ph(6), d.ph(7), condition),
		newGeneration, workerID, leaseUntil, task.ID, task.DocID,
		task.RetiredVersion, task.ClaimGeneration)
	if err != nil {
		return nil, false, err
	}
	if ok, err := ragRowsAffected(result); err != nil || !ok {
		return nil, true, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	task.Status = "RUNNING"
	task.ClaimGeneration = newGeneration
	task.LeaseOwner = workerID
	task.LeaseUntil = &leaseUntil
	task.HeartbeatAt = &now
	task.AttemptCount++
	task.NextRunAt = nil
	fence := RAGIndexGCFence{
		TaskID: task.ID, DocID: task.DocID, RetiredVersion: task.RetiredVersion,
		ClaimGeneration: task.ClaimGeneration, LeaseOwner: task.LeaseOwner,
	}
	return &RAGIndexGCClaim{Task: *task, KBID: route.KBID, Fence: fence}, true, nil
}

func (d *DBStore) CheckRAGIndexGCFence(ctx context.Context, fence RAGIndexGCFence) (bool, error) {
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	var present int
	err = d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT 1 FROM rag_index_gc_tasks g
		JOIN rag_documents d ON d.id=g.doc_id
		JOIN rag_document_versions v ON v.doc_id=g.doc_id AND v.doc_version=g.retired_version
		WHERE g.id=%s AND g.doc_id=%s AND g.retired_version=%s
		AND g.claim_generation=%s AND g.lease_owner=%s AND g.status='RUNNING'
		AND g.lease_until > %s AND v.status='RETIRED' AND d.active_version<>g.retired_version`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		fence.TaskID, fence.DocID, fence.RetiredVersion, fence.ClaimGeneration,
		fence.LeaseOwner, now).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (d *DBStore) HeartbeatRAGIndexGCTask(
	ctx context.Context,
	fence RAGIndexGCFence,
	leaseDuration time.Duration,
) (bool, error) {
	if leaseDuration <= 0 {
		return false, errors.New("store: RAG GC lease duration must be positive")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
		lease_until=%s,heartbeat_at=%s WHERE id=%s AND doc_id=%s AND retired_version=%s
		AND claim_generation=%s AND lease_owner=%s AND status='RUNNING'
		AND lease_until > %s AND EXISTS (SELECT 1 FROM rag_documents d
			JOIN rag_document_versions v ON v.doc_id=d.id AND v.doc_version=%s
			WHERE d.id=%s AND d.active_version<>%s AND v.status='RETIRED')`,
		d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10)), now.Add(leaseDuration),
		fence.TaskID, fence.DocID, fence.RetiredVersion, fence.ClaimGeneration,
		fence.LeaseOwner, now, fence.RetiredVersion, fence.DocID, fence.RetiredVersion)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

func (d *DBStore) RetryRAGIndexGCTask(
	ctx context.Context,
	fence RAGIndexGCFence,
	nextRunDelay time.Duration,
) (bool, error) {
	if nextRunDelay < 0 {
		nextRunDelay = 0
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	nextRunAt := now.Add(nextRunDelay)
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
		status='PENDING',lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=%s
		WHERE id=%s AND doc_id=%s AND retired_version=%s AND claim_generation=%s
		AND lease_owner=%s AND status='RUNNING' AND lease_until > %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		nextRunAt, fence.TaskID, fence.DocID, fence.RetiredVersion,
		fence.ClaimGeneration, fence.LeaseOwner, now)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

type ragLockedGCFence struct {
	doc     *RAGDocumentRecord
	task    *RAGIndexGCTaskRecord
	version *RAGDocumentVersionRecord
	now     time.Time
}

func (d *DBStore) lockRAGIndexGCFence(
	ctx context.Context,
	tx *sql.Tx,
	fence RAGIndexGCFence,
) (*ragLockedGCFence, bool, error) {
	doc, err := d.ragDocumentInTx(ctx, tx, fence.DocID)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	task, err := d.ragGCTaskInTx(ctx, tx, fence.TaskID)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	version, err := d.ragVersionInTx(ctx, tx, fence.DocID, fence.RetiredVersion)
	if ragIsNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	valid := task.DocID == fence.DocID && task.RetiredVersion == fence.RetiredVersion &&
		task.ClaimGeneration == fence.ClaimGeneration && task.LeaseOwner == fence.LeaseOwner &&
		task.Status == "RUNNING" && task.LeaseUntil != nil && task.LeaseUntil.After(now) &&
		version.Status == RAGDocumentVersionRetired && doc.ActiveVersion != fence.RetiredVersion
	if !valid {
		return nil, false, nil
	}
	return &ragLockedGCFence{doc: doc, task: task, version: version, now: now}, true, nil
}

// FinishRAGIndexGCTask performs only the SQL half of exact-version cleanup.
// The caller must successfully delete the matching external vector version
// first; otherwise it should call RetryRAGIndexGCTask and retain RETIRED.
func (d *DBStore) FinishRAGIndexGCTask(ctx context.Context, fence RAGIndexGCFence) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, ok, err := d.lockRAGIndexGCFence(ctx, tx, fence)
	if err != nil || !ok {
		return false, err
	}
	if err := d.deleteRAGVersionCatalogInTx(ctx, tx, fence.DocID, fence.RetiredVersion); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='GCED',updated_at=%s WHERE doc_id=%s AND doc_version=%s AND status='RETIRED'`,
		d.ragNowExpr(), d.ph(1), d.ph(2)), fence.DocID, fence.RetiredVersion)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}
	result, err = tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_gc_tasks SET
		status='DONE',lease_owner='',lease_until=NULL,heartbeat_at=NULL,next_run_at=NULL
		WHERE id=%s AND doc_id=%s AND retired_version=%s AND claim_generation=%s
		AND lease_owner=%s AND status='RUNNING'`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		fence.TaskID, fence.DocID, fence.RetiredVersion, fence.ClaimGeneration,
		fence.LeaseOwner)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}
	return true, tx.Commit()
}

func (d *DBStore) deleteRAGVersionCatalogInTx(
	ctx context.Context,
	tx *sql.Tx,
	docID string,
	docVersion int64,
) error {
	if err := d.touchRAGVersionAssetsBeforeRemoval(ctx, tx, docID, docVersion); err != nil {
		return err
	}
	if err := d.touchRAGVersionAttachmentsBeforeRemoval(ctx, tx, docID, docVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_chunk_assets
		WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), docID, docVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_assets
		WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), docID, docVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_attachments
		WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), docID, docVersion); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_chunks
		WHERE doc_id=%s AND doc_version=%s`, d.ph(1), d.ph(2)), docID, docVersion)
	return err
}

func (d *DBStore) ListRAGVersionCleanupCandidates(
	ctx context.Context,
	staleFor time.Duration,
	limit int,
) ([]RAGVersionCleanupCandidate, error) {
	if staleFor < 0 {
		return nil, errors.New("store: RAG version stale duration must not be negative")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return nil, err
	}
	olderThan := now.Add(-staleFor)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT v.doc_id,d.kb_id,v.doc_version,
		v.status,v.parse_artifact_key,v.updated_at FROM rag_document_versions v
		JOIN rag_documents d ON d.id=v.doc_id
		WHERE v.status IN ('FAILED','SUPERSEDED','GCED') AND d.active_version<>v.doc_version
		AND v.updated_at<=%s ORDER BY v.updated_at,v.doc_id,v.doc_version LIMIT %s`,
		d.ph(1), d.ph(2)), olderThan.UTC(), ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGVersionCleanupCandidate, 0)
	for rows.Next() {
		var candidate RAGVersionCleanupCandidate
		if err := rows.Scan(&candidate.DocID, &candidate.KBID, &candidate.DocVersion,
			&candidate.Status, &candidate.ParseArtifactKey, &candidate.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

// MarkRAGDocumentVersionGCED is the idempotent SQL terminal step used by the
// orphan sweeper after an exact external vector delete. A GCED tombstone is
// retained and its timestamp refreshed so bounded sweeps progress fairly.
func (d *DBStore) MarkRAGDocumentVersionGCED(
	ctx context.Context,
	docID string,
	docVersion int64,
) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	doc, err := d.ragDocumentInTx(ctx, tx, docID)
	if ragIsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if doc.ActiveVersion == docVersion {
		return false, nil
	}
	version, err := d.ragVersionInTx(ctx, tx, docID, docVersion)
	if ragIsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if version.Status != RAGDocumentVersionFailed &&
		version.Status != RAGDocumentVersionSuperseded &&
		version.Status != RAGDocumentVersionGCED {
		return false, nil
	}
	if err := d.deleteRAGVersionCatalogInTx(ctx, tx, docID, docVersion); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_document_versions SET
		status='GCED',updated_at=%s WHERE doc_id=%s AND doc_version=%s
		AND status IN ('FAILED','SUPERSEDED','GCED')`, d.ragNowExpr(), d.ph(1), d.ph(2)),
		docID, docVersion)
	if err != nil {
		return false, err
	}
	if updated, err := ragRowsAffected(result); err != nil || !updated {
		return false, err
	}
	return true, tx.Commit()
}

// IsRAGParseArtifactReferenced protects cache reuse across versions. Any
// runnable, active, done, or retired version pins the artifact; terminal
// FAILED/SUPERSEDED/GCED-only references do not.
func (d *DBStore) IsRAGParseArtifactReferenced(
	ctx context.Context,
	docID, artifactKey string,
) (bool, error) {
	if strings.TrimSpace(docID) == "" || strings.TrimSpace(artifactKey) == "" {
		return false, nil
	}
	var count int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
		FROM rag_document_versions v JOIN rag_documents d ON d.id=v.doc_id
		WHERE v.doc_id=%s AND v.parse_artifact_key=%s AND (
			v.status IN ('PENDING','RUNNING','DONE','RETIRED') OR d.active_version=v.doc_version)`,
		d.ph(1), d.ph(2)), docID, artifactKey).Scan(&count)
	return count > 0, err
}

func (d *DBStore) ragTextContains(column, needle string) string {
	switch d.dialect {
	case "postgres":
		return "POSITION(" + needle + " IN " + column + ") > 0"
	case mysqlDialect:
		return "LOCATE(" + needle + "," + column + ") > 0"
	default:
		return "instr(" + column + "," + needle + ") > 0"
	}
}

func (d *DBStore) ragAssetReferenceSQL(assetAlias string) string {
	return `EXISTS (SELECT 1 FROM rag_version_assets va
		JOIN rag_document_versions v ON v.doc_id=va.doc_id AND v.doc_version=va.doc_version
		JOIN rag_documents d ON d.id=va.doc_id
		WHERE va.asset_id=` + assetAlias + `.id AND va.doc_id=` + assetAlias + `.doc_id
		AND (v.status IN ('PENDING','RUNNING','DONE','RETIRED') OR d.active_version=va.doc_version)) OR
	EXISTS (SELECT 1 FROM rag_chunk_assets ca
		JOIN rag_document_versions v ON v.doc_id=ca.doc_id AND v.doc_version=ca.doc_version
		JOIN rag_documents d ON d.id=ca.doc_id
		WHERE ca.asset_id=` + assetAlias + `.id
		AND (v.status IN ('PENDING','RUNNING','DONE','RETIRED') OR d.active_version=ca.doc_version)) OR
	EXISTS (SELECT 1 FROM rag_chat_turns t WHERE ` + d.ragTextContains("t.sources", assetAlias+".id") + `) OR
	EXISTS (SELECT 1 FROM session_messages m WHERE m.role='assistant' AND ` +
		d.ragTextContains("m.metadata", assetAlias+".id") + `)`
}

func (d *DBStore) ListRAGStagingAssetCleanupCandidates(
	ctx context.Context,
	staleFor time.Duration,
	limit int,
) ([]RAGAssetRecord, error) {
	if staleFor < 0 {
		return nil, errors.New("store: RAG asset stale duration must not be negative")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return nil, err
	}
	olderThan := now.Add(-staleFor)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT `+ragAssetColumns+`
		FROM rag_assets a WHERE a.updated_at<=%s AND NOT (%s)
		ORDER BY a.updated_at,a.doc_id,a.id LIMIT %s`, d.ph(1),
		d.ragAssetReferenceSQL("a"), d.ph(2)), olderThan.UTC(), ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGAssetRecord, 0)
	for rows.Next() {
		asset, err := scanRAGAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *asset)
	}
	return out, rows.Err()
}

func (d *DBStore) IsRAGAssetReferenced(ctx context.Context, assetID string) (bool, error) {
	if strings.TrimSpace(assetID) == "" {
		return false, nil
	}
	var count int
	err := d.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_assets a
		WHERE a.id=%s AND (%s)`, d.ph(1), d.ragAssetReferenceSQL("a")), assetID).Scan(&count)
	return count > 0, err
}

// ClaimRAGStagingAssetCleanup atomically removes an unreferenced asset from
// the SQL catalog and changes each immutable object registry row to a durable
// DELETING tombstone. External deletion starts only after this transaction
// commits, so a failed or delayed delete is always discoverable by the global
// object-write sweeper. Legacy assets without registry rows receive tombstones
// before their catalog row disappears.
func (d *DBStore) ClaimRAGStagingAssetCleanup(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
	assetID string,
) (*RAGAssetCleanupClaim, bool, error) {
	assetID = strings.TrimSpace(assetID)
	if strings.TrimSpace(fence.DocID) == "" || assetID == "" {
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
	if err != nil {
		return nil, false, err
	}
	if !active {
		return nil, false, nil
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	valid, err := d.ragDocumentMaintenanceFenceValidInTx(ctx, tx, fence, now)
	if err != nil || !valid {
		return nil, false, err
	}

	asset, err := scanRAGAsset(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragAssetColumns+` FROM rag_assets WHERE id=%s%s`,
		d.ph(1), d.ragLockSuffix()), assetID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if asset.DocID != fence.DocID {
		return nil, false, nil
	}
	var references int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_assets a
		WHERE a.id=%s AND (%s)`, d.ph(1), d.ragAssetReferenceSQL("a")), assetID).
		Scan(&references); err != nil {
		return nil, false, err
	}
	if references != 0 {
		return nil, false, nil
	}

	objects := []struct {
		kind string
		key  string
	}{
		{kind: RAGObjectKindAssetSource, key: strings.TrimSpace(asset.SourceObjectKey)},
		{kind: RAGObjectKindAssetDisplay, key: strings.TrimSpace(asset.DisplayObjectKey)},
		{kind: RAGObjectKindAssetThumbnail, key: strings.TrimSpace(asset.ThumbnailObjectKey)},
	}
	claim := &RAGAssetCleanupClaim{AssetID: asset.ID, DocID: asset.DocID}
	for _, object := range objects {
		if object.key == "" {
			continue
		}
		handleID := ragObjectWriteHandleID(object.key)
		current, scanErr := scanRAGObjectWrite(tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT `+ragObjectWriteColumns+` FROM rag_object_write_staging
			WHERE handle_id=%s%s`, d.ph(1), d.ragLockSuffix()), handleID))
		if errors.Is(scanErr, sql.ErrNoRows) {
			current = &RAGObjectWriteFence{
				HandleID: handleID, UserID: route.UserID, KBID: route.KBID,
				DocID: asset.DocID, ObjectKind: object.kind, ObjectKey: object.key,
				ReferenceKey: asset.ID, Generation: 1, Status: ragObjectWriteDeleting,
				UpdatedAt: now,
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_object_write_staging
				(handle_id,user_id,kb_id,doc_id,object_kind,object_key,reference_key,generation,status,created_at,updated_at)
				VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3),
				d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11)),
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
				current.DocID != asset.DocID || current.ObjectKind != object.kind ||
				current.ObjectKey != object.key || current.ReferenceKey != asset.ID {
				return nil, false, ErrRAGDocumentVersionMismatch
			}
			switch current.Status {
			case ragObjectWritePublished:
				nextGeneration := current.Generation + 1
				result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_object_write_staging SET
					generation=%s,status='DELETING',updated_at=%s WHERE handle_id=%s
					AND generation=%s AND status='PUBLISHED'`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
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
				// Immutable generation keys make overlapping deletion attempts safe.
			default:
				return nil, false, ErrRAGDocumentVersionConflict
			}
		}
		claim.ObjectWrites = append(claim.ObjectWrites, *current)
	}

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_assets WHERE asset_id=%s`, d.ph(1)), asset.ID); err != nil {
		return nil, false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_assets WHERE id=%s AND doc_id=%s`,
		d.ph(1), d.ph(2)), asset.ID, asset.DocID)
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

func (d *DBStore) DeleteRAGAsset(ctx context.Context, assetID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_assets WHERE asset_id=%s`, d.ph(1)), assetID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_assets WHERE id=%s`, d.ph(1)), assetID)
	if err := ragMutationResult(result, err); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteRAGAssetWithMaintenance is the fenced catalog half of staging asset
// cleanup. External objects are deleted by the caller only after checking the
// same maintenance fence; this final transaction revalidates both the fence
// and all durable asset references before removing the row.
func (d *DBStore) DeleteRAGAssetWithMaintenance(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
	assetID string,
) (bool, error) {
	route, err := d.ragOwnershipRoute(ctx, fence.DocID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, _, err := d.lockRAGDocumentHierarchyTx(ctx, tx, fence.DocID, route); err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return false, err
	}
	valid, err := d.ragDocumentMaintenanceFenceValidInTx(ctx, tx, fence, now)
	if err != nil || !valid {
		return false, err
	}
	var assetDocID string
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT doc_id FROM rag_assets WHERE id=%s`, d.ph(1)), assetID).
		Scan(&assetDocID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if assetDocID != fence.DocID {
		return false, nil
	}
	var references int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_assets a
		WHERE a.id=%s AND (%s)`, d.ph(1), d.ragAssetReferenceSQL("a")), assetID).
		Scan(&references); err != nil {
		return false, err
	}
	if references != 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_version_assets WHERE asset_id=%s`, d.ph(1)), assetID); err != nil {
		return false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_assets WHERE id=%s AND doc_id=%s`,
		d.ph(1), d.ph(2)), assetID, fence.DocID)
	if err != nil {
		return false, err
	}
	if deleted, err := ragRowsAffected(result); err != nil || !deleted {
		return false, err
	}
	return true, tx.Commit()
}
