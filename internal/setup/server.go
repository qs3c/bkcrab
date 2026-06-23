package setup

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/agent/tools"
	"github.com/qs3c/bkclaw/internal/api"
	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/session"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/taskqueue"
	"github.com/qs3c/bkclaw/internal/usage"
	"github.com/qs3c/bkclaw/internal/users"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// AgentHandle 是 Web UI 与运行中的 agent 通信的接口。
type AgentHandle interface {
	Name() string
	HandleWebChat(ctx context.Context, sessionId, projectIdHint, userID, text string, imageURLs []string, params map[string]any) string
	HandleWebChatStream(ctx context.Context, sessionId, projectIdHint, userID, text string, imageURLs []string, params map[string]any, events chan<- agent.ChatEvent) string
	// SteerWeb 将消息缓冲到该会话正在进行的轮次中；
	// 当没有正在运行的轮次时返回 false（调用者回退到普通发送）。
	SteerWeb(sessionId, projectIDHint, text string) bool
	WebChatHistory(sessionId string) []map[string]any
	WebChatSessions() []session.WebSession
	DeleteWebChatSession(sessionId string) error
	RenameWebChatSession(sessionId, title string) error
	ContextUsageBaseline() map[string]any
	// MoveWebChatSession 将聊天重新分配到另一个项目（当 projectID=="" 时分离）。
	// 在旧和新作用域目录之间迁移工作区文件，并释放任何活动的沙箱，
	// 以便下一轮在新绑定挂载路径上冷启动。
	MoveWebChatSession(ctx context.Context, sessionId, projectID string) error
	ReloadWorkspaceFiles()
	// WriteSessionAttachments 将用户上传的字节（data URL / HTTPS URL）物化到 agent 的会话工作区中，
	// 以便技能可以通过 /workspace/<filename> 读取它们。按输入顺序返回相对文件名；跳过单个项目的错误。
	WriteSessionAttachments(ctx context.Context, sessionID, projectID string, atts []agent.Attachment) []string
	// RegisteredTools 返回实时的工具注册表投影 — 该 agent 当前已加载的内容
	//（内置工具 + MCP + 插件工具）。Tools 标签页使用它来渲染允许列表复选框选择器。
	RegisteredTools() []tools.ToolInfo
}

// AgentProvider 由 gateway.UserSpace 的 agent 管理器实现 — 供那些需要枚举*当前调用者*的
// agent（通过用户解析器解析，而非从全局池中获取）的处理程序使用。
type AgentProvider interface {
	AllAgents() []AgentHandle
	AgentByID(id string) AgentHandle
	ReloadAgents() error
}

// Server 托管 Web UI + 管理 API。多用户是强制的 —
// 每个请求必须通过 auth.Resolver 解析为真实的 users.id。
type Server struct {
	port           int
	bind           string
	gatewayCfg     *config.GatewayCfg
	userResolver   api.UserResolver
	taskQueue      *taskqueue.Queue
	apiServer      *api.Server
	authResolver   *auth.Resolver
	accounts       *users.Accounts
	apikeys        *users.APIKeys
	dataStore      store.Store
	workspaceStore workspace.Store
	webChan        *channels.WebChannel
	// chatEvents 将实时的 agent 聊天事件分发到跨浏览器标签页的已订阅 SSE 客户端。
	// 首次使用时延迟初始化，以便没有显式连接它的旧调用者仍然可以工作。
	chatEvents *agent.EventHub
	usage      usage.Meter
	startedAt  time.Time
}

// NewServer 在指定端口上创建一个设置向导服务器。
func NewServer(port int) *Server {
	return &Server{port: port, bind: "loopback", startedAt: time.Now()}
}

// SetGatewayConfig 设置网关配置的绑定地址和 HTTP 端点。
func (s *Server) SetGatewayConfig(cfg *config.GatewayCfg) {
	s.gatewayCfg = cfg
	if cfg.Bind != "" {
		s.bind = cfg.Bind
	}
	if cfg.Port > 0 {
		s.port = cfg.Port
	}
}

// SetTaskQueue 设置任务 API 端点的任务队列。
func (s *Server) SetTaskQueue(tq *taskqueue.Queue) {
	s.taskQueue = tq
}

