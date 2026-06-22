package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/memory"
)

func TestMemoryToolAddReplaceRemove(t *testing.T) {
	r, store := newMemoryToolTestRegistry(t)

	add := executeMemoryTool(t, r, map[string]any{
		"target":  "memory",
		"action":  "add",
		"content": "release checklist lives in docs/release.md",
	})
	if !add.Success || add.EntryCount != 1 {
		t.Fatalf("add result = %+v, want success with 1 entry", add)
	}
	if got := string(store.file("agent-1", "owner-user", "MEMORY.md")); !strings.Contains(got, "release checklist lives in docs/release.md") {
		t.Fatalf("stored MEMORY.md = %q, want added entry", got)
	}

	replaced := executeMemoryTool(t, r, map[string]any{
		"target":   "memory",
		"action":   "replace",
		"old_text": "release checklist",
		"content":  "release checklist is owned by platform",
	})
	if !replaced.Success {
		t.Fatalf("replace failed: %+v", replaced)
	}
	if got := string(store.file("agent-1", "owner-user", "MEMORY.md")); !strings.Contains(got, "owned by platform") {
		t.Fatalf("stored MEMORY.md after replace = %q", got)
	}

	removed := executeMemoryTool(t, r, map[string]any{
		"target":   "memory",
		"action":   "remove",
		"old_text": "platform",
	})
	if !removed.Success {
		t.Fatalf("remove failed: %+v", removed)
	}
	if got := string(store.file("agent-1", "owner-user", "MEMORY.md")); strings.Contains(got, "platform") {
		t.Fatalf("stored MEMORY.md after remove = %q, want entry gone", got)
	}
}

func TestMemoryToolBatchIsAtomic(t *testing.T) {
	r, store := newMemoryToolTestRegistry(t)
	executeMemoryTool(t, r, map[string]any{
		"target":  "memory",
		"action":  "add",
		"content": "alpha",
	})

	result := executeMemoryTool(t, r, map[string]any{
		"target": "memory",
		"operations": []map[string]any{
			{"action": "add", "content": "beta"},
			{"action": "remove", "old_text": "missing"},
		},
	})
	if result.Success {
		t.Fatalf("batch result = %+v, want failure", result)
	}
	if !strings.Contains(result.Message, "matches no entries") {
		t.Fatalf("batch message = %q, want no-match explanation", result.Message)
	}

	got := string(store.file("agent-1", "owner-user", "MEMORY.md"))
	if !strings.Contains(got, "alpha") || strings.Contains(got, "beta") {
		t.Fatalf("batch was not atomic; stored = %q", got)
	}
	if store.writeCount != 1 {
		t.Fatalf("writeCount = %d, want only the initial add", store.writeCount)
	}
}

func TestMemoryToolSuccessDoesNotEchoEntries(t *testing.T) {
	r, _ := newMemoryToolTestRegistry(t)

	_, raw := executeMemoryToolRaw(t, r, map[string]any{
		"target":  "memory",
		"action":  "add",
		"content": "private deployment note",
	})
	assertMemoryMutationDidNotEcho(t, raw, "private deployment note")

	_, raw = executeMemoryToolRaw(t, r, map[string]any{
		"target":   "memory",
		"action":   "replace",
		"old_text": "deployment",
		"content":  "private launch note",
	})
	assertMemoryMutationDidNotEcho(t, raw, "private launch note")

	_, raw = executeMemoryToolRaw(t, r, map[string]any{
		"target":   "memory",
		"action":   "remove",
		"old_text": "launch",
	})
	assertMemoryMutationDidNotEcho(t, raw, "private launch note")
}

