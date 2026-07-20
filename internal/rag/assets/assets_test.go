package assets

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	assetSourceHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	assetHash       = "054edec1d0211f624fed0cbca9d4f9400b0e491c43742af2c5b0abebf0c990d8"
)

type memoryObjects struct {
	mu       sync.Mutex
	data     map[string][]byte
	putOrder []string
	putErr   map[string]error
	getErr   map[string]error
	getCount map[string]int
}

func newMemoryObjects() *memoryObjects {
	return &memoryObjects{data: map[string][]byte{}, putErr: map[string]error{}, getErr: map[string]error{}, getCount: map[string]int{}}
}

func (m *memoryObjects) Put(ctx context.Context, key string, r io.Reader, size int64, _ string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	err := m.putErr[key]
	m.mu.Unlock()
	if err != nil {
		return err
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size mismatch")
	}
	m.mu.Lock()
	m.data[key] = append([]byte(nil), b...)
	m.putOrder = append(m.putOrder, key)
	m.mu.Unlock()
	return nil
}

func (m *memoryObjects) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCount[key]++
	if err := m.getErr[key]; err != nil {
		return nil, err
	}
	b, ok := m.data[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), b...))), nil
}

func (m *memoryObjects) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.data, key)
	m.mu.Unlock()
	return nil
}

type memoryCatalog struct {
	mu      sync.Mutex
	records map[string]store.RAGAssetRecord
}

func newMemoryCatalog() *memoryCatalog {
	return &memoryCatalog{records: map[string]store.RAGAssetRecord{}}
}

func (m *memoryCatalog) UpsertRAGAsset(_ context.Context, record *store.RAGAssetRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.records[record.ID]; ok {
		if existing.DocID != record.DocID || existing.ContentSHA256 != record.ContentSHA256 ||
			existing.SourceObjectKey != record.SourceObjectKey || existing.ByteSize != record.ByteSize {
			return store.ErrRAGAssetConflict
		}
	}
	m.records[record.ID] = *record
	return nil
}

func (m *memoryCatalog) ListRAGAssetsByIDs(_ context.Context, ids []string) ([]store.RAGAssetRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.RAGAssetRecord, 0, len(ids))
	for _, id := range ids {
		if record, ok := m.records[id]; ok {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func parsedDocument(t *testing.T, cleanup *atomic.Int32) *document.ParsedDocument {
	t.Helper()
	bbox := document.NormalizedBBox{10, 20, 900, 950}
	input := document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source: document.ParsedSource{
			DocID: "doc_1", FileName: "guide.pdf", Format: "pdf", ByteSize: 4, SHA256: assetSourceHash,
		},
		Parser: document.ParserInfo{Name: "fake", Version: "v1"},
		Units: []document.MarkdownUnit{{
			ID: "unit_page_0001", Location: document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "第 1 页"},
			Markdown: "正文\n\n![图](rag-asset://occ_1)\n\n![图又出现](rag-asset://occ_2)",
		}},
		Assets: []document.ExtractedAsset{{
			LocalID: "asset_local", ContentSHA256: assetHash, Kind: document.AssetKindImage,
			SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: "image/png",
			Width: 2, Height: 2, ByteSize: 4, BundleEntry: "assets/image.png",
		}},
		Occurrences: []document.AssetOccurrence{
			{ID: "occ_1", AssetLocalID: "asset_local", UnitID: "unit_page_0001", Order: 1,
				Location: document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "第 1 页"},
				BBox:     &bbox, AltText: "安全替代文字", Confidence: 1},
			{ID: "occ_2", AssetLocalID: "asset_local", UnitID: "unit_page_0001", Order: 2,
				Location: document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "第 1 页"},
				Caption:  "模型图像说明", OCRText: "A → B", Confidence: .9},
		},
	}
	return document.NewParsedDocument(input, func(ctx context.Context, entry string) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry != "assets/image.png" {
			return nil, os.ErrNotExist
		}
		return io.NopCloser(bytes.NewReader([]byte{0, 1, 2, 3})), nil
	}, func() error {
		cleanup.Add(1)
		return nil
	})
}

func testPersister(objects *memoryObjects, catalog *memoryCatalog) *Persister {
	return &Persister{
		Objects: objects,
		Catalog: catalog,
		Limits: Limits{MaxAssets: 10, MaxAssetBytes: 1 << 20, MaxExtractedBytes: 2 << 20,
			MaxImagePixels: 40_000_000, MaxArtifactBytes: 1 << 20},
	}
}

