package imagegen

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

type unavailableIntegrationDispatcher struct{ calls int }

func (d *unavailableIntegrationDispatcher) TryDispatch(context.Context, string) error {
	d.calls++
	return errors.New("rabbit unavailable")
}

func TestIntegrationDurableCreatePlannerAuthorizationCancelAndSanitization(t *testing.T) {
	db := integrationMySQLStore(t)
	identity, err := db.ReadFairQueueWriterIdentity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	fair, err := db.BindImageFairQueueWriter(identity.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &unavailableIntegrationDispatcher{}
	var batchID string
	service := NewBatchService(BatchServiceOptions{
		Store: fair, ProviderPlans: integrationPlanResolver{}, Dispatcher: dispatcher,
		IDGenerator: func(kind string, _ int) string {
			id := integrationID(t, map[bool]string{true: "imgb_", false: "imgt_"}[kind == "batch"])
			if kind == "batch" {
				batchID = id
			}
			return id
		},
		MaxImagesPerBatch: 16, MaxImagesPerTask: 4, MaxItems: 16, PromptMaxRunes: 8000, WaitMaxSeconds: 240,
	})
	t.Cleanup(func() { cleanupIntegrationBatch(t, db, batchID) })
	request := integrationRequest(t, `{"items":[{"label":"A","prompt":"prompt-sentinel-A","count":5},{"label":"B","prompt":"prompt-sentinel-B","count":3},{"label":"C","prompt":"prompt-sentinel-C","count":1}],"wait_seconds":0}`)
	created, err := service.Create(context.Background(), integrationIdentity("integration-user", "agent-a"), request)
	if err != nil {
		t.Fatal(err)
	}
	if dispatcher.calls != 1 || created.Status != BatchStatusPending || len(created.Tasks) != 4 {
		t.Fatalf("durable create during Rabbit outage = %+v dispatch=%d", created, dispatcher.calls)
	}
	wantCounts := []int{4, 1, 3, 1}
	for i, task := range created.Tasks {
		if task.RequestedCount != wantCounts[i] {
			t.Fatalf("task counts=%+v", created.Tasks)
		}
	}
	if _, err := service.Status(context.Background(), integrationIdentity("integration-user", "agent-b"), batchID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("different agent authorization error=%v", err)
	}
	crossSession := integrationIdentity("integration-user", "agent-a")
	crossSession.WorkspaceSessionID = "new-session"
	status, err := service.Status(context.Background(), crossSession, batchID)
	if err != nil || status.BatchID != batchID {
		t.Fatalf("cross-session status=%+v err=%v", status, err)
	}
	canceled, err := service.Cancel(context.Background(), crossSession, batchID)
	if err != nil || !canceled.CancelRequested {
		t.Fatalf("cancel=%+v err=%v", canceled, err)
	}
	repeated, err := service.Cancel(context.Background(), crossSession, batchID)
	if err != nil || repeated.Status != BatchStatusCanceled {
		t.Fatalf("repeat cancel=%+v err=%v", repeated, err)
	}

	var planText string
	if err := db.DB().QueryRow(`SELECT CAST(provider_plan_json AS CHAR) FROM image_generation_batches WHERE id=?`, batchID).Scan(&planText); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"api-key-sentinel", "Authorization", "data:image", "secret="} {
		if strings.Contains(planText, forbidden) {
			t.Fatalf("provider plan leaked %q: %s", forbidden, planText)
		}
	}
}

func TestIntegrationRejectsSeventeenBeforeAnyDurableRow(t *testing.T) {
	db := integrationMySQLStore(t)
	before := 0
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM image_generation_batches`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	_, err := NormalizeRequest([]byte(`{"prompt":"too many","count":17}`), RequestLimits{MaxImagesPerBatch: 16, MaxItems: 16, PromptMaxRunes: 8000, RequestMaxBytes: 128 << 10, WaitMaxSeconds: 240})
	if err == nil {
		t.Fatal("count=17 passed normalization")
	}
	after := 0
	if err := db.DB().QueryRow(`SELECT COUNT(*) FROM image_generation_batches`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("rejected request changed durable rows: before=%d after=%d", before, after)
	}
}
