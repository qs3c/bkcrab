package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

func newRAGTestStore(t *testing.T) *store.DBStore {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newEmbeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		response := struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}{}
		for index, text := range request.Input {
			vector := []float32{0, 1, 0, 0}
			if strings.Contains(text, "安装") || strings.Contains(text, "管理员") || strings.Contains(text, "权限") {
				vector = []float32{1, 0, 0, 0}
			}
			response.Data = append(response.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: vector, Index: index})
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server
}

// blockingEmbeddingServer lets a test stop each embedding request at a known
// point. Positive waits still have deadlines so a broken worker cannot hang
// the test suite, but ordering never depends on an arbitrary sleep.
type blockingEmbeddingServer struct {
	server  *httptest.Server
	entered chan int
	release chan struct{}
	stop    chan struct{}
	calls   atomic.Int64
}

func newBlockingEmbeddingServer(t *testing.T) *blockingEmbeddingServer {
	t.Helper()
	gate := &blockingEmbeddingServer{
		entered: make(chan int, 4),
		release: make(chan struct{}),
		stop:    make(chan struct{}),
	}
	gate.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		call := int(gate.calls.Add(1))
		select {
		case gate.entered <- call:
		case <-gate.stop:
			return
		case <-r.Context().Done():
			return
		}
		select {
		case <-gate.release:
		case <-gate.stop:
			return
		case <-r.Context().Done():
			return
		}

		response := struct {
			Data []struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			} `json:"data"`
		}{}
		for index := range request.Input {
			response.Data = append(response.Data, struct {
				Embedding []float32 `json:"embedding"`
				Index     int       `json:"index"`
			}{Embedding: []float32{1, 0, 0, 0}, Index: index})
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(func() {
		close(gate.stop)
		gate.server.Close()
	})
	return gate
}

func (g *blockingEmbeddingServer) waitForCall(t *testing.T, want int) {
	t.Helper()
	select {
	case got := <-g.entered:
		if got != want {
			t.Fatalf("embedding call = %d, want %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("embedding call %d did not start", want)
	}
}

func (g *blockingEmbeddingServer) releaseCall(t *testing.T) {
	t.Helper()
	select {
	case g.release <- struct{}{}:
	case <-time.After(5 * time.Second):
		t.Fatal("blocked embedding call was not waiting for release")
	}
}

// versionTrackingVector decorates the regular in-memory fake with enough
// visibility to assert which document versions remain after cleanup.
type versionTrackingVector struct {
	*vector.Fake
	mu       sync.Mutex
	versions map[string]map[string]map[int64]int
}

func newVersionTrackingVector() *versionTrackingVector {
	return &versionTrackingVector{
		Fake:     vector.NewFake(),
		versions: make(map[string]map[string]map[int64]int),
	}
}

func (v *versionTrackingVector) UpsertChunks(ctx context.Context, kbID string, chunks []vector.ChunkData) error {
	if err := v.Fake.UpsertChunks(ctx, kbID, chunks); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.versions[kbID] == nil {
		v.versions[kbID] = make(map[string]map[int64]int)
	}
	for _, chunk := range chunks {
		if v.versions[kbID][chunk.DocID] == nil {
			v.versions[kbID][chunk.DocID] = make(map[int64]int)
		}
		v.versions[kbID][chunk.DocID][chunk.DocVersion]++
	}
	return nil
}

func (v *versionTrackingVector) DeleteDocVersion(ctx context.Context, kbID, docID string, version int64) error {
	if err := v.Fake.DeleteDocVersion(ctx, kbID, docID, version); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.versions[kbID][docID], version)
	return nil
}

func (v *versionTrackingVector) DeleteDoc(ctx context.Context, kbID, docID string) error {
	if err := v.Fake.DeleteDoc(ctx, kbID, docID); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.versions[kbID], docID)
	return nil
}

func (v *versionTrackingVector) documentVersions(kbID, docID string) []int64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	versions := make([]int64, 0, len(v.versions[kbID][docID]))
	for version := range v.versions[kbID][docID] {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions
}

func newGatedTestService(t *testing.T, endpoint string, vec vector.Store) *Service {
	t.Helper()
	service := New(Deps{
		Store:   newRAGTestStore(t),
		Vector:  vec,
		Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: endpoint, Model: "embed-test", Dims: 4},
		},
		Workers: 1,
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.Start(ctx)
	return service
}

