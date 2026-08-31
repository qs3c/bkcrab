package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParserEvalDraftReservationAndLimits(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	run := newParserEvalRun("per_draft", time.Now().UTC())
	if err := st.CreateParserEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	doc1 := newParserEvalDocument(run.ID, "ped_one", 10)
	if err := st.ReserveParserEvalDocument(ctx, doc1, 2, 25); err != nil {
		t.Fatal(err)
	}
	if doc1.Ordinal != 1 || doc1.SourceObjectKey != "" {
		t.Fatalf("reserved document=%+v", doc1)
	}
	if ok, err := st.CompleteParserEvalDocumentUpload(ctx, run.ID, doc1.ID, "parser-eval/runs/per_draft/documents/ped_one/source.bin"); err != nil || !ok {
		t.Fatalf("complete upload=%v err=%v", ok, err)
	}
	doc2 := newParserEvalDocument(run.ID, "ped_two", 15)
	if err := st.ReserveParserEvalDocument(ctx, doc2, 2, 25); err != nil {
		t.Fatal(err)
	}
	if doc2.Ordinal != 2 {
		t.Fatalf("second ordinal=%d", doc2.Ordinal)
	}
	if err := st.ReserveParserEvalDocument(ctx, newParserEvalDocument(run.ID, "ped_three", 1), 2, 25); !errors.Is(err, ErrParserEvalLimit) {
		t.Fatalf("file limit error=%v", err)
	}
	if ok, err := st.AbortParserEvalDocumentUpload(ctx, run.ID, doc2.ID); err != nil || !ok {
		t.Fatalf("abort reservation=%v err=%v", ok, err)
	}
	if _, err := st.GetParserEvalDocument(ctx, run.ID, doc2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("aborted reservation remains: %v", err)
	}
	if ok, err := st.DeleteParserEvalDraftDocument(ctx, run.ID, doc1.ID); err != nil || !ok {
		t.Fatalf("delete draft document=%v err=%v", ok, err)
	}
}

