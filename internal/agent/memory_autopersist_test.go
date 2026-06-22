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
}

func (f *fakeExtractProvider) Chat(ctx context.Context, msgs []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	f.gotMaxTokens = maxTokens
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
	if res := mgr.Apply(context.Background(), memory.TargetMemory, []memory.Operation{{Action: memory.ActionAdd, Content: "old fact"}}); !res.Success {
		t.Fatalf("seed failed: %+v", res)
	}

	prov := &fakeExtractProvider{resp: `{"memory_ops":[{"action":"replace","old_text":"old fact","content":"new fact"},{"action":"add","content":"second fact"}],"user_ops":[{"action":"add","content":"name is Ada"}]}`}
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

func TestAutoPersistMemoryParseFailureReturnsError(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: "not json at all"}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "test-model", groups); err == nil {
		t.Fatalf("expected parse error to be returned")
	}
}
