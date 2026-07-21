package sidecar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

// TestParserSidecarIntegration is deliberately opt-in: unlike the protocol
// unit tests, it sends a repository-owned PDF through the configured running
// sidecar, decodes its real tar streams, and pairs analyze/render responses.
func TestParserSidecarIntegration(t *testing.T) {
	if os.Getenv("RAG_PARSER_INTEGRATION") != "1" {
		t.Skip("RAG_PARSER_INTEGRATION=1 is required for the real parser sidecar test")
	}
	endpoint := strings.TrimSpace(os.Getenv("BKCRAB_RAG_PARSER_ENDPOINT"))
	if endpoint == "" {
		t.Fatal("RAG_PARSER_INTEGRATION=1 requires BKCRAB_RAG_PARSER_ENDPOINT")
	}

	timeout := integrationParserTimeout(t)
	client, err := NewClient(ClientConfig{
		Endpoint: endpoint, Timeout: timeout, HealthTTL: timeout,
		TempDir: t.TempDir(), PDFLicenseApproved: true,
	})
	if err != nil {
		t.Fatalf("create real sidecar client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	health, err := client.ProbeHealth(ctx)
	if err != nil {
		t.Fatalf("probe real sidecar health: %v", err)
	}
	if !health.Healthy || health.ProtocolVersion != ProtocolVersion || health.MaxInputBytes <= 0 {
		t.Fatalf("incompatible real sidecar health: %+v", health)
	}
	if !health.PDF.Enabled || !health.PDF.LicenseApproved {
		t.Fatalf("real sidecar PDF capability is not release-compatible: %+v", health.PDF)
	}

	source := integrationPDFSource(t)
	analyze, err := client.AnalyzePDF(ctx, source)
	if err != nil {
		t.Fatalf("analyze repository PDF with real sidecar: %v", err)
	}
	defer analyze.Close()
	if analyze.Manifest.BundleKind != BundleKindPDFAnalyze ||
		analyze.Manifest.Source.Format != "pdf" ||
		analyze.Manifest.Source.ByteSize != source.Size ||
		analyze.Manifest.Source.SHA256 != source.SHA256 ||
		len(analyze.Manifest.Pages) == 0 {
		t.Fatalf("unexpected real analyze manifest: %+v", analyze.Manifest)
	}
	for index, page := range analyze.Manifest.Pages {
		if page.Page != index+1 || page.Status != PageStatusOK {
			t.Fatalf("analyze page %d is not a contiguous successful page: %+v", index+1, page)
		}
		primitive, primitiveErr := analyze.PagePrimitive(ctx, page.Page)
		if primitiveErr != nil {
			t.Fatalf("decode real primitive for page %d: %v", page.Page, primitiveErr)
		}
		if primitive.Page != page.Page || primitive.Width <= 0 || primitive.Height <= 0 {
			t.Fatalf("invalid real primitive for page %d: %+v", page.Page, primitive)
		}
	}

	render, err := client.RenderPDF(ctx, source, []int{1})
	if err != nil {
		t.Fatalf("render repository PDF with real sidecar: %v", err)
	}
	defer render.Close()
	if err := ValidatePDFBundlePair(&analyze.Manifest, &render.Manifest, []int{1}); err != nil {
		t.Fatalf("real analyze/render bundles do not form a valid pair: %v", err)
	}
	if len(render.Manifest.Pages) != 1 || render.Manifest.Pages[0].Status != PageStatusOK {
		t.Fatalf("unexpected real render pages: %+v", render.Manifest.Pages)
	}
	renderEntry, err := render.OpenEntry(ctx, render.Manifest.Pages[0].RenderEntry)
	if err != nil {
		t.Fatalf("open real rendered page: %v", err)
	}
	renderedBytes, readErr := io.ReadAll(io.LimitReader(renderEntry, 8<<20))
	closeErr := renderEntry.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read real rendered page: read=%v close=%v", readErr, closeErr)
	}
	if len(renderedBytes) == 0 || len(renderedBytes) >= 8<<20 {
		t.Fatalf("real rendered page has invalid bounded size %d", len(renderedBytes))
	}
}

func integrationParserTimeout(t *testing.T) time.Duration {
	t.Helper()
	const fallback = 2 * time.Minute
	raw := strings.TrimSpace(os.Getenv("BKCRAB_RAG_PARSER_TIMEOUT_MS"))
	if raw == "" {
		return fallback
	}
	milliseconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || milliseconds <= 0 {
		t.Fatalf("BKCRAB_RAG_PARSER_TIMEOUT_MS must be a positive integer, got %q", raw)
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func integrationPDFSource(t *testing.T) document.Source {
	t.Helper()
	fixturePath := filepath.Join("..", "testdata", "sample.pdf")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read repository PDF fixture: %v", err)
	}
	digest := sha256.Sum256(data)
	return document.Source{
		DocID: "doc_parser_integration", FileName: "sample.pdf", Format: "pdf",
		Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]),
		Open: func(context.Context) (io.ReadCloser, error) { return os.Open(fixturePath) },
	}
}
