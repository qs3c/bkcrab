package rag

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

// Hit is one cross-knowledge-base retrieval result.
type Hit struct {
	KBID         string   `json:"kbId"`
	KBName       string   `json:"kbName"`
	DocID        string   `json:"docId"`
	DocName      string   `json:"docName"`
	ChunkIndex   int      `json:"chunkIndex"`
	SectionTitle string   `json:"sectionTitle,omitempty"`
	PageNum      int      `json:"pageNum,omitempty"`
	Content      string   `json:"content"`
	Score        float64  `json:"score"`
	RecallScore  float64  `json:"recallScore"`
	RerankScore  *float64 `json:"rerankScore,omitempty"`
}

// SearchContext keeps the user's current question separate from the earlier
// questions that may be used by the retrieval pipeline's query-rewrite stage.
// Callers must not flatten History into Query themselves: doing so makes topic
// changes noisy and prevents the rewriter from deciding which clues matter.
type SearchContext struct {
	Query   string
	History []string
}

// Search performs hybrid retrieval across authorized KBs and merges their
// results by score. Every target is ownership-checked before any query runs.
func (s *Service) Search(ctx context.Context, ownerID string, kbIDs []string, query string, topN int) ([]Hit, error) {
	return s.SearchWithContext(ctx, ownerID, kbIDs, SearchContext{Query: query}, topN)
}

// SearchWithContext is the shared retrieval entry point for knowledge-base
// search, knowledge-base chat, and rag_search. A single LLM call rewrites the
// current query and creates a hypothetical document. The rewrite drives both
// BM25 and one dense route; HyDE drives a second dense route. If planning fails
// or omits HyDE, the identical dense inputs are deduplicated.
func (s *Service) SearchWithContext(ctx context.Context, ownerID string, kbIDs []string, input SearchContext, topN int) ([]Hit, error) {
	query := strings.TrimSpace(input.Query)
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
		dense [][]float32
	}
	kbs := make([]*store.RAGKBRecord, 0, len(kbIDs))
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
		kbs = append(kbs, kb)
	}
	if len(kbs) == 0 {
		return []Hit{}, nil
	}

	plan := s.planQuery(ctx, kbs[0].UserID, SearchContext{Query: query, History: input.History})
	targets := make([]target, 0, len(kbs))
	vectorCache := make(map[string][][]float32)
	for _, kb := range kbs {
		cacheKey := fmt.Sprintf("%s/%d", kb.EmbedModel, kb.EmbedDims)
		queryVectors, ok := vectorCache[cacheKey]
		if !ok {
			denseTexts := []string{plan.RewrittenQuery}
			if plan.HypotheticalDocument != plan.RewrittenQuery {
				denseTexts = append(denseTexts, plan.HypotheticalDocument)
			}
			vectors, err := s.embedderForKB(ctx, kb).Embed(ctx, denseTexts)
			if err != nil {
				return nil, fmt.Errorf("查询向量化(%s): %w", kb.EmbedModel, err)
			}
			if len(vectors) != len(denseTexts) {
				return nil, fmt.Errorf("查询向量化(%s): 返回向量数异常", kb.EmbedModel)
			}
			queryVectors = vectors
			vectorCache[cacheKey] = queryVectors
		}
		targets = append(targets, target{kb: kb, dense: queryVectors})
	}

	candidateTopK := s.cfg.Reranker.CandidateTopK
	if candidateTopK < topN {
		candidateTopK = topN
	}
	results := make([]Hit, 0, len(targets)*candidateTopK)
	for _, target := range targets {
		if err := s.vec.EnsureCollection(ctx, target.kb.ID, target.kb.EmbedDims); err != nil {
			return nil, fmt.Errorf("准备检索 %s: %w", target.kb.Name, err)
		}
		vectorHits, err := s.vec.HybridSearch(ctx, target.kb.ID, vector.SearchQuery{
			Dense: target.dense,
			Text:  plan.RewrittenQuery,
		}, candidateTopK)
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
				RecallScore:  hit.Score,
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
	if len(results) > candidateTopK {
		results = results[:candidateTopK]
	}
	if s.reranker != nil && len(results) > 0 {
		reranked, err := s.rerankHits(ctx, plan.RewrittenQuery, results, topN)
		if err == nil {
			return reranked, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn("rag: reranker failed; using RRF candidates", "error", err, "candidates", len(results))
	}
	if len(results) > topN {
		results = results[:topN]
	}
	return results, nil
}

// rerankHits replaces the public score with the normalized semantic score and
// applies the confidence threshold only after a successful reranker call. Any
// service or response error is returned to SearchWithContext, which falls back
// to the untouched RRF ordering without applying this threshold.
func (s *Service) rerankHits(ctx context.Context, query string, candidates []Hit, topN int) ([]Hit, error) {
	documents := make([]string, len(candidates))
	for index := range candidates {
		documents[index] = candidates[index].Content
	}
	ranked, err := s.reranker.Rerank(ctx, query, documents, topN)
	if err != nil {
		return nil, err
	}
	if len(ranked) == 0 {
		return nil, fmt.Errorf("reranker 返回空结果")
	}

	seen := make(map[int]struct{}, len(ranked))
	for _, item := range ranked {
		if item.Index < 0 || item.Index >= len(candidates) {
			return nil, fmt.Errorf("reranker 返回非法 index %d", item.Index)
		}
		if _, exists := seen[item.Index]; exists {
			return nil, fmt.Errorf("reranker 返回重复 index %d", item.Index)
		}
		if math.IsNaN(item.Score) || math.IsInf(item.Score, 0) || item.Score < 0 || item.Score > 1 {
			return nil, fmt.Errorf("reranker 返回非法分数 %v", item.Score)
		}
		seen[item.Index] = struct{}{}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].Score != ranked[j].Score {
			return ranked[i].Score > ranked[j].Score
		}
		return ranked[i].Index < ranked[j].Index
	})
	if len(ranked) > topN {
		ranked = ranked[:topN]
	}

	filtered := make([]Hit, 0, len(ranked))
	for _, item := range ranked {
		if item.Score < s.cfg.Reranker.MinScore {
			continue
		}
		hit := candidates[item.Index]
		score := item.Score
		hit.Score = score
		hit.RerankScore = &score
		filtered = append(filtered, hit)
	}
	return filtered, nil
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
