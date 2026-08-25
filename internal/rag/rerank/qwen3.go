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
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	qwen3Instruction = "Given a web search query, retrieve relevant passages that answer the query"
	qwen3System      = `Judge whether the Document meets the requirements based on the Query and the Instruct provided. Note that the answer can only be "yes" or "no".`
	qwen3LogitBias   = 100.0
)

// Qwen3HTTPClient adapts Qwen3-Reranker's generative yes/no contract to the
// Reranker interface. It deliberately uses llama.cpp's native /completion and
// /tokenize endpoints instead of the pooling-based /rerank endpoint: the GGUF
// is a causal language model and has no classification pooling head.
type Qwen3HTTPClient struct {
	completionEndpoint string
	tokenizeEndpoint   string
	apiKey             string
	timeout            time.Duration
	hc                 *http.Client
	slots              chan struct{}

	tokenMu     sync.Mutex
	tokensReady bool
	yesTokenID  int
	noTokenID   int
}

// NewQwen3HTTP constructs the llama.cpp-native Qwen3 reranker adapter.
// concurrency is a client-wide limit and should match the server's --parallel
// slot count so concurrent evaluation cases cannot overload the model queue.
func NewQwen3HTTP(endpoint, apiKey string, timeout time.Duration, concurrency int) (*Qwen3HTTPClient, error) {
	completionEndpoint, tokenizeEndpoint, err := qwen3Endpoints(endpoint)
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	return &Qwen3HTTPClient{
		completionEndpoint: completionEndpoint,
		tokenizeEndpoint:   tokenizeEndpoint,
		apiKey:             strings.TrimSpace(apiKey),
		timeout:            timeout,
		hc:                 &http.Client{},
		slots:              make(chan struct{}, concurrency),
	}, nil
}

func qwen3Endpoints(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", fmt.Errorf("reranker endpoint 无效: %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", "", fmt.Errorf("reranker endpoint 只支持 http/https: %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", "", fmt.Errorf("reranker endpoint 不能包含凭据、查询参数或 fragment: %q", raw)
	}

	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{
		"/v1/reranking", "/v1/rerank", "/v1/completions", "/v1/completion",
		"/reranking", "/rerank", "/completion", "/tokenize", "/v1",
	} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	base := *parsed
	base.Path = strings.TrimRight(path, "/") + "/completion"
	completionEndpoint := base.String()
	base.Path = strings.TrimRight(path, "/") + "/tokenize"
	return completionEndpoint, base.String(), nil
}

func (c *Qwen3HTTPClient) Rerank(ctx context.Context, query string, documents []string, topN int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("reranker query 不能为空")
	}
	if len(documents) == 0 || topN <= 0 {
		return []Result{}, nil
	}
	topN = min(topN, len(documents))

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	yesTokenID, noTokenID, err := c.tokenIDs(ctx)
	if err != nil {
		return nil, err
	}

	results := make([]Result, len(documents))
	group, groupCtx := errgroup.WithContext(ctx)
	for index, document := range documents {
		index, document := index, document
		group.Go(func() error {
			select {
			case c.slots <- struct{}{}:
				defer func() { <-c.slots }()
			case <-groupCtx.Done():
				return groupCtx.Err()
			}
			score, err := c.score(groupCtx, yesTokenID, noTokenID, query, document)
			if err != nil {
				return fmt.Errorf("reranker 文档 %d: %w", index, err)
			}
			results[index] = Result{Index: index, Score: score}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].Index < results[j].Index
	})
	return results[:topN], nil
}

func (c *Qwen3HTTPClient) tokenIDs(ctx context.Context) (int, int, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.tokensReady {
		return c.yesTokenID, c.noTokenID, nil
	}
	yesTokenID, err := c.tokenID(ctx, "yes")
	if err != nil {
		return 0, 0, err
	}
	noTokenID, err := c.tokenID(ctx, "no")
	if err != nil {
		return 0, 0, err
	}
	if yesTokenID == noTokenID {
		return 0, 0, fmt.Errorf("reranker tokenizer 为 yes/no 返回了相同 token id %d", yesTokenID)
	}
	c.yesTokenID, c.noTokenID, c.tokensReady = yesTokenID, noTokenID, true
	return yesTokenID, noTokenID, nil
}

