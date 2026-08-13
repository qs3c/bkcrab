package config

import (
	"reflect"
	"testing"
	"time"
)

func TestApplyDefaultsUsesLargerToolIterationBudget(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.Agents.Defaults.MaxToolIterations != 200 {
		t.Fatalf("MaxToolIterations = %d, want 200", cfg.Agents.Defaults.MaxToolIterations)
	}
}

func TestFairQueueDefaults(t *testing.T) {
	cfg := DefaultFairQueueCfg()
	if cfg.Enabled {
		t.Fatal("fair queue must default to disabled")
	}
	if cfg.RedisMode != FairQueueRedisModeStandalone || cfg.RedisAddr != "" ||
		cfg.RedisPassword != "" || cfg.RedisDB != 0 {
		t.Fatalf("Redis defaults = %+v", cfg)
	}
	if cfg.RabbitMQURL != "" || cfg.Exchange != "bkcrab.fair.task" ||
		cfg.DeadLetterExchange != "bkcrab.fair.dlx" || cfg.KeyPrefix != "bkcrab:" ||
		cfg.MySQLWriterTopology != "" {
		t.Fatalf("shared fair queue defaults = %+v", cfg)
	}

	rag := cfg.RAGIndex
	if rag.WorkerMode != FairQueueWorkerModeLegacy || rag.LocalWorkers != 4 ||
		rag.GlobalConcurrency != 4 || rag.PerUserBaseConcurrency != 2 ||
		rag.PerUserBurstConcurrency != 4 || !rag.BorrowEnabled {
		t.Fatalf("RAG fair queue concurrency defaults = %+v", rag)
	}
	if rag.ReconcileInterval != 30*time.Second ||
		rag.ExpiredRunningSweepInterval != 15*time.Second || rag.ReconcilePageSize != 200 ||
		rag.ReservationTTL != 60*time.Second || rag.ReservationHeartbeat != 20*time.Second ||
		rag.PrepareTimeout != 10*time.Second || rag.ProvisionalTTL != 15*time.Second ||
		rag.ProcessingTurnTTL != 15*time.Second || rag.RecoveryDrainTimeout != 2*time.Minute ||
		rag.DispatchInterval != time.Second || rag.PublishAttemptTimeout != 15*time.Second {
		t.Fatalf("RAG fair queue timing defaults = %+v", rag)
	}
}

func TestFairQueueDeploymentScope(t *testing.T) {
	if _, ok := reflect.TypeOf(EnvConfig{}).FieldByName("FairQueue"); !ok {
		t.Fatal("deployment EnvConfig is missing FairQueue")
	}
	if _, ok := reflect.TypeOf(Config{}).FieldByName("FairQueue"); ok {
		t.Fatal("FairQueue must not enter database-backed user/agent Config")
	}
}

func TestImagegenBatchDefaults(t *testing.T) {
	cfg := DefaultImagegenBatchCfg()
	if cfg.Mode != ImagegenBatchModeLegacy || cfg.MaxImagesPerBatch != 16 ||
		cfg.MaxImagesPerTask != 4 || cfg.ToolWaitDefault != 180*time.Second ||
		cfg.ToolWaitMax != 240*time.Second || cfg.PromptMaxRunes != 8000 ||
		cfg.RequestMaxBytes != 128*1024 || cfg.ImageMaxBytes != 20*1024*1024 ||
		cfg.BatchMaxBytes != 128*1024*1024 {
		t.Fatalf("imagegen request defaults = %+v", cfg)
	}
	if cfg.LocalWorkers != 4 || cfg.GlobalConcurrency != 4 ||
		cfg.PerUserBaseConcurrency != 2 || cfg.PerUserBurstConcurrency != 4 ||
		!cfg.BorrowEnabled || cfg.MaxRetries != 3 ||
		cfg.ProviderConcurrencyDefault != 4 {
		t.Fatalf("imagegen concurrency defaults = %+v", cfg)
	}
	if cfg.TaskLease != 180*time.Second || cfg.TaskHeartbeat != 30*time.Second ||
		cfg.ReservationTTL != 180*time.Second || cfg.ReservationHeartbeat != 30*time.Second ||
		cfg.PrepareTimeout != 10*time.Second || cfg.ProvisionalTTL != 15*time.Second ||
		cfg.ProcessingTurnTTL != 15*time.Second || cfg.PublishAttemptTimeout != 15*time.Second ||
		cfg.RecoveryDrainTimeout != 2*time.Minute || cfg.DispatchInterval != time.Second ||
		cfg.ReconcileInterval != 30*time.Second || cfg.ExpiredSweepInterval != 15*time.Second ||
		cfg.ReconcilePageSize != 200 || cfg.ProviderCallTimeout != 120*time.Second ||
		cfg.ArtifactDownloadTimeout != 60*time.Second {
		t.Fatalf("imagegen timing defaults = %+v", cfg)
	}
	if _, ok := reflect.TypeOf(EnvConfig{}).FieldByName("ImagegenBatch"); !ok {
		t.Fatal("deployment EnvConfig is missing ImagegenBatch")
	}
	if _, ok := reflect.TypeOf(Config{}).FieldByName("ImagegenBatch"); ok {
		t.Fatal("ImagegenBatch must not enter database-backed user/agent Config")
	}
}
