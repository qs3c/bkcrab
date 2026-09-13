package agent

import "context"

// promptMemorySnapshot is private to one turn. Sharing it across the builder
// and reminder avoids rereading USER.md/MEMORY.md even with Redis disabled.
type promptMemorySnapshot struct {
	MemoryStore
	files map[[3]string][]byte
	errs  map[[3]string]error
}

func (s *promptMemorySnapshot) read(ctx context.Context, a, u, f string, exact bool) ([]byte, error) {
	mode := "overlay:"
	if exact {
		mode = "exact:"
	}
	key := [3]string{a, u, mode + f}
	if b, ok := s.files[key]; ok {
		return append([]byte(nil), b...), s.errs[key]
	}
	var b []byte
	var err error
	if exact {
		b, err = s.MemoryStore.GetWorkspaceFileExact(ctx, a, u, f)
	} else {
		b, err = s.MemoryStore.GetWorkspaceFile(ctx, a, u, f)
	}
	s.files[key] = append([]byte(nil), b...)
	s.errs[key] = err
	return b, err
}
func (s *promptMemorySnapshot) GetWorkspaceFile(ctx context.Context, a, u, f string) ([]byte, error) {
	return s.read(ctx, a, u, f, false)
}
func (s *promptMemorySnapshot) GetWorkspaceFileExact(ctx context.Context, a, u, f string) ([]byte, error) {
	return s.read(ctx, a, u, f, true)
}
func (s *promptMemorySnapshot) GetMemory(ctx context.Context, a, u string) (string, error) {
	key := [3]string{a, u, "exact:MEMORY.md"}
	if b, ok := s.files[key]; ok {
		return string(b), s.errs[key]
	}
	v, err := s.MemoryStore.GetMemory(ctx, a, u)
	s.files[key] = []byte(v)
	s.errs[key] = err
	return v, err
}

func (a *Agent) promptInputs(uid string) (*ContextBuilder, *Memory) {
	cb := *a.ctxBuilder
	mem := a.memory.WithUserID(uid)
	if cb.store != nil && mem != nil && mem.store != nil {
		snap := &promptMemorySnapshot{MemoryStore: mem.store, files: make(map[[3]string][]byte), errs: make(map[[3]string]error)}
		cb.store = snap
		mem.store = snap
	}
	return &cb, mem
}
