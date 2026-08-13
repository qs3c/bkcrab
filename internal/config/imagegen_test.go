package config

import (
	"strings"
	"testing"
	"time"
)

func validImagegenBatchConfig() (ImagegenBatchCfg, FairQueueCfg) {
	image := DefaultImagegenBatchCfg()
	image.Mode = ImagegenBatchModeFair
	fair := DefaultFairQueueCfg()
	fair.Enabled = true
	fair.RedisAddr = "redis:6379"
	fair.RabbitMQURL = "amqp://rabbit:5672/"
	fair.MySQLWriterTopology = FairQueueMySQLWriterTopologySingle
	return image, fair
}

func TestImagegenBatchValidation(t *testing.T) {
	image, fair := validImagegenBatchConfig()
	for _, mode := range []ImagegenBatchMode{ImagegenBatchModeLegacy, ImagegenBatchModeDrain, ImagegenBatchModeFair} {
		cfg := image
		cfg.Mode = mode
		if err := cfg.Validate("mysql", fair); err != nil {
			t.Fatalf("valid mode %q rejected: %v", mode, err)
		}
	}

	tests := []struct {
		name    string
		storage string
		mutate  func(*ImagegenBatchCfg, *FairQueueCfg)
	}{
		{name: "unknown mode", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.Mode = "unknown" }},
		{name: "zero max batch", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.MaxImagesPerBatch = 0 }},
		{name: "batch above fixed maximum", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.MaxImagesPerBatch = 17 }},
		{name: "zero max task", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.MaxImagesPerTask = 0 }},
		{name: "task above fixed maximum", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.MaxImagesPerTask = 5 }},
		{name: "default wait above max", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.ToolWaitDefault = c.ToolWaitMax + time.Second }},
		{name: "max wait above protocol cap", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.ToolWaitMax = 241 * time.Second }},
		{name: "zero prompt limit", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.PromptMaxRunes = 0 }},
		{name: "batch bytes below image bytes", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.BatchMaxBytes = c.ImageMaxBytes - 1 }},
		{name: "base above burst", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.PerUserBaseConcurrency = 5 }},
		{name: "burst above global", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.GlobalConcurrency = 3 }},
		{name: "task heartbeat reaches lease", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.TaskHeartbeat = c.TaskLease }},
		{name: "reservation heartbeat reaches ttl", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.ReservationHeartbeat = c.ReservationTTL }},
		{name: "prepare reaches provisional", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.PrepareTimeout = c.ProvisionalTTL }},
		{name: "publish timeout reaches recovery drain", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.PublishAttemptTimeout = c.RecoveryDrainTimeout }},
		{name: "duration above deployment limit", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.TaskLease = FairQueueMaxDuration + time.Nanosecond }},
		{name: "zero artifact download timeout", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.ArtifactDownloadTimeout = 0 }},
		{name: "page above deployment limit", storage: "mysql", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.ReconcilePageSize = FairQueueMaxReconcilePageSize + 1 }},
		{name: "fair on sqlite", storage: "sqlite", mutate: func(*ImagegenBatchCfg, *FairQueueCfg) {}},
		{name: "drain on sqlite", storage: "sqlite", mutate: func(c *ImagegenBatchCfg, _ *FairQueueCfg) { c.Mode = ImagegenBatchModeDrain }},
		{name: "fair queue disabled", storage: "mysql", mutate: func(_ *ImagegenBatchCfg, f *FairQueueCfg) { f.Enabled = false }},
		{name: "writer topology not single", storage: "mysql", mutate: func(_ *ImagegenBatchCfg, f *FairQueueCfg) { f.MySQLWriterTopology = "multi" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, shared := validImagegenBatchConfig()
			tt.mutate(&cfg, &shared)
			if err := cfg.Validate(tt.storage, shared); err == nil {
				t.Fatalf("invalid config accepted: %+v", cfg)
			}
		})
	}
}

