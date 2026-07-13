package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/session"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/usage"
	"github.com/qs3c/bkcrab/internal/workspace"
)

// providerForAgent 为单个代理选择一个 LLM 提供商。解析：
//
//  1. 将 `rc.Model` 解析为 "<providerKey>/<modelId>"。
//  2. 查找 `rc.Providers[providerKey]`。`Providers` 是合并后的视图
//     （全局 ← agent.json），所以代理独占的提供商会遮蔽具有相同键的
//     全局提供商。
//  3. 回退到共享提供商（Manager/UserSpace 从全局默认值中选择的），
//     使没有每个代理提供商的旧部署保持工作。
//
// 这就是使每个代理的凭证在运行时变真实的方式——每个代理从其自己的
// API 密钥+基础 URL 构建自己的 provider.Provider，而不是使用用户空间
// 范围的提供商。
func providerForAgent(rc config.ResolvedAgent, shared provider.Provider) provider.Provider {
	parts := strings.SplitN(rc.Model, "/", 2)
	if len(parts) == 2 {
		if pc, ok := rc.Providers[parts[0]]; ok && pc.APIKey != "" {
			return provider.NewProvider(pc.APIKey, pc.APIBase, pc.APIType)
		}
	}
	return shared
}

// ManagerOption 配置可选的 Manager 行为。
type ManagerOption func(*managerOpts)

type managerOpts struct {
	sessionStore     session.SessionStore
	memoryStore      MemoryStore
	workspaceStore   workspace.Store
	dataStore        store.Store
	meter            usage.Meter
	userID           string
	globalSkillsCfg  config.SkillsCfg
	skillsLearnerCfg config.SkillsLearnerCfg
}

func WithSessionStore(st session.SessionStore) ManagerOption {
	return func(o *managerOpts) { o.sessionStore = st }
}

func WithMemoryStore(st MemoryStore) ManagerOption {
	return func(o *managerOpts) { o.memoryStore = st }
}

// WithUserID 用拥有者用户标记 Manager 加载的每个代理，使基于存储的
// Memory + Session 调用按 user_id 作用域行。UserSpace 传递已解析的用户；
// 本地模式网关使用 config.DefaultUserID。
func WithUserID(userID string) ManagerOption {
	return func(o *managerOpts) { o.userID = userID }
}

// WithWorkspaceStore 在每个代理的工具注册表上安装一个持久的 blob 存储，
// 使文件操作（write_file / read_file / list_dir）落在共享存储中，而
// 不是 Pod 本地文件系统。
func WithWorkspaceStore(ws workspace.Store) ManagerOption {
	return func(o *managerOpts) { o.workspaceStore = ws }
}

// WithDataStore 将平台的关系型存储暴露给代理。cron 工具需要它来持久化
// cron.Scheduler 稍后拾取的定时任务；没有它，create_cron_job 会从代理的
// 工具列表中省略，有时限的请求会回退到 HEARTBEAT.md 中的自然语言提醒
// （这些提醒只有懒散的 30 分钟审查，对于短时效提醒来说是错误的）。
func WithDataStore(st store.Store) ManagerOption {
	return func(o *managerOpts) { o.dataStore = st }
}

// WithMeter 在每个代理上安装管理员级别的令牌计量器，使每个
// provider.Chat / ChatStream 调用记录到 token_usage_daily。省略以禁用
// 计量（测试、单用户开发运行）。
func WithMeter(m usage.Meter) ManagerOption {
	return func(o *managerOpts) { o.meter = m }
}

// WithGlobalSkillsCfg 将 cfg.Skills（持有每个技能或每个（代理，技能）
// 的 apiKey/env 的 entries + agentEntries）传播到管理器构建的代理中。
// 没有这个，buildAgent → NewAgent 会传递零值 SkillsCfg，并且
// SkillsLoader.SkillEnvVars 会看到空条目——每个技能都会在没有配置的
// FAL_KEY / REPLICATE_API_TOKEN 的情况下运行，无论数据库中保存了什么。
func WithGlobalSkillsCfg(cfg config.SkillsCfg) ManagerOption {
	return func(o *managerOpts) { o.globalSkillsCfg = cfg }
}

// WithSkillsLearner 把系统级 SkillsLearner 配置(启用/门槛/模型/生命周期)
// 线程进 Manager 的生产构造路径。没有它,buildAgent 走 NewAgentWithSkillsCfg
// 从不构造 learner——技能自动提炼与生命周期在生产中静默不启用(历史缺口:
// 该配置此前只在无人调用的 NewAgentWithFullCfg 里接线)。
func WithSkillsLearner(cfg config.SkillsLearnerCfg) ManagerOption {
	return func(o *managerOpts) { o.skillsLearnerCfg = cfg }
}

