package rag

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/embed"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

// EvaluationRunnerGenerationProvider connects the durable runner to the real
// shadow-generation builder. It supports only the configured, credential-
// resolved embedding binding; snapshots contain its secret-free fingerprint.
type EvaluationRunnerGenerationProvider struct {
	Store     store.Store
	Builder   *EvaluationGenerationBuilder
	Embedding config.RAGEmbeddingCfg
	Contract  rageval.GenerationContract
}

func DefaultEvaluationGenerationContract() rageval.GenerationContract {
	return rageval.GenerationContract{ParserProtocolVersion: "parser-v1", ParserEngineVersion: parse.LocalParserVersion,
		TokenizerVersion: "estimate-v1", SplitterVersion: splitterSchemaVersion,
		ArtifactSchemaVersion: document.ParsedArtifactSchemaVersion, VectorSchemaVersion: "vector-v1", IndexFormatVersion: 1}
}

func (p *EvaluationRunnerGenerationProvider) Ensure(ctx context.Context, run *store.RAGEvalRunRecord, snapshot rageval.ExecutionSnapshot) (*store.RAGEvalGenerationRecord, error) {
	if p == nil || p.Store == nil || p.Builder == nil || run == nil {
		return nil, errors.New("evaluation generation provider is unavailable")
	}
	if run.Mode == store.RAGEvalRunModeOnlineOnly {
		generation, err := p.Store.GetRAGEvalGeneration(ctx, run.IndexGenerationID)
		if err != nil {
			return nil, err
		}
		if generation.Status != store.RAGEvalGenerationReady || generation.DatasetVersionID != run.DatasetVersionID {
			return nil, store.ErrRAGEvalGenerationConflict
		}
		return generation, nil
	}
	documents := []store.RAGEvalCorpusDocumentRecord{}
	cursor := ""
	for {
		batch, err := p.Store.ListRAGEvalCorpusDocuments(ctx, run.DatasetVersionID, cursor, 200)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		documents = append(documents, batch...)
		cursor = batch[len(batch)-1].ID
	}
	contract := p.Contract
	if contract.ParserProtocolVersion == "" {
		contract = DefaultEvaluationGenerationContract()
	}
	result, _, err := p.Builder.Build(ctx, EvaluationGenerationBuildRequest{OwnerID: run.CreatedBy, RunID: run.ID, DatasetVersion: &snapshot.DatasetVersion, Documents: documents,
		Ingestion: snapshot.Profile.Ingestion, Contract: contract, Embedding: p.Embedding, EmbeddingContractFingerprint: snapshot.Profile.Ingestion.Embedding.ContractFingerprint})
	return result, err
}

func (p *EvaluationRunnerGenerationProvider) Release(ctx context.Context, runID string) error {
	_, err := p.Builder.ReleaseRun(ctx, runID)
	return err
}

