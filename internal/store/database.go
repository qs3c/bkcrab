package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"  // PostgreSQL 驱动程序
	_ "modernc.org/sqlite" // SQLite 驱动程序（纯 Go）
)

// DBStore 使用 SQL 数据库实现 Store。
type DBStore struct {
	db      *sql.DB
	dialect string // "mysql"、"postgres" 或 "sqlite"
}

// NewDBStore 创建一个数据库支持的 store。
func NewDBStore(dialect, dsn string) (*DBStore, error) {
	switch dialect {
	case mysqlDialect:
		var err error
		dsn, err = normalizeMySQLDSN(dsn)
		if err != nil {
			return nil, err
		}
	case "postgres", "sqlite":
	default:
		return nil, fmt.Errorf("unsupported database dialect %q", dialect)
	}
	db, err := sql.Open(driverName(dialect), dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dialect, err)
	}
	// SQLite 通过单个全局锁序列化所有写入；允许 25 个并行连接只会让用户态
	// 队列*更深*而不会增加吞吐量，在繁忙的安装（cron 调度器 + web 流量）
	// 中会迅速堆积超过 busy_timeout 的 SQLITE_BUSY 错误。
	// Postgres 处理真正的并发，因此我们为它保留更宽的连接池。
	if dialect == "sqlite" {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
	} else {
		db.SetMaxOpenConns(25)
		db.SetMaxIdleConns(5)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s: %w", dialect, err)
	}
	return &DBStore{db: db, dialect: dialect}, nil
}

// DB 返回底层的 *sql.DB，以便卫星包（例如 internal/usage）可以针对相同
// 的连接池运行自己的查询，而无需重新打开 DSN。
func (d *DBStore) DB() *sql.DB { return d.db }

// Dialect 返回 SQL 方言，以便卫星包可以为它们的查询选择正确的
// 占位符语法和 upsert 形式。
func (d *DBStore) Dialect() string { return d.dialect }

func driverName(dialect string) string {
	switch dialect {
	case mysqlDialect:
		return "mysql"
	case "postgres":
		return "postgres"
	case "sqlite":
		return "sqlite"
	default:
		return dialect
	}
}

// Migrate 在表不存在时创建它们。该模式是规范形状——没有就地 ALTER 操作，
// 因为在此重写之前没有已安装的基础。
func (d *DBStore) Migrate(ctx context.Context) error {
	// DDL 之前的重命名必须在 migrationSQL 之前运行——否则下面的
	// `CREATE TABLE IF NOT EXISTS <new_name>` 行会在重命名之前创建一个空目标，
	// 并触发"两个表都存在"的分支。
	if err := d.migrateRenameChatEventsToSessionEvents(ctx); err != nil {
		return fmt.Errorf("migrate chat_events → session_events: %w", err)
	}
	migrationSQL := d.migrationSQL()
	if d.dialect == mysqlDialect {
		migrationSQL = mysqlMigrationSQL()
	}
	for _, stmt := range migrationSQL {
		if err := d.execDDL(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w\nSQL: %s", err, stmt)
		}
	}
	if err := d.migrateAgentFilesUserID(ctx); err != nil {
		return fmt.Errorf("migrate agent_files.user_id: %w", err)
	}
	if err := d.migrateAgentsDropTemplateID(ctx); err != nil {
		return fmt.Errorf("migrate agents.template_id drop: %w", err)
	}
	if err := d.migrateAgentsDropModel(ctx); err != nil {
		return fmt.Errorf("migrate agents.model drop: %w", err)
	}
	if err := d.migrateSkillsAgentEntriesSplit(ctx); err != nil {
		return fmt.Errorf("migrate skills.agentEntries split: %w", err)
	}
	if err := d.migrateAgentFilesDropTemplate(ctx); err != nil {
		return fmt.Errorf("migrate agent_files drop template: %w", err)
	}
	if err := d.migrateUsersAppUserCols(ctx); err != nil {
		return fmt.Errorf("migrate users app_user cols: %w", err)
	}
	if err := d.migrateAPIKeysAddType(ctx); err != nil {
		return fmt.Errorf("migrate apikeys.type: %w", err)
	}
	if err := d.migrateUsersAvatarURL(ctx); err != nil {
		return fmt.Errorf("migrate users.avatar_url: %w", err)
	}
	if err := d.migrateCronJobsFailureCount(ctx); err != nil {
		return fmt.Errorf("migrate cron_jobs.failure_count: %w", err)
	}
	if err := d.migrateAgentsAddIsPublic(ctx); err != nil {
		return fmt.Errorf("migrate agents.is_public: %w", err)
	}
	if err := d.migrateDropAgentGrants(ctx); err != nil {
		return fmt.Errorf("migrate drop agent_grants: %w", err)
	}
	if err := d.migrateUsersAddAgentQuota(ctx); err != nil {
		return fmt.Errorf("migrate users.agent_quota: %w", err)
	}
	if err := d.migrateSessionsAddChannelTriple(ctx); err != nil {
		return fmt.Errorf("migrate sessions channel triple: %w", err)
	}
	if err := d.migrateConfigsScopeToUserAgent(ctx); err != nil {
		return fmt.Errorf("migrate configs scope→(user_id,agent_id): %w", err)
	}
	if err := d.migrateCronJobsAddUserID(ctx); err != nil {
		return fmt.Errorf("migrate cron_jobs.user_id: %w", err)
	}
	if err := d.migrateConfigsAddScopeColumn(ctx); err != nil {
		return fmt.Errorf("migrate configs.scope: %w", err)
	}
	if err := d.migrateSessionsAddProjectID(ctx); err != nil {
		return fmt.Errorf("migrate sessions.project_id: %w", err)
	}
	if err := d.migrateSessionMessagesAddOrigin(ctx); err != nil {
		return fmt.Errorf("migrate session_messages.origin: %w", err)
	}
	if err := d.migrateAgentGoalsAddRouting(ctx); err != nil {
		return fmt.Errorf("migrate agent_goals routing: %w", err)
	}
	if err := d.migrateTokenUsageAddProvider(ctx); err != nil {
		return fmt.Errorf("migrate token_usage_daily.provider: %w", err)
	}
	if err := d.migrateSessionsAddChatterUserID(ctx); err != nil {
		return fmt.Errorf("migrate sessions chatter_user_id: %w", err)
	}
	return nil
}

