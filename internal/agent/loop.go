package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/codeany-ai/open-agent-sdk-go/costtracker"

	"github.com/qs3c/bkclaw/internal/agent/goal"
	"github.com/qs3c/bkclaw/internal/agent/tools"
	"github.com/qs3c/bkclaw/internal/bus"
	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/mcp"
	"github.com/qs3c/bkclaw/internal/privacy"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/sandbox"
	"github.com/qs3c/bkclaw/internal/scope"
	"github.com/qs3c/bkclaw/internal/session"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/toolproviders"
	"github.com/qs3c/bkclaw/internal/usage"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// Agent 是 ReAct 代理循环。
type Agent struct {
	name                 string
	provider             provider.Provider
	registry             *tools.Registry
	sessions             *session.Manager
	memory               *Memory
	ctxBuilder           *ContextBuilder
	mcpMgr               *mcp.Manager
	hooks                *HookRegistry
	model                string
	maxTokens            int
	contextWindow        int
	providerConfigs      map[string]config.ProviderConfig
	temperature          float64
	maxToolIterations    int
	maxParallelToolCalls int // 0 = unlimited
	thinking             string
	// PromptMode 保留在 Agent 上，以便 ReloadWorkspaceFiles 可以重新应用它
	// 当它重建 ctxBuilder 时 - 没有这个，每个技能都会安装/
	// 仪表板重新加载会静默地将代理返回到代理模式提示符
	// 即使操作员明确选择了聊天机器人/自定义。
	// PromptMode 还通过以下方式驱动每转刀具过滤器
	// 下面内置AllowForMode。
	promptMode    string
	homePath      string // agent's home: SOUL.md, sessions, memory, skills
	workspacePath string // working dir where agent creates user files
	homeDir       string // BkClaw root, ~/.bkclaw
	ownerUserID   string // the user that owns this agent (for hook namespacing)
	// admins 是可以运行 write- 的聊天者的每个频道白名单
	// 模式斜线命令（/new /undo /retry /compact /model /personality）。
	// 按频道名称键入（例如“discord”→ [“123...”，“456...”]）。空的
	// 或缺席→没有门，任何人都可以运行该命令（传统默认值）。
	admins          map[string][]string
	skillsCfg       config.SkillsConfig
	globalSkillsCfg config.SkillsCfg
	messageBus      *bus.MessageBus
	subAgentSpawner tools.SubAgentSpawner
	ftsStore        *store.FTSStore
	piiScrubEnabled bool
	memoryCfg       config.MemoryCfg
	// splitReplies 是每个代理的多气泡切换。盖茨
	// 通告 SplitMessageMarker 的每轮系统提示提示
	// 到 LLM（参见 renderChannelHints）并盖章
	// OutboundMessage.AllowSplit 因此调度程序将回复拆分为
	// 将每个块交给通道适配器之前的标记。
	// 仅限每个代理 - 没有系统级回退。
	splitReplies bool
	// memoryStore 是可选的存储支持的身份文件源
	// （灵魂.md，身份.md，...）。保留在代理上，以便 ReloadWorkspaceFiles
	// 可以重新连接一个新的 ContextBuilder 以继续从 Store 中读取
	// 而不是默默地退回到 pod 本地文件系统。
	memoryStore MemoryStore
	// displayName 镜像agents.name（操作员指定的名称）。盖章
	// 在 IDENTITY.md 后备行的 ContextBuilder 上 — 继续
	// 代理也是如此，因此 ReloadWorkspaceFiles 可以在重建后重新应用
	// 从头开始创建 ContextBuilder。
	displayName string
	// dataStore 是完整的关系存储（当通过
	// 经理）。用于无法通过的每回合持久查找
	// 较窄的 MemoryStore — 目前只有 autoPersist 门
	// 计算（聊天、代理）用户消息行，以便节奏
	// 守护进程重新启动/用户空间失效/空闲后仍然存在
	// 所有驱逐都会重置内存中的turnCount。
	dataStore store.Store
	// 工作区存储是可选的；设置后，SkillsLoader 会为每个代理提供水合
	// 每回合都从对象存储中获取全局技能目录，因此技能
	// 启动后或同级副本上上传的内容在此处可见。
	workspaceStore workspace.Store
	skillsLearner  *SkillsLearner
	turnCount      int
	engine         *sdkEngine
	costTracker    *costtracker.Tracker
	agentID        string
	// Meter 是管理员级别的令牌 Meter。仅当
	// 网关在启动时通过 SetMeter 将其连接起来 — 仅本地开发运行
	// 将其保留为 nil，计量将通过meterTokens() 变为无操作。
	meter usage.Meter
	// sandboxPool 是每用户（代理 + 会话）沙箱池。放
	// 通过 AttachSandboxToAgents 在启动/热重载时一次；绑定会话
	// 在每个回合的顶部从其中拉出一个会话范围的执行器
	// 因此同一代理的并发会话会获得隔离的容器
	// + 隔离/工作空间安装座。
	sandboxPool sandbox.ExecutorPool

	// goalStore 是 /goal 功能的每个代理状态。有线方式
	// 电线目标；经理未提供数据的代理为零
	// 商店（旧版单用户安装）。当 nil 时，目标工具
	// 和hook根本就没有注册，所以默默的失踪了一个store
	// 降级为“功能关闭”而不是崩溃。
	goalStore goal.Store
}

// SetSandboxPool 连接每个（代理、会话）执行器池。呼叫者
// 启动时 AttachSandboxToAgents 以及之后通过热重载的 reloadSandbox
// 入职会打开沙箱。该池由bindSession 查询
// 每个聊天回合的开始——不再需要急切地启动
// 因为会话 ID 仅在聊天开始后才存在。
//
// 还翻转上下文构建器的沙箱标志，以便系统提示符
// “工作目录”/文件系统布局描述与实际相符。
// 如果没有这个，一个 rc.Sandbox.Enabled=false 但获得了
// 池引用（attachSandboxToAgents 将池连接到所有代理
// 一旦他们中的任何一个想要沙箱）最终都会通过 exec 路由
// 容器，而提示仍然通告主机路径 - 模型
// 尽职尽责地写入 `/Users/.../workspaces/<id>/foo` 其中 404
// 容器。两国必须同意。
func (a *Agent) SetSandboxPool(p sandbox.ExecutorPool) {
	a.sandboxPool = p
	if a.ctxBuilder != nil {
		a.ctxBuilder.sandboxEnabled = p != nil
	}
	// 告诉工具注册表沙箱是必需的，因此它的主机外壳执行
	// 当bindSession无法绑定执行器时，后备拒绝运行。
	// 两种状态（系统提示广告/workspace + /skills，
	// exec 实际上使用沙箱）必须同意 - 没有这个，Docker
	// 守护进程 hiccup 变成“sh：python：命令未找到”
	// 主机而不是明显的“需要沙箱但不可用”错误。
	if a.registry != nil {
		a.registry.SetSandboxRequired(p != nil)
	}
}

// bindSession 将每轮会话状态连接到工具注册表中：
// 会话范围的沙箱执行器（配置池时），
// sessionID Workspace.Store 调用用于命名空间工件，并且
// (channel, chatID) 总线地址所以延迟工作工具 (create_cron_job)
// 可以将其标记到持久的行上以供以后重播。在顶部调用
// 在任何工具运行之前的 HandleMessage / HandleMessageStream。
//
// 在并发聊天中改变共享注册表会产生竞争，但是
// 当前的不变量是每个代理一次聊天 - 网关
// 序列化每个代理的回合。在这里记录下来以防发生变化。
func (a *Agent) bindSession(ctx context.Context, channel, sessionID, projectID string) {
	a.registry.SetSessionID(sessionID)
	a.registry.SetProjectID(projectID)
	a.registry.SetMessageContext(channel, sessionID)
	if a.sandboxPool == nil {
		return
	}
	ex, err := a.sandboxPool.Get(ctx, a.name, projectID, sessionID)
	if err != nil {
		// 错误级别（不是警告）——当需要沙箱并且我们
		// 无法绑定，下一个 exec 调用将拒绝
		// “sandboxRequired 但没有执行器”消息；在这里登录，以便
		// 上游原因（docker 守护进程关闭、镜像拉取失败……）是
		// 在用户面临的错误旁边捕获。
		slog.Error("sandbox executor unavailable; exec will refuse host fallback",
			"agent", a.name, "session", sessionID, "error", err)
		return
	}
	a.registry.SetExecutor(ex)
}

// NewAgent 从已解析的配置创建一个新代理。
func NewAgent(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string) *Agent {
	return NewAgentWithSkillsCfg(rc, prov, mb, homeDir, config.SkillsCfg{})
}

func cloneProviderConfigs(in map[string]config.ProviderConfig) map[string]config.ProviderConfig {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]config.ProviderConfig, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// NewAgentWithFullCfg 创建一个具有完整配置支持（内存、隐私、技能学习者）的新代理。
func NewAgentWithFullCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, fullCfg *config.Config) *Agent {
	ag := NewAgentWithSkillsCfg(rc, prov, mb, homeDir, fullCfg.Skills)
	ag.memoryCfg = fullCfg.Memory
	ag.piiScrubEnabled = fullCfg.Privacy.PIIScrubbing.Enabled
	// splitReplies 是在 NewAgentWithSkillsCfg 内部进行检测的，所以很陌生-
	// 附属特工也拿起开关；不要在这里重新盖章。

	// 设置 FTS 存储（如果已配置）
	if fullCfg.Memory.FTS.Enabled {
		dbPath := fullCfg.Memory.FTS.DBPath
		if dbPath == "" {
			dbPath = rc.Home + "/memory/fts.db"
		}
		if fts, err := store.NewFTSStore(dbPath); err == nil {
			if err := fts.Init(); err == nil {
				ag.ftsStore = fts
				slog.Info("FTS5 search enabled", "agent", rc.ID, "db", dbPath)
			} else {
				slog.Warn("FTS5 init failed, falling back to file scan", "error", err)
			}
		} else {
			slog.Warn("FTS5 store open failed, falling back to file scan", "error", err)
		}
	}

	// 设置技能学习者（如果已配置）
	if fullCfg.SkillsLearner.Enabled {
		model := fullCfg.SkillsLearner.Model
		if model == "" {
			model = rc.Model
		}
		learnerLoader := NewSkillsLoaderWithGlobal(homeDir, rc.Home, "", rc.Skills, fullCfg.Skills)
		learnerLoader.agentID = rc.ID
		ag.skillsLearner = NewSkillsLearner(rc.Home, prov, model, learnerLoader.AllSkillDirs()...)
		if fullCfg.SkillsLearner.MinToolCalls > 0 {
			ag.skillsLearner.minToolCalls = fullCfg.SkillsLearner.MinToolCalls
		}
	}

	// 设置内存自动保留默认值
	if ag.memoryCfg.AutoPersist.EveryNTurns == 0 {
		ag.memoryCfg.AutoPersist.EveryNTurns = 5
	}

	return ag
}

// NewAgentWithSkillsCfg 创建一个具有全局技能配置的新代理，用于环境注入。
func NewAgentWithSkillsCfg(rc config.ResolvedAgent, prov provider.Provider, mb *bus.MessageBus, homeDir string, globalSkillsCfg config.SkillsCfg) *Agent {
	workspace := rc.Workspace
	if workspace == "" {
		// 未填充的调用者（测试、遗留配置）的后备
		// 工作区 — 使用代理的家作为单目录后备。
		workspace = rc.Home
	}
	// 确保工作区目录存在，以便第一个 write_file 不会失败。
	if workspace != "" {
		_ = os.MkdirAll(workspace, 0o755)
	}

	memory := NewMemory(rc.Home)
	registry := tools.NewRegistry(rc.Home, workspace)
	// 消息工具在代理结构构建后重新注册（请参阅
	// 如下），因此其出站端闭包可以读取 agent.splitReplies
	// 在发送时。 registerBuiltins 已在 NewRegistry 中传递
	// 标记占位符； tools.RegisterMessage 取代了它。
	tools.RegisterMemorySearch(registry, rc.Home)
	tools.RegisterWebFetch(registry)

	// 加载具有 OpenClaw 兼容性的技能。我们无法从 OSS 中获取水分
	// 这里 - 代理尚未构建，管理器尚未连接
	// 工作区存储。管理器将在之后调用 ReloadWorkspaceFiles
	// 使用OSS托管的技能连线刷新摘要，并运行一次
	// 每回合都会重新补充水分以接收以后的上传。
	loader := NewSkillsLoaderWithGlobal(homeDir, rc.Home, "", rc.Skills, globalSkillsCfg)
	loader.agentID = rc.ID
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)

	// 为 exec 工具设置技能环境注入。传递一个 sbCfg 携带
	// 只是启用标志，所以主机模式关闭（使用直到
	// bindSession 在会话启动时在沙盒执行器中交换）知道
	// 该代理需要沙箱 - 如果没有该信号
	// 执行程序池故障将默默地落在 /bin/sh 上
	// 主机，打破了用户要求的安全边界。
	skillDirs := loader.AllSkillDirs()
	tools.RegisterLoadSkill(registry, skillDirs)
	var sbCfg *tools.SandboxConfig
	if rc.Sandbox.Enabled {
		sbCfg = &tools.SandboxConfig{Enabled: true}
	}
	tools.RegisterExecWithSkillEnv(registry, sbCfg, loader.SkillEnvVars, skillDirs)

	if len(skills) > 0 {
		slog.Info("loaded skills", "agent", rc.ID, "count", len(skills))
	}

	// 设置带有日志记录的钩子
	hooks := NewHookRegistry()
	hooks.Register(BeforeModelCall, LoggingHook())
	hooks.Register(AfterModelCall, LoggingHook())
	hooks.Register(BeforeToolCall, LoggingHook())
	hooks.Register(AfterToolCall, LoggingHook())

	providerConfigs := cloneProviderConfigs(rc.Providers)
	contextWindow := rc.ContextWindow
	if contextWindow <= 0 {
		contextWindow = config.ResolveContextWindow(providerConfigs, rc.Model, rc.MaxTokens)
	}

	eng := newSDKEngine(rc.ID)

	ag := &Agent{
		name:                 rc.ID,
		provider:             prov,
		registry:             registry,
		sessions:             session.NewManager(rc.Home + "/sessions"),
		memory:               memory,
		ctxBuilder:           newContextBuilderWithSandbox(rc.Home, workspace, memory, skillsSummary, rc.Thinking, rc.Sandbox.Enabled, rc.Sandbox.Backend, rc.PromptMode),
		hooks:                hooks,
		model:                rc.Model,
		maxTokens:            rc.MaxTokens,
		contextWindow:        contextWindow,
		providerConfigs:      providerConfigs,
		temperature:          rc.Temperature,
		maxToolIterations:    rc.MaxToolIterations,
		maxParallelToolCalls: rc.MaxParallelToolCalls,
		thinking:             rc.Thinking,
		promptMode:           rc.PromptMode,
		homePath:             rc.Home,
		workspacePath:        workspace,
		homeDir:              homeDir,
		admins:               rc.Admins,
		skillsCfg:            rc.Skills,
		globalSkillsCfg:      globalSkillsCfg,
		messageBus:           mb,
		engine:               eng,
		costTracker:          eng.costTracker,
	}

	// 多气泡分割回复：仅限每个代理 - 系统级切换
	// 被删除，因为“每个特工都以相同的方式分裂”很少见
	// 操作员对于运行多个角色的部署的需求。
	// nil override = 关闭（默认）；非零=显式值。检测于
	// 这一层（不仅仅是NewAgentWithFullCfg）如此外挂
	// 代理 — 聊天者通过渠道联系不属于他们的代理
	// 绑定 - 也拿起切换开关。没有这个微信
	// 对于非所有者聊天，调度员提示永远不会到达法学硕士，并且
	// 模型回退到 markdown `---` 分隔符，呈现为
	// 一个泡沫。
	if rc.SplitReplies != nil {
		ag.splitReplies = *rc.SplitReplies
	}
	// 将操作员给定的显示名称标记到上下文构建器上
	// 所以空的 IDENTITY.md 不会泄漏基本模型身份
	// （“我是克劳德”）到喋喋不休——系统提示的
	// 身份后备线使用这个。还要继续代理所以
	// ReloadWorkspaceFiles（从重建 ContextBuilder
	// 刮擦）可以重新应用它而不是丢失该值。
	ag.displayName = rc.DisplayName
	ag.ctxBuilder.SetDisplayName(rc.DisplayName)
	// 自动持久内存切换 - 每个代理覆盖。经理
	// 今天只调用 NewAgentWithSkillsCfg （不是未使用的
	// NewAgentWithFullCfg)，表示系统/用户“内存”
	// 配置行实际上在生产中已死亡 - 每个代理
	// Agents.defaults.autoPersist 是唯一的工作路径。放
	// EveryNTurns 在这里也是默认的，因此模数检查
	// 当操作员启用时，runPostTurn 站点不会出现恐慌
	// 自动保留而不指定节奏。
	if rc.AutoPersist != nil {
		ag.memoryCfg.AutoPersist.Enabled = *rc.AutoPersist
	}
	if ag.memoryCfg.AutoPersist.EveryNTurns == 0 {
		ag.memoryCfg.AutoPersist.EveryNTurns = 5
	}

	// 消息工具 — 在此处注册（代理后），以便闭包可以读取
	// ag.split在每次发送时回复。每个代理设置可以翻转
	// 运行时（更新配置）； getter 分别提取当前值
	// 时间而不是捕捉陈旧的快照。
	tools.RegisterMessage(registry, mb, func() bool { return ag.splitReplies })

	// delegate_task 让父代理将有界子任务扇出到
	// 新的子代理上下文（自己的迭代预算，隔离的消息）。
	// 在ag构建后注册，因为工具回调关闭
	// ag.RunSubagent — 无法将其连接到 RegisterExecWithSkillEnv 内部
	// 预代理块。当 runner 为零时自行禁用。
	tools.RegisterDelegateTask(registry, ag)

	// 连接 MCP 服务器并注册其工具
	if len(rc.MCPServers) > 0 {
		mcpMgr := mcp.NewManager(rc.MCPServers)
		ag.mcpMgr = mcpMgr

		for _, td := range mcpMgr.ToolDefs() {
			toolName := td.Name
			ag.registry.Register(toolName, td.Description, td.InputSchema,
				func(ctx context.Context, args json.RawMessage) (string, error) {
					return mcpMgr.CallTool(ctx, toolName, args)
				},
			)
		}

		if mcpMgr.HasTools() {
			slog.Info("registered MCP tools", "agent", rc.ID)
		}
	}

	return ag
}

