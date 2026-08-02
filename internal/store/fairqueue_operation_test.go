package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fairQueueOperationTestDDL = `CREATE TABLE fairqueue_resource_operations (
	resource TEXT PRIMARY KEY,
	operation_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	phase TEXT NOT NULL,
	current_writer_fingerprint TEXT NOT NULL,
	original_writer_fingerprint TEXT NOT NULL DEFAULT '',
	target_writer_fingerprint TEXT NOT NULL DEFAULT '',
	repair_high_water TEXT,
	repair_pass_complete BOOLEAN NOT NULL DEFAULT FALSE,
	force_not_before TIMESTAMP,
	force_delete_pass_complete BOOLEAN NOT NULL DEFAULT FALSE,
	version BIGINT NOT NULL DEFAULT 1,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
)`

func newFairQueueOperationTestSession(t *testing.T, resource, writer string) (*DBStore, *FairQueueOperationStartSession) {
	t.Helper()
	store, err := NewDBStore("sqlite", "file:"+t.TempDir()+"/journal.db?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.ExecContext(context.Background(), fairQueueOperationTestDDL); err != nil {
		t.Fatal(err)
	}
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return store, &FairQueueOperationStartSession{
		store: store, conn: conn, resource: resource, expectedWriter: writer,
	}
}

func TestFairQueueOperationLockNameIsStableAndIndependent(t *testing.T) {
	first := fairQueueOperationStartLockName("bkcrab", "rag.index")
	if got := len(first); got >= 64 {
		t.Fatalf("operation lock length=%d name=%q", got, first)
	}
	if first != fairQueueOperationStartLockName("bkcrab", "rag.index") {
		t.Fatal("operation-start lock name is not deterministic")
	}
	if first == fairQueueOperationStartLockName("bkcrab", "image.generate") ||
		first == fairQueueOperationStartLockName("other", "rag.index") {
		t.Fatal("database/resource did not isolate operation-start lock")
	}
	if !strings.HasPrefix(first, "bkcrab:fqo:") || strings.Contains(first, "rag.index") {
		t.Fatalf("unsafe operation-start lock name %q", first)
	}
}

func TestFairQueueOperationProposalValidationAndTimeNormalization(t *testing.T) {
	id, err := NewFairQueueOperationID()
	if err != nil || !lowerHex32Pattern.MatchString(id) {
		t.Fatalf("operation id=%q err=%v", id, err)
	}
	deadline := time.Date(2026, 8, 2, 1, 2, 3, 987654321, time.FixedZone("test", 8*60*60))
	normalized := normalizeFairQueueOperationProposal(FairQueueOperationProposal{
		Resource: "rag.index", OperationID: id, Kind: FairQueueOperationForceRebuild,
		CurrentWriterFingerprint: strings.Repeat("a", 64), ForceNotBefore: &deadline,
	})
	if normalized.ForceNotBefore == nil || normalized.ForceNotBefore.Location() != time.UTC ||
		normalized.ForceNotBefore.Nanosecond()%1000 != 0 {
		t.Fatalf("force deadline was not normalized to MySQL microseconds: %v", normalized.ForceNotBefore)
	}
	bad := normalized
	bad.OperationID = strings.Repeat("A", 32)
	if !errors.Is(validateFairQueueOperationProposal(bad), ErrFairQueueOperationInvalid) {
		t.Fatal("uppercase/noncanonical operation id accepted")
	}
	bad = normalized
	bad.Kind = FairQueueOperationKind("NORMAL")
	if !errors.Is(validateFairQueueOperationProposal(bad), ErrFairQueueOperationInvalid) {
		t.Fatal("NORMAL was accepted by the special-operation journal")
	}
	bad = normalized
	bad.Resource = "RAG.index"
	if !errors.Is(validateFairQueueOperationProposal(bad), ErrFairQueueOperationInvalid) {
		t.Fatal("noncanonical uppercase resource was accepted")
	}
}

