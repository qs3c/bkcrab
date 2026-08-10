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
	"github.com/qs3c/bkcrab/internal/config"
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

// SearchOptions is an immutable per-request override used by evaluation runs.
// Nil booleans preserve production behavior; explicit false disables a stage.
type SearchOptions struct {
	TopN                  int                             `json:"topN,omitempty"`
	CandidateTopK         int                             `json:"candidateTopK,omitempty"`
	MinScore              *float64                        `json:"minScore,omitempty"`
	Rewrite               *bool                           `json:"rewrite,omitempty"`
	HyDE                  *bool                           `json:"hyde,omitempty"`
	Reranker              *bool                           `json:"reranker,omitempty"`
	RerankerFailurePolicy config.RAGRerankerFailurePolicy `json:"rerankerFailurePolicy,omitempty"`
}

type SearchTrace struct {
	RetrievalID           string                          `json:"retrievalId"`
	DurationMS            int64                           `json:"durationMs"`
	PlannerDurationMS     int64                           `json:"plannerDurationMs"`
	RetrievalDurationMS   int64                           `json:"retrievalDurationMs"`
	RerankerDurationMS    int64                           `json:"rerankerDurationMs"`
	HydrationDurationMS   int64                           `json:"hydrationDurationMs"`
	KnowledgeBaseCount    int                             `json:"knowledgeBaseCount"`
	DenseRouteCount       int                             `json:"denseRouteCount"`
	CandidateCount        int                             `json:"candidateCount"`
	RerankerRankedCount   int                             `json:"rerankerRankedCount"`
	RerankerFilteredCount int                             `json:"rerankerFilteredCount"`
	ReturnedCount         int                             `json:"returnedCount"`
	RewriteEnabled        bool                            `json:"rewriteEnabled"`
	HyDEEnabled           bool                            `json:"hydeEnabled"`
	PlannerAttempted      bool                            `json:"plannerAttempted"`
	RewriteApplied        bool                            `json:"rewriteApplied"`
	HyDEApplied           bool                            `json:"hydeApplied"`
	PlannerFallback       bool                            `json:"plannerFallback"`
	PlannerFallbackReason string                          `json:"plannerFallbackReason,omitempty"`
	RerankerRequested     bool                            `json:"rerankerRequested"`
	RerankerConfigured    bool                            `json:"rerankerConfigured"`
	RerankerEnabled       bool                            `json:"rerankerEnabled"`
	RerankerAttempted     bool                            `json:"rerankerAttempted"`
	RerankerSucceeded     bool                            `json:"rerankerSucceeded"`
	RerankerFallback      bool                            `json:"rerankerFallback"`
	RerankerFailureCode   string                          `json:"rerankerFailureCode,omitempty"`
	RerankerFailurePolicy config.RAGRerankerFailurePolicy `json:"rerankerFailurePolicy"`
	Degraded              bool                            `json:"degraded"`
	MinScore              float64                         `json:"minScore"`
	TopN                  int                             `json:"topN"`
	CandidateTopK         int                             `json:"candidateTopK"`
	RecallScoreMin        *float64                        `json:"recallScoreMin,omitempty"`
	RecallScoreMax        *float64                        `json:"recallScoreMax,omitempty"`
	RerankScoreMin        *float64                        `json:"rerankScoreMin,omitempty"`
	RerankScoreMax        *float64                        `json:"rerankScoreMax,omitempty"`
}

type searchOptionsKey struct{}
type searchTraceKey struct{}

