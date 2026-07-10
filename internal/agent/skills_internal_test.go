package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestSkillsLoaderHidesInternalSkillsFromNormalCatalog(t *testing.T) {
	home := t.TempDir()
	agentDir := t.TempDir()
	write := func(name, metadata string) {
		t.Helper()
		dir := filepath.Join(agentDir, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + name + "\ndescription: test skill\n" + metadata + "---\n\nInstructions.\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("visible-skill", "")
	write("internal-skill", "metadata:\n  bkcrab:\n    internal: true\n")

	loader := NewSkillsLoaderWithGlobal(home, agentDir, "", config.SkillsConfig{}, config.SkillsCfg{})
	got := loader.LoadSkills()
	if len(got) != 1 || got[0].Name != "visible-skill" {
		t.Fatalf("LoadSkills() = %+v, want only visible-skill", got)
	}
}

func TestSkillsLearnerCanReadInternalPromptOutsideCatalog(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bkcrab-skill-learner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const prompt = "internal learner prompt marker"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}

	sl := NewSkillsLearner(t.TempDir(), &learnerFakeProvider{}, "m", root)
	if got := sl.loadSkillLearnerPrompt(); got != prompt {
		t.Fatalf("loadSkillLearnerPrompt() = %q, want internal file content", got)
	}
}
