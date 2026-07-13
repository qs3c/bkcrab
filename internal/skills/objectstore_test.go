package skills

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func writeObjectStoreTestSkill(t *testing.T, rootDir, slug, content string) {
	t.Helper()
	dir := filepath.Join(rootDir, slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readObjectStoreTestSkill(t *testing.T, rootDir, slug string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(rootDir, slug, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLearnerObjectStoreNamespaceDoesNotTouchInstalledSkills(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	const owner = "agent-a"

	installedRoot := filepath.Join(t.TempDir(), "skills")
	learnerRoot := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, installedRoot, "same-slug", "installed")
	writeObjectStoreTestSkill(t, learnerRoot, "same-slug", "learner")

	if err := SyncSkillUp(ctx, ws, owner, "same-slug", installedRoot); err != nil {
		t.Fatal(err)
	}
	if err := SyncLearnerSkillUp(ctx, ws, owner, "same-slug", learnerRoot); err != nil {
		t.Fatal(err)
	}

	objects, err := ws.List(ctx, owner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]bool, len(objects))
	for _, object := range objects {
		paths[object.Path] = true
	}
	if !paths["skills/same-slug/SKILL.md"] || !paths["learner-skills/same-slug/SKILL.md"] {
		t.Fatalf("object paths = %v, want isolated installed and learner keys", paths)
	}

	if err := DeleteLearnerSkillUp(ctx, ws, owner, "same-slug"); err != nil {
		t.Fatal(err)
	}
	objects, err = ws.List(ctx, owner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	paths = make(map[string]bool, len(objects))
	for _, object := range objects {
		paths[object.Path] = true
	}
	if !paths["skills/same-slug/SKILL.md"] {
		t.Fatalf("deleting learner namespace removed installed skill: %v", paths)
	}
	if paths["learner-skills/same-slug/SKILL.md"] {
		t.Fatalf("learner object survived delete: %v", paths)
	}
}

func TestDeleteLearnerNamespaceLeavesInstalledSkills(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	installedRoot := filepath.Join(t.TempDir(), "skills")
	learnerRoot := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, installedRoot, "same", "installed")
	writeObjectStoreTestSkill(t, learnerRoot, "same", "learner")
	if err := SyncSkillUp(ctx, ws, "agent-a", "same", installedRoot); err != nil {
		t.Fatal(err)
	}
	if err := SyncLearnerSkillUp(ctx, ws, "agent-a", "same", learnerRoot); err != nil {
		t.Fatal(err)
	}

	if err := DeleteLearnerNamespace(ctx, ws, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Stat(ctx, "agent-a", "", "", "skills/same/SKILL.md"); err != nil {
		t.Fatalf("installed skill removed with learner namespace: %v", err)
	}
	if _, err := ws.Stat(ctx, "agent-a", "", "", "learner-skills/same/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("learner skill object survived namespace deletion: %v", err)
	}
	if _, err := ws.Stat(ctx, "agent-a", "", "", "learner-skills/.initialized"); err != nil {
		t.Fatalf("authoritative empty namespace marker missing after deletion: %v", err)
	}
}

type markerFailStore struct {
	workspace.Store
	markerErr error
}

func (s markerFailStore) Put(ctx context.Context, agentID, projectID, sessionID, objectPath string, r io.Reader, size int64, contentType string) error {
	if objectPath == learnerNamespaceMarker {
		return s.markerErr
	}
	return s.Store.Put(ctx, agentID, projectID, sessionID, objectPath, r, size, contentType)
}

func TestLearnerCreateDoesNotWriteAssetWhenNamespaceMarkerFails(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewLocalFS(t.TempDir())
	ws := markerFailStore{Store: base, markerErr: errors.New("marker unavailable")}
	root := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, root, "new-skill", "content")

	err := SyncLearnerSkillContent(ctx, ws, "agent-a", "new-skill", "content")
	if err == nil || !strings.Contains(err.Error(), "marker unavailable") {
		t.Fatalf("SyncLearnerSkillContent err=%v", err)
	}
	if _, statErr := base.Stat(ctx, "agent-a", "", "", "learner-skills/new-skill/SKILL.md"); !errors.Is(statErr, workspace.ErrNotFound) {
		t.Fatalf("failed create left remote learner asset: %v", statErr)
	}
}

func TestDeleteLearnerSkillStillDeletesWhenMarkerRefreshFails(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewLocalFS(t.TempDir())
	root := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, root, "old-skill", "content")
	if err := SyncLearnerSkillContent(ctx, base, "agent-a", "old-skill", "content"); err != nil {
		t.Fatal(err)
	}
	ws := markerFailStore{Store: base, markerErr: errors.New("marker unavailable")}

	err := DeleteLearnerSkillUp(ctx, ws, "agent-a", "old-skill")
	if err == nil || !strings.Contains(err.Error(), "marker unavailable") {
		t.Fatalf("DeleteLearnerSkillUp err=%v", err)
	}
	if _, statErr := base.Stat(ctx, "agent-a", "", "", "learner-skills/old-skill/SKILL.md"); !errors.Is(statErr, workspace.ErrNotFound) {
		t.Fatalf("marker failure suppressed learner asset deletion: %v", statErr)
	}
}

func TestHydrateLearnerSkillsDownRefreshesSameSizeUpdate(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	const owner = "agent-a"
	const v1 = "---\nname: Same\ndescription: same-size update\n---\nversion one\n"
	const v2 = "---\nname: Same\ndescription: same-size update\n---\nversion two\n"
	if len(v1) != len(v2) {
		t.Fatalf("test fixture lengths differ: %d != %d", len(v1), len(v2))
	}

	sourceRoot := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	destRoot := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, sourceRoot, "same", v1)
	if err := SyncLearnerSkillUp(ctx, ws, owner, "same", sourceRoot); err != nil {
		t.Fatal(err)
	}
	if err := HydrateLearnerSkillsDown(ctx, ws, owner, destRoot); err != nil {
		t.Fatal(err)
	}
	if got := readObjectStoreTestSkill(t, destRoot, "same"); got != v1 {
		t.Fatalf("initial hydrate = %q, want v1", got)
	}

	writeObjectStoreTestSkill(t, sourceRoot, "same", v2)
	if err := SyncLearnerSkillUp(ctx, ws, owner, "same", sourceRoot); err != nil {
		t.Fatal(err)
	}
	if err := HydrateLearnerSkillsDown(ctx, ws, owner, destRoot); err != nil {
		t.Fatal(err)
	}
	if got := readObjectStoreTestSkill(t, destRoot, "same"); got != v2 {
		t.Fatalf("same-size update stayed stale: got %q, want v2", got)
	}
}

