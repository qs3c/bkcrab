package agent

import (
	"context"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

func newCadenceFixture(t *testing.T, responses []string) (*Agent, *store.DBStore, string, *learnerFakeProvider) {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + dir + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ws := t.TempDir()
	fp := &learnerFakeProvider{responses: responses}
	a := &Agent{name: "agentA", ownerUserID: "u1", dataStore: db, skillsLearner: NewSkillsLearner(ws, fp, "m")}
	return a, db, ws, fp
}

func seedAgentTurns(t *testing.T, db *store.DBStore, counts []int) *turnAnchor {
	t.Helper()
	ctx := context.Background()
	var last int64
	for _, c := range counts {
		seq, err := db.AppendTurnAnchor(ctx, "u1", "agentA", "s1", store.SessionMessage{Role: "user", Content: "q"})
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

func TestSkillsCadenceBelowThresholdNoClaimNoLLM(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	anchor := seedAgentTurns(t, db, []int{3, 4})
	a.maybeExtractSkillsCadence(context.Background(), anchor)
	if fp.calls != 0 {
		t.Fatalf("below threshold should not call LLM, calls=%d", fp.calls)
	}
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", 7, 32)
	if err != nil || id == "" || len(refs) != 2 {
		t.Fatalf("anchors were consumed unexpectedly: id=%q refs=%d err=%v", id, len(refs), err)
	}
}

func TestSkillsCadenceExtractsAndWrites(t *testing.T) {
	a, db, ws, _ := newCadenceFixture(t, []string{learnerExtractionJSON(t, "cadence-skill", learnerValidSkill)})
	anchor := seedAgentTurns(t, db, []int{4, 4, 4})
	a.maybeExtractSkillsCadence(context.Background(), anchor)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := readSkill(t, ws, "cadence-skill"); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cadence extraction timed out before writing the skill")
}

func TestRunSkillBatchExtractionResetsOnProviderError(t *testing.T) {
	a, db, _, _ := newCadenceFixture(t, nil)
	a.skillsLearner = NewSkillsLearner(t.TempDir(), &learnerErrProvider{}, "m")
	seedAgentTurns(t, db, []int{6, 6})
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", 10, 32)
	if err != nil || id == "" {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	a.runSkillBatchExtraction(context.Background(), id, refs)
	id2, refs2, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", 10, 32)
	if err != nil || id2 == "" || len(refs2) != 2 {
		t.Fatalf("batch was not reset: id=%q refs=%d err=%v", id2, len(refs2), err)
	}
}

func TestRunSkillBatchExtractionNotWorthyConsumes(t *testing.T) {
	a, db, _, _ := newCadenceFixture(t, []string{`{"extract": false}`})
	seedAgentTurns(t, db, []int{6, 6})
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", 10, 32)
	if err != nil || id == "" {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	a.runSkillBatchExtraction(context.Background(), id, refs)
	id2, _, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "" {
		t.Fatal("not-worthy extraction should keep the batch consumed")
	}
}