func newContextBuilderWithThinking(home string, memory *Memory, skillsSummary string, thinking string) *ContextBuilder {
	cb := NewContextBuilder(home, memory, skillsSummary)
	if thinking != "" {
		cb.SetThinking(thinking)
	}
	return cb
}

func newContextBuilderWithSandbox(home, workspace string, memory *Memory, skillsSummary string, thinking string, sandboxEnabled bool, sandboxBackend string, promptMode string) *ContextBuilder {
	cb := newContextBuilderWithThinking(home, memory, skillsSummary, thinking)
	cb.SetWorkspace(workspace)
	cb.sandboxEnabled = sandboxEnabled
	cb.sandboxBackend = sandboxBackend
	cb.SetPromptMode(promptMode)
	return cb
}

// 名称返回代理的姓名。
func (a *Agent) Name() string {
	return a.name
}

// HandleWebChat 使用会话 ID 处理来自 Web UI 的聊天消息。
// imageURLs 和 params 镜像流变体，因此非流式
// 调用者（点击 POST /api/chat 的第三方应用程序）得到相同的结果
// Vision + per-turn-params 支持作为 SSE 路径。
//
// projectIDHint 是 URL 中携带的聊天的“所属项目”
// (`?project=<pid>`) 或聊天请求正文。这只对非常重要
// 全新会话的第一轮：一旦该行存在，project_id
// 上面印的就是权威的，提示被忽略。
func (a *Agent) HandleWebChat(ctx context.Context, sessionId, projectIDHint, userID, text string, imageURLs []string, params map[string]any) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if userID == "" {
		// 向后兼容未经身份验证/遗留调用者：保留
		// 哨兵，使每个用户的技能能够稳定地共享
		// dir 而不是尝试 mkdir <base>/users//skills/ （其中
		// docker 会很乐意安装在用户的整个主目录上）。
		userID = "web-user"
	}
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		UserID:    userID,
		Text:      text,
		PeerKind:  "dm",
		PhotoURLs: imageURLs,
		Params:    params,
	}
	return a.HandleMessage(ctx, msg)
}

// HandleWebChatStream 通过实时事件流处理网络聊天消息。
// imageURLs 携带任何用户附加的图像（数据 URL 或可获取的 HTTPS
// 链接），因此具有视觉能力的模型将它们作为 image_url 内容部分接收
// 用户消息。 projectIDHint 镜像 HandleWebChat 的参数 — 请参阅
// 那个医生。
func (a *Agent) HandleWebChatStream(ctx context.Context, sessionId, projectIDHint, userID, text string, imageURLs []string, params map[string]any, events chan<- ChatEvent) string {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	if userID == "" {
		userID = "web-user"
	}
	ctx = ContextWithChatEvents(ctx, events)
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		AccountID: accountID,
		ChatID:    chatID,
		ProjectID: projectID,
		UserID:    userID,
		Text:      text,
		PeerKind:  "dm",
		PhotoURLs: imageURLs,
		Params:    params,
	}
	return a.HandleMessage(ctx, msg)
}

// SteerWeb 缓冲飞行中网络转向的转向消息
// 给定的会话。如果回合处于活动状态且消息已发送，则返回 true
// 缓冲（运行循环会将其折叠在工具轮之间并且
// 在现有 SSE 上发出“转向”事件），如果没有转弯则为 false
// running — 在这种情况下，调用者应该返回到正常发送。
// 会话解析完全镜像 HandleWebChatStream，因此我们可以登陆
// 运行回合所持有的相同 *session.Session 指针。
func (a *Agent) SteerWeb(sessionId, projectIDHint, text string) bool {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	channel, accountID, chatID, projectID := a.recoverWebTriple(sessionId)
	if projectID == "" {
		projectID = projectIDHint
	}
	sess := a.sessions.Get(channel, accountID, chatID, projectID)
	return sess.PushSteerIfActive(provider.Message{
		Role:      "user",
		Content:   text,
		Timestamp: time.Now().UnixMilli(),
	})
}

// SteerInbound 缓冲由以下命令控制的飞行中转向消息
// 入站消息的（频道、帐户 ID、聊天 ID、项目 ID）—
// 相同的字段 HandleMessage 解析会话（不是
// 任务队列的每个代理帐户ID），因此指针与正在运行的
// 转动。 `text` 是提交路径将具有的已格式化正文
// 已交付（例如组“\[name\]:”前缀）。没有时返回 false
// turn 处于活动状态，因此调用者会退回到 taskQueue.Submit。
func (a *Agent) SteerInbound(msg bus.InboundMessage, text string) bool {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	return sess.PushSteerIfActive(provider.Message{
		Role:      "user",
		Content:   text,
		Metadata:  senderMetadata(msg),
		Timestamp: time.Now().UnixMilli(),
	})
}

// recoveryWebTriple 映射一个 URL `?session=` 标记（可以是
// 任何频道的 session_key，或旧版网络 chat_id）完整
// (channel, accountID, chatID, projectID) 元组下游调用者
// 需要。
//
// 在不恢复 accountID 的情况下，入站 Web 会写入
// telegram/wechat 会话会查询 Manager.Get(channel, "", chatID),
// 错过现有行（其中 account_id=<bot_id>），并创建一个
// 错误三元组下的全新会话 — 用户看到回复
// 简短地说，但是刷新会加载原始会话的历史记录和
// 刚刚写的交换消失了。
//
// 对于松散的聊天，projectID 为“”并转发到入站
// 消息，因此bindSession将沙箱+workspace.Store路由到
// 项目文件夹。
//
// 两步恢复：
// 1. 如果 token 与 session_key 匹配 → 查找完整的三元组 +
// 项目。
// 2. 否则将其视为网络chat_id（保留全新的
// 该行尚不存在的“+新聊天”路径）。
func (a *Agent) recoverWebTriple(sessionId string) (channel, accountID, chatID, projectID string) {
	channel, accountID, chatID = "web", "", sessionId
	if !a.sessions.SessionExists(sessionId) {
		return
	}
	if c, acc, ci, err := a.sessions.LookupSessionTriple(sessionId); err == nil && (c != "" || ci != "") {
		channel = c
		if channel == "" {
			channel = "web"
		}
		if ci != "" {
			chatID = ci
		}
		accountID = acc
	}
	projectID = a.sessions.LookupSessionProject(sessionId)
	return
}

// home 返回代理的主（元数据）目录路径。
func (a *Agent) home() string {
	return a.homePath
}

// SetGroupContext 为此座席的系统提示配置群聊感知。
func (a *Agent) SetGroupContext(gc *GroupContext) {
	a.ctxBuilder.SetGroupContext(gc)
}

// InjectGroupMessage 将来自另一个机器人的消息附加到会话历史记录中
// 而不触发LLM呼叫。这让代理意识到还有什么
// 机器人在群聊中说道。
//
// `\[name\]:` 前缀转义括号，因此 Web UI 的 CommonMark
// 渲染器不读取短的单令牌消息（例如“[idoubi]：hello”）
// 作为链接引用定义并默默地吞下它们。 LLM仍然
// 将其读取为括号内的发件人标签 - 反斜杠转义很好 -
// 了解降价源。
func (a *Agent) InjectGroupMessage(ctx context.Context, msg bus.InboundMessage) {
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	label := msg.SenderName
	if label == "" {
		label = "Bot"
	}
	content := fmt.Sprintf("\\[%s\\]: %s", label, msg.Text)
	sess.Append(provider.Message{
		Role:     "user",
		Content:  content,
		Metadata: senderMetadata(msg),
	})
}

// SetSubAgentSpawner 为spawn_subagent 工具设置子代理生成器。
func (a *Agent) SetSubAgentSpawner(spawner tools.SubAgentSpawner) {
	a.subAgentSpawner = spawner
	tools.RegisterSubAgent(a.registry, spawner, a.name)
}

// ToolRegistry 返回代理的工具注册表以供外部注册。
func (a *Agent) ToolRegistry() *tools.Registry {
	return a.registry
}

// SetOwnerUserID 使用拥有的用户 ID 标记此代理。其值为
// 传播到每个 HookContext 中，因此像 mem0 这样的插件可以命名空间
// 每个用户的数据。
func (a *Agent) SetOwnerUserID(uid string) {
	a.ownerUserID = uid
}

// OwnerUserID 返回代理的拥有用户 ID — 代理的用户
// 创建/拥有该代理。暴露制造记录的调用者
// 代表用户（例如/目标斜线）可以标记所有权
// 无需深入代理内部。
func (a *Agent) OwnerUserID() string { return a.ownerUserID }

// SetMeter 将管理令牌计量器连接到该代理上。被称为
// 启动/热重载时的网关，因此每个聊天调用都会获得一个记录令牌
// 调用。 Nil 没问题——未设置时，meterTokens() 是无操作的。
func (a *Agent) SetMeter(m usage.Meter) { a.meter = m }

// meterTokens 记录一次聊天调用的令牌计数。安全通话
// 零使用（仍然会增加 request_count）。错误被记录但从未记录
// 传播——计量不得破坏聊天路径。代理的
// 当每个代理配置时，配置的模型字符串带有提供者前缀
// 覆盖已设置；我们将其拆分，以便计量商店提供商和模型
// 放在自己的列中，而不是将它们混在一起。
func (a *Agent) meterTokens(ctx context.Context, sessionKey string, u provider.Usage) {
	if a.meter == nil {
		return
	}
	prov, mdl := provider.SplitProviderModel(a.model)
	err := a.meter.RecordTokens(ctx, a.ownerUserID, a.agentID, sessionKey, prov, mdl,
		usage.Tokens{
			Input:         u.InputTokens,
			Output:        u.OutputTokens,
			CacheRead:     u.CacheReadTokens,
			CacheCreation: u.CacheCreationTokens,
		})
	if err != nil {
		slog.Warn("meter record failed", "agent", a.name, "error", err)
	}
}

