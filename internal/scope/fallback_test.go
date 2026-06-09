package scope

import (
	"context"
	"testing"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// Verifies the agent → user → system precedence the dashboard promises
// in the Models page: agent-scope agents.defaults.model must win over a
// user-scope override, which must in turn win over the system default.
// Reported as "agent setting doesn't take effect" by users — this test
// pins the contract so a future store / merge refactor surfaces a
// regression instead of silently flipping precedence.
func TestSettingPrecedence_AgentBeatsUserBeatsSystem(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	userID := "user-a"
	agentID := "agent-x"

	// system: default model
	if err := SaveSetting(ctx, db, "", "", "agents.defaults",
		map[string]interface{}{"model": "deepseek/deepseek-v4-pro"}); err != nil {
		t.Fatalf("save system: %v", err)
	}
	// user-scope override
	if err := SaveSetting(ctx, db, userID, "", "agents.defaults",
		map[string]interface{}{"model": "openai/gpt-5.5"}); err != nil {
		t.Fatalf("save user: %v", err)
	}

	// At this point, user wins over system.
	var got config.AgentDefaults
	if err := SettingInto(ctx, db, "agents.defaults", userID, agentID, &got); err != nil {
		t.Fatalf("setting into: %v", err)
	}
	if got.Model != "openai/gpt-5.5" {
		t.Fatalf("user should beat system: want openai/gpt-5.5, got %q", got.Model)
	}

	// agent-scope override on top
	if err := SaveSetting(ctx, db, "", agentID, "agents.defaults",
		map[string]interface{}{"model": "anthropic/claude-opus-4.7"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	got = config.AgentDefaults{}
	if err := SettingInto(ctx, db, "agents.defaults", userID, agentID, &got); err != nil {
		t.Fatalf("setting into: %v", err)
	}
	if got.Model != "anthropic/claude-opus-4.7" {
		t.Fatalf("agent should beat user: want anthropic/claude-opus-4.7, got %q", got.Model)
	}

	// Verify the raw agent-scope row reads what we wrote, independent of
	// the merge — the loadUserSpace overlay path reads this directly, not
	// via Setting(), so a row malformed at write time would still slip
	// past the merge test.
	rec, err := db.GetConfigByName(ctx, store.KindSetting, "", agentID, "agents.defaults")
	if err != nil {
		t.Fatalf("get agent-scope row: %v", err)
	}
	if rec == nil {
		t.Fatal("agent-scope row missing after save")
	}
	if v, _ := rec.Data["model"].(string); v != "anthropic/claude-opus-4.7" {
		t.Fatalf("agent-scope row model: want anthropic/claude-opus-4.7, got %q", v)
	}

	// Delete agent-scope (empty data) → falls back to user-scope.
	if err := SaveSetting(ctx, db, "", agentID, "agents.defaults", nil); err != nil {
		t.Fatalf("delete agent-scope: %v", err)
	}
	got = config.AgentDefaults{}
	if err := SettingInto(ctx, db, "agents.defaults", userID, agentID, &got); err != nil {
		t.Fatalf("setting into after delete: %v", err)
	}
	if got.Model != "openai/gpt-5.5" {
		t.Fatalf("clear agent should fall back to user: want openai/gpt-5.5, got %q", got.Model)
	}
}
