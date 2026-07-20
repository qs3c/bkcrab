package vector

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/milvus-io/milvus/client/v2/entity"
)

func TestMilvusSchemaContract(t *testing.T) {
	schema := ragMilvusSchema(4)
	fields := make(map[string]*entity.Field, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Name] = field
	}

	wantTypes := map[string]entity.FieldType{
		milvusFieldID:           entity.FieldTypeVarChar,
		milvusFieldDocID:        entity.FieldTypeVarChar,
		milvusFieldChunkIndex:   entity.FieldTypeInt64,
		milvusFieldSectionTitle: entity.FieldTypeVarChar,
		milvusFieldPageNum:      entity.FieldTypeInt64,
		milvusFieldDocVersion:   entity.FieldTypeInt64,
		milvusFieldContent:      entity.FieldTypeVarChar,
		milvusFieldSparse:       entity.FieldTypeSparseVector,
		milvusFieldEmbedding:    entity.FieldTypeFloatVector,
	}
	for name, wantType := range wantTypes {
		field := fields[name]
		if field == nil {
			t.Fatalf("schema 缺少字段 %s", name)
		}
		if field.DataType != wantType {
			t.Fatalf("字段 %s 类型 = %v, want %v", name, field.DataType, wantType)
		}
	}
	if !fields[milvusFieldID].PrimaryKey || fields[milvusFieldID].AutoID {
		t.Fatalf("id 字段必须是手工赋值的主键: %+v", fields[milvusFieldID])
	}
	if dim, err := fields[milvusFieldEmbedding].GetDim(); err != nil || dim != 4 {
		t.Fatalf("embedding dim = %d, err=%v", dim, err)
	}
	contentParams := fields[milvusFieldContent].TypeParams
	if contentParams["enable_analyzer"] != "true" || contentParams["analyzer_params"] != `{"type":"chinese"}` {
		t.Fatalf("content analyzer 配置错误: %v", contentParams)
	}
	if len(schema.Functions) != 1 {
		t.Fatalf("BM25 function 数量 = %d, want 1", len(schema.Functions))
	}
	fn := schema.Functions[0]
	if fn.Type != entity.FunctionTypeBM25 || len(fn.InputFieldNames) != 1 || fn.InputFieldNames[0] != milvusFieldContent ||
		len(fn.OutputFieldNames) != 1 || fn.OutputFieldNames[0] != milvusFieldSparse {
		t.Fatalf("BM25 function 配置错误: %+v", fn)
	}
}

func TestMilvusHelpers(t *testing.T) {
	if got, want := ragCollectionName("kb-a/中文"), "rag_kb_a___"; got != want {
		t.Fatalf("ragCollectionName = %q, want %q", got, want)
	}
	chunk := ChunkData{DocID: "doc_1", DocVersion: 1 << 40, Index: 7}
	if got, want := milvusChunkID(chunk), "doc_1_1099511627776_7"; got != want {
		t.Fatalf("milvusChunkID = %q, want %q", got, want)
	}
	if got, want := escapeMilvusExprString("a\\b\"c"), "a\\\\b\\\"c"; got != want {
		t.Fatalf("escapeMilvusExprString = %q, want %q", got, want)
	}
	if got, want := milvusDocVersionExpr("a\\b\"c", 1<<40),
		`doc_id == "a\\b\"c" && doc_version == 1099511627776`; got != want {
		t.Fatalf("milvusDocVersionExpr = %q, want %q", got, want)
	}
	if _, err := NewMilvus(context.Background(), "", "", ""); err == nil {
		t.Fatal("空地址应在连接前被拒绝")
	}
}

func TestMilvusActiveVersionFilterIsStableAndBounded(t *testing.T) {
	t.Parallel()
	active := map[string]int64{"doc_b": 2, "doc_a": 1, "doc_c": 2}
	got, err := buildActiveVersionFilter(active, 32*1024)
	if err != nil {
		t.Fatal(err)
	}
	want := `((doc_version == 1 && doc_id in ["doc_a"]) || (doc_version == 2 && doc_id in ["doc_b","doc_c"]))`
	if got != want {
		t.Fatalf("filter = %q, want %q", got, want)
	}
	if _, err := buildActiveVersionFilter(map[string]int64{`doc_"unsafe`: 1}, 32*1024); err == nil {
		t.Fatal("unsafe document ID was accepted")
	}
	if _, err := buildActiveVersionFilter(active, len(got)-1); err == nil {
		t.Fatal("oversized filter was accepted")
	}
}

