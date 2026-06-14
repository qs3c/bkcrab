package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// --- Agents ---

const agentSelectCols = `id, user_id, name, config, is_public, created_at, updated_at`

func (d *DBStore) ListAgents(ctx context.Context, ownerUserID string) ([]AgentRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+agentSelectCols+` FROM agents WHERE user_id = %s ORDER BY created_at`, d.ph(1)),
		ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgents(rows)
}

func (d *DBStore) GetAgent(ctx context.Context, agentID string) (*AgentRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+agentSelectCols+` FROM agents WHERE id = %s`, d.ph(1)), agentID)
	var ag AgentRecord
	var cfgStr string
	if err := row.Scan(&ag.ID, &ag.UserID, &ag.Name, &cfgStr, &ag.IsPublic, &ag.CreatedAt, &ag.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	json.Unmarshal([]byte(cfgStr), &ag.Config)
	return &ag, nil
}

func (d *DBStore) SaveAgent(ctx context.Context, agent *AgentRecord) error {
	if agent.ID == "" {
		return errors.New("store: agent.id is required")
	}
	if agent.UserID == "" {
		return errors.New("store: agent.user_id is required")
	}
	cfgData, _ := json.Marshal(agent.Config)
	now := time.Now().UTC()
	if agent.CreatedAt.IsZero() {
		agent.CreatedAt = now
	}
	agent.UpdatedAt = now
	if d.dialect == mysqlDialect {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agents (id, user_id, name, config, is_public, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  user_id=VALUES(user_id), name=VALUES(name), config=VALUES(config),
				  is_public=VALUES(is_public), updated_at=VALUES(updated_at)`,
			agent.ID, agent.UserID, agent.Name, string(cfgData), agent.IsPublic, agent.CreatedAt, agent.UpdatedAt)
		return err
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agents (id, user_id, name, config, is_public, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (id) DO UPDATE
				SET user_id=$2, name=$3, config=$4, is_public=$5, updated_at=$7`,
			agent.ID, agent.UserID, agent.Name, string(cfgData), agent.IsPublic, agent.CreatedAt, agent.UpdatedAt)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO agents (id, user_id, name, config, is_public, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			  user_id=excluded.user_id, name=excluded.name,
			  config=excluded.config, is_public=excluded.is_public,
			  updated_at=excluded.updated_at`,
		agent.ID, agent.UserID, agent.Name, string(cfgData), agent.IsPublic, agent.CreatedAt, agent.UpdatedAt)
	return err
}

func (d *DBStore) DeleteAgent(ctx context.Context, agentID string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"agent_files", "sessions", "session_messages", "session_events", "cron_jobs"} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE agent_id = %s`, t, d.ph(1)), agentID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikey_agents WHERE agent_id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	// 删除指向此 agent 的每个 config 行——拥有者的官方行
	// (user_id='', agent_id=X)、agent 拥有者的每个 agent 覆盖
	// (user_id=owner, agent_id=X) 以及任何非拥有者的每个 agent 覆盖
	// (user_id=other, agent_id=X)。
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM configs WHERE agent_id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agents WHERE id = %s`, d.ph(1)), agentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) ListAllAgents(ctx context.Context) ([]AgentRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+agentSelectCols+` FROM agents ORDER BY user_id, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAgents(rows)
}

func scanAgents(rows *sql.Rows) ([]AgentRecord, error) {
	var out []AgentRecord
	for rows.Next() {
		var ag AgentRecord
		var cfgStr string
		if err := rows.Scan(&ag.ID, &ag.UserID, &ag.Name, &cfgStr, &ag.IsPublic, &ag.CreatedAt, &ag.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(cfgStr), &ag.Config)
		out = append(out, ag)
	}
	return out, rows.Err()
}