func TestMemoryToolWritesChatterScopedFile(t *testing.T) {
	r, store := newMemoryToolTestRegistry(t)
	r.SetChatterUserID("chatter-user")

	userResult := executeMemoryTool(t, r, map[string]any{
		"target":  "user",
		"action":  "add",
		"content": "name is Ada",
	})
	if !userResult.Success {
		t.Fatalf("user add failed: %+v", userResult)
	}
	if got := string(store.file("agent-1", "chatter-user", "USER.md")); !strings.Contains(got, "name is Ada") {
		t.Fatalf("chatter USER.md = %q, want added note", got)
	}
	if got := store.file("agent-1", "owner-user", "USER.md"); got != nil {
		t.Fatalf("owner USER.md was written: %q", string(got))
	}

	memoryResult := executeMemoryTool(t, r, map[string]any{
		"target":  "memory",
		"action":  "add",
		"content": "likes concise summaries",
	})
	if !memoryResult.Success {
		t.Fatalf("memory add failed: %+v", memoryResult)
	}
	if got := string(store.file("agent-1", "chatter-user", "MEMORY.md")); !strings.Contains(got, "likes concise summaries") {
		t.Fatalf("chatter MEMORY.md = %q, want added note", got)
	}
	if got := store.file("agent-1", "owner-user", "MEMORY.md"); got != nil {
		t.Fatalf("owner MEMORY.md was written: %q", string(got))
	}
}

func TestMemoryToolInvalidTargetReturnsError(t *testing.T) {
	r, _ := newMemoryToolTestRegistry(t)

	_, err := r.Execute(context.Background(), "memory", `{"target":"profile","action":"add"}`)
	if err == nil {
		t.Fatalf("expected invalid target error")
	}
	if !strings.Contains(err.Error(), "target") {
		t.Fatalf("error = %v, want target explanation", err)
	}
}

func TestMemoryToolUsesFilesystemFallbackWithoutStore(t *testing.T) {
	systemRoot := t.TempDir()
	r := NewRegistry(systemRoot, t.TempDir())
	r.SetOwnerUserID("owner-user")

	result := executeMemoryTool(t, r, map[string]any{
		"target":  "memory",
		"action":  "add",
		"content": "fallback works",
	})
	if !result.Success {
		t.Fatalf("fallback add failed: %+v", result)
	}

	data, err := os.ReadFile(filepath.Join(systemRoot, "MEMORY.md"))
	if err != nil {
		t.Fatalf("read fallback file: %v", err)
	}
	if !strings.Contains(string(data), "fallback works") {
		t.Fatalf("fallback file = %q, want added entry", string(data))
	}
}

func TestMemoryToolRejectsListAction(t *testing.T) {
	r, _ := newMemoryToolTestRegistry(t)
	_, err := r.Execute(context.Background(), "memory", `{"target":"memory","action":"list"}`)
	if err == nil {
		t.Fatalf("expected error rejecting list action")
	}
	if !strings.Contains(err.Error(), "add, replace, or remove") {
		t.Fatalf("error = %v, want action guidance", err)
	}
}

func TestMemoryToolRejectsEmptyAction(t *testing.T) {
	r, _ := newMemoryToolTestRegistry(t)
	_, err := r.Execute(context.Background(), "memory", `{"target":"memory"}`)
	if err == nil {
		t.Fatalf("expected error rejecting empty action")
	}
}

func TestMemoryToolManagedConfigPreservesDisabledAndDefaultsLimits(t *testing.T) {
	r, _ := newMemoryToolTestRegistry(t)

	r.SetManagedMemoryConfig(memory.Config{Enabled: false})
	if r.managedMemoryCfg.Enabled {
		t.Fatalf("managed memory config should preserve Enabled=false")
	}
	defaults := memory.DefaultConfig()
	if r.managedMemoryCfg.UserCharLimit != defaults.UserCharLimit {
		t.Fatalf("UserCharLimit = %d, want %d", r.managedMemoryCfg.UserCharLimit, defaults.UserCharLimit)
	}
	if r.managedMemoryCfg.MemoryCharLimit != defaults.MemoryCharLimit {
		t.Fatalf("MemoryCharLimit = %d, want %d", r.managedMemoryCfg.MemoryCharLimit, defaults.MemoryCharLimit)
	}

	result := executeMemoryTool(t, r, map[string]any{
		"target":  "memory",
		"action":  "add",
		"content": "should not be stored",
	})
	if result.Success {
		t.Fatalf("disabled memory add result = %+v, want failure", result)
	}
	if !strings.Contains(result.Message, "disabled") {
		t.Fatalf("message = %q, want disabled explanation", result.Message)
	}
}