func TestHydrateLearnerSkillsDownPrunesAfterLastRemoteDelete(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	const owner = "agent-a"
	sourceRoot := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	destRoot := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, sourceRoot, "last", "last learner")

	if err := SyncLearnerSkillUp(ctx, ws, owner, "last", sourceRoot); err != nil {
		t.Fatal(err)
	}
	if err := HydrateLearnerSkillsDown(ctx, ws, owner, destRoot); err != nil {
		t.Fatal(err)
	}
	if err := DeleteLearnerSkillUp(ctx, ws, owner, "last"); err != nil {
		t.Fatal(err)
	}
	if err := HydrateLearnerSkillsDown(ctx, ws, owner, destRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(destRoot, "last")); !os.IsNotExist(err) {
		t.Fatalf("last local learner skill was not pruned after remote became empty: %v", err)
	}
}

func TestHydrateInstalledSkillsKeepsLocalWhenRemoteEmpty(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	root := filepath.Join(t.TempDir(), "skills")
	writeObjectStoreTestSkill(t, root, "manual", "manual skill")

	if err := HydrateSkillsDown(ctx, ws, "agent-a", root); err != nil {
		t.Fatal(err)
	}
	if got := readObjectStoreTestSkill(t, root, "manual"); got != "manual skill" {
		t.Fatalf("ordinary empty-remote compatibility changed: got %q", got)
	}
}

func TestHydrateLearnerSkillsKeepsLegacyLocalBeforeNamespaceInitialized(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	root := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, root, "legacy", "legacy learner skill")

	if err := HydrateLearnerSkillsDown(ctx, ws, "agent-a", root); err != nil {
		t.Fatal(err)
	}
	if got := readObjectStoreTestSkill(t, root, "legacy"); got != "legacy learner skill" {
		t.Fatalf("uninitialized learner namespace deleted migration source: got %q", got)
	}
}

func TestMirrorLearnerSkillsUpInitializesOnlyEmptyNamespace(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	root := filepath.Join(t.TempDir(), LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, root, "local", "local learner skill")

	if err := MirrorLearnerSkillsUp(ctx, ws, "agent-a", root); err != nil {
		t.Fatal(err)
	}
	if r, err := ws.Get(ctx, "agent-a", "", "", "learner-skills/local/SKILL.md"); err != nil {
		t.Fatalf("local learner was not mirrored: %v", err)
	} else {
		r.Close()
	}

	if err := DeleteLearnerSkillUp(ctx, ws, "agent-a", "local"); err != nil {
		t.Fatal(err)
	}
	writeObjectStoreTestSkill(t, root, "stale", "must not resurrect")
	if err := MirrorLearnerSkillsUp(ctx, ws, "agent-a", root); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Get(ctx, "agent-a", "", "", "learner-skills/stale/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("initialized empty namespace resurrected stale local skill: %v", err)
	}
}

