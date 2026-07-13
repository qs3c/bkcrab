package store

import (
	"context"
	"sort"
	"sync"
	"testing"
)

func TestAdvanceSkillLifecycleConcurrent(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()

	const advances = 32
	start := make(chan struct{})
	seqs := make(chan int64, advances)
	errs := make(chan error, advances)
	var wg sync.WaitGroup
	for i := 0; i < advances; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			seq, _, err := db.AdvanceSkillLifecycle(ctx, "agent-concurrent", 0)
			if err != nil {
				errs <- err
				return
			}
			seqs <- seq
		}()
	}
	close(start)
	wg.Wait()
	close(seqs)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent advance: %v", err)
	}

	got := make([]int, 0, advances)
	for seq := range seqs {
		got = append(got, int(seq))
	}
	sort.Ints(got)
	if len(got) != advances {
		t.Fatalf("advance results=%d want %d", len(got), advances)
	}
	for i, seq := range got {
		want := i + 2 // lifecycle clocks are initialized at one
		if seq != want {
			t.Fatalf("sorted sequence[%d]=%d want %d; all=%v", i, seq, want, got)
		}
	}
	current, err := db.CurrentSkillLifecycleSeq(ctx, "agent-concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if current != advances+1 {
		t.Fatalf("current=%d want %d", current, advances+1)
	}
}

func TestSkillLifecycleDoesNotRegressWhenMaxLedgerRowDeleted(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx, `INSERT INTO skill_usage
		(agent_id, slug, created_seq, edited_seq, last_load_seq)
		VALUES ('agent-stable', 'older', 2, 4, 5), ('agent-stable', 'newest', 7, 9, 12)`); err != nil {
		t.Fatal(err)
	}

	seq, err := db.CurrentSkillLifecycleSeq(ctx, "agent-stable")
	if err != nil || seq != 12 {
		t.Fatalf("initial seq=(%d,%v) want (12,nil)", seq, err)
	}
	if err := db.DeleteSkillUsage(ctx, "agent-stable", "newest"); err != nil {
		t.Fatal(err)
	}
	seq, err = db.CurrentSkillLifecycleSeq(ctx, "agent-stable")
	if err != nil || seq != 12 {
		t.Fatalf("seq after max ledger delete=(%d,%v) want (12,nil)", seq, err)
	}
	seq, _, err = db.AdvanceSkillLifecycle(ctx, "agent-stable", 0)
	if err != nil || seq != 13 {
		t.Fatalf("advance after delete=(%d,%v) want (13,nil)", seq, err)
	}
}

func TestAdvanceSkillLifecycleClaimsCleanupPeriodically(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()

	wantDue := map[int64]bool{4: true, 7: true}
	for wantSeq := int64(2); wantSeq <= 7; wantSeq++ {
		seq, due, err := db.AdvanceSkillLifecycle(ctx, "agent-cleanup", 3)
		if err != nil {
			t.Fatal(err)
		}
		if seq != wantSeq || due != wantDue[wantSeq] {
			t.Fatalf("advance got seq=%d due=%v want seq=%d due=%v", seq, due, wantSeq, wantDue[wantSeq])
		}
	}
	seq, due, err := db.AdvanceSkillLifecycle(ctx, "agent-cleanup", 0)
	if err != nil || seq != 8 || due {
		t.Fatalf("disabled cleanup got seq=%d due=%v err=%v", seq, due, err)
	}
	seq, due, err = db.AdvanceSkillLifecycle(ctx, "agent-cleanup", -1)
	if err != nil || seq != 9 || due {
		t.Fatalf("negative cleanup got seq=%d due=%v err=%v", seq, due, err)
	}
}

func TestRecordSkillLoadUsesClockWithoutAdvancing(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if err := db.UpsertSkillUsage(ctx, "agent-load", "foo", "hash", true); err != nil {
		t.Fatal(err)
	}
	before, err := db.CurrentSkillLifecycleSeq(ctx, "agent-load")
	if err != nil || before != 1 {
		t.Fatalf("initial clock=(%d,%v) want (1,nil)", before, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := db.RecordSkillLoad(ctx, "agent-load", "foo", "hash", false, 32, 3); err != nil {
			t.Fatal(err)
		}
	}
	after, err := db.CurrentSkillLifecycleSeq(ctx, "agent-load")
	if err != nil || after != before {
		t.Fatalf("load advanced clock: before=%d after=%d err=%v", before, after, err)
	}
	rows, err := db.ListSkillUsage(ctx, "agent-load")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list rows=%+v err=%v", rows, err)
	}
	if rows[0].TotalLoads != 2 || rows[0].LastLoadSeq != before || rows[0].Activity != 2 {
		t.Fatalf("same-turn loads were not accumulated at one sequence: %+v", rows[0])
	}
}

func TestSkillUsageCreateAndEditUseCurrentLifecycleClock(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seq, _, err := db.AdvanceSkillLifecycle(ctx, "agent-upsert", 0)
	if err != nil || seq != 2 {
		t.Fatalf("advance create clock=(%d,%v)", seq, err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-upsert", "foo", "hash-1", true); err != nil {
		t.Fatal(err)
	}
	seq, _, err = db.AdvanceSkillLifecycle(ctx, "agent-upsert", 0)
	if err != nil || seq != 3 {
		t.Fatalf("advance edit clock=(%d,%v)", seq, err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-upsert", "foo", "hash-2", false); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListSkillUsage(ctx, "agent-upsert")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list rows=%+v err=%v", rows, err)
	}
	if rows[0].CreatedSeq != 2 || rows[0].EditedSeq != 3 || rows[0].ContentHash != "hash-2" {
		t.Fatalf("upsert lifecycle stamps are wrong: %+v", rows[0])
	}
}

func TestSkillUsageUpdateRepairsMissingLedgerAtCurrentClock(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seq, _, err := db.AdvanceSkillLifecycle(ctx, "agent-update-repair", 0)
	if err != nil || seq != 2 {
		t.Fatalf("advance clock=(%d,%v)", seq, err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-update-repair", "missing", "repaired-hash", false); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListSkillUsage(ctx, "agent-update-repair")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list rows=%+v err=%v", rows, err)
	}
	if rows[0].Origin != "learner" || rows[0].CreatedSeq != seq || rows[0].EditedSeq != seq || rows[0].ContentHash != "repaired-hash" {
		t.Fatalf("missing update ledger was not repaired at current clock: %+v", rows[0])
	}
}

func TestCurrentSkillLifecycleLazilyBackfillsLegacyLedger(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if _, err := db.db.ExecContext(ctx, `INSERT INTO skill_usage
		(agent_id, slug, created_seq, edited_seq, last_load_seq)
		VALUES ('agent-legacy', 'a', 4, 8, 6), ('agent-legacy', 'b', 3, 0, 11)`); err != nil {
		t.Fatal(err)
	}
	seq, err := db.CurrentSkillLifecycleSeq(ctx, "agent-legacy")
	if err != nil || seq != 11 {
		t.Fatalf("lazy backfill=(%d,%v) want (11,nil)", seq, err)
	}
	var clock, lastCleanup int64
	if err := db.db.QueryRowContext(ctx, `SELECT clock_seq, last_cleanup_seq
		FROM agent_skill_lifecycle WHERE agent_id='agent-legacy'`).Scan(&clock, &lastCleanup); err != nil {
		t.Fatal(err)
	}
	if clock != 11 || lastCleanup != 11 {
		t.Fatalf("persisted lazy clock=(%d,%d) want (11,11)", clock, lastCleanup)
	}
}