// Manager 加载并管理所有代理实例。
type Manager struct {
	agents       map[string]*Agent
	defaultAgent *Agent
	// opts 被保留，以便 AddAgent（入职/代理创建后的热重载）可以应用
	// 构造函数所做的相同存储连接。没有这个，新添加的代理的工具注册表
	// 永远不会得到 SetSystemFileStore，所以 read_file 会落到主机 FS 并
	// 对仅存在于数据库中的身份文件（SOUL/IDENTITY/...）返回 404。
	opts managerOpts
	uid  string
}

// NewManager 从解析后的配置创建代理。
func NewManager(resolved []config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, opts ...ManagerOption) (*Manager, error) {
	m := &Manager{
		agents: make(map[string]*Agent),
	}
	for _, o := range opts {
		o(&m.opts)
	}

	if _, err := config.HomeDir(); err != nil {
		return nil, err
	}

	m.uid = m.opts.userID
	if m.uid == "" {
		return nil, fmt.Errorf("agent.NewManager: WithUserID is required")
	}
	for _, rc := range resolved {
		deleting, err := m.agentDeletionState(rc.ID)
		if err != nil {
			return nil, fmt.Errorf("verify deletion state for agent %q: %w", rc.ID, err)
		}
		if deleting {
			slog.Warn("skip tombstoned agent during manager build", "agent", rc.ID)
			continue
		}
		ag := m.buildAgent(rc, prov, mb)
		m.agents[rc.ID] = ag

		slog.Info("loaded agent",
			"id", rc.ID,
			"model", rc.Model,
			"home", rc.Home,
			"workspace", rc.Workspace,
		)
	}

	// 如果只有一个代理，将其设为默认
	if len(m.agents) == 1 {
		for _, ag := range m.agents {
			m.defaultAgent = ag
		}
	}

	return m, nil
}

