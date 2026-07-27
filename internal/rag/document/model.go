// Package document defines the transient parser IR and the canonical,
// serializable parse artifact used by the RAG indexing pipeline.
package document

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"
)

const (
	ParsedDocumentSchemaVersion = 1
	ParsedArtifactSchemaVersion = 1

	LocationDocument = "document"
	LocationPage     = "page"
	LocationSlide    = "slide"
	LocationSheet    = "sheet"

	AssetKindImage = "image"

	SourceKindEmbeddedOriginal = "embedded_original"
	SourceKindEmbeddedPreview  = "embedded_preview"
	SourceKindPageCrop         = "page_crop"
	SourceKindScannedPage      = "scanned_page"

	AttachmentKindVisioSource = "visio_source"
	MIMETypeVSDX              = "application/vnd.ms-visio.drawing"

	DisplayReady       = "ready"
	DisplayUnavailable = "unavailable"
)

var safeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,255}$`)

// Source is a reopenable source document. Parsers receive this type instead
// of a one-shot reader so retries and streaming sidecars never require an
// unbounded in-memory copy.
type Source struct {
	DocID    string
	FileName string
	Format   string
	Size     int64
	SHA256   string
	Open     func(context.Context) (io.ReadCloser, error)
}

func (s Source) Parsed() ParsedSource {
	return ParsedSource{
		DocID: s.DocID, FileName: s.FileName, Format: s.Format,
		ByteSize: s.Size, SHA256: s.SHA256,
	}
}

func (s Source) Validate() error {
	if err := s.Parsed().Validate(); err != nil {
		return err
	}
	if s.Open == nil {
		return errors.New("document source opener is required")
	}
	return nil
}

// ParsedSource is the secret-free, serializable source identity retained in
// ParsedDocument and ParsedArtifact. It deliberately has no opener or object
// storage key.
type ParsedSource struct {
	DocID    string `json:"docId"`
	FileName string `json:"fileName"`
	Format   string `json:"format"`
	ByteSize int64  `json:"byteSize"`
	SHA256   string `json:"sha256"`
}

func (s ParsedSource) Validate() error {
	if !safeID(s.DocID) {
		return fmt.Errorf("invalid source doc ID %q", s.DocID)
	}
	if !validText(s.FileName) || strings.TrimSpace(s.FileName) == "" {
		return errors.New("source file name is required and must be valid UTF-8")
	}
	if !safeID(strings.ToLower(s.Format)) {
		return fmt.Errorf("invalid source format %q", s.Format)
	}
	if s.ByteSize < 0 {
		return errors.New("source byte size cannot be negative")
	}
	if !CanonicalSHA256(s.SHA256) {
		return errors.New("source SHA-256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

type ParserInfo struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	WrapperVersion string `json:"wrapperVersion,omitempty"`
}

func (p ParserInfo) Validate() error {
	if !validContractString(p.Name, 128) || !validContractString(p.Version, 128) {
		return errors.New("parser name and version are required")
	}
	if p.WrapperVersion != "" && !validContractString(p.WrapperVersion, 128) {
		return errors.New("invalid parser wrapper version")
	}
	return nil
}

type MarkdownUnit struct {
	ID       string         `json:"id"`
	Location SourceLocation `json:"location"`
	Markdown string         `json:"markdown"`
}

type SourceLocation struct {
	Kind  string `json:"kind"`
	Index int    `json:"index"`
	Label string `json:"label"`
}

func (l SourceLocation) Validate() error {
	switch l.Kind {
	case LocationDocument:
		if l.Index != 0 {
			return errors.New("document location index must be 0")
		}
	case LocationPage, LocationSlide, LocationSheet:
		if l.Index < 1 {
			return fmt.Errorf("%s location index must be 1-based", l.Kind)
		}
	default:
		return fmt.Errorf("unsupported source location kind %q", l.Kind)
	}
	if !validText(l.Label) {
		return errors.New("source location label must be valid UTF-8")
	}
	return nil
}

// NormalizedBBox uses the protocol's 0..1000 coordinate system and JSON array
// representation.
type NormalizedBBox [4]int

func (b NormalizedBBox) Validate() error {
	if b[0] < 0 || b[1] < 0 || b[2] > 1000 || b[3] > 1000 || b[0] >= b[2] || b[1] >= b[3] {
		return fmt.Errorf("invalid normalized bbox %v", b)
	}
	return nil
}

type ExtractedAsset struct {
	LocalID       string `json:"localId"`
	ContentSHA256 string `json:"contentSha256"`
	Kind          string `json:"kind"`
	SourceKind    string `json:"sourceKind"`
	SourceMIME    string `json:"sourceMime"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ByteSize      int64  `json:"byteSize"`
	BundleEntry   string `json:"-"`
}

