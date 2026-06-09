package store

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateTokenUsageAddProvider exercises the upgrade path: an
// existing token_usage_daily without the provider column gets
// dropped and recreated with the new shape. The data loss is
// intentional (pre-release; the table only holds accrued counters).
func TestMigrateTokenUsageAddProvider(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db")

	st, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Plant the OLD schema (no provider column) so the migration
	// has something to upgrade.
	if _, err := st.DB().ExecContext(ctx, `CREATE TABLE token_usage_daily (
		day DATE NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		agent_id TEXT NOT NULL DEFAULT '',
		session_key TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		input_tokens BIGINT NOT NULL DEFAULT 0,
		output_tokens BIGINT NOT NULL DEFAULT 0,
		cache_read_tokens BIGINT NOT NULL DEFAULT 0,
		cache_create_tokens BIGINT NOT NULL DEFAULT 0,
		request_count BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (day, user_id, agent_id, session_key, model)
	)`); err != nil {
		t.Fatalf("plant old table: %v", err)
	}

	// Run the full Migrate(); the upgrade step should detect the
	// missing column and rebuild the table.
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// New shape must have a provider column.
	has, err := st.tableHasColumn(ctx, "token_usage_daily", "provider")
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !has {
		t.Fatalf("provider column missing after migration")
	}

	// And we should be able to UPSERT with the new key shape.
	_, err = st.DB().ExecContext(ctx, `INSERT INTO token_usage_daily
		(day, user_id, agent_id, session_key, provider, model,
		 input_tokens, output_tokens, cache_read_tokens, cache_create_tokens, request_count)
		VALUES ('2026-01-01','u','a','s','anthropic-messages','sonnet',1,2,0,0,1)`)
	if err != nil {
		t.Fatalf("insert into new shape: %v", err)
	}
}
