package rag

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/qs3c/bkcrab/internal/agent/tools"
)

const maxAgentRAGResources = 6

// SearchForAgent satisfies tools.RAGSearcher. The model-visible text remains
// URL-free; resource references travel only through the registry-validated
// typed result side channel.
func (s *Service) SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (tools.ToolResult, error) {
	hits, err := s.Search(ctx, ownerID, kbIDs, query, topN)
	if err != nil {
		return tools.ToolResult{}, err
	}
	return agentToolResult(hits), nil
}

func agentToolResult(hits []Hit) tools.ToolResult {
	result := tools.ToolResult{Text: FormatHits(hits)}
	refs := BuildRAGResourceRefs(hits)
	if len(refs) == 0 {
		return result
	}
	if len(refs) > maxAgentRAGResources {
		refs = refs[:maxAgentRAGResources]
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		// Every field in RAGResourceRef has a deterministic JSON encoding. Keep
		// the text result usable if that contract ever changes unexpectedly.
		slog.Warn("rag: encode agent resource metadata", "error", err)
		return result
	}
	result.Metadata = tools.ResultMetadata{tools.RAGResourcesMetadataKey: raw}
	return result
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
