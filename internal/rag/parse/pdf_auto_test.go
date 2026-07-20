package parse

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

func primitiveWithText(text string) sidecar.PagePrimitive {
	return sidecar.PagePrimitive{
		Page: 1, Width: 612, Height: 792, TextChars: utf8RuneCount(text), BlockCount: 1,
		TextCoverage:   .1,
		TextBlocks:     []sidecar.PrimitiveTextBlock{{Text: text, BBox: []int{50, 50, 950, 200}}},
		EmbeddedImages: []sidecar.PrimitiveEmbeddedImage{},
	}
}

func utf8RuneCount(value string) int { return len([]rune(value)) }

func TestPDFAutoRoutingTableAndThresholdBoundaries(t *testing.T) {
	type routingFixture struct {
		Name        string  `json:"name"`
		NativeChars int     `json:"nativeChars"`
		TextChars   int     `json:"textChars"`
		Images      [][]int `json:"images"`
		Signals     struct {
			Table                 bool `json:"table"`
			Code                  bool `json:"code"`
			Scanned               bool `json:"scanned"`
			Multicolumn           bool `json:"multicolumn"`
			ReadingOrderUncertain bool `json:"readingOrderUncertain"`
		} `json:"signals"`
		WantVision bool `json:"wantVision"`
	}
	encoded, err := os.ReadFile("testdata/pdf-auto/routing_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var tests []routingFixture
	if err := json.Unmarshal(encoded, &tests); err != nil {
		t.Fatal(err)
	}
	if len(tests) == 0 {
		t.Fatal("routing fixture is empty")
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			native := strings.Repeat("a", test.NativeChars)
			primitive := primitiveWithText(strings.Repeat("a", test.TextChars))
			primitive.Signals = sidecar.PrimitiveSignals{
				Table: test.Signals.Table, Code: test.Signals.Code, Scanned: test.Signals.Scanned,
				Multicolumn: test.Signals.Multicolumn, ReadingOrderUncertain: test.Signals.ReadingOrderUncertain,
			}
			for _, bbox := range test.Images {
				primitive.EmbeddedImages = append(primitive.EmbeddedImages, sidecar.PrimitiveEmbeddedImage{BBox: bbox})
			}
			if got := shouldRoutePDFPage(native, primitive); got != test.WantVision {
				t.Fatalf("shouldRoutePDFPage()=%v, want %v", got, test.WantVision)
			}
		})
	}
}

