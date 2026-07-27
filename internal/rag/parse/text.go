package parse

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func parseText(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
) (*document.ParsedDocument, error) {
	content, err := readSourceBytes(ctx, source)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(content) {
		return nil, fmt.Errorf("%w: TXT is not valid UTF-8", ErrInvalidDocument)
	}
	plain := strings.TrimPrefix(string(content), "\ufeff")
	if strings.TrimSpace(plain) == "" {
		return nil, ErrEmptyContent
	}
	units, warnings, err := normalizeLocalUnits([]document.MarkdownUnit{{
		ID: "unit_document_0000",
		Location: document.SourceLocation{
			Kind: document.LocationDocument,
		},
		Markdown: plainTextToMarkdown(plain),
	}}, nil)
	if err != nil {
		return nil, err
	}
	if len(units) == 0 || strings.TrimSpace(units[0].Markdown) == "" {
		return nil, ErrEmptyContent
	}
	return newLocalParsedDocument(source, "text", options, units, warnings)
}

func plainTextToMarkdown(value string) string {
	value = normalizeMarkdownSource(value)
	var output strings.Builder
	for _, char := range value {
		switch char {
		case '&':
			output.WriteString("&amp;")
			continue
		case '<':
			output.WriteString("&lt;")
			continue
		case '>':
			output.WriteString("&gt;")
			continue
		}
		switch char {
		case '\\', '`', '*', '_', '{', '}', '[', ']', '(', ')', '#', '+', '-', '.', '!', '|', '~':
			output.WriteByte('\\')
			output.WriteRune(char)
		default:
			output.WriteRune(char)
		}
	}
	return output.String()
}
