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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func testOfficeManifest(markdown []byte) Manifest {
	return Manifest{
		ProtocolVersion: ProtocolVersion,
		BundleKind:      BundleKindOfficeConvert,
		Source: SourceDescriptor{
			Format: "docx", ByteSize: 4, SHA256: strings.Repeat("a", 64),
		},
		Parser: ParserDescriptor{
			Name: "markitdown", Version: "0.1.6", WrapperVersion: ExpectedOfficeWrapper,
		},
		Entries: []EntryDescriptor{{
			Path: "units/0001.md", SHA256: testSHA(markdown), ByteSize: int64(len(markdown)),
			MIMEType: MIMETypeMarkdown,
		}},
		Units: []UnitDescriptor{{
			ID:            "unit_document_0000",
			Location:      Location{Kind: "document", Index: 0, Label: "文档"},
			MarkdownEntry: "units/0001.md",
		}},
		Assets:      []AssetDescriptor{},
		Attachments: []AttachmentDescriptor{},
		Occurrences: []OccurrenceDescriptor{},
		Pages:       []PageDescriptor{},
		Warnings:    []WarningDescriptor{},
	}
}

type testTarEntry struct {
	header tar.Header
	body   []byte
}

type cancelingBundleReader struct {
	reader    *bytes.Reader
	cancel    context.CancelFunc
	remaining int
}

func (r *cancelingBundleReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 256 {
		buffer = buffer[:256]
	}
	read, err := r.reader.Read(buffer)
	r.remaining -= read
	if r.remaining <= 0 {
		r.cancel()
	}
	return read, err
}

func makeTestTar(t *testing.T, manifestJSON []byte, entries ...testTarEntry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	if err := w.WriteHeader(&tar.Header{
		Name: ManifestEntryName, Mode: 0o600, Size: int64(len(manifestJSON)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(manifestJSON); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		header := entry.header
		if header.Typeflag == 0 {
			header.Typeflag = tar.TypeReg
		}
		if header.Mode == 0 {
			header.Mode = 0o600
		}
		header.Size = int64(len(entry.body))
		if err := w.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(entry.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func defaultDecodeOptions(manifest Manifest) DecodeOptions {
	return DecodeOptions{
		ExpectedKind: manifest.BundleKind,
		ExpectedSource: SourceDescriptor{
			Format: manifest.Source.Format, ByteSize: manifest.Source.ByteSize, SHA256: manifest.Source.SHA256,
		},
		Limits: DecodeLimits{
			MaxManifestBytes: 1 << 20,
			MaxEntries:       32,
			MaxPages:         300,
			MaxAssets:        500,
			MaxImagePixels:   40_000_000,
			MaxEntryBytes:    1 << 20,
			MaxAssetBytes:    1 << 20,
			MaxRenderBytes:   1 << 20,
			MaxTotalBytes:    4 << 20,
		},
	}
}

func TestDecodeBundleValidatesAndCleansTemporaryEntries(t *testing.T) {
	markdown := []byte("# 标题\n")
	manifest := testOfficeManifest(markdown)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeTestTar(t, manifestJSON, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})

	handle, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), defaultDecodeOptions(manifest))
	if err != nil {
		t.Fatalf("DecodeBundle: %v", err)
	}
	root := handle.root
	entry, err := handle.OpenEntry(context.Background(), "units/0001.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(entry)
	closeErr := entry.Close()
	if err != nil || closeErr != nil || !bytes.Equal(got, markdown) {
		t.Fatalf("entry=%q readErr=%v closeErr=%v", got, err, closeErr)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary bundle root still exists: %v", err)
	}
	if _, err := handle.OpenEntry(context.Background(), "units/0001.md"); err == nil {
		t.Fatal("OpenEntry after Close unexpectedly succeeded")
	}
}

func TestDecodeBundleRejectsUnknownManifestField(t *testing.T) {
	markdown := []byte("ok")
	manifest := testOfficeManifest(markdown)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	encoded = bytes.Replace(encoded, []byte(`"warnings":[]`), []byte(`"warnings":[],"futureField":true`), 1)
	bundle := makeTestTar(t, encoded, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})
	if _, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), defaultDecodeOptions(manifest)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("error=%v, want ErrInvalidBundle", err)
	}
}

