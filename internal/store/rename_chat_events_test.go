package store

import (
	"context"
	"testing"
)

// TestRenameChatEventsToSessionEvents simulates a legacy install whose
// streaming events still live in `chat_events`. Migrate must rename
// the table + carry the index over without losing any rows.
func TestRenameChatEventsToSessionEvents(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Rebuild from scratch with the legacy name. Drop the new-name
	// table created by openTestDB's first Migrate, then create the
	// pre-rename table + seed a row.
	for _, stmt := range []string{
		`DROP TABLE session_events`,
		`CREATE TABLE chat_events (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL,
			seq INTEGER NOT NULL,
			type TEXT NOT NULL,
			data TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, agent_id, session_key, seq)
		)`,
		`CREATE INDEX idx_chat_events_lookup ON chat_events (user_id, agent_id, session_key, seq)`,
		`INSERT INTO chat_events (user_id, agent_id, session_key, seq, type, data) VALUES ('u', 'a', 's-1', 0, 'content', '{"text":"hi"}')`,
	} {
		if _, err := db.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("simulate legacy: %v (sql: %s)", err, stmt)
		}
	}

	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Old table is gone, new table is here, seeded row survived.
	hasOld, err := db.tableExists(ctx, "chat_events")
	if err != nil {
		t.Fatalf("tableExists chat_events: %v", err)
	}
	if hasOld {
		t.Errorf("chat_events still exists after rename")
	}
	hasNew, err := db.tableExists(ctx, "session_events")
	if err != nil {
		t.Fatalf("tableExists session_events: %v", err)
	}
	if !hasNew {
		t.Fatalf("session_events should exist after rename")
	}
	var seq int64
	var typ string
	if err := db.db.QueryRowContext(ctx,
		`SELECT seq, type FROM session_events WHERE user_id='u' AND agent_id='a' AND session_key='s-1'`).Scan(&seq, &typ); err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	if seq != 0 || typ != "content" {
		t.Errorf("seeded row corrupted: seq=%d type=%q", seq, typ)
	}

	// Public API should now work end-to-end against the renamed table.
	newSeq, err := db.AppendSessionEvent(ctx, "u", "a", "s-1", "done", []byte("{}"))
	if err != nil {
		t.Fatalf("AppendSessionEvent: %v", err)
	}
	if newSeq <= seq {
		t.Errorf("seq did not advance: got %d, prior %d", newSeq, seq)
	}

	// Index must exist under the new name.
	rows, err := db.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='session_events'`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if n == "idx_session_events_lookup" {
			found = true
		}
	}
	if !found {
		t.Errorf("idx_session_events_lookup missing post-rename")
	}

	// Idempotency: running Migrate again is a no-op.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("re-Migrate after rename: %v", err)
	}
}
