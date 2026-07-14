package rag

import (
	"context"
	"log/slog"

	"github.com/qs3c/bkcrab/internal/agent/tools"
)

// SearchForAgent satisfies tools.RAGSearcher.
func (s *Service) SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error) {
	hits, err := s.Search(ctx, ownerID, kbIDs, query, topN)
	if err != nil {
		return "", err
	}
	return FormatHits(hits), nil
}

// ResolveAgentKBs resolves configured IDs into safe display references. Stale,
// cross-user, and deleting KB references are silently removed from the tool.
func (s *Service) ResolveAgentKBs(ctx context.Context, ownerID string, kbIDs []string) []tools.RAGKBRef {
	refs := make([]tools.RAGKBRef, 0, len(kbIDs))
	seen := make(map[string]struct{}, len(kbIDs))
	for _, kbID := range kbIDs {
		if _, ok := seen[kbID]; ok {
			continue
		}
		seen[kbID] = struct{}{}
		kb, err := s.GetKB(ctx, ownerID, kbID)
		if err != nil {
			slog.Warn("rag: removing unavailable KB from agent authorization", "kb", kbID, "owner", ownerID, "error", err)
			continue
		}
		if kb.Status != "active" {
			continue
		}
		refs = append(refs, tools.RAGKBRef{ID: kb.ID, Name: kb.Name, Description: kb.Description})
	}
	return refs
}
