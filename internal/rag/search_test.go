package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/rerank"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

type stubReranker struct {
	results   []rerank.Result
	err       error
	query     string
	documents []string
	topN      int
}

type searchCountingVector struct {
	*vector.Fake
	ensureCalls int
	searchCalls int
}

type searchUserLookupStore struct {
	store.Store
	getUserCalls map[string]int
}

func (s *searchUserLookupStore) GetUser(ctx context.Context, userID string) (*store.UserRecord, error) {
	s.getUserCalls[userID]++
	return s.Store.GetUser(ctx, userID)
}

func (v *searchCountingVector) EnsureCollection(ctx context.Context, kbID string, dims int) error {
	v.ensureCalls++
	return v.Fake.EnsureCollection(ctx, kbID, dims)
}

func (v *searchCountingVector) HybridSearch(ctx context.Context, kbID string, query vector.SearchQuery, topK int) ([]vector.SearchHit, error) {
	v.searchCalls++
	return v.Fake.HybridSearch(ctx, kbID, query, topK)
}

type activeMapRetryStore struct {
	store.Store
	doc       store.RAGDocumentRecord
	listCalls int
	getCalls  int
}

func (s *activeMapRetryStore) ListRAGDocumentsByKB(context.Context, string) ([]store.RAGDocumentRecord, error) {
	s.listCalls++
	doc := s.doc
	if s.listCalls > 1 {
		doc.ActiveVersion = 2
	}
	return []store.RAGDocumentRecord{doc}, nil
}

func (s *activeMapRetryStore) GetRAGDocument(context.Context, string) (*store.RAGDocumentRecord, error) {
	s.getCalls++
	doc := s.doc
	doc.ActiveVersion = 2
	return &doc, nil
}

type activeMapRetryVector struct {
	*vector.Fake
	searchCalls int
}

type assetBatchStore struct {
	store.Store
	mappings  []store.RAGChunkAssetRecord
	assets    map[string]store.RAGAssetRecord
	batchSize []int
}

func (s *assetBatchStore) ListRAGChunkAssetsByRefs(context.Context, []store.RAGChunkRef) ([]store.RAGChunkAssetRecord, error) {
	return append([]store.RAGChunkAssetRecord(nil), s.mappings...), nil
}

func (s *assetBatchStore) ListRAGAssetsByIDs(_ context.Context, ids []string) ([]store.RAGAssetRecord, error) {
	s.batchSize = append(s.batchSize, len(ids))
	records := make([]store.RAGAssetRecord, 0, len(ids))
	for _, id := range ids {
		records = append(records, s.assets[id])
	}
	return records, nil
}