func TestFairQueueOperationJournalFullCASAndIdempotency(t *testing.T) {
	ctx := context.Background()
	writer := strings.Repeat("a", 64)
	_, session := newFairQueueOperationTestSession(t, "rag.index", writer)
	proposal := FairQueueOperationProposal{
		Resource: "rag.index", OperationID: strings.Repeat("1", 32),
		Kind: FairQueueOperationRabbitRepair, CurrentWriterFingerprint: writer,
	}
	active, err := session.BeginSpecial(ctx, nil, proposal)
	if err != nil || active.Phase != FairQueueOperationActive || active.Version != 1 {
		t.Fatalf("begin active=%+v err=%v", active, err)
	}
	read, found, err := session.Read(ctx)
	if err != nil || !found || !fairQueueOperationCASMatches(active, read) {
		t.Fatalf("session read=%+v found=%v err=%v", read, found, err)
	}

	wrongResource := proposal
	wrongResource.Resource = "image.generate"
	wrongResource.OperationID = strings.Repeat("2", 32)
	if _, err := session.BeginSpecial(ctx, nil, wrongResource); !errors.Is(err, ErrFairQueueOperationInvalid) {
		t.Fatalf("cross-resource begin error=%v", err)
	}

	highWater := "42"
	withHighWater, err := session.updateExpected(ctx, active, `repair_high_water=?`, []any{highWater},
		func(record FairQueueOperationRecord) bool {
			return record.RepairHighWater != nil && *record.RepairHighWater == highWater
		})
	if err != nil || withHighWater.Version != active.Version+1 {
		t.Fatalf("set high water=%+v err=%v", withHighWater, err)
	}
	// Retrying with the stale pre-mutation record is a read-only idempotent
	// success only because the exact operation identity and desired value match.
	repeated, err := session.updateExpected(ctx, active, `repair_high_water=?`, []any{highWater},
		func(record FairQueueOperationRecord) bool {
			return record.RepairHighWater != nil && *record.RepairHighWater == highWater
		})
	if err != nil || repeated.Version != withHighWater.Version {
		t.Fatalf("idempotent high water=%+v err=%v", repeated, err)
	}
	if _, err := session.updateExpected(ctx, active, `repair_high_water=?`, []any{"43"},
		func(record FairQueueOperationRecord) bool {
			return record.RepairHighWater != nil && *record.RepairHighWater == "43"
		}); !errors.Is(err, ErrFairQueueOperationConflict) {
		t.Fatalf("stale rewrite error=%v", err)
	}

	pass, err := session.updateExpected(ctx, withHighWater, `repair_pass_complete=TRUE`, nil,
		func(record FairQueueOperationRecord) bool { return record.RepairPassComplete })
	if err != nil || !pass.RepairPassComplete {
		t.Fatalf("mark repair pass=%+v err=%v", pass, err)
	}
	ready, err := session.updateExpected(ctx, pass, `phase='READY_COMMITTED'`, nil,
		func(record FairQueueOperationRecord) bool {
			return record.Phase == FairQueueOperationReadyCommitted
		})
	if err != nil || ready.Phase != FairQueueOperationReadyCommitted {
		t.Fatalf("commit ready=%+v err=%v", ready, err)
	}
	completed, err := session.updateExpected(ctx, ready, `phase='COMPLETED'`, nil,
		func(record FairQueueOperationRecord) bool { return record.Phase == FairQueueOperationCompleted })
	if err != nil || completed.Phase != FairQueueOperationCompleted {
		t.Fatalf("complete=%+v err=%v", completed, err)
	}

	nextProposal := proposal
	nextProposal.OperationID = strings.Repeat("2", 32)
	next, err := session.BeginSpecial(ctx, &completed, nextProposal)
	if err != nil || next.OperationID != nextProposal.OperationID ||
		next.Phase != FairQueueOperationActive || next.Version != completed.Version+1 {
		t.Fatalf("replace completed=%+v err=%v", next, err)
	}
	if _, err := session.BeginSpecial(ctx, &completed, FairQueueOperationProposal{
		Resource: "rag.index", OperationID: strings.Repeat("3", 32),
		Kind: FairQueueOperationForceRebuild, CurrentWriterFingerprint: writer,
		ForceNotBefore: func() *time.Time { value := time.Now().UTC(); return &value }(),
	}); !errors.Is(err, ErrFairQueueOperationConflict) {
		t.Fatalf("stale completed CAS error=%v", err)
	}
}

func TestFairQueueOperationReadyValidationFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	base := FairQueueOperationRecord{
		Resource: "rag.index", OperationID: strings.Repeat("1", 32),
		Kind: FairQueueOperationRabbitRepair, Phase: FairQueueOperationReadyCommitted,
		CurrentWriterFingerprint: strings.Repeat("a", 64), Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if !errors.Is(validateFairQueueOperationRecord(base), ErrFairQueueOperationInvalid) {
		t.Fatal("READY_COMMITTED Rabbit repair without authoritative progress accepted")
	}
	highWater := "10"
	base.RepairHighWater = &highWater
	base.RepairPassComplete = true
	if err := validateFairQueueOperationRecord(base); err != nil {
		t.Fatalf("valid Rabbit repair record rejected: %v", err)
	}
}

type fairQueueFenceDriverState struct {
	mu sync.Mutex

	serverUUID string
	database   string
	connIDs    []int64

	getResult     driver.Value
	getErr        error
	releaseResult driver.Value
	releaseErr    error
	lockOwners    []driver.Value

	identityCalls int
	lockChecks    int
	getCalls      int
	releaseCalls  int
	physicalClose int
}

