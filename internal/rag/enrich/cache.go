package enrich

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
	"github.com/qs3c/bkcrab/internal/store"
)

type SchemaLimits struct {
	MaxJSONDepth  int
	MaxFieldBytes int
	MaxItems      int
	MaxTextBytes  int
}

func DefaultSchemaLimits() SchemaLimits {
	return SchemaLimits{MaxJSONDepth: 32, MaxFieldBytes: 8 << 10, MaxItems: 32, MaxTextBytes: 32 << 10}
}

func (l SchemaLimits) normalized() SchemaLimits {
	defaults := DefaultSchemaLimits()
	if l.MaxJSONDepth <= 0 {
		l.MaxJSONDepth = defaults.MaxJSONDepth
	}
	if l.MaxFieldBytes <= 0 {
		l.MaxFieldBytes = defaults.MaxFieldBytes
	}
	if l.MaxItems <= 0 {
		l.MaxItems = defaults.MaxItems
	}
	if l.MaxTextBytes <= 0 {
		l.MaxTextBytes = defaults.MaxTextBytes
	}
	return l
}

type ResultCache interface {
	Get(context.Context, CacheScope, string, BlockKind) (Enhancement, bool, error)
	Put(context.Context, CacheScope, string, Enhancement) error
}

type cacheEnvelope struct {
	SchemaVersion string      `json:"schemaVersion"`
	Value         Enhancement `json:"value"`
}

type ObjectCache struct {
	store    objects.Store
	catalog  store.RAGCacheCatalog
	limits   SchemaLimits
	maxBytes int64
}

func NewObjectCache(objectStore objects.Store, limits SchemaLimits, catalogs ...store.RAGCacheCatalog) *ObjectCache {
	limits = limits.normalized()
	var catalog store.RAGCacheCatalog
	if len(catalogs) != 0 {
		catalog = catalogs[0]
	}
	return &ObjectCache{store: objectStore, catalog: catalog, limits: limits, maxBytes: cacheObjectByteLimit(limits)}
}

func (c *ObjectCache) Get(ctx context.Context, scope CacheScope, key string, kind BlockKind) (Enhancement, bool, error) {
	if c == nil || c.store == nil || !scope.valid() {
		return Enhancement{}, false, nil
	}
	objectKey, err := document.EnrichmentCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		return Enhancement{}, false, err
	}
	reader, err := c.store.Get(ctx, objectKey)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Enhancement{}, false, nil
		}
		return Enhancement{}, false, err
	}
	defer reader.Close()
	readLimit := c.maxBytes
	if readLimit < math.MaxInt64 {
		readLimit++
	}
	raw, err := io.ReadAll(io.LimitReader(reader, readLimit))
	if err != nil {
		return Enhancement{}, false, err
	}
	if int64(len(raw)) > c.maxBytes {
		return Enhancement{}, false, nil
	}
	var envelope cacheEnvelope
	if err := strictDecode(raw, c.limits.MaxJSONDepth+2, &envelope); err != nil ||
		envelope.SchemaVersion != EnrichmentSchemaVersion || envelope.Value.Kind != kind ||
		envelope.Value.validate(c.limits) != nil {
		return Enhancement{}, false, nil
	}
	if err := c.register(ctx, scope, key, objectKey); err != nil {
		return Enhancement{}, false, err
	}
	return envelope.Value, true, nil
}

func (c *ObjectCache) Put(ctx context.Context, scope CacheScope, key string, value Enhancement) error {
	if c == nil || c.store == nil || !scope.valid() {
		return nil
	}
	if err := value.validate(c.limits); err != nil {
		return err
	}
	objectKey, err := document.EnrichmentCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cacheEnvelope{SchemaVersion: EnrichmentSchemaVersion, Value: value})
	if err != nil {
		return err
	}
	if int64(len(raw)) > c.maxBytes {
		return fmt.Errorf("enrichment cache envelope exceeds %d bytes", c.maxBytes)
	}
	if err := c.register(ctx, scope, key, objectKey); err != nil {
		return err
	}
	return c.store.Put(ctx, objectKey, bytes.NewReader(raw), int64(len(raw)), "application/json")
}

func (c *ObjectCache) register(ctx context.Context, scope CacheScope, cacheKey, objectKey string) error {
	if c == nil || c.catalog == nil {
		return nil
	}
	return c.catalog.RegisterRAGCacheObject(ctx, store.RAGCacheObjectRecord{
		DocID: scope.DocID, CacheKind: store.RAGCacheKindEnrich, CacheKey: cacheKey,
		ObjectKey: objectKey, FingerprintKind: store.RAGCacheFingerprintIndex,
		Fingerprint: scope.IndexFingerprint,
	})
}

func cacheObjectByteLimit(limits SchemaLimits) int64 {
	const (
		// A valid UTF-8 control byte can expand to six JSON bytes (\u00XX).
		jsonExpansion        int64 = 6
		cacheFixedOverhead   int64 = 8 << 10
		cachePerItemOverhead int64 = 512
	)
	text := cacheSaturatingMultiply(int64(limits.MaxTextBytes), jsonExpansion)
	structure := cacheSaturatingAdd(cacheFixedOverhead,
		cacheSaturatingMultiply(int64(limits.MaxItems), cachePerItemOverhead))
	return cacheSaturatingAdd(text, structure)
}

func cacheSaturatingAdd(left, right int64) int64 {
	if left > math.MaxInt64-right {
		return math.MaxInt64
	}
	return left + right
}

func cacheSaturatingMultiply(left, right int64) int64 {
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
	values map[string]Enhancement
}

func NewMemoryCache(limits SchemaLimits) *MemoryCache {
	return &MemoryCache{limits: limits.normalized(), values: make(map[string]Enhancement)}
}

func scopedCacheKey(scope CacheScope, key string) string {
	return scope.UserID + "\x00" + scope.KBID + "\x00" + scope.DocID + "\x00" + key
}

func (c *MemoryCache) Get(_ context.Context, scope CacheScope, key string, kind BlockKind) (Enhancement, bool, error) {
	if c == nil || !scope.valid() {
		return Enhancement{}, false, nil
	}
	c.mu.RLock()
	value, ok := c.values[scopedCacheKey(scope, key)]
	c.mu.RUnlock()
	if !ok || value.Kind != kind || value.validate(c.limits) != nil {
		return Enhancement{}, false, nil
	}
	return value, true, nil
}

func (c *MemoryCache) Put(_ context.Context, scope CacheScope, key string, value Enhancement) error {
	if c == nil || !scope.valid() {
		return nil
	}
	if err := value.validate(c.limits); err != nil {
		return err
	}
	c.mu.Lock()
	c.values[scopedCacheKey(scope, key)] = value
	c.mu.Unlock()
	return nil
}

func strictDecode(raw []byte, maxDepth int, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("empty JSON")
	}
	if err := validateJSONDepth(raw, maxDepth); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validateJSONDepth(raw []byte, maxDepth int) error {
	if maxDepth <= 0 {
		return errors.New("invalid maximum JSON depth")
	}
	inString, escaped, depth := false, false, 0
	for _, b := range raw {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return fmt.Errorf("JSON depth exceeds %d", maxDepth)
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return errors.New("unbalanced JSON")
			}
		}
	}
	if inString || depth != 0 {
		return errors.New("incomplete JSON")
	}
	return nil
}

var _ ResultCache = (*ObjectCache)(nil)
var _ ResultCache = (*MemoryCache)(nil)