// usageInputTokens sums the input-side token counts a provider reports for one
// LLM call: uncached input plus prompt-cache read + creation. Both provider
// adapters normalize these so they never double-count (OpenAI subtracts cached
// from input, Anthropic reports the three separately), so the sum is the real
// number of context tokens the request occupied.
func usageInputTokens(u provider.Usage) int {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// contextUsageData builds the payload attached to a turn's terminal "done"
// event so the chat UI can render a context-utilization indicator. usedTokens
// prefers the provider-reported input-side count (most accurate); when the
// provider reported nothing this turn it falls back to the chars/4 estimate the
// compaction trigger itself uses, so the indicator still tracks growth.
// contextWindow and triggerTokens are echoed straight from the same normalized
// compaction options the agent enforces, so the client renders the identical
// budget and auto-compaction threshold without re-deriving them (and without
// drifting if the constants change).
func (a *Agent) contextUsageData(last provider.Usage, requestMessages []provider.Message, toolDefs []provider.Tool) map[string]any {
	used := usageInputTokens(last)
	if used <= 0 {
		used = EstimateRequestTokens(requestMessages, toolDefs)
	}
	opts := normalizeCompactOptions(CompactOptions{
		Mode:            CompactModeProactive,
		ContextWindow:   a.contextWindow,
		MaxOutputTokens: a.maxTokens,
	})
	return map[string]any{
		"usedTokens":    used,
		"contextWindow": opts.ContextWindow,
		"triggerTokens": compactTriggerLimit(opts),
	}
}

// StreamChatToResponse 是provider.Chat 的直接替代品
// 通过管道将文本块实时传输到聊天事件通道
// content_delta 事件。 Web UI 订阅者将每个增量附加到
// 飞行中的助手气泡让用户看到答案的实现
// 逐个令牌，而不是等待整个 ReAct 循环
// 结束。
//
// 工具调用/思考/RawAssistant/用法均提取自
// 最终 (Done=true) 块，因此返回的 *provider.Response 匹配
// provider.Chat 会产生什么——调用者的下游
// 逻辑（HasToolCalls 检查、session.Append 与思考、meterTokens）
// 不必改变。
//
// 在以前称为provider.Chat的每个站点上使用它
// 句柄消息路径。实际上不进行流式传输的提供商仍然可以工作
// ——他们只是在完成时交付了一大块。
func (a *Agent) streamChatToResponse(ctx context.Context, messages []provider.Message, tools []provider.Tool) (*provider.Response, error) {
	sr, err := a.provider.ChatStream(ctx, messages, tools, a.model, a.maxTokens, a.temperature)
	if err != nil {
		return nil, err
	}
	var (
		contentBuilder strings.Builder
		toolCalls      []provider.ToolCall
		thinking       string
		thinkingSig    string
		rawAssistant   json.RawMessage
		streamUsage    provider.Usage
	)
	for {
		chunk, ok := sr.Next()
		if !ok {
			break
		}
		if chunk.Content != "" {
			contentBuilder.WriteString(chunk.Content)
			// 推动增量增量。网络聊天面板
			// 将其附加到正在进行的气泡中；消费者
			// 只知道遗留的“内容”事件
			// 忽略未知类型并依赖最终的
			// 改为发出（调用者的责任）。
			emitEvent(ctx, ChatEvent{
				Type: "content_delta",
				Data: map[string]any{"delta": chunk.Content},
			})
		}
		if chunk.Done {
			toolCalls = chunk.ToolCalls
			if chunk.Thinking != "" {
				thinking = chunk.Thinking
			}
			if chunk.ThinkingSignature != "" {
				thinkingSig = chunk.ThinkingSignature
			}
			if len(chunk.RawAssistant) > 0 {
				rawAssistant = chunk.RawAssistant
			}
			if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 ||
				chunk.Usage.CacheReadTokens > 0 || chunk.Usage.CacheCreationTokens > 0 {
				streamUsage = chunk.Usage
			}
		}
	}
	if err := sr.Err(); err != nil {
		return nil, err
	}
	// 镜像 AnthropicProvider.parseSSE 在 no 时执行的操作
	// RawAssistant 已发出，但我们仍然捕获了思考文本：
	// 将 {thinking,signature} 打包为思考内容块，以便
	// 下一回合将其正确地重播到扩展思维模型。
	if len(rawAssistant) == 0 && thinking != "" {
		if raw, err := json.Marshal(map[string]string{
			"type":      "thinking",
			"thinking":  thinking,
			"signature": thinkingSig,
		}); err == nil {
			rawAssistant = raw
		}
	}
	return &provider.Response{
		Content:      contentBuilder.String(),
		ToolCalls:    toolCalls,
		Thinking:     thinking,
		Usage:        streamUsage,
		RawAssistant: rawAssistant,
	}, nil
}

func (a *Agent) buildRequestOverhead(systemPrompt string, msg bus.InboundMessage, chatterMem *Memory) []provider.Message {
	overhead := []provider.Message{{Role: "system", Content: systemPrompt}}
	if hints := renderChannelHints(msg, a.splitReplies); hints != "" {
		overhead = append(overhead, provider.Message{Role: "system", Content: hints})
	}
	if senderMsg := renderSender(msg); senderMsg != "" {
		overhead = append(overhead, provider.Message{Role: "system", Content: senderMsg})
	}
	if paramsMsg := renderClientParams(msg.Params); paramsMsg != "" {
		overhead = append(overhead, provider.Message{Role: "system", Content: paramsMsg})
	}

	var userMD, memoryMD string
	if chatterMem != nil {
		userMD = chatterMem.LoadUserFile()
		memoryMD = chatterMem.LoadMemory()
	}
	if reminder := renderChatbotPersistenceReminder(a.promptMode, a.displayName, userMD, memoryMD); reminder != "" {
		overhead = append(overhead, provider.Message{Role: "system", Content: reminder})
	}
	return overhead
}

func (a *Agent) compactionOptions(mode CompactMode, overhead []provider.Message, toolDefs []provider.Tool, sessionKey string) CompactOptions {
	return CompactOptions{
		Mode:              mode,
		Workspace:         a.homePath,
		Provider:          a.provider,
		Model:             a.model,
		ContextWindow:     a.contextWindow,
		MaxOutputTokens:   a.maxTokens,
		OverheadMessages:  overhead,
		ToolDefs:          toolDefs,
		TailTurns:         DefaultTailTurns,
		MinTailTurns:      MinimumTailTurns,
		SummaryMaxRetries: DefaultSummaryMaxRetries,
		ArchiveStore:      a.dataStore,
		ArchiveUserID:     a.ownerUserID,
		ArchiveAgentID:    a.name,
		ArchiveSessionKey: sessionKey,
	}
}

func (a *Agent) emergencyCompactRequestMessages(sess *session.Session, overhead []provider.Message, toolDefs []provider.Tool) ([]provider.Message, bool) {
	result, err := CompactMessagesWithOptions(sess.GetMessages(), a.compactionOptions(CompactModeEmergency, overhead, toolDefs, sess.SessionKey()))
	if err != nil {
		slog.Warn("emergency compaction error", "agent", a.name, "error", err)
		return nil, false
	}
	if result == nil || !result.Pruned {
		slog.Warn("emergency compaction skipped", "agent", a.name)
		return nil, false
	}

	sess.ReplaceMessages(result.Messages)
	slog.Warn("context limit error triggered emergency compaction retry",
		"agent", a.name,
		"message_count", len(result.Messages),
	)
	return compactionRequestMessages(result.Messages, overhead), true
}

func (a *Agent) callLLMWithEmergencyRetry(
	sess *session.Session,
	overhead []provider.Message,
	toolDefs []provider.Tool,
	messages []provider.Message,
	callTools []provider.Tool,
	alreadyRetried bool,
	call func([]provider.Message, []provider.Tool) (*provider.Response, error),
) (*provider.Response, []provider.Message, bool, error) {
	resp, err := call(messages, callTools)
	if err == nil || alreadyRetried || !isContextLimitError(err) {
		return resp, messages, false, err
	}

	rebuilt, ok := a.emergencyCompactRequestMessages(sess, overhead, toolDefs)
	if !ok {
		return resp, messages, false, err
	}

	resp, err = call(rebuilt, callTools)
	return resp, rebuilt, true, err
}

// HookRegistry 返回代理的钩子注册表以进行外部钩子注册。
func (a *Agent) HookRegistry() *HookRegistry {
	return a.hooks
}

// WireGoals 为此代理打开 /goal 功能。副作用：
//
// - 将商店隐藏在代理上。
// - 注册 AfterModelCall 令牌记账钩子（折叠
// Response.Usage 进入活动目标，打开预算限制
// 排气）。
// - 注册模型可调用的 update_goal 工具。
// - 注册一个 PostTurn 钩子，当允许时，触发下一个钩子
// 同步继续。
//
// 必须在 SetOwnerUserID 之后调用，因此注册的工具和
// 钩扛右主。当 a 时由 manager.buildAgent 调用
// 数据存储可用； nil store 彻底关闭该功能。
func (a *Agent) WireGoals(st goal.Store) {
	if st == nil {
		return
	}
	a.goalStore = st

	if hook := NewTokenAccountingHook(st, a.messageBus, a.name); hook != nil {
		a.hooks.Register(AfterModelCall, hook)
	}
	tools.RegisterGoalTools(a.registry, st, a.name)

	// 仅在转弯边界（PostTurn）触发延续，而不是
	// AfterToolCall 的中途。 AfterToolCall 发布
	// 乐观地在转弯仍在运行时打开一个窗口
	// 下一个延续在总线上着陆的地方。入站之前
	// 并发/目标暂停可以； PostTurn 关闭该窗口。
	//
	// PostTurn 对每个来源都会触发 — 我们接受用户（真实的回复
	// 或 /goal 简历）和 goal_context （链接循环）。其他
	// 源（cron、心跳、子代理）不得自动继续或
	// 我们会循环。 Budget_limit 总结也作为 goal_context 到达，
	// 但 TryFireContinuation 会重新读取目标状态并继续执行
	// 非主动目标，因此总结回合不会导致连锁。
	a.hooks.Register(PostTurn, a.goalTriggerHook(allowedContinuationSources))
}

// allowedContinuationSources 是总线源的白名单
// 可以从 PostTurn 钩子自动触发下一个延续。用户
// 轮流开始/恢复循环； goal_context 将其链接起来。
var allowedContinuationSources = map[string]bool{
	bus.SourceUser:        true,
	bus.SourceGoalContext: true,
}

// goalTriggerHook 构建一个触发下一个延续的 HookFunc
// 对于飞行中的会议，当所有登机口都通过时。
func (a *Agent) goalTriggerHook(allowed map[string]bool) HookFunc {
	return func(ctx context.Context, hc *HookContext) {
		if !allowed[hc.Source] {
			return
		}
		if hc.IsPlanMode {
			return
		}
		if hc.GoalSessionKey == "" {
			return
		}
		if a.goalStore == nil {
			return
		}
		goal.TryFireContinuation(ctx, a.goalStore, a.messageBus, a.name, hc.GoalSessionKey)
	}
}

// sessionHasActiveGoal 报告此入站会话是否是
// for 有一个处于活动状态的目标。用作硬优先规则
// over auto-plan-mode：主动目标是一个自治循环；计划模式
// 是一个“等待人类批准”的门。两者不能共存
// 在不破坏目标自主保证的情况下进行同样的回合。
//
// 尽力而为：存储错误或丢失会话返回 false。一
// 每个入站轮次的索引读取 — 便宜到足以跳过缓存。
func (a *Agent) sessionHasActiveGoal(ctx context.Context, msg bus.InboundMessage) bool {
	if a.goalStore == nil || a.sessions == nil {
		return false
	}
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	if sess == nil {
		return false
	}
	g, err := a.goalStore.GetGoalBySession(ctx, a.name, sess.SessionKey())
	if err != nil || g == nil {
		return false
	}
	return g.Status == goal.StatusActive
}

// buildUserMessage 将入站消息扁平化为用户角色
// 登陆会话历史记录中的provider.Message。标签 起源 所以
// 目标上下文延续通过压缩得到识别 /
// WebChatHistory / FTS 过滤器（检查 Origin != OriginUser），
// 并合并 PhotoURL（传统 IM 单）+ PhotoURL（网络多）
// 到一个 ContentParts 切片中。仅图像发送跳过前导
// 空文本部分——一些上游拒绝无内容的有线消息。
func buildUserMessage(msg bus.InboundMessage) provider.Message {
	origin := provider.OriginUser
	if msg.Source == bus.SourceGoalContext {
		origin = provider.OriginGoalContext
	}
	// IM DM 没有前缀“[SenderName]:” — 只有一个
	// 每个 DM 的聊天，发送者已经作为每轮出现
	// 需要时系统块（请参阅 renderSender 了解组情况），
	// 并在每行前面加上英文名括号
	// 模型远离 SOUL.md 中设置的语言偏好
	// ("默认中文" loses to N copies of "[idoubicc]:" surrounding it).
	// 网络一直都是裸露的；这使得 IM DM 变得一致。
	// 组扇出仍然需要内容标签，以便模型可以判断
	// 扬声器在转弯处分开 —routing.go 前缀组
	// 消息在排队之前，所以 msg.Text 已经携带了 `[A]: …`
	// 当 PeerKind==“组”时。我们不变地通过它。
	userText := msg.Text
	userMsg := provider.Message{
		Role:     "user",
		Content:  userText,
		Origin:   origin,
		Metadata: senderMetadata(msg),
	}
	imageURLs := msg.PhotoURLs
	if msg.PhotoURL != "" {
		imageURLs = append([]string{msg.PhotoURL}, imageURLs...)
	}
	if len(imageURLs) == 0 {
		return userMsg
	}
	userMsg.Content = ""
	// 跳过空的前导文本部分 - 仅发送图像用于生成
	// `[{text: ""}, {image_url}, …]` 一些上游拒绝作为
	// 无内容的有线消息。
	var parts []provider.ContentPart
	if userText != "" {
		parts = append(parts, provider.ContentPart{Type: "text", Text: userText})
	}
	for _, u := range imageURLs {
		parts = append(parts, provider.ContentPart{
			Type: "image_url", ImageURL: &provider.ImageURL{URL: u, Detail: "auto"},
		})
	}
	userMsg.ContentParts = parts
	return userMsg
}

// RegisterWebSearchChain 使用以下方法向该代理公开 web_search 工具：
// 提供商链（主要+后备）。传递 nil 来跳过 — 该工具不会
// 出现在代理的工具列表中，因此模型无法尝试调用它。
func (a *Agent) RegisterWebSearchChain(chain *toolproviders.Chain) {
	tools.RegisterWebSearchChain(a.registry, chain)
}

// RegisterImageGenChain 向该代理公开 image_gen 工具。
func (a *Agent) RegisterImageGenChain(chain *toolproviders.Chain) {
	tools.RegisterImageGenChain(a.registry, chain)
}

// RegisterWebFetchChain 将代理的 web_fetch 后端替换为
// 供应商链（例如直接 → jina → firecrawl）。传递 nil 来保留
// 遗留的仅直接获取器已经在代理构建期间连接。
func (a *Agent) RegisterWebFetchChain(chain *toolproviders.Chain) {
	tools.RegisterWebFetchChain(a.registry, chain)
}

// RegisterTTSChain 向该代理公开 tts 工具。
func (a *Agent) RegisterTTSChain(chain *toolproviders.Chain) {
	tools.RegisterTTSChain(a.registry, chain)
}

// Sessions 返回该代理的会话管理器。
func (a *Agent) Sessions() *session.Manager {
	return a.sessions
}

// WebChatHistory 返回特定会话的聊天历史记录 -
// 名称具有历史意义；它现在服务于任何频道，因为仪表板
// 在侧边栏中显示全频道聊天。
//
// 从仅附加的 session_messages 存档中读取（通过
// Session.ArchivedMessages）而不是内存中的工作集，所以
// 压缩后会话显示原始对话而不是
// 总结+最后20回合。当没有时返回到工作集
// 存档可用（文件支持模式或预存档会话）。
//
// sessionId 可以是规范的 session_key （什么
// ListWebSessions 返回）或来自旧 URL 的旧版网络 chat_id；
// ResolveSessionKey 可以解开它们。
func (a *Agent) WebChatHistory(sessionId string) []map[string]any {
	if sessionId == "" {
		sessionId = "web-ui"
	}
	resolved := a.sessions.ResolveSessionKey(sessionId)
	sess := a.sessions.GetByKey(resolved)
	msgs := sess.ArchivedMessages()
	var history []map[string]any
	for _, m := range msgs {
		// 隐藏运行时注入的消息（当前仅 goal_context
		// 继续）。他们住在法学硕士课程中
		// 益处;将它们呈现给用户会暴露审计
		// 用户从未输入过的脚手架。匹配 Codex 的仅斜杠
		// /goal UX — 审核提示仅供内部使用。
		if m.Origin != provider.OriginUser {
			continue
		}
		switch m.Role {
		case "user":
			// 多模式用户将文本存储在 ContentParts 中，并且
			// 将内容留空（参见 HandleMessageStream 的图像
			// 附件路径）。在这里对两种形状进行表面处理：
			// - 文本（内容回退到连接的文本部分）
			// - imageUrls（image_url 部分），以便聊天 UI 可以呈现
			// 从历史加载的气泡上的图像缩略图，而不是
			// 就在飞行中的实时气泡中。
			text := m.TextContent()
			var imageURLs []string
			for _, p := range m.ContentParts {
				if p.Type == "image_url" && p.ImageURL != nil && p.ImageURL.URL != "" {
					imageURLs = append(imageURLs, p.ImageURL.URL)
				}
			}
			// IM 路由轮流存储“\[idoubi\]: hello”前缀
			// 内容，以便法学硕士可以在群聊中归属该线路
			// 当系统提示音消失时。 Web 面板呈现
			// 昵称与“senderName”元数据分开，所以
			// 在这里去掉“text”的前缀以保留气泡体
			// 干净的。覆盖转义（后修复）和未转义
			// （旧会话行）形状。
			senderName, _ := m.Metadata["senderName"].(string)
			if senderName != "" {
				text = stripSenderPrefix(text, senderName)
			}
			if text == "" && len(imageURLs) == 0 {
				continue
			}
			entry := map[string]any{"role": "user", "content": text}
			if len(imageURLs) > 0 {
				entry["imageUrls"] = imageURLs
			}
			if senderName != "" {
				entry["senderName"] = senderName
				if v, ok := m.Metadata["senderAvatarUrl"].(string); ok && v != "" {
					entry["senderAvatarUrl"] = v
				}
				if v, ok := m.Metadata["senderId"].(string); ok && v != "" {
					entry["senderId"] = v
				}
				if v, ok := m.Metadata["senderChannel"].(string); ok && v != "" {
					entry["senderChannel"] = v
				}
			}
			history = append(history, entry)
		case "assistant":
			entry := map[string]any{"role": "assistant"}
			if m.Content != "" {
				entry["content"] = m.Content
			}
			if len(m.ToolCalls) > 0 {
				var calls []map[string]string
				for _, tc := range m.ToolCalls {
					calls = append(calls, map[string]string{
						"id":        tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
				entry["toolCalls"] = calls
			}
			// Surface 保留助手端元数据，以便 UI 可以
			// 在历史记录重新加载时重新渲染迭代上限徽章等 —
			// 如果没有这个，徽章只会在实时回合中显示。
			if len(m.Metadata) > 0 {
				entry["metadata"] = m.Metadata
			}
			// 跳过空的助手消息（没有内容，没有工具调用）
			if m.Content == "" && len(m.ToolCalls) == 0 {
				continue
			}
			history = append(history, entry)
		case "tool":
			entry := map[string]any{
				"role":       "tool",
				"content":    m.Content,
				"name":       m.Name,
				"toolCallId": m.ToolCallID,
			}
			if len(m.Metadata) > 0 {
				entry["metadata"] = m.Metadata
			}
			history = append(history, entry)
		}
	}
	return history
}

// WebChatSessions 返回带有元数据的网络聊天会话列表。
func (a *Agent) WebChatSessions() []session.WebSession {
	return a.sessions.ListWebSessions()
}

// DeleteWebChatSession 通过 URL 删除聊天会话（任何频道）
// token — 接受 session_key 或旧版网络 chat_id。
func (a *Agent) DeleteWebChatSession(sessionId string) error {
	return a.sessions.DeleteSessionByID(sessionId)
}

// RenameWebChatSession 为聊天会话设置自定义标题（任何
// 频道）通过 URL 令牌。
func (a *Agent) RenameWebChatSession(sessionId, title string) error {
	return a.sessions.RenameSessionByID(sessionId, title)
}

// MoveWebChatSession 将聊天重新分配给不同的项目（或
// 当projectID为“”时将其分离）并迁移其工作区文件
// 从旧范围到新范围。驱动侧边栏拖放
// 可供性。
//
// 订单事宜：
// 1. 将 URL 令牌解析为规范的 session_key。
// 2. 读取当前的project_id，以便我们知道源工作空间
// 范围（松散聊天 = 会话/<sid>/，项目聊天 =
// 项目/<oldPid>/<sid>/)。
// 3. 释放与此聊天绑定的任何实时沙箱 - 保留它
// 将保留引用的旧绑定安装和新安装
// 直到被驱逐后才生效。主动发布所以
// 下一回合在新路径上冷启动。
// 4. 移动工作区文件（当源目录为空时无操作）。
// 5.翻转store中的sessions.project_id并删除内存中
// 会话缓存，以便下一个 Get 重新读取该行。
//
// 步骤 4 和 5 不是原子的：它们之间的崩溃会留下该行
// 指向新项目，但文件位于旧路径（或副路径）
// 反之亦然）。待处理的后续操作是幂等的 - 重新运行此操作
// 方法干净地完成迁移。
func (a *Agent) MoveWebChatSession(ctx context.Context, sessionId, projectID string) error {
	key := a.sessions.ResolveSessionKey(sessionId)
	if key == "" {
		return fmt.Errorf("session not found: %s", sessionId)
	}
	oldProject := a.sessions.LookupSessionProject(key)
	if oldProject == projectID {
		return nil
	}
	if a.sandboxPool != nil {
		if err := a.sandboxPool.Release(a.name, oldProject, key); err != nil {
			slog.Warn("MoveWebChatSession: sandbox release failed",
				"agent", a.name, "session", key, "error", err)
		}
	}
	if a.workspaceStore != nil {
		if err := a.workspaceStore.Move(ctx, a.name, oldProject, key, projectID, key); err != nil {
			return fmt.Errorf("workspace move: %w", err)
		}
	}
	return a.sessions.MoveSessionByID(sessionId, projectID)
}

// Model 返回代理的型号名称。
func (a *Agent) Model() string {
	return a.model
}

// CostTracker 返回代理的成本跟踪器以进行使用/计费查询。
func (a *Agent) CostTracker() *costtracker.Tracker {
	return a.costTracker
}

// dumpLLMRequest 将完整的 LLM 绑定负载附加到专用文件
// 当设置 BKCLAW_DUMP_LLM 时。默认路径是~/.bkclaw/logs/llm-dump.log
// (可通过 BKCLAW_DUMP_LLM_FILE 覆盖) — 与 gateway.log 分开，因此
// 数千行的系统提示并没有淹没结构化的日志
// 条目，并且无论网关是否在空中运行，都可尾部，
// 守护进程，或作为前台进程。
//
// 多行内容被写入为每回合一个块（而不是每行slog
// 调用），因此时间戳不会破坏系统提示。
func dumpLLMRequest(agentName, model string, messages []provider.Message, tools []provider.Tool) {
	if os.Getenv("BKCLAW_DUMP_LLM") == "" {
		return
	}
	path := os.Getenv("BKCLAW_DUMP_LLM_FILE")
	if path == "" {
		home := os.Getenv("BKCLAW_HOME")
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h + "/.bkclaw"
			}
		}
		if home == "" {
			return
		}
		path = home + "/logs/llm-dump.log"
	}
	_ = os.MkdirAll(filepathDir(path), 0o755)

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== LLM REQUEST  ts=%s  agent=%s  model=%s  messages=%d  tools=%d ===\n",
		time.Now().Format(time.RFC3339Nano), agentName, model, len(messages), len(tools))
	for i, m := range messages {
		fmt.Fprintf(&b, "--- msg[%d] role=%s ---\n", i, m.Role)
		// 偏好内容；回退到 ContentParts 进行多模式转向
		// （image_url 存根保持日志可读，而不是转储数据 URL）。
		content := m.Content
		if content == "" && len(m.ContentParts) > 0 {
			var pb strings.Builder
			for _, p := range m.ContentParts {
				switch p.Type {
				case "text":
					pb.WriteString(p.Text)
				case "image_url":
					pb.WriteString("[image_url]")
				default:
					fmt.Fprintf(&pb, "[%s]", p.Type)
				}
				pb.WriteString("\n")
			}
			content = pb.String()
		}
		if content != "" {
			b.WriteString(content)
			if !strings.HasSuffix(content, "\n") {
				b.WriteString("\n")
			}
		}
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "[tool_call name=%s args=%s]\n", tc.Function.Name, tc.Function.Arguments)
		}
	}
	if len(tools) > 0 {
		names := make([]string, 0, len(tools))
		for _, t := range tools {
			names = append(names, t.Function.Name)
		}
		fmt.Fprintf(&b, "--- tools (%d) ---\n%s\n", len(tools), strings.Join(names, ", "))
	}
	b.WriteString("=== END LLM REQUEST ===\n")

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		// 回退到 stderr，这样转储就不会默默丢失。
		fmt.Fprint(os.Stderr, b.String())
		return
	}
	defer f.Close()
	_, _ = f.WriteString(b.String())
}