func TestMilvusActiveVersionFilterRejectsDocumentIDBeyondDBContract(t *testing.T) {
	tooLong := "d" + strings.Repeat("x", 64)
	if _, err := buildActiveVersionFilter(map[string]int64{tooLong: 1}, 32*1024); err == nil {
		t.Fatal("active-version filter accepted a document ID longer than VARCHAR(64)")
	}
}

func TestMilvusRoundTrip(t *testing.T) {
	addr := os.Getenv("RAG_TEST_MILVUS_ADDR")
	if addr == "" {
		t.Skip("RAG_TEST_MILVUS_ADDR 未设置,跳过 Milvus 集成测试")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	m, err := NewMilvus(ctx, addr, os.Getenv("RAG_TEST_MILVUS_USER"), os.Getenv("RAG_TEST_MILVUS_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		if err := m.Close(closeCtx); err != nil {
			t.Logf("close Milvus client: %v", err)
		}
	})

	kbID := fmt.Sprintf("test_roundtrip_%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := m.DropCollection(cleanupCtx, kbID); err != nil {
			t.Logf("cleanup collection: %v", err)
		}
	})

	if err := m.EnsureCollection(ctx, kbID, 4); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureCollection(ctx, kbID, 4); err != nil {
		t.Fatalf("EnsureCollection 应幂等: %v", err)
	}
	chunks := []ChunkData{
		{
			DocID: "d1", Index: 0, Content: "北京今天天气晴朗", SectionTitle: "天气", PageNum: 1,
			DocVersion: 1, Vector: []float32{1, 0, 0, 0},
		},
		{
			DocID: "d1", Index: 1, Content: "上海美食小笼包很有名", SectionTitle: "美食", PageNum: 2,
			DocVersion: 1, Vector: []float32{0, 1, 0, 0},
		},
	}
	if err := m.UpsertChunks(ctx, kbID, chunks); err != nil {
		t.Fatal(err)
	}

	hits, err := m.HybridSearch(ctx, kbID, SearchQuery{
		Dense:          [][]float32{{0.9, 0.1, 0, 0}, {0.8, 0.2, 0, 0}},
		Text:           "天气",
		ActiveVersions: map[string]int64{"d1": 1},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ChunkIndex != 0 {
		t.Fatalf("hybrid top1 应为天气 chunk: %+v", hits)
	}
	if hits[0].SectionTitle != "天气" || hits[0].PageNum != 1 {
		t.Fatalf("结果元数据未完整返回: %+v", hits[0])
	}
	if hits[0].DocVersion != 1 {
		t.Fatalf("结果 doc_version = %d, want 1", hits[0].DocVersion)
	}

	if err := m.UpsertChunks(ctx, kbID, []ChunkData{{
		DocID: "d1", Index: 0, Content: "新版本", DocVersion: 2, Vector: []float32{1, 0, 0, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := m.DeleteDocVersion(ctx, kbID, "d1", 1); err != nil {
		t.Fatal(err)
	}
	hits, err = m.HybridSearch(ctx, kbID, SearchQuery{Dense: [][]float32{{1, 0, 0, 0}}, ActiveVersions: map[string]int64{"d1": 2}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Content != "新版本" {
		t.Fatalf("旧版本应删净: %+v", hits)
	}

	if err := m.DeleteDoc(ctx, kbID, "d1"); err != nil {
		t.Fatal(err)
	}
	hits, err = m.HybridSearch(ctx, kbID, SearchQuery{Dense: [][]float32{{1, 0, 0, 0}}, ActiveVersions: map[string]int64{"d1": 2}}, 10)
	if err != nil || len(hits) != 0 {
		t.Fatalf("DeleteDoc 后仍有结果: %+v err=%v", hits, err)
	}
	if err := m.DropCollection(ctx, kbID); err != nil {
		t.Fatal(err)
	}
	if err := m.DropCollection(ctx, kbID); err != nil {
		t.Fatalf("DropCollection 对不存在 collection 应幂等: %v", err)
	}
}
