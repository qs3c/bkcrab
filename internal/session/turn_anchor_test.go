package session

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/store"
)

type barrierSessionStore struct {
	SessionStore
	mu            sync.Mutex
	order         []string
	anchorEntered chan struct{}
	releaseAnchor chan struct{}
	saveCalled    chan struct{}
	anchorErr     error
	saveErr       error
	saveErrors    []error
	saveCalls     int
	cancelErr     error
	cancelSeq     int64
}

func (s *barrierSessionStore) record(step string) {
	s.mu.Lock()
	s.order = append(s.order, step)
	s.mu.Unlock()
}

func (s *barrierSessionStore) AppendTurnAnchor(context.Context, string, string, provider.Message) (int64, error) {
	s.record("anchor")
	if s.anchorEntered != nil {
		close(s.anchorEntered)
	}
	if s.releaseAnchor != nil {
		<-s.releaseAnchor
	}
	return 42, s.anchorErr
}

func (s *barrierSessionStore) AppendMessage(context.Context, string, string, provider.Message) error {
	return nil
}

func (s *barrierSessionStore) SaveSession(context.Context, string, string, string, string, string, string, []provider.Message) error {
	s.record("save")
	if s.saveCalled != nil {
		close(s.saveCalled)
	}
	s.mu.Lock()
	s.saveCalls++
	call := s.saveCalls
	if call <= len(s.saveErrors) {
		err := s.saveErrors[call-1]
		s.mu.Unlock()
		return err
	}
	s.mu.Unlock()
	return s.saveErr
}

func (s *barrierSessionStore) CancelTurnAnchor(_ context.Context, _, _ string, seq int64) error {
	s.record("cancel")
	s.cancelSeq = seq
	return s.cancelErr
}

func (s *barrierSessionStore) steps() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

func TestSessionAppendTurnAnchorReturnsSeq(t *testing.T) {
	dir := t.TempDir()
	db, err := store.NewDBStore("sqlite", "file:"+dir+"/t.db?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	adapter := NewStoreAdapter(db, "u1")
	s := &Session{store: adapter, agentID: "agentA", sessionKey: "sess1"}

	seq0, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "hi"})
	if err != nil {
		t.Fatalf("anchor 0: %v", err)
	}
	seq1, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "again"})
	if err != nil {
		t.Fatalf("anchor 1: %v", err)
	}
	if seq0 != 0 || seq1 != 1 {
		t.Fatalf("seq want 0,1 got %d,%d", seq0, seq1)
	}
}

func TestSessionAppendTurnAnchorPublishesRunningBarrierBeforeSnapshot(t *testing.T) {
	st := &barrierSessionStore{
		anchorEntered: make(chan struct{}),
		releaseAnchor: make(chan struct{}),
		saveCalled:    make(chan struct{}),
	}
	s := &Session{store: st, agentID: "agentA", sessionKey: "sess1"}
	type result struct {
		seq int64
		err error
	}
	done := make(chan result, 1)
	go func() {
		seq, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "new turn"})
		done <- result{seq: seq, err: err}
	}()

	select {
	case <-st.anchorEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("anchor barrier was not entered")
	}
	select {
	case <-st.saveCalled:
		t.Fatal("sessions snapshot was published before running anchor completed")
	default:
	}
	close(st.releaseAnchor)
	got := <-done
	if got.err != nil || got.seq != 42 {
		t.Fatalf("append result seq=%d err=%v", got.seq, got.err)
	}
	if want := []string{"anchor", "save"}; !reflect.DeepEqual(st.steps(), want) {
		t.Fatalf("persistence order=%v want=%v", st.steps(), want)
	}
}

func TestSessionAppendTurnAnchorCompensatesSnapshotFailure(t *testing.T) {
	saveErr := errors.New("snapshot write failed")
	st := &barrierSessionStore{saveErr: saveErr}
	s := &Session{store: st, agentID: "agentA", sessionKey: "sess1"}
	seq, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "new turn"})
	if seq != -1 || !errors.Is(err, saveErr) {
		t.Fatalf("append result seq=%d err=%v", seq, err)
	}
	if st.cancelSeq != 42 {
		t.Fatalf("compensated seq=%d want=42", st.cancelSeq)
	}
	if want := []string{"anchor", "save", "cancel"}; !reflect.DeepEqual(st.steps(), want) {
		t.Fatalf("failure order=%v want=%v", st.steps(), want)
	}
}

func TestSessionAppendTurnAnchorReturnsPersistedSeqWhenCompensationFails(t *testing.T) {
	saveErr := errors.New("snapshot write failed")
	cancelErr := errors.New("compensation failed")
	st := &barrierSessionStore{saveErr: saveErr, cancelErr: cancelErr}
	s := &Session{store: st, agentID: "agentA", sessionKey: "sess1"}
	seq, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "new turn"})
	if seq != 42 || !errors.Is(err, saveErr) || !errors.Is(err, cancelErr) {
		t.Fatalf("append result seq=%d err=%v", seq, err)
	}
}

func TestSessionEnsureSnapshotPersistedRetriesFinalAppendFailure(t *testing.T) {
	finalSaveErr := errors.New("final assistant snapshot failed")
	st := &barrierSessionStore{saveErrors: []error{nil, finalSaveErr, nil}}
	s := &Session{store: st, agentID: "agentA", sessionKey: "sess1"}
	if _, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "question"}); err != nil {
		t.Fatalf("append anchor: %v", err)
	}
	s.Append(provider.Message{Role: "assistant", Content: "final answer"})
	if err := s.EnsureSnapshotPersisted(); err != nil {
		t.Fatalf("retry final snapshot: %v", err)
	}
	if st.saveCalls != 3 {
		t.Fatalf("save calls=%d want anchor, failed final append, retry", st.saveCalls)
	}
	if err := s.EnsureSnapshotPersisted(); err != nil || st.saveCalls != 3 {
		t.Fatalf("clean version should not save again: calls=%d err=%v", st.saveCalls, err)
	}
}

func TestSessionEnsureSnapshotPersistedReportsPersistentFinalAppendFailure(t *testing.T) {
	finalSaveErr := errors.New("final assistant snapshot failed")
	st := &barrierSessionStore{saveErrors: []error{nil, finalSaveErr, finalSaveErr}}
	s := &Session{store: st, agentID: "agentA", sessionKey: "sess1"}
	if _, err := s.AppendTurnAnchor(provider.Message{Role: "user", Content: "question"}); err != nil {
		t.Fatalf("append anchor: %v", err)
	}
	s.Append(provider.Message{Role: "assistant", Content: "final answer"})
	if err := s.EnsureSnapshotPersisted(); !errors.Is(err, finalSaveErr) {
		t.Fatalf("persistent final snapshot error=%v", err)
	}
}
