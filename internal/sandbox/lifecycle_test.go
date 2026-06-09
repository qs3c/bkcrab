package sandbox

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/workspace"
)

// fakeExecutor counts Exec calls so tests can prove the sandbox was actually
// invoked (or wasn't). Also records WriteFile targets so hydrate tests can
// check which paths landed inside the sandbox.
type fakeExecutor struct {
	execs   int32
	closed  int32
	agentID string

	mu     sync.Mutex
	writes map[string]string
}

func (f *fakeExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	atomic.AddInt32(&f.execs, 1)
	return "ok", nil
}
func (f *fakeExecutor) ReadFile(ctx context.Context, path string) (string, error) { return "", nil }
func (f *fakeExecutor) WriteFile(ctx context.Context, p, c string) (string, error) {
	f.mu.Lock()
	if f.writes == nil {
		f.writes = map[string]string{}
	}
	f.writes[p] = c
	f.mu.Unlock()
	return "", nil
}
func (f *fakeExecutor) ListDir(ctx context.Context, path string) (string, error) { return "", nil }
func (f *fakeExecutor) Backend() string                                           { return "fake" }
func (f *fakeExecutor) Close() error {
	atomic.AddInt32(&f.closed, 1)
	return nil
}

// fakePool tracks created/released executors so tests can assert lifecycle
// events happened in the right order.
type fakePool struct {
	creates   int32
	releases  int32
	closedAll int32
	live      map[string]*fakeExecutor
}

func newFakePool() *fakePool { return &fakePool{live: map[string]*fakeExecutor{}} }

func (p *fakePool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.live[key]; ok {
		return ex, nil
	}
	atomic.AddInt32(&p.creates, 1)
	ex := &fakeExecutor{agentID: key}
	p.live[key] = ex
	return ex, nil
}

func (p *fakePool) Release(agentID, projectID, sessionID string) error {
	atomic.AddInt32(&p.releases, 1)
	key := poolKey(agentID, projectID, sessionID)
	if ex, ok := p.live[key]; ok {
		delete(p.live, key)
		return ex.Close()
	}
	return nil
}

func (p *fakePool) Backend() string { return "fake" }

func (p *fakePool) CloseAll() {
	atomic.AddInt32(&p.closedAll, 1)
	for id, ex := range p.live {
		ex.Close()
		delete(p.live, id)
	}
}

// TestLifecycle_LazyCreation proves that calling Get on the LifecyclePool
// does NOT hit the inner pool until the first real tool call. This is the
// main cost-saver: an agent that just chats never spawns a sandbox.
func TestLifecycle_LazyCreation(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, 0, 0) // eviction disabled
	lp.Start()
	defer lp.CloseAll()

	ex, err := lp.Get(context.Background(), "alice", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 0 {
		t.Fatalf("expected 0 creates after Get; got %d", got)
	}

	// First tool call should trigger lazy creation.
	if _, err := ex.Exec(context.Background(), "echo hi", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("expected 1 create after Exec; got %d", got)
	}

	// Second call reuses the cached executor.
	if _, err := ex.Exec(context.Background(), "echo bye", time.Second); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("expected still 1 create after second Exec; got %d", got)
	}
}

// TestLifecycle_IdleEviction proves the sweeper Release()s sandboxes after
// they've been unused for IdleTTL.
func TestLifecycle_IdleEviction(t *testing.T) {
	inner := newFakePool()
	// Tight times so the test is fast; sweep twice per TTL window.
	lp := NewLifecyclePool(inner, 50*time.Millisecond, 20*time.Millisecond)
	lp.Start()
	defer lp.CloseAll()

	ex, _ := lp.Get(context.Background(), "bob", "", "")
	ex.Exec(context.Background(), "ls", time.Second)

	if got := atomic.LoadInt32(&inner.creates); got != 1 {
		t.Fatalf("expected 1 create; got %d", got)
	}

	// Wait longer than the TTL — sweeper should release.
	time.Sleep(150 * time.Millisecond)
	if got := atomic.LoadInt32(&inner.releases); got < 1 {
		t.Fatalf("expected release after idle; got %d", got)
	}

	// A new call after eviction recreates.
	ex.Exec(context.Background(), "ls", time.Second)
	if got := atomic.LoadInt32(&inner.creates); got != 2 {
		t.Fatalf("expected 2 creates (first + after eviction); got %d", got)
	}
}

