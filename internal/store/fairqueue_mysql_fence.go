package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// fairQueueCapacityLockName is shared by every resource. Keeping the database
// name in the digest prevents unrelated schemas on one server from sharing a
// capacity fence.
func fairQueueCapacityLockName(database, resource string) string {
	digest := sha256.Sum256([]byte(database + "\x00" + resource))
	return "bkcrab:fq:" + hex.EncodeToString(digest[:])[:48]
}

// withFairQueueResourceLock pins one authoritative writer session, acquires
// the resource advisory lock before any transaction begins, verifies that the
// same physical session still owns it, and treats uncertain release as fatal.
func (d *DBStore) withFairQueueResourceLock(
	ctx context.Context,
	expectedWriter, resource string,
	lockTimeout time.Duration,
	fn func(*sql.Conn, fairQueueMySQLIdentity) error,
) error {
	if resource == "" || lockTimeout <= 0 {
		return ErrFairQueueOperationInvalid
	}
	return d.withFairQueueExpectedWriterConn(ctx, expectedWriter,
		func(conn *sql.Conn, identity fairQueueMySQLIdentity) (callbackErr error) {
			lockName := fairQueueCapacityLockName(identity.database, resource)
			timeoutSeconds := lockTimeout.Seconds()
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
			if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, ?)`, lockName, timeoutSeconds).Scan(&acquired); err != nil {
				return errors.Join(ErrFairQueueStartLockUnavailable, ErrFairQueueUnsafeConnection, err)
			}
			if !acquired.Valid {
				return errors.Join(ErrFairQueueStartLockUnavailable, ErrFairQueueUnsafeConnection)
			}
			if acquired.Int64 != 1 {
				return ErrFairQueueStartLockUnavailable
			}

			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), fairQueueOperationCleanupTimeout)
				defer cancel()
				var released sql.NullInt64
				releaseErr := conn.QueryRowContext(cleanupCtx, `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
				if releaseErr != nil || !released.Valid || released.Int64 != 1 {
					callbackErr = errors.Join(callbackErr, ErrFairQueueUnsafeConnection, releaseErr)
				}
			}()
			if err := verifyFairQueueMySQLSession(ctx, conn, identity, lockName); err != nil {
				return err
			}
			return fn(conn, identity)
		})
}
