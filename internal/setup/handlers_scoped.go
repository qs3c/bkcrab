package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

// 作用域感知的 providers + channels CRUD（以及一个通用设置端点）。授权规则：
//
//   scope=system → 读取：任何已认证用户（系统提供者设计为可继承；api 密钥在输出时被屏蔽）。写入：仅限 super_admin。
//   scope=user   → super_admin 或 scopeId == 调用者的 user_id
//   scope=agent  → super_admin 或调用者拥有该 agent
//
// 所有四个路由共享同一个门控辅助函数，以确保规则保持一致。

// scopeOp 区分 authorizeScope 的读取和变更操作。目前唯一重要的地方是系统作用域：
// 普通用户可以列出继承的系统提供者，但永远不能编辑它们。
type scopeOp int

const (
	scopeRead scopeOp = iota
	scopeWrite
)

// authorizeScope 对于给定操作，如果请求在 (scope, scopeID) 处被允许则返回 true。
// 变更调用者应额外通过 `requireWritable`（它会拒绝 super_admin actAs 模式）。
func (s *Server) authorizeScope(w http.ResponseWriter, r *http.Request, sc, scopeID string, op scopeOp) bool {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return false
	}
	if ident.Role == users.RoleSuperAdmin {
		return true
	}
	switch sc {
	case scope.System:
		// 系统作用域是广播的：每个 agent 都继承自它。读取是开放的，以便仪表板可以向非管理员显示他们正在继承哪些提供者，
		// 运行时也可以解析它们。写入仍锁定为 super_admin（门控的业务原因未改变）。
		if op == scopeRead {
			return true
		}
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "super_admin required to mutate system scope"})
		return false
	case scope.User:
		if scopeID != ident.UserID {
			jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "cannot manage other users' configs"})
			return false
		}
		// app_user 账户是由下游应用提供的最终用户 — 他们不应该能够重定向其 LLM 提供者
		// 或从调用应用下分出频道绑定。读取仍然允许（以便 agent 运行时可以看到上游栈为它们配置了什么）；
		// 只有变更调用者通过 requireWritable 到达此路径，但我们提前硬拒绝以消除歧义。
		if ident.Role == users.RoleAppUser {
			jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "app_user cannot manage user-scope configs"})
			return false
		}
		return true
	case scope.Agent:
		// 必须拥有该 agent。我们通过一次廉价的存储查询来验证。
		if s.dataStore == nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "store not configured"})
			return false
		}
		rec, err := s.dataStore.GetAgent(r.Context(), scopeID)
		if err != nil || rec == nil {
			jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not yours"})
			return false
		}
		if rec.UserID == ident.UserID {
			return true
		}
		// 共享 agent 上的非拥有者读取访问：当拥有者启用了 shareModelConfig（默认）时，
		// agent 的运行时解析已经包含其 agent 作用域的提供者供聊天者使用（EnsureAgent 会叠加它们）。
		// 聊天者的 agent 设置对话框中的 Models 标签页需要显示相同的行 — 带屏蔽密钥 —
		// 以便聊天者知道 agent 正在使用哪些凭证以及哪些模型可用。写入仍仅限拥有者。
		if op == scopeRead {
			if agentShareModelConfig(rec) {
				if rec.IsPublic || s.callerOwnsAgent(r, scopeID) {
					return true
				}
				// 镜像 requireAgentReadable 的 apikey-ACL 门控，以便作用域为此 agent 的 apikey 也可以读取。
				if ident.AuthMethod == "apikey" && ident.CanAccessAgent(scopeID) {
					return true
				}
				// 非公开共享 agent 上的已登录用户：除了 IsPublic / apikey ACL 之外，
				// 我们没有单独的"此用户已被授予访问权限"表，因此落入下面的标准 403。
				//（如果以后构建共享邀请，请在此处进行门控。）
			}
		}
		jsonResponse(w, http.StatusForbidden, map[string]any{"ok": false, "error": "agent not yours"})
		return false
	default:
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid scope"})
		return false
	}
}