// fakeWorkspace is an in-memory workspace.Store for tests that stashes
// objects under {agent}/{path}. Minimal fields — enough for hydrate to
// iterate and read.
type fakeWorkspace struct {
	mu      sync.Mutex
	objects map[string]map[string][]byte // agentID → path → bytes
}

func newFakeWorkspace() *fakeWorkspace {
	return &fakeWorkspace{objects: map[string]map[string][]byte{}}
}

func (w *fakeWorkspace) put(agentID, path string, data []byte) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.objects[agentID]; !ok {
		w.objects[agentID] = map[string][]byte{}
	}
	w.objects[agentID][path] = data
}

// scopeKey collapses (agent, session) into the per-test in-memory key.
// Empty session keeps the "agent shared" bucket so existing tests that
// don't care about sessions still work without changes.
func wsScopeKey(agentID, sessionID string) string {
	if sessionID == "" {
		return agentID
	}
	return agentID + ":" + sessionID
}

func (w *fakeWorkspace) Put(ctx context.Context, agentID, projectID, sessionID, p string, r io.Reader, _ int64, _ string) error {
	buf, _ := io.ReadAll(r)
	w.put(wsScopeKey(agentID, scopeForKey(projectID, sessionID)), p, buf)
	return nil
}

func (w *fakeWorkspace) Get(ctx context.Context, agentID, projectID, sessionID, p string) (io.ReadCloser, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, ok := w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))][p]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (w *fakeWorkspace) Stat(ctx context.Context, agentID, projectID, sessionID, p string) (*workspace.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	data, ok := w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))][p]
	if !ok {
		return nil, workspace.ErrNotFound
	}
	return &workspace.ObjectInfo{Path: p, Size: int64(len(data))}, nil
}

func (w *fakeWorkspace) List(ctx context.Context, agentID, projectID, sessionID string) ([]workspace.ObjectInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []workspace.ObjectInfo
	for p, data := range w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))] {
		out = append(out, workspace.ObjectInfo{Path: p, Size: int64(len(data))})
	}
	return out, nil
}

func (w *fakeWorkspace) Delete(ctx context.Context, agentID, projectID, sessionID, p string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.objects[wsScopeKey(agentID, scopeForKey(projectID, sessionID))], p)
	return nil
}

func (w *fakeWorkspace) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	srcKey := wsScopeKey(agentID, scopeForKey(fromProjectID, fromSessionID))
	dstKey := wsScopeKey(agentID, scopeForKey(toProjectID, toSessionID))
	if srcKey == dstKey {
		return nil
	}
	if existing, ok := w.objects[dstKey]; ok && len(existing) > 0 {
		return workspace.ErrMoveDestinationExists
	}
	if src, ok := w.objects[srcKey]; ok {
		w.objects[dstKey] = src
		delete(w.objects, srcKey)
	}
	return nil
}

func (w *fakeWorkspace) SignedURL(ctx context.Context, agentID, projectID, sessionID, p string, ttl time.Duration) (string, error) {
	return "", workspace.ErrSignedURLUnsupported
}

// scopeForKey collapses the (project, session) tuple to a single string
// the test fakes can use as a map sub-key. Project wins so all chats in
// one project share a slot, mirroring production scopeDir logic.
func scopeForKey(projectID, sessionID string) string {
	if projectID != "" {
		return "p:" + projectID
	}
	return sessionID
}

// TestLifecycle_HydrateOnCreate proves that the first tool call triggers
// a copy from workspace.Store into the sandbox, and that a second call on
// the same live sandbox does not re-hydrate.
func TestLifecycle_HydrateOnCreate(t *testing.T) {
	inner := newFakePool()
	ws := newFakeWorkspace()
	ws.put("dave", "report.pdf", []byte("pdf-bytes"))
	ws.put("dave", "scripts/gen.py", []byte("print(1)"))

	lp := NewLifecyclePool(inner, 0, 0)
	lp.SetWorkspace(ws)
	lp.Start()
	defer lp.CloseAll()

	ex, _ := lp.Get(context.Background(), "dave", "", "")
	ex.Exec(context.Background(), "ls /workspace", time.Second)

	// Grab the underlying fake executor to inspect writes.
	underlying := inner.live["dave"]
	if underlying == nil {
		t.Fatal("expected inner pool to hold the created executor")
	}
	underlying.mu.Lock()
	writes := len(underlying.writes)
	hasReport := strings.Contains(strings.Join(mapKeys(underlying.writes), ","), "report.pdf")
	hasScript := strings.Contains(strings.Join(mapKeys(underlying.writes), ","), "gen.py")
	underlying.mu.Unlock()

	if writes != 2 {
		t.Fatalf("expected 2 hydrate writes; got %d", writes)
	}
	if !hasReport || !hasScript {
		t.Fatalf("hydrate missed files: writes=%+v", underlying.writes)
	}

	// Second tool call should NOT trigger another hydrate (idempotent
	// during the sandbox's life).
	ex.Exec(context.Background(), "ls", time.Second)
	underlying.mu.Lock()
	writesAfter := len(underlying.writes)
	underlying.mu.Unlock()
	if writesAfter != writes {
		t.Fatalf("second exec triggered re-hydrate; writes grew from %d to %d", writes, writesAfter)
	}
}

func mapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// snapshottingExecutor is a fakeExecutor that also implements
// WorkspaceSnapshotter so we can verify flush-on-evict copies the right
// bytes to the store.
type snapshottingExecutor struct {
	fakeExecutor
	files map[string][]byte
}

func (s *snapshottingExecutor) SnapshotWorkspace(ctx context.Context) (map[string][]byte, error) {
	out := make(map[string][]byte, len(s.files))
	for k, v := range s.files {
		out[k] = v
	}
	return out, nil
}

// fakePool that returns a snapshottingExecutor on first Get, so the
// lifecycle pool sees a backend that supports SnapshotWorkspace.
type snappingPool struct {
	fakePool
	current *snapshottingExecutor
}

func newSnappingPool(files map[string][]byte) *snappingPool {
	return &snappingPool{
		fakePool: *newFakePool(),
		current:  &snapshottingExecutor{files: files},
	}
}

func (p *snappingPool) Get(ctx context.Context, agentID, projectID, sessionID string) (Executor, error) {
	key := poolKey(agentID, projectID, sessionID)
	if _, ok := p.fakePool.live[key]; !ok {
		atomic.AddInt32(&p.fakePool.creates, 1)
		// Lie: stash the underlying fakeExecutor in fakePool.live so
		// Release knows to clean, but return the snapshotter so type-
		// assertion works.
		p.fakePool.live[key] = &p.current.fakeExecutor
	}
	return p.current, nil
}

// TestLifecycle_FlushOnEvict proves that files the sandbox wrote (but
// weren't routed through write_file) get copied into workspace.Store when
// the sandbox is idle-evicted.
func TestLifecycle_FlushOnEvict(t *testing.T) {
	ws := newFakeWorkspace()
	files := map[string][]byte{
		"new_artifact.txt": []byte("hello from exec"),
		"subdir/data.json": []byte(`{"ok":true}`),
	}
	pool := newSnappingPool(files)

	lp := NewLifecyclePool(pool, 40*time.Millisecond, 15*time.Millisecond)
	lp.SetWorkspace(ws)
	lp.Start()
	defer lp.CloseAll()

	ex, _ := lp.Get(context.Background(), "erin", "", "")
	ex.Exec(context.Background(), "python generate.py", time.Second)

	// Wait past idle TTL so the sweeper evicts — flush should fire.
	time.Sleep(150 * time.Millisecond)

	// Both files should now be in the workspace store.
	for path, want := range files {
		got, err := ws.Get(context.Background(), "erin", "", "", path)
		if err != nil {
			t.Fatalf("expected flushed file %q in store: %v", path, err)
		}
		data, _ := io.ReadAll(got)
		got.Close()
		if string(data) != string(want) {
			t.Fatalf("flushed %q content mismatch: got %q want %q", path, data, want)
		}
	}
}

// TestLifecycle_CloseAll stops the sweeper and tears down everything.
// Important on pod shutdown so we don't leak billable sandboxes.
func TestLifecycle_CloseAll(t *testing.T) {
	inner := newFakePool()
	lp := NewLifecyclePool(inner, time.Second, 50*time.Millisecond)
	lp.Start()

	ex, _ := lp.Get(context.Background(), "carol", "", "")
	ex.Exec(context.Background(), "echo", time.Second)

	lp.CloseAll()

	if got := atomic.LoadInt32(&inner.closedAll); got != 1 {
		t.Fatalf("expected inner CloseAll to be called once; got %d", got)
	}

	// Safe to call twice.
	lp.CloseAll()
}
