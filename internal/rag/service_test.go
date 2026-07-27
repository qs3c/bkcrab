package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	agenttools "github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/parse"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

type telemetryAwarePrimitiveExtractor struct {
	parse.PrimitiveExtractor
	recorder telemetry.Recorder
}

type blockingDeleteObjects struct {
	objects.Store
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type failFirstObjectDelete struct {
	objects.Store
	attempts atomic.Int32
}

type blockingLatePutObjects struct {
	objects.Store
	blockKey string
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
}

type blockingFirstDeleteObjects struct {
	objects.Store
	blockKey string
	started  chan struct{}
	release  chan struct{}
	attempts atomic.Int32
}

func (s *blockingLatePutObjects) Put(ctx context.Context, key string, reader io.Reader, size int64, contentType string) error {
	if key != s.blockKey {
		return s.Store.Put(ctx, key, reader, size, contentType)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	s.once.Do(func() { close(s.started) })
	<-s.release // deliberately ignores cancellation: models a late storage completion
	return s.Store.Put(context.Background(), key, bytes.NewReader(data), int64(len(data)), contentType)
}

func (s *blockingFirstDeleteObjects) Delete(ctx context.Context, key string) error {
	if key == s.blockKey && s.attempts.Add(1) == 1 {
		close(s.started)
		<-s.release // deliberately ignores cancellation: models a late delete acknowledgement
		return s.Store.Delete(context.Background(), key)
	}
	return s.Store.Delete(ctx, key)
}

func (s *failFirstObjectDelete) Delete(ctx context.Context, key string) error {
	if s.attempts.Add(1) == 1 {
		return errors.New("injected cache object delete failure")
	}
	return s.Store.Delete(ctx, key)
}

func (s *blockingDeleteObjects) Delete(ctx context.Context, key string) error {
	s.once.Do(func() { close(s.started) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
	}
	return s.Store.Delete(ctx, key)
}

func (p *telemetryAwarePrimitiveExtractor) SetRecorder(recorder telemetry.Recorder) {
	p.recorder = recorder
}

type policyRejectingStore struct {
	store.Store
	createErr  error
	advanceErr error
}

func (s *policyRejectingStore) CreateRAGDocumentWithVersionAndIndexTaskPolicy(
	ctx context.Context,
	doc *store.RAGDocumentRecord,
	version *store.RAGDocumentVersionRecord,
	maxRetry int,
	policy store.RAGAdvancedEnqueuePolicy,
) (int64, error) {
	if s.createErr != nil {
		return 0, s.createErr
	}
	return s.Store.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, version, maxRetry, policy)
}

func (s *policyRejectingStore) AdvanceDocumentVersionAndCreateTaskPolicy(
	ctx context.Context,
	expectedVersion int64,
	snapshot *store.RAGDocumentVersionRecord,
	policy store.RAGAdvancedEnqueuePolicy,
) (*store.RAGIndexTaskRecord, error) {
	if s.advanceErr != nil {
		return nil, s.advanceErr
	}
	return s.Store.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, expectedVersion, snapshot, policy)
}

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
	for _, userID := range []string{"u1", "u2", "u_pipeline"} {
		if err := db.CreateUser(context.Background(), &store.UserRecord{
			ID: userID, Username: userID, Email: userID + "@example.invalid",
			Role: "user", Status: "active",
		}); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

type reconcileRecordingStore struct {
	store.Store
	called chan struct{}
	once   sync.Once
	count  int
	err    error
}

func (s *reconcileRecordingStore) ReconcileRAGDocumentAIUsage(
	context.Context,
	time.Time,
	time.Time,
	int,
) (int, error) {
	s.once.Do(func() { close(s.called) })
	return s.count, s.err
}

func TestRAGDocumentAIReconcileLoopEmitsClosedTelemetryWithoutProviders(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		count         int
		err           error
		wantOutcome   string
		wantErrorCode string
	}{
		{name: "success", count: 3, wantOutcome: "ok"},
		{name: "store error", err: errors.New("database contains SECRET row text"), wantOutcome: "error", wantErrorCode: "store_error"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			recordingStore := &reconcileRecordingStore{
				called: make(chan struct{}), count: testCase.count, err: testCase.err,
			}
			events := &pipelineTelemetryRecorder{}
			service := New(Deps{Store: recordingStore, Telemetry: events})
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				service.documentAIReconcileLoop(ctx)
				close(done)
			}()

			select {
			case <-recordingStore.called:
			case <-done:
				t.Fatal("DocumentAI reconciler returned when providers were not configured")
			case <-time.After(time.Second):
				t.Fatal("DocumentAI reconciler did not call the durable store")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("DocumentAI reconciler did not stop after cancellation")
			}

			var found bool
			for _, event := range events.snapshot() {
				fields := event.Fields
				if event.Name != telemetry.EventDocumentAIBudget || fields.Transition != "reconcile" {
					continue
				}
				found = true
				if fields.Operation != "usage_reconcile" || fields.Outcome != testCase.wantOutcome ||
					fields.ErrorCode != testCase.wantErrorCode || fields.SuccessCount != testCase.count {
					t.Fatalf("reconcile telemetry=%+v", fields)
				}
			}
			if !found {
				t.Fatal("missing durable usage reconcile telemetry")
			}
		})
	}
}

func TestNewInjectsTelemetryIntoPrimitiveExtractor(t *testing.T) {
	events := &pipelineTelemetryRecorder{}
	primitives := &telemetryAwarePrimitiveExtractor{}
	service := New(Deps{Primitives: primitives, Telemetry: events})

	if service.primitives != primitives {
		t.Fatal("service did not retain the configured primitive extractor")
	}
	if primitives.recorder != events {
		t.Fatal("service telemetry was not injected into the primitive extractor")
	}
}

