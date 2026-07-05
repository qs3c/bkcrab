# Gateway-Backed MCP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make BkCrab's agent-level MCP configuration usable end to end by routing both stdio and HTTP MCP servers through one per-user lucky-aeon MCP Gateway container, with owner-controlled public-agent sharing.

**Architecture:** Agent config remains stored on `agents.config.mcpServers`, but agent runtime no longer starts stdio MCP children directly when the gateway runtime is enabled. A new `internal/mcp/runtime` service owns per-user gateway containers, deploys an agent's enabled MCP server set to that user's gateway, and returns one aggregated MCP manager to the agent. Setup APIs and the frontend edit masked agent MCP config, test deployment, show tools, and expose `shareMcpConfig`.

**Tech Stack:** Go `net/http`, `database/sql`, existing BkCrab store/config/gateway/agent packages, Docker CLI, `ghcr.io/lucky-aeon/mcp-gateway:latest`, Next.js 16, React 19, lucide-react.

---

## Reference Inputs

- Design spec: `docs/superpowers/specs/2026-07-05-gateway-backed-mcp-design.md`
- lucky-aeon gateway runtime facts verified from its README and source:
  - Docker image: `ghcr.io/lucky-aeon/mcp-gateway:latest`
  - Local config file: `config.json`, with `WorkspacePath`, `Bind`, `Auth`, `GatewayProtocol`, and `McpServiceMgrConfig`
  - Deploy endpoint: `POST /deploy` with `{ "mcpServers": { "time": { "command": "uvx", "args": ["mcp-server-time"] } } }`
  - Aggregated Streamable HTTP endpoint: `/stream`
  - Aggregated SSE gateway endpoints: `GET /sse`, `POST /message`
  - Upstream bearer auth for remote MCP is represented as env `MCP_REMOTE_AUTH_ACCESS_TOKEN`

## Boundary Decisions

- V1 stores agent MCP secrets in `agents.config` as plaintext, but every read API and UI render masks `env` and `headers`.
- V1 supports arbitrary stdio `env`.
- V1 supports HTTP MCP servers with no auth or `Authorization: Bearer <token>`. Other custom upstream HTTP headers are rejected at save time with a concrete validation error because lucky-aeon does not expose generic downstream header config.
- The gateway MCP protocol exposed by the per-user container is `all`; BkCrab chooses `/stream` by default for agent runtime and keeps `/message` support for SSE-mode testing.
- Runtime container lifecycle is owned by BkCrab: start on first deployment, reference-count while user spaces hold agents using that gateway, stop after idle TTL when refs are zero, stop all managed containers during process shutdown.
- Public agent MCP sharing is independent from model/provider sharing:
  - Owner using their own agent always gets that agent's MCP config.
  - Foreign user gets owner's MCP config only when `shareMcpConfig: true`.
  - If shared, MCP runs in the owner's per-user gateway, not the visitor's gateway.

## File Structure

- Modify: `internal/config/config.go`
  - Add MCP transport constants, enabled/default helper, config fields, and `shareMcpConfig` propagation.
- Create: `internal/config/mcp_config_test.go`
  - Unit tests for MCP defaults and agent config resolution.
- Modify: `internal/mcp/client.go`
  - Keep existing `Client` interface and add factory types used by runtime injection.
- Modify: `internal/mcp/manager.go`
  - Add `NewManagerWithFactory`, `NewAggregatedManager`, disabled-server filtering, and close hooks.
- Modify: `internal/mcp/http.go`
  - Keep the existing direct JSON-RPC-over-HTTP client for backward compatibility.
- Create: `internal/mcp/streamable_http.go`
  - Streamable HTTP client with `/stream` initialize, session header, notification, request, and delete behavior.
- Create: `internal/mcp/gateway_sse.go`
  - Lucky gateway SSE-message client using `POST /message`.
- Create: `internal/mcp/manager_test.go`
  - Manager factory, disabled filtering, and aggregated tool prefix tests.
- Create: `internal/mcp/streamable_http_test.go`
  - Streamable HTTP handshake and tool-call tests using `httptest`.
- Create: `internal/mcp/gateway_sse_test.go`
  - SSE-message client tests using `httptest`.
- Modify: `internal/store/store.go`
  - Add `MCPGatewayRuntimeRecord` and Store interface methods.
- Modify: `internal/store/database.go`
  - Add SQL schema and CRUD for gateway runtimes.
- Modify: `internal/store/database_mysql.go`
  - Add MySQL schema for gateway runtimes.
- Create: `internal/store/mcp_gateway_runtime_test.go`
  - SQLite migration and CRUD tests.
- Modify: `internal/config/env.go`
  - Add `BKCRAB_MCP_GATEWAY_*` bootstrap env parsing.
- Create: `internal/mcp/runtime/config.go`
  - Runtime config defaults and env conversion.
- Create: `internal/mcp/runtime/docker.go`
  - Docker CLI adapter behind a small interface.
- Create: `internal/mcp/runtime/service.go`
  - Per-user gateway lifecycle, deploy, test, status, refs, idle stop.
- Create: `internal/mcp/runtime/gateway_client.go`
  - `/deploy` HTTP client and lucky payload conversion.
- Create: `internal/mcp/runtime/service_test.go`
  - Fake Docker/store/gateway tests for lifecycle and deploy behavior.
- Modify: `internal/agent/manager.go`
  - Add MCP manager factory option and close-all behavior.
- Modify: `internal/agent/loop.go`
  - Build MCP managers through the injected factory and close them when agents are removed.
- Create: `internal/agent/mcp_factory_test.go`
  - Prove injected MCP factory is used and close hooks run.
- Modify: `internal/gateway/gateway.go`
  - Construct MCP runtime service, start/stop its sweeper, expose it to setup.
- Modify: `internal/gateway/userspace.go`
  - Pass MCP factory into agent managers and gate foreign public-agent MCP with `shareMcpConfig`.
- Create: `internal/gateway/userspace_mcp_test.go`
  - Owner/foreign sharing tests for MCP config and runtime owner selection.
- Modify: `cmd/bkcrab/main.go`
  - Wire gateway MCP runtime into the setup server.
- Modify: `internal/setup/server.go`
  - Add MCP runtime field, setter, and HTTP routes.
- Create: `internal/setup/handlers_mcp.go`
  - Agent MCP read/write/test/status handlers, validation, masking, merge semantics.
- Create: `internal/setup/handlers_mcp_test.go`
  - Handler and helper tests for owner auth, masking, validation, and invalidation.
- Modify: `web/src/lib/api.ts`
  - Add MCP types and API client methods.
- Create: `web/src/app/agents/[id]/mcp/page.tsx`
  - Agent MCP configuration page.
- Modify: `web/src/components/agent-settings-dialog.tsx`
  - Add MCP tab and icon.
- Modify: `web/src/components/app-sidebar.tsx`
  - Include `mcp` in agent-route extraction if direct route is opened.
- Create: `docs/mcp-gateway.md`
  - Operator notes for environment variables, Docker requirement, and V1 header support.

---

### Task 1: Config Model and Default Semantics

**Files:**
- Modify: `internal/config/config.go`
- Create: `internal/config/mcp_config_test.go`

- [ ] **Step 1: Write failing config tests**

Add `internal/config/mcp_config_test.go`:

```go
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
```

- [ ] **Step 2: Run the new tests and verify they fail**

Run: `go test ./internal/config -run 'TestMCP' -count=1`

Expected: compile failure mentioning `MCPServerEnabled`, `NormalizeMCPTransport`, `MCPTransportStreamableHTTP`, or `ShareMCPConfig`.

- [ ] **Step 3: Add MCP config fields and helpers**

In `internal/config/config.go`, extend `MCPServerConfig` and add helpers near the type:

