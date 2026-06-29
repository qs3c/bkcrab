package setup

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/buildinfo"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
	"github.com/qs3c/bkcrab/internal/workspace"
)

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
func (s *Server) agentScopeModel(r *http.Request, agentID string) string {
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
func (s *Server) saveAgentScopeModel(r *http.Request, agentID, model string) error {
	model = strings.TrimSpace(model)
	if model == "" {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", nil)
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, scope.Agent, agentID, "agents.defaults", map[string]interface{}{"model": model})
}

// agentScopeDefaultsRead 返回当前 agent 作用域的 agents.defaults 行数据，如果行不存在则返回空 map。
// 调用者将其作为合并感知补丁的基础（读-改-写），以便一个只接触一个字段的 PATCH 不会破坏其余部分。
func (s *Server) agentScopeDefaultsRead(r *http.Request, agentID string) map[string]interface{} {
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
func (s *Server) applyAgentScopeDefaultsPatch(r *http.Request, agentID string, patch map[string]interface{}) error {
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
func (s *Server) applyAgentScopePluginsPatch(r *http.Request, agentID string, patch map[string]bool, reset bool) error {
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
func (s *Server) agentScopeSplitReplies(r *http.Request, agentID string) *bool {
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
func (s *Server) agentScopePromptMode(r *http.Request, agentID string) string {
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
func (s *Server) agentScopePlugins(r *http.Request, agentID string) map[string]bool {
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
func (s *Server) agentScopeAutoPersist(r *http.Request, agentID string) *bool {
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
func (s *Server) effectiveUserID(r *http.Request) string {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	return ident.EffectiveUserID()
}

// requireWritable 如果调用者可以变更则返回 true，否则写入 4xx 响应并返回 false。
func (s *Server) requireWritable(w http.ResponseWriter, r *http.Request) bool {
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

func (s *Server) handleListAgents(w http.ResponseWriter, r *http.Request) {
	uid := s.effectiveUserID(r)
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
			"model":       s.agentScopeModel(r, ar.ID),
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

func (s *Server) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	ident, _ := auth.FromContext(r.Context())
	if !ident.CanCreateAgent() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "type=agent api keys cannot create agents"})
		return
	}
	uid := s.effectiveUserID(r)
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
		if err := s.saveAgentScopeModel(r, id, req.Model); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	s.invalidateUser(uid)
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
func (s *Server) requireUserOrAdmin(w http.ResponseWriter, r *http.Request, pathUserID string) bool {
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
func (s *Server) requireAgentOwner(w http.ResponseWriter, r *http.Request, agentID string) *store.AgentRecord {
	uid := s.effectiveUserID(r)
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
// 这是 /api/chat/history 使用的相同门控，因此通过 X-Bkcrab-End-User 集成代理的 app_user
// 请求可以读取他们拥有的会话的工件，而不会在严格的拥有者检查上返回 403。
// callerOwnsAgent 在调用者是 agent 的拥有者、super_admin 或明确作用域到该 agent 的 apikey 时返回 true。
// 与 requireAgentReadable 不同，它不授予公开 agent 的读取者权限 — 由需要区分"浏览所有内容"
// （拥有者）和"限定到自己的会话"（公开 agent 上的外部调用者）的文件作用域代码使用。
// 失败时静默：由调用者决定如何响应。
func (s *Server) callerOwnsAgent(r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		return false
	}
	uid := s.effectiveUserID(r)
	ident, _ := auth.FromContext(r.Context())
	if rec.UserID == uid || ident.Role == users.RoleSuperAdmin {
		return true
	}
	if ident.AuthMethod == "apikey" && ident.CanAccessAgent(agentID) {
		return true
	}
	return false
}

func (s *Server) requireAgentReadable(w http.ResponseWriter, r *http.Request, agentID string) bool {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return false
	}
	uid := s.effectiveUserID(r)
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

func (s *Server) handleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
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
	if err := s.applyAgentScopeDefaultsPatch(r, rec.ID, defaultsPatch); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 插件启用覆盖层：单独的配置行（scope=agent, name=plugins.enabled），因此不经过 agents.defaults
	// 补丁路径。Reset 完全清除该行；否则我们将传入的 map 键合并到现有数据中。
	if req.PluginsReset || req.Plugins != nil {
		if err := s.applyAgentScopePluginsPatch(r, rec.ID, req.Plugins, req.PluginsReset); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	// 使用 invalidateAgent（而不是 invalidateUser），这样 super_admin / 公开链接查看者 / apikey 调用者
	// 通过延迟附加将此 agent 加载到自己的 UserSpace 中时，也会丢弃其过时的 rc.Model —
	// 没有这个，他们会继续使用以前的模型，直到 30 分钟的空闲驱逐。
	s.invalidateAgent(rec.ID)
	share := agentShareModelConfig(rec)
	jsonResponse(w, http.StatusOK, map[string]any{
		"agent": map[string]any{
			"id":               rec.ID,
			"userId":           rec.UserID,
			"name":             rec.Name,
			"model":            s.agentScopeModel(r, rec.ID),
			"promptMode":       s.agentScopePromptMode(r, rec.ID),
			"splitReplies":     s.agentScopeSplitReplies(r, rec.ID),
			"autoPersist":      s.agentScopeAutoPersist(r, rec.ID),
			"plugins":          s.agentScopePlugins(r, rec.ID),
			"config":           rec.Config,
			"isPublic":         rec.IsPublic,
			"shareModelConfig": share,
		},
	})
}

// handleGetAgent 返回单个 agent 的基本 AgentRecord（id, name, description, userId）。
// 由聊天头部/侧边栏切换器用于解析显示名称。权限为读取级别 — 拥有者、super_admin
// 或共享记录的任何受让人。
func (s *Server) handleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	desc, _ := rec.Config["description"].(string)
	share := agentShareModelConfig(rec)
	uid := s.effectiveUserID(r)
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
			"model":            s.agentScopeModel(r, rec.ID),
			"promptMode":       s.agentScopePromptMode(r, rec.ID),
			"splitReplies":     s.agentScopeSplitReplies(r, rec.ID),
			"autoPersist":      s.agentScopeAutoPersist(r, rec.ID),
			"plugins":          s.agentScopePlugins(r, rec.ID),
			"avatarUrl":        "/api/agents/" + rec.ID + "/files/avatar.png",
			"createdAt":        rec.CreatedAt,
			"isPublic":         rec.IsPublic,
			"shareModelConfig": share,
		},
	})
}

func (s *Server) handleGetAgentConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
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

func (s *Server) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if err := s.dataStore.DeleteAgent(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// 从每个缓存的 UserSpace 中删除 agent，而不仅仅是拥有者的，
	// 这样外部调用者停止通过 EnsureAgent 的延迟附加路径解析已删除的 agent。
	s.invalidateAgent(rec.ID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// Agent identity / memory files 文件 — 都存在于 agent_files 中，按 agent 作用域划分。
// 两个类别：
//
//   - identity 文件（下面的 agentIdentityFiles）是 agent 的规范"共享模板"。
//     它们存在于由 agent 拥有者的 user_id 键控的单行中 — 因此管理员配置、拥有者的编辑、
//     以及 agent 自己的 BOOTSTRAP 流程中的 write_file 调用都汇聚到同一行。
//     镜像 handlers_admin.forkAgentFiles 和 internal/agent/tools.identityFiles；
//     保持这三个列表同步。
//
//   - per-user 文件（USER.md, MEMORY.md）是真正因每个聊天者而不同的状态。
//     它们由调用者的有效 user_id 键控；非拥有者调用者可以编写自己的覆盖，
//     读取路径在不存在时回退到拥有者的行。
//
// 文件名允许列表门控此端点可以触及的文件；agent 运行时工具调用通过 workspace store 进行。
var agentSystemFileAllowlist = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "MEMORY.md": true,
	"HEARTBEAT.md": true, "USER.md": true, "agent.json": true,
}

var agentIdentityFiles = map[string]bool{
	"SOUL.md": true, "IDENTITY.md": true, "AGENTS.md": true,
	"BOOTSTRAP.md": true, "TOOLS.md": true, "HEARTBEAT.md": true,
	"agent.json": true,
}

func (s *Server) handleGetAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	rec, err := s.dataStore.GetAgent(r.Context(), id)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	caller := s.effectiveUserID(r)

	// Identity 文件：直接读取拥有者的行 — 这是唯一的事实来源，无论谁在询问。
	if agentIdentityFiles[name] {
		data, err := s.dataStore.GetAgentFileExact(r.Context(), id, rec.UserID, name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				jsonResponse(w, http.StatusOK, map[string]any{"content": "", "source": "default"})
				return
			}
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		jsonResponse(w, http.StatusOK, map[string]any{"content": string(data), "source": "owner"})
		return
	}

	// Per-user 文件：优先使用调用者自己的行，回退到拥有者的行。
	// `source: "db"` 表示调用者已编写了覆盖；"owner" 表示我们通过回退显示
	// agent 拥有者的行。前端用此决定是否显示"已编辑"徽章并启用还原操作。
	if data, err := s.dataStore.GetAgentFileExact(r.Context(), id, caller, name); err == nil {
		baseContent := ""
		if rec.UserID != caller {
			if base, err2 := s.dataStore.GetAgentFileExact(r.Context(), id, rec.UserID, name); err2 == nil {
				baseContent = string(base)
			}
		}
		resp := map[string]any{"content": string(data), "source": "db"}
		if baseContent != "" {
			resp["baseContent"] = baseContent
		}
		jsonResponse(w, http.StatusOK, resp)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if rec.UserID != caller {
		if data, err := s.dataStore.GetAgentFileExact(r.Context(), id, rec.UserID, name); err == nil {
			jsonResponse(w, http.StatusOK, map[string]any{"content": string(data), "source": "owner"})
			return
		} else if !errors.Is(err, store.ErrNotFound) {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"content": "", "source": "default"})
}

func (s *Server) handlePutAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	target, ok := s.resolveSystemFileTarget(w, r, id, name)
	if !ok {
		return
	}
	if err := s.dataStore.SaveAgentFile(r.Context(), id, target, name, []byte(body.Content)); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateUser(target)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteAgentSystemFile(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	name := r.PathValue("name")
	if !agentSystemFileAllowlist[name] {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "filename not allowed"})
		return
	}
	target, ok := s.resolveSystemFileTarget(w, r, id, name)
	if !ok {
		return
	}
	if err := s.dataStore.DeleteAgentFile(r.Context(), id, target, name); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateUser(target)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// resolveSystemFileTarget 确定对 (agentID, filename) 的写入/删除应影响哪个 user_id 行，并门控访问：
//
//   - Identity 文件（SOUL/IDENTITY/AGENTS/BOOTSTRAP/TOOLS/HEARTBEAT/agent.json）
//     始终以 agent 拥有者的行为目标 — 这是规范的"共享模板"。调用者必须是拥有者
//     或持有平台管理员权限（super_admin 会话或 type=admin apikey）。
//   - Per-user 文件（USER.md, MEMORY.md）以调用者自己的行为目标，
//     因此每个聊天者都有独立的覆盖。调用者只需要对 agent 的读取权限。
//
// 写入 4xx 并在权限/查找失败时返回 ok=false。
func (s *Server) resolveSystemFileTarget(w http.ResponseWriter, r *http.Request, agentID, name string) (string, bool) {
	rec, err := s.dataStore.GetAgent(r.Context(), agentID)
	if err != nil || rec == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return "", false
	}
	caller := s.effectiveUserID(r)
	ident, _ := auth.FromContext(r.Context())
	if agentIdentityFiles[name] {
		if rec.UserID != caller && !ident.CanAdminPlatform() {
			jsonResponse(w, http.StatusForbidden, map[string]any{"error": "not your agent"})
			return "", false
		}
		return rec.UserID, true
	}
	if !s.requireAgentReadable(w, r, agentID) {
		return "", false
	}
	return caller, true
}

// Workspace 文件 — 列出/获取/上传 agent 产生的工件。
// 由 workspace.Store blob 后端支持，其布局为
//
//   workspaces/<agent_id>/<session_id>/<path>
//
// 下面的 HTTP 文件端点操作在 agent 根级别（sessionID=""）—
// 那就是上传的目标位置，ListByAgent 返回该 agent 每个会话的对象。
// agent 运行时对于聊天中的工具调用传递自己的 sessionID；它们自动落在会话子前缀下。

// workspaceSessionScope 将 URL 中的 `?sessionId=` token 转换为
// workspaces/<agent>/sessions/ 下使用的目录名。URL token 是 session_key
// （因此仪表板可以统一地寻址任何会话），但工作区工件按 chat_id 命名空间 —
// 那是 agent 运行时在写入时传递的。
//
// 当 session_key 在调用者的 (user_id, agent_id) 下解析时返回 chat_id。
// 当查找失败时返回 "" — 包括会话属于不同用户的情况 —
// 因此调用者不会意外地扩大范围进入另一个用户的文件。
// 修复前的行为是回退到原始 URL token；在公开 agent 上，这允许非拥有者调用者
// 传递已知的拥有者 chat_id 并读取其文件，因为结果范围是 sessions/<他们的 chat>/。
func (s *Server) workspaceSessionScope(ctx context.Context, agentID, urlToken string) string {
	tok := strings.TrimSpace(urlToken)
	if tok == "" || s.dataStore == nil {
		return ""
	}
	uid := config.UserIDFromContext(ctx)
	if uid == "" {
		return ""
	}
	_, _, chatID, err := s.dataStore.LookupSessionTriple(ctx, uid, agentID, tok)
	if err != nil || chatID == "" {
		return ""
	}
	return chatID
}

func (s *Server) handleAgentFileList(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"files": []any{}})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	// 始终以 project 和 session 都为空的方式 List，以便返回的路径保持 agent 相对
	//（例如 "sessions/<sid>/foo.png" 或 "projects/<pid>/notes.md"）—
	// 下载端点期望该形状，在此处过滤比两个发散的代码路径更便宜。
	objects, err := s.workspaceStore.List(r.Context(), id, "", "")
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	scope := s.fileScopeForRequest(r, id)
	files := make([]map[string]any, 0, len(objects))
	for _, o := range objects {
		if !scope.acceptPath(o.Path) {
			continue
		}
		files = append(files, map[string]any{
			"path":    o.Path,
			"size":    o.Size,
			"modTime": o.ModTime.Unix(),
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"files": files})
}

// fileScope 描述哪些 agent 相对路径对文件浏览器/zip 过滤器可见。
// acceptPath 对作用域认为范围内的路径返回 true：
//
//	普通聊天：sessions/<chat_id>/ 下的路径
//	项目聊天：projects/<pid>/<chat_id>/ 下的路径（聊天自己的文件），
//	          加上直接位于 projects/<pid>/ 的文件（项目根目录的"共享/遗留"文件 —
//	          预子目录布局仍在那里，操作员可能有意将共享文件放在根目录）。
//	          其他聊天的子目录（projects/<pid>/<other-sid>/...）被排除 —
//	          它们属于那个聊天的面板。
//	无会话：所有内容（管理员浏览器）。
//
// archiveSuffix 返回 zip 文件名中使用的人类可读的作用域 id —
// 普通聊天为 chat_id，项目聊天为 "<pid>-<chat_id>"，
// 以便下载名称为 "agent-pid-sid.zip" 而不是仅靠 chat_id 消除歧义。
type fileScope struct {
	acceptPath    func(string) bool
	archiveSuffix string
}

// stripScopePrefix 从 agent 相对路径中删除最深的已知作用域前缀，
// 以便 zip 条目读作纯文件名。顺序很重要：项目聊天在会话聊天之前尝试，
// 这样 `projects/<pid>/<sid>/foo.md` 折叠为 `foo.md` 而不是 `<pid>/<sid>/foo.md`。
// 顶级项目文件也保留前导 `projects/<pid>/` 的剥离，以便它们也读作裸文件名。
func stripScopePrefix(p string) string {
	for _, top := range []string{"projects/", "sessions/"} {
		if !strings.HasPrefix(p, top) {
			continue
		}
		rest := p[len(top):]
		// 在作用域 id 后切割（一个路径段）。
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			rest = rest[i+1:]
			// 项目路径可以有第二个 id 段用于每个聊天的子目录；存在时也折叠它。
			if top == "projects/" {
				if j := strings.IndexByte(rest, '/'); j >= 0 {
					// 仅当第一个段看起来像聊天 id（s-... 前缀）时才将其视为聊天 id。
					// 否则保持 rest 不变，以便遗留的"子目录/file.md"结构不会被过度剥离。
					if first := rest[:j]; strings.HasPrefix(first, "s-") {
						rest = rest[j+1:]
					}
				}
			}
			return rest
		}
		return ""
	}
	return p
}

// rejectAllScope 返回一个不让任何内容通过的 fileScope。当调用者请求了 sessionId
// 但我们无法为他们解析时使用，这样非拥有者无法仅通过猜测/泄露 chat_id
// 在公开 agent 上扩大进入另一个用户的文件。
func rejectAllScope() fileScope {
	return fileScope{acceptPath: func(string) bool { return false }}
}

func (s *Server) fileScopeForRequest(r *http.Request, agentID string) fileScope {
	rawSession := r.URL.Query().Get("sessionId")
	rawProject := r.URL.Query().Get("projectId")
	// 项目登录页面：没有打开特定的聊天，因此面板显示 projects/<pid>/ 下的所有内容 —
	// 每个聊天的子树加上根级别的共享文件。下面的 sessionId 分支是按聊天的视图；
	// 当 URL 是 /agents/<aid>/project/<pid> 且未选择聊天时使用此分支。
	if rawSession == "" && rawProject != "" {
		prefix := "projects/" + rawProject + "/"
		return fileScope{
			acceptPath:    func(p string) bool { return strings.HasPrefix(p, prefix) },
			archiveSuffix: rawProject,
		}
	}
	if rawSession == "" {
		// Agent 范围视图（完全没有范围参数）。拥有者 / super_admin
		// 可以合法地浏览每个文件；非拥有者（公开 agent 查看者、外部 apikey 调用者）
		// 必须指定一个他们拥有的会话，否则我们会给他们其他用户的文件。
		if s.callerOwnsAgent(r, agentID) {
			return fileScope{acceptPath: func(string) bool { return true }}
		}
		return rejectAllScope()
	}
	chatID := s.workspaceSessionScope(r.Context(), agentID, rawSession)
	if chatID == "" {
		// sessionId 未解析到此调用者拥有的聊天 — 要么不存在，要么属于另一个用户。
		// 无论哪种方式，都不返回任何内容。修复前的行为是回退到"接受所有"，
		// 这在公开 agent 上意味着非拥有者可以通过传递垃圾 sessionId 列出每个聊天的文件。
		return rejectAllScope()
	}
	if pid := s.resolveSessionProject(r.Context(), r, agentID, rawSession); pid != "" {
		ownPrefix := "projects/" + pid + "/" + chatID + "/"
		rootPrefix := "projects/" + pid + "/"
		return fileScope{
			acceptPath: func(p string) bool {
				if strings.HasPrefix(p, ownPrefix) {
					return true
				}
				// 位于 projects/<pid>/<file> 的顶级文件（没有进一步的 "/" — 即不在任何 sid 子目录中）。
				if strings.HasPrefix(p, rootPrefix) {
					rest := p[len(rootPrefix):]
					return rest != "" && !strings.Contains(rest, "/")
				}
				return false
			},
			archiveSuffix: pid + "-" + chatID,
		}
	}
	prefix := "sessions/" + chatID + "/"
	return fileScope{
		acceptPath:    func(p string) bool { return strings.HasPrefix(p, prefix) },
		archiveSuffix: chatID,
	}
}

// handleAgentFilesZip 流式传输 agent 所有工作区文件的 zip（或仅当设置了 ?sessionId= 时的一个会话）。
// 文件以其会话相对路径添加，以便存档布局与用户在聊天面板中看到的匹配 — 没有外层包装目录。
func (s *Server) handleAgentFilesZip(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		http.Error(w, "no workspace store", http.StatusServiceUnavailable)
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	objects, err := s.workspaceStore.List(r.Context(), id, "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	scope := s.fileScopeForRequest(r, id)
	archiveName := fmt.Sprintf("%s.zip", id)
	if scope.archiveSuffix != "" {
		archiveName = fmt.Sprintf("%s-%s.zip", id, scope.archiveSuffix)
	}
	// 将条目包装在以存档命名的文件夹中，以便解压器（macOS Archive Utility、Windows Explorer、7zip…）
	// 将所有文件放在一个目录内，而不是松散地解压到 zip 旁边。
	// 没有这个，"解压了 5 个文件"看起来像"文件丢失了"，因为它们散布到 Downloads/ 中。
	wrapper := strings.TrimSuffix(archiveName, ".zip") + "/"

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, archiveName))

	zw := zip.NewWriter(w)
	written, skipped, failed := 0, 0, 0
	for _, o := range objects {
		if !scope.acceptPath(o.Path) {
			skipped++
			continue
		}
		// 从存档条目名称中剥离最深的作用域前缀，这样用户在 zip 中看到干净的
		// 文件名，而不是嵌套的 `projects/<pid>/<sid>/foo.md` 路径。
		entryName := stripScopePrefix(o.Path)
		if entryName == "" {
			skipped++
			continue
		}
		hdr := &zip.FileHeader{
			Name:     wrapper + entryName,
			Method:   zip.Deflate,
			Modified: o.ModTime,
		}
		entry, err := zw.CreateHeader(hdr)
		if err != nil {
			// 继续而非返回 — 用其余条目完成存档比中途退出给用户留下单个文件更有用。
			// 修复前的行为：途中任何瞬时故障都会截断 zip 为已写入的内容，
			// 在生产中表现为"只有一张图片出来了"。
			slog.Warn("zip: create entry failed", "agent", id, "path", o.Path, "err", err)
			failed++
			continue
		}
		rc, err := s.workspaceStore.Get(r.Context(), id, "", "", o.Path)
		if err != nil {
			slog.Warn("zip: open object failed", "agent", id, "path", o.Path, "err", err)
			failed++
			continue
		}
		_, copyErr := io.Copy(entry, rc)
		rc.Close()
		if copyErr != nil {
			slog.Warn("zip: copy failed", "agent", id, "path", o.Path, "err", copyErr)
			failed++
			continue
		}
		written++
	}
	if err := zw.Close(); err != nil {
		slog.Warn("zip: writer close failed", "agent", id, "err", err)
	}
	slog.Info("zip: archive sent", "agent", id, "archive", archiveName,
		"objects", len(objects), "written", written, "skipped", skipped, "failed", failed)
}

