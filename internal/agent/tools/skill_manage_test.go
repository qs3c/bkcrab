package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/skills"
)

const validSkillMD = "---\nname: Test Skill\ndescription: a test skill\n---\n1. step one\n"

type skillLedgerUpsert struct {
	AgentID, Slug string
	FirstCreate   bool
}

type fakeSkillManageLedger struct {
	upserts []skillLedgerUpsert
	deletes []string
}

func (f *fakeSkillManageLedger) UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error {
	f.upserts = append(f.upserts, skillLedgerUpsert{agentID, slug, firstCreate})
	return nil
}

func (f *fakeSkillManageLedger) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	f.deletes = append(f.deletes, slug)
	return nil
}

func newSkillManageExec(t *testing.T, allowDelete bool) (ToolFunc, *skills.Manager, *fakeSkillManageLedger) {
	t.Helper()
	mgr := skills.NewManager(filepath.Join(t.TempDir(), "skills"), skills.DefaultManagerConfig())
	ledger := &fakeSkillManageLedger{}
	fn := SkillManageExec(SkillManageDeps{
		Manager:     mgr,
		Upserter:    ledger,
		Deleter:     ledger,
		AgentID:     "agent-1",
		AllowDelete: allowDelete,
	})
	return fn, mgr, ledger
}

func execSkillManage(t *testing.T, fn ToolFunc, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return fn(context.Background(), raw)
}

func TestSkillManageCreateReadList(t *testing.T) {
	fn, mgr, ledger := newSkillManageExec(t, true)

	out, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "test-skill", "content": validSkillMD})
	if err != nil || !strings.Contains(out, "test-skill") {
		t.Fatalf("create = (%q, %v), want success mentioning slug", out, err)
	}
	if _, ok := mgr.Read("test-skill"); !ok {
		t.Fatal("skill not on disk after create")
	}
	if len(ledger.upserts) != 1 || !ledger.upserts[0].FirstCreate || ledger.upserts[0].AgentID != "agent-1" {
		t.Fatalf("ledger upserts = %+v, want one firstCreate for agent-1", ledger.upserts)
	}

	got, err := execSkillManage(t, fn, map[string]any{"action": "read", "slug": "test-skill"})
	if err != nil || got != validSkillMD {
		t.Fatalf("read = (%q, %v), want stored content", got, err)
	}

	listOut, err := execSkillManage(t, fn, map[string]any{"action": "list"})
	if err != nil || !strings.Contains(listOut, "test-skill") || !strings.Contains(listOut, "a test skill") {
		t.Fatalf("list = (%q, %v), want slug and description", listOut, err)
	}
}

func TestSkillManageUpdateAndDeleteSyncLedger(t *testing.T) {
	fn, mgr, ledger := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "s", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "update", "slug": "s", "content": updated}); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.Read("s"); !strings.Contains(got, "step two") {
		t.Fatalf("content after update = %q", got)
	}
	if len(ledger.upserts) != 2 || ledger.upserts[1].FirstCreate {
		t.Fatalf("ledger upserts = %+v, want second with firstCreate=false", ledger.upserts)
	}

	if _, err := execSkillManage(t, fn, map[string]any{"action": "delete", "slug": "s"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Read("s"); ok {
		t.Fatal("skill still on disk after delete")
	}
	if len(ledger.deletes) != 1 || ledger.deletes[0] != "s" {
		t.Fatalf("ledger deletes = %v, want [s]", ledger.deletes)
	}
}

func TestSkillManageErrorsSurfaceToCaller(t *testing.T) {
	fn, _, _ := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "dup", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	// 重复 create → manager 拒绝,错误必须返回给模型而非静默丢弃
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "dup", "content": validSkillMD}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create err = %v, want already-exists", err)
	}
	// 缺 frontmatter → 校验错误上抛
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "bad", "content": "no frontmatter"}); err == nil {
		t.Fatal("invalid content create succeeded, want validation error")
	}
	// 不存在的 update
	if _, err := execSkillManage(t, fn, map[string]any{"action": "update", "slug": "ghost", "content": validSkillMD}); err == nil {
		t.Fatal("update of missing skill succeeded")
	}
	// 未知 action
	if _, err := execSkillManage(t, fn, map[string]any{"action": "explode"}); err == nil {
		t.Fatal("unknown action succeeded")
	}
}

func TestSkillManageDeleteDisallowed(t *testing.T) {
	fn, mgr, _ := newSkillManageExec(t, false)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "keep", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	if _, err := execSkillManage(t, fn, map[string]any{"action": "delete", "slug": "keep"}); err == nil {
		t.Fatal("delete succeeded with AllowDelete=false")
	}
	if _, ok := mgr.Read("keep"); !ok {
		t.Fatal("skill removed despite delete being disallowed")
	}
}

func TestSkillManageNilManagerFails(t *testing.T) {
	fn := SkillManageExec(SkillManageDeps{})
	if _, err := execSkillManage(t, fn, map[string]any{"action": "list"}); err == nil {
		t.Fatal("nil manager list succeeded, want not-configured error")
	}
}

func newSkillManageTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetOwnerUserID("owner-1")
	r.SetAgentOwnerUserID("owner-1")
	mgr := skills.NewManager(filepath.Join(t.TempDir(), "skills"), skills.DefaultManagerConfig())
	r.SetSkillManage(mgr, nil)
	return r
}

func execRegistrySkillManage(t *testing.T, r *Registry, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	fn := r.GetFunc("skill_manage")
	if fn == nil {
		t.Fatal("skill_manage not registered")
	}
	return fn(context.Background(), raw)
}

func TestSkillManageOwnerGateOnTurnRegistry(t *testing.T) {
	base := newSkillManageTestRegistry(t)

	guest := base.ForTurn()
	guest.SetChatterUserID("guest-9")
	if _, err := execRegistrySkillManage(t, guest, map[string]any{
		"action": "create", "slug": "s1", "content": validSkillMD,
	}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("guest create err = %v, want owner-restriction refusal", err)
	}
	// read/list 不受门控
	if _, err := execRegistrySkillManage(t, guest, map[string]any{"action": "list"}); err != nil {
		t.Fatalf("guest list err = %v, want allowed", err)
	}

	owner := base.ForTurn()
	owner.SetChatterUserID("owner-1")
	if _, err := execRegistrySkillManage(t, owner, map[string]any{
		"action": "create", "slug": "s1", "content": validSkillMD,
	}); err != nil {
		t.Fatalf("owner create err = %v, want allowed", err)
	}

	// chatter 未设置(web 回合/旧版单用户)视为所有者语境
	blank := base.ForTurn()
	if _, err := execRegistrySkillManage(t, blank, map[string]any{
		"action": "update", "slug": "s1", "content": strings.Replace(validSkillMD, "step one", "step two", 1),
	}); err != nil {
		t.Fatalf("blank-chatter update err = %v, want allowed", err)
	}
}

func TestSkillManageDepsSurviveForTurn(t *testing.T) {
	base := newSkillManageTestRegistry(t)
	turn := base.ForTurn()
	turn.SetChatterUserID("owner-1")
	if _, err := execRegistrySkillManage(t, turn, map[string]any{
		"action": "create", "slug": "turn-skill", "content": validSkillMD,
	}); err != nil {
		t.Fatalf("create on ForTurn copy err = %v — skillManager/skillLedger 未复制进回合副本", err)
	}
}
