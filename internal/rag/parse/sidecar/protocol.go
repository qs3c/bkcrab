package sidecar

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolVersion   = "rag-parser/v1"
	ManifestEntryName = "manifest.json"

	MIMETypeMarkdown = "text/markdown; charset=utf-8"
	MIMETypeJSON     = "application/json"
)

type BundleKind string

const (
	BundleKindOfficeConvert BundleKind = "office-convert"
	BundleKindPDFAnalyze    BundleKind = "pdf-analyze"
	BundleKindPDFRender     BundleKind = "pdf-render"
)

type PageStatus string

const (
	PageStatusOK     PageStatus = "ok"
	PageStatusFailed PageStatus = "failed"
)

type SourceDescriptor struct {
	Format   string `json:"format"`
	ByteSize int64  `json:"byteSize"`
	SHA256   string `json:"sha256"`
}

type ParserDescriptor struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	WrapperVersion string `json:"wrapperVersion"`
}

type EntryDescriptor struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	ByteSize int64  `json:"byteSize"`
	MIMEType string `json:"mimeType"`
}

type Location struct {
	Kind  string `json:"kind"`
	Index int    `json:"index"`
	Label string `json:"label"`
}

type UnitDescriptor struct {
	ID            string   `json:"id"`
	Location      Location `json:"location"`
	MarkdownEntry string   `json:"markdownEntry"`
}

type AssetDescriptor struct {
	LocalID    string `json:"localId"`
	Entry      string `json:"entry"`
	Kind       string `json:"kind"`
	SourceKind string `json:"sourceKind"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

type OccurrenceDescriptor struct {
	ID           string   `json:"id"`
	AssetLocalID string   `json:"assetLocalId"`
	UnitID       string   `json:"unitId"`
	Order        int      `json:"order"`
	Location     Location `json:"location"`
	BBox         []int    `json:"bbox"`
	AltText      string   `json:"altText"`
	Caption      string   `json:"caption"`
	OCRText      string   `json:"ocrText"`
	Decorative   bool     `json:"decorative"`
	Confidence   float64  `json:"confidence"`
}

type PageDescriptor struct {
	Page                int        `json:"page"`
	Status              PageStatus `json:"status"`
	ErrorCode           string     `json:"errorCode"`
	UnitID              string     `json:"unitId"`
	NativeMarkdownEntry string     `json:"nativeMarkdownEntry"`
	RenderEntry         string     `json:"renderEntry"`
	PrimitiveEntry      string     `json:"primitiveEntry"`
}

type WarningDescriptor struct {
	Code     string    `json:"code"`
	Message  string    `json:"message"`
	Location *Location `json:"location"`
	Degraded bool      `json:"degraded"`
}

// Manifest is the complete rag-parser/v1 manifest. Slice fields are required
// in canonical JSON even when empty; nil is rejected so missing/null fields do
// not silently acquire a new meaning.
type Manifest struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	BundleKind      BundleKind             `json:"bundleKind"`
	Source          SourceDescriptor       `json:"source"`
	Parser          ParserDescriptor       `json:"parser"`
	Entries         []EntryDescriptor      `json:"entries"`
	Units           []UnitDescriptor       `json:"units"`
	Assets          []AssetDescriptor      `json:"assets"`
	Occurrences     []OccurrenceDescriptor `json:"occurrences"`
	Pages           []PageDescriptor       `json:"pages"`
	Warnings        []WarningDescriptor    `json:"warnings"`
}

type HealthLimits struct {
	MaxInputBytes  int64 `json:"maxInputBytes"`
	MaxOutputBytes int64 `json:"maxOutputBytes"`
}

type OfficeCapability struct {
	Enabled           bool     `json:"enabled"`
	Formats           []string `json:"formats"`
	MarkItDownVersion string   `json:"markitdownVersion"`
	WrapperVersion    string   `json:"wrapperVersion"`
}

type PDFCapability struct {
	Enabled       bool   `json:"enabled"`
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion"`
}

type HealthCapabilities struct {
	Office OfficeCapability `json:"office"`
	PDF    PDFCapability    `json:"pdf"`
}

type Health struct {
	ProtocolVersion string             `json:"protocolVersion"`
	ServiceVersion  string             `json:"serviceVersion"`
	Limits          HealthLimits       `json:"limits"`
	Capabilities    HealthCapabilities `json:"capabilities"`
}

type PrimitiveTextBlock struct {
	Text string `json:"text"`
	BBox []int  `json:"bbox"`
}

