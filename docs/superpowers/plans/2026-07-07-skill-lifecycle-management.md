# Learner 技能生命周期管理 Implementation Plan

> **已取代：** 本文记录历史实施过程，当前行为与不变量以 [Skill 自动提炼（自进化）Canonical 设计与实现](../../skill-self-evolution.md) 为准。

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 给 review 自动生成的 learner 技能加生命周期——用指数衰减活跃度把进入系统提示 catalog 的 learner 技能稳定在小规模,基于真实加载信号做 active/cooling/deleted 三态派生淘汰。

**Architecture:** 新增 `skill_usage` 表(每行 = 一个 agent 的一个 learner 技能账本);打分是纯函数 `skills.Rank`,以"`load_skill` 加载序号"为时钟做指数衰减;三态全部派生不落状态列;explicit 信号由 `load_skill(invoked_by_user)` 交模型自陈述。依赖方向 `agent → skills → store`。

**Tech Stack:** Go;database/sql(SQLite 开发/测试 + MySQL 生产,双方言);现有 store 事务/迁移模式;标准库 `crypto/sha256`、`math`。

**Spec:** `docs/superpowers/specs/2026-07-07-skill-lifecycle-management-design.md`

---

## File Structure

- `internal/store/skill_usage.go`(新):`SkillUsageRow` 类型、`DecayFactor`、`HashSkillContent`、`UpsertSkillUsage`/`RecordSkillLoad`/`ListSkillUsage`/`DeleteSkillUsage` 实现。
- `internal/store/skill_usage_test.go`(新):上述的表驱动测试(`newTestSQLite`)。
- `internal/store/database.go`(改):`migrationSQL()` 追加 `skill_usage` 建表(SQLite)。
- `internal/store/database_mysql.go`(改):`mysqlMigrationSQL()` 追加 `skill_usage` 建表(MySQL)。
- `internal/store/store.go`(改):`Store` 接口加 4 个方法声明。
- `internal/skills/lifecycle.go`(新):`LifecycleConfig`、`Rank`、`NowSeq` 纯函数。
- `internal/skills/lifecycle_test.go`(新):纯函数表驱动测试。
- `internal/config/config.go`(改):`SkillLifecycleCfg` + 挂到 `SkillsLearnerCfg`。
- `internal/config/skills_learner_cfg_test.go`(改):加 `SkillLifecycleCfg.IsEnabled()` 三态。
- `internal/agent/tools/load_skill.go`(改):`invoked_by_user` 参数 + 记账接线。
- `internal/agent/tools/load_skill_test.go`(改):记账/gain/手改测试。
- `internal/agent/skills_learner.go`(改):`persistExtracted` upsert 账本 + 提取 prompt 注入现有 slug 清单。
- `internal/agent/skills.go`(改):`BuildSkillsSummary` 前置 `Rank` 过滤 + directive 补 `invoked_by_user`。
- `internal/agent/loop.go`(改):post-turn 顺风车删除清理。

**依赖核对(实现前先做):** 运行 `grep -rn "internal/skills" internal/store/` 确认 store **不** import skills(否则 skills→store 成环)。预期无匹配。

---

## Task 1: store — skill_usage 表 + 类型 + 无状态 helper

**Files:**
- Create: `internal/store/skill_usage.go`
- Create: `internal/store/skill_usage_test.go`
- Modify: `internal/store/database.go`(`migrationSQL()` 返回的 slice 末尾追加)
- Modify: `internal/store/database_mysql.go`(`mysqlMigrationSQL()` 返回的 slice 末尾追加)

- [ ] **Step 1: Write the failing test**(helper 纯函数,先不碰 DB)

`internal/store/skill_usage_test.go`:

```go
package store

import (
	"math"
	"testing"
)

func TestDecayFactor(t *testing.T) {
	// 半衰期 32:经过 32 次机会后因子 = 0.5。
	got := DecayFactor(32, 32)
	if math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("DecayFactor(32,32)=%v want 0.5", got)
	}
	// dt=0 → 1(刚加载,无衰减)。
	if got := DecayFactor(0, 32); math.Abs(got-1) > 1e-9 {
		t.Fatalf("DecayFactor(0,32)=%v want 1", got)
	}
	// H<=0 视为默认 32,不 panic、不 NaN。
	if got := DecayFactor(32, 0); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("DecayFactor(32,0)=%v want 0.5 (default H)", got)
	}
}

func TestHashSkillContentNormalizesCRLF(t *testing.T) {
	// 与 manager 写盘的 \r\n→\n 归一化一致,使 learner 建行 hash 与 load 读盘 hash 可比。
	a := HashSkillContent("---\nname: x\n---\nbody\n")
	b := HashSkillContent("---\r\nname: x\r\n---\r\nbody\r\n")
	if a != b {
		t.Fatalf("CRLF 归一化后 hash 应相等: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex 应 64 字符,得 %d", len(a))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestDecayFactor|TestHashSkillContent' -v`
Expected: FAIL(`undefined: DecayFactor` / `undefined: HashSkillContent`)

- [ ] **Step 3: Write minimal implementation**

`internal/store/skill_usage.go`:

```go
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// SkillUsageRow 是一个 (agent, learner 技能) 的使用账本行。导出供 skills.Rank 消费。
type SkillUsageRow struct {
	Slug         string
	Origin       string  // 目前恒为 "learner"
	Activity     float64 // 惰性衰减活跃度,写在加载时
	LastLoadSeq  int64
	TotalLoads   int64
	ExplicitUses int64
	CreatedSeq   int64
	EditedSeq    int64 // 0=从未检测到用户手改
	ContentHash  string
}

// DecayFactor 是活跃度衰减的唯一实现:0.5^(dt/H)。写路径(RecordSkillLoad)与
// 读路径(skills.Rank)共用,杜绝公式漂移。H<=0 视为默认 32(防 0 除)。
func DecayFactor(dt int64, halfLifeLoads int) float64 {
	if halfLifeLoads <= 0 {
		halfLifeLoads = 32
	}
	if dt <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(dt)/float64(halfLifeLoads))
}

// HashSkillContent 归一化 \r\n→\n(与 skills.Manager 写盘一致)后取 sha256 hex。
func HashSkillContent(content string) string {
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// —— 以下方法在后续 Task 实现,此处先占位声明以便接口编译 ——
// (Task 2/3 填充函数体)

var _ = context.Background
var _ = fmt.Sprintf
```

在 `internal/store/database.go` 的 `migrationSQL()`(约 database.go:1266,返回 `[]string`)末尾、`return` 的 slice 里追加一条(与 session_messages 同风格,SQLite 类型):

```go
		`CREATE TABLE IF NOT EXISTS skill_usage (
			agent_id TEXT NOT NULL,
			slug TEXT NOT NULL,
			origin TEXT NOT NULL DEFAULT 'learner',
			activity REAL NOT NULL DEFAULT 0,
			last_load_seq INTEGER NOT NULL DEFAULT 0,
			total_loads INTEGER NOT NULL DEFAULT 0,
			explicit_uses INTEGER NOT NULL DEFAULT 0,
			created_seq INTEGER NOT NULL DEFAULT 0,
			edited_seq INTEGER NOT NULL DEFAULT 0,
			content_hash TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, slug)
		)`,
```

在 `internal/store/database_mysql.go` 的 `mysqlMigrationSQL()`(database_mysql.go:79)末尾追加对应 MySQL 版:

```go
		`CREATE TABLE IF NOT EXISTS skill_usage (
			agent_id VARCHAR(120) NOT NULL,
			slug VARCHAR(64) NOT NULL,
			origin VARCHAR(16) NOT NULL DEFAULT 'learner',
			activity DOUBLE NOT NULL DEFAULT 0,
			last_load_seq BIGINT NOT NULL DEFAULT 0,
			total_loads BIGINT NOT NULL DEFAULT 0,
			explicit_uses BIGINT NOT NULL DEFAULT 0,
			created_seq BIGINT NOT NULL DEFAULT 0,
			edited_seq BIGINT NOT NULL DEFAULT 0,
			content_hash CHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (agent_id, slug)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestDecayFactor|TestHashSkillContent' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/skill_usage.go internal/store/skill_usage_test.go internal/store/database.go internal/store/database_mysql.go
git commit -m "feat(store): add skill_usage table + DecayFactor/HashSkillContent helpers"
```

---

## Task 2: store — UpsertSkillUsage / ListSkillUsage / DeleteSkillUsage

**Files:**
- Modify: `internal/store/skill_usage.go`
- Modify: `internal/store/store.go`(接口加声明)
- Modify: `internal/store/skill_usage_test.go`

- [ ] **Step 1: Write the failing test**

追加到 `internal/store/skill_usage_test.go`:

```go
import "context" // 若未导入则加

func TestUpsertAndListSkillUsage(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()

	// 首建:写 created_seq(此刻时钟=0,因无任何加载)+ content_hash。
	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "hash1", true); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	rows, err := db.ListSkillUsage(ctx, "agentA")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Slug != "foo" || rows[0].ContentHash != "hash1" || rows[0].Origin != "learner" {
		t.Fatalf("unexpected row: %+v", rows)
	}

	// 再 upsert 同 slug firstCreate=false:只刷 hash,不动 created_seq/计数。
	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "hash2", false); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	rows, _ = db.ListSkillUsage(ctx, "agentA")
	if len(rows) != 1 || rows[0].ContentHash != "hash2" {
		t.Fatalf("hash not refreshed: %+v", rows)
	}

	// 隔离:另一 agent 不可见。
	if r, _ := db.ListSkillUsage(ctx, "agentB"); len(r) != 0 {
		t.Fatalf("agent isolation broken: %+v", r)
	}
}

func TestDeleteSkillUsage(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	_ = db.UpsertSkillUsage(ctx, "agentA", "foo", "h", true)
	if err := db.DeleteSkillUsage(ctx, "agentA", "foo"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if r, _ := db.ListSkillUsage(ctx, "agentA"); len(r) != 0 {
		t.Fatalf("row not deleted: %+v", r)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestUpsertAndListSkillUsage|TestDeleteSkillUsage' -v`
Expected: FAIL(`db.UpsertSkillUsage undefined`)

- [ ] **Step 3: Write minimal implementation**

替换 `internal/store/skill_usage.go` 底部的占位块为:

```go
// currentSkillSeq 返回该 agent 当前时钟值 = MAX(last_load_seq)(无行则 0)。
// tx 可为 nil(用 d.db)。
func (d *DBStore) currentSkillSeq(ctx context.Context, q queryer, agentID string) (int64, error) {
	var seq sql.NullInt64
	err := q.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT MAX(last_load_seq) FROM skill_usage WHERE agent_id=%s`, d.ph(1)),
		agentID).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

// UpsertSkillUsage 建/更 learner 技能账本行。firstCreate=true:插入行,created_seq=
// 当前时钟,content_hash=contentHash,计数归零;false:只刷 content_hash + updated_at。
func (d *DBStore) UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error {
	if agentID == "" || slug == "" {
		return nil
	}
	if !firstCreate {
		_, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`UPDATE skill_usage SET content_hash=%s, updated_at=CURRENT_TIMESTAMP
				WHERE agent_id=%s AND slug=%s`, d.ph(1), d.ph(2), d.ph(3)),
			contentHash, agentID, slug)
		return err
	}
	createdSeq, err := d.currentSkillSeq(ctx, d.db, agentID)
	if err != nil {
		return err
	}
	// INSERT OR IGNORE 语义:已存在则不覆盖 created_seq(重复首建保护)。
	verb := "INSERT OR IGNORE INTO"
	if d.dialect == mysqlDialect {
		verb = "INSERT IGNORE INTO"
	}
	_, err = d.db.ExecContext(ctx,
		fmt.Sprintf(`%s skill_usage (agent_id, slug, origin, created_seq, content_hash)
			VALUES (%s, %s, 'learner', %s, %s)`,
			verb, d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		agentID, slug, createdSeq, contentHash)
	if err != nil {
		return err
	}
	// 若行已存在(首建对已存在 slug),仍刷新 hash(learner 重新提取到同 slug)。
	_, err = d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE skill_usage SET content_hash=%s, updated_at=CURRENT_TIMESTAMP
			WHERE agent_id=%s AND slug=%s`, d.ph(1), d.ph(2), d.ph(3)),
		contentHash, agentID, slug)
	return err
}

// ListSkillUsage 返回该 agent 全部 learner 账本行。
func (d *DBStore) ListSkillUsage(ctx context.Context, agentID string) ([]SkillUsageRow, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT slug, origin, activity, last_load_seq, total_loads,
			explicit_uses, created_seq, edited_seq, content_hash
			FROM skill_usage WHERE agent_id=%s`, d.ph(1)), agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillUsageRow
	for rows.Next() {
		var r SkillUsageRow
		if err := rows.Scan(&r.Slug, &r.Origin, &r.Activity, &r.LastLoadSeq,
			&r.TotalLoads, &r.ExplicitUses, &r.CreatedSeq, &r.EditedSeq, &r.ContentHash); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteSkillUsage 删账本行(与 Manager.Delete 目录配对)。
func (d *DBStore) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM skill_usage WHERE agent_id=%s AND slug=%s`, d.ph(1), d.ph(2)),
		agentID, slug)
	return err
}

// queryer 抽象 *sql.DB / *sql.Tx 的 QueryRowContext,供 currentSkillSeq 在事务内外复用。
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}
```

把 `skill_usage.go` 顶部 import 补齐(删掉 Task 1 的占位 `var _`):

```go
import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)
```

在 `internal/store/store.go` 的 `Store` 接口内(靠近 `ClaimSkillBatch` 声明处)加:

```go
	// —— learner 技能生命周期账本(skill_usage 表)——
	UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error
	ListSkillUsage(ctx context.Context, agentID string) ([]SkillUsageRow, error)
	DeleteSkillUsage(ctx context.Context, agentID, slug string) error
	// RecordSkillLoad 在 Task 3 声明。
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestUpsertAndListSkillUsage|TestDeleteSkillUsage' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/skill_usage.go internal/store/store.go internal/store/skill_usage_test.go
git commit -m "feat(store): skill_usage upsert/list/delete + Store interface"
```

---

## Task 3: store — RecordSkillLoad(加载记账事务)

**Files:**
- Modify: `internal/store/skill_usage.go`
- Modify: `internal/store/store.go`
- Modify: `internal/store/skill_usage_test.go`

- [ ] **Step 1: Write the failing test**

追加到 `internal/store/skill_usage_test.go`:

```go
import "math" // 若未导入则加

func TestRecordSkillLoadBasic(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	_ = db.UpsertSkillUsage(ctx, "agentA", "foo", "h", true)

	// 模型自选:gain=1。seq 从 0→1。
	row, err := db.RecordSkillLoad(ctx, "agentA", "foo", "h", false, 32, 3)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if row == nil || row.TotalLoads != 1 || row.LastLoadSeq != 1 {
		t.Fatalf("unexpected: %+v", row)
	}
	if math.Abs(row.Activity-1) > 1e-9 { // activity = 0*decay + 1
		t.Fatalf("activity=%v want 1", row.Activity)
	}
	if row.ExplicitUses != 0 {
		t.Fatalf("explicit_uses should stay 0 for model-initiated")
	}

	// 用户点名:gain=3,explicit_uses+1,seq→2。
	row, _ = db.RecordSkillLoad(ctx, "agentA", "foo", "h", true, 32, 3)
	if row.TotalLoads != 2 || row.ExplicitUses != 1 || row.LastLoadSeq != 2 {
		t.Fatalf("explicit path wrong: %+v", row)
	}
	// activity = 1*0.5^(1/32) + 3 ≈ 3.978
	if row.Activity < 3.9 || row.Activity > 4.0 {
		t.Fatalf("activity=%v out of expected band", row.Activity)
	}
}

func TestRecordSkillLoadDetectsEdit(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	_ = db.UpsertSkillUsage(ctx, "agentA", "foo", "origHash", true)
	// 盘上 hash 与账本不符 → 置 edited_seq。
	row, err := db.RecordSkillLoad(ctx, "agentA", "foo", "DIFFERENThash", false, 32, 3)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if row.EditedSeq == 0 {
		t.Fatalf("edited_seq should be set on hash mismatch, got %+v", row)
	}
}

func TestRecordSkillLoadNoRowSkips(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	// 无账本行(手工技能)→ 返回 (nil, nil),不建行。
	row, err := db.RecordSkillLoad(ctx, "agentA", "manual", "h", true, 32, 3)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if row != nil {
		t.Fatalf("expected nil row for no-ledger skill, got %+v", row)
	}
	if r, _ := db.ListSkillUsage(ctx, "agentA"); len(r) != 0 {
		t.Fatalf("should not create a row")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run 'TestRecordSkillLoad' -v`
Expected: FAIL(`db.RecordSkillLoad undefined`)

- [ ] **Step 3: Write minimal implementation**

追加到 `internal/store/skill_usage.go`:

```go
// RecordSkillLoad 在单个写事务内记一次加载。无账本行(非 learner/手工技能)→ (nil,nil)。
// seq = MAX(last_load_seq)+1(该 agent);activity = activity·DecayFactor(seq-last,H) + gain;
// gain = explicitGain if invokedByUser else 1;total_loads+1;explicit_uses(+1 if invoked);
// diskHash != content_hash → edited_seq=seq(用户手改检测)。返回更新后的行快照。
func (d *DBStore) RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*SkillUsageRow, error) {
	if agentID == "" || slug == "" {
		return nil, nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	lock := ""
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		lock = " FOR UPDATE"
	}
	var r SkillUsageRow
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT slug, activity, last_load_seq, total_loads, explicit_uses,
			created_seq, edited_seq, content_hash FROM skill_usage
			WHERE agent_id=%s AND slug=%s`+lock, d.ph(1), d.ph(2)), agentID, slug).
		Scan(&r.Slug, &r.Activity, &r.LastLoadSeq, &r.TotalLoads, &r.ExplicitUses,
			&r.CreatedSeq, &r.EditedSeq, &r.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil // 非 learner / 无账本行:跳过,不建行
	}
	if err != nil {
		return nil, err
	}

	// 该 agent 当前时钟(在事务内读,含刚锁定的行)。
	curSeq, err := d.currentSkillSeq(ctx, tx, agentID)
	if err != nil {
		return nil, err
	}
	seq := curSeq + 1

	gain := 1
	if invokedByUser {
		gain = explicitGain
		if gain < 1 {
			gain = 1
		}
	}
	decay := DecayFactor(seq-r.LastLoadSeq, halfLifeLoads)
	r.Activity = r.Activity*decay + float64(gain)
	r.LastLoadSeq = seq
	r.TotalLoads++
	if invokedByUser {
		r.ExplicitUses++
	}
	if diskHash != "" && r.ContentHash != "" && diskHash != r.ContentHash {
		r.EditedSeq = seq
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE skill_usage SET activity=%s, last_load_seq=%s, total_loads=%s,
			explicit_uses=%s, edited_seq=%s, updated_at=CURRENT_TIMESTAMP
			WHERE agent_id=%s AND slug=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		r.Activity, r.LastLoadSeq, r.TotalLoads, r.ExplicitUses, r.EditedSeq, agentID, slug)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}
