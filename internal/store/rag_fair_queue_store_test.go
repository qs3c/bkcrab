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
)

type ragFairQueueStoreDriverState struct {
	mu sync.Mutex

	connectionIDs []int64
	identityCalls int
	highWater     int64
	highWaterRead int
	physicalClose int
}

type ragFairQueueStoreDriver struct{ state *ragFairQueueStoreDriverState }

func (d *ragFairQueueStoreDriver) Open(string) (driver.Conn, error) {
	return &ragFairQueueStoreDriverConn{state: d.state}, nil
}

type ragFairQueueStoreDriverConn struct{ state *ragFairQueueStoreDriverState }

func (*ragFairQueueStoreDriverConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (*ragFairQueueStoreDriverConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transaction not supported")
}
func (c *ragFairQueueStoreDriverConn) Close() error {
	c.state.mu.Lock()
	c.state.physicalClose++
	c.state.mu.Unlock()
	return nil
}
func (c *ragFairQueueStoreDriverConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	switch {
	case strings.Contains(query, "@@server_uuid"):
		index := c.state.identityCalls
		c.state.identityCalls++
		if index >= len(c.state.connectionIDs) {
			index = len(c.state.connectionIDs) - 1
		}
		return &ragFairQueueStoreDriverRows{
			columns: []string{"server_uuid", "database", "connection_id"},
			values:  []driver.Value{"test-server-uuid", "bkcrab_test", c.state.connectionIDs[index]},
		}, nil
	case strings.Contains(query, "COALESCE(MAX(id),0)"):
		c.state.highWaterRead++
		return &ragFairQueueStoreDriverRows{
			columns: []string{"high_water"}, values: []driver.Value{c.state.highWater},
		}, nil
	default:
		return nil, fmt.Errorf("unexpected query %q", query)
	}
}

type ragFairQueueStoreDriverRows struct {
	columns []string
	values  []driver.Value
	done    bool
}

func (r *ragFairQueueStoreDriverRows) Columns() []string { return r.columns }
func (*ragFairQueueStoreDriverRows) Close() error        { return nil }
func (r *ragFairQueueStoreDriverRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}

var ragFairQueueStoreDriverSequence atomic.Uint64

func newRAGFairQueueFacadeTestStore(
	t *testing.T,
	state *ragFairQueueStoreDriverState,
) (*DBStore, string) {
	t.Helper()
	if len(state.connectionIDs) == 0 {
		state.connectionIDs = []int64{7}
	}
	name := fmt.Sprintf("rag-fairqueue-store-%d", ragFairQueueStoreDriverSequence.Add(1))
	sql.Register(name, &ragFairQueueStoreDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	digest := sha256.Sum256([]byte("test-server-uuid\x00bkcrab_test"))
	return &DBStore{db: db, dialect: mysqlDialect}, hex.EncodeToString(digest[:])
}

func TestRAGFairQueueWriterFacadeBindingValidation(t *testing.T) {
	const writer = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	for _, test := range []struct {
		name     string
		store    *DBStore
		expected string
		wantErr  error
	}{
		{
			name:  "invalid fingerprint wins over non mysql",
			store: &DBStore{dialect: "sqlite"}, expected: "not-a-fingerprint",
			wantErr: ErrFairQueueWriterMismatch,
		},
		{
			name:  "non mysql",
			store: &DBStore{dialect: "sqlite"}, expected: writer,
			wantErr: ErrFairQueueMySQLRequired,
		},
		{
			name:  "nil store invalid fingerprint",
			store: nil, expected: "not-a-fingerprint",
			wantErr: ErrFairQueueWriterMismatch,
		},
		{
			name:  "nil store",
			store: nil, expected: writer,
			wantErr: ErrFairQueueMySQLRequired,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			facade, err := test.store.BindRAGFairQueueWriter(test.expected)
			if facade != nil || !errors.Is(err, test.wantErr) {
				t.Fatalf("BindRAGFairQueueWriter() facade=%v err=%v, want %v", facade, err, test.wantErr)
			}
		})
	}

	store, actualWriter := newRAGFairQueueFacadeTestStore(t, &ragFairQueueStoreDriverState{})
	facade, err := store.BindRAGFairQueueWriter(actualWriter)
	if err != nil {
		t.Fatalf("BindRAGFairQueueWriter() error = %v", err)
	}
	if facade == nil || facade.ExpectedWriterFingerprint() != actualWriter {
		t.Fatalf("bound facade=%v fingerprint=%q, want %q",
			facade, facade.ExpectedWriterFingerprint(), actualWriter)
	}
}

func TestRAGFairQueueWriterFacadeWithholdsSnapshotAfterSessionSwitch(t *testing.T) {
	t.Run("stable pinned session", func(t *testing.T) {
		state := &ragFairQueueStoreDriverState{
			connectionIDs: []int64{7, 7}, highWater: 42,
		}
		store, writer := newRAGFairQueueFacadeTestStore(t, state)
		facade, err := store.BindRAGFairQueueWriter(writer)
		if err != nil {
			t.Fatal(err)
		}
		highWater, err := facade.CaptureRAGFairQueueHighWater(context.Background())
		if err != nil || highWater != 42 {
			t.Fatalf("CaptureRAGFairQueueHighWater()=%d, %v", highWater, err)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.identityCalls != 2 || state.highWaterRead != 1 || state.physicalClose != 0 {
			t.Fatalf("identity=%d high-water=%d close=%d",
				state.identityCalls, state.highWaterRead, state.physicalClose)
		}
	})

	t.Run("identity changes after read", func(t *testing.T) {
		state := &ragFairQueueStoreDriverState{
			connectionIDs: []int64{7, 8}, highWater: 42,
		}
		store, writer := newRAGFairQueueFacadeTestStore(t, state)
		facade, err := store.BindRAGFairQueueWriter(writer)
		if err != nil {
			t.Fatal(err)
		}
		highWater, err := facade.CaptureRAGFairQueueHighWater(context.Background())
		if highWater != 0 || !errors.Is(err, ErrFairQueueWriterMismatch) {
			t.Fatalf("CaptureRAGFairQueueHighWater()=%d, %v; want withheld writer mismatch",
				highWater, err)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.identityCalls != 2 || state.highWaterRead != 1 || state.physicalClose != 1 {
			t.Fatalf("identity=%d high-water=%d close=%d",
				state.identityCalls, state.highWaterRead, state.physicalClose)
		}
	})
}
