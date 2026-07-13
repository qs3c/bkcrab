package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func acquireMutationTestJob(t *testing.T, db *DBStore, session string) (*SkillExtractionJob, string) {
	t.Helper()
	saveJobSnapshot(t, db, session, "mutation material")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	if job == nil {
		t.Fatal("expected cadence job")
	}
	worker := "mutation-worker-" + session
	leased, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, worker, time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire job: ok=%v err=%v job=%+v", ok, err, leased)
	}
	return leased, worker
}

func createMutationIntent(slug, content string) SkillExtractionMutationIntent {
	return SkillExtractionMutationIntent{
		Action: "create", Slug: slug, AfterHash: HashSkillContent(content), DesiredContent: content,
	}
}

func TestPrepareSkillExtractionMutationIsOneDurableIntent(t *testing.T) {
	db := newTestSQLite(t)
	job, worker := acquireMutationTestJob(t, db, "prepare")
	if _, err := db.GetSkillExtractionMutation(context.Background(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("receipt before prepare err=%v, want ErrNotFound", err)
	}

	intent := createMutationIntent("deploy-service", "first content")
	first, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, intent)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if first.Status != SkillExtractionMutationPrepared || first.AgentID != jobAgent || first.DesiredContent != intent.DesiredContent {
		t.Fatalf("prepared receipt = %+v", first)
	}
	var snapshot string
	if err := db.db.QueryRow(`SELECT snapshot_json FROM skill_extraction_jobs WHERE id=?`, job.ID).Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot != "[]" {
		t.Fatalf("snapshot retained after durable intent: %q", snapshot)
	}

	second, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, intent)
	if err != nil {
		t.Fatalf("idempotent prepare: %v", err)
	}
	if second.JobID != first.JobID || second.AfterHash != first.AfterHash || second.Status != SkillExtractionMutationPrepared {
		t.Fatalf("idempotent receipt changed: first=%+v second=%+v", first, second)
	}
	different := createMutationIntent("another-service", "second content")
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, different); err == nil || !strings.Contains(err.Error(), "different mutation") {
		t.Fatalf("different second intent err=%v", err)
	}
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM skill_extraction_mutations WHERE job_id=?`, job.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("receipt count=%d err=%v", count, err)
	}
}

func TestPrepareSkillExtractionMutationValidatesLeaseHashAndNoOp(t *testing.T) {
	db := newTestSQLite(t)
	job, worker := acquireMutationTestJob(t, db, "validation")
	content := "updated content"
	intent := SkillExtractionMutationIntent{
		Action: "update", Slug: "existing", BeforeHash: HashSkillContent("old content"),
		AfterHash: HashSkillContent(content), DesiredContent: content,
	}
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, "wrong-worker", intent); err == nil || !strings.Contains(err.Error(), "not leased") {
		t.Fatalf("wrong worker err=%v", err)
	}
	badHash := intent
	badHash.AfterHash = HashSkillContent("different")
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, badHash); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("hash mismatch err=%v", err)
	}
	noOp := intent
	noOp.BeforeHash = noOp.AfterHash
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, noOp); err == nil || !strings.Contains(err.Error(), "no-op") {
		t.Fatalf("no-op update err=%v", err)
	}

	if _, err := db.db.Exec(`UPDATE skill_extraction_jobs SET lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Minute), job.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, intent); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired lease err=%v", err)
	}
}

func TestCommitSkillExtractionMutationAtomicallyAppliesLedgerAndScrubsContent(t *testing.T) {
	db := newTestSQLite(t)
	job, worker := acquireMutationTestJob(t, db, "commit")
	intent := createMutationIntent("new-workflow", "durable workflow")
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, intent); err != nil {
		t.Fatal(err)
	}

	applied, err := db.CommitSkillExtractionMutation(context.Background(), job.ID, worker)
	if err != nil {
		t.Fatalf("commit mutation: %v", err)
	}
	if applied.Status != SkillExtractionMutationApplied || applied.DesiredContent != "" || applied.ResolvedAt == nil {
		t.Fatalf("applied receipt = %+v", applied)
	}
	persisted, err := db.GetSkillExtractionMutation(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != SkillExtractionMutationApplied || persisted.DesiredContent != "" || persisted.AfterHash != intent.AfterHash {
		t.Fatalf("persisted receipt = %+v", persisted)
	}
	rows, err := db.ListSkillUsage(context.Background(), jobAgent)
	if err != nil || len(rows) != 1 || rows[0].Slug != intent.Slug || rows[0].ContentHash != intent.AfterHash {
		t.Fatalf("ledger rows=%+v err=%v", rows, err)
	}
	createdSeq := rows[0].CreatedSeq
	if _, err := db.CommitSkillExtractionMutation(context.Background(), job.ID, worker); err != nil {
		t.Fatalf("idempotent commit: %v", err)
	}
	rows, err = db.ListSkillUsage(context.Background(), jobAgent)
	if err != nil || len(rows) != 1 || rows[0].CreatedSeq != createdSeq {
		t.Fatalf("idempotent ledger rows=%+v err=%v", rows, err)
	}
}

