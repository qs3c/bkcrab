package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestRAGCfgDefaults(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.RAG.Limits.MaxFileMB != 50 || cfg.RAG.Limits.MaxDocsPerKB != 200 ||
		cfg.RAG.Limits.MaxKBsPerUser != 20 {
		t.Fatalf("RAG default limits = %+v", cfg.RAG.Limits)
	}
	if cfg.RAG.Reranker.TimeoutMS != 5000 || cfg.RAG.Reranker.CandidateTopK != 20 ||
		cfg.RAG.Reranker.MinScore != 0.5 {
		t.Fatalf("RAG reranker defaults = %+v", cfg.RAG.Reranker)
	}

	cfg.RAG.Limits.MaxFileMB = 7
	ApplyDefaults(&cfg)
	if cfg.RAG.Limits.MaxFileMB != 7 {
		t.Fatalf("explicit maxFileMB overwritten: %d", cfg.RAG.Limits.MaxFileMB)
	}
}

func TestRAGAdvancedDefaultsAndSearchContentValidation(t *testing.T) {
	var cfg RAGCfg
	cfg.ApplyDefaults()

	if cfg.Features.AdvancedParsingEnabled || cfg.Features.OfficeParsingEnabled || cfg.Features.TextEnrichmentEnabled {
		t.Fatalf("RAG feature flags must default off: %+v", cfg.Features)
	}
	if cfg.DocumentAI.APIType != "openai-compatible" || cfg.DocumentAI.TimeoutMS <= 0 ||
		cfg.DocumentAI.VisionConcurrency <= 0 || cfg.DocumentAI.EnrichmentConcurrency <= 0 {
		t.Fatalf("DocumentAI defaults = %+v", cfg.DocumentAI)
	}
	if cfg.ParserSidecar.TimeoutMS != 600_000 {
		t.Fatalf("parser sidecar timeout = %d, want 600000", cfg.ParserSidecar.TimeoutMS)
	}
	if cfg.Limits.MaxPagesPerDocument != 300 || cfg.Limits.MaxVisionPagesPerDocument != 100 ||
		cfg.Limits.MaxVisionAssetsPerDocument != 100 || cfg.Limits.MaxAssetsPerDocument != 500 ||
		cfg.Limits.MaxImagePixels != 40_000_000 || cfg.Limits.PDFRenderDPI != 180 ||
		cfg.Limits.ThumbnailMaxEdge != 480 || cfg.Limits.DisplayMaxEdge != 2400 {
		t.Fatalf("document parsing defaults = %+v", cfg.Limits)
	}
	if cfg.Limits.MaxDocumentAIRequests != 300 || cfg.Limits.MaxDocumentAITokens != 200_000 ||
		cfg.Limits.MaxEstimatedDocumentAICostUSD != 1 || cfg.Limits.MaxSearchContentBytes != 60*1024 {
		t.Fatalf("DocumentAI/search defaults = %+v", cfg.Limits)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	if !cfg.Limits.SearchContentWithinLimit(strings.Repeat("界", 20*1024)) {
		t.Fatal("exactly 60 KiB of UTF-8 content should fit")
	}
	if cfg.Limits.SearchContentWithinLimit(strings.Repeat("界", 20*1024+1)) {
		t.Fatal("UTF-8 byte limit was treated as a rune limit")
	}
	cfg.Limits.MaxSearchContentBytes = RAGMilvusContentMaxLength + 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("maxSearchContentBytes above Milvus VarChar maxLength should fail validation")
	}
}

func TestRAGMilvusFilterLimitCoversMaximumDocumentCardinality(t *testing.T) {
	var cfg RAGCfg
	cfg.ApplyDefaults()
	required := worstCaseMilvusActiveFilterBytes(cfg.Limits.MaxDocsPerKB)
	if required <= 0 || required > cfg.Limits.MaxMilvusFilterBytes {
		t.Fatalf("default active filter requires %d bytes, configured %d", required, cfg.Limits.MaxMilvusFilterBytes)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config rejected: %v", err)
	}
	cfg.Limits.MaxMilvusFilterBytes = required - 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "active-version filter") {
		t.Fatalf("undersized active filter limit validation error = %v", err)
	}
	cfg.Limits.MaxMilvusFilterBytes = required
	if err := cfg.Validate(); err != nil {
		t.Fatalf("exact worst-case active filter limit rejected: %v", err)
	}
}

