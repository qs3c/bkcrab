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
		{Name: "hi", Layer: "agent"},
		{Name: "lo", Layer: "agent"},
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

func TestFilterActiveSkillsFailOpenOnEmptyRows(t *testing.T) {
	all := []Skill{{Name: "a", Layer: "agent"}, {Name: "b", Layer: "agent"}}
	out := filterActiveSkills(all, nil, skillspkg.LifecycleConfig{ActiveMax: 1})
	if len(out) != len(all) {
		t.Fatalf("empty ledger should keep all skills, got %d", len(out))
	}
}
