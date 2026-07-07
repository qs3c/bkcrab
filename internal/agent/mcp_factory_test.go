package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/mcp"
)

func TestManagerUsesInjectedMCPFactory(t *testing.T) {
	called := false
	factory := MCPManagerFactoryFunc(func(ctx context.Context, rc config.ResolvedAgent) (*mcp.Manager, error) {
		called = true
		return mcp.NewAggregatedManager(&testMCPClient{}), nil
	})
	_, err := NewManager([]config.ResolvedAgent{{
		ID: "agt_1", UserID: "u1", Model: "p/m", Home: t.TempDir(),
		MCPServers: map[string]config.MCPServerConfig{"time": {Type: "stdio", Command: "uvx"}},
	}}, nil, bus.New(), WithUserID("u1"), WithMCPManagerFactory(factory))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if !called {
		t.Fatal("MCP factory was not called")
	}
}

type testMCPClient struct{}

func (testMCPClient) Connect() error { return nil }
func (testMCPClient) ListTools() ([]mcp.ToolDef, error) {
	return []mcp.ToolDef{{Name: "time_now", Description: "time"}}, nil
}
func (testMCPClient) CallTool(name string, args json.RawMessage) (string, error) {
	return "ok", nil
}
func (testMCPClient) Close() error { return nil }
