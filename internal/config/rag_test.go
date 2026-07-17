package config

import (
	"encoding/json"
	"testing"
)

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
