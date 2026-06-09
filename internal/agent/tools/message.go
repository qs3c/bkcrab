package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastclaw-ai/fastclaw/internal/bus"
)

type messageArgs struct {
	Channel string `json:"channel"`
	ChatID  string `json:"chat_id"`
	Text    string `json:"text"`
}

// RegisterMessage registers the message tool with the given message bus.
// allowSplitFn (optional) is consulted on every send to stamp
// OutboundMessage.AllowSplit — controls whether the WeChat adapter will
// honor SplitMessageMarker for multi-bubble output. Pass nil if the
// caller doesn't care (e.g. tests, non-WeChat-bound deployments) —
// AllowSplit defaults to false in that case.
func RegisterMessage(r *Registry, mb *bus.MessageBus, allowSplitFn func() bool) {
	r.tools["message"] = registeredTool{
		def: r.tools["message"].def,
		fn:  makeMessageTool(mb, allowSplitFn),
	}
}

func registerMessage(r *Registry) {
	// Register with a placeholder; will be re-registered with actual bus later.
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
