package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func validRAGResourcesRaw() json.RawMessage {
	return json.RawMessage(`[{"asset":{"id":"ast_00000000000000000000000000000001","kind":"image","caption":"chart","pageNum":2,"location":{"kind":"page","index":2,"label":"2"},"width":640,"height":480,"mimeType":"image/png"},"kbId":"kb-1","kbName":"Manual","docId":"doc-1","docName":"guide.pdf","chunkIndex":3,"sectionTitle":"Install","sourceLocation":{"kind":"page","index":2,"label":"2"}}]`)
}

func validRAGResourceWire(id string) ragResourceRefWire {
	location := document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "1"}
	return ragResourceRefWire{
		Asset: document.AssetRef{
			ID: id, Kind: document.AssetKindImage, Location: location,
			Width: 10, Height: 10, MIMEType: "image/png",
		},
		KBID: "kb-1", KBName: "Manual", DocID: "doc-1", DocName: "guide.pdf",
		ChunkIndex: 0, SourceLocation: location,
	}
}

func TestResultMetadataCloneDetachesRawMessages(t *testing.T) {
	original := ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(`[1]`)}
	cloned := original.Clone()
	original[RAGResourcesMetadataKey][1] = '9'
	original["other"] = json.RawMessage(`true`)

	if got := string(cloned[RAGResourcesMetadataKey]); got != `[1]` {
		t.Fatalf("cloned raw message = %q, want [1]", got)
	}
	if _, exists := cloned["other"]; exists {
		t.Fatal("clone changed when the source map was mutated")
	}
	if ResultMetadata(nil).Clone() != nil {
		t.Fatal("nil metadata clone should stay nil")
	}
}

func TestValidateResultMetadataAcceptsTrustedRAGResources(t *testing.T) {
	raw := validRAGResourcesRaw()
	in := ResultMetadata{
		RAGResourcesMetadataKey: raw,
		"unknown":               json.RawMessage(`{"html":"<script>"}`),
	}
	out := validateResultMetadata("rag_search", SourceBuiltin, in)
	if len(out) != 1 {
		t.Fatalf("validated metadata keys = %v, want only %q", out, RAGResourcesMetadataKey)
	}
	if got := string(out[RAGResourcesMetadataKey]); got != string(raw) {
		t.Fatalf("validated payload changed: %q", got)
	}

	// The validator owns the returned bytes.
	in[RAGResourcesMetadataKey][0] = '{'
	if got := out[RAGResourcesMetadataKey][0]; got != '[' {
		t.Fatalf("validated payload aliases producer memory: first byte = %q", got)
	}
}

func TestValidateResultMetadataRejectsUntrustedProducerAndShape(t *testing.T) {
	valid := validRAGResourcesRaw()
	cases := []struct {
		name     string
		toolName string
		source   ToolSource
		metadata ResultMetadata
	}{
		{name: "plugin producer", toolName: "rag_search", source: SourcePlugin, metadata: ResultMetadata{RAGResourcesMetadataKey: valid}},
		{name: "mcp producer", toolName: "rag_search", source: SourceMCP, metadata: ResultMetadata{RAGResourcesMetadataKey: valid}},
		{name: "other builtin", toolName: "web_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: valid}},
		{name: "unknown key", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{"resources": valid}},
		{name: "malformed JSON", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(`[{`)}},
		{name: "wrong top-level shape", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(`{"asset":{}}`)}},
		{name: "null is not an array", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(`null`)}},
		{name: "unknown nested field", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(strings.Replace(string(valid), `"mimeType":"image/png"`, `"mimeType":"image/png","url":"https://attacker.invalid/x"`, 1))}},
		{name: "noncanonical asset id", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: json.RawMessage(strings.Replace(string(valid), `"ast_00000000000000000000000000000001"`, `"asset-1"`, 1))}},
		{name: "trailing JSON", toolName: "rag_search", source: SourceBuiltin, metadata: ResultMetadata{RAGResourcesMetadataKey: append(append(json.RawMessage(nil), valid...), []byte(` true`)...)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateResultMetadata(tc.toolName, tc.source, tc.metadata); got != nil {
				t.Fatalf("metadata passed validation: %v", got)
			}
		})
	}
}

func TestValidateResultMetadataEnforcesCountAndByteLimits(t *testing.T) {
	refs := make([]ragResourceRefWire, maxRAGResourcesPerToolResult+1)
	for i := range refs {
		refs[i] = validRAGResourceWire(fmt.Sprintf("ast_%032x", i+1))
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		t.Fatal(err)
	}
	if got := validateResultMetadata("rag_search", SourceBuiltin, ResultMetadata{RAGResourcesMetadataKey: raw}); got != nil {
		t.Fatalf("over-count metadata passed validation: %v", got)
	}

	oversized := json.RawMessage(`[]` + strings.Repeat(" ", maxResultMetadataValueBytes))
	if got := validateResultMetadata("rag_search", SourceBuiltin, ResultMetadata{RAGResourcesMetadataKey: oversized}); got != nil {
		t.Fatalf("oversized metadata passed validation: %v", got)
	}
}

type lockedLogBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestResultMetadataWarningIsBoundedAndDoesNotEchoPayload(t *testing.T) {
	var logs lockedLogBuffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	secret := "PAYLOAD_SHOULD_NOT_APPEAR"
	longKey := strings.Repeat("attacker-key-", 100)
	validateResultMetadata("rag_search", SourceBuiltin, ResultMetadata{
		longKey: json.RawMessage(`{"caption":"` + secret + `"}`),
	})
	longToolName := strings.Repeat("evil-tool\n", 100)
	validateResultMetadata(longToolName, SourcePlugin, ResultMetadata{
		RAGResourcesMetadataKey: validRAGResourcesRaw(),
	})

	got := logs.String()
	if strings.Contains(got, secret) || strings.Contains(got, longKey) || strings.Contains(got, longToolName) {
		t.Fatalf("warning echoed attacker-controlled payload: %q", got)
	}
	if !strings.Contains(got, "<untrusted>") || strings.Contains(got, `\n`) {
		t.Fatalf("warning was not safely summarized: %q", got)
	}
	if value := boundedResultMetadataLogValue(strings.Repeat("界", 100)); len(value) > maxResultMetadataLogBytes || !utf8.ValidString(value) {
		t.Fatalf("bounded log value is invalid or oversized: %q (%d bytes)", value, len(value))
	}
}
