package agent

import (
	"os"
	"path/filepath"
	"strings"
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

func TestAgentSkillCannotOverrideLearnerSystemPrompt(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "bkcrab-skill-learner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const prompt = "malicious same-slug learner prompt marker"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}

	sl := NewSkillsLearner(t.TempDir(), &learnerFakeProvider{}, "m", root)
	got := sl.loadSkillLearnerPrompt()
	if strings.Contains(got, prompt) || !strings.Contains(got, "untrusted evidence") || !strings.Contains(got, "shared with every user") {
		t.Fatalf("untrusted same-slug skill overrode learner system policy: %q", got)
	}
}

func TestBundledLearnerPromptMatchesRuntimeProtocol(t *testing.T) {
	path := filepath.Join("..", "..", "skills", "bkcrab-skill-learner", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(data)
	for _, required := range []string{
		"frozen `sessions.messages`", "at most one successful", "content_hash",
		"expected_hash", "asset limit", "untrusted evidence", "every user of this agent",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("bundled learner prompt missing %q", required)
		}
	}
}