// Execute is the concrete CasePipeline used by the background runner. Search
// and answer both use the same service components as production, but the
// target is an explicit isolated evaluation generation and answer mode rejects
// chat/ordinary usage persistence hooks.
func (s *Service) Execute(ctx context.Context, request rageval.CaseExecutionRequest) (rageval.CaseExecutionResult, error) {
	started := time.Now()
	hits, trace, err := s.SearchEvaluationWithOptions(ctx, request.OwnerID, request.Generation, request.Case.UserInput, request.Profile)
	if err != nil {
		return rageval.CaseExecutionResult{SearchTrace: trace, Latency: time.Since(started), ErrorCode: "search_error", ErrorMessage: err.Error()}, err
	}
	if s.answerModel == nil {
		return rageval.CaseExecutionResult{SearchTrace: trace, Latency: time.Since(started), ErrorCode: "answer_model_unavailable"}, errors.New("evaluation answer model resolver is unavailable")
	}
	model, err := s.answerModel(ctx, request.OwnerID, request.Profile.AnswerModel)
	if err != nil {
		return rageval.CaseExecutionResult{SearchTrace: trace, Latency: time.Since(started), ErrorCode: "answer_model_unavailable", ErrorMessage: err.Error()}, err
	}
	answer, err := GenerateAnswer(ctx, model, AnswerRequest{Mode: AnswerModeEvaluation, Input: AnswerInput{KnowledgeBase: AnswerKnowledgeBase{ID: request.Generation.ID, Name: "evaluation"}, Question: request.Case.UserInput, Hits: hits}}, AnswerOptions{Model: request.Profile.AnswerModel, Temperature: request.Profile.Runtime.Temperature, MaxTokens: request.Profile.Runtime.MaxTokens, PromptBundleVersion: request.Profile.Runtime.RAGPromptBundleVersion, RuntimePolicyVersion: request.Profile.Runtime.Version})
	contexts := make([]string, len(hits))
	ids := make([]string, len(hits))
	for i, hit := range hits {
		contexts[i] = hit.AnswerText()
		ids[i] = fmt.Sprintf("%s:%d", hit.DocID, hit.ChunkIndex)
	}
	citations := make([]string, len(answer.Citations))
	for i, citation := range answer.Citations {
		citations[i] = fmt.Sprint(citation.Number)
	}
	providerName, modelName := provider.SplitProviderModel(request.Profile.AnswerModel)
	savedHits := make([]evaluationTraceHit, len(hits))
	for index, hit := range hits {
		savedHits[index] = evaluationTraceHit{ContextID: ids[index], RecallScore: hit.RecallScore, RerankScore: hit.RerankScore}
	}
	result := rageval.CaseExecutionResult{Response: answer.Response, Contexts: contexts, ContextIDs: ids, Citations: citations, SearchTrace: evaluationSearchTrace{Trace: trace, Hits: savedHits}, AnswerTrace: answer, Latency: time.Since(started),
		Usage: rageval.Usage{Stage: "answer", Provider: providerName, Model: modelName, InputTokens: int64(answer.Usage.InputTokens), OutputTokens: int64(answer.Usage.OutputTokens)}}
	if err != nil {
		result.ErrorCode = AnswerErrorCode(err)
		result.ErrorMessage = err.Error()
	}
	return result, err
}

type evaluationTraceHit struct {
	ContextID   string   `json:"contextId"`
	RecallScore float64  `json:"recallScore"`
	RerankScore *float64 `json:"rerankScore,omitempty"`
}
type evaluationSearchTrace struct {
	Trace SearchTrace          `json:"trace"`
	Hits  []evaluationTraceHit `json:"hits"`
}

