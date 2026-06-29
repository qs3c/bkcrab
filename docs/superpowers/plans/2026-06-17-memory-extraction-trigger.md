# 记忆提取触发机制重构 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把按"用户消息计数取模"触发的记忆提取,改为按"完整执行完毕的 turn"触发——每累计 5 个未提取的已完成 turn 在单个写事务内原子认领并异步提取,提取输入从归档表按 turn 区间回放。

**Architecture:** 在 `session_messages` 加 `turn_status`(`''｜running｜done`,白名单默认值)与 `extraction_id`(NULL=未提取)两列;turn 起点写 `running` 锚点并拿回 seq、终点标 `done`,然后在事务内数够 5 个就 uuid 认领、异步提取,失败补偿重置 `extraction_id`。三后端(MySQL 默认 / PostgreSQL / SQLite)用代码库已有的 dialect 分支与部分索引降级套路落地。

**Tech Stack:** Go,`database/sql`(modernc.org/sqlite / lib-pq / go-sql-driver-mysql),标准库 `testing`。

**关联规格:** [docs/superpowers/specs/2026-06-17-memory-extraction-trigger-design.md](../specs/2026-06-17-memory-extraction-trigger-design.md)

---

## File Structure

| 文件 | 职责 | 改动 |
|------|------|------|
| `internal/store/database.go` | SQLite/PG 建表 DDL、迁移、DBStore 方法实现 | 加列 DDL、新迁移函数、5 个新方法、删 `CountChatterUserMessages` |
| `internal/store/database_mysql.go` | MySQL 手写建表 DDL | `session_messages` 加两列 |
| `internal/store/store.go` | `Store` 接口、`SessionMessage`/`TurnRef` 类型 | 增删接口方法、加 `TurnRef` 类型 |
| `internal/session/store_adapter.go` | `Session` → `store.Store` 适配 | 加锚点写入适配方法 |
| `internal/session/manager.go` | `Session`、`SessionStore` 接口 | 加 `AppendTurnAnchor` |
| `internal/agent/loop.go` | turn 起点写锚点、`runPostTurn` 触发 | 改两处锚点写入、改触发门、加 `turnAnchor` |
| `internal/agent/memory.go` | `AutoPersistMemory` 提取 | 输入改为归档批次、去掉 last-20 截断、加 error 返回 |

**约定(贯穿全程):**
- 行号会随改动漂移,**按代码内容定位**,不要盯死行号。
- 每个任务结束都 `go build ./...` 通过 + 该任务的测试通过才提交。
- 提交信息用中文 + conventional 前缀,结尾保留 `Co-Authored-By` 行(见各任务的 commit 步骤)。
- 后端方言判断一律用 `d.dialect == mysqlDialect` / `d.dialect == "postgres"`,其余落 SQLite 分支(沿用现有写法)。

---

## Task 1: 新装库 DDL 加两列(SQLite/PG + MySQL)

**Files:**
- Modify: `internal/store/database.go`(`migrationSQL()` 内 `CREATE TABLE IF NOT EXISTS session_messages`,约 1310–1334)
- Modify: `internal/store/database_mysql.go`(`mysqlMigrationSQL()` 内 `CREATE TABLE IF NOT EXISTS session_messages`,约 153–172)
- Test: `internal/store/turn_columns_test.go`(新建)

- [ ] **Step 1: 写失败测试**

新建 `internal/store/turn_columns_test.go`:

```go
package store

import (
	"context"
	"testing"
)

// newTestSQLite 打开一个内存外的临时 SQLite 库并迁移。
func newTestSQLite(t *testing.T) *DBStore {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + dir + "/test.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestSessionMessagesHasTurnColumns(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	for _, col := range []string{"turn_status", "extraction_id"} {
		has, err := db.tableHasColumn(ctx, "session_messages", col)
		if err != nil {
			t.Fatalf("tableHasColumn(%s): %v", col, err)
		}
		if !has {
			t.Fatalf("session_messages missing column %s", col)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestSessionMessagesHasTurnColumns -v`
Expected: FAIL —`session_messages missing column turn_status`。

- [ ] **Step 3: 在 SQLite/PG 建表 DDL 加两列**

在 `database.go` 的 `CREATE TABLE IF NOT EXISTS session_messages (...)` 里,`chatter_user_id TEXT NOT NULL DEFAULT '',` 之后、`PRIMARY KEY (...)` 之前插入:

```sql
				-- turn_status 标记锚点行(代表一个完整 turn 的用户消息):
				-- '' = 非锚点(steer / 群聊注入 / goal_context / 历史存量行),
				-- 'running' = 锚点,turn 进行中, 'done' = 锚点,turn 已完成。
				-- 白名单语义:只有 turn 起点显式写 'running',其余默认 ''。
				turn_status TEXT NOT NULL DEFAULT '',
				-- extraction_id:NULL = 未被任何记忆提取认领;非 NULL(uuid)= 已认领。
				extraction_id TEXT,
```

- [ ] **Step 4: 在 MySQL 建表 DDL 加两列**

在 `database_mysql.go` 的 `CREATE TABLE IF NOT EXISTS session_messages (...)` 里,`chatter_user_id VARCHAR(120) NOT NULL DEFAULT '',` 之后、`PRIMARY KEY (...)` 之前插入:

