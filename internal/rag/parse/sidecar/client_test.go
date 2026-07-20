package sidecar

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

type guardedSourceReader struct {
	reader      *bytes.Reader
	maxRequest  int
	largestRead atomic.Int64
	closed      atomic.Bool
}

func (r *guardedSourceReader) Read(buffer []byte) (int, error) {
	for {
		previous := r.largestRead.Load()
		if int64(len(buffer)) <= previous || r.largestRead.CompareAndSwap(previous, int64(len(buffer))) {
			break
		}
	}
	if len(buffer) > r.maxRequest {
		return 0, errors.New("source reader was asked for an unbounded buffer")
	}
	return r.reader.Read(buffer)
}

func (r *guardedSourceReader) Close() error {
	r.closed.Store(true)
	return nil
}

func testDocumentSource(data []byte, format string, open func() io.ReadCloser) document.Source {
	sum := sha256.Sum256(data)
	return document.Source{
		DocID: "doc_sidecar_test", FileName: "input." + format, Format: format,
		Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
		Open: func(context.Context) (io.ReadCloser, error) { return open(), nil },
	}
}

func healthyResponse(t *testing.T) []byte {
	t.Helper()
	return readSharedGolden(t, "health.json")
}

func officeResponseTar(t *testing.T, source document.Source) []byte {
	t.Helper()
	markdown := []byte("# Converted\n")
	manifest := testOfficeManifest(markdown)
	manifest.Source = SourceDescriptor{Format: source.Format, ByteSize: source.Size, SHA256: source.SHA256}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return makeTestTar(t, manifestJSON, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})
}

func TestSourceMIMETypesMatchSidecarAllowlist(t *testing.T) {
	tests := map[string]string{
		"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"pdf":  "application/pdf",
	}
	for format, expected := range tests {
		if actual, err := sourceMIMEType(format); err != nil || actual != expected {
			t.Errorf("sourceMIMEType(%q)=%q, %v; want %q", format, actual, err, expected)
		}
	}
	if _, err := sourceMIMEType("doc"); err == nil {
		t.Fatal("legacy Office MIME unexpectedly accepted")
	}
}

func TestClientStreamsMultipartAndDecodesOfficeBundle(t *testing.T) {
	data := bytes.Repeat([]byte("office-source-"), 128*1024)
	guard := &guardedSourceReader{reader: bytes.NewReader(data), maxRequest: 64 * 1024}
	source := testDocumentSource(data, "docx", func() io.ReadCloser { return guard })
	var convertCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(healthyResponse(t))
		case "/v1/office/convert":
			convertCalls.Add(1)
			if request.URL.Query().Get("format") != "docx" {
				t.Errorf("format query=%q", request.URL.RawQuery)
			}
			if request.ContentLength != -1 {
				t.Errorf("multipart request was buffered, ContentLength=%d", request.ContentLength)
			}
			mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if err != nil || mediaType != "multipart/form-data" {
				t.Errorf("content type=%q err=%v", request.Header.Get("Content-Type"), err)
			}
			multipartReader, err := request.MultipartReader()
			if err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			part, err := multipartReader.NextPart()
			if err != nil {
				t.Error(err)
				response.WriteHeader(http.StatusBadRequest)
				return
			}
			if part.FormName() != "file" || part.FileName() != source.FileName {
				t.Errorf("multipart part name=%q filename=%q", part.FormName(), part.FileName())
			}
			if got := part.Header.Get("Content-Type"); got != "application/vnd.openxmlformats-officedocument.wordprocessingml.document" {
				t.Errorf("multipart part Content-Type=%q", got)
			}
			received, err := io.ReadAll(part)
			if err != nil || !bytes.Equal(received, data) {
				t.Errorf("received source len=%d err=%v", len(received), err)
			}
			response.Header().Set("Content-Type", "application/x-tar")
			_, _ = response.Write(officeResponseTar(t, source))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, Timeout: 5 * time.Second, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 4 << 20, MaxOutputBytes: 4 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	handle, err := client.ConvertOffice(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if handle.Manifest.BundleKind != BundleKindOfficeConvert || convertCalls.Load() != 1 {
		t.Fatalf("kind=%q calls=%d", handle.Manifest.BundleKind, convertCalls.Load())
	}
	if !guard.closed.Load() || guard.largestRead.Load() > 64*1024 {
		t.Fatalf("source close=%v largestRead=%d", guard.closed.Load(), guard.largestRead.Load())
	}
}

func TestPDFCapabilityUnavailableDoesNotDisableOffice(t *testing.T) {
	data := []byte("office")
	source := testDocumentSource(data, "docx", func() io.ReadCloser { return io.NopCloser(bytes.NewReader(data)) })
	var pdfCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			_, _ = response.Write(healthyResponse(t))
		case "/v1/office/convert":
			_, _ = io.Copy(io.Discard, request.Body)
			response.Header().Set("Content-Type", "application/x-tar")
			_, _ = response.Write(officeResponseTar(t, source))
		default:
			pdfCalls.Add(1)
			response.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 1024, MaxOutputBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if handle, err := client.ConvertOffice(context.Background(), source); err != nil {
		t.Fatalf("Office conversion failed: %v", err)
	} else {
		_ = handle.Close()
	}
	pdfSource := source
	pdfSource.Format = "pdf"
	pdfSource.FileName = "input.pdf"
	if _, err := client.AnalyzePDF(context.Background(), pdfSource); !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("AnalyzePDF error=%v", err)
	}
	if _, err := client.RenderPDF(context.Background(), pdfSource, []int{1}); !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("RenderPDF error=%v", err)
	}
	if pdfCalls.Load() != 0 {
		t.Fatalf("unavailable PDF capability made %d HTTP requests", pdfCalls.Load())
	}
}

