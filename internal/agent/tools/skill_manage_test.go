package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

const validSkillMD = "---\nname: Test Skill\ndescription: a test skill\n---\n1. step one\n"

type skillLedgerUpsert struct {
	AgentID, Slug, ContentHash string
	FirstCreate                bool
}

type fakeSkillManageLedger struct {
	upserts []skillLedgerUpsert
	deletes []string
	rows    []store.SkillUsageRow
	listErr error
	mu      sync.Mutex
}

func (f *fakeSkillManageLedger) UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts = append(f.upserts, skillLedgerUpsert{AgentID: agentID, Slug: slug, ContentHash: contentHash, FirstCreate: firstCreate})
	for i := range f.rows {
		if f.rows[i].Slug == slug {
			f.rows[i].Origin = "learner"
			f.rows[i].ContentHash = contentHash
			return nil
		}
	}
	f.rows = append(f.rows, store.SkillUsageRow{Slug: slug, Origin: "learner", ContentHash: contentHash})
	return nil
}

func (f *fakeSkillManageLedger) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, slug)
	for i := range f.rows {
		if f.rows[i].Slug == slug {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			break
		}
	}
	return nil
}

func (f *fakeSkillManageLedger) ListSkillUsage(context.Context, string) ([]store.SkillUsageRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]store.SkillUsageRow(nil), f.rows...), nil
}

func newSkillManageExec(t *testing.T, allowDelete bool) (ToolFunc, *skills.Manager, *fakeSkillManageLedger) {
	t.Helper()
	mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
	ledger := &fakeSkillManageLedger{}
	actions := SkillManageAll
	if !allowDelete {
		actions = SkillManageCadence
	}
	fn := SkillManageExec(SkillManageDeps{
		Manager:  mgr,
		Upserter: ledger,
		Deleter:  ledger,
		AgentID:  "agent-1",
	}, actions)
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

func decodeSkillManageRead(t *testing.T, out string) skillManageReadResult {
	t.Helper()
	var result skillManageReadResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode skill_manage read %q: %v", out, err)
	}
	return result
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
	if err != nil {
		t.Fatal(err)
	}
	read := decodeSkillManageRead(t, got)
	if read.Slug != "test-skill" || read.Content != validSkillMD || read.ContentHash != store.HashSkillContent(validSkillMD) {
		t.Fatalf("read = %+v, want slug, stored content, and stable hash", read)
	}

	listOut, err := execSkillManage(t, fn, map[string]any{"action": "list"})
	if err != nil || !strings.Contains(listOut, "test-skill") || !strings.Contains(listOut, "a test skill") {
		t.Fatalf("list = (%q, %v), want slug and description", listOut, err)
	}
}

func TestSkillManageNoOpUpdateDoesNotWriteLedgerOrConsumeBudget(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	if err := mgr.Create("no-op", validSkillMD); err != nil {
		t.Fatal(err)
	}
	ledger := &fakeSkillManageLedger{}
	if err := ledger.UpsertSkillUsage(context.Background(), "agent-1", "no-op", store.HashSkillContent(validSkillMD), true); err != nil {
		t.Fatal(err)
	}
	ledger.upserts = nil
	fn := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, AgentID: "agent-1", MutationBudget: NewSkillMutationBudget(1),
	}, SkillManageCadence)

	out, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "no-op", "content": validSkillMD,
		"expected_hash": store.HashSkillContent(validSkillMD),
	})
	if err != nil {
		t.Fatalf("identical update: %v", err)
	}
	mutation, ok := ParseSkillManageMutationResult(out)
	if !ok || mutation.Applied || !mutation.NoOp {
		t.Fatalf("identical update result = %+v, parsed=%v", mutation, ok)
	}
	if len(ledger.upserts) != 0 {
		t.Fatalf("no-op update wrote lifecycle ledger: %+v", ledger.upserts)
	}

	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	out, err = execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "no-op", "content": updated,
		"expected_hash": store.HashSkillContent(validSkillMD),
	})
	if err != nil {
		t.Fatalf("real update after no-op should retain mutation budget: %v", err)
	}
	mutation, ok = ParseSkillManageMutationResult(out)
	if !ok || !mutation.Applied || mutation.NoOp {
		t.Fatalf("real update result = %+v, parsed=%v", mutation, ok)
	}
	if len(ledger.upserts) != 1 || ledger.upserts[0].ContentHash != store.HashSkillContent(updated) {
		t.Fatalf("real update ledger writes = %+v", ledger.upserts)
	}
}

