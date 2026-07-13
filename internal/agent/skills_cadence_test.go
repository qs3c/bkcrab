package agent

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func newCadenceFixture(t *testing.T, responses []*provider.Response) (*Agent, *store.DBStore, string, *learnerFakeProvider) {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + dir + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		db.Close()
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SaveAgent(context.Background(), &store.AgentRecord{ID: "agentA", UserID: "u1", Name: "Agent A"}); err != nil {
		db.Close()
		t.Fatalf("save agent: %v", err)
	}
	ws := t.TempDir()
	fp := &learnerFakeProvider{responses: responses}
	learner := NewSkillsLearner(ws, fp, "m")
	learner.agentID = "agentA"
	learner.ledger = db
	a := &Agent{
		name:             "agentA",
		ownerUserID:      "u1",
		agentOwnerUserID: "u1",
		dataStore:        db,
		skillsLearner:    learner,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = a.stopSkillJobs(ctx)
		cancel()
		db.Close()
	})
	return a, db, ws, fp
}

func seedAgentTurns(t *testing.T, db *store.DBStore, counts []int) *turnAnchor {
	t.Helper()
	ctx := store.WithChatterUserID(context.Background(), "u1")
	var last int64
	for _, c := range counts {
		seq, err := db.AppendTurnAnchor(ctx, "u1", "agentA", "s1", store.SessionMessage{Role: "user", Content: "archived turn"})
		if err != nil {
			t.Fatalf("anchor: %v", err)
		}
		if err := db.FinishTurn(ctx, "u1", "agentA", "s1", seq, c); err != nil {
			t.Fatalf("finish: %v", err)
		}
		last = seq
	}
	return &turnAnchor{sessionKey: "s1", seq: last}
}

func seedSessionRecord(t *testing.T, db *store.DBStore, msgs []store.SessionMessage) {
	t.Helper()
	ctx := store.WithChatterUserID(context.Background(), "u1")
	if err := db.SaveSession(ctx, "u1", "agentA", "s1", &store.SessionRecord{Messages: msgs}); err != nil {
		t.Fatalf("save session: %v", err)
	}
}

func enqueueCadenceJob(t *testing.T, db *store.DBStore, threshold int) *store.SkillExtractionJob {
	t.Helper()
	job, err := db.EnqueueSkillExtractionJob(context.Background(), "u1", "agentA", "s1", "u1", threshold)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return job
}

func prepareCrashedCadenceCreate(t *testing.T, db *store.DBStore, slug, content string) (*store.SkillExtractionJob, string) {
	t.Helper()
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, sessionMessagesFixture())
	job := enqueueCadenceJob(t, db, 10)
	if job == nil {
		t.Fatal("expected cadence job")
	}
	const crashedWorker = "crashed-worker"
	if _, acquired, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, crashedWorker, time.Minute); err != nil || !acquired {
		t.Fatalf("acquire crashed worker lease = (%v, %v)", acquired, err)
	}
	if _, err := db.PrepareSkillExtractionMutation(context.Background(), job.ID, crashedWorker, store.SkillExtractionMutationIntent{
		Action: "create", Slug: slug, AfterHash: store.HashSkillContent(content), DesiredContent: content,
	}); err != nil {
		t.Fatalf("prepare crashed mutation: %v", err)
	}
	return job, crashedWorker
}

