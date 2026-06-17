package store

import (
	"context"
	"testing"
)

// newTestSQLite 打开一个临时 SQLite 库并迁移。
func newTestSQLite(t *testing.T) *DBStore {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + dir + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestSessionMessagesHasTurnColumns(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	for _, col := range []string{"turn_status", "extraction_id"} {
		has, err := db.tableHasColumn(ctx, "session_messages", col)
		if err != nil {
			t.Fatalf("tableHasColumn(%s): %v", col, err)
		}
		if !has {
			t.Fatalf("session_messages missing column %s", col)
		}
	}
}

func TestMigrateTurnColumnsIdempotent(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	// 再迁移一次必须无错(幂等)。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// pending 索引必须存在。
	var name string
	err := db.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sm_pending'`).Scan(&name)
	if err != nil {
		t.Fatalf("idx_sm_pending not found: %v", err)
	}
}