// listConfigsByScope 是 store.ListConfigs 的 HTTP 端桥接：
// 将 (scope, scopeID) URL 惯用法转换为 (userID, agentID)。
// 新代码应直接调用 store.ListConfigs 并显式指定拥有者；此辅助函数的存在是为了使仪表板的
// 作用域键控路由不必在每个调用点内联转换。
func (s *Server) listConfigsByScope(ctx context.Context, kind, sc, scopeID string) ([]store.ConfigRecord, error) {
	uid, aid := scope.OwnershipFromScope(sc, scopeID)
	return s.dataStore.ListConfigs(ctx, kind, uid, aid)
}

// getConfigByNameScope 是同一桥接函数的 GetConfigByName 变体。
func (s *Server) getConfigByNameScope(ctx context.Context, kind, sc, scopeID, name string) (*store.ConfigRecord, error) {
	uid, aid := scope.OwnershipFromScope(sc, scopeID)
	return s.dataStore.GetConfigByName(ctx, kind, uid, aid, name)
}

// scopeFromQuery 读取 scope/scopeId 查询参数，带有合理的默认值：
// 缺少 scope 时回退到调用者的用户作用域，这样普通用户的 `GET /api/providers` 返回"他们的"提供者。
func scopeFromQuery(r *http.Request) (string, string) {
	sc := r.URL.Query().Get("scope")
	scopeID := r.URL.Query().Get("scopeId")
	if sc == "" {
		ident, _ := auth.FromContext(r.Context())
		if ident.Role == users.RoleSuperAdmin {
			sc = scope.System
		} else {
			sc = scope.User
			scopeID = ident.UserID
		}
	}
	return sc, scopeID
}

// --- 提供者 ---

func (s *Server) handleListProviders(w http.ResponseWriter, r *http.Request) {
	sc, scopeID := scopeFromQuery(r)
	if !s.authorizeScope(w, r, sc, scopeID, scopeRead) {
		return
	}
	rows, err := s.listConfigsByScope(r.Context(), store.KindProvider, sc, scopeID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		pc := config.ProviderConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &pc)
		}
		out = append(out, map[string]any{
			"id":        r.ID,
			"scope":     r.LegacyScope(),
			"scopeId":   r.LegacyScopeID(),
			"name":      r.Name,
			"apiBase":   pc.APIBase,
			"apiKey":    maskAPIKey(pc.APIKey),
			"apiType":   pc.APIType,
			"authType":  pc.AuthType,
			"models":    pc.Models,
			"updatedAt": r.UpdatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"providers": out, "scope": sc, "scopeId": scopeID})
}

type writeProviderRequest struct {
	Scope    string              `json:"scope"`
	ScopeID  string              `json:"scopeId"`
	Name     string              `json:"name"`
	APIBase  string              `json:"apiBase"`
	APIKey   string              `json:"apiKey"`
	APIType  string              `json:"apiType"`
	AuthType string              `json:"authType"`
	Models   []config.ModelEntry `json:"models,omitempty"`
}

