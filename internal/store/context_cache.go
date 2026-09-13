package store

import (
	"context"
	"errors"

	"github.com/qs3c/bkcrab/internal/contextcache"
)

// ContextCache is also used for the gateway's object-store metadata cache.
func (d *DBStore) ContextCache() *contextcache.Cache { return d.contextCache }

func fileCacheKey(agentID, userID, filename string) string {
	return contextcache.Key("file", agentID, userID, filename)
}

func sessionCacheKey(userID, agentID, sessionKey string) string {
	return contextcache.Key("session", userID, agentID, sessionKey)
}

// Explicit missing envelopes preserve Exact semantics, including empty files.
// Database errors are never stored as missing content.
func cachedRecord[T any](ctx context.Context, c *contextcache.Cache, key string, load func() (T, error)) (T, error) {
	type record struct {
		Value   T
		Missing bool
	}
	r, err := contextcache.Read(ctx, c, key, func() (record, error) {
		v, err := load()
		if errors.Is(err, ErrNotFound) {
			return record{Missing: true}, nil
		}
		return record{Value: v}, err
	})
	if err == nil && r.Missing {
		err = ErrNotFound
	}
	return r.Value, err
}

func (d *DBStore) GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error) {
	return cachedRecord(ctx, d.contextCache, sessionCacheKey(userID, agentID, sessionKey), func() (*SessionRecord, error) {
		return d.getSession(ctx, userID, agentID, sessionKey)
	})
}

func (d *DBStore) SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error {
	finish := d.contextCache.Changing(ctx, sessionCacheKey(userID, agentID, sessionKey))
	err := d.saveSession(ctx, userID, agentID, sessionKey, session)
	finish()
	if err == nil && d.contextCache != nil {
		// Read back the committed row: upsert intentionally preserves its existing
		// project/channel fields, which may differ from the caller's input.
		_, _ = d.GetSession(ctx, userID, agentID, sessionKey)
	}
	return err
}

func (d *DBStore) GetAgentFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if agentID == "" || userID == "" {
		return d.getAgentFileExact(ctx, agentID, userID, filename)
	}
	return cachedRecord(ctx, d.contextCache, fileCacheKey(agentID, userID, filename), func() ([]byte, error) {
		return d.getAgentFileExact(ctx, agentID, userID, filename)
	})
}

func (d *DBStore) GetAgentFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if d.contextCache == nil || agentID == "" || userID == "" {
		return d.getAgentFile(ctx, agentID, userID, filename)
	}
	data, err := d.GetAgentFileExact(ctx, agentID, userID, filename)
	if !errors.Is(err, ErrNotFound) {
		return data, err
	}
	owner, err := cachedRecord(ctx, d.contextCache, contextcache.Key("file-owner", agentID), func() (string, error) {
		a, err := d.GetAgent(ctx, agentID)
		if err != nil {
			return "", err
		}
		return a.UserID, nil
	})
	if err != nil {
		return nil, err
	}
	if owner == userID || owner == "" {
		return nil, ErrNotFound
	}
	return d.GetAgentFileExact(ctx, agentID, owner, filename)
}
