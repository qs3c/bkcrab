package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/qs3c/bkcrab/internal/config"
)

func Fingerprint(value any) (string, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func DatasetFingerprint(dataset CanonicalDataset) (string, error) {
	if report := ValidateDataset(dataset, DefaultValidationLimits()); !report.Valid {
		return "", fmt.Errorf("invalid dataset: %d validation error(s)", validationErrorCount(report))
	}
	type doc struct {
		ID, FileName, MediaType, SHA256 string
		SizeBytes                       int64
		Metadata                        map[string]any
	}
	documents := make([]doc, len(dataset.Corpus))
	for i, item := range dataset.Corpus {
		documents[i] = doc{item.ID, item.FileName, item.MediaType, item.SHA256, item.SizeBytes, canonicalFingerprintMetadata(item.Metadata)}
	}
	cases := append([]Case(nil), dataset.Cases...)
	for index := range cases {
		cases[index].Metadata = canonicalFingerprintMetadata(cases[index].Metadata)
		cases[index].Tags = append([]string(nil), cases[index].Tags...)
		sort.Strings(cases[index].Tags)
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })
	return Fingerprint(struct {
		Corpus []doc  `json:"corpus"`
		Cases  []Case `json:"cases"`
	}{documents, cases})
}

func IngestionFingerprint(policy config.RAGIngestionPolicyData) (string, error) {
	if err := policy.Validate(); err != nil {
		return "", fmt.Errorf("invalid ingestion policy: %w", err)
	}
	return Fingerprint(policy)
}

func ProfileFingerprint(profile Profile) (string, error) {
	if err := profile.Data.Validate(); err != nil {
		return "", fmt.Errorf("invalid profile: %w", err)
	}
	return Fingerprint(profile.Data)
}

func canonicalFingerprintMetadata(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil
	}
	out := make(map[string]any, len(normalized))
	for key, child := range normalized {
		if sensitiveMetadataKey(key) {
			continue
		}
		out[key] = canonicalFingerprintValue(child)
	}
	return out
}

func canonicalFingerprintValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return canonicalFingerprintMetadata(typed)
	case []any:
		out := make([]any, len(typed))
		for index, child := range typed {
			out[index] = canonicalFingerprintValue(child)
		}
		return out
	default:
		return value
	}
}

func validationErrorCount(report ValidationReport) int {
	count := 0
	for _, issue := range report.Issues {
		if issue.Severity == SeverityError {
			count++
		}
	}
	return count
}
