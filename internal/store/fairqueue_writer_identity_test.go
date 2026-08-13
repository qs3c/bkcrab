package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestReadFairQueueWriterIdentityReturnsOnlyCanonicalFingerprint(t *testing.T) {
	state := &fairQueueFenceDriverState{
		serverUUID: "sensitive-server-uuid",
		database:   "sensitive_database",
		connIDs:    []int64{41, 41},
	}
	store, expected := newFairQueueFenceTestStore(t, state)

	identity, err := store.ReadFairQueueWriterIdentity(context.Background())
	if err != nil {
		t.Fatalf("read writer identity: %v", err)
	}
	if identity.Fingerprint != expected || !lowerHex64Pattern.MatchString(identity.Fingerprint) {
		t.Fatalf("noncanonical fingerprint %q, want %q", identity.Fingerprint, expected)
	}

	typ := reflect.TypeOf(identity)
	if typ.NumField() != 1 || typ.Field(0).Name != "Fingerprint" || typ.Field(0).Type.Kind() != reflect.String {
		t.Fatalf("writer identity leaks fields: %v", typ)
	}
	snapshot := store.ReadFairQueueConnectionSafetySnapshot()
	if snapshot.SessionAffinity != FairQueueSessionAffinityVerified || snapshot.LastSuccessfulVerifiedAt.IsZero() {
		t.Fatalf("discovery snapshot=%+v", snapshot)
	}
}

func TestReadFairQueueWriterIdentityDiscardsSessionMismatch(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{41, 42}}
	store, _ := newFairQueueFenceTestStore(t, state)

	identity, err := store.ReadFairQueueWriterIdentity(context.Background())
	if !errors.Is(err, ErrFairQueueWriterMismatch) || identity != (FairQueueWriterIdentity{}) {
		t.Fatalf("identity=%+v error=%v", identity, err)
	}
	_, _, closed := state.counts()
	if closed != 1 {
		t.Fatalf("physical closes=%d, want 1", closed)
	}
	snapshot := store.ReadFairQueueConnectionSafetySnapshot()
	if snapshot.SessionAffinity != FairQueueSessionAffinityMismatch || !snapshot.LastSuccessfulVerifiedAt.IsZero() {
		t.Fatalf("mismatch snapshot=%+v", snapshot)
	}
}

func TestFairQueueConnectionSafetySnapshotStateMachine(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{7, 7, 7, 8, 8, 8}}
	store, writer := newFairQueueFenceTestStore(t, state)

	initial := store.ReadFairQueueConnectionSafetySnapshot()
	if initial.SessionAffinity != FairQueueSessionAffinityUnknown || !initial.LastSuccessfulVerifiedAt.IsZero() {
		t.Fatalf("initial snapshot=%+v", initial)
	}

	if err := store.withFairQueueExpectedWriterConn(context.Background(), writer,
		func(*sql.Conn, fairQueueMySQLIdentity) error { return nil }); err != nil {
		t.Fatalf("verified connection: %v", err)
	}
	verified := store.ReadFairQueueConnectionSafetySnapshot()
	if verified.SessionAffinity != FairQueueSessionAffinityVerified || verified.LastSuccessfulVerifiedAt.IsZero() {
		t.Fatalf("verified snapshot=%+v", verified)
	}

	err := store.withFairQueueExpectedWriterConn(context.Background(), writer,
		func(*sql.Conn, fairQueueMySQLIdentity) error { return nil })
	if !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("identity change error=%v", err)
	}
	mismatch := store.ReadFairQueueConnectionSafetySnapshot()
	if mismatch.SessionAffinity != FairQueueSessionAffinityMismatch ||
		!mismatch.LastSuccessfulVerifiedAt.Equal(verified.LastSuccessfulVerifiedAt) {
		t.Fatalf("mismatch snapshot=%+v, previous=%+v", mismatch, verified)
	}

	identity, err := store.ReadFairQueueWriterIdentity(context.Background())
	if err != nil || identity.Fingerprint != writer {
		t.Fatalf("rediscover identity=%+v err=%v", identity, err)
	}
	recovered := store.ReadFairQueueConnectionSafetySnapshot()
	if recovered.SessionAffinity != FairQueueSessionAffinityVerified ||
		recovered.LastSuccessfulVerifiedAt.Before(verified.LastSuccessfulVerifiedAt) {
		t.Fatalf("recovered snapshot=%+v, previous=%+v", recovered, verified)
	}
}

func TestFairQueueSafetyFailureObserverIsOneShotAndRunsOutsideStoreLock(t *testing.T) {
	t.Parallel()
	st := &DBStore{}
	calls := 0
	st.SetFairQueueSafetyFailureObserver(func() {
		calls++
		_ = st.ReadFairQueueConnectionSafetySnapshot()
	})
	st.recordFairQueueConnectionMismatch()
	st.recordFairQueueConnectionVerified(time.Now())
	st.recordFairQueueConnectionMismatch()
	if calls != 1 {
		t.Fatalf("safety observer calls = %d, want 1", calls)
	}
}

