package parse

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

const (
	OfficeMarkItDownVersion = sidecar.ExpectedMarkItDownVersion
	OfficeWrapperVersion    = sidecar.ExpectedOfficeWrapper
	// OfficeParserVersion covers the Go bundle mapper and the exact wrapper
	// positioning contract. Bump it whenever either behavior changes.
	OfficeParserVersion = "office-parser-v1+" + OfficeWrapperVersion

	defaultMaxVisionAssets = 100
	defaultMaxOfficeAssets = 500
	defaultMaxAssetBytes   = int64(20 << 20)
)

var officeCellAnchorAltPattern = regexp.MustCompile(`^(单元格 [A-Z]+[1-9][0-9]*：)`)

type officeParseLimits struct {
	maxPages            int
	maxAssets           int
	maxVisionAssets     int
	maxExtractedBytes   int64
	maxAssetBytes       int64
	maxVisionInputBytes int64
	maxImagePixels      int64
	visionImageMaxEdge  int
}

func (p *LocalParser) officeLimits() officeParseLimits {
	limits := officeParseLimits{
		maxPages: p.MaxPages, maxAssets: p.MaxAssets,
		maxVisionAssets: p.MaxVisionAssets, maxExtractedBytes: p.MaxExtractedBytes,
		maxAssetBytes: p.MaxAssetBytes, maxVisionInputBytes: p.MaxVisionInputBytes,
		maxImagePixels: p.MaxImagePixels, visionImageMaxEdge: p.VisionImageMaxEdge,
	}
	if limits.maxPages <= 0 {
		limits.maxPages = defaultMaxNativePDFPages
	}
	if limits.maxAssets <= 0 {
		limits.maxAssets = defaultMaxOfficeAssets
	}
	if limits.maxVisionAssets <= 0 {
		limits.maxVisionAssets = defaultMaxVisionAssets
	}
	if limits.maxExtractedBytes <= 0 {
		limits.maxExtractedBytes = defaultMaxNativePDFExtractedBytes
	}
	if limits.maxAssetBytes <= 0 {
		limits.maxAssetBytes = defaultMaxAssetBytes
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
	return limits
}

func (p *LocalParser) parseOffice(
	ctx context.Context,
	source document.Source,
	format string,
	options ParseOptions,
) (_ *document.ParsedDocument, resultErr error) {
	bundle, err := p.Primitives.ConvertOffice(ctx, source)
	if err != nil {
		if bundle != nil {
			return nil, errors.Join(err, bundle.Close())
		}
		return nil, err
	}
	if bundle == nil {
		return nil, fmt.Errorf("%w: Office sidecar returned a nil bundle", ErrInvalidDocument)
	}
	transferred := false
	defer func() {
		if !transferred {
			resultErr = errors.Join(resultErr, bundle.Close())
		}
	}()
	if bundle.Manifest.BundleKind != sidecar.BundleKindOfficeConvert || bundle.Manifest.Source.Format != format {
		return nil, fmt.Errorf("%w: Office bundle kind/source mismatch", ErrInvalidDocument)
	}

	limits := p.officeLimits()
	entries := entryDescriptors(bundle.Manifest.Entries)
	if err := validateOfficeBundleLimits(bundle.Manifest, entries, limits); err != nil {
		return nil, err
	}
	units, err := loadOfficeUnits(ctx, bundle, entries, limits.maxExtractedBytes)
	if err != nil {
		return nil, err
	}
	assets, localIDs, err := loadOfficeAssets(bundle.Manifest, entries)
	if err != nil {
		return nil, err
	}
	attachments, attachmentLocalIDs, err := loadOfficeAttachments(bundle.Manifest, entries)
	if err != nil {
		return nil, err
	}
	occurrences, err := loadOfficeOccurrences(bundle.Manifest.Occurrences, localIDs, attachmentLocalIDs)
	if err != nil {
		return nil, err
	}
	// Missing-alt degradation is mode-dependent: standard retains the neutral
	// placeholder, while a successful auto description resolves the omission.
	// The sidecar cannot know the parse mode, so the Go boundary owns this code.
	warnings := officeWarningsWithoutCode(
		sidecarWarnings(bundle.Manifest.Warnings), "office_image_alt_missing",
	)
	if options.Mode == config.ParseModeAuto {
		var visionWarnings []document.ParseWarning
		occurrences, visionWarnings, err = p.describeOfficeAssets(
			ctx, bundle, format, assets, occurrences, options, limits,
		)
		if err != nil {
			return nil, err
		}
		warnings = append(warnings, visionWarnings...)
	}
	warnings = append(warnings, officeMissingAltWarnings(occurrences)...)

	occurrenceMap := make(map[string]document.AssetOccurrence, len(occurrences))
	for _, occurrence := range occurrences {
		occurrenceMap[occurrence.ID] = occurrence
	}
	units, normalizeWarnings, err := NormalizeMarkdown(units, occurrenceMap, true)
	if err != nil {
		return nil, fmt.Errorf("%w: normalize Office Markdown: %v", ErrInvalidDocument, err)
	}
	if err := validateOfficeOccurrenceMarkers(units, occurrences); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDocument, err)
	}
	warnings = append(warnings, normalizeWarnings...)
	if !officeHasContent(units) {
		return nil, ErrEmptyContent
	}
	version := strings.TrimSpace(options.ParserVersion)
	if version == "" {
		version = OfficeParserVersion
	}
	parsed := document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source:        source.Parsed(),
		Parser: document.ParserInfo{
			Name: "markitdown-office", Version: version,
			WrapperVersion: bundle.Manifest.Parser.WrapperVersion,
		},
		Units: units, Assets: assets, Attachments: attachments,
		Occurrences: occurrences, Warnings: warnings,
	}, bundle.OpenEntry, bundle.Close)
	if err := parsed.Validate(); err != nil {
		_ = parsed.Close()
		return nil, fmt.Errorf("validate Office parsed document: %w", err)
	}
	transferred = true
	return parsed, nil
}

