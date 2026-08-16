package eval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/telemetry"
)

func ragasRequest(metrics ...string) EvaluateRequest {
	return EvaluateRequest{OwnerID: "admin", RequestID: "request-1", Metrics: metrics, Samples: []EvaluationSample{{
		CaseID: "case-1", UserInput: "question", RetrievedContexts: []string{"context"},
		RetrievedContextIDs: []string{"chunk-1"}, Response: "answer", Reference: "reference",
	}}}
}

func ragasResponse(metrics string) string {
	return `{"requestId":"request-1","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","results":[{"caseId":"case-1","metrics":` + metrics + `}],"usage":{"llmInputTokens":11,"llmOutputTokens":7,"llmEstimatedCostUsd":0.003,"embeddingInputTokens":5,"embeddingEstimatedCostUsd":0.0001}}`
}

func clientForServer(t *testing.T, handler http.Handler, timeout time.Duration) (*RagasClient, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	client, err := NewRagasClient(server.URL, "secret", timeout, 16, 20, 64<<10)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return client, server
}

func TestRagasAcceptsPartialMetricStatuses(t *testing.T) {
	client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing evaluator bearer secret")
		}
		_, _ = w.Write([]byte(ragasResponse(`{"faithfulness":{"status":"ok","value":0.75,"reason":""},"response_relevancy":{"status":"error","value":null,"reason":"judge unavailable"}}`)))
	}), time.Second)
	defer server.Close()
	response, err := client.Evaluate(context.Background(), ragasRequest("faithfulness", "response_relevancy"))
	if err != nil || response.Results[0].Metrics["response_relevancy"].Status != MetricError || response.Usage.LLMInputTokens != 11 || response.Usage.EmbeddingInputTokens != 5 {
		t.Fatalf("partial metric response rejected: response=%+v err=%v", response, err)
	}
}

func TestRagasTelemetryIsBoundedAndSeparatesSidecarFromJudge(t *testing.T) {
	client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(ragasResponse(`{"faithfulness":{"status":"ok","value":1,"reason":""}}`)))
	}), time.Second)
	defer server.Close()
	var events []telemetry.Event
	client.SetRecorder(telemetry.RecorderFunc(func(_ context.Context, event telemetry.Event) { events = append(events, event) }))
	if _, err := client.Evaluate(context.Background(), ragasRequest("faithfulness")); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Name != telemetry.EventEvalStage || events[0].Fields.Operation != "eval_sidecar" ||
		events[1].Fields.Operation != "eval_judge" || events[0].Fields.RunID != "request-1" || events[0].Fields.ItemCount != 1 {
		t.Fatalf("events=%+v", events)
	}
}

func TestRagasDoesNotRetryHTTPFailures(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
			}), time.Second)
			defer server.Close()
			_, err := client.Evaluate(context.Background(), ragasRequest("faithfulness"))
			if err == nil || calls.Load() != 1 {
				t.Fatalf("HTTP %d should fail without retry: calls=%d err=%v", status, calls.Load(), err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestRagasRetriesOneSafeNetworkFailureWithSameRequest(t *testing.T) {
	var serverCalls atomic.Int32
	client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		serverCalls.Add(1)
		_, _ = w.Write([]byte(ragasResponse(`{"faithfulness":{"status":"ok","value":1,"reason":""}}`)))
	}), time.Second)
	defer server.Close()
	transport := http.DefaultTransport
	var attempts atomic.Int32
	client.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("temporary connection reset")
		}
		return transport.RoundTrip(request)
	})
	if _, err := client.Evaluate(context.Background(), ragasRequest("faithfulness")); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 || serverCalls.Load() != 1 {
		t.Fatalf("bounded retry mismatch: attempts=%d calls=%d", attempts.Load(), serverCalls.Load())
	}
}

func TestRagasTimeoutAndTruncatedResponse(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		client, server := clientForServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}), 10*time.Millisecond)
		defer server.Close()
		if _, err := client.Evaluate(context.Background(), ragasRequest("faithfulness")); err == nil {
			t.Fatal("timeout accepted")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", (8<<20)+1)))
		}), time.Second)
		defer server.Close()
		if _, err := client.Evaluate(context.Background(), ragasRequest("faithfulness")); err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("oversized response error=%v", err)
		}
	})
}

func TestRagasRejectsDuplicateAndUnknownCasesAndMetrics(t *testing.T) {
	request := ragasRequest("faithfulness")
	request.Metrics = append(request.Metrics, "faithfulness")
	client, server := clientForServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), time.Second)
	if _, err := client.Evaluate(context.Background(), request); err == nil {
		t.Fatal("duplicate request metric accepted")
	}
	server.Close()

	responses := map[string]string{
		"duplicate case":   `{"requestId":"request-1","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","results":[{"caseId":"case-1","metrics":{"faithfulness":{"status":"ok","value":1,"reason":""}}},{"caseId":"case-1","metrics":{"faithfulness":{"status":"ok","value":1,"reason":""}}}]}`,
		"unknown case":     `{"requestId":"request-1","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","results":[{"caseId":"other","metrics":{"faithfulness":{"status":"ok","value":1,"reason":""}}}]}`,
		"unknown metric":   ragasResponse(`{"other":{"status":"ok","value":1,"reason":""}}`),
		"duplicate metric": ragasResponse(`{"faithfulness":{"status":"ok","value":1,"reason":""},"faithfulness":{"status":"ok","value":1,"reason":""}}`),
	}
	for name, body := range responses {
		t.Run(name, func(t *testing.T) {
			client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), time.Second)
			defer server.Close()
			if _, err := client.Evaluate(context.Background(), ragasRequest("faithfulness")); err == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
}

func TestRagasRejectsInvalidMetricStatusAndScores(t *testing.T) {
	values := []wireMetricResult{
		{Status: "unknown"},
		{Status: MetricOK},
		{Status: MetricOK, Value: score(-0.1)},
		{Status: MetricOK, Value: score(1.1)},
		{Status: MetricOK, Value: score(math.NaN())},
		{Status: MetricOK, Value: score(math.Inf(1))},
		{Status: MetricError, Value: score(0)},
	}
	for _, value := range values {
		if _, err := validateWireMetric(value); err == nil {
			t.Fatalf("invalid metric accepted: %+v", value)
		}
	}
}

func TestRagasResponseExactDecode(t *testing.T) {
	responses := map[string]string{
		"unknown top field":    `{"requestId":"request-1","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","results":[],"extra":true}`,
		"unknown metric field": ragasResponse(`{"faithfulness":{"status":"ok","value":1,"reason":"","extra":true}}`),
		"trailing json":        ragasResponse(`{"faithfulness":{"status":"ok","value":1,"reason":""}}`) + `{}`,
		"missing case":         `{"requestId":"request-1","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","results":[]}`,
		"wrong ragas":          `{"requestId":"request-1","ragasVersion":"9.9.9","metricBundleVersion":"rag-core-v1","results":[]}`,
	}
	for name, body := range responses {
		t.Run(name, func(t *testing.T) {
			client, server := clientForServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }), time.Second)
			defer server.Close()
			if _, err := client.Evaluate(context.Background(), ragasRequest("faithfulness")); err == nil {
				t.Fatal("non-exact response accepted")
			}
		})
	}
}

func TestRagasConfiguredEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"", "ftp://example.com", "http://user:pass@example.com", "http://example.com?a=b", "http://example.com/#fragment"} {
		if _, err := NewRagasClient(endpoint, "", time.Second, 1, 1, 1); err == nil {
			t.Fatalf("unsafe endpoint accepted: %q", endpoint)
		}
	}
}
