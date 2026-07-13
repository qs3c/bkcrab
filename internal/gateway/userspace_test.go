package gateway

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

// readUserScopeAgentDefaults must distinguish "user has no row" from
// "user explicitly chose the system default". EnsureAgent relies on the
// returned Model being empty in case 1 (fall through to owner/agent
// overlays) and non-empty in case 2 (pin chatter's choice past the
// overlay chain) — the only way to tell apart is reading the raw row,
// not the merged Setting() view.
func TestReadUserScopeAgentDefaults(t *testing.T) {
	db, err := store.NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()

	// No row → zero value.
	got := readUserScopeAgentDefaults(ctx, db, "chatter-a")
	if got.Model != "" {
		t.Fatalf("missing row should give empty model, got %q", got.Model)
	}

	// Empty userID is a system caller — never pin.
	if got := readUserScopeAgentDefaults(ctx, db, ""); got.Model != "" {
		t.Fatalf("empty userID should give empty model, got %q", got.Model)
	}

	// Set a user-scope model → reads back.
	if err := scope.SaveSetting(ctx, db, "chatter-a", "", "agents.defaults",
		map[string]interface{}{"model": "openai/gpt-5.5"}); err != nil {
		t.Fatalf("save chatter row: %v", err)
	}
	got = readUserScopeAgentDefaults(ctx, db, "chatter-a")
	if got.Model != "openai/gpt-5.5" {
		t.Fatalf("explicit user-scope: want openai/gpt-5.5, got %q", got.Model)
	}

	// A different user with no row still returns empty — chatter pins
	// are per-user, never spill across accounts.
	if got := readUserScopeAgentDefaults(ctx, db, "chatter-b"); got.Model != "" {
		t.Fatalf("other user's row should not leak, got %q", got.Model)
	}

	// A row that exists but has no model key (chatter cleared override
	// while keeping other defaults) reads as zero — fall-through, no pin.
	if err := scope.SaveSetting(ctx, db, "chatter-a", "", "agents.defaults",
		map[string]interface{}{"maxTokens": float64(8192)}); err != nil {
		t.Fatalf("rewrite chatter row without model: %v", err)
	}
	got = readUserScopeAgentDefaults(ctx, db, "chatter-a")
	if got.Model != "" {
		t.Fatalf("row without model key should not pin, got %q", got.Model)
	}
	if got.MaxTokens != 8192 {
		t.Fatalf("other fields should still parse, got MaxTokens=%d", got.MaxTokens)
	}
}

func TestLoadUserSpaceMigratesLegacyLearnerBeforeOrdinaryHydration(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("BKCRAB_HOME", home)
	db, err := store.NewDBStore("sqlite", "file:userspace-learner-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.SaveAgent(ctx, &store.AgentRecord{
		ID: "agent-a", UserID: "owner-a", Name: "Agent A", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	agentHome, err := config.AgentHomeDir("agent-a")
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "---\nname: Legacy\ndescription: pre-isolation learner\n---\n\n1. Keep this workflow.\n"
	legacyDir := filepath.Join(agentHome, "skills", "legacy")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "SKILL.md"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-a", "legacy", store.HashSkillContent(legacy), true); err != nil {
		t.Fatal(err)
	}

	remote := workspace.NewLocalFS(t.TempDir())
	manualRoot := filepath.Join(t.TempDir(), "skills")
	manualDir := filepath.Join(manualRoot, "manual")
	if err := os.MkdirAll(manualDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const manual = "---\nname: Manual\ndescription: ordinary remote skill\n---\n\n1. Stay installed.\n"
	if err := os.WriteFile(filepath.Join(manualDir, "SKILL.md"), []byte(manual), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := skills.SyncSkillUp(ctx, remote, "agent-a", "manual", manualRoot); err != nil {
		t.Fatal(err)
	}

	if _, err := loadUserSpace(ctx, "owner-a", bus.New(), db, remote, nil, nil, nil); err != nil {
		t.Fatal(err)
	}

	learner := skills.NewManager(skills.LearnerSkillsDir(agentHome), skills.DefaultManagerConfig())
	if got, ok := learner.Read("legacy"); !ok || got != legacy {
		t.Fatalf("legacy learner was lost before migration: got (%q, %v)", got, ok)
	}
	r, err := remote.Get(ctx, "agent-a", "", "", "learner-skills/legacy/SKILL.md")
	if err != nil {
		t.Fatalf("migrated learner missing from remote: %v", err)
	}
	r.Close()
	if _, err := os.Stat(filepath.Join(agentHome, "skills", "legacy", "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy source was not removed after verified migration: %v", err)
	}
}

func TestEnsureAgentMigratesLegacyLearnerBeforeOrdinaryHydration(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("BKCRAB_HOME", home)
	db, err := store.NewDBStore("sqlite", "file:ensure-agent-learner-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.SaveAgent(ctx, &store.AgentRecord{
		ID: "agent-a", UserID: "owner-a", Name: "Agent A", IsPublic: true, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	agentHome, err := config.AgentHomeDir("agent-a")
	if err != nil {
		t.Fatal(err)
	}
	const legacy = "---\nname: Legacy\ndescription: guest-first migration source\n---\n\n1. Keep this workflow.\n"
	legacyDir := filepath.Join(agentHome, "skills", "legacy")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "SKILL.md"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-a", "legacy", store.HashSkillContent(legacy), true); err != nil {
		t.Fatal(err)
	}

	remote := workspace.NewLocalFS(t.TempDir())
	manualRoot := filepath.Join(t.TempDir(), "skills")
	manualDir := filepath.Join(manualRoot, "manual")
	if err := os.MkdirAll(manualDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const manual = "---\nname: Manual\ndescription: ordinary remote skill\n---\n\n1. Stay installed.\n"
	if err := os.WriteFile(filepath.Join(manualDir, "SKILL.md"), []byte(manual), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := skills.SyncSkillUp(ctx, remote, "agent-a", "manual", manualRoot); err != nil {
		t.Fatal(err)
	}

	sp, err := loadUserSpace(ctx, "guest-a", bus.New(), db, remote, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.EnsureAgent(ctx, db, bus.New(), remote, "agent-a"); err != nil {
		t.Fatal(err)
	}

	learner := skills.NewManager(skills.LearnerSkillsDir(agentHome), skills.DefaultManagerConfig())
	if got, ok := learner.Read("legacy"); !ok || got != legacy {
		t.Fatalf("guest-first EnsureAgent lost legacy learner before migration: got (%q, %v)", got, ok)
	}
	r, err := remote.Get(ctx, "agent-a", "", "", "learner-skills/legacy/SKILL.md")
	if err != nil {
		t.Fatalf("guest-first migrated learner missing from remote: %v", err)
	}
	r.Close()
}
