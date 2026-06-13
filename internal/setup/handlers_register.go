package setup

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/users"
)

// registrationSettingNamespace 是公开注册开关的 configs 表键。
// 存储为 kind=setting, name=registration, data={"open": bool}。
// 默认为关闭：新实例不应接受匿名注册，直到操作员明确打开。
const registrationSettingNamespace = "registration"

// registrationOpen 读取系统级开关。错误被视为"关闭" — 故障安全为"禁止注册"。
func (s *configRepo) registrationOpen(r *http.Request) bool {
	if s.dataStore == nil {
		return false
	}
	merged, err := scope.Setting(r.Context(), s.dataStore, registrationSettingNamespace, "", "")
	if err != nil {
		return false
	}
	v, _ := merged["open"].(bool)
	return v
}

type registerRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
}

// handleRegister 是公开注册端点。由管理员控制的 registration_open 设置门控；
// 在任何读取错误时回退为关闭，以便瞬时的存储故障不会意外打开大门。
func (s *SessionHandler) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil || s.authResolver == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth not configured"})
		return
	}
	if !s.cfg.registrationOpen(r) {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "registration is closed"})
		return
	}
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if req.Username == "" || req.Email == "" || req.Password == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "username, email, password required"})
		return
	}
	// 轻量级电子邮件格式检查 — users.Account 存储也会验证；
	// 这一层只是在到达数据库之前捕获明显的"缺少 @"。
	if !strings.Contains(req.Email, "@") || strings.Contains(req.Email, " ") {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid email"})
		return
	}
	if len(req.Password) < 8 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "password must be at least 8 characters"})
		return
	}
	acct, err := s.accounts.Create(r.Context(), users.CreateInput{
		Username:    req.Username,
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Role:        users.RoleUser,
	})
	if err != nil {
		// users.Create 将"用户名已存在"/"电子邮件已存在"作为普通错误返回；
		// 传递它们给表单提供可用的消息。
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cookie, err := s.authResolver.IssueSession(r.Context(), acct.ID)
	if err == nil {
		http.SetCookie(w, cookie)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "user": acct})
}

// --- 管理员：读取和写入开关 ---

type registrationConfig struct {
	Open bool `json:"open"`
}

func (s *SessionHandler) handleGetRegistration(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, registrationConfig{Open: s.cfg.registrationOpen(r)})
}

func (s *SessionHandler) handleSetRegistration(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store not ready"})
		return
	}
	var req registrationConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	data := map[string]interface{}{"open": req.Open}
	if err := scope.SaveSettingByScope(r.Context(), s.dataStore, scope.System, "", registrationSettingNamespace, data); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, registrationConfig{Open: req.Open})
}
