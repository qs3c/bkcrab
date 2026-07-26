package split

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func TestEstimateTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"你好世界", 4},
		{"hello world!", 3},
		{"中文 mixed text", 5},
	}
	for _, tc := range cases {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestMarkdownBreadcrumbAndSearchContentContract(t *testing.T) {
	t.Parallel()
	markdown := "# Installation\nintro\n### Deep\n" + strings.Repeat("body sentence. ", 80)
	chunks := Markdown(markdown, Config{ChunkSize: 50, ChunkOverlap: 10})
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	var sawDeep bool
	for _, chunk := range chunks {
		if chunk.SectionTitle == "Installation > Deep" {
			sawDeep = true
		}
		if !strings.HasPrefix(chunk.SearchContent, "章节："+chunk.SectionTitle+"\n\n") {
			t.Fatalf("search content lost breadcrumb: %+v", chunk)
		}
		if strings.Contains(chunk.RawContent, "Installation") || chunk.Content != chunk.RawContent {
			t.Fatalf("raw/compat content contract regressed: %+v", chunk)
		}
		if chunk.Tokens != EstimateTokens(chunk.SearchContent) || chunk.Tokens > 50 {
			t.Fatalf("invalid provisional token count: %+v", chunk)
		}
	}
	if !sawDeep {
		t.Fatal("skipped heading level was not represented in breadcrumb")
	}
}

func TestMarkdownFencedCodeDoesNotCreateHeadingImageOrTableStructure(t *testing.T) {
	t.Parallel()
	markdown := "# Root\nintro\n```markdown\n# fake\n![fake](rag-asset://missing)\n| a | b |\n|---|---|\n```\n## Real\nbody"
	chunks := Markdown(markdown, Config{ChunkSize: 200, ChunkOverlap: 20})
	var code, body *Chunk
	for i := range chunks {
		switch chunks[i].Kind {
		case BlockCode:
			code = &chunks[i]
		case BlockText:
			if chunks[i].RawContent == "body" {
				body = &chunks[i]
			}
		}
	}
	if code == nil || code.SectionTitle != "Root" ||
		!strings.Contains(code.RawContent, "# fake") ||
		!strings.Contains(code.RawContent, "rag-asset://missing") || len(code.AssetRefs) != 0 {
		t.Fatalf("fenced literals were interpreted as structure: %+v", code)
	}
	if body == nil || body.SectionTitle != "Root > Real" {
		t.Fatalf("real heading after fence was not recognized: %+v", body)
	}
}

func TestParagraphListAndBlockquoteGreedyPacking(t *testing.T) {
	t.Parallel()
	markdown := "# Topic\n\nfirst paragraph.\n\n- list one\n- list two\n\n> quoted text\n> continues"
	chunks := Markdown(markdown, Config{ChunkSize: 80, ChunkOverlap: 8})
	if len(chunks) != 1 {
		t.Fatalf("short ordinary blocks should greedily pack, got %+v", chunks)
	}
	for _, want := range []string{"first paragraph", "- list one", "> quoted text"} {
		if !strings.Contains(chunks[0].RawContent, want) {
			t.Fatalf("packed chunk missing %q: %q", want, chunks[0].RawContent)
		}
	}
}

func TestOrdinaryCandidateSplitsBeforeGreedyBox(t *testing.T) {
	t.Parallel()
	first := strings.Repeat("a", 176) + "."
	second := strings.Repeat("b", 176) + "."
	chunks := Markdown(first+"\n\n"+second, Config{ChunkSize: 50, ChunkOverlap: 4})
	if len(chunks) < 2 {
		t.Fatalf("expected a new chunk for the second individually-large candidate: %+v", chunks)
	}
	for _, chunk := range chunks {
		if chunk.Tokens > 50 {
			t.Fatalf("chunk approached the historical 2x limit: tokens=%d raw=%q", chunk.Tokens, chunk.RawContent)
		}
	}
}

func TestSlidingWindowOverlapIsBounded(t *testing.T) {
	t.Parallel()
	var text strings.Builder
	for range 40 {
		text.WriteString("这是一个用于测试的句子。")
	}
	chunks := SlidingWindow(text.String(), Config{ChunkSize: 100, ChunkOverlap: 20}, "", 2)
	if len(chunks) < 4 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.Tokens > 100 || chunk.Index != i || chunk.PageNum != 2 || chunk.Location.Index != 2 {
			t.Fatalf("invalid sliding chunk %d: %+v", i, chunk)
		}
	}
	if !strings.Contains(chunks[1].RawContent, "测试的句子") {
		t.Fatalf("second chunk has no ordinary text overlap: %q", chunks[1].RawContent)
	}
}

