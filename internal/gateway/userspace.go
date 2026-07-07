package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/plugin"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/sandbox"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/session"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/usage"
	"github.com/qs3c/bkcrab/internal/workspace"
)

// loadAgentSkillEntries 收集此用户拥有的每个代理作用域 skills.entries 行。
// 镜像 HTTP 层中的相同逻辑；保留在此处以便运行时网关永远不会导入设置处理程序包。
func loadAgentSkillEntries(ctx context.Context, st store.Store, userID string) (map[string]map[string]config.SkillEntryCfg, error) {
	if st == nil {
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

// ensureAgentHome 幂等地创建代理的本地文件系统布局。只有 `skills/`（文件系统物化的 SKILL.md 包）
// 和 `memory/`（压缩将历史 JSONL 转储到此用于审计/恢复）存在于磁盘上；
// 身份文件、会话消息和 MEMORY.md 都在数据库中。
func ensureAgentHome(rc config.ResolvedAgent) {
	if rc.Home == "" {
		return
	}
	for _, dir := range []string{
		rc.Home,
		filepath.Join(rc.Home, "skills"),
		filepath.Join(rc.Home, "memory", "logs"),
	} {
		_ = os.MkdirAll(dir, 0o755)
	}
}

// globalSkillsDirPath 返回 ~/.bkcrab/skills。
func globalSkillsDirPath() (string, error) {
	home, err := config.HomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "skills"), nil
}

// buildSystemSandboxPool 从系统作用域沙箱配置构建网关范围的沙箱池。
// 当沙箱在系统作用域未启用时返回 nil（每个用户空间随后不附加池，exec 回退到每个代理的路径根）。
//
// 存在于网关作用域，而不是每个 UserSpace。先前的设计为每个用户构建一个池，
// 这（a）在共享相同镜像的用户之间重复了 docker 池，并且（b）使临时的 UserSpace
// （特别是 API 密钥调用者被切换到的 `app_user` 身份）没有任何池 —
// 这些 UserSpace 有零个自己的代理，因此每个用户的构建器使用 `resolved=[]` 运行并产生 nil。
// 延迟注入的代理（super_admin 聊天、app 模式访问）然后使用启用沙箱但没有执行器的 exec 运行，
// 并向用户显示"sandbox required but no executor available"。将池提升到网关作用域
// 使借用路径成为每个 UserSpace 的默认配置。
func buildSystemSandboxPool(cfg config.SandboxCfg, ws workspace.Store) sandbox.ExecutorPool {
	if !cfg.Enabled {
		return nil
	}
	var inner sandbox.ExecutorPool
	home, _ := config.HomeDir()
	// 优先使用每个后端的镜像字段（DockerImage / E2BTemplate / BoxliteSnapshot）；
	// 对于早于拆分的配置，回退到遗留的共享 Image 槽位。
	switch cfg.Backend {
	case "e2b":
		apiKey := cfg.E2BKey
		if apiKey == "" {
			apiKey = os.Getenv("E2B_API_KEY")
		}
		template := cfg.E2BTemplate
		if template == "" {
			template = cfg.Image
		}
		if template == "" {
			template = "base"
		}
		inner = sandbox.NewE2BExecutorPool(apiKey, template, home, 30*time.Minute)
		slog.Info("system sandbox executor pool created",
			"backend", "e2b", "template", template)
	case "boxlite":
		secret := cfg.BoxliteKey
		if secret == "" {
			secret = os.Getenv("BOXLITE_API_KEY")
		}
		snapshot := cfg.BoxliteSnapshot
		if snapshot == "" {
			snapshot = cfg.Image
		}
		inner = sandbox.NewBoxliteExecutorPool(
			cfg.BoxliteURL,
			cfg.BoxlitePrefix,
			cfg.BoxliteClientID,
			secret,
			snapshot,
			home,
			30*time.Minute,
		)
		slog.Info("system sandbox executor pool created",
			"backend", "boxlite", "image", snapshot, "url", cfg.BoxliteURL)
	default:
		image := cfg.DockerImage
		if image == "" {
			image = cfg.Image
		}
		policy := &sandbox.Policy{NetMode: cfg.Network}
		inner = sandbox.NewDockerExecutorPool(image, home, policy)
		slog.Info("system sandbox executor pool created",
			"backend", "docker", "network", cfg.Network)
	}
	idle := time.Duration(cfg.IdleTTLSec) * time.Second
	if idle <= 0 {
		idle = 10 * time.Minute
	}
	lp := sandbox.NewLifecyclePool(inner, idle, 30*time.Second)
	if ws != nil {
		lp.SetWorkspace(ws)
	}
	lp.Start()
	slog.Info("system sandbox lifecycle pool enabled",
		"idleTTL", idle, "hydrate", ws != nil)
	return lp
}

// attachSandboxToAgents 将网关的共享沙箱池挂接到 `agentMgr` 中的每个代理。
// 当 `systemPool` 为 nil（沙箱已禁用或未在系统作用域配置）时，回退到仅路径模式：
// 每个代理的文件工具被限制在自己的工作空间目录中。
//
// 池所有权停留在网关：UserSpace 驱逐绝不能关闭池。返回的引用是网关持有的同一个指针 —
// 保存在 UserSpace.SandboxPool 上，以便每个请求的热路径（延迟注入代理的 EnsureAgent）
// 可以获取它而无需回溯到网关。
func attachSandboxToAgents(
	systemPool sandbox.ExecutorPool,
	userID string,
	resolved []config.ResolvedAgent,
	agentMgr *agent.Manager,
) sandbox.ExecutorPool {
	if systemPool != nil {
		for _, ag := range agentMgr.All() {
			ag.SetSandboxPool(systemPool)
		}
		return systemPool
	}
	for _, rc := range resolved {
		if rc.Workspace == "" {
			continue
		}
		_ = os.MkdirAll(rc.Workspace, 0o755)
		if ag := agentMgr.AgentByID(rc.ID); ag != nil {
			ag.ToolRegistry().SetSandboxRoot(rc.Workspace)
		}
	}
	slog.Info("path sandbox enabled (no system pool configured)", "user", userID)
	return nil
}

// assembleConfig 读取命名空间的设置行和作用域合并的提供者/通道对于一个 (account, agent)，
// 并将它们投影到运行时 config.Config 中。传递 userID="" / agentID="" 以跳过这些层
// （代理启动使用仅用户视图；仅系统用于 super_admin 仪表盘）。
//
// 每个设置命名空间是其自己的配置行。assembleConfig 依次读取它们全部
// （概念上并行但为了简单起见串行）；每个命名空间的成本是一次索引点查找。
func assembleConfig(ctx context.Context, st store.Store, userID, agentID string) (*config.Config, error) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{},
		Channels:  map[string]config.ChannelConfig{},
	}
	if st == nil {
		return cfg, nil
	}
	if err := scope.SettingInto(ctx, st, NSAgentDefaults, userID, agentID, &cfg.Agents.Defaults); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSandbox, userID, agentID, &cfg.Sandbox); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSObjectStore, userID, agentID, &cfg.ObjectStore); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSHooks, userID, agentID, &cfg.Hooks); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSPlugins, userID, agentID, &cfg.Plugins); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSTaskQueue, userID, agentID, &cfg.TaskQueue); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSToolProviders, userID, agentID, &cfg.ToolProviders); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSToolCategories, userID, agentID, &cfg.Tools); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSkillsInstall, userID, agentID, &cfg.Skills.Install); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSkillsEntries, userID, agentID, &cfg.Skills.Entries); err != nil {
		return nil, err
	}
	// 每个代理的技能环境覆盖过去存在于一个以 agentID 为键的单用户作用域行中；
	// 现在它们每个持久化为一个 scope=agent 行，名称为 skills.entries
	//（相同命名空间，更窄的作用域）。收集此用户拥有的每个代理 —
	// 代理运行时仍然需要通过 cfg.Skills.AgentEntries 获取以代理为键的映射形状。
	if userID != "" {
		entries, err := loadAgentSkillEntries(ctx, st, userID)
		if err != nil {
			return nil, err
		}
		if len(entries) > 0 {
			cfg.Skills.AgentEntries = entries
		}
	}
	if err := scope.SettingInto(ctx, st, NSMemory, userID, agentID, &cfg.Memory); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSPrivacy, userID, agentID, &cfg.Privacy); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSSkillsLearner, userID, agentID, &cfg.SkillsLearner); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSHeartbeat, userID, agentID, &cfg.Heartbeat); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSTeams, userID, agentID, &cfg.Teams); err != nil {
		return nil, err
	}
	if err := scope.SettingInto(ctx, st, NSBindings, userID, agentID, &cfg.Bindings); err != nil {
		return nil, err
	}
	provs, err := scope.Providers(ctx, st, userID, agentID)
	if err != nil {
		return nil, err
	}
	for k, v := range provs {
		cfg.Providers[k] = v
	}
	chs, err := scope.Channels(ctx, st, userID, agentID)
	if err != nil {
		return nil, err
	}
	for k, v := range chs {
		cfg.Channels[k] = v
	}
	return cfg, nil
}

