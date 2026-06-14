package store

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// --- Agent 文件 ---
//
// SOUL.md / IDENTITY.md / MEMORY.md / AGENTS.md / BOOTSTRAP.md / 等。
// 键控在 (agent_id, user_id, filename) 上。每行都携带真实的
// user_id——没有共享模板行。
//
// 读取路径：优先使用调用方自己的行；当调用方没有覆盖时回退到 agent
// 拥有者的行。这让非拥有者调用方（与之共享 agent 的其他人类，或代表
// 下游应用最终用户创建的 app_user 账户）继承拥有者自定义的 SOUL.md /
// IDENTITY.md，同时仍然能够通过保存来创建自己的 MEMORY.md / USER.md——
// 保存始终写入调用方的精确行，从不写入拥有者的行。运行时还回退到
// <agent_home>/<name> 处的本地 FS 文件，适用于希望为 agent 设置全局默认值的安装。

// GetAgentFile 返回 (agent_id, filename) 的文件，优先使用调用方自己的行，
// 回退到 agent 拥有者的行。userID 是必需的。
func (d *DBStore) GetAgentFile(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if agentID == "" {
		return nil, errors.New("store: GetAgentFile requires agent_id")
	}
	if userID == "" {
		return nil, errors.New("store: GetAgentFile requires user_id")
	}
	// 单次往返：如果存在则选择调用方的行（排序键 0），否则选择拥有者的行
	//（排序键 1）。LIMIT 1 返回胜出行。子查询解析 agent 的拥有者；
	// 如果 agent 不存在，它只产生 NULL 且 IN 忽略它——调用方的行在存在时
	// 仍然返回，否则返回 NoRows。
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT content FROM agent_files
			WHERE agent_id = %s AND filename = %s
			  AND user_id IN (%s, COALESCE((SELECT user_id FROM agents WHERE id = %s), ''))
			ORDER BY CASE WHEN user_id = %s THEN 0 ELSE 1 END
			LIMIT 1`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		agentID, filename, userID, agentID, userID)
	var content string
	if err := row.Scan(&content); err != nil {
		return nil, scanErr(err)
	}
	return []byte(content), nil
}

// GetAgentFileExact 绕过拥有者回退覆盖层，仅返回 (agent_id, user_id, filename)
// 行，或 ErrNotFound。当调用方明确需要知道*他们自己的*覆盖行是否存在时使用
// （例如 Customize 页面区分"你已创建覆盖"与"你正在查看拥有者的内容"）。
func (d *DBStore) GetAgentFileExact(ctx context.Context, agentID, userID, filename string) ([]byte, error) {
	if agentID == "" {
		return nil, errors.New("store: GetAgentFileExact requires agent_id")
	}
	if userID == "" {
		return nil, errors.New("store: GetAgentFileExact requires user_id")
	}
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT content FROM agent_files
			WHERE agent_id = %s AND user_id = %s AND filename = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, userID, filename)
	var content string
	if err := row.Scan(&content); err != nil {
		return nil, scanErr(err)
	}
	return []byte(content), nil
}

// SaveAgentFile 精确写入 (agent_id, user_id, filename) 行。
// userID 是必需的——每次写入都是每用户的。如果你想要 agent 的一个共享默认值，
// 请使用 <agent_home>/<name> 处的本地 FS 文件。
func (d *DBStore) SaveAgentFile(ctx context.Context, agentID, userID, filename string, data []byte) error {
	if agentID == "" {
		return errors.New("store: SaveAgentFile requires agent_id")
	}
	if userID == "" {
		return errors.New("store: SaveAgentFile requires user_id")
	}
	now := time.Now().UTC()
	if d.dialect == mysqlDialect {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
				VALUES (?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE content=VALUES(content), updated_at=VALUES(updated_at)`,
			agentID, userID, filename, string(data), now)
		return err
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (agent_id, user_id, filename) DO UPDATE SET content=$4, updated_at=$5`,
			agentID, userID, filename, string(data), now)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (agent_id, user_id, filename) DO UPDATE SET
			  content=excluded.content, updated_at=excluded.updated_at`,
		agentID, userID, filename, string(data), now)
	return err
}

func (d *DBStore) DeleteAgentFile(ctx context.Context, agentID, userID, filename string) error {
	if agentID == "" {
		return errors.New("store: DeleteAgentFile requires agent_id")
	}
	if userID == "" {
		return errors.New("store: DeleteAgentFile requires user_id")
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agent_files WHERE agent_id = %s AND user_id = %s AND filename = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		agentID, userID, filename)
	return err
}

// ListAgentFiles 返回为 (agent_id, user_id) 存储的文件名。
// userID 是必需的——没有共享模板回退。
func (d *DBStore) ListAgentFiles(ctx context.Context, agentID, userID string) ([]string, error) {
	if agentID == "" {
		return nil, errors.New("store: ListAgentFiles requires agent_id")
	}
	if userID == "" {
		return nil, errors.New("store: ListAgentFiles requires user_id")
	}
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT filename FROM agent_files
			WHERE agent_id = %s AND user_id = %s ORDER BY filename`,
			d.ph(1), d.ph(2)),
		agentID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}
