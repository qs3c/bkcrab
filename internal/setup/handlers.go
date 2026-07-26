package setup

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/api"
	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/buildinfo"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/session"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type agentChatEvent = agent.ChatEvent

// loadUserConfig 读取请求用户的合并 Config 视图。
// 遍历 system → user 设置命名空间 + 作用域感知的 provider/channel 行。
// 结果与 gateway.assembleConfig 生成的形状相同 — Storage/Gateway 等仅 UI 字段由环境覆盖填充，不由数据库填充。
func (s *Server) loadUserConfig(r *http.Request) (*config.Config, error) {
	if s.dataStore == nil {
		return &config.Config{}, nil
	}
	uid := config.UserIDFromContext(r.Context())
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Channels:  map[string]config.ChannelConfig{},
	}
	for _, ns := range settingNamespaces {
		if err := scope.SettingInto(r.Context(), s.dataStore, ns.namespace, uid, "", ns.dst(cfg)); err != nil {
			return nil, err
		}
	}
	if provs, err := scope.Providers(r.Context(), s.dataStore, uid, ""); err == nil {
		for k, v := range provs {
			cfg.Providers[k] = v
		}
	}
	if chs, err := scope.Channels(r.Context(), s.dataStore, uid, ""); err == nil {
		for k, v := range chs {
			cfg.Channels[k] = v
		}
	}
	if ae, err := loadAgentSkillEntriesForUser(r.Context(), s.dataStore, uid); err == nil && len(ae) > 0 {
		cfg.Skills.AgentEntries = ae
	}
	config.LoadEnv().ApplyToConfig(cfg)
	config.ApplyDefaults(cfg)
	return cfg, nil
}

// loadAgentSkillEntriesForUser 收集此用户拥有的所有 agent 作用域 skills.entries 行。
// 替换了旧的单行按 agent 键控的 blob — 每个 agent 现在持久化自己的行，
// 因此我们返回的 JSON 通过列出用户的 agent 并拉取每个 agent 的行来重建。
func loadAgentSkillEntriesForUser(ctx context.Context, st store.Store, userID string) (map[string]map[string]config.SkillEntryCfg, error) {
	if st == nil || userID == "" {
		return nil, nil
	}
	agents, err := st.ListAgents(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]config.SkillEntryCfg{}
	for _, ar := range agents {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, "", ar.ID, "skills.entries")
		if err != nil || rec == nil || len(rec.Data) == 0 {
			continue
		}
		blob, _ := json.Marshal(rec.Data)
		var entries map[string]config.SkillEntryCfg
		if json.Unmarshal(blob, &entries) == nil && len(entries) > 0 {
			out[ar.ID] = entries
		}
	}
	return out, nil
}

// saveAgentSkillEntries 更新或插入 agent 作用域的 skills.entries 行。
// 空的内部映射删除该行（无覆盖 → 无行，保持 configs 表紧凑）。授权是调用者的责任；我们只持久化请求的内容。
func saveAgentSkillEntries(ctx context.Context, st store.Store, agentID string, entries map[string]config.SkillEntryCfg) error {
	if len(entries) == 0 {
		return scope.SaveSetting(ctx, st, "", agentID, "skills.entries", nil)
	}
	blob, _ := json.Marshal(entries)
	var asMap map[string]interface{}
	_ = json.Unmarshal(blob, &asMap)
	return scope.SaveSetting(ctx, st, "", agentID, "skills.entries", asMap)
}

// saveUserConfig 持久化调用用户作用域的命名空间设置行。
// Providers/Channels 位于它们自己的 configs 行中，此处不涉及 — 专用的 /api/providers 和 /api/channels 端点（以及 onboard handler）写入它们。
func (s *Server) saveUserConfig(r *http.Request, cfg *config.Config) error {
	if s.dataStore == nil {
		return errors.New("store not configured")
	}
	ident, ok := authIdentity(r)
	// 决定谁拥有我们将要保存的行：
	//   - super_admin 无 ?actAs=  → system 行 (user_id='')
	//   - super_admin 带 ?actAs=X    → 写入用户 X 的作用域
	//   - 普通用户                 → 写入自己的作用域
	uid := ""
	if ok && ident.Role == "super_admin" {
		if ident.IsActingAs() {
			uid = ident.EffectiveUserID()
		}
	} else if ok {
		uid = ident.UserID
	}
	for _, ns := range settingNamespaces {
		if ns.loadOnly {
			continue
		}
		data := ns.collect(cfg)
		if err := scope.SaveSetting(r.Context(), s.dataStore, uid, "", ns.namespace, data); err != nil {
			return err
		}
	}
	return nil
}

// settingNamespaces 是驱动 loadUserConfig / saveUserConfig 的表。
// 向往返中添加新的 Config 子块只需在此添加一行。
var settingNamespaces = []settingNamespace{
	{namespace: "agents.defaults",
		dst:     func(c *config.Config) interface{} { return &c.Agents.Defaults },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Agents.Defaults) }},
	{namespace: "sandbox",
		dst:     func(c *config.Config) interface{} { return &c.Sandbox },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Sandbox) }},
	{namespace: "objectstore",
		dst:     func(c *config.Config) interface{} { return &c.ObjectStore },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.ObjectStore) }},
	{namespace: "hooks",
		dst:     func(c *config.Config) interface{} { return &c.Hooks },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Hooks) }},
	{namespace: "plugins",
		dst:     func(c *config.Config) interface{} { return &c.Plugins },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Plugins) }},
	{namespace: "taskqueue",
		dst:     func(c *config.Config) interface{} { return &c.TaskQueue },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.TaskQueue) }},
	{namespace: "tools.providers",
		dst:     func(c *config.Config) interface{} { return &c.ToolProviders },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.ToolProviders) }},
	{namespace: "tools.categories",
		dst:     func(c *config.Config) interface{} { return &c.Tools },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.Tools) }},
	{namespace: "skills.install",
		dst:     func(c *config.Config) interface{} { return &c.Skills.Install },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Skills.Install) }},
	{namespace: "skills.entries",
		dst:     func(c *config.Config) interface{} { return &c.Skills.Entries },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.Skills.Entries) }},
	// 每个 agent 的技能 env/key 覆盖已从此表拆分为每个 agent 一行，scope=agent, name=skills.entries —
	// 请参见下面的 loadAgentSkillEntriesForUser / saveAgentSkillEntries。
	// 将所有 agent 的覆盖合并到单个 user/system 作用域行会导致 JSON blob 随每个 agent × skill 增长，
	// 并强制每次补丁都完全重写。
	{namespace: "memory",
		dst:     func(c *config.Config) interface{} { return &c.Memory },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Memory) }},
	{namespace: "rag",
		dst:      func(c *config.Config) interface{} { return &c.RAG },
		loadOnly: true},
	{namespace: "privacy",
		dst:     func(c *config.Config) interface{} { return &c.Privacy },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Privacy) }},
	{namespace: "skillsLearner",
		dst:     func(c *config.Config) interface{} { return &c.SkillsLearner },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.SkillsLearner) }},
	{namespace: "heartbeat",
		dst:     func(c *config.Config) interface{} { return &c.Heartbeat },
		collect: func(c *config.Config) map[string]interface{} { return toMap(c.Heartbeat) }},
	{namespace: "teams",
		dst:     func(c *config.Config) interface{} { return &c.Teams },
		collect: func(c *config.Config) map[string]interface{} { return wrapKeyed(c.Teams) }},
	{namespace: "bindings",
		dst: func(c *config.Config) interface{} { return &c.Bindings },
		collect: func(c *config.Config) map[string]interface{} {
			if len(c.Bindings) == 0 {
				return nil
			}
			return map[string]interface{}{"list": c.Bindings}
		}},
}