func TestSkillManageReadRefreshesRemoteVersion(t *testing.T) {
	mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
	if err := mgr.Create("shared", validSkillMD); err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(validSkillMD, "step one", "newer remote step", 1)
	remote := workspace.NewLocalFS(t.TempDir())
	if err := skills.SyncLearnerSkillContent(context.Background(), remote, "agent-1", "shared", newer); err != nil {
		t.Fatal(err)
	}
	fn := SkillManageExec(SkillManageDeps{
		Manager: mgr, AgentID: "agent-1", Workspace: remote,
	}, SkillManageForeground)
	out, err := execSkillManage(t, fn, map[string]any{"action": "read", "slug": "shared"})
	if err != nil {
		t.Fatal(err)
	}
	read := decodeSkillManageRead(t, out)
	if read.Content != newer || read.ContentHash != store.HashSkillContent(newer) {
		t.Fatalf("read after remote refresh = %+v, want latest remote content/version", read)
	}
	if got, _ := mgr.Read("shared"); got != newer {
		t.Fatalf("local learner cache = %q, want refreshed remote content", got)
	}
}

func TestSkillManageUpdateAndDeleteSyncLedger(t *testing.T) {
	fn, mgr, ledger := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "s", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "s", "content": updated, "expected_hash": store.HashSkillContent(validSkillMD),
	}); err != nil {
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

func TestSkillManageUpdateRequiresLatestReadHash(t *testing.T) {
	fn, mgr, _ := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "cas-skill", "content": validSkillMD,
	}); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "cas-skill", "content": updated,
	}); err == nil || !strings.Contains(err.Error(), "expected_hash") || !strings.Contains(err.Error(), "read") {
		t.Fatalf("missing expected_hash err = %v, want read-and-retry guidance", err)
	}
	if got, _ := mgr.Read("cas-skill"); got != validSkillMD {
		t.Fatalf("missing-hash update changed content to %q", got)
	}
}

func TestSkillManageConcurrentUpdatesUseCAS(t *testing.T) {
	fn, mgr, _ := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "cas-skill", "content": validSkillMD,
	}); err != nil {
		t.Fatal(err)
	}
	expected := store.HashSkillContent(validSkillMD)
	contents := []string{
		strings.Replace(validSkillMD, "step one", "step two", 1),
		strings.Replace(validSkillMD, "step one", "step three", 1),
	}
	type result struct {
		content string
		err     error
	}
	results := make(chan result, len(contents))
	start := make(chan struct{})
	for _, content := range contents {
		content := content
		go func() {
			raw, err := json.Marshal(map[string]any{
				"action": "update", "slug": "cas-skill", "content": content, "expected_hash": expected,
			})
			if err == nil {
				<-start
				_, err = fn(context.Background(), raw)
			}
			results <- result{content: content, err: err}
		}()
	}
	close(start)
	var succeeded string
	conflicts := 0
	for range contents {
		result := <-results
		if result.err == nil {
			if succeeded != "" {
				t.Fatalf("both stale concurrent updates succeeded: %q and %q", succeeded, result.content)
			}
			succeeded = result.content
			continue
		}
		if !strings.Contains(result.err.Error(), "changed since it was read") || !strings.Contains(result.err.Error(), "merge") {
			t.Fatalf("losing update err = %v, want explicit read/merge conflict", result.err)
		}
		conflicts++
	}
	if succeeded == "" || conflicts != 1 {
		t.Fatalf("success=%q conflicts=%d, want exactly one of each", succeeded, conflicts)
	}
	if got, _ := mgr.Read("cas-skill"); got != succeeded {
		t.Fatalf("final content = %q, want winning update %q", got, succeeded)
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
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "ghost", "content": validSkillMD, "expected_hash": store.HashSkillContent(validSkillMD),
	}); err == nil {
		t.Fatal("update of missing skill succeeded")
	}
	// 未知 action
	if _, err := execSkillManage(t, fn, map[string]any{"action": "explode"}); err == nil {
		t.Fatal("unknown action succeeded")
	}
}

