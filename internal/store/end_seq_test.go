package store

import (
	"context"
	"database/sql"
	"testing"
)

func ptrInt64(v int64) *int64 { return &v }

func readEndSeq(t *testing.T, db *DBStore, uid, agent, sk string, seq int64) (int64, bool) {
	t.Helper()
	var end sql.NullInt64
	if err := db.db.QueryRowContext(context.Background(),
		`SELECT end_seq FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`,
		uid, agent, sk, seq).Scan(&end); err != nil {
		t.Fatalf("read end_seq: %v", err)
	}
	return end.Int64, end.Valid
}

func assertContents(t *testing.T, groups []TurnGroup, sk string, want []string) {
	t.Helper()
	for _, g := range groups {
		if g.SessionKey != sk {
			continue
		}
		var got []string
		for _, m := range g.Messages {
			got = append(got, m.Content)
		}
		if len(got) != len(want) {
			t.Fatalf("%s contents = %v, want %v", sk, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s contents = %v, want %v", sk, got, want)
			}
		}
		return
	}
	t.Fatalf("no group for %s in %+v", sk, groups)
}

// TestFinishTurnRecordsEndSeq: FinishTurn 把锚点行的 end_seq 写成该 turn 最后一条
// 消息的 seq(= 完成时该 session 的 MAX(seq))。
func TestFinishTurnRecordsEndSeq(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"

	// turn1:锚点 seq0 + 两条消息 seq1,seq2 → end_seq 应为 2。
	a0, _ := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q1"})
	db.AppendSessionMessage(ctx, uid, agent, sk, SessionMessage{Role: "assistant", Content: "A1"})
	db.AppendSessionMessage(ctx, uid, agent, sk, SessionMessage{Role: "tool", Content: "T1"})
	if err := db.FinishTurn(ctx, uid, agent, sk, a0); err != nil {
		t.Fatalf("finish1: %v", err)
	}
	if got, ok := readEndSeq(t, db, uid, agent, sk, a0); !ok || got != 2 {
		t.Fatalf("turn1 end_seq = %d (valid=%v), want 2", got, ok)
	}

	// turn2:锚点 seq3 + 一条消息 seq4 → end_seq 应为 4。
	b0, _ := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q2"})
	db.AppendSessionMessage(ctx, uid, agent, sk, SessionMessage{Role: "assistant", Content: "A2"})
	if err := db.FinishTurn(ctx, uid, agent, sk, b0); err != nil {
		t.Fatalf("finish2: %v", err)
	}
	if got, ok := readEndSeq(t, db, uid, agent, sk, b0); !ok || got != 4 {
		t.Fatalf("turn2 end_seq = %d (valid=%v), want 4", got, ok)
	}
}

// TestClaimCadenceBatchCarriesEndSeq: 认领返回的 TurnRef 带上 end_seq,
// 供 LoadTurnMessages 做精确区间回放。
func TestClaimCadenceBatchCarriesEndSeq(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"
	a0, _ := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q1"})
	db.AppendSessionMessage(ctx, uid, agent, sk, SessionMessage{Role: "assistant", Content: "A1"})
	db.FinishTurn(ctx, uid, agent, sk, a0)

	_, refs, err := db.ClaimCadenceBatch(context.Background(), agent, "chatterA", 1, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("want 1 ref, got %d", len(refs))
	}
	if refs[0].EndSeq == nil || *refs[0].EndSeq != 1 {
		t.Fatalf("ref EndSeq = %v, want 1", refs[0].EndSeq)
	}
}

// TestLoadTurnMessagesBoundedExcludesGap: 带 end_seq 的精确区间回放只取被认领 turn
// 的行——跳过中间未认领的 turn(gap),也不会扫到 session 末尾的未认领 turn(tail)。
func TestLoadTurnMessagesBoundedExcludesGap(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent := "u1", "agentA"
	// s1: turnA(Q1,A1)[0-1] 认领 / turnB(Q2)[2] 不认领 / turnC(Q3)[3] 认领 / 尾 turnD(Q4)[4] 不认领
	a0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q1"})
	db.AppendSessionMessage(ctx, uid, agent, "s1", SessionMessage{Role: "assistant", Content: "A1"})
	db.FinishTurn(ctx, uid, agent, "s1", a0)
	b0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q2"})
	db.FinishTurn(ctx, uid, agent, "s1", b0)
	c0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q3"})
	db.FinishTurn(ctx, uid, agent, "s1", c0)
	d0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q4"})
	db.FinishTurn(ctx, uid, agent, "s1", d0)

	refs := []TurnRef{
		{SessionKey: "s1", StartSeq: a0, EndSeq: ptrInt64(1)},
		{SessionKey: "s1", StartSeq: c0, EndSeq: ptrInt64(c0)},
	}
	groups, err := db.LoadTurnMessages(ctx, uid, agent, refs)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assertContents(t, groups, "s1", []string{"Q1", "A1", "Q3"})
}

// TestLoadTurnMessagesLegacyNullEndSeq: 旧存量锚点 end_seq 为 NULL(TurnRef.EndSeq=nil)
// 时回退到无上界扫描 + 锚点切片,结果与精确区间路径一致。
func TestLoadTurnMessagesLegacyNullEndSeq(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent := "u1", "agentA"
	a0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q1"})
	db.AppendSessionMessage(ctx, uid, agent, "s1", SessionMessage{Role: "assistant", Content: "A1"})
	db.FinishTurn(ctx, uid, agent, "s1", a0)
	b0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q2"})
	db.FinishTurn(ctx, uid, agent, "s1", b0)
	c0, _ := db.AppendTurnAnchor(ctx, uid, agent, "s1", SessionMessage{Role: "user", Content: "Q3"})
	db.FinishTurn(ctx, uid, agent, "s1", c0)
	// 模拟旧库:抹掉所有 end_seq。
	if _, err := db.db.ExecContext(context.Background(), `UPDATE session_messages SET end_seq=NULL`); err != nil {
		t.Fatalf("null end_seq: %v", err)
	}
	refs := []TurnRef{
		{SessionKey: "s1", StartSeq: a0}, // EndSeq nil → 回退
		{SessionKey: "s1", StartSeq: c0},
	}
	groups, err := db.LoadTurnMessages(ctx, uid, agent, refs)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assertContents(t, groups, "s1", []string{"Q1", "A1", "Q3"})
}
