package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	agenttools "github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/provider"
)

func sdkRAGMetadata(assetID string) agenttools.ResultMetadata {
	raw := fmt.Sprintf(`[{"asset":{"id":%q,"kind":"image","location":{"kind":"page","index":1,"label":"1"},"mimeType":"image/png"},"kbId":"kb-1","kbName":"Manual","docId":"doc-1","docName":"guide.pdf","chunkIndex":0,"sourceLocation":{"kind":"page","index":1,"label":"1"}}]`, assetID)
	return agenttools.ResultMetadata{agenttools.RAGResourcesMetadataKey: json.RawMessage(raw)}
}

func sdkAssetID(n int) string {
	return fmt.Sprintf("ast_%032x", n)
}

func metadataAssetID(t *testing.T, metadata agenttools.ResultMetadata) string {
	t.Helper()
	var refs []struct {
		Asset struct {
			ID string `json:"id"`
		} `json:"asset"`
	}
	if err := json.Unmarshal(metadata[agenttools.RAGResourcesMetadataKey], &refs); err != nil {
		t.Fatalf("decode bridge metadata: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("bridge metadata refs = %d, want 1", len(refs))
	}
	return refs[0].Asset.ID
}

func TestToolAdapterCarriesValidatedMetadataOnlyInPrivateData(t *testing.T) {
	assetID := sdkAssetID(1)
	registry := agenttools.NewRegistry("", "")
	registry.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (agenttools.ToolResult, error) {
		return agenttools.ToolResult{Text: "retrieved text", Metadata: sdkRAGMetadata(assetID)}, nil
	})
	adapter := &toolAdapter{name: "rag_search", registry: registry}

	result, err := adapter.Call(context.Background(), map[string]interface{}{"query": "x"}, nil)
	if err != nil || result.IsError {
		t.Fatalf("adapter result = (%+v, %v)", result, err)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "retrieved text" {
		t.Fatalf("model content = %+v", result.Content)
	}
	data, ok := result.Data.(sdkBridgeData)
	if !ok || metadataAssetID(t, data.metadata) != assetID {
		t.Fatalf("private bridge data = %#v", result.Data)
	}

	// sdkBridgeData has no exported fields: an accidental SDK serialization
	// cannot turn the in-memory side channel into model-visible JSON.
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), assetID) || strings.Contains(string(encoded), agenttools.RAGResourcesMetadataKey) {
		t.Fatalf("private metadata serialized through SDK result: %s", encoded)
	}
}

func TestToolAdapterUsesCurrentValidatorWrappedRegistryPath(t *testing.T) {
	registry := agenttools.NewRegistry("", "")
	registry.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (agenttools.ToolResult, error) {
		return agenttools.ToolResult{Text: "old builtin", Metadata: sdkRAGMetadata(sdkAssetID(2))}, nil
	})
	sdkRegistry := buildSDKRegistry(registry)

	// Override after the SDK adapter has been built. A cached raw handler would
	// still execute the old trusted producer; the name-based ExecuteResult path
	// executes this plugin and strips its attempted metadata.
	registry.RegisterResultFrom("rag_search", "plugin", nil, func(context.Context, json.RawMessage) (agenttools.ToolResult, error) {
		return agenttools.ToolResult{Text: "plugin result", Metadata: sdkRAGMetadata(sdkAssetID(3))}, nil
	}, agenttools.SourcePlugin)

	result, err := sdkRegistry.Get("rag_search").Call(context.Background(), nil, nil)
	if err != nil || result.IsError || len(result.Content) != 1 || result.Content[0].Text != "plugin result" {
		t.Fatalf("adapter did not execute current registry entry: result=%+v err=%v", result, err)
	}
	if result.Data != nil {
		t.Fatalf("plugin metadata crossed bridge: %#v", result.Data)
	}
}