type ExtractedAttachment struct {
	LocalID       string `json:"localId"`
	ContentSHA256 string `json:"contentSha256"`
	Kind          string `json:"kind"`
	FileName      string `json:"fileName"`
	MIMEType      string `json:"mimeType"`
	ByteSize      int64  `json:"byteSize"`
	BundleEntry   string `json:"-"`
}

type AssetOccurrence struct {
	ID                string          `json:"id"`
	AssetLocalID      string          `json:"assetLocalId"`
	AttachmentLocalID string          `json:"attachmentLocalId,omitempty"`
	UnitID            string          `json:"unitId"`
	Order             int             `json:"order"`
	Location          SourceLocation  `json:"location"`
	BBox              *NormalizedBBox `json:"bbox,omitempty"`
	AltText           string          `json:"altText,omitempty"`
	Caption           string          `json:"caption,omitempty"`
	OCRText           string          `json:"ocrText,omitempty"`
	Decorative        bool            `json:"decorative"`
	Confidence        float64         `json:"confidence"`
}

type ParseWarning struct {
	Code     string          `json:"code"`
	Message  string          `json:"message"`
	Location *SourceLocation `json:"location,omitempty"`
	Degraded bool            `json:"degraded"`
}

type BundleEntryOpener func(context.Context, string) (io.ReadCloser, error)

type ParsedDocumentInput struct {
	SchemaVersion int
	Source        ParsedSource
	Parser        ParserInfo
	Units         []MarkdownUnit
	Assets        []ExtractedAsset
	Attachments   []ExtractedAttachment
	Occurrences   []AssetOccurrence
	Warnings      []ParseWarning
}

// ParsedDocument owns parser bundle handles. It is intentionally impossible
// to JSON encode; persistence must first canonicalize it to ParsedArtifact.
type ParsedDocument struct {
	SchemaVersion int
	Source        ParsedSource
	Parser        ParserInfo
	Units         []MarkdownUnit
	Assets        []ExtractedAsset
	Attachments   []ExtractedAttachment
	Occurrences   []AssetOccurrence
	Warnings      []ParseWarning

	openEntry BundleEntryOpener
	cleanup   func() error

	cleanupOnce sync.Once
	cleanupErr  error
}

func NewParsedDocument(input ParsedDocumentInput, opener BundleEntryOpener, cleanup func() error) *ParsedDocument {
	return &ParsedDocument{
		SchemaVersion: input.SchemaVersion,
		Source:        input.Source,
		Parser:        input.Parser,
		Units:         input.Units,
		Assets:        input.Assets,
		Attachments:   input.Attachments,
		Occurrences:   input.Occurrences,
		Warnings:      input.Warnings,
		openEntry:     opener,
		cleanup:       cleanup,
	}
}

func (d *ParsedDocument) MarshalJSON() ([]byte, error) {
	return nil, errors.New("transient ParsedDocument cannot be serialized; canonicalize to ParsedArtifact")
}

func (d *ParsedDocument) Close() error {
	if d == nil {
		return nil
	}
	d.cleanupOnce.Do(func() {
		if d.cleanup != nil {
			d.cleanupErr = d.cleanup()
		}
	})
	return d.cleanupErr
}

func (d *ParsedDocument) OpenBundleEntry(ctx context.Context, entry string) (io.ReadCloser, error) {
	if d == nil || d.openEntry == nil {
		return nil, errors.New("parsed document has no bundle entry opener")
	}
	if err := ValidateBundleEntry(entry); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.openEntry(ctx, entry)
}

