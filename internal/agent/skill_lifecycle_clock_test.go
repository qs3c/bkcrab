package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

func configureLifecycleFixture(t *testing.T, a *Agent, db *store.DBStore, home string) {
	t.Helper()
	a.homePath = home
	a.homeDir = t.TempDir()
	a.agentID = a.name
	a.promptMode = config.PromptModeAgent
	a.lifecycleCfg = config.SkillLifecycleCfg{}
	a.skillsLearner.ledger = db
}

func TestRefreshSkillsAdvancesLifecycleWithoutObjectStore(t *testing.T) {
	a, db, home, _ := newCadenceFixture(t, nil)
	configureLifecycleFixture(t, a, db, home)
	if err := a.skillsLearner.Manager().Create("local-learner", learnerValidSkill); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(context.Background(), a.name, "local-learner", store.HashSkillContent(learnerValidSkill), true); err != nil {
		t.Fatal(err)
	}
	before, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil {
		t.Fatal(err)
	}
	dirs, summary := a.refreshSkillsFromStore(context.Background(), "u1")
	a.skillJobsWG.Wait()
	after, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil {
		t.Fatal(err)
	}
	if after != before+1 {
		t.Fatalf("catalog exposure did not advance lifecycle clock: before=%d after=%d", before, after)
	}
	if len(dirs) == 0 || !strings.Contains(summary, "local-learner") {
		t.Fatalf("local-only refresh did not expose the learner skill: dirs=%v summary=%q", dirs, summary)
	}
}

func TestRefreshSkillsDoesNotAdvanceWhenPromptModeHidesCatalog(t *testing.T) {
	a, db, home, _ := newCadenceFixture(t, nil)
	configureLifecycleFixture(t, a, db, home)
	before, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{config.PromptModeChatbot, config.PromptModeCustomize} {
		a.promptMode = mode
		a.refreshSkillsFromStore(context.Background(), "u1")
	}
	afterHidden, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil {
		t.Fatal(err)
	}
	if afterHidden != before {
		t.Fatalf("hidden catalog advanced lifecycle clock: before=%d after=%d", before, afterHidden)
	}
	a.promptMode = config.PromptModeAgent
	a.refreshSkillsFromStore(context.Background(), "u1")
	afterVisible, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil || afterVisible != before+1 {
		t.Fatalf("visible catalog clock=(%d,%v), want %d", afterVisible, err, before+1)
	}
}

func TestGuestUserSpaceCanUseButCannotAgeOwnerLearnerAssets(t *testing.T) {
	a, db, home, _ := newCadenceFixture(t, nil)
	configureLifecycleFixture(t, a, db, home)
	a.ownerUserID = "public-visitor"
	a.agentOwnerUserID = "u1"
	if err := a.skillsLearner.Manager().Create("shared-learner", learnerValidSkill); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(context.Background(), a.name, "shared-learner", store.HashSkillContent(learnerValidSkill), true); err != nil {
		t.Fatal(err)
	}
	before, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		_, summary := a.refreshSkillsFromStore(context.Background(), "public-visitor")
		if !strings.Contains(summary, "shared-learner") {
			t.Fatalf("guest exposure %d did not receive owner learner catalog", i)
		}
	}
	a.skillJobsWG.Wait()
	after, err := db.CurrentSkillLifecycleSeq(context.Background(), a.name)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("guest traffic aged owner lifecycle clock: before=%d after=%d", before, after)
	}
	if _, ok := a.skillsLearner.Manager().Read("shared-learner"); !ok {
		t.Fatal("guest traffic deleted owner learner asset")
	}
}

func TestLifecycleCleanupRunsWithoutSkillExtraction(t *testing.T) {
	a, db, home, fp := newCadenceFixture(t, nil)
	configureLifecycleFixture(t, a, db, home)
	a.lifecycleCfg = config.SkillLifecycleCfg{
		CleanupEveryTurns: 1,
		ProtectLoads:      1,
		EditProtectLoads:  1,
		DeleteAfterLoads:  1,
		ActiveMax:         10,
	}
	if err := db.SaveAgent(context.Background(), &store.AgentRecord{ID: a.name, UserID: "u1", Name: "Agent A"}); err != nil {
		t.Fatal(err)
	}
	if err := a.skillsLearner.Manager().Create("never-used", learnerValidSkill); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(context.Background(), a.name, "never-used", store.HashSkillContent(learnerValidSkill), true); err != nil {
		t.Fatal(err)
	}

	// First exposure only ages the asset to the boundary; deletion uses a strict
	// greater-than check so it remains available through that turn.
	a.refreshSkillsFromStore(context.Background(), "u1")
	a.skillJobsWG.Wait()
	if _, ok := a.skillsLearner.Manager().Read("never-used"); !ok {
		t.Fatal("skill was deleted before its configured age elapsed")
	}

	// The next catalog exposure independently schedules cleanup. No cadence job
	// or learner LLM call is involved.
	a.refreshSkillsFromStore(context.Background(), "u1")
	done := make(chan struct{})
	go func() {
		a.skillJobsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("independent lifecycle cleanup did not finish")
	}
	if _, ok := a.skillsLearner.Manager().Read("never-used"); ok {
		t.Fatal("stale learner asset survived independent cleanup")
	}
	rows, err := db.ListSkillUsage(context.Background(), a.name)
	if err != nil || len(rows) != 0 {
		t.Fatalf("cleanup ledger rows=%+v err=%v", rows, err)
	}
	if fp.calls != 0 {
		t.Fatalf("lifecycle cleanup unexpectedly called learner LLM %d times", fp.calls)
	}
}
