package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/qs3c/bkcrab/internal/contextcache"
	"golang.org/x/sync/singleflight"
)

type cacheScope struct{ Kind, S1, S2, S3 string }

func (s cacheScope) key() string { return contextcache.Key(s.Kind, s.S1, s.S2, s.S3) }

type cacheEnvelope struct {
	Schema   int
	Found    bool
	Revision int64
	Value    json.RawMessage
}
type dbContextCache struct {
	backend     *contextcache.Cache
	cancel      context.CancelFunc
	done        chan struct{}
	group       singleflight.Group
	mu          sync.RWMutex
	degraded    bool
	sourceReads atomic.Uint64
	stop        sync.Once
}

// EnableContextCache is called once, before this store is shared with workers.
// Migrations are deliberately separate; autoMigrate=false never changes schema.
func (d *DBStore) EnableContextCache(cfg contextcache.Config) error {
	var count int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM context_cache_changes WHERE 1=0").Scan(&count); err != nil {
		return fmt.Errorf("context cache schema is missing; run migrations: %w", err)
	}
	c, err := contextcache.New(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.contextCache = &dbContextCache{backend: c, cancel: cancel, done: make(chan struct{}), degraded: true}
	go func() {
		defer close(d.contextCache.done)
		d.reconcileContextCache(ctx)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.reconcileContextCache(ctx)
			}
		}
	}()
	return nil
}
func (d *DBStore) ContextCacheStats() contextcache.Stats {
	if d.contextCache == nil {
		return contextcache.Stats{}
	}
	out := d.contextCache.backend.Stats()
	out.SourceReads = d.contextCache.sourceReads.Load()
	return out
}
func (d *DBStore) cacheRevision(ctx context.Context, s cacheScope) (int64, error) {
	var rev int64
	err := d.db.QueryRowContext(ctx, fmt.Sprintf("SELECT revision FROM context_cache_changes WHERE kind=%s AND s1=%s AND s2=%s AND s3=%s", d.ph(1), d.ph(2), d.ph(3), d.ph(4)), s.Kind, s.S1, s.S2, s.S3).Scan(&rev)
	if errors.Is(scanErr(err), ErrNotFound) {
		return 0, nil
	}
	return rev, err
}
func (c *dbContextCache) available() bool { c.mu.RLock(); defer c.mu.RUnlock(); return !c.degraded }
func (c *dbContextCache) degrade()        { c.mu.Lock(); c.degraded = true; c.mu.Unlock() }

// Stable source reads bracket data with revisions. They run only on misses;
// mutation between reads prevents caching a mismatched payload/version pair.
func (d *DBStore) stableCacheLoad(ctx context.Context, s cacheScope, load func() (any, error)) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		before, err := d.cacheRevision(ctx, s)
		if err != nil {
			return nil, err
		}
		value, readErr := load()
		if readErr != nil && !errors.Is(readErr, ErrNotFound) {
			return nil, readErr
		}
		after, err := d.cacheRevision(ctx, s)
		if err != nil {
			return nil, err
		}
		if before != after {
			continue
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return json.Marshal(cacheEnvelope{Schema: 1, Found: readErr == nil, Revision: after, Value: data})
	}
	return nil, errors.New("context source changed during cache fill")
}
func cachedRead[T any](d *DBStore, ctx context.Context, s cacheScope, load func() (T, error)) (T, error) {
	source := load
	load = func() (T, error) {
		if d.contextCache != nil {
			d.contextCache.sourceReads.Add(1)
		}
		return source()
	}

	if d.contextCache == nil || !d.contextCache.available() {
		return load()
	}
	c := d.contextCache
	// Singleflight result is serialized bytes, never a shared mutable message slice.
	result, err, _ := c.group.Do(s.key(), func() (any, error) {
		p, e, err := c.backend.Read(ctx, s.key())
		if err != nil {
			c.degrade()
			return nil, err
		}
		if len(p) > 0 {
			var env cacheEnvelope
			var decoded T
			if json.Unmarshal(p, &env) == nil && env.Schema == 1 && env.Revision >= 0 && json.Valid(env.Value) && (!env.Found || json.Unmarshal(env.Value, &decoded) == nil) {
				return p, nil
			}
			_ = c.backend.Drop(ctx, s.key())
			p, e, err = c.backend.Read(ctx, s.key())
			if err != nil {
				return nil, err
			}
		}
		p, err = d.stableCacheLoad(ctx, s, func() (any, error) { return load() })
		if err != nil {
			return nil, err
		}
		var env cacheEnvelope
		_ = json.Unmarshal(p, &env)
		if err = c.backend.Fill(ctx, s.key(), e, env.Revision, p); err != nil {
			if errors.Is(err, contextcache.ErrSuperseded) {
				return nil, err
			}
			c.degrade()
		}
		return p, nil
	})
	if err != nil {
		return load()
	}
	var env cacheEnvelope
	if err = json.Unmarshal(result.([]byte), &env); err != nil {
		return load()
	}
	var value T
	if !env.Found {
		return value, ErrNotFound
	}
	if err = json.Unmarshal(env.Value, &value); err != nil {
		return load()
	}
	return value, nil
}

