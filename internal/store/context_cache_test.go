package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/contextcache"
	"github.com/redis/go-redis/v9"
)

func TestContextCacheStore(t *testing.T) {
	url := os.Getenv("BKCRAB_TEST_CONTEXT_REDIS_URL")
	if url == "" {
		t.Skip("set BKCRAB_TEST_CONTEXT_REDIS_URL to an isolated Redis")
	}
	for _, dialect := range []string{"sqlite", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			var db *DBStore
			if dialect == "sqlite" {
				db = openTestDB(t)
			} else {
				dsn := os.Getenv("BKCRAB_TEST_CONTEXT_MYSQL_DSN")
				if dsn == "" {
					t.Skip("set BKCRAB_TEST_CONTEXT_MYSQL_DSN to a disposable database")
				}
				var err error
				db, err = NewDBStore("mysql", dsn)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Migrate(context.Background()); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { db.Close() })
			ns := uuid.NewString()
			makeCache := func() *contextcache.Cache {
				opts, err := redis.ParseURL(url)
				if err != nil {
					t.Fatal(err)
				}
				c := contextcache.New(redis.NewClient(opts), ns)
				t.Cleanup(func() { c.Close() })
				return c
			}
			db.contextCache = makeCache()
			// Separate cache clients model independent application processes.
			peer := &DBStore{db: db.db, dialect: db.dialect, contextCache: makeCache()}
			ctx, agent := context.Background(), ns
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(db.SaveAgent(ctx, &AgentRecord{ID: agent, UserID: "owner", Name: "cache test"}))
			must(db.SaveAgentFile(ctx, agent, "owner", "USER.md", []byte("owner profile")))
			if _, err := peer.GetAgentFileExact(ctx, agent, "visitor", "USER.md"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("visitor inherited owner: %v", err)
			}
			must(db.SaveAgentFile(ctx, agent, "visitor", "USER.md", []byte("visitor profile")))
			got, err := peer.GetAgentFileExact(ctx, agent, "visitor", "USER.md")
			must(err)
			if string(got) != "visitor profile" {
				t.Fatalf("negative cache not invalidated: %q", got)
			}
			must(db.SaveAgentFile(ctx, agent, "owner", "SOUL.md", []byte("old")))
			got, err = peer.GetAgentFile(ctx, agent, "visitor", "SOUL.md")
			must(err)
			if string(got) != "old" {
				t.Fatalf("owner fallback: %q", got)
			}
			must(db.SaveAgentFile(ctx, agent, "owner", "SOUL.md", []byte("new")))
			got, err = peer.GetAgentFile(ctx, agent, "visitor", "SOUL.md")
			must(err)
			if string(got) != "new" {
				t.Fatalf("stale owner fallback: %q", got)
			}
			_, err = db.MutateAgentFile(ctx, agent, "visitor", "USER.md", func(b []byte, exists bool) ([]byte, bool, error) { return []byte("changed"), false, nil })
			must(err)
			got, err = peer.GetAgentFileExact(ctx, agent, "visitor", "USER.md")
			must(err)
			if string(got) != "changed" {
				t.Fatalf("mutation not invalidated: %q", got)
			}
			_, err = db.MutateAgentFile(ctx, agent, "visitor", "USER.md", func(b []byte, exists bool) ([]byte, bool, error) { return nil, false, errors.New("rollback") })
			if err == nil {
				t.Fatal("mutation error hidden")
			}
			got, err = peer.GetAgentFileExact(ctx, agent, "visitor", "USER.md")
			must(err)
			if string(got) != "changed" {
				t.Fatal("uncommitted data cached")
			}
			must(db.DeleteAgentFile(ctx, agent, "visitor", "USER.md"))
			if _, err = peer.GetAgentFileExact(ctx, agent, "visitor", "USER.md"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted file revived: %v", err)
			}

			messages := []SessionMessage{{Role: "assistant", Content: "answer", Thinking: "reason", RawAssistant: json.RawMessage(`{"role":"assistant"}`), Metadata: map[string]interface{}{"x": "y"}, Origin: "goal_context", Timestamp: time.Now().UTC(), ToolCalls: []any{map[string]any{"id": "call"}}}}
			rec := &SessionRecord{Channel: "web", ChatID: "chat", ProjectID: "project", Messages: messages}
			must(db.SaveSession(ctx, "owner", agent, "session", rec))
			cached, err := peer.GetSession(ctx, "owner", agent, "session")
			must(err)
			if len(cached.Messages) != 1 || cached.Messages[0].Thinking != "reason" || cached.Messages[0].Metadata["x"] != "y" || cached.Messages[0].ToolCalls == nil || string(cached.Messages[0].RawAssistant) != `{"role":"assistant"}` {
				t.Fatalf("message fields lost: %+v", cached)
			}
			// Compaction replaces the workset, while the durable archive survives.
			must(db.AppendSessionMessage(ctx, "owner", agent, "session", messages[0]))
			must(db.SaveSession(ctx, "owner", agent, "session", &SessionRecord{Messages: []SessionMessage{{Role: "user", Content: "compacted"}}}))
			cached, err = peer.GetSession(ctx, "owner", agent, "session")
			must(err)
			if cached.ProjectID != "project" || cached.Channel != "web" || cached.Messages[0].Content != "compacted" {
				t.Fatalf("bad committed workset: %+v", cached)
			}
			must(db.MoveSession(ctx, "owner", agent, "session", "moved"))
			cached, err = peer.GetSession(ctx, "owner", agent, "session")
			must(err)
			if cached.ProjectID != "moved" {
				t.Fatal("project cache stale")
			}
			finish := db.contextCache.Changing(ctx, sessionCacheKey("owner", agent, "session"))
			finish()
			cached, err = peer.GetSession(ctx, "owner", agent, "session")
			must(err)
			if cached.Messages[0].Content != "compacted" {
				t.Fatal("miss did not restore workset")
			}
			must(db.DeleteSession(ctx, "owner", agent, "session"))
			if _, err = peer.GetSession(ctx, "owner", agent, "session"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted session revived: %v", err)
			}
			// A warm file read must work without a DB connection at all.
			_, err = peer.GetAgentFileExact(ctx, agent, "owner", "SOUL.md")
			must(err)
			must(db.db.Close())
			got, err = peer.GetAgentFileExact(ctx, agent, "owner", "SOUL.md")
			must(err)
			if string(got) != "new" {
				t.Fatal("warm file read used source")
			}
		})
	}
}
