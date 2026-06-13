package channels

import (
	"context"
	"log/slog"
	"strings"
	"sync"

	"github.com/qs3c/bkclaw/internal/bus"
)

// Manager 管理所有渠道实例并路由出站消息。
type Manager struct {
	mu       sync.Mutex
	channels map[string]Channel // key: "channel:accountID"
	// singleton 跟踪哪些注册渠道由 Leaser 门控（每个 (channel, accountID) 同一时间
	// 只允许一个进程运行）。由 RegisterSingleton 设置；非单例渠道（webhook 适配器、
	// Web 扇出、插件渠道）不在此映射中，在每个副本上无条件运行其 Start。
	singleton map[string]struct{}
	// tgTokens 跟踪已被此进程声明的 Telegram bot token，以避免在同一个 token 上
	// 启动两个轮询器（它们会争夺长轮询锁并永远互相刷 409 Conflict）。
	// 进程生命周期内有效——Unregister 不释放，因为底层的 GetUpdatesChan
	// goroutine 无法在轮询中途取消（参见 Unregister）。
	tgTokens map[string]struct{}
	bus      *bus.MessageBus
	// leaser + holderID 驱动跨进程单例门控。nil leaser（或 NopLeaser）
	// 将 RegisterSingleton 降级为普通 Register。holderID 是持久化到
	// channel_leases.holder_id 的每进程标识符，必须在续约间保持稳定。
	leaser   Leaser
	holderID string
	// 由 Start 捕获，以便 RegisterAndStart 可以为初始引导后添加的渠道
	// 热启动 goroutine。在 Start 运行前为 nil。
	rootCtx context.Context
}

// NewManager 创建不带跨进程单例支持的新渠道管理器——所有标记为
// 单例的渠道降级为普通渠道（每个副本都 Start）。在多实例运行时
// 使用 NewManagerWithLeaser 来门控轮询适配器。
func NewManager(mb *bus.MessageBus) *Manager {
	return NewManagerWithLeaser(mb, NopLeaser{}, "")
}

// NewManagerWithLeaser 连接跨进程 Leaser。`holderID` 必须每进程唯一
// （通常为启动时生成的 UUID）且在进程生命周期内稳定，以便
// RenewChannelLease 始终匹配同一行。
func NewManagerWithLeaser(mb *bus.MessageBus, leaser Leaser, holderID string) *Manager {
	if leaser == nil {
		leaser = NopLeaser{}
	}
	return &Manager{
		channels:  make(map[string]Channel),
		singleton: make(map[string]struct{}),
		tgTokens:  make(map[string]struct{}),
		bus:       mb,
		leaser:    leaser,
		holderID:  holderID,
	}
}

// ClaimTelegramToken 在调用方是此进程中第一个声明此 token 时返回 true，
// 如果另一个适配器已持有则返回 false。此函数返回 false 时调用方应跳过注册。
// 空 token 不被跟踪（NewTelegram 会在空 token 上大声失败）。
func (m *Manager) ClaimTelegramToken(token string) bool {
	if token == "" {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tgTokens[token]; exists {
		return false
	}
	m.tgTokens[token] = struct{}{}
	return true
}

// Register 以 channel:accountID 为键将渠道添加到管理器。在 Start 之前
// 使用此方法；对于 Start 后的热添加，使用 RegisterAndStart。
func (m *Manager) Register(ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := channelKey(ch.Name(), ch.AccountID())
	m.channels[key] = ch
}

// RegisterSingleton 类似 Register，但标记渠道需要跨进程领导选举。
// 每个 (channel, accountID) 在同一时间只有一个副本的 Start 运行；
// 对等方在 Leaser 上等待，直到活跃持有者死亡。用于轮询/持久连接适配器
//（Telegram 长轮询、微信 iLink 长轮询、Discord WS、Slack Socket Mode、
// 飞书长连接）——任何在两个进程同时与同一上游协议对话时会导致入站消息
// 重复的适配器。
func (m *Manager) RegisterSingleton(ch Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := channelKey(ch.Name(), ch.AccountID())
	m.channels[key] = ch
	m.singleton[key] = struct{}{}
}

// RegisterAndStart 添加渠道并且，如果 Start 已经运行，立即启动其
// 轮询 goroutine。由仪表板的渠道配置处理程序使用，以便新保存的
// Telegram bot 无需进程重启即可开始接收更新。
//
// 在 Start 之前调用也是安全的——该情况下回退为普通 Register
//（Start 会像任何其他条目一样拾取它）。
func (m *Manager) RegisterAndStart(ch Channel) {
	m.registerAndStart(ch, false)
}

// RegisterSingletonAndStart 是单例门控适配器的热添加路径。
// 与 RegisterAndStart 形状相同，但启动的 goroutine 通过 Leaser
// 而非直接调用 ch.Start。
func (m *Manager) RegisterSingletonAndStart(ch Channel) {
	m.registerAndStart(ch, true)
}

func (m *Manager) registerAndStart(ch Channel, singleton bool) {
	m.mu.Lock()
	key := channelKey(ch.Name(), ch.AccountID())
	m.channels[key] = ch
	if singleton {
		m.singleton[key] = struct{}{}
	}
	ctx := m.rootCtx
	leaser := m.leaser
	holderID := m.holderID
	m.mu.Unlock()
	if ctx == nil {
		return
	}
	go func() {
		slog.Info("hot-starting channel", "key", key, "singleton", singleton)
		if singleton {
			runWithLease(ctx, ch, leaser, holderID)
			return
		}
		if err := ch.Start(ctx); err != nil {
			slog.Error("channel stopped with error", "key", key, "error", err)
		}
	}()
}

// Unregister 从路由表中移除渠道。渠道自身的 Start goroutine 不会在此处
// 被取消——它会在根 ctx 结束时退出。目前这只是停止出站路由；
// bot 适配器的轮询循环不会被触碰（Telegram 的 GetUpdatesChan 无法
// 在不拆除整个管理器的情况下中途取消）。对从 UI 删除来说足够了：
// 下次进程重启时干净启动，绑定从数据库消失，因此入站消息不再路由到 agent。
func (m *Manager) Unregister(channelType, accountID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.channels, channelKey(channelType, accountID))
}

