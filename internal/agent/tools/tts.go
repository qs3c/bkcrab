package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// RegisterTTSChain registers the tts tool against a provider chain. Absent
// credentials ⇒ the tool isn't visible to the agent at all.
func RegisterTTSChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil {
		return
	}
	// "none" is a sentinel meaning the admin explicitly opted out of
	// fastclaw's tts. Detected anywhere in the chain → don't register
	// the tool at all so the model falls back to its own native audio
	// capability (or does without).
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
	r.Register("tts", "Convert text to speech. Uses a configurable provider chain (OpenAI tts-1, MiniMax speech-02, …) with automatic fallback. The audio file is attached to the chat message automatically.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"text": map[string]interface{}{
				"type":        "string",
				"description": "Text to synthesize",
			},
			"voice": map[string]interface{}{
				"type":        "string",
				"description": "Voice id (provider-specific; default picked automatically)",
			},
		},
		"required": []string{"text"},
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
