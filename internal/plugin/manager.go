package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

const shutdownTimeout = 5 * time.Second

// defaultChatSendDelay 是在将插件的 chat.send 推送到 bus.Outbound 之前应用的
// 小异步延迟——相关排序说明见 handleNotification 中 chat.send 分支的注释。
const defaultChatSendDelay = 50 * time.Millisecond

func pluginChatSendDelay() time.Duration {
	v := os.Getenv("BKCRAB_PLUGIN_CHAT_SEND_DELAY_MS")
	if v == "" {
		return defaultChatSendDelay
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms < 0 {
		return defaultChatSendDelay
	}
	return time.Duration(ms) * time.Millisecond
}

// Manifest 是 plugin.json 的描述符。
type Manifest struct {
	ID           string                       `json:"id"`
	Name         string                       `json:"name"`
	Version      string                       `json:"version"`
	Description  string                       `json:"description"`
	Type         string                       `json:"type"` // channel, tool, provider, hook
	Command      string                       `json:"command"`
	Capabilities []string                     `json:"capabilities,omitempty"`
	ConfigSchema map[string]ManifestConfigDef `json:"config,omitempty"`

	Dir string `json:"-"` // directory containing the plugin
}

// ManifestConfigDef 描述 plugin.json 中的配置字段。
type ManifestConfigDef struct {
	Type      string `json:"type"`
	Required  bool   `json:"required,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
	Default   string `json:"default,omitempty"`
}

// PluginInstance 保存已加载插件的清单、进程和运行时状态。
type PluginInstance struct {
	Manifest *Manifest
	Process  *Process
	Config   map[string]interface{}
	Enabled  bool
}

// Manager 发现、加载并管理插件的生命周期。
type Manager struct {
	plugins map[string]*PluginInstance
	bus     *bus.MessageBus
	mu      sync.RWMutex
}

// NewManager 创建一个插件管理器。
func NewManager(mb *bus.MessageBus) *Manager {
	return &Manager{
		plugins: make(map[string]*PluginInstance),
		bus:     mb,
	}
}

// Discover 扫描目录中的 plugin.json 文件并加载清单。
func (m *Manager) Discover(paths []string) error {
	for _, dir := range paths {
		dir = expandHome(dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			slog.Warn("plugin: cannot read directory", "path", dir, "error", err)
			continue
		}

		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			pluginDir := filepath.Join(dir, entry.Name())
			manifest, err := loadManifest(pluginDir)
			if err != nil {
				slog.Warn("plugin: skip directory", "path", pluginDir, "error", err)
				continue
			}

			m.mu.Lock()
			m.plugins[manifest.ID] = &PluginInstance{
				Manifest: manifest,
				Enabled:  true,
			}
			m.mu.Unlock()

			slog.Info("plugin: discovered", "id", manifest.ID, "type", manifest.Type, "version", manifest.Version)
		}
	}
	return nil
}

// ApplyConfig 从用户配置中设置每个插件的配置和启用状态。
func (m *Manager) ApplyConfig(entries map[string]PluginEntryCfg) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for id, entry := range entries {
		inst, ok := m.plugins[id]
		if !ok {
			continue
		}
		inst.Enabled = entry.Enabled
		inst.Config = entry.Config
	}
}

// StartAll 启动所有已启用的插件并发送初始化请求。
func (m *Manager) StartAll(ctx context.Context) error {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for id, inst := range m.plugins {
		if !inst.Enabled {
			slog.Info("plugin: skipping disabled", "id", id)
			continue
		}

		proc := NewProcess(inst.Manifest)
		inst.Process = proc

		// 设置入站消息的通知处理函数
		proc.SetNotifyHandler(func(n Notification) {
			m.handleNotification(id, n)
		})

		if err := proc.Start(ctx); err != nil {
			slog.Error("plugin: failed to start", "id", id, "error", err)
			continue
		}

		// 发送带配置的初始化请求
		cfg := inst.Config
		if cfg == nil {
			cfg = make(map[string]interface{})
		}
		initParams := InitializeParams{Config: cfg}
		if _, err := proc.Call(ctx, MethodInitialize, initParams); err != nil {
			slog.Error("plugin: initialize failed", "id", id, "error", err)
			proc.Stop(shutdownTimeout)
			continue
		}

		slog.Info("plugin: started", "id", id)
	}
	return nil
}

// StopAll 优雅地停止所有正在运行的插件。
func (m *Manager) StopAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	for id, inst := range m.plugins {
		if inst.Process != nil && inst.Process.IsRunning() {
			slog.Info("plugin: stopping", "id", id)
			inst.Process.Stop(shutdownTimeout)
		}
	}
}

// Plugins 返回所有已发现的插件实例。
func (m *Manager) Plugins() []*PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]*PluginInstance, 0, len(m.plugins))
	for _, inst := range m.plugins {
		result = append(result, inst)
	}
	return result
}

// Plugin 根据 ID 返回特定的插件。
func (m *Manager) Plugin(id string) *PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.plugins[id]
}

// ChannelPlugins 返回所有正在运行且提供通道能力的插件。
func (m *Manager) ChannelPlugins() []*PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*PluginInstance
	for _, inst := range m.plugins {
		if !inst.Enabled || inst.Process == nil || !inst.Process.IsRunning() {
			continue
		}
		if hasCapability(inst.Manifest, "channel") {
			result = append(result, inst)
		}
	}
	return result
}

// ToolPlugins 返回所有正在运行且提供工具能力的插件。
func (m *Manager) ToolPlugins() []*PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*PluginInstance
	for _, inst := range m.plugins {
		if !inst.Enabled || inst.Process == nil || !inst.Process.IsRunning() {
			continue
		}
		if hasCapability(inst.Manifest, "tool") {
			result = append(result, inst)
		}
	}
	return result
}

// HookPlugins 返回所有正在运行且提供钩子能力的插件。
func (m *Manager) HookPlugins() []*PluginInstance {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []*PluginInstance
	for _, inst := range m.plugins {
		if !inst.Enabled || inst.Process == nil || !inst.Process.IsRunning() {
			continue
		}
		if hasCapability(inst.Manifest, "hook") {
			result = append(result, inst)
		}
	}
	return result
}

// ListTools 查询插件以获取其可用的工具列表。
func (m *Manager) ListTools(ctx context.Context, pluginID string) ([]ToolDef, error) {
	inst := m.Plugin(pluginID)
	if inst == nil || inst.Process == nil || !inst.Process.IsRunning() {
		return nil, fmt.Errorf("plugin %s not running", pluginID)
	}

	result, err := inst.Process.Call(ctx, MethodToolList, nil)
	if err != nil {
		return nil, err
	}

	var toolResult ToolListResult
	if err := json.Unmarshal(result, &toolResult); err != nil {
		return nil, fmt.Errorf("parse tool.list response: %w", err)
	}
	return toolResult.Tools, nil
}

// ExecuteTool 调用特定插件的工具。
func (m *Manager) ExecuteTool(ctx context.Context, pluginID, toolName string, args map[string]interface{}) (string, error) {
	inst := m.Plugin(pluginID)
	if inst == nil || inst.Process == nil || !inst.Process.IsRunning() {
		return "", fmt.Errorf("plugin %s not running", pluginID)
	}

	params := ToolExecuteParams{Name: toolName, Args: args}
	result, err := inst.Process.Call(ctx, MethodToolExecute, params)
	if err != nil {
		return "", err
	}

	var toolResult ToolExecuteResult
	if err := json.Unmarshal(result, &toolResult); err != nil {
		return "", fmt.Errorf("parse tool.execute response: %w", err)
	}
	return toolResult.Result, nil
}

// ListProviders 查询插件以获取其填充的工具提供者槽位。
// 未实现 provider.list 的插件仅返回空切片。
func (m *Manager) ListProviders(ctx context.Context, pluginID string) ([]ProviderDef, error) {
	inst := m.Plugin(pluginID)
	if inst == nil || inst.Process == nil || !inst.Process.IsRunning() {
		return nil, fmt.Errorf("plugin %s not running", pluginID)
	}
	result, err := inst.Process.Call(ctx, MethodProviderList, nil)
	if err != nil {
		// 将"未知方法"等视为"插件声明零个提供者"，
		// 以便旧插件与新协议共存。
		return nil, nil
	}
	var listResult ProviderListResult
	if err := json.Unmarshal(result, &listResult); err != nil {
		return nil, fmt.Errorf("parse provider.list response: %w", err)
	}
	return listResult.Providers, nil
}

// ExecuteProvider 调用插件的一个已注册提供者。
func (m *Manager) ExecuteProvider(ctx context.Context, pluginID string, params ProviderExecuteParams) (ProviderExecuteResult, error) {
	inst := m.Plugin(pluginID)
	if inst == nil || inst.Process == nil || !inst.Process.IsRunning() {
		return ProviderExecuteResult{}, fmt.Errorf("plugin %s not running", pluginID)
	}
	result, err := inst.Process.Call(ctx, MethodProviderExecute, params)
	if err != nil {
		return ProviderExecuteResult{}, err
	}
	var out ProviderExecuteResult
	if err := json.Unmarshal(result, &out); err != nil {
		return ProviderExecuteResult{}, fmt.Errorf("parse provider.execute response: %w", err)
	}
	return out, nil
}

// SendToChannel 通过通道插件发送消息。
func (m *Manager) SendToChannel(ctx context.Context, pluginID, chatID, text string) error {
	inst := m.Plugin(pluginID)
	if inst == nil || inst.Process == nil || !inst.Process.IsRunning() {
		return fmt.Errorf("plugin %s not running", pluginID)
	}

	params := ChannelSendParams{ChatID: chatID, Text: text}
	_, err := inst.Process.Call(ctx, MethodChannelSend, params)
	return err
}

// handleNotification 处理来自插件的通知。
func (m *Manager) handleNotification(pluginID string, n Notification) {
	switch n.Method {
	case MethodMessageInbound:
		var params InboundMessageParams
		if err := json.Unmarshal(n.Params, &params); err != nil {
			slog.Warn("plugin: invalid inbound message", "plugin", pluginID, "error", err)
			return
		}

		channel := params.Channel
		if channel == "" {
			channel = "plugin:" + pluginID
		}
		peerKind := params.PeerKind
		if peerKind == "" {
			peerKind = "dm"
		}

		m.bus.Inbound <- bus.InboundMessage{
			Channel:    channel,
			ChatID:     params.ChatID,
			UserID:     params.UserID,
			Text:       params.Text,
			PeerKind:   peerKind,
			SenderName: params.SenderName,
		}

		slog.Info("plugin: inbound message", "plugin", pluginID, "channel", channel, "chat_id", params.ChatID)

	case MethodChatSend:
		var params ChatSendParams
		if err := json.Unmarshal(n.Params, &params); err != nil {
			slog.Warn("plugin: invalid chat.send", "plugin", pluginID, "error", err)
			return
		}
		if params.Channel == "" || params.ChatID == "" {
			slog.Warn("plugin: chat.send missing channel/chatId", "plugin", pluginID)
			return
		}
		items := make([]bus.MediaItem, 0, len(params.Media))
		for _, m := range params.Media {
			data, err := base64.StdEncoding.DecodeString(m.BytesB64)
			if err != nil {
				slog.Warn("plugin: chat.send media base64 decode failed",
					"plugin", pluginID, "filename", m.Filename, "error", err)
				continue
			}
			items = append(items, bus.MediaItem{
				Filename:    m.Filename,
				ContentType: m.ContentType,
				Bytes:       data,
			})
		}
		out := bus.OutboundMessage{
			Channel:    params.Channel,
			AccountID:  params.AccountID,
			AgentID:    params.AgentID,
			ChatID:     params.ChatID,
			Text:       params.Text,
			MediaItems: items,
		}
		// 排序与代理的主回复的关系：PostTurn 钩子在代理循环
		// 仍在完成时触发，因此当插件响应 PostTurn 并立即调用
		// chat.send 时，网关尚未将代理的回复推送到
		// bus.Outbound。如果没有下面的延迟，快速插件会赢得
		// 竞态条件，导致聊天者先在气泡中看到插件的后续消息，
		// 然后才是代理的实际回复。
		//
		// 一旦 HandleMessage 返回，网关的 bus.Outbound 入队
		// 时间在亚毫秒级。这里的一个短异步延迟足以让它在
		// 实践中胜出。异步操作使插件的 stdout 读取器不会被
		// 阻塞。可通过 BKCRAB_PLUGIN_CHAT_SEND_DELAY_MS 调整；
		// 设置为 0 可完全禁用延迟。
		delay := pluginChatSendDelay()
		go func() {
			if delay > 0 {
				time.Sleep(delay)
			}
			select {
			case m.bus.Outbound <- out:
				slog.Info("plugin: chat.send dispatched",
					"plugin", pluginID, "channel", out.Channel,
					"chat_id", out.ChatID, "text_len", len(out.Text),
					"media_count", len(out.MediaItems))
			default:
				slog.Warn("plugin: chat.send dropped — bus.Outbound full",
					"plugin", pluginID, "channel", out.Channel, "chat_id", out.ChatID)
			}
		}()

	default:
		slog.Debug("plugin: unhandled notification", "plugin", pluginID, "method", n.Method)
	}
}

// PluginEntryCfg 是来自用户配置文件的每个插件的配置。
type PluginEntryCfg struct {
	Enabled bool                   `json:"enabled"`
	Config  map[string]interface{} `json:"config,omitempty"`
}

// loadManifest 从目录中读取 plugin.json 文件。
func loadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, "plugin.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if m.ID == "" {
		return nil, fmt.Errorf("%s: missing id field", path)
	}
	if m.Command == "" {
		return nil, fmt.Errorf("%s: missing command field", path)
	}

	m.Dir = dir
	return &m, nil
}

func hasCapability(m *Manifest, cap string) bool {
	// 首先检查能力列表
	for _, c := range m.Capabilities {
		if c == cap {
			return true
		}
	}
	// 回退到类型字段
	return m.Type == cap
}

func expandHome(path string) string {
	if len(path) > 1 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