func TestPDFAutoVisualMarkerKeysDoNotAliasPrefixes(t *testing.T) {
	markdown := "![ten](rag-visual://v10)\n\n![one](rag-visual://v1)"
	visuals := []vision.Visual{{Key: "v1"}, {Key: "v10"}}
	orders := visualMarkerOrders(markdown, visuals)
	if orders["v10"] != 1 || orders["v1"] != 2 {
		t.Fatalf("marker orders=%v", orders)
	}
	bound, err := bindPDFVisualMarkers(markdown, map[string]string{
		"v1": "occ_pdf_0001_v1", "v10": "occ_pdf_0001_v10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(bound, "rag-visual://") ||
		!strings.Contains(bound, "rag-asset://occ_pdf_0001_v10") ||
		!strings.Contains(bound, "rag-asset://occ_pdf_0001_v1)") {
		t.Fatalf("bound markdown=%q", bound)
	}
}

type analyzePageFixture struct {
	status    sidecar.PageStatus
	errorCode string
	native    string
	primitive sidecar.PagePrimitive
}

type embeddedFixture struct {
	data []byte
	bbox document.NormalizedBBox
	alt  string
}

type renderPageFixture struct {
	status    sidecar.PageStatus
	errorCode string
	render    []byte
	embedded  []embeddedFixture
}

type pdfFixtureExtractor struct {
	t               *testing.T
	source          document.Source
	analyzePages    []analyzePageFixture
	renderPages     map[int]renderPageFixture
	analyzeErr      error
	renderErr       error
	renderRequests  [][]int
	lastRenderPages []sidecar.PageDescriptor
	lastRenderErr   error
}

func (f *pdfFixtureExtractor) ConvertOffice(context.Context, document.Source) (*sidecar.BundleHandle, error) {
	return nil, sidecar.ErrCapabilityUnavailable
}

func (f *pdfFixtureExtractor) AnalyzePDF(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error) {
	if f.analyzeErr != nil {
		return nil, f.analyzeErr
	}
	return buildAnalyzeHandle(f.t, ctx, source, f.analyzePages)
}

func (f *pdfFixtureExtractor) RenderPDF(ctx context.Context, source document.Source, pages []int) (*sidecar.BundleHandle, error) {
	f.renderRequests = append(f.renderRequests, append([]int(nil), pages...))
	if f.renderErr != nil {
		return nil, f.renderErr
	}
	fixtures := make([]renderPageFixture, len(pages))
	for index, page := range pages {
		fixtures[index] = f.renderPages[page]
	}
	handle, err := buildRenderHandle(f.t, ctx, source, pages, fixtures)
	f.lastRenderErr = err
	if handle != nil {
		f.lastRenderPages = append([]sidecar.PageDescriptor(nil), handle.Manifest.Pages...)
	}
	return handle, err
}

type fakePDFVision struct {
	results map[int]vision.PageTranscription
	errors  map[int]error
	calls   []int
}

func (f *fakePDFVision) TranscribePage(_ context.Context, input vision.PageInput, _ *vision.TaskDocumentAIBudget) (vision.PageTranscription, error) {
	page := input.Image.Location.Index
	f.calls = append(f.calls, page)
	if err := f.errors[page]; err != nil {
		return vision.PageTranscription{}, err
	}
	return f.results[page], nil
}

func TestPDFAutoUsesEmbeddedOriginalBeforeCrop(t *testing.T) {
	source := fakePDFSource([]byte("%PDF fake embedded"))
	native := strings.Repeat("nativeanchor", 9)
	primitive := primitiveWithText(native)
	primitive.Page = 1
	primitive.Signals.Table = true
	render := solidPNG(t, 200, 200, color.NRGBA{R: 20, G: 40, B: 60, A: 255})
	embedded := solidPNG(t, 40, 40, color.NRGBA{R: 200, G: 10, B: 10, A: 255})
	extractor := &pdfFixtureExtractor{t: t, source: source,
		analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, native: native, primitive: primitive}},
		renderPages: map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: render,
			embedded: []embeddedFixture{{data: embedded, bbox: document.NormalizedBBox{100, 100, 600, 600}}}}},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {
		Markdown: native + "\n\n![diagram](rag-visual://v1)",
		Visuals: []vision.Visual{{Key: "v1", Kind: "diagram", BBox: document.NormalizedBBox{110, 110, 590, 590},
			Caption: "architecture", OCRText: "A to B", Confidence: .9}},
	}}, errors: map[int]error{}}
	parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
	defer parsed.Close()
	if len(parsed.Assets) != 1 || parsed.Assets[0].SourceKind != document.SourceKindEmbeddedOriginal {
		t.Fatalf("assets=%+v", parsed.Assets)
	}
	if parsed.Assets[0].ContentSHA256 != shaHex(embedded) || strings.Contains(parsed.Assets[0].BundleEntry, "pages/page") {
		t.Fatalf("embedded source was not preserved: %+v", parsed.Assets[0])
	}
	if strings.Contains(parsed.Units[0].Markdown, "rag-visual://") || !strings.Contains(parsed.Units[0].Markdown, "rag-asset://") {
		t.Fatalf("visual marker was not bound: %q", parsed.Units[0].Markdown)
	}
	reader, err := parsed.OpenBundleEntry(context.Background(), parsed.Assets[0].BundleEntry)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(reader)
	_ = reader.Close()
	if !bytes.Equal(got, embedded) {
		t.Fatal("embedded asset bytes changed")
	}
}

