// Package assets persists parser-extracted binary assets and publishes the
// canonical ParsedArtifact. It depends only on narrow object/catalog surfaces.
package assets

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/store"
)

type ObjectStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

type Catalog interface {
	UpsertRAGAsset(ctx context.Context, asset *store.RAGAssetRecord) error
	ListRAGAssetsByIDs(ctx context.Context, ids []string) ([]store.RAGAssetRecord, error)
}

type Limits struct {
	MaxAssets         int
	MaxAssetBytes     int64
	MaxExtractedBytes int64
	MaxImagePixels    int64
	MaxArtifactBytes  int64
}

func (l Limits) validate() error {
	if l.MaxAssets < 1 || l.MaxAssetBytes < 1 || l.MaxExtractedBytes < 1 || l.MaxImagePixels < 1 || l.MaxArtifactBytes < 1 {
		return errors.New("positive asset count, asset, extracted, and artifact limits are required")
	}
	if l.MaxAssetBytes > l.MaxExtractedBytes {
		return errors.New("single asset byte limit cannot exceed total extracted byte limit")
	}
	return nil
}

type Persister struct {
	Objects ObjectStore
	Catalog Catalog
	Limits  Limits
}

type PersistRequest struct {
	UserID           string
	KBID             string
	DocID            string
	DocVersion       int64
	ParseFingerprint string
	NeutralCaption   string
	Document         *document.ParsedDocument
}

type CacheRequest struct {
	UserID           string
	KBID             string
	DocID            string
	ParseFingerprint string
}

