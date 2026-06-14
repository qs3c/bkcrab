# User Extension Model Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan.

**Goal:** 将 BkClaw 的 MCP 与插件从系统级启动配置改为用户级安装配置，并提供仅供显式开发使用的 `local` Extension Runtime。

**Architecture:** BkClaw 数据库保存扩展期望状态，`internal/extensions.Service` 按当前发言用户和 Agent 计算有效安装集合。Agent 在每轮执行开始时刷新 MCP/插件能力代理，代理调用时再次校验安装版本和权限。系统插件目录只作为目录源，不再在 Gateway 启动时启动进程；本地进程运行只允许在 `BKCLAW_EXTENSION_RUNTIME=local` 时启用。

**Tech Stack:** Go 1.25, Gin, SQLite/PostgreSQL/MySQL, React/Next.js 16, TypeScript, Vitest/Go testing

---

## Repository Scope

- Primary repository: `E:\fromGithub\bkclaw`
- Approved design: `docs/superpowers/specs/2026-06-14-user-extension-runtime-design.md`
- This plan intentionally does not add Docker orchestration. Docker isolation is implemented by the second-stage Gateway plan.

### Task 1: Add User-Owned Extension Persistence

**Files:**
- Modify: `internal/store/store.go`
- Modify: `internal/store/database.go`
- Modify: `internal/store/database_mysql.go`
- Create: `internal/store/extensions.go`
- Create: `internal/store/extensions_test.go`
- Create: `internal/store/extensions_migration_test.go`

- [ ] **Step 1: Write failing migration tests**

Add table-presence and idempotency tests for SQLite. Assert the following constraints:

```sql
UNIQUE (user_id, extension_id)
UNIQUE (user_id, agent_id, installation_id)
PRIMARY KEY (user_id)
```

The tests must call `Migrate` twice and verify that both calls succeed.

Run:

```powershell
go test ./internal/store -run 'TestExtension(Migration|MigrateIdempotent)' -count=1
```

Expected: FAIL because the extension tables do not exist.

- [ ] **Step 2: Define store records and interface methods**

Add these records to `internal/store/store.go`:

```go
type ExtensionInstallationRecord struct {
	ID             string
	UserID         string
	ExtensionID    string
	CatalogID      string
	Type           string
	Name           string
	Version        string
	PackageDigest  string
	ManifestJSON   string
	ConfigJSON     string
	SecretsJSON    string
	Enabled        bool
	AlwaysOn       bool
	NetworkPolicy  string
	Revision       int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ExtensionAgentOverrideRecord struct {
	ID             string
	UserID         string
	AgentID        string
	InstallationID string
	Enabled        bool
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ExtensionRuntimeLeaseRecord struct {
	UserID            string
	HolderID          string
	ExpiresAt         time.Time
	LastEventEpoch    string
	LastEventSequence uint64
	UpdatedAt         time.Time
}
```

Extend `Store` with scoped CRUD and lease methods:

```go
ListExtensionInstallations(context.Context, string) ([]ExtensionInstallationRecord, error)
GetExtensionInstallation(context.Context, string, string) (*ExtensionInstallationRecord, error)
GetExtensionInstallationByExtensionID(context.Context, string, string) (*ExtensionInstallationRecord, error)
SaveExtensionInstallation(context.Context, ExtensionInstallationRecord) error
DeleteExtensionInstallation(context.Context, string, string) error
ListExtensionAgentOverrides(context.Context, string, string) ([]ExtensionAgentOverrideRecord, error)
GetExtensionAgentOverride(context.Context, string, string, string) (*ExtensionAgentOverrideRecord, error)
SaveExtensionAgentOverride(context.Context, ExtensionAgentOverrideRecord) error
DeleteExtensionAgentOverride(context.Context, string, string, string) error
AcquireExtensionRuntimeLease(context.Context, string, string, time.Time, time.Time) (bool, error)
RenewExtensionRuntimeLease(context.Context, string, string, time.Time, time.Time) (bool, error)
ReleaseExtensionRuntimeLease(context.Context, string, string) error
GetExtensionRuntimeLease(context.Context, string) (*ExtensionRuntimeLeaseRecord, error)
AdvanceExtensionRuntimeCursor(context.Context, string, string, string, uint64) (bool, error)
```