func (v *activeMapRetryVector) HybridSearch(_ context.Context, _ string, query vector.SearchQuery, _ int) ([]vector.SearchHit, error) {
	v.searchCalls++
	version := query.ActiveVersions["doc_retry_once"]
	return []vector.SearchHit{{
		DocID: "doc_retry_once", DocVersion: version, ChunkIndex: 0,
		Content: "vector payload", SearchContent: "vector search payload", Score: 1,
	}}, nil
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

	hits, err := service.rerankHits(context.Background(), "retrieval-test", "改写后的问题", candidates, 3)
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
	hits, err := service.rerankHits(context.Background(), "retrieval-test", "q", []Hit{
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

func TestSearchWithNoActiveVersionDoesNotAccessVectorStore(t *testing.T) {
	embedding := newEmbeddingServer(t)
	vec := &searchCountingVector{Fake: vector.NewFake()}
	service := New(Deps{
		Store: newRAGTestStore(t), Vector: vec,
		Objects: nil,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	kb, err := service.CreateKB(context.Background(), "u1", "inactive", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.st.CreateRAGDocument(context.Background(), &store.RAGDocumentRecord{
		ID: "doc_not_active", KBID: kb.ID, FileName: "pending.md", FileType: "md",
		Status: "PROCESSING", Version: 1, ActiveVersion: 0, IndexFormatVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	vec.ensureCalls = 0 // CreateKB is expected to initialize the collection.
	hits, err := service.Search(context.Background(), "u1", []string{kb.ID}, "query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || vec.ensureCalls != 0 || vec.searchCalls != 0 {
		t.Fatalf("hits=%+v ensureCalls=%d searchCalls=%d", hits, vec.ensureCalls, vec.searchCalls)
	}
}

func TestSearchRetriesActiveMapChangeAtMostOnce(t *testing.T) {
	embedding := newEmbeddingServer(t)
	baseStore := newRAGTestStore(t)
	retryingStore := &activeMapRetryStore{
		Store: baseStore,
		doc: store.RAGDocumentRecord{
			ID: "doc_retry_once", FileName: "retry.md", FileType: "md", Status: "DONE",
			Version: 2, ActiveVersion: 1, IndexFormatVersion: 1,
		},
	}
	vec := &activeMapRetryVector{Fake: vector.NewFake()}
	service := New(Deps{
		Store: retryingStore, Vector: vec,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	kb, err := service.CreateKB(context.Background(), "u1", "retry", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	retryingStore.doc.KBID = kb.ID

	hits, err := service.Search(context.Background(), "u1", []string{kb.ID}, "query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 || vec.searchCalls != 2 || retryingStore.listCalls != 2 || retryingStore.getCalls != 2 {
		t.Fatalf("hits=%+v searchCalls=%d listCalls=%d getCalls=%d",
			hits, vec.searchCalls, retryingStore.listCalls, retryingStore.getCalls)
	}
}

func TestSearchCrossKBMergeFiltersStagingAndOrphanVectors(t *testing.T) {
	service, fake := newTestService(t, false)
	ctx := context.Background()
	first, err := service.CreateKB(ctx, "u1", "first", "", 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateKB(ctx, "u1", "second", "", 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	for index, kb := range []*store.RAGKBRecord{first, second} {
		docID := fmt.Sprintf("doc_cross_%d", index)
		if err := service.st.CreateRAGDocument(ctx, &store.RAGDocumentRecord{
			ID: docID, KBID: kb.ID, FileName: docID + ".md", FileType: "md",
			Status: "DONE", Version: 2, ActiveVersion: 1, IndexFormatVersion: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if err := service.st.PutRAGChunks(ctx, []store.RAGChunkRecord{{
			KBID: kb.ID, DocID: docID, DocVersion: 1, ChunkIndex: 0,
			RawContent: "active " + docID, SearchContent: "active " + docID, TokenCount: 3,
		}}); err != nil {
			t.Fatal(err)
		}
		if err := fake.UpsertChunks(ctx, kb.ID, []vector.ChunkData{
			{DocID: docID, Index: 0, DocVersion: 1, Content: "active", SearchContent: "active", Vector: []float32{0, 1, 0, 0}},
			{DocID: docID, Index: 0, DocVersion: 2, Content: "staging", SearchContent: "staging", Vector: []float32{0, 1, 0, 0}},
			{DocID: "doc_orphan", Index: 0, DocVersion: 1, Content: "orphan", SearchContent: "orphan", Vector: []float32{0, 1, 0, 0}},
		}); err != nil {
			t.Fatal(err)
		}
	}

	hits, err := service.Search(ctx, "u1", []string{second.ID, first.ID}, "plain query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].KBID > hits[1].KBID {
		t.Fatalf("cross-KB hits were not stable: %+v", hits)
	}
	for _, hit := range hits {
		if hit.DocVersion != 1 || !strings.HasPrefix(hit.Content, "active ") ||
			strings.Contains(hit.Content, "staging") || strings.Contains(hit.Content, "orphan") {
			t.Fatalf("inactive/orphan vector leaked: %+v", hit)
		}
	}
}

func TestSearchExcludesDeletingDocumentImmediately(t *testing.T) {
	service, fake := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "deleting", "", 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	doc := &store.RAGDocumentRecord{
		ID: "doc_deleting_search", KBID: kb.ID, FileName: "manual.md", FileType: "md",
		Status: "DELETING", Version: 1, ActiveVersion: 1, IndexFormatVersion: 1,
	}
	if err := service.st.CreateRAGDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := service.st.PutRAGChunks(ctx, []store.RAGChunkRecord{{
		KBID: kb.ID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0,
		RawContent: "must be revoked", SearchContent: "must be revoked", TokenCount: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := fake.UpsertChunks(ctx, kb.ID, []vector.ChunkData{{
		DocID: doc.ID, Index: 0, DocVersion: 1, Content: "must be revoked",
		SearchContent: "must be revoked", Vector: []float32{0, 1, 0, 0},
	}}); err != nil {
		t.Fatal(err)
	}

	hits, err := service.Search(ctx, "u1", []string{kb.ID}, "plain query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("deleting document leaked into search: %+v", hits)
	}
}

func TestSearchExcludesDeletingUserImmediately(t *testing.T) {
	service, fake := newTestService(t, false)
	ctx := context.Background()
	if err := service.st.CreateUser(ctx, &store.UserRecord{
		ID: "u_deleting", Username: "deleting", Email: "deleting@example.invalid",
		Role: "user", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	kb, err := service.CreateKB(ctx, "u_deleting", "user deleting", "", 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	doc := &store.RAGDocumentRecord{
		ID: "doc_deleting_user_search", KBID: kb.ID, FileName: "manual.md", FileType: "md",
		Status: "DONE", Version: 1, ActiveVersion: 1, IndexFormatVersion: 1,
	}
	if err := service.st.CreateRAGDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := service.st.PutRAGChunks(ctx, []store.RAGChunkRecord{{
		KBID: kb.ID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0,
		RawContent: "must be revoked", SearchContent: "must be revoked", TokenCount: 3,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := fake.UpsertChunks(ctx, kb.ID, []vector.ChunkData{{
		DocID: doc.ID, Index: 0, DocVersion: 1, Content: "must be revoked",
		SearchContent: "must be revoked", Vector: []float32{0, 1, 0, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	user, err := service.st.GetUser(ctx, "u_deleting")
	if err != nil {
		t.Fatal(err)
	}
	user.Status = "deleting"
	if err := service.st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	hits, err := service.Search(ctx, "u_deleting", []string{kb.ID}, "plain query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("deleting user leaked into search: %+v", hits)
	}
	// Even if a legacy/direct user deletion left RAG rows behind, the missing
	// owner row remains a fail-closed tombstone rather than restoring access.
	if err := service.st.DeleteUser(ctx, "u_deleting"); err != nil {
		t.Fatal(err)
	}
	hits, err = service.Search(ctx, "u_deleting", []string{kb.ID}, "plain query", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("missing/deleted user leaked orphaned KB search: %+v", hits)
	}
}

func TestSearchAdminPathsExcludeDeletingActualKBOwner(t *testing.T) {
	// Both platform admin roles reach Service.Search through its privileged
	// empty-owner path. The service deliberately does not receive the actor's
	// role, so keep both caller mappings explicit as regression cases here.
	for _, role := range []string{"admin", "super_admin"} {
		t.Run(role, func(t *testing.T) {
			ctx := context.Background()
			baseStore := newRAGTestStore(t)
			ownerID := "u_search_owner_" + role
			if err := baseStore.CreateUser(ctx, &store.UserRecord{
				ID: ownerID, Username: ownerID, Email: ownerID + "@example.invalid",
				Role: "user", Status: "active",
			}); err != nil {
				t.Fatal(err)
			}

			lookupStore := &searchUserLookupStore{
				Store: baseStore, getUserCalls: make(map[string]int),
			}
			vec := &searchCountingVector{Fake: vector.NewFake()}
			service := New(Deps{
				Store: lookupStore, Vector: vec,
				Cfg: config.RAGCfg{
					Milvus: config.MilvusCfg{Address: "fake"},
					Embedding: config.RAGEmbeddingCfg{
						Endpoint: newEmbeddingServer(t).URL, Model: "embed-test", Dims: 4,
					},
				},
			})
			first, err := service.CreateKB(ctx, ownerID, "first", "", 512, 64)
			if err != nil {
				t.Fatal(err)
			}
			second, err := service.CreateKB(ctx, ownerID, "second", "", 512, 64)
			if err != nil {
				t.Fatal(err)
			}
			doc := &store.RAGDocumentRecord{
				ID: "doc_admin_tombstone_" + role, KBID: first.ID,
				FileName: "manual.md", FileType: "md", Status: "DONE",
				Version: 1, ActiveVersion: 1, IndexFormatVersion: 1,
			}
			if err := baseStore.CreateRAGDocument(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if err := baseStore.PutRAGChunks(ctx, []store.RAGChunkRecord{{
				KBID: first.ID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0,
				RawContent: "must be revoked", SearchContent: "must be revoked", TokenCount: 3,
			}}); err != nil {
				t.Fatal(err)
			}
			if err := vec.UpsertChunks(ctx, first.ID, []vector.ChunkData{{
				DocID: doc.ID, Index: 0, DocVersion: 1, Content: "must be revoked",
				SearchContent: "must be revoked", Vector: []float32{0, 1, 0, 0},
			}}); err != nil {
				t.Fatal(err)
			}
			if _, err := baseStore.MarkUserDeleting(ctx, ownerID); err != nil {
				t.Fatal(err)
			}

			lookupStore.getUserCalls = make(map[string]int)
			vec.searchCalls = 0
			hits, err := service.Search(ctx, "", []string{first.ID, second.ID}, "plain query", 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) != 0 || vec.searchCalls != 0 {
				t.Fatalf("%s search crossed owner tombstone: hits=%+v searchCalls=%d",
					role, hits, vec.searchCalls)
			}
			if calls := lookupStore.getUserCalls[ownerID]; calls != 1 {
				t.Fatalf("actual KB owner active checks were not deduplicated: calls=%d", calls)
			}
		})
	}
}

func TestSearchHydratesCatalogForRerankAndLoadsReadyAssets(t *testing.T) {
	service, fake := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "图文手册", "", 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	doc := &store.RAGDocumentRecord{
		ID: "doc_hydrate", KBID: kb.ID, FileName: "manual.pdf", FileType: "pdf",
		Status: "DONE", Version: 2, ActiveVersion: 1, IndexFormatVersion: 1,
	}
	if err := service.st.CreateRAGDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	location := document.SourceLocation{Kind: document.LocationPage, Index: 3, Label: "第 3 页"}
	locationJSON, _ := json.Marshal(location)
	if err := service.st.PutRAGChunks(ctx, []store.RAGChunkRecord{{
		KBID: kb.ID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0,
		SectionTitle: "安装 > 架构", LocationJSON: string(locationJSON),
		RawContent: "原始正文", Enhancement: "表格摘要", SearchContent: "章节：安装 > 架构\n\n原始正文\n\n表格摘要",
		TokenCount: 12, CreatedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	asset := &store.RAGAssetRecord{
		ID: "ast_ready", DocID: doc.ID, ContentSHA256: strings.Repeat("a", 64),
		SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: "image/png", DisplayMIME: "image/webp",
		SourceObjectKey: "source", DisplayObjectKey: "display", ThumbnailObjectKey: "thumb",
		DisplayStatus: document.DisplayReady, DisplaySHA256: strings.Repeat("b", 64),
		ThumbnailSHA256: strings.Repeat("c", 64),
		ByteSize:        128, Width: 640, Height: 480, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := service.st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	firstAsset := *asset
	firstAsset.ID = "ast_first"
	firstAsset.ContentSHA256 = strings.Repeat("d", 64)
	firstAsset.SourceObjectKey = "source-first"
	firstAsset.DisplayObjectKey = "display-first"
	firstAsset.ThumbnailObjectKey = "thumb-first"
	firstAsset.DisplaySHA256 = strings.Repeat("e", 64)
	firstAsset.ThumbnailSHA256 = strings.Repeat("f", 64)
	if err := service.st.UpsertRAGAsset(ctx, &firstAsset); err != nil {
		t.Fatal(err)
	}
	unavailable := firstAsset
	unavailable.ID = "ast_unavailable"
	unavailable.ContentSHA256 = strings.Repeat("1", 64)
	unavailable.SourceObjectKey = "source-unavailable"
	unavailable.DisplayObjectKey = "display-unavailable"
	unavailable.ThumbnailObjectKey = "thumb-unavailable"
	unavailable.DisplaySHA256 = strings.Repeat("2", 64)
	unavailable.ThumbnailSHA256 = strings.Repeat("3", 64)
	unavailable.DisplayStatus = document.DisplayUnavailable
	if err := service.st.UpsertRAGAsset(ctx, &unavailable); err != nil {
		t.Fatal(err)
	}
	if err := service.st.PutRAGChunkAssets(ctx, []store.RAGChunkAssetRecord{
		{DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, AssetID: firstAsset.ID,
			Ordinal: 0, LocationJSON: string(locationJSON), Caption: "first", OCRText: "A"},
		{DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, AssetID: asset.ID,
			Ordinal: 1, LocationJSON: string(locationJSON), Caption: "架构图", OCRText: "Gateway"},
		{DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, AssetID: unavailable.ID,
			Ordinal: 2, LocationJSON: string(locationJSON), Caption: "not ready", OCRText: "hidden"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := fake.UpsertChunks(ctx, kb.ID, []vector.ChunkData{
		{DocID: doc.ID, Index: 0, DocVersion: 1, Content: "不得作为新格式正文", SearchContent: "vector search payload", Vector: []float32{1, 0, 0, 0}},
		{DocID: doc.ID, Index: 0, DocVersion: 2, Content: "未激活高分版本", SearchContent: "staging", Vector: []float32{1, 0, 0, 0}},
	}); err != nil {
		t.Fatal(err)
	}
	ranker := &stubReranker{results: []rerank.Result{{Index: 0, Score: 0.9}}}
	service.reranker = ranker
	hits, err := service.Search(ctx, "u1", []string{kb.ID}, "安装权限", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranker.documents) != 1 || ranker.documents[0] != "章节：安装 > 架构\n\n原始正文\n\n表格摘要" {
		t.Fatalf("reranker did not receive hydrated SearchContent: %#v", ranker.documents)
	}
	if len(hits) != 1 || hits[0].DocVersion != 1 || hits[0].Content != "原始正文" || hits[0].Enhancement != "表格摘要" {
		t.Fatalf("hit did not use active SQL catalog: %+v", hits)
	}
	if got := hits[0].AnswerText(); !strings.Contains(got, "原始正文") || !strings.Contains(got, "语义辅助") {
		t.Fatalf("answer text = %q", got)
	}
	if len(hits[0].Assets) != 2 || hits[0].Assets[0].ID != firstAsset.ID ||
		hits[0].Assets[1].ID != asset.ID || hits[0].Assets[1].PageNum != 3 {
		t.Fatalf("ready assets were not hydrated after top-N: %+v", hits[0].Assets)
	}
	resources := BuildRAGResourceRefs(append(hits, hits...))
	if len(resources) != 2 || resources[0].Asset.ID != firstAsset.ID || resources[1].Asset.ID != asset.ID ||
		resources[0].KBID != kb.ID || resources[0].DocID != doc.ID {
		t.Fatalf("resource refs were not stable/deduplicated: %+v", resources)
	}
}

func TestRerankUsesExplicitSearchContent(t *testing.T) {
	ranker := &stubReranker{results: []rerank.Result{{Index: 0, Score: 0.9}}}
	service := &Service{cfg: config.RAGCfg{Reranker: config.RAGRerankerCfg{MinScore: 0.5}}, reranker: ranker}
	_, err := service.rerankHits(context.Background(), "retrieval-test", "query", []Hit{{
		Content: "raw", Enhancement: "enhancement", SectionTitle: "heading", SearchContent: "canonical search content",
	}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranker.documents) != 1 || ranker.documents[0] != "canonical search content" {
		t.Fatalf("reranker documents = %#v", ranker.documents)
	}
}

func TestLegacyHitNeverHydratesAssets(t *testing.T) {
	hits := []Hit{{DocID: "doc_legacy", DocVersion: 1, ChunkIndex: 0, IndexFormatVersion: 0}}
	got, err := (&Service{}).hydrateHitAssets(context.Background(), hits)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Assets) != 0 {
		t.Fatalf("legacy hit assets = %+v", got)
	}
}

func TestHydrateHitAssetsBatchesLargeAssetSets(t *testing.T) {
	const assetCount = 401
	batchStore := &assetBatchStore{assets: make(map[string]store.RAGAssetRecord, assetCount)}
	for index := 0; index < assetCount; index++ {
		id := fmt.Sprintf("ast_%04d", index)
		batchStore.mappings = append(batchStore.mappings, store.RAGChunkAssetRecord{
			DocID: "doc_many_assets", DocVersion: 1, ChunkIndex: 0, AssetID: id, Ordinal: index,
		})
		batchStore.assets[id] = store.RAGAssetRecord{
			ID: id, DocID: "doc_many_assets", DisplayMIME: "image/png",
			DisplayStatus: document.DisplayReady, DisplaySHA256: strings.Repeat("a", 64),
			ThumbnailSHA256: strings.Repeat("b", 64), DisplayObjectKey: "display/" + id,
			ThumbnailObjectKey: "thumbnail/" + id, FirstSeenVersion: 1, LastSeenVersion: 1,
		}
	}
	service := &Service{st: batchStore}
	hits, err := service.hydrateHitAssets(context.Background(), []Hit{{
		DocID: "doc_many_assets", DocVersion: 1, ChunkIndex: 0, IndexFormatVersion: 1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hydrated hits = %d", len(hits))
	}
	if len(hits[0].Assets) != assetCount {
		t.Fatalf("hydrated assets = %d", len(hits[0].Assets))
	}
	if fmt.Sprint(batchStore.batchSize) != "[200 200 1]" ||
		hits[0].Assets[0].ID != "ast_0000" || hits[0].Assets[assetCount-1].ID != "ast_0400" {
		t.Fatalf("batches=%v first/last=%q/%q", batchStore.batchSize,
			hits[0].Assets[0].ID, hits[0].Assets[len(hits[0].Assets)-1].ID)
	}
}
