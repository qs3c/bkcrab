package parse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/assets"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

const (
	// PDFAutoRoutingVersion is part of the parse fingerprint. Any threshold or
	// quality-rule change must bump this value.
	PDFAutoRoutingVersion = "pdf-auto-routing-v1"

	defaultMaxVisionPages        = 100
	defaultMaxVisionInputBytes   = int64(8 << 20)
	defaultMaxImagePixels        = int64(40_000_000)
	defaultVisionImageMaxEdge    = 2400
	defaultMinVisualAreaPermille = 2
	visionRouteImageArea         = 150_000 // 15% in the protocol's 1000x1000 plane.
	visionNativeAnchorCoverage   = 0.70
	maxAutoEntryBytes            = int64(200 << 20)
)

var pdfVisualMarkerPattern = regexp.MustCompile(`rag-visual://[A-Za-z0-9][A-Za-z0-9._-]{0,63}`)

type pdfAutoLimits struct {
	maxPages              int
	maxVisionPages        int
	maxExtractedBytes     int64
	maxVisionInputBytes   int64
	maxImagePixels        int64
	visionImageMaxEdge    int
	minVisualAreaPermille int
	tempDir               string
}

func (p *LocalParser) autoLimits() pdfAutoLimits {
	limits := pdfAutoLimits{
		maxPages: p.MaxPages, maxVisionPages: p.MaxVisionPages,
		maxExtractedBytes: p.MaxExtractedBytes, maxVisionInputBytes: p.MaxVisionInputBytes,
		maxImagePixels: p.MaxImagePixels, visionImageMaxEdge: p.VisionImageMaxEdge,
		minVisualAreaPermille: p.MinVisualAreaPermille, tempDir: p.TempDir,
	}
	if limits.maxPages <= 0 {
		limits.maxPages = defaultMaxNativePDFPages
	}
	if limits.maxVisionPages <= 0 {
		limits.maxVisionPages = defaultMaxVisionPages
	}
	if limits.maxExtractedBytes <= 0 {
		limits.maxExtractedBytes = defaultMaxNativePDFExtractedBytes
	}
	if limits.maxVisionInputBytes <= 0 {
		limits.maxVisionInputBytes = defaultMaxVisionInputBytes
	}
	if limits.maxImagePixels <= 0 {
		limits.maxImagePixels = defaultMaxImagePixels
	}
	if limits.visionImageMaxEdge <= 0 {
		limits.visionImageMaxEdge = defaultVisionImageMaxEdge
	}
	if limits.minVisualAreaPermille <= 0 {
		limits.minVisualAreaPermille = defaultMinVisualAreaPermille
	}
	return limits
}

type pdfAnalyzePage struct {
	descriptor sidecar.PageDescriptor
	native     string
	primitive  sidecar.PagePrimitive
	vision     bool
}

type pdfPageResult struct {
	unit        *document.MarkdownUnit
	assets      []document.ExtractedAsset
	occurrences []document.AssetOccurrence
}

