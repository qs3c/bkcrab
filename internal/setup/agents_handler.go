package setup

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/agent/tools"
	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/users"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// AgentsHandler 负责 agent CRUD、per-agent 配置、已注册工具列表。
type AgentsHandler struct {
	dataStore agentsStore
	accounts  *users.Accounts
	guard     *agentGuard
	cfg       *configRepo
	mw        *Middleware
}

// NewAgentsHandler 构造 AgentsHandler。
func NewAgentsHandler(dataStore agentsStore, accounts *users.Accounts, guard *agentGuard, cfg *configRepo, mw *Middleware) *AgentsHandler {
	return &AgentsHandler{dataStore: dataStore, accounts: accounts, guard: guard, cfg: cfg, mw: mw}
}

// RegisterRoutes 注册 agent CRUD 与配置相关路由。
func (s *AgentsHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/api/agents", wrap(s.mw.Auth(s.handleListAgents)))
	r.POST("/api/agents", wrap(s.mw.Auth(s.handleCreateAgent)))
	r.GET("/api/agents/:id", wrap(s.mw.Auth(s.handleGetAgent)))
	r.PUT("/api/agents/:id", wrap(s.mw.Auth(s.handleUpdateAgent)))
	r.DELETE("/api/agents/:id", wrap(s.mw.Auth(s.handleDeleteAgent)))
	r.GET("/api/agents/:id/config", wrap(s.mw.Auth(s.handleGetAgentConfig)))
	r.GET("/api/agents/:id/tools/registered", wrap(s.mw.Auth(s.handleListAgentRegisteredTools)))
}

// agentShareModelConfig 报告 agent 拥有者是否选择与聊天者共享其模型+提供者配置。
// 默认为 true：当 rec.Config 中缺少该键时，共享开启。拥有者通过写入 `false` 显式退出。
// 集中在此处，以便 API 层、运行时覆盖门控（EnsureAgent）和 listProviders 认证放宽
// 使用一致的默认值读取该标志。
func agentShareModelConfig(rec *store.AgentRecord) bool {
	if rec == nil {
		return true
	}
	v, ok := rec.Config["shareModelConfig"].(bool)
	if !ok {
		return true
	}
	return v
}

// agentScopeModel 从 configs 表读取 per-agent 模型覆盖 —
// kind=setting, scope=agent 行，设置后取代系统/用户默认值。
func (s *configRepo) agentScopeModel(r *http.Request, agentID string) string {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "agents.defaults")
	if err != nil || rec == nil {
		return ""
	}
	if v, ok := rec.Data["model"].(string); ok {
		return v
	}
	return ""
}

// saveAgentScopeModel 当 model!="" 时写入（upsert），当 model=="" 时删除 agent 作用域的 agents.defaults 行。
func (s *configRepo) saveAgentScopeModel(r *http.Request, agentID, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", nil)
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", map[string]interface{}{"model": model})
}

// agentScopeDefaultsRead 返回当前 agent 作用域的 agents.defaults 行数据，如果行不存在则返回空 map。
// 调用者将其作为合并感知补丁的基础（读-改-写），以便一个只接触一个字段的 PATCH 不会破坏其余部分。
func (s *configRepo) agentScopeDefaultsRead(r *http.Request, agentID string) map[string]interface{} {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "agents.defaults")
	if err != nil || rec == nil || rec.Data == nil {
		return map[string]interface{}{}
	}
	// 复制一份，以便调用者修改结果时不会意外通过缓存存储对象写回。
	out := make(map[string]interface{}, len(rec.Data))
	for k, v := range rec.Data {
		out[k] = v
	}
	return out
}