func TestRAGParseModeValidation(t *testing.T) {
	for _, value := range []string{`"standard"`, `"auto"`} {
		var mode ParseMode
		if err := json.Unmarshal([]byte(value), &mode); err != nil {
			t.Fatalf("unmarshal %s: %v", value, err)
		}
		if !mode.Valid() {
			t.Fatalf("mode %q reported invalid", mode)
		}
	}
	for _, value := range []string{`""`, `"advanced"`, `null`} {
		var mode ParseMode
		if err := json.Unmarshal([]byte(value), &mode); err == nil {
			t.Fatalf("invalid parse mode %s accepted as %q", value, mode)
		}
	}
}

func TestRAGAdvancedEnvironmentOverlay(t *testing.T) {
	t.Setenv("BKCRAB_RAG_ADVANCED_ENABLED", "false")
	t.Setenv("BKCRAB_RAG_OFFICE_ENABLED", "true")
	t.Setenv("BKCRAB_RAG_ENRICHMENT_ENABLED", "true")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_API_TYPE", "openai-compatible")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_ENDPOINT", "http://document-ai.internal/v1")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY", "document-ai-secret")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_VISION_MODEL", "vision-test")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_TEXT_MODEL", "text-test")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_TIMEOUT_MS", "90000")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_VISION_CONCURRENCY", "3")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_ENRICHMENT_CONCURRENCY", "5")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS", "document-ai.internal, backup.internal ")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_ALLOW_PRIVATE_ENDPOINT", "true")
	t.Setenv("BKCRAB_RAG_PARSER_ENDPOINT", "http://rag-parser:8080")
	t.Setenv("BKCRAB_RAG_PARSER_TIMEOUT_MS", "500000")
	t.Setenv("BKCRAB_RAG_LIMITS_MAX_PAGES_PER_DOCUMENT", "123")
	t.Setenv("BKCRAB_RAG_LIMITS_MAX_SEARCH_CONTENT_BYTES", "60000")

	env := LoadEnv()
	dst := RAGCfg{
		Features:   RAGFeatureCfg{AdvancedParsingEnabled: true},
		DocumentAI: RAGDocumentAICfg{AllowPrivateEndpoint: false},
	}
	env.ApplySystemRAG(&dst)

	if dst.Features.AdvancedParsingEnabled || !dst.Features.OfficeParsingEnabled || !dst.Features.TextEnrichmentEnabled {
		t.Fatalf("feature flag overlay = %+v", dst.Features)
	}
	if dst.DocumentAI.Endpoint != "http://document-ai.internal/v1" ||
		dst.DocumentAI.APIKey != "document-ai-secret" || dst.DocumentAI.VisionModel != "vision-test" ||
		dst.DocumentAI.TextModel != "text-test" || dst.DocumentAI.TimeoutMS != 90000 ||
		dst.DocumentAI.VisionConcurrency != 3 || dst.DocumentAI.EnrichmentConcurrency != 5 ||
		!dst.DocumentAI.AllowPrivateEndpoint || len(dst.DocumentAI.AllowedEndpointHosts) != 2 {
		t.Fatalf("DocumentAI env overlay mismatch: endpoint=%q visionModel=%q textModel=%q timeout=%d visionConcurrency=%d enrichmentConcurrency=%d allowPrivate=%v hosts=%v",
			dst.DocumentAI.Endpoint, dst.DocumentAI.VisionModel, dst.DocumentAI.TextModel,
			dst.DocumentAI.TimeoutMS, dst.DocumentAI.VisionConcurrency,
			dst.DocumentAI.EnrichmentConcurrency, dst.DocumentAI.AllowPrivateEndpoint,
			dst.DocumentAI.AllowedEndpointHosts)
	}
	if dst.ParserSidecar.Endpoint != "http://rag-parser:8080" || dst.ParserSidecar.TimeoutMS != 500000 {
		t.Fatalf("parser sidecar env overlay = %+v", dst.ParserSidecar)
	}
	if dst.Limits.MaxPagesPerDocument != 123 || dst.Limits.MaxSearchContentBytes != 60000 {
		t.Fatalf("limit env overlay = %+v", dst.Limits)
	}

}

func TestRAGLegacyTaskMigrationModeRequiresExactOfflineAcknowledgement(t *testing.T) {
	t.Setenv("BKCRAB_RAG_LEGACY_TASK_MIGRATION_MODE", "  offline-v1  ")
	env := LoadEnv()
	if env.RAGLegacyTaskMigrationMode != RAGLegacyTaskMigrationModeOfflineV1 {
		t.Fatalf("legacy task migration mode = %q", env.RAGLegacyTaskMigrationMode)
	}
}

