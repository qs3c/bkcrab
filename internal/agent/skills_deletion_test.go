package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/skills"
)

type testLearnerDeletionGuard struct {
	deleting bool
	err      error
}

func (g testLearnerDeletionGuard) IsAgentDeleting(context.Context, string) (bool, error) {
	return g.deleting, g.err
}

func writeLoaderLearnerSkill(t *testing.T, agentDir, slug string) {
	t.Helper()
	dir := filepath.Join(skills.LearnerSkillsDir(agentDir), slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: Stale\ndescription: stale learner\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSkillsLoaderHidesAndPrunesTombstonedLearnerLayer(t *testing.T) {
	agentDir := t.TempDir()
	writeLoaderLearnerSkill(t, agentDir, "stale")
	loader := NewSkillsLoader(t.TempDir(), agentDir, "", config.SkillsConfig{}).
		WithLearnerDeletionGuard(testLearnerDeletionGuard{deleting: true}, "deleted-agent")

	for _, skill := range loader.LoadSkills() {
		if skill.Layer == "learner" || skill.Name == "stale" {
			t.Fatalf("tombstoned learner leaked into catalog: %+v", skill)
		}
	}
	if _, err := os.Stat(filepath.Join(skills.LearnerSkillsDir(agentDir), "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale learner cache survived tombstone refresh: %v", err)
	}
	for _, dir := range loader.AllSkillDirs() {
		got, _ := filepath.Abs(dir)
		want, _ := filepath.Abs(skills.LearnerSkillsDir(agentDir))
		if filepath.Clean(got) == filepath.Clean(want) {
			t.Fatalf("tombstoned learner root remained loadable: %v", loader.AllSkillDirs())
		}
	}
}

func TestSkillsLoaderFailsClosedWhenDeletionStateUnavailable(t *testing.T) {
	agentDir := t.TempDir()
	writeLoaderLearnerSkill(t, agentDir, "stale")
	loader := NewSkillsLoader(t.TempDir(), agentDir, "", config.SkillsConfig{}).
		WithLearnerDeletionGuard(testLearnerDeletionGuard{err: errors.New("database unavailable")}, "agent-a")

	for _, skill := range loader.LoadSkills() {
		if skill.Layer == "learner" || skill.Name == "stale" {
			t.Fatalf("unverified learner leaked into catalog: %+v", skill)
		}
	}
	if !loader.learnerUnavailable {
		t.Fatal("learner layer was not marked unavailable")
	}
}
