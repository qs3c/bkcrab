package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *RAGFairQueueStore) validateRAGDocumentAIFenceWriter(fence IndexFence) error {
	if err := s.validate(); err != nil {
		return err
	}
	if fence.ExpectedWriterFingerprint != s.expectedWriter {
		s.store.recordFairQueueConnectionMismatch()
		return ErrFairQueueWriterMismatch
	}
	return nil
}

func (d *DBStore) readRAGOwnershipRouteOn(
	ctx context.Context,
	exec ragExecutor,
	docID string,
) (ragOwnershipRoute, error) {
	var route ragOwnershipRoute
	err := exec.QueryRowContext(ctx, fmt.Sprintf(`SELECT d.kb_id,kb.user_id
		FROM rag_documents d JOIN rag_kbs kb ON kb.id=d.kb_id WHERE d.id=%s`, d.ph(1)), docID).
		Scan(&route.KBID, &route.UserID)
	if errors.Is(err, sql.ErrNoRows) {
		return ragOwnershipRoute{}, ErrNotFound
	}
	return route, err
}

func (d *DBStore) currentRAGDocumentAIFullFenceTx(
	ctx context.Context,
	tx *sql.Tx,
	fence IndexFence,
	route ragOwnershipRoute,
	expectedUserID string,
) (bool, error) {
	locked, ok, err := d.lockRAGIndexFence(ctx, tx, fence, route)
	if err != nil || !ok {
		return false, err
	}
	return strings.TrimSpace(expectedUserID) != "" && expectedUserID == route.UserID &&
		locked.task.UserID == route.UserID, nil
}

