package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestToolRecallUsesOnlySessionMessages(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	if exists, err := db.tableExists(ctx, "context_archives"); err != nil || exists {
		t.Fatalf("obsolete archive table should not be created: exists=%v err=%v", exists, err)
	}
	f := seedRecallFixture(t, db)
	if err := db.DeleteSession(ctx, f.userA, f.agentA, f.session1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSessionToolMessage(ctx, f.userA, f.agentA, f.session1, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session remained recallable: %v", err)
	}
}

func TestToolRecallSchemaDoesNotCreateArchiveCopies(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", mysqlDialect} {
		t.Run(dialect, func(t *testing.T) {
			st := &DBStore{dialect: dialect}
			statements := st.migrationSQL()
			if dialect == mysqlDialect {
				statements = mysqlMigrationSQL()
			}
			for _, statement := range statements {
				if strings.Contains(statement, "context_archives") {
					t.Fatal("schema still creates redundant context archives")
				}
			}
		})
	}
}

// recallFixture 是一次 seed 用到的标识符。全部带 t.Name() 前缀:openTestDB
// 给的是进程内共享的 `file::memory:?cache=shared`,同包的测试可能落在同一个
// 库上,不加命名空间会互相污染行数断言。
type recallFixture struct {
	userA, userB   string
	agentA, agentB string
	session1       string
	session2       string
}

// seedRecallFixture 铺一个刻意"容易串号"的现场:两个用户、两个 agent、两个
// 会话,每个会话里的 seq 都从 0 开始重新计数,所以同一个 seq 在多处同时存在。
// 这正是回溯查询必须靠三元组而不是靠 seq 本身来定位的原因。
func seedRecallFixture(t *testing.T, db *DBStore) recallFixture {
	t.Helper()
	ctx := context.Background()
	ns := t.Name()
	f := recallFixture{
		userA:    ns + "/user-a",
		userB:    ns + "/user-b",
		agentA:   ns + "/agent-A",
		agentB:   ns + "/agent-B",
		session1: ns + "/session-1",
		session2: ns + "/session-2",
	}
	seed := func(uid, agent, key string, msgs ...SessionMessage) {
		t.Helper()
		for _, m := range msgs {
			if err := db.AppendSessionMessage(ctx, uid, agent, key, m); err != nil {
				t.Fatalf("append %s/%s/%s: %v", uid, agent, key, err)
			}
		}
	}
	// userA / agentA / session1:seq 0=user, 1=tool, 2=assistant, 3=tool
	seed(f.userA, f.agentA, f.session1,
		SessionMessage{Role: "user", Content: "run the tests"},
		SessionMessage{Role: "tool", ToolCallID: "call-1", Name: "exec", Content: "MINE: session-1 exec output"},
		SessionMessage{Role: "assistant", Content: "tests pass"},
		SessionMessage{Role: "tool", ToolCallID: "call-2", Name: "read_file", Content: "MINE: session-1 file body"},
	)
	// 同一用户的另一个会话——seq 又从 0 开始。
	seed(f.userA, f.agentA, f.session2,
		SessionMessage{Role: "user", Content: "other thread"},
		SessionMessage{Role: "tool", ToolCallID: "call-9", Name: "exec", Content: "OTHER SESSION secret"},
	)
	// 另一个用户,同一个 agent,而且 session_key 故意取成同名。
	seed(f.userB, f.agentA, f.session1,
		SessionMessage{Role: "user", Content: "b thread"},
		SessionMessage{Role: "tool", ToolCallID: "call-b", Name: "exec", Content: "OTHER USER secret"},
	)
	// 同一用户,另一个 agent,同名 session_key。
	seed(f.userA, f.agentB, f.session1,
		SessionMessage{Role: "tool", ToolCallID: "call-x", Name: "exec", Content: "OTHER AGENT secret"},
	)
	return f
}

// TestGetSessionToolMessageIsPinnedToTriple 是这条 SQL 的核心断言:seq 是
// 每会话计数器,同一个 seq 在别的用户/agent/会话里都存在且内容不同。查询
// 必须靠三元组定位,只拿 seq 绝不能捞到别人的行。
func TestGetSessionToolMessageIsPinnedToTriple(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	f := seedRecallFixture(t, db)

	got, err := db.GetSessionToolMessage(ctx, f.userA, f.agentA, f.session1, "", 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != "MINE: session-1 exec output" {
		t.Fatalf("wrong row for seq 1: %q", got.Content)
	}

	// 同一个 seq=1,换任意一个作用域字段,都必须落到那个作用域自己的行上
	// (或者没有),绝不能返回上面那条。
	for _, tc := range []struct{ user, agent, key string }{
		{f.userB, f.agentA, f.session1},
		{f.userA, f.agentB, f.session1},
		{f.userA, f.agentA, f.session2},
	} {
		got, err := db.GetSessionToolMessage(ctx, tc.user, tc.agent, tc.key, "", 1)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			t.Fatalf("get %v: %v", tc, err)
		}
		if got.Content == "MINE: session-1 exec output" {
			t.Fatalf("scope %v leaked another session's row", tc)
		}
	}
}

