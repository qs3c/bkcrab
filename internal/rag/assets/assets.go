// Package assets persists parser-extracted binary assets and publishes the
// canonical ParsedArtifact. It depends only on narrow object/catalog surfaces.
package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
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
	UpsertRAGAttachment(ctx context.Context, attachment *store.RAGAttachmentRecord) error
	ReplaceRAGVersionAssets(ctx context.Context, docID string, docVersion int64, assetIDs []string) error
	ReplaceRAGVersionAttachments(ctx context.Context, docID string, docVersion int64, attachmentIDs []string) error
	PublishRAGAssetsForIndex(ctx context.Context, fence store.IndexFence, assets []store.RAGAssetRecord, assetIDs []string) (bool, error)
	PublishRAGAssetsAndAttachmentsForIndex(ctx context.Context, fence store.IndexFence, assets []store.RAGAssetRecord, assetIDs []string, attachments []store.RAGAttachmentRecord, attachmentIDs []string) (bool, error)
	ListRAGAssetsByIDs(ctx context.Context, ids []string) ([]store.RAGAssetRecord, error)
	ListRAGAttachmentsByIDs(ctx context.Context, ids []string) ([]store.RAGAttachmentRecord, error)
	BeginRAGObjectWrite(ctx context.Context, request store.RAGObjectWriteRequest) (*store.RAGObjectWriteFence, error)
	MarkRAGObjectWriteReady(ctx context.Context, fence store.RAGObjectWriteFence) (bool, error)
}

type Limits struct {
	MaxAssets         int
	MaxAssetBytes     int64
	MaxExtractedBytes int64
	MaxImagePixels    int64
	MaxArtifactBytes  int64
	DisplayMaxEdge    int
	ThumbnailMaxEdge  int
}

func (l Limits) validate() error {
	if l.MaxAssets < 1 || l.MaxAssetBytes < 1 || l.MaxExtractedBytes < 1 || l.MaxImagePixels < 1 || l.MaxArtifactBytes < 1 {
		return errors.New("positive asset count, asset, extracted, and artifact limits are required")
	}
	if l.MaxAssetBytes > l.MaxExtractedBytes {
		return errors.New("single asset byte limit cannot exceed total extracted byte limit")
	}
	if _, err := (SafeImageLimits{
		MaxSourceBytes: l.MaxAssetBytes, MaxPixels: l.MaxImagePixels,
		DisplayMaxEdge: l.DisplayMaxEdge, ThumbnailMaxEdge: l.ThumbnailMaxEdge,
	}).normalized(); err != nil {
		return err
	}
	return nil
}

type Persister struct {
	Objects ObjectStore
	Catalog Catalog
	Limits  Limits
}

type PersistRequest struct {
	UserID              string
	KBID                string
	DocID               string
	DocVersion          int64
	ParseFingerprint    string
	NeutralCaption      string
	ArtifactObjectKey   string
	NormalizedObjectKey string
	IndexFence          *store.IndexFence
	Document            *document.ParsedDocument
}

