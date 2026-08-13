package eval

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateEmptyCorpusAndCases(t *testing.T) {
	tests := []struct {
		name    string
		dataset CanonicalDataset
		code    string
	}{
		{name: "empty corpus", dataset: CanonicalDataset{Name: "empty", Cases: []Case{{ID: "case", UserInput: "question"}}}, code: "empty_corpus"},
		{name: "empty cases", dataset: CanonicalDataset{Name: "empty", Corpus: []CorpusDocument{{ID: "doc", FileName: "doc.txt", MediaType: "text/plain", SHA256: strings.Repeat("a", 64)}}}, code: "empty_cases"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := ValidateDataset(tt.dataset, DefaultValidationLimits())
			if report.Valid || !validationHasCode(report, tt.code) {
				t.Fatalf("report=%+v, want %s", report, tt.code)
			}
		})
	}
}

func TestValidateDuplicateAndDanglingReferenceIDs(t *testing.T) {
	dataset := validDataset()
	dataset.Corpus = append(dataset.Corpus, dataset.Corpus[0])
	dataset.Cases = append(dataset.Cases, dataset.Cases[0])
	dataset.Cases[0].ReferenceContextIDs = []string{"missing#section"}
	report := ValidateDataset(dataset, DefaultValidationLimits())
	if report.Valid || !validationHasCode(report, "duplicate_id") || !validationHasCode(report, "dangling_reference") {
		t.Fatalf("report=%+v", report)
	}
}

func TestValidateMetadataLimitsAndInvalidUTF8(t *testing.T) {
	dataset := validDataset()
	dataset.Corpus[0].Metadata = map[string]any{"nested": map[string]any{"a": map[string]any{"b": "value"}}}
	dataset.Cases[0].Metadata = map[string]any{"payload": strings.Repeat("x", 64)}
	dataset.Cases[0].Tags = []string{string([]byte{0xff})}
	limits := DefaultValidationLimits()
	limits.MaxJSONDepth = 2
	limits.MaxMetadataBytes = 32
	report := ValidateDataset(dataset, limits)
	for _, code := range []string{"too_deep", "too_large", "invalid_utf8"} {
		if !validationHasCode(report, code) {
			t.Errorf("report missing %s: %+v", code, report)
		}
	}
}

func TestValidateMissingMetricInputWarnsAndMetricsSkip(t *testing.T) {
	dataset := validDataset()
	dataset.Cases[0].Reference = ""
	dataset.Cases[0].ReferenceContexts = nil
	dataset.Cases[0].ReferenceContextIDs = nil
	report := ValidateDataset(dataset, DefaultValidationLimits())
	if !report.Valid || !validationHasCode(report, "metric_coverage") {
		t.Fatalf("missing inputs should warn without invalidating: %+v", report)
	}
	for _, metric := range []string{"context_recall", "factual_correctness", "hit_at_k", "recall_at_k", "mrr", "ndcg"} {
		coverage, ok := validationCoverage(report, metric)
		if !ok || coverage.Eligible != 0 || coverage.Total != 1 {
			t.Errorf("coverage %s=%+v found=%v", metric, coverage, ok)
		}
	}
	results := DeterministicMetrics(DeterministicInput{RetrievedContextIDs: []string{"doc-1"}}, 1)
	for _, metric := range []string{"hit_at_k", "recall_at_k", "mrr", "ndcg"} {
		if results[metric].Status != MetricSkippedMissingInput {
			t.Errorf("%s status=%q", metric, results[metric].Status)
		}
	}
	if dataset.Cases[0].Reference != "" || dataset.Cases[0].ReferenceContextIDs != nil {
		t.Fatal("validation synthesized missing reference input")
	}
}

func TestValidationReportDoesNotReflectCredentialValues(t *testing.T) {
	const secret = "secret-token-that-must-not-appear"
	dataset := validDataset()
	dataset.Cases[0].ReferenceContextIDs = []string{secret + "#section"}
	dataset.Cases[0].Metadata = map[string]any{"apiKey": secret, "endpoint": "https://user:password@example.invalid"}
	report := ValidateDataset(dataset, DefaultValidationLimits())
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "password@example") {
		t.Fatalf("validation report reflected credential value: %s", encoded)
	}
	if !validationHasCode(report, "sensitive_metadata_ignored") {
		t.Fatalf("sensitive metadata warning missing: %+v", report)
	}
}

func validationHasCode(report ValidationReport, code string) bool {
	for _, issue := range report.Issues {
		if issue.Code == code {
			return true
		}
	}
	return false
}

func validationCoverage(report ValidationReport, metric string) (MetricCoverage, bool) {
	for _, coverage := range report.Coverage {
		if coverage.Metric == metric {
			return coverage, true
		}
	}
	return MetricCoverage{}, false
}
