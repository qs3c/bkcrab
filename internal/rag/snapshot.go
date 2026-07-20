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
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/enrich"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	splitterSchemaVersion = "search-content-v2"
)

// indexFingerprintInput is the complete persisted search/index contract. Keep
// it named so schema-version changes are testable and cannot disappear inside
// an anonymous struct refactor.
type indexFingerprintInput struct {
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
	EnrichmentSchemaVersion string `json:"enrichmentSchemaVersion,omitempty"`
}

func buildIndexFingerprint(input indexFingerprintInput) string { return fingerprint(input) }

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

	documentAIProviderFingerprint := vision.ProviderFingerprint(s.cfg.DocumentAI)
	embeddingContractFingerprint := embeddingContractFingerprintForKB(kb, embeddingCfg)
	parserVersion, markItDownVersion := parseContractVersions(doc.FileType)

	parseFingerprint, err := document.ParseFingerprint(document.ParseFingerprintInput{
		SourceSHA256:              sourceSHA256,
		ParseMode:                 string(parseMode),
		ParserVersion:             parserVersion,
		MarkItDownVersion:         markItDownVersion,
		PDFRenderDPI:              s.cfg.Limits.PDFRenderDPI,
		PDFRoutingVersion:         parse.PDFAutoRoutingVersion,
		MaxPages:                  s.cfg.Limits.MaxPagesPerDocument,
		MaxVisionPages:            s.cfg.Limits.MaxVisionPagesPerDocument,
		MaxVisionAssets:           s.cfg.Limits.MaxVisionAssetsPerDocument,
		MaxAssets:                 s.cfg.Limits.MaxAssetsPerDocument,
		MaxAssetBytes:             s.cfg.Limits.MaxAssetBytes,
		MaxExtractedBytes:         s.cfg.Limits.MaxExtractedBytes,
		MaxVisionInputBytes:       s.cfg.Limits.MaxVisionInputBytes,
		MaxImagePixels:            s.cfg.Limits.MaxImagePixels,
		DisplayMaxEdge:            s.cfg.Limits.DisplayMaxEdge,
		ThumbnailMaxEdge:          s.cfg.Limits.ThumbnailMaxEdge,
		VisionProviderFingerprint: documentAIProviderFingerprint,
		VisionModel:               strings.TrimSpace(s.cfg.DocumentAI.VisionModel),
		VisionPromptVersion:       strings.TrimSpace(s.cfg.DocumentAI.VisionPromptVersion),
		PageSchemaVersion:         vision.PageSchemaVersion,
		ImageSchemaVersion:        vision.ImageDescriptionSchemaVersion,
	})
	if err != nil {
		return nil, config.RAGEmbeddingCfg{}, fmt.Errorf("build parse fingerprint: %w", err)
	}
	indexFingerprint := buildIndexFingerprint(indexFingerprintInput{
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
		EnrichmentSchemaVersion: func() string {
			if kb.EnrichmentEnabled {
				return enrich.EnrichmentSchemaVersion
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
		ParserVersion:                parserVersion,
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

func parseContractVersions(fileType string) (parserVersion, markItDownVersion string) {
	switch strings.TrimPrefix(strings.ToLower(strings.TrimSpace(fileType)), ".") {
	case "docx", "pptx", "xlsx":
		return parse.OfficeParserVersion, parse.OfficeMarkItDownVersion
	default:
		return parse.LocalParserVersion, "none"
	}
}

// embeddingContractFingerprintForKB identifies the secret-free embedding
// contract that determines vector compatibility. Search uses the same key so
// vectors are shared only when provider routing, endpoint, model, and
// dimensions are all identical.
func embeddingContractFingerprintForKB(kb *store.RAGKBRecord, cfg config.RAGEmbeddingCfg) string {
	return fingerprint(struct {
		Provider string `json:"provider"`
		Endpoint string `json:"endpoint"`
		Model    string `json:"model"`
		Dims     int    `json:"dims"`
	}{
		Provider: kb.EmbedProvider,
		Endpoint: canonicalEndpoint(cfg.Endpoint),
		Model:    kb.EmbedModel,
		Dims:     kb.EmbedDims,
	})
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

func microUSD(value float64) int64 {
	if value <= 0 {
		return 0
	}
	return int64(math.Round(value * 1_000_000))
}

// sameRuntimeProviderContracts is deliberately narrower than the full index
// fingerprint. A queued version keeps immutable KB chunk choices, but it must
// be superseded if the running binary cannot execute its parse/search schema
// versions or if an outbound provider contract drifted.
func sameRuntimeProviderContracts(left, right *store.RAGDocumentVersionRecord) bool {
	if left == nil || right == nil ||
		left.ParserVersion != right.ParserVersion ||
		left.ParseFingerprint != right.ParseFingerprint ||
		left.SplitterVersion != right.SplitterVersion ||
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
