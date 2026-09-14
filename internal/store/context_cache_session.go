package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrSessionConflict = errors.New("session changed in another worker; reload before continuing")

// lockSessionRevision serializes writers using the durable outbox row, including
// first creation. The SQL trigger advances it in the same transaction as data.
func (d *DBStore) lockSessionRevision(ctx context.Context, tx *sql.Tx, u, a, k string) (int64, error) {
	stmt := fmt.Sprintf("INSERT INTO context_cache_changes(kind,s1,s2,s3,revision,applied) VALUES ('session',%s,%s,%s,0,0)", d.ph(1), d.ph(2), d.ph(3))
	if d.dialect == mysqlDialect {
		stmt += " ON DUPLICATE KEY UPDATE kind=kind"
	} else {
		stmt += " ON CONFLICT(kind,s1,s2,s3) DO NOTHING"
	}
	if _, err := tx.ExecContext(ctx, stmt, u, a, k); err != nil {
		return 0, err
	}
	lock := ""
	if d.dialect != "sqlite" {
		lock = " FOR UPDATE"
	}
	var rev int64
	err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT revision FROM context_cache_changes WHERE kind='session' AND s1=%s AND s2=%s AND s3=%s", d.ph(1), d.ph(2), d.ph(3))+lock, u, a, k).Scan(&rev)
	return rev, err
}

type contextRevisionKey struct{}

func WithExpectedSessionRevision(ctx context.Context, revision int64) context.Context {
	return context.WithValue(ctx, contextRevisionKey{}, revision)
}
func expectedSessionRevision(ctx context.Context) (int64, bool) {
	r, ok := ctx.Value(contextRevisionKey{}).(int64)
	return r, ok
}

func (d *DBStore) SessionRevision(ctx context.Context, u, a, k string) (int64, error) {
	return d.cacheRevision(ctx, cacheScope{"session", u, a, k})
}

// GetSessionAfterMiss bypasses negative cache entries. An absent workset and its
// tombstone revision must describe the same source state, or a concurrent create
// could lend its revision to an empty workset and authorize a destructive save.
func (d *DBStore) GetSessionAfterMiss(ctx context.Context, u, a, k string) (*SessionRecord, error) {
	for attempt := 0; attempt < 3; attempt++ {
		before, err := d.SessionRevision(ctx, u, a, k)
		if err != nil {
			return nil, err
		}
		rec, err := d.getSessionUncached(ctx, u, a, k)
		if !errors.Is(err, ErrNotFound) {
			return rec, err // Existing rows include the revision in the same SELECT.
		}
		after, err := d.SessionRevision(ctx, u, a, k)
		if err != nil {
			return nil, err
		}
		if before == after {
			return &SessionRecord{Revision: after}, nil
		}
	}
	return nil, ErrSessionConflict
}
