package parse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
)

func TestLocalParserMarkdownNeverFetchesImages(t *testing.T) {
	t.Parallel()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	data := []byte("# Guide\n\n![remote](" + server.URL + "/image.png)\n\ntext")
	source := testDocumentSource("md", "guide.md", data, sourceCopyBufferBytes)
	parsed, err := NewLocalParser(nil, 300).Parse(context.Background(), source, ParseOptions{
		Mode: config.ParseModeStandard,
	})
	if err != nil {
		t.Fatalf("parse Markdown: %v", err)
	}
	defer parsed.Close()
	if got := requests.Load(); got != 0 {
		t.Fatalf("Markdown parser made %d network requests", got)
	}
	if !strings.Contains(parsed.Units[0].Markdown, `\[已忽略文档中的图片：remote\]`) {
		t.Fatalf("image was not replaced: %q", parsed.Units[0].Markdown)
	}
	if len(parsed.Warnings) == 0 || parsed.Warnings[0].Code != "markdown_image_ignored" {
		t.Fatalf("warnings=%+v", parsed.Warnings)
	}
}

func TestTextParserTreatsMarkdownImageSyntaxAsText(t *testing.T) {
	t.Parallel()
	data := []byte("literal ![not an image](https://example.invalid/a.png)\n<script>x</script>")
	parsed, err := NewLocalParser(nil, 300).Parse(
		context.Background(),
		testDocumentSource("txt", "notes.txt", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeStandard},
	)
	if err != nil {
		t.Fatalf("parse text: %v", err)
	}
	defer parsed.Close()
	got := parsed.Units[0].Markdown
	if !strings.Contains(got, `\!\[not an image\]`) {
		t.Fatalf("TXT image-like syntax became Markdown: %q", got)
	}
	if !strings.Contains(got, `&lt;script&gt;x&lt;/script&gt;`) {
		t.Fatalf("TXT HTML-like text was not retained safely: %q", got)
	}
	for _, warning := range parsed.Warnings {
		if warning.Code == "markdown_image_ignored" || warning.Code == "markdown_raw_html_removed" {
			t.Fatalf("TXT literal must not be interpreted as Markdown: %+v", warning)
		}
	}
}

func TestTextParserRejectsContentThatNormalizesToEmpty(t *testing.T) {
	t.Parallel()
	for _, data := range [][]byte{{0}, {1, 2, 3, '\t', '\n'}} {
		_, err := NewLocalParser(nil, 300).Parse(
			context.Background(),
			testDocumentSource("txt", "empty.txt", data, sourceCopyBufferBytes),
			ParseOptions{Mode: config.ParseModeStandard},
		)
		if !errors.Is(err, ErrEmptyContent) {
			t.Fatalf("control-only TXT error=%v, want ErrEmptyContent", err)
		}
	}
}

func TestTextParserRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()
	data := []byte{'t', 'e', 'x', 't', 0xff}
	_, err := NewLocalParser(nil, 300).Parse(
		context.Background(),
		testDocumentSource("txt", "invalid.txt", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeStandard},
	)
	if !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("invalid UTF-8 TXT error=%v, want ErrInvalidDocument", err)
	}
}

func TestNativePDFParserStreamsSourceAndUsesStableUnits(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	source := testDocumentSource("pdf", "sample.pdf", data, sourceCopyBufferBytes)
	parsed, err := NewLocalParser(nil, 300).Parse(context.Background(), source, ParseOptions{
		Mode: config.ParseModeStandard,
	})
	if err != nil {
		t.Fatalf("parse native PDF: %v", err)
	}
	defer parsed.Close()
	if len(parsed.Units) != 1 || !strings.Contains(parsed.Units[0].Markdown, "hello rag") {
		t.Fatalf("unexpected units: %+v", parsed.Units)
	}
	wantID := "unit_page_" + source.SHA256[:12] + "_0001"
	if parsed.Units[0].ID != wantID {
		t.Fatalf("unit id=%q, want %q", parsed.Units[0].ID, wantID)
	}
	if parsed.Units[0].Location.Index != 1 {
		t.Fatalf("page index=%d, want 1", parsed.Units[0].Location.Index)
	}
}

func TestNativePDFFilterWarnsWhenNormalizationEmptiesAPage(t *testing.T) {
	t.Parallel()
	units, warnings := retainNonEmptyPDFUnits([]document.MarkdownUnit{
		{ID: "unit_page_hash_0001", Location: document.SourceLocation{Kind: document.LocationPage, Index: 1}, Markdown: ""},
		{ID: "unit_page_hash_0002", Location: document.SourceLocation{Kind: document.LocationPage, Index: 2}, Markdown: "text"},
	}, nil)
	if len(units) != 1 || units[0].Location.Index != 2 {
		t.Fatalf("retained units=%+v", units)
	}
	if len(warnings) != 1 || warnings[0].Code != "pdf_native_page_empty" || !warnings[0].Degraded ||
		warnings[0].Location == nil || warnings[0].Location.Index != 1 {
		t.Fatalf("empty normalized page warnings=%+v", warnings)
	}
}

