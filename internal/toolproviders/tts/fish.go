package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bkclaw-ai/bkclaw/internal/toolproviders"
)

// Fish posts to https://api.fish.audio/v1/tts (a.k.a. fish.studio). Auth is
// `Authorization: Bearer <token>`. Voice (the LLM-provided `voice` arg) maps
// to the `reference_id` field — Fish exposes voices as cloned-voice IDs the
// admin/user picks from their dashboard. When omitted, the request omits the
// field too and Fish picks a built-in voice.
type Fish struct{}

func (Fish) Category() string { return Category }
func (Fish) Name() string     { return "fish" }

func (f *Fish) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("fish: missing api key")
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"text":   a.Text,
		"format": "mp3",
	}
	if a.Voice != "" {
		body["reference_id"] = a.Voice
	}
	// Model (suffix in "fish/<model>") selects the synthesis backend (s1 /
	// speech-1.5 / etc.). When set, pass it through as `backend`; default
	// is "s1" — left unset so Fish picks its own current default.
	if req.Config.Model != "" {
		body["backend"] = req.Config.Model
	}
	buf, _ := json.Marshal(body)

	endpoint := "https://api.fish.audio/v1/tts"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("fish request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("fish", resp); err != nil {
		return toolproviders.Response{}, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("read fish audio: %w", err)
	}
	return writeAudio(data, "mp3")
}
