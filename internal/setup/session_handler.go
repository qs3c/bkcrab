package setup

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/api"
	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/buildinfo"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/users"
)

// SessionHandler 负责引导、登录/会话、当前用户账户、注册、
// 提供者连通性测试，以及当前用户的系统配置。
// 方法体跨 session_handler.go（登录/账户/注册）与 helpers.go 历史拆分的若干段，
// 这里集中持有 SessionHandler 的全部方法。
type SessionHandler struct {
	accounts     *users.Accounts
	authResolver *auth.Resolver
	dataStore    agentConfigStore
	userResolver api.UserResolver
	port         int
	startedAt    time.Time
	guard        *agentGuard
	cfg          *configRepo
	mw           *Middleware
}

// NewSessionHandler 构造 SessionHandler。
func NewSessionHandler(accounts *users.Accounts, authResolver *auth.Resolver, dataStore agentConfigStore, userResolver api.UserResolver, port int, startedAt time.Time, guard *agentGuard, cfg *configRepo, mw *Middleware) *SessionHandler {
	return &SessionHandler{accounts: accounts, authResolver: authResolver, dataStore: dataStore, userResolver: userResolver, port: port, startedAt: startedAt, guard: guard, cfg: cfg, mw: mw}
}

// RegisterRoutes 注册 session、登录、账户、配置相关路由。
func (s *SessionHandler) RegisterRoutes(r *gin.Engine) {
	// 引导 / 登录
	r.GET("/api/status", wrap(s.mw.Opt(s.handleStatus)))
	r.POST("/api/login", wrap(s.handleLogin))
	r.POST("/api/logout", wrap(s.mw.Auth(s.handleLogout)))
	r.POST("/api/onboard", wrap(s.handleOnboard))
	r.POST("/api/register", wrap(s.handleRegister))

	// 当前登录用户自身
	r.GET("/api/me", wrap(s.mw.Auth(s.handleMe)))
	r.PUT("/api/me", wrap(s.mw.Auth(s.handleUpdateMe)))
	r.POST("/api/me/password", wrap(s.mw.Auth(s.handleChangeMyPassword)))

	// 提供者连通性测试
	r.POST("/api/test-provider", wrap(s.mw.Opt(s.handleTestProvider)))
	r.POST("/api/providers/:id/test", wrap(s.mw.Auth(s.handleTestStoredProvider)))

	// 管理员：注册策略
	r.GET("/api/admin/registration", wrap(s.mw.Admin(s.handleGetRegistration)))
	r.PUT("/api/admin/registration", wrap(s.mw.Admin(s.handleSetRegistration)))

	// 当前用户的系统配置
	r.GET("/api/config", wrap(s.mw.Auth(s.handleGetConfig)))
	r.POST("/api/config", wrap(s.mw.Auth(s.handleUpdateConfig)))
}

// --- 登录/登出/我的信息 ---

type loginRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

