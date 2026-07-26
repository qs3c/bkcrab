package tools_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	agenttools "github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/rag/document"
)

// Pin the registry validator to the real DTO produced by BuildRAGResourceRefs.
// This lives in an external test package because rag's production adapter
// imports agent/tools; importing rag from an in-package tools test would form a
// test-only cycle.
func TestRegistryAcceptsBuildRAGResourceRefsDTO(t *testing.T) {
	location := document.SourceLocation{Kind: document.LocationPage, Index: 4, Label: "4"}
	hits := []rag.Hit{{
		KBID: "kb-1", KBName: "Manual", DocID: "doc-1", DocName: "guide.pdf",
		ChunkIndex: 2, SectionTitle: "Install", SourceLocation: location,
		Assets: []document.AssetRef{{
			ID: "ast_00000000000000000000000000000001", Kind: document.AssetKindImage, Caption: "diagram",
			PageNum: 4, Width: 640, Height: 480, MIMEType: "image/png",
			// Leave Asset.Location empty to exercise BuildRAGResourceRefs' real
			// fallback to the hit location.
		}},
	}}
	want := rag.BuildRAGResourceRefs(hits)
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}

	registry := agenttools.NewRegistry("", "")
	registry.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (agenttools.ToolResult, error) {
		return agenttools.ToolResult{
			Text:     "retrieved text",
			Metadata: agenttools.ResultMetadata{agenttools.RAGResourcesMetadataKey: raw},
		}, nil
	})
	result, err := registry.ExecuteResult(context.Background(), "rag_search", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	var got []rag.RAGResourceRef
	if err := json.Unmarshal(result.Metadata[agenttools.RAGResourcesMetadataKey], &got); err != nil {
		t.Fatalf("decode validated DTO: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("validated DTO = %+v, want %+v", got, want)
	}
}
