package vision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"sync"

	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/objects"
)

type ResultCache interface {
	GetPage(context.Context, CacheScope, string) (PageTranscription, bool, error)
	PutPage(context.Context, CacheScope, string, PageTranscription) error
	GetImage(context.Context, CacheScope, string) (ImageDescription, bool, error)
	PutImage(context.Context, CacheScope, string, ImageDescription) error
}

type cacheEnvelope struct {
	SchemaVersion string          `json:"schemaVersion"`
	Value         json.RawMessage `json:"value"`
}

type ObjectCache struct {
	store         objects.Store
	limits        SchemaLimits
	maxPageBytes  int64
	maxImageBytes int64
}

func NewObjectCache(store objects.Store, limits SchemaLimits) *ObjectCache {
	limits = limits.normalized()
	maxPageBytes, maxImageBytes := cacheObjectByteLimits(limits)
	return &ObjectCache{store: store, limits: limits, maxPageBytes: maxPageBytes, maxImageBytes: maxImageBytes}
}

func (c *ObjectCache) GetPage(ctx context.Context, scope CacheScope, key string) (PageTranscription, bool, error) {
	if c == nil || c.store == nil || scope.empty() {
		return PageTranscription{}, false, nil
	}
	objectKey, err := document.PageCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		return PageTranscription{}, false, err
	}
	raw, ok, err := c.read(ctx, objectKey, c.maxPageBytes)
	if err != nil || !ok {
		return PageTranscription{}, false, err
	}
	envelope, ok := c.decodeEnvelope(raw, PageSchemaVersion)
	if !ok {
		return PageTranscription{}, false, nil
	}
	value, err := DecodePageTranscription(envelope.Value, c.limits)
	if err != nil {
		return PageTranscription{}, false, nil
	}
	return value, true, nil
}

func (c *ObjectCache) PutPage(ctx context.Context, scope CacheScope, key string, value PageTranscription) error {
	if c == nil || c.store == nil || scope.empty() {
		return nil
	}
	if err := value.Validate(c.limits); err != nil {
		return err
	}
	objectKey, err := document.PageCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		return err
	}
	return c.write(ctx, objectKey, PageSchemaVersion, value, c.maxPageBytes)
}

func (c *ObjectCache) GetImage(ctx context.Context, scope CacheScope, key string) (ImageDescription, bool, error) {
	if c == nil || c.store == nil || scope.empty() {
		return ImageDescription{}, false, nil
	}
	objectKey, err := document.ImageCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		return ImageDescription{}, false, err
	}
	raw, ok, err := c.read(ctx, objectKey, c.maxImageBytes)
	if err != nil || !ok {
		return ImageDescription{}, false, err
	}
	envelope, ok := c.decodeEnvelope(raw, ImageDescriptionSchemaVersion)
	if !ok {
		return ImageDescription{}, false, nil
	}
	value, err := DecodeImageDescription(envelope.Value, c.limits)
	if err != nil {
		return ImageDescription{}, false, nil
	}
	return value, true, nil
}

func (c *ObjectCache) PutImage(ctx context.Context, scope CacheScope, key string, value ImageDescription) error {
	if c == nil || c.store == nil || scope.empty() {
		return nil
	}
	if err := value.Validate(c.limits); err != nil {
		return err
	}
	objectKey, err := document.ImageCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		return err
	}
	return c.write(ctx, objectKey, ImageDescriptionSchemaVersion, value, c.maxImageBytes)
}

func (c *ObjectCache) read(ctx context.Context, key string, maxBytes int64) ([]byte, bool, error) {
	reader, err := c.store.Get(ctx, key)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer reader.Close()
	readLimit := maxBytes
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	limited := &io.LimitedReader{R: reader, N: readLimit}
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, false, nil
	}
	return raw, true, nil
}

