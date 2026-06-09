package plugin

import (
	"context"
	"log/slog"

	"github.com/bkclaw-ai/bkclaw/internal/bus"
)

// ChannelAdapter wraps a channel plugin to implement the channels.Channel interface.
// This lets plugin-based channels be registered with the channel manager seamlessly.
type ChannelAdapter struct {
	manager  *Manager
	pluginID string
	manifest *Manifest
}

// NewChannelAdapter creates an adapter for a channel plugin.
func NewChannelAdapter(mgr *Manager, pluginID string) *ChannelAdapter {
	inst := mgr.Plugin(pluginID)
	return &ChannelAdapter{
		manager:  mgr,
		pluginID: pluginID,
		manifest: inst.Manifest,
	}
}

// Name returns the channel name (plugin ID, e.g. "feishu").
func (a *ChannelAdapter) Name() string {
	return a.manifest.ID
}

// AccountID returns the plugin ID as account identifier.
func (a *ChannelAdapter) AccountID() string {
	return ""
}

// BotUsername returns empty since plugin channels manage their own identity.
func (a *ChannelAdapter) BotUsername() string {
	return ""
}

// Start blocks until ctx is cancelled. The actual message receiving is handled
// by the plugin process sending message.inbound notifications.
func (a *ChannelAdapter) Start(ctx context.Context) error {
	slog.Info("plugin channel started", "plugin", a.pluginID)
	<-ctx.Done()
	return nil
}

// Send sends a message through the plugin channel.
func (a *ChannelAdapter) Send(chatID string, text string) error {
	ctx := context.Background()
	return a.manager.SendToChannel(ctx, a.pluginID, chatID, text)
}

// SendMessage sends a rich outbound message. Plugin channels use plain text.
func (a *ChannelAdapter) SendMessage(msg bus.OutboundMessage) error {
	return a.Send(msg.ChatID, msg.Text)
}

// SendTyping is a no-op for plugin channels.
func (a *ChannelAdapter) SendTyping(_ string) error {
	return nil
}