func (p *LocalParser) parseAutoPDF(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
) (_ *document.ParsedDocument, resultErr error) {
	limits := p.autoLimits()
	analyze, err := p.Primitives.AnalyzePDF(ctx, source)
	if err != nil {
		if analyze != nil {
			if closeErr := analyze.Close(); closeErr != nil {
				return nil, fmt.Errorf("close failed PDF analyze bundle: %w", closeErr)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return p.nativePDFWithAutoWarning(ctx, source, options, "pdf_auto_sidecar_unavailable",
			"PDF auto parser was unavailable; bounded native text extraction was used")
	}

	var render *sidecar.BundleHandle
	derived := &derivedPDFEntries{tempDir: limits.tempDir}
	var cleanupOnce sync.Once
	var cleanupErr error
	cleanup := func() error {
		cleanupOnce.Do(func() {
			if render != nil {
				cleanupErr = errors.Join(cleanupErr, render.Close())
			}
			cleanupErr = errors.Join(cleanupErr, analyze.Close(), derived.Close())
		})
		return cleanupErr
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, cleanup())
		}
	}()

	pages, warnings, err := loadPDFAnalyzePages(ctx, analyze, limits)
	if err != nil {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return nil, fmt.Errorf("close invalid PDF analyze bundle: %w", cleanupErr)
		}
		return p.nativePDFWithAutoWarning(ctx, source, options, "pdf_auto_analyze_invalid",
			"PDF page analysis was invalid; bounded native text extraction was used")
	}

	nativeFallback := newPDFNativeFallback(source, options, limits)
	defer nativeFallback.Close()
	results := make(map[int]pdfPageResult, len(pages))
	visionPages := make([]int, 0, min(len(pages), limits.maxVisionPages))
	for index := range pages {
		page := &pages[index]
		if page.descriptor.Status == sidecar.PageStatusFailed {
			unit := nativeFallback.Page(ctx, page.descriptor.Page)
			fallbackErr := nativeFallback.Err()
			if errors.Is(fallbackErr, context.Canceled) || errors.Is(fallbackErr, context.DeadlineExceeded) ||
				errors.Is(fallbackErr, ErrDocumentLimitExceeded) || errors.Is(fallbackErr, ErrSourceIntegrity) {
				return nil, fallbackErr
			}
			results[page.descriptor.Page] = pdfNativePageResult(unit)
			warnings = append(warnings, pageWarning(page.descriptor.Page, "pdf_analyze_page_failed",
				"PDF page analysis failed; native text extraction was used"))
			continue
		}
		page.vision = shouldRoutePDFPage(page.native, page.primitive)
		if !page.vision {
			results[page.descriptor.Page] = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
			continue
		}
		if len(visionPages) >= limits.maxVisionPages {
			results[page.descriptor.Page] = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
			warnings = append(warnings, pageWarning(page.descriptor.Page, "pdf_vision_page_limit",
				"PDF page used native text because the document vision-page limit was reached"))
			continue
		}
		visionPages = append(visionPages, page.descriptor.Page)
	}

	if len(visionPages) > 0 {
		if err := reportPDFVisionProgress(ctx, options.Progress, 0, len(visionPages), 0); err != nil {
			return nil, err
		}
		if options.PageTranscriber == nil || options.DocumentAIBudget == nil {
			for current, pageNumber := range visionPages {
				page := pages[pageNumber-1]
				results[pageNumber] = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
				warnings = append(warnings, pageWarning(pageNumber, "pdf_vision_unavailable",
					"PDF page used native text because DocumentAI vision was unavailable"))
				if err := reportPDFVisionProgress(ctx, options.Progress, current+1, len(visionPages), pageNumber); err != nil {
					return nil, err
				}
			}
		} else {
			render, err = p.Primitives.RenderPDF(ctx, source, visionPages)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				for current, pageNumber := range visionPages {
					page := pages[pageNumber-1]
					results[pageNumber] = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
					warnings = append(warnings, pageWarning(pageNumber, "pdf_render_page_failed",
						"PDF page render failed; native text extraction was used"))
					if progressErr := reportPDFVisionProgress(ctx, options.Progress, current+1, len(visionPages), pageNumber); progressErr != nil {
						return nil, progressErr
					}
				}
			} else if render == nil {
				for current, pageNumber := range visionPages {
					page := pages[pageNumber-1]
					results[pageNumber] = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
					warnings = append(warnings, pageWarning(pageNumber, "pdf_render_bundle_invalid",
						"PDF page render bundle was invalid; native text extraction was used"))
					if progressErr := reportPDFVisionProgress(ctx, options.Progress, current+1, len(visionPages), pageNumber); progressErr != nil {
						return nil, progressErr
					}
				}
			} else if err := sidecar.ValidatePDFBundlePair(&analyze.Manifest, &render.Manifest, visionPages); err != nil {
				for current, pageNumber := range visionPages {
					page := pages[pageNumber-1]
					results[pageNumber] = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
					warnings = append(warnings, pageWarning(pageNumber, "pdf_render_bundle_invalid",
						"PDF page render bundle was invalid; native text extraction was used"))
					if progressErr := reportPDFVisionProgress(ctx, options.Progress, current+1, len(visionPages), pageNumber); progressErr != nil {
						return nil, progressErr
					}
				}
			} else {
				warnings = append(warnings, sidecarWarnings(render.Manifest.Warnings)...)
				for current, pageNumber := range visionPages {
					page := pages[pageNumber-1]
					pageResult, pageWarnings := processPDFVisionPage(
						ctx, page, render, derived, options, limits,
					)
					if pageResult.unit == nil {
						pageResult = pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
					}
					results[pageNumber] = pageResult
					warnings = append(warnings, pageWarnings...)
					if err := reportPDFVisionProgress(ctx, options.Progress, current+1, len(visionPages), pageNumber); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	units := make([]document.MarkdownUnit, 0, len(pages))
	collector := newPDFAutoCollector(render, derived)
	for _, page := range pages {
		result := results[page.descriptor.Page]
		if result.unit == nil || strings.TrimSpace(result.unit.Markdown) == "" {
			warnings = append(warnings, pageWarning(page.descriptor.Page, "pdf_auto_page_empty",
				"PDF page had no usable native or visual text and was skipped"))
			continue
		}
		units = append(units, *result.unit)
		for _, asset := range result.assets {
			collector.AddAsset(asset)
		}
		for _, occurrence := range result.occurrences {
			collector.AddOccurrence(occurrence)
		}
	}
	if len(units) == 0 {
		return nil, ErrEmptyContent
	}
	occurrenceMap := make(map[string]document.AssetOccurrence, len(collector.occurrences))
	for _, occurrence := range collector.occurrences {
		occurrenceMap[occurrence.ID] = occurrence
	}
	normalized, normalizeWarnings, err := NormalizeMarkdown(units, occurrenceMap, true)
	if err != nil {
		return nil, fmt.Errorf("%w: normalize PDF auto Markdown: %v", ErrInvalidDocument, err)
	}
	warnings = append(warnings, normalizeWarnings...)
	normalized, warnings = retainNonEmptyPDFUnits(normalized, warnings)
	if len(normalized) == 0 {
		return nil, ErrEmptyContent
	}

	version := strings.TrimSpace(options.ParserVersion)
	if version == "" {
		version = PDFAutoRoutingVersion
	}
	parsed := document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser: document.ParserInfo{
			Name: "pdf-auto", Version: version, WrapperVersion: analyze.Manifest.Parser.WrapperVersion,
		},
		Units: normalized, Assets: collector.assets, Occurrences: collector.occurrences, Warnings: warnings,
	}, collector.OpenEntry, cleanup)
	if err := parsed.Validate(); err != nil {
		_ = parsed.Close()
		return nil, fmt.Errorf("validate PDF auto document: %w", err)
	}
	return parsed, nil
}