func TestIncompatibleOfficeHealthCannotPublishAnAvailableConverter(t *testing.T) {
	health := bytes.Replace(healthyResponse(t), []byte(`"markitdownVersion": "0.1.6"`), []byte(`"markitdownVersion": "9.9.9"`), 1)
	var convertCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_, _ = response.Write(health)
			return
		}
		convertCalls.Add(1)
		response.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 1024, MaxOutputBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.ProbeHealth(context.Background())
	if err != nil {
		t.Fatalf("schema-valid health probe: %v", err)
	}
	if !snapshot.Healthy || snapshot.Office.Enabled {
		t.Fatalf("incompatible Office snapshot=%+v", snapshot)
	}
	data := []byte("office")
	source := testDocumentSource(data, "docx", func() io.ReadCloser {
		return io.NopCloser(bytes.NewReader(data))
	})
	if _, err := client.ConvertOffice(context.Background(), source); !errors.Is(err, ErrCapabilityUnavailable) {
		t.Fatalf("ConvertOffice error=%v", err)
	}
	if convertCalls.Load() != 0 {
		t.Fatalf("incompatible Office converter made %d requests", convertCalls.Load())
	}
}

func TestHealthTTLAndProbeFailureAreCached(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	var healthy atomic.Bool
	healthy.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if healthy.Load() {
			_, _ = response.Write(healthyResponse(t))
			return
		}
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, HealthTTL: 30 * time.Second, Now: clock,
		Limits: ClientLimits{MaxInputBytes: 1 << 20, MaxOutputBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.ProbeHealth(context.Background())
	if err != nil || !snapshot.Healthy || !snapshot.ExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	nowMu.Lock()
	now = now.Add(31 * time.Second)
	nowMu.Unlock()
	if client.officeAvailable("docx") {
		t.Fatal("expired health snapshot still enabled Office")
	}
	healthy.Store(false)
	snapshot, err = client.ProbeHealth(context.Background())
	if err == nil || snapshot.Healthy || snapshot.CheckedAt.IsZero() || snapshot.Reason == "" {
		t.Fatalf("failed probe snapshot=%+v err=%v", snapshot, err)
	}
}

func TestClientClassifiesHTTPAndInvalidBundleErrors(t *testing.T) {
	data := []byte("office")
	source := testDocumentSource(data, "docx", func() io.ReadCloser { return io.NopCloser(bytes.NewReader(data)) })
	mode := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_, _ = response.Write(healthyResponse(t))
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		if mode.Load() == 0 {
			response.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		response.Header().Set("Content-Type", "application/x-tar")
		_, _ = response.Write([]byte("not a tar"))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 1024, MaxOutputBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ConvertOffice(context.Background(), source); err == nil {
		t.Fatal("503 response succeeded")
	} else {
		var httpErr *HTTPError
		if !errors.As(err, &httpErr) || httpErr.HTTPStatus() != http.StatusServiceUnavailable {
			t.Fatalf("error=%T %v", err, err)
		}
	}
	mode.Store(1)
	if _, err := client.ConvertOffice(context.Background(), source); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("invalid bundle error=%v", err)
	}
}

func TestClientClassifiesRequestTimeout(t *testing.T) {
	data := []byte("office")
	source := testDocumentSource(data, "docx", func() io.ReadCloser { return io.NopCloser(bytes.NewReader(data)) })
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_, _ = response.Write(healthyResponse(t))
			return
		}
		<-releaseHandler
	}))
	defer func() {
		close(releaseHandler)
		server.Close()
	}()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, Timeout: 100 * time.Millisecond, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 1024, MaxOutputBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ConvertOffice(context.Background(), source); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error=%T %v", err, err)
	}
}

