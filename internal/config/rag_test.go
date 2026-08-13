package config

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFairQueueValidation(t *testing.T) {
	validFair := func() FairQueueCfg {
		cfg := DefaultFairQueueCfg()
		cfg.Enabled = true
		cfg.RedisAddr = "redis:6379"
		cfg.RabbitMQURL = "amqp://rabbitmq:5672/"
		cfg.MySQLWriterTopology = FairQueueMySQLWriterTopologySingle
		cfg.RAGIndex.WorkerMode = FairQueueWorkerModeFair
		return cfg
	}

	for _, mode := range []string{
		FairQueueWorkerModeLegacy,
		FairQueueWorkerModePaused,
		FairQueueWorkerModeFair,
	} {
		cfg := validFair()
		cfg.RAGIndex.WorkerMode = mode
		if err := cfg.Validate("mysql"); err != nil {
			t.Fatalf("valid worker mode %q rejected: %v", mode, err)
		}
	}
	boundary := validFair()
	boundary.RAGIndex.ReconcileInterval = FairQueueMaxDuration
	boundary.RAGIndex.ReconcilePageSize = FairQueueMaxReconcilePageSize
	if err := boundary.Validate("mysql"); err != nil {
		t.Fatalf("inclusive deployment limits rejected: %v", err)
	}

	tests := []struct {
		name    string
		storage string
		mutate  func(*FairQueueCfg)
	}{
		{name: "unknown worker mode", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.WorkerMode = "unknown" }},
		{name: "fair mode while disabled", storage: "mysql", mutate: func(c *FairQueueCfg) { c.Enabled = false }},
		{name: "zero local workers", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.LocalWorkers = 0 }},
		{name: "zero base concurrency", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.PerUserBaseConcurrency = 0 }},
		{name: "base above burst", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.PerUserBaseConcurrency = 5 }},
		{name: "burst above global", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.GlobalConcurrency = 3 }},
		{name: "zero reservation heartbeat", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ReservationHeartbeat = 0 }},
		{name: "heartbeat reaches reservation TTL", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ReservationHeartbeat = c.RAGIndex.ReservationTTL }},
		{name: "zero prepare timeout", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.PrepareTimeout = 0 }},
		{name: "prepare reaches provisional TTL", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.PrepareTimeout = c.RAGIndex.ProvisionalTTL }},
		{name: "provisional reaches recovery drain", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ProvisionalTTL = c.RAGIndex.RecoveryDrainTimeout }},
		{name: "processing reaches recovery drain", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ProcessingTurnTTL = c.RAGIndex.RecoveryDrainTimeout }},
		{name: "zero dispatch interval", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.DispatchInterval = 0 }},
		{name: "duration above deployment limit", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ReconcileInterval = FairQueueMaxDuration + time.Nanosecond }},
		{name: "zero recovery page size", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ReconcilePageSize = 0 }},
		{name: "page size above deployment limit", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.ReconcilePageSize = FairQueueMaxReconcilePageSize + 1 }},
		{name: "zero publish attempt timeout", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.PublishAttemptTimeout = 0 }},
		{name: "publish attempt reaches recovery drain", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RAGIndex.PublishAttemptTimeout = c.RAGIndex.RecoveryDrainTimeout }},
		{name: "enabled without Redis address", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RedisAddr = "" }},
		{name: "enabled without RabbitMQ URL", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RabbitMQURL = "" }},
		{name: "enabled without exchange", storage: "mysql", mutate: func(c *FairQueueCfg) { c.Exchange = "" }},
		{name: "enabled without dead-letter exchange", storage: "mysql", mutate: func(c *FairQueueCfg) { c.DeadLetterExchange = "" }},
		{name: "enabled without key prefix", storage: "mysql", mutate: func(c *FairQueueCfg) { c.KeyPrefix = "" }},
		{name: "unsupported Redis mode", storage: "mysql", mutate: func(c *FairQueueCfg) { c.RedisMode = "cluster" }},
		{name: "fair mode on non-MySQL storage", storage: "sqlite", mutate: func(*FairQueueCfg) {}},
		{name: "fair mode without single writer declaration", storage: "mysql", mutate: func(c *FairQueueCfg) { c.MySQLWriterTopology = "" }},
		{name: "fair mode with multi writer declaration", storage: "mysql", mutate: func(c *FairQueueCfg) { c.MySQLWriterTopology = "multi" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validFair()
			tt.mutate(&cfg)
			if err := cfg.Validate(tt.storage); err == nil {
				t.Fatalf("invalid fair queue config accepted: %+v", cfg)
			}
		})
	}

	disabled := DefaultFairQueueCfg()
	if err := disabled.Validate("sqlite"); err != nil {
		t.Fatalf("disabled legacy config rejected: %v", err)
	}
}

