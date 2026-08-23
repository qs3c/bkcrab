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
	if text.Track != DatasetTrackTextRAG || text.CorpusLimit != 1_000 || !reflect.DeepEqual(text.EvidenceTypes, []string{"text"}) {
		t.Fatalf("text defaults=%+v", text)
	}
	textSubset := CatalogImportOptions{CatalogID: CatalogOpenRAGBench, Track: DatasetTrackTextRAG, SampleSize: 20, CorpusLimit: 50}
	if err := textSubset.ApplyDefaults(); err != nil || textSubset.CorpusLimit != 50 {
		t.Fatalf("text subset=%+v error=%v", textSubset, err)
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
	tooLarge := CatalogImportOptions{CatalogID: CatalogOpenRAGBench, CorpusLimit: 1_001}
	if err := tooLarge.ApplyDefaults(); err == nil {
		t.Fatal("Open RAGBench accepted an oversized corpus limit")
	}
}

func TestSelectOpenRAGCorpusKeepsPositivesAndSamplesNegatives(t *testing.T) {
	cases := []Case{
		{ID: "c1", Metadata: map[string]any{"sourceDocId": "positive-1"}},
		{ID: "c2", Metadata: map[string]any{"sourceDocId": "positive-2"}},
		{ID: "c3", Metadata: map[string]any{"sourceDocId": "positive-1"}},
	}
	available := []string{"negative-4", "positive-2", "negative-3", "positive-1", "negative-2", "negative-1"}
	selectedCases, documents, err := selectOpenRAGCorpus(cases, available, 4, 42, "test/positive", "test/negative")
	if err != nil {
		t.Fatal(err)
	}
	if len(selectedCases) != len(cases) || len(documents) != 4 || !containsString(documents, "positive-1") || !containsString(documents, "positive-2") {
		t.Fatalf("selected cases=%v documents=%v", selectedCases, documents)
	}
	repeatedCases, repeatedDocuments, err := selectOpenRAGCorpus(cases,
		[]string{"negative-1", "negative-2", "positive-1", "negative-3", "positive-2", "negative-4"},
		4, 42, "test/positive", "test/negative")
	if err != nil || !reflect.DeepEqual(selectedCases, repeatedCases) || !reflect.DeepEqual(documents, repeatedDocuments) {
		t.Fatalf("stable selection drifted: cases=%v documents=%v error=%v", repeatedCases, repeatedDocuments, err)
	}

	filteredCases, filteredDocuments, err := selectOpenRAGCorpus(cases, available, 1, 42, "test/positive", "test/negative")
	if err != nil {
		t.Fatal(err)
	}
	if len(filteredDocuments) != 1 || len(filteredCases) == 0 {
		t.Fatalf("filtered cases=%v documents=%v", filteredCases, filteredDocuments)
	}
	for _, item := range filteredCases {
		if caseSourceDocID(item) != filteredDocuments[0] {
			t.Fatalf("case %s references excluded document %s", item.ID, caseSourceDocID(item))
		}
	}
}
