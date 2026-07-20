package parse

import (
	"fmt"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const LocalParserVersion = "local-parser-v1"

func newLocalParsedDocument(
	source document.Source,
	parserName string,
	options ParseOptions,
	units []document.MarkdownUnit,
	warnings []document.ParseWarning,
) (*document.ParsedDocument, error) {
	version := strings.TrimSpace(options.ParserVersion)
	if version == "" {
		version = LocalParserVersion
	}
	doc := document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser: document.ParserInfo{
			Name:    parserName,
			Version: version,
		},
		Units:    units,
		Warnings: warnings,
	}, nil, nil)
	if err := doc.Validate(); err != nil {
		_ = doc.Close()
		return nil, fmt.Errorf("validate local parsed document: %w", err)
	}
	return doc, nil
}

func normalizeLocalUnits(
	units []document.MarkdownUnit,
	warnings []document.ParseWarning,
) ([]document.MarkdownUnit, []document.ParseWarning, error) {
	normalized, normalizeWarnings, err := NormalizeMarkdown(units, nil, false)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	return normalized, append(warnings, normalizeWarnings...), nil
}
