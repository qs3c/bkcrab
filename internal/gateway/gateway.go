// Package gateway 是运行时编排器。它打开存储、托管每个用户的 UserSpace（首次认证时延迟加载），
// 并启动通道管理器/cron 调度器/webhook 服务器/插件管理器。
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/channels"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/cron"
	mcpruntime "github.com/qs3c/bkcrab/internal/mcp/runtime"
	"github.com/qs3c/bkcrab/internal/plugin"
	"github.com/qs3c/bkcrab/internal/sandbox"
	"github.com/qs3c/bkcrab/internal/scope"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/taskqueue"
	"github.com/qs3c/bkcrab/internal/toolproviders"
	"github.com/qs3c/bkcrab/internal/toolproviders/imagegen"
	"github.com/qs3c/bkcrab/internal/toolproviders/tts"
	"github.com/qs3c/bkcrab/internal/toolproviders/webfetch"
	"github.com/qs3c/bkcrab/internal/toolproviders/websearch"
	"github.com/qs3c/bkcrab/internal/usage"
	"github.com/qs3c/bkcrab/internal/users"
	"github.com/qs3c/bkcrab/internal/webhook"
	"github.com/qs3c/bkcrab/internal/workspace"
)

var toolProviderRegistry = func() *toolproviders.Registry {
	r := toolproviders.NewRegistry()
	websearch.RegisterAll(r)
	webfetch.RegisterAll(r)
	imagegen.RegisterAll(r)
	tts.RegisterAll(r)
	return r
}()

// ToolProviderRegistry 为想要列出可用提供者的调用者（管理 API）暴露注册表。
func ToolProviderRegistry() *toolproviders.Registry { return toolProviderRegistry }

// registerAgentToolChains 使用合并的配置视图（解析器叠加的系统+用户+代理作用域）将每个提供者支持的工具类别挂接到给定的代理上。
func registerAgentToolChains(cfg *config.Config, agents []*agent.Agent) {
	envSearxNG := strings.TrimSpace(os.Getenv("BKCRAB_SEARXNG_ENDPOINT"))
	for _, ag := range agents {
		resolved := cfg.MergedAgentConfig(config.AgentEntry{ID: ag.Name()})
		chain := buildToolChainFromResolved(resolved, "web_search")
		// 回退：如果没有配置 web_search 链且环境中设置了 BKCRAB_SEARXNG_ENDPOINT，
		// 合成一个指向该端点的单提供者链。一行设置（"docker run searxng …" + 环境变量）
		// 是一个可以在第一次尝试就找到正确 URL 的代理与一个耗费 11 轮猜测的代理之间的区别 —
		// 我们在实际环境中观察到了后者，让用户没有搜索的代价不值得注入合理默认值的代价。
		if chain == nil && envSearxNG != "" {
			chain = synthesizeSearxNGChain(envSearxNG)
		}
		if chain != nil {
			ag.RegisterWebSearchChain(chain)
		}
		if chain := buildToolChainFromResolved(resolved, "image_gen"); chain != nil {
			ag.RegisterImageGenChain(chain)
		}
		if chain := buildToolChainFromResolved(resolved, "tts"); chain != nil {
			ag.RegisterTTSChain(chain)
		}
		// web_fetch：链优先，否则代理保留构建时已注册的内置直接获取器（loop.go 中的 RegisterWebFetch），
		// 因此此调用仅在管理员实际配置了链时才交换后端。
		if chain := buildToolChainFromResolved(resolved, "web_fetch"); chain != nil {
			ag.RegisterWebFetchChain(chain)
		}
	}
}

// synthesizeSearxNGChain 构建一个仅由 SearxNG 提供者支持的临时 web_search 链，
// 从 BKCRAB_SEARXNG_ENDPOINT 配置。让新安装无需经过仪表盘的工具提供者配置即可启用搜索 —
// 用户在实际环境中从未看到 web_search 的最常见原因是他们没有意识到需要在两个地方
// （提供者条目 + 类别链）配置它。
func synthesizeSearxNGChain(endpoint string) *toolproviders.Chain {
	chain := &toolproviders.Chain{
		Category:     "web_search",
		Order:        []string{"searxng"},
		AutoFallback: false,
		Registry:     toolProviderRegistry,
		GetConfig: func(name string) toolproviders.ProviderConfig {
			if name != "searxng" {
				return toolproviders.ProviderConfig{}
			}
			return toolproviders.ProviderConfig{Endpoint: endpoint}
		},
	}
	if !chain.Available() {
		return nil
	}
	return chain
}

func buildToolChainFromResolved(resolved config.ResolvedAgent, category string) *toolproviders.Chain {
	cat, ok := resolved.Tools[category]
	if !ok {
		return nil
	}
	order := cat.Chain()
	if len(order) == 0 {
		return nil
	}
	providers := resolved.ToolProviders
	chain := &toolproviders.Chain{
		Category:     category,
		Order:        order,
		AutoFallback: cat.FallbackEnabled(),
		Registry:     toolProviderRegistry,
		GetConfig: func(name string) toolproviders.ProviderConfig {
			pc := providers[name]
			return toolproviders.ProviderConfig{
				APIKey:   pc.APIKey,
				Endpoint: pc.Endpoint,
				Options:  pc.Options,
			}
		},
	}
	if !chain.Available() {
		return nil
	}
	return chain
}

