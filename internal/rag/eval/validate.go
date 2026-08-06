package eval

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type ValidationIssue struct {
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`
	Code     string   `json:"code"`
	Message  string   `json:"message"`
}

type MetricCoverage struct {
	Metric   string `json:"metric"`
	Eligible int    `json:"eligible"`
	Total    int    `json:"total"`
}

type ValidationReport struct {
	Valid    bool              `json:"valid"`
	Issues   []ValidationIssue `json:"issues"`
	Coverage []MetricCoverage  `json:"coverage"`
}

type ValidationLimits struct {
	MaxDocuments     int
	MaxCases         int
	MaxStringBytes   int
	MaxMetadataBytes int
	MaxJSONDepth     int
}

func DefaultValidationLimits() ValidationLimits {
	return ValidationLimits{MaxDocuments: 10_000, MaxCases: 100_000, MaxStringBytes: 1 << 20, MaxMetadataBytes: 64 << 10, MaxJSONDepth: 8}
}

func ValidateDataset(dataset CanonicalDataset, limits ValidationLimits) ValidationReport {
	if limits.MaxDocuments <= 0 {
		limits = DefaultValidationLimits()
	}
	report := ValidationReport{Valid: true}
	add := func(severity Severity, path, code, message string) {
		report.Issues = append(report.Issues, ValidationIssue{severity, path, code, message})
		if severity == SeverityError {
			report.Valid = false
		}
	}
	if len(dataset.Corpus) == 0 {
		add(SeverityError, "corpus", "empty_corpus", "at least one corpus document is required")
	}
	if len(dataset.Cases) == 0 {
		add(SeverityError, "cases", "empty_cases", "at least one case is required")
	}
	if len(dataset.Corpus) > limits.MaxDocuments {
		add(SeverityError, "corpus", "too_many_documents", "document limit exceeded")
	}
	if len(dataset.Cases) > limits.MaxCases {
		add(SeverityError, "cases", "too_many_cases", "case limit exceeded")
	}
	documents := make(map[string]struct{}, len(dataset.Corpus))
	for i, document := range dataset.Corpus {
		path := fmt.Sprintf("corpus[%d]", i)
		validateString := func(name, value string, required bool) {
			if required && strings.TrimSpace(value) == "" {
				add(SeverityError, path+"."+name, "required", "field is required")
			}
			if !utf8.ValidString(value) {
				add(SeverityError, path+"."+name, "invalid_utf8", "field must contain valid UTF-8")
			}
			if len(value) > limits.MaxStringBytes {
				add(SeverityError, path+"."+name, "too_large", "field exceeds byte limit")
			}
		}
		validateString("id", document.ID, true)
		validateString("fileName", document.FileName, true)
		validateString("mediaType", document.MediaType, true)
		validateString("sha256", document.SHA256, true)
		if document.SizeBytes < 0 {
			add(SeverityError, path+".sizeBytes", "invalid_size", "size must be non-negative")
		}
		if _, exists := documents[document.ID]; exists {
			add(SeverityError, path+".id", "duplicate_id", "document id must be unique")
		}
		documents[document.ID] = struct{}{}
		validateMetadata(&report, path+".metadata", document.Metadata, limits)
	}
	cases := make(map[string]struct{}, len(dataset.Cases))
	reference, contexts, ids := 0, 0, 0
	for i, item := range dataset.Cases {
		path := fmt.Sprintf("cases[%d]", i)
		if strings.TrimSpace(item.ID) == "" {
			add(SeverityError, path+".id", "required", "field is required")
		}
		if strings.TrimSpace(item.UserInput) == "" {
			add(SeverityError, path+".user_input", "required", "field is required")
		}
		if !utf8.ValidString(item.ID) || !utf8.ValidString(item.UserInput) || !utf8.ValidString(item.Reference) {
			add(SeverityError, path, "invalid_utf8", "case contains invalid UTF-8")
		}
		if len(item.UserInput) > limits.MaxStringBytes || len(item.Reference) > limits.MaxStringBytes {
			add(SeverityError, path, "too_large", "case field exceeds byte limit")
		}
		if _, exists := cases[item.ID]; exists {
			add(SeverityError, path+".id", "duplicate_id", "case id must be unique")
		}
		cases[item.ID] = struct{}{}
		if item.Reference != "" {
			reference++
		}
		if len(item.ReferenceContexts) > 0 {
			contexts++
		}
		if len(item.ReferenceContextIDs) > 0 {
			ids++
		}
		for _, ref := range item.ReferenceContextIDs {
			docID := ref
			if pos := strings.IndexByte(docID, '#'); pos >= 0 {
				docID = docID[:pos]
			}
			if _, exists := documents[docID]; !exists {
				add(SeverityError, path+".reference_context_ids", "dangling_reference", fmt.Sprintf("document %q does not exist", docID))
			}
		}
		validateMetadata(&report, path+".metadata", item.Metadata, limits)
	}
	total := len(dataset.Cases)
	report.Coverage = []MetricCoverage{{"context_recall", reference, total}, {"factual_correctness", reference, total}, {"context_precision", reference, total}, {"hit_at_k", ids, total}, {"mrr", ids, total}, {"ndcg", ids, total}, {"reference_context_match", contexts, total}}
	for _, coverage := range report.Coverage {
		if coverage.Eligible < coverage.Total {
			add(SeverityWarning, "cases", "metric_coverage", fmt.Sprintf("%s eligible for %d/%d cases", coverage.Metric, coverage.Eligible, coverage.Total))
		}
	}
	return report
}

func validateMetadata(report *ValidationReport, path string, value map[string]any, limits ValidationLimits) {
	if value == nil {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		report.Valid = false
		report.Issues = append(report.Issues, ValidationIssue{SeverityError, path, "invalid_json", err.Error()})
		return
	}
	if len(b) > limits.MaxMetadataBytes {
		report.Valid = false
		report.Issues = append(report.Issues, ValidationIssue{SeverityError, path, "too_large", "metadata exceeds byte limit"})
	}
	if jsonDepth(value) > limits.MaxJSONDepth {
		report.Valid = false
		report.Issues = append(report.Issues, ValidationIssue{SeverityError, path, "too_deep", "metadata exceeds JSON depth limit"})
	}
}

func jsonDepth(value any) int {
	switch typed := value.(type) {
	case map[string]any:
		max := 1
		for _, child := range typed {
			if depth := 1 + jsonDepth(child); depth > max {
				max = depth
			}
		}
		return max
	case []any:
		max := 1
		for _, child := range typed {
			if depth := 1 + jsonDepth(child); depth > max {
				max = depth
			}
		}
		return max
	default:
		return 0
	}
}
