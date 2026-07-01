package tools

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

// TestForTurnIsolatesCronMessageContext 确认 cron 工具（create_cron_job 读
// r.MessageChannel()/MessageChatID() 等每回合状态）在 forTurn 后按回合隔离：
// turnA 创建的定时任务记录的是 turnA 的总线地址，不会被随后绑定的 turnB 串改。
// 这正是"定时提醒回错频道/会话"这类并发误路由的回归保护。
func TestForTurnIsolatesCronMessageContext(t *testing.T) {
	dsn := fmt.Sprintf("file:cron_iso_%d?mode=memory&cache=shared", time.Now().UnixNano())
	st, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	parent := NewRegistry("", "")
	RegisterCronTools(parent, st, "user-1", "agent-cron-iso")

	turnA := parent.ForTurn()
	turnA.SetMessageContext("chanA", "chatA")
	turnA.SetChatterUserID("user-1")
	// turnB 在 turnA 之后绑定——模拟并发回合交错。
	turnB := parent.ForTurn()
	turnB.SetMessageContext("chanB", "chatB")
	turnB.SetChatterUserID("user-1")

	if _, err := turnA.Execute(ctx, "create_cron_job",
		`{"name":"remind","schedule":"30m","message":"hi","type":"interval"}`); err != nil {
		t.Fatalf("turnA create_cron_job: %v", err)
	}

	jobs, err := st.ListCronJobsByAgent(ctx, "agent-cron-iso")
	if err != nil {
		t.Fatalf("list cron jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d cron jobs, want 1", len(jobs))
	}
	if jobs[0].Channel != "chanA" || jobs[0].ChatID != "chatA" {
		t.Errorf("cron job bus address = (%q,%q), want (chanA,chatA)；turnB 的总线地址串入了 turnA",
			jobs[0].Channel, jobs[0].ChatID)
	}
}