func largeTestPDF(paddingBytes int) []byte {
	return testPDFWithPageCount(paddingBytes, "1")
}

func testPDFWithPageCount(paddingBytes int, pageCount string) []byte {
	content := strings.Repeat(" ", paddingBytes) + "BT /F1 12 Tf 20 100 Td (hello rag) Tj ET\n"
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [3 0 R] /Count %s >>", pageCount),
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", len(content), content),
	}
	var output strings.Builder
	output.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects)+1)
	for index, object := range objects {
		offsets[index+1] = output.Len()
		fmt.Fprintf(&output, "%d 0 obj\n%s\nendobj\n", index+1, object)
	}
	xref := output.Len()
	fmt.Fprintf(&output, "xref\n0 %d\n0000000000 65535 f \n", len(offsets))
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&output, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&output, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return []byte(output.String())
}

func TestNativePDFRejectsHostilePageCountsWithoutPanicking(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		pageCount string
		want      error
	}{
		{name: "negative", pageCount: "-1", want: ErrInvalidDocument},
		{name: "huge", pageCount: "2147483647", want: ErrDocumentLimitExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := testPDFWithPageCount(0, test.pageCount)
			_, err := NewLocalParser(nil, 300).Parse(
				context.Background(),
				testDocumentSource("pdf", "hostile.pdf", data, sourceCopyBufferBytes),
				ParseOptions{Mode: config.ParseModeStandard},
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("page count %s error=%v, want %v", test.pageCount, err, test.want)
			}
		})
	}
}

func TestNativePDFEnforcesConfiguredExtractedByteLimit(t *testing.T) {
	t.Parallel()
	data := largeTestPDF(0)
	_, err := NewLocalParser(nil, 300, 16).Parse(
		context.Background(),
		testDocumentSource("pdf", "limited.pdf", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeStandard},
	)
	if !errors.Is(err, ErrDocumentLimitExceeded) {
		t.Fatalf("extracted byte limit error=%v, want ErrDocumentLimitExceeded", err)
	}
}

func TestParserDoesNotReadAll(t *testing.T) {
	t.Parallel()
	data := largeTestPDF(256 * 1024)
	for _, mode := range []config.ParseMode{config.ParseModeStandard, config.ParseModeAuto} {
		mode := mode
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			source := testDocumentSource("pdf", "sample.pdf", data, sourceCopyBufferBytes)
			originalOpen := source.Open
			source.Open = func(ctx context.Context) (io.ReadCloser, error) {
				reader, openErr := originalOpen(ctx)
				if openErr != nil {
					return nil, openErr
				}
				guard := reader.(*guardReadCloser)
				guard.minRead = sourceCopyBufferBytes / 2
				return guard, nil
			}
			parsed, parseErr := NewLocalParser(nil, 300).Parse(
				context.Background(), source, ParseOptions{Mode: mode},
			)
			if parseErr != nil {
				t.Fatalf("parse %s PDF through streaming source: %v", mode, parseErr)
			}
			defer parsed.Close()
		})
	}
}