type CacheRequest struct {
	UserID            string
	KBID              string
	DocID             string
	DocVersion        int64
	ParseFingerprint  string
	ExpectedSource    *document.ParsedSource
	ArtifactObjectKey string
	IndexFence        *store.IndexFence
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
	logicalArtifactKey, err := document.ArtifactJSONKey(request.UserID, request.KBID, request.DocID, request.ParseFingerprint)
	if err != nil {
		return nil, err
	}
	logicalNormalizedKey, err := document.NormalizedMarkdownKey(request.UserID, request.KBID, request.DocID, request.ParseFingerprint)
	if err != nil {
		return nil, err
	}
	expectedArtifactKey, err := document.VersionedObjectKey(logicalArtifactKey, request.DocVersion)
	if err != nil {
		return nil, err
	}
	artifactKey := strings.TrimSpace(request.ArtifactObjectKey)
	if artifactKey == "" {
		artifactKey = expectedArtifactKey
	} else if artifactKey != expectedArtifactKey {
		return nil, errors.New("artifact object key does not belong to the document version")
	}
	expectedNormalizedKey, err := document.VersionedObjectKey(logicalNormalizedKey, request.DocVersion)
	if err != nil {
		return nil, err
	}
	normalizedKey := strings.TrimSpace(request.NormalizedObjectKey)
	if normalizedKey == "" {
		normalizedKey = expectedNormalizedKey
	} else if normalizedKey != expectedNormalizedKey {
		return nil, errors.New("normalized object key does not belong to the document version")
	}
	if err := request.Document.Validate(); err != nil {
		return nil, fmt.Errorf("validate parsed document: %w", err)
	}
	if len(request.Document.Assets)+len(request.Document.Attachments) > p.Limits.MaxAssets {
		return nil, fmt.Errorf("parsed document has %d assets/attachments, limit is %d",
			len(request.Document.Assets)+len(request.Document.Attachments), p.Limits.MaxAssets)
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
	for _, attachment := range request.Document.Attachments {
		if attachment.ByteSize > p.Limits.MaxAssetBytes {
			return nil, fmt.Errorf("attachment %q is %d bytes, per-asset limit is %d",
				attachment.LocalID, attachment.ByteSize, p.Limits.MaxAssetBytes)
		}
		if attachment.ByteSize > p.Limits.MaxExtractedBytes-totalBytes {
			return nil, fmt.Errorf("extracted assets/attachments exceed %d byte limit", p.Limits.MaxExtractedBytes)
		}
		totalBytes += attachment.ByteSize
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
	attachmentByHash := make(map[string]*document.ArtifactAttachment, len(artifact.Attachments))
	attachmentIDs := make([]string, 0, len(artifact.Attachments))
	for i := range artifact.Attachments {
		attachmentByHash[artifact.Attachments[i].ContentSHA256] = &artifact.Attachments[i]
		attachmentIDs = append(attachmentIDs, artifact.Attachments[i].ID)
	}
	existingAttachments, err := p.Catalog.ListRAGAttachmentsByIDs(ctx, attachmentIDs)
	if err != nil {
		return nil, fmt.Errorf("load existing attachment catalog: %w", err)
	}
	existingAttachmentByID := make(map[string]store.RAGAttachmentRecord, len(existingAttachments))
	for _, record := range existingAttachments {
		existingAttachmentByID[record.ID] = record
	}

	recordsToPublish := make([]store.RAGAssetRecord, 0, len(request.Document.Assets))
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
		logicalKeys := keys
		keys.AssetSource, err = document.VersionedObjectKey(keys.AssetSource, request.DocVersion)
		if err != nil {
			return nil, err
		}
		keys.AssetDisplay, err = document.VersionedObjectKey(keys.AssetDisplay, request.DocVersion)
		if err != nil {
			return nil, err
		}
		keys.AssetThumbnail, err = document.VersionedObjectKey(keys.AssetThumbnail, request.DocVersion)
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
		prior, existed := existingByID[canonical.ID]
		if existed {
			if err := validateExistingAsset(prior, record, logicalKeys); err != nil {
				return nil, err
			}
			record.FirstSeenVersion = min(prior.FirstSeenVersion, request.DocVersion)
			record.LastSeenVersion = max(prior.LastSeenVersion, request.DocVersion)
			record.CreatedAt = prior.CreatedAt
			record.SourceObjectKey = prior.SourceObjectKey
			record.DisplayMIME = prior.DisplayMIME
			record.DisplayObjectKey = prior.DisplayObjectKey
			record.ThumbnailObjectKey = prior.ThumbnailObjectKey
			record.DisplayStatus = prior.DisplayStatus
			record.DisplaySHA256 = prior.DisplaySHA256
			record.ThumbnailSHA256 = prior.ThumbnailSHA256
			canonical.DisplayStatus = prior.DisplayStatus
		}
		if existed {
			if err := p.requireExistingAssetObjects(ctx, prior); err != nil {
				return nil, fmt.Errorf("reuse asset %q: %w", canonical.ID, err)
			}
			if prior.DisplayStatus == document.DisplayUnavailable {
				appendDisplayWarning(artifact, transient, request.Document, "asset safe display remains unavailable")
			} else if prior.ThumbnailSHA256 == "" {
				// Phase B did not persist the thumbnail byte hash. Recompute the
				// deterministic bytes for the one-time SQL backfill, but never
				// reopen or rewrite the already-published object keys.
				variants, err := p.makeDisplayVariants(ctx, request.Document, transient)
				if err != nil {
					return nil, fmt.Errorf("backfill asset %q thumbnail hash: %w", canonical.ID, err)
				}
				if prior.DisplayMIME != variants.Display.MIMEType || prior.DisplaySHA256 != variants.Display.SHA256 {
					return nil, fmt.Errorf("existing ready asset %q conflicts with safe renderer output", canonical.ID)
				}
				record.ThumbnailSHA256 = variants.Thumbnail.SHA256
			}
		} else {
			sourceFence, err := p.Catalog.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
				UserID: request.UserID, KBID: request.KBID, DocID: request.DocID,
				ObjectKind: store.RAGObjectKindAssetSource, ObjectKey: record.SourceObjectKey,
				ReferenceKey: canonical.ID,
			})
			if err != nil {
				return nil, fmt.Errorf("stage asset %q source object: %w", transient.LocalID, err)
			}
			reader, err := request.Document.OpenBundleEntry(ctx, transient.BundleEntry)
			if err != nil {
				return nil, fmt.Errorf("reopen asset %q: %w", transient.LocalID, err)
			}
			putErr := p.Objects.Put(ctx, record.SourceObjectKey, reader, transient.ByteSize, transient.SourceMIME)
			closeErr := reader.Close()
			if putErr != nil {
				return nil, fmt.Errorf("store asset %q: %w", transient.LocalID, putErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close asset %q after store: %w", transient.LocalID, closeErr)
			}
			if ready, err := p.Catalog.MarkRAGObjectWriteReady(ctx, *sourceFence); err != nil {
				return nil, fmt.Errorf("mark asset %q source object ready: %w", transient.LocalID, err)
			} else if !ready {
				return nil, fmt.Errorf("mark asset %q source object ready: %w", transient.LocalID, store.ErrRAGLifecycleInactive)
			}
			variants, variantErr := p.makeDisplayVariants(ctx, request.Document, transient)
			if variantErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, ctxErr
				}
				appendDisplayWarning(artifact, transient, request.Document, "safe display/thumbnail generation failed")
			} else {
				if err := p.storeDisplayVariant(ctx, request, canonical.ID, store.RAGObjectKindAssetDisplay,
					keys.AssetDisplay, variants.Display, transient.LocalID, "display"); err != nil {
					return nil, err
				}
				if err := p.storeDisplayVariant(ctx, request, canonical.ID, store.RAGObjectKindAssetThumbnail,
					keys.AssetThumbnail, variants.Thumbnail, transient.LocalID, "thumbnail"); err != nil {
					return nil, err
				}
				record.DisplayMIME = variants.Display.MIMEType
				record.DisplayObjectKey = keys.AssetDisplay
				record.ThumbnailObjectKey = keys.AssetThumbnail
				record.DisplayStatus = document.DisplayReady
				record.DisplaySHA256 = variants.Display.SHA256
				record.ThumbnailSHA256 = variants.Thumbnail.SHA256
				canonical.DisplayStatus = document.DisplayReady
			}
		}
		recordsToPublish = append(recordsToPublish, record)
		if request.IndexFence == nil {
			if err := p.Catalog.UpsertRAGAsset(ctx, &record); err != nil {
				return nil, fmt.Errorf("upsert asset %q: %w", canonical.ID, err)
			}
		}
	}
	attachmentRecordsToPublish := make([]store.RAGAttachmentRecord, 0, len(request.Document.Attachments))
	for _, transient := range request.Document.Attachments {
		if err := p.validateBundleAttachment(ctx, request.Document, transient); err != nil {
			return nil, err
		}
		canonical := attachmentByHash[transient.ContentSHA256]
		logicalKey, err := document.AttachmentSourceKey(
			request.UserID, request.KBID, request.DocID, transient.ContentSHA256)
		if err != nil {
			return nil, err
		}
		objectKey, err := document.VersionedObjectKey(logicalKey, request.DocVersion)
		if err != nil {
			return nil, err
		}
		record := store.RAGAttachmentRecord{
			ID: canonical.ID, DocID: request.DocID, ContentSHA256: transient.ContentSHA256,
			Kind: transient.Kind, FileName: transient.FileName, MIMEType: transient.MIMEType,
			ObjectKey: objectKey, ByteSize: transient.ByteSize,
			FirstSeenVersion: request.DocVersion, LastSeenVersion: request.DocVersion,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}
		if prior, existed := existingAttachmentByID[canonical.ID]; existed {
			if err := validateExistingAttachment(prior, record, logicalKey); err != nil {
				return nil, err
			}
			if exists, err := p.objectExists(ctx, prior.ObjectKey); err != nil {
				return nil, fmt.Errorf("check attachment %q object: %w", canonical.ID, err)
			} else if !exists {
				return nil, fmt.Errorf("reuse attachment %q: published object is missing", canonical.ID)
			}
			record.FirstSeenVersion = min(prior.FirstSeenVersion, request.DocVersion)
			record.LastSeenVersion = max(prior.LastSeenVersion, request.DocVersion)
			record.CreatedAt = prior.CreatedAt
			record.ObjectKey = prior.ObjectKey
		} else {
			fence, err := p.Catalog.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
				UserID: request.UserID, KBID: request.KBID, DocID: request.DocID,
				ObjectKind: store.RAGObjectKindAssetAttachment, ObjectKey: objectKey,
				ReferenceKey: canonical.ID,
			})
			if err != nil {
				return nil, fmt.Errorf("stage attachment %q object: %w", transient.LocalID, err)
			}
			reader, err := request.Document.OpenBundleEntry(ctx, transient.BundleEntry)
			if err != nil {
				return nil, fmt.Errorf("reopen attachment %q: %w", transient.LocalID, err)
			}
			putErr := p.Objects.Put(ctx, objectKey, reader, transient.ByteSize, transient.MIMEType)
			closeErr := reader.Close()
			if putErr != nil {
				return nil, fmt.Errorf("store attachment %q: %w", transient.LocalID, putErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close attachment %q after store: %w", transient.LocalID, closeErr)
			}
			if ready, err := p.Catalog.MarkRAGObjectWriteReady(ctx, *fence); err != nil {
				return nil, fmt.Errorf("mark attachment %q ready: %w", transient.LocalID, err)
			} else if !ready {
				return nil, fmt.Errorf("mark attachment %q ready: %w",
					transient.LocalID, store.ErrRAGLifecycleInactive)
			}
		}
		attachmentRecordsToPublish = append(attachmentRecordsToPublish, record)
		if request.IndexFence == nil {
			if err := p.Catalog.UpsertRAGAttachment(ctx, &record); err != nil {
				return nil, fmt.Errorf("upsert attachment %q: %w", canonical.ID, err)
			}
		}
	}
	if err := artifact.Validate(); err != nil {
		return nil, fmt.Errorf("validate canonical artifact: %w", err)
	}
	if request.IndexFence != nil {
		published, err := p.Catalog.PublishRAGAssetsAndAttachmentsForIndex(
			ctx, *request.IndexFence, recordsToPublish, ids,
			attachmentRecordsToPublish, attachmentIDs)
		if err != nil {
			return nil, fmt.Errorf("publish version %d asset set: %w", request.DocVersion, err)
		}
		if !published {
			return nil, store.ErrRAGLifecycleInactive
		}
	} else {
		if err := p.Catalog.ReplaceRAGVersionAssets(ctx, request.DocID, request.DocVersion, ids); err != nil {
			return nil, fmt.Errorf("record version %d asset set: %w", request.DocVersion, err)
		}
		if err := p.Catalog.ReplaceRAGVersionAttachments(
			ctx, request.DocID, request.DocVersion, attachmentIDs); err != nil {
			return nil, fmt.Errorf("record version %d attachment set: %w", request.DocVersion, err)
		}
	}
	normalized := artifact.NormalizedMarkdown()
	if err := p.stageArtifactObject(ctx, request, store.RAGObjectKindNormalized,
		normalizedKey, artifactKey, normalized, "text/markdown; charset=utf-8"); err != nil {
		return nil, fmt.Errorf("store normalized Markdown: %w", err)
	}
	encoded, err := document.EncodeArtifact(artifact, p.Limits.MaxArtifactBytes)
	if err != nil {
		return nil, err
	}
	if err := p.stageArtifactObject(ctx, request, store.RAGObjectKindParsedArtifact,
		artifactKey, artifactKey, string(encoded), "application/json"); err != nil {
		return nil, fmt.Errorf("publish parsed artifact: %w", err)
	}
	return artifact, nil
}