func TestSyncLearnerSkillUpRejectsEscapingSlug(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	base := t.TempDir()
	root := filepath.Join(base, LearnerSkillsDirName)
	writeObjectStoreTestSkill(t, base, "escape", "must not upload outside learner namespace")

	if err := SyncLearnerSkillUp(ctx, ws, "agent-a", "../escape", root); err == nil {
		t.Fatal("escaping learner slug was accepted")
	}
	if _, err := ws.Stat(ctx, "agent-a", "", "", "escape/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("escaping slug wrote outside learner namespace: %v", err)
	}
}

type maliciousLearnerObjectStore struct {
	workspace.Store
	path    string
	content string
}

func (m *maliciousLearnerObjectStore) List(context.Context, string, string, string) ([]workspace.ObjectInfo, error) {
	return []workspace.ObjectInfo{{Path: m.path, Size: int64(len(m.content))}}, nil
}

func (m *maliciousLearnerObjectStore) Get(context.Context, string, string, string, string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader(m.content)), nil
}

func TestHydrateLearnerSkillsDownRejectsEscapingRemotePath(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "nested", LearnerSkillsDirName)
	outside := filepath.Join(base, "escape", "SKILL.md")
	store := &maliciousLearnerObjectStore{
		Store:   workspace.NewLocalFS(t.TempDir()),
		path:    "learner-skills/../../escape/SKILL.md",
		content: "malicious",
	}

	if err := HydrateLearnerSkillsDown(context.Background(), store, "agent-a", root); err == nil {
		t.Fatal("escaping remote object path was accepted")
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaping remote object wrote outside learner root: %v", err)
	}
}

func TestHydrateLearnerSkillsDownRejectsSymlinkedSkillDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, LearnerSkillsDirName)
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	store := &maliciousLearnerObjectStore{
		Store:   workspace.NewLocalFS(t.TempDir()),
		path:    "learner-skills/linked/SKILL.md",
		content: "must not escape through a symlink",
	}

	if err := HydrateLearnerSkillsDown(context.Background(), store, "agent-a", root); err == nil {
		t.Fatal("symlinked learner directory was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "SKILL.md")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hydrate followed symlink outside learner root: %v", err)
	}
}

func TestDeleteLearnerNamespaceLeavesAuthoritativeEmptyTombstone(t *testing.T) {
	ctx := context.Background()
	remote := workspace.NewLocalFS(t.TempDir())
	owner := "deleted-agent"
	sourceRoot := LearnerSkillsDir(t.TempDir())
	mgr := NewManager(sourceRoot, DefaultManagerConfig())
	const content = "---\nname: Stale\ndescription: stale sibling cache\n---\nbody\n"
	if err := mgr.Create("stale", content); err != nil {
		t.Fatal(err)
	}
	if err := SyncLearnerSkillUp(ctx, remote, owner, "stale", sourceRoot); err != nil {
		t.Fatal(err)
	}
	stalePodRoot := LearnerSkillsDir(t.TempDir())
	stalePod := NewManager(stalePodRoot, DefaultManagerConfig())
	if err := stalePod.Create("stale", content); err != nil {
		t.Fatal(err)
	}

	if err := DeleteLearnerNamespace(ctx, remote, owner); err != nil {
		t.Fatal(err)
	}
	initialized, err := LearnerNamespaceInitialized(ctx, remote, owner)
	if err != nil || !initialized {
		t.Fatalf("deleted namespace tombstone initialized=%v err=%v", initialized, err)
	}
	if _, err := remote.Stat(ctx, owner, "", "", learnerNamespaceMarker); err != nil {
		t.Fatalf("namespace deletion marker missing: %v", err)
	}
	if _, err := remote.Stat(ctx, owner, "", "", "learner-skills/stale/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("deleted learner object survived: %v", err)
	}
	if err := HydrateLearnerSkillsDown(ctx, remote, owner, stalePodRoot); err != nil {
		t.Fatal(err)
	}
	if _, ok := stalePod.Read("stale"); ok {
		t.Fatal("stale sibling Pod kept learner asset after remote namespace tombstone")
	}
}
