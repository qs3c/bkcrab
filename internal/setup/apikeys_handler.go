package setup

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/users"
)

// APIKeysHandler 负责当前调用者自己的 API key（含 agent 多选授权）。
type APIKeysHandler struct {
	apikeys   *users.APIKeys
	dataStore store.AgentStore
	mw        *Middleware
}

// NewAPIKeysHandler 构造 APIKeysHandler。
func NewAPIKeysHandler(apikeys *users.APIKeys, dataStore store.AgentStore, mw *Middleware) *APIKeysHandler {
	return &APIKeysHandler{apikeys: apikeys, dataStore: dataStore, mw: mw}
}

// RegisterRoutes 注册当前用户 API key 管理路由。
func (s *APIKeysHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/api/apikeys", wrap(s.mw.Auth(s.handleListAPIKeys)))
	r.POST("/api/apikeys", wrap(s.mw.Auth(s.handleCreateAPIKey)))
	r.DELETE("/api/apikeys/:id", wrap(s.mw.Auth(s.handleDeleteAPIKey)))
	r.POST("/api/apikeys/:id/rotate", wrap(s.mw.Auth(s.handleRotateAPIKey)))
	r.PUT("/api/apikeys/:id/agents", wrap(s.mw.Auth(s.handleSetAPIKeyAgents)))
}

func (s *APIKeysHandler) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false})
		return
	}
	list, err := s.apikeys.List(r.Context(), ident.EffectiveUserID())
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	enriched := make([]map[string]any, 0, len(list))
	for _, ak := range list {
		// 只有 type=agent 密钥携带显式的 agent 列表；user/admin 在认证时从拥有者派生作用域，
		// 因此此处的空数组意味着"层级定义作用域，而不是行。"
		var agents []string
		if ak.Type == users.APIKeyTypeAgent {
			agents, _ = s.apikeys.Agents(r.Context(), ak.ID)
		}
		enriched = append(enriched, map[string]any{
			"id":        ak.ID,
			"userId":    ak.UserID,
			"name":      ak.Name,
			"key":       ak.Key,
			"type":      ak.Type,
			"agents":    agents,
			"createdAt": ak.CreatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"apikeys": enriched})
}

// handleCreateAPIKey 强制执行角色 × 类型策略：
//   - super_admin 可以发出 admin / user / agent 密钥
//   - 普通用户可以发出 user / agent 密钥（仅限他们自己的 agent）
//   - app_user（通过 apikey 配置）根本不能发出密钥
//
// type=agent 额外要求每个 agentId 解析为一个调用者被允许绑定的 agent —
// 拥有者可以绑定自己的，super_admin 可以绑定任何人的。这是权威门控；
// users 包只验证形状，不验证策略。
func (s *APIKeysHandler) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	if ident.Role == users.RoleAppUser {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "app_user cannot issue api keys"})
		return
	}
	var req createAPIKeyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.Type == "" {
		req.Type = users.APIKeyTypeAgent
	}
	if !users.IsAPIKeyType(req.Type) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid type"})
		return
	}
	if req.Type == users.APIKeyTypeAdmin && ident.Role != users.RoleSuperAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "only super_admin may issue admin keys"})
		return
	}
	if req.Type == users.APIKeyTypeAgent {
		if len(req.AgentIDs) == 0 {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "type=agent requires at least one agentId"})
			return
		}
		// 只绑定调用者控制的 agent。Super_admin 可以绑定任何人的；其他所有人都必须拥有每个 agent。
		if ident.Role != users.RoleSuperAdmin {
			for _, aid := range req.AgentIDs {
				rec, err := s.dataStore.GetAgent(r.Context(), aid)
				if err != nil || rec == nil || rec.UserID != ident.UserID {
					jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "cannot bind agent " + aid})
					return
				}
			}
		}
	}
	ak, token, err := s.apikeys.Create(r.Context(), ident.UserID, req.Name, req.Type, req.AgentIDs)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	ak.Key = token
	jsonResponse(w, http.StatusCreated, map[string]any{"apikey": ak, "token": token})
}

func (s *APIKeysHandler) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	id := r.PathValue("id")
	rec, err := s.apikeys.Get(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	if rec.UserID != ident.UserID && ident.Role != users.RoleSuperAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	if err := s.apikeys.Delete(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *APIKeysHandler) handleRotateAPIKey(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	id := r.PathValue("id")
	rec, err := s.apikeys.Get(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	if rec.UserID != ident.UserID && ident.Role != users.RoleSuperAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	token, err := s.apikeys.Rotate(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"token": token})
}

type setAPIKeyAgentsReq struct {
	AgentIDs []string `json:"agentIds"`
}

func (s *APIKeysHandler) handleSetAPIKeyAgents(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	id := r.PathValue("id")
	rec, err := s.apikeys.Get(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	if rec.UserID != ident.UserID && ident.Role != users.RoleSuperAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	var req setAPIKeyAgentsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if ident.Role != users.RoleSuperAdmin {
		for _, aid := range req.AgentIDs {
			ar, err := s.dataStore.GetAgent(r.Context(), aid)
			if err != nil || ar == nil || ar.UserID != ident.UserID {
				jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "cannot bind agent " + aid})
				return
			}
		}
	}
	if err := s.apikeys.SetAgents(r.Context(), id, req.AgentIDs); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
