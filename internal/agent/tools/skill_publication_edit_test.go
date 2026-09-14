package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type editPublicationDB struct {
	rows map[string]store.SkillPublication
	fail bool
}

func (d *editPublicationDB) SaveSkillPublication(_ context.Context, _ string, p store.SkillPublication) error {
	if d.fail {
		return errors.New("publication failed")
	}
	d.rows[p.Slug] = p
	return nil
}
func (d *editPublicationDB) ListSkillPublications(_ context.Context, _ string) ([]store.SkillPublication, error) {
	var out []store.SkillPublication
	for _, p := range d.rows {
		out = append(out, p)
	}
	return out, nil
}

func TestPublishedSkillEditPreservesTurnVersion(t *testing.T) {
	for _, sandboxMode := range []bool{false, true} {
		name := "host"
		if sandboxMode {
			name = "sandbox"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			skillRoot := filepath.Join(root, "skills")
			db := &editPublicationDB{rows: map[string]store.SkillPublication{}}
			p := skills.NewPublishedStore(workspace.NewLocalFS(t.TempDir()), db)
			if err := p.WritePublishedSkillFile(ctx, "a", skillRoot, "demo", "SKILL.md", []byte("OLD")); err != nil {
				t.Fatal(err)
			}
			frozen := p.FrozenSkillDirs([]string{skillRoot})[0]
			r := NewRegistry(root, t.TempDir())
			r.workspaceStore = p
			r.agentID = "a"
			edit := makeEditFile(r)
			if sandboxMode {
				registerSandboxedFile(r, nil) // Skill edits must not execute in the sandbox.
				edit = r.GetFunc("edit_file")
			}
			args, _ := json.Marshal(editFileArgs{Path: "skills/demo/SKILL.md", OldString: "OLD", NewString: "NEW"})
			db.fail = true
			if _, err := edit(ctx, args); err == nil {
				t.Fatal("publication failure hidden")
			}
			check := func(dir, want string) {
				t.Helper()
				b, err := os.ReadFile(filepath.Join(dir, "demo", "SKILL.md"))
				if err != nil || string(b) != want {
					t.Fatalf("read %s: %q %v", dir, b, err)
				}
			}
			check(skillRoot, "OLD")
			check(frozen, "OLD")
			db.fail = false
			if _, err := edit(ctx, args); err != nil {
				t.Fatal(err)
			}
			check(frozen, "OLD")
			check(skillRoot, "NEW")
			for _, path := range []string{filepath.Join(frozen, "demo", "SKILL.md"), filepath.Join(skillRoot, "demo", "SKILL.md"), "skills/.versions/demo/bad/SKILL.md"} {
				direct, _ := json.Marshal(editFileArgs{Path: path, OldString: "OLD", NewString: "CORRUPTED"})
				if _, err := edit(ctx, direct); err == nil || !strings.Contains(err.Error(), "read-only") {
					t.Fatalf("direct version edit not refused: %s (%v)", path, err)
				}
				write := makeWriteFile(r)
				if sandboxMode {
					write = r.GetFunc("write_file")
				}
				directWrite, _ := json.Marshal(writeFileArgs{Path: path, Content: "CORRUPTED"})
				if _, err := write(ctx, directWrite); err == nil || !strings.Contains(err.Error(), "read-only") {
					t.Fatalf("direct version write not refused: %s (%v)", path, err)
				}
			}
			check(frozen, "OLD")
			check(skillRoot, "NEW")
			peerRoot := t.TempDir()
			if err := skills.NewPublishedStore(p.Store, db).HydratePublishedSkills(ctx, "a", peerRoot); err != nil {
				t.Fatal(err)
			}
			check(peerRoot, "NEW")
		})
	}
}