// SearchWithOptions runs the same production retrieval implementation while
// injecting a bounded immutable option snapshot and returning a secret-free
// trace. It never mutates Service configuration.
func (s *Service) SearchWithOptions(ctx context.Context, ownerID string, kbIDs []string, input SearchContext, options SearchOptions) ([]Hit, SearchTrace, error) {
	if options.TopN < 0 || options.TopN > 20 {
		return nil, SearchTrace{}, errors.New("topN must be between 1 and 20")
	}
	if options.TopN == 0 {
		options.TopN = 5
	}
	if options.CandidateTopK < 0 || options.CandidateTopK > 100 {
		return nil, SearchTrace{}, errors.New("candidateTopK must be between 1 and 100")
	}
	if options.CandidateTopK > 0 && options.CandidateTopK < options.TopN {
		return nil, SearchTrace{}, errors.New("candidateTopK must be greater than or equal to topN")
	}
	if options.MinScore != nil && (*options.MinScore < 0 || *options.MinScore > 1 || math.IsNaN(*options.MinScore) || math.IsInf(*options.MinScore, 0)) {
		return nil, SearchTrace{}, errors.New("minScore must be between 0 and 1")
	}
	if !options.RerankerFailurePolicy.Valid() {
		return nil, SearchTrace{}, errors.New("unknown reranker failure policy")
	}
	if options.RerankerFailurePolicy == "" {
		options.RerankerFailurePolicy = config.RAGRerankerFallbackRRF
	}
	candidateTopK := options.CandidateTopK
	if candidateTopK == 0 {
		candidateTopK = s.cfg.Reranker.CandidateTopK
	}
	if candidateTopK < options.TopN {
		candidateTopK = options.TopN
	}
	options.CandidateTopK = candidateTopK
	minScore := s.cfg.Reranker.MinScore
	if options.MinScore != nil {
		minScore = *options.MinScore
	}
	rewrite, hyde := true, true
	if options.Rewrite != nil {
		rewrite = *options.Rewrite
	}
	if options.HyDE != nil {
		hyde = *options.HyDE
	}
	rerankerRequested := s.reranker != nil
	if options.Reranker != nil {
		rerankerRequested = *options.Reranker
	}
	retrievalID := uuid.NewString()
	started := time.Now()
	trace := SearchTrace{
		RetrievalID: retrievalID, RewriteEnabled: rewrite, HyDEEnabled: hyde,
		RerankerRequested: rerankerRequested, RerankerConfigured: s.reranker != nil,
		RerankerEnabled:       rerankerRequested && s.reranker != nil,
		RerankerFailurePolicy: options.RerankerFailurePolicy,
		MinScore:              minScore, TopN: options.TopN, CandidateTopK: candidateTopK,
	}
	ctx = context.WithValue(ctx, searchOptionsKey{}, options)
	ctx = context.WithValue(ctx, searchTraceKey{}, &trace)
	hits, err := s.SearchWithContext(ctx, ownerID, kbIDs, input, options.TopN)
	trace.DurationMS = time.Since(started).Milliseconds()
	trace.ReturnedCount = len(hits)
	return hits, trace, err
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
	trace, _ := ctx.Value(searchTraceKey{}).(*SearchTrace)
	if trace != nil {
		retrievalID = trace.RetrievalID
		resetSearchExecutionTrace(trace)
	}
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
	if trace != nil {
		trace.KnowledgeBaseCount = len(kbs)
	}

	options, hasOptions := ctx.Value(searchOptionsKey{}).(SearchOptions)
	rewriteEnabled, hydeEnabled := true, true
	if hasOptions && options.Rewrite != nil {
		rewriteEnabled = *options.Rewrite
	}
	if hasOptions && options.HyDE != nil {
		hydeEnabled = *options.HyDE
	}
	plan := QueryPlan{RewrittenQuery: query, HypotheticalDocument: query}
	if rewriteEnabled || hydeEnabled {
		plan = s.planQuery(ctx, retrievalID, kbs[0].UserID, SearchContext{Query: query, History: input.History})
	}
	if !rewriteEnabled {
		plan.RewrittenQuery = query
		plan.Route.RewriteApplied = false
	}
	if !hydeEnabled {
		plan.HypotheticalDocument = plan.RewrittenQuery
		plan.Route.HyDEApplied = false
	}
	if trace != nil {
		trace.PlannerAttempted = plan.Route.PlannerAttempted
		trace.RewriteApplied = plan.Route.RewriteApplied
		trace.HyDEApplied = plan.Route.HyDEApplied
		trace.PlannerFallback = plan.Route.Fallback
		trace.PlannerFallbackReason = plan.Route.FallbackReason
		trace.PlannerDurationMS = plan.Route.DurationMS
		if plan.Route.Fallback {
			trace.Degraded = true
		}
	}
	retrievalStarted := time.Now()
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
	if hasOptions && options.CandidateTopK > 0 {
		candidateTopK = options.CandidateTopK
	}
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
	if trace != nil {
		trace.RetrievalDurationMS = time.Since(retrievalStarted).Milliseconds()
		trace.CandidateCount = candidateCount
		if len(targets) > 0 {
			trace.DenseRouteCount = len(targets[0].dense)
		}
		setRecallScoreRange(trace, results)
	}
	rerankerRequested := s.reranker != nil
	if hasOptions && options.Reranker != nil {
		rerankerRequested = *options.Reranker
	}
	rerankerEnabled := rerankerRequested && s.reranker != nil
	if hasOptions && rerankerRequested && s.reranker == nil {
		if trace != nil {
			trace.RerankerFallback = true
			trace.RerankerFailureCode = "reranker_unavailable"
			trace.Degraded = true
		}
		if options.RerankerFailurePolicy == config.RAGRerankerFailClosed {
			return nil, errors.New("reranker unavailable")
		}
	}
	if rerankerEnabled && len(results) > 0 {
		minScore := s.cfg.Reranker.MinScore
		if hasOptions && options.MinScore != nil {
			minScore = *options.MinScore
		}
		if trace != nil {
			trace.RerankerAttempted = true
		}
		reranked, stats, err := s.rerankHitsWithStats(ctx, retrievalID, plan.RewrittenQuery, results, topN, minScore)
		if trace != nil {
			trace.RerankerDurationMS = stats.durationMS
			trace.RerankerRankedCount = stats.rankedCount
			trace.RerankerFilteredCount = stats.filteredCount
			trace.RerankScoreMin = stats.scoreMin
			trace.RerankScoreMax = stats.scoreMax
		}
		if err == nil {
			if trace != nil {
				trace.RerankerSucceeded = true
			}
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
			return s.hydrateSearchHits(ctx, reranked, trace)
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if trace != nil {
			trace.RerankerFailureCode = "reranker_error"
			trace.Degraded = true
		}
		if hasOptions && options.RerankerFailurePolicy == config.RAGRerankerFailClosed {
			return nil, fmt.Errorf("reranker failed: %w", err)
		}
		if trace != nil {
			trace.RerankerFallback = true
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
	return s.hydrateSearchHits(ctx, results, trace)
}

func resetSearchExecutionTrace(trace *SearchTrace) {
	trace.PlannerDurationMS = 0
	trace.RetrievalDurationMS = 0
	trace.RerankerDurationMS = 0
	trace.HydrationDurationMS = 0
	trace.KnowledgeBaseCount = 0
	trace.DenseRouteCount = 0
	trace.CandidateCount = 0
	trace.RerankerRankedCount = 0
	trace.RerankerFilteredCount = 0
	trace.ReturnedCount = 0
	trace.PlannerAttempted = false
	trace.RewriteApplied = false
	trace.HyDEApplied = false
	trace.PlannerFallback = false
	trace.PlannerFallbackReason = ""
	trace.RerankerAttempted = false
	trace.RerankerSucceeded = false
	trace.RerankerFallback = false
	trace.RerankerFailureCode = ""
	trace.Degraded = false
	trace.RecallScoreMin = nil
	trace.RecallScoreMax = nil
	trace.RerankScoreMin = nil
	trace.RerankScoreMax = nil
}

func setRecallScoreRange(trace *SearchTrace, hits []Hit) {
	if len(hits) == 0 {
		return
	}
	minimum, maximum := hits[0].RecallScore, hits[0].RecallScore
	for _, hit := range hits[1:] {
		minimum = math.Min(minimum, hit.RecallScore)
		maximum = math.Max(maximum, hit.RecallScore)
	}
	trace.RecallScoreMin = &minimum
	trace.RecallScoreMax = &maximum
}

func (s *Service) hydrateSearchHits(ctx context.Context, hits []Hit, trace *SearchTrace) ([]Hit, error) {
	started := time.Now()
	hydrated, err := s.hydrateHitAssets(ctx, hits)
	if trace != nil {
		trace.HydrationDurationMS = time.Since(started).Milliseconds()
	}
	return hydrated, err
}

// rerankHits replaces the public score with the normalized semantic score and
// applies the confidence threshold only after a successful reranker call. Any
// service or response error is returned to SearchWithContext, which falls back
// to the untouched RRF ordering without applying this threshold.
func (s *Service) rerankHits(ctx context.Context, retrievalID, query string, candidates []Hit, topN int) ([]Hit, error) {
	return s.rerankHitsWithMinScore(ctx, retrievalID, query, candidates, topN, s.cfg.Reranker.MinScore)
}

func (s *Service) rerankHitsWithMinScore(ctx context.Context, retrievalID, query string, candidates []Hit, topN int, minScore float64) ([]Hit, error) {
	hits, _, err := s.rerankHitsWithStats(ctx, retrievalID, query, candidates, topN, minScore)
	return hits, err
}

type rerankStats struct {
	durationMS    int64
	rankedCount   int
	filteredCount int
	scoreMin      *float64
	scoreMax      *float64
}

func (s *Service) rerankHitsWithStats(ctx context.Context, retrievalID, query string, candidates []Hit, topN int, minScore float64) ([]Hit, rerankStats, error) {
	started := time.Now()
	stats := rerankStats{}
	documents := make([]string, len(candidates))
	for index := range candidates {
		documents[index] = candidates[index].SearchContent
		if documents[index] == "" {
			documents[index] = chunktext.Search(candidates[index].SectionTitle, candidates[index].AnswerText())
		}
	}
	ranked, err := s.reranker.Rerank(ctx, query, documents, topN)
	if err != nil {
		stats.durationMS = time.Since(started).Milliseconds()
		return nil, stats, err
	}
	if len(ranked) == 0 {
		stats.durationMS = time.Since(started).Milliseconds()
		return nil, stats, fmt.Errorf("reranker 返回空结果")
	}

	seen := make(map[int]struct{}, len(ranked))
	for _, item := range ranked {
		if item.Index < 0 || item.Index >= len(candidates) {
			stats.durationMS = time.Since(started).Milliseconds()
			return nil, stats, fmt.Errorf("reranker 返回非法 index %d", item.Index)
		}
		if _, exists := seen[item.Index]; exists {
			stats.durationMS = time.Since(started).Milliseconds()
			return nil, stats, fmt.Errorf("reranker 返回重复 index %d", item.Index)
		}
		if math.IsNaN(item.Score) || math.IsInf(item.Score, 0) || item.Score < 0 || item.Score > 1 {
			stats.durationMS = time.Since(started).Milliseconds()
			return nil, stats, fmt.Errorf("reranker 返回非法分数 %v", item.Score)
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
	stats.rankedCount = len(ranked)
	if len(ranked) > 0 {
		minimum, maximum := ranked[0].Score, ranked[0].Score
		for _, item := range ranked[1:] {
			minimum = math.Min(minimum, item.Score)
			maximum = math.Max(maximum, item.Score)
		}
		stats.scoreMin = &minimum
		stats.scoreMax = &maximum
	}

	filtered := make([]Hit, 0, len(ranked))
	for _, item := range ranked {
		if item.Score < minScore {
			continue
		}
		hit := candidates[item.Index]
		score := item.Score
		hit.Score = score
		hit.RerankScore = &score
		filtered = append(filtered, hit)
	}
	stats.filteredCount = len(ranked) - len(filtered)
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
		"min_score", minScore,
		"top_score", topScore,
		"lowest_returned_score", lowestReturnedScore,
		"duration_ms", time.Since(started).Milliseconds(),
	)
	stats.durationMS = time.Since(started).Milliseconds()
	return filtered, stats, nil
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
