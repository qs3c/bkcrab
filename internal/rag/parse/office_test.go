package parse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/color"
	"io"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

type officeFixture struct {
	markdown    string
	images      [][]byte
	assetIDs    []string
	occurrences []sidecar.OccurrenceDescriptor
	warnings    []sidecar.WarningDescriptor
}

type officeFixtureExtractor struct {
	t       *testing.T
	fixture officeFixture
	calls   int
}

func (f *officeFixtureExtractor) ConvertOffice(ctx context.Context, source document.Source) (*sidecar.BundleHandle, error) {
	f.calls++
	return buildOfficeHandle(f.t, ctx, source, f.fixture)
}

func (*officeFixtureExtractor) AnalyzePDF(context.Context, document.Source) (*sidecar.BundleHandle, error) {
	return nil, sidecar.ErrCapabilityUnavailable
}

func (*officeFixtureExtractor) RenderPDF(context.Context, document.Source, []int) (*sidecar.BundleHandle, error) {
	return nil, sidecar.ErrCapabilityUnavailable
}

type recordingOfficeVision struct {
	description vision.ImageDescription
	err         error
	inputs      []vision.NormalizedImageInput
	budgets     []*vision.TaskDocumentAIBudget
}

func (v *recordingOfficeVision) DescribeImage(
	_ context.Context,
	input vision.NormalizedImageInput,
	budget *vision.TaskDocumentAIBudget,
) (vision.ImageDescription, error) {
	v.inputs = append(v.inputs, input)
	v.budgets = append(v.budgets, budget)
	if v.err != nil {
		return vision.ImageDescription{}, v.err
	}
	return v.description, nil
}