// handleAgentWorkspaceReveal 在操作系统的原生文件浏览器（Finder/Explorer/xdg-open）中打开聊天者的工作区文件夹。
// 仅限自托管 — 托管部署没有"操作员的本地文件系统"的有意义概念，聊天者也不拥有守护进程，
// 因此暴露此功能将是权限泄露。从查询字符串读取 sessionId / projectId，
// 镜像 fileScopeForRequest 的解析（session_key → chat_id, project 查找），
// 以便打开的目录匹配聊天侧 Workspace 面板显示的内容。
//
// 尽力而为：成功时返回 200 及解析的路径，作用域错误时返回 4xx，
// 配置的工作区存储不暴露主机路径时返回 503（S3 / R2 部署），
// OS 打开命令失败时返回 500。非阻塞 — 我们不等待 Finder 实际显示窗口。
func (s *Server) handleAgentWorkspaceReveal(w http.ResponseWriter, r *http.Request) {
	if buildinfo.IsHostedDeploy() {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "workspace reveal is disabled on hosted deployments"})
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store configured"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}

	scoper, ok := s.workspaceStore.(workspace.LocalScoper)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store has no local path (e.g. S3-backed) — open in Finder is unavailable"})
		return
	}

	rawSession := r.URL.Query().Get("sessionId")
	rawProject := r.URL.Query().Get("projectId")

	// 解析到聊天侧面板作用域的相同 (project, chatID)。
	// 空的 rawSession + 非空 projectId 表示项目登录页面 — 打开项目根目录。
	// 两者都空表示 agent 根目录（管理员浏览器）；我们仍然允许，因为 requireAgentReadable 已经门控了访问。
	chatID := ""
	projectID := rawProject
	if rawSession != "" {
		chatID = s.workspaceSessionScope(r.Context(), id, rawSession)
		if pid := s.resolveSessionProject(r.Context(), r, id, rawSession); pid != "" {
			projectID = pid
		}
	}

	dir, ok := scoper.LocalScopeDir(id, projectID, chatID)
	if !ok || dir == "" {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "workspace store did not return a host path"})
		return
	}

	// 预创建目录，这样 `open <不存在的路径>` 在一个尚未写入任何文件的全新聊天上不会出错 —
	// 空的文件夹仍然给用户一种进展的感觉。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	if err := openInFileBrowser(dir); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

