package store

import (
	"context"
	"database/sql"
	"fmt"
)

type deletionExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// MarkAgentDeleting is a durable fail-closed tombstone written before external
// learner assets are removed. It intentionally survives final agent deletion,
// preventing a stale background job (or immediate ID reuse) from recreating
// the deleted agent's private learner assets.
func (d *DBStore) MarkAgentDeleting(ctx context.Context, agentID string) error {
	if agentID == "" {
		return fmt.Errorf("store: agent id is required for deletion marker")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := d.lockAgentMutation(ctx, tx, agentID); err != nil {
		return err
	}
	if err := d.markAgentDeleting(ctx, tx, agentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) lockAgentMutation(ctx context.Context, tx *sql.Tx, agentID string) error {
	var insert string
	switch d.dialect {
	case mysqlDialect:
		insert = `INSERT IGNORE INTO agent_mutation_locks (agent_id) VALUES (?)`
	case "postgres":
		insert = `INSERT INTO agent_mutation_locks (agent_id) VALUES ($1) ON CONFLICT (agent_id) DO NOTHING`
	default:
		insert = `INSERT INTO agent_mutation_locks (agent_id) VALUES (?) ON CONFLICT(agent_id) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, insert, agentID); err != nil {
		return fmt.Errorf("initialize agent mutation lock: %w", err)
	}
	if d.dialect == "sqlite" {
		// A SQLite write transaction already serializes writers; FOR UPDATE is
		// not supported by its grammar.
		return nil
	}
	var lockedID string
	if err := tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT agent_id FROM agent_mutation_locks WHERE agent_id=%s FOR UPDATE`, d.ph(1)),
		agentID).Scan(&lockedID); err != nil {
		return fmt.Errorf("lock agent mutation row: %w", err)
	}
	return nil
}

func (d *DBStore) markAgentDeleting(ctx context.Context, exec deletionExecer, agentID string) error {
	var query string
	switch d.dialect {
	case mysqlDialect:
		query = `INSERT INTO agent_deletions (agent_id) VALUES (?)
			ON DUPLICATE KEY UPDATE deleted_at=deleted_at`
	case "postgres":
		query = `INSERT INTO agent_deletions (agent_id) VALUES ($1)
			ON CONFLICT (agent_id) DO NOTHING`
	default:
		query = `INSERT INTO agent_deletions (agent_id) VALUES (?)
			ON CONFLICT(agent_id) DO NOTHING`
	}
	_, err := exec.ExecContext(ctx, query, agentID)
	return err
}

func (d *DBStore) IsAgentDeleting(ctx context.Context, agentID string) (bool, error) {
	if agentID == "" {
		return false, nil
	}
	var n int
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM agent_deletions WHERE agent_id=%s`, d.ph(1)), agentID).Scan(&n)
	return n > 0, err
}
