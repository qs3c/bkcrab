package agent

import (
	"context"
	"sync"
)

// EventEnvelope 是一个带有持久 seq 标记的 ChatEvent
// 存储在附加时分配的。订阅者使用 Seq 进行重复数据删除
// 他们已经通过 ListSessionEventsSince 重播的事件。
type EventEnvelope struct {
	Seq   int64
	Event ChatEvent
}

// EventHub 是实时聊天事件的进程内发布/订阅。订阅者
// （SSE 聊天订阅处理程序）按（userID、agentID、
// 会话密钥）；发布者（在代理循环上发出事件，扇出到
// 集线器）推送包含持久序列的信封，以便重新连接
// 简历可以干净地拼接在一起。
//
// 仅内存中 — 多 Pod 部署需要将其交换为 Redis
// pub/sub 或类似的（与 WebChannel 限制相同的形状，称为
// 出到其他地方）。
type EventHub struct {
	mu   sync.RWMutex
	subs map[string][]chan EventEnvelope
}

// NewEventHub 返回一个空集线器。
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[string][]chan EventEnvelope)}
}

// 订阅为一个人（用户、代理、
// 会话）元组。清理函数必须被推迟——没有它
// hub 在重新连接时会泄漏 goroutine 和通道。
func (h *EventHub) Subscribe(userID, agentID, sessionKey string) (<-chan EventEnvelope, func()) {
	key := hubKey(userID, agentID, sessionKey)
	ch := make(chan EventEnvelope, 32)
	h.mu.Lock()
	h.subs[key] = append(h.subs[key], ch)
	h.mu.Unlock()
	cleanup := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		list := h.subs[key]
		for i, c := range list {
			if c == ch {
				h.subs[key] = append(list[:i], list[i+1:]...)
				close(ch)
				break
			}
		}
		if len(h.subs[key]) == 0 {
			delete(h.subs, key)
		}
	}
	return ch, cleanup
}

// Publish 向每个当前订阅者分发一个信封。慢的
// 消费者（完整的缓冲区）被跳过，而不是被阻止——一个卡住的客户端
// 无法阻止代理循环。
func (h *EventHub) Publish(userID, agentID, sessionKey string, env EventEnvelope) {
	key := hubKey(userID, agentID, sessionKey)
	h.mu.RLock()
	subs := append([]chan EventEnvelope(nil), h.subs[key]...)
	h.mu.RUnlock()
	for _, ch := range subs {
		select {
		case ch <- env:
		default:
		}
	}
}

func hubKey(userID, agentID, sessionKey string) string {
	return userID + "/" + agentID + "/" + sessionKey
}

// EventSink 是聊天事件管道的持久性部分。这
// store.Store接口的AppendSessionEvent正好满足这一点，所以
// 网关可以按原样传递其存储。
type EventSink interface {
	AppendSessionEvent(ctx context.Context, userID, agentID, sessionKey, eventType string, data []byte) (int64, error)
}

// StreamCtx 携带的每轮发射事件句柄达到：
// 旧的内存中 ChatEvent 通道（由 handleChatStream 使用
// 当客户端连接时）、持久接收器、集线器和
// 地址键（us​​erID、agentID、sessionKey）——最后三个
// 无法从代理结构派生，因为代理运行在
// 代表聊天者，而不是其所有者。
type streamCtx struct {
	channel    chan<- ChatEvent
	sink       EventSink
	hub        *EventHub
	userID     string
	agentID    string
	sessionKey string
}

type streamCtxKey struct{}

// ContextWithStream 将流管道附加到 ctx。发出事件
// 读取它并保留/发布/转发到旧通道
// 在一个地方。
func ContextWithStream(ctx context.Context, channel chan<- ChatEvent, sink EventSink, hub *EventHub, userID, agentID, sessionKey string) context.Context {
	return context.WithValue(ctx, streamCtxKey{}, &streamCtx{
		channel:    channel,
		sink:       sink,
		hub:        hub,
		userID:     userID,
		agentID:    agentID,
		sessionKey: sessionKey,
	})
}

func streamFromContext(ctx context.Context) *streamCtx {
	s, _ := ctx.Value(streamCtxKey{}).(*streamCtx)
	return s
}