func TestTableAndCodeReserveApplicationBudget(t *testing.T) {
	t.Parallel()
	markdown := "# Heading\n\n| key | value |\n| --- | --- |\n| a | b |\n\n```go\nfunc f() {}\n```\n\nplain"
	chunks := Markdown(markdown, Config{
		ChunkSize: 50, ChunkOverlap: 5, EnhancementReserveTokens: 20,
	})
	var sawTable, sawCode, sawText bool
	for _, chunk := range chunks {
		switch chunk.Kind {
		case BlockTable:
			sawTable = true
		case BlockCode:
			sawCode = true
		case BlockText:
			sawText = true
		}
		if chunk.Kind == BlockTable || chunk.Kind == BlockCode {
			if chunk.ReservedTokens != 10 || chunk.Tokens+chunk.ReservedTokens > 50 {
				t.Fatalf("table/code did not reserve min(config, size/5): %+v", chunk)
			}
		} else if chunk.ReservedTokens != 0 {
			t.Fatalf("ordinary text unexpectedly reserved enhancement tokens: %+v", chunk)
		}
	}
	if !sawTable || !sawCode || !sawText {
		t.Fatalf("missing semantic chunk kinds: %+v", chunks)
	}
}

func TestTinyChunkConfigurationsNeverPanic(t *testing.T) {
	t.Parallel()
	for _, size := range []int{-1, 1, 2, 3} {
		chunks := Markdown("# a very long heading\nabcdef中文", Config{
			ChunkSize: size, ChunkOverlap: 999, EnhancementReserveTokens: 999,
		})
		if len(chunks) == 0 {
			t.Fatalf("size %d unexpectedly dropped content", size)
		}
		for _, chunk := range chunks {
			if !utf8.ValidString(chunk.RawContent) || !utf8.ValidString(chunk.SearchContent) {
				t.Fatalf("size %d produced invalid UTF-8: %+v", size, chunk)
			}
			if size > 0 && chunk.Tokens > size {
				t.Fatalf("size %d exceeded requested budget: %+v", size, chunk)
			}
		}
	}
}

func TestSplitCarriesHeadingAcrossUnitsWithoutCrossLocationPacking(t *testing.T) {
	t.Parallel()
	artifact := document.ParsedArtifact{Units: []document.MarkdownUnit{
		{ID: "u1", Location: document.SourceLocation{Kind: document.LocationPage, Index: 1}, Markdown: "# Root\npage one"},
		{ID: "u2", Location: document.SourceLocation{Kind: document.LocationPage, Index: 2}, Markdown: "page two"},
	}}
	chunks := Split(artifact, Config{ChunkSize: 100})
	if len(chunks) != 2 || chunks[0].PageNum != 1 || chunks[1].PageNum != 2 ||
		chunks[1].SectionTitle != "Root" {
		t.Fatalf("unit order/location/breadcrumb contract failed: %+v", chunks)
	}
}

func FuzzMarkdownSplit(f *testing.F) {
	f.Add("# Root\nparagraph\n\n- item", 64, 8)
	f.Add("```md\n# not heading\n```", 16, 2)
	f.Add("中文\x00mixed", 1, 0)
	f.Fuzz(func(t *testing.T, markdown string, size, overlap int) {
		// Structural Markdown has irreducible syntax overhead (a legal GFM
		// delimiter row or two code fences). Tiny 1..15 behavior is covered by
		// the deterministic no-panic test; fuzz budgets start at that overhead.
		if size < 16 || size > 1024 {
			size = 64
		}
		if overlap < 0 || overlap >= size {
			overlap = size / 8
		}
		chunks := Markdown(markdown, Config{ChunkSize: size, ChunkOverlap: overlap})
		for i, chunk := range chunks {
			if chunk.Index != i {
				t.Fatalf("index=%d want=%d", chunk.Index, i)
			}
			if !utf8.ValidString(chunk.RawContent) || !utf8.ValidString(chunk.SearchContent) {
				t.Fatal("invalid UTF-8")
			}
			if chunk.Tokens > size {
				t.Fatalf("tokens=%d size=%d", chunk.Tokens, size)
			}
		}
	})
}
