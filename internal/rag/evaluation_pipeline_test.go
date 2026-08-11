package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/vector"
)

type countingEvaluationParser struct {
	inner parse.Parser
	calls atomic.Int32
}

func (p *countingEvaluationParser) Parse(ctx context.Context, source document.Source, options parse.ParseOptions) (*document.ParsedDocument, error) {
	p.calls.Add(1)
	return p.inner.Parse(ctx, source, options)
}

func TestRealEvaluationPipelineUsesIsolatedTargetAndParseArtifactReuse(t *testing.T) {
	embeddingServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response := struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}{Data: make([]struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		}, len(body.Input))}
		for index := range response.Data {
			response.Data[index].Embedding = []float32{1, 0, float32(index)}
			response.Data[index].Index = index
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer embeddingServer.Close()

	objectStore := objects.NewLocalFS(t.TempDir())
	vectorStore := vector.NewFake()
	parser := &countingEvaluationParser{inner: parse.NewLocalParser(nil, 100, 10<<20)}
	service := New(Deps{Vector: vectorStore, Objects: objectStore, Parser: parser, Cfg: config.RAGCfg{}})
	const sourceText = "# Evaluation\n\nThe isolated pipeline uses the real parser, splitter and vector store."
	digest := sha256.Sum256([]byte(sourceText))
	sha := hex.EncodeToString(digest[:])
	sourceKey := "rag-eval/datasets/d/versions/1/corpus/doc/one.md"
	if err := objectStore.Put(context.Background(), sourceKey, strings.NewReader(sourceText), int64(len(sourceText)), "text/markdown"); err != nil {
		t.Fatal(err)
	}
	contract := rageval.GenerationContract{ParserProtocolVersion: "local-v1", ParserEngineVersion: parse.LocalParserVersion,
		TokenizerVersion: "estimate-v1", SplitterVersion: "split-v1", ArtifactSchemaVersion: document.ParsedArtifactSchemaVersion,
		VectorSchemaVersion: "vector-v1", IndexFormatVersion: 1}
	policy := config.RAGIngestionPolicyData{Version: 1, ChunkSize: 128, ChunkOverlap: 16, ParseMode: config.ParseModeStandard,
		Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "contract", Model: "embed", Dims: 3}}
	firstTarget, err := NewEvaluationPipelineTarget("admin", "run-one", "version-one", "reg_one")
	if err != nil {
		t.Fatal(err)
	}
	request := EvaluationPipelineRequest{Target: firstTarget, Ingestion: policy, Contract: contract,
		Embedding: config.RAGEmbeddingCfg{Endpoint: embeddingServer.URL, Model: "embed", Dims: 3},
		Documents: []EvaluationPipelineDocument{{ID: "doc", FileName: "one.md", MediaType: "text/markdown", ObjectKey: sourceKey, SHA256: sha, SizeBytes: int64(len(sourceText))}}}
	first, err := service.BuildEvaluationGeneration(context.Background(), request)
	if err != nil || first.DocumentCount != 1 || first.ChunkCount < 1 || vectorStore.Count(firstTarget.CollectionKey) != int(first.ChunkCount) {
		t.Fatalf("first build=%+v vectors=%d err=%v", first, vectorStore.Count(firstTarget.CollectionKey), err)
	}
	secondTarget, _ := NewEvaluationPipelineTarget("admin", "run-two", "version-one", "reg_two")
	request.Target = secondTarget
	request.Ingestion.ChunkSize = 256
	second, err := service.BuildEvaluationGeneration(context.Background(), request)
	if err != nil || second.DocumentCount != 1 || parser.calls.Load() != 1 || !vectorStore.HasCollection(secondTarget.CollectionKey) {
		t.Fatalf("artifact reuse build=%+v parserCalls=%d err=%v", second, parser.calls.Load(), err)
	}
	if err := service.DropEvaluationGeneration(context.Background(), firstTarget); err != nil {
		t.Fatal(err)
	}
	if vectorStore.HasCollection(firstTarget.CollectionKey) || !vectorStore.HasCollection(secondTarget.CollectionKey) {
		t.Fatal("evaluation generation drop crossed collection namespace")
	}
}
