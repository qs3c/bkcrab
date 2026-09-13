package contextcache

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

func testCache(t *testing.T) *Cache {
	t.Helper()
	url := os.Getenv("BKCRAB_TEST_CONTEXT_REDIS_URL")
	if url == "" {
		t.Skip("set BKCRAB_TEST_CONTEXT_REDIS_URL to an isolated Redis")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	opts.MaxRetries = -1
	c := New(redis.NewClient(opts), uuid.NewString())
	t.Cleanup(func() { c.Close() })
	if err := c.client.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestCacheReadInvalidationAndRecovery(t *testing.T) {
	c, ctx := testCache(t), context.Background()
	key := Key("file", "agent", "user", "MEMORY.md")
	value, loads := "old", 0
	load := func() (string, error) { loads++; return value, nil }
	read := func(want string) {
		t.Helper()
		got, err := Read(ctx, c, key, load)
		if err != nil || got != want {
			t.Fatalf("got %q, %v; want %q", got, err, want)
		}
	}
	read("old")
	read("old")
	if loads != 1 {
		t.Fatalf("warm read hit source %d times", loads)
	}
	finish := c.Changing(ctx, key)
	read("old") // In-flight write cannot populate the cache.
	value = "new"
	finish()
	read("new")
	read("new")
	if loads != 3 {
		t.Fatalf("loads = %d", loads)
	}
	if err := c.client.Del(ctx, c.prefix+key).Err(); err != nil {
		t.Fatal(err)
	}
	read("new") // Lost/evicted Redis key recovers from source.
	if loads != 4 {
		t.Fatalf("loads = %d", loads)
	}
	_ = c.client.HSet(ctx, c.prefix+key, "data", "bad json").Err()
	read("new")
	_ = c.client.Close()
	value = "after outage"
	read(value)
	read(value)
	if !c.bypassing() {
		t.Fatal("Redis failure did not open source bypass")
	}
}

func TestLateFillCannotResurrectDeletedOrUpdatedData(t *testing.T) {
	c, ctx := testCache(t), context.Background()
	key := Key("session", "a")
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := Read(ctx, c, key, func() (string, error) { close(started); <-release; return "old", nil })
		done <- err
	}()
	<-started
	finish := c.Changing(ctx, key)
	finish()
	if _, err := Read(ctx, c, key, func() (string, error) { return "new", nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := Read(ctx, c, key, func() (string, error) { return "", errors.New("unexpected source read") })
	if err != nil || got != "new" {
		t.Fatalf("stale fill won: %q %v", got, err)
	}
}

func TestExpiryDoesNotRenewOnHitsAndSourceErrorsAreNotCached(t *testing.T) {
	c, ctx := testCache(t), context.Background()
	key := Key("test")
	for i := 0; i < 2; i++ {
		_, err := Read(ctx, c, key, func() (string, error) { return "", errors.New("source down") })
		if err == nil {
			t.Fatal("source error hidden")
		}
	}
	c.invalidate(ctx, key)
	_, _ = Read(ctx, c, key, func() (string, error) { return "cached", nil })
	_ = c.client.PExpire(ctx, c.prefix+key, 2*time.Second).Err()
	_, _ = Read(ctx, c, key, func() (string, error) { t.Error("cache miss"); return "", nil })
	if ttl := c.client.PTTL(ctx, c.prefix+key).Val(); ttl > 2*time.Second {
		t.Fatalf("hit renewed TTL: %s", ttl)
	}
	if Key("a:b", "c") == Key("a", "b:c") {
		t.Fatal("ambiguous scope encoding")
	}
}
