package document

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func validArtifact(t *testing.T) *ParsedArtifact {
	t.Helper()
	id, err := AssetID("doc_1", testAssetHash)
	if err != nil {
		t.Fatal(err)
	}
	bbox := NormalizedBBox{100, 200, 700, 800}
	return &ParsedArtifact{
		SchemaVersion: ParsedArtifactSchemaVersion,
		Source:        ParsedSource{DocID: "doc_1", FileName: "guide.pdf", Format: "pdf", ByteSize: 123, SHA256: testSourceHash},
		Parser:        ParserInfo{Name: "pdf-fake", Version: "v1"},
		Units: []MarkdownUnit{{
			ID: "unit_page_0001", Location: SourceLocation{Kind: LocationPage, Index: 1, Label: "第 1 页"},
			Markdown: "正文\n\n![架构图](rag-asset://occ_1)",
		}},
		Assets: []ArtifactAsset{{
			ID: id, ContentSHA256: testAssetHash, Kind: AssetKindImage,
			SourceKind: SourceKindEmbeddedOriginal, SourceMIME: "image/png",
			Width: 20, Height: 10, ByteSize: 4, DisplayStatus: DisplayUnavailable,
		}},
		Occurrences: []ArtifactOccurrence{{
			ID: "occ_1", AssetID: id, UnitID: "unit_page_0001", Order: 1,
			Location: SourceLocation{Kind: LocationPage, Index: 1, Label: "第 1 页"},
			BBox:     &bbox, Caption: "架构图", Confidence: 1,
		}},
	}
}

func TestParsedArtifactJSONRoundTripExcludesSensitiveMaterial(t *testing.T) {
	artifact := validArtifact(t)
	encoded, err := EncodeArtifact(artifact, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"BundleEntry", "bundleEntry", "objectKey", "http://", "https://", "file://", "base64"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("artifact JSON contains forbidden material %q: %s", forbidden, encoded)
		}
	}
	decoded, err := DecodeArtifact(bytes.NewReader(encoded), int64(len(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(encoded, reencoded) {
		t.Fatalf("round trip drift\nfirst=%s\nsecond=%s\nerr=%v", encoded, reencoded, err)
	}

	if _, err := DecodeArtifact(strings.NewReader(string(encoded)), int64(len(encoded)-1)); err == nil {
		t.Fatal("artifact exceeding byte limit must be rejected")
	}
	withUnknown := strings.TrimSuffix(string(encoded), "}") + `,"unknown":true}`
	if _, err := DecodeArtifact(strings.NewReader(withUnknown), int64(len(withUnknown))); err == nil {
		t.Fatal("unknown artifact JSON field must be rejected")
	}
}

func TestArtifactValidateRejectsDanglingAndNonCanonicalAssets(t *testing.T) {
	artifact := validArtifact(t)
	if err := artifact.Validate(); err != nil {
		t.Fatalf("valid artifact rejected: %v", err)
	}
	artifact.Occurrences[0].AssetID = "ast_missing"
	if err := artifact.Validate(); err == nil {
		t.Fatal("dangling artifact occurrence must be rejected")
	}
	artifact = validArtifact(t)
	artifact.Assets[0].ID = "ast_00000000000000000000000000000000"
	if err := artifact.Validate(); err == nil {
		t.Fatal("asset ID not derived from doc+content hash must be rejected")
	}
}

func TestArtifactObjectKeysAndFingerprintsUseSafeSegments(t *testing.T) {
	keys, err := NewObjectKeys("user_1", "kb_1", "doc_1", testAssetHash, "image/png", testSourceHash)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := "rag/user_1/kb_1/doc_1/"
	for _, key := range []string{keys.ArtifactJSON, keys.NormalizedMarkdown, keys.AssetSource, keys.AssetDisplay, keys.AssetThumbnail} {
		if !strings.HasPrefix(key, wantPrefix) || strings.Contains(key, "\\") || strings.Contains(key, "..") {
			t.Fatalf("unsafe object key %q", key)
		}
	}
	if _, err := NewObjectKeys("../user", "kb", "doc", testAssetHash, "image/png", testSourceHash); err == nil {
		t.Fatal("unsafe tenant path segment must be rejected")
	}
	if _, err := ArtifactJSONKey("user", "kb", "doc", "../fingerprint"); err == nil {
		t.Fatal("unsafe fingerprint segment must be rejected")
	}

	parseA, err := ParseFingerprint(ParseFingerprintInput{
		SourceSHA256: testSourceHash, ParseMode: "auto", ParserVersion: "parser-v1",
		MarkItDownVersion: "0.1.6", PDFRenderDPI: 180, PDFRoutingVersion: "route-v1",
		VisionProviderFingerprint: testAssetHash, VisionModel: "vision", VisionPromptVersion: "prompt-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	parseB, _ := ParseFingerprint(ParseFingerprintInput{
		SourceSHA256: testSourceHash, ParseMode: "auto", ParserVersion: "parser-v1",
		MarkItDownVersion: "0.1.6", PDFRenderDPI: 181, PDFRoutingVersion: "route-v1",
		VisionProviderFingerprint: testAssetHash, VisionModel: "vision", VisionPromptVersion: "prompt-v1",
	})
	if parseA == parseB || len(parseA) != 64 {
		t.Fatalf("parse fingerprints did not separate settings: %q %q", parseA, parseB)
	}
	pageA := PageCacheKey([]byte("render"), testAssetHash, "vision", "prompt-v1", "page-v1")
	pageB := PageCacheKey([]byte("render2"), testAssetHash, "vision", "prompt-v1", "page-v1")
	if pageA == pageB || len(pageA) != 64 {
		t.Fatalf("page cache keys = %q %q", pageA, pageB)
	}
	if EnrichmentCacheKey("raw", "table", testAssetHash, "text", "prompt", "schema") ==
		EnrichmentCacheKey("raw", "code", testAssetHash, "text", "prompt", "schema") {
		t.Fatal("enrichment cache key must include block kind")
	}
}

func TestCanonicalizeFoldsAltAndDeduplicatesOccurrences(t *testing.T) {
	input := validParsedInput()
	input.Occurrences = append(input.Occurrences,
		AssetOccurrence{
			ID: "occ_2", AssetLocalID: "asset_1", UnitID: "unit_page_0001", Order: 2,
			Location: SourceLocation{Kind: LocationPage, Index: 1, Label: "第 1 页"},
			AltText:  "第二处替代文字", Caption: "模型说明", Confidence: .9,
		},
	)
	input.Units[0].Markdown += "\n\n![模型说明](rag-asset://occ_2)"
	doc := NewParsedDocument(input, nil, nil)
	artifact, err := Canonicalize(doc, "图片（未进行视觉识别）")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Assets) != 1 || len(artifact.Occurrences) != 2 {
		t.Fatalf("assets=%d occurrences=%d", len(artifact.Assets), len(artifact.Occurrences))
	}
	if artifact.Occurrences[0].Caption != "架构图" || artifact.Occurrences[1].Caption != "模型说明" {
		t.Fatalf("captions = %+v", artifact.Occurrences)
	}
	encoded, _ := json.Marshal(artifact)
	if bytes.Contains(encoded, []byte("altText")) {
		t.Fatalf("canonical artifact retained transient altText: %s", encoded)
	}
}
