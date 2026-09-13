// Package contextcache provides a disposable, shared cache for Agent inputs.
package contextcache

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// ponytail: fixed, non-sliding one-minute TTL bounds missed invalidation after
// a writer crash or partial Redis outage. Add a transactional outbox if that
// eventual-consistency ceiling is insufficient; this is not a durable store.
const TTL = time.Minute
const timeout = 200 * time.Millisecond

type Cache struct {
	client      *redis.Client
	prefix      string
	bypassUntil atomic.Int64
}

// FromEnv is shared by the gateway and CLI store factories. Empty URL disables
// caching. Never share the fair-queue namespace or log credentials from the URL.
func FromEnv(namespace string) (*Cache, error) {
	url := os.Getenv("BKCRAB_CONTEXT_CACHE_REDIS_URL")
	if url == "" {
		return nil, nil
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("invalid BKCRAB_CONTEXT_CACHE_REDIS_URL")
	}
	opts.DialTimeout, opts.ReadTimeout, opts.WriteTimeout = timeout, timeout, timeout
	opts.ContextTimeoutEnabled = true
	opts.MaxRetries = -1
	if shared := os.Getenv("BKCRAB_CONTEXT_CACHE_NAMESPACE"); shared != "" {
		namespace = shared
	}
	return New(redis.NewClient(opts), namespace), nil
}

func New(client *redis.Client, namespace string) *Cache {
	return &Cache{client: client, prefix: fmt.Sprintf("bkcrab:agentctx:v1:%x:", sha256.Sum256([]byte(namespace)))}
}

func (c *Cache) Close() error {
	if c == nil {
		return nil
	}
	return c.client.Close()
}

// Key encodes boundaries (including empty components) without exposing user IDs.
func Key(parts ...string) string {
	b, _ := json.Marshal(parts)
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// The loading token lives in the SAME expiring key as the data. Deletion,
// eviction, a writer or restart revokes a previous loader's right to fill it.
var readScript = redis.NewScript(`
local data = redis.call('HGET', KEYS[1], 'data')
if data then return {1, data} end
if redis.call('EXISTS', KEYS[1]) == 1 then return {0, ''} end
redis.call('HSET', KEYS[1], 'token', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return {0, ARGV[1]}
`)

var fillScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'token') ~= ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'data', ARGV[2])
return 1
`)

var blockScript = redis.NewScript(`
redis.call('DEL', KEYS[1])
redis.call('HSET', KEYS[1], 'token', ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

type cachedValue[T any] struct {
	Version int
	Value   T
}

func Read[T any](ctx context.Context, c *Cache, key string, load func() (T, error)) (T, error) {
	if c == nil || c.bypassing() {
		return load()
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	res, err := readScript.Run(rctx, c.client, []string{c.prefix + key}, uuid.NewString(), TTL.Milliseconds()).Slice()
	cancel()
	if err != nil {
		c.failed()
	}
	token := ""
	if err == nil && len(res) == 2 {
		value, _ := res[1].(string)
		if res[0] == int64(1) {
			var out cachedValue[T]
			if json.Unmarshal([]byte(value), &out) == nil && out.Version == 1 {
				return out.Value, nil
			}
			c.invalidate(ctx, key) // Corrupt/old schema: read the source, never return zero values.
		} else {
			token = value
		}
	}
	start := time.Now()
	out, err := load()
	slog.Debug("agent context cache source read", "key", key, "duration", time.Since(start), "failed", err != nil)
	if err != nil || token == "" {
		return out, err
	}
	b, err := json.Marshal(cachedValue[T]{Version: 1, Value: out})
	if err == nil && len(b) <= 2<<20 {
		rctx, cancel = context.WithTimeout(ctx, timeout)
		if err := fillScript.Run(rctx, c.client, []string{c.prefix + key}, token, string(b)).Err(); err != nil {
			c.failed()
		}
		cancel()
	}
	return out, nil
}

// Changing invalidates before AND after a source write. Reads during the write
// go to the source without populating Redis. A crashed writer leaves an expiring
// miss marker, not an indefinitely cached pre-write value. Overlapping writers
// can cause extra misses; this does not replace source-side write serialization.
func (c *Cache) Changing(ctx context.Context, keys ...string) func() {
	if c == nil {
		return func() {}
	}
	if c.bypassing() {
		// Keep attempting post-write invalidation so a recovered Redis is usable
		// after the cooldown even when this Agent receives continuous writes.
		return func() {
			for _, key := range keys {
				c.invalidate(ctx, key)
			}
		}
	}
	for _, key := range keys {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		if err := blockScript.Run(rctx, c.client, []string{c.prefix + key}, uuid.NewString(), TTL.Milliseconds()).Err(); err != nil {
			c.failed()
		}
		cancel()
	}
	return func() {
		for _, key := range keys {
			c.invalidate(ctx, key)
		}
	}
}

func (c *Cache) invalidate(ctx context.Context, key string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	if err := c.client.Del(rctx, c.prefix+key).Err(); err != nil {
		c.failed()
	}
}

func (c *Cache) bypassing() bool { return time.Now().UnixNano() < c.bypassUntil.Load() }

func (c *Cache) failed() {
	if !c.bypassing() {
		slog.Warn("agent context cache unavailable; bypassing until cached data expires")
	}
	c.bypassUntil.Store(time.Now().Add(TTL).UnixNano())
}

// ChangingAll is reserved for destructive user/agent deletion. ponytail: these
// rare operations scan this database's cache namespace; add an entity key index
// only if deletion latency becomes significant. Never touch fair-queue keys.
func (c *Cache) ChangingAll(ctx context.Context) func() {
	clear := func() {
		if c == nil {
			return
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		iter := c.client.Scan(rctx, 0, c.prefix+"*", 200).Iterator()
		for iter.Next(rctx) {
			if err := c.client.Del(rctx, iter.Val()).Err(); err != nil {
				c.failed()
				return
			}
		}
		if iter.Err() != nil {
			c.failed()
		}
	}
	clear()
	return clear
}