func TestFairQueueEnvironmentOverlay(t *testing.T) {
	t.Setenv("BKCRAB_FAIR_QUEUE_ENABLED", "true")
	t.Setenv("BKCRAB_FAIR_QUEUE_REDIS_ADDR", "redis.internal:6380")
	t.Setenv("BKCRAB_FAIR_QUEUE_REDIS_PASSWORD", "redis-secret")
	t.Setenv("BKCRAB_FAIR_QUEUE_REDIS_DB", "3")
	t.Setenv("BKCRAB_FAIR_QUEUE_REDIS_MODE", "standalone")
	t.Setenv("BKCRAB_FAIR_QUEUE_RABBITMQ_URL", "amqp://user:rabbit-secret@rabbit.internal:5672/vhost")
	t.Setenv("BKCRAB_FAIR_QUEUE_EXCHANGE", "custom.task")
	t.Setenv("BKCRAB_FAIR_QUEUE_DEAD_LETTER_EXCHANGE", "custom.dlx")
	t.Setenv("BKCRAB_FAIR_QUEUE_KEY_PREFIX", "custom:")
	t.Setenv("BKCRAB_FAIR_QUEUE_MYSQL_WRITER_TOPOLOGY", "single")
	t.Setenv("BKCRAB_RAG_INDEX_WORKER_MODE", "paused")
	t.Setenv("BKCRAB_RAG_INDEX_LOCAL_WORKERS", "8")
	t.Setenv("BKCRAB_RAG_INDEX_GLOBAL_CONCURRENCY", "12")
	t.Setenv("BKCRAB_RAG_INDEX_PER_USER_BASE_CONCURRENCY", "3")
	t.Setenv("BKCRAB_RAG_INDEX_PER_USER_BURST_CONCURRENCY", "6")
	t.Setenv("BKCRAB_RAG_INDEX_BORROW_ENABLED", "false")
	t.Setenv("BKCRAB_RAG_INDEX_RECONCILE_INTERVAL", "45s")
	t.Setenv("BKCRAB_RAG_INDEX_RESERVATION_TTL", "90s")
	t.Setenv("BKCRAB_RAG_INDEX_RESERVATION_HEARTBEAT", "25s")
	t.Setenv("BKCRAB_RAG_INDEX_PREPARE_TIMEOUT", "12s")
	t.Setenv("BKCRAB_RAG_INDEX_PROVISIONAL_TTL", "20s")
	t.Setenv("BKCRAB_RAG_INDEX_DISPATCH_INTERVAL", "2s")
	t.Setenv("BKCRAB_RAG_INDEX_PUBLISH_ATTEMPT_TIMEOUT", "30s")
	t.Setenv("BKCRAB_RAG_INDEX_EXPIRED_SWEEP_INTERVAL", "18s")
	t.Setenv("BKCRAB_RAG_INDEX_PROCESSING_TTL", "22s")
	t.Setenv("BKCRAB_RAG_INDEX_RECONCILE_PAGE_SIZE", "350")
	t.Setenv("BKCRAB_RAG_INDEX_RECOVERY_DRAIN_TIMEOUT", "3m")

	cfg := LoadEnv().FairQueue
	if !cfg.Enabled || cfg.RedisAddr != "redis.internal:6380" ||
		cfg.RedisPassword != "redis-secret" || cfg.RedisDB != 3 ||
		cfg.RedisMode != FairQueueRedisModeStandalone ||
		cfg.RabbitMQURL != "amqp://user:rabbit-secret@rabbit.internal:5672/vhost" ||
		cfg.Exchange != "custom.task" || cfg.DeadLetterExchange != "custom.dlx" ||
		cfg.KeyPrefix != "custom:" || cfg.MySQLWriterTopology != FairQueueMySQLWriterTopologySingle {
		t.Fatalf("shared fair queue environment overlay = %+v", cfg)
	}
	rag := cfg.RAGIndex
	if rag.WorkerMode != FairQueueWorkerModePaused || rag.LocalWorkers != 8 ||
		rag.GlobalConcurrency != 12 || rag.PerUserBaseConcurrency != 3 ||
		rag.PerUserBurstConcurrency != 6 || rag.BorrowEnabled ||
		rag.ReconcileInterval != 45*time.Second || rag.ReservationTTL != 90*time.Second ||
		rag.ReservationHeartbeat != 25*time.Second || rag.PrepareTimeout != 12*time.Second ||
		rag.ProvisionalTTL != 20*time.Second || rag.DispatchInterval != 2*time.Second ||
		rag.PublishAttemptTimeout != 30*time.Second || rag.ExpiredRunningSweepInterval != 18*time.Second ||
		rag.ProcessingTurnTTL != 22*time.Second || rag.ReconcilePageSize != 350 ||
		rag.RecoveryDrainTimeout != 3*time.Minute {
		t.Fatalf("RAG fair queue environment overlay = %+v", rag)
	}
	if err := cfg.Validate("mysql"); err != nil {
		t.Fatalf("environment-derived fair queue config rejected: %v", err)
	}
}

