# Gateway-backed MCP MVP Design

> Status: approved design, pending implementation
> Date: 2026-07-05
> Scope: replace the current agent-local MCP runtime path with a per-user MCP gateway runtime backed by lucky-aeon/mcp-gateway.

## 1. Background

bkcrab already has the beginning of MCP support:

- `internal/config/config.go` defines `mcpServers` on global config, agent file config, and resolved agent config.
- `internal/mcp/manager.go` can connect configured MCP servers, collect tool definitions, prefix tool names, and route calls.
- `internal/agent/loop.go` registers MCP tools into an agent registry when `ResolvedAgent.MCPServers` is present.

The current runtime shape is still agent-local:

```text
Agent instance
  -> direct MCP client
  -> http MCP server, or stdio MCP server launched by bkcrab
```

That shape has three product-level problems:

1. `stdio` MCP servers run as child processes of the bkcrab runtime environment.
2. Public agents loaded by multiple users can create multiple runtime agent instances, which can create duplicate stdio MCP server processes.
3. The frontend/API path for configuring `mcpServers` is not complete, so the runtime capability is not productized.

AgentX uses a different model: each user gets an MCP gateway container, and the application connects agents to that user's gateway endpoint. The gateway hosts or proxies MCP servers, while the application manages the gateway container lifecycle.

This design adopts that direction for bkcrab V1.

## 2. Goals

1. Use a per-user MCP gateway runtime as the default MCP execution path.
2. Deploy both `stdio` and `http` MCP server configs through the gateway.
3. Keep MCP configuration agent-scoped by storing it in `agents.config.mcpServers`.
4. Add a frontend agent MCP settings page with create, edit, delete, test, tools preview, gateway status, and `shareMcpConfig`.
5. Support both SSE and Streamable HTTP MCP transports from bkcrab to the gateway.
6. Prevent secret disclosure through API responses, UI display, and logs.
7. Add explicit public-agent MCP sharing through `shareMcpConfig`.
8. Keep direct stdio MCP only as a disabled-by-default development or legacy fallback.

## 3. Non-goals

- No MCP marketplace, catalog, install approval workflow, or community registry in V1.
- No Docker official MCP catalog/profile/secrets integration in V1.
- No per-tool permission matrix in V1.
- No encrypted secret vault in V1.
- No per-agent gateway runtime in V1.
- No default direct stdio MCP execution from the bkcrab main runtime.

## 4. Confirmed Decisions

### Runtime Isolation

V1 uses a per-user gateway runtime:

```text
User A -> Gateway A -> A's MCP servers
User B -> Gateway B -> B's MCP servers
```

The same user's agents share that user's gateway. Different users do not share a gateway.

### Gateway Implementation

V1 targets `lucky-aeon/mcp-gateway`.

bkcrab manages the gateway container. The gateway manages its internal MCP services and exposes MCP endpoints. This separation is important:

```text
bkcrab:
  create/start/stop/delete/health-check per-user gateway containers

lucky-aeon/mcp-gateway:
  deploy and expose stdio/http MCP services inside the gateway runtime
```

### MCP Server Routing

Both `stdio` and `http` MCP server configs go through the gateway in V1:

```text
agent mcpServers
  -> deploy to user's gateway
  -> bkcrab connects to gateway endpoint
```

Direct MCP clients remain available only behind an explicit development/legacy switch.

### Public Agent Sharing

`shareMcpConfig` is separate from `shareModelConfig`.

```text
shareMcpConfig=false:
  visitors do not inherit the owner MCP config

shareMcpConfig=true:
  visitors use the owner mcpServers config
  visitors use the owner per-user gateway runtime
```

The default is `false`.

### Configuration Scope

MCP server configuration is agent-scoped and stored in `agents.config`:

```json
{
  "mcpServers": {
    "github": {
      "enabled": true,
      "type": "stdio",
      "transport": "sse",
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": {
        "GITHUB_PERSONAL_ACCESS_TOKEN": "secret"
      }
    },
    "docs": {
      "enabled": true,
      "type": "http",
      "transport": "streamable_http",
      "url": "https://example.com/mcp",
      "headers": {
        "Authorization": "Bearer secret"
      }
    }
  },
  "shareMcpConfig": false
}
```

## 5. Architecture

The V1 runtime path:

```text
Frontend MCP settings
  -> setup API saves agents.config.mcpServers
  -> agent invalidation
  -> agent runtime resolves MCP owner user
  -> McpRuntimeManager ensures user gateway
  -> McpGatewayClient deploys MCP server configs
  -> transport clients connect to gateway endpoints
  -> tools register in the agent registry
  -> tool calls route back through the gateway
```

New backend responsibilities:

