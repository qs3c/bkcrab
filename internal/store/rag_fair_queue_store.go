package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RAGFairQueueStore binds every fair-queue read and mutation to the same
// authoritative MySQL writer identity. It is deliberately separate from the
// legacy DBStore surface so disabling fair scheduling keeps the old execution
// path unchanged.
type RAGFairQueueStore struct {
	store          *DBStore
	expectedWriter string
}

// BindRAGFairQueueWriter creates an expected-writer-bound RAG store facade.
// Fingerprint shape is checked before the database dialect so malformed
// control-plane identity always fails as a writer mismatch.
func (d *DBStore) BindRAGFairQueueWriter(expected string) (*RAGFairQueueStore, error) {
	if !lowerHex64Pattern.MatchString(expected) {
		return nil, ErrFairQueueWriterMismatch
	}
	if d == nil || d.db == nil || d.dialect != mysqlDialect {
		return nil, ErrFairQueueMySQLRequired
	}
	return &RAGFairQueueStore{store: d, expectedWriter: expected}, nil
}

// ExpectedWriterFingerprint returns the credential-free identity required by
// every operation on this facade.
func (s *RAGFairQueueStore) ExpectedWriterFingerprint() string {
	if s == nil {
		return ""
	}
	return s.expectedWriter
}

// GetConfigByName reads execution-affecting user configuration from the
// expected writer on one pinned physical connection. The snapshot is withheld
// if the post-read session identity check fails.
func (s *RAGFairQueueStore) GetConfigByName(
	ctx context.Context,
	kind, userID, agentID, name string,
) (*ConfigRecord, error) {
	var record *ConfigRecord
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			record, err = scanConfigRow(conn.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT `+configSelectCols+`
					FROM configs WHERE kind = %s AND user_id = %s AND agent_id = %s AND name = %s`,
					s.store.ph(1), s.store.ph(2), s.store.ph(3), s.store.ph(4)),
				kind, userID, agentID, name))
			return err
		})
	if err != nil {
		return nil, err
	}
	return record, nil
}

func (s *RAGFairQueueStore) validate() error {
	if s == nil || !lowerHex64Pattern.MatchString(s.expectedWriter) {
		return ErrFairQueueWriterMismatch
	}
	if s.store == nil || s.store.db == nil || s.store.dialect != mysqlDialect {
		return ErrFairQueueMySQLRequired
	}
	return nil
}

func (s *RAGFairQueueStore) withExpectedWriterConn(
	ctx context.Context,
	fn func(*sql.Conn, fairQueueMySQLIdentity) error,
) error {
	if err := s.validate(); err != nil {
		return err
	}
	return s.store.withFairQueueExpectedWriterConn(ctx, s.expectedWriter, fn)
}

// withExpectedWriterTx keeps a mutation uncommitted until the transaction has
// revalidated the exact server/database/session identity observed before it
// began. The outer pinned-connection helper performs a second verification
// after commit and physically discards an unsafe connection.
func (s *RAGFairQueueStore) withExpectedWriterTx(
	ctx context.Context,
	fn func(*sql.Tx) error,
) error {
	return s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
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
		})
}

func (s *RAGFairQueueStore) ListDispatchableRAGIndexTasksPage(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	var page []RAGIndexTaskDispatchCandidate
	var next int64
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			page, next, err = s.store.listDispatchableRAGIndexTasksPageOn(
				ctx, conn, afterID, limit,
			)
			return err
		})
	if err != nil {
		return nil, afterID, err
	}
	return page, next, nil
}

func (s *RAGFairQueueStore) GetDispatchableRAGIndexTaskByID(
	ctx context.Context,
	taskID int64,
) (*RAGIndexTaskDispatchCandidate, error) {
	var candidate *RAGIndexTaskDispatchCandidate
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			candidate, err = s.store.getDispatchableRAGIndexTaskByIDOn(ctx, conn, taskID)
			return err
		})
	if err != nil {
		return nil, err
	}
	return candidate, nil
}

func (s *RAGFairQueueStore) MarkRAGIndexTaskDispatched(
	ctx context.Context,
	candidate RAGIndexTaskDispatchCandidate,
) (bool, error) {
	var changed bool
	err := s.withExpectedWriterTx(ctx, func(tx *sql.Tx) error {
		var err error
		changed, err = s.store.markRAGIndexTaskDispatchedOn(ctx, tx, candidate)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func (s *RAGFairQueueStore) ArmExpiredRAGIndexTasksPage(
	ctx context.Context,
	afterID int64,
	limit int,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	var page []RAGIndexTaskDispatchCandidate
	var next int64
	err := s.withRAGFairQueueCapacityTx(ctx, fairQueueOperationStartLockTimeout, func(tx *sql.Tx) error {
		var err error
		page, next, err = s.store.armExpiredRAGIndexTasksPageOn(ctx, tx, afterID, limit)
		return err
	})
	if err != nil {
		return nil, afterID, err
	}
	return page, next, nil
}

func (s *RAGFairQueueStore) CaptureRAGFairQueueHighWater(
	ctx context.Context,
) (int64, error) {
	var highWater int64
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			highWater, err = s.store.captureRAGFairQueueHighWaterOn(ctx, conn)
			return err
		})
	if err != nil {
		return 0, err
	}
	return highWater, nil
}

func (s *RAGFairQueueStore) ListCanonicalRAGTenantsPage(
	ctx context.Context,
	highWater int64,
	afterUserID string,
	limit int,
) ([]string, string, error) {
	var tenants []string
	var next string
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			tenants, next, err = s.store.listCanonicalRAGTenantsPageOn(
				ctx, conn, highWater, afterUserID, limit,
			)
			return err
		})
	if err != nil {
		return nil, afterUserID, err
	}
	return tenants, next, nil
}

func (s *RAGFairQueueStore) ListDispatchedRAGIndexTasksPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskRecord, int64, error) {
	var page []RAGIndexTaskRecord
	var next int64
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			page, next, err = s.store.listDispatchedRAGIndexTasksPageOn(
				ctx, conn, highWater, afterTaskID, limit,
			)
			return err
		})
	if err != nil {
		return nil, afterTaskID, err
	}
	return page, next, nil
}

func (s *RAGFairQueueStore) ListValidRunningRAGIndexTasksPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskRunningSnapshot, int64, error) {
	var page []RAGIndexTaskRunningSnapshot
	var next int64
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			page, next, err = s.store.listValidRunningRAGIndexTasksPageOn(
				ctx, conn, highWater, afterTaskID, limit,
			)
			return err
		})
	if err != nil {
		return nil, afterTaskID, err
	}
	return page, next, nil
}

func (s *RAGFairQueueStore) CaptureRAGBrokerRepairHighWater(
	ctx context.Context,
) (int64, error) {
	var highWater int64
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			highWater, err = s.store.captureRAGFairQueueHighWaterOn(ctx, conn)
			return err
		})
	if err != nil {
		return 0, err
	}
	return highWater, nil
}

func (s *RAGFairQueueStore) ListBrokerBackedRAGCandidatesPage(
	ctx context.Context,
	highWater, afterTaskID int64,
	limit int,
) ([]RAGIndexTaskDispatchCandidate, int64, error) {
	var page []RAGIndexTaskDispatchCandidate
	var next int64
	err := s.withExpectedWriterConn(ctx,
		func(conn *sql.Conn, _ fairQueueMySQLIdentity) error {
			var err error
			page, next, err = s.store.listBrokerBackedRAGCandidatesPageOn(
				ctx, conn, highWater, afterTaskID, limit,
			)
			return err
		})
	if err != nil {
		return nil, afterTaskID, err
	}
	return page, next, nil
}

func (s *RAGFairQueueStore) RearmRAGCandidateAfterBrokerLoss(
	ctx context.Context,
	original RAGIndexTaskDispatchCandidate,
) (*RAGIndexTaskDispatchCandidate, bool, error) {
	var candidate *RAGIndexTaskDispatchCandidate
	var changed bool
	err := s.withExpectedWriterTx(ctx, func(tx *sql.Tx) error {
		var err error
		candidate, changed, err = s.store.rearmRAGCandidateAfterBrokerLossOn(
			ctx, tx, original,
		)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return candidate, changed, nil
}
