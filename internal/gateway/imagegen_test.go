package gateway

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	agenttools "github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type imagegenNotifierStore struct {
	tasks []store.ImageGenerationTaskRecord
}

func (s imagegenNotifierStore) ListImageGenerationTasks(context.Context, string) ([]store.ImageGenerationTaskRecord, error) {
	return append([]store.ImageGenerationTaskRecord(nil), s.tasks...), nil
}

func TestImagegenBatchTrustedArtifactsUsePersistedOriginAndStableOrder(t *testing.T) {
	ws := workspace.NewLocalFS(t.TempDir())
	refs := []agenttools.ImageArtifactRef{
		{BatchID: "imgb_0000000000000001", TaskID: "imgt_0000000000000001", ItemIndex: 0, Index: 0, Path: "imagegen/imgb_0000000000000001/imgt_0000000000000001/claims/1/first.png", MIMEType: "image/png"},
		{BatchID: "imgb_0000000000000001", TaskID: "imgt_0000000000000002", ItemIndex: 1, Index: 0, Path: "imagegen/imgb_0000000000000001/imgt_0000000000000002/claims/1/second.png", MIMEType: "image/png"},
	}
	for i := range refs {
		refs[i].Origin.AgentID = "agent-origin"
		refs[i].Origin.ProjectID = "project-origin"
		refs[i].Origin.SessionID = "session-origin"
		data := []byte{byte(i + 1), byte(i + 2)}
		if err := ws.Put(context.Background(), refs[i].Origin.AgentID, refs[i].Origin.ProjectID, refs[i].Origin.SessionID, refs[i].Path, strings.NewReader(string(data)), int64(len(data)), refs[i].MIMEType); err != nil {
			t.Fatal(err)
		}
	}
	items := appendTrustedImageArtifacts(context.Background(), ws, map[string]any{agenttools.ImageArtifactsMetadataKey: refs}, nil)
	if len(items) != 2 || items[0].Filename != "first.png" || items[1].Filename != "second.png" || items[0].Bytes[0] != 1 || items[1].Bytes[0] != 2 {
		t.Fatalf("trusted media order/origin = %#v", items)
	}
}

type imagegenNotifierTarget struct {
	calls []string
	fail  string
}

func (t *imagegenNotifierTarget) TryDispatch(_ context.Context, resource, taskID string) (bool, error) {
	t.calls = append(t.calls, resource+":"+taskID)
	if taskID == t.fail {
		return false, errors.New("recovering")
	}
	return true, nil
}

func TestImagegenBatchRuntimeModeGate(t *testing.T) {
	if imageBatchRuntimeEnabled(config.ImagegenBatchModeLegacy) {
		t.Fatal("legacy mode starts image resource runtime")
	}
	for _, mode := range []config.ImagegenBatchMode{config.ImagegenBatchModeFair, config.ImagegenBatchModeDrain} {
		if !imageBatchRuntimeEnabled(mode) {
			t.Fatalf("mode %s did not start image runtime", mode)
		}
	}
}

func TestImagegenBatchNotifierDispatchesAllDurableTasksDuringRecovery(t *testing.T) {
	target := &imagegenNotifierTarget{fail: "imgt_0000000000000002"}
	notifier := imageBatchNotifier{
		store:  imagegenNotifierStore{tasks: []store.ImageGenerationTaskRecord{{ID: "imgt_0000000000000001"}, {ID: "imgt_0000000000000002"}, {ID: "imgt_0000000000000003"}}},
		target: target,
	}
	if err := notifier.TryDispatch(context.Background(), "imgb_0000000000000001"); err == nil {
		t.Fatal("recovering dispatch error was hidden")
	}
	want := []string{
		store.ImageGenerationResource + ":imgt_0000000000000001",
		store.ImageGenerationResource + ":imgt_0000000000000002",
		store.ImageGenerationResource + ":imgt_0000000000000003",
	}
	if !reflect.DeepEqual(target.calls, want) {
		t.Fatalf("dispatch calls = %v, want %v", target.calls, want)
	}
}
