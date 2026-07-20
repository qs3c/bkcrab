package enrich

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/chunktext"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/split"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

type recordingEnricher struct {
	mu      sync.Mutex
	blocks  []EnrichableBlock
	budgets []*vision.TaskDocumentAIBudget
	err     error
}

func (e *recordingEnricher) Enrich(_ context.Context, block EnrichableBlock, budget *vision.TaskDocumentAIBudget) (Enhancement, error) {
	e.mu.Lock()
	e.blocks = append(e.blocks, block)
	e.budgets = append(e.budgets, budget)
	e.mu.Unlock()
	if e.err != nil {
		return Enhancement{}, e.err
	}
	if block.Kind == BlockTable {
		return Enhancement{Kind: BlockTable, Table: &TableEnhancement{
			Topic: "容量", Columns: []ColumnMeaning{{Name: "name", Meaning: "名称"}}, Summary: "容量表。",
		}}, nil
	}
	return Enhancement{Kind: BlockCode, Code: &CodeEnhancement{
		Language: "Go", Responsibility: "返回状态。", Symbols: []string{"Status"}, Description: "状态查询代码。",
	}}, nil
}

func (e *recordingEnricher) snapshot() ([]EnrichableBlock, []*vision.TaskDocumentAIBudget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]EnrichableBlock(nil), e.blocks...), append([]*vision.TaskDocumentAIBudget(nil), e.budgets...)
}

func processChunks() []split.Chunk {
	return []split.Chunk{
		{Index: 0, Kind: split.BlockText, RawContent: "ordinary", SearchContent: "ordinary"},
		{Index: 1, Kind: split.BlockImage, RawContent: "图片说明：diagram", SearchContent: "图片说明：diagram"},
		{Index: 2, Kind: split.BlockTable, RawContent: "| a |\n|---|\n| b |", SearchContent: "| a |\n|---|\n| b |", ReservedTokens: 20},
		{Index: 3, Kind: split.BlockCode, RawContent: "```go\nfunc Status() {}\n```", SearchContent: "```go\nfunc Status() {}\n```", ReservedTokens: 20},
	}
}

func TestProcessorStrictGatesAndSemanticKinds(t *testing.T) {
	t.Parallel()
	finalize := FinalizeConfig{ChunkSize: 256, MaxSearchContentBytes: 4096, CollectionMaxLength: 4096}
	for _, test := range []struct {
		name string
		cfg  ProcessConfig
	}{
		{"system gate", ProcessConfig{SystemEnabled: false, TextModel: "text", KBEnabled: true, Finalize: finalize}},
		{"text model", ProcessConfig{SystemEnabled: true, TextModel: "", KBEnabled: true, Finalize: finalize}},
		// This is the standard-mode case: parse mode is deliberately not a
		// bypass input; explicit KB consent remains mandatory.
		{"standard KB opt-in", ProcessConfig{SystemEnabled: true, TextModel: "text", KBEnabled: false, Finalize: finalize}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &recordingEnricher{}
			got, warnings := NewProcessor(fake).EnrichChunks(context.Background(), processChunks(), test.cfg, nil)
			if calls, _ := fake.snapshot(); len(calls) != 0 {
				t.Fatalf("disabled gate made %d calls", len(calls))
			}
			if len(warnings) != 0 || got[2].Enhancement != "" || got[2].RawContent != processChunks()[2].RawContent {
				t.Fatalf("disabled gate was not a strict no-op: got=%+v warnings=%+v", got, warnings)
			}
		})
	}

	fake := &recordingEnricher{}
	sharedBudget := &vision.TaskDocumentAIBudget{}
	got, warnings := NewProcessor(fake).EnrichChunks(context.Background(), processChunks(), ProcessConfig{
		SystemEnabled: true, TextModel: "text", KBEnabled: true, MaxBlocks: 10,
		Finalize: finalize, Scope: CacheScope{UserID: "u", KBID: "kb", DocID: "doc"},
	}, sharedBudget)
	blocks, budgets := fake.snapshot()
	if len(blocks) != 2 || len(warnings) != 0 {
		t.Fatalf("table/code calls=%d warnings=%+v", len(blocks), warnings)
	}
	for i, block := range blocks {
		if block.Kind != BlockTable && block.Kind != BlockCode {
			t.Fatalf("paragraph/image reached enricher: %+v", block)
		}
		if block.TokenBudget <= 0 || block.ByteBudget <= 0 {
			t.Fatalf("call %d did not receive actual remaining budgets: %+v", i, block)
		}
		if budgets[i] != sharedBudget {
			t.Fatal("processor did not pass the shared task DocumentAI budget pointer")
		}
	}
	if got[0].Enhancement != "" || got[1].Enhancement != "" || got[2].Enhancement == "" || got[3].Enhancement == "" {
		t.Fatalf("unexpected enhancement placement: %+v", got)
	}
	for i := range got {
		if got[i].RawContent != processChunks()[i].RawContent {
			t.Fatalf("model output replaced raw chunk %d", i)
		}
	}
}