func (s *Server) handleCreateProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	var req writeProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name required"})
		return
	}
	sc, scopeID := req.Scope, req.ScopeID
	if sc == "" {
		sc, scopeID = scopeFromQuery(r)
	}
	if !s.authorizeScope(w, r, sc, scopeID, scopeWrite) {
		return
	}
	pcfg := config.ProviderConfig{
		APIBase:  req.APIBase,
		APIKey:   req.APIKey,
		APIType:  req.APIType,
		AuthType: req.AuthType,
		Models:   req.Models,
	}
	if err := scope.SaveProviderByScope(r.Context(), s.dataStore, sc, scopeID, req.Name, pcfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeWrite) {
		return
	}
	var req writeProviderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	pc := config.ProviderConfig{}
	if blob, err := json.Marshal(rec.Data); err == nil {
		_ = json.Unmarshal(blob, &pc)
	}
	// 补丁 — 当调用者发送了屏蔽的哨兵值时，保留 apiKey。
	if req.APIBase != "" {
		pc.APIBase = req.APIBase
	}
	if req.APIKey != "" && !isMaskedSecret(req.APIKey) {
		pc.APIKey = req.APIKey
	}
	if req.APIType != "" {
		pc.APIType = req.APIType
	}
	if req.AuthType != "" {
		pc.AuthType = req.AuthType
	}
	// `models` 在每次 PUT 中作为完整的期望集合发送（对话框是事实来源）—
	// 即使数组为空也要覆盖，以便"移除最后一个模型"实际生效。
	if req.Models != nil {
		pc.Models = req.Models
	}
	if err := scope.SaveProviderByScope(r.Context(), s.dataStore, rec.LegacyScope(), rec.LegacyScopeID(), rec.Name, pc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.LegacyScope(), rec.LegacyScopeID())
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeWrite) {
		return
	}
	if err := s.dataStore.DeleteConfig(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.LegacyScope(), rec.LegacyScopeID())
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// --- 频道 ---

func (s *Server) handleListScopedChannels(w http.ResponseWriter, r *http.Request) {
	sc, scopeID := scopeFromQuery(r)
	if !s.authorizeScope(w, r, sc, scopeID, scopeRead) {
		return
	}
	rows, err := s.listConfigsByScope(r.Context(), store.KindChannel, sc, scopeID)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		cc := config.ChannelConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &cc)
		}
		out = append(out, map[string]any{
			"id":            r.ID,
			"scope":         r.LegacyScope(),
			"scopeId":       r.LegacyScopeID(),
			"type":          r.Name,
			"enabled":       r.Enabled,
			"botToken":      maskAPIKey(cc.BotToken),
			"appToken":      maskAPIKey(cc.AppToken),
			"credentialKey": r.CredentialKey,
			"updatedAt":     r.UpdatedAt,
		})
	}
	jsonResponse(w, http.StatusOK, map[string]any{"channels": out, "scope": sc, "scopeId": scopeID})
}

