package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestImageDispatchDTOContainsNoPromptOrProviderPlan(t *testing.T) {
	typeOf := reflect.TypeOf(ImageTaskDispatchRecord{})
	for _, forbidden := range []string{"Prompt", "Label", "RequestJSON", "ProviderPlanJSON", "ArtifactsJSON"} {
		if _, exists := typeOf.FieldByName(forbidden); exists {
			t.Fatalf("dispatch DTO exposes forbidden field %s", forbidden)
		}
	}
}

func TestBoundedImageErrorPreservesUTF8(t *testing.T) {
	got := boundedImageError("界界", 5)
	if !utf8.ValidString(got) || len(got) > 5 {
		t.Fatalf("bounded error = %q (%d bytes)", got, len(got))
	}
}

func TestImageGenerationCanonicalMySQLDDL(t *testing.T) {
	ddl := strings.ToLower(strings.Join(mysqlMigrationSQL(), "\n"))
	if got := strings.Count(ddl, "create table if not exists image_generation_"); got != 2 {
		t.Fatalf("imagegen business table count = %d, want 2", got)
	}
	for _, token := range []string{
		"create table if not exists image_generation_batches",
		"request_json json not null",
		"provider_plan_json json not null",
		"artifact_byte_limit bigint not null default 134217728",
		"key idx_image_generation_batches_owner (user_id, created_at, id)",
		"create table if not exists image_generation_tasks",
		"sequence_id bigint not null auto_increment",
		"unique key uq_image_generation_tasks_sequence (sequence_id)",
		"unique key uq_image_generation_tasks_chunk (batch_id, item_index, chunk_index)",
		"dispatch_generation bigint not null default 1",
		"claim_generation bigint not null default 0",
		"artifacts_json json",
		"key idx_image_generation_tasks_dispatch (status, dispatched_at, next_run_at, lease_until, sequence_id)",
		"key idx_image_generation_tasks_user_running (user_id, status, lease_until, sequence_id)",
	} {
		if !strings.Contains(ddl, token) {
			t.Errorf("MySQL canonical migration is missing %q", token)
		}
	}
}

