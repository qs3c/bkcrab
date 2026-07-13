package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type fakeLearnerMigrationUsage struct {
	rows []store.SkillUsageRow
}

func (f *fakeLearnerMigrationUsage) ListSkillUsage(context.Context, string) ([]store.SkillUsageRow, error) {
	return f.rows, nil
}

func writeLegacyMigrationSkill(t *testing.T, agentDir, slug, content string, extra bool) {
	t.Helper()
	dir := filepath.Join(agentDir, "skills", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if extra {
		if err := os.WriteFile(filepath.Join(dir, "script.sh"), []byte("echo manual"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMigrateLegacyLearnerSkillsMovesVerifiedAssetAndSyncsRemote(t *testing.T) {
	agentDir := t.TempDir()
	const content = "---\nname: Legacy\ndescription: migrated learner\n---\n\nDo the steps.\n"
	writeLegacyMigrationSkill(t, agentDir, "legacy", content, false)
	usage := &fakeLearnerMigrationUsage{rows: []store.SkillUsageRow{{
		Slug: "legacy", Origin: "learner", ContentHash: store.HashSkillContent(content),
	}}}
	remote := workspace.NewLocalFS(t.TempDir())
	target := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())

	migrateLegacyLearnerSkills(context.Background(), usage, remote, "agent-a", agentDir, target)

	if got, ok := target.Read("legacy"); !ok || got != content {
		t.Fatalf("migrated target = (%q,%v)", got, ok)
	}
	if _, err := os.Stat(filepath.Join(agentDir, "skills", "legacy")); !os.IsNotExist(err) {
		t.Fatalf("legacy source still exists: %v", err)
	}
	r, err := remote.Get(context.Background(), "agent-a", "", "", "learner-skills/legacy/SKILL.md")
	if err != nil {
		t.Fatalf("remote learner missing: %v", err)
	}
	r.Close()
}

func TestMigrateLegacyLearnerSkillsMigratesWholeBatchBeforeMarkerBecomesAuthoritative(t *testing.T) {
	agentDir := t.TempDir()
	contents := map[string]string{
		"first":  "---\nname: First\ndescription: first migrated learner\n---\n\nFirst steps.\n",
		"second": "---\nname: Second\ndescription: second migrated learner\n---\n\nSecond steps.\n",
	}
	usage := &fakeLearnerMigrationUsage{}
	for _, slug := range []string{"first", "second"} {
		content := contents[slug]
		writeLegacyMigrationSkill(t, agentDir, slug, content, false)
		usage.rows = append(usage.rows, store.SkillUsageRow{
			Slug: slug, Origin: "learner", ContentHash: store.HashSkillContent(content),
		})
	}
	remote := workspace.NewLocalFS(t.TempDir())
	target := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())

	migrateLegacyLearnerSkills(context.Background(), usage, remote, "agent-a", agentDir, target)

	for _, slug := range []string{"first", "second"} {
		if got, ok := target.Read(slug); !ok || got != contents[slug] {
			t.Fatalf("batch target %q = (%q,%v)", slug, got, ok)
		}
		if _, err := os.Stat(filepath.Join(agentDir, "skills", slug)); !os.IsNotExist(err) {
			t.Fatalf("legacy source %q survived: %v", slug, err)
		}
		r, err := remote.Get(context.Background(), "agent-a", "", "", "learner-skills/"+slug+"/SKILL.md")
		if err != nil {
			t.Fatalf("remote batch learner %q missing: %v", slug, err)
		}
		r.Close()
	}
}

func TestMigrateLegacyLearnerSkillsLeavesAmbiguousSources(t *testing.T) {
	const content = "---\nname: Legacy\ndescription: migration guard\n---\n\nDo the steps.\n"
	for _, tc := range []struct {
		name      string
		hash      string
		extraFile bool
	}{
		{name: "hash mismatch", hash: store.HashSkillContent(content + "changed")},
		{name: "extra file", hash: store.HashSkillContent(content), extraFile: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agentDir := t.TempDir()
			writeLegacyMigrationSkill(t, agentDir, "legacy", content, tc.extraFile)
			usage := &fakeLearnerMigrationUsage{rows: []store.SkillUsageRow{{
				Slug: "legacy", Origin: "learner", ContentHash: tc.hash,
			}}}
			target := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())

			migrateLegacyLearnerSkills(context.Background(), usage, nil, "agent-a", agentDir, target)

			if _, ok := target.Read("legacy"); ok {
				t.Fatal("ambiguous source was migrated")
			}
			if _, err := os.Stat(filepath.Join(agentDir, "skills", "legacy", "SKILL.md")); err != nil {
				t.Fatalf("ambiguous source was removed: %v", err)
			}
		})
	}
}

func TestMigrateLegacyLearnerSkillsIsIdempotentForMatchingTarget(t *testing.T) {
	agentDir := t.TempDir()
	const content = "---\nname: Legacy\ndescription: idempotent migration\n---\n\nDo the steps.\n"
	writeLegacyMigrationSkill(t, agentDir, "legacy", content, false)
	target := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())
	if err := target.Create("legacy", content); err != nil {
		t.Fatal(err)
	}
	usage := &fakeLearnerMigrationUsage{rows: []store.SkillUsageRow{{
		Slug: "legacy", Origin: "learner", ContentHash: store.HashSkillContent(content),
	}}}

	migrateLegacyLearnerSkills(context.Background(), usage, nil, "agent-a", agentDir, target)

	if _, err := os.Stat(filepath.Join(agentDir, "skills", "legacy")); !os.IsNotExist(err) {
		t.Fatalf("matching legacy duplicate not reaped: %v", err)
	}
	if got, ok := target.Read("legacy"); !ok || got != content {
		t.Fatalf("target changed during idempotent migration: (%q,%v)", got, ok)
	}
}

