// Package imagegen bundles built-in image_gen providers. Providers accept a
// prompt (+ optional size/n) and return an LLM-visible text payload that
// embeds either inline base64 image data or a remote URL — whichever the
// upstream gave us. The chat UI renders markdown image tags inline.
package imagegen

import (
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// Category is the tool category these providers plug into.
const Category = "image_gen"

// RegisterAll installs every built-in image_gen provider in r.
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&OpenAI{})
	r.Register(&Fal{})
	r.Register(&Replicate{})
	r.Register(&None{})
}

type args struct {
	Prompt string
	Size   string
	N      int
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["prompt"].(string); ok {
		a.Prompt = strings.TrimSpace(s)
	}
	if a.Prompt == "" {
		return a, fmt.Errorf("prompt is required")
	}
	if s, ok := raw["size"].(string); ok {
		a.Size = s
	}
	switch v := raw["n"].(type) {
	case float64:
		a.N = int(v)
	case int:
		a.N = v
	}
	if a.N <= 0 {
		a.N = 1
	}
	if a.N > 4 {
		a.N = 4
	}
	return a, nil
}

// renderURLs builds a LLM-visible response from a list of image URLs. Each
// URL is emitted as a markdown image tag so the chat UI renders it inline
// without the model having to know about markdown quirks.
func renderURLs(prompt string, urls []string) string {
	if len(urls) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Generated %d image(s) for: %s\n\n", len(urls), prompt)
	for i, u := range urls {
		fmt.Fprintf(&sb, "%d. ![image %d](%s)\n", i+1, i+1, u)
	}
	return sb.String()
}

// renderB64 emits base64 images inline. Used when the provider returns raw
// bytes (e.g. gpt-image-1 with response_format=b64_json).
func renderB64(prompt string, b64s []string) string {
	if len(b64s) == 0 {
		return ""
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Generated %d image(s) for: %s\n\n", len(b64s), prompt)
	for i, b := range b64s {
		fmt.Fprintf(&sb, "%d. ![image %d](data:image/png;base64,%s)\n", i+1, i+1, b)
	}
	return sb.String()
}
