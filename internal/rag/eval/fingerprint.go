package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
)

type GenerationDocumentFingerprint struct {
	ID        string `json:"id"`
	FileName  string `json:"fileName"`
	MediaType string `json:"mediaType,omitempty"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"sizeBytes"`
}

// GenerationContract names the code and storage contracts that are not
// represented by a user-selectable ingestion profile but still affect parse,
// chunk or vector output.
type GenerationContract struct {
	ParserProtocolVersion string `json:"parserProtocolVersion"`
	ParserEngineVersion   string `json:"parserEngineVersion"`
	TokenizerVersion      string `json:"tokenizerVersion"`
	SplitterVersion       string `json:"splitterVersion"`
	ArtifactSchemaVersion int    `json:"artifactSchemaVersion"`
	VectorSchemaVersion   string `json:"vectorSchemaVersion"`
	IndexFormatVersion    int    `json:"indexFormatVersion"`
}

func (c GenerationContract) Validate() error {
	if strings.TrimSpace(c.ParserProtocolVersion) == "" || strings.TrimSpace(c.ParserEngineVersion) == "" ||
		strings.TrimSpace(c.TokenizerVersion) == "" || strings.TrimSpace(c.SplitterVersion) == "" ||
		strings.TrimSpace(c.VectorSchemaVersion) == "" || c.ArtifactSchemaVersion < 1 || c.IndexFormatVersion < 1 {
		return fmt.Errorf("generation code contract is incomplete")
	}
	return nil
}

// GenerationFingerprint binds an immutable corpus version to every ingestion
// and code contract that can affect searchable chunks or vectors. Credentials
// are absent; provider compatibility is represented only by the policy's
// irreversible contract fingerprint.
func GenerationFingerprint(datasetVersionID, corpusFingerprint string, documents []GenerationDocumentFingerprint, policy config.RAGIngestionPolicyData, contract GenerationContract) (string, error) {
	if strings.TrimSpace(datasetVersionID) == "" || strings.TrimSpace(corpusFingerprint) == "" {
		return "", fmt.Errorf("dataset version and corpus fingerprint are required")
	}
	if err := policy.Validate(); err != nil {
		return "", fmt.Errorf("invalid ingestion policy: %w", err)
	}
	if err := contract.Validate(); err != nil {
		return "", err
	}
	documents = append([]GenerationDocumentFingerprint(nil), documents...)
	for index := range documents {
		documents[index].ID = strings.TrimSpace(documents[index].ID)
		documents[index].FileName = strings.TrimSpace(documents[index].FileName)
		documents[index].MediaType = strings.TrimSpace(documents[index].MediaType)
		documents[index].SHA256 = strings.ToLower(strings.TrimSpace(documents[index].SHA256))
		decoded, err := hex.DecodeString(documents[index].SHA256)
		if documents[index].ID == "" || documents[index].FileName == "" || strings.ContainsAny(documents[index].FileName, `/\\`) ||
			documents[index].SizeBytes < 0 || err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("invalid generation document fingerprint at index %d", index)
		}
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].ID < documents[j].ID })
	for index := 1; index < len(documents); index++ {
		if documents[index-1].ID == documents[index].ID {
			return "", fmt.Errorf("duplicate generation document %q", documents[index].ID)
		}
	}
	return Fingerprint(struct {
		DatasetVersionID string                          `json:"datasetVersionId"`
		Corpus           string                          `json:"corpusFingerprint"`
		Documents        []GenerationDocumentFingerprint `json:"documents"`
		Ingestion        config.RAGIngestionPolicyData   `json:"ingestion"`
		Contract         GenerationContract              `json:"contract"`
	}{strings.TrimSpace(datasetVersionID), strings.TrimSpace(corpusFingerprint), documents, policy, contract})
}

// CorpusArtifactFingerprint contains parse-only inputs. It can therefore be
// reused across generations whose chunk or embedding settings differ, while
// the full GenerationFingerprint still protects chunk/vector reuse.
func CorpusArtifactFingerprint(document GenerationDocumentFingerprint, policy config.RAGIngestionPolicyData, contract GenerationContract) (string, error) {
	if err := policy.Validate(); err != nil {
		return "", fmt.Errorf("invalid ingestion policy: %w", err)
	}
	if err := contract.Validate(); err != nil {
		return "", err
	}
	document.ID = strings.TrimSpace(document.ID)
	document.FileName = strings.TrimSpace(document.FileName)
	document.MediaType = strings.TrimSpace(document.MediaType)
	document.SHA256 = strings.ToLower(strings.TrimSpace(document.SHA256))
	decoded, err := hex.DecodeString(document.SHA256)
	if document.ID == "" || document.FileName == "" || strings.ContainsAny(document.FileName, `/\\`) ||
		document.SizeBytes < 0 || err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("invalid corpus artifact document fingerprint")
	}
	return Fingerprint(struct {
		Document       GenerationDocumentFingerprint  `json:"document"`
		ParseMode      config.ParseMode               `json:"parseMode"`
		SelectedParser string                         `json:"selectedParser"`
		DocumentAI     config.RAGPolicyDocumentAIData `json:"documentAI"`
		ParserProtocol string                         `json:"parserProtocolVersion"`
		ParserEngine   string                         `json:"parserEngineVersion"`
		ArtifactSchema int                            `json:"artifactSchemaVersion"`
	}{document, policy.ParseMode, policy.ParserEngine, policy.DocumentAI, contract.ParserProtocolVersion, contract.ParserEngineVersion, contract.ArtifactSchemaVersion})
}

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
