package setup

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestRAGEvalCapabilitiesReadsFreshCachedHealth(t *testing.T) {
	server := NewServer(0)
	cfg := config.RAGCfg{}
	cfg.ApplyDefaults()
	cfg.Evaluation.Enabled = true
	cfg.Evaluation.Sidecar.Endpoint = "http://rag-evaluator:8080"
	server.SetRAGConfig(cfg)
	server.SetRAGEvaluatorHealthSnapshot(config.RAGEvaluatorHealthSnapshot{
		Healthy: true, ExpiresAt: time.Now().Add(time.Minute), ProtocolVersion: "rag-evaluator-v2",
	})

	recorder := httptest.NewRecorder()
	server.handleRAGEvalCapabilities(recorder, httptest.NewRequest("GET", "/api/admin/rag-evals/capabilities", nil))
	var response config.RAGEvaluationCapabilities
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.SidecarHealthy {
		t.Fatalf("fresh cached evaluator health was not published: %+v", response)
	}
}

func TestRAGEvalCapabilitiesRejectsExpiredCachedHealth(t *testing.T) {
	server := NewServer(0)
	cfg := config.RAGCfg{}
	cfg.ApplyDefaults()
	cfg.Evaluation.Enabled = true
	cfg.Evaluation.Sidecar.Endpoint = "http://rag-evaluator:8080"
	server.SetRAGConfig(cfg)
	server.SetRAGEvaluatorHealthSnapshot(config.RAGEvaluatorHealthSnapshot{
		Healthy: true, ExpiresAt: time.Now().Add(-time.Second),
	})

	recorder := httptest.NewRecorder()
	server.handleRAGEvalCapabilities(recorder, httptest.NewRequest("GET", "/api/admin/rag-evals/capabilities", nil))
	var response config.RAGEvaluationCapabilities
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SidecarHealthy {
		t.Fatalf("expired evaluator health was published: %+v", response)
	}
}