- [ ] **Step 3: Add all three database migrations**

Add `extension_installations`, `extension_agent_overrides`, and `extension_runtime_leases` to SQLite/PostgreSQL `migrationSQL()` and MySQL `mysqlMigrationSQL()`.

Use `TEXT` identifiers for SQLite/PostgreSQL and bounded `VARCHAR(191)` indexed identifiers for MySQL. Store booleans using each backend's existing conventions. Add indexes for:

```text
extension_installations(user_id, enabled)
extension_agent_overrides(user_id, agent_id)
extension_runtime_leases(expires_at)
```

`extension_runtime_leases` must include `last_event_epoch` and `last_event_sequence` so the elected event consumer can resume after BkClaw failover without adding a fourth extension-state table.

- [ ] **Step 4: Implement user-scoped database methods**

Create `internal/store/extensions.go`. Every read, update, and delete must include `user_id` in the query predicate. `SaveExtensionInstallation` must increment `revision` on update and preserve `created_at`.

Lease acquisition must be one atomic statement or transaction with this rule:

```text
acquire succeeds when the row is absent, expired, or already held by holder_id
```

- [ ] **Step 5: Add ownership and lease behavior tests**

Cover:

```text
one user cannot read/update/delete another user's installation
same extension_id can be installed by different users
revision increments after update
agent override defaults to absent
expired lease can be acquired by another holder
unexpired lease cannot be stolen
only the holder can renew or release a lease
only the holder can advance an event cursor
event sequence never moves backward within one epoch
```

Run:

```powershell
go test ./internal/store -run 'TestExtension' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit persistence**

```powershell
git add internal/store
git commit -m "feat: add user extension persistence"
```

### Task 2: Stop Child Processes From Inheriting BkClaw Secrets

**Files:**
- Modify: `internal/mcp/stdio.go`
- Create: `internal/mcp/stdio_test.go`
- Modify: `internal/plugin/process.go`
- Create: `internal/plugin/process_test.go`
- Create: `internal/extensions/processenv/processenv.go`
- Create: `internal/extensions/processenv/processenv_test.go`

- [ ] **Step 1: Write failing environment-isolation tests**

Set a sentinel in the test process:

```go
t.Setenv("BKCLAW_TEST_PARENT_SECRET", "must-not-leak")
```

Launch a fixture child through both MCP stdio and plugin process paths. Assert:

```text
BKCLAW_TEST_PARENT_SECRET is absent
explicitly supplied extension variables are present
PATH, HOME, and temporary directory are present
database and object-storage variables are absent
```

Run:

```powershell
go test ./internal/mcp ./internal/plugin ./internal/extensions/processenv -run 'Test.*Environment' -count=1
```

Expected: FAIL because current child processes inherit the parent environment.

- [ ] **Step 2: Add one minimal-environment builder**

Implement:

```go
type Options struct {
	Home     string
	TempDir  string
	Path     string
	Explicit map[string]string
}

func Build(opts Options) []string
```

The result must include only:

```text
PATH
HOME or USERPROFILE as appropriate
TMPDIR/TEMP/TMP as appropriate
LANG/LC_ALL when explicitly configured by BkClaw
the extension's explicit environment map
```

Reject keys containing `=` or NUL and sort output for deterministic tests.

- [ ] **Step 3: Apply the builder to MCP and plugin processes**

Change constructors so callers pass explicit environment maps. Do not call `os.Environ()` and do not leave `cmd.Env` nil.

Replace command-string parsing for plugins with a structured command and argument list:

```go
type ProcessSpec struct {
	Command    string
	Args       []string
	WorkingDir string
	Env        map[string]string
}
```

Keep a compatibility parser only at legacy catalog import boundaries, not in process execution.

- [ ] **Step 4: Verify isolation**

Run:

```powershell
go test ./internal/mcp ./internal/plugin ./internal/extensions/processenv -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit environment hardening**

