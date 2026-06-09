package webfetch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Firecrawl calls api.firecrawl.dev's /v1/scrape endpoint, which renders
// JS-heavy pages with a headless browser and returns markdown. Requires
// an API key (Bearer). The default formats=["markdown"] keeps responses
// LLM-readable; an admin can flip to "html" via the per-provider Options
// map (key "format") if they want raw HTML to post-process.
type Firecrawl struct{}

func (Firecrawl) Category() string { return Category }
func (Firecrawl) Name() string     { return "firecrawl" }

const (
	firecrawlTimeout = 45 * time.Second
	firecrawlURL     = "https://api.firecrawl.dev/v1/scrape"
)

func (f *Firecrawl) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if strings.TrimSpace(req.Config.APIKey) == "" {
		return toolproviders.Response{}, fmt.Errorf("firecrawl: missing api key")
	}
	ctx, cancel := context.WithTimeout(ctx, firecrawlTimeout)
	defer cancel()

	format := "markdown"
	if v := strings.TrimSpace(req.Config.Options["format"]); v != "" {
		format = v
	}
	body := map[string]any{
		"url":     a.URL,
		"formats": []string{format},
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, firecrawlURL, bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("firecrawl request: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		err := fmt.Errorf("firecrawl HTTP %d: %s", resp.StatusCode, string(raw))
		switch {
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			return toolproviders.Response{}, toolproviders.Retry(err)
		default:
			return toolproviders.Response{}, err
		}
	}
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			Markdown string `json:"markdown"`
			HTML     string `json:"html"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("firecrawl decode: %w", err)
	}
	if !out.Success && out.Error != "" {
		return toolproviders.Response{}, fmt.Errorf("firecrawl: %s", out.Error)
	}
	text := out.Data.Markdown
	if text == "" && out.Data.HTML != "" {
		text = stripHTML(out.Data.HTML)
	}
	if strings.TrimSpace(text) == "" {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: truncate(text, a.MaxLen)}, nil
}
