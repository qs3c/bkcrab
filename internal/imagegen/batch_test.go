package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type batchServiceStore struct {
	mu          sync.Mutex
	createCalls int
	createReq   store.CreateImageGenerationBatchRequest
	batches     map[string]*store.ImageGenerationBatchRecord
	tasks       map[string][]store.ImageGenerationTaskRecord
	onCreate    func()
}

func (s *batchServiceStore) CreateImageGenerationBatch(_ context.Context, request store.CreateImageGenerationBatchRequest) (*store.ImageGenerationBatchRecord, []store.ImageGenerationTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.createCalls++
	s.createReq = request
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	batch := &store.ImageGenerationBatchRecord{
		ID: request.BatchID, UserID: request.UserID, ConfigUserID: request.ConfigUserID,
		AgentOwnerUserID: request.AgentOwnerUserID, AgentID: request.AgentID,
		WorkspaceProjectID: request.WorkspaceProjectID, WorkspaceSessionID: request.WorkspaceSessionID,
		RequestJSON: request.RequestJSON, ProviderPlanJSON: request.ProviderPlanJSON,
		Status: store.ImageGenerationBatchPending, RequestedCount: request.RequestedCount, CreatedAt: now, UpdatedAt: now,
	}
	tasks := make([]store.ImageGenerationTaskRecord, 0, len(request.Tasks))
	for index, task := range request.Tasks {
		tasks = append(tasks, store.ImageGenerationTaskRecord{
			ID: task.ID, SequenceID: int64(index + 1), BatchID: request.BatchID, UserID: request.UserID,
			ItemIndex: task.ItemIndex, ChunkIndex: task.ChunkIndex, Label: task.Label, Prompt: task.Prompt,
			Size: task.Size, RequestedCount: task.RequestedCount, RequestFingerprint: task.RequestFingerprint,
			Status: store.ImageGenerationTaskPending, MaxRetry: request.MaxRetries,
		})
	}
	if s.batches == nil {
		s.batches = map[string]*store.ImageGenerationBatchRecord{}
	}
	if s.tasks == nil {
		s.tasks = map[string][]store.ImageGenerationTaskRecord{}
	}
	s.batches[batch.ID] = batch
	s.tasks[batch.ID] = tasks
	if s.onCreate != nil {
		s.onCreate()
	}
	return cloneBatchRecord(batch), append([]store.ImageGenerationTaskRecord(nil), tasks...), nil
}

func (s *batchServiceStore) GetImageGenerationBatchForPrincipal(_ context.Context, userID, agentID, batchID string) (*store.ImageGenerationBatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.batches[batchID]
	if batch == nil || batch.UserID != userID || batch.AgentID != agentID {
		return nil, store.ErrNotFound
	}
	return cloneBatchRecord(batch), nil
}

func (s *batchServiceStore) ListImageGenerationTasks(_ context.Context, batchID string) ([]store.ImageGenerationTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.ImageGenerationTaskRecord(nil), s.tasks[batchID]...), nil
}

func (s *batchServiceStore) RequestImageBatchCancel(_ context.Context, userID, agentID, batchID string) (*store.ImageGenerationBatchRecord, []store.ImageGenerationTaskRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batch := s.batches[batchID]
	if batch == nil || batch.UserID != userID || batch.AgentID != agentID {
		return nil, nil, store.ErrNotFound
	}
	if batch.Status == store.ImageGenerationBatchDone || batch.Status == store.ImageGenerationBatchFailed || batch.Status == store.ImageGenerationBatchPartial || batch.Status == store.ImageGenerationBatchCanceled {
		return cloneBatchRecord(batch), append([]store.ImageGenerationTaskRecord(nil), s.tasks[batchID]...), nil
	}
	batch.CancelRequested = true
	running := false
	for index := range s.tasks[batchID] {
		task := &s.tasks[batchID][index]
		if task.Status == store.ImageGenerationTaskPending {
			task.Status = store.ImageGenerationTaskCanceled
			batch.CanceledCount += task.RequestedCount
		}
		if task.Status == store.ImageGenerationTaskRunning {
			running = true
		}
	}
	if running {
		batch.Status = store.ImageGenerationBatchCanceling
	} else {
		batch.Status = store.ImageGenerationBatchCanceled
	}
	return cloneBatchRecord(batch), append([]store.ImageGenerationTaskRecord(nil), s.tasks[batchID]...), nil
}

func cloneBatchRecord(batch *store.ImageGenerationBatchRecord) *store.ImageGenerationBatchRecord {
	if batch == nil {
		return nil
	}
	clone := *batch
	return &clone
}

type batchServicePlanResolver struct {
	calls int
	err   error
}

