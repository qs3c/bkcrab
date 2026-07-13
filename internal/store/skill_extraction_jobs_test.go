package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

const (
	jobOwner   = "owner-job"
	jobAgent   = "agent-job"
	jobChatter = "owner-job"
)

func saveJobSnapshot(t *testing.T, db *DBStore, session, text string) {
	t.Helper()
	ctx := WithChatterUserID(context.Background(), jobChatter)
	if err := db.SaveSession(ctx, jobOwner, jobAgent, session, &SessionRecord{
		Messages: []SessionMessage{{Role: "user", Content: text}},
	}); err != nil {
		t.Fatalf("save session snapshot: %v", err)
	}
}

func appendJobTurn(t *testing.T, db *DBStore, session string, count int) int64 {
	t.Helper()
	ctx := WithChatterUserID(context.Background(), jobChatter)
	seq, err := db.AppendTurnAnchor(ctx, jobOwner, jobAgent, session, SessionMessage{Role: "user", Content: "turn"})
	if err != nil {
		t.Fatalf("append anchor: %v", err)
	}
	if err := db.FinishTurn(ctx, jobOwner, jobAgent, session, seq, count); err != nil {
		t.Fatalf("finish turn: %v", err)
	}
	return seq
}

func enqueueJob(t *testing.T, db *DBStore, session string, threshold int) *SkillExtractionJob {
	t.Helper()
	job, err := db.EnqueueSkillExtractionJob(context.Background(), jobOwner, jobAgent, session, jobChatter, threshold)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return job
}

func TestSkillExtractionJobCumulativeThresholdMarksNewestTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts []int
	}{
		{name: "four plus six", counts: []int{4, 6}},
		{name: "nine plus one", counts: []int{9, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestSQLite(t)
			session := "threshold-" + tc.name
			saveJobSnapshot(t, db, session, tc.name)
			first := appendJobTurn(t, db, session, tc.counts[0])
			if job := enqueueJob(t, db, session, 10); job != nil {
				t.Fatalf("job queued below threshold: %+v", job)
			}
			latest := appendJobTurn(t, db, session, tc.counts[1])
			job := enqueueJob(t, db, session, 10)
			if job == nil || job.ThroughSeq != latest || job.ToolCallCount != 10 {
				t.Fatalf("job = %+v, want through=%d count=10", job, latest)
			}
			var firstMarker, latestMarker *string
			if err := db.db.QueryRow(`SELECT skill_extraction_id FROM session_messages
				WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`, jobOwner, jobAgent, session, first).Scan(&firstMarker); err != nil {
				t.Fatal(err)
			}
			if err := db.db.QueryRow(`SELECT skill_extraction_id FROM session_messages
				WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`, jobOwner, jobAgent, session, latest).Scan(&latestMarker); err != nil {
				t.Fatal(err)
			}
			if firstMarker != nil || latestMarker == nil || *latestMarker != job.ID {
				t.Fatalf("markers first=%v latest=%v job=%s", firstMarker, latestMarker, job.ID)
			}
		})
	}
}

func TestSkillExtractionJobFreezesSessionSnapshot(t *testing.T) {
	db := newTestSQLite(t)
	session := "snapshot"
	saveJobSnapshot(t, db, session, "before")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	if job == nil {
		t.Fatal("expected job")
	}
	wantSnapshot := append([]byte(nil), job.SnapshotJSON...)
	wantHash := job.SnapshotSHA256

	saveJobSnapshot(t, db, session, "after")
	same := enqueueJob(t, db, session, 10)
	if same == nil || same.ID != job.ID || string(same.SnapshotJSON) != string(wantSnapshot) || same.SnapshotSHA256 != wantHash {
		t.Fatalf("inflight snapshot changed: before=%s/%s after=%+v", wantSnapshot, wantHash, same)
	}
	var decoded []SessionMessage
	if err := json.Unmarshal(same.SnapshotJSON, &decoded); err != nil || len(decoded) != 1 || decoded[0].Content != "before" {
		t.Fatalf("frozen snapshot = %#v err=%v", decoded, err)
	}
	sum := sha256.Sum256(wantSnapshot)
	if got := hex.EncodeToString(sum[:]); got != wantHash {
		t.Fatalf("snapshot hash=%s want=%s", wantHash, got)
	}
}

