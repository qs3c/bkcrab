package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func sharedGoldenPath(name string) string {
	return filepath.Join("..", "..", "..", "..", "testdata", "rag-parser-protocol", "v2", name)
}

func readSharedGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(sharedGoldenPath(name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSharedManifestGoldensDecodeStrictly(t *testing.T) {
	tests := []struct {
		name           string
		kind           BundleKind
		requestedPages []int
	}{
		{"manifest-office.json", BundleKindOfficeConvert, nil},
		{"manifest-pdf-analyze.json", BundleKindPDFAnalyze, nil},
		{"manifest-pdf-render.json", BundleKindPDFRender, []int{1}},
		{"manifest-pdf-failed-page.json", BundleKindPDFRender, []int{1, 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var manifest Manifest
			if err := decodeStrict(readSharedGolden(t, test.name), &manifest); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if err := ValidateManifest(&manifest, DecodeOptions{
				ExpectedKind:   test.kind,
				ExpectedSource: manifest.Source,
				RequestedPages: test.requestedPages,
			}); err != nil {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}

func TestSharedHealthGoldenDecodesStrictly(t *testing.T) {
	var health Health
	if err := decodeStrict(readSharedGolden(t, "health.json"), &health); err != nil {
		t.Fatal(err)
	}
	if err := validateHealth(&health); err != nil {
		t.Fatal(err)
	}
	if health.Limits.MaxInputBytes != 52_428_800 || health.Limits.MaxOutputBytes != 209_715_200 ||
		!health.Capabilities.Office.Enabled || health.Capabilities.PDF.Enabled {
		t.Fatalf("unexpected health: %+v", health)
	}
}

func TestSharedPagePrimitiveGoldenDecodesStrictly(t *testing.T) {
	primitive, err := DecodePagePrimitive(bytes.NewReader(readSharedGolden(t, "page-primitive.json")))
	if err != nil {
		t.Fatal(err)
	}
	if primitive.Page != 1 || primitive.BlockCount != 1 || len(primitive.TextBlocks) != 1 ||
		len(primitive.EmbeddedImages) != 1 {
		t.Fatalf("unexpected primitive: %+v", primitive)
	}
}

func TestSharedPDFAnalyzeAndRenderGoldensFormAStablePair(t *testing.T) {
	var analyze, render Manifest
	if err := decodeStrict(readSharedGolden(t, "manifest-pdf-analyze.json"), &analyze); err != nil {
		t.Fatal(err)
	}
	if err := decodeStrict(readSharedGolden(t, "manifest-pdf-render.json"), &render); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePDFBundlePair(&analyze, &render, []int{1}); err != nil {
		t.Fatalf("valid analyze/render pair: %v", err)
	}
	if err := ValidatePDFBundlePair(&analyze, &render, []int{2}); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("mismatched requested pages error=%v", err)
	}
	unknownPage := render
	unknownPage.Pages = append([]PageDescriptor(nil), render.Pages...)
	unknownPage.Occurrences = append([]OccurrenceDescriptor(nil), render.Occurrences...)
	unknownPage.Pages[0].Page = 2
	unknownPage.Pages[0].UnitID = "unit_page_aaaaaaaaaaaa_0002"
	unknownPage.Occurrences[0].UnitID = unknownPage.Pages[0].UnitID
	unknownPage.Occurrences[0].Location.Index = 2
	if err := ValidatePDFBundlePair(&analyze, &unknownPage, []int{2}); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("render page absent from analyze catalog error=%v", err)
	}
}

func TestProtocolObjectsRejectMissingAndDuplicateFields(t *testing.T) {
	health := readSharedGolden(t, "health.json")
	var raw map[string]any
	if err := json.Unmarshal(health, &raw); err != nil {
		t.Fatal(err)
	}
	capabilities := raw["capabilities"].(map[string]any)
	pdf := capabilities["pdf"].(map[string]any)
	delete(pdf, "engineVersion")
	missing, _ := json.Marshal(raw)
	var decoded Health
	if err := decodeStrict(missing, &decoded); err == nil {
		t.Fatal("health with a missing empty-valued field was accepted")
	}

	duplicate := bytes.Replace(health, []byte(`"serviceVersion": "test-build"`),
		[]byte(`"serviceVersion": "test-build", "serviceVersion": "other"`), 1)
	if err := decodeStrict(duplicate, &decoded); err == nil {
		t.Fatal("health with a duplicate field was accepted")
	}
}

func TestPagePrimitiveRejectsUnknownOrInconsistentBlocks(t *testing.T) {
	valid := readSharedGolden(t, "page-primitive.json")
	var raw map[string]any
	if err := json.Unmarshal(valid, &raw); err != nil {
		t.Fatal(err)
	}
	raw["path"] = "C:/secret"
	unknown, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePagePrimitive(bytes.NewReader(unknown)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("unknown field error=%v", err)
	}
	inconsistent := bytes.Replace(valid, []byte(`"blockCount": 1`), []byte(`"blockCount": 2`), 1)
	if _, err := DecodePagePrimitive(bytes.NewReader(inconsistent)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("inconsistent block count error=%v", err)
	}
	delete(raw, "path")
	raw["textChars"] = float64(21)
	inconsistent, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodePagePrimitive(bytes.NewReader(inconsistent)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("inconsistent textChars error=%v", err)
	}
	invalidUTF8 := append([]byte(nil), valid...)
	if index := bytes.Index(invalidUTF8, []byte("Canonical")); index < 0 {
		t.Fatal("page primitive fixture text not found")
	} else {
		invalidUTF8[index] = 0xff
	}
	if _, err := DecodePagePrimitive(bytes.NewReader(invalidUTF8)); !errors.Is(err, ErrInvalidBundle) {
		t.Fatalf("invalid UTF-8 primitive error=%v", err)
	}
}