func TestSkillManageCreateRespectsAssetMax(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, AssetMax: 1}, SkillManageCadence)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "first", "content": validSkillMD,
	}); err != nil {
		t.Fatal(err)
	}
	different := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "second", "content": different,
	}); err == nil || !strings.Contains(err.Error(), "asset limit") || !strings.Contains(err.Error(), "read/update/merge") {
		t.Fatalf("create at asset limit err = %v, want limit refusal with read/update/merge guidance", err)
	}
	if _, ok := mgr.Read("second"); ok {
		t.Fatal("asset-limit refusal still created the second skill")
	}
}

func TestSkillManageUpdateAllowedAtAssetMax(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, AssetMax: 1}, SkillManageCadence)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "existing", "content": validSkillMD,
	}); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "existing", "content": updated, "expected_hash": store.HashSkillContent(validSkillMD),
	}); err != nil {
		t.Fatalf("update at asset limit failed: %v", err)
	}
	if got, ok := mgr.Read("existing"); !ok || got != updated {
		t.Fatalf("updated content = (%q, %v), want persisted update", got, ok)
	}
}

func TestSkillManageCreateRejectsExactContentUnderAnotherSlug(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	fn := SkillManageExec(SkillManageDeps{Manager: mgr}, SkillManageCadence)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "canonical", "content": validSkillMD,
	}); err != nil {
		t.Fatal(err)
	}
	crlfContent := strings.ReplaceAll(validSkillMD, "\n", "\r\n")
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "renamed-copy", "content": crlfContent,
	}); err == nil || !strings.Contains(err.Error(), "canonical") || !strings.Contains(err.Error(), "identical") {
		t.Fatalf("exact-content duplicate err = %v, want refusal pointing to canonical", err)
	}
	if _, ok := mgr.Read("renamed-copy"); ok {
		t.Fatal("exact-content duplicate was created under another slug")
	}
}

func TestSkillManageCreateAllowsDifferentContent(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	fn := SkillManageExec(SkillManageDeps{Manager: mgr}, SkillManageCadence)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "first", "content": validSkillMD,
	}); err != nil {
		t.Fatal(err)
	}
	different := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "create", "slug": "second", "content": different,
	}); err != nil {
		t.Fatalf("different-content create failed: %v", err)
	}
	if _, ok := mgr.Read("second"); !ok {
		t.Fatal("different-content skill was not created")
	}
}

func TestSkillManageCreateUsesAgentGlobalLedgerForQuotaAndDedupe(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		assetMax   int
		wantErr    string
		globalHash string
	}{
		{
			name: "quota counts remote-only ledger row", content: strings.Replace(validSkillMD, "step one", "different", 1),
			assetMax: 1, wantErr: "asset limit", globalHash: store.HashSkillContent(validSkillMD),
		},
		{
			name: "dedupe sees remote-only content hash", content: validSkillMD,
			assetMax: 10, wantErr: "identical", globalHash: store.HashSkillContent(validSkillMD),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
			ledger := &fakeSkillManageLedger{rows: []store.SkillUsageRow{{
				Slug: "remote-only", Origin: "learner", ContentHash: tt.globalHash,
			}}}
			fn := SkillManageExec(SkillManageDeps{
				Manager: mgr, Upserter: ledger, AgentID: "agent-1", AssetMax: tt.assetMax,
			}, SkillManageCadence)
			if _, err := execSkillManage(t, fn, map[string]any{
				"action": "create", "slug": "new-skill", "content": tt.content,
			}); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("create err = %v, want %q from global ledger", err, tt.wantErr)
			}
			if _, ok := mgr.Read("new-skill"); ok {
				t.Fatal("global-ledger refusal still created local skill")
			}
		})
	}
}