type settingNamespace struct {
	namespace string
	dst       func(*config.Config) interface{}
	collect   func(*config.Config) map[string]interface{}
	loadOnly  bool
}

func toMap(v interface{}) map[string]interface{} {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	if len(m) == 0 {
		return nil
	}
	return m
}

// wrapKeyed 将 map[string]X 编组为 map[string]interface{} 以适配 configs.data 列。
// 空的 map 返回 nil，以便 SaveSetting 删除行而不是写入 {}。
func wrapKeyed(v interface{}) map[string]interface{} {
	blob, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	if len(m) == 0 {
		return nil
	}
	return m
}

func authIdentity(r *http.Request) (auth.Identity, bool) {
	return auth.FromContext(r.Context())
}

// resolveAgent 返回调用者用户空间内给定 agent 的 AgentHandle。
// Apikey 调用者在返回句柄之前还会根据其访问列表进行检查。
// 没有 actAs 覆盖的 super_admin 会将外部 agent 注入到自己的 UserSpace 中 —
// sessions、memory 和 provider 作用域保持按调用者键控（管理员看不到拥有者的聊天），而 agent 的持久
// resolveSessionProject 读取请求目标聊天的 sessions.project_id，
// 以便当聊天属于某个 project 时，附件和其他 workspace IO 可以路由到 projects/<pid>/。
// 返回 ""（调用者将其视为松散聊天）— 不存在的 session、无 auth context 和 "no datastore"
// 都归结为相同结果，我们不想让路径查找破坏聊天的热路径。
func (s *Server) resolveSessionProject(ctx context.Context, r *http.Request, agentID, sessionKey string) string {
	if sessionKey == "" || s.dataStore == nil {
		return ""
	}
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return ""
	}
	uid := ident.EffectiveUserID()
	if uid == "" {
		return ""
	}
	pid, err := s.dataStore.LookupSessionProject(ctx, uid, agentID, sessionKey)
	if err != nil {
		return ""
	}
	return pid
}

// 身份（system prompt、agent 作用域配置、skills、files — 全部按 agent_id 键控）被重用。
func (s *Server) resolveAgent(r *http.Request, agentID string) AgentHandle {
	ident, ok := auth.FromContext(r.Context())
	if !ok {
		return nil
	}
	if !ident.CanAccessAgent(agentID) {
		return nil
	}
	if s.userResolver == nil {
		return nil
	}
	uid := ident.EffectiveUserID()
	space, err := s.userResolver.UserSpaceFor(uid)
	if err != nil || space == nil || space.Agents == nil {
		return nil
	}
	ag := space.Agents.AgentByID(agentID)
	// 延迟附加：当 agent 不在调用者的 UserSpace 中但调用者已被授权使用时。
	// 具体场景：
	//
	//   1. super_admin 浏览其他用户的 agent。
	//   2. api_key 的 ACL 授予了此 agent — 通常是密钥拥有者 == agent 拥有者，
	//      但此路径也处理 SwitchToAppUser 将身份翻转为新的 app_user 的情况，
	//      该 app_user 的 UserSpace 根本没有 agent。在该 UserSpace 下写入的
	//      Sessions/files 然后按最终用户分区，这是所需的隔离。
	//   3. 会话用户访问其他人拥有的公开 agent（基于链接的共享 — 受 agents.is_public 门控）。
	//
	// 对于公开 agent 路径，我们确实需要数据库命中以确认 is_public；
	// 其他所有情况（super_admin、apikey ACL）已由 Identity 回答。
	// EnsureAgent 是幂等的，因此查找仅在 agent 进入用户的 Manager 之前触发一次，
	// 一旦附加，AgentByID 在后续请求上成功。
	if ag == nil {
		injector, hasInjector := s.userResolver.(api.AgentInjector)
		// super_admin 可以延迟附加外部 agent，无论 actAs 模式如何。
		// 在 actAs 模式下，EffectiveUserID() 是被模拟的用户
		// — 将该 agent 附加到该用户的 UserSpace 正是
		// 管理 Chats "Open" 流程读取在 user_id=模拟用户 下写入的会话所需要的。
		// 以前的 `!ident.IsActingAs()` 门控阻止了这种情况，
		// 即使 session_messages 行存在于数据库中，聊天面板也会呈现空白。
		canAttach := hasInjector &&
			(ident.AuthMethod == "apikey" || ident.Role == users.RoleSuperAdmin)
		if !canAttach && hasInjector && uid != "" && s.dataStore != nil {
			if rec, err := s.dataStore.GetAgent(r.Context(), agentID); err == nil && rec != nil && rec.IsPublic {
				canAttach = true
			}
		}
		if canAttach {
			if err := injector.EnsureAgent(r.Context(), uid, agentID); err == nil {
				ag = space.Agents.AgentByID(agentID)
			}
		}
	}
	if ag == nil {
		return nil
	}
	return ag
}

func (s *Server) resolveAllAgents(r *http.Request) []AgentHandle {
	ident, ok := auth.FromContext(r.Context())
	if !ok || s.userResolver == nil {
		return nil
	}
	space, err := s.userResolver.UserSpaceFor(ident.EffectiveUserID())
	if err != nil || space == nil || space.Agents == nil {
		return nil
	}
	all := space.Agents.All()
	out := make([]AgentHandle, 0, len(all))
	for _, ag := range all {
		if !ident.CanAccessAgent(ag.Name()) {
			continue
		}
		out = append(out, ag)
	}
	return out
}