func TestPDFAutoFallsBackToSafePageCropAndNeverStoresFullRender(t *testing.T) {
	source := fakePDFSource([]byte("%PDF fake crop"))
	native := strings.Repeat("anchortext", 10)
	primitive := primitiveWithText(native)
	primitive.Page = 1
	primitive.Signals.Code = true
	render := solidPNG(t, 200, 100, color.NRGBA{R: 12, G: 34, B: 56, A: 255})
	extractor := &pdfFixtureExtractor{t: t, source: source,
		analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, native: native, primitive: primitive}},
		renderPages: map[int]renderPageFixture{1: {
			status: sidecar.PageStatusOK, render: render,
			embedded: []embeddedFixture{{
				data: solidPNG(t, 10, 10, color.Black), bbox: document.NormalizedBBox{0, 0, 100, 100},
			}},
		}},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {
		Markdown: native + "\n\n![code screenshot](rag-visual://v1)",
		Visuals: []vision.Visual{{Key: "v1", Kind: "screenshot", BBox: document.NormalizedBBox{250, 200, 750, 800},
			Caption: "code screenshot", Confidence: .95}},
	}}, errors: map[int]error{}}
	progress := make([]ParseProgress, 0, 2)
	parsed := parseAutoFixture(t, source, extractor, transcriber, 100, func(_ context.Context, value ParseProgress) error {
		progress = append(progress, value)
		return nil
	})
	defer parsed.Close()
	if len(parsed.Assets) != 1 || parsed.Assets[0].SourceKind != document.SourceKindPageCrop ||
		!strings.HasPrefix(parsed.Assets[0].BundleEntry, "derived/") {
		t.Fatalf("crop asset=%+v warnings=%+v visionCalls=%v renderPages=%+v renderErr=%v", parsed.Assets, parsed.Warnings, transcriber.calls, extractor.lastRenderPages, extractor.lastRenderErr)
	}
	if parsed.Assets[0].Width != 100 || parsed.Assets[0].Height != 60 || parsed.Assets[0].ContentSHA256 == shaHex(render) {
		t.Fatalf("crop dimensions/hash=%+v", parsed.Assets[0])
	}
	for _, asset := range parsed.Assets {
		if asset.SourceKind == document.SourceKindScannedPage || strings.Contains(asset.BundleEntry, "pages/page") {
			t.Fatalf("full page render leaked into ordinary assets: %+v", asset)
		}
	}
	if len(progress) != 2 || progress[0].Stage != "vision" || progress[0].Current != 0 || progress[0].Total != 1 ||
		progress[1].Current != 1 || progress[1].Total != 1 {
		t.Fatalf("progress=%+v", progress)
	}
}

func TestPDFAutoVLMFailureRetainsEmbeddedOriginalOnNativeUnit(t *testing.T) {
	source := fakePDFSource([]byte("%PDF fake fallback"))
	native := strings.Repeat("fallbackanchor", 8)
	primitive := primitiveWithText(native)
	primitive.Page = 1
	primitive.Signals.Table = true
	render := solidPNG(t, 100, 100, color.White)
	embedded := solidPNG(t, 20, 20, color.Black)
	extractor := &pdfFixtureExtractor{t: t, source: source,
		analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, native: native, primitive: primitive}},
		renderPages: map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: render,
			embedded: []embeddedFixture{{data: embedded, bbox: document.NormalizedBBox{100, 100, 500, 500}}}}},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{}, errors: map[int]error{1: context.DeadlineExceeded}}
	parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
	defer parsed.Close()
	if len(parsed.Assets) != 1 || parsed.Assets[0].SourceKind != document.SourceKindEmbeddedOriginal {
		t.Fatalf("fallback assets=%+v", parsed.Assets)
	}
	if len(parsed.Occurrences) != 1 || parsed.Occurrences[0].AltText != "图片（视觉识别失败）" {
		t.Fatalf("fallback occurrences=%+v", parsed.Occurrences)
	}
	if !strings.Contains(parsed.Units[0].Markdown, "rag-asset://") || !hasWarning(parsed.Warnings, "pdf_vision_page_failed") {
		t.Fatalf("unit=%q warnings=%+v", parsed.Units[0].Markdown, parsed.Warnings)
	}
}

