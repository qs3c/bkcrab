package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

// TestManagerWiresSkillsLearnerInProductionPath 锁定发现 1:生产环境经
// NewManager → buildAgent(用 NewAgentWithSkillsCfg)构造 agent,此前从不
// 装配 skillsLearner,导致技能提炼与生命周期在生产静默失效。修复后,只要
// 通过 WithSkillsLearner 传入启用配置,生产路径构造的 agent 必须带 learner。
func TestManagerWiresSkillsLearnerInProductionPath(t *testing.T) {
	rc := config.ResolvedAgent{
		ID:        "agentX",
		Model:     "prov/m",
		Home:      t.TempDir(),
		Workspace: t.TempDir(),
	}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("u1"),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 9}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ag := mgr.agents["agentX"]
	if ag == nil {
		t.Fatal("agent not built")
	}
	if ag.skillsLearner == nil {
		t.Fatal("production buildAgent must wire skills learner when enabled")
	}
	if ag.skillsLearner.agentID != "agentX" {
		t.Fatalf("learner agentID=%q want agentX", ag.skillsLearner.agentID)
	}
	if ag.skillsLearner.minToolCalls != 9 {
		t.Fatalf("learner minToolCalls=%d want 9", ag.skillsLearner.minToolCalls)
	}
}

// TestManagerSkipsLearnerWhenDisabled 确认显式关闭时生产路径不构造 learner。
func TestManagerSkipsLearnerWhenDisabled(t *testing.T) {
	no := false
	rc := config.ResolvedAgent{ID: "agentY", Model: "prov/m", Home: t.TempDir(), Workspace: t.TempDir()}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("u1"),
		WithSkillsLearner(config.SkillsLearnerCfg{Enabled: &no}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if ag := mgr.agents["agentY"]; ag == nil || ag.skillsLearner != nil {
		t.Fatalf("disabled learner must not be wired: %+v", ag)
	}
}

func TestManagerKeepsAgentOwnerSeparateFromForeignUserSpace(t *testing.T) {
	rc := config.ResolvedAgent{
		ID: "public-agent", UserID: "actual-owner", Model: "prov/m",
		Home: t.TempDir(), Workspace: t.TempDir(),
	}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("public-visitor"),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 9}),
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	a := mgr.agents[rc.ID]
	if a == nil {
		t.Fatal("agent not built")
	}
	if a.ownerUserID != "public-visitor" {
		t.Fatalf("runtime UserSpace owner = %q, want public-visitor", a.ownerUserID)
	}
	if a.agentOwnerUserID != "actual-owner" || a.OwnerUserID() != "actual-owner" {
		t.Fatalf("agent owner = field %q accessor %q, want actual-owner", a.agentOwnerUserID, a.OwnerUserID())
	}
	if a.isLearnerOwnerTurn(bus.InboundMessage{UserID: "public-visitor", PeerKind: "dm"}, "public-visitor") {
		t.Fatal("public visitor was treated as the agent owner")
	}
}

func TestManagerMigratesLegacyLearnerBeforeOrdinarySkillHydration(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	const legacyBody = "---\nname: Legacy\ndescription: legacy learner\n---\n\nLearned steps.\n"
	writeLegacyMigrationSkill(t, agentDir, "legacy", legacyBody, false)

	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "manager.db")) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-migrate", "legacy", store.HashSkillContent(legacyBody), true); err != nil {
		t.Fatal(err)
	}

	remote := workspace.NewLocalFS(t.TempDir())
	manualRoot := filepath.Join(t.TempDir(), "skills")
	const manualBody = "---\nname: Manual\ndescription: installed manual\n---\n\nManual steps.\n"
	if err := os.MkdirAll(filepath.Join(manualRoot, "manual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manualRoot, "manual", "SKILL.md"), []byte(manualBody), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := skills.SyncSkillUp(ctx, remote, "agent-migrate", "manual", manualRoot); err != nil {
		t.Fatal(err)
	}

	rc := config.ResolvedAgent{
		ID: "agent-migrate", UserID: "owner", Model: "prov/m",
		Home: agentDir, Workspace: t.TempDir(),
	}
	_, err = NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("owner"), WithDataStore(db), WithWorkspaceStore(remote),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 9}),
	)
	if err != nil {
		t.Fatal(err)
	}

	learner := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())
	if got, ok := learner.Read("legacy"); !ok || got != legacyBody {
		t.Fatalf("legacy learner was pruned before migration: (%q,%v)", got, ok)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "skills", "legacy")); !os.IsNotExist(err) {
		t.Fatalf("legacy source survived migration: %v", err)
	}
	if _, err := remote.Stat(ctx, "agent-migrate", "", "", "learner-skills/legacy/SKILL.md"); err != nil {
		t.Fatalf("migrated learner not persisted remotely: %v", err)
	}
	manualOnDisk, err := os.ReadFile(filepath.Join(agentDir, "skills", "manual", "SKILL.md"))
	if err != nil || string(manualOnDisk) != manualBody {
		t.Fatalf("ordinary remote skill was not hydrated after migration: body=%q err=%v", manualOnDisk, err)
	}
}
