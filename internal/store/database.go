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
	"strings"
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
	if err := d.migrateSessionMessagesAddTurnColumns(ctx); err != nil {
		return fmt.Errorf("migrate session_messages turn columns: %w", err)
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

// migrateSessionMessagesAddTurnColumns 将 turn_status / extraction_id 列改装到
// 旧的 session_messages 表上,并建立"待提取"部分索引。turn_status 默认 ''
// (非锚点),extraction_id 默认 NULL(未提取),历史存量行无需回填。幂等。
func (d *DBStore) migrateSessionMessagesAddTurnColumns(ctx context.Context) error {
	statusType, idType := "TEXT", "TEXT"
	if d.dialect == mysqlDialect {
		statusType, idType = "VARCHAR(16)", "VARCHAR(64)"
	}
	if has, err := d.tableHasColumn(ctx, "session_messages", "turn_status"); err != nil {
		return err
	} else if !has {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN turn_status %s NOT NULL DEFAULT ''`, statusType)); err != nil {
			return fmt.Errorf("add turn_status: %w", err)
		}
	}
	if has, err := d.tableHasColumn(ctx, "session_messages", "extraction_id"); err != nil {
		return err
	} else if !has {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`ALTER TABLE session_messages ADD COLUMN extraction_id %s`, idType)); err != nil {
			return fmt.Errorf("add extraction_id: %w", err)
		}
	}
	// 待提取索引:SQLite/PG 用部分索引;MySQL 不支持,降级为普通复合索引。
	idx := `CREATE INDEX IF NOT EXISTS idx_sm_pending ON session_messages (agent_id, chatter_user_id) WHERE turn_status = 'done' AND extraction_id IS NULL`
	if d.dialect == mysqlDialect {
		idx = `CREATE INDEX IF NOT EXISTS idx_sm_pending ON session_messages (agent_id, chatter_user_id, turn_status)`
	}
	if err := d.execDDL(ctx, idx); err != nil {
		return fmt.Errorf("create idx_sm_pending: %w", err)
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
			-- turn_status 标记锚点行(代表一个完整 turn 的用户消息)：
			-- '' = 非锚点(steer / 群聊注入 / goal_context / 历史存量行),
			-- 'running' = 锚点,turn 进行中, 'done' = 锚点,turn 已完成。
			turn_status TEXT NOT NULL DEFAULT '',
			-- extraction_id：NULL = 未被任何记忆提取认领；非 NULL(uuid)= 已认领。
			extraction_id TEXT,
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

// --- Web sessions ---

func (d *DBStore) CreateWebSession(ctx context.Context, s *WebSessionRecord) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO web_sessions (sid, user_id, created_at, expires_at) VALUES (%s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		s.SID, s.UserID, s.CreatedAt, s.ExpiresAt)
	return err
}

func (d *DBStore) GetWebSession(ctx context.Context, sid string) (*WebSessionRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT sid, user_id, created_at, expires_at FROM web_sessions WHERE sid = %s`, d.ph(1)), sid)
	var s WebSessionRecord
	if err := row.Scan(&s.SID, &s.UserID, &s.CreatedAt, &s.ExpiresAt); err != nil {
		return nil, scanErr(err)
	}
	return &s, nil
}

func (d *DBStore) DeleteWebSession(ctx context.Context, sid string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM web_sessions WHERE sid = %s`, d.ph(1)), sid)
	return err
}

func (d *DBStore) DeleteExpiredWebSessions(ctx context.Context, before time.Time) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM web_sessions WHERE expires_at < %s`, d.ph(1)), before)
	return err
}

// --- API keys ---