func TestMemoryToolRegisteredBuiltin(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())

	if !r.HasBuiltin("memory") {
		t.Fatalf("memory builtin is not registered")
	}
}

func TestMemoryToolDefinitionsForChatbotStyleAllowlist(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())

	defs := r.DefinitionsForMode([]string{"memory"})
	names := map[string]bool{}
	for _, def := range defs {
		names[def.Function.Name] = true
	}
	if !names["memory"] {
		t.Fatalf("memory missing from chatbot-style definitions: %#v", names)
	}
	if names["write_file"] {
		t.Fatalf("write_file exposed in chatbot-style definitions: %#v", names)
	}
	if names["edit_file"] {
		t.Fatalf("edit_file exposed in chatbot-style definitions: %#v", names)
	}
}

func newMemoryToolTestRegistry(t *testing.T) (*Registry, *fakeSystemFileStore) {
	t.Helper()
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetOwnerUserID("owner-user")
	store := newFakeSystemFileStore()
	r.SetSystemFileStore(store, "agent-1")
	return r, store
}

func executeMemoryTool(t *testing.T, r *Registry, args map[string]any) memory.Result {
	t.Helper()
	result, _ := executeMemoryToolRaw(t, r, args)
	return result
}

func executeMemoryToolRaw(t *testing.T, r *Registry, args map[string]any) (memory.Result, string) {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	raw, err := r.Execute(context.Background(), "memory", string(rawArgs))
	if err != nil {
		t.Fatalf("memory execute error: %v\noutput:\n%s", err, raw)
	}
	var result memory.Result
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("unmarshal memory result: %v\nraw:\n%s", err, raw)
	}
	return result, raw
}

func assertMemoryMutationDidNotEcho(t *testing.T, raw, forbidden string) {
	t.Helper()
	if strings.Contains(raw, `"entries"`) {
		t.Fatalf("mutation success echoed entries:\n%s", raw)
	}
	if strings.Contains(raw, forbidden) {
		t.Fatalf("mutation success echoed entry %q:\n%s", forbidden, raw)
	}
}

type fakeSystemFileStore struct {
	files      map[string][]byte
	writeCount int
}

func newFakeSystemFileStore() *fakeSystemFileStore {
	return &fakeSystemFileStore{files: map[string][]byte{}}
}

func (f *fakeSystemFileStore) GetWorkspaceFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if data := f.file(agentID, userID, filename); data != nil {
		return data, nil
	}
	if data := f.file(agentID, "owner-user", filename); data != nil {
		return data, nil
	}
	return nil, nil
}

func (f *fakeSystemFileStore) GetWorkspaceFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	return f.file(agentID, userID, filename), nil
}

func (f *fakeSystemFileStore) SaveWorkspaceFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	f.files[f.key(agentID, userID, filename)] = append([]byte(nil), data...)
	f.writeCount++
	return nil
}

func (f *fakeSystemFileStore) MutateWorkspaceFile(ctx context.Context, agentID, userID, filename string, fn memory.Mutator) ([]byte, error) {
	key := f.key(agentID, userID, filename)
	current, exists := f.files[key]
	next, deleteFile, err := fn(append([]byte(nil), current...), exists)
	if err != nil {
		return append([]byte(nil), current...), err
	}
	if deleteFile {
		if exists {
			delete(f.files, key)
			f.writeCount++
		}
		return nil, nil
	}
	if !bytes.Equal(current, next) {
		f.files[key] = append([]byte(nil), next...)
		f.writeCount++
	}
	return append([]byte(nil), next...), nil
}

func (f *fakeSystemFileStore) file(agentID, userID, filename string) []byte {
	data, ok := f.files[f.key(agentID, userID, filename)]
	if !ok {
		return nil
	}
	return append([]byte(nil), data...)
}

func (f *fakeSystemFileStore) key(agentID, userID, filename string) string {
	return agentID + "|" + userID + "|" + filename
}
