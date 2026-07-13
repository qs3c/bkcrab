package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/buildinfo"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/session"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

// --- 登录/登出/我的信息 ---

type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.accounts == nil || s.authResolver == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "auth not configured"})
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	acct, err := s.accounts.Authenticate(r.Context(), req.Login, req.Password)
	if err != nil {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "invalid credentials"})
		return
	}
	cookie, err := s.authResolver.IssueSession(r.Context(), acct.ID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	http.SetCookie(w, cookie)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "user": acct})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if s.authResolver != nil {
		if c, err := r.Cookie(auth.SessionCookieName); err == nil {
			_ = s.authResolver.RevokeSession(r.Context(), c.Value)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:   auth.SessionCookieName,
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false})
		return
	}
	acct, err := s.accounts.Get(r.Context(), ident.UserID)
	if err != nil {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// deployMode 让前端显示或隐藏仅限本地的便利功能
	//（在工作区文件夹中打开 Finder、未来的"在 $EDITOR 中编辑 SOUL.md"钩子等）。
	// 此处作为唯一事实来源，这样我们不必在 5 个不同的处理程序中读取环境变量；
	// 前端可以将其与用户资料一起缓存，因为它在运行时不会改变。
	deployMode := "self-hosted"
	if buildinfo.IsHostedDeploy() {
		deployMode = "hosted"
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"ok":          true,
		"user":        acct,
		"authMethod":  ident.AuthMethod,
		"actAsUserId": ident.ActAsUserID,
		"readOnly":    ident.ReadOnly(),
		"deployMode":  deployMode,
	})
}

// --- 自助服务资料 ---

// maxAvatarBytes 限制 base64 编码的头像负载大小。约 256KB
// 对于一个合理的正方形（例如 256×256 PNG）足够了；更大的会将用户行推入 Postgres 的 TOAST 区域并减慢 /api/me。
// 前端应在上传前调整大小/压缩 — 这只是一个限制墙。
const maxAvatarBytes = 256 * 1024

type updateMeReq struct {
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl"`
}

// handleUpdateMe 允许已登录用户编辑自己的显示名称和头像。
// 头像必须为空（清除）或 data: URL — 完整的 HTTP URL
// 会在渲染时通过 referer 泄露用户数据，因此我们限制为仅内联图像。
func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	var req updateMeReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.AvatarURL != "" {
		if !strings.HasPrefix(req.AvatarURL, "data:image/") {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "avatar must be a data:image/* URL"})
			return
		}
		if len(req.AvatarURL) > maxAvatarBytes {
			jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "avatar too large (max 256KB)"})
			return
		}
	}
	acct, err := s.accounts.UpdateProfile(r.Context(), ident.UserID, req.DisplayName, req.AvatarURL)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "user": acct})
}

type changePasswordReq struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

// handleChangeMyPassword 是管理员密码重置的自助服务变体 —
// 在接受新密码之前需要当前密码。最小长度匹配其他地方的隐式默认值；
// 我们不强制执行强规则，因为安装是单租户的，而且我们不想成为用正则表达式拒绝"correcthorse"的地方。
func (s *Server) handleChangeMyPassword(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	if ident.Role == users.RoleAppUser {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "app_user has no password"})
		return
	}
	var req changePasswordReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.NewPassword == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "new password required"})
		return
	}
	if err := s.accounts.VerifyPassword(r.Context(), ident.UserID, req.OldPassword); err != nil {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "current password incorrect"})
		return
	}
	if err := s.accounts.SetPassword(r.Context(), ident.UserID, req.NewPassword); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// --- 引导 ---

type onboardRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`

	Provider string `json:"provider"`
	APIBase  string `json:"apiBase"`
	APIKey   string `json:"apiKey"`
	APIType  string `json:"apiType,omitempty"`
	AuthType string `json:"authType,omitempty"`
	Model    string `json:"model"`

	AgentName string `json:"agentName,omitempty"`

	SandboxEnabled         bool   `json:"sandboxEnabled,omitempty"`
	SandboxBackend         string `json:"sandboxBackend,omitempty"`
	SandboxImage           string `json:"sandboxImage,omitempty"`
	SandboxE2BKey          string `json:"sandboxE2BKey,omitempty"`
	SandboxBoxliteURL      string `json:"sandboxBoxliteUrl,omitempty"`
	SandboxBoxliteClientID string `json:"sandboxBoxliteClientId,omitempty"`
	SandboxBoxliteKey      string `json:"sandboxBoxliteKey,omitempty"`
	SandboxBoxlitePrefix   string `json:"sandboxBoxlitePrefix,omitempty"`
}

// handleOnboard 在单个逻辑操作中创建第一个 super_admin + 第一个系统提供者 + 第一个 agent。
// 仅在用户表为空时可调用；后续调用返回 409。
func (s *Server) handleOnboard(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil || s.accounts == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "store not ready"})
		return
	}
	count, err := s.accounts.Count(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if count > 0 {
		jsonResponse(w, http.StatusConflict, map[string]any{"ok": false, "error": "already onboarded"})
		return
	}
	var req onboardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.Username == "" || req.Email == "" || req.Password == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "username, email, password required"})
		return
	}
	acct, err := s.accounts.Create(r.Context(), users.CreateInput{
		Username:    req.Username,
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Role:        users.RoleSuperAdmin,
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.Provider != "" && req.APIKey != "" {
		pcfg := config.ProviderConfig{
			APIBase:  req.APIBase,
			APIKey:   req.APIKey,
			APIType:  req.APIType,
			AuthType: req.AuthType,
		}
		// 将选择的模型种子写入 Provider.Models，以便 Models/Providers
		// 管理员页面立即显示它 — 没有这个，用户进入"编辑 Provider"对话框时，
		// 即使 agents.defaults 已经命名了该模型，Models 列表也是空的，
		// 测试连接按钮也是非活动的。
		if req.Model != "" {
			pcfg.Models = []config.ModelEntry{{ID: req.Model, Name: req.Model}}
		}
		if err := scope.SaveProviderByScope(r.Context(), s.dataStore, scope.System, "", req.Provider, pcfg); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if req.Model != "" {
			defaults := map[string]interface{}{
				"model": req.Provider + "/" + req.Model,
			}
			if err := scope.SaveSettingByScope(r.Context(), s.dataStore, scope.System, "", "agents.defaults", defaults); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
	}
	agentID, _ := generateID("agt_")
	agentName := req.AgentName
	if agentName == "" {
		agentName = "default"
	}
	agentRec := &store.AgentRecord{
		ID:     agentID,
		UserID: acct.ID,
		Name:   agentName,
	}
	if err := s.dataStore.SaveAgent(r.Context(), agentRec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if req.SandboxEnabled {
		backend := req.SandboxBackend
		if backend == "" {
			backend = "docker"
		}
		sandbox := map[string]interface{}{
			"enabled": true,
			"backend": backend,
		}
		if req.SandboxImage != "" {
			sandbox["image"] = req.SandboxImage
		}
		if req.SandboxE2BKey != "" {
			sandbox["e2bKey"] = req.SandboxE2BKey
		}
		if req.SandboxBoxliteURL != "" {
			sandbox["boxliteUrl"] = req.SandboxBoxliteURL
		}
		if req.SandboxBoxliteClientID != "" {
			sandbox["boxliteClientId"] = req.SandboxBoxliteClientID
		}
		if req.SandboxBoxliteKey != "" {
			sandbox["boxliteKey"] = req.SandboxBoxliteKey
		}
		if req.SandboxBoxlitePrefix != "" {
			sandbox["boxlitePrefix"] = req.SandboxBoxlitePrefix
		}
		if err := scope.SaveSettingByScope(r.Context(), s.dataStore, scope.System, "", "sandbox", sandbox); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	cookie, err := s.authResolver.IssueSession(r.Context(), acct.ID)
	if err == nil {
		http.SetCookie(w, cookie)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "user": acct, "agentId": agentID})
}

// --- 管理员：用户管理 ---

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	list, err := s.accounts.List(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// app_user 账户是由 apikeys 代为下游最终用户以编程方式创建的 —
	// 它们不是管理员管理的人类用户，其数量可能非常大。默认隐藏它们；
	// 需要审计它们的管理员可以传递 ?includeAppUsers=1。
	if r.URL.Query().Get("includeAppUsers") != "1" {
		filtered := make([]*users.Account, 0, len(list))
		for _, u := range list {
			if u.Role == users.RoleAppUser {
				continue
			}
			filtered = append(filtered, u)
		}
		list = filtered
	}
	jsonResponse(w, http.StatusOK, map[string]any{"users": list})
}

type createUserReq struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role,omitempty"`
	// AgentQuota 使用指针，以便管理员可以区分"未设置 → 使用默认无限制"和"显式 0 → 禁止自助创建"。
	AgentQuota *int64 `json:"agentQuota,omitempty"`
	// AvatarURL 是一个可选的内联 data:image/* URL（≤256KB）。与自助服务 /api/me 端点相同的形状和上限。
	AvatarURL string `json:"avatarUrl,omitempty"`
	// ExternalID 是调用应用自己的用户标识符。与认证派生的 apikey_id（不从请求体获取）结合，
	// 使配置幂等：相同的上游用户始终解析为相同的 bkcrab user_id。
	// 对于会话调用者（Web 管理员点击）是可选的；对于上游 apikey 配置是典型的，
	// 其中调用者希望稳定映射回自己的用户表。
	ExternalID string `json:"externalId,omitempty"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if req.AvatarURL != "" {
		if !strings.HasPrefix(req.AvatarURL, "data:image/") {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "avatar must be a data:image/* URL"})
			return
		}
		if len(req.AvatarURL) > maxAvatarBytes {
			jsonResponse(w, http.StatusRequestEntityTooLarge, map[string]any{"ok": false, "error": "avatar too large (max 256KB)"})
			return
		}
	}
	// apikey_id 由认证派生，绝不应从请求体信任 — 该行将配置的用户审计回创建他们的密钥。
	// 对于会话调用者（Web 管理员）为空，当管理 apikey 访问此端点时填充。
	apikeyID := ""
	if ident, ok := auth.FromContext(r.Context()); ok {
		apikeyID = ident.APIKeyID
	}
	role := req.Role
	if role == "" {
		role = users.RoleUser
	}
	acct, err := s.accounts.Create(r.Context(), users.CreateInput{
		Username:    req.Username,
		Email:       req.Email,
		Password:    req.Password,
		DisplayName: req.DisplayName,
		Role:        role,
		AgentQuota:  req.AgentQuota,
		AvatarURL:   req.AvatarURL,
		APIKeyID:    apikeyID,
		ExternalID:  req.ExternalID,
	})
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"user": acct})
}

type updateUserReq struct {
	DisplayName string `json:"displayName,omitempty"`
	Role        string `json:"role,omitempty"`
	Status      string `json:"status,omitempty"`
	AgentQuota  *int64 `json:"agentQuota,omitempty"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req updateUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	acct, err := s.accounts.Update(r.Context(), id, req.DisplayName, req.Role, req.Status, req.AgentQuota)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"user": acct})
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agents, err := s.dataStore.ListAgents(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	for _, rec := range agents {
		deleteCtx, release, err := s.beginLearnerAgentDeletion(r.Context(), rec.ID)
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := s.deleteLearnerAgentAssets(deleteCtx, rec.ID); err != nil {
			_ = release()
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		if err := release(); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
	}
	if err := s.accounts.Delete(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

type resetPasswordReq struct {
	Password string `json:"password"`
}

func (s *Server) handleResetUserPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req resetPasswordReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	if err := s.accounts.SetPassword(r.Context(), id, req.Password); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// respondAllAgents 返回每个用户的所有 agent，附带拥有者的用户名/电子邮件。
// 为平台范围的管理视图支持 GET /api/agents?all=true；认证门控在 handleListAgents 中
//（仅在 CanAdminPlatform 通过后调用此函数）。
func (s *Server) respondAllAgents(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no data store"})
		return
	}
	records, err := s.dataStore.ListAllAgents(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 为每个唯一 userID 解析拥有者用户名一次 — N 个 agent 可能属于少数几个用户，
	// 因此逐行查找会反复命中同一 id 的存储。
	ownerCache := map[string]*users.Account{}
	resolveOwner := func(uid string) *users.Account {
		if uid == "" {
			return nil
		}
		if a, ok := ownerCache[uid]; ok {
			return a
		}
		a, _ := s.accounts.Get(r.Context(), uid)
		ownerCache[uid] = a
		return a
	}
	out := make([]map[string]any, 0, len(records))
	for _, ar := range records {
		desc, _ := ar.Config["description"].(string)
		entry := map[string]any{
			"id":          ar.ID,
			"name":        ar.Name,
			"description": desc,
			"userId":      ar.UserID,
			"createdAt":   ar.CreatedAt,
		}
		if owner := resolveOwner(ar.UserID); owner != nil {
			entry["ownerUsername"] = owner.Username
			entry["ownerEmail"] = owner.Email
			if owner.DisplayName != "" {
				entry["ownerDisplayName"] = owner.DisplayName
			}
		}
		out = append(out, entry)
	}
	jsonResponse(w, http.StatusOK, map[string]any{"agents": out})
}

// handleAdminChats 返回跨所有 (user, agent) 对的每个聊天会话，附带上拥有用户的用户名和 agent 名称，
// 以便平台范围的管理员 Chats 页面可以渲染一个扁平表，而无需在客户端为每个 agent 展开。
// 仅限 super_admin — 注册在 /api/admin/chats 上，由管理中间件门控。
//
// 实现说明：我们按每个（聊天者 user_id, agent_id）对从 sessions 表展开，而不是每个 agent。
// 将自己的 bot 绑定到公开 agent（或在 Web 上与公开 agent 聊天）的非拥有者会在自己的 user_id 下
// 写入会话行 — 按 agent.owner 迭代会完全错过这些会话。成对展开捕获每个聊天者，
// 无论他们是否拥有该 agent。"拥有者"列然后反映聊天的实际用户，因此仪表板中的 actAs 链接
// 可以模拟真正的会话拥有者，而不是 agent 拥有者（后者可能没有对会话的读取权限）。
func (s *Server) handleAdminChats(w http.ResponseWriter, r *http.Request) {
	if s.dataStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no data store"})
		return
	}
	pairs, err := s.dataStore.ListSessionOwnerPairs(r.Context())
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	ownerCache := map[string]*users.Account{}
	resolveOwner := func(uid string) *users.Account {
		if uid == "" {
			return nil
		}
		if a, ok := ownerCache[uid]; ok {
			return a
		}
		a, _ := s.accounts.Get(r.Context(), uid)
		ownerCache[uid] = a
		return a
	}
	agentCache := map[string]*store.AgentRecord{}
	resolveAgent := func(agentID string) *store.AgentRecord {
		if agentID == "" {
			return nil
		}
		if a, ok := agentCache[agentID]; ok {
			return a
		}
		a, _ := s.dataStore.GetAgent(r.Context(), agentID)
		agentCache[agentID] = a
		return a
	}
	out := make([]map[string]any, 0)
	for _, p := range pairs {
		ag := resolveAgent(p.AgentID)
		if ag == nil {
			// 孤立会话行，其 agent 已被删除 — 跳过而不是显示一行 Agent 列为空的行。
			continue
		}
		adapter := session.NewStoreAdapter(s.dataStore, p.UserID)
		sessions, err := adapter.ListWebSessions(r.Context(), p.AgentID)
		if err != nil {
			continue
		}
		owner := resolveOwner(p.UserID)
		for _, ws := range sessions {
			entry := map[string]any{
				"id":             ws.ID,
				"agentId":        p.AgentID,
				"agentName":      ag.Name,
				"userId":         p.UserID,
				"channel":        ws.Channel,
				"accountId":      ws.AccountID,
				"chatId":         ws.ChatID,
				"projectId":      ws.ProjectID,
				"title":          ws.Title,
				"preview":        ws.Preview,
				"thumbnailUrl":   ws.ThumbnailURL,
				"createdAt":      ws.CreatedAt,
				"updatedAt":      ws.UpdatedAt,
				"lastTurnStatus": ws.LastTurnStatus,
			}
			if owner != nil {
				entry["ownerUsername"] = owner.Username
				entry["ownerEmail"] = owner.Email
				if owner.DisplayName != "" {
					entry["ownerDisplayName"] = owner.DisplayName
				}
			}
			out = append(out, entry)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"sessions": out})
}

// --- 管理员配置（按用户） ---
//
// 下面的处理程序都位于 /api/users/{id}/* 下 — 根据 requireUserOrAdmin 的要求，管理员或自己。
// 管理员路径绕过目标用户的 agent_quota（由平台发起的调用）；自助路径强制执行它。
// 配额/分叉语义位于相应的处理程序内部。

// handleListUserAgents 返回路径解析的用户拥有的 agent。
// 通过 requireUserOrAdmin 实现管理员或自己（管理员可以列出任何用户的；非管理员只能列出自己的）。
// 与常规 agent 列表相同的响应形状，以便管理工具可以复用渲染。
func (s *Server) handleListUserAgents(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("id")
	if !s.requireUserOrAdmin(w, r, uid) {
		return
	}
	if _, err := s.dataStore.GetUser(r.Context(), uid); err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "user not found"})
		return
	}
	records, err := s.dataStore.ListAgents(r.Context(), uid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(records))
	for _, ar := range records {
		desc, _ := ar.Config["description"].(string)
		out = append(out, map[string]any{
			"id":          ar.ID,
			"name":        ar.Name,
			"description": desc,
			"userId":      ar.UserID,
			"isPublic":    ar.IsPublic,
			"createdAt":   ar.CreatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"agents": out})
}

