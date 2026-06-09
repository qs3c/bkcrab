package imagegen

import (
	"context"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// None is a sentinel provider meaning "do not expose image_gen to the model."
// The tool-registration layer (internal/agent/tools/image_gen.go) detects
// "none" anywhere in the chain and skips registering image_gen at all, so the
// model falls back to whatever native image-generation capability it has (or
// does without).
//
// It opts into CredentialFree so chain.Available() reports true when "none"
// is the only configured provider — the dashboard can distinguish "admin made
// an explicit choice" from "forgot to configure anything".
type None struct{}

func (None) Category() string     { return Category }
func (None) Name() string         { return "none" }
func (None) CredentialFree() bool { return true }

// Execute should never be reached: image_gen registration short-circuits on
// "none" before the chain runs. The error is defensive — if someone wires the
// chain a new way and bypasses the skip, the failure should surface loudly
// rather than silently returning empty results.
func (None) Execute(_ context.Context, _ toolproviders.Request) (toolproviders.Response, error) {
	return toolproviders.Response{}, toolproviders.ErrNoResults
}
