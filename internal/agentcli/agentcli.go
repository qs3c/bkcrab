// Package agentcli 提供了 bkcrab 的 `agents …` CLI 子命令在操作员自身
// BkCrab 存储上执行的数据层操作。CLI 是对网关和仪表盘所使用的同一
// 存储的轻量便捷封装——通过此方式创建的代理与通过 Web 界面创建的
// 代理完全一致。
package agentcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

// validateName 与仪表盘的唯一校验保持一致：去除空格后不为空。
// 更严格的校验会拒绝 Web 界面接受的代理名称，从而破坏
// "CLI ≡ 仪表盘"的一致性契约。
func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("agent name is required")
	}
	return nil
}

// InitOptions 控制 Init 的行为。
type InitOptions struct {
	Description string
	// AgentID 覆盖自动生成的 id。设置此项可更新原本通过仪表盘创建的代理。
	AgentID string

	Provider  string
	Model     string
	APIKeyEnv string
	APIBase   string
	APIType   string
	AuthType  string

	// Username 将代理绑定到指定账户。未设置时，Init 使用现有代理的
	// 所有者，或第一个 super_admin，或在数据库为空时创建管理员。
	Username    string
	Email       string
	Password    string
	DisplayName string
}

// InitResult 描述 Init 创建或更新的内容。
type InitResult struct {
	Agent             store.AgentRecord
	OwnerUsername     string
	Created           bool
	OwnerCreated      bool
	GeneratedPassword string
	ProviderSaved     bool
	ModelSaved        bool
}

// Init 在操作员的存储中创建新代理或更新现有代理。它写入与仪表盘相同的
// 表——由此创建的代理与仪表盘创建的代理地位相同。
func Init(ctx context.Context, st store.Store, name string, opts InitOptions) (*InitResult, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	displayName := strings.TrimSpace(name)

	var (
		providerName string
		fullModel    string
		pcfg         config.ProviderConfig
		saveProvider bool
	)
	if opts.Provider != "" || opts.Model != "" {
		var modelID string
		var err error
		providerName, modelID, fullModel, err = normalizeProviderModel(opts.Provider, opts.Model)
		if err != nil {
			return nil, err
		}
		pcfg, err = providerConfigFromOptions(ctx, st, providerName, modelID, opts)
		if err != nil {
			return nil, err
		}
		saveProvider = true
	}

	existing, err := lookupAgent(ctx, st, displayName, opts)
	if err != nil {
		return nil, err
	}
	// 未指定 --id 时，init 视为仅创建模式：名称冲突即为错误
	//（可能是忘记了 --id），而非隐式更新。--id 仍是更新仪表盘创建
	// 代理的文档化途径。
	if existing != nil && opts.AgentID == "" {
		return nil, fmt.Errorf("agent %q already exists (id %s); use a different name, or pass --id %s to update it", displayName, existing.ID, existing.ID)
	}

	// 更新现有代理（--id）时若未指定 --username，保留当前所有者
	// 而非查找默认管理员（该管理员可能并非代理的所有者）。
	var owner *users.Account
	var ownerCreated bool
	var generatedPassword string
	if existing != nil && opts.Username == "" {
		owner, err = loadAccount(ctx, st, existing.UserID)
		if err != nil {
			return nil, err
		}
	} else {
		owner, ownerCreated, generatedPassword, err = ensureOwner(ctx, st, opts)
		if err != nil {
			return nil, err
		}
	}

	rec, created, err := writeAgent(ctx, st, existing, displayName, owner, opts)
	if err != nil {
		return nil, err
	}

	res := &InitResult{
		Agent:             *rec,
		OwnerUsername:     owner.Username,
		Created:           created,
		OwnerCreated:      ownerCreated,
		GeneratedPassword: generatedPassword,
	}

	if saveProvider {
		if err := scope.SaveProvider(ctx, st, "", "", providerName, pcfg); err != nil {
			return nil, err
		}
		res.ProviderSaved = true
		if fullModel != "" {
			data := map[string]interface{}{}
			if cur, err := st.GetConfigByName(ctx, store.KindSetting, "", rec.ID, "agents.defaults"); err == nil && cur != nil && cur.Data != nil {
				data = cur.Data
			} else if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
			data["model"] = fullModel
			if err := scope.SaveSetting(ctx, st, "", rec.ID, "agents.defaults", data); err != nil {
				return nil, err
			}
			res.ModelSaved = true
		}
	}
	return res, nil
}

