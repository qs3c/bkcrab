package provider

import "testing"

func TestNormalizeAPIBase(t *testing.T) {
	tests := []struct {
		name    string
		apiBase string
		apiType string
		want    string
	}{
		// OpenAI: bare host gets /v1 appended.
		{"openai bare host", "https://api.openai.com", "openai-chat", "https://api.openai.com/v1"},
		{"openai bare host trailing slash", "https://api.openai.com/", "openai-chat", "https://api.openai.com/v1"},
		// OpenAI: already-canonical /v1 left alone.
		{"openai with v1", "https://api.openai.com/v1", "openai-chat", "https://api.openai.com/v1"},
		{"openai with v1 trailing slash", "https://api.openai.com/v1/", "openai-chat", "https://api.openai.com/v1"},
		// OpenAI: third-party gateway with custom path is left alone — we
		// can't safely guess where /v1 belongs in the gateway's routing.
		{"openai gateway custom path", "https://gw.example.com/openai", "openai-chat", "https://gw.example.com/openai"},
		{"openai gateway with v1 path", "https://gw.example.com/openai/v1", "openai-chat", "https://gw.example.com/openai/v1"},

		// Anthropic: bare host left alone — runtime appends /v1/messages.
		{"anthropic bare host", "https://api.anthropic.com", "anthropic-messages", "https://api.anthropic.com"},
		// Anthropic: trailing /v1 stripped to avoid /v1/v1/messages.
		{"anthropic with v1", "https://api.anthropic.com/v1", "anthropic-messages", "https://api.anthropic.com"},
		{"anthropic with v1 trailing slash", "https://api.anthropic.com/v1/", "anthropic-messages", "https://api.anthropic.com"},
		// Anthropic gateway with non-/v1 path is left intact — runtime
		// will append /v1/messages onto whatever path the gateway has.
		{"anthropic gateway custom path", "https://gw.example.com/anthropic", "anthropic-messages", "https://gw.example.com/anthropic"},

		// Whitespace and empty handling.
		{"empty string", "", "openai-chat", ""},
		{"whitespace only", "   ", "openai-chat", ""},
		{"surrounding whitespace", "  https://api.openai.com  ", "openai-chat", "https://api.openai.com/v1"},

		// Typo guard: /v12 is NOT a /v1 suffix, so we don't strip.
		{"openai v12 typo", "https://api.openai.com/v12", "openai-chat", "https://api.openai.com/v12"},
		{"anthropic v12 typo", "https://api.deepseek.com/v12", "anthropic-messages", "https://api.deepseek.com/v12"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeAPIBase(tc.apiBase, tc.apiType)
			if got != tc.want {
				t.Errorf("NormalizeAPIBase(%q, %q) = %q; want %q", tc.apiBase, tc.apiType, got, tc.want)
			}
		})
	}
}
