package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"math"
	"os"
	"reflect"
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
	wantLimits := RAGEvaluationCfg{
		WorkerConcurrency:       1,
		DocumentConcurrency:     1,
		CaseConcurrency:         1,
		ScoreConcurrency:        1,
		MaxBatchSize:            16,
		MaxContextsPerSample:    20,
		MaxContextBytes:         64 * 1024,
		MaxRequestBytes:         4 * 1024 * 1024,
		MaxRunCases:             1_000,
		MaxRunTokens:            2_000_000,
		MaxRunCostUSD:           25,
		MaxRunDurationSec:       6 * 60 * 60,
		RunRetentionDays:        90,
		DatasetRetentionDays:    365,
		GenerationRetentionDays: 30,
	}
	if cfg.Evaluation.WorkerConcurrency != wantLimits.WorkerConcurrency ||
		cfg.Evaluation.DocumentConcurrency != wantLimits.DocumentConcurrency ||
		cfg.Evaluation.CaseConcurrency != wantLimits.CaseConcurrency ||
		cfg.Evaluation.ScoreConcurrency != wantLimits.ScoreConcurrency ||
		cfg.Evaluation.MaxContextsPerSample != wantLimits.MaxContextsPerSample ||
		cfg.Evaluation.MaxContextBytes != wantLimits.MaxContextBytes ||
		cfg.Evaluation.MaxRequestBytes != wantLimits.MaxRequestBytes ||
		cfg.Evaluation.MaxRunCases != wantLimits.MaxRunCases ||
		cfg.Evaluation.MaxRunTokens != wantLimits.MaxRunTokens ||
		cfg.Evaluation.MaxRunCostUSD != wantLimits.MaxRunCostUSD ||
		cfg.Evaluation.MaxRunDurationSec != wantLimits.MaxRunDurationSec ||
		cfg.Evaluation.RunRetentionDays != wantLimits.RunRetentionDays ||
		cfg.Evaluation.DatasetRetentionDays != wantLimits.DatasetRetentionDays ||
		cfg.Evaluation.GenerationRetentionDays != wantLimits.GenerationRetentionDays {
		t.Fatalf("evaluation defaults are incomplete: %+v", cfg.Evaluation)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if capabilities := cfg.Evaluation.Capabilities(true, "disabled"); capabilities.SidecarHealthy {
		t.Fatal("disabled evaluation reported a healthy sidecar")
	}
}

func TestRAGEvaluationEnabledRequiresEndpoint(t *testing.T) {
	cfg := RAGCfg{Evaluation: RAGEvaluationCfg{Enabled: true}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected missing endpoint error")
	}
}

