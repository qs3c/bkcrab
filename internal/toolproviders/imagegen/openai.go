package imagegen

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

// OpenAI generates via POST /v1/images/generations. Model (the suffix in
// "openai/<model>") defaults to "gpt-image-1" which returns base64; older
// dall-e-3 returns a URL. Both paths are handled transparently.
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
		model = "gpt-image-1"
	}
	size := a.Size
	if size == "" {
		size = "1024x1024"
	}

	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	body := map[string]any{
		"model":  model,
		"prompt": a.Prompt,
		"n":      a.N,
		"size":   size,
	}
	endpoint := "https://api.openai.com/v1/images/generations"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("openai image request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("openai", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Data []struct {
			URL     string `json:"url"`
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("openai decode: %w", err)
	}
	if len(out.Data) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	// Prefer URL if present; otherwise embed b64 inline.
	urls := make([]string, 0, len(out.Data))
	b64s := make([]string, 0, len(out.Data))
	for _, d := range out.Data {
		if d.URL != "" {
			urls = append(urls, d.URL)
		} else if d.B64JSON != "" {
			b64s = append(b64s, d.B64JSON)
		}
	}
	if len(urls) > 0 {
		return toolproviders.Response{Text: renderURLs(a.Prompt, urls)}, nil
	}
	if len(b64s) > 0 {
		return toolproviders.Response{Text: renderB64(a.Prompt, b64s)}, nil
	}
	return toolproviders.Response{}, toolproviders.ErrNoResults
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