// UserSpace 持有每个用户的运行时：其配置快照、LLM 提供者、代理管理器和沙箱池引用。
// 在首次认证时延迟加载。
//
// SandboxPool 从网关**借用** — 每个 UserSpace 共享同一个指针
// （或在系统作用域禁用沙箱时为 nil）。驱逐绝不能在其上调用 CloseAll；
// 网关拥有生命周期并在关闭时一次性销毁它。
type UserSpace struct {
	UserID      string
	Config      *config.Config
	Provider    provider.Provider
	Agents      *agent.Manager
	SandboxPool sandbox.ExecutorPool
	// PluginMgr 从网关借用（进程范围单例）。在此持有以便 EnsureAgent —
	// 外部代理附加路径 — 可以将钩子插件注册到延迟构建的代理上，
	// 而无需回溯到网关。当 systemPlugins 关闭时为 nil。
	PluginMgr *plugin.Manager

	mu sync.Mutex
}

// readUserScopeAgentDefaults 读取 (user=X, agent="") agents.defaults 行的原始数据 —
// 与 assembleConfig 不同，后者合并系统+用户且无法区分"用户显式选择了系统值"和"根本没有用户作用域行"。
// EnsureAgent 使用此函数检测聊天者的*显式*模型偏好，因此它可以胜过所有者/代理作用域对外部代理的覆盖。
// 当没有行、行数据无法反序列化或 userID 为空（系统调用者 — 没有要遵守的每个用户固定）时返回零值。
func readUserScopeAgentDefaults(ctx context.Context, st store.Store, userID string) config.AgentDefaults {
	var out config.AgentDefaults
	if userID == "" || st == nil {
		return out
	}
	rec, err := st.GetConfigByName(ctx, store.KindSetting, userID, "", NSAgentDefaults)
	if err != nil || rec == nil {
		return out
	}
	blob, err := json.Marshal(rec.Data)
	if err != nil {
		return out
	}
	_ = json.Unmarshal(blob, &out)
	return out
}