type PrimitiveEmbeddedImage struct {
	BBox []int `json:"bbox"`
}

type PrimitiveSignals struct {
	Table                 bool `json:"table"`
	Code                  bool `json:"code"`
	Scanned               bool `json:"scanned"`
	Multicolumn           bool `json:"multicolumn"`
	ReadingOrderUncertain bool `json:"readingOrderUncertain"`
}

type PagePrimitive struct {
	Page           int                      `json:"page"`
	Width          float64                  `json:"width"`
	Height         float64                  `json:"height"`
	TextChars      int                      `json:"textChars"`
	BlockCount     int                      `json:"blockCount"`
	TextCoverage   float64                  `json:"textCoverage"`
	TextBlocks     []PrimitiveTextBlock     `json:"textBlocks"`
	EmbeddedImages []PrimitiveEmbeddedImage `json:"embeddedImages"`
	Signals        PrimitiveSignals         `json:"signals"`
}

type DecodeLimits struct {
	MaxManifestBytes int64
	MaxEntries       int
	MaxPages         int
	MaxAssets        int
	MaxImagePixels   int64
	MaxEntryBytes    int64
	MaxAssetBytes    int64
	MaxRenderBytes   int64
	MaxTotalBytes    int64
	MaxArchiveBytes  int64
}

type DecodeOptions struct {
	ExpectedKind      BundleKind
	ExpectedSource    SourceDescriptor
	RequestedPages    []int
	ExpectedPageUnits map[int]string
	AllowedPageErrors map[string]struct{}
	Limits            DecodeLimits
	TempDir           string
}

var defaultPageErrorCodes = map[string]struct{}{
	"engine_error":        {},
	"invalid_page":        {},
	"page_analyze_failed": {},
	"page_limit_exceeded": {},
	"page_render_failed":  {},
	"timeout":             {},
}

var identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func requireExactObjectFields(data []byte, fields ...string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := start.(json.Delim)
	if !ok || delimiter != '{' {
		return errors.New("JSON value must be an object")
	}
	seen := make(map[string]struct{}, len(fields))
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		name, ok := token.(string)
		if !ok {
			return errors.New("JSON object key must be a string")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("duplicate JSON field %q", name)
		}
		seen[name] = struct{}{}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	if len(seen) != len(fields) {
		return fmt.Errorf("JSON object field count=%d, want %d", len(seen), len(fields))
	}
	for _, field := range fields {
		if _, ok := seen[field]; !ok {
			return fmt.Errorf("missing JSON field %q", field)
		}
	}
	return nil
}

func unmarshalExact(data []byte, target any, fields ...string) error {
	if err := requireExactObjectFields(data, fields...); err != nil {
		return err
	}
	return decodeStrict(data, target)
}

func (value *SourceDescriptor) UnmarshalJSON(data []byte) error {
	type wire SourceDescriptor
	return unmarshalExact(data, (*wire)(value), "format", "byteSize", "sha256")
}

func (value *ParserDescriptor) UnmarshalJSON(data []byte) error {
	type wire ParserDescriptor
	return unmarshalExact(data, (*wire)(value), "name", "version", "wrapperVersion")
}

func (value *EntryDescriptor) UnmarshalJSON(data []byte) error {
	type wire EntryDescriptor
	return unmarshalExact(data, (*wire)(value), "path", "sha256", "byteSize", "mimeType")
}

func (value *Location) UnmarshalJSON(data []byte) error {
	type wire Location
	return unmarshalExact(data, (*wire)(value), "kind", "index", "label")
}

func (value *UnitDescriptor) UnmarshalJSON(data []byte) error {
	type wire UnitDescriptor
	return unmarshalExact(data, (*wire)(value), "id", "location", "markdownEntry")
}

func (value *AssetDescriptor) UnmarshalJSON(data []byte) error {
	type wire AssetDescriptor
	return unmarshalExact(data, (*wire)(value), "localId", "entry", "kind", "sourceKind", "width", "height")
}

func (value *OccurrenceDescriptor) UnmarshalJSON(data []byte) error {
	type wire OccurrenceDescriptor
	return unmarshalExact(data, (*wire)(value), "id", "assetLocalId", "unitId", "order", "location", "bbox", "altText", "caption", "ocrText", "decorative", "confidence")
}

func (value *PageDescriptor) UnmarshalJSON(data []byte) error {
	type wire PageDescriptor
	return unmarshalExact(data, (*wire)(value), "page", "status", "errorCode", "unitId", "nativeMarkdownEntry", "renderEntry", "primitiveEntry")
}