func TestFairQueueSafetyFailureObserverSeesPreexistingMismatch(t *testing.T) {
	t.Parallel()
	st := &DBStore{}
	st.recordFairQueueConnectionMismatch()
	calls := 0
	st.SetFairQueueSafetyFailureObserver(func() { calls++ })
	if calls != 1 {
		t.Fatalf("late safety observer calls = %d, want 1", calls)
	}
}

func TestFairQueueConnectionSafetySnapshotDoesNotPromoteDependencyErrors(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{7, 7}}
	store, _ := newFairQueueFenceTestStore(t, state)
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.ReadFairQueueWriterIdentity(context.Background()); err == nil {
		t.Fatal("closed dependency unexpectedly returned an identity")
	}
	snapshot := store.ReadFairQueueConnectionSafetySnapshot()
	if snapshot.SessionAffinity != FairQueueSessionAffinityUnknown || !snapshot.LastSuccessfulVerifiedAt.IsZero() {
		t.Fatalf("dependency failure changed snapshot=%+v", snapshot)
	}
}

func TestFairQueueOperationStartFencePublishesSafetyOutcome(t *testing.T) {
	t.Run("dependency error stays unknown", func(t *testing.T) {
		state := &fairQueueFenceDriverState{
			connIDs: []int64{7}, getErr: errors.New("dependency unavailable"), releaseResult: int64(1),
		}
		store, writer := newFairQueueFenceTestStore(t, state)
		err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
			func(*FairQueueOperationStartSession) error { return nil })
		if !errors.Is(err, ErrFairQueueStartLockUnavailable) {
			t.Fatalf("operation-start error=%v", err)
		}
		snapshot := store.ReadFairQueueConnectionSafetySnapshot()
		if snapshot.SessionAffinity != FairQueueSessionAffinityUnknown || !snapshot.LastSuccessfulVerifiedAt.IsZero() {
			t.Fatalf("dependency snapshot=%+v", snapshot)
		}
	})

	t.Run("verified", func(t *testing.T) {
		state := &fairQueueFenceDriverState{
			connIDs: []int64{7, 7, 7}, getResult: int64(1), releaseResult: int64(1),
		}
		store, writer := newFairQueueFenceTestStore(t, state)
		if err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
			func(*FairQueueOperationStartSession) error { return nil }); err != nil {
			t.Fatalf("operation-start fence: %v", err)
		}
		snapshot := store.ReadFairQueueConnectionSafetySnapshot()
		if snapshot.SessionAffinity != FairQueueSessionAffinityVerified || snapshot.LastSuccessfulVerifiedAt.IsZero() {
			t.Fatalf("verified snapshot=%+v", snapshot)
		}
	})

	t.Run("unsafe release", func(t *testing.T) {
		state := &fairQueueFenceDriverState{
			connIDs: []int64{7, 7, 7}, getResult: int64(1), releaseResult: driver.Value(int64(0)),
		}
		store, writer := newFairQueueFenceTestStore(t, state)
		err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
			func(*FairQueueOperationStartSession) error { return nil })
		if !errors.Is(err, ErrFairQueueUnsafeConnection) {
			t.Fatalf("release error=%v", err)
		}
		snapshot := store.ReadFairQueueConnectionSafetySnapshot()
		if snapshot.SessionAffinity != FairQueueSessionAffinityMismatch {
			t.Fatalf("unsafe snapshot=%+v", snapshot)
		}
	})
}

func TestFairQueueConnectionSafetySnapshotContainsNoIdentityMaterial(t *testing.T) {
	typ := reflect.TypeOf(FairQueueConnectionSafetySnapshot{})
	if typ.NumField() != 2 || typ.Field(0).Name != "LastSuccessfulVerifiedAt" ||
		typ.Field(1).Name != "SessionAffinity" {
		t.Fatalf("safety snapshot exposes unexpected fields: %v", typ)
	}
}

func TestFairQueueExpectedWriterConnectionPanicDiscardsPhysicalSession(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{7, 7}}
	store, writer := newFairQueueFenceTestStore(t, state)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = store.withFairQueueExpectedWriterConn(context.Background(), writer,
			func(*sql.Conn, fairQueueMySQLIdentity) error {
				panic("callback panic")
			})
	}()
	if recovered != "callback panic" {
		t.Fatalf("recovered panic=%v", recovered)
	}
	_, _, closed := state.counts()
	if closed != 1 {
		t.Fatalf("physical closes=%d after panic, want 1", closed)
	}
	snapshot := store.ReadFairQueueConnectionSafetySnapshot()
	if snapshot.SessionAffinity != FairQueueSessionAffinityMismatch {
		t.Fatalf("panic safety snapshot=%+v", snapshot)
	}
}