func (p *Persister) stageArtifactObject(
	ctx context.Context,
	request PersistRequest,
	objectKind, objectKey, referenceKey, content, contentType string,
) error {
	fence, err := p.Catalog.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: request.UserID, KBID: request.KBID, DocID: request.DocID,
		ObjectKind: objectKind, ObjectKey: objectKey, ReferenceKey: referenceKey,
	})
	if err != nil {
		return fmt.Errorf("stage object: %w", err)
	}
	if err := p.Objects.Put(ctx, objectKey, strings.NewReader(content), int64(len(content)), contentType); err != nil {
		return err
	}
	ready, err := p.Catalog.MarkRAGObjectWriteReady(ctx, *fence)
	if err != nil {
		return fmt.Errorf("mark object ready: %w", err)
	}
	if !ready {
		return store.ErrRAGLifecycleInactive
	}
	return nil
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

func (p *Persister) validateBundleAttachment(
	ctx context.Context,
	parsed *document.ParsedDocument,
	attachment document.ExtractedAttachment,
) error {
	reader, err := parsed.OpenBundleEntry(ctx, attachment.BundleEntry)
	if err != nil {
		return fmt.Errorf("open attachment %q: %w", attachment.LocalID, err)
	}
	hash := sha256.New()
	written, copyErr := io.Copy(hash, io.LimitReader(
		&contextReader{ctx: ctx, reader: reader}, p.Limits.MaxAssetBytes+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return fmt.Errorf("read attachment %q: %w", attachment.LocalID, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close attachment %q: %w", attachment.LocalID, closeErr)
	}
	if written != attachment.ByteSize {
		return fmt.Errorf("attachment %q declared %d bytes but bundle contained %d",
			attachment.LocalID, attachment.ByteSize, written)
	}
	actualHash := hex.EncodeToString(hash.Sum(nil))
	if actualHash != attachment.ContentSHA256 {
		return fmt.Errorf("attachment %q SHA-256 mismatch: declared %s, actual %s",
			attachment.LocalID, attachment.ContentSHA256, actualHash)
	}
	return nil
}

func (p *Persister) makeDisplayVariants(
	ctx context.Context,
	parsed *document.ParsedDocument,
	asset document.ExtractedAsset,
) (DisplayVariants, error) {
	if !SafeRasterSupported(asset.SourceMIME) {
		return DisplayVariants{}, fmt.Errorf("%w: %s", ErrUnsupportedRaster, asset.SourceMIME)
	}
	reader, err := parsed.OpenBundleEntry(ctx, asset.BundleEntry)
	if err != nil {
		return DisplayVariants{}, fmt.Errorf("open asset %q for safe display: %w", asset.LocalID, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: reader}, p.Limits.MaxAssetBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return DisplayVariants{}, fmt.Errorf("read asset %q for safe display: %w", asset.LocalID, readErr)
	}
	if closeErr != nil {
		return DisplayVariants{}, fmt.Errorf("close asset %q after safe display read: %w", asset.LocalID, closeErr)
	}
	if int64(len(raw)) != asset.ByteSize {
		return DisplayVariants{}, fmt.Errorf("asset %q changed while generating safe display", asset.LocalID)
	}
	return MakeDisplayVariants(ctx, raw, asset.SourceMIME, SafeImageLimits{
		MaxSourceBytes:   p.Limits.MaxAssetBytes,
		MaxPixels:        p.Limits.MaxImagePixels,
		DisplayMaxEdge:   p.Limits.DisplayMaxEdge,
		ThumbnailMaxEdge: p.Limits.ThumbnailMaxEdge,
	})
}

func (p *Persister) storeDisplayVariant(
	ctx context.Context,
	request PersistRequest,
	assetID, objectKind string,
	key string,
	variant EncodedRaster,
	localID, variantName string,
) error {
	fence, err := p.Catalog.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: request.UserID, KBID: request.KBID, DocID: request.DocID,
		ObjectKind: objectKind, ObjectKey: key, ReferenceKey: assetID,
	})
	if err != nil {
		return fmt.Errorf("stage asset %q %s: %w", localID, variantName, err)
	}
	exists, err := p.objectExists(ctx, key)
	if err != nil {
		return fmt.Errorf("check asset %q %s object: %w", localID, variantName, err)
	}
	if !exists {
		if err := p.Objects.Put(ctx, key, bytes.NewReader(variant.Bytes), int64(len(variant.Bytes)), variant.MIMEType); err != nil {
			return fmt.Errorf("store asset %q %s: %w", localID, variantName, err)
		}
	}
	if ready, err := p.Catalog.MarkRAGObjectWriteReady(ctx, *fence); err != nil {
		return fmt.Errorf("mark asset %q %s ready: %w", localID, variantName, err)
	} else if !ready {
		return fmt.Errorf("mark asset %q %s ready: %w", localID, variantName, store.ErrRAGLifecycleInactive)
	}
	return nil
}