type adminCreateUserAgentReq struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	// ForkFrom 是可选的源 agent id。设置后，新 agent 从源的拥有者行继承
	// SOUL.md / IDENTITY.md / AGENTS.md / BOOTSTRAP.md / TOOLS.md / HEARTBEAT.md / agent.json，
	// 以及源的 agent 作用域 `agents.defaults` 和 `skills.entries` 配置行。
	// 每个用户的状态（MEMORY.md, USER.md, sessions, cron_jobs）和每个拥有者的路由（频道绑定）
	// 明确不复制。分叉源可以是调用者（super_admin）可以读取的任何 agent。
	ForkFrom string `json:"forkFrom,omitempty"`
}

// handleCreateUserAgent 创建由路径解析的用户拥有 agent。行为取决于调用者：
//   - 管理员（super_admin / type=admin apikey）→ 绕过目标的 agent_quota；forkFrom 被启用（将现有 agent 的身份克隆到新 agent 中）。
//   - 自己（目标用户为自己调用）→ 强制执行自己的 agent_quota；forkFrom 被忽略，
//     以避免让用户通过此路径将其他人的私有 agent 克隆到自己的命名空间中。
//
// 创建的 agent 始终是私有的；通过常规的 PUT /api/agents/{id} 流程切换为公开。
func (s *Server) handleCreateUserAgent(w http.ResponseWriter, r *http.Request) {
	targetUserID := r.PathValue("id")
	if !s.requireUserOrAdmin(w, r, targetUserID) {
		return
	}
	if !s.requireWritable(w, r) {
		return
	}
	target, err := s.dataStore.GetUser(r.Context(), targetUserID)
	if err != nil || target == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "user not found"})
		return
	}
	ident, _ := auth.FromContext(r.Context())
	isAdmin := ident.CanAdminPlatform()

	var req adminCreateUserAgentReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// 配额仅适用于自助路径。管理员配置由平台发起，有意绕过它。
	if !isAdmin && target.AgentQuota >= 0 {
		owned, err := s.dataStore.ListAgents(r.Context(), targetUserID)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if int64(len(owned)) >= target.AgentQuota {
			jsonResponse(w, http.StatusForbidden, map[string]any{
				"error": fmt.Sprintf("agent quota reached (%d) — contact your admin to provision more", target.AgentQuota),
			})
			return
		}
	}

	var source *store.AgentRecord
	if isAdmin && strings.TrimSpace(req.ForkFrom) != "" {
		source, err = s.dataStore.GetAgent(r.Context(), req.ForkFrom)
		if err != nil || source == nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "forkFrom: source agent not found"})
			return
		}
	}

	name := strings.TrimSpace(req.Name)
	if name == "" && source != nil {
		name = source.Name
	}
	if name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	description := req.Description
	if description == "" && source != nil {
		if d, ok := source.Config["description"].(string); ok {
			description = d
		}
	}

	id, err := generateID("agt_")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rec := &store.AgentRecord{
		ID:     id,
		UserID: targetUserID,
		Name:   name,
	}
	if description != "" {
		rec.Config = map[string]interface{}{"description": description}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	model := req.Model
	if model == "" && source != nil {
		model = s.agentScopeModel(r, source.ID)
	}
	if model != "" {
		if err := s.saveAgentScopeModel(r, id, model); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "save model: " + err.Error()})
			return
		}
	}

	// Fork 内容：身份文件 + agent 作用域配置。
	if source != nil {
		if err := s.forkAgentContent(r, source, rec); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "fork content: " + err.Error()})
			return
		}
	}

	s.invalidateUser(targetUserID)
	jsonResponse(w, http.StatusCreated, map[string]any{
		"agent": map[string]any{
			"id":          rec.ID,
			"userId":      rec.UserID,
			"name":        rec.Name,
			"description": description,
			"model":       model,
			"isPublic":    rec.IsPublic,
		},
	})
}

