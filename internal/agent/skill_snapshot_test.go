package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/contextcache"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestPublishedSkillSummaryAndExecutionShareSnapshot(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	t.Setenv("BKCRAB_HOME", home)
	dsn := "file:" + filepath.Join(t.TempDir(), "test.db") + "?_pragma=busy_timeout(5000)"
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if addr := os.Getenv("BKCRAB_CACHE_TEST_REDIS"); addr != "" {
		cfg := contextcache.Config{Addr: addr, Prefix: "bkcrab:agentctx:test:" + uuid.NewString() + ":"}
		for _, d := range []*store.DBStore{db, peer} {
			if err = d.EnableContextCache(cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	objects := workspace.NewLocalFS(t.TempDir())
	publisher := skills.NewPublishedStore(objects, db)
	reader := skills.NewPublishedStore(objects, peer)
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
	loader := NewSkillsLoaderWithGlobal(home, agentDir, "", config.SkillsConfig{}, config.SkillsCfg{}).WithObjectStore(reader, "agent")
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
	// Warm the peer's catalog before publishing, so the next turn must see the
	// addition through invalidation rather than waiting for the 30-minute TTL.
	if os.Getenv("BKCRAB_CACHE_TEST_REDIS") != "" {
		deadline := time.Now().Add(5 * time.Second)
		for {
			before := peer.ContextCacheStats().Hits
			if _, err := peer.ListSkillPublications(ctx, "agent"); err != nil {
				t.Fatal(err)
			}
			if peer.ContextCacheStats().Hits > before {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("peer catalog never entered cache")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	write("new")
	if err := os.MkdirAll(filepath.Join(source, "added"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "added", "SKILL.md"), []byte("---\nname: added\ndescription: new skill\n---\nnew tool instructions"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := publisher.PublishSkill(ctx, "agent", "added", source); err != nil {
		t.Fatal(err)
	}
	if err := reader.HydratePublishedSkills(ctx, "agent", filepath.Join(agentDir, "skills")); err != nil {
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
	if _, err := os.Stat(filepath.Join(dirs[0], "added", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("new skill leaked into active turn", err)
	}
	next := loader.LoadSkills()
	seen := map[string]string{}
	for _, s := range next {
		seen[s.Name] = s.Description
	}
	if seen["demo"] != "new" || seen["added"] != "new skill" {
		t.Fatalf("next turn did not see update and addition: %+v", seen)
	}
	nextDirs := loader.AllSkillDirs()
	if data, err := os.ReadFile(filepath.Join(nextDirs[0], "added", "SKILL.md")); err != nil || !strings.Contains(string(data), "new tool instructions") {
		t.Fatalf("new skill missing from execution view: %q %v", data, err)
	}
	if dirs[0] == loader.AllSkillDirs()[0] {
		t.Fatal("next turn kept old view")
	}
}