```go
const (
	MCPTransportSSE            = "sse"
	MCPTransportStreamableHTTP = "streamable-http"
)

type MCPServerConfig struct {
	Type      string            `json:"type"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Transport string            `json:"transport,omitempty"`
	Enabled   *bool             `json:"enabled,omitempty"`
}

func MCPServerEnabled(cfg MCPServerConfig) bool {
	if cfg.Enabled == nil {
		return true
	}
	return *cfg.Enabled
}

func NormalizeMCPTransport(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "streamhttp", "streamable", "streamable-http":
		return MCPTransportStreamableHTTP
	case "sse":
		return MCPTransportSSE
	default:
		return strings.ToLower(strings.TrimSpace(v))
	}
}

func MCPTransportValid(v string) bool {
	switch NormalizeMCPTransport(v) {
	case MCPTransportSSE, MCPTransportStreamableHTTP:
		return true
	default:
		return false
	}
}
```

`internal/config/config.go` already imports `strings`, so no new import should be needed for these helpers.

- [ ] **Step 4: Add `shareMcpConfig` to agent file and resolved config**

In `AgentFileConfig`, add:

```go
ShareMCPConfig bool `json:"shareMcpConfig,omitempty"`
```

In `ResolvedAgent`, add:

```go
ShareMCPConfig bool
```

In `MergedAgentConfig`, inside the existing `if fileCfg, ok := m.AgentOverrides[base.ID]; ok` block, copy the value before returning:

```go
if fileCfg.ShareMCPConfig {
	resolved.ShareMCPConfig = true
}
```

- [ ] **Step 5: Run config tests**

Run: `go test ./internal/config -run 'TestMCP' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/mcp_config_test.go
git commit -m "feat: add agent mcp config semantics"
```

---

### Task 2: MCP Manager Factories and Gateway Transports

**Files:**
- Modify: `internal/mcp/client.go`
- Modify: `internal/mcp/manager.go`
- Modify: `internal/mcp/http.go`
- Create: `internal/mcp/streamable_http.go`
- Create: `internal/mcp/gateway_sse.go`
- Create: `internal/mcp/manager_test.go`
- Create: `internal/mcp/streamable_http_test.go`
- Create: `internal/mcp/gateway_sse_test.go`

- [ ] **Step 1: Write manager tests**

Create `internal/mcp/manager_test.go`:

```go
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
```

- [ ] **Step 2: Write Streamable HTTP client tests**

Create `internal/mcp/streamable_http_test.go`:

```go
package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamableHTTPClientHandshakeAndTools(t *testing.T) {
	var sessionSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-1")
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"protocolVersion":"2025-03-26"}`),
			})
		case "notifications/initialized":
			sessionSeen = r.Header.Get("Mcp-Session-Id")
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Fatalf("tools/list missing session header")
			}
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"tools":[{"name":"time_now","description":"time","inputSchema":{"type":"object"}}]}`),
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  json.RawMessage(`{"content":[{"type":"text","text":"noon"}]}`),
			})
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer srv.Close()

	client := NewStreamableHTTPClient(srv.URL, nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if sessionSeen != "sess-1" {
		t.Fatalf("initialized notification session = %q", sessionSeen)
	}
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "time_now" {
		t.Fatalf("tools = %#v", tools)
	}
	out, err := client.CallTool("time_now", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if out != "noon" {
		t.Fatalf("call output = %q", out)
	}
}
```

- [ ] **Step 3: Write SSE-message client tests**

Create `internal/mcp/gateway_sse_test.go`:

```go
package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGatewaySSEClientUsesMessageEndpoint(t *testing.T) {
	var methods []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/message" {
			t.Fatalf("path = %q, want /message", r.URL.Path)
		}
		var req jsonRPCRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		methods = append(methods, req.Method)
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{}`)})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"tools":[{"name":"fs_read","description":"read","inputSchema":{"type":"object"}}]}`)})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"file"}]}`)})
		default:
			t.Fatalf("unexpected method %s", req.Method)
		}
	}))
	defer srv.Close()

	client := NewGatewaySSEClient(srv.URL+"/message", nil)
	if err := client.Connect(); err != nil {
		t.Fatalf("connect: %v", err)
	}
	tools, err := client.ListTools()
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "fs_read" {
		t.Fatalf("tools = %#v", tools)
	}
	out, err := client.CallTool("fs_read", json.RawMessage(`{"path":"a.txt"}`))
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if out != "file" {
		t.Fatalf("output = %q", out)
	}
	if len(methods) != 3 {
		t.Fatalf("method count = %d, want 3", len(methods))
	}
}
```

- [ ] **Step 4: Run MCP tests and verify they fail**

Run: `go test ./internal/mcp -run 'Test(NewManager|Aggregated|ManagerClose|Prefix|Streamable|GatewaySSE)' -count=1`

Expected: compile failure for missing factory, aggregated manager, and new clients.

- [ ] **Step 5: Add factory types and manager support**

In `internal/mcp/client.go`, add:

```go
type ClientFactory func(name string, cfg config.MCPServerConfig) (Client, error)
```

Add the `config` import to `client.go`.

In `internal/mcp/manager.go`, add factory and aggregated constructors:

```go
type Manager struct {
	servers    map[string]Client
	toolMap    map[string]toolRoute
	closeHooks []func()
}

func NewManagerWithFactory(servers map[string]config.MCPServerConfig, factory ClientFactory) *Manager {
	m := &Manager{
		servers: make(map[string]Client),
		toolMap: make(map[string]toolRoute),
	}
	for name, cfg := range servers {
		if !config.MCPServerEnabled(cfg) {
			continue
		}
		client, err := factory(name, cfg)
		if err != nil {
			slog.Warn("failed to create MCP client, skipping", "server", name, "error", err)
			continue
		}
		if err := client.Connect(); err != nil {
			slog.Warn("failed to connect to MCP server, skipping", "server", name, "error", err)
			continue
		}
		tools, err := client.ListTools()
		if err != nil {
			slog.Warn("failed to list MCP tools, skipping", "server", name, "error", err)
			_ = client.Close()
			continue
		}
		m.servers[name] = client
		for _, t := range tools {
			prefixed := prefixToolName(name, t.Name)
			m.toolMap[prefixed] = toolRoute{serverName: name, originalName: t.Name}
		}
	}
	return m
}

func NewAggregatedManager(client Client) *Manager {
	m := &Manager{
		servers: map[string]Client{"gateway": client},
		toolMap: make(map[string]toolRoute),
	}
	if err := client.Connect(); err != nil {
		slog.Warn("failed to connect to aggregated MCP gateway", "error", err)
		return m
	}
	tools, err := client.ListTools()
	if err != nil {
		slog.Warn("failed to list aggregated MCP gateway tools", "error", err)
		_ = client.Close()
		return m
	}
	for _, t := range tools {
		m.toolMap[prefixAggregatedToolName(t.Name)] = toolRoute{
			serverName:   "gateway",
			originalName: t.Name,
		}
	}
	return m
}

func (m *Manager) AddCloseHook(fn func()) {
	if fn != nil {
		m.closeHooks = append(m.closeHooks, fn)
	}
}

func prefixAggregatedToolName(toolName string) string {
	return "mcp_" + sanitizeToolName(toolName)
}

func sanitizeToolName(v string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, v)
}
```

Change `prefixToolName` to call `sanitizeToolName(serverName)` instead of duplicating the sanitizer. Update `Close` to run hooks after closing clients:

```go
func (m *Manager) Close() {
	for name, client := range m.servers {
		if err := client.Close(); err != nil {
			slog.Warn("error closing MCP server", "server", name, "error", err)
		}
	}
	for _, fn := range m.closeHooks {
		fn()
	}
	m.closeHooks = nil
}
```

Make existing `NewManager` delegate:

```go
func NewManager(servers map[string]config.MCPServerConfig) *Manager {
	return NewManagerWithFactory(servers, NewClientFromConfig)
}
```

- [ ] **Step 6: Add client selection**

Add this function to `internal/mcp/client.go`:

```go
func NewClientFromConfig(name string, cfg config.MCPServerConfig) (Client, error) {
	switch cfg.Type {
	case "http":
		switch config.NormalizeMCPTransport(cfg.Transport) {
		case config.MCPTransportStreamableHTTP:
			return NewStreamableHTTPClient(cfg.URL, cfg.Headers), nil
		case config.MCPTransportSSE:
			return NewGatewaySSEClient(cfg.URL, cfg.Headers), nil
		default:
			return NewHTTPClient(cfg.URL, cfg.Headers), nil
		}
	case "stdio":
		return NewStdioClient(cfg.Command, cfg.Args, cfg.Env), nil
	default:
		return nil, fmt.Errorf("unknown MCP server type %q for %s", cfg.Type, name)
	}
}
```

Add `fmt` and `github.com/qs3c/bkcrab/internal/config` imports to `client.go`.

- [ ] **Step 7: Implement `StreamableHTTPClient`**

Create `internal/mcp/streamable_http.go` with a struct mirroring `HTTPClient`, plus `sessionID string`. Use:

```go
const mcpSessionHeader = "Mcp-Session-Id"

func NewStreamableHTTPClient(url string, headers map[string]string) *StreamableHTTPClient {
	return &StreamableHTTPClient{
		url:     url,
		headers: expandHeaders(headers),
		client:  &http.Client{},
		nextID:  1,
	}
}
```

Implement `Connect` as:

```go
func (c *StreamableHTTPClient) Connect() error {
	resp, err := c.sendRequest("initialize", initializeParams{
		ProtocolVersion: "2025-03-26",
		ClientInfo:      clientInfo{Name: "bkcrab", Version: "0.1.0"},
	}, false)
	if err != nil {
		return err
	}
	if resp.SessionID == "" {
		return fmt.Errorf("streamable HTTP initialize did not return %s", mcpSessionHeader)
	}
	c.sessionID = resp.SessionID
	return c.sendNotification("notifications/initialized", struct{}{})
}
```

`sendRequest` must set `Accept: application/json, text/event-stream`, `Content-Type: application/json`, all configured headers, and `Mcp-Session-Id` after initialization. `sendNotification` must accept `202 Accepted` or `200 OK`. `Close` must send `DELETE` with `Mcp-Session-Id` when a session exists.

- [ ] **Step 8: Implement `GatewaySSEClient`**

Create `internal/mcp/gateway_sse.go` as a thin JSON-RPC POST client that uses the existing `HTTPClient` request shape against `/message`:

```go
type GatewaySSEClient struct {
	*HTTPClient
}

func NewGatewaySSEClient(messageURL string, headers map[string]string) *GatewaySSEClient {
	return &GatewaySSEClient{HTTPClient: NewHTTPClient(messageURL, headers)}
}
```

This client intentionally does not keep a long-lived `GET /sse` stream because BkCrab's current MCP tool use only needs request/response `initialize`, `tools/list`, and `tools/call`. lucky-aeon supports these through `POST /message`.

- [ ] **Step 9: Run MCP tests**