func (c *ObjectCache) decodeEnvelope(raw []byte, schema string) (cacheEnvelope, bool) {
	var envelope cacheEnvelope
	if err := strictDecode(raw, c.limits.MaxJSONDepth+2, &envelope); err != nil ||
		envelope.SchemaVersion != schema || len(envelope.Value) == 0 {
		return cacheEnvelope{}, false
	}
	return envelope, true
}

func (c *ObjectCache) write(ctx context.Context, key, schema string, value any, maxBytes int64) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cacheEnvelope{SchemaVersion: schema, Value: payload})
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxBytes {
		return fmt.Errorf("vision cache envelope exceeds %d bytes", maxBytes)
	}
	return c.store.Put(ctx, key, bytes.NewReader(raw), int64(len(raw)), "application/json")
}

func cacheObjectByteLimits(limits SchemaLimits) (page, image int64) {
	const (
		jsonExpansion         int64 = 6
		cacheEnvelopeOverhead int64 = 4 << 10
		cacheVisualOverhead   int64 = 512
	)
	descriptionBytes := min(
		int64(limits.MaxDescriptionBytes),
		saturatingAdd(int64(limits.MaxCaptionBytes), int64(limits.MaxOCRBytes)),
	)
	escapedDescription := saturatingMultiply(descriptionBytes, jsonExpansion)
	perVisual := saturatingAdd(escapedDescription, cacheVisualOverhead)
	page = saturatingAdd(cacheEnvelopeOverhead, saturatingMultiply(int64(limits.MaxMarkdownBytes), jsonExpansion))
	page = saturatingAdd(page, saturatingMultiply(int64(limits.MaxVisuals), perVisual))
	image = saturatingAdd(cacheEnvelopeOverhead, perVisual)
	return page, image
}

func saturatingAdd(left, right int64) int64 {
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func saturatingMultiply(left, right int64) int64 {
	if left == 0 || right == 0 {
		return 0
	}
	if left > math.MaxInt64/right {
		return math.MaxInt64
	}
	return left * right
}

type MemoryCache struct {
	mu     sync.RWMutex
	limits SchemaLimits
	pages  map[string]PageTranscription
	images map[string]ImageDescription
}

func NewMemoryCache(limits SchemaLimits) *MemoryCache {
	return &MemoryCache{limits: limits.normalized(), pages: map[string]PageTranscription{}, images: map[string]ImageDescription{}}
}

func memoryCacheKey(scope CacheScope, key string) string {
	return scope.UserID + "\x00" + scope.KBID + "\x00" + scope.DocID + "\x00" + key
}

func (c *MemoryCache) GetPage(_ context.Context, scope CacheScope, key string) (PageTranscription, bool, error) {
	if c == nil || scope.empty() {
		return PageTranscription{}, false, nil
	}
	c.mu.RLock()
	value, ok := c.pages[memoryCacheKey(scope, key)]
	c.mu.RUnlock()
	return value, ok, nil
}
func (c *MemoryCache) PutPage(_ context.Context, scope CacheScope, key string, value PageTranscription) error {
	if c == nil || scope.empty() {
		return nil
	}
	if err := value.Validate(c.limits); err != nil {
		return err
	}
	c.mu.Lock()
	c.pages[memoryCacheKey(scope, key)] = value
	c.mu.Unlock()
	return nil
}
func (c *MemoryCache) GetImage(_ context.Context, scope CacheScope, key string) (ImageDescription, bool, error) {
	if c == nil || scope.empty() {
		return ImageDescription{}, false, nil
	}
	c.mu.RLock()
	value, ok := c.images[memoryCacheKey(scope, key)]
	c.mu.RUnlock()
	return value, ok, nil
}
func (c *MemoryCache) PutImage(_ context.Context, scope CacheScope, key string, value ImageDescription) error {
	if c == nil || scope.empty() {
		return nil
	}
	if err := value.Validate(c.limits); err != nil {
		return err
	}
	c.mu.Lock()
	c.images[memoryCacheKey(scope, key)] = value
	c.mu.Unlock()
	return nil
}
