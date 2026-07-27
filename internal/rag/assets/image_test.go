package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/png"
	"io"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func testRasterPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	value := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value.SetNRGBA(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 77, A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, value); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestSafeImageReencodesMetadataFreeDisplayAndThumbnail(t *testing.T) {
	raw := testRasterPNG(t, 120, 60)
	variants, err := MakeDisplayVariants(context.Background(), raw, "image/png", SafeImageLimits{
		MaxSourceBytes: int64(len(raw)), MaxPixels: 10_000,
		DisplayMaxEdge: 80, ThumbnailMaxEdge: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if variants.Display.MIMEType != "image/png" || variants.Display.Width != 80 || variants.Display.Height != 40 {
		t.Fatalf("display=%+v", variants.Display)
	}
	if variants.Thumbnail.Width != 24 || variants.Thumbnail.Height != 12 {
		t.Fatalf("thumbnail=%+v", variants.Thumbnail)
	}
	if !document.CanonicalSHA256(variants.Display.SHA256) || !document.CanonicalSHA256(variants.Thumbnail.SHA256) {
		t.Fatal("derived hashes are not canonical")
	}
	for name, value := range map[string]EncodedRaster{"display": variants.Display, "thumbnail": variants.Thumbnail} {
		config, format, decodeErr := image.DecodeConfig(bytes.NewReader(value.Bytes))
		if decodeErr != nil || format != "png" || config.Width != value.Width || config.Height != value.Height {
			t.Fatalf("%s decode format=%q config=%+v err=%v", name, format, config, decodeErr)
		}
	}
}

func TestSafeImageDropsAdditionalGIFFrames(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	first := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	second := image.NewPaletted(image.Rect(0, 0, 2, 2), palette)
	for index := range second.Pix {
		second.Pix[index] = 1
	}
	var raw bytes.Buffer
	if err := gif.EncodeAll(&raw, &gif.GIF{Image: []*image.Paletted{first, second}, Delay: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	variants, err := MakeDisplayVariants(context.Background(), raw.Bytes(), "image/gif", SafeImageLimits{
		MaxSourceBytes: 1024, MaxPixels: 4, DisplayMaxEdge: 2, ThumbnailMaxEdge: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gif.DecodeAll(bytes.NewReader(variants.Display.Bytes)); err == nil {
		t.Fatal("safe display unexpectedly remained an animated GIF")
	}
	if _, format, err := image.Decode(bytes.NewReader(variants.Display.Bytes)); err != nil || format != "png" {
		t.Fatalf("safe display format=%q err=%v", format, err)
	}
}

func TestSafeImageRejectsUnsafeAndOverLimitSources(t *testing.T) {
	raw := testRasterPNG(t, 10, 10)
	limits := SafeImageLimits{MaxSourceBytes: int64(len(raw)), MaxPixels: 100, DisplayMaxEdge: 10, ThumbnailMaxEdge: 5}
	for _, mimeType := range []string{"image/svg+xml", "image/emf", "image/wmf", "image/webp"} {
		if _, err := MakeDisplayVariants(context.Background(), []byte("unsafe"), mimeType, limits); !errors.Is(err, ErrUnsupportedRaster) {
			t.Fatalf("MIME %q error=%v, want ErrUnsupportedRaster", mimeType, err)
		}
	}
	if _, err := MakeDisplayVariants(context.Background(), raw, "image/jpeg", limits); err == nil {
		t.Fatal("declared/decoded MIME mismatch accepted")
	}
	limits.MaxPixels = 99
	if _, err := MakeDisplayVariants(context.Background(), raw, "image/png", limits); err == nil {
		t.Fatal("pixel bomb accepted")
	}
	limits.MaxPixels = 100
	limits.MaxSourceBytes = int64(len(raw) - 1)
	if _, err := MakeDisplayVariants(context.Background(), raw, "image/png", limits); err == nil {
		t.Fatal("oversized source accepted")
	}
}

func TestSafeImageCropUsesNormalizedBBoxAndFreshEncoding(t *testing.T) {
	raw := testRasterPNG(t, 100, 80)
	crop, err := CropRaster(context.Background(), raw, "image/png", document.NormalizedBBox{250, 250, 750, 750}, SafeImageLimits{
		MaxSourceBytes: int64(len(raw)), MaxPixels: 8000, DisplayMaxEdge: 100, ThumbnailMaxEdge: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if crop.Width != 50 || crop.Height != 40 || crop.MIMEType != "image/png" {
		t.Fatalf("crop=%+v", crop)
	}
	if bytes.Equal(crop.Bytes, raw) {
		t.Fatal("crop reused untrusted source encoding")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CropRaster(cancelled, raw, "image/png", document.NormalizedBBox{0, 0, 1000, 1000}, SafeImageLimits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled crop error=%v", err)
	}
}

func TestSafeImagePersisterPublishesDerivedVariants(t *testing.T) {
	raw := testRasterPNG(t, 120, 60)
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)
	persister.Limits.DisplayMaxEdge = 80
	persister.Limits.ThumbnailMaxEdge = 24

	artifact, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 1,
		ParseFingerprint: assetSourceHash,
		Document:         parsedRasterDocument(t, raw, "image/png", 120, 60, "assets/image.png"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Assets) != 1 || artifact.Assets[0].DisplayStatus != document.DisplayReady {
		t.Fatalf("artifact assets=%+v", artifact.Assets)
	}
	record := catalog.records[artifact.Assets[0].ID]
	if record.DisplayStatus != document.DisplayReady || record.DisplayMIME != "image/png" ||
		record.DisplayObjectKey == "" || record.ThumbnailObjectKey == "" {
		t.Fatalf("catalog record=%+v", record)
	}
	display := objects.data[record.DisplayObjectKey]
	thumbnail := objects.data[record.ThumbnailObjectKey]
	if len(display) == 0 || len(thumbnail) == 0 {
		t.Fatalf("missing derived objects: display=%d thumbnail=%d", len(display), len(thumbnail))
	}
	displayHash := sha256.Sum256(display)
	if hex.EncodeToString(displayHash[:]) != record.DisplaySHA256 {
		t.Fatalf("display hash=%s record=%s", hex.EncodeToString(displayHash[:]), record.DisplaySHA256)
	}
	thumbnailHash := sha256.Sum256(thumbnail)
	if hex.EncodeToString(thumbnailHash[:]) != record.ThumbnailSHA256 {
		t.Fatalf("thumbnail hash=%s record=%s", hex.EncodeToString(thumbnailHash[:]), record.ThumbnailSHA256)
	}
	for name, expected := range map[string]struct {
		width, height int
		data          []byte
	}{
		"display":   {width: 80, height: 40, data: display},
		"thumbnail": {width: 24, height: 12, data: thumbnail},
	} {
		config, format, decodeErr := image.DecodeConfig(bytes.NewReader(expected.data))
		if decodeErr != nil || format != "png" || config.Width != expected.width || config.Height != expected.height {
			t.Fatalf("%s format=%q config=%+v err=%v", name, format, config, decodeErr)
		}
	}
	if got := objects.putOrder[len(objects.putOrder)-1]; !strings.HasSuffix(got, "/parsed.json") {
		t.Fatalf("artifact was not published last: %v", objects.putOrder)
	}
	delete(objects.data, record.ThumbnailObjectKey)
	if _, hit, loadErr := persister.LoadParsedArtifact(context.Background(), CacheRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 1,
		ParseFingerprint: assetSourceHash,
	}); loadErr != nil || hit {
		t.Fatalf("missing thumbnail cache hit=%v err=%v", hit, loadErr)
	}
}

func TestSafeImagePersisterBackfillsPhaseBThumbnailHash(t *testing.T) {
	raw := testRasterPNG(t, 40, 20)
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)

	first, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 1,
		ParseFingerprint: assetSourceHash,
		Document:         parsedRasterDocument(t, raw, "image/png", 40, 20, "assets/image.png"),
	})
	if err != nil {
		t.Fatal(err)
	}
	record := catalog.records[first.Assets[0].ID]
	record.ThumbnailSHA256 = ""
	catalog.records[record.ID] = record

	if _, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 2,
		ParseFingerprint: assetSourceHash,
		Document:         parsedRasterDocument(t, raw, "image/png", 40, 20, "assets/image.png"),
	}); err != nil {
		t.Fatalf("rebuild Phase B asset: %v", err)
	}
	backfilled := catalog.records[record.ID]
	thumbnail := objects.data[backfilled.ThumbnailObjectKey]
	hash := sha256.Sum256(thumbnail)
	if backfilled.ThumbnailSHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("thumbnail hash=%q actual=%q", backfilled.ThumbnailSHA256, hex.EncodeToString(hash[:]))
	}
}

func TestSafeImagePersisterKeepsUnsafeSourceUnavailable(t *testing.T) {
	raw := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	objects := newMemoryObjects()
	catalog := newMemoryCatalog()
	persister := testPersister(objects, catalog)

	artifact, err := persister.PersistParsedDocument(context.Background(), PersistRequest{
		UserID: "user_1", KBID: "kb_1", DocID: "doc_1", DocVersion: 1,
		ParseFingerprint: assetSourceHash,
		Document:         parsedRasterDocument(t, raw, "image/svg+xml", 10, 10, "assets/image.svg"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.Assets[0].DisplayStatus != document.DisplayUnavailable {
		t.Fatalf("unsafe asset became displayable: %+v", artifact.Assets[0])
	}
	if len(artifact.Warnings) != 1 || artifact.Warnings[0].Code != "asset_display_unavailable" || !artifact.Warnings[0].Degraded {
		t.Fatalf("warnings=%+v", artifact.Warnings)
	}
	record := catalog.records[artifact.Assets[0].ID]
	if record.DisplayStatus != document.DisplayUnavailable || record.DisplayObjectKey != "" || record.ThumbnailObjectKey != "" {
		t.Fatalf("unsafe catalog record=%+v", record)
	}
	for key := range objects.data {
		if strings.HasSuffix(key, "/display.webp") || strings.HasSuffix(key, "/thumbnail.webp") {
			t.Fatalf("unsafe source produced browser object %q", key)
		}
	}
}

func parsedRasterDocument(t *testing.T, raw []byte, mimeType string, width, height int, entry string) *document.ParsedDocument {
	t.Helper()
	hash := sha256.Sum256(raw)
	contentHash := hex.EncodeToString(hash[:])
	location := document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "第 1 页"}
	return document.NewParsedDocument(document.ParsedDocumentInput{
		SchemaVersion: document.ParsedDocumentSchemaVersion,
		Source: document.ParsedSource{
			DocID: "doc_1", FileName: "guide.pdf", Format: "pdf", ByteSize: int64(len(raw)), SHA256: assetSourceHash,
		},
		Parser: document.ParserInfo{Name: "fake", Version: "v1"},
		Units: []document.MarkdownUnit{{
			ID: "unit_page_0001", Location: location, Markdown: "![图](rag-asset://occ_1)",
		}},
		Assets: []document.ExtractedAsset{{
			LocalID: "asset_local", ContentSHA256: contentHash, Kind: document.AssetKindImage,
			SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: mimeType,
			Width: width, Height: height, ByteSize: int64(len(raw)), BundleEntry: entry,
		}},
		Occurrences: []document.AssetOccurrence{{
			ID: "occ_1", AssetLocalID: "asset_local", UnitID: "unit_page_0001", Order: 1,
			Location: location, AltText: "图", Confidence: 1,
		}},
	}, func(ctx context.Context, requested string) (io.ReadCloser, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if requested != entry {
			return nil, errors.New("unexpected bundle entry")
		}
		return io.NopCloser(bytes.NewReader(raw)), nil
	}, nil)
}