Run: `go test ./internal/mcp -count=1`

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/mcp
git commit -m "feat: add gateway mcp transports"
```

---

### Task 3: Persist Per-User Gateway Runtime State

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Create: `internal/store/mcp_gateway_runtime_test.go`

- [ ] **Step 1: Write runtime store tests**

Create `internal/store/mcp_gateway_runtime_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestMCPGatewayRuntimeCRUD(t *testing.T) {
	st, err := NewDBStore("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := &MCPGatewayRuntimeRecord{
		ID:                  "mgr_u1",
		UserID:              "u1",
		Status:              "running",
		DockerContainerID:   "container-1",
		ContainerName:       "bkcrab-mcp-u1",
		Image:               "ghcr.io/lucky-aeon/mcp-gateway:latest",
		InternalPort:        8080,
		ExternalPort:        39123,
		BaseURL:             "http://127.0.0.1:39123",
		APIKey:              "secret",
		DeployedServersJSON: `{"time":{"command":"uvx"}}`,
		LastAccessedAt:      now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}
	if err := st.SaveMCPGatewayRuntime(ctx, rec); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := st.GetMCPGatewayRuntime(ctx, "u1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.BaseURL != rec.BaseURL || got.Status != "running" {
		t.Fatalf("got runtime = %#v", got)
	}
	got.Status = "stopped"
	got.ErrorMessage = "idle"
	if err := st.SaveMCPGatewayRuntime(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	list, err := st.ListMCPGatewayRuntimesByStatus(ctx, "stopped")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].UserID != "u1" {
		t.Fatalf("list = %#v", list)
	}
}
```

- [ ] **Step 2: Run the store test and verify it fails**

Run: `go test ./internal/store -run TestMCPGatewayRuntimeCRUD -count=1`

Expected: compile failure for missing `MCPGatewayRuntimeRecord` and Store methods.

- [ ] **Step 3: Add store interface and record**

In `internal/store/store.go`, add Store methods after config methods:

```go
GetMCPGatewayRuntime(ctx context.Context, userID string) (*MCPGatewayRuntimeRecord, error)
SaveMCPGatewayRuntime(ctx context.Context, rec *MCPGatewayRuntimeRecord) error
ListMCPGatewayRuntimesByStatus(ctx context.Context, statuses ...string) ([]MCPGatewayRuntimeRecord, error)
```

Add the record near other persistent record structs:

```go
type MCPGatewayRuntimeRecord struct {
	ID                  string    `json:"id"`
	UserID              string    `json:"userId"`
	Status              string    `json:"status"`
	DockerContainerID   string    `json:"dockerContainerId,omitempty"`
	ContainerName       string    `json:"containerName,omitempty"`
	Image               string    `json:"image"`
	InternalPort        int       `json:"internalPort"`
	ExternalPort        int       `json:"externalPort,omitempty"`
	BaseURL             string    `json:"baseUrl,omitempty"`
	APIKey              string    `json:"-"`
	DeployedServersJSON string    `json:"deployedServersJson,omitempty"`
	LastAccessedAt      time.Time `json:"lastAccessedAt"`
	ErrorMessage        string    `json:"errorMessage,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}
```

- [ ] **Step 4: Add SQL schema**

In `internal/store/database.go` `migrationSQL`, add:

```sql
CREATE TABLE IF NOT EXISTS mcp_gateway_runtimes (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL UNIQUE,
	status TEXT NOT NULL,
	docker_container_id TEXT NOT NULL DEFAULT '',
	container_name TEXT NOT NULL DEFAULT '',
	image TEXT NOT NULL,
	internal_port INTEGER NOT NULL,
	external_port INTEGER NOT NULL DEFAULT 0,
	base_url TEXT NOT NULL DEFAULT '',
	api_key TEXT NOT NULL DEFAULT '',
	deployed_servers_json TEXT NOT NULL DEFAULT '',
	last_accessed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	error_message TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
)
```

Add:

```sql
CREATE INDEX IF NOT EXISTS idx_mcp_gateway_runtimes_status ON mcp_gateway_runtimes (status, last_accessed_at)
```

Add the MySQL equivalent to `internal/store/database_mysql.go`:

```sql
CREATE TABLE IF NOT EXISTS mcp_gateway_runtimes (
	id VARCHAR(120) PRIMARY KEY,
	user_id VARCHAR(120) NOT NULL UNIQUE,
	status VARCHAR(32) NOT NULL,
	docker_container_id VARCHAR(191) NOT NULL DEFAULT '',
	container_name VARCHAR(191) NOT NULL DEFAULT '',
	image VARCHAR(255) NOT NULL,
	internal_port INTEGER NOT NULL,
	external_port INTEGER NOT NULL DEFAULT 0,
	base_url VARCHAR(512) NOT NULL DEFAULT '',
	api_key VARCHAR(255) NOT NULL DEFAULT '',
	deployed_servers_json LONGTEXT NOT NULL,
	last_accessed_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
	error_message LONGTEXT NOT NULL,
	created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
	updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
	KEY idx_mcp_gateway_runtimes_status (status, last_accessed_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci
```

- [ ] **Step 5: Implement CRUD**

In `internal/store/database.go`, add:

```go
const mcpGatewayRuntimeColumns = `id, user_id, status, docker_container_id, container_name, image, internal_port, external_port, base_url, api_key, deployed_servers_json, last_accessed_at, error_message, created_at, updated_at`

func scanMCPGatewayRuntime(scanner interface{ Scan(dest ...any) error }) (*MCPGatewayRuntimeRecord, error) {
	var rec MCPGatewayRuntimeRecord
	if err := scanner.Scan(&rec.ID, &rec.UserID, &rec.Status, &rec.DockerContainerID, &rec.ContainerName, &rec.Image, &rec.InternalPort, &rec.ExternalPort, &rec.BaseURL, &rec.APIKey, &rec.DeployedServersJSON, &rec.LastAccessedAt, &rec.ErrorMessage, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		return nil, err
	}
	return &rec, nil
}
```

Implement `GetMCPGatewayRuntime`, `SaveMCPGatewayRuntime`, and `ListMCPGatewayRuntimesByStatus`. `SaveMCPGatewayRuntime` must:

```go
if rec.ID == "" {
	rec.ID = "mcpgr_" + uuid.NewString()
}
if rec.UserID == "" {
	return errors.New("store: mcp gateway runtime.user_id is required")
}
now := time.Now().UTC()
if rec.CreatedAt.IsZero() {
	rec.CreatedAt = now
}
rec.UpdatedAt = now
if rec.LastAccessedAt.IsZero() {
	rec.LastAccessedAt = now
}
```

Use dialect-specific upsert:

- SQLite/Postgres: `ON CONFLICT (user_id) DO UPDATE SET status=excluded.status, docker_container_id=excluded.docker_container_id, container_name=excluded.container_name, image=excluded.image, internal_port=excluded.internal_port, external_port=excluded.external_port, base_url=excluded.base_url, api_key=excluded.api_key, deployed_servers_json=excluded.deployed_servers_json, last_accessed_at=excluded.last_accessed_at, error_message=excluded.error_message, updated_at=excluded.updated_at`
- MySQL: `ON DUPLICATE KEY UPDATE status=VALUES(status), docker_container_id=VALUES(docker_container_id), container_name=VALUES(container_name), image=VALUES(image), internal_port=VALUES(internal_port), external_port=VALUES(external_port), base_url=VALUES(base_url), api_key=VALUES(api_key), deployed_servers_json=VALUES(deployed_servers_json), last_accessed_at=VALUES(last_accessed_at), error_message=VALUES(error_message), updated_at=VALUES(updated_at)`

- [ ] **Step 6: Run store tests**

Run: `go test ./internal/store -run 'TestMCPGatewayRuntime|TestMigrate' -count=1`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store/store.go internal/store/database.go internal/store/database_mysql.go internal/store/mcp_gateway_runtime_test.go
git commit -m "feat: persist mcp gateway runtimes"
```

---

### Task 4: Gateway Runtime Service and Docker Lifecycle

**Files:**
- Modify: `internal/config/env.go`
- Create: `internal/mcp/runtime/config.go`
- Create: `internal/mcp/runtime/docker.go`
- Create: `internal/mcp/runtime/gateway_client.go`
- Create: `internal/mcp/runtime/service.go`
- Create: `internal/mcp/runtime/service_test.go`

- [ ] **Step 1: Write runtime service tests**

Create `internal/mcp/runtime/service_test.go`:

```go
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
```

The fake store intentionally implements only the runtime methods used by this package; `Options.Store` should depend on a narrow interface in `internal/mcp/runtime`, not the full `store.Store`.

- [ ] **Step 2: Run runtime tests and verify they fail**

Run: `go test ./internal/mcp/runtime -count=1`

Expected: package not found or compile failure for missing runtime service.

- [ ] **Step 3: Add env config**

In `internal/config/env.go`, extend `EnvConfig`:

```go
MCPGateway EnvMCPGateway
```

Add:

```go
type EnvMCPGateway struct {
	Enabled       bool
	Image         string
	RuntimeDir    string
	ContainerPort int
	Protocol      string
	IdleTTLSec    int
}
```

In `LoadEnv`, default to enabled without starting Docker until an agent actually uses MCP:

```go
cfg.MCPGateway = EnvMCPGateway{
	Enabled:       true,
	Image:         "ghcr.io/lucky-aeon/mcp-gateway:latest",
	ContainerPort: 8080,
	Protocol:      "all",
	IdleTTLSec:    1800,
}
if v := os.Getenv("BKCRAB_MCP_GATEWAY_ENABLED"); v != "" {
	cfg.MCPGateway.Enabled = v == "true" || v == "1"
}
if v := os.Getenv("BKCRAB_MCP_GATEWAY_IMAGE"); v != "" {
	cfg.MCPGateway.Image = v
}
if v := os.Getenv("BKCRAB_MCP_GATEWAY_RUNTIME_DIR"); v != "" {
	cfg.MCPGateway.RuntimeDir = v
}
if v := os.Getenv("BKCRAB_MCP_GATEWAY_CONTAINER_PORT"); v != "" {
	if p, err := strconv.Atoi(v); err == nil && p > 0 {
		cfg.MCPGateway.ContainerPort = p
	}
}
if v := os.Getenv("BKCRAB_MCP_GATEWAY_PROTOCOL"); v != "" {
	cfg.MCPGateway.Protocol = v
}
if v := os.Getenv("BKCRAB_MCP_GATEWAY_IDLE_TTL_SEC"); v != "" {
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		cfg.MCPGateway.IdleTTLSec = n
	}
}
```

- [ ] **Step 4: Add runtime config**

Create `internal/mcp/runtime/config.go`:

```go
package runtime

