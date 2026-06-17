package store

import (
	"context"
	"testing"
)

// seedDoneTurns 在一个 session 里写 n 个已完成锚点(running→done)。
func seedDoneTurns(t *testing.T, db *DBStore, agent, chatter string, n int) {
	t.Helper()
	ctx := WithChatterUserID(context.Background(), chatter)
	for i := 0; i < n; i++ {
		seq, err := db.AppendTurnAnchor(ctx, "u1", agent, "sess1", SessionMessage{Role: "user", Content: "q"})
		if err != nil {
			t.Fatalf("anchor: %v", err)
		}
		if err := db.FinishTurn(ctx, "u1", agent, "sess1", seq); err != nil {
			t.Fatalf("finish: %v", err)
		}
	}
}

func TestClaimCadenceBatch(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()

	seedDoneTurns(t, db, "agentA", "chatterA", 4)
	id, refs, err := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if err != nil {
		t.Fatalf("claim<5: %v", err)
	}
	if id != "" || len(refs) != 0 {
		t.Fatalf("want no claim, got id=%q refs=%d", id, len(refs))
	}

	seedDoneTurns(t, db, "agentA", "chatterA", 1)
	id, refs, err = db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if err != nil {
		t.Fatalf("claim=5: %v", err)
	}
	if id == "" || len(refs) != 5 {
		t.Fatalf("want 5 claimed with id, got id=%q refs=%d", id, len(refs))
	}

	id2, refs2, err := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if id2 != "" || len(refs2) != 0 {
		t.Fatalf("want empty second claim, got id=%q refs=%d", id2, len(refs2))
	}
}

func TestClaimCadenceBatchNoDoubleClaim(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seedDoneTurns(t, db, "agentA", "chatterA", 10)

	type res struct {
		id   string
		refs int
	}
	const goroutines = 8
	ch := make(chan res, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			id, refs, err := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			ch <- res{id, len(refs)}
		}()
	}
	claimedTurns, claimers := 0, 0
	for i := 0; i < goroutines; i++ {
		r := <-ch
		if r.id != "" {
			claimers++
			claimedTurns += r.refs
		}
	}
	if claimedTurns > 10 {
		t.Fatalf("over-claimed: %d turns across %d claimers", claimedTurns, claimers)
	}
	var pending, claimed int
	db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE turn_status='done' AND extraction_id IS NULL`).Scan(&pending)
	db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE turn_status='done' AND extraction_id IS NOT NULL`).Scan(&claimed)
	if pending+claimed != 10 || claimed != claimedTurns {
		t.Fatalf("accounting off: pending=%d claimed=%d claimedTurns=%d", pending, claimed, claimedTurns)
	}
}

func TestResetExtractionReturnsToPending(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	seedDoneTurns(t, db, "agentA", "chatterA", 5)
	id, refs, _ := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if id == "" || len(refs) != 5 {
		t.Fatalf("setup claim failed")
	}
	if err := db.ResetExtraction(ctx, id); err != nil {
		t.Fatalf("reset: %v", err)
	}
	id2, refs2, _ := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if id2 == "" || len(refs2) != 5 {
		t.Fatalf("re-claim after reset failed: id=%q refs=%d", id2, len(refs2))
	}
}

func TestLoadTurnMessagesRange(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"
	seq1, _ := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q1"})
	db.AppendSessionMessage(ctx, uid, agent, sk, SessionMessage{Role: "assistant", Content: "A1"})
	db.FinishTurn(ctx, uid, agent, sk, seq1)
	db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q2"})

	msgs, err := db.LoadTurnMessages(ctx, uid, agent, []TurnRef{{SessionKey: sk, StartSeq: seq1}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "Q1" || msgs[1].Content != "A1" {
		t.Fatalf("unexpected range: %+v", msgs)
	}
}
