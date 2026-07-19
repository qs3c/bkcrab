package vector

import (
	"context"
	"math"
	"reflect"
	"testing"
)

func mkChunk(docID string, idx, version int, content string, vec []float32) ChunkData {
	return ChunkData{DocID: docID, Index: idx, Content: content, DocVersion: version, Vector: vec}
}

func TestFakeUpsertSearchAndVersionDelete(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatal(err)
	}
	chunks := []ChunkData{
		mkChunk("d1", 0, 1, "北京的天气预报", []float32{1, 0}),
		mkChunk("d1", 1, 1, "上海美食推荐指南", []float32{0, 1}),
	}
	if err := f.UpsertChunks(ctx, "kb1", chunks); err != nil {
		t.Fatal(err)
	}

	// Dense similarity and a keyword match should jointly rank the weather
	// chunk first.
	hits, err := f.HybridSearch(ctx, "kb1", SearchQuery{Dense: [][]float32{{0.9, 0.1}}, Text: "天气"}, 2)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search: %v err=%v", hits, err)
	}
	if hits[0].DocID != "d1" || hits[0].ChunkIndex != 0 {
		t.Fatalf("top1 应是天气 chunk: %+v", hits[0])
	}

	// Version replacement is two explicit operations: write v2, then remove
	// entities older than v2.
	if err := f.UpsertChunks(ctx, "kb1", []ChunkData{mkChunk("d1", 0, 2, "只剩这一条", []float32{1, 0})}); err != nil {
		t.Fatal(err)
	}
	if err := f.DeleteOldVersions(ctx, "kb1", "d1", 2); err != nil {
		t.Fatal(err)
	}
	hits, err = f.HybridSearch(ctx, "kb1", SearchQuery{Dense: [][]float32{{0, 1}}}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Content != "只剩这一条" {
		t.Fatalf("旧版本应被删净: %+v", hits)
	}

	if err := f.DropCollection(ctx, "kb1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.HybridSearch(ctx, "kb1", SearchQuery{Dense: [][]float32{{1, 0}}, Text: "x"}, 1); err == nil {
		t.Fatal("collection 已删应报错")
	}
}

func TestFakeCollectionContractAndHelpers(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatal(err)
	}
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatalf("EnsureCollection 应幂等: %v", err)
	}
	if err := f.EnsureCollection(ctx, "kb1", 3); err == nil {
		t.Fatal("已有 collection 的维度不可改变")
	}
	if !f.HasCollection("kb1") {
		t.Fatal("HasCollection 应返回 true")
	}

	chunks := []ChunkData{
		mkChunk("d1", 0, 1, "first", []float32{1, 0}),
		mkChunk("d2", 0, 2, "second", []float32{0, 1}),
	}
	if err := f.UpsertChunks(ctx, "kb1", chunks); err != nil {
		t.Fatal(err)
	}
	if got := f.Count("kb1"); got != 2 {
		t.Fatalf("Count = %d, want 2", got)
	}
	if err := f.DeleteOldVersions(ctx, "kb1", "d1", 2); err != nil {
		t.Fatal(err)
	}
	wantOps := []string{"upsert_v1", "upsert_v2", "delete_old_v2"}
	if got := f.Ops("kb1"); !reflect.DeepEqual(got, wantOps) {
		t.Fatalf("Ops = %v, want %v", got, wantOps)
	}
	// Ops must be returned as a copy.
	ops := f.Ops("kb1")
	ops[0] = "mutated"
	if got := f.Ops("kb1")[0]; got != "upsert_v1" {
		t.Fatalf("Ops 暴露了内部切片: %q", got)
	}

	if err := f.DeleteDoc(ctx, "kb1", "d2"); err != nil {
		t.Fatal(err)
	}
	if got := f.Count("kb1"); got != 0 {
		t.Fatalf("DeleteDoc 后 Count = %d, want 0", got)
	}
}

func TestFakeRejectsWrongVectorDimensionsWithoutPartialWrite(t *testing.T) {
	f := NewFake()
	ctx := context.Background()
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatal(err)
	}
	err := f.UpsertChunks(ctx, "kb1", []ChunkData{
		mkChunk("d1", 0, 1, "valid", []float32{1, 0}),
		mkChunk("d1", 1, 1, "invalid", []float32{1}),
	})
	if err == nil {
		t.Fatal("维度不匹配应报错")
	}
	if got := f.Count("kb1"); got != 0 {
		t.Fatalf("失败批次不应部分写入, Count = %d", got)
	}
	if _, err := f.HybridSearch(ctx, "kb1", SearchQuery{Dense: [][]float32{{1}}}, 1); err == nil {
		t.Fatal("查询向量维度不匹配应报错")
	}
}

func TestFakeThreeRouteRRF(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatal(err)
	}
	if err := f.UpsertChunks(ctx, "kb1", []ChunkData{
		{DocID: "dense-a", Index: 0, DocVersion: 1, Content: "beta", Vector: []float32{1, 0}},
		{DocID: "dense-b", Index: 0, DocVersion: 1, Content: "alpha", Vector: []float32{0, 1}},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := f.HybridSearch(ctx, "kb1", SearchQuery{
		Dense: [][]float32{{1, 0}, {0, 1}},
		Text:  "alpha",
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].DocID != "dense-b" {
		t.Fatalf("three-route RRF did not combine both dense routes and BM25: %+v", hits)
	}
	want := 1.0/62.0 + 1.0/61.0 + 1.0/61.0
	if math.Abs(hits[0].Score-want) > 1e-12 {
		t.Fatalf("three-route score = %.12f, want %.12f", hits[0].Score, want)
	}
}

func TestFakeBM25IndexesSectionTitleButReturnsOriginalBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := NewFake()
	if err := f.EnsureCollection(ctx, "kb1", 2); err != nil {
		t.Fatal(err)
	}
	if err := f.UpsertChunks(ctx, "kb1", []ChunkData{
		{
			DocID: "title-hit", Index: 0, DocVersion: 1,
			Content: "正文没有查询词", SectionTitle: "罕见安装标题", Vector: []float32{0, 1},
		},
		{
			DocID: "dense-hit", Index: 0, DocVersion: 1,
			Content: "另一段正文", SectionTitle: "其他章节", Vector: []float32{1, 0},
		},
	}); err != nil {
		t.Fatal(err)
	}
	hits, err := f.HybridSearch(ctx, "kb1", SearchQuery{
		Dense: [][]float32{{1, 0}},
		Text:  "罕见安装标题",
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].DocID != "title-hit" {
		t.Fatalf("section-title BM25 route did not affect ranking: %+v", hits)
	}
	if hits[0].Content != "正文没有查询词" {
		t.Fatalf("retrieval exposed indexed title envelope: %q", hits[0].Content)
	}
}