func (s *SessionHandler) handleLogin(w http.ResponseWriter, r *http.Request) {
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

func (s *SessionHandler) handleLogout(w http.ResponseWriter, r *http.Request) {
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

func (s *SessionHandler) handleMe(w http.ResponseWriter, r *http.Request) {
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
func (s *SessionHandler) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
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
func (s *SessionHandler) handleChangeMyPassword(w http.ResponseWriter, r *http.Request) {
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
func (s *SessionHandler) handleOnboard(w http.ResponseWriter, r *http.Request) {
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

func (s *SessionHandler) handleStatus(w http.ResponseWriter, r *http.Request) {
	configured := false
	if s.accounts != nil {
		if n, err := s.accounts.Count(r.Context()); err == nil && n > 0 {
			configured = true
		}
	}
	resp := map[string]any{
		"configured":       configured,
		"registrationOpen": s.cfg.registrationOpen(r),
		"running":          s.userResolver != nil,
		"port":             s.port,
		"version":          buildinfo.Version,
		"agents":           []any{},
		"channels":         []any{},
		"provider":         nil,
		"uptime":           formatDuration(time.Since(s.startedAt)),
	}
	ident, authed := auth.FromContext(r.Context())
	if !authed {
		jsonResponse(w, http.StatusOK, resp)
		return
	}
	resp["userId"] = ident.UserID
	resp["role"] = ident.Role
	resp["isAdmin"] = ident.Role == "super_admin"
	if resp["isAdmin"].(bool) && s.accounts != nil {
		if n, err := s.accounts.Count(r.Context()); err == nil {
			resp["users"] = n
		}
	}

	if !configured {
		jsonResponse(w, http.StatusOK, resp)
		return
	}
	cfg, err := s.cfg.loadUserConfig(r)
	if err == nil {
		// 选择实际支持默认模型的 provider。模型 ID 是 "<providerName>/<modelID>"
		//（在第一个斜杠处分割 — modelID 本身可以包含斜杠，例如 "openrouter/xiaomi/mimo-v2-flash"）。
		// 回退到"map 中的第一个 provider"会产生不匹配的面板，
		// 其中标题说一个 provider 但默认模型属于另一个。
		defaultModel := cfg.Agents.Defaults.Model
		var provName string
		if i := strings.IndexByte(defaultModel, '/'); i > 0 {
			provName = defaultModel[:i]
		}
		if prov, ok := cfg.Providers[provName]; ok {
			resp["provider"] = map[string]string{
				"name":    provName,
				"model":   defaultModel,
				"apiBase": prov.APIBase,
				"apiKey":  maskAPIKey(prov.APIKey),
			}
		} else {
			for name, prov := range cfg.Providers {
				resp["provider"] = map[string]string{
					"name":    name,
					"model":   defaultModel,
					"apiBase": prov.APIBase,
					"apiKey":  maskAPIKey(prov.APIKey),
				}
				break
			}
		}
		var chs []map[string]string
		for chType, ch := range cfg.Channels {
			if !ch.Enabled {
				continue
			}
			chs = append(chs, map[string]string{"type": chType})
		}
		if len(chs) > 0 {
			resp["channels"] = chs
		}
	}
	allAgents := s.guard.resolveAllAgents(r)
	if len(allAgents) > 0 {
		var agentList []map[string]string
		for _, ag := range allAgents {
			id := ag.Name() // AgentHandle.Name() 返回 agent id
			entry := map[string]string{"id": id}
			// 从 agents 行中显示对人类友好的名称，以便仪表板列表显示
			// "default" / "ImgAny" 而不是 "agt_…"。查找失败时回退到仅 ID，
			// 这样短暂的存储错误不会导致面板变黑。
			if s.dataStore != nil {
				if rec, _ := s.dataStore.GetAgent(r.Context(), id); rec != nil && rec.Name != "" {
					entry["name"] = rec.Name
				}
			}
			agentList = append(agentList, entry)
		}
		resp["agents"] = agentList
	}
	jsonResponse(w, http.StatusOK, resp)
}

// --- /api/config (GET / POST) ---

func (s *SessionHandler) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.cfg.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	masked := *cfg
	masked.Providers = make(map[string]config.ProviderConfig)
	for k, v := range cfg.Providers {
		v.APIKey = maskAPIKey(v.APIKey)
		masked.Providers[k] = v
	}
	if len(cfg.Skills.Entries) > 0 {
		me := make(map[string]config.SkillEntryCfg, len(cfg.Skills.Entries))
		for k, v := range cfg.Skills.Entries {
			me[k] = maskSkillEntry(v)
		}
		masked.Skills.Entries = me
	}
	if len(cfg.Skills.AgentEntries) > 0 {
		ma := make(map[string]map[string]config.SkillEntryCfg, len(cfg.Skills.AgentEntries))
		for aid, inner := range cfg.Skills.AgentEntries {
			out := make(map[string]config.SkillEntryCfg, len(inner))
			for k, v := range inner {
				out[k] = maskSkillEntry(v)
			}
			ma[aid] = out
		}
		masked.Skills.AgentEntries = ma
	}
	// 计算 system-only 解析的 agents.defaults，以便仪表板可以区分
	// "从 system 继承"与"在我的用户作用域覆盖" — `cfg` 已经合并了 user over system，
	// 因此没有这个提示，UI 会在两种情况下看到相同的值，无法渲染 Inheriting/Override 徽章。
	sysDefaults := config.AgentsConfig{}.Defaults
	if s.dataStore != nil {
		_ = scope.SettingInto(r.Context(), s.dataStore, "agents.defaults", "", "", &sysDefaults)
	}
	// Marshal-then-extend 保持响应形状兼容（现有调用者忽略额外的 `meta` 键），
	// 而无需强制重构 config.Config 以携带展示元数据。
	blob, _ := json.Marshal(masked)
	out := map[string]any{}
	_ = json.Unmarshal(blob, &out)
	out["meta"] = map[string]any{
		"systemDefaultModel": sysDefaults.Model,
	}
	jsonResponse(w, http.StatusOK, out)
}

func (s *SessionHandler) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	ident, ok := auth.FromContext(r.Context())
	if !ok || ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return
	}
	// PATCH 语义：加载现有 cfg，然后将请求解码到其中。
	// Go 的 json.Unmarshal 会保持 JSON 中不存在的结构体字段和映射条目不变，
	// 因此仅 POST `{"sandbox":{...}}` 的 /settings 不再通过 saveUserConfig 的命名空间扫描擦除 agents.defaults / skills.* / 每个其他命名空间。
	buf, err := io.ReadAll(r.Body)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	merged, err := s.cfg.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 避免将过时的每个 agent 技能条目合并回已保存的状态 —
	// 这些通过下面的每个 agent 循环持久化，而不是通过命名空间扫描，
	// 在此处重写它们会重建我们刚刚拆分开的旧形状。
	merged.Skills.AgentEntries = nil
	if err := json.Unmarshal(buf, merged); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if err := s.cfg.saveUserConfig(r, merged); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 每个 agent 的技能 env 覆盖（每个 agent 一行，scope=agent，name=skills.entries）。
	// 从原始 body 中拉取 — 而不是从合并的 Config 中 — 这样我们只触及调用者实际修补的 agent，
	// 不会将每个现有覆盖作为写入回显。
	var raw struct {
		Skills *struct {
			AgentEntries map[string]map[string]config.SkillEntryCfg `json:"agentEntries"`
		} `json:"skills"`
	}
	_ = json.Unmarshal(buf, &raw)
	if raw.Skills != nil && raw.Skills.AgentEntries != nil {
		for agentID, entries := range raw.Skills.AgentEntries {
			rec, err := s.dataStore.GetAgent(r.Context(), agentID)
			if err != nil || rec == nil {
				jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent not found: " + agentID})
				return
			}
			if !s.cfg.authorizeScope(w, r, scope.Agent, agentID, scopeWrite) {
				return
			}
			if err := saveAgentSkillEntries(r.Context(), s.dataStore, agentID, entries); err != nil {
				jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
				return
			}
		}
	}
	// 缓存的 UserSpaces 保存了合并配置的快照（包括 agents.defaults.model 和 provider 链）。
	// 没有这步操作，在变更前加载的 agent 会一直看到过时的模型并在聊天中显示"no usable LLM provider"。
	sc, scopeID := s.cfg.scopeForSave(r)
	s.guard.invalidateScope(sc, scopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

type testProviderRequest struct {
	APIBase  string `json:"apiBase"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	APIType  string `json:"apiType"`
	AuthType string `json:"authType"`
}

func (s *SessionHandler) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var req testProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}
	jsonResponse(w, http.StatusOK, runProviderTest(r.Context(), req))
}

// handleTestStoredProvider 运行相同的连接检查，但从已保存的 provider 行中读取
// apiKey + apiBase + apiType + authType，而不是从请求体中获取。
// 让编辑对话框针对存储的密钥进行测试，这样用户不必在每次编辑时重新粘贴密钥。
func (s *SessionHandler) handleTestStoredProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	// 测试 = 读取等效：任何可以读取该行的用户都可以验证它是否有效。
	// 他们无论如何都会通过其 agent 运行时使用它，因此仪表板端的试运行不应更严格。
	if !s.cfg.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeRead) {
		return
	}
	// 浏览器永远不会收到未掩码的 API 密钥，因此它通过存储的行保留在服务器端。
	// 但其他所有内容（apiBase、apiType、authType）在表单中可自由编辑，
	// 用户期望 Test 执行*他们输入的内容*，而不是保存的行 —
	// 否则调整 URL 并单击 Test 会静默地重新 ping 旧 URL 并报告绿色。
	// 当某个字段被省略时，采用客户端发送的任何覆盖；回退到存储的值。
	var body struct {
		Model    string  `json:"model"`
		APIBase  *string `json:"apiBase,omitempty"`
		APIType  *string `json:"apiType,omitempty"`
		AuthType *string `json:"authType,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	pc := config.ProviderConfig{}
	if blob, err := json.Marshal(rec.Data); err == nil {
		_ = json.Unmarshal(blob, &pc)
	}
	apiBase := pc.APIBase
	if body.APIBase != nil {
		apiBase = *body.APIBase
	}
	apiType := pc.APIType
	if body.APIType != nil {
		apiType = *body.APIType
	}
	authType := pc.AuthType
	if body.AuthType != nil {
		authType = *body.AuthType
	}
	jsonResponse(w, http.StatusOK, runProviderTest(r.Context(), testProviderRequest{
		APIBase:  apiBase,
		APIKey:   pc.APIKey,
		Model:    body.Model,
		APIType:  apiType,
		AuthType: authType,
	}))
}
