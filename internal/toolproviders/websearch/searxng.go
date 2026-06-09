package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// SearxNG is the odd one out: no API key, just an endpoint URL of a
// self-hosted SearxNG instance. Config.Endpoint is required.
type SearxNG struct{}

func (SearxNG) Category() string { return Category }
func (SearxNG) Name() string     { return "searxng" }

func (s *SearxNG) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	endpoint := strings.TrimRight(req.Config.Endpoint, "/")
	if endpoint == "" {
		return toolproviders.Response{}, fmt.Errorf("searxng: missing endpoint")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/search", nil)
	if err != nil {
		return toolproviders.Response{}, err
	}
	q := httpReq.URL.Query()
	q.Set("q", a.Query)
	q.Set("format", "json")
	httpReq.URL.RawQuery = q.Encode()
	// Most SearxNG deployments want a browser-like UA; an empty UA gets 403.
	httpReq.Header.Set("User-Agent", "fastclaw/1.0")

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("searxng request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("searxng", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("searxng decode: %w", err)
	}
	limit := a.Count
	if limit > len(out.Results) {
		limit = len(out.Results)
	}
	items := make([]resultItem, 0, limit)
	for _, r := range out.Results[:limit] {
		items = append(items, resultItem{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	if len(items) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: render(a.Query, items)}, nil
}
