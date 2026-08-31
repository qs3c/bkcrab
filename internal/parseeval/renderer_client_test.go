package parseeval

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func TestRendererClientHealthAndStreamingRender(t *testing.T) {
	descriptor := testRendererDescriptor()
	sourceBytes := minimalOOXML(t, FormatDOCX, nil)
	source := testRendererSource(sourceBytes, FormatDOCX)
	pngBytes := testPNG(t, 10, 5)
	bundle := testRenderBundle(t, descriptor, source, pngBytes, renderBundleMutation{})
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			writeRendererHealth(t, writer, descriptor, nil)
		case "/v1/render":
			if request.URL.Query().Get("format") != "docx" || request.Header.Get("Accept") != "application/x-tar" {
				t.Errorf("render request url=%s accept=%q", request.URL.String(), request.Header.Get("Accept"))
			}
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			file, header, err := request.FormFile("file")
			if err != nil {
				t.Error(err)
				return
			}
			defer file.Close()
			received, _ = io.ReadAll(file)
			if header.Filename != "sample.docx" || header.Header.Get("Content-Type") != MediaTypeDOCX {
				t.Errorf("multipart header=%+v", header)
			}
			writer.Header().Set("Content-Type", "application/x-tar")
			_, _ = writer.Write(bundle)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testRendererClient(t, server.URL)
	snapshot, err := client.ProbeHealth(context.Background())
	if err != nil || !snapshot.Healthy || snapshot.Descriptor != descriptor || len(snapshot.Formats) != 3 {
		t.Fatalf("health=%+v err=%v", snapshot, err)
	}
	truth, err := client.Render(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer truth.Close()
	if !bytes.Equal(received, sourceBytes) || truth.TotalPages != 1 || truth.CoveredPages != 1 || len(truth.Pages) != 1 {
		t.Fatalf("received=%d truth=%+v", len(received), truth)
	}
	page, err := truth.OpenPage(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	gotPNG, _ := io.ReadAll(page)
	_ = page.Close()
	if !bytes.Equal(gotPNG, pngBytes) {
		t.Fatal("rendered page bytes changed")
	}
	if err := truth.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := truth.OpenPage(context.Background(), 1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed truth open error=%v", err)
	}
}

func TestRendererClientRejectsUnknownHealthFields(t *testing.T) {
	descriptor := testRendererDescriptor()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeRendererHealth(t, writer, descriptor, map[string]any{"unknown": true})
	}))
	defer server.Close()
	client := testRendererClient(t, server.URL)
	snapshot, err := client.ProbeHealth(context.Background())
	if err == nil || snapshot.Healthy || snapshot.Reason != "protocol_mismatch" {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}

