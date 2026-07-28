package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/mcp"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeDocker struct {
	started []ContainerSpec
	stopped []string
	ref     ContainerRef
}

func (f *fakeDocker) Ensure(ctx context.Context, spec ContainerSpec) (ContainerRef, error) {
	f.started = append(f.started, spec)
	if f.ref.BaseURL == "" {
		f.ref = ContainerRef{ID: "ctr-1", Name: spec.Name, BaseURL: "http://127.0.0.1:39001", ExternalPort: 39001, Running: true}
	}
	return f.ref, nil
}

func (f *fakeDocker) Stop(ctx context.Context, name string) error {
	f.stopped = append(f.stopped, name)
	return nil
}

type fakeRuntimeStore struct {
	rec *store.MCPGatewayRuntimeRecord
}

type fakeResourceStore struct {
	rows []store.ConfigRecord
}

func TestNewServiceDefaultsGatewayRequestTimeoutToFiveMinutes(t *testing.T) {
	svc := NewService(Options{})
	if got, want := svc.httpClient.Timeout, 5*time.Minute; got != want {
		t.Fatalf("gateway request timeout = %s, want %s", got, want)
	}
}

func (f *fakeResourceStore) ListConfigs(ctx context.Context, kind, userID, agentID string) ([]store.ConfigRecord, error) {
	var rows []store.ConfigRecord
	for _, row := range f.rows {
		if row.Kind == kind && row.UserID == userID && row.AgentID == agentID {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (f *fakeRuntimeStore) GetMCPGatewayRuntime(ctx context.Context, userID string) (*store.MCPGatewayRuntimeRecord, error) {
	if f.rec == nil || f.rec.UserID != userID {
		return nil, store.ErrNotFound
	}
	cp := *f.rec
	return &cp, nil
}

func (f *fakeRuntimeStore) SaveMCPGatewayRuntime(ctx context.Context, rec *store.MCPGatewayRuntimeRecord) error {
	cp := *rec
	f.rec = &cp
	return nil
}

func (f *fakeRuntimeStore) ListMCPGatewayRuntimesByStatus(ctx context.Context, statuses ...string) ([]store.MCPGatewayRuntimeRecord, error) {
	if f.rec == nil {
		return nil, nil
	}
	for _, status := range statuses {
		if f.rec.Status == status {
			return []store.MCPGatewayRuntimeRecord{*f.rec}, nil
		}
	}
	return nil, nil
}

func TestServiceEnsureDeploysToPerUserGateway(t *testing.T) {
	var deployed map[string]LuckyServerConfig
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deploy" {
			t.Fatalf("path = %s, want /deploy", r.URL.Path)
		}
		var body struct {
			MCPServers map[string]LuckyServerConfig `json:"mcpServers"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		deployed = body.MCPServers
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer api.Close()

	fd := &fakeDocker{ref: ContainerRef{ID: "ctr-1", Name: "bkcrab-mcp-u1", BaseURL: api.URL, ExternalPort: 39001, Running: true}}
	fs := &fakeRuntimeStore{}
	svc := NewService(Options{
		Store:  fs,
		Docker: fd,
		Config: Config{Enabled: true, Image: defaultImage, RuntimeDir: t.TempDir(), ContainerPort: 8080, Protocol: "all", IdleTTL: time.Minute},
	})
	servers := map[string]config.MCPServerConfig{
		"time": {Type: "stdio", Command: "uvx", Args: []string{"mcp-server-time"}, Env: map[string]string{"TZ": "Asia/Shanghai"}},
	}
	if _, err := svc.Deploy(ctxWithTestDeadline(t), "u1", servers); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(fd.started) != 1 {
		t.Fatalf("docker starts = %d, want 1", len(fd.started))
	}
	if deployed["time"].Command != "uvx" {
		t.Fatalf("deployed payload = %#v", deployed)
	}
	if deployed["time"].GatewayProtocol != "sse" {
		t.Fatalf("stdio gateway protocol = %q, want sse", deployed["time"].GatewayProtocol)
	}
	if fs.rec == nil || fs.rec.Status != StatusRunning {
		t.Fatalf("runtime record = %#v", fs.rec)
	}
}

func TestToLuckyConfigMapsBearerHeader(t *testing.T) {
	got, err := ToLuckyServerConfig(config.MCPServerConfig{
		Type:    "http",
		URL:     "https://example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer token-1"},
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.Env["MCP_REMOTE_AUTH_ACCESS_TOKEN"] != "token-1" {
		t.Fatalf("env = %#v", got.Env)
	}
}

func TestToLuckyConfigUsesSSEBridgeForStdio(t *testing.T) {
	got, err := ToLuckyServerConfig(config.MCPServerConfig{
		Type:      "stdio",
		Command:   "npx",
		Args:      []string{"-y", "@modelcontextprotocol/server-everything"},
		Transport: config.MCPTransportStreamableHTTP,
	})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got.GatewayProtocol != "sse" {
		t.Fatalf("gateway protocol = %q, want sse", got.GatewayProtocol)
	}
}

func TestToLuckyConfigRejectsUnsupportedHeader(t *testing.T) {
	_, err := ToLuckyServerConfig(config.MCPServerConfig{
		Type:    "http",
		URL:     "https://example.com/mcp",
		Headers: map[string]string{"X-API-Key": "secret"},
	})
	if err == nil {
		t.Fatal("expected unsupported custom header error")
	}
}

func TestServiceStopsIdleRuntimeWhenRefsAreZero(t *testing.T) {
	fd := &fakeDocker{}
	fs := &fakeRuntimeStore{rec: &store.MCPGatewayRuntimeRecord{
		UserID:         "u1",
		Status:         StatusRunning,
		ContainerName:  "bkcrab-mcp-u1",
		LastAccessedAt: time.Now().UTC().Add(-2 * time.Hour),
	}}
	svc := NewService(Options{
		Store:  fs,
		Docker: fd,
		Config: Config{Enabled: true, IdleTTL: time.Minute},
	})
	if err := svc.StopIdle(ctxWithTestDeadline(t), time.Now().UTC()); err != nil {
		t.Fatalf("stop idle: %v", err)
	}
	if len(fd.stopped) != 1 || fd.stopped[0] != "bkcrab-mcp-u1" {
		t.Fatalf("stopped = %#v", fd.stopped)
	}
	if fs.rec.Status != StatusStopped {
		t.Fatalf("status = %q, want stopped", fs.rec.Status)
	}
}

func TestNewManagerFromServersSkipsDeployWhenNoEnabledServers(t *testing.T) {
	fd := &fakeDocker{}
	fs := &fakeRuntimeStore{}
	svc := NewService(Options{
		Store:  fs,
		Docker: fd,
		Config: Config{Enabled: true, IdleTTL: time.Minute},
	})
	disabled := false
	mgr, err := svc.NewManagerFromServers(ctxWithTestDeadline(t), "u1", map[string]config.MCPServerConfig{
		"time": {Type: "stdio", Command: "uvx", Enabled: &disabled},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer mgr.Close()
	if len(fd.started) != 0 {
		t.Fatalf("all-disabled config must not start a gateway container: %#v", fd.started)
	}
	if len(mgr.ToolDefs()) != 0 {
		t.Fatalf("expected no tools, got %#v", mgr.ToolDefs())
	}
	if fs.rec != nil {
		t.Fatalf("no runtime record should be written: %#v", fs.rec)
	}
}

func TestNewManagerFromResourcesDeploysUserSetButFiltersAgentTools(t *testing.T) {
	var deployed map[string]LuckyServerConfig
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/deploy" {
			var body struct {
				MCPServers map[string]LuckyServerConfig `json:"mcpServers"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode deploy: %v", err)
			}
			deployed = body.MCPServers
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		if r.URL.Path != "/stream" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+gatewayAPIKey; got != want {
			t.Fatalf("Authorization = %q, want %q", got, want)
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode MCP request: %v", err)
		}
		switch request.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result":  map[string]any{"protocolVersion": "2025-03-26"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      request.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "sc_alpha_read", "inputSchema": map[string]any{"type": "object"}},
					{"name": "sc_beta_write", "inputSchema": map[string]any{"type": "object"}},
				}},
			})
		default:
			t.Fatalf("unexpected MCP method %s", request.Method)
		}
	}))
	defer api.Close()

	resourceRows := make([]store.ConfigRecord, 0, 2)
	for _, resource := range []mcp.Resource{
		{ID: "sc_alpha", UserID: "u1", Name: "github", Enabled: true, Config: config.MCPServerConfig{Type: "stdio", Command: "alpha"}},
		{ID: "sc_beta", UserID: "u1", Name: "slack", Enabled: true, Config: config.MCPServerConfig{Type: "stdio", Command: "beta"}},
	} {
		row := store.ConfigRecord{ID: resource.ID}
		resource.ApplyToRecord(&row)
		resourceRows = append(resourceRows, row)
	}
	runtimeStore := &fakeRuntimeStore{}
	resourceStore := &fakeResourceStore{rows: resourceRows}
	svc := NewService(Options{
		Store:     runtimeStore,
		Resources: resourceStore,
		Docker: &fakeDocker{ref: ContainerRef{
			ID: "ctr-1", Name: "bkcrab-mcp-u1", BaseURL: api.URL, ExternalPort: 39001, Running: true,
		}},
		Config: Config{Enabled: true, Image: defaultImage, RuntimeDir: t.TempDir(), ContainerPort: 8080, Protocol: "all"},
	})
	manager, err := svc.NewManagerFromResources(ctxWithTestDeadline(t), "u1", []string{"sc_alpha"})
	if err != nil {
		t.Fatalf("new manager from resources: %v", err)
	}
	defer manager.Close()

	if len(deployed) != 2 || deployed["sc_alpha"].Command != "alpha" || deployed["sc_beta"].Command != "beta" {
		t.Fatalf("gateway should receive the full user resource set, got %#v", deployed)
	}
	tools := manager.ToolDefs()
	if len(tools) != 1 || tools[0].Name != "mcp_github_read" {
		t.Fatalf("agent should receive only granted tools, got %#v", tools)
	}

	disabled := false
	alpha, err := mcp.ResourceFromRecord(resourceStore.rows[0])
	if err != nil {
		t.Fatalf("decode alpha resource: %v", err)
	}
	alpha.Enabled = false
	alpha.Config.Enabled = &disabled
	alpha.ApplyToRecord(&resourceStore.rows[0])
	if err := svc.SyncUserResources(ctxWithTestDeadline(t), "u1"); err != nil {
		t.Fatalf("sync resources: %v", err)
	}
	if len(deployed) != 1 || deployed["sc_beta"].Command != "beta" {
		t.Fatalf("sync should remove disabled resources from running gateway, got %#v", deployed)
	}
}

func ctxWithTestDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
