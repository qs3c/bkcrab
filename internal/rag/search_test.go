package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/rerank"
)

type stubReranker struct {
	results   []rerank.Result
	err       error
	query     string
	documents []string
	topN      int
}

func (r *stubReranker) Rerank(_ context.Context, query string, documents []string, topN int) ([]rerank.Result, error) {
	r.query = query
	r.documents = append([]string(nil), documents...)
	r.topN = topN
	return append([]rerank.Result(nil), r.results...), r.err
}

func TestRerankHitsSortsFiltersAndPreservesRecallScore(t *testing.T) {
	ranker := &stubReranker{results: []rerank.Result{
		{Index: 1, Score: 0.4},
		{Index: 0, Score: 0.7},
		{Index: 2, Score: 0.9},
	}}
	service := &Service{
		cfg:      config.RAGCfg{Reranker: config.RAGRerankerCfg{MinScore: 0.5}},
		reranker: ranker,
	}
	candidates := []Hit{
		{DocID: "a", Content: "A", Score: 0.03, RecallScore: 0.03},
		{DocID: "b", Content: "B", Score: 0.02, RecallScore: 0.02},
		{DocID: "c", Content: "C", Score: 0.01, RecallScore: 0.01},
	}

	hits, err := service.rerankHits(context.Background(), "改写后的问题", candidates, 3)
	if err != nil {
		t.Fatal(err)
	}
	if ranker.query != "改写后的问题" || ranker.topN != 3 || len(ranker.documents) != 3 {
		t.Fatalf("reranker call = query:%q topN:%d docs:%v", ranker.query, ranker.topN, ranker.documents)
	}
	if len(hits) != 2 || hits[0].DocID != "c" || hits[1].DocID != "a" {
		t.Fatalf("filtered hits = %+v", hits)
	}
	if hits[0].Score != 0.9 || hits[0].RecallScore != 0.01 ||
		hits[0].RerankScore == nil || *hits[0].RerankScore != 0.9 {
		t.Fatalf("score provenance was not preserved: %+v", hits[0])
	}
}

func TestRerankHitsReturnsEmptyWhenAllScoresAreLow(t *testing.T) {
	service := &Service{
		cfg: config.RAGCfg{Reranker: config.RAGRerankerCfg{MinScore: 0.5}},
		reranker: &stubReranker{results: []rerank.Result{
			{Index: 0, Score: 0.49},
			{Index: 1, Score: 0.1},
		}},
	}
	hits, err := service.rerankHits(context.Background(), "q", []Hit{
		{Content: "a"}, {Content: "b"},
	}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("all-low rerank should return no hits: %+v", hits)
	}
}

func TestSearchFallsBackToRRFWithoutConfidenceFilter(t *testing.T) {
	service, _ := newTestService(t, true)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "手册", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	content := "# 安装\n\n安装需要管理员权限。"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "guide.md",
		strings.NewReader(content), int64(len([]byte(content))))
	if err != nil {
		t.Fatal(err)
	}
	waitDocumentStatus(t, service, doc.ID, "DONE")
	service.reranker = &stubReranker{err: errors.New("reranker unavailable")}

	hits, err := service.Search(ctx, "u1", []string{kb.ID}, "安装权限", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("reranker failure should preserve RRF hits")
	}
	for _, hit := range hits {
		if hit.RerankScore != nil || hit.Score != hit.RecallScore {
			t.Fatalf("fallback hit should contain only the RRF score: %+v", hit)
		}
	}
}