// Gateway 是运行时编排器。它不在启动时加载任何代理；UserSpace 在认证请求解析到其所有者时延迟构建。
//
// `sandboxPool` 是网关范围的执行器池。从系统作用域沙箱配置构建一次，由每个 UserSpace 共享。
// 每个 UserSpace 的 `SandboxPool` 字段只是一个借用的引用；关闭时只关闭这一个池。
type Gateway struct {
	bus         *bus.MessageBus
	users       *userSpaceRegistry
	chanMgr     *channels.Manager
	webChan     *channels.WebChannel
	scheduler   *cron.Scheduler
	webhookSrv  *webhook.Server
	pluginMgr   *plugin.Manager
	taskQueue   *taskqueue.Queue
	store       store.Store
	accounts    *users.Accounts
	workspace   workspace.Store
	sandboxPool sandbox.ExecutorPool
	mcpRuntime  *mcpruntime.Service
	usage       usage.Meter
	envCfg      *config.EnvConfig
	// chatEvents 设置后，允许总线触发的 web 轮次（cron/目标延续/心跳/子代理）
	// 通过用户输入的 POST /api/chat 轮次使用的同一个 SSE hub 流式传输。
	// 安全地为 nil：未设置时保留传统的 bus.Outbound → WebChannel 异步气泡路径。
	chatEvents *agent.EventHub
	mu         sync.RWMutex
	dedup      sync.Map
}

// SetChatEvents 挂接设置服务器延迟初始化的代理事件中心。必须在 Run() 之前调用，
// 以便第一个总线触发的 web 轮次通过中心流式传输，而不是作为一个延迟的异步气泡到达。安全地只调用一次。
func (g *Gateway) SetChatEvents(h *agent.EventHub) { g.chatEvents = h }

// WebChannel 返回 web SSE 订阅者的进程内扇出。由设置服务器用于注册聊天流订阅者，
// 以便 cron 触发的（和其他异步）出站消息到达活跃的仪表盘标签页。
func (g *Gateway) WebChannel() *channels.WebChannel { return g.webChan }

// Workspace 返回持久化工件存储。
func (g *Gateway) Workspace() workspace.Store { return g.workspace }

// Usage 返回每个租户的资源计量器。
func (g *Gateway) Usage() usage.Meter { return g.usage }

// Store 返回网关的存储后端。
func (g *Gateway) Store() store.Store { return g.store }

// TaskQueue 返回网关的任务队列。
func (g *Gateway) TaskQueue() *taskqueue.Queue { return g.taskQueue }

// MCPRuntime returns the gateway-backed MCP runtime service.
func (g *Gateway) MCPRuntime() *mcpruntime.Service { return g.mcpRuntime }

// EnvConfig 返回引导配置（BKCRAB_* 环境变量）。
func (g *Gateway) EnvConfig() *config.EnvConfig { return g.envCfg }