func newTestService(t *testing.T, start bool) (*Service, *vector.Fake) {
	t.Helper()
	embedding := newEmbeddingServer(t)
	fake := vector.NewFake()
	cfg := config.RAGCfg{
		Milvus:    config.MilvusCfg{Address: "fake"},
		Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
	}
	service := New(Deps{
		Store:   newRAGTestStore(t),
		Vector:  fake,
		Objects: objects.NewLocalFS(t.TempDir()),
		Cfg:     cfg,
		Workers: 1,
	})
	if start {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		service.Start(ctx)
	}
	return service, fake
}

func waitDocumentStatus(t *testing.T, service *Service, docID, wanted string) *store.RAGDocumentRecord {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		doc, err := service.st.GetRAGDocument(context.Background(), docID)
		if err == nil && doc.Status == wanted {
			return doc
		}
		time.Sleep(20 * time.Millisecond)
	}
	doc, _ := service.st.GetRAGDocument(context.Background(), docID)
	t.Fatalf("document %s did not reach %s: %+v", docID, wanted, doc)
	return nil
}

func TestKnowledgeBaseLifecycleAndOwnership(t *testing.T) {
	service, fake := newTestService(t, true)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "产品手册", "说明", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if kb.EmbedModel != "embed-test" || kb.EmbedDims != 4 || kb.ChunkSize != 512 || kb.ChunkOverlap != 64 {
		t.Fatalf("knowledge-base snapshot/defaults are wrong: %+v", kb)
	}
	if !fake.HasCollection(kb.ID) {
		t.Fatal("CreateKB did not ensure a collection")
	}
	if _, err := service.GetKB(ctx, "u2", kb.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-user GetKB error = %v", err)
	}
	service.cfg.Limits.MaxKBsPerUser = 1
	if _, err := service.CreateKB(ctx, "u1", "超额", "", 0, 0); !errors.Is(err, ErrQuota) {
		t.Fatalf("quota error = %v", err)
	}
	if err := service.DeleteKB(ctx, "u1", kb.ID); err != nil {
		t.Fatal(err)
	}
	if fake.HasCollection(kb.ID) {
		t.Fatal("DeleteKB did not drop collection")
	}
}

