package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

type imageFairQueueStoreFake struct {
	candidates    []store.ImageTaskDispatchCandidate
	claimResult   store.ImageGenerationClaimResult
	claimErr      error
	marked        bool
	finished      string
	finishDoneErr error
	heartbeats    atomic.Int64
}

type imageFairGeneratorFake struct{ calls *[]string }

func (f imageFairGeneratorFake) Generate(context.Context, ExecutionIdentity, SafeProviderPlan, GenerateRequest) (ProviderResult, error) {
	*f.calls = append(*f.calls, "generate")
	return ProviderResult{Provider: "openai", Model: "gpt-image-1", Images: []GeneratedImage{{Bytes: []byte("png")}}}, nil
}

type imageFairArtifactsFake struct{ calls *[]string }

func (f imageFairArtifactsFake) Salvage(_ context.Context, request ArtifactSalvageRequest) (ArtifactManifest, bool, error) {
	*f.calls = append(*f.calls, "salvage:"+strconv.FormatInt(request.PreviousClaimGeneration, 10))
	return ArtifactManifest{}, false, nil
}
func (f imageFairArtifactsFake) Publish(_ context.Context, request ArtifactPublishRequest) (ArtifactManifest, error) {
	*f.calls = append(*f.calls, "publish:"+strconv.FormatInt(request.ClaimGeneration, 10))
	return ArtifactManifest{Provider: request.Provider, Model: request.Model, ManifestKey: "imagegen/manifest.json", Artifacts: []ImageArtifact{{Index: 0, Key: "imagegen/image.png", MIMEType: "image/png", Size: 3, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1}}}, nil
}
func (f imageFairArtifactsFake) DeleteClaimArtifacts(context.Context, ArtifactManifest) error {
	return nil
}

func (f *imageFairQueueStoreFake) ListDispatchableImageTasksPage(context.Context, int64, int) ([]store.ImageTaskDispatchCandidate, int64, error) {
	return f.candidates, int64(len(f.candidates)), nil
}
func (f *imageFairQueueStoreFake) GetDispatchableImageTaskByID(_ context.Context, id string) (*store.ImageTaskDispatchCandidate, error) {
	for i := range f.candidates {
		if f.candidates[i].Task.ID == id {
			return &f.candidates[i], nil
		}
	}
	return nil, store.ErrNotFound
}
func (f *imageFairQueueStoreFake) MarkImageTaskDispatched(context.Context, store.ImageTaskDispatchCandidate, int64) (bool, error) {
	f.marked = true
	return true, nil
}
func (f *imageFairQueueStoreFake) SweepExpiredImageGenerationTasks(context.Context, int64, int, time.Duration) ([]store.ImageTaskDispatchCandidate, int64, error) {
	return nil, 0, nil
}
func (f *imageFairQueueStoreFake) ClaimImageGenerationTaskByID(context.Context, string, string, int64, string, time.Duration, store.ImageGenerationClaimLimits) (store.ImageGenerationClaimResult, error) {
	return f.claimResult, f.claimErr
}
func (f *imageFairQueueStoreFake) RepairPoisonImageCandidate(context.Context, store.ImagePoisonRepairLocator, string, string) (*store.ImageTaskDispatchCandidate, store.ImagePoisonRepairDisposition, error) {
	return nil, store.ImagePoisonRepairStale, nil
}
func (f *imageFairQueueStoreFake) HeartbeatImageGenerationTask(context.Context, store.ImageGenerationFence, time.Duration) (store.ImageGenerationHeartbeatDisposition, error) {
	f.heartbeats.Add(1)
	return store.ImageGenerationHeartbeatExtended, nil
}
func (f *imageFairQueueStoreFake) FinishImageGenerationTaskDone(context.Context, store.ImageGenerationFence, store.ImageTaskDoneResult) (*store.ImageGenerationBatchRecord, bool, error) {
	f.finished = "done"
	return &store.ImageGenerationBatchRecord{}, true, f.finishDoneErr
}

type imageSlowArtifactsFake struct {
	delay   time.Duration
	deleted atomic.Int64
}

func (f *imageSlowArtifactsFake) Salvage(context.Context, ArtifactSalvageRequest) (ArtifactManifest, bool, error) {
	return ArtifactManifest{}, false, nil
}

func (f *imageSlowArtifactsFake) Publish(ctx context.Context, request ArtifactPublishRequest) (ArtifactManifest, error) {
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ArtifactManifest{}, ctx.Err()
	case <-timer.C:
	}
	return ArtifactManifest{Provider: request.Provider, Model: request.Model, ManifestKey: "imagegen/manifest.json", Artifacts: []ImageArtifact{{Index: 0, Key: "imagegen/image.png", Size: 3}}}, nil
}

