package skills

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type publicationDB struct {
	mu   sync.Mutex
	rows map[string]map[string]store.SkillPublication
}

func (db *publicationDB) SaveSkillPublication(_ context.Context, o string, p store.SkillPublication) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.rows[o] == nil {
		db.rows[o] = map[string]store.SkillPublication{}
	}
	db.rows[o][p.Slug] = p
	return nil
}
func (db *publicationDB) ListSkillPublications(_ context.Context, o string) ([]store.SkillPublication, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []store.SkillPublication
	for _, p := range db.rows[o] {
		out = append(out, p)
	}
	return out, nil
}

type failBlobStore struct {
	workspace.Store
	fail  bool
	lists int
}

func (s *failBlobStore) Put(ctx context.Context, a, p, k, f string, r io.Reader, n int64, c string) error {
	if s.fail {
		return errors.New("injected upload failure")
	}
	return s.Store.Put(ctx, a, p, k, f, r, n, c)
}
func (s *failBlobStore) List(ctx context.Context, a, p, k string) ([]workspace.ObjectInfo, error) {
	s.lists++
	return s.Store.List(ctx, a, p, k)
}
func TestPublicationAtomicUpdateDeleteAndPinnedVersion(t *testing.T) {
	ctx := context.Background()
	raw, err := workspace.Factory{Type: "local", LocalDir: t.TempDir()}.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blobs := &failBlobStore{Store: raw}
	db := &publicationDB{rows: map[string]map[string]store.SkillPublication{}}
	a := NewPublishedStore(blobs, db)
	b := NewPublishedStore(blobs, db)
	source := t.TempDir()
	os.MkdirAll(filepath.Join(source, "demo"), 0755)
	old := "---\nname: demo\ndescription: old\n---\nOLD"
	newText := "---\nname: demo\ndescription: new\n---\nNEW"
	os.WriteFile(filepath.Join(source, "demo", "SKILL.md"), []byte(old), 0644)
	if err = a.PublishSkill(ctx, "owner", "demo", source); err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	if err = b.HydratePublishedSkills(ctx, "owner", local); err != nil {
		t.Fatal(err)
	}
	pinned := b.FrozenSkillDirs([]string{local})[0]
	os.WriteFile(filepath.Join(source, "demo", "SKILL.md"), []byte(newText), 0644)
	blobs.fail = true
	if err = a.PublishSkill(ctx, "owner", "demo", source); err == nil {
		t.Fatal("failed upload published")
	}
	blobs.fail = false
	if err = b.HydratePublishedSkills(ctx, "owner", local); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(filepath.Join(local, "demo", "SKILL.md"))
	if string(content) != old {
		t.Fatal("partial version visible")
	}
	if err = a.PublishSkill(ctx, "owner", "demo", source); err != nil {
		t.Fatal(err)
	}
	if err = b.HydratePublishedSkills(ctx, "owner", local); err != nil {
		t.Fatal(err)
	}
	content, _ = os.ReadFile(filepath.Join(local, "demo", "SKILL.md"))
	if string(content) != newText {
		t.Fatalf("same size update not seen %q", content)
	}
	content, _ = os.ReadFile(filepath.Join(pinned, "demo", "SKILL.md"))
	if string(content) != old {
		t.Fatal("old turn changed")
	}
	lists := blobs.lists
	if err = b.HydratePublishedSkills(ctx, "owner", local); err != nil {
		t.Fatal(err)
	}
	if blobs.lists != lists {
		t.Fatal("hot hydrate called object LIST")
	}
	mgr := NewManager(local, DefaultManagerConfig())
	mgr.SetPublisher(b, "owner")
	if err = mgr.Update("demo", old); err != nil {
		t.Fatal(err)
	}
	if err = a.DeletePublishedSkill(ctx, "owner", "demo"); err != nil {
		t.Fatal(err)
	}
	if err = b.HydratePublishedSkills(ctx, "owner", local); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(local, "demo")); !os.IsNotExist(err) {
		t.Fatal("last skill not deleted")
	}
	if err = b.MirrorPublishedSkills(ctx, "owner", local); err != nil {
		t.Fatal(err)
	}
	names, _ := b.PublishedSkillNames(ctx, "owner")
	if len(names) != 0 {
		t.Fatal("deleted skill resurrected")
	}
}
func TestPublicationRejectsTraversalManifest(t *testing.T) {
	raw, _ := workspace.Factory{Type: "local", LocalDir: t.TempDir()}.New(t.TempDir())
	db := &publicationDB{rows: map[string]map[string]store.SkillPublication{}}
	s := NewPublishedStore(raw, db)
	m, _ := json.Marshal(packageManifest{Files: []packageFile{{Path: "../escape", Hash: "bad"}}})
	db.SaveSkillPublication(context.Background(), "u", store.SkillPublication{Slug: "bad", Manifest: m})
	if err := s.HydratePublishedSkills(context.Background(), "u", t.TempDir()); err == nil {
		t.Fatal("unsafe manifest accepted")
	}
}

func TestPublicationFirstSkillWaitsForNextTurn(t *testing.T) {
	ctx := context.Background()
	db := &publicationDB{rows: map[string]map[string]store.SkillPublication{}}
	p := NewPublishedStore(workspace.NewLocalFS(t.TempDir()), db)
	root := filepath.Join(t.TempDir(), "skills")
	if err := p.HydratePublishedSkills(ctx, "a", root); err != nil {
		t.Fatal(err)
	}
	oldView := p.FrozenSkillDirs([]string{root})[0]
	if err := p.WritePublishedSkillFile(ctx, "a", root, "first", "SKILL.md", []byte("first skill")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(oldView, "first", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("first skill leaked into old empty turn", err)
	}
	newView := p.FrozenSkillDirs([]string{root})[0]
	data, err := os.ReadFile(filepath.Join(newView, "first", "SKILL.md"))
	if err != nil || string(data) != "first skill" || newView == oldView {
		t.Fatalf("next turn did not get first skill: %q %v", data, err)
	}
}