// forkAgentFiles 是在分叉期间复制的文件允许列表。这些是 agent 的身份（它是什么/做什么）；
// 每个用户的状态（MEMORY.md, USER.md）被有意省略，以便每个聊天者在新 agent 上从空白开始。
var forkAgentFiles = []string{
	"SOUL.md", "IDENTITY.md", "AGENTS.md",
	"BOOTSTRAP.md", "TOOLS.md", "HEARTBEAT.md", "agent.json",
}

// forkAgentScopeConfigs 是在分叉期间复制的 agent 作用域配置行允许列表。
// 绑定被有意排除 — 它们编码了源拥有者的 IM 路由（bot token、chat id），
// 在不同的拥有者下的新 agent 上它们是没有意义的。
var forkAgentScopeConfigs = map[string]bool{
	"agents.defaults": true,
	"skills.entries":  true,
}

// forkAgentContent 将源 agent 的拥有者行身份文件和 agent 作用域配置复制到目标 agent。
// 每个文件尽力而为：缺失的源文件被静默跳过（目标只是没有它的覆盖，运行时通过通常的回退路径处理）。
func (s *Server) forkAgentContent(r *http.Request, src, dst *store.AgentRecord) error {
	for _, name := range forkAgentFiles {
		data, err := s.dataStore.GetAgentFileExact(r.Context(), src.ID, src.UserID, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			return err
		}
		if len(data) == 0 {
			continue
		}
		if err := s.dataStore.SaveAgentFile(r.Context(), dst.ID, dst.UserID, name, data); err != nil {
			return err
		}
	}
	rows, err := s.dataStore.ListConfigs(r.Context(), store.KindSetting, "", src.ID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if !forkAgentScopeConfigs[row.Name] {
			continue
		}
		if err := scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, dst.ID, row.Name, row.Data); err != nil {
			return err
		}
	}
	return nil
}

