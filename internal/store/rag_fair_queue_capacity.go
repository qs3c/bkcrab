package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type ragFairQueueCapacitySession struct {
	conn     *sql.Conn
	identity fairQueueMySQLIdentity
	lockName string
}

// withRAGFairQueueCapacityLock serializes the MySQL admission boundary shared
// by exact claim, heartbeat and expired-lease rearm. The callback stays on the
// physical connection which owns the named lock. An uncertain acquisition,
// session switch or release physically discards that connection.
func (d *DBStore) withRAGFairQueueCapacityLock(
	ctx context.Context,
	expectedWriter string,
	timeout time.Duration,
	fn func(ragFairQueueCapacitySession) error,
) error {
	if timeout <= 0 {
		return errors.New("store: RAG fair queue capacity lock timeout must be positive")
	}
	err := d.withFairQueueResourceLock(ctx, expectedWriter, "rag.index", timeout,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) error {
			return fn(ragFairQueueCapacitySession{conn: conn, identity: identity,
				lockName: fairQueueCapacityLockName(identity.database, "rag.index")})
		})
	if errors.Is(err, ErrFairQueueStartLockUnavailable) {
		return errors.Join(ErrRAGFairQueueCapacityLockUnavailable, err)
	}
	return err
}

func (s *RAGFairQueueStore) withRAGFairQueueCapacityTx(
	ctx context.Context,
	timeout time.Duration,
	fn func(*sql.Tx) error,
) error {
	if err := s.validate(); err != nil {
		return err
	}
	return s.store.withRAGFairQueueCapacityLock(
		ctx, s.expectedWriter, timeout,
		func(session ragFairQueueCapacitySession) (callbackErr error) {
			tx, err := session.conn.BeginTx(ctx, nil)
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
			if err := verifyFairQueueMySQLSession(
				ctx, tx, session.identity, session.lockName,
			); err != nil {
				return err
			}
			if err := tx.Commit(); err != nil {
				return errors.Join(ErrFairQueueUnsafeConnection, err)
			}
			committed = true
			return nil
		},
	)
}
