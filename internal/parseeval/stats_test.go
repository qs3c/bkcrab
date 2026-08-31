package parseeval

import (
	"testing"
	"unicode/utf8"
)

func TestComputeStructureStats(t *testing.T) {
	markdown := "# 标题🙂\n\n" +
		"| name | value |\n| --- | ---: |\n| a | 1 |\n| b | 2 |\n\n" +
		"- first\n- second\n1. ordered\n\n" +
		"[site](https://example.test)\n\n" +
		"![ordinary](image.png)\n\n" +
		"![asset](rag-asset://occ_1)\n\n" +
		"plain rag-asset://occ_2 marker\n\n" +
		"reference[^note]\n\n[^note]: footnote definition\n\n" +
		"```markdown\n# fake\n| h | h |\n|---|---|\n| fake | row |\n- fake\n[fake](https://invalid)\n![fake](rag-asset://fake)\n[^fake]: fake\n```\n\n" +
		"`![also fake](rag-asset://inline)`\n"

	got := ComputeStructureStats(markdown, 3)
	if got.Version != "parser-structure-stats-v1" {
		t.Fatalf("version = %q", got.Version)
	}
	if got.Characters != utf8.RuneCountInString(markdown) {
		t.Fatalf("characters = %d, want %d", got.Characters, utf8.RuneCountInString(markdown))
	}
	if got.Headings != 1 || got.TableDataRows != 2 || got.ListItems != 3 {
		t.Fatalf("block stats = headings %d, rows %d, items %d", got.Headings, got.TableDataRows, got.ListItems)
	}
	if got.Links != 1 {
		t.Fatalf("links = %d, want 1", got.Links)
	}
	if got.ImageOrAssetMarkers != 3 {
		t.Fatalf("image/asset markers = %d, want 3", got.ImageOrAssetMarkers)
	}
	if got.FootnoteDefinitions != 1 {
		t.Fatalf("footnote definitions = %d, want 1", got.FootnoteDefinitions)
	}
	if got.ParserWarnings != 3 {
		t.Fatalf("parser warnings = %d, want 3", got.ParserWarnings)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestComputeStructureStatsClampsNegativeWarnings(t *testing.T) {
	if got := ComputeStructureStats("正文", -1); got.ParserWarnings != 0 {
		t.Fatalf("parser warnings = %d, want 0", got.ParserWarnings)
	}
}