func TestProcessorSoftFailureAndBlockLimit(t *testing.T) {
	t.Parallel()
	fake := &recordingEnricher{err: errors.New("provider timeout")}
	chunks := processChunks()
	got, warnings := NewProcessor(fake).EnrichChunks(context.Background(), chunks, ProcessConfig{
		SystemEnabled: true, TextModel: "text", KBEnabled: true, MaxBlocks: 1,
		Finalize: FinalizeConfig{ChunkSize: 256, MaxSearchContentBytes: 4096, CollectionMaxLength: 4096},
	}, &vision.TaskDocumentAIBudget{})
	calls, _ := fake.snapshot()
	if len(calls) != 1 || len(warnings) != 2 {
		t.Fatalf("calls=%d warnings=%+v", len(calls), warnings)
	}
	codes := map[string]bool{}
	for _, warning := range warnings {
		codes[warning.Code] = true
	}
	if !codes["enrichment_failed"] || !codes["enrichment_block_limit"] {
		t.Fatalf("missing soft failure warnings: %+v", warnings)
	}
	if got[2].RawContent != chunks[2].RawContent || got[3].RawContent != chunks[3].RawContent ||
		got[2].Enhancement != "" || got[3].Enhancement != "" {
		t.Fatalf("soft failure damaged source: %+v", got)
	}
}

func TestTypedEnhancementSchemasAreStrict(t *testing.T) {
	t.Parallel()
	tableJSON := []byte(`{"topic":"容量","columns":[{"name":"region","meaning":"区域"}],"keyEntities":["华东"],"units":["GiB"],"ranges":["1-9"],"summary":"容量范围。"}`)
	value, err := decodeEnhancement(tableJSON, BlockTable, DefaultSchemaLimits())
	if err != nil || value.Table == nil || !strings.Contains(value.Text(), "表格主题：容量") {
		t.Fatalf("decode table: value=%+v err=%v", value, err)
	}
	codeJSON := []byte(`{"language":"Go","responsibility":"校验","inputs":["value"],"outputs":["error"],"sideEffects":[],"symbols":["Validate"],"errorConditions":["empty"],"description":"输入校验。"}`)
	value, err = decodeEnhancement(codeJSON, BlockCode, DefaultSchemaLimits())
	if err != nil || value.Code == nil || !strings.Contains(value.Text(), "关键符号：Validate") {
		t.Fatalf("decode code: value=%+v err=%v", value, err)
	}
	for _, invalid := range [][]byte{
		[]byte(`{"topic":"x","columns":[],"keyEntities":[],"units":[],"ranges":[],"summary":"x","extra":true}`),
		[]byte(`{"language":"Go"}`),
		[]byte(`{"topic":"x","columns":[],"keyEntities":[],"units":[],"ranges":[],"summary":"x"} {}`),
	} {
		if _, err := decodeEnhancement(invalid, BlockTable, DefaultSchemaLimits()); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("invalid typed response accepted: %s err=%v", invalid, err)
		}
	}
}

func TestEnhancementTextUsesUTF8ByteAndTokenBudget(t *testing.T) {
	t.Parallel()
	value := Enhancement{Kind: BlockTable, Table: &TableEnhancement{
		Topic: "多字节主题", Summary: strings.Repeat("摘要。", 20),
	}}
	bounded := boundedEnhancement(value, EnrichableBlock{TokenBudget: 8, ByteBudget: 18})
	if bounded.Text() == "" || len([]byte(bounded.Text())) > 18 || split.EstimateTokens(bounded.Text()) > 8 {
		t.Fatalf("bounded enhancement=%q tokens=%d bytes=%d", bounded.Text(),
			split.EstimateTokens(bounded.Text()), len([]byte(bounded.Text())))
	}
	if value.Text() == bounded.Text() {
		t.Fatal("fixture did not exercise output cropping")
	}
}

type runeTokenizer struct{}

func (runeTokenizer) CountTokens(_ context.Context, value string) (int, error) {
	return len([]rune(value)), nil
}

type countingRuneTokenizer struct{ calls atomic.Int64 }

func (t *countingRuneTokenizer) CountTokens(_ context.Context, value string) (int, error) {
	t.calls.Add(1)
	return len([]rune(value)), nil
}

