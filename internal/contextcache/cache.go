// Package contextcache implements disposable, generation-fenced Redis data.
// SQL/object storage owns the source data; Redis is never required for a read.
package contextcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var ErrSuperseded = errors.New("cache fill superseded by a newer generation")

// DefaultTTL bounds cache residency; it does not limit a turn's lifetime.
const DefaultTTL = 30 * time.Minute

type Config struct {
	Addr     string
	Password string
	DB       int
	Prefix   string
	TTL      time.Duration
	Timeout  time.Duration
}

func (c Config) Validate() error {
	if c.Addr == "" {
		return errors.New("context cache Redis address is required")
	}
	if c.DB < 0 || c.TTL < 0 || c.Timeout < 0 {
		return errors.New("invalid context cache Redis configuration")
	}
	return nil
}

type Cache struct {
	client                           *redis.Client
	cfg                              Config
	hits, misses, failures, rejected atomic.Uint64
}
type Stats struct{ Hits, Misses, Failures, Rejected, SourceReads uint64 }

func (c *Cache) Stats() Stats {
	return Stats{Hits: c.hits.Load(), Misses: c.misses.Load(), Failures: c.failures.Load(), Rejected: c.rejected.Load()}
}
func New(cfg Config) (*Cache, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Prefix == "" {
		cfg.Prefix = "bkcrab:agentctx:v3:"
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 200 * time.Millisecond
	}
	return &Cache{cfg: cfg, client: redis.NewClient(&redis.Options{Addr: cfg.Addr, Password: cfg.Password, DB: cfg.DB, DialTimeout: cfg.Timeout, ReadTimeout: cfg.Timeout, WriteTimeout: cfg.Timeout, MaxRetries: -1, ContextTimeoutEnabled: true})}, nil
}
func (c *Cache) Close() error { return c.client.Close() }

// Key exposes the resource kind for inspection while hashing the complete JSON
// tuple to avoid exposing raw scope identifiers or delimiter ambiguity. Health has no scope.
func Key(parts ...string) string {
	if len(parts) == 1 && parts[0] == "health" {
		return "health"
	}
	b, _ := json.Marshal(parts)
	s := sha256.Sum256(b)
	digest := hex.EncodeToString(s[:])
	if len(parts) == 4 {
		switch parts[0] {
		case "file":
			// Escape filenames so colons, slashes and whitespace cannot introduce
			// extra key hierarchy. The digest still identifies the complete scope.
			return "file:" + url.QueryEscape(parts[3]) + ":" + digest
		case "skillcatalog":
			layer := "agent"
			if parts[1] == "_global" {
				layer = "global"
			} else if strings.HasPrefix(parts[1], "_user_") {
				layer = "user"
			}
			return "skillcatalog:" + layer + ":" + digest
		}
	}
	if len(parts) > 0 {
		return parts[0] + ":" + digest
	}
	return digest
}
func (c *Cache) key(k string) string { return c.cfg.Prefix + k }
func (c *Cache) call(ctx context.Context, script *redis.Script, k string, args ...any) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	out, err := script.Run(ctx, c.client, []string{c.key(k)}, args...).Result()
	if err != nil {
		c.failures.Add(1)
	}
	return out, err
}

// One hash stores both the epoch and payload: eviction cannot leave an orphan
// payload or let an old fill create a new epoch. Every cold epoch is a UUID.
var readScript = redis.NewScript(`
local p=redis.call('HGET',KEYS[1],'payload')
if p then return {p,''} end
local e=redis.call('HGET',KEYS[1],'epoch')
if not e then e=ARGV[1]; redis.call('HSET',KEYS[1],'epoch',e); redis.call('PEXPIRE',KEYS[1],ARGV[2]) end
return {'',e}
`)

func (c *Cache) Read(ctx context.Context, k string) (payload []byte, epoch string, err error) {
	out, err := c.call(ctx, readScript, k, uuid.NewString(), c.cfg.TTL.Milliseconds())
	if err != nil {
		return nil, "", err
	}
	a, ok := out.([]interface{})
	if !ok || len(a) != 2 {
		return nil, "", errors.New("invalid cache response")
	}
	p, _ := a[0].(string)
	e, _ := a[1].(string)
	if p != "" {
		c.hits.Add(1)
	} else {
		c.misses.Add(1)
	}
	return []byte(p), e, nil
}

// Revisions use fixed-width decimal strings so Lua never rounds a 64-bit int.
func revision(v int64) string { return fmt.Sprintf("%020d", v) }

var fillScript = redis.NewScript(`
if redis.call('HGET',KEYS[1],'epoch') ~= ARGV[1] then return 0 end
local r=redis.call('HGET',KEYS[1],'revision')
if r and r > ARGV[2] then return 0 end
redis.call('HSET',KEYS[1],'revision',ARGV[2],'payload',ARGV[3])
redis.call('PEXPIRE',KEYS[1],ARGV[4]); return 1
`)

func (c *Cache) Fill(ctx context.Context, k, epoch string, rev int64, payload []byte) error {
	if epoch == "" {
		return nil
	}
	out, err := c.call(ctx, fillScript, k, epoch, revision(rev), string(payload), c.cfg.TTL.Milliseconds())
	if err == nil && out == int64(0) {
		c.rejected.Add(1)
		return ErrSuperseded
	}
	return err
}

// Mutation only advances an EXISTING epoch. If expired/restarted, readers must
// obtain a new epoch before reading the source; delayed writes never create it.
var changeScript = redis.NewScript(`
local e=redis.call('HGET',KEYS[1],'epoch')
if not e then return 0 end
local r=redis.call('HGET',KEYS[1],'revision')
if r and r > ARGV[1] then return 0 end
redis.call('HSET',KEYS[1],'epoch',ARGV[2],'revision',ARGV[1])
redis.call('HDEL',KEYS[1],'payload')
if ARGV[3] ~= '' then redis.call('HSET',KEYS[1],'payload',ARGV[3]) end
redis.call('PEXPIRE',KEYS[1],ARGV[4]); return 1
`)

func (c *Cache) Change(ctx context.Context, k string, rev int64, payload []byte) error {
	_, err := c.call(ctx, changeScript, k, revision(rev), uuid.NewString(), string(payload), c.cfg.TTL.Milliseconds())
	return err
}

// Drop discards malformed entries. Missing payloads are ordinary cold misses.
func (c *Cache) Drop(ctx context.Context, k string) error {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	return c.client.Del(ctx, c.key(k)).Err()
}
