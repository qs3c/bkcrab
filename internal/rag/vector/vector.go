// Package vector defines the narrow vector-store contract used by RAG.
//
// UpsertChunks and version deletion deliberately remain separate operations.
// Callers can therefore finish computing and writing a new document version
// before removing the old one, so searchable data is never deleted early.
package vector

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
)

// CollectionKey is an opaque, server-resolved vector target. HTTP handlers and
// client DTOs must continue accepting logical KB IDs only.
type CollectionKey string

func LegacyCollectionKey(kbID string) (CollectionKey, error) {
	kbID = strings.TrimSpace(kbID)
	if kbID == "" || len(kbID) > 255 {
		return "", errors.New("invalid legacy collection identity")
	}
	return CollectionKey(kbID), nil
}

// CollectionKeyFromPersistence validates the trusted SQL representation before
// it crosses into a vector driver. Physical keys are never accepted from API
// requests.
func CollectionKeyFromPersistence(value string) (CollectionKey, error) {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 255 {
		return "", errors.New("invalid persisted collection key")
	}
	return CollectionKey(value), nil
}

// GenerationCollectionKey derives a bounded key without embedding logical KB
// names or other user text. generationID is a server-generated identifier and
// remains visible in the physical name for operational correlation.
func GenerationCollectionKey(logicalKBID, generationID string) (CollectionKey, error) {
	logicalKBID = strings.TrimSpace(logicalKBID)
	generationID = strings.TrimSpace(generationID)
	if logicalKBID == "" || generationID == "" || len(logicalKBID) > 255 || len(generationID) > 120 {
		return "", errors.New("invalid generation collection identity")
	}
	for _, char := range generationID {
		if char == '_' || char == '-' || char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
			continue
		}
		return "", errors.New("invalid generation id")
	}
	hash := sha256.Sum256([]byte(logicalKBID))
	return CollectionKey(fmt.Sprintf("kb_%x_g_%s", hash[:8], generationID)), nil
}

// ChunkData is one indexed document chunk.
type ChunkData struct {
	DocID         string
	Index         int
	Content       string // original body returned to retrieval callers
	SearchContent string // heading-prefixed text used by embedding and BM25
	SectionTitle  string
	PageNum       int
	DocVersion    int64
	Vector        []float32
}

// ChunkRef identifies one exact indexed chunk version. Metadata generation
// uses refs instead of vector search so its first/middle/last sampling remains
// deterministic and does not bias the result toward a synthetic query.
type ChunkRef struct {
	DocID      string
	Index      int
	DocVersion int64
}

// SearchHit is one result returned by a hybrid search.
type SearchHit struct {
	DocID         string
	ChunkIndex    int
	Content       string
	SearchContent string
	SectionTitle  string
	PageNum       int
	DocVersion    int64
	Score         float64
}

// SearchQuery describes the independent routes participating in one hybrid
// retrieval. Dense contains one vector for each semantic route (normally the
// rewritten question and the HyDE document); Text drives the BM25 route.
// Every non-empty route contributes equally to RRF fusion.
type SearchQuery struct {
	Dense [][]float32
	Text  string
	// ActiveVersions is the SQL snapshot captured at retrieval start. Every
	// ANN route must apply the predicate derived from this exact map; result
	// hydration repeats the same check defensively.
	ActiveVersions map[string]int64
	MaxFilterBytes int
}

// Store is the vector database surface needed by the RAG service.
type Store interface {
	EnsureCollection(ctx context.Context, collectionKey CollectionKey, dims int) error
	UpsertChunks(ctx context.Context, collectionKey CollectionKey, chunks []ChunkData) error
	// DeleteDocVersion removes only the entities for the exact physical
	// doc_version. Delayed cleanup must use this method so one retired version
	// cannot delete a newer version whose grace period has not elapsed.
	DeleteDocVersion(ctx context.Context, collectionKey CollectionKey, docID string, version int64) error
	// DeleteDoc removes every indexed version of docID.
	DeleteDoc(ctx context.Context, collectionKey CollectionKey, docID string) error
	DropCollection(ctx context.Context, collectionKey CollectionKey) error
	// HybridSearch combines one or more dense-vector routes and an optional
	// full-text route with RRF. At least one dense route is required.
	HybridSearch(ctx context.Context, collectionKey CollectionKey, query SearchQuery, topK int) ([]SearchHit, error)
	// GetChunks returns the exact indexed chunks named by refs. Missing refs are
	// omitted, allowing callers to tolerate a concurrent reindex without ever
	// reading an older document version.
	GetChunks(ctx context.Context, collectionKey CollectionKey, refs []ChunkRef) ([]ChunkData, error)
}
