package eval

import (
	"context"
	"io"
	"net/http"
	"net/url"
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
	preset, _ := CatalogPresetByID(CatalogOpenRAGBench)
	objectStore := objects.NewLocalFS(t.TempDir())
	var calls atomic.Int32
	client := &http.Client{Transport: catalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if !strings.Contains(request.URL.Path, preset.Revision) || !strings.HasSuffix(request.URL.Path, "/pdf/arxiv/queries.json") {
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
		reader, err := source.Open(context.Background(), "pdf/arxiv/queries.json")
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

func TestTATQAMirrorSourcesAreRevisionPinnedAndChecksummed(t *testing.T) {
	preset, _ := CatalogPresetByID(CatalogTATQA)
	source, err := NewCatalogHTTPSource(preset, objects.NewLocalFS(t.TempDir()), &http.Client{})
	if err != nil {
		t.Fatal(err)
	}
	for logicalPath, wantSHA := range tatQAFileSHA256 {
		remote, maxBytes, gotSHA, err := source.resolve(logicalPath)
		if err != nil {
			t.Fatalf("resolve %s: %v", logicalPath, err)
		}
		parsed, err := url.Parse(remote)
		if err != nil || parsed.Hostname() != "hf-mirror.com" || !strings.Contains(parsed.Path, "/"+preset.Revision+"/") {
			t.Fatalf("TAT-QA source is not mirror/revision pinned: %q", remote)
		}
		if maxBytes != defaultCatalogFile || gotSHA != wantSHA || len(gotSHA) != 64 {
			t.Fatalf("TAT-QA source contract for %s = max:%d sha:%q", logicalPath, maxBytes, gotSHA)
		}
	}
}

func TestTATQAMirrorRejectsContentOutsidePinnedChecksum(t *testing.T) {
	preset, _ := CatalogPresetByID(CatalogTATQA)
	client := &http.Client{Transport: catalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`[{"unexpected":true}]`)), Request: request}, nil
	})}
	source, err := NewCatalogHTTPSource(preset, objects.NewLocalFS(t.TempDir()), client)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := source.Open(context.Background(), "tatqa_dataset_dev.json")
	if reader != nil {
		_ = reader.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("unexpected mirror content was accepted: %v", err)
	}
}

func TestCatalogRemoteURLAllowsExactHuggingFaceDeliveryHosts(t *testing.T) {
	for _, raw := range []string{
		"https://huggingface.co/datasets/example",
		"https://us.aws.cdn.hf.co/xet-bridge-us/object",
		"https://hf-mirror.com/datasets/example",
	} {
		parsed, err := url.Parse(raw)
		if err != nil || validateCatalogRemoteURL(parsed) != nil {
			t.Fatalf("trusted catalog host rejected: %q", raw)
		}
	}
	for _, raw := range []string{
		"https://us.aws.cdn.hf.co.evil.example/object",
		"https://hf-mirror.com.evil.example/datasets/example",
	} {
		parsed, _ := url.Parse(raw)
		if validateCatalogRemoteURL(parsed) == nil {
			t.Fatalf("lookalike catalog host accepted: %q", raw)
		}
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
