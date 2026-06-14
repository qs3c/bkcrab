// Package scope 从 store.configs 中读取以 (user, agent) 为键的行，
// 并将其合并为运行时所需的扁平结构。
//
// configs 中的每一行都包含 (kind, user_id, agent_id, name) 元组。
// 解析按所有权从外到内遍历，内层行按 `name` 遮蔽外层行：
//
//	system (user='', agent='') →
//	  user (user=X, agent='')   →
//	    agent (user='', agent=Y) →
//	      per-(user, agent) (user=X, agent=Y)
//
// kind="provider"：name 是提供者键（如 "openai"）。内层行
//
//	完全替换外层条目（无字段级合并）。
//
// kind="channel"：name 是通道类型（如 "telegram"）。被禁用的内层
//
//	行会擦除外层条目 —— 允许用户退出系统级机器人。
//
// kind="setting"：name 是命名空间（如 "agents.defaults", "sandbox" 等）。
//
//	顶层键按字段合并；内层作用域的键优先。
package scope

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/store"
)

// HTTP 层的作用域标识符。存储层直接以 (user_id, agent_id) 为键；
// 这些常量用于 HTTP API 契约（URL ?scope= 参数、仪表盘作用域选择器）。
// 通过 OwnershipFromScope 转换为存储形式。
const (
	System = "system"
	User   = "user"
	Agent  = "agent"
)

// OwnershipFromScope 将 HTTP 端的 (scope, scopeID) 对转换为
// 存储端的 (user_id, agent_id) 对。空/未知作用域返回 ("", "")，
// 存储层将其解读为 "system / global"。
func OwnershipFromScope(sc, scopeID string) (userID, agentID string) {
	switch sc {
	case User:
		return scopeID, ""
	case Agent:
		return "", scopeID
	default:
		return "", ""
	}
}

// ScopeFromOwnership 是逆操作，用于将 (scope, scopeID) 输出回仪表盘 JSON。
// (X, Y) —— 两者都填写 —— 渲染为 scope="user-agent"，以便 UI 区分
// 每 (user, agent) 覆盖与普通 user 或 agent 行。当前仪表盘仅读取
// scope="system"/"user"/"agent"；新的复合形式为多租户视图保留扩展性。
func ScopeFromOwnership(userID, agentID string) (scope, scopeID string) {
	switch {
	case userID != "" && agentID != "":
		return "user-agent", userID + "/" + agentID
	case userID != "":
		return User, userID
	case agentID != "":
		return Agent, agentID
	default:
		return System, ""
	}
}

// Providers 返回给定 (user, agent) 的合并后的 LLM 提供者配置映射。
// 传入 agentID="" 仅获取用户级视图。两者都为空则仅获取系统级。
func Providers(ctx context.Context, st store.ConfigStore, userID, agentID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.Providers: store is required")
	}
	out := map[string]config.ProviderConfig{}
	apply := func(rows []store.ConfigRecord) {
		for _, r := range rows {
			out[r.Name] = providerToConfig(r)
		}
	}
	// 系统层
	if rows, err := st.ListConfigs(ctx, store.KindProvider, "", ""); err != nil {
		return nil, err
	} else {
		apply(rows)
	}
	// 用户层
	if userID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, userID, ""); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	// 智能体层
	if agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, "", agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	// 每 (user, agent) 层
	if userID != "" && agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindProvider, userID, agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	return out, nil
}

// AgentScopeProviders 仅返回存储在 (user="", agent=Y) 的提供者 ——
// 智能体的"官方"行，不包含系统层或用户层的合并。
// 用于在已合并系统+用户的视图之上叠加智能体自身的行：
// 重新运行完整的 Providers 遍历会重新应用外层，并静默覆盖调用方已合并的任何用户作用域覆盖。
func AgentScopeProviders(ctx context.Context, st store.ConfigStore, agentID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.AgentScopeProviders: store is required")
	}
	if agentID == "" {
		return map[string]config.ProviderConfig{}, nil
	}
	rows, err := st.ListConfigs(ctx, store.KindProvider, "", agentID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.ProviderConfig, len(rows))
	for _, r := range rows {
		out[r.Name] = providerToConfig(r)
	}
	return out, nil
}