// migrateSessionsAddChatterUserID 将 chatter_user_id 列改装到
// sessions / session_messages / session_events 表上。user_id 继续
// 表示"UserSpace 拥有者"（频道拥有者），因此列出"我的 bot 上所有会话"
// 的管理员视图保持不变；chatter_user_id 保存实际的对话参与者，
// 当 IM 频道将按发送者的 app_users 路由到单个频道拥有者 UserSpace 时，
// 它与会话的 user_id 不同。
//
// 空默认值 + 部分索引保留了在此列存在之前写入的行的现有查询计划。
// 想要获取聊天者的读取者应使用 COALESCE(NULLIF(chatter_user_id,''), user_id)
// ——回退值对于 web 频道完全正确（user_id 在那里已经是聊天者），
// 并且匹配修复前在 IM 上的行为（无论如何每个聊天者都被错误地归属于频道拥有者）。
func (d *DBStore) migrateSessionsAddChatterUserID(ctx context.Context) error {
	for _, t := range []string{"sessions", "session_messages", "session_events"} {
		exists, err := d.tableExists(ctx, t)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		has, err := d.tableHasColumn(ctx, t, "chatter_user_id")
		if err != nil {
			return err
		}
		if !has {
			columnType := "TEXT"
			if d.dialect == mysqlDialect {
				columnType = "VARCHAR(120)"
			}
			if _, err := d.db.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE %s ADD COLUMN chatter_user_id %s NOT NULL DEFAULT ''`, t, columnType)); err != nil {
				return fmt.Errorf("add column on %s: %w", t, err)
			}
		}
	}
	// 部分索引——只有具有非空聊天者的行才会填充索引，因此旧行不会使其膨胀。
	indexSQL := []string{
		`CREATE INDEX IF NOT EXISTS idx_sessions_by_chatter ON sessions (chatter_user_id, agent_id, updated_at DESC) WHERE chatter_user_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_by_chatter ON session_messages (chatter_user_id, agent_id, session_key, seq) WHERE chatter_user_id <> ''`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_by_chatter ON session_events (chatter_user_id, agent_id, session_key, seq) WHERE chatter_user_id <> ''`,
	}
	if d.dialect == mysqlDialect {
		indexSQL = []string{
			`CREATE INDEX IF NOT EXISTS idx_sessions_by_chatter ON sessions (chatter_user_id, agent_id, updated_at DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_session_messages_by_chatter ON session_messages (chatter_user_id, agent_id, session_key, seq)`,
			`CREATE INDEX IF NOT EXISTS idx_session_events_by_chatter ON session_events (chatter_user_id, agent_id, session_key, seq)`,
		}
	}
	for _, stmt := range indexSQL {
		if err := d.execDDL(ctx, stmt); err != nil {
			return fmt.Errorf("create chatter index: %w (sql: %s)", err, stmt)
		}
	}
	return nil
}

// migrateAgentGoalsAddRouting 将 channel/account_id/chat_id/project_id
// 改装到旧的 agent_goals 表上。所有四个列默认值为 ''——预先存在的行
// 无论如何都没有附加延续基础设施，因此空值仅表示"未记录路由；无法自动
// 继续此目标"，TryFireContinuation 安全退出。幂等。
func (d *DBStore) migrateAgentGoalsAddRouting(ctx context.Context) error {
	for _, col := range []string{"channel", "account_id", "chat_id", "project_id"} {
		has, err := d.tableHasColumn(ctx, "agent_goals", col)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		columnType := "TEXT"
		if d.dialect == mysqlDialect {
			columnType = "VARCHAR(191)"
			if col == "channel" {
				columnType = "VARCHAR(64)"
			} else if col == "project_id" {
				columnType = "VARCHAR(120)"
			}
		}
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE agent_goals ADD COLUMN %s %s NOT NULL DEFAULT ''`, col, columnType)); err != nil {
			return fmt.Errorf("add column %s: %w", col, err)
		}
	}
	return nil
}

// migrateSessionMessagesAddOrigin 将 origin 列改装到旧的 session_messages
// 表上。空默认值 = 预先存在的用户/助手消息保持不变。非空值标记运行时注入
// 的行（目前仅有 "goal_context"），以便 WebChatHistory 读取器可以跳过它们。
// 幂等。
func (d *DBStore) migrateSessionMessagesAddOrigin(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "session_messages", "origin")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	columnType := "TEXT"
	if d.dialect == mysqlDialect {
		columnType = "VARCHAR(64)"
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN origin %s NOT NULL DEFAULT ''`, columnType)); err != nil {
		return fmt.Errorf("add column: %w", err)
	}
	return nil
}

// migrateTokenUsageAddProvider 将 `provider` 列改装到旧的 token_usage_daily
// 表上，该表在按提供商细分功能发布之前创建。预发布模式在主键中只有
// (day, user, agent, session, model)，这使得按 provider 进行 GROUP BY 不可行
//（并允许不同提供商的同名模型冲突）。由于该表只保存仪表板每次刷新时重新读取的
// 累计计数器，在罕见的升级路径上删除它比使用 SQLite 的"创建新表+复制+交换"
// 方式重建主键更便宜。幂等：如果列已存在则提前返回，如果表本身尚不存在
// 则无操作（新安装运行 migrationSQL 中的新 CREATE TABLE）。
func (d *DBStore) migrateTokenUsageAddProvider(ctx context.Context) error {
	exists, err := d.tableExists(ctx, "token_usage_daily")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	has, err := d.tableHasColumn(ctx, "token_usage_daily", "provider")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.db.ExecContext(ctx, `DROP TABLE token_usage_daily`); err != nil {
		return fmt.Errorf("drop old token_usage_daily: %w", err)
	}
	// migrationSQL 的 CREATE TABLE IF NOT EXISTS 将在下一次传递中用新模式
	// 重新创建它。我们依赖这样的事实：此迁移步骤在同一个 Migrate() 调用顺序中
	// 在 migrationSQL 之后运行——因此在任何 agent 流量到达之前，
	// 该表将以正确的形状重新创建。
	createSQL := `CREATE TABLE token_usage_daily (
		day DATE NOT NULL,
		user_id TEXT NOT NULL DEFAULT '',
		agent_id TEXT NOT NULL DEFAULT '',
		session_key TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		input_tokens BIGINT NOT NULL DEFAULT 0,
		output_tokens BIGINT NOT NULL DEFAULT 0,
		cache_read_tokens BIGINT NOT NULL DEFAULT 0,
		cache_create_tokens BIGINT NOT NULL DEFAULT 0,
		request_count BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (day, user_id, agent_id, session_key, provider, model)
	)`
	if d.dialect == mysqlDialect {
		createSQL = mysqlTokenUsageTableSQL()
	}
	if _, err := d.db.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("recreate token_usage_daily: %w", err)
	}
	if err := d.execDDL(ctx,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_agent ON token_usage_daily (agent_id, day)`); err != nil {
		return err
	}
	if err := d.execDDL(ctx,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_user ON token_usage_daily (user_id, day)`); err != nil {
		return err
	}
	return nil
}

// migrateSessionsAddProjectID 将 project_id 列添加到旧的 sessions 表。
// 空默认值 = "松散聊天"（现有行为），非空值 = 属于该项目。
// 幂等：如果列已存在则提前返回。
func (d *DBStore) migrateSessionsAddProjectID(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "sessions", "project_id")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	columnType := "TEXT"
	if d.dialect == mysqlDialect {
		columnType = "VARCHAR(120)"
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE sessions ADD COLUMN project_id %s NOT NULL DEFAULT ''`, columnType)); err != nil {
		return fmt.Errorf("add column: %w", err)
	}
	return nil
}

// migrateConfigsAddScopeColumn 将反规范化的 scope 标签列改装到旧的 configs 行上。
// 功能之前的 configs 行有 (user_id, agent_id) 对但没有 scope 提示——此函数添加该列
// 并一次性回填。新行由 SaveConfig 写入，它是唯一可以发出 scope 值的地方。
//
// 幂等：如果列已存在则提前返回。
func (d *DBStore) migrateConfigsAddScopeColumn(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "configs", "scope")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	columnType := "TEXT"
	if d.dialect == mysqlDialect {
		columnType = "VARCHAR(32)"
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE configs ADD COLUMN scope %s NOT NULL DEFAULT ''`, columnType)); err != nil {
		return fmt.Errorf("add column: %w", err)
	}
	// 在一个 UPDATE 中回填——与 computeConfigScope 在 Go 中编码的相同 CASE 表达式。
	// 两种方言都支持 CASE WHEN 形式不变。
	if _, err := d.db.ExecContext(ctx, `UPDATE configs SET scope = CASE
		WHEN user_id != '' AND agent_id != '' THEN 'user-agent'
		WHEN user_id != ''                     THEN 'user'
		WHEN agent_id != ''                    THEN 'agent'
		ELSE 'system'
	END WHERE scope = ''`); err != nil {
		return fmt.Errorf("backfill scope: %w", err)
	}
	return nil
}

