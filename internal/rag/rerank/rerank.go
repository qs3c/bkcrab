// Package rerank defines the optional second-stage relevance model used by
// RAG retrieval. Its narrow contract keeps provider-specific HTTP formats out
// of the retrieval pipeline.
package rerank

import "context"

// Result maps one reranked document back to its input position.
type Result struct {
	Index int
	Score float64
}

// Reranker scores documents against a query and returns at most topN results
// in descending relevance order.
type Reranker interface {
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]Result, error)
}