// ensureOwner 选择（或创建）代理所属的用户账户。
// 规则：--username 指定查找目标。未指定时优先使用 "admin"，
// 其次回退到最旧的活跃 super_admin，以兼容通过仪表盘配置了
// 不同名称管理员的环境。若指定用户不存在，当数据库为空时
// 创建该用户（并返回生成的密码供首次使用），否则报错。
func ensureOwner(ctx context.Context, st store.Store, opts InitOptions) (*users.Account, bool, string, error) {
	accts, err := users.NewAccounts(st)
	if err != nil {
		return nil, false, "", err
	}
	username := opts.Username
	explicit := username != ""
	if !explicit {
		username = "admin"
	}

	rec, err := st.GetUserByLogin(ctx, username)
	if err == nil {
		acct, err := accts.Get(ctx, rec.ID)
		return acct, false, "", err
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, false, "", err
	}

	count, err := st.CountUsers(ctx)
	if err != nil {
		return nil, false, "", err
	}
	if count > 0 {
		if !explicit {
			all, err := accts.List(ctx)
			if err != nil {
				return nil, false, "", err
			}
			for _, a := range all {
				if a.Role == users.RoleSuperAdmin && a.Status == users.StatusActive {
					return a, false, "", nil
				}
			}
			return nil, false, "", errors.New(`no "admin" user and no active super_admin found; pass --username to pick an existing user`)
		}
		return nil, false, "", fmt.Errorf("user %q not found; pass --username to pick an existing user", username)
	}

	password := opts.Password
	generated := ""
	if password == "" {
		password, err = randomPassword()
		if err != nil {
			return nil, false, "", err
		}
		generated = password
	}
	email := defaultStr(opts.Email, username+"@local.bkcrab")
	acct, err := accts.Create(ctx, users.CreateInput{
		Username:    username,
		Email:       email,
		Password:    password,
		DisplayName: opts.DisplayName,
		Role:        users.RoleSuperAdmin,
	})
	return acct, err == nil, generated, err
}

// lookupAgent 通过 --id 或显示名称查找现有代理记录。
// 无匹配时返回 (nil, nil)。
func lookupAgent(ctx context.Context, st store.Store, displayName string, opts InitOptions) (*store.AgentRecord, error) {
	if opts.AgentID != "" {
		rec, err := st.GetAgent(ctx, opts.AgentID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("agent id %q not found", opts.AgentID)
		}
		if err != nil {
			return nil, err
		}
		return rec, nil
	}
	return findAgentByName(ctx, st, displayName)
}

// writeAgent 更新现有代理或创建具有随机 id 的新代理。
// 重新初始化时，若 --username 指向不同账户，则拒绝静默切换所有者。
func writeAgent(ctx context.Context, st store.Store, existing *store.AgentRecord, displayName string, owner *users.Account, opts InitOptions) (*store.AgentRecord, bool, error) {
	if existing != nil {
		if existing.UserID != "" && existing.UserID != owner.ID && opts.Username != "" {
			return nil, false, fmt.Errorf("agent %q is owned by user %s; pass a matching --username or rename the agent first", displayName, existing.UserID)
		}
		if existing.UserID == "" {
			existing.UserID = owner.ID
		}
		if existing.Config == nil {
			existing.Config = map[string]interface{}{}
		}
		if opts.Description != "" {
			existing.Config["description"] = opts.Description
		}
		existing.Name = displayName
		if err := st.SaveAgent(ctx, existing); err != nil {
			return nil, false, err
		}
		return existing, false, nil
	}
	id, err := generateAgentID()
	if err != nil {
		return nil, false, err
	}
	rec := &store.AgentRecord{
		ID:     id,
		UserID: owner.ID,
		Name:   displayName,
		Config: map[string]interface{}{},
	}
	if opts.Description != "" {
		rec.Config["description"] = opts.Description
	}
	if err := st.SaveAgent(ctx, rec); err != nil {
		return nil, false, err
	}
	return rec, true, nil
}