// EnsureAgent 将一个用户不拥有的代理附加到此 UserSpace。
// 由 super_admin 聊天使用：管理员在其自己的 user_id 命名空间下操作外部代理
// （会话、内存、mem0 作用域都保持调用者键控），而代理的持久身份 —
// 系统提示、代理作用域配置 (`agents.defaults`)、技能和 agent_files —
// 被重用，因为它们在存储中以 agent_id 为键，而不是 user_id 为键。
//
// 幂等的：如果代理已加载则返回 nil。
func (sp *UserSpace) EnsureAgent(ctx context.Context, st store.Store, mb *bus.MessageBus, ws workspace.Store, agentID string) error {
	if sp == nil || sp.Agents == nil {
		return fmt.Errorf("EnsureAgent: nil UserSpace")
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.Agents.AgentByID(agentID) != nil {
		return nil
	}
	if st == nil {
		return fmt.Errorf("EnsureAgent: store required")
	}
	rec, err := st.GetAgent(ctx, agentID)
	if err != nil || rec == nil {
		return fmt.Errorf("EnsureAgent: agent %q not found", agentID)
	}
	resolved := config.ResolveAgents(sp.Config, []config.AgentEntry{{ID: rec.ID, UserID: rec.UserID, Name: rec.Name}})
	if len(resolved) != 1 {
		return fmt.Errorf("EnsureAgent: ResolveAgents returned %d entries", len(resolved))
	}
	rc := resolved[0]
	// 所有者回退层：当调用 UserSpace 不是代理所有者时
	//（super_admin、公开链接查看器、apikey 共享用户），拉取所有者的用户作用域设置/提供者，
	// 以便代理使用所有者实际意图的凭据和模型运行。没有这个，
	// 没有自己提供者的查看者会落到系统共享提供者（通常是会用完的免费层密钥）
	// 或根本没有提供者 → 429 / "no provider configured"。
	// 顺序：查看者的已解析 cfg → 所有者的用户作用域（此块）→
	// 代理作用域 `agents.defaults` → 代理作用域提供者。代理作用域仍然获胜，
	// 匹配所有者自己的 loadUserSpace 路径使用的优先级。
	//
	// shareModelConfig（代理记录）控制此行为：默认为 true —
	// 聊天者开箱即用地继承所有者的密钥 + 模型选择，匹配"所有者已经为代理付费，
	// 他们是分享它的人"的心智模型。显式设置为 false 则退出，
	// 在这种情况下，所有者回退 + 代理作用域覆盖被完全跳过，
	// 聊天者只看到自己的用户作用域 + 系统。所有者自己的 loadUserSpace 路径
	//（sp.UserID == rec.UserID）不受影响，仍然获得完整的代理作用域覆盖。
	// 默认值在 setup.agentShareModelConfig 中 — 通过这里的同一个辅助函数读取
	//（内联以避免包循环）。
	//
	// 异常：当查看者（聊天者）*显式*设置了自己的用户作用域 agents.defaults.model 行时，
	// 该选择胜过所有者的用户作用域和代理作用域覆盖 —
	// "我的令牌，我的模型"。我们通过直接读取聊天者的原始行来检测这一点
	//（而不是合并的 cfg，它无法区分"显式用户作用域 = 系统默认"和"没有用户作用域行"）
	// 并在覆盖链之后重新固定该字段。目前只有 Model 被固定 —
	// MaxTokens / Temp / Thinking 等字段仍然通过所有者/代理覆盖，
	// 因为聊天者没有 UI 来为每个代理设置它们。
	chatterPin := readUserScopeAgentDefaults(ctx, st, sp.UserID)
	isForeign := rec.UserID != "" && rec.UserID != sp.UserID
	// 当键缺失时默认为 true — 与 setup.agentShareModelConfig 保持一致。
	shareCfg := true
	if v, ok := rec.Config["shareModelConfig"].(bool); ok {
		shareCfg = v
	}
	applyOwnerOverlays := !isForeign || shareCfg
	if isForeign && applyOwnerOverlays {
		if ownerCfg, err := assembleConfig(ctx, st, rec.UserID, ""); err == nil && ownerCfg != nil {
			ovr := ownerCfg.Agents.Defaults
			if ovr.Model != "" {
				rc.Model = ovr.Model
			}
			if ovr.MaxTokens > 0 {
				rc.MaxTokens = ovr.MaxTokens
			}
			if ovr.Temperature > 0 {
				rc.Temperature = ovr.Temperature
			}
			if ovr.MaxToolIterations > 0 {
				rc.MaxToolIterations = ovr.MaxToolIterations
			}
			if ovr.MaxParallelToolCalls > 0 {
				rc.MaxParallelToolCalls = ovr.MaxParallelToolCalls
			}
			if ovr.Thinking != "" {
				rc.Thinking = ovr.Thinking
			}
			if ovr.PolicyPreset != "" {
				rc.PolicyPreset = ovr.PolicyPreset
			}
		}
		// 仅拉取所有者的用户作用域提供者行（不是所有者的完整合并视图），
		// 这样我们就不会在查看者已合并的集合上重新应用系统行。
		// 所有者的用户作用域密钥然后位于查看者的用户作用域和下面的代理作用域覆盖之间 —
		// 与所有者自己的 UserSpace 会构建的优先级相同。
		if ownerProvs, err := scope.UserScopeProviders(ctx, st, rec.UserID); err == nil {
			for k, v := range ownerProvs {
				if rc.Providers == nil {
					rc.Providers = make(map[string]config.ProviderConfig)
				}
				rc.Providers[k] = v
			}
		}
	}
	if applyOwnerOverlays {
		if cfgRec, err := st.GetConfigByName(ctx, store.KindSetting, "", rc.ID, "agents.defaults"); err == nil && cfgRec != nil {
			var ovr config.AgentDefaults
			blob, _ := json.Marshal(cfgRec.Data)
			_ = json.Unmarshal(blob, &ovr)
			if ovr.Model != "" {
				rc.Model = ovr.Model
			}
			if ovr.MaxTokens > 0 {
				rc.MaxTokens = ovr.MaxTokens
			}
			if ovr.Temperature > 0 {
				rc.Temperature = ovr.Temperature
			}
			if ovr.MaxToolIterations > 0 {
				rc.MaxToolIterations = ovr.MaxToolIterations
			}
			if ovr.MaxParallelToolCalls > 0 {
				rc.MaxParallelToolCalls = ovr.MaxParallelToolCalls
			}
			if ovr.Thinking != "" {
				rc.Thinking = ovr.Thinking
			}
			if ovr.PolicyPreset != "" {
				rc.PolicyPreset = ovr.PolicyPreset
			}
			// 保持此覆盖与 loadUserSpace 中的所有者路径等效项对齐 —
			// 缺失的字段会静默破坏通过通道绑定延迟附加代理的聊天者的每个代理设置
			//（例如微信多气泡提示从不触发，因为 rc.SplitReplies 保持 nil；
			// 聊天机器人角色以代理提示模式渲染，因为 rc.PromptMode 保持 ""）。
			if ovr.PromptMode != "" {
				rc.PromptMode = ovr.PromptMode
			}
			if ovr.SplitReplies != nil {
				v := *ovr.SplitReplies
				rc.SplitReplies = &v
			}
			if ovr.AutoPersist != nil {
				v := *ovr.AutoPersist
				rc.AutoPersist = &v
			}
		}
	}
	if chatterPin.Model != "" {
		rc.Model = chatterPin.Model
	}
	// 叠加代理作用域提供者 — sp.Config.Providers 只携带系统+用户行
	//（loadUserSpace 中的 assembleConfig 使用 agentID="" 运行）。没有此叠加，
	// providerForAgent 看不到代理自己的凭据并回退到共享提供者，
	// 在错误的 base URL 上触发代理选择的模型 id。
	//
	// 与上面的 agents.defaults 覆盖相同的门：当聊天者使用所有者未选择共享的外部代理时，
	// 代理作用域提供者行保持对所有者私有。聊天者在他们自己的用户作用域提供者
	//（加上系统）能提供的任何东西上运行。
	if applyOwnerOverlays {
		if agentProvs, err := scope.AgentScopeProviders(ctx, st, rc.ID); err == nil {
			for k, v := range agentProvs {
				if rc.Providers == nil {
					rc.Providers = make(map[string]config.ProviderConfig)
				}
				rc.Providers[k] = v
			}
		}
	}
	rc.RefreshModelContextWindow()
	ensureAgentHome(rc)
	if ws != nil {
		if err := skills.HydrateSkillsDown(ctx, ws, rc.ID, filepath.Join(rc.Home, "skills")); err != nil {
			slog.Warn("skill hydrate failed", "agent", rc.ID, "error", err)
		}
	}
	// 构建一次性技能配置，将此代理自己的代理作用域技能环境（例如 image-tool 的 REPLICATE_API_TOKEN）
	// 注入到新代理将使用的 SkillsLoader 闭包中。
	//
	// 为什么我们不能直接修补 sp.Config：管理器的 globalSkillsCfg 在管理器构建时按值捕获，
	// 并在代理构建时由每个代理的 SkillsLoader 再次捕获，因此在事后修补 sp.Config 永远不会到达闭包。
	// AddAgentWithSkillsCfg 仅在此构建期间交换覆盖。
	//
	// 此修复的症状：代理所有者下的 web 聊天正常工作（所有者的用户空间 cfg 已经携带代理的技能环境），
	// 但在 apikey/app_user 下到达此处的 API 调用静默地落到技能提供的任何无密钥路径
	//（例如 image-tool → pollinations，或当编辑模式没有免费回退时的 "no provider configured"）。
	//
	// 作用域有意收紧：仅以 rc.ID 为键的代理作用域行。我们不拉取所有者的用户作用域全局技能环境 —
	// 那会将所有者的 API 密钥泄漏到另一个用户的会话中，用于他们可能甚至没有调用的技能。
	skillsCfg := sp.Config.Skills
	if cfgRec, err := st.GetConfigByName(ctx, store.KindSetting, "", rc.ID, "skills.entries"); err == nil && cfgRec != nil && len(cfgRec.Data) > 0 {
		blob, _ := json.Marshal(cfgRec.Data)
		var entries map[string]config.SkillEntryCfg
		if json.Unmarshal(blob, &entries) == nil && len(entries) > 0 {
			if skillsCfg.AgentEntries == nil {
				skillsCfg.AgentEntries = map[string]map[string]config.SkillEntryCfg{}
			} else {
				// 写时复制：不要改变 UserSpace.Config 其余部分仍然指向的共享映射。
				cp := make(map[string]map[string]config.SkillEntryCfg, len(skillsCfg.AgentEntries)+1)
				for k, v := range skillsCfg.AgentEntries {
					cp[k] = v
				}
				skillsCfg.AgentEntries = cp
			}
			skillsCfg.AgentEntries[rc.ID] = entries
		}
	}
	if err := sp.Agents.AddAgentWithSkillsCfg(rc, sp.Provider, mb, skillsCfg); err != nil {
		return fmt.Errorf("EnsureAgent: add agent: %w", err)
	}
	if sp.SandboxPool != nil {
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			ag.SetSandboxPool(sp.SandboxPool)
		}
	} else if rc.Workspace != "" {
		_ = os.MkdirAll(rc.Workspace, 0o755)
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			ag.ToolRegistry().SetSandboxRoot(rc.Workspace)
		}
	}
	// 将钩子插件挂接到新附加的代理上。镜像 loadUserSpace 对所有者代理所做的操作 —
	// 没有这个，钩子插件只会为代理的所有者触发，永远不会为通过外部附加
	//（通道绑定、公开链接、super_admin 浏览）到达代理的聊天者触发。
	if sp.PluginMgr != nil {
		if ag := sp.Agents.AgentByID(rc.ID); ag != nil {
			registerHookPluginsForAgent(ctx, sp.PluginMgr, st, ag)
		}
	}
	slog.Info("agent injected into foreign user space",
		"caller", sp.UserID, "agent", rc.ID, "owner", rec.UserID)
	return nil
}

