package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Exa 调用 api.exa.ai。使用 x-api-key 头进行认证；请求体控制结果数量和搜索模式。
// Model（来自 "exa/<mode>"）可以是空、"auto"、"keyword" 或 "neural"。
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
		"query":    a.Query,
		"type":     mode,
		"contents": map[string]any{"highlights": true},
	}
	// Preserve the tool's explicit count control; otherwise use Exa's default.
	if _, explicit := req.Args["count"]; explicit {
		body["numResults"] = a.Count
	}
	buf, _ := json.Marshal(body)
	endpoint := "https://api.exa.ai/search"
	if req.Config.Endpoint != "" {
		endpoint = req.Config.Endpoint
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(buf))
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
			Title      string   `json:"title"`
			URL        string   `json:"url"`
			Highlights []string `json:"highlights"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return toolproviders.Response{}, fmt.Errorf("exa decode: %w", err)
	}
	items := make([]resultItem, 0, len(out.Results))
	for _, r := range out.Results {
		items = append(items, resultItem{Title: r.Title, URL: r.URL, Snippet: strings.Join(r.Highlights, "\n")})
	}
	if len(items) == 0 {
		return toolproviders.Response{}, toolproviders.ErrNoResults
	}
	return toolproviders.Response{Text: render(a.Query, items)}, nil
}