func TestPDFAutoRejectsLowAnchorCoverageAndAbnormalExpansionPerPage(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
	}{
		{name: "omission", markdown: "unrelated text"},
		{name: "expansion", markdown: strings.Repeat("nativeanchor", 8) + " " + strings.Repeat("hallucination ", 500)},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := fakePDFSource([]byte("%PDF fidelity " + test.name))
			native := strings.Repeat("nativeanchor", 8)
			primitive := primitiveWithText(native)
			primitive.Page = 1
			primitive.Signals.Table = true
			extractor := &pdfFixtureExtractor{t: t, source: source,
				analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, native: native, primitive: primitive}},
				renderPages:  map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: solidPNG(t, 80, 80, color.White)}},
			}
			transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {Markdown: test.markdown, Visuals: []vision.Visual{}}}, errors: map[int]error{}}
			parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
			defer parsed.Close()
			if !strings.Contains(parsed.Units[0].Markdown, native) || !hasWarning(parsed.Warnings, "pdf_vision_fidelity_failed") {
				t.Fatalf("unit=%q warnings=%+v", parsed.Units[0].Markdown, parsed.Warnings)
			}
		})
	}
}

func TestPDFAutoVisionLimitPreservesPageOrderAndUsesOnlyAllowlistedRenderPages(t *testing.T) {
	source := fakePDFSource([]byte("%PDF fake three pages"))
	analyze := make([]analyzePageFixture, 3)
	for index := range analyze {
		native := fmt.Sprintf("page%d%s", index+1, strings.Repeat("a", 100))
		primitive := primitiveWithText(native)
		primitive.Page = index + 1
		primitive.Signals.Table = true
		analyze[index] = analyzePageFixture{status: sidecar.PageStatusOK, native: native, primitive: primitive}
	}
	extractor := &pdfFixtureExtractor{t: t, source: source, analyzePages: analyze,
		renderPages: map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: solidPNG(t, 40, 40, color.White)}},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {Markdown: analyze[0].native, Visuals: []vision.Visual{}}}, errors: map[int]error{}}
	parser := NewLocalParser(extractor, 300)
	parser.MaxVisionPages = 1
	parsed, err := parser.Parse(context.Background(), source, ParseOptions{
		Mode: config.ParseModeAuto, PageTranscriber: transcriber, DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if len(extractor.renderRequests) != 1 || fmt.Sprint(extractor.renderRequests[0]) != "[1]" {
		t.Fatalf("render requests=%v", extractor.renderRequests)
	}
	if len(parsed.Units) != 3 || parsed.Units[0].Location.Index != 1 || parsed.Units[1].Location.Index != 2 || parsed.Units[2].Location.Index != 3 {
		t.Fatalf("page order=%+v", parsed.Units)
	}
	if warningCount(parsed.Warnings, "pdf_vision_page_limit") != 2 {
		t.Fatalf("warnings=%+v", parsed.Warnings)
	}
}

func TestPDFAutoPageFailuresDoNotDiscardSuccessfulPages(t *testing.T) {
	t.Run("analyze failure", func(t *testing.T) {
		source := fakePDFSource([]byte("%PDF invalid native fallback but valid sidecar"))
		native := strings.Repeat("secondpageanchor", 7)
		primitive := primitiveWithText(native)
		primitive.Page = 2
		primitive.Signals.Table = true
		extractor := &pdfFixtureExtractor{t: t, source: source,
			analyzePages: []analyzePageFixture{
				{status: sidecar.PageStatusFailed, errorCode: "page_analyze_failed"},
				{status: sidecar.PageStatusOK, native: native, primitive: primitive},
			},
			renderPages: map[int]renderPageFixture{2: {status: sidecar.PageStatusOK, render: solidPNG(t, 40, 40, color.White)}},
		}
		transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{2: {Markdown: native, Visuals: []vision.Visual{}}}, errors: map[int]error{}}
		parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
		defer parsed.Close()
		if len(parsed.Units) != 1 || parsed.Units[0].Location.Index != 2 || !hasWarning(parsed.Warnings, "pdf_analyze_page_failed") {
			t.Fatalf("units=%+v warnings=%+v", parsed.Units, parsed.Warnings)
		}
	})

	t.Run("render failure", func(t *testing.T) {
		source := fakePDFSource([]byte("%PDF mixed render"))
		analyze := make([]analyzePageFixture, 2)
		for index := range analyze {
			native := fmt.Sprintf("page%d%s", index+1, strings.Repeat("anchor", 18))
			primitive := primitiveWithText(native)
			primitive.Page = index + 1
			primitive.Signals.Table = true
			analyze[index] = analyzePageFixture{status: sidecar.PageStatusOK, native: native, primitive: primitive}
		}
		extractor := &pdfFixtureExtractor{t: t, source: source, analyzePages: analyze,
			renderPages: map[int]renderPageFixture{
				1: {status: sidecar.PageStatusOK, render: solidPNG(t, 40, 40, color.White)},
				2: {status: sidecar.PageStatusFailed, errorCode: "page_render_failed"},
			},
		}
		transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {Markdown: analyze[0].native, Visuals: []vision.Visual{}}}, errors: map[int]error{}}
		parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
		defer parsed.Close()
		if len(parsed.Units) != 2 || fmt.Sprint(transcriber.calls) != "[1]" || !hasWarning(parsed.Warnings, "pdf_render_page_failed") {
			t.Fatalf("units=%+v calls=%v warnings=%+v", parsed.Units, transcriber.calls, parsed.Warnings)
		}
	})
}