func (d *DBStore) ListAPIKeys(ctx context.Context, userID string) ([]APIKeyRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, name, key_hash, key_prefix, type, created_at FROM apikeys WHERE user_id = %s ORDER BY created_at`, d.ph(1)),
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKeyRecord
	for rows.Next() {
		var ak APIKeyRecord
		if err := rows.Scan(&ak.ID, &ak.UserID, &ak.Name, &ak.KeyHash, &ak.KeyPrefix, &ak.Type, &ak.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, ak)
	}
	return out, rows.Err()
}

func (d *DBStore) GetAPIKey(ctx context.Context, id string) (*APIKeyRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, name, key_hash, key_prefix, type, created_at FROM apikeys WHERE id = %s`, d.ph(1)), id)
	var ak APIKeyRecord
	if err := row.Scan(&ak.ID, &ak.UserID, &ak.Name, &ak.KeyHash, &ak.KeyPrefix, &ak.Type, &ak.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &ak, nil
}

func (d *DBStore) CreateAPIKey(ctx context.Context, ak *APIKeyRecord) error {
	if ak.CreatedAt.IsZero() {
		ak.CreatedAt = time.Now().UTC()
	}
	if ak.Type == "" {
		ak.Type = "agent"
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO apikeys (id, user_id, name, key_hash, key_prefix, type, created_at) VALUES (%s, %s, %s, %s, %s, %s, %s)`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7)),
		ak.ID, ak.UserID, ak.Name, ak.KeyHash, ak.KeyPrefix, ak.Type, ak.CreatedAt)
	return err
}

func (d *DBStore) DeleteAPIKey(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikey_agents WHERE apikey_id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikeys WHERE id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) RotateAPIKey(ctx context.Context, id, keyHash, keyPrefix string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE apikeys SET key_hash = %s, key_prefix = %s WHERE id = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		keyHash, keyPrefix, id)
	return err
}

func (d *DBStore) LookupAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT id, user_id, name, key_hash, key_prefix, type, created_at FROM apikeys WHERE key_hash = %s`, d.ph(1)),
		keyHash)
	var ak APIKeyRecord
	if err := row.Scan(&ak.ID, &ak.UserID, &ak.Name, &ak.KeyHash, &ak.KeyPrefix, &ak.Type, &ak.CreatedAt); err != nil {
		return nil, scanErr(err)
	}
	return &ak, nil
}

// --- API key ↔ agent permissions ---

func (d *DBStore) SetAPIKeyAgents(ctx context.Context, apikeyID string, agentIDs []string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM apikey_agents WHERE apikey_id = %s`, d.ph(1)), apikeyID); err != nil {
		return err
	}
	for _, aid := range agentIDs {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO apikey_agents (apikey_id, agent_id) VALUES (%s, %s)`, d.ph(1), d.ph(2)),
			apikeyID, aid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DBStore) ListAPIKeyAgents(ctx context.Context, apikeyID string) ([]string, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT agent_id FROM apikey_agents WHERE apikey_id = %s ORDER BY agent_id`, d.ph(1)),
		apikeyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var aid string
		if err := rows.Scan(&aid); err != nil {
			return nil, err
		}
		out = append(out, aid)
	}
	return out, rows.Err()
}

func (d *DBStore) APIKeyCanAccessAgent(ctx context.Context, apikeyID, agentID string) (bool, error) {
	var n int
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM apikey_agents WHERE apikey_id = %s AND agent_id = %s`, d.ph(1), d.ph(2)),
		apikeyID, agentID).Scan(&n)
	return n > 0, err
}

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

// --- Sessions ---

func (d *DBStore) GetSession(ctx context.Context, userID, agentID, sessionKey string) (*SessionRecord, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT messages, channel, account_id, chat_id, project_id, updated_at FROM sessions WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	var msgsStr string
	var rec SessionRecord
	if err := row.Scan(&msgsStr, &rec.Channel, &rec.AccountID, &rec.ChatID, &rec.ProjectID, &rec.UpdatedAt); err != nil {
		return nil, scanErr(err)
	}
	json.Unmarshal([]byte(msgsStr), &rec.Messages)
	return &rec, nil
}

