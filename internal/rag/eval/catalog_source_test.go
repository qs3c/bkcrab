package eval

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/objects"
)

type catalogRoundTripFunc func(*http.Request) (*http.Response, error)

func (f catalogRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCatalogHTTPSourceCachesPinnedFileAcrossInstances(t *testing.T) {
	preset, _ := CatalogPresetByID(CatalogTATQA)
	objectStore := objects.NewLocalFS(t.TempDir())
	var calls atomic.Int32
	client := &http.Client{Transport: catalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if !strings.Contains(request.URL.Path, preset.Revision) || !strings.HasSuffix(request.URL.Path, "/tatqa_dataset_dev.json") {
			t.Fatalf("unpinned or unexpected source URL: %s", request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`[{"table":{"uid":"x","table":[]},"paragraphs":[],"questions":[]}]`)), Request: request}, nil
	})}
	for iteration := 0; iteration < 2; iteration++ {
		source, err := NewCatalogHTTPSource(preset, objectStore, client)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := source.Open(context.Background(), "tatqa_dataset_dev.json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(reader); err != nil {
			t.Fatal(err)
		}
		_ = reader.Close()
	}
	if calls.Load() != 1 {
		t.Fatalf("network calls=%d, want one server-wide cached download", calls.Load())
	}
}

func TestCatalogHTTPSourceRejectsUnregisteredPathsAndURLs(t *testing.T) {
	preset, _ := CatalogPresetByID(CatalogOpenRAGBench)
	source, err := NewCatalogHTTPSource(preset, objects.NewLocalFS(t.TempDir()), &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Open(context.Background(), "../../secret"); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if _, err := source.OpenExternal(context.Background(), "https://example.com/file.pdf"); err == nil {
		t.Fatal("unregistered external host was accepted")
	}
}