// UserScopeProviders 仅返回存储在 (user=X, agent="") 的提供者 ——
// 用户的个人行，不包含系统层。由外部智能体路径使用，以便查看者可以
// 回退到所有者的提供者凭据，而无需拖入所有者的完整合并视图
// （后者会在查看者已合并的集合之上重新应用系统行）。
func UserScopeProviders(ctx context.Context, st store.ConfigStore, userID string) (map[string]config.ProviderConfig, error) {
	if st == nil {
		return nil, errors.New("scope.UserScopeProviders: store is required")
	}
	if userID == "" {
		return map[string]config.ProviderConfig{}, nil
	}
	rows, err := st.ListConfigs(ctx, store.KindProvider, userID, "")
	if err != nil {
		return nil, err
	}
	out := make(map[string]config.ProviderConfig, len(rows))
	for _, r := range rows {
		out[r.Name] = providerToConfig(r)
	}
	return out, nil
}

// Channels 返回合并后的通道映射。内层作用域中被禁用的行会擦除外层条目。
func Channels(ctx context.Context, st store.ConfigStore, userID, agentID string) (map[string]config.ChannelConfig, error) {
	if st == nil {
		return nil, errors.New("scope.Channels: store is required")
	}
	out := map[string]config.ChannelConfig{}
	apply := func(rows []store.ConfigRecord) {
		for _, r := range rows {
			if !r.Enabled {
				delete(out, r.Name)
				continue
			}
			out[r.Name] = channelToConfig(r)
		}
	}
	if rows, err := st.ListConfigs(ctx, store.KindChannel, "", ""); err != nil {
		return nil, err
	} else {
		apply(rows)
	}
	if userID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, userID, ""); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	if agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, "", agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	if userID != "" && agentID != "" {
		if rows, err := st.ListConfigs(ctx, store.KindChannel, userID, agentID); err != nil {
			return nil, err
		} else {
			apply(rows)
		}
	}
	return out, nil
}