func runRAGFairQueueBudgetTx(
	ctx context.Context,
	conn *sql.Conn,
	identity fairQueueMySQLIdentity,
	fn func(*sql.Tx) error,
) (callbackErr error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return errors.Join(ErrFairQueueUnsafeConnection, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if rollbackErr := tx.Rollback(); rollbackErr != nil &&
			!errors.Is(rollbackErr, sql.ErrTxDone) {
			callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, rollbackErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := verifyFairQueueMySQLSession(ctx, tx, identity, ""); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return errors.Join(ErrFairQueueUnsafeConnection, err)
	}
	committed = true
	return nil
}

func validRAGDocumentAITaskBudgetRecord(budget *RAGDocumentAITaskBudgetRecord) bool {
	return budget != nil && budget.TaskID > 0 && strings.TrimSpace(budget.UserID) != "" &&
		budget.MaxRequests >= 0 && budget.MaxTokens >= 0 && budget.MaxCostMicroUSD >= 0 &&
		budget.ChargedRequests >= 0 && budget.ChargedTokens >= 0 && budget.ChargedCostMicroUSD >= 0
}

func (d *DBStore) createRAGDocumentAITaskBudgetOn(
	ctx context.Context,
	exec ragExecutor,
	budget *RAGDocumentAITaskBudgetRecord,
) error {
	query := fmt.Sprintf(`INSERT INTO rag_document_ai_task_budgets (
		task_id,user_id,max_requests,max_tokens,max_cost_microusd,
		charged_requests,charged_tokens,charged_cost_microusd,updated_at)
		VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9))
	if d.dialect == mysqlDialect {
		query += ` ON DUPLICATE KEY UPDATE task_id=task_id`
	} else {
		query += ` ON CONFLICT (task_id) DO NOTHING`
	}
	if _, err := exec.ExecContext(ctx, query, budget.TaskID, budget.UserID,
		budget.MaxRequests, budget.MaxTokens, budget.MaxCostMicroUSD,
		budget.ChargedRequests, budget.ChargedTokens,
		budget.ChargedCostMicroUSD, budget.UpdatedAt); err != nil {
		return err
	}
	existing, err := d.readRAGDocumentAITaskBudgetOn(ctx, exec, budget.TaskID)
	if err != nil {
		return err
	}
	if existing.UserID != budget.UserID || existing.MaxRequests != budget.MaxRequests ||
		existing.MaxTokens != budget.MaxTokens ||
		existing.MaxCostMicroUSD != budget.MaxCostMicroUSD {
		return ErrRAGDocumentAIUsageConflict
	}
	return nil
}

// CreateRAGDocumentAITaskBudget is retained only to satisfy the legacy budget
// ledger shape. A fair execution must use CreateRAGDocumentAITaskBudgetForIndex
// so the immutable snapshot cannot be written after its exact claim loses the
// live lease.
func (s *RAGFairQueueStore) CreateRAGDocumentAITaskBudget(
	ctx context.Context,
	budget *RAGDocumentAITaskBudgetRecord,
) error {
	_ = ctx
	if err := s.validate(); err != nil {
		return err
	}
	if !validRAGDocumentAITaskBudgetRecord(budget) {
		return errors.New("store: invalid RAG DocumentAI task budget")
	}
	return ErrRAGDocumentAIInvalidFence
}

func (s *RAGFairQueueStore) CreateRAGDocumentAITaskBudgetForIndex(
	ctx context.Context,
	fence IndexFence,
	budget *RAGDocumentAITaskBudgetRecord,
) error {
	if err := s.validateRAGDocumentAIFenceWriter(fence); err != nil {
		return err
	}
	if !validRAGDocumentAITaskBudgetRecord(budget) || budget.TaskID != fence.TaskID {
		return ErrRAGDocumentAIInvalidFence
	}
	normalized := *budget
	if normalized.UpdatedAt.IsZero() {
		normalized.UpdatedAt = time.Now().UTC()
	}
	changed, err := s.store.withLiveRAGIndexFenceTx(ctx, fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked == nil || locked.task == nil || locked.task.ID != normalized.TaskID ||
				locked.task.UserID != normalized.UserID {
				return false, ErrRAGDocumentAIInvalidFence
			}
			if err := s.store.createRAGDocumentAITaskBudgetOn(ctx, tx, &normalized); err != nil {
				return false, err
			}
			return true, nil
		})
	if err != nil {
		return err
	}
	if !changed {
		return ErrRAGDocumentAIInvalidFence
	}
	return nil
}

func (s *RAGFairQueueStore) GetRAGDocumentAITaskBudget(
	ctx context.Context,
	taskID int64,
) (*RAGDocumentAITaskBudgetRecord, error) {
	var budget *RAGDocumentAITaskBudgetRecord
	err := s.withExpectedWriterConn(ctx, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		var err error
		budget, err = s.store.readRAGDocumentAITaskBudgetOn(ctx, conn, taskID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return budget, nil
}

func (s *RAGFairQueueStore) GetRAGDocumentAIUserBudget(
	ctx context.Context,
	userID string,
	periodStartUTC time.Time,
) (*RAGDocumentAIUserBudgetRecord, error) {
	var budget *RAGDocumentAIUserBudgetRecord
	err := s.withExpectedWriterConn(ctx, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		var err error
		budget, err = s.store.readRAGDocumentAIUserBudgetOn(ctx, conn, userID, periodStartUTC)
		return err
	})
	if err != nil {
		return nil, err
	}
	return budget, nil
}

func (s *RAGFairQueueStore) GetRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (*RAGDocumentAIUsageRecord, error) {
	var usage *RAGDocumentAIUsageRecord
	err := s.withExpectedWriterConn(ctx, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		var err error
		usage, err = s.store.readRAGDocumentAIUsageOn(ctx, conn, idempotencyKey)
		return err
	})
	if err != nil {
		return nil, err
	}
	return usage, nil
}

func (s *RAGFairQueueStore) ReserveRAGDocumentAIUsage(
	ctx context.Context,
	fence IndexFence,
	usage *RAGDocumentAIUsageRecord,
	userLimits RAGDocumentAILimits,
) (created bool, err error) {
	if err := s.validateRAGDocumentAIFenceWriter(fence); err != nil {
		return false, err
	}
	reservedTokens, err := validateRAGDocumentAIReservation(fence, usage, userLimits)
	if err != nil {
		return false, err
	}
	err = s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			route, routeErr := s.store.readRAGOwnershipRouteOn(ctx, conn, fence.DocID)
			if errors.Is(routeErr, ErrNotFound) {
				return ErrRAGDocumentAIInvalidFence
			}
			if routeErr != nil {
				return routeErr
			}
			txErr := runRAGFairQueueBudgetTx(ctx, conn, identity, func(tx *sql.Tx) error {
				var coreErr error
				created, coreErr = s.store.reserveRAGDocumentAIUsageTx(ctx, tx, fence, usage,
					userLimits, reservedTokens, true,
					func(ctx context.Context, fence IndexFence) (bool, error) {
						return s.store.currentRAGDocumentAIFullFenceTx(ctx, tx, fence, route, usage.UserID)
					})
				return coreErr
			})
			var insertErr *ragDocumentAIUsageInsertError
			if errors.As(txErr, &insertErr) && !errors.Is(txErr, ErrFairQueueUnsafeConnection) {
				created, txErr = s.store.resolveRAGDocumentAIReserveInsertError(
					ctx, conn, usage, insertErr.err)
			}
			return txErr
		})
	if err != nil {
		return false, err
	}
	return created, nil
}

func (s *RAGFairQueueStore) MarkSentRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	fence IndexFence,
) (sent bool, err error) {
	if err := s.validateRAGDocumentAIFenceWriter(fence); err != nil {
		return false, err
	}
	err = s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			preflight, err := s.store.readRAGDocumentAIUsageOn(ctx, conn, idempotencyKey)
			if err != nil {
				return err
			}
			if !ragDocumentAIUsageMatchesFence(preflight, fence) {
				return ErrRAGDocumentAIInvalidFence
			}
			route, err := s.store.readRAGOwnershipRouteOn(ctx, conn, fence.DocID)
			if errors.Is(err, ErrNotFound) {
				return ErrRAGDocumentAIInvalidFence
			}
			if err != nil {
				return err
			}
			return runRAGFairQueueBudgetTx(ctx, conn, identity, func(tx *sql.Tx) error {
				var coreErr error
				sent, coreErr = s.store.markSentRAGDocumentAIUsageTx(ctx, tx, preflight,
					idempotencyKey, fence, true,
					func(ctx context.Context, fence IndexFence) (bool, error) {
						return s.store.currentRAGDocumentAIFullFenceTx(ctx, tx, fence, route, preflight.UserID)
					})
				return coreErr
			})
		})
	if err != nil {
		return false, err
	}
	return sent, nil
}

func (s *RAGFairQueueStore) releaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	requireInvalidFence bool,
) (released bool, err error) {
	err = s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			preflight, err := s.store.readRAGDocumentAIUsageOn(ctx, conn, idempotencyKey)
			if err != nil {
				return err
			}
			var route ragOwnershipRoute
			routeMissing := false
			if requireInvalidFence {
				route, err = s.store.readRAGOwnershipRouteOn(ctx, conn, preflight.DocID)
				if errors.Is(err, ErrNotFound) {
					routeMissing = true
					err = nil
				}
				if err != nil {
					return err
				}
			}
			return runRAGFairQueueBudgetTx(ctx, conn, identity, func(tx *sql.Tx) error {
				var coreErr error
				released, coreErr = s.store.releaseRAGDocumentAIUsageTx(ctx, tx, preflight,
					idempotencyKey, requireInvalidFence,
					func(ctx context.Context, fence IndexFence) (bool, error) {
						if routeMissing {
							return false, nil
						}
						fence.ExpectedWriterFingerprint = s.expectedWriter
						return s.store.currentRAGDocumentAIFullFenceTx(ctx, tx, fence, route, preflight.UserID)
					})
				return coreErr
			})
		})
	if err != nil {
		return false, err
	}
	return released, nil
}

func (s *RAGFairQueueStore) ReleaseRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
) (bool, error) {
	return s.releaseRAGDocumentAIUsage(ctx, idempotencyKey, false)
}

func (s *RAGFairQueueStore) CommitRAGDocumentAIUsage(
	ctx context.Context,
	idempotencyKey string,
	actualInputTokens, actualOutputTokens, actualCostMicroUSD int64,
	usageEstimated bool,
) (committed bool, err error) {
	if _, err := ragDocumentAITokenTotal(actualInputTokens, actualOutputTokens); err != nil ||
		actualCostMicroUSD < 0 {
		return false, errors.New("store: invalid RAG DocumentAI settlement")
	}
	err = s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			preflight, err := s.store.readRAGDocumentAIUsageOn(ctx, conn, idempotencyKey)
			if err != nil {
				return err
			}
			return runRAGFairQueueBudgetTx(ctx, conn, identity, func(tx *sql.Tx) error {
				var coreErr error
				committed, coreErr = s.store.commitRAGDocumentAIUsageTx(ctx, tx, preflight,
					idempotencyKey, actualInputTokens, actualOutputTokens,
					actualCostMicroUSD, usageEstimated)
				return coreErr
			})
		})
	if err != nil {
		return false, err
	}
	return committed, nil
}

func (s *RAGFairQueueStore) ReconcileRAGDocumentAIUsage(
	ctx context.Context,
	reservedBefore, sentBefore time.Time,
	limit int,
) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	var candidates []ragDocumentAIReconcileCandidate
	err := s.withExpectedWriterConn(ctx, func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
		rows, err := conn.QueryContext(ctx, fmt.Sprintf(`SELECT idempotency_key,state
			FROM rag_document_ai_usage WHERE
			(state='RESERVED' AND reservation_expires_at IS NOT NULL
			 AND reservation_expires_at <= %s AND updated_at <= %s) OR
			(state='SENT' AND sent_at IS NOT NULL AND sent_at <= %s)
			ORDER BY updated_at,idempotency_key LIMIT %s`, s.store.ragNowExpr(),
			s.store.ph(1), s.store.ph(2), s.store.ph(3)),
			reservedBefore.UTC(), sentBefore.UTC(), limit)
		if err != nil {
			return err
		}
		candidates, err = collectRAGDocumentAIReconcileCandidates(rows)
		return err
	})
	if err != nil {
		return 0, err
	}

	transitioned := 0
	for _, item := range candidates {
		switch item.state {
		case RAGDocumentAIUsageReserved:
			ok, err := s.releaseRAGDocumentAIUsage(ctx, item.key, true)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return transitioned, err
			}
			if ok {
				transitioned++
			}
		case RAGDocumentAIUsageSent:
			usage, err := s.GetRAGDocumentAIUsage(ctx, item.key)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					continue
				}
				return transitioned, err
			}
			ok, err := s.CommitRAGDocumentAIUsage(ctx, item.key,
				usage.ReservedInputTokens, usage.ReservedOutputTokens,
				usage.EstimatedCostMicroUSD, true)
			if err != nil {
				return transitioned, err
			}
			if ok {
				transitioned++
			}
		}
	}
	return transitioned, nil
}