func (m *Manager) agentDeletionState(agentID string) (bool, error) {
	guard, ok := m.opts.dataStore.(learnerAgentDeletionReader)
	if !ok || agentID == "" {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return guard.IsAgentDeleting(ctx, agentID)
}

// buildAgent 构造一个 Agent 并连接 Manager 配置的每个存储。
// 在 NewManager 的引导循环和 AddAgent 的热重载路径之间共享，
// 使新入职的代理获得相同的基于数据库的身份/内存/工作空间管道。
func (m *Manager) buildAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus) *Agent {
	homeDir, _ := config.HomeDir()
	// 传递全局 SkillsCfg，使 SkillsLoader 看到管理 UI 配置的每个技能的
	// apiKey + env（以及每个代理的覆盖映射）。普通的 NewAgent 使用零值
	// SkillsCfg 构造加载器，这就是 FAL_KEY / REPLICATE_API_TOKEN 从未
	// 到达沙箱的原因。
	agProv := providerForAgent(rc, prov)
	ag := NewAgentWithSkillsCfg(rc, agProv, mb, homeDir, m.opts.globalSkillsCfg)
	// 技能学习者 + 生命周期:生产路径此前从不装配(learner 配置历史上只接在
	// 无人调用的 NewAgentWithFullCfg 上)。在此构造,使下方 dataStore 块能接上
	// ledger、runPostTurn 的 cadence 提取与生命周期过滤/清理在生产真正生效。
	ag.lifecycleCfg = m.opts.skillsLearnerCfg.Lifecycle
	configureSkillsLearner(ag, rc, agProv, homeDir, m.opts.globalSkillsCfg, m.opts.skillsLearnerCfg)
	// Asset isolation is always-on even when automatic extraction is disabled.
	// Disabling the learner turns off create/update cadence and lifecycle jobs;
	// it must not strand verified pre-isolation assets under <agent>/skills.
	learnerAssetManager := skills.NewManager(skills.LearnerSkillsDir(rc.Home), skills.DefaultManagerConfig())
	if ag.skillsLearner != nil {
		learnerAssetManager = ag.skillsLearner.Manager()
	}
	ag.SetOwnerUserID(m.uid)
	ag.agentID = rc.ID
	// m.uid is the runtime UserSpace owner and may be a public-agent visitor.
	// rc.UserID is the authoritative agents.user_id and must be wired
	// independently for owner-only learner operations.
	ag.registry.SetOwnerUserID(m.uid)
	ag.registry.SetAgentOwnerUserID(rc.UserID)
	learnerStorageReady := true
	// 每个用户的技能桶：聊天时“技能/...”将路由写入
	// ~/.bkcrab/users/<uid>/，其中 SkillsLoader 的“个人”层
	// 还进行扫描（请参阅 SkillsLoader.WithUserID）。设置用户ID
	// 预先注册（下面的 systemFileStore 分支也设置
	// 它，但仅当内存存储已连接时 - 没有这个葫芦
	// 非云安装会将镜像技能存储在agentID下
	// 而不是每个用户的所有者密钥，拆分相同的技能
	// 两个商店命名空间之间的内容）。跳过旧版/
	// 单用户安装，其中 m.uid 为空 — file.go 会回退
	// 到 systemRoot（代理主页），以便现有的技能包仍然有效。
	if m.uid != "" {
		if base := userSkillsRootDir(m.uid); base != "" {
			ag.registry.SetUserSkillsRoot(base)
		}
	}
	if m.opts.sessionStore != nil {
		ag.sessions = session.NewManagerWithStoreForUser(rc.Home+"/sessions", m.opts.sessionStore, m.uid, rc.ID)
	}
	if m.opts.memoryStore != nil {
		ag.memory = NewMemoryWithStoreForUser(rc.Home, m.opts.memoryStore, m.uid, rc.ID)
		ag.ctxBuilder.store = m.opts.memoryStore
		ag.ctxBuilder.agentID = rc.ID
		ag.ctxBuilder.userID = m.uid
		ag.memoryStore = m.opts.memoryStore
		// 身份文件（SOUL/IDENTITY/USER/...）共享相同的数据库
		// 存储为内存，因此来自代理的 write_file 最终位于
		// 管理 UI 的“自定义”页面读取的行相同。
		ag.registry.SetSystemFileStore(m.opts.memoryStore, rc.ID)
		// 为每个用户文件（USER.md /
		// MEMORY.md) 和代理所有者 (rc.UserID) 的身份
		// 文件（SOUL.md / IDENTITY.md / BOOTSTRAP.md / ...）。没有
		// 第二次调用时，代理的 BOOTSTRAP 流程将写入
		// SOUL/IDENTITY/BOOTSTRAP下的喋喋不休和Customize
		// 页面（以代理所有者为关键字）永远不会看到它们。
	}
	if m.opts.workspaceStore != nil {
		ag.registry.SetWorkspaceStore(m.opts.workspaceStore, rc.ID)
		// 还使该存储可供 SkillsLoader 使用，以便对象存储
		// 技能（全局+每个代理）在每个回合都会得到补充。没有
		// 这个，没有处理原始上传的 Pod 永远不会
		// 看到一个新技能。
		ag.workspaceStore = m.opts.workspaceStore
		if ag.skillsLearner != nil {
			ag.skillsLearner.workspaceStore = m.opts.workspaceStore
		}
		// Reconcile only the already-isolated learner namespace before local
		// migration. This remains necessary when extraction is disabled because
		// existing owner assets are still shared with users of the agent.
		hydrateCtx, cancelHydrate := context.WithTimeout(context.Background(), 30*time.Second)
		if err := skills.HydrateLearnerSkillsDown(hydrateCtx, m.opts.workspaceStore, rc.ID, learnerAssetManager.RootDir()); err != nil {
			slog.Warn("initial learner skill hydrate failed", "agent", rc.ID, "error", err)
			learnerStorageReady = false
			if ag.skillsLearner != nil {
				ag.skillsLearner.createDisabledReason = "initial learner object-store hydration failed"
			}
		}
		cancelHydrate()
		// Defer the first hydrate until after the dataStore block below has had a
		// chance to migrate verified legacy learner directories. Hydrating the
		// ordinary agent/skills namespace first could prune an old local learner
		// whenever that namespace already contains any authoritative remote skill.
	}
	if m.opts.dataStore != nil {
		ag.registry.SetContextArchiveStore(m.opts.dataStore, rc.ID)
		// Cron 工具需要关系存储来持久保存计划
		// 工作；关闭还会从注册表中读取频道/聊天ID
		// 在执行时（bindSession 每回合都会标记它们），因此
		// 触发的消息路由回原始聊天。
		tools.RegisterCronTools(ag.registry, m.opts.dataStore, m.uid, rc.ID)
		// set_timezone 将聊天者的 IANA 时区保留到范围内
		// prefs — 系统提示日期行和 cron 相同的行
		// 调度解决通过。需要关系存储，所以它
		// 与 cron 拥有相同的守卫。
		tools.RegisterTimezoneTool(ag.registry, m.opts.dataStore)
		// /goal 功能：代币记账钩子 + update_goal 工具，全部
		// 指定代理的所有者（上面通过 SetOwnerUserID 设置）。
		// 与 cron 相同的数据存储保护，因为这两个功能都需要
		// 关系商店；没有一个代理会悄悄降级。
		ag.WireGoals(m.opts.dataStore)
		// 也在代理上标记，以便运行时检查（例如 autoPersist
		// 节奏门计算 session_messages 而不是依赖
		// 在重新启动清除的内存计数器上）可以命中
		// 直接存储，无需通过 Manager 重新管道。
		ag.dataStore = m.opts.dataStore
		if ag.skillsLearner != nil {
			ag.skillsLearner.agentID = rc.ID
			ag.skillsLearner.ledger = m.opts.dataStore
			// skill_manage 与 learner 共用管理器与账本:主对话工具的
			// create/update/delete 与提取路径同一份生命周期记账。
			ag.registry.SetSkillManage(ag.skillsLearner.Manager(), m.opts.dataStore)
		}
		migrationStore := m.opts.workspaceStore
		if !learnerStorageReady {
			// Still move a hash-verified legacy source out of the ordinary
			// skills directory before full hydration, but do not write to a
			// remote store whose current state we could not read.
			migrationStore = nil
		}
		migrationCtx, cancelMigration := context.WithTimeout(context.Background(), 30*time.Second)
		migrateLegacyLearnerSkills(migrationCtx, m.opts.dataStore, migrationStore, rc.ID, rc.Home, learnerAssetManager)
		cancelMigration()
		// 聊天者所在时区的日期线 - 需要 dataStore
		// 范围首选项查找，因此在此处连接并由
		// 每次 ctxBuilder 重建后重新加载工作空间文件。
		ag.ctxBuilder.SetTimezoneResolver(ag.chatterLocation)
	}
	// One initial refresh after every store dependency is wired: migration has
	// completed, lifecycle filtering can see its ledger, and object-store-only
	// skills become visible without a second redundant hydrate.
	if m.opts.workspaceStore != nil || m.opts.dataStore != nil {
		ag.ReloadWorkspaceFiles()
	}
	// Only the authoritative owner UserSpace may resume learner creation. A
	// public-agent visitor still receives the owner's learner assets through the
	// loader, but must never run or recover extraction jobs.
	if ag.skillsLearner != nil && ag.dataStore != nil && rc.UserID != "" && m.uid == rc.UserID {
		ag.startSkillExtractionRecovery()
	}
	if m.opts.meter != nil {
		ag.SetMeter(m.opts.meter)
	}
	return ag
}