func TestSkillManageCreateFailsClosedWhenGlobalViewUnhealthy(t *testing.T) {
	tests := []struct {
		name   string
		deps   func(*skills.Manager) SkillManageDeps
		needle string
	}{
		{
			name: "ledger read failure",
			deps: func(mgr *skills.Manager) SkillManageDeps {
				return SkillManageDeps{
					Manager: mgr, Upserter: &fakeSkillManageLedger{listErr: errors.New("db unavailable")}, AgentID: "agent-1", AssetMax: 50,
				}
			},
			needle: "global learner skill ledger",
		},
		{
			name: "object store hydrate failure",
			deps: func(mgr *skills.Manager) SkillManageDeps {
				return SkillManageDeps{Manager: mgr, CreateDisabledReason: "initial learner object-store hydration failed"}
			},
			needle: "not healthy",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
			fn := SkillManageExec(tt.deps(mgr), SkillManageCadence)
			if _, err := execSkillManage(t, fn, map[string]any{
				"action": "create", "slug": "blocked", "content": validSkillMD,
			}); err == nil || !strings.Contains(err.Error(), tt.needle) {
				t.Fatalf("create err = %v, want %q", err, tt.needle)
			}
			if _, ok := mgr.Read("blocked"); ok {
				t.Fatal("fail-closed create wrote a skill")
			}
		})
	}
}

func TestSkillManageUpdateRejectsNewerAgentGlobalVersion(t *testing.T) {
	mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
	if err := mgr.Create("stale", validSkillMD); err != nil {
		t.Fatal(err)
	}
	newer := strings.Replace(validSkillMD, "step one", "newer remote step", 1)
	ledger := &fakeSkillManageLedger{rows: []store.SkillUsageRow{{
		Slug: "stale", Origin: "learner", ContentHash: store.HashSkillContent(newer),
	}}}
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, Upserter: ledger, AgentID: "agent-1"}, SkillManageForeground)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "stale", "content": newer,
		"expected_hash": store.HashSkillContent(validSkillMD),
	}); err == nil || !strings.Contains(err.Error(), "changed since it was read") || !strings.Contains(err.Error(), "read it again") {
		t.Fatalf("cross-Pod stale update err = %v, want explicit CAS conflict", err)
	}
	if got, _ := mgr.Read("stale"); got != validSkillMD {
		t.Fatalf("stale update changed local content to %q", got)
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

func TestSkillManageDeleteRevalidatesAfterMutationLock(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	if err := mgr.Create("keep", validSkillMD); err != nil {
		t.Fatal(err)
	}
	checks := 0
	fn := SkillManageExec(SkillManageDeps{
		Manager: mgr,
		BeforeDelete: func(context.Context, string) error {
			checks++
			return errors.New("no longer deletable")
		},
	}, SkillManageLifecycle)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "delete", "slug": "keep"}); err == nil || !strings.Contains(err.Error(), "no longer deletable") {
		t.Fatalf("delete revalidation err=%v", err)
	}
	if checks != 1 {
		t.Fatalf("delete revalidation checks=%d want 1", checks)
	}
	if _, ok := mgr.Read("keep"); !ok {
		t.Fatal("revalidation failure still deleted skill")
	}
}

func TestSkillManageNilManagerFails(t *testing.T) {
	fn := SkillManageExec(SkillManageDeps{}, SkillManageAll)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "list"}); err == nil {
		t.Fatal("nil manager list succeeded, want not-configured error")
	}
}

