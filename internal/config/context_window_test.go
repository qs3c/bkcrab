package config

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/modelspec"
)

func TestResolveContextWindowUsesProviderPrefixedModelID(t *testing.T) {
	providers := map[string]ProviderConfig{
		"openai": {
			Models: []ModelEntry{
				{ID: "gpt-4.1", ContextWindow: 1048576, MaxTokens: 32768},
			},
		},
	}

	got := ResolveContextWindow(providers, "openai/gpt-4.1", 8192)
	if got != 1048576 {
		t.Fatalf("context window = %d, want 1048576", got)
	}
}

func TestResolveContextWindowUsesModelNameWhenIDDiffers(t *testing.T) {
	providers := map[string]ProviderConfig{
		"anthropic": {
			Models: []ModelEntry{
				{ID: "claude-sonnet-4", Name: "Claude Sonnet 4", ContextWindow: 200000},
			},
		},
	}

	got := ResolveContextWindow(providers, "Claude Sonnet 4", 8192)
	if got != 200000 {
		t.Fatalf("context window = %d, want 200000", got)
	}
}

func TestResolveContextWindowFallsBackToDefault(t *testing.T) {
	got := ResolveContextWindow(nil, "unknown/model", 8192)
	if got != DefaultContextWindow {
		t.Fatalf("context window = %d, want %d", got, DefaultContextWindow)
	}
}

func TestResolvedAgentRefreshModelContextWindow(t *testing.T) {
	rc := ResolvedAgent{
		Model:     "openrouter/qwen/qwen3-coder",
		MaxTokens: 12000,
		Providers: map[string]ProviderConfig{
			"openrouter": {
				Models: []ModelEntry{
					{ID: "qwen/qwen3-coder", ContextWindow: 262144},
				},
			},
		},
	}

	rc.RefreshModelContextWindow()
	if rc.ContextWindow != 262144 {
		t.Fatalf("context window = %d, want 262144", rc.ContextWindow)
	}
}

func TestResolveContextWindowUsesCatalogWhenEntryUnset(t *testing.T) {
	want, ok := modelspec.Lookup("glm-5.1", "opencode.ai")
	if !ok || want.ContextWindow <= 0 {
		t.Fatalf("snapshot missing glm-5.1: ok=%v want=%+v", ok, want)
	}

	providers := map[string]ProviderConfig{
		"opencode": {
			APIBase: "https://opencode.ai/zen/v1",
			Models:  []ModelEntry{{ID: "glm-5.1", Name: "GLM 5.1"}},
		},
	}
	got := ResolveContextWindow(providers, "opencode/glm-5.1", 8192)
	if got != want.ContextWindow {
		t.Fatalf("context window = %d, want %d (from catalog)", got, want.ContextWindow)
	}
	if got == DefaultContextWindow {
		t.Fatalf("catalog layer not consulted: got default %d", got)
	}
}

func TestResolveContextWindowExplicitEntryStillWins(t *testing.T) {
	providers := map[string]ProviderConfig{
		"opencode": {
			APIBase: "https://opencode.ai/zen/v1",
			Models:  []ModelEntry{{ID: "glm-5.1", ContextWindow: 12345}},
		},
	}
	if got := ResolveContextWindow(providers, "opencode/glm-5.1", 8192); got != 12345 {
		t.Fatalf("explicit entry should win: got %d, want 12345", got)
	}
}

func TestResolveContextWindowUnknownStillFallsBack(t *testing.T) {
	if got := ResolveContextWindow(nil, "totally/unknown-model", 8192); got != DefaultContextWindow {
		t.Fatalf("got %d, want default %d", got, DefaultContextWindow)
	}
}

func TestResolveMaxOutputTokensExplicitWins(t *testing.T) {
	providers := map[string]ProviderConfig{
		"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
	}
	if got := ResolveMaxOutputTokens(providers, "opencode/glm-5.1", 16000, 8192); got != 16000 {
		t.Fatalf("explicit should win: got %d, want 16000", got)
	}
}

func TestResolveMaxOutputTokensUsesCatalogWhenNotExplicit(t *testing.T) {
	want, ok := modelspec.Lookup("glm-5.1", "opencode.ai")
	if !ok || want.MaxOutputTokens <= 0 {
		t.Fatalf("snapshot missing glm-5.1 output: ok=%v want=%+v", ok, want)
	}
	providers := map[string]ProviderConfig{
		"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
	}
	got := ResolveMaxOutputTokens(providers, "opencode/glm-5.1", 0, 8192)
	if got != want.MaxOutputTokens {
		t.Fatalf("should use catalog: got %d, want %d", got, want.MaxOutputTokens)
	}
	if got == 8192 {
		t.Fatal("system default 8192 wrongly shadowed catalog")
	}
}

func TestResolveMaxOutputTokensFallsBackWhenUnknown(t *testing.T) {
	got := ResolveMaxOutputTokens(nil, "totally/unknown-model", 0, 0)
	if got != DefaultMaxOutputTokens {
		t.Fatalf("got %d, want %d", got, DefaultMaxOutputTokens)
	}
}

func TestMergedAgentConfigMaxTokensFromCatalog(t *testing.T) {
	withoutAgentFileConfig(t)

	want, ok := modelspec.Lookup("glm-5.1", "opencode.ai")
	if !ok || want.MaxOutputTokens <= 0 {
		t.Fatalf("snapshot missing glm-5.1 output")
	}
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
		},
	}
	cfg.Agents.Defaults.Model = "opencode/glm-5.1"
	ApplyDefaults(cfg)

	rc := cfg.MergedAgentConfig(AgentEntry{ID: "a1"})
	if rc.MaxTokens != want.MaxOutputTokens {
		t.Fatalf("MaxTokens = %d, want %d (catalog)", rc.MaxTokens, want.MaxOutputTokens)
	}
}

func TestMergedAgentConfigExplicitMaxTokensWins(t *testing.T) {
	withoutAgentFileConfig(t)

	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
		},
	}
	cfg.Agents.Defaults.Model = "opencode/glm-5.1"
	ApplyDefaults(cfg)

	rc := cfg.MergedAgentConfig(AgentEntry{ID: "a1", MaxTokens: 4096})
	if rc.MaxTokens != 4096 {
		t.Fatalf("explicit per-agent MaxTokens should win: got %d, want 4096", rc.MaxTokens)
	}
}

func withoutAgentFileConfig(t *testing.T) {
	t.Helper()
	old := AgentFileConfigLoader
	AgentFileConfigLoader = func(_, _ string) (AgentFileConfig, bool) {
		return AgentFileConfig{}, false
	}
	t.Cleanup(func() {
		AgentFileConfigLoader = old
	})
}
