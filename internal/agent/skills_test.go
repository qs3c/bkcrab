package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	skillspkg "github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestBuildSkillsSummaryUsesProgressiveDisclosureByDefault(t *testing.T) {
	t.Setenv("BKCRAB_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

SECRET_INLINE_BODY_SHOULD_NOT_APPEAR
Run scripts/render.py with JSON input.`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), "", config.SkillsConfig{}, config.SkillsCfg{})
	summary := loader.BuildSkillsSummary(loader.LoadSkills())

	if !strings.Contains(summary, "chart-maker") {
		t.Fatalf("summary missing skill name:\n%s", summary)
	}
	if !strings.Contains(summary, "Build charts from tabular data") {
		t.Fatalf("summary missing skill description:\n%s", summary)
	}
	if strings.Contains(summary, "SECRET_INLINE_BODY_SHOULD_NOT_APPEAR") {
		t.Fatalf("summary leaked SKILL.md body:\n%s", summary)
	}
	if !strings.Contains(summary, "load_skill") {
		t.Fatalf("summary should tell the model to call load_skill:\n%s", summary)
	}
}

func TestLoadSkillsDoesNotKeepBodyContentByDefault(t *testing.T) {
	t.Setenv("BKCRAB_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "chart-maker")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: chart-maker
description: Build charts from tabular data.
---

BODY_SHOULD_STAY_ON_DISK_UNTIL_LOAD_SKILL`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(home, t.TempDir(), "", config.SkillsConfig{}, config.SkillsCfg{})
	skills := loader.LoadSkills()

	if len(skills) != 1 {
		t.Fatalf("skills len = %d, want 1", len(skills))
	}
	if skills[0].Content != "" {
		t.Fatalf("LoadSkills should not keep default skill body in memory, got:\n%s", skills[0].Content)
	}
}

func TestBuildSkillsSummaryKeepsAlwaysLoadSkillsInline(t *testing.T) {
	t.Setenv("BKCRAB_HOME", t.TempDir())
	home := t.TempDir()
	skillDir := filepath.Join(home, "skills", "always-inline")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `---
name: always-inline
description: Needs full instructions immediately.
---

ALWAYS_LOAD_BODY_SHOULD_APPEAR`
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	loader := NewSkillsLoaderWithGlobal(
		home,
		t.TempDir(),
		"",
		config.SkillsConfig{AlwaysLoad: []string{"always-inline"}},
		config.SkillsCfg{},
	)
	summary := loader.BuildSkillsSummary(loader.LoadSkills())

	if !strings.Contains(summary, "ALWAYS_LOAD_BODY_SHOULD_APPEAR") {
		t.Fatalf("summary should inline explicitly always-loaded skill:\n%s", summary)
	}
}

func TestFilterLearnerSkillsByActive(t *testing.T) {
	all := []Skill{
		{Name: "manual", Layer: "user"},
		{Name: "hi", Layer: "learner"},
		{Name: "lo", Layer: "learner"},
	}
	rows := []store.SkillUsageRow{
		{Slug: "hi", Origin: "learner", Activity: 10, LastLoadSeq: 100, TotalLoads: 3, CreatedSeq: 0},
		{Slug: "lo", Origin: "learner", Activity: 1, LastLoadSeq: 100, TotalLoads: 3, CreatedSeq: 0},
	}
	out := filterActiveSkills(all, rows, skillspkg.LifecycleConfig{ActiveMax: 1, ProtectLoads: 20})
	names := map[string]bool{}
	for _, s := range out {
		names[s.Name] = true
	}
	if !names["manual"] || !names["hi"] || names["lo"] {
		t.Fatalf("want manual+hi kept, lo dropped. got %+v", names)
	}
}

func TestFilterActiveSkillsAppliesHardCapWhenLedgerIsEmpty(t *testing.T) {
	all := []Skill{
		{Name: "manual", Layer: "agent"},
		{Name: "b", Layer: "learner"},
		{Name: "a", Layer: "learner"},
	}
	out := filterActiveSkills(all, nil, skillspkg.LifecycleConfig{ActiveMax: 1})
	if len(out) != 2 || out[0].Name != "manual" || out[1].Name != "a" {
		t.Fatalf("empty ledger should keep manual skills and one deterministic learner, got %+v", out)
	}
}

func TestLoadSkillsLearnerLayerIsSharedAndLowestPriority(t *testing.T) {
	t.Setenv("BKCRAB_HOME", t.TempDir())
	home := t.TempDir()
	agentDir := t.TempDir()
	writeSkill := func(root, slug, description, marker string) {
		t.Helper()
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + slug + "\ndescription: " + description + "\n---\n" + marker + "\n"
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	learnerDir := skillspkg.LearnerSkillsDir(agentDir)
	writeSkill(learnerDir, "learner-only", "generated asset", "learner")
	writeSkill(learnerDir, "same-slug", "generated duplicate", "learner")
	writeSkill(filepath.Join(agentDir, "skills"), "same-slug", "manual asset", "manual")

	for _, chatter := range []string{"owner-a", "guest-b"} {
		loader := NewSkillsLoaderWithGlobal(home, agentDir, "", config.SkillsConfig{}, config.SkillsCfg{}).
			WithUserID(chatter)
		loaded := loader.LoadSkills()
		byName := make(map[string]Skill, len(loaded))
		for _, skill := range loaded {
			byName[skill.Name] = skill
		}
		if got := byName["learner-only"]; got.Layer != "learner" {
			t.Fatalf("chatter %s learner-only = %+v, want learner layer", chatter, got)
		}
		if got := byName["same-slug"]; got.Layer != "agent" || got.Description != "manual asset" {
			t.Fatalf("chatter %s same-slug = %+v, want manual agent skill to win", chatter, got)
		}
		dirs := loader.AllSkillDirs()
		if len(dirs) == 0 || dirs[len(dirs)-1] != learnerDir {
			t.Fatalf("chatter %s skill dir order = %v, want learner dir last", chatter, dirs)
		}
		if loader.LearnerSkillsDir() != learnerDir {
			t.Fatalf("chatter %s learner dir = %q, want %q", chatter, loader.LearnerSkillsDir(), learnerDir)
		}
	}
}
