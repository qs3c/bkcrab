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

// engine 构建 gin 路由器。
// 遵循 webook 风格：engine() 只负责构造 handler（注入依赖）并调用各 handler
// 的 RegisterRoutes——路由细节完全封装在各 handler 内部。
func (s *Server) engine() *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// SPA handler 自行处理 trailing slash；gin 的自动 301 重定向
	// 会改变 API 客户端可观测的行为，因此禁用。
	r.RedirectTrailingSlash = false
	r.RedirectFixedPath = false

	// 健康探针（无需认证）
	healthz := func(c *gin.Context) { c.String(http.StatusOK, "ok") }
	r.GET("/healthz", healthz)
	r.GET("/livez", healthz)
	r.GET("/readyz", healthz)

	// Middleware 束：三种认证级别，构造时注入给每个 handler。
	mw := &Middleware{
		Auth:  s.authMiddleware,
		Opt:   s.optionalAuth,
		Admin: s.requireSuperAdmin,
	}

	// 共享服务层：从 Server 持有的实例构造，按需分发给各 handler。
	guard := &agentGuard{dataStore: s.dataStore, userResolver: s.userResolver}
	cfg := &configRepo{dataStore: s.dataStore, guard: guard}
	wsRepo := &workspaceRepo{dataStore: s.dataStore, workspaceStore: s.workspaceStore, guard: guard}
	chanRepo := &channelRepo{dataStore: s.dataStore, userResolver: s.userResolver, guard: guard}

	// 各域 handler：通过各自的 NewXxxHandler 构造函数注入依赖（webook 风格）。
	// 每个 handler 只接收它实际使用的具体依赖 —— 构造函数签名即依赖契约。
	handlers := []Handler{
		NewSessionHandler(s.accounts, s.authResolver, s.dataStore, s.userResolver, s.port, s.startedAt, guard, cfg, mw),
		NewChatHandler(s.dataStore, s.webChan, s.chatEventHub(), guard, wsRepo, mw),
		NewAgentsHandler(s.dataStore, s.accounts, guard, cfg, mw),
		NewAgentFilesHandler(s.dataStore, s.workspaceStore, guard, wsRepo, mw),
		NewAgentChannelsHandler(s.dataStore, s.userResolver, guard, chanRepo, mw),
		NewProjectsHandler(s.dataStore, guard, mw),
		NewCronHandler(s.dataStore, guard, cfg, mw),
		NewSkillsHandler(s.workspaceStore, guard, mw),
		NewPluginsHandler(cfg, mw),
		NewToolsHandler(guard, cfg, mw),
		NewScopedHandler(s.dataStore, guard, cfg, chanRepo, mw),
		NewUsersHandler(s.accounts, s.apikeys, s.dataStore, guard, cfg, mw),
		NewAPIKeysHandler(s.apikeys, s.dataStore, mw),
		NewUsageHandler(s.usage, guard, mw),
		NewTasksHandler(s.taskQueue, mw),
	}

	// 每个 handler 自行注册路由（webook 风格）。
	for _, h := range handlers {
		h.RegisterRoutes(r)
	}

	// OpenAI 兼容 API 与 WebSocket 网关。
	if s.apiServer != nil {
		s.apiServer.RegisterRoutes(r)
	}

	// SPA 回退：未命中 API 路由的请求返回嵌入的 Next.js 应用。
	r.NoRoute(wrap(s.spaHandlerFunc()))

	return r
}
