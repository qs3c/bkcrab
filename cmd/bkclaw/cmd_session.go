package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qs3c/bkclaw/internal/store"
)

// sessionPathPattern 从形如 http(s)://host[:port]/agents/<agentId>/chat/<sessionKey>[/] 的聊天 URL 中提取代理 ID 和会话密钥。
// 我们锚定在字面的 "/agents/" 段上，因此主机可以是任意值
//（本地开发 localhost、云端 prod），并且多余的尾部斜杠 / 查询字符串不会导致匹配失败。
var sessionPathPattern = regexp.MustCompile(`/agents/([^/]+)/chat/([^/?#]+)`)

func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Inspect / export chat sessions",
	}
	cmd.AddCommand(sessionExportCmd())
	return cmd
}

func sessionExportCmd() *cobra.Command {
	var outputPath, dbPath string
	cmd := &cobra.Command{
		Use:   "export <chat-url>",
		Short: "Export a chat session as JSON (full message stream)",
		Long: `Export every turn of a chat session — user / assistant / tool
messages, tool_calls, tool_call_ids, thinking — to a JSON file so a
downstream agent (or you) can analyze where the session went well or
got stuck.

The URL is the chat page URL from the browser; only its path matters
(/agents/<agentId>/chat/<sessionKey>). The session data is always read
from the LOCAL bkclaw.db — "online" sessions need their DB pulled
down first (rsync ~/.bkclaw/bkclaw.db from the host).

Examples:
  bkclaw session export http://localhost:18953/agents/agt_xxx/chat/s-yyy/
  bkclaw session export https://app.bkclaw.ai/agents/agt_xxx/chat/s-yyy -o /tmp/run.json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID, sessionKey, err := parseChatURL(args[0])
			if err != nil {
				return err
			}

			st, closeFn, err := openStoreAt(dbPath)
			if err != nil {
				return err
			}
			defer closeFn()

			ctx := context.Background()

	// 会话以 (user_id, agent_id, session_key) 为主键，因此
	// 仅凭 URL 无法唯一标识一行——但在实践中
	// (agent_id, session_key) 是唯一的，因为密钥是
	// 基于时间戳随机生成的。通过对 DBStore 底层连接的
	// 轻量 SQL 查询查找所有者。
			userID, err := lookupSessionUser(ctx, st, agentID, sessionKey)
			if err != nil {
				return err
			}

			msgs, err := st.ListSessionMessages(ctx, userID, agentID, sessionKey)
			if err != nil {
				return fmt.Errorf("list messages: %w", err)
			}

			out := map[string]any{
				"version":      1,
				"exportedAt":   time.Now().UTC().Format(time.RFC3339),
				"sourceUrl":    args[0],
				"userId":       userID,
				"agentId":      agentID,
				"sessionKey":   sessionKey,
				"messageCount": len(msgs),
				"messages":     msgs,
			}

			finalPath, err := resolveOutputPath(outputPath, sessionKey)
			if err != nil {
				return err
			}
			if err := writeJSON(finalPath, out); err != nil {
				return err
			}
			fmt.Printf("exported %d messages → %s\n", len(msgs), finalPath)
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputPath, "output", "o", "",
		"output JSON path (default ~/.bkclaw/logs/<sessionKey>.json)")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"legacy SQLite DB path (default: BKCLAW_STORAGE_* MySQL database)")
	return cmd
}

// parseChatURL 接受完整 URL 或裸路径，并返回
// (agentID, sessionKey)。错误信息包含输入内容，以便用户
// 确切看到被拒绝的内容。
func parseChatURL(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("chat URL is empty")
	}
	// url.Parse 接受裸路径；我们无论如何都提取路径段并在其上运行正则表达式。
	path := raw
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		path = u.Path
	}
	m := sessionPathPattern.FindStringSubmatch(path)
	if len(m) != 3 {
		return "", "", fmt.Errorf("URL doesn't match /agents/<id>/chat/<key>: %q", raw)
	}
	return m[1], m[2], nil
}

// openStoreAt 默认使用已配置的运行时数据库。显式路径仅用于从旧版 SQLite 数据库导出数据。
func openStoreAt(dbPath string) (*store.DBStore, func(), error) {
	if dbPath == "" {
		st, err := openStoreFromEnv()
		if err != nil {
			return nil, nil, err
		}
		db, ok := st.(*store.DBStore)
		if !ok {
			_ = st.Close()
			return nil, nil, fmt.Errorf("configured store does not expose SQL access")
		}
		return db, func() { _ = db.Close() }, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return nil, nil, fmt.Errorf("db file: %w", err)
	}
	dsn := "file:" + dbPath +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
	db, err := store.NewDBStore("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}
	return db, func() { _ = db.Close() }, nil
}

// lookupSessionUser 通过 (agent_id, session_key) 查找会话的拥有者 user_id。
// 当零行匹配（URL 拼写错误、会话已删除）或多个用户恰好共享该组合时返回错误
//（极不可能——session_key 是基于时间戳随机生成的——但
// 会导致静默地导出错误用户，因此标记出来）。
func lookupSessionUser(ctx context.Context, st *store.DBStore, agentID, sessionKey string) (string, error) {
	query := `SELECT user_id FROM sessions WHERE agent_id = ? AND session_key = ?`
	if st.Dialect() == "postgres" {
		query = `SELECT user_id FROM sessions WHERE agent_id = $1 AND session_key = $2`
	}
	rows, err := st.DB().QueryContext(ctx,
		query,
		agentID, sessionKey)
	if err != nil {
		return "", fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()
	var users []string
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return "", err
		}
		users = append(users, uid)
	}
	switch len(users) {
	case 0:
		return "", fmt.Errorf("no session found for agent=%s key=%s", agentID, sessionKey)
	case 1:
		return users[0], nil
	default:
		return "", fmt.Errorf("ambiguous: session key matches %d users (%v) — narrow with --user", len(users), users)
	}
}

// resolveOutputPath 如果设置了 -o 则返回用户指定的值，否则返回
// ~/.bkclaw/logs/<sessionKey>.json。每个会话使用独立文件名，
// 使得批量导出不会相互覆盖；父目录与守护进程自身日志相同，
// 便于分析器在同一个位置扫描。
func resolveOutputPath(explicit, sessionKey string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".bkclaw", "logs", sessionKey+".json"), nil
}

func writeJSON(path string, payload any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}
