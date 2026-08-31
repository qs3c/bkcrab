package parseeval

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestParserEvaluationObjectKeys(t *testing.T) {
	tests := map[string]struct {
		got  string
		want string
	}{
		"source": {mustKey(SourceObjectKey("per_run", "ped_doc")), "parser-eval/runs/per_run/documents/ped_doc/source.bin"},
		"truth":  {mustKey(TruthPageObjectKey("per_run", "ped_doc", 1)), "parser-eval/runs/per_run/documents/ped_doc/truth/page-0001.png"},
		"markitdown": {mustKey(MarkdownObjectKey("per_run", "ped_doc", EngineMarkItDown)),
			"parser-eval/runs/per_run/documents/ped_doc/outputs/markitdown.md"},
		"anydoc": {mustKey(MarkdownObjectKey("per_run", "ped_doc", EngineAnyDoc)),
			"parser-eval/runs/per_run/documents/ped_doc/outputs/anydoc.md"},
		"judge markitdown": {mustKey(JudgeRawObjectKey("per_run", "ped_doc", JudgeMarkItDownA)),
			"parser-eval/runs/per_run/documents/ped_doc/judge/markitdown-a.json"},
		"judge anydoc": {mustKey(JudgeRawObjectKey("per_run", "ped_doc", JudgeAnyDocA)),
			"parser-eval/runs/per_run/documents/ped_doc/judge/anydoc-a.json"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if test.got != test.want {
				t.Fatalf("key=%q want=%q", test.got, test.want)
			}
		})
	}
	if _, err := SourceObjectKey("../run", "ped_doc"); err == nil {
		t.Fatal("path-like run ID accepted")
	}
}

