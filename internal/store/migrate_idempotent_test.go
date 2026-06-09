package store

import (
	"context"
	"strings"
	"testing"
)

// TestMigrateIdempotentOnFreshInstall guards against migrations that
// silently re-do work on a clean DB. openTestDB already runs Migrate
// once; we run it a second time and assert every check returns clean
// — any new migration that forgets the "if column exists, return"
// short-circuit will surface here as an error or a re-run side
// effect.
//
// The test is intentionally low-resolution: we don't poke into per-
// migration counters. We rely on Migrate being the union of every
// idempotent step, so two-back-to-back runs producing no error means
// none of them attempted a non-idempotent ALTER/UPDATE/DELETE.
func TestMigrateIdempotentOnFreshInstall(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// First Migrate already ran inside openTestDB. Run twice more to
	// triple-check — second run could lazily realize "I have stale
	// state" and act on it; third run must be the same as second.
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("re-Migrate iteration %d: %v", i, err)
		}
	}

	// Sanity: schema landed in the new shape (no `scope` column on
	// configs, user_id on cron_jobs, channel triple on sessions).
	missing := func(t *testing.T, table, column string, wantPresent bool) {
		t.Helper()
		has, err := db.tableHasColumn(ctx, table, column)
		if err != nil {
			t.Fatalf("tableHasColumn(%s, %s): %v", table, column, err)
		}
		if has != wantPresent {
			t.Errorf("%s.%s present = %v; want %v", table, column, has, wantPresent)
		}
	}
	// `scope` is back as a denormalized label column; `scope_id` is
	// gone for good. user_id/agent_id are the source of truth.
	missing(t, "configs", "scope", true)
	missing(t, "configs", "scope_id", false)
	missing(t, "configs", "user_id", true)
	missing(t, "configs", "agent_id", true)
	missing(t, "cron_jobs", "user_id", true)
	missing(t, "sessions", "channel", true)
	missing(t, "sessions", "account_id", true)
	missing(t, "sessions", "chat_id", true)

	// Spot-check that the indexes exist (the migrators all flow
	// through CREATE INDEX IF NOT EXISTS, so missing index would mean
	// the migration didn't reach the index step on fresh install).
	rows, err := db.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan index: %v", err)
		}
		names = append(names, n)
	}
	want := []string{
		"idx_configs_lookup",
		"idx_configs_credential",
		"idx_cron_jobs_user",
		"idx_sessions_chat_active",
	}
	joined := strings.Join(names, ",")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("expected index %q on fresh DB; got %s", w, joined)
		}
	}
}