func readSkillManageObject(t *testing.T, ws workspace.Store, path string) string {
	t.Helper()
	r, err := ws.Get(context.Background(), "agent-1", "", "", path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSkillManagePersistsOnlyLearnerNamespace(t *testing.T) {
	mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
	ledger := &fakeSkillManageLedger{}
	ws := workspace.NewLocalFS(t.TempDir())
	fn := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: ws,
	}, SkillManageAll)

	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "remote", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	if got := readSkillManageObject(t, ws, "learner-skills/remote/SKILL.md"); got != validSkillMD {
		t.Fatalf("remote create = %q", got)
	}
	if _, err := ws.Stat(context.Background(), "agent-1", "", "", "skills/remote/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("skill_manage wrote ordinary skills namespace: %v", err)
	}

	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{
		"action": "update", "slug": "remote", "content": updated, "expected_hash": store.HashSkillContent(validSkillMD),
	}); err != nil {
		t.Fatal(err)
	}
	if got := readSkillManageObject(t, ws, "learner-skills/remote/SKILL.md"); got != updated {
		t.Fatalf("remote update = %q", got)
	}

	if _, err := execSkillManage(t, fn, map[string]any{"action": "delete", "slug": "remote"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Stat(context.Background(), "agent-1", "", "", "learner-skills/remote/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("remote learner survived delete: %v", err)
	}
}

type failingSkillPutStore struct{ workspace.Store }

func (f *failingSkillPutStore) Put(context.Context, string, string, string, string, io.Reader, int64, string) error {
	return errors.New("put unavailable")
}

type failingSkillDeleteStore struct{ workspace.Store }

func (f *failingSkillDeleteStore) Delete(context.Context, string, string, string, string) error {
	return errors.New("delete unavailable")
}

func TestSkillManageRemoteFailureDoesNotReportDivergentSuccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	ledger := &fakeSkillManageLedger{}
	base := workspace.NewLocalFS(t.TempDir())
	working := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: base,
	}, SkillManageAll)

	failingPut := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: &failingSkillPutStore{Store: base},
	}, SkillManageAll)
	if _, err := execSkillManage(t, failingPut, map[string]any{"action": "create", "slug": "new", "content": validSkillMD}); err == nil {
		t.Fatal("create reported success after remote failure")
	}
	if _, ok := mgr.Read("new"); ok {
		t.Fatal("failed remote create left a local-only learner skill")
	}

	if _, err := execSkillManage(t, working, map[string]any{"action": "create", "slug": "existing", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, failingPut, map[string]any{
		"action": "update", "slug": "existing", "content": updated, "expected_hash": store.HashSkillContent(validSkillMD),
	}); err == nil {
		t.Fatal("update reported success after remote failure")
	}
	if got, _ := mgr.Read("existing"); got != validSkillMD {
		t.Fatalf("failed update did not restore local content: %q", got)
	}

	failingDelete := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: &failingSkillDeleteStore{Store: base},
	}, SkillManageAll)
	if _, err := execSkillManage(t, failingDelete, map[string]any{"action": "delete", "slug": "existing"}); err == nil {
		t.Fatal("delete reported success after remote failure")
	}
	if _, ok := mgr.Read("existing"); !ok {
		t.Fatal("remote delete failure removed local learner")
	}
}

type clobberingSkillPutStore struct {
	workspace.Store
	localPath   string
	replacement string
}

func (s *clobberingSkillPutStore) Put(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string) error {
	if path == "learner-skills/race/SKILL.md" {
		if err := os.WriteFile(s.localPath, []byte(s.replacement), 0o644); err != nil {
			return err
		}
	}
	return s.Store.Put(ctx, agentID, projectID, sessionID, path, r, size, contentType)
}