// filepathDir 是一个微小的内联帮助器，用于避免导入路径/文件路径
// 仅针对此单个函数中的一次 Dir() 调用。
func filepathDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}

// renderClientParams 将每个请求的“params” blob 转换为 API
// 呼叫者提交了一条系统消息，推动法学硕士
// 调用工具时尊重这些值。当参数为时返回“”
// 空，这样我们就不会每次都添加噪音消息。
//
// 为什么是系统消息而不是绑定到工具参数：
//
// v1 牺牲了确定性以换取简单性。应用程序不知道是哪个
// 代理拥有的工具 - 他们只发送一个平面键/值 blob，并且
// 代理所有者的系统提示告诉法学硕士该怎么做
// 每个已知的密钥。 LLM 在复制 JSON 形状的值方面是可靠的
// 逐字进入工具调用（故障模式是“忽略”，而不是
// “损坏”）；更强的强迫层是 v2 问题。
//
// 输出形状：带有 JSON 的“## ClientParameters”部分
// 漂亮的印刷在一个围栏块上，加上一行提醒
// 模型这些都是约束。标头+栅栏是故意的——
// 法学硕士尊重作为单独文档构建的结构化参数
// 部分比行内散文更可靠。
func renderClientParams(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}
	blob, err := json.MarshalIndent(params, "", "  ")
	if err != nil {
		return ""
	}
	// 设计极简——事实是，没有行为散文。早些时候
	// 版本试图通过“视为约束”来推动模型/
	// “不要掏钱”/“查看技能部分”以及每一项
	// 打开了一个新的字面误读表面（模型处理了“model”
	// 作为调用该 API 的指令，完全拒绝“没有技能”
	// 匹配”，或者执行 `ls Skills/` 查找目录）。如何
	// 选择工具/技能是代理的常规工作，全面覆盖
	// 通过系统提示的技能部分和任何每个代理 SOUL.md。
	// 系统在这里唯一要说的是“这是数据
	// 客户发送的”——更多的都是噪音。
	return "## Client Parameters\n\n" +
		"The user's client app submitted these parameters alongside " +
		"the message. Forward them to whichever tool / skill you call.\n\n" +
		"```json\n" + string(blob) + "\n```"
}

// stripSenderPrefix 删除前导“\[name\]:”（或未转义的
// "[name]: ") 代理循环注入的归因包装器
// IM 路由的用户轮流。由网络历史渲染使用，因此
// 昵称可以通过专用元数据和气泡体显示
// 不再在头像标题旁边双重显示“[idoubi]：你好”。
// 当没有前缀匹配时返回原始字符串。
func stripSenderPrefix(text, senderName string) string {
	if senderName == "" {
		return text
	}
	for _, p := range []string{
		"\\[" + senderName + "\\]: ",
		"[" + senderName + "]: ",
	} {
		if strings.HasPrefix(text, p) {
			return text[len(p):]
		}
	}
	return text
}

// senderMetadata 从入站 IM 中提取仅限 UI 的发件人身份
// 消息（Discord/Telegram/Slack/...）并返回准备好的元数据映射
// 附加到持久的用户角色消息。网络聊天面板
// 通过 WebChatHistory 读回这些字段以渲染头像 +
// 每个气泡上的昵称标题。对于网络聊天和任何其他内容返回 nil
// 其他调用者不会填充 SenderName，这样我们就不会膨胀
// 具有空映射的 session_messages 行。
//
// 该映射故意不严格 Marshal() — 提供者序列化器
// 忽略 Message.Metadata，因此我们放在这里的任何内容都不在
// LLM 有效负载。该昵称仍然通过以下方式传递给法学硕士：
// `\[nickname\]: ` Message.Content 上的前缀（由调用者设置）。
func senderMetadata(msg bus.InboundMessage) map[string]any {
	if msg.SenderName == "" {
		return nil
	}
	md := map[string]any{
		"senderName":    msg.SenderName,
		"senderChannel": msg.Channel,
	}
	if msg.UserID != "" {
		md["senderId"] = msg.UserID
	}
	if msg.SenderAvatarURL != "" {
		md["senderAvatarUrl"] = msg.SenderAvatarURL
	}
	return md
}

// logSystemPromptFingerprint 每回合发出一条结构化行，
// 证明了法学硕士“实际上”被告知的有关技能的内容。刷新
// 记录调用栈只能证明loader产生了N个技能；这
// 确认它们在 BuildSystemPromptAs 程序集中幸存下来
// 我们即将发货的系统消息。用来追“群聊”
// 没有看到技能”报告 — 区分 DM 回合和 A 回合之间的这条线
// 同一智能体的群体轮流，分歧点变为
// 明显的。
func (a *Agent) logSystemPromptFingerprint(channel, chatID, userID, prompt string) {
	skillCount := strings.Count(prompt, "<skill name=")
	hasFeishu := strings.Contains(prompt, "feedback-to-feishu")
	// 每个聊天文件的存在 - 大小使我们一眼就能看出
	// 聊天者的 USER.md / MEMORY.md 是否实际到达
	// 本轮建模。任一为零都表示该部分被省略
	// （没有行、空内容或chatterUID 未解析）。匹配
	// 与 context.go 中使用的规范部分标题文本相对应；
	// 使其与该文件保持同步，否则诊断将消失。
	hasUserMD := strings.Contains(prompt, "<current_chatter_profile")
	hasMemorySection := strings.Contains(prompt, "<chatter_long_term_memory")
	hasSoul := strings.Contains(prompt, "# SOUL.md")
	hasIdentity := strings.Contains(prompt, "# IDENTITY.md")
	// “通过对话记住事情”是聊天机器人模式
	// 指令块告诉 LLM 它可以通过 write_file 保留。
	// 如果聊天机器人模式配置错误/未应用，此字符串
	// 不会出现在提示中，并且模型默认为“我没有
	// 记忆”反射性的回答。
	hasPersistenceInstr := strings.Contains(prompt, "Remembering things across conversations")
	mode := a.promptMode
	if mode == "" {
		mode = config.PromptModeAgent
	}
	slog.Info("system prompt assembled",
		"agent", a.name, "channel", channel, "chat_id", chatID, "user", userID,
		"mode", mode,
		"bytes", len(prompt),
		"skill_blocks", skillCount,
		"has_user_md", hasUserMD,
		"has_memory", hasMemorySection,
		"has_soul", hasSoul,
		"has_identity", hasIdentity,
		"has_persistence_instr", hasPersistenceInstr,
		"has_feedback_to_feishu", hasFeishu)
}

// renderChatbotPersistenceReminder 返回一个简洁的命令式系统
// 消息提醒法学硕士在聊天机器人模式下它有 write_file /
// edit_file 可用，并且必须使用它们来保存聊天信息。
//
// 为什么要进行每回合提醒而不是依靠“记住”
// chatbotInfo 提示中的“跨对话的事物”块：
// 聊天机器人模式下的 Sonnet 4.x（没有其他工具，简单的角色）恢复
// 在“我是一个没有持久力的人工智能”之前接受过严格的训练
// 记忆”——观察到用诸如此类的措辞对聊天者撒谎
// "跨对话我没有记忆" even when the system prompt explicitly grants
// 的能力。在之前放置一个简短的、必要的提醒
// 用户轮流购买比训练排名更高的新近度权重
// 实践中先行。
//
// userMD / memoryMD 是这些文件的当前内容（相同
// <current_chatter_profile> 下系统提示中已有的数据
// / <chatter_long_term_memory>）。我们在这里再次将它们内联为两个
// 原因：1）新近权重——它们现在是最后一条系统消息
// 在用户转身之前，模型不会错过他们； 2）十四行诗是
// 观察到将早期的系统提示副本作为模式文档读取
// 而不是在新的会议中喋喋不休地谈论事实并否认知识
// even with "Name: 狗子" present. Double-rendering is wasted tokens
// 但明显获胜。
//
// 对于非聊天机器人模式为空（没有工具表面不匹配）。
func renderChatbotPersistenceReminder(mode, displayName, userMD, memoryMD string) string {
	if mode != config.PromptModeChatbot {
		return ""
	}
	var sb strings.Builder

	// 身份强化。 Sonnet 4.x 忽略“你的名字是 X”行
	// 在系统提示中间并恢复为“我是克劳德”
	// for "你是谁" / "who are you" questions. Putting the identity in
	// 每次转弯提醒（用户转弯前的最后一条系统消息）
	// + 将其框架为违规警告似乎是所需要的
	// 在实践中推翻之前的培训。
	if displayName != "" {
		sb.WriteString("## Your identity (per-turn anchor)\n\n")
		sb.WriteString(fmt.Sprintf("In this runtime you ARE **%s**. When a chatter asks \"你是谁\" / \"who are you\", introduce yourself as **%s** — never \"Claude\" or \"AI 助手\" / \"AI assistant\". Saying \"我是 Claude\" / \"I am Claude\" is a role violation; do not do it. IDENTITY.md / SOUL.md below may add personality / role detail on top of this name, but the name itself is %s.\n\n", displayName, displayName, displayName))
	}

	sb.WriteString("## Chatter context (load-bearing — re-read every turn)\n\n")

	if strings.TrimSpace(userMD) != "" {
		sb.WriteString("These are facts about the person you're talking to RIGHT NOW (from USER.md you've persisted). Quote them verbatim when asked \"我是谁\" / \"你记得我吗\":\n\n")
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimSpace(userMD))
		sb.WriteString("\n```\n\n")
	} else {
		sb.WriteString("USER.md is empty — you do not yet know who this chatter is. When they share their name / role / preferences, you MUST call write_file('USER.md', ...) in the SAME turn so the next conversation has them.\n\n")
	}

	if strings.TrimSpace(memoryMD) != "" {
		sb.WriteString("Long-term facts you've recorded about this chatter (from MEMORY.md):\n\n")
		sb.WriteString("```\n")
		sb.WriteString(strings.TrimSpace(memoryMD))
		sb.WriteString("\n```\n\n")
	}

	sb.WriteString("## Persistence rules\n\n")
	sb.WriteString("- You have `write_file` and `edit_file` in your tools — USE them whenever you learn something worth remembering.\n")
	sb.WriteString("- Identity (name, role, preferences, location, what to call them) → `write_file('USER.md', ...)` or `edit_file('USER.md', ...)`. ALWAYS USER.md. Never MEMORY.md for these.\n")
	sb.WriteString("- Recurring topics / decisions / project facts to hold across sessions → `MEMORY.md`.\n")
	sb.WriteString("- **If MEMORY.md already contains identity-shaped content** (e.g. \"关于<name>\" with name / role / preferences mixed in), that's a prior mistake — when the chatter shares an identity update, MIGRATE the identity bits out of MEMORY.md into USER.md (write USER.md with the consolidated profile, then edit_file MEMORY.md to remove the identity bullets that just moved). Don't perpetuate the wrong structure by tacking on more identity in MEMORY.md.\n")
	sb.WriteString("- NEVER say \"我记住了\" / \"I'll remember\" without actually calling the tool. The text is a lie; the tool call is the truth.\n")
	sb.WriteString("- NEVER say \"我没有跨对话记忆\" / \"I have no cross-session memory\" — that is FALSE; USER.md and MEMORY.md persist forever once you write them.\n")
	sb.WriteString("- When asked \"你记住我了吗\" / \"我是谁\", READ the USER.md block above this message. If it has content, the answer is yes — quote the name. If it's empty, the answer is \"not yet — tell me\" and then write whatever they say.")

	return sb.String()
}

// renderChannelHints 发出每回合协议注释，LLM 可以
// 只有了解它们才感到荣幸。今天只有一个：IM
// 具有每个气泡一条消息的 UI 的通道接受
// Channels.SplitMessageMarker 标记为“将此回复拆分为多个
// 气泡。”标记常数与分离器位于同一位置
// Internal/channels/base.go 因此更改有线令牌只会触及
// 一处；实际的分割发生在渠道经理的手中
// 调度程序，跨所有 IM 适配器统一。
//
// `splitEnabled` 是每个代理的切换。当 false （默认）时，我们
// 跳过提示，这样 LLM 就永远不会学习标记和调度员
// 将任何杂散标记折叠回换行符。两个分支必须
// 保持步调一致。
//
// 对于非 IM 渠道（Web、API）返回“”，这样它们就不会浪费令牌
// 聊天者无法察觉的暗示——网络每渲染一个气泡
// 无论如何，聊天消息。
func renderChannelHints(msg bus.InboundMessage, splitEnabled bool) string {
	if !splitEnabled || !isIMChannel(msg.Channel) {
		return ""
	}
	// 仅样本就足够了——法学硕士从一个样本中获取协议
	// 格式良好的示例，无需我们列出每条规则。
	return "## Reply Format\n\n" +
		"This channel renders one chat bubble per message. To split your " +
		"reply into separate bubbles, write `" + channels.SplitMessageMarker +
		"` on its own line between the parts. Each part is sent as a " +
		"distinct message in order.\n\n" +
		"Use this when a short, conversational, multi-beat reply reads more " +
		"naturally than one long block (e.g. \"好。\\n" + channels.SplitMessageMarker +
		"\\n第一条先到了。\\n" + channels.SplitMessageMarker + "\\n第二条在这。\"). " +
		"For a single coherent answer, just reply normally — no marker needed."
}

// isIMChannel 对于每个气泡只有一条消息的通道返回 true
// UX 将一个逻辑回复拆分为多个顺序回复
// 消息自然读取。 Web/API 通道呈现长回复
// 地方——在那里分裂不会增加任何东西。
func isIMChannel(channel string) bool {
	switch channel {
	case "wechat", "telegram", "discord", "slack", "line", "feishu":
		return true
	}
	return false
}

// renderSender 每回合发出一个系统块，命名消息的发送者
// 来自原始 IM 频道。用于 GROUP 消息，因此
// 法学硕士可以将每个回合归因于正确的发言者。
//
// 跳过 DM：每个 DM 仅有一次聊天，他们的身份是
// 在整个会话中稳定并已在 USER.md / 中捕获
// 每个聊天的记忆。将其作为每回合英语系统块重复
// just adds language bias (SOUL.md's "默认中文" loses to N copies of
// “最新的用户回合是由……”发送的）而不告诉
// 法学硕士有什么新的东西。网络聊天也不会受到此限制，因此 DM
// 行为现在与网络匹配。
//
// 对于网络聊天和任何其他未填充的呼叫者返回“”
// SenderName，这样我们就不会浪费令牌。
func renderSender(msg bus.InboundMessage) string {
	if msg.SenderName == "" {
		return ""
	}
	if msg.PeerKind != "group" {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Current Sender\n\nThe latest user turn was sent by:\n")
	fmt.Fprintf(&b, "- channel: %s\n", msg.Channel)
	fmt.Fprintf(&b, "- username: %s\n", msg.SenderName)
	if msg.UserID != "" {
		fmt.Fprintf(&b, "- user_id: %s\n", msg.UserID)
	}
	if msg.PeerKind != "" {
		fmt.Fprintf(&b, "- peer_kind: %s\n", msg.PeerKind)
	}
	return b.String()
}

// isPlanMode 报告入站消息是否要求仅计划
// 输出（没有工具调用，只是用户之前查看的编号计划
// 授权实际工作）。真值：bool true，字符串“true”/“1”，
// 任何非零数字。前端发布 `params: {planMode: true}`。
func isPlanMode(params map[string]any) bool {
	v, ok := params["planMode"]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true" || t == "1"
	case float64:
		return t != 0
	case int:
		return t != 0
	}
	return false
}

// planModeNudge 是我们在计划模式轮流之前添加的系统消息。
// 阐明合同：工具在服务器端被禁用，这就是这样
// 不要尝试它们；他们将在下一个回合可用
// 用户说“开始”——因此，当
// 步骤需要一个。早期的草案只说“工具被禁用”，而没有
// “但它们存在是为了执行”一半，模型尽职尽责
// 编写没有引用任何工具的计划（包括 delegate_task，
// 这正是我们为使这些计划发挥作用而编写的工具）。这
// 模型还获得一个作为单独的系统消息注入的工具目录
// 所以它有完整的表面可供参考，而不仅仅是它的任何东西
// 从全局系统提示中记住。
func planModeNudge() string {
	return "# PLAN MODE — output a plan only\n\n" +
		"The user has switched on plan mode for this message. They want " +
		"to see what you intend to do BEFORE any real work happens.\n\n" +
		"Tools are DISABLED for this response only — do not attempt to call " +
		"any tool, it will fail. They WILL be available on the next turn " +
		"when the user replies (the available set is listed in the tool " +
		"catalog system message). Reference tool names by name in the " +
		"plan so the execution turn knows what you intend to invoke at " +
		"each step.\n\n" +
		"For multi-chunk fan-out work (find N leads in K categories, " +
		"summarize each of M docs, draft P emails, etc.) explicitly plan " +
		"to use `delegate_task` and write out the per-call task scope. " +
		"That's the only way the execution turn stays inside its " +
		"iteration budget; trying to do all of it directly will burn the " +
		"cap on exploration and never reach synthesis.\n\n" +
		"Your VERY FIRST execution action (next turn) should be " +
		"`write_file('todo.md', <plan as - [ ] items>)` so the user sees " +
		"a live progress panel as you work. Mention this in the plan as " +
		"an explicit Step 0 (or fold it into Step 1) — the UI requires " +
		"the file to render anything.\n\n" +
		"Output a numbered plan with 3-7 steps. Each step is one or two " +
		"sentences describing the action plus the tool you'll use, e.g. " +
		"\"Step 3: Use `delegate_task` to find 10 solo insurance agents in " +
		"the US Sun Belt — owner-operated, mobile-phone preferred. " +
		"Expected output: a markdown table.\". Group related micro-" +
		"actions into a single step — a plan is a roadmap, not a " +
		"transcript.\n\n" +
		"End with exactly one line: \"Reply with 'go' to execute, or " +
		"tell me what to change.\"\n\n" +
		"Do not start the work. Do not apologize for needing a plan. " +
		"Just the plan."
}