import (
	"path/filepath"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

const defaultImage = "ghcr.io/lucky-aeon/mcp-gateway:latest"

type Config struct {
	Enabled       bool
	Image         string
	RuntimeDir    string
	ContainerPort int
	Protocol      string
	IdleTTL       time.Duration
}

func FromEnv(env config.EnvMCPGateway) Config {
	home, _ := config.HomeDir()
	dir := env.RuntimeDir
	if dir == "" {
		dir = filepath.Join(home, "mcp-gateways")
	}
	image := env.Image
	if image == "" {
		image = defaultImage
	}
	port := env.ContainerPort
	if port == 0 {
		port = 8080
	}
	protocol := env.Protocol
	if protocol == "" {
		protocol = "all"
	}
	idle := time.Duration(env.IdleTTLSec) * time.Second
	if idle <= 0 {
		idle = 30 * time.Minute
	}
	return Config{Enabled: env.Enabled, Image: image, RuntimeDir: dir, ContainerPort: port, Protocol: protocol, IdleTTL: idle}
}
```

- [ ] **Step 5: Add Docker adapter interfaces**

Create `internal/mcp/runtime/docker.go`:

```go
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type DockerClient interface {
	Ensure(ctx context.Context, spec ContainerSpec) (ContainerRef, error)
	Stop(ctx context.Context, name string) error
}

type ContainerSpec struct {
	Name          string
	Image         string
	ConfigDir     string
	ContainerPort int
}

type ContainerRef struct {
	ID           string
	Name         string
	BaseURL      string
	ExternalPort int
	Running      bool
}

type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}
```

Implement `CLIClient.Ensure` to:

1. Write `config.json` in `spec.ConfigDir`:

```json
{
  "LogLevel": 0,
  "WorkspacePath": "./vm",
  "Bind": "[::]:8080",
  "Auth": {"Enabled": false, "ApiKey": "bkcrab-local"},
  "GatewayProtocol": "all",
  "McpServiceMgrConfig": {"McpServiceRetryCount": 3}
}
```

2. Run `docker inspect <name>`; if running, return parsed port.
3. If not present, run:

```text
docker run -d --name <name> -p 127.0.0.1::<containerPort> -v <configDir>:/app/vm --restart unless-stopped <image>
```

4. Resolve `BaseURL` from `docker port <name> <containerPort>/tcp`.

- [ ] **Step 6: Add lucky deploy client and payload conversion**

Create `internal/mcp/runtime/gateway_client.go` with:

```go
type LuckyServerConfig struct {
	URL             string            `json:"url,omitempty"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	GatewayProtocol string            `json:"gateway_protocol,omitempty"`
}

func ToLuckyServerConfig(src config.MCPServerConfig) (LuckyServerConfig, error) {
	dst := LuckyServerConfig{
		URL:             strings.TrimSpace(src.URL),
		Command:         strings.TrimSpace(src.Command),
		Args:            append([]string(nil), src.Args...),
		Env:             copyStringMap(src.Env),
		GatewayProtocol: luckyProtocol(src.Transport),
	}
	if dst.Env == nil {
		dst.Env = map[string]string{}
	}
	if auth := strings.TrimSpace(src.Headers["Authorization"]); auth != "" {
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			return dst, fmt.Errorf("only Authorization: Bearer tokens are supported for remote HTTP MCP servers")
		}
		dst.Env["MCP_REMOTE_AUTH_ACCESS_TOKEN"] = strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	}
	for k, v := range src.Headers {
		if strings.EqualFold(k, "Authorization") && strings.TrimSpace(v) != "" {
			continue
		}
		if strings.TrimSpace(v) != "" {
			return dst, fmt.Errorf("header %q is not supported by the selected MCP gateway", k)
		}
	}
	return dst, nil
}
```

`luckyProtocol` maps `streamable-http` to `streamhttp`, `sse` to `sse`, empty to `streamhttp`.

Implement:

```go
func DeployToGateway(ctx context.Context, client *http.Client, baseURL string, servers map[string]config.MCPServerConfig) error
```

It sends:

```go
body := struct {
	MCPServers map[string]LuckyServerConfig `json:"mcpServers"`
}{MCPServers: converted}
```

to `POST <baseURL>/deploy` with JSON content type and requires a 2xx response.

- [ ] **Step 7: Add runtime service**

Create `internal/mcp/runtime/service.go` with statuses:

```go
const (
	StatusRunning = "running"
	StatusStopped = "stopped"
	StatusError   = "error"
)
```

Use narrow interfaces:

```go
type RuntimeStore interface {
	GetMCPGatewayRuntime(ctx context.Context, userID string) (*store.MCPGatewayRuntimeRecord, error)
	SaveMCPGatewayRuntime(ctx context.Context, rec *store.MCPGatewayRuntimeRecord) error
	ListMCPGatewayRuntimesByStatus(ctx context.Context, statuses ...string) ([]store.MCPGatewayRuntimeRecord, error)
}

type Service struct {
	store      RuntimeStore
	docker     DockerClient
	httpClient *http.Client
	cfg        Config
	mu         sync.Mutex
	refs       map[string]int
}
```

Implement:

```go
func (s *Service) Deploy(ctx context.Context, userID string, servers map[string]config.MCPServerConfig) (*store.MCPGatewayRuntimeRecord, error)
func (s *Service) NewManagerForAgent(ctx context.Context, rc config.ResolvedAgent) (*mcp.Manager, error)
func (s *Service) NewManagerFromServers(ctx context.Context, userID string, servers map[string]config.MCPServerConfig) (*mcp.Manager, error)
func (s *Service) Acquire(userID string) func()
func (s *Service) StopIdle(ctx context.Context, now time.Time) error
func (s *Service) Start(ctx context.Context)
func (s *Service) StopAll(ctx context.Context) error
func (s *Service) Status(ctx context.Context, userID string) (*store.MCPGatewayRuntimeRecord, error)
```

Container names must be deterministic and safe:

```go
func containerName(userID string) string {
	sum := sha256.Sum256([]byte(userID))
	return "bkcrab-mcp-gateway-" + hex.EncodeToString(sum[:])[:16]
}
```

`Deploy` must:

1. Return a clear error if `cfg.Enabled` is false.
2. Ensure a container with `docker.Ensure`.
3. Call `DeployToGateway`.
4. Store `StatusRunning`, `BaseURL`, `ContainerName`, `DockerContainerID`, ports, image, `DeployedServersJSON`, and timestamps.

`NewManagerFromServers` must call `Deploy`, call `Acquire`, create one streamable HTTP gateway client against `record.BaseURL + "/stream"`, wrap it with `mcp.NewAggregatedManager`, and register a close hook that releases the acquired reference. `NewManagerForAgent` must call `NewManagerFromServers(ctx, rc.UserID, rc.MCPServers)`.

`Acquire` increments an in-memory ref count and returns a release function that decrements once. `StopIdle` stops only records with `StatusRunning`, refs equal zero, and `LastAccessedAt` older than `cfg.IdleTTL`.

- [ ] **Step 8: Run runtime tests**

Run: `go test ./internal/mcp/runtime -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/config/env.go internal/mcp/runtime
git commit -m "feat: manage per-user mcp gateway runtime"
```

---

### Task 5: Wire Gateway Runtime Into Agents and User Spaces

**Files:**
- Modify: `internal/agent/manager.go`
- Modify: `internal/agent/loop.go`
- Create: `internal/agent/mcp_factory_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/userspace.go`
- Create: `internal/gateway/userspace_mcp_test.go`
- Modify: `cmd/bkcrab/main.go`

- [ ] **Step 1: Write agent factory tests**

Create `internal/agent/mcp_factory_test.go`:

```go
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
func (testMCPClient) CallTool(name string, args json.RawMessage) (string, error) { return "ok", nil }
func (testMCPClient) Close() error { return nil }
```

- [ ] **Step 2: Run agent test and verify it fails**

Run: `go test ./internal/agent -run TestManagerUsesInjectedMCPFactory -count=1`

Expected: compile failure for missing `WithMCPManagerFactory` and `MCPManagerFactoryFunc`.

- [ ] **Step 3: Add agent MCP factory option**

In `internal/agent/manager.go`, add:

```go
type MCPManagerFactory interface {
	NewMCPManager(ctx context.Context, rc config.ResolvedAgent) (*mcp.Manager, error)
}

type MCPManagerFactoryFunc func(ctx context.Context, rc config.ResolvedAgent) (*mcp.Manager, error)

func (f MCPManagerFactoryFunc) NewMCPManager(ctx context.Context, rc config.ResolvedAgent) (*mcp.Manager, error) {
	return f(ctx, rc)
}
```

Add `mcpFactory MCPManagerFactory` to `managerOpts`, and:

```go
func WithMCPManagerFactory(f MCPManagerFactory) ManagerOption {
	return func(o *managerOpts) { o.mcpFactory = f }
}
```

Add imports for `context` and `github.com/qs3c/bkcrab/internal/mcp`.

- [ ] **Step 4: Use the factory in agent construction**

In `internal/agent/loop.go`, replace the MCP block with:

```go
if len(rc.MCPServers) > 0 {
	var mcpMgr *mcp.Manager
	if ag.mcpFactory != nil {
		var err error
		mcpMgr, err = ag.mcpFactory.NewMCPManager(context.Background(), rc)
		if err != nil {
			slog.Warn("failed to create gateway-backed MCP manager", "agent", rc.ID, "error", err)
		}
	} else {
		mcpMgr = mcp.NewManager(rc.MCPServers)
	}
	if mcpMgr != nil {
		ag.mcpMgr = mcpMgr
		for _, td := range mcpMgr.ToolDefs() {
			toolName := td.Name
			ag.registry.Register(toolName, td.Description, td.InputSchema,
				func(ctx context.Context, args json.RawMessage) (string, error) {
					return mcpMgr.CallTool(ctx, toolName, args)
				},
			)
		}
		if mcpMgr.HasTools() {
			slog.Info("registered MCP tools", "agent", rc.ID)
		}
	}
}
```

Add `mcpFactory` to `Agent` in `internal/agent/loop.go` and set it from manager options immediately after `NewAgentWithSkillsCfg` in `buildAgent`:

```go
ag.mcpFactory = m.opts.mcpFactory
```

- [ ] **Step 5: Close MCP managers when agents are removed**

In `internal/agent/loop.go`, add:

```go
func (a *Agent) Close() {
	if a == nil || a.mcpMgr == nil {
		return
	}
	a.mcpMgr.Close()
	a.mcpMgr = nil
}
```

In `internal/agent/manager.go`, update `RemoveAgent` and add `CloseAll`:

```go
func (m *Manager) RemoveAgent(id string) {
	ag, ok := m.agents[id]
	if !ok {
		return
	}
	ag.Close()
	delete(m.agents, id)
	if m.defaultAgent != nil && m.defaultAgent.Name() == id {
		m.defaultAgent = nil
	}
	slog.Info("agent removed dynamically", "id", id)
}

func (m *Manager) CloseAll() {
	for id, ag := range m.agents {
		ag.Close()
		delete(m.agents, id)
	}
	m.defaultAgent = nil
}
```

- [ ] **Step 6: Run agent tests**

Run: `go test ./internal/agent -run 'TestManagerUsesInjectedMCPFactory' -count=1`

Expected: PASS.

- [ ] **Step 7: Add gateway runtime wiring**

In `internal/gateway/gateway.go`, add a field to `Gateway`:

```go
mcpRuntime *mcpruntime.Service
```

Import the package with an alias:

```go
mcpruntime "github.com/qs3c/bkcrab/internal/mcp/runtime"
```

In `New`, after the existing `systemSandboxPool` initialization, create:

```go
mcpRuntime := mcpruntime.NewService(mcpruntime.Options{
	Store:  st,
	Docker: mcpruntime.NewDockerCLIClient(),
	Config: mcpruntime.FromEnv(env.MCPGateway),
})
```

Pass it into `newUserSpaceRegistry`. Add:

```go
func (g *Gateway) MCPRuntime() *mcpruntime.Service { return g.mcpRuntime }
```

In `Run`, start and stop it:

```go
if g.mcpRuntime != nil {
	wg.Add(1)
	go func() { defer wg.Done(); g.mcpRuntime.Start(ctx) }()
}
```

Before logging `gateway stopped`:

```go
if g.mcpRuntime != nil {
	_ = g.mcpRuntime.StopAll(context.Background())
}
```

- [ ] **Step 8: Pass runtime into user spaces**

In `internal/gateway/userspace.go`, extend `UserSpace` and `userSpaceRegistry` with:

```go
MCPRuntime *mcpruntime.Service
```

Add `mcpRuntime *mcpruntime.Service` to `loadUserSpace` and `newUserSpaceRegistry` parameters.

When building `managerOpts`, append:

```go
if mcpRuntime != nil {
	managerOpts = append(managerOpts, agent.WithMCPManagerFactory(agent.MCPManagerFactoryFunc(mcpRuntime.NewManagerForAgent)))
}
```

Add a `Close` method:

```go
func (sp *UserSpace) Close() {
	if sp != nil && sp.Agents != nil {
		sp.Agents.CloseAll()
	}
}
```

Update `evictIdle` to collect evicted spaces, delete them under lock, then call `sp.Close()` outside the lock.

- [ ] **Step 9: Apply public-agent MCP sharing gate**

In `UserSpace.EnsureAgent`, after `isForeign` and before `AddAgentWithSkillsCfg`, read:

```go
shareMCP := false
if v, ok := rec.Config["shareMcpConfig"].(bool); ok {
	shareMCP = v
}
if isForeign && !shareMCP {
	rc.MCPServers = nil
	rc.ShareMCPConfig = false
}
```

When `shareMCP` is true, leave `rc.UserID` as the owner and leave `rc.MCPServers` intact. The runtime factory uses `rc.UserID`, so the owner gateway is selected.

- [ ] **Step 10: Wire setup server**

In `cmd/bkcrab/main.go`, after `webSrv.SetWebChannel(gw.WebChannel())`, add:

```go
webSrv.SetMCPRuntime(gw.MCPRuntime())
```

- [ ] **Step 11: Write gateway sharing tests**

Create `internal/gateway/userspace_mcp_test.go` with a focused helper that calls the new internal function used by `EnsureAgent` to gate MCP sharing. Extract this helper in `userspace.go`:

```go
func applyForeignMCPSharing(rc *config.ResolvedAgent, rec *store.AgentRecord, viewerUserID string) {
	if rc == nil || rec == nil {
		return
	}
	isForeign := rec.UserID != "" && rec.UserID != viewerUserID
	if !isForeign {
		return
	}
	if v, ok := rec.Config["shareMcpConfig"].(bool); ok && v {
		rc.ShareMCPConfig = true
		return
	}
	rc.MCPServers = nil
	rc.ShareMCPConfig = false
}
```

Test it:

```go
func TestApplyForeignMCPSharing(t *testing.T) {
	rc := &config.ResolvedAgent{UserID: "owner", MCPServers: map[string]config.MCPServerConfig{"time": {Type: "stdio"}}}
	rec := &store.AgentRecord{UserID: "owner", Config: map[string]interface{}{}}
	applyForeignMCPSharing(rc, rec, "viewer")
	if len(rc.MCPServers) != 0 {
		t.Fatalf("foreign viewer should not inherit MCP by default: %#v", rc.MCPServers)
	}

	rc = &config.ResolvedAgent{UserID: "owner", MCPServers: map[string]config.MCPServerConfig{"time": {Type: "stdio"}}}
	rec = &store.AgentRecord{UserID: "owner", Config: map[string]interface{}{"shareMcpConfig": true}}
	applyForeignMCPSharing(rc, rec, "viewer")
	if len(rc.MCPServers) != 1 || !rc.ShareMCPConfig {
		t.Fatalf("shared MCP config was not preserved: %#v", rc)
	}
}
```

- [ ] **Step 12: Run integration package tests**

Run: `go test ./internal/agent ./internal/gateway -run 'Test.*MCP|TestManagerUsesInjectedMCPFactory' -count=1`

Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/agent internal/gateway cmd/bkcrab/main.go
git commit -m "feat: route agent mcp through per-user gateway"
```

---

### Task 6: Agent MCP Setup APIs

**Files:**
- Modify: `internal/setup/server.go`
- Create: `internal/setup/handlers_mcp.go`
- Create: `internal/setup/handlers_mcp_test.go`

- [ ] **Step 1: Write helper tests**

Create `internal/setup/handlers_mcp_test.go` with helper tests first:

```go
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
```

- [ ] **Step 2: Run setup helper tests and verify they fail**

Run: `go test ./internal/setup -run 'Test(MaskMCP|MergeMasked|ValidateMCP)' -count=1`

Expected: compile failure for missing helper functions.

- [ ] **Step 3: Add setup server runtime interface and routes**

In `internal/setup/server.go`, import `mcpruntime`:

```go
mcpruntime "github.com/qs3c/bkcrab/internal/mcp/runtime"
```

Add to `Server`:

```go
mcpRuntime *mcpruntime.Service
```

Add setter:

```go
func (s *Server) SetMCPRuntime(rt *mcpruntime.Service) {
	s.mcpRuntime = rt
}
```

Add routes next to existing agent config routes:

```go
mux.HandleFunc("GET /api/agents/{id}/mcp", auth(s.handleGetAgentMCP))
mux.HandleFunc("PUT /api/agents/{id}/mcp", auth(s.handlePutAgentMCP))
mux.HandleFunc("POST /api/agents/{id}/mcp/test", auth(s.handleTestAgentMCP))
mux.HandleFunc("GET /api/agents/{id}/mcp/status", auth(s.handleGetAgentMCPStatus))
```

- [ ] **Step 4: Implement masking and validation helpers**

Create `internal/setup/handlers_mcp.go`:

```go
package setup

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
)

