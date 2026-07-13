package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AdvanceSkillLifecycle advances the persistent, agent-wide lifecycle clock by
// one catalog exposure. When cleanupEvery is positive, the same transaction
// claims a cleanup after that many advances since the previous claim.
func (d *DBStore) AdvanceSkillLifecycle(ctx context.Context, agentID string, cleanupEvery int64) (int64, bool, error) {
	if agentID == "" {
		return 0, false, errors.New("store: AdvanceSkillLifecycle requires agent_id")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	seq, lastCleanup, err := d.lockSkillLifecycle(ctx, tx, agentID)
	if err != nil {
		return 0, false, err
	}
	seq++
	cleanupDue := cleanupEvery > 0 && seq-lastCleanup >= cleanupEvery
	if cleanupDue {
		lastCleanup = seq
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE agent_skill_lifecycle
			SET clock_seq=%s, last_cleanup_seq=%s, updated_at=CURRENT_TIMESTAMP
			WHERE agent_id=%s`, d.ph(1), d.ph(2), d.ph(3)),
		seq, lastCleanup, agentID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return seq, cleanupDue, nil
}

// CurrentSkillLifecycleSeq returns the persistent lifecycle clock without
// advancing it. Agents created before the clock table are initialized lazily
// from their largest recorded lifecycle event, with one as the minimum.
func (d *DBStore) CurrentSkillLifecycleSeq(ctx context.Context, agentID string) (int64, error) {
	if agentID == "" {
		return 0, errors.New("store: CurrentSkillLifecycleSeq requires agent_id")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	seq, _, err := d.lockSkillLifecycle(ctx, tx, agentID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// lockSkillLifecycle ensures the row exists and locks it until tx completes.
// Initializing last_cleanup_seq to the inherited clock makes cleanupEvery count
// real catalog exposures after migration instead of historical ledger values.
func (d *DBStore) lockSkillLifecycle(ctx context.Context, tx *sql.Tx, agentID string) (seq, lastCleanup int64, err error) {
	if d.dialect == mysqlDialect {
		return d.lockMySQLSkillLifecycle(ctx, tx, agentID)
	}
	seq, lastCleanup, found, err := d.selectSkillLifecycleForUpdate(ctx, tx, agentID)
	if err != nil {
		return 0, 0, err
	}
	if found {
		return d.normalizeSkillLifecycle(ctx, tx, agentID, seq, lastCleanup)
	}

	legacySeq, err := d.legacySkillLifecycleSeq(ctx, tx, agentID)
	if err != nil {
		return 0, 0, err
	}

	var insertSQL string
	switch d.dialect {
	case mysqlDialect:
		insertSQL = `INSERT INTO agent_skill_lifecycle
			(agent_id, clock_seq, last_cleanup_seq) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE agent_id=VALUES(agent_id)`
	case "postgres":
		insertSQL = `INSERT INTO agent_skill_lifecycle
			(agent_id, clock_seq, last_cleanup_seq) VALUES ($1, $2, $3)
			ON CONFLICT (agent_id) DO NOTHING`
	default:
		insertSQL = `INSERT INTO agent_skill_lifecycle
			(agent_id, clock_seq, last_cleanup_seq) VALUES (?, ?, ?)
			ON CONFLICT(agent_id) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, insertSQL, agentID, legacySeq, legacySeq); err != nil {
		return 0, 0, err
	}

	seq, lastCleanup, found, err = d.selectSkillLifecycleForUpdate(ctx, tx, agentID)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, fmt.Errorf("skill lifecycle row missing after initialization for agent %q", agentID)
	}
	return d.normalizeSkillLifecycle(ctx, tx, agentID, seq, lastCleanup)
}

// lockMySQLSkillLifecycle inserts before performing a locking read. Under
// InnoDB REPEATABLE READ, SELECT ... FOR UPDATE on the same missing key in two
// transactions can grant compatible gap locks and deadlock when both then try
// to INSERT. INSERT IGNORE serializes on the unique key without that upgrade
// pattern; only the winner performs legacy initialization.
func (d *DBStore) lockMySQLSkillLifecycle(ctx context.Context, tx *sql.Tx, agentID string) (int64, int64, error) {
	res, err := tx.ExecContext(ctx, `INSERT IGNORE INTO agent_skill_lifecycle
		(agent_id, clock_seq, last_cleanup_seq) VALUES (?, 1, 1)`, agentID)
	if err != nil {
		return 0, 0, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if inserted > 0 {
		legacySeq, err := d.legacySkillLifecycleSeq(ctx, tx, agentID)
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_skill_lifecycle
			SET clock_seq=?, last_cleanup_seq=?, updated_at=CURRENT_TIMESTAMP
			WHERE agent_id=?`, legacySeq, legacySeq, agentID); err != nil {
			return 0, 0, err
		}
	}
	seq, lastCleanup, found, err := d.selectSkillLifecycleForUpdate(ctx, tx, agentID)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return 0, 0, fmt.Errorf("skill lifecycle row missing after MySQL initialization for agent %q", agentID)
	}
	return d.normalizeSkillLifecycle(ctx, tx, agentID, seq, lastCleanup)
}

func (d *DBStore) selectSkillLifecycleForUpdate(ctx context.Context, tx *sql.Tx, agentID string) (seq, lastCleanup int64, found bool, err error) {
	lock := ""
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		lock = " FOR UPDATE"
	}
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT clock_seq, last_cleanup_seq FROM agent_skill_lifecycle
			WHERE agent_id=%s`+lock, d.ph(1)), agentID).
		Scan(&seq, &lastCleanup)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return seq, lastCleanup, true, nil
}

func (d *DBStore) normalizeSkillLifecycle(ctx context.Context, tx *sql.Tx, agentID string, seq, lastCleanup int64) (int64, int64, error) {
	dirty := false
	if seq < 1 {
		seq = 1
		dirty = true
	}
	if lastCleanup < 0 || lastCleanup > seq {
		lastCleanup = seq
		dirty = true
	}
	if dirty {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`UPDATE agent_skill_lifecycle SET clock_seq=%s, last_cleanup_seq=%s,
				updated_at=CURRENT_TIMESTAMP WHERE agent_id=%s`, d.ph(1), d.ph(2), d.ph(3)),
			seq, lastCleanup, agentID); err != nil {
			return 0, 0, err
		}
	}
	return seq, lastCleanup, nil
}

func (d *DBStore) legacySkillLifecycleSeq(ctx context.Context, tx *sql.Tx, agentID string) (int64, error) {
	greatest := "MAX(created_seq, edited_seq, last_load_seq)"
	if d.dialect == "postgres" || d.dialect == mysqlDialect {
		greatest = "GREATEST(created_seq, edited_seq, last_load_seq)"
	}
	var seq sql.NullInt64
	err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT MAX(%s) FROM skill_usage WHERE agent_id=%s`, greatest, d.ph(1)),
		agentID).Scan(&seq)
	if err != nil {
		return 0, err
	}
	if !seq.Valid || seq.Int64 < 1 {
		return 1, nil
	}
	return seq.Int64, nil
}