func (value *WarningDescriptor) UnmarshalJSON(data []byte) error {
	type wire WarningDescriptor
	return unmarshalExact(data, (*wire)(value), "code", "message", "location", "degraded")
}

func (value *Manifest) UnmarshalJSON(data []byte) error {
	type wire Manifest
	return unmarshalExact(data, (*wire)(value), "protocolVersion", "bundleKind", "source", "parser", "entries", "units", "assets", "occurrences", "pages", "warnings")
}

func (value *HealthLimits) UnmarshalJSON(data []byte) error {
	type wire HealthLimits
	return unmarshalExact(data, (*wire)(value), "maxInputBytes", "maxOutputBytes")
}

func (value *OfficeCapability) UnmarshalJSON(data []byte) error {
	type wire OfficeCapability
	return unmarshalExact(data, (*wire)(value), "enabled", "formats", "markitdownVersion", "wrapperVersion")
}

func (value *PDFCapability) UnmarshalJSON(data []byte) error {
	type wire PDFCapability
	return unmarshalExact(data, (*wire)(value), "enabled", "engine", "engineVersion")
}

func (value *HealthCapabilities) UnmarshalJSON(data []byte) error {
	type wire HealthCapabilities
	return unmarshalExact(data, (*wire)(value), "office", "pdf")
}

func (value *Health) UnmarshalJSON(data []byte) error {
	type wire Health
	return unmarshalExact(data, (*wire)(value), "protocolVersion", "serviceVersion", "limits", "capabilities")
}

func (value *PrimitiveTextBlock) UnmarshalJSON(data []byte) error {
	type wire PrimitiveTextBlock
	return unmarshalExact(data, (*wire)(value), "text", "bbox")
}

func (value *PrimitiveEmbeddedImage) UnmarshalJSON(data []byte) error {
	type wire PrimitiveEmbeddedImage
	return unmarshalExact(data, (*wire)(value), "bbox")
}

func (value *PrimitiveSignals) UnmarshalJSON(data []byte) error {
	type wire PrimitiveSignals
	return unmarshalExact(data, (*wire)(value), "table", "code", "scanned", "multicolumn", "readingOrderUncertain")
}

func (value *PagePrimitive) UnmarshalJSON(data []byte) error {
	type wire PagePrimitive
	return unmarshalExact(data, (*wire)(value), "page", "width", "height", "textChars", "blockCount", "textCoverage", "textBlocks", "embeddedImages", "signals")
}

func decodeStrict(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

type strictUTF8Reader struct {
	reader io.Reader
	tail   []byte
}

func (r *strictUTF8Reader) Read(buffer []byte) (int, error) {
	read, readErr := r.reader.Read(buffer)
	if read == 0 && readErr == nil {
		return 0, nil
	}
	combined := make([]byte, 0, len(r.tail)+read)
	combined = append(combined, r.tail...)
	combined = append(combined, buffer[:read]...)
	r.tail = r.tail[:0]
	for len(combined) > 0 {
		if !utf8.FullRune(combined) {
			r.tail = append(r.tail, combined...)
			break
		}
		runeValue, size := utf8.DecodeRune(combined)
		if runeValue == utf8.RuneError && size == 1 {
			return 0, errors.New("JSON is not valid UTF-8")
		}
		combined = combined[size:]
	}
	if errors.Is(readErr, io.EOF) && len(r.tail) != 0 {
		return 0, errors.New("JSON ends with incomplete UTF-8")
	}
	return read, readErr
}

func validSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}

func validIdentifier(value string) bool { return identifierPattern.MatchString(value) }

func validBundlePath(value string) bool {
	if value == "" || strings.ContainsRune(value, 0) || strings.Contains(value, "\\") ||
		strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return false
	}
	if filepath.VolumeName(filepath.FromSlash(value)) != "" {
		return false
	}
	segments := strings.Split(value, "/")
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	cleaned := path.Clean(value)
	return cleaned == value && cleaned != ManifestEntryName
}

func validLocation(location Location) bool {
	switch location.Kind {
	case "document":
		return location.Index == 0
	case "page", "slide", "sheet":
		return location.Index > 0
	default:
		return false
	}
}

func sameLocation(left, right Location) bool {
	return left.Kind == right.Kind && left.Index == right.Index && left.Label == right.Label
}