// New 创建一个 Gateway。存储 + 工作空间 + 插件管理器 + 通道管理器 + cron 调度器 + webhook 都在此初始化，
// 但在认证请求到达用户之前不会加载任何代理。
func New(env *config.EnvConfig) (*Gateway, error) {
	if env == nil {
		env = &config.EnvConfig{}
	}
	mb := bus.New()

	homeDir, _ := config.HomeDir()
	st, err := store.New(&store.StorageConfig{
		Type:        store.StorageType(env.Storage.Type),
		DSN:         env.Storage.DSN,
		AutoMigrate: env.Storage.AutoMigrate,
	}, homeDir)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	// 挂接第 3 层代理配置（每个代理的覆盖）以从数据库读取。
	config.AgentFileConfigLoader = makeStoreFirstAgentFileLoader(st)

	// 代理产生的工作成果的对象存储。对象存储配置位于 system_settings 中用于运行时编辑的字段，
	// 以及 BKCRAB_OBJECT_STORE_* 环境变量用于运维管理的覆盖。
	osCfg := readObjectStoreCfg(st)
	wsInner, err := workspace.Factory{
		Type:         osCfg.Type,
		LocalDir:     osCfg.Local.Root,
		AccountID:    osCfg.AccountID,
		AliyunIntern: osCfg.AliyunIntern,
		S3: workspace.S3Config{
			Endpoint:  osCfg.S3.Endpoint,
			Region:    osCfg.S3.Region,
			Bucket:    osCfg.S3.Bucket,
			Prefix:    osCfg.S3.Prefix,
			AccessKey: osCfg.S3.AccessKey,
			SecretKey: osCfg.S3.SecretKey,
			UseSSL:    osCfg.S3.UseSSL,
		},
	}.New(filepath.Join(homeDir, "workspaces"))
	if err != nil {
		return nil, fmt.Errorf("open object store: %w", err)
	}
	slog.Info("object store", "type", defaultStr(osCfg.Type, "local"))

	// LLM 令牌计量：SQLMeter UPSERT 到 Store 打开的同一个数据库的 token_usage_daily 表中，
	// 这样管理报告在重启后仍然存在。如果存储不暴露 *sql.DB（在实际安装中不应该发生 —
	// 只有嵌入式测试替身会这样），则回退到 MemMeter。
	var meter usage.Meter = usage.NewMemMeter()
	if dbs, ok := st.(*store.DBStore); ok {
		meter = usage.NewSQLMeter(dbs.DB(), dbs.Dialect())
	}
	ws := wsInner

	// holderID 是印记在 channel_leases.holder_id 中的每个进程标识符。
	// 在此网关的生命周期内保持稳定，以便续租保持与行匹配；对等进程生成自己的 holderID，
	// 只能在我们过期后窃取租约。在启动时记录，以便运维可以将"谁当前正在驱动此微信 bot"与特定副本关联起来。
	holderID := uuid.NewString()
	slog.Info("gateway holder id", "id", holderID)
	chanMgr := channels.NewManagerWithLeaser(mb, storeLeaser{st: st}, holderID)
	// 常开的 web 通道：将 cron 触发的（和任何其他异步发出的）出站消息路由到仪表盘的 SSE 订阅者，
	// 以便用户实时看到代理的回复，而不是仅在下次页面重新加载时。
	webChan := channels.NewWebChannel()
	chanMgr.Register(webChan)

	// Cron 调度器每次时钟滴答时直接从数据库读取任务 — 没有内存中的任务列表，
	// 没有 bkcrab.json 副本。每个触发的任务携带其 OwnerUserID，
	// 以便 processInbound 可以路由到正确的空间。
	scheduler := cron.NewSchedulerFromStore(&cronStoreAdapter{st: st}, mb)
	// 预检投递检查：当配置的目标通道适配器未注册时（例如微信令牌过期且行被清除），
	// 调度器递增 failure_count，而不是向虚空触发；连续错过太多时钟滴答的行会被自动删除。
	scheduler.SetChannelChecker(chanMgr)

	systemHooks := readSystemHooks(st)
	var webhookSrv *webhook.Server
	if systemHooks.Enabled {
		webhookSrv = webhook.NewServer(systemHooks.Token, systemHooks.Path, nil, nil)
	}

	var pluginMgr *plugin.Manager
	systemPlugins := readSystemPlugins(st)
	if systemPlugins.Enabled {
		pluginMgr = plugin.NewManager(mb)
		pluginPaths := []string{filepath.Join(homeDir, "plugins")}
		pluginPaths = append(pluginPaths, systemPlugins.Paths...)
		if err := pluginMgr.Discover(pluginPaths); err != nil {
			slog.Warn("plugin discovery error", "error", err)
		}
		if len(systemPlugins.Entries) > 0 {
			entries := make(map[string]plugin.PluginEntryCfg, len(systemPlugins.Entries))
			for k, v := range systemPlugins.Entries {
				entries[k] = plugin.PluginEntryCfg{Enabled: v.Enabled, Config: v.Config}
			}
			pluginMgr.ApplyConfig(entries)
		}
	}

	taskCfg := readSystemTaskQueue(st)
	maxConcurrent := taskCfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 10
	}
	taskTimeoutSec := taskCfg.TaskTimeoutSec
	if taskTimeoutSec <= 0 {
		taskTimeoutSec = 300
	}
	taskTimeout := time.Duration(taskTimeoutSec) * time.Second

	// 系统范围沙箱池。在启动时从系统作用域沙箱配置（合并环境变量）构建一次，
	// 并在每个 UserSpace 之间共享。延迟注入的代理（super_admin 聊天、app 模式 API 密钥调用者，
	// 其 `app_user` UserSpace 自身不拥有任何代理）需要这个 — 没有系统级池，
	// 每个用户的构建器为这些空间产生 nil，代理的 exec 工具拒绝运行并显示
	// "sandbox required but no executor available"。
	systemSandboxPool := buildSystemSandboxPool(readSystemSandboxCfg(st), ws)
	mcpRuntime := mcpruntime.NewService(mcpruntime.Options{
		Store:  st,
		Docker: mcpruntime.NewDockerCLIClient(),
		Config: mcpruntime.FromEnv(env.MCPGateway),
	})

	// Accounts 服务由入站路由循环用于延迟铸造每个（通道，IM 发送者）的 app_user 行，
	// 以便 IM 通道上的每个聊天者最终拥有自己稳定的 bkcrab u_xxx id
	//（从而拥有自己的每个聊天者的 USER.md / MEMORY.md）。
	accts, err := users.NewAccounts(st)
	if err != nil {
		return nil, fmt.Errorf("init accounts: %w", err)
	}

	g := &Gateway{
		bus:         mb,
		store:       st,
		accounts:    accts,
		workspace:   ws,
		usage:       meter,
		sandboxPool: systemSandboxPool,
		mcpRuntime:  mcpRuntime,
		users:       newUserSpaceRegistry(mb, st, ws, meter, systemSandboxPool, mcpRuntime, pluginMgr),
		chanMgr:     chanMgr,
		webChan:     webChan,
		scheduler:   scheduler,
		webhookSrv:  webhookSrv,
		pluginMgr:   pluginMgr,
		envCfg:      env,
	}

	if webhookSrv != nil {
		webhookSrv.SetHandler(&webhookAgentHandler{gateway: g})
	}

	tq := taskqueue.NewQueue(maxConcurrent, taskTimeout, func(ctx context.Context, task *taskqueue.Task) (string, error) {
		space, err := g.users.getOrLoad(ctx, task.OwnerUserID)
		if err != nil {
			return "", fmt.Errorf("load user space: %w", err)
		}
		ag := space.Agents.AgentByID(task.AgentID)
		if ag == nil {
			return "", fmt.Errorf("agent %q not found", task.AgentID)
		}
		chanMgr.SendTyping(task.Message.Channel, task.AccountID, task.Message.ChatID)
		typingDone := make(chan struct{})
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-typingDone:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					chanMgr.SendTyping(task.Message.Channel, task.AccountID, task.Message.ChatID)
				}
			}
		}()

		// IM 通道只显示最终回复 — 没有每个工具调用的进度消息。用户在运行期间看到输入指示器（如上）；
		// 中间的"calling X…"行在多工具轮次中增加了太多噪音。Web UI 通过 HandleWebChatStream
		// 直接订阅聊天事件，不受影响。

		// 记录轮次开始。在 ag.HandleMessage 返回后，我们列出工作空间并附加每个 ModTime >= turnStart 的图片 —
		// 无论 LLM 的回复是否包含可用的 markdown 引用都有效。基于时间的比路径差异更健壮
		//（轮次前快照时序、不保留路径稳定性的存储后端、原地覆盖的文件等）。
		turnStart := time.Now()

		// 为 web 通道总线触发的轮次附加流管道，以便事件到达用户输入路径使用的同一个 SSE hub。
		// 当中心未挂接（例如 CLI/测试工具）或会话无法解析时为空操作 —
		// 下面的 OutboundMessage 推送仍然会传递最终回复。
		webStreamed := false
		if g.chatEvents != nil && task.Message.Channel == "web" {
			if sess := ag.Sessions().Get(task.Message.Channel, task.Message.AccountID, task.Message.ChatID, task.Message.ProjectID); sess != nil {
				ctx = agent.ContextWithStream(ctx, nil, g.store, g.chatEvents, task.OwnerUserID, task.AgentID, sess.SessionKey())
				webStreamed = true
			}
		}

		reply := ag.HandleMessage(ctx, task.Message)
		close(typingDone)
		// 从代理的回复中提取 `![alt](workspace/relative/path)` markdown 图片引用，
		// 通过 workspace.Store 解析它们的字节，并作为 MediaItems 发送，
		// 以便 IM 通道可以上传为照片。文本占位符从正文中剥离，
		// 这样用户看不到原始的 `![](...)` 语法。
		text, items := splitMediaFromReply(ctx, g.workspace, task.AgentID, task.Message.ProjectID, task.Message.ChatID, reply)
		// 工作空间回退：列出会话的文件并附加每个 mtime 在此轮次窗口内的图片。
		// 捕获 LLM 发出损坏的 data URL（截断的 base64 / 字面 "..." 占位符）
		// 但 image-tool 已将真实文件保存到 /workspace 的情况。
		// 按文件名去重，这样我们不会重复发送 splitMediaFromReply 已解决的任何内容。
		items = appendRecentWorkspaceMedia(ctx, g.workspace, task.AgentID, task.Message.ProjectID, task.Message.ChatID, turnStart, items)
		// Web 流式轮次已通过中心传递了回复。当没有媒体时完全跳过出站推送；
		// 有媒体时，推送空文本以便附件仍然流动，但聊天面板不会双重渲染文本。
		if webStreamed && len(items) == 0 {
			return reply, nil
		}
		outText := text
		if webStreamed {
			outText = ""
		}
		out := bus.OutboundMessage{
			Channel:      task.Message.Channel,
			AccountID:    task.AccountID,
			AgentID:      task.AgentID,
			ChatID:       task.Message.ChatID,
			Text:         outText,
			ReplyToMsgID: task.Message.MessageID,
			ParseMode:    "Markdown",
			MediaItems:   items,
			// AllowSplit 让微信适配器为多气泡输出遵循 SplitMessageMarker。
			// 来自原始代理的每个代理设置（或启动时内建的系统回退）— 见 Agent.SplitReplies。
			AllowSplit: ag.SplitReplies(),
		}
		// 有界入队。如果 routeOutbound 卡住了，任务不应该永远占用其 taskQueue 插槽 —
		// 让 ctx 的任务超时作为上限，丢弃回复，而不是阻塞来自此用户的下一个入站消息。
		select {
		case mb.Outbound <- out:
		case <-ctx.Done():
			slog.Warn("outbound enqueue cancelled", "agent", task.AgentID, "chat", task.Message.ChatID)
		}
		return reply, nil
	})
	g.taskQueue = tq

	// 从数据库注册所有已启用的通道行。
	if err := registerChannelsFromStore(st, mb, chanMgr); err != nil {
		slog.Warn("registerChannelsFromStore", "error", err)
	}

	return g, nil
}