```sql
				turn_status VARCHAR(16) NOT NULL DEFAULT '',
				extraction_id VARCHAR(64) NULL,
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestSessionMessagesHasTurnColumns -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/store/database.go internal/store/database_mysql.go internal/store/turn_columns_test.go
git commit -F - <<'EOF'
feat(store): session_messages 新装 DDL 增加 turn_status / extraction_id 两列

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 2: 存量库迁移 + pending 索引

**Files:**
- Modify: `internal/store/database.go`(新增 `migrateSessionMessagesAddTurnColumns`;在 `Migrate()` 中注册)
- Test: `internal/store/turn_columns_test.go`(追加用例)

- [ ] **Step 1: 写失败测试(幂等 + 索引存在)**

在 `turn_columns_test.go` 追加:

```go
func TestMigrateTurnColumnsIdempotent(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	// 再迁移一次必须无错(幂等)。
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// pending 索引必须存在。
	var name string
	err := db.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='index' AND name='idx_sm_pending'`).Scan(&name)
	if err != nil {
		t.Fatalf("idx_sm_pending not found: %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestMigrateTurnColumnsIdempotent -v`
Expected: FAIL —`idx_sm_pending not found`。

- [ ] **Step 3: 新增迁移函数**

在 `database.go` 紧跟 `migrateSessionMessagesAddOrigin` 之后新增(仿其结构):

```go
// migrateSessionMessagesAddTurnColumns 将 turn_status / extraction_id 列改装到
// 旧的 session_messages 表上,并建立"待提取"部分索引。turn_status 默认 ''
// (非锚点),extraction_id 默认 NULL(未提取),因此历史存量行无需回填:
// 它们既不被计数也不被提取。幂等:列已存在则跳过 ALTER,索引用
// IF NOT EXISTS / MySQL 重复忽略兜底。
func (d *DBStore) migrateSessionMessagesAddTurnColumns(ctx context.Context) error {
	textType := "TEXT"
	varchar16 := "TEXT"
	varchar64 := "TEXT"
	if d.dialect == mysqlDialect {
		textType = "VARCHAR(16)"
		varchar16 = "VARCHAR(16)"
		varchar64 = "VARCHAR(64)"
	}
	_ = textType
	has, err := d.tableHasColumn(ctx, "session_messages", "turn_status")
	if err != nil {
		return err
	}
	if !has {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN turn_status %s NOT NULL DEFAULT ''`, varchar16)); err != nil {
			return fmt.Errorf("add turn_status: %w", err)
		}
	}
	has, err = d.tableHasColumn(ctx, "session_messages", "extraction_id")
	if err != nil {
		return err
	}
	if !has {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN extraction_id %s`, varchar64)); err != nil {
			return fmt.Errorf("add extraction_id: %w", err)
		}
	}
	// 待提取索引。SQLite/PG 用部分索引(只收录待提取行,旧行不膨胀);
	// MySQL 不支持部分索引,降级为普通复合索引(参见 chatter 索引同款处理)。
	idx := `CREATE INDEX IF NOT EXISTS idx_sm_pending ON session_messages (agent_id, chatter_user_id) WHERE turn_status = 'done' AND extraction_id IS NULL`
	if d.dialect == mysqlDialect {
		idx = `CREATE INDEX IF NOT EXISTS idx_sm_pending ON session_messages (agent_id, chatter_user_id, turn_status)`
	}
	if err := d.execDDL(ctx, idx); err != nil {
		return fmt.Errorf("create idx_sm_pending: %w", err)
	}
	return nil
}
```

> 注:`textType` 局部变量与 `_ = textType` 是为可读性保留的占位,可直接删去这两行——只用 `varchar16` / `varchar64`。实现时请删掉这两行冗余,保持简洁。

精简版(实现时采用这个,去掉冗余):

```go
func (d *DBStore) migrateSessionMessagesAddTurnColumns(ctx context.Context) error {
	statusType, idType := "TEXT", "TEXT"
	if d.dialect == mysqlDialect {
		statusType, idType = "VARCHAR(16)", "VARCHAR(64)"
	}
	if has, err := d.tableHasColumn(ctx, "session_messages", "turn_status"); err != nil {
		return err
	} else if !has {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN turn_status %s NOT NULL DEFAULT ''`, statusType)); err != nil {
			return fmt.Errorf("add turn_status: %w", err)
		}
	}
	if has, err := d.tableHasColumn(ctx, "session_messages", "extraction_id"); err != nil {
		return err
	} else if !has {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN extraction_id %s`, idType)); err != nil {
			return fmt.Errorf("add extraction_id: %w", err)
		}
	}
	idx := `CREATE INDEX IF NOT EXISTS idx_sm_pending ON session_messages (agent_id, chatter_user_id) WHERE turn_status = 'done' AND extraction_id IS NULL`
	if d.dialect == mysqlDialect {
		idx = `CREATE INDEX IF NOT EXISTS idx_sm_pending ON session_messages (agent_id, chatter_user_id, turn_status)`
	}
	if err := d.execDDL(ctx, idx); err != nil {
		return fmt.Errorf("create idx_sm_pending: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: 在 Migrate() 注册**

在 `Migrate()` 的 `migrateSessionMessagesAddOrigin` 调用块之后插入:

```go
	if err := d.migrateSessionMessagesAddTurnColumns(ctx); err != nil {
		return fmt.Errorf("migrate session_messages turn columns: %w", err)
	}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/store/ -run 'TestSessionMessagesHasTurnColumns|TestMigrateTurnColumnsIdempotent' -v`
Expected: 两个都 PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/store/database.go internal/store/turn_columns_test.go
git commit -F - <<'EOF'
feat(store): 存量库 turn 列迁移 + idx_sm_pending 部分索引(MySQL 降级)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 3: `AppendTurnAnchor` —— 写 running 锚点并返回 seq

**Files:**
- Modify: `internal/store/store.go`(`Store` 接口加方法)
- Modify: `internal/store/database.go`(`DBStore` 实现,仿 `AppendSessionEvent`)
- Test: `internal/store/turn_anchor_test.go`(新建)

- [ ] **Step 1: 写失败测试**

新建 `internal/store/turn_anchor_test.go`:

```go
package store

import (
	"context"
	"testing"
)

func TestAppendTurnAnchorReturnsSeqAndStatus(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"

	seq0, err := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor 0: %v", err)
	}
	seq1, err := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "again"})
	if err != nil {
		t.Fatalf("anchor 1: %v", err)
	}
	if seq0 != 0 || seq1 != 1 {
		t.Fatalf("seq want 0,1 got %d,%d", seq0, seq1)
	}
	var status string
	if err := db.db.QueryRowContext(ctx,
		`SELECT turn_status FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`,
		uid, agent, sk, seq0).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "running" {
		t.Fatalf("turn_status want running got %q", status)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestAppendTurnAnchorReturnsSeqAndStatus -v`
Expected: FAIL(编译错误:`db.AppendTurnAnchor` 未定义)。

- [ ] **Step 3: 接口加方法**

在 `store.go` 的 `AppendSessionMessage` 行下方加:

```go
	// AppendTurnAnchor 写入一条 turn 起点的用户消息(turn_status='running')
	// 并返回分配的 seq。仅由 turn 起点调用——普通 AppendSessionMessage 的
	// turn_status 默认 ''(非锚点)。seq 在事务内分配,模式同 AppendSessionEvent。
	AppendTurnAnchor(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) (int64, error)
```

- [ ] **Step 4: DBStore 实现**

在 `database.go` 的 `AppendSessionMessage` 函数之后加(列清单与 `AppendSessionMessage` 一致,额外把 `turn_status` 设为 `'running'`):

```go
// AppendTurnAnchor 见接口文档。仿 AppendSessionEvent 的事务内分配 seq 模式,
// 但写的是 session_messages 全列 + turn_status='running'。
func (d *DBStore) AppendTurnAnchor(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) (int64, error) {
	if userID == "" {
		return 0, errors.New("store: AppendTurnAnchor requires user_id")
	}
	contentParts, _ := json.Marshal(msg.ContentParts)
	toolCalls, _ := json.Marshal(msg.ToolCalls)
	metadata, _ := json.Marshal(msg.Metadata)
	rawAssistant := string(msg.RawAssistant)
	ts := msg.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	chatterID := ChatterUserIDFromContext(ctx)
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int64
	if d.dialect == "postgres" {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM session_messages
				WHERE user_id=$1 AND agent_id=$2 AND session_key=$3`,
			userID, agentID, sessionKey).Scan(&seq); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_messages
				(user_id, agent_id, session_key, seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at, chatter_user_id, turn_status)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'running')`,
			userID, agentID, sessionKey, seq, msg.Role, msg.Content, string(contentParts), string(toolCalls),
			msg.ToolCallID, msg.Name, string(metadata), msg.Thinking, rawAssistant, msg.Origin, ts, chatterID); err != nil {
			return 0, err
		}
	} else {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM session_messages
				WHERE user_id=? AND agent_id=? AND session_key=?`,
			userID, agentID, sessionKey).Scan(&seq); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_messages
				(user_id, agent_id, session_key, seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at, chatter_user_id, turn_status)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'running')`,
			userID, agentID, sessionKey, seq, msg.Role, msg.Content, string(contentParts), string(toolCalls),
			msg.ToolCallID, msg.Name, string(metadata), msg.Thinking, rawAssistant, msg.Origin, ts, chatterID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}
```

- [ ] **Step 5: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestAppendTurnAnchorReturnsSeqAndStatus -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/store/store.go internal/store/database.go internal/store/turn_anchor_test.go
git commit -F - <<'EOF'
feat(store): AppendTurnAnchor 写 running 锚点并返回 seq

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 4: `FinishTurn` —— 按主键把锚点翻成 done

**Files:**
- Modify: `internal/store/store.go`、`internal/store/database.go`
- Test: `internal/store/turn_anchor_test.go`(追加)

- [ ] **Step 1: 写失败测试**

追加:

```go
func TestFinishTurnFlipsStatus(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"
	seq, err := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if err := db.FinishTurn(ctx, uid, agent, sk, seq); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var status string
	db.db.QueryRowContext(ctx,
		`SELECT turn_status FROM session_messages WHERE user_id=? AND agent_id=? AND session_key=? AND seq=?`,
		uid, agent, sk, seq).Scan(&status)
	if status != "done" {
		t.Fatalf("turn_status want done got %q", status)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestFinishTurnFlipsStatus -v`
Expected: FAIL(`db.FinishTurn` 未定义)。

- [ ] **Step 3: 接口 + 实现**

`store.go` 接口加:

```go
	// FinishTurn 把锚点行翻成 turn_status='done'(按主键精确定位,避免认错
	// 上次崩溃残留的僵尸 running 行)。turn 结束时由 runPostTurn 调用。
	FinishTurn(ctx context.Context, userID, agentID, sessionKey string, seq int64) error
```

`database.go` 实现:

```go
// FinishTurn 见接口文档。
func (d *DBStore) FinishTurn(ctx context.Context, userID, agentID, sessionKey string, seq int64) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE session_messages SET turn_status='done'
			WHERE user_id=%s AND agent_id=%s AND session_key=%s AND seq=%s AND turn_status='running'`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		userID, agentID, sessionKey, seq)
	return err
}
```

> `AND turn_status='running'` 守卫:只翻进行中的锚点,避免重复/异常调用把已 done 或非锚点行误改。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestFinishTurnFlipsStatus -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/store/store.go internal/store/database.go internal/store/turn_anchor_test.go
git commit -F - <<'EOF'
feat(store): FinishTurn 按主键把锚点翻成 done

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 5: `ClaimCadenceBatch` —— 单事务原子认领

**Files:**
- Modify: `internal/store/store.go`(加 `TurnRef` 类型 + 接口方法)
- Modify: `internal/store/database.go`(实现,三后端锁分支)
- Test: `internal/store/claim_test.go`(新建)

- [ ] **Step 1: 写失败测试(够 N 认领 / 不够空 / 二次认领空)**

新建 `internal/store/claim_test.go`:

```go
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

	// 不够 N:返回空 id、空批次。
	seedDoneTurns(t, db, "agentA", "chatterA", 4)
	id, refs, err := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if err != nil {
		t.Fatalf("claim<5: %v", err)
	}
	if id != "" || len(refs) != 0 {
		t.Fatalf("want no claim, got id=%q refs=%d", id, len(refs))
	}

	// 凑满第 5 个:认领 5 条。
	seedDoneTurns(t, db, "agentA", "chatterA", 1)
	id, refs, err = db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if err != nil {
		t.Fatalf("claim=5: %v", err)
	}
	if id == "" || len(refs) != 5 {
		t.Fatalf("want 5 claimed with id, got id=%q refs=%d", id, len(refs))
	}

	// 二次认领:已被认领,返回空。
	id2, refs2, err := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if id2 != "" || len(refs2) != 0 {
		t.Fatalf("want empty second claim, got id=%q refs=%d", id2, len(refs2))
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run TestClaimCadenceBatch -v`
Expected: FAIL(`db.ClaimCadenceBatch` / `TurnRef` 未定义)。

- [ ] **Step 3: 加 TurnRef 类型**

在 `store.go` `SessionMessage` 类型附近加:

```go
// TurnRef 指向一个已认领 turn 的锚点位置:用于从归档表回放该 turn 的消息区间。
type TurnRef struct {
	SessionKey string
	StartSeq   int64
}
```

- [ ] **Step 4: 接口加方法**

```go
	// ClaimCadenceBatch 在单个写事务内:统计该 (agent, chatter) 下 turn_status='done'
	// 且 extraction_id IS NULL 的锚点,若 >= n 则生成 uuid、对其中至多 cap 条置位
	// extraction_id 并返回 (uuid, 这批 TurnRef)。不足 n 返回 ("", nil, nil)。
	// 事务保证并发 runPostTurn 不会重复认领同一批。
	ClaimCadenceBatch(ctx context.Context, agentID, chatterUserID string, n, cap int) (string, []TurnRef, error)
```

- [ ] **Step 5: DBStore 实现(三后端锁分支)**

`database.go` 需要 uuid。先确认包内已有 uuid 依赖:

Run: `grep -rn "github.com/google/uuid" internal/store/ internal/agent/ go.mod`

若 `internal/store` 未引入,用 `internal/agent` 已用的同一个 uuid 包(下方代码以 `github.com/google/uuid` 为准;若仓库用别的,改成同名调用)。实现:

```go
// ClaimCadenceBatch 见接口文档。
func (d *DBStore) ClaimCadenceBatch(ctx context.Context, agentID, chatterUserID string, n, cap int) (string, []TurnRef, error) {
	if chatterUserID == "" || n <= 0 {
		return "", nil, nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()

	// 选候选(锁定行;SQLite 无 FOR UPDATE,靠写事务序列化)。
	selSQL := `SELECT session_key, seq FROM session_messages
		WHERE agent_id=%s AND chatter_user_id=%s AND turn_status='done' AND extraction_id IS NULL
		ORDER BY created_at, seq LIMIT %d`
	lock := ""
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		lock = " FOR UPDATE"
	}
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf(selSQL+lock, d.ph(1), d.ph(2), cap), agentID, chatterUserID)
	if err != nil {
		return "", nil, err
	}
	var refs []TurnRef
	for rows.Next() {
		var r TurnRef
		if err := rows.Scan(&r.SessionKey, &r.StartSeq); err != nil {
			rows.Close()
			return "", nil, err
		}
		refs = append(refs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(refs) < n {
		return "", nil, nil // 不足门槛,事务回滚(defer)
	}

	id := uuid.NewString()
	for _, r := range refs {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE session_messages SET extraction_id=%s
				WHERE agent_id=%s AND session_key=%s AND seq=%s AND extraction_id IS NULL`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
			id, agentID, r.SessionKey, r.StartSeq); err != nil {
			return "", nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return id, refs, nil
}
```

在 `database.go` import 块补 `"github.com/google/uuid"`(若尚未引入)。

> SQLite 注意:本事务先 SELECT 后 UPDATE,deferred 事务在高并发下可能 `SQLITE_BUSY`。现有 DSN 已带 `busy_timeout(5000)`(factory.go)使写者等待;若压测复现死锁,在 SQLite DSN 追加 `&_txlock=immediate`(modernc 支持),让事务开局即取写锁。本任务不改 DSN,留作运维开关。

- [ ] **Step 6: 跑测试确认通过**

Run: `go test ./internal/store/ -run TestClaimCadenceBatch -v`
Expected: PASS。

- [ ] **Step 7: 并发回归测试**

追加到 `claim_test.go`:

```go
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
	// 10 条 done、N=5、cap=15:可能 1 个认领者拿 10,或 2 个各拿 5。
	// 无论如何:认领的 turn 总数不得超过 10,且无重复(每行 extraction_id 唯一)。
	if claimedTurns > 10 {
		t.Fatalf("over-claimed: %d turns across %d claimers", claimedTurns, claimers)
	}
	// 校验:不存在被两次认领(每行至多一个 extraction_id,DB 天然保证;
	// 这里校验剩余未提取 + 已提取 = 10)。
	var pending, claimed int
	db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE turn_status='done' AND extraction_id IS NULL`).Scan(&pending)
	db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_messages WHERE turn_status='done' AND extraction_id IS NOT NULL`).Scan(&claimed)
	if pending+claimed != 10 || claimed != claimedTurns {
		t.Fatalf("accounting off: pending=%d claimed=%d claimedTurns=%d", pending, claimed, claimedTurns)
	}
}
```

Run: `go test ./internal/store/ -run TestClaimCadenceBatchNoDoubleClaim -race -v`
Expected: PASS(无 data race、无超额认领)。

> 若 SQLite 在 `-race` 高并发下偶发 `SQLITE_BUSY`,这恰好印证 Step 5 的注记;此时给测试库 DSN 加 `&_txlock=immediate` 后重跑。

- [ ] **Step 8: 提交**

```bash
git add internal/store/store.go internal/store/database.go internal/store/claim_test.go
git commit -F - <<'EOF'
feat(store): ClaimCadenceBatch 单事务原子认领(三后端锁分支)

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 6: `ResetExtraction` + `LoadTurnMessages`

