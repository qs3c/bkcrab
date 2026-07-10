package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

func newCadenceFixture(t *testing.T, responses []*provider.Response) (*Agent, *store.DBStore, string, *learnerFakeProvider) {
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
	ctx := store.WithChatterUserID(context.Background(), "u1")
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

func seedAgentTurnForChatter(t *testing.T, db *store.DBStore, chatter, userContent, assistantContent string, toolCalls int) int64 {
	t.Helper()
	ctx := store.WithChatterUserID(context.Background(), chatter)
	seq, err := db.AppendTurnAnchor(ctx, "u1", "agentA", "s1", store.SessionMessage{Role: "user", Content: userContent})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if assistantContent != "" {
		if err := db.AppendSessionMessage(ctx, "u1", "agentA", "s1", store.SessionMessage{Role: "assistant", Content: assistantContent}); err != nil {
			t.Fatalf("assistant message: %v", err)
		}
	}
	if err := db.FinishTurn(ctx, "u1", "agentA", "s1", seq, toolCalls); err != nil {
		t.Fatalf("finish: %v", err)
	}
	return seq
}

// seedSessionRecord 写入 s1 的 sessions.messages 工作集——提取素材的来源。
func seedSessionRecord(t *testing.T, db *store.DBStore, msgs []store.SessionMessage) {
	t.Helper()
	if err := db.SaveSession(context.Background(), "u1", "agentA", "s1", &store.SessionRecord{Messages: msgs}); err != nil {
		t.Fatalf("save session: %v", err)
	}
}

func TestSkillsCadenceBelowThresholdNoClaimNoLLM(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	anchor := seedAgentTurns(t, db, []int{3, 4})
	a.maybeExtractSkillsCadence(context.Background(), anchor, "u1")
	if fp.calls != 0 {
		t.Fatalf("below threshold should not call LLM, calls=%d", fp.calls)
	}
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 7, 32)
	if err != nil || id == "" || len(refs) != 2 {
		t.Fatalf("anchors were consumed unexpectedly: id=%q refs=%d err=%v", id, len(refs), err)
	}
}

func TestSkillsCadenceExtractsAndWrites(t *testing.T) {
	a, db, ws, _ := newCadenceFixture(t, []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "create", "slug": "cadence-skill", "content": learnerValidSkill}),
		textResp("done"),
	})
	anchor := seedAgentTurns(t, db, []int{4, 4, 4})
	seedSessionRecord(t, db, sessionMessagesFixture())
	a.maybeExtractSkillsCadence(context.Background(), anchor, "u1")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := readSkill(t, ws, "cadence-skill"); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("cadence extraction timed out before writing the skill")
}

// 提取素材必须来自 sessions.messages 完整快照(含未被截断的长内容),
// 而不是被认领 turn 的归档片段。
func TestRunSkillBatchExtractionReplaysOnlyClaimedOwnerTurns(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, []*provider.Response{textResp("Nothing to save.")})
	guestDetail := strings.Repeat("GUEST_MATERIAL_MUST_NOT_LEAK ", 100)
	seedAgentTurnForChatter(t, db, "guest-9", guestDetail, "guest answer", 20)
	longDetail := strings.Repeat("OWNER_WORKFLOW_DETAIL ", 300)
	seedAgentTurnForChatter(t, db, "u1", longDetail, "owner answer one", 6)
	seedAgentTurnForChatter(t, db, "u1", "owner follow-up", "owner answer two", 6)
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 10, 32)
	if err != nil || id == "" || len(refs) != 2 {
		t.Fatalf("claim: id=%q refs=%d err=%v", id, len(refs), err)
	}
	a.runSkillBatchExtraction(context.Background(), id, refs)
	if len(fp.prompts) != 1 {
		t.Fatalf("provider prompts=%d want 1", len(fp.prompts))
	}
	if !strings.Contains(fp.prompts[0], longDetail) {
		t.Fatal("extraction material does not carry claimed owner turn details")
	}
	if strings.Contains(fp.prompts[0], guestDetail) {
		t.Fatal("guest turn leaked into owner extraction material")
	}
}

// 会话行已不存在(被删除):批次消费而非重置,否则会对着空会话无限重试。
func TestRunSkillBatchExtractionSessionGoneConsumesBatch(t *testing.T) {
	a, db, _, fp := newCadenceFixture(t, nil)
	seedAgentTurns(t, db, []int{6, 6})
	seedSessionRecord(t, db, sessionMessagesFixture())
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 10, 32)
	if err != nil || id == "" {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	if err := db.DeleteSession(context.Background(), "u1", "agentA", "s1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	a.runSkillBatchExtraction(context.Background(), id, refs)
	if fp.calls != 0 {
		t.Fatalf("missing session must not reach the LLM, calls=%d", fp.calls)
	}
	id2, _, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "" {
		t.Fatal("missing session should keep the batch consumed, not reset it")
	}
}

func TestRunSkillBatchExtractionResetsOnProviderError(t *testing.T) {
	a, db, _, _ := newCadenceFixture(t, nil)
	a.skillsLearner = NewSkillsLearner(t.TempDir(), &learnerFakeProvider{err: errors.New("provider down")}, "m")
	seedAgentTurns(t, db, []int{6, 6})
	seedSessionRecord(t, db, sessionMessagesFixture())
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 10, 32)
	if err != nil || id == "" {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	a.runSkillBatchExtraction(context.Background(), id, refs)
	id2, refs2, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 10, 32)
	if err != nil || id2 == "" || len(refs2) != 2 {
		t.Fatalf("batch was not reset: id=%q refs=%d err=%v", id2, len(refs2), err)
	}
}

func TestRunSkillBatchExtractionNotWorthyConsumes(t *testing.T) {
	a, db, _, _ := newCadenceFixture(t, []*provider.Response{textResp("Nothing to save.")})
	seedAgentTurns(t, db, []int{6, 6})
	seedSessionRecord(t, db, sessionMessagesFixture())
	id, refs, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 10, 32)
	if err != nil || id == "" {
		t.Fatalf("claim: id=%q err=%v", id, err)
	}
	a.runSkillBatchExtraction(context.Background(), id, refs)
	id2, _, err := db.ClaimSkillBatch(context.Background(), "agentA", "s1", "u1", 1, 32)
	if err != nil {
		t.Fatal(err)
	}
	if id2 != "" {
		t.Fatal("not-worthy extraction should keep the batch consumed")
	}
}
