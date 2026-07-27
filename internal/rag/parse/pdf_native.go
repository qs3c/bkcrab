package parse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	pdflib "github.com/ledongthuc/pdf"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const (
	defaultMaxNativePDFPages          = 300
	defaultMaxNativePDFExtractedBytes = 200 << 20
)

func parseNativePDF(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
) (*document.ParsedDocument, error) {
	return parseNativePDFWithLimit(
		ctx, source, options, defaultMaxNativePDFPages, defaultMaxNativePDFExtractedBytes, "",
	)
}

func parseNativePDFWithLimit(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
	maxPages int,
	maxExtractedBytes int64,
	tempDir string,
) (parsed *document.ParsedDocument, resultErr error) {
	defer func() {
		if recover() == nil {
			return
		}
		if parsed != nil {
			_ = parsed.Close()
			parsed = nil
		}
		resultErr = errors.Join(
			fmt.Errorf("%w: malformed PDF structure", ErrInvalidDocument),
			resultErr,
		)
	}()
	spool, cleanup, err := spoolSourceFile(ctx, source, tempDir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			if parsed != nil {
				_ = parsed.Close()
				parsed = nil
			}
			resultErr = errors.Join(resultErr, fmt.Errorf("cleanup PDF source spool: %w", cleanupErr))
		}
	}()

	reader, err := pdflib.NewReader(spool, source.Size)
	if err != nil {
		return nil, fmt.Errorf("%w: pdf parse: %v", ErrInvalidDocument, err)
	}
	if maxPages <= 0 {
		maxPages = defaultMaxNativePDFPages
	}
	if maxExtractedBytes <= 0 {
		maxExtractedBytes = defaultMaxNativePDFExtractedBytes
	}
	pageCount := reader.NumPage()
	if pageCount < 0 {
		return nil, fmt.Errorf("%w: PDF page count is negative", ErrInvalidDocument)
	}
	if pageCount == 0 {
		return nil, ErrEmptyContent
	}
	if pageCount > maxPages {
		return nil, fmt.Errorf("%w: pdf has %d pages, limit is %d", ErrDocumentLimitExceeded, pageCount, maxPages)
	}

	units := make([]document.MarkdownUnit, 0, pageCount)
	warnings := make([]document.ParseWarning, 0)
	var decodedContentBytes int64
	var extractedMarkdownBytes int64
	for pageNumber := 1; pageNumber <= pageCount; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		location := document.SourceLocation{
			Kind:  document.LocationPage,
			Index: pageNumber,
			Label: fmt.Sprintf("第 %d 页", pageNumber),
		}
		page := reader.Page(pageNumber)
		if page.V.IsNull() {
			warnings = append(warnings, document.ParseWarning{
				Code: "pdf_native_page_invalid", Message: "PDF page could not be opened",
				Location: &location, Degraded: true,
			})
			continue
		}
		if err := consumePDFContentStreams(
			ctx, page.V.Key("Contents"), &decodedContentBytes, maxExtractedBytes,
		); err != nil {
			return nil, err
		}
		plain, pageErr := page.GetPlainText(nil)
		if pageErr != nil {
			warnings = append(warnings, document.ParseWarning{
				Code: "pdf_native_page_failed", Message: "PDF text layer extraction failed",
				Location: &location, Degraded: true,
			})
			continue
		}
		if strings.TrimSpace(plain) == "" {
			warnings = append(warnings, document.ParseWarning{
				Code: "pdf_native_page_empty", Message: "PDF page has no extractable text",
				Location: &location, Degraded: true,
			})
			continue
		}
		markdown := plainTextToMarkdown(plain)
		if int64(len(markdown)) > maxExtractedBytes-extractedMarkdownBytes {
			return nil, fmt.Errorf("%w: native PDF text exceeds %d bytes", ErrDocumentLimitExceeded, maxExtractedBytes)
		}
		extractedMarkdownBytes += int64(len(markdown))
		units = append(units, document.MarkdownUnit{
			ID:       fmt.Sprintf("unit_page_%s_%04d", source.SHA256[:12], pageNumber),
			Location: location,
			Markdown: markdown,
		})
	}
	if len(units) == 0 {
		return nil, ErrEmptyContent
	}
	units, warnings, err = normalizeLocalUnits(units, warnings)
	if err != nil {
		return nil, err
	}
	units, warnings = retainNonEmptyPDFUnits(units, warnings)
	normalizedBytes := int64(0)
	for _, unit := range units {
		if int64(len(unit.Markdown)) > maxExtractedBytes-normalizedBytes {
			return nil, fmt.Errorf("%w: normalized PDF text exceeds %d bytes", ErrDocumentLimitExceeded, maxExtractedBytes)
		}
		normalizedBytes += int64(len(unit.Markdown))
	}
	if len(units) == 0 {
		return nil, ErrEmptyContent
	}
	return newLocalParsedDocument(source, "go-pdf-native", options, units, warnings)
}

func consumePDFContentStreams(
	ctx context.Context,
	value pdflib.Value,
	consumed *int64,
	limit int64,
) error {
	switch value.Kind() {
	case pdflib.Null:
		return nil
	case pdflib.Array:
		for index := 0; index < value.Len(); index++ {
			if err := consumePDFContentStreams(ctx, value.Index(index), consumed, limit); err != nil {
				return err
			}
		}
		return nil
	case pdflib.Stream:
		remaining := limit - *consumed
		if remaining < 0 {
			return fmt.Errorf("%w: decoded PDF content exceeds %d bytes", ErrDocumentLimitExceeded, limit)
		}
		stream := value.Reader()
		read, copyErr := io.CopyBuffer(
			io.Discard,
			io.LimitReader(&sourceContextReader{ctx: ctx, reader: stream}, remaining+1),
			make([]byte, sourceCopyBufferBytes),
		)
		closeErr := stream.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("%w: invalid PDF content stream", ErrInvalidDocument)
		}
		if read > remaining {
			return fmt.Errorf("%w: decoded PDF content exceeds %d bytes", ErrDocumentLimitExceeded, limit)
		}
		*consumed += read
		return nil
	default:
		return fmt.Errorf("%w: invalid PDF page content", ErrInvalidDocument)
	}
}

func retainNonEmptyPDFUnits(
	units []document.MarkdownUnit,
	warnings []document.ParseWarning,
) ([]document.MarkdownUnit, []document.ParseWarning) {
	nonEmptyUnits := units[:0]
	for _, unit := range units {
		if strings.TrimSpace(unit.Markdown) != "" {
			nonEmptyUnits = append(nonEmptyUnits, unit)
			continue
		}
		location := unit.Location
		warnings = append(warnings, document.ParseWarning{
			Code: "pdf_native_page_empty", Message: "PDF page has no extractable text",
			Location: &location, Degraded: true,
		})
	}
	return nonEmptyUnits, warnings
}
