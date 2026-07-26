package split

import (
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

func TestTableSplitsByRowsAndRepeatsHeader(t *testing.T) {
	t.Parallel()
	markdown := "# Metrics\n\n| name | value |\n| :--- | ---: |\n" +
		"| alpha | 111111111111 |\n| beta | 222222222222 |\n| gamma | 333333333333 |\n| delta | 444444444444 |"
	chunks := Markdown(markdown, Config{
		ChunkSize: 28, EnhancementReserveTokens: 5, ChunkOverlap: 20,
	})
	if len(chunks) < 2 {
		t.Fatalf("expected row splitting, got %+v", chunks)
	}
	for _, chunk := range chunks {
		if chunk.Kind != BlockTable {
			t.Fatalf("table overlap leaked into another kind: %+v", chunk)
		}
		if !strings.HasPrefix(chunk.RawContent, "| name | value |\n| :--- | ---: |") {
			t.Fatalf("split table did not repeat header: %q", chunk.RawContent)
		}
		if chunk.Tokens+chunk.ReservedTokens > 28 {
			t.Fatalf("table exceeded raw+reserve budget: %+v", chunk)
		}
		assertParsesAsOneGFMTable(t, chunk.RawContent)
	}
}

func TestTableOversizedCellStaysLegalAndWithinBudget(t *testing.T) {
	t.Parallel()
	markdown := "| key | description |\n| --- | --- |\n| one | " + strings.Repeat("超长单元格内容", 20) + " |"
	chunks := Markdown(markdown, Config{ChunkSize: 45, EnhancementReserveTokens: 5})
	if len(chunks) < 2 {
		t.Fatalf("expected long cell fragments, got %+v", chunks)
	}
	for _, chunk := range chunks {
		if chunk.Kind != BlockTable || chunk.Tokens+chunk.ReservedTokens > 45 {
			t.Fatalf("invalid long-cell chunk: %+v", chunk)
		}
		if !strings.Contains(chunk.RawContent, "| key | description |") {
			t.Fatalf("long-cell fragment lost header: %q", chunk.RawContent)
		}
		assertParsesAsOneGFMTable(t, chunk.RawContent)
	}
}

func TestOverlapNeverCopiesPartialTable(t *testing.T) {
	t.Parallel()
	markdown := strings.Repeat("before sentence. ", 15) +
		"\n\n| a | b |\n| --- | --- |\n| c | d |\n\n" +
		strings.Repeat("after sentence. ", 15)
	chunks := Markdown(markdown, Config{ChunkSize: 30, ChunkOverlap: 12})
	var tableCount int
	for _, chunk := range chunks {
		if chunk.Kind == BlockTable {
			tableCount++
			assertParsesAsOneGFMTable(t, chunk.RawContent)
			continue
		}
		if strings.Contains(chunk.RawContent, "| --- |") {
			t.Fatalf("ordinary overlap copied a partial table: %+v", chunk)
		}
	}
	if tableCount != 1 {
		t.Fatalf("expected one atomic table, got %d in %+v", tableCount, chunks)
	}
}

func TestCodeSyntaxInsideTableCellIsNotCodeBlock(t *testing.T) {
	t.Parallel()
	chunks := Markdown("| syntax | value |\n| --- | --- |\n| heading | `# fake` |", Config{ChunkSize: 80})
	if len(chunks) != 1 || chunks[0].Kind != BlockTable || strings.Count(chunks[0].RawContent, "|") < 6 {
		t.Fatalf("table cell syntax changed block structure: %+v", chunks)
	}
	assertParsesAsOneGFMTable(t, chunks[0].RawContent)
}

func TestLongTableNestedInBlockquoteRemainsStructured(t *testing.T) {
	t.Parallel()
	var markdown strings.Builder
	markdown.WriteString("> | name | value |\n> | --- | --- |\n")
	for i := range 18 {
		markdown.WriteString("> | row")
		markdown.WriteString(strings.Repeat("x", i%4+1))
		markdown.WriteString(" | ")
		markdown.WriteString(strings.Repeat("value", 4))
		markdown.WriteString(" |\n")
	}
	chunks := Markdown(markdown.String(), Config{ChunkSize: 44, EnhancementReserveTokens: 5})
	if len(chunks) < 2 {
		t.Fatalf("expected nested table splitting, got %+v", chunks)
	}
	for _, chunk := range chunks {
		if chunk.Kind != BlockTable || chunk.Tokens+chunk.ReservedTokens > 44 {
			t.Fatalf("invalid nested table chunk: %+v", chunk)
		}
		assertContainsGFMTable(t, chunk.RawContent)
	}
}

func TestLongTableNestedInListRemainsStructured(t *testing.T) {
	t.Parallel()
	var markdown strings.Builder
	markdown.WriteString("- metrics\n\n  | name | value |\n  | --- | --- |\n")
	for i := range 18 {
		markdown.WriteString("  | row")
		markdown.WriteString(strings.Repeat("x", i%4+1))
		markdown.WriteString(" | ")
		markdown.WriteString(strings.Repeat("value", 4))
		markdown.WriteString(" |\n")
	}
	chunks := Markdown(markdown.String(), Config{ChunkSize: 44, EnhancementReserveTokens: 5})
	tableChunks := 0
	for _, chunk := range chunks {
		if chunk.Kind != BlockTable {
			continue
		}
		tableChunks++
		if chunk.Tokens+chunk.ReservedTokens > 44 {
			t.Fatalf("invalid list-nested table chunk: %+v", chunk)
		}
		assertContainsGFMTable(t, chunk.RawContent)
	}
	if tableChunks < 2 {
		t.Fatalf("expected a split table nested in the list, got %+v", chunks)
	}
}

func TestPathologicalTableUsesExplicitSafeDegradation(t *testing.T) {
	t.Parallel()
	markdown := "| " + strings.Repeat("oversized-header", 12) + " | value |\n| --- | --- |\n| row | data |"
	chunks := Markdown(markdown, Config{ChunkSize: 36, EnhancementReserveTokens: 4})
	if len(chunks) < 2 {
		t.Fatalf("expected pathological table degradation, got %+v", chunks)
	}
	for _, chunk := range chunks {
		if strings.Contains(chunk.RawContent, "|x|") {
			t.Fatalf("pathological table used a fabricated header: %q", chunk.RawContent)
		}
		if chunk.Kind != BlockTable || chunk.Tokens+chunk.ReservedTokens > 36 {
			t.Fatalf("invalid degraded table chunk: %+v", chunk)
		}
		assertFencedTableFragment(t, chunk.RawContent)
	}
}

func assertContainsGFMTable(t *testing.T, markdown string) {
	t.Helper()
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM))
	root := parser.Parser().Parse(text.NewReader([]byte(markdown)))
	found := false
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if _, ok := node.(*extast.Table); ok {
				found = true
				return ast.WalkStop, nil
			}
		}
		return ast.WalkContinue, nil
	})
	if !found {
		t.Fatalf("chunk no longer contains a GFM table: %q", markdown)
	}
}

func assertFencedTableFragment(t *testing.T, markdown string) {
	t.Helper()
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM))
	root := parser.Parser().Parse(text.NewReader([]byte(markdown)))
	found := false
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		code, ok := node.(*ast.FencedCodeBlock)
		if ok && string(code.Language([]byte(markdown))) == "table-gfm-fragment" {
			found = true
			return ast.WalkStop, nil
		}
		return ast.WalkContinue, nil
	})
	if !found {
		t.Fatalf("pathological table was not explicitly degraded to a fenced GFM fragment: %q", markdown)
	}
}

func assertParsesAsOneGFMTable(t *testing.T, markdown string) {
	t.Helper()
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM))
	root := parser.Parser().Parse(text.NewReader([]byte(markdown)))
	first := root.FirstChild()
	if first == nil || first.NextSibling() != nil {
		t.Fatalf("chunk is not one GFM block: %q", markdown)
	}
	if _, ok := first.(*extast.Table); !ok {
		t.Fatalf("chunk is not a legal GFM table (%T): %q", root.FirstChild(), markdown)
	}
}
