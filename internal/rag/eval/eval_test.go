package eval

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

func validDataset() CanonicalDataset {
	return CanonicalDataset{
		Name: "golden",
		Corpus: []CorpusDocument{{
			ID: "doc-1", FileName: "policy.pdf", MediaType: "application/pdf", SHA256: "abc", SizeBytes: 10,
		}},
		Cases: []Case{{
			ID: "case-1", UserInput: "question", Reference: "answer", ReferenceContextIDs: []string{"doc-1#s1"},
		}},
	}
}

func TestValidateDatasetAndFingerprint(t *testing.T) {
	dataset := validDataset()
	report := ValidateDataset(dataset, DefaultValidationLimits())
	if !report.Valid {
		t.Fatalf("unexpected issues: %+v", report.Issues)
	}
	first, err := DatasetFingerprint(dataset)
	if err != nil {
		t.Fatal(err)
	}
	dataset.Corpus[0].ObjectKey = "another/storage/key"
	second, err := DatasetFingerprint(dataset)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("object key changed content fingerprint")
	}
}

func TestValidateDatasetRejectsDuplicateAndDangling(t *testing.T) {
	dataset := validDataset()
	dataset.Corpus = append(dataset.Corpus, dataset.Corpus[0])
	dataset.Cases[0].ReferenceContextIDs = []string{"missing#s1"}
	report := ValidateDataset(dataset, DefaultValidationLimits())
	if report.Valid {
		t.Fatal("invalid dataset accepted")
	}
}

func TestCanonicalImporter(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Register("canonical", CanonicalImporter{}); err != nil {
		t.Fatal(err)
	}
	importer, _ := registry.Lookup("canonical")
	normalized, err := importer.Normalize(context.Background(), ImportSource{Payload: validDataset()})
	if err != nil || normalized.Name != "golden" {
		t.Fatalf("normalize: %+v %v", normalized, err)
	}
}

func TestProfileFingerprintValidates(t *testing.T) {
	profile := Profile{Name: "p", Data: config.RAGEvalProfileData{Ingestion: config.RAGIngestionPolicyData{ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard, Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "f", Model: "m", Dims: 10}}, Runtime: config.RAGRuntimePolicyData{TopN: 5, CandidateTopK: 20, MinScore: .5, Temperature: .2, MaxTokens: 100, RAGPromptBundleVersion: "v1"}, RerankerFailurePolicy: config.RAGRerankerFallbackRRF, AnswerModel: "a"}}
	if _, err := ProfileFingerprint(profile); err != nil {
		t.Fatal(err)
	}
}

func TestDeterministicMetrics(t *testing.T) {
	results := DeterministicMetrics(DeterministicInput{RetrievedContextIDs: []string{"x", "a", "b"}, ReferenceContextIDs: []string{"a", "b"}, Citations: []string{"a", "bad"}, ExpectedAbstention: true, Abstained: true}, 3)
	if got := *results["recall_at_k"].Value; got != 1 {
		t.Fatalf("recall=%v", got)
	}
	if got := *results["mrr"].Value; got != .5 {
		t.Fatalf("mrr=%v", got)
	}
	if got := *results["citation_precision"].Value; got != .5 {
		t.Fatalf("citation precision=%v", got)
	}
}

func TestAggregateSkipsAreNotZero(t *testing.T) {
	values := []MetricResult{{Status: MetricOK, Value: score(.8)}, {Status: MetricSkippedMissingInput}, {Status: MetricError}}
	aggregate := AggregateMetric(values)
	if aggregate.ScoredCount != 1 || aggregate.SkippedCount != 1 || aggregate.ErrorCount != 1 || *aggregate.Mean != .8 {
		t.Fatalf("unexpected aggregate: %+v", aggregate)
	}
}

func TestComparePairedListsMissing(t *testing.T) {
	baseline := map[string]MetricResult{"a": {Status: MetricOK, Value: score(.5)}, "b": {Status: MetricOK, Value: score(.2)}}
	candidate := map[string]MetricResult{"a": {Status: MetricOK, Value: score(.7)}, "c": {Status: MetricOK, Value: score(.9)}}
	delta := ComparePaired(baseline, candidate)
	if delta.Pairs != 1 || len(delta.MissingBaseline) != 1 || len(delta.MissingCandidate) != 1 {
		t.Fatalf("unexpected delta: %+v", delta)
	}
}