func TestIndexTaskPolicyRejectionsEmitStableTelemetry(t *testing.T) {
	embedding := newEmbeddingServer(t)
	backing := newRAGTestStore(t)
	rejecting := &policyRejectingStore{Store: backing}
	events := &pipelineTelemetryRecorder{}
	service := New(Deps{
		Store: rejecting, Vector: vector.NewFake(), Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
			DocumentAI: config.RAGDocumentAICfg{
				VisionModel: "vision-test", VisionPromptVersion: "vision-prompt-v1",
			},
		},
		Telemetry: events,
	})
	ctx := context.Background()
	kb, err := service.CreateKBWithOptions(ctx, "u1", "advanced telemetry", "", 0, 0, KBParsingOptions{
		ParseMode: config.ParseModeAuto,
	})
	if err != nil {
		t.Fatal(err)
	}
	seed := "seed advanced document"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "seed.txt", strings.NewReader(seed), int64(len(seed)))
	if err != nil {
		t.Fatal(err)
	}

	rejecting.createErr = fmt.Errorf("wrapped enqueue rejection: %w", store.ErrRAGAdvancedPendingLimit)
	queued := "second advanced document"
	if _, err := service.UploadDocument(ctx, "u1", kb.ID, "queued.txt", strings.NewReader(queued), int64(len(queued))); !errors.Is(err, store.ErrRAGAdvancedPendingLimit) {
		t.Fatalf("pending-limit upload error=%v", err)
	}
	rejecting.createErr = nil
	rejecting.advanceErr = fmt.Errorf("wrapped reindex rejection: %w", store.ErrRAGAdvancedReindexRateLimit)
	if err := service.ReindexDocument(ctx, "u1", kb.ID, doc.ID); !errors.Is(err, store.ErrRAGAdvancedReindexRateLimit) {
		t.Fatalf("reindex-rate-limit error=%v", err)
	}

	var rejected []telemetry.Event
	for _, event := range events.snapshot() {
		if event.Name == telemetry.EventIndexTask && event.Fields.Outcome == "rejected" {
			rejected = append(rejected, event)
		}
	}
	if len(rejected) != 2 {
		t.Fatalf("rejected index-task events=%+v want=2", rejected)
	}
	if fields := rejected[0].Fields; fields.Transition != "enqueue" || fields.ErrorCode != "pending_limit" ||
		fields.DocID == "" || fields.DocVersion != 1 || fields.TaskID != 0 {
		t.Fatalf("unexpected enqueue rejection telemetry: %+v", fields)
	}
	if fields := rejected[1].Fields; fields.Transition != "reindex" || fields.ErrorCode != "reindex_rate_limit" ||
		fields.DocID != doc.ID || fields.DocVersion != doc.Version || fields.TaskID != 0 {
		t.Fatalf("unexpected reindex rejection telemetry: %+v", fields)
	}
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

type failOnceDeleteVector struct {
	*vector.Fake
	mu            sync.Mutex
	failDocDelete bool
	failKBDelete  bool
}

type cancelAfterEnsureVector struct {
	*vector.Fake
	cancel       context.CancelFunc
	created      chan string
	dropAttempts atomic.Int32
}

func (v *cancelAfterEnsureVector) EnsureCollection(ctx context.Context, kbID string, dims int) error {
	if err := v.Fake.EnsureCollection(ctx, kbID, dims); err != nil {
		return err
	}
	v.created <- kbID
	v.cancel()
	<-ctx.Done()
	return ctx.Err()
}

func (v *cancelAfterEnsureVector) DropCollection(ctx context.Context, kbID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.dropAttempts.Add(1) == 1 {
		return errors.New("injected vector collection drop failure")
	}
	return v.Fake.DropCollection(ctx, kbID)
}

type blockingProvisionVector struct {
	*vector.Fake
	started chan string
}

func (v *blockingProvisionVector) EnsureCollection(ctx context.Context, kbID string, dims int) error {
	if err := v.Fake.EnsureCollection(ctx, kbID, dims); err != nil {
		return err
	}
	v.started <- kbID
	<-ctx.Done()
	return ctx.Err()
}

func (v *failOnceDeleteVector) DeleteDoc(ctx context.Context, kbID, docID string) error {
	v.mu.Lock()
	fail := v.failDocDelete
	v.failDocDelete = false
	v.mu.Unlock()
	if fail {
		return errors.New("transient vector document delete")
	}
	return v.Fake.DeleteDoc(ctx, kbID, docID)
}

func (v *failOnceDeleteVector) DropCollection(ctx context.Context, kbID string) error {
	v.mu.Lock()
	fail := v.failKBDelete
	v.failKBDelete = false
	v.mu.Unlock()
	if fail {
		return errors.New("transient vector collection delete")
	}
	return v.Fake.DropCollection(ctx, kbID)
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

func TestCreateKBCancellationRetainsDurableCleanupHandleAfterDropFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	vec := &cancelAfterEnsureVector{
		Fake: vector.NewFake(), cancel: cancel, created: make(chan string, 1),
	}
	service := New(Deps{
		Store: newRAGTestStore(t), Vector: vec, Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus: config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{
				Endpoint: "http://embedding.invalid", Model: "embed-test", Dims: 4,
			},
		},
		Workers: 1,
	})
	_, err := service.CreateKB(ctx, "u1", "cancelled provision", "", 0, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateKB error=%v, want context cancellation", err)
	}
	kbID := <-vec.created
	marked, err := service.st.GetRAGKB(context.Background(), kbID)
	if err != nil || marked.Status != store.RAGKBStatusDeleting {
		t.Fatalf("durable cleanup handle=%+v err=%v", marked, err)
	}
	if !vec.HasCollection(kbID) || vec.dropAttempts.Load() != 1 {
		t.Fatalf("failed first cleanup collection=%v attempts=%d",
			vec.HasCollection(kbID), vec.dropAttempts.Load())
	}

	service.runLifecyclePass(context.Background())
	if _, err := service.st.GetRAGKB(context.Background(), kbID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("retry did not finalize KB row: %v", err)
	}
	if vec.HasCollection(kbID) || vec.dropAttempts.Load() != 2 {
		t.Fatalf("retry cleanup collection=%v attempts=%d",
			vec.HasCollection(kbID), vec.dropAttempts.Load())
	}
}

