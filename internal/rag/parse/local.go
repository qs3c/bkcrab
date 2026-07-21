package parse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
)

// LocalParser supplies the deterministic MD/TXT/native-PDF portion of the
// parser facade. Office conversion and PDF visual routing remain capability
// gated; a missing PDF sidecar always degrades to bounded native extraction.
type LocalParser struct {
	Primitives            PrimitiveExtractor
	MaxPages              int
	MaxAssets             int
	MaxVisionPages        int
	MaxVisionAssets       int
	MaxExtractedBytes     int64
	MaxAssetBytes         int64
	MaxVisionInputBytes   int64
	MaxImagePixels        int64
	VisionImageMaxEdge    int
	MinVisualAreaPermille int
	TempDir               string
	recorder              telemetry.Recorder
}

func NewLocalParser(primitives PrimitiveExtractor, maxPages int, maxExtractedBytes ...int64) *LocalParser {
	extractedLimit := int64(defaultMaxNativePDFExtractedBytes)
	if len(maxExtractedBytes) > 0 && maxExtractedBytes[0] > 0 {
		extractedLimit = maxExtractedBytes[0]
	}
	return &LocalParser{
		Primitives: primitives, MaxPages: maxPages, MaxExtractedBytes: extractedLimit,
		recorder: telemetry.NewSlogRecorder(nil),
	}
}

// SetRecorder replaces the privacy-safe parser telemetry sink. Parsed text,
// captions/OCR, temporary paths and object keys are not representable by the
// event schema.
func (p *LocalParser) SetRecorder(recorder telemetry.Recorder) {
	if p == nil {
		return
	}
	if recorder == nil {
		recorder = telemetry.NopRecorder()
	}
	p.recorder = recorder
}

func (p *LocalParser) Parse(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
) (parsed *document.ParsedDocument, resultErr error) {
	started := time.Now()
	format := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(source.Format)), ".")
	defer func() {
		p.recordDocument(ctx, source.DocID, format, options, parsed, resultErr, time.Since(started))
	}()
	if err := source.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSourceIntegrity, err)
	}
	if options.Mode == "" {
		options.Mode = config.ParseModeStandard
	}
	if !options.Mode.Valid() {
		return nil, fmt.Errorf("invalid parse mode %q", options.Mode)
	}
	switch format {
	case "md", "markdown":
		return parseMarkdown(ctx, source, options)
	case "txt":
		return parseText(ctx, source, options)
	case "pdf":
		if options.Mode == config.ParseModeAuto && p.Primitives != nil {
			return p.parseAutoPDF(ctx, source, options)
		}
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
		// Modern Office formats always use the sidecar. A conversion failure is
		// explicit and must never fall back to the legacy DOCX XML parser.
		return p.parseOffice(ctx, source, format, options)
	default:
		return nil, fmt.Errorf("unsupported document format %q", format)
	}
}

func (p *LocalParser) recordDocument(
	ctx context.Context,
	docID, format string,
	options ParseOptions,
	parsed *document.ParsedDocument,
	err error,
	duration time.Duration,
) {
	if p == nil {
		return
	}
	fields := telemetry.Fields{
		DocID: docID, Format: format, ParseMode: string(options.Mode),
		ParserVersion: options.ParserVersion, Duration: duration, Outcome: "ok",
	}
	if parsed != nil {
		fields.ParserVersion = parsed.Parser.Version
		fields.PageCount = parserPageCount(parsed.Units)
		if format == "pdf" && options.Mode != config.ParseModeAuto {
			fields.NativePages = fields.PageCount
		}
		fields.AssetCount = len(parsed.Assets)
		fields.WarningCount = len(parsed.Warnings)
		degradedPages := make(map[int]struct{})
		for _, occurrence := range parsed.Occurrences {
			if occurrence.Decorative {
				fields.Decorative++
			}
		}
		for _, warning := range parsed.Warnings {
			if warning.Degraded && warning.Location != nil && warning.Location.Kind == document.LocationPage && warning.Location.Index > 0 {
				degradedPages[warning.Location.Index] = struct{}{}
			}
		}
		fields.DegradedPages = len(degradedPages)
	}
	if fields.ParseMode == "" {
		fields.ParseMode = string(config.ParseModeStandard)
	}
	if err != nil {
		fields.Outcome = "error"
		fields.ErrorCode = parserErrorCode(err)
	}
	telemetry.Emit(ctx, p.recorder, telemetry.EventParserDocument, fields)
}

func parserPageCount(units []document.MarkdownUnit) int {
	count := 0
	for _, unit := range units {
		if unit.Location.Kind == document.LocationPage {
			count++
		}
	}
	return count
}

func parserErrorCode(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrDocumentLimitExceeded), errors.Is(err, sidecar.ErrBundleLimitExceeded),
		errors.Is(err, sidecar.ErrSourceLimitExceeded):
		return "limit_exceeded"
	case errors.Is(err, ErrSourceIntegrity), errors.Is(err, sidecar.ErrSourceIntegrity):
		return "source_integrity"
	case errors.Is(err, sidecar.ErrCapabilityUnavailable):
		return "capability_unavailable"
	case errors.Is(err, sidecar.ErrInvalidBundle):
		return "invalid_bundle"
	case errors.Is(err, ErrEmptyContent):
		return "empty_content"
	case errors.Is(err, ErrInvalidDocument):
		return "invalid_document"
	default:
		return "parser_error"
	}
}

var _ Parser = (*LocalParser)(nil)

func isCapabilityUnavailable(err error) bool {
	return errors.Is(err, sidecar.ErrCapabilityUnavailable)
}