// migrateRenameChatEventsToSessionEvents 将流式事件增量表从 `chat_events`
// 重命名为 `session_events`，使其与 `sessions` / `session_messages` 共享
// session_* 前缀。"chat" 标签具有误导性——该表也存储 wechat / telegram / line
// / web 会话的事件，而不仅仅是 web "聊天"。
//
// 幂等：如果新名称已存在或者旧名称不存在，则该函数无操作。在重命名时，
// 查找索引也会移动。
func (d *DBStore) migrateRenameChatEventsToSessionEvents(ctx context.Context) error {
	hasNew, err := d.tableExists(ctx, "session_events")
	if err != nil {
		return err
	}
	if hasNew {
		return nil
	}
	hasOld, err := d.tableExists(ctx, "chat_events")
	if err != nil {
		return err
	}
	if !hasOld {
		// 防御性——新安装从未有 chat_events，因为 migrationSQL 直接写入
		// session_events。已经运行过此迁移的旧安装会在上面的 hasNew=true 处命中。
		return nil
	}
	if _, err := d.db.ExecContext(ctx, `ALTER TABLE chat_events RENAME TO session_events`); err != nil {
		return fmt.Errorf("rename table: %w", err)
	}
	if d.dialect == "postgres" {
		_, _ = d.db.ExecContext(ctx,
			`ALTER INDEX IF EXISTS idx_chat_events_lookup RENAME TO idx_session_events_lookup`)
	} else if d.dialect == mysqlDialect {
		_, _ = d.db.ExecContext(ctx,
			`ALTER TABLE session_events RENAME INDEX idx_chat_events_lookup TO idx_session_events_lookup`)
	} else {
		// SQLite 在较旧版本上没有 ALTER INDEX RENAME；删除并用新名称重新创建。
		// DROP 是尽力而为的——它可能已经不存在了。
		_, _ = d.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_chat_events_lookup`)
	}
	if err := d.execDDL(ctx,
		`CREATE INDEX IF NOT EXISTS idx_session_events_lookup ON session_events (user_id, agent_id, session_key, seq)`); err != nil {
		return fmt.Errorf("recreate index: %w", err)
	}
	return nil
}

// tableExists 是一个由表重命名迁移使用的小型辅助函数。
// SQLite 读取 sqlite_master；Postgres 使用 to_regclass。
func (d *DBStore) tableExists(ctx context.Context, table string) (bool, error) {
	if d.dialect == "postgres" {
		var name *string
		err := d.db.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, table).Scan(&name)
		if err != nil {
			return false, err
		}
		return name != nil, nil
	}
	if d.dialect == mysqlDialect {
		var n int
		err := d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables
				WHERE table_schema = DATABASE() AND table_name = ?`, table).Scan(&n)
		return n > 0, err
	}
	var name string
	err := d.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name = ?`, table).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return name != "", nil
}

// migrateConfigsScopeToUserAgent 将 configs 表从历史 (scope, scope_id)
// 多态对重写为显式的 (user_id, agent_id) 列。
//
// 回填规则：
//
//	scope='system'           → user_id='',           agent_id=''
//	scope='user',  scope_id=X→ user_id=X,            agent_id=''
//	scope='agent', scope_id=Y→
//	  - kind='channel' 或 'setting'/name='bindings'：
//	      user_id = agents.user_id（拥有者；频道路由到他们），
//	      agent_id = Y。这最终在行本身内记录了是谁绑定了频道。
//	  - 其他 kind（provider / setting）：
//	      user_id = '',          agent_id = Y
//
// kind='setting'/name='bindings' 的行在最后被迁移删除——频道行现在直接携带
// 它们的 agent_id，因此间接层已消失。
func (d *DBStore) migrateConfigsScopeToUserAgent(ctx context.Context) error {
	hasUserID, err := d.tableHasColumn(ctx, "configs", "user_id")
	if err != nil {
		return err
	}
	if !hasUserID {
		// 探测 `scope_id` 而不是 `scope`：重构后的模式将 `scope` 重新引入为
		// 反规范化标签，因此它的存在不再意味着"这是旧形状"。
		hasScopeID, err := d.tableHasColumn(ctx, "configs", "scope_id")
		if err != nil {
			return err
		}
		if hasScopeID {
			if d.dialect == "postgres" {
				if err := d.migrateConfigsScopeToUserAgentPostgres(ctx); err != nil {
					return err
				}
			} else if d.dialect == mysqlDialect {
				if err := d.migrateConfigsScopeToUserAgentMySQL(ctx); err != nil {
					return err
				}
			} else {
				if err := d.migrateConfigsScopeToUserAgentSQLite(ctx); err != nil {
					return err
				}
			}
		}
	}
	// 总是确保查找索引——升级和新安装路径都流经这里。
	// CREATE INDEX IF NOT EXISTS 是幂等的。
	if err := d.execDDL(ctx,
		`CREATE INDEX IF NOT EXISTS idx_configs_lookup ON configs (kind, user_id, agent_id)`); err != nil {
		return fmt.Errorf("create configs index: %w", err)
	}
	return nil
}

func (d *DBStore) migrateConfigsScopeToUserAgentPostgres(ctx context.Context) error {
	// Postgres 可以直接 ALTER：添加列、回填、删除索引 + 唯一约束、
	// 删除 scope 列、重新创建索引 + 唯一约束。
	stmts := []string{
		`ALTER TABLE configs ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE configs ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT ''`,
		// scope=user：user_id = scope_id
		`UPDATE configs SET user_id = scope_id WHERE scope = 'user' AND user_id = ''`,
		// scope=agent +（channel 或 bindings）：user_id = agents.user_id，agent_id = scope_id
		`UPDATE configs c SET user_id = a.user_id, agent_id = c.scope_id
		   FROM agents a
		   WHERE c.scope = 'agent' AND c.user_id = '' AND c.agent_id = ''
		     AND a.id = c.scope_id
		     AND (c.kind = 'channel' OR (c.kind = 'setting' AND c.name = 'bindings'))`,
		// scope=agent + 其他 kind：仅设置 agent_id
		`UPDATE configs SET agent_id = scope_id
		   WHERE scope = 'agent' AND agent_id = ''`,
		// 删除 kind=setting/name=bindings 的行——绑定现在隐含在频道行的 agent_id 中。
		`DELETE FROM configs WHERE kind = 'setting' AND name = 'bindings'`,
		`DROP INDEX IF EXISTS idx_configs_lookup`,
		`ALTER TABLE configs DROP CONSTRAINT IF EXISTS configs_kind_scope_scope_id_name_key`,
		`ALTER TABLE configs DROP COLUMN IF EXISTS scope`,
		`ALTER TABLE configs DROP COLUMN IF EXISTS scope_id`,
		`ALTER TABLE configs ADD CONSTRAINT configs_kind_user_agent_name_key UNIQUE (kind, user_id, agent_id, name)`,
		`CREATE INDEX IF NOT EXISTS idx_configs_lookup ON configs (kind, user_id, agent_id)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("postgres migrate configs: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

func (d *DBStore) migrateConfigsScopeToUserAgentSQLite(ctx context.Context) error {
	// SQLite 不能在我们支持的范围内跨所有版本可靠地就地删除/更改列，
	// 因此我们使用复制-重命名表的方式。
	stmts := []string{
		`CREATE TABLE configs_new (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (kind, user_id, agent_id, name)
		)`,
		// scope=system：两个 ID 都为空
		// scope=user：user_id = scope_id，agent_id = ''
		// scope=agent +（channel | setting/name=bindings）：
		//               user_id = agents.user_id，agent_id = scope_id
		// scope=agent + 其他：
		//               user_id = ''，agent_id = scope_id
		// 跳过 kind='setting' AND name='bindings' 的行——频道行现在直接携带
		// agent_id，因此这个间接表是多余的。
		`INSERT INTO configs_new (id, kind, user_id, agent_id, name, enabled, credential_key, data, created_at, updated_at)
		   SELECT
		     c.id,
		     c.kind,
		     CASE
		       WHEN c.scope = 'user' THEN c.scope_id
		       WHEN c.scope = 'agent' AND (c.kind = 'channel' OR (c.kind = 'setting' AND c.name = 'bindings'))
		         THEN COALESCE((SELECT a.user_id FROM agents a WHERE a.id = c.scope_id), '')
		       ELSE ''
		     END AS user_id,
		     CASE
		       WHEN c.scope = 'agent' THEN c.scope_id
		       ELSE ''
		     END AS agent_id,
		     c.name, c.enabled, c.credential_key, c.data, c.created_at, c.updated_at
		   FROM configs c
		   WHERE NOT (c.kind = 'setting' AND c.name = 'bindings')`,
		`DROP TABLE configs`,
		`ALTER TABLE configs_new RENAME TO configs`,
		`DROP INDEX IF EXISTS idx_configs_lookup`,
		`CREATE INDEX IF NOT EXISTS idx_configs_lookup ON configs (kind, user_id, agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_configs_credential ON configs (kind, credential_key)`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("sqlite migrate configs: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// migrateCronJobsAddUserID 将 user_id 改装到 cron_jobs 表上，使
// (user_id, agent_id) 键控与代码库的其余部分匹配。回填连接 agents 表
// 以恢复拥有用户。新行必须显式填充 user_id（SaveCronJob 强制执行）。
func (d *DBStore) migrateCronJobsAddUserID(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "cron_jobs", "user_id")
	if err != nil {
		return err
	}
	if !has {
		columnType := "TEXT"
		if d.dialect == mysqlDialect {
			columnType = "VARCHAR(120)"
		}
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE cron_jobs ADD COLUMN user_id %s NOT NULL DEFAULT ''`, columnType)); err != nil {
			return fmt.Errorf("add cron_jobs.user_id: %w", err)
		}
		if _, err := d.db.ExecContext(ctx,
			`UPDATE cron_jobs SET user_id = COALESCE((SELECT a.user_id FROM agents a WHERE a.id = cron_jobs.agent_id), '')
			 WHERE user_id = ''`); err != nil {
			return fmt.Errorf("backfill cron_jobs.user_id: %w", err)
		}
	}
	// 总是确保查找索引——新安装也流经这里。
	if err := d.execDDL(ctx,
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_user ON cron_jobs (user_id, agent_id)`); err != nil {
		return fmt.Errorf("index cron_jobs.user_id: %w", err)
	}
	return nil
}

// migrateSessionsAddChannelTriple 将 channel / account_id / chat_id
// 改装到功能之前的 sessions 行上。现有的 session_keys 遵循
// `<channel>_<chatID>` 约定（web_<sid>、wechat_<openid>、…），
// 因此回填在第一个下划线处分割。account_id 没有历史来源——功能之前的安装
// 每个频道只运行一个 bot，因此为这些行保留 '' 是正确的。
// 此迁移之后写入的新会话始终显式填充完整的三元组。
func (d *DBStore) migrateSessionsAddChannelTriple(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "sessions", "channel")
	if err != nil {
		return err
	}
	if !has {
		textType := "TEXT"
		channelType := textType
		if d.dialect == mysqlDialect {
			textType = "VARCHAR(191)"
			channelType = "VARCHAR(64)"
		}
		for _, stmt := range []string{
			fmt.Sprintf(`ALTER TABLE sessions ADD COLUMN channel %s NOT NULL DEFAULT ''`, channelType),
			fmt.Sprintf(`ALTER TABLE sessions ADD COLUMN account_id %s NOT NULL DEFAULT ''`, textType),
			fmt.Sprintf(`ALTER TABLE sessions ADD COLUMN chat_id %s NOT NULL DEFAULT ''`, textType),
		} {
			if _, err := d.db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("add column: %w (sql: %s)", err, stmt)
			}
		}
		// 从旧的 `<channel>_<chatID>` session_key 形状回填。
		// SQLite 和 Postgres 都提供 substr / instr 风格的函数；
		// 我们选择适合方言的那个。没有下划线的行
		//（在实践中不应该发生，但防御性）得到 channel='' 和 chat_id=key。
		var backfill string
		if d.dialect == "postgres" {
			backfill = `UPDATE sessions
				SET channel = COALESCE(NULLIF(SPLIT_PART(session_key, '_', 1), ''), ''),
				    chat_id = CASE
				        WHEN POSITION('_' IN session_key) > 0
				        THEN SUBSTRING(session_key FROM POSITION('_' IN session_key) + 1)
				        ELSE session_key
				    END
				WHERE channel = '' AND chat_id = ''`
		} else {
			backfill = `UPDATE sessions
				SET channel = CASE WHEN INSTR(session_key, '_') > 0 THEN SUBSTR(session_key, 1, INSTR(session_key, '_') - 1) ELSE '' END,
				    chat_id = CASE WHEN INSTR(session_key, '_') > 0 THEN SUBSTR(session_key, INSTR(session_key, '_') + 1) ELSE session_key END
				WHERE channel = '' AND chat_id = ''`
		}
		if _, err := d.db.ExecContext(ctx, backfill); err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
	}
	// 总是（重新）确保索引——migrationSQL 中的 CREATE INDEX 已被移除，
	// 因为它在旧数据库上会在列存在之前触发。IF NOT EXISTS 使其对新安装幂等。
	if err := d.execDDL(ctx,
		`CREATE INDEX IF NOT EXISTS idx_sessions_chat_active ON sessions (user_id, agent_id, channel, account_id, chat_id, updated_at DESC)`); err != nil {
		return fmt.Errorf("create index: %w", err)
	}
	return nil
}

// migrateUsersAddAgentQuota 将 agent_quota 列改装到功能之前的安装上。
// 默认 -1 = 无限制，这为配额引入之前已存在的用户保留了
// "任何人都可以创建任意数量的 agent"的现有行为。
func (d *DBStore) migrateUsersAddAgentQuota(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "users", "agent_quota")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.db.ExecContext(ctx,
		`ALTER TABLE users ADD COLUMN agent_quota INTEGER NOT NULL DEFAULT -1`); err != nil {
		return fmt.Errorf("add agent_quota: %w", err)
	}
	return nil
}

