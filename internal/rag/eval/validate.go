package eval

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
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
	MaxIDBytes       int
	MaxStringBytes   int
	MaxItemsPerCase  int
	MaxMetadataBytes int
	MaxJSONDepth     int
}

func DefaultValidationLimits() ValidationLimits {
	return ValidationLimits{MaxDocuments: 10_000, MaxCases: 100_000, MaxIDBytes: 255, MaxStringBytes: 1 << 20, MaxItemsPerCase: 1_000, MaxMetadataBytes: 64 << 10, MaxJSONDepth: 8}
}

func (l ValidationLimits) withDefaults() ValidationLimits {
	defaults := DefaultValidationLimits()
	if l.MaxDocuments <= 0 {
		l.MaxDocuments = defaults.MaxDocuments
	}
	if l.MaxCases <= 0 {
		l.MaxCases = defaults.MaxCases
	}
	if l.MaxIDBytes <= 0 {
		l.MaxIDBytes = defaults.MaxIDBytes
	}
	if l.MaxStringBytes <= 0 {
		l.MaxStringBytes = defaults.MaxStringBytes
	}
	if l.MaxItemsPerCase <= 0 {
		l.MaxItemsPerCase = defaults.MaxItemsPerCase
	}
	if l.MaxMetadataBytes <= 0 {
		l.MaxMetadataBytes = defaults.MaxMetadataBytes
	}
	if l.MaxJSONDepth <= 0 {
		l.MaxJSONDepth = defaults.MaxJSONDepth
	}
	return l
}

func ValidateDataset(dataset CanonicalDataset, limits ValidationLimits) ValidationReport {
	limits = limits.withDefaults()
	report := ValidationReport{Valid: true}
	add := func(severity Severity, path, code, message string) {
		report.Issues = append(report.Issues, ValidationIssue{severity, path, code, message})
		if severity == SeverityError {
			report.Valid = false
		}
	}
	validateString := func(path, value string, required bool, maxBytes int) {
		if required && strings.TrimSpace(value) == "" {
			add(SeverityError, path, "required", "field is required")
		}
		if !utf8.ValidString(value) {
			add(SeverityError, path, "invalid_utf8", "field must contain valid UTF-8")
		}
		if len(value) > maxBytes {
			add(SeverityError, path, "too_large", "field exceeds byte limit")
		}
	}
	validateString("name", dataset.Name, true, limits.MaxStringBytes)
	validateString("description", dataset.Description, false, limits.MaxStringBytes)
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
		validateString(path+".id", document.ID, true, limits.MaxIDBytes)
		validateString(path+".fileName", document.FileName, true, limits.MaxStringBytes)
		validateString(path+".mediaType", document.MediaType, true, limits.MaxStringBytes)
		validateString(path+".objectKey", document.ObjectKey, false, limits.MaxStringBytes)
		validateString(path+".sha256", document.SHA256, true, 64)
		if !canonicalSHA256(document.SHA256) {
			add(SeverityError, path+".sha256", "invalid_sha256", "sha256 must be 64 lowercase hexadecimal characters")
		}
		if document.SizeBytes < 0 {
			add(SeverityError, path+".sizeBytes", "invalid_size", "size must be non-negative")
		}
		if _, exists := documents[document.ID]; exists {
			add(SeverityError, path+".id", "duplicate_id", "document id must be unique")
		}
		documents[document.ID] = struct{}{}
		validateMetadata(&report, path+".metadata", document.Metadata, limits, add)
	}
	cases := make(map[string]struct{}, len(dataset.Cases))
	reference, contexts, ids := 0, 0, 0
	for i, item := range dataset.Cases {
		path := fmt.Sprintf("cases[%d]", i)
		validateString(path+".id", item.ID, true, limits.MaxIDBytes)
		validateString(path+".user_input", item.UserInput, true, limits.MaxStringBytes)
		validateString(path+".reference", item.Reference, false, limits.MaxStringBytes)
		for field, values := range map[string][]string{"reference_contexts": item.ReferenceContexts, "reference_context_ids": item.ReferenceContextIDs, "history": item.History, "tags": item.Tags} {
			if len(values) > limits.MaxItemsPerCase {
				add(SeverityError, path+"."+field, "too_many_items", "field exceeds item limit")
			}
			for index, value := range values {
				validateString(fmt.Sprintf("%s.%s[%d]", path, field, index), value, false, limits.MaxStringBytes)
			}
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
				add(SeverityError, path+".reference_context_ids", "dangling_reference", "referenced document does not exist")
			}
		}
		validateMetadata(&report, path+".metadata", item.Metadata, limits, add)
	}
	total := len(dataset.Cases)
	report.Coverage = []MetricCoverage{{"context_recall", reference, total}, {"factual_correctness", reference, total}, {"context_precision", reference, total}, {"hit_at_k", ids, total}, {"recall_at_k", ids, total}, {"mrr", ids, total}, {"ndcg", ids, total}, {"reference_context_match", contexts, total}}
	for _, coverage := range report.Coverage {
		if coverage.Eligible < coverage.Total {
			add(SeverityWarning, "cases", "metric_coverage", fmt.Sprintf("%s eligible for %d/%d cases", coverage.Metric, coverage.Eligible, coverage.Total))
		}
	}
	return report
}

type validationIssueAdder func(Severity, string, string, string)

func validateMetadata(report *ValidationReport, path string, value map[string]any, limits ValidationLimits, add validationIssueAdder) {
	if value == nil {
		return
	}
	b, err := json.Marshal(value)
	if err != nil {
		add(SeverityError, path, "invalid_json", "metadata must contain JSON-compatible values")
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
	invalidUTF8, sensitive := inspectMetadata(value)
	if invalidUTF8 {
		add(SeverityError, path, "invalid_utf8", "metadata strings must contain valid UTF-8")
	}
	if sensitive {
		add(SeverityWarning, path, "sensitive_metadata_ignored", "credential-like metadata is excluded from fingerprints and reports")
	}
}

func canonicalSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) || value != strings.TrimSpace(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func inspectMetadata(value any) (invalidUTF8, sensitive bool) {
	return inspectMetadataValue(reflect.ValueOf(value))
}

func inspectMetadataValue(value reflect.Value) (invalidUTF8, sensitive bool) {
	if !value.IsValid() {
		return false, false
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return false, false
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key()
			if key.Kind() == reflect.String {
				text := key.String()
				invalidUTF8 = invalidUTF8 || !utf8.ValidString(text)
				sensitive = sensitive || sensitiveMetadataKey(text)
			}
			childInvalid, childSensitive := inspectMetadataValue(iterator.Value())
			invalidUTF8 = invalidUTF8 || childInvalid
			sensitive = sensitive || childSensitive
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			childInvalid, childSensitive := inspectMetadataValue(value.Index(index))
			invalidUTF8 = invalidUTF8 || childInvalid
			sensitive = sensitive || childSensitive
		}
	case reflect.String:
		invalidUTF8 = !utf8.ValidString(value.String())
	}
	return invalidUTF8, sensitive
}

func sensitiveMetadataKey(key string) bool {
	normalized := strings.NewReplacer("_", "", "-", "", ".", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(key)))
	for _, marker := range []string{"apikey", "secret", "password", "credential", "authorization"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return normalized == "token" || strings.HasSuffix(normalized, "token") || normalized == "endpoint" || strings.HasSuffix(normalized, "endpoint")
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
