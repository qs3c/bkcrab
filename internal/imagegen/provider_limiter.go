package imagegen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	redis "github.com/redis/go-redis/v9"
)

var (
	providerLimiterNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,119}$`)
	providerLimiterTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{16,128}$`)
	ErrProviderLeaseLost        = errors.New("imagegen: provider call lease lost")
)

type ProviderLease struct {
	Provider string
	Model    string
	Key      string
	Token    string
}

type ProviderCallGate interface {
	Acquire(ctx context.Context, provider, model, token string, limit int, ttl time.Duration) (ProviderLease, bool, error)
	Renew(ctx context.Context, lease ProviderLease, ttl time.Duration) error
	Release(ctx context.Context, lease ProviderLease) error
}

type RedisProviderLimiter struct {
	client redis.Scripter
	prefix string
}

func NewRedisProviderLimiter(client redis.Scripter, prefix string) (*RedisProviderLimiter, error) {
	if client == nil {
		return nil, errors.New("imagegen: Redis provider limiter client is required")
	}
	if len(prefix) > 256 || strings.ContainsAny(prefix, "\x00\r\n{}") {
		return nil, errors.New("imagegen: invalid provider limiter prefix")
	}
	return &RedisProviderLimiter{client: client, prefix: prefix}, nil
}

func ProviderLimiterKey(prefix, provider, model string) (string, error) {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if !providerLimiterNamePattern.MatchString(provider) || len(model) > 240 || strings.ContainsAny(model, "\x00\r\n") || strings.Contains(model, "..") ||
		len(prefix) > 256 || strings.ContainsAny(prefix, "\x00\r\n{}") {
		return "", errors.New("imagegen: invalid provider limiter identity")
	}
	key := prefix + "image:provider:" + provider
	if model != "" {
		digest := sha256.Sum256([]byte(model))
		key += ":" + hex.EncodeToString(digest[:12])
	}
	if len(key) > 512 {
		return "", errors.New("imagegen: provider limiter key is too long")
	}
	return key, nil
}

func (l *RedisProviderLimiter) Acquire(ctx context.Context, provider, model, token string, limit int, ttl time.Duration) (ProviderLease, bool, error) {
	if l == nil || l.client == nil {
		return ProviderLease{}, false, errors.New("imagegen: Redis provider limiter is not configured")
	}
	key, err := ProviderLimiterKey(l.prefix, provider, model)
	if err != nil {
		return ProviderLease{}, false, err
	}
	if !providerLimiterTokenPattern.MatchString(token) || limit < 1 || ttl <= 0 || ttl > 24*time.Hour {
		return ProviderLease{}, false, errors.New("imagegen: invalid provider limiter acquire request")
	}
	result, err := providerLimiterAcquireScript.Run(ctx, l.client, []string{key}, token, limit, ttl.Milliseconds()).Int64()
	if err != nil {
		return ProviderLease{}, false, fmt.Errorf("imagegen: acquire provider limiter: %w", err)
	}
	lease := ProviderLease{Provider: provider, Model: model, Key: key, Token: token}
	return lease, result == 1, nil
}

func (l *RedisProviderLimiter) Renew(ctx context.Context, lease ProviderLease, ttl time.Duration) error {
	if l == nil || l.client == nil {
		return errors.New("imagegen: Redis provider limiter is not configured")
	}
	key, err := ProviderLimiterKey(l.prefix, lease.Provider, lease.Model)
	if err != nil || key != lease.Key || !providerLimiterTokenPattern.MatchString(lease.Token) || ttl <= 0 || ttl > 24*time.Hour {
		return errors.New("imagegen: invalid provider limiter renewal")
	}
	result, err := providerLimiterRenewScript.Run(ctx, l.client, []string{key}, lease.Token, ttl.Milliseconds()).Int64()
	if err != nil {
		return fmt.Errorf("imagegen: renew provider limiter: %w", err)
	}
	if result != 1 {
		return ErrProviderLeaseLost
	}
	return nil
}

func (l *RedisProviderLimiter) Release(ctx context.Context, lease ProviderLease) error {
	if l == nil || l.client == nil {
		return errors.New("imagegen: Redis provider limiter is not configured")
	}
	key, err := ProviderLimiterKey(l.prefix, lease.Provider, lease.Model)
	if err != nil || key != lease.Key || !providerLimiterTokenPattern.MatchString(lease.Token) {
		return errors.New("imagegen: invalid provider limiter release")
	}
	if err := providerLimiterReleaseScript.Run(ctx, l.client, []string{key}, lease.Token).Err(); err != nil {
		return fmt.Errorf("imagegen: release provider limiter: %w", err)
	}
	return nil
}

const providerLimiterCommonLua = `
local now_parts = redis.call('TIME')
local now_ms = tonumber(now_parts[1]) * 1000 + math.floor(tonumber(now_parts[2]) / 1000)
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now_ms)
local function refresh_key_ttl()
  local last = redis.call('ZREVRANGE', KEYS[1], 0, 0, 'WITHSCORES')
  if #last == 0 then
    redis.call('DEL', KEYS[1])
  else
    redis.call('PEXPIREAT', KEYS[1], math.ceil(tonumber(last[2])))
  end
end
`

var providerLimiterAcquireScript = redis.NewScript(providerLimiterCommonLua + `
local existing = redis.call('ZSCORE', KEYS[1], ARGV[1])
local expires_ms = now_ms + tonumber(ARGV[3])
if existing then
  redis.call('ZADD', KEYS[1], 'XX', expires_ms, ARGV[1])
  refresh_key_ttl()
  return 1
end
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then
  refresh_key_ttl()
  return 0
end
redis.call('ZADD', KEYS[1], 'NX', expires_ms, ARGV[1])
refresh_key_ttl()
return 1
`)

var providerLimiterRenewScript = redis.NewScript(providerLimiterCommonLua + `
if not redis.call('ZSCORE', KEYS[1], ARGV[1]) then
  refresh_key_ttl()
  return 0
end
redis.call('ZADD', KEYS[1], 'XX', now_ms + tonumber(ARGV[2]), ARGV[1])
refresh_key_ttl()
return 1
`)

var providerLimiterReleaseScript = redis.NewScript(providerLimiterCommonLua + `
redis.call('ZREM', KEYS[1], ARGV[1])
refresh_key_ttl()
return 1
`)
