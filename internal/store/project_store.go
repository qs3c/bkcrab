package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// --- Projects ---

func (d *DBStore) ListProjects(ctx context.Context, userID, agentID string) ([]ProjectRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT project_id, name, description, created_at, updated_at FROM projects
			WHERE user_id = %s AND agent_id = %s ORDER BY updated_at DESC`,
			d.ph(1), d.ph(2)),
		userID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectRecord
	for rows.Next() {
		p := ProjectRecord{UserID: userID, AgentID: agentID}
		if err := rows.Scan(&p.ID, &p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DBStore) GetProject(ctx context.Context, userID, agentID, projectID string) (*ProjectRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT name, description, created_at, updated_at FROM projects
			WHERE user_id = %s AND agent_id = %s AND project_id = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, projectID)
	p := ProjectRecord{UserID: userID, AgentID: agentID, ID: projectID}
	if err := row.Scan(&p.Name, &p.Description, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &p, nil
}

// SaveProject 进行 upsert。created_at 在更新时保留；updated_at 每次写入时更新。
// 行级别允许空名称——HTTP 处理程序强制执行非空名称，因此我们在此不必双重验证。
func (d *DBStore) SaveProject(ctx context.Context, p *ProjectRecord) error {
	if p.UserID == "" || p.AgentID == "" || p.ID == "" {
		return errors.New("store: SaveProject requires user_id, agent_id, project_id")
	}
	now := time.Now().UTC()
	if d.dialect == mysqlDialect {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO projects (user_id, agent_id, project_id, name, description, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  name=VALUES(name), description=VALUES(description), updated_at=VALUES(updated_at)`,
			p.UserID, p.AgentID, p.ID, p.Name, p.Description, now, now)
		return err
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO projects (user_id, agent_id, project_id, name, description, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $6)
				ON CONFLICT (user_id, agent_id, project_id) DO UPDATE
				SET name=$4, description=$5, updated_at=$6`,
			p.UserID, p.AgentID, p.ID, p.Name, p.Description, now)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO projects (user_id, agent_id, project_id, name, description, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, agent_id, project_id) DO UPDATE SET
			  name=excluded.name, description=excluded.description, updated_at=excluded.updated_at`,
		p.UserID, p.AgentID, p.ID, p.Name, p.Description, now, now)
	return err
}

// DeleteProject 删除该行。调用方必须确保没有会话仍然引用它（通过
// CountProjectSessions）；此方法不检查，因为处理程序决定策略（阻止 vs 级联）
// ——store 保持机械性。
func (d *DBStore) DeleteProject(ctx context.Context, userID, agentID, projectID string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM projects WHERE user_id = %s AND agent_id = %s AND project_id = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, projectID)
	return err
}

func (d *DBStore) CountProjectSessions(ctx context.Context, userID, agentID, projectID string) (int, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM sessions WHERE user_id = %s AND agent_id = %s AND project_id = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, projectID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