func TestCommitUpdateMutationStampsEditedSequence(t *testing.T) {
	db := newTestSQLite(t)
	if err := db.UpsertSkillUsage(context.Background(), jobAgent, "existing", HashSkillContent("old"), true); err != nil {
		t.Fatal(err)
	}
	job, worker := acquireMutationTestJob(t, db, "commit-update")
	content := "new"
	intent := SkillExtractionMutationIntent{
		Action: "update", Slug: "existing", BeforeHash: HashSkillContent("old"),
		AfterHash: HashSkillContent(content), DesiredContent: content,
	}
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitSkillExtractionMutation(context.Background(), job.ID, worker); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListSkillUsage(context.Background(), jobAgent)
	if err != nil || len(rows) != 1 || rows[0].EditedSeq == 0 || rows[0].ContentHash != intent.AfterHash {
		t.Fatalf("updated ledger rows=%+v err=%v", rows, err)
	}
}

func TestConflictSkillExtractionMutationScrubsContentWithoutLedgerWrite(t *testing.T) {
	db := newTestSQLite(t)
	job, worker := acquireMutationTestJob(t, db, "conflict")
	intent := createMutationIntent("conflicted", "private desired content")
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, intent); err != nil {
		t.Fatal(err)
	}
	receipt, err := db.ConflictSkillExtractionMutation(context.Background(), job.ID, worker, "authoritative asset has a divergent hash")
	if err != nil {
		t.Fatalf("mark conflict: %v", err)
	}
	if receipt.Status != SkillExtractionMutationConflict || receipt.DesiredContent != "" || receipt.LastError == "" || receipt.ResolvedAt == nil {
		t.Fatalf("conflict receipt=%+v", receipt)
	}
	persisted, err := db.GetSkillExtractionMutation(context.Background(), job.ID)
	if err != nil || persisted.DesiredContent != "" || persisted.Status != SkillExtractionMutationConflict {
		t.Fatalf("persisted conflict=%+v err=%v", persisted, err)
	}
	rows, err := db.ListSkillUsage(context.Background(), jobAgent)
	if err != nil || len(rows) != 0 {
		t.Fatalf("conflict wrote ledger rows=%+v err=%v", rows, err)
	}
	if _, err := db.CommitSkillExtractionMutation(context.Background(), job.ID, worker); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("commit conflicted receipt err=%v", err)
	}
}

func TestPrepareSkillExtractionMutationRefusesDeletingAgent(t *testing.T) {
	t.Run("before prepare", func(t *testing.T) {
		db := newTestSQLite(t)
		job, worker := acquireMutationTestJob(t, db, "deleting-before-prepare")
		if err := db.MarkAgentDeleting(context.Background(), job.AgentID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, createMutationIntent("late", "late content")); err == nil || !strings.Contains(err.Error(), "being deleted") {
			t.Fatalf("prepare during deletion err=%v", err)
		}
	})
	t.Run("before ledger commit", func(t *testing.T) {
		db := newTestSQLite(t)
		job, worker := acquireMutationTestJob(t, db, "deleting-before-commit")
		if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, createMutationIntent("late", "late content")); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkAgentDeleting(context.Background(), job.AgentID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitSkillExtractionMutation(context.Background(), job.ID, worker); err == nil || !strings.Contains(err.Error(), "being deleted") {
			t.Fatalf("commit during deletion err=%v", err)
		}
	})
}

func TestDeleteAgentRemovesSkillExtractionMutationReceipt(t *testing.T) {
	db := newTestSQLite(t)
	job, worker := acquireMutationTestJob(t, db, "delete-receipt")
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, worker, createMutationIntent("delete-me", "temporary content")); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAgent(context.Background(), job.AgentID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSkillExtractionMutation(context.Background(), job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("receipt survived agent deletion: %v", err)
	}
}
