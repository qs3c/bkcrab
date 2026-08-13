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

// GetRAGKBForLifecycle reads API task-creation policy from the expected
// writer. The facade withholds the DTO until its post-read session check.
func (s *RAGFairQueueStore) GetRAGKBForLifecycle(
	ctx context.Context,
	id string,
) (*RAGKBRecord, error) {
	var record *RAGKBRecord
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			record, err = scanRAGKB(conn.QueryRowContext(ctx, fmt.Sprintf(
				`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s`, s.store.ph(1),
			), id))
			return scanErr(err)
		})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// GetRAGDocumentForLifecycle reads the canonical document snapshot used by
// fair-mode reindex/delete APIs from the expected writer.
func (s *RAGFairQueueStore) GetRAGDocumentForLifecycle(
	ctx context.Context,
	id string,
) (*RAGDocumentRecord, error) {
	var record *RAGDocumentRecord
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			record, err = scanRAGDocument(conn.QueryRowContext(ctx, fmt.Sprintf(
				`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE id=%s`, s.store.ph(1),
			), id))
			return scanErr(err)
		})
	if err != nil {
		return nil, err
	}
	return record, nil
}

// ListRAGDocumentsByKBForLifecycle keeps the upload quota snapshot on the
// expected writer and withholds the entire page after a session switch.
func (s *RAGFairQueueStore) ListRAGDocumentsByKBForLifecycle(
	ctx context.Context,
	kbID string,
) ([]RAGDocumentRecord, error) {
	var records []RAGDocumentRecord
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			rows, err := conn.QueryContext(ctx, fmt.Sprintf(
				`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE kb_id=%s ORDER BY uploaded_at,id`,
				s.store.ph(1),
			), kbID)
			if err != nil {
				return err
			}
			defer rows.Close()
			page := make([]RAGDocumentRecord, 0)
			for rows.Next() {
				record, err := scanRAGDocument(rows)
				if err != nil {
					return err
				}
				page = append(page, *record)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			records = page
			return nil
		})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// GetUserForRAGLifecycle binds the active-owner read used by upload/reindex
// policy to the same expected writer as subsequent lifecycle mutations.
func (s *RAGFairQueueStore) GetUserForRAGLifecycle(
	ctx context.Context,
	id string,
) (*UserRecord, error) {
	var record *UserRecord
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			record, err = scanUser(conn.QueryRowContext(ctx, fmt.Sprintf(
				`SELECT `+userColumns+` FROM users WHERE id=%s`, s.store.ph(1),
			), id))
			return scanErr(err)
		})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (s *RAGFairQueueStore) CreateRAGDocumentWithVersionAndIndexTask(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
) (int64, error) {
	return s.createRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry, nil)
}

// BeginOriginalRAGObjectWrite registers the upload object's immutable key on
// the expected writer before the document row and index task exist. This API
// lifecycle operation has no claim fence yet, so writer identity and the
// canonical KB owner are its fail-closed boundary.
func (s *RAGFairQueueStore) BeginOriginalRAGObjectWrite(
	ctx context.Context,
	request RAGObjectWriteRequest,
) (*RAGObjectWriteFence, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	request, err := normalizeRAGObjectWriteRequest(request)
	if err != nil {
		return nil, err
	}
	if request.ObjectKind != RAGObjectKindOriginal {
		return nil, ErrRAGDocumentVersionMismatch
	}
	var created *RAGObjectWriteFence
	err = s.withExpectedWriterTx(ctx, func(tx *sql.Tx) error {
		var coreErr error
		created, coreErr = s.store.beginRAGObjectWriteInTx(ctx, tx, request)
		return coreErr
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// MarkOriginalRAGObjectWriteReady acknowledges the exact immutable upload
// object on the expected writer. All locator fields are validated before the
// transition and the active KB owner is locked in the same transaction.
func (s *RAGFairQueueStore) MarkOriginalRAGObjectWriteReady(
	ctx context.Context,
	fence RAGObjectWriteFence,
) (bool, error) {
	if err := s.validate(); err != nil {
		return false, err
	}
	if fence.ObjectKind != RAGObjectKindOriginal {
		return false, ErrRAGDocumentVersionMismatch
	}
	var ready bool
	err := s.withExpectedWriterTx(ctx, func(tx *sql.Tx) error {
		if _, err := s.store.lockActiveRAGKBOwnerTx(
			ctx, tx, fence.KBID, fence.UserID,
		); err != nil {
			return err
		}
		var coreErr error
		ready, coreErr = s.store.markRAGObjectWriteReadyExactOn(ctx, tx, fence)
		return coreErr
	})
	if err != nil {
		return false, err
	}
	return ready, nil
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
				ctx, tx, &docCopy, &versionCopy, maxRetry, policy, expectedOwner, true,
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
