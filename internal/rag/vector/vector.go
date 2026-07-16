// Package vector defines the narrow vector-store contract used by RAG.
//
// UpsertChunks and DeleteOldVersions deliberately remain separate operations.
// Callers can therefore finish computing and writing a new document version
// before removing the old one, so searchable data is never deleted early.
package vector

import "context"

// ChunkData is one indexed document chunk.
type ChunkData struct {
	DocID        string
	Index        int
	Content      string
	SectionTitle string
	PageNum      int
	DocVersion   int
	Vector       []float32
}

// ChunkRef identifies one exact indexed chunk version. Metadata generation
// uses refs instead of vector search so its first/middle/last sampling remains
// deterministic and does not bias the result toward a synthetic query.
type ChunkRef struct {
	DocID      string
	Index      int
	DocVersion int
}

// SearchHit is one result returned by a hybrid search.
type SearchHit struct {
	DocID        string
	ChunkIndex   int
	Content      string
	SectionTitle string
	PageNum      int
	Score        float64
}

// Store is the vector database surface needed by the RAG service.
type Store interface {
	EnsureCollection(ctx context.Context, kbID string, dims int) error
	UpsertChunks(ctx context.Context, kbID string, chunks []ChunkData) error
	// DeleteOldVersions removes entities for docID whose doc_version is less
	// than keepVersion.
	DeleteOldVersions(ctx context.Context, kbID, docID string, keepVersion int) error
	// DeleteDoc removes every indexed version of docID.
	DeleteDoc(ctx context.Context, kbID, docID string) error
	DropCollection(ctx context.Context, kbID string) error
	// HybridSearch combines dense-vector and full-text result lists with RRF.
	// An empty queryText disables the full-text route.
	HybridSearch(ctx context.Context, kbID string, queryVec []float32, queryText string, topK int) ([]SearchHit, error)
	// GetChunks returns the exact indexed chunks named by refs. Missing refs are
	// omitted, allowing callers to tolerate a concurrent reindex without ever
	// reading an older document version.
	GetChunks(ctx context.Context, kbID string, refs []ChunkRef) ([]ChunkData, error)
}