type writeChannelRequest struct {
	Scope         string `json:"scope"`
	ScopeID       string `json:"scopeId"`
	Type          string `json:"type"`
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"botToken"`
	AppToken      string `json:"appToken,omitempty"`
	CredentialKey string `json:"credentialKey,omitempty"`
}

// credentialKeyFor 从频道的凭证派生一个稳定的查找句柄。
// 对于 bot-token 频道，我们使用最后 12 个字符（与 Telegram / Discord bot token 的可识别方式匹配）；
// 当调用者已提供密钥时，回退到调用者提供的密钥。
func credentialKeyFor(channelType, botToken, callerKey string) string {
	if callerKey != "" {
		return callerKey
	}
	if botToken == "" {
		return ""
	}
	if len(botToken) <= 12 {
		return botToken
	}
	return botToken[len(botToken)-12:]
}

func (s *Server) handleCreateScopedChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	var req writeChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Type == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "type required"})
		return
	}
	sc, scopeID := req.Scope, req.ScopeID
	if sc == "" {
		sc, scopeID = scopeFromQuery(r)
	}
	if !s.authorizeScope(w, r, sc, scopeID, scopeWrite) {
		return
	}
	credKey := credentialKeyFor(req.Type, req.BotToken, req.CredentialKey)
	uid, aid := scope.OwnershipFromScope(sc, scopeID)
	if err := s.assertChannelCredentialUnique(r, req.Type, credKey, "", uid, aid); err != nil {
		jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	cc := config.ChannelConfig{
		Enabled:  req.Enabled,
		BotToken: req.BotToken,
		AppToken: req.AppToken,
	}
	if err := scope.SaveChannelByScope(r.Context(), s.dataStore, sc, scopeID, req.Type, credKey, req.Enabled, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(sc, scopeID)
	if req.Enabled {
		if rec, _ := s.dataStore.LookupChannelByCredential(r.Context(), req.Type, credKey); rec != nil {
			s.hotRegisterChannel(*rec)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateScopedChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindChannel {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeWrite) {
		return
	}
	var req writeChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cc := config.ChannelConfig{}
	if blob, err := json.Marshal(rec.Data); err == nil {
		_ = json.Unmarshal(blob, &cc)
	}
	if req.BotToken != "" && !isMaskedSecret(req.BotToken) {
		cc.BotToken = req.BotToken
	}
	if req.AppToken != "" && !isMaskedSecret(req.AppToken) {
		cc.AppToken = req.AppToken
	}
	enabled := req.Enabled
	credKey := credentialKeyFor(rec.Name, cc.BotToken, req.CredentialKey)
	if credKey != rec.CredentialKey {
		if err := s.assertChannelCredentialUnique(r, rec.Name, credKey, rec.ID, rec.UserID, rec.AgentID); err != nil {
			jsonResponse(w, http.StatusConflict, map[string]any{"error": err.Error()})
			return
		}
	}
	if err := scope.SaveChannelByScope(r.Context(), s.dataStore, rec.LegacyScope(), rec.LegacyScopeID(), rec.Name, credKey, enabled, cc); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.LegacyScope(), rec.LegacyScopeID())
	if enabled {
		if updated, _ := s.dataStore.LookupChannelByCredential(r.Context(), rec.Name, credKey); updated != nil {
			s.hotRegisterChannel(*updated)
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteScopedChannel(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindChannel {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	if !s.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeWrite) {
		return
	}
	if err := s.dataStore.DeleteConfig(r.Context(), id); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	s.invalidateScope(rec.LegacyScope(), rec.LegacyScopeID())
	// 尽力而为：停止 bot 适配器接收出站路由。
	// 不解码 rec 我们不知道它的 accountID，所以从刚刚查找的行中派生（rec 在此处仍然有效）。
	cc := decodeChannelConfigFromRecord(rec)
	for accountID := range cc.Accounts {
		s.hotUnregisterChannel(rec.Name, accountID)
	}
	if len(cc.Accounts) == 0 {
		s.hotUnregisterChannel(rec.Name, "")
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeChannelConfigFromRecord — gateway.decodeChannelConfig 的本地镜像，
// 这样此包不需要导入 gateway。
func decodeChannelConfigFromRecord(rec *store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: rec.Enabled}
	if blob, err := json.Marshal(rec.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = rec.Enabled
	return cc
}

// assertChannelCredentialUnique 强制实现模式没有数据库约束的软唯一性不变性：
// 两行具有相同的 (kind="channel", credential_key) 会在入站分发器中产生竞争
// — 只返回一行，返回哪一行是未定义的。
//
// "相同逻辑行"豁免：
//   - excludeID 匹配：更新路径；我们在原地重写该行
//   - 相同 (kind, user_id, agent_id, name)：upsert 路径；
//     连接处理程序正在将同一个 bot 重新连接到之前绑定的同一个 (user, agent)。
//     SaveChannel 的 ON CONFLICT 将刷新该行，不会产生竞争。
//
// callerUserID / callerAgentID 是调用者即将写入的行的 (user_id, agent_id) —
// 传递空字符串为没有该上下文信息的调用者保留更严格的"全局唯一性"语义。
func (s *Server) assertChannelCredentialUnique(r *http.Request, channelType, credKey, excludeID string, callerUserID, callerAgentID string) error {
	if credKey == "" {
		return nil
	}
	existing, err := s.dataStore.LookupChannelByCredential(r.Context(), channelType, credKey)
	if err != nil {
		return nil // not-found is fine; other errors get bubbled by the caller
	}
	if existing == nil || existing.ID == excludeID {
		return nil
	}
	// 同一调用者将同一 bot 重新连接到同一 agent — upsert 路径将刷新现有行，而不是创建新行。
	if existing.UserID == callerUserID &&
		existing.AgentID == callerAgentID &&
		existing.Name == channelType {
		return nil
	}
	// 显示冲突实际存在的位置，以便操作员知道先在哪里断开连接，而不是看着一条通用消息。
	scopeHint := existing.LegacyScope()
	if existing.AgentID != "" {
		scopeHint = "agent " + existing.AgentID
	} else if existing.UserID != "" {
		scopeHint = "user " + existing.UserID
	}
	return fmt.Errorf("this bot is already connected at %s — disconnect it there first", scopeHint)
}

// invalidateOwner 是 invalidateScope 的 (userID, agentID) 形式 —
// 对于已经使用新拥有者惯用法的代码更推荐。回退到 invalidateScope，
// 以便无论调用者使用哪个入口点，缓存拓扑都保持一致。
func (s *Server) invalidateOwner(userID, agentID string) {
	sc, scopeID := scope.ScopeFromOwnership(userID, agentID)
	// "user-agent" 在 invalidateScope 的 switch 中不存在 — 对于每个 (user, agent) 的写入，
	// 丢弃用户缓存的 UserSpace 是正确的行为，因此将其映射到 scope=user。
	if sc == "user-agent" {
		sc, scopeID = scope.User, userID
	}
	s.invalidateScope(sc, scopeID)
}

// invalidateScope 丢弃受作用域级别写入影响的缓存 UserSpaces。
// 系统更改影响每个已加载的空间；用户更改影响一个。
func (s *Server) invalidateScope(sc, scopeID string) {
	if s.userResolver == nil {
		return
	}
	type globalInvalidator interface{ ReloadAgents() error }
	type userInvalidator interface{ InvalidateUser(string) }
	type agentInvalidator interface{ InvalidateAgent(string) }
	switch sc {
	case scope.System:
		if r, ok := s.userResolver.(globalInvalidator); ok {
			_ = r.ReloadAgents()
		}
	case scope.User:
		if r, ok := s.userResolver.(userInvalidator); ok {
			r.InvalidateUser(scopeID)
		}
	case scope.Agent:
		// Agent 作用域的写入（provider, channel, setting）影响每个缓存了该 agent 的 UserSpace —
		// 拥有者以及通过 EnsureAgent 延迟附加的任何外部调用者。InvalidateAgent 遍历注册表并全部丢弃；
		// 回退到仅拥有者失效，为未实现较新钩子的解析器保持行为一致。
		if r, ok := s.userResolver.(agentInvalidator); ok {
			r.InvalidateAgent(scopeID)
			return
		}
		ctx := context.Background()
		if all, err := s.dataStore.ListAllAgents(ctx); err == nil {
			for _, ar := range all {
				if ar.ID == scopeID {
					if r, ok := s.userResolver.(userInvalidator); ok {
						r.InvalidateUser(ar.UserID)
					}
					return
				}
			}
		}
	}
}

// hotRegisterChannel 请求网关立即为 `rec` 启动频道适配器。
// 尽力而为 — 当解析器未实现该钩子时（例如在带有存根解析器的测试中），不执行任何操作。
func (s *Server) hotRegisterChannel(rec store.ConfigRecord) {
	if s.userResolver == nil {
		return
	}
	type chanRegistrar interface {
		RegisterChannelFromConfig(rec store.ConfigRecord) error
	}
	if r, ok := s.userResolver.(chanRegistrar); ok {
		if err := r.RegisterChannelFromConfig(rec); err != nil {
			// 不要让请求失败 — 行已保存，下次进程重启时会拾取它。
			// 但在日志中显示错误，以便明显损坏的 bot token 可被调试。
			slog.Warn("hot-register channel failed", "type", rec.Name, "error", err)
		}
	}
}

// hotUnregisterChannel — 与 hotRegisterChannel 配对，用于删除路径。
func (s *Server) hotUnregisterChannel(channelType, accountID string) {
	if s.userResolver == nil {
		return
	}
	type chanUnregistrar interface {
		UnregisterChannel(channelType, accountID string)
	}
	if r, ok := s.userResolver.(chanUnregistrar); ok {
		r.UnregisterChannel(channelType, accountID)
	}
}
