package usage_test

import (
	"context"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/fastclaw-ai/fastclaw/internal/store"
	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// TestSQLMeterRecordAndQuery runs the meter end-to-end against an
// on-disk SQLite created by the real store migration. It verifies:
//   - UPSERT accumulates across multiple RecordTokens calls
//   - Totals sums correctly across the day
//   - TopAgents / TopUsers order by combined tokens desc
//   - The "system" (empty user_id) row survives recording and ranks alongside named users
func TestSQLMeterRecordAndQuery(t *testing.T) {
	dir := t.TempDir()
	dsn := "file:" + filepath.Join(dir, "test.db") + "?_pragma=foreign_keys(1)"

	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	m := usage.NewSQLMeter(st.DB(), "sqlite")
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	// Two calls into the same (agent, user, provider, model) bucket — must merge.
	must(m.RecordTokens(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 100, Output: 50}))
	must(m.RecordTokens(ctx, "alice", "agentA", "sess1", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 200, Output: 80, CacheRead: 30}))

	// Different agent, different user.
	must(m.RecordTokens(ctx, "bob", "agentB", "sess2", "anthropic-messages", "sonnet-4-6",
		usage.Tokens{Input: 500, Output: 1000}))

	// system row (empty user_id) — separately, an empty-provider record
	// to verify the column accepts the shared-provider sentinel.
	must(m.RecordTokens(ctx, "", "agentA", "cron-tick", "", "sonnet-4-6",
		usage.Tokens{Input: 10, Output: 5}))

	r := usage.LastN(1)
	tot, err := m.Totals(ctx, r)
	if err != nil {
		t.Fatalf("totals: %v", err)
	}
	wantIn := int64(100 + 200 + 500 + 10)
	wantOut := int64(50 + 80 + 1000 + 5)
	if tot.Input != wantIn || tot.Output != wantOut || tot.CacheRead != 30 {
		t.Errorf("totals = %+v, want in=%d out=%d cache=30", tot, wantIn, wantOut)
	}
	if tot.Requests != 4 {
		t.Errorf("request_count = %d, want 4", tot.Requests)
	}

	// Verify UPSERT collapsed the two alice calls into one row.
	var rowCount int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM token_usage_daily`).Scan(&rowCount); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rowCount != 3 {
		t.Errorf("row count = %d, want 3 (alice+bob+system)", rowCount)
	}

	agents, err := m.TopAgents(ctx, r, 10)
	if err != nil {
		t.Fatalf("top agents: %v", err)
	}
	// agentB has 1500 tokens (single call), agentA has alice's
	// 100+50+200+80+30 = 460 plus system's 10+5 = 15 = 475.
	if len(agents) != 2 || agents[0].Key != "agentB" || agents[0].Tokens != 1500 {
		t.Errorf("top agents head = %+v, want agentB=1500", agents)
	}
	if agents[1].Key != "agentA" || agents[1].Tokens != 475 {
		t.Errorf("top agents tail = %+v, want agentA=475", agents[1])
	}

	users, err := m.TopUsers(ctx, r, 10)
	if err != nil {
		t.Fatalf("top users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("top users len = %d, want 3 (alice/bob/system)", len(users))
	}
	if users[0].Key != "bob" || users[0].Tokens != 1500 {
		t.Errorf("top users head = %+v, want bob=1500", users[0])
	}
}
