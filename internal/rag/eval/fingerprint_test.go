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

func TestGenerationFingerprintChangesForEveryIndexContract(t *testing.T) {
	policy := config.RAGIngestionPolicyData{
		Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard,
		ParserEngine:      "anydoc",
		DocumentAI:        config.RAGPolicyDocumentAIData{VisionModel: "vision", TextModel: "text", VisionPromptVersion: "vp1", EnrichmentPromptVersion: "ep1"},
		EnrichmentEnabled: true,
		Embedding:         config.RAGPolicyEmbeddingData{ContractFingerprint: "contract", Model: "embed", Dims: 1024},
	}
	contract := GenerationContract{ParserProtocolVersion: "protocol-v1", ParserEngineVersion: "engine-v1", TokenizerVersion: "token-v1", SplitterVersion: "split-v1", ArtifactSchemaVersion: 1, VectorSchemaVersion: "vector-v1", IndexFormatVersion: 1}
	documents := []GenerationDocumentFingerprint{{ID: "doc", FileName: "doc.md", MediaType: "text/markdown", SHA256: strings.Repeat("a", 64), SizeBytes: 10}}
	baseline, err := GenerationFingerprint("version", "corpus", documents, policy, contract)
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string]func(*config.RAGIngestionPolicyData, *GenerationContract, *[]GenerationDocumentFingerprint){
		"source sha": func(_ *config.RAGIngestionPolicyData, _ *GenerationContract, docs *[]GenerationDocumentFingerprint) {
			(*docs)[0].SHA256 = strings.Repeat("b", 64)
		},
		"source filename": func(_ *config.RAGIngestionPolicyData, _ *GenerationContract, docs *[]GenerationDocumentFingerprint) {
			(*docs)[0].FileName = "doc.txt"
		},
		"source media type": func(_ *config.RAGIngestionPolicyData, _ *GenerationContract, docs *[]GenerationDocumentFingerprint) {
			(*docs)[0].MediaType = "text/plain"
		},
		"source size": func(_ *config.RAGIngestionPolicyData, _ *GenerationContract, docs *[]GenerationDocumentFingerprint) {
			(*docs)[0].SizeBytes++
		},
		"parse mode": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.ParseMode = config.ParseModeAuto
		},
		"selected parser": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.ParserEngine = "markitdown"
		},
		"vision prompt": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.DocumentAI.VisionPromptVersion = "vp2"
		},
		"enrichment prompt": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.DocumentAI.EnrichmentPromptVersion = "ep2"
		},
		"chunk size": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.ChunkSize++
		},
		"chunk overlap": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.ChunkOverlap++
		},
		"embedding contract": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.Embedding.ContractFingerprint = "contract-2"
		},
		"embedding model": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.Embedding.Model = "embed-2"
		},
		"embedding dims": func(p *config.RAGIngestionPolicyData, _ *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			p.Embedding.Dims++
		},
		"parser protocol": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.ParserProtocolVersion = "protocol-v2"
		},
		"parser engine": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.ParserEngineVersion = "engine-v2"
		},
		"tokenizer": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.TokenizerVersion = "token-v2"
		},
		"splitter": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.SplitterVersion = "split-v2"
		},
		"artifact schema": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.ArtifactSchemaVersion++
		},
		"vector schema": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.VectorSchemaVersion = "vector-v2"
		},
		"index format version": func(_ *config.RAGIngestionPolicyData, c *GenerationContract, _ *[]GenerationDocumentFingerprint) {
			c.IndexFormatVersion++
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidatePolicy, candidateContract := policy, contract
			candidateDocuments := append([]GenerationDocumentFingerprint(nil), documents...)
			mutate(&candidatePolicy, &candidateContract, &candidateDocuments)
			fingerprint, err := GenerationFingerprint("version", "corpus", candidateDocuments, candidatePolicy, candidateContract)
			if err != nil {
				t.Fatal(err)
			}
			if fingerprint == baseline {
				t.Fatal("critical generation contract change reused fingerprint")
			}
		})
	}
}

func TestCorpusArtifactFingerprintIgnoresChunkAndEmbeddingOnlyChanges(t *testing.T) {
	document := GenerationDocumentFingerprint{ID: "doc", FileName: "doc.md", MediaType: "text/markdown", SHA256: strings.Repeat("a", 64), SizeBytes: 10}
	policy := config.RAGIngestionPolicyData{Version: 1, ChunkSize: 512, ChunkOverlap: 64, ParseMode: config.ParseModeStandard,
		ParserEngine: "anydoc",
		Embedding:    config.RAGPolicyEmbeddingData{ContractFingerprint: "one", Model: "embed-one", Dims: 1024}}
	contract := GenerationContract{ParserProtocolVersion: "protocol-v1", ParserEngineVersion: "engine-v1", TokenizerVersion: "token-v1", SplitterVersion: "split-v1", ArtifactSchemaVersion: 1, VectorSchemaVersion: "vector-v1", IndexFormatVersion: 1}
	first, err := CorpusArtifactFingerprint(document, policy, contract)
	if err != nil {
		t.Fatal(err)
	}
	policy.ChunkSize, policy.ChunkOverlap = 768, 96
	policy.Embedding = config.RAGPolicyEmbeddingData{ContractFingerprint: "two", Model: "embed-two", Dims: 2048}
	second, err := CorpusArtifactFingerprint(document, policy, contract)
	if err != nil || second != first {
		t.Fatalf("chunk/vector-only changes invalidated parse artifact: %q/%q err=%v", first, second, err)
	}
	contract.ParserEngineVersion = "engine-v2"
	third, err := CorpusArtifactFingerprint(document, policy, contract)
	if err != nil || third == first {
		t.Fatalf("parser change reused parse artifact: %q/%q err=%v", first, third, err)
	}
	contract.ParserEngineVersion = "engine-v1"
	policy.ParserEngine = "markitdown"
	fourth, err := CorpusArtifactFingerprint(document, policy, contract)
	if err != nil || fourth == first {
		t.Fatalf("selected parser reused parse artifact: %q/%q err=%v", first, fourth, err)
	}
}