// afterContextWrite never converts a committed SQL write into a retryable
// business failure. Its durable outbox remains pending when Redis is down.
func (d *DBStore) afterContextWrite(ctx context.Context, s cacheScope) {
	if d.contextCache == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if s.Kind == "agent" {
		rows, err := d.db.QueryContext(ctx, fmt.Sprintf("SELECT kind,s1,s2,s3 FROM context_cache_changes WHERE dirty=TRUE AND ((kind='file' AND s1=%s) OR (kind='session' AND s2=%s))", d.ph(1), d.ph(2)), s.S1, s.S1)
		if err == nil {
			var children []cacheScope
			for rows.Next() {
				var child cacheScope
				if rows.Scan(&child.Kind, &child.S1, &child.S2, &child.S3) == nil {
					children = append(children, child)
				}
			}
			rows.Close()
			for _, child := range children {
				if err := d.syncContextScope(ctx, child); err != nil {
					d.contextCache.degrade()
				}
			}
		}
	}

	if err := d.syncContextScope(ctx, s); err != nil {
		d.contextCache.degrade()
		slog.Warn("context cache sync pending", "kind", s.Kind, "error", err)
	}
}
func (d *DBStore) syncContextScope(ctx context.Context, s cacheScope) error {
	rev, err := d.cacheRevision(ctx, s)
	if err != nil {
		return err
	}
	c := d.contextCache.backend
	if err = c.Change(ctx, s.key(), rev, nil); err != nil {
		return err
	}
	// Sessions are warmed from the committed source, not the caller's stale copy.
	// Obtain an epoch BEFORE reading SQL, so restart/eviction fences late fills.
	if s.Kind == "session" {
		_, epoch, err := c.Read(ctx, s.key())
		if err != nil {
			return err
		}
		if epoch != "" {
			p, err := d.stableCacheLoad(ctx, s, func() (any, error) { return d.getSessionUncached(ctx, s.S1, s.S2, s.S3) })
			if err != nil {
				return err
			}
			var env cacheEnvelope
			_ = json.Unmarshal(p, &env)
			if err = c.Fill(ctx, s.key(), epoch, env.Revision, p); err != nil {
				return err
			}
		}
	}
	_, err = d.db.ExecContext(ctx, fmt.Sprintf("UPDATE context_cache_changes SET applied=%s,dirty=FALSE WHERE kind=%s AND s1=%s AND s2=%s AND s3=%s AND revision=%s", d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)), rev, s.Kind, s.S1, s.S2, s.S3, rev)
	return err
}

// A coalescing outbox needs no per-event lease: workers may duplicate the
// idempotent operation; conditional acknowledgment cannot swallow a new write.
func (d *DBStore) reconcileContextCache(ctx context.Context) {
	rows, err := d.db.QueryContext(ctx, "SELECT kind,s1,s2,s3 FROM context_cache_changes WHERE dirty=TRUE ORDER BY kind,s1,s2,s3 LIMIT 100")
	if err != nil {
		d.contextCache.degrade()
		return
	}
	var scopes []cacheScope
	for rows.Next() {
		var s cacheScope
		if err = rows.Scan(&s.Kind, &s.S1, &s.S2, &s.S3); err != nil {
			break
		}
		scopes = append(scopes, s)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		d.contextCache.degrade()
		return
	}
	for _, s := range scopes {
		if err = d.syncContextScope(ctx, s); err != nil {
			d.contextCache.degrade()
			slog.Warn("context cache outbox retry pending", "kind", s.Kind, "error", err)
		}
	}
	// A successful Redis probe is required even for an empty outbox.
	if _, _, err = d.contextCache.backend.Read(ctx, contextcache.Key("health")); err != nil {
		d.contextCache.degrade()
		return
	}
	var pending int
	if err = d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM context_cache_changes WHERE dirty=TRUE").Scan(&pending); err != nil || pending > 0 {
		return
	}
	d.contextCache.mu.Lock()
	d.contextCache.degraded = false
	d.contextCache.mu.Unlock()
}

func (d *DBStore) GetSession(ctx context.Context, u, a, k string) (*SessionRecord, error) {
	return cachedRead(d, ctx, cacheScope{"session", u, a, k}, func() (*SessionRecord, error) { return d.getSessionUncached(ctx, u, a, k) })
}
func (d *DBStore) GetAgentFileExact(ctx context.Context, a, u, f string) ([]byte, error) {
	return cachedRead(d, ctx, cacheScope{"file", a, u, f}, func() ([]byte, error) { return d.getAgentFileExactUncached(ctx, a, u, f) })
}
func (d *DBStore) GetAgentFile(ctx context.Context, a, u, f string) ([]byte, error) {
	if d.contextCache == nil {
		return d.getAgentFileUncached(ctx, a, u, f)
	}
	b, err := d.GetAgentFileExact(ctx, a, u, f)
	if !errors.Is(err, ErrNotFound) {
		return b, err
	}
	owner, err := cachedRead(d, ctx, cacheScope{"agent", a, "", ""}, func() (string, error) {
		var owner string
		err := d.db.QueryRowContext(ctx, "SELECT user_id FROM agents WHERE id="+d.ph(1), a).Scan(&owner)
		return owner, scanErr(err)
	})
	if err != nil {
		return nil, err
	}
	if owner == u {
		return nil, ErrNotFound
	}
	return d.GetAgentFileExact(ctx, a, owner, f)
}
func (d *DBStore) ListSkillUsage(ctx context.Context, a string) ([]SkillUsageRow, error) {
	return cachedRead(d, ctx, cacheScope{"skillstate", a, "", ""}, func() ([]SkillUsageRow, error) { return d.listSkillUsageUncached(ctx, a) })
}

// StopContextCache releases only cache-owned resources, leaving SQL available to
// the gateway's remaining shutdown work. It is safe to call again from Close.
func (d *DBStore) StopContextCache() {
	if c := d.contextCache; c != nil {
		c.stop.Do(func() { c.cancel(); <-c.done; _ = c.backend.Close() })
	}
}
