package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/rag/chunktext"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

// Hit is one cross-knowledge-base retrieval result.
type Hit struct {
	KBID           string                  `json:"kbId"`
	KBName         string                  `json:"kbName"`
	DocID          string                  `json:"docId"`
	DocName        string                  `json:"docName"`
	ChunkIndex     int                     `json:"chunkIndex"`
	SectionTitle   string                  `json:"sectionTitle,omitempty"`
	PageNum        int                     `json:"pageNum,omitempty"`
	SourceLocation document.SourceLocation `json:"sourceLocation"`
	Content        string                  `json:"content"`
	Enhancement    string                  `json:"enhancement,omitempty"`
	Assets         []document.AssetRef     `json:"assets,omitempty"`
	Score          float64                 `json:"score"`
	RecallScore    float64                 `json:"recallScore"`
	RerankScore    *float64                `json:"rerankScore,omitempty"`

	DocVersion         int64  `json:"-"`
	SearchContent      string `json:"-"`
	IndexFormatVersion int    `json:"-"`
}

func (h Hit) AnswerText() string { return chunktext.Answer(h.Content, h.Enhancement) }

type RAGResourceRef struct {
	Asset          document.AssetRef       `json:"asset"`
	KBID           string                  `json:"kbId"`
	KBName         string                  `json:"kbName"`
	DocID          string                  `json:"docId"`
	DocName        string                  `json:"docName"`
	ChunkIndex     int                     `json:"chunkIndex"`
	SectionTitle   string                  `json:"sectionTitle,omitempty"`
	SourceLocation document.SourceLocation `json:"sourceLocation"`
}

func BuildRAGResourceRefs(hits []Hit) []RAGResourceRef {
	seen := make(map[string]struct{})
	refs := make([]RAGResourceRef, 0)
	for _, hit := range hits {
		for _, asset := range hit.Assets {
			resourceKey := asset.ID
			if asset.Attachment != nil {
				resourceKey += "\x00" + asset.Attachment.ID
			}
			if _, exists := seen[resourceKey]; exists {
				continue
			}
			seen[resourceKey] = struct{}{}
			location := asset.Location
			if location.Kind == "" {
				location = hit.SourceLocation
			}
			refs = append(refs, RAGResourceRef{
				Asset: asset, KBID: hit.KBID, KBName: hit.KBName,
				DocID: hit.DocID, DocName: hit.DocName, ChunkIndex: hit.ChunkIndex,
				SectionTitle: hit.SectionTitle, SourceLocation: location,
			})
		}
	}
	return refs
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
	return s.searchWithContext(ctx, ownerID, kbIDs, input, topN, false)
}