// handleCreateUserAPIKey 为路径解析的用户发出 apikey。
// 通过 requireUserOrAdmin 实现管理员或自己：
//   - 管理员调用者可以为任何用户发出 user/agent 密钥
//   - 非管理员调用者只能为自己发出密钥（id == self）
//
// type=admin 始终通过此路径被拒绝 — 管理密钥授予平台范围的权利，
// 不应为目标用户自动配置；需要管理密钥的管理员通过
// POST /api/users/{self}/apikeys 为自己发出一个（这成为自助创建，但路由仍然需要管理员调用者）。
func (s *Server) handleCreateUserAPIKey(w http.ResponseWriter, r *http.Request) {
	targetUserID := r.PathValue("id")
	if !s.requireUserOrAdmin(w, r, targetUserID) {
		return
	}
	if !s.requireWritable(w, r) {
		return
	}
	target, err := s.dataStore.GetUser(r.Context(), targetUserID)
	if err != nil || target == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "user not found"})
		return
	}
	if target.Role == users.RoleAppUser {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "app_user cannot hold api keys"})
		return
	}
	ident, _ := auth.FromContext(r.Context())
	isAdmin := ident.CanAdminPlatform()

	var req createAPIKeyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	if req.Type == "" {
		req.Type = users.APIKeyTypeUser
	}
	if !users.IsAPIKeyType(req.Type) {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid type"})
		return
	}
	if req.Type == users.APIKeyTypeAdmin {
		// 管理密钥从不通过此路径生成 — 它们只能通过 super_admin 为自身
		// 执行 POST /api/users/{self}/apikeys 产生，这仍然会绕过意图
		//（"这是给那个其他用户的平台密钥"）。如果 super_admin 需要为自己获取新的管理密钥，
		// 他们从设置 UI 自助发出；我们不暴露编程式的管理密钥生成。
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "admin keys cannot be issued through this path"})
		return
	}
	if req.Type == users.APIKeyTypeAgent {
		if len(req.AgentIDs) == 0 {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "type=agent requires at least one agentId"})
			return
		}
		for _, aid := range req.AgentIDs {
			rec, err := s.dataStore.GetAgent(r.Context(), aid)
			if err != nil || rec == nil {
				jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agent not found: " + aid})
				return
			}
			// 自助调用者：必须拥有每个 agent。
			// 管理员调用者：目标必须拥有每个 agent（管理员不能将随机用户 A 的 agent 绑定到用户 B 的 apikey）。
			if rec.UserID != targetUserID {
				jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "cannot bind agent " + aid + " — not owned by target user"})
				return
			}
		}
	}
	_ = isAdmin // 当前内部没有仅管理员的路径；保留以供将来开关使用
	ak, token, err := s.apikeys.Create(r.Context(), targetUserID, req.Name, req.Type, req.AgentIDs)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ak.Key = token
	jsonResponse(w, http.StatusCreated, map[string]any{"apikey": ak, "token": token})
}

// --- Apikey CRUD（按用户） ---

type createAPIKeyReq struct {
	Name     string   `json:"name"`
	Type     string   `json:"type,omitempty"` // "admin" | "user" | "agent"; default "agent"
	AgentIDs []string `json:"agentIds,omitempty"`
}

func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleRotateAPIKey(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) handleSetAPIKeyAgents(w http.ResponseWriter, r *http.Request) {
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

// generateID 返回带有给定前缀的随机十六进制 ID。
func generateID(prefix string) (string, error) {
	id, err := newRandID()
	if err != nil {
		return "", err
	}
	return prefix + id, nil
}

// newRandID 在 handlers.go 中实现，以与其他生成器共享。
func init() {
	// 强制编译引用，以便重构时未使用的导入警告保持响亮；否则无操作。
	_ = errors.New
}
