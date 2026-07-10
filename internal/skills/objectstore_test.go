package skills

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