func validBBox(bbox []int) bool {
	if bbox == nil {
		return true
	}
	if len(bbox) != 4 {
		return false
	}
	for _, coordinate := range bbox {
		if coordinate < 0 || coordinate > 1000 {
			return false
		}
	}
	return bbox[0] < bbox[2] && bbox[1] < bbox[3]
}

func expectedOfficeUnitID(location Location) (string, bool) {
	switch location.Kind {
	case "document":
		return "unit_document_0000", location.Index == 0
	case "slide":
		return fmt.Sprintf("unit_slide_%04d", location.Index), location.Index > 0
	case "sheet":
		return fmt.Sprintf("unit_sheet_%04d", location.Index), location.Index > 0
	default:
		return "", false
	}
}

func expectedMIMEForPath(entryPath string) (string, bool) {
	switch strings.ToLower(path.Ext(entryPath)) {
	case ".md":
		return MIMETypeMarkdown, true
	case ".json":
		return MIMETypeJSON, true
	case ".png":
		return "image/png", true
	case ".jpg", ".jpeg":
		return "image/jpeg", true
	case ".webp":
		return "image/webp", true
	default:
		return "", false
	}
}

func validateMIME(entry EntryDescriptor) bool {
	expected, ok := expectedMIMEForPath(entry.Path)
	if !ok || entry.MIMEType != expected {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(entry.MIMEType)
	if err != nil || mediaType == "" {
		return false
	}
	if mediaType == "text/markdown" {
		return strings.EqualFold(params["charset"], "utf-8") && len(params) == 1
	}
	return len(params) == 0
}

func invalidBundle(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidBundle, fmt.Sprintf(format, args...))
}

func limitExceeded(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrBundleLimitExceeded, fmt.Sprintf(format, args...))
}

