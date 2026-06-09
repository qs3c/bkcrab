package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// OpenAI posts to /v1/audio/speech. Model (e.g. "openai/tts-1") defaults to
// "tts-1". Voice defaults to "alloy".
type OpenAI struct{}

func (OpenAI) Category() string { return Category }
func (OpenAI) Name() string     { return "openai" }

func (o *OpenAI) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("openai: missing api key")
	}
	model := req.Config.Model
	if model == "" {
		model = "tts-1"
	}
	voice := a.Voice
	if voice == "" {
		voice = "alloy"
	}
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"model":  model,
		"input":  a.Text,
		"voice":  voice,
		"format": "mp3",
	}
	buf, _ := json.Marshal(body)
	endpoint := "https://api.openai.com/v1/audio/speech"
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
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("openai tts: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("openai", resp); err != nil {
		return toolproviders.Response{}, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return toolproviders.Response{}, fmt.Errorf("read openai audio: %w", err)
	}
	return writeAudio(data, "mp3")
}

func retriableHTTP(name string, resp *http.Response) error {
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	err := fmt.Errorf("%s HTTP %d: %s", name, resp.StatusCode, string(body))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests,
		resp.StatusCode == http.StatusRequestTimeout,
		resp.StatusCode >= 500:
		return toolproviders.Retry(err)
	default:
		return err
	}
}
