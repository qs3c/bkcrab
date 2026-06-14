package setup

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/usage"
)

// UsageHandler 负责租户级与 per-agent 的用量统计。
type UsageHandler struct {
	usage usage.Meter
	guard *agentGuard
	mw    *Middleware
}

// NewUsageHandler 构造 UsageHandler。
func NewUsageHandler(meter usage.Meter, guard *agentGuard, mw *Middleware) *UsageHandler {
	return &UsageHandler{usage: meter, guard: guard, mw: mw}
}

// RegisterRoutes 注册用量统计路由。
func (s *UsageHandler) RegisterRoutes(r *gin.Engine) {
	// 租户级用量（仅管理员）
	r.GET("/api/usage", wrap(s.mw.Admin(s.handleGetUsage)))
	// per-agent 用量：agent 拥有者或超级管理员，权限在 handler 内部门控
	r.GET("/api/agents/:id/usage", wrap(s.mw.Auth(s.handleGetAgentUsage)))
}

// rangeFromQuery 将 ?range=24h|7d|30d（默认 7d）解析为最近 N 天的 usage.Range。
// 管理仪表板不暴露精确的小时窗口 — 按天统计已足够回答"谁消耗了什么"。
func rangeFromQuery(r *http.Request) usage.Range {
	switch r.URL.Query().Get("range") {
	case "24h":
		return usage.LastN(1)
	case "30d":
		return usage.LastN(30)
	default:
		return usage.LastN(7)
	}
}

// limitFromQuery 将 ?limit= 限制在 [1, 100] 之间，默认为 10。
func limitFromQuery(r *http.Request) int {
	v, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || v <= 0 {
		return 10
	}
	if v > 100 {
		return 100
	}
	return v
}

// handleGetUsage 返回管理仪表板的关键数据：
// 总 token 数，以及指定时间窗口内的 top agent 和 top 用户。在 server.go 中由 requireSuperAdmin 包裹。
func (s *UsageHandler) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"totals":    usage.Totals{},
			"topAgents": []usage.Rank{},
			"topUsers":  []usage.Rank{},
		})
		return
	}
	rng := rangeFromQuery(r)
	limit := limitFromQuery(r)
	totals, err := s.usage.Totals(r.Context(), rng)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	topAgents, err := s.usage.TopAgents(r.Context(), rng, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	topUsers, err := s.usage.TopUsers(r.Context(), rng, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"range":     r.URL.Query().Get("range"),
		"totals":    totals,
		"topAgents": topAgents,
		"topUsers":  topUsers,
	})
}

// handleGetAgentUsage 返回单个 agent 的每个会话 token 汇总
// — agent 设置对话框中"Token Usage"标签页背后的数据。
// 通过 requireAgentOwner 进行拥有者门控，因此公开 agent 的聊天查看者看不到拥有者的其他会话。
// `sessions` 列表是以 session_key 为键的 Rank[]（在名称查找后，客户端使用会话标题渲染）。
func (s *UsageHandler) handleGetAgentUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.guard.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if s.usage == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"range":    r.URL.Query().Get("range"),
			"totals":   nil,
			"sessions": []any{},
		})
		return
	}
	rng := rangeFromQuery(r)
	limit := limitFromQuery(r)
	if limit < 50 {
		// 会话列表是该标签页的主要视图；默认限制（limitFromQuery 的 10）太短。
		// 除非调用者明确要求更少，否则上限为 50 行。
		limit = 50
	}
	sessions, err := s.usage.SessionsForAgent(r.Context(), id, "", rng, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"range":    r.URL.Query().Get("range"),
		"agentId":  id,
		"sessions": sessions,
	})
}