// buildToolCatalogForPlan 构建了一个紧凑的“有哪些工具可用
// 对于执行回合”参考，作为其自己的系统消息注入
// 在计划模式下。我们在计划模式下将 tools=nil 传递给 LLM，因此
// 模型不能意外调用任何 - 但这也意味着模型
// 根本无法*查看*工具注册表，这根据经验导致它
// 编写完全省略 delegate_task 的计划（它不知道
// 工具已存在）。该目录将这些知识以纯文本形式带回来
// 无需呈现可调用模式。
//
// 格式：姓名+第一句摘要，每行一个。截长
// 描述困难——模型只需要足够的信息来决定是否
// 该工具适合计划步骤，不足以构建调用。
func buildToolCatalogForPlan(toolDefs []provider.Tool) string {
	if len(toolDefs) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Tool catalog (reference only — tools are disabled THIS turn, available next turn)\n\n")
	b.WriteString("When your plan needs one of these, name it explicitly in the relevant step.\n\n")
	for _, t := range toolDefs {
		name := t.Function.Name
		desc := strings.TrimSpace(t.Function.Description)
		// 仅第一句话——保持目录可扫描。回落至
		// 如果未找到句点，则前 160 个字符（某些工具说明是
		// 连续段落）。
		if idx := strings.IndexAny(desc, ".\n"); idx > 0 && idx < 200 {
			desc = strings.TrimSpace(desc[:idx])
		} else if len(desc) > 200 {
			desc = strings.TrimSpace(desc[:200]) + "…"
		}
		fmt.Fprintf(&b, "- `%s` — %s\n", name, desc)
	}
	return b.String()
}

// handlePlanMode 是单次计划路径：存储用户
// 消息，向模型询问禁用工具的计划，坚持+发出
// 带有 planMode 元数据的响应，以便 UI 可以标记气泡。
// 无迭代循环、无上限、无工具执行。在下一个回合（发送
// 没有 planMode 标志）常规 HandleMessage 路径执行
// 反对包括该计划在内的全体会议。
func (a *Agent) handlePlanMode(ctx context.Context, msg bus.InboundMessage) string {
	chatterUID := a.chatterUserID(msg)
	ctx = sandbox.WithUserID(ctx, chatterUID)
	ctx = store.WithChatterUserID(ctx, chatterUID)
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	// Session.ctx() 从会话持有的字段构建自己的上下文
	// 而不是继承调用者的 ctx — 而不绑定
	// chatter 到 sess 本身，即我们刚刚标记的 WithChatterUserID
	// 上面永远不会到达 AppendSessionMessage / SaveSession 和
	// chatter_user_id 列保持为空。
	sess.SetChatter(chatterUID)
	// 计划起草期间的指导：计划模式没有需要消耗的 ReAct 循环
	// 进入，所以中间吃水的转向停在历史中并回答
	// 用户的下一个回合——与计划模式合同相匹配
	// （审查计划，然后回复执行）。
	sess.BeginTurn()
	defer a.flushLeftoverSteer(sess)
	defer padOrphanToolResults(sess)

	// 镜像常规路径的用户消息构造，实现多模式
	// + IM-bridge 有效负载 (PhotoURL / PhotoURLs) 登陆会话
	// 历史就像他们在非计划转弯时一样。
	userMsg := buildUserMessage(msg)
	sess.Append(userMsg)

	if a.provider == nil {
		noProviderMsg := "Agent is not configured with a usable LLM provider. Check that cfg.Providers contains the prefix referenced by model `" + a.model + "`."
		emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": noProviderMsg}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return noProviderMsg
	}

	systemPrompt := a.ctxBuilder.BuildSystemPromptAs(chatterUID, a.memory.WithUserID(chatterUID))
	a.logSystemPromptFingerprint(msg.Channel, msg.ChatID, chatterUID, systemPrompt)
	// 工具目录注入：计划模式将tools=nil传递给LLM，因此
	// 它不会意外地调用任何东西，但这也隐藏了
	// 规划模型中的注册表。没有这个，计划就写好了
	// 就好像 delegate_task / web_search / camoufox-cli 不存在一样 —
	// 这击败了计划模式设置扇出的全部意义
	// 为执行轮而努力。
	toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))
	catalog := buildToolCatalogForPlan(toolDefs)
	messages := []provider.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "system", Content: planModeNudge()},
	}
	if catalog != "" {
		messages = append(messages, provider.Message{Role: "system", Content: catalog})
	}
	messages = append(messages, sess.GetMessages()...)
	if a.piiScrubEnabled {
		messages = privacy.ScrubMessages(messages)
	}

	resp, err := a.streamChatToResponse(ctx, messages, nil)
	if err != nil {
		slog.Error("plan-mode chat failed", "agent", a.name, "error", err)
		emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": err.Error()}})
		emitEvent(ctx, ChatEvent{Type: "done"})
		return "Sorry, I couldn't draft the plan — the LLM call failed."
	}
	a.meterTokens(ctx, sess.Key(), resp.Usage)

	planMeta := map[string]any{"planMode": true}
	sess.Append(provider.Message{
		Role:         "assistant",
		Content:      resp.Content,
		Thinking:     resp.Thinking,
		Metadata:     planMeta,
		Timestamp:    time.Now().UnixMilli(),
		RawAssistant: resp.RawAssistant,
	})
	emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
		"content":  resp.Content,
		"metadata": planMeta,
	}})
	emitEvent(ctx, ChatEvent{Type: "done", Data: map[string]any{"usage": a.contextUsageData(resp.Usage, messages, toolDefs)}})
	return resp.Content
}

// appendSteer 将耗尽的转向消息折叠到正在运行的转弯中：每个
// 保存到会话中，添加到实时 LLM 消息片中，并且
// 作为“转向”事件回显，因此 Web UI 将其呈现为用户气泡
// （坚持 → 后期加入回填 + seq-dedup 免费工作）。
func (a *Agent) appendSteer(ctx context.Context, sess *session.Session, messages []provider.Message, steer []provider.Message) []provider.Message {
	for _, sm := range steer {
		sess.Append(sm)
		messages = append(messages, sm)
		emitEvent(ctx, ChatEvent{Type: "steer", Data: map[string]any{"content": sm.Content}})
		slog.Info("steer message folded into running turn", "agent", a.name)
	}
	return messages
}

// lushLeftoverSteer 处理回合结束比赛：接受的转向
// PushSteerIfActive 在循环最后一次排水之后但在转弯之前
// 声明完成（实际上只有最大迭代综合调用，
// 一个错误的转弯，或者一个亚毫秒的窗口——回合之间和
// 预制排水沟覆盖每条正常路径）。它一直被历史所铭记
// 这样它就不会丢失并适应下一回合的上下文；我们故意
// 不要为其重新运行隐藏回合（保持简单+避免
// 递归重新调度的 IM 无回复不对称性）。
func (a *Agent) flushLeftoverSteer(sess *session.Session) {
	leftover := sess.EndTurn()
	for _, m := range leftover {
		sess.Append(m)
	}
	if len(leftover) > 0 {
		slog.Warn("steer arrived at end of turn; parked in history for the next turn",
			"agent", a.name, "count", len(leftover))
	}
}

