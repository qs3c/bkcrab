package config

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRAGEvaluationDefaultsDisabled(t *testing.T) {
	var cfg RAGCfg
	cfg.ApplyDefaults()
	if cfg.Evaluation.Enabled {
		t.Fatal("evaluation must default to disabled")
	}
	if cfg.Evaluation.MaxBatchSize != 16 || cfg.Evaluation.Sidecar.MetricBundleVersion != "rag-core-v1" {
		t.Fatalf("unexpected defaults: %+v", cfg.Evaluation)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRAGEvaluationEnabledRequiresEndpoint(t *testing.T) {
	cfg := RAGCfg{Evaluation: RAGEvaluationCfg{Enabled: true}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing endpoint error")
	}
}

func TestRAGEvaluatorLogValueAndCapabilitiesHideSecret(t *testing.T) {
	cfg := RAGEvaluationCfg{Enabled: true, Sidecar: RAGEvaluatorCfg{Endpoint: "http://eval:8080", APIKey: "top-secret"}}
	cfg.ApplyDefaults()
	text := cfg.Sidecar.LogValue().String()
	if strings.Contains(text, "top-secret") {
		t.Fatal("log value exposed secret")
	}
	b, err := json.Marshal(cfg.Capabilities(true, ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "top-secret") {
		t.Fatal("capabilities exposed secret")
	}
	_ = slog.AnyValue(cfg.Sidecar)
}

func TestRAGPolicyClosedJSONAndBounds(t *testing.T) {
	valid := []byte(`{"version":1,"topN":5,"candidateTopK":20,"minScore":0.5,"temperature":0.2,"maxTokens":4096,"ragPromptBundleVersion":"v1"}`)
	if _, err := DecodeRAGRuntimePolicy(valid); err != nil {
		t.Fatal(err)
	}
	unknown := []byte(`{"version":1,"topN":5,"candidateTopK":20,"minScore":0.5,"temperature":0.2,"maxTokens":4096,"ragPromptBundleVersion":"v1","secret":"x"}`)
	if _, err := DecodeRAGRuntimePolicy(unknown); err == nil {
		t.Fatal("unknown field accepted")
	}
	bad := RAGRuntimePolicyData{TopN: 21, CandidateTopK: 20, MinScore: .5, Temperature: .2, MaxTokens: 100, RAGPromptBundleVersion: "v1"}
	if err := bad.Validate(); err == nil {
		t.Fatal("candidateTopK below topN accepted")
	}
}
