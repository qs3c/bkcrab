package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// GetRAGPoisonRepairCandidate captures the exact canonical row snapshot that
// one independently validated delivery locator may repair. The RAG adapter
// must compare the returned canonical user with the registered queue tenant
// hash (and, for a body locator, its tenant ID) before applying the CAS.
// Callers capture every body/header locator before applying any repair so two
// locators for the same row cannot advance more than one generation.
func (s *RAGFairQueueStore) GetRAGPoisonRepairCandidate(
	ctx context.Context,
	taskID int64,
	expectedDispatchGeneration int64,
) (*RAGIndexTaskDispatchCandidate, error) {
	var candidate *RAGIndexTaskDispatchCandidate
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			candidate, err = s.store.getRAGPoisonRepairCandidateOn(
				ctx, conn, taskID, expectedDispatchGeneration,
			)
			return err
		})
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

func (d *DBStore) getRAGPoisonRepairCandidateOn(
	ctx context.Context,
	session ragFairQueueSession,
	taskID int64,
	expectedDispatchGeneration int64,
) (*RAGIndexTaskDispatchCandidate, error) {
	if taskID <= 0 || expectedDispatchGeneration <= 0 {
		return nil, ErrNotFound
	}
	query := fmt.Sprintf(`SELECT `+ragFairQueueTaskColumns+d.ragFairQueueRawTimestampColumns("t")+
		ragFairQueueCanonicalJoin+` WHERE t.id=%s
		AND t.dispatch_generation=%s AND t.dispatch_generation>t.claim_generation`,
		d.ph(1), d.ph(2))
	query += ` AND ` + d.ragFairQueueDuePredicate("t")
	candidate, err := d.scanRAGIndexTaskDispatchCandidate(
		session.QueryRowContext(ctx, query, taskID, expectedDispatchGeneration),
	)
	if err != nil {
		return nil, scanErr(err)
	}
	return candidate, nil
}

// RearmRAGPoisonCandidate creates a fresh durable publish obligation from a
// previously captured canonical snapshot. A stale duplicate is a successful
// no-op; it must never advance the row a second time.
func (s *RAGFairQueueStore) RearmRAGPoisonCandidate(
	ctx context.Context,
	original RAGIndexTaskDispatchCandidate,
) (*RAGIndexTaskDispatchCandidate, bool, error) {
	var candidate *RAGIndexTaskDispatchCandidate
	var changed bool
	err := s.withExpectedWriterTx(ctx, func(tx *sql.Tx) error {
		var err error
		candidate, changed, err = s.store.rearmRAGPoisonCandidateOn(ctx, tx, original)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return candidate, changed, nil
}

func (d *DBStore) rearmRAGPoisonCandidateOn(
	ctx context.Context,
	session ragFairQueueSession,
	original RAGIndexTaskDispatchCandidate,
) (*RAGIndexTaskDispatchCandidate, bool, error) {
	if !validRAGIndexTaskDispatchCandidate(original) ||
		original.Guard.DispatchGeneration <= original.Guard.ClaimGeneration {
		return nil, false, ErrRAGIndexTaskDispatchGuard
	}
	guard := original.Guard
	baseGeneration := guard.DispatchGeneration
	if guard.ClaimGeneration > baseGeneration {
		baseGeneration = guard.ClaimGeneration
	}
	if baseGeneration == math.MaxInt64 {
		return nil, false, ErrRAGDispatchGenerationExhausted
	}
	query := fmt.Sprintf(`UPDATE rag_index_tasks
		SET dispatch_generation=%s,dispatched_at=NULL
		WHERE id=%s AND doc_id=%s AND doc_version=%s AND user_id=%s AND status=%s
		AND dispatch_generation=%s AND claim_generation=%s AND retry_count=%s
		AND lease_owner=%s AND %s AND %s AND %s
		AND dispatch_generation>claim_generation AND %s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8),
		d.ph(9), d.ph(10), d.ragNullSafeRawTimestampEqual("next_run_at", 11),
		d.ragNullSafeRawTimestampEqual("lease_until", 12),
		d.ragNullSafeRawTimestampEqual("dispatched_at", 13),
		ragCanonicalTaskOwnerExists("rag_index_tasks"))
	query += ` AND ` + d.ragFairQueueDuePredicate("rag_index_tasks")
	result, err := session.ExecContext(ctx, query,
		baseGeneration+1, guard.TaskID, guard.DocID, guard.DocVersion, guard.UserID,
		guard.Status, guard.DispatchGeneration, guard.ClaimGeneration, guard.RetryCount,
		guard.LeaseOwner, ragTimestampGuardArgument(guard.NextRunAtRaw),
		ragTimestampGuardArgument(guard.LeaseUntilRaw),
		ragTimestampGuardArgument(guard.DispatchedAtRaw))
	if err != nil {
		return nil, false, err
	}
	changed, err := ragRowsAffected(result)
	if err != nil || !changed {
		return nil, false, err
	}
	updated := original.Task
	updated.DispatchGeneration = baseGeneration + 1
	updated.DispatchedAt = nil
	raw := ragRawTimestampsFromGuard(original.Guard)
	raw.dispatchedAt = sql.NullString{}
	candidate := newRAGIndexTaskDispatchCandidate(updated, raw)
	return &candidate, true, nil
}
