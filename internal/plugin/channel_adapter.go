package plugin

import (
	"context"
	"log/slog"

	"github.com/qs3c/bkclaw/internal/bus"
)

// ChannelAdapter 包装了一个通道插件，用于实现 channels.Channel 接口。
// 这使得基于插件的通道可以无缝注册到通道管理器。
type ChannelAdapter struct {
	manager  *Manager
	pluginID string
	manifest *Manifest
}

// NewChannelAdapter 为通道插件创建一个适配器。
func NewChannelAdapter(mgr *Manager, pluginID string) *ChannelAdapter {
	inst := mgr.Plugin(pluginID)
	return &ChannelAdapter{
		manager:  mgr,
		pluginID: pluginID,
		manifest: inst.Manifest,
	}
}

// Name 返回通道名称（插件 ID，例如 "feishu"）。
func (a *ChannelAdapter) Name() string {
	return a.manifest.ID
}

// AccountID 返回插件 ID 作为账户标识符。
func (a *ChannelAdapter) AccountID() string {
	return ""
}

// BotUsername 返回空，因为插件通道管理自己的身份。
func (a *ChannelAdapter) BotUsername() string {
	return ""
}

// Start 阻塞直到 ctx 被取消。实际的消息接收由插件进程发送 message.inbound 通知来处理。
func (a *ChannelAdapter) Start(ctx context.Context) error {
	slog.Info("plugin channel started", "plugin", a.pluginID)
	<-ctx.Done()
	return nil
}

// Send 通过插件通道发送消息。
func (a *ChannelAdapter) Send(chatID string, text string) error {
	ctx := context.Background()
	return a.manager.SendToChannel(ctx, a.pluginID, chatID, text)
}

// SendMessage 发送一条富文本出站消息。插件通道使用纯文本。
func (a *ChannelAdapter) SendMessage(msg bus.OutboundMessage) error {
	return a.Send(msg.ChatID, msg.Text)
}

// SendTyping 对插件通道来说是一个空操作。
func (a *ChannelAdapter) SendTyping(_ string) error {
	return nil
}
