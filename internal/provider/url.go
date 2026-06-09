package provider

import (
	"net/url"
	"strings"
)

// NormalizeAPIBase folds the user-typed apiBase into the canonical form
// that downstream code expects to concatenate path segments onto.
//
// Different API types disagree on whether `/v1` is part of the base or
// part of the path:
//
//   - OpenAI Chat Completions: runtime appends "/chat/completions",
//     assuming /v1 is already in the base. A bare host hits 404.
//   - Anthropic Messages: runtime appends "/v1/messages", assuming /v1
//     is NOT in the base. A trailing /v1 produces /v1/v1/messages.
//
// Both forms are common typos (people copy "https://api.openai.com" off
// a doc page, or paste "https://api.anthropic.com/v1" by analogy with
// OpenAI). We fold them into the canonical shape here so the connection
// test, the runtime, and any other consumer all hit the same URL.
//
// The rules are intentionally conservative — we only touch the trailing
// /v1 segment, and only when the user gave us a bare host (no custom
// path). Third-party gateways with their own routing convention
// (e.g. "https://my-gateway.com/openai") are left alone, because we
// can't safely guess where /v1 belongs in their path.
func NormalizeAPIBase(apiBase, apiType string) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		return ""
	}
	switch apiType {
	case "anthropic-messages":
		return strings.TrimSuffix(base, "/v1")
	default:
		u, err := url.Parse(base)
		if err != nil || u.Path != "" {
			return base
		}
		return base + "/v1"
	}
}
