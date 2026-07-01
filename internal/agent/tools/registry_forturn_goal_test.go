package tools

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent/goal"
)

// seedActiveGoal 直接插入一条指定会话的 Active 目标。
func seedActiveGoal(t *testing.T, st *memGoalStore, sessionKey string) {
	t.Helper()
	if err := st.CreateGoal(context.Background(), &goal.Goal{
		ID:          "g-" + sessionKey,
		AgentID:     "agent-A",
		SessionKey:  sessionKey,
		OwnerUserID: "user-1",
		Objective:   "do it",
		Status:      goal.StatusActive,
	}); err != nil {
		t.Fatalf("seed goal %q: %v", sessionKey, err)
	}
}

// TestForTurnIsolatesGoalSessionKey 确认 goal 工具（唯一读每回合状态的非内置
// 工具）在 forTurn 后也按回合隔离：turnA 完成的是自己会话的目标，turnB 后续
// 绑定不会把 turnA 的 update_goal 串到 turnB 的会话。
func TestForTurnIsolatesGoalSessionKey(t *testing.T) {
	parent := NewRegistry("", "")
	st := newMemGoalStore()
	RegisterGoalTools(parent, st, "agent-A")
	seedActiveGoal(t, st, "s-A")
	seedActiveGoal(t, st, "s-B")

	turnA := parent.ForTurn()
	turnA.SetGoalSessionKey("s-A")
	turnB := parent.ForTurn()
	turnB.SetGoalSessionKey("s-B")

	if _, err := turnA.Execute(context.Background(), "update_goal", `{"status":"complete"}`); err != nil {
		t.Fatalf("turnA update_goal: %v", err)
	}

	ga, err := st.GetGoalBySession(context.Background(), "agent-A", "s-A")
	if err != nil {
		t.Fatalf("get s-A goal: %v", err)
	}
	if ga.Status != goal.StatusComplete {
		t.Errorf("s-A goal status = %q, want complete", ga.Status)
	}
	gb, err := st.GetGoalBySession(context.Background(), "agent-A", "s-B")
	if err != nil {
		t.Fatalf("get s-B goal: %v", err)
	}
	if gb.Status == goal.StatusComplete {
		t.Errorf("s-B goal 被 turnA 串改为 complete；应保持 active（goal 未按回合隔离）")
	}
}
