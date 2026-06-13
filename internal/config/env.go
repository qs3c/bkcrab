package config

import (
	"os"
	"strconv"
)

// EnvConfig 是引导配置：存储 DSN、网关端口、沙箱后端。在进程启动时
// 从 BKCLAW_* 环境变量读取——没有配置文件。所有用户可见的配置
//（提供者、渠道、agent 等）都存储在数据库中。
//
// 在进程/容器层面以 `BKCLAW_<大写下划线形式>`（或下面 `env:` 标签中的
// 显式名称）设置。systemd unit、docker-compose、k8s deployment env 是
// 规范的设置位置。
type EnvConfig struct {
	Gateway EnvGateway
	Storage EnvStorage
	Sandbox EnvSandbox
	Log     EnvLog
}

type EnvGateway struct {
	Port int    // BKCLAW_PORT       — 默认 18953
	Bind string // BKCLAW_BIND       — "loopback"（默认）或 "all"
}

type EnvStorage struct {
	Type        string // BKCLAW_STORAGE_TYPE  — 默认 "mysql"
	DSN         string // BKCLAW_STORAGE_DSN   — 必需；无 SQLite 回退
	AutoMigrate bool   // BKCLAW_STORAGE_AUTO_MIGRATE — 默认 true
}

type EnvSandbox struct {
	Enabled         bool   // BKCLAW_SANDBOX_ENABLED
	Backend         string // BKCLAW_SANDBOX_BACKEND  — "docker"、"e2b" 或 "boxlite"
	Image           string // BKCLAW_SANDBOX_IMAGE
	E2BKey          string // E2B_API_KEY
	BoxliteURL      string // BKCLAW_SANDBOX_BOXLITE_URL — 完整基础 URL，例如 https://api.boxlite.ai/v1
	BoxliteClientID string // BKCLAW_SANDBOX_BOXLITE_CLIENT_ID — 默认 "default"
	BoxliteKey      string // BOXLITE_API_KEY — 作为 Authorization: Bearer 发送的 apikey
	BoxlitePrefix   string // BKCLAW_SANDBOX_BOXLITE_PREFIX — 工作区前缀，默认 "default"
}

type EnvLog struct {
	Level string // BKCLAW_LOG_LEVEL — "debug" / "info" / "warn" / "error"
}

// LoadEnv 从 BKCLAW_* 环境变量读取引导配置。没有配置文件：
// 部署时设置是部署清单（systemd / docker-compose / k8s env）的一部分。
func LoadEnv() *EnvConfig {
	cfg := &EnvConfig{
		// MySQL 默认必需。AutoMigrate 创建全新 schema，但仍需 DSN，
		// 且启动时绝不回退到 SQLite。
		Storage: EnvStorage{Type: "mysql", AutoMigrate: true},
	}

	if v := os.Getenv("BKCLAW_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Gateway.Port = p
		}
	}
	if v := os.Getenv("BKCLAW_BIND"); v != "" {
		cfg.Gateway.Bind = v
	}

	if v := os.Getenv("BKCLAW_STORAGE_TYPE"); v != "" {
		cfg.Storage.Type = v
	}
	if v := os.Getenv("BKCLAW_STORAGE_DSN"); v != "" {
		cfg.Storage.DSN = v
	}
	if v := os.Getenv("BKCLAW_STORAGE_AUTO_MIGRATE"); v != "" {
		cfg.Storage.AutoMigrate = v == "true" || v == "1"
	}

	if v := os.Getenv("BKCLAW_SANDBOX_ENABLED"); v != "" {
		cfg.Sandbox.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("BKCLAW_SANDBOX_BACKEND"); v != "" {
		cfg.Sandbox.Backend = v
		// 设置后端意味着操作员希望沙箱开启；这反映了
		// 之前的 LoadEnv 行为。
		cfg.Sandbox.Enabled = true
	}
	if v := os.Getenv("BKCLAW_SANDBOX_IMAGE"); v != "" {
		cfg.Sandbox.Image = v
	}
	if v := os.Getenv("E2B_API_KEY"); v != "" {
		cfg.Sandbox.E2BKey = v
	}
	if v := os.Getenv("BKCLAW_SANDBOX_BOXLITE_URL"); v != "" {
		cfg.Sandbox.BoxliteURL = v
	}
	if v := os.Getenv("BKCLAW_SANDBOX_BOXLITE_CLIENT_ID"); v != "" {
		cfg.Sandbox.BoxliteClientID = v
	}
	if v := os.Getenv("BOXLITE_API_KEY"); v != "" {
		cfg.Sandbox.BoxliteKey = v
	}
	if v := os.Getenv("BKCLAW_SANDBOX_BOXLITE_PREFIX"); v != "" {
		cfg.Sandbox.BoxlitePrefix = v
	}

	if v := os.Getenv("BKCLAW_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	return cfg
}

// applyObjectStoreEnv 将 BKCLAW_OBJECT_STORE_* 环境变量读入 cfg。
func applyObjectStoreEnv(cfg *Config) {
	read := func(key string) string { return os.Getenv("BKCLAW_OBJECT_STORE_" + key) }
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
func ScrubBootSecrets() {
	keys := []string{
		"BKCLAW_STORAGE_DSN",
		"BKCLAW_OBJECT_STORE_TYPE",
		"BKCLAW_OBJECT_STORE_LOCAL_ROOT",
		"BKCLAW_OBJECT_STORE_REGION",
		"BKCLAW_OBJECT_STORE_BUCKET",
		"BKCLAW_OBJECT_STORE_PREFIX",
		"BKCLAW_OBJECT_STORE_ACCESSKEY",
		"BKCLAW_OBJECT_STORE_SECRETKEY",
		"BKCLAW_OBJECT_STORE_ACCOUNTID",
		"BKCLAW_OBJECT_STORE_ENDPOINT",
		"BKCLAW_OBJECT_STORE_USESSL",
		"BKCLAW_OBJECT_STORE_ALIYUN_INTERNAL",
		"BOXLITE_API_KEY",
		"E2B_API_KEY",
	}
	for _, k := range keys {
		_ = os.Unsetenv(k)
	}
}

// ApplyToConfig 将环境派生的值叠加到运行时 Config 上。由网关引导
// 使用，用于在数据库存储的对象存储命名空间之上叠加 BKCLAW_OBJECT_STORE_*。
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
