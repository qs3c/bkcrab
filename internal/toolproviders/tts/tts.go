// Package tts bundles text-to-speech providers. The category returns the
// generated audio as a MEDIA: line per tmp file, so the chat pipeline
// auto-attaches the clip to the assistant's outgoing message.
package tts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bkclaw-ai/bkclaw/internal/toolproviders"
)

// Category is the tool category these providers plug into.
const Category = "tts"

// RegisterAll installs built-in TTS providers in r.
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&OpenAI{})
	r.Register(&MiniMax{})
	r.Register(&ElevenLabs{})
	r.Register(&Fish{})
	r.Register(&None{})
}

type args struct {
	Text  string
	Voice string
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["text"].(string); ok {
		a.Text = strings.TrimSpace(s)
	}
	if a.Text == "" {
		return a, fmt.Errorf("text is required")
	}
	if s, ok := raw["voice"].(string); ok {
		a.Voice = s
	}
	return a, nil
}

// writeAudio dumps audio bytes to a tmp file and returns an LLM-visible
// response. The MEDIA: marker is how loop.go's extractMediaPaths picks files
// up and attaches them to the outbound chat message.
func writeAudio(data []byte, ext string) (toolproviders.Response, error) {
	if len(data) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	if ext == "" {
		ext = "mp3"
	}
	f, err := os.CreateTemp("", "bkclaw-tts-*."+ext)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("create tmp: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return toolproviders.Response{}, fmt.Errorf("write tmp: %w", err)
	}
	f.Close()
	path, _ := filepath.Abs(f.Name())
	// First line is an LLM-visible status; the MEDIA: line is consumed by
	// the loop before the text reaches the model on the next turn.
	text := fmt.Sprintf("Generated audio: %s\nMEDIA:%s", filepath.Base(path), path)
	return toolproviders.Response{Text: text}, nil
}
