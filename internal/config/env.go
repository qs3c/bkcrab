package config

import (
	"os"
	"strconv"
	"strings"
)

const RAGLegacyTaskMigrationModeOfflineV1 = "offline-v1"

// EnvConfig 是引导配置：存储 DSN、网关端口、沙箱后端。在进程启动时
// 从 BKCRAB_* 环境变量读取——没有配置文件。所有用户可见的配置
// （提供者、渠道、agent 等）都存储在数据库中。
//
// 在进程/容器层面以 `BKCRAB_<大写下划线形式>`（或下面 `env:` 标签中的
// 显式名称）设置。systemd unit、docker-compose、k8s deployment env 是
// 规范的设置位置。
type EnvConfig struct {
	Gateway EnvGateway
	Storage EnvStorage
	Sandbox EnvSandbox
	Log     EnvLog
	RAG     RAGCfg

	// RAGLegacyTaskMigrationMode is a deployment-only acknowledgement for the
	// offline legacy index-task backfill. It is intentionally separate from
	// Storage.AutoMigrate: schema expansion is safe during normal startup, while
	// legacy task backfill requires the maintenance window documented in
	// docs/database.md.
	RAGLegacyTaskMigrationMode string

	ragRerankerEnabledSet                bool
	ragAdvancedEnabledSet                bool
	ragOfficeEnabledSet                  bool
	ragEnrichmentEnabledSet              bool
	ragDocumentAIAllowPrivateEndpointSet bool
	ragDocumentAIAllowedEndpointHostsSet bool
}

type EnvGateway struct {
	Port int    // BKCRAB_PORT       — 默认 18953
	Bind string // BKCRAB_BIND       — "loopback"（默认）或 "all"
}

type EnvStorage struct {
	Type        string // BKCRAB_STORAGE_TYPE  — 默认 "mysql"
	DSN         string // BKCRAB_STORAGE_DSN   — 必需；无 SQLite 回退
	AutoMigrate bool   // BKCRAB_STORAGE_AUTO_MIGRATE — 默认 true
}

type EnvSandbox struct {
	Enabled         bool   // BKCRAB_SANDBOX_ENABLED
	Backend         string // BKCRAB_SANDBOX_BACKEND  — "docker"、"e2b" 或 "boxlite"
	Image           string // BKCRAB_SANDBOX_IMAGE
	E2BKey          string // E2B_API_KEY
	BoxliteURL      string // BKCRAB_SANDBOX_BOXLITE_URL — 完整基础 URL，例如 https://api.boxlite.ai/v1
	BoxliteClientID string // BKCRAB_SANDBOX_BOXLITE_CLIENT_ID — 默认 "default"
	BoxliteKey      string // BOXLITE_API_KEY — 作为 Authorization: Bearer 发送的 apikey
	BoxlitePrefix   string // BKCRAB_SANDBOX_BOXLITE_PREFIX — 工作区前缀，默认 "default"
}

type EnvLog struct {
	Level string // BKCRAB_LOG_LEVEL — "debug" / "info" / "warn" / "error"
}