func (r *batchServicePlanResolver) Snapshot(_ context.Context, identity ExecutionIdentity) (SafeProviderPlan, error) {
	r.calls++
	if r.err != nil {
		return SafeProviderPlan{}, r.err
	}
	return SafeProviderPlan{
		Version: ProviderPlanSchemaVersion, ConfigUserID: identity.ConfigUserID,
		AgentOwnerUserID: identity.AgentOwnerUserID, AgentID: identity.AgentID,
		AutoFallback: true, Candidates: []ProviderCandidateRef{{Provider: "openai", Model: "gpt-image-1"}},
	}, nil
}

func (r *batchServicePlanResolver) Resolve(context.Context, ExecutionIdentity, SafeProviderPlan) (ResolvedProviderPlan, error) {
	return ResolvedProviderPlan{}, errors.New("unused")
}

type batchServiceDispatcher struct {
	calls int
	err   error
}

func (d *batchServiceDispatcher) TryDispatch(context.Context, string) error {
	d.calls++
	return d.err
}

type batchServiceClock struct{ now time.Time }

func (c *batchServiceClock) Now() time.Time { return c.now }

type batchServiceWakeup struct {
	clock *batchServiceClock
	waits int
	after func(int)
}

func (w *batchServiceWakeup) Wait(ctx context.Context, _ string, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.waits++
	w.clock.now = w.clock.now.Add(delay)
	if w.after != nil {
		w.after(w.waits)
	}
	return nil
}

type batchServiceURLResolver struct {
	scopes []ArtifactScope
}

func (r *batchServiceURLResolver) ResolveArtifactURL(_ context.Context, scope ArtifactScope, key string) (string, error) {
	r.scopes = append(r.scopes, scope)
	if len(r.scopes) > 1 {
		return "", workspace.ErrSignedURLUnsupported
	}
	return "https://signed.example/object", nil
}

func batchIdentity() ExecutionIdentity {
	return ExecutionIdentity{
		UserID: "tenant-a", ConfigUserID: "config-a", AgentOwnerUserID: "owner-a", AgentID: "agent-a",
		WorkspaceProjectID: "project-a", WorkspaceSessionID: "session-a", MessageChannel: "web",
	}
}

func batchCreateRequest(count, wait int) NormalizedRequest {
	return NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, WaitSeconds: wait, Items: []NormalizedItem{
		{Index: 0, Label: "item", Prompt: "draw", Size: SizeSquare, Count: count},
	}}
}

func newBatchServiceForTest(store *batchServiceStore, resolver *batchServicePlanResolver, dispatcher *batchServiceDispatcher, options BatchServiceOptions) *BatchService {
	options.Store = store
	options.ProviderPlans = resolver
	options.Dispatcher = dispatcher
	options.IDGenerator = func(kind string, sequence int) string {
		if kind == "batch" {
			return "imgb_0123456789abcdef"
		}
		return fmt.Sprintf("imgt_%016x", sequence+1)
	}
	return NewBatchService(options)
}

func TestBatchServiceCreateRejectsBeforeResolverOrStore(t *testing.T) {
	storeFake := &batchServiceStore{}
	resolver := &batchServicePlanResolver{}
	dispatcher := &batchServiceDispatcher{}
	service := newBatchServiceForTest(storeFake, resolver, dispatcher, BatchServiceOptions{})
	invalid := batchCreateRequest(17, 0)
	if _, err := service.Create(context.Background(), batchIdentity(), invalid); err == nil {
		t.Fatal("invalid create accepted")
	}
	if resolver.calls != 0 || storeFake.createCalls != 0 || dispatcher.calls != 0 {
		t.Fatalf("invalid input touched dependencies: resolver=%d store=%d dispatcher=%d", resolver.calls, storeFake.createCalls, dispatcher.calls)
	}
}

func TestBatchServiceCreateSnapshotFailureDoesNotCreate(t *testing.T) {
	storeFake := &batchServiceStore{}
	resolver := &batchServicePlanResolver{err: errors.New("provider unavailable")}
	service := newBatchServiceForTest(storeFake, resolver, &batchServiceDispatcher{}, BatchServiceOptions{})
	if _, err := service.Create(context.Background(), batchIdentity(), batchCreateRequest(1, 0)); err == nil {
		t.Fatal("snapshot failure accepted")
	}
	if storeFake.createCalls != 0 {
		t.Fatal("snapshot failure created batch")
	}
}

