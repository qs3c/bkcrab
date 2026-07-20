package parse

import (
	"context"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

// Parser is the streaming document-parser boundary used by the indexing
// pipeline. Implementations own any transient bundle files until the returned
// ParsedDocument is closed.
type Parser interface {
	Parse(ctx context.Context, source document.Source, options ParseOptions) (*document.ParsedDocument, error)
}

type ParseOptions struct {
	Mode             config.ParseMode
	ParserVersion    string
	PageTranscriber  vision.PageTranscriber
	ImageTranscriber vision.ImageTranscriber
	DocumentAIBudget *vision.TaskDocumentAIBudget
	VisionScope      vision.CacheScope
	Progress         ParseProgressFunc
}

// ParseProgress is deliberately parser-local. Pipeline code can translate it
// to its fenced SQL progress DTO without making the parser depend on task
// storage or worker state.
type ParseProgress struct {
	Stage   string
	Current int
	Total   int
	Unit    string
	Message string
}

type ParseProgressFunc func(context.Context, ParseProgress) error

// PrimitiveExtractor is the narrow local-sidecar boundary. The sidecar only
// extracts deterministic Office/PDF primitives; it never receives model or
// object-store credentials and never performs VLM calls.
type PrimitiveExtractor interface {
	ConvertOffice(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error)
	AnalyzePDF(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error)
	RenderPDF(ctx context.Context, source document.Source, pages []int) (*sidecar.BundleHandle, error)
}