func (f *imageSlowArtifactsFake) DeleteClaimArtifacts(context.Context, ArtifactManifest) error {
	f.deleted.Add(1)
	return nil
}
func (f *imageFairQueueStoreFake) FinishImageGenerationTaskRetry(context.Context, store.ImageGenerationFence, string, time.Time) (bool, error) {
	f.finished = "retry"
	return true, nil
}
func (f *imageFairQueueStoreFake) FinishImageGenerationTaskFailed(context.Context, store.ImageGenerationFence, string) (bool, error) {
	f.finished = "failed"
	return true, nil
}
func (f *imageFairQueueStoreFake) FinishImageGenerationTaskCanceled(context.Context, store.ImageGenerationFence) (bool, error) {
	f.finished = "canceled"
	return true, nil
}

func imageFairCandidate() store.ImageTaskDispatchCandidate {
	task := store.ImageTaskDispatchRecord{ID: "imgt_1111111111111111", SequenceID: 1, BatchID: "imgb_1111111111111111", UserID: "user-a", Status: store.ImageGenerationTaskPending, DispatchGeneration: 3}
	return store.ImageTaskDispatchCandidate{Task: task, Guard: store.ImageTaskDispatchGuard{TaskID: task.ID, SequenceID: 1, BatchID: task.BatchID, UserID: task.UserID, Status: task.Status, DispatchGeneration: 3}}
}