func TestClientCancellationClosesStreamingSource(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1<<20)
	guard := &guardedSourceReader{reader: bytes.NewReader(data), maxRequest: 64 * 1024}
	source := testDocumentSource(data, "docx", func() io.ReadCloser { return guard })
	started := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_, _ = response.Write(healthyResponse(t))
			return
		}
		close(started)
		select {
		case <-request.Context().Done():
		case <-releaseHandler:
		}
	}))
	defer func() {
		close(releaseHandler)
		server.Close()
	}()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 2 << 20, MaxOutputBytes: 2 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, convertErr := client.ConvertOffice(ctx, source)
		done <- convertErr
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ConvertOffice did not stop after cancellation")
	}
	if !guard.closed.Load() {
		t.Fatal("source was not closed after cancellation")
	}
}

func TestClientRejectsSourceAndHealthLimitsBeforeUpload(t *testing.T) {
	data := []byte(strings.Repeat("x", 64))
	source := testDocumentSource(data, "docx", func() io.ReadCloser { return io.NopCloser(bytes.NewReader(data)) })
	var calls atomic.Int32
	health := healthyResponse(t)
	health = bytes.Replace(health, []byte("52428800"), []byte("32"), 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			_, _ = response.Write(health)
			return
		}
		calls.Add(1)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		Endpoint: server.URL, HealthTTL: time.Minute,
		Limits: ClientLimits{MaxInputBytes: 1024, MaxOutputBytes: 1 << 20, MaxEntryBytes: 1 << 20, MaxEntries: 32},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ConvertOffice(context.Background(), source); !errors.Is(err, ErrSourceLimitExceeded) {
		t.Fatalf("error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("oversize source made %d conversion calls", calls.Load())
	}
}

func TestWriteMultipartSourceDoesNotSendBytesPastImmutableSize(t *testing.T) {
	declared := []byte("safe")
	actual := append(append([]byte(nil), declared...), []byte("-unexpected-tail")...)
	source := testDocumentSource(declared, "docx", func() io.ReadCloser {
		return io.NopCloser(bytes.NewReader(actual))
	})
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	boundary := writer.Boundary()
	type readResult struct {
		body []byte
		err  error
	}
	readDone := make(chan readResult, 1)
	go func() {
		body, err := io.ReadAll(pipeReader)
		readDone <- readResult{body: body, err: err}
	}()

	writeErr := writeMultipartSource(context.Background(), pipeWriter, writer, source)
	if !errors.Is(writeErr, ErrSourceIntegrity) {
		t.Fatalf("write error=%v, want ErrSourceIntegrity", writeErr)
	}
	result := <-readDone
	if result.err == nil {
		t.Fatal("pipe reader did not receive the source integrity failure")
	}
	multipartReader := multipart.NewReader(bytes.NewReader(result.body), boundary)
	part, err := multipartReader.NextPart()
	if err != nil {
		t.Fatalf("read bounded multipart part: %v", err)
	}
	received, err := io.ReadAll(part)
	if err != nil {
		t.Fatalf("read bounded source bytes: %v", err)
	}
	if !bytes.Equal(received, declared) {
		t.Fatalf("sidecar-bound bytes=%q, want exactly declared snapshot %q", received, declared)
	}
}
