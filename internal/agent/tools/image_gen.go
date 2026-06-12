package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// RegisterImageGenChain registers the image_gen tool against a provider
// chain. Only registered when at least one provider in the chain has
// credentials configured — so an agent without image-gen keys doesn't see
// a tool it can't use.
func RegisterImageGenChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil {
		return
	}
	// "none" is a sentinel meaning the admin explicitly opted out of
	// bkclaw's image_gen. Detected anywhere in the chain → don't
	// register the tool at all so the model falls back to its own
	// native image-generation capability (or does without).
	for _, ref := range chain.Order {
		name := ref
		if i := strings.IndexByte(ref, '/'); i >= 0 {
			name = ref[:i]
		}
		if name == "none" {
			return
		}
	}
	if !chain.Available() {
		return
	}
	r.Register("image_gen", "Generate images from a text prompt. Uses a configurable provider chain (OpenAI gpt-image-1, fal flux, …) with automatic fallback. Returns markdown image tags that render inline in chat.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"prompt": map[string]interface{}{
				"type":        "string",
				"description": "Description of the image to generate",
			},
			"size": map[string]interface{}{
				"type":        "string",
				"description": "Image size (e.g. 1024x1024). Provider-specific.",
			},
			"n": map[string]interface{}{
				"type":        "integer",
				"description": "How many variations (default 1, max 4)",
			},
		},
		"required": []string{"prompt"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args map[string]any
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		resp, err := chain.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}