func (p *LocalParser) nativePDFWithAutoWarning(
	ctx context.Context,
	source document.Source,
	options ParseOptions,
	code string,
	message string,
) (*document.ParsedDocument, error) {
	limits := p.autoLimits()
	parsed, err := parseNativePDFWithLimit(ctx, source, options, limits.maxPages, limits.maxExtractedBytes, limits.tempDir)
	if err != nil {
		return nil, err
	}
	location := document.SourceLocation{Kind: document.LocationDocument}
	parsed.Warnings = append(parsed.Warnings, document.ParseWarning{
		Code: code, Message: message, Location: &location, Degraded: true,
	})
	if err := parsed.Validate(); err != nil {
		_ = parsed.Close()
		return nil, err
	}
	return parsed, nil
}

func loadPDFAnalyzePages(ctx context.Context, bundle *sidecar.BundleHandle, limits pdfAutoLimits) ([]pdfAnalyzePage, []document.ParseWarning, error) {
	if bundle == nil || bundle.Manifest.BundleKind != sidecar.BundleKindPDFAnalyze {
		return nil, nil, errors.New("invalid PDF analyze handle")
	}
	if len(bundle.Manifest.Pages) == 0 || len(bundle.Manifest.Pages) > limits.maxPages {
		return nil, nil, ErrDocumentLimitExceeded
	}
	entries := entryDescriptors(bundle.Manifest.Entries)
	pages := make([]pdfAnalyzePage, 0, len(bundle.Manifest.Pages))
	for _, descriptor := range bundle.Manifest.Pages {
		page := pdfAnalyzePage{descriptor: descriptor}
		if descriptor.Status == sidecar.PageStatusOK {
			entry, ok := entries[descriptor.NativeMarkdownEntry]
			if !ok {
				return nil, nil, errors.New("missing native Markdown descriptor")
			}
			markdown, err := readBoundedBundleEntry(ctx, bundle, descriptor.NativeMarkdownEntry, minPositiveInt64(entry.ByteSize, limits.maxExtractedBytes))
			if err != nil {
				return nil, nil, err
			}
			if !utf8.Valid(markdown) {
				return nil, nil, errors.New("native PDF Markdown is not UTF-8")
			}
			page.native = string(markdown)
			primitive, err := bundle.PagePrimitive(ctx, descriptor.Page)
			if err != nil {
				return nil, nil, err
			}
			page.primitive = primitive
		}
		pages = append(pages, page)
	}
	return pages, sidecarWarnings(bundle.Manifest.Warnings), nil
}

func shouldRoutePDFPage(nativeMarkdown string, primitive sidecar.PagePrimitive) bool {
	if nonBlankRunesInBlocks(primitive.TextBlocks) < 80 {
		return true
	}
	if meaningfulImageUnionArea(primitive.EmbeddedImages) >= visionRouteImageArea {
		return true
	}
	if primitive.Signals.Table || primitive.Signals.Code || primitive.Signals.Multicolumn || primitive.Signals.ReadingOrderUncertain {
		return true
	}
	if primitive.Signals.Scanned {
		return true
	}
	return nativeMarkdownStatsMismatch(nativeMarkdown, primitive.TextChars)
}

func nonBlankRunesInBlocks(blocks []sidecar.PrimitiveTextBlock) int {
	count := 0
	for _, block := range blocks {
		for _, value := range block.Text {
			if !unicode.IsSpace(value) {
				count++
			}
		}
	}
	return count
}

func meaningfulImageUnionArea(images []sidecar.PrimitiveEmbeddedImage) int {
	rectangles := make([]document.NormalizedBBox, 0, len(images))
	for _, image := range images {
		if len(image.BBox) != 4 {
			continue
		}
		bbox := document.NormalizedBBox(image.BBox)
		if bbox.Validate() != nil || bboxArea(bbox) < defaultMinVisualAreaPermille*1000 {
			continue
		}
		rectangles = append(rectangles, bbox)
	}
	return normalizedRectangleUnionArea(rectangles)
}

func normalizedRectangleUnionArea(rectangles []document.NormalizedBBox) int {
	if len(rectangles) == 0 {
		return 0
	}
	xs := make([]int, 0, len(rectangles)*2)
	for _, rectangle := range rectangles {
		xs = append(xs, rectangle[0], rectangle[2])
	}
	sort.Ints(xs)
	area := 0
	for index := 0; index+1 < len(xs); index++ {
		x0, x1 := xs[index], xs[index+1]
		if x0 == x1 {
			continue
		}
		intervals := make([][2]int, 0, len(rectangles))
		for _, rectangle := range rectangles {
			if rectangle[0] < x1 && rectangle[2] > x0 {
				intervals = append(intervals, [2]int{rectangle[1], rectangle[3]})
			}
		}
		sort.Slice(intervals, func(i, j int) bool { return intervals[i][0] < intervals[j][0] })
		covered := 0
		if len(intervals) > 0 {
			start, end := intervals[0][0], intervals[0][1]
			for _, interval := range intervals[1:] {
				if interval[0] > end {
					covered += end - start
					start, end = interval[0], interval[1]
				} else if interval[1] > end {
					end = interval[1]
				}
			}
			covered += end - start
		}
		area += (x1 - x0) * covered
	}
	return min(area, 1_000_000)
}

