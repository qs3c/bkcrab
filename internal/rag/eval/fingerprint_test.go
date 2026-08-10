package eval

import (
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestFingerprintCanonicalOrderingAndArtifactIdentity(t *testing.T) {
	first := validDataset()
	first.Corpus = append(first.Corpus, CorpusDocument{ID: "doc-2", FileName: "two.txt", MediaType: "text/plain", SHA256: strings.Repeat("b", 64), Metadata: map[string]any{"z": 1, "a": 2}})
	first.Cases = append(first.Cases, Case{ID: "case-2", UserInput: "second", Tags: []string{"z", "a"}})
	first.Corpus[0].Metadata = map[string]any{"department": "finance", "apiKey": "secret-one", "nested": map[string]string{"endpoint": "https://one.invalid", "stable": "yes"}}
	first.Corpus[0].ObjectKey = "staging/object-one"
	firstHash, err := DatasetFingerprint(first)
	if err != nil {
		t.Fatal(err)
	}

	second := first
	second.Name = "renamed"
	second.Description = "not content identity"
	second.Corpus = append([]CorpusDocument(nil), first.Corpus...)
	second.Cases = append([]Case(nil), first.Cases...)
	second.Corpus[0], second.Corpus[1] = second.Corpus[1], second.Corpus[0]
	second.Cases[0], second.Cases[1] = second.Cases[1], second.Cases[0]
	for index := range second.Corpus {
		if second.Corpus[index].ID == "doc-1" {
			second.Corpus[index].ObjectKey = "ready/object-two"
			second.Corpus[index].Metadata = map[string]any{"nested": map[string]string{"stable": "yes", "endpoint": "https://two.invalid"}, "apiKey": "secret-two", "department": "finance"}
		}
	}
	secondHash, err := DatasetFingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Fatalf("storage keys, order, names or credential metadata changed fingerprint: %s != %s", firstHash, secondHash)
	}

	second.Corpus[0].SHA256 = strings.Repeat("c", 64)
	changedHash, err := DatasetFingerprint(second)
	if err != nil {
		t.Fatal(err)
	}
	if changedHash == firstHash {
		t.Fatal("original artifact SHA-256 did not change fingerprint")
	}
}

func TestIngestionAndProfileFingerprintsValidateClosedInputs(t *testing.T) {
	ingestion := config.RAGIngestionPolicyData{Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard, Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "contract", Model: "embed", Dims: 1024}}
	first, err := IngestionFingerprint(ingestion)
	if err != nil {
		t.Fatal(err)
	}
	second, err := IngestionFingerprint(ingestion)
	if err != nil || first != second {
		t.Fatalf("ingestion fingerprint=%q/%q err=%v", first, second, err)
	}
	ingestion.Embedding.Dims = 0
	if _, err := IngestionFingerprint(ingestion); err == nil {
		t.Fatal("invalid ingestion policy was fingerprinted")
	}
}