func TestSkillExtractionJobDefersSnapshotWhileNewerTurnIsRunning(t *testing.T) {
	db := newTestSQLite(t)
	session := "running-boundary"
	saveJobSnapshot(t, db, session, "completed turn")
	first := appendJobTurn(t, db, session, 10)

	// AppendTurnAnchor mirrors the production order: sessions.messages is
	// saved first and the running anchor is then persisted. The cadence must
	// not freeze that partial full-session view into the completed window.
	saveJobSnapshot(t, db, session, "partial next turn")
	ctx := WithChatterUserID(context.Background(), jobChatter)
	running, err := db.AppendTurnAnchor(ctx, jobOwner, jobAgent, session, SessionMessage{Role: "user", Content: "partial next turn"})
	if err != nil {
		t.Fatalf("append running anchor: %v", err)
	}
	if job := enqueueJob(t, db, session, 10); job != nil {
		t.Fatalf("queued snapshot containing a running turn: %+v", job)
	}
	var jobs int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM skill_extraction_jobs`).Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("jobs=%d err=%v, want none", jobs, err)
	}

	if err := db.FinishTurn(ctx, jobOwner, jobAgent, session, running, 0); err != nil {
		t.Fatalf("finish newer turn: %v", err)
	}
	job := enqueueJob(t, db, session, 10)
	if job == nil || job.ThroughSeq != running || job.ToolCallCount != 10 || first >= running {
		t.Fatalf("completed boundary job=%+v first=%d running=%d", job, first, running)
	}
}

func TestSkillExtractionJobNewTurnSupersedesStaleRunningAnchor(t *testing.T) {
	db := newTestSQLite(t)
	session := "stale-running"
	ctx := WithChatterUserID(context.Background(), jobChatter)
	saveJobSnapshot(t, db, session, "crashed turn")
	stale, err := db.AppendTurnAnchor(ctx, jobOwner, jobAgent, session, SessionMessage{Role: "user", Content: "crashed"})
	if err != nil {
		t.Fatalf("append stale anchor: %v", err)
	}

	// A later accepted web turn explicitly succeeds the crashed one.
	saveJobSnapshot(t, db, session, "recovered completed turn")
	latest, err := db.AppendTurnAnchor(ctx, jobOwner, jobAgent, session, SessionMessage{Role: "user", Content: "recovered"})
	if err != nil {
		t.Fatalf("append successor anchor: %v", err)
	}
	if err := db.FinishTurn(ctx, jobOwner, jobAgent, session, latest, 10); err != nil {
		t.Fatalf("finish successor: %v", err)
	}
	var staleStatus string
	if err := db.db.QueryRow(`SELECT turn_status FROM session_messages
		WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`, jobOwner, jobAgent, session, stale).Scan(&staleStatus); err != nil {
		t.Fatal(err)
	}
	if staleStatus != "" {
		t.Fatalf("stale anchor status=%q, want ordinary archived row", staleStatus)
	}
	job := enqueueJob(t, db, session, 10)
	if job == nil || job.ThroughSeq != latest || job.ToolCallCount != 10 {
		t.Fatalf("stale anchor blocked successor cadence: %+v latest=%d", job, latest)
	}
}

func TestSkillExtractionJobConcurrentEnqueueCreatesOneJob(t *testing.T) {
	db := newTestSQLite(t)
	session := "concurrent"
	saveJobSnapshot(t, db, session, "snapshot")
	appendJobTurn(t, db, session, 10)

	const workers = 12
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := db.EnqueueSkillExtractionJob(context.Background(), jobOwner, jobAgent, session, jobChatter, 10)
			if err != nil {
				errs <- err
				return
			}
			if job != nil {
				ids <- job.ID
			}
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent enqueue: %v", err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("unique job IDs = %v, want one", unique)
	}
	var onlyID string
	for id := range unique {
		onlyID = id
	}
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM skill_extraction_jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("job rows=%d err=%v", count, err)
	}

	acquired := make(chan bool, workers)
	leaseErrs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, ok, err := db.AcquireSkillExtractionJob(context.Background(), onlyID, "lease-worker-"+string(rune('a'+i)), time.Minute)
			if err != nil {
				leaseErrs <- err
				return
			}
			acquired <- ok
		}(i)
	}
	wg.Wait()
	close(acquired)
	close(leaseErrs)
	for err := range leaseErrs {
		t.Fatalf("concurrent acquire: %v", err)
	}
	leaseWinners := 0
	for ok := range acquired {
		if ok {
			leaseWinners++
		}
	}
	if leaseWinners != 1 {
		t.Fatalf("lease winners=%d, want one", leaseWinners)
	}
}

func TestSkillExtractionJobSkipAdvancesCheckpoint(t *testing.T) {
	db := newTestSQLite(t)
	session := "skip"
	saveJobSnapshot(t, db, session, "first")
	appendJobTurn(t, db, session, 10)
	first := enqueueJob(t, db, session, 10)
	leased, ok, err := db.AcquireSkillExtractionJob(context.Background(), first.ID, "worker-1", time.Minute)
	if err != nil || !ok || leased.ID != first.ID {
		t.Fatalf("acquire=%+v ok=%v err=%v", leased, ok, err)
	}
	if err := db.CompleteSkillExtractionJob(context.Background(), first.ID, "worker-1", "skip", 0, nil); err != nil {
		t.Fatalf("complete skip: %v", err)
	}
	var completedSnapshot string
	if err := db.db.QueryRow(`SELECT snapshot_json FROM skill_extraction_jobs WHERE id=?`, first.ID).Scan(&completedSnapshot); err != nil || completedSnapshot != "[]" {
		t.Fatalf("completed snapshot=%q err=%v; want scrubbed []", completedSnapshot, err)
	}

	appendJobTurn(t, db, session, 9)
	if job := enqueueJob(t, db, session, 10); job != nil {
		t.Fatalf("skip did not consume first window: %+v", job)
	}
	latest := appendJobTurn(t, db, session, 1)
	saveJobSnapshot(t, db, session, "second")
	second := enqueueJob(t, db, session, 10)
	if second == nil || second.ID == first.ID || second.AfterSeq != first.ThroughSeq || second.ThroughSeq != latest {
		t.Fatalf("second job=%+v first=%+v", second, first)
	}
}

func TestSkillExtractionJobRetryKeepsSameJobAndSnapshot(t *testing.T) {
	db := newTestSQLite(t)
	session := "retry"
	saveJobSnapshot(t, db, session, "original")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	first, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first acquire ok=%v err=%v", ok, err)
	}
	if err := db.RetrySkillExtractionJob(context.Background(), job.ID, "worker-1", "provider unavailable"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	saveJobSnapshot(t, db, session, "changed later")
	queued := enqueueJob(t, db, session, 10)
	if queued.ID != job.ID || queued.SnapshotSHA256 != first.SnapshotSHA256 {
		t.Fatalf("retry created/replaced snapshot: first=%+v queued=%+v", first, queued)
	}
	if queued.NextAttemptAt == nil || !queued.NextAttemptAt.After(time.Now().UTC()) {
		t.Fatalf("retry availability=%v, want future durable timestamp", queued.NextAttemptAt)
	}
	if recoverable, err := db.ListRecoverableSkillExtractionJobs(context.Background(), jobAgent, 10); err != nil || len(recoverable) != 0 {
		t.Fatalf("backed-off job recoverable immediately: %+v err=%v", recoverable, err)
	}
	if got, acquired, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-too-early", time.Minute); err != nil || acquired || got.NextAttemptAt == nil {
		t.Fatalf("early acquire=%+v acquired=%v err=%v", got, acquired, err)
	}
	if _, err := db.db.Exec(`UPDATE skill_extraction_jobs SET next_attempt_at=? WHERE id=?`, time.Now().UTC().Add(-time.Second), job.ID); err != nil {
		t.Fatal(err)
	}
	second, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-2", time.Minute)
	if err != nil || !ok || second.AttemptCount != 2 || second.SnapshotSHA256 != first.SnapshotSHA256 {
		t.Fatalf("second acquire=%+v ok=%v err=%v", second, ok, err)
	}
}

func TestSkillExtractionJobRetryBackoffEventuallyFailsTerminal(t *testing.T) {
	db := newTestSQLite(t)
	session := "retry-terminal"
	saveJobSnapshot(t, db, session, "original")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)

	for attempt := 1; attempt <= skillExtractionMaxAttempts; attempt++ {
		worker := fmt.Sprintf("retry-worker-%d", attempt)
		leased, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, worker, time.Minute)
		if err != nil || !ok || leased.AttemptCount != attempt {
			t.Fatalf("attempt %d acquire=%+v ok=%v err=%v", attempt, leased, ok, err)
		}
		beforeRetry := time.Now().UTC()
		if err := db.RetrySkillExtractionJob(context.Background(), job.ID, worker, "provider unavailable"); err != nil {
			t.Fatalf("attempt %d retry: %v", attempt, err)
		}

		var status, outcome, snapshot string
		var nextAttempt, completed sql.NullTime
		if err := db.db.QueryRow(`SELECT status, outcome, snapshot_json, next_attempt_at, completed_at
			FROM skill_extraction_jobs WHERE id=?`, job.ID).
			Scan(&status, &outcome, &snapshot, &nextAttempt, &completed); err != nil {
			t.Fatal(err)
		}
		if attempt == skillExtractionMaxAttempts {
			if status != "failed" || outcome != "retry_exhausted" || snapshot != "[]" || nextAttempt.Valid || !completed.Valid {
				t.Fatalf("terminal row status=%q outcome=%q snapshot=%q next=%v completed=%v", status, outcome, snapshot, nextAttempt, completed)
			}
			break
		}

		wantDelay := skillExtractionRetryDelay(attempt)
		if status != "pending" || outcome != "" || !nextAttempt.Valid {
			t.Fatalf("attempt %d row status=%q outcome=%q next=%v", attempt, status, outcome, nextAttempt)
		}
		gotDelay := nextAttempt.Time.Sub(beforeRetry)
		if gotDelay < wantDelay-time.Second || gotDelay > wantDelay+time.Second {
			t.Fatalf("attempt %d delay=%s want about %s", attempt, gotDelay, wantDelay)
		}
		if _, err := db.db.Exec(`UPDATE skill_extraction_jobs SET next_attempt_at=? WHERE id=?`, time.Now().UTC().Add(-time.Second), job.ID); err != nil {
			t.Fatal(err)
		}
	}

	if recoverable, err := db.ListRecoverableSkillExtractionJobs(context.Background(), jobAgent, 10); err != nil || len(recoverable) != 0 {
		t.Fatalf("retry-exhausted job recoverable: %+v err=%v", recoverable, err)
	}
	appendJobTurn(t, db, session, 10)
	next := enqueueJob(t, db, session, 10)
	if next == nil || next.ID == job.ID || next.AfterSeq != job.ThroughSeq {
		t.Fatalf("retry terminal did not release checkpoint: first=%+v next=%+v", job, next)
	}
}

func TestSkillExtractionJobRetryAvailabilityMigrationIsIdempotent(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx, `DROP INDEX idx_skill_extraction_jobs_available`); err != nil {
		t.Fatalf("drop availability index: %v", err)
	}
	if _, err := db.db.ExecContext(ctx, `ALTER TABLE skill_extraction_jobs DROP COLUMN next_attempt_at`); err != nil {
		t.Fatalf("drop availability column: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
	}
	has, err := db.tableHasColumn(ctx, "skill_extraction_jobs", "next_attempt_at")
	if err != nil || !has {
		t.Fatalf("next_attempt_at present=%v err=%v", has, err)
	}
}

func TestSkillExtractionJobExpiredLeaseIsRecoverable(t *testing.T) {
	db := newTestSQLite(t)
	session := "lease"
	saveJobSnapshot(t, db, session, "snapshot")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	if _, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-old", time.Minute); err != nil || !ok {
		t.Fatalf("initial acquire ok=%v err=%v", ok, err)
	}
	if _, err := db.db.Exec(`UPDATE skill_extraction_jobs SET lease_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Minute), job.ID); err != nil {
		t.Fatal(err)
	}
	recoverable, err := db.ListRecoverableSkillExtractionJobs(context.Background(), jobAgent, 10)
	if err != nil || len(recoverable) != 1 || recoverable[0].ID != job.ID {
		t.Fatalf("recoverable=%+v err=%v", recoverable, err)
	}
	reacquired, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-new", time.Minute)
	if err != nil || !ok || reacquired.LeaseOwner != "worker-new" || reacquired.AttemptCount != 2 {
		t.Fatalf("reacquire=%+v ok=%v err=%v", reacquired, ok, err)
	}
}

