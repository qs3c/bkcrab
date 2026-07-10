package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListMissingRootReturnsEmpty(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "none"), DefaultManagerConfig())
	if got := m.List(); len(got) != 0 {
		t.Fatalf("List() on missing root = %v, want empty", got)
	}
}

func TestListReturnsSkillsSortedWithMetadata(t *testing.T) {
	m := NewManager(t.TempDir(), DefaultManagerConfig())
	if err := m.Create("zeta-skill", "---\nname: Zeta\ndescription: does z\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Create("alpha-skill", "---\nname: Alpha\ndescription: does a\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	got := m.List()
	if len(got) != 2 || got[0].Slug != "alpha-skill" || got[1].Slug != "zeta-skill" {
		t.Fatalf("List() = %+v, want [alpha-skill zeta-skill]", got)
	}
	if got[0].Name != "Alpha" || got[0].Description != "does a" {
		t.Fatalf("List()[0] = %+v, want frontmatter metadata", got[0])
	}
}

func TestListSkipsBrokenFrontmatter(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, DefaultManagerConfig())
	if err := m.Create("good-skill", "---\nname: Good\ndescription: ok\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "bad-skill")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.List()
	if len(got) != 1 || got[0].Slug != "good-skill" {
		t.Fatalf("List() = %+v, want only good-skill", got)
	}
}
