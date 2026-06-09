package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Brave calls api.search.brave.com. Requires an X-Subscription-Token API key.
type Brave struct{}

func (Brave) Category() string { return Category }
func (Brave) Name() string     { return "brave" }

func (b *Brave) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("brave: missing api key")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.search.brave.com/res/v1/web/search", nil)
	if err != nil {
		return toolproviders.Response{}, err
	}
	q := httpReq.URL.Query()
	q.Set("q", a.Query)
	q.Set("count", fmt.Sprintf("%d", a.Count))
	httpReq.URL.RawQuery = q.Encode()
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("X-Subscription-Token", req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("brave request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("brave", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return toolproviders.Response{}, fmt.Errorf("brave decode: %w", err)
	}
	items := make([]resultItem, 0, len(body.Web.Results))
	for _, r := range body.Web.Results {
		items = append(items, resultItem{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	if len(items) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: render(a.Query, items)}, nil
}

// retriableHTTP inspects the response status and either returns nil (200),
// a retriable error (network-y codes that deserve a fallback) or a fatal
// error (4xx that signals config problems).
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
