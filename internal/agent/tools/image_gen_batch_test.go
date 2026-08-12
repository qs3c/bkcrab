package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/imagegen"
)

type fakeImagegenBatchService struct {
	createRequest imagegen.NormalizedRequest
	identity      imagegen.ExecutionIdentity
	createCalls   int
	statusCalls   int
	cancelCalls   int
	result        imagegen.BatchResult
}

func (s *fakeImagegenBatchService) Create(_ context.Context, identity imagegen.ExecutionIdentity, request imagegen.NormalizedRequest) (imagegen.BatchResult, error) {
	s.createCalls++
	s.identity, s.createRequest = identity, request
	return s.result, nil
}

func (s *fakeImagegenBatchService) Status(_ context.Context, identity imagegen.ExecutionIdentity, _ string) (imagegen.BatchResult, error) {
	s.statusCalls++
	s.identity = identity
	return s.result, nil
}

func (s *fakeImagegenBatchService) Cancel(_ context.Context, identity imagegen.ExecutionIdentity, _ string) (imagegen.BatchResult, error) {
	s.cancelCalls++
	s.identity = identity
	return s.result, nil
}

func imagegenBatchTestRegistry() *Registry {
	r := NewRegistry(tTempRoot(), tTempRoot())
	r.SetWorkspaceStore(nil, "agent_1")
	r.SetOwnerUserID("config-user")
	r.SetAgentOwnerUserID("owner-user")
	r.SetChatterUserID("caller-user")
	r.SetProjectID("project-1")
	r.SetSessionID("session-1")
	r.SetMessageContext("web", "chat-1")
	return r
}

func tTempRoot() string { return "." }

func testImagegenBatchConfig(mode config.ImagegenBatchMode) config.ImagegenBatchCfg {
	cfg := config.DefaultImagegenBatchCfg()
	cfg.Mode = mode
	return cfg
}

func TestImageGenBatchStrictActionsAndTrustedIdentity(t *testing.T) {
	batchID := "imgb_0000000000000001"
	svc := &fakeImagegenBatchService{result: imagegen.BatchResult{BatchID: batchID, Status: imagegen.BatchStatusPending, CreatedAt: time.Unix(1, 0), UpdatedAt: time.Unix(1, 0)}}
	r := imagegenBatchTestRegistry()
	RegisterImageGenBatch(r, testImagegenBatchConfig(config.ImagegenBatchModeFair), svc)
	fn := r.GetResultFunc("image_gen_batch")
	if fn == nil {
		t.Fatal("image_gen_batch was not registered")
	}

	valid := []string{
		`{"prompt":"one","count":4,"wait_seconds":0}`,
		`{"action":"create","items":[{"label":"a","prompt":"one","count":4},{"label":"b","prompt":"two","count":1}]}`,
		`{"action":"status","batch_id":"` + batchID + `"}`,
		`{"action":"cancel","batch_id":"` + batchID + `"}`,
	}
	for _, raw := range valid {
		if _, err := fn(context.Background(), json.RawMessage(raw)); err != nil {
			t.Fatalf("valid request %s failed: %v", raw, err)
		}
	}
	if svc.createCalls != 2 || svc.statusCalls != 1 || svc.cancelCalls != 1 {
		t.Fatalf("calls create/status/cancel = %d/%d/%d", svc.createCalls, svc.statusCalls, svc.cancelCalls)
	}
	if svc.identity.UserID != "caller-user" || svc.identity.ConfigUserID != "config-user" || svc.identity.AgentOwnerUserID != "owner-user" || svc.identity.AgentID != "agent_1" || svc.identity.WorkspaceProjectID != "project-1" || svc.identity.WorkspaceSessionID != "session-1" {
		t.Fatalf("untrusted or incomplete identity: %+v", svc.identity)
	}

	invalid := []string{
		`{"prompt":"one","items":[{"prompt":"two"}]}`,
		`{"prompt":"one","count":17}`,
		`{"prompt":"one","wait_seconds":241}`,
		`{"prompt":"one","unknown":true}`,
		`{"prompt":`,
	}
	for _, raw := range invalid {
		if _, err := fn(context.Background(), json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid request passed: %s", raw)
		}
	}
}

func TestImageGenBatchRegistrationModes(t *testing.T) {
	for _, mode := range []config.ImagegenBatchMode{config.ImagegenBatchModeFair, config.ImagegenBatchModeDrain} {
		r := imagegenBatchTestRegistry()
		svc := &fakeImagegenBatchService{result: imagegen.BatchResult{BatchID: "imgb_0000000000000001"}}
		RegisterImageGenBatch(r, testImagegenBatchConfig(mode), svc)
		if r.GetFunc("image_gen_batch") == nil || r.GetFunc("image_gen") != nil {
			t.Fatalf("mode %s did not expose only image_gen_batch", mode)
		}
		_, err := r.GetFunc("image_gen_batch")(context.Background(), json.RawMessage(`{"prompt":"one"}`))
		if mode == config.ImagegenBatchModeDrain && (err == nil || !strings.Contains(err.Error(), "drain")) {
			t.Fatalf("drain create error = %v", err)
		}
		if mode == config.ImagegenBatchModeDrain && svc.createCalls != 0 {
			t.Fatal("drain mode called create service")
		}
	}

	r := imagegenBatchTestRegistry()
	RegisterImageGenBatch(r, testImagegenBatchConfig(config.ImagegenBatchModeLegacy), &fakeImagegenBatchService{})
	if r.GetFunc("image_gen_batch") != nil {
		t.Fatal("legacy mode exposed image_gen_batch")
	}
}

func TestImageGenBatchResultMetadataAndDescription(t *testing.T) {
	artifact := imagegen.BatchArtifactResult{
		BatchID: "imgb_0000000000000001", TaskID: "imgt_0000000000000001", ItemIndex: 0, ChunkIndex: 0, Index: 0,
		Path: "imagegen/imgb_0000000000000001/imgt_0000000000000001/claims/1/image-0-" + strings.Repeat("a", 64) + ".png",
		URL:  "https://provider.invalid/raw.png?secret=leak", MIMEType: "image/png", Size: 12, Width: 2, Height: 2,
		SHA256: strings.Repeat("a", 64), Origin: imagegen.ArtifactScope{AgentID: "agent_1", ProjectID: "project-1", SessionID: "old-session"},
	}
	svc := &fakeImagegenBatchService{result: imagegen.BatchResult{BatchID: artifact.BatchID, Status: imagegen.BatchStatusDone, Artifacts: []imagegen.BatchArtifactResult{artifact}}}
	r := imagegenBatchTestRegistry()
	RegisterImageGenBatch(r, testImagegenBatchConfig(config.ImagegenBatchModeFair), svc)
	result, err := r.GetResultFunc("image_gen_batch")(context.Background(), json.RawMessage(`{"action":"status","batch_id":"imgb_0000000000000001"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.Text, "provider.invalid") || strings.Contains(result.Text, "secret=") {
		t.Fatalf("provider URL leaked in result: %s", result.Text)
	}
	if len(result.Metadata[ImageArtifactsMetadataKey]) == 0 {
		t.Fatalf("artifact metadata missing: %v", result.Metadata)
	}
	for _, info := range r.RegisteredTools() {
		if info.Name == "image_gen_batch" && !strings.Contains(info.Description, "不要在同一轮高频轮询") {
			t.Fatalf("description lacks polling guidance: %q", info.Description)
		}
	}
}