func validateOfficeBundleLimits(
	manifest sidecar.Manifest,
	entries map[string]sidecar.EntryDescriptor,
	limits officeParseLimits,
) error {
	if len(manifest.Units) == 0 {
		return fmt.Errorf("%w: Office bundle has no Markdown units", ErrInvalidDocument)
	}
	if len(manifest.Units) > limits.maxPages {
		return fmt.Errorf("%w: Office unit count exceeds %d", ErrDocumentLimitExceeded, limits.maxPages)
	}
	if len(manifest.Assets)+len(manifest.Attachments) > limits.maxAssets {
		return fmt.Errorf("%w: Office asset/attachment count exceeds %d", ErrDocumentLimitExceeded, limits.maxAssets)
	}
	var total int64
	for _, entry := range manifest.Entries {
		if entry.ByteSize < 0 || entry.ByteSize > limits.maxExtractedBytes-total {
			return fmt.Errorf("%w: Office extracted bytes exceed %d", ErrDocumentLimitExceeded, limits.maxExtractedBytes)
		}
		total += entry.ByteSize
	}
	for _, asset := range manifest.Assets {
		entry, ok := entries[asset.Entry]
		if !ok {
			return fmt.Errorf("%w: Office asset entry %q is missing", ErrInvalidDocument, asset.Entry)
		}
		if entry.ByteSize > limits.maxAssetBytes {
			return fmt.Errorf("%w: Office asset %q exceeds %d bytes", ErrDocumentLimitExceeded, asset.LocalID, limits.maxAssetBytes)
		}
	}
	for _, attachment := range manifest.Attachments {
		entry, ok := entries[attachment.Entry]
		if !ok {
			return fmt.Errorf("%w: Office attachment entry %q is missing", ErrInvalidDocument, attachment.Entry)
		}
		if entry.ByteSize > limits.maxAssetBytes {
			return fmt.Errorf("%w: Office attachment %q exceeds %d bytes", ErrDocumentLimitExceeded, attachment.LocalID, limits.maxAssetBytes)
		}
	}
	return nil
}

func loadOfficeUnits(
	ctx context.Context,
	bundle *sidecar.BundleHandle,
	entries map[string]sidecar.EntryDescriptor,
	maxBytes int64,
) ([]document.MarkdownUnit, error) {
	remaining := maxBytes
	units := make([]document.MarkdownUnit, 0, len(bundle.Manifest.Units))
	for _, descriptor := range bundle.Manifest.Units {
		entry, ok := entries[descriptor.MarkdownEntry]
		if !ok || entry.ByteSize > remaining {
			return nil, ErrDocumentLimitExceeded
		}
		raw, err := readBoundedBundleEntry(ctx, bundle, descriptor.MarkdownEntry, entry.ByteSize)
		if err != nil {
			return nil, fmt.Errorf("read Office Markdown unit %q: %w", descriptor.ID, err)
		}
		remaining -= int64(len(raw))
		units = append(units, document.MarkdownUnit{
			ID: descriptor.ID, Location: officeLocationFromSidecar(descriptor.Location),
			Markdown: string(raw),
		})
	}
	return units, nil
}

