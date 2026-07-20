package parse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
)

// LocalParser supplies the deterministic MD/TXT/native-PDF portion of the
// parser facade. Office conversion and PDF visual routing remain capability
// gated; a missing PDF sidecar always degrades to bounded native extraction.
type LocalParser struct {
	Primitives        PrimitiveExtractor
	MaxPages          int
	MaxExtractedBytes int64
	TempDir           string
}

func NewLocalParser(primitives PrimitiveExtractor, maxPages int, maxExtractedBytes ...int64) *LocalParser {
	extractedLimit := int64(defaultMaxNativePDFExtractedBytes)
	if len(maxExtractedBytes) > 0 && maxExtractedBytes[0] > 0 {
		extractedLimit = maxExtractedBytes[0]
	}
	return &LocalParser{
		Primitives: primitives, MaxPages: maxPages, MaxExtractedBytes: extractedLimit,
	}
}

func (p *LocalParser) Parse(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
) (*document.ParsedDocument, error) {
	if err := source.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceIntegrity, err)
	}
	if options.Mode == "" {
		options.Mode = config.ParseModeStandard
	}
	if !options.Mode.Valid() {
		return nil, fmt.Errorf("invalid parse mode %q", options.Mode)
	}
	format := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(source.Format)), ".")
	switch format {
	case "md", "markdown":
		return parseMarkdown(ctx, source, options)
	case "txt":
		return parseText(ctx, source, options)
	case "pdf":
		parsed, err := parseNativePDFWithLimit(
			ctx, source, options, p.MaxPages, p.MaxExtractedBytes, p.TempDir,
		)
		if err != nil {
			return nil, err
		}
		if options.Mode == config.ParseModeAuto {
			location := document.SourceLocation{Kind: document.LocationDocument}
			parsed.Warnings = append(parsed.Warnings, document.ParseWarning{
				Code: "pdf_auto_unavailable", Message: "PDF auto parser is unavailable; native text was used",
				Location: &location, Degraded: true,
			})
		}
		return parsed, nil
	case "docx", "pptx", "xlsx":
		if p.Primitives == nil {
			return nil, sidecar.ErrCapabilityUnavailable
		}
		// Task 16 owns the format-specific occurrence placement and typed image
		// description integration. Never fall back to the legacy DOCX XML parser.
		return nil, sidecar.ErrCapabilityUnavailable
	default:
		return nil, fmt.Errorf("unsupported document format %q", format)
	}
}

var _ Parser = (*LocalParser)(nil)

func isCapabilityUnavailable(err error) bool {
	return errors.Is(err, sidecar.ErrCapabilityUnavailable)
}