func nativeMarkdownStatsMismatch(markdown string, textChars int) bool {
	if textChars <= 0 {
		return strings.TrimSpace(markdown) != ""
	}
	visible := normalizedAnchorText(markdown)
	count := utf8.RuneCountInString(visible)
	return count*10 < textChars*7 || count*10 > textChars*17
}

func processPDFVisionPage(
	ctx context.Context,
	page pdfAnalyzePage,
	render *sidecar.BundleHandle,
	derived *derivedPDFEntries,
	options ParseOptions,
	limits pdfAutoLimits,
) (pdfPageResult, []document.ParseWarning) {
	pageNumber := page.descriptor.Page
	renderPage, ok := findPDFPage(render.Manifest.Pages, pageNumber)
	if !ok || renderPage.Status != sidecar.PageStatusOK {
		return pdfPageResult{}, []document.ParseWarning{pageWarning(pageNumber, "pdf_render_page_failed",
			"PDF page render failed; native text extraction was used")}
	}
	entry, ok := entryDescriptors(render.Manifest.Entries)[renderPage.RenderEntry]
	if !ok {
		return pdfPageResult{}, []document.ParseWarning{pageWarning(pageNumber, "pdf_render_page_failed",
			"PDF page render was unavailable; native text extraction was used")}
	}
	raw, err := readBoundedBundleEntry(ctx, render, renderPage.RenderEntry, minPositiveInt64(entry.ByteSize, limits.maxVisionInputBytes))
	if err != nil {
		return pdfVisionFallbackPage(page, render, pageNumber, "pdf_render_page_failed",
			"PDF page render could not be read; native text extraction was used")
	}
	normalized, err := vision.NormalizeImage(ctx, raw, entry.MIMEType, vision.ImageLimits{
		MaxSourceBytes: limits.maxVisionInputBytes, MaxEncodedBytes: limits.maxVisionInputBytes,
		MaxBase64Bytes: limits.maxVisionInputBytes, MaxPixels: limits.maxImagePixels,
		MaxEdge: limits.visionImageMaxEdge,
	})
	if err != nil {
		return pdfVisionFallbackPage(page, render, pageNumber, "pdf_vision_input_invalid",
			"PDF page could not be normalized for vision; native text extraction was used")
	}
	normalized.Format = "pdf"
	normalized.Location = pageLocation(pageNumber)
	normalized.Scope = options.VisionScope
	transcription, err := options.PageTranscriber.TranscribePage(ctx, vision.PageInput{Image: normalized}, options.DocumentAIBudget)
	if err != nil {
		return pdfVisionFallbackPage(page, render, pageNumber, "pdf_vision_page_failed",
			"PDF page vision failed; native text extraction was used")
	}
	if err := transcription.Validate(vision.DefaultSchemaLimits()); err != nil {
		return pdfVisionFallbackPage(page, render, pageNumber, "pdf_vision_page_failed",
			"PDF page vision response was invalid; native text extraction was used")
	}
	if err := validatePDFPageFidelity(page.primitive, transcription); err != nil {
		return pdfVisionFallbackPage(page, render, pageNumber, "pdf_vision_fidelity_failed",
			"PDF page vision omitted or expanded native text; native text extraction was used")
	}
	result, err := bindPDFVisuals(ctx, page, transcription, raw, entry.MIMEType, render, derived, limits)
	if err != nil {
		return pdfVisionFallbackPage(page, render, pageNumber, "pdf_vision_asset_failed",
			"PDF visual resources could not be safely bound; native text extraction was used")
	}
	return result, nil
}

