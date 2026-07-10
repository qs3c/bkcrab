package agent

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	skillspkg "github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type fakeCleanupStore struct {
	rows    []store.SkillUsageRow
	deleted []string
}

func (f *fakeCleanupStore) ListSkillUsage(ctx context.Context, agentID string) ([]store.SkillUsageRow, error) {
	return f.rows, nil
}

func (f *fakeCleanupStore) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	f.deleted = append(f.deleted, slug)
	return nil
}

func TestCleanupDeletesDeadLearnerSkills(t *testing.T) {
	ws := t.TempDir()
	mgr := skillspkg.NewManager(filepath.Join(ws, "skills"), skillspkg.DefaultManagerConfig())
	if err := mgr.Create("dead", "---\nname: Dead\ndescription: d\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	st := &fakeCleanupStore{rows: []store.SkillUsageRow{
		{Slug: "dead", Origin: "learner", TotalLoads: 0, CreatedSeq: 0, EditedSeq: 0},
	}}

	cleanupDeadSkills(context.Background(), st, nil, mgr, "agentA", 300, skillspkg.LifecycleConfig{DeleteAfterLoads: 200})

	if len(st.deleted) != 1 || st.deleted[0] != "dead" {
		t.Fatalf("ledger delete calls=%+v want [dead]", st.deleted)
	}
	if _, ok := mgr.Read("dead"); ok {
		t.Fatalf("skill dir should be removed")
	}
}

// TestCleanupReapsOrphanLedgerRowWhenDirGone 锁定发现 3 的修复:当技能目录已被
// 带外删除(手动 rm / 兄弟副本竞争 / 上次部分清理),账本行仍标记 deletable。
// 旧实现 mgr.Delete 失败即 continue,跳过 DeleteSkillUsage,孤儿行每轮重新符合删除
// 条件、反复告警、永不回收。修复后:目录已不存在视为 reconcile,仍删账本行。
func TestCleanupReapsOrphanLedgerRowWhenDirGone(t *testing.T) {
	ws := t.TempDir()
	mgr := skillspkg.NewManager(filepath.Join(ws, "skills"), skillspkg.DefaultManagerConfig())
	// 注意:不建 ghost 的盘上目录,只有账本行。
	st := &fakeCleanupStore{rows: []store.SkillUsageRow{
		{Slug: "ghost", Origin: "learner", TotalLoads: 0, CreatedSeq: 0, EditedSeq: 0},
	}}

	cleanupDeadSkills(context.Background(), st, nil, mgr, "agentA", 300, skillspkg.LifecycleConfig{DeleteAfterLoads: 200})

	if len(st.deleted) != 1 || st.deleted[0] != "ghost" {
		t.Fatalf("orphan ledger row should be reaped even without dir, got %+v", st.deleted)
	}
}

type failingLearnerDeleteStore struct {
	workspace.Store
}

func (f *failingLearnerDeleteStore) Delete(context.Context, string, string, string, string) error {
	return errors.New("delete unavailable")
}

func TestCleanupDeletesRemoteLearnerBeforeLocalAndLedger(t *testing.T) {
	ctx := context.Background()
	agentHome := t.TempDir()
	root := skillspkg.LearnerSkillsDir(agentHome)
	mgr := skillspkg.NewManager(root, skillspkg.DefaultManagerConfig())
	if err := mgr.Create("dead", "---\nname: Dead\ndescription: d\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	ws := workspace.NewLocalFS(t.TempDir())
	if err := skillspkg.SyncLearnerSkillUp(ctx, ws, "agentA", "dead", root); err != nil {
		t.Fatal(err)
	}
	st := &fakeCleanupStore{rows: []store.SkillUsageRow{{Slug: "dead", Origin: "learner"}}}

	cleanupDeadSkills(ctx, st, ws, mgr, "agentA", 300, skillspkg.LifecycleConfig{DeleteAfterLoads: 200})

	if _, ok := mgr.Read("dead"); ok {
		t.Fatal("local learner survived cleanup")
	}
	if len(st.deleted) != 1 || st.deleted[0] != "dead" {
		t.Fatalf("ledger deletes = %v, want [dead]", st.deleted)
	}
	objects, err := ws.List(ctx, "agentA", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		if object.Path == "learner-skills/dead/SKILL.md" {
			t.Fatal("remote learner survived cleanup")
		}
	}
}

func TestCleanupRemoteFailureRetainsLocalAndLedger(t *testing.T) {
	ctx := context.Background()
	agentHome := t.TempDir()
	root := skillspkg.LearnerSkillsDir(agentHome)
	mgr := skillspkg.NewManager(root, skillspkg.DefaultManagerConfig())
	if err := mgr.Create("dead", "---\nname: Dead\ndescription: d\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	base := workspace.NewLocalFS(t.TempDir())
	if err := skillspkg.SyncLearnerSkillUp(ctx, base, "agentA", "dead", root); err != nil {
		t.Fatal(err)
	}
	st := &fakeCleanupStore{rows: []store.SkillUsageRow{{Slug: "dead", Origin: "learner"}}}

	cleanupDeadSkills(ctx, st, &failingLearnerDeleteStore{Store: base}, mgr, "agentA", 300, skillspkg.LifecycleConfig{DeleteAfterLoads: 200})

	if _, ok := mgr.Read("dead"); !ok {
		t.Fatal("local learner was deleted after remote failure")
	}
	if len(st.deleted) != 0 {
		t.Fatalf("ledger was deleted after remote failure: %v", st.deleted)
	}
}
