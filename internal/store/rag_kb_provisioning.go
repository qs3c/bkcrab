package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func validRAGKBProvisionFence(fence RAGKBProvisionFence) bool {
	return strings.TrimSpace(fence.KBID) != "" &&
		strings.TrimSpace(fence.UserID) != "" &&
		fence.Generation > 0 && validRAGWorkerID(fence.LeaseOwner)
}

// BeginRAGKBProvisioning publishes a fail-closed SQL cleanup handle before
// any Milvus operation can create external state. The user lock both fences a
// concurrent account tombstone and makes the quota decision atomic across
// application replicas.
func (d *DBStore) BeginRAGKBProvisioning(
	ctx context.Context,
	kb *RAGKBRecord,
	leaseOwner string,
	leaseDuration time.Duration,
	maxKBsPerUser int,
) (*RAGKBProvisionFence, error) {
	if kb == nil || strings.TrimSpace(kb.ID) == "" || strings.TrimSpace(kb.UserID) == "" {
		return nil, ErrRAGLifecycleInactive
	}
	if !validRAGWorkerID(leaseOwner) {
		return nil, errors.New("store: RAG KB provisioning owner must be 1..96 trimmed bytes")
	}
	if leaseDuration <= 0 {
		return nil, errors.New("store: RAG KB provisioning lease duration must be positive")
	}
	if maxKBsPerUser <= 0 {
		return nil, errors.New("store: RAG KB quota must be positive")
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := d.lockActiveRAGUserTx(ctx, tx, kb.UserID); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COUNT(*) FROM rag_kbs WHERE user_id=%s`, d.ph(1)), kb.UserID).Scan(&count); err != nil {
		return nil, err
	}
	if count >= maxKBsPerUser {
		return nil, ErrRAGKBQuotaExceeded
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	if kb.CreatedAt.IsZero() {
		kb.CreatedAt = now
	}
	kb.UpdatedAt = now
	if kb.ParseMode == "" {
		kb.ParseMode = RAGParseModeStandard
	}
	kb.Status = RAGKBStatusProvisioning
	const generation int64 = 1
	leaseUntil := now.Add(leaseDuration)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kbs
		(id, user_id, name, description, embed_provider, embed_model, embed_dims,
		 chunk_size, chunk_overlap, parse_mode, enrichment_enabled, status,
		 provisioning_generation, provisioning_lease_owner, provisioning_lease_until,
		 created_at, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9),
		d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14), d.ph(15), d.ph(16), d.ph(17)),
		kb.ID, kb.UserID, kb.Name, kb.Description, kb.EmbedProvider, kb.EmbedModel,
		kb.EmbedDims, kb.ChunkSize, kb.ChunkOverlap, kb.ParseMode, kb.EnrichmentEnabled,
		kb.Status, generation, leaseOwner, leaseUntil, kb.CreatedAt, kb.UpdatedAt); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &RAGKBProvisionFence{
		KBID: kb.ID, UserID: kb.UserID, Generation: generation,
		LeaseOwner: leaseOwner, LeaseUntil: leaseUntil,
	}, nil
}

func (d *DBStore) HeartbeatRAGKBProvisioning(
	ctx context.Context,
	fence RAGKBProvisionFence,
	leaseDuration time.Duration,
) (bool, error) {
	if !validRAGKBProvisionFence(fence) {
		return false, errors.New("store: invalid RAG KB provisioning fence")
	}
	if leaseDuration <= 0 {
		return false, errors.New("store: RAG KB provisioning lease duration must be positive")
	}
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs
		SET provisioning_lease_until=%s,updated_at=%s
		WHERE id=%s AND user_id=%s AND LOWER(status)=%s
		AND provisioning_generation=%s AND provisioning_lease_owner=%s
		AND provisioning_lease_until IS NOT NULL AND provisioning_lease_until>%s`,
		d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		now.Add(leaseDuration), fence.KBID, fence.UserID, RAGKBStatusProvisioning,
		fence.Generation, fence.LeaseOwner, now)
	if err != nil {
		return false, err
	}
	return ragRowsAffected(result)
}

