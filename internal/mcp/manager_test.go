package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

type fakeClient struct {
	tools  []ToolDef
	called []string
	closed bool
}

func (f *fakeClient) Connect() error { return nil }
func (f *fakeClient) ListTools() ([]ToolDef, error) {
	return append([]ToolDef(nil), f.tools...), nil
}
func (f *fakeClient) CallTool(name string, args json.RawMessage) (string, error) {
	f.called = append(f.called, name)
	return "ok:" + name, nil
}
func (f *fakeClient) Close() error {
	f.closed = true
	return nil
}

func TestNewManagerSkipsDisabledServers(t *testing.T) {
	enabled := true
	disabled := false
	created := map[string]bool{}
	mgr := NewManagerWithFactory(map[string]config.MCPServerConfig{
		"on":  {Type: "stdio", Enabled: &enabled},
		"off": {Type: "stdio", Enabled: &disabled},
	}, func(name string, cfg config.MCPServerConfig) (Client, error) {
		created[name] = true
		return &fakeClient{tools: []ToolDef{{Name: "ping"}}}, nil
	})
	if !created["on"] {
		t.Fatal("enabled server was not created")
	}
	if created["off"] {
		t.Fatal("disabled server should not be created")
	}
	if len(mgr.ToolDefs()) != 1 {
		t.Fatalf("tool count = %d, want 1", len(mgr.ToolDefs()))
	}
}

func TestAggregatedManagerPrefixesGatewayToolsOnce(t *testing.T) {
	client := &fakeClient{tools: []ToolDef{{Name: "time_get_current_time"}}}
	mgr := NewAggregatedManager(client)
	defs := mgr.ToolDefs()
	if len(defs) != 1 {
		t.Fatalf("tool count = %d, want 1", len(defs))
	}
	if defs[0].Name != "mcp_time_get_current_time" {
		t.Fatalf("tool name = %q, want mcp_time_get_current_time", defs[0].Name)
	}
	out, err := mgr.CallTool(nil, "mcp_time_get_current_time", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if out != "ok:time_get_current_time" {
		t.Fatalf("call output = %q", out)
	}
}

func TestManagerCloseRunsHooks(t *testing.T) {
	client := &fakeClient{tools: []ToolDef{{Name: "ping"}}}
	mgr := NewAggregatedManager(client)
	closedHook := false
	mgr.AddCloseHook(func() { closedHook = true })
	mgr.Close()
	if !client.closed {
		t.Fatal("client was not closed")
	}
	if !closedHook {
		t.Fatal("close hook did not run")
	}
}

func TestPrefixSanitizesGatewayToolNames(t *testing.T) {
	got := prefixAggregatedToolName("github/search-repos")
	if got != "mcp_github_search_repos" {
		t.Fatalf("sanitized name = %q", got)
	}
	if strings.Contains(got, "/") || strings.Contains(got, "-") {
		t.Fatalf("sanitized name still contains unsafe characters: %q", got)
	}
}
