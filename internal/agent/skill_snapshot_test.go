package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestPublishedSkillSummaryAndExecutionShareSnapshot(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("BKCRAB_HOME", home)
	db, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	publisher := skills.NewPublishedStore(workspace.NewLocalFS(t.TempDir()), db)
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "demo"), 0755); err != nil {
		t.Fatal(err)
	}
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, "demo", "SKILL.md"), []byte("---\nname: demo\ndescription: "+text+"\n---\n"+text), 0644); err != nil {
			t.Fatal(err)
		}
		if err := publisher.PublishSkill(ctx, "agent", "demo", source); err != nil {
			t.Fatal(err)
		}
	}
	write("old")
	agentDir := filepath.Join(home, "agents", "agent")
	loader := NewSkillsLoaderWithGlobal(home, agentDir, "", config.SkillsConfig{}, config.SkillsCfg{}).WithObjectStore(publisher, "agent")
	initial := loader.LoadSkills()
	found := false
	for _, s := range initial {
		if s.Name == "demo" {
			found = true
			if s.Description != "old" {
				t.Fatal(s.Description)
			}
		}
	}
	if !found {
		t.Fatal("published skill missing from summary")
	}
	write("new")
	if err := publisher.HydratePublishedSkills(ctx, "agent", filepath.Join(agentDir, "skills")); err != nil {
		t.Fatal(err)
	}
	dirs := loader.AllSkillDirs()
	b, err := os.ReadFile(filepath.Join(dirs[0], "demo", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "---\nname: demo\ndescription: old\n---\nold" {
		t.Fatalf("old summary paired with newer executable: %s", b)
	}
	next := loader.LoadSkills()
	for _, s := range next {
		if s.Name == "demo" && s.Description != "new" {
			t.Fatal("next turn did not refresh")
		}
	}
	if dirs[0] == loader.AllSkillDirs()[0] {
		t.Fatal("next turn kept old view")
	}
}