func bindPDFVisuals(
	ctx context.Context,
	page pdfAnalyzePage,
	transcription vision.PageTranscription,
	renderBytes []byte,
	renderMIME string,
	render *sidecar.BundleHandle,
	derived *derivedPDFEntries,
	limits pdfAutoLimits,
) (pdfPageResult, error) {
	pageNumber := page.descriptor.Page
	markdown := transcription.Markdown
	result := pdfPageResult{}
	embedded := renderOccurrencesForPage(render.Manifest, pageNumber)
	markerOrders := visualMarkerOrders(markdown, transcription.Visuals)
	markerBindings := make(map[string]string, len(transcription.Visuals))
	for _, visual := range transcription.Visuals {
		order, ok := markerOrders[visual.Key]
		if !ok {
			return pdfPageResult{}, errors.New("visual marker order is unavailable")
		}
		occurrenceID := fmt.Sprintf("occ_pdf_%04d_%s", pageNumber, visual.Key)
		asset, err := matchedEmbeddedAsset(render.Manifest, embedded, visual.BBox)
		if err != nil {
			return pdfPageResult{}, err
		}
		if asset == nil {
			crop, cropErr := assets.CropRaster(ctx, renderBytes, renderMIME, visual.BBox, assets.SafeImageLimits{
				MaxSourceBytes: limits.maxVisionInputBytes, MaxPixels: limits.maxImagePixels,
				DisplayMaxEdge: limits.visionImageMaxEdge, ThumbnailMaxEdge: min(480, limits.visionImageMaxEdge),
			})
			if cropErr != nil {
				return pdfPageResult{}, cropErr
			}
			entryPath, storeErr := derived.Add(crop.Bytes, crop.SHA256)
			if storeErr != nil {
				return pdfPageResult{}, storeErr
			}
			asset = &document.ExtractedAsset{
				LocalID: pdfAssetLocalID(crop.SHA256), ContentSHA256: crop.SHA256,
				Kind: document.AssetKindImage, SourceKind: document.SourceKindPageCrop,
				SourceMIME: crop.MIMEType, Width: crop.Width, Height: crop.Height,
				ByteSize: int64(len(crop.Bytes)), BundleEntry: entryPath,
			}
		}
		decorative := visual.Decorative || bboxArea(visual.BBox) < limits.minVisualAreaPermille*1000
		bbox := visual.BBox
		result.assets = append(result.assets, *asset)
		result.occurrences = append(result.occurrences, document.AssetOccurrence{
			ID: occurrenceID, AssetLocalID: asset.LocalID, UnitID: page.descriptor.UnitID,
			Order: order, Location: pageLocation(pageNumber), BBox: &bbox,
			Caption: visual.Caption, OCRText: visual.OCRText, Decorative: decorative,
			Confidence: visual.Confidence,
		})
		markerBindings[visual.Key] = occurrenceID
	}
	markdown, err := bindPDFVisualMarkers(markdown, markerBindings)
	if err != nil {
		return pdfPageResult{}, err
	}
	if len(transcription.Visuals) == 0 && page.primitive.Signals.Scanned {
		full := document.NormalizedBBox{0, 0, 1000, 1000}
		scan, err := assets.CropRaster(ctx, renderBytes, renderMIME, full, assets.SafeImageLimits{
			MaxSourceBytes: limits.maxVisionInputBytes, MaxPixels: limits.maxImagePixels,
			DisplayMaxEdge: limits.visionImageMaxEdge, ThumbnailMaxEdge: min(480, limits.visionImageMaxEdge),
		})
		if err != nil {
			return pdfPageResult{}, err
		}
		entryPath, err := derived.Add(scan.Bytes, scan.SHA256)
		if err != nil {
			return pdfPageResult{}, err
		}
		occurrenceID := fmt.Sprintf("occ_pdf_%04d_scanned", pageNumber)
		result.assets = append(result.assets, document.ExtractedAsset{
			LocalID: pdfAssetLocalID(scan.SHA256), ContentSHA256: scan.SHA256,
			Kind: document.AssetKindImage, SourceKind: document.SourceKindScannedPage,
			SourceMIME: scan.MIMEType, Width: scan.Width, Height: scan.Height,
			ByteSize: int64(len(scan.Bytes)), BundleEntry: entryPath,
		})
		result.occurrences = append(result.occurrences, document.AssetOccurrence{
			ID: occurrenceID, AssetLocalID: pdfAssetLocalID(scan.SHA256), UnitID: page.descriptor.UnitID,
			Order: 1, Location: pageLocation(pageNumber), BBox: &full,
			Caption: "扫描页", Confidence: 1,
		})
		markdown = appendInternalImage(markdown, "扫描页", occurrenceID, "")
	}
	result.unit = markdownUnitForPage(page.descriptor, markdown)
	return result, nil
}

func pdfVisionFallbackPage(page pdfAnalyzePage, render *sidecar.BundleHandle, pageNumber int, code, message string) (pdfPageResult, []document.ParseWarning) {
	result := pdfNativePageResult(markdownUnitForPage(page.descriptor, page.native))
	if result.unit != nil && render != nil {
		entries := entryDescriptors(render.Manifest.Entries)
		assetsByID := make(map[string]sidecar.AssetDescriptor, len(render.Manifest.Assets))
		for _, asset := range render.Manifest.Assets {
			assetsByID[asset.LocalID] = asset
		}
		for _, occurrence := range renderOccurrencesForPage(render.Manifest, pageNumber) {
			if occurrence.Decorative {
				continue
			}
			descriptor, ok := assetsByID[occurrence.AssetLocalID]
			entry, entryOK := entries[descriptor.Entry]
			if !ok || !entryOK || descriptor.SourceKind != document.SourceKindEmbeddedOriginal {
				continue
			}
			hash := entry.SHA256
			asset := document.ExtractedAsset{
				LocalID: pdfAssetLocalID(hash), ContentSHA256: hash, Kind: document.AssetKindImage,
				SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: entry.MIMEType,
				Width: descriptor.Width, Height: descriptor.Height, ByteSize: entry.ByteSize,
				BundleEntry: "render/" + descriptor.Entry,
			}
			bbox := document.NormalizedBBox(occurrence.BBox)
			caption := strings.TrimSpace(occurrence.AltText)
			if caption == "" {
				caption = "图片（视觉识别失败）"
			}
			occurrenceID := fmt.Sprintf("occ_pdf_%04d_fallback_%04d", pageNumber, occurrence.Order)
			result.assets = append(result.assets, asset)
			result.occurrences = append(result.occurrences, document.AssetOccurrence{
				ID: occurrenceID, AssetLocalID: asset.LocalID, UnitID: page.descriptor.UnitID,
				Order: occurrence.Order, Location: pageLocation(pageNumber), BBox: &bbox,
				AltText: caption, Confidence: occurrence.Confidence,
			})
			result.unit.Markdown = appendInternalImage(result.unit.Markdown, caption, occurrenceID, "")
		}
	}
	return result, []document.ParseWarning{pageWarning(pageNumber, code, message)}
}

