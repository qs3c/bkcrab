package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestDockerStreamSameSizeAndRestore(t *testing.T) {
	dir := t.TempDir()
	d := &DockerExecutor{sb: NewDockerSandbox("", dir, nil)}
	ws := newFakeWorkspace()
	ctx := context.Background()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.SyncWorkspace(ctx, ws, "a", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := d.SyncWorkspace(ctx, ws, "a", "", ""); err != nil {
		t.Fatal(err)
	}
	r, _ := ws.Get(ctx, "a", "", "", "file")
	data, _ := io.ReadAll(r)
	r.Close()
	if string(data) != "two" {
		t.Fatalf("same-size change lost: %q", data)
	}
	ws.put("a", "file", []byte("old"))
	ws.put("a", "missing", []byte("restore"))
	if err := d.HydrateWorkspace(ctx, ws, "a", "", ""); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "two" {
		t.Fatal("stale remote overwrote local")
	}
	data, _ = os.ReadFile(filepath.Join(dir, "missing"))
	if string(data) != "restore" {
		t.Fatal("missing file not restored")
	}
}
func TestDockerStreamIncludesGitAndSkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("secret"), 0600)
	os.Mkdir(filepath.Join(dir, ".git"), 0700)
	os.WriteFile(filepath.Join(dir, ".git", "config"), []byte("git"), 0600)
	linkErr := os.Symlink(outside, filepath.Join(dir, "link"))
	d := &DockerExecutor{sb: NewDockerSandbox("", dir, nil)}
	ws := newFakeWorkspace()
	if err := d.SyncWorkspace(context.Background(), ws, "a", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.Stat(context.Background(), "a", "", "", ".git/config"); err != nil {
		t.Fatal(err)
	}
	if linkErr == nil {
		if _, err := ws.Stat(context.Background(), "a", "", "", "link"); err == nil {
			t.Fatal("symlink leaked")
		}
	}
}

// Discarding the upload lets a large file prove bounded reader chunking without
// the test store itself retaining the payload.
type streamingStore struct {
	*fakeWorkspace
	bytes   int64
	maxRead int
}

func (s *streamingStore) Put(ctx context.Context, a, p, id, name string, r io.Reader, size int64, mime string) error {
	buf := make([]byte, 32768)
	for {
		n, err := r.Read(buf)
		s.bytes += int64(n)
		if n > s.maxRead {
			s.maxRead = n
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
func TestDockerLargeFileStreams(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "large"))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(32 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	s := &streamingStore{fakeWorkspace: newFakeWorkspace()}
	d := &DockerExecutor{sb: NewDockerSandbox("", dir, nil)}
	if err := d.SyncWorkspace(context.Background(), s, "a", "", ""); err != nil {
		t.Fatal(err)
	}
	if s.bytes != 32<<20 || s.maxRead > 32768 {
		t.Fatalf("unexpected transfer %d/%d", s.bytes, s.maxRead)
	}
}
