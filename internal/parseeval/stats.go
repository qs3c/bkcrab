package parseeval

import (
	"bytes"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extensionast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var structureStatsMarkdown = goldmark.New(
	goldmark.WithExtensions(extension.GFM, extension.Footnote),
)

// ComputeStructureStats calculates stable, presentation-oriented Markdown
// statistics without treating Markdown-looking text inside code as structure.
func ComputeStructureStats(markdown string, parserWarnings int) StructureStats {
	if parserWarnings < 0 {
		parserWarnings = 0
	}
	source := []byte(markdown)
	root := structureStatsMarkdown.Parser().Parse(text.NewReader(source))
	stats := StructureStats{
		Version:        "parser-structure-stats-v1",
		Characters:     utf8.RuneCountInString(markdown),
		ParserWarnings: parserWarnings,
	}

	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.Heading:
			stats.Headings++
		case *extensionast.TableRow:
			stats.TableDataRows++
		case *ast.ListItem:
			stats.ListItems++
		case *ast.Link:
			stats.Links++
		case *ast.Image:
			stats.ImageOrAssetMarkers++
		case *extensionast.Footnote:
			stats.FootnoteDefinitions++
		case *ast.Text:
			stats.ImageOrAssetMarkers += bytes.Count(typed.Segment.Value(source), []byte("rag-asset://"))
		case *ast.CodeSpan, *ast.FencedCodeBlock, *ast.CodeBlock:
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return stats
}