func imageFairExecutionClaim(t *testing.T) *store.ImageGenerationTaskClaim {
	t.Helper()
	plan, err := json.Marshal(SafeProviderPlan{Version: ProviderPlanSchemaVersion, ConfigUserID: "config-a", AgentOwnerUserID: "owner-a", AgentID: "agent-a", Candidates: []ProviderCandidateRef{{Provider: "openai", Model: "gpt-image-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	return &store.ImageGenerationTaskClaim{
		Task:  store.ImageGenerationTaskRecord{ID: "imgt_1111111111111111", BatchID: "imgb_1111111111111111", UserID: "user-a", Prompt: "prompt", Size: SizeSquare, RequestedCount: 1, RequestFingerprint: strings.Repeat("a", 64), Status: store.ImageGenerationTaskRunning, ClaimGeneration: 7, DispatchGeneration: 7},
		Batch: store.ImageGenerationBatchRecord{ID: "imgb_1111111111111111", UserID: "user-a", ConfigUserID: "config-a", AgentOwnerUserID: "owner-a", AgentID: "agent-a", ProviderPlanJSON: plan, ArtifactByteLimit: 128 << 20},
		Fence: store.ImageGenerationFence{TaskID: "imgt_1111111111111111", BatchID: "imgb_1111111111111111", UserID: "user-a", ClaimGeneration: 7, LeaseOwner: "worker-a", ExpectedWriterFingerprint: strings.Repeat("a", 64)},
	}
}

func imageExecutablePrepareRequest(t *testing.T, message fairqueue.Message) fairqueue.PrepareRequest {
	t.Helper()
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	tenantHash, err := fairqueue.TenantHash(message.Resource, message.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	version := int32(fairqueue.MessageVersion1)
	resource, taskID, generation := message.Resource, message.TaskID, int64(message.DispatchToken.Generation)
	token := message.DispatchToken
	body := message
	request := fairqueue.PrepareRequest{
		Message: &message, BodyCandidate: &body, HeaderToken: &token,
		HeaderFacts:        fairqueue.StableHeaderFacts{ProtocolVersion: &version, Resource: &resource, TaskID: &taskID, DispatchGeneration: &generation},
		RegisteredResource: message.Resource, QueueTenantHash: tenantHash,
		PublishAttemptID: strings.Repeat("a", 32), RawBody: raw,
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestFairQueueImageResourceConfigAndDispatchMessageAreStrict(t *testing.T) {
	cfg := config.DefaultImagegenBatchCfg()
	resource := ImageFairQueueResourceConfig(cfg)
	if resource.Key != store.ImageGenerationResource || !resource.ValidateTaskID("imgt_1111111111111111") {
		t.Fatalf("resource config=%+v", resource)
	}
	for _, invalid := range []string{"task-1", "imgt_short", "imgt_" + strings.Repeat("a", 65)} {
		if resource.ValidateTaskID(invalid) {
			t.Fatalf("accepted task id %q", invalid)
		}
	}
	fake := &imageFairQueueStoreFake{candidates: []store.ImageTaskDispatchCandidate{imageFairCandidate()}}
	adapter := NewFairQueueAdapter(FairQueueAdapterOptions{Store: fake, WorkerID: "worker-a", TaskLease: time.Minute, ClaimLimits: store.ImageGenerationClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 4, AdvisoryLockTimeout: time.Second}})
	candidates, _, err := adapter.ListDispatchCandidates(context.Background(), "", 10)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("dispatch candidates=%+v err=%v", candidates, err)
	}
	message := candidates[0].Message
	if message.Resource != store.ImageGenerationResource || message.TaskType != ImageGenerationTaskType || message.DispatchToken.Generation != 3 {
		t.Fatalf("message=%+v", message)
	}
	raw, _ := json.Marshal(message)
	for _, forbidden := range []string{"prompt", "provider_plan", "artifact", "batch_id"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("wire message leaked %q: %s", forbidden, raw)
		}
	}
	if changed, err := adapter.MarkDispatched(context.Background(), candidates[0]); err != nil || !changed || !fake.marked {
		t.Fatalf("mark changed=%v marked=%v err=%v", changed, fake.marked, err)
	}
}

func TestFairQueuePrepareMapsExactClaimDispositions(t *testing.T) {
	message := fairqueue.Message{Version: fairqueue.MessageVersion1, Resource: store.ImageGenerationResource, TenantID: "user-a", TaskType: ImageGenerationTaskType, TaskID: "imgt_1111111111111111", DispatchToken: fairqueue.DispatchToken{Resource: store.ImageGenerationResource, TaskID: "imgt_1111111111111111", Generation: 1}}
	request := imageExecutablePrepareRequest(t, message)
	for _, tc := range []struct {
		name        string
		disposition store.ImageGenerationClaimDisposition
		want        fairqueue.PrepareDisposition
		prepared    bool
	}{{"claimed", store.ImageGenerationClaimed, fairqueue.PrepareClaimed, true}, {"capacity", store.ImageGenerationClaimCapacityDeferred, fairqueue.PrepareCapacityDeferred, false}, {"stale", store.ImageGenerationClaimDuplicateStale, fairqueue.PrepareDuplicateStaleTerminal, false}, {"canceled", store.ImageGenerationClaimBatchCanceled, fairqueue.PrepareDuplicateStaleTerminal, false}} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &imageFairQueueStoreFake{claimResult: store.ImageGenerationClaimResult{Disposition: tc.disposition}}
			if tc.prepared {
				fake.claimResult.Claim = &store.ImageGenerationTaskClaim{Task: store.ImageGenerationTaskRecord{ID: message.TaskID}, Fence: store.ImageGenerationFence{TaskID: message.TaskID, ClaimGeneration: 1}}
			}
			adapter := NewFairQueueAdapter(FairQueueAdapterOptions{Store: fake, WorkerID: "worker-a", TaskLease: time.Minute, ClaimLimits: store.ImageGenerationClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 4, AdvisoryLockTimeout: time.Second}})
			prepared, result, err := adapter.Prepare(context.Background(), request)
			if err != nil || result.Disposition != tc.want || (prepared != nil) != tc.prepared {
				t.Fatalf("prepared=%v result=%+v err=%v", prepared != nil, result, err)
			}
		})
	}

	fake := &imageFairQueueStoreFake{claimErr: errors.New("temporary mysql error")}
	adapter := NewFairQueueAdapter(FairQueueAdapterOptions{Store: fake, WorkerID: "worker-a", TaskLease: time.Minute, ClaimLimits: store.ImageGenerationClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 4, AdvisoryLockTimeout: time.Second}})
	if prepared, result, err := adapter.Prepare(context.Background(), request); err == nil || prepared != nil || result.Disposition != fairqueue.PrepareTransientInfrastructure {
		t.Fatalf("transient prepared=%v result=%+v err=%v", prepared, result, err)
	}
}