// ValidateManifest checks all manifest-only invariants before the decoder
// creates a temporary directory or trusts any tar payload path.
func ValidateManifest(manifest *Manifest, options DecodeOptions) error {
	if manifest == nil {
		return invalidBundle("manifest is nil")
	}
	if manifest.ProtocolVersion != ProtocolVersion {
		return invalidBundle("unsupported protocolVersion %q", manifest.ProtocolVersion)
	}
	switch manifest.BundleKind {
	case BundleKindOfficeConvert, BundleKindPDFAnalyze, BundleKindPDFRender:
	default:
		return invalidBundle("unsupported bundleKind %q", manifest.BundleKind)
	}
	if options.ExpectedKind != "" && manifest.BundleKind != options.ExpectedKind {
		return invalidBundle("bundleKind=%q, expected %q", manifest.BundleKind, options.ExpectedKind)
	}
	if manifest.Entries == nil || manifest.Units == nil || manifest.Assets == nil ||
		manifest.Occurrences == nil || manifest.Pages == nil || manifest.Warnings == nil {
		return invalidBundle("canonical array field is missing or null")
	}
	if options.Limits.MaxEntries > 0 && len(manifest.Entries) > options.Limits.MaxEntries {
		return limitExceeded("entry count %d exceeds %d", len(manifest.Entries), options.Limits.MaxEntries)
	}
	if options.Limits.MaxPages > 0 && (len(manifest.Pages) > options.Limits.MaxPages || len(manifest.Units) > options.Limits.MaxPages) {
		return limitExceeded("document unit/page count exceeds %d", options.Limits.MaxPages)
	}
	if options.Limits.MaxAssets > 0 && len(manifest.Assets) > options.Limits.MaxAssets {
		return limitExceeded("asset count %d exceeds %d", len(manifest.Assets), options.Limits.MaxAssets)
	}
	if manifest.Source.ByteSize <= 0 || !validSHA256(manifest.Source.SHA256) {
		return invalidBundle("invalid source descriptor")
	}
	if options.ExpectedSource.Format != "" && manifest.Source.Format != options.ExpectedSource.Format {
		return invalidBundle("source format=%q, expected %q", manifest.Source.Format, options.ExpectedSource.Format)
	}
	if options.ExpectedSource.ByteSize > 0 && manifest.Source.ByteSize != options.ExpectedSource.ByteSize {
		return invalidBundle("source byteSize=%d, expected %d", manifest.Source.ByteSize, options.ExpectedSource.ByteSize)
	}
	if options.ExpectedSource.SHA256 != "" && manifest.Source.SHA256 != options.ExpectedSource.SHA256 {
		return invalidBundle("source sha256 does not match request")
	}
	if strings.TrimSpace(manifest.Parser.Name) == "" || strings.TrimSpace(manifest.Parser.Version) == "" ||
		strings.TrimSpace(manifest.Parser.WrapperVersion) == "" {
		return invalidBundle("incomplete parser descriptor")
	}

	entryByPath := make(map[string]EntryDescriptor, len(manifest.Entries))
	referenceCount := make(map[string]int, len(manifest.Entries))
	lastPath := ""
	for index, entry := range manifest.Entries {
		if !validBundlePath(entry.Path) || !validSHA256(entry.SHA256) || entry.ByteSize < 0 || !validateMIME(entry) {
			return invalidBundle("invalid entries[%d]", index)
		}
		if index > 0 && entry.Path <= lastPath {
			return invalidBundle("entries must be strictly path-sorted")
		}
		if options.Limits.MaxEntryBytes > 0 && entry.ByteSize > options.Limits.MaxEntryBytes {
			return limitExceeded("entry %q size %d exceeds %d", entry.Path, entry.ByteSize, options.Limits.MaxEntryBytes)
		}
		entryByPath[entry.Path] = entry
		lastPath = entry.Path
	}
	addReference := func(entryPath string) error {
		if entryPath == "" {
			return nil
		}
		if _, ok := entryByPath[entryPath]; !ok {
			return invalidBundle("reference to undeclared entry %q", entryPath)
		}
		referenceCount[entryPath]++
		return nil
	}

	unitByID := make(map[string]UnitDescriptor, len(manifest.Units))
	for index, unit := range manifest.Units {
		if !validIdentifier(unit.ID) || !validLocation(unit.Location) || unit.MarkdownEntry == "" {
			return invalidBundle("invalid units[%d]", index)
		}
		if _, duplicate := unitByID[unit.ID]; duplicate {
			return invalidBundle("duplicate unit id %q", unit.ID)
		}
		if manifest.BundleKind == BundleKindOfficeConvert {
			expectedID, ok := expectedOfficeUnitID(unit.Location)
			if !ok || unit.ID != expectedID {
				return invalidBundle("office unit %q is not deterministic for its location", unit.ID)
			}
		}
		if err := addReference(unit.MarkdownEntry); err != nil {
			return err
		}
		if entryByPath[unit.MarkdownEntry].MIMEType != MIMETypeMarkdown {
			return invalidBundle("unit %q does not reference markdown", unit.ID)
		}
		unitByID[unit.ID] = unit
	}

	assetByID := make(map[string]AssetDescriptor, len(manifest.Assets))
	assetUses := make(map[string]int, len(manifest.Assets))
	for index, asset := range manifest.Assets {
		if !validIdentifier(asset.LocalID) || asset.Entry == "" || asset.Kind != "image" || asset.Width <= 0 || asset.Height <= 0 {
			return invalidBundle("invalid assets[%d]", index)
		}
		if options.Limits.MaxImagePixels > 0 && int64(asset.Width) > options.Limits.MaxImagePixels/int64(asset.Height) {
			return limitExceeded("asset %q exceeds pixel quota", asset.LocalID)
		}
		switch asset.SourceKind {
		case "embedded_original", "page_crop", "scanned_page":
		default:
			return invalidBundle("invalid asset sourceKind %q", asset.SourceKind)
		}
		if _, duplicate := assetByID[asset.LocalID]; duplicate {
			return invalidBundle("duplicate asset localId %q", asset.LocalID)
		}
		if err := addReference(asset.Entry); err != nil {
			return err
		}
		if options.Limits.MaxAssetBytes > 0 && entryByPath[asset.Entry].ByteSize > options.Limits.MaxAssetBytes {
			return limitExceeded("asset %q exceeds byte quota", asset.LocalID)
		}
		if !strings.HasPrefix(entryByPath[asset.Entry].MIMEType, "image/") {
			return invalidBundle("asset %q does not reference an image", asset.LocalID)
		}
		assetByID[asset.LocalID] = asset
	}

	pageByNumber := make(map[int]PageDescriptor, len(manifest.Pages))
	pageByUnit := make(map[string]PageDescriptor, len(manifest.Pages))
	lastPage := 0
	for index, pageDescriptor := range manifest.Pages {
		if pageDescriptor.Page <= 0 || !validIdentifier(pageDescriptor.UnitID) || pageDescriptor.Page <= lastPage {
			return invalidBundle("invalid or unsorted pages[%d]", index)
		}
		if _, duplicate := pageByUnit[pageDescriptor.UnitID]; duplicate {
			return invalidBundle("duplicate page unitId %q", pageDescriptor.UnitID)
		}
		expectedUnitID := fmt.Sprintf("unit_page_%s_%04d", manifest.Source.SHA256[:12], pageDescriptor.Page)
		if pageDescriptor.UnitID != expectedUnitID {
			return invalidBundle("page %d has non-deterministic unitId %q", pageDescriptor.Page, pageDescriptor.UnitID)
		}
		if options.ExpectedPageUnits != nil {
			expectedUnit, exists := options.ExpectedPageUnits[pageDescriptor.Page]
			if !exists || expectedUnit != pageDescriptor.UnitID {
				return invalidBundle("page %d unitId does not match analyze bundle", pageDescriptor.Page)
			}
		}
		switch pageDescriptor.Status {
		case PageStatusOK:
			if pageDescriptor.ErrorCode != "" {
				return invalidBundle("ok page %d has errorCode", pageDescriptor.Page)
			}
			switch manifest.BundleKind {
			case BundleKindPDFAnalyze:
				if pageDescriptor.NativeMarkdownEntry == "" || pageDescriptor.PrimitiveEntry == "" || pageDescriptor.RenderEntry != "" {
					return invalidBundle("pdf-analyze ok page %d violates entry matrix", pageDescriptor.Page)
				}
			case BundleKindPDFRender:
				if pageDescriptor.RenderEntry == "" || pageDescriptor.NativeMarkdownEntry != "" || pageDescriptor.PrimitiveEntry != "" {
					return invalidBundle("pdf-render ok page %d violates entry matrix", pageDescriptor.Page)
				}
			default:
				return invalidBundle("office bundle contains pages")
			}
		case PageStatusFailed:
			allowed := options.AllowedPageErrors
			if allowed == nil {
				allowed = defaultPageErrorCodes
			}
			if _, ok := allowed[pageDescriptor.ErrorCode]; !ok || pageDescriptor.NativeMarkdownEntry != "" ||
				pageDescriptor.RenderEntry != "" || pageDescriptor.PrimitiveEntry != "" {
				return invalidBundle("failed page %d violates entry matrix", pageDescriptor.Page)
			}
		default:
			return invalidBundle("invalid page status %q", pageDescriptor.Status)
		}
		for _, entryPath := range []string{
			pageDescriptor.NativeMarkdownEntry, pageDescriptor.RenderEntry, pageDescriptor.PrimitiveEntry,
		} {
			if err := addReference(entryPath); err != nil {
				return err
			}
		}
		if pageDescriptor.NativeMarkdownEntry != "" && entryByPath[pageDescriptor.NativeMarkdownEntry].MIMEType != MIMETypeMarkdown {
			return invalidBundle("page %d native entry is not markdown", pageDescriptor.Page)
		}
		if pageDescriptor.PrimitiveEntry != "" && entryByPath[pageDescriptor.PrimitiveEntry].MIMEType != MIMETypeJSON {
			return invalidBundle("page %d primitive entry is not JSON", pageDescriptor.Page)
		}
		if pageDescriptor.RenderEntry != "" && !strings.HasPrefix(entryByPath[pageDescriptor.RenderEntry].MIMEType, "image/") {
			return invalidBundle("page %d render entry is not an image", pageDescriptor.Page)
		}
		if pageDescriptor.RenderEntry != "" && options.Limits.MaxRenderBytes > 0 &&
			entryByPath[pageDescriptor.RenderEntry].ByteSize > options.Limits.MaxRenderBytes {
			return limitExceeded("page %d render exceeds byte quota", pageDescriptor.Page)
		}
		pageByNumber[pageDescriptor.Page] = pageDescriptor
		pageByUnit[pageDescriptor.UnitID] = pageDescriptor
		lastPage = pageDescriptor.Page
	}

	switch manifest.BundleKind {
	case BundleKindOfficeConvert:
		if manifest.Source.Format != "docx" && manifest.Source.Format != "pptx" && manifest.Source.Format != "xlsx" {
			return invalidBundle("office source format %q is unsupported", manifest.Source.Format)
		}
		if manifest.Parser.Name != "markitdown" || manifest.Parser.Version != ExpectedMarkItDownVersion ||
			manifest.Parser.WrapperVersion != ExpectedOfficeWrapper {
			return invalidBundle("office parser identity/version is incompatible")
		}
		if len(manifest.Pages) != 0 || len(manifest.Units) == 0 {
			return invalidBundle("office bundle requires units and forbids pages")
		}
	case BundleKindPDFAnalyze:
		if manifest.Source.Format != "pdf" || len(manifest.Units) != 0 || len(manifest.Assets) != 0 || len(manifest.Occurrences) != 0 {
			return invalidBundle("pdf-analyze top-level contract is invalid")
		}
		if len(manifest.Pages) == 0 {
			return invalidBundle("pdf-analyze requires at least one page")
		}
		for index, pageDescriptor := range manifest.Pages {
			if pageDescriptor.Page != index+1 {
				return invalidBundle("pdf-analyze pages must cover 1..N without gaps")
			}
		}
	case BundleKindPDFRender:
		if manifest.Source.Format != "pdf" || len(manifest.Units) != 0 {
			return invalidBundle("pdf-render top-level contract is invalid")
		}
		if len(manifest.Pages) == 0 {
			return invalidBundle("pdf-render requires at least one page")
		}
		if len(options.RequestedPages) > 0 {
			requested := append([]int(nil), options.RequestedPages...)
			sort.Ints(requested)
			if len(requested) != len(manifest.Pages) {
				return invalidBundle("pdf-render page set differs from request")
			}
			for index, pageNumber := range requested {
				if pageNumber <= 0 || (index > 0 && requested[index-1] == pageNumber) || manifest.Pages[index].Page != pageNumber {
					return invalidBundle("pdf-render page set differs from request")
				}
			}
		}
	}

	occurrenceIDs := make(map[string]struct{}, len(manifest.Occurrences))
	orders := make(map[string]map[int]struct{})
	for index, occurrence := range manifest.Occurrences {
		if !validIdentifier(occurrence.ID) || !validIdentifier(occurrence.AssetLocalID) || !validIdentifier(occurrence.UnitID) ||
			occurrence.Order < 0 || !validLocation(occurrence.Location) || !validBBox(occurrence.BBox) ||
			occurrence.Confidence < 0 || occurrence.Confidence > 1 {
			return invalidBundle("invalid occurrences[%d]", index)
		}
		if _, duplicate := occurrenceIDs[occurrence.ID]; duplicate {
			return invalidBundle("duplicate occurrence id %q", occurrence.ID)
		}
		asset, assetOK := assetByID[occurrence.AssetLocalID]
		if !assetOK {
			return invalidBundle("occurrence %q references unknown asset", occurrence.ID)
		}
		_ = asset
		var expectedLocation Location
		if manifest.BundleKind == BundleKindOfficeConvert {
			unit, ok := unitByID[occurrence.UnitID]
			if !ok {
				return invalidBundle("occurrence %q references unknown unit", occurrence.ID)
			}
			expectedLocation = unit.Location
		} else {
			pageDescriptor, ok := pageByUnit[occurrence.UnitID]
			if !ok || pageDescriptor.Status != PageStatusOK {
				return invalidBundle("occurrence %q references unknown/failed page unit", occurrence.ID)
			}
			expectedLocation = Location{Kind: "page", Index: pageDescriptor.Page, Label: occurrence.Location.Label}
		}
		if !sameLocation(expectedLocation, occurrence.Location) {
			return invalidBundle("occurrence %q location differs from unit", occurrence.ID)
		}
		if orders[occurrence.UnitID] == nil {
			orders[occurrence.UnitID] = make(map[int]struct{})
		}
		if _, duplicate := orders[occurrence.UnitID][occurrence.Order]; duplicate {
			return invalidBundle("duplicate occurrence order %d in unit %q", occurrence.Order, occurrence.UnitID)
		}
		orders[occurrence.UnitID][occurrence.Order] = struct{}{}
		occurrenceIDs[occurrence.ID] = struct{}{}
		assetUses[occurrence.AssetLocalID]++
	}
	for assetID := range assetByID {
		if assetUses[assetID] == 0 {
			return invalidBundle("asset %q has no occurrence", assetID)
		}
	}
	for index, warning := range manifest.Warnings {
		if !validIdentifier(warning.Code) || strings.TrimSpace(warning.Message) == "" ||
			(warning.Location != nil && !validLocation(*warning.Location)) {
			return invalidBundle("invalid warnings[%d]", index)
		}
	}
	for entryPath := range entryByPath {
		if referenceCount[entryPath] != 1 {
			return invalidBundle("entry %q reference count is %d, want 1", entryPath, referenceCount[entryPath])
		}
	}
	return nil
}