func TestValidateManifestRejectsOfficeParserVersionDrift(t *testing.T) {
	manifest := testOfficeManifest([]byte("ok"))
	manifest.Parser.Version = "future-version"
	if err := ValidateManifest(&manifest, defaultDecodeOptions(manifest)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("parser version drift error=%v", err)
	}
}

func TestValidateManifestAcceptsOccurrenceBoundVisioAttachment(t *testing.T) {
	manifest := testOfficeManifest([]byte("![Visio](rag-asset://occ_visio)"))
	manifest.Entries = append([]EntryDescriptor{
		{Path: "assets/asset_visio.png", SHA256: strings.Repeat("b", 64), ByteSize: 64, MIMEType: "image/png"},
		{Path: "attachments/attachment_visio.vsdx", SHA256: strings.Repeat("c", 64), ByteSize: 128, MIMEType: MIMETypeVSDX},
	}, manifest.Entries...)
	manifest.Assets = []AssetDescriptor{{
		LocalID: "asset_visio", Entry: "assets/asset_visio.png", Kind: "image",
		SourceKind: "embedded_preview", Width: 100, Height: 80,
	}}
	manifest.Attachments = []AttachmentDescriptor{{
		LocalID: "attachment_visio", Entry: "attachments/attachment_visio.vsdx",
		Kind: "visio_source", FileName: "architecture.vsdx",
	}}
	manifest.Occurrences = []OccurrenceDescriptor{{
		ID: "occ_visio", AssetLocalID: "asset_visio", UnitID: "unit_document_0000",
		Order: 1, Location: manifest.Units[0].Location, AltText: "Visio",
		Confidence: 1, AttachmentLocalID: "attachment_visio",
	}}
	if err := ValidateManifest(&manifest, defaultDecodeOptions(manifest)); err != nil {
		t.Fatalf("valid Visio manifest rejected: %v", err)
	}

	dangling := manifest
	dangling.Occurrences = append([]OccurrenceDescriptor(nil), manifest.Occurrences...)
	dangling.Occurrences[0].AttachmentLocalID = ""
	if err := ValidateManifest(&dangling, defaultDecodeOptions(dangling)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("dangling attachment error=%v", err)
	}

	unsafeName := manifest
	unsafeName.Attachments = append([]AttachmentDescriptor(nil), manifest.Attachments...)
	unsafeName.Attachments[0].FileName = "../architecture.vsdx"
	if err := ValidateManifest(&unsafeName, defaultDecodeOptions(unsafeName)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("unsafe attachment filename error=%v", err)
	}
}