func TestParserEvalStartLeaseFenceRetryAndPurge(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	created := time.Now().UTC().Add(-time.Hour)
	run := newParserEvalRun("per_lifecycle", created)
	if err := st.CreateParserEvalRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	doc := newParserEvalDocument(run.ID, "ped_lifecycle", 10)
	if err := st.ReserveParserEvalDocument(ctx, doc, 50, 100); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.StartParserEvalRun(ctx, run.ID, "admin", `{"frozen":true}`, `{"documentsTotal":1}`); ok || !errors.Is(err, ErrParserEvalIncomplete) {
		t.Fatalf("start incomplete=%v err=%v", ok, err)
	}
	if ok, err := st.CompleteParserEvalDocumentUpload(ctx, run.ID, doc.ID, "parser-eval/runs/per_lifecycle/documents/ped_lifecycle/source.bin"); err != nil || !ok {
		t.Fatalf("complete=%v err=%v", ok, err)
	}
	if ok, err := st.StartParserEvalRun(ctx, run.ID, "admin", `{"frozen":true}`, `{"documentsTotal":1}`); err != nil || !ok {
		t.Fatalf("start=%v err=%v", ok, err)
	}

	now := time.Now().UTC()
	lease1, ok, err := st.ClaimParserEvalRun(ctx, "worker-1", now, time.Minute)
	if err != nil || !ok || lease1.RunID != run.ID || lease1.FenceToken != 1 {
		t.Fatalf("claim1=%+v ok=%v err=%v", lease1, ok, err)
	}
	if alive, err := st.HeartbeatParserEvalRun(ctx, *lease1, now.Add(10*time.Second), time.Minute); err != nil || !alive {
		t.Fatalf("heartbeat=%v err=%v", alive, err)
	}
	lease2, ok, err := st.ClaimParserEvalRun(ctx, "worker-2", now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || lease2.FenceToken != 2 {
		t.Fatalf("claim2=%+v ok=%v err=%v", lease2, ok, err)
	}
	if alive, err := st.HeartbeatParserEvalRun(ctx, *lease1, now.Add(2*time.Minute), time.Minute); err != nil || alive {
		t.Fatalf("stale heartbeat=%v err=%v", alive, err)
	}

	success := `{"status":"SUCCEEDED","markdown":{"objectKey":"x"}}`
	failed := `{"status":"FAILED","error":{"code":"boom"}}`
	update := ParserEvalDocumentUpdate{
		DocumentID: doc.ID, Status: ParserEvalDocumentPartial, Stage: ParserEvalDocumentStageScoring,
		MarkItDownResultJSON: success, AnyDocResultJSON: failed,
	}
	if ok, err := st.PutParserEvalDocumentResults(ctx, *lease2, update); err != nil || !ok {
		t.Fatalf("put results=%v err=%v", ok, err)
	}
	update.MarkItDownResultJSON = failed
	if ok, err := st.PutParserEvalDocumentResults(ctx, *lease2, update); err != nil || !ok {
		t.Fatalf("repeat results=%v err=%v", ok, err)
	}
	storedDoc, err := st.GetParserEvalDocument(ctx, run.ID, doc.ID)
	if err != nil || storedDoc.MarkItDownResultJSON != success {
		t.Fatalf("successful slot overwritten: doc=%+v err=%v", storedDoc, err)
	}

	finish := ParserEvalRunFinish{
		Status: ParserEvalRunPartial, Stage: ParserEvalRunStageAggregating,
		ProgressJSON: `{"documentsTotal":1,"documentsCompleted":1}`, SummaryJSON: `{"wins":{}}`,
	}
	if ok, err := st.FinishParserEvalRun(ctx, *lease2, finish, now.Add(3*time.Minute)); err != nil || !ok {
		t.Fatalf("finish=%v err=%v", ok, err)
	}
	if ok, err := st.FinishParserEvalRun(ctx, *lease1, finish, now.Add(3*time.Minute)); err != nil || ok {
		t.Fatalf("stale finish=%v err=%v", ok, err)
	}
	if ok, err := st.RequeueParserEvalFailures(ctx, run.ID, now.Add(4*time.Minute)); err != nil || !ok {
		t.Fatalf("requeue=%v err=%v", ok, err)
	}
	requeuedDoc, err := st.GetParserEvalDocument(ctx, run.ID, doc.ID)
	if err != nil || requeuedDoc.MarkItDownResultJSON != success || requeuedDoc.AnyDocResultJSON != failed || requeuedDoc.Status != ParserEvalDocumentUploaded {
		t.Fatalf("requeued document=%+v err=%v", requeuedDoc, err)
	}

	if _, err := st.DB().ExecContext(ctx, `UPDATE parser_eval_runs SET status=?,expires_at=? WHERE id=?`, ParserEvalRunFailed, now.Add(-time.Minute), run.ID); err != nil {
		t.Fatal(err)
	}
	expired, err := st.ListExpiredParserEvalRuns(ctx, now, 10)
	if err != nil || len(expired) != 1 || expired[0].ID != run.ID {
		t.Fatalf("expired=%+v err=%v", expired, err)
	}
	if ok, err := st.PurgeParserEvalRun(ctx, run.ID); err != nil || !ok {
		t.Fatalf("purge=%v err=%v", ok, err)
	}
	if _, err := st.GetParserEvalRun(ctx, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("purged run remains: %v", err)
	}
}

func TestParserEvalCancelAndListOrdering(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	ctx := context.Background()
	first := newParserEvalRun("per_first", time.Now().UTC().Add(-time.Minute))
	second := newParserEvalRun("per_second", time.Now().UTC())
	if err := st.CreateParserEvalRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateParserEvalRun(ctx, second); err != nil {
		t.Fatal(err)
	}
	items, err := st.ListParserEvalRuns(ctx, "", 10)
	if err != nil || len(items) != 2 || items[0].ID != second.ID || items[1].ID != first.ID {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	page, err := st.ListParserEvalRuns(ctx, items[0].ID, 10)
	if err != nil || len(page) != 1 || page[0].ID != first.ID {
		t.Fatalf("cursor page=%+v err=%v", page, err)
	}
	if ok, err := st.RequestCancelParserEvalRun(ctx, second.ID, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("cancel draft=%v err=%v", ok, err)
	}
	got, err := st.GetParserEvalRun(ctx, second.ID)
	if err != nil || got.Status != ParserEvalRunCancelled || !got.CancelRequestedAt.Valid {
		t.Fatalf("cancelled run=%+v err=%v", got, err)
	}
}

func newParserEvalRun(id string, created time.Time) *ParserEvalRunRecord {
	return &ParserEvalRunRecord{
		ID: id, Status: ParserEvalRunDraft, Stage: ParserEvalRunStageUploading,
		ProgressJSON: `{}`, ExecutionSnapshotJSON: `{}`, SummaryJSON: `{}`,
		CreatedBy: "admin", CreatedAt: created, UpdatedAt: created, ExpiresAt: created.Add(90 * 24 * time.Hour),
	}
}

func newParserEvalDocument(runID, id string, size int64) *ParserEvalDocumentRecord {
	return &ParserEvalDocumentRecord{
		ID: id, RunID: runID, FileName: id + ".docx", Format: "docx",
		MediaType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		SizeBytes: size, SHA256: strings.Repeat("a", 64), Status: ParserEvalDocumentUploaded,
		Stage: ParserEvalDocumentStageValidating, TruthJSON: `{"status":"PENDING"}`,
		MarkItDownResultJSON: `{"status":"PENDING"}`, AnyDocResultJSON: `{"status":"PENDING"}`,
		JudgeResultJSON: `{"status":"PENDING"}`,
	}
}