type nonAdditiveTokenizer struct{ calls atomic.Int64 }

func (t *nonAdditiveTokenizer) CountTokens(_ context.Context, value string) (int, error) {
	t.calls.Add(1)
	if value == "x" {
		return 100, nil
	}
	return 10, nil
}

func TestRemainingEnhancementBudgetDoesNotAssumeTokenizerAdditivity(t *testing.T) {
	t.Parallel()
	tokenizer := &nonAdditiveTokenizer{}
	tokens, bytes, err := RemainingEnhancementBudget(context.Background(), split.Chunk{
		RawContent: "raw", SearchContent: "raw",
	}, FinalizeConfig{
		ChunkSize: 20, MaxSearchContentBytes: 1024, CollectionMaxLength: 1024,
		ProviderTokenizer: tokenizer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tokens > 10 || bytes <= 0 {
		t.Fatalf("remaining budget tokens=%d bytes=%d, want conservative <=10", tokens, bytes)
	}
	if calls := tokenizer.calls.Load(); calls != 1 {
		t.Fatalf("provider tokenizer calls=%d, want one full-prefix measurement", calls)
	}
}

func TestFinalizeChunkPrioritizesRawAndEnforcesAllBoundaries(t *testing.T) {
	t.Parallel()
	raw := "| 名称 | 数值 |\n| --- | --- |\n| 示例 | 123 |"
	chunk := split.Chunk{Index: 7, Kind: split.BlockTable, RawContent: raw, Content: raw,
		SearchContent: raw, Enhancement: strings.Repeat("冗长摘要。", 100)}
	parts, err := FinalizeChunk(context.Background(), chunk, FinalizeConfig{
		ChunkSize: 80, MaxSearchContentBytes: 180, CollectionMaxLength: 180, ProviderTokenizer: runeTokenizer{},
	})
	if err != nil || len(parts) != 1 {
		t.Fatalf("finalize: parts=%+v err=%v", parts, err)
	}
	got := parts[0]
	if got.RawContent != raw || got.Content != raw {
		t.Fatalf("raw was changed for enhancement: %q", got.RawContent)
	}
	if got.Enhancement == "" || len(got.Enhancement) >= len(chunk.Enhancement) {
		t.Fatalf("enhancement was not safely cropped: %q", got.Enhancement)
	}
	if !strings.Contains(got.SearchContent, "语义辅助（可能有误，原文优先）：") ||
		got.Tokens > 80 || len([]byte(got.SearchContent)) > 180 {
		t.Fatalf("final boundaries failed: tokens=%d bytes=%d search=%q", got.Tokens, len([]byte(got.SearchContent)), got.SearchContent)
	}
	providerTokens, _ := runeTokenizer{}.CountTokens(context.Background(), got.SearchContent)
	if providerTokens > 80 {
		t.Fatalf("provider tokens=%d", providerTokens)
	}
	if answer := chunktext.Answer(got.RawContent, got.Enhancement); !strings.Contains(answer, got.RawContent) ||
		!strings.Contains(answer, "原文优先") {
		t.Fatalf("answer contract drifted: %q", answer)
	}
}

func TestFinalizeChunkResplitsRawDeterministicallyByUTF8Bytes(t *testing.T) {
	t.Parallel()
	raw := strings.Repeat("界", 36)
	chunk := split.Chunk{Kind: split.BlockText, RawContent: raw, Content: raw, SearchContent: raw,
		Enhancement: "must be dropped before raw is split"}
	cfg := FinalizeConfig{ChunkSize: 64, MaxSearchContentBytes: 24, CollectionMaxLength: 24}
	first, err := FinalizeChunk(context.Background(), chunk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := FinalizeChunk(context.Background(), chunk, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 2 || len(first) != len(second) {
		t.Fatalf("expected deterministic raw re-split: first=%+v second=%+v", first, second)
	}
	var rebuilt strings.Builder
	for i := range first {
		if first[i].RawContent != second[i].RawContent || first[i].Enhancement != "" ||
			len([]byte(first[i].SearchContent)) > 24 {
			t.Fatalf("invalid part %d: first=%+v second=%+v", i, first[i], second[i])
		}
		rebuilt.WriteString(first[i].RawContent)
	}
	if rebuilt.String() != raw {
		t.Fatalf("raw bytes were lost: got %q want %q", rebuilt.String(), raw)
	}
}

func TestFinalizeLargeChunkUsesBoundedResplitAndEnhancementSearch(t *testing.T) {
	t.Parallel()
	t.Run("60KiB raw resplit", func(t *testing.T) {
		tokenizer := &countingRuneTokenizer{}
		raw := strings.Repeat("界", 24<<10)
		parts, err := FinalizeChunk(context.Background(), split.Chunk{
			Kind: split.BlockText, RawContent: raw, Content: raw, SearchContent: raw,
		}, FinalizeConfig{
			ChunkSize: 1_000_000, MaxSearchContentBytes: 60 << 10,
			CollectionMaxLength: 60 << 10, ProviderTokenizer: tokenizer,
		})
		if err != nil || len(parts) < 2 {
			t.Fatalf("large finalize parts=%d err=%v", len(parts), err)
		}
		var rebuilt strings.Builder
		for _, part := range parts {
			if len(part.SearchContent) > 60<<10 {
				t.Fatalf("part bytes=%d exceed 60KiB", len(part.SearchContent))
			}
			rebuilt.WriteString(part.RawContent)
		}
		if rebuilt.String() != raw {
			t.Fatal("bounded resplit lost immutable raw text")
		}
		if calls := tokenizer.calls.Load(); calls > 256 {
			t.Fatalf("bounded resplit made %d tokenizer calls", calls)
		}
	})

	t.Run("enhancement crop", func(t *testing.T) {
		tokenizer := &countingRuneTokenizer{}
		chunk := split.Chunk{RawContent: "raw", SearchContent: "raw", Enhancement: strings.Repeat("x", 20_000)}
		parts, err := FinalizeChunk(context.Background(), chunk, FinalizeConfig{
			ChunkSize: 256, MaxSearchContentBytes: 60 << 10,
			CollectionMaxLength: 60 << 10, ProviderTokenizer: tokenizer,
		})
		if err != nil || len(parts) != 1 || parts[0].Enhancement == "" {
			t.Fatalf("enhancement finalize parts=%+v err=%v", parts, err)
		}
		if calls := tokenizer.calls.Load(); calls > 64 {
			t.Fatalf("enhancement crop made %d tokenizer calls", calls)
		}
	})
}

func TestFinalizeRejectsCollectionMaxLengthMismatch(t *testing.T) {
	t.Parallel()
	_, err := FinalizeChunk(context.Background(), split.Chunk{RawContent: "raw", SearchContent: "raw"}, FinalizeConfig{
		ChunkSize: 10, MaxSearchContentBytes: 101, CollectionMaxLength: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "collection maxLength") {
		t.Fatalf("expected maxLength validation, got %v", err)
	}
}

func TestMemoryCacheIsDocumentScoped(t *testing.T) {
	t.Parallel()
	cache := NewMemoryCache(DefaultSchemaLimits())
	value := Enhancement{Kind: BlockTable, Table: &TableEnhancement{Topic: "x", Summary: "summary"}}
	key := strings.Repeat("a", 64)
	scope := CacheScope{UserID: "u", KBID: "kb", DocID: "doc-a"}
	if err := cache.Put(context.Background(), scope, key, value); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := cache.Get(context.Background(), scope, key, BlockTable); err != nil || !ok || got.Text() != value.Text() {
		t.Fatalf("cache hit got=%+v ok=%v err=%v", got, ok, err)
	}
	if _, ok, _ := cache.Get(context.Background(), CacheScope{UserID: "u", KBID: "kb", DocID: "doc-b"}, key, BlockTable); ok {
		t.Fatal("cache crossed a document prefix")
	}
}

func TestObjectCacheRoundTripsWorstCaseEscapedEnvelope(t *testing.T) {
	t.Parallel()
	limits := SchemaLimits{MaxJSONDepth: 16, MaxFieldBytes: 8, MaxItems: 2, MaxTextBytes: 32}
	cache := NewObjectCache(objects.NewLocalFS(t.TempDir()), limits)
	control := strings.Repeat("\x01", limits.MaxFieldBytes)
	value := Enhancement{Kind: BlockCode, Code: &CodeEnhancement{
		Language: control, Responsibility: control, Description: control, Inputs: []string{control},
	}}
	scope := CacheScope{UserID: "user", KBID: "kb", DocID: "doc"}
	key := strings.Repeat("a", 64)
	if err := cache.Put(context.Background(), scope, key, value); err != nil {
		t.Fatalf("put maximum escaped envelope: %v", err)
	}
	got, ok, err := cache.Get(context.Background(), scope, key, BlockCode)
	if err != nil || !ok || got.Code == nil || got.Code.Description != control {
		t.Fatalf("maximum escaped cache round-trip got=%+v ok=%v err=%v", got, ok, err)
	}
}
