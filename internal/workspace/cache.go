package workspace

import (
	"context"
	"io"

	"github.com/qs3c/bkcrab/internal/contextcache"
)

// WithCache caches listings used by skill hydration, not artifact bytes. All
// gateway consumers (setup, tools, skills) receive this same Store wrapper.
func WithCache(st Store, cache *contextcache.Cache, namespace string) Store {
	if cache == nil {
		return st
	}
	if _, local := st.(*LocalFS); local {
		return st
	} // Pod-local files are not shared Redis data.
	return &cachedStore{Store: st, cache: cache, namespace: namespace}
}

type cachedStore struct {
	Store
	cache     *contextcache.Cache
	namespace string
}

func (s *cachedStore) key(agentID string) string {
	return contextcache.Key("workspace-list", s.namespace, agentID)
}

func (s *cachedStore) List(ctx context.Context, agentID, projectID, sessionID string) ([]ObjectInfo, error) {
	if projectID != "" || sessionID != "" {
		return s.Store.List(ctx, agentID, projectID, sessionID)
	}
	return contextcache.Read(ctx, s.cache, s.key(agentID), func() ([]ObjectInfo, error) {
		return s.Store.List(ctx, agentID, projectID, sessionID)
	})
}

func (s *cachedStore) Put(ctx context.Context, agentID, projectID, sessionID, path string, r io.Reader, size int64, contentType string) error {
	defer s.cache.Changing(ctx, s.key(agentID))()
	return s.Store.Put(ctx, agentID, projectID, sessionID, path, r, size, contentType)
}

func (s *cachedStore) Delete(ctx context.Context, agentID, projectID, sessionID, path string) error {
	defer s.cache.Changing(ctx, s.key(agentID))()
	return s.Store.Delete(ctx, agentID, projectID, sessionID, path)
}

func (s *cachedStore) Move(ctx context.Context, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID string) error {
	defer s.cache.Changing(ctx, s.key(agentID))()
	return s.Store.Move(ctx, agentID, fromProjectID, fromSessionID, toProjectID, toSessionID)
}

// ChangingSkills brackets multi-file skill publication so a listing made during
// upload cannot remain cached after the whole package has finished.
func ChangingSkills(ctx context.Context, st Store, owner string) func() {
	if s, ok := st.(*cachedStore); ok {
		return s.cache.Changing(ctx, s.key(owner))
	}
	return func() {}
}
