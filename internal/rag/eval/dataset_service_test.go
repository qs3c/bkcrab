package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

type probeReader struct {
	remaining int64
	maxRead   int
}

type failOnceDeletePrefixStore struct {
	objects.Store
	failed bool
}

func (s *failOnceDeletePrefixStore) DeletePrefix(ctx context.Context, prefix string) error {
	if !s.failed {
		s.failed = true
		return errors.New("injected object deletion interruption")
	}
	return s.Store.DeletePrefix(ctx, prefix)
}

func (r *probeReader) Read(buffer []byte) (int, error) {
	if len(buffer) > r.maxRead {
		return 0, errors.New("consumer requested an unbounded read buffer")
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	for index := range buffer {
		buffer[index] = 'x'
	}
	r.remaining -= int64(len(buffer))
	return len(buffer), nil
}

type memoryDatasetStore struct {
	mu         sync.Mutex
	versions   map[string]*store.RAGEvalDatasetVersionRecord
	docs       []store.RAGEvalCorpusDocumentRecord
	cases      []store.RAGEvalCaseRecord
	tombstoned map[string]bool
}

func newMemoryDatasetStore() *memoryDatasetStore {
	return &memoryDatasetStore{versions: map[string]*store.RAGEvalDatasetVersionRecord{}, tombstoned: map[string]bool{}}
}
func (s *memoryDatasetStore) TombstoneRAGEvalDataset(_ context.Context, id string) (bool, error) {
	s.tombstoned[id] = true
	return true, nil
}
func (s *memoryDatasetStore) ListRAGEvalDatasetStagingCandidates(_ context.Context, before time.Time, limit int) ([]store.RAGEvalDatasetVersionRecord, error) {
	items := []store.RAGEvalDatasetVersionRecord{}
	for _, version := range s.versions {
		if len(items) == limit {
			break
		}
		if (version.Status == store.RAGEvalDatasetDraft || version.Status == store.RAGEvalDatasetFailed || version.Status == store.RAGEvalDatasetReady) && version.CreatedAt.Before(before) {
			items = append(items, *version)
		}
	}
	return items, nil
}
func (s *memoryDatasetStore) ListRAGEvalDatasetGCCandidates(_ context.Context, _ time.Time, limit int) ([]string, error) {
	items := []string{}
	for id := range s.tombstoned {
		items = append(items, id)
		if len(items) == limit {
			break
		}
	}
	return items, nil
}
func (s *memoryDatasetStore) PurgeRAGEvalDataset(_ context.Context, id string) (bool, error) {
	if !s.tombstoned[id] {
		return false, nil
	}
	delete(s.tombstoned, id)
	for versionID, version := range s.versions {
		if version.DatasetID == id {
			delete(s.versions, versionID)
		}
	}
	return true, nil
}

func (s *memoryDatasetStore) CreateRAGEvalDatasetVersion(_ context.Context, record *store.RAGEvalDatasetVersionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	copy := *record
	s.versions[record.ID] = &copy
	return nil
}
func (s *memoryDatasetStore) TransitionRAGEvalDatasetVersion(_ context.Context, id, from, to, report string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version := s.versions[id]
	if version == nil || version.Status != from {
		return false, nil
	}
	version.Status, version.ValidationReportJSON = to, report
	return true, nil
}
func (s *memoryDatasetStore) PutRAGEvalCorpusDocument(_ context.Context, record *store.RAGEvalCorpusDocumentRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[record.DatasetVersionID].Status != store.RAGEvalDatasetDraft {
		return store.ErrRAGEvalImmutable
	}
	if record.ID == "" {
		record.ID = "doc-row-" + record.ExternalID
	}
	s.docs = append(s.docs, *record)
	return nil
}
func (s *memoryDatasetStore) PutRAGEvalCase(_ context.Context, record *store.RAGEvalCaseRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions[record.DatasetVersionID].Status != store.RAGEvalDatasetDraft {
		return store.ErrRAGEvalImmutable
	}
	if record.ID == "" {
		record.ID = "case-row-" + record.ExternalID
	}
	s.cases = append(s.cases, *record)
	return nil
}
func (s *memoryDatasetStore) ListRAGEvalCorpusDocuments(_ context.Context, versionID, cursor string, limit int) ([]store.RAGEvalCorpusDocumentRecord, error) {
	out := []store.RAGEvalCorpusDocumentRecord{}
	for _, item := range s.docs {
		if item.DatasetVersionID == versionID && item.ID > cursor {
			out = append(out, item)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}
func (s *memoryDatasetStore) ListRAGEvalCases(_ context.Context, versionID, cursor string, limit int) ([]store.RAGEvalCaseRecord, error) {
	out := []store.RAGEvalCaseRecord{}
	for _, item := range s.cases {
		if item.DatasetVersionID == versionID && item.ID > cursor {
			out = append(out, item)
			if len(out) == limit {
				break
			}
		}
	}
	return out, nil
}

func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	file, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func TestDatasetServiceCanonicalGoldenBecomesImmutableReady(t *testing.T) {
	ctx := context.Background()
	datasetStore := newMemoryDatasetStore()
	objectStore := objects.NewLocalFS(t.TempDir())
	service, err := NewDatasetService(datasetStore, objectStore)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.ImportCanonical(ctx, DatasetImportRequest{
		DatasetID: "dataset-1", Version: 1, CreatedBy: "admin", Manifest: fixture(t, "canonical_valid.json"),
		Documents: []DatasetDocumentUpload{{ExternalID: "doc-1", FileName: "guide.txt", MediaType: "text/plain", SizeBytes: 12, Reader: strings.NewReader("hello corpus")}},
	})
	if err != nil || result.Version.Status != store.RAGEvalDatasetReady || !result.Report.Valid {
		t.Fatalf("import result=%+v err=%v", result, err)
	}
	if len(datasetStore.docs) != 1 || len(datasetStore.cases) != 1 || datasetStore.docs[0].ObjectKey == "" {
		t.Fatalf("SQL projection incomplete: docs=%+v cases=%+v", datasetStore.docs, datasetStore.cases)
	}
	reader, err := objectStore.Get(ctx, datasetStore.docs[0].ObjectKey)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	if _, err := objectStore.Get(ctx, "rag-eval/staging/"+result.Version.ID+"/doc-1"); err == nil {
		t.Fatal("staging object survived READY publication")
	}
	if err := datasetStore.PutRAGEvalCase(ctx, &store.RAGEvalCaseRecord{DatasetVersionID: result.Version.ID}); !errors.Is(err, store.ErrRAGEvalImmutable) {
		t.Fatalf("READY version remained mutable: %v", err)
	}
	page, err := service.Preview(ctx, result.Version.ID, "", "", 50)
	if err != nil || len(page.Documents) != 1 || len(page.Cases) != 1 {
		t.Fatalf("preview=%+v err=%v", page, err)
	}
}

func TestDatasetServiceStreamsOriginalWhileHashing(t *testing.T) {
	const size = int64(2 << 20)
	hasher := sha256.New()
	if _, err := io.Copy(hasher, &probeReader{remaining: size, maxRead: 64 << 10}); err != nil {
		t.Fatal(err)
	}
	dataset := CanonicalDataset{Name: "streaming", Corpus: []CorpusDocument{{
		ID: "large-doc", FileName: "large.bin", MediaType: "application/octet-stream",
		SHA256: hex.EncodeToString(hasher.Sum(nil)), SizeBytes: size,
	}}, Cases: []Case{{ID: "case-1", UserInput: "question"}}}
	manifest, err := json.Marshal(dataset)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := NewDatasetService(newMemoryDatasetStore(), objects.NewLocalFS(t.TempDir()))
	result, err := service.ImportCanonical(context.Background(), DatasetImportRequest{
		DatasetID: "dataset-stream", Version: 1, CreatedBy: "admin", Manifest: strings.NewReader(string(manifest)),
		Documents: []DatasetDocumentUpload{{
			ExternalID: "large-doc", FileName: "large.bin", MediaType: "application/octet-stream",
			SizeBytes: size, Reader: &probeReader{remaining: size, maxRead: 64 << 10},
		}},
	})
	if err != nil || result.Version.Status != store.RAGEvalDatasetReady {
		t.Fatalf("streamed import=%+v err=%v", result, err)
	}
}

func TestDatasetServiceRejectsGoldenAndCleansStaging(t *testing.T) {
	datasetStore := newMemoryDatasetStore()
	objectStore := objects.NewLocalFS(t.TempDir())
	service, _ := NewDatasetService(datasetStore, objectStore)
	result, err := service.ImportCanonical(context.Background(), DatasetImportRequest{
		DatasetID: "dataset-1", Version: 1, CreatedBy: "admin", Manifest: fixture(t, "canonical_rejected.json"),
		Documents: []DatasetDocumentUpload{{ExternalID: "doc-1", FileName: "guide.txt", SizeBytes: 12, Reader: strings.NewReader("hello corpus")}},
	})
	if !errors.Is(err, ErrDatasetValidation) || result.Version == nil || datasetStore.versions[result.Version.ID].Status != store.RAGEvalDatasetFailed {
		t.Fatalf("invalid dataset not persisted as FAILED: result=%+v err=%v", result, err)
	}
}

func TestDatasetServiceRejectsTraversalDuplicateAndLimits(t *testing.T) {
	for name, request := range map[string]DatasetImportRequest{
		"traversal":           {DatasetID: "d", Version: 1, CreatedBy: "a", Manifest: fixture(t, "canonical_valid.json"), Documents: []DatasetDocumentUpload{{ExternalID: "doc-1", FileName: "../guide.txt", SizeBytes: 12, Reader: strings.NewReader("hello corpus")}}},
		"duplicate filename":  {DatasetID: "d", Version: 1, CreatedBy: "a", Manifest: fixture(t, "canonical_valid.json"), Documents: []DatasetDocumentUpload{{ExternalID: "doc-1", FileName: "guide.txt"}, {ExternalID: "doc-2", FileName: "GUIDE.TXT"}}},
		"manifest limit":      {DatasetID: "d", Version: 1, CreatedBy: "a", Manifest: strings.NewReader(`{"name":"too large"}`), MaxManifest: 4},
		"archive unsupported": {DatasetID: "d", Version: 1, CreatedBy: "a", Manifest: strings.NewReader("PK\x03\x04not-json")},
	} {
		t.Run(name, func(t *testing.T) {
			service, _ := NewDatasetService(newMemoryDatasetStore(), objects.NewLocalFS(t.TempDir()))
			if _, err := service.ImportCanonical(context.Background(), request); err == nil {
				t.Fatal("unsafe import accepted")
			}
		})
	}
}

func TestDatasetServiceRejectsSHAAndSizeMismatch(t *testing.T) {
	for name, body := range map[string]string{"sha": "hello corpuz", "short": "short", "long": "hello corpus extra"} {
		t.Run(name, func(t *testing.T) {
			service, _ := NewDatasetService(newMemoryDatasetStore(), objects.NewLocalFS(t.TempDir()))
			_, err := service.ImportCanonical(context.Background(), DatasetImportRequest{
				DatasetID: "d", Version: 1, CreatedBy: "a", Manifest: fixture(t, "canonical_valid.json"),
				Documents: []DatasetDocumentUpload{{ExternalID: "doc-1", FileName: "guide.txt", MediaType: "text/plain", SizeBytes: 12, Reader: strings.NewReader(body)}},
			})
			if err == nil {
				t.Fatal("corrupt upload accepted")
			}
		})
	}
}

func TestDatasetServiceTTLStagingCleanupAndTombstoneGC(t *testing.T) {
	ctx := context.Background()
	datasetStore := newMemoryDatasetStore()
	objectStore := objects.NewLocalFS(t.TempDir())
	service, _ := NewDatasetService(datasetStore, objectStore)
	version := &store.RAGEvalDatasetVersionRecord{ID: "version-failed", DatasetID: "dataset-1", Status: store.RAGEvalDatasetFailed, CreatedAt: time.Now().Add(-2 * time.Hour)}
	datasetStore.versions[version.ID] = version
	if err := objectStore.Put(ctx, "rag-eval/staging/version-failed/doc", strings.NewReader("stale"), 5, "text/plain"); err != nil {
		t.Fatal(err)
	}
	cleaned, err := service.CleanupStaging(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || cleaned != 1 {
		t.Fatalf("staging cleanup=%d err=%v", cleaned, err)
	}
	if _, err := objectStore.Get(ctx, "rag-eval/staging/version-failed/doc"); err == nil {
		t.Fatal("stale staging object survived TTL cleanup")
	}
	if err := objectStore.Put(ctx, "rag-eval/datasets/dataset-1/versions/1/manifest.json", strings.NewReader("{}"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}
	if changed, err := service.Tombstone(ctx, "dataset-1"); err != nil || !changed {
		t.Fatalf("tombstone=%v err=%v", changed, err)
	}
	cleaned, err = service.GarbageCollect(ctx, time.Now(), 10)
	if err != nil || cleaned != 1 {
		t.Fatalf("GC=%d err=%v", cleaned, err)
	}
	if _, err := objectStore.Get(ctx, "rag-eval/datasets/dataset-1/versions/1/manifest.json"); err == nil {
		t.Fatal("tombstoned dataset object survived GC")
	}
}

func TestDatasetGCInterruptionKeepsSQLTombstoneForRetry(t *testing.T) {
	ctx := context.Background()
	datasetStore := newMemoryDatasetStore()
	inner := objects.NewLocalFS(t.TempDir())
	objectStore := &failOnceDeletePrefixStore{Store: inner}
	service, _ := NewDatasetService(datasetStore, objectStore)
	if err := inner.Put(ctx, "rag-eval/datasets/dataset-retry/versions/1/manifest.json", strings.NewReader("{}"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}
	if changed, err := service.Tombstone(ctx, "dataset-retry"); err != nil || !changed {
		t.Fatalf("tombstone=%v err=%v", changed, err)
	}
	if cleaned, err := service.GarbageCollect(ctx, time.Now(), 10); err == nil || cleaned != 0 || !datasetStore.tombstoned["dataset-retry"] {
		t.Fatalf("interrupted GC cleaned=%d tombstone=%v err=%v", cleaned, datasetStore.tombstoned["dataset-retry"], err)
	}
	if cleaned, err := service.GarbageCollect(ctx, time.Now(), 10); err != nil || cleaned != 1 || datasetStore.tombstoned["dataset-retry"] {
		t.Fatalf("retry GC cleaned=%d tombstone=%v err=%v", cleaned, datasetStore.tombstoned["dataset-retry"], err)
	}
}
