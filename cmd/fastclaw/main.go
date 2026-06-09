package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/fastclaw-ai/fastclaw/internal/agent"
	"github.com/fastclaw-ai/fastclaw/internal/api"
	"github.com/fastclaw-ai/fastclaw/internal/auth"
	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/daemon"
	"github.com/fastclaw-ai/fastclaw/internal/gateway"
	"github.com/fastclaw-ai/fastclaw/internal/setup"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// apiResolver adapts *gateway.Gateway to api.UserResolver.
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

// InvalidateAgent forwards to the gateway so agent-scope mutations
// (PUT /api/agents/{id} model change, agent-scope provider/setting
// writes) actually drop the cached UserSpace. Without this method on
// the resolver, setup.invalidateAgent's type assertion silently fails
// and chat keeps firing the pre-change model until the 30-min idle
// eviction kicks in.
func (a *apiResolver) InvalidateAgent(agentID string) { a.gw.InvalidateAgent(agentID) }

func (a *apiResolver) EnsureAgent(ctx context.Context, userID, agentID string) error {
	return a.gw.EnsureAgent(ctx, userID, agentID)
}

// ReloadAgents drops every cached UserSpace so each one reloads on the
// next request. setup.invalidateScope's system-scope branch type-
// asserts the resolver to this interface — without the method the
// assertion silently fails and a system-scope settings save (sandbox,
// agents.defaults, …) leaves the running gateway pinned to its pre-
// save snapshot, surfacing as "model is empty" mysteries on the next
// chat turn.
func (a *apiResolver) ReloadAgents() error { return a.gw.ReloadAgents() }

// RegisterChannelFromConfig hot-starts a freshly-saved channel row.
// Called by setup handlers after they persist a new bot config so the
// adapter starts polling without a process restart.
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
		Use:   "fastclaw",
		Short: "FastClaw - Multi-User AI Agent Platform",
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
		Short: "Start the FastClaw gateway",
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

	// Remove credential-bearing env vars from the process environment
	// now that boot config has been read. Closes the /proc/<pid>/environ
	// path that a shell-having LLM could otherwise use to recover the
	// daemon's storage DSN and object-store keys. See
	// config.ScrubBootSecrets for the trade-off note.
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
	webSrv.SetAuth(authResolver)
	webSrv.SetWebChannel(gw.WebChannel())
	// Share the chat-event hub so bus-fired web turns (cron / goal
	// continuation / heartbeat / sub-agent) stream through the same
	// SSE pipeline a user-typed turn uses. Must be wired before
	// gw.Run() starts the bus consumer.
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
	// Auto-open the browser when this looks like a fresh install.
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