// UserSpaceFor 返回已解析用户的 UserSpace，首次调用时延迟加载。没有隐式/本地用户 — userID 必须是真实的 users.id。
func (g *Gateway) UserSpaceFor(userID string) (*UserSpace, error) {
	return g.UserSpaceForCtx(context.Background(), userID)
}

// UserSpaceForCtx 是感知 ctx 的变体；HTTP 处理程序应优先使用它，以便底层数据库查询继承请求截止时间。
func (g *Gateway) UserSpaceForCtx(ctx context.Context, userID string) (*UserSpace, error) {
	if userID == "" {
		return nil, fmt.Errorf("UserSpaceFor: userID required")
	}
	return g.users.getOrLoad(ctx, userID)
}

// LocalAgentManager 满足 api.UserResolver 接口 — 但不再有"本地"固定的管理器。
// 合法需要任何代理管理器的调用者应解析请求自己的 user_id 并调用 UserSpaceFor。
func (g *Gateway) LocalAgentManager() *agent.Manager { return nil }

// EnsureAgent 将一个不属于 userID 的代理加载到该用户的 UserSpace 中。由 super_admin 聊天处理程序使用 — 见 UserSpace.EnsureAgent。
func (g *Gateway) EnsureAgent(ctx context.Context, userID, agentID string) error {
	sp, err := g.UserSpaceForCtx(ctx, userID)
	if err != nil {
		return err
	}
	return sp.EnsureAgent(ctx, g.store, g.bus, g.workspace, agentID)
}

