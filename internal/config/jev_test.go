package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJevEnvAndSecrets(t *testing.T) {
	t.Setenv("BKCRAB_JEV_ENABLED", "true")
	t.Setenv("BKCRAB_JEV_API_KEY", "test-only-secret")
	t.Setenv("BKCRAB_JEV_MEMORY_GUARD_MODE", "shadow")
	t.Setenv("BKCRAB_JEV_RETRIES", "0")
	t.Setenv("BKCRAB_RAG_RERANKER_PROTOCOL", "jev")
	t.Setenv("BKCRAB_RAG_RERANKER_JEV_BATCH_SIZE", "5")
	t.Setenv("BKCRAB_RAG_RERANKER_DISABLE_SCORE_FILTER", "true")
	env := LoadEnv()
	client, e := env.Jev.NewClient()
	if e != nil || client == nil || env.Jev.Retries != 0 {
		t.Fatal(e)
	}
	var cfg RAGCfg
	env.ApplySystemRAG(&cfg)
	cfg.ApplyDefaults()
	if cfg.Reranker.Protocol != RAGRerankerProtocolJev || cfg.Reranker.JevBatchSize != 5 || cfg.Reranker.DisableScoreFilter == nil || !*cfg.Reranker.DisableScoreFilter {
		t.Fatal("env not propagated")
	}
	b, _ := json.Marshal(env.Jev)
	if strings.Contains(string(b), "test-only-secret") {
		t.Fatal("key serialized")
	}
	ScrubBootSecrets()
	if LoadEnv().Jev.APIKey != "" {
		t.Fatal("key not scrubbed")
	}
	if env.Jev.APIKey != "test-only-secret" {
		t.Fatal("boot snapshot lost key")
	}
}
func TestJevInvalidConfiguration(t *testing.T) {
	for _, kv := range [][2]string{{"RETRIES", "bad"}, {"MODEL", "jev-latest"}, {"MAX_REQUEST_COST_USD", "NaN"}, {"CONCURRENCY", "99"}, {"MEMORY_GUARD_MODE", "allow"}} {
		t.Run(kv[0], func(t *testing.T) {
			t.Setenv("BKCRAB_JEV_ENABLED", "true")
			t.Setenv("BKCRAB_JEV_API_KEY", "test")
			t.Setenv("BKCRAB_JEV_"+kv[0], kv[1])
			if _, e := LoadEnv().Jev.NewClient(); e == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