func TestPDFAutoScannedPageIsExplicitAndSmallVisualIsDecorative(t *testing.T) {
	t.Run("scanned page", func(t *testing.T) {
		source := fakePDFSource([]byte("%PDF scan"))
		primitive := sidecar.PagePrimitive{Page: 1, Width: 612, Height: 792,
			TextBlocks: []sidecar.PrimitiveTextBlock{}, EmbeddedImages: []sidecar.PrimitiveEmbeddedImage{},
			Signals: sidecar.PrimitiveSignals{Scanned: true}}
		render := solidPNG(t, 80, 120, color.White)
		extractor := &pdfFixtureExtractor{t: t, source: source,
			analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, primitive: primitive}},
			renderPages:  map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: render}},
		}
		transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {Markdown: "scanned OCR text", Visuals: []vision.Visual{}}}, errors: map[int]error{}}
		parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
		defer parsed.Close()
		if len(parsed.Assets) != 1 || parsed.Assets[0].SourceKind != document.SourceKindScannedPage ||
			len(parsed.Occurrences) != 1 || parsed.Occurrences[0].Decorative {
			t.Fatalf("scan assets=%+v occurrences=%+v", parsed.Assets, parsed.Occurrences)
		}
	})

	t.Run("small visual", func(t *testing.T) {
		source := fakePDFSource([]byte("%PDF small visual"))
		native := strings.Repeat("smallvisualanchor", 7)
		primitive := primitiveWithText(native)
		primitive.Page = 1
		primitive.Signals.Code = true
		extractor := &pdfFixtureExtractor{t: t, source: source,
			analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, native: native, primitive: primitive}},
			renderPages:  map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: solidPNG(t, 100, 100, color.White)}},
		}
		transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{1: {
			Markdown: native + "\n\n![dot](rag-visual://tiny)",
			Visuals:  []vision.Visual{{Key: "tiny", Kind: "other", BBox: document.NormalizedBBox{10, 10, 20, 20}, Caption: "dot", Confidence: .8}},
		}}, errors: map[int]error{}}
		parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
		defer parsed.Close()
		if len(parsed.Occurrences) != 1 || !parsed.Occurrences[0].Decorative {
			t.Fatalf("small visual occurrence=%+v", parsed.Occurrences)
		}
	})
}