// loadUserSpace 通过以下方式构建 UserSpace：
//  1. 快照系统配置（system_settings + 系统提供者/通道）
//  2. 在上面叠加用户自己的提供者 + 通道行
//  3. 从数据库列出用户的代理行
//  4. 构建拥有这些代理的 agent.Manager
//
// `systemSandboxPool` 是网关范围的池 — 由结果 UserSpace 借用，不拥有。
// 当系统作用域禁用沙箱时传递 nil；在这种情况下代理将以仅路径文件根运行。
func loadUserSpace(ctx context.Context, userID string, mb *bus.MessageBus, st store.Store, ws workspace.Store, meter usage.Meter, systemSandboxPool sandbox.ExecutorPool, pluginMgr *plugin.Manager) (*UserSpace, error) {
	if userID == "" {
		return nil, fmt.Errorf("loadUserSpace: userID required")
	}
	if st == nil {
		return nil, fmt.Errorf("loadUserSpace: store required")
	}
	cfg, err := assembleConfig(ctx, st, userID, "")
	if err != nil {
		return nil, fmt.Errorf("assemble config: %w", err)
	}
	config.LoadEnv().ApplyToConfig(cfg)
	config.ApplyDefaults(cfg)

	prov := newProviderFromConfig(cfg)

	// 从数据库拉取用户的代理。ResolveAgents 合并系统+用户默认值；
	// 每个代理覆盖来自 configs 表，通过创建/更新代理处理程序写入的代理作用域 `agents.defaults` 行。
	agentRecords, err := st.ListAgents(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list agents: %w", err)
	}

	// 其他用户拥有的公共代理不在此处急切加载 —
	// 它们在聊天者首次访问公共代理聊天 URL 时通过 UserSpace.EnsureAgent 延迟附加（见 resolveAgent）。
	// 会话/内存/agent_files 保持以聊天者的 user_id 为键，这样每个访问者获得私有历史记录，
	// 而代理身份（SOUL/IDENTITY/skills）从所有者的行共享。

	entries := make([]config.AgentEntry, 0, len(agentRecords))
	for _, ar := range agentRecords {
		entries = append(entries, config.AgentEntry{ID: ar.ID, UserID: ar.UserID, Name: ar.Name})
	}

	// Bindings 过去存在于自己的 kind=setting/name=bindings 行中。
	// 在 configs 模式重构后，通道行直接携带 agent_id，
	// 因此我们从通道表本身合成 Bindings — 每个 agent_id 等于此用户拥有的代理之一的行
	// 为其数据中的每个 Account 贡献一个 Binding。
	cfg.Bindings = append(cfg.Bindings, bindingsFromChannelRows(ctx, st, userID, agentRecords)...)
	resolved := config.ResolveAgents(cfg, entries)
	for i := range resolved {
		// 在 ResolveAgents 已应用的系统→用户合并之上叠加代理作用域 agents.defaults。
		// 我们直接读取代理作用域行（不通过 SettingInto 系统+用户，
		// 那样会重新合并这些层并破坏已在 cfg.Agents.Defaults 中的用户作用域 Model）。
		//
		// 索引到 resolved（不是按值范围），以便写入落到管理器稍后读取的切片元素上 —
		// 否则代理作用域 Model 永远不会到达 NewManager，聊天静默使用系统/用户默认值。
		rc := &resolved[i]
		var agentOverride config.AgentDefaults
		if rec, err := st.GetConfigByName(ctx, store.KindSetting, "", rc.ID, "agents.defaults"); err == nil && rec != nil {
			blob, _ := json.Marshal(rec.Data)
			_ = json.Unmarshal(blob, &agentOverride)
			if agentOverride.Model != "" {
				rc.Model = agentOverride.Model
			}
			if agentOverride.MaxTokens > 0 {
				rc.MaxTokens = agentOverride.MaxTokens
			}
			if agentOverride.Temperature > 0 {
				rc.Temperature = agentOverride.Temperature
			}
			if agentOverride.MaxToolIterations > 0 {
				rc.MaxToolIterations = agentOverride.MaxToolIterations
			}
			if agentOverride.MaxParallelToolCalls > 0 {
				rc.MaxParallelToolCalls = agentOverride.MaxParallelToolCalls
			}
			if agentOverride.Thinking != "" {
				rc.Thinking = agentOverride.Thinking
			}
			if agentOverride.PolicyPreset != "" {
				rc.PolicyPreset = agentOverride.PolicyPreset
			}
			if agentOverride.PromptMode != "" {
				rc.PromptMode = agentOverride.PromptMode
			}
			// 每个代理微信分割回复 — 指针语义，以便"缺失"（无行或行没有键）与"显式 false"不同。
			// 行中的非 nil 意味着操作员做出了深思熟虑的选择；nil 稍后在 NewAgentWithFullCfg 中落到系统 WeChatCfg.SplitReplies。
			if agentOverride.SplitReplies != nil {
				v := *agentOverride.SplitReplies
				rc.SplitReplies = &v
			}
			// 每个代理的 autoPersist — 相同的指针语义。此处的非 nil 会专门为此代理覆盖系统/用户 memory.autoPersist.enabled。
			// 最常用于聊天机器人模式的角色，其中 LLM 不能直接 write_file，因此后台蒸馏传递是唯一的持久化路径。
			if agentOverride.AutoPersist != nil {
				v := *agentOverride.AutoPersist
				rc.AutoPersist = &v
			}
		}
		// 提供者同理：assembleConfig 使用 agentID="" 调用，因此 cfg.Providers（现在在 rc.Providers 中）
		// 只携带系统+用户行。没有这个，每个代理的提供者密钥（例如代理作用域的 OpenRouter 凭证）
		// 对 providerForAgent 不可见，它回退到共享提供者 —
		// 聊天在错误的 base URL 上触发代理选择的模型 id，并从错误的供应商得到 400。
		if agentProvs, err := scope.AgentScopeProviders(ctx, st, rc.ID); err == nil {
			for k, v := range agentProvs {
				if rc.Providers == nil {
					rc.Providers = make(map[string]config.ProviderConfig)
				}
				rc.Providers[k] = v
			}
		}
		rc.RefreshModelContextWindow()
		ensureAgentHome(*rc)
		if ws != nil {
			if err := skills.HydrateSkillsDown(
				ctx, ws, rc.ID, filepath.Join(rc.Home, "skills"),
			); err != nil {
				slog.Warn("skill hydrate failed", "agent", rc.ID, "error", err)
			}
		}
	}
	if ws != nil {
		if dir, gerr := globalSkillsDirPath(); gerr == nil {
			if err := skills.HydrateSkillsDown(
				ctx, ws, skills.GlobalSkillOwner, dir,
				agent.BundledSkillNames()...,
			); err != nil {
				slog.Warn("global skill hydrate failed", "error", err)
			}
		}
	}

	managerOpts := []agent.ManagerOption{
		agent.WithUserID(userID),
		agent.WithGlobalSkillsCfg(cfg.Skills),
		agent.WithSkillsLearner(cfg.SkillsLearner),
		agent.WithSessionStore(session.NewStoreAdapter(st, userID)),
		agent.WithMemoryStore(agent.NewMemoryStoreAdapter(st)),
		agent.WithDataStore(st),
	}
	if ws != nil {
		managerOpts = append(managerOpts, agent.WithWorkspaceStore(ws))
	}
	if meter != nil {
		managerOpts = append(managerOpts, agent.WithMeter(meter))
	}
	agentMgr, err := agent.NewManager(resolved, prov, mb, managerOpts...)
	if err != nil {
		return nil, fmt.Errorf("create agent manager for user %q: %w", userID, err)
	}

	registerAgentToolChains(cfg, agentMgr.All())

	pool := attachSandboxToAgents(systemSandboxPool, userID, resolved, agentMgr)

	// 将钩子插件挂接到每个代理的 HookRegistry。每个代理的启用来自 configs 行
	// (scope=agent, agent_id=X, name=plugins.enabled) —
	// 当没有每个代理的覆盖时，回退到插件清单的启动时启用状态。
	if pluginMgr != nil {
		for _, ag := range agentMgr.All() {
			registerHookPluginsForAgent(ctx, pluginMgr, st, ag)
		}
	}

	slog.Info("loaded user space", "user", userID, "agents", agentMgr.Names())

	return &UserSpace{
		UserID:      userID,
		Config:      cfg,
		Provider:    prov,
		Agents:      agentMgr,
		SandboxPool: pool,
		PluginMgr:   pluginMgr,
	}, nil
}

