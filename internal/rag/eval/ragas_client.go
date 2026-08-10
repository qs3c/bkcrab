package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

var RagasMetricRequiredFields = map[string][]string{
	"context_precision":   {"userInput", "reference", "retrievedContexts"},
	"context_recall":      {"reference", "retrievedContexts"},
	"faithfulness":        {"response", "retrievedContexts"},
	"response_relevancy":  {"userInput", "response"},
	"factual_correctness": {"response", "reference"},
}

var RagasMetrics = func() map[string]struct{} {
	metrics := make(map[string]struct{}, len(RagasMetricRequiredFields))
	for name := range RagasMetricRequiredFields {
		metrics[name] = struct{}{}
	}
	return metrics
}()

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
	OK                   bool                `json:"ok"`
	ServiceVersion       string              `json:"serviceVersion"`
	ProtocolVersion      string              `json:"protocolVersion"`
	RagasVersion         string              `json:"ragasVersion"`
	MetricBundleVersion  string              `json:"metricBundleVersion"`
	JudgeConfigured      bool                `json:"judgeConfigured"`
	MetricsInitialized   bool                `json:"metricsInitialized"`
	MetricRequiredFields map[string][]string `json:"metricRequiredFields"`
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

func requiredFieldsCompatible(got map[string][]string) bool {
	if len(got) != len(RagasMetricRequiredFields) {
		return false
	}
	for metric, expected := range RagasMetricRequiredFields {
		actual, ok := got[metric]
		if !ok || len(actual) != len(expected) {
			return false
		}
		for i := range expected {
			if actual[i] != expected[i] {
				return false
			}
		}
	}
	return true
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
		health.ProtocolVersion != ExpectedEvaluatorProtocolVersion || health.MetricBundleVersion != MetricBundleV1 ||
		health.RagasVersion != ExpectedRagasVersion || !requiredFieldsCompatible(health.MetricRequiredFields) {
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

type wireMetricResult struct {
	Status MetricStatus `json:"status"`
	Value  *float64     `json:"value"`
	Reason string       `json:"reason"`
}

type wireCaseMetricResults struct {
	CaseID  string                      `json:"caseId"`
	Metrics map[string]wireMetricResult `json:"metrics"`
}

func (result *wireCaseMetricResults) UnmarshalJSON(data []byte) error {
	var envelope struct {
		CaseID  string          `json:"caseId"`
		Metrics json.RawMessage `json:"metrics"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("case result contains trailing JSON")
	}
	metricDecoder := json.NewDecoder(bytes.NewReader(envelope.Metrics))
	metricDecoder.DisallowUnknownFields()
	token, err := metricDecoder.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("case metrics must be an object")
	}
	metrics := map[string]wireMetricResult{}
	for metricDecoder.More() {
		nameToken, err := metricDecoder.Token()
		if err != nil {
			return err
		}
		name, ok := nameToken.(string)
		if !ok {
			return errors.New("metric name must be a string")
		}
		if _, duplicate := metrics[name]; duplicate {
			return fmt.Errorf("duplicate response metric %q", name)
		}
		var metric wireMetricResult
		if err := metricDecoder.Decode(&metric); err != nil {
			return err
		}
		metrics[name] = metric
	}
	if _, err := metricDecoder.Token(); err != nil {
		return err
	}
	if err := metricDecoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("case metrics contain trailing JSON")
	}
	result.CaseID, result.Metrics = envelope.CaseID, metrics
	return nil
}

type wireEvaluateResponse struct {
	RequestID           string                  `json:"requestId"`
	RagasVersion        string                  `json:"ragasVersion"`
	MetricBundleVersion string                  `json:"metricBundleVersion"`
	Results             []wireCaseMetricResults `json:"results"`
}

func (c *RagasClient) validateEvaluateRequest(request *EvaluateRequest) error {
	if strings.TrimSpace(request.RequestID) == "" {
		return errors.New("requestId is required")
	}
	if len(request.RequestID) > 255 {
		return errors.New("requestId exceeds byte limit")
	}
	if request.MetricBundleVersion != "" && request.MetricBundleVersion != MetricBundleV1 {
		return errors.New("unsupported metric bundle")
	}
	request.MetricBundleVersion = MetricBundleV1
	if len(request.Metrics) == 0 || len(request.Metrics) > len(RagasMetrics) {
		return errors.New("invalid metric count")
	}
	seenMetrics := make(map[string]struct{}, len(request.Metrics))
	for _, metric := range request.Metrics {
		if _, ok := RagasMetrics[metric]; !ok {
			return fmt.Errorf("unsupported metric %q", metric)
		}
		if _, duplicate := seenMetrics[metric]; duplicate {
			return fmt.Errorf("duplicate metric %q", metric)
		}
		seenMetrics[metric] = struct{}{}
	}
	if len(request.Samples) == 0 || c.maxBatch <= 0 || len(request.Samples) > c.maxBatch {
		return errors.New("invalid sample batch size")
	}
	seenCases := make(map[string]struct{}, len(request.Samples))
	for _, sample := range request.Samples {
		if strings.TrimSpace(sample.CaseID) == "" || strings.TrimSpace(sample.UserInput) == "" {
			return errors.New("caseId and userInput are required")
		}
		if len(sample.CaseID) > 255 {
			return errors.New("caseId exceeds byte limit")
		}
		if _, duplicate := seenCases[sample.CaseID]; duplicate {
			return fmt.Errorf("duplicate caseId %q", sample.CaseID)
		}
		seenCases[sample.CaseID] = struct{}{}
		if len(sample.RetrievedContexts) > c.maxContexts || len(sample.ReferenceContexts) > c.maxContexts {
			return errors.New("too many contexts")
		}
		if len(sample.RetrievedContextIDs) != len(sample.RetrievedContexts) {
			return errors.New("retrievedContextIds must match retrievedContexts")
		}
		if len(sample.UserInput) > 64<<10 || len(sample.Response) > 256<<10 || len(sample.Reference) > 256<<10 {
			return errors.New("sample text exceeds byte limit")
		}
		for _, value := range append(append([]string(nil), sample.RetrievedContexts...), sample.ReferenceContexts...) {
			if c.maxContextBytes <= 0 || len(value) > c.maxContextBytes {
				return errors.New("context exceeds byte limit")
			}
		}
	}
	return nil
}

func validateWireMetric(result wireMetricResult) (MetricResult, error) {
	if len(result.Reason) > 2048 {
		return MetricResult{}, errors.New("metric reason exceeds byte limit")
	}
	switch result.Status {
	case MetricOK:
		if result.Value == nil || math.IsNaN(*result.Value) || math.IsInf(*result.Value, 0) || *result.Value < 0 || *result.Value > 1 {
			return MetricResult{}, errors.New("ok metric requires a finite score in [0,1]")
		}
	case MetricSkippedMissingInput, MetricError:
		if result.Value != nil {
			return MetricResult{}, errors.New("non-ok metric must not contain a score")
		}
	default:
		return MetricResult{}, fmt.Errorf("unknown metric status %q", result.Status)
	}
	return MetricResult{Status: result.Status, Value: result.Value, Reason: result.Reason}, nil
}

func validateWireResponse(request EvaluateRequest, wire wireEvaluateResponse) (EvaluateResponse, error) {
	if wire.RequestID != request.RequestID || wire.MetricBundleVersion != MetricBundleV1 || wire.RagasVersion != ExpectedRagasVersion {
		return EvaluateResponse{}, errors.New("evaluator response contract mismatch")
	}
	expectedCases := make(map[string]struct{}, len(request.Samples))
	for _, sample := range request.Samples {
		expectedCases[sample.CaseID] = struct{}{}
	}
	expectedMetrics := make(map[string]struct{}, len(request.Metrics))
	for _, metric := range request.Metrics {
		expectedMetrics[metric] = struct{}{}
	}
	seenCases := make(map[string]struct{}, len(wire.Results))
	response := EvaluateResponse{RequestID: wire.RequestID, RagasVersion: wire.RagasVersion, MetricBundleVersion: wire.MetricBundleVersion}
	for _, item := range wire.Results {
		if _, ok := expectedCases[item.CaseID]; !ok {
			return EvaluateResponse{}, fmt.Errorf("unknown response caseId %q", item.CaseID)
		}
		if _, duplicate := seenCases[item.CaseID]; duplicate {
			return EvaluateResponse{}, fmt.Errorf("duplicate response caseId %q", item.CaseID)
		}
		seenCases[item.CaseID] = struct{}{}
		if len(item.Metrics) != len(expectedMetrics) {
			return EvaluateResponse{}, fmt.Errorf("case %q returned incomplete metrics", item.CaseID)
		}
		metrics := make(map[string]MetricResult, len(item.Metrics))
		for name, wireMetric := range item.Metrics {
			if _, ok := expectedMetrics[name]; !ok {
				return EvaluateResponse{}, fmt.Errorf("unknown response metric %q", name)
			}
			metric, err := validateWireMetric(wireMetric)
			if err != nil {
				return EvaluateResponse{}, fmt.Errorf("metric %q: %w", name, err)
			}
			metrics[name] = metric
		}
		response.Results = append(response.Results, CaseMetricResults{CaseID: item.CaseID, Metrics: metrics})
	}
	if len(seenCases) != len(expectedCases) {
		return EvaluateResponse{}, errors.New("evaluator response omitted cases")
	}
	return response, nil
}

func (c *RagasClient) Evaluate(ctx context.Context, request EvaluateRequest) (EvaluateResponse, error) {
	if c == nil {
		return EvaluateResponse{}, errors.New("evaluator client is not configured")
	}
	if err := c.validateEvaluateRequest(&request); err != nil {
		return EvaluateResponse{}, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return EvaluateResponse{}, err
	}
	const maxRequestBytes = 4 << 20
	if len(body) > maxRequestBytes {
		return EvaluateResponse{}, errors.New("evaluator request exceeds 4 MiB")
	}
	var res *http.Response
	for attempt := 0; attempt < 2; attempt++ {
		req, requestErr := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/v1/evaluate", bytes.NewReader(body))
		if requestErr != nil {
			return EvaluateResponse{}, requestErr
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		res, err = c.client.Do(req)
		if err == nil {
			break
		}
		if res != nil || ctx.Err() != nil || attempt == 1 {
			return EvaluateResponse{}, err
		}
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return EvaluateResponse{}, fmt.Errorf("evaluator returned HTTP %d", res.StatusCode)
	}
	const maxResponseBytes = int64(8 << 20)
	responseBody, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil {
		return EvaluateResponse{}, err
	}
	if int64(len(responseBody)) > maxResponseBytes {
		return EvaluateResponse{}, errors.New("evaluator response exceeds 8 MiB")
	}
	var wire wireEvaluateResponse
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&wire); err != nil {
		return EvaluateResponse{}, fmt.Errorf("decode evaluator response: %w", err)
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return EvaluateResponse{}, errors.New("evaluator response contains trailing JSON")
	}
	return validateWireResponse(request, wire)
}
