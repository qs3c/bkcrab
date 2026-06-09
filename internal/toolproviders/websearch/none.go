package websearch

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// None is a sentinel provider meaning "do not expose web_search to the model."
// The tool-registration layer (internal/agent/tools/web_search.go) detects
// "none" anywhere in the chain and skips registering web_search at all, so
// the model falls back to whatever native search capability it has (e.g. an
// Anthropic server-side tool, if wired up later) or does without.
//
// It opts into CredentialFree so chain.Available() reports true when "none"
// is the only configured provider — the dashboard can distinguish "user made
// an explicit choice" from "forgot to configure anything".
type None struct{}

func (None) Category() string     { return Category }
func (None) Name() string         { return "none" }
func (None) CredentialFree() bool { return true }

// Execute should never be reached: web_search registration short-circuits on
// "none" before the chain runs. The error is defensive — if someone wires the
// chain a new way and bypasses the skip, the failure should surface loudly
// rather than silently returning empty results.
func (None) Execute(_ context.Context, _ toolproviders.Request) (toolproviders.Response, error) {
	return toolproviders.Response{}, toolproviders.ErrNoResults
}