// registerHookPluginsForAgent 遍历每个正在运行的钩子类型插件，
// 并仅在此代理通过每个代理的 plugins.enabled 行显式选择加入时将其附加到 ag.HookRegistry。
//
// 默认是选择加入：插件在系统范围内启用仅意味着其进程运行并可附加。
// 每个代理必须单独设置 `plugins.enabled[<id>] = true`（通过仪表盘插件卡片或直接在 configs 表中）
// 才能使插件的钩子在其轮次上触发。系统范围启用而没有每个代理选择加入 = 该代理的插件空闲。
//
// 理由：钩子插件可能以令人惊讶的方式改变代理行为（额外消息、修改提示、记录的对话数据）。
// 默认拒绝避免意外影响操作员未打算影响的代理。
//
// 在管理器级别是幂等的（进程已经在运行），但 HookRegistry 侧会累积 —
// 调用站点不能为同一代理双重注册。目前唯一的调用站点是 loadUserSpace（每个 UserSpace 启动一次）
// 和 EnsureAgent（每个外部附加一次），两者都不会为同一代理触发两次。
func registerHookPluginsForAgent(ctx context.Context, pluginMgr *plugin.Manager, st store.Store, ag *agent.Agent) {
	overrides := readAgentScopePluginsEnabled(ctx, st, ag.Name())
	if len(overrides) == 0 {
		return // 快速路径：此代理没有选择加入
	}
	for _, inst := range pluginMgr.HookPlugins() {
		id := inst.Manifest.ID
		// 选择加入：仅当此代理显式设置为 true 时才附加。缺少键或显式 false → 跳过。
		if !overrides[id] {
			continue
		}
		if inst.Process == nil || !inst.Process.IsRunning() {
			slog.Warn("plugin: agent opted in but plugin not running",
				"plugin", id, "agent", ag.Name())
			continue
		}
		if err := plugin.RegisterPluginHooks(ctx, pluginMgr, id, ag.HookRegistry(), ag.Name()); err != nil {
			slog.Warn("plugin: hook register failed",
				"plugin", id, "agent", ag.Name(), "error", err)
		}
	}
}

