package split

import (
	"strings"
	"testing"
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

func TestSlidingWindowRespectsSizeAndOverlap(t *testing.T) {
	t.Parallel()
	var text strings.Builder
	for range 40 {
		text.WriteString("这是一个用于测试的句子。")
	}
	chunks := SlidingWindow(text.String(), Config{ChunkSize: 100, ChunkOverlap: 20}, "", 0)
	if len(chunks) < 4 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.Tokens > 100 {
			t.Errorf("chunk %d has %d tokens, want <= 100", i, chunk.Tokens)
		}
		if chunk.Index != i {
			t.Errorf("chunk index = %d, want %d", chunk.Index, i)
		}
	}
	if !strings.Contains(chunks[1].Content, "测试的句子") {
		t.Errorf("second chunk does not contain overlap: %q", chunks[1].Content)
	}
}

func TestSlidingWindowSplitsLongUnpunctuatedText(t *testing.T) {
	t.Parallel()
	chunks := SlidingWindow(strings.Repeat("长文本", 200), Config{
		ChunkSize: 50, ChunkOverlap: 10,
	}, "section", 2)
	if len(chunks) < 2 {
		t.Fatalf("expected long text to be split, got %d chunk", len(chunks))
	}
	for i, chunk := range chunks {
		if chunk.Tokens > 50 {
			t.Fatalf("chunk %d has %d tokens, want <= 50", i, chunk.Tokens)
		}
		if chunk.SectionTitle != "section" || chunk.PageNum != 2 {
			t.Fatalf("metadata was not preserved: %+v", chunk)
		}
	}
}

func TestMarkdownStructureSplit(t *testing.T) {
	t.Parallel()
	markdown := `# 安装指南

前置要求正文。

## 下载

下载步骤正文，很短。

## 配置

` + strings.Repeat("配置项说明。", 200) + "\n"
	chunks := Markdown(markdown, Config{ChunkSize: 200, ChunkOverlap: 30})
	if len(chunks) < 3 {
		t.Fatalf("not enough structure-aware chunks: %d", len(chunks))
	}
	var sawDownload, sawConfig bool
	for i, chunk := range chunks {
		if chunk.SectionTitle == "安装指南 > 下载" {
			sawDownload = true
		}
		if strings.HasPrefix(chunk.SectionTitle, "安装指南 > 配置") {
			sawConfig = true
			if chunk.Tokens > 200 {
				t.Errorf("long section chunk has %d tokens", chunk.Tokens)
			}
		}
		if chunk.Index != i {
			t.Fatalf("chunk index = %d, want %d", chunk.Index, i)
		}
	}
	if !sawDownload || !sawConfig {
		t.Fatalf("missing section title: download=%v config=%v", sawDownload, sawConfig)
	}
}

func TestMarkdownSkippedHeadingLevelHasCleanBreadcrumb(t *testing.T) {
	t.Parallel()
	chunks := Markdown("# Root\nintro\n### Deep\nbody", Config{})
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[1].SectionTitle != "Root > Deep" {
		t.Fatalf("breadcrumb = %q", chunks[1].SectionTitle)
	}
}

func TestMarkdownIgnoresHeadingsInsideFencedCode(t *testing.T) {
	t.Parallel()
	markdown := "# Root\nintro\n```shell\n# shell comment\n## not a section\n````\nafter code\n~~~text\n# tilde content\n~~~\n## Real\nbody"
	chunks := Markdown(markdown, Config{ChunkSize: 200, ChunkOverlap: 20})
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want root and real sections: %+v", len(chunks), chunks)
	}
	if chunks[0].SectionTitle != "Root" ||
		!strings.Contains(chunks[0].Content, "# shell comment") ||
		!strings.Contains(chunks[0].Content, "## not a section") ||
		!strings.Contains(chunks[0].Content, "# tilde content") {
		t.Fatalf("fenced code changed document structure: %+v", chunks[0])
	}
	if chunks[1].SectionTitle != "Root > Real" || chunks[1].Content != "body" {
		t.Fatalf("heading after fences was not recognized: %+v", chunks[1])
	}
}

func TestMarkdownSearchContentIncludesTitleWithinChunkBudget(t *testing.T) {
	t.Parallel()
	markdown := "# Installation\n" + strings.Repeat("body sentence. ", 80)
	chunks := Markdown(markdown, Config{ChunkSize: 50, ChunkOverlap: 10})
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for _, chunk := range chunks {
		if !strings.HasPrefix(chunk.SearchContent, "章节：Installation\n\n") {
			t.Fatalf("search content has no section title: %q", chunk.SearchContent)
		}
		if strings.Contains(chunk.Content, "Installation") {
			t.Fatalf("display content should remain the original body: %q", chunk.Content)
		}
		if chunk.Tokens != EstimateTokens(chunk.SearchContent) || chunk.Tokens > 50 {
			t.Fatalf("search content tokens = %d, content=%q", chunk.Tokens, chunk.SearchContent)
		}
	}
}

func TestMarkdownTinyChunkStillRespectsBudget(t *testing.T) {
	t.Parallel()
	chunks := Markdown("# Long heading\nabcdef", Config{ChunkSize: 1, ChunkOverlap: 0})
	if len(chunks) == 0 {
		t.Fatal("expected body chunks")
	}
	for _, chunk := range chunks {
		if chunk.Tokens > 1 || EstimateTokens(chunk.SearchContent) > 1 {
			t.Fatalf("tiny chunk exceeded budget: %+v", chunk)
		}
		if chunk.SectionTitle != "Long heading" {
			t.Fatalf("section metadata was lost: %+v", chunk)
		}
	}
}

func TestPagesPreservesPageNumbersAndContinuousIndexes(t *testing.T) {
	t.Parallel()
	chunks := Pages([]string{"first page", "", "third page"}, Config{})
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2", len(chunks))
	}
	if chunks[0].Index != 0 || chunks[0].PageNum != 1 ||
		chunks[1].Index != 1 || chunks[1].PageNum != 3 {
		t.Fatalf("unexpected page metadata: %+v", chunks)
	}
}
