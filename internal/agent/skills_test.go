package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
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
