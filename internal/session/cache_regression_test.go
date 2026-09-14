package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

func sessionRegressionDB(t *testing.T) *store.DBStore {
	t.Helper()
	d, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err = d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSessionVersionRenameDuringTurn(t *testing.T) {
	d := sessionRegressionDB(t)
	m := NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(d, "u"), "u", "a")
	s := m.Get("web", "", "chat", "")
	s.Append(provider.Message{Role: "user", Content: "question"})
	if err := s.PersistenceError(); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"new title", "new title"} {
		if err := m.RenameSessionByID(s.Key(), title); err != nil {
			t.Fatal(err)
		}
		s.Append(provider.Message{Role: "assistant", Content: "answer"})
		if err := s.PersistenceError(); err != nil {
			t.Fatalf("title edit rejected assistant save: %v", err)
		}
	}
	rec, err := d.GetSession(context.Background(), "u", "a", s.Key())
	if err != nil || len(rec.Messages) != 3 {
		t.Fatalf("assistant replies not persisted: %+v %v", rec, err)
	}
}

// Simulate a concurrent create after an observed miss, or an old Redis negative
// entry. The actual source already contains a committed message when we return.
type createAfterSessionMiss struct {
	*store.DBStore
	once bool
}

func (d *createAfterSessionMiss) GetSession(ctx context.Context, u, a, k string) (*store.SessionRecord, error) {
	if !d.once {
		d.once = true
		rec := &store.SessionRecord{Messages: []store.SessionMessage{{Role: "user", Content: "concurrent durable message"}}}
		if err := d.DBStore.SaveSession(ctx, u, a, k, rec); err != nil {
			return nil, err
		}
		return nil, store.ErrNotFound
	}
	return d.DBStore.GetSession(ctx, u, a, k)
}

func TestSessionVersionMissReloadsConcurrentWorkset(t *testing.T) {
	d := sessionRegressionDB(t)
	adapter := NewStoreAdapter(&createAfterSessionMiss{DBStore: d}, "u")
	ctx := context.Background()
	msgs, rev, err := adapter.GetSessionVersion(ctx, "a", "s")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Content != "concurrent durable message" {
		t.Fatalf("miss adopted revision %d without its workset: %+v", rev, msgs)
	}
	_, err = adapter.SaveSessionVersion(ctx, "a", "s", "web", "", "s", "", append(msgs, provider.Message{Role: "assistant", Content: "answer"}), rev)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := d.GetSession(ctx, "u", "a", "s")
	if err != nil || len(rec.Messages) != 2 || rec.Messages[0].Content != "concurrent durable message" {
		t.Fatalf("concurrent message lost: %+v %v", rec, err)
	}
}

func TestSessionVersionAbsentSnapshotRejectsLaterCreate(t *testing.T) {
	d := sessionRegressionDB(t)
	adapter := NewStoreAdapter(d, "u")
	ctx := context.Background()
	for _, deleted := range []bool{false, true} {
		if deleted {
			if err := d.DeleteSession(ctx, "u", "a", "s"); err != nil {
				t.Fatal(err)
			}
		}
		msgs, rev, err := adapter.GetSessionVersion(ctx, "a", "s")
		if err != nil || len(msgs) != 0 {
			t.Fatalf("missing snapshot: %+v %d %v", msgs, rev, err)
		}
		if err = d.SaveSession(ctx, "u", "a", "s", &store.SessionRecord{Messages: []store.SessionMessage{{Role: "user", Content: "concurrent"}}}); err != nil {
			t.Fatal(err)
		}
		_, err = adapter.SaveSessionVersion(ctx, "a", "s", "web", "", "s", "", []provider.Message{{Role: "user", Content: "stale"}}, rev)
		if !errors.Is(err, store.ErrSessionConflict) {
			t.Fatalf("late creation not fenced (deleted=%v): %v", deleted, err)
		}
	}
}

type recreateAfterSessionDelete struct{ *store.DBStore }

func (d *recreateAfterSessionDelete) DeleteSession(ctx context.Context, u, a, k string) error {
	if err := d.DBStore.DeleteSession(ctx, u, a, k); err != nil {
		return err
	}
	return d.DBStore.SaveSession(ctx, u, a, k, &store.SessionRecord{Messages: []store.SessionMessage{{Role: "user", Content: "recreated concurrently"}}})
}

func TestSessionVersionClearKeepsConcurrentRecreation(t *testing.T) {
	d := sessionRegressionDB(t)
	m := NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(&recreateAfterSessionDelete{DBStore: d}, "u"), "u", "a")
	s := m.Get("web", "", "chat", "")
	s.Append(provider.Message{Role: "user", Content: "old"})
	if err := s.PersistenceError(); err != nil {
		t.Fatal(err)
	}
	s.Clear()
	s.Append(provider.Message{Role: "assistant", Content: "after clear"})
	if err := s.PersistenceError(); err != nil {
		t.Fatal(err)
	}
	rec, err := d.GetSession(context.Background(), "u", "a", s.Key())
	if err != nil || len(rec.Messages) != 2 || rec.Messages[0].Content != "recreated concurrently" {
		t.Fatalf("clear lost concurrent recreation: %+v %v", rec, err)
	}
}