func releaseCrashedCadenceWorker(t *testing.T, db *store.DBStore, jobID, workerID string, attemptCount int) {
	t.Helper()
	if err := db.RetrySkillExtractionJob(context.Background(), jobID, workerID, "simulated process crash"); err != nil {
		t.Fatalf("release crashed worker: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE skill_extraction_jobs SET next_attempt_at=?, attempt_count=? WHERE id=?`, time.Now().Add(-time.Minute), attemptCount, jobID); err != nil {
		t.Fatalf("make crashed job recoverable: %v", err)
	}
}

func waitForJobState(t *testing.T, db *store.DBStore, jobID, wantStatus string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var status, outcome string
		err := db.DB().QueryRow(`SELECT status, outcome FROM skill_extraction_jobs WHERE id=?`, jobID).Scan(&status, &outcome)
		if err == nil && status == wantStatus {
			return outcome
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", jobID, wantStatus)
	return ""
}

func TestSkillsCadenceBelowThresholdDoesNotQueueOrCallLLM(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	anchor := seedAgentTurns(t, db, []int{3, 4})
	seedSessionRecord(t, db, sessionMessagesFixture())
	a.maybeExtractSkillsCadence(context.Background(), anchor, "u1")
	if fp.calls != 0 {
		t.Fatalf("below threshold should not call LLM, calls=%d", fp.calls)
	}
	if job := enqueueCadenceJob(t, db, 10); job != nil {
		t.Fatalf("below-threshold cadence queued a job: %+v", job)
	}
}

func TestSkillsCadenceExtractsFrozenSessionAndWrites(t *testing.T) {
	a, db, ws, fp := newCadenceFixture(t, []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "create", "slug": "cadence-skill", "content": learnerValidSkill}),
		textResp("done"),
	})
	anchor := seedAgentTurns(t, db, []int{4, 6})
	fullSessionDetail := strings.Repeat("FULL_SESSION_WORKSET ", 100)
	seedSessionRecord(t, db, []store.SessionMessage{
		{Role: "assistant", Content: "compacted overview"},
		{Role: "user", Content: fullSessionDetail},
	})
	a.maybeExtractSkillsCadence(context.Background(), anchor, "u1")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := readSkill(t, ws, "cadence-skill"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := readSkill(t, ws, "cadence-skill"); !ok {
		t.Fatalf("cadence extraction timed out before writing the skill; provider calls=%d prompts=%q", fp.calls, fp.prompts)
	}
	if len(fp.prompts) == 0 || !strings.Contains(fp.prompts[0], fullSessionDetail) || !strings.Contains(fp.prompts[0], "compacted overview") {
		t.Fatalf("learner did not receive the complete sessions.messages snapshot: %#v", fp.prompts)
	}

	var jobID string
	if err := db.DB().QueryRow(`SELECT id FROM skill_extraction_jobs`).Scan(&jobID); err != nil {
		t.Fatal(err)
	}
	if outcome := waitForJobState(t, db, jobID, "completed"); outcome != SkillExtractionCreated {
		t.Fatalf("outcome=%q want create", outcome)
	}
}

func TestRunSkillExtractionJobReconcilesPreparedCreateWithoutCallingModel(t *testing.T) {
	a, db, wsDir, fp := newCadenceFixture(t, nil)
	remote := workspace.NewLocalFS(t.TempDir())
	a.workspaceStore = remote
	a.skillsLearner.workspaceStore = remote
	job, crashedWorker := prepareCrashedCadenceCreate(t, db, "prepared-create", learnerValidSkill)

	// Simulate a crash after the local atomic rename but before the first remote
	// namespace marker/object write and before the relational receipt commit.
	if err := a.skillsLearner.Manager().Create("prepared-create", learnerValidSkill); err != nil {
		t.Fatalf("seed local after-image: %v", err)
	}
	releaseCrashedCadenceWorker(t, db, job.ID, crashedWorker, 5)

	a.runSkillExtractionJob(context.Background(), job.ID)
	if fp.calls != 0 {
		t.Fatalf("prepared receipt called learner model %d times", fp.calls)
	}
	if outcome := waitForJobState(t, db, job.ID, "completed"); outcome != SkillExtractionCreated {
		t.Fatalf("outcome=%q want create", outcome)
	}
	if got, ok := readSkill(t, wsDir, "prepared-create"); !ok || got != learnerValidSkill {
		t.Fatalf("reconciled local skill = (%q, %v)", got, ok)
	}
	r, err := remote.Get(context.Background(), "agentA", "", "", "learner-skills/prepared-create/SKILL.md")
	if err != nil {
		t.Fatalf("read reconciled remote skill: %v", err)
	}
	remoteContent, readErr := io.ReadAll(r)
	_ = r.Close()
	if readErr != nil || string(remoteContent) != learnerValidSkill {
		t.Fatalf("reconciled remote skill = (%q, %v)", remoteContent, readErr)
	}
	receipt, err := db.GetSkillExtractionMutation(context.Background(), job.ID)
	if err != nil || receipt.Status != store.SkillExtractionMutationApplied || receipt.DesiredContent != "" {
		t.Fatalf("receipt after reconciliation = (%+v, %v)", receipt, err)
	}
}