func TestVerifyEntryFileAcceptsOnlyZIPSniffForDeclaredVSDX(t *testing.T) {
	validPath := filepath.Join(t.TempDir(), "architecture.vsdx")
	if err := os.WriteFile(validPath, []byte("PK\x03\x04test-vsdx"), 0o600); err != nil {
		t.Fatal(err)
	}
	descriptor := EntryDescriptor{Path: "attachments/architecture.vsdx", MIMEType: MIMETypeVSDX}
	if err := verifyEntryFile(validPath, descriptor); err != nil {
		t.Fatalf("ZIP-sniffed VSDX rejected: %v", err)
	}

	invalidPath := filepath.Join(t.TempDir(), "architecture.vsdx")
	if err := os.WriteFile(invalidPath, []byte("not-a-zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyEntryFile(invalidPath, descriptor); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("non-ZIP VSDX error=%v", err)
	}
}

func TestStrictJSONRejectsInvalidUTF8(t *testing.T) {
	data := []byte(`{"protocolVersion":"rag-parser/v2"}`)
	data[len(data)-3] = 0xff
	var manifest Manifest
	if err := decodeStrict(data, &manifest); err == nil {
		t.Fatal("invalid UTF-8 JSON was accepted")
	}
}

func TestDecodeBundleRejectsUnsafeTarAndCatalogMismatch(t *testing.T) {
	markdown := []byte("ok")
	base := testOfficeManifest(markdown)
	tests := []struct {
		name       string
		mutate     func(*Manifest)
		tarEntries func(Manifest) []testTarEntry
	}{
		{
			name: "path traversal",
			mutate: func(m *Manifest) {
				m.Entries[0].Path = "../escape.md"
				m.Units[0].MarkdownEntry = "../escape.md"
			},
		},
		{
			name: "absolute path",
			mutate: func(m *Manifest) {
				m.Entries[0].Path = "/tmp/escape.md"
				m.Units[0].MarkdownEntry = "/tmp/escape.md"
			},
		},
		{
			name: "windows volume",
			mutate: func(m *Manifest) {
				m.Entries[0].Path = `C:\escape.md`
				m.Units[0].MarkdownEntry = `C:\escape.md`
			},
		},
		{
			name:   "hash mismatch",
			mutate: func(m *Manifest) { m.Entries[0].SHA256 = strings.Repeat("b", 64) },
		},
		{
			name: "undeclared tar entry",
			tarEntries: func(m Manifest) []testTarEntry {
				return []testTarEntry{
					{header: tar.Header{Name: m.Entries[0].Path}, body: markdown},
					{header: tar.Header{Name: "units/9999.md"}, body: []byte("extra")},
				}
			},
		},
		{
			name: "duplicate tar entry",
			tarEntries: func(m Manifest) []testTarEntry {
				return []testTarEntry{
					{header: tar.Header{Name: m.Entries[0].Path}, body: markdown},
					{header: tar.Header{Name: m.Entries[0].Path}, body: markdown},
				}
			},
		},
		{
			name: "symlink",
			tarEntries: func(m Manifest) []testTarEntry {
				return []testTarEntry{{
					header: tar.Header{Name: m.Entries[0].Path, Typeflag: tar.TypeSymlink, Linkname: "elsewhere"},
				}}
			},
		},
		{
			name: "hardlink",
			tarEntries: func(m Manifest) []testTarEntry {
				return []testTarEntry{{
					header: tar.Header{Name: m.Entries[0].Path, Typeflag: tar.TypeLink, Linkname: "elsewhere"},
				}}
			},
		},
		{
			name: "pax path override",
			tarEntries: func(m Manifest) []testTarEntry {
				return []testTarEntry{{
					header: tar.Header{
						Name: m.Entries[0].Path, Format: tar.FormatPAX,
						PAXRecords: map[string]string{"path": m.Entries[0].Path},
					},
					body: markdown,
				}}
			},
		},
		{
			name: "gnu extension",
			tarEntries: func(m Manifest) []testTarEntry {
				return []testTarEntry{{
					header: tar.Header{Name: m.Entries[0].Path, Format: tar.FormatGNU}, body: markdown,
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := base
			manifest.Entries = append([]EntryDescriptor(nil), base.Entries...)
			manifest.Units = append([]UnitDescriptor(nil), base.Units...)
			if tt.mutate != nil {
				tt.mutate(&manifest)
			}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			entries := []testTarEntry{{header: tar.Header{Name: manifest.Entries[0].Path}, body: markdown}}
			if tt.tarEntries != nil {
				entries = tt.tarEntries(manifest)
			}
			bundle := makeTestTar(t, encoded, entries...)
			if _, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), defaultDecodeOptions(manifest)); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("error=%v, want ErrInvalidBundle", err)
			}
		})
	}
}

func TestDecodeBundleEnforcesEntryAndTotalQuotas(t *testing.T) {
	markdown := bytes.Repeat([]byte("x"), 32)
	manifest := testOfficeManifest(markdown)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeTestTar(t, encoded, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})

	entryOptions := defaultDecodeOptions(manifest)
	entryOptions.Limits.MaxEntryBytes = 16
	if _, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), entryOptions); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("entry quota error=%v", err)
	}
	totalOptions := defaultDecodeOptions(manifest)
	totalOptions.Limits.MaxTotalBytes = 16
	if _, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), totalOptions); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("total quota error=%v", err)
	}
	archiveOptions := defaultDecodeOptions(manifest)
	archiveOptions.Limits.MaxArchiveBytes = 1024
	if _, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), archiveOptions); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("archive quota error=%v", err)
	}
}