func loadOfficeAssets(
	manifest sidecar.Manifest,
	entries map[string]sidecar.EntryDescriptor,
) ([]document.ExtractedAsset, map[string]string, error) {
	assets := make([]document.ExtractedAsset, 0, len(manifest.Assets))
	localIDs := make(map[string]string, len(manifest.Assets))
	byHash := make(map[string]string, len(manifest.Assets))
	for _, descriptor := range manifest.Assets {
		entry, ok := entries[descriptor.Entry]
		if !ok {
			return nil, nil, fmt.Errorf("%w: Office asset entry %q is missing", ErrInvalidDocument, descriptor.Entry)
		}
		localID, duplicate := byHash[entry.SHA256]
		if !duplicate {
			localID = "asset_office_" + entry.SHA256[:24]
			byHash[entry.SHA256] = localID
			assets = append(assets, document.ExtractedAsset{
				LocalID: localID, ContentSHA256: entry.SHA256, Kind: document.AssetKindImage,
				SourceKind: descriptor.SourceKind, SourceMIME: entry.MIMEType,
				Width: descriptor.Width, Height: descriptor.Height, ByteSize: entry.ByteSize,
				BundleEntry: descriptor.Entry,
			})
		}
		localIDs[descriptor.LocalID] = localID
	}
	return assets, localIDs, nil
}

func loadOfficeAttachments(
	manifest sidecar.Manifest,
	entries map[string]sidecar.EntryDescriptor,
) ([]document.ExtractedAttachment, map[string]string, error) {
	attachments := make([]document.ExtractedAttachment, 0, len(manifest.Attachments))
	localIDs := make(map[string]string, len(manifest.Attachments))
	byHash := make(map[string]string, len(manifest.Attachments))
	for _, descriptor := range manifest.Attachments {
		entry, ok := entries[descriptor.Entry]
		if !ok {
			return nil, nil, fmt.Errorf("%w: Office attachment entry %q is missing", ErrInvalidDocument, descriptor.Entry)
		}
		localID, duplicate := byHash[entry.SHA256]
		if !duplicate {
			localID = "attachment_office_" + entry.SHA256[:24]
			byHash[entry.SHA256] = localID
			attachments = append(attachments, document.ExtractedAttachment{
				LocalID: localID, ContentSHA256: entry.SHA256,
				Kind: descriptor.Kind, FileName: descriptor.FileName, MIMEType: entry.MIMEType,
				ByteSize: entry.ByteSize, BundleEntry: descriptor.Entry,
			})
		}
		localIDs[descriptor.LocalID] = localID
	}
	return attachments, localIDs, nil
}

func loadOfficeOccurrences(
	descriptors []sidecar.OccurrenceDescriptor,
	localIDs map[string]string,
	attachmentLocalIDs map[string]string,
) ([]document.AssetOccurrence, error) {
	occurrences := make([]document.AssetOccurrence, 0, len(descriptors))
	for _, descriptor := range descriptors {
		localID, ok := localIDs[descriptor.AssetLocalID]
		if !ok {
			return nil, fmt.Errorf("%w: Office occurrence %q has no asset", ErrInvalidDocument, descriptor.ID)
		}
		attachmentLocalID := ""
		if descriptor.AttachmentLocalID != "" {
			var attachmentOK bool
			attachmentLocalID, attachmentOK = attachmentLocalIDs[descriptor.AttachmentLocalID]
			if !attachmentOK {
				return nil, fmt.Errorf("%w: Office occurrence %q has no attachment", ErrInvalidDocument, descriptor.ID)
			}
		}
		var bbox *document.NormalizedBBox
		if len(descriptor.BBox) == 4 {
			value := document.NormalizedBBox(descriptor.BBox)
			bbox = &value
		}
		occurrences = append(occurrences, document.AssetOccurrence{
			ID: descriptor.ID, AssetLocalID: localID, AttachmentLocalID: attachmentLocalID,
			UnitID: descriptor.UnitID,
			Order:  descriptor.Order, Location: officeLocationFromSidecar(descriptor.Location),
			BBox: bbox, AltText: descriptor.AltText, Caption: descriptor.Caption,
			OCRText: descriptor.OCRText, Decorative: descriptor.Decorative,
			Confidence: descriptor.Confidence,
		})
	}
	return occurrences, nil
}