func TestBatchServiceCreatePersistsTrustedScopePlanAndDeterministicTasks(t *testing.T) {
	storeFake := &batchServiceStore{}
	resolver := &batchServicePlanResolver{}
	dispatcher := &batchServiceDispatcher{err: errors.New("rabbit unavailable")}
	service := newBatchServiceForTest(storeFake, resolver, dispatcher, BatchServiceOptions{MaxImagesPerTask: 4, MaxRetries: 3})
	result, err := service.Create(context.Background(), batchIdentity(), batchCreateRequest(16, 0))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if result.BatchID != "imgb_0123456789abcdef" || result.Status != BatchStatusPending || dispatcher.calls != 1 {
		t.Fatalf("create result/dispatch: %#v calls=%d", result, dispatcher.calls)
	}
	request := storeFake.createReq
	identity := batchIdentity()
	if request.UserID != identity.UserID || request.ConfigUserID != identity.ConfigUserID || request.AgentOwnerUserID != identity.AgentOwnerUserID || request.AgentID != identity.AgentID || request.WorkspaceProjectID != identity.WorkspaceProjectID || request.WorkspaceSessionID != identity.WorkspaceSessionID {
		t.Fatalf("untrusted or missing persisted scope: %#v", request)
	}
	if request.RequestedCount != 16 || len(request.Tasks) != 4 || request.Tasks[0].RequestedCount != 4 || request.Tasks[3].ChunkIndex != 3 {
		t.Fatalf("task planning/order: %#v", request.Tasks)
	}
	for index, task := range request.Tasks {
		if task.ID != fmt.Sprintf("imgt_%016x", index+1) || task.UserID != identity.UserID {
			t.Fatalf("task %d identity/order: %#v", index, task)
		}
	}
	lowerPlan := strings.ToLower(string(request.ProviderPlanJSON))
	for _, forbidden := range []string{"apikey", "authorization", "bearer", "secretkey", "access token", "sk-"} {
		if strings.Contains(lowerPlan, forbidden) {
			t.Fatalf("provider plan leaked %q: %s", forbidden, request.ProviderPlanJSON)
		}
	}
}

func TestBatchServiceCreateContextCanceledAfterCommitKeepsBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	storeFake := &batchServiceStore{onCreate: cancel}
	service := newBatchServiceForTest(storeFake, &batchServicePlanResolver{}, &batchServiceDispatcher{}, BatchServiceOptions{})
	result, err := service.Create(ctx, batchIdentity(), batchCreateRequest(1, 30))
	if err != nil || result.BatchID == "" || storeFake.batches[result.BatchID] == nil {
		t.Fatalf("committed batch lost on cancellation: result=%#v err=%v", result, err)
	}
}

func TestBatchServiceStatusAuthorizesCrossSessionOrdersAndSanitizes(t *testing.T) {
	created := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	batchID := "imgb_0123456789abcdef"
	storeFake := &batchServiceStore{
		batches: map[string]*store.ImageGenerationBatchRecord{batchID: {
			ID: batchID, UserID: "tenant-a", AgentID: "agent-a", WorkspaceProjectID: "origin-project", WorkspaceSessionID: "origin-session",
			Status: store.ImageGenerationBatchPartial, RequestedCount: 3, SucceededCount: 2, FailedCount: 1, CreatedAt: created, UpdatedAt: created,
		}},
		tasks: map[string][]store.ImageGenerationTaskRecord{batchID: {
			{ID: "imgt_2222222222222222", BatchID: batchID, ItemIndex: 1, ChunkIndex: 0, Label: "failed", RequestedCount: 1, Status: store.ImageGenerationTaskFailed, ErrorCode: "UPSTREAM_TRANSIENT", ErrorMessage: "failed https://provider.invalid/path?token=do-not-leak " + strings.Repeat("x", 600)},
			{ID: "imgt_1111111111111111", BatchID: batchID, ItemIndex: 0, ChunkIndex: 0, Label: "done", RequestedCount: 2, Status: store.ImageGenerationTaskDone, Provider: "openai", Model: "gpt-image-1", ManifestKey: "imagegen/manifest", ArtifactsJSON: mustArtifactJSON(t, []ImageArtifact{
				{Index: 0, Key: "imagegen/" + batchID + "/imgt_1111111111111111/claims/1/image-0-" + strings.Repeat("a", 64) + ".png", MIMEType: "image/png", Size: 10, SHA256: strings.Repeat("a", 64), Width: 2, Height: 3},
				{Index: 1, Key: "imagegen/" + batchID + "/imgt_1111111111111111/claims/1/image-1-" + strings.Repeat("b", 64) + ".png", MIMEType: "image/png", Size: 11, SHA256: strings.Repeat("b", 64), Width: 4, Height: 5},
			})},
		}},
	}
	urlResolver := &batchServiceURLResolver{}
	service := NewBatchService(BatchServiceOptions{Store: storeFake, ArtifactURLs: urlResolver})
	identity := batchIdentity()
	identity.WorkspaceProjectID = "another-project"
	identity.WorkspaceSessionID = "another-session"
	result, err := service.Status(context.Background(), identity, batchID)
	if err != nil {
		t.Fatalf("cross-session status: %v", err)
	}
	if result.Status != BatchStatusPartial || len(result.Tasks) != 2 || result.Tasks[0].ItemIndex != 0 || len(result.Artifacts) != 2 {
		t.Fatalf("status ordering/aggregate: %#v", result)
	}
	if result.Artifacts[0].Origin.ProjectID != "origin-project" || result.Artifacts[0].Origin.SessionID != "origin-session" || result.Artifacts[0].URL == "" {
		t.Fatalf("artifact origin/URL: %#v", result.Artifacts[0])
	}
	if result.Artifacts[1].URL != "" || result.Artifacts[1].Path == "" {
		t.Fatalf("signed URL unsupported should retain workspace path: %#v", result.Artifacts[1])
	}
	if len(urlResolver.scopes) != 2 || urlResolver.scopes[0].ProjectID != "origin-project" {
		t.Fatalf("URL resolver used current rather than origin scope: %#v", urlResolver.scopes)
	}
	errorMessage := result.Tasks[1].ErrorMessage
	if len(errorMessage) > 256 || strings.Contains(errorMessage, "do-not-leak") || strings.Contains(errorMessage, "?token=") {
		t.Fatalf("unbounded/query-bearing task error: %q", errorMessage)
	}
	wrongUser := identity
	wrongUser.UserID = "other-user"
	if _, err := service.Status(context.Background(), wrongUser, batchID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("different user status error=%v", err)
	}
	wrongAgent := identity
	wrongAgent.AgentID = "other-agent"
	if _, err := service.Status(context.Background(), wrongAgent, batchID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("different agent status error=%v", err)
	}
}

