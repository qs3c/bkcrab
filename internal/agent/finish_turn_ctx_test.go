package agent

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/usage"
)

// TestFinishTurnSurvivesCanceledRequestCtx 复现并锁定:流式回合在后台 goroutine 里
// 调 finishTurnAndMaybeExtract,此时 HTTP 请求 ctx 已随流结束被取消。把锚点翻成
// done 是必须落库的记账(否则起点 user 消息永远卡在 turn_status='running',且提取
// 节拍永远认领不到这一批),不能随请求取消而失败。
func TestFinishTurnSurvivesCanceledRequestCtx(t *testing.T) {
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

	uid, name, sk := "u1", "agentA", "sess1"
	seq, err := db.AppendTurnAnchor(context.Background(), uid, name, sk,
		store.SessionMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("append anchor: %v", err)
	}

	a := &Agent{name: name, ownerUserID: uid, dataStore: db}

	// 模拟流式回合收尾:HTTP 请求 ctx 已取消,后台 goroutine 才跑收尾记账。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.finishTurnAndMaybeExtract(ctx, nil, &turnAnchor{sessionKey: sk, seq: seq})

	// 用独立连接直接查归档表,确认锚点已翻 done。
	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close()
	var status string
	if err := probe.QueryRow(
		`SELECT turn_status FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`,
		uid, name, sk, seq).Scan(&status); err != nil {
		t.Fatalf("read turn_status: %v", err)
	}
	if status != "done" {
		t.Fatalf("turn_status = %q, want \"done\" (FinishTurn 必须脱离请求 ctx 的取消)", status)
	}
}

// TestMeterTokensSurvivesCanceledRequestCtx 锁定同类问题:流式回合的最终回复在后台
// goroutine 里计费,此时 HTTP 请求 ctx 已取消。token 记账是写后即忘、必须落库的副作用
// (LLM 调用已产生费用),不能随请求取消而丢账。
func TestMeterTokensSurvivesCanceledRequestCtx(t *testing.T) {
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

	a := &Agent{
		name:        "agentA",
		agentID:     "agentA",
		ownerUserID: "u1",
		model:       "anthropic/claude",
		meter:       usage.NewSQLMeter(db.DB(), db.Dialect()),
	}

	// 模拟流式回合收尾:HTTP 请求 ctx 已取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.meterTokens(ctx, "sess1", provider.Usage{InputTokens: 10, OutputTokens: 5})

	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open probe: %v", err)
	}
	defer probe.Close()
	var input, output int64
	if err := probe.QueryRow(
		`SELECT input_tokens, output_tokens FROM token_usage_daily WHERE agent_id=? AND session_key=?`,
		"agentA", "sess1").Scan(&input, &output); err != nil {
		t.Fatalf("read token_usage_daily: %v", err)
	}
	if input != 10 || output != 5 {
		t.Fatalf("tokens = (%d,%d), want (10,5) (meterTokens 必须脱离请求 ctx 的取消)", input, output)
	}
}
