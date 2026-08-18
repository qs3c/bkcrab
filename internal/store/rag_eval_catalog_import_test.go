package store

import (
	"context"
	"testing"
	"time"
)

func TestRAGEvalCatalogImportReservesVersionsAndUsesFencedLifecycle(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	dataset := &RAGEvalDatasetRecord{Name: "catalog", CreatedBy: "admin"}
	if err := st.CreateRAGEvalDataset(ctx, dataset); err != nil {
		t.Fatal(err)
	}
	first := &RAGEvalCatalogImportRecord{DatasetID: dataset.ID, CatalogID: "next-tat-tatqa", RequestJSON: `{}`, CreatedBy: "admin"}
	second := &RAGEvalCatalogImportRecord{DatasetID: dataset.ID, CatalogID: "next-tat-tatqa", RequestJSON: `{}`, CreatedBy: "admin"}
	if err := st.CreateRAGEvalCatalogImport(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateRAGEvalCatalogImport(ctx, second); err != nil {
		t.Fatal(err)
	}
	if first.TargetVersion != 1 || second.TargetVersion != 2 {
		t.Fatalf("reserved versions=%d,%d", first.TargetVersion, second.TargetVersion)
	}
	fence, claimed, err := st.ClaimRAGEvalCatalogImport(ctx, first.ID, "worker", time.Minute)
	if err != nil || !claimed {
		t.Fatalf("claim=%v fence=%+v err=%v", claimed, fence, err)
	}
	if changed, err := st.UpdateRAGEvalCatalogImport(ctx, *fence, "downloading", `{"files":1}`); err != nil || !changed {
		t.Fatalf("progress=%v err=%v", changed, err)
	}
	if changed, err := st.RequestCancelRAGEvalCatalogImport(ctx, first.ID); err != nil || !changed {
		t.Fatalf("cancel running=%v err=%v", changed, err)
	}
	if changed, err := st.FinishRAGEvalCatalogImport(ctx, *fence, RAGEvalRunCancelled, "", "cancelled", "cancelled"); err != nil || !changed {
		t.Fatalf("finish cancel=%v err=%v", changed, err)
	}
	stored, err := st.GetRAGEvalCatalogImport(ctx, first.ID)
	if err != nil || stored.Status != RAGEvalRunCancelled || !stored.CancelRequestedAt.Valid {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	if changed, err := st.RequestCancelRAGEvalCatalogImport(ctx, second.ID); err != nil || !changed {
		t.Fatalf("cancel queued=%v err=%v", changed, err)
	}
	stored, err = st.GetRAGEvalCatalogImport(ctx, second.ID)
	if err != nil || stored.Status != RAGEvalRunCancelled || !stored.FinishedAt.Valid {
		t.Fatalf("queued cancellation=%+v err=%v", stored, err)
	}
}