func TestSkillManageSyncsValidatedContentNotMutableDisk(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	ledger := &fakeSkillManageLedger{}
	base := workspace.NewLocalFS(t.TempDir())
	initial := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: base,
	}, SkillManageAll)
	if _, err := execSkillManage(t, initial, map[string]any{"action": "create", "slug": "race", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}

	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	clobberStore := &clobberingSkillPutStore{
		Store: base, localPath: filepath.Join(root, "race", "SKILL.md"), replacement: validSkillMD,
	}
	update := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: clobberStore,
	}, SkillManageAll)
	if _, err := execSkillManage(t, update, map[string]any{
		"action": "update", "slug": "race", "content": updated, "expected_hash": store.HashSkillContent(validSkillMD),
	}); err != nil {
		t.Fatal(err)
	}
	if got := readSkillManageObject(t, base, "learner-skills/race/SKILL.md"); got != updated {
		t.Fatalf("remote content = %q, want validated update bytes", got)
	}
}

type failingSkillLedger struct{ fakeSkillManageLedger }

func (f *failingSkillLedger) UpsertSkillUsage(context.Context, string, string, string, bool) error {
	return errors.New("ledger unavailable")
}

func TestSkillManageLedgerFailureRollsBackCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	ledger := &failingSkillLedger{}
	remote := workspace.NewLocalFS(t.TempDir())
	fn := SkillManageExec(SkillManageDeps{
		Manager: mgr, Upserter: ledger, Deleter: ledger,
		AgentID: "agent-1", Workspace: remote,
	}, SkillManageAll)

	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "ledger", "content": validSkillMD}); err == nil {
		t.Fatal("create reported success after ledger failure")
	}
	if _, ok := mgr.Read("ledger"); ok {
		t.Fatal("ledger failure left local learner skill")
	}
	if _, err := remote.Stat(context.Background(), "agent-1", "", "", "learner-skills/ledger/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("ledger failure left remote learner skill: %v", err)
	}
}

func TestSkillManageRejectsOrdinarySkillManager(t *testing.T) {
	mgr := skills.NewManager(filepath.Join(t.TempDir(), "skills"), skills.DefaultManagerConfig())
	fn := SkillManageExec(SkillManageDeps{Manager: mgr}, SkillManageAll)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "list"}); err == nil || !strings.Contains(err.Error(), "learner") {
		t.Fatalf("ordinary manager list err = %v, want learner-root refusal", err)
	}
}

type busySkillMutationLedger struct{ fakeSkillManageLedger }

func (b *busySkillMutationLedger) AcquireChannelLease(context.Context, string, string, string, time.Duration) (bool, error) {
	return false, nil
}

func (b *busySkillMutationLedger) RenewChannelLease(context.Context, string, string, string, time.Duration) (bool, error) {
	return false, nil
}

func (b *busySkillMutationLedger) ReleaseChannelLease(context.Context, string, string, string) error {
	return nil
}

func TestSkillManageFailsClosedWhenCrossPodLeaseIsBusy(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	ledger := &busySkillMutationLedger{}
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, Upserter: ledger, Deleter: ledger, AgentID: "agent-1"}, SkillManageCadence)

	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "busy", "content": validSkillMD}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy lease create err = %v, want busy refusal", err)
	}
	if _, ok := mgr.Read("busy"); ok {
		t.Fatal("busy cross-Pod lease still mutated local skill")
	}
}

type deletedSkillAgentLedger struct{ fakeSkillManageLedger }

func (d *deletedSkillAgentLedger) GetAgent(context.Context, string) (*store.AgentRecord, error) {
	return nil, store.ErrNotFound
}

func TestSkillManageRefusesMutationAfterAgentDeletion(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	ledger := &deletedSkillAgentLedger{}
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, Upserter: ledger, Deleter: ledger, AgentID: "deleted-agent"}, SkillManageCadence)

	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "late", "content": validSkillMD}); err == nil || !strings.Contains(err.Error(), "no longer exists") {
		t.Fatalf("post-delete mutation err = %v, want deletion refusal", err)
	}
	if _, ok := mgr.Read("late"); ok {
		t.Fatal("post-delete mutation recreated learner skill")
	}
}

type deletingSkillAgentLedger struct{ fakeSkillManageLedger }

func (d *deletingSkillAgentLedger) IsAgentDeleting(context.Context, string) (bool, error) {
	return true, nil
}

