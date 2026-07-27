package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/lib/pq"
)

// ErrLastActiveSuperAdmin is returned when a user mutation would leave the
// installation without an active super administrator. Callers must use
// errors.Is: the mutation may be retried internally after a serialization
// failure before this stable business error is returned.
var ErrLastActiveSuperAdmin = errors.New("store: refusing to remove the last active super_admin")

const userMutationMaxAttempts = 8

// runSerializableUserMutation protects decisions based on the set of active
// super administrators. SERIALIZABLE is important here: locking only the
// target user does not prevent two administrators from concurrently disabling
// or deleting each other (a write-skew race). PostgreSQL rejects that cycle at
// commit, MySQL's SERIALIZABLE predicate reads deadlock one participant, and
// SQLite permits only one writer. Retrying the rejected participant makes it
// observe the winner and return ErrLastActiveSuperAdmin instead of a transient
// database error.
func (d *DBStore) runSerializableUserMutation(
	ctx context.Context,
	mutation func(*sql.Tx) error,
) error {
	var lastErr error
	for attempt := 0; attempt < userMutationMaxAttempts; attempt++ {
		tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
		if err != nil {
			if !isRetryableUserMutationError(err) {
				return err
			}
			lastErr = err
		} else {
			err = mutation(tx)
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
			if err == nil {
				return nil
			}
			// Commit may already have closed the transaction. Rollback is safe in
			// both cases and ensures a statement error never leaks a connection
			// with an open transaction back into the pool.
			_ = tx.Rollback()
			if !isRetryableUserMutationError(err) {
				return err
			}
			lastErr = err
		}

		if err := waitForUserMutationRetry(ctx, attempt); err != nil {
			return err
		}
	}
	return fmt.Errorf("store: serialize user mutation after %d attempts: %w",
		userMutationMaxAttempts, lastErr)
}

func waitForUserMutationRetry(ctx context.Context, attempt int) error {
	delay := time.Duration(1<<min(attempt, 5)) * 5 * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isRetryableUserMutationError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var postgresErr *pq.Error
	if errors.As(err, &postgresErr) {
		return postgresErr.Code == "40001" || // serialization_failure
			postgresErr.Code == "40P01" // deadlock_detected
	}

	var mysqlErr *mysqlDriver.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1205, // lock wait timeout
			1213: // deadlock
			return true
		}
	}

	// modernc SQLite exposes the extended result code in its concrete error,
	// but matching the stable symbolic/message forms keeps this helper decoupled
	// from that driver's internal API and also recognizes wrapped errors.
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "sqlite_busy") ||
		strings.Contains(message, "sqlite_locked") ||
		strings.Contains(message, "database is locked") ||
		strings.Contains(message, "database table is locked")
}

func (d *DBStore) userInTx(ctx context.Context, tx *sql.Tx, id string) (*UserRecord, error) {
	row := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE id = %s`, d.ph(1)), id)
	user, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return user, nil
}

func activeSuperAdmin(user *UserRecord) bool {
	return user != nil && strings.EqualFold(strings.TrimSpace(user.Role), "super_admin") &&
		strings.EqualFold(strings.TrimSpace(user.Status), "active")
}

func (d *DBStore) rejectLastActiveSuperAdminInTx(
	ctx context.Context,
	tx *sql.Tx,
	current, replacement *UserRecord,
) error {
	if !activeSuperAdmin(current) || activeSuperAdmin(replacement) {
		return nil
	}

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users
		WHERE LOWER(role) = 'super_admin' AND LOWER(status) = 'active'`).Scan(&count); err != nil {
		return err
	}
	if count <= 1 {
		return ErrLastActiveSuperAdmin
	}
	return nil
}

// updateUserWithLifecycleGuard is the transactional implementation used by
// DBStore.UpdateUser. Keeping it separate lets the legacy database.go method
// remain a small delegating entry point.
func (d *DBStore) updateUserWithLifecycleGuard(ctx context.Context, user *UserRecord) error {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return ErrNotFound
	}

	user.UpdatedAt = time.Now().UTC()
	return d.runSerializableUserMutation(ctx, func(tx *sql.Tx) error {
		current, err := d.userInTx(ctx, tx, user.ID)
		if err != nil {
			return err
		}
		if strings.EqualFold(current.Status, "deleting") {
			return ErrRAGLifecycleInactive
		}
		if err := d.rejectLastActiveSuperAdminInTx(ctx, tx, current, user); err != nil {
			return err
		}

		result, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE users SET username = %s, email = %s, password_hash = %s, display_name = %s,
				role = %s, status = %s, avatar_url = %s, agent_quota = %s, updated_at = %s
				WHERE id = %s AND LOWER(status) <> 'deleting'`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)),
			user.Username, user.Email, user.PasswordHash, user.DisplayName, user.Role, user.Status,
			user.AvatarURL, user.AgentQuota, user.UpdatedAt, user.ID)
		if err != nil {
			return err
		}
		updated, err := ragRowsAffected(result)
		if err != nil {
			return err
		}
		if !updated {
			// ClientFoundRows is enabled for MySQL, so a zero result means the
			// status CAS lost to a tombstone (or the row disappeared).
			latest, getErr := d.userInTx(ctx, tx, user.ID)
			if getErr != nil {
				return getErr
			}
			if strings.EqualFold(latest.Status, "deleting") {
				return ErrRAGLifecycleInactive
			}
		}
		return nil
	})
}

// markUserDeletingWithLifecycleGuard atomically checks the active-admin
// invariant and writes the durable user tombstone.
func (d *DBStore) markUserDeletingWithLifecycleGuard(ctx context.Context, id string) (*UserRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrNotFound
	}

	var marked *UserRecord
	err := d.runSerializableUserMutation(ctx, func(tx *sql.Tx) error {
		current, err := d.userInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if strings.EqualFold(current.Status, "deleting") {
			marked = current
			return nil
		}

		replacement := *current
		replacement.Status = "deleting"
		if err := d.rejectLastActiveSuperAdminInTx(ctx, tx, current, &replacement); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE users SET status='deleting',
			updated_at=%s WHERE id=%s`, d.ragNowExpr(), d.ph(1)), id); err != nil {
			return err
		}
		marked, err = d.userInTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return marked, nil
}
