package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// withRAGFairQueueLifecycleTx performs the lock-order routing read and the
// lifecycle mutation on one expected-writer pinned connection. The callback's
// result is withheld until both the pre-commit session check and the facade's
// independent post-callback identity check have succeeded.
func withRAGFairQueueLifecycleTx[T any](
	ctx context.Context,
	s *RAGFairQueueStore,
	preflight func(*sql.Conn) (ragOwnershipRoute, error),
	fn func(*sql.Tx, ragOwnershipRoute) (T, error),
) (T, error) {
	var zero T
	if preflight == nil || fn == nil {
		return zero, errors.New("store: nil RAG fair queue lifecycle callback")
	}
	var result T
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			route, err := preflight(conn)
			if err != nil {
				return err
			}
			if err := verifyFairQueueMySQLSession(ctx, conn, identity, ""); err != nil {
				return err
			}
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
					callbackErr = errors.Join(
						callbackErr, ErrFairQueueUnsafeConnection, rollbackErr,
					)
				}
			}()
			candidate, err := fn(tx, route)
			if err != nil {
				return err
			}
			if err := verifyFairQueueMySQLSession(ctx, tx, identity, ""); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, err)
			}
			committed = true
			result = candidate
			return nil
		})
	if err != nil {
		return zero, err
	}
	return result, nil
}

func (s *RAGFairQueueStore) CreateRAGDocumentWithVersionAndIndexTask(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
) (int64, error) {
	return s.createRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry, nil)
}

func (s *RAGFairQueueStore) CreateRAGDocumentWithVersionAndIndexTaskPolicy(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
	policy RAGAdvancedEnqueuePolicy,
) (int64, error) {
	return s.createRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry, &policy)
}

func (s *RAGFairQueueStore) createRAGDocumentWithVersionAndIndexTask(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
	policy *RAGAdvancedEnqueuePolicy,
) (int64, error) {
	if err := s.validate(); err != nil {
		return 0, err
	}
	if doc == nil || version == nil {
		return 0, ErrRAGDocumentVersionMismatch
	}
	docCopy := *doc
	versionCopy := *version
	if err := prepareRAGDocumentWithVersionAndIndexTask(&docCopy, &versionCopy); err != nil {
		return 0, err
	}
	return withRAGFairQueueLifecycleTx(ctx, s,
		func(conn *sql.Conn) (ragOwnershipRoute, error) {
			var userID string
			err := conn.QueryRowContext(ctx, fmt.Sprintf(
				`SELECT user_id FROM rag_kbs WHERE id=%s`, s.store.ph(1),
			), docCopy.KBID).Scan(&userID)
			if errors.Is(err, sql.ErrNoRows) {
				return ragOwnershipRoute{}, ErrRAGLifecycleInactive
			}
			if err != nil {
				return ragOwnershipRoute{}, err
			}
			return ragOwnershipRoute{KBID: docCopy.KBID, UserID: userID}, nil
		},
		func(tx *sql.Tx, route ragOwnershipRoute) (int64, error) {
			expectedOwner := route.UserID
			if policy != nil {
				expectedOwner = policy.UserID
			}
			return s.store.createRAGDocumentWithVersionAndIndexTaskInTx(
				ctx, tx, &docCopy, &versionCopy, maxRetry, policy, expectedOwner,
			)
		})
}

func (s *RAGFairQueueStore) AdvanceDocumentVersionAndCreateTask(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
) (*RAGIndexTaskRecord, error) {
	return s.advanceDocumentVersionAndCreateTask(ctx, expectedVersion, snapshot, nil)
}

func (s *RAGFairQueueStore) AdvanceDocumentVersionAndCreateTaskPolicy(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
	policy RAGAdvancedEnqueuePolicy,
) (*RAGIndexTaskRecord, error) {
	return s.advanceDocumentVersionAndCreateTask(ctx, expectedVersion, snapshot, &policy)
}

func (s *RAGFairQueueStore) advanceDocumentVersionAndCreateTask(
	ctx context.Context,
	expectedVersion int64,
	snapshot *RAGDocumentVersionRecord,
	policy *RAGAdvancedEnqueuePolicy,
) (*RAGIndexTaskRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if snapshot == nil || snapshot.DocID == "" {
		return nil, ErrRAGDocumentVersionMismatch
	}
	snapshotCopy := *snapshot
	return withRAGFairQueueLifecycleTx(ctx, s,
		func(conn *sql.Conn) (ragOwnershipRoute, error) {
			return s.store.ragOwnershipRouteOn(ctx, conn, snapshotCopy.DocID)
		},
		func(tx *sql.Tx, route ragOwnershipRoute) (*RAGIndexTaskRecord, error) {
			return s.store.advanceDocumentVersionAndCreateTaskInTx(
				ctx, tx, expectedVersion, &snapshotCopy, policy, route,
			)
		})
}

func (s *RAGFairQueueStore) MarkRAGDocumentDeleting(
	ctx context.Context,
	id string,
) (*RAGDocumentRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	return withRAGFairQueueLifecycleTx(ctx, s,
		func(conn *sql.Conn) (ragOwnershipRoute, error) {
			return s.store.ragOwnershipRouteOn(ctx, conn, id)
		},
		func(tx *sql.Tx, route ragOwnershipRoute) (*RAGDocumentRecord, error) {
			return s.store.markRAGDocumentDeletingInTx(ctx, tx, id, route)
		})
}

func (s *RAGFairQueueStore) MarkRAGKBDeleting(
	ctx context.Context,
	id string,
) (*RAGKBRecord, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	return withRAGFairQueueLifecycleTx(ctx, s,
		func(conn *sql.Conn) (ragOwnershipRoute, error) {
			var userID string
			err := conn.QueryRowContext(ctx, fmt.Sprintf(
				`SELECT user_id FROM rag_kbs WHERE id=%s`, s.store.ph(1),
			), id).Scan(&userID)
			if err != nil {
				return ragOwnershipRoute{}, scanErr(err)
			}
			return ragOwnershipRoute{KBID: id, UserID: userID}, nil
		},
		func(tx *sql.Tx, route ragOwnershipRoute) (*RAGKBRecord, error) {
			return s.store.markRAGKBDeletingInTx(ctx, tx, id, route.UserID)
		})
}
