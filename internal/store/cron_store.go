package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// --- Cron jobs ---

const cronSelectCols = `id, user_id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, failure_count, created_at`

func (d *DBStore) ListCronJobsByOwner(ctx context.Context, ownerUserID string) ([]CronJobRecord, error) {
	// user_id 已反规范化到 cron_jobs 上；与 agents 表的 JOIN 现已消失。
	// 更便宜，并且允许我们即使在 agent 行被删除的情况下也能列出用户的 cron
	//（孤行可以通过单独的清理操作清除）。
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+cronSelectCols+` FROM cron_jobs WHERE user_id = %s ORDER BY created_at`, d.ph(1)),
		ownerUserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

func (d *DBStore) ListCronJobsByAgent(ctx context.Context, agentID string) ([]CronJobRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT `+cronSelectCols+` FROM cron_jobs WHERE agent_id = %s ORDER BY created_at`, d.ph(1)),
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

func (d *DBStore) GetCronJob(ctx context.Context, jobID string) (*CronJobRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+cronSelectCols+` FROM cron_jobs WHERE id = %s`, d.ph(1)), jobID)
	var j CronJobRecord
	var lastRun, nextRun sql.NullTime
	if err := row.Scan(&j.ID, &j.UserID, &j.AgentID, &j.Name, &j.Type, &j.Schedule, &j.Message, &j.Channel, &j.ChatID, &j.AccountID, &j.Timezone, &j.Enabled, &lastRun, &nextRun, &j.FailureCount, &j.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	if lastRun.Valid {
		j.LastRun = &lastRun.Time
	}
	if nextRun.Valid {
		j.NextRun = &nextRun.Time
	}
	return &j, nil
}

func (d *DBStore) SaveCronJob(ctx context.Context, job *CronJobRecord) error {
	if job.AgentID == "" {
		return errors.New("store: cron job.agent_id is required")
	}
	// user_id 被添加以保持 cron_jobs 与代码库其余部分的 (user_id, agent_id)
	// 键控一致。当调用方未设置时，SaveCronJob 从 agents.user_id 自动填充它，
	// 因此现有调用方不必一次性修改。
	if job.UserID == "" {
		var uid sql.NullString
		row := d.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT user_id FROM agents WHERE id = %s`, d.ph(1)), job.AgentID)
		if err := row.Scan(&uid); err == nil && uid.Valid {
			job.UserID = uid.String
		}
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = time.Now().UTC()
	}
	if d.dialect == mysqlDialect {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO cron_jobs (id, user_id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  user_id=VALUES(user_id), agent_id=VALUES(agent_id), name=VALUES(name), type=VALUES(type),
				  schedule=VALUES(schedule), message=VALUES(message), channel=VALUES(channel),
				  chat_id=VALUES(chat_id), account_id=VALUES(account_id), timezone=VALUES(timezone),
				  enabled=VALUES(enabled), last_run=VALUES(last_run), next_run=VALUES(next_run)`,
			job.ID, job.UserID, job.AgentID, job.Name, job.Type, job.Schedule, job.Message, job.Channel, job.ChatID, job.AccountID, job.Timezone, job.Enabled, job.LastRun, job.NextRun, job.CreatedAt)
		return err
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO cron_jobs (id, user_id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, created_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
				ON CONFLICT (id) DO UPDATE SET
				  user_id=$2, agent_id=$3, name=$4, type=$5, schedule=$6, message=$7, channel=$8,
				  chat_id=$9, account_id=$10, timezone=$11, enabled=$12, last_run=$13, next_run=$14`,
			job.ID, job.UserID, job.AgentID, job.Name, job.Type, job.Schedule, job.Message, job.Channel, job.ChatID, job.AccountID, job.Timezone, job.Enabled, job.LastRun, job.NextRun, job.CreatedAt)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO cron_jobs (id, user_id, agent_id, name, type, schedule, message, channel, chat_id, account_id, timezone, enabled, last_run, next_run, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
			  user_id=excluded.user_id, agent_id=excluded.agent_id, name=excluded.name, type=excluded.type,
			  schedule=excluded.schedule, message=excluded.message, channel=excluded.channel,
			  chat_id=excluded.chat_id, account_id=excluded.account_id, timezone=excluded.timezone,
			  enabled=excluded.enabled, last_run=excluded.last_run, next_run=excluded.next_run`,
		job.ID, job.UserID, job.AgentID, job.Name, job.Type, job.Schedule, job.Message, job.Channel, job.ChatID, job.AccountID, job.Timezone, job.Enabled, job.LastRun, job.NextRun, job.CreatedAt)
	return err
}

func (d *DBStore) DeleteCronJob(ctx context.Context, jobID string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM cron_jobs WHERE id = %s`, d.ph(1)), jobID)
	return err
}

