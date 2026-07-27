package parse

import (
	"archive/zip"
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestParseMarkdownAndText(t *testing.T) {
	t.Parallel()
	markdown, err := os.Open("testdata/sample.md")
	if err != nil {
		t.Fatal(err)
	}
	defer markdown.Close()
	result, err := Parse(markdown, "a.MARKDOWN")
	if err != nil || result.Format != "md" ||
		!strings.Contains(result.Pages[0].Text, "# RAG 示例") {
		t.Fatalf("markdown result=%+v err=%v", result, err)
	}

	result, err = Parse(strings.NewReader("纯文本"), "b.TXT")
	if err != nil || result.Format != "txt" || result.Pages[0].Num != 1 {
		t.Fatalf("text result=%+v err=%v", result, err)
	}
}

func TestSupportedExtAndUnknownExtension(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"a.md", "a.MARKDOWN", "a.Txt", "a.PDF", "a.docx", "a.PPTX", "a.xlsx",
	} {
		if !SupportedExt(name) {
			t.Errorf("SupportedExt(%q) = false", name)
		}
	}
	if SupportedExt("a.exe") {
		t.Fatal("unexpected support for .exe")
	}
	if _, err := Parse(strings.NewReader("x"), "a.exe"); err == nil {
		t.Fatal("unknown extension should fail")
	}
}

func makeDocx(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	document, err := writer.Create("word/document.xml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = document.Write([]byte(`<?xml version="1.0"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main">
<w:body>
<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>第一章</w:t></w:r></w:p>
<w:p><w:r><w:t>这是正文</w:t></w:r><w:r><w:t>第二段落。</w:t></w:r></w:p>
<w:p><w:r><w:t>带</w:t><w:tab/><w:t>制表符</w:t><w:br/><w:t>换行</w:t></w:r></w:p>
</w:body></w:document>`))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestParseDocx(t *testing.T) {
	t.Parallel()
	result, err := Parse(bytes.NewReader(makeDocx(t)), "c.docx")
	if err != nil {
		t.Fatalf("parse docx: %v", err)
	}
	text := result.Pages[0].Text
	if result.Format != "docx" || !strings.Contains(text, "# 第一章") {
		t.Errorf("Heading1 should become Markdown, got %q", text)
	}
	if !strings.Contains(text, "这是正文第二段落。") {
		t.Errorf("text runs should be joined, got %q", text)
	}
	if !strings.Contains(text, "带\t制表符\n换行") {
		t.Errorf("tabs and breaks should be retained, got %q", text)
	}
}

func TestParsePDFTextLayer(t *testing.T) {
	t.Parallel()
	file, err := os.Open("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	result, err := Parse(file, "sample.pdf")
	if err != nil {
		t.Fatalf("parse pdf: %v", err)
	}
	if result.Format != "pdf" || len(result.Pages) != 1 ||
		!strings.Contains(result.Pages[0].Text, "hello rag") {
		t.Fatalf("unexpected pdf result: %+v", result)
	}
}

func TestParseEmptyContent(t *testing.T) {
	t.Parallel()
	for _, content := range []string{"   \n  ", "\ufeff  \n"} {
		_, err := Parse(strings.NewReader(content), "a.txt")
		if !errors.Is(err, ErrEmptyContent) {
			t.Fatalf("empty content error = %v", err)
		}
	}
}

func TestParseRejectsMalformedDocx(t *testing.T) {
	t.Parallel()
	if _, err := Parse(strings.NewReader("not a zip"), "bad.docx"); err == nil {
		t.Fatal("malformed docx should fail")
	}
}

func TestHeadingStyleLevel(t *testing.T) {
	t.Parallel()
	for style, want := range map[string]int{
		"Heading1": 1, "heading 2": 2, "3": 3, "标题4": 4, "Title": 0,
	} {
		if got := headingStyleLevel(style); got != want {
			t.Errorf("headingStyleLevel(%q) = %d, want %d", style, got, want)
		}
	}
}