func (s *Service) searchWithContext(ctx context.Context, ownerID string, kbIDs []string, input SearchContext, topN int, retried bool) ([]Hit, error) {
	retrievalID := uuid.NewString()
	started := time.Now()
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
	activeOwners := make(map[string]bool, len(kbIDs))
	for _, kbID := range kbIDs {
		if _, exists := seenKB[kbID]; exists {
			continue
		}
		seenKB[kbID] = struct{}{}
		kb, err := s.GetKB(ctx, ownerID, kbID)
		if err != nil {
			return nil, err
		}
		// ownerID is only the caller's ownership fence. Platform admins use an
		// empty ownerID, so retrieval availability must always be derived from
		// the KB's actual owner. Cache the result for cross-KB searches while
		// treating a missing/deleting owner as a durable fail-closed tombstone.
		ownerActive, checked := activeOwners[kb.UserID]
		if !checked {
			user, userErr := s.st.GetUser(ctx, kb.UserID)
			switch {
			case userErr == nil:
				ownerActive = strings.EqualFold(user.Status, "active")
			case errors.Is(userErr, store.ErrNotFound):
				ownerActive = false
			default:
				return nil, userErr
			}
			activeOwners[kb.UserID] = ownerActive
		}
		if !ownerActive {
			continue
		}
		if kb.Status != "active" {
			continue
		}
		kbs = append(kbs, kb)
	}
	if len(kbs) == 0 {
		return []Hit{}, nil
	}

	plan := s.planQuery(ctx, retrievalID, kbs[0].UserID, SearchContext{Query: query, History: input.History})
	targets := make([]target, 0, len(kbs))
	vectorCache := make(map[string][][]float32)
	for _, kb := range kbs {
		embeddingCfg, err := s.embeddingConfigForKB(ctx, kb)
		if err != nil {
			return nil, err
		}
		cacheKey := embeddingContractFingerprintForKB(kb, embeddingCfg)
		queryVectors, ok := vectorCache[cacheKey]
		if !ok {
			denseTexts := []string{plan.RewrittenQuery}
			if plan.HypotheticalDocument != plan.RewrittenQuery {
				denseTexts = append(denseTexts, plan.HypotheticalDocument)
			}
			embedder := embed.New(embeddingCfg.Endpoint, embeddingCfg.APIKey, kb.EmbedModel, kb.EmbedDims)
			vectors, err := embedder.Embed(ctx, denseTexts)
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
		docs, err := s.st.ListRAGDocumentsByKB(ctx, target.kb.ID)
		if err != nil {
			return nil, err
		}
		docByID := make(map[string]store.RAGDocumentRecord, len(docs))
		activeVersions := make(map[string]int64, len(docs))
		for _, doc := range docs {
			if doc.ActiveVersion <= 0 || strings.EqualFold(doc.Status, "deleting") {
				continue
			}
			docByID[doc.ID] = doc
			activeVersions[doc.ID] = doc.ActiveVersion
		}
		if len(activeVersions) == 0 {
			continue
		}
		if err := s.vec.EnsureCollection(ctx, target.kb.ID, target.kb.EmbedDims); err != nil {
			return nil, fmt.Errorf("准备检索 %s: %w", target.kb.Name, err)
		}
		vectorHits, err := s.vec.HybridSearch(ctx, target.kb.ID, vector.SearchQuery{
			Dense: target.dense, Text: plan.RewrittenQuery,
			ActiveVersions: activeVersions, MaxFilterBytes: s.cfg.Limits.MaxMilvusFilterBytes,
		}, candidateTopK)
		if err != nil {
			return nil, fmt.Errorf("检索 %s: %w", target.kb.Name, err)
		}
		refs := make([]store.RAGChunkRef, 0, len(vectorHits))
		filteredVectorHits := make([]vector.SearchHit, 0, len(vectorHits))
		for _, hit := range vectorHits {
			doc, exists := docByID[hit.DocID]
			if !exists || doc.ActiveVersion != hit.DocVersion {
				continue
			}
			filteredVectorHits = append(filteredVectorHits, hit)
			refs = append(refs, store.RAGChunkRef{DocID: hit.DocID, DocVersion: hit.DocVersion, ChunkIndex: hit.ChunkIndex})
		}
		catalog, err := s.st.ListRAGChunksByRefs(ctx, refs)
		if err != nil {
			return nil, err
		}
		catalogByRef := make(map[string]store.RAGChunkRecord, len(catalog))
		for _, chunk := range catalog {
			catalogByRef[ragChunkKey(chunk.DocID, chunk.DocVersion, chunk.ChunkIndex)] = chunk
		}
		activeChanged := false
		for _, hit := range filteredVectorHits {
			doc := docByID[hit.DocID]
			chunk, exists := catalogByRef[ragChunkKey(hit.DocID, hit.DocVersion, hit.ChunkIndex)]
			if !exists && doc.IndexFormatVersion != 0 {
				current, lookupErr := s.st.GetRAGDocument(ctx, hit.DocID)
				if lookupErr == nil && current.ActiveVersion != doc.ActiveVersion {
					activeChanged = true
				}
				continue
			}
			content := hit.Content
			searchContent := hit.SearchContent
			sectionTitle := hit.SectionTitle
			pageNum := hit.PageNum
			location := sourceLocationFromPage(pageNum)
			enhancement := ""
			if exists {
				content = chunk.RawContent
				enhancement = chunk.Enhancement
				searchContent = chunk.SearchContent
				sectionTitle = chunk.SectionTitle
				location = decodeSourceLocation(chunk.LocationJSON, pageNum)
				if location.Kind == document.LocationPage {
					pageNum = location.Index
				}
			}
			results = append(results, Hit{
				KBID:               target.kb.ID,
				KBName:             target.kb.Name,
				DocID:              hit.DocID,
				DocName:            doc.FileName,
				ChunkIndex:         hit.ChunkIndex,
				SectionTitle:       sectionTitle,
				PageNum:            pageNum,
				SourceLocation:     location,
				Content:            content,
				Enhancement:        enhancement,
				Score:              hit.Score,
				RecallScore:        hit.Score,
				DocVersion:         hit.DocVersion,
				SearchContent:      searchContent,
				IndexFormatVersion: doc.IndexFormatVersion,
			})
		}
		if activeChanged && !retried {
			return s.searchWithContext(ctx, ownerID, kbIDs, input, topN, true)
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
	candidateCount := len(results)
	if s.reranker != nil && len(results) > 0 {
		reranked, err := s.rerankHits(ctx, retrievalID, plan.RewrittenQuery, results, topN)
		if err == nil {
			slog.Info("rag: retrieval completed",
				"retrieval_id", retrievalID,
				"owner", ownerID,
				"knowledge_bases", len(kbs),
				"dense_routes", len(targets[0].dense),
				"candidates", candidateCount,
				"returned", len(reranked),
				"reranked", true,
				"duration_ms", time.Since(started).Milliseconds(),
			)
			return s.hydrateHitAssets(ctx, reranked)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		slog.Warn("rag: reranker failed; using RRF candidates",
			"retrieval_id", retrievalID,
			"error", err,
			"candidates", len(results),
		)
	}
	if len(results) > topN {
		results = results[:topN]
	}
	slog.Info("rag: retrieval completed",
		"retrieval_id", retrievalID,
		"owner", ownerID,
		"knowledge_bases", len(kbs),
		"dense_routes", len(targets[0].dense),
		"candidates", candidateCount,
		"returned", len(results),
		"reranked", false,
		"reranker_configured", s.reranker != nil,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	return s.hydrateHitAssets(ctx, results)
}

// rerankHits replaces the public score with the normalized semantic score and
// applies the confidence threshold only after a successful reranker call. Any
// service or response error is returned to SearchWithContext, which falls back
// to the untouched RRF ordering without applying this threshold.
func (s *Service) rerankHits(ctx context.Context, retrievalID, query string, candidates []Hit, topN int) ([]Hit, error) {
	started := time.Now()
	documents := make([]string, len(candidates))
	for index := range candidates {
		documents[index] = candidates[index].SearchContent
		if documents[index] == "" {
			documents[index] = chunktext.Search(candidates[index].SectionTitle, candidates[index].AnswerText())
		}
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
	topScore := ranked[0].Score
	lowestReturnedScore := float64(0)
	if len(filtered) > 0 && filtered[len(filtered)-1].RerankScore != nil {
		lowestReturnedScore = *filtered[len(filtered)-1].RerankScore
	}
	slog.Info("rag: reranker applied",
		"retrieval_id", retrievalID,
		"query_hash", retrievalFingerprint(query),
		"candidates", len(candidates),
		"requested_top_n", topN,
		"ranked", len(ranked),
		"returned", len(filtered),
		"filtered", len(ranked)-len(filtered),
		"min_score", s.cfg.Reranker.MinScore,
		"top_score", topScore,
		"lowest_returned_score", lowestReturnedScore,
		"duration_ms", time.Since(started).Milliseconds(),
	)
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
		out.WriteString(strings.TrimSpace(hit.AnswerText()))
		if i < len(hits)-1 {
			out.WriteString("\n\n---\n\n")
		}
	}
	return out.String()
}

func ragChunkKey(docID string, version int64, chunkIndex int) string {
	return fmt.Sprintf("%s\x00%d\x00%d", docID, version, chunkIndex)
}

func sourceLocationFromPage(page int) document.SourceLocation {
	if page > 0 {
		return document.SourceLocation{Kind: document.LocationPage, Index: page, Label: fmt.Sprintf("第 %d 页", page)}
	}
	return document.SourceLocation{Kind: document.LocationDocument}
}

func decodeSourceLocation(raw string, fallbackPage int) document.SourceLocation {
	var location document.SourceLocation
	if json.Unmarshal([]byte(raw), &location) == nil && location.Kind != "" && location.Validate() == nil {
		return location
	}
	var legacy struct {
		PageNum int `json:"pageNum"`
	}
	if json.Unmarshal([]byte(raw), &legacy) == nil && legacy.PageNum > 0 {
		return sourceLocationFromPage(legacy.PageNum)
	}
	return sourceLocationFromPage(fallbackPage)
}

func (s *Service) hydrateHitAssets(ctx context.Context, hits []Hit) ([]Hit, error) {
	if len(hits) == 0 {
		return hits, nil
	}
	refs := make([]store.RAGChunkRef, 0, len(hits))
	for _, hit := range hits {
		if hit.IndexFormatVersion == 0 {
			continue
		}
		refs = append(refs, store.RAGChunkRef{DocID: hit.DocID, DocVersion: hit.DocVersion, ChunkIndex: hit.ChunkIndex})
	}
	if len(refs) == 0 {
		return hits, nil
	}
	mappings, err := s.st.ListRAGChunkAssetsByRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	assetIDs := make([]string, 0, len(mappings))
	attachmentIDs := make([]string, 0, len(mappings))
	seen := make(map[string]struct{}, len(mappings))
	seenAttachments := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		if mapping.AttachmentID != "" {
			if _, exists := seenAttachments[mapping.AttachmentID]; !exists {
				seenAttachments[mapping.AttachmentID] = struct{}{}
				attachmentIDs = append(attachmentIDs, mapping.AttachmentID)
			}
		}
		if _, exists := seen[mapping.AssetID]; exists {
			continue
		}
		seen[mapping.AssetID] = struct{}{}
		assetIDs = append(assetIDs, mapping.AssetID)
	}
	assets := make([]store.RAGAssetRecord, 0, len(assetIDs))
	for start := 0; start < len(assetIDs); start += pipelineStageBatchSize {
		end := min(start+pipelineStageBatchSize, len(assetIDs))
		batch, err := s.st.ListRAGAssetsByIDs(ctx, assetIDs[start:end])
		if err != nil {
			return nil, err
		}
		assets = append(assets, batch...)
	}
	assetByID := make(map[string]store.RAGAssetRecord, len(assets))
	for _, asset := range assets {
		if asset.DisplayStatus == document.DisplayReady && safeDisplayMIME(asset.DisplayMIME) &&
			canonicalSHA256(asset.DisplaySHA256) && canonicalSHA256(asset.ThumbnailSHA256) &&
			strings.TrimSpace(asset.DisplayObjectKey) != "" && strings.TrimSpace(asset.ThumbnailObjectKey) != "" {
			assetByID[asset.ID] = asset
		}
	}
	attachments := make([]store.RAGAttachmentRecord, 0, len(attachmentIDs))
	for start := 0; start < len(attachmentIDs); start += pipelineStageBatchSize {
		end := min(start+pipelineStageBatchSize, len(attachmentIDs))
		batch, err := s.st.ListRAGAttachmentsByIDs(ctx, attachmentIDs[start:end])
		if err != nil {
			return nil, err
		}
		attachments = append(attachments, batch...)
	}
	attachmentByID := make(map[string]store.RAGAttachmentRecord, len(attachments))
	for _, attachment := range attachments {
		if attachment.Kind == document.AttachmentKindVisioSource &&
			attachment.MIMEType == document.MIMETypeVSDX &&
			canonicalSHA256(attachment.ContentSHA256) &&
			strings.TrimSpace(attachment.ObjectKey) != "" &&
			attachment.ByteSize > 0 {
			attachmentByID[attachment.ID] = attachment
		}
	}
	hitByRef := make(map[string]int, len(hits))
	for i, hit := range hits {
		hitByRef[ragChunkKey(hit.DocID, hit.DocVersion, hit.ChunkIndex)] = i
	}
	for _, mapping := range mappings {
		asset, ready := assetByID[mapping.AssetID]
		index, exists := hitByRef[ragChunkKey(mapping.DocID, mapping.DocVersion, mapping.ChunkIndex)]
		if !ready || !exists || asset.DocID != mapping.DocID ||
			mapping.DocVersion < asset.FirstSeenVersion || mapping.DocVersion > asset.LastSeenVersion {
			continue
		}
		location := decodeSourceLocation(mapping.LocationJSON, hits[index].PageNum)
		pageNum := 0
		if location.Kind == document.LocationPage {
			pageNum = location.Index
		}
		ref := document.AssetRef{
			ID: asset.ID, Kind: document.AssetKindImage, Caption: mapping.Caption,
			PageNum: pageNum, Location: location, Width: asset.Width, Height: asset.Height,
			MIMEType: asset.DisplayMIME,
		}
		if mapping.AttachmentID != "" {
			attachment, ready := attachmentByID[mapping.AttachmentID]
			if !ready || attachment.DocID != mapping.DocID ||
				mapping.DocVersion < attachment.FirstSeenVersion ||
				mapping.DocVersion > attachment.LastSeenVersion {
				continue
			}
			ref.Attachment = &document.AttachmentRef{
				ID: attachment.ID, Kind: attachment.Kind, FileName: attachment.FileName,
				MIMEType: attachment.MIMEType, SizeBytes: attachment.ByteSize,
			}
		}
		hits[index].Assets = append(hits[index].Assets, ref)
	}
	return hits, nil
}
