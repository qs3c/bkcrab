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
