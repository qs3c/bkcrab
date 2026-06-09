package config

import (
	"os"
	"strconv"
)

// EnvConfig is the bootstrap configuration: storage DSN, gateway port,
// sandbox backend. Read at process start from BKCLAW_* environment
// variables — there is no config file. Everything user-facing
// (providers, channels, agents) lives in the database.
//
// Set these as `BKCLAW_<UPPER_SNAKE_CASE>` (or the explicit name in
// the `env:` tag below) at the process / container level. systemd unit,
// docker-compose, k8s deployment env are the canonical places.
type EnvConfig struct {
	Gateway EnvGateway
	Storage EnvStorage
	Sandbox EnvSandbox
	Log     EnvLog
}

type EnvGateway struct {
	Port int    // BKCLAW_PORT       — default 18953
	Bind string // BKCLAW_BIND       — "loopback" (default) or "all"
}

type EnvStorage struct {
	Type        string // BKCLAW_STORAGE_TYPE  — "sqlite" (default) or "postgres"
	DSN         string // BKCLAW_STORAGE_DSN   — empty = sqlite at $BKCLAW_HOME/bkclaw.db
	AutoMigrate bool   // BKCLAW_STORAGE_AUTO_MIGRATE — default true
}

type EnvSandbox struct {
	Enabled         bool   // BKCLAW_SANDBOX_ENABLED
	Backend         string // BKCLAW_SANDBOX_BACKEND  — "docker", "e2b", or "boxlite"
	Image           string // BKCLAW_SANDBOX_IMAGE
	E2BKey          string // E2B_API_KEY
	BoxliteURL      string // BKCLAW_SANDBOX_BOXLITE_URL — full base URL e.g. https://api.boxlite.ai/v1
	BoxliteClientID string // BKCLAW_SANDBOX_BOXLITE_CLIENT_ID — default "default"
	BoxliteKey      string // BOXLITE_API_KEY — apikey sent as Authorization: Bearer
	BoxlitePrefix   string // BKCLAW_SANDBOX_BOXLITE_PREFIX — workspace prefix, default "default"
}

type EnvLog struct {
	Level string // BKCLAW_LOG_LEVEL — "debug" / "info" / "warn" / "error"
}

// LoadEnv reads the bootstrap configuration from BKCLAW_* environment
// variables. There is no config file: deployment-time settings are part
// of the deployment manifest (systemd / docker-compose / k8s env).
func LoadEnv() *EnvConfig {
	cfg := &EnvConfig{
		// Defaults — used when the env var isn't set. AutoMigrate=true
		// makes a fresh sqlite install boot without manual schema steps.
		Storage: EnvStorage{AutoMigrate: true},
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
		// Setting a backend implies the operator wants sandbox on; this
		// mirrors the previous LoadEnv behavior.
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

// applyObjectStoreEnv reads BKCLAW_OBJECT_STORE_* env vars into cfg.
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

// ScrubBootSecrets removes credential-bearing env vars from the
// process environment AFTER bootstrap config has been read. Call once
// from the daemon entry point after gateway construction.
//
// Why: every shell command the agent runs inherits the daemon's env
// by default. Even with subprocess-level scrubbing (see tools/env_scrub.go),
// a subprocess can read /proc/<daemon_pid>/environ as the same UID
// and recover anything still set on the parent. Unsetting at the
// parent closes that path.
//
// Trade-off: the runtime hot-reload paths in gateway.go
// (readObjectStoreCfg / readSystemSandboxCfg) re-call LoadEnv and
// would see empty values for these keys after scrub. That's
// intentional — env is treated as a one-time bootstrap override,
// not a live config source. Operators wanting to rotate credentials
// at runtime should edit the DB-stored config via admin UI.
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

// ApplyToConfig overlays env-derived values onto a runtime Config. Used
// by gateway boot to layer BKCLAW_OBJECT_STORE_* on top of the DB-
// stored object-store namespace.
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
