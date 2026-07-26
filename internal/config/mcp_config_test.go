package config

import "testing"

func TestMCPServerEnabledDefaultsTrue(t *testing.T) {
	if !MCPServerEnabled(MCPServerConfig{}) {
		t.Fatal("missing enabled flag should mean enabled")
	}
	disabled := false
	if MCPServerEnabled(MCPServerConfig{Enabled: &disabled}) {
		t.Fatal("explicit enabled=false should disable the server")
	}
	enabled := true
	if !MCPServerEnabled(MCPServerConfig{Enabled: &enabled}) {
		t.Fatal("explicit enabled=true should enable the server")
	}
}

func TestMCPTransportDefaultAndValidation(t *testing.T) {
	if got := NormalizeMCPTransport(""); got != MCPTransportStreamableHTTP {
		t.Fatalf("empty transport = %q, want %q", got, MCPTransportStreamableHTTP)
	}
	if got := NormalizeMCPTransport("streamhttp"); got != MCPTransportStreamableHTTP {
		t.Fatalf("streamhttp alias = %q, want %q", got, MCPTransportStreamableHTTP)
	}
	if got := NormalizeMCPTransport("sse"); got != MCPTransportSSE {
		t.Fatalf("sse transport = %q, want %q", got, MCPTransportSSE)
	}
	if MCPTransportValid("websocket") {
		t.Fatal("websocket should not be accepted in V1")
	}
}

func TestMergedAgentConfigCarriesMCPConfig(t *testing.T) {
	cfg := &Config{}
	prev := AgentFileConfigLoader
	defer func() { AgentFileConfigLoader = prev }()
	AgentFileConfigLoader = func(agentID, path string) (AgentFileConfig, bool) {
		return AgentFileConfig{
			ShareMCPConfig: true,
			MCPServers: map[string]MCPServerConfig{
				"time": {
					Type:      "stdio",
					Command:   "uvx",
					Args:      []string{"mcp-server-time"},
					Transport: MCPTransportSSE,
				},
			},
		}, true
	}

	rc := cfg.MergedAgentConfig(AgentEntry{ID: "agt_1", UserID: "u_owner", Name: "Time"})
	if !rc.ShareMCPConfig {
		t.Fatal("expected shareMcpConfig to resolve onto ResolvedAgent")
	}
	if got := rc.MCPServers["time"].Transport; got != MCPTransportSSE {
		t.Fatalf("transport = %q, want %q", got, MCPTransportSSE)
	}
}

func TestMergedAgentConfigCopiesMCPResourceGrant(t *testing.T) {
	cfg := &Config{}
	prev := AgentFileConfigLoader
	defer func() { AgentFileConfigLoader = prev }()
	stored := []string{"sc_one", "sc_two"}
	AgentFileConfigLoader = func(agentID, path string) (AgentFileConfig, bool) {
		return AgentFileConfig{
			MCP: &MCPAgentCfg{Servers: stored},
		}, true
	}

	rc := cfg.MergedAgentConfig(AgentEntry{ID: "agt_1", UserID: "u_owner"})
	if len(rc.MCP.Servers) != 2 || rc.MCP.Servers[0] != "sc_one" {
		t.Fatalf("resolved MCP grants = %#v", rc.MCP.Servers)
	}
	rc.MCP.Servers[0] = "mutated"
	if stored[0] != "sc_one" {
		t.Fatal("resolved MCP grants must not alias stored config")
	}
}