func TestRAGEvaluationCostBudgetRequiresExplicitPrices(t *testing.T) {
	cfg := RAGEvaluationCfg{Enabled: true, Sidecar: RAGEvaluatorCfg{Endpoint: "http://eval:8080"}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "token prices") {
		t.Fatalf("missing token prices err=%v", err)
	}
	cfg.AnswerInputCostPerMUSD = 1
	cfg.AnswerOutputCostPerMUSD = 2
	cfg.Sidecar.LLMInputCostPerMUSD = 3
	cfg.Sidecar.LLMOutputCostPerMUSD = 4
	cfg.Sidecar.EmbeddingCostPerMUSD = 5
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRAGEvaluatorLogValueAndCapabilitiesHideSecret(t *testing.T) {
	cfg := RAGEvaluationCfg{Enabled: true, Sidecar: RAGEvaluatorCfg{Endpoint: "http://eval:8080", APIKey: "top-secret"}}
	cfg.ApplyDefaults()
	var output bytes.Buffer
	slog.New(slog.NewTextHandler(&output, nil)).Info("evaluator", "config", cfg.Sidecar)
	if strings.Contains(output.String(), "top-secret") {
		t.Fatalf("log value exposed secret: %s", output.String())
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "top-secret") {
		t.Fatalf("config JSON exposed secret: %s", b)
	}
	b, err = json.Marshal(cfg.Capabilities(true, ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "top-secret") {
		t.Fatalf("capabilities exposed secret: %s", b)
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

func TestRAGPolicyLegalBoundariesAndInvalidNumericValues(t *testing.T) {
	for _, policy := range []RAGRuntimePolicyData{
		{TopN: 1, CandidateTopK: 1, MinScore: 0, Temperature: 0, MaxTokens: 1, RAGPromptBundleVersion: "v1"},
		{Version: math.MaxInt64, TopN: 100, CandidateTopK: 500, MinScore: 1, Temperature: 2, MaxTokens: 131_072, RAGPromptBundleVersion: "v1"},
	} {
		if err := policy.Validate(); err != nil {
			t.Fatalf("legal runtime boundary rejected: %+v: %v", policy, err)
		}
	}
	validRuntime := RAGRuntimePolicyData{TopN: 5, CandidateTopK: 20, MinScore: .5, Temperature: .2, MaxTokens: 4096, RAGPromptBundleVersion: "v1"}
	for name, mutate := range map[string]func(*RAGRuntimePolicyData){
		"topN":     func(p *RAGRuntimePolicyData) { p.TopN = 0 },
		"minScore": func(p *RAGRuntimePolicyData) { p.MinScore = 1.01 },
		"NaN score": func(p *RAGRuntimePolicyData) {
			p.MinScore = math.NaN()
		},
	} {
		t.Run("runtime "+name, func(t *testing.T) {
			policy := validRuntime
			mutate(&policy)
			if err := policy.Validate(); err == nil {
				t.Fatal("invalid runtime numeric value accepted")
			}
		})
	}

	for _, policy := range []RAGIngestionPolicyData{
		{ChunkSize: 128, ChunkOverlap: 0, ParseMode: ParseModeStandard, Embedding: RAGPolicyEmbeddingData{ContractFingerprint: "x", Model: "embed", Dims: 1}},
		{Version: math.MaxInt64, ChunkSize: 8192, ChunkOverlap: 8191, ParseMode: ParseModeAuto, Embedding: RAGPolicyEmbeddingData{ContractFingerprint: "x", Model: "embed", Dims: 65_536}},
	} {
		if err := policy.Validate(); err != nil {
			t.Fatalf("legal ingestion boundary rejected: %+v: %v", policy, err)
		}
	}
	validIngestion := RAGIngestionPolicyData{ChunkSize: 512, ChunkOverlap: 64, ParseMode: ParseModeStandard, Embedding: RAGPolicyEmbeddingData{ContractFingerprint: "x", Model: "embed", Dims: 1024}}
	for name, mutate := range map[string]func(*RAGIngestionPolicyData){
		"chunk":         func(p *RAGIngestionPolicyData) { p.ChunkSize = 127 },
		"overlap":       func(p *RAGIngestionPolicyData) { p.ChunkOverlap = p.ChunkSize },
		"dims":          func(p *RAGIngestionPolicyData) { p.Embedding.Dims = 65_537 },
		"parser engine": func(p *RAGIngestionPolicyData) { p.ParserEngine = "unknown" },
		"parser casing": func(p *RAGIngestionPolicyData) { p.ParserEngine = "AnyDoc" },
	} {
		t.Run("ingestion "+name, func(t *testing.T) {
			policy := validIngestion
			mutate(&policy)
			if err := policy.Validate(); err == nil {
				t.Fatal("invalid ingestion numeric value accepted")
			}
		})
	}
}

func TestRAGEvaluationRejectsUnknownEnumsAndTrailingJSON(t *testing.T) {
	badParseMode := []byte(`{"version":1,"chunkSize":512,"chunkOverlap":64,"parseMode":"mystery","enrichmentEnabled":false,"documentAI":{},"embedding":{"contractFingerprint":"sha256:x","model":"embed","dims":1024}}`)
	if _, err := DecodeRAGIngestionPolicy(badParseMode); err == nil {
		t.Fatal("unknown parse mode accepted")
	}
	badFailurePolicy := []byte(`{"ingestion":{"version":1,"chunkSize":512,"chunkOverlap":64,"parseMode":"standard","enrichmentEnabled":false,"documentAI":{},"embedding":{"contractFingerprint":"sha256:x","model":"embed","dims":1024}},"runtime":{"version":1,"topN":5,"candidateTopK":20,"minScore":0.5,"temperature":0.2,"maxTokens":4096,"ragPromptBundleVersion":"rag-answer-v1"},"rewriteEnabled":false,"hydeEnabled":false,"rerankerEnabled":true,"rerankerModel":"ranker","rerankerTimeoutMs":5000,"rerankerFailurePolicy":"ignore","answerModel":"answer"}`)
	if _, err := DecodeRAGEvalProfile(badFailurePolicy); err == nil {
		t.Fatal("unknown reranker failure policy accepted")
	}
	trailing := []byte(`{"version":1,"topN":5,"candidateTopK":20,"minScore":0.5,"temperature":0.2,"maxTokens":4096,"ragPromptBundleVersion":"v1"} {}`)
	if _, err := DecodeRAGRuntimePolicy(trailing); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
}

func TestRAGEvaluationLimitBoundaries(t *testing.T) {
	valid := RAGEvaluationCfg{Enabled: true, Sidecar: RAGEvaluatorCfg{Endpoint: "http://rag-evaluator:8080"}}
	valid.ApplyDefaults()
	valid.AnswerInputCostPerMUSD = 1
	valid.AnswerOutputCostPerMUSD = 1
	valid.Sidecar.LLMInputCostPerMUSD = 1
	valid.Sidecar.LLMOutputCostPerMUSD = 1
	valid.Sidecar.EmbeddingCostPerMUSD = 1
	valid.Sidecar.TimeoutMS = ragEvalMaxTimeoutMS
	valid.WorkerConcurrency = ragEvalMaxWorkerConcurrency
	valid.MaxBatchSize = ragEvalMaxBatchSize
	valid.MaxContextsPerSample = ragEvalMaxContextsPerSample
	valid.MaxContextBytes = ragEvalMaxContextBytes
	valid.MaxRequestBytes = ragEvalMaxRequestBytes
	valid.MaxRunCases = ragEvalMaxRunCases
	valid.MaxRunTokens = ragEvalMaxRunTokens
	valid.MaxRunCostUSD = ragEvalMaxRunCostUSD
	valid.MaxRunDurationSec = ragEvalMaxRunDurationSec
	valid.RunRetentionDays = ragEvalMaxRetentionDays
	valid.DatasetRetentionDays = ragEvalMaxRetentionDays
	valid.GenerationRetentionDays = ragEvalMaxRetentionDays
	if err := valid.Validate(); err != nil {
		t.Fatalf("legal upper boundaries rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*RAGEvaluationCfg)
	}{
		{name: "timeout", mutate: func(c *RAGEvaluationCfg) { c.Sidecar.TimeoutMS = ragEvalMaxTimeoutMS + 1 }},
		{name: "worker concurrency", mutate: func(c *RAGEvaluationCfg) { c.WorkerConcurrency = ragEvalMaxWorkerConcurrency + 1 }},
		{name: "batch", mutate: func(c *RAGEvaluationCfg) { c.MaxBatchSize = ragEvalMaxBatchSize + 1 }},
		{name: "contexts", mutate: func(c *RAGEvaluationCfg) { c.MaxContextsPerSample = ragEvalMaxContextsPerSample + 1 }},
		{name: "context bytes", mutate: func(c *RAGEvaluationCfg) { c.MaxContextBytes = ragEvalMaxContextBytes + 1 }},
		{name: "request bytes", mutate: func(c *RAGEvaluationCfg) { c.MaxRequestBytes = ragEvalMaxRequestBytes + 1 }},
		{name: "cases", mutate: func(c *RAGEvaluationCfg) { c.MaxRunCases = ragEvalMaxRunCases + 1 }},
		{name: "tokens", mutate: func(c *RAGEvaluationCfg) { c.MaxRunTokens = ragEvalMaxRunTokens + 1 }},
		{name: "cost", mutate: func(c *RAGEvaluationCfg) { c.MaxRunCostUSD = ragEvalMaxRunCostUSD + 1 }},
		{name: "duration", mutate: func(c *RAGEvaluationCfg) { c.MaxRunDurationSec = ragEvalMaxRunDurationSec + 1 }},
		{name: "run retention", mutate: func(c *RAGEvaluationCfg) { c.RunRetentionDays = ragEvalMaxRetentionDays + 1 }},
		{name: "dataset retention", mutate: func(c *RAGEvaluationCfg) { c.DatasetRetentionDays = ragEvalMaxRetentionDays + 1 }},
		{name: "generation retention", mutate: func(c *RAGEvaluationCfg) { c.GenerationRetentionDays = ragEvalMaxRetentionDays + 1 }},
		{name: "NaN cost", mutate: func(c *RAGEvaluationCfg) { c.MaxRunCostUSD = math.NaN() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			tt.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("out-of-range value accepted")
			}
		})
	}
}

func TestRAGEvaluationEnvironmentOverlayIsIsolated(t *testing.T) {
	t.Setenv("BKCRAB_RAG_EVAL_ENABLED", "true")
	t.Setenv("BKCRAB_RAG_EVAL_ENDPOINT", "http://eval.internal:8080")
	t.Setenv("BKCRAB_RAG_EVAL_API_KEY", "eval-secret")
	t.Setenv("BKCRAB_RAG_EVAL_LLM_MODEL", "judge-model")
	t.Setenv("BKCRAB_RAG_EVAL_EMBEDDING_MODEL", "judge-embedding")
	t.Setenv("BKCRAB_RAG_EVAL_MAX_RUN_TOKENS", "123456")
	t.Setenv("BKCRAB_RAG_EVAL_RUN_RETENTION_DAYS", "45")
	t.Setenv("BKCRAB_RAG_EVAL_ANSWER_INPUT_COST_USD_PER_MILLION", "1")
	t.Setenv("BKCRAB_RAG_EVAL_ANSWER_OUTPUT_COST_USD_PER_MILLION", "2")
	t.Setenv("BKCRAB_RAG_EVAL_LLM_INPUT_COST_USD_PER_MILLION", "3")
	t.Setenv("BKCRAB_RAG_EVAL_LLM_OUTPUT_COST_USD_PER_MILLION", "4")
	t.Setenv("BKCRAB_RAG_EVAL_EMBEDDING_COST_USD_PER_MILLION", "5")

	dst := RAGCfg{
		Embedding: RAGEmbeddingCfg{Model: "production-embedding"},
		Reranker:  RAGRerankerCfg{Model: "production-reranker"},
	}
	LoadEnv().ApplySystemRAG(&dst)
	if !dst.Evaluation.Enabled || dst.Evaluation.Sidecar.Endpoint != "http://eval.internal:8080" ||
		dst.Evaluation.Sidecar.APIKey != "eval-secret" || dst.Evaluation.Sidecar.LLMModel != "judge-model" ||
		dst.Evaluation.Sidecar.EmbeddingModel != "judge-embedding" || dst.Evaluation.MaxRunTokens != 123456 ||
		dst.Evaluation.RunRetentionDays != 45 || dst.Evaluation.AnswerInputCostPerMUSD != 1 ||
		dst.Evaluation.AnswerOutputCostPerMUSD != 2 || dst.Evaluation.Sidecar.LLMInputCostPerMUSD != 3 ||
		dst.Evaluation.Sidecar.LLMOutputCostPerMUSD != 4 || dst.Evaluation.Sidecar.EmbeddingCostPerMUSD != 5 {
		t.Fatalf("evaluation overlay mismatch: %+v", dst.Evaluation)
	}
	if dst.Embedding.Model != "production-embedding" || dst.Reranker.Model != "production-reranker" {
		t.Fatalf("evaluation overlay changed production RAG config: embedding=%q reranker=%q", dst.Embedding.Model, dst.Reranker.Model)
	}

	ScrubBootSecrets()
	if got := os.Getenv("BKCRAB_RAG_EVAL_API_KEY"); got != "" {
		t.Fatalf("evaluation bootstrap secret was not scrubbed: %q", got)
	}
}

func TestRAGPolicyCanonicalInputTypesAreStableAndCredentialFree(t *testing.T) {
	profile := RAGEvalProfileData{
		Ingestion: RAGIngestionPolicyData{
			Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: ParseModeStandard,
			Embedding: RAGPolicyEmbeddingData{ContractFingerprint: "sha256:contract", Model: "embed", Dims: 1024},
		},
		Runtime:         RAGRuntimePolicyData{Version: 2, TopN: 5, CandidateTopK: 20, MinScore: .5, Temperature: .2, MaxTokens: 4096, RAGPromptBundleVersion: "rag-answer-v1"},
		RerankerEnabled: true, RerankerModel: "ranker", RerankerTimeoutMS: 5000,
		RerankerFailurePolicy: RAGRerankerFallbackRRF, AnswerModel: "answer-model",
	}
	first, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("canonical input JSON changed: %s != %s", first, second)
	}
	for _, forbidden := range []string{"apiKey", "endpoint", "secret"} {
		if bytes.Contains(first, []byte(forbidden)) {
			t.Fatalf("canonical input contains credential field %q: %s", forbidden, first)
		}
	}
}