func TestFairQueuePreparedTaskUsesPreviousGenerationThenPublishesAndFencedFinishes(t *testing.T) {
	message := fairqueue.Message{Version: fairqueue.MessageVersion1, Resource: store.ImageGenerationResource, TenantID: "user-a", TaskType: ImageGenerationTaskType, TaskID: "imgt_1111111111111111", DispatchToken: fairqueue.DispatchToken{Resource: store.ImageGenerationResource, TaskID: "imgt_1111111111111111", Generation: 7}}
	plan, _ := json.Marshal(SafeProviderPlan{Version: ProviderPlanSchemaVersion, ConfigUserID: "config-a", AgentOwnerUserID: "owner-a", AgentID: "agent-a", Candidates: []ProviderCandidateRef{{Provider: "openai", Model: "gpt-image-1"}}})
	fake := &imageFairQueueStoreFake{claimResult: store.ImageGenerationClaimResult{Disposition: store.ImageGenerationClaimed, Claim: &store.ImageGenerationTaskClaim{
		Task:                    store.ImageGenerationTaskRecord{ID: message.TaskID, BatchID: "imgb_1111111111111111", UserID: "user-a", Prompt: "prompt", Size: SizeSquare, RequestedCount: 1, RequestFingerprint: strings.Repeat("a", 64), Status: store.ImageGenerationTaskRunning, ClaimGeneration: 7, DispatchGeneration: 7},
		Batch:                   store.ImageGenerationBatchRecord{ID: "imgb_1111111111111111", UserID: "user-a", ConfigUserID: "config-a", AgentOwnerUserID: "owner-a", AgentID: "agent-a", ProviderPlanJSON: plan},
		Fence:                   store.ImageGenerationFence{TaskID: message.TaskID, BatchID: "imgb_1111111111111111", UserID: "user-a", ClaimGeneration: 7, LeaseOwner: "worker-a", ExpectedWriterFingerprint: strings.Repeat("a", 64)},
		PreviousClaimGeneration: 3,
	}}}
	var calls []string
	adapter := NewFairQueueAdapter(FairQueueAdapterOptions{
		Store: fake, WorkerID: "worker-a", TaskLease: time.Minute, TaskHeartbeat: time.Second,
		ClaimLimits: store.ImageGenerationClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 4, AdvisoryLockTimeout: time.Second},
		Generation:  imageFairGeneratorFake{calls: &calls}, Artifacts: imageFairArtifactsFake{calls: &calls},
	})
	prepared, result, err := adapter.Prepare(context.Background(), imageExecutablePrepareRequest(t, message))
	if err != nil || result.Disposition != fairqueue.PrepareClaimed {
		t.Fatalf("prepare result=%+v err=%v", result, err)
	}
	if err := prepared.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if strings.Join(calls, ",") != "salvage:3,generate,publish:7" || fake.finished != "done" {
		t.Fatalf("execution order=%v finished=%q", calls, fake.finished)
	}
}

func TestFairQueueClaimHeartbeatCoversArtifactPublish(t *testing.T) {
	fake := &imageFairQueueStoreFake{}
	artifacts := &imageSlowArtifactsFake{delay: 35 * time.Millisecond}
	var calls []string
	adapter := NewFairQueueAdapter(FairQueueAdapterOptions{
		Store: fake, WorkerID: "worker-a", TaskLease: time.Second, TaskHeartbeat: 5 * time.Millisecond,
		ClaimLimits: store.ImageGenerationClaimLimits{GlobalConcurrency: 1, PerUserBurstConcurrency: 1, AdvisoryLockTimeout: time.Second},
		Generation:  imageFairGeneratorFake{calls: &calls}, Artifacts: artifacts,
	})
	if err := adapter.runClaim(context.Background(), imageFairExecutionClaim(t)); err != nil {
		t.Fatal(err)
	}
	if heartbeats := fake.heartbeats.Load(); heartbeats < 2 {
		t.Fatalf("artifact publish was not lease-protected, heartbeats=%d", heartbeats)
	}
}

func TestFairQueueBatchArtifactLimitDeletesRejectedClaimArtifacts(t *testing.T) {
	fake := &imageFairQueueStoreFake{finishDoneErr: store.ErrImageGenerationBatchArtifactLimit}
	artifacts := &imageSlowArtifactsFake{}
	var calls []string
	adapter := NewFairQueueAdapter(FairQueueAdapterOptions{
		Store: fake, WorkerID: "worker-a", TaskLease: time.Second, TaskHeartbeat: 50 * time.Millisecond,
		ClaimLimits: store.ImageGenerationClaimLimits{GlobalConcurrency: 1, PerUserBurstConcurrency: 1, AdvisoryLockTimeout: time.Second},
		Generation:  imageFairGeneratorFake{calls: &calls}, Artifacts: artifacts,
	})
	err := adapter.runClaim(context.Background(), imageFairExecutionClaim(t))
	if !errors.Is(err, store.ErrImageGenerationBatchArtifactLimit) || artifacts.deleted.Load() != 1 || fake.finished != "done" {
		t.Fatalf("limit cleanup err=%v deleted=%d finished=%q", err, artifacts.deleted.Load(), fake.finished)
	}
}
