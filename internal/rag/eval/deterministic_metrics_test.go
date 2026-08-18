package eval

import (
	"strings"
	"testing"
)

func TestMetricStableContextIDsAndMissingAnnotation(t *testing.T) {
	results := DeterministicMetrics(DeterministicInput{
		RetrievedContextIDs: []string{"chunk-a", "chunk-b", "chunk-c"},
		ReferenceContextIDs: []string{"chunk-b", "chunk-c"},
	}, 2)
	if *results["hit_at_k"].Value != 1 || *results["recall_at_k"].Value != .5 || *results["mrr"].Value != .5 {
		t.Fatalf("unexpected stable-id metrics: %+v", results)
	}
	missing := DeterministicMetrics(DeterministicInput{RetrievedContextIDs: []string{"same text"}}, 1)
	for _, name := range []string{"hit_at_k", "recall_at_k", "mrr", "ndcg"} {
		if missing[name].Status != MetricSkippedMissingInput {
			t.Fatalf("%s used unannotated text as ground truth: %+v", name, missing[name])
		}
	}
}

func TestDocumentMetricsCollapseDynamicChunksWithoutSectionMapping(t *testing.T) {
	results := DeterministicMetrics(DeterministicInput{
		RetrievedContextIDs:  []string{"doc-c:2", "doc-a:7", "doc-a:8", "doc-b:4"},
		RetrievedDocumentIDs: []string{"doc-c", "doc-a", "doc-b"},
		ReferenceDocumentIDs: []string{"doc-a", "doc-b"},
	}, 3)
	if got := *results["doc_hit_at_k"].Value; got != 1 {
		t.Fatalf("doc hit@3=%v", got)
	}
	if got := *results["doc_recall_at_k"].Value; got != 1 {
		t.Fatalf("doc recall@3=%v", got)
	}
	if got := *results["doc_mrr"].Value; got != .5 {
		t.Fatalf("doc mrr=%v", got)
	}
	for _, metric := range []string{"hit_at_k", "recall_at_k", "mrr", "ndcg"} {
		if results[metric].Status != MetricSkippedMissingInput {
			t.Fatalf("chunk metric %s must skip without stable chunk qrels: %+v", metric, results[metric])
		}
	}
}

func TestCitationParserUsesOnlyBracketNumberContract(t *testing.T) {
	results := DeterministicMetrics(DeterministicInput{
		RetrievedContextIDs: []string{"chunk-a", "chunk-b"},
		Response:            "First claim [1]. Second claim citation:2. Third claim [3].",
	}, 2)
	if got := *results["citation_precision"].Value; got != .5 {
		t.Fatalf("citation precision=%v", got)
	}
	if got := *results["citation_coverage"].Value; got != 1.0/3.0 {
		t.Fatalf("citation coverage=%v", got)
	}
	if !strings.Contains(results["citation_precision"].Reason, "[3]") {
		t.Fatalf("out-of-range reason missing: %+v", results["citation_precision"])
	}
}

func TestCitationAndAbstentionEdgeRules(t *testing.T) {
	results := DeterministicMetrics(DeterministicInput{
		RetrievedContextIDs: []string{"chunk-a"}, Response: "No citation claim.",
		ExpectedAbstention: true, Abstained: false,
	}, 1)
	if results["citation_precision"].Status != MetricSkippedMissingInput || *results["citation_coverage"].Value != 0 {
		t.Fatalf("unexpected no-citation rules: %+v", results)
	}
	if *results["abstention_accuracy"].Value != 0 {
		t.Fatalf("abstention mismatch should score zero: %+v", results["abstention_accuracy"])
	}
	if !LooksLikeAbstention("Insufficient information to answer") {
		t.Fatal("explicit abstention was not recognized")
	}
}