func TestRunSkillExtractionJobCompletesAppliedReceiptWithoutCallingModel(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	job, crashedWorker := prepareCrashedCadenceCreate(t, db, "already-applied", learnerValidSkill)
	if err := a.skillsLearner.Manager().Create("already-applied", learnerValidSkill); err != nil {
		t.Fatalf("seed durable asset: %v", err)
	}
	if _, err := db.CommitSkillExtractionMutation(context.Background(), job.ID, crashedWorker); err != nil {
		t.Fatalf("commit receipt before simulated crash: %v", err)
	}
	releaseCrashedCadenceWorker(t, db, job.ID, crashedWorker, 1)

	a.runSkillExtractionJob(context.Background(), job.ID)
	if fp.calls != 0 {
		t.Fatalf("applied receipt called learner model %d times", fp.calls)
	}
	if outcome := waitForJobState(t, db, job.ID, "completed"); outcome != SkillExtractionCreated {
		t.Fatalf("outcome=%q want create", outcome)
	}
}

func TestRunSkillExtractionJobFailsPreparedDivergenceWithoutCallingModel(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	job, crashedWorker := prepareCrashedCadenceCreate(t, db, "prepared-conflict", learnerValidSkill)
	divergent := strings.Replace(learnerValidSkill, "second step", "different durable step", 1)
	if err := a.skillsLearner.Manager().Create("prepared-conflict", divergent); err != nil {
		t.Fatalf("seed divergent asset: %v", err)
	}
	releaseCrashedCadenceWorker(t, db, job.ID, crashedWorker, 1)

	a.runSkillExtractionJob(context.Background(), job.ID)
	if fp.calls != 0 {
		t.Fatalf("conflicted receipt called learner model %d times", fp.calls)
	}
	if outcome := waitForJobState(t, db, job.ID, "failed"); outcome != "failed" {
		t.Fatalf("outcome=%q want failed", outcome)
	}
	receipt, err := db.GetSkillExtractionMutation(context.Background(), job.ID)
	if err != nil || receipt.Status != store.SkillExtractionMutationConflict || receipt.DesiredContent != "" {
		t.Fatalf("conflict receipt = (%+v, %v)", receipt, err)
	}
	rows, err := db.ListSkillUsage(context.Background(), "agentA")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Slug == "prepared-conflict" {
			t.Fatalf("conflicted mutation wrote lifecycle ledger: %+v", row)
		}
	}
}

func TestRunSkillExtractionJobUsesFrozenSnapshotAfterSessionChanges(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, []*provider.Response{textResp("Nothing to save.")})
	seedAgentTurns(t, db, []int{10})
	original := strings.Repeat("ORIGINAL_FROZEN_MATERIAL ", 50)
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: original}})
	job := enqueueCadenceJob(t, db, 10)
	if job == nil {
		t.Fatal("expected job")
	}
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "later replacement"}})

	a.runSkillExtractionJob(context.Background(), job.ID)
	if len(fp.prompts) != 1 || !strings.Contains(fp.prompts[0], original) || strings.Contains(fp.prompts[0], "later replacement") {
		t.Fatalf("worker did not use frozen job snapshot: %#v", fp.prompts)
	}
	if outcome := waitForJobState(t, db, job.ID, "completed"); outcome != SkillExtractionSkipped {
		t.Fatalf("outcome=%q want skip", outcome)
	}
}

func TestRunSkillExtractionJobSurvivesSessionDeletion(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, []*provider.Response{textResp("Nothing to save.")})
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, sessionMessagesFixture())
	job := enqueueCadenceJob(t, db, 10)
	if err := db.DeleteSession(context.Background(), "u1", "agentA", "s1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	a.runSkillExtractionJob(context.Background(), job.ID)
	if fp.calls != 1 {
		t.Fatalf("frozen job should not depend on the live session, calls=%d", fp.calls)
	}
	waitForJobState(t, db, job.ID, "completed")
}

func TestRunSkillExtractionJobProviderErrorKeepsSamePendingJob(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	fp.err = errors.New("provider down")
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "stable frozen snapshot"}})
	job := enqueueCadenceJob(t, db, 10)
	a.runSkillExtractionJob(context.Background(), job.ID)

	var status, lastError, hash string
	var attempts int
	if err := db.DB().QueryRow(`SELECT status, last_error, snapshot_sha256, attempt_count
		FROM skill_extraction_jobs WHERE id=?`, job.ID).Scan(&status, &lastError, &hash, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || !strings.Contains(lastError, "provider down") || hash != job.SnapshotSHA256 || attempts != 1 {
		t.Fatalf("retry state status=%q error=%q hash=%q attempts=%d job=%+v", status, lastError, hash, attempts, job)
	}
	queued := enqueueCadenceJob(t, db, 10)
	if queued == nil || queued.ID != job.ID || queued.SnapshotSHA256 != job.SnapshotSHA256 {
		t.Fatalf("retry replaced the durable job: %+v", queued)
	}
}