// applyAgentScopeDefaultsPatch 将 patch 合并到当前的 agents.defaults 行中并写入结果。
// 值为 nil 的键将从行中删除（调用者的信号"清除此覆盖"）。
// 结果为空的整行被完全移除，以便 MergedAgentConfig 完全回退到系统/用户默认值。
func (s *configRepo) applyAgentScopeDefaultsPatch(r *http.Request, agentID string, patch map[string]interface{}) error {
	if len(patch) == 0 {
		return nil
	}
	data := s.agentScopeDefaultsRead(r, agentID)
	for k, v := range patch {
		if v == nil {
			delete(data, k)
			continue
		}
		data[k] = v
	}
	if len(data) == 0 {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", nil)
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", data)
}

// applyAgentScopePluginsPatch 将 per-agent 插件启用覆盖合并到 (scope=agent, name=plugins.enabled) 行中。
//
// 值为 true/false 的补丁键被写入；行的其余部分被保留
// （这样对一个插件的 UI 切换不会破坏兄弟插件的覆盖）。
// 当 reset 为 true 时，整行被删除 — agent 回退到系统范围的插件启用状态。
func (s *configRepo) applyAgentScopePluginsPatch(r *http.Request, agentID string, patch map[string]bool, reset bool) error {
	if reset {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "plugins.enabled", nil)
	}
	if len(patch) == 0 {
		return nil
	}
	data := map[string]interface{}{}
	if rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "plugins.enabled"); err == nil && rec != nil {
		for k, v := range rec.Data {
			data[k] = v
		}
	}
	for k, v := range patch {
		data[k] = v
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "plugins.enabled", data)
}

// agentScopeSplitReplies 读取 per-agent 多气泡覆盖。
// 当不存在时返回 nil — nil 被每个运行时消费者视为 false，
// 因此区别仅在于 GET 响应（仪表板可以选择将"未设置"与"关闭"渲染不同，
// 但目前 Switch 将两者都渲染为关闭，这没问题）。
func (s *configRepo) agentScopeSplitReplies(r *http.Request, agentID string) *bool {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "agents.defaults")
	if err != nil || rec == nil {
		return nil
	}
	v, ok := rec.Data["splitReplies"].(bool)
	if !ok {
		return nil
	}
	return &v
}

// agentScopePromptMode reads the per-agent promptMode override.
func (s *configRepo) agentScopePromptMode(r *http.Request, agentID string) string {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "agents.defaults")
	if err != nil || rec == nil {
		return ""
	}
	if v, ok := rec.Data["promptMode"].(string); ok {
		return v
	}
	return ""
}

// agentScopePlugins 读取 per-agent 插件启用覆盖层。当没有行存在时返回 nil。
// 键为 pluginID → bool；缺失的键回退到系统范围插件条目的启用状态。
func (s *configRepo) agentScopePlugins(r *http.Request, agentID string) map[string]bool {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "plugins.enabled")
	if err != nil || rec == nil {
		return nil
	}
	out := make(map[string]bool, len(rec.Data))
	for k, v := range rec.Data {
		if b, ok := v.(bool); ok {
			out[k] = b
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// agentScopeAutoPersist 读取 per-agent autoPersist 覆盖。
// 不存在时返回 nil — 与 agentScopeSplitReplies 相同的约定。
// 驱动 runPostTurn AutoPersistMemory 过程（LLM 提炼写入 USER.md / MEMORY.md），
// 这是聊天机器人模式下唯一的聊天者记忆持久化路径。
func (s *configRepo) agentScopeAutoPersist(r *http.Request, agentID string) *bool {
	rec, err := s.dataStore.GetConfigByName(r.Context(), store.KindSetting, "", agentID, "agents.defaults")
	if err != nil || rec == nil {
		return nil
	}
	v, ok := rec.Data["autoPersist"].(bool)
	if !ok {
		return nil
	}
	return &v
}

// effectiveUserID 返回请求的已解析 user_id：调用者自己的 id，或者 — 对于处于 actAs 模式的 super_admin — 被模拟用户的 id。
func effectiveUserID(r *http.Request) string {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	return ident.EffectiveUserID()
}

// requireWritable 如果调用者可以变更则返回 true，否则写入 4xx 响应并返回 false。
func requireWritable(w http.ResponseWriter, r *http.Request) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return false
	}
	if ident.ReadOnly() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "read-only"})
		return false
	}
	return true
}

func (s *AgentsHandler) handleListAgents(w http.ResponseWriter, r *http.Request) {
	uid := effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	// ?all=true 是跨租户视图（取代 /api/admin/agents）。仅限管理员 —
	// 用于平台范围的"Agents"管理员页面，该页面拼接了拥有者的用户名。
	if r.URL.Query().Get("all") == "true" {
		ident, _ := auth.FromContext(r.Context())
		if !ident.CanAdminPlatform() {
			jsonResponse(w, http.StatusForbidden, map[string]any{"error": "all=true requires admin"})
			return
		}
		s.respondAllAgents(w, r)
		return
	}
	owned, err := s.dataStore.ListAgents(r.Context(), uid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(owned))
	for _, ar := range owned {
		desc, _ := ar.Config["description"].(string)
		out = append(out, map[string]any{
			"id":          ar.ID,
			"name":        ar.Name,
			"description": desc,
			"model":       s.cfg.agentScopeModel(r, ar.ID),
			"avatarUrl":   "/api/agents/" + ar.ID + "/files/avatar.png",
			"createdAt":   ar.CreatedAt,
			"userId":      ar.UserID,
			"role":        "owner",
			"isPublic":    ar.IsPublic,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"agents": out})
}

type createAgentRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
}

