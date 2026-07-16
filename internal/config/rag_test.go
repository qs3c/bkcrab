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

	cfg.RAG.Limits.MaxFileMB = 7
	ApplyDefaults(&cfg)
	if cfg.RAG.Limits.MaxFileMB != 7 {
		t.Fatalf("explicit maxFileMB overwritten: %d", cfg.RAG.Limits.MaxFileMB)
	}
}

func TestRAGCfgJSONAndAvailable(t *testing.T) {
	var cfg Config
	err := json.Unmarshal([]byte(`{"rag":{"milvus":{"address":"127.0.0.1:19530","username":"u","password":"p"},"embedding":{"endpoint":"http://embed/v1","apiKey":"secret","model":"embed-v3","dims":1024},"limits":{"maxFileMB":12}}}`), &cfg)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.RAG.Available() {
		t.Fatalf("complete RAG config reported unavailable: %+v", cfg.RAG)
	}
	if cfg.RAG.Milvus.Username != "u" || cfg.RAG.Embedding.APIKey != "secret" ||
		cfg.RAG.Limits.MaxFileMB != 12 {
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
