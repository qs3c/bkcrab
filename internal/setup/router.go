package setup

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// wrap adapts a net/http handler into a gin handler. It bridges gin's
// path params back onto the request so the existing handlers keep using
// r.PathValue(...) unchanged, and serves the raw ResponseWriter so SSE
// streaming and websocket hijack keep working.
//
// gin's catch-all params (*path) carry a leading slash that net/http's
// {path...} did not, so we trim it to preserve the value handlers saw
// under ServeMux. Single-segment (:name) params never start with a
// slash, so trimming is a no-op for them.
func wrap(h http.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, p := range c.Params {
			c.Request.SetPathValue(p.Key, strings.TrimPrefix(p.Value, "/"))
		}
		h(c.Writer, c.Request)
	}
}

// engine builds the gin router with every route registered. Auth is
// composed exactly as before — the http.HandlerFunc middlewares
// (authMiddleware / optionalAuth / requireSuperAdmin) wrap each handler,
// then wrap() bridges the result into gin. This keeps identity
// propagation (via request context) and per-route gating identical to
// the previous ServeMux wiring.
func (s *Server) engine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// We trim trailing slashes / fix paths ourselves in the SPA handler;
	// gin's automatic 301 redirects would change observable behavior for
	// API clients, so disable them.
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false

	// Health probes (unauthenticated).
	healthz := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	r.GET("/healthz", healthz)
	r.GET("/livez", healthz)
	r.GET("/readyz", healthz)

	auth := s.authMiddleware
	opt := s.optionalAuth
	admin := s.requireSuperAdmin

	// Composition root: build the shared services from the Server's
	// gathered instances, then hand each handler exactly the concrete
	// dependencies (and services) it uses — nothing more.
	guard := &agentGuard{dataStore: s.dataStore, userResolver: s.userResolver}
	cfg := &configRepo{dataStore: s.dataStore, guard: guard}
	wsRepo := &workspaceRepo{dataStore: s.dataStore, workspaceStore: s.workspaceStore, guard: guard}
	chanRepo := &channelRepo{dataStore: s.dataStore, userResolver: s.userResolver, guard: guard}

	sess := &SessionHandler{
		accounts: s.accounts, authResolver: s.authResolver, dataStore: s.dataStore,
		userResolver: s.userResolver, port: s.port, startedAt: s.startedAt,
		guard: guard, cfg: cfg,
	}
	chat := &ChatHandler{
		dataStore: s.dataStore, webChan: s.webChan, chatEvents: s.chatEventHub(),
		guard: guard, ws: wsRepo,
	}
	agents := &AgentsHandler{dataStore: s.dataStore, accounts: s.accounts, guard: guard, cfg: cfg}
	files := &AgentFilesHandler{
		dataStore: s.dataStore, workspaceStore: s.workspaceStore, guard: guard, ws: wsRepo,
	}
	chans := &AgentChannelsHandler{
		dataStore: s.dataStore, userResolver: s.userResolver, guard: guard, chans: chanRepo,
	}
	projects := &ProjectsHandler{dataStore: s.dataStore, guard: guard}
	cron := &CronHandler{dataStore: s.dataStore, guard: guard, cfg: cfg}
	skills := &SkillsHandler{workspaceStore: s.workspaceStore, guard: guard}
	plugins := &PluginsHandler{cfg: cfg}
	tools := &ToolsHandler{guard: guard, cfg: cfg}
	scoped := &ScopedHandler{dataStore: s.dataStore, guard: guard, cfg: cfg, chans: chanRepo}
	users := &UsersHandler{
		accounts: s.accounts, apikeys: s.apikeys, dataStore: s.dataStore, guard: guard, cfg: cfg,
	}
	apikeys := &APIKeysHandler{apikeys: s.apikeys, dataStore: s.dataStore}
	usage := &UsageHandler{usage: s.usage, guard: guard}
	tasks := &TasksHandler{taskQueue: s.taskQueue}

	// Bootstrap / login.
	r.GET("/api/status", wrap(opt(sess.handleStatus)))
	r.POST("/api/login", wrap(sess.handleLogin))
	r.POST("/api/logout", wrap(auth(sess.handleLogout)))
	r.GET("/api/me", wrap(auth(sess.handleMe)))
	r.PUT("/api/me", wrap(auth(sess.handleUpdateMe)))
	r.POST("/api/me/password", wrap(auth(sess.handleChangeMyPassword)))
	r.POST("/api/test-provider", wrap(opt(sess.handleTestProvider)))
	r.POST("/api/onboard", wrap(sess.handleOnboard))
	r.POST("/api/register", wrap(sess.handleRegister))
	r.GET("/api/admin/registration", wrap(admin(sess.handleGetRegistration)))
	r.PUT("/api/admin/registration", wrap(admin(sess.handleSetRegistration)))
	r.GET("/api/admin/chats", wrap(admin(users.handleAdminChats)))

	// Per-user config (system_settings + scoped providers/channels).
	r.GET("/api/config", wrap(auth(sess.handleGetConfig)))
	r.POST("/api/config", wrap(auth(sess.handleUpdateConfig)))

	// Chat
	r.POST("/api/chat", wrap(auth(chat.handleChat)))
	r.POST("/api/chat/stream", wrap(auth(chat.handleChatStream)))
	r.POST("/api/chat/steer", wrap(auth(chat.handleChatSteer)))
	r.GET("/api/chat/history", wrap(auth(chat.handleChatHistory)))
	r.GET("/api/chat/todo", wrap(auth(chat.handleChatTodo)))
	r.GET("/api/chat/sessions", wrap(auth(chat.handleChatSessions)))
	r.PUT("/api/chat/sessions/:key", wrap(auth(chat.handleRenameSession)))
	r.DELETE("/api/chat/sessions/:key", wrap(auth(chat.handleDeleteSession)))
	r.PATCH("/api/chat/sessions/:key/project", wrap(auth(chat.handleMoveSessionProject)))
	// Long-lived SSE subscription so cron-fired (and other async)
	// messages reach the open chat panel without a manual refresh.
	r.GET("/api/chat/subscribe", wrap(auth(chat.handleChatSubscribe)))

	// Agents
	r.GET("/api/agents", wrap(auth(agents.handleListAgents)))
	r.POST("/api/agents", wrap(auth(agents.handleCreateAgent)))
	r.GET("/api/agents/:id", wrap(auth(agents.handleGetAgent)))
	r.PUT("/api/agents/:id", wrap(auth(agents.handleUpdateAgent)))
	r.GET("/api/agents/:id/config", wrap(auth(agents.handleGetAgentConfig)))
	r.GET("/api/agents/:id/tools/registered", wrap(auth(agents.handleListAgentRegisteredTools)))
	r.DELETE("/api/agents/:id", wrap(auth(agents.handleDeleteAgent)))

	r.GET("/api/agents/:id/files", wrap(auth(files.handleAgentFileList)))
	r.GET("/api/agents/:id/files.zip", wrap(auth(files.handleAgentFilesZip)))
	r.GET("/api/agents/:id/files/*path", wrap(auth(files.handleAgentFile)))
	r.POST("/api/agents/:id/files", wrap(auth(files.handleAgentFileUpload)))
	// Self-hosted-only: opens the workspace dir in the operator's
	// native file browser (Finder/Explorer/xdg-open). Hosted
	// deployments 403 inside the handler — chatters there don't
	// own the daemon's filesystem.
	r.POST("/api/agents/:id/workspace/reveal", wrap(auth(files.handleAgentWorkspaceReveal)))

	r.GET("/api/agents/:id/system-files/:name", wrap(auth(files.handleGetAgentSystemFile)))
	r.PUT("/api/agents/:id/system-files/:name", wrap(auth(files.handlePutAgentSystemFile)))
	r.DELETE("/api/agents/:id/system-files/:name", wrap(auth(files.handleDeleteAgentSystemFile)))

	// Per-agent projects: named workspace folders that group chats and
	// share files across all sessions inside them. POST .../sessions
	// is the "New chat in project" path — pre-creates the session row
	// stamped with project_id so the very first turn already routes
	// workspace IO to projects/<pid>/.
	r.GET("/api/agents/:id/projects", wrap(auth(projects.handleListProjects)))
	r.POST("/api/agents/:id/projects", wrap(auth(projects.handleCreateProject)))
	r.PATCH("/api/agents/:id/projects/:pid", wrap(auth(projects.handleUpdateProject)))
	r.DELETE("/api/agents/:id/projects/:pid", wrap(auth(projects.handleDeleteProject)))

	// Per-agent channels (IM bot bindings)
	r.GET("/api/agents/:id/channels", wrap(auth(chans.handleListAgentChannels)))
	r.POST("/api/agents/:id/channels/telegram", wrap(auth(chans.handleConnectAgentTelegram)))
	r.POST("/api/agents/:id/channels/discord", wrap(auth(chans.handleConnectAgentDiscord)))
	r.POST("/api/agents/:id/channels/slack", wrap(auth(chans.handleConnectAgentSlack)))
	r.POST("/api/agents/:id/channels/wechat/login", wrap(auth(chans.handleStartAgentWeChatLogin)))
	r.GET("/api/agents/:id/channels/wechat/login/status", wrap(auth(chans.handleAgentWeChatLoginStatus)))
	r.POST("/api/agents/:id/channels/line", wrap(auth(chans.handleConnectAgentLINE)))
	r.POST("/api/agents/:id/channels/feishu", wrap(auth(chans.handleConnectAgentFeishu)))
	r.DELETE("/api/agents/:id/channels/:type/:accountId", wrap(auth(chans.handleDisconnectAgentChannel)))

	// Feishu (飞书) event webhook. UNAUTHENTICATED — Feishu posts here
	// without a bkclaw bearer token. Per-event security comes from
	// the verification_token validated inside the adapter against the
	// payload's header.token. The :appId path segment scopes the
	// receive to one registered channel.
	r.POST("/api/feishu/webhook/:appId", wrap(chans.handleFeishuWebhook))

	// LINE Messaging API event webhook. UNAUTHENTICATED at the bkclaw
	// layer — per-event security is HMAC-SHA256(channel_secret, body)
	// validated by the adapter against the `x-line-signature` header.
	// The :accountId path segment is the bot's userId, scoping the
	// receive to one registered channel.
	r.POST("/api/line/webhook/:accountId", wrap(chans.handleLINEWebhook))

	// Skills
	r.GET("/api/skills", wrap(auth(skills.handleListSkills)))
	r.GET("/api/skills/search", wrap(auth(skills.handleSearchSkills)))
	r.POST("/api/skills/install", wrap(auth(skills.handleInstallSkill)))
	r.POST("/api/skills/upload", wrap(auth(skills.handleUploadSkill)))
	r.DELETE("/api/skills/:name", wrap(admin(skills.handleDeleteSkill)))
	r.GET("/api/agents/:id/skills", wrap(auth(skills.handleListAgentSkills)))
	r.DELETE("/api/agents/:id/skills/:name", wrap(auth(skills.handleDeleteAgentSkill)))

	// Plugins (super_admin only).
	r.GET("/api/plugins", wrap(admin(plugins.handleListPlugins)))
	r.PUT("/api/plugins/:id", wrap(admin(plugins.handleUpdatePlugin)))
	// Hook plugin discovery — read-only metadata for the per-agent
	// Plugins toggle on the Context page. Agent owners (not just
	// admins) need this to know what plugins they can enable.
	r.GET("/api/plugins/hook", wrap(auth(plugins.handleListHookPlugins)))

	// Tools (super_admin only).
	r.GET("/api/tools", wrap(admin(tools.handleGetTools)))
	r.PUT("/api/tools", wrap(admin(tools.handleSaveTools)))

	// Channels (read-only list of registered channel adapters at runtime)
	r.GET("/api/channels", wrap(auth(scoped.handleListChannels)))

	// Scoped CRUD: providers + channels at system / user / agent scope.
	r.GET("/api/providers", wrap(auth(scoped.handleListProviders)))
	r.POST("/api/providers", wrap(auth(scoped.handleCreateProvider)))
	r.PUT("/api/providers/:id", wrap(auth(scoped.handleUpdateProvider)))
	r.DELETE("/api/providers/:id", wrap(auth(scoped.handleDeleteProvider)))
	r.POST("/api/providers/:id/test", wrap(auth(sess.handleTestStoredProvider)))
	r.GET("/api/scoped-channels", wrap(auth(scoped.handleListScopedChannels)))
	r.POST("/api/scoped-channels", wrap(auth(scoped.handleCreateScopedChannel)))
	r.PUT("/api/scoped-channels/:id", wrap(auth(scoped.handleUpdateScopedChannel)))
	r.DELETE("/api/scoped-channels/:id", wrap(auth(scoped.handleDeleteScopedChannel)))

	// Cron jobs (per-user, config-defined catalog)
	r.GET("/api/cron", wrap(auth(cron.handleListCronJobs)))
	r.POST("/api/cron", wrap(auth(cron.handleCreateCronJob)))
	r.PUT("/api/cron/:id", wrap(auth(cron.handleUpdateCronJob)))
	r.DELETE("/api/cron/:id", wrap(auth(cron.handleDeleteCronJob)))

	// Per-agent cron jobs (DB-backed, includes anything the agent
	// scheduled itself via create_cron_job at runtime).
	r.GET("/api/agents/:id/cron", wrap(auth(cron.handleListAgentCronJobs)))
	r.DELETE("/api/agents/:id/cron/:jobId", wrap(auth(cron.handleDeleteAgentCronJob)))
	r.PUT("/api/agents/:id/cron/:jobId", wrap(auth(cron.handleToggleAgentCronJob)))

	// Tasks
	r.GET("/api/tasks", wrap(admin(tasks.handleListTasks)))

	// Apikeys (per-user, with agent multi-select).
	r.GET("/api/apikeys", wrap(auth(apikeys.handleListAPIKeys)))
	r.POST("/api/apikeys", wrap(auth(apikeys.handleCreateAPIKey)))
	r.DELETE("/api/apikeys/:id", wrap(auth(apikeys.handleDeleteAPIKey)))
	r.POST("/api/apikeys/:id/rotate", wrap(auth(apikeys.handleRotateAPIKey)))
	r.PUT("/api/apikeys/:id/agents", wrap(auth(apikeys.handleSetAPIKeyAgents)))

	// Users — flat resource paths. Top-level CRUD is admin-only;
	// nested :id/apikeys + :id/agents accept admin-or-self
	// (gated in-handler via requireUserOrAdmin).
	r.GET("/api/users", wrap(admin(users.handleListUsers)))
	r.POST("/api/users", wrap(admin(users.handleCreateUser)))
	r.PUT("/api/users/:id", wrap(admin(users.handleUpdateUser)))
	r.DELETE("/api/users/:id", wrap(admin(users.handleDeleteUser)))
	r.POST("/api/users/:id/password", wrap(admin(users.handleResetUserPassword)))
	r.POST("/api/users/:id/apikeys", wrap(auth(users.handleCreateUserAPIKey)))
	r.GET("/api/users/:id/agents", wrap(auth(users.handleListUserAgents)))
	r.POST("/api/users/:id/agents", wrap(auth(users.handleCreateUserAgent)))
	// Cross-tenant agent list moved into /api/agents?all=true (admin-only
	// when the param is set). /api/usage replaces /api/admin/usage.
	r.GET("/api/usage", wrap(admin(usage.handleGetUsage)))
	// Per-agent usage: owner-gated (or super_admin) inside the handler
	// itself via requireAgentOwner, so we use the plain auth wrapper here.
	r.GET("/api/agents/:id/usage", wrap(auth(usage.handleGetAgentUsage)))

	// OpenAI-compatible API and WebSocket gateway.
	if s.apiServer != nil {
		s.apiServer.RegisterRoutes(r)
	}

	// Static UI files — anything that didn't match an API route falls
	// through to the embedded Next.js SPA handler.
	r.NoRoute(wrap(s.spaHandlerFunc()))

	return r
}