func (s *AgentsHandler) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	ident, _ := auth.FromContext(r.Context())
	if !ident.CanCreateAgent() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "type=agent api keys cannot create agents"})
		return
	}
	uid := effectiveUserID(r)
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	// 强制每用户 agent 配额。 -1 = 无限制（默认），0 = 禁止自助创建
	//（单租户客户 — 管理员通过 POST /api/users/{id}/agents 为他们配置），
	// N>0 = 最多同时拥有 N 个。管理员路径绕过此检查。
	if u, err := s.dataStore.GetUser(r.Context(), uid); err == nil && u != nil && u.AgentQuota >= 0 {
		owned, err := s.dataStore.ListAgents(r.Context(), uid)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if int64(len(owned)) >= u.AgentQuota {
			jsonResponse(w, http.StatusForbidden, map[string]any{
				"error": fmt.Sprintf("agent quota reached (%d) — contact your admin to provision more", u.AgentQuota),
			})
			return
		}
	}
	id, err := generateID("agt_")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rec := &store.AgentRecord{
		ID:     id,
		UserID: uid,
		Name:   req.Name,
	}
	if req.Description != "" {
		// Description 存在于 agents.config JSON blob 中 — 保持模式稳定，
		// 同时仍然通过 GetAgentConfig 和 agents.config 命名空间设置覆盖层暴露。
		rec.Config = map[string]interface{}{"description": req.Description}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if req.Model != "" {
		if err := s.cfg.saveAgentScopeModel(r, id, req.Model); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	s.guard.invalidateUser(uid)
	jsonResponse(w, http.StatusCreated, map[string]any{
		"agent": map[string]any{
			"id":     rec.ID,
			"userId": rec.UserID,
			"name":   rec.Name,
			"model":  req.Model,
			"config": rec.Config,
		},
	})
}

// requireUserOrAdmin 门控 /api/users/{id}/* 嵌套路由：
//   - 任何调用者可以操作自己（pathUserID == ident.UserID）
//   - super_admin / type=admin apikey 可以操作任何用户
//
// 成功时返回 true；失败时写入 401/403 并返回 false。
// 当操作依赖于路径用户时，调用者仍应验证该用户实际存在。
func requireUserOrAdmin(w http.ResponseWriter, r *http.Request, pathUserID string) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	if pathUserID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "user id required"})
		return false
	}
	if pathUserID == ident.EffectiveUserID() {
		return true
	}
	if ident.CanAdminPlatform() {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
	return false
}

// requireAgentOwner 如果调用者拥有该 agent（或是 super_admin）则返回 agent 记录，
// 否则写入 403/404 并返回 nil。
func (s *agentGuard) requireAgentOwner(w http.ResponseWriter, r *http.Request, agentID string) *store.AgentRecord {
	uid := effectiveUserID(r)
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return nil
	}
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID != uid && ident.Role != users.RoleSuperAdmin {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
		return nil
	}
	return rec
}

// requireAgentReadable 在调用者是拥有者、super_admin、持有 apikey-ACL 授权（CanAccessAgent）、
// 或者 agent 标记为公开且调用者至少是经过认证的会话时允许访问。
// 公开 agent 通过链接共享：任何访问 URL 的已登录用户可以在自己的 user_id 命名空间下聊天，
// 而 agent 的身份（SOUL/IDENTITY/skills）从拥有者的行重用。
// 这是 /api/chat/history 使用的相同门控，因此通过 X-Bkclaw-End-User 集成代理的 app_user
// 请求可以读取他们拥有的会话的工件，而不会在严格的拥有者检查上返回 403。
// callerOwnsAgent 在调用者是 agent 的拥有者、super_admin 或明确作用域到该 agent 的 apikey 时返回 true。
// 与 requireAgentReadable 不同，它不授予公开 agent 的读取者权限 — 由需要区分"浏览所有内容"
// （拥有者）和"限定到自己的会话"（公开 agent 上的外部调用者）的文件作用域代码使用。
// 失败时静默：由调用者决定如何响应。
func (s *agentGuard) callerOwnsAgent(r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return false
	}
	uid := effectiveUserID(r)
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID == uid || ident.Role == users.RoleSuperAdmin {
		return true
	}
	if ident.AuthMethod == "apikey" && ident.CanAccessAgent(agentID) {
		return true
	}
	return false
}

