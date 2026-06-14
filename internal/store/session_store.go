package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

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
// （聊天者/绑定者）——按 (拥有者, agent) 的 ListSessions 会遗漏非拥有者用户
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
// （sqlite 全局锁，postgres MVCC + 提交时的复合主键唯一性检查）。
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
// 列存在之前写入的旧行将其设置为 ” 并且不被计数；那些行早于按聊天者解析，
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
// （拖出到"Chats"）。调用方必须已经迁移了工作区文件并验证了 projectID
// （当非空时）是用户在此 agent 下拥有的真实项目——此方法仅影响 sessions 行。
func (d *DBStore) MoveSession(ctx context.Context, userID, agentID, sessionKey, projectID string) error {
	_, err := d.db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE sessions SET project_id = %s WHERE user_id = %s AND agent_id = %s AND session_key = %s`,
			d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		projectID, userID, agentID, sessionKey)
	return err
}
