package scope

import (
	"context"
	"testing"

	"github.com/bkclaw-ai/bkclaw/internal/store"
)

// Pins the chatter-first precedence that makes Timezone different from
// the generic Setting walk: the chatter's personal timezone must beat
// the agent's default. (Under Setting's outer→inner merge, agent scope
// would shadow user scope — wrong for a property of the person.)
func TestTimezonePrecedence_ChatterBeatsAgentBeatsSystem(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	chatter := "chatter-1"
	agentID := "agent-x"

	// Nothing set → empty (caller falls back to server-local).
	if tz := Timezone(ctx, db, chatter, agentID); tz != "" {
		t.Fatalf("unset: want empty, got %q", tz)
	}

	// system default
	if err := SaveSetting(ctx, db, "", "", PrefsNamespace,
		map[string]interface{}{"timezone": "UTC"}); err != nil {
		t.Fatalf("save system: %v", err)
	}
	if tz := Timezone(ctx, db, chatter, agentID); tz != "UTC" {
		t.Fatalf("system: want UTC, got %q", tz)
	}

	// agent default beats system
	if err := SaveSetting(ctx, db, "", agentID, PrefsNamespace,
		map[string]interface{}{"timezone": "Asia/Shanghai"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	if tz := Timezone(ctx, db, chatter, agentID); tz != "Asia/Shanghai" {
		t.Fatalf("agent: want Asia/Shanghai, got %q", tz)
	}

	// chatter's personal setting beats the agent default
	if err := SaveUserTimezone(ctx, db, chatter, "Europe/Berlin"); err != nil {
		t.Fatalf("save chatter: %v", err)
	}
	if tz := Timezone(ctx, db, chatter, agentID); tz != "Europe/Berlin" {
		t.Fatalf("chatter: want Europe/Berlin, got %q", tz)
	}

	// ...and follows the chatter to another agent.
	if tz := Timezone(ctx, db, chatter, "agent-other"); tz != "Europe/Berlin" {
		t.Fatalf("other agent: want Europe/Berlin, got %q", tz)
	}

	// A different chatter of the same agent still sees the agent default.
	if tz := Timezone(ctx, db, "chatter-2", agentID); tz != "Asia/Shanghai" {
		t.Fatalf("other chatter: want Asia/Shanghai, got %q", tz)
	}

	// per-(chatter, agent) override is the most specific layer.
	if err := SaveSetting(ctx, db, chatter, agentID, PrefsNamespace,
		map[string]interface{}{"timezone": "America/New_York"}); err != nil {
		t.Fatalf("save chatter-agent: %v", err)
	}
	if tz := Timezone(ctx, db, chatter, agentID); tz != "America/New_York" {
		t.Fatalf("chatter-agent: want America/New_York, got %q", tz)
	}
}

// SaveUserTimezone must not clobber sibling keys in the prefs row.
func TestSaveUserTimezonePreservesOtherPrefs(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	if err := SaveSetting(ctx, db, "u1", "", PrefsNamespace,
		map[string]interface{}{"language": "zh-CN"}); err != nil {
		t.Fatalf("seed prefs: %v", err)
	}
	if err := SaveUserTimezone(ctx, db, "u1", "Asia/Shanghai"); err != nil {
		t.Fatalf("save timezone: %v", err)
	}
	rec, err := db.GetConfigByName(ctx, store.KindSetting, "u1", "", PrefsNamespace)
	if err != nil || rec == nil {
		t.Fatalf("read back prefs: %v", err)
	}
	if rec.Data["language"] != "zh-CN" {
		t.Errorf("language pref clobbered: %v", rec.Data)
	}
	if rec.Data["timezone"] != "Asia/Shanghai" {
		t.Errorf("timezone not saved: %v", rec.Data)
	}
}