func (d *deletingSkillAgentLedger) GetAgent(context.Context, string) (*store.AgentRecord, error) {
	return &store.AgentRecord{ID: "deleting-agent"}, nil
}

func TestSkillManageRefusesMutationWhileAgentDeletionIsInProgress(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	ledger := &deletingSkillAgentLedger{}
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, Upserter: ledger, Deleter: ledger, AgentID: "deleting-agent"}, SkillManageCadence)

	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "late", "content": validSkillMD}); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("in-progress deletion mutation err = %v, want deletion refusal", err)
	}
}

func newSkillManageTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetOwnerUserID("owner-1")
	r.SetAgentOwnerUserID("owner-1")
	mgr := skills.NewManager(filepath.Join(t.TempDir(), skills.LearnerSkillsDirName), skills.DefaultManagerConfig())
	for _, slug := range []string{"s1", "turn-skill"} {
		if err := mgr.Create(slug, validSkillMD); err != nil {
			t.Fatal(err)
		}
	}
	r.SetSkillManage(mgr, nil)
	return r
}

func execRegistrySkillManage(t *testing.T, r *Registry, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := r.tools["skill_manage"]
	if !ok {
		t.Fatal("skill_manage not registered")
	}
	return tool.fn(context.Background(), raw)
}

func registryHasToolDef(r *Registry, name string) bool {
	for _, def := range r.DefinitionsForMode(nil) {
		if def.Function.Name == name {
			return true
		}
	}
	return false
}

func TestSkillManageOwnerGateOnTurnRegistry(t *testing.T) {
	base := newSkillManageTestRegistry(t)

	guest := base.ForTurn()
	guest.SetChatterUserID("guest-9")
	if registryHasToolDef(guest, "skill_manage") {
		t.Fatal("guest tool definitions expose skill_manage")
	}
	for _, action := range []string{"create", "update", "read", "delete", "list"} {
		args := map[string]any{"action": action, "slug": "s1", "content": validSkillMD}
		if _, err := execRegistrySkillManage(t, guest, args); err == nil || !strings.Contains(err.Error(), "owner") {
			t.Fatalf("guest %s err = %v, want owner-restriction refusal", action, err)
		}
	}

	owner := base.ForTurn()
	owner.SetChatterUserID("owner-1")
	owner.SetSkillManageActions(SkillManageForeground)
	if !registryHasToolDef(owner, "skill_manage") {
		t.Fatal("owner tool definitions do not expose skill_manage")
	}
	if _, err := execRegistrySkillManage(t, owner, map[string]any{
		"action": "create", "slug": "new-skill", "content": validSkillMD,
	}); err == nil {
		t.Fatal("foreground owner created a new learner skill")
	}
	if _, err := execRegistrySkillManage(t, owner, map[string]any{
		"action": "update", "slug": "s1", "content": strings.Replace(validSkillMD, "step one", "step two", 1),
		"expected_hash": store.HashSkillContent(validSkillMD),
	}); err != nil {
		t.Fatalf("owner update err = %v, want allowed", err)
	}

	// Missing per-turn authorization is fail-closed, including legacy callers.
	blank := base.ForTurn()
	if _, err := execRegistrySkillManage(t, blank, map[string]any{
		"action": "update", "slug": "s1", "content": strings.Replace(validSkillMD, "step one", "step two", 1),
		"expected_hash": store.HashSkillContent(validSkillMD),
	}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("blank-chatter update err = %v, want fail-closed refusal", err)
	}
}

func TestSkillManageDepsSurviveForTurn(t *testing.T) {
	base := newSkillManageTestRegistry(t)
	turn := base.ForTurn()
	turn.SetChatterUserID("owner-1")
	turn.SetSkillManageActions(SkillManageForeground)
	if _, err := execRegistrySkillManage(t, turn, map[string]any{
		"action": "update", "slug": "turn-skill", "content": strings.Replace(validSkillMD, "step one", "step two", 1),
		"expected_hash": store.HashSkillContent(validSkillMD),
	}); err != nil {
		t.Fatalf("update on ForTurn copy err = %v — skillManager/skillLedger 未复制进回合副本", err)
	}
}