**Files:**
- Modify: `internal/store/store.go`、`internal/store/database.go`
- Test: `internal/store/claim_test.go`(追加)

- [ ] **Step 1: 写失败测试**

```go
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
	// 重置后可再次认领。
	id2, refs2, _ := db.ClaimCadenceBatch(ctx, "agentA", "chatterA", 5, 15)
	if id2 == "" || len(refs2) != 5 {
		t.Fatalf("re-claim after reset failed: id=%q refs=%d", id2, len(refs2))
	}
}

func TestLoadTurnMessagesRange(t *testing.T) {
	db := newTestSQLite(t)
	ctx := WithChatterUserID(context.Background(), "chatterA")
	uid, agent, sk := "u1", "agentA", "sess1"
	// turn1 锚点 + 一条助手回复;turn2 锚点。
	seq1, _ := db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q1"})
	db.AppendSessionMessage(ctx, uid, agent, sk, SessionMessage{Role: "assistant", Content: "A1"})
	db.FinishTurn(ctx, uid, agent, sk, seq1)
	db.AppendTurnAnchor(ctx, uid, agent, sk, SessionMessage{Role: "user", Content: "Q2"})

	msgs, err := db.LoadTurnMessages(ctx, uid, agent, []TurnRef{{SessionKey: sk, StartSeq: seq1}})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// turn1 区间 = [seq1, 下一个锚点) = Q1 + A1,不含 Q2。
	if len(msgs) != 2 || msgs[0].Content != "Q1" || msgs[1].Content != "A1" {
		t.Fatalf("unexpected range: %+v", msgs)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/store/ -run 'TestResetExtractionReturnsToPending|TestLoadTurnMessagesRange' -v`
