package workspace

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func testQuota(t *testing.T, bytes int64, files int) *QuotaStore {
	t.Helper()
	return NewQuotaStore(NewLocalFS(t.TempDir()), bytes, files,
		func(context.Context, string) (string, error) { return "alice", nil },
		func(context.Context, string) ([]string, error) { return []string{"a", "b"}, nil })
}
func TestQuotaAcrossAgentsOverwriteDelete(t *testing.T) {
	q := testQuota(t, 6, 2)
	ctx := context.Background()
	put := func(a, path, data string) error {
		return q.Put(ctx, a, "", "", path, strings.NewReader(data), int64(len(data)), "")
	}
	if err := put("a", "one", "123"); err != nil {
		t.Fatal(err)
	}
	if err := put("b", "two", "456"); err != nil {
		t.Fatal(err)
	}
	if err := put("a", "three", "x"); err == nil {
		t.Fatal("allowed over quota")
	}
	if err := put("a", "one", "abc"); err != nil {
		t.Fatal("same-size overwrite should fit", err)
	}
	if err := q.Delete(ctx, "b", "", "", "two"); err != nil {
		t.Fatal(err)
	}
	if err := put("a", "three", "xyz"); err != nil {
		t.Fatal("delete did not free quota", err)
	}
}
func TestQuotaConcurrentReservation(t *testing.T) {
	q := testQuota(t, 8, 100)
	var wg sync.WaitGroup
	var success atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if q.Put(context.Background(), "a", "", "", string(rune('a'+i)), strings.NewReader("xx"), 2, "") == nil {
				success.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if success.Load() != 4 {
		t.Fatalf("admitted %d instead of 4 writes", success.Load())
	}
}
func TestQuotaCountsPreexistingData(t *testing.T) {
	q := testQuota(t, 4, 100)
	ctx := context.Background()
	if err := q.Store.Put(ctx, "b", "", "", "old", strings.NewReader("1234"), 4, ""); err != nil {
		t.Fatal(err)
	}
	if err := q.Put(ctx, "a", "", "", "new", strings.NewReader("x"), 1, ""); err == nil {
		t.Fatal("ignored preexisting objects")
	}
}
