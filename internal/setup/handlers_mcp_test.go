package setup

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestMaskMCPServersMasksEnvAndHeaders(t *testing.T) {
	in := map[string]config.MCPServerConfig{
		"github": {
			Type: "http",
			URL:  "https://example.com/mcp",
			Headers: map[string]string{
				"Authorization": "Bearer secret",
			},
			Env: map[string]string{"TOKEN": "secret"},
		},
	}
	got := maskMCPServers(in)
	if got["github"].Headers["Authorization"] != mcpSecretMask {
		t.Fatalf("header not masked: %#v", got["github"].Headers)
	}
	if got["github"].Env["TOKEN"] != mcpSecretMask {
		t.Fatalf("env not masked: %#v", got["github"].Env)
	}
	if in["github"].Env["TOKEN"] != "secret" {
		t.Fatal("masking mutated the input map")
	}
}

func TestMergeMaskedMCPSecretsPreservesOldValues(t *testing.T) {
	old := map[string]config.MCPServerConfig{
		"github": {
			Type:    "http",
			Headers: map[string]string{"Authorization": "Bearer old"},
			Env:     map[string]string{"TOKEN": "old"},
		},
	}
	next := map[string]config.MCPServerConfig{
		"github": {
			Type:    "http",
			Headers: map[string]string{"Authorization": mcpSecretMask},
			Env:     map[string]string{"TOKEN": mcpSecretMask},
		},
	}
	got := mergeMaskedMCPSecrets(old, next)
	if got["github"].Headers["Authorization"] != "Bearer old" {
		t.Fatalf("header = %q", got["github"].Headers["Authorization"])
	}
	if got["github"].Env["TOKEN"] != "old" {
		t.Fatalf("env = %q", got["github"].Env["TOKEN"])
	}
}

func TestValidateMCPServersRejectsUnsupportedHeader(t *testing.T) {
	err := validateMCPServers(map[string]config.MCPServerConfig{
		"remote": {Type: "http", URL: "https://example.com/mcp", Headers: map[string]string{"X-API-Key": "secret"}},
	})
	if err == nil {
		t.Fatal("expected validation error")
	}
}