func officeLocationFromSidecar(value sidecar.Location) document.SourceLocation {
	return document.SourceLocation{Kind: value.Kind, Index: value.Index, Label: value.Label}
}

func validateOfficeOccurrenceMarkers(units []document.MarkdownUnit, occurrences []document.AssetOccurrence) error {
	expectedUnits := make(map[string]string, len(occurrences))
	counts := make(map[string]int, len(occurrences))
	for _, occurrence := range occurrences {
		expectedUnits[occurrence.ID] = occurrence.UnitID
	}
	markdown := goldmark.New(goldmark.WithExtensions(extension.GFM))
	for _, unit := range units {
		root := markdown.Parser().Parse(text.NewReader([]byte(unit.Markdown)))
		err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
			if !entering {
				return ast.WalkContinue, nil
			}
			image, ok := node.(*ast.Image)
			if !ok {
				return ast.WalkContinue, nil
			}
			destination := strings.TrimSpace(string(image.Destination))
			if !strings.HasPrefix(destination, "rag-asset://") {
				return ast.WalkContinue, nil
			}
			occurrenceID := strings.TrimPrefix(destination, "rag-asset://")
			expectedUnit, exists := expectedUnits[occurrenceID]
			if !exists || expectedUnit != unit.ID {
				return ast.WalkStop, fmt.Errorf("Office Markdown contains an unknown or cross-unit asset marker %q", occurrenceID)
			}
			counts[occurrenceID]++
			return ast.WalkContinue, nil
		})
		if err != nil {
			return err
		}
	}
	for _, occurrence := range occurrences {
		if counts[occurrence.ID] != 1 {
			return fmt.Errorf("Office occurrence %q must have exactly one Markdown marker", occurrence.ID)
		}
	}
	return nil
}

func (p *LocalParser) describeOfficeAssets(
	ctx context.Context,
	bundle *sidecar.BundleHandle,
	format string,
	assets []document.ExtractedAsset,
	occurrences []document.AssetOccurrence,
	options ParseOptions,
	limits officeParseLimits,
) ([]document.AssetOccurrence, []document.ParseWarning, error) {
	warnings := make([]document.ParseWarning, 0)
	occurrencesByAsset := make(map[string][]int, len(assets))
	for index, occurrence := range occurrences {
		occurrencesByAsset[occurrence.AssetLocalID] = append(occurrencesByAsset[occurrence.AssetLocalID], index)
	}
	if err := reportOfficeVisionProgress(ctx, options.Progress, 0, len(assets)); err != nil {
		return nil, nil, err
	}
	for index, asset := range assets {
		indices := occurrencesByAsset[asset.LocalID]
		location, alt := officeAssetContext(occurrences, indices)
		advance := func() error {
			return reportOfficeVisionProgress(ctx, options.Progress, index+1, len(assets))
		}
		if index >= limits.maxVisionAssets {
			warnings = append(warnings, officeVisionWarning("office_vision_asset_limit",
				"Office image used alt text because the document vision-asset limit was reached", location))
			if err := advance(); err != nil {
				return nil, nil, err
			}
			continue
		}
		if options.ImageTranscriber == nil || options.DocumentAIBudget == nil {
			warnings = append(warnings, officeVisionWarning("office_vision_unavailable",
				"Office image used alt text because DocumentAI vision was unavailable", location))
			if err := advance(); err != nil {
				return nil, nil, err
			}
			continue
		}
		raw, err := readBoundedBundleEntry(ctx, bundle, asset.BundleEntry,
			minPositiveInt64(asset.ByteSize, limits.maxAssetBytes))
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			warnings = append(warnings, officeVisionWarning("office_vision_input_invalid",
				"Office image could not be read safely; alt text was retained", location))
			if err := advance(); err != nil {
				return nil, nil, err
			}
			continue
		}
		input, err := vision.NormalizeImage(ctx, raw, asset.SourceMIME, vision.ImageLimits{
			MaxSourceBytes: limits.maxAssetBytes, MaxEncodedBytes: limits.maxVisionInputBytes,
			MaxBase64Bytes: limits.maxVisionInputBytes, MaxPixels: limits.maxImagePixels,
			MaxEdge: limits.visionImageMaxEdge,
		})
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			warnings = append(warnings, officeVisionWarning("office_vision_input_invalid",
				"Office image could not be normalized safely; alt text was retained", location))
			if err := advance(); err != nil {
				return nil, nil, err
			}
			continue
		}
		input.Format, input.Location, input.AltText, input.Scope = format, location, alt, options.VisionScope
		description, err := options.ImageTranscriber.DescribeImage(ctx, input, options.DocumentAIBudget)
		if err == nil {
			err = description.Validate(vision.DefaultSchemaLimits())
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, nil, ctx.Err()
			}
			code := "office_vision_image_failed"
			var typed *vision.Error
			if errors.As(err, &typed) && typed.Kind == vision.ErrorBudget {
				code = "office_vision_budget_exhausted"
			}
			warnings = append(warnings, officeVisionWarning(code,
				"Office image vision failed; alt text was retained", location))
		} else {
			for _, occurrenceIndex := range indices {
				occurrences[occurrenceIndex].Caption = officeDescriptionCaption(
					format, occurrences[occurrenceIndex].AltText, description.Caption,
				)
				occurrences[occurrenceIndex].OCRText = description.OCRText
				occurrences[occurrenceIndex].Decorative = description.Decorative
				occurrences[occurrenceIndex].Confidence = description.Confidence
			}
		}
		if err := advance(); err != nil {
			return nil, nil, err
		}
	}
	return occurrences, warnings, nil
}

