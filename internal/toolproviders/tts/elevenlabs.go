package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// ElevenLabs posts to /v1/text-to-speech/{voice_id} with the API key in the
// non-standard `xi-api-key` header. Model (the suffix in
// "elevenlabs/<model>") defaults to "eleven_multilingual_v2"; Voice defaults
// to the long-standing built-in "Rachel" voice. Response is raw audio/mpeg.
type ElevenLabs struct{}

func (ElevenLabs) Category() string { return Category }
func (ElevenLabs) Name() string     { return "elevenlabs" }

// elevenLabsDefaultVoice is the voice_id used when the caller doesn't pass
// one. "Rachel" is the canonical ElevenLabs sample voice and is available on
// every account tier.
const elevenLabsDefaultVoice = "21m00Tcm4TlvDq8ikWAM"

func (e *ElevenLabs) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("elevenlabs: missing api key")
	}
	model := req.Config.Model
	if model == "" {
		model = "eleven_multilingual_v2"
	}
	voice := a.Voice
	if voice == "" {
		voice = elevenLabsDefaultVoice
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"text":     a.Text,
		"model_id": model,
	}
	buf, _ := json.Marshal(body)
	url := "https://api.elevenlabs.io/v1/text-to-speech/" + voice + "?output_format=mp3_44100_128"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// ElevenLabs uses a custom auth header, not Bearer.
	httpReq.Header.Set("xi-api-key", req.Config.APIKey)
	httpReq.Header.Set("Accept", "audio/mpeg")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("elevenlabs request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("elevenlabs", resp); err != nil {
		return toolproviders.Response{}, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("read elevenlabs audio: %w", err)
	}
	return writeAudio(data, "mp3")
}
