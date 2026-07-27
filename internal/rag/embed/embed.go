// Package embed provides a minimal client for OpenAI-compatible embeddings
// endpoints. endpoint is a base URL such as https://api.openai.com/v1.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// The bundled llama.cpp embedding service runs with one parallel slot.
	// Bound both item count and aggregate UTF-8 bytes so punctuation- and
	// number-heavy tables cannot turn a nominal 512-token estimator batch into
	// several thousand provider tokens processed sequentially under one
	// 60-second HTTP deadline. UTF-8 bytes are a conservative upper bound for
	// byte-level tokenizer pieces; a single oversized item is still sent alone.
	batchSize                   = 8
	maxEmbeddingBatchInputBytes = 2 << 10
	maxEmbeddingResponseBytes   = 32 << 20
)

// endpointError preserves the HTTP status through error wrapping so callers
// can distinguish deterministic request failures from retryable provider
// failures without parsing a localized error message.
type endpointError struct {
	statusCode int
	message    string
}

func (e *endpointError) Error() string {
	return fmt.Sprintf("embeddings 端点返回 %d: %s", e.statusCode, e.message)
}

func (e *endpointError) HTTPStatus() int { return e.statusCode }

// Client embeds text with the model and dimensions snapshotted by a knowledge
// base when it is created.
type Client struct {
	endpoint string
	apiKey   string
	model    string
	dims     int
	hc       *http.Client
}

// New constructs an OpenAI-compatible embeddings client.
func New(endpoint, apiKey, model string, dims int) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		apiKey:   apiKey,
		model:    model,
		dims:     dims,
		hc:       &http.Client{Timeout: 60 * time.Second},
	}
}

// Model returns the configured embedding model.
func (c *Client) Model() string { return c.model }

// Dims returns the required output vector dimensions.
func (c *Client) Dims() int { return c.dims }

// Embed embeds texts in count- and byte-bounded batches and preserves input
// order.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); {
		end := embeddingBatchEnd(texts, start)
		batch, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embedding 批次 %d-%d: %w", start, end, err)
		}
		vectors = append(vectors, batch...)
		start = end
	}
	return vectors, nil
}

func embeddingBatchEnd(texts []string, start int) int {
	end, inputBytes := start, 0
	for end < len(texts) && end-start < batchSize {
		nextBytes := len([]byte(texts[end]))
		if end > start && inputBytes+nextBytes > maxEmbeddingBatchInputBytes {
			break
		}
		inputBytes += nextBytes
		end++
	}
	return end
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("编码请求: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.endpoint+"/embeddings", bytes.NewReader(body),
	)
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
		return nil, &endpointError{
			statusCode: response.StatusCode,
			message:    strings.TrimSpace(string(message)),
		}
	}

	var payload struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxEmbeddingResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析响应: %w", err)
	}
	if len(payload.Data) != len(texts) {
		return nil, fmt.Errorf("返回向量数 %d != 输入数 %d", len(payload.Data), len(texts))
	}

	vectors := make([][]float32, len(texts))
	seen := make([]bool, len(texts))
	for _, item := range payload.Data {
		if item.Index < 0 || item.Index >= len(texts) {
			return nil, fmt.Errorf("非法 index %d", item.Index)
		}
		if seen[item.Index] {
			return nil, fmt.Errorf("重复 index %d", item.Index)
		}
		if len(item.Embedding) != c.dims {
			return nil, fmt.Errorf(
				"维度不符: 期望 %d 实际 %d(检查 KB 绑定的模型与端点)",
				c.dims, len(item.Embedding),
			)
		}
		seen[item.Index] = true
		vectors[item.Index] = item.Embedding
	}
	return vectors, nil
}