func officeWarningsWithoutCode(warnings []document.ParseWarning, code string) []document.ParseWarning {
	filtered := warnings[:0]
	for _, warning := range warnings {
		if warning.Code != code {
			filtered = append(filtered, warning)
		}
	}
	return filtered
}

func officeDescriptionCaption(format, altText, caption string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" || format != "xlsx" {
		return caption
	}
	prefix := officeCellAnchorAltPattern.FindString(strings.TrimSpace(altText))
	if prefix == "" || strings.HasPrefix(caption, prefix) {
		return caption
	}
	return prefix + caption
}

func officeAssetContext(
	occurrences []document.AssetOccurrence,
	indices []int,
) (document.SourceLocation, string) {
	if len(indices) == 0 {
		return document.SourceLocation{Kind: document.LocationDocument}, ""
	}
	location := occurrences[indices[0]].Location
	for _, index := range indices {
		if alt := strings.TrimSpace(occurrences[index].AltText); alt != "" {
			return location, alt
		}
	}
	return location, ""
}

func officeMissingAltWarnings(occurrences []document.AssetOccurrence) []document.ParseWarning {
	warnings := make([]document.ParseWarning, 0)
	for _, occurrence := range occurrences {
		if occurrence.Decorative || strings.TrimSpace(occurrence.Caption) != "" ||
			strings.TrimSpace(occurrence.OCRText) != "" || officeHasMeaningfulAlt(occurrence.AltText) {
			continue
		}
		warnings = append(warnings, officeVisionWarning("office_image_alt_missing",
			"Office image has no alt text; a neutral placeholder was retained", occurrence.Location))
	}
	return warnings
}

func officeHasMeaningfulAlt(value string) bool {
	value = strings.TrimSpace(value)
	value = strings.TrimSpace(officeCellAnchorAltPattern.ReplaceAllString(value, ""))
	return value != "" && value != "图片（未进行视觉识别）"
}

func officeVisionWarning(code, message string, location document.SourceLocation) document.ParseWarning {
	return document.ParseWarning{Code: code, Message: message, Location: &location, Degraded: true}
}

func reportOfficeVisionProgress(ctx context.Context, callback ParseProgressFunc, current, total int) error {
	if callback == nil {
		return nil
	}
	return callback(ctx, ParseProgress{
		Stage: "vision", Current: current, Total: total, Unit: "assets",
		Message: fmt.Sprintf("正在解析 Office 图片 %d/%d", current, total),
	})
}

func officeHasContent(units []document.MarkdownUnit) bool {
	for _, unit := range units {
		if strings.TrimSpace(unit.Markdown) != "" {
			return true
		}
	}
	return false
}