func TestValidateManifestEnforcesDocumentPageQuota(t *testing.T) {
	manifest := Manifest{
		ProtocolVersion: ProtocolVersion,
		BundleKind:      BundleKindPDFAnalyze,
		Source:          SourceDescriptor{Format: "pdf", ByteSize: 10, SHA256: strings.Repeat("c", 64)},
		Parser:          ParserDescriptor{Name: "fake-pdf", Version: "1", WrapperVersion: "pdf-wrapper-v1"},
		Entries:         []EntryDescriptor{},
		Units:           []UnitDescriptor{},
		Assets:          []AssetDescriptor{},
		Attachments:     []AttachmentDescriptor{},
		Occurrences:     []OccurrenceDescriptor{},
		Pages: []PageDescriptor{
			{Page: 1, Status: PageStatusFailed, ErrorCode: "engine_error", UnitID: "unit_page_cccccccccccc_0001"},
			{Page: 2, Status: PageStatusFailed, ErrorCode: "engine_error", UnitID: "unit_page_cccccccccccc_0002"},
		},
		Warnings: []WarningDescriptor{},
	}
	options := defaultDecodeOptions(manifest)
	options.Limits.MaxPages = 1
	if err := ValidateManifest(&manifest, options); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("page quota error=%v", err)
	}
}

func TestValidateManifestEnforcesAssetQuotaBeforeExtraction(t *testing.T) {
	manifest := testOfficeManifest([]byte("ok"))
	manifest.Assets = []AssetDescriptor{{LocalID: "asset_1"}, {LocalID: "asset_2"}}
	options := defaultDecodeOptions(manifest)
	options.Limits.MaxAssets = 1
	if err := ValidateManifest(&manifest, options); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("asset quota error=%v", err)
	}
}

func TestValidateManifestEnforcesAssetPixelQuota(t *testing.T) {
	manifest := testOfficeManifest([]byte("ok"))
	manifest.Assets = []AssetDescriptor{{
		LocalID: "asset_1", Entry: "assets/asset_1.png", Kind: "image", SourceKind: "embedded_original",
		Width: 100, Height: 100,
	}}
	options := defaultDecodeOptions(manifest)
	options.Limits.MaxImagePixels = 9_999
	if err := ValidateManifest(&manifest, options); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("asset pixel quota error=%v", err)
	}
}

func TestValidateManifestEnforcesPerAssetByteQuota(t *testing.T) {
	manifest := testOfficeManifest([]byte("ok"))
	manifest.Entries = append([]EntryDescriptor{{
		Path: "assets/asset_1.png", SHA256: strings.Repeat("d", 64), ByteSize: 10, MIMEType: "image/png",
	}}, manifest.Entries...)
	manifest.Assets = []AssetDescriptor{{
		LocalID: "asset_1", Entry: "assets/asset_1.png", Kind: "image", SourceKind: "embedded_original",
		Width: 10, Height: 10,
	}}
	options := defaultDecodeOptions(manifest)
	options.Limits.MaxAssetBytes = 9
	if err := ValidateManifest(&manifest, options); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("asset byte quota error=%v", err)
	}
}

func TestValidateManifestEnforcesRenderByteQuota(t *testing.T) {
	manifest := Manifest{
		ProtocolVersion: ProtocolVersion,
		BundleKind:      BundleKindPDFRender,
		Source:          SourceDescriptor{Format: "pdf", ByteSize: 10, SHA256: strings.Repeat("c", 64)},
		Parser:          ParserDescriptor{Name: "fake-pdf", Version: "1", WrapperVersion: "pdf-wrapper-v1"},
		Entries: []EntryDescriptor{{
			Path: "pages/page-0001.png", SHA256: strings.Repeat("d", 64), ByteSize: 10, MIMEType: "image/png",
		}},
		Units:       []UnitDescriptor{},
		Assets:      []AssetDescriptor{},
		Attachments: []AttachmentDescriptor{},
		Occurrences: []OccurrenceDescriptor{},
		Pages: []PageDescriptor{{
			Page: 1, Status: PageStatusOK, UnitID: "unit_page_cccccccccccc_0001", RenderEntry: "pages/page-0001.png",
		}},
		Warnings: []WarningDescriptor{},
	}
	options := defaultDecodeOptions(manifest)
	options.RequestedPages = []int{1}
	options.Limits.MaxRenderBytes = 9
	if err := ValidateManifest(&manifest, options); !errors.Is(err, ErrBundleLimitExceeded) {
		t.Fatalf("render byte quota error=%v", err)
	}
}

