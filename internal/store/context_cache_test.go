package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/qs3c/bkcrab/internal/contextcache"
)

func cacheStores(t *testing.T) (*DBStore, *DBStore) {
	t.Helper()
	addr := os.Getenv("BKCRAB_CACHE_TEST_REDIS")
	if addr == "" {
		t.Skip("set BKCRAB_CACHE_TEST_REDIS")
	}
	dialect, dsn := "sqlite", "file:"+filepath.Join(t.TempDir(), "db.sqlite")+"?_pragma=busy_timeout(5000)"
	if mysql := os.Getenv("BKCRAB_CACHE_TEST_MYSQL"); mysql != "" {
		dialect, dsn = "mysql", mysql
	}
	a, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Migrate(context.Background()); err != nil {
		a.Close()
		t.Fatal(err)
	}
	b, err := NewDBStore(dialect, dsn)
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	cfg := contextcache.Config{Addr: addr, Prefix: "bkcrab:agentctx:test:" + uuid.NewString() + ":", TTL: time.Minute}
	for _, d := range []*DBStore{a, b} {
		if err = d.EnableContextCache(cfg); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		d.reconcileContextCache(context.Background())
	}
	return a, b
}
func TestContextCacheCrossInstanceFilesAndSession(t *testing.T) {
	a, b := cacheStores(t)
	ctx := context.Background()
	id := uuid.NewString()
	owner := "owner"
	visitor := "visitor"
	if err := a.SaveAgent(ctx, &AgentRecord{ID: id, UserID: owner, Name: "cache test"}); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveAgentFile(ctx, id, owner, "SOUL.md", []byte("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetAgentFileExact(ctx, id, visitor, "SOUL.md"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	got, err := b.GetAgentFile(ctx, id, visitor, "SOUL.md")
	if err != nil || string(got) != "old" {
		t.Fatalf("fallback %q %v", got, err)
	}
	before := b.ContextCacheStats()
	b.GetAgentFile(ctx, id, visitor, "SOUL.md")
	after := b.ContextCacheStats()
	if after.SourceReads != before.SourceReads {
		t.Fatal("hot file/fallback reads reached SQL")
	}
	if after.Hits <= before.Hits {
		t.Fatal("hot read missed cache")
	}
	a.SaveAgentFile(ctx, id, owner, "SOUL.md", []byte("new"))
	got, err = b.GetAgentFile(ctx, id, visitor, "SOUL.md")
	if err != nil || string(got) != "new" {
		t.Fatalf("update %q %v", got, err)
	}
	a.MutateAgentFile(ctx, id, owner, "SOUL.md", func(_ []byte, _ bool) ([]byte, bool, error) { return []byte("mutated"), false, nil })
	got, _ = b.GetAgentFile(ctx, id, visitor, "SOUL.md")
	if string(got) != "mutated" {
		t.Fatal(string(got))
	}
	a.DeleteAgentFile(ctx, id, owner, "SOUL.md")
	if _, err = b.GetAgentFile(ctx, id, visitor, "SOUL.md"); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete", err)
	}
	rec := &SessionRecord{Channel: "web", ChatID: id, Messages: []SessionMessage{{Role: "user", Content: "hello", Metadata: map[string]any{"x": "y"}, Timestamp: time.Now().UTC()}}}
	if err = a.SaveSession(ctx, owner, id, id, rec); err != nil {
		t.Fatal(err)
	}
	snapshot, err := b.GetSession(ctx, owner, id, id)
	if err != nil || len(snapshot.Messages) != 1 {
		t.Fatal(snapshot, err)
	}
	old := snapshot.Revision
	rec.Messages = append(rec.Messages, SessionMessage{Role: "assistant", Content: "answer"})
	if err = a.SaveSession(WithExpectedSessionRevision(ctx, old), owner, id, id, rec); err != nil {
		t.Fatal(err)
	}
	if err = b.SaveSession(WithExpectedSessionRevision(ctx, old), owner, id, id, snapshot); !errors.Is(err, ErrSessionConflict) {
		t.Fatal("stale save accepted", err)
	}
	gotRec, err := b.GetSession(ctx, owner, id, id)
	if err != nil || len(gotRec.Messages) != 2 {
		t.Fatal(gotRec, err)
	}
	gotRec.Messages[0].Content = "caller mutation"
	fresh, _ := b.GetSession(ctx, owner, id, id)
	if fresh.Messages[0].Content != "hello" {
		t.Fatal("shared cache object")
	}
	b.contextCache.backend.Drop(ctx, cacheScope{"session", owner, id, id}.key())
	fresh, err = b.GetSession(ctx, owner, id, id)
	if err != nil || len(fresh.Messages) != 2 {
		t.Fatal("recovery", err)
	}
}
func TestContextCacheTransactionalOutboxAndRawWriter(t *testing.T) {
	a, b := cacheStores(t)
	ctx := context.Background()
	id := uuid.NewString()
	a.SaveAgentFile(ctx, id, "u", "USER.md", []byte("before"))
	b.GetAgentFileExact(ctx, id, "u", "USER.md")
	scope := cacheScope{"file", id, "u", "USER.md"}
	initial, _ := a.cacheRevision(ctx, scope)
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	sql := fmt.Sprintf("UPDATE agent_files SET content=%s WHERE agent_id=%s", a.ph(1), a.ph(2))
	if _, err = tx.ExecContext(ctx, sql, "rolled back", id); err != nil {
		t.Fatal(err)
	}
	tx.Rollback()
	revision, _ := a.cacheRevision(ctx, scope)
	if revision != initial {
		t.Fatal("outbox escaped rollback")
	}
	if _, err = a.db.ExecContext(ctx, sql, "external writer", id); err != nil {
		t.Fatal(err)
	}
	a.reconcileContextCache(ctx)
	value, err := b.GetAgentFileExact(ctx, id, "u", "USER.md")
	if err != nil || string(value) != "external writer" {
		t.Fatalf("%q %v", value, err)
	}
}

func TestContextCacheRedisFailureLeavesRecoverableOutbox(t *testing.T) {
	a, b := cacheStores(t)
	ctx := context.Background()
	id := uuid.NewString()
	scope := cacheScope{"file", id, "u", "MEMORY.md"}
	if err := a.SaveAgentFile(ctx, id, "u", "MEMORY.md", []byte("old")); err != nil {
		t.Fatal(err)
	}
	b.GetAgentFileExact(ctx, id, "u", "MEMORY.md")
	// Stop the writer's cache worker and client, simulating its Redis connection
	// being unavailable while another instance still has a healthy connection.
	a.contextCache.cancel()
	<-a.contextCache.done
	a.contextCache.backend.Close()
	if err := a.SaveAgentFile(ctx, id, "u", "MEMORY.md", []byte("new")); err != nil {
		t.Fatal("Redis failure rejected committed SQL", err)
	}
	value, err := a.GetAgentFileExact(ctx, id, "u", "MEMORY.md")
	if err != nil || string(value) != "new" {
		t.Fatal("writer did not fall back", err)
	}
	b.reconcileContextCache(ctx)
	value, err = b.GetAgentFileExact(ctx, id, "u", "MEMORY.md")
	if err != nil || string(value) != "new" {
		t.Fatalf("outbox recovery %q %v", value, err)
	}
	var rev, applied int64
	if err = b.db.QueryRowContext(ctx, fmt.Sprintf("SELECT revision,applied FROM context_cache_changes WHERE kind=%s AND s1=%s AND s2=%s AND s3=%s", b.ph(1), b.ph(2), b.ph(3), b.ph(4)), scope.Kind, scope.S1, scope.S2, scope.S3).Scan(&rev, &applied); err != nil || rev != applied {
		t.Fatal("notification not acknowledged", err)
	}
}
func TestContextCacheSkillPublicationInvalidation(t *testing.T) {
	a, b := cacheStores(t)
	ctx := context.Background()
	owner := uuid.NewString()
	a.SaveSkillPublication(ctx, owner, SkillPublication{Slug: "demo", Manifest: []byte(`{"Files":[]}`)})
	rows, err := b.ListSkillPublications(ctx, owner)
	if err != nil || len(rows) != 1 || rows[0].Deleted {
		t.Fatal(rows, err)
	}
	before := b.ContextCacheStats().SourceReads
	b.ListSkillPublications(ctx, owner)
	if b.ContextCacheStats().SourceReads != before {
		t.Fatal("hot catalog hit SQL")
	}
	a.SaveSkillPublication(ctx, owner, SkillPublication{Slug: "demo", Manifest: []byte(`{"Files":[]}`), Deleted: true})
	rows, err = b.ListSkillPublications(ctx, owner)
	if err != nil || len(rows) != 1 || !rows[0].Deleted {
		t.Fatal("publication deletion stale", err)
	}
}

func TestContextCacheSessionTitleTriggerUpgrade(t *testing.T) {
	a, b := cacheStores(t)
	ctx := context.Background()
	id := uuid.NewString()
	// Recreate the candidate's original trigger, then exercise the upgrade twice.
	if _, err := a.db.ExecContext(ctx, "DROP TRIGGER ctxcache_sessions_update_v2"); err != nil {
		t.Fatal(err)
	}
	stmt := "INSERT INTO context_cache_changes(kind,s1,s2,s3,revision,applied) VALUES ('session',NEW.user_id,NEW.agent_id,NEW.session_key,1,0)"
	if a.dialect == mysqlDialect {
		stmt += " ON DUPLICATE KEY UPDATE revision=revision+1,dirty=TRUE"
		stmt = "CREATE TRIGGER ctxcache_sessions_update AFTER UPDATE ON sessions FOR EACH ROW " + stmt
	} else {
		stmt += " ON CONFLICT(kind,s1,s2,s3) DO UPDATE SET revision=context_cache_changes.revision+1,dirty=TRUE"
		stmt = "CREATE TRIGGER ctxcache_sessions_update AFTER UPDATE ON sessions BEGIN " + stmt + "; END"
	}
	if _, err := a.db.ExecContext(ctx, stmt); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.migrateContextCache(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rec := &SessionRecord{Messages: []SessionMessage{{Role: "user", Content: "question"}}}
	if err := a.SaveSession(ctx, "u", id, id, rec); err != nil {
		t.Fatal(err)
	}
	original := rec.Revision
	for _, title := range []string{"renamed", "renamed"} {
		if err := b.RenameSession(ctx, "u", id, id, title); err != nil {
			t.Fatal(err)
		}
		revision, err := a.SessionRevision(ctx, "u", id, id)
		if err != nil || revision != original {
			t.Fatalf("title changed workset revision: %d -> %d (%v)", original, revision, err)
		}
	}
	rec.Messages = append(rec.Messages, SessionMessage{Role: "assistant", Content: "answer"})
	if err := a.SaveSession(WithExpectedSessionRevision(ctx, original), "u", id, id, rec); err != nil {
		t.Fatal(err)
	}
	persisted, err := b.GetSession(ctx, "u", id, id)
	if err != nil || persisted.Revision != rec.Revision || len(persisted.Messages) != 2 {
		t.Fatalf("inconsistent saved revision/workset: %+v %v", persisted, err)
	}
	if err := b.SaveSession(WithExpectedSessionRevision(ctx, original), "u", id, id, &SessionRecord{}); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("real stale writer accepted: %v", err)
	}
	// Case-only JSON edits must advance the revision even under a MySQL
	// case-insensitive table collation and without changing updated_at.
	query := fmt.Sprintf("UPDATE sessions SET messages=%s WHERE user_id=%s AND agent_id=%s AND session_key=%s", a.ph(1), a.ph(2), a.ph(3), a.ph(4))
	for _, content := range []string{`[{"role":"user","content":"UPPER"}]`, `[{"role":"user","content":"upper"}]`} {
		before, err := a.SessionRevision(ctx, "u", id, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = a.db.ExecContext(ctx, query, content, "u", id, id); err != nil {
			t.Fatal(err)
		}
		after, err := a.SessionRevision(ctx, "u", id, id)
		if err != nil || after != before+1 {
			t.Fatalf("case-sensitive edit not versioned: %d -> %d (%v)", before, after, err)
		}
	}
}

func TestContextCacheMissingWorksetReloadsSource(t *testing.T) {
	a, b := cacheStores(t)
	ctx := context.Background()
	id := uuid.NewString()
	// Stop reconciliation so a healthy reader can retain a stale negative entry
	// while a writer loses its Redis connection. Only these private clients stop.
	for _, d := range []*DBStore{a, b} {
		d.contextCache.cancel()
		<-d.contextCache.done
	}
	b.reconcileContextCache(ctx) // Cancellation may have interrupted an in-flight worker.
	for i := 0; i < 2; i++ {
		if _, err := b.GetSession(ctx, "u", id, id); !errors.Is(err, ErrNotFound) {
			t.Fatal(err)
		}
	}
	if err := a.contextCache.backend.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.SaveSession(ctx, "u", id, id, &SessionRecord{Messages: []SessionMessage{{Role: "user", Content: "created during partition"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetSession(ctx, "u", id, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected stale negative cache before reload: %v", err)
	}
	rec, err := b.GetSessionAfterMiss(ctx, "u", id, id)
	if err != nil || len(rec.Messages) != 1 || rec.Messages[0].Content != "created during partition" {
		t.Fatalf("miss adopted wrong workset: %+v %v", rec, err)
	}
}
