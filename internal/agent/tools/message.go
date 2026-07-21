package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/qs3c/bkcrab/internal/bus"
)

type messageArgs struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
	Text    string `json:"text"`
}

// RegisterMessage 将消息工具注册到给定的消息总线。
// 每次发送到邮票时都会咨询allowSplitFn（可选）
// OutboundMessage.AllowSplit — 控制微信适配器是否会
// 尊重 SplitMessageMarker 的多气泡输出。如果满足则传递 nil
// 调用者不在乎（例如测试、非微信绑定部署）-
// 在这种情况下，AllowSplit 默认为 false。
func RegisterMessage(r *Registry, mb *bus.MessageBus, allowSplitFn func() bool) {
	registered := r.tools["message"]
	registered.fn = resultHandlerFromToolFunc(makeMessageTool(mb, allowSplitFn))
	r.tools["message"] = registered
}

func registerMessage(r *Registry) {
	// 使用占位符注册；稍后将在实际总线上重新注册。
	r.Register("message", "Send a message to a channel", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"channel": map[string]interface{}{
				"type":        "string",
				"description": "Target channel (e.g. 'telegram')",
			},
			"chat_id": map[string]interface{}{
				"type":        "string",
				"description": "Target chat ID",
			},
			"text": map[string]interface{}{
				"type":        "string",
				"description": "Message text to send",
			},
		},
		"required": []string{"channel", "chat_id", "text"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		return "", fmt.Errorf("message bus not initialized")
	})
}

func makeMessageTool(mb *bus.MessageBus, allowSplitFn func() bool) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args messageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}

		allowSplit := false
		if allowSplitFn != nil {
			allowSplit = allowSplitFn()
		}

		mb.Outbound <- bus.OutboundMessage{
			Channel:    args.Channel,
			ChatID:     args.ChatID,
			Text:       args.Text,
			AllowSplit: allowSplit,
		}

		return "Message sent", nil
	}
}