// ValidatePDFBundlePair proves that render pages use the analyze bundle's
// deterministic page unit IDs instead of relying on array position.
func ValidatePDFBundlePair(analyze, render *Manifest, requestedPages []int) error {
	if analyze == nil || render == nil || analyze.BundleKind != BundleKindPDFAnalyze || render.BundleKind != BundleKindPDFRender {
		return invalidBundle("invalid analyze/render bundle pair")
	}
	if err := ValidateManifest(analyze, DecodeOptions{
		ExpectedKind:   BundleKindPDFAnalyze,
		ExpectedSource: analyze.Source,
	}); err != nil {
		return err
	}
	if analyze.Source != render.Source {
		return invalidBundle("analyze/render source descriptors differ")
	}
	units := make(map[int]string, len(analyze.Pages))
	for _, pageDescriptor := range analyze.Pages {
		units[pageDescriptor.Page] = pageDescriptor.UnitID
	}
	return ValidateManifest(render, DecodeOptions{
		ExpectedKind:      BundleKindPDFRender,
		ExpectedSource:    analyze.Source,
		RequestedPages:    requestedPages,
		ExpectedPageUnits: units,
	})
}

func validatePagePrimitive(primitive *PagePrimitive) error {
	if primitive == nil || primitive.Page <= 0 || primitive.Width <= 0 || primitive.Height <= 0 ||
		primitive.TextChars < 0 || primitive.BlockCount < 0 || primitive.TextCoverage < 0 || primitive.TextCoverage > 1 ||
		primitive.TextBlocks == nil || primitive.EmbeddedImages == nil || primitive.BlockCount != len(primitive.TextBlocks) {
		return invalidBundle("invalid page primitive summary")
	}
	textChars := 0
	for index, block := range primitive.TextBlocks {
		if block.Text == "" || block.BBox == nil || !validBBox(block.BBox) {
			return invalidBundle("invalid page primitive textBlocks[%d]", index)
		}
		textChars += utf8.RuneCountInString(block.Text)
	}
	if primitive.TextChars != textChars {
		return invalidBundle("page primitive textChars=%d, want %d", primitive.TextChars, textChars)
	}
	for index, image := range primitive.EmbeddedImages {
		if image.BBox == nil || !validBBox(image.BBox) {
			return invalidBundle("invalid page primitive embeddedImages[%d]", index)
		}
	}
	return nil
}

