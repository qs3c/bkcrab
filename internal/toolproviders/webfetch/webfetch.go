// Package webfetch bundles built-in web_fetch providers. Every provider takes
// the same {url, max_length} arg shape and returns plain text the LLM can
// read directly. Per-call credentials/endpoint come from the
// toolproviders.Request.Config so providers stay stateless.
package webfetch

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Category is the tool category these providers plug into.
const Category = "web_fetch"

// DefaultMaxLen is the cap when the caller doesn't pass max_length. Mirrors
// the value that the legacy direct-only web_fetch tool used so swapping in
// a chain doesn't change reply truncation behaviour.
const DefaultMaxLen = 10000

// RegisterAll installs every built-in web_fetch provider in r.
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&Direct{})
	r.Register(&Jina{})
	r.Register(&Firecrawl{})
}

type args struct {
	URL    string
	MaxLen int
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["url"].(string); ok {
		a.URL = strings.TrimSpace(s)
	}
	if a.URL == "" {
		return a, fmt.Errorf("url is required")
	}
	switch v := raw["max_length"].(type) {
	case float64:
		a.MaxLen = int(v)
	case int:
		a.MaxLen = v
	}
	if a.MaxLen <= 0 {
		a.MaxLen = DefaultMaxLen
	}
	return a, nil
}

// truncate caps text at maxLen with a visible marker so the LLM knows the
// page was longer than what it received and can ask for a higher cap (or
// pick a more specific URL) instead of treating the cut as authoritative.
func truncate(text string, maxLen int) string {
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "\n[...truncated]"
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// stripHTML removes script/style blocks, drops remaining HTML tags, and
// collapses whitespace. Mirrors the helper that lived in the agent's
// web_fetch tool so the Direct provider produces identical output.
func stripHTML(html string) string {
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	text := htmlTagRe.ReplaceAllString(html, " ")

	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}