// --- /api/status ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	configured := false
	if s.accounts != nil {
		if n, err := s.accounts.Count(r.Context()); err == nil && n > 0 {
			configured = true
		}
	}
	resp := map[string]any{
		"configured":       configured,
		"registrationOpen": s.registrationOpen(r),
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
	cfg, err := s.loadUserConfig(r)
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
	allAgents := s.resolveAllAgents(r)
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

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadUserConfig(r)
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
	masked.RAG = cfg.RAG
	masked.RAG.Milvus.Password = maskConfigSecret(cfg.RAG.Milvus.Password)
	masked.RAG.Embedding.APIKey = maskConfigSecret(cfg.RAG.Embedding.APIKey)
	masked.RAG.Reranker.APIKey = maskConfigSecret(cfg.RAG.Reranker.APIKey)
	masked.RAG.DocumentAI.APIKey = maskConfigSecret(cfg.RAG.DocumentAI.APIKey)
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

func (s *Server) handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
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
	merged, err := s.loadUserConfig(r)
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
	if err := s.saveUserConfig(r, merged); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	var rawConfig map[string]json.RawMessage
	if err := json.Unmarshal(buf, &rawConfig); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if ragPatch, ok := rawConfig["rag"]; ok {
		if err := s.saveRAGConfigPatch(r, ragPatch); err != nil {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
			return
		}
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
			if !s.authorizeScope(w, r, scope.Agent, agentID, scopeWrite) {
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
	sc, scopeID := s.scopeForSave(r)
	s.invalidateScope(sc, scopeID)
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// scopeForSave 镜像 saveUserConfig 中的作用域解析逻辑，以便
// 调用者可以精确地使刚被触及的 UserSpaces 失效。
func (s *Server) scopeForSave(r *http.Request) (string, string) {
	ident, ok := authIdentity(r)
	if ok && ident.Role == "super_admin" {
		if !ident.IsActingAs() {
			return scope.System, ""
		}
		return scope.User, ident.EffectiveUserID()
	}
	if ok {
		return scope.User, ident.UserID
	}
	return scope.User, ""
}

// saveRAGConfigPatch persists only fields explicitly supplied by the caller.
// System scope owns Milvus, embedding, reranker, and limits. User scope may
// override only embedding; it must never copy system credentials into a
// user-owned row.
func (s *Server) saveRAGConfigPatch(r *http.Request, patch json.RawMessage) error {
	sc, scopeID := s.scopeForSave(r)
	current, err := s.loadRAGConfigAtScope(r.Context(), sc, scopeID)
	if err != nil {
		return err
	}

	trimmed := bytes.TrimSpace(patch)
	if bytes.Equal(trimmed, []byte("null")) {
		return scope.SaveSettingByScope(r.Context(), s.dataStore, sc, scopeID, "rag", nil)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &raw); err != nil {
		return fmt.Errorf("invalid rag config: %w", err)
	}

	if sc == scope.System {
		next := current
		if err := json.Unmarshal(trimmed, &next); err != nil {
			return fmt.Errorf("invalid rag config: %w", err)
		}
		if value, ok := rawNestedString(raw, "milvus", "password"); ok && isMaskedSecret(value) {
			next.Milvus.Password = current.Milvus.Password
		}
		if value, ok := rawNestedString(raw, "embedding", "apiKey"); ok && isMaskedSecret(value) {
			next.Embedding.APIKey = current.Embedding.APIKey
		}
		if value, ok := rawNestedString(raw, "reranker", "apiKey"); ok && isMaskedSecret(value) {
			next.Reranker.APIKey = current.Reranker.APIKey
		}
		if value, ok := rawNestedString(raw, "documentAI", "apiKey"); ok && isMaskedSecret(value) {
			next.DocumentAI.APIKey = current.DocumentAI.APIKey
		}
		return scope.SaveSettingByScope(r.Context(), s.dataStore, sc, scopeID, "rag", ragSystemData(next))
	}

	embeddingPatch, ok := raw["embedding"]
	if !ok {
		return nil
	}
	next := current.Embedding
	if bytes.Equal(bytes.TrimSpace(embeddingPatch), []byte("null")) {
		next = config.RAGEmbeddingCfg{}
	} else if err := json.Unmarshal(embeddingPatch, &next); err != nil {
		return fmt.Errorf("invalid rag embedding config: %w", err)
	}
	if value, ok := rawNestedString(raw, "embedding", "apiKey"); ok && isMaskedSecret(value) {
		next.APIKey = current.Embedding.APIKey
	}
	return scope.SaveSettingByScope(r.Context(), s.dataStore, sc, scopeID, "rag", ragUserData(next))
}

func (s *Server) loadRAGConfigAtScope(ctx context.Context, sc, scopeID string) (config.RAGCfg, error) {
	var out config.RAGCfg
	userID, agentID := scope.OwnershipFromScope(sc, scopeID)
	rec, err := s.dataStore.GetConfigByName(ctx, store.KindSetting, userID, agentID, "rag")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return out, nil
		}
		return out, err
	}
	if rec == nil || len(rec.Data) == 0 {
		return out, nil
	}
	blob, err := json.Marshal(rec.Data)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(blob, &out); err != nil {
		return out, err
	}
	return out, nil
}

func rawNestedString(raw map[string]json.RawMessage, section, key string) (string, bool) {
	sectionRaw, ok := raw[section]
	if !ok {
		return "", false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(sectionRaw, &fields) != nil {
		return "", false
	}
	valueRaw, ok := fields[key]
	if !ok {
		return "", false
	}
	var value string
	if json.Unmarshal(valueRaw, &value) != nil {
		return "", false
	}
	return value, true
}

func ragSystemData(cfg config.RAGCfg) map[string]interface{} {
	if reflect.DeepEqual(cfg, config.RAGCfg{}) {
		return nil
	}
	return toMap(cfg)
}

func ragUserData(embedding config.RAGEmbeddingCfg) map[string]interface{} {
	if embedding == (config.RAGEmbeddingCfg{}) {
		return nil
	}
	return map[string]interface{}{"embedding": toMap(embedding)}
}

// --- /api/test-provider ---

type testProviderRequest struct {
	APIBase  string `json:"apiBase"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	APIType  string `json:"apiType"`
	AuthType string `json:"authType"`
}

func (s *Server) handleTestProvider(w http.ResponseWriter, r *http.Request) {
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
func (s *Server) handleTestStoredProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, err := s.dataStore.GetConfig(r.Context(), id)
	if err != nil || rec == nil || rec.Kind != store.KindProvider {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not found"})
		return
	}
	// 测试 = 读取等效：任何可以读取该行的用户都可以验证它是否有效。
	// 他们无论如何都会通过其 agent 运行时使用它，因此仪表板端的试运行不应更严格。
	if !s.authorizeScope(w, r, rec.LegacyScope(), rec.LegacyScopeID(), scopeRead) {
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

// runProviderTest 向上游 provider 发起轻量级聊天补全。
// 在内联测试（请求体中提供密钥，在创建/重新密钥时使用）和存储测试
// （服务器端查找密钥，在编辑时使用）之间共享。
//
// 我们有意识地比"HTTP 2xx = ok"更严格，因为某些上游（one-api/new-api 网关、
// 通用反向代理，甚至配置错误的 nginx）会在错误路径上愉快地返回 200 和 HTML。
// 在这里仅检查 2xx 会在运行时稍后 404 的 URL 报告为绿色。
// 因此在请求之后，我们还要求响应看起来像一个真正的 Messages / ChatCompletion 对象。
func runProviderTest(ctx context.Context, req testProviderRequest) map[string]any {
	base := provider.NormalizeAPIBase(req.APIBase, req.APIType)
	var testURL string
	var body io.Reader
	if req.APIType == "anthropic-messages" {
		testURL = base + "/v1/messages"
		model := req.Model
		if model == "" {
			model = "claude-sonnet-4-20250514"
		}
		payload := fmt.Sprintf(`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	} else {
		testURL = base + "/chat/completions"
		model := req.Model
		if model == "" {
			model = "gpt-4o-mini"
		}
		payload := fmt.Sprintf(`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
		body = strings.NewReader(payload)
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", testURL, body)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIType == "anthropic-messages" {
		httpReq.Header.Set("x-api-key", req.APIKey)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
	} else if req.AuthType == "api-key" {
		httpReq.Header.Set("api-key", req.APIKey)
	} else {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return map[string]any{
			"ok":    false,
			"error": fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(respBody)), 240)),
		}
	}
	if err := validateProviderTestBody(req.APIType, respBody); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	return map[string]any{"ok": true}
}

// validateProviderTestBody 确认 2xx 响应体是一个真正的 Messages / ChatCompletion 对象，
// 而不是 HTML 启动页面或通用网关"ok"负载。如果形状匹配则返回 nil。
func validateProviderTestBody(apiType string, body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty response body")
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return fmt.Errorf("response is not JSON: %s", truncate(string(trimmed), 120))
	}
	var probe map[string]any
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return fmt.Errorf("response is not valid JSON: %v", err)
	}
	if apiType == "anthropic-messages" {
		if _, ok := probe["content"].([]any); ok {
			return nil
		}
		if t, _ := probe["type"].(string); t == "message" {
			return nil
		}
		return fmt.Errorf("response missing Anthropic Messages fields (content/type=message)")
	}
	if _, ok := probe["choices"].([]any); ok {
		return nil
	}
	return fmt.Errorf("response missing OpenAI Chat Completion field 'choices'")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// --- /api/tasks ---

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if s.taskQueue == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	tasks := s.taskQueue.RecentTasks(50)
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		entry := map[string]any{
			"id":        t.ID,
			"agentId":   t.AgentID,
			"chatKey":   t.ChatKey,
			"status":    string(t.Status),
			"createdAt": t.CreatedAt.Format(time.RFC3339),
		}
		if t.StartedAt != nil && t.DoneAt != nil {
			entry["duration"] = t.DoneAt.Sub(*t.StartedAt).Milliseconds()
		}
		if t.Error != nil {
			entry["error"] = t.Error.Error()
		}
		out = append(out, entry)
	}
	jsonResponse(w, http.StatusOK, out)
}

// --- chat handlers (delegate to per-user agent) ---

type chatRequest struct {
	AgentID   string `json:"agentId,omitempty"`
	SessionID string `json:"sessionId"`
	// ProjectID，当非空且 session 行尚不存在时，
	// 是 URL 在第一消息之前携带的"此聊天属于项目 X"提示（`?project=<pid>`）。
	// 一旦行存在，它就是权威的 — 服务器从行中读取 project_id 并忽略任何后续提示。
	ProjectID string `json:"projectId,omitempty"`
	Message   string `json:"message"`
	// Images 携带用于图像附件的数据 URL / HTTPS URL。
	// Web 客户端历史上在 `imageUrls`（驼峰式）下发送它们，
	// 而 API 路径使用 `images`；我们两者都接受并在下面合并，
	// 以便服务器端管道有一个规范的切片。没有这个，
	// web 的 image_url 内容部分永远不会到达 agent（空切片
	// → 没有 ContentParts 持久化 → 历史重载不显示图像，
	// vision LLM 只看到文本面包屑）。
	Images    []string `json:"images,omitempty"`
	ImageURLs []string `json:"imageUrls,omitempty"`
	// Attachments 是类型化的通用附件字段。每个条目可以携带一个可选的 Name，
	// 该 Name 被清理并用作磁盘文件名，以便 LLM 看到 `report.pdf` 而不是
	// `image_3jk7l_0.pdf`。与 Images / ImageURLs 不同，此处的条目
	// 不会内联为 vision 内容部分 — 它们只进入 /workspace
	// 并通过 `[Attached: /workspace/X]` 面包屑到达 LLM。
	// 当你希望字节直接显示给视觉模型时，使用 Images / ImageURLs（而不是 Attachments）。
	Attachments []attachmentRequest `json:"attachments,omitempty"`
	Params      map[string]any      `json:"params,omitempty"`
}

// attachmentRequest 是单个附件的传输形式。URL 是 data: 或 http(s) URL；Name 是可选的调用者提供的文件名。
type attachmentRequest struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// allAttachments 将三种输入形状（Images、ImageURLs、Attachments）
// 扁平化为一个有序切片以物化到 /workspace。
// 顺序：Images → ImageURLs → Attachments。客户端通常只选一个；允许混合且不进行去重。
func (r chatRequest) allAttachments() []agent.Attachment {
	n := len(r.Images) + len(r.ImageURLs) + len(r.Attachments)
	if n == 0 {
		return nil
	}
	out := make([]agent.Attachment, 0, n)
	for _, u := range r.Images {
		out = append(out, agent.Attachment{URL: u})
	}
	for _, u := range r.ImageURLs {
		out = append(out, agent.Attachment{URL: u})
	}
	for _, a := range r.Attachments {
		out = append(out, agent.Attachment{URL: a.URL, Name: a.Name})
	}
	return out
}

// inlineImageURLs 返回应内联为 vision 内容部分（PhotoURLs → image_url 内容块）的 URL。
// 只有遗留的纯图像字段符合条件：Images 和 ImageURLs 按约定是调用者断言的图像，
// 因此将它们包裹为 image_url 是安全的。较新的 Attachments 字段是通用的（pdf / zip / txt 都是合法的），
// 将非图像 URL 提供给 provider 视觉通道会导致整个轮次失败（Anthropic 对
// `{type:image, source:{type:url, url:<pdf>}}` 返回 400）。
// Attachments 通过 `[Attached: /workspace/<file>]` 面包屑到达 LLM。
func (r chatRequest) inlineImageURLs() []string {
	if len(r.Images) == 0 && len(r.ImageURLs) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Images)+len(r.ImageURLs))
	out = append(out, r.Images...)
	out = append(out, r.ImageURLs...)
	return out
}

// preMaterialized 报告调用者是否已将附件 + 前缀 `[Attached: /workspace/...]` 面包屑
// 上传到消息中。Web 客户端端到端地执行此操作（uploadAgentFiles + chat/page.tsx 中的内联面包屑）；
// 在服务器端再次执行会以生成的名称重复写入文件并发出第二个面包屑，
// 这会使 LLM 读取为两个不同的图像并尝试分别编辑每个。
// 仅通过聊天补全扩展发送原始图像的 API 调用者没有面包屑，因此服务器必须代表他们物化。
func (r chatRequest) preMaterialized() bool {
	return strings.HasPrefix(r.Message, "[Attached:")
}

// annotateMessageWithAttachments 在每个附件前向用户消息添加一行
// `[Attached: /workspace/<file>]` — 与 Web UI 使用的相同面包屑格式
// （参见 web/src/app/agents/[id]/chat/page.tsx:639-645），
// 因此 LLM 看到的传输形状无论轮次是通过 Web 聊天还是聊天 API 到达都是相同的。
// provider.StripAttachedPrefix 在存储的历史记录到达 UI 气泡/页面标题之前清除这些标签。
//
// 我们有意识地不添加尾随的"do not probe"块。早期的一次尝试这样做了 —
// 但显式的负面指令引起了与预期相反的效果（模型反射性地
// `which`/`ls`/`file` 路径"以确认"然后才使用它）。
// Web 路径证明单个裸面包屑就足够了；精确镜像即可。
func annotateMessageWithAttachments(message string, paths []string) string {
	if len(paths) == 0 {
		return message
	}
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("[Attached: /workspace/")
		b.WriteString(p)
		b.WriteString("]\n")
	}
	if message != "" {
		b.WriteString(message)
	}
	return b.String()
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	atts := req.allAttachments()
	msgText := req.Message
	if !req.preMaterialized() {
		// Resolve the chat's project so attachments land in
		// projects/<pid>/ when the session belongs to one. Best-effort:
		// failure → empty pid → loose-chat scope (the historical
		// behavior).
		projectID := s.resolveSessionProject(r.Context(), r, ag.Name(), req.SessionID)
		paths := ag.WriteSessionAttachments(r.Context(), req.SessionID, projectID, atts)
		msgText = annotateMessageWithAttachments(req.Message, paths)
	}
	reply := ag.HandleWebChat(r.Context(), req.SessionID, req.ProjectID, s.effectiveUserID(r), msgText, req.inlineImageURLs(), req.Params)
	jsonResponse(w, http.StatusOK, map[string]any{"reply": reply})
}

// handleChatSteer 将一个消息缓冲到正在进行的轮次中。
// 它不会打开流或发出事件 — 正在运行的轮次（由先前的 /api/chat/stream POST 启动）
// 在工具轮次之间折叠该消息并在其现有的 SSE 上发出 "steer" 事件。
// 当有活跃轮次时返回 200 {"buffered":true}；没有运行时返回 409 {"buffered":false}，
// 以便客户端回退到普通的 /api/chat/stream 发送。
func (s *Server) handleChatSteer(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if s.effectiveUserID(r) == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "empty message"})
		return
	}
	if ag.SteerWeb(req.SessionID, req.ProjectID, req.Message) {
		jsonResponse(w, http.StatusOK, map[string]any{"buffered": true})
		return
	}
	jsonResponse(w, http.StatusConflict, map[string]any{"buffered": false})
}

// agentTurnTimeout 是客户端连接断开后允许 agent goroutine 运行的上限。
// 在扇出 delegate_task 工作（6 个并行子 agent × 每个约 10 分钟驱动 camoufox-cli）
// 在 Chat 调用中间常规性地突破之前 15m 预算后，提升到 45m，
// 立即向所有兄弟显示"context deadline exceeded"。
// 45m 舒适地超出带有浏览器自动化的实际最大并行扇出；
// 仍然有边界，因此真正的失控循环不会永久占用 goroutine。
const agentTurnTimeout = 45 * time.Minute

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 击败 nginx / Cloudflare 对长寿命响应的缓冲；
	// agent 循环以人类打字速度发出块，我们希望它们立即在线路上传输，
	// 而不是保持到响应关闭。
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	atts := req.allAttachments()
	imageURLs := req.inlineImageURLs()
	msgText := req.Message
	if !req.preMaterialized() {
		projectID := s.resolveSessionProject(r.Context(), r, ag.Name(), req.SessionID)
		paths := ag.WriteSessionAttachments(r.Context(), req.SessionID, projectID, atts)
		msgText = annotateMessageWithAttachments(req.Message, paths)
	}

	// 在启动 agent 之前订阅 hub，这样我们就不会与第一个发出的事件竞争。
	// hub 缓冲进行中的事件，因此 emitEvent 的分发即使我们消耗缓慢也永远不会阻塞。
	hub := s.chatEventHub()
	agentID := ag.Name()
	sub, unsubscribe := hub.Subscribe(uid, agentID, req.SessionID)
	defer unsubscribe()

	// 从请求中分离 agent 的 ctx：当浏览器标签页断开连接（刷新、关闭、网络抖动）时，
	// 我们希望 agent 继续运行，以便其已付费的 LLM 调用完成并且回复记录在 session_events 中。
	// 15 分钟的上限是唯一可以杀死它的因素。
	agentCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), agentTurnTimeout)
	// cancel 活在 handler 上，而不是 agent goroutine 上：当斜杠命令
	// 排队一个延续时，我们在 HandleMessage 返回之后保持 SSE 打开，
	// 而内部作用域的 cancel 会在延续的事件到达此 handler 的安全网检查之前拆除 agentCtx。
	defer cancel()
	agentCtx = agent.ContextWithStream(agentCtx, nil, s.dataStore, hub, uid, agentID, req.SessionID)

	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		// events 参数保持为 nil — emitEvent 现在通过上面附加的
		// streamCtx 扇出（persist + hub）。此 handler 不再需要旧的通道路径。
		_ = ag.HandleWebChatStream(agentCtx, req.SessionID, req.ProjectID, uid, msgText, imageURLs, req.Params, nil)
	}()

	// Heartbeat 防止代理（nginx 60s 默认、Cloudflare 100s、ELB 60s）
	// 在 agent 正在思考但尚未发出内容时杀死空闲的 SSE 连接。
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	clientGone := r.Context().Done()
	// turnPending 在斜杠命令 handler 报告它通过 bus.Inbound 排队了
	// 一个延续（`turn_pending` 事件）时打开。
	// POST goroutine 的 HandleMessage 已经返回，但真正的回复仍在
	// 不同的 goroutine 上 10-15 秒外 — 我们保持 SSE 打开，
	// 以便浏览器的打字指示器保持可见，延续的 content_delta/content 事件
	// 流入同一个连接。当延续自己的 `done` 到达时清除，
	// 此时循环正常返回。
	turnPending := false
	for {
		select {
		case <-clientGone:
			// 客户端断开；agent goroutine 在其分离的 ctx 上继续运行
			// 并持久化它发出的每个事件。重新加载聊天页面的用户
			// 将通过 /api/chat/subscribe?since=N 获取其余内容。
			return
		case <-agentDone:
			// 竞争：HandleMessage 向 hub 发布 `turn_pending`
			// 并且 `defer close(agentDone)` 从同一个 goroutine 触发。
			// 当两者都就绪时，Go 的 select 随机选择，因此即使
			// turn_pending 事件在 sub 缓冲区中，agentDone 也可能获胜。
			// 首先排空待处理事件以使决策确定性。
		drain:
			for {
				select {
				case env, ok := <-sub:
					if !ok {
						return
					}
					turnPendingEvent, done := forwardChatStreamEvent(w, flusher, env)
					if turnPendingEvent {
						turnPending = true
						continue
					}
					if done {
						return
					}
				default:
					break drain
				}
			}
			if turnPending {
				// HandleMessage 在排队一个延续后静默返回。
				// 不要关闭；等待延续的 `done` 事件通过 hub。
				// agentCtx.Done()（15 分钟超时）如果它永远不到达则是上限。
				agentDone = nil
				continue
			}
			return
		case <-agentCtx.Done():
			// 上面 turnPending 路径的安全网：
			// 即使没有 `done` 事件到达，也在 agent 上下文硬超时时退出。
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case env, ok := <-sub:
			if !ok {
				return
			}
			turnPendingEvent, done := forwardChatStreamEvent(w, flusher, env)
			if turnPendingEvent {
				turnPending = true
				continue
			}
			if done {
				return
			}
		}
	}
}

func forwardChatStreamEvent(w http.ResponseWriter, flusher http.Flusher, env agent.EventEnvelope) (turnPending bool, done bool) {
	if env.Event.Type == "turn_pending" {
		return true, false
	}
	forwardEvent(w, flusher, env)
	return false, env.Event.Type == "done"
}

// forwardEvent 将一个 EventEnvelope 写入 SSE 响应。
// 在 JSON 负载中包含 seq 内联（除了 SSE `id:` 行），
// 以便前端 POST sendChatStream 使用的基于 fetch 的解析器
// 可以针对并行的 /api/chat/subscribe SSE 连接进行去重。
// 没有这个去重，每个块在活跃轮次期间会渲染两次。
func forwardEvent(w http.ResponseWriter, flusher http.Flusher, env agent.EventEnvelope) {
	payload := map[string]any{
		"seq":  env.Seq,
		"type": env.Event.Type,
	}
	if env.Event.Data != nil {
		payload["data"] = env.Event.Data
	}
	data, _ := json.Marshal(payload)
	if env.Seq >= 0 {
		fmt.Fprintf(w, "id: %d\n", env.Seq)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// handleChatSubscribe 为一对（agent, session）保持一个 SSE 连接打开，
// 并转发三种类型的流量：
//
//  1. Replay：seq > since（或 > Last-Event-ID）的 session_events 行，
//     客户端在连接之前错过了。让刚重新加载的页面拾取进行中的轮次，而无需回复的其余部分消失。
//
//  2. 来自 hub 的实时 agent 聊天事件 — agent 循环中的每个 emitEvent 调用
//     都通过这里扇出。这涵盖同步 POST /api/chat/stream 路径
//     以及其他标签页/cron 触发启动的轮次，因此任何打开的聊天面板都能看到它们，
//     无论谁触发了工作。
//
//  3. 遗留的 WebChannel bus 消息 — 通过 bus.Outbound 路由的 cron 触发最终回复，
//     而不是聊天事件路径。保留以免我们在过渡期间丢失现有的功能。
//
// Auth 门控重用 resolveAgent，因此调用者必须已经有权限与此 agent 聊天。
// 订阅本身不会产生任何流量 — 关闭是静默的（客户端离开）。
func (s *Server) handleChatSubscribe(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	if agentID == "" || sessionID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId and sessionId required"})
		return
	}
	if ag := s.resolveAgent(r, agentID); ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// 初始刷新以便客户端 EventSource 立即触发 `open`。
	fmt.Fprintf(w, ": ok\n\n")
	flusher.Flush()

	// 恢复点：优先使用 Last-Event-ID（浏览器管理的重连），
	// 回退到显式传递它的调用者的 ?since=N。 -1 表示"仅实时流，无回放"。
	sinceSeq := int64(-1)
	if hdr := r.Header.Get("Last-Event-ID"); hdr != "" {
		if v, err := strconv.ParseInt(hdr, 10, 64); err == nil {
			sinceSeq = v
		}
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			sinceSeq = v
		}
	}

	hub := s.chatEventHub()
	// 在回放之前订阅，这样在我们扫描数据库时到达的任何事件
	// 最终要么在回放范围内，要么在实时通道中 — 永远不会两者都，永远不会丢失。
	live, unsubscribeLive := hub.Subscribe(uid, agentID, sessionID)
	defer unsubscribeLive()

	// 从持久化日志中回放错过的事件。
	if s.dataStore != nil {
		rows, err := s.dataStore.ListSessionEventsSince(r.Context(), uid, agentID, sessionID, sinceSeq)
		if err != nil {
			slog.Warn("session_events replay failed", "agent", agentID, "session", sessionID, "since", sinceSeq, "error", err)
		}
		for _, rec := range rows {
			fmt.Fprintf(w, "id: %d\n", rec.Seq)
			if len(rec.Data) == 0 || string(rec.Data) == "null" {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"type\":%q}\n\n", rec.Seq, rec.Type)
			} else {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"type\":%q,\"data\":%s}\n\n", rec.Seq, rec.Type, string(rec.Data))
			}
			flusher.Flush()
			if rec.Seq > sinceSeq {
				sinceSeq = rec.Seq
			}
		}
	}

	// 遗留的 webChan 路径：cron 触发的 bus.Outbound 消息。保留直到
	// cron 路径被重构为通过聊天事件 hub 发出（然后这可以消失）。
	var outbound <-chan bus.OutboundMessage
	var unsubscribeOutbound func() = func() {}
	if s.webChan != nil {
		outbound, unsubscribeOutbound = s.webChan.Subscribe(agentID, sessionID)
	}
	defer unsubscribeOutbound()

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case env, ok := <-live:
			if !ok {
				return
			}
			// content_delta 是驱动活跃轮次气泡的高吞吐量逐 token 流。
			// 它有意识地被 NOT 持久化（参见 emitEvent），以 seq=-1 到达，
			// 并且已经通过同一 hub 上的 POST /api/chat/stream 订阅传递给了发起标签页。
			// 在这里转发它会在活跃标签页上双倍渲染；中途加入的重新加载者
			// 会错过部分揭示但仍然获得包含完整文本的尾随 `content` 事件。
			if env.Event.Type == "content_delta" {
				continue
			}
			// 丢弃回放重叠事件：任何 seq <= 我们在回放期间已经流式传输的最高 seq 的事件。
			// 没有这个，在完全错误的时刻重新连接的浏览器会渲染相同的内容块两次。
			if env.Seq >= 0 && env.Seq <= sinceSeq {
				continue
			}
			if env.Seq >= 0 {
				sinceSeq = env.Seq
				fmt.Fprintf(w, "id: %d\n", env.Seq)
			}
			payload := map[string]any{
				"seq":  env.Seq,
				"type": env.Event.Type,
			}
			if env.Event.Data != nil {
				payload["data"] = env.Event.Data
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case msg, ok := <-outbound:
			if !ok {
				outbound = nil
				continue
			}
			payload := map[string]any{
				"text":      msg.Text,
				"parseMode": msg.ParseMode,
			}
			if len(msg.MediaItems) > 0 {
				items := make([]map[string]any, 0, len(msg.MediaItems))
				for _, m := range msg.MediaItems {
					items = append(items, map[string]any{
						"filename":    m.Filename,
						"contentType": m.ContentType,
					})
				}
				payload["mediaItems"] = items
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleChatTodo 读取 agent 维护的每个会话 todo.md，
// 并同时以原始 markdown 和解析的检查列表形式返回。
// 我们在这里解析 session key → (chatID, projectID)，
// 以便前端不需要知道磁盘路径布局（`sessions/<chat>/todo.md` vs `projects/<pid>/<chat>/todo.md`）。
//
// 缺少的文件不是错误 — 不使用 todo 约定的新会话或运行返回 {items: [], raw: ""}。
// 当 items 为空时前端隐藏面板。
func (s *Server) handleChatTodo(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if sessionID == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"items": []any{}, "raw": ""})
		return
	}

	// Build the agent-relative path. Project chats live under
	// projects/<pid>/<chat>/, plain chats under sessions/<chat>/. The
	// agent's workdir resolves bare filenames to its session subdir, so
	// a `write_file("todo.md", ...)` from the agent lands at one of
	// these two paths — same shape that handleAgentFileList already
	// surfaces.
	chatID := s.workspaceSessionScope(r.Context(), ag.Name(), sessionID)
	projectID := s.resolveSessionProject(r.Context(), r, ag.Name(), sessionID)
	var relPath string
	switch {
	case projectID != "" && chatID != "":
		relPath = "projects/" + projectID + "/" + chatID + "/todo.md"
	case chatID != "":
		relPath = "sessions/" + chatID + "/todo.md"
	default:
		jsonResponse(w, http.StatusOK, map[string]any{"items": []any{}, "raw": ""})
		return
	}

	raw, err := s.readWorkspaceFileBytes(r.Context(), ag.Name(), relPath)
	if err != nil {
		// 404 / 未写入 / FS 未命中 — 返回空而不是显示错误；
		// 面板保持隐藏，直到 agent 写入一个。
		jsonResponse(w, http.StatusOK, map[string]any{"items": []any{}, "raw": ""})
		return
	}
	items := parseTodoMarkdown(string(raw))
	jsonResponse(w, http.StatusOK, map[string]any{
		"items": items,
		"raw":   string(raw),
	})
}

// readWorkspaceFileBytes 通过 workspace store 读取单个 agent 相对路径的文件，
// 当没有配置 store 时回退到本地 FS 布局。裸路径字符串接口仅由 todo 端点使用
// — workspaceStore.Get 期望 (projectID, chatID)，但这里我们已经将它们烘焙到路径中，因此传递空字符串。
func (s *Server) readWorkspaceFileBytes(ctx context.Context, agentID, relPath string) ([]byte, error) {
	if s.workspaceStore != nil {
		rc, err := s.workspaceStore.Get(ctx, agentID, "", "", relPath)
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	home, err := config.HomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, "workspaces", agentID)
	abs := filepath.Join(root, filepath.Clean("/"+relPath))
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return nil, fmt.Errorf("path escape")
	}
	return os.ReadFile(abs)
}

// parseTodoMarkdown 从 todo.md 主体中提取复选框行并以结构化条目返回。约定：
//
//   - [ ] text   → 待处理
//   - [x] text   → 已完成
//   - [X] text   → 已完成（不区分大小写）
//
// 其他任何内容（标题行、空行、非复选框列表项）被忽略 —
// todo.md 同时作为人类可读的计划文档，因此我们不强制严格的 schema。
// v1 中不支持缩进复选框（无子任务）；如果需要，在模型的提示中扁平化。
//
// 重复文本的条目被合并：第一次出现赢得槽位，
// `done` 在所有出现之间进行 OR。这是防御性的 —
// 约定说使用 edit_file 翻转单个项目，但如果模型意外重新运行 write_file 并堆叠旧列表和新列表，
// 我们否则会显示相同的步骤两次。进度（done=true）在合并时是粘性的，
// 因此后面的待处理重复项不能回退已检查的项目。
func parseTodoMarkdown(s string) []map[string]any {
	out := []map[string]any{}
	idx := map[string]int{}
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trim, "- [") && !strings.HasPrefix(trim, "* [") {
			continue
		}
		if len(trim) < 6 {
			continue
		}
		box := trim[3]
		rest := strings.TrimSpace(trim[5:])
		if rest == "" {
			continue
		}
		done := box == 'x' || box == 'X'
		if i, ok := idx[rest]; ok {
			if done {
				out[i]["done"] = true
			}
			continue
		}
		idx[rest] = len(out)
		out = append(out, map[string]any{
			"text": rest,
			"done": done,
		})
	}
	return out
}

func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	resp := map[string]any{
		"history":      ag.WebChatHistory(sessionID),
		"contextUsage": ag.ContextUsageBaseline(),
	}
	// latestEventSeq 是 /api/chat/subscribe 的恢复游标 —
	// 客户端用 `since=<latestEventSeq>` 打开该端点，
	// 以便新页面加载只拾取尚未渲染的增量。
	// 尽力而为：丢失/零值仅表示"仅实时流，无回放"，
	// 当会话没有进行中的轮次或 session_events 未被回填时，这是正确的回退。
	if s.dataStore != nil {
		uid := s.effectiveUserID(r)
		if uid != "" {
			if rows, err := s.dataStore.ListSessionEventsSince(r.Context(), uid, ag.Name(), sessionID, -1); err == nil {
				if len(rows) > 0 {
					resp["latestEventSeq"] = rows[len(rows)-1].Seq
				}
				if usage := latestContextUsageFromEvents(rows); usage != nil {
					resp["contextUsage"] = usage
				}
			} else if seq, err := s.dataStore.LatestSessionEventSeq(r.Context(), uid, ag.Name(), sessionID); err == nil {
				resp["latestEventSeq"] = seq
			}
		}
	}
	jsonResponse(w, http.StatusOK, resp)
}

func latestContextUsageFromEvents(rows []store.SessionEventRecord) map[string]any {
	for i := len(rows) - 1; i >= 0; i-- {
		switch rows[i].Type {
		case "usage", "done":
		default:
			continue
		}
		var payload struct {
			Usage map[string]any `json:"usage"`
		}
		if err := json.Unmarshal(rows[i].Data, &payload); err != nil {
			continue
		}
		if len(payload.Usage) > 0 {
			return payload.Usage
		}
	}
	return nil
}

func (s *Server) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"sessions": []session.WebSession{}})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"sessions": ag.WebChatSessions()})
}

func (s *Server) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	// agentId 来自 body 或 query — 前端在 JSON body 中发送它
	//（参见 web/src/lib/api.ts 中的 renameChatSession），与 handleMoveSessionProject 约定一致。
	// 以前的仅 query 路径总是看到 "" 并在 resolveAgent 处以静默 404 退出，
	// 因此即使对话框正常提交，"Edit chat title"也看起来像无操作。
	agentID := r.URL.Query().Get("agentId")
	var req struct {
		AgentID string `json:"agentId"`
		Title   string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if agentID == "" {
		agentID = req.AgentID
	}
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.RenameWebChatSession(r.PathValue("key"), req.Title); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.DeleteWebChatSession(r.PathValue("key")); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMoveSessionProject 将一个聊天重新分配给不同的项目
// （或在 projectId 为 "" 时将其分离回松散聊天列表）。
// 支持侧边栏拖放功能：将聊天行拖到项目标题上/从项目标题拖出会触发此端点。
//
// 请求体：{ "agentId": "...", "projectId": "<pid>" | "" }
//
// 除 sessions.project_id 翻转之外的副作用：
//   - workspace 文件在 sessions/<sid>/ 和 projects/<pid>/<sid>/ 之间移动，
//     以便下一个轮次在新作用域下看到自己的工件。空源目录 = 无操作。
//   - 绑定到此聊天的任何活跃 sandbox 被释放，以便替换容器以新的绑定挂载路径启动。
//
// 当目标目录已有文件时返回 409，code="destination_exists"
// （防御性 — session_keys 是唯一的，因此这不应自然发生，但比静默合并好）。
func (s *Server) handleMoveSessionProject(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	var req struct {
		AgentID   string `json:"agentId"`
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if agentID == "" {
		agentID = req.AgentID
	}
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId required"})
		return
	}
	// 仅拥有者 — 移动聊天会更改其 workspace 路径，只读查看器绝不应触发。
	if rec := s.requireAgentOwner(w, r, agentID); rec == nil {
		return
	}
	if !s.requireWritable(w, r) {
		return
	}
	uid := s.effectiveUserID(r)
	// 验证目标项目存在且属于此调用者。
	// 空的 projectId 是"分离"情况 — 始终允许。
	if req.ProjectID != "" && s.dataStore != nil {
		p, err := s.dataStore.GetProject(r.Context(), uid, agentID, req.ProjectID)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if p == nil {
			jsonResponse(w, http.StatusNotFound, map[string]any{"error": "project not found"})
			return
		}
	}
	ag := s.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.MoveWebChatSession(r.Context(), r.PathValue("key"), req.ProjectID); err != nil {
		if errors.Is(err, workspace.ErrMoveDestinationExists) {
			jsonResponse(w, http.StatusConflict, map[string]any{
				"error": "destination workspace already exists",
				"code":  "destination_exists",
			})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleFeishuWebhook 接收飞书 / Feishu 事件 POST。路由是公开的
// （飞书不通过 bkcrab bearer 认证）；每事件安全性
// 在飞书适配器内部通过验证负载的 header.token 与连接时存储的验证令牌来强制执行。
//
// 将原始 body 交给网关（通过类型断言的 dispatcher hook），
// 后者通过 accountID 找到正确的适配器。适配器返回 HTTP body + status —
// handler 仅中继它。URL 验证挑战和真实事件都通过此相同路径；
// 适配器内部进行区分。
func (s *Server) handleFeishuWebhook(w http.ResponseWriter, r *http.Request) {
	appID := r.PathValue("appId")
	if appID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "appId required"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	type feishuDispatcher interface {
		DispatchFeishuWebhook(accountID string, body []byte) ([]byte, int, error)
	}
	d, ok := s.userResolver.(feishuDispatcher)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "feishu webhook dispatch not available"})
		return
	}
	respBody, status, derr := d.DispatchFeishuWebhook(appID, body)
	if derr != nil {
		slog.Warn("feishu webhook dispatch error", "appId", appID, "status", status, "error", derr)
		if respBody == nil {
			respBody = []byte(`{"ok":false}`)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// handleLINEWebhook 接收 LINE Messaging API 事件 POST。路由是公开的
// （LINE 不通过 bkcrab bearer 认证）；每事件安全性
// 来自 `x-line-signature` 中的 HMAC-SHA256 签名，
// 适配器根据 channel_secret + 原始 body 进行验证。
//
// 读取 body 一次，将原始字节 + 签名交给网关 dispatcher
// （重新编码 JSON 会改变计算 HMAC 所用的字节并破坏验证）。
func (s *Server) handleLINEWebhook(w http.ResponseWriter, r *http.Request) {
	accountID := r.PathValue("accountId")
	if accountID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "accountId required"})
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	signature := r.Header.Get("x-line-signature")
	type lineDispatcher interface {
		DispatchLINEWebhook(accountID string, body []byte, signature string) ([]byte, int, error)
	}
	d, ok := s.userResolver.(lineDispatcher)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"error": "line webhook dispatch not available"})
		return
	}
	respBody, status, derr := d.DispatchLINEWebhook(accountID, body, signature)
	if derr != nil {
		slog.Warn("line webhook dispatch error", "accountId", accountID, "status", status, "error", derr)
		if respBody == nil {
			respBody = []byte(`{"ok":false}`)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(respBody)
}

// --- Helpers ---

func jsonResponse(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}

func maskConfigSecret(secret string) string {
	if secret == "" {
		return ""
	}
	return "****"
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, mins)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}

func looksLikeSecret(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "CREDENTIAL"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func isMaskedSecret(s string) bool {
	if s == "" {
		return false
	}
	return s == "****" || strings.Contains(s, "****")
}

func maskSkillEntry(v config.SkillEntryCfg) config.SkillEntryCfg {
	out := config.SkillEntryCfg{Enabled: v.Enabled, APIKey: maskAPIKey(v.APIKey)}
	if len(v.Env) > 0 {
		out.Env = make(map[string]string, len(v.Env))
		for ek, ev := range v.Env {
			if looksLikeSecret(ek) {
				out.Env[ek] = maskAPIKey(ev)
			} else {
				out.Env[ek] = ev
			}
		}
	}
	return out
}

func mergeSkillEntry(existing, in config.SkillEntryCfg) config.SkillEntryCfg {
	out := config.SkillEntryCfg{Enabled: in.Enabled, APIKey: in.APIKey, Env: in.Env}
	if isMaskedSecret(out.APIKey) {
		out.APIKey = existing.APIKey
	}
	if out.Env != nil {
		for k, v := range out.Env {
			if isMaskedSecret(v) {
				out.Env[k] = existing.Env[k]
			}
		}
	}
	return out
}

func newRandID() (string, error) {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

func generateRandomToken(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "bkcrab-default-token"
	}
	return hex.EncodeToString(b)
}

// debugLog 被各种 handler 用于诊断事件；作为薄包装保留，
// 以便 handler 文件不直接导入 slog。
func debugLog(msg string, kv ...any) { slog.Debug(msg, kv...) }
