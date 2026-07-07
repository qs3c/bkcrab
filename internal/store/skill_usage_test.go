package store

import (
	"context"
	"math"
	"testing"
)

func TestDecayFactor(t *testing.T) {
	if got := DecayFactor(32, 32); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("DecayFactor(32,32)=%v want 0.5", got)
	}
	if got := DecayFactor(0, 32); math.Abs(got-1) > 1e-9 {
		t.Fatalf("DecayFactor(0,32)=%v want 1", got)
	}
	if got := DecayFactor(32, 0); math.Abs(got-0.5) > 1e-9 {
		t.Fatalf("DecayFactor(32,0)=%v want default half-life result 0.5", got)
	}
}

func TestHashSkillContentNormalizesCRLF(t *testing.T) {
	a := HashSkillContent("---\nname: x\n---\nbody\n")
	b := HashSkillContent("---\r\nname: x\r\n---\r\nbody\r\n")
	if a != b {
		t.Fatalf("hash mismatch after line ending normalization: %s vs %s", a, b)
	}
	if len(a) != 64 {
		t.Fatalf("sha256 hex length=%d want 64", len(a))
	}
}

func TestUpsertListAndDeleteSkillUsage(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()

	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "hash1", true); err != nil {
		t.Fatalf("upsert create: %v", err)
	}
	rows, err := db.ListSkillUsage(ctx, "agentA")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Slug != "foo" || rows[0].Origin != "learner" || rows[0].ContentHash != "hash1" {
		t.Fatalf("unexpected row: %+v", rows)
	}

	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "hash2", false); err != nil {
		t.Fatalf("upsert update: %v", err)
	}
	rows, err = db.ListSkillUsage(ctx, "agentA")
	if err != nil {
		t.Fatalf("list after update: %v", err)
	}
	if len(rows) != 1 || rows[0].ContentHash != "hash2" || rows[0].TotalLoads != 0 {
		t.Fatalf("hash update should preserve counters: %+v", rows)
	}
	if other, err := db.ListSkillUsage(ctx, "agentB"); err != nil || len(other) != 0 {
		t.Fatalf("agent isolation broken rows=%+v err=%v", other, err)
	}

	if err := db.DeleteSkillUsage(ctx, "agentA", "foo"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if rows, err := db.ListSkillUsage(ctx, "agentA"); err != nil || len(rows) != 0 {
		t.Fatalf("delete did not remove row rows=%+v err=%v", rows, err)
	}
}

func TestRecordSkillLoad(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "hash", true); err != nil {
		t.Fatal(err)
	}

	row, err := db.RecordSkillLoad(ctx, "agentA", "foo", "hash", false, 32, 3)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if row == nil || row.TotalLoads != 1 || row.LastLoadSeq != 1 || row.ExplicitUses != 0 {
		t.Fatalf("unexpected first record: %+v", row)
	}
	if math.Abs(row.Activity-1) > 1e-9 {
		t.Fatalf("activity=%v want 1", row.Activity)
	}

	row, err = db.RecordSkillLoad(ctx, "agentA", "foo", "hash", true, 32, 3)
	if err != nil {
		t.Fatalf("explicit record: %v", err)
	}
	if row.TotalLoads != 2 || row.ExplicitUses != 1 || row.LastLoadSeq != 2 {
		t.Fatalf("explicit counters wrong: %+v", row)
	}
	if row.Activity < 3.9 || row.Activity > 4.0 {
		t.Fatalf("activity=%v outside expected band", row.Activity)
	}
}

func TestRecordSkillLoadDetectsEditAndSkipsNoRow(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "orig", true); err != nil {
		t.Fatal(err)
	}
	row, err := db.RecordSkillLoad(ctx, "agentA", "foo", "changed", false, 32, 3)
	if err != nil {
		t.Fatalf("record changed: %v", err)
	}
	if row == nil || row.EditedSeq == 0 {
		t.Fatalf("edited_seq not set: %+v", row)
	}

	row, err = db.RecordSkillLoad(ctx, "agentA", "manual", "hash", true, 32, 3)
	if err != nil {
		t.Fatalf("record missing row: %v", err)
	}
	if row != nil {
		t.Fatalf("missing ledger row should not be created: %+v", row)
	}
}

// TestRecordSkillLoadEditStampIsOneShot 锁定发现 2 的修复:手改只应 stamp 一次
// edited_seq。若 content_hash 不回写,后续每次加载都因盘上 hash 与账本不符而重新
// stamp,edited_seq 一路推进 → loadAge 恒≈0 → 手改保护变永久,技能永不冷却。
func TestRecordSkillLoadEditStampIsOneShot(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	if err := db.UpsertSkillUsage(ctx, "agentA", "foo", "orig", true); err != nil {
		t.Fatal(err)
	}
	// 加载 1:盘上 hash 与账本("orig")不符 → 检测手改、stamp edited_seq,
	// 并把账本 content_hash 同步为盘上新值。
	r1, err := db.RecordSkillLoad(ctx, "agentA", "foo", "edited", false, 32, 3)
	if err != nil {
		t.Fatalf("load1: %v", err)
	}
	if r1.EditedSeq == 0 {
		t.Fatalf("first divergence should stamp edited_seq")
	}
	firstEdit := r1.EditedSeq
	// 加载 2:盘上 hash 未变(仍 "edited")且已被账本采纳 → 不应再 stamp。
	r2, err := db.RecordSkillLoad(ctx, "agentA", "foo", "edited", false, 32, 3)
	if err != nil {
		t.Fatalf("load2: %v", err)
	}
	if r2.EditedSeq != firstEdit {
		t.Fatalf("edited_seq re-stamped on unchanged disk: was %d now %d (edit protection would be permanent)", firstEdit, r2.EditedSeq)
	}
}
