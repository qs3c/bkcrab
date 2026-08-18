package eval

import (
	"reflect"
	"testing"
)

func TestBuiltinCatalogContainsOnlyRAGDatasets(t *testing.T) {
	items := BuiltinCatalog()
	if len(items) != 3 {
		t.Fatalf("catalog size=%d", len(items))
	}
	want := []string{CatalogMultiDoc2Dial, CatalogTATQA, CatalogOpenRAGBench}
	for index, id := range want {
		if items[index].ID != id || items[index].Revision == "" || items[index].AdapterVersion == "" {
			t.Fatalf("catalog[%d]=%+v", index, items[index])
		}
	}
}

func TestStableSampleIDsReproducibleAndSeeded(t *testing.T) {
	ids := []string{"case-5", "case-1", "case-4", "case-3", "case-2"}
	first, err := StableSampleIDs("dataset@revision", ids, 3, 42)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := StableSampleIDs("dataset@revision", []string{"case-2", "case-1", "case-5", "case-4", "case-3"}, 3, 42)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same seed drifted: %v != %v", first, second)
	}
	other, _ := StableSampleIDs("dataset@revision", ids, 3, 2026)
	if reflect.DeepEqual(first, other) {
		t.Fatalf("different seeds selected the same small sample: %v", first)
	}
}

func TestCatalogOptionsKeepTextAndPDFTracksSeparate(t *testing.T) {
	text := CatalogImportOptions{CatalogID: CatalogOpenRAGBench}
	if err := text.ApplyDefaults(); err != nil {
		t.Fatal(err)
	}
	if text.Track != DatasetTrackTextRAG || text.CorpusLimit != 0 || !reflect.DeepEqual(text.EvidenceTypes, []string{"text"}) {
		t.Fatalf("text defaults=%+v", text)
	}
	pdf := CatalogImportOptions{CatalogID: CatalogOpenRAGBench, Track: DatasetTrackPDFE2E, SampleSize: 20}
	if err := pdf.ApplyDefaults(); err != nil {
		t.Fatal(err)
	}
	if pdf.CorpusLimit != 50 {
		t.Fatalf("pdf defaults=%+v", pdf)
	}
	invalid := CatalogImportOptions{CatalogID: CatalogTATQA, Track: DatasetTrackPDFE2E}
	if err := invalid.ApplyDefaults(); err == nil {
		t.Fatal("TAT-QA accepted PDF track")
	}
}
