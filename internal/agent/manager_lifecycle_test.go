package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type failingLearnerHydrateStore struct{ workspace.Store }

func (f *failingLearnerHydrateStore) List(context.Context, string, string, string) ([]workspace.ObjectInfo, error) {
	return nil, errors.New("object store unavailable")
}

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

func TestManagerDisablesCadenceCreateAfterLearnerHydrateFailure(t *testing.T) {
	home := t.TempDir()
	provider := &learnerFakeProvider{responses: []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "create", "slug": "unsafe-create", "content": learnerValidSkill}),
		textResp("done"),
	}}
	remote := &failingLearnerHydrateStore{Store: workspace.NewLocalFS(t.TempDir())}
	rc := config.ResolvedAgent{ID: "hydrate-fail", Model: "prov/m", Home: home, Workspace: t.TempDir()}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, provider, bus.New(),
		WithUserID("owner"), WithWorkspaceStore(remote),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 10}),
	)
	if err != nil {
		t.Fatal(err)
	}
	learner := mgr.agents[rc.ID].skillsLearner
	if learner == nil || learner.createDisabledReason == "" {
		t.Fatalf("learner create health = %+v, want fail-closed hydrate marker", learner)
	}
	if err := learner.extractFromSession(context.Background(), sessionMessagesFixture()); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSkill(t, home, "unsafe-create"); ok {
		t.Fatal("cadence created a skill after initial remote hydrate failed")
	}
	if len(provider.prompts) < 2 || !strings.Contains(provider.prompts[1], "not healthy") {
		t.Fatalf("create refusal was not returned to learner: %v", provider.prompts)
	}
}

func TestManagerRecoversSkillJobsOnlyInOwnerUserSpace(t *testing.T) {
	ctx := context.Background()
	dsn := "file:" + filepath.ToSlash(filepath.Join(t.TempDir(), "recovery.db")) + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	const (
		owner   = "recovery-owner"
		agentID = "recovery-agent"
		session = "recovery-session"
	)
	if err := db.SaveAgent(ctx, &store.AgentRecord{ID: agentID, UserID: owner, Name: "Recovery Agent"}); err != nil {
		t.Fatal(err)
	}
	ownerCtx := store.WithChatterUserID(ctx, owner)
	if err := db.SaveSession(ownerCtx, owner, agentID, session, &store.SessionRecord{
		Messages: []store.SessionMessage{{Role: "user", Content: "frozen owner workflow"}},
	}); err != nil {
		t.Fatal(err)
	}
	seq, err := db.AppendTurnAnchor(ownerCtx, owner, agentID, session, store.SessionMessage{Role: "user", Content: "trigger"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.FinishTurn(ownerCtx, owner, agentID, session, seq, 10); err != nil {
		t.Fatal(err)
	}
	job, err := db.EnqueueSkillExtractionJob(ctx, owner, agentID, session, owner, 10)
	if err != nil || job == nil {
		t.Fatalf("enqueue job=%+v err=%v", job, err)
	}

	rc := config.ResolvedAgent{
		ID: agentID, UserID: owner, Model: "prov/m",
		Home: t.TempDir(), Workspace: t.TempDir(),
	}
	guestProvider := &learnerFakeProvider{}
	guestManager, err := NewManager([]config.ResolvedAgent{rc}, guestProvider, bus.New(),
		WithUserID("public-visitor"), WithDataStore(db),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 10}),
	)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	var status string
	if err := db.DB().QueryRow(`SELECT status FROM skill_extraction_jobs WHERE id=?`, job.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || guestProvider.calls != 0 {
		t.Fatalf("guest UserSpace recovered owner job: status=%q calls=%d", status, guestProvider.calls)
	}
	closeCtx, closeCancel := context.WithTimeout(ctx, 5*time.Second)
	if err := guestManager.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	closeCancel()

	ownerProvider := &learnerFakeProvider{}
	ownerManager, err := NewManager([]config.ResolvedAgent{rc}, ownerProvider, bus.New(),
		WithUserID(owner), WithDataStore(db),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 10}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = ownerManager.Close(closeCtx)
		cancel()
	})
	if outcome := waitForJobState(t, db, job.ID, "completed"); outcome != SkillExtractionSkipped {
		t.Fatalf("owner recovery outcome=%q want skip", outcome)
	}
	if ownerProvider.calls != 1 {
		t.Fatalf("owner recovery provider calls=%d want 1", ownerProvider.calls)
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

func TestDisabledLearnerStillMigratesLegacyAssetsIntoIsolatedNamespace(t *testing.T) {
	ctx := context.Background()
	agentDir := t.TempDir()
	const content = "---\nname: Legacy\ndescription: legacy learned workflow\n---\nsteps\n"
	writeLegacyMigrationSkill(t, agentDir, "legacy-disabled", content, false)
	db, err := store.NewDBStore("sqlite", "file:disabled-learner-migration?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(ctx, "disabled-agent", "legacy-disabled", store.HashSkillContent(content), true); err != nil {
		t.Fatal(err)
	}
	disabled := false
	rc := config.ResolvedAgent{
		ID: "disabled-agent", UserID: "owner", Model: "prov/m",
		Home: agentDir, Workspace: t.TempDir(),
	}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("owner"), WithDataStore(db),
		WithSkillsLearner(config.SkillsLearnerCfg{Enabled: &disabled}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if mgr.AgentByID(rc.ID).skillsLearner != nil {
		t.Fatal("disabled learner unexpectedly enabled extraction")
	}
	isolated := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())
	if got, ok := isolated.Read("legacy-disabled"); !ok || got != content {
		t.Fatalf("disabled learner legacy asset not isolated: (%q,%v)", got, ok)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "skills", "legacy-disabled")); !os.IsNotExist(err) {
		t.Fatalf("legacy mixed-namespace source survived: %v", err)
	}
}

