package store

import "context"

// chatterUserIDCtxKey tags ctx with the resolved per-turn chatter
// userID so DBStore writes (AppendSessionMessage / SaveSession /
// AppendSessionEvent) can persist it without changing every callsite's
// signature. The agent loop sets this at the top of HandleMessage /
// HandleMessageStream; everything downstream just propagates ctx.
type chatterUserIDCtxKey struct{}

// WithChatterUserID returns ctx tagged with the per-turn chatter
// userID. Distinct from config.WithUserID (the authenticated user
// resolved by middleware) and sandbox.WithUserID (the executor mount
// target) — those two carry different values whenever an IM channel
// routes a per-sender app_user into a channel-owner UserSpace.
// Empty uid is a no-op so callers don't have to guard.
func WithChatterUserID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, chatterUserIDCtxKey{}, uid)
}

// ChatterUserIDFromContext returns the chatter userID set by
// WithChatterUserID, or "" if none. Store implementations should
// COALESCE the result into the chatter_user_id column on session
// writes; an empty value (background ctx, untagged code path) writes
// '' and readers fall back to user_id at query time.
func ChatterUserIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(chatterUserIDCtxKey{}).(string)
	return v
}