// SetAPIServer 设置 OpenAI 兼容的 API 服务器，用于 /v1/* 和 /ws 路由。
func (s *Server) SetAPIServer(apiSrv *api.Server) {
	s.apiServer = apiSrv
}

// SetUserResolver 设置按用户的 agent 路由解析器。
func (s *Server) SetUserResolver(resolver api.UserResolver) {
	s.userResolver = resolver
}

// SetStore 设置存储后端。
func (s *Server) SetStore(st store.Store) {
	s.dataStore = st
	if st != nil {
		s.accounts, _ = users.NewAccounts(st)
		s.apikeys, _ = users.NewAPIKeys(st)
	}
}

// SetWorkspaceStore 安装用于 agent 生成的工件的 blob 存储。
func (s *Server) SetWorkspaceStore(ws workspace.Store) {
	s.workspaceStore = ws
}

// SetUsageMeter 安装按租户的资源计数器。
func (s *Server) SetUsageMeter(m usage.Meter) {
	s.usage = m
}

// SetAuth 安装认证解析器。必需的。
func (s *Server) SetAuth(resolver *auth.Resolver) {
	s.authResolver = resolver
}

// SetWebChannel 安装 SSE 订阅端点使用的进程内扇出机制。
// 设置后，/api/chat/subscribe 为每个 (agent, session) 对保持 SSE 流打开，
// 并转发所有路由到 channel="web" 的出站消息 — 这就是 cron 触发的
// agent 回复实时显示在仪表板聊天面板中的方式。
func (s *Server) SetWebChannel(wc *channels.WebChannel) {
	s.webChan = wc
}

// chatEventHub 返回延迟初始化的 hub。集中化使得每个聊天处理程序访问同一个实例 —
// 如果没有这个，流式处理程序的 hub 发布将永远无法到达订阅处理程序。
func (s *Server) chatEventHub() *agent.EventHub {
	if s.chatEvents == nil {
		s.chatEvents = agent.NewEventHub()
	}
	return s.chatEvents
}

// ChatEventHub 暴露 hub，以便网关可以将流管道附加到总线触发的 web 轮次
//（cron / 目标延续 / 心跳 / 子 agent），赋予它们与用户键入轮次相同的 SSE 流式体验。
// 包裹了 chatEventHub 的延迟初始化。
func (s *Server) ChatEventHub() *agent.EventHub { return s.chatEventHub() }

// authMiddleware 包裹 auth.Resolver 的 Middleware。每个需要认证的路由都必须使用。
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	if s.authResolver == nil {
		return func(w http.ResponseWriter, r *http.Request) {
			jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "auth not configured"})
		}
	}
	return s.authResolver.Middleware(next)
}

// optionalAuth 是适用于登录前可访问的端点（status、login、onboard）的引导友好变体。
func (s *Server) optionalAuth(next http.HandlerFunc) http.HandlerFunc {
	if s.authResolver == nil {
		return next
	}
	return s.authResolver.Optional(next)
}

// requireSuperAdmin 将处理程序限制为平台管理员权限：
// 可以是 super_admin 会话或 type=admin 的 apikey。尽管名称如此，
// 但它接受两者 — apikey 路径是编程管理客户端无需浏览器 cookie 即可访问 /api/admin/*
// 的唯一方式。对于需要纯会话形式的罕见情况，直接使用 auth.RequireSuperAdmin。
func (s *Server) requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.authMiddleware(auth.RequirePlatformAdmin(next))
}