func (d *ParsedDocument) Validate() error {
	if d == nil {
		return errors.New("parsed document is nil")
	}
	if d.SchemaVersion != ParsedDocumentSchemaVersion {
		return fmt.Errorf("unsupported parsed document schema version %d", d.SchemaVersion)
	}
	if err := d.Source.Validate(); err != nil {
		return fmt.Errorf("source: %w", err)
	}
	if err := d.Parser.Validate(); err != nil {
		return fmt.Errorf("parser: %w", err)
	}
	units, err := validateUnits(d.Units)
	if err != nil {
		return err
	}
	assets := make(map[string]ExtractedAsset, len(d.Assets))
	hashes := make(map[string]string, len(d.Assets))
	entries := make(map[string]string, len(d.Assets))
	for i, asset := range d.Assets {
		if err := validateExtractedAsset(asset); err != nil {
			return fmt.Errorf("asset %d: %w", i, err)
		}
		if _, exists := assets[asset.LocalID]; exists {
			return fmt.Errorf("duplicate asset local ID %q", asset.LocalID)
		}
		if prior, exists := hashes[asset.ContentSHA256]; exists {
			return fmt.Errorf("duplicate asset content hash %q in %q and %q", asset.ContentSHA256, prior, asset.LocalID)
		}
		if prior, exists := entries[asset.BundleEntry]; exists {
			return fmt.Errorf("duplicate bundle entry %q in %q and %q", asset.BundleEntry, prior, asset.LocalID)
		}
		assets[asset.LocalID] = asset
		hashes[asset.ContentSHA256] = asset.LocalID
		entries[asset.BundleEntry] = asset.LocalID
	}
	attachments := make(map[string]ExtractedAttachment, len(d.Attachments))
	attachmentHashes := make(map[string]string, len(d.Attachments))
	for i, attachment := range d.Attachments {
		if err := validateExtractedAttachment(attachment); err != nil {
			return fmt.Errorf("attachment %d: %w", i, err)
		}
		if _, exists := attachments[attachment.LocalID]; exists {
			return fmt.Errorf("duplicate attachment local ID %q", attachment.LocalID)
		}
		if prior, exists := attachmentHashes[attachment.ContentSHA256]; exists {
			return fmt.Errorf("duplicate attachment content hash %q in %q and %q", attachment.ContentSHA256, prior, attachment.LocalID)
		}
		if prior, exists := entries[attachment.BundleEntry]; exists {
			return fmt.Errorf("duplicate bundle entry %q in %q and %q", attachment.BundleEntry, prior, attachment.LocalID)
		}
		attachments[attachment.LocalID] = attachment
		attachmentHashes[attachment.ContentSHA256] = attachment.LocalID
		entries[attachment.BundleEntry] = attachment.LocalID
	}
	occurrences := make(map[string]struct{}, len(d.Occurrences))
	referencedAssets := make(map[string]struct{}, len(d.Assets))
	referencedAttachments := make(map[string]struct{}, len(d.Attachments))
	for i, occurrence := range d.Occurrences {
		if err := validateTransientOccurrence(occurrence, units, assets, attachments); err != nil {
			return fmt.Errorf("occurrence %d: %w", i, err)
		}
		if _, exists := occurrences[occurrence.ID]; exists {
			return fmt.Errorf("duplicate occurrence ID %q", occurrence.ID)
		}
		occurrences[occurrence.ID] = struct{}{}
		referencedAssets[occurrence.AssetLocalID] = struct{}{}
		if occurrence.AttachmentLocalID != "" {
			referencedAttachments[occurrence.AttachmentLocalID] = struct{}{}
		}
	}
	for localID := range assets {
		if _, ok := referencedAssets[localID]; !ok {
			return fmt.Errorf("asset %q has no occurrence", localID)
		}
	}
	for localID := range attachments {
		if _, ok := referencedAttachments[localID]; !ok {
			return fmt.Errorf("attachment %q has no occurrence", localID)
		}
	}
	for _, unit := range d.Units {
		if err := ValidateMarkdownAssetMarkers(unit.Markdown, occurrences); err != nil {
			return fmt.Errorf("unit %q: %w", unit.ID, err)
		}
	}
	for i, warning := range d.Warnings {
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

func validateUnits(units []MarkdownUnit) (map[string]MarkdownUnit, error) {
	if len(units) == 0 {
		return nil, errors.New("parsed document has no Markdown units")
	}
	byID := make(map[string]MarkdownUnit, len(units))
	lastIndexByKind := make(map[string]int)
	for i, unit := range units {
		if !safeID(unit.ID) {
			return nil, fmt.Errorf("unit %d has invalid ID %q", i, unit.ID)
		}
		if _, exists := byID[unit.ID]; exists {
			return nil, fmt.Errorf("duplicate unit ID %q", unit.ID)
		}
		if err := unit.Location.Validate(); err != nil {
			return nil, fmt.Errorf("unit %q location: %w", unit.ID, err)
		}
		if !validText(unit.Markdown) {
			return nil, fmt.Errorf("unit %q Markdown must be valid UTF-8", unit.ID)
		}
		if previous, seen := lastIndexByKind[unit.Location.Kind]; seen && unit.Location.Index < previous {
			return nil, fmt.Errorf("unit %q is out of source reading order", unit.ID)
		}
		lastIndexByKind[unit.Location.Kind] = unit.Location.Index
		byID[unit.ID] = unit
	}
	return byID, nil
}

func validateExtractedAsset(asset ExtractedAsset) error {
	if !safeID(asset.LocalID) {
		return fmt.Errorf("invalid local ID %q", asset.LocalID)
	}
	if !CanonicalSHA256(asset.ContentSHA256) {
		return errors.New("content SHA-256 must be canonical lowercase hexadecimal")
	}
	if asset.Kind != AssetKindImage {
		return fmt.Errorf("unsupported asset kind %q", asset.Kind)
	}
	switch asset.SourceKind {
	case SourceKindEmbeddedOriginal, SourceKindEmbeddedPreview, SourceKindPageCrop, SourceKindScannedPage:
	default:
		return fmt.Errorf("unsupported source kind %q", asset.SourceKind)
	}
	if !validMIME(asset.SourceMIME) {
		return fmt.Errorf("invalid source MIME %q", asset.SourceMIME)
	}
	if asset.Width < 1 || asset.Height < 1 || asset.ByteSize < 1 {
		return errors.New("asset dimensions and byte size must be positive")
	}
	return ValidateBundleEntry(asset.BundleEntry)
}

func validateExtractedAttachment(attachment ExtractedAttachment) error {
	if !safeID(attachment.LocalID) {
		return fmt.Errorf("invalid local ID %q", attachment.LocalID)
	}
	if !CanonicalSHA256(attachment.ContentSHA256) {
		return errors.New("content SHA-256 must be canonical lowercase hexadecimal")
	}
	if attachment.Kind != AttachmentKindVisioSource {
		return fmt.Errorf("unsupported attachment kind %q", attachment.Kind)
	}
	if !validAttachmentFileName(attachment.FileName) {
		return fmt.Errorf("invalid attachment file name %q", attachment.FileName)
	}
	if attachment.MIMEType != MIMETypeVSDX || attachment.ByteSize < 1 {
		return errors.New("invalid attachment MIME type or byte size")
	}
	return ValidateBundleEntry(attachment.BundleEntry)
}

func validateTransientOccurrence(
	occurrence AssetOccurrence,
	units map[string]MarkdownUnit,
	assets map[string]ExtractedAsset,
	attachments map[string]ExtractedAttachment,
) error {
	if !safeID(occurrence.ID) {
		return fmt.Errorf("invalid ID %q", occurrence.ID)
	}
	if _, ok := assets[occurrence.AssetLocalID]; !ok {
		return fmt.Errorf("references unknown asset %q", occurrence.AssetLocalID)
	}
	if occurrence.AttachmentLocalID != "" {
		if _, ok := attachments[occurrence.AttachmentLocalID]; !ok {
			return fmt.Errorf("references unknown attachment %q", occurrence.AttachmentLocalID)
		}
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
	if math.IsNaN(occurrence.Confidence) || math.IsInf(occurrence.Confidence, 0) || occurrence.Confidence < 0 || occurrence.Confidence > 1 {
		return errors.New("confidence must be between 0 and 1")
	}
	for _, text := range []string{occurrence.AltText, occurrence.Caption, occurrence.OCRText} {
		if !validText(text) {
			return errors.New("occurrence text must be valid UTF-8")
		}
	}
	return nil
}

func AssetID(docID, contentSHA256 string) (string, error) {
	return contentID("ast_", docID, contentSHA256)
}

func AttachmentID(docID, contentSHA256 string) (string, error) {
	return contentID("att_", docID, contentSHA256)
}

func contentID(prefix, docID, contentSHA256 string) (string, error) {
	if !safeID(docID) {
		return "", fmt.Errorf("invalid document ID %q", docID)
	}
	if !CanonicalSHA256(contentSHA256) {
		return "", errors.New("content SHA-256 must be canonical lowercase hexadecimal")
	}
	sum := sha256.Sum256([]byte(docID + "\x00" + contentSHA256))
	return prefix + hex.EncodeToString(sum[:16]), nil
}

func CanonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func ValidateBundleEntry(entry string) error {
	if entry == "" || strings.Contains(entry, "\\") || path.IsAbs(entry) || filepath.IsAbs(entry) || filepath.VolumeName(entry) != "" {
		return fmt.Errorf("bundle entry %q must be a controlled relative path", entry)
	}
	for _, segment := range strings.Split(entry, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("bundle entry %q contains an unsafe path segment", entry)
		}
	}
	if cleaned := path.Clean(entry); cleaned != entry || cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("bundle entry %q is not canonical", entry)
	}
	return nil
}

func safeID(value string) bool { return safeIDPattern.MatchString(value) }

func validText(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, '\x00')
}

func validContractString(value string, max int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= max && validText(value)
}

func validMIME(value string) bool {
	return value != "" && len(value) <= 96 && validText(value) && !strings.ContainsAny(value, "\r\n") && strings.Contains(value, "/")
}

func validAttachmentFileName(value string) bool {
	return value != "" && len(value) <= 255 && validText(value) &&
		!strings.ContainsAny(value, "\r\n/\\") && filepath.Base(value) == value &&
		path.Base(value) == value && strings.EqualFold(path.Ext(value), ".vsdx")
}

var _ json.Marshaler = (*ParsedDocument)(nil)
