package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	builtinParserVersion        = "builtin-parser-v1"
	parsedArtifactSchemaVersion = "parsed-artifact-v1"
	splitterSchemaVersion       = "search-content-v1"
)

// BuildVersionSnapshot derives the immutable, secret-free execution contract
// for a document from the current KB and runtime configuration. It is also the
// runtime SnapshotBuilder used to finish the staged legacy-task migration.
func (s *Service) BuildVersionSnapshot(
	ctx context.Context,
	doc *store.RAGDocumentRecord,
) (*store.RAGDocumentVersionRecord, error) {
	snapshot, _, err := s.buildVersionSnapshotAndBinding(ctx, doc)
	return snapshot, err
}

// buildVersionSnapshotAndBinding resolves the embedding binding exactly once
// and returns the same endpoint/key material that produced the snapshot
// fingerprint. A worker must use this binding for its outbound embedding call
// to avoid a provider-resolution TOCTOU within one claimed version.
func (s *Service) buildVersionSnapshotAndBinding(
	ctx context.Context,
	doc *store.RAGDocumentRecord,
) (*store.RAGDocumentVersionRecord, config.RAGEmbeddingCfg, error) {
	if doc == nil || strings.TrimSpace(doc.ID) == "" {
		return nil, config.RAGEmbeddingCfg{}, errors.New("RAG snapshot requires a document")
	}
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return nil, config.RAGEmbeddingCfg{}, err
	}

	sourceSHA256 := strings.ToLower(strings.TrimSpace(doc.SourceSHA256))
	if !validSHA256Hex(sourceSHA256) {
		sourceSHA256, err = s.hashSourceObject(ctx, doc.ObjectKey)
		if err != nil {
			return nil, config.RAGEmbeddingCfg{}, fmt.Errorf("hash RAG source object: %w", err)
		}
	}

	parseMode := config.ParseMode(kb.ParseMode)
	if !parseMode.Valid() {
		return nil, config.RAGEmbeddingCfg{}, fmt.Errorf("invalid knowledge-base parse mode %q", kb.ParseMode)
	}
	embeddingCfg, err := s.embeddingConfigForKB(ctx, kb)
	if err != nil {
		return nil, config.RAGEmbeddingCfg{}, err
	}

	documentAIProviderFingerprint := fingerprint(struct {
		APIType              string   `json:"apiType"`
		Endpoint             string   `json:"endpoint"`
		AllowedEndpointHosts []string `json:"allowedEndpointHosts"`
		AllowPrivateEndpoint bool     `json:"allowPrivateEndpoint"`
	}{
		APIType:              strings.TrimSpace(s.cfg.DocumentAI.APIType),
		Endpoint:             canonicalEndpoint(s.cfg.DocumentAI.Endpoint),
		AllowedEndpointHosts: canonicalStrings(s.cfg.DocumentAI.AllowedEndpointHosts),
		AllowPrivateEndpoint: s.cfg.DocumentAI.AllowPrivateEndpoint,
	})
	embeddingContractFingerprint := fingerprint(struct {
		Provider string `json:"provider"`
		Endpoint string `json:"endpoint"`
		Model    string `json:"model"`
		Dims     int    `json:"dims"`
	}{
		Provider: kb.EmbedProvider,
		Endpoint: canonicalEndpoint(embeddingCfg.Endpoint),
		Model:    kb.EmbedModel,
		Dims:     kb.EmbedDims,
	})

	parseFingerprint := fingerprint(struct {
		SourceSHA256              string `json:"sourceSha256"`
		ParseMode                 string `json:"parseMode"`
		ArtifactSchemaVersion     string `json:"artifactSchemaVersion"`
		ParserVersion             string `json:"parserVersion"`
		MarkItDownVersion         string `json:"markItDownVersion"`
		PDFRenderDPI              int    `json:"pdfRenderDpi"`
		VisionProviderFingerprint string `json:"visionProviderFingerprint"`
		VisionModel               string `json:"visionModel"`
		VisionPromptVersion       string `json:"visionPromptVersion"`
	}{
		SourceSHA256:              sourceSHA256,
		ParseMode:                 string(parseMode),
		ArtifactSchemaVersion:     parsedArtifactSchemaVersion,
		ParserVersion:             builtinParserVersion,
		MarkItDownVersion:         "none",
		PDFRenderDPI:              s.cfg.Limits.PDFRenderDPI,
		VisionProviderFingerprint: documentAIProviderFingerprint,
		VisionModel:               strings.TrimSpace(s.cfg.DocumentAI.VisionModel),
		VisionPromptVersion:       strings.TrimSpace(s.cfg.DocumentAI.VisionPromptVersion),
	})
	indexFingerprint := fingerprint(struct {
		ParseFingerprint        string `json:"parseFingerprint"`
		ChunkSize               int    `json:"chunkSize"`
		ChunkOverlap            int    `json:"chunkOverlap"`
		SplitterSchemaVersion   string `json:"splitterSchemaVersion"`
		EmbeddingModel          string `json:"embeddingModel"`
		EmbeddingDimensions     int    `json:"embeddingDimensions"`
		EmbeddingContract       string `json:"embeddingContract"`
		EnrichmentEnabled       bool   `json:"enrichmentEnabled"`
		TextProviderFingerprint string `json:"textProviderFingerprint,omitempty"`
		TextModel               string `json:"textModel,omitempty"`
		EnrichmentPromptVersion string `json:"enrichmentPromptVersion,omitempty"`
	}{
		ParseFingerprint:      parseFingerprint,
		ChunkSize:             kb.ChunkSize,
		ChunkOverlap:          kb.ChunkOverlap,
		SplitterSchemaVersion: splitterSchemaVersion,
		EmbeddingModel:        kb.EmbedModel,
		EmbeddingDimensions:   kb.EmbedDims,
		EmbeddingContract:     embeddingContractFingerprint,
		EnrichmentEnabled:     kb.EnrichmentEnabled,
		TextProviderFingerprint: func() string {
			if kb.EnrichmentEnabled {
				return documentAIProviderFingerprint
			}
			return ""
		}(),
		TextModel: func() string {
			if kb.EnrichmentEnabled {
				return strings.TrimSpace(s.cfg.DocumentAI.TextModel)
			}
			return ""
		}(),
		EnrichmentPromptVersion: func() string {
			if kb.EnrichmentEnabled {
				return strings.TrimSpace(s.cfg.DocumentAI.EnrichmentPromptVersion)
			}
			return ""
		}(),
	})

	return &store.RAGDocumentVersionRecord{
		DocID:                        doc.ID,
		DocVersion:                   doc.Version,
		Status:                       store.RAGDocumentVersionPending,
		SourceSHA256:                 sourceSHA256,
		ParseMode:                    string(parseMode),
		ChunkSize:                    kb.ChunkSize,
		ChunkOverlap:                 kb.ChunkOverlap,
		ParserVersion:                builtinParserVersion,
		SplitterVersion:              splitterSchemaVersion,
		ParseFingerprint:             parseFingerprint,
		IndexFingerprint:             indexFingerprint,
		VisionModel:                  strings.TrimSpace(s.cfg.DocumentAI.VisionModel),
		VisionProviderFingerprint:    documentAIProviderFingerprint,
		VisionPromptVersion:          strings.TrimSpace(s.cfg.DocumentAI.VisionPromptVersion),
		TextModel:                    strings.TrimSpace(s.cfg.DocumentAI.TextModel),
		TextProviderFingerprint:      documentAIProviderFingerprint,
		EnrichmentPromptVersion:      strings.TrimSpace(s.cfg.DocumentAI.EnrichmentPromptVersion),
		EnrichmentEnabled:            kb.EnrichmentEnabled,
		MaxDocumentAIRequests:        s.cfg.Limits.MaxDocumentAIRequests,
		MaxDocumentAITokens:          s.cfg.Limits.MaxDocumentAITokens,
		MaxDocumentAICostMicroUSD:    microUSD(s.cfg.Limits.MaxEstimatedDocumentAICostUSD),
		EmbeddingProvider:            kb.EmbedProvider,
		EmbeddingModel:               kb.EmbedModel,
		EmbeddingDimensions:          kb.EmbedDims,
		EmbeddingContractFingerprint: embeddingContractFingerprint,
	}, embeddingCfg, nil
}

