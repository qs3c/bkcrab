package setup

import (
	"time"

	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/api"
	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/taskqueue"
	"github.com/qs3c/bkclaw/internal/usage"
	"github.com/qs3c/bkclaw/internal/users"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// Per-domain handler structs. Each holds ONLY the concrete instances its
// own handlers touch, plus pointers to the shared services it relies on.
// There is no shared god-object: a handler literally cannot reach a
// dependency it wasn't given. The composition root (Server.engine) wires
// each one explicitly.

// SessionHandler: bootstrap, login/session, the caller's own account,
// registration, provider connectivity tests, and per-user config.
type SessionHandler struct {
	accounts     *users.Accounts
	authResolver *auth.Resolver
	dataStore    store.Store
	userResolver api.UserResolver
	port         int
	startedAt    time.Time
	guard        *agentGuard
	cfg          *configRepo
}

// ChatHandler: the web chat surface — turns, steering, streaming, the SSE
// subscription, history, and session management.
type ChatHandler struct {
	dataStore  store.Store
	webChan    *channels.WebChannel
	chatEvents *agent.EventHub
	guard      *agentGuard
	ws         *workspaceRepo
}

// AgentsHandler: agent CRUD, per-agent config, registered tools.
type AgentsHandler struct {
	dataStore store.Store
	accounts  *users.Accounts
	guard     *agentGuard
	cfg       *configRepo
}

// AgentFilesHandler: per-agent workspace files and system files.
type AgentFilesHandler struct {
	dataStore      store.Store
	workspaceStore workspace.Store
	guard          *agentGuard
	ws             *workspaceRepo
}

// AgentChannelsHandler: per-agent IM bot bindings + inbound webhooks.
type AgentChannelsHandler struct {
	dataStore    store.Store
	userResolver api.UserResolver
	guard        *agentGuard
	chans        *channelRepo
}

// ProjectsHandler: per-agent projects (named workspace folders).
type ProjectsHandler struct {
	dataStore store.Store
	guard     *agentGuard
}

// CronHandler: per-user config cron catalog + per-agent DB cron jobs.
type CronHandler struct {
	dataStore store.Store
	guard     *agentGuard
	cfg       *configRepo
}

// SkillsHandler: skills catalog, install/upload/search, per-agent skills.
type SkillsHandler struct {
	workspaceStore workspace.Store
	guard          *agentGuard
}

// PluginsHandler: plugin management + hook-plugin discovery.
type PluginsHandler struct {
	cfg *configRepo
}

// ToolsHandler: global tool registry config.
type ToolsHandler struct {
	guard *agentGuard
	cfg   *configRepo
}

// ScopedHandler: scoped CRUD for providers/channels + registered adapters.
type ScopedHandler struct {
	dataStore store.Store
	guard     *agentGuard
	cfg       *configRepo
	chans     *channelRepo
}

// UsersHandler: admin user CRUD, nested per-user agent/apikey provisioning.
type UsersHandler struct {
	accounts  *users.Accounts
	apikeys   *users.APIKeys
	dataStore store.Store
	guard     *agentGuard
	cfg       *configRepo
}

// APIKeysHandler: the caller's own API keys.
type APIKeysHandler struct {
	apikeys   *users.APIKeys
	dataStore store.Store
}

// UsageHandler: tenant- and agent-level usage metering.
type UsageHandler struct {
	usage usage.Meter
	guard *agentGuard
}

// TasksHandler: the admin task-queue listing.
type TasksHandler struct {
	taskQueue *taskqueue.Queue
}