```

`currentSkillSeq` 的 `queryer` 参数已能接受 `*sql.Tx`(它实现了 `QueryRowContext`)。

在 `internal/store/store.go` 接口里把 Task 2 的占位注释替换为:

```go
	RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*SkillUsageRow, error)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -run 'TestRecordSkillLoad' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/skill_usage.go internal/store/store.go internal/store/skill_usage_test.go
git commit -m "feat(store): RecordSkillLoad accounting txn (decay+gain, edit detection)"
```

---

## Task 4: skills — lifecycle.go 纯排名

**Files:**
- Create: `internal/skills/lifecycle.go`
- Create: `internal/skills/lifecycle_test.go`

**依赖核对:** 本 Task 让 skills import store。先跑 `grep -rn "internal/skills" internal/store/`,确认为空(无环)。

- [ ] **Step 1: Write the failing test**

`internal/skills/lifecycle_test.go`:

```go
package skills

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

func cfg() LifecycleConfig {
	return LifecycleConfig{ActiveMax: 2, HalfLifeLoads: 32, ProtectLoads: 20, EditProtectLoads: 30, DeleteAfterLoads: 200}
}

func TestRankTopKByEffective(t *testing.T) {
	now := int64(100)
	rows := []store.SkillUsageRow{
		{Slug: "hi", Origin: "learner", Activity: 10, LastLoadSeq: 100, TotalLoads: 5, CreatedSeq: 0},
		{Slug: "mid", Origin: "learner", Activity: 5, LastLoadSeq: 100, TotalLoads: 5, CreatedSeq: 0},
		{Slug: "lo", Origin: "learner", Activity: 1, LastLoadSeq: 100, TotalLoads: 5, CreatedSeq: 0},
	}
	active, _ := Rank(rows, now, cfg())
	if !active["hi"] || !active["mid"] || active["lo"] {
		t.Fatalf("want hi,mid active; lo cooling. got %+v", active)
	}
}

func TestRankProtectsNewSkillsIndependentOfK(t *testing.T) {
	now := int64(10)
	// 三个都在创建保护期(now-created<20),ActiveMax=2 也应全 active(独占名额)。
	rows := []store.SkillUsageRow{
		{Slug: "a", Origin: "learner", CreatedSeq: 5, TotalLoads: 0},
		{Slug: "b", Origin: "learner", CreatedSeq: 5, TotalLoads: 0},
		{Slug: "c", Origin: "learner", CreatedSeq: 5, TotalLoads: 0},
	}
	active, _ := Rank(rows, now, cfg())
	if !active["a"] || !active["b"] || !active["c"] {
		t.Fatalf("protected new skills must all be active: %+v", active)
	}
}

func TestRankEditProtectionExpires(t *testing.T) {
	c := cfg()
	// 手改保护期内:active。edited_seq=100, now=110, P=30 → 110-100<30 → 保护。
	rowsIn := []store.SkillUsageRow{{Slug: "e", Origin: "learner", Activity: 0, EditedSeq: 100, LastLoadSeq: 100, CreatedSeq: 0, TotalLoads: 1}}
	active, _ := Rank(rowsIn, 110, c)
	if !active["e"] {
		t.Fatalf("edited skill within P must be active")
	}
	// 到期(now-edited>=P):回归竞争,activity=0 → 落 cooling(ActiveMax 名额被占用场景)。
	// 单行且 ActiveMax=2 时它仍会进前 K;为验证"回归竞争",给两个更高分对手。
	rowsOut := []store.SkillUsageRow{
		{Slug: "e", Origin: "learner", Activity: 0, EditedSeq: 100, LastLoadSeq: 100, CreatedSeq: 0, TotalLoads: 1},
		{Slug: "x", Origin: "learner", Activity: 100, LastLoadSeq: 200, CreatedSeq: 0, TotalLoads: 9},
		{Slug: "y", Origin: "learner", Activity: 100, LastLoadSeq: 200, CreatedSeq: 0, TotalLoads: 9},
	}
	active2, _ := Rank(rowsOut, 200, c) // now-edited=100>=30 → e 不再受保护
	if active2["e"] {
		t.Fatalf("expired edit protection: e should compete and lose, got active")
	}
}