// LoadEnv 从 BKCRAB_* 环境变量读取引导配置。没有配置文件：
// 部署时设置是部署清单（systemd / docker-compose / k8s env）的一部分。
func LoadEnv() *EnvConfig {
	cfg := &EnvConfig{
		// MySQL 默认必需。AutoMigrate 创建全新 schema，但仍需 DSN，
		// 且启动时绝不回退到 SQLite。
		Storage: EnvStorage{Type: "mysql", AutoMigrate: true},
	}

	if v := os.Getenv("BKCRAB_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Gateway.Port = p
		}
	}
	if v := os.Getenv("BKCRAB_BIND"); v != "" {
		cfg.Gateway.Bind = v
	}

	if v := os.Getenv("BKCRAB_STORAGE_TYPE"); v != "" {
		cfg.Storage.Type = v
	}
	if v := os.Getenv("BKCRAB_STORAGE_DSN"); v != "" {
		cfg.Storage.DSN = v
	}
	if v := os.Getenv("BKCRAB_STORAGE_AUTO_MIGRATE"); v != "" {
		cfg.Storage.AutoMigrate = v == "true" || v == "1"
	}
	if v := os.Getenv("BKCRAB_RAG_LEGACY_TASK_MIGRATION_MODE"); v != "" {
		cfg.RAGLegacyTaskMigrationMode = strings.TrimSpace(v)
	}

	if v := os.Getenv("BKCRAB_SANDBOX_ENABLED"); v != "" {
		cfg.Sandbox.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("BKCRAB_SANDBOX_BACKEND"); v != "" {
		cfg.Sandbox.Backend = v
		// 设置后端意味着操作员希望沙箱开启；这反映了
		// 之前的 LoadEnv 行为。
		cfg.Sandbox.Enabled = true
	}
	if v := os.Getenv("BKCRAB_SANDBOX_IMAGE"); v != "" {
		cfg.Sandbox.Image = v
	}
	if v := os.Getenv("E2B_API_KEY"); v != "" {
		cfg.Sandbox.E2BKey = v
	}
	if v := os.Getenv("BKCRAB_SANDBOX_BOXLITE_URL"); v != "" {
		cfg.Sandbox.BoxliteURL = v
	}
	if v := os.Getenv("BKCRAB_SANDBOX_BOXLITE_CLIENT_ID"); v != "" {
		cfg.Sandbox.BoxliteClientID = v
	}
	if v := os.Getenv("BOXLITE_API_KEY"); v != "" {
		cfg.Sandbox.BoxliteKey = v
	}
	if v := os.Getenv("BKCRAB_SANDBOX_BOXLITE_PREFIX"); v != "" {
		cfg.Sandbox.BoxlitePrefix = v
	}

	if v := os.Getenv("BKCRAB_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}

	if v := os.Getenv("BKCRAB_RAG_MILVUS_ADDRESS"); v != "" {
		cfg.RAG.Milvus.Address = v
	}
	if v := os.Getenv("BKCRAB_RAG_MILVUS_USERNAME"); v != "" {
		cfg.RAG.Milvus.Username = v
	}
	if v := os.Getenv("BKCRAB_RAG_MILVUS_PASSWORD"); v != "" {
		cfg.RAG.Milvus.Password = v
	}
	if v := os.Getenv("BKCRAB_RAG_EMBEDDING_ENDPOINT"); v != "" {
		cfg.RAG.Embedding.Endpoint = v
	}
	if v := os.Getenv("BKCRAB_RAG_EMBEDDING_API_KEY"); v != "" {
		cfg.RAG.Embedding.APIKey = v
	}
	if v := os.Getenv("BKCRAB_RAG_EMBEDDING_MODEL"); v != "" {
		cfg.RAG.Embedding.Model = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_EMBEDDING_DIMS"); v > 0 {
		cfg.RAG.Embedding.Dims = v
	}
	if v := os.Getenv("BKCRAB_RAG_RERANKER_ENABLED"); v != "" {
		cfg.RAG.Reranker.Enabled = v == "true" || v == "1"
		cfg.ragRerankerEnabledSet = true
	}
	if v := os.Getenv("BKCRAB_RAG_RERANKER_ENDPOINT"); v != "" {
		cfg.RAG.Reranker.Endpoint = v
	}
	if v := os.Getenv("BKCRAB_RAG_RERANKER_API_KEY"); v != "" {
		cfg.RAG.Reranker.APIKey = v
	}
	if v := os.Getenv("BKCRAB_RAG_RERANKER_MODEL"); v != "" {
		cfg.RAG.Reranker.Model = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_RERANKER_TIMEOUT_MS"); v > 0 {
		cfg.RAG.Reranker.TimeoutMS = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_RERANKER_CANDIDATE_TOP_K"); v > 0 {
		cfg.RAG.Reranker.CandidateTopK = v
	}
	if v := os.Getenv("BKCRAB_RAG_RERANKER_MIN_SCORE"); v != "" {
		if score, err := strconv.ParseFloat(v, 64); err == nil && score > 0 && score <= 1 {
			cfg.RAG.Reranker.MinScore = score
		}
	}
	if v, ok := lookupEnvBool("BKCRAB_RAG_ADVANCED_ENABLED"); ok {
		cfg.RAG.Features.AdvancedParsingEnabled = v
		cfg.ragAdvancedEnabledSet = true
	}
	if v, ok := lookupEnvBool("BKCRAB_RAG_OFFICE_ENABLED"); ok {
		cfg.RAG.Features.OfficeParsingEnabled = v
		cfg.ragOfficeEnabledSet = true
	}
	if v, ok := lookupEnvBool("BKCRAB_RAG_ENRICHMENT_ENABLED"); ok {
		cfg.RAG.Features.TextEnrichmentEnabled = v
		cfg.ragEnrichmentEnabledSet = true
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_API_TYPE"); v != "" {
		cfg.RAG.DocumentAI.APIType = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_ENDPOINT"); v != "" {
		cfg.RAG.DocumentAI.Endpoint = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY"); v != "" {
		cfg.RAG.DocumentAI.APIKey = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_VISION_MODEL"); v != "" {
		cfg.RAG.DocumentAI.VisionModel = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_TEXT_MODEL"); v != "" {
		cfg.RAG.DocumentAI.TextModel = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_RESPONSE_FORMAT"); v != "" {
		cfg.RAG.DocumentAI.ResponseFormat = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_DOCUMENT_AI_TIMEOUT_MS"); v > 0 {
		cfg.RAG.DocumentAI.TimeoutMS = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_DOCUMENT_AI_VISION_CONCURRENCY"); v > 0 {
		cfg.RAG.DocumentAI.VisionConcurrency = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_DOCUMENT_AI_ENRICHMENT_CONCURRENCY"); v > 0 {
		cfg.RAG.DocumentAI.EnrichmentConcurrency = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_VISION_PROMPT_VERSION"); v != "" {
		cfg.RAG.DocumentAI.VisionPromptVersion = v
	}
	if v := os.Getenv("BKCRAB_RAG_DOCUMENT_AI_ENRICHMENT_PROMPT_VERSION"); v != "" {
		cfg.RAG.DocumentAI.EnrichmentPromptVersion = v
	}
	if v, ok := os.LookupEnv("BKCRAB_RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS"); ok {
		cfg.RAG.DocumentAI.AllowedEndpointHosts = splitEnvList(v)
		cfg.ragDocumentAIAllowedEndpointHostsSet = true
	}
	if v, ok := lookupEnvBool("BKCRAB_RAG_DOCUMENT_AI_ALLOW_PRIVATE_ENDPOINT"); ok {
		cfg.RAG.DocumentAI.AllowPrivateEndpoint = v
		cfg.ragDocumentAIAllowPrivateEndpointSet = true
	}
	if v := os.Getenv("BKCRAB_RAG_PARSER_ENDPOINT"); v != "" {
		cfg.RAG.ParserSidecar.Endpoint = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_PARSER_TIMEOUT_MS"); v > 0 {
		cfg.RAG.ParserSidecar.TimeoutMS = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_FILE_MB"); v > 0 {
		cfg.RAG.Limits.MaxFileMB = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_DOCS_PER_KB"); v > 0 {
		cfg.RAG.Limits.MaxDocsPerKB = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_KBS_PER_USER"); v > 0 {
		cfg.RAG.Limits.MaxKBsPerUser = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_PAGES_PER_DOCUMENT"); v > 0 {
		cfg.RAG.Limits.MaxPagesPerDocument = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_VISION_PAGES_PER_DOCUMENT"); v > 0 {
		cfg.RAG.Limits.MaxVisionPagesPerDocument = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_VISION_ASSETS_PER_DOCUMENT"); v > 0 {
		cfg.RAG.Limits.MaxVisionAssetsPerDocument = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_ENRICHMENT_BLOCKS_PER_DOCUMENT"); v > 0 {
		cfg.RAG.Limits.MaxEnrichmentBlocksPerDocument = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_REQUESTS"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAIRequests = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_TOKENS"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAITokens = v
	}
	if v := positiveEnvFloat("BKCRAB_RAG_LIMITS_MAX_ESTIMATED_DOCUMENT_AI_COST_USD"); v > 0 {
		cfg.RAG.Limits.MaxEstimatedDocumentAICostUSD = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_RESPONSE_BYTES"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAIResponseBytes = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_OUTPUT_TOKENS"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAIOutputTokens = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_JSON_DEPTH"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAIJSONDepth = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_REQUESTS_PER_USER_PER_DAY"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAIRequestsPerUserPerDay = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_DOCUMENT_AI_TOKENS_PER_USER_PER_DAY"); v > 0 {
		cfg.RAG.Limits.MaxDocumentAITokensPerUserPerDay = v
	}
	if v := positiveEnvFloat("BKCRAB_RAG_LIMITS_MAX_ESTIMATED_DOCUMENT_AI_COST_PER_USER_PER_DAY_USD"); v > 0 {
		cfg.RAG.Limits.MaxEstimatedDocumentAICostPerUserPerDayUSD = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_PENDING_ADVANCED_TASKS_PER_USER"); v > 0 {
		cfg.RAG.Limits.MaxPendingAdvancedTasksPerUser = v
	}
	if v := firstPositiveEnvInt("BKCRAB_RAG_LIMITS_MIN_ADVANCED_REINDEX_INTERVAL", "BKCRAB_RAG_LIMITS_MIN_ADVANCED_REINDEX_INTERVAL_SECONDS"); v > 0 {
		cfg.RAG.Limits.MinAdvancedReindexInterval = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_VISION_INPUT_BYTES"); v > 0 {
		cfg.RAG.Limits.MaxVisionInputBytes = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_SEARCH_CONTENT_BYTES"); v > 0 {
		cfg.RAG.Limits.MaxSearchContentBytes = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_MILVUS_FILTER_BYTES"); v > 0 {
		cfg.RAG.Limits.MaxMilvusFilterBytes = v
	}
	if v := firstPositiveEnvInt("BKCRAB_RAG_LIMITS_INDEX_GC_GRACE_PERIOD", "BKCRAB_RAG_LIMITS_INDEX_GC_GRACE_PERIOD_SECONDS"); v > 0 {
		cfg.RAG.Limits.IndexGCGracePeriod = v
	}
	if v := firstPositiveEnvInt("BKCRAB_RAG_LIMITS_STAGING_ARTIFACT_TTL", "BKCRAB_RAG_LIMITS_STAGING_ARTIFACT_TTL_SECONDS"); v > 0 {
		cfg.RAG.Limits.StagingArtifactTTL = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_CACHE_FINGERPRINTS_PER_DOCUMENT"); v > 0 {
		cfg.RAG.Limits.MaxCacheFingerprintsPerDocument = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_ASSETS_PER_DOCUMENT"); v > 0 {
		cfg.RAG.Limits.MaxAssetsPerDocument = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_ASSET_BYTES"); v > 0 {
		cfg.RAG.Limits.MaxAssetBytes = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_EXTRACTED_BYTES"); v > 0 {
		cfg.RAG.Limits.MaxExtractedBytes = v
	}
	if v := positiveEnvInt64("BKCRAB_RAG_LIMITS_MAX_IMAGE_PIXELS"); v > 0 {
		cfg.RAG.Limits.MaxImagePixels = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_PDF_RENDER_DPI"); v > 0 {
		cfg.RAG.Limits.PDFRenderDPI = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_THUMBNAIL_MAX_EDGE"); v > 0 {
		cfg.RAG.Limits.ThumbnailMaxEdge = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_DISPLAY_MAX_EDGE"); v > 0 {
		cfg.RAG.Limits.DisplayMaxEdge = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_PARSE_TIMEOUT_MS"); v > 0 {
		cfg.RAG.Limits.ParseTimeoutMS = v
	}
	return cfg
}

func positiveEnvInt(name string) int {
	n, err := strconv.Atoi(os.Getenv(name))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func positiveEnvInt64(name string) int64 {
	n, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func positiveEnvFloat(name string) float64 {
	n, err := strconv.ParseFloat(os.Getenv(name), 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func firstPositiveEnvInt(names ...string) int {
	for _, name := range names {
		if n := positiveEnvInt(name); n > 0 {
			return n
		}
	}
	return 0
}

func lookupEnvBool(name string) (bool, bool) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return false, false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true":
		return true, true
	case "0", "false":
		return false, true
	default:
		return false, false
	}
}

func splitEnvList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// applyObjectStoreEnv 将 BKCRAB_OBJECT_STORE_* 环境变量读入 cfg。
func applyObjectStoreEnv(cfg *Config) {
	read := func(key string) string { return os.Getenv("BKCRAB_OBJECT_STORE_" + key) }
	if v := read("TYPE"); v != "" {
		cfg.ObjectStore.Type = v
	}
	if v := read("LOCAL_ROOT"); v != "" {
		cfg.ObjectStore.Local.Root = v
	}
	if v := read("REGION"); v != "" {
		cfg.ObjectStore.S3.Region = v
	}
	if v := read("BUCKET"); v != "" {
		cfg.ObjectStore.S3.Bucket = v
	}
	if v := read("PREFIX"); v != "" {
		cfg.ObjectStore.S3.Prefix = v
	}
	if v := read("ACCESSKEY"); v != "" {
		cfg.ObjectStore.S3.AccessKey = v
	}
	if v := read("SECRETKEY"); v != "" {
		cfg.ObjectStore.S3.SecretKey = v
	}
	if v := read("ACCOUNTID"); v != "" {
		cfg.ObjectStore.AccountID = v
	}
	if v := read("ENDPOINT"); v != "" {
		cfg.ObjectStore.S3.Endpoint = v
	}
	if v := read("USESSL"); v != "" {
		cfg.ObjectStore.S3.UseSSL = v == "true" || v == "1"
	}
	if v := read("ALIYUN_INTERNAL"); v != "" {
		cfg.ObjectStore.AliyunIntern = v == "true" || v == "1"
	}
}

// ScrubBootSecrets 在引导配置读取后从进程环境中移除含凭据的环境变量。
// 在网关构造完成后从守护进程入口调用一次。
//
// 原因：agent 运行的每个 shell 命令默认继承守护进程的环境。即使有
// 子进程级别的清理（见 tools/env_scrub.go），子进程仍可以以同一 UID
// 读取 /proc/<daemon_pid>/environ 并恢复父进程仍设置的任何内容。
// 在父进程取消设置即关闭了该路径。
//
// 权衡：gateway.go 中的运行时热重载路径（readObjectStoreCfg /
// readSystemSandboxCfg）会重新调用 LoadEnv，并在清理后看到这些键的
// 空值。这是故意的——环境变量被视为一次性引导覆盖，而非实时配置源。
// 希望在运行时轮换凭据的操作员应通过管理 UI 编辑数据库存储的配置。
var bootSecretEnvKeys = []string{
	"BKCRAB_STORAGE_DSN",
	"BKCRAB_OBJECT_STORE_TYPE",
	"BKCRAB_OBJECT_STORE_LOCAL_ROOT",
	"BKCRAB_OBJECT_STORE_REGION",
	"BKCRAB_OBJECT_STORE_BUCKET",
	"BKCRAB_OBJECT_STORE_PREFIX",
	"BKCRAB_OBJECT_STORE_ACCESSKEY",
	"BKCRAB_OBJECT_STORE_SECRETKEY",
	"BKCRAB_OBJECT_STORE_ACCOUNTID",
	"BKCRAB_OBJECT_STORE_ENDPOINT",
	"BKCRAB_OBJECT_STORE_USESSL",
	"BKCRAB_OBJECT_STORE_ALIYUN_INTERNAL",
	"BOXLITE_API_KEY",
	"E2B_API_KEY",
	"BKCRAB_RAG_MILVUS_PASSWORD",
	"BKCRAB_RAG_EMBEDDING_API_KEY",
	"BKCRAB_RAG_RERANKER_API_KEY",
	"BKCRAB_RAG_DOCUMENT_AI_API_KEY",
}

func ScrubBootSecrets() {
	for _, k := range bootSecretEnvKeys {
		_ = os.Unsetenv(k)
	}
}

// ApplySystemRAG overlays one-time BKCRAB_RAG_* bootstrap values onto the
// system RAG configuration loaded from the database. Keep this separate from
// ApplyToConfig: that method is also used after system -> user scope merging,
// where applying system environment defaults last would incorrectly suppress
// a user's embedding override.
func (e *EnvConfig) ApplySystemRAG(dst *RAGCfg) {
	if e == nil || dst == nil {
		return
	}
	if e.RAG.Milvus.Address != "" {
		dst.Milvus.Address = e.RAG.Milvus.Address
	}
	if e.RAG.Milvus.Username != "" {
		dst.Milvus.Username = e.RAG.Milvus.Username
	}
	if e.RAG.Milvus.Password != "" {
		dst.Milvus.Password = e.RAG.Milvus.Password
	}
	if e.RAG.Embedding.Endpoint != "" {
		dst.Embedding.Endpoint = e.RAG.Embedding.Endpoint
	}
	if e.RAG.Embedding.APIKey != "" {
		dst.Embedding.APIKey = e.RAG.Embedding.APIKey
	}
	if e.RAG.Embedding.Model != "" {
		dst.Embedding.Model = e.RAG.Embedding.Model
	}
	if e.RAG.Embedding.Dims > 0 {
		dst.Embedding.Dims = e.RAG.Embedding.Dims
	}
	if e.ragRerankerEnabledSet {
		dst.Reranker.Enabled = e.RAG.Reranker.Enabled
	}
	if e.RAG.Reranker.Endpoint != "" {
		dst.Reranker.Endpoint = e.RAG.Reranker.Endpoint
	}
	if e.RAG.Reranker.APIKey != "" {
		dst.Reranker.APIKey = e.RAG.Reranker.APIKey
	}
	if e.RAG.Reranker.Model != "" {
		dst.Reranker.Model = e.RAG.Reranker.Model
	}
	if e.RAG.Reranker.TimeoutMS > 0 {
		dst.Reranker.TimeoutMS = e.RAG.Reranker.TimeoutMS
	}
	if e.RAG.Reranker.CandidateTopK > 0 {
		dst.Reranker.CandidateTopK = e.RAG.Reranker.CandidateTopK
	}
	if e.RAG.Reranker.MinScore > 0 {
		dst.Reranker.MinScore = e.RAG.Reranker.MinScore
	}
	if e.ragAdvancedEnabledSet {
		dst.Features.AdvancedParsingEnabled = e.RAG.Features.AdvancedParsingEnabled
	}
	if e.ragOfficeEnabledSet {
		dst.Features.OfficeParsingEnabled = e.RAG.Features.OfficeParsingEnabled
	}
	if e.ragEnrichmentEnabledSet {
		dst.Features.TextEnrichmentEnabled = e.RAG.Features.TextEnrichmentEnabled
	}
	if e.RAG.DocumentAI.APIType != "" {
		dst.DocumentAI.APIType = e.RAG.DocumentAI.APIType
	}
	if e.RAG.DocumentAI.Endpoint != "" {
		dst.DocumentAI.Endpoint = e.RAG.DocumentAI.Endpoint
	}
	if e.RAG.DocumentAI.APIKey != "" {
		dst.DocumentAI.APIKey = e.RAG.DocumentAI.APIKey
	}
	if e.RAG.DocumentAI.VisionModel != "" {
		dst.DocumentAI.VisionModel = e.RAG.DocumentAI.VisionModel
	}
	if e.RAG.DocumentAI.TextModel != "" {
		dst.DocumentAI.TextModel = e.RAG.DocumentAI.TextModel
	}
	if e.RAG.DocumentAI.ResponseFormat != "" {
		dst.DocumentAI.ResponseFormat = e.RAG.DocumentAI.ResponseFormat
	}
	if e.RAG.DocumentAI.TimeoutMS > 0 {
		dst.DocumentAI.TimeoutMS = e.RAG.DocumentAI.TimeoutMS
	}
	if e.RAG.DocumentAI.VisionConcurrency > 0 {
		dst.DocumentAI.VisionConcurrency = e.RAG.DocumentAI.VisionConcurrency
	}
	if e.RAG.DocumentAI.EnrichmentConcurrency > 0 {
		dst.DocumentAI.EnrichmentConcurrency = e.RAG.DocumentAI.EnrichmentConcurrency
	}
	if e.RAG.DocumentAI.VisionPromptVersion != "" {
		dst.DocumentAI.VisionPromptVersion = e.RAG.DocumentAI.VisionPromptVersion
	}
	if e.RAG.DocumentAI.EnrichmentPromptVersion != "" {
		dst.DocumentAI.EnrichmentPromptVersion = e.RAG.DocumentAI.EnrichmentPromptVersion
	}
	if e.ragDocumentAIAllowedEndpointHostsSet {
		dst.DocumentAI.AllowedEndpointHosts = append([]string(nil), e.RAG.DocumentAI.AllowedEndpointHosts...)
	}
	if e.ragDocumentAIAllowPrivateEndpointSet {
		dst.DocumentAI.AllowPrivateEndpoint = e.RAG.DocumentAI.AllowPrivateEndpoint
	}
	if e.RAG.ParserSidecar.Endpoint != "" {
		dst.ParserSidecar.Endpoint = e.RAG.ParserSidecar.Endpoint
	}
	if e.RAG.ParserSidecar.TimeoutMS > 0 {
		dst.ParserSidecar.TimeoutMS = e.RAG.ParserSidecar.TimeoutMS
	}
	if e.RAG.Limits.MaxFileMB > 0 {
		dst.Limits.MaxFileMB = e.RAG.Limits.MaxFileMB
	}
	if e.RAG.Limits.MaxDocsPerKB > 0 {
		dst.Limits.MaxDocsPerKB = e.RAG.Limits.MaxDocsPerKB
	}
	if e.RAG.Limits.MaxKBsPerUser > 0 {
		dst.Limits.MaxKBsPerUser = e.RAG.Limits.MaxKBsPerUser
	}
	if e.RAG.Limits.MaxPagesPerDocument > 0 {
		dst.Limits.MaxPagesPerDocument = e.RAG.Limits.MaxPagesPerDocument
	}
	if e.RAG.Limits.MaxVisionPagesPerDocument > 0 {
		dst.Limits.MaxVisionPagesPerDocument = e.RAG.Limits.MaxVisionPagesPerDocument
	}
	if e.RAG.Limits.MaxVisionAssetsPerDocument > 0 {
		dst.Limits.MaxVisionAssetsPerDocument = e.RAG.Limits.MaxVisionAssetsPerDocument
	}
	if e.RAG.Limits.MaxEnrichmentBlocksPerDocument > 0 {
		dst.Limits.MaxEnrichmentBlocksPerDocument = e.RAG.Limits.MaxEnrichmentBlocksPerDocument
	}
	if e.RAG.Limits.MaxDocumentAIRequests > 0 {
		dst.Limits.MaxDocumentAIRequests = e.RAG.Limits.MaxDocumentAIRequests
	}
	if e.RAG.Limits.MaxDocumentAITokens > 0 {
		dst.Limits.MaxDocumentAITokens = e.RAG.Limits.MaxDocumentAITokens
	}
	if e.RAG.Limits.MaxEstimatedDocumentAICostUSD > 0 {
		dst.Limits.MaxEstimatedDocumentAICostUSD = e.RAG.Limits.MaxEstimatedDocumentAICostUSD
	}
	if e.RAG.Limits.MaxDocumentAIResponseBytes > 0 {
		dst.Limits.MaxDocumentAIResponseBytes = e.RAG.Limits.MaxDocumentAIResponseBytes
	}
	if e.RAG.Limits.MaxDocumentAIOutputTokens > 0 {
		dst.Limits.MaxDocumentAIOutputTokens = e.RAG.Limits.MaxDocumentAIOutputTokens
	}
	if e.RAG.Limits.MaxDocumentAIJSONDepth > 0 {
		dst.Limits.MaxDocumentAIJSONDepth = e.RAG.Limits.MaxDocumentAIJSONDepth
	}
	if e.RAG.Limits.MaxDocumentAIRequestsPerUserPerDay > 0 {
		dst.Limits.MaxDocumentAIRequestsPerUserPerDay = e.RAG.Limits.MaxDocumentAIRequestsPerUserPerDay
	}
	if e.RAG.Limits.MaxDocumentAITokensPerUserPerDay > 0 {
		dst.Limits.MaxDocumentAITokensPerUserPerDay = e.RAG.Limits.MaxDocumentAITokensPerUserPerDay
	}
	if e.RAG.Limits.MaxEstimatedDocumentAICostPerUserPerDayUSD > 0 {
		dst.Limits.MaxEstimatedDocumentAICostPerUserPerDayUSD = e.RAG.Limits.MaxEstimatedDocumentAICostPerUserPerDayUSD
	}
	if e.RAG.Limits.MaxPendingAdvancedTasksPerUser > 0 {
		dst.Limits.MaxPendingAdvancedTasksPerUser = e.RAG.Limits.MaxPendingAdvancedTasksPerUser
	}
	if e.RAG.Limits.MinAdvancedReindexInterval > 0 {
		dst.Limits.MinAdvancedReindexInterval = e.RAG.Limits.MinAdvancedReindexInterval
	}
	if e.RAG.Limits.MaxVisionInputBytes > 0 {
		dst.Limits.MaxVisionInputBytes = e.RAG.Limits.MaxVisionInputBytes
	}
	if e.RAG.Limits.MaxSearchContentBytes > 0 {
		dst.Limits.MaxSearchContentBytes = e.RAG.Limits.MaxSearchContentBytes
	}
	if e.RAG.Limits.MaxMilvusFilterBytes > 0 {
		dst.Limits.MaxMilvusFilterBytes = e.RAG.Limits.MaxMilvusFilterBytes
	}
	if e.RAG.Limits.IndexGCGracePeriod > 0 {
		dst.Limits.IndexGCGracePeriod = e.RAG.Limits.IndexGCGracePeriod
	}
	if e.RAG.Limits.StagingArtifactTTL > 0 {
		dst.Limits.StagingArtifactTTL = e.RAG.Limits.StagingArtifactTTL
	}
	if e.RAG.Limits.MaxCacheFingerprintsPerDocument > 0 {
		dst.Limits.MaxCacheFingerprintsPerDocument = e.RAG.Limits.MaxCacheFingerprintsPerDocument
	}
	if e.RAG.Limits.MaxAssetsPerDocument > 0 {
		dst.Limits.MaxAssetsPerDocument = e.RAG.Limits.MaxAssetsPerDocument
	}
	if e.RAG.Limits.MaxAssetBytes > 0 {
		dst.Limits.MaxAssetBytes = e.RAG.Limits.MaxAssetBytes
	}
	if e.RAG.Limits.MaxExtractedBytes > 0 {
		dst.Limits.MaxExtractedBytes = e.RAG.Limits.MaxExtractedBytes
	}
	if e.RAG.Limits.MaxImagePixels > 0 {
		dst.Limits.MaxImagePixels = e.RAG.Limits.MaxImagePixels
	}
	if e.RAG.Limits.PDFRenderDPI > 0 {
		dst.Limits.PDFRenderDPI = e.RAG.Limits.PDFRenderDPI
	}
	if e.RAG.Limits.ThumbnailMaxEdge > 0 {
		dst.Limits.ThumbnailMaxEdge = e.RAG.Limits.ThumbnailMaxEdge
	}
	if e.RAG.Limits.DisplayMaxEdge > 0 {
		dst.Limits.DisplayMaxEdge = e.RAG.Limits.DisplayMaxEdge
	}
	if e.RAG.Limits.ParseTimeoutMS > 0 {
		dst.Limits.ParseTimeoutMS = e.RAG.Limits.ParseTimeoutMS
	}
}

// ApplyToConfig 将环境派生的值叠加到运行时 Config 上。由网关引导
// 使用，用于在数据库存储的对象存储命名空间之上叠加 BKCRAB_OBJECT_STORE_*。
func (e *EnvConfig) ApplyToConfig(cfg *Config) {
	if e.Gateway.Port > 0 {
		cfg.Gateway.Port = e.Gateway.Port
	}
	if e.Gateway.Bind != "" {
		cfg.Gateway.Bind = e.Gateway.Bind
	}
	if e.Storage.Type != "" {
		cfg.Storage.Type = e.Storage.Type
	}
	if e.Storage.DSN != "" {
		cfg.Storage.DSN = e.Storage.DSN
	}
	if e.Storage.AutoMigrate {
		cfg.Storage.AutoMigrate = true
	}
	if e.Sandbox.Enabled {
		cfg.Sandbox.Enabled = true
		if e.Sandbox.Backend != "" {
			cfg.Sandbox.Backend = e.Sandbox.Backend
		}
		if e.Sandbox.Image != "" {
			cfg.Sandbox.Image = e.Sandbox.Image
		}
		if e.Sandbox.E2BKey != "" {
			cfg.Sandbox.E2BKey = e.Sandbox.E2BKey
		}
		if e.Sandbox.BoxliteURL != "" {
			cfg.Sandbox.BoxliteURL = e.Sandbox.BoxliteURL
		}
		if e.Sandbox.BoxliteClientID != "" {
			cfg.Sandbox.BoxliteClientID = e.Sandbox.BoxliteClientID
		}
		if e.Sandbox.BoxliteKey != "" {
			cfg.Sandbox.BoxliteKey = e.Sandbox.BoxliteKey
		}
		if e.Sandbox.BoxlitePrefix != "" {
			cfg.Sandbox.BoxlitePrefix = e.Sandbox.BoxlitePrefix
		}
	}
	applyObjectStoreEnv(cfg)
}