type fairQueueFenceDriver struct{ state *fairQueueFenceDriverState }

func (d *fairQueueFenceDriver) Open(string) (driver.Conn, error) {
	return &fairQueueFenceDriverConn{state: d.state}, nil
}

type fairQueueFenceDriverConn struct{ state *fairQueueFenceDriverState }

func (c *fairQueueFenceDriverConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *fairQueueFenceDriverConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transaction not supported")
}
func (c *fairQueueFenceDriverConn) Close() error {
	c.state.mu.Lock()
	c.state.physicalClose++
	c.state.mu.Unlock()
	return nil
}
func (c *fairQueueFenceDriverConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	switch {
	case strings.Contains(query, "IS_USED_LOCK"):
		index := c.state.identityCalls
		c.state.identityCalls++
		if index >= len(c.state.connIDs) {
			index = len(c.state.connIDs) - 1
		}
		connectionID := c.state.connIDs[index]
		lockOwner := driver.Value(connectionID)
		if c.state.lockChecks < len(c.state.lockOwners) {
			lockOwner = c.state.lockOwners[c.state.lockChecks]
		}
		c.state.lockChecks++
		return &fairQueueFenceRows{
			columns: []string{"server_uuid", "database", "connection_id", "lock_owner"},
			values:  []driver.Value{c.state.serverUUID, c.state.database, connectionID, lockOwner},
		}, nil
	case strings.Contains(query, "@@server_uuid"):
		index := c.state.identityCalls
		c.state.identityCalls++
		if index >= len(c.state.connIDs) {
			index = len(c.state.connIDs) - 1
		}
		return &fairQueueFenceRows{
			columns: []string{"server_uuid", "database", "connection_id"},
			values:  []driver.Value{c.state.serverUUID, c.state.database, c.state.connIDs[index]},
		}, nil
	case strings.Contains(query, "GET_LOCK"):
		c.state.getCalls++
		if c.state.getErr != nil {
			return nil, c.state.getErr
		}
		return &fairQueueFenceRows{columns: []string{"GET_LOCK"}, values: []driver.Value{c.state.getResult}}, nil
	case strings.Contains(query, "RELEASE_LOCK"):
		c.state.releaseCalls++
		if c.state.releaseErr != nil {
			return nil, c.state.releaseErr
		}
		return &fairQueueFenceRows{columns: []string{"RELEASE_LOCK"}, values: []driver.Value{c.state.releaseResult}}, nil
	default:
		return nil, fmt.Errorf("unexpected query %q", query)
	}
}

var _ driver.QueryerContext = (*fairQueueFenceDriverConn)(nil)

type fairQueueFenceRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *fairQueueFenceRows) Columns() []string { return r.columns }
func (r *fairQueueFenceRows) Close() error      { return nil }
func (r *fairQueueFenceRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}

var fairQueueFenceDriverSequence atomic.Uint64

func newFairQueueFenceTestStore(
	t *testing.T,
	state *fairQueueFenceDriverState,
) (*DBStore, string) {
	t.Helper()
	if state.serverUUID == "" {
		state.serverUUID = "test-server-uuid"
	}
	if state.database == "" {
		state.database = "bkcrab_test"
	}
	if len(state.connIDs) == 0 {
		state.connIDs = []int64{7}
	}
	name := fmt.Sprintf("fairqueue-fence-test-%d", fairQueueFenceDriverSequence.Add(1))
	sql.Register(name, &fairQueueFenceDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	digest := sha256.Sum256([]byte(state.serverUUID + "\x00" + state.database))
	return &DBStore{db: db, dialect: mysqlDialect}, hex.EncodeToString(digest[:])
}

func (s *fairQueueFenceDriverState) counts() (get, release, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCalls, s.releaseCalls, s.physicalClose
}

func TestFairQueueOperationStartFencePinsResourceAndReleasesAfterCanceledCallback(t *testing.T) {
	state := &fairQueueFenceDriverState{
		connIDs: []int64{7, 7, 7}, getResult: int64(1), releaseResult: int64(1),
	}
	store, writer := newFairQueueFenceTestStore(t, state)
	ctx, cancel := context.WithCancel(context.Background())
	callbackErr := errors.New("callback failed")
	err := store.WithFairQueueOperationStartFence(ctx, "rag.index", writer,
		func(session *FairQueueOperationStartSession) error {
			if session.resource != "rag.index" || session.expectedWriter != writer || session.conn == nil {
				t.Fatalf("unbound start session: %+v", session)
			}
			cancel()
			return callbackErr
		})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback error=%v", err)
	}
	get, release, closed := state.counts()
	if get != 1 || release != 1 || closed != 0 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}