func matchedEmbeddedAsset(
	manifest sidecar.Manifest,
	occurrences []sidecar.OccurrenceDescriptor,
	visualBBox document.NormalizedBBox,
) (*document.ExtractedAsset, error) {
	var match *sidecar.OccurrenceDescriptor
	for index := range occurrences {
		candidate := &occurrences[index]
		if len(candidate.BBox) != 4 || !reliableBBoxMatch(visualBBox, document.NormalizedBBox(candidate.BBox)) {
			continue
		}
		if match != nil {
			return nil, nil
		}
		match = candidate
	}
	if match == nil {
		return nil, nil
	}
	assetsByID := make(map[string]sidecar.AssetDescriptor, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		assetsByID[asset.LocalID] = asset
	}
	descriptor, ok := assetsByID[match.AssetLocalID]
	if !ok || descriptor.SourceKind != document.SourceKindEmbeddedOriginal {
		return nil, nil
	}
	entry, ok := entryDescriptors(manifest.Entries)[descriptor.Entry]
	if !ok {
		return nil, errors.New("embedded asset entry is missing")
	}
	return &document.ExtractedAsset{
		LocalID: pdfAssetLocalID(entry.SHA256), ContentSHA256: entry.SHA256,
		Kind: document.AssetKindImage, SourceKind: document.SourceKindEmbeddedOriginal,
		SourceMIME: entry.MIMEType, Width: descriptor.Width, Height: descriptor.Height,
		ByteSize: entry.ByteSize, BundleEntry: "render/" + descriptor.Entry,
	}, nil
}

func reliableBBoxMatch(left, right document.NormalizedBBox) bool {
	intersection := bboxIntersectionArea(left, right)
	if intersection <= 0 {
		return false
	}
	leftArea, rightArea := bboxArea(left), bboxArea(right)
	union := leftArea + rightArea - intersection
	return intersection*100 >= min(leftArea, rightArea)*80 && intersection*100 >= union*50
}

func bboxArea(value document.NormalizedBBox) int {
	return (value[2] - value[0]) * (value[3] - value[1])
}

func bboxIntersectionArea(left, right document.NormalizedBBox) int {
	width := min(left[2], right[2]) - max(left[0], right[0])
	height := min(left[3], right[3]) - max(left[1], right[1])
	if width <= 0 || height <= 0 {
		return 0
	}
	return width * height
}

func validatePDFPageFidelity(primitive sidecar.PagePrimitive, transcription vision.PageTranscription) error {
	native := make([]rune, 0)
	for _, block := range primitive.TextBlocks {
		native = append(native, []rune(normalizedAnchorText(block.Text))...)
	}
	output := []rune(normalizedAnchorText(transcription.Markdown))
	for _, visual := range transcription.Visuals {
		output = append(output, []rune(normalizedAnchorText(visual.Caption))...)
		output = append(output, []rune(normalizedAnchorText(visual.OCRText))...)
	}
	if len(native) == 0 {
		if len(output) == 0 {
			return errors.New("scan page transcription is empty")
		}
		return nil
	}
	anchors := runeAnchors(native, 12)
	matched := 0
	outputText := string(output)
	for _, anchor := range anchors {
		if strings.Contains(outputText, anchor) {
			matched += utf8.RuneCountInString(anchor)
		}
	}
	total := 0
	for _, anchor := range anchors {
		total += utf8.RuneCountInString(anchor)
	}
	if total == 0 || float64(matched)/float64(total) < visionNativeAnchorCoverage {
		return errors.New("native anchor coverage below 70 percent")
	}
	if len(output) > max(len(native)*4, len(native)+2048) {
		return errors.New("vision transcription expanded native text abnormally")
	}
	return nil
}

func normalizedAnchorText(value string) string {
	var output strings.Builder
	for _, character := range strings.ToLower(value) {
		if unicode.IsLetter(character) || unicode.IsNumber(character) {
			output.WriteRune(character)
		}
	}
	return output.String()
}

func runeAnchors(value []rune, width int) []string {
	if len(value) == 0 {
		return nil
	}
	if len(value) <= width {
		return []string{string(value)}
	}
	anchors := make([]string, 0, (len(value)+width-1)/width)
	for start := 0; start < len(value); start += width {
		end := min(len(value), start+width)
		anchors = append(anchors, string(value[start:end]))
	}
	return anchors
}