func TestRankDeletable(t *testing.T) {
	c := cfg()
	rows := []store.SkillUsageRow{
		// 从没加载、超 D、未手改 → 可删。
		{Slug: "dead", Origin: "learner", TotalLoads: 0, CreatedSeq: 0, EditedSeq: 0},
		// 用过一次 → 永不删。
		{Slug: "used", Origin: "learner", TotalLoads: 1, CreatedSeq: 0},
		// 手改过 → 不删。
		{Slug: "edited", Origin: "learner", TotalLoads: 0, CreatedSeq: 0, EditedSeq: 5},
	}
	_, deletable := Rank(rows, 300, c) // now-created=300 > D=200
	if len(deletable) != 1 || deletable[0] != "dead" {
		t.Fatalf("only 'dead' deletable, got %+v", deletable)
	}
}

func TestRankNaNInfSinksToBottom(t *testing.T) {
	c := LifecycleConfig{ActiveMax: 1, HalfLifeLoads: 32}
	inf := 1.0
	for i := 0; i < 2000; i++ {
		inf *= 10 // 溢出到 +Inf
	}
	rows := []store.SkillUsageRow{
		{Slug: "bad", Origin: "learner", Activity: inf, LastLoadSeq: 0, TotalLoads: 1, CreatedSeq: 0},
		{Slug: "good", Origin: "learner", Activity: 5, LastLoadSeq: 100, TotalLoads: 1, CreatedSeq: 0},
	}
	active, _ := Rank(rows, 100, c)
	if !active["good"] || active["bad"] {
		t.Fatalf("Inf activity must sink; want good active. got %+v", active)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/skills/ -run TestRank -v`
Expected: FAIL(`undefined: Rank` / `undefined: LifecycleConfig`)

- [ ] **Step 3: Write minimal implementation**

`internal/skills/lifecycle.go`:

```go
package skills

import (
	"math"
	"sort"

	"github.com/qs3c/bkcrab/internal/store"
)

// LifecycleConfig 是排名/删除的可调参数。零值字段由 normalize 兜默认。
type LifecycleConfig struct {
	ActiveMax        int // K
	HalfLifeLoads    int // H
	ProtectLoads     int // G
	EditProtectLoads int // P
	DeleteAfterLoads int // D
}

func (c LifecycleConfig) normalized() LifecycleConfig {
	if c.ActiveMax <= 0 {
		c.ActiveMax = 10
	}
	if c.HalfLifeLoads <= 0 {
		c.HalfLifeLoads = 32
	}
	if c.ProtectLoads <= 0 {
		c.ProtectLoads = 20
	}
	if c.EditProtectLoads <= 0 {
		c.EditProtectLoads = 30
	}
	if c.DeleteAfterLoads <= 0 {
		c.DeleteAfterLoads = 200
	}
	return c
}

// NowSeq 取行集中 MAX(last_load_seq) 作当前时钟(agent 层已 List,免再查库)。
func NowSeq(rows []store.SkillUsageRow) int64 {
	var m int64
	for _, r := range rows {
		if r.LastLoadSeq > m {
			m = r.LastLoadSeq
		}
	}
	return m
}

// effective 把活跃度衰减到当前时点;NaN/Inf 钳制为 0(排到末尾)。
func effective(r store.SkillUsageRow, nowSeq int64, h int) float64 {
	v := r.Activity * store.DecayFactor(nowSeq-r.LastLoadSeq, h)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// Rank 输入账本行 + 当前时钟 + 配置,输出 active 集与可删名单。纯函数,无 IO。
// active = 手改保护 + 创建保护(均独占名额) + 成熟池前 K。
// deletable = learner && edited_seq==0 && total_loads==0 && now-created>D。
func Rank(rows []store.SkillUsageRow, nowSeq int64, cfg LifecycleConfig) (map[string]bool, []string) {
	c := cfg.normalized()
	active := make(map[string]bool)
	var deletable []string
	var mature []store.SkillUsageRow

	for _, r := range rows {
		// 手改保护(有限期)。
		if r.EditedSeq > 0 && nowSeq-r.EditedSeq < int64(c.EditProtectLoads) {
			active[r.Slug] = true
			continue
		}
		// 创建保护。
		if nowSeq-r.CreatedSeq < int64(c.ProtectLoads) {
			active[r.Slug] = true
			continue
		}
		// 删除候选(与竞争互斥:能删的不进竞争池)。
		if r.EditedSeq == 0 && r.TotalLoads == 0 && nowSeq-r.CreatedSeq > int64(c.DeleteAfterLoads) {
			deletable = append(deletable, r.Slug)
			continue
		}
		mature = append(mature, r)
	}

	sort.SliceStable(mature, func(i, j int) bool {
		return effective(mature[i], nowSeq, c.HalfLifeLoads) > effective(mature[j], nowSeq, c.HalfLifeLoads)
	})
	for i, r := range mature {
		if i >= c.ActiveMax {
			break
		}
		active[r.Slug] = true
	}
	return active, deletable
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/skills/ -run TestRank -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/skills/lifecycle.go internal/skills/lifecycle_test.go
git commit -m "feat(skills): pure Rank (exponential-decay activity, derived tri-state)"
```

---

## Task 5: config — SkillLifecycleCfg

**Files:**
- Modify: `internal/config/config.go`(SkillsLearnerCfg 附近,约 config.go:263)
- Modify: `internal/config/skills_learner_cfg_test.go`

- [ ] **Step 1: Write the failing test**

追加到 `internal/config/skills_learner_cfg_test.go`:

```go
func TestSkillLifecycleIsEnabled(t *testing.T) {
	if !(SkillLifecycleCfg{}).IsEnabled() {
		t.Fatal("nil Enabled should default to enabled")
	}
	tru := true
	if !(SkillLifecycleCfg{Enabled: &tru}).IsEnabled() {
		t.Fatal("explicit true")
	}
	fal := false
	if (SkillLifecycleCfg{Enabled: &fal}).IsEnabled() {
		t.Fatal("explicit false should disable")
	}
}

func TestSkillsLearnerCarriesLifecycle(t *testing.T) {
	// 结构可组合、字段可访问(编译级保证 + 默认零值)。
	c := SkillsLearnerCfg{Lifecycle: SkillLifecycleCfg{ActiveMax: 5}}
	if c.Lifecycle.ActiveMax != 5 {
		t.Fatal("lifecycle not wired into SkillsLearnerCfg")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run 'TestSkillLifecycle|TestSkillsLearnerCarriesLifecycle' -v`
Expected: FAIL(`undefined: SkillLifecycleCfg`)

- [ ] **Step 3: Write minimal implementation**

在 `internal/config/config.go` 的 `SkillsLearnerCfg` 定义(config.go:263)里加字段,并新增结构体:

```go
type SkillsLearnerCfg struct {
	Enabled      *bool  `json:"enabled,omitempty"`
	MinToolCalls int    `json:"minToolCalls,omitempty"`
	Model        string `json:"model,omitempty"`
	// Lifecycle 控制 learner 技能的 active/cooling/deleted 生命周期。
	Lifecycle SkillLifecycleCfg `json:"lifecycle,omitempty"`
}

// SkillLifecycleCfg 是 learner 技能生命周期的可调参数(全部可选,零值兜默认)。
type SkillLifecycleCfg struct {
	Enabled          *bool `json:"enabled,omitempty"`          // nil=默认开启
	ActiveMax        int   `json:"activeMax,omitempty"`        // K, 默认10
	HalfLifeLoads    int   `json:"halfLifeLoads,omitempty"`    // H, 默认32
	ProtectLoads     int   `json:"protectLoads,omitempty"`     // G, 默认20
	EditProtectLoads int   `json:"editProtectLoads,omitempty"` // P, 默认30
	DeleteAfterLoads int   `json:"deleteAfterLoads,omitempty"` // D, 默认200
	ExplicitGain     int   `json:"explicitGain,omitempty"`     // 默认3
}

func (c SkillLifecycleCfg) IsEnabled() bool { return c.Enabled == nil || *c.Enabled }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run 'TestSkillLifecycle|TestSkillsLearnerCarriesLifecycle' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/skills_learner_cfg_test.go
git commit -m "feat(config): SkillLifecycleCfg under SkillsLearnerCfg"
```

---

## Task 6: tools — load_skill 加 invoked_by_user + 记账

**Files:**
- Modify: `internal/agent/tools/load_skill.go`
- Modify: `internal/agent/tools/load_skill_test.go`

**接口说明:** load_skill 记账需要 store + agentID + lifecycle 参数(H、ExplicitGain)。定义一个窄接口只含要用的方法,避免 tools 包依赖整个 store.Store。

- [ ] **Step 1: Write the failing test**

追加到 `internal/agent/tools/load_skill_test.go`(若无 import 补 `context`/`encoding/json`/`testing`):

```go
// fakeSkillRecorder 记录 RecordSkillLoad 的调用参数。返回签名与 store 完全一致
// (返回 *store.SkillUsageRow, error),使真实 store.Store 也满足 skillLoadRecorder。
type fakeSkillRecorder struct {
	agentID       string
	slug          string
	invokedByUser bool
	calls         int
}

func (f *fakeSkillRecorder) RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, h, gain int) (*store.SkillUsageRow, error) {
	f.agentID = agentID
	f.slug = slug
	f.invokedByUser = invokedByUser
	f.calls++
	return nil, nil
}

func TestLoadSkillRecordsInvokedByUser(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "foo", "---\nname: Foo\ndescription: d\n---\nbody\n")
	rec := &fakeSkillRecorder{}
	r := NewRegistry()
	RegisterLoadSkillWithLedger(r, []string{dir}, rec, "agentA", 32, 3)

	out, err := callTool(t, r, "load_skill", map[string]any{"name": "foo", "invoked_by_user": true})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if out == "" {
		t.Fatal("expected skill content returned")
	}
	// 记账是异步的:等待其完成(测试用同步 hook 或轮询)。
	waitFor(t, func() bool { return rec.calls == 1 })
	if rec.slug != "foo" || !rec.invokedByUser || rec.agentID != "agentA" {
		t.Fatalf("recorder got wrong args: %+v", rec)
	}
}
```

> 说明:`writeSkill`/`callTool`/`waitFor` 若 load_skill_test.go 尚无,按下方实现补入(放测试文件顶部):
```go
func writeSkill(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func callTool(t *testing.T, r *Registry, name string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return r.Call(context.Background(), name, raw)
}
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
```
> `r.Call` 若非 Registry 的实际执行方法名,替换为现有等价方法(查 registry.go 的公开执行入口)。

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/tools/ -run TestLoadSkillRecordsInvokedByUser -v`
Expected: FAIL(`undefined: RegisterLoadSkillWithLedger`)

- [ ] **Step 3: Write minimal implementation**

改 `internal/agent/tools/load_skill.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkcrab/internal/store"
)

type loadSkillArgs struct {
	Name          string `json:"name"`
	InvokedByUser bool   `json:"invoked_by_user"`
}

// skillLoadRecorder 是 load_skill 记账所需的窄接口。签名与 store.RecordSkillLoad
// 完全一致(含 *store.SkillUsageRow 返回值),使真实 store.Store 直接满足它;
// 工具侧忽略行快照,只关心 error。
type skillLoadRecorder interface {
	RecordSkillLoad(ctx context.Context, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*store.SkillUsageRow, error)
}

// RegisterLoadSkill 保留旧签名(无账本):plan 模式/无 store 装配走这条,不记账。
func RegisterLoadSkill(r *Registry, skillDirs []string) {
	registerLoadSkill(r, skillDirs, nil, "", 0, 0)
}

// RegisterLoadSkillWithLedger 带生命周期账本:加载成功后异步记账。
func RegisterLoadSkillWithLedger(r *Registry, skillDirs []string, rec skillLoadRecorder, agentID string, halfLifeLoads, explicitGain int) {
	registerLoadSkill(r, skillDirs, rec, agentID, halfLifeLoads, explicitGain)
}

func registerLoadSkill(r *Registry, skillDirs []string, rec skillLoadRecorder, agentID string, halfLifeLoads, explicitGain int) {
	r.Register("load_skill", "Load the full content of a skill by name. Use this when you need detailed instructions for a specific skill.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "The skill name to load",
			},
			"invoked_by_user": map[string]interface{}{
				"type":        "boolean",
				"description": "Set true only when the user explicitly named or clearly asked for this specific skill in their message; false when you selected it on your own initiative.",
			},
		},
		"required": []string{"name"},
	}, makeLoadSkill(skillDirs, rec, agentID, halfLifeLoads, explicitGain))
}

func makeLoadSkill(skillDirs []string, rec skillLoadRecorder, agentID string, halfLifeLoads, explicitGain int) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args loadSkillArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		if args.Name == "" {
			return "", fmt.Errorf("skill name is required")
		}
		for _, dir := range skillDirs {
			if dir == "" {
				continue
			}
			skillPath := filepath.Join(dir, args.Name, "SKILL.md")
			data, err := os.ReadFile(skillPath)
			if err == nil {
				skillDir, _ := filepath.Abs(filepath.Join(dir, args.Name))
				content := strings.ReplaceAll(string(data), "{baseDir}", skillDir)
				// 记账不阻塞返回:异步。无行(手工技能)由 store 侧跳过。
				if rec != nil {
					diskHash := store.HashSkillContent(string(data))
					go func(name string, invoked bool) {
						rc := context.WithoutCancel(ctx)
						if _, err := rec.RecordSkillLoad(rc, agentID, name, diskHash, invoked, halfLifeLoads, explicitGain); err != nil {
							slog.Warn("skill usage record failed", "agent", agentID, "slug", name, "error", err)
						}
					}(args.Name, args.InvokedByUser)
				}
				return wrapSkillContentInternal(args.Name, content), nil
			}
		}
		return "", fmt.Errorf("skill %q not found", args.Name)
	}
}
```

`wrapSkillContentInternal` 保持不变(已在文件内)。

> **测试同步性说明:** 上面异步 `go func` 让 `waitFor` 轮询可通过。若 Registry 执行入口名不是 `Call`,在测试里替换为实际方法。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/tools/ -run TestLoadSkillRecordsInvokedByUser -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tools/load_skill.go internal/agent/tools/load_skill_test.go
git commit -m "feat(tools): load_skill invoked_by_user param + async usage accounting"
```

---

## Task 7: agent — learner 建账本行 + 提取 prompt 注入 slug 清单

**Files:**
- Modify: `internal/agent/skills_learner.go`
- Modify: `internal/agent/skills_learner_test.go`

**背景:** `SkillsLearner`(skills_learner.go:17)当前无 store 引用。加一个可选的账本 upsert 回调(与 tools 同理,窄接口),`persistExtracted`(skills_learner.go:83)成功 Create/Update 后调用。

- [ ] **Step 1: Write the failing test**

追加到 `internal/agent/skills_learner_test.go`:

```go
type fakeLedger struct {
	upserts []struct {
		slug        string
		firstCreate bool
	}
}

func (f *fakeLedger) UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error {
	f.upserts = append(f.upserts, struct {
		slug        string
		firstCreate bool
	}{slug, firstCreate})
	return nil
}

func TestPersistExtractedUpsertsLedgerOnCreate(t *testing.T) {
	ws := t.TempDir()
	led := &fakeLedger{}
	sl := NewSkillsLearner(ws, nil, "m")
	sl.agentID = "agentA"
	sl.ledger = led

	skill := &extractedSkill{
		Name: "Foo", Slug: "foo", Description: "d",
		Content: "---\nname: Foo\ndescription: d\n---\nbody\n",
	}
	if err := sl.persistExtracted(context.Background(), skill); err != nil {
		t.Fatalf("persist: %v", err)
	}
	if len(led.upserts) != 1 || led.upserts[0].slug != "foo" || !led.upserts[0].firstCreate {
		t.Fatalf("expected firstCreate upsert, got %+v", led.upserts)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestPersistExtractedUpsertsLedger -v`
Expected: FAIL(`sl.ledger undefined` / `sl.agentID undefined`)

- [ ] **Step 3: Write minimal implementation**

改 `internal/agent/skills_learner.go`:

在 `SkillsLearner` 结构体(skills_learner.go:17)加字段:

```go
type SkillsLearner struct {
	workspace    string
	provider     provider.Provider
	model        string
	minToolCalls int
	skillDirs    []string
	manager      *skills.Manager
	// —— 生命周期账本(可选;nil 则不记账,行为回到当前)——
	agentID string
	ledger  skillLedger
}

// skillLedger 是 learner 建账本行所需的窄接口(store.Store 满足)。
type skillLedger interface {
	UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error
}
```

改 `persistExtracted`(skills_learner.go:83)在成功写盘后 upsert:

```go
func (sl *SkillsLearner) persistExtracted(ctx context.Context, skill *extractedSkill) error {
	if existing, ok := sl.manager.Read(skill.Slug); ok {
		merged, err := sl.decideUpdate(ctx, existing, skill)
		if err != nil {
			return fmt.Errorf("decide update: %w", err)
		}
		if merged == "" {
			slog.Debug("skill exists, update not needed", "slug", skill.Slug)
			return nil
		}
		if err := sl.manager.Update(skill.Slug, merged); err != nil {
			slog.Warn("skill update rejected", "slug", skill.Slug, "error", err)
			return nil
		}
		sl.upsertLedger(ctx, skill.Slug, merged, false)
		slog.Info("updated existing skill", "name", skill.Name, "slug", skill.Slug)
		return nil
	}

	if err := sl.manager.Create(skill.Slug, skill.Content); err != nil {
		slog.Warn("skill create rejected", "slug", skill.Slug, "error", err)
		return nil
	}
	sl.upsertLedger(ctx, skill.Slug, skill.Content, true)
	slog.Info("extracted new skill", "name", skill.Name, "slug", skill.Slug)
	return nil
}

// upsertLedger 建/更生命周期账本行。账本或 agentID 缺失则跳过(不记账,行为不变)。
func (sl *SkillsLearner) upsertLedger(ctx context.Context, slug, content string, firstCreate bool) {
	if sl.ledger == nil || sl.agentID == "" {
		return
	}
	hash := store.HashSkillContent(content)
	if err := sl.ledger.UpsertSkillUsage(ctx, sl.agentID, slug, hash, firstCreate); err != nil {
		slog.Warn("skill ledger upsert failed", "slug", slug, "error", err)
	}
}
```

确保 `skills_learner.go` 顶部 import 含 `"github.com/qs3c/bkcrab/internal/store"`(已在 import 内,见 skills_learner.go:14)。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestPersistExtractedUpsertsLedger -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/skills_learner.go internal/agent/skills_learner_test.go
git commit -m "feat(agent): learner upserts skill_usage ledger on create/update"
```

---

## Task 8: agent — catalog 按 Rank 过滤 + directive 补充

**Files:**
- Modify: `internal/agent/skills.go`
- Modify: `internal/agent/skills_test.go`

**背景:** `BuildSkillsSummary`(skills.go:284)当前无条件列出所有技能。加一个前置过滤:learner 技能里只保留 Rank 判定为 active 的;非 learner(无账本行)直通;`ListSkillUsage` 失败则全直通(fail-open)。

**关键判定:** 一个技能是不是 learner 由"账本里有它的行"决定。过滤函数接收账本行集,slug 在账本中 → 受 Rank 约束;不在账本 → 直通。

- [ ] **Step 1: Write the failing test**

追加到 `internal/agent/skills_test.go`:

```go
func TestFilterLearnerSkillsByActive(t *testing.T) {
	all := []Skill{
		{Name: "manual", Layer: "user"},   // 无账本行 → 直通
		{Name: "hi", Layer: "agent"},       // learner, active
		{Name: "lo", Layer: "agent"},       // learner, cooling
	}
	rows := []store.SkillUsageRow{
		{Slug: "hi", Origin: "learner", Activity: 10, LastLoadSeq: 100, TotalLoads: 3, CreatedSeq: 0},
		{Slug: "lo", Origin: "learner", Activity: 1, LastLoadSeq: 100, TotalLoads: 3, CreatedSeq: 0},
	}
	cfg := skills.LifecycleConfig{ActiveMax: 1, HalfLifeLoads: 32, ProtectLoads: 20, DeleteAfterLoads: 200}
	out := filterActiveSkills(all, rows, cfg)
	names := map[string]bool{}
	for _, s := range out {
		names[s.Name] = true
	}
	if !names["manual"] || !names["hi"] || names["lo"] {
		t.Fatalf("want manual+hi kept, lo dropped. got %+v", names)
	}
}

func TestFilterFailOpenOnNilRows(t *testing.T) {
	all := []Skill{{Name: "a", Layer: "agent"}, {Name: "b", Layer: "agent"}}
	// rows=nil 模拟 ListSkillUsage 失败:全直通。
	out := filterActiveSkills(all, nil, skills.LifecycleConfig{ActiveMax: 1})
	if len(out) != 2 {
		t.Fatalf("fail-open should keep all, got %d", len(out))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run 'TestFilterLearnerSkillsByActive|TestFilterFailOpenOnNilRows' -v`
Expected: FAIL(`undefined: filterActiveSkills`)

- [ ] **Step 3: Write minimal implementation**

在 `internal/agent/skills.go` 加过滤函数(纯函数,便于单测),并在 `BuildSkillsSummary` 入口调用:

```go
// filterActiveSkills 剔除落入 cooling 的 learner 技能。判定 learner 的依据是
// "账本里有该 slug 的行";无行的技能(手工/bundled/存量 learner)一律直通。
// rows 为 nil(如 ListSkillUsage 失败)时全直通——fail-open,绝不因账本读失败而藏技能。
func filterActiveSkills(all []Skill, rows []store.SkillUsageRow, cfg skills.LifecycleConfig) []Skill {
	if len(rows) == 0 {
		return all
	}
	ledger := make(map[string]bool, len(rows))
	for _, r := range rows {
		ledger[r.Slug] = true
	}
	active, _ := skills.Rank(rows, skills.NowSeq(rows), cfg)
	out := make([]Skill, 0, len(all))
	for _, s := range all {
		if ledger[s.Name] && !active[s.Name] {
			continue // learner 技能且落 cooling:剔出 catalog
		}
		out = append(out, s)
	}
	return out
}
```

`BuildSkillsSummary` 签名保持不变,但调用方(agent 构建 system prompt 处)在传入 `skills` 前先 `filterActiveSkills`。为让 loader 能取账本行,给 `SkillsLoader` 加可选字段并在装配时(Task 10)接上:

```go
// 在 SkillsLoader 结构体加:
//   usageLister func(ctx context.Context, agentID string) ([]store.SkillUsageRow, error)
//   lifecycleCfg skills.LifecycleConfig
//   lifecycleOn  bool
// 并新增方法:
func (sl *SkillsLoader) FilterActive(ctx context.Context, list []Skill) []Skill {
	if !sl.lifecycleOn || sl.usageLister == nil || sl.agentID == "" {
		return list
	}
	rows, err := sl.usageLister(ctx, sl.agentID)
	if err != nil {
		slog.Warn("skill usage list failed; fail-open", "agent", sl.agentID, "error", err)
		return list // fail-open
	}
	return filterActiveSkills(list, rows, sl.lifecycleCfg)
}
```

在 `skillsDirective`(skills.go:366)的 `load_skill` 说明句尾补一句(告诉模型自陈述语义):

```
When you call load_skill, set invoked_by_user=true only if the user explicitly named or clearly asked for that specific skill; set it false when you chose the skill on your own initiative.
```

调用点:构建 system prompt 处把 `LoadSkills()` 的结果先经 `loader.FilterActive(ctx, skills)` 再传给 `BuildSkillsSummary`(Task 10 装配时接线;此 Task 仅落纯函数 + loader 方法 + 测试)。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run 'TestFilterLearnerSkillsByActive|TestFilterFailOpenOnNilRows' -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/skills.go internal/agent/skills_test.go
git commit -m "feat(agent): filter cooling learner skills out of prompt catalog"
```

---

## Task 9: agent — post-turn 删除清理接线

**Files:**
- Modify: `internal/agent/loop.go`(`maybeExtractSkillsCadence` 附近,loop.go:2830)
- Modify: `internal/agent/skills_learner_test.go` 或新增 `internal/agent/skill_cleanup_test.go`

**背景:** deleted 是唯一持久动作。搭 `maybeExtractSkillsCadence` 同一条 post-turn goroutine 尾流,扫账本取 deletable → `Manager.Delete` + `DeleteSkillUsage`。

- [ ] **Step 1: Write the failing test**

新建 `internal/agent/skill_cleanup_test.go`:

```go
package agent

import (
	"context"
	"testing"

	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
)

type fakeCleanupStore struct {
	rows    []store.SkillUsageRow
	deleted []string
}

func (f *fakeCleanupStore) ListSkillUsage(ctx context.Context, agentID string) ([]store.SkillUsageRow, error) {
	return f.rows, nil
}
func (f *fakeCleanupStore) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	f.deleted = append(f.deleted, slug)
	return nil
}

func TestCleanupDeletesDeadLearnerSkills(t *testing.T) {
	ws := t.TempDir()
	mgr := skills.NewManager(ws+"/skills", skills.DefaultManagerConfig())
	// 建一个盘上的 dead 技能目录。
	_ = mgr.Create("dead", "---\nname: Dead\ndescription: d\n---\nbody\n")

	st := &fakeCleanupStore{rows: []store.SkillUsageRow{
		{Slug: "dead", Origin: "learner", TotalLoads: 0, CreatedSeq: 0, EditedSeq: 0},
	}}
	cfg := skills.LifecycleConfig{ActiveMax: 10, DeleteAfterLoads: 200}
	// now-created=300 > 200 → dead 可删。用 NowSeq 需要一行推进时钟,这里直接传 nowSeq。
	cleanupDeadSkills(context.Background(), st, mgr, "agentA", 300, cfg)

	if len(st.deleted) != 1 || st.deleted[0] != "dead" {
		t.Fatalf("want dead deleted from ledger, got %+v", st.deleted)
	}
	if _, ok := mgr.Read("dead"); ok {
		t.Fatalf("skill dir should be removed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestCleanupDeletesDeadLearnerSkills -v`
Expected: FAIL(`undefined: cleanupDeadSkills`)

- [ ] **Step 3: Write minimal implementation**

在 `internal/agent/loop.go` 加清理函数,并在 `maybeExtractSkillsCadence`(loop.go:2830)的 goroutine 尾部调用:

```go
// skillCleanupStore 是清理所需的窄接口(store.Store 满足)。
type skillCleanupStore interface {
	ListSkillUsage(ctx context.Context, agentID string) ([]store.SkillUsageRow, error)
	DeleteSkillUsage(ctx context.Context, agentID, slug string) error
}

// cleanupDeadSkills 删除"从没被加载、超 D、未手改"的 learner 技能(目录+账本行)。
// nowSeq 为当前时钟;调用方从账本 NowSeq 取或复用已知值。
func cleanupDeadSkills(ctx context.Context, st skillCleanupStore, mgr *skills.Manager, agentID string, nowSeq int64, cfg skills.LifecycleConfig) {
	rows, err := st.ListSkillUsage(ctx, agentID)
	if err != nil {
		slog.Warn("skill cleanup: list failed", "agent", agentID, "error", err)
		return
	}
	_, deletable := skills.Rank(rows, nowSeq, cfg)
	for _, slug := range deletable {
		if err := mgr.Delete(slug); err != nil {
			slog.Warn("skill cleanup: dir delete failed", "slug", slug, "error", err)
			continue
		}
		if err := st.DeleteSkillUsage(ctx, agentID, slug); err != nil {
			slog.Warn("skill cleanup: ledger delete failed", "slug", slug, "error", err)
		}
	}
}
```

在 `maybeExtractSkillsCadence` 的 goroutine 内(claim/extract 之后)追加清理调用。account 时钟从账本内部 `NowSeq` 取,故清理函数内可改为先 List 再算 `skills.NowSeq(rows)` 传入 Rank(把上面的 `nowSeq` 参数换成内部计算)。为契合 Task 9 测试签名保留 `nowSeq` 参数,装配处(Task 10)传 `skills.NowSeq(rows)`——或在 loop 里先 List 再调。**实现选择:** loop 内先 `rows,_ := dataStore.ListSkillUsage(...)`,`cleanupDeadSkills(ctx, dataStore, a.skillsLearner.manager, a.name, skills.NowSeq(rows), cfg)`。

> `a.skillsLearner.manager` 目前未导出访问;在 SkillsLearner 上加 `func (sl *SkillsLearner) Manager() *skills.Manager { return sl.manager }`。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestCleanupDeletesDeadLearnerSkills -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/loop.go internal/agent/skill_cleanup_test.go internal/agent/skills_learner.go
git commit -m "feat(agent): post-turn cleanup of dead learner skills"
```

---

## Task 10: 装配 + 全量回归

**Files:**
- Modify: `internal/agent/loop.go`(agent 构建处,约 loop.go:257-266 learner 装配 + system prompt 构建处)

**目标:** 把 store + lifecycleCfg 接到三处:(a) load_skill 注册用 `RegisterLoadSkillWithLedger`;(b) learner 的 `agentID`/`ledger`;(c) system prompt 构建处调用 `loader.FilterActive`;(d) post-turn 清理接线。全部只在 `dataStore != nil && lifecycleCfg.IsEnabled()` 时启用。

- [ ] **Step 1: 接线 learner 账本(loop.go:263 附近)**

在 `ag.skillsLearner = NewSkillsLearner(...)` 之后:

```go
	if ag.dataStore != nil && fullCfg.SkillsLearner.Lifecycle.IsEnabled() {
		ag.skillsLearner.agentID = rc.ID
		ag.skillsLearner.ledger = ag.dataStore
	}
```

- [ ] **Step 2: 接线 load_skill 账本**

找到 `RegisterLoadSkill(reg, skillDirs)` 的调用点(grep `RegisterLoadSkill(`),替换为条件装配:

```go
	if a.dataStore != nil && lifecycleCfg.IsEnabled() {
		tools.RegisterLoadSkillWithLedger(reg, skillDirs, a.dataStore, a.name,
			lifecycleH(lifecycleCfg), lifecycleGain(lifecycleCfg))
	} else {
		tools.RegisterLoadSkill(reg, skillDirs)
	}
```

其中 `lifecycleH`/`lifecycleGain` 是把 cfg 的 0 值兜默认的小 helper(H 默认 32、gain 默认 3),放 agent 包:

```go
func lifecycleH(c config.SkillLifecycleCfg) int {
	if c.HalfLifeLoads > 0 {
		return c.HalfLifeLoads
	}
	return 32
}
func lifecycleGain(c config.SkillLifecycleCfg) int {
	if c.ExplicitGain > 0 {
		return c.ExplicitGain
	}
	return 3
}
```

- [ ] **Step 3: 接线 catalog 过滤(system prompt 构建处)**

找到 `BuildSkillsSummary(loadedSkills)` 的调用点(grep `BuildSkillsSummary(`)。在其前设置 loader 的生命周期字段并过滤:

```go
	// loader 已有 agentID(WithObjectStore 或直接赋值);补生命周期依赖。
	loader.lifecycleOn = a.dataStore != nil && lifecycleCfg.IsEnabled()
	loader.usageLister = func(ctx context.Context, agentID string) ([]store.SkillUsageRow, error) {
		return a.dataStore.ListSkillUsage(ctx, agentID)
	}
	loader.lifecycleCfg = skills.LifecycleConfig{
		ActiveMax: lifecycleCfg.ActiveMax, HalfLifeLoads: lifecycleCfg.HalfLifeLoads,
		ProtectLoads: lifecycleCfg.ProtectLoads, EditProtectLoads: lifecycleCfg.EditProtectLoads,
		DeleteAfterLoads: lifecycleCfg.DeleteAfterLoads,
	}
	loadedSkills = loader.FilterActive(ctx, loadedSkills)
```

> 若 system prompt 构建处拿不到 request ctx,用 `context.Background()`(只读一次账本,无取消需求)。`lifecycleCfg` 来源为 `fullCfg.SkillsLearner.Lifecycle`,在构建处从 agent 持有的 config 取;若该处无 fullCfg,给 Agent 加字段缓存 `a.lifecycleCfg config.SkillLifecycleCfg` 于构建时赋值。

- [ ] **Step 4: 接线 post-turn 清理**

在 `maybeExtractSkillsCadence`(loop.go:2830)的 goroutine 尾部:

```go
	if a.dataStore != nil && a.lifecycleCfg.IsEnabled() {
		rows, err := a.dataStore.ListSkillUsage(extractCtx, a.name)
		if err == nil {
			cfg := skills.LifecycleConfig{
				ActiveMax: a.lifecycleCfg.ActiveMax, HalfLifeLoads: a.lifecycleCfg.HalfLifeLoads,
				ProtectLoads: a.lifecycleCfg.ProtectLoads, EditProtectLoads: a.lifecycleCfg.EditProtectLoads,
				DeleteAfterLoads: a.lifecycleCfg.DeleteAfterLoads,
			}
			cleanupDeadSkills(extractCtx, a.dataStore, a.skillsLearner.Manager(), a.name, skills.NowSeq(rows), cfg)
		}
	}
```

> 若 `maybeExtractSkillsCadence` 仅在 `a.skillsLearner != nil` 分支被调,清理天然只在 learner 启用时跑,符合预期。

- [ ] **Step 5: 全量回归 + 提交**

Run:
```bash
go build ./... && go test ./...
```
Expected: 全部 PASS(no build errors, all green)

修掉任何编译错误(常见:`store.SkillUsageRow` import、`config.SkillLifecycleCfg` 传参、`a.lifecycleCfg` 字段声明与赋值)。全绿后:

```bash
git add -A
git commit -m "feat(agent): wire skill lifecycle (ledger+filter+cleanup) end-to-end"
```

---

## Self-Review 记录

**Spec coverage 对照:**

| Spec 组件 | Task |
|---|---|
| §数据模型 skill_usage 表(双方言) | Task 1 |
| DecayFactor / HashSkillContent | Task 1 |
| Upsert/List/Delete SkillUsage | Task 2 |
| RecordSkillLoad(decay+gain+手改+无行跳过) | Task 3 |
| §打分 Rank / NowSeq / 保护期 / 前 K / 删除 / NaN-Inf 钳制 | Task 4 |
| §7.7 config SkillLifecycleCfg | Task 5 |
| §7.3 load_skill invoked_by_user + 记账 | Task 6 |
| §7.4 learner 建账本 + prompt(注:slug 清单注入见下) | Task 7 |
| §7.5 catalog 过滤 + directive | Task 8 |
| §7.6 post-turn 删除清理 | Task 9 |
| 端到端装配(§6 双路径:lifecycle 仅 store+enabled 时启用) | Task 10 |

**已知缩减(实现时补或确认):**
- **提取 prompt 注入现有 slug 清单**(§7.4 的半程序化去重)在 Task 7 只落了账本 upsert,未含"把现有 learner slug 清单拼进 extractFromSummary 的 prompt"。这是纯 prompt 增强、无新类型,建议作为 Task 7 的一个附加 step 或独立小 Task 落地:在 `extractFromSummary`(skills_learner.go:177)构造 messages 前,若 `ledger` 可列出现有 slug,则把 `slug — description` 清单追加进 system prompt。列 slug 需要一个 `ListSkillUsage` 读取——可复用 `skillLedger` 接口扩一个 List 方法,或直接扫 `workspace/skills`。**实现者决策点,不阻塞核心淘汰逻辑。**

**Type consistency 核对:**
- `RecordSkillLoad` 签名在 store 实现(Task 3)、Store 接口(Task 3)、tools 窄接口 `skillLoadRecorder`(Task 6)、fake(Task 6)四处**完全一致**:`(ctx, agentID, slug, diskHash string, invokedByUser bool, halfLifeLoads, explicitGain int) (*store.SkillUsageRow, error)`。工具侧忽略行快照只看 error,但保留返回值以使真实 `store.Store` 直接满足窄接口(否则 Task 10 传 `a.dataStore` 编译失败)。✅ 已修正对齐。
- `LifecycleConfig` 字段名(ActiveMax/HalfLifeLoads/ProtectLoads/EditProtectLoads/DeleteAfterLoads)在 Task 4/8/9/10 一致。✅
- `UpsertSkillUsage(ctx, agentID, slug, contentHash string, firstCreate bool)` 在 store(Task 2)、learner 窄接口 `skillLedger`(Task 7)一致。✅

**Placeholder 扫描:** 无 TBD/TODO;每个代码 step 含完整可粘贴代码。Task 6 的 `r.Call`/`waitFor` 等测试脚手架给了实现,并注明"若方法名不同则替换为实际入口"——属实现环境适配,非占位。