// readAgentScopePluginsEnabled 从 configs 表读取每个代理的插件启用覆盖：
// scope=agent, name=plugins.enabled, data = {"<pluginID>": true|false, ...}。
// 缺少行/缺少键意味着"无覆盖；使用系统默认"。查找错误时返回 nil（调用者将 nil 视为"无覆盖"）。
func readAgentScopePluginsEnabled(ctx context.Context, st store.Store, agentID string) map[string]bool {
	if st == nil || agentID == "" {
		return nil
	}
	rec, err := st.GetConfigByName(ctx, store.KindSetting, "", agentID, "plugins.enabled")
	if err != nil || rec == nil {
		return nil
	}
	out := make(map[string]bool, len(rec.Data))
	for k, v := range rec.Data {
		if b, ok := v.(bool); ok {
			out[k] = b
		}
	}
	return out
}

// newProviderFromConfig 为已解析的默认模型选择一个 LLM 提供者。当没有匹配时返回 nil（带清晰的日志行）；
// 代理循环在第一个轮次将缺失提供者状态作为错误暴露，而不是静默地发出虚假调用。
func newProviderFromConfig(cfg *config.Config) provider.Provider {
	defaultModel := cfg.Agents.Defaults.Model
	parts := strings.SplitN(defaultModel, "/", 2)
	if len(parts) != 2 {
		slog.Warn("no provider configured: default model is missing the '<provider>/<model>' prefix",
			"defaultModel", defaultModel, "providerCount", len(cfg.Providers))
		return nil
	}
	key := parts[0]
	p, ok := cfg.Providers[key]
	if !ok {
		slog.Warn("no provider configured: default model references a provider key that isn't in cfg.Providers",
			"key", key, "defaultModel", defaultModel,
			"availableKeys", providerKeyList(cfg.Providers))
		return nil
	}
	if p.APIKey == "" {
		slog.Warn("provider matched but its APIKey is empty",
			"key", key, "apiBase", p.APIBase)
		return nil
	}
	slog.Info("provider selected",
		"key", key, "apiBase", p.APIBase, "apiType", p.APIType,
		"defaultModel", defaultModel,
	)
	return provider.NewProvider(p.APIKey, p.APIBase, p.APIType)
}

