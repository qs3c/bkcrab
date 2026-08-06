package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var RagasMetrics = map[string]struct{}{"context_precision": {}, "context_recall": {}, "faithfulness": {}, "response_relevancy": {}, "factual_correctness": {}}

type EvaluationSample struct {
	CaseID              string   `json:"caseId"`
	UserInput           string   `json:"userInput"`
	RetrievedContexts   []string `json:"retrievedContexts"`
	RetrievedContextIDs []string `json:"retrievedContextIds,omitempty"`
	Response            string   `json:"response"`
	Reference           string   `json:"reference,omitempty"`
	ReferenceContexts   []string `json:"referenceContexts,omitempty"`
}
type EvaluateRequest struct {
	RequestID           string             `json:"requestId"`
	MetricBundleVersion string             `json:"metricBundleVersion"`
	Metrics             []string           `json:"metrics"`
	Samples             []EvaluationSample `json:"samples"`
}
type CaseMetricResults struct {
	CaseID  string                  `json:"caseId"`
	Metrics map[string]MetricResult `json:"metrics"`
}
type EvaluateResponse struct {
	RequestID           string              `json:"requestId"`
	RagasVersion        string              `json:"ragasVersion"`
	MetricBundleVersion string              `json:"metricBundleVersion"`
	Results             []CaseMetricResults `json:"results"`
}

type RagasClient struct {
	endpoint, apiKey                       string
	maxBatch, maxContexts, maxContextBytes int
	client                                 *http.Client
}

func NewRagasClient(endpoint, apiKey string, timeout time.Duration, maxBatch, maxContexts, maxContextBytes int) (*RagasClient, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil, errors.New("evaluator endpoint is required")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &RagasClient{endpoint, apiKey, maxBatch, maxContexts, maxContextBytes, &http.Client{Timeout: timeout}}, nil
}

func (c *RagasClient) Evaluate(ctx context.Context, request EvaluateRequest) (EvaluateResponse, error) {
	var response EvaluateResponse
	if request.RequestID == "" {
		return response, errors.New("requestId is required")
	}
	if request.MetricBundleVersion != "" && request.MetricBundleVersion != MetricBundleV1 {
		return response, errors.New("unsupported metric bundle")
	}
	request.MetricBundleVersion = MetricBundleV1
	if len(request.Samples) == 0 || len(request.Samples) > c.maxBatch {
		return response, errors.New("invalid sample batch size")
	}
	for _, metric := range request.Metrics {
		if _, ok := RagasMetrics[metric]; !ok {
			return response, fmt.Errorf("unsupported metric %q", metric)
		}
	}
	for _, sample := range request.Samples {
		if sample.CaseID == "" || sample.UserInput == "" {
			return response, errors.New("caseId and userInput are required")
		}
		if len(sample.RetrievedContexts) > c.maxContexts {
			return response, errors.New("too many contexts")
		}
		for _, value := range sample.RetrievedContexts {
			if len(value) > c.maxContextBytes {
				return response, errors.New("context exceeds byte limit")
			}
		}
	}
	body, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/evaluate", bytes.NewReader(body))
	if err != nil {
		return response, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	res, err := c.client.Do(req)
	if err != nil {
		return response, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		limited, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return response, fmt.Errorf("evaluator returned %d: %s", res.StatusCode, strings.TrimSpace(string(limited)))
	}
	decoder := json.NewDecoder(io.LimitReader(res.Body, 8<<20))
	if err := decoder.Decode(&response); err != nil {
		return response, err
	}
	if response.RequestID != request.RequestID || response.MetricBundleVersion != MetricBundleV1 {
		return EvaluateResponse{}, errors.New("evaluator response contract mismatch")
	}
	return response, nil
}
