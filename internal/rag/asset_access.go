package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/store"
)

const assetRendererVersion = "safe-raster-v1"

var canonicalAssetID = regexp.MustCompile(`^ast_[0-9a-f]{32}$`)

// AssetVariant identifies one of the two safe, derived raster objects exposed
// by the owner-only RAG asset endpoints. Source objects are deliberately not a
// variant: uploaded/embedded bytes can use unsafe formats and are never served.
type AssetVariant string

const (
	AssetDisplay   AssetVariant = "display"
	AssetThumbnail AssetVariant = "thumbnail"
)

// AuthorizedAsset contains only response-safe metadata. The object key and
// authorization inputs stay private to this package, but the descriptor can be
// passed back to OpenAuthorizedAsset after conditional-request handling.
type AuthorizedAsset struct {
	MIMEType string
	ETag     string

	assetID string
	ownerID string
	variant AssetVariant
}

// AuthorizeAsset resolves asset -> document -> knowledge base -> user and
// checks every current-state tombstone before exposing cache metadata. Unknown
// and cross-tenant assets intentionally share ErrNotFound semantics.
func (s *Service) AuthorizeAsset(ctx context.Context, ownerID, assetID string, variant AssetVariant) (*AuthorizedAsset, error) {
	resolved, err := s.authorizeAsset(ctx, ownerID, assetID, variant, false)
	if err != nil {
		return nil, err
	}
	return resolved.descriptor, nil
}

// OpenAuthorizedAsset repeats the current-state authorization and opens the
// safe derived object while the document/KB mutation locks are held. This keeps
// conditional 304 handling free of object reads without trusting a stale
// descriptor when a deletion starts between authorization and streaming.
func (s *Service) OpenAuthorizedAsset(ctx context.Context, descriptor *AuthorizedAsset) (io.ReadCloser, error) {
	if descriptor == nil {
		return nil, ErrNotFound
	}
	resolved, err := s.authorizeAsset(ctx, descriptor.ownerID, descriptor.assetID, descriptor.variant, true)
	if err != nil {
		return nil, err
	}
	if resolved.descriptor.ETag != descriptor.ETag || resolved.descriptor.MIMEType != descriptor.MIMEType {
		return nil, ErrNotFound
	}
	return resolved.reader, nil
}

type resolvedAsset struct {
	descriptor *AuthorizedAsset
	reader     io.ReadCloser
}

func (s *Service) authorizeAsset(
	ctx context.Context,
	ownerID, assetID string,
	variant AssetVariant,
	openObject bool,
) (*resolvedAsset, error) {
	if s == nil || s.st == nil || s.obj == nil || !canonicalAssetID.MatchString(assetID) {
		return nil, ErrNotFound
	}
	if variant != AssetDisplay && variant != AssetThumbnail {
		return nil, ErrNotFound
	}

	// Resolve the lock identities without trusting them for authorization. Every
	// row is re-read after acquiring the locks in the same KB -> document order
	// used by deletion and indexing mutations.
	asset, err := s.st.GetRAGAsset(ctx, assetID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	doc, err := s.st.GetRAGDocument(ctx, asset.DocID)
	if err != nil {
		return nil, assetLookupError(err)
	}

	kbLock := s.kbMutex(doc.KBID)
	kbLock.RLock()
	defer kbLock.RUnlock()
	docLock := s.docMutex(doc.ID)
	docLock.Lock()
	defer docLock.Unlock()

	asset, err = s.st.GetRAGAsset(ctx, assetID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	doc, err = s.st.GetRAGDocument(ctx, asset.DocID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	kb, err := s.st.GetRAGKB(ctx, doc.KBID)
	if err != nil {
		return nil, assetLookupError(err)
	}
	user, err := s.st.GetUser(ctx, kb.UserID)
	if err != nil {
		return nil, assetLookupError(err)
	}

	if ownerID != "" && kb.UserID != ownerID {
		return nil, ErrNotFound
	}
	if !strings.EqualFold(kb.Status, "active") ||
		strings.EqualFold(doc.Status, "deleting") ||
		!strings.EqualFold(user.Status, "active") ||
		!strings.EqualFold(asset.DisplayStatus, "ready") {
		return nil, ErrNotFound
	}
	if !safeDisplayMIME(asset.DisplayMIME) || !canonicalSHA256(asset.DisplaySHA256) ||
		!canonicalSHA256(asset.ThumbnailSHA256) {
		return nil, ErrNotFound
	}

	objectKey := asset.DisplayObjectKey
	derivedSHA256 := asset.DisplaySHA256
	if variant == AssetThumbnail {
		objectKey = asset.ThumbnailObjectKey
		derivedSHA256 = asset.ThumbnailSHA256
	}
	if strings.TrimSpace(objectKey) == "" {
		return nil, ErrNotFound
	}
	descriptor := &AuthorizedAsset{
		MIMEType: asset.DisplayMIME,
		ETag:     derivedAssetETag(derivedSHA256, variant),
		assetID:  asset.ID,
		ownerID:  ownerID,
		variant:  variant,
	}
	if !openObject {
		return &resolvedAsset{descriptor: descriptor}, nil
	}
	reader, err := s.obj.Get(ctx, objectKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		// Object-store errors can include private keys or backend diagnostics.
		// Keep them out of the HTTP error surface; callers only need a typed-safe
		// availability failure here.
		return nil, errors.New("RAG display asset is temporarily unavailable")
	}
	return &resolvedAsset{descriptor: descriptor, reader: reader}, nil
}

func assetLookupError(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func safeDisplayMIME(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "image/png", "image/webp":
		return true
	default:
		return false
	}
}

func canonicalSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func derivedAssetETag(derivedSHA256 string, variant AssetVariant) string {
	// Each validator uses the hash of the exact safe bytes served for its
	// variant, namespaced by renderer version so renderer changes invalidate it.
	return fmt.Sprintf(`"%s-%s-%s"`, variant, assetRendererVersion, derivedSHA256)
}
