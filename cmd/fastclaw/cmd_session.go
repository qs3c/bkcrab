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

	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// sessionPathPattern pulls the agent id and session key out of a chat
// URL of the shape http(s)://host[:port]/agents/<agentId>/chat/<sessionKey>[/].
// We anchor at the literal "/agents/" segment so the host is whatever
// (localhost dev, cloud prod) and so a stray trailing slash / query
// string doesn't trip the matcher.
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
from the LOCAL fastclaw.db — "online" sessions need their DB pulled
down first (rsync ~/.fastclaw/fastclaw.db from the host).

Examples:
  fastclaw session export http://localhost:18953/agents/agt_xxx/chat/s-yyy/
  fastclaw session export https://app.fastclaw.ai/agents/agt_xxx/chat/s-yyy -o /tmp/run.json`,
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

			// sessions are PK'd by (user_id, agent_id, session_key) so
			// the URL alone doesn't identify a unique row — but in
			// practice (agent_id, session_key) is unique because keys
			// are timestamp-randomized. Look up the owner via a thin
			// SQL query against the DBStore's underlying connection.
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
		"output JSON path (default ~/.fastclaw/logs/<sessionKey>.json)")
	cmd.Flags().StringVar(&dbPath, "db", "",
		"sqlite DB path (default ~/.fastclaw/fastclaw.db)")
	return cmd
}

// parseChatURL accepts either a full URL or a bare path and returns
// (agentID, sessionKey). Errors include the input so the user sees
// exactly what was rejected.
func parseChatURL(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("chat URL is empty")
	}
	// url.Parse tolerates a bare path; we extract the path segment
	// either way and run the regex on it.
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

// openStoreAt opens a DBStore at an explicit sqlite path. Empty
// dbPath defaults to ~/.fastclaw/fastclaw.db with the same pragma
// tuning the daemon uses, so a CLI export against a live db doesn't
// hit lock contention.
func openStoreAt(dbPath string) (*store.DBStore, func(), error) {
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, nil, fmt.Errorf("resolve home dir: %w", err)
		}
		dbPath = filepath.Join(home, ".fastclaw", "fastclaw.db")
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

// lookupSessionUser finds the owning user_id for a session by
// (agent_id, session_key). Errors when zero rows match (typo in URL,
// session deleted) or when multiple users happen to share the pair
// (extremely unlikely — session_key is timestamp-randomized — but
// would lead to a silent wrong-user export, so flag it).
func lookupSessionUser(ctx context.Context, st *store.DBStore, agentID, sessionKey string) (string, error) {
	rows, err := st.DB().QueryContext(ctx,
		`SELECT user_id FROM sessions WHERE agent_id = ? AND session_key = ?`,
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

// resolveOutputPath returns the user's -o value if set, else
// ~/.fastclaw/logs/<sessionKey>.json. Per-session filename so a
// batch of exports doesn't overwrite each other; same parent dir as
// the daemon's own logs so the analyzer can sweep one place.
func resolveOutputPath(explicit, sessionKey string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".fastclaw", "logs", sessionKey+".json"), nil
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