```powershell
git add internal/mcp internal/plugin internal/extensions/processenv
git commit -m "fix: isolate extension process environments"
```

### Task 3: Build the Extension Domain, Catalog, and Artifact Model

**Files:**
- Create: `internal/extensions/model.go`
- Create: `internal/extensions/catalog.go`
- Create: `internal/extensions/catalog_test.go`
- Create: `internal/extensions/artifact.go`
- Create: `internal/extensions/artifact_test.go`
- Modify: `internal/config/env.go`
- Create: `internal/config/env_test.go`

- [ ] **Step 1: Write manifest normalization tests**

Test the supported types:

```go
const (
	TypeMCPStdio      = "mcp-stdio"
	TypeMCPHTTP       = "mcp-http"
	TypeBkClawPlugin  = "bkclaw-plugin"
	TypeOpenClawPlugin = "openclaw-plugin"
)
```

Validate:

```text
id uses [a-z0-9][a-z0-9._-]{0,127}
command is required for local process types
HTTP URL is required for mcp-http
capabilities contain only tool/provider/hook/channel
channel capability defaults alwaysOn to true
networkPolicy is none or bridge
workingDir cannot escape the extracted artifact root
```

Run:

```powershell
go test ./internal/extensions -run 'TestCatalog|TestManifest' -count=1
```

Expected: FAIL because the package does not exist.

- [ ] **Step 2: Define normalized deployment and runtime contracts**

Add:

```go
type Manifest struct {
	ID             string            `json:"id"`
	Type           string            `json:"type"`
	Name           string            `json:"name"`
	Version        string            `json:"version"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	URL            string            `json:"url,omitempty"`
	WorkingDir     string            `json:"workingDir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Capabilities   []string          `json:"capabilities"`
	RestartPolicy  string            `json:"restartPolicy,omitempty"`
	NetworkPolicy  string            `json:"networkPolicy,omitempty"`
	PersistentData bool              `json:"persistentData,omitempty"`
	AlwaysOn       bool              `json:"alwaysOn,omitempty"`
}

type Installation struct {
	Record   store.ExtensionInstallationRecord
	Manifest Manifest
	Config   map[string]any
	Secrets  map[string]string
}
```

- [ ] **Step 3: Implement catalog discovery**

Scan:

```text
$BKCLAW_HOME/extensions/catalog
$BKCLAW_HOME/plugins
```

Treat both as read-only catalogs. Normalize legacy plugin manifests into `Manifest`; discovery must never start a process.

- [ ] **Step 4: Implement deterministic artifact packaging**

For directory-backed plugins, create a deterministic `tar.gz` archive in memory or a temporary BkClaw-owned path and calculate SHA-256. Sort entries, zero gzip/tar timestamps and ownership metadata, reject symlinks escaping the package root, and normalize path separators to `/`.

Add tests proving identical input produces identical digest and traversal entries are rejected.

- [ ] **Step 5: Add runtime configuration**

Add:

```go
type ExtensionRuntimeConfig struct {
	Mode          string
	LocalDataDir  string
	MasterKey     string
}
```

Parse:

```text
BKCLAW_EXTENSION_RUNTIME=local|docker
BKCLAW_EXTENSION_LOCAL_DATA_DIR
BKCLAW_EXTENSION_MASTER_KEY
```

Default to `docker`; fail startup on any other value. Require a valid master key before storing extension secrets. Include the key in boot-secret scrubbing after service construction.

- [ ] **Step 6: Run tests and commit**

```powershell
go test ./internal/extensions ./internal/config -count=1
git add internal/extensions internal/config
git commit -m "feat: add extension catalog model"
```

