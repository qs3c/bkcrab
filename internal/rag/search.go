package rag

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/store"
)

// Hit is one cross-knowledge-base retrieval result.
type Hit struct {
	KBID         string  `json:"kbId"`
	KBName       string  `json:"kbName"`
	DocID        string  `json:"docId"`
	DocName      string  `json:"docName"`
	ChunkIndex   int     `json:"chunkIndex"`
	SectionTitle string  `json:"sectionTitle,omitempty"`
	PageNum      int     `json:"pageNum,omitempty"`
	Content      string  `json:"content"`
	Score        float64 `json:"score"`
}

// Search performs hybrid retrieval across authorized KBs and merges their
// results by score. Every target is ownership-checked before any query runs.
func (s *Service) Search(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) ([]Hit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("query 不能为空")
	}
	if topN <= 0 {
		topN = 5
	}
	if topN > 20 {
		topN = 20
	}

	type target struct {
		kb    *store.RAGKBRecord
		query []float32
	}
	targets := make([]target, 0, len(kbIDs))
	vectorCache := make(map[string][]float32)
	seenKB := make(map[string]struct{}, len(kbIDs))
	for _, kbID := range kbIDs {
		if _, exists := seenKB[kbID]; exists {
			continue
		}
		seenKB[kbID] = struct{}{}
		kb, err := s.GetKB(ctx, ownerID, kbID)
		if err != nil {
			return nil, err
		}
		if kb.Status != "active" {
			continue
		}
		cacheKey := fmt.Sprintf("%s/%d", kb.EmbedModel, kb.EmbedDims)
		queryVector, ok := vectorCache[cacheKey]
		if !ok {
			vectors, err := s.embedderForKB(ctx, kb).Embed(ctx, []string{query})
			if err != nil {
				return nil, fmt.Errorf("查询向量化(%s): %w", kb.EmbedModel, err)
			}
			if len(vectors) != 1 {
				return nil, fmt.Errorf("查询向量化(%s): 返回向量数异常", kb.EmbedModel)
			}
			queryVector = vectors[0]
			vectorCache[cacheKey] = queryVector
		}
		targets = append(targets, target{kb: kb, query: queryVector})
	}

	results := make([]Hit, 0, len(targets)*topN)
	for _, target := range targets {
		if err := s.vec.EnsureCollection(ctx, target.kb.ID, target.kb.EmbedDims); err != nil {
			return nil, fmt.Errorf("准备检索 %s: %w", target.kb.Name, err)
		}
		vectorHits, err := s.vec.HybridSearch(ctx, target.kb.ID, target.query, query, topN)
		if err != nil {
			return nil, fmt.Errorf("检索 %s: %w", target.kb.Name, err)
		}
		docs, err := s.st.ListRAGDocumentsByKB(ctx, target.kb.ID)
		if err != nil {
			return nil, err
		}
		docNames := make(map[string]string, len(docs))
		for _, doc := range docs {
			docNames[doc.ID] = doc.FileName
		}
		for _, hit := range vectorHits {
			docName, exists := docNames[hit.DocID]
			if !exists {
				// Vector deletion and relational deletion cannot be atomic across
				// stores. Never surface an orphaned chunk after its document row has
				// gone away; cleanup can then be retried without a data leak.
				continue
			}
			results = append(results, Hit{
				KBID:         target.kb.ID,
				KBName:       target.kb.Name,
				DocID:        hit.DocID,
				DocName:      docName,
				ChunkIndex:   hit.ChunkIndex,
				SectionTitle: hit.SectionTitle,
				PageNum:      hit.PageNum,
				Content:      hit.Content,
				Score:        hit.Score,
			})
		}
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		if results[i].KBID != results[j].KBID {
			return results[i].KBID < results[j].KBID
		}
		if results[i].DocID != results[j].DocID {
			return results[i].DocID < results[j].DocID
		}
		return results[i].ChunkIndex < results[j].ChunkIndex
	})
	if len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}

// FormatHits renders search results for the rag_search tool with an explicit
// citation header on every passage.
func FormatHits(hits []Hit) string {
	if len(hits) == 0 {
		return "未在授权的知识库中检索到相关内容。"
	}
	var out strings.Builder
	for i, hit := range hits {
		out.WriteString("[来源: ")
		if hit.DocName != "" {
			out.WriteString(hit.DocName)
		} else {
			out.WriteString(hit.DocID)
		}
		if hit.SectionTitle != "" {
			out.WriteString(" · ")
			out.WriteString(hit.SectionTitle)
		}
		if hit.PageNum > 0 {
			fmt.Fprintf(&out, " · 第%d页", hit.PageNum)
		}
		fmt.Fprintf(&out, " · chunk#%d · 知识库:%s]\n", hit.ChunkIndex, hit.KBName)
		out.WriteString(strings.TrimSpace(hit.Content))
		if i < len(hits)-1 {
			out.WriteString("\n\n---\n\n")
		}
	}
	return out.String()
}