// IsCloudMode 为少数仍在其上分支的调用者保留，但现在始终返回 true：多用户是无条件的。
func (g *Gateway) IsCloudMode() bool { return true }

// Run 启动网关并阻塞直到进程收到 SIGINT/SIGTERM。
// 在 Unix 上，SIGHUP 触发每个缓存 UserSpace 的热重载，以便下一个请求拾取 CLI 或其他对等体所做的存储变更。
func (g *Gateway) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopCh := make(chan os.Signal, 1)
	signal.Notify(stopCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-stopCh
		slog.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	reloadCh := make(chan os.Signal, 1)
	notifyReloadSignal(reloadCh)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reloadCh:
				slog.Info("received reload signal, reloading agents")
				if err := g.ReloadAgents(); err != nil {
					slog.Warn("agent reload failed", "error", err)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	if g.mcpRuntime != nil {
		g.mcpRuntime.Start(ctx)
	}
	wg.Add(1)
	go func() { defer wg.Done(); g.users.startEvictor(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); g.cleanupDedup(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); g.processInbound(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); g.chanMgr.Start(ctx) }()
	if g.scheduler != nil {
		wg.Add(1)
		go func() { defer wg.Done(); g.scheduler.Start(ctx) }()
	}
	if g.webhookSrv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port := readSystemHooks(g.store).Port
			if port == 0 {
				port = 18954
			}
			addr := fmt.Sprintf(":%d", port)
			if err := g.webhookSrv.ListenAndServe(ctx, addr); err != nil {
				slog.Error("webhook server error", "error", err)
			}
		}()
	}
	if g.pluginMgr != nil {
		if err := g.pluginMgr.StartAll(ctx); err != nil {
			slog.Error("plugin start error", "error", err)
		}
		for _, inst := range g.pluginMgr.ChannelPlugins() {
			adapter := plugin.NewChannelAdapter(g.pluginMgr, inst.Manifest.ID)
			g.chanMgr.Register(adapter)
		}
		plugin.RegisterPluginProviders(ctx, g.pluginMgr, toolProviderRegistry)
	}
	slog.Info("gateway started")
	wg.Wait()
	if g.taskQueue != nil {
		g.taskQueue.Stop()
	}
	if g.pluginMgr != nil {
		g.pluginMgr.StopAll()
	}
	if g.users != nil {
		g.users.closeAll()
	}
	if g.mcpRuntime != nil {
		_ = g.mcpRuntime.StopAll(context.Background())
	}
	if g.sandboxPool != nil {
		g.sandboxPool.CloseAll()
	}
	slog.Info("gateway stopped")
	return nil
}

// makeStoreFirstAgentFileLoader 返回一个从 agents.config 列读取每个代理配置的加载器。
func makeStoreFirstAgentFileLoader(st store.Store) func(string, string) (config.AgentFileConfig, bool) {
	return func(agentID, _ string) (config.AgentFileConfig, bool) {
		if st == nil || agentID == "" {
			return config.AgentFileConfig{}, false
		}
		// 我们现在需要 GetAgent 的 user_id；遍历每个用户代价很高。而是使用 ListAllAgents 并选择。
		all, err := st.ListAllAgents(context.Background())
		if err != nil {
			return config.AgentFileConfig{}, false
		}
		for _, ar := range all {
			if ar.ID != agentID {
				continue
			}
			if len(ar.Config) == 0 {
				return config.AgentFileConfig{}, false
			}
			blob, _ := json.Marshal(ar.Config)
			var cfg config.AgentFileConfig
			if err := json.Unmarshal(blob, &cfg); err == nil {
				return cfg, true
			}
		}
		return config.AgentFileConfig{}, false
	}
}

func defaultStr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// readObjectStoreCfg 拉取"objectstore"设置命名空间，然后在上面叠加 BKCRAB_OBJECT_STORE_* 环境变量。
func readObjectStoreCfg(st store.Store) config.ObjectStoreCfg {
	cfg := &config.Config{}
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSObjectStore, "", "", &cfg.ObjectStore)
	}
	config.LoadEnv().ApplyToConfig(cfg)
	return cfg.ObjectStore
}

func readSystemHooks(st store.Store) config.HooksCfg {
	var out config.HooksCfg
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSHooks, "", "", &out)
	}
	return out
}