// lockRAGKBProvisionTx follows the global user -> KB order. The caller passes
// a routing owner read before the transaction and every ownership value is
// revalidated after both serialization locks are held.
func (d *DBStore) lockRAGKBProvisionTx(
	ctx context.Context,
	tx *sql.Tx,
	kbID, expectedUserID string,
) (*RAGKBRecord, string, bool, error) {
	userStatus, userExists, err := d.lockRAGUserTx(ctx, tx, expectedUserID)
	if err != nil {
		return nil, "", false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET updated_at=updated_at
		WHERE id=%s`, d.ph(1)), kbID); err != nil {
		return nil, "", false, err
	}
	kb, err := d.ragKBInTx(ctx, tx, kbID)
	if ragIsNoRows(err) {
		return nil, userStatus, userExists, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if kb.UserID != expectedUserID {
		return nil, userStatus, userExists, ErrRAGLifecycleInactive
	}
	return kb, userStatus, userExists, nil
}

func (d *DBStore) ActivateRAGKBProvisioning(
	ctx context.Context,
	fence RAGKBProvisionFence,
) (*RAGKBRecord, bool, error) {
	if !validRAGKBProvisionFence(fence) {
		return nil, false, errors.New("store: invalid RAG KB provisioning fence")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	kb, userStatus, userExists, err := d.lockRAGKBProvisionTx(ctx, tx, fence.KBID, fence.UserID)
	if err != nil {
		return nil, false, err
	}
	if kb == nil {
		return nil, false, nil
	}
	if !userExists || !strings.EqualFold(userStatus, "active") {
		return nil, false, ErrRAGLifecycleInactive
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET
		status='active',provisioning_lease_owner='',provisioning_lease_until=NULL,updated_at=%s
		WHERE id=%s AND user_id=%s AND LOWER(status)=%s
		AND provisioning_generation=%s AND provisioning_lease_owner=%s
		AND provisioning_lease_until IS NOT NULL AND provisioning_lease_until>%s`,
		d.ragNowExpr(), d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		fence.KBID, fence.UserID, RAGKBStatusProvisioning, fence.Generation,
		fence.LeaseOwner, now)
	if err != nil {
		return nil, false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || !updated {
		return nil, false, err
	}
	active, err := ragKBInTxWithoutLock(ctx, d, tx, fence.KBID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return active, true, nil
}

// AbortRAGKBProvisioning acknowledges that the external EnsureCollection call
// has returned. It releases the provisioning lease and retains a DELETING row
// until idempotent external cleanup and the SQL finalizer both succeed.
func (d *DBStore) AbortRAGKBProvisioning(
	ctx context.Context,
	fence RAGKBProvisionFence,
) (*RAGKBRecord, bool, error) {
	if !validRAGKBProvisionFence(fence) {
		return nil, false, errors.New("store: invalid RAG KB provisioning fence")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	kb, _, _, err := d.lockRAGKBProvisionTx(ctx, tx, fence.KBID, fence.UserID)
	if err != nil {
		return nil, false, err
	}
	if kb == nil {
		return nil, false, nil
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET
		status=%s,provisioning_lease_owner='',provisioning_lease_until=NULL,updated_at=%s
		WHERE id=%s AND user_id=%s AND provisioning_generation=%s
		AND provisioning_lease_owner=%s AND LOWER(status) IN (%s,%s)`,
		d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		RAGKBStatusDeleting, fence.KBID, fence.UserID, fence.Generation,
		fence.LeaseOwner, RAGKBStatusProvisioning, RAGKBStatusDeleting)
	if err != nil {
		return nil, false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || !updated {
		return nil, false, err
	}
	marked, err := ragKBInTxWithoutLock(ctx, d, tx, fence.KBID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return marked, true, nil
}

// ragKBInTxWithoutLock is used only after lockRAGKBProvisionTx has already
// acquired the KB row in the same transaction.
func ragKBInTxWithoutLock(ctx context.Context, d *DBStore, tx *sql.Tx, kbID string) (*RAGKBRecord, error) {
	return scanRAGKB(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s`, d.ph(1)), kbID))
}

func (d *DBStore) ListExpiredRAGKBProvisions(
	ctx context.Context,
	limit int,
) ([]RAGKBProvisionCleanupCandidate, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT id,user_id,provisioning_generation
		FROM rag_kbs WHERE LOWER(status)=%s AND
		(provisioning_lease_until IS NULL OR provisioning_lease_until<=%s)
		ORDER BY updated_at,id LIMIT %s`, d.ph(1), d.ragNowExpr(), d.ph(2)),
		RAGKBStatusProvisioning, ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGKBProvisionCleanupCandidate, 0)
	for rows.Next() {
		var candidate RAGKBProvisionCleanupCandidate
		if err := rows.Scan(&candidate.KBID, &candidate.UserID, &candidate.Generation); err != nil {
			return nil, err
		}
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func (d *DBStore) ExpireRAGKBProvisioning(
	ctx context.Context,
	candidate RAGKBProvisionCleanupCandidate,
) (*RAGKBRecord, bool, error) {
	if strings.TrimSpace(candidate.KBID) == "" || strings.TrimSpace(candidate.UserID) == "" ||
		candidate.Generation <= 0 {
		return nil, false, errors.New("store: invalid expired RAG KB provisioning candidate")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	kb, _, _, err := d.lockRAGKBProvisionTx(ctx, tx, candidate.KBID, candidate.UserID)
	if err != nil {
		return nil, false, err
	}
	if kb == nil {
		return nil, false, nil
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET
		status=%s,provisioning_lease_owner='',provisioning_lease_until=NULL,updated_at=%s
		WHERE id=%s AND user_id=%s AND LOWER(status)=%s
		AND provisioning_generation=%s AND
		(provisioning_lease_until IS NULL OR provisioning_lease_until<=%s)`,
		d.ph(1), d.ragNowExpr(), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		RAGKBStatusDeleting, candidate.KBID, candidate.UserID, RAGKBStatusProvisioning,
		candidate.Generation, now)
	if err != nil {
		return nil, false, err
	}
	updated, err := ragRowsAffected(result)
	if err != nil || !updated {
		return nil, false, err
	}
	marked, err := ragKBInTxWithoutLock(ctx, d, tx, candidate.KBID)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return marked, true, nil
}

func (d *DBStore) IsRAGKBCleanupReady(ctx context.Context, kbID string) (bool, error) {
	now, err := d.ragDBNow(ctx, d.db)
	if err != nil {
		return false, err
	}
	return d.ragKBCleanupReadyAt(ctx, d.db, kbID, now)
}

func (d *DBStore) ragKBCleanupReadyAt(
	ctx context.Context,
	queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	},
	kbID string,
	now time.Time,
) (bool, error) {
	var count int
	err := queryer.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_kbs
		WHERE id=%s AND provisioning_lease_until IS NOT NULL
		AND provisioning_lease_until>%s`, d.ph(1), d.ph(2)), kbID, now).Scan(&count)
	return count == 0, err
}