func (c *Qwen3HTTPClient) tokenID(ctx context.Context, label string) (int, error) {
	body, err := json.Marshal(struct {
		Content    string `json:"content"`
		AddSpecial bool   `json:"add_special"`
		WithPieces bool   `json:"with_pieces"`
	}{Content: label, WithPieces: true})
	if err != nil {
		return 0, err
	}
	response, err := c.post(ctx, c.tokenizeEndpoint, body)
	if err != nil {
		return 0, fmt.Errorf("tokenize %q: %w", label, err)
	}
	defer response.Body.Close()
	var payload struct {
		Tokens []struct {
			ID    *int   `json:"id"`
			Piece string `json:"piece"`
		} `json:"tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return 0, fmt.Errorf("解析 tokenizer 响应: %w", err)
	}
	if len(payload.Tokens) != 1 || payload.Tokens[0].ID == nil || payload.Tokens[0].Piece != label {
		return 0, fmt.Errorf("标签 %q 必须是一个完全匹配的 token，响应为 %+v", label, payload.Tokens)
	}
	return *payload.Tokens[0].ID, nil
}

func (c *Qwen3HTTPClient) score(ctx context.Context, yesTokenID, noTokenID int, query, document string) (float64, error) {
	prompt := qwen3Prompt(query, document)
	body, err := json.Marshal(struct {
		Prompt            string       `json:"prompt"`
		NPredict          int          `json:"n_predict"`
		Temperature       float64      `json:"temperature"`
		TopK              int          `json:"top_k"`
		TopP              float64      `json:"top_p"`
		MinP              float64      `json:"min_p"`
		NProbs            int          `json:"n_probs"`
		PostSamplingProbs bool         `json:"post_sampling_probs"`
		Samplers          []string     `json:"samplers"`
		LogitBias         [][2]float64 `json:"logit_bias"`
		CachePrompt       bool         `json:"cache_prompt"`
	}{
		Prompt:            prompt,
		NPredict:          1,
		Temperature:       1,
		TopK:              0,
		TopP:              1,
		MinP:              0,
		NProbs:            2,
		PostSamplingProbs: true,
		Samplers:          []string{"temperature"},
		LogitBias:         [][2]float64{{float64(yesTokenID), qwen3LogitBias}, {float64(noTokenID), qwen3LogitBias}},
		CachePrompt:       true,
	})
	if err != nil {
		return 0, fmt.Errorf("编码 completion 请求: %w", err)
	}
	response, err := c.post(ctx, c.completionEndpoint, body)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	var payload struct {
		CompletionProbabilities []struct {
			TopProbs []struct {
				ID   *int     `json:"id"`
				Prob *float64 `json:"prob"`
			} `json:"top_probs"`
		} `json:"completion_probabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return 0, fmt.Errorf("解析 completion 响应: %w", err)
	}
	if len(payload.CompletionProbabilities) != 1 {
		return 0, fmt.Errorf("completion 必须返回一个 token 的概率，实际为 %d", len(payload.CompletionProbabilities))
	}
	var yesProbability, noProbability *float64
	for _, candidate := range payload.CompletionProbabilities[0].TopProbs {
		if candidate.ID == nil || candidate.Prob == nil {
			continue
		}
		switch *candidate.ID {
		case yesTokenID:
			yesProbability = candidate.Prob
		case noTokenID:
			noProbability = candidate.Prob
		}
	}
	if yesProbability == nil || noProbability == nil {
		return 0, fmt.Errorf("completion top_probs 缺少精确 yes/no token（yes=%d no=%d）", yesTokenID, noTokenID)
	}
	yes, no := *yesProbability, *noProbability
	if math.IsNaN(yes) || math.IsInf(yes, 0) || math.IsNaN(no) || math.IsInf(no, 0) ||
		yes < 0 || no < 0 || yes > 1 || no > 1 || yes+no <= 0 {
		return 0, fmt.Errorf("completion 返回非法 yes/no 概率 yes=%v no=%v", yes, no)
	}
	return yes / (yes + no), nil
}

func (c *Qwen3HTTPClient) post(ctx context.Context, endpoint string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
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
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("reranker 端点返回 %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return response, nil
}

func qwen3Prompt(query, document string) string {
	query = escapeQwen3ControlTokens(strings.TrimSpace(query))
	document = escapeQwen3ControlTokens(strings.TrimSpace(document))
	return "<|im_start|>system\n" + qwen3System + "<|im_end|>\n" +
		"<|im_start|>user\n<Instruct>: " + qwen3Instruction +
		"\n<Query>: " + query + "\n<Document>: " + document + "<|im_end|>\n" +
		"<|im_start|>assistant\n<think>\n\n</think>\n\n"
}

func escapeQwen3ControlTokens(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "<|", "<\u200b|"), "|>", "|\u200b>")
}

var _ Reranker = (*Qwen3HTTPClient)(nil)