func visualMarkerOrders(markdown string, visuals []vision.Visual) map[string]int {
	type marker struct {
		key   string
		index int
	}
	wanted := make(map[string]struct{}, len(visuals))
	for _, visual := range visuals {
		wanted[visual.Key] = struct{}{}
	}
	markers := make([]marker, 0, len(visuals))
	for _, indices := range pdfVisualMarkerPattern.FindAllStringIndex(markdown, -1) {
		key := strings.TrimPrefix(markdown[indices[0]:indices[1]], "rag-visual://")
		if _, ok := wanted[key]; ok {
			markers = append(markers, marker{key: key, index: indices[0]})
		}
	}
	if len(markers) != len(visuals) {
		return nil
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].index < markers[j].index })
	orders := make(map[string]int, len(markers))
	for index, marker := range markers {
		if _, duplicate := orders[marker.key]; duplicate {
			return nil
		}
		orders[marker.key] = index + 1
	}
	return orders
}

func bindPDFVisualMarkers(markdown string, bindings map[string]string) (string, error) {
	seen := make(map[string]struct{}, len(bindings))
	bound := pdfVisualMarkerPattern.ReplaceAllStringFunc(markdown, func(marker string) string {
		key := strings.TrimPrefix(marker, "rag-visual://")
		occurrenceID, ok := bindings[key]
		if !ok {
			return marker
		}
		seen[key] = struct{}{}
		return "rag-asset://" + occurrenceID
	})
	if strings.Contains(bound, "rag-visual://") || len(seen) != len(bindings) {
		return "", errors.New("unresolved visual marker")
	}
	return bound, nil
}

func appendInternalImage(markdown, caption, occurrenceID, ocr string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		caption = "图片"
	}
	markdown = strings.TrimSpace(markdown) + "\n\n![" + strings.ReplaceAll(caption, "]", "\\]") + "](rag-asset://" + occurrenceID + ")"
	if strings.TrimSpace(ocr) != "" {
		markdown += "\n\n> 图片文字：" + strings.TrimSpace(ocr)
	}
	return strings.TrimSpace(markdown)
}

func markdownUnitForPage(descriptor sidecar.PageDescriptor, markdown string) *document.MarkdownUnit {
	if strings.TrimSpace(markdown) == "" {
		return nil
	}
	return &document.MarkdownUnit{
		ID: descriptor.UnitID, Location: pageLocation(descriptor.Page), Markdown: markdown,
	}
}

func pdfNativePageResult(unit *document.MarkdownUnit) pdfPageResult {
	return pdfPageResult{unit: unit}
}

func pageLocation(page int) document.SourceLocation {
	return document.SourceLocation{Kind: document.LocationPage, Index: page, Label: fmt.Sprintf("第 %d 页", page)}
}

func pageWarning(page int, code, message string) document.ParseWarning {
	location := pageLocation(page)
	return document.ParseWarning{Code: code, Message: message, Location: &location, Degraded: true}
}

func sidecarWarnings(values []sidecar.WarningDescriptor) []document.ParseWarning {
	warnings := make([]document.ParseWarning, 0, len(values))
	for _, value := range values {
		var location *document.SourceLocation
		if value.Location != nil {
			converted := document.SourceLocation{Kind: value.Location.Kind, Index: value.Location.Index, Label: value.Location.Label}
			location = &converted
		}
		warnings = append(warnings, document.ParseWarning{
			Code: value.Code, Message: value.Message, Location: location, Degraded: value.Degraded,
		})
	}
	return warnings
}

func reportPDFVisionProgress(ctx context.Context, callback ParseProgressFunc, current, total, page int) error {
	if callback == nil {
		return nil
	}
	message := fmt.Sprintf("准备解析 %d 个视觉页", total)
	if current > 0 {
		message = fmt.Sprintf("正在解析第 %d/%d 个视觉页（原文第 %d 页）", current, total, page)
	}
	return callback(ctx, ParseProgress{
		Stage: "vision", Current: current, Total: total, Unit: "pages",
		Message: message,
	})
}

func findPDFPage(pages []sidecar.PageDescriptor, page int) (sidecar.PageDescriptor, bool) {
	for _, descriptor := range pages {
		if descriptor.Page == page {
			return descriptor, true
		}
	}
	return sidecar.PageDescriptor{}, false
}

