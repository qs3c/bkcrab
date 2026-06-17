package agent

import (
	"encoding/json"

	"github.com/qs3c/bkclaw/internal/provider"
)

func sanitizeToolPairs(messages []provider.Message) []provider.Message {
	sanitized, _ := sanitizeToolPairsWithChange(messages)
	return sanitized
}

func sanitizeToolPairsWithChange(messages []provider.Message) ([]provider.Message, bool) {
	out := make([]provider.Message, 0, len(messages))
	changed := false

	for i := 0; i < len(messages); {
		msg := messages[i]
		if msg.Role == "tool" {
			changed = true
			i++
			continue
		}

		if msg.Role != "assistant" {
			out = append(out, msg)
			i++
			continue
		}

		expected := expectedToolCallIDs(msg)
		if len(expected) == 0 {
			out = append(out, msg)
			i++
			continue
		}

		j := i + 1
		for j < len(messages) && messages[j].Role == "tool" {
			j++
		}

		seen := make(map[string]struct{}, len(expected))
		tools := make([]provider.Message, 0, len(expected))
		for k := i + 1; k < j; k++ {
			tool := messages[k]
			if _, ok := expected[tool.ToolCallID]; !ok {
				changed = true
				continue
			}
			if _, ok := seen[tool.ToolCallID]; ok {
				changed = true
				continue
			}
			seen[tool.ToolCallID] = struct{}{}
			tools = append(tools, tool)
		}

		complete := len(seen) == len(expected)
		if complete {
			out = append(out, msg)
			out = append(out, tools...)
			i = j
			continue
		}

		changed = true
		if assistantHasText(msg) {
			msg.ToolCalls = nil
			msg.RawAssistant = nil
			out = append(out, msg)
		}
		i = j
	}

	if !changed {
		return messages, false
	}
	return out, true
}

func assistantHasText(msg provider.Message) bool {
	return msg.Content != "" || len(msg.ContentParts) > 0 || msg.Thinking != ""
}

func expectedToolCallIDs(msg provider.Message) map[string]struct{} {
	ids := make(map[string]struct{}, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if tc.ID != "" {
			ids[tc.ID] = struct{}{}
		}
	}
	for _, id := range rawAssistantToolCallIDs(msg.RawAssistant) {
		ids[id] = struct{}{}
	}
	return ids
}

func rawAssistantToolCallIDs(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var msg struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}
	ids := make([]string, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		if tc.ID != "" {
			ids = append(ids, tc.ID)
		}
	}
	return ids
}