// openInFileBrowser 调用平台适当的"打开"命令。macOS 和 Linux 行为一致（在默认文件管理器中打开目录）；
// Windows 使用 explorer.exe。我们故意不等待子进程 — Finder 特别是立即返回，
// 而且无论哪种方式都没有有用的退出代码可显示。
func openInFileBrowser(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		// `explorer` 即使成功也返回退出码 1，因此我们不检查 err。
		// 唯一的真正失败模式是"二进制不在 PATH 上"，Start() 会报告。
		cmd = exec.Command("explorer", path)
		return cmd.Start()
	default:
		// Linux / *BSD — xdg-open 是 freedesktop 标准。
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	// 分离：我们不关心文件管理器的生命周期。
	go func() { _ = cmd.Wait() }()
	return nil
}

func (s *Server) handleAgentFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rel := r.PathValue("path")
	if rel == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "path required"})
		return
	}
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	if s.workspaceStore != nil {
		s.serveFileFromWorkspaceStore(w, r, id, rel)
		return
	}
	// Workspace store 未配置 — 回退到直接 FS 读取。
	// 本地 FS 布局镜像 workspace store：
	// ~/.bkcrab/workspaces/<agent_id>/<path>。
	home, err := config.HomeDir()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	root := filepath.Join(home, "workspaces", id)
	abs := filepath.Join(root, filepath.Clean("/"+rel))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		jsonResponse(w, http.StatusForbidden, map[string]any{"error": "path escape"})
		return
	}
	// ServeFile 从 mime 数据库自身设置 Content-Type；我们只是在此基础上为 HTML 添加 CSP sandbox —
	// 与上面 setFileResponseHeaders 中相同的理由。
	if ext := strings.ToLower(filepath.Ext(rel)); ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, abs)
}

