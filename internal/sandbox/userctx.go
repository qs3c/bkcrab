package sandbox

import "context"

// userIDCtxKey carries the current chatter's userID through ctx into the
// sandbox creation path. Threading it via context (instead of widening
// ExecutorPool.Get's signature) keeps the existing 4-tuple pool key
// untouched and lets sites that don't know about chatters (cron flushes,
// admin reload triggers) keep calling Get() the way they already do —
// they just won't get the per-user mount, which is the correct fallback.
type userIDCtxKey struct{}

// WithUserID returns ctx tagged with the chatter userID. Empty uid is a
// no-op (returns ctx unchanged) so call sites don't have to nil-check
// before wrapping.
func WithUserID(ctx context.Context, uid string) context.Context {
	if uid == "" {
		return ctx
	}
	return context.WithValue(ctx, userIDCtxKey{}, uid)
}

// UserIDFromContext extracts the chatter userID set by WithUserID, or
// "" when no wrap happened. Sandbox backends use the empty case to skip
// the per-user skills bind-mount entirely.
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userIDCtxKey{}).(string); ok {
		return v
	}
	return ""
}
