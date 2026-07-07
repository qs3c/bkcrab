package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
)

// SkillUsageRow is one (agent, learner skill) lifecycle ledger row.
type SkillUsageRow struct {
	Slug         string
	Origin       string
	Activity     float64
	LastLoadSeq  int64
	TotalLoads   int64
	ExplicitUses int64
	CreatedSeq   int64
	EditedSeq    int64
	ContentHash  string
}

// DecayFactor is the shared exponential decay formula for skill activity.
func DecayFactor(dt int64, halfLifeLoads int) float64 {
	if halfLifeLoads <= 0 {
		halfLifeLoads = 32
	}
	if dt <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(dt)/float64(halfLifeLoads))
}

// HashSkillContent normalizes line endings before hashing SKILL.md content.
func HashSkillContent(content string) string {
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

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

// UpsertSkillUsage creates or refreshes the learner skill ledger row.
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
	var insertSQL string
	switch d.dialect {
	case mysqlDialect:
		insertSQL = fmt.Sprintf(`INSERT IGNORE INTO skill_usage (agent_id, slug, origin, created_seq, content_hash)
			VALUES (%s, %s, 'learner', %s, %s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	case "postgres":
		insertSQL = fmt.Sprintf(`INSERT INTO skill_usage (agent_id, slug, origin, created_seq, content_hash)
			VALUES (%s, %s, 'learner', %s, %s) ON CONFLICT (agent_id, slug) DO NOTHING`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	default:
		insertSQL = fmt.Sprintf(`INSERT OR IGNORE INTO skill_usage (agent_id, slug, origin, created_seq, content_hash)
			VALUES (%s, %s, 'learner', %s, %s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4))
	}
	if _, err := d.db.ExecContext(ctx, insertSQL, agentID, slug, createdSeq, contentHash); err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE skill_usage SET content_hash=%s, updated_at=CURRENT_TIMESTAMP
			WHERE agent_id=%s AND slug=%s`, d.ph(1), d.ph(2), d.ph(3)),
		contentHash, agentID, slug)
	return err
}

// RecordSkillLoad records one successful load_skill hit for a learner skill.
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
		fmt.Sprintf(`SELECT slug, origin, activity, last_load_seq, total_loads, explicit_uses,
			created_seq, edited_seq, content_hash FROM skill_usage
			WHERE agent_id=%s AND slug=%s`+lock, d.ph(1), d.ph(2)), agentID, slug).
		Scan(&r.Slug, &r.Origin, &r.Activity, &r.LastLoadSeq, &r.TotalLoads, &r.ExplicitUses,
			&r.CreatedSeq, &r.EditedSeq, &r.ContentHash)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

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
	r.Activity = r.Activity*DecayFactor(seq-r.LastLoadSeq, halfLifeLoads) + float64(gain)
	r.LastLoadSeq = seq
	r.TotalLoads++
	if invokedByUser {
		r.ExplicitUses++
	}
	if diskHash != "" && r.ContentHash != "" && diskHash != r.ContentHash {
		r.EditedSeq = seq
	}
	// 采纳盘上最新内容 hash,使手改只 stamp 一次 edited_seq。若不回写,后续每次
	// 加载都因盘上 hash 与账本旧值不符而重新 stamp,edited_seq 一路推进 →
	// loadAge 恒≈0 → 手改保护变永久、技能永不冷却。空 diskHash(上游读盘失败)
	// 不覆盖,避免抹掉已知 hash。
	if diskHash != "" {
		r.ContentHash = diskHash
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE skill_usage SET activity=%s, last_load_seq=%s, total_loads=%s,
			explicit_uses=%s, edited_seq=%s, content_hash=%s, updated_at=CURRENT_TIMESTAMP
			WHERE agent_id=%s AND slug=%s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8)),
		r.Activity, r.LastLoadSeq, r.TotalLoads, r.ExplicitUses, r.EditedSeq, r.ContentHash, agentID, slug)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListSkillUsage returns every learner skill ledger row for an agent.
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

// DeleteSkillUsage deletes the ledger row paired with a deleted skill directory.
func (d *DBStore) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM skill_usage WHERE agent_id=%s AND slug=%s`, d.ph(1), d.ph(2)),
		agentID, slug)
	return err
}