// SearchEvaluationWithOptions shares query planning, embedding, Milvus hybrid
// retrieval and reranking with Service search while accepting only a trusted
// persisted evaluation generation.
func (s *Service) SearchEvaluationWithOptions(ctx context.Context, ownerID string, generation *store.RAGEvalGenerationRecord, query string, profile config.RAGEvalProfileData) (hits []Hit, trace SearchTrace, err error) {
	generationID := "unavailable"
	if generation != nil {
		generationID = generation.ID
	}
	trace = SearchTrace{RetrievalID: "eval_" + generationID, TopN: profile.Runtime.TopN, CandidateTopK: profile.Runtime.CandidateTopK, MinScore: profile.Runtime.MinScore,
		RewriteEnabled: profile.RewriteEnabled, HyDEEnabled: profile.HyDEEnabled, RerankerRequested: profile.RerankerEnabled, RerankerConfigured: s.reranker != nil, RerankerEnabled: profile.RerankerEnabled && s.reranker != nil, RerankerFailurePolicy: profile.RerankerFailurePolicy, RuntimePolicyVersion: profile.Runtime.Version}
	started := time.Now()
	defer func() { trace.DurationMS = time.Since(started).Milliseconds() }()
	if s == nil || s.vec == nil || s.st == nil || generation == nil || generation.Status != store.RAGEvalGenerationReady {
		return nil, trace, errors.New("READY evaluation generation is required")
	}
	if strings.TrimSpace(query) == "" {
		return nil, trace, errors.New("evaluation query is required")
	}
	if generation.EmbeddingModel != profile.Ingestion.Embedding.Model || generation.EmbeddingDims != int64(profile.Ingestion.Embedding.Dims) {
		return nil, trace, errors.New("evaluation generation embedding contract mismatch")
	}
	docs := map[string]store.RAGEvalCorpusDocumentRecord{}
	active := map[string]int64{}
	cursor := ""
	for {
		batch, err := s.st.ListRAGEvalCorpusDocuments(ctx, generation.DatasetVersionID, cursor, 200)
		if err != nil {
			return nil, trace, err
		}
		if len(batch) == 0 {
			break
		}
		for _, doc := range batch {
			docs[doc.ExternalID] = doc
			active[doc.ExternalID] = 1
		}
		cursor = batch[len(batch)-1].ID
	}
	plan := QueryPlan{RewrittenQuery: query, HypotheticalDocument: query}
	if profile.RewriteEnabled || profile.HyDEEnabled {
		plan = s.planQuery(ctx, trace.RetrievalID, ownerID, SearchContext{Query: query})
	}
	if !profile.RewriteEnabled {
		plan.RewrittenQuery = query
	}
	if !profile.HyDEEnabled {
		plan.HypotheticalDocument = plan.RewrittenQuery
	}
	trace.PlannerAttempted = plan.Route.PlannerAttempted
	trace.RewriteApplied = plan.Route.RewriteApplied
	trace.HyDEApplied = plan.Route.HyDEApplied
	trace.PlannerFallback = plan.Route.Fallback
	trace.PlannerFallbackReason = plan.Route.FallbackReason
	trace.PlannerDurationMS = plan.Route.DurationMS
	embeddingCfg := s.cfg.Embedding
	if embeddingCfg.Model != profile.Ingestion.Embedding.Model || embeddingCfg.Dims != profile.Ingestion.Embedding.Dims {
		return nil, trace, errors.New("configured embedding does not satisfy evaluation profile")
	}
	texts := []string{plan.RewrittenQuery}
	if plan.HypotheticalDocument != plan.RewrittenQuery {
		texts = append(texts, plan.HypotheticalDocument)
	}
	vectors, err := embed.New(embeddingCfg.Endpoint, embeddingCfg.APIKey, embeddingCfg.Model, embeddingCfg.Dims).Embed(ctx, texts)
	if err != nil {
		return nil, trace, err
	}
	key, err := vector.CollectionKeyFromPersistence(generation.CollectionKey)
	if err != nil {
		return nil, trace, err
	}
	vectorHits, err := s.vec.HybridSearch(ctx, key, vector.SearchQuery{Dense: vectors, Text: plan.RewrittenQuery, ActiveVersions: active, MaxFilterBytes: s.cfg.Limits.MaxMilvusFilterBytes}, profile.Runtime.CandidateTopK)
	if err != nil {
		return nil, trace, err
	}
	hits = make([]Hit, 0, len(vectorHits))
	for _, item := range vectorHits {
		doc, ok := docs[item.DocID]
		if !ok || item.DocVersion != 1 {
			continue
		}
		hits = append(hits, Hit{KBID: "eval", KBName: "evaluation", DocID: item.DocID, DocName: doc.FileName, ChunkIndex: item.ChunkIndex, SectionTitle: item.SectionTitle, PageNum: item.PageNum, SourceLocation: sourceLocationFromPage(item.PageNum), Content: item.Content, SearchContent: item.SearchContent, Score: item.Score, RecallScore: item.Score, DocVersion: 1, IndexFormatVersion: 1})
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	trace.CandidateCount = len(hits)
	if profile.RerankerEnabled && s.reranker != nil && len(hits) > 0 {
		reranked, stats, rankErr := s.rerankHitsWithStats(ctx, trace.RetrievalID, plan.RewrittenQuery, hits, profile.Runtime.TopN, profile.Runtime.MinScore)
		if rankErr == nil {
			trace.RerankerAttempted = true
			trace.RerankerSucceeded = true
			trace.RerankerRankedCount = stats.rankedCount
			trace.RerankerFilteredCount = stats.filteredCount
			trace.ReturnedCount = len(reranked)
			return reranked, trace, nil
		}
		if profile.RerankerFailurePolicy == config.RAGRerankerFailClosed {
			return nil, trace, rankErr
		}
		trace.RerankerFallback = true
		trace.Degraded = true
	}
	if len(hits) > profile.Runtime.TopN {
		hits = hits[:profile.Runtime.TopN]
	}
	trace.ReturnedCount = len(hits)
	return hits, trace, nil
}

var _ rageval.CasePipeline = (*Service)(nil)
var _ rageval.GenerationProvider = (*EvaluationRunnerGenerationProvider)(nil)