func TestPersistParsedDocumentStreamsCanonicalizesAndPublishesArtifactLast(t *testing.T) {
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)
	var cleanup atomic.Int32
	doc := parsedDocument(t, &cleanup)

	artifact, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 7,
		ParseFingerprint: assetSourceHash, Document: doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cleanup.Load() != 1 {
		t.Fatalf("cleanup calls = %d", cleanup.Load())
	}
	if len(artifact.Assets) != 1 || len(artifact.Occurrences) != 2 ||
		artifact.Occurrences[0].AssetID != artifact.Assets[0].ID ||
		artifact.Occurrences[1].AssetID != artifact.Assets[0].ID {
		t.Fatalf("artifact not canonicalized: %+v", artifact)
	}
	if artifact.Occurrences[0].Caption != "安全替代文字" || artifact.Occurrences[1].Caption != "模型图像说明" {
		t.Fatalf("caption fallback drift: %+v", artifact.Occurrences)
	}
	if len(objects.putOrder) != 3 {
		t.Fatalf("put order = %v", objects.putOrder)
	}
	if !strings.HasSuffix(objects.putOrder[0], "/source.png") ||
		!strings.HasSuffix(objects.putOrder[1], "/normalized.md") ||
		!strings.HasSuffix(objects.putOrder[2], "/parsed.json") {
		t.Fatalf("artifact was not published last: %v", objects.putOrder)
	}
	record := catalog.records[artifact.Assets[0].ID]
	if record.DocID != "doc_1" || record.FirstSeenVersion != 7 || record.LastSeenVersion != 7 ||
		record.SourceObjectKey != objects.putOrder[0] || record.DisplayStatus != document.DisplayUnavailable {
		t.Fatalf("catalog record = %+v", record)
	}
	for key := range objects.data {
		if strings.Contains(key, "asset_local") {
			t.Fatalf("transient local ID leaked into object key %q", key)
		}
	}
}

func TestPersistParsedDocumentReindexReusesStableAssetObject(t *testing.T) {
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)
	var cleanup1, cleanup2 atomic.Int32
	first, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 7,
		ParseFingerprint: assetSourceHash, Document: parsedDocument(t, &cleanup1),
	})
	if err != nil {
		t.Fatal(err)
	}
	secondFingerprint := strings.Repeat("e", 64)
	second, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 8,
		ParseFingerprint: secondFingerprint, Document: parsedDocument(t, &cleanup2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Assets[0].ID != second.Assets[0].ID {
		t.Fatalf("asset ID changed across reindex: %q != %q", first.Assets[0].ID, second.Assets[0].ID)
	}
	var sourcePuts int
	for _, key := range objects.putOrder {
		if strings.HasSuffix(key, "/source.png") {
			sourcePuts++
		}
	}
	if sourcePuts != 1 {
		t.Fatalf("canonical source object rewritten %d times", sourcePuts)
	}
	record := catalog.records[first.Assets[0].ID]
	if record.FirstSeenVersion != 7 || record.LastSeenVersion != 8 {
		t.Fatalf("stable asset version visibility = %d..%d, want 7..8",
			record.FirstSeenVersion, record.LastSeenVersion)
	}
}

func TestPersistFailureOrCancelCleansHandleButKeepsCanonicalStaging(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*memoryObjects, context.CancelFunc, string)
	}{
		{"artifact failure", func(objects *memoryObjects, _ context.CancelFunc, artifactKey string) {
			objects.putErr[artifactKey] = errors.New("publish failed")
		}},
		{"cancel", func(_ *memoryObjects, cancel context.CancelFunc, _ string) { cancel() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := newMemoryObjects()
			catalog := newMemoryCatalog()
			persister := testPersister(objects, catalog)
			var cleanup atomic.Int32
			doc := parsedDocument(t, &cleanup)
			artifactKey, err := document.ArtifactJSONKey("user_1", "kb_1", "doc_1", assetSourceHash)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tt.setup(objects, cancel, artifactKey)
			_, err = persister.PersistParsedDocument(ctx, PersistRequest{
				UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 7,
				ParseFingerprint: assetSourceHash, Document: doc,
			})
			if err == nil {
				t.Fatal("persistence unexpectedly succeeded")
			}
			if cleanup.Load() != 1 {
				t.Fatalf("cleanup calls = %d", cleanup.Load())
			}
			if tt.name == "artifact failure" {
				if len(catalog.records) != 1 {
					t.Fatalf("canonical staging catalog row was deleted: %+v", catalog.records)
				}
				for key := range objects.data {
					if strings.HasSuffix(key, "/parsed.json") {
						t.Fatalf("failed artifact was published: %q", key)
					}
				}
			}
		})
	}
}