// migrateAgentsAddIsPublic 将 is_public 列改装到功能之前的安装上。
// 默认 FALSE 使每个现有的 agent 在升级后仍然仅限拥有者——通过编辑对话框选择启用。
func (d *DBStore) migrateAgentsAddIsPublic(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "agents", "is_public")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.db.ExecContext(ctx,
		`ALTER TABLE agents ADD COLUMN is_public BOOLEAN NOT NULL DEFAULT FALSE`); err != nil {
		return fmt.Errorf("add is_public: %w", err)
	}
	return nil
}

// migrateDropAgentGrants 删除旧的按用户共享表。
// 共享现在位于 agents.is_public 上；现有的按用户授权不会向前迁移
//（先前的模型没有发布给普通用户）。DROP TABLE IF EXISTS 是幂等的，
// 在从未创建该表的新安装上是无操作。
func (d *DBStore) migrateDropAgentGrants(ctx context.Context) error {
	if _, err := d.db.ExecContext(ctx, `DROP TABLE IF EXISTS agent_grants`); err != nil {
		return fmt.Errorf("drop agent_grants: %w", err)
	}
	return nil
}

// migrateCronJobsFailureCount 将 failure_count 列改装到功能之前的安装上。
// 默认 0 将现有行回填为"健康"状态，这样自动删除阈值不会在升级后的
// 第一次触发时生效。
func (d *DBStore) migrateCronJobsFailureCount(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "cron_jobs", "failure_count")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := d.db.ExecContext(ctx,
		`ALTER TABLE cron_jobs ADD COLUMN failure_count INTEGER NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("add failure_count: %w", err)
	}
	return nil
}

// migrateUsersAvatarURL 将 avatar_url 列改装到功能之前的安装上。
// 存储为 data: URL，因此文件与行内联存在——没有单独的 blob 存储路径或清理。
// 空字符串表示"无头像"；UI 回退到首字母。
func (d *DBStore) migrateUsersAvatarURL(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "users", "avatar_url")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	columnDDL := "TEXT NOT NULL DEFAULT ''"
	if d.dialect == mysqlDialect {
		columnDDL = "LONGTEXT NOT NULL"
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE users ADD COLUMN avatar_url %s`, columnDDL)); err != nil {
		return fmt.Errorf("add avatar_url: %w", err)
	}
	return nil
}

