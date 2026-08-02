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

func TestRAGFairQueueLifecycleEntryPointsRejectMalformedWriter(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	fair := &RAGFairQueueStore{store: st, expectedWriter: "not-a-writer"}
	ctx := context.Background()
	doc := &RAGDocumentRecord{ID: "never-created", KBID: "never-used", Version: 1}
	version := &RAGDocumentVersionRecord{DocID: doc.ID, DocVersion: 1}
	originalRequest := RAGObjectWriteRequest{
		UserID: "never-used", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindOriginal, ObjectKey: "never-used", ReferenceKey: doc.ID,
	}
	originalFence := RAGObjectWriteFence{
		UserID: "never-used", KBID: doc.KBID, DocID: doc.ID,
		ObjectKind: RAGObjectKindOriginal, ObjectKey: "never-used", ReferenceKey: doc.ID,
	}
	if got, err := fair.GetRAGKBForLifecycle(ctx, doc.KBID); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("lifecycle KB=%+v err=%v", got, err)
	}
	if got, err := fair.GetRAGDocumentForLifecycle(ctx, doc.ID); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("lifecycle document=%+v err=%v", got, err)
	}
	if got, err := fair.ListRAGDocumentsByKBForLifecycle(ctx, doc.KBID); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("lifecycle documents=%+v err=%v", got, err)
	}
	if got, err := fair.GetUserForRAGLifecycle(ctx, "never-used"); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("lifecycle user=%+v err=%v", got, err)
	}

	if got, err := fair.BeginOriginalRAGObjectWrite(ctx, originalRequest); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("original begin=%+v err=%v", got, err)
	}
	if ready, err := fair.MarkOriginalRAGObjectWriteReady(ctx, originalFence); ready || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("original ready=%v err=%v", ready, err)
	}

	if taskID, err := fair.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, 3); taskID != 0 || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("create task=%d err=%v", taskID, err)
	}
	policy := RAGAdvancedEnqueuePolicy{UserID: "never-used", MaxPendingTasks: 1}
	if taskID, err := fair.CreateRAGDocumentWithVersionAndIndexTaskPolicy(
		ctx, doc, version, 3, policy,
	); taskID != 0 || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("policy create task=%d err=%v", taskID, err)
	}
	if task, err := fair.AdvanceDocumentVersionAndCreateTask(ctx, 1, version); task != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("reindex task=%+v err=%v", task, err)
	}
	if task, err := fair.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, 1, version, policy); task != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("policy reindex task=%+v err=%v", task, err)
	}
	if got, err := fair.MarkRAGDocumentDeleting(ctx, doc.ID); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("document tombstone=%+v err=%v", got, err)
	}
	if got, err := fair.MarkRAGKBDeleting(ctx, doc.KBID); got != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("KB tombstone=%+v err=%v", got, err)
	}
}

type ragFairLifecycleDriverState struct {
	mu sync.Mutex

	connectionIDs []int64
	identityCalls int
	commits       int
	rollbacks     int
	physicalClose int
	mutationRan   bool
}

type ragFairLifecycleDriver struct{ state *ragFairLifecycleDriverState }

func (d *ragFairLifecycleDriver) Open(string) (driver.Conn, error) {
	return &ragFairLifecycleDriverConn{state: d.state}, nil
}

type ragFairLifecycleDriverConn struct {
	state *ragFairLifecycleDriverState
	inTx  bool
}

func (*ragFairLifecycleDriverConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *ragFairLifecycleDriverConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *ragFairLifecycleDriverConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.inTx {
		return nil, errors.New("transaction already active")
	}
	c.inTx = true
	return &ragFairLifecycleDriverTx{conn: c}, nil
}
func (c *ragFairLifecycleDriverConn) Close() error {
	c.state.mu.Lock()
	c.state.physicalClose++
	c.state.mu.Unlock()
	return nil
}
func (c *ragFairLifecycleDriverConn) QueryContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Rows, error) {
	if !strings.Contains(query, "@@server_uuid") {
		return nil, fmt.Errorf("unexpected query %q", query)
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	index := c.state.identityCalls
	c.state.identityCalls++
	if index >= len(c.state.connectionIDs) {
		index = len(c.state.connectionIDs) - 1
	}
	return &ragFairLifecycleDriverRows{
		values: []driver.Value{"test-server-uuid", "bkcrab_test", c.state.connectionIDs[index]},
	}, nil
}
func (c *ragFairLifecycleDriverConn) ExecContext(
	_ context.Context,
	query string,
	_ []driver.NamedValue,
) (driver.Result, error) {
	if query != "UPDATE lifecycle_marker SET touched=1" {
		return nil, fmt.Errorf("unexpected exec %q", query)
	}
	c.state.mu.Lock()
	c.state.mutationRan = true
	c.state.mu.Unlock()
	return driver.RowsAffected(1), nil
}

type ragFairLifecycleDriverTx struct{ conn *ragFairLifecycleDriverConn }

func (tx *ragFairLifecycleDriverTx) Commit() error {
	tx.conn.state.mu.Lock()
	defer tx.conn.state.mu.Unlock()
	tx.conn.inTx = false
	tx.conn.state.commits++
	return nil
}
func (tx *ragFairLifecycleDriverTx) Rollback() error {
	tx.conn.state.mu.Lock()
	defer tx.conn.state.mu.Unlock()
	tx.conn.inTx = false
	tx.conn.state.rollbacks++
	return nil
}

type ragFairLifecycleDriverRows struct {
	values []driver.Value
	done   bool
}

func (*ragFairLifecycleDriverRows) Columns() []string {
	return []string{"server_uuid", "database", "connection_id"}
}
func (*ragFairLifecycleDriverRows) Close() error { return nil }
func (r *ragFairLifecycleDriverRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	copy(dest, r.values)
	r.done = true
	return nil
}

var ragFairLifecycleDriverSequence atomic.Uint64

func newRAGFairLifecycleDriverStore(
	t *testing.T,
	connectionIDs []int64,
) (*RAGFairQueueStore, *ragFairLifecycleDriverState) {
	t.Helper()
	state := &ragFairLifecycleDriverState{connectionIDs: connectionIDs}
	name := fmt.Sprintf("rag-fair-lifecycle-%d", ragFairLifecycleDriverSequence.Add(1))
	sql.Register(name, &ragFairLifecycleDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	digest := sha256.Sum256([]byte("test-server-uuid\x00bkcrab_test"))
	return &RAGFairQueueStore{
		store: &DBStore{db: db, dialect: mysqlDialect}, expectedWriter: hex.EncodeToString(digest[:]),
	}, state
}

func TestRAGFairQueueLifecycleTxWithholdsTaskAndRollsBackOnSessionSwitch(t *testing.T) {
	t.Run("stable session commits and returns task", func(t *testing.T) {
		fair, state := newRAGFairLifecycleDriverStore(t, []int64{7, 7, 7, 7})
		task, err := withRAGFairQueueLifecycleTx(context.Background(), fair,
			func(*sql.Conn) (ragOwnershipRoute, error) {
				return ragOwnershipRoute{KBID: "kb", UserID: "user"}, nil
			},
			func(tx *sql.Tx, _ ragOwnershipRoute) (*RAGIndexTaskRecord, error) {
				if _, err := tx.ExecContext(context.Background(), "UPDATE lifecycle_marker SET touched=1"); err != nil {
					return nil, err
				}
				return &RAGIndexTaskRecord{ID: 41}, nil
			})
		if err != nil || task == nil || task.ID != 41 {
			t.Fatalf("task=%+v err=%v", task, err)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.mutationRan || state.commits != 1 || state.rollbacks != 0 {
			t.Fatalf("mutation=%v commits=%d rollbacks=%d", state.mutationRan, state.commits, state.rollbacks)
		}
	})

	t.Run("session switches before commit", func(t *testing.T) {
		fair, state := newRAGFairLifecycleDriverStore(t, []int64{7, 7, 8, 8})
		task, err := withRAGFairQueueLifecycleTx(context.Background(), fair,
			func(*sql.Conn) (ragOwnershipRoute, error) {
				return ragOwnershipRoute{KBID: "kb", UserID: "user"}, nil
			},
			func(tx *sql.Tx, _ ragOwnershipRoute) (*RAGIndexTaskRecord, error) {
				if _, err := tx.ExecContext(context.Background(), "UPDATE lifecycle_marker SET touched=1"); err != nil {
					return nil, err
				}
				return &RAGIndexTaskRecord{ID: 42}, nil
			})
		if task != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
			t.Fatalf("task=%+v err=%v", task, err)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.mutationRan || state.commits != 0 || state.rollbacks != 1 || state.physicalClose != 1 {
			t.Fatalf("mutation=%v commits=%d rollbacks=%d close=%d",
				state.mutationRan, state.commits, state.rollbacks, state.physicalClose)
		}
	})

	t.Run("outer verification withholds committed result", func(t *testing.T) {
		fair, state := newRAGFairLifecycleDriverStore(t, []int64{7, 7, 7, 8})
		task, err := withRAGFairQueueLifecycleTx(context.Background(), fair,
			func(*sql.Conn) (ragOwnershipRoute, error) {
				return ragOwnershipRoute{KBID: "kb", UserID: "user"}, nil
			},
			func(tx *sql.Tx, _ ragOwnershipRoute) (*RAGIndexTaskRecord, error) {
				if _, err := tx.ExecContext(context.Background(), "UPDATE lifecycle_marker SET touched=1"); err != nil {
					return nil, err
				}
				return &RAGIndexTaskRecord{ID: 43}, nil
			})
		if task != nil || !errors.Is(err, ErrFairQueueWriterMismatch) {
			t.Fatalf("task=%+v err=%v", task, err)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if !state.mutationRan || state.commits != 1 || state.rollbacks != 0 || state.physicalClose != 1 {
			t.Fatalf("mutation=%v commits=%d rollbacks=%d close=%d",
				state.mutationRan, state.commits, state.rollbacks, state.physicalClose)
		}
	})
}

func TestRAGFairQueueLifecycleMySQL(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("fair_lifecycle_%d", time.Now().UTC().UnixNano())
	userID := "u_" + suffix
	kbID := "kb_" + suffix
	docID := "doc_" + suffix
	missingDocID := "doc_missing_" + suffix
	objectKey := "rag/" + userID + "/" + kbID + "/" + docID + "/source.md"
	secondKBID := "kb_cancel_" + suffix
	ensureRAGLifecycleUser(t, st, userID, "active")
	now := time.Now().UTC()
	for _, id := range []string{kbID, secondKBID} {
		if err := st.CreateRAGKB(ctx, &RAGKBRecord{
			ID: id, UserID: userID, Name: id, EmbedProvider: "system",
			EmbedModel: "embed-v1", EmbedDims: 3, ChunkSize: 512, ChunkOverlap: 64,
			ParseMode: RAGParseModeStandard, Status: "active", CreatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_object_write_staging WHERE object_key=?`, objectKey)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_index_tasks WHERE doc_id IN (?,?)`, docID, missingDocID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_document_versions WHERE doc_id IN (?,?)`, docID, missingDocID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_documents WHERE id IN (?,?)`, docID, missingDocID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_kbs WHERE id IN (?,?)`, kbID, secondKBID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM users WHERE id=?`, userID)
	})

	identity, err := st.ReadFairQueueWriterIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fair, err := st.BindRAGFairQueueWriter(identity.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	version := testRAGVersion(docID, 1)
	doc := &RAGDocumentRecord{
		ID: docID, KBID: kbID, FileName: docID + ".md", FileType: "md", FileSize: 1,
		ObjectKey: objectKey,
		Status:    "PENDING", Version: 1, SourceSHA256: version.SourceSHA256,
		IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: now,
	}
	original, err := fair.BeginOriginalRAGObjectWrite(ctx, RAGObjectWriteRequest{
		UserID: userID, KBID: kbID, DocID: docID, ObjectKind: RAGObjectKindOriginal,
		ObjectKey: objectKey, ReferenceKey: docID,
	})
	if err != nil || original == nil {
		t.Fatalf("fair original begin=%+v err=%v", original, err)
	}
	if ready, err := fair.MarkOriginalRAGObjectWriteReady(ctx, *original); err != nil || !ready {
		t.Fatalf("fair original ready=%v err=%v", ready, err)
	}
	policy := RAGAdvancedEnqueuePolicy{UserID: userID, MaxPendingTasks: 10}
	missingDoc := *doc
	missingDoc.ID = missingDocID
	missingDoc.ObjectKey = "rag/" + userID + "/" + kbID + "/" + missingDocID + "/source.md"
	missingVersion := testRAGVersion(missingDocID, 1)
	if taskID, err := fair.CreateRAGDocumentWithVersionAndIndexTaskPolicy(
		ctx, &missingDoc, missingVersion, 3, policy,
	); taskID != 0 || !errors.Is(err, ErrRAGLifecycleInactive) {
		t.Fatalf("fair create without original staging task=%d err=%v", taskID, err)
	}
	if got, err := st.GetRAGDocument(ctx, missingDocID); got != nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing-staging document=%+v err=%v", got, err)
	}
	taskID, err := fair.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, version, 3, policy)
	if err != nil || taskID <= 0 {
		t.Fatalf("fair create task=%d err=%v", taskID, err)
	}
	next := testRAGVersion(docID, 0)
	task, err := fair.AdvanceDocumentVersionAndCreateTaskPolicy(ctx, 1, next, policy)
	if err != nil || task == nil || task.DocVersion != 2 {
		t.Fatalf("fair reindex task=%+v err=%v", task, err)
	}
	deleted, err := fair.MarkRAGDocumentDeleting(ctx, docID)
	if err != nil || deleted == nil || deleted.Status != RAGDocumentStatusDeleting {
		t.Fatalf("fair document tombstone=%+v err=%v", deleted, err)
	}
	deletedKB, err := fair.MarkRAGKBDeleting(ctx, secondKBID)
	if err != nil || deletedKB == nil || deletedKB.Status != RAGKBStatusDeleting {
		t.Fatalf("fair KB tombstone=%+v err=%v", deletedKB, err)
	}
}
