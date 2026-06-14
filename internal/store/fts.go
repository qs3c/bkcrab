package store

import (
	"database/sql"
	"fmt"
	"time"
)

// FTSStore 管理用于会话历史记录的SQLite FTS5索引。
type FTSStore struct {
	db *sql.DB
}

// FTSResult 是单个全文搜索命中结果。
type FTSResult struct {
	Content   string
	Timestamp time.Time
	AgentID   string
	ChatID    string
	Snippet   string  // FTS5 snippet() 函数输出
	Rank      float64
}

// NewFTSStore 打开（或创建）位于dbPath的SQLite数据库并返回一个FTSStore实例。
func NewFTSStore(dbPath string) (*FTSStore, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open fts db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite 单写入者
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping fts db: %w", err)
	}
	return &FTSStore{db: db}, nil
}

// Init 创建FTS5虚拟表和影子内容表（如果它们尚不存在）。
func (f *FTSStore) Init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS messages_content (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			content TEXT NOT NULL,
			timestamp TEXT NOT NULL,
			agent_id TEXT NOT NULL,
			chat_id TEXT NOT NULL
		)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			content,
			timestamp,
			agent_id,
			chat_id,
			content='messages_content',
			content_rowid='id',
			tokenize='porter unicode61'
		)`,
		// 触发器用于保持FTS与内容表同步
		`CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages_content BEGIN
			INSERT INTO messages_fts(rowid, content, timestamp, agent_id, chat_id)
			VALUES (new.id, new.content, new.timestamp, new.agent_id, new.chat_id);
		END`,
		`CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages_content BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, content, timestamp, agent_id, chat_id)
			VALUES ('delete', old.id, old.content, old.timestamp, old.agent_id, old.chat_id);
		END`,
	}
	for _, stmt := range stmts {
		if _, err := f.db.Exec(stmt); err != nil {
			return fmt.Errorf("fts init: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

// Index 将一条消息添加到FTS索引中。
func (f *FTSStore) Index(agentID, chatID, role, content string, ts time.Time) error {
	_, err := f.db.Exec(
		`INSERT INTO messages_content (content, timestamp, agent_id, chat_id) VALUES (?, ?, ?, ?)`,
		role+": "+content, ts.Format(time.RFC3339), agentID, chatID,
	)
	return err
}

// Search 执行全文搜索并返回排序后的结果。
func (f *FTSStore) Search(query string, limit int) ([]FTSResult, error) {
	if limit <= 0 {
		limit = 10
	}

	rows, err := f.db.Query(
		`SELECT
			snippet(messages_fts, 0, '<b>', '</b>', '...', 32) AS snippet,
			messages_fts.content,
			messages_fts.timestamp,
			messages_fts.agent_id,
			messages_fts.chat_id,
			rank
		FROM messages_fts
		WHERE messages_fts MATCH ?
		ORDER BY rank
		LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("fts search: %w", err)
	}
	defer rows.Close()

	var results []FTSResult
	for rows.Next() {
		var r FTSResult
		var tsStr string
		if err := rows.Scan(&r.Snippet, &r.Content, &tsStr, &r.AgentID, &r.ChatID, &r.Rank); err != nil {
			continue
		}
		r.Timestamp, _ = time.Parse(time.RFC3339, tsStr)
		results = append(results, r)
	}
	return results, nil
}

// Close 关闭底层数据库。
func (f *FTSStore) Close() error {
	return f.db.Close()
}
