package parse

import (
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func TestMarkdownImagePolicy(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		"# Images",
		"",
		"![network](https://example.invalid/a.png)",
		"![relative](../secret.png)",
		"![inline](data:image/png;base64,AAAA)",
		"![reference][ref]",
		"",
		"[ref]: file:///etc/passwd",
		"",
		`<img src="https://example.invalid/raw.png" alt="raw">`,
		"",
		`<picture><source srcset="https://example.invalid/a.webp"><img src="x"></picture>`,
		"",
		`<svg><image href="file:///etc/passwd"></image></svg>`,
		"",
		`<div style="background:url(https://example.invalid/bg.png)">unsafe html</div>`,
		"",
		"[safe link](https://example.com/docs?q=rag)",
		"[unsafe link](javascript:alert(1))",
		"",
		"```markdown",
		"![literal](https://example.invalid/in-code.png)",
		`<img src="file:///etc/passwd">`,
		"```",
	}, "\n")

	units, warnings, err := NormalizeMarkdown([]document.MarkdownUnit{{
		ID:       "unit_document",
		Location: document.SourceLocation{Kind: "document"},
		Markdown: input,
	}}, nil, false)
	if err != nil {
		t.Fatalf("NormalizeMarkdown: %v", err)
	}
	if len(units) != 1 {
		t.Fatalf("units=%d, want 1", len(units))
	}
	got := units[0].Markdown
	for _, want := range []string{
		`\[已忽略文档中的图片：network\]`,
		`\[已忽略文档中的图片：relative\]`,
		`\[已忽略文档中的图片：inline\]`,
		`\[已忽略文档中的图片：reference\]`,
		"[safe link](https://example.com/docs?q=rag)",
		"unsafe link",
		"![literal](https://example.invalid/in-code.png)",
		`<img src="file:///etc/passwd">`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("normalized Markdown missing %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{
		"](../secret.png)",
		"](data:image/",
		"](file:///etc/passwd)",
		"](javascript:",
		`<picture>`,
		`<svg>`,
		`style="background:url(`,
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("normalized Markdown retained unsafe construct %q:\n%s", forbidden, got)
		}
	}
	if len(warnings) == 0 {
		t.Fatal("ignored images/raw HTML must produce warnings")
	}
	for _, warning := range warnings {
		if !warning.Degraded {
			t.Errorf("warning must mark degraded: %+v", warning)
		}
	}
}

