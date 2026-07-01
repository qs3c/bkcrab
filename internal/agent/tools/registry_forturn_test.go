package tools

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/workspace"
)

// readSessionTodo 通过 workspace store 读回某会话的 todo.md。
// write_file("todo.md") 走 workspaceStore.Put(agentID, "", sessionID, "todo.md")，
// 因此读回也用同样的 (agentID, "", sessionID) 作用域。
func readSessionTodo(t *testing.T, ws workspace.Store, agentID, sessionID string) string {
	t.Helper()
	rc, err := ws.Get(context.Background(), agentID, "", sessionID, "todo.md")
	if err != nil {
		t.Fatalf("get todo.md for session %q: %v", sessionID, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read todo.md for session %q: %v", sessionID, err)
	}
	return string(b)
}

// TestForTurnIsolatesConcurrentSessionWrites 复现"多会话共用 todo.md"根因：
// 同一 agent 的两个回合各自 forTurn() 出独立的 registry 并绑定不同 sessionID，
// 一个回合绑定/写入不能污染另一个回合的会话作用域。
//
// 在修复前 a.registry 是共享可变状态：SetSessionID 就地改写，turnB 绑定后
// turnA 的 write_file 会落到 turnB 的会话目录。forTurn() 给每个回合独立的
// sessionID + 独立的工具闭包，从而隔离。
func TestForTurnIsolatesConcurrentSessionWrites(t *testing.T) {
	ws := workspace.NewLocalFS(t.TempDir())
	parent := NewRegistry("", "")
	parent.SetWorkspaceStore(ws, "agent-x")

	turnA := parent.ForTurn()
	turnA.SetSessionID("sess-A")
	// turnB 在 turnA 之后绑定——模拟并发 bindSession 交错。
	turnB := parent.ForTurn()
	turnB.SetSessionID("sess-B")

	if _, err := turnA.Execute(context.Background(), "write_file", `{"path":"todo.md","content":"PLAN-A"}`); err != nil {
		t.Fatalf("turnA write_file: %v", err)
	}
	if _, err := turnB.Execute(context.Background(), "write_file", `{"path":"todo.md","content":"PLAN-B"}`); err != nil {
		t.Fatalf("turnB write_file: %v", err)
	}

	if got := readSessionTodo(t, ws, "agent-x", "sess-A"); got != "PLAN-A" {
		t.Errorf("sess-A todo.md = %q, want %q (turnB 的会话上下文串入了 turnA)", got, "PLAN-A")
	}
	if got := readSessionTodo(t, ws, "agent-x", "sess-B"); got != "PLAN-B" {
		t.Errorf("sess-B todo.md = %q, want %q", got, "PLAN-B")
	}
}

// TestForTurnConcurrentWritesNoCrossTalk 在真正并发下验证隔离 + 无数据竞争
// （配合 go test -race 能抓出共享 tools map / 共享字段的竞争）。
func TestForTurnConcurrentWritesNoCrossTalk(t *testing.T) {
	ws := workspace.NewLocalFS(t.TempDir())
	parent := NewRegistry("", "")
	parent.SetWorkspaceStore(ws, "agent-x")

	const n = 24
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rt := parent.ForTurn()
			rt.SetSessionID(fmt.Sprintf("sess-%d", i))
			args := fmt.Sprintf(`{"path":"todo.md","content":"PLAN-%d"}`, i)
			if _, err := rt.Execute(context.Background(), "write_file", args); err != nil {
				t.Errorf("write sess-%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		want := fmt.Sprintf("PLAN-%d", i)
		if got := readSessionTodo(t, ws, "agent-x", fmt.Sprintf("sess-%d", i)); got != want {
			t.Errorf("sess-%d todo.md = %q, want %q", i, got, want)
		}
	}
}
