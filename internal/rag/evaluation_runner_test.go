package rag

import "testing"

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