// migrateAPIKeysAddType 将 `type` 列改装到层级之前的 apikeys 安装上。
// 每个旧行都是显式-agent-列表密钥，因此回填 DEFAULT 'agent' 保留了行为
// ——admin/user 层级只能从此时开始创建。
func (d *DBStore) migrateAPIKeysAddType(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "apikeys", "type")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	columnType := "TEXT"
	if d.dialect == mysqlDialect {
		columnType = "VARCHAR(32)"
	}
	if _, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE apikeys ADD COLUMN type %s NOT NULL DEFAULT 'agent'`, columnType)); err != nil {
		return fmt.Errorf("add type: %w", err)
	}
	return nil
}

// migrateUsersAppUserCols 将 apikey_id + external_id 列改装到现有安装的
// users 表上，并创建用于幂等配置的部分唯一索引。CREATE TABLE 仅在新数据库上
// 触发；旧数据库以旧的 7 列 users 表到达此处，并以新的 9 列形状退出。
// 幂等：每一步在变更之前探测现有状态。
func (d *DBStore) migrateUsersAppUserCols(ctx context.Context) error {
	hasAPIKey, err := d.tableHasColumn(ctx, "users", "apikey_id")
	if err != nil {
		return err
	}
	if !hasAPIKey {
		columnType := "TEXT"
		if d.dialect == mysqlDialect {
			columnType = "VARCHAR(120)"
		}
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE users ADD COLUMN apikey_id %s NOT NULL DEFAULT ''`, columnType)); err != nil {
			return fmt.Errorf("add apikey_id: %w", err)
		}
	}
	hasExt, err := d.tableHasColumn(ctx, "users", "external_id")
	if err != nil {
		return err
	}
	if !hasExt {
		columnType := "TEXT"
		if d.dialect == mysqlDialect {
			columnType = "VARCHAR(120)"
		}
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE users ADD COLUMN external_id %s NOT NULL DEFAULT ''`, columnType)); err != nil {
			return fmt.Errorf("add external_id: %w", err)
		}
	}
	if d.dialect == mysqlDialect {
		return d.ensureMySQLAppUserKey(ctx)
	}
	if err := d.execDDL(ctx,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_apikey_external
			ON users (apikey_id, external_id)
			WHERE apikey_id <> '' AND external_id <> ''`); err != nil {
		return fmt.Errorf("create idx_users_apikey_external: %w", err)
	}
	return nil
}