func DecodePagePrimitive(reader io.Reader) (PagePrimitive, error) {
	var primitive PagePrimitive
	decoder := json.NewDecoder(&strictUTF8Reader{reader: reader})
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&primitive); err != nil {
		return primitive, invalidBundle("decode page primitive: %v", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return primitive, invalidBundle("page primitive has trailing JSON")
	}
	if err := validatePagePrimitive(&primitive); err != nil {
		return primitive, err
	}
	return primitive, nil
}

func validateHealth(health *Health) error {
	if health == nil || health.ProtocolVersion != ProtocolVersion || strings.TrimSpace(health.ServiceVersion) == "" ||
		health.Limits.MaxInputBytes <= 0 || health.Limits.MaxOutputBytes <= 0 || health.Capabilities.Office.Formats == nil {
		return invalidBundle("invalid health contract")
	}
	seen := make(map[string]struct{}, len(health.Capabilities.Office.Formats))
	lastFormat := ""
	for index, format := range health.Capabilities.Office.Formats {
		if format != "docx" && format != "pptx" && format != "xlsx" {
			return invalidBundle("unknown office health format %q", format)
		}
		if _, duplicate := seen[format]; duplicate {
			return invalidBundle("duplicate office health format %q", format)
		}
		if index > 0 && format <= lastFormat {
			return invalidBundle("office health formats are not sorted")
		}
		seen[format] = struct{}{}
		lastFormat = format
	}
	if health.Capabilities.Office.MarkItDownVersion == "" || health.Capabilities.Office.WrapperVersion == "" {
		return invalidBundle("office capability has no version")
	}
	if health.Capabilities.PDF.Enabled && (health.Capabilities.PDF.Engine == "" || health.Capabilities.PDF.EngineVersion == "") {
		return invalidBundle("enabled PDF capability has no engine version")
	}
	if !health.Capabilities.PDF.Enabled && (health.Capabilities.PDF.Engine != "" || health.Capabilities.PDF.EngineVersion != "") {
		return invalidBundle("disabled PDF capability advertises an engine")
	}
	return nil
}
