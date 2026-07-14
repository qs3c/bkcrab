package config

import (
	"os"
	"strconv"
)

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
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_FILE_MB"); v > 0 {
		cfg.RAG.Limits.MaxFileMB = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_DOCS_PER_KB"); v > 0 {
		cfg.RAG.Limits.MaxDocsPerKB = v
	}
	if v := positiveEnvInt("BKCRAB_RAG_LIMITS_MAX_KBS_PER_USER"); v > 0 {
		cfg.RAG.Limits.MaxKBsPerUser = v
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
	if e.RAG.Limits.MaxFileMB > 0 {
		dst.Limits.MaxFileMB = e.RAG.Limits.MaxFileMB
	}
	if e.RAG.Limits.MaxDocsPerKB > 0 {
		dst.Limits.MaxDocsPerKB = e.RAG.Limits.MaxDocsPerKB
	}
	if e.RAG.Limits.MaxKBsPerUser > 0 {
		dst.Limits.MaxKBsPerUser = e.RAG.Limits.MaxKBsPerUser
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
