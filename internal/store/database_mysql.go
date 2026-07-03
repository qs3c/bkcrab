package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
)

const mysqlDialect = "mysql"

// normalizeMySQLDSN 使驱动程序的时间处理与 store 的其余部分一致。
// 调用方仍可以提供 TLS、超时和其他驱动程序参数。
func normalizeMySQLDSN(dsn string) (string, error) {
	cfg, err := mysqlDriver.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("invalid MySQL DSN: %w", err)
	}
	if cfg.DBName == "" {
		return "", errors.New("invalid MySQL DSN: database name is required")
	}
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	if cfg.Collation == "" {
		cfg.Collation = "utf8mb4_unicode_ci"
	}
	return cfg.FormatDSN(), nil
}

func isMySQLDuplicateIndex(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1061
}

func isMySQLDuplicateKey(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

// execDDL 处理 MySQL 缺少 CREATE INDEX IF NOT EXISTS 的情况。
// 所有其他语句保持不变地传递。
func (d *DBStore) execDDL(ctx context.Context, stmt string) error {
	if d.dialect != mysqlDialect {
		_, err := d.db.ExecContext(ctx, stmt)
		return err
	}
	normalized := strings.Replace(stmt, "CREATE UNIQUE INDEX IF NOT EXISTS", "CREATE UNIQUE INDEX", 1)
	normalized = strings.Replace(normalized, "CREATE INDEX IF NOT EXISTS", "CREATE INDEX", 1)
	_, err := d.db.ExecContext(ctx, normalized)
	if isMySQLDuplicateIndex(err) {
		return nil
	}
	return err
}

func mysqlTokenUsageTableSQL() string {
	return `CREATE TABLE IF NOT EXISTS token_usage_daily (
		day DATE NOT NULL,
		user_id VARCHAR(120) NOT NULL DEFAULT '',
		agent_id VARCHAR(120) NOT NULL DEFAULT '',
		session_key VARCHAR(191) NOT NULL DEFAULT '',
		provider VARCHAR(120) NOT NULL DEFAULT '',
		model VARCHAR(191) NOT NULL DEFAULT '',
		input_tokens BIGINT NOT NULL DEFAULT 0,
		output_tokens BIGINT NOT NULL DEFAULT 0,
		cache_read_tokens BIGINT NOT NULL DEFAULT 0,
		cache_create_tokens BIGINT NOT NULL DEFAULT 0,
		request_count BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (day, user_id, agent_id, session_key, provider, model)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`
}

// mysqlMigrationSQL 是有意明确写出的。PostgreSQL 和 SQLite 允许在键中使用
// TEXT 列；InnoDB 要求键列有长度限制，并且在 utf8mb4 下有 3072 字节的
// 复合键限制。
func mysqlMigrationSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS users (
			id VARCHAR(120) PRIMARY KEY,
			username VARCHAR(191) NOT NULL UNIQUE,
			email VARCHAR(191) NOT NULL UNIQUE,
			password_hash VARCHAR(255) NOT NULL DEFAULT '',
			display_name VARCHAR(191) NOT NULL DEFAULT '',
			role VARCHAR(32) NOT NULL DEFAULT 'user',
			status VARCHAR(32) NOT NULL DEFAULT 'active',
			apikey_id VARCHAR(120) NOT NULL DEFAULT '',
			external_id VARCHAR(120) NOT NULL DEFAULT '',
			app_user_key VARCHAR(255) GENERATED ALWAYS AS (
				CASE
					WHEN apikey_id <> '' AND external_id <> ''
					THEN CONCAT(apikey_id, ':', external_id)
					ELSE NULL
				END
			) STORED,
			avatar_url LONGTEXT NOT NULL,
			agent_quota INTEGER NOT NULL DEFAULT -1,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS web_sessions (
			sid VARCHAR(191) PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			expires_at DATETIME(6) NOT NULL,
			KEY idx_web_sessions_user (user_id),
			KEY idx_web_sessions_expires (expires_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS apikeys (
			id VARCHAR(120) PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL,
			name VARCHAR(191) NOT NULL DEFAULT '',
			key_hash VARCHAR(255) NOT NULL,
			key_prefix VARCHAR(64) NOT NULL DEFAULT '',
			type VARCHAR(32) NOT NULL DEFAULT 'agent',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			KEY idx_apikeys_user (user_id),
			KEY idx_apikeys_key_hash (key_hash)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS apikey_agents (
			apikey_id VARCHAR(120) NOT NULL,
			agent_id VARCHAR(120) NOT NULL,
			PRIMARY KEY (apikey_id, agent_id),
			KEY idx_apikey_agents_agent (agent_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS agents (
			id VARCHAR(120) PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL,
			name VARCHAR(191) NOT NULL DEFAULT '',
			config LONGTEXT NOT NULL,
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			KEY idx_agents_user (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS sessions (
			user_id VARCHAR(120) NOT NULL,
			agent_id VARCHAR(120) NOT NULL,
			session_key VARCHAR(191) NOT NULL,
			channel VARCHAR(64) NOT NULL DEFAULT '',
			account_id VARCHAR(191) NOT NULL DEFAULT '',
			chat_id VARCHAR(191) NOT NULL DEFAULT '',
			project_id VARCHAR(120) NOT NULL DEFAULT '',
			title VARCHAR(512) NOT NULL DEFAULT '',
			messages LONGTEXT NOT NULL,
			message_count INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			chatter_user_id VARCHAR(120) NOT NULL DEFAULT '',
			PRIMARY KEY (user_id, agent_id, session_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS session_messages (
			user_id VARCHAR(120) NOT NULL,
			agent_id VARCHAR(120) NOT NULL,
			session_key VARCHAR(191) NOT NULL,
			seq INTEGER NOT NULL,
			role VARCHAR(32) NOT NULL,
			content LONGTEXT NOT NULL,
			content_parts LONGTEXT NOT NULL,
			tool_calls LONGTEXT NOT NULL,
			tool_call_id VARCHAR(191) NOT NULL DEFAULT '',
			name VARCHAR(191) NOT NULL DEFAULT '',
			metadata LONGTEXT NOT NULL,
			thinking LONGTEXT NOT NULL,
			raw_assistant LONGTEXT NOT NULL,
			origin VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			chatter_user_id VARCHAR(120) NOT NULL DEFAULT '',
			turn_status VARCHAR(16) NOT NULL DEFAULT '',
			extraction_id VARCHAR(64) NULL,
			tool_call_count INT NOT NULL DEFAULT 0,
			skill_extraction_id VARCHAR(64) NULL,
			PRIMARY KEY (user_id, agent_id, session_key, seq)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS session_events (
			user_id VARCHAR(120) NOT NULL,
			agent_id VARCHAR(120) NOT NULL,
			session_key VARCHAR(191) NOT NULL,
			seq INTEGER NOT NULL,
			type VARCHAR(64) NOT NULL,
			data LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			chatter_user_id VARCHAR(120) NOT NULL DEFAULT '',
			PRIMARY KEY (user_id, agent_id, session_key, seq)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS context_archives (
			user_id VARCHAR(120) NOT NULL DEFAULT '',
			agent_id VARCHAR(120) NOT NULL,
			session_key VARCHAR(191) NOT NULL,
			id VARCHAR(120) NOT NULL,
			tool_call_id VARCHAR(191) NOT NULL DEFAULT '',
			tool_name VARCHAR(191) NOT NULL DEFAULT '',
			content LONGTEXT NOT NULL,
			content_bytes BIGINT NOT NULL DEFAULT 0,
			content_sha256 VARCHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (agent_id, session_key, id),
			KEY idx_context_archives_user (user_id, agent_id, session_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS agent_files (
			agent_id VARCHAR(120) NOT NULL,
			user_id VARCHAR(120) NOT NULL DEFAULT '',
			filename VARCHAR(255) NOT NULL,
			content LONGTEXT NOT NULL,
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (agent_id, user_id, filename)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS configs (
			id VARCHAR(120) PRIMARY KEY,
			kind VARCHAR(64) NOT NULL,
			scope VARCHAR(32) NOT NULL DEFAULT '',
			user_id VARCHAR(120) NOT NULL DEFAULT '',
			agent_id VARCHAR(120) NOT NULL DEFAULT '',
			name VARCHAR(191) NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key VARCHAR(191) NOT NULL DEFAULT '',
			data LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY configs_kind_user_agent_name_key (kind, user_id, agent_id, name),
			KEY idx_configs_credential (kind, credential_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS cron_jobs (
			id VARCHAR(120) PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL DEFAULT '',
			agent_id VARCHAR(120) NOT NULL,
			name VARCHAR(191) NOT NULL DEFAULT '',
			type VARCHAR(32) NOT NULL DEFAULT 'cron',
			schedule VARCHAR(255) NOT NULL,
			message LONGTEXT NOT NULL,
			channel VARCHAR(64) NOT NULL,
			chat_id VARCHAR(191) NOT NULL,
			account_id VARCHAR(191) NOT NULL DEFAULT '',
			timezone VARCHAR(64) NOT NULL DEFAULT 'UTC',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run DATETIME(6),
			next_run DATETIME(6),
			locked_by VARCHAR(120),
			locked_at DATETIME(6),
			failure_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			KEY idx_cron_jobs_schedule (enabled, next_run),
			KEY idx_cron_jobs_agent (agent_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS projects (
			user_id VARCHAR(120) NOT NULL,
			agent_id VARCHAR(120) NOT NULL,
			project_id VARCHAR(120) NOT NULL,
			name VARCHAR(191) NOT NULL DEFAULT '',
			description LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (user_id, agent_id, project_id),
			KEY idx_projects_listing (user_id, agent_id, updated_at DESC)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS agent_goals (
			id VARCHAR(120) PRIMARY KEY,
			agent_id VARCHAR(120) NOT NULL,
			session_key VARCHAR(191) NOT NULL,
			owner_user_id VARCHAR(120) NOT NULL,
			channel VARCHAR(64) NOT NULL DEFAULT '',
			account_id VARCHAR(191) NOT NULL DEFAULT '',
			chat_id VARCHAR(191) NOT NULL DEFAULT '',
			project_id VARCHAR(120) NOT NULL DEFAULT '',
			objective LONGTEXT NOT NULL,
			status VARCHAR(32) NOT NULL DEFAULT 'active',
			token_budget BIGINT,
			tokens_used BIGINT NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY idx_agent_goals_session (agent_id, session_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		mysqlTokenUsageTableSQL(),
		`CREATE TABLE IF NOT EXISTS channel_leases (
			channel VARCHAR(64) NOT NULL,
			account_id VARCHAR(191) NOT NULL,
			holder_id VARCHAR(120) NOT NULL,
			expires_at DATETIME(6) NOT NULL,
			PRIMARY KEY (channel, account_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	}
}

func (d *DBStore) migrateConfigsScopeToUserAgentMySQL(ctx context.Context) error {
	stmts := []string{
		`DROP TABLE IF EXISTS configs_new`,
		`CREATE TABLE configs_new (
			id VARCHAR(120) PRIMARY KEY,
			kind VARCHAR(64) NOT NULL,
			scope VARCHAR(32) NOT NULL DEFAULT '',
			user_id VARCHAR(120) NOT NULL DEFAULT '',
			agent_id VARCHAR(120) NOT NULL DEFAULT '',
			name VARCHAR(191) NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key VARCHAR(191) NOT NULL DEFAULT '',
			data LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			UNIQUE KEY configs_kind_user_agent_name_key (kind, user_id, agent_id, name),
			KEY idx_configs_credential (kind, credential_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`INSERT INTO configs_new
			(id, kind, scope, user_id, agent_id, name, enabled, credential_key, data, created_at, updated_at)
		 SELECT
			c.id,
			c.kind,
			CASE
				WHEN c.scope = 'user' THEN 'user'
				WHEN c.scope = 'agent' AND
					(c.kind = 'channel' OR (c.kind = 'setting' AND c.name = 'bindings'))
					THEN 'user-agent'
				WHEN c.scope = 'agent' THEN 'agent'
				ELSE 'system'
			END,
			CASE
				WHEN c.scope = 'user' THEN c.scope_id
				WHEN c.scope = 'agent' AND
					(c.kind = 'channel' OR (c.kind = 'setting' AND c.name = 'bindings'))
					THEN COALESCE(a.user_id, '')
				ELSE ''
			END,
			CASE WHEN c.scope = 'agent' THEN c.scope_id ELSE '' END,
			c.name, c.enabled, c.credential_key, c.data, c.created_at, c.updated_at
		 FROM configs c
		 LEFT JOIN agents a ON a.id = c.scope_id
		 WHERE NOT (c.kind = 'setting' AND c.name = 'bindings')`,
		`DROP TABLE configs`,
		`RENAME TABLE configs_new TO configs`,
	}
	for _, stmt := range stmts {
		if _, err := d.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("mysql migrate configs: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

func (d *DBStore) ensureMySQLAppUserKey(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "users", "app_user_key")
	if err != nil {
		return err
	}
	if !has {
		if _, err := d.db.ExecContext(ctx, `ALTER TABLE users
			ADD COLUMN app_user_key VARCHAR(255) GENERATED ALWAYS AS (
				CASE
					WHEN apikey_id <> '' AND external_id <> ''
					THEN CONCAT(apikey_id, ':', external_id)
					ELSE NULL
				END
			) STORED`); err != nil {
			return fmt.Errorf("add users.app_user_key: %w", err)
		}
	}
	if err := d.execDDL(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_apikey_external ON users (app_user_key)`); err != nil {
		return fmt.Errorf("create idx_users_apikey_external: %w", err)
	}
	return nil
}