func (s *Server) serveFileFromWorkspaceStore(w http.ResponseWriter, r *http.Request, agentID, path string) {
	rc, err := s.workspaceStore.Get(r.Context(), agentID, "", "", path)
	if err != nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	defer rc.Close()
	setFileResponseHeaders(w, path)
	io.Copy(w, rc)
}

// setFileResponseHeaders 为用户产生的工作区文件选择正确的 Content-Type，
// 并锁定 agent 生成的 HTML，使其即使在用户直接在标签页中打开 URL 时也无法访问
// 应用的 cookie/存储。从扩展名派生的 Content-Type 允许 iframe 渲染文件
// （octet-stream → about:blank，因为 iframe 不嗅探）。CSP `sandbox` 头部
// 与聊天预览通过 iframe `sandbox` 属性获得的保护相同，但在 HTTP 层应用，
// 因此无论文件如何加载都能生效。
func setFileResponseHeaders(w http.ResponseWriter, path string) {
	ext := strings.ToLower(filepath.Ext(path))
	ctype := mime.TypeByExtension(ext)
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if ext == ".html" || ext == ".htm" {
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
	}
}

func (s *Server) handleAgentFileUpload(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	if s.workspaceStore == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "no workspace store"})
		return
	}
	if rec := s.requireAgentOwner(w, r, id); rec == nil {
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// 聊天客户端对每个附件发送一个表单字段 "file"，因此 multipart 负载通常在同一个键下携带多个条目。
	// r.FormFile 只返回第一个 — 遍历 MultipartForm.File 以便多附件上传提交所有文件，而不仅仅是一个。
	headers := r.MultipartForm.File["file"]
	if len(headers) == 0 {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "no file"})
		return
	}
	// sessionId 将上传限定到 agent 实际看到的沙箱挂载点。
	// 我们解析会话以找到其 project_id，以便在项目聊天中的上传落在 projects/<pid>/ 旁边
	// 与 agent 自己的写入一起；普通聊天保留旧的 sessions/<chat>/ 子目录。
	sessionKey := r.URL.Query().Get("sessionId")
	sessionID := s.workspaceSessionScope(r.Context(), id, sessionKey)
	projectID := s.resolveSessionProject(r.Context(), r, id, sessionKey)
	if projectID != "" {
		// 项目会话不使用每个聊天的子目录 — 清除它，以便 workspace store 路由到 projects/<pid>/。
		sessionID = ""
	}
	saved := make([]map[string]any, 0, len(headers))
	for _, h := range headers {
		fh, err := h.Open()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		data, err := io.ReadAll(fh)
		fh.Close()
		if err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.workspaceStore.Put(r.Context(), id, projectID, sessionID, h.Filename, strings.NewReader(string(data)), int64(len(data)), ""); err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		saved = append(saved, map[string]any{"name": h.Filename, "size": len(data)})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "files": saved})
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// invalidateUser 丢弃用户延迟加载的 UserSpace，以便下次访问时从数据库重新加载。
// gateway 在 api.UserResolver 接口后面实现 InvalidateUser。
func (s *Server) invalidateUser(userID string) {
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
func (s *Server) invalidateAgent(agentID string) {
	if agentID == "" || s.userResolver == nil {
		return
	}
	if r, ok := s.userResolver.(interface{ InvalidateAgent(string) }); ok {
		r.InvalidateAgent(agentID)
	}
	slog.Debug("invalidated user spaces holding agent", "agent", agentID)
}

// requireOwnerOrSuperAdmin 门控变更另一个用户资源的端点。
func (s *Server) requireOwnerOrSuperAdmin(w http.ResponseWriter, r *http.Request, ownerID string) bool {
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
func (s *Server) handleListAgentRegisteredTools(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	ag := s.resolveAgent(r, id)
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
