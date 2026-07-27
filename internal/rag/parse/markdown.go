package parse

import (
	"context"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func parseMarkdown(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
) (*document.ParsedDocument, error) {
	content, err := readSourceBytes(ctx, source)
	if err != nil {
		return nil, err
	}
	units, warnings, err := normalizeLocalUnits([]document.MarkdownUnit{{
		ID: "unit_document_0000",
		Location: document.SourceLocation{
			Kind: document.LocationDocument,
		},
		Markdown: string(content),
	}}, nil)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 || strings.TrimSpace(units[0].Markdown) == "" {
		return nil, ErrEmptyContent
	}
	return newLocalParsedDocument(source, "markdown", options, units, warnings)
}