func TestManagerDoesNotBuildTombstonedAgent(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", "file:manager-tombstoned-agent?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAgentDeleting(ctx, "deleted-agent"); err != nil {
		t.Fatal(err)
	}
	rc := config.ResolvedAgent{
		ID: "deleted-agent", UserID: "owner", Model: "prov/m",
		Home: t.TempDir(), Workspace: t.TempDir(),
	}
	mgr, err := NewManager([]config.ResolvedAgent{rc}, nil, bus.New(),
		WithUserID("owner"), WithDataStore(db),
		WithSkillsLearner(config.SkillsLearnerCfg{MinToolCalls: 10}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := mgr.AgentByID(rc.ID); got != nil {
		t.Fatalf("tombstoned agent was rebuilt: %+v", got)
	}
	if err := mgr.AddAgent(rc, nil, bus.New()); err == nil {
		t.Fatal("AddAgent accepted tombstoned agent")
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

func TestForeignAgentUsesOwnerLifecyclePolicy(t *testing.T) {
	visitorCfg := config.SkillsLearnerCfg{Lifecycle: config.SkillLifecycleCfg{ActiveMax: 1, HalfLifeLoads: 1, ExplicitGain: 999}}
	ownerCfg := config.SkillsLearnerCfg{Lifecycle: config.SkillLifecycleCfg{ActiveMax: 7, HalfLifeLoads: 40, ExplicitGain: 4}}
	mgr, err := NewManager(nil, nil, bus.New(), WithUserID("visitor"), WithSkillsLearner(visitorCfg))
	if err != nil {
		t.Fatal(err)
	}
	rc := config.ResolvedAgent{
		ID: "shared-agent", UserID: "owner", Model: "prov/m",
		Home: t.TempDir(), Workspace: t.TempDir(),
	}
	if err := mgr.AddAgentWithOwnerPolicies(rc, nil, bus.New(), config.SkillsCfg{}, ownerCfg); err != nil {
		t.Fatal(err)
	}
	ag := mgr.AgentByID(rc.ID)
	if ag == nil {
		t.Fatal("foreign agent was not added")
	}
	if ag.lifecycleCfg.ActiveMax != ownerCfg.Lifecycle.ActiveMax ||
		ag.lifecycleCfg.HalfLifeLoads != ownerCfg.Lifecycle.HalfLifeLoads ||
		ag.lifecycleCfg.ExplicitGain != ownerCfg.Lifecycle.ExplicitGain {
		t.Fatalf("foreign agent lifecycle=%+v, want owner policy %+v", ag.lifecycleCfg, ownerCfg.Lifecycle)
	}
	if ag.ownsLearnerAssets() {
		t.Fatal("foreign UserSpace became learner asset owner")
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
