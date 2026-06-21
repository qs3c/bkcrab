package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMutateAgentFileCreatesUpdatesAndDeletes(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	got, err := db.MutateAgentFile(ctx, "agent-mut", "user-mut", "MEMORY.md",
		func(current []byte, exists bool) ([]byte, bool, error) {
			if exists {
				t.Fatal("fresh file unexpectedly exists")
			}
			if len(current) != 0 {
				t.Fatalf("fresh file current bytes = %q, want empty", current)
			}
			return []byte("first"), false, nil
		})
	if err != nil {
		t.Fatalf("create mutate: %v", err)
	}
	if string(got) != "first" {
		t.Fatalf("create returned %q, want first", got)
	}

	got[0] = 'X'
	stored, err := db.GetAgentFileExact(ctx, "agent-mut", "user-mut", "MEMORY.md")
	if err != nil {
		t.Fatalf("read after mutating returned bytes: %v", err)
	}
	if string(stored) != "first" {
		t.Fatalf("stored bytes changed through returned slice: got %q", stored)
	}

	got, err = db.MutateAgentFile(ctx, "agent-mut", "user-mut", "MEMORY.md",
		func(current []byte, exists bool) ([]byte, bool, error) {
			if !exists {
				t.Fatal("existing file reported missing")
			}
			if string(current) != "first" {
				t.Fatalf("update saw %q, want first", current)
			}
			return append(current, []byte("-second")...), false, nil
		})
	if err != nil {
		t.Fatalf("update mutate: %v", err)
	}
	if string(got) != "first-second" {
		t.Fatalf("update returned %q, want first-second", got)
	}

	got, err = db.MutateAgentFile(ctx, "agent-mut", "user-mut", "MEMORY.md",
		func(current []byte, exists bool) ([]byte, bool, error) {
			if !exists {
				t.Fatal("delete saw missing file")
			}
			if string(current) != "first-second" {
				t.Fatalf("delete saw %q, want first-second", current)
			}
			return nil, true, nil
		})
	if err != nil {
		t.Fatalf("delete mutate: %v", err)
	}
	if got != nil {
		t.Fatalf("delete returned %q, want nil", got)
	}
	if _, err := db.GetAgentFileExact(ctx, "agent-mut", "user-mut", "MEMORY.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected deleted file to be missing, got %v", err)
	}
}

func TestMutateAgentFileRollsBackOnError(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	if err := db.SaveAgentFile(ctx, "agent-rollback", "user-rollback", "MEMORY.md", []byte("original")); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	mutateErr := errors.New("mutator failed")
	got, err := db.MutateAgentFile(ctx, "agent-rollback", "user-rollback", "MEMORY.md",
		func(current []byte, exists bool) ([]byte, bool, error) {
			if !exists {
				t.Fatal("seeded file reported missing")
			}
			current[0] = 'X'
			return []byte("changed"), false, mutateErr
		})
	if !errors.Is(err, mutateErr) {
		t.Fatalf("MutateAgentFile error = %v, want %v", err, mutateErr)
	}
	if string(got) != "original" {
		t.Fatalf("rollback returned %q, want original", got)
	}

	stored, err := db.GetAgentFileExact(ctx, "agent-rollback", "user-rollback", "MEMORY.md")
	if err != nil {
		t.Fatalf("read after rollback: %v", err)
	}
	if string(stored) != "original" {
		t.Fatalf("rollback stored %q, want original", stored)
	}
}

func TestMutateAgentFileConcurrentAppendsDoNotDropEntries(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	const writers = 40
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := db.MutateAgentFile(ctx, "agent-concurrent", "user-concurrent", "MEMORY.md",
				func(current []byte, exists bool) ([]byte, bool, error) {
					line := fmt.Sprintf("entry-%02d\n", i)
					if !exists && len(current) != 0 {
						return nil, false, fmt.Errorf("missing file had current bytes %q", current)
					}
					return append(current, []byte(line)...), false, nil
				})
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent mutate: %v", err)
		}
	}

	got, err := db.GetAgentFileExact(ctx, "agent-concurrent", "user-concurrent", "MEMORY.md")
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != writers {
		t.Fatalf("final file has %d entries, want %d:\n%s", len(lines), writers, got)
	}
	seen := make(map[string]bool, writers)
	for _, line := range lines {
		if seen[line] {
			t.Fatalf("duplicate line %q in final file:\n%s", line, got)
		}
		seen[line] = true
	}
	for i := 0; i < writers; i++ {
		line := fmt.Sprintf("entry-%02d", i)
		if !seen[line] {
			t.Fatalf("missing %q in final file:\n%s", line, got)
		}
	}
}
