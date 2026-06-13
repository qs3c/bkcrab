package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/qs3c/bkclaw/internal/auth"
)

// HandleProvisionAppUser 处理 POST /v1/users。
//
// 仅通过 api_key 认证。创建（或返回）代表调用应用的终端用户（由 external_id 标识）
// 的 bkclaw 用户。幂等：使用相同 external_id 重复调用返回相同的
// bkclaw user_id，无论该行是否已存在。
//
// 请求体：{ "external_id": "...", "display_name": "..."（可选） }
// 响应：  { "user_id": "u_…", "external_id": "...", "created": bool }
//
// 会话、agent_files 和 scope=user 的配置都以返回的 user_id 为键，
// 因此一旦调用应用获得该 user_id，该终端用户的每个下游交互
// 都能干净地分区。不想预创建的应用可以完全跳过此端点，
// 在每次调用时通过 /v1/chat/completions 请求体中的 `user` 字段
// （或 X-Bkclaw-End-User 头）传递 — 无论哪种方式，认证层都会在首次见到时懒创建。
func (s *Server) HandleProvisionAppUser(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.AuthMethod != "apikey" || ident.APIKeyID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{"message": "api_key required", "type": "authentication_error"},
		})
		return
	}

	var req struct {
		ExternalID  string `json:"external_id"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "invalid request body", "type": "invalid_request_error"},
		})
		return
	}
	req.ExternalID = strings.TrimSpace(req.ExternalID)
	if req.ExternalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "external_id is required", "type": "invalid_request_error"},
		})
		return
	}

	// 通过 SwitchToAppUser 保持创建逻辑在一处
	// — 与请求时切换使用的是同一代码路径。返回的身份
	// 包含已解析的 app_user user_id；我们不会将其写回
	// 请求上下文，因为此端点是纯粹的配置调用，而非透传。
	switched, err := s.authResolver.SwitchToAppUser(r.Context(), ident, req.ExternalID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "server_error"},
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":     switched.UserID,
		"external_id": req.ExternalID,
	})
}