// Run 启动 HTTP 服务器并阻塞，直到上下文被取消。
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()

	// 健康检查探测（无需认证）。
	healthz := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
	mux.HandleFunc("GET /healthz", healthz)
	mux.HandleFunc("GET /livez", healthz)
	mux.HandleFunc("GET /readyz", healthz)

	auth := s.authMiddleware
	opt := s.optionalAuth
	admin := s.requireSuperAdmin

	// 引导 / 登录。
	mux.HandleFunc("GET /api/status", opt(s.handleStatus))
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", auth(s.handleLogout))
	mux.HandleFunc("GET /api/me", auth(s.handleMe))
	mux.HandleFunc("PUT /api/me", auth(s.handleUpdateMe))
	mux.HandleFunc("POST /api/me/password", auth(s.handleChangeMyPassword))
	mux.HandleFunc("POST /api/test-provider", opt(s.handleTestProvider))
	mux.HandleFunc("POST /api/onboard", s.handleOnboard)
	mux.HandleFunc("POST /api/register", s.handleRegister)
	mux.HandleFunc("GET /api/admin/registration", admin(s.handleGetRegistration))
	mux.HandleFunc("PUT /api/admin/registration", admin(s.handleSetRegistration))
	mux.HandleFunc("GET /api/admin/chats", admin(s.handleAdminChats))

	// 按用户配置（system_settings + 作用域内的 providers/channels）。
	mux.HandleFunc("GET /api/config", auth(s.handleGetConfig))
	mux.HandleFunc("POST /api/config", auth(s.handleUpdateConfig))

	// 聊天
	mux.HandleFunc("POST /api/chat", auth(s.handleChat))
	mux.HandleFunc("POST /api/chat/stream", auth(s.handleChatStream))
	mux.HandleFunc("POST /api/chat/steer", auth(s.handleChatSteer))
	mux.HandleFunc("GET /api/chat/history", auth(s.handleChatHistory))
	mux.HandleFunc("GET /api/chat/todo", auth(s.handleChatTodo))
	mux.HandleFunc("GET /api/chat/sessions", auth(s.handleChatSessions))
	mux.HandleFunc("PUT /api/chat/sessions/{key}", auth(s.handleRenameSession))
	mux.HandleFunc("DELETE /api/chat/sessions/{key}", auth(s.handleDeleteSession))
	mux.HandleFunc("PATCH /api/chat/sessions/{key}/project", auth(s.handleMoveSessionProject))
	// 长生命周期的 SSE 订阅，使得 cron 触发（及其他异步）消息无需手动刷新即可到达打开的聊天面板。
	mux.HandleFunc("GET /api/chat/subscribe", auth(s.handleChatSubscribe))

	// Agent 管理
	mux.HandleFunc("GET /api/agents", auth(s.handleListAgents))
	mux.HandleFunc("POST /api/agents", auth(s.handleCreateAgent))
	mux.HandleFunc("GET /api/agents/{id}", auth(s.handleGetAgent))
	mux.HandleFunc("PUT /api/agents/{id}", auth(s.handleUpdateAgent))
	mux.HandleFunc("GET /api/agents/{id}/config", auth(s.handleGetAgentConfig))
	mux.HandleFunc("GET /api/agents/{id}/tools/registered", auth(s.handleListAgentRegisteredTools))
	mux.HandleFunc("DELETE /api/agents/{id}", auth(s.handleDeleteAgent))

	mux.HandleFunc("GET /api/agents/{id}/files", auth(s.handleAgentFileList))
	mux.HandleFunc("GET /api/agents/{id}/files.zip", auth(s.handleAgentFilesZip))
	mux.HandleFunc("GET /api/agents/{id}/files/{path...}", auth(s.handleAgentFile))
	mux.HandleFunc("POST /api/agents/{id}/files", auth(s.handleAgentFileUpload))
	// 仅限自托管：在操作系统的原生文件浏览器（Finder/Explorer/xdg-open）中打开工作区目录。
	// 托管部署在处理程序内部返回 403 — 那里的聊天者不拥有守护进程的文件系统。
	mux.HandleFunc("POST /api/agents/{id}/workspace/reveal", auth(s.handleAgentWorkspaceReveal))

	mux.HandleFunc("GET /api/agents/{id}/system-files/{name}", auth(s.handleGetAgentSystemFile))
	mux.HandleFunc("PUT /api/agents/{id}/system-files/{name}", auth(s.handlePutAgentSystemFile))
	mux.HandleFunc("DELETE /api/agents/{id}/system-files/{name}", auth(s.handleDeleteAgentSystemFile))

	// 按 agent 的项目：命名的工作区文件夹，用于组织聊天并在其所有会话中共享文件。
	// POST .../sessions 是"在项目中新建聊天"的路径 — 预先创建带有 project_id 标记的会话行，
	// 这样第一次轮次就已经将工作区 IO 路由到 projects/<pid>/。
	mux.HandleFunc("GET /api/agents/{id}/projects", auth(s.handleListProjects))
	mux.HandleFunc("POST /api/agents/{id}/projects", auth(s.handleCreateProject))
	mux.HandleFunc("PATCH /api/agents/{id}/projects/{pid}", auth(s.handleUpdateProject))
	mux.HandleFunc("DELETE /api/agents/{id}/projects/{pid}", auth(s.handleDeleteProject))

	// 按 agent 的频道（IM 机器人绑定）
	mux.HandleFunc("GET /api/agents/{id}/channels", auth(s.handleListAgentChannels))
	mux.HandleFunc("POST /api/agents/{id}/channels/telegram", auth(s.handleConnectAgentTelegram))
	mux.HandleFunc("POST /api/agents/{id}/channels/discord", auth(s.handleConnectAgentDiscord))
	mux.HandleFunc("POST /api/agents/{id}/channels/slack", auth(s.handleConnectAgentSlack))
	mux.HandleFunc("POST /api/agents/{id}/channels/wechat/login", auth(s.handleStartAgentWeChatLogin))
	mux.HandleFunc("GET /api/agents/{id}/channels/wechat/login/status", auth(s.handleAgentWeChatLoginStatus))
	mux.HandleFunc("POST /api/agents/{id}/channels/line", auth(s.handleConnectAgentLINE))
	mux.HandleFunc("POST /api/agents/{id}/channels/feishu", auth(s.handleConnectAgentFeishu))
	mux.HandleFunc("DELETE /api/agents/{id}/channels/{type}/{accountId}", auth(s.handleDisconnectAgentChannel))

	// 飞书事件 webhook。无需认证 — 飞书不带 bkclaw bearer token 直接发送到此端点。
	// 每个事件的安全性来自 verification_token，在适配器内部针对 payload 的 header.token 进行验证。
	// {appId} 路径段将接收范围限定到一个已注册的频道。
	mux.HandleFunc("POST /api/feishu/webhook/{appId}", s.handleFeishuWebhook)

	// LINE Messaging API 事件 webhook。在 bkclaw 层无需认证 —
	// 每个事件的安全性通过 HMAC-SHA256(channel_secret, body) 实现，
	// 由适配器根据 `x-line-signature` 头部验证。{accountId} 路径段是机器人的 userId，
	// 将接收范围限定到一个已注册的频道。
	mux.HandleFunc("POST /api/line/webhook/{accountId}", s.handleLINEWebhook)

	// 技能
	mux.HandleFunc("GET /api/skills", auth(s.handleListSkills))
	mux.HandleFunc("GET /api/skills/search", auth(s.handleSearchSkills))
	mux.HandleFunc("POST /api/skills/install", auth(s.handleInstallSkill))
	mux.HandleFunc("POST /api/skills/upload", auth(s.handleUploadSkill))
	mux.HandleFunc("DELETE /api/skills/{name}", admin(s.handleDeleteSkill))
	mux.HandleFunc("GET /api/agents/{id}/skills", auth(s.handleListAgentSkills))
	mux.HandleFunc("DELETE /api/agents/{id}/skills/{name}", auth(s.handleDeleteAgentSkill))

	// 插件（仅限 super_admin）。
	mux.HandleFunc("GET /api/plugins", admin(s.handleListPlugins))
	mux.HandleFunc("PUT /api/plugins/{id}", admin(s.handleUpdatePlugin))
	// Hook 插件发现 — Context 页面上每个 agent 的 Plugins 开关的只读元数据。
	// Agent 拥有者（不仅仅是管理员）需要此信息来了解他们可以启用哪些插件。
	mux.HandleFunc("GET /api/plugins/hook", auth(s.handleListHookPlugins))

	// 工具（仅限 super_admin）。
	mux.HandleFunc("GET /api/tools", admin(s.handleGetTools))
	mux.HandleFunc("PUT /api/tools", admin(s.handleSaveTools))

	// 频道（运行时可用的已注册频道适配器的只读列表）
	mux.HandleFunc("GET /api/channels", auth(s.handleListChannels))

	// 作用域 CRUD：系统 / 用户 / agent 范围内的 providers + channels。
	mux.HandleFunc("GET /api/providers", auth(s.handleListProviders))
	mux.HandleFunc("POST /api/providers", auth(s.handleCreateProvider))
	mux.HandleFunc("PUT /api/providers/{id}", auth(s.handleUpdateProvider))
	mux.HandleFunc("DELETE /api/providers/{id}", auth(s.handleDeleteProvider))
	mux.HandleFunc("POST /api/providers/{id}/test", auth(s.handleTestStoredProvider))
	mux.HandleFunc("GET /api/scoped-channels", auth(s.handleListScopedChannels))
	mux.HandleFunc("POST /api/scoped-channels", auth(s.handleCreateScopedChannel))
	mux.HandleFunc("PUT /api/scoped-channels/{id}", auth(s.handleUpdateScopedChannel))
	mux.HandleFunc("DELETE /api/scoped-channels/{id}", auth(s.handleDeleteScopedChannel))

	// Cron 任务（按用户，配置定义的目录）
	mux.HandleFunc("GET /api/cron", auth(s.handleListCronJobs))
	mux.HandleFunc("POST /api/cron", auth(s.handleCreateCronJob))
	mux.HandleFunc("PUT /api/cron/{id}", auth(s.handleUpdateCronJob))
	mux.HandleFunc("DELETE /api/cron/{id}", auth(s.handleDeleteCronJob))

	// 按 agent 的 cron 任务（数据库支持，包括 agent 在运行时通过 create_cron_job 自行调度的任何任务）。
	mux.HandleFunc("GET /api/agents/{id}/cron", auth(s.handleListAgentCronJobs))
	mux.HandleFunc("DELETE /api/agents/{id}/cron/{jobId}", auth(s.handleDeleteAgentCronJob))
	mux.HandleFunc("PUT /api/agents/{id}/cron/{jobId}", auth(s.handleToggleAgentCronJob))

	// 任务
	mux.HandleFunc("GET /api/tasks", admin(s.handleListTasks))

	// API 密钥（按用户，支持 agent 多选）。
	mux.HandleFunc("GET /api/apikeys", auth(s.handleListAPIKeys))
	mux.HandleFunc("POST /api/apikeys", auth(s.handleCreateAPIKey))
	mux.HandleFunc("DELETE /api/apikeys/{id}", auth(s.handleDeleteAPIKey))
	mux.HandleFunc("POST /api/apikeys/{id}/rotate", auth(s.handleRotateAPIKey))
	mux.HandleFunc("PUT /api/apikeys/{id}/agents", auth(s.handleSetAPIKeyAgents))

	// 用户 — 扁平资源路径。顶层 CRUD 仅限管理员；
	// 嵌套的 {id}/apikeys + {id}/agents 接受管理员或自己
	//（在处理程序内部通过 requireUserOrAdmin 进行门控）。
	mux.HandleFunc("GET /api/users", admin(s.handleListUsers))
	mux.HandleFunc("POST /api/users", admin(s.handleCreateUser))
	mux.HandleFunc("PUT /api/users/{id}", admin(s.handleUpdateUser))
	mux.HandleFunc("DELETE /api/users/{id}", admin(s.handleDeleteUser))
	mux.HandleFunc("POST /api/users/{id}/password", admin(s.handleResetUserPassword))
	mux.HandleFunc("POST /api/users/{id}/apikeys", auth(s.handleCreateUserAPIKey))
	mux.HandleFunc("GET /api/users/{id}/agents", auth(s.handleListUserAgents))
	mux.HandleFunc("POST /api/users/{id}/agents", auth(s.handleCreateUserAgent))
	// 跨租户 agent 列表移至 /api/agents?all=true（设置该参数时仅限管理员）。
	// /api/usage 取代了 /api/admin/usage。
	mux.HandleFunc("GET /api/usage", admin(s.handleGetUsage))
	// 按 agent 的使用量：在处理程序内部通过 requireAgentOwner 进行拥有者门控（或 super_admin），
	// 因此这里使用普通的 auth 包装器。
	mux.HandleFunc("GET /api/agents/{id}/usage", auth(s.handleGetAgentUsage))

	// OpenAI 兼容的 API 和 WebSocket 网关。
	if s.apiServer != nil {
		s.apiServer.RegisterRoutes(mux)
	}

	// 静态 UI 文件。
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		return fmt.Errorf("setup: embed sub: %w", err)
	}
	mux.Handle("/", spaHandler{fs: webRoot})

	var addr string
	if s.bind == "all" {
		addr = fmt.Sprintf("0.0.0.0:%d", s.port)
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	srv := &http.Server{Addr: addr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("setup: listen %s: %w", addr, err)
	}
	slog.Info("web UI running", "url", fmt.Sprintf("http://localhost:%d", s.port))
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// spaHandler 使用 SPA 风格的回退机制提供嵌入式 Next.js UI 服务。
type spaHandler struct {
	fs fs.FS
}

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path != "/" && strings.HasSuffix(path, "/") {
		path = strings.TrimSuffix(path, "/")
	}
	fsPath := strings.TrimPrefix(path, "/")
	if fsPath == "" {
		fsPath = "."
	}
	if f, err := h.fs.Open(fsPath); err == nil {
		stat, statErr := f.Stat()
		f.Close()
		if statErr == nil && !stat.IsDir() {
			http.ServeFileFS(w, r, h.fs, fsPath)
			return
		}
	}
	var indexPath string
	if fsPath == "." {
		indexPath = "index.html"
	} else {
		indexPath = fsPath + "/index.html"
	}
	if f, err := h.fs.Open(indexPath); err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, indexPath)
		return
	}
	if strings.HasPrefix(fsPath, "agents/") {
		parts := strings.SplitN(fsPath, "/", 3)
		if len(parts) >= 3 && parts[1] != "default" {
			directFallback := "agents/default/" + parts[2]
			if f, err := h.fs.Open(directFallback); err == nil {
				stat, statErr := f.Stat()
				f.Close()
				if statErr == nil && !stat.IsDir() {
					http.ServeFileFS(w, r, h.fs, directFallback)
					return
				}
			}
			dirFallback := "agents/default/" + parts[2] + "/index.html"
			if f, err := h.fs.Open(dirFallback); err == nil {
				f.Close()
				http.ServeFileFS(w, r, h.fs, dirFallback)
				return
			}
		// 嵌套动态段回退：像 agents/[id]/chat/[session] 和 agents/[id]/project/[pid] 这样的路由
		// 在构建时发出单个占位符 ("_")。将任何紧跟在已知动态父级 (chat, project) 下的段替换为 "_"，
		// 无论后面是什么。这涵盖了页面 HTML 和 Next 16 在客户端导航期间获取的每个路由的 RSC 负载：
		//   /chat/<sid>/                            → /chat/_/index.html
		//   /chat/<sid>/index.txt                   → /chat/_/index.txt
		//   /chat/<sid>/__next.agents.$d$id.chat.$d$session.__PAGE__.txt
		//   …
		// 没有这个，App Router 在侧边栏点击时的 RSC 获取会得到 404（或根 index.html），
		// 放弃软导航，回退到 window.location — 这会导致页面闪烁并中断正在进行的流。
		// 随着新动态路由的引入，将它们添加到下面的 dynamicParents 中。
			dynamicParents := map[string]bool{"chat": true, "project": true}
			sub := strings.Split(parts[2], "/")
			substituted := false
			for i := 0; i < len(sub)-1; i++ {
				if dynamicParents[sub[i]] && sub[i+1] != "_" {
					sub[i+1] = "_"
					substituted = true
				}
			}
			if substituted {
				placeholder := "agents/default/" + strings.Join(sub, "/")
				if f, err := h.fs.Open(placeholder); err == nil {
					stat, statErr := f.Stat()
					f.Close()
					if statErr == nil && !stat.IsDir() {
						http.ServeFileFS(w, r, h.fs, placeholder)
						return
					}
				}
				placeholderIndex := placeholder + "/index.html"
				if f, err := h.fs.Open(placeholderIndex); err == nil {
					f.Close()
					http.ServeFileFS(w, r, h.fs, placeholderIndex)
					return
				}
			}
		}
	}
	htmlPath := fsPath + ".html"
	if f, err := h.fs.Open(htmlPath); err == nil {
		f.Close()
		http.ServeFileFS(w, r, h.fs, htmlPath)
		return
	}
	http.ServeFileFS(w, r, h.fs, "index.html")
}