func TestNormalizeMarkdownInternalAssetTrustBoundary(t *testing.T) {
	t.Parallel()
	unit := document.MarkdownUnit{
		ID:       "unit_page_0001",
		Location: document.SourceLocation{Kind: "page", Index: 1, Label: "第 1 页"},
		Markdown: "![architecture](rag-asset://occ_page_0001_0001)",
	}
	occurrence := document.AssetOccurrence{
		ID:           "occ_page_0001_0001",
		AssetLocalID: "asset_0001",
		UnitID:       unit.ID,
		Order:        1,
		Location:     unit.Location,
	}

	trusted, warnings, err := NormalizeMarkdown(
		[]document.MarkdownUnit{unit},
		map[string]document.AssetOccurrence{occurrence.ID: occurrence},
		true,
	)
	if err != nil {
		t.Fatalf("trusted internal marker: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("trusted marker warnings=%+v", warnings)
	}
	if !strings.Contains(trusted[0].Markdown, "rag-asset://"+occurrence.ID) {
		t.Fatalf("trusted marker removed: %q", trusted[0].Markdown)
	}

	untrusted, warnings, err := NormalizeMarkdown([]document.MarkdownUnit{unit}, nil, false)
	if err != nil {
		t.Fatalf("user-authored marker must be ignored, not trusted: %v", err)
	}
	if !strings.Contains(untrusted[0].Markdown, `\[已忽略文档中的图片：architecture\]`) {
		t.Fatalf("untrusted marker not replaced: %q", untrusted[0].Markdown)
	}
	if len(warnings) == 0 {
		t.Fatal("untrusted internal marker must produce warning")
	}

	if _, _, err := NormalizeMarkdown([]document.MarkdownUnit{unit}, nil, true); err == nil {
		t.Fatal("unknown parser-authored occurrence must fail closed")
	}
	visual := unit
	visual.Markdown = "![unbound](rag-visual://v1)"
	if _, _, err := NormalizeMarkdown([]document.MarkdownUnit{visual}, nil, true); err == nil {
		t.Fatal("unresolved rag-visual marker must fail before artifact publication")
	}
}

func TestNormalizeMarkdownPreservesTableCellPipes(t *testing.T) {
	t.Parallel()
	unit := document.MarkdownUnit{
		ID:       "unit_document_0000",
		Location: document.SourceLocation{Kind: document.LocationDocument},
		Markdown: "| image | code |\n| --- | --- |\n| ![a\\|b](rag-asset://occ_1) | `a\\|b` |",
	}
	occurrence := document.AssetOccurrence{
		ID: "occ_1", AssetLocalID: "asset_1", UnitID: unit.ID,
		Order: 1, Location: unit.Location,
	}

	once, warnings, err := NormalizeMarkdown(
		[]document.MarkdownUnit{unit},
		map[string]document.AssetOccurrence{occurrence.ID: occurrence},
		true,
	)
	if err != nil {
		t.Fatalf("normalize table cell pipes: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("trusted table asset warnings=%+v", warnings)
	}
	for _, want := range []string{
		`![a\|b](rag-asset://occ_1)`,
		"`a\\|b`",
	} {
		if !strings.Contains(once[0].Markdown, want) {
			t.Fatalf("normalized table lost %q: %q", want, once[0].Markdown)
		}
	}
	twice, _, err := NormalizeMarkdown(
		once,
		map[string]document.AssetOccurrence{occurrence.ID: occurrence},
		true,
	)
	if err != nil {
		t.Fatalf("renormalize table cell pipes: %v", err)
	}
	if twice[0].Markdown != once[0].Markdown {
		t.Fatalf("table normalization is not idempotent:\nonce: %q\ntwice: %q", once[0].Markdown, twice[0].Markdown)
	}
}

func TestNormalizeMarkdownIsSourceIndependentAndIdempotent(t *testing.T) {
	t.Parallel()
	malicious := document.MarkdownUnit{
		ID:       "unit_1",
		Location: document.SourceLocation{Kind: "document"},
		Markdown: "<script>fetch('https://example.invalid')</script>\n\n" +
			"[x](file:///etc/passwd) ![y](https://example.invalid/y.png)\n\n" +
			"```text\n![code](data:image/png;base64,AAAA)\n```",
	}

	var canonical string
	for _, sourceName := range []string{"md", "office", "pdf-native", "vlm"} {
		units, _, err := NormalizeMarkdown([]document.MarkdownUnit{malicious}, nil, false)
		if err != nil {
			t.Fatalf("%s normalize: %v", sourceName, err)
		}
		if canonical == "" {
			canonical = units[0].Markdown
		} else if units[0].Markdown != canonical {
			t.Fatalf("source-dependent result for %s:\n%s\nwant:\n%s", sourceName, units[0].Markdown, canonical)
		}

		twice, _, err := NormalizeMarkdown(units, nil, false)
		if err != nil {
			t.Fatalf("%s second normalize: %v", sourceName, err)
		}
		if twice[0].Markdown != units[0].Markdown {
			t.Fatalf("normalization is not idempotent for %s:\nonce:\n%s\ntwice:\n%s", sourceName, units[0].Markdown, twice[0].Markdown)
		}
	}
}

func TestNormalizeMarkdownImagePlaceholderIsIdempotent(t *testing.T) {
	t.Parallel()
	for _, markdown := range []string{
		`![a\\]b](https://example.invalid/x.png)`,
		`![<img src=x> *emphasis*](../local.png)`,
	} {
		units := []document.MarkdownUnit{{
			ID:       "unit_1",
			Location: document.SourceLocation{Kind: document.LocationDocument},
			Markdown: markdown,
		}}
		once, _, err := NormalizeMarkdown(units, nil, false)
		if err != nil {
			t.Fatalf("first normalize %q: %v", markdown, err)
		}
		twice, _, err := NormalizeMarkdown(once, nil, false)
		if err != nil {
			t.Fatalf("second normalize %q: %v", markdown, err)
		}
		if once[0].Markdown != twice[0].Markdown {
			t.Fatalf("image placeholder is not idempotent for %q:\nonce: %q\ntwice: %q", markdown, once[0].Markdown, twice[0].Markdown)
		}
		if strings.Contains(once[0].Markdown, "<img") {
			t.Fatalf("image alt reintroduced raw HTML: %q", once[0].Markdown)
		}
	}

	code := []document.MarkdownUnit{{
		ID:       "unit_code",
		Location: document.SourceLocation{Kind: document.LocationDocument},
		Markdown: "```text\n\\[已忽略文档中的图片\\]\n```",
	}}
	normalized, _, err := NormalizeMarkdown(code, nil, false)
	if err != nil {
		t.Fatalf("normalize code placeholder: %v", err)
	}
	if !strings.Contains(normalized[0].Markdown, "```text\n\\[已忽略文档中的图片\\]\n```") {
		t.Fatalf("normalizer changed fenced code content: %q", normalized[0].Markdown)
	}
}

func TestNormalizeMarkdownConvergesOnAmbiguousDelimiters(t *testing.T) {
	t.Parallel()
	for _, markdown := range []string{"~~a~", "***a**", "_a__", `\~~~a~~`} {
		units := []document.MarkdownUnit{{
			ID:       "unit_1",
			Location: document.SourceLocation{Kind: document.LocationDocument},
			Markdown: markdown,
		}}
		once, _, err := NormalizeMarkdown(units, nil, false)
		if err != nil {
			t.Fatalf("normalize %q: %v", markdown, err)
		}
		twice, _, err := NormalizeMarkdown(once, nil, false)
		if err != nil {
			t.Fatalf("renormalize %q: %v", markdown, err)
		}
		if once[0].Markdown != twice[0].Markdown {
			t.Fatalf("ambiguous delimiters are not idempotent for %q: %q != %q", markdown, once[0].Markdown, twice[0].Markdown)
		}
	}
}

func TestNormalizeMarkdownWarnsWhenCanonicalLinkExceedsLimit(t *testing.T) {
	t.Parallel()
	unit := document.MarkdownUnit{
		ID:       "unit_document_0000",
		Location: document.SourceLocation{Kind: document.LocationDocument},
		Markdown: "[long](https://example.com/" + strings.Repeat("中", 500) + ")",
	}
	normalized, warnings, err := NormalizeMarkdown([]document.MarkdownUnit{unit}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if normalized[0].Markdown != "long\n" {
		t.Fatalf("oversize canonical link=%q", normalized[0].Markdown)
	}
	if len(warnings) != 1 || warnings[0].Code != "markdown_link_unsafe" {
		t.Fatalf("oversize canonical link warnings=%+v", warnings)
	}
}
