package agent

import (
	"context"
	"encoding/json"
	"log/slog"
)

// ChatEvent 表示代理 ReAct 循环期间发出的实时事件。
type ChatEvent struct {
	Type string         `json:"type"` // "content", "content_delta", "tool_call", "tool_result", "steer", "error", "usage", "done", "turn_pending", "subagent_progress", "compaction"
	Data map[string]any `json:"data,omitempty"`
}

type chatEventsKey struct{}

// ChatEventsFromContext 从上下文中检索事件通道（如果存在）。
//
// 已弃用：更喜欢携带持久性的 ContextWithStream
// 传统通道旁边的接收器+集线器。仅保留给来电者
// 需要通道（测试、简单的非持久流）。
func ChatEventsFromContext(ctx context.Context) chan<- ChatEvent {
	ch, _ := ctx.Value(chatEventsKey{}).(chan<- ChatEvent)
	return ch
}

// ContextWithChatEvents 返回一个附加了事件通道的新上下文。
//
// 已弃用：更喜欢 ContextWithStream，因此事件也持久化 + 发布
// 到集线器以重新连接时恢复。
func ContextWithChatEvents(ctx context.Context, ch chan<- ChatEvent) context.Context {
	return context.WithValue(ctx, chatEventsKey{}, ch)
}

// assistantContentEvent builds the canonical final-content event shape used by
// both regular and fallback delivery paths.
func assistantContentEvent(content string, metadata map[string]any) ChatEvent {
	data := map[string]any{"content": content}
	if len(metadata) > 0 {
		data["metadata"] = metadata
	}
	return ChatEvent{Type: "content", Data: data}
}

// emitAssistantMetadataEvent publishes metadata after a true provider stream
// has already delivered the answer bytes. The empty content prevents clients
// from duplicating the answer while making live SSE and history reload agree.
func emitAssistantMetadataEvent(ctx context.Context, metadata map[string]any) {
	if len(metadata) == 0 {
		return
	}
	emitEvent(ctx, assistantContentEvent("", metadata))
}

// emitEvent 将一个事件分发给在 ctx 上注册的每个消费者：
// - 持久接收器（session_events 表）- 分配一个使用的 seq
// 重新连接客户端以删除重播事件
// - 进程内中心（跨选项卡/处理程序的实时订阅者）
// - 遗留通道（同步 SSE 处理程序仍然存在）
// 保持请求开放）
//
// 持久化是尽力而为，但登录失败——数据库故障
// 不应该杀死回合。集线器发布永不阻塞（全缓冲
// 订阅者被跳过）。传统频道致以敬意
// ctx.Done() 这样代理 goroutine 在通道运行时不会泄漏
// 消费者消失了，但是代理ctx被取消。
func emitEvent(ctx context.Context, evt ChatEvent) {
	stream := streamFromContext(ctx)

	var seq int64 = -1
	// 跳过大容量直播活动的持久性。内容增量
	// 流 ~ 每个生成的令牌一个块（每轮 100 多行
	// 谦虚的答案），这将使其余的 session_events 相形见绌
	// 没有重播价值：尾随的“内容”事件携带完整的内容
	// 最终文本，因此在转弯过程中刷新即可重新加入
	// 现场中心并在完成后获得最终结果。
	persist := evt.Type != "content_delta"

	if persist && stream != nil && stream.sink != nil && stream.userID != "" && stream.sessionKey != "" {
		blob, _ := json.Marshal(evt.Data)
		s, err := stream.sink.AppendSessionEvent(ctx, stream.userID, stream.agentID, stream.sessionKey, evt.Type, blob)
		if err != nil {
			slog.Warn("persist chat event failed",
				"agent", stream.agentID, "session", stream.sessionKey,
				"type", evt.Type, "error", err)
		} else {
			seq = s
		}
	}

	if stream != nil && stream.hub != nil && stream.userID != "" && stream.sessionKey != "" {
		stream.hub.Publish(stream.userID, stream.agentID, stream.sessionKey, EventEnvelope{Seq: seq, Event: evt})
	}

	// 传统通道路径：更喜欢在streamCtx上保存的通道（由
	// 新的 SSE 处理程序）；回退到已弃用的 chatEventsKey
	// 尚未迁移的呼叫者的通道。
	var ch chan<- ChatEvent
	if stream != nil {
		ch = stream.channel
	}
	if ch == nil {
		ch = ChatEventsFromContext(ctx)
	}
	if ch == nil {
		return
	}
	select {
	case ch <- evt:
	case <-ctx.Done():
	}
}
