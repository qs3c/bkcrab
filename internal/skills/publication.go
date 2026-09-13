package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type PublicationStore interface {
	SaveSkillPublication(context.Context, string, store.SkillPublication) error
	ListSkillPublications(context.Context, string) ([]store.SkillPublication, error)
}
type packageFile struct {
	Path, Hash string
	Mode       uint32
	Size       int64
}
type packageManifest struct{ Files []packageFile }

// PublishedStore exposes immutable skill packages alongside normal workspace
// objects. Uploads finish before the atomic SQL publication pointer is changed.
type PublishedStore struct {
	workspace.Store
	db    PublicationStore
	mu    sync.Mutex
	local map[string]string
	roots map[string]string
}

func NewPublishedStore(inner workspace.Store, db PublicationStore) *PublishedStore {
	return &PublishedStore{Store: inner, db: db, local: make(map[string]string), roots: make(map[string]string)}
}
func safeSkillPath(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.HasPrefix(s, "/") && path.Clean(s) == s && !strings.HasPrefix(s, "../") && !strings.Contains(s, "\\")
}
func blobPath(hash string) string { return "_skill_blobs/" + hash }
func validHash(s string) bool     { b, e := hex.DecodeString(s); return e == nil && len(b) == 32 }
func (s *PublishedStore) putBlob(ctx context.Context, owner string, b []byte) (string, error) {
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	return h, s.Store.Put(ctx, owner, "", "", blobPath(h), bytes.NewReader(b), int64(len(b)), "")
}
func (s *PublishedStore) PublishSkill(ctx context.Context, owner, slug, root string) error {
	if !safeSkillPath(slug) || strings.Contains(slug, "/") {
		return fmt.Errorf("invalid skill slug")
	}
	var m packageManifest
	sourceRoot, err := filepath.EvalSymlinks(filepath.Join(root, slug))
	if err != nil {
		return err
	}
	err = filepath.WalkDir(sourceRoot, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill package contains symlink: %s", e.Name())
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill package contains nonregular file")
		}
		rel, err := filepath.Rel(sourceRoot, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !safeSkillPath(rel) {
			return fmt.Errorf("invalid package path")
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h, err := s.putBlob(ctx, owner, b)
		if err != nil {
			return err
		}
		m.Files = append(m.Files, packageFile{rel, h, uint32(info.Mode().Perm()), int64(len(b))})
		return nil
	})
	if err != nil {
		return err
	}
	found := false
	for _, f := range m.Files {
		if f.Path == "SKILL.md" {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("incomplete skill: SKILL.md is missing")
	}
	return s.publish(ctx, owner, slug, m, false)
}
func (s *PublishedStore) publish(ctx context.Context, owner, slug string, m packageManifest, deleted bool) error {
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.db.SaveSkillPublication(ctx, owner, store.SkillPublication{Slug: slug, Manifest: b, Deleted: deleted})
}
func (s *PublishedStore) DeletePublishedSkill(ctx context.Context, owner, slug string) error {
	return s.publish(ctx, owner, slug, packageManifest{}, true)
}

// Import legacy object trees once. Publication is per complete skill. A crash
// can leave unreferenced blobs, never a half-published SKILL.md/script tree.
func (s *PublishedStore) publications(ctx context.Context, owner string) ([]store.SkillPublication, error) {
	rows, err := s.db.ListSkillPublications(ctx, owner)
	if err != nil {
		return nil, err
	}
	initialized := false
	known := map[string]bool{}
	for _, p := range rows {
		known[p.Slug] = true
		if p.Slug == "_initialized" {
			initialized = true
		}
	}
	if initialized {
		return rows, nil
	}
	objs, err := s.Store.List(ctx, owner, "", "")
	if err != nil {
		return nil, err
	}
	groups := map[string]packageManifest{}
	for _, o := range objs {
		if !strings.HasPrefix(o.Path, "skills/") {
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(o.Path, "skills/"), "/", 2)
		if len(parts) != 2 || known[parts[0]] {
			continue
		}
		if !safeSkillPath(parts[0]) || !safeSkillPath(parts[1]) {
			return nil, fmt.Errorf("invalid legacy skill path")
		}
		r, err := s.Store.Get(ctx, owner, "", "", o.Path)
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return nil, err
		}
		h, err := s.putBlob(ctx, owner, b)
		if err != nil {
			return nil, err
		}
		m := groups[parts[0]]
		m.Files = append(m.Files, packageFile{parts[1], h, 0644, int64(len(b))})
		groups[parts[0]] = m
	}
	for slug, m := range groups {
		has := false
		for _, f := range m.Files {
			if f.Path == "SKILL.md" {
				has = true
			}
		}
		if has {
			b, e := json.Marshal(m)
			if e != nil {
				return nil, e
			}
			if importer, ok := s.db.(interface {
				ImportSkillPublication(context.Context, string, store.SkillPublication) error
			}); ok {
				err = importer.ImportSkillPublication(ctx, owner, store.SkillPublication{Slug: slug, Manifest: b})
			} else {
				err = s.publish(ctx, owner, slug, m, false)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	if err = s.publish(ctx, owner, "_initialized", packageManifest{}, true); err != nil {
		return nil, err
	}
	return s.db.ListSkillPublications(ctx, owner)
}
func (s *PublishedStore) HydratePublishedSkills(ctx context.Context, owner, root string, keep ...string) error {
	s.mu.Lock()
	s.roots[root] = owner
	s.mu.Unlock()
	rows, err := s.publications(ctx, owner)
	if err != nil {
		return err
	}
	// Serialize local materialization; never expose a partially downloaded tree.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range rows {
		if p.Slug == "_initialized" {
			continue
		}
		if !safeSkillPath(p.Slug) || strings.Contains(p.Slug, "/") {
			return fmt.Errorf("invalid published skill slug")
		}
		target := filepath.Join(root, p.Slug)
		sum := sha256.Sum256(p.Manifest)
		version := hex.EncodeToString(sum[:])
		if p.Deleted {
			if err = os.RemoveAll(target); err != nil {
				return err
			}
			delete(s.local, target)
			continue
		}
		if s.local[target] == version {
			if _, err = os.Stat(filepath.Join(target, "SKILL.md")); err == nil {
				continue
			}
		}
		var m packageManifest
		if err = json.Unmarshal(p.Manifest, &m); err != nil {
			return err
		}
		if err = os.MkdirAll(root, 0755); err != nil {
			return err
		}
		tmp, err := os.MkdirTemp(root, ".hydrate-")
		if err != nil {
			return err
		}
		installErr := func() error {
			for _, f := range m.Files {
				if !safeSkillPath(f.Path) || !validHash(f.Hash) {
					return fmt.Errorf("invalid manifest entry")
				}
				r, err := s.Store.Get(ctx, owner, "", "", blobPath(f.Hash))
				if err != nil {
					return err
				}
				b, err := io.ReadAll(r)
				r.Close()
				if err != nil {
					return err
				}
				h := sha256.Sum256(b)
				if hex.EncodeToString(h[:]) != f.Hash {
					return fmt.Errorf("skill blob hash mismatch")
				}
				dest := filepath.Join(tmp, filepath.FromSlash(f.Path))
				if err = os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
					return err
				}
				if err = os.WriteFile(dest, b, os.FileMode(f.Mode)&0777); err != nil {
					return err
				}
			}
			if _, err := os.Stat(filepath.Join(tmp, "SKILL.md")); err != nil {
				return err
			}
			// Version directories retain previous packages; switching a symlink is atomic.
			versionRoot := filepath.Join(root, ".versions", p.Slug)
			if err = os.MkdirAll(versionRoot, 0755); err != nil {
				return err
			}
			versionDir := filepath.Join(versionRoot, version)
			if _, err = os.Stat(versionDir); os.IsNotExist(err) {
				if err = os.Rename(tmp, versionDir); err != nil {
					return err
				}
			}
			link := tmp + "-link"
			if err = os.Symlink(versionDir, link); err != nil {
				return err
			}
			defer os.Remove(link)
			if info, err := os.Lstat(target); err == nil && info.IsDir() {
				backup := filepath.Join(versionRoot, fmt.Sprintf("legacy-%d", time.Now().UnixNano()))
				if err = os.Rename(target, backup); err != nil {
					return err
				}
			}
			return os.Rename(link, target)
		}()
		os.RemoveAll(tmp)
		if installErr != nil {
			return installErr
		}
		s.local[target] = version
	}
	return nil
}
func (s *PublishedStore) MirrorPublishedSkills(ctx context.Context, owner, root string) error {
	rows, err := s.publications(ctx, owner)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, p := range rows {
		known[p.Slug] = true
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || known[e.Name()] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "SKILL.md")); err == nil {
			if err = s.PublishSkill(ctx, owner, e.Name(), root); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *PublishedStore) PublishedSkillNames(ctx context.Context, owner string) ([]string, error) {
	rows, err := s.publications(ctx, owner)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, p := range rows {
		if !p.Deleted {
			names = append(names, p.Slug)
		}
	}
	return names, nil
}

// WritePublishedSkillFile stages a copy; editing a published symlink must not
// mutate bytes still referenced by an earlier turn's immutable package.
func (s *PublishedStore) WritePublishedSkillFile(ctx context.Context, owner, root, slug, rel string, data []byte) error {
	if !safeSkillPath(slug) || strings.Contains(slug, "/") || !safeSkillPath(rel) {
		return fmt.Errorf("invalid skill write path")
	}
	stage, err := os.MkdirTemp("", "bkcrab-skill-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	dest := filepath.Join(stage, slug)
	if err = os.MkdirAll(dest, 0755); err != nil {
		return err
	}
	source, err := filepath.EvalSymlinks(filepath.Join(root, slug))
	if err == nil {
		if err = os.CopyFS(dest, os.DirFS(source)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file := filepath.Join(dest, filepath.FromSlash(rel))
	if err = os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return err
	}
	if err = os.WriteFile(file, data, 0644); err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(dest, "SKILL.md")); os.IsNotExist(err) {
		// Incomplete new scaffolds remain local until SKILL.md is written.
		file = filepath.Join(root, slug, filepath.FromSlash(rel))
		if err = os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			return err
		}
		return os.WriteFile(file, data, 0644)
	}
	if err = s.PublishSkill(ctx, owner, slug, stage); err != nil {
		return err
	}
	return s.HydratePublishedSkills(ctx, owner, root)
}

// FrozenSkillDirs pins symlink targets for load_skill in the current turn.
func (s *PublishedStore) FrozenSkillDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, root := range dirs {
		entries, err := os.ReadDir(root)
		if err != nil {
			out = append(out, root)
			continue
		}
		targets := map[string]string{}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			p := filepath.Join(root, e.Name())
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				continue
			}
			if _, err = os.Stat(filepath.Join(resolved, "SKILL.md")); err == nil {
				targets[e.Name()] = resolved
			}
		}
		b, _ := json.Marshal(targets)
		h := sha256.Sum256(b)
		view := filepath.Join(root, ".views", hex.EncodeToString(h[:]))
		if err = os.MkdirAll(view, 0755); err != nil {
			out = append(out, root)
			continue
		}
		for name, target := range targets {
			if err = os.Symlink(target, filepath.Join(view, name)); err != nil && !os.IsExist(err) {
				continue
			}
		}
		out = append(out, view)
	}
	return out
}