func TestValidateCreateImageGenerationBatch(t *testing.T) {
	request := testCreateImageBatchRequest("imgb_0123456789abcdef", "imgt_0123456789abcdef", "imgt_fedcba9876543210")
	if err := validateCreateImageGenerationBatch(request); err != nil {
		t.Fatalf("valid create request rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*CreateImageGenerationBatchRequest)
	}{
		{name: "bad batch id", mutate: func(r *CreateImageGenerationBatchRequest) { r.BatchID = "batch" }},
		{name: "missing owner", mutate: func(r *CreateImageGenerationBatchRequest) { r.UserID = "" }},
		{name: "invalid request json", mutate: func(r *CreateImageGenerationBatchRequest) { r.RequestJSON = json.RawMessage(`{`) }},
		{name: "request missing schema version", mutate: func(r *CreateImageGenerationBatchRequest) { r.RequestJSON = json.RawMessage(`{"items":[]}`) }},
		{name: "invalid provider plan", mutate: func(r *CreateImageGenerationBatchRequest) { r.ProviderPlanJSON = json.RawMessage(`null`) }},
		{name: "provider plan missing schema version", mutate: func(r *CreateImageGenerationBatchRequest) { r.ProviderPlanJSON = json.RawMessage(`{"providers":[]}`) }},
		{name: "provider plan contains secret", mutate: func(r *CreateImageGenerationBatchRequest) {
			r.ProviderPlanJSON = json.RawMessage(`{"version":1,"apiKey":"must-not-persist"}`)
		}},
		{name: "no tasks", mutate: func(r *CreateImageGenerationBatchRequest) { r.Tasks = nil }},
		{name: "task owner mismatch", mutate: func(r *CreateImageGenerationBatchRequest) { r.Tasks[0].UserID = "other" }},
		{name: "task count zero", mutate: func(r *CreateImageGenerationBatchRequest) { r.Tasks[0].RequestedCount = 0 }},
		{name: "task count above four", mutate: func(r *CreateImageGenerationBatchRequest) { r.Tasks[0].RequestedCount = 5 }},
		{name: "requested total mismatch", mutate: func(r *CreateImageGenerationBatchRequest) { r.RequestedCount++ }},
		{name: "zero artifact byte limit", mutate: func(r *CreateImageGenerationBatchRequest) { r.ArtifactByteLimit = 0 }},
		{name: "duplicate task id", mutate: func(r *CreateImageGenerationBatchRequest) { r.Tasks[1].ID = r.Tasks[0].ID }},
		{name: "duplicate chunk", mutate: func(r *CreateImageGenerationBatchRequest) {
			r.Tasks[1].ItemIndex, r.Tasks[1].ChunkIndex = r.Tasks[0].ItemIndex, r.Tasks[0].ChunkIndex
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			copy := request
			copy.Tasks = append([]CreateImageGenerationTaskRequest(nil), request.Tasks...)
			tt.mutate(&copy)
			if err := validateCreateImageGenerationBatch(copy); err == nil {
				t.Fatalf("invalid create request accepted: %+v", copy)
			}
		})
	}
}

func TestComputeImageBatchAggregate(t *testing.T) {
	tests := []struct {
		name            string
		cancelRequested bool
		tasks           []ImageGenerationTaskRecord
		wantStatus      ImageGenerationBatchStatus
		wantSucceeded   int
		wantFailed      int
		wantCanceled    int
	}{
		{name: "pending", tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskPending, RequestedCount: 4}}, wantStatus: ImageGenerationBatchPending},
		{name: "running", tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskRunning, RequestedCount: 4}}, wantStatus: ImageGenerationBatchRunning},
		{name: "progress with pending work", tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskDone, RequestedCount: 4}, {Status: ImageGenerationTaskPending, RequestedCount: 1}}, wantStatus: ImageGenerationBatchRunning, wantSucceeded: 4},
		{name: "done", tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskDone, RequestedCount: 4}}, wantStatus: ImageGenerationBatchDone, wantSucceeded: 4},
		{name: "partial", tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskDone, RequestedCount: 4}, {Status: ImageGenerationTaskFailed, RequestedCount: 1}}, wantStatus: ImageGenerationBatchPartial, wantSucceeded: 4, wantFailed: 1},
		{name: "failed", tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskFailed, RequestedCount: 3}}, wantStatus: ImageGenerationBatchFailed, wantFailed: 3},
		{name: "canceling", cancelRequested: true, tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskRunning, RequestedCount: 2}, {Status: ImageGenerationTaskCanceled, RequestedCount: 1}}, wantStatus: ImageGenerationBatchCanceling, wantCanceled: 1},
		{name: "canceled", cancelRequested: true, tasks: []ImageGenerationTaskRecord{{Status: ImageGenerationTaskDone, RequestedCount: 2}, {Status: ImageGenerationTaskCanceled, RequestedCount: 1}}, wantStatus: ImageGenerationBatchCanceled, wantSucceeded: 2, wantCanceled: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeImageBatchAggregate(tt.cancelRequested, tt.tasks)
			if got.Status != tt.wantStatus || got.SucceededCount != tt.wantSucceeded || got.FailedCount != tt.wantFailed || got.CanceledCount != tt.wantCanceled {
				t.Fatalf("aggregate = %+v", got)
			}
		})
	}
}

func testCreateImageBatchRequest(batchID string, taskIDs ...string) CreateImageGenerationBatchRequest {
	tasks := make([]CreateImageGenerationTaskRequest, len(taskIDs))
	total := 0
	for i, id := range taskIDs {
		count := 1
		if i == 0 {
			count = 4
		}
		total += count
		tasks[i] = CreateImageGenerationTaskRequest{
			ID: id, UserID: "user-a", ItemIndex: i, ChunkIndex: 0,
			Label: "item", Prompt: "prompt", Size: "square", RequestedCount: count,
			RequestFingerprint: strings.Repeat(string(rune('a'+i)), 64),
		}
	}
	return CreateImageGenerationBatchRequest{
		BatchID: batchID, UserID: "user-a", ConfigUserID: "config-a",
		AgentOwnerUserID: "owner-a", AgentID: "agent-a",
		WorkspaceProjectID: "project-a", WorkspaceSessionID: "session-a",
		RequestJSON:      json.RawMessage(`{"version":1,"items":[]}`),
		ProviderPlanJSON: json.RawMessage(`{"version":1,"providers":[]}`),
		RequestedCount:   total, MaxRetries: 3, Tasks: tasks,
		ArtifactByteLimit: 128 << 20,
	}
}

func TestImageArtifactSummaryRequiresPositiveBoundedSizes(t *testing.T) {
	count, total, ok := imageArtifactSummary(json.RawMessage(`[{"size":2},{"size":3}]`))
	if !ok || count != 2 || total != 5 {
		t.Fatalf("summary count=%d total=%d ok=%t", count, total, ok)
	}
	for _, raw := range []string{`[{"size":0}]`, `[{"size":-1}]`, `[{}]`} {
		if _, _, ok := imageArtifactSummary(json.RawMessage(raw)); ok {
			t.Fatalf("invalid artifact sizes accepted: %s", raw)
		}
	}
}