func readSystemPlugins(st store.Store) config.PluginsCfg {
	var out config.PluginsCfg
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSPlugins, "", "", &out)
	}
	return out
}

func readSystemTaskQueue(st store.Store) config.TaskQueueCfg {
	var out config.TaskQueueCfg
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSTaskQueue, "", "", &out)
	}
	return out
}

// readSystemSandboxCfg 读取系统作用域沙箱设置，并在上面合并 BKCRAB_SANDBOX_* 环境变量。
// 网关范围沙箱池的事实来源。
func readSystemSandboxCfg(st store.Store) config.SandboxCfg {
	cfg := &config.Config{}
	if st != nil {
		_ = scope.SettingInto(context.Background(), st, NSSandbox, "", "", &cfg.Sandbox)
	}
	config.LoadEnv().ApplyToConfig(cfg)
	return cfg.Sandbox
}

// 设置命名空间常量。每个映射到 configs 中 kind="setting" 的一行。
// 添加新命名空间是一行追加；scope.Setting/SettingInto 辅助函数处理跨作用域的合并。
const (
	NSAgentDefaults  = "agents.defaults"
	NSSandbox        = "sandbox"
	NSObjectStore    = "objectstore"
	NSHooks          = "hooks"
	NSPlugins        = "plugins"
	NSTaskQueue      = "taskqueue"
	NSToolProviders  = "tools.providers"
	NSToolCategories = "tools.categories"
	NSSkillsInstall  = "skills.install"
	NSSkillsEntries  = "skills.entries"
	NSMemory         = "memory"
	NSPrivacy        = "privacy"
	NSSkillsLearner  = "skillsLearner"
	NSHeartbeat      = "heartbeat"
	NSTeams          = "teams"
	NSBindings       = "bindings"
)

// registerChannelsFromStore 从 configs 加载每个已启用的 kind="channel" 行，
// 并为每个行启动一个通道适配器，无论作用域如何。所有者按行捕获，
// 在消息接收时通过 LookupChannelByCredential 解析。
func registerChannelsFromStore(st store.Store, mb *bus.MessageBus, chanMgr *channels.Manager) error {
	if st == nil {
		return nil
	}
	rows, err := allChannelRows(st)
	if err != nil {
		return err
	}
	for _, r := range rows {
		if !r.Enabled {
			continue
		}
		if err := registerChannelInstance(r, mb, chanMgr, st, false); err != nil {
			slog.Warn("register channel failed",
				"type", r.Name, "user_id", r.UserID, "agent_id", r.AgentID, "error", err)
		}
	}
	return nil
}

// allChannelRows 返回每个通道行，无论所有权如何 —
// 系统行 ("", "") 加上每个用户、每个代理和每个（用户，代理）行。
// 引导路径需要并集，以便每个所有者的适配器被热启动；每行路由在消息接收时通过 LookupChannelByCredential 决定。
func allChannelRows(st store.Store) ([]store.ConfigRecord, error) {
	rows, err := st.QueryAllConfigs(context.Background(), store.KindChannel)
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// imgRefRegex 匹配 markdown 图片引用 `![alt](path)`。我们为 alt 和 path 都保留捕获组，
// 以便下面的辅助函数在构建 MediaItems 和从聊天正文剥离标记时可以重用它们。
var imgRefRegex = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// splitMediaFromReply 从 `reply` 中提取每个 `![alt](src)` 引用，
// 并将其转换为 IM 通道可以直接上传的 MediaItem：
//
//   - data:image/...;base64,… → 内联解码字节，从文本中剥离
//   - /workspace/foo 或 foo → 通过 workspace.Store 获取，从文本中剥离
//   - http:// 或 https:// → 保留在原位（某些 IM 自动嵌入 URL）
//
// 字节无法解析的引用仍然会从输出中**剥离**（否则 200KB base64 data URL
// 或损坏的 `![alt](missing)` 会作为原始文本出现在聊天中 —
// 导致了"telegram 转储 base64"报告）。当我们剥离时，alt 文本也被丢弃，
// 因为代理周围的文字通常可以独立存在。
//
// sessionID = msgChatID，因为网关每个会话路由一个聊天。
func splitMediaFromReply(ctx context.Context, ws workspace.Store, agentID, projectID, sessionID, reply string) (string, []bus.MediaItem) {
	if reply == "" {
		return reply, nil
	}
	matches := imgRefRegex.FindAllStringSubmatchIndex(reply, -1)
	if len(matches) == 0 {
		return reply, nil
	}
	var items []bus.MediaItem
	var out strings.Builder
	cursor := 0
	for _, m := range matches {
		path := reply[m[4]:m[5]]

		// 远程 URL：保持 markdown 引用完整，以便 IM 客户端可以渲染其自己的预览。不剥离，不获取。
		if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
			continue
		}

		var bytes []byte
		var filename string

		if strings.HasPrefix(path, "data:") {
			b, name, err := decodeDataURL(path)
			if err != nil {
				// 常见情况：LLM 幻觉产生一个带有截断/缩写 base64 的 data URL
				//（"...", 占位符, 随机假字节）。预期情况 — 在 Debug 级别记录，
				// 依赖工作空间回退仍然传递真实文件。不要为此刷 Warn 日志。
				head := path
				if len(head) > 80 {
					head = head[:80] + "…"
				}
				slog.Debug("data URL decode failed (LLM-fabricated bytes are expected — workspace fallback covers it)",
					"agent", agentID, "error", err, "len", len(path), "head", head)
			} else {
				bytes = b
				filename = name
			}
		} else if ws != nil {
			key := strings.TrimPrefix(path, "/workspace/")
			key = strings.TrimPrefix(key, "workspace/")
			key = strings.TrimPrefix(key, "/")
			if key != "" {
				rc, err := ws.Get(ctx, agentID, projectID, sessionID, key)
				if err != nil {
					slog.Warn("split media: workspace get failed", "agent", agentID, "project", projectID, "session", sessionID, "key", key, "error", err)
				} else {
					data, rerr := io.ReadAll(rc)
					rc.Close()
					if rerr != nil {
						slog.Warn("split media: read failed", "key", key, "error", rerr)
					} else {
						bytes = data
						filename = filepath.Base(key)
					}
				}
			}
		}

		if len(bytes) > 0 {
			if len(bytes) > maxAttachmentBytes {
				slog.Warn("split media: skipping oversize attachment",
					"agent", agentID, "session", sessionID,
					"filename", filename, "size", len(bytes), "cap", maxAttachmentBytes)
			} else {
				items = append(items, bus.MediaItem{
					Filename:    filename,
					ContentType: mime.TypeByExtension(filepath.Ext(filename)),
					Bytes:       bytes,
				})
			}
		}

		// 无论哪种方式都剥离 `![alt](src)` — 在聊天正文中留下无法解析的引用只会显示原始 markdown / base64 块。
		out.WriteString(reply[cursor:m[0]])
		cursor = m[1]
		// 如果后面有换行符，则删除图片引用后的尾随换行 — 当 LLM 将引用放在自己行上时保持正文整洁。
		if cursor < len(reply) && reply[cursor] == '\n' {
			cursor++
		}
	}
	out.WriteString(reply[cursor:])
	return strings.TrimSpace(out.String()), items
}