### Task 4: Implement the Runtime Interface and Explicit Local Backend

**Files:**
- Create: `internal/extensions/runtime.go`
- Create: `internal/extensions/local_runtime.go`
- Create: `internal/extensions/local_runtime_test.go`
- Modify: `internal/plugin/process.go`
- Modify: `internal/mcp/manager.go`

- [ ] **Step 1: Write runtime contract tests**

Use fixture MCP and plugin processes to assert lazy startup, shared reuse for the same user, RPC dispatch, event delivery, stop, and status.

Run:

```powershell
go test ./internal/extensions -run 'TestLocalRuntime' -count=1
```

Expected: FAIL.

- [ ] **Step 2: Define the backend interface**

```go
type Runtime interface {
	Reconcile(context.Context, string, []Installation) error
	ListTools(context.Context, string, Installation) ([]ToolDefinition, error)
	CallTool(context.Context, string, Installation, string, json.RawMessage) (json.RawMessage, error)
	ListProviders(context.Context, string, Installation) ([]ProviderDefinition, error)
	CallProvider(context.Context, string, Installation, string, json.RawMessage) (json.RawMessage, error)
	FireHook(context.Context, string, Installation, string, json.RawMessage) (json.RawMessage, error)
	SendChannel(context.Context, string, Installation, json.RawMessage) error
	Events(context.Context, string) (<-chan Event, error)
	Status(context.Context, string, Installation) (RuntimeStatus, error)
	StopUser(context.Context, string) error
}
```

Every method receives `userID`; no runtime method may infer a user from the installation alone.

- [ ] **Step 3: Implement local process supervision**

Key running processes by `(userID, installationID, revision)`. Start only on the first capability call, except enabled `alwaysOn` installations which `Reconcile` starts. Stop the old revision before replacing it.

Use the hardened process environment from Task 2. Keep stdout protocol parsing separate from stderr logging. Bound pending RPC calls and enforce request timeouts.

- [ ] **Step 4: Adapt existing MCP and plugin primitives**

Reuse protocol/client code from `internal/mcp` and `internal/plugin`, but move lifecycle ownership into `LocalRuntime`. The old managers must not auto-start configured processes.

- [ ] **Step 5: Verify and commit**

```powershell
go test ./internal/extensions ./internal/mcp ./internal/plugin -count=1
git add internal/extensions internal/mcp internal/plugin
git commit -m "feat: add local extension runtime"
```

### Task 5: Add ExtensionService, Effective Installations, and Secret Encryption

**Files:**
- Create: `internal/extensions/service.go`
- Create: `internal/extensions/service_test.go`
- Create: `internal/extensions/crypto.go`
- Create: `internal/extensions/crypto_test.go`
- Create: `internal/extensions/http_mcp.go`
- Create: `internal/extensions/http_mcp_test.go`

- [ ] **Step 1: Write service behavior tests**

Cover:

```text
installation is visible only to its owner
all of the user's accessible agents enable an installation by default
an explicit false override disables it for one agent
public/shared agent execution uses the current chatter user's installations
disabled installations never reach runtime
capability cache key includes installation ID and revision
runtime calls re-read installation ownership and revision
secrets are encrypted at rest and decrypted only for runtime deployment
user-owned HTTP MCP uses its installation URL, headers, and secrets directly
HTTP MCP never resolves header values from BkClaw process environment
```

Run:

```powershell
go test ./internal/extensions -run 'TestService|TestSecret' -count=1
```

Expected: FAIL.

- [ ] **Step 2: Implement authenticated desired-state operations**

Expose:

```go
Install(context.Context, auth.Identity, InstallRequest) (*Installation, error)
Update(context.Context, auth.Identity, string, UpdateRequest) (*Installation, error)
Uninstall(context.Context, auth.Identity, string) error
List(context.Context, auth.Identity) ([]InstallationView, error)
SetAgentEnabled(context.Context, auth.Identity, string, string, bool) error
Effective(context.Context, auth.Identity, string) ([]Installation, error)
```

