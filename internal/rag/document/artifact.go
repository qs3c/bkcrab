package document

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

const ParsedArtifactSchemaName = "parsed-artifact-v1"

type ParsedArtifact struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Source        ParsedSource         `json:"source"`
	Parser        ParserInfo           `json:"parser"`
	Units         []MarkdownUnit       `json:"units"`
	Assets        []ArtifactAsset      `json:"assets"`
	Occurrences   []ArtifactOccurrence `json:"occurrences"`
	Warnings      []ParseWarning       `json:"warnings"`
}

type ArtifactAsset struct {
	ID            string `json:"id"`
	ContentSHA256 string `json:"contentSha256"`
	Kind          string `json:"kind"`
	SourceKind    string `json:"sourceKind"`
	SourceMIME    string `json:"sourceMime"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ByteSize      int64  `json:"byteSize"`
	DisplayStatus string `json:"displayStatus"`
}

type ArtifactOccurrence struct {
	ID         string          `json:"id"`
	AssetID    string          `json:"assetId"`
	UnitID     string          `json:"unitId"`
	Order      int             `json:"order"`
	Location   SourceLocation  `json:"location"`
	BBox       *NormalizedBBox `json:"bbox,omitempty"`
	Caption    string          `json:"caption"`
	OCRText    string          `json:"ocrText,omitempty"`
	Decorative bool            `json:"decorative"`
	Confidence float64         `json:"confidence"`
}

// AssetRef is the public, URL-free resource descriptor consumed by later
// splitter/search stages. Object keys remain in the SQL catalog only.
type AssetRef struct {
	ID       string         `json:"id"`
	Kind     string         `json:"kind"`
	Caption  string         `json:"caption,omitempty"`
	PageNum  int            `json:"pageNum,omitempty"`
	Location SourceLocation `json:"location"`
	Width    int            `json:"width,omitempty"`
	Height   int            `json:"height,omitempty"`
	MIMEType string         `json:"mimeType,omitempty"`
}

func (a *ParsedArtifact) Validate() error {
	if a == nil {
		return errors.New("parsed artifact is nil")
	}
	if a.SchemaVersion != ParsedArtifactSchemaVersion {
		return fmt.Errorf("unsupported parsed artifact schema version %d", a.SchemaVersion)
	}
	if err := a.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := a.Parser.Validate(); err != nil {
		return fmt.Errorf("parser: %w", err)
	}
	units, err := validateUnits(a.Units)
	if err != nil {
		return err
	}
	assets := make(map[string]ArtifactAsset, len(a.Assets))
	hashes := make(map[string]string, len(a.Assets))
	for i, asset := range a.Assets {
		if err := validateArtifactAsset(a.Source.DocID, asset); err != nil {
			return fmt.Errorf("asset %d: %w", i, err)
		}
		if _, exists := assets[asset.ID]; exists {
			return fmt.Errorf("duplicate canonical asset ID %q", asset.ID)
		}
		if prior, exists := hashes[asset.ContentSHA256]; exists {
			return fmt.Errorf("duplicate asset content hash %q in %q and %q", asset.ContentSHA256, prior, asset.ID)
		}
		assets[asset.ID] = asset
		hashes[asset.ContentSHA256] = asset.ID
	}
	occurrenceIDs := make(map[string]struct{}, len(a.Occurrences))
	referencedAssets := make(map[string]struct{}, len(a.Assets))
	for i, occurrence := range a.Occurrences {
		if err := validateArtifactOccurrence(occurrence, units, assets); err != nil {
			return fmt.Errorf("occurrence %d: %w", i, err)
		}
		if _, exists := occurrenceIDs[occurrence.ID]; exists {
			return fmt.Errorf("duplicate occurrence ID %q", occurrence.ID)
		}
		occurrenceIDs[occurrence.ID] = struct{}{}
		referencedAssets[occurrence.AssetID] = struct{}{}
	}
	for assetID := range assets {
		if _, ok := referencedAssets[assetID]; !ok {
			return fmt.Errorf("asset %q has no occurrence", assetID)
		}
	}
	for _, unit := range a.Units {
		if err := ValidateMarkdownAssetMarkers(unit.Markdown, occurrenceIDs); err != nil {
			return fmt.Errorf("unit %q: %w", unit.ID, err)
		}
	}
	for i, warning := range a.Warnings {
		if !safeID(warning.Code) || !validText(warning.Message) {
			return fmt.Errorf("warning %d is invalid", i)
		}
		if warning.Location != nil {
			if err := warning.Location.Validate(); err != nil {
				return fmt.Errorf("warning %d location: %w", i, err)
			}
		}
	}
	return nil
}

func validateArtifactAsset(docID string, asset ArtifactAsset) error {
	expected, err := AssetID(docID, asset.ContentSHA256)
	if err != nil {
		return err
	}
	if asset.ID != expected {
		return fmt.Errorf("asset ID %q is not canonical for document and content hash", asset.ID)
	}
	if asset.Kind != AssetKindImage {
		return fmt.Errorf("unsupported asset kind %q", asset.Kind)
	}
	switch asset.SourceKind {
	case SourceKindEmbeddedOriginal, SourceKindPageCrop, SourceKindScannedPage:
	default:
		return fmt.Errorf("unsupported source kind %q", asset.SourceKind)
	}
	if !validMIME(asset.SourceMIME) || asset.Width < 1 || asset.Height < 1 || asset.ByteSize < 1 {
		return errors.New("invalid asset source description")
	}
	if asset.DisplayStatus != DisplayReady && asset.DisplayStatus != DisplayUnavailable {
		return fmt.Errorf("invalid display status %q", asset.DisplayStatus)
	}
	return nil
}

func validateArtifactOccurrence(occurrence ArtifactOccurrence, units map[string]MarkdownUnit, assets map[string]ArtifactAsset) error {
	if !safeID(occurrence.ID) {
		return fmt.Errorf("invalid ID %q", occurrence.ID)
	}
	if _, ok := assets[occurrence.AssetID]; !ok {
		return fmt.Errorf("references unknown asset %q", occurrence.AssetID)
	}
	unit, ok := units[occurrence.UnitID]
	if !ok {
		return fmt.Errorf("references unknown unit %q", occurrence.UnitID)
	}
	if occurrence.Order < 0 {
		return errors.New("order cannot be negative")
	}
	if err := occurrence.Location.Validate(); err != nil {
		return err
	}
	if occurrence.Location != unit.Location {
		return errors.New("location does not match referenced unit")
	}
	if occurrence.BBox != nil {
		if err := occurrence.BBox.Validate(); err != nil {
			return err
		}
	}
	if occurrence.Confidence < 0 || occurrence.Confidence > 1 {
		return errors.New("confidence must be between 0 and 1")
	}
	if !validText(occurrence.Caption) || !validText(occurrence.OCRText) {
		return errors.New("occurrence text must be valid UTF-8")
	}
	return nil
}

// Canonicalize maps every parser-local asset ID to the stable ID derived from
// docID+content hash and folds transient caption/alt fallback into one field.
func Canonicalize(parsed *ParsedDocument, neutralCaption string) (*ParsedArtifact, error) {
	if err := parsed.Validate(); err != nil {
		return nil, err
	}
	localToCanonical := make(map[string]string, len(parsed.Assets))
	assets := make([]ArtifactAsset, 0, len(parsed.Assets))
	for _, transient := range parsed.Assets {
		id, err := AssetID(parsed.Source.DocID, transient.ContentSHA256)
		if err != nil {
			return nil, err
		}
		localToCanonical[transient.LocalID] = id
		assets = append(assets, ArtifactAsset{
			ID: id, ContentSHA256: transient.ContentSHA256, Kind: transient.Kind,
			SourceKind: transient.SourceKind, SourceMIME: transient.SourceMIME,
			Width: transient.Width, Height: transient.Height, ByteSize: transient.ByteSize,
			DisplayStatus: DisplayUnavailable,
		})
	}
	occurrences := make([]ArtifactOccurrence, 0, len(parsed.Occurrences))
	for _, transient := range parsed.Occurrences {
		var bbox *NormalizedBBox
		if transient.BBox != nil {
			value := *transient.BBox
			bbox = &value
		}
		occurrences = append(occurrences, ArtifactOccurrence{
			ID: transient.ID, AssetID: localToCanonical[transient.AssetLocalID],
			UnitID: transient.UnitID, Order: transient.Order, Location: transient.Location,
			BBox: bbox, Caption: FinalCaption(transient.Caption, transient.AltText, neutralCaption),
			OCRText: cleanPlainText(transient.OCRText, 16*1024), Decorative: transient.Decorative,
			Confidence: transient.Confidence,
		})
	}
	units := append([]MarkdownUnit(nil), parsed.Units...)
	warnings := make([]ParseWarning, len(parsed.Warnings))
	for i, warning := range parsed.Warnings {
		warnings[i] = warning
		if warning.Location != nil {
			location := *warning.Location
			warnings[i].Location = &location
		}
	}
	artifact := &ParsedArtifact{
		SchemaVersion: ParsedArtifactSchemaVersion,
		Source:        parsed.Source, Parser: parsed.Parser, Units: units,
		Assets: assets, Occurrences: occurrences, Warnings: warnings,
	}
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	return artifact, nil
}

func EncodeArtifact(artifact *ParsedArtifact, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, errors.New("artifact byte limit must be positive")
	}
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxBytes {
		return nil, fmt.Errorf("parsed artifact is %d bytes, limit is %d", len(encoded), maxBytes)
	}
	return encoded, nil
}

func DecodeArtifact(reader io.Reader, maxBytes int64) (*ParsedArtifact, error) {
	if reader == nil || maxBytes <= 0 {
		return nil, errors.New("artifact reader and positive byte limit are required")
	}
	encoded, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > maxBytes {
		return nil, fmt.Errorf("parsed artifact exceeds %d byte limit", maxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var artifact ParsedArtifact
	if err := decoder.Decode(&artifact); err != nil {
		return nil, fmt.Errorf("decode parsed artifact: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("parsed artifact contains a second JSON value")
		}
		return nil, fmt.Errorf("decode trailing artifact data: %w", err)
	}
	if err := artifact.Validate(); err != nil {
		return nil, fmt.Errorf("validate parsed artifact: %w", err)
	}
	return &artifact, nil
}

func (a *ParsedArtifact) NormalizedMarkdown() string {
	if a == nil || len(a.Units) == 0 {
		return ""
	}
	var builder strings.Builder
	for i, unit := range a.Units {
		if i > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(unit.Markdown)
	}
	return builder.String()
}

type ObjectKeys struct {
	ArtifactJSON       string
	NormalizedMarkdown string
	AssetSource        string
	AssetDisplay       string
	AssetThumbnail     string
}

func ArtifactJSONKey(userID, kbID, docID, parseFingerprint string) (string, error) {
	prefix, err := documentPrefix(userID, kbID, docID)
	if err != nil {
		return "", err
	}
	if !CanonicalSHA256(parseFingerprint) {
		return "", errors.New("parse fingerprint must be a canonical SHA-256")
	}
	return path.Join(prefix, "artifacts", parseFingerprint, "parsed.json"), nil
}

func NormalizedMarkdownKey(userID, kbID, docID, parseFingerprint string) (string, error) {
	prefix, err := documentPrefix(userID, kbID, docID)
	if err != nil {
		return "", err
	}
	if !CanonicalSHA256(parseFingerprint) {
		return "", errors.New("parse fingerprint must be a canonical SHA-256")
	}
	return path.Join(prefix, "artifacts", parseFingerprint, "normalized.md"), nil
}

func AssetSourceKey(userID, kbID, docID, contentSHA256, sourceMIME string) (string, error) {
	prefix, err := documentPrefix(userID, kbID, docID)
	if err != nil {
		return "", err
	}
	if !CanonicalSHA256(contentSHA256) {
		return "", errors.New("asset content hash must be a canonical SHA-256")
	}
	extension, err := sourceExtension(sourceMIME)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, "assets", contentSHA256, "source."+extension), nil
}

func AssetDisplayKey(userID, kbID, docID, contentSHA256 string) (string, error) {
	prefix, err := assetPrefix(userID, kbID, docID, contentSHA256)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, "display.webp"), nil
}

func AssetThumbnailKey(userID, kbID, docID, contentSHA256 string) (string, error) {
	prefix, err := assetPrefix(userID, kbID, docID, contentSHA256)
	if err != nil {
		return "", err
	}
	return path.Join(prefix, "thumbnail.webp"), nil
}

func NewObjectKeys(userID, kbID, docID, contentSHA256, sourceMIME, parseFingerprint string) (ObjectKeys, error) {
	artifactJSON, err := ArtifactJSONKey(userID, kbID, docID, parseFingerprint)
	if err != nil {
		return ObjectKeys{}, err
	}
	normalized, err := NormalizedMarkdownKey(userID, kbID, docID, parseFingerprint)
	if err != nil {
		return ObjectKeys{}, err
	}
	source, err := AssetSourceKey(userID, kbID, docID, contentSHA256, sourceMIME)
	if err != nil {
		return ObjectKeys{}, err
	}
	display, err := AssetDisplayKey(userID, kbID, docID, contentSHA256)
	if err != nil {
		return ObjectKeys{}, err
	}
	thumbnail, err := AssetThumbnailKey(userID, kbID, docID, contentSHA256)
	if err != nil {
		return ObjectKeys{}, err
	}
	return ObjectKeys{ArtifactJSON: artifactJSON, NormalizedMarkdown: normalized, AssetSource: source, AssetDisplay: display, AssetThumbnail: thumbnail}, nil
}

func PageCacheObjectKey(userID, kbID, docID, cacheKey string) (string, error) {
	return cacheObjectKey(userID, kbID, docID, "pages", cacheKey)
}

func ImageCacheObjectKey(userID, kbID, docID, cacheKey string) (string, error) {
	return cacheObjectKey(userID, kbID, docID, "images", cacheKey)
}

func EnrichmentCacheObjectKey(userID, kbID, docID, cacheKey string) (string, error) {
	return cacheObjectKey(userID, kbID, docID, "enrich", cacheKey)
}

func cacheObjectKey(userID, kbID, docID, kind, cacheKey string) (string, error) {
	prefix, err := documentPrefix(userID, kbID, docID)
	if err != nil {
		return "", err
	}
	if !CanonicalSHA256(cacheKey) {
		return "", errors.New("cache key must be a canonical SHA-256")
	}
	return path.Join(prefix, "cache", kind, cacheKey+".json"), nil
}

func documentPrefix(userID, kbID, docID string) (string, error) {
	for name, value := range map[string]string{"userID": userID, "kbID": kbID, "docID": docID} {
		if !safeID(value) {
			return "", fmt.Errorf("invalid %s path segment %q", name, value)
		}
	}
	return path.Join("rag", userID, kbID, docID), nil
}

func assetPrefix(userID, kbID, docID, contentSHA256 string) (string, error) {
	prefix, err := documentPrefix(userID, kbID, docID)
	if err != nil {
		return "", err
	}
	if !CanonicalSHA256(contentSHA256) {
		return "", errors.New("asset content hash must be a canonical SHA-256")
	}
	return path.Join(prefix, "assets", contentSHA256), nil
}

func sourceExtension(mimeType string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mimeType, ";")[0])) {
	case "image/png":
		return "png", nil
	case "image/jpeg":
		return "jpg", nil
	case "image/webp":
		return "webp", nil
	case "image/gif":
		return "gif", nil
	case "image/tiff":
		return "tiff", nil
	case "image/bmp":
		return "bmp", nil
	case "image/svg+xml":
		return "svg", nil
	case "image/emf", "image/x-emf":
		return "emf", nil
	case "image/wmf", "image/x-wmf":
		return "wmf", nil
	default:
		return "", fmt.Errorf("unsupported asset source MIME %q", mimeType)
	}
}

type ParseFingerprintInput struct {
	SourceSHA256              string `json:"sourceSha256"`
	ParseMode                 string `json:"parseMode"`
	ArtifactSchemaVersion     string `json:"artifactSchemaVersion"`
	ParserVersion             string `json:"parserVersion"`
	MarkItDownVersion         string `json:"markItDownVersion"`
	PDFRenderDPI              int    `json:"pdfRenderDpi"`
	PDFRoutingVersion         string `json:"pdfRoutingVersion"`
	MaxPages                  int    `json:"maxPages"`
	MaxVisionPages            int    `json:"maxVisionPages"`
	MaxVisionAssets           int    `json:"maxVisionAssets"`
	MaxAssets                 int    `json:"maxAssets"`
	MaxAssetBytes             int64  `json:"maxAssetBytes"`
	MaxExtractedBytes         int64  `json:"maxExtractedBytes"`
	MaxVisionInputBytes       int64  `json:"maxVisionInputBytes"`
	MaxImagePixels            int64  `json:"maxImagePixels"`
	DisplayMaxEdge            int    `json:"displayMaxEdge"`
	ThumbnailMaxEdge          int    `json:"thumbnailMaxEdge"`
	VisionProviderFingerprint string `json:"visionProviderFingerprint"`
	VisionModel               string `json:"visionModel"`
	VisionPromptVersion       string `json:"visionPromptVersion"`
	PageSchemaVersion         string `json:"pageSchemaVersion"`
	ImageSchemaVersion        string `json:"imageSchemaVersion"`
}

func ParseFingerprint(input ParseFingerprintInput) (string, error) {
	if !CanonicalSHA256(input.SourceSHA256) {
		return "", errors.New("source SHA-256 must be canonical")
	}
	if input.ParseMode != "standard" && input.ParseMode != "auto" {
		return "", fmt.Errorf("invalid parse mode %q", input.ParseMode)
	}
	if !validContractString(input.ParserVersion, 128) ||
		!validContractString(input.MarkItDownVersion, 128) ||
		!validContractString(input.PDFRoutingVersion, 128) ||
		!validContractString(input.PageSchemaVersion, 128) ||
		!validContractString(input.ImageSchemaVersion, 128) ||
		input.PDFRenderDPI < 1 || input.MaxPages < 1 || input.MaxVisionPages < 1 ||
		input.MaxVisionAssets < 1 || input.MaxAssets < 1 || input.MaxAssetBytes < 1 ||
		input.MaxExtractedBytes < 1 || input.MaxVisionInputBytes < 1 || input.MaxImagePixels < 1 ||
		input.DisplayMaxEdge < 1 || input.ThumbnailMaxEdge < 1 {
		return "", errors.New("complete parser versions and positive parse/render limits are required")
	}
	if input.VisionProviderFingerprint != "" && !CanonicalSHA256(input.VisionProviderFingerprint) {
		return "", errors.New("vision provider fingerprint must be canonical")
	}
	input.ArtifactSchemaVersion = ParsedArtifactSchemaName
	return hashJSON(input), nil
}

func PageCacheKey(pageRender []byte, providerFingerprint, model, promptVersion, schemaVersion string) string {
	return framedHash(pageRender, []byte(providerFingerprint), []byte(model), []byte(promptVersion), []byte(schemaVersion))
}

func ImageDescriptionCacheKey(image []byte, providerFingerprint, model, promptVersion, schemaVersion string) string {
	return framedHash(image, []byte(providerFingerprint), []byte(model), []byte(promptVersion), []byte(schemaVersion))
}

func EnrichmentCacheKey(rawBlock, blockKind, providerFingerprint, model, promptVersion, schemaVersion string) string {
	return framedHash([]byte(rawBlock), []byte(blockKind), []byte(providerFingerprint), []byte(model), []byte(promptVersion), []byte(schemaVersion))
}

func hashJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal fixed document fingerprint contract: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func framedHash(parts ...[]byte) string {
	hash := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(part)
	}
	return hex.EncodeToString(hash.Sum(nil))
}
