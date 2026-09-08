package rag

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

type sessionRecordingAnswer struct{ id string }

func (a *sessionRecordingAnswer) Chat(ctx context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	a.id = provider.SessionIDFromContext(ctx)
	return &provider.Response{Content: "No matching evidence."}, nil
}

func TestEvaluationScopesPlannerAndAnswerToRunCase(t *testing.T) {
	service, vectors := newTestService(t, false)
	if err := vectors.EnsureCollection(context.Background(), vector.CollectionKey("eval_sessions"), 4); err != nil {
		t.Fatal(err)
	}
	var plannerID string
	service.queryLLM = func(ctx context.Context, _, _, _ string) (string, error) {
		plannerID = provider.SessionIDFromContext(ctx)
		return `{"rewritten_query":"question","hypothetical_document":"answer"}`, nil
	}
	answer := &sessionRecordingAnswer{}
	service.answerModel = func(context.Context, string, string) (AnswerModel, error) { return answer, nil }
	request := rageval.CaseExecutionRequest{
		OwnerID: "u1", RunID: "run-one", Case: rageval.Case{ID: "case-one", UserInput: "question"},
		Generation: &store.RAGEvalGenerationRecord{ID: "gen", DatasetVersionID: "dataset", Status: store.RAGEvalGenerationReady,
			CollectionKey: "eval_sessions", EmbeddingModel: "embed-test", EmbeddingDims: 4},
		Profile: config.RAGEvalProfileData{RewriteEnabled: true, HyDEEnabled: true, AnswerModel: "fake/model",
			Ingestion: config.RAGIngestionPolicyData{Embedding: config.RAGPolicyEmbeddingData{Model: "embed-test", Dims: 4}},
			Runtime:   config.RAGRuntimePolicyData{TopN: 5, CandidateTopK: 20, MaxTokens: 128, RAGPromptBundleVersion: RAGAnswerPromptBundleV1}},
	}
	var ids []string
	for _, runCase := range [][2]string{{"run-one", "case-one"}, {"run-one", "case-one"}, {"run-one", "case-two"}, {"run-two", "case-one"}} {
		request.RunID, request.Case.ID = runCase[0], runCase[1]
		if _, err := service.Execute(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		if plannerID == "" || plannerID != answer.id {
			t.Fatal("planner and answer lost the shared case scope")
		}
		ids = append(ids, plannerID)
	}
	if ids[0] != ids[1] || ids[0] == ids[2] || ids[0] == ids[3] || ids[2] == ids[3] {
		t.Fatalf("run/case session isolation failed: %v", ids)
	}
}

func TestEvaluationSavedHitsPreservesPreFilterCandidatesAndReferenceLabels(t *testing.T) {
	trace := SearchTrace{RerankCandidates: []RerankCandidateTrace{
		{DocumentID: "gold", ContextID: "gold:3", RecallScore: 0.8, RerankScore: 0.4, Selected: false},
		{DocumentID: "other", ContextID: "other:1", RecallScore: 0.7, RerankScore: 0.9, Selected: true},
	}}

	hits := evaluationSavedHits(trace, nil, []string{"gold"})
	if len(hits) != 2 {
		t.Fatalf("saved hits = %+v", hits)
	}
	if hits[0].Relevant == nil || !*hits[0].Relevant || hits[0].Selected || hits[0].RerankScore == nil || *hits[0].RerankScore != 0.4 {
		t.Fatalf("gold candidate = %+v", hits[0])
	}
	if hits[1].Relevant == nil || *hits[1].Relevant || !hits[1].Selected {
		t.Fatalf("non-gold candidate = %+v", hits[1])
	}
}

func TestEvaluationSavedHitsLeavesRelevanceUnknownWithoutReferences(t *testing.T) {
	hits := evaluationSavedHits(SearchTrace{}, []Hit{{DocID: "doc", ChunkIndex: 2, RecallScore: 0.5}}, nil)
	if len(hits) != 1 || hits[0].ContextID != "doc:2" || hits[0].Relevant != nil || !hits[0].Selected {
		t.Fatalf("saved hits = %+v", hits)
	}
}