func TestRendererClientRejectsUntrustedBundles(t *testing.T) {
	descriptor := testRendererDescriptor()
	sourceBytes := minimalOOXML(t, FormatDOCX, nil)
	source := testRendererSource(sourceBytes, FormatDOCX)
	pngBytes := testPNG(t, 10, 5)
	tests := []struct {
		name     string
		mutation renderBundleMutation
	}{
		{name: "path traversal", mutation: renderBundleMutation{pagePath: "../page.png"}},
		{name: "hash mismatch", mutation: renderBundleMutation{badHash: true}},
		{name: "size mismatch", mutation: renderBundleMutation{sizeDelta: 1}},
		{name: "pixel mismatch", mutation: renderBundleMutation{widthDelta: 1}},
		{name: "descriptor mismatch", mutation: renderBundleMutation{descriptorMismatch: true}},
		{name: "duplicate entry", mutation: renderBundleMutation{extraEntry: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundle := testRenderBundle(t, descriptor, source, pngBytes, test.mutation)
			server := rendererTestServer(t, descriptor, bundle)
			defer server.Close()
			client := testRendererClient(t, server.URL)
			if _, err := client.ProbeHealth(context.Background()); err != nil {
				t.Fatal(err)
			}
			if truth, err := client.Render(context.Background(), source); !errors.Is(err, ErrInvalidRendererBundle) {
				if truth != nil {
					_ = truth.Close()
				}
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRendererClientUsesCachedHealthAndHonorsCancellation(t *testing.T) {
	descriptor := testRendererDescriptor()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			writeRendererHealth(t, writer, descriptor, nil)
			return
		}
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		server.CloseClientConnections()
		server.Close()
	}()
	client := testRendererClient(t, server.URL)
	if _, err := client.ProbeHealth(context.Background()); err != nil {
		t.Fatal(err)
	}
	data := minimalOOXML(t, FormatDOCX, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.Render(ctx, testRendererSource(data, FormatDOCX)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error=%v", err)
	}
	close(release)
}

func testRendererClient(t *testing.T, endpoint string) *HTTPRendererClient {
	t.Helper()
	cfg := DefaultRendererClientConfig(endpoint)
	cfg.RequestTimeout = 2 * time.Second
	cfg.HealthTTL = time.Minute
	cfg.TempDir = t.TempDir()
	client, err := NewHTTPRendererClient(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testRendererDescriptor() RendererDescriptor {
	return RendererDescriptor{
		ProtocolVersion: "parser-eval-renderer/v1", ServiceVersion: "renderer-test",
		LibreOfficeVersion: "LibreOffice test", PyMuPDFVersion: "1.26.4",
	}
}

func testRendererSource(data []byte, format Format) document.Source {
	digest := sha256.Sum256(data)
	return document.Source{
		DocID: "ped_renderer", FileName: "sample." + string(format), Format: string(format), Size: int64(len(data)),
		SHA256: hex.EncodeToString(digest[:]), Open: func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(data)), nil
		},
	}
}

func writeRendererHealth(t *testing.T, writer http.ResponseWriter, descriptor RendererDescriptor, additions map[string]any) {
	t.Helper()
	payload := map[string]any{
		"descriptor": descriptor,
		"formats":    []string{"docx", "pptx", "xlsx"},
		"limits": RendererHealthLimits{
			MaxInputBytes: 50 << 20, MaxPages: 6, RenderDPI: 100, MaxPagePixels: 40_000_000,
			MaxPageBytes: 20 << 20, MaxBundleBytes: 128 << 20,
		},
	}
	for key, value := range additions {
		payload[key] = value
	}
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(payload); err != nil {
		t.Error(err)
	}
}

type renderBundleMutation struct {
	pagePath           string
	badHash            bool
	sizeDelta          int64
	widthDelta         int
	descriptorMismatch bool
	extraEntry         bool
}

func testRenderBundle(t *testing.T, descriptor RendererDescriptor, source document.Source, pngBytes []byte, mutation renderBundleMutation) []byte {
	t.Helper()
	pagePath := mutation.pagePath
	if pagePath == "" {
		pagePath = "pages/page-0001.png"
	}
	digest := sha256.Sum256(pngBytes)
	hash := hex.EncodeToString(digest[:])
	if mutation.badHash {
		hash = strings.Repeat("0", 64)
	}
	manifestDescriptor := descriptor
	if mutation.descriptorMismatch {
		manifestDescriptor.ServiceVersion = "other"
	}
	manifest := rendererManifest{
		ProtocolVersion: "parser-eval-renderer/v1", Descriptor: manifestDescriptor,
		Source:     rendererSourceDescriptor{Format: Format(source.Format), ByteSize: source.Size, SHA256: source.SHA256},
		TotalPages: 1, CoveredPages: 1, RenderDurationMS: 12,
		Pages: []rendererPageDescriptor{{
			Page: 1, Width: 10 + mutation.widthDelta, Height: 5, Path: pagePath, MediaType: "image/png",
			SHA256: hash, ByteSize: int64(len(pngBytes)) + mutation.sizeDelta,
		}},
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	writeTarEntry(t, writer, "manifest.json", manifestBytes)
	writeTarEntry(t, writer, pagePath, pngBytes)
	if mutation.extraEntry {
		writeTarEntry(t, writer, pagePath, pngBytes)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func writeTarEntry(t *testing.T, writer *tar.Writer, name string, data []byte) {
	t.Helper()
	header := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), Typeflag: tar.TypeReg, Format: tar.FormatUSTAR}
	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
}

func testPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func rendererTestServer(t *testing.T, descriptor RendererDescriptor, bundle []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/healthz" {
			writeRendererHealth(t, writer, descriptor, nil)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "application/x-tar")
		_, _ = writer.Write(bundle)
	}))
}