```text
internal/mcp/runtime
  Per-user gateway runtime creation, startup, stop, health, cleanup.

internal/mcp/gateway
  lucky-aeon gateway API client: deploy, health, endpoint building, list tools.

internal/mcp/transport
  SSE and Streamable HTTP MCP clients implementing the existing MCP client interface.

internal/setup/handlers_mcp.go
  Agent MCP config API, test connection API, gateway status API.
```

Existing code to preserve conceptually:

- Tool aggregation and call routing from `internal/mcp/manager.go`.
- Tool registration in `internal/agent/loop.go`.
- Agent config merge through `config.ResolveAgents`.

## 6. Data Model

### Agent Config

`config.AgentFileConfig` and `config.ResolvedAgent` already have `MCPServers`. V1 extends the server config with:

```text
enabled
transport
```

`transport` values:

```text
sse
streamable_http
```

If absent, the default is `sse`.

### shareMcpConfig

Add `shareMcpConfig` to the agent config JSON stored in `agents.config`. It is a boolean with default `false`.

### Gateway Runtime Table

Add a table for per-user gateway runtimes:

```sql
CREATE TABLE mcp_gateway_runtimes (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL,
  docker_container_id TEXT,
  image TEXT NOT NULL,
  internal_port INTEGER NOT NULL,
  external_port INTEGER,
  base_url TEXT,
  api_key TEXT NOT NULL,
  deployed_servers_json TEXT,
  last_accessed_at TIMESTAMP,
  error_message TEXT,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
```

`deployed_servers_json` stores deployment hashes:

```json
{
  "github": {
    "hash": "sha256:...",
    "lastDeployedAt": "2026-07-05T12:00:00Z"
  }
}
```

This lets bkcrab skip redundant deploy calls when a server config has not changed.

Backends must add equivalent migrations for SQLite, PostgreSQL, and MySQL.

## 7. API Design

### Read Agent MCP Config

```text
GET /api/agents/{id}/mcp
```

Response:

```json
{
  "mcpServers": {},
  "shareMcpConfig": false,
  "gateway": {
    "status": "running",
    "baseUrl": "http://127.0.0.1:30123",
    "lastAccessedAt": "2026-07-05T12:00:00Z"
  }
}
```

Secrets are masked in `env` and `headers`.

### Save Agent MCP Config

```text
PUT /api/agents/{id}/mcp
```

Request:

```json
{
  "mcpServers": {},
  "shareMcpConfig": true
}
```

The save operation:

1. Checks agent ownership or super admin permission.
2. Merges masked secret values with the existing config.
3. Saves to `agents.config`.
4. Invalidates runtime user spaces holding that agent.

### Test MCP Connection

```text
POST /api/agents/{id}/mcp/test
```

Request:

```json
{
  "server": "github"
}
```

If `server` is omitted, all enabled servers are tested.

Response:

```json
{
  "ok": true,
  "servers": [
    {
      "name": "github",
      "status": "running",
      "transport": "sse",
      "tools": [
        {
          "name": "search_repositories",
          "description": "Search GitHub repositories"
        }
      ],
      "error": ""
    }
  ]
}
```

### Current User Gateway Status

```text
GET /api/mcp/gateway/status
POST /api/mcp/gateway/start
POST /api/mcp/gateway/stop
```

These operate on the authenticated user's gateway runtime.

## 8. Runtime Semantics

### MCP Owner Resolution

For a private agent:

```text
mcpOwnerUserID = agent owner user_id
```

For a public agent used by a visitor:

```text
shareMcpConfig=false:
  no owner MCP config is loaded

shareMcpConfig=true:
  mcpOwnerUserID = agent owner user_id
```

This means the owner explicitly decides whether their MCP environment powers public-agent usage.

### Gateway Ensure

`McpRuntimeManager.Ensure(userID)`:

1. Loads or creates the runtime row.
2. Creates the Docker container if no container exists.
3. Starts the container if stopped.
4. Checks Docker and gateway health if running.
5. Updates `last_accessed_at`.
6. Returns the runtime endpoint and API key.

### Deploy

For each enabled server:

1. Normalize the config and redact secrets for logs.
2. Compute a stable hash from the unmasked config.
3. If the stored hash matches, skip deploy.
4. Otherwise call lucky-aeon gateway deploy.
5. Store the new hash on success.

Disabled servers are not deployed and their tools are not registered.

### Register Tools

Each enabled server becomes a gateway-backed MCP client:

```text
server transport=sse
  -> SSEClient(endpoint)

server transport=streamable_http
  -> StreamableHTTPClient(endpoint)
```

Tool names keep the current namespace pattern:

```text
mcp_<serverName>_<toolName>
```

If one server fails to deploy or list tools, that server is skipped and logged. Other MCP servers still register.

## 9. Gateway Client

`McpGatewayClient` wraps lucky-aeon gateway behavior.

Default endpoint conventions:

```text
deploy:
  POST /deploy

SSE MCP endpoint:
  /{serverName}/sse?api_key={apiKey}

Streamable HTTP endpoint:
  /{serverName}/mcp?api_key={apiKey}
```

These paths are adapter configuration, not frontend-facing API. The implementation will centralize them so a future gateway version can change route patterns without touching agent runtime code.

Deploy payloads are generated from `MCPServerConfig`:

```text
stdio:
  name, command, args, env

http:
  name, url, headers
```

The gateway API key is generated by bkcrab per runtime and is not exposed to the frontend.

## 10. MCP Transports

Both new transports implement the existing client interface:

```go
type Client interface {
    Connect() error
    ListTools() ([]ToolDef, error)
    CallTool(name string, args json.RawMessage) (string, error)
    Close() error
}
```

### SSE Transport

The SSE client:

1. Opens the SSE endpoint.
2. Reads the gateway message endpoint from the MCP SSE handshake.
3. Sends JSON-RPC requests to the message endpoint.
4. Correlates JSON-RPC responses by id.
5. Supports `initialize`, `tools/list`, and `tools/call`.
6. Closes the SSE connection on `Close`.

### Streamable HTTP Transport

The Streamable HTTP client:

1. Sends JSON-RPC requests to the gateway streamable endpoint.
2. Tracks the MCP session id when provided.
3. Supports `initialize`, `tools/list`, and `tools/call`.
4. Terminates the session on `Close` when supported by the endpoint.

Shared JSON-RPC request/response parsing should be reused by both clients.

## 11. Frontend

Add an agent MCP page:

```text
/agents/{id}/mcp
```

The page contains:

- Gateway status card.
- Start, stop, and test actions.
- `shareMcpConfig` toggle.
- MCP server list.
- Add/edit/delete server controls.
- Tools preview after testing.

Server fields:

```text
common:
  name
  enabled
  type
  transport

stdio:
  command
  args
  env

http:
  url
  headers
```

Secret behavior:

```text
existing secret:
  display ********

unchanged secret:
  submit ********

changed secret:
  submit new value
```

The frontend does not show gateway API keys.

## 12. Security

V1 security requirements:

1. `stdio` MCP servers do not run in the bkcrab main runtime by default.
2. `shareMcpConfig` defaults to false.
3. When `shareMcpConfig=true`, the UI must warn that public-agent visitors can trigger owner MCP tools.
4. API responses never include raw `env` or `headers` secret values.
5. Logs must not print env/header values, gateway API keys, or deploy payload secrets.
6. Non-owners cannot edit agent MCP config.
7. Super admins can inspect status but do not receive raw secrets.
8. Direct stdio requires an explicit development/legacy setting.

## 13. Operations

Runtime statuses:

```text
creating
running
stopped
error
deleted
```

V1 lifecycle operations:

```text
ensure:
  create, start, health-check, recover

shutdown:
  stop running gateway containers

cleanup:
  stop idle runtimes
  delete abandoned runtimes
```

Default cleanup policy:

```text
idle for 1 day:
  stop container

idle for 5 days:
  delete container and mark runtime deleted
```

The cleanup job may ship after the manual start/stop/status path, but the data model and runtime manager must be designed for it from the start.

## 14. Testing Strategy

Backend tests:

- Save and read `mcpServers`.
- Save and read `shareMcpConfig`.
- Mask secrets in API responses.
- Preserve old secrets when receiving masked values.
- Reject non-owner MCP edits.
- Resolve public-agent MCP owner correctly for `shareMcpConfig=false`.
- Resolve public-agent MCP owner correctly for `shareMcpConfig=true`.
- Ensure gateway runtime creates, reuses, starts, and records access time.
- Skip deploy when config hash is unchanged.
- Redeploy when config hash changes.
- Keep registering other servers when one server fails.
- SSE transport supports initialize, list tools, and call tool against a fake gateway.
- Streamable HTTP transport supports initialize, list tools, and call tool against a fake gateway.

Frontend tests:

- MCP page renders loaded config.
- Add, edit, disable, and delete server.
- Masked secret fields preserve values.
- Test connection displays server status and tools.
- `shareMcpConfig` toggle saves and reloads.

Integration tests:

- Fake gateway deploy/list/call flow.
- Public agent with `shareMcpConfig=false` does not load owner tools for visitors.
- Public agent with `shareMcpConfig=true` loads owner gateway tools for visitors.

## 15. Rollout

1. Add data model and backend API without changing default runtime behavior.
2. Add gateway runtime manager and fake-gateway tests.
3. Add SSE and Streamable HTTP transports.
4. Switch MCP runtime path to gateway-backed for configured agents.
5. Add frontend MCP page.
6. Disable direct stdio by default and keep it behind a development/legacy setting.

This rollout keeps existing non-MCP agents unaffected while the gateway-backed MCP path is added.