// SaveSession 对会话行进行 upsert。Channel / AccountID / ChatID /
// ProjectID 仅在 INSERT 时写入；ON CONFLICT 分支故意保留现有值，
// 以便不知道三元组的回调（例如压缩调用 ReplaceMessages）不会意外清除它。
func (d *DBStore) SaveSession(ctx context.Context, userID, agentID, sessionKey string, session *SessionRecord) error {
	if userID == "" {
		return errors.New("store: SaveSession requires user_id")
	}
	msgsData, _ := json.Marshal(session.Messages)
	now := time.Now().UTC()
	count := len(session.Messages)
	// 每轮的聊天者（= 实际的对话参与者）通过 ctx 传递，因此此签名保持向后兼容。
	// 当没有上游调用者标记 ctx 时为空——读取器回退到 user_id。
	chatterID := ChatterUserIDFromContext(ctx)
	if d.dialect == mysqlDialect {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO sessions (user_id, agent_id, session_key, channel, account_id, chat_id, project_id, messages, message_count, updated_at, chatter_user_id)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  messages=VALUES(messages), message_count=VALUES(message_count), updated_at=VALUES(updated_at),
				  chatter_user_id=CASE
					WHEN VALUES(chatter_user_id) <> '' THEN VALUES(chatter_user_id)
					ELSE chatter_user_id
				  END`,
			userID, agentID, sessionKey, session.Channel, session.AccountID, session.ChatID, session.ProjectID,
			string(msgsData), count, now, chatterID)
		return err
	}
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO sessions (user_id, agent_id, session_key, channel, account_id, chat_id, project_id, messages, message_count, updated_at, chatter_user_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				ON CONFLICT (user_id, agent_id, session_key) DO UPDATE
				SET messages=$8, message_count=$9, updated_at=$10,
				    chatter_user_id = CASE WHEN $11 <> '' THEN $11 ELSE sessions.chatter_user_id END`,
			userID, agentID, sessionKey, session.Channel, session.AccountID, session.ChatID, session.ProjectID,
			string(msgsData), count, now, chatterID)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions (user_id, agent_id, session_key, channel, account_id, chat_id, project_id, messages, message_count, updated_at, chatter_user_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (user_id, agent_id, session_key) DO UPDATE SET
			  messages=excluded.messages, message_count=excluded.message_count, updated_at=excluded.updated_at,
			  chatter_user_id = CASE WHEN excluded.chatter_user_id <> '' THEN excluded.chatter_user_id ELSE sessions.chatter_user_id END`,
		userID, agentID, sessionKey, session.Channel, session.AccountID, session.ChatID, session.ProjectID,
		string(msgsData), count, now, chatterID)
	return err
}

func (d *DBStore) ListSessions(ctx context.Context, userID, agentID string) ([]SessionMeta, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT session_key, channel, account_id, chat_id, project_id, title, message_count, updated_at FROM sessions
			WHERE user_id = %s AND agent_id = %s ORDER BY updated_at DESC`, d.ph(1), d.ph(2)),
		userID, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var metas []SessionMeta
	for rows.Next() {
		var m SessionMeta
		if err := rows.Scan(&m.Key, &m.Channel, &m.AccountID, &m.ChatID, &m.ProjectID, &m.Title, &m.MessageCount, &m.UpdatedAt); err != nil {
			return nil, err
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

// ListSessionOwnerPairs 枚举 sessions 表中每个不同的 (user_id, agent_id)
// 元组。管理员 Chats 页面调用此函数来查找所有 agent 上的所有对话拥有者
//（聊天者/绑定者）——按 (拥有者, agent) 的 ListSessions 会遗漏非拥有者用户
// 与公共 agent 聊天或绑定 IM bot 的会话，因为这些行位于聊天者的 user_id 下，
// 而不是 agent 拥有者的 user_id 下。
func (d *DBStore) ListSessionOwnerPairs(ctx context.Context) ([]SessionOwnerPair, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT DISTINCT user_id, agent_id FROM sessions
			WHERE user_id <> '' AND agent_id <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pairs []SessionOwnerPair
	for rows.Next() {
		var p SessionOwnerPair
		if err := rows.Scan(&p.UserID, &p.AgentID); err != nil {
			return nil, err
		}
		pairs = append(pairs, p)
	}
	return pairs, rows.Err()
}

// LookupSessionTriple 是 ResolveActiveSessionKey 的逆操作：给定一个
// session_key（规范行 id），返回它所属的 (channel, accountID, chatID)。
// 被从 URL 获取 session_key 并需要原始 chat_id 的处理程序使用——例如，
// 保持工作区文件按对话而非会话命名空间。
func (d *DBStore) LookupSessionTriple(ctx context.Context, userID, agentID, sessionKey string) (string, string, string, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT channel, account_id, chat_id FROM sessions
			WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	var ch, acc, ci string
	if err := row.Scan(&ch, &acc, &ci); err != nil {
		return "", "", "", scanErr(err)
	}
	return ch, acc, ci, nil
}

// LookupSessionProject 返回 session_key 的 project_id（或 ""）
// — 工作区路径解析器查阅此值来决定沙箱挂载时使用 projects/<id>/
// 还是 sessions/<chat>/。
func (d *DBStore) LookupSessionProject(ctx context.Context, userID, agentID, sessionKey string) (string, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT project_id FROM sessions
			WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	var pid string
	if err := row.Scan(&pid); err != nil {
		return "", scanErr(err)
	}
	return pid, nil
}

// ResolveActiveSessionKey 返回 (user, agent) 内 (channel, account_id, chat_id)
// 三元组中最近更新的 session_key，或 ErrNotFound。该三元组是 IM 路由的自然地址——
// IM 适配器本身不携带会话 id，因此网关在消息到达时选择最新的线程。`/new` 创建新行，
// 该行随后在后续解析中赢得 ORDER BY。
func (d *DBStore) ResolveActiveSessionKey(ctx context.Context, userID, agentID, channel, accountID, chatID string) (string, error) {
	row := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT session_key FROM sessions
			WHERE user_id = %s AND agent_id = %s
			  AND channel = %s AND account_id = %s AND chat_id = %s
			ORDER BY updated_at DESC LIMIT 1`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		userID, agentID, channel, accountID, chatID)
	var key string
	if err := row.Scan(&key); err != nil {
		return "", scanErr(err)
	}
	return key, nil
}

func (d *DBStore) DeleteSession(ctx context.Context, userID, agentID, sessionKey string) error {
	for _, t := range []string{"session_messages", "session_events"} {
		if _, err := d.db.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
				t, d.ph(1), d.ph(2), d.ph(3)),
			userID, agentID, sessionKey); err != nil {
			return err
		}
	}
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM sessions WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	return err
}

// AppendSessionMessage 将一条消息写入每会话存档。
// seq 通过 `COALESCE(MAX(seq), -1) + 1` 在 INSERT 内原子性地计算，
// 因此两个在同一个会话上竞争的并发追加者不会在唯一键上冲突——
// 第二个插入在第一个提交后读取 MAX。多 pod 安全性依赖于引擎的写入序列化
//（sqlite 全局锁，postgres MVCC + 提交时的复合主键唯一性检查）。
func (d *DBStore) AppendSessionMessage(ctx context.Context, userID, agentID, sessionKey string, msg SessionMessage) error {
	if userID == "" {
		return errors.New("store: AppendSessionMessage requires user_id")
	}
	contentParts, _ := json.Marshal(msg.ContentParts)
	toolCalls, _ := json.Marshal(msg.ToolCalls)
	metadata, _ := json.Marshal(msg.Metadata)
	rawAssistant := string(msg.RawAssistant)
	ts := msg.Timestamp
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	chatterID := ChatterUserIDFromContext(ctx)
	if d.dialect == "postgres" {
		_, err := d.db.ExecContext(ctx,
			`INSERT INTO session_messages
				(user_id, agent_id, session_key, seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at, chatter_user_id)
			SELECT $1, $2, $3, COALESCE(MAX(seq), -1) + 1, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
				FROM session_messages
				WHERE user_id = $1 AND agent_id = $2 AND session_key = $3`,
			userID, agentID, sessionKey,
			msg.Role, msg.Content, string(contentParts), string(toolCalls),
			msg.ToolCallID, msg.Name, string(metadata), msg.Thinking, rawAssistant, msg.Origin, ts, chatterID)
		return err
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO session_messages
			(user_id, agent_id, session_key, seq, role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at, chatter_user_id)
		SELECT ?, ?, ?, COALESCE(MAX(seq), -1) + 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
			FROM session_messages
			WHERE user_id = ? AND agent_id = ? AND session_key = ?`,
		userID, agentID, sessionKey,
		msg.Role, msg.Content, string(contentParts), string(toolCalls),
		msg.ToolCallID, msg.Name, string(metadata), msg.Thinking, rawAssistant, msg.Origin, ts, chatterID,
		userID, agentID, sessionKey)
	return err
}

// AppendSessionEvent 持久化一个流式事件增量并返回分配的 seq。
// seq 按 (user, agent, session) 分配——与 session_messages 相同模式——
// 并在事务内原子性地分配，以便并发追加者（例如扇出 + 重放）不会在主键上冲突。
// 被重连客户端用来跳过它们已经渲染过的事件。
func (d *DBStore) AppendSessionEvent(ctx context.Context, userID, agentID, sessionKey, eventType string, data []byte) (int64, error) {
	if userID == "" || agentID == "" || sessionKey == "" {
		return 0, errors.New("store: AppendSessionEvent requires user_id, agent_id, session_key")
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var seq int64
	chatterID := ChatterUserIDFromContext(ctx)
	if d.dialect == "postgres" {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM session_events
				WHERE user_id = $1 AND agent_id = $2 AND session_key = $3`,
			userID, agentID, sessionKey).Scan(&seq); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_events (user_id, agent_id, session_key, seq, type, data, created_at, chatter_user_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			userID, agentID, sessionKey, seq, eventType, string(data), time.Now().UTC(), chatterID); err != nil {
			return 0, err
		}
	} else {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM session_events
				WHERE user_id = ? AND agent_id = ? AND session_key = ?`,
			userID, agentID, sessionKey).Scan(&seq); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_events (user_id, agent_id, session_key, seq, type, data, created_at, chatter_user_id)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			userID, agentID, sessionKey, seq, eventType, string(data), time.Now().UTC(), chatterID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// ListSessionEventsSince 返回 seq 严格大于 sinceSeq 的每个聊天事件，按升序排列。
// 传递 sinceSeq=-1 以获取所有事件。
func (d *DBStore) ListSessionEventsSince(ctx context.Context, userID, agentID, sessionKey string, sinceSeq int64) ([]SessionEventRecord, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT seq, type, data, created_at FROM session_events
			WHERE user_id = %s AND agent_id = %s AND session_key = %s AND seq > %s
			ORDER BY seq ASC`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		userID, agentID, sessionKey, sinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionEventRecord
	for rows.Next() {
		var rec SessionEventRecord
		var dataStr string
		if err := rows.Scan(&rec.Seq, &rec.Type, &dataStr, &rec.CreatedAt); err != nil {
			return nil, err
		}
		rec.UserID = userID
		rec.AgentID = agentID
		rec.SessionKey = sessionKey
		if dataStr != "" {
			rec.Data = []byte(dataStr)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// LatestSessionEventSeq 返回会话的最高 seq，如果没有则返回 -1。
// 通过聊天历史响应暴露给客户端，以便他们在新页面加载时知道从哪里订阅。
func (d *DBStore) LatestSessionEventSeq(ctx context.Context, userID, agentID, sessionKey string) (int64, error) {
	var seq sql.NullInt64
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT MAX(seq) FROM session_events
			WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey).Scan(&seq)
	if err != nil {
		return -1, err
	}
	if !seq.Valid {
		return -1, nil
	}
	return seq.Int64, nil
}

// ListSessionMessages 按升序 seq 顺序返回一个会话的每个存档轮次。
// 对于尚未有存档的会话（例如早于该表的行）返回空切片。
// 想要回退到 sessions.messages 的调用方应检查 len() 并决定。
func (d *DBStore) ListSessionMessages(ctx context.Context, userID, agentID, sessionKey string) ([]SessionMessage, error) {
	rows, err := d.db.QueryContext(ctx,
		fmt.Sprintf(`SELECT role, content, content_parts, tool_calls, tool_call_id, name, metadata, thinking, raw_assistant, origin, created_at
			FROM session_messages
			WHERE user_id = %s AND agent_id = %s AND session_key = %s
			ORDER BY seq ASC`, d.ph(1), d.ph(2), d.ph(3)),
		userID, agentID, sessionKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionMessage
	for rows.Next() {
		var m SessionMessage
		var contentParts, toolCalls, metadata, rawAssistant string
		if err := rows.Scan(&m.Role, &m.Content, &contentParts, &toolCalls, &m.ToolCallID, &m.Name, &metadata, &m.Thinking, &rawAssistant, &m.Origin, &m.Timestamp); err != nil {
			return nil, err
		}
		if contentParts != "" && contentParts != "null" {
			var v interface{}
			if json.Unmarshal([]byte(contentParts), &v) == nil {
				m.ContentParts = v
			}
		}
		if toolCalls != "" && toolCalls != "null" {
			var v interface{}
			if json.Unmarshal([]byte(toolCalls), &v) == nil {
				m.ToolCalls = v
			}
		}
		if metadata != "" && metadata != "null" {
			_ = json.Unmarshal([]byte(metadata), &m.Metadata)
		}
		if rawAssistant != "" && rawAssistant != "null" {
			m.RawAssistant = json.RawMessage(rawAssistant)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountChatterUserMessages 返回此聊天者在 agent 下所有会话中积累的
// user 角色消息计数。被 autoPersist 节奏门控用作持久计数器——参见 Store
// 上的接口文档了解为什么不重用内存中的 turnCount。
//
// 过滤器严格在 chatter_user_id 上（不回退到 user_id）。在 chatter_user_id
// 列存在之前写入的旧行将其设置为 '' 并且不被计数；那些行早于按聊天者解析，
// 将它们纳入会过度计数（它们按频道拥有者键控，而不是实际的聊天者）。
// 新对话正确写入 chatter_user_id，因此这仅适用于从修复前的守护进程运行迁移的会话。
func (d *DBStore) CountChatterUserMessages(ctx context.Context, agentID, chatterUserID string) (int, error) {
	if chatterUserID == "" {
		return 0, nil
	}
	var n int
	err := d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM session_messages
			WHERE agent_id = %s AND chatter_user_id = %s AND role = 'user'`,
			d.ph(1), d.ph(2)),
		agentID, chatterUserID).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func (d *DBStore) RenameSession(ctx context.Context, userID, agentID, sessionKey, title string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE sessions SET title = %s WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		title, userID, agentID, sessionKey)
	return err
}

// MoveSession 翻转会话的 project_id。空字符串将会话从其当前项目中分离
//（拖出到"Chats"）。调用方必须已经迁移了工作区文件并验证了 projectID
//（当非空时）是用户在此 agent 下拥有的真实项目——此方法仅影响 sessions 行。
func (d *DBStore) MoveSession(ctx context.Context, userID, agentID, sessionKey, projectID string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE sessions SET project_id = %s WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		projectID, userID, agentID, sessionKey)
	return err
}

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
//（例如 Customize 页面区分"你已创建覆盖"与"你正在查看拥有者的内容"）。
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

// --- 频道租约 ---
//
// 轮询/持久连接频道适配器的跨进程单例门控。模式是每个 (channel, account_id)
// 一行；持有者将其 instanceID 写入 holder_id 并在周期性 tick 上续约 expires_at。
// 想要接管的对等方必须等到 expires_at 过去——此时相同的 upsert 查询原子性地
// 将该行旋转到新持有者。

// AcquireChannelLease 尝试获取 (channel, accountID) 的 `ttl` 租约。
// 仅当行不存在、已由 holderID 持有（续约）或已过期（抢占）时返回 true。
// 在竞争中失败的并发获取者得到 (false, nil)——不是错误。
func (d *DBStore) AcquireChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	now := time.Now()
	expires := now.Add(ttl)
	if d.dialect == mysqlDialect {
		res, err := d.db.ExecContext(ctx,
			`INSERT INTO channel_leases (channel, account_id, holder_id, expires_at)
				VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE
				  holder_id=IF(channel_leases.expires_at < ?, VALUES(holder_id), channel_leases.holder_id),
				  expires_at=IF(channel_leases.holder_id = VALUES(holder_id), VALUES(expires_at), channel_leases.expires_at)`,
			channel, accountID, holderID, expires, now)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	if d.dialect == "postgres" {
		// ON CONFLICT 仅在前持有者的租约已过期或我们已持有时（续约）更新行。
		// WHERE 子句至关重要——没有它，第二个实例会在其 INSERT 冲突的瞬间窃取租约。
		res, err := d.db.ExecContext(ctx,
			`INSERT INTO channel_leases (channel, account_id, holder_id, expires_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (channel, account_id) DO UPDATE
				SET holder_id = EXCLUDED.holder_id, expires_at = EXCLUDED.expires_at
				WHERE channel_leases.expires_at < $5 OR channel_leases.holder_id = $3`,
			channel, accountID, holderID, expires, now)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}
	// SQLite 路径：ON CONFLICT DO UPDATE ... WHERE 在 modernc.org/sqlite
	//（SQLite 3.24+）中支持。语义与上面的 PG 分支相同；占位符语法不同。
	res, err := d.db.ExecContext(ctx,
		`INSERT INTO channel_leases (channel, account_id, holder_id, expires_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (channel, account_id) DO UPDATE
			SET holder_id = excluded.holder_id, expires_at = excluded.expires_at
			WHERE channel_leases.expires_at < ? OR channel_leases.holder_id = ?`,
		channel, accountID, holderID, expires, now, holderID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RenewChannelLease 扩展已持有的租约。当行的 holder_id 不再匹配时返回 false
//（不是错误）——意味着前持有者的 TTL 已过，并且在对等方在我们离线时接管了。
// 调用方必须将 false 视为"立即停止轮询"：对等方现在正在驱动此 (channel, account_id)
// 对的入站流量。
func (d *DBStore) RenewChannelLease(ctx context.Context, channel, accountID, holderID string, ttl time.Duration) (bool, error) {
	expires := time.Now().Add(ttl)
	var res sql.Result
	var err error
	if d.dialect == "postgres" {
		res, err = d.db.ExecContext(ctx,
			`UPDATE channel_leases SET expires_at = $1
				WHERE channel = $2 AND account_id = $3 AND holder_id = $4`,
			expires, channel, accountID, holderID)
	} else {
		res, err = d.db.ExecContext(ctx,
			`UPDATE channel_leases SET expires_at = ?
				WHERE channel = ? AND account_id = ? AND holder_id = ?`,
			expires, channel, accountID, holderID)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReleaseChannelLease 自愿放弃租约，以便对等方可以在其下一次获取尝试中
// 接手而无需等待 TTL。受 holder_id 限制，因此来自被驱逐实例的过时 Release
// 不会意外地使当前持有者的行失效。
func (d *DBStore) ReleaseChannelLease(ctx context.Context, channel, accountID, holderID string) error {
	var err error
	if d.dialect == "postgres" {
		_, err = d.db.ExecContext(ctx,
			`DELETE FROM channel_leases WHERE channel = $1 AND account_id = $2 AND holder_id = $3`,
			channel, accountID, holderID)
	} else {
		_, err = d.db.ExecContext(ctx,
			`DELETE FROM channel_leases WHERE channel = ? AND account_id = ? AND holder_id = ?`,
			channel, accountID, holderID)
	}
	return err
}

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

// parseTimeString 尝试 modernc.org/sqlite 可能为 TIMESTAMP 列生成的常见时间格式
//（RFC3339, RFC3339Nano 以及旧代码路径写入的 Go 默认格式）。
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
//（子串 "UNIQUE constraint failed"）中的 UNIQUE 约束违规。两个驱动程序在错误文本中
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

var _ Store = (*DBStore)(nil)
