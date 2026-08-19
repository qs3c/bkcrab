package rag

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
)

type countingEvaluationParser struct {
	inner      parse.Parser
	calls      atomic.Int32
	lastEngine atomic.Value
}

func (p *countingEvaluationParser) Parse(ctx context.Context, source document.Source, options parse.ParseOptions) (*document.ParsedDocument, error) {
	p.calls.Add(1)
	p.lastEngine.Store(source.ParserEngine)
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
	var stageEvents []telemetry.Event
	service := New(Deps{Vector: vectorStore, Objects: objectStore, Parser: parser, Cfg: config.RAGCfg{}, Telemetry: telemetry.RecorderFunc(func(_ context.Context, event telemetry.Event) {
		if event.Name == telemetry.EventEvalStage {
			stageEvents = append(stageEvents, event)
		}
	})})
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
		ParserEngine: "anydoc",
		Embedding:    config.RAGPolicyEmbeddingData{ContractFingerprint: "contract", Model: "embed", Dims: 3}}
	firstTarget, err := NewEvaluationPipelineTarget("admin", "run-one", "version-one", "reg_one")
	if err != nil {
		t.Fatal(err)
	}
	var progressMu sync.Mutex
	var generationProgress []EvaluationGenerationProgress
	request := EvaluationPipelineRequest{Target: firstTarget, Ingestion: policy, Contract: contract,
		Embedding: config.RAGEmbeddingCfg{Endpoint: embeddingServer.URL, Model: "embed", Dims: 3},
		Documents: []EvaluationPipelineDocument{{ID: "doc", FileName: "one.md", MediaType: "text/markdown", ObjectKey: sourceKey, SHA256: sha, SizeBytes: int64(len(sourceText))}},
		Progress: func(update EvaluationGenerationProgress) error {
			progressMu.Lock()
			defer progressMu.Unlock()
			generationProgress = append(generationProgress, update)
			return nil
		}}
	first, err := service.BuildEvaluationGeneration(context.Background(), request)
	if err != nil || first.DocumentCount != 1 || first.ChunkCount < 1 || vectorStore.Count(firstTarget.CollectionKey) != int(first.ChunkCount) {
		t.Fatalf("first build=%+v vectors=%d err=%v", first, vectorStore.Count(firstTarget.CollectionKey), err)
	}
	if parser.lastEngine.Load() != "anydoc" {
		t.Fatalf("evaluation parser engine=%v, want anydoc", parser.lastEngine.Load())
	}
	progressMu.Lock()
	progressSnapshot := append([]EvaluationGenerationProgress(nil), generationProgress...)
	progressMu.Unlock()
	if len(progressSnapshot) < 3 || progressSnapshot[len(progressSnapshot)-1].Stage != "finalizing_generation" ||
		progressSnapshot[len(progressSnapshot)-1].DocumentsCompleted != 1 || progressSnapshot[len(progressSnapshot)-1].ChunksCompleted < 1 {
		t.Fatalf("generation progress=%+v", generationProgress)
	}
	secondTarget, _ := NewEvaluationPipelineTarget("admin", "run-two", "version-one", "reg_two")
	request.Target = secondTarget
	request.Ingestion.ChunkSize = 256
	second, err := service.BuildEvaluationGeneration(context.Background(), request)
	if err != nil || second.DocumentCount != 1 || parser.calls.Load() != 1 || !vectorStore.HasCollection(secondTarget.CollectionKey) {
		t.Fatalf("artifact reuse build=%+v parserCalls=%d err=%v", second, parser.calls.Load(), err)
	}
	thirdTarget, _ := NewEvaluationPipelineTarget("admin", "run-three", "version-one", "reg_three")
	request.Target = thirdTarget
	request.Ingestion.ParserEngine = "markitdown"
	if _, err := service.BuildEvaluationGeneration(context.Background(), request); err != nil || parser.calls.Load() != 2 || parser.lastEngine.Load() != "markitdown" {
		t.Fatalf("parser selection did not isolate artifact: calls=%d engine=%v err=%v", parser.calls.Load(), parser.lastEngine.Load(), err)
	}
	textTarget, _ := NewEvaluationPipelineTarget("admin", "run-text", "version-one", "reg_text")
	request.Target = textTarget
	request.BypassParser = true
	request.Contract.ParserProtocolVersion = "canonical-text-v1"
	request.Contract.ParserEngineVersion = "canonical-text-v1"
	if _, err := service.BuildEvaluationGeneration(context.Background(), request); err != nil || parser.calls.Load() != 2 || !vectorStore.HasCollection(textTarget.CollectionKey) {
		t.Fatalf("canonical text track called parser: calls=%d err=%v", parser.calls.Load(), err)
	}
	if err := service.DropEvaluationGeneration(context.Background(), firstTarget); err != nil {
		t.Fatal(err)
	}
	if vectorStore.HasCollection(firstTarget.CollectionKey) || !vectorStore.HasCollection(secondTarget.CollectionKey) {
		t.Fatal("evaluation generation drop crossed collection namespace")
	}
	seen := map[string]bool{}
	for _, event := range stageEvents {
		seen[event.Fields.Operation] = true
		if event.Fields.RunID == "" || event.Fields.Outcome != "ok" {
			t.Fatalf("unbounded/failed stage telemetry: %+v", event)
		}
	}
	for _, operation := range []string{"eval_parser", "eval_text_normalize", "eval_embedding", "eval_milvus"} {
		if !seen[operation] {
			t.Fatalf("missing %s telemetry: %+v", operation, stageEvents)
		}
	}
}
