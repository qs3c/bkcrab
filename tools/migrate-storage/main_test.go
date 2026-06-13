package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkclaw/internal/store"
)

func TestRunSQLiteToMySQL(t *testing.T) {
	dsn := os.Getenv("BKCLAW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("BKCLAW_TEST_MYSQL_DSN is not set")
	}

	ctx := context.Background()
	sqlitePath := filepath.Join(t.TempDir(), "source.db")
	src, err := store.NewDBStore("sqlite", "file:"+filepath.ToSlash(sqlitePath))
	if err != nil {
		t.Fatalf("open SQLite source: %v", err)
	}
	if err := src.Migrate(ctx); err != nil {
		t.Fatalf("migrate SQLite source: %v", err)
	}
	user := &store.UserRecord{
		ID:       "u_migrate_mysql",
		Username: "migrate-mysql",
		Email:    "migrate-mysql@example.com",
		Role:     "user",
		Status:   "active",
	}
	if err := src.CreateUser(ctx, user); err != nil {
		t.Fatalf("seed SQLite source: %v", err)
	}
	if err := src.Close(); err != nil {
		t.Fatalf("close SQLite source: %v", err)
	}

	if err := run(ctx, sqlitePath, "mysql", dsn, true); err != nil {
		t.Fatalf("migrate SQLite to MySQL: %v", err)
	}

	dst, err := store.NewDBStore("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL destination: %v", err)
	}
	defer dst.Close()
	got, err := dst.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("read migrated user: %v", err)
	}
	if got.Email != user.Email {
		t.Fatalf("migrated email = %q, want %q", got.Email, user.Email)
	}
}
