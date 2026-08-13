package eval

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRagasHealthProbeCachesCompatibleSnapshot(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
			t.Fatalf("unexpected health request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true,"serviceVersion":"0.1.0","protocolVersion":"rag-evaluator-v2","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","judgeConfigured":false,"metricsInitialized":true,"metricRequiredFields":{"context_precision":["userInput","reference","retrievedContexts"],"context_recall":["reference","retrievedContexts"],"faithfulness":["response","retrievedContexts"],"response_relevancy":["userInput","response"],"factual_correctness":["response","reference"]}}`))
	}))
	defer server.Close()

	client, err := NewRagasClient(server.URL, "", time.Second, 16, 20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	client.now = func() time.Time { return now }
	snapshot, err := client.ProbeHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Healthy || snapshot.ProtocolVersion != "rag-evaluator-v2" || !snapshot.Fresh(now) {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
	for range 3 {
		_ = client.HealthSnapshot()
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("snapshot read made network request: got %d requests", got)
	}
}

func TestRagasHealthProbeRejectsUninitializedSidecar(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"serviceVersion":"0.1.0","protocolVersion":"rag-evaluator-v2","ragasVersion":"0.3.9","metricBundleVersion":"rag-core-v1","judgeConfigured":false,"metricsInitialized":false,"metricRequiredFields":{}}`))
	}))
	defer server.Close()
	client, err := NewRagasClient(server.URL, "", time.Second, 16, 20, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.ProbeHealth(context.Background())
	if err == nil || snapshot.Healthy || snapshot.Reason != "evaluator_health_protocol_invalid" {
		t.Fatalf("expected incompatible health failure, snapshot=%+v err=%v", snapshot, err)
	}
}