func TestOOXMLPreflight(t *testing.T) {
	for _, format := range []Format{FormatDOCX, FormatPPTX, FormatXLSX} {
		t.Run(string(format), func(t *testing.T) {
			data := minimalOOXML(t, format, nil)
			if err := validateOOXMLBytes(data, format, defaultOOXMLLimits()); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("truncated", func(t *testing.T) {
		data := minimalOOXML(t, FormatDOCX, nil)
		if err := validateOOXMLBytes(data[:len(data)/2], FormatDOCX, defaultOOXMLLimits()); err == nil {
			t.Fatal("truncated ZIP accepted")
		}
	})
	t.Run("traversal", func(t *testing.T) {
		data := minimalOOXML(t, FormatDOCX, map[string]string{"../escape": "bad"})
		if err := validateOOXMLBytes(data, FormatDOCX, defaultOOXMLLimits()); err == nil {
			t.Fatal("traversal member accepted")
		}
	})
	t.Run("missing required part", func(t *testing.T) {
		data := zipEntries(t, map[string]string{"[Content_Types].xml": "<Types/>"})
		if err := validateOOXMLBytes(data, FormatDOCX, defaultOOXMLLimits()); err == nil {
			t.Fatal("fake docx accepted")
		}
	})
	t.Run("entry count", func(t *testing.T) {
		data := minimalOOXML(t, FormatDOCX, map[string]string{"extra": "x"})
		limits := defaultOOXMLLimits()
		limits.MaxEntries = 2
		if err := validateOOXMLBytes(data, FormatDOCX, limits); err == nil {
			t.Fatal("entry limit ignored")
		}
	})
	t.Run("expanded bytes", func(t *testing.T) {
		data := minimalOOXML(t, FormatDOCX, map[string]string{"large": strings.Repeat("x", 512)})
		limits := defaultOOXMLLimits()
		limits.MaxTotalUncompressedBytes = 128
		if err := validateOOXMLBytes(data, FormatDOCX, limits); err == nil {
			t.Fatal("expanded byte limit ignored")
		}
	})
	t.Run("compression ratio", func(t *testing.T) {
		data := minimalOOXML(t, FormatDOCX, map[string]string{"bomb": strings.Repeat("x", 4096)})
		limits := defaultOOXMLLimits()
		limits.MaxCompressionRatio = 2
		if err := validateOOXMLBytes(data, FormatDOCX, limits); err == nil {
			t.Fatal("compression ratio limit ignored")
		}
	})
}

func TestServiceUploadIdempotencyAndCompensation(t *testing.T) {
	ctx := context.Background()
	db := newFakeParserEvalStore()
	objects := newFakeObjects()
	svc := newTestService(t, db, objects)
	run, err := svc.CreateDraft(ctx, CreateDraftRequest{
		CreatedBy: "admin", JudgeModelBindingID: "binding-1", IdempotencyKey: "draft-key-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	data := minimalOOXML(t, FormatDOCX, nil)
	req := UploadDocumentRequest{
		RunID: run.ID, CreatedBy: "admin", FileName: "sample.docx", MediaType: MediaTypeDOCX,
		DeclaredSizeBytes: int64(len(data)), Reader: bytes.NewReader(data), IdempotencyKey: "upload-key-01",
	}
	doc, err := svc.UploadDocument(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := mustKey(SourceObjectKey(run.ID, doc.ID))
	if doc.SourceObjectKey != wantKey || !bytes.Equal(objects.data[wantKey], data) {
		t.Fatalf("stored document=%+v object=%d bytes", doc, len(objects.data[wantKey]))
	}
	wantHash := sha256.Sum256(data)
	if doc.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("sha=%q", doc.SHA256)
	}

	req.Reader = bytes.NewReader(data)
	again, err := svc.UploadDocument(ctx, req)
	if err != nil || again.ID != doc.ID {
		t.Fatalf("idempotent upload=%+v err=%v", again, err)
	}
	if objects.puts != 1 {
		t.Fatalf("idempotent retry wrote %d objects", objects.puts)
	}

	different := minimalOOXML(t, FormatDOCX, map[string]string{"word/extra.xml": "different"})
	req.Reader, req.DeclaredSizeBytes = bytes.NewReader(different), int64(len(different))
	if _, err := svc.UploadDocument(ctx, req); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different content error=%v", err)
	}

	badSize := req
	badSize.IdempotencyKey = "upload-key-02"
	badSize.Reader = bytes.NewReader(data)
	badSize.DeclaredSizeBytes = int64(len(data) + 1)
	if _, err := svc.UploadDocument(ctx, badSize); !errors.Is(err, ErrDeclaredSizeMismatch) {
		t.Fatalf("declared size error=%v", err)
	}
	badMIME := req
	badMIME.IdempotencyKey = "upload-key-03"
	badMIME.Reader = bytes.NewReader(data)
	badMIME.DeclaredSizeBytes = int64(len(data))
	badMIME.MediaType = "application/zip"
	if _, err := svc.UploadDocument(ctx, badMIME); !errors.Is(err, ErrInvalidUpload) {
		t.Fatalf("MIME error=%v", err)
	}

	objects.failPut = true
	failing := req
	failing.IdempotencyKey = "upload-key-04"
	failing.Reader = bytes.NewReader(data)
	failing.DeclaredSizeBytes = int64(len(data))
	if _, err := svc.UploadDocument(ctx, failing); err == nil {
		t.Fatal("object failure was ignored")
	}
	failingID, err := deterministicID("ped_", "admin", "upload-document", run.ID, failing.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.documents[failingID]; ok {
		t.Fatal("failed object upload left reservation")
	}
	if got := objects.events[len(objects.events)-2:]; got[0] != "put" || got[1] != "delete-prefix" {
		t.Fatalf("compensation order=%v", got)
	}
}

func TestServiceRemoveAndOpenArtifact(t *testing.T) {
	ctx := context.Background()
	db := newFakeParserEvalStore()
	objects := newFakeObjects()
	svc := newTestService(t, db, objects)
	run, err := svc.CreateDraft(ctx, CreateDraftRequest{CreatedBy: "admin", JudgeModelBindingID: "binding-1"})
	if err != nil {
		t.Fatal(err)
	}
	data := minimalOOXML(t, FormatDOCX, nil)
	doc, err := svc.UploadDocument(ctx, UploadDocumentRequest{
		RunID: run.ID, CreatedBy: "admin", FileName: "sample.docx", MediaType: MediaTypeDOCX,
		DeclaredSizeBytes: int64(len(data)), Reader: bytes.NewReader(data),
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := svc.OpenArtifact(ctx, run.ID, doc.ID, ArtifactRequest{Kind: ArtifactSource})
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(artifact.Reader)
	_ = artifact.Reader.Close()
	if !bytes.Equal(read, data) || artifact.FileName != "sample.docx" || artifact.ContentType != MediaTypeDOCX {
		t.Fatalf("artifact=%+v bytes=%d", artifact, len(read))
	}
	if _, err := svc.OpenArtifact(ctx, run.ID, doc.ID, ArtifactRequest{Kind: ArtifactMarkdown, Engine: "other"}); !errors.Is(err, ErrInvalidArtifactRequest) {
		t.Fatalf("open invalid artifact error=%v", err)
	}
	markdown := []byte("# parsed\n")
	markdownHash := sha256.Sum256(markdown)
	markdownKey := mustKey(MarkdownObjectKey(run.ID, doc.ID, EngineMarkItDown))
	result := ParserResult{
		Status:     StepSucceeded,
		Descriptor: ParserDescriptor{Name: "markitdown", Version: "1", WrapperVersion: "1"},
		Order:      1,
		Markdown: StoredArtifact{
			ObjectKey: mustKey(SourceObjectKey(run.ID, doc.ID)),
			SHA256:    hex.EncodeToString(markdownHash[:]),
			MediaType: "text/markdown; charset=utf-8",
			ByteSize:  int64(len(markdown)),
		},
		Stats: StructureStats{Version: "parser-structure-stats-v1", Characters: len(markdown)},
	}
	encoded, err := EncodeBoundedJSON(result, MaxResultJSONBytes)
	if err != nil {
		t.Fatal(err)
	}
	db.documents[doc.ID].MarkItDownResultJSON = encoded
	if _, err := svc.OpenArtifact(ctx, run.ID, doc.ID, ArtifactRequest{Kind: ArtifactMarkdown, Engine: EngineMarkItDown}); !errors.Is(err, ErrArtifactUnavailable) {
		t.Fatalf("cross-kind object key error=%v", err)
	}
	result.Markdown.ObjectKey = markdownKey
	encoded, err = EncodeBoundedJSON(result, MaxResultJSONBytes)
	if err != nil {
		t.Fatal(err)
	}
	db.documents[doc.ID].MarkItDownResultJSON = encoded
	objects.data[markdownKey] = markdown
	parsed, err := svc.OpenArtifact(ctx, run.ID, doc.ID, ArtifactRequest{Kind: ArtifactMarkdown, Engine: EngineMarkItDown})
	if err != nil {
		t.Fatal(err)
	}
	parsedBytes, _ := io.ReadAll(parsed.Reader)
	_ = parsed.Reader.Close()
	if !bytes.Equal(parsedBytes, markdown) || parsed.FileName != "markitdown.md" {
		t.Fatalf("markdown artifact=%+v bytes=%q", parsed, parsedBytes)
	}

	objects.events = nil
	if err := svc.RemoveDraftDocument(ctx, run.ID, doc.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	if len(objects.events) != 1 || objects.events[0] != "delete-prefix" || db.deleteEvents[0] != "delete-row" {
		t.Fatalf("delete events objects=%v database=%v", objects.events, db.deleteEvents)
	}
}

func TestUploadIdentityRejectsPathAndControlNames(t *testing.T) {
	tests := []struct {
		name      string
		fileName  string
		mediaType string
	}{
		{name: "path", fileName: "folder/sample.docx", mediaType: MediaTypeDOCX},
		{name: "windows path", fileName: `folder\sample.docx`, mediaType: MediaTypeDOCX},
		{name: "newline", fileName: "sample\n.docx", mediaType: MediaTypeDOCX},
		{name: "fake extension", fileName: "sample.pdf", mediaType: MediaTypeDOCX},
		{name: "wrong MIME", fileName: "sample.docx", mediaType: "application/zip"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateUploadIdentity(test.fileName, test.mediaType); !errors.Is(err, ErrInvalidUpload) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func newTestService(t *testing.T, db *fakeParserEvalStore, objects *fakeObjects) *Service {
	t.Helper()
	cfg := config.DefaultParserEvaluationCfg()
	cfg.MaxFileBytes = 2 << 20
	cfg.MaxBatchBytes = 4 << 20
	svc, err := NewService(db, objects, cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc.now = func() time.Time { return time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC) }
	return svc
}

func mustKey(value string, err error) string {
	if err != nil {
		panic(err)
	}
	return value
}

func minimalOOXML(t *testing.T, format Format, extra map[string]string) []byte {
	t.Helper()
	entries := map[string]string{"[Content_Types].xml": "<Types/>"}
	switch format {
	case FormatDOCX:
		entries["word/document.xml"] = "<document/>"
	case FormatPPTX:
		entries["ppt/presentation.xml"] = "<presentation/>"
	case FormatXLSX:
		entries["xl/workbook.xml"] = "<workbook/>"
	}
	for name, value := range extra {
		entries[name] = value
	}
	return zipEntries(t, entries)
}

func zipEntries(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := zip.NewWriter(&out)
	for name, value := range entries {
		entry, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

type fakeParserEvalStore struct {
	mu           sync.Mutex
	runs         map[string]*store.ParserEvalRunRecord
	documents    map[string]*store.ParserEvalDocumentRecord
	deleteEvents []string
}

func newFakeParserEvalStore() *fakeParserEvalStore {
	return &fakeParserEvalStore{runs: map[string]*store.ParserEvalRunRecord{}, documents: map[string]*store.ParserEvalDocumentRecord{}}
}

func (f *fakeParserEvalStore) CreateParserEvalRun(_ context.Context, record *store.ParserEvalRunRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.runs[record.ID]; exists {
		return errors.New("duplicate run")
	}
	copy := *record
	f.runs[record.ID] = &copy
	return nil
}

func (f *fakeParserEvalStore) GetParserEvalRun(_ context.Context, id string) (*store.ParserEvalRunRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.runs[id]
	if !exists {
		return nil, store.ErrNotFound
	}
	copy := *record
	return &copy, nil
}

func (f *fakeParserEvalStore) ReserveParserEvalDocument(_ context.Context, record *store.ParserEvalDocumentRecord, maxFiles int, maxBytes int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.documents[record.ID]; exists {
		return errors.New("duplicate document")
	}
	var count int
	var total int64
	for _, existing := range f.documents {
		if existing.RunID == record.RunID {
			count++
			total += existing.SizeBytes
		}
	}
	if count >= maxFiles || total+record.SizeBytes > maxBytes {
		return store.ErrParserEvalLimit
	}
	copy := *record
	copy.Ordinal = int64(count + 1)
	copy.SourceObjectKey = ""
	f.documents[record.ID] = &copy
	record.Ordinal = copy.Ordinal
	return nil
}

func (f *fakeParserEvalStore) CompleteParserEvalDocumentUpload(_ context.Context, runID, documentID, sourceObjectKey string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.documents[documentID]
	if !exists || record.RunID != runID || record.SourceObjectKey != "" {
		return false, nil
	}
	record.SourceObjectKey = sourceObjectKey
	return true, nil
}

func (f *fakeParserEvalStore) AbortParserEvalDocumentUpload(_ context.Context, runID, documentID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.documents[documentID]
	if !exists || record.RunID != runID || record.SourceObjectKey != "" {
		return false, nil
	}
	delete(f.documents, documentID)
	return true, nil
}

func (f *fakeParserEvalStore) DeleteParserEvalDraftDocument(_ context.Context, runID, documentID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.documents[documentID]
	if !exists || record.RunID != runID {
		return false, nil
	}
	f.deleteEvents = append(f.deleteEvents, "delete-row")
	delete(f.documents, documentID)
	return true, nil
}

func (f *fakeParserEvalStore) GetParserEvalDocument(_ context.Context, runID, documentID string) (*store.ParserEvalDocumentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	record, exists := f.documents[documentID]
	if !exists || record.RunID != runID {
		return nil, store.ErrNotFound
	}
	copy := *record
	return &copy, nil
}

type fakeObjects struct {
	data       map[string][]byte
	events     []string
	puts       int
	failPut    bool
	failDelete bool
}

func newFakeObjects() *fakeObjects { return &fakeObjects{data: map[string][]byte{}} }

func (f *fakeObjects) Put(_ context.Context, key string, reader io.Reader, size int64, _ string) error {
	f.events = append(f.events, "put")
	f.puts++
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return errors.New("wrong object size")
	}
	if f.failPut {
		return errors.New("put failed")
	}
	f.data[key] = data
	return nil
}

func (f *fakeObjects) Get(_ context.Context, key string) (io.ReadCloser, error) {
	data, exists := f.data[key]
	if !exists {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeObjects) Delete(_ context.Context, key string) error {
	delete(f.data, key)
	return nil
}

func (f *fakeObjects) DeletePrefix(_ context.Context, prefix string) error {
	f.events = append(f.events, "delete-prefix")
	if f.failDelete {
		return errors.New("delete failed")
	}
	for key := range f.data {
		if strings.HasPrefix(key, prefix) {
			delete(f.data, key)
		}
	}
	return nil
}