// TestGetSessionToolMessageRejectsNonToolRows 锁住 role='tool' 这道硬编码
// 的门。放开它等于给模型一条绕过压缩边界、把整段对话原文重放回上下文的路。
func TestGetSessionToolMessageRejectsNonToolRows(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	f := seedRecallFixture(t, db)

	// seq 0 是 role='user',seq 2 是 role='assistant'。
	for _, seq := range []int64{0, 2} {
		_, err := db.GetSessionToolMessage(ctx, f.userA, f.agentA, f.session1, "", seq)
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("seq %d (non-tool row) was reachable: err=%v", seq, err)
		}
	}
}

func TestListSessionToolRefsOnlyListsOwnToolRows(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	f := seedRecallFixture(t, db)

	refs, err := db.ListSessionToolRefs(ctx, f.userA, f.agentA, f.session1, "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("ref count = %d, want 2 (only the tool rows of session-1): %+v", len(refs), refs)
	}
	if refs[0].Seq != 1 || refs[0].ToolCallID != "call-1" || refs[0].Name != "exec" {
		t.Fatalf("first ref mismatch: %+v", refs[0])
	}
	if refs[1].Seq != 3 || refs[1].ToolCallID != "call-2" || refs[1].Name != "read_file" {
		t.Fatalf("second ref mismatch: %+v", refs[1])
	}
	if refs[0].Chars == 0 {
		t.Fatalf("Chars was not populated: %+v", refs[0])
	}
}

// TestToolRecallChatterScope 覆盖群聊/共享 agent:同一个 session_key 下有
// 多个发言人时,带聊天者约束的查询只能看到自己的行。
func TestToolRecallChatterScope(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	ns := t.Name()
	uid, agent, key := ns+"/owner-1", ns+"/agent-A", ns+"/group-1"
	appendAs := func(chatter string, m SessionMessage) {
		t.Helper()
		c := ctx
		if chatter != "" {
			c = WithChatterUserID(ctx, chatter)
		}
		if err := db.AppendSessionMessage(c, uid, agent, key, m); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	appendAs("chatter-a", SessionMessage{Role: "tool", ToolCallID: "ca", Name: "exec", Content: "A's output"})
	appendAs("chatter-b", SessionMessage{Role: "tool", ToolCallID: "cb", Name: "exec", Content: "B's output"})
	// 历史存量行:chatter_user_id 为空,按既有约定回落到 user_id。
	appendAs("", SessionMessage{Role: "tool", ToolCallID: "cl", Name: "exec", Content: "legacy output"})

	refs, err := db.ListSessionToolRefs(ctx, uid, agent, key, "chatter-a")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].ToolCallID != "ca" {
		t.Fatalf("chatter-a should see exactly its own row, got %+v", refs)
	}

	// B 的行对 A 不可见——这是隔离的要点。
	if _, err := db.GetSessionToolMessage(ctx, uid, agent, key, "chatter-a", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("chatter-a reached chatter-b's row: err=%v", err)
	}

	// 空 chatter_user_id 的历史行回落到 user_id,因此只有拥有者匹配到它——
	// 不能对每个聊天者都可见,那等于把隔离打穿。
	if _, err := db.GetSessionToolMessage(ctx, uid, agent, key, "chatter-a", 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("legacy row with empty chatter leaked to chatter-a: err=%v", err)
	}
	legacy, err := db.GetSessionToolMessage(ctx, uid, agent, key, uid, 2)
	if err != nil {
		t.Fatalf("owner should reach the legacy row: %v", err)
	}
	if legacy.Content != "legacy output" {
		t.Fatalf("legacy row mismatch: %q", legacy.Content)
	}

	// 不带聊天者约束时看到全部三条(拥有者路径)。
	all, err := db.ListSessionToolRefs(ctx, uid, agent, key, "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("unscoped list = %d rows, want 3", len(all))
	}
}

func TestToolRecallRequiresFullScope(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()

	for _, tc := range []struct{ user, agent, key string }{
		{"", "agent-A", "session-1"},
		{"user-a", "", "session-1"},
		{"user-a", "agent-A", ""},
	} {
		if _, err := db.ListSessionToolRefs(ctx, tc.user, tc.agent, tc.key, ""); err == nil {
			t.Fatalf("ListSessionToolRefs accepted an incomplete scope: %+v", tc)
		}
		if _, err := db.GetSessionToolMessage(ctx, tc.user, tc.agent, tc.key, "", 0); err == nil {
			t.Fatalf("GetSessionToolMessage accepted an incomplete scope: %+v", tc)
		}
	}
}