func fakeOfficeSource(format string) document.Source {
	data := []byte("fake-office-source-" + format)
	return document.Source{
		DocID: "doc_office", FileName: "sample." + format, Format: format,
		Size: int64(len(data)), SHA256: shaHex(data),
		Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

func officeLocation(format string) sidecar.Location {
	switch format {
	case "pptx":
		return sidecar.Location{Kind: document.LocationSlide, Index: 1, Label: "幻灯片 1"}
	case "xlsx":
		return sidecar.Location{Kind: document.LocationSheet, Index: 1, Label: "Summary"}
	default:
		return sidecar.Location{Kind: document.LocationDocument, Index: 0, Label: "文档"}
	}
}

func officeUnitID(location sidecar.Location) string {
	switch location.Kind {
	case document.LocationSlide:
		return fmt.Sprintf("unit_slide_%04d", location.Index)
	case document.LocationSheet:
		return fmt.Sprintf("unit_sheet_%04d", location.Index)
	default:
		return "unit_document_0000"
	}
}

func buildOfficeHandle(
	t *testing.T,
	ctx context.Context,
	source document.Source,
	fixture officeFixture,
) (*sidecar.BundleHandle, error) {
	t.Helper()
	location := officeLocation(source.Format)
	unitID := officeUnitID(location)
	markdownPath := "units/0001.md"
	payloads := map[string]bundlePayload{
		markdownPath: {data: []byte(fixture.markdown), mime: sidecar.MIMETypeMarkdown},
	}
	assets := make([]sidecar.AssetDescriptor, len(fixture.images))
	for index, image := range fixture.images {
		localID := fmt.Sprintf("asset_%04d", index+1)
		if index < len(fixture.assetIDs) && fixture.assetIDs[index] != "" {
			localID = fixture.assetIDs[index]
		}
		entry := fmt.Sprintf("assets/%s.png", localID)
		payloads[entry] = bundlePayload{data: image, mime: "image/png"}
		assets[index] = sidecar.AssetDescriptor{
			LocalID: localID, Entry: entry, Kind: document.AssetKindImage,
			SourceKind: document.SourceKindEmbeddedOriginal, Width: 12, Height: 8,
		}
	}
	manifest := sidecar.Manifest{
		ProtocolVersion: sidecar.ProtocolVersion, BundleKind: sidecar.BundleKindOfficeConvert,
		Source: sidecar.SourceDescriptor{Format: source.Format, ByteSize: source.Size, SHA256: source.SHA256},
		Parser: sidecar.ParserDescriptor{Name: "markitdown", Version: "0.1.6", WrapperVersion: "office-wrapper-v1"},
		Units:  []sidecar.UnitDescriptor{{ID: unitID, Location: location, MarkdownEntry: markdownPath}},
		Assets: assets, Occurrences: append([]sidecar.OccurrenceDescriptor{}, fixture.occurrences...),
		Pages: []sidecar.PageDescriptor{}, Warnings: append([]sidecar.WarningDescriptor{}, fixture.warnings...),
		Entries: []sidecar.EntryDescriptor{},
	}
	return decodeFixtureBundle(t, ctx, manifest, payloads, sidecar.DecodeOptions{
		ExpectedKind: sidecar.BundleKindOfficeConvert,
	})
}

func repeatedOfficeFixture(t *testing.T, format string) officeFixture {
	t.Helper()
	location := officeLocation(format)
	unitID := officeUnitID(location)
	image := solidPNG(t, 12, 8, imageBlue)
	return officeFixture{
		markdown: "Before\n\n![Architecture](rag-asset://occ_first)\n\nBetween\n\n" +
			"![Architecture again](rag-asset://occ_second)\n\nAfter\n",
		images: [][]byte{image},
		occurrences: []sidecar.OccurrenceDescriptor{
			{ID: "occ_first", AssetLocalID: "asset_0001", UnitID: unitID, Order: 1,
				Location: location, AltText: "Architecture", Confidence: 1},
			{ID: "occ_second", AssetLocalID: "asset_0001", UnitID: unitID, Order: 2,
				Location: location, AltText: "Architecture again", Confidence: 1},
		},
	}
}

var imageBlue = colorRGBA(35, 99, 180)

func colorRGBA(red, green, blue uint8) color.RGBA {
	return color.RGBA{R: red, G: green, B: blue, A: 255}
}

func TestOfficeStandardMapsStableAssetsOccurrencesWithoutVision(t *testing.T) {
	for _, format := range []string{"docx", "pptx", "xlsx"} {
		t.Run(format, func(t *testing.T) {
			fixture := repeatedOfficeFixture(t, format)
			extractor := &officeFixtureExtractor{t: t, fixture: fixture}
			transcriber := &recordingOfficeVision{}
			parser := NewLocalParser(extractor, 300)
			parsed, err := parser.Parse(context.Background(), fakeOfficeSource(format), ParseOptions{
				Mode: config.ParseModeStandard, ImageTranscriber: transcriber,
			})
			if err != nil {
				t.Fatalf("Parse(%s): %v", format, err)
			}
			defer parsed.Close()
			if len(transcriber.inputs) != 0 {
				t.Fatalf("standard mode made %d vision calls", len(transcriber.inputs))
			}
			if len(parsed.Assets) != 1 || len(parsed.Occurrences) != 2 {
				t.Fatalf("assets/occurrences=%d/%d", len(parsed.Assets), len(parsed.Occurrences))
			}
			wantHash := shaHex(fixture.images[0])
			if parsed.Assets[0].ContentSHA256 != wantHash ||
				parsed.Assets[0].LocalID != "asset_office_"+wantHash[:24] {
				t.Fatalf("stable asset=%+v", parsed.Assets[0])
			}
			if parsed.Occurrences[0].AssetLocalID != parsed.Assets[0].LocalID ||
				parsed.Occurrences[1].AssetLocalID != parsed.Assets[0].LocalID {
				t.Fatalf("occurrence mapping=%+v", parsed.Occurrences)
			}
			if !strings.Contains(parsed.Units[0].Markdown, "Before") ||
				!strings.Contains(parsed.Units[0].Markdown, "rag-asset://occ_first") {
				t.Fatalf("unit markdown=%q", parsed.Units[0].Markdown)
			}
		})
	}
}

func TestOfficeAutoDescribesEachUniqueAssetWithSharedBudget(t *testing.T) {
	format := "docx"
	location := officeLocation(format)
	unitID := officeUnitID(location)
	first := solidPNG(t, 12, 8, imageBlue)
	second := solidPNG(t, 12, 8, colorRGBA(180, 40, 40))
	fixture := officeFixture{
		markdown: "![one](rag-asset://occ_one)\n\n![two](rag-asset://occ_two)\n",
		images:   [][]byte{first, second},
		occurrences: []sidecar.OccurrenceDescriptor{
			{ID: "occ_one", AssetLocalID: "asset_0001", UnitID: unitID, Order: 1,
				Location: location, AltText: "one alt", Confidence: 1},
			{ID: "occ_two", AssetLocalID: "asset_0002", UnitID: unitID, Order: 2,
				Location: location, AltText: "two alt", Confidence: 1},
		},
	}
	extractor := &officeFixtureExtractor{t: t, fixture: fixture}
	transcriber := &recordingOfficeVision{description: vision.ImageDescription{
		Kind: "diagram", Caption: "typed caption", OCRText: "A -> B", Confidence: .9,
	}}
	budget := &vision.TaskDocumentAIBudget{}
	parser := NewLocalParser(extractor, 300)
	parser.MaxVisionAssets = 1
	parsed, err := parser.Parse(context.Background(), fakeOfficeSource(format), ParseOptions{
		Mode: config.ParseModeAuto, ImageTranscriber: transcriber, DocumentAIBudget: budget,
		VisionScope: vision.CacheScope{UserID: "u1", KBID: "kb1", DocID: "doc_office"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if len(transcriber.inputs) != 1 || len(transcriber.budgets) != 1 || transcriber.budgets[0] != budget {
		t.Fatalf("vision calls/budget=%d/%p", len(transcriber.inputs), transcriber.budgets[0])
	}
	if transcriber.inputs[0].Format != format || transcriber.inputs[0].AltText != "one alt" ||
		transcriber.inputs[0].Scope.DocID != "doc_office" {
		t.Fatalf("vision input=%+v", transcriber.inputs[0])
	}
	if parsed.Occurrences[0].Caption != "typed caption" || parsed.Occurrences[0].OCRText != "A -> B" {
		t.Fatalf("described occurrence=%+v", parsed.Occurrences[0])
	}
	if parsed.Occurrences[1].Caption != "" || parsed.Occurrences[1].AltText != "two alt" {
		t.Fatalf("limited occurrence lost fallback=%+v", parsed.Occurrences[1])
	}
	if !hasOfficeWarning(parsed.Warnings, "office_vision_asset_limit") {
		t.Fatalf("warnings=%+v", parsed.Warnings)
	}
}

func TestOfficeAutoCanonicalizeSplitRetainsXLSXCellAnchors(t *testing.T) {
	format := "xlsx"
	location := officeLocation(format)
	unitID := officeUnitID(location)
	fixture := officeFixture{
		markdown: "## Summary\n\n![B3](rag-asset://occ_b3)\n\nB3 context.\n\n---\n\n" +
			"![A2](rag-asset://occ_a2)\n\nA2 context.\n",
		images: [][]byte{solidPNG(t, 12, 8, imageBlue)},
		occurrences: []sidecar.OccurrenceDescriptor{
			{ID: "occ_b3", AssetLocalID: "asset_0001", UnitID: unitID, Order: 0,
				Location: location, AltText: "单元格 B3：Metrics chart", Confidence: 1},
			{ID: "occ_a2", AssetLocalID: "asset_0001", UnitID: unitID, Order: 1,
				Location: location, AltText: "单元格 A2：Metrics chart", Confidence: 1},
		},
	}
	transcriber := &recordingOfficeVision{description: vision.ImageDescription{
		Kind: "chart", Caption: "Quarterly growth", OCRText: "Q1 42 Q2 57", Confidence: .9,
	}}
	parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
	parsed, err := parser.Parse(context.Background(), fakeOfficeSource(format), ParseOptions{
		Mode: config.ParseModeAuto, ImageTranscriber: transcriber,
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if len(transcriber.inputs) != 1 {
		t.Fatalf("same XLSX asset made %d vision calls", len(transcriber.inputs))
	}
	if parsed.Occurrences[0].Caption != "单元格 B3：Quarterly growth" ||
		parsed.Occurrences[1].Caption != "单元格 A2：Quarterly growth" {
		t.Fatalf("position-aware captions=%+v", parsed.Occurrences)
	}

	artifact, err := document.Canonicalize(parsed, "图片（未进行视觉识别）")
	if err != nil {
		t.Fatal(err)
	}
	artifact.Assets[0].DisplayStatus = document.DisplayReady
	chunks := split.Split(*artifact, split.Config{ChunkSize: 40})
	seen := map[string]string{}
	assetID := artifact.Assets[0].ID
	for _, chunk := range chunks {
		if len(chunk.AssetBindings) == 0 {
			continue
		}
		if len(chunk.AssetBindings) != 1 || len(chunk.AssetRefs) != 1 ||
			chunk.AssetRefs[0].ID != assetID {
			t.Fatalf("invalid XLSX asset binding: %+v", chunk)
		}
		binding := chunk.AssetBindings[0]
		seen[binding.OccurrenceID] = chunk.RawContent
		anchor := map[string]string{"occ_b3": "B3", "occ_a2": "A2"}[binding.OccurrenceID]
		if anchor == "" || !strings.Contains(chunk.RawContent, "单元格 "+anchor+"：Quarterly growth") ||
			!strings.Contains(chunk.RawContent, "Q1 42 Q2 57") ||
			!strings.Contains(chunk.SearchContent, "单元格 "+anchor+"：Quarterly growth") {
			t.Fatalf("anchor/search content lost for %q: %+v", anchor, chunk)
		}
	}
	if len(seen) != 2 || strings.Contains(seen["occ_b3"], "单元格 A2：") ||
		strings.Contains(seen["occ_a2"], "单元格 B3：") {
		t.Fatalf("XLSX occurrence anchors crossed: %+v", seen)
	}
}

func TestOfficeAutoSingleImageFailureIsDegraded(t *testing.T) {
	fixture := repeatedOfficeFixture(t, "docx")
	extractor := &officeFixtureExtractor{t: t, fixture: fixture}
	transcriber := &recordingOfficeVision{err: errors.New("provider failed")}
	parser := NewLocalParser(extractor, 300)
	parsed, err := parser.Parse(context.Background(), fakeOfficeSource("docx"), ParseOptions{
		Mode: config.ParseModeAuto, ImageTranscriber: transcriber,
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatalf("single image failure aborted document: %v", err)
	}
	defer parsed.Close()
	if len(parsed.Occurrences) != 2 || parsed.Occurrences[0].AltText == "" ||
		!hasOfficeWarning(parsed.Warnings, "office_vision_image_failed") {
		t.Fatalf("fallback/warnings=%+v / %+v", parsed.Occurrences, parsed.Warnings)
	}
}

func TestOfficeMissingAltDegradationIsResolvedAfterSuccessfulAutoVision(t *testing.T) {
	format := "xlsx"
	location := officeLocation(format)
	unitID := officeUnitID(location)
	fixture := officeFixture{
		markdown: "![单元格 B3：图片（未进行视觉识别）](rag-asset://occ_missing_alt)\n",
		images:   [][]byte{solidPNG(t, 12, 8, imageBlue)},
		occurrences: []sidecar.OccurrenceDescriptor{{
			ID: "occ_missing_alt", AssetLocalID: "asset_0001", UnitID: unitID,
			Order: 0, Location: location,
			AltText: "单元格 B3：图片（未进行视觉识别）", Confidence: 1,
		}},
		// Simulate a mode-agnostic warning from a converter. The Go parser must
		// derive the final warning only after standard/auto handling completes.
		warnings: []sidecar.WarningDescriptor{{
			Code: "office_image_alt_missing", Message: "missing alt",
			Location: &location, Degraded: true,
		}},
	}
	parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
	auto, err := parser.Parse(context.Background(), fakeOfficeSource(format), ParseOptions{
		Mode: config.ParseModeAuto,
		ImageTranscriber: &recordingOfficeVision{description: vision.ImageDescription{
			Kind: "diagram", Caption: "generated caption", Confidence: .9,
		}},
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer auto.Close()
	if auto.Occurrences[0].Caption != "单元格 B3：generated caption" ||
		warningCountByCode(auto.Warnings, "office_image_alt_missing") != 0 {
		t.Fatalf("auto occurrence/warnings=%+v/%+v", auto.Occurrences[0], auto.Warnings)
	}

	standard, err := parser.Parse(context.Background(), fakeOfficeSource(format), ParseOptions{
		Mode: config.ParseModeStandard,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer standard.Close()
	if warningCountByCode(standard.Warnings, "office_image_alt_missing") != 1 {
		t.Fatalf("standard warnings=%+v", standard.Warnings)
	}

	ocrOnly, err := parser.Parse(context.Background(), fakeOfficeSource(format), ParseOptions{
		Mode: config.ParseModeAuto,
		ImageTranscriber: &recordingOfficeVision{description: vision.ImageDescription{
			Kind: "other", OCRText: "recognized cell image", Confidence: .8,
		}},
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ocrOnly.Close()
	if warningCountByCode(ocrOnly.Warnings, "office_image_alt_missing") != 0 {
		t.Fatalf("OCR-only auto warnings=%+v", ocrOnly.Warnings)
	}
}

func TestOfficeRejectsOccurrenceWithoutMarkdownMarker(t *testing.T) {
	fixture := repeatedOfficeFixture(t, "docx")
	fixture.markdown = "image marker was lost\n"
	parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
	parsed, err := parser.Parse(context.Background(), fakeOfficeSource("docx"), ParseOptions{
		Mode: config.ParseModeStandard,
	})
	if parsed != nil {
		_ = parsed.Close()
	}
	if err == nil || !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("missing marker error=%v", err)
	}
}

func TestOfficeRejectsPlainTextAndCodeThatForgeOccurrenceMarkers(t *testing.T) {
	for _, test := range []struct {
		name     string
		markdown string
	}{
		{"plain-text", "plain rag-asset://occ_first)\nplain rag-asset://occ_second)\n"},
		{"fenced-code", "```markdown\n![one](rag-asset://occ_first)\n![two](rag-asset://occ_second)\n```\n"},
		{"inline-code", "`![one](rag-asset://occ_first)` and `![two](rag-asset://occ_second)`\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := repeatedOfficeFixture(t, "docx")
			fixture.markdown = test.markdown
			parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
			parsed, err := parser.Parse(context.Background(), fakeOfficeSource("docx"), ParseOptions{
				Mode: config.ParseModeStandard,
			})
			if parsed != nil {
				_ = parsed.Close()
			}
			if err == nil || !errors.Is(err, ErrInvalidDocument) {
				t.Fatalf("forged marker error=%v", err)
			}
		})
	}
}

func TestOfficeParserRechecksAssetAndTotalLimits(t *testing.T) {
	fixture := repeatedOfficeFixture(t, "docx")
	second := solidPNG(t, 12, 8, colorRGBA(180, 40, 40))
	location := officeLocation("docx")
	unitID := officeUnitID(location)
	fixture.images = append(fixture.images, second)
	fixture.markdown += "\n![second asset](rag-asset://occ_third)\n"
	fixture.occurrences = append(fixture.occurrences, sidecar.OccurrenceDescriptor{
		ID: "occ_third", AssetLocalID: "asset_0002", UnitID: unitID, Order: 3,
		Location: location, AltText: "second asset", Confidence: 1,
	})
	for _, test := range []struct {
		name  string
		apply func(*LocalParser)
	}{
		{"asset-count", func(parser *LocalParser) { parser.MaxAssets = 1 }},
		{"single-asset-bytes", func(parser *LocalParser) { parser.MaxAssetBytes = 1 }},
		{"total-entry-bytes", func(parser *LocalParser) { parser.MaxExtractedBytes = 8 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
			test.apply(parser)
			parsed, err := parser.Parse(context.Background(), fakeOfficeSource("docx"), ParseOptions{
				Mode: config.ParseModeStandard,
			})
			if parsed != nil {
				_ = parsed.Close()
			}
			if !errors.Is(err, ErrDocumentLimitExceeded) {
				t.Fatalf("limit error=%v", err)
			}
		})
	}
}

func TestOfficeVisionProgressAdvancesWhenNormalizationFallsBack(t *testing.T) {
	fixture := repeatedOfficeFixture(t, "docx")
	parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
	parser.MaxVisionInputBytes = 16
	var progress []int
	parsed, err := parser.Parse(context.Background(), fakeOfficeSource("docx"), ParseOptions{
		Mode: config.ParseModeAuto, ImageTranscriber: &recordingOfficeVision{},
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
		Progress: func(_ context.Context, value ParseProgress) error {
			if value.Stage == "vision" {
				progress = append(progress, value.Current)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if fmt.Sprint(progress) != "[0 1]" || !hasOfficeWarning(parsed.Warnings, "office_vision_input_invalid") {
		t.Fatalf("progress/warnings=%v/%+v", progress, parsed.Warnings)
	}
}

func TestOfficeBudgetExhaustionUsesSpecificDegradedWarning(t *testing.T) {
	fixture := repeatedOfficeFixture(t, "docx")
	parser := NewLocalParser(&officeFixtureExtractor{t: t, fixture: fixture}, 300)
	parsed, err := parser.Parse(context.Background(), fakeOfficeSource("docx"), ParseOptions{
		Mode: config.ParseModeAuto,
		ImageTranscriber: &recordingOfficeVision{err: &vision.Error{
			Kind: vision.ErrorBudget, Err: errors.New("task budget exhausted"),
		}},
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()
	if !hasOfficeWarning(parsed.Warnings, "office_vision_budget_exhausted") {
		t.Fatalf("warnings=%+v", parsed.Warnings)
	}
}

func hasOfficeWarning(warnings []document.ParseWarning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code && warning.Degraded {
			return true
		}
	}
	return false
}

func warningCountByCode(warnings []document.ParseWarning, code string) int {
	count := 0
	for _, warning := range warnings {
		if warning.Code == code {
			count++
		}
	}
	return count
}