Expected: FAIL(方法未定义)。

- [ ] **Step 3: 接口 + 实现**

`store.go` 接口加:

```go
	// ResetExtraction 把某次提取认领的所有行 extraction_id 重置回 NULL,
	// 使它们回到待提取状态(异步提取失败时的补偿回滚)。
	ResetExtraction(ctx context.Context, extractionID string) error
	// LoadTurnMessages 按 TurnRef 列表从归档表回放每个 turn 的消息区间:
	// 同 session 内 [StartSeq, 下一个锚点 seq)(无下一个锚点则到该 session 最大 seq)。
	// 按 (session_key, seq) 升序返回。供记忆提取构建 prompt。
	LoadTurnMessages(ctx context.Context, userID, agentID string, refs []TurnRef) ([]SessionMessage, error)
```

`database.go` 实现:

```go
// ResetExtraction 见接口文档。
func (d *DBStore) ResetExtraction(ctx context.Context, extractionID string) error {
	if extractionID == "" {
		return nil
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE session_messages SET extraction_id=NULL WHERE extraction_id=%s`, d.ph(1)),
		extractionID)
	return err
}

// LoadTurnMessages 见接口文档。
func (d *DBStore) LoadTurnMessages(ctx context.Context, userID, agentID string, refs []TurnRef) ([]SessionMessage, error) {
	var out []SessionMessage
	for _, r := range refs {
		// 同 session 内本 turn 之后的下一个锚点 seq(running 或 done 都算锚点边界)。
		var nextSeq int64
		err := d.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT COALESCE(MIN(seq), -1) FROM session_messages
				WHERE user_id=%s AND agent_id=%s AND session_key=%s AND seq > %s
				  AND turn_status <> ''`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
			userID, agentID, r.SessionKey, r.StartSeq).Scan(&nextSeq)
		if err != nil {
			return nil, err
		}
		var rows *sql.Rows
		base := `SELECT role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at
			FROM session_messages
			WHERE user_id=%s AND agent_id=%s AND session_key=%s AND seq >= %s`
		if nextSeq < 0 {
			rows, err = d.db.QueryContext(ctx,
				fmt.Sprintf(base+` ORDER BY seq ASC`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
				userID, agentID, r.SessionKey, r.StartSeq)
		} else {
			rows, err = d.db.QueryContext(ctx,
				fmt.Sprintf(base+` AND seq < %s ORDER BY seq ASC`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
				userID, agentID, r.SessionKey, r.StartSeq, nextSeq)
		}
		if err != nil {
			return nil, err
		}
		if err := scanSessionMessages(rows, &out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanSessionMessages 把 session_messages 查询结果追加到 out。
// 列顺序与 ListSessionMessages 一致。
func scanSessionMessages(rows *sql.Rows, out *[]SessionMessage) error {
	defer rows.Close()
	for rows.Next() {
		var m SessionMessage
		var contentParts, toolCalls, metadata, rawAssistant string
		if err := rows.Scan(&m.Role, &m.Content, &contentParts, &toolCalls,
			&m.ToolCallID, &m.Name, &metadata, &m.Thinking, &rawAssistant, &m.Origin, &m.Timestamp); err != nil {
			return err
		}
		if contentParts != "" {
			_ = json.Unmarshal([]byte(contentParts), &m.ContentParts)
		}
		if toolCalls != "" {
			_ = json.Unmarshal([]byte(toolCalls), &m.ToolCalls)
		}
		if metadata != "" {
			_ = json.Unmarshal([]byte(metadata), &m.Metadata)
		}
		if rawAssistant != "" {
			m.RawAssistant = json.RawMessage(rawAssistant)
		}
		*out = append(*out, m)
	}
	return rows.Err()
}
```

> 复核:`ListSessionMessages`(database.go:2313)的 SELECT 列顺序为 `role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at` —— `scanSessionMessages` 必须与之逐列对齐。若实现时该函数已有等价私有 scanner,直接复用,勿重复造。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/store/ -run 'TestResetExtractionReturnsToPending|TestLoadTurnMessagesRange' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/store/store.go internal/store/database.go internal/store/claim_test.go
git commit -F - <<'EOF'
feat(store): ResetExtraction 补偿重置 + LoadTurnMessages 按 turn 区间回放

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 7: Session 层锚点写入(`AppendTurnAnchor`)

**Files:**
- Modify: `internal/session/store_adapter.go`(适配方法)
- Modify: `internal/session/manager.go`(`SessionStore` 接口 + `Session.AppendTurnAnchor`)
- Test: `internal/session/turn_anchor_test.go`(新建,若 session 包已有测试夹具则复用)

- [ ] **Step 1: 确认 SessionStore 接口形状**

Run: `grep -n "AppendMessage\|type SessionStore\|SaveSession" internal/session/manager.go`
读出 `SessionStore` 接口当前方法,确认 `AppendMessage(ctx, agentID, sessionKey, msg) error` 在其中(manager.go:106 一带)。

- [ ] **Step 2: 写失败测试**

新建 `internal/session/turn_anchor_test.go`(用与现有 session 测试相同的内存 store 夹具;若不存在,用一个最小 fake 实现 `SessionStore`,只需让 `AppendTurnAnchor` 返回递增 seq):

```go
package session

import (
	"context"
	"testing"

	"<module>/internal/provider"
)

func TestSessionAppendTurnAnchorReturnsSeq(t *testing.T) {
	st := newFakeSessionStore() // 见下方说明
	s := newTestSession(st)      // 见下方说明
	seq, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor: %v", err)
	}
	if seq != 0 {
		t.Fatalf("want seq 0 got %d", seq)
	}
}
```

> `<module>` 用 `go.mod` 第一行的模块路径替换。`newFakeSessionStore` / `newTestSession`:若 `internal/session` 已有测试辅助(grep `func newTest` / `_test.go`),复用;否则在本测试文件内写一个最小 fake——实现 `SessionStore` 全部方法,`AppendTurnAnchor` 用一个 map 计数返回递增 seq,其余方法空实现返回 nil。

- [ ] **Step 3: 跑测试确认失败**

Run: `go test ./internal/session/ -run TestSessionAppendTurnAnchorReturnsSeq -v`
Expected: FAIL(`s.AppendTurnAnchor` / 接口方法未定义)。

- [ ] **Step 4: 扩展 SessionStore 接口**

在 `manager.go` 的 `SessionStore` 接口里(`AppendMessage` 旁)加:

```go
	AppendTurnAnchor(ctx context.Context, agentID, sessionKey string, msg provider.Message) (int64, error)
```

- [ ] **Step 5: StoreAdapter 适配**

`store_adapter.go` 在 `AppendMessage` 之后加:

```go
// AppendTurnAnchor 把 turn 起点用户消息写入归档(turn_status='running')并返回 seq。
func (a *StoreAdapter) AppendTurnAnchor(ctx context.Context, agentID, sessionKey string, m provider.Message) (int64, error) {
	return a.st.AppendTurnAnchor(ctx, a.userID, agentID, sessionKey, sessionMessageFromProvider(m))
}
```

- [ ] **Step 6: Session.AppendTurnAnchor**

`manager.go` 在 `Append` 之后加(与 `Append` 同样维护内存工作集 + SaveSession blob,但归档走锚点路径并返回 seq):

```go
// AppendTurnAnchor 与 Append 等价地把消息加入内存工作集并 SaveSession,
// 但归档行带 turn_status='running' 并返回分配的 seq——供 turn 起点登记锚点、
// turn 结束时按 (sessionKey, seq) 翻 done。仅用于真正开启一个 turn 的用户消息。
// 无持久化 store 时退化为 Append 语义并返回 (-1, nil)。
func (s *Session) AppendTurnAnchor(msg provider.Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if msg.Timestamp == 0 {
		msg.Timestamp = time.Now().UnixMilli()
	}
	s.Messages = append(s.Messages, msg)
	if s.store == nil {
		s.appendToFile(msg)
		return -1, nil
	}
	s.store.SaveSession(s.ctx(), s.agentID, s.sessionKey, s.channel, s.accountID, s.chatID, s.projectID, s.Messages)
	return s.store.AppendTurnAnchor(s.ctx(), s.agentID, s.sessionKey, msg)
}
```

- [ ] **Step 7: 跑测试确认通过 + 全包构建**

Run: `go test ./internal/session/ -run TestSessionAppendTurnAnchorReturnsSeq -v && go build ./...`
Expected: PASS 且构建通过(此时 `store.Store` 接口已被所有实现满足)。

- [ ] **Step 8: 提交**

```bash
git add internal/session/manager.go internal/session/store_adapter.go internal/session/turn_anchor_test.go
git commit -F - <<'EOF'
feat(session): Session.AppendTurnAnchor 写 running 锚点并返回 seq

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 8: loop.go —— 两处 turn 起点写锚点 + 把锚点透传给 runPostTurn

**Files:**
- Modify: `internal/agent/loop.go`

**说明:** 仅 `HandleMessage`(`sess.Append(userMsg)` 约 1847)与 `HandleMessageStream`(约 2551)是有效锚点点位——它们驱动 `runPostTurn`。计划模式路径(约 1618)**不**写锚点(它不调用 runPostTurn,写了会留下永久 running 僵尸)。本任务只改管道、不动触发逻辑(触发在 Task 9)。

- [ ] **Step 1: 定义 turnAnchor 类型**

在 `loop.go` 靠近 `runPostTurn` 定义处(或 Agent 相关类型区)加:

```go
// turnAnchor 标识一个 turn 起点写入的锚点行。turn 结束时(runPostTurn)
// 用它把锚点翻成 done。并发/重叠 turn 各自持有独立的 turnAnchor,
// 因此显式透传而非挂在 Session 上。nil = 该次调用没有锚点(如计划模式)。
type turnAnchor struct {
	sessionKey string
	seq        int64
}
```

- [ ] **Step 2: HandleMessage 起点改为写锚点**

把 `HandleMessage` 中(约 1847)的:

```go
	userMsg := buildUserMessage(msg)
	sess.Append(userMsg)
```

改为:

```go
	userMsg := buildUserMessage(msg)
	var anchor *turnAnchor
	if a.dataStore != nil {
		if seq, err := sess.AppendTurnAnchor(userMsg); err != nil {
			slog.Warn("turn anchor append failed", "agent", a.name, "error", err)
			sess.Append(userMsg) // 兜底:退回普通追加,本 turn 不计入提取
		} else if seq >= 0 {
			anchor = &turnAnchor{sessionKey: sess.SessionKey(), seq: seq}
		}
	} else {
		sess.Append(userMsg)
	}
```

> 注:`AppendTurnAnchor` 已经把消息加入工作集 + SaveSession,所以成功分支**不再**调用 `sess.Append`。仅在出错或无 store 时回退 `sess.Append`,确保消息一定进工作集。

- [ ] **Step 3: HandleMessageStream 起点同样改造**

把 `HandleMessageStream` 中(约 2551)的 `userMsg := buildUserMessage(msg)` / `sess.Append(userMsg)` 用与 Step 2 完全相同的代码块替换(同样产出局部变量 `anchor`)。

- [ ] **Step 4: runPostTurn 增加 anchor 参数**

把签名:

```go
func (a *Agent) runPostTurn(ctx context.Context, msg bus.InboundMessage, messages []provider.Message, toolCallCount int, chatterMem *Memory) {
```

改为:

```go
func (a *Agent) runPostTurn(ctx context.Context, msg bus.InboundMessage, messages []provider.Message, toolCallCount int, chatterMem *Memory, anchor *turnAnchor) {
```

- [ ] **Step 5: 所有 runPostTurn 调用点传入 anchor**

`HandleMessage` 内的两个调用(约 1996、2273)在末尾加 `, anchor`。
`HandleMessageStream` 内的四个调用(约 2613、2686、2780、2854)在末尾加 `, anchor`。其中 2686 处于捕获式 goroutine —— `anchor` 是局部 `*turnAnchor`,与现有 `capturedToolCalls` / `capturedChatterMem` 一样可被闭包捕获;若该处显式 `captured := ...`,照样加一个 `capturedAnchor := anchor` 并传它。

逐个核对:`grep -n "a.runPostTurn(" internal/agent/loop.go` 必须全部带 6 个实参(原 5 个 + anchor)。计划模式路径若也调用了某个共用收尾,确认它传 `nil`。

- [ ] **Step 6: 构建**

Run: `go build ./...`
Expected: 通过(runPostTurn 暂未使用 anchor,Go 允许未使用的函数参数)。

- [ ] **Step 7: 提交**

```bash
git add internal/agent/loop.go
git commit -F - <<'EOF'
feat(agent): turn 起点写 running 锚点并透传 turnAnchor 给 runPostTurn

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 9: 触发逻辑改造(finish → claim → 归档回放提取)+ 删旧计数门

**Files:**
- Modify: `internal/agent/memory.go`(`AutoPersistMemory` 改输入语义 + 返回 error)
- Modify: `internal/agent/loop.go`(`runPostTurn` 触发块)

- [ ] **Step 1: 改 AutoPersistMemory —— 输入即批次、去掉 last-20、返回 error**

`memory.go`:把 `AutoPersistMemory` 的"仅取最后 20 条"逻辑删掉(批次已被 `cap` 上限约束),并改为返回 `error`(供失败补偿重置)。具体:

签名改为:

```go
func AutoPersistMemory(ctx context.Context, mem *Memory, prov provider.Provider, model string, messages []provider.Message) error {
```

把:

```go
	start := 0
	if len(messages) > 20 {
		start = len(messages) - 20
	}
	for _, m := range messages[start:] {
```

改为(遍历整批,跳过 system 与合成行):

```go
	for _, m := range messages {
		if m.Origin != "" {
			continue // 跳过 goal_context 等合成注入行
		}
```

把两处 `return`(LLM 失败、解析失败)改为 `return err`(解析失败用 `return fmt.Errorf("parse: %w", err)`)。函数末尾(成功路径,现有保存 USER.md 之后)加 `return nil`。

> 复核:`m.Origin != ""` 的判断需要 `provider.Message` 有 `Origin` 字段——已确认有(store_adapter 转换里用到)。`provider.OriginUser` 若是空串常量则等价;用字符串比较 `!= ""` 最稳。

- [ ] **Step 2: 改 runPostTurn 触发块**

`loop.go` 把 `runPostTurn` 中"每 N 个用户轮"那段(从 `var chatterUID string` 到 `go AutoPersistMemory(...)` 结束,约 2453–2482)整体替换为:

```go
	// turn 结束:把锚点翻成 done,再按"已完成且未提取 >= N"的节拍认领并异步提取。
	var chatterUID string
	if chatterMem != nil {
		chatterUID = chatterMem.UserID()
	}
	if a.dataStore == nil || anchor == nil {
		return // 无持久化 或 本次无锚点(计划模式/兜底追加):不触发
	}
	if err := a.dataStore.FinishTurn(ctx, a.ownerUserID, a.name, anchor.sessionKey, anchor.seq); err != nil {
		slog.Warn("finish turn failed", "agent", a.name, "error", err)
		// 锚点没翻成 done 只是本 turn 不计入,不阻塞主流程。
	}
	if !a.memoryCfg.AutoPersist.Enabled || a.memoryCfg.AutoPersist.EveryNTurns <= 0 || chatterUID == "" {
		return
	}
	n := a.memoryCfg.AutoPersist.EveryNTurns
	extractionID, refs, err := a.dataStore.ClaimCadenceBatch(ctx, a.name, chatterUID, n, 3*n)
	if err != nil {
		slog.Warn("auto-persist: claim failed", "agent", a.name, "chatter", chatterUID, "error", err)
		return
	}
	if extractionID == "" {
		return // 不足 N,不触发
	}
	model := a.memoryCfg.AutoPersist.Model
	if model == "" {
		model = a.model
	}
	slog.Info("auto-persist firing", "agent", a.name, "chatter", chatterUID, "model", model, "turns", len(refs), "extraction_id", extractionID)
	go func() {
		archived, err := a.dataStore.LoadTurnMessages(ctx, a.ownerUserID, a.name, refs)
		if err != nil {
			slog.Warn("auto-persist: load turn messages failed", "agent", a.name, "extraction_id", extractionID, "error", err)
			_ = a.dataStore.ResetExtraction(ctx, extractionID)
			return
		}
		msgs := make([]provider.Message, 0, len(archived))
		for _, m := range archived {
			msgs = append(msgs, provider.Message{Role: m.Role, Content: m.Content, Origin: m.Origin})
		}
		if err := AutoPersistMemory(ctx, chatterMem, a.provider, model, msgs); err != nil {
			slog.Warn("auto-persist: extraction failed, resetting batch", "agent", a.name, "extraction_id", extractionID, "error", err)
			_ = a.dataStore.ResetExtraction(ctx, extractionID)
		}
	}()
```

> 注意保留替换块**之前**的 `a.turnCount++`、FTS 索引、`a.hooks.Run(PostTurn)` 等代码不动;也保留替换块**之后**的 skillsLearner 等代码不动。只替换"自动保留内存"那一段。`provider` 包已在 loop.go import。

- [ ] **Step 3: 构建 + 跑 agent/memory 包测试**

Run: `go build ./... && go test ./internal/agent/ -v`
Expected: 构建通过;agent 现有测试通过(若有用到 `AutoPersistMemory` 旧签名的测试,改为接收 error 返回值)。

- [ ] **Step 4: 提交**

```bash
git add internal/agent/loop.go internal/agent/memory.go
git commit -F - <<'EOF'
feat(agent): runPostTurn 改为 finish→claim→归档回放提取,失败补偿重置

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 10: 删除 `CountChatterUserMessages` 死代码

**Files:**
- Modify: `internal/store/store.go`(接口删方法)
- Modify: `internal/store/database.go`(实现删除)
- 检查其它引用

- [ ] **Step 1: 查全部引用**

Run: `grep -rn "CountChatterUserMessages" internal/`
预期:仅剩接口定义(store.go)、实现(database.go)、可能的 mock/test。Task 9 已移除 loop.go 的调用。

- [ ] **Step 2: 删除**

- 删 `store.go` 接口里的 `CountChatterUserMessages(...)` 方法声明及其上方注释块。
- 删 `database.go` 的 `func (d *DBStore) CountChatterUserMessages(...)` 整个实现及注释。
- 若有 mock store(`grep -rln "CountChatterUserMessages" internal/`)实现该方法,一并删除其实现。

- [ ] **Step 3: 构建 + 全量测试**

Run: `go build ./... && go test ./...`
Expected: 全部通过,无"未实现接口"/"未定义"错误。

- [ ] **Step 4: 提交**

```bash
git add -A
git commit -F - <<'EOF'
refactor(store): 删除被取代的 CountChatterUserMessages 计数门

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>
EOF
```

---

## Task 11: 端到端验证 + MySQL 集成测试 + 收尾

**Files:**
- 可能 Modify: `internal/store/mysql_integration_test.go`(若要加 turn 列断言)

- [ ] **Step 1: 全量构建 + 测试 + race**

Run: `go build ./... && go test ./... && go test ./internal/store/ -race -run Claim`
Expected: 全绿。

- [ ] **Step 2: MySQL 集成测试(若环境可用)**

查看现有 MySQL 集成测试如何起库:

Run: `sed -n '1,80p' internal/store/mysql_integration_test.go`

按其约定(通常读 `BKCRAB_TEST_MYSQL_DSN` 之类的环境变量并 skip-if-unset)补一个用例,断言 MySQL 下:`AppendTurnAnchor` 返回递增 seq、`ClaimCadenceBatch` 够 5 认领、二次为空、`idx_sm_pending` 已建(`SHOW INDEX FROM session_messages` 含 `idx_sm_pending`)。

Run(有 DSN 时): `BKCRAB_TEST_MYSQL_DSN='<dsn>' go test ./internal/store/ -run MySQL -v`
Expected: PASS;无 DSN 时 SKIP(不阻塞)。

- [ ] **Step 3: 手动冒烟(可选但推荐)**

按项目 `run` 方式起服务(SQLite 单机模式最快:`BKCRAB_STORAGE=sqlite` 或等价配置),在同一 agent+chatter 下连发消息,观察日志出现 `auto-persist firing ... turns=5 extraction_id=...`,且不再出现旧的 `auto-persist gate ... chatter_turns=...`。确认 `MEMORY.md` 被追加。

- [ ] **Step 4: 对照 spec 自查**

逐条核对 spec §2 目标 1–6 均被实现;§9 白名单(steer/注入/goal_context 默认 `''`)在代码上成立(只有 Task 8 两处写 `running`)。

- [ ] **Step 5: 合并收尾**

调用 superpowers:finishing-a-development-branch,在 merge / PR / 清理之间做选择。

---

## Self-Review(写计划后自查记录)

- **Spec 覆盖**:§3 两列→Task 1/2;三后端建表→Task 1;迁移+索引降级→Task 2;§4.1 锚点+seq→Task 3/7/8;§4.2 done→Task 4/9;§5 认领→Task 5/9;§6 归档回放→Task 6/9;§7 失败重置→Task 6/9;§8 压缩预留→**本计划不实现**(spec 明确非目标,列设计已支撑,无需任务);§9 白名单→Task 8(只两处写 running);§10 删 Count→Task 10。无遗漏。
- **占位符**:无 TBD;所有 code step 给出完整代码。Task 2 故意保留了"冗余版+精简版",已注明采用精简版——实现者照精简版写。
- **类型一致性**:`TurnRef{SessionKey, StartSeq}`、`turnAnchor{sessionKey, seq}`、`AppendTurnAnchor(...)(int64,error)`、`ClaimCadenceBatch(...)(string,[]TurnRef,error)`、`FinishTurn(...,seq int64)`、`ResetExtraction(id)`、`LoadTurnMessages(...)([]SessionMessage,error)`、`AutoPersistMemory(...) error` 在各任务间一致。
- **已知实现注记**:`<module>` 占位需用 go.mod 模块路径替换(Task 7);uuid 包名以仓库实际为准(Task 5 Step 5 已要求先 grep);SQLite 高并发可选 `_txlock=immediate`(Task 5)。
