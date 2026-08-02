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
	return d.withFairQueueExpectedWriterConn(ctx, expectedWriter,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			lockName := fairQueueCapacityLockName(identity.database, "rag.index")
			timeoutSeconds := timeout.Seconds()
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline).Seconds()
				if remaining <= 0 {
					return context.DeadlineExceeded
				}
				if remaining < timeoutSeconds {
					timeoutSeconds = remaining
				}
			}

			var acquired sql.NullInt64
			if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, lockName, timeoutSeconds).
				Scan(&acquired); err != nil {
				return errors.Join(
					ErrRAGFairQueueCapacityLockUnavailable,
					ErrFairQueueUnsafeConnection,
					err,
				)
			}
			if !acquired.Valid {
				return errors.Join(
					ErrRAGFairQueueCapacityLockUnavailable,
					ErrFairQueueUnsafeConnection,
				)
			}
			if acquired.Int64 != 1 {
				return ErrRAGFairQueueCapacityLockUnavailable
			}

			lockHeld := true
			defer func() {
				if !lockHeld {
					return
				}
				cleanupCtx, cancel := context.WithTimeout(
					context.Background(), fairQueueOperationCleanupTimeout,
				)
				defer cancel()
				var released sql.NullInt64
				releaseErr := conn.QueryRowContext(
					cleanupCtx, `SELECT RELEASE_LOCK(?)`, lockName,
				).Scan(&released)
				lockHeld = false
				if releaseErr != nil || !released.Valid || released.Int64 != 1 {
					callbackErr = errors.Join(
						callbackErr, ErrFairQueueUnsafeConnection, releaseErr,
					)
				}
			}()

			if err := verifyFairQueueMySQLSession(ctx, conn, identity, lockName); err != nil {
				// Releasing through a switched/unknown session is not authoritative.
				// The outer expected-writer fence will physically discard it.
				lockHeld = false
				return err
			}
			return fn(ragFairQueueCapacitySession{
				conn: conn, identity: identity, lockName: lockName,
			})
		})
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
