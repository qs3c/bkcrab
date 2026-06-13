package channels

import (
	"context"
	"sync"

	"github.com/qs3c/bkclaw/internal/bus"
)

// WebChannel 是 Web 聊天客户端的进程内扇出。它满足 Channel 接口，
// 因此 channels.Manager 可以像任何其他渠道一样通过它路由
// Channel="web" 的 bus.Outbound 消息——但它不是推送到外部服务
//（Telegram / Discord / Slack），而是转发到由 SSE 处理程序保持打开的
// 每 (agentID, sessionID) 订阅者。
//
// 这修复了 cron 触发回复时出现 "WARN unknown outbound channel key=web:" 的问题：
// cron 调度器在总线排队一个出站消息，channels 管理器找到注册在 "web:" 的
// WebChannel，WebChannel.SendMessage 扇出到订阅该聊天的浏览器标签。未订阅
// 的标签（用户关闭了页面）会静默丢弃——消息已经被 agent 循环保存在会话行中，
// 下次加载时用户可以看到。
type WebChannel struct {
	mu          sync.RWMutex
	subscribers map[string][]chan bus.OutboundMessage
}

// NewWebChannel 返回一个没有订阅者的全新 WebChannel。
func NewWebChannel() *WebChannel {
	return &WebChannel{
		subscribers: make(map[string][]chan bus.OutboundMessage),
	}
}

// Subscribe 注册一个 channel 来接收每条 (AgentID, ChatID) 匹配的
// OutboundMessage。返回该 channel 和一个清理函数，调用方必须 defer
// 调用以移除其槽位——没有它，切片会在重新连接时无限增长。
//
// 缓冲区大小有意设置得很小：cron 消息以人类速度到达，而非高频，
// 且落后比卡住客户端的无界内存增长更可取。丢弃在发送处记录。
func (w *WebChannel) Subscribe(agentID, chatID string) (<-chan bus.OutboundMessage, func()) {
	key := webKey(agentID, chatID)
	ch := make(chan bus.OutboundMessage, 8)
	w.mu.Lock()
	w.subscribers[key] = append(w.subscribers[key], ch)
	w.mu.Unlock()
	cleanup := func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		list := w.subscribers[key]
		for i, c := range list {
			if c == ch {
				w.subscribers[key] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(w.subscribers[key]) == 0 {
			delete(w.subscribers, key)
		}
		close(ch)
	}
	return ch, cleanup
}

// Name 返回 "web"。
func (w *WebChannel) Name() string { return "web" }

// AccountID 返回 "" —— web 是全局渠道，不是每 bot 的。
func (w *WebChannel) AccountID() string { return "" }

// BotUsername 返回 "" ——对 web 渠道不适用。
func (w *WebChannel) BotUsername() string { return "" }

// Start 阻塞直到 ctx 被取消。没有入站侧：web 聊天请求通过
// 仪表板 SSE / OpenAI 兼容端点传入，而非通过此渠道。
func (w *WebChannel) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Send 对 web 渠道未使用——出站投递总是通过承载完整 OutboundMessage
// 形状的 SendMessage 到达。为满足 Channel 接口而实现；混用调用方无法
// 发现特定会话。
func (w *WebChannel) Send(chatID, text string) error {
	return w.SendMessage(bus.OutboundMessage{
		Channel: "web",
		ChatID:  chatID,
		Text:    text,
	})
}

// SendMessage 将 msg 扇出到绑定到 (msg.AgentID, msg.ChatID) 的每个订阅者。
// 缓冲区已满的订阅者会被跳过（不会阻塞），这样单个卡住的客户端不会
// 停滞 cron 调度器。
func (w *WebChannel) SendMessage(msg bus.OutboundMessage) error {
	key := webKey(msg.AgentID, msg.ChatID)
	w.mu.RLock()
	subs := append([]chan bus.OutboundMessage(nil), w.subscribers[key]...)
	w.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- msg:
		default:
			// 缓冲区已满——客户端卡住；跳过而非阻塞。
		}
	}
	return nil
}

// SendTyping 对 web 是空操作——输入指示器由仪表板自身的 UI 状态驱动，
// 而非服务器信号。
func (w *WebChannel) SendTyping(chatID string) error { return nil }

func webKey(agentID, chatID string) string {
	return agentID + ":" + chatID
}
