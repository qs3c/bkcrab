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
	"strings"
	"time"

	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/api"
	"github.com/qs3c/bkclaw/internal/auth"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/users"
)

type agentChatEvent = agent.ChatEvent

// loadUserConfig 读取请求用户的合并 Config 视图。
// 遍历 system → user 设置命名空间 + 作用域感知的 provider/channel 行。
// 结果与 gateway.assembleConfig 生成的形状相同 — Storage/Gateway 等仅 UI 字段由环境覆盖填充，不由数据库填充。
func (s *configRepo) loadUserConfig(r *http.Request) (*config.Config, error) {
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
func loadAgentSkillEntriesForUser(ctx context.Context, st agentConfigStore, userID string) (map[string]map[string]config.SkillEntryCfg, error) {
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
func saveAgentSkillEntries(ctx context.Context, st store.ConfigStore, agentID string, entries map[string]config.SkillEntryCfg) error {
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
func (s *configRepo) saveUserConfig(r *http.Request, cfg *config.Config) error {
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
func (s *workspaceRepo) resolveSessionProject(ctx context.Context, r *http.Request, agentID, sessionKey string) string {
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
func (s *agentGuard) resolveAgent(r *http.Request, agentID string) AgentHandle {
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

func (s *agentGuard) resolveAllAgents(r *http.Request) []AgentHandle {
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

// scopeForSave 镜像 saveUserConfig 中的作用域解析逻辑，以便
// 调用者可以精确地使刚被触及的 UserSpaces 失效。
func (s *configRepo) scopeForSave(r *http.Request) (string, string) {
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

// --- /api/test-provider ---

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

// --- chat handlers (delegate to per-user agent) ---

// readWorkspaceFileBytes 通过 workspace store 读取单个 agent 相对路径的文件，
// 当没有配置 store 时回退到本地 FS 布局。裸路径字符串接口仅由 todo 端点使用
// — workspaceStore.Get 期望 (projectID, chatID)，但这里我们已经将它们烘焙到路径中，因此传递空字符串。
func (s *workspaceRepo) readWorkspaceFileBytes(ctx context.Context, agentID, relPath string) ([]byte, error) {
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
		return "bkclaw-default-token"
	}
	return hex.EncodeToString(b)
}

// debugLog 被各种 handler 用于诊断事件；作为薄包装保留，
// 以便 handler 文件不直接导入 slog。
func debugLog(msg string, kv ...any) { slog.Debug(msg, kv...) }
