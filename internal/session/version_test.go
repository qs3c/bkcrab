package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestSessionVersionClearUndoAndStaleWriter(t *testing.T) {
	d, err := store.NewDBStore("sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err = d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a := NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(d, "u"), "u", "a")
	b := NewManagerWithStoreForUser(t.TempDir(), NewStoreAdapter(d, "u"), "u", "a")
	x := a.Get("web", "", "s", "")
	x.Append(provider.Message{Role: "user", Content: "one"})
	if err = x.PersistenceError(); err != nil {
		t.Fatal(err)
	}
	y := b.Get("web", "", "s", "")
	x.Snapshot()
	x.Append(provider.Message{Role: "assistant", Content: "two"})
	if !x.Undo() {
		t.Fatal("undo failed")
	}
	rec, err := d.GetSession(context.Background(), "u", "a", x.Key())
	if err != nil || len(rec.Messages) != 1 {
		t.Fatal("undo not persisted", err)
	}
	y.Append(provider.Message{Role: "assistant", Content: "stale"})
	if y.PersistenceError() == nil {
		t.Fatal("stale save accepted")
	}
	x.Clear()
	x.Append(provider.Message{Role: "user", Content: "new"})
	if err = x.PersistenceError(); err != nil {
		t.Fatal("clear cannot continue", err)
	}
	rec, err = d.GetSession(context.Background(), "u", "a", x.Key())
	if err != nil || len(rec.Messages) != 1 || rec.Messages[0].Content != "new" {
		t.Fatal(rec, err)
	}
}

type failingLoadStore struct {
	SessionStore
	fail  bool
	saves int
}

func (s *failingLoadStore) GetSession(context.Context, string, string) ([]provider.Message, error) {
	if s.fail {
		return nil, errors.New("database unavailable")
	}
	return []provider.Message{{Role: "user", Content: "durable"}}, nil
}
func (s *failingLoadStore) SaveSession(context.Context, string, string, string, string, string, string, []provider.Message) error {
	s.saves++
	return nil
}
func TestSessionVersionLoadFailureStopsWrites(t *testing.T) {
	st := &failingLoadStore{fail: true}
	m := NewManagerWithStoreForUser(t.TempDir(), st, "u", "a")
	s := m.getByKey("key", "web", "", "s", "")
	if s.PersistenceError() == nil {
		t.Fatal("cold load failure was hidden")
	}
	s.Append(provider.Message{Role: "user", Content: "must not overwrite"})
	if st.saves != 0 {
		t.Fatal("saved after failed load")
	}
	st.fail = false
	s = m.getByKey("key", "web", "", "s", "")
	if s.PersistenceError() != nil || len(s.Messages) != 1 || s.Messages[0].Content != "durable" {
		t.Fatal("load recovery failed")
	}
	st.fail = true
	s = m.getByKey("key", "web", "", "s", "")
	if s.PersistenceError() == nil {
		t.Fatal("warm load failure was hidden")
	}
	s.Append(provider.Message{Role: "user", Content: "must not overwrite"})
	if st.saves != 0 {
		t.Fatal("saved after failed warm load")
	}
}