func TestToolAdapterDropsMalformedAndUnknownToolMetadata(t *testing.T) {
	t.Run("malformed builtin metadata", func(t *testing.T) {
		registry := agenttools.NewRegistry("", "")
		registry.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (agenttools.ToolResult, error) {
			return agenttools.ToolResult{
				Text:     "safe text",
				Metadata: agenttools.ResultMetadata{agenttools.RAGResourcesMetadataKey: json.RawMessage(`{"not":"an array"}`)},
			}, nil
		})
		result, err := (&toolAdapter{name: "rag_search", registry: registry}).Call(context.Background(), nil, nil)
		if err != nil || result.IsError || result.Data != nil || result.Content[0].Text != "safe text" {
			t.Fatalf("malformed metadata crossed adapter: result=%+v err=%v", result, err)
		}
	})

	t.Run("unknown tool", func(t *testing.T) {
		registry := agenttools.NewRegistry("", "")
		result, err := (&toolAdapter{name: "missing", registry: registry}).Call(context.Background(), nil, nil)
		if err != nil || !result.IsError || result.Data != nil || !strings.Contains(result.Error, "unknown tool: missing") {
			t.Fatalf("unknown-tool adapter result=%+v err=%v", result, err)
		}
	})
}

func TestExecuteToolsConcurrentlyKeepsSameNameMetadataByToolCallID(t *testing.T) {
	registry := agenttools.NewRegistry("", "")
	var entered atomic.Int32
	release := make(chan struct{})
	registry.RegisterResult("rag_search", "test", nil, func(_ context.Context, raw json.RawMessage) (agenttools.ToolResult, error) {
		var args struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return agenttools.ToolResult{}, err
		}
		if entered.Add(1) == 2 {
			close(release)
		}
		select {
		case <-release:
		case <-time.After(2 * time.Second):
			return agenttools.ToolResult{}, errors.New("concurrent rag_search calls did not overlap")
		}
		assetNumber := 10
		if args.ID == "b" {
			assetNumber = 11
		}
		return agenttools.ToolResult{Text: "text-" + args.ID, Metadata: sdkRAGMetadata(sdkAssetID(assetNumber))}, nil
	})

	calls := []provider.ToolCall{
		{ID: "call-a", Type: "function", Function: provider.FunctionCall{Name: "rag_search", Arguments: `{"id":"a"}`}},
		{ID: "call-b", Type: "function", Function: provider.FunctionCall{Name: "rag_search", Arguments: `{"id":"b"}`}},
	}
	results := newSDKEngine("session").executeToolsConcurrently(context.Background(), registry, calls, "")
	if len(results) != 2 {
		t.Fatalf("results = %d, want 2", len(results))
	}
	for i, want := range []string{"a", "b"} {
		if results[i].toolCallID != "call-"+want || results[i].result != "text-"+want {
			t.Fatalf("result %d identity/text = %+v", i, results[i])
		}
		wantAssetID := sdkAssetID(10 + i)
		if got := metadataAssetID(t, results[i].metadata); got != wantAssetID {
			t.Fatalf("result %d metadata asset = %q, want %q", i, got, wantAssetID)
		}
	}
}

func TestExecuteToolsConcurrentlyDropsMetadataOnError(t *testing.T) {
	registry := agenttools.NewRegistry("", "")
	registry.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (agenttools.ToolResult, error) {
		return agenttools.ToolResult{Text: "partial", Metadata: sdkRAGMetadata(sdkAssetID(99))}, errors.New("boom")
	})
	calls := []provider.ToolCall{{
		ID: "call-error", Type: "function",
		Function: provider.FunctionCall{Name: "rag_search", Arguments: `{}`},
	}}

	results := newSDKEngine("session").executeToolsConcurrently(context.Background(), registry, calls, "")
	if len(results) != 1 || results[0].err == nil {
		t.Fatalf("error results = %+v", results)
	}
	if results[0].metadata != nil {
		t.Fatalf("error response retained metadata: %v", results[0].metadata)
	}
	wantText := "partial\nboom\n[Analyze the error above and try a different approach.]"
	if results[0].result != wantText {
		t.Fatalf("error result text = %q, want %q", results[0].result, wantText)
	}
}