func renderOccurrencesForPage(manifest sidecar.Manifest, page int) []sidecar.OccurrenceDescriptor {
	out := make([]sidecar.OccurrenceDescriptor, 0)
	for _, occurrence := range manifest.Occurrences {
		if occurrence.Location.Kind == document.LocationPage && occurrence.Location.Index == page {
			out = append(out, occurrence)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

func entryDescriptors(entries []sidecar.EntryDescriptor) map[string]sidecar.EntryDescriptor {
	out := make(map[string]sidecar.EntryDescriptor, len(entries))
	for _, entry := range entries {
		out[entry.Path] = entry
	}
	return out
}

func readBoundedBundleEntry(ctx context.Context, bundle *sidecar.BundleHandle, entryPath string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes > maxAutoEntryBytes {
		maxBytes = maxAutoEntryBytes
	}
	reader, err := bundle.OpenEntry(ctx, entryPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	value, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > maxBytes {
		return nil, ErrDocumentLimitExceeded
	}
	return value, nil
}

func minPositiveInt64(left, right int64) int64 {
	if left <= 0 {
		return right
	}
	if right <= 0 || left < right {
		return left
	}
	return right
}

func pdfAssetLocalID(contentSHA256 string) string {
	return "asset_pdf_" + contentSHA256[:24]
}

type pdfNativeFallback struct {
	source  document.Source
	options ParseOptions
	limits  pdfAutoLimits
	once    sync.Once
	doc     *document.ParsedDocument
	err     error
	units   map[int]document.MarkdownUnit
}

func newPDFNativeFallback(source document.Source, options ParseOptions, limits pdfAutoLimits) *pdfNativeFallback {
	return &pdfNativeFallback{source: source, options: options, limits: limits}
}

func (f *pdfNativeFallback) Page(ctx context.Context, page int) *document.MarkdownUnit {
	f.once.Do(func() {
		f.doc, f.err = parseNativePDFWithLimit(ctx, f.source, f.options, f.limits.maxPages, f.limits.maxExtractedBytes, f.limits.tempDir)
		if f.err != nil {
			return
		}
		f.units = make(map[int]document.MarkdownUnit, len(f.doc.Units))
		for _, unit := range f.doc.Units {
			f.units[unit.Location.Index] = unit
		}
	})
	unit, ok := f.units[page]
	if !ok {
		return nil
	}
	copy := unit
	return &copy
}

func (f *pdfNativeFallback) Close() error {
	if f == nil || f.doc == nil {
		return nil
	}
	return f.doc.Close()
}

func (f *pdfNativeFallback) Err() error {
	if f == nil {
		return nil
	}
	return f.err
}

type derivedPDFEntries struct {
	tempDir  string
	root     string
	entries  map[string]string
	once     sync.Once
	closeErr error
}

func (d *derivedPDFEntries) Add(value []byte, hash string) (string, error) {
	if !document.CanonicalSHA256(hash) {
		return "", errors.New("derived PDF asset hash is invalid")
	}
	if d.root == "" {
		root, err := os.MkdirTemp(d.tempDir, "bkcrab-rag-pdf-auto-*")
		if err != nil {
			return "", redactFileOperationError("create PDF auto asset directory", err)
		}
		d.root = root
		d.entries = make(map[string]string)
	}
	entry := "derived/" + hash + ".png"
	if _, exists := d.entries[entry]; exists {
		return entry, nil
	}
	path := d.root + string(os.PathSeparator) + hash + ".png"
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", redactFileOperationError("create derived PDF asset", err)
	}
	written, writeErr := file.Write(value)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || written != len(value) {
		_ = os.Remove(path)
		return "", errors.Join(writeErr, closeErr, errors.New("write derived PDF asset failed"))
	}
	d.entries[entry] = path
	return entry, nil
}

func (d *derivedPDFEntries) Open(ctx context.Context, entry string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, ok := d.entries[entry]
	if !ok {
		return nil, os.ErrNotExist
	}
	return os.Open(path)
}

func (d *derivedPDFEntries) Close() error {
	if d == nil {
		return nil
	}
	d.once.Do(func() {
		if d.root != "" {
			d.closeErr = os.RemoveAll(d.root)
		}
	})
	return d.closeErr
}

type pdfAutoCollector struct {
	render      *sidecar.BundleHandle
	derived     *derivedPDFEntries
	assets      []document.ExtractedAsset
	occurrences []document.AssetOccurrence
	assetIndex  map[string]int
	occurrence  map[string]struct{}
}

func newPDFAutoCollector(render *sidecar.BundleHandle, derived *derivedPDFEntries) *pdfAutoCollector {
	return &pdfAutoCollector{
		render: render, derived: derived, assetIndex: make(map[string]int), occurrence: make(map[string]struct{}),
	}
}

func (c *pdfAutoCollector) AddAsset(asset document.ExtractedAsset) {
	if _, ok := c.assetIndex[asset.ContentSHA256]; ok {
		return
	}
	c.assetIndex[asset.ContentSHA256] = len(c.assets)
	c.assets = append(c.assets, asset)
}

func (c *pdfAutoCollector) AddOccurrence(occurrence document.AssetOccurrence) {
	if _, duplicate := c.occurrence[occurrence.ID]; duplicate {
		return
	}
	if index, ok := c.assetIndexByLocalID(occurrence.AssetLocalID); ok {
		occurrence.AssetLocalID = c.assets[index].LocalID
	}
	c.occurrence[occurrence.ID] = struct{}{}
	c.occurrences = append(c.occurrences, occurrence)
}

func (c *pdfAutoCollector) assetIndexByLocalID(localID string) (int, bool) {
	for index := range c.assets {
		if c.assets[index].LocalID == localID {
			return index, true
		}
	}
	return 0, false
}

func (c *pdfAutoCollector) OpenEntry(ctx context.Context, entry string) (io.ReadCloser, error) {
	if strings.HasPrefix(entry, "render/") && c.render != nil {
		return c.render.OpenEntry(ctx, strings.TrimPrefix(entry, "render/"))
	}
	if strings.HasPrefix(entry, "derived/") && c.derived != nil {
		return c.derived.Open(ctx, entry)
	}
	return nil, os.ErrNotExist
}
