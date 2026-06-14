package store

import (
	"context"
	"fmt"
	"time"
)

// --- Users ---

// userColumns 是规范的选择列表——保持顺序与下面的 Scan 调用一致，
// 这样添加一列就意味着编辑两行。
const userColumns = `id, username, email, password_hash, display_name, role, status, apikey_id, external_id, avatar_url, agent_quota, created_at, updated_at`

func scanUser(scanner interface{ Scan(dest ...any) error }) (*UserRecord, error) {
	var u UserRecord
	if err := scanner.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.DisplayName, &u.Role, &u.Status, &u.APIKeyID, &u.ExternalID, &u.AvatarURL, &u.AgentQuota, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DBStore) CreateUser(ctx context.Context, u *UserRecord) error {
	now := time.Now().UTC()
	if u.CreatedAt.IsZero() {
		u.CreatedAt = now
	}
	u.UpdatedAt = now
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO users (id, username, email, password_hash, display_name, role, status, apikey_id, external_id, avatar_url, agent_quota, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13)),
		u.ID, u.Username, u.Email, u.PasswordHash, u.DisplayName, u.Role, u.Status, u.APIKeyID, u.ExternalID, u.AvatarURL, u.AgentQuota, u.CreatedAt, u.UpdatedAt)
	return err
}

func (d *DBStore) GetUser(ctx context.Context, id string) (*UserRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE id = %s`, d.ph(1)), id)
	u, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return u, nil
}

func (d *DBStore) GetUserByLogin(ctx context.Context, usernameOrEmail string) (*UserRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE username = %s OR email = %s LIMIT 1`, d.ph(1), d.ph(2)),
		usernameOrEmail, usernameOrEmail)
	u, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return u, nil
}

// GetUserByExternal 通过 (apikey_id, external_id) 查找 app_user。
// 无匹配时返回 ErrNotFound——被 api_key 聊天调用上的惰性创建流程
// 和显式配置端点用于使创建在重入时幂等。
func (d *DBStore) GetUserByExternal(ctx context.Context, apikeyID, externalID string) (*UserRecord, error) {
	if apikeyID == "" || externalID == "" {
		return nil, ErrNotFound
	}
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+userColumns+` FROM users WHERE apikey_id = %s AND external_id = %s LIMIT 1`,
			d.ph(1), d.ph(2)),
		apikeyID, externalID)
	u, err := scanUser(row)
	if err != nil {
		return nil, scanErr(err)
	}
	return u, nil
}

func (d *DBStore) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserRecord
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (d *DBStore) UpdateUser(ctx context.Context, u *UserRecord) error {
	u.UpdatedAt = time.Now().UTC()
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE users SET username = %s, email = %s, password_hash = %s, display_name = %s,
			role = %s, status = %s, avatar_url = %s, agent_quota = %s, updated_at = %s WHERE id = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10)),
		u.Username, u.Email, u.PasswordHash, u.DisplayName, u.Role, u.Status, u.AvatarURL, u.AgentQuota, u.UpdatedAt, u.ID)
	return err
}

func (d *DBStore) DeleteUser(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// 首先，查找此用户拥有的每个 agent——我们将在删除 agent 本身之前
	// 级联处理每个 agent 的状态（cron, agent_files, sessions, configs）。
	rows, err := tx.QueryContext(ctx,
		fmt.Sprintf("SELECT id FROM agents WHERE user_id = %s", d.ph(1)), id)
	if err != nil {
		return err
	}
	var ownedAgents []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			rows.Close()
			return err
		}
		ownedAgents = append(ownedAgents, aid)
	}
	rows.Close()
	for _, aid := range ownedAgents {
		for _, t := range []string{"agent_files", "sessions", "session_messages", "session_events", "cron_jobs"} {
			if _, err := tx.ExecContext(ctx,
				fmt.Sprintf("DELETE FROM %s WHERE agent_id = %s", t, d.ph(1)), aid); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM apikey_agents WHERE agent_id = %s", d.ph(1)), aid); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM configs WHERE agent_id = %s", d.ph(1)), aid); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM agents WHERE user_id = %s", d.ph(1)), id); err != nil {
		return err
	}
	// 非 agent 范围的每用户状态（agent_files 现在仅为 agent 所有）。
	for _, t := range []string{"web_sessions", "apikeys", "sessions", "session_messages", "session_events"} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("DELETE FROM %s WHERE user_id = %s", t, d.ph(1)), id); err != nil {
			return err
		}
	}
	// 删除此用户拥有的每个 config 行——包括他们自己的
	// ('user_id=X, agent_id="') 以及他们在别人的 agent 上创建的任何
	// 每个 agent 覆盖 ('user_id=X, agent_id=Y')。
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM configs WHERE user_id = %s", d.ph(1)), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM apikey_agents WHERE apikey_id NOT IN (SELECT id FROM apikeys)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM users WHERE id = %s", d.ph(1)), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}