func providerKeyList(m map[string]config.ProviderConfig) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// userSpaceRegistry 是用户空间的线程安全延迟加载映射。没有预加载/固定的空间；
// 每个用户在首次认证时加载，并在 idleTTL 不活动后驱逐。
//
// `systemSandboxPool` 作为借用的引用持有，并在加载时交给每个 UserSpace。网关拥有其生命周期。
type userSpaceRegistry struct {
	mu                sync.RWMutex
	spaces            map[string]*userSpaceEntry
	bus               *bus.MessageBus
	store             store.Store
	workspace         workspace.Store
	meter             usage.Meter
	systemSandboxPool sandbox.ExecutorPool
	// pluginMgr 是共享的（进程范围）插件管理器。当 systemPlugins 禁用时为 nil。
	// 由 loadUserSpace 和 EnsureAgent 使用，用于将钩子类型插件注册到每个代理的 HookRegistry，
	// 由每个代理的 plugins.enabled 配置控制。
	pluginMgr *plugin.Manager
	idleTTL   time.Duration
}

type userSpaceEntry struct {
	space    *UserSpace
	lastUsed time.Time
}

func newUserSpaceRegistry(mb *bus.MessageBus, st store.Store, ws workspace.Store, meter usage.Meter, systemSandboxPool sandbox.ExecutorPool, pluginMgr *plugin.Manager) *userSpaceRegistry {
	return &userSpaceRegistry{
		spaces:            make(map[string]*userSpaceEntry),
		bus:               mb,
		store:             st,
		workspace:         ws,
		meter:             meter,
		systemSandboxPool: systemSandboxPool,
		pluginMgr:         pluginMgr,
		idleTTL:           30 * time.Minute,
	}
}

func (r *userSpaceRegistry) get(userID string) (*UserSpace, bool) {
	r.mu.RLock()
	e, ok := r.spaces[userID]
	r.mu.RUnlock()
	if !ok {
		return nil, false
	}
	r.mu.Lock()
	e.lastUsed = time.Now()
	r.mu.Unlock()
	return e.space, true
}