// decodeDataURL 解析 `data:image/png;base64,...` 样式的 URL 为原始字节。
// 返回 (bytes, suggested filename, error)。扩展名从 MIME 派生，
// 以便通过文件名嗅探内容类型的 IM（Telegram 对文档这样做）获得合理的默认值。
func decodeDataURL(dataURL string) ([]byte, string, error) {
	if !strings.HasPrefix(dataURL, "data:") {
		return nil, "", fmt.Errorf("not a data URL")
	}
	rest := dataURL[len("data:"):]
	commaIdx := strings.IndexByte(rest, ',')
	if commaIdx < 0 {
		return nil, "", fmt.Errorf("data URL missing payload")
	}
	meta := rest[:commaIdx]
	payload := rest[commaIdx+1:]
	mimeType := "application/octet-stream"
	isBase64 := false
	for _, part := range strings.Split(meta, ";") {
		switch {
		case part == "base64":
			isBase64 = true
		case strings.Contains(part, "/"):
			mimeType = part
		}
	}
	var raw []byte
	if isBase64 {
		// LLM 经常软换行长 base64 负载，在字符串中间放置空白，
		// 标准 StdEncoding 会拒绝这些空白并报 "illegal base64 data"。
		// 在解码前剥离空白，并通过备选字母表/填充，以便代理的 markdown
		// 在任何常见变体中都能生存：标准、URL 安全、带或不带填充。
		clean := stripWhitespace(payload)
		decoded, err := decodeBase64Tolerant(clean)
		if err != nil {
			return nil, "", fmt.Errorf("base64 decode: %w", err)
		}
		raw = decoded
	} else {
		// URL 编码的文本负载 — 对于图片很少见，但处理它。
		u, err := url.QueryUnescape(payload)
		if err != nil {
			return nil, "", fmt.Errorf("url unescape: %w", err)
		}
		raw = []byte(u)
	}
	return raw, "media" + mimeExt(mimeType), nil
}

func stripWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			continue
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func decodeBase64Tolerant(s string) ([]byte, error) {
	// 先尝试标准字母表变体（LLM 发出的 base64 几乎总是使用 `+/`）。
	// URL 字母表变体保留为低概率回退，但列在最后，以便报告的错误是标准字母表失败
	//（信息量大得多 — URL 字母表在遇到 `/` 字符时立即在字节 0 处失败，这对诊断毫无用处）。
	candidates := []struct {
		name string
		enc  *base64.Encoding
	}{
		{"std", base64.StdEncoding},
		{"raw_std", base64.RawStdEncoding},
		{"url", base64.URLEncoding},
		{"raw_url", base64.RawURLEncoding},
	}
	var errs []string
	for _, c := range candidates {
		if data, err := c.enc.DecodeString(s); err == nil {
			return data, nil
		} else {
			errs = append(errs, c.name+": "+err.Error())
		}
	}
	return nil, fmt.Errorf("all base64 encodings failed (%s)", strings.Join(errs, "; "))
}

// maxAttachmentBytes 限制每个文件的出站附件。大小设计为适合我们关心的最严格的 IM 平台限制
// （Discord 免费层 = 25MB），并远低于微信 CDN 上传超时的实际上限
// （90 秒在典型住宅上行链路上为约 25MB 留有余量）。超过此大小的文件被跳过 + 记录，而不是截断；
// 接收者看不到附件，但聊天侧文本仍然通过。
const maxAttachmentBytes = 25 * 1024 * 1024

