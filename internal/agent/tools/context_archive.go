package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/store"
)

type retrieveCompactedToolResultArgs struct {
	ID string `json:"id"`
}

func registerContextArchive(r *Registry) {
	r.Register(
		"retrieve_compacted_tool_result",
		"Retrieve the exact original output for a compacted historical tool result by archive id.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "string",
					"description": "The archive_id shown in a compacted tool-result summary.",
				},
			},
			"required": []string{"id"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args retrieveCompactedToolResultArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			id := strings.TrimSpace(args.ID)
			if id == "" {
				return "", errors.New("id is required")
			}
			if r.contextArchiveStore == nil {
				return "", errors.New("context archive store is unavailable")
			}
			sessionKey := r.contextArchiveSessionKey
			if sessionKey == "" {
				sessionKey = r.sessionID
			}
			if r.agentID == "" || sessionKey == "" {
				return "", errors.New("context archive scope is unavailable")
			}
			rec, err := r.contextArchiveStore.GetContextArchive(ctx, r.agentID, sessionKey, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return "", fmt.Errorf("compacted tool result %q not found in this session", id)
				}
				return "", err
			}
			var b strings.Builder
			b.WriteString("[Compacted Tool Result]\n")
			b.WriteString("archive_id: ")
			b.WriteString(rec.ID)
			b.WriteByte('\n')
			if rec.ToolName != "" {
				b.WriteString("tool: ")
				b.WriteString(rec.ToolName)
				b.WriteByte('\n')
			}
			if rec.ToolCallID != "" {
				b.WriteString("tool_call_id: ")
				b.WriteString(rec.ToolCallID)
				b.WriteByte('\n')
			}
			b.WriteString("content_bytes: ")
			b.WriteString(fmt.Sprint(rec.ContentBytes))
			b.WriteString("\n\n")
			b.WriteString(rec.Content)
			return b.String(), nil
		},
	)
}