func TestValidateManifestPDFRequiredForbiddenMatrix(t *testing.T) {
	base := Manifest{
		ProtocolVersion: ProtocolVersion,
		BundleKind:      BundleKindPDFAnalyze,
		Source:          SourceDescriptor{Format: "pdf", ByteSize: 10, SHA256: strings.Repeat("c", 64)},
		Parser:          ParserDescriptor{Name: "fake-pdf", Version: "1", WrapperVersion: "pdf-wrapper-v1"},
		Entries: []EntryDescriptor{
			{Path: "pages/0001.json", SHA256: strings.Repeat("1", 64), ByteSize: 2, MIMEType: MIMETypeJSON},
			{Path: "units/0001.md", SHA256: strings.Repeat("2", 64), ByteSize: 0, MIMEType: MIMETypeMarkdown},
		},
		Units:       []UnitDescriptor{},
		Assets:      []AssetDescriptor{},
		Attachments: []AttachmentDescriptor{},
		Occurrences: []OccurrenceDescriptor{},
		Pages: []PageDescriptor{{
			Page: 1, Status: PageStatusOK, ErrorCode: "", UnitID: "unit_page_cccccccccccc_0001",
			NativeMarkdownEntry: "units/0001.md", PrimitiveEntry: "pages/0001.json", RenderEntry: "",
		}},
		Warnings: []WarningDescriptor{},
	}
	options := defaultDecodeOptions(base)
	if err := ValidateManifest(&base, options); err != nil {
		t.Fatalf("valid analyze manifest: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Manifest)
	}{
		{"ok errorCode nonempty", func(m *Manifest) { m.Pages[0].ErrorCode = "page_parse_failed" }},
		{"analyze render forbidden", func(m *Manifest) { m.Pages[0].RenderEntry = "pages/render.png" }},
		{"analyze missing native", func(m *Manifest) { m.Pages[0].NativeMarkdownEntry = "" }},
		{"analyze missing primitive", func(m *Manifest) { m.Pages[0].PrimitiveEntry = "" }},
		{"failed carries payload", func(m *Manifest) {
			m.Pages[0].Status = PageStatusFailed
			m.Pages[0].ErrorCode = "page_analyze_failed"
		}},
		{"failed missing error", func(m *Manifest) {
			m.Pages[0].Status = PageStatusFailed
			m.Pages[0].NativeMarkdownEntry = ""
			m.Pages[0].PrimitiveEntry = ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manifest := base
			manifest.Pages = append([]PageDescriptor(nil), base.Pages...)
			tt.mutate(&manifest)
			if err := ValidateManifest(&manifest, options); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("error=%v, want ErrInvalidBundle", err)
			}
		})
	}
}

func TestDecodeBundleHonorsCancelledContext(t *testing.T) {
	markdown := []byte("ok")
	manifest := testOfficeManifest(markdown)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeTestTar(t, encoded, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DecodeBundle(ctx, bytes.NewReader(bundle), defaultDecodeOptions(manifest)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
}

func TestDecodeBundleCancellationCleansPartiallyExtractedFiles(t *testing.T) {
	markdown := bytes.Repeat([]byte("bounded markdown\n"), 4096)
	manifest := testOfficeManifest(markdown)
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	bundle := makeTestTar(t, encoded, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})
	ctx, cancel := context.WithCancel(context.Background())
	tempDir := t.TempDir()
	options := defaultDecodeOptions(manifest)
	options.TempDir = tempDir
	reader := &cancelingBundleReader{reader: bytes.NewReader(bundle), cancel: cancel, remaining: 4096}
	if _, err := DecodeBundle(ctx, reader, options); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("cancel left temporary bundle directories: %v", entries)
	}
}

func TestBundleHandleRejectsUnknownEntryAndEscapedPath(t *testing.T) {
	markdown := []byte("ok")
	manifest := testOfficeManifest(markdown)
	encoded, _ := json.Marshal(manifest)
	bundle := makeTestTar(t, encoded, testTarEntry{
		header: tar.Header{Name: "units/0001.md"}, body: markdown,
	})
	handle, err := DecodeBundle(context.Background(), bytes.NewReader(bundle), defaultDecodeOptions(manifest))
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	for _, name := range []string{"missing", "../units/0001.md", filepath.Join("..", "units", "0001.md")} {
		if _, err := handle.OpenEntry(context.Background(), name); err == nil {
			t.Fatalf("OpenEntry(%q) succeeded", name)
		}
	}
}
