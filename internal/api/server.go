package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/config"
)

// UserResolver 通过用户 ID 查找用户空间。
type UserResolver interface {
	UserSpaceFor(userID string) (*UserSpaceView, error)
	LocalAgentManager() *agent.Manager
	IsCloudMode() bool
}

// AgentInjector 是解析器的可选能力，可以将外部 agent_id 动态
// 附加到调用者的 UserSpace 中。由 super_admin 聊天处理器使用，
// 使管理员在管理员自己的 user_id 下操作 agent（该 agent 位于
// 所有者的账户中）— 会话、内存、provider 范围都以管理员为键，
// 而 agent 的持久身份（系统提示、agent 范围配置、技能）被复用。
// 实现必须是幂等的。
type AgentInjector interface {
	EnsureAgent(ctx context.Context, userID, agentID string) error
}

// UserSpaceView 是 API 层所需的 gateway.UserSpace 的子集。
type UserSpaceView struct {
	UserID string
	Agents *agent.Manager
	Config *config.Config
}

// Server 处理兼容 OpenAI 的 API 和 WebSocket 网关。
type Server struct {
	resolver     UserResolver
	authResolver *auth.Resolver
	gatewayCfg   *config.GatewayCfg
	limiter      *rateLimiter
}

// NewServer 创建一个新的 API 服务器。authResolver 是必需的 — 没有回退的"本地"认证。
func NewServer(resolver UserResolver, authResolver *auth.Resolver, gatewayCfg *config.GatewayCfg) *Server {
	var rpm int
	if gatewayCfg != nil {
		rpm = gatewayCfg.RateLimit.RPM
	}
	return &Server{
		resolver:     resolver,
		authResolver: authResolver,
		gatewayCfg:   gatewayCfg,
		limiter:      newRateLimiter(rpm),
	}
}

// RegisterRoutes 在给定的 mux 上注册 API 路由。
func (s *Server) RegisterRoutes(r gin.IRouter) {
	r.GET("/ws", wrapHTTP(s.HandleWebSocket))
	r.OPTIONS("/v1/*any", wrapHTTP(s.handleCORS))

	getUserID := func(r *http.Request) string { return config.UserIDFromContext(r.Context()) }

	if s.gatewayCfg == nil || s.gatewayCfg.HTTP.Endpoints.ChatCompletions.Enabled {
		r.POST("/v1/chat/completions",
			wrapHTTP(s.authMiddleware(rateLimitMiddleware(s.limiter, getUserID, s.HandleChatCompletions))))
	}
	if s.gatewayCfg == nil || s.gatewayCfg.HTTP.Endpoints.Agents.Enabled {
		r.GET("/v1/agents",
			wrapHTTP(s.authMiddleware(rateLimitMiddleware(s.limiter, getUserID, s.HandleListAgents))))
	}
	// 为下游终端用户显式配置 app_user。
	// 始终可用 — 任何 api_key 调用都可以使用相同的身份切换
	//（header 或 `user` 请求体字段）而无需预先创建，此端点
	// 仅为偏好提前创建并本地存储返回的 bkclaw user_id 的调用者而存在。
	r.POST("/v1/users",
		wrapHTTP(s.authMiddleware(rateLimitMiddleware(s.limiter, getUserID, s.HandleProvisionAppUser))))
}

// wrapHTTP 将 net/http handler 适配为 gin handler，把 gin 的路径参数
// 回写到请求上，使 handler 继续使用 r.PathValue(...)。这里不读取
// catch-all 参数值，因此无需处理前导斜杠。
func wrapHTTP(h http.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		for _, p := range c.Params {
			c.Request.SetPathValue(p.Key, p.Value)
		}
		h(c.Writer, c.Request)
	}
}

func (s *Server) handleCORS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, x-bkclaw-agent-id, x-bkclaw-session-key")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

// HandleListAgents 处理 GET /v1/agents。仅返回此调用者有权限的 agent。
func (s *Server) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	space, err := s.userSpaceFor(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "authentication_error"},
		})
		return
	}
	ident, _ := auth.FromContext(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"agents": buildAgentList(space, ident)})
}

func buildAgentList(space *UserSpaceView, ident auth.Identity) []map[string]string {
	all := space.Agents.All()
	modelMap := make(map[string]string)
	if space.Config != nil {
		for _, ra := range config.ResolveAgents(space.Config, nil) {
			modelMap[ra.ID] = ra.Model
		}
	}
	agents := make([]map[string]string, 0, len(all))
	for _, ag := range all {
		if !ident.CanAccessAgent(ag.Name()) {
			continue
		}
		model := ag.Model()
		if model == "" {
			model = modelMap[ag.Name()]
		}
		agents = append(agents, map[string]string{
			"id":    ag.Name(),
			"name":  ag.Name(),
			"model": model,
		})
	}
	return agents
}

// userSpaceFor 从请求的身份解析用户空间。
func (s *Server) userSpaceFor(r *http.Request) (*UserSpaceView, error) {
	uid := config.UserIDFromContext(r.Context())
	if uid == "" {
		return nil, errors.New("unauthorized")
	}
	return s.resolver.UserSpaceFor(uid)
}

// authMiddleware 验证 apikey/cookie 并将解析后的身份
// 写入 ctx。仅 apikey 的端点还可以额外检查
// Identity.CanAccessAgent 以验证请求的 agentID。
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if s.authResolver == nil {
			writeUnauth(w, "auth resolver not configured")
			return
		}
		s.authResolver.Middleware(next)(w, r)
	}
}

func writeUnauth(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{
		"error": map[string]string{"message": msg, "type": "authentication_error"},
	})
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