func TestSkillExtractionJobFailedIsTerminalAndReleasesCheckpoint(t *testing.T) {
	db := newTestSQLite(t)
	session := "terminal-fail"
	saveJobSnapshot(t, db, session, "first")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	if _, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-fail", time.Minute); err != nil || !ok {
		t.Fatalf("acquire ok=%v err=%v", ok, err)
	}
	if err := db.FailSkillExtractionJob(context.Background(), job.ID, "worker-fail", "invalid snapshot invariant"); err != nil {
		t.Fatalf("fail: %v", err)
	}
	var failedSnapshot string
	if err := db.db.QueryRow(`SELECT snapshot_json FROM skill_extraction_jobs WHERE id=?`, job.ID).Scan(&failedSnapshot); err != nil || failedSnapshot != "[]" {
		t.Fatalf("failed snapshot=%q err=%v; want scrubbed []", failedSnapshot, err)
	}
	if got, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-again", time.Minute); err != nil || ok || got.Status != "failed" {
		t.Fatalf("failed acquire got=%+v ok=%v err=%v", got, ok, err)
	}
	if recoverable, err := db.ListRecoverableSkillExtractionJobs(context.Background(), jobAgent, 10); err != nil || len(recoverable) != 0 {
		t.Fatalf("terminal failed job is recoverable: %+v err=%v", recoverable, err)
	}
	appendJobTurn(t, db, session, 10)
	next := enqueueJob(t, db, session, 10)
	if next == nil || next.ID == job.ID || next.AfterSeq != job.ThroughSeq {
		t.Fatalf("checkpoint was not released after fail: first=%+v next=%+v", job, next)
	}
}