const mcpSecretMask = "********"

var mcpServerNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
```

Implement:

```go
func maskMCPServers(src map[string]config.MCPServerConfig) map[string]config.MCPServerConfig
func mergeMaskedMCPSecrets(old, next map[string]config.MCPServerConfig) map[string]config.MCPServerConfig
func validateMCPServers(servers map[string]config.MCPServerConfig) error
```

Validation must enforce:

- Name matches `^[A-Za-z0-9_-]{1,64}$`
- `type` is `stdio` or `http`
- stdio has `command`, no `url`
- http has `url`, no `command`
- transport is valid when set
- only empty headers or `Authorization: Bearer <token>` are accepted
- disabled servers can keep incomplete command/url only if the UI is saving a draft; implement this by applying command/url validation only when `config.MCPServerEnabled(cfg)` is true

- [ ] **Step 5: Implement read/write handlers**

In `handlers_mcp.go`, implement:

```go
func (s *Server) handleGetAgentMCP(w http.ResponseWriter, r *http.Request)
func (s *Server) handlePutAgentMCP(w http.ResponseWriter, r *http.Request)
```

`GET` must require owner access with `requireAgentOwner`, load `config.AgentFileConfig` from `rec.Config`, and return:

```json
{
  "mcpServers": {},
  "shareMcpConfig": false,
  "gateway": {"status":"stopped"}
}
```

If runtime status is available, include `status`, `baseUrl`, `image`, `lastAccessedAt`, and `errorMessage`.

`PUT` body:

```go
type agentMCPUpdateRequest struct {
	MCPServers     map[string]config.MCPServerConfig `json:"mcpServers"`
	ShareMCPConfig bool                              `json:"shareMcpConfig"`
}
```

On save:

1. Require owner.
2. Load existing `AgentFileConfig`.
3. Merge masked secrets.
4. Validate.
5. Replace `cfg.MCPServers`.
6. Set `cfg.ShareMCPConfig`.
7. Save with `saveAgentFileConfig`.
8. Call `s.invalidateAgent(rec.ID)`.
9. Return masked config.

- [ ] **Step 6: Implement status and test handlers**

Add:

```go
func (s *Server) handleGetAgentMCPStatus(w http.ResponseWriter, r *http.Request)
func (s *Server) handleTestAgentMCP(w http.ResponseWriter, r *http.Request)
```

`status` returns runtime record for the agent owner. `test` requires saved config and runtime:

```go
if s.mcpRuntime == nil {
	jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "mcp gateway runtime is not configured"})
	return
}
```

`test` calls a new runtime method:

```go
tools, err := s.mcpRuntime.TestServers(r.Context(), rec.UserID, cfg.MCPServers)
```

Return:

```json
{"ok": true, "tools": [{"name":"mcp_time_now","description":"Read the current time"}]}
```

- [ ] **Step 7: Add runtime `TestServers` method**

In `internal/mcp/runtime/service.go`, add:

```go
func (s *Service) TestServers(ctx context.Context, userID string, servers map[string]config.MCPServerConfig) ([]mcp.ToolDef, error) {
	mgr, err := s.NewManagerFromServers(ctx, userID, servers)
	if err != nil {
		return nil, err
	}
	defer mgr.Close()
	return mgr.ToolDefs(), nil
}
```

Refactor `NewManagerForAgent` to call `NewManagerFromServers(ctx, rc.UserID, rc.MCPServers)`.

- [ ] **Step 8: Run setup tests**

Run: `go test ./internal/setup -run 'Test(MaskMCP|MergeMasked|ValidateMCP)' -count=1`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/setup internal/mcp/runtime/service.go
git commit -m "feat: add agent mcp setup api"
```

