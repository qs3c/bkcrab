package rag

import (
	"context"
	"errors"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

type IngestionPolicyDifference struct {
	Field string `json:"field"`
	From  any    `json:"from,omitempty"`
	To    any    `json:"to,omitempty"`
}

type IngestionPolicyEstimate struct {
	DocumentCount     int   `json:"documentCount"`
	SourceBytes       int64 `json:"sourceBytes"`
	TemporaryBytesMax int64 `json:"temporaryBytesMax"`
}

type KBIngestionPolicyStatus struct {
	PinnedVersion         int64                       `json:"pinnedVersion"`
	LatestVersion         int64                       `json:"latestVersion"`
	Drift                 bool                        `json:"drift"`
	FullCollectionRebuild bool                        `json:"fullCollectionRebuild"`
	Differences           []IngestionPolicyDifference `json:"differences"`
	Estimate              IngestionPolicyEstimate     `json:"estimate"`
}

// GetKBIngestionPolicyStatus is deliberately read-only. Drift never changes a
// KB health/status and never blocks reads or same-policy document ingestion.
func (s *Service) GetKBIngestionPolicyStatus(ctx context.Context, ownerID, kbID string) (KBIngestionPolicyStatus, error) {
	kb, err := s.GetKB(ctx, ownerID, kbID)
	if err != nil {
		return KBIngestionPolicyStatus{}, err
	}
	if !kb.PinnedPolicyVersion.Valid {
		return KBIngestionPolicyStatus{}, errors.New("knowledge base has no pinned ingestion policy")
	}
	pinnedRecord, err := s.st.GetRAGPolicy(ctx, store.RAGPolicyIngestion, kb.PinnedPolicyVersion.Int64)
	if err != nil {
		return KBIngestionPolicyStatus{}, err
	}
	latestRecord, err := s.st.ActiveRAGPolicy(ctx, store.RAGPolicyIngestion)
	if err != nil {
		return KBIngestionPolicyStatus{}, err
	}
	pinned, err := config.DecodeRAGIngestionPolicy([]byte(pinnedRecord.PolicyJSON))
	if err != nil {
		// Legacy backfill revisions use a richer corpus snapshot schema. Keep
		// the read model available by projecting the KB's immutable contract;
		// the opaque legacy fingerprint conservatively reports embedding rebuild.
		pinned = config.RAGIngestionPolicyData{
			Version: kb.PinnedPolicyVersion.Int64, ChunkSize: kb.ChunkSize,
			ChunkOverlap: kb.ChunkOverlap, ParseMode: config.ParseMode(kb.ParseMode),
			EnrichmentEnabled: kb.EnrichmentEnabled,
			Embedding:         config.RAGPolicyEmbeddingData{ContractFingerprint: pinnedRecord.Fingerprint, Model: kb.EmbedModel, Dims: kb.EmbedDims},
		}
	}
	latest, err := config.DecodeRAGIngestionPolicy([]byte(latestRecord.PolicyJSON))
	if err != nil {
		return KBIngestionPolicyStatus{}, err
	}
	differences := ingestionPolicyDifferences(pinned, latest)
	documents, err := s.st.ListRAGDocumentsByKB(ctx, kb.ID)
	if err != nil {
		return KBIngestionPolicyStatus{}, err
	}
	var bytes int64
	for _, document := range documents {
		bytes += document.FileSize
	}
	return KBIngestionPolicyStatus{
		PinnedVersion: kb.PinnedPolicyVersion.Int64, LatestVersion: latestRecord.Version,
		Drift:                 kb.PinnedPolicyVersion.Int64 != latestRecord.Version,
		FullCollectionRebuild: pinned.Embedding != latest.Embedding,
		Differences:           differences,
		Estimate:              IngestionPolicyEstimate{DocumentCount: len(documents), SourceBytes: bytes, TemporaryBytesMax: bytes * 2},
	}, nil
}

func ingestionPolicyDifferences(from, to config.RAGIngestionPolicyData) []IngestionPolicyDifference {
	diffs := make([]IngestionPolicyDifference, 0, 10)
	add := func(field string, left, right any, changed bool) {
		if changed {
			diffs = append(diffs, IngestionPolicyDifference{Field: field, From: left, To: right})
		}
	}
	add("chunkSize", from.ChunkSize, to.ChunkSize, from.ChunkSize != to.ChunkSize)
	add("chunkOverlap", from.ChunkOverlap, to.ChunkOverlap, from.ChunkOverlap != to.ChunkOverlap)
	add("parseMode", from.ParseMode, to.ParseMode, from.ParseMode != to.ParseMode)
	add("enrichmentEnabled", from.EnrichmentEnabled, to.EnrichmentEnabled, from.EnrichmentEnabled != to.EnrichmentEnabled)
	add("documentAI.visionModel", from.DocumentAI.VisionModel, to.DocumentAI.VisionModel, from.DocumentAI.VisionModel != to.DocumentAI.VisionModel)
	add("documentAI.textModel", from.DocumentAI.TextModel, to.DocumentAI.TextModel, from.DocumentAI.TextModel != to.DocumentAI.TextModel)
	add("documentAI.visionPromptVersion", from.DocumentAI.VisionPromptVersion, to.DocumentAI.VisionPromptVersion, from.DocumentAI.VisionPromptVersion != to.DocumentAI.VisionPromptVersion)
	add("documentAI.enrichmentPromptVersion", from.DocumentAI.EnrichmentPromptVersion, to.DocumentAI.EnrichmentPromptVersion, from.DocumentAI.EnrichmentPromptVersion != to.DocumentAI.EnrichmentPromptVersion)
	add("embedding.contractFingerprint", from.Embedding.ContractFingerprint, to.Embedding.ContractFingerprint, from.Embedding.ContractFingerprint != to.Embedding.ContractFingerprint)
	add("embedding.model", from.Embedding.Model, to.Embedding.Model, from.Embedding.Model != to.Embedding.Model)
	add("embedding.dims", from.Embedding.Dims, to.Embedding.Dims, from.Embedding.Dims != to.Embedding.Dims)
	return diffs
}