func TestPersistRejectsHashSizeAndLimitViolations(t *testing.T) {
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)

	t.Run("hash mismatch", func(t *testing.T) {
		var cleanup atomic.Int32
		doc := parsedDocument(t, &cleanup)
		doc.Assets[0].ContentSHA256 = strings.Repeat("c", 64)
		if _, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
			UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 1,
			ParseFingerprint: assetSourceHash, Document: doc,
		}); err == nil {
			t.Fatal("hash mismatch accepted")
		}
	})
	t.Run("asset limit", func(t *testing.T) {
		var cleanup atomic.Int32
		doc := parsedDocument(t, &cleanup)
		limited := testPersister(newMemoryObjects(), newMemoryCatalog())
		limited.Limits.MaxAssetBytes = 3
		if _, err := limited.PersistParsedDocument(context.Background(), PersistRequest{
			UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 1,
			ParseFingerprint: assetSourceHash, Document: doc,
		}); err == nil {
			t.Fatal("oversized asset accepted")
		}
	})
}

func TestLoadParsedArtifactRehydratesAndInvalidatesMissingDependencies(t *testing.T) {
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)
	var cleanup atomic.Int32
	artifact, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 7,
		ParseFingerprint: assetSourceHash, Document: parsedDocument(t, &cleanup),
	})
	if err != nil {
		t.Fatal(err)
	}

	loaded, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", ParseFingerprint: assetSourceHash,
	})
	if err != nil || !hit || loaded.Assets[0].ID != artifact.Assets[0].ID {
		t.Fatalf("cache load hit=%v artifact=%+v err=%v", hit, loaded, err)
	}
	wrongSource := artifact.Source
	wrongSource.SHA256 = strings.Repeat("b", 64)
	if _, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 8,
		ParseFingerprint: assetSourceHash, ExpectedSource: &wrongSource,
	}); err != nil || hit {
		t.Fatalf("source-mismatched artifact hit=%v err=%v", hit, err)
	}
	if record := catalog.records[artifact.Assets[0].ID]; record.LastSeenVersion != 7 {
		t.Fatalf("source-mismatched cache advanced asset visibility: %+v", record)
	}
	expectedSource := artifact.Source
	if _, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 8,
		ParseFingerprint: assetSourceHash, ExpectedSource: &expectedSource,
	}); err != nil || !hit {
		t.Fatalf("valid version-8 cache hit=%v err=%v", hit, err)
	}
	if record := catalog.records[artifact.Assets[0].ID]; record.FirstSeenVersion != 7 || record.LastSeenVersion != 8 {
		t.Fatalf("cache-hit asset version visibility = %+v", record)
	}

	record := catalog.records[artifact.Assets[0].ID]
	delete(catalog.records, artifact.Assets[0].ID)
	if _, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", ParseFingerprint: assetSourceHash,
	}); err != nil || hit {
		t.Fatalf("missing catalog hit=%v err=%v", hit, err)
	}
	catalog.records[record.ID] = record
	delete(objects.data, record.SourceObjectKey)
	if _, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", ParseFingerprint: assetSourceHash,
	}); err != nil || hit {
		t.Fatalf("missing source hit=%v err=%v", hit, err)
	}

	objects.data[record.SourceObjectKey] = []byte{0, 1, 2, 3}
	record.DisplayStatus = document.DisplayReady
	record.DisplayMIME = "image/webp"
	record.DisplaySHA256 = strings.Repeat("d", 64)
	record.ThumbnailSHA256 = strings.Repeat("e", 64)
	record.DisplayObjectKey = strings.TrimSuffix(record.SourceObjectKey, "/source.png") + "/display.webp"
	record.ThumbnailObjectKey = strings.TrimSuffix(record.SourceObjectKey, "/source.png") + "/thumbnail.webp"
	catalog.records[record.ID] = record
	// Artifact and catalog must agree before object existence is checked.
	artifact.Assets[0].DisplayStatus = document.DisplayReady
	encoded, err := document.EncodeArtifact(artifact, persister.Limits.MaxArtifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	artifactKey, _ := document.ArtifactJSONKey("user_1", "kb_1", "doc_1", assetSourceHash)
	objects.data[artifactKey] = encoded
	if _, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", ParseFingerprint: assetSourceHash,
	}); err != nil || hit {
		t.Fatalf("ready asset without display object hit=%v err=%v", hit, err)
	}
}

func TestLoadParsedArtifactDoesNotTurnStorageOutageIntoCacheMiss(t *testing.T) {
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)
	var cleanup atomic.Int32
	if _, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 7,
		ParseFingerprint: assetSourceHash, Document: parsedDocument(t, &cleanup),
	}); err != nil {
		t.Fatal(err)
	}
	artifactKey, _ := document.ArtifactJSONKey("user_1", "kb_1", "doc_1", assetSourceHash)
	objects.getErr[artifactKey] = errors.New("object store unavailable")
	if _, hit, err := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", ParseFingerprint: assetSourceHash,
	}); err == nil || hit {
		t.Fatalf("storage outage was hidden as cache miss: hit=%v err=%v", hit, err)
	}
}
