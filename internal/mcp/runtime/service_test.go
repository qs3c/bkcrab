package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
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
	var deployed map[string]config.MCPServerConfig
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deploy" {
			t.Fatalf("path = %s, want /deploy", r.URL.Path)
		}
		var body struct {
			MCPServers map[string]config.MCPServerConfig `json:"mcpServers"`
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

func ctxWithTestDeadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	return ctx
}
