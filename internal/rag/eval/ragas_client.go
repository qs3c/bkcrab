package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
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
	healthTTL, healthProbeInterval         time.Duration
	now                                    func() time.Time
	healthMu                               sync.RWMutex
	health                                 config.RAGEvaluatorHealthSnapshot
	probeMu                                sync.Mutex
	startOnce                              sync.Once
}

func NewRagasClient(endpoint, apiKey string, timeout time.Duration, maxBatch, maxContexts, maxContextBytes int) (*RagasClient, error) {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("valid evaluator http(s) endpoint is required")
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &RagasClient{
		endpoint: endpoint, apiKey: apiKey, maxBatch: maxBatch, maxContexts: maxContexts,
		maxContextBytes: maxContextBytes, client: client, healthTTL: 30 * time.Second,
		healthProbeInterval: 15 * time.Second, now: time.Now,
	}, nil
}

type evaluatorHealth struct {
	OK                  bool   `json:"ok"`
	ServiceVersion      string `json:"serviceVersion"`
	ProtocolVersion     string `json:"protocolVersion"`
	RagasVersion        string `json:"ragasVersion"`
	MetricBundleVersion string `json:"metricBundleVersion"`
	JudgeConfigured     bool   `json:"judgeConfigured"`
	MetricsInitialized  bool   `json:"metricsInitialized"`
}

func (c *RagasClient) healthURL() string {
	base, _ := url.Parse(c.endpoint)
	base.Path = path.Join(base.Path, "/healthz")
	return base.String()
}

func (c *RagasClient) HealthSnapshot() config.RAGEvaluatorHealthSnapshot {
	if c == nil {
		return config.RAGEvaluatorHealthSnapshot{}
	}
	c.healthMu.RLock()
	snapshot := c.health
	c.healthMu.RUnlock()
	return snapshot
}

func (c *RagasClient) storeHealth(snapshot config.RAGEvaluatorHealthSnapshot) config.RAGEvaluatorHealthSnapshot {
	c.healthMu.Lock()
	c.health = snapshot
	c.healthMu.Unlock()
	return snapshot
}

func (c *RagasClient) healthFailure(now time.Time, reason string) config.RAGEvaluatorHealthSnapshot {
	return c.storeHealth(config.RAGEvaluatorHealthSnapshot{
		Healthy: false, Reason: reason, CheckedAt: now, ExpiresAt: now.Add(c.healthTTL),
	})
}

// ProbeHealth is called only by the background loop (and tests), never by an
// HTTP capability handler. It validates compatibility without calling a judge.
func (c *RagasClient) ProbeHealth(ctx context.Context) (config.RAGEvaluatorHealthSnapshot, error) {
	if c == nil {
		return config.RAGEvaluatorHealthSnapshot{}, errors.New("evaluator client is not configured")
	}
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	now := c.now().UTC()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.healthURL(), nil)
	if err != nil {
		return c.healthFailure(now, "evaluator_health_unavailable"), err
	}
	res, err := c.client.Do(req)
	if err != nil {
		reason := "evaluator_health_unavailable"
		if ctx.Err() != nil {
			err, reason = ctx.Err(), "evaluator_health_timeout"
		}
		return c.healthFailure(now, reason), err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		err = fmt.Errorf("evaluator health returned %d", res.StatusCode)
		return c.healthFailure(now, "evaluator_health_http_"+strconv.Itoa(res.StatusCode)), err
	}
	const maxHealthBytes = int64(64 << 10)
	body, err := io.ReadAll(io.LimitReader(res.Body, maxHealthBytes+1))
	if err != nil || int64(len(body)) > maxHealthBytes {
		if err == nil {
			err = errors.New("evaluator health response exceeds 64 KiB")
		}
		return c.healthFailure(now, "evaluator_health_protocol_invalid"), err
	}
	var health evaluatorHealth
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&health); err != nil || !health.OK || !health.MetricsInitialized ||
		health.ProtocolVersion != "rag-evaluator-v1" || health.MetricBundleVersion != MetricBundleV1 ||
		strings.TrimSpace(health.RagasVersion) == "" {
		if err == nil {
			err = errors.New("incompatible evaluator health response")
		}
		return c.healthFailure(now, "evaluator_health_protocol_invalid"), err
	}
	snapshot := config.RAGEvaluatorHealthSnapshot{
		Healthy: true, ServiceVersion: health.ServiceVersion, ProtocolVersion: health.ProtocolVersion,
		RagasVersion: health.RagasVersion, MetricBundleVersion: health.MetricBundleVersion,
		JudgeConfigured: health.JudgeConfigured, CheckedAt: now, ExpiresAt: now.Add(c.healthTTL),
	}
	return c.storeHealth(snapshot), nil
}

func (c *RagasClient) StartHealthProbe(ctx context.Context) {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		go func() {
			c.probeHealthWithTimeout(ctx)
			ticker := time.NewTicker(c.healthProbeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.probeHealthWithTimeout(ctx)
				}
			}
		}()
	})
}

func (c *RagasClient) probeHealthWithTimeout(ctx context.Context) {
	timeout := c.client.Timeout
	if timeout <= 0 || timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = c.ProbeHealth(probeCtx)
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
