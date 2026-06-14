package gateway

import (
	"context"
	"errors"
	"log/slog"

	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/store"
)

// InvalidateUser 丢弃用户缓存的 UserSpace，以便下次访问时从数据库重新加载。
// 由管理处理程序在代理/提供者/通道写入后调用，以便更改生效而无需重启进程。
func (g *Gateway) InvalidateUser(userID string) {
	if g.users == nil || userID == "" {
		return
	}
	g.users.invalidate(userID)
	slog.Info("user space invalidated; will reload on next access", "user", userID)
}

// InvalidateAgent 丢弃所有持有给定代理的缓存 UserSpace — 所有者空间（始终由 loadUserSpace 预加载）
// 加上通过 EnsureAgent 延迟附加的任何外部空间（super_admin 浏览、公开链接查看器、apikey 调用者）。
// 在代理作用域设置/提供者写入后使用，以便非所有者查看者不会保留过时的 rc.Model/providers
// 直到 30 分钟空闲驱逐生效。
func (g *Gateway) InvalidateAgent(agentID string) {
	if g.users == nil || agentID == "" {
		return
	}
	for _, sp := range g.users.all() {
		if sp.Agents == nil {
			continue
		}
		if sp.Agents.AgentByID(agentID) != nil {
			g.users.invalidate(sp.UserID)
		}
	}
	slog.Info("agent invalidated; affected user spaces dropped", "agent", agentID)
}

// ReloadAgents 保留在 Gateway 上供调用者（代理 CRUD 后的管理 API）
// 想要强制刷新每个已加载的空间。新模型在每次认证时延迟加载，所以实际效果只是丢弃缓存。
func (g *Gateway) ReloadAgents() error {
	if g.users == nil {
		return nil
	}
	for _, sp := range g.users.all() {
		g.users.invalidate(sp.UserID)
	}
	slog.Info("hot-reload: invalidated all loaded user spaces")
	return nil
}

// reloadAgentForUser 是更细粒度的无效化，由 setup 处理程序在单个用户修改自己的代理后使用。
func (g *Gateway) reloadAgentForUser(_ context.Context, userID string) {
	g.InvalidateUser(userID)
}

// RegisterChannelFromConfig 热启动一个通道适配器用于新保存的配置行，无需重启进程。
// 由仪表盘的每个代理通道处理程序在成功保存后调用，以便新的 Telegram bot 立即开始轮询。
// 在 chanMgr 级别是幂等的 — 重新保存相同的 accountID 会替换适配器。
func (g *Gateway) RegisterChannelFromConfig(rec store.ConfigRecord) error {
	if g.chanMgr == nil || g.bus == nil {
		return nil
	}
	return registerChannelInstance(rec, g.bus, g.chanMgr, g.store, true)
}

// UnregisterChannel 从路由表中移除一个通道。注意：
// bot 的轮询 goroutine 在根 ctx 结束时自然消亡 —
// 原因见 channels.Manager.Unregister。绑定行被删除的那一刻，入站消息停止路由到代理。
func (g *Gateway) UnregisterChannel(channelType, accountID string) {
	if g.chanMgr == nil {
		return
	}
	g.chanMgr.Unregister(channelType, accountID)
}

// DispatchLINEWebhook 将原始的 LINE webhook POST 主体交给 accountID 的适配器。
// Signature 是 `x-line-signature` 头的值 — 适配器将其与 HMAC-SHA256(channel_secret, body) 进行校验。
// 返回 HTTP 处理程序应写回的响应体 + 状态码；LINE 在非 2xx 时重试。
func (g *Gateway) DispatchLINEWebhook(accountID string, body []byte, signature string) (responseBody []byte, status int, err error) {
	if g.chanMgr == nil {
		return nil, 503, errors.New("channel manager not running")
	}
	ch := g.chanMgr.Get("line", accountID)
	if ch == nil {
		return nil, 404, errors.New("no line channel for account")
	}
	ln, ok := ch.(*channels.LINE)
	if !ok {
		return nil, 500, errors.New("registered channel is not a LINE adapter")
	}
	return ln.HandleWebhook(body, signature)
}

// DispatchFeishuWebhook 将原始的飞书 webhook POST 主体交给注册在 accountID 的适配器。
// 适配器处理 URL 验证挑战 + im.message.receive_v1 分发 + token 验证；
// HTTP 处理程序仅转发响应体/状态码。当没有注册适配器时返回 ErrUnknownAccount。
func (g *Gateway) DispatchFeishuWebhook(accountID string, body []byte) (responseBody []byte, status int, err error) {
	if g.chanMgr == nil {
		return nil, 503, errors.New("channel manager not running")
	}
	ch := g.chanMgr.Get("feishu", accountID)
	if ch == nil {
		return nil, 404, errors.New("no feishu channel for account")
	}
	lk, ok := ch.(*channels.Feishu)
	if !ok {
		return nil, 500, errors.New("registered channel is not a Feishu adapter")
	}
	return lk.HandleWebhook(body)
}