func TestSkillExtractionJobHasNoThirtyTwoTurnStarvation(t *testing.T) {
	db := newTestSQLite(t)
	session := "long"
	saveJobSnapshot(t, db, session, "snapshot")
	var latest int64
	for i := 0; i < 40; i++ {
		latest = appendJobTurn(t, db, session, 1)
	}
	job := enqueueJob(t, db, session, 40)
	if job == nil || job.ToolCallCount != 40 || job.ThroughSeq != latest {
		t.Fatalf("long-window job=%+v latest=%d", job, latest)
	}
}

func TestSkillExtractionCheckpointLegacyMarkerBackfillIsIdempotent(t *testing.T) {
	db := newTestSQLite(t)
	session := "legacy-backfill"
	saveJobSnapshot(t, db, session, "snapshot")
	first := appendJobTurn(t, db, session, 5)
	latest := appendJobTurn(t, db, session, 5)
	legacyID, refs, err := db.ClaimSkillBatch(context.Background(), jobAgent, session, jobChatter, 10, 32)
	if err != nil || legacyID == "" || len(refs) != 2 {
		t.Fatalf("legacy claim id=%q refs=%v err=%v", legacyID, refs, err)
	}
	for i := 0; i < 2; i++ {
		if err := db.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate %d: %v", i, err)
		}
	}
	var consumed int64
	var rows int
	if err := db.db.QueryRow(`SELECT consumed_through_seq, COUNT(*) FROM skill_extraction_checkpoints
		WHERE owner_user_id=? AND agent_id=? AND session_key=? AND chatter_user_id=?`,
		jobOwner, jobAgent, session, jobChatter).Scan(&consumed, &rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || consumed != latest || first >= latest {
		t.Fatalf("checkpoint rows=%d consumed=%d first=%d latest=%d", rows, consumed, first, latest)
	}
	appendJobTurn(t, db, session, 9)
	if job := enqueueJob(t, db, session, 10); job != nil {
		t.Fatalf("legacy consumed boundary replayed: %+v", job)
	}
}

