package split

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

func TestCodeSplitsAtFunctionsAndBlankLinesWithCompleteFence(t *testing.T) {
	t.Parallel()
	source := "```go\n" +
		"func alpha() {\n    println(\"alpha alpha alpha alpha\")\n}\n\n" +
		"func beta() {\n    println(\"beta beta beta beta\")\n}\n\n" +
		"func gamma() {\n    println(\"gamma gamma gamma gamma\")\n}\n```"
	chunks := Markdown(source, Config{ChunkSize: 30, EnhancementReserveTokens: 5})
	if len(chunks) < 2 {
		t.Fatalf("expected code splitting, got %+v", chunks)
	}
	for _, chunk := range chunks {
		if chunk.Kind != BlockCode || chunk.Tokens+chunk.ReservedTokens > 30 {
			t.Fatalf("invalid code chunk: %+v", chunk)
		}
		if !strings.HasPrefix(chunk.RawContent, "```go\n") {
			t.Fatalf("language fence was not repeated: %q", chunk.RawContent)
		}
		assertParsesAsOneCodeBlock(t, chunk.RawContent)
	}
}

func TestOversizedCodeLineRuneSafeAndMarkedContinuation(t *testing.T) {
	t.Parallel()
	source := "```text\n" + strings.Repeat("超长行", 60) + "\n```"
	chunks := Markdown(source, Config{ChunkSize: 24, EnhancementReserveTokens: 3})
	if len(chunks) < 2 {
		t.Fatalf("expected oversized line split, got %+v", chunks)
	}
	var sawContinuation bool
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk.RawContent) || chunk.Tokens+chunk.ReservedTokens > 24 {
			t.Fatalf("unsafe long-line split %d: %+v", i, chunk)
		}
		if i > 0 && strings.Contains(chunk.RawContent, "续") {
			sawContinuation = true
		}
		assertParsesAsOneCodeBlock(t, chunk.RawContent)
	}
	if !sawContinuation {
		t.Fatal("continuation fragments were not marked")
	}
}

func TestOverlapNeverCopiesPartialFence(t *testing.T) {
	t.Parallel()
	markdown := strings.Repeat("before. ", 25) +
		"\n\n```python\ndef f():\n    return 1\n```\n\n" +
		strings.Repeat("after. ", 25)
	chunks := Markdown(markdown, Config{ChunkSize: 35, ChunkOverlap: 15})
	var codeCount int
	for _, chunk := range chunks {
		if chunk.Kind == BlockCode {
			codeCount++
			assertParsesAsOneCodeBlock(t, chunk.RawContent)
		} else if strings.Contains(chunk.RawContent, "```") {
			t.Fatalf("ordinary overlap copied a fence suffix: %+v", chunk)
		}
	}
	if codeCount != 1 {
		t.Fatalf("expected one atomic code block, got %d", codeCount)
	}
}

func TestIndentedCodeBecomesClosedFence(t *testing.T) {
	t.Parallel()
	chunks := Markdown("    # literal heading\n    ![literal](url)\n", Config{ChunkSize: 50})
	if len(chunks) != 1 || chunks[0].Kind != BlockCode || len(chunks[0].AssetRefs) != 0 {
		t.Fatalf("indented code was interpreted as Markdown: %+v", chunks)
	}
	assertParsesAsOneCodeBlock(t, chunks[0].RawContent)
}

func TestCodeFenceExpandsAroundBackticks(t *testing.T) {
	t.Parallel()
	chunks := Markdown("````text\nliteral ``` inside\n````", Config{ChunkSize: 50})
	if len(chunks) != 1 || !strings.HasPrefix(chunks[0].RawContent, "````text") {
		t.Fatalf("unsafe code fence selection: %+v", chunks)
	}
	assertParsesAsOneCodeBlock(t, chunks[0].RawContent)
}

func TestCodeNestedInBlockquoteKeepsFenceAndHeadingScope(t *testing.T) {
	t.Parallel()
	chunks := Markdown("# Root\n\n> note\n>\n> ```md\n> # literal\n> ```\n\n## Real\nbody", Config{ChunkSize: 80})
	var nested *Chunk
	for i := range chunks {
		if strings.Contains(chunks[i].RawContent, "# literal") {
			nested = &chunks[i]
		}
	}
	if nested == nil || nested.SectionTitle != "Root" ||
		!strings.Contains(nested.RawContent, "> ```md") || !strings.Contains(nested.RawContent, "> ```") {
		t.Fatalf("nested code lost its fence or changed heading scope: %+v", chunks)
	}
}

func TestLongCodeNestedInContainersKeepsCompleteFence(t *testing.T) {
	t.Parallel()
	longCode := strings.Repeat("println(\"nested code must stay fenced\")\n", 24)
	tests := []struct {
		name     string
		markdown string
	}{
		{
			name:     "blockquote",
			markdown: "> ```go\n> " + strings.ReplaceAll(strings.TrimSuffix(longCode, "\n"), "\n", "\n> ") + "\n> ```",
		},
		{
			name:     "list item",
			markdown: "- ```go\n  " + strings.ReplaceAll(strings.TrimSuffix(longCode, "\n"), "\n", "\n  ") + "\n  ```",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			chunks := Markdown(test.markdown, Config{ChunkSize: 42, EnhancementReserveTokens: 6})
			if len(chunks) < 2 {
				t.Fatalf("expected nested code to split, got %+v", chunks)
			}
			for _, chunk := range chunks {
				if chunk.Kind != BlockCode || chunk.Tokens+chunk.ReservedTokens > 42 {
					t.Fatalf("invalid nested code chunk: %+v", chunk)
				}
				assertContainsFencedCode(t, chunk.RawContent)
			}
		})
	}
}

func assertContainsFencedCode(t *testing.T, markdown string) {
	t.Helper()
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM))
	root := parser.Parser().Parse(text.NewReader([]byte(markdown)))
	found := false
	_ = ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if entering {
			if _, ok := node.(*ast.FencedCodeBlock); ok {
				found = true
				return ast.WalkStop, nil
			}
		}
		return ast.WalkContinue, nil
	})
	if !found {
		t.Fatalf("chunk no longer contains a complete fenced code block: %q", markdown)
	}
}

func assertParsesAsOneCodeBlock(t *testing.T, markdown string) {
	t.Helper()
	parser := goldmark.New(goldmark.WithExtensions(extension.GFM))
	root := parser.Parser().Parse(text.NewReader([]byte(markdown)))
	if root.ChildCount() != 1 {
		t.Fatalf("code chunk has %d top-level blocks: %q", root.ChildCount(), markdown)
	}
	switch root.FirstChild().(type) {
	case *ast.FencedCodeBlock, *ast.CodeBlock:
	default:
		t.Fatalf("chunk is not a code block (%T): %q", root.FirstChild(), markdown)
	}
}