func mustArtifactJSON(t *testing.T, artifacts []ImageArtifact) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(artifacts)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestBatchServiceCancelIsIdempotentAndNeverDispatches(t *testing.T) {
	batchID := "imgb_0123456789abcdef"
	storeFake := &batchServiceStore{
		batches: map[string]*store.ImageGenerationBatchRecord{batchID: {ID: batchID, UserID: "tenant-a", AgentID: "agent-a", Status: store.ImageGenerationBatchRunning, RequestedCount: 3}},
		tasks: map[string][]store.ImageGenerationTaskRecord{batchID: {
			{ID: "imgt_1111111111111111", BatchID: batchID, ItemIndex: 0, RequestedCount: 1, Status: store.ImageGenerationTaskDone},
			{ID: "imgt_2222222222222222", BatchID: batchID, ItemIndex: 1, RequestedCount: 1, Status: store.ImageGenerationTaskRunning},
			{ID: "imgt_3333333333333333", BatchID: batchID, ItemIndex: 2, RequestedCount: 1, Status: store.ImageGenerationTaskPending},
		}},
	}
	dispatcher := &batchServiceDispatcher{}
	service := NewBatchService(BatchServiceOptions{Store: storeFake, Dispatcher: dispatcher})
	first, err := service.Cancel(context.Background(), batchIdentity(), batchID)
	if err != nil || first.Status != BatchStatusCanceling || first.CanceledCount != 1 {
		t.Fatalf("first cancel=%#v err=%v", first, err)
	}
	second, err := service.Cancel(context.Background(), batchIdentity(), batchID)
	if err != nil || second.Status != BatchStatusCanceling || second.CanceledCount != 1 {
		t.Fatalf("idempotent cancel=%#v err=%v", second, err)
	}
	if dispatcher.calls != 0 {
		t.Fatalf("cancel invoked dispatcher %d times", dispatcher.calls)
	}
}

func TestBatchServiceWaitUsesAuthoritativeReadsAndReturnsCurrentAtDeadline(t *testing.T) {
	clock := &batchServiceClock{now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	storeFake := &batchServiceStore{}
	wakeup := &batchServiceWakeup{clock: clock}
	wakeup.after = func(wait int) {
		storeFake.mu.Lock()
		defer storeFake.mu.Unlock()
		batch := storeFake.batches["imgb_0123456789abcdef"]
		if wait == 1 {
			batch.Status = store.ImageGenerationBatchRunning
		}
	}
	service := newBatchServiceForTest(storeFake, &batchServicePlanResolver{}, &batchServiceDispatcher{}, BatchServiceOptions{
		Clock: clock, Wakeup: wakeup, PollInterval: time.Second, Jitter: func(delay time.Duration) time.Duration { return delay },
	})
	result, err := service.Create(context.Background(), batchIdentity(), batchCreateRequest(1, 2))
	if err != nil {
		t.Fatalf("create/wait: %v", err)
	}
	if result.Status != BatchStatusRunning || wakeup.waits != 2 {
		t.Fatalf("deadline should return authoritative current status: result=%#v waits=%d", result, wakeup.waits)
	}
	if storeFake.batches[result.BatchID].CancelRequested {
		t.Fatal("wait deadline canceled durable batch")
	}
}