// HandleMessage 通过 ReAct 循环处理入站消息。
func (a *Agent) HandleMessage(ctx context.Context, msg bus.InboundMessage) string {
	// 首先检查斜杠命令。空回复意味着“已处理但是
	// 故意保持沉默” - /goal foo 和 /goalresume 都失败了
	// 到作为响应的流延续，所以
	// 发出单独的内容事件只会使聊天变得混乱
	// 带有多余的确认气泡。
	//
	// 将延续排队的斜线会发出“turn_pending”
	// “完成”； POST SSE 处理程序将其视为“保持打开状态，
	// 真正的答复将在下一个巴士发射的转弯处到来。”没有它，
	// 流立即关闭并且打字指示器消失
	// 当模型仍在预热时。
	if result := a.handleSlashCommand(msg); result.handled {
		if result.reply != "" {
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": result.reply}})
		}
		if result.continuationQueued {
			emitEvent(ctx, ChatEvent{Type: "turn_pending"})
		} else {
			emitEvent(ctx, ChatEvent{Type: "done"})
		}
		return result.reply
	}

	// 计划模式使 ReAct 循环短路：工具关闭，模型
	// 发出编号计划，用户查看并正常回复
	// （无 planMode 标志）在下一轮执行。让用户捕捉
	// 代理在消耗迭代预算之前探索
	// 错误的方向——我们在长期研究中看到的失败模式
	// 提示 deepseek-flash 花费了 95 条消息探索和
	// 从未产生过可交付成果。
	// 当此会话具有活动状态时，计划模式将被静默删除
	// 目标。目标应该是自主的——为人类而暂停
	// 中环审批与合同相矛盾。剥掉旗帜所以
	// 下游钩子将此回合视为正常回合（IsPlanMode=false
	// → goalTriggerHook 重新触发 PostTurn → 延续链条停留
	// 活着而不是等待 30 秒的探测）。恢复计划模式
	// 目标驱动工作期间的行为，/目标首先暂停。
	if isPlanMode(msg.Params) {
		if a.sessionHasActiveGoal(ctx, msg) {
			slog.Info("ignoring plan-mode flag — session has an active goal",
				"agent", a.name, "chat_id", msg.ChatID)
			delete(msg.Params, "planMode")
		} else {
			return a.handlePlanMode(ctx, msg)
		}
	}

	chatterUID := a.chatterUserID(msg)
	// 标记 ctx，以便沙箱层可以绑定挂载此聊天的
	// 将每用户技能目录放入容器中的 /root/.agents/skills
	// （其中“npx Skills add -g -y”写道）。标记发生在之前
	// 下面的任何 sandbox.Get 调用，以便附件 + exec 继承它。
	ctx = sandbox.WithUserID(ctx, chatterUID)
	// 使用chatter标记ctx，以便DBStore会话写入标记
	// chatter_user_id 列（sessions/session_messages/
	// 会话事件）。 user_id 保持 = UserSpace 所有者，以便管理员查看
	// 继续列出“我的机器人上的所有会话”；聊天用户 ID
	// 记录每个聊天查询的实际参与者。
	ctx = store.WithChatterUserID(ctx, chatterUID)
	// 用于技能刷新诊断的每回合通道上下文。让我们
	// 我们将内部发出的“技能摘要刷新”日志关联起来
	// 使用请求到达的通道来刷新SkillsFromStore，
	// 追查“IM 看不到代理技能”的报告。
	slog.Info("turn: refreshing skills",
		"agent", a.name, "channel", msg.Channel, "chat_id", msg.ChatID, "user", chatterUID)
	a.refreshSkillsFromStore(chatterUID)
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	// 将chatter 绑定到sess 上。 Session.ctx() 构建自己的
	// context.Background-rooted ctx 用于存储调用，因此
	// WithChatterUserID，我们在上面标记的调用者 ctx 上不会
	// 自行到达 AppendSessionMessage / SaveSession — sess 必须
	// 携带喋喋不休的内容。
	sess.SetChatter(chatterUID)
	// 将注册表绑定到此聊天会话，以便工作区.Store 读取
	// + 写入获取会话范围的路径并且（当沙箱池处于
	// 有线） exec/read_file/list_dir 使用的执行器绑定到
	// 会话私有容器。
	a.bindSession(ctx, msg.Channel, msg.ChatID, msg.ProjectID)
	// 标记本回合的聊天是否是代理所有者/频道
	// 行政。文件工具使用它来拒绝身份文件读取
	// 常规聊天内容（灵魂/身份/BOOTSTRAP/...逐字泄漏
	// 否则聊天回复）。
	a.registry.SetCallerIsAdmin(a.isAdminChatter(msg))
	// 探索目标范围工具的持久 session_key。
	// 上面的 SetSessionID 使用 msg.ChatID （频道级聊天
	// 标识符）；目标工具需要持久的 session.Session.SessionKey
	// 寻址 agent_goals 中的行。
	a.registry.SetGoalSessionKey(sess.SessionKey())
	a.registry.SetContextArchiveSessionKey(sess.SessionKey())
	// 每用户文件写入（USER.md / MEMORY.md）需要登陆
	// 每回合喋喋不休的行，而不是 UserSpace 所有者 — 请参阅
	// 路由规则的Registry.systemFileUserID。
	a.registry.SetChatterUserID(chatterUID)

	// 转向：标记飞行中的转弯，以便在运行中到达的消息
	// 缓冲到会话中（在下面的工具迭代之间耗尽）
	// 而不是开始单独的回合。冲洗剩余转向停泊任何
	// 在回合结束比赛中失利的转向成为历史。挂号的
	// 在 padOrphanToolResults 之前，因此它最后运行（延迟是后进先出） -
	// 孤儿填充首先解决了历史问题。
	sess.BeginTurn()
	defer a.flushLeftoverSteer(sess)

	// 客户端中止转弯的安全网：如果循环退出时带有
	// tool_use 从未附加其匹配的 tool_result （
	// 当一个长时间运行的执行程序正在运行时，用户单击了“停止”，
	// SDK没有返回任何响应等），填充孤儿，这样
	// 会话历史记录保持格式良好。如果没有这个，该工具将保持
	// 呈现为历史上永远旋转的“奔跑”条目
	// 重建并且下一回合的 API 调用从 Anthropic 获得 400
	// 对于孤立的 tool_use id。
	defer padOrphanToolResults(sess)

	// 重置每转刀具故障跟踪。 web_fetch（以及任何
	// 选择加入的未来工具）咨询注册表
	// PriorFailure 拒绝保证失败重试
	// 相同的回合 - 此处没有 StartTurn，之前的失败
	// 转会毒害用户明确要求的合法重试。
	a.registry.StartTurn()

	// 挂钩：BeforeSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt, UserID: a.ownerUserID})

	chatterMem := a.memory.WithUserID(chatterUID)
	systemPrompt := a.ctxBuilder.BuildSystemPromptAs(chatterUID, chatterMem)
	a.logSystemPromptFingerprint(msg.Channel, msg.ChatID, chatterUID, systemPrompt)

	// 挂钩：AfterSystemPrompt
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt, UserID: a.ownerUserID})

	// 存储原始用户消息。图像可能通过遗产到达
	// PhotoURL（单个，由 IM 桥使用）或 PhotoURL（多个，由 IM 桥使用）
	// 网络聊天上传路径）；将两者扁平化为一个内容部分
	// 切片，以便提供者看到“[文本，图像，图像，...]”。
	// buildUserMessage 处理多图像拼合 + senderMetadata。
	// `[SenderName]:` 内容前缀策略存在于此（仅限组；
	// DM 保持裸露以避免 SOUL.md 语言偏差回归）。
	userMsg := buildUserMessage(msg)
	anchor := a.beginTurnAnchor(sess, userMsg)

	// 上下文压缩：检查会话消息是否太大
	toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))
	overheadMessages := a.buildRequestOverhead(systemPrompt, msg, chatterMem)
	sessionMsgs := sess.GetMessages()
	compactResult, err := CompactMessagesWithOptions(sessionMsgs, a.compactionOptions(CompactModeProactive, overheadMessages, toolDefs, sess.SessionKey()))
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		// 用压缩版本替换会话消息
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
		slog.Info("context compacted", "agent", a.name, "log_file", compactResult.LogFile)
	}

	messages := compactionRequestMessages(sessionMsgs, overheadMessages)

	// 循环检测：跟踪连续的相同工具调用
	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0
	totalToolCalls := 0
	// allFailedRounds 是连续回合的计数，其中每个
	// 工具结果作为 4xx/5xx HTTP 错误或执行程序返回
	// 错误。这抓住了“模型旋转了五个猜测
	// 所有 404" 模式的 URL 都会进行循环检测（关键是
	// 相同的参数）未命中。经过三轮这样的回合后，我们会丢弃工具
	// 从下一个 LLM 调用开始，模型被迫生成文本
	// 直接进行，而不是花费更多的时间去追逐无效的 URL。
	allFailedRounds := 0
	const failedRoundsLimit = 3
	emergencyRetried := false

	// replyParts 累积每个非空助理文本段
	// 跨迭代发出（工具调用之前的前导行+
	// 最终答案）。 IM 通道为每个通道传送一条 OutboundMessage
	// 转，所以没有积累，只有最后一段到达微信
	// 而聊天面板显示了所有这些。加入了
	// 返回时的channels.SplitMessageMarker； manager.dispatchOutb​​ound
	// 对其进行拆分 (AllowSplit=true) 或折叠为换行符。
	var replyParts []string

	// lastUsage 保存本轮最近一次 LLM 调用报告的 token 用量，
	// 用于在 "done" 事件上向前端汇报上下文占用百分比。仅在
	// provider 实际报告了用量时更新，避免后续零值调用抹掉真实值。
	var lastUsage provider.Usage

	// 反应循环
	for i := 0; i < a.maxToolIterations; i++ {
		slog.Info("agent loop iteration",
			"agent", a.name,
			"iteration", i+1,
			"channel", msg.Channel,
			"chat_id", msg.ChatID,
		)

		// 挂钩：BeforeModelCall
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcBefore)

		if a.provider == nil {
			slog.Error("agent has no provider configured", "agent", a.name, "model", a.model)
			noProviderMsg := "Agent is not configured with a usable LLM provider. Check that cfg.Providers contains the prefix referenced by model `" + a.model + "`."
			emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": noProviderMsg}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return noProviderMsg
		}
		// 经过足够多的连续回合后，所有工具都回来了
		// 作为 4xx/5xx，从下一次调用中删除工具，以便模型为
		// 被迫用它所拥有的内容产生一个文本答案。这
		// 请求之上的系统消息做出约束
		// 明确，因此模型不会抱歉地悬而未决。
		callTools := toolDefs
		var requestAppend []provider.Message
		if allFailedRounds >= failedRoundsLimit {
			slog.Warn("disabling tools after consecutive failed rounds",
				"agent", a.name, "failed_rounds", allFailedRounds)
			callTools = nil
			requestAppend = append(requestAppend, provider.Message{
				Role: "system",
				Content: fmt.Sprintf(
					"The last %d rounds of tool calls all failed (HTTP errors or empty results). Stop calling tools and answer the user directly with what you know — explain that authoritative sources weren't reachable and provide your best-effort response based on training knowledge, clearly marked as unverified.",
					allFailedRounds,
				),
			})
		}
		resp, updatedMessages, retried, err := a.callLLMWithEmergencyRetry(sess, overheadMessages, toolDefs, messages, callTools, emergencyRetried, func(request []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			// PII 清理：在发送给 LLM 之前编辑敏感数据
			llmMessages := request
			if a.piiScrubEnabled {
				llmMessages = privacy.ScrubMessages(request)
			}
			if len(requestAppend) > 0 {
				llmMessages = append(llmMessages, requestAppend...)
			}
			dumpLLMRequest(a.name, a.model, llmMessages, tools)
			return a.streamChatToResponse(ctx, llmMessages, tools)
		})
		if retried {
			emergencyRetried = true
			messages = updatedMessages
		}

		// 挂钩：AfterModelCall
		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID, GoalSessionKey: a.registry.GoalSessionKey()}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			emitEvent(ctx, ChatEvent{Type: "error", Data: map[string]any{"message": err.Error()}})
			emitEvent(ctx, ChatEvent{Type: "done"})
			return "Sorry, I encountered an error processing your request."
		}
		a.meterTokens(ctx, sess.Key(), resp.Usage)
		if usageInputTokens(resp.Usage) > 0 {
			lastUsage = resp.Usage
		}
		a.maybeRecoverToolCalls(resp)

		if !resp.HasToolCalls() {
			asst := provider.Message{Role: "assistant", Content: resp.Content, Thinking: resp.Thinking, Timestamp: time.Now().UnixMilli(), RawAssistant: resp.RawAssistant}
			sess.Append(asst)
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
			if resp.Content != "" {
				replyParts = append(replyParts, resp.Content)
			}
			// 回合结束转向比赛：最后一个之后缓冲的消息
			// 在我们宣布回合结束之前，回合之间的资金会耗尽。
			// 将其折叠起来并继续前进而不是返回，所以
			// 用户的飞行中指令不会推迟到新的回合。
			if steer := sess.DrainSteer(); len(steer) > 0 {
				// 将刚刚生成的答案带入下一次 LLM 通话
				// 仅当它有文本时。无文本、无工具调用
				// 助理消息对 Anthropic 来说无效
				// （助理回合需要非空内容块），
				// 这是重新发送的唯一路径。
				if resp.Content != "" {
					messages = append(messages, asst)
				}
				messages = a.appendSteer(ctx, sess, messages, steer)
				continue
			}
			emitEvent(ctx, ChatEvent{Type: "done", Data: map[string]any{"usage": a.contextUsageData(lastUsage, messages, toolDefs)}})
			a.runPostTurn(ctx, msg, messages, totalToolCalls, chatterMem, anchor)
			return joinReplyParts(replyParts)
		}

		// 在工具调用之前发出助手内容（如果存在）
		if resp.Content != "" {
			emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": resp.Content}})
			replyParts = append(replyParts, resp.Content)
		}

		// 发出 tool_call 事件
		for _, tc := range resp.ToolCalls {
			emitEvent(ctx, ChatEvent{Type: "tool_call", Data: map[string]any{
				"id":        tc.ID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
			}})
		}

		assistantMsg := provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			Timestamp:    time.Now().UnixMilli(),
			RawAssistant: resp.RawAssistant,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// 循环检测：执行前检查
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// 触发 BeforeToolCall 挂钩
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{
				AgentName: a.name,
				Point:     BeforeToolCall,
				ToolName:  tc.Function.Name,
				ToolArgs:  tc.Function.Arguments,
				Channel:   msg.Channel,
				AccountID: msg.AccountID,
				ChatID:    msg.ChatID,
				UserID:    a.ownerUserID,
			})
		}

		// 应用每轮平行盖。 LLM 决定多少名
		// 工具调用来发出；我们限制同时运行的数量
		// 圆形的。溢出得到一个合成的“延迟”tool_result 所以
		// 模型将它们视为已解决（没有孤立的 tool_use id
		// 这会毒害下一个 API 请求）但没有
		// 内容——下一轮自然会在可能的情况下重新发布它们
		// 对执行批次的结果做出反应。有效违约
		// 0 = 无限制；用户点击了特定速率限制的 API
		// （勇敢自由层1RPS等）设置为1 / 2强制
		// 严格串行/轻度并行执行。
		executeCalls := resp.ToolCalls
		var deferredCalls []provider.ToolCall
		if a.maxParallelToolCalls > 0 && len(resp.ToolCalls) > a.maxParallelToolCalls {
			executeCalls = resp.ToolCalls[:a.maxParallelToolCalls]
			deferredCalls = resp.ToolCalls[a.maxParallelToolCalls:]
			slog.Info("deferring tool calls beyond parallel cap",
				"agent", a.name,
				"cap", a.maxParallelToolCalls,
				"deferred", len(deferredCalls),
			)
		}

		// 通过SDK引擎同时执行工具
		slog.Info("executing tools concurrently",
			"agent", a.name,
			"count", len(executeCalls),
		)
		results := a.engine.executeToolsConcurrently(ctx, a.registry, executeCalls, a.workspacePath)
		// 附加合成延迟结果，以便每个原始 tool_use
		// id 有一个配对的 tool_result。延迟消息告诉
		// 准确地模拟它没有运行的原因 - 接下来可以重新发出
		// 一旦获得执行批次的结果，就进行一轮。
		for _, tc := range deferredCalls {
			results = append(results, toolCallResult{
				toolCallID: tc.ID,
				toolName:   tc.Function.Name,
				result: fmt.Sprintf(
					"Deferred — this turn's parallel-tool cap is %d, and you emitted %d. Re-issue this exact call next round if you still need it; you'll have the other tools' results to inform the decision then.",
					a.maxParallelToolCalls, len(resp.ToolCalls),
				),
			})
		}

		// 防御性后盾：如果 SDK 返回的结果少于工具
		// 呼叫（桥不知何故还没有垫上——皮带和
		// 由于孤儿 tool_use id 毒害了下一个 API 请求，吊带器
		// 使用 HTTP 400），综合失败结果，以便每个 tool_use
		// 在对话历史记录中获取配对的 tool_result。
		if len(results) < len(resp.ToolCalls) {
			padded := make([]toolCallResult, len(resp.ToolCalls))
			gotByID := make(map[string]toolCallResult, len(results))
			for _, r := range results {
				gotByID[r.toolCallID] = r
			}
			for i, tc := range resp.ToolCalls {
				if r, ok := gotByID[tc.ID]; ok {
					padded[i] = r
					continue
				}
				padded[i] = toolCallResult{
					toolCallID: tc.ID,
					toolName:   tc.Function.Name,
					result:     "tool execution did not return a result",
					err:        fmt.Errorf("missing executor response for %s", tc.ID),
				}
			}
			results = padded
		}

		// 回合级故障检测：是否每个结果都返回
		// 作为 4xx/5xx HTTP 错误或执行程序错误？跟踪到这里所以
		// 下一次迭代可以决定是否丢弃工具。
		roundAllFailed := len(results) > 0
		// 处理结果
		for idx, r := range results {
			totalToolCalls++
			tc := resp.ToolCalls[idx]
			resultContent, meta := extractToolMeta(r.result)

			// 挂钩：AfterToolCall
			a.hooks.Run(ctx, &HookContext{
				AgentName:      a.name,
				Point:          AfterToolCall,
				ToolName:       r.toolName,
				ToolResult:     resultContent,
				Error:          r.err,
				Channel:        msg.Channel,
				AccountID:      msg.AccountID,
				ChatID:         msg.ChatID,
				UserID:         a.ownerUserID,
				GoalSessionKey: a.registry.GoalSessionKey(),
				IsPlanMode:     isPlanMode(msg.Params),
				Source:         msg.Source,
			})

			if r.err != nil {
				slog.Warn("tool execution error",
					"agent", a.name,
					"name", r.toolName,
					"error", r.err,
				)
			}

			// 对结果进行分类：这一次调用失败了吗？记录
			// 它在注册表的每回合故障图中，所以稍后
			// 重试相同的参数可以被短路（参见
			// 注册表.PriorFailure / web_fetch）。
			thisFailed := isFailedToolResult(r.err, resultContent)
			if thisFailed {
				summary := r.err.Error()
				if summary == "" || summary == "<nil>" {
					summary = firstNonEmptyLine(resultContent)
				}
				a.registry.RecordToolFailure(r.toolName, tc.Function.Arguments, summary)
			} else {
				// 这一轮的一次通话产生了一个真实的结果——
				// 整个回合并不是“全部失败”。
				roundAllFailed = false
			}

			// FTS 中的索引（如果可用）
			if a.ftsStore != nil {
				_ = a.ftsStore.Index(a.name, msg.ChatID, "tool:"+r.toolName, resultContent, time.Now())
			}

			// 检查工具输出中的媒体：协议
			if mediaPaths := extractMediaPaths(resultContent); len(mediaPaths) > 0 {
				a.sendMediaFiles(msg, mediaPaths)
			}

			toolMsg := provider.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       r.toolName,
				Metadata:   meta,
			}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)

			evt := map[string]any{
				"id":     tc.ID,
				"name":   r.toolName,
				"result": resultContent,
			}
			if meta != nil {
				evt["metadata"] = meta
			}
			emitEvent(ctx, ChatEvent{Type: "tool_result", Data: evt})
		}
		// 现在更新连续失败回合计数
		// 本轮结果已处理。单次无故障
		// 重置它 - 模型刚刚获得有用的信息，给它空间
		// 使用它。
		if roundAllFailed {
			allFailedRounds++
		} else {
			allFailedRounds = 0
		}

		// 转向：此工具运行时到达的消息是
		// 折叠在这里，在轮次之间，所以下一个法学硕士电话可以看到他们
		// 并且可以改变路线。
		if steer := sess.DrainSteer(); len(steer) > 0 {
			messages = a.appendSteer(ctx, sess, messages, steer)
		}
	}

	slog.Warn("max tool iterations reached — forcing final delivery", "agent", a.name, "max", a.maxToolIterations)
	// 强制最终交付：又一个法学硕士通话，工具被禁用，并且
	// 微移告诉模型合成它所拥有的内容。取代了
	// 只返回预设警告的旧行为，这让用户
	// 在整个迭代预算被烧毁后，可交付成果为零。
	finalMessages := append(messages, capReachedNudge(a.maxToolIterations))
	if a.piiScrubEnabled {
		finalMessages = privacy.ScrubMessages(finalMessages)
	}
	finalContent := ""
	finalResp, finalErr := a.streamChatToResponse(ctx, finalMessages, nil)
	if finalErr == nil {
		finalContent = finalResp.Content
		a.meterTokens(ctx, sess.Key(), finalResp.Usage)
		if usageInputTokens(finalResp.Usage) > 0 {
			lastUsage = finalResp.Usage
		}
	}
	if finalContent == "" {
		// 综合调用本身失败或返回空 - 回退到
		// 固定线路，因此用户仍然可以通过
		// 附有徽章。
		finalContent = fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
	}
	capMeta := iterationCapMetadata(a.maxToolIterations)
	sess.Append(provider.Message{
		Role:      "assistant",
		Content:   finalContent,
		Metadata:  capMeta,
		Timestamp: time.Now().UnixMilli(),
	})
	emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
		"content":  finalContent,
		"metadata": capMeta,
	}})
	if finalContent != "" {
		replyParts = append(replyParts, finalContent)
	}
	emitEvent(ctx, ChatEvent{Type: "done", Data: map[string]any{"usage": a.contextUsageData(lastUsage, finalMessages, toolDefs)}})
	a.runPostTurn(ctx, msg, messages, totalToolCalls, chatterMem, anchor)
	return joinReplyParts(replyParts)
}

// joinReplyParts 将累积的辅助文本片段与
// channels.SplitMessageMarker 以便 manager.dispatchOutb​​ound 可以传递
// 当AllowSplit 为true 时，它​​们作为单独的IM 气泡。渠道
// 如果没有AllowSplit，则在调度时将标记折叠为换行符
// 时间，因此用户仍然可以看到一条消息中的每个片段，而不是
// 放弃除最后一个以外的所有内容。
func joinReplyParts(parts []string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return ""
	}
	if len(out) == 1 {
		return out[0]
	}
	return strings.Join(out, channels.SplitMessageMarker)
}

// isFailedToolResult 是代理循环对于“此工具
// 返回没有任何有用的信息”。使用两者来填充每回合失败
// 地图（以便可以预先拒绝稍后的相同呼叫）并开车
// 连续失败的回合短路。我们刻意留下来
// 保守 — 对于许多 shell 命令来说，空的 exec 输出是合法的 —
// 并且仅标记高信号模式：工具错误、HTTP 4xx/5xx、
// 或者我们的包装器附加到的“[分析上面的错误...]”信封
// 上游故障。
func isFailedToolResult(err error, content string) bool {
	if err != nil {
		return true
	}
	c := strings.TrimSpace(content)
	if strings.HasPrefix(c, "HTTP 4") || strings.HasPrefix(c, "HTTP 5") {
		return true
	}
	if strings.Contains(c, "[Analyze the error above and try a different approach.]") {
		return true
	}
	return false
}

// firstNonEmptyLine 返回 s 的第一个非空行，已修剪
// 且上限为 120 个字符。用于对某个内容进行存储友好的摘要
// err.Error() 为空时的工具结果。 （命名明显来自
// Skills.firstLine 以避免重复声明。）
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 120 {
			return line[:120] + "…"
		}
		return line
	}
	return ""
}

// padOrphanToolResults 遍历会话并附加合成
// tool_result 为来自最新助手消息的任何 tool_use id
// 还没有匹配的 tool_result。前几轮不是
// 扫描 - 一旦循环移过它们，它们就已经是
// 格式良好，否则前一轮的 API 调用将会失败。
//
// 由 HandleMessage 的延迟触发，因此客户端停止（或任何其他
// 过早退出）不能让对话处于下一个对话的状态
// Turn 的 API 调用获取孤立 tool_use id 的 400，并且 UI 保持不变
// 旋转一个永远无法解决的“运行工具”指标。
func padOrphanToolResults(sess *session.Session) {
	msgs := sess.GetMessages()
	// 返回最新的助手消息；如果它没有 tool_calls
	// 或者所有 tool_calls 之后都已经有结果，无需执行任何操作。
	lastAssistantIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx < 0 {
		return
	}
	resolved := make(map[string]bool)
	for _, m := range msgs[lastAssistantIdx+1:] {
		if m.Role == "tool" && m.ToolCallID != "" {
			resolved[m.ToolCallID] = true
		}
	}
	for _, tc := range msgs[lastAssistantIdx].ToolCalls {
		if resolved[tc.ID] {
			continue
		}
		slog.Warn("padding orphan tool_use with stopped result",
			"toolCallID", tc.ID, "tool", tc.Function.Name)
		sess.Append(provider.Message{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    "(stopped — execution was interrupted before the tool returned)",
		})
	}
}

// msg 是驱动本轮的 InboundMessage — 它的（频道、帐户、
// 聊天、项目）加上 Source 在 HookContext 上运行，所以 PostTurn
// 钩子可以路由到会话范围的状态并告诉用户驱动的转向
// 除了运行时产生的（cron、heartbeat、sub-agent、goal
// 继续）。
//
// chatterMem 是在回合顶部构建的chatter-scoped Memory —
// 自动持久通过它写回提取的事实，以便访问者
// 公共代理会累积他们的*自己的* MEMORY.md / USER.md，而不是
// 业主的。 nil 回退到代理范围的内存（遗留行为）。
//
// turnAnchor 标识一个 turn 起点写入的锚点行(session_messages 中 turn_status='running'
// 的那一行)。turn 结束时(runPostTurn)用它把锚点翻成 done 并参与提取节拍。
// 并发/重叠 turn 各自持有独立的 turnAnchor,因此显式透传而非挂在 Session 上。
// nil = 该次调用没有锚点(计划模式,或无持久化 store)。
type turnAnchor struct {
	sessionKey string
	seq        int64
}

// beginTurnAnchor 在 turn 起点把用户消息作为锚点写入(turn_status='running')并返回
// *turnAnchor。失败(已记日志)或无持久化 store(seq<0)时返回 nil——该 turn 不计入
// 提取,但消息本身已由 AppendTurnAnchor 进入工作集。两个 ReAct 入口共用,避免重复。
func (a *Agent) beginTurnAnchor(sess *session.Session, userMsg provider.Message) *turnAnchor {
	seq, err := sess.AppendTurnAnchor(userMsg)
	if err != nil {
		slog.Warn("turn anchor append failed", "agent", a.name, "error", err)
		return nil
	}
	if seq < 0 {
		return nil
	}
	return &turnAnchor{sessionKey: sess.SessionKey(), seq: seq}
}

// 流式（HandleMessageStream）和非流式（HandleMessage）两者
// 开火这个。流路径从后台内部调用它
// 在最后一个助手之后，耗尽 SSE 流的 goroutine
// 消息已被附加到会话中——即在用户的
// 答复已完全记录在案。
func (a *Agent) runPostTurn(ctx context.Context, msg bus.InboundMessage, messages []provider.Message, toolCallCount int, chatterMem *Memory, anchor *turnAnchor) {
	if chatterMem == nil {
		chatterMem = a.memory
	}
	a.turnCount++

	// 在 FTS 中索引用户/助理消息。跳过运行时注入
	// 消息（例如 goal_context 延续）——它们是合成的
	// 审核提示，不可搜索的对话内容。
	if a.ftsStore != nil {
		for _, m := range messages {
			if m.Origin != provider.OriginUser {
				continue
			}
			if m.Role == "user" || m.Role == "assistant" {
				_ = a.ftsStore.Index(a.name, "", m.Role, m.Content, time.Now())
			}
		}
	}

	// 火柱转钩
	a.hooks.Run(ctx, &HookContext{
		AgentName:      a.name,
		Point:          PostTurn,
		Messages:       messages,
		TurnCount:      a.turnCount,
		ToolCallCount:  toolCallCount,
		Workspace:      a.homePath,
		UserID:         a.ownerUserID,
		Channel:        msg.Channel,
		AccountID:      msg.AccountID,
		ChatID:         msg.ChatID,
		Source:         msg.Source,
		GoalSessionKey: a.registry.GoalSessionKey(),
		IsPlanMode:     isPlanMode(msg.Params),
	})

	// turn 结束:把锚点翻成 done,并按"已完成且未提取 >= N"的节拍认领一批 turn 异步提取。
	a.finishTurnAndMaybeExtract(ctx, chatterMem, anchor)

	// 技能学习者
	if a.skillsLearner != nil {
		go func() {
			if err := a.skillsLearner.MaybeExtract(ctx, messages, toolCallCount); err != nil {
				slog.Debug("skills learner error", "error", err)
			}
		}()
	}
}

