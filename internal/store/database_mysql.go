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
	// Fenced idempotent updates (for example repeated progress values) must
	// still report the matched row. Otherwise MySQL's changed-row semantics can
	// be mistaken for a lost lease fence.
	cfg.ClientFoundRows = true
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
		`CREATE TABLE IF NOT EXISTS skill_usage (
			agent_id VARCHAR(120) NOT NULL,
			slug VARCHAR(64) NOT NULL,
			origin VARCHAR(16) NOT NULL DEFAULT 'learner',
			activity DOUBLE NOT NULL DEFAULT 0,
			last_load_seq BIGINT NOT NULL DEFAULT 0,
			total_loads BIGINT NOT NULL DEFAULT 0,
			explicit_uses BIGINT NOT NULL DEFAULT 0,
			created_seq BIGINT NOT NULL DEFAULT 0,
			edited_seq BIGINT NOT NULL DEFAULT 0,
			content_hash CHAR(64) NOT NULL DEFAULT '',
			created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
			PRIMARY KEY (agent_id, slug)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_kbs (
			id VARCHAR(120) PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL,
			name VARCHAR(191) NOT NULL,
			description LONGTEXT NOT NULL,
			embed_provider VARCHAR(64) NOT NULL DEFAULT 'system',
			embed_model VARCHAR(191) NOT NULL,
			embed_dims INTEGER NOT NULL,
			chunk_size INTEGER NOT NULL DEFAULT 512,
			chunk_overlap INTEGER NOT NULL DEFAULT 64,
			parse_mode VARCHAR(16) NOT NULL DEFAULT 'standard',
			enrichment_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			status VARCHAR(32) NOT NULL DEFAULT 'active',
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			KEY idx_rag_kbs_user (user_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_chat_turns (
			id VARCHAR(120) PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL,
			kb_id VARCHAR(120) NOT NULL,
			session_id VARCHAR(120) NOT NULL,
			title VARCHAR(255) NOT NULL DEFAULT '',
			question LONGTEXT NOT NULL,
			answer LONGTEXT NOT NULL,
			sources LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL,
			KEY idx_rag_chat_sessions (user_id, kb_id, created_at),
			KEY idx_rag_chat_turns_session (user_id, kb_id, session_id, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_documents (
			id VARCHAR(120) PRIMARY KEY,
			kb_id VARCHAR(120) NOT NULL,
			file_name VARCHAR(255) NOT NULL,
			file_type VARCHAR(32) NOT NULL,
			file_size BIGINT NOT NULL DEFAULT 0,
			object_key TEXT NOT NULL,
			status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
			error_msg LONGTEXT NOT NULL,
			chunk_count INTEGER NOT NULL DEFAULT 0,
			token_count INTEGER NOT NULL DEFAULT 0,
			version BIGINT NOT NULL DEFAULT 1,
			source_sha256 CHAR(64) NOT NULL DEFAULT '',
			active_version BIGINT NOT NULL DEFAULT 0,
			index_format_version SMALLINT NOT NULL DEFAULT 1,
			processing_stage VARCHAR(24) NOT NULL DEFAULT 'queued',
			progress_current INTEGER NOT NULL DEFAULT 0,
			progress_total INTEGER NOT NULL DEFAULT 0,
			progress_unit VARCHAR(16) NOT NULL DEFAULT '',
			degraded BOOLEAN NOT NULL DEFAULT FALSE,
			warning_count INTEGER NOT NULL DEFAULT 0,
			uploaded_at DATETIME(6) NOT NULL,
			indexed_at DATETIME(6),
			KEY idx_rag_documents_kb (kb_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_index_tasks (
			id BIGINT NOT NULL AUTO_INCREMENT,
			doc_id VARCHAR(120) NOT NULL,
			doc_version BIGINT NOT NULL,
			status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
			retry_count INTEGER NOT NULL DEFAULT 0,
			max_retry INTEGER NOT NULL DEFAULT 3,
			claim_generation BIGINT NOT NULL DEFAULT 0,
			lease_owner VARCHAR(96) NOT NULL DEFAULT '',
			lease_until DATETIME(6),
			heartbeat_at DATETIME(6),
			next_run_at DATETIME(6),
			error_msg LONGTEXT NOT NULL,
			created_at DATETIME(6) NOT NULL,
			started_at DATETIME(6),
			finished_at DATETIME(6),
			PRIMARY KEY (id),
			UNIQUE KEY uq_rag_index_tasks_doc_version (doc_id, doc_version),
			KEY idx_rag_tasks_status (status, created_at),
			KEY idx_rag_index_tasks_runnable (status, next_run_at, lease_until, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_document_versions (
			doc_id VARCHAR(120) NOT NULL,
			doc_version BIGINT NOT NULL,
			status VARCHAR(20) NOT NULL,
			source_sha256 CHAR(64) NOT NULL,
			parse_mode VARCHAR(16) NOT NULL,
			chunk_size INTEGER NOT NULL,
			chunk_overlap INTEGER NOT NULL,
			parser_version VARCHAR(64) NOT NULL,
			splitter_version VARCHAR(64) NOT NULL,
			parse_fingerprint CHAR(64) NOT NULL,
			index_fingerprint CHAR(64) NOT NULL,
			vision_model VARCHAR(128) NOT NULL,
			vision_provider_fingerprint CHAR(64) NOT NULL,
			vision_prompt_version VARCHAR(64) NOT NULL,
			text_model VARCHAR(128) NOT NULL,
			text_provider_fingerprint CHAR(64) NOT NULL,
			enrichment_prompt_version VARCHAR(64) NOT NULL,
			enrichment_enabled BOOLEAN NOT NULL,
			max_document_ai_requests INTEGER NOT NULL,
			max_document_ai_tokens BIGINT NOT NULL,
			max_document_ai_cost_microusd BIGINT NOT NULL,
			embedding_provider VARCHAR(64) NOT NULL,
			embedding_model VARCHAR(128) NOT NULL,
			embedding_dimensions INTEGER NOT NULL,
			embedding_contract_fingerprint CHAR(64) NOT NULL,
			parse_artifact_key LONGTEXT NOT NULL,
			page_count INTEGER NOT NULL DEFAULT 0,
			asset_count INTEGER NOT NULL DEFAULT 0,
			degraded BOOLEAN NOT NULL DEFAULT FALSE,
			warning_count INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			PRIMARY KEY (doc_id, doc_version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_assets (
			id VARCHAR(40) PRIMARY KEY,
			doc_id VARCHAR(120) NOT NULL,
			content_sha256 CHAR(64) NOT NULL,
			source_kind VARCHAR(24) NOT NULL,
			source_mime VARCHAR(96) NOT NULL,
			display_mime VARCHAR(96) NOT NULL,
			source_object_key LONGTEXT NOT NULL,
			display_object_key LONGTEXT NOT NULL,
			thumbnail_object_key LONGTEXT NOT NULL,
			display_status VARCHAR(16) NOT NULL,
			display_sha256 CHAR(64) NOT NULL,
			thumbnail_sha256 CHAR(64) NOT NULL,
			byte_size BIGINT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			first_seen_version BIGINT NOT NULL,
			last_seen_version BIGINT NOT NULL,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			UNIQUE KEY uq_rag_assets_doc_hash (doc_id, content_sha256),
			KEY idx_rag_assets_doc (doc_id)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_chunks (
			kb_id VARCHAR(120) NOT NULL,
			doc_id VARCHAR(120) NOT NULL,
			doc_version BIGINT NOT NULL,
			chunk_index INTEGER NOT NULL,
			section_title LONGTEXT NOT NULL,
			location_json LONGTEXT NOT NULL,
			raw_content LONGTEXT NOT NULL,
			enhancement LONGTEXT NOT NULL,
			search_content LONGTEXT NOT NULL,
			token_count INTEGER NOT NULL,
			created_at DATETIME(6) NOT NULL,
			PRIMARY KEY (doc_id, doc_version, chunk_index),
			KEY idx_rag_chunks_lookup (kb_id, doc_id, doc_version)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_chunk_assets (
			doc_id VARCHAR(120) NOT NULL,
			doc_version BIGINT NOT NULL,
			chunk_index INTEGER NOT NULL,
			asset_id VARCHAR(40) NOT NULL,
			ordinal INTEGER NOT NULL,
			location_json LONGTEXT NOT NULL,
			caption LONGTEXT NOT NULL,
			ocr_text LONGTEXT NOT NULL,
			PRIMARY KEY (doc_id, doc_version, chunk_index, asset_id, ordinal),
			KEY idx_rag_chunk_assets_lookup (doc_id, doc_version, chunk_index, ordinal)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_index_gc_tasks (
			id BIGINT NOT NULL AUTO_INCREMENT,
			doc_id VARCHAR(120) NOT NULL,
			retired_version BIGINT NOT NULL,
			retired_at DATETIME(6) NOT NULL,
			not_before DATETIME(6) NOT NULL,
			status VARCHAR(16) NOT NULL,
			claim_generation BIGINT NOT NULL DEFAULT 0,
			lease_owner VARCHAR(96) NOT NULL DEFAULT '',
			lease_until DATETIME(6),
			heartbeat_at DATETIME(6),
			attempt_count INTEGER NOT NULL DEFAULT 0,
			next_run_at DATETIME(6),
			created_at DATETIME(6) NOT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY uq_rag_index_gc_doc_version (doc_id, retired_version),
			KEY idx_rag_index_gc_tasks_runnable (status, next_run_at, lease_until, created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_document_ai_task_budgets (
			task_id BIGINT PRIMARY KEY,
			user_id VARCHAR(120) NOT NULL,
			max_requests BIGINT NOT NULL,
			max_tokens BIGINT NOT NULL,
			max_cost_microusd BIGINT NOT NULL,
			charged_requests BIGINT NOT NULL DEFAULT 0,
			charged_tokens BIGINT NOT NULL DEFAULT 0,
			charged_cost_microusd BIGINT NOT NULL DEFAULT 0,
			updated_at DATETIME(6) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_document_ai_user_budgets (
			user_id VARCHAR(120) NOT NULL,
			period_start_utc DATE NOT NULL,
			charged_requests BIGINT NOT NULL DEFAULT 0,
			charged_tokens BIGINT NOT NULL DEFAULT 0,
			charged_cost_microusd BIGINT NOT NULL DEFAULT 0,
			updated_at DATETIME(6) NOT NULL,
			PRIMARY KEY (user_id, period_start_utc)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
		`CREATE TABLE IF NOT EXISTS rag_document_ai_usage (
			idempotency_key CHAR(64) PRIMARY KEY,
			logical_request_key CHAR(64) NOT NULL,
			user_id VARCHAR(120) NOT NULL,
			doc_id VARCHAR(120) NOT NULL,
			task_id BIGINT NOT NULL,
			doc_version BIGINT NOT NULL,
			claim_generation BIGINT NOT NULL,
			lease_owner VARCHAR(96) NOT NULL,
			operation VARCHAR(24) NOT NULL,
			provider_fingerprint CHAR(64) NOT NULL,
			period_start_utc DATE NOT NULL,
			reserved_input_tokens BIGINT NOT NULL,
			reserved_output_tokens BIGINT NOT NULL,
			actual_input_tokens BIGINT NOT NULL DEFAULT 0,
			actual_output_tokens BIGINT NOT NULL DEFAULT 0,
			estimated_cost_microusd BIGINT NOT NULL,
			state VARCHAR(16) NOT NULL,
			reservation_expires_at DATETIME(6),
			sent_at DATETIME(6),
			usage_estimated BOOLEAN NOT NULL DEFAULT FALSE,
			created_at DATETIME(6) NOT NULL,
			updated_at DATETIME(6) NOT NULL,
			KEY idx_rag_document_ai_usage_user_period (user_id, period_start_utc, provider_fingerprint),
			KEY idx_rag_document_ai_usage_task_logical (task_id, logical_request_key)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
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