func TestSkillManageAuthorizationDoesNotLeakAcrossForTurnCopies(t *testing.T) {
	base := newSkillManageTestRegistry(t)
	base.SetSkillManageActions(SkillManageForeground)

	turn := base.ForTurn()
	turn.SetChatterUserID("guest-9")
	if registryHasToolDef(turn, "skill_manage") {
		t.Fatal("ForTurn inherited skill_manage authorization from parent")
	}
	if _, err := execRegistrySkillManage(t, turn, map[string]any{"action": "list"}); err == nil {
		t.Fatal("ForTurn inherited executable skill_manage authorization from parent")
	}
}

func skillManageActionEnum(t *testing.T, actions SkillManageActions) []string {
	t.Helper()
	def := SkillManageToolDef(actions)
	params, ok := def.Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("skill_manage parameters type = %T", def.Function.Parameters)
	}
	props := params["properties"].(map[string]any)
	action := props["action"].(map[string]any)
	return action["enum"].([]string)
}

func TestSkillManageCapabilitySchemas(t *testing.T) {
	for _, tt := range []struct {
		name    string
		actions SkillManageActions
		want    []string
	}{
		{name: "foreground", actions: SkillManageForeground, want: []string{"list", "read", "update"}},
		{name: "cadence", actions: SkillManageCadence, want: []string{"list", "read", "create", "update"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := skillManageActionEnum(t, tt.actions); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("actions = %v, want %v", got, tt.want)
			}
			params := SkillManageToolDef(tt.actions).Function.Parameters.(map[string]any)
			props := params["properties"].(map[string]any)
			if _, ok := props["expected_hash"]; !ok {
				t.Fatal("update-capable schema omitted expected_hash")
			}
			allOf := params["allOf"].([]any)
			then := allOf[0].(map[string]any)["then"].(map[string]any)
			if got, want := then["required"].([]string), []string{"slug", "content", "expected_hash"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("update required fields = %v, want %v", got, want)
			}
		})
	}
}

func TestSkillManageDefinitionsAlsoHideUnauthorizedTool(t *testing.T) {
	base := newSkillManageTestRegistry(t)
	guest := base.ForTurn()
	for _, def := range guest.Definitions() {
		if def.Function.Name == "skill_manage" {
			t.Fatal("Definitions exposed skill_manage without per-turn capability")
		}
	}
	owner := base.ForTurn()
	owner.SetSkillManageActions(SkillManageForeground)
	found := false
	for _, def := range owner.Definitions() {
		if def.Function.Name != "skill_manage" {
			continue
		}
		found = true
		params := def.Function.Parameters.(map[string]any)
		got := params["properties"].(map[string]any)["action"].(map[string]any)["enum"].([]string)
		if want := []string{"list", "read", "update"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("Definitions foreground actions = %v, want %v", got, want)
		}
	}
	if !found {
		t.Fatal("Definitions hid authorized foreground skill_manage")
	}
}

func TestSkillManageCadenceAllowsOnlyOneSuccessfulMutation(t *testing.T) {
	root := filepath.Join(t.TempDir(), skills.LearnerSkillsDirName)
	mgr := skills.NewManager(root, skills.DefaultManagerConfig())
	budget := NewSkillMutationBudget(1)
	fn := SkillManageExec(SkillManageDeps{Manager: mgr, MutationBudget: budget}, SkillManageCadence)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "first", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "second", "content": validSkillMD}); err == nil || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("second cadence mutation err = %v, want budget refusal", err)
	}
	if _, err := execSkillManage(t, fn, map[string]any{"action": "read", "slug": "first"}); err != nil {
		t.Fatalf("read after mutation budget was rejected: %v", err)
	}
	if _, ok := mgr.Read("second"); ok {
		t.Fatal("second cadence mutation changed disk")
	}
}
