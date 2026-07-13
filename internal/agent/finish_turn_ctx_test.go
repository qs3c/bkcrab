package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/session"
	"github.com/qs3c/bkcrab/internal/store"
)

type selectiveFailSessionStore struct {
	session.SessionStore
	mu        sync.Mutex
	saveCalls int
	failCalls map[int]error
}

func (s *selectiveFailSessionStore) SaveSession(ctx context.Context, agentID, sessionKey, channel, accountID, chatID, projectID string, messages []provider.Message) error {
	s.mu.Lock()
	s.saveCalls++
	call := s.saveCalls
	err := s.failCalls[call]
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.SessionStore.SaveSession(ctx, agentID, sessionKey, channel, accountID, chatID, projectID, messages)
}

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

func TestFinishTurnRequiresFinalSessionSnapshot(t *testing.T) {
	finalSaveErr := errors.New("final assistant snapshot failed")
	for _, tc := range []struct {
		name         string
		failCalls    map[int]error
		wantFinished bool
		wantStatus   string
		wantMessages int
	}{
		{
			name:         "retry succeeds before done",
			failCalls:    map[int]error{2: finalSaveErr},
			wantFinished: true,
			wantStatus:   "done",
			wantMessages: 2,
		},
		{
			name:         "persistent failure cancels anchor",
			failCalls:    map[int]error{2: finalSaveErr, 3: finalSaveErr},
			wantFinished: false,
			wantStatus:   "",
			wantMessages: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			const uid, agentID, sk = "u1", "agentA", "final-save-session"
			adapter := session.NewStoreAdapter(db, uid)
			failing := &selectiveFailSessionStore{SessionStore: adapter, failCalls: tc.failCalls}
			mgr := session.NewManagerWithStoreForUser(dir, failing, uid, agentID)
			sess := mgr.GetByKey(sk)
			sess.SetChatter(uid)
			seq, err := sess.AppendTurnAnchor(provider.Message{Role: "user", Content: "question"})
			if err != nil {
				t.Fatalf("append anchor: %v", err)
			}
			// This is the final append of the turn. Its SaveSession failure is not
			// returned by Append, so finalization must consult Session's version.
			sess.Append(provider.Message{Role: "assistant", Content: "final answer"})
			// A concurrent/cache-refresh lookup must not overwrite the dirty
			// in-memory version with the older durable blob.
			if got := mgr.GetByKey(sk).GetMessages(); len(got) != 2 || got[1].Content != "final answer" {
				t.Fatalf("dirty snapshot was overwritten during reload: %#v", got)
			}

			a := &Agent{name: agentID, ownerUserID: uid, dataStore: db}
			finished := a.finishTurnAndMaybeExtract(context.Background(), nil, &turnAnchor{sessionKey: sk, seq: seq, sess: sess}, 0)
			if finished != tc.wantFinished {
				t.Fatalf("finished=%v want=%v", finished, tc.wantFinished)
			}
			var status string
			if err := db.DB().QueryRow(`SELECT turn_status FROM session_messages
				WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`, uid, agentID, sk, seq).Scan(&status); err != nil {
				t.Fatal(err)
			}
			if status != tc.wantStatus {
				t.Fatalf("anchor status=%q want=%q", status, tc.wantStatus)
			}
			rec, err := db.GetSession(context.Background(), uid, agentID, sk)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if len(rec.Messages) != tc.wantMessages {
				t.Fatalf("durable messages=%#v want count=%d", rec.Messages, tc.wantMessages)
			}
		})
	}
}
