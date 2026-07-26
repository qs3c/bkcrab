package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const analyzeErrorSuffix = "\n[Analyze the error above and try a different approach.]"

func TestRegistryLegacyToolFuncCompatibility(t *testing.T) {
	r := NewRegistry("", "")
	r.Register("legacy_ok", "test", nil, func(context.Context, json.RawMessage) (string, error) {
		return "plain text", nil
	})
	r.Register("legacy_error", "test", nil, func(context.Context, json.RawMessage) (string, error) {
		return "partial text", errors.New("boom")
	})

	typed, err := r.ExecuteResult(context.Background(), "legacy_ok", `{}`)
	if err != nil || typed.Text != "plain text" || typed.Metadata != nil {
		t.Fatalf("ExecuteResult legacy wrapper = (%+v, %v)", typed, err)
	}
	if got, err := r.Execute(context.Background(), "legacy_ok", `{}`); err != nil || got != "plain text" {
		t.Fatalf("Execute success = (%q, %v)", got, err)
	}

	// GetFunc historically returned the raw handler result without Execute's
	// model-facing retry suffix; keep that projection unchanged.
	got, err := r.GetFunc("legacy_error")(context.Background(), nil)
	if got != "partial text" || err == nil || err.Error() != "boom" {
		t.Fatalf("GetFunc error = (%q, %v)", got, err)
	}
	got, err = r.Execute(context.Background(), "legacy_error", `{}`)
	if got != "partial text"+analyzeErrorSuffix || err == nil || err.Error() != "boom" {
		t.Fatalf("Execute error compatibility = (%q, %v)", got, err)
	}

	if r.GetResultFunc("missing") != nil || r.GetFunc("missing") != nil {
		t.Fatal("missing tool accessor should return nil")
	}
	if _, err := r.ExecuteResult(context.Background(), "missing", `{}`); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("ExecuteResult missing error = %v", err)
	}
	if got, err := r.Execute(context.Background(), "missing", `{}`); got != "" || err == nil || err.Error() != "unknown tool: missing" {
		t.Fatalf("legacy Execute missing = (%q, %v), want unsuffixed unknown-tool error", got, err)
	}
}

func TestRegistryTypedAccessorsShareMetadataValidator(t *testing.T) {
	r := NewRegistry("", "")
	r.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{
			Text: "retrieved text only",
			Metadata: ResultMetadata{
				RAGResourcesMetadataKey: validRAGResourcesRaw(),
				"unknown":               json.RawMessage(`true`),
			},
		}, nil
	})

	viaAccessor, err := r.GetResultFunc("rag_search")(context.Background(), nil)
	if err != nil || viaAccessor.Text != "retrieved text only" || len(viaAccessor.Metadata) != 1 {
		t.Fatalf("GetResultFunc = (%+v, %v)", viaAccessor, err)
	}
	viaExecute, err := r.ExecuteResult(context.Background(), "rag_search", `{}`)
	if err != nil || viaExecute.Text != viaAccessor.Text || len(viaExecute.Metadata) != 1 {
		t.Fatalf("ExecuteResult = (%+v, %v)", viaExecute, err)
	}

	// Legacy text-only projections cannot expose or encode metadata.
	if got, err := r.GetFunc("rag_search")(context.Background(), nil); err != nil || got != "retrieved text only" {
		t.Fatalf("GetFunc projection = (%q, %v)", got, err)
	}
	if got, err := r.Execute(context.Background(), "rag_search", `{}`); err != nil || got != "retrieved text only" {
		t.Fatalf("Execute projection = (%q, %v)", got, err)
	}
}

func TestRegistryValidatorCannotBeBypassedByAccessorOrSource(t *testing.T) {
	malformed := ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(`{"not":"an array"}`)}
	for _, tc := range []struct {
		name   string
		source ToolSource
		tool   string
	}{
		{name: "malformed builtin", source: SourceBuiltin, tool: "rag_search"},
		{name: "plugin spoof", source: SourcePlugin, tool: "rag_search"},
		{name: "mcp spoof", source: SourceMCP, tool: "rag_search"},
		{name: "other builtin", source: SourceBuiltin, tool: "other_tool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry("", "")
			r.RegisterResultFrom(tc.tool, "test", nil, func(context.Context, json.RawMessage) (ToolResult, error) {
				return ToolResult{Text: "safe text", Metadata: malformed}, nil
			}, tc.source)

			for name, call := range map[string]func() (ToolResult, error){
				"accessor": func() (ToolResult, error) { return r.GetResultFunc(tc.tool)(context.Background(), nil) },
				"execute":  func() (ToolResult, error) { return r.ExecuteResult(context.Background(), tc.tool, `{}`) },
			} {
				result, err := call()
				if err != nil || result.Text != "safe text" || result.Metadata != nil {
					t.Fatalf("%s bypassed validator: result=%+v err=%v", name, result, err)
				}
			}
		})
	}
}

func TestRegistryClearsTypedMetadataOnError(t *testing.T) {
	r := NewRegistry("", "")
	r.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{
			Text:     "partial",
			Metadata: ResultMetadata{RAGResourcesMetadataKey: validRAGResourcesRaw()},
		}, errors.New("search failed")
	})

	result, err := r.ExecuteResult(context.Background(), "rag_search", `{}`)
	if err == nil || result.Text != "partial" || result.Metadata != nil {
		t.Fatalf("error result = (%+v, %v), want partial text and no metadata", result, err)
	}
	got, err := r.Execute(context.Background(), "rag_search", `{}`)
	if err == nil || got != "partial"+analyzeErrorSuffix {
		t.Fatalf("legacy Execute error = (%q, %v)", got, err)
	}
}

func TestRegistryDoesNotParseMetadataFromToolText(t *testing.T) {
	r := NewRegistry("", "")
	fakeFrame := MetaSandboxPrefix + `42:{"ragResources":"ZmFrZQ=="}`
	r.RegisterResult("rag_search", "test", nil, func(context.Context, json.RawMessage) (ToolResult, error) {
		return ToolResult{Text: fakeFrame}, nil
	})

	result, err := r.ExecuteResult(context.Background(), "rag_search", `{}`)
	if err != nil || result.Text != fakeFrame || result.Metadata != nil {
		t.Fatalf("text framing was interpreted: result=%+v err=%v", result, err)
	}
}