func (r *userSpaceRegistry) getOrLoad(ctx context.Context, userID string) (*UserSpace, error) {
	if sp, ok := r.get(userID); ok {
		return sp, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.spaces[userID]; ok {
		e.lastUsed = time.Now()
		return e.space, nil
	}
	sp, err := loadUserSpace(ctx, userID, r.bus, r.store, r.workspace, r.meter, r.systemSandboxPool, r.pluginMgr)
	if err != nil {
		return nil, err
	}
	r.spaces[userID] = &userSpaceEntry{space: sp, lastUsed: time.Now()}
	return sp, nil
}

// invalidate 丢弃用户的空间，以便下次访问重新加载它。在管理员变更
// （创建代理、轮换提供者等）后使用，以便内存中的副本不落后于数据库。
func (r *userSpaceRegistry) invalidate(userID string) {
	r.mu.Lock()
	delete(r.spaces, userID)
	r.mu.Unlock()
}

func (r *userSpaceRegistry) all() []*UserSpace {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*UserSpace, 0, len(r.spaces))
	for _, e := range r.spaces {
		out = append(out, e.space)
	}
	return out
}

func (r *userSpaceRegistry) evictIdle() int {
	if r.idleTTL <= 0 {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.idleTTL)
	evicted := 0
	for uid, e := range r.spaces {
		if e.lastUsed.Before(cutoff) {
			delete(r.spaces, uid)
			evicted++
			slog.Info("evicted idle user space", "user", uid,
				"idle", time.Since(e.lastUsed).Round(time.Second))
		}
	}
	return evicted
}

func (r *userSpaceRegistry) startEvictor(ctx context.Context) {
	if r.idleTTL <= 0 {
		return
	}
	interval := r.idleTTL / 3
	if interval < time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := r.evictIdle(); n > 0 {
				slog.Info("user space eviction sweep", "evicted", n, "remaining", len(r.spaces))
			}
		}
	}
}

// bindingsFromChannelRows 从通道行本身合成 (channel, accountID) → agentID 路由表。
// 它替代了旧的 kind=setting/name=bindings 间接引用：每个 agent_id 指向此用户可以路由的代理的通道行，
// 为其每个 Account 贡献一个 Binding。
//
// 从此用户可以路由的三个所有权角落拉取行：
//   - (user_id="", agent_id=Y)：用户拥有的任何代理 Y 的"官方"行（遗留/重构前数据）
//   - (user_id=userID, agent_id=Y) 其中用户拥有 Y：此用户在其自己代理上的绑定（正常重构后模式）
//   - (user_id=userID, agent_id=Z) 其中用户不拥有 Z：此用户创作的外部代理上的通道覆盖
//     （例如聊天者将其微信 bot 绑定到公共代理）。没有这个反向查找，
//     resolveChannelOwner 正确地将入站 DM 路由到绑定者的 UserSpace，
//     但 matchAgent 发现空的 Bindings 列表，因为代理不在 ListAgents(userID) 中。
//     然后 matchAgent 路径在首次匹配时通过 ensureForeignAgent 延迟附加外部代理。
//
// 没有显式通道行覆盖的已授权代理绑定保持在外部 — 它们存在于代理所有者的空间中，而不是每个受让人的空间。
func bindingsFromChannelRows(ctx context.Context, st store.Store, userID string, agents []store.AgentRecord) []config.Binding {
	if st == nil {
		return nil
	}
	var out []config.Binding
	covered := make(map[string]bool, len(agents))
	for _, ar := range agents {
		covered[ar.ID] = true
		rows, err := st.ListConfigs(ctx, store.KindChannel, "", ar.ID)
		if err == nil {
			out = append(out, expandChannelBindings(rows, ar.ID)...)
		}
		if userID != "" {
			rows, err := st.ListConfigs(ctx, store.KindChannel, userID, ar.ID)
			if err == nil {
				out = append(out, expandChannelBindings(rows, ar.ID)...)
			}
		}
	}
	// 反向查找：此用户针对其不拥有的代理编写的任何通道行。matchAgent 将在首次命中时延迟附加代理。
	if userID != "" {
		foreignRows, err := st.ListConfigsByUser(ctx, store.KindChannel, userID)
		if err == nil {
			for i := range foreignRows {
				rec := foreignRows[i]
				if rec.AgentID == "" || covered[rec.AgentID] {
					continue
				}
				out = append(out, expandChannelBindings([]store.ConfigRecord{rec}, rec.AgentID)...)
			}
		}
	}
	return out
}

func expandChannelBindings(rows []store.ConfigRecord, agentID string) []config.Binding {
	var out []config.Binding
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		cc := config.ChannelConfig{}
		if blob, err := json.Marshal(r.Data); err == nil {
			_ = json.Unmarshal(blob, &cc)
		}
		// 行上的每个账号一个 Binding；空的 Accounts 映射意味着单个 bot，
		// 其 accountID 是隐式的（尚未按用户名索引的旧适配器）。
		if len(cc.Accounts) == 0 {
			out = append(out, config.Binding{
				AgentID: agentID,
				Match:   config.Match{Channel: r.Name},
			})
			continue
		}
		for accountID := range cc.Accounts {
			out = append(out, config.Binding{
				AgentID: agentID,
				Match:   config.Match{Channel: r.Name, AccountID: accountID},
			})
		}
	}
	return out
}