// loadAccount 读取现有代理的用户账户。用于重新初始化时
// 在不经过 ensureOwner 的情况下保留所有者。
func loadAccount(ctx context.Context, st store.Store, userID string) (*users.Account, error) {
	accts, err := users.NewAccounts(st)
	if err != nil {
		return nil, err
	}
	return accts.Get(ctx, userID)
}

// Resolve 通过精确 id 和显示名称查找代理。代理名称可以合法地
// 以 "agt_" 开头，因此仅凭前缀不足以判断用户提供的引用类型。
func Resolve(ctx context.Context, st store.Store, ref string) (*store.AgentRecord, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, errors.New("agent name is required")
	}

	var byID *store.AgentRecord
	if rec, err := st.GetAgent(ctx, ref); err == nil {
		byID = rec
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	byName, err := findAgentByName(ctx, st, ref)
	if err != nil {
		if errors.Is(err, ErrAmbiguousName) && byID != nil {
			return nil, fmt.Errorf("agent reference %q is ambiguous: it matches an id and multiple display names; use the exact agt_ id of the intended agent", ref)
		}
		return nil, err
	}
	if byID != nil && byName != nil && byID.ID != byName.ID {
		return nil, fmt.Errorf("agent reference %q is ambiguous: it matches an id and a different display name; use the exact agt_ id of the intended agent", ref)
	}
	if byID != nil {
		return byID, nil
	}
	if byName != nil {
		return byName, nil
	}
	return nil, fmt.Errorf("agent %q not found", ref)
}

// ErrAmbiguousName 表示一个显示名称匹配到多个代理；
// 调用方应要求用户提供 agt_ id。
var ErrAmbiguousName = errors.New("multiple agents share that name; use the agt_ id instead")

func findAgentByName(ctx context.Context, st store.Store, name string) (*store.AgentRecord, error) {
	all, err := st.ListAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	var matches []store.AgentRecord
	for _, ag := range all {
		if ag.Name == name {
			matches = append(matches, ag)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil
	case 1:
		ag := matches[0]
		return &ag, nil
	default:
		return nil, ErrAmbiguousName
	}
}

// List 返回操作员存储中的所有代理，按显示名称排序。
func List(ctx context.Context, st store.Store) ([]store.AgentRecord, error) {
	all, err := st.ListAllAgents(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Name == all[j].Name {
			return all[i].ID < all[j].ID
		}
		return all[i].Name < all[j].Name
	})
	return all, nil
}

// Remove 删除代理记录及其拥有的所有系统文件。
// 系统范围内的 Provider 配置保持不变；它们在代理与仪表盘之间共享。
func Remove(ctx context.Context, st store.Store, name string) (*store.AgentRecord, error) {
	rec, err := Resolve(ctx, st, name)
	if err != nil {
		return nil, err
	}
	files, err := st.ListAgentFiles(ctx, rec.ID, rec.UserID)
	if err == nil {
		for _, f := range files {
			_ = st.DeleteAgentFile(ctx, rec.ID, rec.UserID, f)
		}
	}
	if err := st.DeleteAgent(ctx, rec.ID); err != nil {
		return nil, err
	}
	return rec, nil
}

