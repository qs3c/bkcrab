package store

import (
	"context"
	"sync"
	"testing"
)

func seedSkillTurns(t *testing.T, db *DBStore, agent, session string, counts []int) {
	t.Helper()
	ctx := context.Background()
	for _, c := range counts {
		seq, err := db.AppendTurnAnchor(ctx, "u1", agent, session, SessionMessage{Role: "user", Content: "q"})
		if err != nil {
			t.Fatalf("anchor: %v", err)
		}
		if err := db.FinishTurn(ctx, "u1", agent, session, seq, c); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}
}

func TestClaimSkillBatchBelowThreshold(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seedSkillTurns(t, db, "agentA", "s1", []int{3, 4})
	id, refs, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if id != "" || len(refs) != 0 {
		t.Fatalf("want no claim, got id=%q refs=%d", id, len(refs))
	}
}

func TestClaimSkillBatchIgnoresZeroCountAnchors(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	counts := make([]int, 0, 34)
	for i := 0; i < 32; i++ {
		counts = append(counts, 0)
	}
	counts = append(counts, 5, 5)
	seedSkillTurns(t, db, "agentA", "s1", counts)
	id, refs, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if id == "" || len(refs) != 2 {
		t.Fatalf("want positive-count turns claimed past zero prefix, got id=%q refs=%d", id, len(refs))
	}
}

func TestClaimSkillBatchClaimsWholeBatch(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seedSkillTurns(t, db, "agentA", "s1", []int{3, 4, 5})
	id, refs, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if id == "" || len(refs) != 3 {
		t.Fatalf("want 3 claimed, got id=%q refs=%d", id, len(refs))
	}
	id2, refs2, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if id2 != "" || len(refs2) != 0 {
		t.Fatalf("want empty second claim, got id=%q refs=%d", id2, len(refs2))
	}
}

func TestClaimSkillBatchScopedToSession(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seedSkillTurns(t, db, "agentA", "s1", []int{6})
	seedSkillTurns(t, db, "agentA", "s2", []int{6})
	if id, _, _ := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32); id != "" {
		t.Fatalf("s1 should not be claimable, got id=%q", id)
	}
}

func TestClaimSkillBatchNoDoubleClaim(t *testing.T) {
	db := newTestSQLite(t)
	seedSkillTurns(t, db, "agentA", "s1", []int{5, 5, 5})
	var wg sync.WaitGroup
	winners := make(chan string, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, _, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", 10, 32)
			if err == nil && id != "" {
				winners <- id
			}
		}()
	}
	wg.Wait()
	close(winners)
	n := 0
	for range winners {
		n++
	}
	if n != 1 {
		t.Fatalf("want exactly 1 winner, got %d", n)
	}
}

func TestSkillAndMemoryClaimsIndependent(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	cctx := WithChatterUserID(ctx, "chatterA")
	for i := 0; i < 3; i++ {
		seq, err := db.AppendTurnAnchor(cctx, "u1", "agentA", "s1", SessionMessage{Role: "user", Content: "q"})
		if err != nil {
			t.Fatalf("anchor: %v", err)
		}
		if err := db.FinishTurn(cctx, "u1", "agentA", "s1", seq, 5); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}
	memID, memRefs, err := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 3, 9)
	if err != nil || memID == "" || len(memRefs) != 3 {
		t.Fatalf("memory claim: id=%q refs=%d err=%v", memID, len(memRefs), err)
	}
	skillID, skillRefs, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil || skillID == "" || len(skillRefs) != 3 {
		t.Fatalf("skill claim after memory claim: id=%q refs=%d err=%v", skillID, len(skillRefs), err)
	}
}

func TestResetSkillExtractionRestoresBatch(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seedSkillTurns(t, db, "agentA", "s1", []int{6, 6})
	id, _, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil || id == "" {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	if err := db.ResetSkillExtraction(ctx, id); err != nil {
		t.Fatalf("reset: %v", err)
	}
	id2, refs2, err := db.ClaimSkillBatch(ctx, "agentA", "s1", 10, 32)
	if err != nil || id2 == "" || len(refs2) != 2 {
		t.Fatalf("re-claim after reset: id=%q refs=%d err=%v", id2, len(refs2), err)
	}
}