---

### Task 7: Frontend Agent MCP Configuration UI

**Files:**
- Modify: `web/src/lib/api.ts`
- Create: `web/src/app/agents/[id]/mcp/page.tsx`
- Modify: `web/src/components/agent-settings-dialog.tsx`
- Modify: `web/src/components/app-sidebar.tsx`

- [ ] **Step 1: Add frontend API types**

In `web/src/lib/api.ts`, add near `AgentFileConfig`:

```ts
export interface MCPServerConfig {
  type: "stdio" | "http";
  url?: string;
  headers?: Record<string, string>;
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  transport?: "sse" | "streamable-http";
  enabled?: boolean;
}

export interface AgentMCPConfigResponse {
  mcpServers: Record<string, MCPServerConfig>;
  shareMcpConfig: boolean;
  gateway?: {
    status?: string;
    baseUrl?: string;
    image?: string;
    lastAccessedAt?: string;
    errorMessage?: string;
  };
}

export interface MCPToolPreview {
  name: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
}

export interface AgentMCPTestResponse {
  ok: boolean;
  tools?: MCPToolPreview[];
  error?: string;
}
```

Add:

```ts
export async function getAgentMCPConfig(id: string): Promise<AgentMCPConfigResponse> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}/mcp`);
  return res.json();
}

export async function saveAgentMCPConfig(
  id: string,
  payload: { mcpServers: Record<string, MCPServerConfig>; shareMcpConfig: boolean },
): Promise<AgentMCPConfigResponse & { error?: string }> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}/mcp`, {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  return res.json();
}

export async function testAgentMCP(id: string): Promise<AgentMCPTestResponse> {
  const res = await apiFetch(`/api/agents/${encodeURIComponent(id)}/mcp/test`, {
    method: "POST",
  });
  return res.json();
}
```

Extend `AgentDetail` and `AgentUpdatePayload` only with:

```ts
shareMcpConfig?: boolean;
```

- [ ] **Step 2: Create MCP page state model**

Create `web/src/app/agents/[id]/mcp/page.tsx` with:

```tsx
"use client";

import * as React from "react";
import { Cable, CheckCircle2, Plus, RefreshCw, ServerCog, Trash2, XCircle } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Switch } from "@/components/ui/switch";
import { Badge } from "@/components/ui/badge";
import { Textarea } from "@/components/ui/textarea";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { getAgentMCPConfig, saveAgentMCPConfig, testAgentMCP, type MCPServerConfig, type MCPToolPreview } from "@/lib/api";
import { useAgentIdFromURL } from "@/hooks/use-agent-id";

type DraftServer = MCPServerConfig & { name: string };

const emptyServer = (): DraftServer => ({
  name: "",
  type: "stdio",
  command: "",
  args: [],
  env: {},
  headers: {},
  transport: "streamable-http",
  enabled: true,
});
```

Use compact dashboard layout:

- Header with title `MCP` and gateway status badge.
- `Switch` for `shareMcpConfig`.
- Server list as a single table-like grid; each row has type select, enabled switch, edit/remove icon buttons.
- Editor panel for the selected server with fields:
  - `name`
  - `type`
  - `transport`
  - `url`
  - `command`
  - `args` as newline textarea, one arg per line
  - `env` as newline `KEY=VALUE`
  - `Authorization` header as a single masked text input
