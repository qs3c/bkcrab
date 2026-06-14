package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// SearxNG 是特例：不需要 API 密钥，只需要自托管 SearxNG 实例的端点 URL。
// Config.Endpoint 是必需的。
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
	// 大多数 SearxNG 部署需要类似浏览器的 UA；空的 UA 会返回 403。
	httpReq.Header.Set("User-Agent", "bkclaw/1.0")

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