func TestRunSkillExtractionJobQueuesWindowAccumulatedWhileRunning(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, []*provider.Response{
		textResp("Nothing to save."),
		textResp("Nothing to save."),
	})
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "first snapshot"}})
	first := enqueueCadenceJob(t, db, 10)
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "second snapshot"}})

	a.runSkillExtractionJob(context.Background(), first.ID)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var completed int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM skill_extraction_jobs WHERE status='completed'`).Scan(&completed); err == nil && completed == 2 {
			if fp.calls != 2 {
				t.Fatalf("provider calls=%d want 2", fp.calls)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("follow-up cadence window was not queued and completed")
}

func TestRunSkillExtractionJobHashMismatchFailsTerminally(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, sessionMessagesFixture())
	job := enqueueCadenceJob(t, db, 10)
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "window after corrupt job"}})
	// Keep the replacement valid and decodable JSON: integrity must come from
	// the persisted hash, not merely from JSON syntax/type validation.
	if _, err := db.DB().Exec(`UPDATE skill_extraction_jobs
		SET snapshot_json='[{"role":"user","content":"tampered but valid"}]' WHERE id=?`, job.ID); err != nil {
		t.Fatal(err)
	}
	a.runSkillExtractionJob(context.Background(), job.ID)
	if outcome := waitForJobState(t, db, job.ID, "failed"); outcome != "failed" {
		t.Fatalf("terminal outcome=%q want failed", outcome)
	}
	var lastError, snapshot string
	if err := db.DB().QueryRow(`SELECT last_error, snapshot_json FROM skill_extraction_jobs WHERE id=?`, job.ID).Scan(&lastError, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(lastError, "sha256 mismatch") || snapshot != "[]" {
		t.Fatalf("integrity failure error=%q snapshot=%q", lastError, snapshot)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var completed int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM skill_extraction_jobs WHERE status='completed'`).Scan(&completed); err == nil && completed == 1 {
			if fp.calls != 1 || len(fp.prompts) != 1 || !strings.Contains(fp.prompts[0], "window after corrupt job") {
				t.Fatalf("terminal failure did not hand off only the next window: calls=%d prompts=%#v", fp.calls, fp.prompts)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("window accumulated behind terminal failure was not queued")
}

func TestRunSkillExtractionJobFinalAttemptCrashWakesNextWindow(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, []*provider.Response{textResp("Nothing to save.")})
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "first frozen window"}})
	first := enqueueCadenceJob(t, db, 10)

	// Accumulate another complete window while the first job appears to be held
	// by a worker that crashed on its final allowed attempt.
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, []store.SessionMessage{{Role: "user", Content: "window after final crash"}})
	if _, err := db.DB().Exec(`UPDATE skill_extraction_jobs
		SET status='running', attempt_count=5, lease_owner='dead-worker',
		    lease_expires_at=?, last_error='worker crashed'
		WHERE id=?`, time.Now().UTC().Add(-time.Minute), first.ID); err != nil {
		t.Fatal(err)
	}

	a.runSkillExtractionJob(context.Background(), first.ID)
	if outcome := waitForJobState(t, db, first.ID, "failed"); outcome != "retry_exhausted" {
		t.Fatalf("terminal outcome=%q want retry_exhausted", outcome)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var completed int
		if err := db.DB().QueryRow(`SELECT COUNT(*) FROM skill_extraction_jobs WHERE status='completed'`).Scan(&completed); err == nil && completed == 1 {
			if fp.calls != 1 || len(fp.prompts) != 1 || !strings.Contains(fp.prompts[0], "window after final crash") {
				t.Fatalf("follow-up calls=%d prompts=%#v", fp.calls, fp.prompts)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("final-attempt crash released checkpoint but did not wake next window")
}

func TestRecoverSkillExtractionJobsLaunchesPersistedWork(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, []*provider.Response{textResp("Nothing to save.")})
	seedAgentTurns(t, db, []int{10})
	seedSessionRecord(t, db, sessionMessagesFixture())
	job := enqueueCadenceJob(t, db, 10)
	a.recoverSkillExtractionJobs()
	if outcome := waitForJobState(t, db, job.ID, "completed"); outcome != SkillExtractionSkipped {
		t.Fatalf("recovered outcome=%q want skip", outcome)
	}
	if fp.calls != 1 {
		t.Fatalf("provider calls=%d want 1", fp.calls)
	}
}