// PersistParsedDocument validates every asset by streaming from its bundle
// handle, writes stable source objects, records immutable catalog rows, then
// publishes normalized.md and finally parsed.json. parsed.json is the sole
// cache publication point. Canonical staging objects are deliberately retained
// on failure because an identical reindex/history snapshot may already use
// them; the later orphan GC owns reclamation.
func (p *Persister) PersistParsedDocument(ctx context.Context, request PersistRequest) (_ *document.ParsedArtifact, resultErr error) {
	if request.Document == nil {
		return nil, errors.New("parsed document is required")
	}
	defer func() {
		if closeErr := request.Document.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close parsed document: %w", closeErr))
		}
	}()
	if err := p.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.DocVersion < 1 {
		return nil, errors.New("document version must be positive")
	}
	if request.Document.Source.DocID != request.DocID {
		return nil, errors.New("persistence document ID does not match parsed source")
	}
	artifactKey, err := document.ArtifactJSONKey(request.UserID, request.KBID, request.DocID, request.ParseFingerprint)
	if err != nil {
		return nil, err
	}
	normalizedKey, err := document.NormalizedMarkdownKey(request.UserID, request.KBID, request.DocID, request.ParseFingerprint)
	if err != nil {
		return nil, err
	}
	if err := request.Document.Validate(); err != nil {
		return nil, fmt.Errorf("validate parsed document: %w", err)
	}
	if len(request.Document.Assets) > p.Limits.MaxAssets {
		return nil, fmt.Errorf("parsed document has %d assets, limit is %d", len(request.Document.Assets), p.Limits.MaxAssets)
	}
	var totalBytes int64
	for _, asset := range request.Document.Assets {
		if asset.ByteSize > p.Limits.MaxAssetBytes {
			return nil, fmt.Errorf("asset %q is %d bytes, per-asset limit is %d", asset.LocalID, asset.ByteSize, p.Limits.MaxAssetBytes)
		}
		if asset.ByteSize > p.Limits.MaxExtractedBytes-totalBytes {
			return nil, fmt.Errorf("extracted assets exceed %d byte limit", p.Limits.MaxExtractedBytes)
		}
		if int64(asset.Width) > p.Limits.MaxImagePixels/int64(asset.Height) {
			return nil, fmt.Errorf("asset %q exceeds %d pixel limit", asset.LocalID, p.Limits.MaxImagePixels)
		}
		totalBytes += asset.ByteSize
	}

	artifact, err := document.Canonicalize(request.Document, request.NeutralCaption)
	if err != nil {
		return nil, fmt.Errorf("canonicalize parsed document: %w", err)
	}
	assetByHash := make(map[string]*document.ArtifactAsset, len(artifact.Assets))
	ids := make([]string, 0, len(artifact.Assets))
	for i := range artifact.Assets {
		assetByHash[artifact.Assets[i].ContentSHA256] = &artifact.Assets[i]
		ids = append(ids, artifact.Assets[i].ID)
	}
	existing, err := p.Catalog.ListRAGAssetsByIDs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("load existing asset catalog: %w", err)
	}
	existingByID := make(map[string]store.RAGAssetRecord, len(existing))
	for _, record := range existing {
		existingByID[record.ID] = record
	}

	for _, transient := range request.Document.Assets {
		if err := p.validateBundleAsset(ctx, request.Document, transient); err != nil {
			return nil, err
		}
		canonical := assetByHash[transient.ContentSHA256]
		keys, err := document.NewObjectKeys(request.UserID, request.KBID, request.DocID,
			transient.ContentSHA256, transient.SourceMIME, request.ParseFingerprint)
		if err != nil {
			return nil, err
		}
		record := store.RAGAssetRecord{
			ID: canonical.ID, DocID: request.DocID, ContentSHA256: transient.ContentSHA256,
			SourceKind: transient.SourceKind, SourceMIME: transient.SourceMIME,
			SourceObjectKey: keys.AssetSource, DisplayStatus: document.DisplayUnavailable,
			ByteSize: transient.ByteSize, Width: transient.Width, Height: transient.Height,
			FirstSeenVersion: request.DocVersion, LastSeenVersion: request.DocVersion,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if prior, ok := existingByID[canonical.ID]; ok {
			if err := validateExistingAsset(prior, record, keys); err != nil {
				return nil, err
			}
			record.DisplayMIME = prior.DisplayMIME
			record.DisplayObjectKey = prior.DisplayObjectKey
			record.ThumbnailObjectKey = prior.ThumbnailObjectKey
			record.DisplayStatus = prior.DisplayStatus
			record.DisplaySHA256 = prior.DisplaySHA256
			canonical.DisplayStatus = prior.DisplayStatus
		}
		exists, err := p.objectExists(ctx, keys.AssetSource)
		if err != nil {
			return nil, fmt.Errorf("check asset %q object: %w", transient.LocalID, err)
		}
		if !exists {
			reader, err := request.Document.OpenBundleEntry(ctx, transient.BundleEntry)
			if err != nil {
				return nil, fmt.Errorf("reopen asset %q: %w", transient.LocalID, err)
			}
			putErr := p.Objects.Put(ctx, keys.AssetSource, reader, transient.ByteSize, transient.SourceMIME)
			closeErr := reader.Close()
			if putErr != nil {
				return nil, fmt.Errorf("store asset %q: %w", transient.LocalID, putErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close asset %q after store: %w", transient.LocalID, closeErr)
			}
		}
		if err := p.Catalog.UpsertRAGAsset(ctx, &record); err != nil {
			return nil, fmt.Errorf("upsert asset %q: %w", canonical.ID, err)
		}
	}
	if err := artifact.Validate(); err != nil {
		return nil, fmt.Errorf("validate canonical artifact: %w", err)
	}
	normalized := artifact.NormalizedMarkdown()
	if err := p.Objects.Put(ctx, normalizedKey, strings.NewReader(normalized), int64(len(normalized)), "text/markdown; charset=utf-8"); err != nil {
		return nil, fmt.Errorf("store normalized Markdown: %w", err)
	}
	encoded, err := document.EncodeArtifact(artifact, p.Limits.MaxArtifactBytes)
	if err != nil {
		return nil, err
	}
	if err := p.Objects.Put(ctx, artifactKey, strings.NewReader(string(encoded)), int64(len(encoded)), "application/json"); err != nil {
		return nil, fmt.Errorf("publish parsed artifact: %w", err)
	}
	return artifact, nil
}

func (p *Persister) validateBundleAsset(ctx context.Context, parsed *document.ParsedDocument, asset document.ExtractedAsset) error {
	reader, err := parsed.OpenBundleEntry(ctx, asset.BundleEntry)
	if err != nil {
		return fmt.Errorf("open asset %q: %w", asset.LocalID, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(&contextReader{ctx: ctx, reader: reader}, p.Limits.MaxAssetBytes+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return fmt.Errorf("read asset %q: %w", asset.LocalID, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close asset %q: %w", asset.LocalID, closeErr)
	}
	if written != asset.ByteSize {
		return fmt.Errorf("asset %q declared %d bytes but bundle contained %d", asset.LocalID, asset.ByteSize, written)
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if actualHash != asset.ContentSHA256 {
		return fmt.Errorf("asset %q SHA-256 mismatch: declared %s, actual %s", asset.LocalID, asset.ContentSHA256, actualHash)
	}
	return nil
}

// LoadParsedArtifact treats a missing/corrupt artifact or any missing binary
// dependency as a cache miss. Catalog query failures remain observable errors.
func (p *Persister) LoadParsedArtifact(ctx context.Context, request CacheRequest) (*document.ParsedArtifact, bool, error) {
	if err := p.validate(); err != nil {
		return nil, false, err
	}
	artifactKey, err := document.ArtifactJSONKey(request.UserID, request.KBID, request.DocID, request.ParseFingerprint)
	if err != nil {
		return nil, false, err
	}
	reader, err := p.Objects.Get(ctx, artifactKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read parsed artifact: %w", err)
	}
	artifact, decodeErr := document.DecodeArtifact(reader, p.Limits.MaxArtifactBytes)
	closeErr := reader.Close()
	if decodeErr != nil || closeErr != nil || artifact.Source.DocID != request.DocID {
		return nil, false, nil
	}
	ids := make([]string, 0, len(artifact.Assets))
	for _, asset := range artifact.Assets {
		ids = append(ids, asset.ID)
	}
	records, err := p.Catalog.ListRAGAssetsByIDs(ctx, ids)
	if err != nil {
		return nil, false, fmt.Errorf("rehydrate asset catalog: %w", err)
	}
	if len(records) != len(ids) {
		return nil, false, nil
	}
	byID := make(map[string]store.RAGAssetRecord, len(records))
	for _, record := range records {
		byID[record.ID] = record
	}
	for _, asset := range artifact.Assets {
		record, ok := byID[asset.ID]
		if !ok {
			return nil, false, nil
		}
		keys, err := document.NewObjectKeys(request.UserID, request.KBID, request.DocID,
			asset.ContentSHA256, asset.SourceMIME, request.ParseFingerprint)
		if err != nil || !recordMatchesArtifact(record, asset, request.DocID, keys) {
			return nil, false, nil
		}
		exists, err := p.objectExists(ctx, record.SourceObjectKey)
		if err != nil {
			return nil, false, fmt.Errorf("check source asset %q: %w", record.ID, err)
		}
		if !exists {
			return nil, false, nil
		}
		if asset.DisplayStatus == document.DisplayReady {
			if record.DisplayMIME != "image/webp" && record.DisplayMIME != "image/png" {
				return nil, false, nil
			}
			exists, err := p.objectExists(ctx, record.DisplayObjectKey)
			if err != nil {
				return nil, false, fmt.Errorf("check display asset %q: %w", record.ID, err)
			}
			if !exists {
				return nil, false, nil
			}
		}
	}
	return artifact, true, nil
}

func (p *Persister) validate() error {
	if p == nil || p.Objects == nil || p.Catalog == nil {
		return errors.New("asset persister requires object store and catalog")
	}
	return p.Limits.validate()
}

func (p *Persister) objectExists(ctx context.Context, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	reader, err := p.Objects.Get(ctx, key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := reader.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func validateExistingAsset(existing, expected store.RAGAssetRecord, keys document.ObjectKeys) error {
	if existing.ID != expected.ID || existing.DocID != expected.DocID || existing.ContentSHA256 != expected.ContentSHA256 ||
		existing.SourceKind != expected.SourceKind || existing.SourceMIME != expected.SourceMIME ||
		existing.SourceObjectKey != keys.AssetSource || existing.ByteSize != expected.ByteSize ||
		existing.Width != expected.Width || existing.Height != expected.Height {
		return fmt.Errorf("existing asset %q conflicts with canonical source description", expected.ID)
	}
	if existing.DisplayStatus != document.DisplayReady && existing.DisplayStatus != document.DisplayUnavailable {
		return fmt.Errorf("existing asset %q has invalid display status %q", expected.ID, existing.DisplayStatus)
	}
	if existing.DisplayStatus == document.DisplayReady &&
		(existing.DisplayObjectKey != keys.AssetDisplay || existing.ThumbnailObjectKey != keys.AssetThumbnail) {
		return fmt.Errorf("existing ready asset %q has non-canonical display keys", expected.ID)
	}
	return nil
}

func recordMatchesArtifact(record store.RAGAssetRecord, asset document.ArtifactAsset, docID string, keys document.ObjectKeys) bool {
	if record.ID != asset.ID || record.DocID != docID || record.ContentSHA256 != asset.ContentSHA256 ||
		record.SourceKind != asset.SourceKind || record.SourceMIME != asset.SourceMIME ||
		record.SourceObjectKey != keys.AssetSource || record.ByteSize != asset.ByteSize ||
		record.Width != asset.Width || record.Height != asset.Height || record.DisplayStatus != asset.DisplayStatus {
		return false
	}
	if record.DisplayStatus == document.DisplayReady {
		return record.DisplayObjectKey == keys.AssetDisplay && record.ThumbnailObjectKey == keys.AssetThumbnail &&
			document.CanonicalSHA256(record.DisplaySHA256)
	}
	return true
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
