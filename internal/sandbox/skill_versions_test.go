package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestPublishedSkillArchives(t *testing.T) {
	root := t.TempDir()
	version := filepath.Join(root, ".versions", "demo", "abc")
	if err := os.MkdirAll(version, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, "SKILL.md"), []byte("pinned"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(version, filepath.Join(root, "demo")); err != nil {
		t.Fatal(err)
	}
	if got, ok := skillDirectory(root, "demo"); !ok || got != version {
		t.Fatalf("resolved %q %v", got, ok)
	}
	if _, ok := skillDirectory(root, ".versions"); ok {
		t.Fatal("internal versions exposed as skill")
	}
	read := func(r io.Reader) {
		t.Helper()
		tr := tar.NewReader(r)
		h, err := tr.Next()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if h.Name != "skills/demo/SKILL.md" || string(b) != "pinned" {
			t.Fatalf("bad archive %s: %s", h.Name, b)
		}
	}
	b := newTarBundle()
	if n, err := b.addLocalDir(filepath.Join(root, "demo"), "skills/demo"); err != nil || n != 1 {
		t.Fatalf("gzip bundle %d %v", n, err)
	}
	if err := b.close(); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&b.gz)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	read(gz)
	p := newPlainTarBundle()
	if n, err := p.addLocalDir(filepath.Join(root, "demo"), "skills/demo"); err != nil || n != 1 {
		t.Fatalf("plain bundle %d %v", n, err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	read(&p.buf)
}

type skillContextPool struct {
	*fakePool
	seen []string
}

func (p *skillContextPool) Get(ctx context.Context, a, project, session string) (Executor, error) {
	p.seen = requestedSkillDirs(ctx, nil)
	return p.fakePool.Get(ctx, a, project, session)
}
func TestPublishedSkillLazyContext(t *testing.T) {
	inner := &skillContextPool{fakePool: newFakePool()}
	pool := NewLifecyclePool(inner, 0, 0)
	pool.Start()
	defer pool.CloseAll()
	dirs := []string{"/fixed/view1"}
	ctx := WithSkillDirs(context.Background(), dirs)
	dirs[0] = "/mutated"
	ex, err := pool.Get(ctx, "a", "", "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ex.Exec(context.Background(), "true", time.Second); err != nil {
		t.Fatal(err)
	}
	if len(inner.seen) != 1 || inner.seen[0] != "/fixed/view1" {
		t.Fatalf("lost turn view: %v", inner.seen)
	}
	if skillViewChanged(context.Background(), inner.seen) {
		t.Fatal("background access must not switch a live view")
	}
	if !skillViewChanged(WithSkillDirs(context.Background(), nil), inner.seen) {
		t.Fatal("deleted final skill must change view")
	}
}

// Opt-in, isolated containers only. The caller supplies an already-local image.
func TestPublishedSkillDockerSwitch(t *testing.T) {
	image := os.Getenv("BKCRAB_CACHE_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set BKCRAB_CACHE_TEST_DOCKER_IMAGE for Docker integration")
	}
	root := t.TempDir()
	makeView := func(name, content string) string {
		v := filepath.Join(root, name)
		target := filepath.Join(root, "versions", name)
		if err := os.MkdirAll(target, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(v, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "SKILL.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(v, "demo")); err != nil {
			t.Fatal(err)
		}
		return v
	}
	old, next := makeView("v1", "old"), makeView("v2", "new")
	pool := NewDockerExecutorPool(image, root, &Policy{NetMode: "none"})
	defer pool.CloseAll()
	ctx1 := WithSkillDirs(context.Background(), []string{old})
	ex1, err := pool.Get(ctx1, "test", "", "session")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := ex1.(*DockerExecutor).sb.Exec(ctx1, "cat /skills/demo/SKILL.md; echo saved > /workspace/keep", "/workspace"); err != nil || out != "old" {
		t.Fatalf("first exec %q %v", out, err)
	}
	again, err := pool.Get(ctx1, "test", "", "session")
	if err != nil || again != ex1 {
		t.Fatal("same view recreated sandbox")
	}
	ctx2 := WithSkillDirs(context.Background(), []string{next})
	ex2, err := pool.Get(ctx2, "test", "", "session")
	if err != nil {
		t.Fatal(err)
	}
	if ex2 == ex1 {
		t.Fatal("new version reused old mounts")
	}
	if out, err := ex2.(*DockerExecutor).sb.Exec(ctx2, "cat /skills/demo/SKILL.md; cat /workspace/keep", "/workspace"); err != nil || out != "newsaved\n" {
		t.Fatalf("second exec %q %v", out, err)
	}
}

type skillSwitchRemote struct {
	*fakeExecutor
	files       map[string][]byte
	snapshotErr error
}

func (s *skillSwitchRemote) IsRemoteWorkspace() {}
func (s *skillSwitchRemote) SnapshotWorkspace(context.Context) (map[string][]byte, error) {
	return s.files, s.snapshotErr
}
func TestPublishedSkillSwitchPreservesSameLength(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewLocalFS(t.TempDir())
	if err := ws.Put(ctx, "a", "", "s", "keep", strings.NewReader("old"), 3, ""); err != nil {
		t.Fatal(err)
	}
	ex := &skillSwitchRemote{fakeExecutor: &fakeExecutor{}, files: map[string][]byte{"keep": []byte("new")}}
	if err := preserveSkillSwitchWorkspace(ctx, ex, ws, "a", "", "s"); err != nil {
		t.Fatal(err)
	}
	r, err := ws.Get(ctx, "a", "", "s", "keep")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "new" {
		t.Fatalf("same-length update lost: %s %v", b, err)
	}
	ex.snapshotErr = errors.New("snapshot unavailable")
	if err := preserveSkillSwitchWorkspace(ctx, ex, ws, "a", "", "s"); err == nil {
		t.Fatal("snapshot failure allowed switch")
	}
	ex.snapshotErr = nil
	if err := preserveSkillSwitchWorkspace(ctx, ex, nil, "a", "", "s"); err == nil {
		t.Fatal("missing durable store allowed switch")
	}
}