func validSHA256Hex(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func (s *Service) hashSourceObject(ctx context.Context, key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", errors.New("source object key is empty")
	}
	r, err := s.obj.Get(ctx, key)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, copyErr := io.Copy(h, r)
	closeErr := r.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fingerprint(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal fixed RAG fingerprint contract: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func canonicalEndpoint(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func canonicalStrings(values []string) []string {
	canonical := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			canonical = append(canonical, value)
		}
	}
	sort.Strings(canonical)
	return canonical
}

func microUSD(value float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(math.Round(value * 1_000_000))
}

// sameRuntimeProviderContracts is deliberately narrower than the full index
// fingerprint. A queued version keeps its immutable chunk/parser choices even
// if the KB is edited later; only a provider endpoint/model contract drift can
// make continuing that physical version unsafe.
func sameRuntimeProviderContracts(left, right *store.RAGDocumentVersionRecord) bool {
	if left == nil || right == nil ||
		left.EmbeddingContractFingerprint != right.EmbeddingContractFingerprint ||
		left.EmbeddingProvider != right.EmbeddingProvider ||
		left.EmbeddingModel != right.EmbeddingModel ||
		left.EmbeddingDimensions != right.EmbeddingDimensions {
		return false
	}
	if left.ParseMode == store.RAGParseModeAuto {
		if left.VisionProviderFingerprint != right.VisionProviderFingerprint ||
			left.VisionModel != right.VisionModel ||
			left.VisionPromptVersion != right.VisionPromptVersion {
			return false
		}
	}
	if left.EnrichmentEnabled {
		if left.TextProviderFingerprint != right.TextProviderFingerprint ||
			left.TextModel != right.TextModel ||
			left.EnrichmentPromptVersion != right.EnrichmentPromptVersion {
			return false
		}
	}
	return true
}