func (d *DBStore) GetDueCronJobs(ctx context.Context, now time.Time) ([]CronJobRecord, error) {
	var rows *sql.Rows
	var err error
	if d.dialect == "postgres" {
		rows, err = d.db.QueryContext(ctx,
			`SELECT `+cronSelectCols+` FROM cron_jobs WHERE enabled = true AND next_run <= $1 ORDER BY next_run`, now)
	} else {
		rows, err = d.db.QueryContext(ctx,
			`SELECT `+cronSelectCols+` FROM cron_jobs WHERE enabled = 1 AND next_run <= ? ORDER BY next_run`, now)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCronJobs(rows)
}

func (d *DBStore) LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error) {
	now := time.Now()
	fiveMinAgo := now.Add(-5 * time.Minute)
	var res sql.Result
	var err error
	if d.dialect == "postgres" {
		res, err = d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET locked_by=$1, locked_at=$2 WHERE id=$3 AND (locked_by IS NULL OR locked_at < $4)`,
			instanceID, now, jobID, fiveMinAgo)
	} else {
		res, err = d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET locked_by=?, locked_at=? WHERE id=? AND (locked_by IS NULL OR locked_at < ?)`,
			instanceID, now, jobID, fiveMinAgo)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (d *DBStore) UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error {
	// 成功的 tick 也会清除 failure_count——该行仅在*连续*失败运行时自动删除。
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET last_run=$1, next_run=$2, failure_count=0, locked_by=NULL, locked_at=NULL WHERE id=$3`,
			lastRun, nextRun, jobID)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`UPDATE cron_jobs SET last_run=?, next_run=?, failure_count=0, locked_by=NULL, locked_at=NULL WHERE id=?`,
		lastRun, nextRun, jobID)
	return err
}

// IncrementCronJobFailure 原子性地增加 failure_count 并返回新总数。
// 同时清除锁，以便下一个 tick 可以自由重试（或者，如果调用方决定在阈值时
// 删除该行，该行干净地消失而不会留下卡住的锁）。
func (d *DBStore) IncrementCronJobFailure(ctx context.Context, jobID string) (int, error) {
	if d.dialect == "postgres" {
		var n int
		err := d.db.QueryRowContext(ctx,
			`UPDATE cron_jobs SET failure_count = failure_count + 1, locked_by=NULL, locked_at=NULL
				WHERE id = $1 RETURNING failure_count`, jobID).Scan(&n)
		if err != nil {
			return 0, scanErr(err)
		}
		return n, nil
	}
	if _, err := d.db.ExecContext(ctx,
		`UPDATE cron_jobs SET failure_count = failure_count + 1, locked_by=NULL, locked_at=NULL WHERE id=?`,
		jobID); err != nil {
		return 0, err
	}
	var n int
	if err := d.db.QueryRowContext(ctx, `SELECT failure_count FROM cron_jobs WHERE id = ?`, jobID).Scan(&n); err != nil {
		return 0, scanErr(err)
	}
	return n, nil
}

func (d *DBStore) GetNextDueTime(ctx context.Context) (time.Time, error) {
	var q string
	if d.dialect != "sqlite" {
		// 服务器数据库返回正确的时间戳；sql.NullTime 有效。
		q = `SELECT MIN(next_run) FROM cron_jobs WHERE enabled = true AND next_run IS NOT NULL`
		var t sql.NullTime
		if err := d.db.QueryRowContext(ctx, q).Scan(&t); err != nil {
			return time.Time{}, err
		}
		if !t.Valid {
			return time.Time{}, nil
		}
		return t.Time, nil
	}
	// SQLite 将 MIN() 作为字符串返回——扫描到 NullString 中，然后解析。
	q = `SELECT MIN(next_run) FROM cron_jobs WHERE enabled = 1 AND next_run IS NOT NULL`
	var s sql.NullString
	if err := d.db.QueryRowContext(ctx, q).Scan(&s); err != nil {
		return time.Time{}, err
	}
	if !s.Valid || s.String == "" {
		return time.Time{}, nil
	}
	return parseTimeString(s.String), nil
}

// parseTimeString 尝试 modernc.org/sqlite 可能为 TIMESTAMP 列生成的常见时间格式
// （RFC3339, RFC3339Nano 以及旧代码路径写入的 Go 默认格式）。
func parseTimeString(s string) time.Time {
	for _, layout := range []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func scanCronJobs(rows *sql.Rows) ([]CronJobRecord, error) {
	var jobs []CronJobRecord
	for rows.Next() {
		var j CronJobRecord
		var lastRun, nextRun sql.NullTime
		if err := rows.Scan(&j.ID, &j.UserID, &j.AgentID, &j.Name, &j.Type, &j.Schedule, &j.Message, &j.Channel, &j.ChatID, &j.AccountID, &j.Timezone, &j.Enabled, &lastRun, &nextRun, &j.FailureCount, &j.CreatedAt); err != nil {
			return nil, err
		}
		if lastRun.Valid {
			j.LastRun = &lastRun.Time
		}
		if nextRun.Valid {
			j.NextRun = &nextRun.Time
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