func TestRAGDocumentAISecretScrubAndLogging(t *testing.T) {
	const secret = "document-ai-secret-that-must-not-leak"
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logger.Info("RAG config", "documentAI", RAGDocumentAICfg{
		Endpoint: "https://document-ai.example/v1",
		APIKey:   secret,
	})
	if strings.Contains(output.String(), secret) {
		t.Fatalf("DocumentAI secret leaked through slog: %s", output.String())
	}

	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY", secret)
	ScrubBootSecrets()
	if value := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY"); value != "" {
		t.Fatalf("DocumentAI bootstrap secret was not scrubbed: %q", value)
	}
}

func TestRAGCfgJSONAndAvailable(t *testing.T) {
	var cfg Config
	err := json.Unmarshal([]byte(`{"rag":{"milvus":{"address":"127.0.0.1:19530","username":"u","password":"p"},"embedding":{"endpoint":"http://embed/v1","apiKey":"secret","model":"embed-v3","dims":1024},"reranker":{"enabled":true,"endpoint":"http://rerank/v1","apiKey":"rank-secret","model":"qwen3-reranker","timeoutMs":3000,"candidateTopK":30,"minScore":0.6},"limits":{"maxFileMB":12}}}`), &cfg)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.RAG.Available() {
		t.Fatalf("complete RAG config reported unavailable: %+v", cfg.RAG)
	}
	if cfg.RAG.Milvus.Username != "u" || cfg.RAG.Embedding.APIKey != "secret" ||
		cfg.RAG.Reranker.APIKey != "rank-secret" || cfg.RAG.Reranker.MinScore != 0.6 ||
		!cfg.RAG.Reranker.Available() || cfg.RAG.Limits.MaxFileMB != 12 {
		t.Fatalf("RAG JSON fields not decoded: %+v", cfg.RAG)
	}
	var designKey MilvusCfg
	if err := json.Unmarshal([]byte(`{"address":"milvus:19530","user":"design-user"}`), &designKey); err != nil {
		t.Fatalf("unmarshal design-spec user key: %v", err)
	}
	if designKey.Username != "design-user" {
		t.Fatalf("design-spec milvus user alias not decoded: %+v", designKey)
	}

	cfg.RAG.Embedding.Dims = 0
	if cfg.RAG.Available() {
		t.Fatal("RAG config without embedding dimensions reported available")
	}
}

func TestRAGRerankerEnvironmentOverlay(t *testing.T) {
	t.Setenv("BKCRAB_RAG_RERANKER_ENABLED", "false")
	t.Setenv("BKCRAB_RAG_RERANKER_ENDPOINT", "http://ranker:8080/v1")
	t.Setenv("BKCRAB_RAG_RERANKER_API_KEY", "rank-key")
	t.Setenv("BKCRAB_RAG_RERANKER_MODEL", "qwen3-reranker")
	t.Setenv("BKCRAB_RAG_RERANKER_TIMEOUT_MS", "7000")
	t.Setenv("BKCRAB_RAG_RERANKER_CANDIDATE_TOP_K", "25")
	t.Setenv("BKCRAB_RAG_RERANKER_MIN_SCORE", "0.55")

	env := LoadEnv()
	dst := RAGCfg{Reranker: RAGRerankerCfg{Enabled: true}}
	env.ApplySystemRAG(&dst)
	if dst.Reranker.Enabled || dst.Reranker.Endpoint != "http://ranker:8080/v1" ||
		dst.Reranker.APIKey != "rank-key" || dst.Reranker.Model != "qwen3-reranker" ||
		dst.Reranker.TimeoutMS != 7000 || dst.Reranker.CandidateTopK != 25 ||
		dst.Reranker.MinScore != 0.55 {
		t.Fatalf("reranker env overlay = %+v", dst.Reranker)
	}
}

func TestRAGAgentCfgMerge(t *testing.T) {
	old := AgentFileConfigLoader
	fileCfg := AgentFileConfig{RAG: &RAGAgentCfg{KBs: []string{"kb_a", "kb_b"}, TopN: 8}}
	AgentFileConfigLoader = func(_, _ string) (AgentFileConfig, bool) {
		return fileCfg, true
	}
	t.Cleanup(func() { AgentFileConfigLoader = old })

	var cfg Config
	ApplyDefaults(&cfg)
	resolved := cfg.MergedAgentConfig(AgentEntry{ID: "agent_1", UserID: "user_1"})
	if len(resolved.RAG.KBs) != 2 || resolved.RAG.KBs[0] != "kb_a" || resolved.RAG.TopN != 8 {
		t.Fatalf("RAG cfg not merged: %+v", resolved.RAG)
	}

	fileCfg.RAG.KBs[0] = "mutated"
	if resolved.RAG.KBs[0] != "kb_a" {
		t.Fatalf("resolved RAG KBs aliases loader data: %+v", resolved.RAG.KBs)
	}
}
