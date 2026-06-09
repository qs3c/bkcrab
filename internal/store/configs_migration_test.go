package store

import (
	"context"
	"testing"
)

// TestConfigsScopeMigration simulates a legacy install — recreates the
// pre-feature configs schema, seeds a representative mix of rows, then
// runs Migrate and checks the (user_id, agent_id) backfill produced
// the right ownership for each row family.
//
// Coverage:
//   - scope=system   → ('', '')
//   - scope=user     → (X, '')
//   - scope=agent + kind=channel → (agent.UserID, Y)
//   - scope=agent + kind=setting/name=bindings → row is dropped
//   - scope=agent + kind=setting (other) → ('', Y)
//   - scope=agent + kind=provider → ('', Y)
//   - cron_jobs.user_id is backfilled from agents
func TestConfigsScopeMigration(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Rebuild configs and cron_jobs from scratch with the pre-feature
	// schema so the migration has something to migrate. SQLite's
	// inline UNIQUE constraint references the columns, so an ALTER
	// DROP COLUMN approach fights the constraint engine — easier to
	// drop and recreate.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_configs_lookup`,
		`DROP INDEX IF EXISTS idx_configs_credential`,
		`DROP TABLE configs`,
		`CREATE TABLE configs (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			scope TEXT NOT NULL,
			scope_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (kind, scope, scope_id, name)
		)`,
		`DROP INDEX IF EXISTS idx_cron_jobs_user`,
		`ALTER TABLE cron_jobs DROP COLUMN user_id`,
	} {
		if _, err := db.db.ExecContext(ctx, stmt); err != nil {
			t.Skipf("can't simulate pre-feature schema: %v (sql: %s)", err, stmt)
		}
	}

	// Seed an agent so the channel-row backfill has a user_id to
	// recover.
	const ownerUID = "u_owner"
	const agentID = "agt_demo"
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, config) VALUES (?, ?, 'demo', '{}')`,
		agentID, ownerUID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	// Seed configs rows under the legacy schema.
	type seed struct {
		id, kind, scope, scopeID, name, data string
	}
	rows := []seed{
		{"cfg_sys", "provider", "system", "", "openai", `{"apiKey":"sk-system"}`},
		{"cfg_user", "provider", "user", ownerUID, "anthropic", `{"apiKey":"sk-user"}`},
		{"cfg_chan", "channel", "agent", agentID, "telegram", `{"accounts":{"@bot":{"botToken":"abc"}}}`},
		{"cfg_bindings", "setting", "agent", agentID, "bindings", `{"list":[{"agentId":"agt_demo"}]}`},
		{"cfg_defaults", "setting", "agent", agentID, "agents.defaults", `{"model":"openai/gpt"}`},
		{"cfg_aprov", "provider", "agent", agentID, "openrouter", `{"apiKey":"sk-agent"}`},
	}
	for _, s := range rows {
		if _, err := db.db.ExecContext(ctx,
			`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, data) VALUES (?, ?, ?, ?, ?, 1, ?)`,
			s.id, s.kind, s.scope, s.scopeID, s.name, s.data); err != nil {
			t.Fatalf("seed %s: %v", s.id, err)
		}
	}

	// Seed a cron_job whose user_id should be backfilled from agents.
	if _, err := db.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, created_at)
			VALUES ('cj_test', ?, 'test', 'cron', '0 0 * * *', 'msg', 'web', 'web-ui', '', 'UTC', 1, CURRENT_TIMESTAMP)`,
		agentID); err != nil {
		t.Fatalf("seed cron_job: %v", err)
	}

	// Run the migration.
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Bindings row got dropped.
	var bindCount int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM configs WHERE name = 'bindings'`).Scan(&bindCount); err != nil {
		t.Fatalf("count bindings: %v", err)
	}
	if bindCount != 0 {
		t.Errorf("bindings row not dropped: %d remain", bindCount)
	}

	// Verify each remaining row's (user_id, agent_id) pair.
	type expect struct {
		id          string
		wantUser    string
		wantAgent   string
		mustExist   bool
	}
	cases := []struct {
		id          string
		wantUser    string
		wantAgent   string
		wantScope   string
		mustExist   bool
	}{
		{"cfg_sys", "", "", "system", true},
		{"cfg_user", ownerUID, "", "user", true},
		{"cfg_chan", ownerUID, agentID, "user-agent", true},
		{"cfg_defaults", "", agentID, "agent", true},
		{"cfg_aprov", "", agentID, "agent", true},
		{"cfg_bindings", "", "", "", false},
	}
	for _, tc := range cases {
		row := db.db.QueryRowContext(ctx,
			`SELECT scope, user_id, agent_id FROM configs WHERE id = ?`, tc.id)
		var scope, uid, aid string
		err := row.Scan(&scope, &uid, &aid)
		if !tc.mustExist {
			if err == nil {
				t.Errorf("%s should be deleted but still exists with (scope=%q user=%q agent=%q)", tc.id, scope, uid, aid)
			}
			continue
		}
		if err != nil {
			t.Fatalf("scan %s: %v", tc.id, err)
		}
		if uid != tc.wantUser || aid != tc.wantAgent {
			t.Errorf("%s: got (user=%q agent=%q); want (user=%q agent=%q)",
				tc.id, uid, aid, tc.wantUser, tc.wantAgent)
		}
		if scope != tc.wantScope {
			t.Errorf("%s: scope=%q; want %q", tc.id, scope, tc.wantScope)
		}
	}

	// cron_jobs.user_id backfill.
	var cronUID string
	if err := db.db.QueryRowContext(ctx,
		`SELECT user_id FROM cron_jobs WHERE id = 'cj_test'`).Scan(&cronUID); err != nil {
		t.Fatalf("scan cron user_id: %v", err)
	}
	if cronUID != ownerUID {
		t.Errorf("cron user_id = %q; want %q", cronUID, ownerUID)
	}

	// Round-trip via the public API: SaveConfig + ListConfigs at the
	// new ownership level should still work (catches any forgotten
	// SQL that still references scope/scope_id).
	rec := &ConfigRecord{
		Kind:    KindProvider,
		UserID:  "u_new",
		AgentID: agentID,
		Name:    "deepseek",
		Enabled: true,
		Data:    map[string]interface{}{"apiKey": "sk-new"},
	}
	if err := db.SaveConfig(ctx, rec); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	listed, err := db.ListConfigs(ctx, KindProvider, "u_new", agentID)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "deepseek" {
		t.Fatalf("ListConfigs returned %+v; want one 'deepseek' row", listed)
	}
}
