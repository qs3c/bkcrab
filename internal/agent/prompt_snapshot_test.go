package agent

import (
	"context"
	"testing"
)

type countedPromptStore struct {
	MemoryStore
	reads map[string]int
}

func (s *countedPromptStore) GetWorkspaceFileExact(ctx context.Context, a, u, f string) ([]byte, error) {
	s.reads[f]++
	return s.MemoryStore.GetWorkspaceFileExact(ctx, a, u, f)
}
func (s *countedPromptStore) GetMemory(ctx context.Context, a, u string) (string, error) {
	s.reads["MEMORY.md"]++
	return s.MemoryStore.GetMemory(ctx, a, u)
}
func TestPromptSnapshotReusesExactFilesWithinTurn(t *testing.T) {
	base := &fakeMemoryStore{files: map[string][]byte{}}
	counter := &countedPromptStore{MemoryStore: base, reads: map[string]int{}}
	snapshot := &promptMemorySnapshot{MemoryStore: counter, files: map[[3]string][]byte{}, errs: map[[3]string]error{}}
	ctx := context.Background()
	snapshot.GetWorkspaceFileExact(ctx, "a", "u", "USER.md")
	snapshot.GetWorkspaceFileExact(ctx, "a", "u", "USER.md")
	snapshot.GetMemory(ctx, "a", "u")
	snapshot.GetMemory(ctx, "a", "u")
	if counter.reads["USER.md"] != 1 || counter.reads["MEMORY.md"] != 1 {
		t.Fatal(counter.reads)
	}
	snapshot.GetWorkspaceFileExact(ctx, "a", "visitor", "USER.md")
	if counter.reads["USER.md"] != 2 {
		t.Fatal("users shared snapshot entry")
	}
}