func TestSkillExtractionCheckpointMigrationDoesNotConsumeInflightJob(t *testing.T) {
	db := newTestSQLite(t)
	session := "migrate-inflight"
	saveJobSnapshot(t, db, session, "snapshot")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate with inflight job: %v", err)
	}
	var consumed int64
	var inflight string
	if err := db.db.QueryRow(`SELECT consumed_through_seq, inflight_job_id
		FROM skill_extraction_checkpoints WHERE owner_user_id=? AND agent_id=? AND session_key=? AND chatter_user_id=?`,
		jobOwner, jobAgent, session, jobChatter).Scan(&consumed, &inflight); err != nil {
		t.Fatal(err)
	}
	if consumed != job.AfterSeq || inflight != job.ID {
		t.Fatalf("migration changed inflight checkpoint: consumed=%d inflight=%q job=%+v", consumed, inflight, job)
	}
}

func TestDeleteAgentRemovesSkillExtractionJobsAndCheckpoints(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if err := db.SaveAgent(ctx, &AgentRecord{ID: jobAgent, UserID: jobOwner, Name: "agent"}); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	session := "delete-agent-job"
	saveJobSnapshot(t, db, session, "snapshot")
	appendJobTurn(t, db, session, 10)
	if job := enqueueJob(t, db, session, 10); job == nil {
		t.Fatal("expected job")
	}
	if err := db.DeleteAgent(ctx, jobAgent); err != nil {
		t.Fatalf("delete agent: %v", err)
	}
	for _, table := range []string{"skill_extraction_jobs", "skill_extraction_checkpoints"} {
		var count int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE agent_id=?`, jobAgent).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("%s retained %d rows", table, count)
		}
	}
}

func TestDeleteSessionKeepsFrozenSkillExtractionJobCompletable(t *testing.T) {
	db := newTestSQLite(t)
	session := "deleted-session-job"
	saveJobSnapshot(t, db, session, "snapshot")
	appendJobTurn(t, db, session, 10)
	job := enqueueJob(t, db, session, 10)
	if _, ok, err := db.AcquireSkillExtractionJob(context.Background(), job.ID, "worker-delete-session", time.Minute); err != nil || !ok {
		t.Fatalf("acquire ok=%v err=%v", ok, err)
	}
	if err := db.DeleteSession(context.Background(), jobOwner, jobAgent, session); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if err := db.CompleteSkillExtractionJob(context.Background(), job.ID, "worker-delete-session", "skip", 0, nil); err != nil {
		t.Fatalf("complete frozen job after session deletion: %v", err)
	}
	var status, snapshot string
	if err := db.db.QueryRow(`SELECT status, snapshot_json FROM skill_extraction_jobs WHERE id=?`, job.ID).Scan(&status, &snapshot); err != nil || status != "completed" || snapshot != "[]" {
		t.Fatalf("status=%q snapshot=%q err=%v", status, snapshot, err)
	}
}