// Start 启动所有渠道和出站消息路由器。
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.rootCtx = ctx
	chans := make(map[string]Channel, len(m.channels))
	singletons := make(map[string]bool, len(m.channels))
	for k, v := range m.channels {
		chans[k] = v
		_, singletons[k] = m.singleton[k]
	}
	leaser := m.leaser
	holderID := m.holderID
	m.mu.Unlock()

	var wg sync.WaitGroup

	// 启动出站路由器
	wg.Add(1)
	go func() {
		defer wg.Done()
		m.routeOutbound(ctx)
	}()

	// 启动每个渠道
	for key, ch := range chans {
		singleton := singletons[key]
		wg.Add(1)
		go func(k string, c Channel, s bool) {
			defer wg.Done()
			slog.Info("starting channel", "key", k, "singleton", s)
			if s {
				runWithLease(ctx, c, leaser, holderID)
				return
			}
			if err := c.Start(ctx); err != nil {
				slog.Error("channel stopped with error", "key", k, "error", err)
			}
		}(key, ch, singleton)
	}

	wg.Wait()
}

func (m *Manager) routeOutbound(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-m.bus.Outbound:
			key := channelKey(msg.Channel, msg.AccountID)
			m.mu.Lock()
			ch, ok := m.channels[key]
			m.mu.Unlock()
			if !ok {
				slog.Warn("unknown outbound channel", "key", key)
				continue
			}
			m.dispatchOutbound(ch, msg, key)
		}
	}
}

// dispatchOutbound 集中处理 SplitMessageMarker——每个渠道适配器在每次
// SendMessage 调用时看到一个逻辑消息，无论 agent 是否决定拆分。
//
//	AllowSplit && 存在标记     → 按标记拆分文本，顺序发送
//	                                （媒体 + 按钮附加到最后一个块，
//	                                所以它们只出现一次）
//	AllowSplit && 无标记        → 原样发送
//	!AllowSplit && 存在标记     → 先将标记折叠为换行，避免原始
//	                                `<|split|>` 标记在过期的系统提示
//	                                缓存中作为纯文本泄露
//	!AllowSplit && 无标记       → 原样发送
//
// 顺序派发由 routeOutbound 的单 goroutine 设计保证——块按顺序到达适配器。
func (m *Manager) dispatchOutbound(ch Channel, msg bus.OutboundMessage, key string) {
	hasMarker := strings.Contains(msg.Text, SplitMessageMarker)
	if !hasMarker {
		if err := ch.SendMessage(msg); err != nil {
			slog.Error("send message failed", "key", key, "error", err)
		}
		return
	}
	if !msg.AllowSplit {
		msg.Text = strings.ReplaceAll(msg.Text, SplitMessageMarker, "\n")
		if err := ch.SendMessage(msg); err != nil {
			slog.Error("send message failed", "key", key, "error", err)
		}
		return
	}
	chunks := SplitOutboundText(msg.Text)
	for i, chunk := range chunks {
		out := msg
		out.Text = chunk
		// 仅将媒体 + 按钮 + ReplyToMsgID + EditMsgID 附加到最后一个块。
		// 否则单个附件要么骑在第一个气泡上（文本跟在后面看起来很怪），
		// 要么在每个块上重新发送（肯定是错的）。
		if i < len(chunks)-1 {
			out.MediaItems = nil
			out.MediaPaths = nil
			out.Buttons = nil
			out.ReplyToMsgID = ""
			out.EditMsgID = ""
		}
		if err := ch.SendMessage(out); err != nil {
			slog.Error("send message failed", "key", key, "chunk", i, "error", err)
			// 在第一个错误时停止——在平台恢复后继续发送剩余气泡对聊天者
			// 来说可能看起来像重复。
			return
		}
	}
}

// BotUsername 返回给定 channel:accountID 对的 bot 用户名。
func (m *Manager) BotUsername(channel, accountID string) string {
	key := channelKey(channel, accountID)
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.channels[key]
	if !ok {
		return ""
	}
	return ch.BotUsername()
}

// SendTyping 为给定渠道和聊天发送输入指示器。
func (m *Manager) SendTyping(channel, accountID, chatID string) {
	key := channelKey(channel, accountID)
	m.mu.Lock()
	ch, ok := m.channels[key]
	m.mu.Unlock()
	if !ok {
		return
	}
	if err := ch.SendTyping(chatID); err != nil {
		slog.Debug("send typing failed", "key", key, "error", err)
	}
}

// Has 在给定 key 的渠道已注册时返回 true。
// 由处理程序用于短路冗余热启动。
func (m *Manager) Has(channel, accountID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.channels[channelKey(channel, accountID)]
	return ok
}

// Get 返回 (channel, accountID) 的已注册适配器，或 nil。
// 由飞书 webhook 处程序用于查找应分派入站事件的适配器——
// HTTP 路由接收原始 POST，需要根据 URL 路径中的 {accountId}
// （飞书 App ID）调用正确的 Feishu 实例的 HandleWebhook。
func (m *Manager) Get(channel, accountID string) Channel {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.channels[channelKey(channel, accountID)]
}

func channelKey(channel, accountID string) string {
	if accountID == "" {
		return channel + ":"
	}
	return channel + ":" + accountID
}