Call `auth.CanAccessAgent` before writing an override. Never accept `userID` from an HTTP request body.

- [ ] **Step 3: Encrypt secrets**

Use AES-256-GCM with a versioned envelope:

```json
{"v":1,"nonce":"base64","ciphertext":"base64"}
```

Derive no key from user IDs. Load the 32-byte master key from `BKCLAW_EXTENSION_MASTER_KEY`, support base64 encoding, and bind `userID + installationID` as associated data.

- [ ] **Step 4: Add capability caching and invalidation**

Cache tool/provider descriptions by:

```go
type capabilityKey struct {
	InstallationID string
	Revision       int64
}
```

Invalidate user cache after install/update/uninstall and agent cache after override changes.

- [ ] **Step 5: Keep HTTP MCP direct but user-owned**

Implement a direct HTTP MCP adapter selected only for `type=mcp-http`. Build headers from the decrypted installation secret/config maps and fixed protocol headers. Do not pass HTTP MCP through `LocalRuntime` or resolve `$ENV_VAR` placeholders from the BkClaw environment.

- [ ] **Step 6: Verify and commit**

```powershell
go test ./internal/extensions -count=1
git add internal/extensions
git commit -m "feat: add user extension service"
```

### Task 6: Replace Eager Agent MCP and System Plugin Registration