// AddAgent 动态创建并注册一个新代理（用于热重载）。
func (m *Manager) AddAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus) error {
	if _, exists := m.agents[rc.ID]; exists {
		return fmt.Errorf("agent %q already exists", rc.ID)
	}
	if deleting, err := m.agentDeletionState(rc.ID); err != nil {
		return fmt.Errorf("verify deletion state for agent %q: %w", rc.ID, err)
	} else if deleting {
		return fmt.Errorf("agent %q is permanently deleted and cannot be loaded", rc.ID)
	}
	m.agents[rc.ID] = m.buildAgent(rc, prov, mb)
	slog.Info("agent added dynamically", "id", rc.ID, "model", rc.Model)
	return nil
}

// AddAgentWithSkillsCfg 是 AddAgent + 一次性技能 cfg 覆盖
// 仅在此版本中替换 m.opts.globalSkillsCfg。 EnsureAgent（其中
// 将外部代理注入到不同用户的 UserSpace 中）使用此功能
// 新代理中包含的 SkillsLoader 闭包会获取代理的
// 自己的代理范围技能环境（例如 image-tool 的 REPLICATE_API_TOKEN） -
// 呼叫者的 UserSpace cfg 不包含它，因为代理不属于其所有
// 由来电者。
//
// 覆盖是本地的：m.opts.globalSkillsCfg 之前已恢复
// 返回，以便同一管理器上的下一个 AddAgent 返回到
// 调用者自己的cfg。没有额外的锁——调用者（UserSpace.
// EnsureAgent) 已通过 sp.mu 序列化。
func (m *Manager) AddAgentWithSkillsCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, cfg config.SkillsCfg) error {
	return m.AddAgentWithOwnerPolicies(rc, prov, mb, cfg, m.opts.skillsLearnerCfg)
}