func TestCreateKBUserTombstoneCancelsInFlightProvisioning(t *testing.T) {
	vec := &blockingProvisionVector{Fake: vector.NewFake(), started: make(chan string, 1)}
	st := newRAGTestStore(t)
	service := New(Deps{
		Store: st, Vector: vec, Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus: config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{
				Endpoint: "http://embedding.invalid", Model: "embed-test", Dims: 4,
			},
		},
		Workers: 1,
	})
	service.leaseDuration = 150 * time.Millisecond
	service.heartbeatInterval = 10 * time.Millisecond
	createResult := make(chan error, 1)
	go func() {
		_, err := service.CreateKB(context.Background(), "u1", "delete race", "", 0, 0)
		createResult <- err
	}()

	var kbID string
	select {
	case kbID = <-vec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("collection provisioning did not start")
	}
	if _, err := st.MarkUserDeleting(context.Background(), "u1"); err != nil {
		t.Fatal(err)
	}
	cleanupResult := make(chan error, 1)
	go func() { cleanupResult <- service.CleanupRAGUser(context.Background(), "u1") }()

	select {
	case err := <-createResult:
		if err == nil {
			t.Fatal("tombstoned provisioning unexpectedly activated")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("provisioning did not stop after tombstone")
	}
	select {
	case err := <-cleanupResult:
		if err != nil {
			t.Fatalf("user cleanup: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("user cleanup remained blocked after provisioning quiesced")
	}
	if _, err := st.GetRAGKB(context.Background(), kbID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("KB row survived cleanup: %v", err)
	}
	if vec.HasCollection(kbID) {
		t.Fatal("collection survived user tombstone cleanup")
	}
}

func TestLifecycleRecoversCrashedKBProvisioning(t *testing.T) {
	service, vec := newTestService(t, false)
	service.pollInterval = 10 * time.Millisecond
	kb := &store.RAGKBRecord{
		ID: "kb_crashed_provision", UserID: "u1", Name: "crashed provision",
		EmbedProvider: "system", EmbedModel: "embed-test", EmbedDims: 4,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: store.RAGParseModeStandard,
	}
	fence, err := service.st.BeginRAGKBProvisioning(
		context.Background(), kb, service.workerID+"-kb", time.Minute, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := vec.EnsureCollection(context.Background(), kb.ID, kb.EmbedDims); err != nil {
		t.Fatal(err)
	}
	if _, err := service.st.(*store.DBStore).DB().ExecContext(context.Background(),
		`UPDATE rag_kbs SET provisioning_lease_until=? WHERE id=?`,
		time.Now().UTC().Add(-time.Minute), fence.KBID); err != nil {
		t.Fatal(err)
	}

	service.runLifecyclePass(context.Background())
	if _, err := service.st.GetRAGKB(context.Background(), kb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("crashed provisioning row was not finalized: %v", err)
	}
	if vec.HasCollection(kb.ID) {
		t.Fatal("crashed provisioning collection was not dropped")
	}
}

func TestRAGDeletingUserCannotCreateOrMutateKnowledgeBases(t *testing.T) {
	service, _ := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "before delete", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "queued before account tombstone"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "queued.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	user, err := service.st.GetUser(ctx, "u1")
	if err != nil {
		t.Fatal(err)
	}
	user.Status = "deleting"
	if err := service.st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}

	if _, err := service.CreateKB(ctx, "u1", "after delete", "", 0, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("CreateKB for deleting user error=%v", err)
	}
	if _, err := service.UploadDocument(ctx, "u1", kb.ID, "late.txt", strings.NewReader("late"), 4); !errors.Is(err, ErrForbidden) {
		t.Fatalf("UploadDocument for deleting user error=%v", err)
	}
	if err := service.ReindexDocument(ctx, "u1", kb.ID, doc.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("ReindexDocument for deleting user error=%v", err)
	}
	if _, err := service.UpdateKB(ctx, "u1", kb.ID, "late", "", 0, 0); !errors.Is(err, ErrForbidden) {
		t.Fatalf("UpdateKB for deleting user error=%v", err)
	}
}

func TestUploadRecognizesButRejectsOfficeUntilConverterGate(t *testing.T) {
	service, _ := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "Office gate", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fileName := range []string{"guide.docx", "slides.PPTX", "table.xlsx"} {
		if _, err := service.UploadDocument(
			ctx, "u1", kb.ID, fileName, strings.NewReader("not uploaded"), 12,
		); err == nil || !strings.Contains(err.Error(), "能力当前不可用") {
			t.Fatalf("UploadDocument(%q) error=%v", fileName, err)
		}
	}
	documents, err := service.ListDocuments(ctx, "u1", kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 0 {
		t.Fatalf("rejected Office uploads created records: %+v", documents)
	}

	service.officeAvailable = func() bool { return true }
	accepted, err := service.UploadDocument(
		ctx, "u1", kb.ID, "guide.docx", strings.NewReader("office bytes"), 12,
	)
	if err != nil {
		t.Fatalf("golden-gated Office upload was rejected: %v", err)
	}
	if accepted.FileType != "docx" {
		t.Fatalf("accepted Office type=%q", accepted.FileType)
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
	tombstone, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(tombstone.Status, "deleting") || tombstone.ActiveVersion != 0 {
		t.Fatalf("delete did not revoke the document before external cleanup: %+v", tombstone)
	}
	revokedTask, err := service.st.GetRAGIndexTask(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if revokedTask.Status != "SUPERSEDED" || revokedTask.LeaseOwner == "" || revokedTask.LeaseUntil == nil {
		t.Fatalf("delete did not revoke the running claim: %+v", revokedTask)
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

func TestDeleteDocumentAcrossServicesWaitsForDurableWorkerQuiescence(t *testing.T) {
	gate := newBlockingEmbeddingServer(t)
	st := newRAGTestStore(t)
	vec := vector.NewFake()
	obj := objects.NewLocalFS(t.TempDir())
	cfg := config.RAGCfg{
		Milvus:    config.MilvusCfg{Address: "fake"},
		Embedding: config.RAGEmbeddingCfg{Endpoint: gate.server.URL, Model: "embed-test", Dims: 4},
	}
	indexer := New(Deps{Store: st, Vector: vec, Objects: obj, Cfg: cfg, Workers: 1})
	deleter := New(Deps{Store: st, Vector: vec, Objects: obj, Cfg: cfg, Workers: 1})
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	indexer.Start(workerCtx)

	kb, err := indexer.CreateKB(context.Background(), "u1", "cross service delete", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "a remote worker is blocked while another service tombstones the document"
	doc, err := indexer.UploadDocument(context.Background(), "u1", kb.ID, "remote.txt",
		strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	gate.waitForCall(t, 1)

	err = deleter.DeleteDocument(context.Background(), "u1", kb.ID, doc.ID)
	if !errors.Is(err, ErrLifecycleCleanupPending) {
		t.Fatalf("cross-service delete before worker quiescence error=%v", err)
	}
	tombstone, err := st.GetRAGDocument(context.Background(), doc.ID)
	if err != nil || !strings.EqualFold(tombstone.Status, store.RAGDocumentStatusDeleting) {
		t.Fatalf("durable tombstone=%+v err=%v", tombstone, err)
	}
	runnable, err := st.ListRunnableRAGIndexTasks(context.Background())
	if err != nil || len(runnable) != 0 {
		t.Fatalf("tombstoned task remained runnable: %+v err=%v", runnable, err)
	}

	gate.releaseCall(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		task, taskErr := st.GetRAGIndexTask(context.Background(), 1)
		if taskErr == nil && task.Status == "SUPERSEDED" && task.LeaseUntil == nil && task.LeaseOwner == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote worker did not acknowledge quiescence: task=%+v err=%v", task, taskErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := deleter.DeleteDocument(context.Background(), "u1", kb.ID, doc.ID); err != nil {
		t.Fatalf("retry delete after quiescence: %v", err)
	}
	if _, err := st.GetRAGDocument(context.Background(), doc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("document survived quiescent cleanup: %v", err)
	}
}

func TestRAGDocumentDeleteRetainsTombstoneUntilCleanupRetrySucceeds(t *testing.T) {
	embedding := newEmbeddingServer(t)
	vec := &failOnceDeleteVector{Fake: vector.NewFake(), failDocDelete: true}
	service := New(Deps{
		Store: newRAGTestStore(t), Vector: vec, Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
		Workers: 1,
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "durable delete", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "durable deletion must revoke before cleanup"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "delete.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	service.runTask(ctx, 0)
	indexed, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil || indexed.Status != "DONE" || indexed.ActiveVersion != 1 {
		t.Fatalf("seed index failed: doc=%+v err=%v", indexed, err)
	}

	if err := service.DeleteDocument(ctx, "u1", kb.ID, doc.ID); !errors.Is(err, ErrLifecycleCleanupPending) {
		t.Fatalf("first cleanup error=%v", err)
	} else if strings.Contains(err.Error(), "transient") || strings.Contains(err.Error(), doc.ID) {
		t.Fatalf("cleanup response leaked backend details: %q", err.Error())
	}
	tombstone, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(tombstone.Status, "deleting") || tombstone.ActiveVersion != 0 {
		t.Fatalf("cleanup failure lost durable tombstone: %+v", tombstone)
	}
	if vec.Count(kb.ID) == 0 {
		t.Fatal("failure injection did not leave the external vector for retry")
	}

	if err := service.DeleteDocument(ctx, "u1", kb.ID, doc.ID); err != nil {
		t.Fatalf("idempotent retry failed: %v", err)
	}
	if _, err := service.st.GetRAGDocument(ctx, doc.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("document row remained after successful retry: %v", err)
	}
	if count := vec.Count(kb.ID); count != 0 {
		t.Fatalf("vectors remained after successful retry: %d", count)
	}
}

func TestRAGKBDeleteTombstonesChildrenUntilCleanupRetrySucceeds(t *testing.T) {
	embedding := newEmbeddingServer(t)
	vec := &failOnceDeleteVector{Fake: vector.NewFake(), failKBDelete: true}
	service := New(Deps{
		Store: newRAGTestStore(t), Vector: vec, Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
		Workers: 1,
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "durable kb delete", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "child document"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "child.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}

	if err := service.DeleteKB(ctx, "u1", kb.ID); !errors.Is(err, ErrLifecycleCleanupPending) {
		t.Fatalf("first KB cleanup error=%v", err)
	} else if strings.Contains(err.Error(), "transient") || strings.Contains(err.Error(), kb.ID) {
		t.Fatalf("KB cleanup response leaked backend details: %q", err.Error())
	}
	markedKB, err := service.st.GetRAGKB(ctx, kb.ID)
	if err != nil || !strings.EqualFold(markedKB.Status, "deleting") {
		t.Fatalf("KB tombstone was not retained: kb=%+v err=%v", markedKB, err)
	}
	markedDoc, err := service.st.GetRAGDocument(ctx, doc.ID)
	if err != nil || !strings.EqualFold(markedDoc.Status, "deleting") || markedDoc.ActiveVersion != 0 {
		t.Fatalf("child document was not revoked transactionally: doc=%+v err=%v", markedDoc, err)
	}

	if err := service.DeleteKB(ctx, "u1", kb.ID); err != nil {
		t.Fatalf("KB cleanup retry failed: %v", err)
	}
	if _, err := service.st.GetRAGKB(ctx, kb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("KB row remained after successful retry: %v", err)
	}
}

func TestRAGExactVersionGCAndSweepRemoveLateWritesWithoutTouchingNewerGrace(t *testing.T) {
	embedding := newEmbeddingServer(t)
	tracked := newVersionTrackingVector()
	events := &pipelineTelemetryRecorder{}
	service := New(Deps{
		Store: newRAGTestStore(t), Vector: tracked, Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
		Telemetry: events, Workers: 1,
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "exact gc", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "same source is reindexed through three physical versions"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "gc.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	service.runTask(ctx, 0)
	for wantVersion := int64(2); wantVersion <= 3; wantVersion++ {
		if err := service.ReindexDocument(ctx, "u1", kb.ID, doc.ID); err != nil {
			t.Fatal(err)
		}
		service.runTask(ctx, 0)
		current, err := service.st.GetRAGDocument(ctx, doc.ID)
		if err != nil || current.ActiveVersion != wantVersion {
			t.Fatalf("active version=%v want=%d err=%v", current, wantVersion, err)
		}
	}
	if got := tracked.documentVersions(kb.ID, doc.ID); fmt.Sprint(got) != "[1 2 3]" {
		t.Fatalf("seed vector versions=%v", got)
	}

	db := service.st.(*store.DBStore)
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE rag_index_gc_tasks SET not_before=?,next_run_at=NULL WHERE doc_id=? AND retired_version=?`,
		time.Now().UTC().Add(-time.Minute), doc.ID, int64(1)); err != nil {
		t.Fatal(err)
	}
	service.runAvailableGCTasks(ctx)
	if got := tracked.documentVersions(kb.ID, doc.ID); fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("exact v1 GC changed newer vector versions: %v", got)
	}
	if chunks, err := service.st.ListRAGChunksByDocumentVersion(ctx, doc.ID, 1); err != nil || len(chunks) != 0 {
		t.Fatalf("v1 SQL chunks remained: chunks=%+v err=%v", chunks, err)
	}
	if chunks, err := service.st.ListRAGChunksByDocumentVersion(ctx, doc.ID, 2); err != nil || len(chunks) == 0 {
		t.Fatalf("v2 SQL chunks were deleted before their own grace: chunks=%+v err=%v", chunks, err)
	}
	v1, err := service.st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || v1.Status != store.RAGDocumentVersionGCED {
		t.Fatalf("v1 GC tombstone=%+v err=%v", v1, err)
	}
	var gcClaim, gcFinish bool
	for _, event := range events.snapshot() {
		if event.Name != telemetry.EventLifecycleGC || event.Fields.RetiredVersion != 1 {
			continue
		}
		gcClaim = gcClaim || event.Fields.Transition == "claim" && event.Fields.Outcome == "ok"
		gcFinish = gcFinish || event.Fields.Transition == "finish" && event.Fields.Outcome == "ok"
	}
	if !gcClaim || !gcFinish {
		t.Fatalf("exact-version GC transitions were not observable: claim=%v finish=%v events=%+v",
			gcClaim, gcFinish, events.snapshot())
	}

	if err := tracked.UpsertChunks(ctx, kb.ID, []vector.ChunkData{{
		DocID: doc.ID, Index: 99, DocVersion: 1, Content: "late", SearchContent: "late",
		Vector: []float32{1, 0, 0, 0},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := service.st.PutRAGChunks(ctx, []store.RAGChunkRecord{{
		KBID: kb.ID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 99,
		RawContent: "late", SearchContent: "late", TokenCount: 1, CreatedAt: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE rag_document_versions SET updated_at=? WHERE doc_id=? AND doc_version=?`,
		time.Now().UTC().Add(-48*time.Hour), doc.ID, int64(1)); err != nil {
		t.Fatal(err)
	}
	service.stagingArtifactTTL = time.Hour
	service.sweepOrphanVersions(ctx)
	if got := tracked.documentVersions(kb.ID, doc.ID); fmt.Sprint(got) != "[2 3]" {
		t.Fatalf("late GCED vector survived periodic exact sweep: %v", got)
	}
	if chunks, err := service.st.ListRAGChunksByDocumentVersion(ctx, doc.ID, 1); err != nil || len(chunks) != 0 {
		t.Fatalf("late GCED SQL chunk survived: chunks=%+v err=%v", chunks, err)
	}
	if _, err := service.st.GetRAGDocumentVersion(ctx, doc.ID, 1); err != nil {
		t.Fatalf("GCED tombstone was removed instead of retained: %v", err)
	}
}

func TestRAGStagingAssetSweepDeletesOnlyUnreferencedExpiredObjects(t *testing.T) {
	embedding := newEmbeddingServer(t)
	objectStore := objects.NewLocalFS(t.TempDir())
	service := New(Deps{
		Store: newRAGTestStore(t), Vector: vector.NewFake(), Objects: objectStore,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "staging assets", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "failed staging parse"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "assets.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	db := service.st.(*store.DBStore)
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE rag_document_versions SET status='FAILED',updated_at=? WHERE doc_id=? AND doc_version=1`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx,
		`UPDATE rag_index_tasks SET status='FAILED',finished_at=? WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}

	makeAsset := func(hash, id string) *store.RAGAssetRecord {
		t.Helper()
		source, err := document.AssetSourceKey("u1", kb.ID, doc.ID, hash, "image/png")
		if err != nil {
			t.Fatal(err)
		}
		display, err := document.AssetDisplayKey("u1", kb.ID, doc.ID, hash)
		if err != nil {
			t.Fatal(err)
		}
		thumbnail, err := document.AssetThumbnailKey("u1", kb.ID, doc.ID, hash)
		if err != nil {
			t.Fatal(err)
		}
		asset := &store.RAGAssetRecord{
			ID: id, DocID: doc.ID, ContentSHA256: hash, SourceKind: "embedded",
			SourceMIME: "image/png", DisplayMIME: "image/webp",
			SourceObjectKey: source, DisplayObjectKey: display, ThumbnailObjectKey: thumbnail,
			DisplayStatus: "ready", DisplaySHA256: hash, ThumbnailSHA256: hash,
			ByteSize: 3, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
			CreatedAt: old, UpdatedAt: old,
		}
		if err := service.st.UpsertRAGAsset(ctx, asset); err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().ExecContext(ctx,
			`UPDATE rag_assets SET created_at=?,updated_at=? WHERE id=?`, old, old, asset.ID); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{source, display, thumbnail} {
			if err := objectStore.Put(ctx, key, strings.NewReader("img"), 3, "image/png"); err != nil {
				t.Fatal(err)
			}
		}
		return asset
	}
	orphan := makeAsset(strings.Repeat("a", 64), "ast_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	pinned := makeAsset(strings.Repeat("b", 64), "ast_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err := service.st.AppendRAGChatTurn(ctx, &store.RAGChatTurnRecord{
		ID: "turn_pins_asset", UserID: "u1", KBID: kb.ID, SessionID: "s1",
		Question: "q", Answer: "a", Sources: json.RawMessage(`[{"assets":[{"id":"` + pinned.ID + `"}]}]`),
	}); err != nil {
		t.Fatal(err)
	}

	service.stagingArtifactTTL = time.Hour
	service.sweepStagingAssets(ctx)
	if _, err := service.st.GetRAGAsset(ctx, orphan.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unreferenced staging asset catalog survived: %v", err)
	}
	for _, key := range []string{orphan.SourceObjectKey, orphan.DisplayObjectKey, orphan.ThumbnailObjectKey} {
		if reader, err := objectStore.Get(ctx, key); err == nil {
			_ = reader.Close()
			t.Fatalf("unreferenced staging object survived: %s", path.Base(key))
		}
	}
	if _, err := service.st.GetRAGAsset(ctx, pinned.ID); err != nil {
		t.Fatalf("history-pinned staging asset was deleted: %v", err)
	}
	if reader, err := objectStore.Get(ctx, pinned.ThumbnailObjectKey); err != nil {
		t.Fatalf("history-pinned object was deleted: %v", err)
	} else {
		_ = reader.Close()
	}
}

func TestRAGStagingAssetSweepLeaseBlocksSecondServiceReindex(t *testing.T) {
	embedding := newEmbeddingServer(t)
	firstStore := newRAGTestStore(t)
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	secondStore, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondStore.Close() })
	localObjects := objects.NewLocalFS(t.TempDir())
	blockingObjects := &blockingDeleteObjects{
		Store: localObjects, started: make(chan struct{}), release: make(chan struct{}),
	}
	cfg := config.RAGCfg{
		Milvus:    config.MilvusCfg{Address: "fake"},
		Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
	}
	firstService := New(Deps{Store: firstStore, Vector: vector.NewFake(), Objects: blockingObjects, Cfg: cfg})
	secondService := New(Deps{Store: secondStore, Vector: vector.NewFake(), Objects: blockingObjects, Cfg: cfg})
	firstService.leaseDuration = 1200 * time.Millisecond
	firstService.heartbeatInterval = 100 * time.Millisecond
	ctx := context.Background()
	kb, err := firstService.CreateKB(ctx, "u1", "maintenance race", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "failed staging parse"
	doc, err := firstService.UploadDocument(ctx, "u1", kb.ID, "race.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := firstStore.DB().ExecContext(ctx, `UPDATE rag_document_versions
		SET status='FAILED',updated_at=? WHERE doc_id=? AND doc_version=1`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := firstStore.DB().ExecContext(ctx, `UPDATE rag_index_tasks
		SET status='FAILED',finished_at=? WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("c", 64)
	source, err := document.AssetSourceKey("u1", kb.ID, doc.ID, hash, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	display, err := document.AssetDisplayKey("u1", kb.ID, doc.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	thumbnail, err := document.AssetThumbnailKey("u1", kb.ID, doc.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	asset := &store.RAGAssetRecord{
		ID: "ast_cccccccccccccccccccccccccccccccc", DocID: doc.ID, ContentSHA256: hash,
		SourceKind: "embedded_original", SourceMIME: "image/png", DisplayMIME: "image/webp",
		SourceObjectKey: source, DisplayObjectKey: display, ThumbnailObjectKey: thumbnail,
		DisplayStatus: "ready", DisplaySHA256: hash, ThumbnailSHA256: hash,
		ByteSize: 3, Width: 1, Height: 1, FirstSeenVersion: 1, LastSeenVersion: 1,
	}
	if err := firstStore.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	if _, err := firstStore.DB().ExecContext(ctx, `UPDATE rag_assets
		SET created_at=?,updated_at=? WHERE id=?`, old, old, asset.ID); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{source, display, thumbnail} {
		if err := blockingObjects.Put(ctx, key, strings.NewReader("img"), 3, "image/png"); err != nil {
			t.Fatal(err)
		}
	}
	firstService.stagingArtifactTTL = time.Hour
	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		firstService.sweepStagingAssets(ctx)
	}()
	select {
	case <-blockingObjects.started:
	case <-time.After(5 * time.Second):
		t.Fatal("staging sweep did not reach fenced object deletion")
	}
	// Stay blocked beyond the original lease deadline. The maintenance
	// heartbeat must keep the durable fence live while object deletion is in
	// flight on another service instance.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-sweepDone:
		t.Fatal("staging sweep returned while object deletion was blocked")
	}
	reindexErr := secondService.ReindexDocument(ctx, "u1", kb.ID, doc.ID)
	close(blockingObjects.release)
	select {
	case <-sweepDone:
	case <-time.After(5 * time.Second):
		t.Fatal("staging sweep did not finish")
	}
	if !errors.Is(reindexErr, store.ErrRAGDocumentMaintenanceActive) {
		t.Fatalf("second service reindex during heartbeated maintenance err=%v", reindexErr)
	}
	if _, err := firstStore.GetRAGAsset(ctx, asset.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("fenced staging asset survived: %v", err)
	}
}

func TestRAGCacheSweepRetriesExpiredObjectAndCatalogCAS(t *testing.T) {
	embedding := newEmbeddingServer(t)
	localObjects := objects.NewLocalFS(t.TempDir())
	failingObjects := &failFirstObjectDelete{Store: localObjects}
	db := newRAGTestStore(t)
	service := New(Deps{
		Store: db, Vector: vector.NewFake(), Objects: failingObjects,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "cache cleanup", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "cache cleanup source"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "cache.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	cacheKey := strings.Repeat("a", 64)
	objectKey, err := document.PageCacheObjectKey("u1", kb.ID, doc.ID, cacheKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterRAGCacheObject(ctx, store.RAGCacheObjectRecord{
		DocID: doc.ID, CacheKind: store.RAGCacheKindPage, CacheKey: cacheKey,
		ObjectKey: objectKey, FingerprintKind: store.RAGCacheFingerprintParse,
		Fingerprint: strings.Repeat("b", 64),
	}); err != nil {
		t.Fatal(err)
	}
	if err := localObjects.Put(ctx, objectKey, strings.NewReader(`{"cached":true}`), -1, "application/json"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_cache_objects SET updated_at=? WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_cache_object_fingerprints SET updated_at=? WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_document_versions SET status='FAILED',updated_at=?
		WHERE doc_id=? AND doc_version=1`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_index_tasks SET status='FAILED',finished_at=?
		WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	service.stagingArtifactTTL = time.Hour
	service.sweepCacheObjects(ctx)
	var catalogRows int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_cache_objects WHERE doc_id=?`, doc.ID).
		Scan(&catalogRows); err != nil || catalogRows != 1 {
		t.Fatalf("catalog removed after failed external delete rows=%d err=%v", catalogRows, err)
	}
	if reader, err := localObjects.Get(ctx, objectKey); err != nil {
		t.Fatalf("object removed after injected delete failure: %v", err)
	} else {
		_ = reader.Close()
	}

	service.sweepCacheObjects(ctx)
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_cache_objects WHERE doc_id=?`, doc.ID).
		Scan(&catalogRows); err != nil || catalogRows != 0 {
		t.Fatalf("catalog survived successful retry rows=%d err=%v", catalogRows, err)
	}
	if reader, err := localObjects.Get(ctx, objectKey); err == nil {
		_ = reader.Close()
		t.Fatal("cache object survived successful retry")
	}
	if failingObjects.attempts.Load() != 2 {
		t.Fatalf("delete attempts=%d, want 2", failingObjects.attempts.Load())
	}
}

func TestRAGObjectWriteSweepRetriesDurableUnreferencedPut(t *testing.T) {
	embedding := newEmbeddingServer(t)
	localObjects := objects.NewLocalFS(t.TempDir())
	failingObjects := &failFirstObjectDelete{Store: localObjects}
	db := newRAGTestStore(t)
	service := New(Deps{
		Store: db, Vector: vector.NewFake(), Objects: failingObjects,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "object staging cleanup", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "source with an abandoned derived put"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "staging.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	objectKey := fmt.Sprintf("rag/u1/%s/%s/staging/abandoned.bin", kb.ID, doc.ID)
	fence, err := db.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: "u1", KBID: kb.ID, DocID: doc.ID, ObjectKind: store.RAGObjectKindAssetSource,
		ObjectKey: objectKey, ReferenceKey: "abandoned",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := localObjects.Put(ctx, objectKey, strings.NewReader("orphan"), -1, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if ready, err := db.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
		t.Fatalf("ready=%v err=%v", ready, err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, old, fence.HandleID); err != nil {
		t.Fatal(err)
	}
	service.stagingArtifactTTL = time.Hour
	service.sweepObjectWriteStaging(ctx)
	var rows int
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_object_write_staging
		WHERE handle_id=?`, fence.HandleID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("failed delete lost durable handle rows=%d err=%v", rows, err)
	}
	service.sweepObjectWriteStaging(ctx)
	if failingObjects.attempts.Load() != 1 {
		t.Fatalf("in-flight cleanup reclaimed immediately attempts=%d", failingObjects.attempts.Load())
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, time.Now().UTC().Add(-time.Minute), fence.HandleID); err != nil {
		t.Fatal(err)
	}
	service.sweepObjectWriteStaging(ctx)
	if err := db.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_object_write_staging
		WHERE handle_id=? AND status='DELETING'`, fence.HandleID).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("successful retry lost durable tombstone rows=%d err=%v", rows, err)
	}
	if reader, err := localObjects.Get(ctx, objectKey); err == nil {
		_ = reader.Close()
		t.Fatal("successful retry retained abandoned object")
	}
	if failingObjects.attempts.Load() != 2 {
		t.Fatalf("delete attempts=%d, want 2", failingObjects.attempts.Load())
	}
}

func TestRAGObjectWriteLatePutCannotOverwriteNewGeneration(t *testing.T) {
	embedding := newEmbeddingServer(t)
	localObjects := objects.NewLocalFS(t.TempDir())
	db := newRAGTestStore(t)
	service := New(Deps{
		Store: db, Vector: vector.NewFake(), Objects: localObjects,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "late put generations", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "late put source"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "late-put.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("d", 64)
	logicalKey, err := document.AssetSourceKey("u1", kb.ID, doc.ID, hash, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	versionOneKey, err := document.VersionedObjectKey(logicalKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	versionTwoKey, err := document.VersionedObjectKey(logicalKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	versionOneFence, err := db.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: "u1", KBID: kb.ID, DocID: doc.ID, ObjectKind: store.RAGObjectKindAssetSource,
		ObjectKey: versionOneKey, ReferenceKey: "ast_late_put_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	blockingObjects := &blockingLatePutObjects{
		Store: localObjects, blockKey: versionOneKey,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	service.obj = blockingObjects
	putDone := make(chan error, 1)
	go func() {
		putDone <- service.obj.Put(ctx, versionOneKey, strings.NewReader("old"), 3, "image/png")
	}()
	select {
	case <-blockingObjects.started:
	case <-time.After(5 * time.Second):
		t.Fatal("version 1 Put did not reach the injected late completion")
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, time.Now().UTC().Add(-48*time.Hour), versionOneFence.HandleID); err != nil {
		t.Fatal(err)
	}
	service.stagingArtifactTTL = time.Hour
	service.sweepObjectWriteStaging(ctx)

	versionTwoFence, err := db.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: "u1", KBID: kb.ID, DocID: doc.ID, ObjectKind: store.RAGObjectKindAssetSource,
		ObjectKey: versionTwoKey, ReferenceKey: "ast_late_put_v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.obj.Put(ctx, versionTwoKey, strings.NewReader("new"), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if ready, err := db.MarkRAGObjectWriteReady(ctx, *versionTwoFence); err != nil || !ready {
		t.Fatalf("version 2 ready=%v err=%v", ready, err)
	}
	assetID, err := document.AssetID(doc.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRAGAsset(ctx, &store.RAGAssetRecord{
		ID: assetID, DocID: doc.ID, ContentSHA256: hash,
		SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: "image/png",
		SourceObjectKey: versionTwoKey, DisplayStatus: document.DisplayUnavailable,
		ByteSize: 3, Width: 1, Height: 1, FirstSeenVersion: 2, LastSeenVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}

	close(blockingObjects.release)
	if err := <-putDone; err != nil {
		t.Fatal(err)
	}
	if ready, err := db.MarkRAGObjectWriteReady(ctx, *versionOneFence); err != nil || ready {
		t.Fatalf("late version 1 publication ready=%v err=%v", ready, err)
	}
	reader, err := localObjects.Get(ctx, versionTwoKey)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != "new" {
		t.Fatalf("version 2 object=%q err=%v", got, err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, time.Now().UTC().Add(-time.Minute), versionOneFence.HandleID); err != nil {
		t.Fatal(err)
	}
	service.sweepObjectWriteStaging(ctx)
	if reader, err := localObjects.Get(ctx, versionOneKey); err == nil {
		_ = reader.Close()
		t.Fatal("late version 1 object survived durable tombstone re-sweep")
	}
	if reader, err := localObjects.Get(ctx, versionTwoKey); err != nil {
		t.Fatalf("version 1 cleanup deleted version 2 object: %v", err)
	} else {
		_ = reader.Close()
	}
}

func TestRAGObjectWriteLateDeleteCannotDeleteNewGeneration(t *testing.T) {
	embedding := newEmbeddingServer(t)
	localObjects := objects.NewLocalFS(t.TempDir())
	db := newRAGTestStore(t)
	service := New(Deps{
		Store: db, Vector: vector.NewFake(), Objects: localObjects,
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "late delete generations", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "late delete source"
	doc, err := service.UploadDocument(ctx, "u1", kb.ID, "late-delete.txt", strings.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("e", 64)
	logicalKey, err := document.AssetSourceKey("u1", kb.ID, doc.ID, hash, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	versionOneKey, err := document.VersionedObjectKey(logicalKey, 1)
	if err != nil {
		t.Fatal(err)
	}
	versionTwoKey, err := document.VersionedObjectKey(logicalKey, 2)
	if err != nil {
		t.Fatal(err)
	}
	versionOneFence, err := db.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: "u1", KBID: kb.ID, DocID: doc.ID, ObjectKind: store.RAGObjectKindAssetSource,
		ObjectKey: versionOneKey, ReferenceKey: "ast_late_delete_v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := localObjects.Put(ctx, versionOneKey, strings.NewReader("old"), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if ready, err := db.MarkRAGObjectWriteReady(ctx, *versionOneFence); err != nil || !ready {
		t.Fatalf("version 1 ready=%v err=%v", ready, err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, time.Now().UTC().Add(-48*time.Hour), versionOneFence.HandleID); err != nil {
		t.Fatal(err)
	}
	blockingObjects := &blockingFirstDeleteObjects{
		Store: localObjects, blockKey: versionOneKey,
		started: make(chan struct{}), release: make(chan struct{}),
	}
	service.obj = blockingObjects
	service.stagingArtifactTTL = time.Hour
	firstSweepDone := make(chan struct{})
	go func() {
		defer close(firstSweepDone)
		service.sweepObjectWriteStaging(ctx)
	}()
	select {
	case <-blockingObjects.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first version 1 Delete did not reach the injected late completion")
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE rag_object_write_staging SET updated_at=?
		WHERE handle_id=?`, time.Now().UTC().Add(-time.Minute), versionOneFence.HandleID); err != nil {
		t.Fatal(err)
	}
	service.sweepObjectWriteStaging(ctx)

	versionTwoFence, err := db.BeginRAGObjectWrite(ctx, store.RAGObjectWriteRequest{
		UserID: "u1", KBID: kb.ID, DocID: doc.ID, ObjectKind: store.RAGObjectKindAssetSource,
		ObjectKey: versionTwoKey, ReferenceKey: "ast_late_delete_v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.obj.Put(ctx, versionTwoKey, strings.NewReader("new"), 3, "image/png"); err != nil {
		t.Fatal(err)
	}
	if ready, err := db.MarkRAGObjectWriteReady(ctx, *versionTwoFence); err != nil || !ready {
		t.Fatalf("version 2 ready=%v err=%v", ready, err)
	}
	assetID, err := document.AssetID(doc.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertRAGAsset(ctx, &store.RAGAssetRecord{
		ID: assetID, DocID: doc.ID, ContentSHA256: hash,
		SourceKind: document.SourceKindEmbeddedOriginal, SourceMIME: "image/png",
		SourceObjectKey: versionTwoKey, DisplayStatus: document.DisplayUnavailable,
		ByteSize: 3, Width: 1, Height: 1, FirstSeenVersion: 2, LastSeenVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}

	close(blockingObjects.release)
	select {
	case <-firstSweepDone:
	case <-time.After(5 * time.Second):
		t.Fatal("late version 1 Delete did not return")
	}
	if blockingObjects.attempts.Load() != 2 {
		t.Fatalf("version 1 delete attempts=%d, want 2", blockingObjects.attempts.Load())
	}
	if reader, err := localObjects.Get(ctx, versionOneKey); err == nil {
		_ = reader.Close()
		t.Fatal("version 1 object survived overlapping cleanup")
	}
	reader, err := localObjects.Get(ctx, versionTwoKey)
	if err != nil {
		t.Fatalf("late version 1 Delete removed version 2 object: %v", err)
	}
	got, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(got) != "new" {
		t.Fatalf("version 2 object=%q err=%v", got, err)
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
	body := "恢复任务正文"
	kb, err := service.CreateKB(ctx, "u1", "恢复测试", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	doc := &store.RAGDocumentRecord{
		ID: "doc_recover", KBID: kb.ID, FileName: "recover.txt", FileType: "txt",
		FileSize: int64(len([]byte(body))), ObjectKey: objects.Key("u1", kb.ID, "doc_recover", "recover.txt"),
		Status: "PENDING", Version: 1, UploadedAt: time.Now().UTC(),
	}
	if err := service.obj.Put(ctx, doc.ObjectKey, strings.NewReader(body), doc.FileSize, "text/plain"); err != nil {
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
	if got.Text != want {
		t.Fatalf("empty agent result = %q, want %q", got.Text, want)
	}
	if len(got.Metadata) != 0 {
		t.Fatalf("empty agent result unexpectedly has metadata: %#v", got.Metadata)
	}
}

func TestAgentToolResultCarriesStableURLFreeResources(t *testing.T) {
	location := document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "Page 1"}
	hits := make([]Hit, 0, 8)
	assetIDs := make([]string, 0, 8)
	for i := 0; i < 8; i++ {
		assetID := fmt.Sprintf("ast_%032x", i+1)
		assetIDs = append(assetIDs, assetID)
		hit := Hit{
			KBID: "kb_manual", KBName: "Manual", DocID: "doc_manual", DocName: "manual.pdf",
			ChunkIndex: i, SectionTitle: "Install", SourceLocation: location, Content: fmt.Sprintf("passage %d", i),
			Assets: []document.AssetRef{{ID: assetID, Kind: document.AssetKindImage, Caption: fmt.Sprintf("figure %d", i), Location: location}},
		}
		if i == 1 {
			// A repeated resource in a later final hit must not change the first
			// occurrence's source or consume the display budget.
			hit.Assets = append([]document.AssetRef{hits[0].Assets[0]}, hit.Assets...)
		}
		hits = append(hits, hit)
	}

	got := agentToolResult(hits)
	if strings.Contains(got.Text, "ast_") || strings.Contains(got.Text, "://") {
		t.Fatalf("model-visible RAG text leaked an asset identifier or URL: %q", got.Text)
	}
	var refs []RAGResourceRef
	if err := json.Unmarshal(got.Metadata[agenttools.RAGResourcesMetadataKey], &refs); err != nil {
		t.Fatal(err)
	}
	if len(refs) != maxAgentRAGResources {
		t.Fatalf("resource count = %d, want %d", len(refs), maxAgentRAGResources)
	}
	for i, ref := range refs {
		if ref.Asset.ID != assetIDs[i] || ref.KBID != "kb_manual" || ref.DocID != "doc_manual" || ref.ChunkIndex != i {
			t.Fatalf("resource %d = %+v, want first-hit order and stable provenance", i, ref)
		}
	}
}