func TestImagegenBatchEnvironmentOverlay(t *testing.T) {
	t.Setenv("BKCRAB_IMAGEGEN_BATCH_MODE", "drain")
	t.Setenv("BKCRAB_IMAGEGEN_MAX_IMAGES_PER_BATCH", "12")
	t.Setenv("BKCRAB_IMAGEGEN_MAX_IMAGES_PER_TASK", "3")
	t.Setenv("BKCRAB_IMAGEGEN_TOOL_WAIT_DEFAULT", "90s")
	t.Setenv("BKCRAB_IMAGEGEN_TOOL_WAIT_MAX", "200s")
	t.Setenv("BKCRAB_IMAGEGEN_PROMPT_MAX_RUNES", "7000")
	t.Setenv("BKCRAB_IMAGEGEN_REQUEST_MAX_BYTES", "65536")
	t.Setenv("BKCRAB_IMAGEGEN_IMAGE_MAX_BYTES", "10485760")
	t.Setenv("BKCRAB_IMAGEGEN_BATCH_MAX_BYTES", "67108864")
	t.Setenv("BKCRAB_IMAGEGEN_LOCAL_WORKERS", "6")
	t.Setenv("BKCRAB_IMAGEGEN_GLOBAL_CONCURRENCY", "8")
	t.Setenv("BKCRAB_IMAGEGEN_PER_USER_BASE_CONCURRENCY", "3")
	t.Setenv("BKCRAB_IMAGEGEN_PER_USER_BURST_CONCURRENCY", "6")
	t.Setenv("BKCRAB_IMAGEGEN_BORROW_ENABLED", "false")
	t.Setenv("BKCRAB_IMAGEGEN_TASK_LEASE", "4m")
	t.Setenv("BKCRAB_IMAGEGEN_TASK_HEARTBEAT", "45s")
	t.Setenv("BKCRAB_IMAGEGEN_RESERVATION_TTL", "4m")
	t.Setenv("BKCRAB_IMAGEGEN_RESERVATION_HEARTBEAT", "45s")
	t.Setenv("BKCRAB_IMAGEGEN_PREPARE_TIMEOUT", "12s")
	t.Setenv("BKCRAB_IMAGEGEN_PROVISIONAL_TTL", "20s")
	t.Setenv("BKCRAB_IMAGEGEN_PROCESSING_TTL", "22s")
	t.Setenv("BKCRAB_IMAGEGEN_PUBLISH_ATTEMPT_TIMEOUT", "30s")
	t.Setenv("BKCRAB_IMAGEGEN_RECOVERY_DRAIN_TIMEOUT", "5m")
	t.Setenv("BKCRAB_IMAGEGEN_DISPATCH_INTERVAL", "2s")
	t.Setenv("BKCRAB_IMAGEGEN_RECONCILE_INTERVAL", "45s")
	t.Setenv("BKCRAB_IMAGEGEN_EXPIRED_SWEEP_INTERVAL", "20s")
	t.Setenv("BKCRAB_IMAGEGEN_RECONCILE_PAGE_SIZE", "350")
	t.Setenv("BKCRAB_IMAGEGEN_MAX_RETRIES", "5")
	t.Setenv("BKCRAB_IMAGEGEN_PROVIDER_CALL_TIMEOUT", "100s")
	t.Setenv("BKCRAB_IMAGEGEN_ARTIFACT_DOWNLOAD_TIMEOUT", "50s")
	t.Setenv("BKCRAB_IMAGEGEN_PROVIDER_CONCURRENCY_DEFAULT", "7")

	cfg := LoadEnv().ImagegenBatch
	if cfg.Mode != ImagegenBatchModeDrain || cfg.MaxImagesPerBatch != 12 || cfg.MaxImagesPerTask != 3 ||
		cfg.ToolWaitDefault != 90*time.Second || cfg.ToolWaitMax != 200*time.Second ||
		cfg.PromptMaxRunes != 7000 || cfg.RequestMaxBytes != 65536 ||
		cfg.ImageMaxBytes != 10485760 || cfg.BatchMaxBytes != 67108864 ||
		cfg.LocalWorkers != 6 || cfg.GlobalConcurrency != 8 ||
		cfg.PerUserBaseConcurrency != 3 || cfg.PerUserBurstConcurrency != 6 || cfg.BorrowEnabled ||
		cfg.TaskLease != 4*time.Minute || cfg.TaskHeartbeat != 45*time.Second ||
		cfg.ReservationTTL != 4*time.Minute || cfg.ReservationHeartbeat != 45*time.Second ||
		cfg.PrepareTimeout != 12*time.Second || cfg.ProvisionalTTL != 20*time.Second ||
		cfg.ProcessingTurnTTL != 22*time.Second || cfg.PublishAttemptTimeout != 30*time.Second ||
		cfg.RecoveryDrainTimeout != 5*time.Minute || cfg.DispatchInterval != 2*time.Second ||
		cfg.ReconcileInterval != 45*time.Second || cfg.ExpiredSweepInterval != 20*time.Second ||
		cfg.ReconcilePageSize != 350 || cfg.MaxRetries != 5 ||
		cfg.ProviderCallTimeout != 100*time.Second || cfg.ArtifactDownloadTimeout != 50*time.Second || cfg.ProviderConcurrencyDefault != 7 {
		t.Fatalf("imagegen environment overlay = %+v", cfg)
	}
}

func TestImagegenBatchInvalidEnvironmentIsReported(t *testing.T) {
	t.Setenv("BKCRAB_IMAGEGEN_LOCAL_WORKERS", "many")
	err := LoadEnv().ImagegenBatch.Validate("mysql", DefaultFairQueueCfg())
	if err == nil || !strings.Contains(err.Error(), "BKCRAB_IMAGEGEN_LOCAL_WORKERS") {
		t.Fatalf("invalid environment error = %v", err)
	}
}