// AddAgentWithOwnerPolicies injects a foreign/public agent using the agent
// owner's authoritative shared-asset policy. A visitor's skills learner
// settings must not control lifecycle ranking or usage weights for assets that
// belong to someone else's agent.
func (m *Manager) AddAgentWithOwnerPolicies(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, cfg config.SkillsCfg, learnerCfg config.SkillsLearnerCfg) error {
	if _, exists := m.agents[rc.ID]; exists {
		return fmt.Errorf("agent %q already exists", rc.ID)
	}
	if deleting, err := m.agentDeletionState(rc.ID); err != nil {
		return fmt.Errorf("verify deletion state for agent %q: %w", rc.ID, err)
	} else if deleting {
		return fmt.Errorf("agent %q is permanently deleted and cannot be loaded", rc.ID)
	}
	prev := m.opts.globalSkillsCfg
	prevLearner := m.opts.skillsLearnerCfg
	m.opts.globalSkillsCfg = cfg
	m.opts.skillsLearnerCfg = learnerCfg
	m.agents[rc.ID] = m.buildAgent(rc, prov, mb)
	m.opts.globalSkillsCfg = prev
	m.opts.skillsLearnerCfg = prevLearner
	slog.Info("agent added dynamically with override skills cfg", "id", rc.ID, "model", rc.Model)
	return nil
}

// RemoveAgent 通过 ID 取消注册代理。如果未加载代理，则无操作。
func (m *Manager) RemoveAgent(id string) {
	ag, ok := m.agents[id]
	if !ok {
		return
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := ag.stopSkillJobs(closeCtx); err != nil {
		slog.Warn("stop learner jobs while removing agent failed", "agent", id, "error", err)
	}
	cancel()
	delete(m.agents, id)
	if m.defaultAgent != nil && m.defaultAgent.Name() == id {
		m.defaultAgent = nil
	}
	slog.Info("agent removed dynamically", "id", id)
}

// Close cancels and waits for learner background work owned by every Agent.
// UserSpace invalidation calls this before dropping the last in-memory handle,
// preventing a stale extraction from recreating assets after agent deletion.
func (m *Manager) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	var errs []error
	for id, ag := range m.agents {
		if err := ag.stopSkillJobs(ctx); err != nil {
			errs = append(errs, fmt.Errorf("stop learner jobs for %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// AgentByID 按 ID 返回代理。
func (m *Manager) AgentByID(id string) *Agent {
	return m.agents[id]
}

// DefaultAgent 返回默认代理（当仅存在一个代理时设置）。
func (m *Manager) DefaultAgent() *Agent {
	return m.defaultAgent
}

// All 返回所有已加载的代理。
func (m *Manager) All() []*Agent {
	result := make([]*Agent, 0, len(m.agents))
	for _, ag := range m.agents {
		result = append(result, ag)
	}
	return result
}

// 名称返回所有代理 ID。
func (m *Manager) Names() []string {
	names := make([]string, 0, len(m.agents))
	for name := range m.agents {
		names = append(names, name)
	}
	return names
}

// UpdateProvider 替换所有代理的 LLM 提供程序（热重载）。
// 具有自己的每个代理提供程序覆盖的代理（agent.json 提供程序
// 跟踪共享的）保留他们的专用提供商 - 这个电话
// 仅影响正在使用共享实例的代理。
func (m *Manager) UpdateProvider(prov provider.Provider) {
	for _, ag := range m.agents {
		ag.provider = prov
	}
}

// UpdateProviderResolved 类似于 UpdateProvider 但了解每个代理
// 提供者覆盖。对于每个代理，它使用以下方法重建提供者
// NewManager 在构造时应用相同的规则：代理级别“providers”
// 在 agent.json 中隐藏共享后备。
func (m *Manager) UpdateProviderResolved(shared provider.Provider, resolved []config.ResolvedAgent) {
	byID := make(map[string]config.ResolvedAgent, len(resolved))
	for _, rc := range resolved {
		byID[rc.ID] = rc
	}
	for id, ag := range m.agents {
		if rc, ok := byID[id]; ok {
			ag.provider = providerForAgent(rc, shared)
		} else {
			ag.provider = shared
		}
	}
}
