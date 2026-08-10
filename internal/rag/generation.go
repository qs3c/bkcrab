package rag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

type GenerationResolutionMode string

const (
	GenerationResolutionLegacy        GenerationResolutionMode = "legacy"
	GenerationResolutionShadow        GenerationResolutionMode = "shadow"
	GenerationResolutionAuthoritative GenerationResolutionMode = "authoritative"
)

type GenerationStore interface {
	ResolveActiveRAGKBGeneration(ctx context.Context, kbID string) (*store.RAGKBGenerationRecord, []store.RAGGenerationDocumentRecord, error)
	ListRAGDocumentsByKB(ctx context.Context, kbID string) ([]store.RAGDocumentRecord, error)
}

type GenerationMismatchReporter func(ctx context.Context, kbID, reason string)

// GenerationResolver keeps the legacy path as a safe fallback while shadow
// reads validate that SQL's active document versions and generation mapping
// describe the same retrieval-visible corpus.
type GenerationResolver struct {
	store  GenerationStore
	mode   GenerationResolutionMode
	report GenerationMismatchReporter
}

func NewGenerationResolver(st GenerationStore, mode GenerationResolutionMode, report GenerationMismatchReporter) *GenerationResolver {
	if mode != GenerationResolutionShadow && mode != GenerationResolutionAuthoritative {
		mode = GenerationResolutionLegacy
	}
	if report == nil {
		report = func(ctx context.Context, kbID, reason string) {
			slog.WarnContext(ctx, "rag generation mapping mismatch; using legacy collection", "kb_id", kbID, "reason", reason)
		}
	}
	return &GenerationResolver{store: st, mode: mode, report: report}
}

func (r *GenerationResolver) ResolveCollection(ctx context.Context, kbID string) (vector.CollectionKey, error) {
	legacy, err := vector.LegacyCollectionKey(kbID)
	if err != nil {
		return "", err
	}
	if r == nil || r.mode == GenerationResolutionLegacy || r.store == nil {
		return legacy, nil
	}
	generation, mapped, err := r.store.ResolveActiveRAGKBGeneration(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return legacy, nil
	}
	if err != nil {
		return "", err
	}
	if r.mode == GenerationResolutionAuthoritative {
		if generation == nil || generation.KBID != kbID || generation.Status != store.RAGGenerationActive {
			r.report(ctx, kbID, "active generation identity/status conflict")
			return legacy, nil
		}
		key, keyErr := vector.CollectionKeyFromPersistence(generation.CollectionKey)
		if keyErr != nil {
			r.report(ctx, kbID, "invalid persisted collection key")
			return legacy, nil
		}
		return key, nil
	}
	documents, err := r.store.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		r.report(ctx, kbID, "legacy active-version read failed: "+err.Error())
		return legacy, nil
	}
	activeVersions := make(map[string]int64, len(documents))
	for _, document := range documents {
		if document.ActiveVersion > 0 && !strings.EqualFold(document.Status, store.RAGDocumentStatusDeleting) {
			activeVersions[document.ID] = document.ActiveVersion
		}
	}
	if reason := validateGenerationMapping(kbID, generation, mapped, activeVersions); reason != "" {
		r.report(ctx, kbID, reason)
	}
	return legacy, nil
}

func (r *GenerationResolver) ResolveSearchCollection(ctx context.Context, kbID string, activeVersions map[string]int64) (vector.CollectionKey, error) {
	legacy, err := vector.LegacyCollectionKey(kbID)
	if err != nil {
		return "", err
	}
	if r == nil || r.mode == GenerationResolutionLegacy || r.store == nil {
		return legacy, nil
	}
	generation, mapped, err := r.store.ResolveActiveRAGKBGeneration(ctx, kbID)
	if errors.Is(err, store.ErrNotFound) {
		return legacy, nil
	}
	if err != nil {
		return "", err
	}
	return r.resolveValidated(ctx, kbID, legacy, generation, mapped, activeVersions)
}

func (r *GenerationResolver) resolveValidated(ctx context.Context, kbID string, legacy vector.CollectionKey, generation *store.RAGKBGenerationRecord, mapped []store.RAGGenerationDocumentRecord, activeVersions map[string]int64) (vector.CollectionKey, error) {
	if reason := validateGenerationMapping(kbID, generation, mapped, activeVersions); reason != "" {
		r.report(ctx, kbID, reason)
		return legacy, nil
	}
	if r.mode != GenerationResolutionAuthoritative {
		return legacy, nil
	}
	key, err := vector.CollectionKeyFromPersistence(generation.CollectionKey)
	if err != nil {
		r.report(ctx, kbID, "invalid persisted collection key")
		return legacy, nil
	}
	return key, nil
}

func validateGenerationMapping(kbID string, generation *store.RAGKBGenerationRecord, mapped []store.RAGGenerationDocumentRecord, activeVersions map[string]int64) string {
	if generation == nil || generation.KBID != kbID || generation.Status != store.RAGGenerationActive {
		return "active generation identity/status conflict"
	}
	if len(activeVersions) != len(mapped) {
		return fmt.Sprintf("document count conflict: legacy=%d generation=%d", len(activeVersions), len(mapped))
	}
	seen := make(map[string]struct{}, len(mapped))
	for _, document := range mapped {
		if document.Status != store.RAGGenerationDocumentReady {
			return "generation contains non-ready document " + document.DocID
		}
		if _, duplicate := seen[document.DocID]; duplicate {
			return "generation contains duplicate document " + document.DocID
		}
		seen[document.DocID] = struct{}{}
		if activeVersions[document.DocID] != document.DocVersion {
			return fmt.Sprintf("active version conflict for %s: legacy=%d generation=%d", document.DocID, activeVersions[document.DocID], document.DocVersion)
		}
	}
	return ""
}