func TestPDFAutoSidecarUnavailableUsesBoundedNativeFallback(t *testing.T) {
	data := largeTestPDF(2 << 20)
	source := testDocumentSource("pdf", "bounded.pdf", data, sourceCopyBufferBytes)
	originalOpen := source.Open
	source.Open = func(ctx context.Context) (io.ReadCloser, error) {
		reader, err := originalOpen(ctx)
		if err != nil {
			return nil, err
		}
		guard := reader.(*guardReadCloser)
		guard.minRead = sourceCopyBufferBytes / 2
		return guard, nil
	}
	extractor := &pdfFixtureExtractor{t: t, source: source, analyzeErr: sidecar.ErrCapabilityUnavailable}
	parser := NewLocalParser(extractor, 300)
	parsed, err := parser.Parse(context.Background(), source, ParseOptions{Mode: config.ParseModeAuto})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if !hasWarning(parsed.Warnings, "pdf_auto_sidecar_unavailable") || len(parsed.Assets) != 0 {
		t.Fatalf("fallback warnings/assets=%+v %+v", parsed.Warnings, parsed.Assets)
	}
}

func TestPDFAutoSkipsOneEmptyPageWithoutDiscardingDocument(t *testing.T) {
	source := fakePDFSource([]byte("%PDF one empty page"))
	empty := sidecar.PagePrimitive{
		Page: 1, Width: 612, Height: 792,
		TextBlocks: []sidecar.PrimitiveTextBlock{}, EmbeddedImages: []sidecar.PrimitiveEmbeddedImage{},
		Signals: sidecar.PrimitiveSignals{Scanned: true},
	}
	native := strings.Repeat("survivingnativeanchor", 6)
	second := primitiveWithText(native)
	second.Page = 2
	extractor := &pdfFixtureExtractor{t: t, source: source,
		analyzePages: []analyzePageFixture{
			{status: sidecar.PageStatusOK, native: "", primitive: empty},
			{status: sidecar.PageStatusOK, native: native, primitive: second},
		},
		renderPages: map[int]renderPageFixture{1: {
			status: sidecar.PageStatusOK, render: solidPNG(t, 40, 40, color.White),
		}},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{}, errors: map[int]error{1: errors.New("timeout")}}
	parsed := parseAutoFixture(t, source, extractor, transcriber, 100, nil)
	defer parsed.Close()
	if len(parsed.Units) != 1 || parsed.Units[0].Location.Index != 2 ||
		!hasWarning(parsed.Warnings, "pdf_vision_page_failed") || !hasWarning(parsed.Warnings, "pdf_auto_page_empty") {
		t.Fatalf("units=%+v warnings=%+v", parsed.Units, parsed.Warnings)
	}
}