func TestLocalParserAutoPDFFallsBackWithWarning(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := NewLocalParser(nil, 300).Parse(
		context.Background(),
		testDocumentSource("pdf", "sample.pdf", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeAuto},
	)
	if err != nil {
		t.Fatalf("auto PDF fallback: %v", err)
	}
	defer parsed.Close()
	found := false
	for _, warning := range parsed.Warnings {
		if warning.Code == "pdf_auto_unavailable" && warning.Degraded {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing degraded fallback warning: %+v", parsed.Warnings)
	}
}

func TestLocalParserOfficeNeverFallsBackToLegacyDOCX(t *testing.T) {
	t.Parallel()
	data := makeDocx(t)
	_, err := NewLocalParser(nil, 300).Parse(
		context.Background(),
		testDocumentSource("docx", "sample.docx", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeStandard},
	)
	if !errors.Is(err, sidecar.ErrCapabilityUnavailable) {
		t.Fatalf("Office without sidecar error=%v, want ErrCapabilityUnavailable", err)
	}
}

func TestLocalParserHonorsCancelledSource(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data := []byte("content")
	_, err := NewLocalParser(nil, 300).Parse(
		ctx,
		testDocumentSource("md", "cancelled.md", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeStandard},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled parse error=%v", err)
	}
}

func TestLocalParserPDFCleansSpoolOnEveryExit(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.pdf")
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()
	parser := NewLocalParser(nil, 300)
	parser.TempDir = spoolDir
	assertEmpty := func(stage string) {
		t.Helper()
		entries, readErr := os.ReadDir(spoolDir)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if len(entries) != 0 {
			t.Fatalf("%s left PDF spool entries: %v", stage, entries)
		}
	}

	parsed, err := parser.Parse(context.Background(), testDocumentSource(
		"pdf", "sample.pdf", data, sourceCopyBufferBytes,
	), ParseOptions{Mode: config.ParseModeStandard})
	if err != nil {
		t.Fatalf("success parse: %v", err)
	}
	_ = parsed.Close()
	assertEmpty("success")

	invalid := []byte("not a PDF")
	if _, err := parser.Parse(context.Background(), testDocumentSource(
		"pdf", "invalid.pdf", invalid, sourceCopyBufferBytes,
	), ParseOptions{Mode: config.ParseModeStandard}); err == nil {
		t.Fatal("invalid PDF unexpectedly parsed")
	}
	assertEmpty("error")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := parser.Parse(ctx, testDocumentSource(
		"pdf", "cancelled.pdf", data, sourceCopyBufferBytes,
	), ParseOptions{Mode: config.ParseModeStandard}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PDF error=%v", err)
	}
	assertEmpty("cancel")
}

func TestNativePDFTempStorageErrorsDoNotExposePaths(t *testing.T) {
	t.Parallel()
	data := largeTestPDF(0)
	const canary = "CANARY-private-temp-path"
	parser := NewLocalParser(nil, 300)
	parser.TempDir = filepath.Join(t.TempDir(), canary, "missing")
	_, err := parser.Parse(
		context.Background(),
		testDocumentSource("pdf", "sample.pdf", data, sourceCopyBufferBytes),
		ParseOptions{Mode: config.ParseModeStandard},
	)
	if err == nil {
		t.Fatal("missing temp directory unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("temporary path leaked in parser error: %v", err)
	}
}

func TestMarkdownSourceClosesExactlyOnceAndPropagatesCloseError(t *testing.T) {
	data := []byte("# content")
	source := testDocumentSource("md", "content.md", data, sourceCopyBufferBytes)
	var closeCount atomic.Int32
	source.Open = func(context.Context) (io.ReadCloser, error) {
		return &countingReadCloser{Reader: bytes.NewReader(data), count: &closeCount}, nil
	}
	parsed, err := NewLocalParser(nil, 300).Parse(
		context.Background(), source, ParseOptions{Mode: config.ParseModeStandard},
	)
	if err != nil {
		t.Fatalf("parse Markdown: %v", err)
	}
	_ = parsed.Close()
	if closeCount.Load() != 1 {
		t.Fatalf("source Close calls=%d, want 1", closeCount.Load())
	}

	closeCount.Store(0)
	closeFailure := errors.New("close failed")
	source.Open = func(context.Context) (io.ReadCloser, error) {
		return &countingReadCloser{
			Reader: bytes.NewReader(data), count: &closeCount, closeErr: closeFailure,
		}, nil
	}
	if _, err := NewLocalParser(nil, 300).Parse(
		context.Background(), source, ParseOptions{Mode: config.ParseModeStandard},
	); !errors.Is(err, closeFailure) {
		t.Fatalf("close failure was not propagated: %v", err)
	}
	if closeCount.Load() != 1 {
		t.Fatalf("failed source Close calls=%d, want 1", closeCount.Load())
	}
}

func TestPDFSpoolJoinsReadAndCloseFailuresAndStillCleans(t *testing.T) {
	readFailure := errors.New("read failed")
	closeFailure := errors.New("close failed")
	data := []byte("x")
	source := testDocumentSource("pdf", "broken.pdf", data, sourceCopyBufferBytes)
	source.Open = func(context.Context) (io.ReadCloser, error) {
		return &failingReadCloser{readErr: readFailure, closeErr: closeFailure}, nil
	}
	tempDir := t.TempDir()
	if _, _, err := spoolSourceFile(context.Background(), source, tempDir); err == nil ||
		!errors.Is(err, readFailure) || !errors.Is(err, closeFailure) {
		t.Fatalf("spool error=%v, want joined read/close failures", err)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed spool left temporary files: %v", entries)
	}
}

func testDocumentSource(format, fileName string, data []byte, maxRead int) document.Source {
	hash := sha256.Sum256(data)
	return document.Source{
		DocID:    "doc_test",
		FileName: fileName,
		Format:   format,
		Size:     int64(len(data)),
		SHA256:   hex.EncodeToString(hash[:]),
		Open: func(ctx context.Context) (io.ReadCloser, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return &guardReadCloser{reader: bytes.NewReader(data), maxRead: maxRead}, nil
		},
	}
}

type guardReadCloser struct {
	reader  *bytes.Reader
	reads   int
	minRead int
	maxRead int
}

func (r *guardReadCloser) Read(value []byte) (int, error) {
	if r.reads == 0 && len(value) < r.minRead {
		return 0, errors.New("parser used an io.ReadAll-style growing buffer")
	}
	r.reads++
	if len(value) > r.maxRead {
		return 0, errors.New("parser requested an unbounded read buffer")
	}
	return r.reader.Read(value)
}

func (*guardReadCloser) Close() error { return nil }

type countingReadCloser struct {
	io.Reader
	count    *atomic.Int32
	closeErr error
}

type failingReadCloser struct {
	readErr  error
	closeErr error
}

func (r *failingReadCloser) Read([]byte) (int, error) { return 0, r.readErr }
func (r *failingReadCloser) Close() error             { return r.closeErr }

func (r *countingReadCloser) Close() error {
	if r.count.Add(1) > 1 {
		return errors.New("source closed more than once")
	}
	return r.closeErr
}
