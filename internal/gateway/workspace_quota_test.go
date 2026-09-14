package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/sandbox"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestPublishedSkillsShareUserWorkspaceQuota(t *testing.T) {
	ctx := context.Background()
	db, err := store.NewDBStore("sqlite", filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAgent(ctx, &store.AgentRecord{ID: "agent-a", UserID: "alice", Name: "a"}); err != nil {
		t.Fatal(err)
	}
	raw := workspace.NewLocalFS(t.TempDir())
	// Existing skill blobs must count when the process starts with an empty quota cache.
	if err := raw.Put(ctx, skills.UserSkillOwner("alice"), "", "", "_skill_blobs/existing", strings.NewReader("12345"), 5, ""); err != nil {
		t.Fatal(err)
	}
	quota := withWorkspaceQuota(db, raw, sandbox.Limits{WorkspaceBytes: 10, WorkspaceFiles: 10})
	if err := quota.Put(ctx, "agent-a", "", "", "too-big", strings.NewReader("123456"), 6, ""); err == nil {
		t.Fatal("existing user skill storage was omitted")
	}
	if err := quota.Put(ctx, "agent-a", "", "", "fits", strings.NewReader("12345"), 5, ""); err != nil {
		t.Fatal(err)
	}
	published := skills.NewPublishedStore(quota, db)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "demo"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "demo", "SKILL.md"), []byte("skill"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := published.PublishSkill(ctx, skills.UserSkillOwner("alice"), "demo", root); err == nil {
		t.Fatal("publication bypassed full user quota")
	}
	rows, err := db.ListSkillPublications(ctx, skills.UserSkillOwner("alice"))
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed upload published a package: %v %v", rows, err)
	}
	// Global packages use a separate bounded scope and need no synthetic Agent row.
	if err := published.PublishSkill(ctx, skills.GlobalSkillOwner, "demo", root); err != nil {
		t.Fatal(err)
	}
	if _, ok := published.LocalScopeDir("agent-a", "", ""); !ok {
		t.Fatal("publication wrapper hid local hydration")
	}
}
