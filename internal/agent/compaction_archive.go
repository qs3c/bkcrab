package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

type contextArchiveStore interface {
	SaveContextArchive(ctx context.Context, rec *store.ContextArchiveRecord) error
}

func archiveToolResult(opts CompactOptions, msg provider.Message, info toolCallInfo) (string, error) {
	if opts.ArchiveStore == nil || opts.ArchiveAgentID == "" || opts.ArchiveSessionKey == "" || msg.Content == "" {
		return "", nil
	}
	toolName := info.Name
	if toolName == "" {
		toolName = msg.Name
	}
	archiveID := compactedToolArchiveID(opts.ArchiveAgentID, opts.ArchiveSessionKey, msg.ToolCallID, toolName, msg.Content)
	contentHash := sha256.Sum256([]byte(msg.Content))
	rec := &store.ContextArchiveRecord{
		ID:            archiveID,
		UserID:        opts.ArchiveUserID,
		AgentID:       opts.ArchiveAgentID,
		SessionKey:    opts.ArchiveSessionKey,
		ToolCallID:    msg.ToolCallID,
		ToolName:      toolName,
		Content:       msg.Content,
		ContentBytes:  len([]byte(msg.Content)),
		ContentSHA256: hex.EncodeToString(contentHash[:]),
		CreatedAt:     time.Now().UTC(),
	}
	if err := opts.ArchiveStore.SaveContextArchive(context.Background(), rec); err != nil {
		return "", err
	}
	return archiveID, nil
}

func compactedToolArchiveID(agentID, sessionKey, toolCallID, toolName, content string) string {
	sum := sha256.Sum256([]byte(agentID + "\x00" + sessionKey + "\x00" + toolCallID + "\x00" + toolName + "\x00" + content))
	return "ctxar_" + hex.EncodeToString(sum[:12])
}