// ReconcileLocalSkills detects out-of-band writes in registered skill roots.
// It is a background operation, never a per-chat full file hash scan.
func (s *PublishedStore) ReconcileLocalSkills(ctx context.Context) error {
	s.mu.Lock()
	roots := make(map[string]string, len(s.roots))
	for root, owner := range s.roots {
		roots[root] = owner
	}
	s.mu.Unlock()
	for root, owner := range roots {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rows, err := s.publications(ctx, owner)
		if err != nil {
			return err
		}
		for _, p := range rows {
			if p.Deleted {
				continue
			}
			target := filepath.Join(root, p.Slug)
			sum := sha256.Sum256(p.Manifest)
			expectedVersion := hex.EncodeToString(sum[:])
			s.mu.Lock()
			installed := s.local[target]
			s.mu.Unlock()
			if installed != expectedVersion {
				continue
			} // Remote changed; don't republish an old local tree.
			var m packageManifest
			if err = json.Unmarshal(p.Manifest, &m); err != nil {
				return err
			}
			changed := false
			for _, f := range m.Files {
				b, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(f.Path)))
				if err != nil {
					changed = true
					break
				}
				h := sha256.Sum256(b)
				if hex.EncodeToString(h[:]) != f.Hash {
					changed = true
					break
				}
			}
			// Also detect additions; old file count alone doesn't catch a replacement.
			resolved, err := filepath.EvalSymlinks(target)
			if err == nil {
				count := 0
				_ = filepath.WalkDir(resolved, func(_ string, e fs.DirEntry, err error) error {
					if err == nil && !e.IsDir() {
						count++
					}
					return err
				})
				if count != len(m.Files) {
					changed = true
				}
			}
			if changed {
				if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
					continue
				} // Partial external edit: retry later.
				if err = s.PublishSkill(ctx, owner, p.Slug, root); err != nil {
					return err
				}
				s.mu.Lock()
				delete(s.local, target)
				s.mu.Unlock()
			}
		}
		if err = s.MirrorPublishedSkills(ctx, owner, root); err != nil {
			return err
		}
	}
	return nil
}
