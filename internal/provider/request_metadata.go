package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/buildinfo"
)

type sessionContextKey struct{}

// WithSession binds a logical conversation to outgoing LLM requests. Scope
// components must include the owner and conversation (or eval run/case), not
// just a model or provider. Only an opaque digest is sent to the provider.
func WithSession(ctx context.Context, scope ...string) context.Context {
	if len(scope) == 0 {
		return EnsureSession(ctx)
	}
	encoded, _ := json.Marshal(scope)
	digest := sha256.Sum256(encoded)
	return context.WithValue(ctx, sessionContextKey{}, "bkcrab_"+hex.EncodeToString(digest[:]))
}

func SessionIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(sessionContextKey{}).(string)
	return id
}

// EnsureSession gives a standalone operation a session while preserving an
// enclosing chat/evaluation scope. Reuse the returned context across retries
// and auxiliary calls; do not put mutable session state on a shared Provider.
func EnsureSession(ctx context.Context) context.Context {
	if SessionIDFromContext(ctx) != "" {
		return ctx
	}
	return WithSession(ctx, "operation", uuid.NewString())
}

// ApplyRequestMetadata is shared by runtime adapters and connection tests.
// OpenCode requires a stable session header for routing and prompt caching:
// https://opencode.ai/docs/go/#where-can-i-use-it
func ApplyRequestMetadata(req *http.Request) {
	req.Header.Set("User-Agent", "BkCrab/"+buildinfo.Version)
	if !strings.EqualFold(req.URL.Hostname(), "opencode.ai") {
		return
	}
	// Callers with a logical conversation bind it before entering the provider.
	// Otherwise this HTTP request is a standalone operation. HTTP retries of
	// the same request retain this header.
	req.Header.Set("x-opencode-session", SessionIDFromContext(EnsureSession(req.Context())))
}