// SetConfig 写入 provider 字段、代理范围设置（model、temperature 等）
// 或系统范围的命名空间配置。
func SetConfig(ctx context.Context, st store.Store, agentID, key, rawValue string) error {
	if strings.HasPrefix(key, "provider.") {
		return setProviderField(ctx, st, key, rawValue)
	}
	namespace, path, isAgentScope, err := settingKey(key)
	if err != nil {
		return err
	}
	uid, aid := "", ""
	if isAgentScope {
		aid = agentID
	}
	if len(path) == 0 {
		obj, ok := parseValue(rawValue).(map[string]interface{})
		if !ok {
			return fmt.Errorf("config key %q expects a JSON object value", key)
		}
		return scope.SaveSetting(ctx, st, uid, aid, namespace, obj)
	}
	data := map[string]interface{}{}
	if rec, err := st.GetConfigByName(ctx, store.KindSetting, uid, aid, namespace); err == nil && rec != nil && rec.Data != nil {
		data = rec.Data
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	setNested(data, path, parseValue(rawValue))
	if namespace == "sandbox" && len(path) > 0 && path[0] == "enabled" {
		if enabled, _ := data["enabled"].(bool); enabled {
			if _, ok := data["backend"].(string); !ok {
				data["backend"] = "docker"
			}
		}
	}
	return scope.SaveSetting(ctx, st, uid, aid, namespace, data)
}

// GetConfig 返回单个配置值，或在 key 为空时返回代理范围与
// 系统范围的全部已保存配置视图。
func GetConfig(ctx context.Context, st store.Store, agentID, key string) (interface{}, error) {
	if key == "" {
		return configDump(ctx, st, agentID)
	}
	if strings.HasPrefix(key, "provider.") {
		return getProviderField(ctx, st, key)
	}
	namespace, path, isAgentScope, err := settingKey(key)
	if err != nil {
		return nil, err
	}
	uid, aid := "", ""
	if isAgentScope {
		aid = agentID
	}
	rec, err := st.GetConfigByName(ctx, store.KindSetting, uid, aid, namespace)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return rec.Data, nil
	}
	return getNested(rec.Data, path), nil
}

// PutFile 将系统文件写入 agent_files 表中该代理的行。
func PutFile(ctx context.Context, st store.Store, agentID, userID, filename string, data []byte) error {
	if err := validateSystemFilename(filename); err != nil {
		return err
	}
	return st.SaveAgentFile(ctx, agentID, userID, filename, data)
}

// GetFile 读取一个系统文件。
func GetFile(ctx context.Context, st store.Store, agentID, userID, filename string) ([]byte, error) {
	if err := validateSystemFilename(filename); err != nil {
		return nil, err
	}
	return st.GetAgentFile(ctx, agentID, userID, filename)
}

// ListFiles 列出代理的系统文件（经过允许列表过滤）。
func ListFiles(ctx context.Context, st store.Store, agentID, userID string) ([]string, error) {
	files, err := st.ListAgentFiles(ctx, agentID, userID)
	if err != nil {
		return nil, err
	}
	out := files[:0]
	for _, file := range files {
		if systemFileAllowlist[file] {
			out = append(out, file)
		}
	}
	return out, nil
}

func normalizeProviderModel(providerName, model string) (string, string, string, error) {
	providerName = strings.TrimSpace(providerName)
	model = strings.TrimSpace(model)
	modelID := model
	if strings.Contains(model, "/") {
		parts := strings.SplitN(model, "/", 2)
		if parts[0] == "" || parts[1] == "" {
			return "", "", "", fmt.Errorf("invalid model %q: expected <provider>/<model>", model)
		}
		if providerName != "" && providerName != parts[0] {
			return "", "", "", fmt.Errorf("--provider %q does not match model provider prefix %q", providerName, parts[0])
		}
		if providerName == "" {
			providerName = parts[0]
		}
		modelID = parts[1]
	}
	if providerName == "" {
		return "", "", "", errors.New("--provider is required when --model does not include a provider prefix")
	}
	fullModel := ""
	if modelID != "" {
		fullModel = providerName + "/" + modelID
	}
	return providerName, modelID, fullModel, nil
}

