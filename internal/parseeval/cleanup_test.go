package parseeval

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

type fakeCleanupStore struct {
	runs   map[string]store.ParserEvalRunRecord
	events *[]string
}

func (f *fakeCleanupStore) GetParserEvalRun(_ context.Context, id string) (*store.ParserEvalRunRecord, error) {
	run, ok := f.runs[id]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &run, nil
}

func (f *fakeCleanupStore) ListExpiredParserEvalRuns(context.Context, time.Time, int) ([]store.ParserEvalRunRecord, error) {
	result := make([]store.ParserEvalRunRecord, 0, len(f.runs))
	for _, run := range f.runs {
		if terminalStoreRun(run.Status) {
			result = append(result, run)
		}
	}
	return result, nil
}

func (f *fakeCleanupStore) PurgeParserEvalRun(_ context.Context, id string) (bool, error) {
	*f.events = append(*f.events, "purge:"+id)
	if _, ok := f.runs[id]; !ok {
		return false, nil
	}
	delete(f.runs, id)
	return true, nil
}

type fakeCleanupObjects struct {
	events     *[]string
	failPrefix string
}

func (*fakeCleanupObjects) Put(context.Context, string, io.Reader, int64, string) error { return nil }
func (*fakeCleanupObjects) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}
func (*fakeCleanupObjects) Delete(context.Context, string) error { return nil }
func (f *fakeCleanupObjects) DeletePrefix(_ context.Context, prefix string) error {
	*f.events = append(*f.events, "objects:"+prefix)
	if strings.Contains(prefix, f.failPrefix) && f.failPrefix != "" {
		return errors.New("object deletion failed")
	}
	return nil
}

func TestCleanupDeletesObjectsBeforeSQL(t *testing.T) {
	events := []string{}
	database := &fakeCleanupStore{events: &events, runs: map[string]store.ParserEvalRunRecord{
		"per_done": {ID: "per_done", Status: store.ParserEvalRunSucceeded},
	}}
	cleanup, err := NewCleanup(database, &fakeCleanupObjects{events: &events})
	if err != nil {
		t.Fatal(err)
	}
	removed, err := cleanup.CleanupExpired(context.Background(), 50)
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	want := "objects:parser-eval/runs/per_done/,purge:per_done"
	if got := strings.Join(events, ","); got != want {
		t.Fatalf("events=%q want=%q", got, want)
	}
}

func TestCleanupKeepsSQLWhenObjectDeletionFails(t *testing.T) {
	events := []string{}
	database := &fakeCleanupStore{events: &events, runs: map[string]store.ParserEvalRunRecord{
		"per_failed": {ID: "per_failed", Status: store.ParserEvalRunFailed},
	}}
	cleanup, _ := NewCleanup(database, &fakeCleanupObjects{events: &events, failPrefix: "per_failed"})
	removed, err := cleanup.CleanupExpired(context.Background(), 50)
	if err == nil || removed != 0 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, remains := database.runs["per_failed"]; !remains || strings.Contains(strings.Join(events, ","), "purge:") {
		t.Fatalf("SQL was purged after object failure: runs=%v events=%v", database.runs, events)
	}
}

func TestCleanupRejectsActiveManualDeleteBeforeObjects(t *testing.T) {
	events := []string{}
	database := &fakeCleanupStore{events: &events, runs: map[string]store.ParserEvalRunRecord{
		"per_active": {ID: "per_active", Status: store.ParserEvalRunRunning},
	}}
	cleanup, _ := NewCleanup(database, &fakeCleanupObjects{events: &events})
	if err := cleanup.DeleteRun(context.Background(), "per_active"); !errors.Is(err, store.ErrParserEvalActive) {
		t.Fatalf("err=%v", err)
	}
	if len(events) != 0 {
		t.Fatalf("active run touched objects: %v", events)
	}
}