**Files:**
- Modify: `internal/agent/tools/registry.go`
- Create: `internal/agent/tools/registry_extension_test.go`
- Modify: `internal/agent/loop.go`
- Create: `internal/agent/loop_extension_test.go`
- Modify: `internal/agent/manager.go`
- Modify: `internal/gateway/userspace.go`
- Modify: `internal/gateway/userspace_test.go`
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/gateway/reload.go`

- [ ] **Step 1: Write failing dynamic-registration tests**

Assert:

```text
built-in tools remain when MCP/plugin tools refresh
removed installations disappear on the next turn
tool name collisions are rejected deterministically
two users chatting with the same public agent see different extension tools
tool execution after uninstall returns a clear unavailable error
Gateway startup does not call plugin StartAll
```

Run:

```powershell
go test ./internal/agent/... ./internal/gateway/... -run 'Test.*Extension|Test.*DynamicTool|Test.*PublicAgent' -count=1
```

Expected: FAIL.

- [ ] **Step 2: Add source replacement to the tool registry**

Implement a locked operation:

```go
func (r *Registry) ReplaceSource(source ToolSource, tools []Tool) error
```

Build a replacement map first, validate duplicate names against built-ins and other sources, then swap only entries owned by `source`.

- [ ] **Step 3: Refresh effective extensions at each turn boundary**

Add a narrow resolver to Agent manager options:

```go
type ExtensionResolver interface {
	Tools(context.Context, auth.Identity, string) ([]tools.Tool, error)
	Providers(context.Context, auth.Identity, string) ([]extensions.ProviderDefinition, error)
	FireHooks(context.Context, auth.Identity, string, string, json.RawMessage) error
}
```

At the beginning of a bound turn, use the current chatter identity and current agent ID to refresh `SourceMCP` and `SourcePlugin`. Tool closures must call `ExtensionService` by installation ID and expected revision rather than retaining a process pointer.

- [ ] **Step 4: Route hooks, providers, and channels through ExtensionService**

Remove the borrowed process-wide `PluginMgr` from `UserSpace`. Register dynamic hook/provider/channel adapters that resolve the current user's effective installations.

Inbound channel events must carry the runtime-authenticated user ID. Resolve the target agent/session through existing authorization paths before accepting `message.inbound` or `chat.send`.

- [ ] **Step 5: Remove system startup behavior**

In `internal/gateway/gateway.go`, remove:

```text
pluginMgr.StartAll
system-wide channel registration
system-wide provider registration
system-wide plugin StopAll ownership
eager agent MCP manager construction from global config
```

Keep catalog discovery for installation UI and migration only.

- [ ] **Step 6: Verify and commit**

```powershell
go test ./internal/agent/... ./internal/gateway/... -count=1
git add internal/agent internal/gateway
git commit -m "refactor: resolve extensions per user"
```

### Task 7: Add User Extension APIs and Legacy Super-Admin Migration

**Files:**
- Create: `internal/setup/handlers_extensions.go`
- Create: `internal/setup/handlers_extensions_test.go`
- Modify: `internal/setup/router.go`
- Modify: `internal/setup/server.go`
- Modify: `internal/setup/services.go`
- Modify: `internal/setup/handlers_plugins.go`
- Create: `internal/extensions/migrate_legacy.go`
- Create: `internal/extensions/migrate_legacy_test.go`

- [ ] **Step 1: Write API authorization tests**

Cover:

```text
GET /api/extensions/catalog
GET /api/extensions
POST /api/extensions
PATCH /api/extensions/:id
DELETE /api/extensions/:id
PUT /api/agents/:agentID/extensions/:installationID
GET /api/extensions/:id/status
```

Assert unauthenticated requests fail, user IDs in bodies are ignored/rejected, and cross-user installation IDs return not found.

- [ ] **Step 2: Register authenticated routes**

Handlers must derive identity with `authIdentity(c)` and pass it to `ExtensionService`. Return sanitized views with secret key names and `configured: true|false`, never secret values.

Use optimistic revision checks on PATCH:

```json
{"revision":3,"enabled":true,"config":{},"secrets":{}}
```

Return HTTP 409 when the stored revision changed.

- [ ] **Step 3: Implement one-time legacy migration**

After database migration and before normal reconciliation:

```text
find the super_admin identity
read legacy globally enabled stdio MCP, HTTP MCP, and plugin definitions
create equivalent installations owned only by super_admin
convert each legacy agent plugins.enabled entry into a super_admin extension_agent_overrides row
preserve explicit true and false entries while the new default remains enabled
record a versioned config marker after successful transaction
never copy secrets to other users
never auto-enable migrated definitions for other users
```

Make the migration idempotent and test interrupted retry behavior.

- [ ] **Step 4: Deprecate old system mutation routes**

Remove or return HTTP 410 for system-level `PUT /api/plugins/:id`. Keep a catalog-compatible read route only if needed by one frontend release, and mark it deprecated in the response.

- [ ] **Step 5: Verify and commit**

```powershell
go test ./internal/setup ./internal/extensions -count=1
git add internal/setup internal/extensions
git commit -m "feat: add user extension management api"
```

### Task 8: Convert the Web UI to User Extensions

**Files:**
- Modify: `web/package.json`
- Modify: `web/pnpm-lock.yaml`
- Create: `web/vitest.config.ts`
- Create: `web/src/test/setup.ts`
- Modify: `web/src/lib/api.ts`
- Modify: `web/src/app/plugins/page.tsx`
- Modify: `web/src/app/agents/[id]/plugins/page.tsx`
- Modify: `web/src/app/agents/[id]/context/page.tsx`
- Modify: `web/src/components/app-sidebar.tsx`
- Create: `web/src/app/plugins/page.test.tsx`
- Create: `web/src/app/agents/[id]/plugins/page.test.tsx`

- [ ] **Step 1: Add the repository's first UI test harness**

Add these development dependencies with pnpm:

```powershell
pnpm --dir web add -D vitest jsdom @testing-library/react @testing-library/jest-dom @testing-library/user-event
```

Add:

```json
"test": "vitest"
```

Configure `vitest.config.ts` with the existing TypeScript path alias, `jsdom`, and `src/test/setup.ts`.

- [ ] **Step 2: Add failing UI tests**

Test:

```text
catalog entries can be installed for the signed-in user
installed extensions can be enabled, disabled, configured, and removed
secret fields never render stored values
agent page shows extensions enabled by default
turning an agent switch off creates an explicit disabled override
runtime status distinguishes desired enabled state from actual process state
```

Run:

```powershell
pnpm --dir web test -- --run
```

Expected: FAIL until API types and pages are updated.

- [ ] **Step 3: Replace system plugin API types**

Add typed clients for catalog, installations, updates, agent overrides, and status. Remove UI dependence on `PluginInfo.enabled` as a global flag.

- [ ] **Step 4: Rebuild the extensions page**

Keep route compatibility at `/plugins`, but label the page “Extensions”. Separate catalog entries from installed entries. Do not add arbitrary upload in this phase.

Show:

```text
type and capabilities
version and package digest
enabled and always-on desired state
network policy
configuration and secret-key forms
local runtime status
```

- [ ] **Step 5: Rebuild agent extension overrides**

List the current user's installed extensions. A missing override renders enabled; persist only explicit disabled/enabled overrides through the agent endpoint.

- [ ] **Step 6: Update explanatory copy and navigation**

Remove text that says plugins run directly from `~/.bkclaw/plugins`. Explain that directories are catalogs and installed extensions belong to the current user.

- [ ] **Step 7: Verify and commit**

```powershell
pnpm --dir web lint
pnpm --dir web test -- --run
pnpm --dir web build
git add web
git commit -m "feat: add user extension management ui"
```

### Task 9: Wire Startup, Compatibility, and End-to-End Local Tests

**Files:**
- Modify: `internal/gateway/gateway.go`
- Modify: `internal/setup/server.go`
- Modify: `internal/config/env.go`
- Create: `internal/gateway/extensions_integration_test.go`
- Create: `docs/extension-runtime.md`

- [ ] **Step 1: Add startup-mode tests**

Assert:

```text
local mode constructs LocalRuntime
docker mode fails with a clear not-yet-wired error until stage two is applied
invalid mode fails closed
missing master key fails before serving APIs
system plugin and stdio MCP processes are absent after Gateway startup
HTTP MCP still uses direct authenticated user installation configuration
```

- [ ] **Step 2: Wire service construction**

Construct:

```text
catalog
secret cipher
selected runtime
ExtensionService
setup handlers
agent extension resolver
event consumer
```

Scrub `BKCLAW_EXTENSION_MASTER_KEY` after constructing the cipher. Close runtime and event consumers during Gateway shutdown.

- [ ] **Step 3: Add a local end-to-end test**

Use two users and one public agent:

```text
user A installs fixture MCP
user B installs fixture plugin
each user's turn exposes only that user's tools
agent-specific disable removes the tool for the selected agent
plugin event cannot claim the other user's identity
uninstall removes the tool on the next turn
```

- [ ] **Step 4: Document the transition**

Document:

```text
default docker mode requires stage-two Gateway integration
local mode is for trusted development only
legacy global definitions migrate only to super_admin
HTTP MCP remains direct but user-owned
system catalog directories do not execute code
```

- [ ] **Step 5: Run the complete phase-one verification**

```powershell
go test ./...
pnpm --dir web lint
pnpm --dir web test -- --run
pnpm --dir web build
git diff --check
```

Expected: all commands PASS.

- [ ] **Step 6: Commit phase one**

```powershell
git add internal web docs/extension-runtime.md
git commit -m "feat: make extensions user scoped"
```

## Phase-One Completion Criteria

- No stdio MCP or plugin starts automatically at BkClaw system Gateway startup.
- All extension installation, configuration, secret, enabled state, and agent overrides are user-owned.
- A public/shared agent resolves extensions from the current chatter user.
- Explicit `local` mode works without inheriting BkClaw credentials.
- Existing built-in tools, session Sandbox, and direct HTTP MCP behavior remain functional.
- Docker mode fails closed until the isolated-runtime plan is applied; it never silently falls back to local execution.
