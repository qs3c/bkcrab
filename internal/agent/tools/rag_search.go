package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// RAGSearcher is the narrow interface exposed by the RAG service to the agent
// tool layer. The returned string is already formatted with source citations.
type RAGSearcher interface {
	SearchForAgent(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) (string, error)
}

// RAGKBRef is the display information for a knowledge base that has already
// been authorized for an agent.
type RAGKBRef struct {
	ID          string
	Name        string
	Description string
}

// RegisterRAGSearch installs the read-only rag_search tool. Agents without an
// authorized knowledge base do not see the tool at all.
func RegisterRAGSearch(r *Registry, svc RAGSearcher, ownerID string, kbs []RAGKBRef, defaultTopN int) {
	if r == nil || svc == nil || len(kbs) == 0 {
		return
	}
	if defaultTopN <= 0 {
		defaultTopN = 5
	}
	if defaultTopN > 20 {
		defaultTopN = 20
	}

	kbLines := make([]string, 0, len(kbs))
	byName := make(map[string]string, len(kbs))
	allIDs := make([]string, 0, len(kbs))
	for _, kb := range kbs {
		if strings.TrimSpace(kb.ID) == "" || strings.TrimSpace(kb.Name) == "" {
			continue
		}
		desc := kb.Name
		if kb.Description != "" {
			desc += ": " + kb.Description
		}
		kbLines = append(kbLines, desc)
		byName[kb.Name] = kb.ID
		allIDs = append(allIDs, kb.ID)
	}
	if len(allIDs) == 0 {
		return
	}

	description := "Search the user's knowledge bases and return the most relevant passages with source citations. " +
		"Read-only. Available knowledge bases: " + strings.Join(kbLines, "; ") + ". " +
		"Use when the question may be answered by these documents."
	r.Register("rag_search", description, map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "自然语言检索词。用与文档同语言的关键词效果最好。",
			},
			"kb": map[string]any{
				"type":        "string",
				"description": "可选。只查这个名字的知识库；缺省查全部已授权知识库。",
			},
			"top_n": map[string]any{
				"type":        "integer",
				"description": fmt.Sprintf("返回条数，默认 %d，最大 20。", defaultTopN),
			},
		},
		"required": []string{"query"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args struct {
			Query string `json:"query"`
			KB    string `json:"kb"`
			TopN  int    `json:"top_n"`
		}
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		args.Query = strings.TrimSpace(args.Query)
		if args.Query == "" {
			return "", fmt.Errorf("query 不能为空")
		}

		ids := append([]string(nil), allIDs...)
		if args.KB != "" {
			id, ok := byName[args.KB]
			if !ok {
				names := make([]string, 0, len(byName))
				for name := range byName {
					names = append(names, name)
				}
				sort.Strings(names)
				return "", fmt.Errorf("知识库 %q 不存在或未授权，可用: %s", args.KB, strings.Join(names, ", "))
			}
			ids = []string{id}
		}
		topN := args.TopN
		if topN <= 0 {
			topN = defaultTopN
		}
		if topN > 20 {
			topN = 20
		}
		return svc.SearchForAgent(ctx, ownerID, ids, args.Query, topN)
	})
}
