package agent

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

// TestFinishTurnSurvivesCanceledRequestCtx 复现侧边栏"转圈不停"的根因。
//
// 流式回合里,post-turn goroutine 在 SSE 响应送达之后才跑 —— 那时 HTTP
// 请求的 ctx 往往已被取消(客户端读完流 / 断开,handler 返回)。turn 起点的
// 锚点(turn_status='running')走 Session 自建的 background ctx 总能落库,但
// turn 结束的 FinishTurn 若仍用这个会被取消的请求 ctx,UPDATE 会被中止,
// 锚点永远翻不成 done,会话列表就一直显示 running 的转圈图标。
//
// 因此:即便传入一个已取消的 ctx,finishTurnAndMaybeExtract 也必须把锚点
// 翻成 done。
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

	uid, agentID, sk := "u1", "agentA", "sess1"
	if err := db.SaveSession(context.Background(), uid, agentID, sk, &store.SessionRecord{
		Messages: []store.SessionMessage{{Role: "user", Content: "hi"}},
	}); err != nil {
		t.Fatalf("save session: %v", err)
	}
	seq, err := db.AppendTurnAnchor(context.Background(), uid, agentID, sk,
		store.SessionMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("append anchor: %v", err)
	}

	a := &Agent{name: agentID, ownerUserID: uid, dataStore: db}

	// 模拟回合结束:请求 ctx 已被取消。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.finishTurnAndMaybeExtract(ctx, nil, &turnAnchor{sessionKey: sk, seq: seq}, 0)

	metas, err := db.ListSessions(context.Background(), uid, agentID)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var got string
	for _, m := range metas {
		if m.Key == sk {
			got = m.LastTurnStatus
		}
	}
	if got != "done" {
		t.Fatalf("turn status = %q; want done (锚点必须在请求 ctx 取消后仍翻成 done)", got)
	}
}
