package store

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// --- 范围配置（providers + channels + settings）---

// ListConfigs 返回给定 (kind, scope) 元组的所有行。当 scopeID 为空时，
// 它匹配范围内的任何 scope_id——被启动时枚举路径（registerChannelsFromStore）
// 使用，这些路径想要所有用户中"每个 agent 的频道"而无需先枚举用户。
// 传递真实 scopeID 的现有调用方继续获得精确匹配语义。系统行无论如何都有
// scope_id=""，因此系统范围查询不受此放宽的影响。
const configSelectCols = `id, kind, scope, user_id, agent_id, name, enabled, credential_key, data, created_at, updated_at`

func (d *DBStore) ListConfigs(ctx context.Context, kind, userID, agentID string) ([]ConfigRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+configSelectCols+`
			FROM configs WHERE kind = %s AND user_id = %s AND agent_id = %s ORDER BY name`,
			d.ph(1), d.ph(2), d.ph(3)),
		kind, userID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConfigs(rows)
}

func (d *DBStore) ListConfigsByUser(ctx context.Context, kind, userID string) ([]ConfigRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+configSelectCols+`
			FROM configs WHERE kind = %s AND user_id = %s ORDER BY agent_id, name`,
			d.ph(1), d.ph(2)),
		kind, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConfigs(rows)
}

func (d *DBStore) QueryAllConfigs(ctx context.Context, kind string) ([]ConfigRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+configSelectCols+`
			FROM configs WHERE kind = %s ORDER BY user_id, agent_id, name`,
			d.ph(1)),
		kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConfigs(rows)
}

func (d *DBStore) GetConfig(ctx context.Context, id string) (*ConfigRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+configSelectCols+` FROM configs WHERE id = %s`, d.ph(1)), id)
	return scanConfigRow(row)
}

func (d *DBStore) GetConfigByName(ctx context.Context, kind, userID, agentID, name string) (*ConfigRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+configSelectCols+`
			FROM configs WHERE kind = %s AND user_id = %s AND agent_id = %s AND name = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		kind, userID, agentID, name)
	return scanConfigRow(row)
}

func (d *DBStore) SaveConfig(ctx context.Context, c *ConfigRecord) error {
	if c.Kind == "" || c.Name == "" {
		return errors.New("store: SaveConfig requires kind and name")
	}
	// scope 是从 (user_id, agent_id) 反规范化而来。SaveConfig 是唯一的写入者——
	// 在每次 upsert 时重新计算，以便调用方提供的过时值不会破坏该列。
	// 数据库转储可读性的保证依赖于这个不变量。
	c.Scope = computeConfigScope(c.UserID, c.AgentID)
	now := time.Now().UTC()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.ID == "" {
		// 随机 id；(kind, user_id, agent_id, name) 唯一索引保证了下面的幂等性。
		// 我们过去从这些列的哈希派生 id，但列重命名（scope/scope_id →
		// user_id/agent_id）改变了同一逻辑行的哈希，使旧 id 和新 id 产生差异。
		// 在自然键上 upserting 完全绕过了这个混乱。
		c.ID = randomConfigID()
	}
	dataBytes, _ := json.Marshal(c.Data)
	if d.dialect == mysqlDialect {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO configs (id, kind, scope, user_id, agent_id, name, enabled, credential_key, data, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  scope=VALUES(scope), enabled=VALUES(enabled), credential_key=VALUES(credential_key),
				  data=VALUES(data), updated_at=VALUES(updated_at)`,
			c.ID, c.Kind, c.Scope, c.UserID, c.AgentID, c.Name, c.Enabled, c.CredentialKey, string(dataBytes), c.CreatedAt, c.UpdatedAt)
		return err
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO configs (id, kind, scope, user_id, agent_id, name, enabled, credential_key, data, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				ON CONFLICT (kind, user_id, agent_id, name) DO UPDATE SET
				  scope=$3, enabled=$7, credential_key=$8, data=$9, updated_at=$11`,
			c.ID, c.Kind, c.Scope, c.UserID, c.AgentID, c.Name, c.Enabled, c.CredentialKey, string(dataBytes), c.CreatedAt, c.UpdatedAt)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO configs (id, kind, scope, user_id, agent_id, name, enabled, credential_key, data, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (kind, user_id, agent_id, name) DO UPDATE SET
			  scope=excluded.scope, enabled=excluded.enabled, credential_key=excluded.credential_key,
			  data=excluded.data, updated_at=excluded.updated_at`,
		c.ID, c.Kind, c.Scope, c.UserID, c.AgentID, c.Name, c.Enabled, c.CredentialKey, string(dataBytes), c.CreatedAt, c.UpdatedAt)
	return err
}

// randomConfigID 为新的 configs 行生成一个不透明 id。格式匹配历史十六进制派生形状，
// 因此任何在日志/仪表板中依赖 `sc_` 前缀的东西都能继续识别它。
func randomConfigID() string {
	var b [10]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		// 回退到时间派生字节——此处的冲突没问题，自然键 upsert 才是强制执行唯一性的机制。
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i * 8))
		}
	}
	return "sc_" + hex.EncodeToString(b[:])
}

func (d *DBStore) DeleteConfig(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM configs WHERE id = %s`, d.ph(1)), id)
	return err
}

func (d *DBStore) LookupChannelByCredential(ctx context.Context, channelType, credKey string) (*ConfigRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+configSelectCols+`
			FROM configs WHERE kind = 'channel' AND name = %s AND credential_key = %s LIMIT 1`,
			d.ph(1), d.ph(2)),
		channelType, credKey)
	return scanConfigRow(row)
}

// configRowID 为 (kind, scope, scope_id, name) 元组生成稳定的 id。
// 被在旧列布局下写入行的遗留迁移（migrateAgentsDropModel,
// migrateSkillsAgentEntriesSplit）使用——那些调用方从遗留四元组计算 ID，
// 我们保留此函数以便历史 id 保持可重现。新调用方改为通过 SaveConfig + 自然键 upsert。
func configRowID(kind, scope, scopeID, name string) string {
	h := sha256.New()
	h.Write([]byte(kind))
	h.Write([]byte{0})
	h.Write([]byte(scope))
	h.Write([]byte{0})
	h.Write([]byte(scopeID))
	h.Write([]byte{0})
	h.Write([]byte(name))
	return "sc_" + hex.EncodeToString(h.Sum(nil)[:10])
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConfigRow(row rowScanner) (*ConfigRecord, error) {
	var c ConfigRecord
	var dataStr string
	if err := row.Scan(&c.ID, &c.Kind, &c.Scope, &c.UserID, &c.AgentID, &c.Name, &c.Enabled, &c.CredentialKey, &dataStr, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	json.Unmarshal([]byte(dataStr), &c.Data)
	return &c, nil
}

func scanConfigs(rows *sql.Rows) ([]ConfigRecord, error) {
	var out []ConfigRecord
	for rows.Next() {
		var c ConfigRecord
		var dataStr string
		if err := rows.Scan(&c.ID, &c.Kind, &c.Scope, &c.UserID, &c.AgentID, &c.Name, &c.Enabled, &c.CredentialKey, &dataStr, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(dataStr), &c.Data)
		out = append(out, c)
	}
	return out, rows.Err()
}
