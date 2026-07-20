package parse

import (
	"context"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
)

// Parser is the streaming document-parser boundary used by the indexing
// pipeline. Implementations own any transient bundle files until the returned
// ParsedDocument is closed.
type Parser interface {
	Parse(ctx context.Context, source document.Source, options ParseOptions) (*document.ParsedDocument, error)
}

type ParseOptions struct {
	Mode          config.ParseMode
	ParserVersion string
}

// PrimitiveExtractor is the narrow local-sidecar boundary. The sidecar only
// extracts deterministic Office/PDF primitives; it never receives model or
// object-store credentials and never performs VLM calls.
type PrimitiveExtractor interface {
	ConvertOffice(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error)
	AnalyzePDF(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error)
	RenderPDF(ctx context.Context, source document.Source, pages []int) (*sidecar.BundleHandle, error)
}