func TestMigrateLegacyLearnerSkillsMirrorsExistingDedicatedLocalAsset(t *testing.T) {
	agentDir := t.TempDir()
	const content = "---\nname: Local\ndescription: local-only learner\n---\n\nDo the steps.\n"
	target := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())
	if err := target.Create("local", content); err != nil {
		t.Fatal(err)
	}
	usage := &fakeLearnerMigrationUsage{rows: []store.SkillUsageRow{{
		Slug: "local", Origin: "learner", ContentHash: store.HashSkillContent(content),
	}}}
	remote := workspace.NewLocalFS(t.TempDir())

	migrateLegacyLearnerSkills(context.Background(), usage, remote, "agent-a", agentDir, target)

	r, err := remote.Get(context.Background(), "agent-a", "", "", "learner-skills/local/SKILL.md")
	if err != nil {
		t.Fatalf("existing dedicated local learner was not mirrored: %v", err)
	}
	r.Close()
}

func TestMigrateLegacyLearnerSkillsDoesNotResurrectInitializedRemoteDeletion(t *testing.T) {
	agentDir := t.TempDir()
	const content = "---\nname: Deleted\ndescription: stale legacy learner\n---\n\nDo the steps.\n"
	writeLegacyMigrationSkill(t, agentDir, "deleted", content, false)
	usage := &fakeLearnerMigrationUsage{rows: []store.SkillUsageRow{{
		Slug: "deleted", Origin: "learner", ContentHash: store.HashSkillContent(content),
	}}}
	remote := workspace.NewLocalFS(t.TempDir())
	target := skills.NewManager(skills.LearnerSkillsDir(agentDir), skills.DefaultManagerConfig())

	// A marker with no skill objects is an authoritative remote tombstone: a
	// newer Pod deleted the last learner skill. A stale Pod may still have the
	// pre-isolation copy under <agent>/skills, but migration must not upload it.
	if err := skills.DeleteLearnerSkillUp(context.Background(), remote, "agent-a", "deleted"); err != nil {
		t.Fatal(err)
	}

	migrateLegacyLearnerSkills(context.Background(), usage, remote, "agent-a", agentDir, target)

	if _, ok := target.Read("deleted"); ok {
		t.Fatal("authoritative remote deletion was resurrected into the dedicated local layer")
	}
	if _, err := remote.Get(context.Background(), "agent-a", "", "", "learner-skills/deleted/SKILL.md"); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("authoritative remote deletion was resurrected: %v", err)
	}
}
