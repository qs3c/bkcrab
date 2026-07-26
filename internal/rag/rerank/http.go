package rerank

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const maxResponseBytes = 4 << 20

// HTTPClient calls the Jina-compatible rerank API implemented by llama.cpp,
// including its /rerank, /reranking, /v1/rerank and /v1/reranking aliases.
type HTTPClient struct {
	endpoint string
	apiKey   string
	model    string
	hc       *http.Client
}

// NewHTTP constructs a reranker client. endpoint may be a base URL such as
// http://qwen3-reranker:8080/v1 or a complete rerank endpoint.
func NewHTTP(endpoint, apiKey, model string, timeout time.Duration) (*HTTPClient, error) {
	resolved, err := rerankEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &HTTPClient{
		endpoint: resolved,
		apiKey:   strings.TrimSpace(apiKey),
		model:    strings.TrimSpace(model),
		hc:       &http.Client{Timeout: timeout},
	}, nil
}

func rerankEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("reranker endpoint 无效: %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("reranker endpoint 只支持 http/https: %q", raw)
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(path, "/rerank") && !strings.HasSuffix(path, "/reranking") {
		path += "/rerank"
	}
	parsed.Path = path
	return parsed.String(), nil
}

func (c *HTTPClient) Rerank(ctx context.Context, query string, documents []string, topN int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("reranker query 不能为空")
	}
	if len(documents) == 0 || topN <= 0 {
		return []Result{}, nil
	}
	topN = min(topN, len(documents))

	body, err := json.Marshal(struct {
		Model     string   `json:"model,omitempty"`
		Query     string   `json:"query"`
		Documents []string `json:"documents"`
		TopN      int      `json:"top_n"`
	}{
		Model:     c.model,
		Query:     query,
		Documents: documents,
		TopN:      topN,
	})
	if err != nil {
		return nil, fmt.Errorf("编码 reranker 请求: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		request.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	response, err := c.hc.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("reranker 端点返回 %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	var payload struct {
		Results []struct {
			Index          *int     `json:"index"`
			RelevanceScore *float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析 reranker 响应: %w", err)
	}
	if len(payload.Results) == 0 {
		return nil, fmt.Errorf("reranker 返回空结果")
	}

	seen := make(map[int]struct{}, len(payload.Results))
	results := make([]Result, 0, min(topN, len(payload.Results)))
	for _, item := range payload.Results {
		if item.Index == nil || item.RelevanceScore == nil {
			return nil, fmt.Errorf("reranker 结果缺少 index 或 relevance_score")
		}
		index, score := *item.Index, *item.RelevanceScore
		if index < 0 || index >= len(documents) {
			return nil, fmt.Errorf("reranker 返回非法 index %d", index)
		}
		if _, exists := seen[index]; exists {
			return nil, fmt.Errorf("reranker 返回重复 index %d", index)
		}
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > 1 {
			return nil, fmt.Errorf("reranker 返回非法分数 %v", score)
		}
		seen[index] = struct{}{}
		results = append(results, Result{Index: index, Score: score})
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Index < results[j].Index
	})
	if len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}

var _ Reranker = (*HTTPClient)(nil)