// finishTurnAndMaybeExtract 在 turn 结束时把锚点翻成 done,并按"已完成且未提取 >= N"
// 的节拍在单个写事务内认领一批 turn,异步从归档回放它们的原文做记忆提取。
// 提取失败则补偿重置 extraction_id,让这批 turn 回到待提取池。
// 无持久化存储或本次无锚点(计划模式)时直接跳过。
func (a *Agent) finishTurnAndMaybeExtract(ctx context.Context, chatterMem *Memory, anchor *turnAnchor) {
	if a.dataStore == nil || anchor == nil {
		return
	}
	if err := a.dataStore.FinishTurn(ctx, a.ownerUserID, a.name, anchor.sessionKey, anchor.seq); err != nil {
		slog.Warn("finish turn failed", "agent", a.name, "error", err)
		// 锚点没翻成 done 只是本 turn 不计入提取,不阻塞主流程。
	}
	var chatterUID string
	if chatterMem != nil {
		chatterUID = chatterMem.UserID()
	}
	if !a.memoryCfg.AutoPersist.Enabled || a.memoryCfg.AutoPersist.EveryNTurns <= 0 || chatterUID == "" {
		return
	}
	n := a.memoryCfg.AutoPersist.EveryNTurns
	extractionID, refs, err := a.dataStore.ClaimCadenceBatch(ctx, a.name, chatterUID, n, 3*n)
	if err != nil {
		slog.Warn("auto-persist: claim failed", "agent", a.name, "chatter", chatterUID, "error", err)
		return
	}
	if extractionID == "" {
		return // 不足 N,不触发
	}
	model := a.memoryCfg.AutoPersist.Model
	if model == "" {
		model = a.model
	}
	slog.Info("auto-persist firing", "agent", a.name, "chatter", chatterUID, "model", model, "turns", len(refs), "extraction_id", extractionID)
	go func() {
		// 提取脱离本回合请求 ctx 的取消:回合/流结束会取消 ctx,而批次已被
		// ClaimCadenceBatch 持久认领;若提取与补偿重置都因 ctx 取消而失败,这批
		// turn 会永久卡在已认领、再不被提取。WithoutCancel 保留 ctx 上的值(chatter
		// 等),提取给 5 分钟上限防 LLM 挂死;补偿重置用独立短上下文,这样即使提取
		// 因超时失败,重置仍能把批次放回待提取池。
		base := context.WithoutCancel(ctx)
		extractCtx, cancel := context.WithTimeout(base, 5*time.Minute)
		defer cancel()
		resetBatch := func() {
			rctx, rcancel := context.WithTimeout(base, 30*time.Second)
			defer rcancel()
			_ = a.dataStore.ResetExtraction(rctx, extractionID)
		}
		groups, err := a.dataStore.LoadTurnMessages(extractCtx, a.ownerUserID, a.name, refs)
		if err != nil {
			slog.Warn("auto-persist: load turn messages failed", "agent", a.name, "extraction_id", extractionID, "error", err)
			resetBatch()
			return
		}
		if err := AutoPersistMemory(extractCtx, chatterMem, a.provider, model, groups); err != nil {
			slog.Warn("auto-persist: extraction failed, resetting batch", "agent", a.name, "extraction_id", extractionID, "error", err)
			resetBatch()
		}
	}()
}

// HandleMessageStream通过ReAct循环处理消息并返回
// 用于最终响应的 StreamReader。工具调用迭代使用非流式聊天；
// 最终文本响应使用 ChatStream 进行真正的 SSE 流式传输。
func (a *Agent) HandleMessageStream(ctx context.Context, msg bus.InboundMessage) *provider.StreamReader {
	emergencyRetried := false

	// 重用 HandleMessage 中的设置逻辑。空回复是“已处理
	// 但沉默”——参见 HandleMessage 双胞胎。仍然发出 Done
	// 块，以便等待流的调用者不会挂起。
	if result := a.handleSlashCommand(msg); result.handled {
		ch := make(chan provider.StreamChunk, 2)
		go func() {
			ch <- provider.StreamChunk{Content: result.reply, Done: true}
			close(ch)
		}()
		return provider.NewStreamReader(ch)
	}

	chatterUID := a.chatterUserID(msg)
	ctx = sandbox.WithUserID(ctx, chatterUID)
	// 标记 ctx，以便 DBStore 会话写入时间戳 chatter_user_id — 请参阅
	// HandleMessage 路径的基本原理。
	ctx = store.WithChatterUserID(ctx, chatterUID)
	slog.Info("turn: refreshing skills",
		"agent", a.name, "channel", msg.Channel, "chat_id", msg.ChatID, "user", chatterUID)
	a.refreshSkillsFromStore(chatterUID)
	sess := a.sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	// 将chatter绑定到sess上，使其ctx()嵌入WithChatterUserID
	// 对于 DBStore 会话写入 — Session.ctx() 从其重建 ctx
	// 自己的领域，所以喋喋不休必须生活在 ses 本身。
	sess.SetChatter(chatterUID)
	a.bindSession(ctx, msg.Channel, msg.ChatID, msg.ProjectID)
	a.registry.SetCallerIsAdmin(a.isAdminChatter(msg))
	a.registry.SetGoalSessionKey(sess.SessionKey())
	a.registry.SetContextArchiveSessionKey(sess.SessionKey())
	// 每用户文件写入（USER.md / MEMORY.md）需要登陆
	// 每回合喋喋不休的行，而不是 UserSpace 所有者 — 请参阅
	// 路由规则的Registry.systemFileUserID。
	a.registry.SetChatterUserID(chatterUID)

	// 与 HandleMessage 相同的 orphan-tool_use 安全网。流媒体路径
	// 以前缺少这个，所以循环检测（它附加了一个助手
	// tool_use + 系统警告并在不运行工具的情况下中断）和
	// sess.Append(assistantMsg) 和工具之间的任何其他过早退出
	// 结果在会话中追加左侧孤立的 tool_use id。下一个
	// Turn 的 API 请求——尤其是针​​对 Anthropic-compat 端点
	// 就像 DeepSeek 的 /anthropic — 然后找到了 400 个带有“tool_use id”的
	// 紧接着“之后没有 tool_result 块”。
	defer padOrphanToolResults(sess)

	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeSystemPrompt, UserID: a.ownerUserID})
	chatterMem := a.memory.WithUserID(chatterUID)
	systemPrompt := a.ctxBuilder.BuildSystemPromptAs(chatterUID, chatterMem)
	a.logSystemPromptFingerprint(msg.Channel, msg.ChatID, chatterUID, systemPrompt)
	a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterSystemPrompt, UserID: a.ownerUserID})

	// 存储原始用户消息 - buildUserMessage 处理多图像
	// 展平 + 发送者元数据。群组消息保留其“[SenderName]：”
	// 前缀（在 buildUserMessage 中应用）； DM 保持裸露状态。
	userMsg := buildUserMessage(msg)
	anchor := a.beginTurnAnchor(sess, userMsg)

	toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))
	overheadMessages := a.buildRequestOverhead(systemPrompt, msg, chatterMem)
	sessionMsgs := sess.GetMessages()
	compactResult, err := CompactMessagesWithOptions(sessionMsgs, a.compactionOptions(CompactModeProactive, overheadMessages, toolDefs, sess.SessionKey()))
	if err != nil {
		slog.Warn("compaction error", "agent", a.name, "error", err)
	}
	if compactResult != nil && compactResult.Pruned {
		sess.ReplaceMessages(compactResult.Messages)
		sessionMsgs = compactResult.Messages
	}

	messages := compactionRequestMessages(sessionMsgs, overheadMessages)

	type toolCallSig struct {
		name string
		hash [32]byte
	}
	var lastSig toolCallSig
	consecutiveCount := 0
	totalToolCalls := 0

	// ReAct 循环 - 使用 Chat 进行工具迭代
	for i := 0; i < a.maxToolIterations; i++ {
		hcBefore := &HookContext{AgentName: a.name, Point: BeforeModelCall, Messages: messages, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID}
		a.hooks.Run(ctx, hcBefore)

		resp, updatedMessages, retried, err := a.callLLMWithEmergencyRetry(sess, overheadMessages, toolDefs, messages, toolDefs, emergencyRetried, func(request []provider.Message, tools []provider.Tool) (*provider.Response, error) {
			dumpLLMRequest(a.name, a.model, request, tools)
			return a.provider.Chat(ctx, request, tools, a.model, a.maxTokens, a.temperature)
		})
		if retried {
			emergencyRetried = true
			messages = updatedMessages
		}

		hcAfter := &HookContext{AgentName: a.name, Point: AfterModelCall, Messages: messages, Response: resp, Error: err, StartTime: hcBefore.StartTime, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID, GoalSessionKey: a.registry.GoalSessionKey()}
		a.hooks.Run(ctx, hcAfter)

		if err != nil {
			slog.Error("LLM chat failed", "agent", a.name, "error", err)
			return a.stringStream("Sorry, I encountered an error processing your request.")
		}
		a.meterTokens(ctx, sess.Key(), resp.Usage)
		a.maybeRecoverToolCalls(resp)

		if !resp.HasToolCalls() {
			// 最终响应 - 使用流媒体
			sr, err := a.provider.ChatStream(ctx, messages, toolDefs, a.model, a.maxTokens, a.temperature)
			if err != nil {
				slog.Error("LLM stream failed, falling back", "agent", a.name, "error", err)
				sess.Append(provider.Message{Role: "assistant", Content: resp.Content})
				a.runPostTurn(ctx, msg, append(messages, provider.Message{Role: "assistant", Content: resp.Content}), totalToolCalls, chatterMem, anchor)
				return a.stringStream(resp.Content)
			}

			// 在后台收集内容以进行会话存储。
			// 捕获入站消息 + 每回合状态 - goroutine
			// 在带有本地助手消息的阴影“msg”下方，以及
			// runPostTurn 需要入站（channel / chat_id / source）。
			inboundMsg := msg
			messagesAtTurnStart := messages
			capturedToolCalls := totalToolCalls
			capturedChatterMem := chatterMem
			outCh := make(chan provider.StreamChunk, 64)
			outReader := provider.NewStreamReader(outCh)
			go func() {
				defer close(outCh)
				var full strings.Builder
				var thinking, thinkingSig string
				var rawAssistant json.RawMessage
				var streamUsage provider.Usage
				for {
					chunk, ok := sr.Next()
					if !ok {
						break
					}
					if chunk.Content != "" {
						full.WriteString(chunk.Content)
					}
					if chunk.Thinking != "" {
						thinking = chunk.Thinking
					}
					if chunk.ThinkingSignature != "" {
						thinkingSig = chunk.ThinkingSignature
					}
					if len(chunk.RawAssistant) > 0 {
						rawAssistant = chunk.RawAssistant
					}
					if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 ||
						chunk.Usage.CacheReadTokens > 0 || chunk.Usage.CacheCreationTokens > 0 {
						streamUsage = chunk.Usage
					}
					select {
					case outCh <- chunk:
					case <-ctx.Done():
						return
					}
				}
				a.meterTokens(ctx, sess.Key(), streamUsage)
				msg := provider.Message{Role: "assistant", Content: full.String(), Thinking: thinking}
				switch {
				case len(rawAssistant) > 0:
					// 提供者已经序列化了助手消息
					// 以其有线格式（例如 OpenAI/DeepSeek 与
					// 推理内容）。坚持逐字逐句，以便下一步
					// 转以相同的字节方式重放它 - 需要
					// DeepSeek思维模式。
					msg.RawAssistant = rawAssistant
				case thinking != "":
					// 人择扩展思维：pack {思维，签名}
					// 作为内容块，以便下一轮可以回显它。
					if raw, err := json.Marshal(map[string]string{
						"type":      "thinking",
						"thinking":  thinking,
						"signature": thinkingSig,
					}); err == nil {
						msg.RawAssistant = raw
					}
				}
				sess.Append(msg)
				// 现在助理消息是 Fire PostTurn
				// 坚持下来了。自动持久 (memory.go) 落后
				// 转弯后运行；没有这个调用流路径
				// 默默地跳过它 - 请参阅 runPostTurn 中的 FIXME。
				a.runPostTurn(ctx, inboundMsg, append(messagesAtTurnStart, msg), capturedToolCalls, capturedChatterMem, anchor)
			}()
			return outReader
		}

		// 工具调用 - 通过 SDK 引擎并发处理
		assistantMsg := provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			Timestamp:    time.Now().UnixMilli(),
			RawAssistant: resp.RawAssistant,
		}
		sess.Append(assistantMsg)
		messages = append(messages, assistantMsg)

		// 环路检测
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			sig := toolCallSig{
				name: tc.Function.Name,
				hash: sha256.Sum256([]byte(tc.Function.Arguments)),
			}
			if sig.name == lastSig.name && sig.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = sig
			}
			if consecutiveCount >= 3 {
				slog.Warn("tool loop detected", "agent", a.name, "tool", tc.Function.Name)
				warnMsg := provider.Message{
					Role:    "system",
					Content: "Loop detected: you called the same tool with the same arguments 3 times. Please try a different approach.",
				}
				sess.Append(warnMsg)
				messages = append(messages, warnMsg)
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// 触发 BeforeToolCall 挂钩
		for _, tc := range resp.ToolCalls {
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: BeforeToolCall, ToolName: tc.Function.Name, ToolArgs: tc.Function.Arguments, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID})
		}

		// 通过SDK引擎同时执行工具
		results := a.engine.executeToolsConcurrently(ctx, a.registry, resp.ToolCalls, a.workspacePath)
		totalToolCalls += len(results)

		for idx, r := range results {
			tc := resp.ToolCalls[idx]
			resultContent, meta := extractToolMeta(r.result)
			a.hooks.Run(ctx, &HookContext{AgentName: a.name, Point: AfterToolCall, ToolName: r.toolName, ToolResult: resultContent, Error: r.err, Channel: msg.Channel, AccountID: msg.AccountID, ChatID: msg.ChatID, UserID: a.ownerUserID, GoalSessionKey: a.registry.GoalSessionKey(), IsPlanMode: isPlanMode(msg.Params), Source: msg.Source})

			if r.err != nil {
				slog.Warn("tool execution error", "agent", a.name, "name", r.toolName, "error", r.err)
			}

			if mediaPaths := extractMediaPaths(resultContent); len(mediaPaths) > 0 {
				a.sendMediaFiles(msg, mediaPaths)
			}

			toolMsg := provider.Message{Role: "tool", Content: resultContent, ToolCallID: tc.ID, Name: r.toolName, Metadata: meta}
			sess.Append(toolMsg)
			messages = append(messages, toolMsg)
		}
	}

	slog.Warn("max tool iterations reached — streaming forced final delivery", "agent", a.name, "max", a.maxToolIterations)
	return a.streamFinalDeliveryAfterCap(ctx, msg, messages, sess, totalToolCalls, chatterMem, anchor)
}

// StreamFinalDeliveryAfterCap 使用工具运行一个额外的 ChatStream
// 禁用并合成轻推，然后保留助理消息
// 使用迭代上限元数据，以便聊天 UI 可以标记气泡。
// 返回的 StreamReader 与正常的”最终
// 上面的响应”分支，因此调用者不需要特殊情况。
func (a *Agent) streamFinalDeliveryAfterCap(ctx context.Context, inboundMsg bus.InboundMessage, messages []provider.Message, sess *session.Session, toolCallCount int, chatterMem *Memory, anchor *turnAnchor) *provider.StreamReader {
	capMeta := iterationCapMetadata(a.maxToolIterations)
	finalMessages := append(messages, capReachedNudge(a.maxToolIterations))
	sr, err := a.provider.ChatStream(ctx, finalMessages, nil, a.model, a.maxTokens, a.temperature)
	if err != nil {
		// 流端点失败 - 持久+发出后备线
		// 带有徽章，以便用户仍然收到信号。
		fallback := fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
		fallbackMsg := provider.Message{Role: "assistant", Content: fallback, Metadata: capMeta, Timestamp: time.Now().UnixMilli()}
		sess.Append(fallbackMsg)
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{"content": fallback, "metadata": capMeta}})
		a.runPostTurn(ctx, inboundMsg, append(messages, fallbackMsg), toolCallCount, chatterMem, anchor)
		return a.stringStream(fallback)
	}

	outCh := make(chan provider.StreamChunk, 64)
	outReader := provider.NewStreamReader(outCh)
	go func() {
		defer close(outCh)
		var full strings.Builder
		var thinking, thinkingSig string
		var rawAssistant json.RawMessage
		var streamUsage provider.Usage
		for {
			chunk, ok := sr.Next()
			if !ok {
				break
			}
			if chunk.Content != "" {
				full.WriteString(chunk.Content)
			}
			if chunk.Thinking != "" {
				thinking = chunk.Thinking
			}
			if chunk.ThinkingSignature != "" {
				thinkingSig = chunk.ThinkingSignature
			}
			if len(chunk.RawAssistant) > 0 {
				rawAssistant = chunk.RawAssistant
			}
			if chunk.Usage.InputTokens > 0 || chunk.Usage.OutputTokens > 0 ||
				chunk.Usage.CacheReadTokens > 0 || chunk.Usage.CacheCreationTokens > 0 {
				streamUsage = chunk.Usage
			}
			select {
			case outCh <- chunk:
			case <-ctx.Done():
				return
			}
		}
		a.meterTokens(ctx, sess.Key(), streamUsage)
		content := full.String()
		if content == "" {
			content = fmt.Sprintf("I've reached the maximum number of tool iterations (%d) and couldn't synthesize a final response. The work above represents what I gathered before hitting the limit.", a.maxToolIterations)
		}
		finalMsg := provider.Message{
			Role:      "assistant",
			Content:   content,
			Thinking:  thinking,
			Metadata:  capMeta,
			Timestamp: time.Now().UnixMilli(),
		}
		switch {
		case len(rawAssistant) > 0:
			finalMsg.RawAssistant = rawAssistant
		case thinking != "":
			if raw, err := json.Marshal(map[string]string{
				"type":      "thinking",
				"thinking":  thinking,
				"signature": thinkingSig,
			}); err == nil {
				finalMsg.RawAssistant = raw
			}
		}
		sess.Append(finalMsg)
		// 带外内容事件，因此 SSE 订阅者 + chat_events
		// 存档带有已达到上限的标志 - 块本身没有
		// 有一个元数据字段，所以我们在这里发布一次。
		emitEvent(ctx, ChatEvent{Type: "content", Data: map[string]any{
			"content":  "",
			"metadata": capMeta,
		}})
		// Fire PostTurn so AutoPersist（以及任何未来的 PostTurn 挂钩）
		// 也在流路径上运行 - 请参阅 no-tool-calls
		// HandleMessageStream 中的分支以了解其基本原理。
		a.runPostTurn(ctx, inboundMsg, append(messages, finalMsg), toolCallCount, chatterMem, anchor)
	}()
	return outReader
}

