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

// SearchQuery describes the independent routes participating in one hybrid
// retrieval. Dense contains one vector for each semantic route (normally the
// rewritten question and the HyDE document); Text drives the BM25 route.
// Every non-empty route contributes equally to RRF fusion.
type SearchQuery struct {
	Dense [][]float32
	Text  string
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
	// HybridSearch combines one or more dense-vector routes and an optional
	// full-text route with RRF. At least one dense route is required.
	HybridSearch(ctx context.Context, kbID string, query SearchQuery, topK int) ([]SearchHit, error)
	// GetChunks returns the exact indexed chunks named by refs. Missing refs are
	// omitted, allowing callers to tolerate a concurrent reindex without ever
	// reading an older document version.
	GetChunks(ctx context.Context, kbID string, refs []ChunkRef) ([]ChunkData, error)
}
