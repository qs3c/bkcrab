package contextcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testCache(t *testing.T) *Cache {
	t.Helper()
	addr := os.Getenv("BKCRAB_CACHE_TEST_REDIS")
	if addr == "" {
		t.Skip("set BKCRAB_CACHE_TEST_REDIS for real Redis tests")
	}
	c, err := New(Config{Addr: addr, Prefix: "bkcrab:agentctx:test:" + uuid.NewString() + ":", TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}
func TestFillFencedByWriteAndEviction(t *testing.T) {
	c := testCache(t)
	ctx := context.Background()
	k := Key("file", "a", "u", "USER.md")
	_, old, err := c.Read(ctx, k)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Change(ctx, k, 2, nil); err != nil {
		t.Fatal(err)
	}
	if err = c.Fill(ctx, k, old, 1, []byte("old")); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	p, e, _ := c.Read(ctx, k)
	if len(p) > 0 {
		t.Fatal("old fill survived invalidation")
	}
	if err = c.Fill(ctx, k, e, 2, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err = c.Change(ctx, k, 1, nil); err != nil {
		t.Fatal(err)
	}
	p, _, _ = c.Read(ctx, k)
	if string(p) != "new" {
		t.Fatal("old event removed newer value")
	}
	c.Drop(ctx, k)
	_, e, _ = c.Read(ctx, k)
	if err = c.Fill(ctx, k, old, 1, []byte("resurrected")); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	p, _, _ = c.Read(ctx, k)
	if len(p) > 0 {
		t.Fatal("fill recreated evicted generation")
	}
	if err = c.Fill(ctx, k, e, 3, []byte("latest")); err != nil {
		t.Fatal(err)
	}
}
func TestRevisionBeyondLuaIntegerPrecision(t *testing.T) {
	c := testCache(t)
	ctx := context.Background()
	_, e, _ := c.Read(ctx, "k")
	c.Fill(ctx, "k", e, 9007199254740993, []byte("new"))
	c.Change(ctx, "k", 9007199254740992, nil)
	p, _, _ := c.Read(ctx, "k")
	if string(p) != "new" {
		t.Fatal("64-bit revision rounded")
	}
}

func TestConcurrentFillAndInvalidate(t *testing.T) {
	c := testCache(t)
	ctx := context.Background()
	for i := int64(1); i < 30; i++ {
		k := Key("race", fmt.Sprint(i))
		_, epoch, err := c.Read(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { defer close(done); _ = c.Fill(ctx, k, epoch, i, []byte("old")) }()
		if err = c.Change(ctx, k, i+1, nil); err != nil {
			t.Fatal(err)
		}
		<-done
		p, _, err := c.Read(ctx, k)
		if err != nil || len(p) > 0 {
			t.Fatal("stale fill won mutation", string(p), err)
		}
	}
}