func TestFairQueueOperationStartFenceDiscardsGETLockFailures(t *testing.T) {
	tests := []struct {
		name   string
		result driver.Value
		err    error
	}{
		{name: "zero", result: int64(0)},
		{name: "null", result: nil},
		{name: "error", err: errors.New("get lock failed")},
		{name: "timeout", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &fairQueueFenceDriverState{
				connIDs: []int64{7}, getResult: test.result, getErr: test.err,
				releaseResult: int64(1),
			}
			store, writer := newFairQueueFenceTestStore(t, state)
			called := false
			err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
				func(*FairQueueOperationStartSession) error { called = true; return nil })
			if !errors.Is(err, ErrFairQueueStartLockUnavailable) || called {
				t.Fatalf("error=%v callback=%v", err, called)
			}
			get, release, closed := state.counts()
			if get != 1 || release != 0 || closed != 1 {
				t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
			}
		})
	}
}

func TestFairQueueOperationStartFenceDiscardsReleaseFailuresExactlyOnce(t *testing.T) {
	tests := []struct {
		name   string
		result driver.Value
		err    error
	}{
		{name: "zero", result: int64(0)},
		{name: "null", result: nil},
		{name: "error", err: errors.New("release failed")},
		{name: "timeout", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &fairQueueFenceDriverState{
				connIDs: []int64{7, 7, 7}, getResult: int64(1),
				releaseResult: test.result, releaseErr: test.err,
			}
			store, writer := newFairQueueFenceTestStore(t, state)
			err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
				func(*FairQueueOperationStartSession) error { return nil })
			if !errors.Is(err, ErrFairQueueUnsafeConnection) {
				t.Fatalf("error=%v", err)
			}
			get, release, closed := state.counts()
			if get != 1 || release != 1 || closed != 1 {
				t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
			}
		})
	}
}

func TestFairQueueOperationStartFenceDiscardsSessionIdentityChange(t *testing.T) {
	state := &fairQueueFenceDriverState{
		connIDs: []int64{7, 8}, getResult: int64(1), releaseResult: int64(1),
	}
	store, writer := newFairQueueFenceTestStore(t, state)
	called := false
	err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
		func(*FairQueueOperationStartSession) error { called = true; return nil })
	if !errors.Is(err, ErrFairQueueWriterMismatch) || called {
		t.Fatalf("identity error=%v callback=%v", err, called)
	}
	get, release, closed := state.counts()
	if get != 1 || release != 0 || closed != 1 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}

func TestFairQueueOperationStartFenceReleasesOnceThenDiscardsAfterCallbackIdentityChange(t *testing.T) {
	state := &fairQueueFenceDriverState{
		connIDs: []int64{7, 7, 8}, getResult: int64(1), releaseResult: int64(0),
	}
	store, writer := newFairQueueFenceTestStore(t, state)
	called := false
	err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
		func(*FairQueueOperationStartSession) error { called = true; return nil })
	if !errors.Is(err, ErrFairQueueWriterMismatch) || !errors.Is(err, ErrFairQueueUnsafeConnection) || !called {
		t.Fatalf("identity error=%v callback=%v", err, called)
	}
	get, release, closed := state.counts()
	if get != 1 || release != 1 || closed != 1 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}

func TestFairQueueOperationStartSessionReadRejectsLostPinnedSession(t *testing.T) {
	state := &fairQueueFenceDriverState{
		connIDs: []int64{7, 7, 8, 8}, getResult: int64(1), releaseResult: int64(0),
	}
	store, writer := newFairQueueFenceTestStore(t, state)
	readRejected := false
	err := store.WithFairQueueOperationStartFence(context.Background(), "rag.index", writer,
		func(session *FairQueueOperationStartSession) error {
			_, _, readErr := session.Read(context.Background())
			readRejected = errors.Is(readErr, ErrFairQueueWriterMismatch)
			return readErr
		})
	if !readRejected || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("readRejected=%v error=%v", readRejected, err)
	}
	get, release, closed := state.counts()
	if get != 1 || release != 1 || closed != 1 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}

func TestFairQueueExpectedWriterConnVerifiesAndDiscardsOnCallbackError(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{7, 8}}
	store, writer := newFairQueueFenceTestStore(t, state)
	callbackErr := errors.New("mutation failed")
	err := store.withFairQueueExpectedWriterConn(context.Background(), writer,
		func(*sql.Conn, fairQueueMySQLIdentity) error { return callbackErr })
	if !errors.Is(err, callbackErr) || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("error=%v", err)
	}
	get, release, closed := state.counts()
	if get != 0 || release != 0 || closed != 1 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}
