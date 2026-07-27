package gateway

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestApplyForeignMCPSharing(t *testing.T) {
	rc := &config.ResolvedAgent{
		UserID:     "owner",
		MCPServers: map[string]config.MCPServerConfig{"time": {Type: "stdio"}},
	}
	rec := &store.AgentRecord{UserID: "owner", Config: map[string]interface{}{}}
	applyForeignMCPSharing(rc, rec, "viewer")
	if len(rc.MCPServers) != 0 {
		t.Fatalf("foreign viewer should not inherit MCP by default: %#v", rc.MCPServers)
	}

	rc = &config.ResolvedAgent{
		UserID:     "owner",
		MCPServers: map[string]config.MCPServerConfig{"time": {Type: "stdio"}},
	}
	rec = &store.AgentRecord{UserID: "owner", Config: map[string]interface{}{"shareMcpConfig": true}}
	applyForeignMCPSharing(rc, rec, "viewer")
	if len(rc.MCPServers) != 1 || !rc.ShareMCPConfig {
		t.Fatalf("shared MCP config was not preserved: %#v", rc)
	}
}

func TestApplyForeignMCPSharingKeepsExplicitResourceGrants(t *testing.T) {
	rc := &config.ResolvedAgent{
		UserID: "owner",
		MCP:    config.MCPAgentCfg{Servers: []string{"sc_github"}},
	}
	rec := &store.AgentRecord{UserID: "owner", Config: map[string]interface{}{}}
	applyForeignMCPSharing(rc, rec, "viewer")
	if len(rc.MCP.Servers) != 1 || rc.MCP.Servers[0] != "sc_github" {
		t.Fatalf("public-agent resource grants should follow the agent: %#v", rc.MCP.Servers)
	}
}