// migrateAgentFilesDropTemplate 清除 agent_files 中旧的 user_id='' 模板行。
// 每行在 (agent_id, filename) 没有已存在的按用户行时，被重新分配给 agent 的
// 拥有者——保留现有内容作为拥有者的个人副本。在此次传递之后，表仅持有
// (agent_id, real_user_id, filename) 元组；
// 任何"跨所有用户共享 SOUL.md"的用例应位于本地文件系统中
// <agent_home>/<name> 的文件中，运行时会回退到该文件。
// 幂等：重新运行时找不到 user_id='' 的行并干净退出。
func (d *DBStore) migrateAgentFilesDropTemplate(ctx context.Context) error {
	rows, err := d.db.QueryContext(ctx,
		`SELECT agent_files.agent_id, agent_files.filename, agent_files.content, agents.user_id
			FROM agent_files
			LEFT JOIN agents ON agents.id = agent_files.agent_id
			WHERE agent_files.user_id = ''`)
	if err != nil {
		return fmt.Errorf("scan template rows: %w", err)
	}
	type tmpl struct {
		agentID, filename, content string
		ownerID                    sql.NullString
	}
	var pending []tmpl
	for rows.Next() {
		var t tmpl
		if err := rows.Scan(&t.agentID, &t.filename, &t.content, &t.ownerID); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, t)
	}
	rows.Close()
	now := time.Now().UTC()
	for _, t := range pending {
		if t.ownerID.Valid && t.ownerID.String != "" {
			// 仅在拥有者对此 (agent_id, filename) 没有自己的行时才重新父化——
			// 绝不覆盖现有的个人副本。
			var exists int
			row := d.db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT 1 FROM agent_files
					WHERE agent_id = %s AND user_id = %s AND filename = %s LIMIT 1`,
					d.ph(1), d.ph(2), d.ph(3)),
				t.agentID, t.ownerID.String, t.filename)
			if err := row.Scan(&exists); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("probe existing row: %w", err)
			}
			if exists != 1 {
				if d.dialect == "postgres" {
					if _, err := d.db.ExecContext(ctx,
						`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
							VALUES ($1, $2, $3, $4, $5)`,
						t.agentID, t.ownerID.String, t.filename, t.content, now); err != nil {
						return fmt.Errorf("reparent template row: %w", err)
					}
				} else {
					if _, err := d.db.ExecContext(ctx,
						`INSERT INTO agent_files (agent_id, user_id, filename, content, updated_at)
							VALUES (?, ?, ?, ?, ?)`,
						t.agentID, t.ownerID.String, t.filename, t.content, now); err != nil {
						return fmt.Errorf("reparent template row: %w", err)
					}
				}
			}
		}
	}
	if _, err := d.db.ExecContext(ctx,
		`DELETE FROM agent_files WHERE user_id = ''`); err != nil {
		return fmt.Errorf("delete template rows: %w", err)
	}
	return nil
}

// migrateSkillsAgentEntriesSplit 将每个 agent 的技能环境覆盖从单个用户/系统范围的
// skills.agentEntries 行（一个以 agent_id 为键的 JSON blob，随着每个 agent × skill
// 无限增长）迁移到每个 agent 一行，scope=agent, name=skills.entries——与运行时现在
// 通过 scope.GetConfigByName 读取的形状相同。
// 幂等：每个遗留行在单次传递中被拆分 + 删除；后续运行找不到遗留行并干净退出。
func (d *DBStore) migrateSkillsAgentEntriesSplit(ctx context.Context) error {
	// 以 `scope_id` 而非 `scope` 为门控：新模式将 `scope` 列作为反规范化标签重新引入，
	// 但 `scope_id` 已消失。探测 `scope_id` 可靠地检测"这是功能之前的安装"，
	// 并避免对新形状运行旧的 SELECT。
	hasScopeID, err := d.tableHasColumn(ctx, "configs", "scope_id")
	if err != nil {
		return err
	}
	if !hasScopeID {
		return nil
	}
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, scope, scope_id, data FROM configs WHERE kind='setting' AND name='skills.agentEntries'`)
	if err != nil {
		return fmt.Errorf("scan legacy: %w", err)
	}
	type legacy struct{ id, scopeID, dataJSON string }
	var legacyRows []legacy
	for rows.Next() {
		var l legacy
		var sc string
		if err := rows.Scan(&l.id, &sc, &l.scopeID, &l.dataJSON); err != nil {
			rows.Close()
			return err
		}
		_ = sc
		legacyRows = append(legacyRows, l)
	}
	rows.Close()
	now := time.Now().UTC()
	for _, l := range legacyRows {
		// data shape: { "<agent_id>": { "<skill_name>": { ...entry } } }
		var byAgent map[string]map[string]interface{}
		if err := json.Unmarshal([]byte(l.dataJSON), &byAgent); err != nil {
			// 格式错误的行——删除它；不值得中止迁移。
			if _, derr := d.db.ExecContext(ctx,
				fmt.Sprintf(`DELETE FROM configs WHERE id=%s`, d.ph(1)), l.id); derr != nil {
				return fmt.Errorf("drop malformed legacy row: %w", derr)
			}
			continue
		}
		for agentID, inner := range byAgent {
			if len(inner) == 0 {
				continue
			}
			cid := configRowID("setting", "agent", agentID, "skills.entries")
			innerBlob, _ := json.Marshal(inner)
			// 如果每个 agent 的行已存在则跳过（手动编辑、之前的部分迁移等）——不覆盖。
			var exists int
			err := d.db.QueryRowContext(ctx,
				fmt.Sprintf(`SELECT 1 FROM configs WHERE id=%s LIMIT 1`, d.ph(1)), cid).Scan(&exists)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check existing per-agent row: %w", err)
			}
			insert := fmt.Sprintf(`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at)
				VALUES (%s, 'setting', 'agent', %s, 'skills.entries', TRUE, '', %s, %s, %s)`,
				d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
			if _, err := d.db.ExecContext(ctx, insert, cid, agentID, string(innerBlob), now, now); err != nil {
				return fmt.Errorf("insert per-agent row for %s: %w", agentID, err)
			}
		}
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM configs WHERE id=%s`, d.ph(1)), l.id); err != nil {
			return fmt.Errorf("drop legacy row: %w", err)
		}
	}
	return nil
}

// migrateAgentsDropModel 将每个 agent 的模型覆盖从 agents.model 列迁移到 configs
// 表（kind=setting, scope=agent, scope_id=<aid>, name="agents.defaults",
// data={"model":"..."})。configs 路径是运行时现在通过 scope.SettingInto 读取的方式，
// 因此保留该列只会重复状态。幂等：列消失后静默无操作。
func (d *DBStore) migrateAgentsDropModel(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "agents", "model")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	// 迁移使用旧的 (scope, scope_id) 列 INSERT 到 configs 中。
	// 最初就没有 agents.model 的新安装不会到达此分支（上面的列存在检查已经返回）。
	// 但一个无序丢失 scope_id 列的旧安装会到达——门控它。（scope 在重构后仍然作为
	// 反规范化标签存在，scope_id 不存在。）
	hasScopeID, err := d.tableHasColumn(ctx, "configs", "scope_id")
	if err != nil {
		return err
	}
	if !hasScopeID {
		// 意外状态的旧安装——删除列以便编排器可以继续；数据可能已被先前的运行迁移。
		stmt := `ALTER TABLE agents DROP COLUMN model`
		if d.dialect == "postgres" {
			stmt = `ALTER TABLE agents DROP COLUMN IF EXISTS model`
		}
		_, _ = d.db.ExecContext(ctx, stmt)
		return nil
	}
	rows, err := d.db.QueryContext(ctx, `SELECT id, model FROM agents WHERE model <> ''`)
	if err != nil {
		return fmt.Errorf("scan legacy models: %w", err)
	}
	type row struct{ id, model string }
	var legacy []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.model); err != nil {
			rows.Close()
			return err
		}
		legacy = append(legacy, r)
	}
	rows.Close()
	now := time.Now().UTC()
	for _, r := range legacy {
		// 不覆盖已存在的 configs 行——运行时自此迁移发布以来一直在那里写入，
		// 因此现有行是事实来源。
		var exists int
		err := d.db.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT 1 FROM configs WHERE kind='setting' AND scope='agent' AND scope_id=%s AND name='agents.defaults' LIMIT 1`,
				d.ph(1)),
			r.id).Scan(&exists)
		if err == nil {
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing setting: %w", err)
		}
		cid := configRowID("setting", "agent", r.id, "agents.defaults")
		blob, _ := json.Marshal(map[string]string{"model": r.model})
		insertSQL := fmt.Sprintf(`INSERT INTO configs (id, kind, scope, scope_id, name, enabled, credential_key, data, created_at, updated_at)
			VALUES (%s, 'setting', 'agent', %s, 'agents.defaults', TRUE, '', %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5))
		if _, err := d.db.ExecContext(ctx, insertSQL, cid, r.id, string(blob), now, now); err != nil {
			return fmt.Errorf("relocate model for agent %s: %w", r.id, err)
		}
	}
	stmt := `ALTER TABLE agents DROP COLUMN model`
	if d.dialect == "postgres" {
		stmt = `ALTER TABLE agents DROP COLUMN IF EXISTS model`
	}
	if _, err := d.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("drop column: %w\nSQL: %s", err, stmt)
	}
	return nil
}

// migrateAgentsDropTemplateID 从现有安装中删除从未被读取的 template_id 列。
// 幂等：列已消失时静默无操作。SQLite 需要 3.35+ 才能使用 DROP COLUMN——
// 这里所有支持的运行时版本都远超于此，因此我们不回退到重建。
func (d *DBStore) migrateAgentsDropTemplateID(ctx context.Context) error {
	has, err := d.tableHasColumn(ctx, "agents", "template_id")
	if err != nil {
		return err
	}
	if !has {
		return nil
	}
	stmt := `ALTER TABLE agents DROP COLUMN template_id`
	if d.dialect == "postgres" {
		stmt = `ALTER TABLE agents DROP COLUMN IF EXISTS template_id`
	}
	if _, err := d.db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("drop column: %w\nSQL: %s", err, stmt)
	}
	return nil
}

// migrateAgentFilesUserID 在现有安装上改装每用户覆盖列。
// CREATE TABLE IF NOT EXISTS 仅在新数据库上触发，因此旧数据库保留旧的
// (agent_id, filename) 主键直到此函数运行。幂等：检测缺失的列并一次性重建表。
// SQLite 没有用于更改主键的 ALTER TABLE，因此我们采用复制-重命名方式。
// Postgres 可以直接 ALTER。
func (d *DBStore) migrateAgentFilesUserID(ctx context.Context) error {
	hasUserID, err := d.tableHasColumn(ctx, "agent_files", "user_id")
	if err != nil {
		return err
	}
	if hasUserID {
		return nil
	}
	if d.dialect == "postgres" {
		stmts := []string{
			`ALTER TABLE agent_files ADD COLUMN user_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE agent_files DROP CONSTRAINT IF EXISTS agent_files_pkey`,
			`ALTER TABLE agent_files ADD PRIMARY KEY (agent_id, user_id, filename)`,
		}
		for _, s := range stmts {
			if _, err := d.db.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("postgres migrate: %w\nSQL: %s", err, s)
			}
		}
		return nil
	}
	if d.dialect == mysqlDialect {
		stmts := []string{
			`ALTER TABLE agent_files ADD COLUMN user_id VARCHAR(120) NOT NULL DEFAULT '' AFTER agent_id`,
			`ALTER TABLE agent_files DROP PRIMARY KEY`,
			`ALTER TABLE agent_files ADD PRIMARY KEY (agent_id, user_id, filename)`,
		}
		for _, s := range stmts {
			if _, err := d.db.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("mysql migrate agent_files: %w\nSQL: %s", err, s)
			}
		}
		return nil
	}
	// SQLite: 重建表以扩展主键。
	stmts := []string{
		`CREATE TABLE agent_files_new (
			agent_id TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			filename TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, user_id, filename)
		)`,
		`INSERT INTO agent_files_new (agent_id, user_id, filename, content, updated_at)
			SELECT agent_id, '', filename, content, updated_at FROM agent_files`,
		`DROP TABLE agent_files`,
		`ALTER TABLE agent_files_new RENAME TO agent_files`,
	}
	for _, s := range stmts {
		if _, err := d.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("sqlite rebuild: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// tableHasColumn 当表中存在指定列时返回 true。
// 后端特定：Postgres 读取 information_schema；SQLite 使用 PRAGMA table_info() 伪表。
func (d *DBStore) tableHasColumn(ctx context.Context, table, column string) (bool, error) {
	if d.dialect == "postgres" {
		row := d.db.QueryRowContext(ctx,
			`SELECT 1 FROM information_schema.columns
				WHERE table_name = $1 AND column_name = $2 LIMIT 1`,
			table, column)
		var x int
		if err := row.Scan(&x); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if d.dialect == mysqlDialect {
		var n int
		err := d.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.columns
				WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?`,
			table, column).Scan(&n)
		return n > 0, err
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (d *DBStore) migrationSQL() []string {
	return []string{
		// users 表保存第一方人类（role=super_admin/user）和
		// 应用配置的最终用户（role=app_user）。后者由 api_key
		// 代表下游应用创建；他们不能登录（password_hash='' 被密码登录路径拒绝）。
		// apikey_id + external_id 共同标识"哪个调用应用，哪个最终用户"，
		// 部分 UNIQUE 使配置在该对上幂等，因此同一外部用户始终解析为相同的
		// bkclaw user_id。
		`CREATE TABLE IF NOT EXISTS users (
			id TEXT PRIMARY KEY,
			username TEXT NOT NULL UNIQUE,
			email TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT 'user',
			status TEXT NOT NULL DEFAULT 'active',
			apikey_id TEXT NOT NULL DEFAULT '',
			external_id TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			agent_quota INTEGER NOT NULL DEFAULT -1,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// app_user 配置的幂等性查找位于 migrateUsersAppUserCols 中，不在此处——
		// 在现有安装上，上面的 CREATE TABLE 是无操作的，apikey_id 列尚不存在，
		// 因此索引必须等到列添加步骤运行之后。
		`CREATE TABLE IF NOT EXISTS web_sessions (
			sid TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at TIMESTAMP NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_web_sessions_user ON web_sessions (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_web_sessions_expires ON web_sessions (expires_at)`,
		// type 值："admin" | "user" | "agent"。默认值 'agent' 在现有行上保留了
		// 层级之前的行为——每个旧密钥隐式是"agent 范围"的密钥（在 apikey_agents 中的
		// 显式列表），因此迁移可以盲目回填。参见 migrateAPIKeysAddType 了解现有安装的 ALTER。
		`CREATE TABLE IF NOT EXISTS apikeys (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			key_hash TEXT NOT NULL,
			key_prefix TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'agent',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_user ON apikeys (user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_apikeys_key_hash ON apikeys (key_hash)`,
		`CREATE TABLE IF NOT EXISTS apikey_agents (
			apikey_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			PRIMARY KEY (apikey_id, agent_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_apikey_agents_agent ON apikey_agents (agent_id)`,
		// is_public 翻转"任何拥有链接的人都可以聊天"的门控。
		// 默认 0（私有——仅拥有者）。当为 1 时，非拥有者访问 agent 的聊天 URL
		// 会将 agent 懒加载到他们自己的 UserSpace 中；会话/记忆/agent 文件
		// 仍按聊天者的 user_id 键控，因此每个聊天者都拥有私有历史记录，
		// 而 agent 身份（SOUL.md, IDENTITY.md, skills）从拥有者的行共享。
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			config TEXT NOT NULL DEFAULT '{}',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_user ON agents (user_id)`,
		// channel / account_id / chat_id 一起标识会话所属的 (频道类型, 频道实例, 对话)。
		// 多个 session_key 可以共享该三元组——IM 路由的活动行是具有最新 updated_at 的行，
		// 这正是 `idx_sessions_chat_active` 加速查找的。
		// session_key 是每个会话的不透明 id（主键），独立于三元组，
		// 因此 IM 中的 `/new` 命令在同一 (channel, account_id, chat_id) 下创建新行。
		`CREATE TABLE IF NOT EXISTS sessions (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL,
			channel TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			project_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			messages TEXT NOT NULL DEFAULT '[]',
			message_count INTEGER NOT NULL DEFAULT 0,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			-- chatter_user_id 是实际的对话参与者。对于
			-- web/dashboard 聊天，它等于 user_id（即登录用户）。
			-- 对于具有按发送者 app_user 的 IM 频道，它是创建的聊天者行；
			-- user_id 保持为频道拥有者 / UserSpace 拥有者，以向后兼容
			-- 列出"我的 bot 上的所有会话"的管理员视图。在此列存在之前
			-- 写入的行上为空——读取器应在这种情况下 COALESCE 到 user_id。
			chatter_user_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (user_id, agent_id, session_key)
		)`,
		// 索引创建已移至 migrateSessionsAddChannelTriple，以便在旧数据库上
		// 在列添加 ALTER *之后*运行。如果放在这里，升级安装会尝试创建引用
		// 旧表尚不具备的列的索引。
		// session_messages 是写入会话的每一轮的仅追加存档。上面的 sessions 行
		// 存储面向 LLM 的工作集（压缩后）；session_messages 存储原始完整历史记录，
		// 因此 UI / 审计 / 多租户恢复拥有压缩从未触及的真实来源。
		// seq 是在 INSERT 时通过 COALESCE(MAX(seq), -1)+1 分配的每会话单调计数器，
		// 因此调用方不需要单独的 SELECT 往返。复合主键兼作自然排序顺序。
		`CREATE TABLE IF NOT EXISTS session_messages (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL,
			seq INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			content_parts TEXT NOT NULL DEFAULT '',
			tool_calls TEXT NOT NULL DEFAULT '',
			tool_call_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			metadata TEXT NOT NULL DEFAULT '',
			thinking TEXT NOT NULL DEFAULT '',
			raw_assistant TEXT NOT NULL DEFAULT '',
			-- origin 标记运行时注入的行（目前仅有 "goal_context"）。
			-- 空值 = 真实的用户/助手交换。
			-- WebChatHistory + FTS 跳过非空 origin 以将合成提示排除在
			-- 用户可见/可搜索的视图之外。
			origin TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			-- chatter_user_id 镜像 sessions.chatter_user_id——参见该注释了解语义。
			-- 每行存储以便按聊天者的查询无需通过 sessions 表连接。
			chatter_user_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (user_id, agent_id, session_key, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_session_messages_lookup ON session_messages (user_id, agent_id, session_key, seq)`,
		// session_events 是 agent 在一轮中发出的实时事件流
		//（内容块、tool_call、error、done）。持久化以便在轮次中
		// 刷新/重连的客户端可以从其最后看到的 seq 恢复，而不是丢失
		// 进行中的增量。seq 是按 (user, agent, session) 并且在 INSERT 时
		// 通过 COALESCE(MAX(seq),-1)+1 分配——与 session_messages 相同模式。
		// 压缩从不触及此表；行只在父会话被删除时消失（DeleteSession 级联）。
		`CREATE TABLE IF NOT EXISTS session_events (
				user_id TEXT NOT NULL,
				agent_id TEXT NOT NULL,
				session_key TEXT NOT NULL,
				seq INTEGER NOT NULL,
				type TEXT NOT NULL,
				data TEXT NOT NULL DEFAULT '',
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			-- chatter_user_id 镜像 sessions.chatter_user_id——参见该注释了解语义。
				chatter_user_id TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (user_id, agent_id, session_key, seq)
			)`,
		`CREATE INDEX IF NOT EXISTS idx_session_events_lookup ON session_events (user_id, agent_id, session_key, seq)`,
		// agent_files 保存 agent 自己的文件：SOUL.md, IDENTITY.md,
		// MEMORY.md, AGENTS.md, BOOTSTRAP.md 等。
		//
		// user_id 将"agent 模板"与"每用户覆盖"分开：
		//   user_id='' — 共享模板，由 agent 拥有者通过 Customize 页面编辑，
		//                对未创建自己覆盖的每个聊天者可见
		//   user_id=u_xxx — 该用户的个人副本（聊天期间的 USER.md / MEMORY.md，
		//                或"为我个性化"覆盖）
		// 读取路径选择 `user_id IN (chatter, '') ORDER BY user_id DESC LIMIT 1`，
		// 因此用户特定行获胜，缺失的行回退到模板。
		`CREATE TABLE IF NOT EXISTS agent_files (
			agent_id TEXT NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			filename TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (agent_id, user_id, filename)
		)`,
		// configs 使用 (user_id, agent_id) 作为所有权对，与
		// agent_files / sessions / session_messages / session_events 匹配。
		// 旧的 (scope, scope_id) 对已消失——scope 是多余的，因为 (user_id, agent_id)
		// 组合已经编码了它：
		//   ('', '')   = 系统 / 全局
		//   (X, '')    = 用户 X 的私有配置
		//   ('', Y)    = agent Y 的"官方"配置（任何使用 Y 的人都继承）
		//   (X, Y)     = 用户 X 在 agent Y 上的每个 agent 覆盖——多租户场景；
		//                允许共享公共 agent 的两个用户分别绑定自己的频道。
		`CREATE TABLE IF NOT EXISTS configs (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			-- scope 是从 (user_id, agent_id) 派生的反规范化
			-- 'system'|'user'|'agent'|'user-agent' 标签。SaveConfig 在
			-- 每次 upsert 时写入它；没有其他写入者。保留用于数据库转储
			-- 可读性和临时管理员查询。
			scope TEXT NOT NULL DEFAULT '',
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			credential_key TEXT NOT NULL DEFAULT '',
			data TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE (kind, user_id, agent_id, name)
		)`,
		// idx_configs_lookup 的创建已移至 migrateConfigsScopeToUserAgent，
		// 以便在旧数据库上在列添加步骤之后运行（在 migrationSQL 的这个点上
		// 它引用的列尚不存在）。新安装通过迁移器内的 IF NOT EXISTS 路径
		// 仍然获得该索引。
		`CREATE INDEX IF NOT EXISTS idx_configs_credential ON configs (kind, credential_key)`,
		`CREATE TABLE IF NOT EXISTS cron_jobs (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'cron',
			schedule TEXT NOT NULL,
			message TEXT NOT NULL,
			channel TEXT NOT NULL,
			chat_id TEXT NOT NULL,
			account_id TEXT NOT NULL DEFAULT '',
			timezone TEXT NOT NULL DEFAULT 'UTC',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			last_run TIMESTAMP,
			next_run TIMESTAMP,
			locked_by TEXT,
			locked_at TIMESTAMP,
			-- failure_count 追踪连续触发尝试中目标频道缺失/不可达的次数。
			-- cron 调度器在每次失败时递增它，并在超过阈值后自行删除该行，
			-- 这样死掉的 bot 就不会永远记录日志。
			failure_count INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		// idx_cron_jobs_user 的创建已移至 migrateCronJobsAddUserID，
		// 以便旧安装在其列添加 ALTER 之后命中它。新安装通过 Migrate 的
		// 完整扫描到达相同的代码路径。
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_schedule ON cron_jobs (enabled, next_run)`,
		`CREATE INDEX IF NOT EXISTS idx_cron_jobs_agent ON cron_jobs (agent_id)`,
		// projects 对共享工作区文件夹的会话进行分组。主键匹配 sessions：
		// 项目是"用户 X 在 agent Y 上的工作文件夹"，同一每用户私有所有权模型。
		// 磁盘上的工作区目录位于 workspaces/<agent>/projects/<pid>/，
		// 由 project_id 等于 pid 的每个会话共享；对于项目会话，
		// 按会话的 sessions/<chat>/ 子目录被绕过，因此文件在项目内的
		// 聊天之间持久存在。
		`CREATE TABLE IF NOT EXISTS projects (
			user_id TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			project_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			description TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (user_id, agent_id, project_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_projects_listing ON projects (user_id, agent_id, updated_at DESC)`,
		// agent_goals 支持 /goal 功能：每个 (agent, session) 一个持久目标。
		// UNIQUE (agent_id, session_key) 约束是"此会话已有目标"的真实来源——
		// CreateGoal 将冲突转换为 ErrGoalAlreadyExists。
		`CREATE TABLE IF NOT EXISTS agent_goals (
			id TEXT PRIMARY KEY,
			agent_id TEXT NOT NULL,
			session_key TEXT NOT NULL,
			owner_user_id TEXT NOT NULL,
			-- 路由元组，在创建时标记，以便延续可以发布到原始轮次到达的
			-- 相同总线地址。镜像 cron_jobs 的 channel/chat_id 列。
			channel TEXT NOT NULL DEFAULT '',
			account_id TEXT NOT NULL DEFAULT '',
			chat_id TEXT NOT NULL DEFAULT '',
			project_id TEXT NOT NULL DEFAULT '',
			objective TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			token_budget BIGINT,
			tokens_used BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_goals_session ON agent_goals (agent_id, session_key)`,
		// token_usage_daily 是管理员 Usage 仪表板背后的按 (day, user, agent, session,
		// provider, model) 计数器。每次成功的 LLM Chat / ChatStream 通过 UPSERT
		// 产生一行——参见 internal/usage.SQLMeter。空 user_id 在写入时保留
		//（管理员拥有或 cron 触发的 agent）并在 UI 中显示为"system"。Provider 是
		// 每个 agent 的覆盖键（例如 "anthropic-messages"）；"" 表示 agent 使用了
		// 共享提供商且没有覆盖。主键是六元组，因此对任何子集的 GROUP BY
		// 都能干净地聚合，无需额外索引。
		`CREATE TABLE IF NOT EXISTS token_usage_daily (
			day DATE NOT NULL,
			user_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			session_key TEXT NOT NULL DEFAULT '',
			provider TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			input_tokens BIGINT NOT NULL DEFAULT 0,
			output_tokens BIGINT NOT NULL DEFAULT 0,
			cache_read_tokens BIGINT NOT NULL DEFAULT 0,
			cache_create_tokens BIGINT NOT NULL DEFAULT 0,
			request_count BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (day, user_id, agent_id, session_key, provider, model)
		)`,
		// 按 day 的范围扫描是主要查询（24h/7d/30d 过滤器）
		// — 主键以 day 开头，因此 SQLite/Postgres 都使用它而无需
		// 二级索引。下面的额外索引在表增长时加速非时间前缀查找
		//（例如 "agent X 在所有时间内的所有行"）。
		`CREATE INDEX IF NOT EXISTS idx_token_usage_agent ON token_usage_daily (agent_id, day)`,
		`CREATE INDEX IF NOT EXISTS idx_token_usage_user ON token_usage_daily (user_id, day)`,
		// channel_leases 将轮询/持久连接频道适配器（微信、Telegram、Discord、
		// Slack、飞书长连接）限制为一次一个进程。没有它，共享同一 bot 令牌的
		// 两个云副本都将长轮询上游服务器，用户将收到每个回复两次。
		// 租约持有者定期续约；崩溃时租约过期，另一个实例接管。
		// 参见 channels.Manager 和 channels.runWithLease。
		`CREATE TABLE IF NOT EXISTS channel_leases (
			channel TEXT NOT NULL,
			account_id TEXT NOT NULL,
			holder_id TEXT NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			PRIMARY KEY (channel, account_id)
		)`,
	}
}

func (d *DBStore) Close() error {
	return d.db.Close()
}

// ph 返回适用于当前方言的正确占位符。
func (d *DBStore) ph(n int) string {
	if d.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

// scanErr 将 sql.ErrNoRows 包装到我们的公共 ErrNotFound 中。
func scanErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

var _ Store = (*DBStore)(nil)