func TestUploadReindexSearchAndDelete(t *testing.T) {
	service, fake := newTestService(t, true)
	ctx := context.Background()
	manual, err := service.CreateKB(ctx, "u1", "产品手册", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := service.CreateKB(ctx, "u1", "运维文档", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	manualText := "# 安装\n\n安装需要管理员权限。"
	manualDoc, err := service.UploadDocument(ctx, "u1", manual.ID, "guide.md", strings.NewReader(manualText), int64(len([]byte(manualText))))
	if err != nil {
		t.Fatal(err)
	}
	opsText := "# 部署\n\n部署采用蓝绿发布。"
	opsDoc, err := service.UploadDocument(ctx, "u1", ops.ID, "ops.md", strings.NewReader(opsText), int64(len([]byte(opsText))))
	if err != nil {
		t.Fatal(err)
	}
	indexed := waitDocumentStatus(t, service, manualDoc.ID, "DONE")
	waitDocumentStatus(t, service, opsDoc.ID, "DONE")
	if indexed.ChunkCount == 0 || indexed.TokenCount == 0 || indexed.IndexedAt == nil {
		t.Fatalf("index statistics were not persisted: %+v", indexed)
	}
	if fake.Count(manual.ID) != indexed.ChunkCount {
		t.Fatalf("vector count = %d, document chunks = %d", fake.Count(manual.ID), indexed.ChunkCount)
	}
	storedChunks, err := fake.GetChunks(ctx, manual.ID, []vector.ChunkRef{{
		DocID: manualDoc.ID, Index: 0, DocVersion: manualDoc.Version,
	}})
	if err != nil || len(storedChunks) != 1 {
		t.Fatalf("read indexed chunk: chunks=%+v err=%v", storedChunks, err)
	}
	if !strings.HasPrefix(storedChunks[0].SearchContent, "章节：安装\n\n") ||
		strings.HasPrefix(storedChunks[0].Content, "章节：") {
		t.Fatalf("indexed and display content were not separated: %+v", storedChunks[0])
	}

	hits, err := service.Search(ctx, "u1", []string{manual.ID, ops.ID}, "安装权限", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("search hits=%+v err=%v", hits, err)
	}
	if hits[0].KBID != manual.ID || hits[0].DocName != "guide.md" {
		t.Fatalf("unexpected top hit: %+v", hits[0])
	}
	formatted := FormatHits(hits)
	if !strings.Contains(formatted, "[来源: guide.md") || !strings.Contains(formatted, "安装") {
		t.Fatalf("formatted result has no citation: %q", formatted)
	}

	if err := service.ReindexDocument(ctx, "u1", manual.ID, manualDoc.ID); err != nil {
		t.Fatal(err)
	}
	reindexed := waitDocumentStatus(t, service, manualDoc.ID, "DONE")
	if reindexed.Version != 2 {
		t.Fatalf("reindex version = %d, want 2", reindexed.Version)
	}
	operations := fake.Ops(manual.ID)
	upsert := -1
	for index, operation := range operations {
		if operation == "upsert_v2" && upsert < 0 {
			upsert = index
		}
		if operation == "delete_v1" {
			t.Fatalf("reindex deleted a retired version before delayed GC: %v", operations)
		}
	}
	if upsert < 0 {
		t.Fatalf("new version was not upserted: %v", operations)
	}

	if err := service.DeleteDocument(ctx, "u1", manual.ID, manualDoc.ID); err != nil {
		t.Fatal(err)
	}
	if fake.Count(manual.ID) != 0 {
		t.Fatal("DeleteDocument left vector chunks behind")
	}
}

func TestDeleteDocumentWaitsForInFlightIndex(t *testing.T) {
	gate := newBlockingEmbeddingServer(t)
	fake := vector.NewFake()
	service := newGatedTestService(t, gate.server.URL, fake)
	ctx := context.Background()

	kb, err := service.CreateKB(ctx, "u1", "delete while indexing", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	content := "indexing is deliberately blocked at the embedding request"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "blocked.txt", strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	gate.waitForCall(t, 1)

	processing, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if processing.Status != "PROCESSING" {
		t.Fatalf("document status while embedding is blocked = %q, want PROCESSING", processing.Status)
	}
	tasks, err := service.st.ListRunnableRAGIndexTasks(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("in-flight tasks = %+v, err=%v", tasks, err)
	}
	taskID := tasks[0].ID

	deleteStarted := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		close(deleteStarted)
		deleteDone <- service.DeleteDocument(ctx, "u1", kb.ID, doc.ID)
	}()
	<-deleteStarted
	select {
	case err := <-deleteDone:
		t.Fatalf("DeleteDocument returned before the in-flight embedding was released: %v", err)
	case <-time.After(100 * time.Millisecond):
		// The index worker still owns the document lock, so deletion must wait.
	}

	gate.releaseCall(t)
	select {
	case err := <-deleteDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DeleteDocument did not finish after indexing was released")
	}

	if _, err := service.st.GetRAGDocument(ctx, doc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("document row remained after deletion: %v", err)
	}
	if _, err := service.st.GetRAGIndexTask(ctx, taskID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("index task row remained after document deletion: %v", err)
	}
	if count := fake.Count(kb.ID); count != 0 {
		t.Fatalf("document vectors remained after deletion: %d", count)
	}
}

func TestReindexWaitsForInFlightIndexWithoutVersionRollback(t *testing.T) {
	gate := newBlockingEmbeddingServer(t)
	tracked := newVersionTrackingVector()
	service := newGatedTestService(t, gate.server.URL, tracked)
	ctx := context.Background()

	kb, err := service.CreateKB(ctx, "u1", "reindex while indexing", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	content := "the first index and reindex use separately gated embedding calls"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "versioned.txt", strings.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	gate.waitForCall(t, 1)

	reindexStarted := make(chan struct{})
	reindexDone := make(chan error, 1)
	go func() {
		close(reindexStarted)
		reindexDone <- service.ReindexDocument(ctx, "u1", kb.ID, doc.ID)
	}()
	<-reindexStarted
	select {
	case err := <-reindexDone:
		t.Fatalf("ReindexDocument returned before version 1 indexing was released: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Reindex is serialized behind the worker holding the document lock.
	}

	gate.releaseCall(t)
	select {
	case err := <-reindexDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReindexDocument did not enqueue version 2 after version 1 completed")
	}

	gate.waitForCall(t, 2)
	processing, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if processing.Version != 2 || processing.Status != "PROCESSING" {
		t.Fatalf("document during second embedding = %+v, want PROCESSING version 2", processing)
	}
	gate.releaseCall(t)

	indexed := waitDocumentStatus(t, service, doc.ID, "DONE")
	if indexed.Version != 2 {
		t.Fatalf("final document version = %d, want 2", indexed.Version)
	}
	versions := tracked.documentVersions(kb.ID, doc.ID)
	if len(versions) != 2 || versions[0] != 1 || versions[1] != 2 {
		t.Fatalf("vector versions after reindex = %v, want retired v1 retained until delayed GC", versions)
	}
	ops := tracked.Ops(kb.ID)
	wantOps := []string{"upsert_v1", "upsert_v2"}
	if len(ops) != len(wantOps) {
		t.Fatalf("vector operations = %v, want %v", ops, wantOps)
	}
	for i := range wantOps {
		if ops[i] != wantOps[i] {
			t.Fatalf("vector operations = %v, want %v", ops, wantOps)
		}
	}
}

func TestRecoverPendingIndexTask(t *testing.T) {
	service, _ := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "恢复测试", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	doc := &store.RAGDocumentRecord{
		ID: "doc_recover", KBID: kb.ID, FileName: "recover.txt", FileType: "txt",
		FileSize: 12, ObjectKey: objects.Key("u1", kb.ID, "doc_recover", "recover.txt"),
		Status: "PENDING", Version: 1, UploadedAt: time.Now().UTC(),
	}
	if err := service.obj.Put(ctx, doc.ObjectKey, strings.NewReader("恢复任务正文"), doc.FileSize, "text/plain"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.BuildVersionSnapshot(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, snapshot, 3); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service.Start(workerCtx)
	waitDocumentStatus(t, service, doc.ID, "DONE")
}

func TestSearchOwnershipAndEmptyFormatting(t *testing.T) {
	service, _ := newTestService(t, true)
	kb, err := service.CreateKB(context.Background(), "u1", "a", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Search(context.Background(), "u2", []string{kb.ID}, "q", 5); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-user Search error = %v", err)
	}
	if got := FormatHits(nil); !strings.Contains(got, "未在授权") {
		t.Fatalf("empty result text = %q", got)
	}
}

func TestIndexTaskFailsAfterMaxRetries(t *testing.T) {
	service, _ := newTestService(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	kb, err := service.CreateKB(ctx, "u1", "失败重试", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	failingEmbedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "embedding unavailable", http.StatusServiceUnavailable)
	}))
	defer failingEmbedding.Close()
	service.cfg.Embedding.Endpoint = failingEmbedding.URL

	content := "这篇文档会在向量化阶段连续失败。"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "failure.txt", strings.NewReader(content), int64(len([]byte(content))))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := service.st.ListRunnableRAGIndexTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 {
		t.Fatalf("runnable tasks = %d, want 1", len(tasks))
	}

	// Execute attempts synchronously so the test covers the retry state machine
	// without waiting for the production 1/2/4-second backoff timers.
	for attempt := 0; attempt <= tasks[0].MaxRetry; attempt++ {
		service.runTask(ctx, tasks[0].ID)
		if db, ok := service.st.(*store.DBStore); ok {
			if _, err := db.DB().ExecContext(ctx,
				`UPDATE rag_index_tasks SET next_run_at='2000-01-01 00:00:00' WHERE id=? AND status='PENDING'`,
				tasks[0].ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	task, err := service.st.GetRAGIndexTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "FAILED" || task.RetryCount != task.MaxRetry {
		t.Fatalf("terminal task = %+v, want FAILED at max retry", task)
	}
	if task.ErrorMsg == "" || task.FinishedAt == nil {
		t.Fatalf("terminal task did not persist failure details: %+v", task)
	}

	failedDoc, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failedDoc.Status != "FAILED" || failedDoc.ErrorMsg == "" {
		t.Fatalf("failed document did not persist error: %+v", failedDoc)
	}
}

func TestResolveAgentKBsFiltersUnavailableReferences(t *testing.T) {
	service, _ := newTestService(t, false)
	ctx := context.Background()
	owned, err := service.CreateKB(ctx, "u1", "产品手册", "安装与使用", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := service.CreateKB(ctx, "u2", "其他用户", "不可访问", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	refs := service.ResolveAgentKBs(ctx, "u1", []string{owned.ID, foreign.ID, "kb_missing", owned.ID})
	if len(refs) != 1 {
		t.Fatalf("resolved refs = %+v, want only the owned KB", refs)
	}
	if refs[0].ID != owned.ID || refs[0].Name != owned.Name || refs[0].Description != owned.Description {
		t.Fatalf("resolved owned KB = %+v, want %+v", refs[0], owned)
	}
}

func TestSearchForAgentReturnsExplicitEmptyResult(t *testing.T) {
	service, _ := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "空知识库", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	got, err := service.SearchForAgent(ctx, "u1", []string{kb.ID}, "不存在的内容", 5)
	if err != nil {
		t.Fatalf("SearchForAgent returned error for empty results: %v", err)
	}
	const want = "未在授权的知识库中检索到相关内容。"
	if got != want {
		t.Fatalf("empty agent result = %q, want %q", got, want)
	}
}
