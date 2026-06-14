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

// Server is the composition root for the web UI + admin API. It holds
// every shared instance gathered from the setters and, in engine(),
// distributes each one to the per-domain handler that actually needs it
// (plus the small shared services). Nothing outside this file embeds
// Server — handlers receive only their own concrete dependencies.
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
// （cron / 目标延续 / 心跳 / 子 agent），赋予它们与用户键入轮次相同的 SSE 流式体验。
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
	handler := s.engine()

	var addr string
	if s.bind == "all" {
		addr = fmt.Sprintf("0.0.0.0:%d", s.port)
	} else {
		addr = fmt.Sprintf("127.0.0.1:%d", s.port)
	}
	srv := &http.Server{Addr: addr, Handler: handler}

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

// spaHandlerFunc 返回嵌入式 UI 的回退处理器（http.HandlerFunc），
// 作为 gin 的 NoRoute 处理器：任何未命中 API 路由的路径都返回
// Next.js SPA。嵌入子 FS 实际上不会失败（已编译进二进制）；万一失败
// 则返回 500 而非在启动时 panic。
func (s *Server) spaHandlerFunc() http.HandlerFunc {
	webRoot, err := fs.Sub(webFS, "web")
	if err != nil {
		slog.Error("setup: embed sub failed", "error", err)
		return func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "ui unavailable", http.StatusInternalServerError)
		}
	}
	h := spaHandler{fs: webRoot}
	return h.ServeHTTP
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