func providerConfigFromOptions(ctx context.Context, st store.Store, providerName, modelID string, opts InitOptions) (config.ProviderConfig, error) {
	preset := providerPreset(providerName)
	existing := config.ProviderConfig{}
	if rec, err := st.GetConfigByName(ctx, store.KindProvider, "", "", providerName); err == nil && rec != nil {
		blob, _ := json.Marshal(rec.Data)
		_ = json.Unmarshal(blob, &existing)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return config.ProviderConfig{}, err
	}
	apiBase := firstNonEmpty(opts.APIBase, existing.APIBase, preset.apiBase)
	apiType := firstNonEmpty(opts.APIType, existing.APIType, preset.apiType)
	authType := firstNonEmpty(opts.AuthType, existing.AuthType, preset.authType)
	apiKeyEnv := opts.APIKeyEnv
	if apiKeyEnv == "" {
		apiKeyEnv = preset.apiKeyEnv
	}
	apiKey := existing.APIKey
	if opts.APIKeyEnv != "" || (apiKey == "" && apiKeyEnv != "") {
		apiKey = os.Getenv(apiKeyEnv)
		if apiKey == "" && providerName != "ollama" {
			return config.ProviderConfig{}, fmt.Errorf("environment variable %s is empty", apiKeyEnv)
		}
	}
	if apiKey == "" && providerName == "ollama" {
		apiKey = "ollama"
	}
	cfg := config.ProviderConfig{
		APIKey:   apiKey,
		APIBase:  apiBase,
		APIType:  apiType,
		AuthType: authType,
		Models:   existing.Models,
	}
	if modelID != "" {
		cfg.Models = appendModel(cfg.Models, modelID)
	}
	return cfg, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

type providerDefaults struct {
	apiBase   string
	apiType   string
	authType  string
	apiKeyEnv string
}

func providerPreset(name string) providerDefaults {
	switch strings.ToLower(name) {
	case "anthropic":
		return providerDefaults{"https://api.anthropic.com", "anthropic-messages", "api-key", "ANTHROPIC_API_KEY"}
	case "openrouter":
		return providerDefaults{"https://openrouter.ai/api/v1", "openai-chat", "bearer-token", "OPENROUTER_API_KEY"}
	case "ollama":
		return providerDefaults{"http://localhost:11434/v1", "openai-chat", "bearer-token", ""}
	case "groq":
		return providerDefaults{"https://api.groq.com/openai/v1", "openai-chat", "bearer-token", "GROQ_API_KEY"}
	case "deepseek":
		return providerDefaults{"https://api.deepseek.com", "openai-chat", "bearer-token", "DEEPSEEK_API_KEY"}
	case "mistral":
		return providerDefaults{"https://api.mistral.ai/v1", "openai-chat", "bearer-token", "MISTRAL_API_KEY"}
	case "openai":
		fallthrough
	default:
		return providerDefaults{"https://api.openai.com/v1", "openai-chat", "bearer-token", "OPENAI_API_KEY"}
	}
}

func setProviderField(ctx context.Context, st store.Store, key, rawValue string) error {
	parts := strings.Split(strings.TrimPrefix(key, "provider."), ".")
	if len(parts) != 2 {
		return errors.New("provider config key must look like provider.<name>.<field>")
	}
	name, field := parts[0], parts[1]
	pc := config.ProviderConfig{}
	if rec, err := st.GetConfigByName(ctx, store.KindProvider, "", "", name); err == nil && rec != nil {
		blob, _ := json.Marshal(rec.Data)
		_ = json.Unmarshal(blob, &pc)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	} else {
		preset := providerPreset(name)
		pc = config.ProviderConfig{APIBase: preset.apiBase, APIType: preset.apiType, AuthType: preset.authType}
	}
	switch field {
	case "apiKeyEnv":
		pc.APIKey = os.Getenv(rawValue)
		if pc.APIKey == "" {
			return fmt.Errorf("environment variable %s is empty", rawValue)
		}
	case "apiKey":
		pc.APIKey = rawValue
	case "apiBase":
		pc.APIBase = rawValue
	case "apiType":
		pc.APIType = rawValue
	case "authType":
		pc.AuthType = rawValue
	case "model":
		if rawValue == "" {
			return errors.New("provider model id is required; use `provider.<name>.models []` to clear")
		}
		pc.Models = appendModel(pc.Models, rawValue)
	case "models":
		if rawValue != "[]" {
			return errors.New(`only "[]" is accepted; use provider.<name>.model <id> to add entries`)
		}
		pc.Models = nil
	default:
		return fmt.Errorf("unsupported provider field %q", field)
	}
	return scope.SaveProvider(ctx, st, "", "", name, pc)
}

func getProviderField(ctx context.Context, st store.Store, key string) (interface{}, error) {
	parts := strings.Split(strings.TrimPrefix(key, "provider."), ".")
	if len(parts) != 2 {
		return nil, errors.New("provider config key must look like provider.<name>.<field>")
	}
	rec, err := st.GetConfigByName(ctx, store.KindProvider, "", "", parts[0])
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return redactProviderData(rec.Data)[parts[1]], nil
}

func appendModel(models []config.ModelEntry, id string) []config.ModelEntry {
	for _, m := range models {
		if m.ID == id {
			return models
		}
	}
	return append(models, config.ModelEntry{ID: id, Name: id})
}

// agentScopeKeys 将顶层 CLI 键映射到它们在代理行中存储的
// 命名空间+路径。这些键与仪表盘为每个代理暴露的键相同。
var agentScopeKeys = map[string]string{
	"model":                "agents.defaults",
	"maxTokens":            "agents.defaults",
	"temperature":          "agents.defaults",
	"maxToolIterations":    "agents.defaults",
	"maxParallelToolCalls": "agents.defaults",
	"thinking":             "agents.defaults",
	"policy":               "agents.defaults",
	// promptMode 选择 BuildSystemPromptAs 生成的框架部分以及 LLM
	// 可见的内置工具。取值为 "agent"、"chatbot" 或 "customize"。
	// 以纯字符串形式存储于 agents.defaults.promptMode 下。
	// 每种模式对应的内置工具集硬编码在 builtinAllowForMode
	// （internal/agent/loop.go）中——自定义工具来自 Plugin / MCP，
	// 而非每个代理的允许列表。
	"promptMode": "agents.defaults",
	// splitReplies —— 每个代理的多气泡切换。为 true 时，调度器
	// 在发送到任何 IM 通道（微信 / Telegram / Discord / Slack / LINE /
	// 飞书）之前，在 SplitMessageMarker 处拆分回复。
	// 系统级回退已移除——当该键不存在时默认为 false。
	"splitReplies": "agents.defaults",
	"sandbox":      "sandbox",
}

var systemSettingNamespaces = []string{
	"skills.install",
	"skills.entries",
	"tools.providers",
	"tools.categories",
	"skillsLearner",
	"objectstore",
	"taskqueue",
	"heartbeat",
	"plugins",
	"memory",
	"privacy",
	"hooks",
	"teams",
}

// settingKey 将 CLI 键解析为 (命名空间, 数据内路径, 作用域)。
// 代理范围的键涵盖 model/temperature/sandbox；其余为系统级命名空间。
// 布尔返回值表示 "isAgentScope"——true 表示行的 agent_id 应设为
// 当前 agentID；false 表示系统行（user_id="", agent_id=""）。
func settingKey(key string) (string, []string, bool, error) {
	if ns, ok := agentScopeKeys[key]; ok {
		path := []string{key}
		if key == ns { // sandbox -> sandbox：整个命名空间，无内部路径
			path = nil
		}
		return ns, path, true, nil
	}
	if strings.HasPrefix(key, "sandbox.") {
		return "sandbox", strings.Split(strings.TrimPrefix(key, "sandbox."), "."), true, nil
	}
	for _, ns := range systemSettingNamespaces {
		if key == ns {
			return ns, nil, false, nil
		}
		prefix := ns + "."
		if strings.HasPrefix(key, prefix) {
			path := strings.Split(strings.TrimPrefix(key, prefix), ".")
			if len(path) > 0 && path[0] != "" {
				return ns, path, false, nil
			}
		}
	}
	return "", nil, false, fmt.Errorf("unsupported config key %q", key)
}

func configDump(ctx context.Context, st store.Store, agentID string) (map[string]interface{}, error) {
	out := map[string]interface{}{}

	agentRec, err := st.GetAgent(ctx, agentID)
	if err == nil && agentRec != nil {
		out["agent"] = map[string]interface{}{
			"id":     agentRec.ID,
			"name":   agentRec.Name,
			"userId": agentRec.UserID,
			"config": agentRec.Config,
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	agentSettings := map[string]interface{}{}
	for _, ns := range []string{"agents.defaults", "sandbox"} {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, "", agentID, ns)
		if errors.Is(err, store.ErrNotFound) || rec == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		agentSettings[ns] = rec.Data
	}
	if len(agentSettings) > 0 {
		out["agentScope"] = agentSettings
	}

	sysSettings := map[string]interface{}{}
	settings, err := st.ListConfigs(ctx, store.KindSetting, "", "")
	if err != nil {
		return nil, err
	}
	for _, rec := range settings {
		sysSettings[rec.Name] = rec.Data
	}
	if len(sysSettings) > 0 {
		out["system"] = sysSettings
	}
	providers, err := st.ListConfigs(ctx, store.KindProvider, "", "")
	if err != nil {
		return nil, err
	}
	if len(providers) > 0 {
		provOut := map[string]interface{}{}
		for _, rec := range providers {
			provOut[rec.Name] = redactProviderData(rec.Data)
		}
		out["providers"] = provOut
	}
	return out, nil
}

func redactProviderData(data map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(data))
	for k, v := range data {
		if k == "apiKey" {
			if s, _ := v.(string); s != "" {
				out[k] = "<set>"
			} else {
				out[k] = ""
			}
			continue
		}
		out[k] = v
	}
	return out
}

func setNested(data map[string]interface{}, path []string, value interface{}) {
	cur := data
	for _, p := range path[:len(path)-1] {
		next, _ := cur[p].(map[string]interface{})
		if next == nil {
			next = map[string]interface{}{}
			cur[p] = next
		}
		cur = next
	}
	cur[path[len(path)-1]] = value
}

func getNested(data map[string]interface{}, path []string) interface{} {
	var cur interface{} = data
	for _, p := range path {
		m, ok := cur.(map[string]interface{})
		if !ok {
			return nil
		}
		cur = m[p]
	}
	return cur
}

// parseValue 将原始 CLI 字符串尽可能转换为有类型的 JSON 值。
// JSON 处理布尔值/数字/对象；未加引号的字符串直接作为原始字符串保留。
func parseValue(raw string) interface{} {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err == nil {
		return v
	}
	return raw
}

var systemFileAllowlist = map[string]bool{
	"SOUL.md":      true,
	"IDENTITY.md":  true,
	"USER.md":      true,
	"BOOTSTRAP.md": true,
	"MEMORY.md":    true,
	"HEARTBEAT.md": true,
	"AGENTS.md":    true,
	"TOOLS.md":     true,
	"agent.json":   true,
}

func validateSystemFilename(filename string) error {
	if !systemFileAllowlist[filename] {
		return fmt.Errorf("filename %q is not a supported agent system file", filename)
	}
	return nil
}

func generateAgentID() (string, error) {
	var b [9]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "agt_" + hex.EncodeToString(b[:]), nil
}

func randomPassword() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
