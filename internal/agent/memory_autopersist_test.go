package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/memory"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
)

type fakeMemStore struct {
	files map[string][]byte
}

func newFakeMemStore() *fakeMemStore {
	return &fakeMemStore{files: map[string][]byte{}}
}

func (f *fakeMemStore) key(agentID, userID, filename string) string {
	return agentID + "|" + userID + "|" + filename
}

func (f *fakeMemStore) GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if data, ok := f.files[f.key(agentID, userID, filename)]; ok {
		return append([]byte(nil), data...), nil
	}
	return nil, nil
}

func (f *fakeMemStore) MutateWorkspaceFile(ctx context.Context, agentID, userID, filename string, fn memory.Mutator) ([]byte, error) {
	k := f.key(agentID, userID, filename)
	cur, exists := f.files[k]
	next, del, err := fn(append([]byte(nil), cur...), exists)
	if err != nil {
		return append([]byte(nil), cur...), err
	}
	if del {
		delete(f.files, k)
		return nil, nil
	}
	f.files[k] = append([]byte(nil), next...)
	return append([]byte(nil), next...), nil
}

type fakeExtractProvider struct {
	resp         string
	gotMaxTokens int
	gotPrompt    string
}

func (f *fakeExtractProvider) Chat(ctx context.Context, msgs []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	f.gotMaxTokens = maxTokens
	if len(msgs) > 0 {
		f.gotPrompt = msgs[0].Content
	}
	return &provider.Response{Content: f.resp}, nil
}

func (f *fakeExtractProvider) ChatStream(ctx context.Context, msgs []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.StreamReader, error) {
	return nil, nil
}

func newTestManager(fs *fakeMemStore) *memory.Manager {
	return memory.NewManager(memory.Options{
		Store:   fs,
		AgentID: "a",
		UserID:  "u",
		Config:  memory.DefaultConfig(),
	})
}

func TestAutoPersistMemoryAppliesOps(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	if res := mgr.Apply(context.Background(), memory.TargetMemory, []memory.Operation{
		{Action: memory.ActionAdd, Content: "old fact"},
		{Action: memory.ActionAdd, Content: "obsolete fact"},
	}); !res.Success {
		t.Fatalf("seed failed: %+v", res)
	}

	prov := &fakeExtractProvider{resp: `{"memory_ops":[{"action":"replace","old_text":"old fact","content":"new fact"},{"action":"add","content":"second fact"},{"action":"remove","old_text":"obsolete fact"}],"user_ops":[{"action":"add","content":"name is Ada"}]}`}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "call me Ada"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "test-model", groups); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}

	if prov.gotMaxTokens != 2048 {
		t.Fatalf("maxTokens = %d, want 2048", prov.gotMaxTokens)
	}
	memEntries := mgr.List(context.Background(), memory.TargetMemory).Entries
	if strings.Join(memEntries, "|") != "new fact|second fact" {
		t.Fatalf("memory entries = %#v", memEntries)
	}
	userEntries := mgr.List(context.Background(), memory.TargetUser).Entries
	if strings.Join(userEntries, "|") != "name is Ada" {
		t.Fatalf("user entries = %#v", userEntries)
	}
}

func TestAutoPersistMemoryStripsJSONFence(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: "```json\n{\"memory_ops\":[{\"action\":\"add\",\"content\":\"fenced fact\"}],\"user_ops\":[]}\n```"}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "test-model", groups); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if strings.Join(mgr.List(context.Background(), memory.TargetMemory).Entries, "|") != "fenced fact" {
		t.Fatalf("fenced JSON not parsed")
	}
}

func TestAutoPersistMemoryEmptyOpsNoWrite(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "test-model", groups); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if len(fs.files) != 0 {
		t.Fatalf("empty ops should not write; files = %v", fs.files)
	}
}

func TestAutoPersistMemoryCompactsOverLimitBatch(t *testing.T) {
	fs := newFakeMemStore()
	mgr := memory.NewManager(memory.Options{
		Store:   fs,
		AgentID: "a",
		UserID:  "u",
		Config:  memory.Config{Enabled: true, MemoryCharLimit: 200, UserCharLimit: 200},
	})
	if res := mgr.Apply(context.Background(), memory.TargetMemory, []memory.Operation{
		{Action: memory.ActionAdd, Content: "alpha fact one with a bit of padding text"},
		{Action: memory.ActionAdd, Content: "beta fact two"},
	}); !res.Success {
		t.Fatalf("seed failed: %+v", res)
	}

	// The model returns a size-reducing replace plus an oversized add. The full
	// batch blows the 200-char limit, so the engine rejects it. Best-effort must
	// still land the shrinking replace — it is judged by its own size effect, NOT
	// lumped in with the add — and drop only the add that genuinely doesn't fit.
	bigAdd := strings.Repeat("z", 300)
	resp := `{"memory_ops":[{"action":"replace","old_text":"alpha fact one with a bit of padding text","content":"alpha short"},{"action":"add","content":"` + bigAdd + `"}],"user_ops":[]}`
	prov := &fakeExtractProvider{resp: resp}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "tidy up"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "test-model", groups); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}

	entries := mgr.List(context.Background(), memory.TargetMemory).Entries
	if strings.Join(entries, "|") != "alpha short|beta fact two" {
		t.Fatalf("entries = %#v, want [alpha short, beta fact two] (shrinking replace landed, oversized add dropped)", entries)
	}
}

func TestAutoPersistMemoryInjectsCompactionPressureNearLimit(t *testing.T) {
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	// Well under the limit: no pressure note.
	low := newFakeMemStore()
	lowMgr := memory.NewManager(memory.Options{
		Store: low, AgentID: "a", UserID: "u",
		Config: memory.Config{Enabled: true, MemoryCharLimit: 12000, UserCharLimit: 4000},
	})
	if res := lowMgr.Apply(context.Background(), memory.TargetMemory, []memory.Operation{
		{Action: memory.ActionAdd, Content: "small fact"},
	}); !res.Success {
		t.Fatalf("seed low: %+v", res)
	}
	lowProv := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	if err := AutoPersistMemory(context.Background(), lowMgr, lowProv, "m", groups); err != nil {
		t.Fatalf("AutoPersistMemory low: %v", err)
	}
	if strings.Contains(lowProv.gotPrompt, "SPACE PRESSURE") {
		t.Fatalf("did not expect pressure note under threshold:\n%s", lowProv.gotPrompt)
	}

	// At/over 80% of the limit: pressure note injected.
	high := newFakeMemStore()
	highMgr := memory.NewManager(memory.Options{
		Store: high, AgentID: "a", UserID: "u",
		Config: memory.Config{Enabled: true, MemoryCharLimit: 120, UserCharLimit: 4000},
	})
	if res := highMgr.Apply(context.Background(), memory.TargetMemory, []memory.Operation{
		{Action: memory.ActionAdd, Content: strings.Repeat("a", 70)},
	}); !res.Success {
		t.Fatalf("seed high: %+v", res)
	}
	highProv := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	if err := AutoPersistMemory(context.Background(), highMgr, highProv, "m", groups); err != nil {
		t.Fatalf("AutoPersistMemory high: %v", err)
	}
	if !strings.Contains(highProv.gotPrompt, "SPACE PRESSURE") {
		t.Fatalf("expected pressure note at/over threshold:\n%s", highProv.gotPrompt)
	}
}

func TestAutoPersistMemoryParseFailureReturnsError(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: "not json at all"}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "test-model", groups); err == nil {
		t.Fatalf("expected parse error to be returned")
	}
}