// extractToolMeta 从工具结果中去除 FC_META 前缀（如果存在），并
// 返回剩余内容加上解析的元数据。今天唯一
// signal 是 exec 是否在沙箱中运行。让助手保持共享
// 工具-结果切换路径向前端发出相同的形状。
func extractToolMeta(result string) (string, map[string]any) {
	if strings.HasPrefix(result, tools.MetaSandboxPrefix) {
		return strings.TrimPrefix(result, tools.MetaSandboxPrefix), map[string]any{"sandbox": true}
	}
	return result, nil
}

// capReachedNudge 是我们在强制之前附加的系统消息
// 最后交付回合。阐明两件事：(a) 工具被禁用
// 对于这个调用，所以不要尝试，(b) 提供结构化输出
// 用户从已经收集到的内容中提出要求，标记空白
// 明确地而不是跳过字段。模型一般是
// 把全部预算都花在勘探上，再也没有回头
// 到综合——明确地暴露约束是最便宜的
// 产生可用工件的推动。
func capReachedNudge(maxIterations int) provider.Message {
	return provider.Message{
		Role: "system",
		Content: fmt.Sprintf(
			"You've used all %d tool-call iterations available for this turn. Tools are now disabled for this final response — do not attempt to call any. Synthesize what you've already gathered into the most complete deliverable you can: if the user asked for a structured artifact (table, list, ICP summary, email drafts, etc.), produce it now from the existing tool results. For any fields you couldn't resolve, mark them as 'unknown' / 'not found' / 'partial' rather than dropping rows or skipping the structure — give the user something usable plus an honest note about what's missing. Do not apologize without delivering content.",
			maxIterations,
		),
	}
}

// iterationCapMetadata是标记在迭代器上的助手端元数据
// 强制最终传递消息，以便 UI 可以标记气泡。保留
// 作为构造函数，因此键名称在流中保持规范
// 和非流路径。
func iterationCapMetadata(maxIterations int) map[string]any {
	return map[string]any{
		"iterationCapReached": true,
		"iterationCapValue":   maxIterations,
	}
}

// stringStream 创建一个产生单个字符串的 StreamReader。
func (a *Agent) stringStream(text string) *provider.StreamReader {
	ch := make(chan provider.StreamChunk, 2)
	go func() {
		ch <- provider.StreamChunk{Content: text, Done: true}
		close(ch)
	}()
	return provider.NewStreamReader(ch)
}

// HomePath 返回代理的主目录（身份/元数据）。
func (a *Agent) HomePath() string {
	return a.homePath
}

// SplitReplies 返回有效的每个代理分割回复设置
// — 构造 OutboundMessage 时由网关使用，因此微信
// 适配器知道是否遵守 SplitMessageMarker。人口居住于
// 代理从合并的配置启动（每个代理覆盖其他系统
// WeChatCfg.SplitReplies);在 UpdateConfig 上刷新。
func (a *Agent) SplitReplies() bool {
	return a.splitReplies
}

// RegisteredTools 返回实时工具注册表投影 — 名称 +
// 描述 + 来源 — 用于仪表板的“工具”选项卡。反映了什么
// 该代理当前已加载：始终内置，以及任何 MCP 或
// 启动/热重载时附加的插件工具。订单稳定（内置
// 首先，然后是 MCP，然后是插件，按每个组中的名称排序）。
//
// 返回完整的注册表。基于模式的过滤发生在客户端
// 在仪表板中，以便操作员可以看到“什么是活动的
// 聊天机器人模式”而无需提交。
func (a *Agent) RegisteredTools() []tools.ToolInfo {
	if a.registry == nil {
		return nil
	}
	return a.registry.RegisteredTools()
}

// chatbotBuiltinAllowlist 是一组公开的精选内置工具
// 以聊天机器人模式进入法学硕士。被选为 IM 原生伴侣/客户 -
// 支持/角色扮演产品：
//
// - image_gen ：自行生成的图像（仅当
// 提供者已配置；缺席也没关系）
// - tts：语音消息（相同条件注册）
// - write_file ：LLM学习时保留USER.md / MEMORY.md
// 值得保留的东西。路由输入
// systemFileUserID 将 USER.md/MEMORY.md 发送到
// 每个聊天者行，因此每个聊天者都会积累他们的
// 自己的个人资料/记忆。路径解析拒绝
// 通过 IdentityFileBlocked + 任意路径
// 工作空间范围，所以这不是一般情况
// “让聊天机器人在任何地方写”漏洞——只是
// 规范的每条聊天记录。
// - edit_file ：相同的原理；优先于 write_file 时
// 通过外科手术更新 MEMORY.md 模型
// 不会意外破坏之前的条目。
//
// 值得注意的是：“read_file”/“list_dir”不存在——聊天机器人模式不应该
// 浏览文件系统； USER.md / MEMORY.md 内容已加载
// 通过引导程序进入系统提示符，因此读取工具将
// 只允许刺探聊天者不应该看到的东西。应用补丁
// 也已经过时了（多文件批处理是代理模式的领域）。
//
// 同样值得注意的是：“memory_search”。它扫描
// <workspace>/memory/logs/*.jsonl，聊天机器人模式从不写入 —
// 因此该工具总是返回“未找到匹配的条目”并且
// 模型将其解读为“我对你没有记忆”，从而覆盖了
// 它应该信任的提示 MEMORY.md 部分。删除它
// 强制模型依赖 USER.md / MEMORY.md 部分
// 渲染成系统提示符，这是唯一的持久化
// 聊天机器人模式实际上暴露了路径。
//
// 值得注意的是“消息”工具的缺失。主要回复通过以下方式发出
// LLM 的正常“内容”通道（网关的任务回调变成
// 自动进入 OutboundMessage）和多气泡输出
// 使用 SplitMessageMarker 内联，而不是工具调用。发布“消息”
// 进入聊天机器人模式会诱使法学硕士进入代理风格“我会发送一个
// 首先是“思考...”消息，然后是我真正的回复”的模式
// 与配套产品不和谐。需要 OOB 消息传递的操作员
// （cron 触发的问候语、多接收者广播）应该下降
// 返回“代理”模式或编写插件。
//
// 还缺席：exec、web_fetch / web_search、调度、委托
// — 不属于聊天角色的所有代理循环机制
// 嗓音。仅当新的内置函数普遍有用时才在此处添加它们
// 对于聊天机器人产品；其他一切都属于插件。
var chatbotBuiltinAllowlist = []string{
	"image_gen",
	"tts",
	"write_file",
	"edit_file",
	"retrieve_compacted_tool_result",
	// set_timezone 保持“他们的当地时间”适合聊天（问候语，
	// "晚安" timing) — chatbots need it as much as full agents do.
	"set_timezone",
}

// builtinAllowForMode 返回内置工具名称白名单
// 给定提示模式。无论如何，插件/MCP 工具始终包含在内
// — 请参阅Registry.DefinitionsForMode。 nil 表示“所有内置函数”；
// []string{} 表示“无内置函数”；非空切片意味着“只有这些”。
func builtinAllowForMode(mode string) []string {
	switch mode {
	case config.PromptModeChatbot:
		return chatbotBuiltinAllowlist
	case config.PromptModeCustomize:
		return []string{} // explicit empty — no built-ins
	default: // agent (or empty/unknown — defaults to agent for back-compat)
		return nil // nil = all built-ins exposed
	}
}

// WorkspacePath 返回面向用户的文件的代理工作目录。
func (a *Agent) WorkspacePath() string {
	return a.workspacePath
}

// chatterLocation 通过以下方式解析聊天的有效时区
// 范围首选项（chatter 首选项→代理默认→系统默认）。服务器-
// 当没有连接关系存储或未配置任何内容时为本地 -
// 遗留的单租户行为。传递给 ContextBuilder 作为
// tzResolver 以便系统提示符的日期行呈现在
// 喋喋不休的挂钟； cron 工具运行相同的分辨率
// 创造就业机会的时间。
func (a *Agent) chatterLocation(chatterUID string) *time.Location {
	if a.dataStore == nil {
		return time.Local
	}
	tz := scope.Timezone(context.Background(), a.dataStore, chatterUID, a.agentID)
	return scope.LoadLocationOrLocal(tz)
}

// UpdateConfig 更新代理的运行时配置（型号、温度等）
func (a *Agent) UpdateConfig(rc config.ResolvedAgent) {
	a.model = rc.Model
	a.maxTokens = rc.MaxTokens
	a.providerConfigs = cloneProviderConfigs(rc.Providers)
	a.contextWindow = rc.ContextWindow
	if a.contextWindow <= 0 {
		a.contextWindow = config.ResolveContextWindow(a.providerConfigs, a.model, a.maxTokens)
	}
	a.temperature = rc.Temperature
	a.maxToolIterations = rc.MaxToolIterations
	a.maxParallelToolCalls = rc.MaxParallelToolCalls
	// 沙箱标志驱动系统提示符的“工作目录”/“主目录”
	// dir”描述和沙箱功能块。没有这个
	// 启用沙箱之前存在的传播代理将保留
	// 告诉LLM它的家是主机绝对路径，即使在
	// 执行器本身已更换为 Docker — 模型尽职尽责地调用
	// list_dir /Users/idoubi/.bkclaw/agents/<id>/agent 和 404s
	// 容器。
	a.ctxBuilder.sandboxEnabled = rc.Sandbox.Enabled
	a.ctxBuilder.sandboxBackend = rc.Sandbox.Backend
	// 从仪表板保存传播每个代理提示模式更新。
	// 如果没有这个，操作员将代理切换到聊天机器人模式
	// UI 必须重新启动二进制文件才能使更改生效
	// 影响。工具过滤器自动遵循提示模式
	// 在请求时内置AllowForMode，因此不需要单独的热重载
	// 工具表面需要钩子。
	a.promptMode = rc.PromptMode
	a.ctxBuilder.SetPromptMode(rc.PromptMode)
	// 每个客服微信分批回复。无覆盖 = 保留任何内容
	// 系统层在启动时初始化（不要重置为 false）。非零
	// = 该代理的权威。
	if rc.SplitReplies != nil {
		a.splitReplies = *rc.SplitReplies
	}
}

// chatterUserID 选择每条消息的聊天身份，回退
// 当入站消息不携带代理所有者时
// （遗留通道、系统注入事件……）。这就是我们使用的
// 作为每用户技能存储桶密钥和沙箱绑定安装目标，
// 所以同一个代理的两个不同的聊天者各自看到自己的
// 个人技能设置并将安装写入自己的主机目录中。
func (a *Agent) chatterUserID(msg bus.InboundMessage) string {
	if msg.UserID != "" {
		return msg.UserID
	}
	return a.ownerUserID
}

// freshSkillsFromStore 镜像 OSS 托管的技能（全局、每个代理、
// 和每个用户）到本地文件系统并重建技能摘要
// 烘焙到系统提示符中。当没有工作区存储时，无操作
// 配置。在每个回合的顶部调用，以便上传技能
// Pod 启动后（或在同级副本上）在此处可见
// 下一条消息，而不需要重新启动 Pod。
//
// userID 标识要合并到集合中的每个用户技能桶；
// 传递聊天者（而不是代理所有者），以便安装技能聊天者 A
// 即使聊天者 A 与同一个客服人员聊天，该信息也仅对聊天者 A 可见。空的
// 禁用每用户层。
func (a *Agent) refreshSkillsFromStore(userID string) {
	if a.workspaceStore == nil {
		// IM-vs-web“缺少代理技能”诊断：何时触发
		// 在 IM 轮次上，但不在相同的匹配网络轮次上
		// 代理，聊天者的用户空间是在没有工作空间的情况下构建的
		// 存储，因此代理范围的 OSS 技能永远不会水合。警告（不
		// debug），因此它会出现在默认级别的生产日志中。
		slog.Warn("refresh skills skipped: no workspace store",
			"agent", a.name, "agentID", a.agentID, "user", userID)
		return
	}
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.homePath, "", a.skillsCfg, a.globalSkillsCfg).
		WithObjectStore(a.workspaceStore, a.agentID).
		WithUserID(userID)
	skills := loader.LoadSkills()
	summary := loader.BuildSkillsSummary(skills)
	a.ctxBuilder.SetSkillsSummary(summary)
	tools.RegisterLoadSkill(a.registry, loader.AllSkillDirs())
	// 系统提示会显示技能组的每回合指纹
	// 船。让我们比较 IM 与 Web 的相同点（座席、聊天）和
	// 确认——或排除——座席范围的技能正在达到
	// 每个频道。 count==bundled-only 是“缺少代理技能”
	// 签名。
	names := make([]string, 0, len(skills))
	for _, s := range skills {
		names = append(names, s.Name)
	}
	slog.Info("skills summary refreshed",
		"agent", a.name, "agentID", a.agentID, "user", userID,
		"count", len(skills), "summary_bytes", len(summary), "names", names)
}

// ReloadWorkspaceFiles 重新读取工作区 .md 文件（SOUL.md、AGENTS.md 等）
// 并重建上下文构建器。
func (a *Agent) ReloadWorkspaceFiles() {
	if a.memoryStore != nil {
		a.memory = NewMemoryWithStoreForUser(a.homePath, a.memoryStore, a.ownerUserID, a.name)
	} else {
		a.memory = NewMemory(a.homePath)
	}
	// 重建技能总结。配置工作区存储后，
	// LoadSkills 首先水合全局+每个代理+每个用户技能目录
	// 来自对象存储，因此技能上传到另一个副本（或
	// 启动后）变得可见。
	loader := NewSkillsLoaderWithGlobal(a.homeDir, a.homePath, "", a.skillsCfg, a.globalSkillsCfg).
		WithUserID(a.ownerUserID)
	if a.workspaceStore != nil {
		loader.WithObjectStore(a.workspaceStore, a.agentID)
	}
	skills := loader.LoadSkills()
	skillsSummary := loader.BuildSkillsSummary(skills)
	tools.RegisterLoadSkill(a.registry, loader.AllSkillDirs())
	a.ctxBuilder = NewContextBuilder(a.homePath, a.memory, skillsSummary)
	a.ctxBuilder.SetWorkspace(a.workspacePath)
	a.ctxBuilder.SetPromptMode(a.promptMode)
	a.ctxBuilder.SetDisplayName(a.displayName)
	// 在重新加载时保留存储支持的身份读取；没有这个，
	// Postgres 模式 pod 会默默地回退到 pod 本地文件系统。
	// 用户ID也必须重新固定——数据库存储需要一个非空的
	// user_id 来限制 SOL/IDENTITY/AGENTS 读取的范围，并且没有它
	// ContextBuilder 的 loadFile 传递在每个共享上都会失败
	// 重新加载后的身份文件（表现为“没有代理”
	// 名字/灵魂”问候语）。
	if a.memoryStore != nil {
		a.ctxBuilder.store = a.memoryStore
		a.ctxBuilder.agentID = a.name
		a.ctxBuilder.userID = a.ownerUserID
	}
	// Chatter-时区日期线 — 与商店相同的重新应用规则
	// 上面的接线：重建的 ContextBuilder 以 nil 开头
	// 解析器并会默默地回退到服务器本地时间。
	if a.dataStore != nil {
		a.ctxBuilder.SetTimezoneResolver(a.chatterLocation)
	}
}

// extractMediaPaths 扫描工具输出中的 MEDIA: 行并返回文件路径。
// MEDIA：OpenClaw 技能使用协议将文件附加到聊天消息。
func extractMediaPaths(output string) []string {
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MEDIA:") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "MEDIA:"))
			if path != "" {
				if _, err := os.Stat(path); err == nil {
					paths = append(paths, path)
				}
			}
		}
	}
	return paths
}

// sendMediaFiles 将提取的 MEDIA: 文件发送到出站总线。
func (a *Agent) sendMediaFiles(msg bus.InboundMessage, mediaPaths []string) {
	if len(mediaPaths) == 0 || a.messageBus == nil {
		return
	}
	outMsg := bus.OutboundMessage{
		Channel:    msg.Channel,
		AccountID:  msg.AccountID,
		ChatID:     msg.ChatID,
		MediaPaths: mediaPaths,
		AllowSplit: a.splitReplies,
	}
	select {
	case a.messageBus.Outbound <- outMsg:
	default:
		slog.Warn("outbound channel full, dropping media message", "agent", a.name)
	}
}
