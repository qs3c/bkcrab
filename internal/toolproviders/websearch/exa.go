package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/fastclaw-ai/fastclaw/internal/toolproviders"
)

// Exa calls api.exa.ai. The x-api-key header authenticates; the request body
// controls result count and search mode. Model (from "exa/<mode>") is either
// empty, "auto", "keyword" or "neural".
type Exa struct{}

func (Exa) Category() string { return Category }
func (Exa) Name() string     { return "exa" }

func (e *Exa) Execute(ctx context.Context, req toolproviders.Request) (toolproviders.Response, error) {
	a, err := parseArgs(req.Args)
	if err != nil {
		return toolproviders.Response{}, err
	}
	if req.Config.APIKey == "" {
		return toolproviders.Response{}, fmt.Errorf("exa: missing api key")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	mode := req.Config.Model
	if mode == "" {
		mode = "auto"
	}
	body := map[string]any{
		"query":      a.Query,
		"numResults": a.Count,
		"type":       mode,
	}
	buf, _ := json.Marshal(body)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.exa.ai/search", bytes.NewReader(buf))
	if err != nil {
		return toolproviders.Response{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", req.Config.APIKey)

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return toolproviders.Response{}, toolproviders.Retry(fmt.Errorf("exa request: %w", err))
	}
	defer resp.Body.Close()
	if err := retriableHTTP("exa", resp); err != nil {
		return toolproviders.Response{}, err
	}
	var out struct {
		Results []struct {
			Title string `json:"title"`
			URL   string `json:"url"`
			Text  string `json:"text"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("exa decode: %w", err)
	}
	items := make([]resultItem, 0, len(out.Results))
	for _, r := range out.Results {
		items = append(items, resultItem{Title: r.Title, URL: r.URL, Snippet: truncate(r.Text, 280)})
	}
	if len(items) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: render(a.Query, items)}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
