package config

import "testing"

func TestApplyDefaultsUsesLargerToolIterationBudget(t *testing.T) {
	var cfg Config
	ApplyDefaults(&cfg)
	if cfg.Agents.Defaults.MaxToolIterations != 200 {
		t.Fatalf("MaxToolIterations = %d, want 200", cfg.Agents.Defaults.MaxToolIterations)
	}
}