func TestFairQueueInvalidEnvironmentDoesNotFallBackToDefaults(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "malformed bool", key: "BKCRAB_FAIR_QUEUE_ENABLED", value: "not-a-bool"},
		{name: "malformed integer", key: "BKCRAB_RAG_INDEX_LOCAL_WORKERS", value: "many"},
		{name: "malformed duration", key: "BKCRAB_RAG_INDEX_RECONCILE_INTERVAL", value: "soon"},
		{name: "zero duration", key: "BKCRAB_RAG_INDEX_PUBLISH_ATTEMPT_TIMEOUT", value: "0s"},
		{name: "negative Redis DB", key: "BKCRAB_FAIR_QUEUE_REDIS_DB", value: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if err := LoadEnv().FairQueue.Validate("mysql"); err == nil {
				t.Fatalf("invalid %s=%q silently fell back to defaults", tt.key, tt.value)
			}
		})
	}
}

func TestFairQueueEnvSecretsAreScrubbed(t *testing.T) {
	const rabbitURL = "amqp://user:rabbit-secret@rabbitmq:5672/"
	const redisPassword = "redis-secret"
	t.Setenv("BKCRAB_FAIR_QUEUE_RABBITMQ_URL", rabbitURL)
	t.Setenv("BKCRAB_FAIR_QUEUE_REDIS_PASSWORD", redisPassword)

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&output, nil))
	logger.Info("fair queue config", "config", LoadEnv().FairQueue)
	if strings.Contains(output.String(), rabbitURL) || strings.Contains(output.String(), redisPassword) {
		t.Fatalf("fair queue credential leaked through slog: %s", output.String())
	}

	ScrubBootSecrets()
	if value := os.Getenv("BKCRAB_FAIR_QUEUE_RABBITMQ_URL"); value != "" {
		t.Fatalf("RabbitMQ bootstrap URL was not scrubbed: %q", value)
	}
	if value := os.Getenv("BKCRAB_FAIR_QUEUE_REDIS_PASSWORD"); value != "" {
		t.Fatalf("Redis bootstrap password was not scrubbed: %q", value)
	}
}

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

	if cfg.Features.AdvancedParsingEnabled || cfg.Features.OfficeParsingEnabled || cfg.Features.TextEnrichmentEnabled ||
		cfg.Features.GenerationShadowReadEnabled || cfg.Features.GenerationResolverAuthoritative {
		t.Fatalf("RAG feature flags must default off: %+v", cfg.Features)
	}
	if cfg.DocumentAI.APIType != "openai-compatible" || cfg.DocumentAI.TimeoutMS <= 0 ||
		cfg.DocumentAI.VisionConcurrency <= 0 || cfg.DocumentAI.EnrichmentConcurrency <= 0 ||
		cfg.DocumentAI.ResponseFormat != RAGDocumentAIResponseFormatJSONSchema {
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
	cfg.Limits.MaxSearchContentBytes = RAGMilvusContentMaxLength
	cfg.DocumentAI.ResponseFormat = "unsupported"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "responseFormat") {
		t.Fatalf("unsupported DocumentAI response format validation error = %v", err)
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
	t.Setenv("BKCRAB_RAG_GENERATION_SHADOW_READ_ENABLED", "true")
	t.Setenv("BKCRAB_RAG_GENERATION_RESOLVER_AUTHORITATIVE", "true")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_API_TYPE", "openai-compatible")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_ENDPOINT", "http://document-ai.internal/v1")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY", "document-ai-secret")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_VISION_MODEL", "vision-test")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_TEXT_MODEL", "text-test")
	t.Setenv("BKCRAB_RAG_DOCUMENT_AI_RESPONSE_FORMAT", "json_object")
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

	if dst.Features.AdvancedParsingEnabled || !dst.Features.OfficeParsingEnabled || !dst.Features.TextEnrichmentEnabled ||
		!dst.Features.GenerationShadowReadEnabled || !dst.Features.GenerationResolverAuthoritative {
		t.Fatalf("feature flag overlay = %+v", dst.Features)
	}
	if dst.DocumentAI.Endpoint != "http://document-ai.internal/v1" ||
		dst.DocumentAI.APIKey != "document-ai-secret" || dst.DocumentAI.VisionModel != "vision-test" ||
		dst.DocumentAI.TextModel != "text-test" ||
		dst.DocumentAI.ResponseFormat != RAGDocumentAIResponseFormatJSONObject ||
		dst.DocumentAI.TimeoutMS != 90000 ||
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