func TestPDFAutoAllPagesWithoutNativeOrVisionContentFails(t *testing.T) {
	source := fakePDFSource([]byte("%PDF empty"))
	primitive := sidecar.PagePrimitive{Page: 1, Width: 612, Height: 792, TextBlocks: []sidecar.PrimitiveTextBlock{}, EmbeddedImages: []sidecar.PrimitiveEmbeddedImage{}, Signals: sidecar.PrimitiveSignals{Scanned: true}}
	extractor := &pdfFixtureExtractor{t: t, source: source,
		analyzePages: []analyzePageFixture{{status: sidecar.PageStatusOK, native: "", primitive: primitive}},
		renderPages:  map[int]renderPageFixture{1: {status: sidecar.PageStatusOK, render: solidPNG(t, 40, 40, color.White)}},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{}, errors: map[int]error{1: errors.New("invalid JSON")}}
	parser := NewLocalParser(extractor, 300)
	_, err := parser.Parse(context.Background(), source, ParseOptions{
		Mode: config.ParseModeAuto, PageTranscriber: transcriber, DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if !errors.Is(err, ErrEmptyContent) {
		t.Fatalf("empty auto PDF error=%v, want ErrEmptyContent", err)
	}
}

func parseAutoFixture(t *testing.T, source document.Source, extractor *pdfFixtureExtractor, transcriber vision.PageTranscriber, maxVisionPages int, progress ParseProgressFunc) *document.ParsedDocument {
	t.Helper()
	parser := NewLocalParser(extractor, 300)
	parser.MaxVisionPages = maxVisionPages
	parser.TempDir = t.TempDir()
	parsed, err := parser.Parse(context.Background(), source, ParseOptions{
		Mode: config.ParseModeAuto, PageTranscriber: transcriber,
		DocumentAIBudget: &vision.TaskDocumentAIBudget{}, Progress: progress,
	})
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func fakePDFSource(data []byte) document.Source {
	return testDocumentSource("pdf", "fixture.pdf", data, sourceCopyBufferBytes)
}

func solidPNG(t *testing.T, width, height int, fill color.Color) []byte {
	t.Helper()
	raster := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			raster.Set(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, raster); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func buildAnalyzeHandle(t *testing.T, ctx context.Context, source document.Source, fixtures []analyzePageFixture) (*sidecar.BundleHandle, error) {
	t.Helper()
	payloads := map[string]bundlePayload{}
	pages := make([]sidecar.PageDescriptor, len(fixtures))
	for index, fixture := range fixtures {
		page := index + 1
		unitID := pdfUnitID(source, page)
		if fixture.status == sidecar.PageStatusFailed {
			pages[index] = sidecar.PageDescriptor{Page: page, Status: fixture.status, ErrorCode: fixture.errorCode, UnitID: unitID}
			continue
		}
		fixture.primitive.Page = page
		primitive, err := json.Marshal(fixture.primitive)
		if err != nil {
			t.Fatal(err)
		}
		nativePath := fmt.Sprintf("units/page-%04d.md", page)
		primitivePath := fmt.Sprintf("pages/page-%04d.json", page)
		payloads[nativePath] = bundlePayload{data: []byte(fixture.native), mime: sidecar.MIMETypeMarkdown}
		payloads[primitivePath] = bundlePayload{data: primitive, mime: sidecar.MIMETypeJSON}
		pages[index] = sidecar.PageDescriptor{Page: page, Status: sidecar.PageStatusOK, UnitID: unitID,
			NativeMarkdownEntry: nativePath, PrimitiveEntry: primitivePath}
	}
	manifest := basePDFManifest(source, sidecar.BundleKindPDFAnalyze)
	manifest.Pages = pages
	return decodeFixtureBundle(t, ctx, manifest, payloads, sidecar.DecodeOptions{ExpectedKind: sidecar.BundleKindPDFAnalyze})
}

func buildRenderHandle(t *testing.T, ctx context.Context, source document.Source, requested []int, fixtures []renderPageFixture) (*sidecar.BundleHandle, error) {
	t.Helper()
	payloads := map[string]bundlePayload{}
	pages := make([]sidecar.PageDescriptor, len(fixtures))
	assetDescriptors := make([]sidecar.AssetDescriptor, 0)
	occurrenceDescriptors := make([]sidecar.OccurrenceDescriptor, 0)
	assetByHash := make(map[string]string)
	for index, fixture := range fixtures {
		page := requested[index]
		unitID := pdfUnitID(source, page)
		if fixture.status == sidecar.PageStatusFailed {
			pages[index] = sidecar.PageDescriptor{Page: page, Status: fixture.status, ErrorCode: fixture.errorCode, UnitID: unitID}
			continue
		}
		renderPath := fmt.Sprintf("pages/page-%04d.png", page)
		payloads[renderPath] = bundlePayload{data: fixture.render, mime: "image/png"}
		pages[index] = sidecar.PageDescriptor{Page: page, Status: sidecar.PageStatusOK, UnitID: unitID, RenderEntry: renderPath}
		for order, embedded := range fixture.embedded {
			hash := shaHex(embedded.data)
			localID, ok := assetByHash[hash]
			if !ok {
				localID = fmt.Sprintf("asset_%04d", len(assetDescriptors)+1)
				assetByHash[hash] = localID
				path := "assets/" + localID + ".png"
				payloads[path] = bundlePayload{data: embedded.data, mime: "image/png"}
				config, _, err := image.DecodeConfig(bytes.NewReader(embedded.data))
				if err != nil {
					t.Fatal(err)
				}
				assetDescriptors = append(assetDescriptors, sidecar.AssetDescriptor{LocalID: localID, Entry: path,
					Kind: document.AssetKindImage, SourceKind: document.SourceKindEmbeddedOriginal, Width: config.Width, Height: config.Height})
			}
			occurrenceDescriptors = append(occurrenceDescriptors, sidecar.OccurrenceDescriptor{
				ID: fmt.Sprintf("occ_page_%04d_%04d", page, order+1), AssetLocalID: localID, UnitID: unitID,
				Order: order + 1, Location: sidecar.Location{Kind: document.LocationPage, Index: page, Label: fmt.Sprintf("第 %d 页", page)},
				BBox: append([]int(nil), embedded.bbox[:]...), AltText: embedded.alt, Confidence: 1,
			})
		}
	}
	manifest := basePDFManifest(source, sidecar.BundleKindPDFRender)
	manifest.Pages, manifest.Assets, manifest.Occurrences = pages, assetDescriptors, occurrenceDescriptors
	return decodeFixtureBundle(t, ctx, manifest, payloads, sidecar.DecodeOptions{ExpectedKind: sidecar.BundleKindPDFRender, RequestedPages: requested})
}

type bundlePayload struct {
	data []byte
	mime string
}

func basePDFManifest(source document.Source, kind sidecar.BundleKind) sidecar.Manifest {
	return sidecar.Manifest{
		ProtocolVersion: sidecar.ProtocolVersion, BundleKind: kind,
		Source:  sidecar.SourceDescriptor{Format: "pdf", ByteSize: source.Size, SHA256: source.SHA256},
		Parser:  sidecar.ParserDescriptor{Name: "fake-pdfium", Version: "1.0.0", WrapperVersion: "pdf-wrapper-v1"},
		Entries: []sidecar.EntryDescriptor{}, Units: []sidecar.UnitDescriptor{}, Assets: []sidecar.AssetDescriptor{},
		Occurrences: []sidecar.OccurrenceDescriptor{}, Pages: []sidecar.PageDescriptor{}, Warnings: []sidecar.WarningDescriptor{},
	}
}

func decodeFixtureBundle(t *testing.T, ctx context.Context, manifest sidecar.Manifest, payloads map[string]bundlePayload, options sidecar.DecodeOptions) (*sidecar.BundleHandle, error) {
	t.Helper()
	paths := make([]string, 0, len(payloads))
	for path := range payloads {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	manifest.Entries = make([]sidecar.EntryDescriptor, 0, len(paths))
	for _, path := range paths {
		payload := payloads[path]
		manifest.Entries = append(manifest.Entries, sidecar.EntryDescriptor{Path: path, SHA256: shaHex(payload.data), ByteSize: int64(len(payload.data)), MIMEType: payload.mime})
	}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	writeTarFixture(t, writer, sidecar.ManifestEntryName, manifestJSON)
	for _, path := range paths {
		writeTarFixture(t, writer, path, payloads[path].data)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	options.ExpectedSource = manifest.Source
	options.Limits = sidecar.DecodeLimits{MaxManifestBytes: 1 << 20, MaxEntries: 100, MaxPages: 300, MaxAssets: 50,
		MaxImagePixels: 40_000_000, MaxEntryBytes: 20 << 20, MaxAssetBytes: 20 << 20,
		MaxRenderBytes: 20 << 20, MaxTotalBytes: 100 << 20, MaxArchiveBytes: 100 << 20}
	options.TempDir = t.TempDir()
	return sidecar.DecodeBundle(ctx, bytes.NewReader(archive.Bytes()), options)
}

func writeTarFixture(t *testing.T, writer *tar.Writer, name string, data []byte) {
	t.Helper()
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
}

func pdfUnitID(source document.Source, page int) string {
	return fmt.Sprintf("unit_page_%s_%04d", source.SHA256[:12], page)
}

func shaHex(value []byte) string {
	hash := sha256.Sum256(value)
	return hex.EncodeToString(hash[:])
}

func hasWarning(warnings []document.ParseWarning, code string) bool {
	return warningCount(warnings, code) > 0
}

func warningCount(warnings []document.ParseWarning, code string) int {
	count := 0
	for _, warning := range warnings {
		if warning.Code == code {
			count++
		}
	}
	return count
}