// appendRecentWorkspaceMedia 列出会话的工作空间，并附加每个在 `turnStart` 或之后修改的可交付文件。
// 这是 IM 侧的保证："如果一个工具在本轮次写了可交付物，用户就会收到它" —
// 独立于 LLM 的回复 markdown 是否正确引用了它（损坏的 data URL、缺失的引用、幻觉文件名都绕过此路径）。
//
// 过滤规则：
//   - 扩展名在可交付白名单中（见 isShippableExt）：图片/视频/音频/常见文档容器。
//     特别排除 .md / .txt / .csv / .json / 源文件 — 这些通常是代理的草稿板
//     （todo.md、计划、中间输出），自动发送它们会是噪音，而不是价值。
//   - ModTime >= turnStart - 1s（为具有秒级粒度 mtime 的存储提供回退缓冲区 —
//     多发送比丢失边界文件好）。
//   - size <= maxAttachmentBytes（否则跳过 + 记录；我们会超出通道限制或使 CDN 上传超时）。
//   - 文件名尚未在 `existing` 中（去重 — splitMediaFromReply 可能已经解析了它）。
//
// 在每个过滤阶段记录计数，以便将来的"没有文件附加"报告可以仅从日志诊断。
func appendRecentWorkspaceMedia(ctx context.Context, ws workspace.Store, agentID, projectID, sessionID string, turnStart time.Time, existing []bus.MediaItem) []bus.MediaItem {
	if ws == nil {
		return existing
	}
	objs, err := ws.List(ctx, agentID, projectID, sessionID)
	if err != nil {
		slog.Warn("workspace list failed for media fallback",
			"agent", agentID, "project", projectID, "session", sessionID, "error", err)
		return existing
	}

	have := make(map[string]bool, len(existing))
	for _, it := range existing {
		have[it.Filename] = true
	}

	// 1 秒回退缓冲区：某些存储后端将 mtime 四舍五入到整秒，这可能导致在轮次开始后 0.4 秒写入的文件
	// 其 mtime 时间戳在 turnStart 前 0.6 秒。
	cutoff := turnStart.Add(-1 * time.Second)

	candidateCount := 0
	recentCount := 0
	oversizeCount := 0
	attached := 0
	for _, obj := range objs {
		if !isShippableExt(obj.Path) {
			continue
		}
		candidateCount++
		if obj.ModTime.Before(cutoff) {
			continue
		}
		recentCount++
		base := filepath.Base(obj.Path)
		if have[base] {
			continue
		}
		// 使用列表的大小提示提前跳过超大文件。不跟踪大小的存储（Size == -1）会落到下面的读取后检查。
		if obj.Size > 0 && obj.Size > maxAttachmentBytes {
			oversizeCount++
			slog.Warn("workspace media fallback: skipping oversize file",
				"agent", agentID, "session", sessionID,
				"path", obj.Path, "size", obj.Size, "cap", maxAttachmentBytes)
			continue
		}
		rc, gerr := ws.Get(ctx, agentID, projectID, sessionID, obj.Path)
		if gerr != nil {
			slog.Warn("workspace get failed for media fallback",
				"path", obj.Path, "error", gerr)
			continue
		}
		data, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil || len(data) == 0 {
			continue
		}
		if len(data) > maxAttachmentBytes {
			oversizeCount++
			slog.Warn("workspace media fallback: skipping oversize file (post-read)",
				"agent", agentID, "session", sessionID,
				"path", obj.Path, "size", len(data), "cap", maxAttachmentBytes)
			continue
		}
		existing = append(existing, bus.MediaItem{
			Filename:    base,
			ContentType: mime.TypeByExtension(filepath.Ext(base)),
			Bytes:       data,
		})
		have[base] = true
		attached++
	}
	slog.Info("workspace media fallback",
		"agent", agentID, "session", sessionID,
		"total_objs", len(objs), "candidates", candidateCount,
		"recent", recentCount, "oversize", oversizeCount,
		"attached", attached,
		"turn_start", turnStart.Format(time.RFC3339Nano))
	return existing
}

// isShippableExt 是工作空间媒体回退使用的"这是可交付物"白名单。有意保守 —
// 自动发送代理写入的每个 .md / .txt / .json 会将内部草稿板
// （todo.md、计划、中间暂存）作为聊天附件暴露。仅当文件几乎总是代表"用户要求的东西"时才添加新扩展名。
func isShippableExt(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	// 图片
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg":
		return true
	// 视频
	case ".mp4", ".mov", ".webm", ".mkv", ".avi":
		return true
	// 音频
	case ".mp3", ".wav", ".ogg", ".m4a", ".flac", ".aac":
		return true
	// 文档容器 — 完成的可交付物，不是草稿板。
	case ".pdf", ".docx", ".xlsx", ".pptx", ".zip":
		return true
	}
	return false
}

// mimeExt 从 MIME 类型中选择文件扩展名 — 覆盖 image-tool/replicate/OpenAI 图片生成实际发出的最小表。
// 对于未知的任何内容回退到 .bin。
func mimeExt(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/svg+xml":
		return ".svg"
	}
	return ".bin"
}