// Setting 返回跨 system → user → agent → per-(user, agent) 链的
// 单个命名空间的合并 JSON。在顶层映射上进行字段级合并；
// 内层所有权的字段覆盖外层。未设置的命名空间返回空映射且不报错 ——
// 调用方将其 Unmarshal 到类型化结构体中，依赖零值字段。
func Setting(ctx context.Context, st store.ConfigStore, namespace, userID, agentID string) (map[string]interface{}, error) {
	if st == nil {
		return nil, errors.New("scope.Setting: store is required")
	}
	out := map[string]interface{}{}
	merge := func(layer map[string]interface{}) {
		for k, v := range layer {
			out[k] = v
		}
	}
	tryGet := func(uid, aid string) error {
		rec, err := st.GetConfigByName(ctx, store.KindSetting, uid, aid, namespace)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			return err
		}
		if rec != nil {
			merge(rec.Data)
		}
		return nil
	}
	if err := tryGet("", ""); err != nil {
		return nil, err
	}
	if userID != "" {
		if err := tryGet(userID, ""); err != nil {
			return nil, err
		}
	}
	if agentID != "" {
		if err := tryGet("", agentID); err != nil {
			return nil, err
		}
	}
	if userID != "" && agentID != "" {
		if err := tryGet(userID, agentID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SettingInto 解析 Setting 并将合并后的 JSON 反序列化到 dst 中。
// 为需要类型化配置块的调用方提供的便利方法。
func SettingInto(ctx context.Context, st store.ConfigStore, namespace, userID, agentID string, dst interface{}) error {
	merged, err := Setting(ctx, st, namespace, userID, agentID)
	if err != nil {
		return err
	}
	if len(merged) == 0 {
		return nil
	}
	blob, err := json.Marshal(merged)
	if err != nil {
		return err
	}
	return json.Unmarshal(blob, dst)
}

// SaveSettingByScope 是为 HTTP 层保留的旧版 (scope, scopeID) 形式，
// HTTP 层仍在 URL 参数和 JSON 中输出作用域字符串。
// 新调用方应使用带显式 (userID, agentID) 的 SaveSetting。
func SaveSettingByScope(ctx context.Context, st store.ConfigStore, sc, scopeID, namespace string, data map[string]interface{}) error {
	uid, aid := OwnershipFromScope(sc, scopeID)
	return SaveSetting(ctx, st, uid, aid, namespace, data)
}

// SaveProviderByScope / SaveChannelByScope 镜像相同的旧版桥接。
func SaveProviderByScope(ctx context.Context, st store.ConfigStore, sc, scopeID, name string, p config.ProviderConfig) error {
	uid, aid := OwnershipFromScope(sc, scopeID)
	return SaveProvider(ctx, st, uid, aid, name, p)
}

func SaveChannelByScope(ctx context.Context, st store.ConfigStore, sc, scopeID, channelType, credentialKey string, enabled bool, c config.ChannelConfig) error {
	uid, aid := OwnershipFromScope(sc, scopeID)
	return SaveChannel(ctx, st, uid, aid, channelType, credentialKey, enabled, c)
}

// SaveSetting 在给定的 (user, agent) 所有权下插入或更新单个命名空间。
// 传入 nil/空数据将删除该行（而非写入 {}）。传入空 userID/agentID 表示系统级。
func SaveSetting(ctx context.Context, st store.ConfigStore, userID, agentID, namespace string, data map[string]interface{}) error {
	if st == nil {
		return errors.New("scope.SaveSetting: store is required")
	}
	if len(data) == 0 {
		// 查找并删除存在的行。幂等：行不存在则为空操作。
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, userID, agentID, namespace); err == nil && rec != nil {
			return st.DeleteConfig(ctx, rec.ID)
		}
		return nil
	}
	rec := &store.ConfigRecord{
		Kind:    store.KindSetting,
		UserID:  userID,
		AgentID: agentID,
		Name:    namespace,
		Enabled: true,
		Data:    data,
	}
	return st.SaveConfig(ctx, rec)
}

// SaveProvider 在给定的 (user, agent) 所有权下插入或更新 kind="provider" 行。
func SaveProvider(ctx context.Context, st store.ConfigStore, userID, agentID, name string, p config.ProviderConfig) error {
	rec := &store.ConfigRecord{
		Kind:    store.KindProvider,
		UserID:  userID,
		AgentID: agentID,
		Name:    name,
		Enabled: true,
		Data:    providerToData(p),
	}
	return st.SaveConfig(ctx, rec)
}

// SaveChannel 在给定的 (user, agent) 所有权下插入或更新 kind="channel" 行。
// credentialKey 是用于入站调度的稳定查找句柄（机器人令牌尾部、应用 ID）。
func SaveChannel(ctx context.Context, st store.ConfigStore, userID, agentID, channelType, credentialKey string, enabled bool, c config.ChannelConfig) error {
	rec := &store.ConfigRecord{
		Kind:          store.KindChannel,
		UserID:        userID,
		AgentID:       agentID,
		Name:          channelType,
		Enabled:       enabled,
		CredentialKey: credentialKey,
		Data:          channelToData(c),
	}
	return st.SaveConfig(ctx, rec)
}

func providerToConfig(r store.ConfigRecord) config.ProviderConfig {
	pc := config.ProviderConfig{}
	if blob, err := json.Marshal(r.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &pc)
	}
	return pc
}

func providerToData(p config.ProviderConfig) map[string]interface{} {
	blob, _ := json.Marshal(p)
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	return m
}

func channelToConfig(r store.ConfigRecord) config.ChannelConfig {
	cc := config.ChannelConfig{Enabled: r.Enabled}
	if blob, err := json.Marshal(r.Data); err == nil && len(blob) > 0 {
		_ = json.Unmarshal(blob, &cc)
	}
	cc.Enabled = r.Enabled
	return cc
}

func channelToData(c config.ChannelConfig) map[string]interface{} {
	blob, _ := json.Marshal(c)
	var m map[string]interface{}
	_ = json.Unmarshal(blob, &m)
	delete(m, "enabled") // enabled 存在于行列上，不在 data 中
	return m
}