func appendDisplayWarning(
	artifact *document.ParsedArtifact,
	asset document.ExtractedAsset,
	parsed *document.ParsedDocument,
	message string,
) {
	var location *document.SourceLocation
	for _, occurrence := range parsed.Occurrences {
		if occurrence.AssetLocalID != asset.LocalID {
			continue
		}
		value := occurrence.Location
		location = &value
		break
	}
	artifact.Warnings = append(artifact.Warnings, document.ParseWarning{
		Code: "asset_display_unavailable", Message: message, Location: location, Degraded: true,
	})
}

// LoadParsedArtifact treats a missing/corrupt artifact or any missing binary
// dependency as a cache miss. Catalog query failures remain observable errors.
func (p *Persister) LoadParsedArtifact(ctx context.Context, request CacheRequest) (*document.ParsedArtifact, bool, error) {
	if err := p.validate(); err != nil {
		return nil, false, err
	}
	artifactKey := strings.TrimSpace(request.ArtifactObjectKey)
	if artifactKey == "" {
		var err error
		artifactKey, err = document.ArtifactJSONKey(request.UserID, request.KBID, request.DocID, request.ParseFingerprint)
		if err != nil {
			return nil, false, err
		}
		if request.DocVersion > 0 {
			artifactKey, err = document.VersionedObjectKey(artifactKey, request.DocVersion)
			if err != nil {
				return nil, false, err
			}
		}
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
	if request.ExpectedSource != nil && artifact.Source != *request.ExpectedSource {
		return nil, false, nil
	}
	ids := make([]string, 0, len(artifact.Assets))
	attachmentIDs := make([]string, 0, len(artifact.Attachments))
	recordsToTouch := make([]store.RAGAssetRecord, 0, len(artifact.Assets))
	recordsToPublish := make([]store.RAGAssetRecord, 0, len(artifact.Assets))
	attachmentRecordsToTouch := make([]store.RAGAttachmentRecord, 0, len(artifact.Attachments))
	attachmentRecordsToPublish := make([]store.RAGAttachmentRecord, 0, len(artifact.Attachments))
	for _, asset := range artifact.Assets {
		ids = append(ids, asset.ID)
	}
	for _, attachment := range artifact.Attachments {
		attachmentIDs = append(attachmentIDs, attachment.ID)
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
			for _, dependency := range []struct{ name, key string }{
				{name: "display", key: record.DisplayObjectKey},
				{name: "thumbnail", key: record.ThumbnailObjectKey},
			} {
				exists, err := p.objectExists(ctx, dependency.key)
				if err != nil {
					return nil, false, fmt.Errorf("check %s asset %q: %w", dependency.name, record.ID, err)
				}
				if !exists {
					return nil, false, nil
				}
			}
		}
		if request.DocVersion > 0 &&
			(request.DocVersion < record.FirstSeenVersion || request.DocVersion > record.LastSeenVersion) {
			if request.DocVersion < record.FirstSeenVersion {
				record.FirstSeenVersion = request.DocVersion
			}
			if request.DocVersion > record.LastSeenVersion {
				record.LastSeenVersion = request.DocVersion
			}
			recordsToTouch = append(recordsToTouch, record)
		}
		recordsToPublish = append(recordsToPublish, record)
	}
	attachmentRecords, err := p.Catalog.ListRAGAttachmentsByIDs(ctx, attachmentIDs)
	if err != nil {
		return nil, false, fmt.Errorf("rehydrate attachment catalog: %w", err)
	}
	if len(attachmentRecords) != len(attachmentIDs) {
		return nil, false, nil
	}
	attachmentByID := make(map[string]store.RAGAttachmentRecord, len(attachmentRecords))
	for _, record := range attachmentRecords {
		attachmentByID[record.ID] = record
	}
	for _, attachment := range artifact.Attachments {
		record, ok := attachmentByID[attachment.ID]
		if !ok {
			return nil, false, nil
		}
		logicalKey, err := document.AttachmentSourceKey(
			request.UserID, request.KBID, request.DocID, attachment.ContentSHA256)
		if err != nil || !recordMatchesArtifactAttachment(
			record, attachment, request.DocID, logicalKey) {
			return nil, false, nil
		}
		exists, err := p.objectExists(ctx, record.ObjectKey)
		if err != nil {
			return nil, false, fmt.Errorf("check attachment %q: %w", record.ID, err)
		}
		if !exists {
			return nil, false, nil
		}
		if request.DocVersion > 0 &&
			(request.DocVersion < record.FirstSeenVersion ||
				request.DocVersion > record.LastSeenVersion) {
			if request.DocVersion < record.FirstSeenVersion {
				record.FirstSeenVersion = request.DocVersion
			}
			if request.DocVersion > record.LastSeenVersion {
				record.LastSeenVersion = request.DocVersion
			}
			attachmentRecordsToTouch = append(attachmentRecordsToTouch, record)
		}
		attachmentRecordsToPublish = append(attachmentRecordsToPublish, record)
	}
	// Version visibility is advanced only after the artifact and every binary
	// dependency have passed validation. A corrupt cache must never make an
	// otherwise inactive asset visible to a newly targeted version.
	if request.IndexFence != nil {
		published, err := p.Catalog.PublishRAGAssetsAndAttachmentsForIndex(
			ctx, *request.IndexFence, recordsToPublish, ids,
			attachmentRecordsToPublish, attachmentIDs)
		if err != nil {
			return nil, false, fmt.Errorf("publish cached version %d asset set: %w", request.DocVersion, err)
		}
		if !published {
			return nil, false, store.ErrRAGLifecycleInactive
		}
	} else {
		for i := range recordsToTouch {
			record := &recordsToTouch[i]
			if err := p.Catalog.UpsertRAGAsset(ctx, record); err != nil {
				return nil, false, fmt.Errorf("mark cached asset %q seen in version %d: %w",
					record.ID, request.DocVersion, err)
			}
		}
		for i := range attachmentRecordsToTouch {
			record := &attachmentRecordsToTouch[i]
			if err := p.Catalog.UpsertRAGAttachment(ctx, record); err != nil {
				return nil, false, fmt.Errorf(
					"mark cached attachment %q seen in version %d: %w",
					record.ID, request.DocVersion, err)
			}
		}
		if request.DocVersion > 0 {
			if err := p.Catalog.ReplaceRAGVersionAssets(ctx, request.DocID, request.DocVersion, ids); err != nil {
				return nil, false, fmt.Errorf("record cached version %d asset set: %w", request.DocVersion, err)
			}
			if err := p.Catalog.ReplaceRAGVersionAttachments(
				ctx, request.DocID, request.DocVersion, attachmentIDs); err != nil {
				return nil, false, fmt.Errorf(
					"record cached version %d attachment set: %w", request.DocVersion, err)
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

func (p *Persister) requireExistingAssetObjects(ctx context.Context, asset store.RAGAssetRecord) error {
	dependencies := []struct {
		name string
		key  string
	}{{name: "source", key: asset.SourceObjectKey}}
	if asset.DisplayStatus == document.DisplayReady {
		dependencies = append(dependencies,
			struct {
				name string
				key  string
			}{name: "display", key: asset.DisplayObjectKey},
			struct {
				name string
				key  string
			}{name: "thumbnail", key: asset.ThumbnailObjectKey},
		)
	}
	for _, dependency := range dependencies {
		exists, err := p.objectExists(ctx, dependency.key)
		if err != nil {
			return fmt.Errorf("check %s object: %w", dependency.name, err)
		}
		if !exists {
			return fmt.Errorf("published %s object is missing", dependency.name)
		}
	}
	return nil
}

func validateExistingAsset(existing, expected store.RAGAssetRecord, logicalKeys document.ObjectKeys) error {
	if existing.ID != expected.ID || existing.DocID != expected.DocID || existing.ContentSHA256 != expected.ContentSHA256 ||
		existing.SourceKind != expected.SourceKind || existing.SourceMIME != expected.SourceMIME ||
		!matchesGenerationObjectKey(existing.SourceObjectKey, logicalKeys.AssetSource) || existing.ByteSize != expected.ByteSize ||
		existing.Width != expected.Width || existing.Height != expected.Height {
		return fmt.Errorf("existing asset %q conflicts with canonical source description", expected.ID)
	}
	if existing.DisplayStatus != document.DisplayReady && existing.DisplayStatus != document.DisplayUnavailable {
		return fmt.Errorf("existing asset %q has invalid display status %q", expected.ID, existing.DisplayStatus)
	}
	if existing.DisplayStatus == document.DisplayReady &&
		(!matchesGenerationObjectKey(existing.DisplayObjectKey, logicalKeys.AssetDisplay) ||
			!matchesGenerationObjectKey(existing.ThumbnailObjectKey, logicalKeys.AssetThumbnail)) {
		return fmt.Errorf("existing ready asset %q has non-canonical display keys", expected.ID)
	}
	return nil
}

func validateExistingAttachment(
	existing, expected store.RAGAttachmentRecord,
	logicalKey string,
) error {
	if existing.ID != expected.ID || existing.DocID != expected.DocID ||
		existing.ContentSHA256 != expected.ContentSHA256 ||
		existing.Kind != expected.Kind || existing.FileName != expected.FileName ||
		existing.MIMEType != expected.MIMEType ||
		!matchesGenerationObjectKey(existing.ObjectKey, logicalKey) ||
		existing.ByteSize != expected.ByteSize {
		return fmt.Errorf("existing attachment %q conflicts with canonical source description", expected.ID)
	}
	return nil
}

func recordMatchesArtifact(record store.RAGAssetRecord, asset document.ArtifactAsset, docID string, keys document.ObjectKeys) bool {
	if record.ID != asset.ID || record.DocID != docID || record.ContentSHA256 != asset.ContentSHA256 ||
		record.SourceKind != asset.SourceKind || record.SourceMIME != asset.SourceMIME ||
		!matchesGenerationObjectKey(record.SourceObjectKey, keys.AssetSource) || record.ByteSize != asset.ByteSize ||
		record.Width != asset.Width || record.Height != asset.Height || record.DisplayStatus != asset.DisplayStatus {
		return false
	}
	if record.DisplayStatus == document.DisplayReady {
		return matchesGenerationObjectKey(record.DisplayObjectKey, keys.AssetDisplay) &&
			matchesGenerationObjectKey(record.ThumbnailObjectKey, keys.AssetThumbnail) &&
			document.CanonicalSHA256(record.DisplaySHA256) && document.CanonicalSHA256(record.ThumbnailSHA256)
	}
	return true
}

func recordMatchesArtifactAttachment(
	record store.RAGAttachmentRecord,
	attachment document.ArtifactAttachment,
	docID, logicalKey string,
) bool {
	return record.ID == attachment.ID && record.DocID == docID &&
		record.ContentSHA256 == attachment.ContentSHA256 &&
		record.Kind == attachment.Kind && record.FileName == attachment.FileName &&
		record.MIMEType == attachment.MIMEType &&
		matchesGenerationObjectKey(record.ObjectKey, logicalKey) &&
		record.ByteSize == attachment.ByteSize
}

func matchesGenerationObjectKey(objectKey, logicalKey string) bool {
	if objectKey == logicalKey {
		return true // legacy/canonical pre-generation object
	}
	logicalDir, logicalBase := path.Dir(logicalKey), path.Base(logicalKey)
	objectDir := path.Dir(objectKey)
	return path.Base(objectKey) == logicalBase &&
		strings.HasPrefix(objectDir, logicalDir+"/versions/") &&
		!strings.Contains(strings.TrimPrefix(objectDir, logicalDir+"/versions/"), "/")
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
