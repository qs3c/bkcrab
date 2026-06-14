package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// --- Agent 目标 ---
//
// 所有目标列在每个 CRUD 路径上一起操作——变更通过"读取、变异领域对象、写回"
// 而非部分更新进行。将每轮记账逻辑保持在 Go 中，而不是分散在 UPDATE … SET 片段中。
//
// 旧列（last_accounted_token_usage / time_used_seconds /
// last_accounted_at / safety_max_iterations / iterations）仍然存在于旧的
// SQLite 数据库上——它们不在当前的 CREATE TABLE 中，下面的 SQL 既不读取也不写入它们。
const goalSelectCols = `id, agent_id, session_key, owner_user_id, channel, account_id, chat_id, project_id, objective, status, token_budget, tokens_used, created_at, updated_at`

func (d *DBStore) CreateGoal(ctx context.Context, g *GoalRecord) error {
	if g.AgentID == "" || g.SessionKey == "" {
		return errors.New("store: goal.agent_id and session_key are required")
	}
	if g.OwnerUserID == "" {
		return errors.New("store: goal.owner_user_id is required")
	}
	now := time.Now().UTC()
	if g.CreatedAt.IsZero() {
		g.CreatedAt = now
	}
	if g.UpdatedAt.IsZero() {
		g.UpdatedAt = now
	}
	if g.Status == "" {
		g.Status = "active"
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO agent_goals (id, agent_id, session_key, owner_user_id, channel, account_id, chat_id, project_id, objective, status, token_budget, tokens_used, created_at, updated_at)
			VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14)),
		g.ID, g.AgentID, g.SessionKey, g.OwnerUserID,
		g.Channel, g.AccountID, g.ChatID, g.ProjectID,
		g.Objective, g.Status,
		g.TokenBudget, g.TokensUsed, g.CreatedAt, g.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrGoalAlreadyExists
		}
		return err
	}
	return nil
}

func (d *DBStore) GetGoalBySession(ctx context.Context, agentID, sessionKey string) (*GoalRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+goalSelectCols+` FROM agent_goals WHERE agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2)),
		agentID, sessionKey)
	return scanGoal(row)
}

func (d *DBStore) UpdateGoal(ctx context.Context, g *GoalRecord) error {
	if g.ID == "" {
		return errors.New("store: goal.id is required for UpdateGoal")
	}
	g.UpdatedAt = time.Now().UTC()
	res, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE agent_goals
			SET status = %s, token_budget = %s, tokens_used = %s, updated_at = %s
			WHERE id = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		g.Status, g.TokenBudget, g.TokensUsed, g.UpdatedAt, g.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DBStore) DeleteGoal(ctx context.Context, goalID string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM agent_goals WHERE id = %s`, d.ph(1)), goalID)
	return err
}

// scanGoal 从 QueryRow 读取一行到 GoalRecord。当查询无匹配时返回
// ErrNotFound（通过 scanErr）。
func scanGoal(row *sql.Row) (*GoalRecord, error) {
	var g GoalRecord
	var tokenBudget sql.NullInt64
	if err := row.Scan(&g.ID, &g.AgentID, &g.SessionKey, &g.OwnerUserID,
		&g.Channel, &g.AccountID, &g.ChatID, &g.ProjectID,
		&g.Objective, &g.Status,
		&tokenBudget, &g.TokensUsed, &g.CreatedAt, &g.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	if tokenBudget.Valid {
		g.TokenBudget = &tokenBudget.Int64
	}
	return &g, nil
}

// isUniqueViolation 报告 err 是否是 Postgres（SQLSTATE 23505）或 SQLite
// （子串 "UNIQUE constraint failed"）中的 UNIQUE 约束违规。两个驱动程序在错误文本中
// 暴露了足够的细节来识别这一点，而无需导入驱动程序包。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	if isMySQLDuplicateKey(err) {
		return true
	}
	msg := err.Error()
	// Postgres lib/pq 显示 "pq: duplicate key value violates unique constraint"
	if strings.Contains(msg, "duplicate key value") {
		return true
	}
	// modernc.org/sqlite 报告 "UNIQUE constraint failed: <table>.<col>"
	if strings.Contains(msg, "UNIQUE constraint failed") {
		return true
	}
	return false
}