func (s *agentGuard) requireAgentReadable(w http.ResponseWriter, r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return false
	}
	uid := effectiveUserID(r)
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID == uid || ident.Role == users.RoleSuperAdmin {
		return true
	}
	// CanAccessAgent 对 apikey 是硬检查（ACL），但对会话调用者是延迟的"true" —
	// Identity.CanAccessAgent 的注释说明了这一点。仅为 apikey 路径使用它；
	// 对于会话用户，我们必须自己做显式的拥有者/公开检查，否则任何已登录用户
	// 都可以通过 /api/agents/{id} 及其相关端点 GET 另一个用户的私有 agent。
	if ident.AuthMethod == "apikey" && ident.CanAccessAgent(agentID) {
		return true
	}
	if rec.IsPublic && uid != "" {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
	return false
}

func (s *AgentsHandler) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.guard.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	var req struct {
		Name             string  `json:"name,omitempty"`
		Description      *string `json:"description,omitempty"` // ptr so empty-string clears it
		Model            *string `json:"model,omitempty"`       // ptr so empty-string clears the agent-scope override
		IsPublic         *bool   `json:"isPublic,omitempty"`    // ptr so caller can leave it unchanged
		ShareModelConfig *bool   `json:"shareModelConfig,omitempty"`
		// PromptMode 使用指针，以便调用者可以区分"保持不变"（省略/null）和"清除覆盖"（空字符串）。
		// 允许的字符串值："agent" | "chatbot" | "customize" — 空字符串回退到系统默认值（"agent"）。
		// PromptMode 也驱动内置工具表面；设计上没有单独的 allowlist 字段（通过插件扩展）。
		PromptMode *string `json:"promptMode,omitempty"`
		// SplitReplies per-agent 覆盖：nil = 不变，
		// 非 nil 的 bool 指针 = 设置显式值（true/false）。
		// 与"清除"不同，后者是一个单独的信号 — 仪表板发送 `splitRepliesReset: true` 来删除覆盖并回退到系统默认值。
		SplitReplies      *bool `json:"splitReplies,omitempty"`
		SplitRepliesReset bool  `json:"splitRepliesReset,omitempty"`
		// AutoPersist per-agent 覆盖 — 与 SplitReplies 相同的语义。
		// `autoPersistReset:true` 清除覆盖并回退到系统默认值（目前为禁用）。
		AutoPersist      *bool `json:"autoPersist,omitempty"`
		AutoPersistReset bool  `json:"autoPersistReset,omitempty"`
		// 每个 agent 的插件启用覆盖层。键是插件 ID，值为 bool。
		// 补丁语义：只写入此 map 中存在的键；现有行中的其他键被保留。
		// 要清除此 agent 的所有覆盖，发送 pluginsReset:true。
		Plugins      map[string]bool `json:"plugins,omitempty"`
		PluginsReset bool            `json:"pluginsReset,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name != "" {
		rec.Name = req.Name
	}
	if req.Description != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if *req.Description == "" {
			delete(rec.Config, "description")
		} else {
			rec.Config["description"] = *req.Description
		}
	}
	if req.IsPublic != nil {
		rec.IsPublic = *req.IsPublic
	}
	// shareModelConfig 控制使用此 agent 的聊天者是否继承拥有者的模型+提供者配置。
	// 默认为 true：除非拥有者显式退出，否则共享开启。
	// 编码：缺少键 = 开启（新 agent 的默认值），显式 `false` = 退出。
	// 我们从不为 true 存储值 — 存储键的缺失可使现有行保持最小，并意味着未来的默认值翻转
	// 只需在一个地方更改（上面的 agentShareModelConfig）。存储在 agent 的 config blob 中，
	// 这样不需要模式迁移；运行时的 EnsureAgent 读取它以门控拥有者回退和 agent 作用域覆盖层。
	if req.ShareModelConfig != nil {
		if rec.Config == nil {
			rec.Config = map[string]interface{}{}
		}
		if *req.ShareModelConfig {
			delete(rec.Config, "shareModelConfig")
		} else {
			rec.Config["shareModelConfig"] = false
		}
	}
	if err := s.dataStore.SaveAgent(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Per-agent 默认值存储在一个 configs 行中（kind=setting, scope=agent, namespace=agents.defaults）。
	// 将调用者触及的每个字段收集到一个合并感知的补丁中，这样例如更新 promptMode 不会破坏
	// 现有的 model 覆盖，反之亦然。nil 指针 = 调用者未触及该字段；指向空值的指针 = "清除此覆盖"。
	defaultsPatch := map[string]interface{}{}
	if req.Model != nil {
		m := strings.TrimSpace(*req.Model)
		if m == "" {
			defaultsPatch["model"] = nil
		} else {
			defaultsPatch["model"] = m
		}
	}
	if req.PromptMode != nil {
		pm := strings.TrimSpace(*req.PromptMode)
		// Allow only the documented values plus empty (= clear).
		// Anything else is a 400 — silently coercing to "agent" would
		// mask typos from the dashboard or CLI.
		switch pm {
		case "":
			defaultsPatch["promptMode"] = nil
		case config.PromptModeAgent, config.PromptModeChatbot, config.PromptModeCustomize:
			defaultsPatch["promptMode"] = pm
		default:
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "promptMode must be one of: agent, chatbot, customize"})
			return
		}
	}
	if req.SplitRepliesReset {
		// 重置在同一次请求中优先于设置 — 仪表板的"Inherit"选项写入此标志。
		defaultsPatch["splitReplies"] = nil
	} else if req.SplitReplies != nil {
		defaultsPatch["splitReplies"] = *req.SplitReplies
	}
	if req.AutoPersistReset {
		defaultsPatch["autoPersist"] = nil
	} else if req.AutoPersist != nil {
		defaultsPatch["autoPersist"] = *req.AutoPersist
	}
	if err := s.cfg.applyAgentScopeDefaultsPatch(r, rec.ID, defaultsPatch); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 插件启用覆盖层：单独的配置行（scope=agent, name=plugins.enabled），因此不经过 agents.defaults
	// 补丁路径。Reset 完全清除该行；否则我们将传入的 map 键合并到现有数据中。
	if req.PluginsReset || req.Plugins != nil {
		if err := s.cfg.applyAgentScopePluginsPatch(r, rec.ID, req.Plugins, req.PluginsReset); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// 使用 invalidateAgent（而不是 invalidateUser），这样 super_admin / 公开链接查看者 / apikey 调用者
	// 通过延迟附加将此 agent 加载到自己的 UserSpace 中时，也会丢弃其过时的 rc.Model —
	// 没有这个，他们会继续使用以前的模型，直到 30 分钟的空闲驱逐。
	s.guard.invalidateAgent(rec.ID)
	share := agentShareModelConfig(rec)
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":               rec.ID,
			"userId":           rec.UserID,
			"name":             rec.Name,
			"model":            s.cfg.agentScopeModel(r, rec.ID),
			"promptMode":       s.cfg.agentScopePromptMode(r, rec.ID),
			"splitReplies":     s.cfg.agentScopeSplitReplies(r, rec.ID),
			"autoPersist":      s.cfg.agentScopeAutoPersist(r, rec.ID),
			"plugins":          s.cfg.agentScopePlugins(r, rec.ID),
			"config":           rec.Config,
			"isPublic":         rec.IsPublic,
			"shareModelConfig": share,
		},
	})
}

// handleGetAgent 返回单个 agent 的基本 AgentRecord（id, name, description, userId）。
// 由聊天头部/侧边栏切换器用于解析显示名称。权限为读取级别 — 拥有者、super_admin
// 或共享记录的任何受让人。
func (s *AgentsHandler) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	desc, _ := rec.Config["description"].(string)
	share := agentShareModelConfig(rec)
	uid := effectiveUserID(r)
	role := "owner"
	if rec.UserID != uid {
		role = "viewer"
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":               rec.ID,
			"name":             rec.Name,
			"description":      desc,
			"userId":           rec.UserID,
			"role":             role,
			"model":            s.cfg.agentScopeModel(r, rec.ID),
			"promptMode":       s.cfg.agentScopePromptMode(r, rec.ID),
			"splitReplies":     s.cfg.agentScopeSplitReplies(r, rec.ID),
			"autoPersist":      s.cfg.agentScopeAutoPersist(r, rec.ID),
			"plugins":          s.cfg.agentScopePlugins(r, rec.ID),
			"avatarUrl":        "/api/agents/" + rec.ID + "/files/avatar.png",
			"createdAt":        rec.CreatedAt,
			"isPublic":         rec.IsPublic,
			"shareModelConfig": share,
		},
	})
}

func (s *AgentsHandler) handleGetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.guard.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	cfg := config.AgentFileConfig{}
	if len(rec.Config) > 0 {
		blob, _ := json.Marshal(rec.Config)
		_ = json.Unmarshal(blob, &cfg)
	}
	jsonResponse(w, http.StatusOK, cfg)
}

func (s *AgentsHandler) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.guard.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if err := s.dataStore.DeleteAgent(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 从每个缓存的 UserSpace 中删除 agent，而不仅仅是拥有者的，
	// 这样外部调用者停止通过 EnsureAgent 的延迟附加路径解析已删除的 agent。
	s.guard.invalidateAgent(rec.ID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// invalidateUser 丢弃用户延迟加载的 UserSpace，以便下次访问时从数据库重新加载。
// gateway 在 api.UserResolver 接口后面实现 InvalidateUser。
func (s *agentGuard) invalidateUser(userID string) {
	if userID == "" || s.userResolver == nil {
		return
	}
	if r, ok := s.userResolver.(interface{ InvalidateUser(string) }); ok {
		r.InvalidateUser(userID)
	}
	slog.Debug("invalidated user space", "user", userID)
}

// invalidateAgent 丢弃每个持有此 agent 的缓存 UserSpace —
// 拥有者以及通过 EnsureAgent 延迟附加的任何外部调用者（super_admin 聊天、公开链接查看者、apikey 用户）。
// 在改变 agent 解析的运行时（agents.defaults、agent-scope providers）的写入后使用；
// 纯用户作用域的写入可以继续使用 invalidateUser。
func (s *agentGuard) invalidateAgent(agentID string) {
	if agentID == "" || s.userResolver == nil {
		return
	}
	if r, ok := s.userResolver.(interface{ InvalidateAgent(string) }); ok {
		r.InvalidateAgent(agentID)
	}
	slog.Debug("invalidated user spaces holding agent", "agent", agentID)
}

// requireOwnerOrSuperAdmin 门控变更另一个用户资源的端点。
func requireOwnerOrSuperAdmin(w http.ResponseWriter, r *http.Request, ownerID string) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return false
	}
	if ident.UserID == ownerID || ident.Role == users.RoleSuperAdmin {
		return true
	}
	jsonResponse(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
	return false
}

var _ workspace.Store = (workspace.Store)(nil)

// handleListAgentRegisteredTools 返回指定 agent 的实时工具注册表。
// 驱动 Tools 标签页的允许列表复选框选择器 — 操作员点击而不是从记忆中键入工具名称。
//
// 权限为读取级别（拥有者 / super_admin / 共享链接查看者）而不是仅限拥有者，
// 因为查看者可能想看看他们可以访问什么，即使他们不能更改允许列表。PUT 路径保持拥有者门控。
func (s *AgentsHandler) handleListAgentRegisteredTools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	ag := s.guard.resolveAgent(r, id)
	if ag == nil {
		// Agent 未在调用者的 UserSpace 中加载，延迟附加也失败了。
		// 我们可以回退到 DB 记录，但此端点的全部意义在于实时注册表
		//（MCP 工具只有在 agent 附加后才存在），因此返回 404 是诚实的，
		// 而不是误导性地仅返回内置工具。
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not loaded"})
		return
	}
	toolList := ag.RegisteredTools()
	if toolList == nil {
		toolList = []tools.ToolInfo{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"tools": toolList})
}

// respondAllAgents 返回每个用户的所有 agent，附带拥有者的用户名/电子邮件。
// 为平台范围的管理视图支持 GET /api/agents?all=true；认证门控在 handleListAgents 中
// （仅在 CanAdminPlatform 通过后调用此函数）。
func (s *AgentsHandler) respondAllAgents(w http.ResponseWriter, r *http.Request) {
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
