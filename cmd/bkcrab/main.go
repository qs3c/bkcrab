package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/api"
	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/daemon"
	"github.com/qs3c/bkcrab/internal/gateway"
	"github.com/qs3c/bkcrab/internal/setup"
	"github.com/qs3c/bkcrab/internal/store"
)

// apiResolver 适配器，将 *gateway.Gateway 转换为 api.UserResolver。
type apiResolver struct {
	gw *gateway.Gateway
}

func (a *apiResolver) UserSpaceFor(userID string) (*api.UserSpaceView, error) {
	sp, err := a.gw.UserSpaceFor(userID)
	if err != nil {
		return nil, err
	}
	return &api.UserSpaceView{
		UserID: sp.UserID,
		Agents: sp.Agents,
		Config: sp.Config,
	}, nil
}

func (a *apiResolver) LocalAgentManager() *agent.Manager { return a.gw.LocalAgentManager() }
func (a *apiResolver) IsCloudMode() bool                 { return a.gw.IsCloudMode() }
func (a *apiResolver) InvalidateUser(userID string)      { a.gw.InvalidateUser(userID) }

// InvalidateAgent 转发到网关，使得代理作用域的变更
// （PUT /api/agents/{id} 模型更改、代理作用域的提供者/设置写入）
// 实际丢弃缓存的 UserSpace。如果没有此方法，
// 则 setup.invalidateAgent 的类型断言会静默失败，
// 聊天将一直使用更改前的模型，直到 30 分钟空闲驱逐触发。
func (a *apiResolver) InvalidateAgent(agentID string) { a.gw.InvalidateAgent(agentID) }

func (a *apiResolver) EnsureAgent(ctx context.Context, userID, agentID string) error {
	return a.gw.EnsureAgent(ctx, userID, agentID)
}

// ReloadAgents 丢弃所有缓存的 UserSpace，以便每个缓存在下一次请求时重新加载。
// setup.invalidateScope 的系统作用域分支对解析器进行类型断言为此接口——没有此方法时，
// 断言会静默失败，系统作用域设置保存（sandbox、agents.defaults 等）
// 会使运行中的网关保持其保存前的快照，在下一次聊天轮次中表现为 "model is empty" 的谜团。
func (a *apiResolver) ReloadAgents() error { return a.gw.ReloadAgents() }

// RegisterChannelFromConfig 热启动新保存的频道记录。
// 由设置处理程序在持久化新的 bot 配置后调用，
// 以便适配器无需重启进程即可开始轮询。
func (a *apiResolver) RegisterChannelFromConfig(rec store.ConfigRecord) error {
	return a.gw.RegisterChannelFromConfig(rec)
}

func (a *apiResolver) UnregisterChannel(channelType, accountID string) {
	a.gw.UnregisterChannel(channelType, accountID)
}

func (a *apiResolver) DispatchFeishuWebhook(accountID string, body []byte) ([]byte, int, error) {
	return a.gw.DispatchFeishuWebhook(accountID, body)
}

func (a *apiResolver) DispatchLINEWebhook(accountID string, body []byte, signature string) ([]byte, int, error) {
	return a.gw.DispatchLINEWebhook(accountID, body, signature)
}

func main() {
	rootCmd := &cobra.Command{
		Use:   "bkcrab",
		Short: "BkCrab - Multi-User AI Agent Platform",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGateway(18953)
		},
	}

	rootCmd.AddCommand(gatewayCmd())
	rootCmd.AddCommand(skillCmd())
	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(upgradeCmd())
	rootCmd.AddCommand(pluginCmd())
	rootCmd.AddCommand(providerCmd())
	rootCmd.AddCommand(sandboxCmd())
	rootCmd.AddCommand(policyCmd())
	rootCmd.AddCommand(daemonCmd())
	rootCmd.AddCommand(adminCmd())
	rootCmd.AddCommand(apikeyCmd())
	rootCmd.AddCommand(agentsCmd())
	rootCmd.AddCommand(sessionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func gatewayCmd() *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Start the BkCrab gateway",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGateway(port)
		},
	}
	cmd.Flags().IntVar(&port, "port", 18953, "port for setup wizard / web UI")
	return cmd
}

func runGateway(port int) error {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	env := config.LoadEnv()
	if env.Gateway.Port > 0 {
		port = env.Gateway.Port
	}

	agent.InstallBundledSkills()

	if err := daemon.WritePIDFile(); err != nil {
		slog.Warn("failed to write PID file", "error", err)
	}
	defer daemon.RemovePIDFile()

	gw, err := gateway.New(env)
	if err != nil {
		return fmt.Errorf("create gateway: %w", err)
	}

	// 从进程环境中移除包含凭据的环境变量，因为启动配置已读取完毕。
	// 关闭 /proc/<pid>/environ 路径，否则拥有 shell 的 LLM 可能利用该路径
	// 恢复守护进程的存储 DSN 和对象存储密钥。有关权衡说明，
	// 请参见 config.ScrubBootSecrets。
	config.ScrubBootSecrets()

	authResolver, err := auth.NewResolver(gw.Store())
	if err != nil {
		return fmt.Errorf("create auth resolver: %w", err)
	}

	gwCfg := &config.GatewayCfg{
		Port: port,
		Bind: env.Gateway.Bind,
		HTTP: config.GatewayHTTP{
			Endpoints: config.GatewayHTTPEndpoints{
				ChatCompletions: config.GatewayEndpoint{Enabled: true},
				Agents:          config.GatewayEndpoint{Enabled: true},
			},
		},
	}

	webSrv := setup.NewServer(port)
	webSrv.SetTaskQueue(gw.TaskQueue())
	webSrv.SetGatewayConfig(gwCfg)
	webSrv.SetUserResolver(&apiResolver{gw: gw})
	webSrv.SetStore(gw.Store())
	webSrv.SetWorkspaceStore(gw.Workspace())
	webSrv.SetUsageMeter(gw.Usage())
	webSrv.SetRAGConfig(gw.RAGConfig())
	webSrv.SetRAGService(gw.RAG())
	webSrv.SetRAGParserHealthProvider(gw)
	webSrv.SetAuth(authResolver)
	webSrv.SetWebChannel(gw.WebChannel())
	// 共享聊天事件集线器，使得总线触发的 web 轮次（cron / 目标
	// 延续 / 心跳 / 子代理）通过与用户输入轮次相同的 SSE 管道传输。
	// 必须在 gw.Run() 启动总线消费者之前连接。
	gw.SetChatEvents(webSrv.ChatEventHub())

	apiSrv := api.NewServer(&apiResolver{gw: gw}, authResolver, gwCfg)
	webSrv.SetAPIServer(apiSrv)

	bindMode := gwCfg.Bind
	if bindMode == "" {
		bindMode = "loopback"
	}
	slog.Info("gateway starting", "port", port, "bind", bindMode)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		if err := webSrv.Run(ctx); err != nil {
			slog.Error("web server error", "error", err)
		}
	}()

	url := fmt.Sprintf("http://localhost:%d", port)
	slog.Info("web UI available", "url", url)
	// 在看起来是全新安装时自动打开浏览器。
	if n, _ := countUsersSafe(gw); n == 0 {
		go openBrowser(url)
	}

	return gw.Run()
}

func countUsersSafe(gw *gateway.Gateway) (int, error) {
	st := gw.Store()
	if st == nil {
		return 0, nil
	}
	return st.CountUsers(context.Background())
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		return
	}
	cmd.Run()
}
