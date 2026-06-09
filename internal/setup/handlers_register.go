package setup

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fastclaw-ai/fastclaw/internal/scope"
	"github.com/fastclaw-ai/fastclaw/internal/users"
)

// registrationSettingNamespace is the configs-table key for the public
// signup toggle. Stored as kind=setting, name=registration, data={"open": bool}.
// Defaults to closed: a fresh instance shouldn't accept anonymous signups
// until the operator explicitly opens the door.
const registrationSettingNamespace = "registration"

// registrationOpen reads the system-level toggle. Errors are treated as
// "closed" — fail safe is "no signup".
func (s *Server) registrationOpen(r *http.Request) bool {
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

// handleRegister is the public signup endpoint. Gated by the admin-controlled
// registration_open setting; falls back to closed on any read error so a
// momentary store hiccup can't accidentally fling the door open.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil || s.authResolver == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth not configured"})
		return
	}
	if !s.registrationOpen(r) {
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
	// Light email shape check — the users.Account store also validates;
	// this layer just catches the obvious "no @" before we hit the DB.
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
		// users.Create surfaces "username taken" / "email taken" as plain
		// errors; passing them through gives the form a usable message.
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	cookie, err := s.authResolver.IssueSession(r.Context(), acct.ID)
	if err == nil {
		http.SetCookie(w, cookie)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "user": acct})
}

// --- Admin: read + write the toggle ---

type registrationConfig struct {
	Open bool `json:"open"`
}

func (s *Server) handleGetRegistration(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, registrationConfig{Open: s.registrationOpen(r)})
}

func (s *Server) handleSetRegistration(w http.ResponseWriter, r *http.Request) {
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