- Footer actions: save, test, refresh.
- Tools preview as small rows after test.

- [ ] **Step 3: Add deterministic form conversion helpers**

Inside the page file, add:

```tsx
function mapToText(map?: Record<string, string>) {
  return Object.entries(map || {})
    .map(([k, v]) => `${k}=${v}`)
    .join("\n");
}

function textToMap(text: string) {
  const out: Record<string, string> = {};
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const idx = line.indexOf("=");
    if (idx <= 0) continue;
    out[line.slice(0, idx).trim()] = line.slice(idx + 1);
  }
  return out;
}

function serversToDrafts(input: Record<string, MCPServerConfig>): DraftServer[] {
  return Object.entries(input || {}).map(([name, cfg]) => ({
    name,
    type: cfg.type,
    url: cfg.url,
    headers: cfg.headers || {},
    command: cfg.command,
    args: cfg.args || [],
    env: cfg.env || {},
    transport: cfg.transport || "streamable-http",
    enabled: cfg.enabled !== false,
  }));
}

function draftsToServers(drafts: DraftServer[]): Record<string, MCPServerConfig> {
  const out: Record<string, MCPServerConfig> = {};
  for (const draft of drafts) {
    const name = draft.name.trim();
    if (!name) continue;
    out[name] = {
      type: draft.type,
      enabled: draft.enabled !== false,
      transport: draft.transport || "streamable-http",
      url: draft.type === "http" ? draft.url?.trim() : undefined,
      headers: draft.type === "http" ? draft.headers || {} : undefined,
      command: draft.type === "stdio" ? draft.command?.trim() : undefined,
      args: draft.type === "stdio" ? draft.args || [] : undefined,
      env: draft.env || {},
    };
  }
  return out;
}
```

- [ ] **Step 4: Implement page behavior**

The component should:

```tsx
export default function AgentMCPPage() {
  const agentId = useAgentIdFromURL();
  const [loading, setLoading] = React.useState(true);
  const [saving, setSaving] = React.useState(false);
  const [testing, setTesting] = React.useState(false);
  const [shareMcpConfig, setShareMcpConfig] = React.useState(false);
  const [servers, setServers] = React.useState<DraftServer[]>([]);
  const [selected, setSelected] = React.useState(0);
  const [gatewayStatus, setGatewayStatus] = React.useState<string>("stopped");
  const [tools, setTools] = React.useState<MCPToolPreview[]>([]);
  const [message, setMessage] = React.useState<string>("");
}
```

Load with `getAgentMCPConfig`, save with `saveAgentMCPConfig`, then test with `testAgentMCP`. On API errors, show the backend error message in a muted destructive text row near the buttons. Do not show long instructional copy.

- [ ] **Step 5: Add settings tab**

In `web/src/components/agent-settings-dialog.tsx`:

1. Import `Cable` and `AgentMCPPage`.
2. Add `"mcp"` to `AgentSettingsTab`.
3. Add `{ id: "mcp", label: "MCP", icon: Cable }` after plugins.
4. Render `{tab === "mcp" && <AgentMCPPage />}`.

Viewer role should not see the MCP tab because only owners can edit/share agent MCP config.

- [ ] **Step 6: Add route extraction**

In `web/src/components/app-sidebar.tsx`, include `mcp` in the agent route regex:

```ts
/^\/agents\/([^/]+)\/(chat|customize|skills|models|sessions|channels|chats|scheduler|project|mcp)/
```

- [ ] **Step 7: Run frontend lint**

Run: `cd web; npm run lint`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add web/src/lib/api.ts web/src/app/agents/[id]/mcp/page.tsx web/src/components/agent-settings-dialog.tsx web/src/components/app-sidebar.tsx
git commit -m "feat: add agent mcp configuration ui"
```

---

### Task 8: Final Verification and Operator Docs

**Files:**
- Create: `docs/mcp-gateway.md`

- [ ] **Step 1: Add operator documentation**

Create `docs/mcp-gateway.md`:

```markdown
# MCP Gateway Runtime

BkCrab runs agent MCP servers through per-user lucky-aeon MCP Gateway containers.

## Environment

| Variable | Default |
| --- | --- |
| `BKCRAB_MCP_GATEWAY_ENABLED` | `true` |
| `BKCRAB_MCP_GATEWAY_IMAGE` | `ghcr.io/lucky-aeon/mcp-gateway:latest` |
| `BKCRAB_MCP_GATEWAY_RUNTIME_DIR` | `$BKCRAB_HOME/mcp-gateways` |
| `BKCRAB_MCP_GATEWAY_CONTAINER_PORT` | `8080` |
| `BKCRAB_MCP_GATEWAY_PROTOCOL` | `all` |
| `BKCRAB_MCP_GATEWAY_IDLE_TTL_SEC` | `1800` |

## Behavior

- Each user gets one gateway container when one of their agents has enabled MCP servers.
- Agent stdio MCP servers run inside that user's gateway container.
- Remote HTTP MCP servers are deployed into the gateway by URL.
- `Authorization: Bearer <token>` is mapped to the gateway's `MCP_REMOTE_AUTH_ACCESS_TOKEN` env value for upstream bearer auth.
- Other custom HTTP headers are rejected in V1 because the selected gateway does not expose generic downstream header configuration.
- Public agents share owner MCP only when `shareMcpConfig` is enabled.
```

- [ ] **Step 2: Run focused backend tests**

Run:

```bash
go test ./internal/config ./internal/mcp ./internal/mcp/runtime ./internal/store ./internal/agent ./internal/gateway ./internal/setup -count=1
```

Expected: PASS.

- [ ] **Step 3: Run full backend tests**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 4: Run frontend verification**

Run:

```bash
cd web
npm run lint
npm run build
```

Expected: both commands pass.

- [ ] **Step 5: Manual Docker smoke test**

With Docker running, create a test agent MCP config from the UI:

```json
{
  "mcpServers": {
    "time": {
      "type": "stdio",
      "command": "uvx",
      "args": ["mcp-server-time", "--local-timezone=Asia/Shanghai"],
      "transport": "streamable-http",
      "enabled": true
    }
  },
  "shareMcpConfig": false
}
```

Click save, then test. Expected:

- A container named `bkcrab-mcp-gateway-<hash>` exists.
- `/api/agents/{id}/mcp/test` returns `ok: true`.
- Tools preview includes a tool prefixed with `mcp_time_`.
- A chat with the agent exposes the same `mcp_time_` tool in the agent registry.

- [ ] **Step 6: Inspect git diff**

Run:

```bash
git diff --stat HEAD
git diff --check
```

Expected:

- Stat includes only MCP runtime/config/API/UI/docs files.
- `git diff --check` prints no whitespace errors.

- [ ] **Step 7: Commit docs and verification fixes**

```bash
git add docs/mcp-gateway.md
git commit -m "docs: document mcp gateway runtime"
```

If verification required small fixes, include those files in the same commit and keep the commit message:

```bash
git add docs/mcp-gateway.md internal web
git commit -m "docs: document mcp gateway runtime"
```

---

## Self-Review Results

- Spec coverage: the plan covers gateway-backed V1, per-user gateway containers, stdio and HTTP deployment, agent-level config, plaintext storage with masking, `shareMcpConfig`, public agent behavior, runtime lifecycle, test connection, tools preview, and frontend entry points.
- Placeholder scan: the plan avoids generic "handle later" language and unbounded test instructions. Remaining ellipsis-style matches are required Go variadic or slice-spread syntax.
- Type consistency: shared names are `shareMcpConfig`, `MCPServerConfig`, `MCPTransportSSE`, `MCPTransportStreamableHTTP`, `NewManagerForAgent`, `TestServers`, and `MCPGatewayRuntimeRecord` across tasks.
